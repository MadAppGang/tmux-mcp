package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// version is the build's version string. GoReleaser sets it at link time
// (-ldflags "-X main.version=<tag>"), so release tarballs report the clean git
// tag; a plain `go build` leaves it as "dev".
var version = "dev"

// resolveVersion returns the build's version. GoReleaser's injected tag wins.
// Failing that — the case that matters for the terminal plugin, which installs
// this via `go install github.com/MadAppGang/tmux-mcp@latest` (its setup.go
// dependency) — the resolved tag is carried in the binary's build info. That is
// accepted only when it is a clean release tag: never the "(devel)" placeholder
// and never a build with metadata such as the "+dirty" suffix a modified working
// tree adds, so the reported version is always a real release and never a weird
// local-build string.
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" && !strings.Contains(v, "+") {
			return v
		}
	}
	return version
}

func main() {
	shellType := flag.String("shell-type", "bash", "Shell type for exit code capture (bash/zsh/fish)")
	channelMode := flag.Bool("channel", false, "Enable Claude Code channel mode: push tmux events as channel notifications")
	showVersion := flag.Bool("version", false, "Print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(resolveVersion())
		return
	}

	// Clean up any stale headless socket from a previous crash.
	CleanStaleHeadlessSocket()

	client := newTmuxClient(*shellType)

	// Resource capabilities are NOT advertised: this server registers no
	// resources. The two it used to serve were tmux://sessions and the
	// tmux://pane/{paneId} template, and the template is a paneId in the
	// surface — the one thing this contract does not have. Advertising an empty
	// capability would tell a client to call resources/list for nothing.
	var serverOpts []server.ServerOption
	serverOpts = append(serverOpts,
		server.WithToolCapabilities(true),
	)

	if *channelMode {
		hooks := &server.Hooks{}
		hooks.AddAfterInitialize(func(
			ctx context.Context, id any,
			req *mcp.InitializeRequest,
			result *mcp.InitializeResult,
		) {
			if result.Capabilities.Experimental == nil {
				result.Capabilities.Experimental = make(map[string]any)
			}
			result.Capabilities.Experimental["claude/channel"] = map[string]any{}
		})
		serverOpts = append(serverOpts,
			server.WithHooks(hooks),
			server.WithInstructions(channelInstructions),
		)
	}

	s := server.NewMCPServer("tmux-mcp", resolveVersion(), serverOpts...)

	emitter := newChannelEmitter(*channelMode, s)

	registerAgenticScope(s, client, emitter)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// ---- Tool registration ----

// registerAgenticScope is the ONE registration path. The binary serves a single
// surface, so there is nothing to choose between and nothing to dispatch on.
//
// The `-scope` flag, TMUX_MCP_SCOPE and the seventeen raw-tmux tools they
// selected are gone, not disabled: the raw surface was an experiment, and an
// agent that wants to drive tmux directly already has the tmux CLI. A flag kept
// alive to accept one value teaches its reader that the others existed and might
// come back, which is the compatibility layer this change refuses to ship.
func registerAgenticScope(s *server.MCPServer, client *tmuxClient, emitter *ChannelEmitter) {
	// Essential Layer 1 tools
	registerListSessions(s, client)
	registerListWindows(s, client)
	registerListPanes(s, client)
	registerCreateSession(s, client)
	registerCreateHeadless(s, client)
	registerSplitPane(s, client)
	registerCapturePane(s, client)
	registerSendKeys(s, client)
	registerExecuteCommand(s, client)
	registerKillSession(s, client)
	registerKillHeadlessServer(s, client)
	registerKillPane(s, client)
	registerScreenshotPane(s, client)
	// All Layer 2 tools
	registerAgentTools(s, client, emitter)
}

// ---- Individual Layer 1 tool registrations ----

func registerListSessions(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("list-sessions",
		mcp.WithDescription("List active tmux sessions. By default lists only the default server. Use headless=true for the isolated headless server, or all=true for both."),
		mcp.WithBoolean("headless",
			mcp.Description("List sessions on the headless server instead of the default server"),
		),
		mcp.WithBoolean("all",
			mcp.Description("List sessions on both the default and headless servers"),
		),
		mcp.WithReadOnlyHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		headless := req.GetBool("headless", false)
		all := req.GetBool("all", false)

		switch {
		case all:
			defaultSessions, err := client.ListSessions(ctx)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("failed to list sessions", err), nil
			}
			headlessSessions, err := client.ListHeadlessSessions(ctx)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("failed to list headless sessions", err), nil
			}
			return jsonResult(append(defaultSessions, headlessSessions...))
		case headless:
			sessions, err := client.ListHeadlessSessions(ctx)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("failed to list headless sessions", err), nil
			}
			return jsonResult(sessions)
		default:
			sessions, err := client.ListSessions(ctx)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("failed to list sessions", err), nil
			}
			return jsonResult(sessions)
		}
	})
}

func registerCreateHeadless(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("create-headless",
		mcp.WithDescription("Create an isolated headless tmux session invisible to the user's tmux ls. Returns IDs prefixed with \"headless:\" that route all subsequent tool calls to the isolated server."),
		mcp.WithString("name",
			mcp.Description("Session name (optional)"),
		),
		mcp.WithString("command",
			mcp.Description("Command to run in the session (optional, defaults to shell)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.GetString("name", "")
		command := req.GetString("command", "")
		created, err := client.CreateHeadlessSession(ctx, name, command)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to create headless session", err), nil
		}
		return jsonResult(created)
	})
}

func registerKillHeadlessServer(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("kill-headless-server",
		mcp.WithDescription("Terminate all headless sessions and shut down the headless tmux server."),
		mcp.WithDestructiveHintAnnotation(true),
	), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		n, err := client.KillHeadlessServer(ctx)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to kill headless server", err), nil
		}
		return jsonResult(struct {
			Killed   bool `json:"killed"`
			Sessions int  `json:"sessions"`
		}{Killed: true, Sessions: n})
	})
}

func registerListWindows(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("list-windows",
		mcp.WithDescription("List windows in a tmux session"),
		mcp.WithString("sessionId",
			mcp.Required(),
			mcp.Description("Session ID (e.g. $0) or name"),
		),
		mcp.WithReadOnlyHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("sessionId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		windows, err := client.ListWindows(ctx, sessionID)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to list windows", err), nil
		}
		return jsonResult(windows)
	})
}

func registerListPanes(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("list-panes",
		mcp.WithDescription("List panes in a tmux window with dimensions, current command, and path"),
		mcp.WithString("windowId",
			mcp.Required(),
			mcp.Description("Window ID (e.g. @0) or target (session:window)"),
		),
		mcp.WithReadOnlyHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		windowID, err := req.RequireString("windowId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		panes, err := client.ListPanes(ctx, windowID)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to list panes", err), nil
		}
		return jsonResult(panes)
	})
}

func registerCapturePane(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("capture-pane",
		mcp.WithDescription("Capture terminal text content from a pane as plain text. Preferred tool for reading command output, logs, and text-based terminal content. Use screenshot-pane only when visual rendering (colors, layout, TUI graphics) matters. paneId is optional: with no paneId it reads helper slot 1, the pane this agent runs commands in."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID (e.g. %0) or target (optional; defaults to helper slot 1)"),
		),
		mcp.WithNumber("lines",
			mcp.Description("Number of lines of history to include (default: pane height)"),
		),
		mcp.WithBoolean("colors",
			mcp.Description("Preserve ANSI color escape sequences"),
		),
		slotProperty(),
		// No readOnlyHint, deliberately, and this is the one place the reason is
		// written out in full — screenshot-pane and pane-state point here.
		//
		// The annotation was correct while paneId was required: the tool read a
		// pane the caller named and changed nothing. On the no-paneId path it now
		// goes through resolveHelper first, and resolution SPLITS the user's
		// window, may adopt one of their idle shells (writing three tmux options
		// into it), and renames the pane it settles on. readOnlyHint is a licence
		// for a client to skip confirmation, to prefetch, or to batch — so an
		// auto-approving client would silently rearrange the user's terminal in
		// order to answer a question about it, which is the one thing the hint
		// promises cannot happen.
		//
		// Do not restore it by pattern-matching list-panes and list-windows. Those
		// take an explicit target, create nothing, and keep the annotation
		// honestly; the difference is the default, not the verb in the name.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		lines := req.GetInt("lines", 0)
		colors := req.GetBool("colors", false)
		content, err := client.CapturePane(ctx, tgt.PaneID, lines, colors)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to capture pane", err), nil
		}
		// The text body is returned untouched. This is the one tool whose entire
		// contract is "the pane's exact content", so prepending a resolution
		// header would corrupt the answer and break every assertion anyone has
		// written against it. The metadata rides alongside in structuredContent,
		// which costs the text nothing; clients that ignore it lose only the
		// pane id, which the design says the agent should never need.
		//
		// And it rides there ONLY when a slot was resolved. paneResolution.PaneID
		// has no omitempty — it cannot have one, because the resolved case must
		// always report which pane it picked — so an unconditional assignment gives
		// an explicit-paneId call a structuredContent key this tool never had,
		// telling the caller the id it just passed in. Explicit paneId answers
		// exactly as it did before slots existed, on every tool; that is a wire
		// promise, and one that a top-level key silently added to two of them
		// breaks. See TestExplicitPaneIdResponsesAreUnchanged.
		res := mcp.NewToolResultText(content)
		if tgt.Resolved() {
			res.StructuredContent = tgt.resolution()
		}
		return res, nil
	})
}

func registerCreateSession(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("create-session",
		mcp.WithDescription("Create a new tmux session"),
		mcp.WithString("name",
			mcp.Description("Session name (optional)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.GetString("name", "")
		created, err := client.CreateSession(ctx, name)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to create session", err), nil
		}
		return jsonResult(created)
	})
}

func registerSplitPane(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("split-pane",
		mcp.WithDescription("Get a pane to work in, beside the agent, in the window the user is already looking at. With no arguments it returns helper slot 1, creating it if needed; slot:2, slot:3 … give further panes. Repeated calls for the same slot return the SAME pane (\"reused\": true), so a process started there is still there next time. Pass paneId only to split a specific pane, in which case direction and size apply and the new pane is unslotted."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID to split (optional; defaults to the pane this server runs in)"),
		),
		mcp.WithString("direction",
			mcp.Description(`Split direction: "horizontal" (side-by-side) or "vertical" (top-bottom, default)`),
			mcp.Enum("horizontal", "vertical"),
		),
		mcp.WithNumber("size",
			mcp.Description("Size of the new pane as a percentage (1-99, default 50). Only applies when paneId is given; on the slot path the server chooses the placement."),
		),
		slotProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		// Slot path: the resolver has already produced the pane, placed by the
		// server's own rules. Reused is set from Created so a consumer that
		// predates slots and keys on "reused" keeps reading a true answer.
		if tgt.Resolved() {
			windowID, _ := client.getWindowIDForPane(ctx, tgt.PaneID)
			return jsonResult(&CreatedPane{
				PaneID:   tgt.PaneID,
				WindowID: windowID,
				Reused:   !tgt.Created,
				Slot:     tgt.Slot,
				Created:  tgt.Created,
			})
		}

		// Explicit-paneId path, unchanged. Note that paneId means something
		// different on this tool than on every other one: it is the pane to
		// SPLIT — an anchor — not the pane the caller wants back. That is why
		// direction and size exist here and nowhere else, and why this branch
		// does its own work instead of using tgt.PaneID as a destination.
		paneID := tgt.PaneID
		direction := req.GetString("direction", "vertical")
		size := req.GetInt("size", 0)

		// Try to reuse an existing idle pane in the same window.
		if idlePaneID, findErr := client.findIdlePaneInWindow(ctx, paneID); findErr == nil && idlePaneID != "" {
			windowID, _ := client.getWindowIDForPane(ctx, idlePaneID)
			return jsonResult(&CreatedPane{
				PaneID:   idlePaneID,
				WindowID: windowID,
				Reused:   true,
			})
		}

		created, err := client.SplitPane(ctx, paneID, direction, size)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to split pane", err), nil
		}
		return jsonResult(created)
	})
}

func registerSendKeys(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("send-keys",
		// Three facts an agent needs before its first bare send-keys, and the
		// last of them is the one nobody expects: with no paneId this call can
		// ADOPT a pane the user opened. The adoption rules refuse anything that
		// is not an idle same-user shell, but "idle" is judged from the outside
		// and cannot see text the user typed without pressing Enter — see
		// canAcquire. Saying so here is the difference between a documented
		// trade-off and a surprise.
		mcp.WithDescription("Send keystrokes to a pane. By default treats input as literal text. Set literal=false to interpret tmux key names (e.g. C-c, Enter, Escape). paneId is optional: with no paneId the keys go to helper slot 1, which is CREATED beside the agent if it does not exist yet, or ADOPTED from an idle unused shell already open in the same window. It is never the agent's own pane. Pass an explicit paneId whenever you already know the pane."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID to send keys to (optional; defaults to helper slot 1)"),
		),
		mcp.WithString("keys",
			mcp.Required(),
			mcp.Description("Keys to send"),
		),
		mcp.WithBoolean("literal",
			mcp.Description("Treat keys as literal text rather than tmux key names (default true)"),
		),
		mcp.WithBoolean("enter",
			mcp.Description("Append an Enter keystroke after the keys (default false)"),
		),
		slotProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// keys is validated before the pane is resolved, and the order matters:
		// resolution can CREATE a pane, and a call that is going to be rejected
		// for a missing argument must not leave a new split behind.
		keys, err := req.RequireString("keys")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		literal := req.GetBool("literal", true)
		enter := req.GetBool("enter", false)
		if err := client.SendKeys(ctx, tgt.PaneID, keys, literal, enter); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to send keys", err), nil
		}
		return jsonResult(tgt.resolution())
	})
}

func registerExecuteCommand(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("execute-command",
		mcp.WithDescription("Run a shell command in a pane and wait for it to complete. Returns the full output and exit code. paneId is optional: with no paneId the command runs in helper slot 1, a pane beside the agent that is created or adopted on first use and reused after that — never the agent's own pane. When headless=true and no paneId is provided, a temporary isolated session is created instead, the command runs, and the session is cleaned up automatically (no paneId in response)."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID to run the command in (optional; defaults to helper slot 1)"),
		),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Shell command to execute"),
		),
		mcp.WithBoolean("headless",
			mcp.Description("When true and no paneId is provided, auto-create a headless session, run the command, return output, and clean up. Default false."),
		),
		mcp.WithNumber("timeoutSeconds",
			mcp.Description("Maximum seconds to wait for the command to complete before returning with timedOut:true and partial output (default: no timeout)"),
		),
		slotProperty(),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// command first: resolution can create a pane, and a call that will be
		// rejected for a missing argument must not leave a split behind.
		command, err := req.RequireString("command")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// This tool used to answer "paneId is required when headless=false", and
		// that refusal is exactly the incident this design comes from: an agent
		// told it may not run a command without naming a pane goes looking for
		// $TMUX_PANE and starts driving raw tmux. It delivers keystrokes like
		// send-keys does, so it resolves like send-keys does.
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{AllowHeadless: true})
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

		if tgt.Headless {
			// One-shot headless execution: create session, run, destroy.
			created, err := client.CreateHeadlessSession(ctx, "", "")
			if err != nil {
				return mcp.NewToolResultErrorFromErr("failed to create headless session", err), nil
			}
			defer func() {
				_ = client.KillSession(context.Background(), created.SessionID)
			}()
			result, err := client.ExecuteCommand(ctx, created.PaneID, command)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("failed to execute command", err), nil
			}
			// Return output without paneId — the session is gone.
			return jsonResult(struct {
				Output   string `json:"output"`
				ExitCode int    `json:"exitCode"`
				TimedOut bool   `json:"timedOut,omitempty"`
			}{Output: result.Output, ExitCode: result.ExitCode, TimedOut: result.TimedOut})
		}

		result, err := client.ExecuteCommand(ctx, tgt.PaneID, command)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to execute command", err), nil
		}
		result.Slot, result.Created = tgt.Slot, tgt.Created
		return jsonResult(result)
	})
}

func registerKillSession(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("kill-session",
		mcp.WithDescription("Kill a tmux session and all its windows and panes"),
		mcp.WithString("sessionId",
			mcp.Required(),
			mcp.Description("Session ID or name to kill"),
		),
		mcp.WithDestructiveHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("sessionId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := client.KillSession(ctx, sessionID); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to kill session", err), nil
		}
		return jsonResult(struct {
			Killed string `json:"killed"`
		}{Killed: sessionID})
	})
}

func registerKillPane(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("kill-pane",
		mcp.WithDescription("Kill a tmux pane"),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID to kill"),
		),
		mcp.WithDestructiveHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := client.KillPane(ctx, paneID); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to kill pane", err), nil
		}
		return jsonResult(struct {
			Killed string `json:"killed"`
		}{Killed: paneID})
	})
}

func registerScreenshotPane(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("screenshot-pane",
		mcp.WithDescription("Render a visual PNG screenshot of a terminal pane with full ANSI colors, styles, and layout via xterm.js. Returns an image the model can see. Use ONLY when visual appearance matters (TUI layouts, color-coded output, ANSI art). For reading text content, prefer capture-pane instead. paneId is optional: with no paneId it renders helper slot 1."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID (e.g. %0) or target (optional; defaults to helper slot 1)"),
		),
		mcp.WithString("theme",
			mcp.Description(`Color theme: "dark" (default) or "light"`),
			mcp.Enum("dark", "light"),
		),
		mcp.WithString("output",
			mcp.Description(`Output mode: default returns a PNG image; "browser" opens in system browser; "html" returns raw HTML as text`),
			mcp.Enum("browser", "html"),
		),
		slotProperty(),
		// No readOnlyHint: with no paneId this renders whatever resolution hands
		// back, and resolution can split the user's window, adopt one of their
		// shells and rename a pane. See capture-pane for the full reason.
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tgt, err := client.resolvePaneArg(ctx, req, paneArgSpec{})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		theme := req.GetString("theme", "dark")
		output := req.GetString("output", "")

		res, err := handleScreenshotPane(ctx, client, tgt.PaneID, theme, output)
		if err != nil {
			return res, err
		}
		// Same reasoning as capture-pane, including the condition: the content is
		// an image (or the HTML fallback) and must stay exactly that, so the
		// resolution rides in structuredContent — and only when there was a
		// resolution. A call that named its pane gets back what it always got.
		if res != nil && tgt.Resolved() {
			res.StructuredContent = tgt.resolution()
		}
		return res, nil
	})
}

// ---- Helpers ----

// jsonResult marshals v to indented JSON and returns it as a tool result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("failed to serialize result", err), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}
