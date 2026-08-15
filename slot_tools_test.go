package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---- Shared helpers for the tool-level slot tests ----

// callToolResult is the shape every tool result shares, kept here because these
// tests care about isError and about the raw text, which callToolJSON hides.
type callToolResult struct {
	IsError bool `json:"isError"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent map[string]any `json:"structuredContent"`
}

func callTool(t *testing.T, c *mcpClient, name string, args map[string]any) callToolResult {
	t.Helper()
	raw := c.callToolRaw(t, name, args)
	var out callToolResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s result: %v\nraw: %s", name, err, raw)
	}
	return out
}

func (r callToolResult) text(t *testing.T, tool string) string {
	t.Helper()
	if len(r.Content) == 0 {
		t.Fatalf("%s returned no content", tool)
	}
	return r.Content[0].Text
}

// agentPaneFixture starts a server that believes it lives in a real tmux pane,
// the way one started from inside the user's terminal does, and returns the
// client plus that pane's id. Every slot the server resolves lands in this
// pane's window.
func agentPaneFixture(t *testing.T) (*mcpClient, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux")
	}
	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })

	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
	return newMCPClientInPane(t, self), self
}

// ---- The rule that must hold for every tool, including ones not yet written ----

// TestSlotAndHeadlessAlwaysConflict enumerates tools/list rather than naming
// tools, and that is the entire point of it.
//
// slot and headless:true are contradictory: a slot names a pane in the window
// this server runs in, while a headless pane lives on a separate tmux server
// with no window at all. Silently preferring either one hands the caller a pane
// in the wrong universe with no way to notice — so the pair is an error.
//
// The rule lives in one function (resolvePaneArg), but a rule is only as good as
// the number of handlers that route through it. By discovering the tools from
// the server's own schema, this test covers the tenth tool somebody adds next
// year without anyone remembering to extend it: declare a slot property, and you
// are in this test.
func TestSlotAndHeadlessAlwaysConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	raw := c.call(t, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}

	// Every required argument any slot-taking tool has, so each call fails on the
	// slot/headless contradiction rather than on a missing argument — a test that
	// passed because the call was malformed would be guarding nothing.
	args := map[string]any{
		"slot":          1,
		"headless":      true,
		"keys":          "echo hi",
		"command":       "true",
		"pattern":       "never-matches-anything",
		"input":         "1+1",
		"promptPattern": ">>> ",
		"text":          "hello",
	}

	var checked []string
	for _, tool := range list.Tools {
		if _, ok := tool.InputSchema.Properties["slot"]; !ok {
			continue
		}
		checked = append(checked, tool.Name)
		res := callTool(t, c, tool.Name, args)
		if !res.IsError {
			t.Errorf("%s accepted slot together with headless:true; the pair must be refused", tool.Name)
			continue
		}
		msg := res.text(t, tool.Name)
		if !strings.Contains(msg, "headless") {
			t.Errorf("%s rejected the call for the wrong reason — the message does not mention "+
				"headless, so something other than the argument checks refused it: %s", tool.Name, msg)
			continue
		}
		// Which of the two refusals is correct depends on the tool. A tool with
		// no headless mode at all refuses on that, and says so, which is the more
		// useful answer; only a tool that genuinely implements headless gets as
		// far as the slot-versus-headless contradiction. Both are errors, and the
		// distinction is read off the schema rather than a hand-written list so a
		// tool that gains or loses headless support cannot fall out of the test.
		if _, hasHeadless := tool.InputSchema.Properties["headless"]; hasHeadless {
			if !strings.Contains(msg, "slot") {
				t.Errorf("%s implements headless, so its refusal must be the slot/headless "+
					"contradiction and must name the slot: %s", tool.Name, msg)
			}
		}
	}

	// Ten tools take a slot, write-to-display being the most recent. The lower
	// bound catches a registration that silently stopped declaring the property,
	// which would otherwise make this test pass by checking nothing.
	if len(checked) < 10 {
		t.Errorf("only %d tools declare a slot property (%v); expected at least 10", len(checked), checked)
	}
}

// slotLiteral matches the form a description uses to show a slot value —
// slot:2, slot:"all" — which is also the form an agent copies verbatim into its
// next call. Prose like "helper slot 1" is not matched, and does not need to be:
// nothing has ever been steered wrong by a sentence, only by an example.
var slotLiteral = regexp.MustCompile(`slot:\s*"?([A-Za-z0-9_-]+)"?`)

// TestDescriptionsAdvertiseOnlySlotsThatParse is the guard for the failure this
// whole server exists to eliminate: a description that tells an agent to do
// something the code refuses.
//
// The concrete case was split-pane, which went on advertising `slot:"new"` for a
// release after parseSlotArg started rejecting it with "slot must be an integer
// from 1 to 64". An agent that read that description and followed it failed on
// every single call, and the only signal was an error message contradicting the
// documentation it had just been given — which is exactly when an agent goes
// looking for another route into the terminal, and finds raw tmux.
//
// So the rule is checked rather than remembered, and checked against the parser
// itself rather than a list of forbidden words. Every slot literal any tool
// shows, in its own description or in its arguments', is fed to the parser that
// tool actually uses. A value that survives the round trip is one an agent can
// copy; a value that does not is a lie, whatever it happens to be. That covers
// the next removed form as well as this one, and it costs nothing when a value
// is added: make it parse and the test goes green by itself.
//
// The descriptions are read from tools/list rather than from the source, because
// tools/list is what the agent is shown.
func TestDescriptionsAdvertiseOnlySlotsThatParse(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	raw := c.call(t, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			InputSchema struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}

	checked := 0
	for _, tool := range list.Tools {
		texts := []string{tool.Description}
		for _, prop := range tool.InputSchema.Properties {
			texts = append(texts, prop.Description)
		}
		for _, text := range texts {
			for _, m := range slotLiteral.FindAllStringSubmatch(text, -1) {
				value := m[1]
				checked++
				// close-pane parses its own argument, because it is the one tool
				// whose slot is an integer-or-"all" union. Every other tool goes
				// through parseSlotArg, so "all" advertised anywhere else is as
				// wrong as "new" is here.
				var err error
				if tool.Name == "close-pane" {
					_, _, _, err = parseCloseSlotArg(slotRequest(value))
				} else {
					_, _, err = parseSlotArg(slotRequest(value))
				}
				if err != nil {
					t.Errorf("%s advertises %s, and its own parser refuses it: %v\n"+
						"An agent that reads this description and follows it fails every call, and a "+
						"tool that contradicts its own documentation is what sends agents to raw tmux.",
						tool.Name, m[0], err)
				}
			}
		}
	}

	// The lower bound is what stops this passing by finding nothing — a regex
	// that stopped matching, or a schema that stopped carrying descriptions,
	// would otherwise look like a clean run. split-pane alone shows two.
	if checked < 3 {
		t.Errorf("only %d slot literals were found in any tool description; the descriptions that "+
			"teach an agent how to name a pane have gone missing, or this test has stopped "+
			"recognising them", checked)
	}
}

// ---- No consumer breaks ----

// TestExplicitPaneIdResponsesAreUnchanged is the evidence for "paneId keeps
// working everywhere", checked at the wire level rather than argued.
//
// send-keys is pinned to its exact bytes because it is the smallest response in
// the server and the one most likely to be parsed by something rigid. The others
// are checked for the absence of the new keys: omitempty is what makes a call
// that named its pane produce the object it produced before slots existed, and
// omitempty is easy to lose in an edit.
//
// # The byte comparison is deliberate and is not to be relaxed
//
// Comparing against pretty-printed bytes looks like over-coupling — the reflex
// is to unmarshal both sides and compare maps — and that reflex is wrong here.
// The promise made to existing consumers of an explicit paneId is a WIRE
// promise, not a semantic one: a caller that string-matches this response, or
// diffs it against a golden file, or feeds it to a model that was shown the old
// shape, breaks the day jsonResult swaps MarshalIndent for Marshal, even though
// every field survives. Nothing else in the suite would notice that swap.
//
// So jsonResult's formatting — two-space indent, one key per line, no trailing
// newline — is part of the published contract, and this line is where that is
// recorded. Changing it is a deliberate compatibility break that needs a version
// bump and a note to consumers; it is not a test to loosen on the way past.
//
// start-and-watch is here because WatchResult is a fourth response shape — its
// own struct, not the shared paneResolution the other three embed — so nothing
// else in this test would notice if its Slot and Created lost their omitempty.
// It is the shape agents block on for minutes at a time, which makes it the
// worst one to break and, until this was added, the only one nobody watched.
func TestExplicitPaneIdResponsesAreUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	sess := createSession(t, c, uniqueSession(t))
	paneID := sess["paneId"].(string)

	// enter:true is not incidental. send-keys without it leaves the text sitting
	// unsubmitted in the shell's line editor, and the next tool call to the same
	// pane would have its command concatenated onto it — the hazard documented on
	// canAcquire, reproduced here by accident the first time this test was
	// written, where it hung the execute-command below for two minutes.
	got := callTool(t, c, "send-keys", map[string]any{
		"paneId": paneID, "keys": "echo hi", "enter": true,
	}).text(t, "send-keys")
	want := fmt.Sprintf("{\n  \"paneId\": %q\n}", paneID)
	if got != want {
		t.Errorf("send-keys with an explicit paneId answered:\n%s\nwant exactly:\n%s", got, want)
	}
	waitForPaneIdle(t, c, paneID)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"split-pane", map[string]any{"paneId": paneID}},
		{"execute-command", map[string]any{"paneId": paneID, "command": "true"}},
		{"pane-state", map[string]any{"paneId": paneID}},
	} {
		var out map[string]any
		c.callToolJSON(t, tc.tool, tc.args, &out)
		for _, key := range []string{"slot", "created"} {
			if _, ok := out[key]; ok {
				t.Errorf("%s with an explicit paneId returned a %q key (%v); the resolution fields "+
					"must be omitted when the caller named the pane", tc.tool, key, out)
			}
		}
	}

	// The two tools whose body is not JSON. capture-pane answers with the pane's
	// exact text and screenshot-pane with an image, so neither can carry the
	// resolution in its content and both put it in structuredContent instead —
	// which is a TOP-LEVEL key of the tool result, and one that neither tool had
	// before slots existed. paneResolution.PaneID has no omitempty and cannot
	// have one (a resolved call must always say which pane it picked), so an
	// unconditional assignment hands a caller that named its own pane a new key
	// containing the id it just passed in. That is not additive metadata, it is a
	// changed response shape, and it shipped because this test watched neither
	// tool.
	//
	// screenshot-pane is asked for html rather than its default PNG so the
	// assertion cannot be skipped on a machine without headless Chrome — the
	// resolution is attached the same way in every output mode.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"capture-pane", map[string]any{"paneId": paneID}},
		{"screenshot-pane", map[string]any{"paneId": paneID, "output": "html"}},
	} {
		res := callTool(t, c, tc.tool, tc.args)
		if res.IsError {
			t.Fatalf("%s errored: %s", tc.tool, res.text(t, tc.tool))
		}
		if res.StructuredContent != nil {
			t.Errorf("%s with an explicit paneId answered with structuredContent %v; this tool had "+
				"no such key before slots existed, and a call that names its pane must get back "+
				"exactly what it always got", tc.tool, res.StructuredContent)
		}
	}

	waitForPaneIdle(t, c, paneID)

	// The WatchResult shape. The pattern is matched by the command's own output
	// rather than by its echo — dropEcho removes the echo — so this returns in
	// well under a second and the timeout is only there to bound a failure.
	var watch map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":  paneID,
		"command": "echo watch-compat",
		"pattern": "watch-compat",
		"timeout": 15,
	}, &watch)
	if watch["paneId"] != paneID {
		t.Errorf("start-and-watch with an explicit paneId answered for pane %v, want %s",
			watch["paneId"], paneID)
	}
	if event, _ := watch["event"].(string); !strings.Contains(event, "watch-compat") {
		t.Fatalf("start-and-watch did not match its own pattern (event %q); the assertion below "+
			"would then be checking a timeout rather than a real answer: %v", event, watch)
	}
	for _, key := range []string{"slot", "created"} {
		if _, ok := watch[key]; ok {
			t.Errorf("start-and-watch with an explicit paneId returned a %q key (%v); WatchResult's "+
				"resolution fields must be omitted when the caller named the pane", key, watch)
		}
	}
}

// ---- Resolution order ----

// panesInWindow lists the panes tmux currently has in a window, in its order.
//
// Several tests below assert that a call did NOT create a pane, and a count of
// the window is the only way to see that: a pane created and then not returned
// is invisible in every response but perfectly visible to the user, who has to
// close it by hand.
func panesInWindow(t *testing.T, windowID string) []string {
	t.Helper()
	return strings.Split(tmuxExec(t, "list-panes", "-t", windowID, "-F", "#{pane_id}"), "\n")
}

// TestExplicitPaneIdBeatsSlot pins the first line of §8's resolution order —
// paneId wins, verbatim — for the one call shape nothing else covers: BOTH
// arguments given at once.
//
// Every other test passes one or the other, so a resolver that read the slot
// "as well, just in case" would be green everywhere today. It is not a
// hypothetical shape either: an agent that has learned to pass slot on every
// call, and then learns a concrete paneId, sends both — and the cost of
// resolving the slot anyway is paid in the user's window. Slot 2 with no slot-1
// pane present splits the agent's own pane, so the user watches a pane appear,
// with a shell in it, titled "agent", for a call that named the pane it wanted
// and had no need of a second one. Nothing in the response would mention it.
//
// The three assertions are one claim from three sides: the response names the
// pane the caller named and carries no resolution fields; the window has the
// same panes it had before; and no pane anywhere in it carries a slot marker,
// which is what a resolution would have written. The last one also catches the
// subtler version, where an idle pane is ADOPTED for the slot rather than
// created — no new pane, no new keys in the response, and one of the user's
// shells quietly claimed.
func TestExplicitPaneIdBeatsSlot(t *testing.T) {
	c, self := agentPaneFixture(t)
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	// The user's own pane, made with raw tmux so the server has never marked it.
	// It is both the pane being named and — being idle and unowned — exactly what
	// an unwanted slot resolution would reach for.
	usersPane := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	waitForPaneIdle(t, c, usersPane)
	before := panesInWindow(t, window)

	var out map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{
		"paneId": usersPane, "slot": 2, "keys": "echo pinned-by-paneid", "enter": true,
	}, &out)

	if got, _ := out["paneId"].(string); got != usersPane {
		t.Fatalf("send-keys({paneId, slot}) answered for pane %v, want the named pane %s",
			out["paneId"], usersPane)
	}
	for _, key := range []string{"slot", "created"} {
		if _, ok := out[key]; ok {
			t.Errorf("send-keys returned a %q key (%v) for a call that named its pane; the slot was "+
				"not resolved, so there is no slot to report", key, out)
		}
	}

	after := panesInWindow(t, window)
	if len(after) != len(before) {
		t.Errorf("the window went from %d panes to %d (%v → %v): the slot was resolved as well as "+
			"the paneId, and the caller was charged a pane it never asked for",
			len(before), len(after), before, after)
	}
	for _, pane := range after {
		if got := tmuxExec(t, "display-message", "-p", "-t", pane, "#{"+paneOptSlot+"}"); got != "" {
			t.Errorf("pane %s carries %s=%q after a call that named its pane explicitly; slot 2 was "+
				"resolved behind the caller's back, and if it was adopted rather than created it is "+
				"one of the user's shells", pane, paneOptSlot, got)
		}
	}

	// And the keys went where they were addressed, so the assertions above are not
	// being satisfied by a call that quietly did nothing.
	waitForPaneIdle(t, c, usersPane)
	captured := c.callToolText(t, "capture-pane", map[string]any{"paneId": usersPane})
	if !strings.Contains(flattenPane(captured), "pinned-by-paneid") {
		t.Errorf("the keys never reached the named pane %s; it holds:\n%s", usersPane, captured)
	}
}

// ---- The default target ----

// TestSendKeysWithNoPaneIdUsesSlotOne is the behaviour change stated as a test.
//
// A bare send-keys({keys}) used to be an error, and that refusal is what the
// incident behind this design began with: an agent told "paneId is required"
// went looking for $TMUX_PANE and started driving raw tmux. It now lands in
// helper slot 1 — never in the agent's own pane, which is Invariant R and is
// asserted here at the tool level as well as inside the resolver.
func TestSendKeysWithNoPaneIdUsesSlotOne(t *testing.T) {
	c, self := agentPaneFixture(t)

	var first map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo one"}, &first)

	pane, _ := first["paneId"].(string)
	if pane == "" {
		t.Fatalf("send-keys with no paneId returned no pane: %v", first)
	}
	if pane == self {
		t.Fatalf("send-keys resolved to the agent's own pane %s — it would be typing into the "+
			"conversation the user is having", pane)
	}
	if slot, _ := first["slot"].(float64); int(slot) != 1 {
		t.Errorf("send-keys reported slot %v, want 1", first["slot"])
	}
	if created, _ := first["created"].(bool); !created {
		t.Error("the first send-keys had to create the slot-1 pane, so created must be true")
	}

	// The second call must land in the same pane, and must NOT claim to have
	// created it: created is the only signal an agent gets that the process it
	// left running there is gone.
	var second map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo two"}, &second)
	if second["paneId"] != pane {
		t.Errorf("second send-keys went to %v, want the same pane %s", second["paneId"], pane)
	}
	if created, ok := second["created"]; ok && created == true {
		t.Error("second send-keys reported created=true for a pane it reused")
	}
}

// TestExecuteCommandWithNoPaneIdUsesSlotOne covers the tool the plan originally
// left out. execute-command delivers keystrokes exactly as send-keys does, and a
// tool that still answered "paneId is required when headless=false" while its
// siblings defaulted to slot 1 would be the loophole that sends an agent hunting
// for the terminal by other means.
func TestExecuteCommandWithNoPaneIdUsesSlotOne(t *testing.T) {
	c, self := agentPaneFixture(t)

	var out map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{"command": "echo slotted-output"}, &out)

	pane, _ := out["paneId"].(string)
	if pane == "" || pane == self {
		t.Fatalf("execute-command resolved to %q (self is %s)", pane, self)
	}
	if slot, _ := out["slot"].(float64); int(slot) != 1 {
		t.Errorf("execute-command reported slot %v, want 1", out["slot"])
	}
	if output, _ := out["output"].(string); !strings.Contains(output, "slotted-output") {
		t.Errorf("command did not run in the helper pane; output was %q", output)
	}
	if code, _ := out["exitCode"].(float64); code != 0 {
		t.Errorf("exit code %v, want 0", out["exitCode"])
	}

	// headless keeps working, and keeps being a different universe: no paneId
	// comes back because the session is destroyed with the answer.
	var headless map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{
		"command": "echo headless-output", "headless": true,
	}, &headless)
	if _, ok := headless["paneId"]; ok {
		t.Errorf("headless execute-command returned a paneId: %v", headless)
	}
	if output, _ := headless["output"].(string); !strings.Contains(output, "headless-output") {
		t.Errorf("headless output was %q", output)
	}
}

// TestWriteToDisplayWithNoPaneIdUsesSlotOne is the same behaviour change for the
// last keystroke-delivering tool that still demanded an explicit pane.
//
// write-to-display calls SendKeys like the others, so refusing a bare call had
// exactly the same consequence: an agent told "paneId is required" goes hunting
// for $TMUX_PANE. Landing in slot 1 is the fix, and slot 1 is never the agent's
// own pane — which for this tool means the coaching text cannot end up in the
// conversation it is designed to stay out of.
func TestWriteToDisplayWithNoPaneIdUsesSlotOne(t *testing.T) {
	c, self := agentPaneFixture(t)

	var first map[string]any
	c.callToolJSON(t, "write-to-display", map[string]any{"text": "coaching-one"}, &first)

	pane, _ := first["paneId"].(string)
	if pane == "" {
		t.Fatalf("write-to-display with no paneId returned no pane: %v", first)
	}
	if pane == self {
		t.Fatalf("write-to-display resolved to the agent's own pane %s — the text would land in "+
			"the conversation it exists to stay out of", pane)
	}
	if slot, _ := first["slot"].(float64); int(slot) != 1 {
		t.Errorf("write-to-display reported slot %v, want 1", first["slot"])
	}
	if created, _ := first["created"].(bool); !created {
		t.Error("the first write-to-display had to create the slot-1 pane, so created must be true")
	}
	// The one thing this tool must never return is the text itself: keeping it
	// out of the model's context is the whole reason the tool exists.
	if _, hasText := first["text"]; hasText {
		t.Errorf("write-to-display echoed its text back in the response: %v", first)
	}

	// Whitespace is stripped before matching: a helper pane is half the window
	// wide and can wrap a word mid-character.
	sleep(300 * time.Millisecond)
	captured := c.callToolText(t, "capture-pane", map[string]any{"paneId": pane})
	if !strings.Contains(flattenPane(captured), "coaching-one") {
		t.Errorf("the text never reached the slot-1 pane; it holds:\n%s", captured)
	}

	// The second call must land in the same pane and must not claim to have
	// created it — created is the only signal that the pane is new to the slot.
	var second map[string]any
	c.callToolJSON(t, "write-to-display", map[string]any{"text": "coaching-two"}, &second)
	if second["paneId"] != pane {
		t.Errorf("second write-to-display went to %v, want the same pane %s", second["paneId"], pane)
	}
	if created, ok := second["created"]; ok && created == true {
		t.Error("second write-to-display reported created=true for a pane it reused")
	}
}

// TestStartAndWatchWithNoArgumentsUsesSlotOne covers the row §8's table calls
// out by name, and it is the row with the most history behind it.
//
// start-and-watch with no paneId, no slot and no headless used to create its own
// detached session on a socket of its own. The command ran, the pattern matched,
// the response was correct in every field — and the pane was somewhere the user
// could not see, in a session no tool of theirs listed, holding a dev server
// that survived until something eventually killed the session. That behaviour
// would pass every OTHER test in this file, because the ones that exercise the
// slot path all pass a slot; this is the default path, and it is the path an
// agent following the tool description actually takes.
//
// So the assertions are about WHERE the pane is, not just which fields came
// back: slot 1, reported as created, in the agent's own window rather than a
// session of the server's own making. The event assertion is what proves the
// command ran in the pane being talked about — a response naming a pane the
// command never reached would satisfy the rest.
func TestStartAndWatchWithNoArgumentsUsesSlotOne(t *testing.T) {
	c, self := agentPaneFixture(t)
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	var watch map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"command": "echo default-path-ready",
		"pattern": "default-path-ready",
		"timeout": 15,
	}, &watch)

	pane, _ := watch["paneId"].(string)
	if pane == "" {
		t.Fatalf("start-and-watch with no arguments returned no pane: %v", watch)
	}
	if pane == self {
		t.Fatalf("start-and-watch ran the command in the agent's own pane %s", pane)
	}
	if strings.HasPrefix(pane, headlessPrefix) {
		t.Fatalf("start-and-watch went headless without being asked (pane %s); with neither slot "+
			"nor paneId nor headless the command must land in slot 1, where the user can see it", pane)
	}
	if slot, _ := watch["slot"].(float64); int(slot) != 1 {
		t.Errorf("start-and-watch reported slot %v, want 1", watch["slot"])
	}
	if created, _ := watch["created"].(bool); !created {
		t.Error("the first start-and-watch had to create the slot-1 pane, so created must be true")
	}
	if event, _ := watch["event"].(string); !strings.Contains(event, "default-path-ready") {
		t.Fatalf("start-and-watch did not match its own pattern (event %q): the command did not run "+
			"in the pane it reported, so the assertions above are about a pane nothing happened in: %v",
			event, watch)
	}

	if panes := panesInWindow(t, window); !containsString(panes, pane) {
		t.Errorf("the slot-1 pane %s is not in the agent's own window %s (which holds %v) — the "+
			"command is running somewhere the user cannot see it, which is the behaviour this "+
			"default replaced", pane, window, panes)
	}
}

// containsString reports whether a list holds an exact string.
func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// ---- The registry is in tmux, not in this process ----

// TestSlotRecordLivesInTmuxNotInThisProcess is the test that makes §2.4's
// storage decision falsifiable, and without it the decision is only a comment.
//
// Every other test in this suite drives ONE client or ONE server child, so a
// `map[int]string` guarded by slotMu would pass all of them: it is idempotent,
// it survives a kill if you also clear the map, it reports created correctly. It
// would then fail in production in two ways that no test would have named. Two
// agents sharing a window — a subagent started beside its parent — would each
// believe they own slot 1 and would open a pane each, because neither can see
// the other's map. And a server restarted mid-session (a crash, a config reload,
// an upgrade) would come back with an empty map and split the window again,
// beside the pane it had already made.
//
// Storing the record on the pane, in tmux's own options, is what makes both
// impossible, and the two halves of this test are the two halves of that claim:
//
//   - A SECOND server process, pointed at the same TMUX_PANE, resolves slot 1 to
//     the pane the FIRST one created, reports it as reused, and adds no pane to
//     the window. Process-external storage is the only way that can be true.
//   - Kill that pane with raw tmux — the user closing it — and the same second
//     process now reports created:true for a NEW pane. The record died with the
//     pane, because it lived ON the pane; nothing had to invalidate it, and
//     nothing in either process was holding a stale id to hand back.
//
// The second half is what catches the tempting middle position, a cache
// consulted before tmux: that cache would answer with a dead pane id, and
// send-keys into a dead pane reports success while swallowing every keystroke.
func TestSlotRecordLivesInTmuxNotInThisProcess(t *testing.T) {
	first, self := agentPaneFixture(t)
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	var opened map[string]any
	first.callToolJSON(t, "split-pane", map[string]any{"slot": 1}, &opened)
	pane, _ := opened["paneId"].(string)
	if pane == "" || pane == self {
		t.Fatalf("the first server resolved slot 1 to %q (its own pane is %s)", pane, self)
	}
	if created, _ := opened["created"].(bool); !created {
		t.Fatalf("slot 1 already existed in a fresh window, so this test is not exercising "+
			"anything: %v", opened)
	}
	before := panesInWindow(t, window)

	// A second server process, in the same pane the first one runs in: what a
	// restart looks like, and what a second agent in the window looks like. It
	// shares nothing with the first but the tmux server.
	second := newMCPClientInPane(t, self)

	var found map[string]any
	second.callToolJSON(t, "split-pane", map[string]any{"slot": 1}, &found)
	if got, _ := found["paneId"].(string); got != pane {
		t.Errorf("the second server resolved slot 1 to %v, want %s — the pane the first server "+
			"created. A registry held in the process cannot be read by another process, and the "+
			"user gets a second identical pane", found["paneId"], pane)
	}
	if created, ok := found["created"].(bool); ok && created {
		t.Errorf("the second server reported created=true for a pane that already existed (%v); "+
			"created is what tells an agent its process is gone, so a false positive says a "+
			"running dev server died", found)
	}
	if after := panesInWindow(t, window); len(after) != len(before) {
		t.Errorf("the window went from %d panes to %d (%v → %v) when a second server resolved a "+
			"slot the first had already opened", len(before), len(after), before, after)
	}

	// The user closes the pane. Raw tmux, because this is the one event the server
	// never performs and cannot be told about.
	tmuxExec(t, "kill-pane", "-t", pane)

	var again map[string]any
	second.callToolJSON(t, "split-pane", map[string]any{"slot": 1}, &again)
	got, _ := again["paneId"].(string)
	if got == pane {
		t.Errorf("slot 1 still resolves to %s after the pane was killed: the record outlived the "+
			"pane, which it cannot do if it lives on the pane — and keystrokes sent to a dead pane "+
			"are accepted and discarded in silence", pane)
	}
	if created, _ := again["created"].(bool); !created {
		t.Errorf("slot 1 reported created=false for pane %v after its pane was killed (%v); the "+
			"agent is not told that whatever it left running there is gone", got, again)
	}
	if got == self {
		t.Fatalf("slot 1 resolved to the server's own pane %s after the helper was killed", got)
	}
}

// flattenPane strips the whitespace a narrow pane inserts, so a substring match
// is not defeated by a line wrap landing mid-word.
func flattenPane(captured string) string {
	return strings.ReplaceAll(strings.ReplaceAll(captured, "\n", ""), " ", "")
}

// TestWriteToDisplayClearRespectsPaneOwnership is the hazard that arrived with
// the default target, in both directions.
//
// clear:true used to send C-u — kill line — before running `clear`, which is
// correct on a pane the server created: the previous write is still unsubmitted
// in the line editor and would otherwise be concatenated with the word "clear".
// It became destructive the moment a bare clear:true could resolve to a pane
// ADOPTED from the user, where that line is the command they were halfway
// through typing and had not run.
//
// The acquired subtest asserts all three properties of the replacement at once,
// because any two of them can be satisfied by something wrong: the user's line
// SURVIVES (so no C-u), the line was NOT SUBMITTED (so not the tempting
// "skip the C-u but still send clear" repair, which appends "clear" to their
// command and presses Enter on it), and the coaching text still ARRIVES (so the
// tool was not quietly turned into a no-op on adopted panes). Submission is
// checked against the filesystem rather than the screen: the pending command
// creates a file, so "was it run?" has an answer that cannot be misread.
//
// The created subtest is the other half. A fix that skipped the kill everywhere
// would pass the first subtest and silently break the tool's actual contract —
// each write replacing the last on the display pane the agent owns.
func TestWriteToDisplayClearRespectsPaneOwnership(t *testing.T) {
	t.Run("acquired", func(t *testing.T) {
		c, self := agentPaneFixture(t)

		// The user's pane starts in a scratch directory, so that a submitted line
		// with a word glued onto it — `touch <sentinel> clear` — leaves its stray
		// second file there rather than in the repository.
		dir := t.TempDir()
		usersPane := tmuxExec(t, "split-window", "-d", "-c", dir, "-t", self, "-P", "-F", "#{pane_id}")
		waitForPaneIdle(t, c, usersPane)

		// The user types a command and stops short of Enter. This is exactly the
		// state canAcquire documents that it cannot see, which is why the pane is
		// still adopted below — the point of the test, not a flaw in it.
		//
		// The trailing SPACE is load-bearing. Without it, a "clear" appended to
		// `touch <sentinel>` runs `touch <sentinel>clear`, which creates a
		// DIFFERENT file: the line would have been submitted and the sentinel
		// assertion below would still have found nothing and reported success.
		// With it, every form of submission — bare Enter, or Enter after something
		// was appended — runs the touch. (A trailing ";" looks like the tidier
		// version and is not: tmux's own argument parser reads a trailing
		// semicolon as a command separator and strips it before the pane ever
		// sees it.)
		sentinel := filepath.Join(dir, "submitted")
		tmuxExec(t, "send-keys", "-t", usersPane, "-l", "touch "+sentinel+" ")
		sleep(300 * time.Millisecond)

		var out map[string]any
		c.callToolJSON(t, "write-to-display", map[string]any{
			"text": "COACH-TEXT", "clear": true,
		}, &out)
		if out["paneId"] != usersPane {
			t.Fatalf("the fixture did not exercise adoption: write-to-display resolved to %v, "+
				"want the user's pane %s", out["paneId"], usersPane)
		}

		sleep(300 * time.Millisecond)
		flat := flattenPane(c.callToolText(t, "capture-pane", map[string]any{"paneId": usersPane}))
		if !strings.Contains(flat, flattenPane("touch "+sentinel)) {
			t.Errorf("clear:true destroyed the user's half-typed line on an adopted pane; the "+
				"pane holds:\n%s", flat)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Errorf("clear:true SUBMITTED the user's line — %s exists, so the command they had "+
				"deliberately not run was run for them", sentinel)
		}
		if !strings.Contains(flat, "COACH-TEXT") {
			t.Errorf("clear:true swallowed the write on an adopted pane; the pane holds:\n%s", flat)
		}
	})

	t.Run("created", func(t *testing.T) {
		c, _ := agentPaneFixture(t)

		var first map[string]any
		c.callToolJSON(t, "write-to-display", map[string]any{
			"text": "STALE-TEXT", "clear": true,
		}, &first)
		pane, _ := first["paneId"].(string)
		if pane == "" {
			t.Fatalf("write-to-display returned no pane: %v", first)
		}
		sleep(300 * time.Millisecond)

		c.callToolJSON(t, "write-to-display", map[string]any{
			"text": "FRESH-TEXT", "clear": true,
		}, &first)
		sleep(300 * time.Millisecond)

		flat := flattenPane(c.callToolText(t, "capture-pane", map[string]any{"paneId": pane}))
		if strings.Contains(flat, "STALE-TEXT") {
			t.Errorf("clear:true left the previous write behind on a pane the server created — the "+
				"kill-line path must still run there, or the display accumulates instead of "+
				"refreshing:\n%s", flat)
		}
		if !strings.Contains(flat, "FRESH-TEXT") {
			t.Errorf("the new text is missing from the display pane:\n%s", flat)
		}
	})
}

// TestClearDecisionNeverKillsALineOnEvidenceItDoesNotHave is the clear decision
// enumerated, away from panes and keystrokes.
//
// The table is small and every row is a claim about what happens to somebody's
// unsubmitted command line, so the two paths are walked in full rather than
// sampled. The rows that matter are the ones where nothing positive is known: an
// empty owner, or an owner kind this binary does not recognise. On the slot path
// those must be the REDRAW, because a resolution that reported no owner is a
// resolution that told us nothing, and "no evidence" is not evidence that the
// pane is ours. On the explicit path they must be the kill, because that is what
// this tool did before slots existed and what every caller that split its own
// display pane relies on — a pane with no record there means "the one I made and
// pointed you at", and the caller took the safety burden by naming it.
func TestClearDecisionNeverKillsALineOnEvidenceItDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owner  string
		bySlot bool
		want   bool
		why    string
	}{
		{"slot resolved to a pane we made", ownerAgent, true, true,
			"the display pane is ours; each write must replace the last, which needs the kill"},
		{"slot resolved to a pane we adopted", ownerAcquired, true, false,
			"the line in it is the user's half-typed command"},
		{"slot resolved but reported no owner", "", true, false,
			"a resolution that says nothing is not permission to destroy a line"},
		{"slot resolved with an owner kind we do not know", "future-owner-kind", true, false,
			"an unrecognised owner is a pane whose semantics this binary does not implement"},
		{"explicit paneId with no record", "", false, true,
			"the pre-slot behaviour: the caller named a display pane it owns"},
		{"explicit paneId on an adopted pane", ownerAcquired, false, false,
			"the record positively says the user opened it"},
		{"explicit paneId on a pane we made", ownerAgent, false, true,
			"our own pane, named directly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clearKillsTheLine(tc.owner, tc.bySlot); got != tc.want {
				verb := map[bool]string{true: "C-u + clear + Enter", false: "C-l (redraw)"}
				t.Errorf("owner=%q bySlot=%v chose %s, want %s — %s",
					tc.owner, tc.bySlot, verb[got], verb[tc.want], tc.why)
			}
		})
	}
}

// TestClearForDisplayTrustsTheOwnerCapturedUnderTheLock is the race, reproduced
// by its state rather than by its timing.
//
// # What is being reproduced
//
// resolvePaneArg resolves a slot under slotMu and RELEASES the lock before
// returning. write-to-display({clear:true}) then acts on that pane with no lock
// held. A concurrent close-pane on the same slot releases the adopted pane —
// releaseAcquiredLocked wipes @mcp_pane/@mcp_owner/@mcp_slot and leaves the pane
// alive, handed back to the user. Land it in that window and a clear which reads
// the registry AGAIN sees no record at all, reads that as "a pane the caller
// named explicitly", and sends C-u + `clear` + Enter into a shell the user is
// typing in: their line destroyed, and Enter pressed on what is left.
//
// The state is what does the damage, so the state is what this test builds:
// resolution really adopts the user's pane and hands back owner=acquired, the
// registration is really wiped by the same call close-pane makes, and the user
// really has an unsubmitted command in the pane. What is NOT reproduced is the
// interleaving — no goroutine loses a race here, because a test that has to win
// a microsecond-wide window to fail is a test that reports green on a bad day.
// Everything after the wipe is exactly what the losing thread would do, and the
// assertion is on what it does to the user's line.
//
// Against a clearForDisplay that re-reads the registry this is RED: the read
// finds nothing, the kill runs, the line is gone and the sentinel file exists.
// Against one that uses the owner captured during resolution it is GREEN, and it
// is green for the reason that makes the fix sound rather than lucky — C-l is
// safe whatever has happened to the pane since.
func TestClearForDisplayTrustsTheOwnerCapturedUnderTheLock(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()

	// The user's own pane, in a scratch directory so that a line submitted by
	// accident leaves its file there rather than in the repository.
	dir := t.TempDir()
	usersPane := tmuxExec(t, "split-window", "-d", "-c", dir, "-t", self, "-P", "-F", "#{pane_id}")
	waitForClientPaneIdle(t, client, usersPane)

	pane, slot, created, owner, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("resolveHelper: %v", err)
	}
	if pane != usersPane || owner != ownerAcquired {
		t.Fatalf("the fixture did not exercise adoption: slot 1 resolved to %s (owner %q), want the "+
			"user's pane %s adopted as %q", pane, owner, usersPane, ownerAcquired)
	}
	// The target the handler is holding when the lock goes away.
	tgt := paneTarget{PaneID: pane, Slot: slot, Created: created, Owner: owner}

	// The concurrent close-pane lands. This is the mutation, verbatim: the
	// registration is erased and the pane is left alive, which is what "released
	// back to the user" means.
	if err := client.clearPaneRegistration(ctx, usersPane); err != nil {
		t.Fatalf("release the adopted pane: %v", err)
	}
	if _, found, err := client.paneRecordFor(ctx, usersPane); err != nil || found {
		t.Fatalf("the pane still carries a registry record (found=%v err=%v); a re-read would find "+
			"it and this test would not be reproducing the race at all", found, err)
	}

	// The pane is theirs again, and they start typing. The trailing space is
	// load-bearing for the same reason it is in the test above: without it a
	// "clear" appended to the line would run `touch <sentinel>clear` and create a
	// different file, so a submitted line would leave the sentinel absent and the
	// assertion below would report success.
	sentinel := filepath.Join(dir, "submitted")
	tmuxExec(t, "send-keys", "-t", usersPane, "-l", "touch "+sentinel+" ")
	sleep(300 * time.Millisecond)

	if err := client.clearForDisplay(ctx, tgt); err != nil {
		t.Fatalf("clearForDisplay: %v", err)
	}
	sleep(300 * time.Millisecond)

	content, err := client.CapturePane(ctx, usersPane, 0, false)
	if err != nil {
		t.Fatalf("capture the pane back: %v", err)
	}
	flat := flattenPane(content)
	if !strings.Contains(flat, flattenPane("touch "+sentinel)) {
		t.Errorf("the clear destroyed the user's half-typed line on a pane that had been released "+
			"back to them between resolution and this call — the owner read under slotMu said "+
			"%q and must have been believed; the pane holds:\n%s", ownerAcquired, flat)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Errorf("the clear SUBMITTED the user's line — %s exists, so a command they had "+
			"deliberately not run was run for them, by a tool whose only job was to print text",
			sentinel)
	}
}

// ---- close-pane ----

// closedEntries calls close-pane and returns the entries it reported.
func closedEntries(t *testing.T, c *mcpClient, args map[string]any) []map[string]any {
	t.Helper()
	var out struct {
		Closed []map[string]any `json:"closed"`
	}
	c.callToolJSON(t, "close-pane", args, &out)
	return out.Closed
}

// paneExists reports whether tmux still knows about a pane.
//
// It enumerates every pane on the server rather than asking about this one,
// because display-message -t falls back to a default target when the pane it
// names is gone: it answers happily with some OTHER pane's id and exit status 0,
// so a kill-then-check written the obvious way reports the pane as still alive.
func paneExists(t *testing.T, paneID string) bool {
	t.Helper()
	for _, id := range strings.Split(tmuxExec(t, "list-panes", "-a", "-F", "#{pane_id}"), "\n") {
		if strings.TrimSpace(id) == paneID {
			return true
		}
	}
	return false
}

// TestClosePaneKillsAgentPane is the simple half: a pane the server created is
// its own to destroy, and closing it must actually remove it rather than leave
// an unslotted split behind for the user to tidy up.
func TestClosePaneKillsAgentPane(t *testing.T) {
	c, _ := agentPaneFixture(t)

	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{"slot": 1}, &split)
	pane := split["paneId"].(string)

	entries := closedEntries(t, c, map[string]any{})
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %v", entries)
	}
	if entries[0]["action"] != "killed" {
		t.Errorf("action is %v, want \"killed\" — the server created this pane", entries[0]["action"])
	}
	if entries[0]["paneId"] != pane {
		t.Errorf("closed %v, want %s", entries[0]["paneId"], pane)
	}
	if paneExists(t, pane) {
		t.Errorf("pane %s still exists after close-pane reported killing it", pane)
	}

	// Closing an empty slot is a satisfied request, not an error: an agent must
	// be able to tear down unconditionally without first checking what it opened.
	again := closedEntries(t, c, map[string]any{})
	if len(again) != 1 || again[0]["action"] != "none" {
		t.Errorf("closing an empty slot answered %v, want a single action:\"none\" entry", again)
	}
}

// TestClosePaneReleasesAcquired is the half that matters.
//
// An adopted pane belongs to the user. Closing it must interrupt whatever it is
// doing and give it back — pane intact, every marker removed — because killing it
// is the one unrecoverable action in this design. All three markers go, not just
// the slot: leaving @mcp_owner=acquired behind would retire the pane forever,
// since the acquisition predicate requires the owner mark to be unset, and the
// user would be left with an ordinary shell the agent mysteriously refuses to
// touch again.
func TestClosePaneReleasesAcquired(t *testing.T) {
	c, self := agentPaneFixture(t)

	usersPane := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	waitForPaneIdle(t, c, usersPane)

	var sent map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo adopted", "enter": true}, &sent)
	if sent["paneId"] != usersPane {
		t.Fatalf("the fixture did not exercise adoption: send-keys resolved to %v, want %s",
			sent["paneId"], usersPane)
	}
	waitForPaneIdle(t, c, usersPane)

	entries := closedEntries(t, c, map[string]any{})
	if len(entries) != 1 || entries[0]["action"] != "released" {
		t.Fatalf("close-pane answered %v, want a single action:\"released\" entry", entries)
	}
	if !paneExists(t, usersPane) {
		t.Fatal("close-pane destroyed a pane the user opened; an adopted pane may only be released")
	}
	for _, opt := range []string{paneOptSlot, paneOptOwner, paneOptWitness} {
		if got := tmuxExec(t, "display-message", "-t", usersPane, "-p", "#{"+opt+"}"); got != "" {
			t.Errorf("%s is still set to %q on the released pane; all three markers must go", opt, got)
		}
	}
}

// TestClosePaneRefusesUnmanagedPane pins the refusal that keeps close-pane from
// becoming a second kill-pane. The pane must still be alive afterwards, and the
// message must name kill-pane — an error that only says "no" is what sends an
// agent looking for another way to do the same thing.
func TestClosePaneRefusesUnmanagedPane(t *testing.T) {
	c, self := agentPaneFixture(t)

	usersPane := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	waitForPaneIdle(t, c, usersPane)

	res := callTool(t, c, "close-pane", map[string]any{"paneId": usersPane})
	if !res.IsError {
		t.Fatalf("close-pane accepted a pane it does not own: %v", res)
	}
	msg := res.text(t, "close-pane")
	if !strings.Contains(msg, "kill-pane") {
		t.Errorf("refusal must name the tool that CAN do this, got %q", msg)
	}
	if !paneExists(t, usersPane) {
		t.Fatal("close-pane killed a pane it had just refused to close")
	}
}

// TestClosePaneRefusesSelfPane is the regression for the one hole every other
// path had already closed.
//
// The fixture is the ordinary nested case, not a contrivance: an outer agent's
// split-pane creates a pane, marks it agent-owned in slot 1 and titles it
// "agent"; a subagent's server is started inside that pane and inherits
// TMUX_PANE. From the inner server the pane is indistinguishable from a helper —
// the witness matches, the owner is "agent", the title says so — which is exactly
// why the record cannot be the thing that authorises the close. The markers are
// written with raw tmux because the outer server is not part of this test.
//
// The explicit-paneId branch is the one being driven, because it is the one that
// went straight from "a record exists" to KillPane. The pane must survive, the
// refusal must name kill-pane as the deliberate way to do this, and it must say
// which pane it is refusing — an error that only says "no" is what sends an agent
// looking for another route.
func TestClosePaneRefusesSelfPane(t *testing.T) {
	c, self := agentPaneFixture(t)

	tmuxExec(t, "set-option", "-p", "-t", self, paneOptWitness, self)
	tmuxExec(t, "set-option", "-p", "-t", self, paneOptOwner, ownerAgent)
	tmuxExec(t, "set-option", "-p", "-t", self, paneOptSlot, "1")

	res := callTool(t, c, "close-pane", map[string]any{"paneId": self})
	if !res.IsError {
		t.Fatalf("close-pane accepted the pane the server is running in: %v", res)
	}
	msg := res.text(t, "close-pane")
	if !strings.Contains(msg, "running in") {
		t.Errorf("the refusal does not explain that this is the server's own pane, so it reads "+
			"like the unmanaged-pane refusal instead: %q", msg)
	}
	if !strings.Contains(msg, "kill-pane") {
		t.Errorf("refusal must name the tool that CAN do this, got %q", msg)
	}
	if !paneExists(t, self) {
		t.Fatal("close-pane killed the pane the server is running in — the agent destroyed its " +
			"own session through the tool whose contract is that it is the safe closer")
	}

	// And the pane is still a working pane, not a husk: the server can answer for
	// it. A kill that had already happened would fail this before the test ended.
	if got := tmuxExec(t, "display-message", "-p", "-t", self, "#{pane_id}"); got != self {
		t.Errorf("self pane reports id %q, want %s", got, self)
	}
}

// waitForPaneDead polls until tmux marks the pane's process exited. Only
// meaningful with remain-on-exit on, which is what keeps the pane in the window
// afterwards.
func waitForPaneDead(t *testing.T, paneID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tmuxExec(t, "display-message", "-p", "-t", paneID, "#{pane_dead}") == "1" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s did not become dead within 5s; the fixture is not exercising remain-on-exit", paneID)
}

// TestClosePaneReapsDeadPane is the disagreement, written as a test.
//
// With remain-on-exit set — the user's setting, not ours — a helper whose process
// exited stays in the window as a corpse. close-pane({slot:"all"}) killed it,
// because that path never filtered dead panes, while the no-argument
// close-pane() answered {"slot":1,"action":"none"} and left it on screen: the
// reuse-path filter that (rightly) refuses to hand a dead pane back as a helper
// had been reused as the lookup for teardown, where it means "I will not touch
// that". Two forms of one tool giving different answers about the same pane is
// the bug; the subtests are the two forms, and they must now agree.
//
// Reusability is the other half. A corpse nobody can close sits in slot 1
// forever, and the reuse path (rightly) refuses to hand a dead pane back — so
// until the corpse is reaped, slot 1 is a number that resolves to nothing usable.
func TestClosePaneReapsDeadPane(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"default", map[string]any{}},
		{"all", map[string]any{"slot": "all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := agentPaneFixture(t)

			var split map[string]any
			c.callToolJSON(t, "split-pane", map[string]any{"slot": 1}, &split)
			pane := split["paneId"].(string)

			// remain-on-exit is what makes a corpse instead of a removal; without
			// it tmux takes the pane away the instant the shell exits and there is
			// nothing left to disagree about.
			tmuxExec(t, "set-option", "-t", pane, "remain-on-exit", "on")
			tmuxExec(t, "send-keys", "-t", pane, "exit", "Enter")
			waitForPaneDead(t, pane)

			entries := closedEntries(t, c, tc.args)
			if len(entries) != 1 {
				t.Fatalf("expected one entry, got %v", entries)
			}
			if entries[0]["action"] != "killed" {
				t.Errorf("close-pane%v answered action %v for a dead slot-1 pane, want \"killed\" — "+
					"\"none\" would mean \"this slot is empty\", which is not what the user is "+
					"looking at", tc.args, entries[0]["action"])
			}
			if paneExists(t, pane) {
				t.Errorf("dead pane %s is still in the window; a corpse nobody can close also "+
					"consumes its slot number forever", pane)
			}

			// Slot 1 has to be usable again, which is the reusability half: with
			// the corpse gone, resolving slot 1 must produce a fresh live pane
			// rather than rediscovering the body or failing.
			var next map[string]any
			c.callToolJSON(t, "split-pane", map[string]any{"slot": 1}, &next)
			if slot, _ := next["slot"].(float64); int(slot) != 1 {
				t.Errorf("slot 1 resolved to slot %v after the corpse was reaped", next["slot"])
			}
			if created, _ := next["created"].(bool); !created {
				t.Error("slot 1 reported created=false after its pane was reaped — it must be a new pane")
			}
			if got, _ := next["paneId"].(string); got == pane {
				t.Errorf("slot 1 handed back the reaped pane %s", got)
			}
		})
	}
}

// TestClosePaneAllReleasesAndKills covers slot:"all", which is the one call an
// agent is told to make at the end of a session. It has to do both jobs in one
// pass — destroy what the server made, hand back what it borrowed — and it must
// never touch the agent's own pane, which is the pane the request arrived
// through.
func TestClosePaneAllReleasesAndKills(t *testing.T) {
	c, self := agentPaneFixture(t)

	// Slot 1: adopted from the user.
	usersPane := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	waitForPaneIdle(t, c, usersPane)
	var one map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{"slot": 1}, &one)
	if one["paneId"] != usersPane {
		t.Fatalf("slot 1 resolved to %v, want the adopted pane %s", one["paneId"], usersPane)
	}

	// Slot 2: created by the server.
	var two map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{"slot": 2}, &two)
	madePane := two["paneId"].(string)

	entries := closedEntries(t, c, map[string]any{"slot": "all"})
	if len(entries) != 2 {
		t.Fatalf("expected two entries, got %v", entries)
	}
	byPane := map[string]string{}
	for _, e := range entries {
		byPane[e["paneId"].(string)] = e["action"].(string)
	}
	if byPane[usersPane] != "released" {
		t.Errorf("adopted pane %s got action %q, want \"released\"", usersPane, byPane[usersPane])
	}
	if byPane[madePane] != "killed" {
		t.Errorf("created pane %s got action %q, want \"killed\"", madePane, byPane[madePane])
	}
	if !paneExists(t, usersPane) {
		t.Error("slot:\"all\" destroyed the pane the user opened")
	}
	if paneExists(t, madePane) {
		t.Error("slot:\"all\" left behind the pane the server created")
	}
	if !paneExists(t, self) {
		t.Fatal("slot:\"all\" closed the agent's own pane")
	}
}

// ---- kill-pane stays blunt ----

// TestKillPaneTakesNoSlot is the other half of close-pane's justification, and
// it is the half nothing was checking.
//
// close-pane exists because destroying a pane should never be the accidental
// outcome of tidying up: it refuses panes it does not own, refuses the pane this
// server runs in, and releases what it merely borrowed. All of that is only
// worth something while kill-pane stays what §4 and §8 say it is — an explicit
// paneId and nothing else. Give kill-pane a slot and the argument collapses: a
// bare kill-pane() would destroy the helper pane, kill-pane({slot:1}) would read
// as tidy-up rather than demolition, and the tool an agent reaches for when it
// wants something GONE would acquire a default target, which is exactly the
// shape of the mistake close-pane was built to make impossible. The same
// reasoning kept send-keys' default away from the agent's own pane; here it
// keeps a destructive tool from having any default at all.
//
// The schema is read from tools/list rather than from the registration source,
// because the schema is what an agent is shown and therefore what it will try.
func TestKillPaneTakesNoSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)

	raw := c.call(t, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}

	var found bool
	for _, tool := range list.Tools {
		if tool.Name != "kill-pane" {
			continue
		}
		found = true
		if _, hasSlot := tool.InputSchema.Properties["slot"]; hasSlot {
			t.Error("kill-pane declares a slot property: the tool that destroys panes must be " +
				"aimed by hand every time, or close-pane's refusals are just a slower way to the " +
				"same accident")
		}
		if _, hasHeadless := tool.InputSchema.Properties["headless"]; hasHeadless {
			t.Error("kill-pane declares a headless property; it kills the pane it is given and " +
				"has no business creating a universe to look in")
		}
		if !containsString(tool.InputSchema.Required, "paneId") {
			t.Errorf("kill-pane's required arguments are %v; paneId must stay required, because "+
				"the alternative is a destructive tool with a default target",
				tool.InputSchema.Required)
		}
	}
	if !found {
		t.Fatal("kill-pane is not in tools/list — this test is guarding a tool that is not there")
	}

	// And the refusal is real rather than only declared: a schema is advisory,
	// and some clients will send the call anyway.
	res := callTool(t, c, "kill-pane", map[string]any{})
	if !res.IsError {
		t.Fatalf("kill-pane with no paneId was accepted: %v", res)
	}
	if msg := res.text(t, "kill-pane"); !strings.Contains(msg, "paneId") {
		t.Errorf("the refusal does not name the argument that is missing, which is what tells an "+
			"agent what to do next: %q", msg)
	}
}

// TestCapturePaneKeepsItsTextBody guards the one tool whose entire contract is
// "the pane's exact content". The resolution metadata must ride in
// structuredContent and never be prepended to the text, because a header there
// would corrupt the answer for every existing caller and be indistinguishable
// from pane output.
func TestCapturePaneKeepsItsTextBody(t *testing.T) {
	c, _ := agentPaneFixture(t)

	var sent map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo capture-marker", "enter": true}, &sent)
	pane := sent["paneId"].(string)
	waitForPaneIdle(t, c, pane)

	res := callTool(t, c, "capture-pane", map[string]any{})
	body := res.text(t, "capture-pane")
	if !strings.Contains(body, "capture-marker") {
		t.Errorf("capture-pane did not read the slot-1 pane; body was:\n%s", body)
	}
	if strings.Contains(body, "paneId") || strings.Contains(body, `"slot"`) {
		t.Errorf("capture-pane wrote resolution metadata into its text body:\n%s", body)
	}
	if res.StructuredContent["paneId"] != pane {
		t.Errorf("structuredContent paneId is %v, want %s", res.StructuredContent["paneId"], pane)
	}
	if slot, _ := res.StructuredContent["slot"].(float64); int(slot) != 1 {
		t.Errorf("structuredContent slot is %v, want 1", res.StructuredContent["slot"])
	}
}

// TestScreenshotPaneReportsItsResolvedPane is the positive half of the gate that
// keeps structuredContent off the explicit-paneId path.
//
// Withholding the key from a call that named its pane is a compatibility fix;
// withholding it from a call that resolved a slot would be a feature deleted,
// and the two are one line apart. A rendered pane cannot say which pane it is —
// that is the whole reason the resolution rides alongside the image — so a slot
// call that answers with no structuredContent leaves the caller unable to tell
// which of its panes it is looking at.
//
// html output rather than the default PNG: the assertion is about the metadata,
// which is attached identically in every mode, and the PNG path needs headless
// Chrome and would turn a compatibility check into a skip on half the machines
// that run it.
func TestScreenshotPaneReportsItsResolvedPane(t *testing.T) {
	c, _ := agentPaneFixture(t)

	var sent map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo shot-marker", "enter": true}, &sent)
	pane := sent["paneId"].(string)
	waitForPaneIdle(t, c, pane)

	res := callTool(t, c, "screenshot-pane", map[string]any{"output": "html"})
	if res.IsError {
		t.Fatalf("screenshot-pane errored: %s", res.text(t, "screenshot-pane"))
	}
	if res.StructuredContent["paneId"] != pane {
		t.Errorf("structuredContent paneId is %v, want the slot-1 pane %s; a rendered pane cannot "+
			"name itself, so this key is the only answer the caller gets",
			res.StructuredContent["paneId"], pane)
	}
	if slot, _ := res.StructuredContent["slot"].(float64); int(slot) != 1 {
		t.Errorf("structuredContent slot is %v, want 1", res.StructuredContent["slot"])
	}
}
