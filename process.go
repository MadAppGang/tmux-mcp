package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// errUIDUnsupported is what processUIDs returns on a platform where we cannot
// resolve a process's owner. See process_other.go for why that is an error and
// not a value — it is the difference between a guard and the appearance of one.
var errUIDUnsupported = errors.New("process uid lookup is unsupported on this platform")

// PaneState captures the OS-level state of a tmux pane's process.
type PaneState struct {
	PanePID         int    `json:"panePid"`
	ForegroundPID   int    `json:"foregroundPid"`
	ForegroundCmd   string `json:"foregroundCmd"`
	IsAlive         bool   `json:"isAlive"`
	WaitingForInput bool   `json:"waitingForInput"`
	ExitCode        int    `json:"exitCode,omitempty"`
}

// procInfo is the minimum a platform must report about a process for
// foregroundOfGroup to choose between them.
type procInfo struct {
	PID  int
	Comm string
}

// foregroundOfGroup picks the process that represents a terminal's foreground
// process group: its leader, the process whose PID equals the group ID.
//
// The obvious alternative — take the highest PID in the group, i.e. the most
// recently spawned process — is wrong, and wrong in a way that only shows up on
// a developer's machine. A shell with a rich prompt forks helpers from its
// precmd hooks (powerlevel10k runs git for the VCS segment, version probes for
// the language segments) and those children run *inside the shell's own process
// group*, not a job of their own. So the newest PID in the group is almost
// always a prompt helper, and a pane sitting idle at its prompt reports
// foregroundCmd "git" or "go". Every caller that asks "is this pane free?" is
// then told no, forever. A bare CI shell forks nothing, which is exactly why
// this passes in CI and fails locally.
//
// The leader is the stable answer: for an idle shell the tty's foreground group
// is the shell's own group, whose leader is the shell. For a real job the shell
// puts it in a new process group, whose leader is the job.
//
// members may be in any order. If the leader has already exited but the group
// still has members — a pipeline whose head finished first — fall back to the
// most recent survivor, which is the best guess available.
func foregroundOfGroup(members []procInfo, pgid int) (pid int, comm string) {
	for _, m := range members {
		if m.PID == pgid {
			return m.PID, m.Comm
		}
	}
	for _, m := range members {
		if m.PID > pid {
			pid, comm = m.PID, m.Comm
		}
	}
	return pid, comm
}

// GetPaneState returns the native OS-level state of the process in a tmux pane.
// It queries tmux for the pane's PID, dead flag, and dead status in a single
// call, then uses OS-specific inspection to determine whether the foreground
// process is alive and waiting for input.
func (t *tmuxClient) GetPaneState(ctx context.Context, paneID string) (*PaneState, error) {
	socket, bareID := parseTarget(paneID)
	// Query pid, dead flag, and dead exit status in a single tmux call.
	out, err := t.runWithSocket(ctx, socket, "display-message", "-p", "-t", bareID,
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
