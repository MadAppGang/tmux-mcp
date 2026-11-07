package main

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// These expectations come from the geometry table, not from gridShape or planGrid.
var gridSpecCases = []struct {
	n, percent, rows, cols int
	counts                 []int
}{
	{0, 100, 0, 0, nil},
	{1, 50, 1, 1, []int{1}},
	{2, 40, 2, 1, []int{1, 1}},
	{3, 40, 3, 1, []int{1, 1, 1}},
	{4, 30, 2, 2, []int{2, 2}},
	{5, 30, 2, 3, []int{3, 2}},
	{6, 30, 2, 3, []int{3, 3}},
	{7, 25, 2, 4, []int{4, 3}},
	{8, 25, 2, 4, []int{4, 4}},
	{9, 25, 2, 5, []int{5, 4}},
	{10, 25, 2, 5, []int{5, 5}},
	{11, 25, 3, 4, []int{4, 4, 3}},
	{15, 25, 3, 5, []int{5, 5, 5}},
}

func gridSpec(n int) (int, []int) {
	for _, tc := range gridSpecCases {
		if tc.n == n {
			return tc.percent, tc.counts
		}
	}
	panic(fmt.Sprintf("no independent geometry fixture for %d slots", n))
}

// gridExpected is a test oracle expressed in screen cells. It never calls any
// production geometry function and is shared by both backend acceptance suites.
func gridExpected(w, h, gap, n int) (cellRect, []cellRect) {
	percent, rows := gridSpec(n)
	agent := cellRect{Left: 0, Top: 0, Width: w * percent / 100, Height: h}
	if n == 0 {
		return agent, nil
	}
	rightX := agent.Width + gap
	rightW := w - rightX
	rowSpace := h - (len(rows)-1)*gap
	var cells []cellRect
	y := 0
	for row, count := range rows {
		height := rowSpace / len(rows)
		if row < rowSpace%len(rows) {
			height++
		}
		widthSpace := rightW - (count-1)*gap
		x := rightX
		for col := 0; col < count; col++ {
			width := widthSpace / count
			if col < widthSpace%count {
				width++
			}
			cells = append(cells, cellRect{Left: x, Top: y, Width: width, Height: height})
			x += width + gap
		}
		y += height + gap
	}
	return agent, cells
}

func gridReadable(cells []cellRect) bool {
	for _, r := range cells {
		if r.Width < 10 || r.Height < 3 {
			return false
		}
	}
	return true
}

func gridAssertRect(t *testing.T, label string, got, want cellRect, tolerance int) {
	t.Helper()
	edges := []struct {
		name      string
		got, want int
	}{
		{"left", got.Left, want.Left},
		{"top", got.Top, want.Top},
		{"right", got.Left + got.Width, want.Left + want.Width},
		{"bottom", got.Top + got.Height, want.Top + want.Height},
	}
	for _, edge := range edges {
		if delta := edge.got - edge.want; delta < -tolerance || delta > tolerance {
			t.Errorf("%s %s boundary = %d, want %d within %d cells (got %+v, want %+v)",
				label, edge.name, edge.got, edge.want, tolerance, got, want)
		}
	}
}

// The supplied contract does not declare rect's fields. Observe plan rectangles
// through the declared select-layout grammar instead of guessing those fields.
type gridLayoutNode struct {
	rect     cellRect
	axis     byte
	children []gridLayoutNode
}

func gridDecodeLayout(t *testing.T, layout string) gridLayoutNode {
	t.Helper()
	pos := 0
	number := func() int {
		start := pos
		for pos < len(layout) && layout[pos] >= '0' && layout[pos] <= '9' {
			pos++
		}
		if start == pos {
			t.Fatalf("expected an integer at byte %d of layout %q", pos, layout)
		}
		v, err := strconv.Atoi(layout[start:pos])
		if err != nil {
			t.Fatalf("decode layout integer: %v", err)
		}
		return v
	}
	take := func(want byte) {
		if pos >= len(layout) || layout[pos] != want {
			t.Fatalf("expected %q at byte %d of layout %q", want, pos, layout)
		}
		pos++
	}
	var node func() gridLayoutNode
	node = func() gridLayoutNode {
		w := number()
		take('x')
		h := number()
		take(',')
		x := number()
		take(',')
		y := number()
		out := gridLayoutNode{rect: cellRect{Left: x, Top: y, Width: w, Height: h}}
		if pos < len(layout) && (layout[pos] == '{' || layout[pos] == '[') {
			out.axis = layout[pos]
			pos++
			end := byte('}')
			if out.axis == '[' {
				end = ']'
			}
			for {
				out.children = append(out.children, node())
				if pos < len(layout) && layout[pos] == end {
					pos++
					break
				}
				take(',')
			}
		}
		return out
	}
	root := node()
	if pos != len(layout) {
		t.Fatalf("unexpected suffix at byte %d of layout %q (leaves must not carry pane ids)", pos, layout)
	}
	return root
}

func gridLeaves(n gridLayoutNode) []cellRect {
	if len(n.children) == 0 {
		return []cellRect{n.rect}
	}
	var out []cellRect
	for _, child := range n.children {
		out = append(out, gridLeaves(child)...)
	}
	return out
}

func gridAssertTree(t *testing.T, root gridLayoutNode, counts []int) {
	t.Helper()
	if len(counts) == 0 {
		if len(root.children) != 0 {
			t.Fatal("zero slots must serialize as one full-window agent leaf")
		}
		return
	}
	if root.axis != '{' || len(root.children) != 2 || len(root.children[0].children) != 0 {
		t.Fatalf("grid must be an agent leaf beside the entire right side: %+v", root)
	}
	right := root.children[1]
	if len(counts) == 1 && counts[0] == 1 {
		if len(right.children) != 0 {
			t.Error("one slot must be a right-hand leaf")
		}
		return
	}
	if right.axis != '[' || len(right.children) != len(counts) {
		t.Fatalf("right side must have %d stacked rows, got %+v", len(counts), right)
	}
	for row, count := range counts {
		n := right.children[row]
		if count == 1 {
			if len(n.children) != 0 {
				t.Errorf("row %d must be a single leaf", row)
			}
		} else if n.axis != '{' || len(n.children) != count {
			t.Errorf("row %d must contain %d side-by-side cells, got %+v", row, count, n)
		}
	}
}

func TestGridShape(t *testing.T) {
	for _, tc := range gridSpecCases {
		t.Run(fmt.Sprintf("n=%d", tc.n), func(t *testing.T) {
			p, rows, cols := gridShape(tc.n)
			if p != tc.percent || rows != tc.rows || cols != tc.cols {
				t.Errorf("gridShape(%d) = (%d,%d,%d), want (%d,%d,%d)",
					tc.n, p, rows, cols, tc.percent, tc.rows, tc.cols)
			}
		})
	}
}

// exact cells, including remainder and short-last-row cases.
func TestGridPlan(t *testing.T) {
	for _, size := range []struct{ w, h int }{{200, 50}, {203, 53}} {
		for _, gap := range []int{0, 1} {
			for _, tc := range gridSpecCases {
				t.Run(fmt.Sprintf("%dx%d/gap=%d/n=%d", size.w, size.h, gap, tc.n), func(t *testing.T) {
					got := planGrid(size.w, size.h, gap, tc.n)
					if got.Width != size.w || got.Height != size.h || got.Gap != gap {
						t.Errorf("plan describes %dx%d gap %d, want %dx%d gap %d",
							got.Width, got.Height, got.Gap, size.w, size.h, gap)
					}
					if !got.Fits || !got.Roomy {
						t.Errorf("a roomy fixture reports Fits=%v Roomy=%v", got.Fits, got.Roomy)
					}
					if len(got.Cells) != tc.n {
						t.Errorf("plan has %d cells, want %d", len(got.Cells), tc.n)
					}
					if len(got.RowCounts) != len(tc.counts) {
						t.Fatalf("RowCounts=%v, want %v", got.RowCounts, tc.counts)
					}
					for i, count := range tc.counts {
						if got.RowCounts[i] != count {
							t.Errorf("RowCounts=%v, want %v", got.RowCounts, tc.counts)
						}
					}
					root := gridDecodeLayout(t, tmuxLayoutString(got))
					gridAssertTree(t, root, tc.counts)
					agent, cells := gridExpected(size.w, size.h, gap, tc.n)
					want := append([]cellRect{agent}, cells...)
					if leaves := gridLeaves(root); !reflect.DeepEqual(leaves, want) {
						t.Errorf("agent/slot-order leaves = %+v, want %+v", leaves, want)
					}
					gridAssertRect(t, "layout root", root.rect,
						cellRect{Width: size.w, Height: size.h}, 0)
				})
			}
		}
	}
}

func TestGridCapacity(t *testing.T) {
	tests := []struct {
		name                string
		w, h, gap, n        int
		wantFits, wantRoomy bool
	}{
		{"agent only", 1, 1, 1, 0, true, true},
		{"minimal visible cells", 3, 1, 1, 1, true, false},
		{"exactly readable", 21, 3, 1, 1, true, true},
		{"one column too narrow", 20, 3, 1, 1, true, false},
		{"one row too short", 21, 2, 1, 1, true, false},
		{"empty right rectangle", 2, 10, 1, 1, false, false},
		{"two readable rows", 33, 7, 1, 4, true, true},
		{"short last row cannot rescue narrow first row", 31, 7, 1, 5, true, false},
		{"grid wider than the window", 4, 15, 1, 4, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planGrid(tc.w, tc.h, tc.gap, tc.n)
			if got.Fits != tc.wantFits || got.Roomy != tc.wantRoomy {
				t.Errorf("planGrid(%d,%d,%d,%d): Fits=%v Roomy=%v, want %v/%v",
					tc.w, tc.h, tc.gap, tc.n, got.Fits, got.Roomy, tc.wantFits, tc.wantRoomy)
			}
		})
	}
}

// screen membership is not the alive-slot reuse filter.
func TestGridMembers(t *testing.T) {
	self, low, old, young := newPaneRef("%100"), newPaneRef("%101"), newPaneRef("%102"), newPaneRef("%103")
	reg := map[paneRef]paneRecord{
		low:   {Ref: low, Owner: "agent", Slot: 2, Seq: 9, Agent: self},
		old:   {Ref: old, Owner: "agent", Slot: 7, Seq: 3, Dead: true, Agent: self},
		young: {Ref: young, Owner: "agent", Slot: 7, Seq: 10, Agent: self},
	}
	members, reason := classifyWindow(self, []paneInfo{
		{Ref: young}, {Ref: self}, {Ref: old}, {Ref: low},
	}, reg)
	if reason != "" {
		t.Fatalf("all panes belong to this agent, got refusal %q", reason)
	}
	want := []paneRecord{reg[low], reg[old], reg[young]}
	if !reflect.DeepEqual(members, want) {
		t.Errorf("members=%+v, want numeric slots then oldest sequence, including the corpse: %+v", members, want)
	}
}

func TestGridForeignReasons(t *testing.T) {
	self, foreign, other := newPaneRef("%100"), newPaneRef("%101"), newPaneRef("%102")
	tests := []struct {
		name   string
		record *paneRecord
		reason string
	}{
		{"user", nil, "the window holds a pane the user opened, so no pane was moved or resized"},
		{"acquired", &paneRecord{Ref: foreign, Owner: "acquired", Slot: 1},
			"the window holds a pane the user opened that an earlier version took over as a slot, so no pane was moved or resized"},
		{"another agent", &paneRecord{Ref: foreign, Owner: "agent", Slot: 1, Agent: other},
			"the window holds another agent's helper pane, so no pane was moved or resized"},
		{"old unstamped helper", &paneRecord{Ref: foreign, Owner: "agent", Slot: 1},
			"the window holds a helper pane that does not record which agent opened it, so no pane was moved or resized"},
		{"unslotted stamped helper", &paneRecord{Ref: foreign, Owner: "agent", Agent: self},
			"the window holds a helper pane that no slot names, so no pane was moved or resized"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := map[paneRef]paneRecord{}
			if tc.record != nil {
				reg[foreign] = *tc.record
			}
			_, got := classifyWindow(self, []paneInfo{{Ref: self}, {Ref: foreign}}, reg)
			if got != tc.reason {
				t.Errorf("refusal=%q, want %q", got, tc.reason)
			}
			if idInText.MatchString(got) {
				t.Errorf("layout reason leaks an id: %q", got)
			}
		})
	}
}

// This interprets the public move-plan output, not backend calls or private state.
// both documented ratio rounding policies must reach the desired cells.
func TestGridHerdrMoves(t *testing.T) {
	for _, tc := range gridSpecCases {
		for _, rounding := range []string{"floor", "nearest"} {
			t.Run(fmt.Sprintf("n=%d/%s", tc.n, rounding), func(t *testing.T) {
				const w, h = 203, 53
				moves := herdrMoves(planGrid(w, h, 0, tc.n))
				panes := map[int]cellRect{-1: {Width: w, Height: h}}
				for _, move := range moves {
					span, ok := panes[move.Target]
					if !ok {
						t.Fatalf("move targets cell %d before that cell exists", move.Target)
					}
					if move.Cell < 0 || move.Cell >= tc.n {
						t.Fatalf("move names cell %d outside the %d slots", move.Cell, tc.n)
					}
					if _, exists := panes[move.Cell]; exists {
						t.Fatalf("move introduces cell %d twice", move.Cell)
					}
					if math.IsNaN(move.Ratio) || move.Ratio <= 0 || move.Ratio >= 1 {
						t.Fatalf("invalid split ratio %v", move.Ratio)
					}
					round := func(v float64) int {
						if rounding == "floor" {
							return int(math.Floor(v))
						}
						return int(math.Round(v))
					}
					keep, child := span, span
					switch move.Dir {
					case "right":
						keep.Width = round(float64(span.Width) * move.Ratio)
						child.Left += keep.Width
						child.Width -= keep.Width
					case "down":
						keep.Height = round(float64(span.Height) * move.Ratio)
						child.Top += keep.Height
						child.Height -= keep.Height
					default:
						t.Fatalf("unknown move direction %q", move.Dir)
					}
					panes[move.Target], panes[move.Cell] = keep, child
				}
				agent, cells := gridExpected(w, h, 0, tc.n)
				gridAssertRect(t, "agent", panes[-1], agent, 0)
				if len(panes) != len(cells)+1 {
					t.Fatalf("move plan produced %d panes, want %d", len(panes), len(cells)+1)
				}
				for i, want := range cells {
					got, ok := panes[i]
					if !ok {
						t.Fatalf("move plan never introduced cell %d", i)
					}
					gridAssertRect(t, fmt.Sprintf("cell %d", i), got, want, 0)
				}
			})
		}
	}
}

func TestGridParseLayout(t *testing.T) {
	request := func(args map[string]any) mcp.CallToolRequest {
		return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "open-pane", Arguments: args}}
	}
	unstated, err := parseLayoutArg(request(map[string]any{}))
	if err != nil || unstated != layoutUnstated {
		t.Fatalf("omitted layout = %v, %v; want unstated", unstated, err)
	}
	t.Run("null", func(t *testing.T) {
		got, err := parseLayoutArg(request(map[string]any{"layout": nil}))
		if err != nil || got != layoutUnstated {
			t.Fatalf("null layout = %v, %v; want unstated with no error", got, err)
		}
	})
	grid, err := parseLayoutArg(request(map[string]any{"layout": "agent-grid"}))
	if err != nil {
		t.Fatal(err)
	}
	def, err := parseLayoutArg(request(map[string]any{"layout": "default"}))
	if err != nil {
		t.Fatal(err)
	}
	if grid == def || grid == unstated || def == unstated {
		t.Error("grid, default and omission must be distinct requests")
	}
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"misspelling", "grid", "layout must be \"agent-grid\" or \"default\", got \"grid\""},
		{"empty", "", "layout must be \"agent-grid\" or \"default\", got \"\""},
		{"case sensitive", "AGENT-GRID", "layout must be \"agent-grid\" or \"default\", got \"AGENT-GRID\""},
		{"bool", true, "layout must be \"agent-grid\" or \"default\", got true"},
		{"number", float64(7), "layout must be \"agent-grid\" or \"default\", got 7"},
		{"array", []any{"agent-grid"}, ""},
		{"object", map[string]any{"mode": "agent-grid"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLayoutArg(request(map[string]any{"layout": tc.value}))
			if err == nil {
				t.Fatalf("accepted invalid layout %#v", tc.value)
			}
			if tc.want != "" && err.Error() != tc.want {
				t.Errorf("error=%q, want %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), "layout must be \"agent-grid\" or \"default\"") {
				t.Errorf("error does not explain the accepted modes: %v", err)
			}
		})
	}
}
