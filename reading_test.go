package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// This file covers the one change in this release that makes a previously
// working call FAIL: the four reading tools no longer create a pane.
//
// capture-pane({slot:3}) on an empty slot used to SPLIT the user's window, or
// ADOPT one of their idle shells — writing three tmux options into a pane they
// opened, and renaming it — in order to answer a question about a pane that did
// not exist. The answer was a screenshot of a fresh prompt, which tells the
// caller nothing, and the cost was a pane the user has to close.

// readingTools is the set, with arguments that would SUCCEED if the tool still
// resolved: every required field present, and every wait short enough that a
// regression is a failure rather than a timeout.
var readingTools = []struct {
	tool string
	args map[string]any
}{
	{"capture-pane", map[string]any{}},
	{"screenshot-pane", map[string]any{"output": "html"}},
	{"pane-state", map[string]any{}},
	{"watch-pane", map[string]any{"triggers": "pattern:never-xyzzy", "timeout": 2}},
}

// argsForSlot copies a reading tool's arguments and points them at one slot.
func argsForSlot(args map[string]any, slot int) map[string]any {
	out := map[string]any{"slot": slot}
	for k, v := range args {
		out[k] = v
	}
	return out
}

// TestReadingToolsDoNotCreate is the behaviour change, stated as a test.
//
// # The control, which is the whole test
//
// "The pane count did not change" proves nothing on its own: a probe that cannot
// observe ANY creation reports unchanged for every input, and would go on
// reporting it while all four tools split the user's window. So the same probe
// is run around a send-keys — a CREATING tool, on the same empty slot — and the
// count must rise by exactly one. Only then does the reading half mean anything.
//
// The second half of the control is that the reads then SUCCEED against that
// same slot, which is what distinguishes "reads do not create" from "reads are
// broken".
func TestReadingToolsDoNotCreate(t *testing.T) {
	c, self := agentPaneFixture(t)
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")
	before := panesInWindow(t, window)

	const empty = 5
	wantMessage := fmt.Sprintf(missingSlotText, empty)

	for _, tc := range readingTools {
		res := callTool(t, c, tc.tool, argsForSlot(tc.args, empty))
		if !res.IsError {
			t.Errorf("%s answered about slot %d, which was never opened: a read that resolves "+
				"splits the user's window to answer a question about a pane that does not exist",
				tc.tool, empty)
			continue
		}
		if got := res.text(t, tc.tool); got != wantMessage {
			t.Errorf("%s refused with %q, want exactly %q — the sentence names both ways to make "+
				"a slot, because an agent told only \"does not exist\" goes looking for another "+
				"route into the terminal", tc.tool, got, wantMessage)
		}
	}

	if after := panesInWindow(t, window); len(after) != len(before) {
		t.Fatalf("the window went from %d panes to %d (%v → %v): a reading tool created one",
			len(before), len(after), before, after)
	}

	// The control. A creating tool on the same slot must move the probe.
	c.callToolJSON(t, "send-keys", map[string]any{
		"slot": empty, "keys": "echo reading-probe", "enter": true,
	}, &map[string]any{})

	after := panesInWindow(t, window)
	if len(after) != len(before)+1 {
		t.Fatalf("send-keys on the same empty slot took the window from %d panes to %d, want %d: "+
			"the pane-count probe cannot observe a creation, so \"unchanged\" above is vacuous",
			len(before), len(after), len(before)+1)
	}

	// And now every read succeeds against it, which is what tells "reads do not
	// create" apart from "reads do not work".
	for _, tc := range readingTools {
		if res := callTool(t, c, tc.tool, argsForSlot(tc.args, empty)); res.IsError {
			t.Errorf("%s failed on a slot that now exists: %s", tc.tool, res.text(t, tc.tool))
		}
	}
	if final := panesInWindow(t, window); len(final) != len(after) {
		t.Errorf("the four reads took the window from %d panes to %d", len(after), len(final))
	}
}

// TestReadingToolsFindIsolatedSlots is the half a visible-only lookup would get
// wrong, and get wrong silently.
//
// The lookup behind the reading tools reads BOTH registries, because close-pane
// and the reading tools have to agree about what "slot 6" means. A lookup that
// read only the user's window would answer "slot 6 does not exist" for a slot
// that very much does — a lie, and one that sends an agent to re-create a pane
// it already has, in a window where it would then be visible.
func TestReadingToolsFindIsolatedSlots(t *testing.T) {
	c, self := agentPaneFixture(t)
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	const slot = 6
	opened := openedSlot(t, c, map[string]any{"slot": slot, "isolated": true})
	if !opened.Isolated {
		t.Fatalf("the fixture did not open an isolated slot: %+v", opened)
	}
	before := panesInWindow(t, window)

	c.callToolJSON(t, "send-keys", map[string]any{
		"slot": slot, "keys": "echo ISOLATED-READ-MARKER", "enter": true,
	}, &map[string]any{})
	sleep(600 * time.Millisecond)

	// Every read below OMITS isolated — reading tools have no such argument, and
	// this is the only way they could ever reach an invisible pane.
	captured := c.callToolText(t, "capture-pane", map[string]any{"slot": slot})
	if !strings.Contains(captured, "ISOLATED-READ-MARKER") {
		t.Errorf("capture-pane did not read the isolated slot:\n%s", captured)
	}
	for _, tc := range readingTools {
		if res := callTool(t, c, tc.tool, argsForSlot(tc.args, slot)); res.IsError {
			t.Errorf("%s could not read isolated slot %d: %s", tc.tool, slot, res.text(t, tc.tool))
		}
	}
	if after := panesInWindow(t, window); len(after) != len(before) {
		t.Errorf("reading an isolated slot took the user's window from %d panes to %d: the lookup "+
			"missed the isolated registry and made a visible pane instead", len(before), len(after))
	}
}

// TestOutsideTmuxIsolatedSlotsWork covers the mode a server outside tmux has,
// which is now a working mode rather than a broken one.
//
// Every path that needs the user's window SKIPS that half when there is none,
// rather than refusing the call: there is no window to enumerate, but a server
// that was never inside tmux still has isolated panes, and they are the only
// panes it can have.
//
// # The control
//
// list-slots must RETURN the isolated slot rather than erroring. That is what
// proves the visible half was skipped, as opposed to the whole call failing on a
// server with no window — and the two are indistinguishable from any assertion
// that only checks for an error.
func TestOutsideTmuxIsolatedSlotsWork(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	// No TMUX_PANE: isolateTmux clears it for the whole suite, so this client's
	// server genuinely believes it is not inside tmux.
	c := newMCPClient(t)

	opened := openedSlot(t, c, map[string]any{"slot": 1, "isolated": true})
	if !opened.created() || !opened.Isolated {
		t.Fatalf("a server outside tmux could not open an isolated slot: %+v", opened)
	}

	c.callToolJSON(t, "send-keys", map[string]any{
		"slot": 1, "keys": "echo OUTSIDE-MARKER", "enter": true,
	}, &map[string]any{})
	sleep(600 * time.Millisecond)

	if captured := c.callToolText(t, "capture-pane", map[string]any{"slot": 1}); !strings.Contains(captured, "OUTSIDE-MARKER") {
		t.Errorf("capture-pane outside tmux did not read the isolated slot:\n%s", captured)
	}
	var state map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"slot": 1}, &state)
	if state["isAlive"] != true {
		t.Errorf("pane-state outside tmux reports isAlive=%v for a live isolated slot", state["isAlive"])
	}

	// The control.
	listed := listedSlots(t, c)
	if len(listed) != 1 {
		t.Fatalf("list-slots outside tmux returned %v, want exactly the one isolated slot: the "+
			"visible half has to be SKIPPED, and a call that failed outright would be "+
			"indistinguishable from one that found nothing", listed)
	}
	if entry := listed[1]; !entry.Isolated {
		t.Errorf("list-slots reports slot 1 as visible on a server with no window: %+v", entry)
	}

	// The visible half is not silently satisfied either. A request that needs the
	// user's window says so, and names the route that works.
	res := callTool(t, c, "open-pane", map[string]any{"slot": 2})
	if !res.IsError {
		t.Fatalf("open-pane made a VISIBLE pane on a server with no window: %v", res)
	}
	if got := res.text(t, "open-pane"); got != errNoWindowText {
		t.Errorf("the refusal reads %q, want exactly %q", got, errNoWindowText)
	}

	entries := closedSlots(t, c, map[string]any{"slot": "all"})
	if len(entries) != 1 || entries[0].Action != actionKilled {
		t.Fatalf("close-pane({slot:\"all\"}) outside tmux answered %+v, want one killed entry: "+
			"refusing here would leave a live shell nobody can see and no window to close it in",
			entries)
	}
	if listed := listedSlots(t, c); len(listed) != 0 {
		t.Errorf("list-slots still reports %v after the sweep", listed)
	}
}
