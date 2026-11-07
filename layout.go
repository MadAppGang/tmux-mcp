package main

import (
	"context"
	"errors"
	"fmt"
)

// This file is the agent grid's policy: when the grid is applied, to which
// panes, and what the caller is told about it. The arithmetic is in
// grid_geometry.go and the moves are the adapters'; what is here decides.
//
// # The mode belongs to the window and the agent
//
// An agent turns the grid on with open-pane{layout:"agent-grid"} and off with
// layout:"default". The mode is stored with the window (tmux) or the tab
// (herdr), under this agent's own pane, so it outlives this server process and
// a second agent in the same window has a mode of its own. It is cleared when
// the agent has no visible slots left in the window, so a later slot 1 is
// placed the default way again.
//
// # When it is applied
//
// In grid mode the grid is applied again after every visible slot this agent
// opens, whichever tool opened it, and after every close-pane. A pane that
// died or vanished on its own is accounted for at the next of those.
//
// # What it never does
//
// It never moves a pane that is not this agent's: one such pane in the window
// and nothing is moved at all, because a layout is set for the whole window at
// once. It never fails the call it rides on: a slot that could be opened is
// opened, and the response says whether the grid was applied and, if not, why.
// It never moves focus. And in default mode it does nothing: placement is
// placementForSlot's, exactly as it always was.

// layoutMode is the mode open-pane reports, in its wire spelling.
type layoutMode string

const (
	layoutDefault layoutMode = "default"
	layoutGrid    layoutMode = "agent-grid"
)

// layoutRequest is what the caller said about the mode: nothing, which leaves
// the mode as it is, or one of the two modes.
type layoutRequest int

const (
	layoutUnstated layoutRequest = iota
	layoutToDefault
	layoutToGrid
)

// layoutOutcome is what open-pane reports: the mode the window is in after the
// call, whether the grid holds, and why not when it does not.
//
// moved and renamed are not reported. moved says whether panes may have been
// resized, which a caller about to type into a shell needs to know: a resized
// shell redraws its prompt, and that redraw has to settle first. renamed maps a
// pane that came out of the arrangement under a new identity to that identity,
// so the target about to be handed out names the pane that is there now.
type layoutOutcome struct {
	Mode    layoutMode
	Applied bool
	Detail  string
	moved   bool
	renamed map[paneRef]paneRef
}

// gridRefusal is an Arrange error that means nothing was moved, and why, in a
// sentence the caller can be given as it is.
type gridRefusal string

func (r gridRefusal) Error() string { return string(r) }

// inDefaultMode is the outcome of every call that leaves the default layout as
// it is.
func inDefaultMode() layoutOutcome { return layoutOutcome{Mode: layoutDefault, Applied: true} }

// The sentences a caller can be given. None names a pane: the slot is the only
// handle this contract has.
const (
	layoutVisibleOnlyText = "layout applies to visible slots"
	layoutZoomedText      = "the window is zoomed, so no pane was moved or resized"
	layoutTooSmallText    = "the window is %dx%d cells and cannot hold %s in the grid, so the panes were left where they are"
	layoutCrampedText     = "the window is %dx%d cells, too small for %s in the grid; it was applied as far as it fits"
	layoutFailedText      = "the grid could not be applied: %s"
	layoutMissedText      = "the panes did not reach the grid's geometry within %d cells"
	layoutNotStoredText   = "the window's layout mode could not be stored: %s"
)

// slotCount is "1 slot", "4 slots".
func slotCount(n int) string {
	if n == 1 {
		return "1 slot"
	}
	return fmt.Sprintf("%d slots", n)
}

// openHook is what resolution calls once it holds the pane it is about to hand
// out, with the self it read and the target it will return. For a pane this
// call created or reclaimed it is called BEFORE the shell is waited for or
// typed into, so that any resize the hook causes is over by the time anything
// is typed. moved reports that panes may have been resized. now is the pane's
// ref after the hook, which is opened.Ref unless arranging the window gave the
// pane a new identity; resolution carries on with now.
type openHook func(ctx context.Context, self paneRef, opened paneTarget) (now paneRef, moved bool)

func (h openHook) call(ctx context.Context, self paneRef, opened paneTarget) (paneRef, bool) {
	if h == nil {
		return opened.Ref, false
	}
	return h(ctx, self, opened)
}

// openSlot resolves a slot as resolveHelper always has and applies the layout
// on the way, all inside one slotMu hold. report says the caller puts the
// outcome in its response; only open-pane does, and only open-pane re-applies
// the grid when it is handed a slot that already existed.
//
// An isolated slot has no place in a window, so a stated layout with
// isolated:true is refused before anything is resolved, and nothing is
// created. A stated layout that turns out to address an existing isolated slot
// is refused once resolution has found it, which also creates nothing: finding
// an existing slot is all resolution does then.
func (s *slots) openSlot(
	ctx context.Context, slot int, kind kindRequest, req layoutRequest, report bool,
) (paneTarget, layoutOutcome, error) {
	if kind == kindIsolated && req != layoutUnstated {
		return paneTarget{}, layoutOutcome{}, errors.New(layoutVisibleOnlyText)
	}
	s.slotMu.Lock()
	defer s.slotMu.Unlock()

	outcome := inDefaultMode()
	refused := false
	hook := func(ctx context.Context, self paneRef, opened paneTarget) (paneRef, bool) {
		if opened.Isolated {
			if req != layoutUnstated {
				refused = true
			} else if report && !self.empty() {
				outcome = s.windowModeLocked(ctx, self)
			}
			return opened.Ref, false
		}
		outcome = s.layoutForOpenLocked(ctx, self, opened, req, report)
		if now, ok := outcome.renamed[opened.Ref]; ok {
			return now, outcome.moved
		}
		return opened.Ref, outcome.moved
	}
	tgt, err := s.resolveHelperLocked(ctx, slot, kind, hook)
	if err != nil {
		return paneTarget{}, layoutOutcome{}, err
	}
	if refused {
		return paneTarget{}, layoutOutcome{}, errors.New(layoutVisibleOnlyText)
	}
	return tgt, outcome, nil
}

// layoutForOpenLocked decides what one visible open does to the layout. The
// caller must hold slotMu.
//
//   - layout:"default" clears the mode and leaves every pane where it is.
//   - layout:"agent-grid" sets the mode and applies the grid.
//   - no layout, on a pane this call created or reclaimed, applies the grid if
//     the mode is on, unless this pane is now the agent's only slot: then the
//     slots the mode was set for are all gone, and the mode goes with them, so
//     the window is back in default mode (and this pane was placed that way).
//   - no layout, on a slot that already existed, re-applies the grid for
//     open-pane, which reports it, and does nothing for the tools that do not.
func (s *slots) layoutForOpenLocked(
	ctx context.Context, self paneRef, opened paneTarget, req layoutRequest, report bool,
) layoutOutcome {
	switch req {
	case layoutToDefault:
		if err := s.b.SetGridMode(ctx, self, false); err != nil {
			out := s.windowModeLocked(ctx, self)
			out.Applied, out.Detail = false, fmt.Sprintf(layoutNotStoredText, err.Error())
			return out
		}
		return inDefaultMode()
	case layoutToGrid:
		if err := s.b.SetGridMode(ctx, self, true); err != nil {
			return layoutOutcome{Mode: layoutDefault, Detail: fmt.Sprintf(layoutNotStoredText, err.Error())}
		}
		return s.arrangeLocked(ctx, self)
	}
	if opened.Created {
		return s.regridAfterCreateLocked(ctx, self, opened.Ref)
	}
	if report {
		return s.arrangeLocked(ctx, self)
	}
	return inDefaultMode()
}

// windowModeLocked reports the window's mode without moving anything. A
// window that cannot be read is reported as default, which is what it is
// treated as.
func (s *slots) windowModeLocked(ctx context.Context, self paneRef) layoutOutcome {
	if win, err := s.b.Window(ctx, self); err == nil && win.Grid {
		return layoutOutcome{Mode: layoutGrid, Applied: true}
	}
	return inDefaultMode()
}

// gridState is one read of everything the grid decision needs.
type gridState struct {
	win     windowInfo
	panes   []paneInfo
	members []paneRecord
	foreign string
}

// readGridLocked reads the window for the grid. done is set when there is
// nothing more to decide: the window is in default mode, or could not be
// read.
//
// A window whose mode cannot be read is treated as default mode, because the
// mode is the read that failed; nothing is moved either way. A window in grid
// mode whose panes cannot be listed is reported as not applied, because a
// window that cannot be classified is one in which nothing may be moved.
func (s *slots) readGridLocked(ctx context.Context, self paneRef) (gridState, *layoutOutcome) {
	win, err := s.b.Window(ctx, self)
	if err != nil || !win.Grid {
		out := inDefaultMode()
		return gridState{}, &out
	}
	reg, err := s.b.Records(ctx, self)
	if err == nil {
		var panes []paneInfo
		if panes, err = s.b.Siblings(ctx, self); err == nil {
			members, foreign := classifyWindow(self, panes, reg)
			return gridState{win: win, panes: panes, members: members, foreign: foreign}, nil
		}
	}
	out := layoutOutcome{Mode: layoutGrid, Detail: fmt.Sprintf(layoutFailedText, err.Error())}
	return gridState{}, &out
}

// clearGridLocked takes the window out of grid mode because the agent has no
// slots left in it. A failure to clear is not reported: nothing is moved in
// a window with no slots, and the next open clears it again.
func (s *slots) clearGridLocked(ctx context.Context, self paneRef) layoutOutcome {
	_ = s.b.SetGridMode(ctx, self, false)
	return inDefaultMode()
}

// arrangeLocked applies the grid if the window is in grid mode. A window in
// which this agent has no slots, and in which nothing foreign stands in the
// way, leaves grid mode. The caller must hold slotMu.
func (s *slots) arrangeLocked(ctx context.Context, self paneRef) layoutOutcome {
	st, done := s.readGridLocked(ctx, self)
	if done != nil {
		return *done
	}
	if len(st.members) == 0 && st.foreign == "" {
		return s.clearGridLocked(ctx, self)
	}
	return s.applyGridLocked(ctx, self, st)
}

// regridAfterCreateLocked is arrangeLocked for a pane resolution has just
// created or reclaimed. If that pane is the agent's only slot, the mode is
// cleared instead: the slots it was set for are gone, so it lapsed before this
// call, and this pane was placed in default mode.
func (s *slots) regridAfterCreateLocked(ctx context.Context, self, created paneRef) layoutOutcome {
	st, done := s.readGridLocked(ctx, self)
	if done != nil {
		return *done
	}
	others := 0
	for _, m := range st.members {
		if m.Ref != created {
			others++
		}
	}
	if others == 0 {
		return s.clearGridLocked(ctx, self)
	}
	return s.applyGridLocked(ctx, self, st)
}

// regridAfterCloseLocked is what every close-pane ends with: the grid again
// over the slots that are left, or default mode when none are. The caller must
// hold slotMu.
func (s *slots) regridAfterCloseLocked(ctx context.Context, self paneRef) {
	st, done := s.readGridLocked(ctx, self)
	if done != nil {
		return
	}
	if len(st.members) == 0 {
		s.clearGridLocked(ctx, self)
		return
	}
	s.applyGridLocked(ctx, self, st)
}

// applyGridLocked moves the panes into the grid and measures where they landed.
//
// Nothing is moved when the window holds a pane that is not this agent's, when
// it is zoomed, or when the grid would leave some pane with no cells at all;
// the caller is told which. A grid whose cells are too small to read a shell in
// is applied anyway, because it is still the layout that was asked for, and
// the caller is told it is cramped.
//
// The adapter may decline as well, with a sentence of its own (gridRefusal),
// when it finds it cannot move the panes without moving the user's focus; that
// sentence is the detail, and nothing was moved.
//
// The result is measured rather than assumed: the panes are listed again and
// compared with the plan within the window's tolerance. A status line drawn on
// the pane borders takes a row from some panes, which is allowed for.
func (s *slots) applyGridLocked(ctx context.Context, self paneRef, st gridState) layoutOutcome {
	out := layoutOutcome{Mode: layoutGrid}
	if st.foreign != "" {
		out.Detail = st.foreign
		return out
	}
	if st.win.Zoomed {
		out.Detail = layoutZoomedText
		return out
	}
	n := len(st.members)
	plan := planGrid(st.win.Width, st.win.Height, st.win.Gap, n)
	if !plan.Fits {
		out.Detail = fmt.Sprintf(layoutTooSmallText, st.win.Width, st.win.Height, slotCount(n))
		return out
	}
	slotRefs := make([]paneRef, n)
	for i, m := range st.members {
		slotRefs[i] = m.Ref
	}
	out.moved = true
	renamed, err := s.b.Arrange(ctx, self, slotRefs, plan)
	if len(renamed) > 0 {
		out.renamed = renamed
		for i, r := range slotRefs {
			if now, ok := renamed[r]; ok {
				slotRefs[i] = now
			}
		}
	}
	var refusal gridRefusal
	switch {
	case errors.As(err, &refusal):
		out.moved = false
		out.Detail = refusal.Error()
		return out
	case err != nil:
		out.Detail = fmt.Sprintf(layoutFailedText, err.Error())
		return out
	}
	panes, err := s.b.Siblings(ctx, self)
	order := append([]paneRef{self}, slotRefs...)
	if err != nil || gridMismatch(plan, order, panes, st.win.Tolerance, st.win.Border) {
		out.Detail = fmt.Sprintf(layoutMissedText, st.win.Tolerance)
		return out
	}
	if !plan.Roomy {
		out.Detail = fmt.Sprintf(layoutCrampedText, st.win.Width, st.win.Height, slotCount(n))
		return out
	}
	out.Applied = true
	return out
}
