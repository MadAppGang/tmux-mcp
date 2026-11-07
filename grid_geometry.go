package main

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the agent grid's geometry, and it is pure: no Backend, no
// context, no multiplexer. Given a window size and a number of slots it says
// where every pane goes, and given the panes in a window it says which of them
// the grid may move. Everything that has to touch a pane lives in layout.go and
// in the adapters, so this arithmetic can be checked on its own.

// rect is a rectangle in cells, with the origin at the window's top left.
type rect struct {
	X, Y, W, H int
}

// gridShape is the grid for n slots: the share of the window's width the agent
// keeps, and how many rows and columns the slots on the right are laid out in.
//
// The table is written for a person watching several helpers at once. One
// helper gets half the window. Two or three stack in one column, because a
// column of shells is still readable at 60% of the width. From four on, a
// single column would give each pane a few rows, so they go into two rows, and
// from eleven into three, with as many columns as that takes. The agent keeps
// less of the width as the slots grow, and never less than a quarter.
//
// Slots fill the grid row by row, so the last row holds whatever is left over.
func gridShape(n int) (agentPercent, rows, cols int) {
	switch {
	case n <= 0:
		return 100, 0, 0
	case n == 1:
		return 50, 1, 1
	case n <= 3:
		return 40, n, 1
	case n <= 6:
		return 30, 2, (n + 1) / 2
	case n <= 10:
		return 25, 2, (n + 1) / 2
	default:
		return 25, 3, (n + 2) / 3
	}
}

// gridPlan is where every pane goes for one window size and one slot count.
//
// Cells are in slot order, row by row, and that order is the contract with the
// adapters: the first cell belongs to the lowest slot. RowCounts says how many
// cells each row holds.
//
// Fits is false when some rectangle would be empty, which is a grid the
// multiplexer cannot hold. Roomy is false when a slot's cell is smaller than a
// shell can be read in (minCellWidth × minCellHeight); such a grid can be
// applied, but the caller is told it is cramped.
type gridPlan struct {
	Width, Height int
	Gap           int
	Agent         rect
	Cells         []rect
	RowCounts     []int
	Fits          bool
	Roomy         bool
}

// The smallest slot cell counted as readable: a line of a prompt and two of
// output, ten columns wide.
const (
	minCellWidth  = 10
	minCellHeight = 3
)

// planGrid lays n slots out in a w×h window whose panes are separated by gap
// cells (tmux draws a one-cell border; herdr draws none).
//
// The agent's width is its percentage of w, rounded down, and it is full
// height. The slots share what is left to its right, after one gap. Rows split
// the height evenly, after the gaps between them, with the cells left over going
// to the top rows. Cells in a row split the width the same way, with the extra
// going to the left cells. A short last row has fewer cells, so they are wider:
// a row is always filled edge to edge.
func planGrid(w, h, gap, n int) gridPlan {
	agentPercent, rows, cols := gridShape(n)
	plan := gridPlan{Width: w, Height: h, Gap: gap}
	if n <= 0 {
		plan.Agent = rect{0, 0, w, h}
		plan.Fits = w >= 1 && h >= 1
		plan.Roomy = plan.Fits
		return plan
	}

	agentW := w * agentPercent / 100
	plan.Agent = rect{0, 0, agentW, h}
	rightX := agentW + gap
	rightW := w - agentW - gap

	plan.RowCounts = make([]int, rows)
	for r := range plan.RowCounts {
		plan.RowCounts[r] = cols
	}
	plan.RowCounts[rows-1] = n - cols*(rows-1)

	heights := splitEvenly(h, rows, gap)
	y := 0
	for r, count := range plan.RowCounts {
		widths := splitEvenly(rightW, count, gap)
		x := rightX
		for _, cw := range widths {
			plan.Cells = append(plan.Cells, rect{x, y, cw, heights[r]})
			x += cw + gap
		}
		y += heights[r] + gap
	}

	plan.Fits = plan.Agent.W >= 1 && plan.Agent.H >= 1
	plan.Roomy = plan.Fits
	for _, c := range plan.Cells {
		if c.W < 1 || c.H < 1 {
			plan.Fits = false
		}
		if c.W < minCellWidth || c.H < minCellHeight {
			plan.Roomy = false
		}
	}
	plan.Roomy = plan.Roomy && plan.Fits
	return plan
}

// splitEvenly divides total cells into parts pieces separated by gap cells. The
// remainder goes one cell at a time to the first pieces. A piece can come out
// zero or negative when the space is too small, and planGrid reports that as a
// grid that does not fit rather than hiding it. parts is at least 1.
func splitEvenly(total, parts, gap int) []int {
	avail := total - (parts-1)*gap
	base, rem := avail/parts, avail%parts
	if rem < 0 {
		rem = 0
	}
	out := make([]int, parts)
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

// ---- Which panes the grid may move ----

// The reasons the grid leaves a window alone. Each is a whole sentence for the
// caller, and none names a pane: the slot is the only handle this contract has,
// and these panes are by definition not slots of the caller's.
const (
	foreignUserPane    = "the window holds a pane the user opened, so no pane was moved or resized"
	foreignAdoptedPane = "the window holds a pane the user opened that an earlier version took over as a slot, so no pane was moved or resized"
	foreignOtherAgent  = "the window holds another agent's helper pane, so no pane was moved or resized"
	foreignUnstamped   = "the window holds a helper pane that does not record which agent opened it, so no pane was moved or resized"
	foreignUnslotted   = "the window holds a helper pane that no slot names, so no pane was moved or resized"
)

// foreignPriority orders the reasons when a window holds several kinds of
// foreign pane, so the answer is the same on every call. The user's own pane
// comes first: it is the one a caller most needs to hear about.
var foreignPriority = []string{
	foreignUserPane, foreignAdoptedPane, foreignOtherAgent, foreignUnstamped, foreignUnslotted,
}

// classifyWindow sorts the panes in the agent's window into the slots the grid
// lays out (members) and the reason it must not, if there is one.
//
// A member is a pane this server family created (owner agent), that holds a
// slot, and whose record says this agent opened it. Everything else in the
// window except the agent's own pane is foreign, and one foreign pane is enough
// to leave the whole window alone: a layout is set for the whole tree at once,
// so applying it would move that pane too.
//
// A helper whose record does not say which agent opened it is foreign, and that
// is the conservative reading on purpose. Such a record comes from a server
// older than the grid, from a server that crashed between claiming a pane and
// stamping it, or, under herdr, from an older binary rewriting the state file.
// Slot numbers are shared by every agent in a window, so nothing in that record
// says the pane is this agent's. Treating it as this agent's would let one agent
// resize another's helpers, which the grid must never do.
//
// Dead panes count. A pane whose process exited is still on the user's screen
// under remain-on-exit and still holds its slot until close-pane reaps it, so
// the grid gives it a cell like any other.
//
// Members come back in slot order, the oldest pane first within a slot, which is
// the order of the grid's cells.
func classifyWindow(self paneRef, panes []paneInfo, reg map[paneRef]paneRecord) ([]paneRecord, string) {
	var members []paneRecord
	found := map[string]bool{}
	for _, p := range panes {
		if p.Ref == self {
			continue
		}
		rec, ok := reg[p.Ref]
		switch {
		case !ok:
			found[foreignUserPane] = true
		case rec.Owner == ownerAcquired:
			found[foreignAdoptedPane] = true
		case rec.Owner != ownerAgent:
			found[foreignUserPane] = true
		case rec.Slot < slotDefault:
			found[foreignUnslotted] = true
		case rec.Agent.empty():
			found[foreignUnstamped] = true
		case rec.Agent != self:
			found[foreignOtherAgent] = true
		default:
			members = append(members, rec)
		}
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].Slot != members[j].Slot {
			return members[i].Slot < members[j].Slot
		}
		return members[i].Seq < members[j].Seq
	})
	for _, reason := range foreignPriority {
		if found[reason] {
			return members, reason
		}
	}
	return members, ""
}

// ---- Checking the result ----

// borderStatus is where tmux draws a pane's status line, which takes a row from
// some panes: with "top" the panes in the window's top row move down one row,
// and with "bottom" the panes in its bottom row lose their last one. A pane with
// another pane above or below it uses the border it already has.
type borderStatus int

const (
	borderOff borderStatus = iota
	borderTop
	borderBottom
)

// expectedRect is where a pane laid out in cell c actually ends up, once the
// status line has taken its row.
func expectedRect(c rect, windowH int, border borderStatus) rect {
	switch {
	case border == borderTop && c.Y == 0:
		c.Y++
		c.H--
	case border == borderBottom && c.Y+c.H == windowH:
		c.H--
	}
	return c
}

// gridMismatch reports whether any pane is further than tolerance cells from
// where the plan puts it, on any edge. order is the agent followed by the
// members in cell order. A pane missing from panes counts as a mismatch.
func gridMismatch(plan gridPlan, order []paneRef, panes []paneInfo, tolerance int, border borderStatus) bool {
	at := make(map[paneRef]paneInfo, len(panes))
	for _, p := range panes {
		at[p.Ref] = p
	}
	want := append([]rect{plan.Agent}, plan.Cells...)
	if len(order) != len(want) {
		return true
	}
	for i, ref := range order {
		p, ok := at[ref]
		if !ok {
			return true
		}
		w := expectedRect(want[i], plan.Height, border)
		if absInt(p.X-w.X) > tolerance || absInt(p.Y-w.Y) > tolerance ||
			absInt(p.Width-w.W) > tolerance || absInt(p.Height-w.H) > tolerance {
			return true
		}
	}
	return false
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// ---- tmux's layout string ----

// tmuxLayoutString is the plan in the form select-layout takes, without the
// checksum: "WxH,X,Y" for a leaf, followed by {…} for children side by side or
// […] for children stacked.
//
// The tree is the agent beside a right-hand side, and the right-hand side is
// one pane, one column, or a column of rows. Leaves carry no pane id, because
// tmux does not need one: it fills the leaves with the window's panes in index
// order, which is a depth-first walk of this tree. That walk visits the agent
// first and then the cells row by row, so the adapter has to put the panes in
// that order before it applies the string.
func tmuxLayoutString(plan gridPlan) string {
	whole := rect{0, 0, plan.Width, plan.Height}
	if len(plan.Cells) == 0 {
		return layoutNode(whole)
	}
	rightX := plan.Agent.W + plan.Gap
	right := rect{rightX, 0, plan.Width - rightX, plan.Height}

	var rightSide string
	switch {
	case len(plan.Cells) == 1:
		rightSide = layoutNode(plan.Cells[0])
	case len(plan.RowCounts) == len(plan.Cells):
		// One cell per row: a single column.
		parts := make([]string, len(plan.Cells))
		for i, c := range plan.Cells {
			parts[i] = layoutNode(c)
		}
		rightSide = layoutNode(right) + "[" + strings.Join(parts, ",") + "]"
	default:
		rowParts := make([]string, 0, len(plan.RowCounts))
		next := 0
		for _, count := range plan.RowCounts {
			cells := plan.Cells[next : next+count]
			next += count
			row := rect{right.X, cells[0].Y, right.W, cells[0].H}
			if count == 1 {
				rowParts = append(rowParts, layoutNode(row))
				continue
			}
			parts := make([]string, len(cells))
			for i, c := range cells {
				parts[i] = layoutNode(c)
			}
			rowParts = append(rowParts, layoutNode(row)+"{"+strings.Join(parts, ",")+"}")
		}
		rightSide = layoutNode(right) + "[" + strings.Join(rowParts, ",") + "]"
	}
	return layoutNode(whole) + "{" + layoutNode(plan.Agent) + "," + rightSide + "}"
}

// ---- herdr's moves ----

// herdrMove is one step of building the grid out of splits: put cell Cell's
// pane beside (Dir "right") or below (Dir "down") the pane of cell Target, with
// Target keeping Ratio of the space the two then share. Target -1 is the agent.
type herdrMove struct {
	Cell   int
	Target int
	Dir    string
	Ratio  float64
}

// herdrMoves is the plan as a sequence of splits, for a multiplexer that can
// only add a pane beside or below another one.
//
// The first cell goes right of the agent, which then keeps its planned width.
// The first cell of each later row goes below the first cell of the row above,
// while those still span the whole right-hand side, so the rows come out full
// width. Only then is each row filled, every cell going right of the one before
// it. Each target spans from its own top-left edge to the far edge of the
// window when it is split, so its share is its planned size over that span.
//
// The share is a quarter of a cell over the exact one, so that a multiplexer
// which rounds the product down and one which rounds it to the nearest cell
// both land on the planned size.
func herdrMoves(plan gridPlan) []herdrMove {
	if len(plan.Cells) == 0 {
		return nil
	}
	share := func(size, span int) float64 {
		if span <= 0 {
			return 0.5
		}
		r := (float64(size) + 0.25) / float64(span)
		return min(max(r, 0.01), 0.99)
	}
	moves := []herdrMove{{Cell: 0, Target: -1, Dir: "right", Ratio: share(plan.Agent.W, plan.Width)}}

	heads := make([]int, len(plan.RowCounts))
	next := 0
	for r, count := range plan.RowCounts {
		heads[r] = next
		next += count
	}
	for r := 1; r < len(heads); r++ {
		above := plan.Cells[heads[r-1]]
		moves = append(moves, herdrMove{
			Cell: heads[r], Target: heads[r-1], Dir: "down", Ratio: share(above.H, plan.Height-above.Y),
		})
	}
	for r, count := range plan.RowCounts {
		for j := 1; j < count; j++ {
			left := plan.Cells[heads[r]+j-1]
			moves = append(moves, herdrMove{
				Cell: heads[r] + j, Target: heads[r] + j - 1, Dir: "right", Ratio: share(left.W, plan.Width-left.X),
			})
		}
	}
	return moves
}

func layoutNode(r rect) string {
	return fmt.Sprintf("%dx%d,%d,%d", r.W, r.H, r.X, r.Y)
}

// layoutChecksum is the four hex digits tmux puts before a layout string, and
// select-layout refuses a string whose checksum does not match: a 16-bit sum
// rotated right by one bit before each byte is added.
func layoutChecksum(layout string) string {
	var sum uint16
	for i := 0; i < len(layout); i++ {
		sum = (sum >> 1) + ((sum & 1) << 15)
		sum += uint16(layout[i])
	}
	return fmt.Sprintf("%04x", sum)
}
