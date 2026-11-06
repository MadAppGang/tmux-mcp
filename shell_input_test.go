package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- Shells the readiness probe cannot read ----

// useDefaultCommand makes every pane the private tmux server opens from now on
// run command instead of its default shell, and restores the default when the
// test ends. The agent pane already exists, so only the slot panes change.
func useDefaultCommand(t *testing.T, command string) {
	t.Helper()
	tmuxExec(t, "set-option", "-g", "default-command", command)
	t.Cleanup(func() { tmuxExec(t, "set-option", "-gu", "default-command") })
}

// TestCreatingToolWorksWithoutALineEditor is a shell that reads its prompt in
// the kernel's canonical mode, which the readiness probe can never see leave
// canonical mode, and a shell under a name the probe does not recognise. Both
// are at a prompt and reading; a creating tool has to type into them, not give
// up after its readiness budget.
//
// REGRESSION: the readiness gate required a line editor, inferred from the shell's name, so bash --noediting and unrecognised shells failed every creating tool after 10s
func TestCreatingToolWorksWithoutALineEditor(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	// The unrecognised shell is a few lines of perl: a prompt, a canonical read
	// of each line, and the line run by bash. The kernel reports it as "perl",
	// which is no shell the probe knows. (A renamed copy of bash would do on
	// Linux, but macOS kills a relocated system binary.)
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("perl is not installed")
	}
	script := filepath.Join(t.TempDir(), "xsh.pl")
	if err := os.WriteFile(script, []byte(`$|=1; print "xsh> ";`+
		` while (<STDIN>) { system("`+bash+`", "-c", $_); print "xsh> "; }`+"\n"), 0o644); err != nil {
		t.Fatalf("write the unrecognised shell: %v", err)
	}

	for _, tc := range []struct{ name, command string }{
		{"bash --noediting", "exec " + bash + " --noediting --norc --noprofile"},
		{"a shell the probe does not know", "exec " + perl + " " + script},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := agentPaneFixture(t, "--shell-type=bash")
			useDefaultCommand(t, tc.command)

			marker := filepath.Join(t.TempDir(), "M")
			started := time.Now()
			res := callTool(t, c, "send-keys", map[string]any{"keys": "echo M > " + marker, "enter": true})
			if res.IsError {
				t.Fatalf("send-keys into a new pane failed after %v: %s",
					time.Since(started).Round(time.Millisecond), res.text(t, "send-keys"))
			}
			if !waitForFile(marker, 5*time.Second) {
				t.Fatalf("the command send-keys typed into the new pane never ran")
			}

			// A second slot, so that execute-command is also the call that creates.
			res = callTool(t, c, "execute-command", map[string]any{
				"slot": 2, "command": "echo hello-from-slot-2", "timeoutSeconds": 10,
			})
			text := res.text(t, "execute-command")
			if res.IsError {
				t.Fatalf("execute-command into a new pane failed: %s", text)
			}
			if !strings.Contains(text, "hello-from-slot-2") || strings.Contains(text, `"timedOut": true`) {
				t.Fatalf("execute-command into a new pane did not run the command: %s", text)
			}
		})
	}
}

// lookBash finds a bash binary to run as a pane's shell.
func lookBash() (string, error) {
	for _, p := range []string{"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash", "/opt/homebrew/bin/bash"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no bash binary on this machine")
}

// ---- Keys for whatever owns the tty ----

// TestSendKeysInterruptsABuiltinLoop is the interrupt an agent sends to a shell
// that is busy inside its own process. A loop of builtins keeps the shell in
// the foreground with the tty in canonical mode, which is exactly the state a
// shell still sourcing its rc is in — but this pane is an existing slot, and
// C-c is meant for whatever owns the tty, not for a line editor.
//
// REGRESSION: send-keys waited for a ready prompt before every send, so C-c into a shell running a builtin loop was refused after 10s
func TestSendKeysInterruptsABuiltinLoop(t *testing.T) {
	c, _ := agentPaneFixture(t)
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "true", "enter": true}, &map[string]any{})

	c.callToolJSON(t, "send-keys", map[string]any{"keys": "while :; do :; done", "enter": true}, &map[string]any{})
	sleep(500 * time.Millisecond)

	started := time.Now()
	res := callTool(t, c, "send-keys", map[string]any{"keys": "C-c", "literal": false})
	took := time.Since(started)
	if res.IsError {
		t.Fatalf("C-c into a shell running a builtin loop was refused after %v: %s",
			took.Round(time.Millisecond), res.text(t, "send-keys"))
	}
	if took > 3*time.Second {
		t.Errorf("C-c took %v to send; it must not wait for a prompt", took.Round(time.Millisecond))
	}

	marker := filepath.Join(t.TempDir(), "back")
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo back > " + marker, "enter": true}, &map[string]any{})
	if !waitForFile(marker, 5*time.Second) {
		t.Fatal("the shell is not back at its prompt after C-c: the loop was not interrupted")
	}
}

// TestNamedEnterIsNotValidatedAsText is a named key sent with literal:false to
// submit a line typed earlier. The line runs a builtin that takes three seconds,
// so the shell stays in the foreground with its tty in canonical mode while the
// command executes. The call must not report that nothing was submitted.
//
// REGRESSION: a named Enter sent with literal:false was checked as typed text, and the tool said "nothing was submitted" for a command that was running
func TestNamedEnterIsNotValidatedAsText(t *testing.T) {
	c, self := agentPaneFixture(t)
	requireZshPanes(t)
	_ = self

	marker := filepath.Join(t.TempDir(), "done")
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys": "zmodload zsh/zselect; zselect -t 300; echo done > " + marker,
	}, &map[string]any{})

	res := callTool(t, c, "send-keys", map[string]any{"keys": "Enter", "literal": false})
	if res.IsError {
		t.Fatalf("a named Enter that submitted a running command was reported as a failure: %s",
			res.text(t, "send-keys"))
	}
	if !waitForFile(marker, 8*time.Second) {
		t.Fatal("the submitted command never finished")
	}
}

// TestMultiLinePayloadNeverClaimsNothingRan is a payload with a line break in
// it. The shell runs the first line as soon as the break arrives, and that line
// is a builtin that takes three seconds. Whatever the call reports, it must not
// say the input was not run: it was.
//
// REGRESSION: a payload whose first line ran a long builtin was reported as "nothing was submitted", inviting a second run
func TestMultiLinePayloadNeverClaimsNothingRan(t *testing.T) {
	c, _ := agentPaneFixture(t)
	requireZshPanes(t)

	marker := filepath.Join(t.TempDir(), "done")
	res := callTool(t, c, "send-keys", map[string]any{
		"keys": "zmodload zsh/zselect; zselect -t 300; echo done > " + marker + "\n",
	})
	if res.IsError {
		text := res.text(t, "send-keys")
		if strings.Contains(text, "not run") || strings.Contains(text, "nothing was submitted") {
			t.Fatalf("the call claimed a line that ran was not run: %s", text)
		}
	}
	if !waitForFile(marker, 8*time.Second) {
		t.Fatal("the line never finished")
	}
}

// swallowFirstEnterZshrc binds Enter to a widget that ignores the first press
// and accepts the line on every press after it, which is what a completion menu
// that takes Enter as "pick this entry" does to a line.
const swallowFirstEnterZshrc = "typeset -gi _swallowed=0\n" +
	"_first_enter() { if (( ! _swallowed )); then _swallowed=1; return 0; fi; zle .accept-line; }\n" +
	"zle -N _first_enter\n" +
	"bindkey '^M' _first_enter\n"

// TestExecuteCommandResendDoesNotRunTwice is the one re-send execute-command
// makes when its command never started. Here the first Enter was taken by a
// widget, so the wrapped line is still in the editor's buffer when the re-send
// comes. The command must run once.
//
// REGRESSION: the re-send typed the command onto a line still holding the first copy, and the combined line ran it twice
func TestExecuteCommandResendDoesNotRunTwice(t *testing.T) {
	installZshrc(t, swallowFirstEnterZshrc)
	c, _ := agentPaneFixture(t)

	out := filepath.Join(t.TempDir(), "runs")
	res := callTool(t, c, "execute-command", map[string]any{
		"command": "echo once >> " + out, "timeoutSeconds": 15,
	})
	if res.IsError {
		t.Fatalf("execute-command failed: %s", res.text(t, "execute-command"))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the command never ran: %v", err)
	}
	if runs := strings.Count(string(data), "once"); runs != 1 {
		t.Fatalf("the command ran %d times, want exactly once:\n%s", runs, data)
	}
}

// installZshrc writes rc as the suite's private .zshrc for the rest of the test.
func installZshrc(t *testing.T, rc string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux")
	}
	if shell := tmuxExec(t, "show-options", "-gv", "default-shell"); filepath.Base(shell) != "zsh" {
		t.Skipf("tmux spawns %q, not zsh, so a .zshrc is never sourced", shell)
	}
	zdotdir := os.Getenv("ZDOTDIR")
	if zdotdir == "" {
		t.Fatal("ZDOTDIR is not set; isolateTmux should have pointed it at a private directory")
	}
	path := filepath.Join(zdotdir, ".zshrc")
	if err := os.WriteFile(path, []byte(rc), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

// ---- A reclaimed pane starts where a new one would ----

// TestReclaimedPaneStartsInTheServersDirectory is a pane an earlier agent left
// behind after cd'ing elsewhere. Reclaimed, it is reported as created, so the
// next command has to run where a new pane's would: the server's directory.
//
// REGRESSION: a reclaimed pane was reported created:true but kept the directory a previous agent left it in
func TestReclaimedPaneStartsInTheServersDirectory(t *testing.T) {
	c, self := agentPaneFixture(t)
	requireZshPanes(t)

	left := rawSplit(t, self, "-h")
	waitForIdleZsh(t, left)
	tmuxExec(t, "send-keys", "-t", left, "cd /tmp", "Enter")
	markAgentUnslotted(t, left)
	sleep(500 * time.Millisecond)

	res := callTool(t, c, "execute-command", map[string]any{"command": "pwd", "timeoutSeconds": 10})
	text := res.text(t, "execute-command")
	if res.IsError {
		t.Fatalf("execute-command failed: %s", text)
	}
	if got := slotPaneID(t, self, 1); got != left {
		t.Fatalf("slot 1 is %s, not the left-behind pane %s: the fixture did not exercise reclaim", got, left)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(wd)
	var out struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(out.Output))
	if got != want {
		t.Fatalf("a reclaimed pane ran its first command in %q, want the server's directory %q", got, want)
	}
}

// ---- A new pane must carry a record ----

// unrecordedSplit is a real Backend whose new panes end up with no record at
// all: the mark the split writes does not stick, and neither does the claim.
type unrecordedSplit struct{ Backend }

func (u *unrecordedSplit) OpenBeside(ctx context.Context, place placement) (paneRef, error) {
	pane, err := u.Backend.OpenBeside(ctx, place)
	if err == nil {
		_ = u.Backend.ClearMarks(ctx, pane)
	}
	return pane, err
}

func (u *unrecordedSplit) Claim(context.Context, paneRef, string, int) error {
	return errors.New("injected: the registry write failed")
}

// TestNewPaneWithNoRecordIsClosed is a split whose pane carries no record of
// ours at all. Such a pane is indistinguishable from one the user opened: no
// reclaim takes it and no close reaches it. It must not be left on screen.
//
// REGRESSION: a new pane whose record write and claim both failed was kept, and became a pane no tool could reach
func TestNewPaneWithNoRecordIsClosed(t *testing.T) {
	sl, self := slotFixture(t)
	sl.b = &unrecordedSplit{Backend: sl.b}
	before := len(panesInWindow(t, self.target()))

	if _, err := sl.resolveHelper(context.Background(), slotDefault, kindUnstated); err == nil {
		t.Error("resolution succeeded with a pane that carries no record")
	}
	if after := len(panesInWindow(t, self.target())); after != before {
		t.Errorf("the window went from %d panes to %d: the unrecorded pane was left behind", before, after)
	}
}

// ---- The validated send, with a scripted pane ----

// scriptedPane is a Backend whose Foreground answers from a script and whose
// SendKeys records what it was asked to send. Nothing else may be called.
type scriptedPane struct {
	Backend
	mu     sync.Mutex
	states []PaneState
	polls  int
	sends  []string
}

func (s *scriptedPane) Foreground(context.Context, paneRef) (*PaneState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := min(s.polls, len(s.states)-1)
	s.polls++
	st := s.states[i]
	return &st, nil
}

func (s *scriptedPane) SendKeys(_ context.Context, _ paneRef, keys string, literal, enter bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := keys
	if !literal {
		rec = "<" + keys + ">"
	}
	if enter {
		rec += "+enter"
	}
	s.sends = append(s.sends, rec)
	return nil
}

func readyState() PaneState {
	return PaneState{PanePID: 7, ForegroundPID: 7, ForegroundCmd: "zsh", IsAlive: true,
		InputKnown: true, LineEditing: true}
}

func busyState() PaneState {
	s := readyState()
	s.LineEditing = false
	return s
}

// TestSendValidatedNeedsTwoReadySamples pins the two-sample rule on the send
// path. One ready sample can land in the instant an rc's `read -k` holds the
// tty in non-canonical mode, so a single sample is not evidence of a prompt.
func TestSendValidatedNeedsTwoReadySamples(t *testing.T) {
	if !ttyProbeSupported {
		t.Skip("no tty probe on this platform")
	}
	pane := &scriptedPane{states: []PaneState{readyState(), busyState()}}
	sl := newSlots(pane)
	if err := sl.sendValidated(context.Background(), newPaneRef("%1"), "ls", true, true); err != nil {
		t.Fatalf("sendValidated: %v", err)
	}
	if want := []string{"ls+enter"}; strings.Join(pane.sends, "|") != strings.Join(want, "|") {
		t.Errorf("sends = %q, want %q: one ready sample was taken as a prompt", pane.sends, want)
	}
}

// TestNamedKeysAreNeverValidated pins that a key sent with literal:false goes
// straight to the pane, whatever state the shell is in.
func TestNamedKeysAreNeverValidated(t *testing.T) {
	if !ttyProbeSupported {
		t.Skip("no tty probe on this platform")
	}
	for _, st := range []PaneState{readyState(), busyState()} {
		pane := &scriptedPane{states: []PaneState{st}}
		sl := newSlots(pane)
		started := time.Now()
		if err := sl.sendValidated(context.Background(), newPaneRef("%1"), "C-c", false, false); err != nil {
			t.Fatalf("sendValidated(C-c): %v", err)
		}
		if took := time.Since(started); took > time.Second {
			t.Errorf("C-c took %v: a named key was waited on", took)
		}
		if strings.Join(pane.sends, "|") != "<C-c>" {
			t.Errorf("sends = %q, want just the key", pane.sends)
		}
	}
}
