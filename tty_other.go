//go:build !linux && !darwin

package main

import "errors"

// ttyProbeSupported is false here: this platform has no implementation of the
// tty probe. Readiness then falls back to a bounded wait for the screen to
// settle, a send is not validated, and reclaim never takes a pane, because
// reclaim needs a positive answer from the probe.
const ttyProbeSupported = false

var errTTYUnsupported = errors.New("tty inspection is unsupported on this platform")

func ttyInputState(string) (bool, int, error) { return false, 0, errTTYUnsupported }

func ttyOfPID(int) (string, error) { return "", errTTYUnsupported }

func parentPID(int) (int, error) { return 0, errTTYUnsupported }

func readingTTY(int) bool { return false }
