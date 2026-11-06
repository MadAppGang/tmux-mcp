//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
)

// ttyProbeSupported says this platform can read a tty's line discipline and
// input queue, which is what the readiness gate is built on.
const ttyProbeSupported = true

// ttyInputState reports whether the program reading a tty has switched its line
// editor on, and how many bytes are queued on it that nobody has read yet. See
// the darwin implementation for why these two facts are the readiness signal.
func ttyInputState(path string) (lineEditing bool, pending int, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, 0, fmt.Errorf("open tty: %w", err)
	}
	defer unix.Close(fd)
	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return false, 0, fmt.Errorf("read termios: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCINQ)
	if err != nil {
		return false, 0, fmt.Errorf("read input queue: %w", err)
	}
	return tio.Lflag&unix.ICANON == 0, n, nil
}

// ttyOfPID returns the path of a process's controlling terminal, from the
// tty_nr field of /proc/<pid>/stat. Pseudo-terminal slaves (majors 136 to 143)
// are /dev/pts/N; the name is confirmed by its device number, and anything else
// falls back to a scan of /dev/pts.
func ttyOfPID(pid int) (string, error) {
	proc, err := procfs.NewProc(pid)
	if err != nil {
		return "", fmt.Errorf("open proc %d: %w", pid, err)
	}
	stat, err := proc.Stat()
	if err != nil {
		return "", fmt.Errorf("read proc stat %d: %w", pid, err)
	}
	dev := uint64(stat.TTY)
	if dev == 0 {
		return "", fmt.Errorf("process %d has no controlling terminal", pid)
	}
	major := (dev >> 8) & 0xfff
	minor := (dev & 0xff) | ((dev >> 12) & 0xfff00)
	if major >= 136 && major <= 143 {
		guess := fmt.Sprintf("/dev/pts/%d", minor+(major-136)*256)
		if rdevOf(guess) == unix.Mkdev(uint32(major), uint32(minor)) {
			return guess, nil
		}
	}
	matches, _ := filepath.Glob("/dev/pts/*")
	for _, m := range matches {
		if rdevOf(m) == unix.Mkdev(uint32(major), uint32(minor)) {
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
	return uint64(st.Rdev)
}

// readingTTY reports whether a process is asleep in a read on its terminal:
// the kernel's tty read routine, or a read(2) on stdin. A process waiting in
// select, in a timer or on a child is somewhere else.
func readingTTY(pid int) bool { return isWaitingForInputLinux(pid) }

// parentPID returns the parent of a process.
func parentPID(pid int) (int, error) {
	proc, err := procfs.NewProc(pid)
	if err != nil {
		return 0, fmt.Errorf("open proc %d: %w", pid, err)
	}
	stat, err := proc.Stat()
	if err != nil {
		return 0, fmt.Errorf("read proc stat %d: %w", pid, err)
	}
	return stat.PPID, nil
}

// processArgs returns a process's command line from /proc/PID/cmdline, where
// each argument is NUL-terminated.
func processArgs(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"), nil
}
