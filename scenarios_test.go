package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestScenario_DevServerWorkflow simulates an agent starting a dev server,
// waiting for readiness, and running work in a second pane.
func TestScenario_DevServerWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	serverPane := sess["paneId"].(string)

	// Split off a work pane.
	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    serverPane,
		"direction": "horizontal",
	}, &split)
	workPane := split["paneId"].(string)

	// Start python HTTP server via start-and-watch.
	serverResult := c.callTaskTool(t, "start-and-watch", map[string]any{
		"paneId":  serverPane,
		"command": "python3 -m http.server 8765",
		"pattern": "Serving HTTP",
		"timeout": 20,
	})
	event, _ := serverResult["event"].(string)
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected pattern event from start-and-watch, got %q", event)
	}

	// Run curl in work pane.
	var curlResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  workPane,
		"command": "curl -s http://localhost:8765",
	}, &curlResult)

	output, _ := curlResult["output"].(string)
	// Python's http.server returns HTML directory listing.
	if !strings.Contains(strings.ToLower(output), "html") &&
		!strings.Contains(strings.ToLower(output), "directory") {
		t.Fatalf("expected HTML from curl, got: %q", output)
	}

	// Kill server.
	exec.Command("tmux", "send-keys", "-t", serverPane, "C-c", "").Run() //nolint:errcheck
}

// TestScenario_REPLSession simulates an agent opening a shell REPL and
// running multiple expressions across multiple run-in-repl calls.
func TestScenario_REPLSession(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Start a lightweight sh REPL with a known prompt.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "env PS1='REPL> ' sh --norc --noprofile",
		"enter":  true,
	}, &map[string]any{})
	sleep(300 * time.Millisecond)

	promptPattern := `^REPL>`

	// Expression 1: set a variable.
	var r1 map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"paneId":        paneID,
		"input":         "x=42",
		"promptPattern": promptPattern,
		"timeout":       10,
	}, &r1)

	// Expression 2: evaluate x * 2 — should produce 84.
	var r2 map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"paneId":        paneID,
		"input":         "echo $((x * 2))",
		"promptPattern": promptPattern,
		"timeout":       10,
	}, &r2)

	output, _ := r2["output"].(string)
	if !strings.Contains(output, "84") {
		t.Fatalf("expected '84' in REPL output, got: %q", output)
	}
}

// TestScenario_BuildMonitoring simulates an agent running builds and checking
// success/failure via exit code.
func TestScenario_BuildMonitoring(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Successful build.
	var success map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneID,
		"command": "echo 'Building...' && sleep 0.2 && echo 'Build complete: 0 errors'",
	}, &success)

	output, _ := success["output"].(string)
	if !strings.Contains(output, "Build complete") {
		t.Fatalf("expected 'Build complete' in output, got: %q", output)
	}
	exitCode, _ := success["exitCode"].(float64)
	if int(exitCode) != 0 {
		t.Fatalf("expected exitCode 0 for successful build, got %v", exitCode)
	}

	// Failed build.
	var failure map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneID,
		"command": "echo 'Building...' && sleep 0.2 && echo 'FATAL: compilation failed' && exit 1",
	}, &failure)

	fOutput, _ := failure["output"].(string)
	if !strings.Contains(fOutput, "FATAL") {
		t.Fatalf("expected 'FATAL' in failure output, got: %q", fOutput)
	}
	fExitCode, _ := failure["exitCode"].(float64)
	if int(fExitCode) != 1 {
		t.Fatalf("expected exitCode 1 for failed build, got %v", fExitCode)
	}
}

// TestScenario_CoachingDisplay simulates an agent using a split pane as a
// coaching display while running work in the main pane.
func TestScenario_CoachingDisplay(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	mainPane := sess["paneId"].(string)

	// Create display pane.
	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    mainPane,
		"direction": "horizontal",
	}, &split)
	displayPane := split["paneId"].(string)

	// Write first status to display.
	c.callToolJSON(t, "write-to-display", map[string]any{
		"paneId": displayPane,
		"text":   "Running tests...",
	}, &map[string]any{})

	// Run work in main pane.
	var workResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  mainPane,
		"command": "echo 'test passed'",
	}, &workResult)

	workOutput, _ := workResult["output"].(string)
	if !strings.Contains(workOutput, "test passed") {
		t.Fatalf("expected 'test passed' in work output, got: %q", workOutput)
	}

	// Update display with result (clear=true).
	c.callToolJSON(t, "write-to-display", map[string]any{
		"paneId": displayPane,
		"text":   "Tests passed!",
		"clear":  true,
	}, &map[string]any{})

	sleep(400 * time.Millisecond)

	// capture-pane on display — should contain "Tests passed".
	// Strip all whitespace when comparing: terminal line-wrapping in narrow split
	// panes can break words mid-character, so we compare without any whitespace.
	displayText := c.callToolText(t, "capture-pane", map[string]any{"paneId": displayPane})
	displayNoWS := strings.ReplaceAll(strings.ReplaceAll(displayText, "\n", ""), " ", "")
	if !strings.Contains(displayNoWS, "Testspassed") {
		t.Fatalf("expected 'Tests passed' in display pane, got:\n%s", displayText)
	}

	// capture-pane on main — should contain "test passed".
	mainText := c.callToolText(t, "capture-pane", map[string]any{"paneId": mainPane})
	if !strings.Contains(mainText, "test passed") {
		t.Fatalf("expected 'test passed' in main pane, got:\n%s", mainText)
	}
}

// TestScenario_MultiPaneOrchestration simulates creating multiple panes and
// running distinct commands in each.
func TestScenario_MultiPaneOrchestration(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneA := sess["paneId"].(string)
	windowID := sess["windowId"].(string)

	// Split pane A horizontally → pane B.
	var splitB map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneA,
		"direction": "horizontal",
	}, &splitB)
	paneB := splitB["paneId"].(string)

	// Split pane B vertically → pane C.
	var splitC map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneB,
		"direction": "vertical",
	}, &splitC)
	paneC := splitC["paneId"].(string)

	// Verify we have 3 panes.
	var panes []map[string]any
	c.callToolJSON(t, "list-panes", map[string]any{"windowId": windowID}, &panes)
	if len(panes) < 3 {
		t.Fatalf("expected at least 3 panes, got %d", len(panes))
	}

	// Run different commands in each pane.
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneA,
		"command": "echo pane-alpha",
	}, &map[string]any{})
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneB,
		"command": "echo pane-beta",
	}, &map[string]any{})
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneC,
		"command": "echo pane-gamma",
	}, &map[string]any{})

	// capture-pane each and verify distinct content.
	textA := c.callToolText(t, "capture-pane", map[string]any{"paneId": paneA})
	textB := c.callToolText(t, "capture-pane", map[string]any{"paneId": paneB})
	textC := c.callToolText(t, "capture-pane", map[string]any{"paneId": paneC})

	if !strings.Contains(textA, "pane-alpha") {
		t.Errorf("pane A: expected 'pane-alpha', got:\n%s", textA)
	}
	if !strings.Contains(textB, "pane-beta") {
		t.Errorf("pane B: expected 'pane-beta', got:\n%s", textB)
	}
	if !strings.Contains(textC, "pane-gamma") {
		t.Errorf("pane C: expected 'pane-gamma', got:\n%s", textC)
	}

	// pane-state on each — all should be alive.
	for _, paneID := range []string{paneA, paneB, paneC} {
		var state map[string]any
		c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &state)
		if state["isAlive"] != true {
			t.Errorf("pane %s: expected isAlive=true, got %v", paneID, state["isAlive"])
		}
	}
}

// TestScenario_NativeInputDetection simulates detecting when a process needs input.
func TestScenario_NativeInputDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// 1. Shell at prompt — waitingForInput=true.
	sleep(500 * time.Millisecond)
	var idleState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &idleState)
	if idleState["isAlive"] != true {
		t.Fatalf("expected isAlive=true for idle shell")
	}
	t.Logf("idle: waitingForInput=%v foregroundCmd=%v", idleState["waitingForInput"], idleState["foregroundCmd"])

	// 2. Start `cat` with no args — reads from stdin.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "cat",
		"enter":  true,
	}, &map[string]any{})
	sleep(700 * time.Millisecond)

	var catState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &catState)
	t.Logf("cat: waitingForInput=%v foregroundCmd=%v", catState["waitingForInput"], catState["foregroundCmd"])
	if catState["isAlive"] != true {
		t.Fatalf("expected isAlive=true while cat runs")
	}

	// 3. Send Ctrl-D (EOF) to cat using literal=false so tmux interprets "C-d".
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId":  paneID,
		"keys":    "C-d",
		"literal": false,
	}, &map[string]any{})
	sleep(700 * time.Millisecond)

	// 4. Back to shell.
	var shellState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &shellState)
	t.Logf("after C-d: waitingForInput=%v foregroundCmd=%v", shellState["waitingForInput"], shellState["foregroundCmd"])
	if shellState["isAlive"] != true {
		t.Fatalf("expected isAlive=true after cat exits")
	}
}

// TestScenario_ProcessLifecycle simulates tracking a full process lifecycle.
func TestScenario_ProcessLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// 1. Shell at prompt: isAlive=true.
	sleep(400 * time.Millisecond)
	var state1 map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &state1)
	if state1["isAlive"] != true {
		t.Fatalf("step 1: expected isAlive=true, got %v", state1["isAlive"])
	}
	t.Logf("step 1 (idle): foregroundCmd=%v waitingForInput=%v", state1["foregroundCmd"], state1["waitingForInput"])

	// 2. Run sleep 2: isAlive=true, foregroundCmd should be "sleep".
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "sleep 2",
		"enter":  true,
	}, &map[string]any{})
	sleep(500 * time.Millisecond)

	var state2 map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &state2)
	if state2["isAlive"] != true {
		t.Fatalf("step 2: expected isAlive=true while sleep runs, got %v", state2["isAlive"])
	}
	cmd2, _ := state2["foregroundCmd"].(string)
	t.Logf("step 2 (sleeping): foregroundCmd=%q waitingForInput=%v", cmd2, state2["waitingForInput"])
	// foregroundCmd may be "sleep" or the shell depending on timing.

	// 3. Wait for sleep to finish: isAlive=true, back to shell.
	sleep(3 * time.Second)
	var state3 map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &state3)
	if state3["isAlive"] != true {
		t.Fatalf("step 3: expected isAlive=true after sleep, got %v", state3["isAlive"])
	}
	t.Logf("step 3 (after sleep): foregroundCmd=%v waitingForInput=%v", state3["foregroundCmd"], state3["waitingForInput"])
}
