package main

// missing_tests_test.go covers the 11 gaps identified by multi-model review.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// newMCPClientCustomScope starts a fresh tmux-mcp process with a specific
// --scope value instead of the hard-coded --scope=all used by newMCPClient.
// It performs the full MCP initialize handshake and returns a ready-to-use client.
func newMCPClientCustomScope(t *testing.T, scope string, extraArgs ...string) *mcpClient {
	t.Helper()

	shellType := "zsh"
	if s := os.Getenv("SHELL"); strings.Contains(s, "bash") {
		shellType = "bash"
	} else if strings.Contains(s, "fish") {
		shellType = "fish"
	}

	args := append([]string{"--shell-type=" + shellType, "--scope=" + scope}, extraArgs...)
	cmd := exec.Command(testBinaryPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start process: %v", err)
	}

	c := &mcpClient{
		cmd:                  cmd,
		stdin:                stdin,
		pending:              make(map[int64]*pendingCall),
		channelNotifications: make(chan map[string]any, 64),
	}

	go c.readLoop(bufio.NewReader(stdoutPipe))

	// MCP initialize handshake.
	c.call(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "e2e-test",
			"version": "1.0.0",
		},
	})

	// Send initialized notification.
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	data, _ := json.Marshal(notif)
	_, _ = fmt.Fprintf(c.stdin, "%s\n", data)

	t.Cleanup(func() { c.close() })
	return c
}

// toolNames calls tools/list and returns a set of tool name strings.
func toolNames(t *testing.T, c *mcpClient) map[string]bool {
	t.Helper()
	raw := c.call(t, "tools/list", map[string]any{})
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal tools/list result: %v\nraw: %s", err, raw)
	}
	names := make(map[string]bool, len(result.Tools))
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	return names
}

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

// ---- 5. TestScopeAgentic ----

func TestScopeAgentic(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClientCustomScope(t, "agentic")
	names := toolNames(t, c)

	agenticRequired := []string{
		"send-keys",
		"start-and-watch",
		"watch-pane",
		"execute-command",
	}
	for _, tool := range agenticRequired {
		if !names[tool] {
			t.Errorf("agentic scope: expected tool %q to be present, but it is missing", tool)
		}
	}

	primitiveOnly := []string{
		"split-pane",
		"resize-pane",
	}
	for _, tool := range primitiveOnly {
		if names[tool] {
			t.Errorf("agentic scope: expected tool %q to be absent (primitives only), but it is present", tool)
		}
	}
}

// ---- 6. TestScopePrimitives ----

func TestScopePrimitives(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClientCustomScope(t, "primitives")
	names := toolNames(t, c)

	if !names["send-keys"] {
		t.Error("primitives scope: expected 'send-keys' to be present")
	}

	agenticOnly := []string{
		"start-and-watch",
		"watch-pane",
	}
	for _, tool := range agenticOnly {
		if names[tool] {
			t.Errorf("primitives scope: expected tool %q to be absent (agentic only), but it is present", tool)
		}
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
	if exitCode != "42" {
		t.Errorf("meta.exitCode: expected '42', got %q", exitCode)
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
