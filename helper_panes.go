package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file holds helper-pane *policy*: which pane the agent gets, where it is
// placed, and under what conditions an existing pane may be adopted. tmux.go is
// a thin wrapper over the tmux CLI — everything in it is a command with
// arguments — and mixing policy into it would spread the safety-critical
// decisions across a 900-line client. The low-level option reads and writes stay
// there, next to the witness constant that explains them; the rules about what
// to do with those records live here, findable as a unit.

// selfPane returns the pane this server process runs in, or "" when the server
// was not started inside tmux.
//
// The value is inherited from the environment at spawn and is stable for the
// process lifetime, which is the entire point: it cannot race with the user
// switching windows, panes or sessions, unlike any query of tmux's "active"
// pane, which answers a question about the user's cursor rather than about this
// process. A resolution that consulted the active pane would place the agent's
// helper next to wherever the user happened to be looking at that instant.
//
// It is also not a guess: every agent runtime that starts this server starts it
// from inside a pane, and tmux sets TMUX_PANE in that pane's environment itself.
// An empty answer therefore means something specific and reportable — "this
// process is not in tmux" — rather than "lookup failed", which is why the caller
// can turn it into errNotInTmux instead of a fallback.
//
// Reading it through a method rather than at each call site is deliberate: the
// set of callers permitted to ask "which pane am I?" is a safety property (a
// pane the agent may split, never a pane the agent may type into), and a single
// named accessor is what makes that set auditable.
func (t *tmuxClient) selfPane() string { return os.Getenv("TMUX_PANE") }

// errNotInTmux is returned whenever a call needs the server's own pane and there
// is none.
//
// The message names the two alternatives on purpose. The incident this design
// comes from began with an agent that could not tell "I am not in tmux" from "I
// passed the wrong argument", and responded by shelling out to raw tmux to find
// out — which is the behaviour every tool in this server exists to make
// unnecessary. An error that says only "no pane" invites exactly that guessing;
// one that names the two ways forward ends the exchange.
var errNotInTmux = errors.New(
	"this server is not running inside tmux ($TMUX_PANE is unset), so it has no window to " +
		"place a pane in — pass an explicit paneId, or use headless:true / create-headless")

// selfWindow returns the server's own pane and the window that contains it.
//
// Every slot operation is scoped to this one window, and that is a deliberate
// limit rather than an implementation shortcut: the agent's helper panes belong
// beside the agent, where the user can see them, and a resolution that could
// reach into the user's other windows would be able to hand back a pane the user
// is not looking at — which is indistinguishable, from the agent's side, from a
// pane it created. Teardown follows the same scope for the same reason.
//
// This is also the single accessor through which resolution and teardown ask
// "which pane am I?". Keeping the question in one place is what makes Invariant
// S — the set of code permitted to know the server's own pane — a property a
// test can check (TestOnlyPolicyCodeKnowsOurOwnPane) rather than a rule every
// future handler has to remember.
func (t *tmuxClient) selfWindow(ctx context.Context) (self, windowID string, err error) {
	self = t.selfPane()
	if self == "" {
		return "", "", errNotInTmux
	}
	windowID, err = t.getWindowIDForPane(ctx, self)
	if err != nil {
		return "", "", fmt.Errorf("locate the window holding this server's own pane %s: %w", self, err)
	}
	return self, windowID, nil
}

// Slot numbering.
//
// slotDefault is what a caller that names neither a pane nor a slot gets, so
// that the overwhelmingly common case — "run this somewhere I can see" — needs
// no argument at all.
//
// maxSlot bounds the numbers a caller may ask for. Its job is to turn a typo
// into an error: without it, slot:99999 would silently create a 99999th helper
// pane, and there is no plausible workflow in which that is what the caller
// meant. A caller that wants several panes asks for 1, 2, 3 — there is no
// "allocate me a free one" form, because a caller that needs more than one pane
// already knows how many, and numbering them itself is what lets it address the
// same pane again on the next call.
const (
	slotDefault = 1
	maxSlot     = 64
)

// helperTitle is the pane title we hang on a helper so the user can see, in the
// pane border or in list-panes, which panes belong to the agent. Slot 1 is the
// unnumbered "agent" because it is the one almost every session has.
func helperTitle(slot int) string {
	if slot <= slotDefault {
		return "agent"
	}
	return fmt.Sprintf("agent:%d", slot)
}

// paneIDNumber extracts the numeric part of a tmux pane id ("%12" → 12).
//
// Sorting pane ids as strings is wrong in a way that only appears after a
// session has been running a while: "%10" sorts before "%9" lexically, so the
// "keep the lowest id" rules below — which mean "keep the oldest pane", the one
// most likely to have the caller's process in it — would start preferring the
// newest pane once the window had passed ten panes. An unparseable id sorts
// last, so a pane we cannot rank can never win a tie-break by accident.
func paneIDNumber(paneID string) int {
	_, bare := parseTarget(paneID)
	n, err := strconv.Atoi(strings.TrimPrefix(bare, "%"))
	if err != nil {
		return math.MaxInt
	}
	return n
}

// resolveHelper returns the helper pane for the given slot in the window this
// server's own pane lives in, reusing or creating one as needed. The returned
// slot number is the one that was asked for.
//
// created reports whether the returned pane is new *to this slot*. It is part of
// every response because it is the only signal an agent gets that the process it
// left running there is gone: the user closed the pane, the process died with
// it, and the next call quietly gets a fresh shell. Without the field that
// failure is silent, and the agent goes on reporting a dev server that stopped
// ten minutes ago.
//
// The mutex is held across the whole resolution, and both halves of that matter.
// mcp-go's stdio server does not handle tool calls one at a time — it queues
// them onto a worker pool (5 workers by default, server/stdio.go), so two
// concurrent calls that both default to slot 1 would each find no candidate and
// each create a pane, and the user would watch two identical empty splits
// appear. What the mutex does NOT do is order two *different* server processes
// sharing a window: subagents may share one server, but two agent sessions in
// one window do not, and tmux offers no compare-and-set on options. That race is
// unpreventable and is instead *repaired* by the duplicate-slot healing in
// slotCandidateLocked, which is not defensive decoration but the second half of
// this lock.
func (t *tmuxClient) resolveHelper(ctx context.Context, slot int) (paneID string, resolved int, created bool, err error) {
	t.slotMu.Lock()
	defer t.slotMu.Unlock()
	return t.resolveHelperLocked(ctx, slot)
}

// resolveHelperNoCreate finds the pane occupying a slot without creating or
// adopting one. close-pane needs it: creating a pane in order to close it would
// be absurd, and adopting one in order to release it would touch a pane the user
// owns for no reason at all.
//
// It runs the same self-exclusion and duplicate healing as the full resolver,
// because those are repairs to state that is already wrong — leaving them to the
// create path would mean a window could only be healed by growing.
//
// This is the lock-taking wrapper, kept because it is the honest way to ask the
// question from outside a lock hold. The teardown that close-pane actually
// performs goes through closePanes, which holds slotMu across the lookup AND the
// mutations that follow it and therefore calls the ...Locked body directly — see
// closePanes for why the split is mandatory rather than tidy.
func (t *tmuxClient) resolveHelperNoCreate(ctx context.Context, slot int) (paneRecord, bool, error) {
	t.slotMu.Lock()
	defer t.slotMu.Unlock()

	self, window, err := t.selfWindow(ctx)
	if err != nil {
		return paneRecord{}, false, err
	}
	return t.resolveHelperNoCreateLocked(ctx, slot, self, window)
}

// resolveHelperNoCreateLocked is the body of the above. The caller must hold
// slotMu, and must have read self and window from selfWindow inside that hold —
// passing them in rather than re-reading them is what keeps one teardown call to
// a single answer about which pane is ours.
func (t *tmuxClient) resolveHelperNoCreateLocked(
	ctx context.Context, slot int, self, window string,
) (paneRecord, bool, error) {
	reg, err := t.paneRegistryInWindow(ctx, window)
	if err != nil {
		return paneRecord{}, false, fmt.Errorf("read the pane registry of window %s: %w", window, err)
	}
	rec, ok := t.closeCandidateLocked(ctx, slot, self, reg)
	return rec, ok, nil
}

// resolveHelperLocked is the body of both entry points above. The caller must
// hold slotMu.
//
// Splitting it out is what keeps the whole decision on one registry read. Every
// branch below — reuse, adopt, create — is chosen from the same `reg` snapshot
// and acts while the lock still holds it, so no concurrent caller can claim the
// slot between the read that found it free and the write that takes it.
func (t *tmuxClient) resolveHelperLocked(ctx context.Context, slot int) (string, int, bool, error) {
	self, window, err := t.selfWindow(ctx)
	if err != nil {
		return "", 0, false, err
	}

	reg, err := t.paneRegistryInWindow(ctx, window)
	if err != nil {
		return "", 0, false, fmt.Errorf("read the pane registry of window %s: %w", window, err)
	}

	// Reuse.
	if rec, ok := t.slotCandidateLocked(ctx, slot, self, reg); ok {
		return t.helperResult(rec.PaneID, self, slot, false)
	}

	// Adopt. The registry lookup here is the "unowned" half of the acquisition
	// predicate, done from the map the caller already holds so that a pane we
	// know to be ours costs no extra tmux round-trip; canAcquire re-reads the
	// owner mark for the panes that reach it, because absence from this map means
	// "no record we recognise", which is not the same as "unclaimed".
	if pane, ok := t.acquirePaneLocked(ctx, window, reg, slot, self); ok {
		return t.helperResult(pane, self, slot, true)
	}

	// Create. Failures of the two markers below are deliberately not fatal, for
	// the same reason createSessionOnSocket ignores its own: the pane exists and
	// works, it just will not be found again as a reuse candidate, so the caller
	// loses nothing now and at worst gets a second pane later. Returning an error
	// here would abandon a pane that has already been created.
	place := t.placementForSlot(ctx, window, reg, slot, self)
	cp, err := t.SplitPane(ctx, place.anchor, place.direction, place.size)
	if err != nil {
		return "", 0, false, fmt.Errorf("create helper pane for slot %d: %w", slot, err)
	}
	_ = t.setPaneSlot(ctx, cp.PaneID, slot)
	_ = t.setPaneTitle(ctx, cp.PaneID, helperTitle(slot))
	t.waitForShellReady(ctx, cp.PaneID)
	return t.helperResult(cp.PaneID, self, slot, true)
}

// helperResult is the single exit through which every resolution passes, and it
// exists for the one check it performs.
//
// Returning the server's own pane would mean the agent typing into its own
// session — the failure this entire design exists to prevent. It should be
// unreachable: the self-exclusion in slotCandidateLocked drops self from reuse,
// canAcquire drops it from acquisition, and a split never returns the pane it
// split. The check is written anyway because it costs one string comparison, and
// because the failure it guards against is the one failure that cannot be
// diagnosed from outside the process: the agent's own transcript would fill with
// its own keystrokes and there would be nothing left to read the diagnosis from.
func (t *tmuxClient) helperResult(paneID, self string, slot int, created bool) (string, int, bool, error) {
	if paneID == self {
		return "", 0, false, fmt.Errorf(
			"internal error: slot %d resolved to this server's own pane %s; refusing to return it",
			slot, paneID)
	}
	return paneID, slot, created, nil
}

// slotCandidateLocked returns the pane currently holding slot, and repairs the
// registry on the way past. The caller must hold slotMu.
//
// Two repairs happen here, and both are cases where the state is already wrong
// before this call begins.
//
// Self-exclusion. A server whose own pane was created by an *outer* agent's
// split-pane call inherits a pane that carries a valid witness, owner "agent",
// and possibly a slot marker — so the inner server can find itself in its own
// registry and resolve "the helper" to its own session. Nested agents are not
// exotic; that is exactly what an agent spawning a subagent into a split looks
// like. The marker is cleared rather than merely skipped so the same pane is not
// re-examined on every later call, and so the stale claim stops steering anyone
// else.
//
// Duplicate healing. Two server processes sharing one window can both create a
// pane for the same slot, because tmux has no compare-and-set on options (see
// resolveHelper). The lowest-numbered — oldest — pane wins, and the others have
// their slot markers cleared: they stay agent-owned panes, they simply stop
// answering to a slot. Keeping the oldest is the choice that preserves whatever
// long-running process the caller already started.
func (t *tmuxClient) slotCandidateLocked(
	ctx context.Context, slot int, self string, reg map[string]paneRecord,
) (paneRecord, bool) {
	var candidates []paneRecord
	for _, rec := range reg {
		// A dead pane is not a candidate. It still appears in list-panes when the
		// user has remain-on-exit set, it still accepts send-keys, and it
		// silently swallows every keystroke — the worst possible helper: no
		// error, no output, no clue.
		if rec.Slot != slot || rec.Dead {
			continue
		}
		if rec.PaneID == self {
			_ = t.setPaneSlot(ctx, rec.PaneID, 0)
			continue
		}
		candidates = append(candidates, rec)
	}
	if len(candidates) == 0 {
		return paneRecord{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return paneIDNumber(candidates[i].PaneID) < paneIDNumber(candidates[j].PaneID)
	})
	for _, dup := range candidates[1:] {
		t.retireDuplicateLocked(ctx, dup)
	}
	return candidates[0], true
}

// retireDuplicateLocked takes the slot away from the loser of a duplicate-slot
// race, and for an adopted pane it does considerably more than that. The caller
// must hold slotMu.
//
// Merely unslotting an ownerAcquired loser strands one of the USER's panes in
// the exact state clearPaneRegistration's comment says must never be produced:
// invisible to close-pane({slot:"all"}), which covers slotted records only;
// permanently un-re-adoptable, because canAcquire refuses any pane whose owner
// mark is set at all; and still wearing the title "agent". To the user that is
// an ordinary shell the agent mysteriously refuses to touch again, and no tool
// in this server lists it, so nothing will ever come back for it. An adopted
// loser is therefore RELEASED — the same C-c, the same three markers, the same
// label removal close-pane would perform — because a claim we have just decided
// not to use is a claim we must drop.
//
// An ownerAgent loser keeps the older, lighter treatment: the slot marker goes
// and the pane stays. It is a pane this server created, so nothing of the user's
// is trapped inside it, and it may well be running the long process whose
// survival is the whole reason the OLDEST pane wins this race — killing it here,
// from inside a resolution nobody asked to destroy anything, would throw that
// away. It becomes an unslotted agent pane, which is kill-pane territory, and
// that is the documented cost of the cross-process race this healing repairs.
func (t *tmuxClient) retireDuplicateLocked(ctx context.Context, rec paneRecord) {
	if rec.Owner == ownerAcquired {
		_ = t.releaseAcquiredLocked(ctx, rec)
		return
	}
	_ = t.setPaneSlot(ctx, rec.PaneID, 0)
}

// closeCandidateLocked returns the pane occupying a slot for the purpose of
// CLOSING it, which is a different question from the one slotCandidateLocked
// answers. The caller must hold slotMu.
//
// The reuse path skips dead panes and must go on skipping them: a pane whose
// process has exited still appears in list-panes under remain-on-exit, still
// accepts send-keys, and silently swallows every keystroke. Handing one back as
// a helper is the worst failure available to this server, and that filter is not
// what is being relaxed here.
//
// The close path wants the opposite thing. A corpse in slot 1 is precisely what
// the user is looking at and what "close the helper pane" means; answering
// {"slot":1,"action":"none"} conflates "this slot is empty" with "this slot
// holds a body I will not touch", and leaves the dead pane on screen to be
// cleared by hand. Worse, close-pane({slot:"all"}) never filtered dead and kills
// it happily — so the two forms of one tool disagreed about the same pane, which
// is not a policy but a bug: the pane the user most wants gone was reachable
// only through the sweep-everything form, while the no-argument teardown the
// description steers callers to reported success and did nothing.
//
// A live candidate always wins, so self-exclusion and duplicate healing happen
// exactly as they do on the reuse path and a corpse is only ever the answer when
// the slot holds nothing else. Only the lowest-numbered corpse is returned per
// call; any others stay slotted and remain visible to slot:"all", which is the
// path that exists to sweep them.
//
// The reuse path keeps its filter, so a caller that asks for slot 1 while a
// corpse holds it gets a fresh pane rather than the body — the corpse is only
// reachable through close-pane, whose entire job is disposing of it.
func (t *tmuxClient) closeCandidateLocked(
	ctx context.Context, slot int, self string, reg map[string]paneRecord,
) (paneRecord, bool) {
	if rec, ok := t.slotCandidateLocked(ctx, slot, self, reg); ok {
		return rec, true
	}
	var dead []paneRecord
	for _, rec := range reg {
		if rec.Slot != slot || !rec.Dead || rec.PaneID == self {
			continue
		}
		dead = append(dead, rec)
	}
	if len(dead) == 0 {
		return paneRecord{}, false
	}
	sort.Slice(dead, func(i, j int) bool {
		return paneIDNumber(dead[i].PaneID) < paneIDNumber(dead[j].PaneID)
	})
	return dead[0], true
}

// acquirePaneLocked adopts the first pane in the window that may be adopted, and
// marks it as ours-by-adoption. The caller must hold slotMu.
//
// Panes are considered in pane-id order — oldest first — so that the choice is
// deterministic under test and so that repeated adoptions in one window walk the
// same sequence rather than depending on tmux's layout ordering.
//
// The owner recorded is ownerAcquired, never ownerAgent, and that distinction is
// the whole reason the owner field carries a kind rather than a boolean:
// close-pane KILLS a pane we created and merely RELEASES one we adopted, because
// killing a pane the user opened is the single unrecoverable action in this
// design. Marking an adopted pane "agent" would also make it a reuse candidate
// for legacy split-pane calls, whose contract says a reused pane is safe to
// kill.
//
// A failure to write the markers abandons the adoption rather than returning the
// pane: an adopted pane whose owner mark did not stick is a pane we would type
// into and never be able to identify again, which is worse than not adopting it.
func (t *tmuxClient) acquirePaneLocked(
	ctx context.Context, windowID string, reg map[string]paneRecord, slot int, self string,
) (string, bool) {
	panes, err := t.ListPanes(ctx, windowID)
	if err != nil {
		return "", false
	}
	sort.Slice(panes, func(i, j int) bool {
		return paneIDNumber(panes[i].ID) < paneIDNumber(panes[j].ID)
	})
	for _, p := range panes {
		if _, claimed := reg[p.ID]; claimed {
			continue
		}
		if !t.canAcquire(ctx, p.ID, self) {
			continue
		}
		// markPaneOwnedAs is the one writer that takes socket and bare id
		// separately, because its original callers are mid-creation and hold
		// both; here they have to be split back out of the id ListPanes reported.
		socket, bareID := parseTarget(p.ID)
		if err := t.markPaneOwnedAs(ctx, socket, bareID, ownerAcquired, slot); err != nil {
			continue
		}
		_ = t.setPaneTitle(ctx, p.ID, helperTitle(slot))
		return p.ID, true
	}
	return "", false
}

// canAcquire reports whether an unowned pane may be adopted as a helper.
//
// Every error is a "no". This predicate decides whether to type into a pane that
// the USER, not the server, opened; a probe that failed is not evidence of
// safety, and the fallback when it says no is merely to create a pane, which is
// always correct and only slightly less thrifty.
//
// # What it checks
//
// The pane is not this server's own (Invariant R). It is alive with its shell in
// the foreground — paneIsIdleShell, NOT PaneState.WaitingForInput, and that must
// not regress: an idle interactive shell reports waitingForInput=false on Linux,
// where readline blocks in poll/select rather than n_tty_read, and true on
// macOS. It carries no owner mark of any kind. And the process that would
// receive our keystrokes belongs to the same user as this server.
//
// The uid guard's usual justification is half wrong, and the correction matters
// because the next reader will otherwise delete the guard as redundant. An
// ordinary `sudo -i` does NOT slip past the idle check: sudo runs as a child of
// the user's shell, so ForegroundPID != PanePID and paneIsIdleShell already says
// no. What the uid guard actually closes is the case where the pane's OWN
// process belongs to another user — `exec sudo -i`, a pane started as
// `split-window sudo -i`, an exec'd `su`, a shell in a namespace with a
// different uid. There ForegroundPID == PanePID, the command is "bash", and
// nothing but the uid tells it apart from an ordinary idle shell.
//
// # What it cannot check — known, accepted limitations
//
// These are not TODOs. They are the price of acquisition, accepted knowingly
// when this shipped enabled by default, and they are written down because the
// guards above look complete and are not.
//
// It cannot see the tty's input buffer. A pane where the user typed a command
// and never pressed Enter is indistinguishable from a pane sitting at an empty
// prompt: same foreground process, same pid, same everything we can observe.
// Adopting it and sending "cmd\n" executes the CONCATENATION of their
// unsubmitted text and our command — `git push --forceecho hello`, or worse,
// something that parses. This is not detectable from outside the process: tmux
// exposes #{cursor_x} and #{cursor_y} but nothing that says where the prompt
// ends, so pending input cannot be told apart from a wide prompt. (write-to-
// display sends C-u before writing for exactly this reason; that clears the
// user's line, which is a destruction we are not willing to perform here.)
//
// And the uid guard proves WHO owns the shell, not WHAT context it is in. A
// shell inside a user namespace, inside a virtualenv, inside a container's exec
// session, or one with AWS_PROFILE=production exported, passes every check here
// — it is the right user, at a prompt, doing nothing. Our commands then run in
// that context, with that environment, against that account.
func (t *tmuxClient) canAcquire(ctx context.Context, paneID, selfPaneID string) bool {
	if paneID == "" || paneID == selfPaneID {
		return false
	}
	owner, err := t.paneOwnerMark(ctx, paneID)
	if err != nil || owner != "" {
		return false
	}
	state, err := t.GetPaneState(ctx, paneID)
	if err != nil {
		return false
	}
	if !paneIsIdleShell(state) {
		return false
	}
	return sameUIDAsAgent(state)
}

// sameUIDAsAgent reports whether the process that would receive our keystrokes
// runs as the same user as this server.
//
// The pid examined is the FOREGROUND pid rather than the pane pid, because the
// tty delivers keystrokes to the foreground process group: the foreground
// process is the one that would execute what we type. paneIsIdleShell has
// already established that the two are equal, so today this reads the same
// process either way — naming the foreground pid keeps the check correct if that
// ordering is ever changed.
//
// Both the real and the effective uid must match. A process that dropped one but
// not the other is not a process we may type into, because the keystrokes would
// execute under whichever the shell restores, and we cannot know which.
//
// The reference identity is os.Getuid(), this server's own uid, rather than the
// uid of the agent's pane. It needs no syscall, cannot fail, and fails in the
// safe direction: a server somehow running as root would compare 0 against the
// user's 501 and acquire nothing at all, whereas comparing pane-to-pane would
// have it cheerfully adopt every shell the user owns.
func sameUIDAsAgent(state *PaneState) bool {
	if state == nil {
		return false
	}
	real, effective, err := processUIDs(state.ForegroundPID)
	if err != nil {
		return false
	}
	me := os.Getuid()
	return real == me && effective == me
}

// helperPlacement is the server-decided geometry for a slot: which pane to
// split, which way, and how big. The agent never passes a direction, and this is
// where that decision lives instead.
type helperPlacement struct {
	anchor    string // the pane to split
	direction string // "horizontal" | "vertical"
	size      int    // percentage; 0 means tmux's default
}

// placementForSlot decides where a new helper pane goes.
//
//	slot 1  → split self horizontally: the helper lands beside the agent, which
//	          is where the user is already looking.
//	slot 2  → split the slot-1 pane vertically, so the two helpers stack in the
//	          same column instead of squeezing the agent's own pane again.
//	slot 3  → split self vertically.
//	slot ≥4 → split whichever agent-owned pane has the most room left.
//
// Only the anchor can be missing, and every fallback chain terminates at self,
// which cannot be missing: resolveHelperLocked has already failed with
// errNotInTmux if there were no self pane. So a slot-2 request made while slot 1
// does not exist produces exactly one pane, placed where slot 1 would have gone
// and marked slot 2.
//
// It deliberately does NOT create the missing anchor first. Resolving one slot
// creates at most one pane: creating slot 1 as a side effect of a slot-2 request
// would give the caller a pane it did not ask for and cannot name, and would
// make created ambiguous — the flag means "the pane for *this* slot is new", and
// it cannot mean that if the call also produced a second pane for another slot.
//
// It returns no error. Every failure inside degrades to the self anchor, which
// is where the fallback chain ends anyway; letting a cosmetic geometry decision
// fail a resolution that is otherwise able to proceed would trade a slightly
// worse layout for no pane at all.
func (t *tmuxClient) placementForSlot(
	ctx context.Context, windowID string, reg map[string]paneRecord, slot int, self string,
) helperPlacement {
	switch slot {
	case 1:
		return helperPlacement{anchor: self, direction: "horizontal", size: 50}
	case 2:
		if rec, ok := aliveSlotPane(reg, slotDefault); ok && rec.PaneID != self {
			return helperPlacement{anchor: rec.PaneID, direction: "vertical", size: 50}
		}
		return helperPlacement{anchor: t.anchorOrSelf(ctx, windowID, reg, self), direction: "vertical", size: 50}
	case 3:
		return helperPlacement{anchor: self, direction: "vertical", size: 50}
	default:
		return helperPlacement{anchor: t.anchorOrSelf(ctx, windowID, reg, self), direction: "vertical", size: 50}
	}
}

// aliveSlotPane returns the live pane holding the given slot, if any.
func aliveSlotPane(reg map[string]paneRecord, slot int) (paneRecord, bool) {
	for _, rec := range reg {
		if rec.Slot == slot && !rec.Dead {
			return rec, true
		}
	}
	return paneRecord{}, false
}

// anchorOrSelf returns the agent-owned pane with the most room to give up, or
// self when there is none.
//
// Panes are ranked by AREA — width × height. tmux exposes no area variable, and
// width and height are the only per-pane size facts available; area rather than
// either dimension alone is the right metric because a split halves one
// dimension, and the pane that can lose half of one and stay usable is the one
// with the most cells to start with. Ties go to the lowest pane id purely so the
// choice is deterministic under test.
//
// Acquired panes are excluded on purpose. An acquired pane is the user's real
// estate: we may type into it, because that is what acquisition means, but
// halving it rearranges a layout the user built by hand, which is a visible
// change nobody asked for. We split panes we made.
func (t *tmuxClient) anchorOrSelf(
	ctx context.Context, windowID string, reg map[string]paneRecord, self string,
) string {
	panes, err := t.ListPanes(ctx, windowID)
	if err != nil {
		return self
	}
	best, bestArea := "", 0
	for _, p := range panes {
		rec, ok := reg[p.ID]
		if !ok || rec.Owner != ownerAgent || rec.Dead || p.ID == self {
			continue
		}
		area := p.Width * p.Height
		switch {
		case area > bestArea:
			best, bestArea = p.ID, area
		case area == bestArea && best != "" && paneIDNumber(p.ID) < paneIDNumber(best):
			best = p.ID
		}
	}
	if best == "" {
		return self
	}
	return best
}

// ---- Teardown ----
//
// Everything from here to waitForShellReady runs under the ONE slotMu hold that
// closePanes takes, and the ...Locked suffix is the reminder. See closePanes.

// Teardown actions, as reported in close-pane's response.
const (
	actionKilled   = "killed"
	actionReleased = "released"
	actionNone     = "none"
	actionError    = "error"
)

// errCloseSelfPane is the refusal close-pane returns when it is pointed at the
// pane this server is running in.
//
// It is a sentinel rather than a bare message because the two call shapes need
// the same refusal in different packaging: a caller that named the pane
// explicitly gets it as the whole answer, so it cannot be mistaken for success,
// while a batch teardown records it as one entry and carries on with the rest.
var errCloseSelfPane = errors.New("refusing to close this server's own pane")

// closeHelperLocked releases one helper pane according to who owns it. The
// caller must hold slotMu and must pass the self pane it read inside that hold.
//
// The distinction is the entire reason the owner field carries a kind. Killing a
// pane the server created is a clean undo: nothing existed before we made it and
// nothing survives that the user wanted. Killing a pane the USER opened is the
// one unrecoverable action available anywhere in this design — whatever was in
// its scrollback, whatever they had half-typed, whatever ssh session it held, is
// gone with no way back. Sending C-c into an idle shell we were allowed to adopt
// is not in that category: it interrupts nothing that was running, and hands the
// pane back in the state we found it.
//
// # Why the self guard is here and not in the caller
//
// Every OTHER path already excludes this server's own pane — slotCandidateLocked
// drops it from reuse, canAcquire drops it from acquisition,
// slottedHelpersInSelfWindowLocked drops it from "all" — and the one path that
// did not was the explicit one. close-pane({paneId}) looked the pane up, found a
// valid agent-owned record, and went straight to KillPane. Putting the guard at
// the single point every teardown funnels through is what stops the next
// teardown path from having to remember.
//
// The scenario is not exotic; this file's own comments call it ordinary. An
// outer agent's split-pane creates %7 with owner=agent, slot=1 and the title
// "agent"; a subagent is started inside %7 and its server inherits TMUX_PANE=%7.
// Every enumeration the inner agent can perform then says %7 is a helper — the
// record is valid, the witness matches, the title says agent — so a tidy
// end-of-task close-pane({paneId:"%7"}) destroys the session it is running in,
// through the tool whose entire contract is that it is the safe closer. Because
// the record is exactly what makes the pane look closeable, no amount of further
// record checking can catch this; only the identity comparison can.
//
// self arrives as a parameter for the same reason helperResult takes one: it is
// read once per call, from selfWindow, under the lock — and a future teardown
// path cannot compile without deciding what to pass, which is the compiler
// asking whether it has thought about this. An empty self means the server is
// not running in tmux at all, so there is no own pane to protect and the guard
// is correctly vacuous; closePanes is where that case is established.
//
// kill-pane remains the deliberate way to destroy any pane, including this one.
// It keeps its blunt signature — paneId required, no slot, no default — precisely
// so that destroying something is never the accidental outcome of a tidy-up.
func (t *tmuxClient) closeHelperLocked(ctx context.Context, rec paneRecord, self string) (string, error) {
	if self != "" && rec.PaneID == self {
		return actionError, fmt.Errorf(
			"%w: %s is the pane this server is running in, and closing it would kill the session "+
				"this very request arrived through — the conversation and everything in it. A pane "+
				"created by an outer agent carries a perfectly valid agent-owned record, so the "+
				"record is not what makes a pane safe to close. Use kill-pane if destroying it is "+
				"genuinely what you want",
			errCloseSelfPane, rec.PaneID)
	}
	switch rec.Owner {
	case ownerAgent:
		if err := t.KillPane(ctx, rec.PaneID); err != nil {
			return actionError, err
		}
		return actionKilled, nil
	case ownerAcquired:
		if err := t.releaseAcquiredLocked(ctx, rec); err != nil {
			return actionError, err
		}
		return actionReleased, nil
	default:
		// Unreachable: every caller checks for a record first, and a record with
		// an unrecognised owner never leaves the registry readers.
		return actionNone, fmt.Errorf("pane %s has owner %q, which this binary does not manage",
			rec.PaneID, rec.Owner)
	}
}

// releaseAcquiredLocked hands an adopted pane back to the user: interrupt
// whatever we may have left running in it, remove every marker, drop the label.
// The caller must hold slotMu.
//
// It is shared by teardown and by duplicate healing on purpose. Those are the
// only two places that stop using a pane the user owns, and a second hand-written
// copy of "give it back" is how one of them would come to give back less — which
// is exactly the defect retireDuplicateLocked exists to fix.
//
// literal=false on the C-c is mandatory, not stylistic. With -l (literal=true)
// tmux would send the three characters C, -, c into the user's shell instead of
// an interrupt — leaving them a pane containing the text "C-c" and a helper that
// believed it had cleaned up. The same trap is already documented on
// write-to-display's clear and on SendKeys itself.
//
// A dead pane is not interrupted. There is no process left to signal, and
// send-keys into a pane under remain-on-exit reports success while doing
// nothing — the kind of success this file spends its comments warning about.
//
// The title is dropped last, and its failure is ignored, because it is the only
// cosmetic step here: a pane still labelled "agent" after we have given up every
// claim to it is a lie to the user, but one that has cost them nothing, and it
// must never be the reason a release reports failure. The label goes to empty
// rather than back to whatever the user had, because we never saw what that was.
func (t *tmuxClient) releaseAcquiredLocked(ctx context.Context, rec paneRecord) error {
	if !rec.Dead {
		if err := t.SendKeys(ctx, rec.PaneID, "C-c", false, false); err != nil {
			return err
		}
	}
	if err := t.clearPaneRegistration(ctx, rec.PaneID); err != nil {
		return err
	}
	_ = t.setPaneTitle(ctx, rec.PaneID, "")
	return nil
}

// closeSelector names what close-pane was asked to close. Exactly one form
// applies, in the order the tool documents: an explicit pane wins, then "all",
// then a slot number (1 when the caller named nothing at all).
type closeSelector struct {
	PaneID string
	All    bool
	Slot   int
}

// closePanes performs every form of close-pane, and holding slotMu across the
// whole of it is the point of the function existing.
//
// Teardown mutates precisely the state resolution reads: it kills panes,
// interrupts shells and erases registry markers. Doing that outside the lock
// races the resolver on the worker pool mcp-go's stdio server dispatches tool
// calls onto (5 workers by default, server/stdio.go). The concrete failure:
// close-pane releases the slot-1 pane — C-c, then clearPaneRegistration — while a
// concurrent bare send-keys takes slotMu, still sees the un-cleared record,
// resolves to that same pane and types into it mid-release. The mirror ordering
// kills a pane between a resolver's return and its SendKeys, so the keystrokes
// land in a pane that no longer exists (or, under remain-on-exit, in one that
// swallows them silently). That is exactly the race the mutex was built for, and
// it was eluded on the one path that destroys things.
//
// The lock is taken HERE and nowhere below it, because sync.Mutex is not
// reentrant: everything this calls is a ...Locked variant, and reaching for the
// lock-taking resolveHelper or resolveHelperNoCreate from inside would be a hard
// deadlock — a hung tool call, not a failing test.
// TestConcurrentSlotResolutionNeitherDuplicatesNorHangs catches that by racing
// real resolutions against a real teardown under a deadline, so a reintroduced
// double-take fails the suite in seconds instead of hanging it. An earlier
// version of that guard walked the AST for second Lock() calls, which read as
// stricter but matched on selector names rather than values — it could not see
// the lock taken through a function variable, i.e. the case most likely to
// reintroduce this.
func (t *tmuxClient) closePanes(ctx context.Context, sel closeSelector) ([]closedPane, error) {
	t.slotMu.Lock()
	defer t.slotMu.Unlock()

	// One read of "which pane am I", used for the guard in closeHelperLocked and
	// for the window every slot is scoped to.
	//
	// Not being in tmux is fatal to the two window-scoped forms — there is no
	// window to enumerate and no slot to resolve — but an explicit paneId names
	// its pane absolutely and must keep working, because that is how a headless
	// pane is closed by a server that was never in tmux to begin with. There is
	// no own pane to protect in that case, so the guard has nothing to do.
	//
	// Any OTHER failure refuses every form, including the explicit one. It means
	// we could not establish which pane is ours, and the self guard is the only
	// thing standing between close-pane and the agent's own session: a guard that
	// cannot be evaluated must not be skipped.
	self, window, err := t.selfWindow(ctx)
	if err != nil {
		if sel.PaneID == "" || !errors.Is(err, errNotInTmux) {
			return nil, err
		}
		self, window = "", ""
	}

	switch {
	case sel.PaneID != "":
		return t.closeExplicitLocked(ctx, sel.PaneID, self)
	case sel.All:
		recs, err := t.slottedHelpersInSelfWindowLocked(ctx, self, window)
		if err != nil {
			return nil, err
		}
		closed := make([]closedPane, 0, len(recs))
		for _, rec := range recs {
			// A failure on one pane does not abort the loop. Teardown that stops
			// at the first problem leaves the caller worse off than one that
			// continues and reports: the panes after the failure would stay open
			// with no indication that they had not been considered.
			entry, _ := t.closeOneLocked(ctx, rec, self)
			closed = append(closed, entry)
		}
		return closed, nil
	default:
		rec, found, err := t.resolveHelperNoCreateLocked(ctx, sel.Slot, self, window)
		if err != nil {
			return nil, err
		}
		if !found {
			// A slot that holds nothing is not an error. "Close slot 2" when slot
			// 2 was never opened is a request that has already been satisfied.
			return []closedPane{{Slot: sel.Slot, Action: actionNone}}, nil
		}
		entry, _ := t.closeOneLocked(ctx, rec, self)
		return []closedPane{entry}, nil
	}
}

// closeExplicitLocked closes the one pane a caller named. The caller must hold
// slotMu.
//
// The refusal of a pane with no registry record is what keeps close-pane from
// becoming a second kill-pane with a friendlier name and a wider blast radius —
// see registerClosePane. The self refusal is raised to a tool error here for the
// same reason: a caller that named a pane and got back an entry saying
// action:"error" could reasonably read the array as "teardown ran", whereas an
// error is unmistakable. In a batch the same refusal stays an entry, because one
// pane must not stop the sweep.
func (t *tmuxClient) closeExplicitLocked(ctx context.Context, paneID, self string) ([]closedPane, error) {
	rec, found, err := t.paneRecordFor(ctx, paneID)
	if err != nil {
		return nil, fmt.Errorf("failed to read pane record: %w", err)
	}
	if !found {
		return nil, fmt.Errorf(
			"pane %s is not one of this server's helper panes (it carries no "+
				"@mcp_pane/@mcp_owner record) — refusing to close it; use kill-pane if you "+
				"are certain", paneID)
	}
	entry, err := t.closeOneLocked(ctx, rec, self)
	if errors.Is(err, errCloseSelfPane) {
		return nil, err
	}
	return []closedPane{entry}, nil
}

// closeOneLocked turns one record into one response entry, and hands back the
// error as well. Both halves are needed: the entry is what a batch teardown
// reports and carries on from, while the error is how the explicit branch tells
// the self refusal apart from an ordinary tmux failure. The caller must hold
// slotMu.
func (t *tmuxClient) closeOneLocked(ctx context.Context, rec paneRecord, self string) (closedPane, error) {
	action, err := t.closeHelperLocked(ctx, rec, self)
	entry := closedPane{PaneID: rec.PaneID, Slot: rec.Slot, Action: action}
	if err != nil {
		entry.Action = actionError
		entry.Detail = err.Error()
	}
	return entry, err
}

// slottedHelpersInSelfWindowLocked returns every slotted helper in the window
// this server runs in, ordered by slot and then by pane id. It backs
// close-pane({slot:"all"}). The caller must hold slotMu, and passes the self and
// window it read from selfWindow inside that hold.
//
// Three scoping decisions live here, and none of them is arbitrary.
//
// It is WINDOW-scoped, because resolution is: "all" undoes what the resolver
// did, and the resolver never reaches outside this window. An agent tearing down
// must not be able to touch the user's other windows.
//
// It covers SLOTTED records only. An agent-owned pane with no slot was created
// by an explicit split-pane({paneId}) call, whose lifecycle belongs to the caller
// that named the pane; close-pane is the inverse of resolveHelper and should
// undo exactly what resolveHelper did. kill-pane remains available for the rest.
//
// It EXCLUDES this server's own pane, which is not a formality. A server whose
// pane was created by an outer agent can inherit a stale slot marker, and "close
// everything" would then C-c the agent's own session — interrupting the very
// conversation that asked for the teardown, from a tool whose purpose is
// tidiness. (closeHelperLocked refuses self as well, so this exclusion is now
// the outer of two; it stays because dropping a pane from the sweep is a better
// answer than reporting an error entry for it every time.)
//
// It deliberately does NOT filter dead panes, and closeCandidateLocked was
// taught to match rather than the reverse: a corpse is exactly what a teardown
// is for. The reuse path is the one that must keep skipping them.
func (t *tmuxClient) slottedHelpersInSelfWindowLocked(
	ctx context.Context, self, window string,
) ([]paneRecord, error) {
	reg, err := t.paneRegistryInWindow(ctx, window)
	if err != nil {
		return nil, fmt.Errorf("read the pane registry of window %s: %w", window, err)
	}
	var recs []paneRecord
	for _, rec := range reg {
		if rec.Slot < slotDefault || rec.PaneID == self {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Slot != recs[j].Slot {
			return recs[i].Slot < recs[j].Slot
		}
		return paneIDNumber(recs[i].PaneID) < paneIDNumber(recs[j].PaneID)
	})
	return recs, nil
}

// waitForShellReady blocks until a just-created pane's shell is sitting at its
// prompt, or until a short timeout expires. It is best-effort by design and
// never reports failure.
//
// tmux's split-window returns as soon as the pane exists, which is well before
// the shell inside it has finished sourcing its rc files. This repo already
// carries direct evidence that the gap is real and long enough to matter: the
// comment on foregroundOfGroup in process.go documents powerlevel10k forking
// helper processes out of its precmd hooks on a developer's machine, which is
// the same startup work, observed from the other side. Keystrokes delivered into
// a shell that is still initialising can be mangled or dropped outright — the
// tty's line discipline and echo settings are still being configured, and any
// input that arrives first is at the mercy of whatever the shell does next —
// which produces the worst class of bug this server can have: a command that
// looks sent, leaves a plausible-looking pane, and never ran.
//
// A timeout returns the pane anyway rather than failing. A slow shell is not an
// error, and a resolution that refused to hand back a pane it had just
// successfully created would turn a cosmetic delay into a hard failure. The wait
// is short for the same reason: it is insurance against the common case, not a
// guarantee.
//
// Only the creation path waits. Reuse does not need it — a pane is only a reuse
// candidate because it was already found idle — and neither does acquisition,
// where canAcquire has just proved the shell idle as its precondition.
func (t *tmuxClient) waitForShellReady(ctx context.Context, paneID string) {
	const (
		readyTimeout  = 2 * time.Second
		readyInterval = 50 * time.Millisecond
	)
	deadline := time.Now().Add(readyTimeout)
	for {
		if state, err := t.GetPaneState(ctx, paneID); err == nil && paneIsIdleShell(state) {
			return
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		time.Sleep(readyInterval)
	}
}
