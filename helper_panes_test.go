package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- Fixture ----

// slotFixture builds the situation every slot test needs: a real tmux window
// that the client believes it is running inside.
//
// The session is created with raw tmux rather than through the client because
// the server's own pane has to exist *before* anything resolves against it —
// that is the production shape, where tmux starts the pane and the agent starts
// the server inside it. TMUX_PANE is injected with t.Setenv rather than
// os.Setenv so it is restored when the test ends: isolateTmux clears the
// variable for the whole suite precisely so a developer running the tests from
// inside their own tmux does not have a client latch onto their real pane, and a
// leaked value would hand the next test that pane.
//
// The window is deliberately large. At tmux's default 80x24 a chain of 50%
// splits runs out of rows after three panes, and a placement test would then be
// measuring tmux's minimum-size clamping rather than the placement rules.
func slotFixture(t *testing.T) (*tmuxClient, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux")
	}
	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })

	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
	t.Setenv("TMUX_PANE", self)
	return newTmuxClient("bash"), self
}

// paneOrigin returns a pane's top-left cell, used to prove which pane a split
// was carved out of.
func paneOrigin(t *testing.T, paneID string) (left, top int) {
	t.Helper()
	out := tmuxExec(t, "display-message", "-p", "-t", paneID, "#{pane_left}\t#{pane_top}")
	parts := strings.Split(out, "\t")
	if len(parts) != 2 {
		t.Fatalf("unexpected geometry for pane %s: %q", paneID, out)
	}
	left, _ = strconv.Atoi(parts[0])
	top, _ = strconv.Atoi(parts[1])
	return left, top
}

// ---- Resolution ----

// TestResolveSlotIsIdempotent is the property that makes slots worth having: the
// agent asks for "slot 1" over and over across a session and keeps landing in
// the same pane, so a dev server started by one call is still there for the
// next. A resolver that created a pane per call would be a slower split-pane
// with a friendlier name.
func TestResolveSlotIsIdempotent(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()

	first, slot, created, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("first resolveHelper: %v", err)
	}
	if first == self {
		t.Fatalf("resolveHelper returned the server's own pane %s", first)
	}
	if slot != slotDefault {
		t.Errorf("first resolveHelper reported slot %d, want %d", slot, slotDefault)
	}
	if !created {
		t.Error("first resolveHelper should have reported created=true — the window had no helper")
	}

	second, slot, created, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("second resolveHelper: %v", err)
	}
	if second != first {
		t.Errorf("second resolveHelper returned %s, want the same pane %s", second, first)
	}
	if slot != slotDefault {
		t.Errorf("second resolveHelper reported slot %d, want %d", slot, slotDefault)
	}
	if created {
		t.Error("second resolveHelper reported created=true for a pane it reused")
	}

	if title := tmuxExec(t, "display-message", "-p", "-t", first, "#{pane_title}"); title != "agent" {
		t.Errorf("helper pane title is %q, want \"agent\" — the label is how the user tells our panes apart", title)
	}
}

// TestResolveSlotAfterKillCreatesAgain covers what created is actually for. The
// user closes the helper pane, taking the agent's process with it; the next
// resolution has to hand back a fresh pane AND say so, because "created" is the
// only signal the agent gets that the dev server it is still reporting on died
// some time ago.
func TestResolveSlotAfterKillCreatesAgain(t *testing.T) {
	client, _ := slotFixture(t)
	ctx := context.Background()

	first, _, _, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("first resolveHelper: %v", err)
	}
	if err := client.KillPane(ctx, first); err != nil {
		t.Fatalf("kill helper pane: %v", err)
	}

	second, _, created, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("second resolveHelper: %v", err)
	}
	if second == first {
		t.Fatalf("resolveHelper returned the killed pane %s", second)
	}
	if !created {
		t.Error("resolveHelper must report created=true after the slot's pane was killed")
	}
}

// TestSlotPlacement pins the placement table for the two slots an agent actually
// uses. The geometry assertion is the interesting half: slot 2 must be carved
// out of the slot-1 pane, not out of the agent's own pane, so the helpers stack
// in one column instead of squeezing the pane the user is reading the
// conversation in. Sharing slot 1's left edge while starting lower down is what
// "split out of slot 1, vertically" looks like from the outside.
func TestSlotPlacement(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()

	one, _, _, err := client.resolveHelper(ctx, 1)
	if err != nil {
		t.Fatalf("resolve slot 1: %v", err)
	}
	two, slot, _, err := client.resolveHelper(ctx, 2)
	if err != nil {
		t.Fatalf("resolve slot 2: %v", err)
	}
	if slot != 2 {
		t.Errorf("slot 2 resolution reported slot %d", slot)
	}
	if two == one {
		t.Fatalf("slots 1 and 2 resolved to the same pane %s", two)
	}
	if two == self {
		t.Fatalf("slot 2 resolved to the server's own pane %s", two)
	}

	selfLeft, _ := paneOrigin(t, self)
	oneLeft, oneTop := paneOrigin(t, one)
	twoLeft, twoTop := paneOrigin(t, two)

	if oneLeft == selfLeft {
		t.Errorf("slot 1 starts at column %d, same as the agent's pane — it should sit beside it", oneLeft)
	}
	if twoLeft != oneLeft {
		t.Errorf("slot 2 starts at column %d, want %d (slot 1's column) — it must be split out of slot 1", twoLeft, oneLeft)
	}
	if twoTop <= oneTop {
		t.Errorf("slot 2 starts at row %d, want below slot 1's row %d", twoTop, oneTop)
	}

	if title := tmuxExec(t, "display-message", "-p", "-t", two, "#{pane_title}"); title != "agent:2" {
		t.Errorf("slot 2 pane title is %q, want \"agent:2\"", title)
	}
}

// TestNumberedSlotsAreIndependent is what remains of the isolation guarantee now
// that slot:"new" is gone. A caller that wants a pane nobody else will type into
// names a number nobody else is using — so the property that has to hold is that
// each number resolves to its own pane and carries that number back, rather than
// collapsing onto the default one.
//
// The registry round-trip at the end is the load-bearing part. A pane that came
// back for slot 3 but does not carry @mcp_slot=3 would be re-created on the very
// next call, and the caller's process would vanish with no error anywhere.
func TestNumberedSlotsAreIndependent(t *testing.T) {
	client, _ := slotFixture(t)
	ctx := context.Background()

	seen := map[string]int{}
	for _, want := range []int{1, 2, 3} {
		pane, slot, created, err := client.resolveHelper(ctx, want)
		if err != nil {
			t.Fatalf("resolve slot %d: %v", want, err)
		}
		if slot != want {
			t.Errorf("resolve slot %d reported slot %d", want, slot)
		}
		if !created {
			t.Errorf("slot %d already existed in a fresh fixture", want)
		}
		if prev, dup := seen[pane]; dup {
			t.Fatalf("slot %d returned pane %s, already handed out for slot %d", want, pane, prev)
		}
		seen[pane] = want

		if rec, found, err := client.paneRecordFor(ctx, pane); err != nil || !found || rec.Slot != want {
			t.Errorf("pane %s does not carry slot %d back (found=%v rec=%+v err=%v)",
				pane, want, found, rec, err)
		}
	}
}

// TestResolveHelperNeverReturnsSelf is Invariant R at its sharpest point.
//
// A server whose own pane was created by an outer agent's split-pane call
// inherits a pane carrying a valid witness and owner "agent" — and, if that
// outer agent used a slot, a slot marker too. Nested agents are ordinary: it is
// what a subagent launched into a split looks like. Without the self-exclusion
// clause the inner server finds itself in its own registry, calls that "the
// helper pane", and every send-keys after that types into the agent's own
// session. Nothing about that failure is visible from outside the process.
func TestResolveHelperNeverReturnsSelf(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()

	// The inherited claim: exactly what an outer agent's slot-1 pane looks like.
	if err := client.markPaneOwnedAs(ctx, "", self, ownerAgent, slotDefault); err != nil {
		t.Fatalf("mark self as a slot-1 pane: %v", err)
	}
	if rec, found, err := client.paneRecordFor(ctx, self); err != nil || !found || rec.Slot != slotDefault {
		t.Fatalf("fixture is not exercising the case: self carries no slot-1 record (found=%v rec=%+v err=%v)", found, rec, err)
	}

	pane, _, created, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("resolveHelper: %v", err)
	}
	if pane == self {
		t.Fatalf("resolveHelper returned the server's own pane %s — the agent would type into its own session", pane)
	}
	if !created {
		t.Error("with self excluded there was no candidate left, so the pane must be reported as created")
	}

	// The stale marker is cleared rather than merely skipped, so the same pane is
	// not re-examined on every later call.
	rec, found, err := client.paneRecordFor(ctx, self)
	if err != nil {
		t.Fatalf("read self's record back: %v", err)
	}
	if found && rec.Slot != 0 {
		t.Errorf("self still carries slot %d; the stale marker must be cleared", rec.Slot)
	}
}

// TestSlotWitnessSurvivesSessionScopedLeak is the sibling of
// TestOwnershipSurvivesSessionScopedOptionLeak, for the option that is more
// dangerous.
//
// tmux user options inherit down the scope chain when interpolated in a
// pane-context format string, so a set-option that forgets -p lands at session
// scope and resolves for EVERY pane in the user's session. For @mcp_owner that
// used to mean every pane looked reusable. For @mcp_slot it is worse: every pane
// in the session would answer to slot 1, so the very next resolution would hand
// the agent one of the user's shells and call it the helper pane — and, unlike
// reuse, resolution's whole purpose is to be typed into.
//
// The witness is what makes that impossible: a record counts only when
// @mcp_pane equals the pane's own ID, and one option value can equal only one
// pane's ID.
func TestSlotWitnessSurvivesSessionScopedLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	client := newTmuxClient("bash")
	ctx := context.Background()

	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	defer exec.Command("tmux", "kill-session", "-t", name).Run() //nolint:errcheck

	windowID := tmuxExec(t, "display-message", "-p", "-t", name, "#{window_id}")
	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
	tmuxExec(t, "split-window", "-d", "-t", windowID)
	tmuxExec(t, "split-window", "-d", "-t", windowID)
	t.Setenv("TMUX_PANE", self)

	// The slip: session scope, because -p was omitted. Both options leak, and
	// still no witness exists anywhere.
	tmuxExec(t, "set-option", "-t", name, paneOptOwner, ownerAgent)
	tmuxExec(t, "set-option", "-t", name, paneOptSlot, "1")

	leaked := tmuxExec(t, "list-panes", "-t", windowID, "-F", "#{"+paneOptSlot+"}")
	if got := strings.Count(leaked, "1"); got != 3 {
		t.Fatalf("fixture is not exercising the leak: %d of 3 panes see the session-scoped slot", got)
	}

	reg, err := client.paneRegistryInWindow(ctx, windowID)
	if err != nil {
		t.Fatalf("paneRegistryInWindow: %v", err)
	}
	if len(reg) != 0 {
		t.Fatalf("a session-scoped @mcp_slot produced %d registry records; the witness must reject all of them", len(reg))
	}

	rec, found, err := client.resolveHelperNoCreate(ctx, slotDefault)
	if err != nil {
		t.Fatalf("resolveHelperNoCreate: %v", err)
	}
	if found {
		t.Fatalf("slot 1 resolved to pane %s off a leaked option — that is one of the user's shells", rec.PaneID)
	}
}

// ---- Acquisition ----

// waitForClientPaneIdle polls the client directly, for the tests that have no
// MCP server to ask.
func waitForClientPaneIdle(t *testing.T, client *tmuxClient, paneID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state, err := client.GetPaneState(context.Background(), paneID); err == nil && paneIsIdleShell(state) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane %s did not settle at an idle shell prompt within 5s", paneID)
}

// TestAcquireIdleUnownedPane covers the half of this design that touches panes
// the server did not create.
//
// The user already has an empty shell open beside the agent. Creating a second
// one next to it is the behaviour that makes an agent's terminal fill with
// identical idle splits, so an idle, unclaimed, same-user shell is adopted
// instead — and marked "acquired" rather than "agent", because the difference
// decides whether close-pane may kill it later.
func TestAcquireIdleUnownedPane(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()

	// The user's own pane: made with raw tmux, so the server never marked it.
	usersPane := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	waitForClientPaneIdle(t, client, usersPane)

	pane, _, created, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("resolveHelper: %v", err)
	}
	if pane != usersPane {
		t.Fatalf("resolveHelper returned %s instead of adopting the idle pane %s", pane, usersPane)
	}
	if !created {
		t.Error("an adopted pane is new to the slot, so created must be true")
	}

	rec, found, err := client.paneRecordFor(ctx, usersPane)
	if err != nil || !found {
		t.Fatalf("adopted pane carries no registry record (found=%v err=%v)", found, err)
	}
	if rec.Owner != ownerAcquired {
		t.Errorf("adopted pane is marked %q, want %q — the owner kind is what stops close-pane "+
			"killing a pane the user opened", rec.Owner, ownerAcquired)
	}
	if rec.Slot != slotDefault {
		t.Errorf("adopted pane carries slot %d, want %d", rec.Slot, slotDefault)
	}

	// And it must not be adopted twice into different slots.
	if _, ok := client.ownedPanesInWindow(ctx, tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")); ok != nil {
		t.Fatalf("ownedPanesInWindow: %v", ok)
	}
}

// TestAcquireRejectsPaneRunningCat is the idleness half of the predicate, tested
// through the signal that actually works on both platforms.
//
// A pane running `cat` is alive and looks calm, and on macOS it even reports
// waitingForInput=true — the field this predicate deliberately does not use.
// What distinguishes it is that the foreground process is no longer the shell,
// which is what paneIsIdleShell tests. Adopting it would send the agent's
// command into cat's stdin, where it would be echoed and never run.
func TestAcquireRejectsPaneRunningCat(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()

	busy := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	waitForClientPaneIdle(t, client, busy)
	tmuxExec(t, "send-keys", "-t", busy, "cat", "Enter")

	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := client.GetPaneState(ctx, busy)
		if err == nil && !paneIsIdleShell(state) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never picked up the cat process", busy)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if client.canAcquire(ctx, busy, self) {
		t.Error("canAcquire accepted a pane running cat; the shell is not the foreground process")
	}

	pane, _, _, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("resolveHelper: %v", err)
	}
	if pane == busy {
		t.Fatalf("resolveHelper adopted the busy pane %s", pane)
	}
	if rec, found, _ := client.paneRecordFor(ctx, busy); found {
		t.Errorf("the busy pane was marked anyway: %+v", rec)
	}
}

// TestAcquireRejectsForeignUID is the uid guard at the level it actually lives.
//
// The case it defends against — a pane whose own process is another user's shell
// (`exec sudo -i`, an exec'd su, a namespace with a different uid) — cannot be
// built in a test without sudo. pid 1 is the same shape of fact: a live process
// owned by root, present on both platforms, needing no privileges to point at.
// If the guard would adopt pid 1 it would adopt a root shell.
func TestAcquireRejectsForeignUID(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, so pid 1 is the same user and proves nothing")
	}
	if sameUIDAsAgent(&PaneState{PanePID: 1, ForegroundPID: 1, IsAlive: true}) {
		t.Error("sameUIDAsAgent accepted pid 1, which is root — the guard is not comparing uids")
	}
	if sameUIDAsAgent(nil) {
		t.Error("sameUIDAsAgent accepted a nil state; every failure to probe must be a no")
	}
	// The positive half, so the test cannot pass by the function always saying no.
	if !sameUIDAsAgent(&PaneState{PanePID: os.Getpid(), ForegroundPID: os.Getpid(), IsAlive: true}) {
		t.Error("sameUIDAsAgent rejected this very process, which is by definition the same user")
	}
}

// TestDuplicateHealingReleasesAcquiredLoser covers what happens to the pane that
// loses a duplicate-slot race, which is a state no teardown path could reach.
//
// Two server processes in one window can each mark a pane as slot 1, because
// tmux has no compare-and-set on options; the oldest wins and the others are
// healed. When a loser is one of the USER's panes, adopted by the other process,
// unslotting it is not enough: close-pane({slot:"all"}) covers slotted records
// only, so it becomes invisible to teardown, while canAcquire refuses any pane
// carrying an owner mark, so it can never be adopted again either — an ordinary
// shell, still titled "agent", that the agent mysteriously refuses to touch. The
// loser must be handed back properly instead: every marker gone, the label gone,
// the pane itself untouched.
func TestDuplicateHealingReleasesAcquiredLoser(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()

	// Two of the user's own panes, both claimed as slot 1 — the state the race
	// leaves behind, written directly because the second server is not part of
	// this test.
	first := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	second := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	waitForClientPaneIdle(t, client, first)
	waitForClientPaneIdle(t, client, second)
	for _, pane := range []string{first, second} {
		if err := client.markPaneOwnedAs(ctx, "", pane, ownerAcquired, slotDefault); err != nil {
			t.Fatalf("mark %s as an acquired slot-1 pane: %v", pane, err)
		}
		if err := client.setPaneTitle(ctx, pane, helperTitle(slotDefault)); err != nil {
			t.Fatalf("title %s: %v", pane, err)
		}
	}

	winner, _, _, err := client.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("resolveHelper: %v", err)
	}
	if winner != first {
		t.Fatalf("resolveHelper kept %s, want the oldest pane %s", winner, first)
	}

	// The loser is the user's pane again: alive, unmarked, unlabelled.
	if !paneExists(t, second) {
		t.Fatal("healing destroyed the losing pane; an adopted pane may only be released")
	}
	for _, opt := range []string{paneOptSlot, paneOptOwner, paneOptWitness} {
		if got := tmuxExec(t, "display-message", "-t", second, "-p", "#{"+opt+"}"); got != "" {
			t.Errorf("%s is still %q on the released loser %s; leaving the owner mark retires the "+
				"pane forever, because acquisition requires it to be unset", opt, got, second)
		}
	}
	if title := tmuxExec(t, "display-message", "-t", second, "-p", "#{pane_title}"); title == "agent" {
		t.Errorf("the released loser is still titled %q; to the user that is a pane the agent "+
			"claims and will not use", title)
	}

	// And it can be adopted again, which is the property the owner mark would
	// otherwise have destroyed.
	if !client.canAcquire(ctx, second, self) {
		t.Error("the released loser cannot be re-adopted; releasing it has to leave it exactly as " +
			"unclaimed as it was before")
	}
}

// ---- Invariant S ----

// TestOnlyPolicyCodeKnowsOurOwnPane enforces the rule that keeps the bare
// send-keys({keys}) default safe, by parsing the package rather than by trusting
// review.
//
// The rule: knowing which pane this server runs in is permitted only where that
// pane is used as a split ANCHOR, or as something to EXCLUDE. It is never
// permitted where keystrokes are delivered. selfPane is an input to placement —
// the pane we split — and never an output of resolution — the pane we type into
// — and a pane produced by splitting is, by construction, not the pane that was
// split.
//
// Only a permitted SET is asserted, and nothing is required to be in it. The
// design (§5.3 step 1) reads `self ← selfPane()` at the top of resolveHelper,
// while the implementation reaches it one level down through selfWindow so that
// resolution and teardown share one answer per call; both are the same rule, and
// a rewrite that moved the call between them was previously RED for no reason
// anyone could defend. So every function named here is a place the spec already
// allows to ask the question, and which of them happens to ask it this month is
// an implementation detail, not a safety property. registerSplitPane stays on
// the list for the same reason it was ever on it: §4 sanctions split-pane's
// no-argument default precisely because splitting delivers no keystrokes, so
// that handler may know the answer even though no other handler may.
//
// What may never appear here is a handler that types. A future send-keys that
// reaches for selfPane, or for os.Getenv("TMUX_PANE") directly to dodge this
// test, fails the build's test step — which is the only way a rule like this
// survives nine handlers and a year of edits, because the failure it prevents is
// invisible from outside the process: the agent's own transcript fills with its
// own keystrokes, and there is nothing left to read the diagnosis from.
//
// The one thing still required is that SOMETHING asks. A package where nothing
// resolves its own pane cannot be excluding it either, and this test would then
// be passing over a resolver that no longer knows what to leave out.
func TestOnlyPolicyCodeKnowsOurOwnPane(t *testing.T) {
	permitted := map[string]string{
		"selfWindow":                       "the single accessor resolution and teardown share, to exclude the pane",
		"resolveHelper":                    "§5.3 step 1, where resolution starts from the pane it must not return",
		"resolveHelperLocked":              "the body of the same step",
		"resolveHelperNoCreate":            "the same, for the lookup close-pane performs",
		"placementForSlot":                 "placement, where the pane is a split anchor",
		"closePanes":                       "teardown, which must not close the pane the request arrived through",
		"slottedHelpersInSelfWindowLocked": "the \"all\" sweep, which excludes this server's own pane",
		"registerSplitPane":                "split-pane's default (§4): a split delivers no keystrokes",
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package files: %v", err)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	sawDeclaration := false

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == "selfPane" {
				sawDeclaration = true
				continue // the accessor itself is where os.Getenv belongs
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "selfPane" {
					found[fn.Name.Name] = true
					return true
				}
				// os.Getenv("TMUX_PANE") is the same knowledge by another route,
				// and reading it directly would bypass both this test and the
				// single accessor it protects.
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "os" && sel.Sel.Name == "Getenv" &&
					len(call.Args) == 1 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"TMUX_PANE"` {
						found[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}

	if !sawDeclaration {
		t.Fatal("selfPane is not declared in this package — the test is looking for the wrong symbol")
	}
	for name := range found {
		if _, ok := permitted[name]; !ok {
			t.Errorf("%s asks which pane this server runs in, and is not permitted to: "+
				"selfPane may be used only as a split anchor or as something to exclude, never as a "+
				"pane to deliver keystrokes to (permitted: %v)", name, permittedNames(permitted))
		}
	}
	if len(found) == 0 {
		t.Errorf("nothing in this package asks which pane this server runs in, so nothing can be "+
			"excluding it either — resolution has stopped knowing what not to hand back, and this "+
			"test is guarding an empty set (permitted callers: %v)", permittedNames(permitted))
	}
}

// ---- Lock discipline ----

// withinDeadline runs work in its own goroutine and fails the test if it has not
// finished in time, rather than letting it hang.
//
// Everything below it exists to catch a deadlock, and a deadlock is not a failing
// test — it is a test that never returns. Left unbounded it holds the package's
// whole run until `go test` fires its own watchdog many minutes later, and what
// arrives then is a panic with a goroutine dump for every test in the binary,
// timed out as a suite rather than reported as one broken invariant. The budget
// is clipped to whatever the run's own -timeout leaves so the failure is always
// ours, with a message that says which phase stopped.
//
// work must not call t.Fatal: it runs off the test goroutine, where Fatal stops
// only that goroutine and leaves the test to pass. Everything below therefore
// collects its results over channels and asserts them here, after the wait.
func withinDeadline(t *testing.T, phase string, budget time.Duration, work func()) {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		if left := time.Until(deadline) - 5*time.Second; left < budget {
			budget = left
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		work()
	}()
	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("%s did not finish within %s — slot resolution is deadlocked. slotMu is a "+
			"sync.Mutex and is not reentrant, so a second Lock() anywhere under a lock hold "+
			"parks the caller forever: in production that is a tool call that never returns, "+
			"an mcp-go worker gone from the pool of five, and no error anywhere", phase, budget)
	}
}

// TestConcurrentSlotResolutionNeitherDuplicatesNorHangs is the lock, tested
// through what a user would see rather than through the shape of the code.
//
// Two failures share one cause and neither announces itself. mcp-go's stdio
// server dispatches tool calls onto a worker pool (5 by default,
// server/stdio.go), so "resolve slot 1" can genuinely run twice at once: without
// the mutex both calls find the slot empty and both create a pane, and the user
// watches two identical empty splits appear beside the agent. With a mutex taken
// twice on one path — a ...Locked helper that reaches for the lock-taking
// resolveHelper, an extracted lock holder that forgets it is already inside one —
// the call parks forever instead, which reads to the agent as a request that
// never came back.
//
// The teardown goroutine is here because teardown is the path where the second
// lock is easiest to reintroduce: closePanes takes slotMu once and then calls a
// chain of ...Locked helpers whose names are the only thing marking them, and
// both halves look correct alone — a resolver that locks is right, and a teardown
// that locks is right.
//
// This deliberately asserts the RULE and not the mechanism. Its predecessor
// parsed the package for `slotMu.Lock` and followed calls by function name, which
// could see neither `fn := t.resolveHelper; fn(ctx, slot)` — the exact shape of
// the deadlock it existed to prevent — nor any correct refactor: a withSlotLock
// helper, singleflight, a serialised worker pool would each have failed it while
// changing nothing a caller could observe. Run this under -race.
func TestConcurrentSlotResolutionNeitherDuplicatesNorHangs(t *testing.T) {
	client, self := slotFixture(t)
	ctx := context.Background()
	window := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	type resolution struct {
		asked, got int
		pane       string
		created    bool
		err        error
	}

	// Phase 1: the same slot from several callers at once, and three slots at
	// once, with no teardown in the way — so "one pane per slot" is a fact about
	// the resolver rather than a race with something destroying panes.
	const callersPerSlot = 4
	slots := []int{1, 2, 3}

	first := make(chan resolution, len(slots)*callersPerSlot)
	var wg sync.WaitGroup
	release := make(chan struct{})
	for _, slot := range slots {
		for i := 0; i < callersPerSlot; i++ {
			wg.Add(1)
			go func(slot int) {
				defer wg.Done()
				<-release // so they arrive together rather than in start order
				pane, got, created, err := client.resolveHelper(ctx, slot)
				first <- resolution{asked: slot, got: got, pane: pane, created: created, err: err}
			}(slot)
		}
	}
	withinDeadline(t, "concurrent resolution", 90*time.Second, func() {
		close(release)
		wg.Wait()
	})
	close(first)

	panesFor := map[int]map[string]bool{}
	createdFor := map[int]int{}
	for r := range first {
		if r.err != nil {
			// Reported rather than fatal, so the accounting below still runs: an
			// unsynchronised resolver fails several ways at once — duplicate panes
			// AND, once the window is full of them, splits that have nowhere to go —
			// and seeing both is what identifies the cause.
			t.Errorf("concurrent resolveHelper(%d): %v", r.asked, r.err)
			continue
		}
		if r.got != r.asked {
			t.Errorf("a caller asking for slot %d was answered for slot %d", r.asked, r.got)
		}
		if r.pane == self {
			t.Fatalf("slot %d resolved to the server's own pane %s under concurrency", r.asked, r.pane)
		}
		if panesFor[r.asked] == nil {
			panesFor[r.asked] = map[string]bool{}
		}
		panesFor[r.asked][r.pane] = true
		if r.created {
			createdFor[r.asked]++
		}
	}

	seen := map[string]int{}
	for _, slot := range slots {
		if n := len(panesFor[slot]); n != 1 {
			t.Errorf("%d callers of slot %d were handed %d different panes (%v); a slot is the "+
				"promise that a process started there is still there next time, and %d concurrent "+
				"callers must land in ONE pane", callersPerSlot, slot, n, panesFor[slot], callersPerSlot)
		}
		if createdFor[slot] != 1 {
			t.Errorf("slot %d reported created=true %d times; exactly one caller may create the "+
				"pane, and every other must be told it is reusing one — created is the signal that "+
				"the process left there is gone", slot, createdFor[slot])
		}
		for pane := range panesFor[slot] {
			if other, dup := seen[pane]; dup {
				t.Errorf("slot %d and slot %d both resolved to pane %s", other, slot, pane)
			}
			seen[pane] = slot
		}
	}

	// The window itself is the user-visible half: one pane per slot plus the
	// agent's own, and nothing extra. A resolver that created a pane it then threw
	// away would satisfy every assertion above and still leave the user tidying up.
	if got := len(strings.Split(tmuxExec(t, "list-panes", "-t", window, "-F", "#{pane_id}"), "\n")); got != len(slots)+1 {
		t.Errorf("the window holds %d panes after %d concurrent resolutions of %d slots, want %d "+
			"(the agent's own pane plus one per slot) — the extras are duplicate splits nobody asked for",
			got, len(slots)*callersPerSlot, len(slots), len(slots)+1)
	}

	// Phase 2: teardown concurrent with resolution, which is the ordering the lock
	// was moved into closePanes for. Nothing here can assert which pane wins — a
	// resolver that runs after the sweep correctly creates a new one — so what is
	// asserted is that every call RETURNS, that none of them is handed the agent's
	// own pane, and that the registry is left coherent rather than with two live
	// panes answering to one slot.
	closeErr := make(chan error, 1)
	second := make(chan resolution, 8)
	var wg2 sync.WaitGroup
	release2 := make(chan struct{})
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		<-release2
		_, err := client.closePanes(ctx, closeSelector{All: true})
		closeErr <- err
	}()
	for _, slot := range []int{1, 2} {
		for i := 0; i < 4; i++ {
			wg2.Add(1)
			go func(slot int) {
				defer wg2.Done()
				<-release2
				pane, got, created, err := client.resolveHelper(ctx, slot)
				second <- resolution{asked: slot, got: got, pane: pane, created: created, err: err}
			}(slot)
		}
	}
	withinDeadline(t, "teardown racing resolution", 90*time.Second, func() {
		close(release2)
		wg2.Wait()
	})
	close(second)
	close(closeErr)

	if err := <-closeErr; err != nil {
		t.Errorf("close-pane({slot:\"all\"}) concurrent with resolution: %v", err)
	}
	for r := range second {
		if r.err != nil {
			t.Errorf("resolveHelper(%d) concurrent with teardown: %v", r.asked, r.err)
		}
		if r.pane == self {
			t.Fatalf("slot %d resolved to the server's own pane %s while teardown ran", r.asked, r.pane)
		}
	}

	reg, err := client.paneRegistryInWindow(ctx, window)
	if err != nil {
		t.Fatalf("read the registry back: %v", err)
	}
	holders := map[int][]string{}
	for _, rec := range reg {
		if rec.Slot == 0 || rec.Dead {
			continue
		}
		holders[rec.Slot] = append(holders[rec.Slot], rec.PaneID)
		if rec.PaneID == self {
			t.Errorf("the agent's own pane %s came out of the storm carrying slot %d", self, rec.Slot)
		}
	}
	for slot, panes := range holders {
		if len(panes) > 1 {
			t.Errorf("slot %d is claimed by %v after teardown raced resolution; the next call would "+
				"heal one of them away, which means one caller's process is about to become "+
				"unreachable", slot, panes)
		}
	}
}

func permittedNames(permitted map[string]string) []string {
	names := make([]string, 0, len(permitted))
	for name := range permitted {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
