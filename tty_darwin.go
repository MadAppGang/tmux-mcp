//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ttyProbeSupported says this platform can read a tty's line discipline and
// input queue, which is what the readiness gate is built on.
const ttyProbeSupported = true

// fionread is FIONREAD, _IOR('f', 127, int). golang.org/x/sys does not export
// it for darwin, and the value is fixed by the BSD ioctl encoding.
const fionread = 0x4004667f

// ttyInputState reports whether the program reading a tty has switched its line
// editor on, and how many bytes are queued on it that nobody has read yet.
//
// A shell's line editor (zle, readline, fish's reader) puts the terminal into
// non-canonical mode while it waits at the prompt, and the kernel keeps the
// terminal canonical before that: all the time a shell spends sourcing its rc
// files, it is in the foreground with its own pid and still not reading. ICANON
// is the one signal that tells the two apart and is the same on every
// multiplexer, because it lives on the tty and not in the multiplexer.
//
// FIONREAD on the slave side counts typed bytes the reader has not consumed.
// Bytes that sit there are bytes an rc file can still throw away.
//
// The tty is opened read-only, without becoming its controlling terminal and
// without blocking, and closed again before returning. Nothing is read from it.
func ttyInputState(path string) (lineEditing bool, pending int, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, 0, fmt.Errorf("open tty: %w", err)
	}
	defer unix.Close(fd)
	tio, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return false, 0, fmt.Errorf("read termios: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, fionread)
	if err != nil {
		return false, 0, fmt.Errorf("read input queue: %w", err)
	}
	return tio.Lflag&unix.ICANON == 0, n, nil
}

// ttyOfPID returns the path of a process's controlling terminal.
//
// The kernel reports it as a device number. Pseudo-terminal slaves are
// /dev/ttysNNN with the minor number as NNN, so that name is tried first and
// confirmed by its device number; anything else falls back to a scan of /dev.
func ttyOfPID(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	dev := uint64(uint32(kp.Eproc.Tdev))
	if kp.Eproc.Tdev == -1 {
		return "", fmt.Errorf("process %d has no controlling terminal", pid)
	}
	guess := fmt.Sprintf("/dev/ttys%03d", dev&0xffffff)
	if rdevOf(guess) == dev {
		return guess, nil
	}
	matches, _ := filepath.Glob("/dev/tty*")
	for _, m := range matches {
		if strings.HasPrefix(filepath.Base(m), "tty") && rdevOf(m) == dev {
			return m, nil
		}
	}
	return "", fmt.Errorf("no device node for the terminal of process %d", pid)
}

// rdevOf is the device number a device node stands for, or MaxUint64 when the
// path is not one.
func rdevOf(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return ^uint64(0)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ^uint64(0)
	}
	return uint64(uint32(st.Rdev))
}

// readingTTY reports whether a process is asleep in a read on its terminal.
// The kernel names that sleep "ttyin"; a process waiting in select, in a
// timer or on a child names it something else.
func readingTTY(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && wmesgIsInput(kp)
}

// parentPID returns the parent of a process.
func parentPID(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("sysctl kern.proc.pid %d: %w", pid, err)
	}
	return int(kp.Eproc.Ppid), nil
}
