package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const gridUserReason = "the window holds a pane the user opened, so no pane was moved or resized"

func gridJSONBody(t *testing.T, result callToolResult, tool string) map[string]any {
	t.Helper()
	if result.IsError {
		t.Fatalf("%s failed: %s", tool, result.text(t, tool))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(result.text(t, tool)), &body); err != nil {
		t.Fatalf("decode %s JSON response: %v", tool, err)
	}
	// if a client chooses structuredContent, no JSON information is lost.
	if result.StructuredContent != nil && !reflect.DeepEqual(result.StructuredContent, body) {
		t.Errorf("%s structuredContent=%#v, text JSON=%#v", tool, result.StructuredContent, body)
	}
	return body
}

func gridOpen(t *testing.T, c *mcpClient, args map[string]any) map[string]any {
	t.Helper()
	body := gridJSONBody(t, callTool(t, c, "open-pane", args), "open-pane")
	mode, ok := body["layout"].(string)
	if !ok || (mode != "default" && mode != "agent-grid") {
		t.Fatalf("open-pane must always report the window mode, got %#v", body)
	}
	applied, ok := body["layoutApplied"].(bool)
	if !ok {
		t.Fatalf("open-pane must always report layoutApplied, including false: %#v", body)
	}
	detail, present := body["layoutDetail"]
	if applied && present {
		t.Errorf("applied layout carries layoutDetail: %#v", body)
	}
	if !applied {
		text, ok := detail.(string)
		if !present || !ok || text == "" {
			t.Fatalf("unapplied layout lacks a reason: %#v", body)
		}
		if idInText.MatchString(text) {
			t.Errorf("layoutDetail leaks an id: %q", text)
		}
	}
	return body
}

func gridOutcome(t *testing.T, body map[string]any, mode string, applied bool, detail string) {
	t.Helper()
	if body["layout"] != mode || body["layoutApplied"] != applied {
		t.Errorf("layout outcome=%#v, want mode=%q applied=%v", body, mode, applied)
	}
	if detail != "" && body["layoutDetail"] != detail {
		t.Errorf("layoutDetail=%q, want exactly %q", body["layoutDetail"], detail)
	}
}

func gridTmuxDimensions(t *testing.T, self string) (int, int) {
	t.Helper()
	var w, h int
	raw := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_width} #{window_height}")
	if _, err := fmt.Sscan(raw, &w, &h); err != nil {
		t.Fatalf("decode window dimensions %q: %v", raw, err)
	}
	return w, h
}

func gridTmuxRects(t *testing.T, self string) map[string]cellRect {
	t.Helper()
	raw := tmuxExec(t, "list-panes", "-t", self, "-F",
		"#{pane_id} #{pane_left} #{pane_top} #{pane_width} #{pane_height}")
	out := map[string]cellRect{}
	for _, line := range strings.Split(raw, "\n") {
		var id string
		var r cellRect
		if _, err := fmt.Sscan(line, &id, &r.Left, &r.Top, &r.Width, &r.Height); err != nil {
			t.Fatalf("decode list-panes geometry %q: %v", line, err)
		}
		out[id] = r
	}
	return out
}

func gridTmuxFocus(t *testing.T, self string) string {
	t.Helper()
	raw := tmuxExec(t, "list-panes", "-t", self, "-F", "#{pane_id} #{pane_active}")
	for _, line := range strings.Split(raw, "\n") {
		var id string
		var active int
		if _, err := fmt.Sscan(line, &id, &active); err != nil {
			t.Fatalf("decode active pane %q: %v", line, err)
		}
		if active == 1 {
			return id
		}
	}
	t.Fatalf("no active pane in the agent window: %s", raw)
	return ""
}

func gridAssertTmuxGrid(t *testing.T, self string, slots []int) {
	t.Helper()
	ordered := append([]int(nil), slots...)
	sort.Ints(ordered)
	w, h := gridTmuxDimensions(t, self)
	agent, cells := gridExpected(w, h, 1, len(ordered))
	actual := gridTmuxRects(t, self)
	if len(actual) != len(ordered)+1 {
		t.Fatalf("window has %d panes, want agent plus %d slots:\n%s",
			len(actual), len(ordered), windowLayout(t, self))
	}
	gridAssertRect(t, "tmux agent", actual[self], agent, 1)
	for i, slot := range ordered {
		id := slotPaneID(t, self, slot)
		got, ok := actual[id]
		if !ok {
			t.Fatalf("slot %d's pane was absent from the list-panes snapshot", slot)
		}
		gridAssertRect(t, fmt.Sprintf("tmux slot %d", slot), got, cells[i], 1)
	}
}

func gridCheckFocus(t *testing.T, self, want string) {
	t.Helper()
	if got := gridTmuxFocus(t, self); got != want {
		t.Errorf("active pane moved from %s to %s", want, got)
	}
}

// measured after every open, not only at the final count.
func TestGridTmuxLifecycle(t *testing.T) {
	c, self := agentPaneFixture(t)
	focus := gridTmuxFocus(t, self)
	var slots []int
	for slot := 1; slot <= 11; slot++ {
		out := gridOpen(t, c, map[string]any{"slot": slot, "layout": "agent-grid"})
		gridOutcome(t, out, "agent-grid", true, "")
		if out["created"] != true {
			t.Errorf("first open of slot %d did not create it: %#v", slot, out)
		}
		slots = append(slots, slot)
		gridAssertTmuxGrid(t, self, slots)
		gridCheckFocus(t, self, focus)
	}
	entries := closedEntries(t, c, map[string]any{"slot": 5})
	if len(entries) != 1 || entries[0]["action"] != "killed" {
		t.Fatalf("middle close=%v, want one killed slot", entries)
	}
	slots = []int{1, 2, 3, 4, 6, 7, 8, 9, 10, 11}
	gridAssertTmuxGrid(t, self, slots)
	gridCheckFocus(t, self, focus)

	all := closedEntries(t, c, map[string]any{"slot": "all"})
	if len(all) != len(slots) {
		t.Errorf("close all reported %d entries, want %d", len(all), len(slots))
	}
	for _, entry := range all {
		if entry["action"] != "killed" {
			t.Errorf("close all did not kill an owned slot: %v", entry)
		}
	}
	gridAssertTmuxGrid(t, self, nil)
	gridCheckFocus(t, self, focus)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1}), "default", true, "")
}

func TestGridTmuxSparseOrder(t *testing.T) {
	c, self := agentPaneFixture(t)
	var slots []int
	for _, slot := range []int{11, 2, 7, 1, 5} {
		gridOutcome(t, gridOpen(t, c, map[string]any{"slot": slot, "layout": "agent-grid"}),
			"agent-grid", true, "")
		slots = append(slots, slot)
		gridAssertTmuxGrid(t, self, slots)
	}
}

func TestGridTmuxReuseAndDefault(t *testing.T) {
	c, self := agentPaneFixture(t)
	for slot := 1; slot <= 4; slot++ {
		args := map[string]any{"slot": slot}
		if slot == 1 {
			args["layout"] = "agent-grid"
		}
		gridOutcome(t, gridOpen(t, c, args), "agent-grid", true, "")
	}
	tmuxExec(t, "select-layout", "-t", self, "even-horizontal")
	perturbed := gridTmuxRects(t, self)
	out := gridOpen(t, c, map[string]any{"slot": 2})
	gridOutcome(t, out, "agent-grid", true, "")
	if out["created"] != false {
		t.Errorf("reuse reported created=%v", out["created"])
	}
	gridAssertTmuxGrid(t, self, []int{1, 2, 3, 4})
	if reflect.DeepEqual(perturbed, gridTmuxRects(t, self)) {
		t.Error("the controlled non-grid layout was not repaired on reuse")
	}
	beforeDefault := gridTmuxRects(t, self)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 2, "layout": "default"}), "default", true, "")
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 3}), "default", true, "")
	if after := gridTmuxRects(t, self); !reflect.DeepEqual(after, beforeDefault) {
		t.Errorf("default/omission rearranged panes: before=%v after=%v", beforeDefault, after)
	}
}

func TestGridTmuxDefaultPlacement(t *testing.T) {
	c, self := agentPaneFixture(t)
	ids := map[int]string{}
	for slot := 1; slot <= 3; slot++ {
		gridOutcome(t, gridOpen(t, c, map[string]any{"slot": slot}), "default", true, "")
		ids[slot] = slotPaneID(t, self, slot)
	}
	before := gridTmuxRects(t, self)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 4}), "default", true, "")
	four := paneRect(t, slotPaneID(t, self, 4))
	agent, three := paneRect(t, self), paneRect(t, ids[3])
	if three.Left != agent.Left || three.Top <= agent.Top {
		t.Errorf("never-enabled slot 3 must remain under the agent after slot 4: agent=%+v slot3=%+v", agent, three)
	}
	largest, anchorArea := 0, -1
	for _, id := range ids {
		r := before[id]
		if r.area() > largest {
			largest = r.area()
		}
		if r.contains(four) {
			anchorArea = r.area()
		}
	}
	if anchorArea != largest {
		t.Errorf("default slot 4 at %+v was not split from a largest helper: before=%v", four, before)
	}
}

func TestGridTmuxRestartAndWindowScope(t *testing.T) {
	c, self := agentPaneFixtureNamed(t, "enabled")
	other, otherSelf := agentPaneFixtureNamed(t, "default")
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"}), "agent-grid", true, "")
	c.close()
	restarted := newMCPClientInPane(t, self)
	gridOutcome(t, gridOpen(t, restarted, map[string]any{"slot": 2}), "agent-grid", true, "")
	gridAssertTmuxGrid(t, self, []int{1, 2})
	gridOutcome(t, gridOpen(t, other, map[string]any{"slot": 1}), "default", true, "")
	otherAgent, otherSlot := paneRect(t, otherSelf), paneRect(t, slotPaneID(t, otherSelf, 1))
	if otherSlot.Left <= otherAgent.Left || otherSlot.Top != otherAgent.Top {
		t.Errorf("unrelated window lost default placement: agent=%+v slot=%+v", otherAgent, otherSlot)
	}
}

func TestGridTmuxAgentScope(t *testing.T) {
	a, selfA := agentPaneFixture(t)
	selfB := rawSplit(t, selfA, "-h")
	b := newMCPClientInPane(t, selfB)
	for _, tc := range []struct {
		c    *mcpClient
		slot int
	}{{a, 1}, {b, 2}} {
		out := gridOpen(t, tc.c, map[string]any{"slot": tc.slot, "layout": "agent-grid"})
		gridOutcome(t, out, "agent-grid", false, "")
	}
	before := gridTmuxRects(t, selfA)
	gridOutcome(t, gridOpen(t, a, map[string]any{"slot": 1, "layout": "default"}), "default", true, "")
	gridOutcome(t, gridOpen(t, b, map[string]any{"slot": 2}), "agent-grid", false, "")
	gridOutcome(t, gridOpen(t, a, map[string]any{"slot": 1}), "default", true, "")
	if after := gridTmuxRects(t, selfA); !reflect.DeepEqual(before, after) {
		t.Errorf("foreign-pane/default reuse changed geometry: before=%v after=%v", before, after)
	}
}

func TestGridTmuxIsolatedRefusal(t *testing.T) {
	for _, mode := range []string{"agent-grid", "default"} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%v", mode, existing), func(t *testing.T) {
				c, self := agentPaneFixture(t)
				t.Cleanup(func() { closedEntries(t, c, map[string]any{"slot": "all"}) })
				if existing {
					out := gridOpen(t, c, map[string]any{"slot": 6, "isolated": true})
					gridOutcome(t, out, "default", true, "")
					if out["isolated"] != true {
						t.Fatal("fixture did not create an isolated slot")
					}
				}
				visible, isolated := gridTmuxRects(t, self), isolatedPaneIDs(t)
				args := map[string]any{"slot": 6, "layout": mode}
				if !existing {
					args["isolated"] = true
				}
				res := callTool(t, c, "open-pane", args)
				if !res.IsError || res.text(t, "open-pane") != "layout applies to visible slots" {
					t.Fatalf("isolated layout refusal=%+v, want exact visible-slots error", res)
				}
				if after := gridTmuxRects(t, self); !reflect.DeepEqual(visible, after) {
					t.Errorf("refused request changed visible panes: %v -> %v", visible, after)
				}
				if after := isolatedPaneIDs(t); !reflect.DeepEqual(isolated, after) {
					t.Errorf("refused request changed isolated panes: %v -> %v", isolated, after)
				}
				if existing {
					out := gridOpen(t, c, map[string]any{"slot": 6})
					gridOutcome(t, out, "default", true, "")
					if out["created"] != false || out["isolated"] != true {
						t.Errorf("refused layout changed the existing isolated slot: %#v", out)
					}
				}
			})
		}
	}
}

func TestGridTmuxInvalidLayout(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		value      any
	}{
		{"word", "layout must be \"agent-grid\" or \"default\", got \"grid\"", "grid"},
		{"bool", "layout must be \"agent-grid\" or \"default\", got true", true},
		{"number", "layout must be \"agent-grid\" or \"default\", got 7", float64(7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, self := agentPaneFixture(t)
			before := gridTmuxRects(t, self)
			res := callTool(t, c, "open-pane", map[string]any{"slot": 1, "layout": tc.value})
			if !res.IsError || res.text(t, "open-pane") != tc.want {
				t.Errorf("invalid layout=%+v, want error %q", res, tc.want)
			}
			if after := gridTmuxRects(t, self); !reflect.DeepEqual(before, after) {
				t.Errorf("invalid layout created or rearranged panes: %v -> %v", before, after)
			}
			gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1}), "default", true, "")
		})
	}
}

var gridOtherCreators = []string{"send-keys", "execute-command", "start-and-watch", "run-in-repl", "write-to-display"}

func gridCreatorArgs(tool string, slot int, marker string) map[string]any {
	command := "printf '" + marker + "\\n'"
	args := map[string]any{"slot": slot}
	switch tool {
	case "send-keys":
		args["keys"], args["enter"] = command, true
	case "execute-command":
		args["command"], args["timeoutSeconds"] = command, 12
	case "start-and-watch":
		args["command"], args["pattern"], args["timeout"] = command, marker, 10
	case "run-in-repl":
		args["input"], args["promptPattern"], args["timeout"] = command, "\\$|>|#|%", 5
	case "write-to-display":
		args["text"] = "\n" + marker + "\n"
	default:
		panic("unknown test creator: " + tool)
	}
	return args
}

func gridCallCreator(t *testing.T, c *mcpClient, tool string, slot int) {
	t.Helper()
	out := gridJSONBody(t, callTool(t, c, tool, gridCreatorArgs(tool, slot, fmt.Sprintf("GS%02d", slot))), tool)
	if out["created"] != true || out["slot"] != float64(slot) {
		t.Errorf("%s did not create slot %d: %#v", tool, slot, out)
	}
	if out["timedOut"] == true {
		t.Errorf("%s timed out while delivering its first command: %#v", tool, out)
	}
}

func TestGridTmuxCreators(t *testing.T) {
	for _, tool := range gridOtherCreators {
		t.Run(tool, func(t *testing.T) {
			c, self := agentPaneFixture(t)
			gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"}), "agent-grid", true, "")
			focus := gridTmuxFocus(t, self)
			for slot := 2; slot <= 4; slot++ {
				gridCallCreator(t, c, tool, slot)
				gridAssertTmuxGrid(t, self, gridIntRange(1, slot))
				gridCheckFocus(t, self, focus)
			}
		})
	}
}

func gridIntRange(first, last int) []int {
	var out []int
	for n := first; n <= last; n++ {
		out = append(out, n)
	}
	return out
}

func TestGridTmuxFirstCommandSize(t *testing.T) {
	c, self := agentPaneFixture(t)
	for slot := 1; slot <= 3; slot++ {
		gridOpen(t, c, map[string]any{"slot": slot, "layout": "agent-grid"})
	}
	path := filepath.Join(t.TempDir(), "size")
	quoted := "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
	out := gridJSONBody(t, callTool(t, c, "execute-command", map[string]any{
		"slot": 4, "command": "stty size > " + quoted, "timeoutSeconds": 12,
	}), "execute-command")
	if out["timedOut"] == true || out["exitCode"] != float64(0) {
		t.Fatalf("first command failed: %#v", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("first command did not write its dimensions: %v", err)
	}
	var height, width int
	if _, err := fmt.Sscan(string(data), &height, &width); err != nil {
		t.Fatalf("stty size returned %q: %v", data, err)
	}
	final := paneRect(t, slotPaneID(t, self, 4))
	if width != final.Width || height != final.Height {
		t.Errorf("first delivered command saw %dx%d, want final measured terminal %dx%d",
			width, height, final.Width, final.Height)
	}
	gridAssertTmuxGrid(t, self, []int{1, 2, 3, 4})
}

func TestGridOtherToolsIgnoreLayout(t *testing.T) {
	for _, tool := range []string{
		"send-keys", "execute-command", "start-and-watch", "run-in-repl", "write-to-display",
		"capture-pane", "screenshot-pane", "pane-state", "watch-pane", "list-slots", "close-pane", "notify",
	} {
		t.Run(tool, func(t *testing.T) {
			c, self := agentPaneFixture(t)
			openSlot(t, c, self, 1)
			args := map[string]any{"layout": "not-a-mode"}
			switch tool {
			case "send-keys", "execute-command", "start-and-watch", "run-in-repl", "write-to-display":
				args = gridCreatorArgs(tool, 1, "IGNORED")
				args["layout"] = "not-a-mode"
			case "screenshot-pane":
				args["output"] = "html"
			case "watch-pane":
				args["triggers"], args["timeout"] = "pattern:never-grid-probe", 1
			case "notify":
				args["message"], args["duration"] = "layout argument ignored", 1
			}
			if res := callTool(t, c, tool, args); res.IsError {
				t.Fatalf("%s refused an irrelevant layout: %s", tool, res.text(t, tool))
			}
			gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1}), "default", true, "")
		})
	}
}

func TestGridTmuxForeignPanes(t *testing.T) {
	for _, kind := range []string{"user", "acquired", "unstamped", "child agent"} {
		t.Run(kind, func(t *testing.T) {
			c, self := agentPaneFixture(t)
			one := openSlot(t, c, self, 1)
			var foreign string
			reason := gridUserReason
			switch kind {
			case "child agent":
				child := newMCPClientInPane(t, one)
				foreign = openSlot(t, child, one, 2)
				reason = "the window holds another agent's helper pane, so no pane was moved or resized"
			default:
				foreign = rawSplit(t, self, "-h")
				if kind == "acquired" {
					markLegacyAcquired(t, foreign, 2)
					reason = "the window holds a pane the user opened that an earlier version took over as a slot, so no pane was moved or resized"
				}
				if kind == "unstamped" {
					markAgentUnslotted(t, foreign)
					tmuxExec(t, "set-option", "-p", "-t", foreign, paneOptSlot, "2")
					reason = "the window holds a helper pane that does not record which agent opened it, so no pane was moved or resized"
				}
			}
			before := gridTmuxRects(t, self)
			gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"}), "agent-grid", false, reason)
			gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1}), "agent-grid", false, reason)
			if after := gridTmuxRects(t, self); !reflect.DeepEqual(before, after) {
				t.Errorf("refused cosmetic layout changed panes: before=%v after=%v", before, after)
			}
			if kind == "user" {
				gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 3}), "agent-grid", false, reason)
				if got := paneRect(t, foreign); got != before[foreign] {
					t.Errorf("new slot changed user pane: got %+v, want %+v", got, before[foreign])
				}
			}
		})
	}
}

func TestGridTmuxZoomed(t *testing.T) {
	c, self := agentPaneFixture(t)
	openSlot(t, c, self, 1)
	tmuxExec(t, "resize-pane", "-Z", "-t", self)
	before := gridTmuxRects(t, self)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"}),
		"agent-grid", false, "the window is zoomed, so no pane was moved or resized")
	if after := gridTmuxRects(t, self); !reflect.DeepEqual(before, after) {
		t.Errorf("zoom refusal rearranged panes: %v -> %v", before, after)
	}
	if zoom := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_zoomed_flag}"); zoom != "1" {
		t.Errorf("layout request unzoomed the window: zoom=%q", zoom)
	}
}

func TestGridTmuxSmallWindows(t *testing.T) {
	t.Run("cramped new slot still opens", func(t *testing.T) {
		c, self := agentPaneFixture(t)
		tmuxExec(t, "resize-window", "-t", self, "-x", "19", "-y", "5")
		w, h := gridTmuxDimensions(t, self)
		if w != 19 || h != 5 {
			t.Fatalf("small-window fixture is %dx%d, want 19x5", w, h)
		}
		out := gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"})
		gridOutcome(t, out, "agent-grid", false,
			"the window is 19x5 cells, too small for 1 slot in the grid; it was applied as far as it fits")
		if out["created"] != true {
			t.Errorf("cramped but split-able slot did not open: %#v", out)
		}
		gridAssertTmuxGrid(t, self, []int{1})
	})
	t.Run("impossible grid leaves existing stack alone", func(t *testing.T) {
		c, self := agentPaneFixture(t)
		for slot := 1; slot <= 4; slot++ {
			openSlot(t, c, self, slot)
		}
		tmuxExec(t, "select-layout", "-t", self, "even-vertical")
		tmuxExec(t, "resize-window", "-t", self, "-x", "4", "-y", "15")
		w, h := gridTmuxDimensions(t, self)
		if w != 4 || h != 15 {
			t.Fatalf("impossible-grid fixture is %dx%d, want 4x15", w, h)
		}
		before := gridTmuxRects(t, self)
		out := gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"})
		gridOutcome(t, out, "agent-grid", false,
			"the window is 4x15 cells and cannot hold 4 slots in the grid, so the panes were left where they are")
		if after := gridTmuxRects(t, self); !reflect.DeepEqual(before, after) {
			t.Errorf("impossible grid changed panes: %v -> %v", before, after)
		}
	})
}

func TestGridTmuxDeadAndGone(t *testing.T) {
	c, self := agentPaneFixture(t)
	for slot := 1; slot <= 3; slot++ {
		gridOpen(t, c, map[string]any{"slot": slot, "layout": "agent-grid"})
	}
	tmuxExec(t, "kill-pane", "-t", slotPaneID(t, self, 2))
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 4}), "agent-grid", true, "")
	gridAssertTmuxGrid(t, self, []int{1, 3, 4})

	dead := slotPaneID(t, self, 1)
	tmuxExec(t, "set-option", "-t", dead, "remain-on-exit", "on")
	tmuxExec(t, "send-keys", "-t", dead, "exit", "Enter")
	waitForPaneDead(t, dead)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 5}), "agent-grid", true, "")
	gridAssertTmuxGrid(t, self, []int{1, 3, 4, 5})
	for _, tc := range []struct {
		close int
		left  []int
	}{{3, []int{1, 4, 5}}, {1, []int{4, 5}}, {4, []int{5}}, {5, nil}} {
		entries := closedEntries(t, c, map[string]any{"slot": tc.close})
		if len(entries) != 1 || entries[0]["action"] != "killed" {
			t.Fatalf("close slot %d=%v, want killed", tc.close, entries)
		}
		gridAssertTmuxGrid(t, self, tc.left)
	}
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 2}), "default", true, "")
}

func TestGridTmuxBorderStatus(t *testing.T) {
	for _, position := range []string{"top", "bottom"} {
		t.Run(position, func(t *testing.T) {
			c, self := agentPaneFixture(t)
			tmuxExec(t, "set-option", "-w", "-t", self, "pane-border-status", position)
			for slot := 1; slot <= 4; slot++ {
				gridOutcome(t, gridOpen(t, c, map[string]any{"slot": slot, "layout": "agent-grid"}), "agent-grid", true, "")
				// The status row can displace a content edge by one cell, which is
				// already the contracted per-boundary tmux tolerance.
				gridAssertTmuxGrid(t, self, gridIntRange(1, slot))
			}
		})
	}
}
