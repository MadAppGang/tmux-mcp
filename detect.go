package main

import (
	"fmt"
	"os"
)

// detectedBackend is which multiplexer this server talks to, decided once at
// startup. The tool surface is the same under every answer: nothing an agent
// sends or reads says which one it is.
type detectedBackend int

const (
	detectedTmuxNoWindow detectedBackend = iota // no multiplexer pane: tmux, isolated slots only
	detectedTmux
	detectedHerdr
)

func (d detectedBackend) String() string {
	switch d {
	case detectedTmux:
		return "tmux"
	case detectedHerdr:
		return "herdr"
	default:
		return "tmux (no window)"
	}
}

// The values -backend accepts. auto, the default, is detectBackend's rule; the
// other two are an explicit choice that skips it.
const (
	backendAuto  = "auto"
	backendTmux  = "tmux"
	backendHerdr = "herdr"
)

// Ancestry probe kinds, as detectBackend asks about them.
const (
	probeTmux  = "tmux"
	probeHerdr = "herdr"
)

// detectBackend picks the multiplexer from the -backend flag value, the
// process environment, and an ancestry probe.
//
// Each multiplexer announces itself through variables it injects into its
// panes: tmux sets TMUX_PANE, herdr sets HERDR_ENV=1 and HERDR_PANE_ID. Those
// are CLAIMS, and on their own they lie in exactly the case that matters. A
// herdr started from a tmux pane hands its own panes the TMUX_PANE it
// inherited, and a tmux started from a herdr pane does the same with HERDR_*.
// So a process can carry both claims while only one multiplexer is the one
// whose window the user is looking at.
//
// The rule:
//
//   - An explicit -backend tmux or -backend herdr wins outright.
//   - With one claim present, that claim is the answer unless the probe
//     disproves it: the claimed pane's shell was found, and it is not an
//     ancestor of this process. That is a variable inherited by something
//     started outside the pane — a GUI app launched from a tmux pane — and it
//     ends in tmux with no window. A probe that cannot run leaves the claim
//     standing: there is no server to ask (TMUX or HERDR_SOCKET_PATH is not
//     set), the multiplexer did not answer, or this platform cannot walk the
//     process tree.
//   - With both present, each is verified: its pane's shell must be an ancestor
//     of this process. The probe answers how many parent hops away that shell
//     is, and the NEAREST verified one wins — the innermost multiplexer is the
//     one whose window holds this process. A claim that does not verify is a
//     leak.
//   - With neither verified, or no claim at all, the answer is tmux with no
//     window: visible slots are refused with the usual message, and isolated
//     slots, which need no window, keep working.
//
// The probe is asked only about a kind whose claim is present, so a
// multiplexer that made no claim can never be chosen by being found up the
// process tree.
func detectBackend(
	env map[string]string,
	ancestorDepth func(kind string) (depth int, ok bool),
	flag string,
) detectedBackend {
	switch flag {
	case backendTmux:
		return detectedTmux
	case backendHerdr:
		return detectedHerdr
	}

	tmuxClaim := env["TMUX_PANE"] != ""
	herdrClaim := env["HERDR_ENV"] == "1" && env["HERDR_PANE_ID"] != ""

	switch {
	case tmuxClaim && herdrClaim:
		tmuxDepth, tmuxOK := ancestorDepth(probeTmux)
		herdrDepth, herdrOK := ancestorDepth(probeHerdr)
		switch {
		case tmuxOK && herdrOK && herdrDepth < tmuxDepth:
			return detectedHerdr
		case tmuxOK:
			return detectedTmux
		case herdrOK:
			return detectedHerdr
		default:
			return detectedTmuxNoWindow
		}
	case tmuxClaim:
		return loneClaim(detectedTmux, probeTmux, ancestorDepth)
	case herdrClaim:
		return loneClaim(detectedHerdr, probeHerdr, ancestorDepth)
	default:
		return detectedTmuxNoWindow
	}
}

// loneClaim settles a single claim. It stands unless the probe disproves it:
// the claimed pane's shell was found and is not an ancestor of this process.
// A probe that could not run (depth probeUnavailable) leaves the claim
// standing, because there is nothing to weigh against it.
func loneClaim(
	claimed detectedBackend, kind string, ancestorDepth func(kind string) (int, bool),
) detectedBackend {
	if depth, ok := ancestorDepth(kind); ok || depth == probeUnavailable {
		return claimed
	}
	return detectedTmuxNoWindow
}

// validBackendFlag reports whether -backend was given a value this build
// implements.
func validBackendFlag(flag string) bool {
	switch flag {
	case "", backendAuto, backendTmux, backendHerdr:
		return true
	}
	return false
}

// detectionEnv reads the multiplexer claims out of this process's environment.
//
// It reads TMUX_PANE and HERDR_PANE_ID, which is otherwise the backends' Self
// alone, and it is permitted to because what it does with them is the opposite
// of addressing a pane: it decides whether to believe them. No pane is typed
// into, split or excluded on the strength of this map; the backend chosen from
// it reads its own variable again through Self.
func detectionEnv() map[string]string {
	return map[string]string{
		"TMUX":              os.Getenv("TMUX"),
		"TMUX_PANE":         os.Getenv("TMUX_PANE"),
		"HERDR_ENV":         os.Getenv("HERDR_ENV"),
		"HERDR_PANE_ID":     os.Getenv("HERDR_PANE_ID"),
		"HERDR_SOCKET_PATH": os.Getenv("HERDR_SOCKET_PATH"),
	}
}

// detectFromProcess applies detectBackend to this process: its environment, and
// an ancestry probe that asks each multiplexer for the pid of the claimed
// pane's shell and walks this process's parents looking for it.
func detectFromProcess(flag string) detectedBackend {
	env := detectionEnv()
	probe := func(kind string) (int, bool) {
		var shell int
		var err error
		switch kind {
		case probeTmux:
			if env["TMUX"] == "" {
				return probeUnavailable, false
			}
			shell, err = tmuxPaneShellPID(env["TMUX"], env["TMUX_PANE"])
		case probeHerdr:
			if env["HERDR_SOCKET_PATH"] == "" {
				return probeUnavailable, false
			}
			shell, err = herdrPaneShellPID(env["HERDR_PANE_ID"])
		default:
			return probeUnavailable, false
		}
		if err != nil {
			return probeUnavailable, false
		}
		return ancestorDepth(os.Getpid(), shell)
	}
	return detectBackend(env, probe, flag)
}

// maxAncestry bounds the parent walk. Real process trees from a pane's shell to
// an MCP server are a handful of hops deep; the bound only matters if the
// parent table answers with a cycle.
const maxAncestry = 128

// probeUnavailable is the depth an ancestry probe answers, with ok=false, when
// it could not run at all. It is not "not an ancestor": nothing was checked.
const probeUnavailable = -1

// ancestorDepth reports how many parent hops separate start from ancestor, or
// ok=false when ancestor is not one of start's ancestors. A walk the platform
// cannot make answers probeUnavailable.
func ancestorDepth(start, ancestor int) (depth int, ok bool) {
	if ancestor <= 1 {
		return 0, false
	}
	pid := start
	for hops := 0; hops < maxAncestry; hops++ {
		if pid == ancestor {
			return hops, true
		}
		parent, err := parentPID(pid)
		if err != nil {
			return probeUnavailable, false
		}
		if parent <= 1 || parent == pid {
			return 0, false
		}
		pid = parent
	}
	return 0, false
}

// errUnknownBackend is the startup failure for a -backend value this build does
// not implement.
func errUnknownBackend(flag string) error {
	return fmt.Errorf("unknown backend %q: use %s, %s or %s", flag, backendAuto, backendTmux, backendHerdr)
}
