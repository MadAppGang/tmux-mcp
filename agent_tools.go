package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerAgentTools(s *server.MCPServer, client *tmuxClient, emitter *ChannelEmitter) {
	registerStartAndWatch(s, client, emitter)
	registerWatchPane(s, client, emitter)
	registerPaneState(s, client)
	registerRunInREPL(s, client)
	registerWriteToDisplay(s, client)
	registerDisplayMessage(s, client)
	registerClosePane(s, client)
}

// closedPane is one entry in close-pane's response. The response is always an
// array, for every form of the call, because one shape is easier to consume than
// two and the tool is new enough to have no consumer to disappoint.
type closedPane struct {
	PaneID string `json:"paneId,omitempty"`
	Slot   int    `json:"slot,omitempty"`
	Action string `json:"action"` // "killed" | "released" | "none" | "error"
	Detail string `json:"detail,omitempty"`
}

// watchResultToToolResult serialises a WatchResult into a CallToolResult.
func watchResultToToolResult(r *WatchResult) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal watch result: %w", err)
	}
	return mcp.NewToolResultText(string(data)), nil
}

// ---- start-and-watch ----

func registerStartAndWatch(s *server.MCPServer, client *tmuxClient, emitter *ChannelEmitter) {
	s.AddTool(mcp.NewTool("start-and-watch",
		mcp.WithDescription("Start a command in a pane and monitor its output. Blocks until a readiness pattern matches, a named trigger fires, or the timeout expires. paneId is optional: with no paneId the command starts in helper slot 1, a reusable pane beside the agent in the user's own window — so a dev server started here stays visible and can be watched again later. headless=true runs it in an invisible isolated session instead."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID (e.g. %0) to run the command in (optional; defaults to helper slot 1)"),
		),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Command to start (e.g. \"npm run dev\")"),
		),
		mcp.WithString("pattern",
			mcp.Required(),
			mcp.Description("Regex pattern indicating readiness (e.g. \"listening on|ready in|compiled successfully\"). This is the primary trigger."),
		),
		mcp.WithBoolean("headless",
			mcp.Description("When true and paneId is omitted, create an isolated headless pane. The returned paneId will have a 'headless:' prefix. Default false."),
		),
		mcp.WithString("mode",
			mcp.Description("Notification preset: quick (0.5s poll/1s or 10 lines), medium (1s poll/5s or 40 lines), slow (2s poll/30s or 100 lines), line (200ms poll/1 line), bunch (500ms poll/10 lines), screen (1s poll/40 lines). Default: quick"),
			mcp.Enum("quick", "medium", "slow", "line", "bunch", "screen"),
		),
		mcp.WithString("triggers",
			mcp.Description("Additional comma-separated trigger names beyond the readiness pattern. Options: exit, shell, user_input, error, bell, idle:N, pattern:REGEX. Default: \"exit,error\""),
		),
		mcp.WithNumber("timeout",
			mcp.Description("Max seconds to watch before giving up (default 60)"),
		),
		slotProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		command, err := req.RequireString("command")
		if err != nil {
			return nil, err
		}
		patternStr, err := req.RequireString("pattern")
		if err != nil {
			return nil, err
		}

		modeName := req.GetString("mode", "quick")
		triggerSpec := req.GetString("triggers", "exit,error")
		timeoutSecs := req.GetInt("timeout", 60)

		re, err := regexp.Compile(patternStr)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", patternStr, err)
		}

		// Resolution failures are reported as tool errors rather than as the
		// bare Go errors the rest of this handler returns, because the message
		// is the whole point: "not in tmux, use headless" and "slot and headless
		// cannot be combined" are instructions to the agent, and a transport
		// error hides the text behind a -32603.
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{AllowHeadless: true})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		paneID := tgt.PaneID
		if tgt.Headless {
			// The one branch that still creates its own session: a headless pane
			// lives on a separate socket with no window, so no slot can name it.
			created, err := client.CreateHeadlessSession(ctx, "", "")
			if err != nil {
				return nil, fmt.Errorf("failed to create headless session: %w", err)
			}
			paneID = created.PaneID
		}

		mode := resolveMode(modeName)

		// Build the trigger list: readiness pattern is always first.
		triggers := []Trigger{
			{
				Name: "pattern:" + patternStr,
				Check: func(_ context.Context, ms *MonitorState) (bool, string) {
					for _, line := range ms.NewLines {
						if re.MatchString(line) {
							return true, fmt.Sprintf("Ready — matched: %s", strings.TrimSpace(line))
						}
					}
					return false, ""
				},
			},
		}
		// Append additional triggers.
		triggers = append(triggers, parseTriggers(triggerSpec, client)...)

		// Snapshot the pane BEFORE sending, and hand that baseline to the
		// monitor. If the monitor took its own baseline it would do so after the
		// command had already been sent, and a command that finishes in under a
		// millisecond ("echo done") would have its output captured *into* the
		// baseline — never appearing as new content, never matching the readiness
		// pattern, and reported as a timeout despite having succeeded.
		baseline, _ := client.CapturePane(ctx, paneID, 0, false)

		if err := client.SendKeys(ctx, paneID, command, true, true); err != nil {
			return nil, fmt.Errorf("failed to send command: %w", err)
		}

		// Passing the command lets the monitor drop the shell's echo of it, which
		// would otherwise be the first line of new output and would be matched by
		// the error and pattern triggers. See dropEcho.
		result, err := monitorPaneFrom(ctx, s, client, paneID, &baseline, command,
			mode, triggers, timeoutSecs, emitter)
		if err != nil {
			return nil, err
		}
		result.Slot, result.Created = tgt.Slot, tgt.Created
		return watchResultToToolResult(result)
	})
}

// ---- watch-pane ----

func registerWatchPane(s *server.MCPServer, client *tmuxClient, emitter *ChannelEmitter) {
	s.AddTool(mcp.NewTool("watch-pane",
		mcp.WithDescription("Monitor a pane using smart triggers. Blocks until a trigger fires or the timeout expires. paneId is optional: with no paneId it watches helper slot 1, which is where start-and-watch and execute-command run by default."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID (e.g. %0) to monitor (optional; defaults to helper slot 1)"),
		),
		mcp.WithString("mode",
			mcp.Description("Notification preset: quick (0.5s poll/1s or 10 lines), medium (1s poll/5s or 40 lines), slow (2s poll/30s or 100 lines), line (200ms poll/1 line), bunch (500ms poll/10 lines), screen (1s poll/40 lines). Default: medium"),
			mcp.Enum("quick", "medium", "slow", "line", "bunch", "screen"),
		),
		mcp.WithString("triggers",
			mcp.Description("Comma-separated trigger names. Options: exit, shell, user_input, error, bell, idle:N (N seconds), pattern:REGEX. Default: \"exit,user_input,error\""),
		),
		mcp.WithNumber("timeout",
			mcp.Description("Max seconds to watch before giving up (default 60)"),
		),
		slotProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		modeName := req.GetString("mode", "medium")
		triggerSpec := req.GetString("triggers", "")
		timeoutSecs := req.GetInt("timeout", 60)

		mode := resolveMode(modeName)
		triggers := parseTriggers(triggerSpec, client)

		result, err := monitorPane(ctx, s, client, tgt.PaneID, mode, triggers, timeoutSecs, emitter)
		if err != nil {
			return nil, err
		}
		result.Slot, result.Created = tgt.Slot, tgt.Created
		return watchResultToToolResult(result)
	})
}

// ---- pane-state ----

func registerPaneState(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("pane-state",
		mcp.WithDescription("Get native OS-level process state for a pane. Returns whether the foreground process is alive and whether it is waiting for user input (detected via OS-level process inspection, not regex). paneId is optional: with no paneId it inspects helper slot 1."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID (e.g. %0) to inspect (optional; defaults to helper slot 1)"),
		),
		slotProperty(),
		// No readOnlyHint: inspecting "helper slot 1" when there is no helper slot
		// 1 yet creates one — a split in the user's window, or the adoption and
		// renaming of one of their idle shells. See capture-pane in main.go for
		// why an annotation a client may act on cannot survive that.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		state, err := client.GetPaneState(ctx, tgt.PaneID)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to get pane state", err), nil
		}
		return jsonResult(paneStateResult{PaneState: state, paneResolution: tgt.resolution()})
	})
}

// ---- run-in-repl ----

func registerRunInREPL(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("run-in-repl",
		mcp.WithDescription("Send input to a running REPL and wait for the prompt to reappear, then return the output. Works with Python, Node, psql, bash, etc. paneId is optional: with no paneId it talks to helper slot 1, which is where a REPL started by start-and-watch or execute-command is running."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID containing the running REPL (optional; defaults to helper slot 1)"),
		),
		mcp.WithString("input",
			mcp.Required(),
			mcp.Description("Command or expression to send to the REPL"),
		),
		mcp.WithString("promptPattern",
			mcp.Required(),
			mcp.Description("Regex matching the REPL prompt (e.g. \">>> \", \"\\\\$ \", \"postgres=#\")"),
		),
		mcp.WithNumber("timeout",
			mcp.Description("Seconds to wait for prompt to reappear (default 10)"),
		),
		slotProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Every required argument is validated, and the prompt regex compiled,
		// before the pane is resolved: resolution can create a pane, and a call
		// rejected for a bad argument must not leave one behind.
		input, err := req.RequireString("input")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		promptStr, err := req.RequireString("promptPattern")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		timeoutSecs := req.GetInt("timeout", 10)

		promptRe, err := regexp.Compile(promptStr)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid promptPattern %q: %v", promptStr, err)), nil
		}

		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		paneID := tgt.PaneID

		// Capture baseline content before sending input.
		baseline, _ := client.CapturePane(ctx, paneID, 0, false)

		// Record the initial foreground command BEFORE sending input
		// so we can detect REPL exit (foreground command change).
		initialState, _ := client.GetPaneState(ctx, paneID)
		initialCmd := ""
		if initialState != nil {
			initialCmd = initialState.ForegroundCmd
		}

		// Send the input with Enter.
		if err := client.SendKeys(ctx, paneID, input, true, true); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to send input", err), nil
		}

		deadline := time.Now().Add(time.Duration(timeoutSecs) * time.Second)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()

		var lastNewContent string
		for {
			select {
			case <-ctx.Done():
				return mcp.NewToolResultError("context cancelled waiting for REPL prompt"), nil
			case tick := <-ticker.C:
				output, err := client.CapturePane(ctx, paneID, 0, false)
				if err != nil {
					// Pane may have been destroyed — check if process exited.
					paneState, _ := client.GetPaneState(ctx, paneID)
					if paneState != nil && !paneState.IsAlive {
						return jsonResult(replResult{
							paneResolution: tgt.resolution(),
							Output:         lastNewContent,
							Exited:         true,
						})
					}
					continue
				}
				// Use diffContent to find lines added after the baseline.
				newContent := diffContent(baseline, output)
				lastNewContent = newContent
				// Match line-by-line from the end.
				contentLines := strings.Split(newContent, "\n")
				for i := len(contentLines) - 1; i >= 0; i-- {
					if promptRe.MatchString(contentLines[i]) {
						// Found the prompt at line i. Return everything before it.
						result := strings.TrimSpace(strings.Join(contentLines[:i], "\n"))
						return jsonResult(replResult{
							paneResolution: tgt.resolution(),
							Output:         result,
						})
					}
				}

				// Check if the REPL process has exited.
				paneState, _ := client.GetPaneState(ctx, paneID)
				if paneState != nil {
					if !paneState.IsAlive || (initialCmd != "" && paneState.ForegroundCmd != initialCmd) {
						return jsonResult(replResult{
							paneResolution: tgt.resolution(),
							Output:         strings.TrimSpace(newContent),
							Exited:         true,
						})
					}
				}

				if tick.After(deadline) {
					// Return whatever we have with a timeout note.
					return jsonResult(replResult{
						paneResolution: tgt.resolution(),
						Output:         fmt.Sprintf("[timeout after %ds]\n%s", timeoutSecs, newContent),
					})
				}
			}
		}
	})
}

// ---- write-to-display ----

// registerWriteToDisplay is the last keystroke-delivering tool to reach the slot
// surface, and it is here for the reason execute-command was: it calls SendKeys,
// so an agent that is told "paneId is required" goes looking for the terminal by
// other means — $TMUX_PANE and raw tmux — which is the failure this whole design
// exists to prevent. paneId and slot are optional and go through the one
// chokepoint, exactly as send-keys does.
//
// paneArgSpec{} and not AllowHeadless: this tool's entire purpose is that a
// human sees the text, and a headless pane lives on a socket with no window and
// no viewer. Refusing headless:true is more useful than honouring it, because
// honouring it would silently write coaching text into a void.
func registerWriteToDisplay(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("write-to-display",
		mcp.WithDescription("Write text to a pane as a side-channel coaching display. The user sees it in their terminal; the tool returns only the pane it wrote to, so the text does not enter the model's context. paneId is optional: with no paneId the text goes to helper slot 1, a pane beside the agent that is created or adopted on first use — never the agent's own pane."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID to write to (optional; defaults to helper slot 1)"),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("Text to display in the pane"),
		),
		mcp.WithBoolean("clear",
			mcp.Description("Clear the pane before writing (default false). On a pane the server created this also wipes whatever is sitting unsubmitted in its line buffer, so each write replaces the last. On a pane adopted from the user it never does: a half-typed command line belongs to them, so the screen is redrawn around it and successive writes append instead."),
		),
		slotProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// text is validated before the pane is resolved, and the order matters:
		// resolution can CREATE a pane, and a call that is going to be rejected
		// for a missing argument must not leave a new split behind.
		text, err := req.RequireString("text")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		if req.GetBool("clear", false) {
			// The whole target goes in, not the pane id. What the clear has to
			// know is whose pane this is, and the only trustworthy answer is the
			// one resolution already read under the lock — see clearForDisplay.
			if err := client.clearForDisplay(ctx, tgt); err != nil {
				return mcp.NewToolResultErrorFromErr("failed to clear pane", err), nil
			}
		}

		if err := client.SendKeys(ctx, tgt.PaneID, text, true, false); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to write to display", err), nil
		}
		return jsonResult(tgt.resolution())
	})
}

// clearForDisplay blanks a pane before write-to-display writes into it, and
// chooses its method from who owns the line it is about to disturb.
//
// # The hazard
//
// The original clear was C-u (kill line) followed by the `clear` command, and
// the C-u is not optional on a pane we made: write-to-display types literal text
// with no Enter, so the PREVIOUS write is still sitting in the shell's line
// editor, and without the kill it would be concatenated with the word "clear"
// and run as one garbage command.
//
// That was safe while the tool demanded an explicit paneId aimed at a display
// pane the agent had just split off. It stopped being safe the moment a bare
// write-to-display({text, clear:true}) began resolving to slot 1, because slot 1
// may be a pane ADOPTED from the user — and the line C-u destroys is then the
// command they were halfway through typing. canAcquire says outright that it
// cannot see unsubmitted input; this is that limitation with a destructive edge
// on it.
//
// # Why "skip the C-u but still send clear" is not the answer
//
// It is the obvious repair and it is worse, which is why it is written down
// rather than left to be rediscovered. Measured in a real shell: with
// `echo THE-USERS-HESITATED-COMMAND ` pending and unsubmitted, sending the
// literal word "clear" plus Enter did not clear anything — it appended to their
// line and RAN `echo THE-USERS-HESITATED-COMMAND clear`. Losing someone's typing
// is bad; pressing Enter on a command they deliberately had not submitted, with
// a word glued onto the end of it, is a different and larger category of wrong.
// The Enter is ours, so the consequences are ours.
//
// # What is done instead
//
// On the user's pane, neither key is sent. C-l is the line editor's redraw:
// readline and zle clear the screen and repaint the prompt with the buffer
// intact, executing nothing — verified with a pending `touch <sentinel>`, which
// survived the clear character for character and never ran. If the foreground
// process is not a line editor at all, ^L is a form feed, which is about as
// inert as a keystroke gets — and strictly less eventful than the coaching text
// this function is clearing the way for.
//
// The cost is honest and documented in the `clear` parameter: on an adopted pane
// our own previous text also survives, so repeated writes append rather than
// replace. A display that grows is a worse display than one that refreshes, and
// still better than a tool that erases the user's work to look tidy.
//
// # Deciding whose pane it is
//
// The registry is the only thing that knows, and ownerAcquired is the one owner
// kind that means "the user opened this". The question is WHEN that answer is
// read, and this function used to get it wrong.
//
// On the slot path the answer is carried in, not looked up. resolvePaneArg
// resolved this pane under slotMu from a registry read taken inside that hold,
// and paneTarget.Owner is that read. Asking again here — one lock-free
// paneRecordFor, after the lock was released — reopens the exact race the lock
// exists to close. A concurrent close-pane on the same slot runs
// releaseAcquiredLocked, which wipes all three markers and leaves the pane
// ALIVE, handed back to the user. Land that between the resolution and the
// re-read and this function sees "no record", reads that as a pane the caller
// named explicitly, and sends C-u + `clear` + Enter into a shell the user is
// once again typing in — destroying their line and pressing Enter on it. That is
// the cardinal failure this design exists to prevent, reached through the one
// tool whose entire purpose is to be helpful.
//
// Carrying the locked answer forward is sound, not merely better, because it
// removes the only dangerous direction and leaves harmless ones:
//
//   - Captured "acquired" → C-l, which is safe whatever has happened since. The
//     line editor repaints and executes nothing, and against a pane that has
//     since died the send simply fails.
//   - Captured "agent" → the pane was one this server created, and the only
//     thing a concurrent close-pane does to one of those is KILL it. Release —
//     the path that hands a pane back to the user — runs for acquired records
//     only, so an agent-owned pane never becomes the user's between our two
//     statements. The worst case is keystrokes sent at a pane that is gone,
//     which fails and is reported.
//
// The dangerous direction is only ever "thought it was ours, it is actually
// theirs", and a value read while the slot was held cannot produce it.
//
// # The explicit-paneId path reads here, deliberately
//
// A caller that names a pane has taken the safety burden for it, there is no
// slot resolution to carry anything forward from, and there is no race of this
// shape either: nothing in the server releases a slot that was never resolved.
// So that path reads the registry now, and the distinction is written into the
// code — the branch is on Slot, the field that means "this came from
// resolution" — rather than being an accident of which value happens to be set.
//
// A registry read that FAILS is treated as the user's pane, not as ours. The
// conservative direction here is the one that declines to destroy something, and
// it costs nothing real: the read only fails when the pane is unreachable, in
// which case the write that follows is about to fail anyway.
//
// A pane with no record at all keeps the old behaviour, and can now only be
// reached by naming it — which is the behaviour every pre-slot caller of this
// tool already relies on.
func (t *tmuxClient) clearForDisplay(ctx context.Context, tgt paneTarget) error {
	owner, bySlot := tgt.Owner, tgt.Slot != 0
	if !bySlot {
		rec, found, err := t.paneRecordFor(ctx, tgt.PaneID)
		if err != nil {
			return t.SendKeys(ctx, tgt.PaneID, "C-l", false, false)
		}
		owner = ""
		if found {
			owner = rec.Owner
		}
	}
	if !clearKillsTheLine(owner, bySlot) {
		return t.SendKeys(ctx, tgt.PaneID, "C-l", false, false)
	}
	if err := t.SendKeys(ctx, tgt.PaneID, "C-u", false, false); err != nil {
		return err
	}
	if err := t.SendKeys(ctx, tgt.PaneID, "clear", false, true); err != nil {
		return err
	}
	// Brief pause so the clear completes before we write. Only the shell-command
	// path needs it: `clear` is a fork and exec that repaints the pane
	// asynchronously, while C-l is handled by the line editor in the same input
	// stream and cannot be overtaken by what we send next.
	time.Sleep(150 * time.Millisecond)
	return nil
}

// clearKillsTheLine is the whole decision above, as a function of what is known
// about the pane and of how that knowledge was obtained. It is separated from
// the keystrokes so the rule can be tested for what it CHOOSES rather than
// through a real pane and a real race — see
// TestClearDecisionNeverKillsALineOnEvidenceItDoesNotHave.
//
// bySlot is not decoration: the two paths answer an empty owner differently, and
// each answer is right for its path.
//
//   - Resolved by slot: destructive only on positive knowledge that this server
//     made the pane. Acquired gets the redraw, and so does anything else —
//     an owner kind this binary does not recognise, or an empty value some
//     future resolution path forgot to fill in. Sending C-l to a pane we own
//     costs a repaint. Sending C-u to a pane the user owns costs their work, so
//     the default when we are unsure has to be the repaint.
//   - Named explicitly: the pre-slot behaviour. No record means "a display pane
//     the caller split off and pointed us at", which is what this tool was built
//     for and what its existing callers rely on; only a record that positively
//     says "acquired" withholds the kill.
func clearKillsTheLine(owner string, bySlot bool) bool {
	if bySlot {
		return owner == ownerAgent
	}
	return owner != ownerAcquired
}

// ---- close-pane ----

// registerClosePane is the owner-aware inverse of resolveHelper, and the reason
// it can exist at all is that it knows the difference between a pane the server
// made and one the user opened.
//
// That is also why an explicit paneId naming a pane with no registry record is
// REFUSED rather than killed. Killing it would make close-pane a second
// kill-pane with a friendlier name and a wider blast radius — an agent reaching
// for "close the pane I am finished with" would eventually point it at one of the
// user's, and the refusal is the only thing standing between a tidy-up and a
// destroyed session. kill-pane deliberately keeps its blunt signature (paneId
// required, no slot, no default) for the mirror-image reason: there must exist no
// argument-less call in this server that destroys something, and the tool that
// destroys unconditionally must never be callable by accident.
//
// The other refusal is the mirror of that one, and it took longer to see: a pane
// WITH a perfectly valid agent-owned record is also refused when it is the pane
// this server is running in. A subagent started into an outer agent's split
// inherits exactly such a record, so "the record proves it is mine to close" is
// true and still fatal. That guard lives in closeHelperLocked, where every
// present and future teardown path inherits it; see there for the whole story.
//
// A slot that holds nothing is not an error. "Close slot 2" when slot 2 was never
// opened is a request that has already been satisfied, and answering
// {"action":"none"} lets an agent tear down unconditionally at the end of a task
// without first checking what it opened.
//
// The handler itself only parses. All three forms are executed by closePanes,
// under one slotMu hold, because teardown mutates the state resolution reads.
func registerClosePane(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("close-pane",
		mcp.WithDescription("Close a helper pane the agent is finished with. Panes this server created are killed; panes it adopted from the user are interrupted (C-c) and released, never killed. With no arguments it closes helper slot 1; slot:\"all\" closes every helper pane in this window. Refuses any paneId it does not recognise as its own, and refuses the pane this server is itself running in — use kill-pane if you are certain."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID to close (optional). Must be a pane this server created or adopted."),
		),
		closeSlotProperty(),
		mcp.WithDestructiveHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slot, all, hasSlot, err := parseCloseSlotArg(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if _, err := checkHeadlessArg(req, paneArgSpec{}, hasSlot); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if !hasSlot {
			slot = slotDefault
		}

		// The handler parses and hands over; every one of the three forms is
		// performed inside a single slotMu hold in closePanes, because teardown
		// mutates the state resolution reads and a close that ran outside the lock
		// would race a concurrent send-keys into the pane it is destroying. An
		// explicit paneId still wins over slot, exactly as it does everywhere else.
		entries, err := client.closePanes(ctx, closeSelector{
			PaneID: req.GetString("paneId", ""),
			All:    all,
			Slot:   slot,
		})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(closedResult(entries...))
	})
}

// closedResult wraps the entries in the object the tool returns.
func closedResult(entries ...closedPane) any {
	if entries == nil {
		entries = []closedPane{}
	}
	return struct {
		Closed []closedPane `json:"closed"`
	}{Closed: entries}
}

// ---- display-message ----

func registerDisplayMessage(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("display-message",
		// The clarification is not decoration. This tool shares a name with
		// "tmux display-message -p", which is a FORMAT QUERY — the exact command
		// the agent in the incident behind this design shelled out to raw tmux to
		// run, after reading this tool's name and assuming it could answer
		// questions. It writes to the status bar and returns nothing about the
		// terminal; the tools that answer questions are pane-state, list-panes
		// and capture-pane.
		mcp.WithDescription("Show a transient notification in the user's tmux status bar. This is a one-way notification, NOT a query: it cannot tell you anything about panes, windows or sessions. To ask questions about the terminal use pane-state, list-panes or capture-pane."),
		mcp.WithString("message",
			mcp.Required(),
			mcp.Description("Message to display in the status bar"),
		),
		mcp.WithNumber("duration",
			mcp.Description("Display duration in seconds (default 3)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		message, err := req.RequireString("message")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		durationSecs := req.GetInt("duration", 3)
		durationMs := durationSecs * 1000

		if err := client.DisplayMessage(ctx, message, durationMs); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to display message", err), nil
		}
		return mcp.NewToolResultText("Message displayed"), nil
	})
}
