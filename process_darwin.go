//go:build darwin

package main

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// SSLEEP is the macOS process state for interruptible sleep (waiting).
const pStatSleep = 2

// fillPaneState populates state using macOS sysctl kernel proc information.
func fillPaneState(_ context.Context, state *PaneState) error {
	panePID := state.PanePID

	// Fetch the kinfo_proc for the pane's shell PID.
	shellKP, err := unix.SysctlKinfoProc("kern.proc.pid", panePID)
	if err != nil {
		return fmt.Errorf("sysctl kern.proc.pid %d: %w", panePID, err)
	}

	// Determine alive status: process state != SZOMB (4) and != SDEAD (8).
	state.IsAlive = shellKP.Proc.P_stat != 4 && shellKP.Proc.P_stat != 8

	// Tpgid is the terminal foreground process group ID.
	tpgid := int(shellKP.Eproc.Tpgid)
	// Pgrp is the shell's own process group.
	shellPGRP := int(shellKP.Eproc.Pgid)

	// Fetch all processes in the foreground process group.
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", tpgid)
	if err != nil {
		// Fall back to the shell process itself.
		state.ForegroundPID = panePID
		state.ForegroundCmd = commFromKP(shellKP)
		// Heuristic: shell is waiting for input when tpgid == shell's own pgrp
		// and the shell is in interruptible sleep.
		state.WaitingForInput = tpgid == shellPGRP && shellKP.Proc.P_stat == pStatSleep
		return nil
	}

	var fgPID int
	var fgCmd string

	for i := range procs {
		kp := &procs[i]
		pid := int(kp.Proc.P_pid)
		if pid > fgPID {
			fgPID = pid
			fgCmd = commFromKP(kp)
		}
	}

	if fgPID == 0 {
		fgPID = panePID
		fgCmd = commFromKP(shellKP)
	}

	state.ForegroundPID = fgPID
	state.ForegroundCmd = fgCmd

	// Determine if waiting for input using two complementary signals:
	//
	// 1. Wmesg == "ttyin": kernel sets this when a process is blocked in
	//    n_tty_read (waiting for terminal input). Works for some processes.
	//
	// 2. Structural heuristic: tpgid == shell's own pgrp means no child
	//    process has seized the terminal (the shell itself is foreground),
	//    and the shell is in interruptible sleep (not running or zombie).
	//    This catches readline/select loops which don't set Wmesg.
	for i := range procs {
		if wmesgIsInput(&procs[i]) {
			state.WaitingForInput = true
			return nil
		}
	}
	// Structural heuristic.
	state.WaitingForInput = tpgid == shellPGRP && shellKP.Proc.P_stat == pStatSleep
	return nil
}

// commFromKP extracts the command name from a kinfo_proc.
func commFromKP(kp *unix.KinfoProc) string {
	// P_comm is [MAXCOMLEN+1]int8 — convert to string.
	b := kp.Proc.P_comm[:]
	var out []byte
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return strings.TrimSpace(string(out))
}

// wmesgIsInput returns true when the kernel wait message indicates the process
// is blocked reading from a tty (the "ttyin" wait channel on macOS).
func wmesgIsInput(kp *unix.KinfoProc) bool {
	b := kp.Eproc.Wmesg[:]
	var out []byte
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	wmesg := strings.TrimSpace(string(out))
	return wmesg == "ttyin" || strings.HasPrefix(wmesg, "ttyin")
}
