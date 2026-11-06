package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// herdrEnvVars is every variable herdr injects into its panes. isolateTmux
// clears them for the whole suite, and each herdr scenario hands its own named
// session's values to one server process at a time.
var herdrEnvVars = []string{
	"HERDR_ENV", "HERDR_PANE_ID", "HERDR_TAB_ID", "HERDR_WORKSPACE_ID",
	"HERDR_SESSION", "HERDR_SOCKET_PATH", "HERDR_BIN_PATH",
}

// herdrSessionSeq numbers the sessions one test process starts.
var herdrSessionSeq atomic.Int64

// herdrSession is a private, headless herdr server under a session name only
// this test uses, plus the workspace whose root pane plays the agent's pane.
type herdrSession struct {
	name      string
	socket    string
	workspace string
	tab       string
	agent     string // the root pane: where the server under test "runs"
}

// skipWithoutHerdr is the guard every herdr scenario starts with.
func skipWithoutHerdr(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires herdr")
	}
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
}

// startHerdrSession runs `herdr --session <name> server` in the background and
// creates one workspace in it, rooted at a temp dir.
//
// The session name is unique to this process and this test, and every herdr
// command here passes --session explicitly, so nothing can reach the default
// session or one the developer is attached to. The server inherits the suite's
// environment, which has no TMUX, TMUX_PANE or HERDR_* left in it and has
// ZDOTDIR pointing at the suite's private directory, so its panes load no user
// rc.
//
// Cleanup stops that server and deletes that session, and nothing else.
func startHerdrSession(t *testing.T) *herdrSession {
	t.Helper()
	return startHerdrSessionEnv(t, nil)
}

// startHerdrSessionEnv is startHerdrSession with extra environment for the
// herdr server, such as the SHELL its panes run.
func startHerdrSessionEnv(t *testing.T, extraEnv []string) *herdrSession {
	t.Helper()
	skipWithoutHerdr(t)

	h := &herdrSession{name: fmt.Sprintf("tmuxmcp-test-%d-%d", os.Getpid(), herdrSessionSeq.Add(1))}
	server := exec.Command("herdr", "--session", h.name, "server")
	if len(extraEnv) > 0 {
		server.Env = append(os.Environ(), extraEnv...)
	}
	var serverLog bytes.Buffer
	server.Stdout, server.Stderr = &serverLog, &serverLog
	if err := server.Start(); err != nil {
		t.Fatalf("start herdr server for session %s: %v", h.name, err)
	}
	exited := make(chan struct{})
	go func() { _ = server.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = exec.Command("herdr", "--session", h.name, "server", "stop").Run()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = server.Process.Kill()
			<-exited
		}
		_ = exec.Command("herdr", "session", "delete", h.name).Run()
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		out, err := exec.Command("herdr", "--session", h.name, "status", "server").CombinedOutput()
		if err == nil && strings.Contains(string(out), "status: running") {
			for _, line := range strings.Split(string(out), "\n") {
				if s, ok := strings.CutPrefix(strings.TrimSpace(line), "socket: "); ok {
					h.socket = s
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("herdr session %s did not come up within 10s: %s\nserver log:\n%s",
				h.name, out, serverLog.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if h.socket == "" {
		t.Fatalf("herdr status for session %s named no socket", h.name)
	}

	var created struct {
		Result struct {
			RootPane struct {
				PaneID      string `json:"pane_id"`
				TabID       string `json:"tab_id"`
				WorkspaceID string `json:"workspace_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	h.cli(t, &created, "workspace", "create", "--cwd", t.TempDir(), "--no-focus")
	h.agent = created.Result.RootPane.PaneID
	h.tab = created.Result.RootPane.TabID
	h.workspace = created.Result.RootPane.WorkspaceID
	if h.agent == "" {
		t.Fatalf("herdr workspace create returned no root pane")
	}
	h.waitIdleShell(t, h.agent)
	return h
}

// cli runs one herdr command against this session and decodes its JSON answer
// into out, when out is not nil.
func (h *herdrSession) cli(t *testing.T, out any, args ...string) {
	t.Helper()
	full := append([]string{"--session", h.name}, args...)
	raw, err := exec.Command("herdr", full...).Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		t.Fatalf("herdr %s: %v\n%s", strings.Join(full, " "), err, stderr)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode herdr %s: %v\n%s", strings.Join(args, " "), err, raw)
		}
	}
}

// mcpClient starts a server that believes it runs in the agent pane of this
// herdr session: the variables herdr itself injects into that pane, and no
// TMUX or TMUX_PANE.
//
// The backend is named on the command line. This server is a child of the
// test process, not of the agent pane's shell, so detection's ancestry check
// would rightly find the claim false and answer "no window"; that rule is
// covered by detect_test.go and detect_probe_test.go, and these scenarios are
// about what the herdr backend does once it is chosen.
func (h *herdrSession) mcpClient(t *testing.T) *mcpClient {
	t.Helper()
	return startMCPClient(t, []string{
		"HERDR_ENV=1",
		"HERDR_PANE_ID=" + h.agent,
		"HERDR_TAB_ID=" + h.tab,
		"HERDR_WORKSPACE_ID=" + h.workspace,
		"HERDR_SESSION=" + h.name,
		"HERDR_SOCKET_PATH=" + h.socket,
	}, "--backend=herdr")
}

// herdrRect is a pane rectangle as `herdr pane layout` reports it.
type herdrRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// layout returns every pane in the agent's tab, by id.
func (h *herdrSession) layout(t *testing.T) map[string]herdrRect {
	t.Helper()
	var out struct {
		Result struct {
			Layout struct {
				Panes []struct {
					PaneID string    `json:"pane_id"`
					Rect   herdrRect `json:"rect"`
				} `json:"panes"`
			} `json:"layout"`
		} `json:"result"`
	}
	h.cli(t, &out, "pane", "layout", "--pane", h.agent)
	panes := map[string]herdrRect{}
	for _, p := range out.Result.Layout.Panes {
		panes[p.PaneID] = p.Rect
	}
	return panes
}

// newPane runs a step and returns the one pane it added to the agent's tab.
func (h *herdrSession) newPane(t *testing.T, step func()) string {
	t.Helper()
	before := h.layout(t)
	step()
	after := h.layout(t)
	var added []string
	for id := range after {
		if _, ok := before[id]; !ok {
			added = append(added, id)
		}
	}
	if len(added) != 1 {
		t.Fatalf("the step added %d panes (%v), want exactly one.\nbefore: %v\nafter: %v",
			len(added), added, before, after)
	}
	return added[0]
}

// paneJSON is `herdr pane get` for one pane, raw, for label checks.
func (h *herdrSession) paneJSON(t *testing.T, pane string) map[string]any {
	t.Helper()
	var out struct {
		Result struct {
			Pane map[string]any `json:"pane"`
		} `json:"result"`
	}
	h.cli(t, &out, "pane", "get", pane)
	return out.Result.Pane
}

// screen reads a pane's recent output.
func (h *herdrSession) screen(t *testing.T, pane string) string {
	t.Helper()
	full := []string{"--session", h.name, "pane", "read", pane, "--source", "recent-unwrapped", "--lines", "200"}
	raw, err := exec.Command("herdr", full...).Output()
	if err != nil {
		t.Fatalf("herdr %s: %v", strings.Join(full, " "), err)
	}
	return string(raw)
}

// waitIdleShell waits until a pane's foreground process is its own shell, with
// a pause for the prompt to be drawn.
func (h *herdrSession) waitIdleShell(t *testing.T, pane string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var out struct {
			Result struct {
				ProcessInfo struct {
					ShellPID int `json:"shell_pid"`
					FgPGID   int `json:"foreground_process_group_id"`
				} `json:"process_info"`
			} `json:"result"`
		}
		h.cli(t, &out, "pane", "process-info", "--pane", pane)
		if pi := out.Result.ProcessInfo; pi.ShellPID != 0 && pi.ShellPID == pi.FgPGID {
			time.Sleep(300 * time.Millisecond)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("herdr pane %s never settled at its shell prompt", pane)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// installSlowZshrcForHerdr is installSlowZshrc for panes herdr spawns, which
// run $SHELL rather than tmux's default-shell.
func installSlowZshrcForHerdr(t *testing.T) {
	t.Helper()
	if filepath.Base(os.Getenv("SHELL")) != "zsh" {
		t.Skipf("herdr spawns $SHELL=%q, not zsh, so a .zshrc is never sourced", os.Getenv("SHELL"))
	}
	rc := filepath.Join(os.Getenv("ZDOTDIR"), ".zshrc")
	if err := os.WriteFile(rc, []byte(slowZshrc), 0o644); err != nil {
		t.Fatalf("write %s: %v", rc, err)
	}
	t.Cleanup(func() { _ = os.Remove(rc) })
}

// ---- Scenarios ----

// TestHerdrCreatedPaneWaitsUntilLineEditorReads is the shell-readiness case on
// herdr: the slot pane sources a slow rc that drops typeahead, and the command
// sent to it must still run.
//
// REGRESSION: a creating tool typed into a new pane before the shell's line editor was reading, so the command was discarded and never ran
func TestHerdrCreatedPaneWaitsUntilLineEditorReads(t *testing.T) {
	h := startHerdrSession(t)
	installSlowZshrcForHerdr(t)

	t.Run("send-keys", func(t *testing.T) {
		c := h.mcpClient(t)
		marker := filepath.Join(t.TempDir(), "M")
		started := time.Now()
		c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo M > " + marker, "enter": true}, &map[string]any{})
		if !waitForFile(marker, commandLandingBudget-time.Since(started)) {
			t.Errorf("the command sent to a new herdr slot pane never ran: %s was not created within %v",
				marker, commandLandingBudget)
		}
	})

	t.Run("execute-command", func(t *testing.T) {
		c := h.mcpClient(t)
		want := fmt.Sprintf("ran-%d", time.Now().UnixNano())
		res := callTool(t, c, "execute-command", map[string]any{
			"command": "echo " + want, "slot": 2,
			"timeoutSeconds": int(commandLandingBudget / time.Second),
		})
		text := res.text(t, "execute-command")
		if res.IsError || strings.Contains(text, `"timedOut": true`) || !strings.Contains(text, want) {
			t.Errorf("execute-command into a new herdr slot pane did not return %q.\nresult: %s", want, text)
		}
	})
}

// TestHerdrHelperIsReusedAfterServerRestart is reuse on herdr: herdr panes carry
// no options, so the registry has to outlive the server process some other way,
// and a second server in the same agent pane must find slot 1 and type into it.
//
// REGRESSION: an idle helper pane was not reused, so every request opened another pane
func TestHerdrHelperIsReusedAfterServerRestart(t *testing.T) {
	h := startHerdrSession(t)

	c := h.mcpClient(t)
	one := h.newPane(t, func() {
		c.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &map[string]any{})
	})
	c.close()
	h.waitIdleShell(t, one)
	before := h.layout(t)

	c2 := h.mcpClient(t)
	marker := filepath.Join(t.TempDir(), "restart")
	var out map[string]any
	c2.callToolJSON(t, "send-keys", map[string]any{"keys": "echo B > " + marker, "enter": true}, &out)

	if after := h.layout(t); len(after) != len(before) {
		t.Errorf("the second server opened a new pane: %d panes, was %d (%v)", len(after), len(before), after)
	}
	if out["created"] != false {
		t.Errorf("the second server reported created=%v for the slot-1 pane the first one opened", out["created"])
	}
	if !waitForFile(marker, 5*time.Second) {
		t.Errorf("the command sent by the second server did not run in slot 1 (%s):\n%s", one, h.screen(t, one))
	}
}

// TestHerdrPlacementTable is the placement table on herdr, read off
// `herdr pane layout`: slot 1 right of the agent, slot 2 under slot 1, slot 3
// under the agent.
//
// REGRESSION: slot placement depended on what had been adopted, so slots 2 and 3 stacked under the agent and squeezed it
func TestHerdrPlacementTable(t *testing.T) {
	h := startHerdrSession(t)
	c := h.mcpClient(t)

	open := func(slot int) string {
		return h.newPane(t, func() {
			c.callToolJSON(t, "open-pane", map[string]any{"slot": slot}, &map[string]any{})
		})
	}
	one, two, three := open(1), open(2), open(3)
	l := h.layout(t)
	agent, r1, r2, r3 := l[h.agent], l[one], l[two], l[three]

	if r1.X <= agent.X || r1.Y != agent.Y {
		t.Errorf("slot 1 is at %+v; it belongs right of the agent at %+v (layout %v)", r1, agent, l)
	}
	if r2.X != r1.X || r2.Y <= r1.Y {
		t.Errorf("slot 2 is at %+v; it belongs under slot 1 at %+v (layout %v)", r2, r1, l)
	}
	if r3.X != agent.X || r3.Y <= agent.Y {
		t.Errorf("slot 3 is at %+v; it belongs under the agent at %+v (layout %v)", r3, agent, l)
	}
}

// TestHerdrUnmarkedUserShellIsNeverAdopted is the never-adopt control on herdr:
// a pane the user split by hand is not slot 1, keeps its geometry and label, and
// never receives the keys.
//
// REGRESSION: an unmarked idle user shell in the window was adopted as slot 1 and typed into
func TestHerdrUnmarkedUserShellIsNeverAdopted(t *testing.T) {
	h := startHerdrSession(t)

	var split struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	h.cli(t, &split, "pane", "split", h.agent, "--direction", "right", "--no-focus")
	users := split.Result.Pane.PaneID
	h.waitIdleShell(t, users)
	usersBefore := h.layout(t)[users]
	labelBefore := h.paneJSON(t, users)["label"]

	c := h.mcpClient(t)
	keys := fmt.Sprintf("echo NEVER-ADOPTED-%d", time.Now().UnixNano())
	var out map[string]any
	one := h.newPane(t, func() {
		c.callToolJSON(t, "send-keys", map[string]any{"keys": keys, "enter": true}, &out)
	})
	time.Sleep(500 * time.Millisecond)

	if one == users || one == h.agent {
		t.Errorf("slot 1 is %s, the user's pane or the agent's own", one)
	}
	if strings.Contains(h.screen(t, users), keys) {
		t.Errorf("the keys sent to slot 1 landed in the user's pane %s", users)
	}
	if after := h.layout(t)[users]; after != usersBefore {
		t.Errorf("the user's pane %s went from %+v to %+v", users, usersBefore, after)
	}
	if label := h.paneJSON(t, users)["label"]; label != labelBefore {
		t.Errorf("the user's pane %s was relabelled from %v to %v", users, labelBefore, label)
	}
}
