//go:build !linux && !darwin

package main

import "context"

// fillPaneState is a no-op stub for platforms that are neither Linux nor macOS.
// WaitingForInput is always false (safe default).
func fillPaneState(_ context.Context, state *PaneState) error {
	state.IsAlive = true
	return nil
}
