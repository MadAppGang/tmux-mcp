package main

import (
	"errors"
	"path/filepath"
	"strings"
)

// errUIDUnsupported is what processUIDs returns on a platform where we cannot
// resolve a process's owner. See process_other.go for why that is an error and
// not a value — it is the difference between a guard and the appearance of one.
var errUIDUnsupported = errors.New("process uid lookup is unsupported on this platform")

// PaneState captures the OS-level state of a pane's process.
//
// The last three fields describe the pane's tty rather than its process, and
// they are kept off the wire: pane-state's response shape does not change, and
// they exist for the readiness gate, not for the agent. See ttyInputState.
type PaneState struct {
	PanePID         int    `json:"panePid"`
	ForegroundPID   int    `json:"foregroundPid"`
	ForegroundCmd   string `json:"foregroundCmd"`
	IsAlive         bool   `json:"isAlive"`
	WaitingForInput bool   `json:"waitingForInput"`
	ExitCode        int    `json:"exitCode,omitempty"`

	// InputKnown is true when the tty probe answered, so the two fields after
	// it are facts rather than zero values. Every gate that relies on them
	// checks it first, and a probe that failed reads as "not ready".
	InputKnown bool `json:"-"`
	// LineEditing is true when the tty is in non-canonical mode: a line editor
	// is reading it key by key.
	LineEditing bool `json:"-"`
	// PendingInput is how many typed bytes are queued on the tty unread.
	PendingInput int `json:"-"`
	// ReadingTTY is true when the pane's own process is in the foreground and
	// asleep in a read on its terminal: a prompt read in canonical mode, by a
	// shell with no line editor or with its editor switched off.
	ReadingTTY bool `json:"-"`
	// EditorOff is true when the pane's own shell has a line editor but its
	// command line started it without one (see editorSwitchedOff), so a prompt
	// read in canonical mode is what it does at rest.
	EditorOff bool `json:"-"`
}

// fillInputState probes the pane's tty and records what it found. Each backend
// supplies the path its own way; the probe and its meaning are the same for
// all of them. A failed probe leaves InputKnown false.
func fillInputState(state *PaneState, tty string) {
	if state == nil || tty == "" {
		return
	}
	lineEditing, pending, err := ttyInputState(tty)
	if err != nil {
		return
	}
	state.InputKnown = true
	state.LineEditing = lineEditing
	state.PendingInput = pending
	if state.PanePID > 0 && state.ForegroundPID == state.PanePID {
		state.ReadingTTY = readingTTY(state.PanePID)
		if hasLineEditor(state.ForegroundCmd) {
			if argv, err := processArgs(state.PanePID); err == nil {
				state.EditorOff = editorSwitchedOff(argv)
			}
		}
	}
}

// editorSwitchedOff reports whether a shell's command line starts it without
// its line editor: bash --noediting, or zsh with its ZLE option unset (+Z,
// --nozle, -o nozle, +o zle). Such a shell reads its prompt in canonical mode
// although its name says it has an editor. Only the options before the first
// operand count; anything after it belongs to a script or a -c string.
func editorSwitchedOff(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	shell := strings.TrimPrefix(filepath.Base(argv[0]), "-")
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" || (arg[0] != '-' && arg[0] != '+') {
			return false
		}
		switch shell {
		case "bash":
			switch arg {
			case "--noediting":
				return true
			case "-c":
				return false
			case "-o", "+o", "-O", "+O", "--rcfile", "--init-file":
				i++
			}
		case "zsh":
			switch {
			case strings.HasPrefix(arg, "--"):
				if zshOptionName(arg[2:]) == "nozle" {
					return true
				}
			case arg == "-o" || arg == "+o":
				if i+1 < len(args) {
					i++
					name := zshOptionName(args[i])
					if (arg == "-o" && name == "nozle") || (arg == "+o" && name == "zle") {
						return true
					}
				}
			case arg[0] == '+' && strings.ContainsRune(arg[1:], 'Z'):
				return true
			case strings.ContainsRune(arg[1:], 'c'):
				return false
			}
		}
	}
	return false
}

// zshOptionName is an option name the way zsh compares them: case and
// underscores do not count, and neither do dashes in a long option.
func zshOptionName(s string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(s))
}

// procInfo is the minimum a platform must report about a process for
// foregroundOfGroup to choose between them.
type procInfo struct {
	PID  int
	Comm string
}

// foregroundOfGroup picks the process that represents a terminal's foreground
// process group: its leader, the process whose PID equals the group ID.
//
// The obvious alternative — take the highest PID in the group, i.e. the most
// recently spawned process — is wrong, and wrong in a way that only shows up on
// a developer's machine. A shell with a rich prompt forks helpers from its
// precmd hooks (powerlevel10k runs git for the VCS segment, version probes for
// the language segments) and those children run *inside the shell's own process
// group*, not a job of their own. So the newest PID in the group is almost
// always a prompt helper, and a pane sitting idle at its prompt reports
// foregroundCmd "git" or "go". Every caller that asks "is this pane free?" is
// then told no, forever. A bare CI shell forks nothing, which is exactly why
// this passes in CI and fails locally.
//
// The leader is the stable answer: for an idle shell the tty's foreground group
// is the shell's own group, whose leader is the shell. For a real job the shell
// puts it in a new process group, whose leader is the job.
//
// members may be in any order. If the leader has already exited but the group
// still has members — a pipeline whose head finished first — fall back to the
// most recent survivor, which is the best guess available.
func foregroundOfGroup(members []procInfo, pgid int) (pid int, comm string) {
	for _, m := range members {
		if m.PID == pgid {
			return m.PID, m.Comm
		}
	}
	for _, m := range members {
		if m.PID > pid {
			pid, comm = m.PID, m.Comm
		}
	}
	return pid, comm
}
