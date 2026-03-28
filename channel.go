package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/server"
)

// channelInstructions is injected as server instructions when channel mode is enabled.
const channelInstructions = `tmux-mcp is running in channel mode.

You will receive proactive notifications for tmux pane events via
notifications/claude/channel. Each notification includes:
  - content: human-readable event summary (plain string)
  - meta.paneId: the tmux pane where the event occurred
  - meta.event: event type (exit, error, user_input, shell, idle:N,
                            bell, pattern:<regex>, timeout)
  - meta.detail: explanation of what triggered the event
  - meta.exitCode: process exit code (for exit events)
  - meta.isAlive: whether the process is still running

When you receive a channel notification:
1. Identify which pane and what event occurred.
2. For exit events: check the exit code; 0 = success, non-zero = failure.
3. For error events: the output matched an error pattern; inspect the pane.
4. For user_input events: a process is waiting for stdin; decide whether to
   send input via send-keys or kill the process.
5. For timeout events: your watch-pane call expired; consider re-watching
   or capturing the pane directly.

Use capture-pane or watch-pane to get current output when needed.`

// ChannelEmitter sends channel notifications to the connected Claude Code client.
// When disabled, all methods are no-ops so callers need no conditional logic.
type ChannelEmitter struct {
	enabled bool
	s       *server.MCPServer
}

// newChannelEmitter creates an emitter. When channelMode is false the
// returned emitter's Emit method is always a no-op.
func newChannelEmitter(channelMode bool, s *server.MCPServer) *ChannelEmitter {
	return &ChannelEmitter{enabled: channelMode, s: s}
}

// Emit sends a notifications/claude/channel notification for the given result.
// Safe to call on a nil or disabled emitter.
func (e *ChannelEmitter) Emit(ctx context.Context, result *WatchResult) {
	if e == nil || !e.enabled {
		return
	}
	params := map[string]any{
		"content": channelText(result),
		"meta":    channelMeta(result),
	}
	_ = e.s.SendNotificationToClient(ctx, "notifications/claude/channel", params)
}

// channelText returns the human-readable content string for a channel notification.
func channelText(r *WatchResult) string {
	if r.PaneState != nil && r.Event == "exit" {
		return fmt.Sprintf("tmux pane %s: process exited (code %d). %s",
			r.PaneID, r.PaneState.ExitCode, r.Detail)
	}
	return fmt.Sprintf("tmux pane %s: %s — %s", r.PaneID, r.Event, r.Detail)
}

// channelMeta returns the structured meta object for a channel notification.
// All values are strings as required by the Claude Code channel spec.
func channelMeta(r *WatchResult) map[string]any {
	m := map[string]any{
		"paneId":      r.PaneID,
		"event":       r.Event,
		"detail":      r.Detail,
		"elapsedSecs": fmt.Sprintf("%.2f", r.Elapsed),
	}
	if r.PaneState != nil {
		m["exitCode"] = fmt.Sprintf("%d", r.PaneState.ExitCode)
		m["isAlive"] = fmt.Sprintf("%v", r.PaneState.IsAlive)
	}
	return m
}
