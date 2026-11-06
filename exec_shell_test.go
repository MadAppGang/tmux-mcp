package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- The exit status follows the pane's real shell ----

// execCallBudget bounds every execute-command call these tests make. A call
// that hangs fails on the bound instead of hanging the suite.
const execCallBudget = 15

// execOutcomeJSON is the part of execute-command's result these tests read.
type execOutcomeJSON struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut"`
}

// runExec calls execute-command with a bound and decodes its result. It
// returns the decoded JSON and every text the call returned.
func runExec(t *testing.T, c *mcpClient, args map[string]any) (execOutcomeJSON, []string) {
	t.Helper()
	args["timeoutSeconds"] = execCallBudget
	started := time.Now()
	res := callTool(t, c, "execute-command", args)
	took := time.Since(started)
	var texts []string
	for _, item := range res.Content {
		texts = append(texts, item.Text)
	}
	if res.IsError {
		t.Fatalf("execute-command %q failed after %v: %s", args["command"], took.Round(time.Millisecond),
			strings.Join(texts, "\n"))
	}
	var out execOutcomeJSON
	if err := json.Unmarshal([]byte(texts[0]), &out); err != nil {
		t.Fatalf("decode %s: %v", texts[0], err)
	}
	if out.TimedOut {
		t.Fatalf("execute-command %q did not finish within %ds: %s", args["command"], execCallBudget, texts[0])
	}
	return out, texts
}

// requireExitCodes runs a failing and a succeeding command in a slot and checks
// both exit codes and the output.
func requireExitCodes(t *testing.T, c *mcpClient, slot int) {
	t.Helper()
	out, _ := runExec(t, c, map[string]any{"slot": slot, "command": "false"})
	if out.ExitCode != 1 {
		t.Errorf("false returned exit code %d, want 1", out.ExitCode)
	}
	out, _ = runExec(t, c, map[string]any{"slot": slot, "command": "exit 3"})
	if out.ExitCode != 3 {
		t.Errorf("exit 3 returned exit code %d, want 3", out.ExitCode)
	}
	out, _ = runExec(t, c, map[string]any{"slot": slot, "command": "echo hi"})
	if out.ExitCode != 0 || strings.TrimSpace(out.Output) != "hi" {
		t.Errorf("echo hi returned exit code %d and output %q, want 0 and \"hi\"", out.ExitCode, out.Output)
	}
}

// TestExitStatusFollowsThePanesShell runs execute-command in a pane whose shell
// is not the one -shell-type names, both ways round, and in dash, which the
// flag cannot name at all. Each must report the command's real exit status.
//
// REGRESSION: the exit-status wrapper was spelled for -shell-type, not the pane's shell, so a mismatch wrote an empty status and execute-command waited forever
func TestExitStatusFollowsThePanesShell(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	cases := []struct{ name, flag, command string }{
		{"a bash pane under -shell-type zsh", "zsh", "exec " + bash + " --norc --noprofile"},
		{"a zsh pane under -shell-type bash", "bash", ""},
		{"a dash pane under -shell-type zsh", "zsh", "exec /bin/dash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.command, "dash") {
				if _, err := os.Stat("/bin/dash"); err != nil {
					t.Skip("dash is not installed")
				}
			}
			c, _ := agentPaneFixture(t, "--shell-type="+tc.flag)
			if tc.command != "" {
				useDefaultCommand(t, tc.command)
			} else {
				requireZshPanes(t)
			}
			requireExitCodes(t, c, 1)
		})
	}
}

// TestHerdrExitStatusFollowsThePanesShell is the same mismatch under herdr: its
// panes run the server's $SHELL, here bash, while -shell-type says zsh.
//
// REGRESSION: under herdr a bash pane with -shell-type zsh wrote an empty status and execute-command waited forever
func TestHerdrExitStatusFollowsThePanesShell(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	h := startHerdrSessionEnv(t, []string{"SHELL=" + bash})
	c := startMCPClient(t, []string{
		"HERDR_ENV=1",
		"HERDR_PANE_ID=" + h.agent,
		"HERDR_TAB_ID=" + h.tab,
		"HERDR_WORKSPACE_ID=" + h.workspace,
		"HERDR_SESSION=" + h.name,
		"HERDR_SOCKET_PATH=" + h.socket,
		"SHELL=" + bash,
	}, "--backend=herdr", "--shell-type=zsh")
	requireExitCodes(t, c, 1)
}

// ---- The status does not cut the output short ----

// TestBackgroundOutputIsKept is a command that leaves a background job
// writing to its output after the command itself is done. The job holds the
// pipe to tee open, and its output belongs to the result as much as the
// foreground's does.
//
// REGRESSION: the status was written before tee finished, so the call returned without what a background job printed later
func TestBackgroundOutputIsKept(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct{ name, command string }{
		{"zsh", ""},
		{"bash", "exec " + bash + " --norc --noprofile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := agentPaneFixture(t)
			if tc.command != "" {
				useDefaultCommand(t, tc.command)
			} else {
				requireZshPanes(t)
			}
			out, _ := runExec(t, c, map[string]any{
				"command": "(sleep 3; printf 'late\\n') & printf 'early\\n'",
			})
			if !strings.Contains(out.Output, "early") || !strings.Contains(out.Output, "late") {
				t.Errorf("output %q, want both early and late", out.Output)
			}
			if out.ExitCode != 0 {
				t.Errorf("exit code %d, want 0", out.ExitCode)
			}
		})
	}
}

// TestErrexitShellReportsTheStatus runs failing commands in shells with
// errexit set. errexit must not take the status write down with the command,
// and it must still stop the command where it would stop it typed directly.
//
// REGRESSION: under errexit a failing command ended the wrapper before it wrote its status, so false was reported as an unreadable -1
// REGRESSION: the command ran as an if condition, where errexit never fires, so false; printf ran the printf and returned 0
func TestErrexitShellReportsTheStatus(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct{ name, command, rc string }{
		{"bash -e", "exec " + bash + " --noprofile --norc -e", ""},
		{"zsh with errexit", "", "setopt errexit\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rc != "" {
				installZshrc(t, tc.rc)
			}
			c, _ := agentPaneFixture(t)
			if tc.command != "" {
				useDefaultCommand(t, tc.command)
			}
			for _, want := range []struct {
				command string
				code    int
			}{
				{"false", 1},
				{"exit 42", 42},
				{"true", 0},
				{"false; printf 'should-not-run\\n'", 1},
			} {
				out, texts := runExec(t, c, map[string]any{"command": want.command})
				if out.ExitCode != want.code {
					t.Errorf("%s returned exit code %d, want %d", want.command, out.ExitCode, want.code)
				}
				if strings.Contains(out.Output, "should-not-run") {
					t.Errorf("%s kept running after false under errexit: %q", want.command, out.Output)
				}
				if len(texts) > 1 {
					t.Errorf("%s carried a note: %q", want.command, texts[1:])
				}
			}
		})
	}
}

// TestCommandWithItsOwnExitTrap runs commands that install an EXIT trap of
// their own, which replaces the one the wrapper records the status with. The
// status must still come back where the shell keeps running after the
// command, and the call must end within its bound where it cannot.
//
// REGRESSION: a command that set its own EXIT trap left no status for the wrapper to read
func TestCommandWithItsOwnExitTrap(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	t.Run("zsh", func(t *testing.T) {
		c, _ := agentPaneFixture(t)
		requireZshPanes(t)
		for _, want := range []struct {
			command string
			code    int
		}{{"trap 'echo mine' EXIT; true", 0}, {"trap 'echo mine' EXIT; false", 1}} {
			out, texts := runExec(t, c, map[string]any{"command": want.command})
			if out.ExitCode != want.code || len(texts) > 1 {
				t.Errorf("%s returned exit code %d (%q), want %d", want.command, out.ExitCode, texts, want.code)
			}
			if !strings.Contains(out.Output, "mine") {
				t.Errorf("%s: the command's own trap did not run: %q", want.command, out.Output)
			}
		}
	})
	t.Run("bash -e, failing", func(t *testing.T) {
		c, _ := agentPaneFixture(t)
		useDefaultCommand(t, "exec "+bash+" --noprofile --norc -e")
		out, texts := runExec(t, c, map[string]any{"command": "trap 'echo mine' EXIT; false"})
		note := len(texts) > 1 && strings.Contains(texts[1], "exit status could not be read")
		if out.ExitCode != 1 && !(out.ExitCode == -1 && note) {
			t.Errorf("exit code %d (%q), want 1, or -1 with the unreadable note", out.ExitCode, texts)
		}
	})
}

// TestShellThatExitsOnFailure runs a failing command in shells with errexit
// and pipefail set. The failure fails the wrapper's pipeline, and the shell
// exits as it would if the command had been typed directly. The call must see
// that, return the command's status with a note that the shell exited, and the
// next call into the slot must get a fresh pane.
//
// REGRESSION: a shell that exited on a failed pipeline left the call waiting for a prompt that never came
func TestShellThatExitsOnFailure(t *testing.T) {
	bash, err := lookBash()
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct{ name, command, rc string }{
		{"bash -e -o pipefail", "exec " + bash + " --noprofile --norc -e -o pipefail", ""},
		{"zsh with errexit and pipefail", "", "setopt errexit pipefail\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := agentPaneFixture(t)
			if tc.command != "" {
				useDefaultCommand(t, tc.command)
			} else {
				installZshrc(t, tc.rc)
			}

			started := time.Now()
			out, texts := runExec(t, c, map[string]any{"command": "false; printf 'should-not-run\\n'"})
			if took := time.Since(started); took > 10*time.Second {
				t.Errorf("the call took %v to notice the shell had exited", took.Round(time.Millisecond))
			}
			if out.ExitCode != 1 {
				t.Errorf("exit code %d, want 1 (%q)", out.ExitCode, texts)
			}
			if strings.Contains(out.Output, "should-not-run") {
				t.Errorf("the command kept running after false: %q", out.Output)
			}
			if !strings.Contains(strings.Join(texts[1:], "\n"), "shell exited") {
				t.Errorf("the call did not say the pane's shell exited: %q", texts)
			}

			next, texts := runExec(t, c, map[string]any{"command": "echo again"})
			var created struct {
				Created bool `json:"created"`
			}
			if err := json.Unmarshal([]byte(texts[0]), &created); err != nil {
				t.Fatalf("decode %s: %v", texts[0], err)
			}
			if !created.Created || next.ExitCode != 0 || strings.TrimSpace(next.Output) != "again" {
				t.Errorf("the next call into the slot = %q, want a fresh pane running echo again", texts)
			}
		})
	}
}

// ---- A status that cannot be read ends the wait ----

// spoilExitFile finds the exit file of the command running in a pane — from
// the argv of the tee the wrapper starts there — and spoils it before the
// command finishes. Only that pane's own process tree is searched, so no other
// server's files are touched.
func spoilExitFile(t *testing.T, pane string, spoil func(exitFile string) error) <-chan error {
	t.Helper()
	panePID := tmuxExec(t, "display-message", "-p", "-t", pane, "#{pane_pid}")
	done := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if out := teeOutFile(panePID); out != "" {
				done <- spoil(strings.TrimSuffix(out, ".out") + ".exit")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		done <- fmt.Errorf("no tee appeared under pane process %s", panePID)
	}()
	return done
}

// teeOutFile returns the output file of a tee running below a process, or "".
func teeOutFile(rootPID string) string {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,args=").Output()
	if err != nil {
		return ""
	}
	parent := map[string]string{}
	args := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		parent[f[0]], args[f[0]] = f[1], f[2:]
	}
	for pid, argv := range args {
		if filepath.Base(argv[0]) != "tee" || len(argv) < 2 || !strings.HasPrefix(argv[1], "/tmp/tmux-mcp-") {
			continue
		}
		for p, hops := parent[pid], 0; p != "" && hops < 16; p, hops = parent[p], hops+1 {
			if p == rootPID {
				return argv[1]
			}
		}
	}
	return ""
}

// TestUnreadableExitStatusEndsTheWait spoils the status of a running command:
// once with a junk line in the status file that the shell cannot overwrite,
// and which the wrapper then moves into place, once with a directory where the
// exit file should be, so no status line ever comes. Both commands finish, so
// the call must end promptly with exit code -1 and say why.
//
// REGRESSION: an exit file that never held a number kept execute-command polling forever
func TestUnreadableExitStatusEndsTheWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(string) error
	}{
		{"a junk status line", func(path string) error {
			if err := os.WriteFile(path+".tmp", []byte("junk\n"), 0o644); err != nil {
				return err
			}
			return os.Chmod(path+".tmp", 0o444)
		}},
		{"no status line", func(path string) error { return os.Mkdir(path, 0o755) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, self := agentPaneFixture(t)
			runExec(t, c, map[string]any{"command": "true"})
			var exitFile string
			spoiled := spoilExitFile(t, slotPaneID(t, self, 1), func(path string) error {
				exitFile = path
				return tc.spoil(path)
			})
			// The directory keeps what was moved into it, so nothing else
			// removes it; the junk file is removed by the call itself.
			t.Cleanup(func() {
				if exitFile != "" {
					_ = os.RemoveAll(exitFile)
					_ = os.Remove(exitFile + ".tmp")
				}
			})

			started := time.Now()
			out, texts := runExec(t, c, map[string]any{"command": "sleep 2"})
			if err := <-spoiled; err != nil {
				t.Fatalf("the fixture could not spoil the exit file: %v", err)
			}
			if took := time.Since(started); took > 10*time.Second {
				t.Errorf("the call took %v to give up on a finished command", took.Round(time.Millisecond))
			}
			if out.ExitCode != -1 {
				t.Errorf("exit code %d, want -1 for a status that could not be read", out.ExitCode)
			}
			if !strings.Contains(strings.Join(texts, "\n"), "exit status could not be read") {
				t.Errorf("the call did not say the exit status could not be read: %q", texts)
			}
		})
	}
}

// TestReadExitFile pins the three states of the status file.
func TestReadExitFile(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, data string
		missing    bool
		state      exitRead
		code       int
	}{
		{name: "no file yet", missing: true, state: exitPending},
		{name: "an empty file", data: "", state: exitPending},
		{name: "a number still being written", data: "3", state: exitPending},
		{name: "an empty line", data: "\n", state: exitUnreadable},
		{name: "a line that is not a number", data: "abc\n", state: exitUnreadable},
		{name: "a status", data: "3\n", state: exitWritten, code: 3},
		{name: "a status with spaces", data: " 0 \n", state: exitWritten, code: 0},
	}
	for i, tc := range cases {
		path := filepath.Join(dir, "exit-"+string(rune('a'+i)))
		if !tc.missing {
			if err := os.WriteFile(path, []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		code, state := readExitFile(path)
		if state != tc.state || (state == exitWritten && code != tc.code) {
			t.Errorf("%s: readExitFile = (%d, %v), want (%d, %v)", tc.name, code, state, tc.code, tc.state)
		}
	}
}

// TestShellFromArgv pins how a pane's real shell is named from its command
// line, and that a program which is not a shell names none.
func TestShellFromArgv(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"-zsh"}, "zsh"},
		{[]string{"/bin/bash", "--norc"}, "bash"},
		{[]string{"/opt/homebrew/bin/fish", "-l"}, "fish"},
		{[]string{"/bin/dash"}, "dash"},
		{[]string{"perl", "xsh.pl"}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := shellFromArgv(tc.argv); got != tc.want {
			t.Errorf("shellFromArgv(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// TestWrapperForUnknownShellUsesTheFlag pins the fallback: a pane whose
// shell cannot be named gets the wrapper -shell-type names.
func TestWrapperForUnknownShellUsesTheFlag(t *testing.T) {
	if got := wrapperShell("", "fish"); got != "fish" {
		t.Errorf("wrapperShell(\"\", fish) = %q, want fish", got)
	}
	if got := wrapperShell("bash", "fish"); got != "bash" {
		t.Errorf("wrapperShell(bash, fish) = %q, want bash: the real shell wins", got)
	}
}
