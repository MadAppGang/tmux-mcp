package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/acarl005/stripansi"
	"github.com/mark3labs/mcp-go/server"
)

// NotificationMode controls how often the monitor polls and when it sends
// progress notifications based on time elapsed or new line count.
type NotificationMode struct {
	Name          string
	PollInterval  time.Duration
	TimeThreshold time.Duration // send progress after this much time (0 = disabled)
	LineThreshold int           // send progress after this many new lines (0 = disabled)
}

// modes is the set of built-in notification presets.
var modes = map[string]NotificationMode{
	"quick":  {Name: "quick", PollInterval: 500 * time.Millisecond, TimeThreshold: 1 * time.Second, LineThreshold: 10},
	"medium": {Name: "medium", PollInterval: 1 * time.Second, TimeThreshold: 5 * time.Second, LineThreshold: 40},
	"slow":   {Name: "slow", PollInterval: 2 * time.Second, TimeThreshold: 30 * time.Second, LineThreshold: 100},
	"line":   {Name: "line", PollInterval: 200 * time.Millisecond, TimeThreshold: 0, LineThreshold: 1},
	"bunch":  {Name: "bunch", PollInterval: 500 * time.Millisecond, TimeThreshold: 0, LineThreshold: 10},
	"screen": {Name: "screen", PollInterval: 1 * time.Second, TimeThreshold: 0, LineThreshold: 40},
}

// resolveMode returns the NotificationMode for the given name, defaulting to
// "medium" if the name is empty or unrecognised.
func resolveMode(name string) NotificationMode {
	if name == "" {
		return modes["medium"]
	}
	if m, ok := modes[name]; ok {
		return m
	}
	return modes["medium"]
}

// MonitorState carries the evolving state passed to trigger Check functions.
type MonitorState struct {
	// PaneID being monitored.
	PaneID string
	// Baseline content captured at the start of monitoring.
	Baseline string
	// Current full pane content.
	Current string
	// NewContent is the portion of Current that appeared after Baseline.
	NewContent string
	// NewLines is NewContent split by newline (ANSI-stripped).
	NewLines []string
	// AllOutput accumulates all new content seen so far.
	AllOutput strings.Builder
	// NewLineCount is the number of new lines added since the last progress report.
	NewLineCount int
	// LastProgressTime is when we last sent a progress notification.
	LastProgressTime time.Time
	// PaneState is the most recently fetched OS-level process state.
	PaneState *PaneState
	// InitialCmd is the foreground command recorded at monitor startup.
	InitialCmd string
	// LastOutputTime is when we last saw new output (for idle detection).
	LastOutputTime time.Time
	// Start is when monitoring began.
	Start time.Time
	// Client provides tmux access.
	Client *tmuxClient
}

// Trigger defines a named condition that, when true, terminates monitoring.
type Trigger struct {
	// Name is the event string returned in WatchResult.
	Name string
	// Check returns (fired, detail) where detail is a human-readable explanation.
	Check func(ctx context.Context, s *MonitorState) (bool, string)
}

// parseTriggers converts a comma-separated list of trigger names into Trigger
// slices. Supported names:
//
//	exit         — process group exited
//	shell        — foreground command reverted to a shell
//	idle:N       — no new output for N seconds
//	user_input   — native OS detection: process waiting for terminal input
//	error        — common error pattern in new output
//	bell         — tmux window bell flag set
//	pattern:X    — custom regex X matches a new output line
func parseTriggers(spec string, client *tmuxClient) []Trigger {
	if spec == "" {
		return defaultTriggers(client)
	}
	var ts []Trigger
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		t := buildTrigger(part, client)
		if t != nil {
			ts = append(ts, *t)
		}
	}
	return ts
}

// defaultTriggers returns the default trigger set for watch-pane.
func defaultTriggers(client *tmuxClient) []Trigger {
	var ts []Trigger
	for _, name := range []string{"exit", "user_input", "error"} {
		t := buildTrigger(name, client)
		if t != nil {
			ts = append(ts, *t)
		}
	}
	return ts
}

// shellNames is the set of commands considered a "shell" for the shell trigger.
var shellNames = map[string]bool{
	"bash": true, "zsh": true, "fish": true, "sh": true, "dash": true,
	"ksh": true, "tcsh": true, "csh": true,
}

var errorRe = regexp.MustCompile(`(?i)error[: ]|fatal|panic|exception|failed|FAIL`)

// buildTrigger constructs a Trigger for the given name string.
func buildTrigger(name string, client *tmuxClient) *Trigger {
	switch {
	case name == "exit":
		return &Trigger{
			Name: "exit",
			Check: func(_ context.Context, s *MonitorState) (bool, string) {
				if s.PaneState == nil {
					return false, ""
				}
				if !s.PaneState.IsAlive {
					return true, fmt.Sprintf("Process exited with code %d", s.PaneState.ExitCode)
				}
				return false, ""
			},
		}

	case name == "shell":
		return &Trigger{
			Name: "shell",
			Check: func(_ context.Context, s *MonitorState) (bool, string) {
				if s.PaneState == nil {
					return false, ""
				}
				cmd := s.PaneState.ForegroundCmd
				if cmd == s.InitialCmd {
					return false, ""
				}
				if shellNames[cmd] {
					return true, fmt.Sprintf("Foreground command returned to shell: %s", cmd)
				}
				return false, ""
			},
		}

	case strings.HasPrefix(name, "idle:"):
		secStr := strings.TrimPrefix(name, "idle:")
		idleSecs, err := strconv.Atoi(secStr)
		if err != nil || idleSecs <= 0 {
			idleSecs = 5
		}
		idleDur := time.Duration(idleSecs) * time.Second
		return &Trigger{
			Name: name,
			Check: func(_ context.Context, s *MonitorState) (bool, string) {
				if s.LastOutputTime.IsZero() {
					return false, ""
				}
				quiescent := time.Since(s.LastOutputTime)
				if quiescent >= idleDur {
					return true, fmt.Sprintf("No new output for %.0fs", quiescent.Seconds())
				}
				return false, ""
			},
		}

	case name == "user_input":
		return &Trigger{
			Name: "user_input",
			Check: func(_ context.Context, s *MonitorState) (bool, string) {
				if s.PaneState == nil {
					return false, ""
				}
				if s.PaneState.WaitingForInput {
					return true, fmt.Sprintf("Process %q is waiting for user input", s.PaneState.ForegroundCmd)
				}
				return false, ""
			},
		}

	case name == "error":
		return &Trigger{
			Name: "error",
			Check: func(_ context.Context, s *MonitorState) (bool, string) {
				for _, line := range s.NewLines {
					if errorRe.MatchString(line) {
						return true, fmt.Sprintf("Matched error pattern: %s", strings.TrimSpace(line))
					}
				}
				return false, ""
			},
		}

	case name == "bell":
		return &Trigger{
			Name: "bell",
			Check: func(ctx context.Context, s *MonitorState) (bool, string) {
				socket, bareID := parseTarget(s.PaneID)
				out, err := client.runWithSocket(ctx, socket, "display-message", "-p", "-t", bareID, "#{window_bell_flag}")
				if err != nil {
					return false, ""
				}
				if strings.TrimSpace(out) == "1" {
					return true, "tmux window bell received"
				}
				return false, ""
			},
		}

	case strings.HasPrefix(name, "pattern:"):
		patStr := strings.TrimPrefix(name, "pattern:")
		re, err := regexp.Compile(patStr)
		if err != nil {
			// Invalid regex — skip this trigger.
			return nil
		}
		return &Trigger{
			Name: name,
			Check: func(_ context.Context, s *MonitorState) (bool, string) {
				for _, line := range s.NewLines {
					if re.MatchString(line) {
						return true, fmt.Sprintf("Matched pattern %q: %s", patStr, strings.TrimSpace(line))
					}
				}
				return false, ""
			},
		}
	}
	return nil
}

// WatchResult is the structured result returned by watch-pane and start-and-watch.
type WatchResult struct {
	PaneID    string     `json:"paneId"`
	Event     string     `json:"event"`            // trigger name or "timeout"
	Detail    string     `json:"detail"`           // human-readable explanation
	Elapsed   float64    `json:"elapsed"`          // seconds
	Output    string     `json:"output"`           // all new content accumulated
	PaneState *PaneState `json:"paneState,omitempty"` // final process state
}

// monitorPane runs the smart trigger-based monitoring loop.
//
// It polls at mode.PollInterval, checks all triggers each tick, accumulates
// new output, and sends progress notifications based on the mode's thresholds.
// It returns when a trigger fires, the timeout expires, or the context is
// cancelled.
func monitorPane(
	ctx context.Context,
	s *server.MCPServer,
	client *tmuxClient,
	paneID string,
	mode NotificationMode,
	triggers []Trigger,
	timeoutSecs int,
	emitter *ChannelEmitter,
) (*WatchResult, error) {
	start := time.Now()
	timeout := time.Duration(timeoutSecs) * time.Second

	// Capture baseline so we only surface new content.
	baseline, _ := client.CapturePane(ctx, paneID, 0, false)

	// Fetch initial process state to record the starting command.
	initialState, _ := client.GetPaneState(ctx, paneID)
	initialCmd := ""
	if initialState != nil {
		initialCmd = initialState.ForegroundCmd
	}

	ms := &MonitorState{
		PaneID:           paneID,
		Baseline:         baseline,
		InitialCmd:       initialCmd,
		LastProgressTime: start,
		LastOutputTime:   start,
		Start:            start,
		Client:           client,
	}

	ticker := time.NewTicker(mode.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-ticker.C:
			elapsed := time.Since(start)

			// Capture current pane content.
			current, err := client.CapturePane(ctx, paneID, 0, false)
			if err != nil {
				// CapturePane fails when the pane has been destroyed (e.g.
				// after shell exit). Still fetch pane state so the "exit"
				// trigger can fire.
				paneState, _ := client.GetPaneState(ctx, paneID)
				ms.PaneState = paneState
				for _, trig := range triggers {
					fired, detail := trig.Check(ctx, ms)
					if fired {
						result := &WatchResult{
							PaneID:    paneID,
							Event:     trig.Name,
							Detail:    detail,
							Elapsed:   elapsed.Seconds(),
							Output:    ms.AllOutput.String(),
							PaneState: paneState,
						}
						emitter.Emit(ctx, result)
						return result, nil
					}
				}
				if elapsed >= time.Duration(timeoutSecs)*time.Second {
					result := &WatchResult{
						PaneID:    paneID,
						Event:     "timeout",
						Detail:    fmt.Sprintf("Timed out after %ds", timeoutSecs),
						Elapsed:   elapsed.Seconds(),
						Output:    ms.AllOutput.String(),
						PaneState: paneState,
					}
					emitter.Emit(ctx, result)
					return result, nil
				}
				continue
			}
			ms.Current = current

			// Compute new content since baseline.
			newContent := diffContent(baseline, current)
			// Accumulate new output that wasn't seen last poll.
			// Compare against the previous NewContent to detect genuinely new lines.
			if newContent != "" && newContent != ms.NewContent {
				ms.AllOutput.WriteString(newContent)
			}
			ms.NewContent = newContent

			// Strip ANSI for pattern matching.
			cleanContent := stripansi.Strip(newContent)
			newLines := strings.Split(strings.TrimRight(cleanContent, "\n"), "\n")
			// Filter blank lines for trigger checking.
			var nonEmpty []string
			for _, l := range newLines {
				if strings.TrimSpace(l) != "" {
					nonEmpty = append(nonEmpty, l)
				}
			}
			ms.NewLines = nonEmpty

			// Track output quiescence for idle trigger.
			if len(nonEmpty) > 0 {
				ms.LastOutputTime = time.Now()
			}

			// Fetch process state for native triggers.
			paneState, _ := client.GetPaneState(ctx, paneID)
			ms.PaneState = paneState

			// Update new-line count since last progress report.
			ms.NewLineCount += len(nonEmpty)

			// Check all triggers.
			for _, trig := range triggers {
				fired, detail := trig.Check(ctx, ms)
				if fired {
					result := &WatchResult{
						PaneID:    paneID,
						Event:     trig.Name,
						Detail:    detail,
						Elapsed:   elapsed.Seconds(),
						Output:    ms.AllOutput.String(),
						PaneState: paneState,
					}
					emitter.Emit(ctx, result)
					return result, nil
				}
			}

			// Determine whether to send a progress notification.
			sinceProgress := time.Since(ms.LastProgressTime)
			shouldNotify := false
			if mode.LineThreshold > 0 && ms.NewLineCount >= mode.LineThreshold {
				shouldNotify = true
			}
			if mode.TimeThreshold > 0 && sinceProgress >= mode.TimeThreshold {
				shouldNotify = true
			}

			if shouldNotify {
				_ = s.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
					"progress": int(elapsed.Seconds()),
					"total":    timeoutSecs,
					"message":  fmt.Sprintf("[%s +%ds] %d new lines", paneID, int(elapsed.Seconds()), ms.NewLineCount),
				})
				ms.LastProgressTime = time.Now()
				ms.NewLineCount = 0
			}

			// Check overall timeout.
			if elapsed >= timeout {
				result := &WatchResult{
					PaneID:    paneID,
					Event:     "timeout",
					Detail:    fmt.Sprintf("Timed out after %ds", timeoutSecs),
					Elapsed:   elapsed.Seconds(),
					Output:    ms.AllOutput.String(),
					PaneState: paneState,
				}
				emitter.Emit(ctx, result)
				return result, nil
			}
		}
	}
}

// diffContent returns the portion of current that comes after the initial
// snapshot. It trims the leading lines that appear in initial.
// ANSI stripping for matching is done by callers; this function preserves raw output.
func diffContent(initial, current string) string {
	if initial == "" {
		return current
	}
	// Find where the initial content ends in the current output.
	idx := strings.LastIndex(current, strings.TrimRight(initial, "\n"))
	if idx < 0 {
		return current
	}
	after := current[idx+len(strings.TrimRight(initial, "\n")):]
	return strings.TrimLeft(after, "\n")
}
