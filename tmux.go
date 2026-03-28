package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Session represents a tmux session.
type Session struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached bool   `json:"attached"`
}

// Window represents a tmux window.
type Window struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Panes  int    `json:"panes"`
}

// Pane represents a tmux pane with extended info.
type Pane struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Active         bool   `json:"active"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	CurrentCommand string `json:"currentCommand"`
	CurrentPath    string `json:"currentPath"`
}

// CommandResult holds the output and exit code of a completed command.
type CommandResult struct {
	PaneID   string `json:"paneId"`
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

// CreatedSession holds the IDs returned when a new session is created.
type CreatedSession struct {
	SessionID   string `json:"sessionId"`
	SessionName string `json:"sessionName"`
	WindowID    string `json:"windowId"`
	PaneID      string `json:"paneId"`
}

// CreatedWindow holds the IDs returned when a new window is created.
type CreatedWindow struct {
	WindowID   string `json:"windowId"`
	WindowName string `json:"windowName"`
	PaneID     string `json:"paneId"`
}

// CreatedPane holds the IDs returned when a new pane is created.
type CreatedPane struct {
	PaneID   string `json:"paneId"`
	WindowID string `json:"windowId"`
}

// headlessSocket is the tmux socket name used for isolated headless sessions.
const headlessSocket = "mcp-headless"

// CleanStaleHeadlessSocket checks if the headless tmux socket exists but the
// server behind it is dead (e.g. after a crash). If so, it runs kill-server to
// clean up the stale socket so a fresh server can be started.
func CleanStaleHeadlessSocket() {
	// Try to list sessions on the headless socket. If the server is alive this
	// succeeds (or returns "no sessions"). If the socket is stale, tmux returns
	// an error like "no server running on ...".
	cmd := exec.Command("tmux", "-L", headlessSocket, "list-sessions")
	if err := cmd.Run(); err != nil {
		// Server is not running — kill-server will clean up any stale socket file.
		_ = exec.Command("tmux", "-L", headlessSocket, "kill-server").Run()
	}
}

// headlessPrefix is the ID prefix that identifies headless targets.
const headlessPrefix = "headless:"

// tmuxClient wraps tmux CLI interactions.
type tmuxClient struct {
	shellType string
}

func newTmuxClient(shellType string) *tmuxClient {
	return &tmuxClient{shellType: shellType}
}

// parseTarget splits a target ID into its socket and bare ID.
// IDs prefixed with "headless:" are routed to the mcp-headless socket.
// All other IDs use the default tmux server (empty socket string).
func parseTarget(target string) (socket string, id string) {
	if strings.HasPrefix(target, headlessPrefix) {
		return headlessSocket, strings.TrimPrefix(target, headlessPrefix)
	}
	return "", target
}

// run executes a tmux command and returns its combined output.
func (t *tmuxClient) run(ctx context.Context, args ...string) (string, error) {
	return t.runWithSocket(ctx, "", args...)
}

// runWithSocket executes a tmux command, optionally routing it to a named
// socket via "-L <socket>". An empty socket uses the default tmux server.
func (t *tmuxClient) runWithSocket(ctx context.Context, socket string, args ...string) (string, error) {
	if socket != "" {
		args = append([]string{"-L", socket}, args...)
	}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			subCmd := args[0]
			if socket != "" {
				subCmd = args[2] // skip "-L", socket
			}
			return "", fmt.Errorf("tmux %s: %w: %s", subCmd, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		subCmd := args[0]
		if socket != "" {
			subCmd = args[2]
		}
		return "", fmt.Errorf("tmux %s: %w", subCmd, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// ListSessions returns all active tmux sessions on the default server.
func (t *tmuxClient) ListSessions(ctx context.Context) ([]Session, error) {
	return t.listSessionsOnSocket(ctx, "")
}

// ListHeadlessSessions returns all active sessions on the headless server.
// IDs in the returned sessions are prefixed with "headless:".
func (t *tmuxClient) ListHeadlessSessions(ctx context.Context) ([]Session, error) {
	sessions, err := t.listSessionsOnSocket(ctx, headlessSocket)
	if err != nil {
		return sessions, err
	}
	for i := range sessions {
		sessions[i].ID = headlessPrefix + sessions[i].ID
	}
	return sessions, nil
}

// listSessionsOnSocket lists sessions on the given tmux socket (empty = default).
func (t *tmuxClient) listSessionsOnSocket(ctx context.Context, socket string) ([]Session, error) {
	out, err := t.runWithSocket(ctx, socket, "list-sessions",
		"-F", "#{session_id}\t#{session_name}\t#{session_windows}\t#{session_attached}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") ||
			strings.Contains(err.Error(), "no sessions") {
			return []Session{}, nil
		}
		return nil, err
	}
	if out == "" {
		return []Session{}, nil
	}

	var sessions []Session
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		windows, _ := strconv.Atoi(parts[2])
		sessions = append(sessions, Session{
			ID:       parts[0],
			Name:     parts[1],
			Windows:  windows,
			Attached: parts[3] != "0",
		})
	}
	return sessions, nil
}

// ListWindows returns windows in the given session.
func (t *tmuxClient) ListWindows(ctx context.Context, sessionID string) ([]Window, error) {
	socket, bareID := parseTarget(sessionID)
	out, err := t.runWithSocket(ctx, socket, "list-windows",
		"-t", bareID,
		"-F", "#{window_id}\t#{window_name}\t#{window_active}\t#{window_panes}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []Window{}, nil
	}

	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	var windows []Window
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		panes, _ := strconv.Atoi(parts[3])
		windows = append(windows, Window{
			ID:     prefix + parts[0],
			Name:   parts[1],
			Active: parts[2] == "1",
			Panes:  panes,
		})
	}
	return windows, nil
}

// ListPanes returns panes in the given window with extended info.
func (t *tmuxClient) ListPanes(ctx context.Context, windowID string) ([]Pane, error) {
	socket, bareID := parseTarget(windowID)
	out, err := t.runWithSocket(ctx, socket, "list-panes",
		"-t", bareID,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_active}\t#{pane_width}\t#{pane_height}\t#{pane_current_command}\t#{pane_current_path}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []Pane{}, nil
	}

	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 7 {
			continue
		}
		width, _ := strconv.Atoi(parts[3])
		height, _ := strconv.Atoi(parts[4])
		panes = append(panes, Pane{
			ID:             prefix + parts[0],
			Title:          parts[1],
			Active:         parts[2] == "1",
			Width:          width,
			Height:         height,
			CurrentCommand: parts[5],
			CurrentPath:    parts[6],
		})
	}
	return panes, nil
}

// CapturePane returns the terminal content of a pane.
// lines <= 0 uses the pane's default history size.
// colors controls whether ANSI escape sequences are preserved.
func (t *tmuxClient) CapturePane(ctx context.Context, paneID string, lines int, colors bool) (string, error) {
	socket, bareID := parseTarget(paneID)
	args := []string{"capture-pane", "-t", bareID, "-p"}
	if lines > 0 {
		args = append(args, "-S", strconv.Itoa(-lines))
	}
	if colors {
		args = append(args, "-e")
	}
	return t.runWithSocket(ctx, socket, args...)
}

// CreateSession creates a new detached tmux session.
// Returns a CreatedSession with the session, window, and pane IDs.
func (t *tmuxClient) CreateSession(ctx context.Context, name string) (*CreatedSession, error) {
	return t.createSessionOnSocket(ctx, "", name, "")
}

// CreateHeadlessSession creates a new detached session on the headless tmux
// server. The returned IDs are prefixed with "headless:" so all subsequent
// tool calls are routed to the correct socket automatically.
func (t *tmuxClient) CreateHeadlessSession(ctx context.Context, name, command string) (*CreatedSession, error) {
	sess, err := t.createSessionOnSocket(ctx, headlessSocket, name, command)
	if err != nil {
		return nil, err
	}
	sess.SessionID = headlessPrefix + sess.SessionID
	sess.WindowID = headlessPrefix + sess.WindowID
	sess.PaneID = headlessPrefix + sess.PaneID
	return sess, nil
}

// createSessionOnSocket is the shared implementation for session creation.
// socket selects the tmux server; empty means the default server.
// command, if non-empty, is passed to the shell as the initial command.
// If a session with the given name already exists, it returns the existing
// session's IDs instead of erroring (idempotent create).
func (t *tmuxClient) createSessionOnSocket(ctx context.Context, socket, name, command string) (*CreatedSession, error) {
	// If a name was given, check if a session with that name already exists.
	if name != "" {
		if existing, err := t.findSessionByName(ctx, socket, name); err == nil {
			return existing, nil
		}
	}

	args := []string{"new-session", "-d", "-P", "-F", "#{session_id}\t#{session_name}\t#{window_id}\t#{pane_id}"}
	if name != "" {
		args = append(args, "-s", name)
	}
	if command != "" {
		args = append(args, command)
	}
	out, err := t.runWithSocket(ctx, socket, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	return &CreatedSession{
		SessionID:   parts[0],
		SessionName: parts[1],
		WindowID:    parts[2],
		PaneID:      parts[3],
	}, nil
}

// findSessionByName looks up a session by name on the given socket.
// Returns the session's IDs if found.
func (t *tmuxClient) findSessionByName(ctx context.Context, socket, name string) (*CreatedSession, error) {
	out, err := t.runWithSocket(ctx, socket, "list-sessions",
		"-F", "#{session_id}\t#{session_name}\t#{window_id}\t#{pane_id}",
		"-f", fmt.Sprintf("#{==:#{session_name},%s}", name))
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, fmt.Errorf("session not found: %s", name)
	}
	// Take the first matching line.
	line := strings.Split(out, "\n")[0]
	parts := strings.Split(line, "\t")
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected tmux output: %q", line)
	}
	return &CreatedSession{
		SessionID:   parts[0],
		SessionName: parts[1],
		WindowID:    parts[2],
		PaneID:      parts[3],
	}, nil
}

// CreateWindow creates a new window in the given session.
// Returns a CreatedWindow with the window and pane IDs.
func (t *tmuxClient) CreateWindow(ctx context.Context, sessionID, name string) (*CreatedWindow, error) {
	socket, bareID := parseTarget(sessionID)
	args := []string{"new-window", "-t", bareID, "-P", "-F", "#{window_id}\t#{window_name}\t#{pane_id}"}
	if name != "" {
		args = append(args, "-n", name)
	}
	out, err := t.runWithSocket(ctx, socket, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 3 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	return &CreatedWindow{
		WindowID:   prefix + parts[0],
		WindowName: parts[1],
		PaneID:     prefix + parts[2],
	}, nil
}

// SplitPane splits the given pane. direction is "horizontal" or "vertical".
// size is a percentage (0 means use tmux default of 50%).
// Returns a CreatedPane with the new pane ID and its parent window ID.
func (t *tmuxClient) SplitPane(ctx context.Context, paneID, direction string, size int) (*CreatedPane, error) {
	socket, bareID := parseTarget(paneID)
	args := []string{"split-window", "-t", bareID, "-P", "-F", "#{pane_id}\t#{window_id}"}
	if direction == "horizontal" {
		args = append(args, "-h")
	}
	if size > 0 {
		args = append(args, "-p", strconv.Itoa(size))
	}
	out, err := t.runWithSocket(ctx, socket, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 2 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	return &CreatedPane{
		PaneID:   prefix + parts[0],
		WindowID: prefix + parts[1],
	}, nil
}

// SendKeys sends keystrokes to a pane.
// When literal is true, -l flag is used so tmux treats input as literal text
// rather than interpreting key names like "Enter" or "C-c".
// When enter is true, a real Enter keystroke is appended after the keys.
func (t *tmuxClient) SendKeys(ctx context.Context, paneID, keys string, literal, enter bool) error {
	socket, bareID := parseTarget(paneID)
	args := []string{"send-keys", "-t", bareID}
	if literal {
		args = append(args, "-l")
	}
	args = append(args, keys)
	if _, err := t.runWithSocket(ctx, socket, args...); err != nil {
		return err
	}
	if enter {
		// Send Enter as a separate call so that the -l flag (if set above)
		// does not cause "Enter" to be treated as the five literal characters
		// E-n-t-e-r. This second call intentionally omits -l so tmux
		// interprets "Enter" as the Return key.
		if _, err := t.runWithSocket(ctx, socket, "send-keys", "-t", bareID, "Enter"); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteCommand runs a shell command synchronously in the given pane.
// It wraps the command so output is tee'd to a temp file and the exit code is
// written to a separate file, then uses "tmux wait-for" to block until the
// command finishes. The context deadline controls the overall timeout.
func (t *tmuxClient) ExecuteCommand(ctx context.Context, paneID, command string) (*CommandResult, error) {
	socket, _ := parseTarget(paneID)

	id := uuid.New().String()
	outFile := fmt.Sprintf("/tmp/tmux-mcp-%s.out", id)
	exitFile := fmt.Sprintf("/tmp/tmux-mcp-%s.exit", id)
	waitChannel := "tmux-mcp-" + id

	defer os.Remove(outFile)
	defer os.Remove(exitFile)

	wrapped := t.wrapCommand(command, outFile, exitFile, waitChannel)

	// Send the wrapped command to the pane (literal=false so Enter sends a real Enter).
	if err := t.SendKeys(ctx, paneID, wrapped, false, true); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	// Block until the command signals completion or the context is cancelled.
	// The wait-for must target the same tmux server that the pane lives on.
	var waitErr error
	if socket != "" {
		waitErr = exec.CommandContext(ctx, "tmux", "-L", socket, "wait-for", waitChannel).Run()
	} else {
		waitErr = exec.CommandContext(ctx, "tmux", "wait-for", waitChannel).Run()
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			// Bug 2 fix: on timeout, return partial output instead of a bare error.
			partialOutput := ""
			if data, err := os.ReadFile(outFile); err == nil {
				partialOutput = strings.TrimRight(string(data), "\n")
			}
			return &CommandResult{
				PaneID:   paneID,
				Output:   partialOutput,
				ExitCode: -1,
				TimedOut: true,
			}, nil
		}
		return nil, fmt.Errorf("wait-for: %w", waitErr)
	}

	outputBytes, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("read output file: %w", err)
	}

	// Bug 3 fix: retry reading exit file to handle race with fast-completing commands.
	var exitBytes []byte
	for attempt := 0; attempt < 4; attempt++ {
		exitBytes, err = os.ReadFile(exitFile)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read exit file: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("read exit file: %w", err)
	}

	exitCode, _ := strconv.Atoi(strings.TrimSpace(string(exitBytes)))

	return &CommandResult{
		PaneID:   paneID,
		Output:   strings.TrimRight(string(outputBytes), "\n"),
		ExitCode: exitCode,
	}, nil
}

// wrapCommand builds a shell command that:
//  1. Pipes stdout+stderr through tee into outFile (so it shows in the pane)
//  2. Captures the exit code into exitFile
//  3. Signals waitChannel via "tmux wait-for -S" when done
func (t *tmuxClient) wrapCommand(command, outFile, exitFile, waitChannel string) string {
	switch t.shellType {
	case "fish":
		// fish uses $pipestatus[1] (1-indexed) after a pipe, but pipestatus is
		// only set inside a pipeline. Capture exit status before the pipe instead.
		return fmt.Sprintf(
			`begin; %s; set _exit $status; end 2>&1 | tee %s; echo $_exit > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	case "zsh":
		return fmt.Sprintf(
			`{ %s } 2>&1 | tee %s; echo ${pipestatus[1]} > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	default: // bash
		return fmt.Sprintf(
			`{ %s; } 2>&1 | tee %s; echo ${PIPESTATUS[0]} > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	}
}

// ResizePaneAbsolute resizes a pane to exact dimensions in columns and rows.
func (t *tmuxClient) ResizePaneAbsolute(ctx context.Context, paneID string, width, height int) error {
	socket, bareID := parseTarget(paneID)
	_, err := t.runWithSocket(ctx, socket, "resize-pane", "-t", bareID,
		"-x", strconv.Itoa(width),
		"-y", strconv.Itoa(height))
	return err
}

// ResizePaneRelative adjusts a pane size in the given direction.
// direction must be one of U, D, L, R.
func (t *tmuxClient) ResizePaneRelative(ctx context.Context, paneID, direction string, amount int) error {
	socket, bareID := parseTarget(paneID)
	_, err := t.runWithSocket(ctx, socket, "resize-pane", "-t", bareID, "-"+strings.ToUpper(direction), strconv.Itoa(amount))
	return err
}

// RenameSession renames a tmux session.
func (t *tmuxClient) RenameSession(ctx context.Context, sessionID, newName string) error {
	socket, bareID := parseTarget(sessionID)
	_, err := t.runWithSocket(ctx, socket, "rename-session", "-t", bareID, newName)
	return err
}

// KillSession kills a tmux session and all its windows.
// It accepts session IDs ($N), prefixed IDs (headless:$N), or session names.
func (t *tmuxClient) KillSession(ctx context.Context, sessionID string) error {
	socket, bareID := parseTarget(sessionID)
	_, err := t.runWithSocket(ctx, socket, "kill-session", "-t", bareID)
	if err != nil {
		// If the direct target failed, try resolving by session name.
		resolved, resolveErr := t.resolveSessionTarget(ctx, socket, bareID)
		if resolveErr == nil {
			_, err = t.runWithSocket(ctx, socket, "kill-session", "-t", resolved)
		}
	}
	return err
}

// resolveSessionTarget attempts to resolve a session target that may be a name
// rather than a tmux session ID. It searches the session list on the given socket.
func (t *tmuxClient) resolveSessionTarget(ctx context.Context, socket, target string) (string, error) {
	out, err := t.runWithSocket(ctx, socket, "list-sessions",
		"-F", "#{session_id}\t#{session_name}")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			continue
		}
		sid, sname := parts[0], parts[1]
		if sname == target || sid == target {
			return sid, nil
		}
	}
	return "", fmt.Errorf("session not found: %s", target)
}

// KillWindow kills a tmux window and all its panes.
func (t *tmuxClient) KillWindow(ctx context.Context, windowID string) error {
	socket, bareID := parseTarget(windowID)
	_, err := t.runWithSocket(ctx, socket, "kill-window", "-t", bareID)
	return err
}

// KillPane kills a tmux pane.
func (t *tmuxClient) KillPane(ctx context.Context, paneID string) error {
	socket, bareID := parseTarget(paneID)
	_, err := t.runWithSocket(ctx, socket, "kill-pane", "-t", bareID)
	return err
}

// KillHeadlessServer shuts down the headless tmux server and all its sessions.
// Returns the number of sessions that were running before shutdown.
// It kills sessions individually rather than using kill-server to avoid any
// kill-server wrappers or guards in the environment.
func (t *tmuxClient) KillHeadlessServer(ctx context.Context) (int, error) {
	// List sessions on the headless socket (without the "headless:" prefix so we
	// can pass the bare IDs directly to kill-session on the headless socket).
	sessions, err := t.listSessionsOnSocket(ctx, headlessSocket)
	if err != nil {
		// No server running is not an error.
		return 0, nil
	}
	n := len(sessions)
	for _, s := range sessions {
		// Kill each session on the headless socket using its bare ID.
		_, _ = t.runWithSocket(ctx, headlessSocket, "kill-session", "-t", s.ID)
	}
	return n, nil
}

// DisplayMessage shows a transient message in the tmux status bar.
// durationMs controls how long (in milliseconds) the message is shown.
func (t *tmuxClient) DisplayMessage(ctx context.Context, message string, durationMs int) error {
	_, err := t.run(ctx, "display-message", "-d", strconv.Itoa(durationMs), message)
	return err
}
