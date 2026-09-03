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
	"strconv"
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
// handshake, and returns a ready-to-use client. The child inherits the test
// process's environment, in which isolateTmux has cleared TMUX_PANE — so the
// server believes it is not running inside tmux.
func newMCPClient(t *testing.T, extraArgs ...string) *mcpClient {
	t.Helper()
	return startMCPClient(t, nil, extraArgs...)
}

// newMCPClientInPane starts a server that believes it is running in paneID.
//
// isolateTmux clears TMUX_PANE for the whole test process, so that a developer
// running the suite from inside their own tmux does not have the server latch
// onto their real pane. Every self-location test therefore has to put the
// variable back for one child process only — t.Setenv would leak the value into
// every other server this test file starts, and os.Setenv would leak it across
// tests entirely.
//
// The failure mode when a test forgets is loud rather than silent: the server
// answers errNoWindow instead of quietly picking some other pane, so a test
// that omits the injection cannot pass by accident.
func newMCPClientInPane(t *testing.T, paneID string, extraArgs ...string) *mcpClient {
	t.Helper()
	return startMCPClient(t, []string{"TMUX_PANE=" + paneID}, extraArgs...)
}

// startMCPClient is the shared body of the two constructors above. extraEnv, if
// non-empty, is appended to the inherited environment of the child only.
func startMCPClient(t *testing.T, extraEnv []string, extraArgs ...string) *mcpClient {
	t.Helper()

	shellType := "zsh"
	if s := os.Getenv("SHELL"); strings.Contains(s, "bash") {
		shellType = "bash"
	} else if strings.Contains(s, "fish") {
		shellType = "fish"
	}

	args := append([]string{"--shell-type=" + shellType}, extraArgs...)
	cmd := exec.Command(testBinaryPath, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
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

// sleep pauses for the given duration (used sparingly).
func sleep(d time.Duration) { time.Sleep(d) }

// ---- Layer 1: Primitive tool tests ----

// slotPaneID returns the tmux id of the pane holding a slot, for the tests that
// have to reach past the tool surface: enabling remain-on-exit, sending keys out
// of band, or proving a pane is gone. No tool answers this question any more,
// which is the point of the contract — so a test that needs the id asks tmux,
// exactly as the user would.
//
// It applies the witness rule while it looks: a record counts only when
// @mcp_pane equals the pane it was read from. A helper that ignored the witness
// would happily return one of the user's panes if an option ever leaked to
// session scope, and the tests would then be asserting against the wrong pane.
func slotPaneID(t *testing.T, self string, slot int) string {
	t.Helper()
	format := "#{pane_id}\t#{" + paneOptWitness + "}\t#{" + paneOptSlot + "}"
	out := tmuxExec(t, "list-panes", "-t", self, "-F", format)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 && f[0] != "" && f[0] == f[1] && f[2] == strconv.Itoa(slot) {
			return f[0]
		}
	}
	t.Fatalf("no pane holds slot %d in the window around %s:\n%s", slot, self, out)
	return ""
}

// openSlot resolves a slot through the tool surface and returns the pane behind
// it, so a test can do both: drive the tools, and inspect what they produced.
func openSlot(t *testing.T, c *mcpClient, self string, slot int) string {
	t.Helper()
	c.callToolJSON(t, "open-pane", map[string]any{"slot": slot}, &map[string]any{})
	return slotPaneID(t, self, slot)
}

func TestSendKeys(t *testing.T) {
	c, _ := agentPaneFixture(t)

	marker := fmt.Sprintf("HELLO-SEND-KEYS-%d", time.Now().UnixNano())
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "echo " + marker,
		"enter": true,
	}, &map[string]any{})

	// Wait for echo to complete.
	sleep(500 * time.Millisecond)

	text := c.callToolText(t, "capture-pane", map[string]any{})
	if !strings.Contains(text, marker) {
		t.Fatalf("capture-pane output does not contain marker %q\noutput:\n%s", marker, text)
	}
}

func TestSendKeysLiteral(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// With literal=true (default), the text is typed and no command runs,
	// because enter is false. It must appear on screen verbatim.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":    "echo literal-test",
		"literal": true,
		"enter":   false,
	}, &map[string]any{})

	sleep(300 * time.Millisecond)
	text := c.callToolText(t, "capture-pane", map[string]any{})
	if !strings.Contains(text, "echo literal-test") {
		t.Fatalf("expected 'echo literal-test' in pane but got:\n%s", text)
	}
}

func TestExecuteCommand(t *testing.T) {
	c, _ := agentPaneFixture(t)

	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
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
	if slot, _ := result["slot"].(float64); int(slot) != 1 {
		t.Fatalf("a bare execute-command must report slot 1, got %v", result["slot"])
	}
	if _, hasCreated := result["created"]; !hasCreated {
		t.Errorf("execute-command is a creating tool, so created must be present: %v", result)
	}
	if _, hasPaneID := result["paneId"]; hasPaneID {
		t.Errorf("execute-command answered with a paneId: %v", result)
	}
}

func TestExecuteCommandFailure(t *testing.T) {
	c, _ := agentPaneFixture(t)

	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"command": "exit 42",
	}, &result)

	exitCode, _ := result["exitCode"].(float64)
	if int(exitCode) != 42 {
		t.Fatalf("expected exitCode 42, got %v", exitCode)
	}
}

func TestCapturePane(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Write something to the pane.
	var result map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"command": "echo capture-test-marker",
	}, &result)

	text := c.callToolText(t, "capture-pane", map[string]any{})
	if !strings.Contains(text, "capture-test-marker") {
		t.Fatalf("capture-pane result does not contain 'capture-test-marker'\noutput:\n%s", text)
	}
}

func TestNotify(t *testing.T) {
	c, _ := agentPaneFixture(t)

	text := c.callToolText(t, "notify", map[string]any{
		"message":  "E2E test notification",
		"duration": 1,
	})
	if text == "" {
		t.Fatal("notify returned empty result")
	}
}

// ---- Layer 2: Agent workflow tool tests ----

func TestRunInREPL(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Start a lightweight sh REPL with a known prompt.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "env PS1='REPL> ' sh --norc --noprofile",
		"enter": true,
	}, &map[string]any{})

	// Wait for sh to start.
	sleep(300 * time.Millisecond)

	// run-in-repl: evaluate 2+2.
	var result map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"input":         "echo $((2 + 2))",
		"promptPattern": `^REPL>`,
		"timeout":       10,
	}, &result)

	output, _ := result["output"].(string)
	if !strings.Contains(output, "4") {
		t.Fatalf("expected REPL output to contain '4', got: %q", output)
	}
	// exited is always present, never omitted: an absent key makes
	// `result.exited === false` unsatisfiable for the caller.
	exited, hasExited := result["exited"]
	if !hasExited {
		t.Errorf("run-in-repl must always report exited, got: %v", result)
	}
	if exited == true {
		t.Errorf("the REPL is still running, so exited must be false: %v", result)
	}
}

func TestWriteToDisplay(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Slot 2, so the coaching display is its own pane rather than the one the
	// rest of the suite runs commands in.
	var wr map[string]any
	c.callToolJSON(t, "write-to-display", map[string]any{
		"slot": 2,
		"text": "Hello Agent",
	}, &wr)

	// The result reports the slot and whether the pane is new, and nothing else:
	// the text must not come back, or the coaching display would enter the
	// model's context, which is the one thing this tool exists to avoid.
	if slot, _ := wr["slot"].(float64); int(slot) != 2 {
		t.Fatalf("write-to-display result reports slot %v, want 2", wr["slot"])
	}
	if _, hasText := wr["text"]; hasText {
		t.Fatal("write-to-display result should not contain 'text' field")
	}

	// Verify text is visible in pane.
	// Strip all whitespace before comparing: narrow split panes can wrap words
	// mid-character, so a direct substring match would fail.
	sleep(300 * time.Millisecond)
	captured := c.callToolText(t, "capture-pane", map[string]any{"slot": 2})
	capturedNoWS := strings.ReplaceAll(strings.ReplaceAll(captured, "\n", ""), " ", "")
	if !strings.Contains(capturedNoWS, "HelloAgent") {
		t.Fatalf("expected 'Hello Agent' in display pane, got:\n%s", captured)
	}
}

func TestPaneState(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// 1. Idle shell — should be alive and waiting for input.
	c.callToolJSON(t, "open-pane", map[string]any{}, &map[string]any{})
	sleep(500 * time.Millisecond)
	var idleState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &idleState)
	if idleState["isAlive"] != true {
		t.Fatalf("expected isAlive=true for idle shell, got %v", idleState["isAlive"])
	}
	if idleState["waitingForInput"] != true {
		t.Logf("note: waitingForInput=%v for idle shell (platform-dependent)", idleState["waitingForInput"])
	}
	if idleState["foregroundCmd"] == nil || idleState["foregroundCmd"] == "" {
		t.Fatal("expected non-empty foregroundCmd")
	}
	// A reading tool never reports created: the key is absent, not false.
	if _, hasCreated := idleState["created"]; hasCreated {
		t.Errorf("pane-state is a reading tool and must not report created: %v", idleState)
	}
	if slot, _ := idleState["slot"].(float64); int(slot) != 1 {
		t.Errorf("pane-state must report the slot it read, got %v", idleState["slot"])
	}

	// 2. Run sleep 5 — process should be running, not waiting for shell input.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "sleep 5",
		"enter": true,
	}, &map[string]any{})
	sleep(600 * time.Millisecond)

	var busyState map[string]any
	c.callToolJSON(t, "pane-state", map[string]any{}, &busyState)
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
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)

	// Enable remain-on-exit so tmux keeps the pane alive (marked dead) after
	// the shell exits. Without this, tmux destroys the pane immediately and
	// there is no dead pane to read.
	tmuxExec(t, "set-option", "-t", pane, "remain-on-exit", "on")

	// Execute "exit", which kills the shell in the pane. The pane becomes dead
	// but stays visible because remain-on-exit is on.
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "exit",
		"enter": true,
	}, &map[string]any{})
	waitForPaneDead(t, pane)

	// The state is read through the same code the tool uses, but against the
	// CORPSE rather than through the slot — because in this release the slot
	// cannot reach it. Resolution refuses to hand back a dead pane (it accepts
	// keystrokes and silently swallows them, which is the worst failure this
	// server can have), so pane-state({slot:1}) resolves to a fresh pane and
	// answers about that instead. The commit that stops reading tools from
	// creating is where this becomes reachable by slot again, with the corpse
	// returned deliberately: a dead slot 1 is exactly what the user is looking at.
	state := paneStateNow(t, pane)
	t.Logf("pane state after exit: isAlive=%v exitCode=%d", state.IsAlive, state.ExitCode)
	if state.IsAlive {
		t.Errorf("expected isAlive=false after the shell exited, got true")
	}
}

// ---- Trigger system tests ----

func TestWatchPaneErrorTrigger(t *testing.T) {
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)

	// Fire the error line asynchronously after a short delay, through tmux
	// rather than through a second tool call: the client is blocked in
	// watch-pane below.
	go func() {
		sleep(800 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", pane,
			"echo 'fatal error: something broke'", "Enter").Run() //nolint:errcheck
	}()

	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
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
	// watch-pane reads, so created is absent — while slot is always reported.
	if _, hasCreated := result["created"]; hasCreated {
		t.Errorf("watch-pane is a reading tool and must not report created: %v", result)
	}
	if slot, _ := result["slot"].(float64); int(slot) != 1 {
		t.Errorf("watch-pane must report the slot it watched, got %v", result["slot"])
	}
}

func TestWatchPaneUserInputTrigger(t *testing.T) {
	c, _ := agentPaneFixture(t)

	// Start `cat` with no args — it reads from stdin and reliably shows
	// waitingForInput=true on all platforms (unlike an idle bash shell which
	// uses readline and may not surface n_tty_read/wait_woken in /proc/PID/wchan
	// on Linux).
	c.callToolJSON(t, "send-keys", map[string]any{
		"keys":  "cat",
		"enter": true,
	}, &map[string]any{})
	sleep(500 * time.Millisecond)

	// Watch for user_input — cat is already blocked on stdin so this should
	// fire immediately.
	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
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
		"keys":    "C-d",
		"literal": false,
	}, &map[string]any{})
}

func TestStartAndWatch(t *testing.T) {
	c, _ := agentPaneFixture(t)

	var result map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
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
	// start-and-watch creates, so created is present and true on the first call.
	if created, hasCreated := result["created"]; !hasCreated || created != true {
		t.Errorf("first start-and-watch must report created:true, got %v", result["created"])
	}
}

func TestWatchPaneTimeout(t *testing.T) {
	c, _ := agentPaneFixture(t)

	start := time.Now()
	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
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
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)

	// Enable remain-on-exit so tmux keeps the pane alive (marked dead) after
	// the shell exits. Without this, tmux destroys the pane immediately and
	// the state read cannot observe pane_dead=1.
	tmuxExec(t, "set-option", "-t", pane, "remain-on-exit", "on")

	// Send exit asynchronously after a short delay.
	go func() {
		sleep(700 * time.Millisecond)
		exec.Command("tmux", "send-keys", "-t", pane, "exit", "Enter").Run() //nolint:errcheck
	}()

	var result map[string]any
	c.callToolJSON(t, "watch-pane", map[string]any{
		"triggers": "exit",
		"mode":     "quick",
		"timeout":  10,
	}, &result)

	event, _ := result["event"].(string)
	if event != "exit" {
		t.Fatalf("expected event='exit', got %q", event)
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

	args := append([]string{"--shell-type=" + shellType}, extraArgs...)
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

	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })
	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")

	c := newMCPClientInPane(t, self, "--channel")
	pane := openSlot(t, c, self, 1)

	// Enable remain-on-exit so tmux keeps the pane alive after the shell exits.
	tmuxExec(t, "set-option", "-t", pane, "remain-on-exit", "on")

	// Start watch-pane in the background — it blocks until the exit trigger fires.
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

	// Give watch-pane a moment to start before sending exit.
	sleep(500 * time.Millisecond)
	exec.Command("tmux", "send-keys", "-t", pane, "exit 42", "Enter").Run() //nolint:errcheck

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

	// Verify notification content. The event names the SLOT: it is the only
	// handle the agent has, and it is what the agent passes back to capture-pane
	// or close-pane when it acts on the notification.
	content, _ := notification["content"].(string)
	if !strings.Contains(content, "exit") {
		t.Fatalf("notification content should contain 'exit', got: %q", content)
	}
	if !strings.Contains(content, "slot 1") {
		t.Fatalf("notification content should name slot 1, got: %q", content)
	}
	if strings.Contains(content, pane) {
		t.Fatalf("notification content leaked the pane id %q: %q", pane, content)
	}

	meta, ok := notification["meta"].(map[string]any)
	if !ok {
		t.Fatalf("notification missing 'meta' map, got: %v", notification["meta"])
	}

	if meta["event"] != "exit" {
		t.Fatalf("meta.event: expected 'exit', got %q", meta["event"])
	}
	if meta["slot"] != "1" {
		t.Fatalf("meta.slot: expected \"1\", got %v", meta["slot"])
	}
	if _, hasPaneID := meta["paneId"]; hasPaneID {
		t.Fatalf("meta still carries a paneId: %v", meta)
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
