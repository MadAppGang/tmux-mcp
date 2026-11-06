package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestHerdrDashLeadingTextIsTyped pins how text that starts with a dash reaches
// a herdr pane: verbatim, flags and all. herdr takes the TEXT argument of
// `pane send-text` as it is, so no `--` is passed before it — one would be
// typed into the pane along with the text.
func TestHerdrDashLeadingTextIsTyped(t *testing.T) {
	h := startHerdrSession(t)
	c := h.mcpClient(t)

	var pane string
	marker := fmt.Sprintf("--dash-marker-%d", time.Now().UnixNano())
	pane = h.newPane(t, func() {
		c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo "}, &map[string]any{})
	})
	c.callToolJSON(t, "send-keys", map[string]any{"keys": marker, "enter": true}, &map[string]any{})

	deadline := time.Now().Add(5 * time.Second)
	for {
		screen := h.screen(t, pane)
		lines := strings.Split(screen, "\n")
		echoed := false
		for _, l := range lines {
			if strings.TrimSpace(l) == marker {
				echoed = true
			}
		}
		if echoed && !strings.Contains(screen, "-- "+marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dash-leading text did not arrive verbatim; the pane holds:\n%s", screen)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
