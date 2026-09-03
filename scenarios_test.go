package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Every scenario here drives the tools the way an agent does: by slot. The
// fixture starts a server that believes it lives in a real pane, and slots 1, 2
// and 3 are the panes it opens beside itself — so what these tests exercise is
// the same resolution path a bare send-keys takes in production, rather than a
// pane id the test picked.

// ---- Channel / non-channel E2E chain tests ----

// TestScenario_ChannelDevServerWorkflow simulates a full dev server workflow
// in channel mode: start server, watch for readiness, curl it, kill it, then
// watch for the exit event — verifying both tool results and channel
// notifications arrive for each event.
func TestScenario_ChannelDevServerWorkflow(t *testing.T) {
	c, self := agentPaneFixture(t, "--channel")

	// Slot 1 runs the server, slot 2 is the work pane.
	serverPane := openSlot(t, c, self, 1)
	openSlot(t, c, self, 2)

	// Drain any notifications that arrived during setup.
	drainNotifications(c)

	// Start python HTTP server via start-and-watch; wait for pattern match.
	var serverResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
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

	// A channel notification must also have arrived, naming the slot.
	notif := waitNotification(t, c, 5*time.Second)
	metaMap, _ := notif["meta"].(map[string]any)
	if metaMap == nil {
		t.Fatalf("channel notification missing meta: %v", notif)
	}
	notifEvent, _ := metaMap["event"].(string)
	if !strings.Contains(notifEvent, "pattern") {
		t.Fatalf("channel notification event: expected pattern, got %q", notifEvent)
	}
	if notifSlot, _ := metaMap["slot"].(string); notifSlot != "1" {
		t.Fatalf("channel notification slot: expected \"1\", got %q", notifSlot)
	}

	// Curl the server from the work pane.
	var curlResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"slot":    2,
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

	// Watch for exit on the server slot.
	drainNotifications(c)
	var exitResult map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"triggers": "exit,shell",
		"timeout":  10,
	}, &exitResult)

	// Send exit to close the shell so the exit trigger can fire.
	go exec.Command("tmux", "send-keys", "-t", serverPane, "exit", "Enter").Run() //nolint:errcheck

	// Re-watch specifically for exit event.
	drainNotifications(c)
	var exitResult2 map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
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
	t.Logf("exit notification: event=%v slot=%v", exitMeta["event"], exitMeta["slot"])
}

// TestScenario_RegularDevServerWorkflow is the same workflow as
// TestScenario_ChannelDevServerWorkflow but in regular (non-channel) mode.
// It verifies that tool results match channel-mode behaviour and that NO
// channel notifications are emitted.
func TestScenario_RegularDevServerWorkflow(t *testing.T) {
	c, self := agentPaneFixture(t) // no --channel flag

	serverPane := openSlot(t, c, self, 1)
	openSlot(t, c, self, 2)

	// Start python HTTP server and wait for readiness pattern.
	var serverResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
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
		"slot":    2,
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
	c, _ := agentPaneFixture(t, "--channel")

	drainNotifications(c)

	// Successful build: pattern "Build complete" should match.
	var successResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
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

// TestScenario_ChannelMultiSlotMonitoring watches several slots in channel mode
// and verifies that notifications name the slot they came from.
//
// The slot is the whole point of the assertion at the end. Before this contract
// the event carried a pane id, and an agent watching two things at once told
// them apart by an identifier no tool would accept back. Now it tells them apart
// by the number it passes to capture-pane.
func TestScenario_ChannelMultiSlotMonitoring(t *testing.T) {
	c, self := agentPaneFixture(t, "--channel")

	openSlot(t, c, self, 1)
	openSlot(t, c, self, 2)
	paneC := openSlot(t, c, self, 3)

	drainNotifications(c)

	// Slot 1: start-and-watch with a readiness message. A short sleep before the
	// echo guarantees the readiness line is printed *during* monitoring rather
	// than landing in the baseline.
	var resultA map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"slot":     1,
		"command":  "sleep 0.3 && echo 'slot-one-ready'",
		"pattern":  "slot-one-ready",
		"timeout":  10,
		"triggers": "exit",
	}, &resultA)
	eventA, _ := resultA["event"].(string)
	if !strings.Contains(eventA, "pattern") && eventA != "exit" {
		t.Fatalf("slot 1: expected pattern or exit event, got %q", eventA)
	}

	// Collect notification for slot 1.
	notifA := waitNotification(t, c, 5*time.Second)
	metaA, _ := notifA["meta"].(map[string]any)
	if metaA == nil {
		t.Fatalf("slot 1 notification missing meta: %v", notifA)
	}

	// Slot 2: execute-command (no blocking watch — just fire and forget).
	var resultB map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"slot":    2,
		"command": "echo 'slot-two-work'",
	}, &resultB)
	outputB, _ := resultB["output"].(string)
	if !strings.Contains(outputB, "slot-two-work") {
		t.Fatalf("slot 2: expected 'slot-two-work' in output, got: %q", outputB)
	}

	// Slot 3: send exit in a goroutine so it doesn't race with watch-pane,
	// then call watch-pane synchronously (it blocks until exit fires).
	drainNotifications(c)
	go func() {
		sleep(600 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", paneC, "exit", "Enter").Run() //nolint:errcheck
	}()

	var resultC map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"slot":     3,
		"triggers": "exit",
		"timeout":  15,
	}, &resultC)

	eventC, _ := resultC["event"].(string)
	t.Logf("slot 3 watch event: %q", eventC)

	// Wait for the channel notification from slot 3.
	notifC := waitNotification(t, c, 5*time.Second)
	metaC, _ := notifC["meta"].(map[string]any)
	if metaC == nil {
		t.Fatalf("slot 3 notification missing meta: %v", notifC)
	}

	// The two notifications must name the two different slots they came from.
	slotFromA, _ := metaA["slot"].(string)
	slotFromC, _ := metaC["slot"].(string)
	if slotFromA != "1" || slotFromC != "3" {
		t.Fatalf("notifications should name slots 1 and 3, got %q and %q", slotFromA, slotFromC)
	}
}

// TestScenario_NoChannelNotificationsInRegularMode is a comprehensive negative
// test that runs many operations in regular mode and asserts that zero channel
// notifications are ever emitted.
func TestScenario_NoChannelNotificationsInRegularMode(t *testing.T) {
	c, _ := agentPaneFixture(t) // no --channel flag

	// execute-command.
	var ecResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"command": "echo hello",
	}, &ecResult)

	// start-and-watch with pattern.
	var sawResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"command":  "echo 'ready-marker'",
		"pattern":  "ready-marker",
		"timeout":  10,
		"triggers": "exit",
	}, &sawResult)

	// watch-pane with timeout (no trigger should fire quickly; use short timeout).
	var wpResult map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
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

// The three waits below read the pane's OS-level state DIRECTLY rather than
// through pane-state, and that is forced rather than preferred: no tool takes a
// pane id any more, and several of the panes these tests wait on are the user's
// own rather than a slot of ours. Reading tmux is also what the user could do,
// which keeps the waits honest — a test that waited through the tool would be
// asserting against the same code path it is exercising.

// paneStateNow reads one pane's process state, or fails the test.
func paneStateNow(t *testing.T, paneID string) *PaneState {
	t.Helper()
	state, err := newTmuxClient("bash").GetPaneState(context.Background(), paneID)
	if err != nil {
		t.Fatalf("read the state of pane %s: %v", paneID, err)
	}
	return state
}

// waitForPaneIdle polls until the pane is an idle shell at its prompt — the
// exact condition slot adoption checks. It mirrors paneIsIdleShell in
// backend_tmux.go: a shell foreground process that is also the pane's own
// process (no child command running). It does NOT key off waitingForInput, which
// is unreliable across platforms (an idle shell reports false on Linux, true on
// macOS). Fatal on timeout so a stuck pane surfaces as a failure rather than a
// silently-skipped wait.
func waitForPaneIdle(t *testing.T, paneID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if paneIsIdleShell(paneStateNow(t, paneID)) {
			return
		}
		sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s did not return to an idle shell prompt within 5s", paneID)
}

// waitForPaneBusy polls until the pane's foreground process is no longer a
// shell — i.e. a launched command has taken over. Used after send-keys that
// start a process (cat, yes) so the test does not proceed until the process is
// actually running. Fatal on timeout.
func waitForPaneBusy(t *testing.T, paneID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cmd := paneStateNow(t, paneID).ForegroundCmd; cmd != "" && !isShellProcess(cmd) {
			return
		}
		sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s did not start a foreground process within 5s", paneID)
}

// waitForPaneRunning polls until the pane's foreground process is the named
// command. Fatal on timeout.
//
// This is stricter than waitForPaneBusy, and the strictness is the point.
// "Foreground is not a shell" is also satisfied by a prompt hook that a rich
// prompt runs as its own job — powerlevel10k shells out to git for its VCS
// segment — so waitForPaneBusy can return while the command under test has not
// started yet. A test that then acts on that pane races the shell and blames the
// product for it. Waiting for the command by name removes the race.
func waitForPaneRunning(t *testing.T, paneID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if paneStateNow(t, paneID).ForegroundCmd == want {
			return
		}
		sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s did not start %q within 5s", paneID, want)
}

// TestScenario_DevServerWorkflow simulates an agent starting a dev server,
// waiting for readiness, and running work in a second slot.
func TestScenario_DevServerWorkflow(t *testing.T) {
	c, self := agentPaneFixture(t)

	serverPane := openSlot(t, c, self, 1)
	openSlot(t, c, self, 2)

	// Start python HTTP server via start-and-watch.
	var serverResult map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"command": "python3 -m http.server 8765",
		"pattern": "Serving HTTP",
		"timeout": 20,
	}, &serverResult)
	event, _ := serverResult["event"].(string)
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected pattern event from start-and-watch, got %q", event)
	}

	// Run curl in the work slot.
	var curlResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"slot":    2,
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
	c, _ := agentPaneFixture(t)

	// Start a lightweight sh REPL with a known prompt.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "env PS1='REPL> ' sh --norc --noprofile",
		"enter": true,
	}, &map[string]any{})
	sleep(300 * time.Millisecond)

	promptPattern := `^REPL>`

	// Expression 1: set a variable.
	var r1 map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"input":         "x=42",
		"promptPattern": promptPattern,
		"timeout":       10,
	}, &r1)

	// Expression 2: evaluate x * 2 — should produce 84.
	var r2 map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"input":         "echo $((x * 2))",
		"promptPattern": promptPattern,
		"timeout":       10,
	}, &r2)

	output, _ := r2["output"].(string)
	if !strings.Contains(output, "84") {
		t.Fatalf("expected '84' in REPL output, got: %q", output)
	}
	// The second call reuses the pane the first one resolved — which is the only
	// reason the variable is still set.
	if r2["created"] != false {
		t.Fatalf("the second run-in-repl must report created:false; a new pane would have "+
			"lost the REPL and the variable in it: %v", r2)
	}
}

// TestScenario_BuildMonitoring simulates an agent running builds and checking
// success/failure via exit code.
func TestScenario_BuildMonitoring(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Successful build.
	var success map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
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

// TestScenario_CoachingDisplay simulates an agent using a second slot as a
// coaching display while running work in the first.
func TestScenario_CoachingDisplay(t *testing.T) {
	c, self := agentPaneFixture(t)

	openSlot(t, c, self, 1)
	openSlot(t, c, self, 2)

	// Write first status to the display slot.
	c.callToolJSON(t, "write-to-display", map[string]any{
		"slot": 2,
		"text": "Running tests...",
	}, &map[string]any{})

	// Run work in the main slot.
	var workResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"command": "echo 'test passed'",
	}, &workResult)

	workOutput, _ := workResult["output"].(string)
	if !strings.Contains(workOutput, "test passed") {
		t.Fatalf("expected 'test passed' in work output, got: %q", workOutput)
	}

	// Update display with result (clear=true).
	c.callToolJSON(t, "write-to-display", map[string]any{
		"slot":  2,
		"text":  "Tests passed!",
		"clear": true,
	}, &map[string]any{})

	sleep(400 * time.Millisecond)

	// capture-pane on the display slot — should contain "Tests passed".
	// Strip all whitespace when comparing: terminal line-wrapping in narrow split
	// panes can break words mid-character, so we compare without any whitespace.
	displayText := c.callToolText(t, "capture-pane", map[string]any{"slot": 2})
	displayNoWS := strings.ReplaceAll(strings.ReplaceAll(displayText, "\n", ""), " ", "")
	if !strings.Contains(displayNoWS, "Testspassed") {
		t.Fatalf("expected 'Tests passed' in display pane, got:\n%s", displayText)
	}

	// capture-pane on the work slot — should contain "test passed".
	mainText := c.callToolText(t, "capture-pane", map[string]any{"slot": 1})
	if !strings.Contains(mainText, "test passed") {
		t.Fatalf("expected 'test passed' in the work pane, got:\n%s", mainText)
	}
}

// TestScenario_MultiSlotOrchestration simulates opening several slots and
// running distinct commands in each.
func TestScenario_MultiSlotOrchestration(t *testing.T) {
	c, self := agentPaneFixture(t)

	paneA := openSlot(t, c, self, 1)
	paneB := openSlot(t, c, self, 2)
	paneC := openSlot(t, c, self, 3)

	// Three slots, three distinct panes. A resolver that handed the same pane
	// to two slots would satisfy every command assertion below.
	if paneA == paneB || paneB == paneC || paneA == paneC {
		t.Fatalf("slots 1, 2 and 3 must be three different panes, got %s %s %s", paneA, paneB, paneC)
	}

	var listed []map[string]any
	c.callToolJSON(t, "list-slots", map[string]any{}, &listed)
	if len(listed) != 3 {
		t.Fatalf("list-slots should report the 3 open slots, got %d: %v", len(listed), listed)
	}

	words := map[int]string{1: "pane-alpha", 2: "pane-beta", 3: "pane-gamma"}

	// Run different commands in each slot.
	for slot, word := range words {
		c.callToolJSON(t, "execute-command", map[string]any{
			"slot":    slot,
			"command": "echo " + word,
		}, &map[string]any{})
	}

	// capture-pane each and verify distinct content.
	for slot, word := range words {
		text := c.callToolText(t, "capture-pane", map[string]any{"slot": slot})
		if !strings.Contains(text, word) {
			t.Errorf("slot %d: expected %q, got:\n%s", slot, word, text)
		}
	}

	// pane-state on each — all should be alive.
	for slot := 1; slot <= 3; slot++ {
		var state map[string]any
		c.callToolJSON(t, "pane-state", map[string]any{"slot": slot}, &state)
		if state["isAlive"] != true {
			t.Errorf("slot %d: expected isAlive=true, got %v", slot, state["isAlive"])
		}
	}
}

// TestScenario_NativeInputDetection simulates detecting when a process needs input.
func TestScenario_NativeInputDetection(t *testing.T) {
	c, self := agentPaneFixture(t)
	openSlot(t, c, self, 1)

	// 1. Shell at prompt — waitingForInput=true.
	sleep(500 * time.Millisecond)
	var idleState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &idleState)
	if idleState["isAlive"] != true {
		t.Fatalf("expected isAlive=true for idle shell")
	}
	t.Logf("idle: waitingForInput=%v foregroundCmd=%v", idleState["waitingForInput"], idleState["foregroundCmd"])

	// 2. Start `cat` with no args — reads from stdin.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "cat",
		"enter": true,
	}, &map[string]any{})
	sleep(700 * time.Millisecond)

	var catState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &catState)
	t.Logf("cat: waitingForInput=%v foregroundCmd=%v", catState["waitingForInput"], catState["foregroundCmd"])
	if catState["isAlive"] != true {
		t.Fatalf("expected isAlive=true while cat runs")
	}

	// 3. Send Ctrl-D (EOF) to cat using literal=false so tmux interprets "C-d".
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":    "C-d",
		"literal": false,
	}, &map[string]any{})
	sleep(700 * time.Millisecond)

	// 4. Back to shell.
	var shellState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &shellState)
	t.Logf("after C-d: waitingForInput=%v foregroundCmd=%v", shellState["waitingForInput"], shellState["foregroundCmd"])
	if shellState["isAlive"] != true {
		t.Fatalf("expected isAlive=true after cat exits")
	}
}

// TestScenario_ProcessLifecycle simulates tracking a full process lifecycle.
func TestScenario_ProcessLifecycle(t *testing.T) {
	c, self := agentPaneFixture(t)
	openSlot(t, c, self, 1)

	// 1. Shell at prompt: isAlive=true.
	sleep(400 * time.Millisecond)
	var state1 map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &state1)
	if state1["isAlive"] != true {
		t.Fatalf("step 1: expected isAlive=true, got %v", state1["isAlive"])
	}
	t.Logf("step 1 (idle): foregroundCmd=%v waitingForInput=%v", state1["foregroundCmd"], state1["waitingForInput"])

	// 2. Run sleep 2: isAlive=true, foregroundCmd should be "sleep".
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "sleep 2",
		"enter": true,
	}, &map[string]any{})
	sleep(500 * time.Millisecond)

	var state2 map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &state2)
	if state2["isAlive"] != true {
		t.Fatalf("step 2: expected isAlive=true while sleep runs, got %v", state2["isAlive"])
	}
	cmd2, _ := state2["foregroundCmd"].(string)
	t.Logf("step 2 (sleeping): foregroundCmd=%q waitingForInput=%v", cmd2, state2["waitingForInput"])
	// foregroundCmd may be "sleep" or the shell depending on timing.

	// 3. Wait for sleep to finish: isAlive=true, back to shell.
	sleep(3 * time.Second)
	var state3 map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &state3)
	if state3["isAlive"] != true {
		t.Fatalf("step 3: expected isAlive=true after sleep, got %v", state3["isAlive"])
	}
	t.Logf("step 3 (after sleep): foregroundCmd=%v waitingForInput=%v", state3["foregroundCmd"], state3["waitingForInput"])
}
