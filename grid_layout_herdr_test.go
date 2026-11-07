package main

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func gridHerdrSize(t *testing.T, h *herdrSession, requiredSlots int) herdrRect {
	t.Helper()
	initial := h.layout(t)
	if len(initial) != 1 {
		t.Fatalf("new herdr tab has %d panes, want just the agent: %v", len(initial), initial)
	}
	r, ok := initial[h.agent]
	if !ok || r.X != 0 || r.Y != 0 || r.Width < 1 || r.Height < 1 {
		t.Fatalf("herdr fixture has no full-tab agent rectangle: %v", initial)
	}
	_, cells := gridExpected(r.Width, r.Height, 0, requiredSlots)
	for _, cell := range cells {
		if cell.Width < 1 || cell.Height < 1 {
			t.Skipf("herdr tab is %dx%d and cannot hold %d grid slots; no tab-size setter is declared",
				r.Width, r.Height, requiredSlots)
		}
	}
	return r
}

func gridHerdrOutcome(t *testing.T, body map[string]any, size herdrRect, n int) {
	t.Helper()
	_, cells := gridExpected(size.Width, size.Height, 0, n)
	if gridReadable(cells) {
		gridOutcome(t, body, "agent-grid", true, "")
		return
	}
	slots := fmt.Sprintf("%d slots", n)
	if n == 1 {
		slots = "1 slot"
	}
	gridOutcome(t, body, "agent-grid", false, fmt.Sprintf(
		"the window is %dx%d cells, too small for %s in the grid; it was applied as far as it fits",
		size.Width, size.Height, slots))
}

func gridHerdrMark(t *testing.T, c *mcpClient, slot int) {
	t.Helper()
	body := gridJSONBody(t, callTool(t, c, "execute-command", map[string]any{
		"slot": slot, "command": fmt.Sprintf("printf 'GS%02d\\n'", slot), "timeoutSeconds": 12,
	}), "execute-command")
	if body["created"] != false || body["timedOut"] == true || body["exitCode"] != float64(0) {
		t.Fatalf("could not mark the existing herdr slot %d: %#v", slot, body)
	}
}

// A rebuild may give a terminal another pane identity. Read persistent screen
// markers instead of assuming ids survive move/swap. Only a just-opened,
// not-yet-marked slot may be inferred as the single remaining pane.
func gridHerdrSlotRects(t *testing.T, h *herdrSession, slots []int, unmarked int, displaySlots ...int) map[int]cellRect {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		actual := h.layout(t)
		if len(actual) != len(slots)+1 {
			t.Fatalf("herdr tab has %d panes, want agent plus %d slots: %v", len(actual), len(slots), actual)
		}
		result := map[int]cellRect{}
		used := map[string]bool{h.agent: true}
		for id, r := range actual {
			if id == h.agent {
				continue
			}
			screen := h.screen(t, id)
			for _, slot := range slots {
				marker := fmt.Sprintf("GS%02d", slot)
				found := false
				// Display text need only be visible; it can share a line with a
				// prompt or be echoed as an unsubmitted multiline paste.
				for _, displaySlot := range displaySlots {
					if slot == displaySlot && strings.Contains(flattenPane(screen), marker) {
						found = true
						break
					}
				}
				for _, line := range strings.Split(screen, "\n") {
					if strings.TrimSpace(line) == marker {
						found = true
						break
					}
				}
				if found {
					if _, duplicate := result[slot]; duplicate {
						t.Fatalf("marker %q appeared in two panes; slot identity probe is ambiguous", marker)
					}
					result[slot] = cellRect{Left: r.X, Top: r.Y, Width: r.Width, Height: r.Height}
					used[id] = true
				}
			}
		}
		if unmarked != 0 {
			if _, already := result[unmarked]; !already {
				var candidates []string
				for id := range actual {
					if !used[id] {
						candidates = append(candidates, id)
					}
				}
				if len(candidates) == 1 && len(result) == len(slots)-1 {
					r := actual[candidates[0]]
					result[unmarked] = cellRect{Left: r.X, Top: r.Y, Width: r.Width, Height: r.Height}
				}
			}
		}
		if len(result) == len(slots) {
			agent, ok := actual[h.agent]
			if !ok {
				t.Fatalf("agent is absent from the herdr layout: %v", actual)
			}
			result[0] = cellRect{Left: agent.X, Top: agent.Y, Width: agent.Width, Height: agent.Height}
			return result
		}
		if time.Now().After(deadline) {
			t.Fatalf("herdr slot markers did not appear; found %v, want slots %v, layout %v", result, slots, actual)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func gridAssertHerdrGrid(t *testing.T, h *herdrSession, size herdrRect, slots []int, unmarked int, displaySlots ...int) {
	t.Helper()
	ordered := append([]int(nil), slots...)
	sort.Ints(ordered)
	agent, cells := gridExpected(size.Width, size.Height, 0, len(ordered))
	actual := gridHerdrSlotRects(t, h, ordered, unmarked, displaySlots...)
	gridAssertRect(t, "herdr agent", actual[0], agent, 2)
	for i, slot := range ordered {
		gridAssertRect(t, fmt.Sprintf("herdr slot %d", slot), actual[slot], cells[i], 2)
	}
}

// the real backend harness, not a mock of herdr's command order.
func TestGridHerdrLifecycle(t *testing.T) {
	skipWithoutHerdr(t)
	h := startHerdrSession(t)
	size := gridHerdrSize(t, h, 11)
	c := h.mcpClient(t)
	for slot := 1; slot <= 11; slot++ {
		out := gridOpen(t, c, map[string]any{"slot": slot, "layout": "agent-grid"})
		gridHerdrOutcome(t, out, size, slot)
		slots := gridIntRange(1, slot)
		// Check before execute-command can possibly reapply the layout.
		gridAssertHerdrGrid(t, h, size, slots, slot)
		gridHerdrMark(t, c, slot)
		gridAssertHerdrGrid(t, h, size, slots, 0)
	}
	entries := closedEntries(t, c, map[string]any{"slot": 5})
	if len(entries) != 1 || entries[0]["action"] != "killed" {
		t.Fatalf("herdr middle close=%v, want one killed entry", entries)
	}
	left := []int{1, 2, 3, 4, 6, 7, 8, 9, 10, 11}
	gridAssertHerdrGrid(t, h, size, left, 0)
	if entries := closedEntries(t, c, map[string]any{"slot": "all"}); len(entries) != len(left) {
		t.Errorf("herdr close all returned %d entries, want %d", len(entries), len(left))
	}
	gridAssertHerdrGrid(t, h, size, nil, 0)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1}), "default", true, "")
}

func TestGridHerdrForeignPane(t *testing.T) {
	skipWithoutHerdr(t)
	h := startHerdrSession(t)
	c := h.mcpClient(t)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1}), "default", true, "")
	var split struct {
		Result struct {
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	h.cli(t, &split, "pane", "split", h.agent, "--direction", "right", "--no-focus")
	user := split.Result.Pane.PaneID
	if user == "" {
		t.Fatal("herdr did not return the user pane")
	}
	before := h.layout(t)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"}), "agent-grid", false, gridUserReason)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 1}), "agent-grid", false, gridUserReason)
	if after := h.layout(t); !reflect.DeepEqual(before, after) {
		t.Errorf("herdr foreign refusal moved or resized panes: before=%v after=%v", before, after)
	}
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 2}), "agent-grid", false, gridUserReason)
	after := h.layout(t)
	if len(after) != len(before)+1 {
		t.Errorf("foreign pane prevented the otherwise valid open: before=%v after=%v", before, after)
	}
	if r, ok := after[user]; !ok || r != before[user] {
		t.Errorf("new slot touched the user pane: before=%+v after=%+v", before[user], r)
	}
}

func TestGridHerdrRestart(t *testing.T) {
	skipWithoutHerdr(t)
	h := startHerdrSession(t)
	size := gridHerdrSize(t, h, 2)
	c := h.mcpClient(t)
	gridHerdrOutcome(t, gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"}), size, 1)
	gridAssertHerdrGrid(t, h, size, []int{1}, 1)
	gridHerdrMark(t, c, 1)
	c.close()

	restarted := h.mcpClient(t)
	gridHerdrOutcome(t, gridOpen(t, restarted, map[string]any{"slot": 2}), size, 2)
	gridAssertHerdrGrid(t, h, size, []int{1, 2}, 2)
	gridHerdrMark(t, restarted, 2)
	gridAssertHerdrGrid(t, h, size, []int{1, 2}, 0)
}

func TestGridHerdrDefault(t *testing.T) {
	skipWithoutHerdr(t)
	h := startHerdrSession(t)
	c := h.mcpClient(t)
	for slot := 1; slot <= 3; slot++ {
		gridOutcome(t, gridOpen(t, c, map[string]any{"slot": slot}), "default", true, "")
		gridHerdrMark(t, c, slot)
	}
	before := gridHerdrSlotRects(t, h, []int{1, 2, 3}, 0)
	gridOutcome(t, gridOpen(t, c, map[string]any{"slot": 4}), "default", true, "")
	after := gridHerdrSlotRects(t, h, []int{1, 2, 3, 4}, 4)
	agent, three, four := after[0], after[3], after[4]
	if three.Left != agent.Left || three.Top <= agent.Top {
		t.Errorf("herdr default slot 3 must stay under the agent after slot 4: agent=%+v slot3=%+v", agent, three)
	}
	largest, anchorArea := 0, -1
	for _, slot := range []int{1, 2, 3} {
		r := before[slot]
		if r.area() > largest {
			largest = r.area()
		}
		if r.contains(four) {
			anchorArea = r.area()
		}
	}
	if anchorArea != largest {
		t.Errorf("herdr default slot 4 at %+v did not split a largest helper: before=%v", four, before)
	}
}

func TestGridHerdrCreators(t *testing.T) {
	skipWithoutHerdr(t)
	for _, tool := range gridOtherCreators {
		t.Run(tool, func(t *testing.T) {
			h := startHerdrSession(t)
			size := gridHerdrSize(t, h, 4)
			c := h.mcpClient(t)
			gridHerdrOutcome(t, gridOpen(t, c, map[string]any{"slot": 1, "layout": "agent-grid"}), size, 1)
			gridAssertHerdrGrid(t, h, size, []int{1}, 1)
			gridHerdrMark(t, c, 1)
			for slot := 2; slot <= 4; slot++ {
				gridCallCreator(t, c, tool, slot)
				var displaySlots []int
				if tool == "write-to-display" {
					displaySlots = gridIntRange(2, slot)
				}
				// No subsequent open/reuse is allowed to repair geometry first.
				gridAssertHerdrGrid(t, h, size, gridIntRange(1, slot), 0, displaySlots...)
			}
		})
	}
}
