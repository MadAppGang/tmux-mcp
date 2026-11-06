package main

import (
	"testing"
)

// TestPlacementTable pins where each slot's pane goes, as the user sees it.
//
//	slot 1  → right of the agent
//	slot 2  → under slot 1, in slot 1's column; right of the agent when there is
//	          no slot 1, which is the spot slot 1 would have taken
//	slot 3  → under the agent, in the agent's column
//	slot ≥4 → carved out of the largest helper, along its longer side
//
// The cases are measured through open-pane and tmux geometry only, so they hold
// whatever the resolver looks like inside. The window is 200x50, which is wide
// enough that every split here is a placement decision rather than tmux's
// minimum-size clamp.
//
// REGRESSION: slot placement depended on what had been adopted, so slots 2 and 3 stacked under the agent and squeezed it
func TestPlacementTable(t *testing.T) {
	t.Run("fresh window, slots 1 to 4", func(t *testing.T) {
		c, self := agentPaneFixture(t)

		one := openSlot(t, c, self, 1)
		two := openSlot(t, c, self, 2)
		three := openSlot(t, c, self, 3)
		agent, r1, r2, r3 := paneRect(t, self), paneRect(t, one), paneRect(t, two), paneRect(t, three)
		layout := windowLayout(t, self)

		if r1.Left <= agent.Left || r1.Top != agent.Top {
			t.Errorf("slot 1 is at %+v; it belongs right of the agent at %+v.\nwindow:\n%s", r1, agent, layout)
		}
		if r2.Left != r1.Left || r2.Top <= r1.Top {
			t.Errorf("slot 2 is at %+v; it belongs under slot 1 at %+v, in the same column.\nwindow:\n%s",
				r2, r1, layout)
		}
		if r3.Left != agent.Left || r3.Top <= agent.Top {
			t.Errorf("slot 3 is at %+v; it belongs under the agent at %+v, in the same column.\nwindow:\n%s",
				r3, agent, layout)
		}

		// Slot 4 is carved out of the largest helper. Which helper that is, is
		// read off the window rather than assumed, and a helper wider than twice
		// its height (cells are about twice as tall as wide) is split side by
		// side, so slot 4 shares its anchor's top row.
		helpers := map[string]cellRect{one: r1, two: r2, three: r3}
		largest := 0
		for _, r := range helpers {
			if r.area() > largest {
				largest = r.area()
			}
		}
		four := openSlot(t, c, self, 4)
		r4 := paneRect(t, four)
		layout = windowLayout(t, self)
		anchor, anchorRect := "", cellRect{}
		for id, r := range helpers {
			if r.contains(r4) {
				anchor, anchorRect = id, r
			}
		}
		switch {
		case anchor == "":
			t.Errorf("slot 4 at %+v was not carved out of any helper (%v).\nwindow:\n%s", r4, helpers, layout)
		case anchorRect.area() != largest:
			t.Errorf("slot 4 was carved out of %s (%+v, area %d), not the largest helper (area %d).\nwindow:\n%s",
				anchor, anchorRect, anchorRect.area(), largest, layout)
		case anchorRect.Width > 2*anchorRect.Height && r4.Top != anchorRect.Top:
			t.Errorf("slot 4 is at %+v, under %s (%+v); that helper is wider than it is tall on "+
				"screen, so it is split along its width and slot 4 sits beside it.\nwindow:\n%s",
				r4, anchor, anchorRect, layout)
		case anchorRect.Width <= 2*anchorRect.Height && r4.Left != anchorRect.Left:
			t.Errorf("slot 4 is at %+v, beside %s (%+v); that helper is taller than it is wide on "+
				"screen, so slot 4 goes under it.\nwindow:\n%s", r4, anchor, anchorRect, layout)
		}
	})

	t.Run("slot 2 first lands right of the agent", func(t *testing.T) {
		c, self := agentPaneFixture(t)

		two := openSlot(t, c, self, 2)
		agent, r2 := paneRect(t, self), paneRect(t, two)
		if r2.Left <= agent.Left || r2.Top != agent.Top {
			t.Errorf("slot 2, opened with no slot 1, is at %+v; it belongs right of the agent at %+v, "+
				"where slot 1 would have gone.\nwindow:\n%s", r2, agent, windowLayout(t, self))
		}
	})

	t.Run("an unmarked user shell does not move slot 1", func(t *testing.T) {
		c, self := agentPaneFixture(t)
		requireZshPanes(t)

		heightBefore := paneRect(t, self).Height
		users := rawSplit(t, self, "-h")
		waitForIdleZsh(t, users)
		usersBefore := paneRect(t, users)

		one := openSlot(t, c, self, 1)
		agentAfterOne, r1 := paneRect(t, self), paneRect(t, one)
		layout := windowLayout(t, self)
		if one == users {
			t.Errorf("slot 1 is the user's pane %s.\nwindow:\n%s", users, layout)
		}
		if r1.Left != agentAfterOne.Left+agentAfterOne.Width+1 || r1.Top != agentAfterOne.Top {
			t.Errorf("slot 1 is at %+v; it belongs directly right of the agent at %+v.\nwindow:\n%s",
				r1, agentAfterOne, layout)
		}

		openSlot(t, c, self, 2)
		openSlot(t, c, self, 3)
		layout = windowLayout(t, self)
		if got := paneRect(t, self).Height; 2*got < heightBefore-1 {
			t.Errorf("after slots 1 to 3 the agent's pane is %d rows tall, from %d; it must keep at "+
				"least half.\nwindow:\n%s", got, heightBefore, layout)
		}
		if after := paneRect(t, users); after != usersBefore {
			t.Errorf("the user's pane %s went from %+v to %+v.\nwindow:\n%s", users, usersBefore, after, layout)
		}
		if marks := mcpMarks(t, users); marks != [3]string{} {
			t.Errorf("the user's pane %s carries marks %q.\nwindow:\n%s", users, marks, layout)
		}
	})
}
