package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Fixture helpers: panes the server did not open ----

// rawSplit opens a pane with tmux directly, the way the user (or an older
// server) would, and returns its id. The extra args choose the axis.
func rawSplit(t *testing.T, target string, args ...string) string {
	t.Helper()
	full := append([]string{"split-window", "-d", "-t", target, "-P", "-F", "#{pane_id}"}, args...)
	return tmuxExec(t, full...)
}

// waitForForeground waits until a pane's foreground command is the one named.
func waitForForeground(t *testing.T, pane, command string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := tmuxExec(t, "display-message", "-p", "-t", pane, "#{pane_current_command}")
		if got == command {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never reached %q in the foreground (it shows %q)", pane, command, got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForIdleZsh waits until a raw pane is at a zsh prompt, with a pause after
// the shell appears so that its prompt is drawn and its line editor is reading.
// The pause is generous because these panes source no rc at all.
func waitForIdleZsh(t *testing.T, pane string) {
	t.Helper()
	waitForForeground(t, pane, "zsh")
	time.Sleep(300 * time.Millisecond)
}

// requireZshPanes skips a test whose fixture depends on the panes tmux spawns
// being zsh, which is how "an idle shell" is recognised from the outside.
func requireZshPanes(t *testing.T) {
	t.Helper()
	if shell := tmuxExec(t, "show-options", "-gv", "default-shell"); filepath.Base(shell) != "zsh" {
		t.Skipf("tmux spawns %q, not zsh", shell)
	}
}

// markAgentUnslotted writes the mark an agent-owned pane with no slot carries:
// the witness and owner "agent", and no @mcp_slot. It is the state a v1
// split-pane pane, a duplicate-race loser and a split whose slot write failed
// all leave behind.
func markAgentUnslotted(t *testing.T, pane string) {
	t.Helper()
	tmuxExec(t, "set-option", "-p", "-t", pane, paneOptWitness, pane)
	tmuxExec(t, "set-option", "-p", "-t", pane, paneOptOwner, ownerAgent)
}

// mcpMarks reads the three registry options a pane carries, in witness, owner,
// slot order. An unmarked pane answers three empty strings.
func mcpMarks(t *testing.T, pane string) [3]string {
	t.Helper()
	out := tmuxExec(t, "display-message", "-p", "-t", pane,
		"#{"+paneOptWitness+"}\t#{"+paneOptOwner+"}\t#{"+paneOptSlot+"}")
	f := strings.Split(out, "\t")
	for len(f) < 3 {
		f = append(f, "")
	}
	return [3]string{f[0], f[1], f[2]}
}

// cellRect is a pane's position and size in cells.
type cellRect struct{ Left, Top, Width, Height int }

func (r cellRect) area() int { return r.Width * r.Height }

// contains reports whether o lies entirely inside r.
func (r cellRect) contains(o cellRect) bool {
	return o.Left >= r.Left && o.Top >= r.Top &&
		o.Left+o.Width <= r.Left+r.Width && o.Top+o.Height <= r.Top+r.Height
}

// paneRect reads a tmux pane's rectangle.
func paneRect(t *testing.T, pane string) cellRect {
	t.Helper()
	var r cellRect
	out := tmuxExec(t, "display-message", "-p", "-t", pane,
		"#{pane_left} #{pane_top} #{pane_width} #{pane_height}")
	if _, err := fmt.Sscan(out, &r.Left, &r.Top, &r.Width, &r.Height); err != nil {
		t.Fatalf("pane %s answered %q for its geometry: %v", pane, out, err)
	}
	return r
}

// windowLayout is a one-line-per-pane picture of the window around a pane, for
// failure messages.
func windowLayout(t *testing.T, self string) string {
	t.Helper()
	return tmuxExec(t, "list-panes", "-t", self, "-F",
		"#{pane_id} #{pane_left},#{pane_top} #{pane_width}x#{pane_height} "+
			"#{pane_current_command} own=#{"+paneOptOwner+"} slot=#{"+paneOptSlot+"}")
}

// ---- Never adopt the user's shell ----

// TestUnmarkedIdleUserShellIsNeverAdopted is the product-owner constraint as a
// test: a shell the user opened is the user's, however idle it looks.
//
// The window holds the agent's pane and one more idle zsh, opened with raw tmux
// so it carries no @mcp_* option at all. That is also what a pane restored by
// tmux-resurrect looks like, and what a released helper looks like. Asking for
// slot 1 must open a new pane: the user's pane must not be returned, must not be
// marked, must not change size, and must not receive the keys.
//
// REGRESSION: an unmarked idle user shell in the window was adopted as slot 1 and typed into
func TestUnmarkedIdleUserShellIsNeverAdopted(t *testing.T) {
	c, self := agentPaneFixture(t)
	requireZshPanes(t)

	users := rawSplit(t, self, "-h")
	waitForIdleZsh(t, users)
	before := paneRect(t, users)
	windowID := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")
	countBefore := len(panesInWindow(t, windowID))

	keys := fmt.Sprintf("echo NEVER-ADOPTED-%d", time.Now().UnixNano())
	var out map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": keys, "enter": true}, &out)
	time.Sleep(500 * time.Millisecond)

	layout := windowLayout(t, self)
	if marks := mcpMarks(t, users); marks != [3]string{} {
		t.Errorf("the user's pane %s now carries %s=%q %s=%q %s=%q; a pane nobody gave us must "+
			"never be marked as ours.\nwindow:\n%s",
			users, paneOptWitness, marks[0], paneOptOwner, marks[1], paneOptSlot, marks[2], layout)
	}
	if got := tmuxExec(t, "capture-pane", "-p", "-t", users); strings.Contains(got, keys) {
		t.Errorf("the keys sent to slot 1 landed in the user's pane %s:\n%s", users, got)
	}
	if after := paneRect(t, users); after != before {
		t.Errorf("the user's pane %s went from %+v to %+v; opening a helper must not rearrange it",
			users, before, after)
	}
	if got := len(panesInWindow(t, windowID)); got != countBefore+1 {
		t.Errorf("the window went from %d panes to %d; slot 1 had to be one NEW pane.\nwindow:\n%s",
			countBefore, got, layout)
	}
	if out["created"] != true {
		t.Errorf("send-keys reported created=%v; slot 1 is a new pane, so it must say so", out["created"])
	}
	if one := slotPaneID(t, self, 1); one == users || one == self {
		t.Errorf("slot 1 is %s, which is the user's pane or the agent's own.\nwindow:\n%s", one, layout)
	}
}

// ---- Reclaim our own idle panes, and nothing else ----

// TestUnslottedAgentPaneIsReclaimed is the pile-up, and the positive twin of
// TestBusyOrTUIPaneIsNeverReclaimed.
//
// An agent-owned pane with no slot is ours and idle, and no request can ever
// reach it: reuse matches on the slot number, and adoption refuses anything with
// an owner mark. So every later request opens one more pane beside it. Slot 1
// must take that pane instead: the window keeps its pane count, the pane now
// carries slot 1, and the keys run there.
//
// REGRESSION: an idle agent-owned pane without a slot was never reused, so every request opened another pane beside it
func TestUnslottedAgentPaneIsReclaimed(t *testing.T) {
	c, self := agentPaneFixture(t)
	requireZshPanes(t)

	ours := rawSplit(t, self, "-h")
	waitForIdleZsh(t, ours)
	markAgentUnslotted(t, ours)
	windowID := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")
	countBefore := len(panesInWindow(t, windowID))

	marker := filepath.Join(t.TempDir(), "reclaimed")
	var out map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo R > " + marker, "enter": true}, &out)

	layout := windowLayout(t, self)
	if got := len(panesInWindow(t, windowID)); got != countBefore {
		t.Errorf("the window went from %d panes to %d: slot 1 opened a new pane beside the idle "+
			"agent-owned pane %s instead of reclaiming it.\nwindow:\n%s", countBefore, got, ours, layout)
	}
	if got := mcpMarks(t, ours)[2]; got != "1" {
		t.Errorf("the reclaimed pane %s carries %s=%q, want \"1\".\nwindow:\n%s", ours, paneOptSlot, got, layout)
	}
	if !waitForFile(marker, 5*time.Second) {
		t.Errorf("the command sent to slot 1 did not run in %s.\nwindow:\n%s", ours, layout)
	}
	// A reclaimed pane is new to the slot, so an agent must not assume a process
	// of its own is running there.
	if out["created"] != true {
		t.Errorf("send-keys reported created=%v for a pane new to slot 1", out["created"])
	}
}

// TestBusyOrTUIPaneIsNeverReclaimed is the negative control for reclaim.
//
// The three panes carry exactly the agent-owned, unslotted mark the reclaimed
// pane above carries, so the mark is not what tells them apart. What does is
// that each is running something: a long sleep, an editor, a program reading
// stdin. Typing a command into any of them would feed it to that program. Slot 1
// must open a new pane and leave their marks and screens as they were.
//
// It passes today, because nothing marked is reused at all. It is here so that
// the reclaim which makes TestUnslottedAgentPaneIsReclaimed pass cannot take a
// pane that is busy.
//
// REGRESSION: guards reclaim against taking an agent-owned pane that is running a program
func TestBusyOrTUIPaneIsNeverReclaimed(t *testing.T) {
	c, self := agentPaneFixture(t)
	requireZshPanes(t)

	type busyPane struct{ id, run, fg string }
	programs := []busyPane{{run: "sleep 300", fg: "sleep"}, {run: "cat", fg: "cat"}}
	if _, err := exec.LookPath("vim"); err == nil {
		programs = append(programs, busyPane{run: "vim -u NONE", fg: "vim"})
	} else {
		t.Log("vim is not installed; covering sleep and cat only")
	}

	// One column on the right, each program in its own row.
	anchor := self
	axis := "-h"
	for i := range programs {
		programs[i].id = rawSplit(t, anchor, axis)
		anchor, axis = programs[i].id, "-v"
		waitForIdleZsh(t, programs[i].id)
		markAgentUnslotted(t, programs[i].id)
		tmuxExec(t, "send-keys", "-t", programs[i].id, programs[i].run, "Enter")
	}
	screens := map[string]string{}
	marks := map[string][3]string{}
	for _, p := range programs {
		waitForForeground(t, p.id, p.fg)
	}
	time.Sleep(300 * time.Millisecond)
	for _, p := range programs {
		screens[p.id] = tmuxExec(t, "capture-pane", "-p", "-t", p.id)
		marks[p.id] = mcpMarks(t, p.id)
	}
	windowID := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")
	countBefore := len(panesInWindow(t, windowID))

	var out map[string]any
	c.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &out)

	layout := windowLayout(t, self)
	one := slotPaneID(t, self, 1)
	for _, p := range programs {
		if one == p.id {
			t.Errorf("slot 1 reclaimed %s, which is running %q.\nwindow:\n%s", p.id, p.run, layout)
		}
		if got := mcpMarks(t, p.id); got != marks[p.id] {
			t.Errorf("the marks on %s (running %q) went from %q to %q", p.id, p.run, marks[p.id], got)
		}
		if got := tmuxExec(t, "capture-pane", "-p", "-t", p.id); got != screens[p.id] {
			t.Errorf("the screen of %s (running %q) changed:\nbefore:\n%s\nafter:\n%s",
				p.id, p.run, screens[p.id], got)
		}
	}
	if got := len(panesInWindow(t, windowID)); got != countBefore+1 {
		t.Errorf("the window went from %d panes to %d; slot 1 had to be one new pane.\nwindow:\n%s",
			countBefore, got, layout)
	}
	for _, p := range programs {
		tmuxExec(t, "send-keys", "-t", p.id, "C-c")
	}
}

// TestSlotOneSurvivesServerRestart keeps what already works: the registry lives
// in tmux, so a second server started in the same pane finds the slot-1 pane the
// first one opened and types into it, without opening another.
//
// REGRESSION: guards slot reuse across a server restart while reuse is widened
func TestSlotOneSurvivesServerRestart(t *testing.T) {
	c, self := agentPaneFixture(t)
	windowID := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	first := openSlot(t, c, self, 1)
	c.close()
	countBefore := len(panesInWindow(t, windowID))

	c2 := newMCPClientInPane(t, self)
	marker := filepath.Join(t.TempDir(), "restart")
	var out map[string]any
	c2.callToolJSON(t, "send-keys", map[string]any{"keys": "echo B > " + marker, "enter": true}, &out)

	if got := len(panesInWindow(t, windowID)); got != countBefore {
		t.Errorf("the second server opened a new pane (%d panes, was %d).\nwindow:\n%s",
			got, countBefore, windowLayout(t, self))
	}
	if again := slotPaneID(t, self, 1); again != first {
		t.Errorf("slot 1 moved from %s to %s across the restart", first, again)
	}
	if out["created"] != false {
		t.Errorf("the second server reported created=%v for the pane the first one opened", out["created"])
	}
	if !waitForFile(marker, 5*time.Second) {
		t.Errorf("the command sent by the second server did not run in %s", first)
	}
}
