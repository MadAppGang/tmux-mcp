//go:build !linux && !darwin

package main

import "context"

// fillPaneState is a no-op stub for platforms that are neither Linux nor macOS.
// WaitingForInput is always false (safe default).
func fillPaneState(_ context.Context, state *PaneState) error {
	state.IsAlive = true
	return nil
}

// processUIDs is unimplemented off Linux and macOS, and returns an error rather
// than a uid. This is the difference between a guard and the appearance of one,
// and it is the single most important line in this file.
//
// The uid check exists to stop the server adopting a pane whose shell belongs to
// another user — a root shell the user left open in a corner of their window.
// A stub that returned a plausible value would defeat that check in the worst
// possible way, because it would still *look* like a working guard in every
// review: returning 0 would match a server running as root, and returning
// os.Getuid() would match EVERYTHING, silently turning the comparison into a
// tautology while the code around it reads as if it were deciding something.
//
// Returning an error makes acquisition fail closed here instead. canAcquire
// treats any error as "no", resolution falls through to creating a fresh pane,
// and the feature degrades to "always create" — correct everywhere, and merely
// less thrifty on platforms nobody runs tmux on. Note that this needs no
// runtime.GOOS check anywhere in the acquisition path: the only way to acquire a
// pane is to get a nil error back from a real implementation, so the platform
// restriction is structural rather than remembered.
//
// -1 rather than 0 as the failure value is also deliberate. 0 is root, a real
// uid: a caller that ignored the error and compared the value would, on a
// root-owned process, conclude "same user" — the exact adoption this guard
// exists to refuse. -1 is not any user, so an ignored error can only ever
// compare unequal.
func processUIDs(_ int) (real, effective int, err error) {
	return -1, -1, errUIDUnsupported
}
