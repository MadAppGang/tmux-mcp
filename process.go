package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// PaneState captures the OS-level state of a tmux pane's process.
type PaneState struct {
	PanePID         int    `json:"panePid"`
	ForegroundPID   int    `json:"foregroundPid"`
	ForegroundCmd   string `json:"foregroundCmd"`
	IsAlive         bool   `json:"isAlive"`
	WaitingForInput bool   `json:"waitingForInput"`
	ExitCode        int    `json:"exitCode,omitempty"`
}

// GetPaneState returns the native OS-level state of the process in a tmux pane.
// It queries tmux for the pane's PID, dead flag, and dead status in a single
// call, then uses OS-specific inspection to determine whether the foreground
// process is alive and waiting for input.
func (t *tmuxClient) GetPaneState(ctx context.Context, paneID string) (*PaneState, error) {
	// Query pid, dead flag, and dead exit status in a single tmux call.
	out, err := t.run(ctx, "display-message", "-p", "-t", paneID,
		"#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}")
	if err != nil {
		return nil, fmt.Errorf("get pane state: %w", err)
	}

	// run() already strips trailing newlines; split on tab to get the three fields.
	// Do NOT TrimSpace the whole string since the trailing tab (empty dead_status)
	// would be stripped, yielding only 2 fields instead of 3.
	parts := strings.SplitN(out, "\t", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("unexpected tmux output for pane state: %q", out)
	}

	pidStr := strings.TrimSpace(parts[0])
	deadFlag := strings.TrimSpace(parts[1])
	deadStatusStr := strings.TrimSpace(parts[2])

	// If the pane PID is empty the pane does not exist.
	if pidStr == "" {
		return nil, fmt.Errorf("pane %s does not exist or has no PID", paneID)
	}

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return nil, fmt.Errorf("parse pane pid %q: %w", pidStr, err)
	}

	// If the pane is dead, return immediately without OS-level process inspection.
	if deadFlag == "1" {
		exitCode, _ := strconv.Atoi(deadStatusStr)
		return &PaneState{
			PanePID:  pid,
			IsAlive:  false,
			ExitCode: exitCode,
		}, nil
	}

	state := &PaneState{PanePID: pid}
	if err := fillPaneState(ctx, state); err != nil {
		// Treat inspection errors as "alive, no input-wait detected" rather
		// than hard failures — the pane is still usable even if we cannot
		// determine the precise state.
		state.IsAlive = true
	}
	return state, nil
}
