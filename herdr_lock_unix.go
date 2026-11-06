//go:build unix

package main

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, waiting for it. flock locks
// belong to the open file, so two opens in one process exclude each other as
// well as two processes do.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
