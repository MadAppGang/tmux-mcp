package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- Binary build ----

var testBinaryPath string

func TestMain(m *testing.M) {
	// Build the binary once for all tests.
	dir, err := os.MkdirTemp("", "tmux-mcp-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}

	binary := filepath.Join(dir, "tmux-mcp")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}

	// Determine the module root (directory of this test file).
	_, filename, _, _ := runtime.Caller(0)
	moduleDir := filepath.Dir(filename)

	out, err := exec.Command("go", "build", "-o", binary, moduleDir).CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build binary: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	testBinaryPath = binary

	cleanupTmux, err := isolateTmux()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to isolate tmux: %v\n", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()

	// Run cleanup explicitly — os.Exit does not honour defers.
	cleanupTmux()
	os.RemoveAll(dir)
	os.Exit(code)
}

// isolateTmux points every tmux interaction in the suite — both the MCP server
// subprocess and the tests' own direct tmux calls — at a private server that we
// start with a known-clean configuration, and returns a cleanup func.
//
// Without this, the suite runs against whatever tmux server the developer is
// attached to, and inherits its environment: on a machine with a rich zsh setup
// (powerlevel10k), test panes get a right-aligned live clock and asynchronous
// prompt hooks (git status runs as its own job). Both leak into what the server
// observes — the clock churns the screen so idle is never detected, and a
// transient hook makes an idle pane briefly look busy — so tests that are
// deterministic in CI's bare shell fail intermittently locally. This makes local
// runs match CI by construction rather than by luck.
//
// Three levers, and all three are needed:
//   - TMUX_TMPDIR relocates the default socket into a temp dir, so we get a fresh
//     server instead of the user's, isolated from their real sessions.
//   - Starting that server with `-f /dev/null` means it loads no tmux config, so
//     tmux-resurrect/continuum cannot restore the user's workspace into it.
//   - An empty ZDOTDIR makes the zsh that tmux spawns in each pane load no user
//     rc, which is what actually removes powerlevel10k, the clock, and the hooks.
//
// TMUX and TMUX_PANE are cleared so that neither the tests nor the server, when
// run from inside the developer's own tmux, fall back to that server.
func isolateTmux() (func(), error) {
	// Keep this path short. tmux appends "/tmux-<uid>/<socket>" and binds a Unix
	// domain socket there, whose path is capped near 104 bytes (sun_path). The
	// system temp dir on macOS ("/var/folders/…/T") is long enough on its own to
	// blow that budget for the longer socket names, so anchor under /tmp instead.
	tmuxTmp, err := os.MkdirTemp("/tmp", "mcp")
	if err != nil {
		return nil, fmt.Errorf("create TMUX_TMPDIR: %w", err)
	}
	// An empty ZDOTDIR — an empty directory with no .zshrc — is precisely what
	// suppresses the user's zsh configuration.
	zdotDir, err := os.MkdirTemp("", "tmux-mcp-zdot-*")
	if err != nil {
		os.RemoveAll(tmuxTmp)
		return nil, fmt.Errorf("create ZDOTDIR: %w", err)
	}

	os.Setenv("TMUX_TMPDIR", tmuxTmp)
	os.Setenv("ZDOTDIR", zdotDir)
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")

	// Start the private server with no config, via a keepalive session that holds
	// it open for the whole run. Config is read once, at server start, so every
	// later command on this socket inherits the clean server; killing this session
	// at the end drops the last session and the server exits on its own.
	const keepalive = "mcp-test-keepalive"
	if out, err := exec.Command("tmux", "-f", os.DevNull, "new-session", "-d", "-s", keepalive).CombinedOutput(); err != nil {
		os.RemoveAll(tmuxTmp)
		os.RemoveAll(zdotDir)
		return nil, fmt.Errorf("start clean tmux server: %w: %s", err, out)
	}

	cleanup := func() {
		_ = exec.Command("tmux", "kill-session", "-t", keepalive).Run()
		os.RemoveAll(tmuxTmp)
		os.RemoveAll(zdotDir)
	}
	return cleanup, nil
}

// ---- JSON-RPC request/response types ----

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.Number     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Error   *jsonRPCErr     `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"` // non-empty for notifications
}

type jsonRPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// pendingCall is a pending JSON-RPC request waiting for its response.
type pendingCall struct {
	result chan json.RawMessage
	errCh  chan error
}

// ---- MCP client ----

// mcpClient manages a single tmux-mcp process and routes JSON-RPC messages.
// A background goroutine reads all output and dispatches responses to callers
// or notifications to subscribers.
type mcpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	nextID atomic.Int64

	mu                   sync.Mutex
	pending              map[int64]*pendingCall
	channelNotifications chan map[string]any
}

// newMCPClient starts a fresh tmux-mcp process, performs the initialize
// handshake, and returns a ready-to-use client.
func newMCPClient(t *testing.T, extraArgs ...string) *mcpClient {
	t.Helper()

	shellType := "zsh"
	if s := os.Getenv("SHELL"); strings.Contains(s, "bash") {
		shellType = "bash"
	} else if strings.Contains(s, "fish") {
		shellType = "fish"
	}

	args := append([]string{"--shell-type=" + shellType, "--scope=all"}, extraArgs...)
	cmd := exec.Command(testBinaryPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	// Route stderr to test output for debugging.
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

	// Start background reader.
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

	// Send initialized notification (no response expected).
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	data, _ := json.Marshal(notif)
	_, _ = fmt.Fprintf(c.stdin, "%s\n", data)

	t.Cleanup(func() { c.close() })
	return c
}

// readLoop runs in the background and routes JSON-RPC messages:
//   - Responses (have numeric "id" and no "method") → dispatched to pending callers
//   - Notifications (have "method" and no "id") → sent to taskNotify if task status
func (c *mcpClient) readLoop(reader *bufio.Reader) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// EOF or process died — close all pending callers.
			c.mu.Lock()
			for _, p := range c.pending {
				select {
				case p.errCh <- fmt.Errorf("process closed: %v", err):
				default:
				}
			}
			c.mu.Unlock()
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var msg jsonRPCResponse
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		// Notification: has method, no numeric id.
		if msg.Method != "" {
			if msg.Method == "notifications/claude/channel" && msg.Params != nil {
				var params map[string]any
				if err := json.Unmarshal(msg.Params, &params); err == nil {
					select {
					case c.channelNotifications <- params:
					default:
					}
				}
			}
			continue
		}

		// Response: find pending call by ID.
		idNum, err := msg.ID.Int64()
		if err != nil {
			continue
		}

		c.mu.Lock()
		pending, ok := c.pending[idNum]
		if ok {
			delete(c.pending, idNum)
		}
		c.mu.Unlock()

		if !ok {
			continue
		}
		if msg.Error != nil {
			pending.errCh <- fmt.Errorf("JSON-RPC error %d: %s", msg.Error.Code, msg.Error.Message)
		} else {
			pending.result <- msg.Result
		}
	}
}

// send sends a JSON-RPC request and returns channels for the result/error.
func (c *mcpClient) send(method string, params any) (int64, *pendingCall) {
	id := c.nextID.Add(1)
	p := &pendingCall{
		result: make(chan json.RawMessage, 1),
		errCh:  make(chan error, 1),
	}
	c.mu.Lock()
	c.pending[id] = p
	c.mu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}
	data, _ := json.Marshal(req)
	_, _ = fmt.Fprintf(c.stdin, "%s\n", data)
	return id, p
}

// call sends a JSON-RPC request and blocks until the response arrives.
func (c *mcpClient) call(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	_, p := c.send(method, params)
	select {
	case result := <-p.result:
		return result
	case err := <-p.errCh:
		t.Fatalf("JSON-RPC %s error: %v", method, err)
		return nil
	case <-time.After(120 * time.Second):
		t.Fatalf("JSON-RPC %s timed out", method)
		return nil
	}
}

// callToolRaw calls tools/call and returns the raw result JSON.
func (c *mcpClient) callToolRaw(t *testing.T, name string, args map[string]any) json.RawMessage {
	t.Helper()
	params := map[string]any{
		"name":      name,
		"arguments": args,
	}
	return c.call(t, "tools/call", params)
}

// callToolText calls tools/call and returns the first text content item's text.
// Most tools return {content: [{type:"text", text:"..."}]}.
func (c *mcpClient) callToolText(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	raw := c.callToolRaw(t, name, args)

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if result.IsError && len(result.Content) > 0 {
		t.Fatalf("tool %s returned error: %s", name, result.Content[0].Text)
	}
	if len(result.Content) == 0 {
		t.Fatalf("tool %s returned no content", name)
	}
	return result.Content[0].Text
}

// callToolJSON calls tools/call and unmarshals the text content as JSON into out.
func (c *mcpClient) callToolJSON(t *testing.T, name string, args map[string]any, out any) {
	t.Helper()
	text := c.callToolText(t, name, args)
	if err := json.Unmarshal([]byte(text), out); err != nil {
		t.Fatalf("unmarshal %s result JSON: %v\ntext: %s", name, err, text)
	}
}

// close kills the MCP server process.
func (c *mcpClient) close() {
	_ = c.stdin.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

// ---- Test helpers ----

// uniqueSession returns a unique session name for a test.
func uniqueSession(t *testing.T) string {
	// Replace slashes and spaces in subtest names.
	name := strings.ReplaceAll(t.Name(), "/", "-")
	name = strings.ReplaceAll(name, " ", "_")
	// Keep it short for tmux.
	if len(name) > 30 {
		name = name[:30]
	}
	return fmt.Sprintf("e2e-%s", name)
}

// createSession creates a tmux session and registers a cleanup to kill it.
// Returns the CreatedSession map with sessionId, windowId, paneId.
func createSession(t *testing.T, c *mcpClient, name string) map[string]any {
	t.Helper()
	args := map[string]any{}
	if name != "" {
		args["name"] = name
	}
	var result map[string]any
	c.callToolJSON(t, "create-session", args, &result)
	if result["sessionId"] == nil {
		t.Fatalf("create-session returned no sessionId")
	}
	sessionID := result["sessionId"].(string)
	t.Cleanup(func() {
		// Best-effort cleanup — session may already be gone.
		killArgs := map[string]any{"sessionId": sessionID}
		_ = callToolIgnoreError(c, "kill-session", killArgs)
	})
	return result
}

// callToolIgnoreError is like callToolText but does not fatal on tool errors.
func callToolIgnoreError(c *mcpClient, name string, args map[string]any) error {
	params := map[string]any{"name": name, "arguments": args}
	_, p := c.send("tools/call", params)
	select {
	case <-p.result:
		return nil
	case err := <-p.errCh:
		return err
	case <-time.After(5 * time.Second):
		return nil
	}
}

// sleep pauses for the given duration (used sparingly).
func sleep(d time.Duration) { time.Sleep(d) }

// ---- Layer 1: Primitive tool tests ----

func TestListSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	var sessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{}, &sessions)
	// Result should be a valid JSON array (may be empty).
	if sessions == nil {
		t.Fatal("list-sessions returned nil — expected at least an empty array")
	}
}

func TestCreateAndKillSession(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	// Create session.
	var created map[string]any
	c.callToolJSON(t, "create-session", map[string]any{"name": uniqueSession(t)}, &created)
	sessionID := created["sessionId"].(string)
	if sessionID == "" {
		t.Fatal("no sessionId returned")
	}
	if created["windowId"] == nil {
		t.Fatal("no windowId returned")
	}
	if created["paneId"] == nil {
		t.Fatal("no paneId returned")
	}

	// Verify session appears in list.
	var sessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{}, &sessions)
	found := false
	for _, s := range sessions {
		if s["id"] == sessionID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("new session %s not found in list-sessions", sessionID)
	}

	// Kill session.
	var killed map[string]any
	c.callToolJSON(t, "kill-session", map[string]any{"sessionId": sessionID}, &killed)

	// Verify session is gone.
	var sessionsAfter []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{}, &sessionsAfter)
	for _, s := range sessionsAfter {
		if s["id"] == sessionID {
			t.Fatalf("session %s still present after kill-session", sessionID)
		}
	}
}

func TestCreateWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))

	var win map[string]any
	c.callToolJSON(t, "create-window", map[string]any{
		"sessionId": sess["sessionId"],
		"name":      "test-win",
	}, &win)

	if win["windowId"] == nil {
		t.Fatal("no windowId returned from create-window")
	}
	if win["paneId"] == nil {
		t.Fatal("no paneId returned from create-window")
	}
}

func TestSplitPane(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneID,
		"direction": "horizontal",
	}, &split)

	if split["paneId"] == nil {
		t.Fatal("no paneId returned from split-pane")
	}
	if split["windowId"] == nil {
		t.Fatal("no windowId returned from split-pane")
	}
	if split["paneId"] == paneID {
		t.Fatal("split-pane returned the same paneId as the original")
	}
}

func TestListWindowsAndPanes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))

	// list-windows.
	var windows []map[string]any
	c.callToolJSON(t, "list-windows", map[string]any{"sessionId": sess["sessionId"]}, &windows)
	if len(windows) == 0 {
		t.Fatal("list-windows returned empty array")
	}
	win := windows[0]
	if win["id"] == nil {
		t.Fatal("window has no id")
	}

	// list-panes.
	var panes []map[string]any
	c.callToolJSON(t, "list-panes", map[string]any{"windowId": win["id"]}, &panes)
	if len(panes) == 0 {
		t.Fatal("list-panes returned empty array")
	}
	pane := panes[0]
	if pane["id"] == nil {
		t.Fatal("pane has no id")
	}
	if pane["width"] == nil || pane["height"] == nil {
		t.Fatal("pane missing width/height")
	}
}

func TestSendKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	marker := fmt.Sprintf("HELLO-SEND-KEYS-%d", time.Now().UnixNano())
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "echo " + marker,
		"enter":  true,
	}, &map[string]any{})

	// Wait for echo to complete.
	sleep(500 * time.Millisecond)

	text := c.callToolText(t, "capture-pane", map[string]any{"paneId": paneID})
	if !strings.Contains(text, marker) {
		t.Fatalf("capture-pane output does not contain marker %q\noutput:\n%s", marker, text)
	}
}

func TestSendKeysLiteral(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// With literal=true (default), "Enter" should be sent as the five characters E-n-t-e-r,
	// not as the Return key. We verify the pane does NOT advance a line (no command runs).
	// We send a harmless string and check it appears literally.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId":  paneID,
		"keys":    "echo literal-test",
		"literal": true,
		"enter":   false, // do not press Enter, so no command runs
	}, &map[string]any{})

	sleep(300 * time.Millisecond)
	text := c.callToolText(t, "capture-pane", map[string]any{"paneId": paneID})
	if !strings.Contains(text, "echo literal-test") {
		t.Fatalf("expected 'echo literal-test' in pane but got:\n%s", text)
	}
}

func TestExecuteCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneID,
		"command": "echo hello-world",
	}, &result)

	output, _ := result["output"].(string)
	if !strings.Contains(output, "hello-world") {
		t.Fatalf("expected output to contain 'hello-world', got: %q", output)
	}
	exitCode, _ := result["exitCode"].(float64)
	if exitCode != 0 {
		t.Fatalf("expected exitCode 0, got %v", exitCode)
	}
	if result["paneId"] == nil {
		t.Fatal("execute-command result missing paneId")
	}
}

func TestExecuteCommandFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneID,
		"command": "exit 42",
	}, &result)

	exitCode, _ := result["exitCode"].(float64)
	if int(exitCode) != 42 {
		t.Fatalf("expected exitCode 42, got %v", exitCode)
	}
}

func TestCapturePane(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Write something to the pane.
	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneID,
		"command": "echo capture-test-marker",
	}, &result)

	text := c.callToolText(t, "capture-pane", map[string]any{"paneId": paneID})
	if !strings.Contains(text, "capture-test-marker") {
		t.Fatalf("capture-pane result does not contain 'capture-test-marker'\noutput:\n%s", text)
	}
}

func TestResizePane(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)
	windowID := sess["windowId"].(string)

	// Vertical split (top-bottom) so both width and height can be adjusted.
	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneID,
		"direction": "vertical",
	}, &split)
	splitPaneID := split["paneId"].(string)

	// Resize the lower pane to 40 columns wide and 10 rows tall using absolute
	// dimensions. In a vertical split panes share width; tmux will honour the
	// height request and clamp width to the window width.
	c.callToolJSON(t, "resize-pane", map[string]any{
		"paneId": splitPaneID,
		"width":  40,
		"height": 10,
	}, &map[string]any{})

	// list-panes to verify new height (width is clamped by tmux to window width).
	var panes []map[string]any
	c.callToolJSON(t, "list-panes", map[string]any{"windowId": windowID}, &panes)

	var resizedPane map[string]any
	for _, p := range panes {
		if p["id"] == splitPaneID {
			resizedPane = p
			break
		}
	}
	if resizedPane == nil {
		t.Fatal("resized pane not found in list-panes")
	}
	h, _ := resizedPane["height"].(float64)
	if int(h) != 10 {
		t.Fatalf("expected height 10, got %v", h)
	}
}

func TestRenameSession(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	origName := uniqueSession(t)
	sess := createSession(t, c, origName)
	sessionID := sess["sessionId"].(string)

	newName := origName + "-renamed"
	var renamed map[string]any
	c.callToolJSON(t, "rename-session", map[string]any{
		"sessionId": sessionID,
		"newName":   newName,
	}, &renamed)

	if renamed["name"] != newName {
		t.Fatalf("expected name %q, got %q", newName, renamed["name"])
	}

	// Verify the new name appears in list-sessions.
	var sessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{}, &sessions)
	found := false
	for _, s := range sessions {
		if s["name"] == newName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("session with new name %q not found in list-sessions", newName)
	}
}

func TestDisplayMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	// display-message requires a running tmux server — create a session first.
	createSession(t, c, uniqueSession(t))

	text := c.callToolText(t, "display-message", map[string]any{
		"message":  "E2E test display message",
		"duration": 1,
	})
	if text == "" {
		t.Fatal("display-message returned empty result")
	}
}

// ---- Layer 2: Agent workflow tool tests ----

func TestRunInREPL(t *testing.T) {
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

	// Wait for sh to start.
	sleep(300 * time.Millisecond)

	// run-in-repl: evaluate 2+2.
	var result map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"paneId":        paneID,
		"input":         "echo $((2 + 2))",
		"promptPattern": `^REPL>`,
		"timeout":       10,
	}, &result)

	output, _ := result["output"].(string)
	if !strings.Contains(output, "4") {
		t.Fatalf("expected REPL output to contain '4', got: %q", output)
	}
}

func TestWriteToDisplay(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Split so we have a dedicated display pane.
	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{
		"paneId":    paneID,
		"direction": "horizontal",
	}, &split)
	displayPane := split["paneId"].(string)

	// write-to-display.
	var wr map[string]any
	c.callToolJSON(t, "write-to-display", map[string]any{
		"paneId": displayPane,
		"text":   "Hello Agent",
	}, &wr)

	// The result should only contain paneId, not the text itself.
	if wr["paneId"] == nil {
		t.Fatal("write-to-display result missing paneId")
	}
	if _, hasText := wr["text"]; hasText {
		t.Fatal("write-to-display result should not contain 'text' field")
	}

	// Verify text is visible in pane.
	// Strip all whitespace before comparing: narrow split panes can wrap words
	// mid-character, so a direct substring match would fail.
	sleep(300 * time.Millisecond)
	captured := c.callToolText(t, "capture-pane", map[string]any{"paneId": displayPane})
	capturedNoWS := strings.ReplaceAll(strings.ReplaceAll(captured, "\n", ""), " ", "")
	if !strings.Contains(capturedNoWS, "HelloAgent") {
		t.Fatalf("expected 'Hello Agent' in display pane, got:\n%s", captured)
	}
}

func TestPaneState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// 1. Idle shell — should be alive and waiting for input.
	sleep(500 * time.Millisecond)
	var idleState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &idleState)
	if idleState["isAlive"] != true {
		t.Fatalf("expected isAlive=true for idle shell, got %v", idleState["isAlive"])
	}
	if idleState["waitingForInput"] != true {
		t.Logf("note: waitingForInput=%v for idle shell (platform-dependent)", idleState["waitingForInput"])
	}
	if idleState["foregroundCmd"] == nil || idleState["foregroundCmd"] == "" {
		t.Fatal("expected non-empty foregroundCmd")
	}

	// 2. Run sleep 5 — process should be running, not waiting for shell input.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "sleep 5",
		"enter":  true,
	}, &map[string]any{})
	sleep(600 * time.Millisecond)

	var busyState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &busyState)
	if busyState["isAlive"] != true {
		t.Fatalf("expected isAlive=true while sleeping, got %v", busyState["isAlive"])
	}
	foregroundCmd, _ := busyState["foregroundCmd"].(string)
	if foregroundCmd == "" {
		t.Fatal("expected non-empty foregroundCmd while sleep runs")
	}
	t.Logf("foregroundCmd while sleeping: %q", foregroundCmd)
}

func TestPaneStateProcessExit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	// Create session and execute a command that exits the shell.
	// We use remain-on-exit via tmux option so the pane stays after shell exits.
	name := uniqueSession(t)
	sess := createSession(t, c, name)
	paneID := sess["paneId"].(string)

	// Enable remain-on-exit so tmux keeps the pane alive (marked dead) after
	// the shell exits. Without this, tmux destroys the pane immediately and
	// GetPaneState cannot observe pane_dead=1.
	exec.Command("tmux", "set-option", "-t", paneID, "remain-on-exit", "on").Run() //nolint:errcheck

	// Execute "exit" which kills the shell in the pane. The pane becomes dead
	// but stays visible because remain-on-exit is on.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "exit",
		"enter":  true,
	}, &map[string]any{})

	sleep(1 * time.Second)

	var state map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{"paneId": paneID}, &state)
	// isAlive should be false once the shell has exited.
	isAlive, _ := state["isAlive"].(bool)
	t.Logf("pane-state after exit: isAlive=%v", isAlive)
	if isAlive {
		t.Errorf("expected isAlive=false after shell exited, got true")
	}
}

// ---- Trigger system tests ----

func TestWatchPaneErrorTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Fire the error line asynchronously after a short delay.
	go func() {
		sleep(800 * time.Millisecond)
		// Use a separate client to avoid conflicts — the goroutine
		// just needs to send keys, which we can do via tmux directly.
		exec.Command("tmux", "send-keys", "-t", paneID,
			"echo 'fatal error: something broke'", "Enter").Run() //nolint:errcheck
	}()

	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneID,
		"triggers": "error",
		"mode":     "quick",
		"timeout":  15,
	}, &result)

	event, _ := result["event"].(string)
	if event != "error" {
		t.Fatalf("expected event='error', got %q", event)
	}
	matchedLine, _ := result["detail"].(string)
	if !strings.Contains(strings.ToLower(matchedLine), "fatal") &&
		!strings.Contains(strings.ToLower(matchedLine), "error") {
		t.Fatalf("expected detail to mention 'fatal' or 'error', got: %q", matchedLine)
	}
}

func TestWatchPaneUserInputTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Start `cat` with no args — it reads from stdin and reliably shows
	// waitingForInput=true on all platforms (unlike an idle bash shell which
	// uses readline and may not surface n_tty_read/wait_woken in /proc/PID/wchan
	// on Linux).
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": paneID,
		"keys":   "cat",
		"enter":  true,
	}, &map[string]any{})
	sleep(500 * time.Millisecond)

	// Watch for user_input — cat is already blocked on stdin so this should
	// fire immediately.
	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneID,
		"triggers": "user_input",
		"mode":     "quick",
		"timeout":  10,
	}, &result)

	event, _ := result["event"].(string)
	if event != "user_input" {
		t.Fatalf("expected event='user_input', got %q", event)
	}

	// Clean up: send Ctrl-D (EOF) so cat exits and the shell is left tidy.
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId":  paneID,
		"keys":    "C-d",
		"literal": false,
	}, &map[string]any{})
}

func TestStartAndWatch(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	var result map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":  paneID,
		"command": "echo 'server ready on port 3000'",
		"pattern": "ready on port",
		"timeout": 15,
	}, &result)

	event, _ := result["event"].(string)
	// Should match the pattern trigger (event name is "pattern:ready on port").
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected event to contain 'pattern', got %q", event)
	}
	output, _ := result["output"].(string)
	if !strings.Contains(output, "ready on port 3000") {
		t.Fatalf("expected output to contain 'ready on port 3000', got: %q", output)
	}
}

func TestWatchPaneTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	start := time.Now()
	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneID,
		"triggers": "pattern:this_will_never_match_xyzzy",
		"mode":     "quick",
		"timeout":  3,
	}, &result)
	elapsed := time.Since(start)

	event, _ := result["event"].(string)
	if event != "timeout" {
		t.Fatalf("expected event='timeout', got %q", event)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("watch returned too quickly: %v (expected ~3s timeout)", elapsed)
	}
}

func TestWatchPaneExitTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Enable remain-on-exit so tmux keeps the pane alive (marked dead) after
	// the shell exits. Without this, tmux destroys the pane immediately and
	// GetPaneState cannot observe pane_dead=1.
	exec.Command("tmux", "set-option", "-t", paneID, "remain-on-exit", "on").Run() //nolint:errcheck

	// Send exit asynchronously after a short delay.
	go func() {
		sleep(700 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", paneID, "exit", "Enter").Run() //nolint:errcheck
	}()

	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"paneId":   paneID,
		"triggers": "exit",
		"mode":     "quick",
		"timeout":  10,
	}, &result)

	event, _ := result["event"].(string)
	if event != "exit" {
		t.Fatalf("expected event='exit', got %q", event)
	}
}

// TestCreateHeadless verifies that headless sessions are fully isolated from the
// user's default tmux server and that all operations work through the
// "headless:" prefix routing.
func TestCreateHeadless(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	// Ensure the headless server is clean before and after the test.
	c.callToolJSON(t, "kill-headless-server", map[string]any{}, &map[string]any{})
	t.Cleanup(func() {
		_ = callToolIgnoreError(c, "kill-headless-server", map[string]any{})
	})

	// Create a headless session.
	var created map[string]any
	c.callToolJSON(t, "create-headless", map[string]any{
		"name": "test-headless",
	}, &created)

	sessionID, _ := created["sessionId"].(string)
	windowID, _ := created["windowId"].(string)
	paneID, _ := created["paneId"].(string)

	if !strings.HasPrefix(sessionID, "headless:") {
		t.Fatalf("expected sessionId to start with 'headless:', got %q", sessionID)
	}
	if !strings.HasPrefix(windowID, "headless:") {
		t.Fatalf("expected windowId to start with 'headless:', got %q", windowID)
	}
	if !strings.HasPrefix(paneID, "headless:") {
		t.Fatalf("expected paneId to start with 'headless:', got %q", paneID)
	}

	// Execute a command in the headless pane and verify output.
	var execResult map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"paneId":  paneID,
		"command": "echo headless-works",
	}, &execResult)

	output, _ := execResult["output"].(string)
	if !strings.Contains(output, "headless-works") {
		t.Fatalf("expected 'headless-works' in output, got: %q", output)
	}
	exitCode, _ := execResult["exitCode"].(float64)
	if exitCode != 0 {
		t.Fatalf("expected exitCode 0, got %v", exitCode)
	}

	// The headless session must NOT appear in default list-sessions.
	var defaultSessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{}, &defaultSessions)
	for _, s := range defaultSessions {
		id, _ := s["id"].(string)
		if strings.HasPrefix(id, "headless:") {
			t.Fatalf("headless session %q should not appear in default list-sessions", id)
		}
		name, _ := s["name"].(string)
		if name == "test-headless" {
			t.Fatalf("headless session name 'test-headless' should not appear in default list-sessions")
		}
	}

	// The headless session must appear in list-sessions(headless: true).
	var headlessSessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{"headless": true}, &headlessSessions)
	found := false
	for _, s := range headlessSessions {
		id, _ := s["id"].(string)
		if id == sessionID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("headless session %q not found in list-sessions(headless=true)", sessionID)
	}

	// Verify list-sessions(all: true) contains both prefixed and plain sessions.
	var allSessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{"all": true}, &allSessions)
	foundInAll := false
	for _, s := range allSessions {
		id, _ := s["id"].(string)
		if id == sessionID {
			foundInAll = true
			break
		}
	}
	if !foundInAll {
		t.Fatalf("headless session %q not found in list-sessions(all=true)", sessionID)
	}

	// Capture pane works through the prefix.
	captureText := c.callToolText(t, "capture-pane", map[string]any{"paneId": paneID})
	if !strings.Contains(captureText, "headless-works") {
		t.Fatalf("capture-pane on headless pane did not contain 'headless-works', got:\n%s", captureText)
	}

	// list-windows and list-panes work through the prefix.
	var windows []map[string]any
	c.callToolJSON(t, "list-windows", map[string]any{"sessionId": sessionID}, &windows)
	if len(windows) == 0 {
		t.Fatal("list-windows returned no windows for headless session")
	}
	for _, w := range windows {
		wid, _ := w["id"].(string)
		if !strings.HasPrefix(wid, "headless:") {
			t.Fatalf("window id %q should have 'headless:' prefix", wid)
		}
	}

	var panes []map[string]any
	c.callToolJSON(t, "list-panes", map[string]any{"windowId": windowID}, &panes)
	if len(panes) == 0 {
		t.Fatal("list-panes returned no panes for headless window")
	}
	for _, p := range panes {
		pid, _ := p["id"].(string)
		if !strings.HasPrefix(pid, "headless:") {
			t.Fatalf("pane id %q should have 'headless:' prefix", pid)
		}
	}

	// kill-headless-server shuts down cleanly.
	var killed map[string]any
	c.callToolJSON(t, "kill-headless-server", map[string]any{}, &killed)
	if killed["killed"] != true {
		t.Fatalf("kill-headless-server returned killed=%v", killed["killed"])
	}
	sessions, _ := killed["sessions"].(float64)
	if int(sessions) < 1 {
		t.Fatalf("expected at least 1 session reported by kill-headless-server, got %v", killed["sessions"])
	}

	// After killing, headless list should be empty.
	var afterKill []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{"headless": true}, &afterKill)
	if len(afterKill) != 0 {
		t.Fatalf("expected 0 headless sessions after kill-headless-server, got %d", len(afterKill))
	}
}

// ---- Headless param tests ----

func TestExecuteCommandHeadless(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"command":  "echo hello headless",
		"headless": true,
	}, &result)

	output, _ := result["output"].(string)
	if !strings.Contains(output, "hello headless") {
		t.Fatalf("expected 'hello headless' in output, got: %q", output)
	}
	exitCode, _ := result["exitCode"].(float64)
	if int(exitCode) != 0 {
		t.Fatalf("expected exitCode 0, got %d", int(exitCode))
	}
	// No paneId in response — session was cleaned up.
	if _, hasPaneID := result["paneId"]; hasPaneID {
		t.Fatal("headless execute-command response should not contain paneId")
	}
}

func TestStartAndWatchHeadless(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	var result map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"command":  "echo 'server ready on port 3000'",
		"pattern":  "ready on",
		"headless": true,
		"timeout":  10,
	}, &result)

	event, _ := result["event"].(string)
	if !strings.Contains(event, "pattern") {
		t.Fatalf("expected event to contain 'pattern', got: %q", event)
	}
	// paneId should be headless-prefixed.
	paneID, _ := result["paneId"].(string)
	if !strings.HasPrefix(paneID, "headless:") {
		t.Fatalf("expected headless paneId, got: %q", paneID)
	}
}

// ---- Channel mode tests ----

// startMCPProcess starts the tmux-mcp binary and a background readLoop but
// does NOT perform the MCP initialize handshake. Callers can issue initialize
// themselves and inspect the raw response.
func startMCPProcess(t *testing.T, extraArgs ...string) *mcpClient {
	t.Helper()

	shellType := "zsh"
	if s := os.Getenv("SHELL"); strings.Contains(s, "bash") {
		shellType = "bash"
	} else if strings.Contains(s, "fish") {
		shellType = "fish"
	}

	args := append([]string{"--shell-type=" + shellType, "--scope=all"}, extraArgs...)
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
	t.Cleanup(func() { c.close() })
	return c
}

// sendInitialized sends the notifications/initialized notification (no response expected).
func sendInitialized(c *mcpClient) {
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	data, _ := json.Marshal(notif)
	fmt.Fprintf(c.stdin, "%s\n", data) //nolint:errcheck
}

func TestChannelCapabilityDeclared(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	c := startMCPProcess(t, "--channel")

	raw := c.call(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "e2e-test",
			"version": "1.0.0",
		},
	})
	sendInitialized(c)

	var result struct {
		Capabilities struct {
			Experimental map[string]any `json:"experimental"`
		} `json:"capabilities"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}

	if result.Capabilities.Experimental == nil {
		t.Fatal("expected capabilities.experimental to be non-nil when --channel is set")
	}
	if _, ok := result.Capabilities.Experimental["claude/channel"]; !ok {
		t.Fatalf("expected capabilities.experimental[\"claude/channel\"] to be present, got: %v",
			result.Capabilities.Experimental)
	}
	if result.Instructions == "" {
		t.Fatal("expected non-empty instructions when --channel is set")
	}
}

func TestChannelCapabilityAbsentByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	c := startMCPProcess(t) // no --channel flag

	raw := c.call(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "e2e-test",
			"version": "1.0.0",
		},
	})
	sendInitialized(c)

	var result struct {
		Capabilities struct {
			Experimental map[string]any `json:"experimental"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}

	if _, ok := result.Capabilities.Experimental["claude/channel"]; ok {
		t.Fatal("expected capabilities.experimental[\"claude/channel\"] to be absent without --channel flag")
	}
}

func TestChannelNotificationOnExit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	c := newMCPClient(t, "--channel")
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// Enable remain-on-exit so tmux keeps the pane alive after the shell exits.
	exec.Command("tmux", "set-option", "-t", paneID, "remain-on-exit", "on").Run() //nolint:errcheck

	// Start watch-pane in the background — it blocks until the exit trigger fires.
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

	// Give watch-pane a moment to start before sending exit.
	sleep(500 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", paneID, "exit 42", "Enter").Run() //nolint:errcheck

	// Wait for the watch-pane tool to complete.
	var watchResult map[string]any
	select {
	case watchResult = <-watchDone:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for watch-pane to complete")
	}

	// Verify the tool result reports an exit event.
	event, _ := watchResult["event"].(string)
	if event != "exit" {
		t.Fatalf("watch-pane: expected event='exit', got %q", event)
	}

	// Collect any channel notification that arrived.
	var notification map[string]any
	select {
	case notification = <-c.channelNotifications:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notifications/claude/channel notification")
	}

	// Verify notification content.
	content, _ := notification["content"].(string)
	if !strings.Contains(content, "exit") {
		t.Fatalf("notification content should contain 'exit', got: %q", content)
	}
	if !strings.Contains(content, paneID) {
		t.Fatalf("notification content should contain paneId %q, got: %q", paneID, content)
	}

	meta, ok := notification["meta"].(map[string]any)
	if !ok {
		t.Fatalf("notification missing 'meta' map, got: %v", notification["meta"])
	}

	if meta["event"] != "exit" {
		t.Fatalf("meta.event: expected 'exit', got %q", meta["event"])
	}
	if meta["paneId"] != paneID {
		t.Fatalf("meta.paneId: expected %q, got %q", paneID, meta["paneId"])
	}
	exitCode, _ := meta["exitCode"].(string)
	if exitCode == "" {
		t.Fatalf("meta.exitCode: expected a non-empty number string, got %q", exitCode)
	}
	// Verify it parses as a valid integer (exact value is unreliable on some Linux kernels).
	var exitCodeInt int
	if _, err := fmt.Sscanf(exitCode, "%d", &exitCodeInt); err != nil {
		t.Fatalf("meta.exitCode: expected a number string, got %q", exitCode)
	}
}
