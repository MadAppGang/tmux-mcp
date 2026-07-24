//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/procfs"
)

// fillPaneState populates state using Linux /proc filesystem information.
func fillPaneState(_ context.Context, state *PaneState) error {
	panePID := state.PanePID

	// Open the shell process via procfs.
	proc, err := procfs.NewProc(panePID)
	if err != nil {
		return fmt.Errorf("open proc %d: %w", panePID, err)
	}

	stat, err := proc.Stat()
	if err != nil {
		return fmt.Errorf("read proc stat %d: %w", panePID, err)
	}

	// Process is alive if it exists and is not a zombie.
	state.IsAlive = stat.State != "Z" && stat.State != "X"
	tpgid := stat.TPGID

	// Find all processes in the foreground process group.
	allProcs, err := procfs.AllProcs()
	if err != nil {
		// Fall back: just examine the shell process itself.
		state.ForegroundPID = panePID
		state.ForegroundCmd = stat.Comm
		state.WaitingForInput = isWaitingForInputLinux(panePID)
		return nil
	}

	// Collect the foreground process group, then let foregroundOfGroup pick which
	// of its members represents the pane. See that function: taking the highest
	// PID here reports a prompt hook's child (p10k's git, a version probe) as the
	// foreground command of a pane that is sitting idle at its prompt.
	var members []procInfo
	var waitingForInput bool

	for _, p := range allProcs {
		pStat, err := p.Stat()
		if err != nil {
			continue
		}
		if pStat.PGRP != tpgid {
			continue
		}
		members = append(members, procInfo{PID: p.PID, Comm: pStat.Comm})
		// Check if any process in the fg group is waiting for input.
		if isWaitingForInputLinux(p.PID) {
			waitingForInput = true
		}
	}

	fgPID, fgCmd := foregroundOfGroup(members, tpgid)

	if fgPID == 0 {
		// No foreground group found — fall back to pane PID.
		fgPID = panePID
		fgCmd = stat.Comm
		waitingForInput = isWaitingForInputLinux(panePID)
	}

	state.ForegroundPID = fgPID
	state.ForegroundCmd = fgCmd
	state.WaitingForInput = waitingForInput
	return nil
}

// isWaitingForInputLinux checks /proc/<pid>/wchan and /proc/<pid>/syscall to
// determine if the process is blocked reading from stdin.
func isWaitingForInputLinux(pid int) bool {
	// Check wchan — the kernel function the process is sleeping in.
	wchanPath := filepath.Join("/proc", strconv.Itoa(pid), "wchan")
	wchanBytes, err := os.ReadFile(wchanPath)
	if err == nil {
		wchan := strings.TrimSpace(string(wchanBytes))
		if wchan == "n_tty_read" || wchan == "wait_woken" || strings.Contains(wchan, "tty_read") {
			return true
		}
	}

	// Check syscall — field 0 is syscall number, field 1 is first argument (fd).
	// SYS_READ (0) with fd=0 means reading from stdin.
	syscallPath := filepath.Join("/proc", strconv.Itoa(pid), "syscall")
	syscallBytes, err := os.ReadFile(syscallPath)
	if err == nil {
		fields := strings.Fields(strings.TrimSpace(string(syscallBytes)))
		if len(fields) >= 2 && fields[0] == "0" && fields[1] == "0x0" {
			return true
		}
	}

	return false
}
