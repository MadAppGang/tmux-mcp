package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This file is the isolated half of the contract: panes on a second terminal
// server, with no window and nobody watching, addressed by exactly the same slot
// numbers as the visible ones.
//
// Every test here carries a control, and the controls are the point. "The user's
// window gained no pane" proves nothing unless the same probe can be shown to
// SEE a pane appear; "this server lists only its own slot" proves nothing unless
// it can be shown to list a second one when it has one. Where the control is
// subtle it is named in the comment above it.

// ---- Helpers ----

// tmuxOnIsolated runs a raw tmux command against the isolated socket, so a test
// can look at that server the way a user would look at theirs — from outside the
// product entirely.
func tmuxOnIsolated(t *testing.T, args ...string) string {
	t.Helper()
	return tmuxExec(t, append(socketArgs(headlessSocket), args...)...)
}

// isolatedPaneIDs lists every pane on the isolated socket, across all
// namespaces, and answers with nothing when no server is running there.
//
// It tolerates the failure deliberately: "no server on that socket" is what an
// empty isolated socket looks like, and it is the state most of these tests
// start and end in.
func isolatedPaneIDs(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("tmux", append(socketArgs(headlessSocket),
		"list-panes", "-a", "-F", "#{pane_id}")...).Output()
	if err != nil {
		return nil
	}
	var ids []string
	for _, id := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// isolatedSessionNames is the same view one level up, used where the question is
// about namespaces rather than panes.
func isolatedSessionNames(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("tmux", append(socketArgs(headlessSocket),
		"list-sessions", "-F", "#{session_name}")...).Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// isolatedFixture is a policy layer with no window at all — the shape a server
// started outside tmux has, and the only shape in which the isolated half can be
// tested without the visible half answering first.
//
// The cleanup sweeps this namespace rather than killing the server, because
// killing the server is the thing the product must never do: the socket is
// shared, and a neighbour's panes are on it.
func isolatedFixture(t *testing.T) (*slots, *tmuxBackend) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux")
	}
	b := newTmuxBackend(newTmuxClient("bash"))
	t.Cleanup(func() {
		panes, err := b.IsolatedPanes(context.Background())
		if err != nil {
			return
		}
		for _, pane := range panes {
			_ = b.Close(context.Background(), pane)
		}
	})
	return newSlots(b), b
}

// backendOf reaches the concrete adapter behind a policy layer, for the tests
// that have to ask a question the port does not expose — which namespace is
// this, and what is in it.
func backendOf(t *testing.T, sl *slots) *tmuxBackend {
	t.Helper()
	b, ok := sl.b.(*tmuxBackend)
	if !ok {
		t.Fatalf("the policy layer holds a %T, not the tmux adapter", sl.b)
	}
	return b
}

// openIsolatedSessionNamed puts a pane on the isolated socket under a name of
// the test's choosing, which is how a test plays the part of another server —
// or of the same server before it crashed.
func openIsolatedSessionNamed(t *testing.T, session string) paneRef {
	t.Helper()
	id := tmuxOnIsolated(t, "new-session", "-d", "-s", session,
		"-x", "200", "-y", "50", "-P", "-F", "#{pane_id}")
	t.Cleanup(func() {
		_ = exec.Command("tmux", append(socketArgs(headlessSocket),
			"kill-session", "-t", session)...).Run()
	})
	return newPaneRef(headlessPrefix + id)
}

// isolatedPaneExists asks tmux directly whether a pane is still on the isolated
// socket. It enumerates rather than targeting, for the reason paneExists does:
// display-message -t falls back to another pane when the one it names is gone.
func isolatedPaneExists(t *testing.T, pane paneRef) bool {
	t.Helper()
	bare := strings.TrimPrefix(pane.target(), headlessPrefix)
	return slices.Contains(isolatedPaneIDs(t), bare)
}

// ---- 9.4: the round trip ----

// TestIsolatedSlotRoundTrip drives one invisible slot through its whole life at
// the wire, and the calls after the first deliberately OMIT the isolated
// argument.
//
// That omission is the contract. If absence meant "visible", an isolated slot
// would become unaddressable by its own number the instant the creating call
// returned — and the reading tools, which have no isolated argument at all,
// could never reach one.
//
// # The controls
//
// (a) A direct list-panes on the USER'S window, taken at every stage. Without
// it, a slot quietly created as an ordinary visible pane satisfies every other
// assertion in this test: the slot resolves, the REPL answers, list-slots
// reports it. The whole difference between an isolated slot and a broken one is
// that nothing appeared beside the user.
//
// (b) The same probe around a deliberate VISIBLE open-pane, which MUST show a
// pane appear. An "unchanged" reading proves nothing unless the probe is shown
// to be capable of changing.
func TestIsolatedSlotRoundTrip(t *testing.T) {
	c, self := agentPaneFixture(t)
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")
	before := panesInWindow(t, window)

	unchanged := func(stage string) {
		t.Helper()
		if after := panesInWindow(t, window); len(after) != len(before) {
			t.Fatalf("the user's window went from %d panes to %d after %s (%v → %v): the slot is "+
				"not isolated at all, it is an ordinary split", len(before), len(after), stage,
				before, after)
		}
	}

	var watch map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"slot":     2,
		"isolated": true,
		"command":  "printf 'IS-READY\\n'",
		"pattern":  "^IS-READY$",
		"timeout":  20,
	}, &watch)
	if slot, _ := watch["slot"].(float64); int(slot) != 2 {
		t.Fatalf("start-and-watch answered for slot %v, want 2: %v", watch["slot"], watch)
	}
	if watch["created"] != true {
		t.Fatalf("the first call on isolated slot 2 reported created=%v, want true", watch["created"])
	}
	if event, _ := watch["event"].(string); !strings.HasPrefix(event, "pattern:") {
		t.Fatalf("the readiness pattern did not fire in the isolated pane: event %q, output %q",
			event, watch["output"])
	}
	unchanged("start-and-watch on an isolated slot")

	// Every call from here omits isolated, which is what the tri-state makes
	// legal — and what makes an invisible pane usable at all.
	c.callToolJSON(t, "send-keys", map[string]any{
		"slot":  2,
		"keys":  "env PS1='REPL> ' sh --norc --noprofile",
		"enter": true,
	}, &map[string]any{})
	sleep(500 * time.Millisecond)

	var repl map[string]any
	c.callToolJSON(t, "run-in-repl", map[string]any{
		"slot":          2,
		"input":         "echo $((1 + 1))",
		"promptPattern": "^REPL>",
		"timeout":       10,
	}, &repl)
	if output, _ := repl["output"].(string); !strings.Contains(output, "2") {
		t.Errorf("run-in-repl on the isolated slot answered %q, want it to contain \"2\"", output)
	}
	if repl["created"] != false {
		t.Errorf("run-in-repl reported created=%v for a slot that already existed; an agent told "+
			"true concludes its process died", repl["created"])
	}
	unchanged("run-in-repl on an isolated slot")

	listed := listedSlots(t, c)
	entry, ok := listed[2]
	if !ok {
		t.Fatalf("list-slots does not report the isolated slot 2: %v", listed)
	}
	if !entry.Isolated {
		t.Errorf("list-slots reports slot 2 as visible; isolated is the only thing that tells the "+
			"two kinds apart: %+v", entry)
	}

	captured := c.callToolText(t, "capture-pane", map[string]any{"slot": 2})
	if !strings.Contains(captured, "REPL>") {
		t.Errorf("capture-pane on the isolated slot did not read the REPL's screen:\n%s", captured)
	}
	unchanged("capture-pane on an isolated slot")

	entries := closedSlots(t, c, map[string]any{"slot": 2})
	if len(entries) != 1 || entries[0].Action != actionKilled {
		t.Fatalf("close-pane({slot:2}) answered %+v, want one killed entry", entries)
	}
	if _, still := listedSlots(t, c)[2]; still {
		t.Error("list-slots still reports slot 2 after it was closed")
	}
	unchanged("close-pane on an isolated slot")

	// Control (b): the probe can see a pane appear. Without this, "unchanged"
	// above is equally consistent with a probe that never changes.
	c.callToolJSON(t, "open-pane", map[string]any{"slot": 3}, &map[string]any{})
	if after := panesInWindow(t, window); len(after) != len(before)+1 {
		t.Fatalf("a deliberate VISIBLE open-pane took the window from %d panes to %d, want %d: the "+
			"pane-count probe cannot observe a creation, so every \"unchanged\" above is vacuous",
			len(before), len(after), len(before)+1)
	}
}

// closedSlots calls close-pane and decodes the typed entries, because these
// tests assert on the action and on the detail rather than merely on the count.
func closedSlots(t *testing.T, c *mcpClient, args map[string]any) []closedPane {
	t.Helper()
	var out []closedPane
	c.callToolJSON(t, "close-pane", args, &out)
	return out
}

// listedSlots calls list-slots and keys the answer by slot number.
func listedSlots(t *testing.T, c *mcpClient) map[int]slotListing {
	t.Helper()
	var listings []slotListing
	c.callToolJSON(t, "list-slots", map[string]any{}, &listings)
	byslot := make(map[int]slotListing, len(listings))
	for _, entry := range listings {
		byslot[entry.Slot] = entry
	}
	return byslot
}

// ---- 9.5: the kind is fixed once a slot exists ----

// TestKindIsFixed is the tri-state pinned from both sides.
//
// A slot is ONE pane. Asking for the other kind of an existing slot is refused
// rather than satisfied with a second pane, because two panes answering to one
// number make every later call ambiguous — and one of them would be invisible.
//
// # The control
//
// The same call with isolated OMITTED succeeds on BOTH kinds, reporting
// created:false. That is what proves the refusals come from the kind conflict
// and not merely from "the slot exists" — and it is the assertion that pins the
// tri-state, because a two-valued isolated flag cannot make both of those pass.
func TestKindIsFixed(t *testing.T) {
	c, _ := agentPaneFixture(t)

	visible := openedSlot(t, c, map[string]any{"slot": 3})
	if visible.Isolated || !visible.created() {
		t.Fatalf("the fixture did not open a visible slot 3: %+v", visible)
	}
	res := callTool(t, c, "open-pane", map[string]any{"slot": 3, "isolated": true})
	if !res.IsError {
		t.Fatalf("open-pane({slot:3, isolated:true}) succeeded on a visible slot 3: %v", res)
	}
	if got, want := res.text(t, "open-pane"), fmt.Sprintf(kindIsVisibleText, 3); got != want {
		t.Errorf("the refusal reads %q, want exactly %q", got, want)
	}

	isolated := openedSlot(t, c, map[string]any{"slot": 4, "isolated": true})
	if !isolated.Isolated || !isolated.created() {
		t.Fatalf("the fixture did not open an isolated slot 4: %+v", isolated)
	}
	// EXPLICIT false, not omission. This is the mirror the tri-state exists for:
	// omitting the argument here has to succeed, and sending false has to fail.
	res = callTool(t, c, "open-pane", map[string]any{"slot": 4, "isolated": false})
	if !res.IsError {
		t.Fatalf("open-pane({slot:4, isolated:false}) succeeded on an isolated slot 4: %v", res)
	}
	if got, want := res.text(t, "open-pane"), fmt.Sprintf(kindIsIsolatedText, 4); got != want {
		t.Errorf("the refusal reads %q, want exactly %q", got, want)
	}

	// The control, on both kinds.
	again := openedSlot(t, c, map[string]any{"slot": 3})
	if again.created() || again.Isolated {
		t.Errorf("open-pane({slot:3}) with no kind reported %+v, want the existing VISIBLE pane "+
			"with created:false", again)
	}
	again = openedSlot(t, c, map[string]any{"slot": 4})
	if again.created() || !again.Isolated {
		t.Errorf("open-pane({slot:4}) with no kind reported %+v, want the existing ISOLATED pane "+
			"with created:false — an omitted kind accepts whichever kind owns the slot, and a slot "+
			"nobody can see is otherwise unreachable for the rest of the session", again)
	}
}

// openedSlot calls open-pane and decodes its answer, keeping created a pointer
// so "absent" stays distinguishable from "false".
func openedSlot(t *testing.T, c *mcpClient, args map[string]any) openedPane {
	t.Helper()
	var got openedPane
	c.callToolJSON(t, "open-pane", args, &got)
	if got.Created == nil {
		t.Fatalf("open-pane is a creating tool and must always report created: %v", args)
	}
	return got
}

func (o openedPane) created() bool { return o.Created != nil && *o.Created }

// ---- 9.6: isolated needs a slot, except for the one-shot ----

// TestIsolatedNeedsSlot covers the argument combination that has no pane at the
// end of it, and the one tool where it does.
//
// An invisible pane with no number is unreachable the moment the call returns:
// nothing can capture it, watch it or close it, and nobody can see it to close
// it by hand. So every tool that LEAVES a pane behind refuses that combination.
// execute-command does not leave one behind — it runs a command to completion
// and returns the output — so there it means a one-shot pane, created, used and
// destroyed inside the call.
//
// # The controls
//
// (a) A socket-level probe around the ephemeral call. list-slots is structurally
// blind to a one-shot pane, because that pane is deliberately never claimed —
// so "list-slots is empty afterwards" would be vacuous for exactly the leak it
// is meant to catch. The pane and session counts on the isolated socket are not.
// list-slots is asserted too, as the cheaper statement of the same thing.
//
// (b) The same call WITH a slot succeeds, proving the refusal is about the
// missing slot rather than about isolated panes being broken.
func TestIsolatedNeedsSlot(t *testing.T) {
	c, _ := agentPaneFixture(t)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"open-pane", map[string]any{"isolated": true}},
		{"send-keys", map[string]any{"isolated": true, "keys": "echo no-slot", "enter": true}},
		{"write-to-display", map[string]any{"isolated": true, "text": "no-slot"}},
		{"start-and-watch", map[string]any{
			"isolated": true, "command": "true", "pattern": "never-matches", "timeout": 3,
		}},
		{"run-in-repl", map[string]any{
			"isolated": true, "input": "", "promptPattern": "\\$|>|#|%", "timeout": 3,
		}},
	} {
		res := callTool(t, c, tc.tool, tc.args)
		if !res.IsError {
			t.Errorf("%s accepted isolated with no slot: it would leave a pane nothing can ever "+
				"reach again", tc.tool)
			continue
		}
		if got := res.text(t, tc.tool); got != isolatedNeedsSlotText {
			t.Errorf("%s refused with %q, want exactly %q", tc.tool, got, isolatedNeedsSlotText)
		}
	}

	panesBefore := len(isolatedPaneIDs(t))
	sessionsBefore := len(isolatedSessionNames(t))

	body := callTool(t, c, "execute-command", map[string]any{
		"isolated": true,
		"command":  "echo one-shot-marker",
	}).text(t, "execute-command")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("the ephemeral execute-command did not answer with JSON: %v\n%s", err, body)
	}
	// Read the RAW keys: the absence of slot is the contract, and a decode into
	// a struct would report a zero for a key that is not there.
	if _, has := raw["slot"]; has {
		t.Errorf("the ephemeral form reported a slot: the pane was destroyed inside the call, so "+
			"that number reaches nothing — %s", body)
	}
	if _, has := raw["created"]; has {
		t.Errorf("the ephemeral form reported created: no slot was opened — %s", body)
	}
	var output string
	if err := json.Unmarshal(raw["output"], &output); err != nil || !strings.Contains(output, "one-shot-marker") {
		t.Errorf("the ephemeral command's output is %q, want it to contain the marker", output)
	}
	if string(raw["exitCode"]) != "0" {
		t.Errorf("the ephemeral command exited %s, want 0", raw["exitCode"])
	}
	if string(raw["timedOut"]) != "false" {
		t.Errorf("the ephemeral command reported timedOut=%s", raw["timedOut"])
	}

	// Control (a): the socket is back where it started.
	if got := len(isolatedPaneIDs(t)); got != panesBefore {
		t.Errorf("the isolated socket holds %d panes, want the %d it started with: the one-shot "+
			"pane leaked, and nothing can ever see or close it", got, panesBefore)
	}
	if got := len(isolatedSessionNames(t)); got != sessionsBefore {
		t.Errorf("the isolated socket holds %d sessions, want the %d it started with", got, sessionsBefore)
	}
	if listed := listedSlots(t, c); len(listed) != 0 {
		t.Errorf("list-slots reports %v after a one-shot command; the pane is deliberately never "+
			"claimed, so it must not register as a slot", listed)
	}

	// Control (b): with a slot, the same request works.
	var watch map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"slot": 2, "isolated": true,
		"command": "printf 'WITH-SLOT\\n'", "pattern": "^WITH-SLOT$", "timeout": 20,
	}, &watch)
	if event, _ := watch["event"].(string); !strings.HasPrefix(event, "pattern:") {
		t.Fatalf("the refusal above is not about the missing slot: the same call WITH a slot also "+
			"failed (event %q, output %q)", event, watch["output"])
	}
}

// ---- 9.8: one namespace per server ----

// TestNamespaceIsolation is the property that makes a shared socket safe: two
// agents on one machine put their invisible panes on the SAME tmux server, and
// neither can see, type into, or close the other's.
//
// # The controls
//
// (a) A different sentinel is written into each server's slot 1, and each server
// captures only its own. Without that, two servers quietly SHARING one pane
// satisfies every count-based assertion here: one entry each, one pane alive
// after the sweep.
//
// (b) Within one server, a second open-pane on the same slot returns
// created:false and the pane it already had. That proves "sees only its own" is
// not "sees nothing" — a namespace filter that matched nothing would pass every
// other assertion.
//
// (c) A third server with no window at all, which is the case the pid-based
// namespace key exists for and the one a two-pane design could never reach.
func TestNamespaceIsolation(t *testing.T) {
	a, _ := agentPaneFixtureNamed(t, "a")
	b, _ := agentPaneFixtureNamed(t, "b")

	for _, c := range []*mcpClient{a, b} {
		opened := openedSlot(t, c, map[string]any{"slot": 1, "isolated": true})
		if !opened.created() || !opened.Isolated {
			t.Fatalf("a server's first isolated slot 1 reported %+v, want created and isolated", opened)
		}
	}

	// Control (b): the second ask is answered from the namespace, not by a
	// second pane.
	if again := openedSlot(t, a, map[string]any{"slot": 1, "isolated": true}); again.created() {
		t.Error("the second open-pane on server A's own isolated slot 1 created another pane; " +
			"a namespace that matches nothing looks exactly like a namespace that is isolated")
	}

	// Control (a): distinct sentinels.
	writeSentinel(t, a, 1, "SENTINEL-ALPHA")
	writeSentinel(t, b, 1, "SENTINEL-BRAVO")
	sleep(600 * time.Millisecond)

	assertSees(t, a, 1, "SENTINEL-ALPHA", "SENTINEL-BRAVO")
	assertSees(t, b, 1, "SENTINEL-BRAVO", "SENTINEL-ALPHA")

	for _, c := range []*mcpClient{a, b} {
		listed := listedSlots(t, c)
		if len(listed) != 1 {
			t.Errorf("a server lists %v, want exactly its own isolated slot 1", listed)
		}
		if entry, ok := listed[1]; ok && !entry.Isolated {
			t.Errorf("slot 1 is reported as visible: %+v", entry)
		}
	}

	// A's teardown must not reach B.
	entries := closedSlots(t, a, map[string]any{"slot": "all"})
	if len(entries) != 1 || entries[0].Action != actionKilled {
		t.Fatalf("close-pane({slot:\"all\"}) on server A answered %+v, want one killed entry", entries)
	}
	assertSees(t, b, 1, "SENTINEL-BRAVO", "SENTINEL-ALPHA")
	if listed := listedSlots(t, b); len(listed) != 1 {
		t.Errorf("server B lists %v after server A swept its namespace; A's teardown reached into "+
			"B's", listed)
	}
	if listed := listedSlots(t, a); len(listed) != 0 {
		t.Errorf("server A still lists %v after sweeping everything it had open", listed)
	}

	// Control (c): a server that was never inside tmux. Its namespace comes from
	// its pid, not from a pane, which is why this case exists at all.
	outside := newMCPClient(t)
	opened := openedSlot(t, outside, map[string]any{"slot": 1, "isolated": true})
	if !opened.created() || !opened.Isolated {
		t.Fatalf("a server outside tmux could not open an isolated slot: %+v", opened)
	}
	writeSentinel(t, outside, 1, "SENTINEL-CHARLIE")
	sleep(600 * time.Millisecond)
	assertSees(t, outside, 1, "SENTINEL-CHARLIE", "SENTINEL-BRAVO")
	if entries := closedSlots(t, outside, map[string]any{"slot": "all"}); len(entries) != 1 {
		t.Errorf("a server outside tmux swept %+v, want its one isolated pane", entries)
	}
}

// agentPaneFixtureNamed is agentPaneFixture with a suffix, for the tests that
// need two servers alive at once: the session name is derived from the test
// name, so two unsuffixed fixtures in one test would collide.
func agentPaneFixtureNamed(t *testing.T, suffix string, extraArgs ...string) (*mcpClient, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux")
	}
	name := uniqueSession(t) + "-" + suffix
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })

	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
	return newMCPClientInPane(t, self, extraArgs...), self
}

// writeSentinel echoes a unique marker into a slot, addressing it WITHOUT the
// isolated argument — which is the way every later call reaches an invisible
// pane.
func writeSentinel(t *testing.T, c *mcpClient, slot int, marker string) {
	t.Helper()
	c.callToolJSON(t, "send-keys", map[string]any{
		"slot": slot, "keys": "echo " + marker, "enter": true,
	}, &map[string]any{})
}

func assertSees(t *testing.T, c *mcpClient, slot int, want, mustNotSee string) {
	t.Helper()
	captured := c.callToolText(t, "capture-pane", map[string]any{"slot": slot})
	if !strings.Contains(captured, want) {
		t.Errorf("slot %d does not show %s:\n%s", slot, want, captured)
	}
	if strings.Contains(captured, mustNotSee) {
		t.Errorf("slot %d shows %s, which belongs to another server: the two are sharing one "+
			"pane, and every count-based assertion in this test would still pass:\n%s",
			slot, mustNotSee, captured)
	}
}

// ---- 9.11: isolated slots are never adopted ----

// TestIsolatedNeverAdopts pins an ABSENCE, which is why it needs the mirror
// below more than most tests need a control.
//
// Adoption exists to reuse a shell the USER left idle in the window they are
// looking at. The isolated socket has no user and no window, and every pane on
// it was made by an agent — so a predicate that could adopt there would be a
// predicate that could type into ANOTHER SERVER'S pane, with the namespace
// filter as the only thing preventing it. It must not become the only thing.
//
// # The mirror
//
// The same shape on the VISIBLE side must ADOPT. Without it, this test passes
// just as well when the adoption machinery is broken everywhere, and the
// abstention it is asserting would be an accident rather than a decision.
func TestIsolatedNeverAdopts(t *testing.T) {
	sl, self := slotFixture(t)
	b := backendOf(t, sl)
	ctx := context.Background()

	// An idle, unmarked shell inside OUR OWN namespace — the most adoptable pane
	// that could possibly exist on that socket.
	decoy := openIsolatedSessionNamed(t, b.namespacePrefix()+"decoy")
	waitForClientPaneIdle(t, sl.b, decoy)

	tgt, err := sl.resolveHelper(ctx, 7, kindIsolated)
	if err != nil {
		t.Fatalf("resolve isolated slot 7: %v", err)
	}
	if tgt.Ref == decoy {
		t.Fatal("isolated slot 7 adopted an idle shell that was already on the socket. On that " +
			"socket the only thing distinguishing another server's pane from an adoptable one is " +
			"the namespace filter, and a predicate that adopts here makes that filter the sole " +
			"guard against typing into someone else's REPL")
	}
	if !tgt.Created {
		t.Errorf("isolated slot 7 reported created=%v, want true: it had to make its own pane", tgt.Created)
	}

	records, err := b.IsolatedRecords(ctx)
	if err != nil {
		t.Fatalf("read this namespace's registry: %v", err)
	}
	if _, claimed := records[decoy]; claimed {
		t.Error("the decoy was marked, so something claimed it even though it was not returned")
	}
	if !isolatedPaneExists(t, decoy) {
		t.Error("the decoy pane is gone; resolution may not destroy a pane it did not create")
	}

	// The mirror. A different slot number, because slot 7 is now isolated and
	// asking for the visible one would be refused by the kind rule this test is
	// not about.
	usersPane := newPaneRef(tmuxExec(t, "split-window", "-d", "-t", self.target(), "-P", "-F", "#{pane_id}"))
	waitForClientPaneIdle(t, sl.b, usersPane)

	visible, err := sl.resolveHelper(ctx, 8, kindVisible)
	if err != nil {
		t.Fatalf("resolve visible slot 8: %v", err)
	}
	if visible.Ref != usersPane || visible.Owner != ownerAcquired {
		t.Fatalf("visible slot 8 resolved to %s (owner %q) rather than adopting the user's idle "+
			"shell %s: the adoption machinery is not running, so the isolated abstention above "+
			"proves nothing", visible.Ref.target(), visible.Owner, usersPane.target())
	}
}

// ---- 9.12: the duplicate loser ----

// TestIsolatedDuplicateLoserIsClosed is the one place the isolated rule is the
// OPPOSITE of the visible one, and the mirror below is what makes that
// deliberate rather than a bug.
//
// Two servers can claim one slot, because tmux has no compare-and-set on
// options. The oldest pane wins either way. What happens to the loser differs:
// a visible loser is merely unslotted, because the user can see it and close it
// by hand, and it may be running the long process whose survival is the whole
// reason the oldest wins. An invisible loser unslotted is a live shell nothing
// can list, reach or reap for the lifetime of the machine — so it is closed.
func TestIsolatedDuplicateLoserIsClosed(t *testing.T) {
	sl, b := isolatedFixture(t)
	ctx := context.Background()

	first, err := b.OpenIsolated(ctx)
	if err != nil {
		t.Fatalf("open the first isolated pane: %v", err)
	}
	second, err := b.OpenIsolated(ctx)
	if err != nil {
		t.Fatalf("open the second isolated pane: %v", err)
	}
	// Written directly, because the second server is not part of this test.
	for _, pane := range []paneRef{first, second} {
		if err := b.Claim(ctx, pane, ownerAgent, 5); err != nil {
			t.Fatalf("claim %v as isolated slot 5: %v", pane, err)
		}
	}
	winner, loser := first, second
	if paneSeq(second.target()) < paneSeq(first.target()) {
		winner, loser = second, first
	}

	tgt, err := sl.resolveHelper(ctx, 5, kindIsolated)
	if err != nil {
		t.Fatalf("resolve isolated slot 5: %v", err)
	}
	if tgt.Ref != winner {
		t.Fatalf("resolution kept the newer pane; the oldest wins so that whatever the caller "+
			"already started there survives (kept %v)", tgt.Ref)
	}
	if isolatedPaneExists(t, loser) {
		t.Error("the losing isolated pane is still alive with its slot taken away. Nobody can see " +
			"it, no tool can reach it, and it pins a live shell and the server behind it until the " +
			"machine reboots — which is why the visible rule cannot be reused here")
	}
}

// TestVisibleDuplicateLoserSurvives is the mirror of the test above: the same
// race on the visible side leaves an agent-owned loser ALIVE and unslotted.
//
// Its value is entirely comparative. Two rules that differ are a decision only
// if both are asserted; one asserted rule and one assumption is how the second
// one quietly becomes the first.
func TestVisibleDuplicateLoserSurvives(t *testing.T) {
	sl, self := slotFixture(t)
	ctx := context.Background()

	first := newPaneRef(tmuxExec(t, "split-window", "-d", "-t", self.target(), "-P", "-F", "#{pane_id}"))
	second := newPaneRef(tmuxExec(t, "split-window", "-d", "-t", self.target(), "-P", "-F", "#{pane_id}"))
	for _, pane := range []paneRef{first, second} {
		if err := sl.b.Claim(ctx, pane, ownerAgent, 5); err != nil {
			t.Fatalf("claim %s as visible slot 5: %v", pane.target(), err)
		}
	}
	winner, loser := first, second
	if paneSeq(second.target()) < paneSeq(first.target()) {
		winner, loser = second, first
	}

	tgt, err := sl.resolveHelper(ctx, 5, kindVisible)
	if err != nil {
		t.Fatalf("resolve visible slot 5: %v", err)
	}
	if tgt.Ref != winner {
		t.Fatalf("resolution kept %s, want the oldest pane %s", tgt.Ref.target(), winner.target())
	}
	if !paneExists(t, loser.target()) {
		t.Fatal("the visible loser was killed. It may be running the long process the oldest-wins " +
			"rule exists to preserve, and the user can see it either way")
	}
	if got := tmuxExec(t, "display-message", "-t", loser.target(), "-p", "#{"+paneOptSlot+"}"); got != "" {
		t.Errorf("the visible loser still answers to slot %q; healing has to take the number away "+
			"even though it leaves the pane", got)
	}
}

// ---- 9.13: reclaiming what was never claimed ----

// TestPartiallyClaimedIsolatedPaneIsReclaimed covers the pane that exists and is
// not in the registry: a process that died between creating it and claiming it.
//
// On the visible side an unmarked pane is presumed the USER'S, and killing it is
// the one unrecoverable action in this design. On the isolated socket the
// presumption inverts, because every pane inside our namespace was made by this
// process — attribution is the session's name, and it is written atomically with
// the pane.
//
// # The control
//
// An identically unclaimed pane in ANOTHER namespace survives the same sweep.
// That is the difference between reclaiming our own orphans and killing a
// neighbour's panes, and a socket-scoped sweep would pass every other assertion
// here. The neighbour's namespace names a LIVE pid, so the startup reaper cannot
// be what spares it.
func TestPartiallyClaimedIsolatedPaneIsReclaimed(t *testing.T) {
	sl, b := isolatedFixture(t)
	ctx := context.Background()

	orphan, err := b.OpenIsolated(ctx)
	if err != nil {
		t.Fatalf("open the orphan: %v", err)
	}
	// No Claim: this is the state a process that died mid-creation leaves.
	if records, err := b.IsolatedRecords(ctx); err != nil {
		t.Fatalf("read this namespace's registry: %v", err)
	} else if _, claimed := records[orphan]; claimed {
		t.Fatal("the fixture claimed the orphan, so it is not the state this test is about")
	}

	neighbour := openIsolatedSessionNamed(t,
		fmt.Sprintf("mcp-%d-notourkey-neighbour", os.Getpid()))

	entries, err := sl.closePanes(ctx, closeSelector{All: true})
	if err != nil {
		t.Fatalf("close everything: %v", err)
	}

	if isolatedPaneExists(t, orphan) {
		t.Error("the sweep left an unclaimed pane in this server's own namespace alive. Nobody " +
			"can see it, no tool can list or reach it, and there is no window to close it in")
	}
	if !isolatedPaneExists(t, neighbour) {
		t.Fatal("the sweep killed an unclaimed pane in ANOTHER server's namespace. That is the " +
			"whole hazard of a shared socket: a namespace-scoped sweep reclaims our orphans, and " +
			"a socket-scoped one destroys a neighbour's work")
	}

	// The entry says what happened, rather than the sweep destroying something
	// silently. Slot 0 is not a slot, and the detail is what stops it reading as
	// a bug.
	if len(entries) != 1 {
		t.Fatalf("the sweep reported %+v, want one entry for the pane it reclaimed", entries)
	}
	if entries[0].Slot != 0 || entries[0].Action != actionKilled || entries[0].Detail == "" {
		t.Errorf("the reclaimed pane is reported as %+v, want slot 0, killed, and a detail saying "+
			"why it has no number", entries[0])
	}
}

// ---- D8: orphaned namespaces ----

// TestOrphanedNamespacesAreReapedByPidLiveness covers what happens to invisible
// panes when the server that made them is gone.
//
// A namespace outlives its server: a crashed or killed process leaves live
// shells in a named session that nothing reclaims until the machine reboots.
// Nobody can see them, and no later server inherits them — the nonce in the name
// is there precisely to stop that, because inheriting a dead server's slots
// means typing into another session's REPL.
//
// The reaper fails safe in the one direction that matters. A live server's pid
// is by definition running, so a live namespace is never reaped; the residual
// risk is pid REUSE, which can only make the reaper DECLINE and leave the
// orphan. Both directions are asserted below, and so is the third case that
// matters on a shared socket: a session this package did not name is left
// entirely alone.
func TestOrphanedNamespacesAreReapedByPidLiveness(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	orphanName := fmt.Sprintf("mcp-%d-gone-x", exitedPID(t))
	liveName := fmt.Sprintf("mcp-%d-alive-x", os.Getpid())
	const foreignName = "someone-elses-session"

	orphan := openIsolatedSessionNamed(t, orphanName)
	live := openIsolatedSessionNamed(t, liveName)
	foreign := openIsolatedSessionNamed(t, foreignName)

	// Construction is when the sweep runs: before the server answers its first
	// request, and never again.
	_ = newTmuxBackend(newTmuxClient("bash"))

	if isolatedPaneExists(t, orphan) {
		t.Errorf("the namespace of a dead process survived (%s): its shells stay alive, invisible "+
			"and unreachable, until the machine reboots", orphanName)
	}
	if !isolatedPaneExists(t, live) {
		t.Errorf("a namespace whose pid is RUNNING was reaped (%s). That is another agent's panes "+
			"destroyed mid-session, which is why the liveness check has to fail towards leaving "+
			"orphans rather than towards killing", liveName)
	}
	if !isolatedPaneExists(t, foreign) {
		t.Errorf("a session this package did not name was reaped (%s). The socket is shared, and "+
			"the only thing we know about a name we did not write is that it is not ours to "+
			"touch", foreignName)
	}
}

// exitedPID returns a pid that named a process and no longer does — the real
// shape of the situation, rather than a number chosen to be implausible.
//
// It confirms the exit with the same signal-0 probe the reaper uses, so the test
// cannot proceed on an assumption about what wait() means.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a process to kill: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if processIsRunning(pid) {
		t.Fatalf("pid %d is still running after being killed and reaped; the fixture cannot "+
			"produce a dead pid", pid)
	}
	return pid
}

// TestProcessIsRunningTreatsPermissionAsAlive pins the direction the liveness
// check has to fail in.
//
// A pid we may not signal EXISTS — the error is the answer, not a failure — and
// reporting it dead would have the reaper kill the namespace of a live server
// running as another user. Pid 1 is the portable case: it is always running and
// never ours.
func TestProcessIsRunningTreatsPermissionAsAlive(t *testing.T) {
	if !processIsRunning(os.Getpid()) {
		t.Error("this very process is reported as not running")
	}
	if !processIsRunning(1) {
		t.Error("pid 1 is reported as not running; a permission error means the process exists, " +
			"and reading it as \"gone\" would reap a live server's panes")
	}
	if processIsRunning(int(^uint32(0) >> 1)) {
		t.Error("a pid above any possible pid_max is reported as running, so nothing would ever " +
			"be reaped")
	}
	// And the probe delivers nothing: signal 0 performs the permission check and
	// stops there. If it were a real signal this test would have killed the
	// suite.
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Errorf("signal 0 to ourselves failed: %v", err)
	}
}

// TestNamespacePIDReadsOnlyNamesWeWrote pins the parse, because everything the
// reaper does downstream of it is a kill.
func TestNamespacePIDReadsOnlyNamesWeWrote(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		ok   bool
	}{
		{"mcp-4711-a1b2c3d4-4f0c8b1e-uuid", 4711, true},
		{"mcp-1-x-y", 1, true},
		{"someone-elses-session", 0, false},
		{"mcp-notanumber-x", 0, false},
		{"mcp--x", 0, false},
		{"mcp-4711", 0, false}, // no nonce: not a name this package writes
		{"", 0, false},
		{"mcp-0-x-y", 0, false},
		{"mcp--1-x", 0, false},
	} {
		pid, ok := namespacePID(tc.name)
		if ok != tc.ok || pid != tc.pid {
			t.Errorf("namespacePID(%q) = (%d, %v), want (%d, %v)", tc.name, pid, ok, tc.pid, tc.ok)
		}
	}
}

// ---- 9.16: the mark write order ----

// TestMarkWriteOrder pins the order registry marks are written in, as data and
// as behaviour.
//
// tmux cannot set several options atomically, so every PREFIX of the sequence is
// a state a crashed or cancelled process can leave behind. The order is chosen
// so that every prefix is INERT: resolution requires all three marks, so
// witness-only and witness+owner steer nothing. The reverse order would publish
// a slot marker on a pane of unknown ownership — and the teardown rule keys off
// exactly that owner value to decide whether a pane may be killed or must merely
// be released.
//
// The whole slice is asserted, not its ends: an assertion on the first and last
// elements passes with the middle pair swapped, and passes with a fourth mark
// inserted anywhere between them.
func TestMarkWriteOrder(t *testing.T) {
	want := []string{paneOptWitness, paneOptOwner, paneOptSlot}
	if !slices.Equal(markOrder, want) {
		t.Fatalf("markOrder is %v, want exactly %v", markOrder, want)
	}
	if !slotMarkerIsLast(markOrder) {
		t.Errorf("the slot marker is not written last in %v", markOrder)
	}
	// The control: the same analysis on a reversed order must report it unsafe.
	// Without it, slotMarkerIsLast could be returning true for everything.
	if reversed := []string{paneOptSlot, paneOptOwner, paneOptWitness}; slotMarkerIsLast(reversed) {
		t.Errorf("%v is reported safe, but its first prefix publishes a slot marker on a pane "+
			"with no recorded owner — the one record this server must never produce", reversed)
	}

	// One writer, driven by the slice. A second set-option statement in either
	// walker is a second order nobody re-derives the safety of.
	for _, fn := range []string{"markPaneOwnedAs", "clearPaneRegistration"} {
		writes, usesOrder := setOptionWritesIn(t, "backend_tmux.go", fn)
		if writes != 1 {
			t.Errorf("%s issues %d set-option calls, want exactly 1: the order has to come from "+
				"markOrder, not from a sequence of statements", fn, writes)
		}
		if !usesOrder {
			t.Errorf("%s does not reference markOrder, so its write order is restated rather than "+
				"derived — and a fourth mark added to the slice would not reach it", fn)
		}
	}
}

// slotMarkerIsLast reports whether no PROPER prefix of an order carries the slot
// marker, which is the property the whole ordering argument reduces to.
func slotMarkerIsLast(order []string) bool {
	for i := 0; i < len(order)-1; i++ {
		if order[i] == paneOptSlot {
			return false
		}
	}
	return len(order) > 0 && order[len(order)-1] == paneOptSlot
}

// setOptionWritesIn counts the "set-option" literals in one function and reports
// whether that function mentions markOrder.
func setOptionWritesIn(t *testing.T, file, fnName string) (writes int, usesOrder bool) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	found := false
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != fnName {
			continue
		}
		found = true
		ast.Inspect(fn, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.BasicLit:
				if node.Value == `"set-option"` {
					writes++
				}
			case *ast.Ident:
				if node.Name == "markOrder" {
					usesOrder = true
				}
			}
			return true
		})
	}
	if !found {
		t.Fatalf("%s declares no function named %s; this test is watching a symbol that has moved",
			file, fnName)
	}
	return writes, usesOrder
}

// TestEveryPrefixOfAClaimIsInert is the behavioural half of the order argument,
// driven against real tmux: a pane carrying only the first mark, or only the
// first two, must be invisible to resolution.
//
// The prefixes are written directly rather than by injecting a failure into
// Claim, because the state is what matters and a fault injector would be a
// second implementation of the write order — the exact duplication the slice
// exists to remove.
func TestEveryPrefixOfAClaimIsInert(t *testing.T) {
	sl, self := slotFixture(t)
	ctx := context.Background()

	pane := newPaneRef(tmuxExec(t, "split-window", "-d", "-t", self.target(), "-P", "-F", "#{pane_id}"))

	for i := 1; i < len(markOrder); i++ {
		prefix := markOrder[:i]
		tmuxExec(t, append([]string{"set-option", "-p", "-t", pane.target()},
			markOrder[i-1], valueForMark(t, markOrder[i-1], pane, 4))...)

		reg, err := sl.b.Records(ctx, self)
		if err != nil {
			t.Fatalf("read the registry after writing %v: %v", prefix, err)
		}
		rec, present := reg[pane]
		if present && rec.Slot != 0 {
			t.Fatalf("after the prefix %v the pane answers to slot %d. A half-claimed pane must "+
				"steer nothing: every prefix of a claim is a state a cancelled process leaves "+
				"behind", prefix, rec.Slot)
		}
	}

	// And the complete claim IS visible, so the assertions above are about
	// prefixes rather than about a registry that reports nothing.
	tmuxExec(t, "set-option", "-p", "-t", pane.target(), paneOptSlot, "4")
	reg, err := sl.b.Records(ctx, self)
	if err != nil {
		t.Fatalf("read the registry after the complete claim: %v", err)
	}
	if rec, ok := reg[pane]; !ok || rec.Slot != 4 {
		t.Fatalf("a fully claimed pane is not in the registry as slot 4 (%+v, present=%v); the "+
			"prefix assertions above prove nothing if nothing ever registers", rec, ok)
	}
}

// valueForMark is the value each mark carries, so the prefix test writes the
// same thing Claim would.
func valueForMark(t *testing.T, mark string, pane paneRef, slot int) string {
	t.Helper()
	switch mark {
	case paneOptWitness:
		return pane.target()
	case paneOptOwner:
		return ownerAgent
	case paneOptSlot:
		return fmt.Sprint(slot)
	}
	t.Fatalf("no value defined for the mark %q; markOrder grew and this test did not", mark)
	return ""
}

// ---- 9.17: pane ordinals ----

// TestPaneSeqOrdersNumerically is the coverage the "%10 before %9" inversion has
// never had.
//
// Three rules are about AGE: keep the oldest pane when two servers race for a
// slot, consider adoption candidates oldest-first, and break geometry ties
// deterministically. All three rank panes by this ordinal, and sorting pane ids
// as STRINGS is wrong in a way that only appears after a window has passed ten
// panes — at which point "keep the oldest" starts keeping the newest, silently,
// and only under a race.
func TestPaneSeqOrdersNumerically(t *testing.T) {
	if paneSeq("%10") <= paneSeq("%9") {
		t.Errorf("paneSeq(%%10)=%d is not greater than paneSeq(%%9)=%d", paneSeq("%10"), paneSeq("%9"))
	}
	if got := paneSeq(headlessPrefix + "%12"); got != 12 {
		t.Errorf("paneSeq of an isolated handle = %d, want 12: the prefix routes the command and "+
			"must not change the pane's age", got)
	}
	if got := paneSeq("not-a-pane"); got != math.MaxInt {
		t.Errorf("paneSeq of an unparseable id = %d, want MaxInt so a pane we cannot rank never "+
			"wins a tie-break by accident", got)
	}

	// The control: the same two ids sorted as strings put them the other way
	// round. Without it, this test passes on a numeric ordering that was never
	// in doubt and says nothing about the bug.
	ids := []string{"%10", "%9"}
	lexical := slices.Clone(ids)
	sort.Strings(lexical)
	numeric := slices.Clone(ids)
	sort.Slice(numeric, func(i, j int) bool { return paneSeq(numeric[i]) < paneSeq(numeric[j]) })
	if lexical[0] != "%10" || numeric[0] != "%9" {
		t.Errorf("lexical order gives %v and numeric order gives %v; the two must disagree here, "+
			"or this pair is not the inversion the ordinal exists to fix", lexical, numeric)
	}
}

// ---- "Empty" and "I could not look" are different answers ----

// TestIsolatedPanesTellsEmptyFromUnreadable pins the discrimination the sweep
// depends on.
//
// The namespace read used to answer "empty, no error" for EVERY failure. The
// rationale was written for resolution, where it holds: an unstated kind
// consults that registry on its way past, so an error there would fail a plain
// send-keys on any machine that has never opened an invisible pane, and reading
// a hiccup as "empty" costs one duplicate pane that heals itself.
//
// It does not hold for the other caller. To teardown, "empty" means "there was
// nothing to clean up" — so a namespace it could not read was reported as a
// completed sweep, with no entry and no error, while the shells were still
// running. A cancelled caller context is enough to produce it, because
// runWithSocket runs tmux under CommandContext.
//
// So the two views now differ, and both halves are asserted here: the
// reclamation view propagates, the resolution view still degrades.
func TestIsolatedPanesTellsEmptyFromUnreadable(t *testing.T) {
	sl, b := isolatedFixture(t)
	ctx := context.Background()

	// Control: a real pane, read normally. Without it, an error below could come
	// from a namespace that was never readable in the first place.
	tgt, err := sl.resolveHelper(ctx, 1, kindIsolated)
	if err != nil {
		t.Fatalf("resolve isolated slot 1: %v", err)
	}
	panes, err := b.IsolatedPanes(ctx)
	if err != nil {
		t.Fatalf("read this namespace's panes: %v", err)
	}
	if !slices.Contains(panes, tgt.Ref) {
		t.Fatalf("the pane just opened is not in its own namespace (%v); nothing below this line "+
			"is about the error path", panes)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if got, err := b.IsolatedPanes(cancelled); err == nil {
		t.Errorf("a read that could not run answered %d panes and no error. The sweep above it "+
			"then reports a complete teardown for a namespace it never saw, and the live shells "+
			"are unreachable by any tool until this process exits", len(got))
	}

	// The other half, and it is a decision rather than an oversight: resolution
	// keeps the forgiving read. If this ever starts failing, a transient hiccup
	// on the isolated socket has begun failing ordinary visible calls.
	records, err := b.IsolatedRecords(cancelled)
	if err != nil {
		t.Errorf("the resolution view propagated a failed namespace read (%v); a plain send-keys "+
			"now fails whenever that socket hiccups", err)
	}
	if len(records) != 0 {
		t.Errorf("the degraded read answered %d records, want none", len(records))
	}
}

// failingIsolatedPanes is a real Backend with one method broken, which is the
// whole of the fake: everything else is delegated, so the test drives the same
// tmux the product does and differs from it in exactly one place.
type failingIsolatedPanes struct {
	Backend
	err error
}

func (f failingIsolatedPanes) IsolatedPanes(context.Context) ([]paneRef, error) {
	return nil, f.err
}

// TestSweepReportsTheNamespaceItCouldNotRead is the response half of the same
// defect.
//
// close-pane({slot:"all"}) is the one call whose entire contract is "leave
// nothing invisible behind". When the namespace read fails it has exactly two
// honest answers — what it closed, and what it could not see — and it used to
// give neither: it returned (nil, err), which threw away the visible panes it
// had ALREADY closed, and before that it did not even get an error to return.
func TestSweepReportsTheNamespaceItCouldNotRead(t *testing.T) {
	sl, b := isolatedFixture(t)
	ctx := context.Background()

	if _, err := sl.resolveHelper(ctx, 2, kindIsolated); err != nil {
		t.Fatalf("resolve isolated slot 2: %v", err)
	}

	broken := newSlots(failingIsolatedPanes{
		Backend: b,
		err:     errors.New("tmux list-panes: connection refused"),
	})
	entries, err := broken.closePanes(ctx, closeSelector{All: true})
	if err != nil {
		t.Fatalf("close-pane({slot:\"all\"}) failed the whole call with %v instead of reporting; "+
			"any visible panes it had already closed would be invisible to the caller", err)
	}
	if len(entries) == 0 {
		t.Fatal("close-pane({slot:\"all\"}) answered an empty array and no error for a namespace " +
			"it could not read: the agent is told its invisible panes are gone while they are " +
			"still running")
	}
	reported := false
	for _, entry := range entries {
		if entry.Action == actionError && strings.Contains(entry.Detail, "still open") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the sweep answered %v, with no action:%q entry saying the namespace could not "+
			"be read", entries, actionError)
	}

	// Control (a): the report is TRUE — the pane really is still open.
	panes, err := b.IsolatedPanes(ctx)
	if err != nil {
		t.Fatalf("read this namespace's panes: %v", err)
	}
	if len(panes) == 0 {
		t.Fatal("the pane was closed after all, so the error entry is a false alarm rather than " +
			"the honest answer this test is asserting")
	}

	// Control (b): the same call on the same state, with nothing broken, closes
	// it and says so. Without this, "an error entry" could be what this path
	// always answers.
	entries, err = sl.closePanes(ctx, closeSelector{All: true})
	if err != nil {
		t.Fatalf("close-pane({slot:\"all\"}) with a working backend: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != actionKilled {
		t.Fatalf("the working sweep answered %v, want one action:%q entry", entries, actionKilled)
	}
	if panes, err := b.IsolatedPanes(ctx); err != nil || len(panes) != 0 {
		t.Errorf("the namespace still holds %v (err %v) after a sweep that reported success",
			panes, err)
	}
}
