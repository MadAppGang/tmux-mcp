package main

import (
	"errors"
	"os"
	"runtime"
	"testing"
)

// TestProcessUIDsSelf pins the contract every platform implementation of
// processUIDs has to honour, using the one process whose credentials the test
// already knows: itself.
//
// On a supported platform the answer must be os.Getuid() twice. Asserting both
// values, rather than just one, is what would catch an implementation that read
// the same field twice — real and effective are distinct fields in two very
// different structures (a kinfo_proc on darwin, a /proc status line on linux),
// and a copy-paste that collapses them would still pass a one-value check while
// silently making the caller's "real == effective" test vacuous.
//
// On an unsupported platform the assertion is the inverse and matters more: an
// error, and -1 for both uids. That is what makes pane acquisition fail closed
// there — see the comment on process_other.go's processUIDs. A stub that
// returned a plausible uid would turn the guard into a no-op that still reads
// like a guard, so this branch is the regression test for the guard existing at
// all.
func TestProcessUIDsSelf(t *testing.T) {
	real, effective, err := processUIDs(os.Getpid())

	switch runtime.GOOS {
	case "darwin", "linux":
		if err != nil {
			t.Fatalf("processUIDs(self) failed on %s: %v", runtime.GOOS, err)
		}
		me := os.Getuid()
		if real != me {
			t.Errorf("real uid = %d, want %d (this process's own uid)", real, me)
		}
		if effective != me {
			t.Errorf("effective uid = %d, want %d (this process's own uid)", effective, me)
		}
	default:
		if !errors.Is(err, errUIDUnsupported) {
			t.Fatalf("processUIDs on %s must report errUIDUnsupported so acquisition fails closed, got err=%v", runtime.GOOS, err)
		}
		if real != -1 || effective != -1 {
			t.Errorf("unsupported processUIDs returned (%d, %d); it must return (-1, -1) so an ignored error cannot match root", real, effective)
		}
	}
}
