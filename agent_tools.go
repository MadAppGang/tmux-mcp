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
		mcp.WithDescription("Start a command in a pane and monitor its output. Blocks until a readiness pattern matches, a named trigger fires, or the timeout expires. When paneId is omitted, a new pane is created automatically (headless=true creates an isolated headless pane)."),
		mcp.WithString("paneId",
			mcp.Description("Pane ID (e.g. %0) to run the command in. If omitted, a new pane is created automatically."),
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
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID := req.GetString("paneId", "")
		headless := req.GetBool("headless", false)

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

		// If no paneId provided, auto-create one.
		if paneID == "" {
			if headless {
				created, err := client.CreateHeadlessSession(ctx, "", "")
				if err != nil {
					return nil, fmt.Errorf("failed to create headless session: %w", err)
				}
				paneID = created.PaneID
			} else {
				created, err := client.CreateSession(ctx, "")
				if err != nil {
					return nil, fmt.Errorf("failed to create session: %w", err)
				}
				paneID = created.PaneID
			}
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

		// Send the command to the pane.
		if err := client.SendKeys(ctx, paneID, command, true, true); err != nil {
			return nil, fmt.Errorf("failed to send command: %w", err)
		}

		result, err := monitorPane(ctx, s, client, paneID, mode, triggers, timeoutSecs, emitter)
		if err != nil {
			return nil, err
		}
		return watchResultToToolResult(result)
	})
}

// ---- watch-pane ----

func registerWatchPane(s *server.MCPServer, client *tmuxClient, emitter *ChannelEmitter) {
	s.AddTool(mcp.NewTool("watch-pane",
		mcp.WithDescription("Monitor a pane using smart triggers. Blocks until a trigger fires or the timeout expires."),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID (e.g. %0) to monitor"),
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
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return nil, err
		}

		modeName := req.GetString("mode", "medium")
		triggerSpec := req.GetString("triggers", "")
		timeoutSecs := req.GetInt("timeout", 60)

		mode := resolveMode(modeName)
		triggers := parseTriggers(triggerSpec, client)

		result, err := monitorPane(ctx, s, client, paneID, mode, triggers, timeoutSecs, emitter)
		if err != nil {
			return nil, err
		}
		return watchResultToToolResult(result)
	})
}

// ---- pane-state ----

func registerPaneState(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("pane-state",
		mcp.WithDescription("Get native OS-level process state for a pane. Returns whether the foreground process is alive and whether it is waiting for user input (detected via OS-level process inspection, not regex)."),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID (e.g. %0) to inspect"),
		),
		mcp.WithReadOnlyHintAnnotation(true),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		state, err := client.GetPaneState(ctx, paneID)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("failed to get pane state", err), nil
		}
		return jsonResult(state)
	})
}

// ---- run-in-repl ----

func registerRunInREPL(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("run-in-repl",
		mcp.WithDescription("Send input to a running REPL and wait for the prompt to reappear, then return the output. Works with Python, Node, psql, bash, etc."),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID containing the running REPL"),
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
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
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
						return jsonResult(struct {
							PaneID string `json:"paneId"`
							Output string `json:"output"`
							Exited bool   `json:"exited"`
						}{PaneID: paneID, Output: lastNewContent, Exited: true})
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
						return jsonResult(struct {
							PaneID string `json:"paneId"`
							Output string `json:"output"`
						}{PaneID: paneID, Output: result})
					}
				}

				// Check if the REPL process has exited.
				paneState, _ := client.GetPaneState(ctx, paneID)
				if paneState != nil {
					if !paneState.IsAlive || (initialCmd != "" && paneState.ForegroundCmd != initialCmd) {
						return jsonResult(struct {
							PaneID string `json:"paneId"`
							Output string `json:"output"`
							Exited bool   `json:"exited"`
						}{PaneID: paneID, Output: strings.TrimSpace(newContent), Exited: true})
					}
				}

				if tick.After(deadline) {
					// Return whatever we have with a timeout note.
					return jsonResult(struct {
						PaneID string `json:"paneId"`
						Output string `json:"output"`
					}{PaneID: paneID, Output: fmt.Sprintf("[timeout after %ds]\n%s", timeoutSecs, newContent)})
				}
			}
		}
	})
}

// ---- write-to-display ----

func registerWriteToDisplay(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("write-to-display",
		mcp.WithDescription("Write text to a pane as a side-channel coaching display. The user sees it in their terminal; the tool returns only 'Display updated' so the text does not enter the model's context."),
		mcp.WithString("paneId",
			mcp.Required(),
			mcp.Description("Pane ID to write to"),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("Text to display in the pane"),
		),
		mcp.WithBoolean("clear",
			mcp.Description("Clear the pane before writing (default false)"),
		),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paneID, err := req.RequireString("paneId")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		text, err := req.RequireString("text")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		clear := req.GetBool("clear", false)

		if clear {
			// Kill any pending input already in the line buffer (C-u = kill line),
			// then run the clear command. This prevents previously-typed literal
			// text from being concatenated with "clear" in the shell's line editor.
			if err := client.SendKeys(ctx, paneID, "C-u", false, false); err != nil {
				return mcp.NewToolResultErrorFromErr("failed to clear line buffer", err), nil
			}
			if err := client.SendKeys(ctx, paneID, "clear", false, true); err != nil {
				return mcp.NewToolResultErrorFromErr("failed to clear pane", err), nil
			}
			// Brief pause so the clear completes before we write.
			time.Sleep(150 * time.Millisecond)
		}

		if err := client.SendKeys(ctx, paneID, text, true, false); err != nil {
			return mcp.NewToolResultErrorFromErr("failed to write to display", err), nil
		}
		return jsonResult(struct {
			PaneID string `json:"paneId"`
		}{PaneID: paneID})
	})
}

// ---- display-message ----

func registerDisplayMessage(s *server.MCPServer, client *tmuxClient) {
	s.AddTool(mcp.NewTool("display-message",
		mcp.WithDescription("Show a transient notification in the tmux status bar."),
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
