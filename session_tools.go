package main

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerSessionTools registers the six high-level session tools.
// These wrap the headless tmux layer so callers never see tmux pane IDs.
func registerSessionTools(s *server.MCPServer, client *tmuxClient, registry *sessionRegistry) {
	registerRun(s, client)
	registerSessionStart(s, client, registry)
	registerSessionSend(s, client, registry)
	registerSessionRead(s, client, registry)
	registerSessionClose(s, client, registry)
	registerSessionList(s, client, registry)
}

// ---- run ----

func registerRun(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("run",
		mcp.WithDescription("Start a command in a new isolated headless session. Waits for the command to complete, then returns the output and exit code. The session is cleaned up automatically."),
		mcp.WithString("command",
			mcp.Required(),
			mcp.Description("Shell command to execute"),
		),
		mcp.WithNumber("timeout",
			mcp.Description("Seconds to wait for the command to complete (default 30)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		command, err := req.RequireString("command")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		timeoutSecs := req.GetInt("timeout", 30)

		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
		defer cancel()

		result, err := client.Run(ctx, command)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("run failed", err), nil
		}
		return jsonResult(result)
	})
}

// ---- session-start ----

func registerSessionStart(s *server.MCPServer, client *tmuxClient, registry *sessionRegistry) {
	s.AddTool(mcp.NewTool("session-start",
		mcp.WithDescription("Start a long-lived headless session. Optionally launch an initial command (e.g. a REPL). Returns a sessionId for use with session-send, session-read, and session-close."),
		mcp.WithString("command",
			mcp.Description("Command to start in the session (optional, e.g. \"python3\", \"psql\")"),
		),
		mcp.WithString("name",
			mcp.Description("Human-readable name for the session (optional)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		command := req.GetString("command", "")
		name := req.GetString("name", "")

		result, err := client.SessionStart(ctx, registry, command, name)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("session-start failed", err), nil
		}
		return jsonResult(result)
	})
}

// ---- session-send ----

func registerSessionSend(s *server.MCPServer, client *tmuxClient, registry *sessionRegistry) {
	s.AddTool(mcp.NewTool("session-send",
		mcp.WithDescription("Send input to a running headless session."),
		mcp.WithString("sessionId",
			mcp.Required(),
			mcp.Description("Session ID returned by session-start"),
		),
		mcp.WithString("input",
			mcp.Required(),
			mcp.Description("Text to send to the session"),
		),
		mcp.WithBoolean("enter",
			mcp.Description("Press Enter after the input (default true)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("sessionId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		input, err := req.RequireString("input")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		enter := req.GetBool("enter", true)

		if err := client.SessionSend(ctx, registry, sessionID, input, enter); err != nil {
			return mcp.NewToolResultErrorFromErr("session-send failed", err), nil
		}
		return jsonResult(struct {
			SessionID string `json:"sessionId"`
		}{SessionID: sessionID})
	})
}

// ---- session-read ----

func registerSessionRead(s *server.MCPServer, client *tmuxClient, registry *sessionRegistry) {
	s.AddTool(mcp.NewTool("session-read",
		mcp.WithDescription("Read current output from a headless session. Returns the visible screen content and the pane's process state."),
		mcp.WithString("sessionId",
			mcp.Required(),
			mcp.Description("Session ID returned by session-start"),
		),
		mcp.WithNumber("lines",
			mcp.Description("Lines of scrollback history to include (default: visible screen only)"),
		),
		mcp.WithReadOnlyHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("sessionId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		lines := req.GetInt("lines", 0)

		result, err := client.SessionRead(ctx, registry, sessionID, lines)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("session-read failed", err), nil
		}
		return jsonResult(result)
	})
}

// ---- session-close ----

func registerSessionClose(s *server.MCPServer, client *tmuxClient, registry *sessionRegistry) {
	s.AddTool(mcp.NewTool("session-close",
		mcp.WithDescription("Close a headless session and clean up its resources."),
		mcp.WithString("sessionId",
			mcp.Required(),
			mcp.Description("Session ID returned by session-start"),
		),
		mcp.WithDestructiveHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessionID, err := req.RequireString("sessionId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		result, err := client.SessionClose(ctx, registry, sessionID)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("session-close failed", err), nil
		}
		return jsonResult(result)
	})
}

// ---- session-list ----

func registerSessionList(s *server.MCPServer, client *tmuxClient, registry *sessionRegistry) {
	s.AddTool(mcp.NewTool("session-list",
		mcp.WithDescription("List all active headless sessions managed by this server."),
		mcp.WithReadOnlyHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sessions, err := client.SessionList(ctx, registry)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("session-list failed", err), nil
		}
		return jsonResult(sessions)
	})
}
