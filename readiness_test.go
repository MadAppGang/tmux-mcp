package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slowZshrc is a zsh rc file that keeps the shell busy for 2.5 seconds before
// its line editor starts, and then throws away anything typed in the meantime.
//
// Both halves copy what real rc files do. The wait is zselect, a builtin, so it
// forks nothing: the shell stays in the foreground with its own pid the whole
// time, which is exactly the state in which "the shell is the foreground
// process" is already true and the shell is still not reading. It is a timed
// wait rather than a busy loop so that its length is 2.5 seconds on the wall
// clock however loaded the machine is. The drain is what powerlevel10k's
// instant prompt and similar setups do to typeahead; without it the queued
// bytes would simply be read late, and a test without it would pass by luck.
const slowZshrc = "zmodload zsh/zselect; zselect -t 250\n" +
	"while read -t 0 -k 1 -s _x; do :; done\n"

// commandLandingBudget is how long a command typed into a freshly created pane
// is given to run. It covers the rc above (two and a half seconds) with room to
// spare, and is well short of anything that would hide a command that never ran.
const commandLandingBudget = 12 * time.Second

// installSlowZshrc writes slowZshrc into the suite's private ZDOTDIR, so that
// every pane spawned from now until the test ends sources it, and removes it
// when the test ends. The suite's tests are sequential, so no other test sees
// it.
//
// It skips when the panes tmux spawns are not zsh: the rc would never be read,
// and the test would pass without exercising anything.
func installSlowZshrc(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux")
	}
	shell := tmuxExec(t, "show-options", "-gv", "default-shell")
	if filepath.Base(shell) != "zsh" {
		t.Skipf("tmux spawns %q, not zsh, so a .zshrc is never sourced", shell)
	}
	zdotdir := os.Getenv("ZDOTDIR")
	if zdotdir == "" {
		t.Fatal("ZDOTDIR is not set; isolateTmux should have pointed it at a private directory")
	}
	rc := filepath.Join(zdotdir, ".zshrc")
	if err := os.WriteFile(rc, []byte(slowZshrc), 0o644); err != nil {
		t.Fatalf("write %s: %v", rc, err)
	}
	t.Cleanup(func() { _ = os.Remove(rc) })
}

// waitForFile polls for a file until the budget runs out, and reports whether
// it appeared.
func waitForFile(path string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestCreatedPaneWaitsUntilLineEditorReads is the command that never ran.
//
// A creating tool opens the slot pane and types into it straight away. If the
// shell there is still sourcing its rc file, the bytes queue on the tty and the
// rc discards them, and the tool reports success for a command that did not
// execute. The tool has to wait until the line editor is reading, and has to be
// able to tell that the keys it sent were taken.
//
// Both ways in are covered: send-keys, whose only evidence is a side effect, and
// execute-command, which waits for an exit code and so hangs until its timeout
// when the command was swallowed.
//
// REGRESSION: a creating tool typed into a new pane before the shell's line editor was reading, so the command was discarded and never ran
func TestCreatedPaneWaitsUntilLineEditorReads(t *testing.T) {
	t.Run("send-keys", func(t *testing.T) {
		installSlowZshrc(t)
		c, self := agentPaneFixture(t)

		marker := filepath.Join(t.TempDir(), "M")
		started := time.Now()
		var out map[string]any
		c.callToolJSON(t, "send-keys", map[string]any{
			"keys":  "echo M > " + marker,
			"enter": true,
		}, &out)
		returnedAfter := time.Since(started)

		if !waitForFile(marker, commandLandingBudget-returnedAfter) {
			t.Errorf("send-keys returned after %v with %v, and the command it typed into the new "+
				"slot-1 pane never ran: %s was not created within %v. The keys were sent while "+
				"the shell was still sourcing its rc file, and the rc threw them away.\nwindow:\n%s",
				returnedAfter.Round(time.Millisecond), out, marker, commandLandingBudget,
				tmuxExec(t, "list-panes", "-t", self, "-F",
					"#{pane_id} #{pane_left},#{pane_top} #{pane_width}x#{pane_height} #{pane_current_command}"))
		}
	})

	t.Run("execute-command", func(t *testing.T) {
		installSlowZshrc(t)
		c, _ := agentPaneFixture(t)

		want := fmt.Sprintf("ran-%d", time.Now().UnixNano())
		res := callTool(t, c, "execute-command", map[string]any{
			"command":        "echo " + want,
			"timeoutSeconds": int(commandLandingBudget / time.Second),
		})
		text := res.text(t, "execute-command")
		if res.IsError {
			t.Fatalf("execute-command into a fresh slot failed: %s", text)
		}
		if strings.Contains(text, `"timedOut": true`) {
			t.Fatalf("execute-command into a fresh slot timed out after %v: the command was typed "+
				"while the shell was still sourcing its rc file and never started.\nresult: %s",
				commandLandingBudget, text)
		}
		if !strings.Contains(text, want) {
			t.Errorf("execute-command did not return the command's output %q.\nresult: %s", want, text)
		}
	})
}
