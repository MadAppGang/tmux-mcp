package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- A multiplexer server that is gone means the pane is gone ----

// TestTmuxSaysGone pins the tmux messages that mean the pane, or the whole
// server it lived on, no longer exists — and that nothing else counts.
func TestTmuxSaysGone(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{"no server running on /private/tmp/tmux-501/mcp-headless", true},
		{"error connecting to /private/tmp/tmux-501/mcp-headless (No such file or directory)", true},
		{"error connecting to /private/tmp/tmux-501/mcp-headless (Connection refused)", true},
		{"server exited unexpectedly", true},
		{"lost server", true},
		{"can't find pane: <pane>", true},
		{"can't find pane <pane>", true},
		{"some warning\nno server running on /tmp/x", true},
		{"error connecting to /x (Permission denied)", false},
		{"error connecting to /x (Operation timed out)", false},
		{"error connecting to /x", false},
		{"error connecting to /x (no such file or directory)", false},
		{"", false},
		{"unknown option -- z", false},
		{"invalid format", false},
		{"protocol version mismatch (client 8, server 7)", false},
		{"open terminal failed: not a terminal", false},
		{"the server mentions no server running on nothing", false},
	}
	for _, tc := range cases {
		if got := tmuxSaysGone(tc.detail); got != tc.want {
			t.Errorf("tmuxSaysGone(%q) = %v, want %v", tc.detail, got, tc.want)
		}
	}
}

// TestPaneStateOnAServerThatIsGone reads a pane on a tmux socket no server
// listens on. That is a pane that no longer exists.
func TestPaneStateOnAServerThatIsGone(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("tmuxmcp-gone-%d", os.Getpid())
	_, err := newTmuxClient().runWithSocket(context.Background(), socket, "display-message", "-p", "x")
	if err == nil {
		t.Fatalf("a call on socket %s succeeded; it should have no server", socket)
	}
	if !errors.Is(err, errTmuxGone) {
		t.Fatalf("the error of a call on a socket with no server was not classified as gone: %v", err)
	}
	if strings.Contains(err.Error(), "tmux ") {
		t.Errorf("the classified error names the multiplexer: %v", err)
	}
}

// TestHerdrServerGoneIsPaneGone reads a pane while herdr reports its server is
// not running — the socket is gone, or nothing answers on it.
func TestHerdrServerGoneIsPaneGone(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\n" +
		`echo '{"id":"cli:pane:process_info","error":{"code":"server_not_running",` +
		`"message":"no herdr server is running at /tmp/x.sock; run herdr to start or attach it"}}' >&2` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &herdrBackend{bin: fake}
	if _, err := b.Foreground(context.Background(), newPaneRef("w1:p1")); !errors.Is(err, errPaneGone) {
		t.Fatalf("Foreground with no herdr server = %v, want errPaneGone", err)
	}
}

// ---- The last pane of an isolated server ----

// TestIsolatedShellThatExitsOnFailure is errexit with pipefail in an isolated
// slot. The shell exits on the failure, and because it is the last pane on its
// private server, the server exits with it. The call must see that the pane is
// gone, and the next call into the slot must get a fresh one.
//
// REGRESSION: a shell that exited as the last pane of the isolated server left its call waiting, because a server that is gone did not count as a pane that is gone
func TestIsolatedShellThatExitsOnFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	zsh, zerr := exec.LookPath("zsh")
	for _, tc := range []struct{ name, exec string }{
		{"bash -e -o pipefail", bash + " --noprofile --norc -e -o pipefail"},
		{"zsh with errexit and pipefail", zsh + " -f -o errexit -o pipefail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.name, "zsh") && zerr != nil {
				t.Skip("zsh is not installed")
			}
			// The isolated server takes its panes' shell from SHELL when it
			// starts, so any server left from an earlier test is stopped first.
			// It lives under the suite's private TMUX_TMPDIR.
			_ = exec.Command("tmux", "-L", headlessSocket, "kill-server").Run()
			t.Cleanup(func() { _ = exec.Command("tmux", "-L", headlessSocket, "kill-server").Run() })

			wrapper := filepath.Join(t.TempDir(), "errexit-shell")
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+tc.exec+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			name := uniqueSession(t)
			tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
			t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })
			self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
			c := startMCPClient(t, []string{"TMUX_PANE=" + self, "SHELL=" + wrapper})

			started := time.Now()
			out, texts := runExec(t, c, map[string]any{
				"slot": 2, "isolated": true,
				"command": "printf 'before\\n'; sleep 1; false; printf 'should-not-run\\n'",
			})
			if took := time.Since(started); took > 10*time.Second {
				t.Errorf("the call took %v to notice the shell had exited", took.Round(time.Millisecond))
			}
			if out.ExitCode != 1 {
				t.Errorf("exit code %d, want 1 (%q)", out.ExitCode, texts)
			}
			if !strings.Contains(out.Output, "before") || strings.Contains(out.Output, "should-not-run") {
				t.Errorf("output %q, want before and not should-not-run", out.Output)
			}
			if !strings.Contains(strings.Join(texts[1:], "\n"), "shell exited") {
				t.Errorf("the call did not say the pane's shell exited: %q", texts)
			}

			next, texts := runExec(t, c, map[string]any{"slot": 2, "isolated": true, "command": "echo again"})
			var created struct {
				Created bool `json:"created"`
			}
			if err := json.Unmarshal([]byte(texts[0]), &created); err != nil {
				t.Fatalf("decode %s: %v", texts[0], err)
			}
			if !created.Created || next.ExitCode != 0 || strings.TrimSpace(next.Output) != "again" {
				t.Errorf("the next isolated call into the slot = %q, want a fresh pane running echo again", texts)
			}
		})
	}
}

// TestUnreachableServerIsNotGone takes the private tmux server's socket
// directory away from this process for two seconds while a command runs. Every
// pane read then fails with Permission denied, which says nothing about the
// pane: the shell lives on, and the call must wait for the command to finish.
//
// REGRESSION: every "error connecting to" counted as a server that was gone, so Permission denied ended a running command with -1 and a false shell-exited note
func TestUnreachableServerIsNotGone(t *testing.T) {
	c, self := agentPaneFixture(t)
	socketDir := filepath.Dir(tmuxExec(t, "display-message", "-p", "-t", self, "#{socket_path}"))
	info, err := os.Stat(socketDir)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	t.Cleanup(func() { _ = os.Chmod(socketDir, mode) })
	runExec(t, c, map[string]any{"command": "true"})

	denied := make(chan error, 1)
	go func() {
		time.Sleep(time.Second)
		if err := os.Chmod(socketDir, 0); err != nil {
			denied <- err
			return
		}
		time.Sleep(2 * time.Second)
		denied <- os.Chmod(socketDir, mode)
	}()
	out, texts := runExec(t, c, map[string]any{"command": "printf 'before\\n'; sleep 4; printf 'after\\n'"})
	if err := <-denied; err != nil {
		t.Fatalf("the fixture could not change the socket directory: %v", err)
	}
	if out.ExitCode != 0 || !strings.Contains(out.Output, "before") || !strings.Contains(out.Output, "after") {
		t.Errorf("exit code %d, output %q; want 0 with before and after", out.ExitCode, out.Output)
	}
	if len(texts) > 1 {
		t.Errorf("the call carried notes for a pane that never went away: %q", texts[1:])
	}
}
