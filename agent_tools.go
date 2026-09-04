package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// This file is the whole tool surface: thirteen tools, one registration helper,
// and no way to reach the multiplexer except through sl.b.
//
// Every tool addresses a pane by SLOT. There is no paneId anywhere in a schema,
// a response, an error, a notification or an image caption, and a request that
// carries one is refused rather than ignored — see addAgenticTool.

// registerAgentTools registers all thirteen tools. It takes the slots policy and
// nothing else: the raw tmux client is not reachable from here, so a handler
// cannot shell out even by accident.
func registerAgentTools(s *server.MCPServer, sl *slots, emitter *ChannelEmitter) {
	registerSendKeys(s, sl)
	registerRunInREPL(s, sl)
	registerExecuteCommand(s, sl)
	registerStartAndWatch(s, sl, emitter)
	registerWriteToDisplay(s, sl)
	registerOpenPane(s, sl)
	registerCapturePane(s, sl)
	registerScreenshotPane(s, sl)
	registerPaneState(s, sl)
	registerWatchPane(s, sl, emitter)
	registerClosePane(s, sl)
	registerListSlots(s, sl)
	registerNotify(s, sl)
}

// addAgenticTool is the ONLY registration path, and that is the mechanism rather
// than a convention.
//
// The id rejection has to run on tools that never resolve a slot. Putting it
// inside the resolver — the obvious place — would miss three of them, and
// close-pane is why that matters: it parses its own slot argument and never
// calls resolveSlot, so an ignored paneId there does not merely type into the
// wrong pane. It defaults to slot 1 and KILLS it, or C-c's and releases it if
// the pane was adopted from the user. list-slots and notify resolve nothing at
// all and would never be covered either.
//
// The rule is "a retired argument in a request is an error, and never ignored",
// and this is the tool where "ignored" is destructive. The wrapper runs BEFORE
// the handler body, so a rejected call cannot create a pane, send a keystroke or
// close anything.
//
// TestOnlyPolicyCodeKnowsOurOwnPane asserts by AST that s.AddTool appears
// nowhere else in the package: a tool registered directly would silently lose
// the check, and nothing about the resulting code would look wrong.
func addAgenticTool(s *server.MCPServer, tool mcp.Tool, h server.ToolHandlerFunc) {
	s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := rejectIdArgs(req); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return h(ctx, req)
	})
}

// ---- Response types that belong to one tool each ----

// closedPane is one entry in close-pane's response. The response is always an
// array, for every form of the call, because one shape is easier to consume than
// two.
type closedPane struct {
	Slot   int    `json:"slot"`
	Action string `json:"action"` // "killed" | "released" | "none" | "error"
	Detail string `json:"detail,omitempty"`
}

// slotListing is one entry in list-slots' response.
//
// Nothing here is omitempty, deliberately: this is the tool an agent reads to
// find out what it has open, and a key that disappears when its value is the
// zero value ("no foreground command", "not alive") is exactly the fact the
// caller was asking about.
type slotListing struct {
	Slot          int    `json:"slot"`
	Isolated      bool   `json:"isolated"`
	Origin        string `json:"origin"` // "created" | "adopted"
	ForegroundCmd string `json:"foregroundCmd"`
	IsAlive       bool   `json:"isAlive"`
}

// watchResultToToolResult serialises a WatchResult into a CallToolResult.
func watchResultToToolResult(r *WatchResult) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal watch result: %w", err)
	}
	return mcp.NewToolResultText(string(data)), nil
}

// ---- send-keys ----

func registerSendKeys(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("send-keys",
		// Two facts an agent needs before its first send-keys, and the second is
		// the one nobody expects: this call can ADOPT a pane the user opened. The
		// adoption rules refuse anything that is not an idle same-user shell, but
		// "idle" is judged from the outside and cannot see text the user typed
		// without pressing Enter — see canAcquire. Saying so here is the
		// difference between a documented trade-off and a surprise.
		mcp.WithDescription("Send keystrokes to a helper pane. By default the input is literal "+
			"text. The pane is helper slot 1 unless you name another one — it is CREATED beside "+
			"the agent if it does not exist yet, or ADOPTED from an idle unused shell already "+
			"open in the same window, and it is never the agent's own pane."),
		mcp.WithString("keys",
			mcp.Required(),
			mcp.Description("Keys to send"),
		),
		mcp.WithBoolean("literal",
			mcp.Description("Treat keys as literal text (default true). With literal=false the "+
				"keys are named: Enter, Escape, Tab, Space, BSpace, Up, Down, Left, Right, PageUp, "+
				"PageDown, Home, End, F1-F12, C-<x> for control and M-<x> for meta."),
		),
		mcp.WithBoolean("enter",
			mcp.Description("Append an Enter keystroke after the keys (default false)"),
		),
		slotProperty(),
		isolatedProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// keys is validated before the pane is resolved, and the order matters:
		// resolution can CREATE a pane, and a call that is going to be rejected
		// for a missing argument must not leave a new split behind.
		keys, err := req.RequireString("keys")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		literal := req.GetBool("literal", true)
		enter := req.GetBool("enter", false)
		if err := sl.b.SendKeys(ctx, tgt.Ref, keys, literal, enter); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to send keys", err), nil
		}
		return jsonResult(slotResolution{Slot: tgt.Slot, Created: creating(tgt.Created)})
	})
}

// ---- run-in-repl ----

func registerRunInREPL(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("run-in-repl",
		mcp.WithDescription("Send input to a running REPL and wait for the prompt to reappear, "+
			"then return the output. Works with Python, Node, psql, bash, etc. With no slot it "+
			"talks to helper slot 1, which is where a REPL started by start-and-watch or "+
			"execute-command is running."),
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
		isolatedProperty(),
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

		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		pane := tgt.Ref
		res := slotResolution{Slot: tgt.Slot, Created: creating(tgt.Created)}

		// Capture baseline content before sending input.
		baseline, _ := sl.b.Capture(ctx, pane, 0, false)

		// Record the initial foreground command BEFORE sending input
		// so we can detect REPL exit (foreground command change).
		initialState, _ := sl.b.Foreground(ctx, pane)
		initialCmd := ""
		if initialState != nil {
			initialCmd = initialState.ForegroundCmd
		}

		// Send the input with Enter.
		if err := sl.b.SendKeys(ctx, pane, input, true, true); err != nil {
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
				output, err := sl.b.Capture(ctx, pane, 0, false)
				if err != nil {
					// Pane may have been destroyed — check if process exited.
					paneState, _ := sl.b.Foreground(ctx, pane)
					if paneState != nil && !paneState.IsAlive {
						return jsonResult(replResult{
							slotResolution: res,
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
							slotResolution: res,
							Output:         result,
						})
					}
				}

				// Check if the REPL process has exited.
				paneState, _ := sl.b.Foreground(ctx, pane)
				if paneState != nil {
					if !paneState.IsAlive || (initialCmd != "" && paneState.ForegroundCmd != initialCmd) {
						return jsonResult(replResult{
							slotResolution: res,
							Output:         strings.TrimSpace(newContent),
							Exited:         true,
						})
					}
				}

				if tick.After(deadline) {
					// Return whatever we have with a timeout note.
					return jsonResult(replResult{
						slotResolution: res,
						Output:         fmt.Sprintf("[timeout after %ds]\n%s", timeoutSecs, newContent),
					})
				}
			}
		}
	})
}

// ---- execute-command ----

func registerExecuteCommand(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("execute-command",
		// This tool used to answer "paneId is required", and that refusal is
		// exactly the incident this design comes from: an agent told it may not
		// run a command without naming a pane goes looking for $TMUX_PANE and
		// starts driving raw tmux. It delivers keystrokes like send-keys does, so
		// it resolves like send-keys does.
		mcp.WithDescription("Run a shell command in a helper pane and wait for it to complete. "+
			"Returns the full output and the exit code. With no slot the command runs in helper "+
			"slot 1, a pane beside the agent that is created or adopted on first use and reused "+
			"after that — never the agent's own pane."),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Shell command to execute"),
		),
		mcp.WithNumber("timeoutSeconds",
			mcp.Description("Maximum seconds to wait for the command to complete before returning "+
				"with timedOut:true and partial output (default: no timeout)"),
		),
		slotProperty(),
		isolatedProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// command first: resolution can create a pane, and a call that will be
		// rejected for a missing argument must not leave a split behind.
		command, err := req.RequireString("command")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// This is the ONE tool that allows isolated with no slot, and the reason
		// is that it is the one tool whose work is over when it returns: a
		// command runs to completion and its output comes back. Nothing is left
		// running, so nothing needs a number to be reached by again.
		tgt, ephemeral, err := sl.resolveSlotOrEphemeral(ctx, req, paneArgSpec{AllowEphemeral: true})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Apply per-call timeout if provided. It is applied after resolution so
		// that it bounds the command, as it always has, rather than the pane
		// creation that may precede it.
		timeoutSecs := req.GetInt("timeoutSeconds", 0)
		if timeoutSecs > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
			defer cancel()
		}

		if ephemeral {
			return runOneShot(ctx, sl, command)
		}

		outcome, err := sl.b.Exec(ctx, tgt.Ref, command)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to execute command", err), nil
		}
		return jsonResult(execResult{
			slotResolution: slotResolution{Slot: tgt.Slot, Created: creating(tgt.Created)},
			Output:         outcome.Output,
			ExitCode:       outcome.ExitCode,
			TimedOut:       outcome.TimedOut,
		})
	})
}

// oneShotCleanupTimeout bounds the teardown of an ephemeral pane. It is short
// because it is one kill-pane against a pane we have just been talking to, and
// long enough that a busy machine still gets the command out.
const oneShotCleanupTimeout = 5 * time.Second

// runOneShot opens an invisible pane, runs one command in it, and destroys it —
// all inside this call, and with no registry entry at any point.
//
// The pane is never claimed, and that is deliberate rather than an omission: a
// slot marker is what makes a pane addressable again, and this pane must not be.
// The consequence is that list-slots cannot see it, which is why the leak this
// function could produce is caught by probing the isolated socket directly
// rather than by listing slots.
//
// # The pane is unclaimed, not invisible to teardown
//
// Being unclaimed does not put it out of reach of close-pane({slot:"all"}).
// sweepNamespaceLocked is namespace-scoped, not slot-scoped, precisely so that a
// pane left behind by a process that died before claiming one can still be
// reclaimed — and this pane is indistinguishable from that one while the command
// runs. mcp-go dispatches tool calls onto a worker pool, so a sweep on one worker
// kills an in-flight one-shot on another: the command is interrupted and this
// function reports "failed to execute command" for a command that was fine.
//
// It is a correctness wrinkle rather than a safety defect — the pane is ours
// either way, and tmux does not reuse pane ids within a server's lifetime, so
// the deferred Close cannot reach a stranger's pane. It is left open rather than
// locked shut because the alternatives are worse: holding slotMu across the Exec
// would block every other slot for the length of an arbitrary command, and
// claiming the pane to protect it would make it addressable, which is the one
// property this call must not have. What was fixed is the report — the sweep no
// longer describes this pane as one that "never finished claiming", which
// described a deliberate design as a crash.
//
// waitForShellReady is not optional. OpenIsolated takes no initial command, so
// Exec would otherwise send into a shell that may still be sourcing its rc
// files — keystrokes delivered then can be mangled or dropped outright, which
// produces the worst failure this server can have: a command that looks sent,
// leaves a plausible pane, and never ran.
//
// The close runs on a context DETACHED from the caller's. The likeliest reason
// this handler is unwinding early is that the caller's context expired — a
// timeout is a documented outcome of this tool — and a kill issued on that same
// dead context is a command that does not run, leaving a live shell nobody can
// see, list or reach for the lifetime of the machine.
func runOneShot(ctx context.Context, sl *slots, command string) (*mcp.CallToolResult, error) {
	pane, err := sl.b.OpenIsolated(ctx)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to open a pane for the command", err), nil
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oneShotCleanupTimeout)
		defer cancel()
		_ = sl.b.Close(closeCtx, pane)
	}()

	sl.waitForShellReady(ctx, pane)

	outcome, err := sl.b.Exec(ctx, pane, command)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to execute command", err), nil
	}
	return jsonResult(oneShotResult{
		Output:   outcome.Output,
		ExitCode: outcome.ExitCode,
		TimedOut: outcome.TimedOut,
	})
}

// ---- start-and-watch ----

func registerStartAndWatch(s *server.MCPServer, sl *slots, emitter *ChannelEmitter) {
	addAgenticTool(s, mcp.NewTool("start-and-watch",
		mcp.WithDescription("Start a command in a helper pane and monitor its output. Blocks until "+
			"a readiness pattern matches, a named trigger fires, or the timeout expires. With no "+
			"slot the command starts in helper slot 1, a reusable pane beside the agent in the "+
			"user's own window — so a dev server started here stays visible and can be watched "+
			"again later."),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Command to start (e.g. \"npm run dev\")"),
		),
		mcp.WithString("pattern",
			mcp.Required(),
			mcp.Description("Regex pattern indicating readiness (e.g. \"listening on|ready in|compiled successfully\"). This is the primary trigger."),
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
		isolatedProperty(),
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
		// is the whole point: "no window to place a pane in" is an instruction to
		// the agent, and a transport error hides the text behind a -32603.
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		pane := tgt.Ref

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
		triggers = append(triggers, parseTriggers(triggerSpec, sl.b)...)

		// Snapshot the pane BEFORE sending, and hand that baseline to the
		// monitor. If the monitor took its own baseline it would do so after the
		// command had already been sent, and a command that finishes in under a
		// millisecond ("echo done") would have its output captured *into* the
		// baseline — never appearing as new content, never matching the readiness
		// pattern, and reported as a timeout despite having succeeded.
		baseline, _ := sl.b.Capture(ctx, pane, 0, false)

		if err := sl.b.SendKeys(ctx, pane, command, true, true); err != nil {
			return nil, fmt.Errorf("failed to send command: %w", err)
		}

		// Passing the command lets the monitor drop the shell's echo of it, which
		// would otherwise be the first line of new output and would be matched by
		// the error and pattern triggers. See dropEcho.
		//
		// This is a CREATING tool, so created is non-nil, and it goes in here
		// rather than being assigned to the result afterwards — see
		// monitorPaneFrom, where Emit fires before this function returns.
		result, err := monitorPaneFrom(ctx, s, sl.b, tgt, creating(tgt.Created), &baseline, command,
			mode, triggers, timeoutSecs, emitter)
		if err != nil {
			return nil, err
		}
		return watchResultToToolResult(result)
	})
}

// ---- write-to-display ----

// registerWriteToDisplay writes text a HUMAN reads, which is the one thing that
// makes it different from send-keys: the model never sees what it wrote back.
//
// It resolves like every other keystroke-delivering tool, and it does so for the
// reason execute-command does: an agent told "paneId is required" goes looking
// for the terminal by other means — $TMUX_PANE and raw tmux — which is the
// failure this whole design exists to prevent.
func registerWriteToDisplay(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("write-to-display",
		mcp.WithDescription("Write text to a helper pane as a side-channel coaching display. The "+
			"user sees it in their terminal; the tool returns only the slot it wrote to, so the "+
			"text does not enter the model's context. With no slot the text goes to helper slot 1, "+
			"a pane beside the agent that is created or adopted on first use — never the agent's "+
			"own pane."),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("Text to display in the pane"),
		),
		mcp.WithBoolean("clear",
			mcp.Description("Clear the pane before writing (default false). On a pane the server created this also wipes whatever is sitting unsubmitted in its line buffer, so each write replaces the last. On a pane adopted from the user it never does: a half-typed command line belongs to them, so the screen is redrawn around it and successive writes append instead."),
		),
		slotProperty(),
		isolatedProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// text is validated before the pane is resolved, and the order matters:
		// resolution can CREATE a pane, and a call that is going to be rejected
		// for a missing argument must not leave a new split behind.
		text, err := req.RequireString("text")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		if req.GetBool("clear", false) {
			// The whole target goes in, not the pane. What the clear has to know
			// is whose pane this is, and the only trustworthy answer is the one
			// resolution already read under the lock — see clearForDisplay.
			if err := sl.clearForDisplay(ctx, tgt); err != nil {
				return mcp.NewToolResultErrorFromErr("failed to clear pane", err), nil
			}
		}

		if err := sl.b.SendKeys(ctx, tgt.Ref, text, true, false); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to write to display", err), nil
		}
		return jsonResult(slotResolution{Slot: tgt.Slot, Created: creating(tgt.Created)})
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
// The answer is carried in, not looked up. resolveSlot resolved this pane under
// slotMu from a registry read taken inside that hold, and paneTarget.Owner is
// that read. Asking again here — one lock-free record read, after the lock was
// released — reopens the exact race the lock exists to close. A concurrent
// close-pane on the same slot runs releaseAcquiredLocked, which wipes all three
// markers and leaves the pane ALIVE, handed back to the user. Land that between
// the resolution and the re-read and this function sees "no record", reads that
// as a pane nobody owns, and sends C-u + `clear` + Enter into a shell the user is
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
// # An isolated pane needs no special case, and that is the point
//
// Every isolated pane is ownerAgent — nothing is ever adopted on that socket,
// because there is no user there to adopt from — so the destructive branch is
// the one it takes, and correctly: there is no half-typed command line of
// anyone's to destroy. The rule is stated in terms of OWNERSHIP rather than
// visibility, which is why the invisible case needed no clause of its own.
func (s *slots) clearForDisplay(ctx context.Context, tgt paneTarget) error {
	if !clearKillsTheLine(tgt.Owner) {
		return s.b.SendKeys(ctx, tgt.Ref, "C-l", false, false)
	}
	if err := s.b.SendKeys(ctx, tgt.Ref, "C-u", false, false); err != nil {
		return err
	}
	if err := s.b.SendKeys(ctx, tgt.Ref, "clear", false, true); err != nil {
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
// about the pane. It is separated from the keystrokes so the rule can be tested
// for what it CHOOSES rather than through a real pane and a real race — see
// TestClearDecisionNeverKillsALineOnEvidenceItDoesNotHave.
//
// The bySlot parameter is gone: every target is resolved by slot now, so the
// branch it distinguished — a pane the caller named, where no record meant "a
// display pane you split off yourself" — is unreachable. What survives is the
// half that matters: destructive only on positive knowledge that this server
// made the pane. Acquired gets the redraw, and so does anything else — an owner
// kind this binary does not recognise, or an empty value some future resolution
// path forgot to fill in. Sending C-l to a pane we own costs a repaint. Sending
// C-u to a pane the user owns costs their work, so the default when we are
// unsure has to be the repaint.
func clearKillsTheLine(owner string) bool { return owner == ownerAgent }

// ---- open-pane ----

func registerOpenPane(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("open-pane",
		// direction and size are gone with paneId. They only ever applied when
		// the caller named the pane to SPLIT, and on the slot path the server
		// decides the placement — beside the agent for slot 1, stacked under it
		// for slot 2, and so on. A caller cannot ask for a layout it is not
		// looking at.
		mcp.WithDescription("Get a pane to work in, beside the agent, in the window the user is "+
			"already looking at. With no arguments it returns helper slot 1, creating it if "+
			"needed; slot:2, slot:3 … give further panes. Repeated calls for the same slot return "+
			"the SAME pane (created:false), so a process started there is still there next time. "+
			"With isolated:true the pane is opened where nobody can see it instead."),
		slotProperty(),
		isolatedProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(openedPane{
			slotResolution: slotResolution{Slot: tgt.Slot, Created: creating(tgt.Created)},
			Isolated:       tgt.Isolated,
		})
	})
}

// ---- capture-pane ----

func registerCapturePane(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("capture-pane",
		mcp.WithDescription("Capture terminal text content from a helper pane as plain text. "+
			"Preferred tool for reading command output, logs, and text-based terminal content. Use "+
			"screenshot-pane only when visual rendering (colors, layout, TUI graphics) matters. "+
			"With no slot it reads helper slot 1, the pane this agent runs commands in. Reading a "+
			"slot that has not been opened is an error, not an empty pane."),
		mcp.WithNumber("lines",
			mcp.Description("Number of lines of history to include (default: pane height)"),
		),
		mcp.WithBoolean("colors",
			mcp.Description("Preserve ANSI color escape sequences"),
		),
		slotProperty(),
		// No readOnlyHint, deliberately, and this is the one place the reason is
		// written out in full — screenshot-pane, pane-state, watch-pane and
		// list-slots point here.
		//
		// The old reason is gone: a read no longer splits the user's window or
		// adopts one of their shells, because a missing slot is an error now. The
		// annotation still cannot go on, and what remains is smaller and sharper.
		// A lookup REPAIRS the registry on its way past, exactly as resolution
		// does: it clears a stale slot marker off this server's own pane
		// (set-option), and duplicate healing can RELEASE an adopted loser, which
		// sends C-c into a pane the user opened.
		//
		// readOnlyHint is a licence for a client to skip confirmation, to
		// prefetch, or to batch. None of those may sit on a call that can
		// interrupt something the user is running, however rare the path.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{NoCreate: true})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		lines := req.GetInt("lines", 0)
		colors := req.GetBool("colors", false)
		content, err := sl.b.Capture(ctx, tgt.Ref, lines, colors)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to capture pane", err), nil
		}
		// The text body is returned untouched. This is the one tool whose entire
		// contract is "the pane's exact content", so prepending a header would
		// corrupt the answer and break every assertion anyone has written against
		// it. The slot rides alongside in structuredContent, which costs the text
		// nothing.
		//
		// It rides there unconditionally now. It used to be attached only when a
		// slot had been resolved, because the explicit-paneId path had to answer
		// exactly as it did before slots existed — a wire promise that is gone
		// with the argument that made it.
		res := mcp.NewToolResultText(content)
		res.StructuredContent = slotRef{Slot: tgt.Slot}
		return res, nil
	})
}

// ---- screenshot-pane ----

func registerScreenshotPane(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("screenshot-pane",
		mcp.WithDescription("Render a visual PNG screenshot of a helper pane with full ANSI "+
			"colors, styles, and layout via xterm.js. Returns an image the model can see. Use ONLY "+
			"when visual appearance matters (TUI layouts, color-coded output, ANSI art). For "+
			"reading text content, prefer capture-pane instead. With no slot it renders helper "+
			"slot 1. Rendering a slot that has not been opened is an error, not a blank image."),
		mcp.WithString("theme",
			mcp.Description(`Color theme: "dark" (default) or "light"`),
			mcp.Enum("dark", "light"),
		),
		mcp.WithString("output",
			mcp.Description(`Output mode: default returns a PNG image; "browser" opens in system browser; "html" returns raw HTML as text`),
			mcp.Enum("browser", "html"),
		),
		slotProperty(),
		// No readOnlyHint: resolving a slot can split the user's window, adopt one
		// of their shells and rename a pane. See capture-pane for the full reason.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{NoCreate: true})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		theme := req.GetString("theme", "dark")
		output := req.GetString("output", "")

		// The slot goes in as well as the handle: the image caption names the
		// pane to the model, and the slot is the only name this contract has.
		res, err := handleScreenshotPane(ctx, sl.b, tgt.Ref, tgt.Slot, theme, output)
		if err != nil {
			return res, err
		}
		if res != nil {
			res.StructuredContent = slotRef{Slot: tgt.Slot}
		}
		return res, nil
	})
}

// ---- pane-state ----

func registerPaneState(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("pane-state",
		mcp.WithDescription("Get native OS-level process state for a helper pane. Returns whether "+
			"the foreground process is alive and whether it is waiting for user input (detected "+
			"via OS-level process inspection, not regex). The pids are OS pids. With no slot it "+
			"inspects helper slot 1. Asking about a slot that has not been opened is an error, "+
			"and a slot whose process has exited answers isAlive:false rather than erroring."),
		slotProperty(),
		// No readOnlyHint: see capture-pane.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{NoCreate: true})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		state, err := sl.b.Foreground(ctx, tgt.Ref)
		if err != nil {
			// The adapter reports a vanished pane as a sentinel rather than a
			// sentence, because it cannot know the slot number and this handler
			// can. That is the whole reason errPaneGone exists.
			if errors.Is(err, errPaneGone) {
				return mcp.NewToolResultError(fmt.Sprintf(
					"slot %d has no live pane; open it with open-pane or by running something in it",
					tgt.Slot)), nil
			}
			return mcp.NewToolResultErrorFromErr("failed to get pane state", err), nil
		}
		return jsonResult(paneStateResult{PaneState: state, slotRef: slotRef{Slot: tgt.Slot}})
	})
}

// ---- watch-pane ----

func registerWatchPane(s *server.MCPServer, sl *slots, emitter *ChannelEmitter) {
	addAgenticTool(s, mcp.NewTool("watch-pane",
		mcp.WithDescription("Monitor a helper pane using smart triggers. Blocks until a trigger "+
			"fires or the timeout expires. With no slot it watches helper slot 1, which is where "+
			"start-and-watch and execute-command run by default. Watching a slot that has not been "+
			"opened is an error: use start-and-watch to open one and watch it in a single call."),
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
		// No readOnlyHint: see capture-pane.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := sl.resolveSlot(ctx, req, paneArgSpec{NoCreate: true})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		modeName := req.GetString("mode", "medium")
		triggerSpec := req.GetString("triggers", "")
		timeoutSecs := req.GetInt("timeout", 60)

		mode := resolveMode(modeName)
		triggers := parseTriggers(triggerSpec, sl.b)

		// created is nil: this is a READING tool, and a field that is
		// structurally always false would teach the model that a read might
		// create. The key is absent from the response entirely.
		result, err := monitorPane(ctx, s, sl.b, tgt, mode, triggers, timeoutSecs, emitter)
		if err != nil {
			return nil, err
		}
		return watchResultToToolResult(result)
	})
}

// ---- close-pane ----

// registerClosePane is the owner-aware inverse of resolveHelper, and the reason
// it can exist at all is that it knows the difference between a pane the server
// made and one the user opened.
//
// A pane WITH a perfectly valid agent-owned record is refused when it is the
// pane this server is running in. A subagent started into an outer agent's split
// inherits exactly such a record, so "the record proves it is mine to close" is
// true and still fatal. That guard lives in closeHelperLocked, where every
// present and future teardown path inherits it; see there for the whole story.
//
// A slot that holds nothing is not an error. "Close slot 2" when slot 2 was
// never opened is a request that has already been satisfied, and answering
// {"action":"none"} lets an agent tear down unconditionally at the end of a task
// without first checking what it opened.
//
// The handler itself only parses. Both forms are executed by closePanes, under
// one slotMu hold, because teardown mutates the state resolution reads.
func registerClosePane(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("close-pane",
		mcp.WithDescription("Close a helper pane the agent is finished with. Panes this server "+
			"created are killed; panes it adopted from the user are interrupted (C-c) and "+
			"released, never killed. With no arguments it closes helper slot 1; slot:\"all\" "+
			"closes every helper pane this server opened, visible and isolated alike. Closing a "+
			"slot that was never opened is not an error."),
		closeSlotProperty(),
		mcp.WithDestructiveHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slot, all, hasSlot, err := parseCloseSlotArg(req)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if !hasSlot {
			slot = slotDefault
		}

		// The handler parses and hands over; both forms are performed inside a
		// single slotMu hold in closePanes, because teardown mutates the state
		// resolution reads and a close that ran outside the lock would race a
		// concurrent send-keys into the pane it is destroying.
		entries, err := sl.closePanes(ctx, closeSelector{All: all, Slot: slot})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(entries)
	})
}

// ---- list-slots ----

func registerListSlots(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("list-slots",
		mcp.WithDescription("List the helper slots this agent has open, with what is running in "+
			"each. origin is \"created\" for a pane this server opened and \"adopted\" for one it "+
			"took over from an idle shell of the user's, and isolated tells the panes nobody can "+
			"see from the ones beside the user. Panes belonging to other agents, and the user's "+
			"own panes, are not listed — this is what THIS agent has, not what is on screen."),
		// No readOnlyHint, and here the reason is not the one capture-pane gives.
		// Reading the registry is mildly MUTATING by design: it clears a stale
		// slot marker off this server's own pane and heals duplicate claims,
		// which can send C-c into an adopted pane it is releasing. See
		// listSlotsLocked.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// list-slots answers for every slot, so a slot argument is a caller
		// belief this tool cannot honour — see rejectPaneArgs.
		if err := rejectPaneArgs(req); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		listings, err := sl.listSlots(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(listings)
	})
}

// ---- notify ----

func registerNotify(s *server.MCPServer, sl *slots) {
	addAgenticTool(s, mcp.NewTool("notify",
		// The clarification is not decoration. This tool used to be called
		// display-message, which is also the name of a tmux command that is a
		// FORMAT QUERY — the exact command the agent in the incident behind this
		// design shelled out to run, after reading the tool's name and assuming
		// it could answer questions. The rename removes the collision; the
		// sentence stays, because the assumption it corrects is about what the
		// verb suggests, not only about the name.
		mcp.WithDescription("Show a transient notification to the user. This is a one-way "+
			"notification, NOT a query: it cannot tell you anything about panes or slots. To ask "+
			"questions about the terminal use pane-state, list-slots or capture-pane."),
		mcp.WithString("message",
			mcp.Required(),
			mcp.Description("Message to show the user"),
		),
		mcp.WithNumber("duration",
			mcp.Description("How long the message stays up, in seconds (default 3)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// The message goes to the user's status line, never to a pane, so a
		// slot argument is a belief this tool cannot honour — see rejectPaneArgs.
		if err := rejectPaneArgs(req); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		message, err := req.RequireString("message")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		durationSecs := req.GetInt("duration", 3)

		if err := sl.b.Notify(ctx, message, time.Duration(durationSecs)*time.Second); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to display message", err), nil
		}
		return mcp.NewToolResultText("Message displayed"), nil
	})
}
