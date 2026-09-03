package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// This file is the contract, tested at the wire.
//
// Everything here asks the same question from a different side: can a pane id
// get IN, and can one get OUT? The answer has to be no through the schema, no
// through any response type, no through any error, notification or caption, and
// a request that carries one has to be REFUSED rather than ignored — because an
// ignored paneId resolves to slot 1, and on close-pane that kills it.

// theThirteenTools is the whole agentic surface, and the list is written out
// rather than counted so that adding a tool is a deliberate edit here as well as
// in the registration. A surface a consumer pins is a surface that must not grow
// by accident.
var theThirteenTools = []string{
	"capture-pane",
	"close-pane",
	"execute-command",
	"list-slots",
	"notify",
	"open-pane",
	"pane-state",
	"run-in-repl",
	"screenshot-pane",
	"send-keys",
	"start-and-watch",
	"watch-pane",
	"write-to-display",
}

// retiredArguments are the four keys no request may carry. windowId and
// sessionId never appeared on the agentic surface at all and are refused anyway:
// they were arguments of tools this release deletes, and a caller that learned
// them from an older build must be told, not silently redirected to slot 1.
var retiredArguments = []string{"paneId", "windowId", "sessionId", "headless"}

// toolSchemas fetches tools/list and returns each tool's raw input schema.
func toolSchemas(t *testing.T, c *mcpClient) map[string]map[string]any {
	t.Helper()
	raw := c.call(t, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	schemas := map[string]map[string]any{}
	for _, tool := range list.Tools {
		schemas[tool.Name] = tool.InputSchema
	}
	return schemas
}

// idPropertiesIn walks a JSON Schema to any depth and returns every retired
// property name it declares.
//
// The recursion is the point. A schema that declared paneId at the top level
// would be caught by a one-level check, and the shape nobody would catch is the
// one a future edit is most likely to produce: an object-valued property, an
// array's items, or a oneOf branch carrying it further down.
func idPropertiesIn(node any) []string {
	var found []string
	switch n := node.(type) {
	case map[string]any:
		if props, ok := n["properties"].(map[string]any); ok {
			for name, child := range props {
				for _, retired := range retiredArguments {
					if name == retired {
						found = append(found, name)
					}
				}
				found = append(found, idPropertiesIn(child)...)
			}
		}
		for key, child := range n {
			if key == "properties" {
				continue // walked above, by name
			}
			found = append(found, idPropertiesIn(child)...)
		}
	case []any:
		for _, child := range n {
			found = append(found, idPropertiesIn(child)...)
		}
	}
	return found
}

// TestSchemaHasNoIdProperties is the surface itself: exactly thirteen tools, and
// not one of them declares an argument that names a pane, window or session.
//
// The schema is where an agent LEARNS what it may send, so a property here is
// worse than a handler that accepts one: the model is being taught to reach for
// an identifier the whole design exists to keep out of its context.
func TestSchemaHasNoIdProperties(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	schemas := toolSchemas(t, c)

	for _, want := range theThirteenTools {
		if _, ok := schemas[want]; !ok {
			t.Errorf("%s is missing from tools/list", want)
		}
	}
	for got := range schemas {
		if !containsString(theThirteenTools, got) {
			t.Errorf("%s is registered and is not one of the thirteen tools", got)
		}
	}
	if len(schemas) != len(theThirteenTools) {
		t.Errorf("tools/list holds %d tools, want exactly %d", len(schemas), len(theThirteenTools))
	}

	for name, schema := range schemas {
		if found := idPropertiesIn(schema); len(found) > 0 {
			t.Errorf("%s declares %v; a schema is what teaches the model which arguments exist, so "+
				"a retired one here is worse than a handler that accepts it", name, found)
		}
	}

	// The control: the same walker, over a tool declared here that hides a paneId
	// one level down. Without it a walker that read the wrong schema key — or
	// stopped recursing — would report a clean surface for any input at all.
	fixture := mcp.NewTool("fixture",
		mcp.WithString("keys"),
		mcp.WithObject("target", mcp.Properties(map[string]any{
			"paneId": map[string]any{"type": "string"},
		})),
	)
	encoded, err := json.Marshal(fixture.InputSchema)
	if err != nil {
		t.Fatalf("marshal the fixture schema: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal the fixture schema: %v", err)
	}
	if found := idPropertiesIn(decoded); len(found) != 1 || found[0] != "paneId" {
		t.Errorf("the walker found %v in a fixture schema that nests a paneId property, so the "+
			"clean result above proves nothing: it is not reading schemas at all", found)
	}
}

// ---- Responses ----

// TestNoResponseCarriesAnId marshals every response type this server can produce
// and asserts that no key names an id and no VALUE looks like one.
//
// Two separate assertions, because the two failures are different. A forbidden
// KEY is a response type that grew a field back. A forbidden VALUE is an id that
// arrived inside a string — an error message, a detail, a caption — which is how
// every confirmed leak in this design actually escaped, and which no amount of
// key checking can see.
func TestNoResponseCarriesAnId(t *testing.T) {
	// A real error, because closedPane.Detail carries err.Error() verbatim to the
	// caller and a hand-written "boom" would prove nothing about the sentences
	// this package actually produces.
	realFailure := visibleError(errNoWindow).Error()

	for name, response := range map[string]any{
		"slotResolution":  slotResolution{Slot: 2, Created: creating(true)},
		"slotRef":         slotRef{Slot: 1},
		"paneStateResult": paneStateResult{PaneState: &PaneState{PanePID: 7, IsAlive: true}, slotRef: slotRef{Slot: 1}},
		"replResult":      replResult{slotResolution: slotResolution{Slot: 2, Created: creating(false)}, Output: "2", Exited: true},
		"execResult":      execResult{slotResolution: slotResolution{Slot: 1, Created: creating(false)}, Output: "x", ExitCode: 1, TimedOut: true},
		"WatchResult": WatchResult{
			Slot: 2, Created: creating(false), Event: "exit", Detail: realFailure,
			Output: "...", PaneState: &PaneState{PanePID: 7},
		},
		"closedPane":  closedPane{Slot: 1, Action: actionError, Detail: realFailure},
		"slotListing": slotListing{Slot: 1, Origin: originAdopted, ForegroundCmd: "zsh", IsAlive: true},
	} {
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		for _, key := range retiredArguments {
			if strings.Contains(string(data), `"`+key+`"`) {
				t.Errorf("%s carries a %q key: %s", name, key, data)
			}
		}
		if m := idInText.FindString(string(data)); m != "" {
			t.Errorf("%s carries something shaped like a multiplexer id (%q): %s", name, m, data)
		}
	}

	// Control (a): the VALUE matcher, proven independently of key detection. The
	// keys here are harmless; the ids are buried inside a sentence, which is the
	// shape of every leak this release fixed — an image caption, a progress
	// message, a channel line, an error string. An anchored regex sees none of
	// them.
	buried := struct {
		Note   string `json:"note"`
		Detail string `json:"detail"`
	}{
		Note:   "see %3 and @2 and $1",
		Detail: "can't find pane %73",
	}
	data, err := json.Marshal(buried)
	if err != nil {
		t.Fatalf("marshal the value fixture: %v", err)
	}
	if got := len(idInText.FindAllString(string(data), -1)); got != 4 {
		t.Errorf("the matcher found %d of the 4 ids embedded in %s; it cannot see an id inside a "+
			"longer string, which is where every confirmed leak was", got, data)
	}

	// Control (b): key detection, proven independently of the regex. The value is
	// harmless, so only the key can fire.
	forbidden := struct {
		PaneID string `json:"paneId"`
	}{PaneID: "a perfectly ordinary sentence"}
	data, err = json.Marshal(forbidden)
	if err != nil {
		t.Fatalf("marshal the key fixture: %v", err)
	}
	if !strings.Contains(string(data), `"paneId"`) {
		t.Fatalf("the key check cannot see a paneId key in %s", data)
	}
	if idInText.MatchString(string(data)) {
		t.Errorf("the value matcher fired on %s, which carries no id — the two controls are not "+
			"independent and either could be passing for the other's reason", data)
	}
}

// ---- Rejection ----

// argsFor returns a request body for a tool that would SUCCEED if the retired
// argument were ignored rather than refused.
//
// That is the whole design of the test. A call that fails for a missing
// argument proves nothing about rejection, and a call that would hang for a
// minute if rejection broke turns a failure into a timeout — so every required
// field is present and every wait is short.
func argsFor(tool string, retired string) map[string]any {
	args := map[string]any{
		"keys":          "echo hi",
		"command":       "true",
		"pattern":       "never-matches-anything",
		"input":         "1+1",
		"promptPattern": ">>> ",
		"text":          "hello",
		"message":       "hello",
		"timeout":       2,
		"duration":      1,
	}
	args[retired] = "%1"
	if retired == "headless" {
		args[retired] = true
	}
	return args
}

// TestRetiredArgumentsAreRejected is ground rule 3 at the wire: a request
// carrying a retired argument is an ERROR, never ignored.
//
// Refusing rather than ignoring is not strictness for its own sake. An MCP
// server that drops an unknown property resolves the call to slot 1 and
// SUCCEEDS: the caller that sent paneId gets its keystrokes delivered to a pane
// it did not name, and the caller that sent headless:true — asking for a pane
// nobody can see — gets a visible one beside the user. Both are the failure the
// refusal makes impossible.
//
// All thirteen tools and all four keys, because the rule lives in one wrapper
// and a wrapper is only as good as the number of registrations that go through
// it. close-pane, list-slots and notify are in the table for a specific reason:
// none of them resolves a slot, so a check placed inside the resolver would miss
// all three — and on close-pane an ignored paneId does not type into the wrong
// pane, it kills slot 1.
func TestRetiredArgumentsAreRejected(t *testing.T) {
	c, self := agentPaneFixture(t)

	// The state the controls read: slot 1 exists, holds a sentinel process, and
	// the window has a known number of panes.
	pane := openSlot(t, c, self, 1)
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")
	tmuxExec(t, "send-keys", "-t", pane, "cat", "Enter")
	waitForPaneBusy(t, pane)
	panesBefore := panesInWindow(t, window)

	wantMessage := map[string]string{
		"paneId":    "paneId is not accepted; address the pane by slot",
		"windowId":  "windowId is not accepted; address the pane by slot",
		"sessionId": "sessionId is not accepted; address the pane by slot",
		"headless": "headless is not accepted; open an invisible pane with isolated: true and " +
			"a slot number",
	}

	for _, tool := range theThirteenTools {
		for _, retired := range retiredArguments {
			res := callTool(t, c, tool, argsFor(tool, retired))
			if !res.IsError {
				t.Errorf("%s accepted %s; an ignored argument resolves the call to slot 1, which "+
					"is the failure the refusal exists to prevent", tool, retired)
				continue
			}
			if got := res.text(t, tool); got != wantMessage[retired] {
				t.Errorf("%s refused %s with %q, want exactly %q — the message names the property "+
					"that was sent, because a caller told \"paneId is not accepted\" after sending "+
					"windowId learns nothing about its own request", tool, retired, got, wantMessage[retired])
			}
		}
	}

	// Control (a): rejected, not ignored-and-defaulted. If the argument were
	// dropped, send-keys would have resolved to slot 1 and typed there.
	if captured := c.callToolText(t, "capture-pane", map[string]any{}); strings.Contains(captured, "LEAK") {
		t.Errorf("a rejected call still reached slot 1; the pane holds:\n%s", captured)
	}

	// Control (b): the destructive half, which a pane count cannot see. close-pane
	// with an ignored paneId defaults to slot 1 and KILLS it — or, on a pane
	// adopted from the user, C-c's it and hands it back. The sentinel process is
	// what catches the second one: a release leaves the pane alive and only the
	// process dies.
	if !paneExists(t, pane) {
		t.Fatal("a rejected close-pane killed slot 1")
	}
	if cmd := paneStateNow(t, pane).ForegroundCmd; cmd != "cat" {
		t.Errorf("slot 1's process is %q, want the sentinel \"cat\" still running: a rejected "+
			"close-pane released the pane instead of refusing the call", cmd)
	}

	// Control (c): nothing was created either. A rejected call that resolved a
	// slot on its way to the refusal would leave the user a pane to tidy up, and
	// no response would mention it.
	if after := panesInWindow(t, window); len(after) != len(panesBefore) {
		t.Errorf("the window went from %d panes to %d (%v → %v): a rejected call resolved a slot "+
			"before refusing", len(panesBefore), len(after), panesBefore, after)
	}
}

// TestRejectedSendKeysNeverTypes is control (a) above, made its own test so the
// sentinel it needs cannot collide with the process the close-pane control
// leaves running.
func TestRejectedSendKeysNeverTypes(t *testing.T) {
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)
	waitForPaneIdle(t, pane)

	res := callTool(t, c, "send-keys", map[string]any{
		"paneId": "%1", "keys": "echo LEAK", "enter": true,
	})
	if !res.IsError {
		t.Fatalf("send-keys accepted a paneId: %v", res)
	}
	sleep(500 * time.Millisecond)

	captured := c.callToolText(t, "capture-pane", map[string]any{})
	if strings.Contains(captured, "LEAK") {
		t.Errorf("the rejected send-keys typed into slot 1 anyway — which is what \"ignored\" "+
			"looks like, and it is indistinguishable from success to the caller:\n%s", captured)
	}
}

// ---- created ----

// TestCreatedIsPresentOnCreatingToolsAndAbsentOnReading pins the one field whose
// ABSENCE is meaningful.
//
// created is a discontinuity signal, not a success flag: every call succeeds, and
// what created reports is whether this slot already had a pane. An agent that
// started a dev server in slot 1 and later sees created:true has learned the only
// way it can that the user closed that pane and its process died with it. So a
// creating tool must report it every time, true or false.
//
// A reading tool must not report it at all. A field that is structurally always
// false would teach the model that a read might create — and reads must not.
func TestCreatedIsPresentOnCreatingToolsAndAbsentOnReading(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"send-keys", map[string]any{"keys": "echo created-probe", "enter": true}},
		{"execute-command", map[string]any{"command": "true"}},
		{"write-to-display", map[string]any{"text": "probe"}},
		{"open-pane", map[string]any{}},
		{"run-in-repl", map[string]any{"input": "", "promptPattern": "\\$|>|#|%", "timeout": 3}},
		{"start-and-watch", map[string]any{
			"command": "echo created-probe", "pattern": "created-probe", "timeout": 10,
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			c, _ := agentPaneFixture(t)

			// The key is read out of the RAW JSON rather than a decoded bool,
			// because a decoded bool cannot tell "absent" from "false" — which is
			// the whole distinction this test exists for.
			first := createdKey(t, callTool(t, c, tc.tool, tc.args).text(t, tc.tool))
			if first != "true" {
				t.Errorf("the first %s reported created=%s, want true: it had to open slot 1",
					tc.tool, first)
			}
			second := createdKey(t, callTool(t, c, tc.tool, tc.args).text(t, tc.tool))
			if second != "false" {
				t.Errorf("the second %s reported created=%s, want false: the slot already had a "+
					"pane, and a caller that is told otherwise concludes its process died",
					tc.tool, second)
			}
		})
	}

	t.Run("reading tools", func(t *testing.T) {
		c, self := agentPaneFixture(t)
		openSlot(t, c, self, 1)

		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"pane-state", map[string]any{}},
			{"watch-pane", map[string]any{"triggers": "pattern:never-xyzzy", "timeout": 2}},
			{"capture-pane", map[string]any{}},
			{"screenshot-pane", map[string]any{"output": "html"}},
		} {
			res := callTool(t, c, tc.tool, tc.args)
			body := res.text(t, tc.tool)
			// capture-pane and screenshot-pane answer with a pane's content, so
			// their slot rides in structuredContent; the assertion is the same
			// either way — no created key anywhere in the answer.
			if strings.Contains(body, `"created"`) {
				t.Errorf("%s reports created in its body; a reading tool must omit the key, or the "+
					"model learns that a read might create:\n%s", tc.tool, body)
			}
			if _, ok := res.StructuredContent["created"]; ok {
				t.Errorf("%s reports created in structuredContent: %v", tc.tool, res.StructuredContent)
			}
		}
	})

	t.Run("control: a plain bool cannot pass this", func(t *testing.T) {
		// The same assertion applied to a struct with `omitempty` on a plain bool
		// must FAIL to find the key when the value is false. That is the defect
		// D2's pointer removes, and without this control the assertions above
		// would be satisfied by a type that cannot express "absent".
		fixture := struct {
			Slot    int  `json:"slot"`
			Created bool `json:"created,omitempty"`
		}{Slot: 1}
		data, err := json.Marshal(fixture)
		if err != nil {
			t.Fatalf("marshal the control fixture: %v", err)
		}
		if got := createdKey(t, string(data)); got != "" {
			t.Fatalf("the fixture reported created=%s; a bool with omitempty cannot report false, "+
				"which is why created is a pointer", got)
		}
		// And the real type CAN.
		data, err = json.Marshal(slotResolution{Slot: 1, Created: creating(false)})
		if err != nil {
			t.Fatalf("marshal slotResolution: %v", err)
		}
		if got := createdKey(t, string(data)); got != "false" {
			t.Fatalf("slotResolution reported created=%q for an explicit false, want \"false\"", got)
		}
	})
}

// createdKey returns the raw JSON value of the "created" key, or "" when the key
// is absent. Working on the raw text is deliberate: decoding into a bool erases
// the difference this test is about.
func createdKey(t *testing.T, body string) string {
	t.Helper()
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		// Not a JSON body at all (capture-pane's text, an image) — no key.
		return ""
	}
	raw, ok := probe["created"]
	if !ok {
		return ""
	}
	return string(raw)
}

// ---- The channel event ----

// TestChannelEventNamesTheSlot is the bug this contract would otherwise have
// shipped.
//
// Emit fires from inside monitorPaneFrom, at four return paths, and the handlers
// used to fill the slot in after that function returned. It was invisible while
// WatchResult carried a pane id the monitor populated itself; with the id gone,
// every channel notification would have announced "slot 0" — the one field the
// event exists to carry, and the field the agent passes back to capture-pane.
//
// The controls are what make it a test rather than a coincidence: slot 0 is
// rejected explicitly, and the same watch is run on slot 3, so a hard-coded "1"
// fails as loudly as a zero.
func TestChannelEventNamesTheSlot(t *testing.T) {
	for _, slot := range []int{2, 3} {
		t.Run("slot "+strconv.Itoa(slot), func(t *testing.T) {
			c, self := agentPaneFixture(t, "--channel")
			pane := openSlot(t, c, self, slot)
			tmuxExec(t, "set-option", "-t", pane, "remain-on-exit", "on")
			drainNotifications(c)

			var result map[string]any
			c.callToolJSON(t, "start-and-watch", map[string]any{
				"slot":     slot,
				"command":  "exit 0",
				"pattern":  "never-matches-anything",
				"triggers": "exit",
				"timeout":  15,
			}, &result)

			if got, _ := result["slot"].(float64); int(got) != slot {
				t.Fatalf("start-and-watch answered for slot %v, want %d", result["slot"], slot)
			}

			notif := waitNotification(t, c, 5*time.Second)
			meta, _ := notif["meta"].(map[string]any)
			if meta == nil {
				t.Fatalf("notification missing meta: %v", notif)
			}
			gotSlot, _ := meta["slot"].(string)
			if gotSlot == "0" {
				t.Fatalf("the channel event reports slot 0: the monitor emitted before the handler "+
					"filled the slot in, so every notification names a slot that does not exist: %v", notif)
			}
			if gotSlot != strconv.Itoa(slot) {
				t.Errorf("meta.slot is %q, want %q", gotSlot, strconv.Itoa(slot))
			}
			content, _ := notif["content"].(string)
			if !strings.Contains(content, "slot "+strconv.Itoa(slot)) {
				t.Errorf("the notification text does not name slot %d: %q", slot, content)
			}
			if idInText.MatchString(content) {
				t.Errorf("the notification text carries a pane id: %q", content)
			}
		})
	}
}

// ---- The isolated socket is not touched by any of this ----

// TestNoToolReachesTheIsolatedSocket is the negative space around the surface:
// with no isolated slots in this release, nothing a tool does may start a server
// on that socket. It is cheap, and it is the probe the isolated-slot commit
// extends rather than invents.
func TestNoToolReachesTheIsolatedSocket(t *testing.T) {
	c, _ := agentPaneFixture(t)
	_ = exec.Command("tmux", "-L", headlessSocket, "kill-server").Run()

	c.callToolJSON(t, "execute-command", map[string]any{"command": "echo visible-only"},
		&map[string]any{})

	out, err := exec.Command("tmux", append(socketArgs(headlessSocket), "list-sessions")...).Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		t.Errorf("a tool started a session on the isolated socket:\n%s", out)
	}
}

// ---- tmux's own stderr ----

// TestScrubIDsKeepsTheSentence pins the one chokepoint where the multiplexer's
// own words enter this process.
//
// tmux says "can't find pane %3", and that sentence reaches the model through
// closedPane.Detail and every NewToolResultErrorFromErr in the package — a path
// no response type covers and no schema check can see. The value is replaced
// rather than the message dropped, because the rest of the sentence is the only
// diagnostic the caller gets.
func TestScrubIDsKeepsTheSentence(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"can't find pane %3", "can't find pane <pane>"},
		{"can't find window @2", "can't find window <pane>"},
		{"session not found: $1", "session not found: <pane>"},
		{"%3", "<pane>"},
		{"no server running on /tmp/tmux-501/default", "no server running on /tmp/tmux-501/default"},
		{"100% done", "100% done"},
		{"mcp_pane%3 is not an id we wrote", "mcp_pane%3 is not an id we wrote"},
	} {
		if got := scrubIDs(tc.in); got != tc.want {
			t.Errorf("scrubIDs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// The control: the scrub is reached from a REAL tmux failure, not only from a
	// unit test. A pane that does not exist is the error tmux phrases with an id
	// in it, and the wrapped error a caller would see must not carry one.
	if testing.Short() {
		t.Skip("the live half requires tmux")
	}
	_, err := newTmuxClient("bash").CapturePane(context.Background(), "%999999", 0, false)
	if err == nil {
		t.Fatal("capturing a pane that does not exist succeeded; the control proves nothing")
	}
	if idInText.MatchString(err.Error()) {
		t.Errorf("a real tmux failure carried an id to the caller: %q", err)
	}
	if !strings.Contains(err.Error(), "<pane>") {
		t.Logf("note: tmux phrased this failure without an id at all: %q", err)
	}
}
