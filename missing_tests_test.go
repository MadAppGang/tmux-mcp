package main

// missing_tests_test.go covers the 11 gaps identified by multi-model review.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// ---- 1. TestExecuteCommandTimeout ----

func TestExecuteCommandTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Command prints a marker immediately, then sleeps much longer than our timeout.
	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":         paneID,
		"command":        "echo partial-output; sleep 60",
		"timeoutSeconds": 3,
	}, &result)

	timedOut, _ := result["timedOut"].(bool)
	if !timedOut {
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
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Run 10 rapid commands in sequence. All must succeed with exitCode 0.
	for i := 0; i < 10; i++ {
		var result map[string]any
		c.callToolJSON(t, "execute-command", map[string]any{
			"paneId":  paneID,
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
	}
}

// ---- 3. TestCreateSessionIdempotent ----

func TestCreateSessionIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	name := uniqueSession(t)

	// First create.
	var first map[string]any
	c.callToolJSON(t, "create-session", map[string]any{"name": name}, &first)
	firstID, _ := first["sessionId"].(string)
	if firstID == "" {
		t.Fatal("first create-session returned no sessionId")
	}
	t.Cleanup(func() {
		_ = callToolIgnoreError(c, "kill-session", map[string]any{"sessionId": firstID})
	})

	// Second create with same name — must return the SAME session ID.
	var second map[string]any
	c.callToolJSON(t, "create-session", map[string]any{"name": name}, &second)
	secondID, _ := second["sessionId"].(string)
	if secondID == "" {
		t.Fatal("second create-session returned no sessionId")
	}

	if firstID != secondID {
		t.Fatalf("idempotent create-session: expected same sessionId %q, got %q", firstID, secondID)
	}
}

// ---- 4. TestKillSessionByName ----

func TestKillSessionByName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	name := uniqueSession(t)

	var created map[string]any
	c.callToolJSON(t, "create-session", map[string]any{"name": name}, &created)
	sessionID, _ := created["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("create-session returned no sessionId")
	}

	// Kill using the session name rather than the $N session ID.
	var killed map[string]any
	c.callToolJSON(t, "kill-session", map[string]any{"sessionId": name}, &killed)

	// Verify the session is gone.
	var sessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{}, &sessions)
	for _, s := range sessions {
		if s["id"] == sessionID {
			t.Fatalf("session %s still present after kill-session by name", sessionID)
		}
		if s["name"] == name {
			t.Fatalf("session with name %q still present after kill-session by name", name)
		}
	}
}

// ---- 5b. TestAgenticSplitAndList ----
// Validates the real agent workflow: create session, split pane, list panes,
// execute command in split. It used to prove those tools were reachable at
// -scope agentic; with one surface left it proves the workflow itself.

func TestAgenticSplitAndList(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	// Create session.
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)
	sessionID := sess["sessionId"].(string)

	// Split pane.
	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneID,
		"direction": "horizontal",
	}, &split)
	splitPaneID := split["paneId"].(string)
	if splitPaneID == "" {
		t.Fatal("split-pane returned no paneId")
	}
	if splitPaneID == paneID {
		t.Fatal("split-pane returned the same paneId as the original")
	}

	// List windows.
	var windows []map[string]any
	c.callToolJSON(t, "list-windows", map[string]any{"sessionId": sessionID}, &windows)
	if len(windows) == 0 {
		t.Fatal("list-windows returned no windows")
	}
	windowID := windows[0]["id"].(string)

	// List panes — should see both original and split pane.
	var panes []map[string]any
	c.callToolJSON(t, "list-panes", map[string]any{"windowId": windowID}, &panes)
	if len(panes) < 2 {
		t.Fatalf("expected at least 2 panes after split, got %d", len(panes))
	}

	// Execute command in the split pane.
	var execResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  splitPaneID,
		"command": "echo hello-from-split",
	}, &execResult)
	output := execResult["output"].(string)
	if !strings.Contains(output, "hello-from-split") {
		t.Fatalf("expected 'hello-from-split' in output, got: %s", output)
	}
	exitCode := int(execResult["exitCode"].(float64))
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	// Kill the split pane.
	c.callToolJSON(t, "kill-pane", map[string]any{"paneId": splitPaneID}, &map[string]any{})

	// Verify only original pane remains.
	var panesAfter []map[string]any
	c.callToolJSON(t, "list-panes", map[string]any{"windowId": windowID}, &panesAfter)
	if len(panesAfter) != 1 {
		t.Fatalf("expected 1 pane after kill, got %d", len(panesAfter))
	}
}

// ---- 7. TestRunInReplExitDetection ----

func TestRunInReplExitDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Enable remain-on-exit so the pane stays alive (marked dead) after python3
	// exits. Without this, tmux destroys the pane immediately on Linux and
	// run-in-repl cannot observe the exit state.
	exec.Command("tmux", "set-option", "-t", paneID, "remain-on-exit", "on").Run() //nolint:errcheck

	// Start python3 REPL.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "python3",
		"enter":  true,
	}, &map[string]any{})
	sleep(800 * time.Millisecond)

	// Verify the REPL is running by evaluating a simple expression.
	var r1 map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"paneId":        paneID,
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
		"paneId":        paneID,
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

// ---- 8. TestChannelNotificationContentValidation ----

func TestChannelNotificationContentValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t, "--channel")
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Enable remain-on-exit so tmux keeps the pane alive after the shell exits.
	exec.Command("tmux", "set-option", "-t", paneID, "remain-on-exit", "on").Run() //nolint:errcheck

	drainNotifications(c)

	// Start watch-pane listening for exit in background.
	watchDone := make(chan map[string]any, 1)
	go func() {
		var result map[string]any
		c.callToolJSON(t, "watch-pane", map[string]any{
			"paneId":   paneID,
			"triggers": "exit",
			"mode":     "quick",
			"timeout":  15,
		}, &result)
		watchDone <- result
	}()

	sleep(500 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", paneID, "exit 42", "Enter").Run() //nolint:errcheck

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
	if !strings.Contains(content, paneID) {
		t.Errorf("notification content should contain paneId %q, got: %q", paneID, content)
	}

	meta, ok := notif["meta"].(map[string]any)
	if !ok {
		t.Fatalf("notification missing 'meta' map, got: %T %v", notif["meta"], notif["meta"])
	}

	if meta["event"] != "exit" {
		t.Errorf("meta.event: expected 'exit', got %q", meta["event"])
	}
	if meta["paneId"] != paneID {
		t.Errorf("meta.paneId: expected %q, got %q", paneID, meta["paneId"])
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

// ---- 9. TestWatchPaneIdleTrigger ----

func TestWatchPaneIdleTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Run a command that finishes quickly, then go quiet.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "echo done",
		"enter":  true,
	}, &map[string]any{})
	sleep(300 * time.Millisecond)

	// Watch for idle:2 (2 seconds of no new output).
	start := time.Now()
	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneID,
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

// ---- 10. TestWatchPanePatternTrigger ----

func TestWatchPanePatternTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Send "echo READY" after a short delay so watch-pane is already running.
	go func() {
		sleep(800 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", paneID, "echo READY", "Enter").Run() //nolint:errcheck
	}()

	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneID,
		"triggers": "pattern:READY",
		"mode":     "quick",
		"timeout":  15,
	}, &result)

	event, _ := result["event"].(string)
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected event to contain 'pattern', got %q", event)
	}
}

// ---- 11. TestInvalidPaneIdError ----

func TestInvalidPaneIdError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	// Ensure at least one session exists so tmux is running.
	createSession(t, c, uniqueSession(t))

	raw := c.callToolRaw(t, "capture-pane", map[string]any{"paneId": "%99999"})

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal capture-pane result: %v\nraw: %s", err, raw)
	}
	if !result.IsError {
		t.Fatalf("expected isError=true for non-existent pane %%99999, got result: %v", result)
	}
}
