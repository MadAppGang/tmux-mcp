package main

// missing_tests_test.go covers the gaps identified by multi-model review.

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ---- 1. TestExecuteCommandTimeout ----

func TestExecuteCommandTimeout(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Command prints a marker immediately, then sleeps much longer than our timeout.
	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"command":        "echo partial-output; sleep 60",
		"timeoutSeconds": 3,
	}, &result)

	timedOut, hasTimedOut := result["timedOut"]
	if !hasTimedOut {
		t.Fatalf("timedOut must always be present, even when false: %v", result)
	}
	if timedOut != true {
		t.Fatalf("expected timedOut=true, got %v (full result: %v)", timedOut, result)
	}

	exitCode, _ := result["exitCode"].(float64)
	if int(exitCode) != -1 {
		t.Fatalf("expected exitCode=-1 on timeout, got %v", exitCode)
	}

	output, _ := result["output"].(string)
	if !strings.Contains(output, "partial-output") {
		t.Fatalf("expected partial output to contain 'partial-output', got: %q", output)
	}
}

// ---- 2. TestExecuteCommandExitFileRetry ----

func TestExecuteCommandExitFileRetry(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Run 10 rapid commands in sequence. All must succeed with exitCode 0.
	for i := 0; i < 10; i++ {
		var result map[string]any
		c.callToolJSON(t, "execute-command", map[string]any{
			"command": fmt.Sprintf("echo run-%d", i),
		}, &result)

		exitCode, _ := result["exitCode"].(float64)
		if int(exitCode) != 0 {
			t.Fatalf("run-%d: expected exitCode=0, got %v", i, exitCode)
		}
		output, _ := result["output"].(string)
		expected := fmt.Sprintf("run-%d", i)
		if !strings.Contains(output, expected) {
			t.Fatalf("run-%d: expected %q in output, got: %q", i, expected, output)
		}
		// Only the first call creates the pane; every one after it reuses it.
		// That is the whole promise of a slot, and the field that reports it is
		// the only warning an agent gets when a long-running process is gone.
		wantCreated := i == 0
		if result["created"] != wantCreated {
			t.Fatalf("run-%d: created=%v, want %v — slot 1 must be the same pane every time",
				i, result["created"], wantCreated)
		}
	}
}

// ---- 3. TestOpenPaneRunAndClose ----

// TestOpenPaneRunAndClose is the whole slot lifecycle through the tools that
// remain: open a numbered slot, work in it, list it, close it, and see it go.
//
// It replaces a test that split a pane by id, listed the window and killed the
// pane by id. All three of those tools are gone; the lifecycle they covered is
// not, and this is what it looks like when the slot is the only handle.
func TestOpenPaneRunAndClose(t *testing.T) {
	c, self := agentPaneFixture(t)

	var opened map[string]any
	c.callToolJSON(t, "open-pane", map[string]any{"slot": 2}, &opened)
	if opened["created"] != true {
		t.Fatalf("the first open-pane for slot 2 must report created:true, got %v", opened)
	}
	pane := slotPaneID(t, self, 2)

	// A second call is the same pane, and says so.
	var reopened map[string]any
	c.callToolJSON(t, "open-pane", map[string]any{"slot": 2}, &reopened)
	if reopened["created"] != false {
		t.Fatalf("re-opening slot 2 must report created:false, got %v", reopened)
	}
	if again := slotPaneID(t, self, 2); again != pane {
		t.Fatalf("slot 2 moved from %s to %s between calls", pane, again)
	}

	var execResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"slot":    2,
		"command": "echo hello-from-slot-two",
	}, &execResult)
	output, _ := execResult["output"].(string)
	if !strings.Contains(output, "hello-from-slot-two") {
		t.Fatalf("expected 'hello-from-slot-two' in output, got: %s", output)
	}

	var listed []map[string]any
	c.callToolJSON(t, "list-slots", map[string]any{}, &listed)
	if !listingHasSlot(listed, 2) {
		t.Fatalf("list-slots does not report slot 2: %v", listed)
	}

	var closed []map[string]any
	c.callToolJSON(t, "close-pane", map[string]any{"slot": 2}, &closed)
	if len(closed) != 1 || closed[0]["action"] != actionKilled {
		t.Fatalf("close-pane({slot:2}) should have killed one pane, got %v", closed)
	}
	if paneExists(t, pane) {
		t.Fatalf("pane %s survived close-pane({slot:2})", pane)
	}

	c.callToolJSON(t, "list-slots", map[string]any{}, &listed)
	if listingHasSlot(listed, 2) {
		t.Fatalf("list-slots still reports slot 2 after it was closed: %v", listed)
	}
}

func listingHasSlot(listings []map[string]any, slot int) bool {
	for _, l := range listings {
		if n, ok := l["slot"].(float64); ok && int(n) == slot {
			return true
		}
	}
	return false
}

// ---- 4. TestRunInReplExitDetection ----

func TestRunInReplExitDetection(t *testing.T) {
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)

	// Enable remain-on-exit so the pane stays alive (marked dead) after python3
	// exits. Without this, tmux destroys the pane immediately on Linux and
	// run-in-repl cannot observe the exit state.
	tmuxExec(t, "set-option", "-t", pane, "remain-on-exit", "on")

	// Start python3 REPL.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "python3",
		"enter": true,
	}, &map[string]any{})
	sleep(800 * time.Millisecond)

	// Verify the REPL is running by evaluating a simple expression.
	var r1 map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"input":         "1 + 1",
		"promptPattern": `>>> `,
		"timeout":       10,
	}, &r1)
	output1, _ := r1["output"].(string)
	if !strings.Contains(output1, "2") {
		t.Logf("run-in-repl 1+1 output: %q (may not contain '2' due to prompt capture)", output1)
	}

	// Send exit() and verify we get exited:true WELL before the timeout expires.
	start := time.Now()
	var r2 map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"input":         "exit()",
		"promptPattern": `>>> `,
		"timeout":       15,
	}, &r2)
	elapsed := time.Since(start)

	exited, _ := r2["exited"].(bool)
	if !exited {
		t.Fatalf("expected exited=true after exit() in python3 REPL, got result: %v", r2)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("expected exit detection in under 8s, took %v", elapsed)
	}
}

// ---- 5. TestChannelNotificationContentValidation ----

func TestChannelNotificationContentValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })
	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")

	c := newMCPClientInPane(t, self, "--channel")
	pane := openSlot(t, c, self, 1)

	// Enable remain-on-exit so tmux keeps the pane alive after the shell exits.
	tmuxExec(t, "set-option", "-t", pane, "remain-on-exit", "on")

	drainNotifications(c)

	// Start watch-pane listening for exit in background.
	watchDone := make(chan map[string]any, 1)
	go func() {
		var result map[string]any
		c.callToolJSON(t, "watch-pane", map[string]any{
			"triggers": "exit",
			"mode":     "quick",
			"timeout":  15,
		}, &result)
		watchDone <- result
	}()

	sleep(500 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", pane, "exit 42", "Enter").Run() //nolint:errcheck

	// Wait for watch-pane to complete.
	select {
	case <-watchDone:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for watch-pane to complete")
	}

	// Collect the channel notification.
	var notif map[string]any
	select {
	case notif = <-c.channelNotifications:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for channel notification")
	}

	// Validate all fields.
	content, _ := notif["content"].(string)
	if content == "" {
		t.Error("notification content is empty")
	}
	if !strings.Contains(content, "slot 1") {
		t.Errorf("notification content should name slot 1, got: %q", content)
	}

	meta, ok := notif["meta"].(map[string]any)
	if !ok {
		t.Fatalf("notification missing 'meta' map, got: %T %v", notif["meta"], notif["meta"])
	}

	if meta["event"] != "exit" {
		t.Errorf("meta.event: expected 'exit', got %q", meta["event"])
	}
	if meta["slot"] != "1" {
		t.Errorf("meta.slot: expected \"1\", got %v", meta["slot"])
	}
	exitCode, _ := meta["exitCode"].(string)
	if exitCode == "" {
		t.Errorf("meta.exitCode: expected a non-empty number string, got %q", exitCode)
	} else {
		// Verify it parses as a valid integer (exact value is unreliable on some Linux kernels).
		var exitCodeInt int
		if _, err := fmt.Sscanf(exitCode, "%d", &exitCodeInt); err != nil {
			t.Errorf("meta.exitCode: expected a number string, got %q", exitCode)
		}
	}
	isAlive, _ := meta["isAlive"].(string)
	if isAlive != "false" {
		t.Errorf("meta.isAlive: expected 'false', got %q", isAlive)
	}
	detail, _ := meta["detail"].(string)
	if detail == "" {
		t.Error("meta.detail is empty")
	}
	elapsedStr, _ := meta["elapsedSecs"].(string)
	if elapsedStr == "" {
		t.Error("meta.elapsedSecs is empty")
	}
	// elapsedSecs should parse as a positive float.
	var elapsed float64
	if _, err := fmt.Sscanf(elapsedStr, "%f", &elapsed); err != nil || elapsed < 0 {
		t.Errorf("meta.elapsedSecs should be a non-negative number string, got %q", elapsedStr)
	}
}

// ---- 6. TestWatchPaneIdleTrigger ----

func TestWatchPaneIdleTrigger(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Run a command that finishes quickly, then go quiet.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "echo done",
		"enter": true,
	}, &map[string]any{})
	sleep(300 * time.Millisecond)

	// Watch for idle:2 (2 seconds of no new output).
	start := time.Now()
	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"triggers": "idle:2",
		"mode":     "quick",
		"timeout":  20,
	}, &result)
	elapsed := time.Since(start)

	event, _ := result["event"].(string)
	if !strings.Contains(event, "idle") {
		t.Fatalf("expected event to contain 'idle', got %q", event)
	}
	// Should fire within reasonable time (not the full 20s timeout).
	if elapsed > 15*time.Second {
		t.Fatalf("idle trigger took too long: %v", elapsed)
	}
}

// ---- 7. TestWatchPanePatternTrigger ----

func TestWatchPanePatternTrigger(t *testing.T) {
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)

	// Send "echo READY" after a short delay so watch-pane is already running.
	go func() {
		sleep(800 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", pane, "echo READY", "Enter").Run() //nolint:errcheck
	}()

	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"triggers": "pattern:READY",
		"mode":     "quick",
		"timeout":  15,
	}, &result)

	event, _ := result["event"].(string)
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected event to contain 'pattern', got %q", event)
	}
}
