package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---- A quiet screen is not a prompt ----

// bannerThenQuietZshrc prints a banner, waits five seconds in a builtin, and
// then throws away anything typed in the meantime. The banner gives the screen
// something to settle on; the wait keeps zsh asleep, canonical and in the
// foreground for far longer than any settle period.
const bannerThenQuietZshrc = "echo 'welcome to this machine'\n" +
	"zmodload zsh/zselect; zselect -t 500\n" +
	"while read -t 0 -k 1 -s _x; do :; done\n"

// TestBannerThenQuietRcIsNotAPrompt is a zsh whose rc draws a banner and then
// waits. Its screen stops changing long before its line editor starts, and the
// command typed into it must still run.
//
// REGRESSION: a settled screen counted as a prompt for a shell with a line editor, so send-keys typed into an rc that was still waiting and its command was discarded
func TestBannerThenQuietRcIsNotAPrompt(t *testing.T) {
	c, _ := agentPaneFixture(t)
	installZshrc(t, bannerThenQuietZshrc)

	marker := filepath.Join(t.TempDir(), "M")
	res := callTool(t, c, "send-keys", map[string]any{"keys": "echo M > " + marker, "enter": true})
	if res.IsError {
		t.Fatalf("send-keys into a new pane failed: %s", res.text(t, "send-keys"))
	}
	if !waitForFile(marker, 10*time.Second) {
		t.Fatal("send-keys reported success, but the command it typed never ran")
	}
}

// TestEditorSwitchedOffByArgv pins which command lines say a shell reads its
// prompt without its line editor.
func TestEditorSwitchedOffByArgv(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{[]string{"bash", "--noediting"}, true},
		{[]string{"/bin/bash", "--noediting", "--norc", "--noprofile"}, true},
		{[]string{"-bash"}, false},
		{[]string{"bash", "-c", "x --noediting"}, false},
		{[]string{"zsh", "+Z"}, true},
		{[]string{"zsh", "-f", "+Z"}, true},
		{[]string{"zsh", "+fZ"}, true},
		{[]string{"zsh", "--nozle"}, true},
		{[]string{"zsh", "--no-zle"}, true},
		{[]string{"zsh", "-o", "nozle"}, true},
		{[]string{"zsh", "-o", "NO_ZLE"}, true},
		{[]string{"zsh", "+o", "zle"}, true},
		{[]string{"zsh", "-o", "zle"}, false},
		{[]string{"-zsh"}, false},
		{[]string{"zsh", "-Z"}, false},
		{[]string{"zsh", "script.zsh", "+Z"}, false},
		{[]string{"fish"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := editorSwitchedOff(tc.argv); got != tc.want {
			t.Errorf("editorSwitchedOff(%q) = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

// TestProcessArgsReadsOwnCommandLine checks the platform's argv reader against
// the one process whose command line is known: this test binary.
func TestProcessArgsReadsOwnCommandLine(t *testing.T) {
	if !ttyProbeSupported {
		t.Skip("no process probe on this platform")
	}
	got, err := processArgs(os.Getpid())
	if err != nil {
		t.Fatalf("processArgs: %v", err)
	}
	if strings.Join(got, "\x00") != strings.Join(os.Args, "\x00") {
		t.Fatalf("processArgs = %q, want %q", got, os.Args)
	}
}

// ---- A pane that never got ready is not kept ----

// slowStartZshrc keeps zsh in its rc for thirteen seconds and then discards
// typeahead: longer than the old readiness budget, well inside the new one.
const slowStartZshrc = "zmodload zsh/zselect; zselect -t 1300\n" +
	"while read -t 0 -k 1 -s _x; do :; done\n"

// TestSlowRcNeverLosesARetriedCommand is a shell whose rc outlasts a
// readiness budget. Whatever the first call reports, a call that succeeds has
// to have run its command — including the retry an agent makes after a
// failure.
//
// REGRESSION: a pane that failed its readiness wait stayed in the slot, and the retry reused it unchecked: it reported success and the rc discarded the command
func TestSlowRcNeverLosesARetriedCommand(t *testing.T) {
	c, _ := agentPaneFixture(t)
	installZshrc(t, slowStartZshrc)

	dir := t.TempDir()
	for attempt := 1; attempt <= 2; attempt++ {
		marker := filepath.Join(dir, "attempt-"+string(rune('0'+attempt)))
		res := callTool(t, c, "send-keys", map[string]any{"keys": "echo ran > " + marker, "enter": true})
		if res.IsError {
			t.Logf("attempt %d failed: %s", attempt, res.text(t, "send-keys"))
			continue
		}
		if !waitForFile(marker, 20*time.Second) {
			t.Fatalf("attempt %d reported success, but its command never ran", attempt)
		}
		return
	}
	t.Fatal("both attempts failed: a thirteen-second rc is inside the readiness budget")
}

// TestNewPaneThatNeverGetsReadyIsClosed is a new pane whose shell is still in
// its rc when the caller's context ends. The pane is not handed out, so it
// must not be left behind holding the slot either.
//
// REGRESSION: a new pane that failed its readiness wait kept its slot
func TestNewPaneThatNeverGetsReadyIsClosed(t *testing.T) {
	sl, self := slotFixture(t)
	installZshrc(t, slowStartZshrc)
	before := len(panesInWindow(t, self.target()))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := sl.resolveHelper(ctx, slotDefault, kindUnstated); err == nil {
		t.Fatal("resolution succeeded with a pane whose shell was still in its rc")
	}
	if after := len(panesInWindow(t, self.target())); after != before {
		t.Errorf("the window went from %d panes to %d: the pane that never got ready was left behind",
			before, after)
	}
}

// keysRefused is a real Backend whose SendKeys always fails.
type keysRefused struct{ Backend }

func (k *keysRefused) SendKeys(context.Context, paneRef, string, bool, bool) error {
	return errors.New("injected: the keys were refused")
}

// TestReclaimedPaneThatFailsIsUnslotted is a reclaimed pane whose reset could
// not be typed. The call fails, so the pane must go back to being unslotted
// rather than keep a slot the caller was never given.
//
// REGRESSION: a reclaimed pane whose reset failed kept the slot, and the next call reused it unchecked
func TestReclaimedPaneThatFailsIsUnslotted(t *testing.T) {
	sl, self := slotFixture(t)
	requireZshPanes(t)
	left := rawSplit(t, self.target(), "-h")
	waitForIdleZsh(t, left)
	markAgentUnslotted(t, left)
	sl.b = &keysRefused{Backend: sl.b}

	if _, err := sl.resolveHelper(context.Background(), slotDefault, kindUnstated); err == nil {
		t.Fatal("resolution succeeded although the reclaimed pane's reset was refused")
	}
	if got := mcpMarks(t, left); got[2] != "" {
		t.Errorf("the reclaimed pane kept slot %q after its reset failed", got[2])
	}
}

// ---- The re-send never runs a command twice ----

// swallowFirstEnterHomeZshrc is swallowFirstEnterZshrc with one more thing
// the widget does on the press it swallows: it moves the cursor to the start
// of the line, so a kill to the start of the line removes nothing.
const swallowFirstEnterHomeZshrc = "typeset -gi _swallowed=0\n" +
	"_first_enter() { if (( ! _swallowed )); then _swallowed=1; zle beginning-of-line; return 0; fi; zle .accept-line; }\n" +
	"zle -N _first_enter\n" +
	"bindkey '^M' _first_enter\n"

// TestResendAfterCursorMovedRunsAtMostOnce is the re-send after a widget took
// the first Enter and left the cursor at the start of the line. The command
// may run once, or the call may report that it could not tell; it must never
// run twice.
//
// REGRESSION: C-u alone left the first copy in the buffer when the cursor was at the start, and the re-send ran the command twice
func TestResendAfterCursorMovedRunsAtMostOnce(t *testing.T) {
	installZshrc(t, swallowFirstEnterHomeZshrc)
	c, _ := agentPaneFixture(t)

	out := filepath.Join(t.TempDir(), "runs")
	res := callTool(t, c, "execute-command", map[string]any{
		"command": "echo once >> " + out, "timeoutSeconds": 15,
	})
	text := res.text(t, "execute-command")
	sleep(time.Second)
	data, _ := os.ReadFile(out)
	runs := strings.Count(string(data), "once")
	if runs > 1 {
		t.Fatalf("the command ran %d times, want at most once (call said: %s)", runs, text)
	}
	if !res.IsError && runs != 1 {
		t.Fatalf("execute-command reported success, but the command ran %d times: %s", runs, text)
	}
}

// ---- Reclaim needs a positive answer ----

// TestCanonicalShellIsNeverReclaimed is an agent-marked, unslotted pane whose
// shell reads in canonical mode: dash running a loop of builtins, and an idle
// bash with its line editor switched off. Neither says it is reading, so
// neither may be taken: the first would have keys typed into its loop.
//
// REGRESSION: a shell without a line editor counted as ready for reclaim, so a dash pane running a builtin loop was reclaimed and typed into
func TestCanonicalShellIsNeverReclaimed(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Stat("/bin/dash"); err != nil {
		t.Skip("dash is not installed")
	}
	for _, tc := range []struct {
		name, shell, fg, run string
		positiveOnLinux      bool
	}{
		{"dash running a builtin loop", "/bin/dash", "dash", "while :; do :; done", false},
		{"bash --noediting at its prompt", bash + " --noediting --norc --noprofile", "bash", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.positiveOnLinux && runtime.GOOS == "linux" {
				t.Skip("Linux reports a canonical read, so an idle shell here is a positive answer")
			}
			c, self := agentPaneFixture(t)
			pane := rawSplit(t, self, "-h", tc.shell)
			waitForForeground(t, pane, tc.fg)
			markAgentUnslotted(t, pane)
			if tc.run != "" {
				tmuxExec(t, "send-keys", "-t", pane, tc.run, "Enter")
			}
			sleep(500 * time.Millisecond)
			screen := tmuxExec(t, "capture-pane", "-p", "-t", pane)
			marks := mcpMarks(t, pane)

			c.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &map[string]any{})

			if one := slotPaneID(t, self, 1); one == pane {
				t.Errorf("slot 1 reclaimed %s (%s)", pane, tc.name)
			}
			if got := mcpMarks(t, pane); got != marks {
				t.Errorf("the marks on %s went from %q to %q", pane, marks, got)
			}
			if got := tmuxExec(t, "capture-pane", "-p", "-t", pane); got != screen {
				t.Errorf("the screen of %s changed:\nbefore:\n%s\nafter:\n%s", pane, screen, got)
			}
			tmuxExec(t, "send-keys", "-t", pane, "C-c")
		})
	}
}

// TestPromptHoldsFindsAWrappedId pins the read-back the re-send depends on: an
// id on the prompt's rows is found even when wrapping split it, and an id only
// in older output is not on the prompt.
func TestPromptHoldsFindsAWrappedId(t *testing.T) {
	const id = "0b6e-4f1c"
	cases := []struct {
		name, screen string
		want         bool
	}{
		{"on the prompt line", "old output\n% printf > /tmp/tmux-mcp-0b6e-4f1c.start\n\n", true},
		{"split by wrapping", "% printf > /tmp/tmux-mcp-0b6e\n-4f1c.exit\n", true},
		{"cleared", "% printf > /tmp/tmux-mcp-0b6e-4f1c.start\nline 2\nline 3\nline 4\n% \n", false},
		{"empty screen", "", false},
	}
	for _, tc := range cases {
		if got := promptHolds(tc.screen, id); got != tc.want {
			t.Errorf("%s: promptHolds = %v, want %v", tc.name, got, tc.want)
		}
	}
}
