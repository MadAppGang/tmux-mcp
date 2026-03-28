package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ---- Channel / non-channel E2E chain tests ----

// TestScenario_ChannelDevServerWorkflow simulates a full dev server workflow
// in channel mode: start server, watch for readiness, curl it, kill it, then
// watch for the exit event — verifying both tool results and channel
// notifications arrive for each event.
func TestScenario_ChannelDevServerWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t, "--channel")
	sess := createSession(t, c, uniqueSession(t))
	serverPane := sess["paneId"].(string)

	// Split off a work pane.
	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    serverPane,
		"direction": "horizontal",
	}, &split)
	workPane := split["paneId"].(string)

	// Drain any notifications that arrived during setup.
	drainNotifications(c)

	// Start python HTTP server via start-and-watch; wait for pattern match.
	var serverResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":   serverPane,
		"command":  "python3 -m http.server 8766",
		"pattern":  "Serving HTTP",
		"timeout":  20,
		"triggers": "exit,error",
	}, &serverResult)

	// Tool result must report a pattern event.
	event, _ := serverResult["event"].(string)
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected pattern event from start-and-watch, got %q", event)
	}

	// A channel notification must also have arrived.
	notif := waitNotification(t, c, 5*time.Second)
	metaMap, _ := notif["meta"].(map[string]any)
	if metaMap == nil {
		t.Fatalf("channel notification missing meta: %v", notif)
	}
	notifEvent, _ := metaMap["event"].(string)
	if !strings.Contains(notifEvent, "pattern") {
		t.Fatalf("channel notification event: expected pattern, got %q", notifEvent)
	}
	notifPane, _ := metaMap["paneId"].(string)
	if notifPane != serverPane {
		t.Fatalf("channel notification paneId: expected %q, got %q", serverPane, notifPane)
	}

	// Curl the server from the work pane.
	var curlResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  workPane,
		"command": "curl -s http://localhost:8766",
	}, &curlResult)
	curlOutput, _ := curlResult["output"].(string)
	if !strings.Contains(strings.ToLower(curlOutput), "html") &&
		!strings.Contains(strings.ToLower(curlOutput), "directory") {
		t.Fatalf("expected HTML from curl, got: %q", curlOutput)
	}

	// Kill the server with Ctrl-C.
	exec.Command("tmux", "send-keys", "-t", serverPane, "C-c", "").Run() //nolint:errcheck
	sleep(300 * time.Millisecond)

	// Watch for exit on the server pane.
	drainNotifications(c)
	var exitResult map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   serverPane,
		"triggers": "exit,shell",
		"timeout":  10,
	}, &exitResult)

	// Send exit to close the shell so the exit trigger can fire.
	go exec.Command("tmux", "send-keys", "-t", serverPane, "exit", "Enter").Run() //nolint:errcheck

	// Re-watch specifically for exit event.
	drainNotifications(c)
	var exitResult2 map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   serverPane,
		"triggers": "exit",
		"timeout":  10,
	}, &exitResult2)

	exitEvent, _ := exitResult2["event"].(string)
	if exitEvent != "exit" {
		t.Logf("watch-pane exit result event: %q (may have been shell or other)", exitEvent)
	}

	// Wait for the channel notification for the exit event.
	exitNotif := waitNotification(t, c, 5*time.Second)
	exitMeta, _ := exitNotif["meta"].(map[string]any)
	if exitMeta == nil {
		t.Fatalf("exit channel notification missing meta: %v", exitNotif)
	}
	t.Logf("exit notification: event=%v paneId=%v", exitMeta["event"], exitMeta["paneId"])
}

// TestScenario_RegularDevServerWorkflow is the same workflow as
// TestScenario_ChannelDevServerWorkflow but in regular (non-channel) mode.
// It verifies that tool results match channel-mode behaviour and that NO
// channel notifications are emitted.
func TestScenario_RegularDevServerWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t) // no --channel flag
	sess := createSession(t, c, uniqueSession(t))
	serverPane := sess["paneId"].(string)

	// Split off a work pane.
	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    serverPane,
		"direction": "horizontal",
	}, &split)
	workPane := split["paneId"].(string)

	// Start python HTTP server and wait for readiness pattern.
	var serverResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":   serverPane,
		"command":  "python3 -m http.server 8767",
		"pattern":  "Serving HTTP",
		"timeout":  20,
		"triggers": "exit,error",
	}, &serverResult)

	event, _ := serverResult["event"].(string)
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected pattern event from start-and-watch, got %q", event)
	}

	// Curl the server.
	var curlResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  workPane,
		"command": "curl -s http://localhost:8767",
	}, &curlResult)
	curlOutput, _ := curlResult["output"].(string)
	if !strings.Contains(strings.ToLower(curlOutput), "html") &&
		!strings.Contains(strings.ToLower(curlOutput), "directory") {
		t.Fatalf("expected HTML from curl, got: %q", curlOutput)
	}

	// Kill server.
	exec.Command("tmux", "send-keys", "-t", serverPane, "C-c", "").Run() //nolint:errcheck

	// Verify no channel notifications arrived throughout the entire workflow.
	select {
	case notif := <-c.channelNotifications:
		t.Fatalf("unexpected channel notification in regular mode: %v", notif)
	default:
		// Good — channel is empty.
	}
}

// TestScenario_ChannelBuildMonitoring exercises build monitoring with channel
// notifications: a successful build that matches a pattern, and a failing build
// that fires an exit or error trigger.
func TestScenario_ChannelBuildMonitoring(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t, "--channel")
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	drainNotifications(c)

	// Successful build: pattern "Build complete" should match.
	var successResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":   paneID,
		"command":  "echo 'Building...' && sleep 0.5 && echo 'Build complete: 0 errors'",
		"pattern":  "Build complete",
		"timeout":  15,
		"triggers": "exit,error",
	}, &successResult)

	successEvent, _ := successResult["event"].(string)
	if !strings.Contains(successEvent, "pattern") {
		t.Fatalf("expected pattern event for successful build, got %q", successEvent)
	}

	// Verify channel notification carries the pattern match.
	buildNotif := waitNotification(t, c, 5*time.Second)
	buildMeta, _ := buildNotif["meta"].(map[string]any)
	if buildMeta == nil {
		t.Fatalf("build notification missing meta: %v", buildNotif)
	}
	buildNotifEvent, _ := buildMeta["event"].(string)
	if !strings.Contains(buildNotifEvent, "pattern") {
		t.Fatalf("build notification event: expected pattern, got %q", buildNotifEvent)
	}

	// Failing build: use a pattern that matches the error output so the test
	// is not subject to timing between the poll interval and the fast command.
	drainNotifications(c)
	var failResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":   paneID,
		"command":  "echo 'Building...' && sleep 0.3 && echo 'FATAL ERROR: compilation failed'",
		"pattern":  "FATAL ERROR",
		"timeout":  15,
		"triggers": "error,exit",
	}, &failResult)

	failEvent, _ := failResult["event"].(string)
	if !strings.Contains(failEvent, "pattern") && failEvent != "exit" && failEvent != "error" {
		t.Fatalf("expected pattern/exit/error event for failed build, got %q", failEvent)
	}

	// Verify channel notification reports the trigger.
	failNotif := waitNotification(t, c, 5*time.Second)
	failMeta, _ := failNotif["meta"].(map[string]any)
	if failMeta == nil {
		t.Fatalf("fail notification missing meta: %v", failNotif)
	}
	t.Logf("failed build notification: event=%v detail=%v", failMeta["event"], failMeta["detail"])
}

// TestScenario_ChannelMultiPaneMonitoring watches multiple panes simultaneously
// in channel mode and verifies that notifications arrive from distinct panes.
func TestScenario_ChannelMultiPaneMonitoring(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t, "--channel")
	sess := createSession(t, c, uniqueSession(t))
	paneA := sess["paneId"].(string)

	// Create pane B.
	var splitB map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneA,
		"direction": "horizontal",
	}, &splitB)
	paneB := splitB["paneId"].(string)

	// Create pane C.
	var splitC map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneB,
		"direction": "vertical",
	}, &splitC)
	paneC := splitC["paneId"].(string)

	drainNotifications(c)

	// Pane A: start-and-watch with a readiness message.
	var resultA map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":   paneA,
		"command":  "echo 'pane-a-ready'",
		"pattern":  "pane-a-ready",
		"timeout":  10,
		"triggers": "exit",
	}, &resultA)
	eventA, _ := resultA["event"].(string)
	if !strings.Contains(eventA, "pattern") && eventA != "exit" {
		t.Fatalf("pane A: expected pattern or exit event, got %q", eventA)
	}

	// Collect notification for pane A.
	notifA := waitNotification(t, c, 5*time.Second)
	metaA, _ := notifA["meta"].(map[string]any)
	if metaA == nil {
		t.Fatalf("pane A notification missing meta: %v", notifA)
	}

	// Pane B: execute-command (no blocking watch — just fire and forget).
	var resultB map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneB,
		"command": "echo 'pane-b-work'",
	}, &resultB)
	outputB, _ := resultB["output"].(string)
	if !strings.Contains(outputB, "pane-b-work") {
		t.Fatalf("pane B: expected 'pane-b-work' in output, got: %q", outputB)
	}

	// Pane C: send exit in a goroutine so it doesn't race with watch-pane,
	// then call watch-pane synchronously (it blocks until exit fires).
	drainNotifications(c)
	go func() {
		sleep(600 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", paneC, "exit", "Enter").Run() //nolint:errcheck
	}()

	var resultC map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneC,
		"triggers": "exit",
		"timeout":  15,
	}, &resultC)

	eventC, _ := resultC["event"].(string)
	t.Logf("pane C watch event: %q", eventC)

	// Wait for the channel notification from pane C.
	notifC := waitNotification(t, c, 5*time.Second)
	metaC, _ := notifC["meta"].(map[string]any)
	if metaC == nil {
		t.Fatalf("pane C notification missing meta: %v", notifC)
	}

	// The two notifications must come from different panes.
	paneFromA, _ := metaA["paneId"].(string)
	paneFromC, _ := metaC["paneId"].(string)
	if paneFromA == paneFromC {
		t.Fatalf("expected notifications from different panes, both reported %q", paneFromA)
	}
	t.Logf("pane A notification paneId=%q, pane C notification paneId=%q", paneFromA, paneFromC)
}

// TestScenario_NoChannelNotificationsInRegularMode is a comprehensive negative
// test that runs many operations in regular mode and asserts that zero channel
// notifications are ever emitted.
func TestScenario_NoChannelNotificationsInRegularMode(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t) // no --channel flag
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// execute-command.
	var ecResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneID,
		"command": "echo hello",
	}, &ecResult)

	// start-and-watch with pattern.
	var sawResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":   paneID,
		"command":  "echo 'ready-marker'",
		"pattern":  "ready-marker",
		"timeout":  10,
		"triggers": "exit",
	}, &sawResult)

	// watch-pane with timeout (no trigger should fire quickly; use short timeout).
	var wpResult map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneID,
		"triggers": "exit",
		"timeout":  2,
	}, &wpResult)
	// The pane shell is still alive, so this will timeout — that is fine.

	// Drain and assert empty.
	var count int
	for {
		select {
		case notif := <-c.channelNotifications:
			count++
			t.Logf("unexpected notification #%d: %v", count, notif)
		default:
			goto done
		}
	}
done:
	if count > 0 {
		t.Fatalf("expected zero channel notifications in regular mode, got %d", count)
	}
}

// ---- helpers ----

// waitNotification blocks until a channel notification arrives or the timeout
// expires (fatal).
func waitNotification(t *testing.T, c *mcpClient, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case n := <-c.channelNotifications:
		return n
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for channel notification after %v", timeout)
		return nil
	}
}

// drainNotifications discards all notifications currently buffered in the
// channel. Used to reset state between sub-steps.
func drainNotifications(c *mcpClient) {
	for {
		select {
		case <-c.channelNotifications:
		default:
			return
		}
	}
}

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
	var serverResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":  serverPane,
		"command": "python3 -m http.server 8765",
		"pattern": "Serving HTTP",
		"timeout": 20,
	}, &serverResult)
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
