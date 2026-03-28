package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	shellType := flag.String("shell-type", "bash", "Shell type for exit code capture (bash/zsh/fish)")
	scope := flag.String("scope", "", "Tool scope: agentic (default), primitives, or all")
	channelMode := flag.Bool("channel", false, "Enable Claude Code channel mode: push tmux events as channel notifications")
	flag.Parse()

	// Resolve scope: flag > env > default
	scopeVal := *scope
	if scopeVal == "" {
		scopeVal = os.Getenv("TMUX_MCP_SCOPE")
	}
	if scopeVal == "" {
		scopeVal = "agentic"
	}

	switch scopeVal {
	case "agentic", "primitives", "all":
		// valid
	default:
		log.Fatalf("invalid scope %q: must be one of agentic, primitives, or all", scopeVal)
	}

	// Clean up any stale headless socket from a previous crash.
	CleanStaleHeadlessSocket()

	client := newTmuxClient(*shellType)

	var serverOpts []server.ServerOption
	serverOpts = append(serverOpts,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
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

	s := server.NewMCPServer("tmux-mcp", "1.0.0", serverOpts...)

	emitter := newChannelEmitter(*channelMode, s)

	switch scopeVal {
	case "primitives":
		registerTools(s, client, emitter)
	case "agentic":
		registerAgenticScope(s, client, emitter)
	case "all":
		registerTools(s, client, emitter)
		registerAgentTools(s, client, emitter)
	}

	registerResources(s, client)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// ---- Tool registration ----

// registerTools registers all 14 Layer 1 (primitive) tools.
func registerTools(s *server.MCPServer, client *tmuxClient, emitter *ChannelEmitter) {
	registerListSessions(s, client)
	registerCreateHeadless(s, client)
	registerKillHeadlessServer(s, client)
	registerListWindows(s, client)
	registerListPanes(s, client)
	registerCapturePane(s, client)
	registerCreateSession(s, client)
	registerCreateWindow(s, client)
	registerSplitPane(s, client)
	registerSendKeys(s, client)
	registerExecuteCommand(s, client)
	registerResizePane(s, client)
	registerRenameSession(s, client)
	registerKillSession(s, client)
	registerKillWindow(s, client)
	registerKillPane(s, client)
}

// registerAgenticScope registers the 6 Layer 2 tools plus 7 essential Layer 1
// tools. This is the default scope.
func registerAgenticScope(s *server.MCPServer, client *tmuxClient, emitter *ChannelEmitter) {
	// Essential Layer 1 tools
	registerListSessions(s, client)
	registerCreateSession(s, client)
	registerCreateHeadless(s, client)
	registerCapturePane(s, client)
	registerSendKeys(s, client)
	registerExecuteCommand(s, client)
	registerKillSession(s, client)
	registerKillHeadlessServer(s, client)
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
		mcp.WithDescription("Capture terminal content from a pane"),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID (e.g. %0) or target"),
		),
		mcp.WithNumber("lines",
			mcp.Description("Number of lines of history to include (default: pane height)"),
		),
		mcp.WithBoolean("colors",
			mcp.Description("Preserve ANSI color escape sequences"),
		),
		mcp.WithReadOnlyHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		lines := req.GetInt("lines", 0)
		colors := req.GetBool("colors", false)
		content, err := client.CapturePane(ctx, paneID, lines, colors)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to capture pane", err), nil
		}
		return mcp.NewToolResultText(content), nil
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

func registerCreateWindow(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("create-window",
		mcp.WithDescription("Create a new window in a tmux session"),
		mcp.WithString("sessionId",
			mcp.Required(),
			mcp.Description("Session ID or name"),
		),
		mcp.WithString("name",
			mcp.Description("Window name (optional)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("sessionId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		name := req.GetString("name", "")
		created, err := client.CreateWindow(ctx, sessionID, name)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to create window", err), nil
		}
		return jsonResult(created)
	})
}

func registerSplitPane(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("split-pane",
		mcp.WithDescription("Split a tmux pane horizontally or vertically"),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID to split"),
		),
		mcp.WithString("direction",
			mcp.Description(`Split direction: "horizontal" (side-by-side) or "vertical" (top-bottom, default)`),
			mcp.Enum("horizontal", "vertical"),
		),
		mcp.WithNumber("size",
			mcp.Description("Size of the new pane as a percentage (1-99, default 50)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		direction := req.GetString("direction", "vertical")
		size := req.GetInt("size", 0)
		created, err := client.SplitPane(ctx, paneID, direction, size)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to split pane", err), nil
		}
		return jsonResult(created)
	})
}

func registerSendKeys(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("send-keys",
		mcp.WithDescription("Send keystrokes to a pane. By default treats input as literal text. Set literal=false to interpret tmux key names (e.g. C-c, Enter, Escape)"),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID to send keys to"),
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
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		keys, err := req.RequireString("keys")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		literal := req.GetBool("literal", true)
		enter := req.GetBool("enter", false)
		if err := client.SendKeys(ctx, paneID, keys, literal, enter); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to send keys", err), nil
		}
		return jsonResult(struct {
			PaneID string `json:"paneId"`
		}{PaneID: paneID})
	})
}

func registerExecuteCommand(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("execute-command",
		mcp.WithDescription("Run a shell command in a pane and wait for it to complete. Returns the full output and exit code. When headless=true and no paneId is provided, a temporary isolated session is created, the command runs, and the session is cleaned up automatically (no paneId in response)."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID to run the command in. Required when headless=false."),
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
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		command, err := req.RequireString("command")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		headless := req.GetBool("headless", false)
		paneID := req.GetString("paneId", "")

		// Apply per-call timeout if provided.
		timeoutSecs := req.GetInt("timeoutSeconds", 0)
		if timeoutSecs > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
			defer cancel()
		}

		if headless && paneID == "" {
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

		if paneID == "" {
			return mcp.NewToolResultError("paneId is required when headless=false"), nil
		}
		result, err := client.ExecuteCommand(ctx, paneID, command)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to execute command", err), nil
		}
		return jsonResult(result)
	})
}

func registerResizePane(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("resize-pane",
		mcp.WithDescription("Resize a tmux pane. Use width+height for absolute size, or direction+amount for relative adjustment"),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID to resize"),
		),
		mcp.WithNumber("width",
			mcp.Description("Absolute width in columns (use with height for absolute resize)"),
		),
		mcp.WithNumber("height",
			mcp.Description("Absolute height in rows (use with width for absolute resize)"),
		),
		mcp.WithString("direction",
			mcp.Description("Resize direction for relative resize: U (up), D (down), L (left), R (right)"),
			mcp.Enum("U", "D", "L", "R"),
		),
		mcp.WithNumber("amount",
			mcp.Description("Amount of cells to resize by (for relative resize, default 5)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		args := req.GetArguments()
		_, hasWidth := args["width"]
		_, hasHeight := args["height"]
		_, hasDirection := args["direction"]

		switch {
		case hasWidth && hasHeight:
			width := req.GetInt("width", 0)
			height := req.GetInt("height", 0)
			if width <= 0 || height <= 0 {
				return mcp.NewToolResultError("width and height must be positive integers"), nil
			}
			if err := client.ResizePaneAbsolute(ctx, paneID, width, height); err != nil {
				return mcp.NewToolResultErrorFromErr("failed to resize pane", err), nil
			}
		case hasDirection:
			direction, _ := req.RequireString("direction")
			amount := req.GetInt("amount", 5)
			if err := client.ResizePaneRelative(ctx, paneID, direction, amount); err != nil {
				return mcp.NewToolResultErrorFromErr("failed to resize pane", err), nil
			}
		default:
			return mcp.NewToolResultError("provide either width+height (absolute) or direction+amount (relative)"), nil
		}
		return jsonResult(struct {
			PaneID string `json:"paneId"`
		}{PaneID: paneID})
	})
}

func registerRenameSession(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("rename-session",
		mcp.WithDescription("Rename a tmux session"),
		mcp.WithString("sessionId",
			mcp.Required(),
			mcp.Description("Session ID or current name"),
		),
		mcp.WithString("newName",
			mcp.Required(),
			mcp.Description("New session name"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("sessionId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		newName, err := req.RequireString("newName")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := client.RenameSession(ctx, sessionID, newName); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to rename session", err), nil
		}
		return jsonResult(struct {
			SessionID string `json:"sessionId"`
			Name      string `json:"name"`
		}{SessionID: sessionID, Name: newName})
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

func registerKillWindow(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("kill-window",
		mcp.WithDescription("Kill a tmux window and all its panes"),
		mcp.WithString("windowId",
			mcp.Required(),
			mcp.Description("Window ID or target to kill"),
		),
		mcp.WithDestructiveHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		windowID, err := req.RequireString("windowId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := client.KillWindow(ctx, windowID); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to kill window", err), nil
		}
		return jsonResult(struct {
			Killed string `json:"killed"`
		}{Killed: windowID})
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

// ---- Resource registration ----

func registerResources(s *server.MCPServer, client *tmuxClient) {
	// tmux://sessions — static resource
	s.AddResource(
		mcp.NewResource("tmux://sessions", "tmux sessions",
			mcp.WithResourceDescription("List of all active tmux sessions"),
			mcp.WithMIMEType("application/json"),
		),
		func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			sessions, err := client.ListSessions(ctx)
			if err != nil {
				return nil, err
			}
			data, err := json.Marshal(sessions)
			if err != nil {
				return nil, fmt.Errorf("marshal sessions: %w", err)
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      "tmux://sessions",
					MIMEType: "application/json",
					Text:     string(data),
				},
			}, nil
		},
	)

	// tmux://pane/{paneId} — template resource
	s.AddResourceTemplate(
		mcp.NewResourceTemplate("tmux://pane/{paneId}", "tmux pane content",
			mcp.WithTemplateDescription("Current terminal content of a tmux pane"),
			mcp.WithTemplateMIMEType("text/plain"),
		),
		func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			paneID := extractTemplateVar(req.Params.URI, "tmux://pane/")
			if paneID == "" {
				return nil, fmt.Errorf("missing pane ID in URI: %s", req.Params.URI)
			}
			content, err := client.CapturePane(ctx, paneID, 0, false)
			if err != nil {
				return nil, err
			}
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      req.Params.URI,
					MIMEType: "text/plain",
					Text:     content,
				},
			}, nil
		},
	)
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

// extractTemplateVar strips a URI prefix to obtain the variable portion.
func extractTemplateVar(uri, prefix string) string {
	if len(uri) <= len(prefix) {
		return ""
	}
	return uri[len(prefix):]
}
