package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// This file holds helper-pane *policy*: which pane the agent gets, where it is
// placed, which of our own panes may be reclaimed, and how keys are typed into a
// shell so that they are known to have landed.
// backend_tmux.go is a thin wrapper over the tmux CLI — everything in it is a
// command with arguments — and mixing policy into it would spread the
// safety-critical decisions across a 900-line client. The low-level option reads
// and writes stay there, next to the witness constant that explains them; the
// rules about what to do with those records live here, findable as a unit.
//
// The boundary is now a port rather than a file-naming convention: this file
// holds a Backend and nothing else, so it cannot reach a tmux command even by
// accident, and it never sees a pane id — it holds opaque paneRefs.

// slots is the policy layer, and the change of receiver IS the design.
//
// It used to be tmuxClient, which meant every rule below sat one dot away from
// the tmux CLI: a new branch could shell out, parse an id, or invent a second
// way to ask "which pane am I" without anything noticing. Holding only the port
// makes that impossible to write rather than merely discouraged.
//
// slotMu moved here from tmuxClient unchanged, and everything resolveHelper and
// closePanes say about it still applies: mcp-go dispatches tool calls onto a
// worker pool, so two concurrent calls that both default to slot 1 would each
// create a pane; and the lock does NOT order two server processes sharing a
// window, which is why the duplicate healing in slotCandidateLocked is the
// second half of this lock rather than defensive decoration.
//
// shell is the -shell-type flag. execInPane spells its exit-status wrapper for
// the pane's real shell, and uses this only when that shell cannot be named.
// It defaults to bash, the flag's own default.
//
// cwd is this server's working directory, which is the agent's. A new pane
// starts there, and a reclaimed one is put back there before it is handed out.
type slots struct {
	b      Backend
	shell  string
	cwd    string
	slotMu sync.Mutex
}

func newSlots(b Backend) *slots {
	s := &slots{b: b, shell: "bash"}
	if wd, err := os.Getwd(); err == nil {
		s.cwd = wd
	}
	return s
}

// errNoWindow is returned whenever a call needs the server's own pane and there
// is none. It is an INTERNAL sentinel: its text never reaches a caller, because
// "there is no window" is not by itself a failure — a pane with no window is
// exactly what an invisible slot is — and only a request that needed a VISIBLE
// pane turns it into a message.
//
// Callers test it with errors.Is and hand the caller errNoWindowText instead.
var errNoWindow = errors.New("this server is not running inside a multiplexer pane")

// errNoWindowText is the one sentence a caller sees when it asked for a pane in
// the user's window and there is no window.
//
// It names the way forward on purpose, and that property is older than this
// contract. The incident this design comes from began with an agent that could
// not tell "I am not in tmux" from "I passed the wrong argument", and responded
// by shelling out to raw tmux to find out — the behaviour every tool in this
// server exists to make unnecessary. An error that says only "no pane" invites
// exactly that guessing. The predecessor of this sentence named three routes —
// paneId, headless:true and create-headless — and all three are deleted by this
// release, so it would have directed an agent to three things that do not exist.
//
// It names no multiplexer. The agent's tools are the same under every backend,
// and a sentence that said "tmux" would tell it which one it is under — and,
// under herdr, would be false.
const errNoWindowText = "no window to place a pane in: this server is not running inside a " +
	"terminal pane — use isolated: true"

// visibleError maps the internal sentinel onto that sentence. It is applied
// where a request that needed the user's window fails, and nowhere else: a path
// that can proceed without a window must test errNoWindow and carry on rather
// than dressing it up as a message.
func visibleError(err error) error {
	if errors.Is(err, errNoWindow) {
		return errors.New(errNoWindowText)
	}
	return err
}

// The window scope did not go away with selfWindow; it moved onto the port.
//
// Every slot operation is still scoped to the one window this server's pane
// lives in, and that is a deliberate limit rather than an implementation
// shortcut: the agent's helper panes belong beside the agent, where the user can
// see them, and a resolution that could reach into the user's other windows
// would be able to hand back a pane the user is not looking at — which is
// indistinguishable, from the agent's side, from a pane it created. Teardown
// follows the same scope for the same reason.
//
// Backend.Records and Backend.Siblings take "the pane the scope is around" and
// resolve the window themselves, so a window id is no longer a value policy
// holds, passes or could accidentally widen. Invariant S survives as Backend
// .Self, still the single accessor and still the only reader of TMUX_PANE —
// see TestOnlyPolicyCodeKnowsOurOwnPane.

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
//
// Isolated panes are titled the same way, and consistency is the whole reason:
// nothing reads the title of a pane nobody can see. One place decides what a
// helper pane is called, and a special case here would be a second.
func helperTitle(slot int) string {
	if slot <= slotDefault {
		return "agent"
	}
	return fmt.Sprintf("agent:%d", slot)
}

// kindRequest is what the CALLER said about the kind of pane it wants, which is
// three answers and not two.
//
// "isolated was omitted" and "isolated was false" are different requests.
// Omission means "I am addressing a slot, whatever kind it is" — the only thing
// a reading tool can mean, since reading tools have no isolated argument at all.
// An explicit false means "I want the visible one", which is a claim that can
// conflict with what the slot already holds.
//
// Reading the omission as false is the mistake this type exists to make
// unwritable: it would make every isolated slot unaddressable by its own number
// the moment the creating call was over, which is the entire contract.
type kindRequest int

const (
	kindUnstated kindRequest = iota // isolated absent: accept whichever kind owns the slot
	kindVisible                     // isolated: false
	kindIsolated                    // isolated: true
)

// The two kind-conflict sentences. A slot is one pane, so a request for the
// other kind of it is a refusal and never a second pane: silently creating an
// invisible slot 2 beside a visible slot 2 would give the agent two panes
// answering to one number, and the next call could not say which it meant.
//
// Both name the way out, because an error that only says "no" is what sends an
// agent looking for another route into the terminal.
const (
	kindIsVisibleText  = "slot %d is a visible pane; close it or use another slot"
	kindIsIsolatedText = "slot %d is an isolated pane; close it or use another slot"
)

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
//
// owner comes back with the pane, and it is not a convenience field. It is the
// registry's answer as read INSIDE this lock — the same read every branch below
// already had to make in order to choose — so that a caller which then has to
// decide something destructive about the pane can decide it from the state
// resolution saw, instead of reading the registry again once the lock is gone.
// Between those two reads a concurrent close-pane can release the pane and hand
// it back to the user, leaving no record at all, and a second read cannot tell
// "released a millisecond ago" from "never claimed". See clearForDisplay, where
// that difference is the user's half-typed command line.
func (s *slots) resolveHelper(ctx context.Context, slot int, kind kindRequest) (paneTarget, error) {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	return s.resolveHelperLocked(ctx, slot, kind)
}

// slotHolder is what a lookup found: the record, and which registry it came
// from. The second half is not derivable from the first — a paneRecord is the
// same shape on either socket — and a reading tool has to report the kind of
// pane it read.
type slotHolder struct {
	Record   paneRecord
	Isolated bool
}

// missingSlotText is what a READING tool says about a slot that does not exist.
//
// It names both ways to make one, because the alternative is an agent that reads
// "slot 3 does not exist", concludes the terminal is unavailable to it, and goes
// looking for another route in — which is the incident this whole design comes
// from. Most callers will not want open-pane at all: they want to run something,
// and running something opens the slot on the way past.
const missingSlotText = "slot %d does not exist; open it with open-pane or by running " +
	"something in it"

// lookupSlot finds the pane occupying a slot without creating or reclaiming one,
// and is the entry point the READING tools use.
//
// This is the lock-taking wrapper. The teardown that close-pane performs goes
// through closePanes, which holds slotMu across the lookup AND the mutations
// that follow it and therefore calls the ...Locked body directly — see
// closePanes for why the split is mandatory rather than tidy.
//
// # Invariant R lives here, and only here
//
// The body below deliberately RETURNS this server's own pane when a stale slot
// marker is on it, because close-pane has to refuse it by name rather than
// report the slot empty. A reading tool must not be handed it: the pane the
// agent is running in is its own transcript, and watch-pane or capture-pane on
// it would feed the model its own output — the failure this design exists to
// prevent, and the one failure that cannot be diagnosed from outside the
// process.
//
// So a self record is a MISS here, and the resulting sentence is the correct
// advice: the caller opens the slot, resolution clears the stale marker on its
// way past (slotCandidateLocked), and a real helper pane appears. Reporting it
// as found would have been the only other option, and there is no sentence for
// it that leads anywhere useful.
func (s *slots) lookupSlot(ctx context.Context, slot int, kind kindRequest) (slotHolder, bool, error) {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()

	self, err := s.b.Self(ctx)
	if err != nil && !errors.Is(err, errNoWindow) {
		return slotHolder{}, false, err
	}
	holder, found, err := s.lookupSlotLocked(ctx, slot, kind, self)
	if err != nil || !found {
		return slotHolder{}, false, err
	}
	if !holder.Isolated && !self.empty() && holder.Record.Ref == self {
		return slotHolder{}, false, nil
	}
	return holder, true, nil
}

// lookupSlotLocked finds the pane occupying a slot WITHOUT creating or reclaiming
// one, in BOTH registries. The caller must hold slotMu and pass the self it read
// inside that hold — an empty self meaning "there is no window", which is not an
// error here.
//
// It is one function because close-pane and the reading tools have to agree
// about what "slot 2" means. They previously did not: this lookup read only the
// visible window, so a read of an isolated slot would have answered "slot 2 does
// not exist" — a lie, and one that sends an agent to re-create a pane it already
// has.
//
// The two callers differ only in what a MISS means, and that difference is the
// caller's: a reading tool errors, close-pane answers {action:"none"}, because
// "close slot 2" when slot 2 was never opened is a request that has already been
// satisfied.
//
// Both repairs the full resolver performs happen here too — self-exclusion and
// duplicate healing — because those fix state that is already wrong, and leaving
// them to the create path would mean a window could only be healed by growing.
//
// Creating one pane in order to close it would be absurd, and reclaiming one in
// order to close it would kill a pane nobody asked about.
func (s *slots) lookupSlotLocked(
	ctx context.Context, slot int, kind kindRequest, self paneRef,
) (slotHolder, bool, error) {
	if kind != kindIsolated && !self.empty() {
		reg, err := s.b.Records(ctx, self)
		if err != nil {
			return slotHolder{}, false, fmt.Errorf("could not read the pane registry for slot %d: %w", slot, err)
		}

		// The server's own pane is CONSIDERED here, and teardown is the only
		// lookup that considers it. Every other path excludes it — reuse must
		// never hand it back, reclaim must never take it, the "all" sweep
		// must never touch it, list-slots must not report it — and here exclusion
		// is wrong for a reason that does not apply to any of those.
		//
		// A subagent started into an outer agent's slot-1 pane inherits a valid
		// agent-owned record on the pane it is running in. Skipping it makes
		// close-pane({slot:1}) answer {"action":"none"}, which says "slot 1 is
		// empty" — and what an agent does next with an empty slot is open one. The
		// truth is the opposite: slot 1 is the pane this request arrived through,
		// and closeHelperLocked is where that is said, in the sentence written for
		// it. Routing the record there rather than dropping it is what keeps that
		// guard reachable now that close-pane({paneId}) — its only other route —
		// is gone.
		//
		// It is returned WITHOUT clearing the stale marker, because unslotting is
		// a mutation and a refused teardown must not leave one behind. The next
		// resolution clears it through slotCandidateLocked, which is where the
		// healing belongs.
		if rec, ok := reg[self]; ok && rec.Slot == slot {
			return slotHolder{Record: rec}, true, nil
		}
		if rec, ok := s.closeCandidateLocked(ctx, slot, self, reg, false); ok {
			return slotHolder{Record: rec}, true, nil
		}
	}

	if kind == kindVisible {
		return slotHolder{}, false, nil
	}
	iso, err := s.b.IsolatedRecords(ctx)
	if err != nil {
		return slotHolder{}, false, fmt.Errorf("could not read the pane registry for slot %d: %w", slot, err)
	}
	rec, ok := s.closeCandidateLocked(ctx, slot, paneRef{}, iso, true)
	return slotHolder{Record: rec, Isolated: ok}, ok, nil
}

// resolveHelperLocked is the body of resolveHelper. The caller must hold slotMu.
//
// Splitting it out is what keeps the whole decision on one registry read per
// kind. Every branch below — reuse, reclaim, create — is chosen from a snapshot
// taken inside this lock hold and acts while the lock still holds it, so no
// concurrent caller in this process can claim the slot between the read that
// found it free and the write that takes it.
//
// Each branch also states the owner it is handing back, and each knows it for
// certain: reuse carries the record it chose from, and a reclaimed pane and a
// pane we made are both ownerAgent. See resolveHelper for what the
// value is for.
//
// # No window is not a failure here
//
// errNoWindow means this server is not running inside tmux, and an ISOLATED slot
// needs no window: it lives on a second server with no client attached. So the
// sentinel is tolerated, the visible registry read is SKIPPED entirely — calling
// Records with an empty self would run `list-panes -t ""` and fail — and only a
// request that has to end in a visible pane turns it into a message. Any other
// failure of Self refuses everything: we could not establish which pane is ours,
// and every self-exclusion below is then unevaluable.
func (s *slots) resolveHelperLocked(ctx context.Context, slot int, kind kindRequest) (paneTarget, error) {
	self, err := s.b.Self(ctx)
	noWindow := errors.Is(err, errNoWindow)
	if err != nil && !noWindow {
		return paneTarget{}, err
	}

	var reg map[paneRef]paneRecord
	if !noWindow {
		reg, err = s.b.Records(ctx, self)
		if err != nil {
			return paneTarget{}, fmt.Errorf("could not read the pane registry for slot %d: %w", slot, err)
		}
	}

	// Reuse, visible. It runs first for an unstated kind because a visible pane
	// is the one the user can see, and because finding one here saves the second
	// registry read below on the overwhelmingly common path.
	//
	// A holder marked ownerAcquired is not reused. That mark means the pane was
	// opened by the user and taken over by an older version of this server, and
	// a pane the user opened is not one we type into: whatever runs in it now
	// may be theirs. The record is dropped instead — marks and label, with no
	// C-c, because the process there may be the user's — and the slot is
	// resolved as if it were empty, so the agent gets a new pane and is told so.
	if kind != kindIsolated {
		if rec, ok := s.slotCandidateLocked(ctx, slot, self, reg, false); ok {
			if rec.Owner != ownerAcquired {
				return s.helperResult(rec.Ref, self, slot, false, rec.Owner, false)
			}
			s.disownLocked(ctx, rec)
			delete(reg, rec.Ref)
		}
	}

	// Reuse, isolated. Self-exclusion is vacuous on that socket — this server
	// does not run there — and is not special-cased, for the same reason
	// helperResult's self check is not: a guard that costs one comparison and
	// cannot be wrong is cheaper than a branch explaining why it was removed.
	var iso map[paneRef]paneRecord
	if kind != kindVisible {
		iso, err = s.b.IsolatedRecords(ctx)
		if err != nil {
			return paneTarget{}, fmt.Errorf("could not read the pane registry for slot %d: %w", slot, err)
		}
		if rec, ok := s.slotCandidateLocked(ctx, slot, paneRef{}, iso, true); ok {
			return s.helperResult(rec.Ref, self, slot, false, rec.Owner, true)
		}
	}

	// A slot is ONE pane. An EXPLICIT kind that disagrees with what the slot
	// already holds is refused rather than satisfied with a second pane, because
	// two panes answering to one number make the next call ambiguous. A DEAD
	// holder counts: it is still on the user's screen under remain-on-exit, and
	// close-pane({slot:N}) is how it is reaped.
	//
	// An unstated kind reaches neither refusal, which is the whole point of the
	// tri-state: it accepts whichever kind owns the slot, and both reuse branches
	// above have already had their chance to say so.
	if kind == kindIsolated && slotOccupied(reg, slot) {
		return paneTarget{}, fmt.Errorf(kindIsVisibleText, slot)
	}
	if kind == kindVisible {
		iso, err = s.b.IsolatedRecords(ctx)
		if err != nil {
			return paneTarget{}, fmt.Errorf("could not read the pane registry for slot %d: %w", slot, err)
		}
		if slotOccupied(iso, slot) {
			return paneTarget{}, fmt.Errorf(kindIsIsolatedText, slot)
		}
	}

	if kind == kindIsolated {
		return s.createIsolatedLocked(ctx, slot, self)
	}
	if noWindow {
		return paneTarget{}, errNoWindow
	}

	// Reclaim. A pane this server family opened and no slot points at any more
	// is ours and otherwise unreachable, so it is used before anything new is
	// opened. Nothing unmarked is ever a candidate: a pane without our marks is
	// the user's, however idle it looks. See reclaimPaneLocked.
	if pane, ok := s.reclaimPaneLocked(ctx, reg, slot, self); ok {
		if err := s.resetReclaimed(ctx, pane); err != nil {
			s.unslotReclaimed(ctx, pane)
			return paneTarget{}, fmt.Errorf("slot %d: %w", slot, err)
		}
		return s.helperResult(pane, self, slot, true, ownerAgent, false)
	}

	// Create. The claim is one write of all three marks, and a failed one is
	// not fatal while the pane still carries the agent-owned mark the backend
	// writes when it opens it: that pane is an unslotted agent pane, and the
	// next resolution reclaims it rather than opening another. A pane carrying
	// no record at all is different. Nothing tells it from one the user opened —
	// no reclaim takes it and no close reaches it — so it is closed, on a
	// detached context for the reason createIsolatedLocked gives, and the
	// failure is reported.
	place := s.placementForSlot(ctx, reg, slot, self)
	pane, err := s.b.OpenBeside(ctx, place)
	if err != nil {
		return paneTarget{}, fmt.Errorf("could not open a pane for slot %d: %w", slot, err)
	}
	if err := s.b.Claim(ctx, pane, ownerAgent, slot); err != nil && !s.carriesRecord(ctx, self, pane) {
		s.closeUnused(ctx, pane)
		return paneTarget{}, fmt.Errorf("could not open a pane for slot %d: %w", slot, err)
	}
	_ = s.b.SetTitle(ctx, pane, helperTitle(slot))
	if err := s.waitForShellReady(ctx, pane, newPaneReadyTimeout); err != nil {
		s.closeUnused(ctx, pane)
		return paneTarget{}, fmt.Errorf("slot %d: %w", slot, err)
	}
	return s.helperResult(pane, self, slot, true, ownerAgent, false)
}

// closeUnused closes a pane this call opened and is not handing out: one that
// carries no record, or whose shell never became ready. Kept, the second kind
// would hold the slot, and the next call would reuse it without waiting and
// type into an rc that discards what it is sent.
//
// It runs on a context detached from the caller's, because the likeliest
// reason to be here is that the caller's context has ended, and a close issued
// on a dead context does not run.
func (s *slots) closeUnused(ctx context.Context, pane paneRef) {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), partialMarkUndoTimeout)
	defer cancel()
	_ = s.b.Close(undoCtx, pane)
}

// unslotReclaimed gives a reclaimed pane whose reset failed back its unslotted
// state, on a detached context for the reason closeUnused gives. The pane was
// not handed out, so it must not answer to the slot; it stays ours and can be
// reclaimed again once it is reading.
func (s *slots) unslotReclaimed(ctx context.Context, pane paneRef) {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), partialMarkUndoTimeout)
	defer cancel()
	_ = s.b.SetSlot(undoCtx, pane, 0)
}

// slotOccupied reports whether any record in a registry claims the slot, dead or
// alive. It is the kind-conflict question, which is deliberately broader than
// the reuse question: a corpse still holds the number, still sits on the user's
// screen under remain-on-exit, and is still what close-pane({slot:N}) reaps.
func slotOccupied(reg map[paneRef]paneRecord, slot int) bool {
	for _, rec := range reg {
		if rec.Slot == slot {
			return true
		}
	}
	return false
}

// createIsolatedLocked opens a pane nobody can see and claims it. The caller
// must hold slotMu.
//
// # There is no reclaim branch
//
// Every pane on this socket was created by an agent, and an unslotted one in
// our namespace is either a one-shot execute-command still running or a pane a
// dead process never finished claiming. Taking the first would type into a
// command in flight, and the second is reaped by close-pane({slot:"all"}). A
// predicate that could take panes here would also be one that could type into
// ANOTHER SERVER'S pane, with the namespace filter as the only thing preventing
// it — and it must not become the only thing.
//
// # A failed claim CLOSES the pane, where the visible path shrugs
//
// The visible create path treats a failed marker as cosmetic: the pane exists,
// the user can see it, and at worst they close it by hand. Here nobody can see
// it. An unclaimed isolated pane is a live shell that no tool can list, reach or
// reap for the lifetime of the machine, so the pane is destroyed and the failure
// reported.
//
// The cleanup runs on a context DETACHED from the caller's, and that is the
// point rather than a detail: the likeliest reason a mark write failed is that
// the caller's context expired, and a kill issued on that same dead context is a
// command that does not run — leaving exactly the stranded shell this branch
// exists to prevent.
//
// waitForShellReady is not optional here. OpenIsolated takes no initial command,
// so whatever the caller does next sends into a shell that may still be sourcing
// its rc files — the failure that wait exists to prevent.
func (s *slots) createIsolatedLocked(ctx context.Context, slot int, self paneRef) (paneTarget, error) {
	pane, err := s.b.OpenIsolated(ctx)
	if err != nil {
		return paneTarget{}, fmt.Errorf("could not open a pane for slot %d: %w", slot, err)
	}
	if err := s.b.Claim(ctx, pane, ownerAgent, slot); err != nil {
		s.closeUnused(ctx, pane)
		return paneTarget{}, fmt.Errorf("could not open a pane for slot %d: %w", slot, err)
	}
	_ = s.b.SetTitle(ctx, pane, helperTitle(slot))
	if err := s.waitForShellReady(ctx, pane, newPaneReadyTimeout); err != nil {
		s.closeUnused(ctx, pane)
		return paneTarget{}, fmt.Errorf("slot %d: %w", slot, err)
	}
	return s.helperResult(pane, self, slot, true, ownerAgent, true)
}

// helperResult is the single exit through which every resolution passes, and it
// exists for the one check it performs.
//
// Returning the server's own pane would mean the agent typing into its own
// session — the failure this entire design exists to prevent. It should be
// unreachable: the self-exclusion in slotCandidateLocked drops self from reuse,
// reclaimPaneLocked skips it, and a split never returns the pane it split. The
// check is written anyway because it costs one string comparison, and because
// the failure it guards against is the one failure that cannot be diagnosed
// from outside the process: the agent's own transcript would fill with
// its own keystrokes and there would be nothing left to read the diagnosis from.
//
// It is also the single exit the owner passes through, which is what makes
// "every resolution reports whose pane it handed back" a property of one
// function rather than a habit four branches share. A refusal reports no owner
// at all: there is no pane, so there is nothing to be the owner of, and an empty
// value is the one clearForDisplay treats as "do not touch the line".
func (s *slots) helperResult(
	pane, self paneRef, slot int, created bool, owner string, isolated bool,
) (paneTarget, error) {
	if pane == self {
		// The slot number is the whole handle: the pane is not named here, because
		// this sentence reaches the model and a pane id in it is a pane id in the
		// model's context. (paneRef's String prints "<pane>" as a second line of
		// defence, so even a careless %s here could not leak one.)
		return paneTarget{}, fmt.Errorf(
			"internal error: slot %d resolved to this server's own pane; refusing to return it", slot)
	}
	return paneTarget{Ref: pane, Slot: slot, Created: created, Owner: owner, Isolated: isolated}, nil
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
// resolveHelper). The lowest-numbered — oldest — pane wins, and the losers are
// retired. Keeping the oldest is the choice that preserves whatever long-running
// process the caller already started.
//
// isolated says which registry this is, and it is a parameter rather than a fact
// read off the record because it changes what retiring a loser MEANS — see
// retireDuplicateLocked, where the two rules are deliberately in one function so
// neither can be edited without seeing the other. On the isolated registry
// self-exclusion is vacuous: this server does not run on that socket, so no
// record there can be self.
func (s *slots) slotCandidateLocked(
	ctx context.Context, slot int, self paneRef, reg map[paneRef]paneRecord, isolated bool,
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
		if !self.empty() && rec.Ref == self {
			_ = s.b.SetSlot(ctx, rec.Ref, 0)
			continue
		}
		candidates = append(candidates, rec)
	}
	if len(candidates) == 0 {
		return paneRecord{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Seq < candidates[j].Seq
	})
	for _, dup := range candidates[1:] {
		s.retireDuplicateLocked(ctx, dup, isolated)
	}
	return candidates[0], true
}

// retireDuplicateLocked takes the slot away from the loser of a duplicate-slot
// race, and for a pane under an ownerAcquired record it does considerably more
// than that. The caller must hold slotMu.
//
// An ownerAcquired record names a pane the USER opened, which an older version
// of this server took over as a slot. Merely unslotting such a loser strands it
// in the exact state clearPaneRegistration's comment says must never be
// produced: invisible to close-pane({slot:"all"}), which covers slotted records
// only; never a slot again, because only ownerAgent panes are reclaimed; and
// still wearing the title "agent". To the user that is an ordinary shell with a
// stale label, and no tool in this server lists it, so nothing will ever come
// back for it. An acquired loser is therefore RELEASED — the same C-c, the same
// three markers, the same label removal close-pane would perform — because a
// claim we have just decided not to use is a claim we must drop.
//
// An ownerAgent loser keeps the older, lighter treatment: the slot marker goes
// and the pane stays. It is a pane this server created, so nothing of the user's
// is trapped inside it, and it may well be running the long process whose
// survival is the whole reason the OLDEST pane wins this race — killing it here,
// from inside a resolution nobody asked to destroy anything, would throw that
// away. It becomes an unslotted agent pane the user can close by hand, and that
// is the documented cost of the cross-process race this healing repairs.
//
// # The isolated rule is the opposite, and the difference is visibility
//
// An unslotted loser on the isolated socket is not a pane the user can close by
// hand: nobody can see it, no tool can reach it, and it pins a live shell and
// the server behind it until the machine reboots. So an isolated loser is
// CLOSED. The two rules live in one function on purpose — the visible rule reads
// as the safe, conservative one, and applying it to a pane nobody can see is
// exactly the mistake that produced the stranded shells this branch exists to
// prevent.
func (s *slots) retireDuplicateLocked(ctx context.Context, rec paneRecord, isolated bool) {
	if isolated {
		_ = s.b.Close(ctx, rec.Ref)
		return
	}
	if rec.Owner == ownerAcquired {
		_ = s.releaseAcquiredLocked(ctx, rec)
		return
	}
	_ = s.b.SetSlot(ctx, rec.Ref, 0)
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
func (s *slots) closeCandidateLocked(
	ctx context.Context, slot int, self paneRef, reg map[paneRef]paneRecord, isolated bool,
) (paneRecord, bool) {
	if rec, ok := s.slotCandidateLocked(ctx, slot, self, reg, isolated); ok {
		return rec, true
	}
	var dead []paneRecord
	for _, rec := range reg {
		if rec.Slot != slot || !rec.Dead || (!self.empty() && rec.Ref == self) {
			continue
		}
		dead = append(dead, rec)
	}
	if len(dead) == 0 {
		return paneRecord{}, false
	}
	sort.Slice(dead, func(i, j int) bool {
		return dead[i].Seq < dead[j].Seq
	})
	return dead[0], true
}

// reclaimPaneLocked gives a slot a pane this server family opened and nothing
// points at any more. The caller must hold slotMu.
//
// # Why such panes exist
//
// An agent-owned pane with no slot marker is reachable by no request: reuse
// matches on the slot number, and nothing else looks at agent-owned panes. Three
// things leave panes in that state — a v1 split-pane, which marked its pane with
// no slot; the loser of a duplicate-slot race, which keeps its pane and loses
// its number (retireDuplicateLocked); and a create whose slot write failed.
// Without this step every later request opens one more pane beside it, and the
// window fills with idle helpers nobody can address.
//
// # What may be taken
//
// Only records from the witnessed registry, so a pane carrying none of our
// marks — the user's shell, a pane restored by tmux-resurrect, a released
// helper — is never a candidate, however idle it looks. Among ours: owner
// "agent" (an ownerAcquired record names a pane the user opened), no slot,
// alive, not this server's own pane, the shell provably reading at a prompt
// with nothing queued (paneIsReclaimable), and the same user as this server. A
// pane running anything — a dev server, an editor, cat, a loop of builtins — is
// left alone, because typing into it would feed that program; so is one still
// sourcing its rc, because the line editor is not reading yet.
//
// Panes are considered oldest first, so the choice is deterministic. A pane
// whose slot write fails is skipped rather than returned: a pane handed back
// without its slot would be found by nothing on the next call.
//
// The reclaimed pane is reported as created, because it is new to this slot:
// the agent must not assume that a process of its own is running there. Its
// caller puts it back in this server's directory; see resetReclaimed.
func (s *slots) reclaimPaneLocked(
	ctx context.Context, reg map[paneRef]paneRecord, slot int, self paneRef,
) (paneRef, bool) {
	var candidates []paneRecord
	for _, rec := range reg {
		if rec.Owner != ownerAgent || rec.Slot != 0 || rec.Dead || rec.Ref.empty() || rec.Ref == self {
			continue
		}
		candidates = append(candidates, rec)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Seq < candidates[j].Seq })
	for _, rec := range candidates {
		state, err := s.b.Foreground(ctx, rec.Ref)
		if err != nil || !paneIsReclaimable(state) || !sameUIDAsAgent(state) {
			continue
		}
		if err := s.b.SetSlot(ctx, rec.Ref, slot); err != nil {
			continue
		}
		_ = s.b.SetTitle(ctx, rec.Ref, helperTitle(slot))
		return rec.Ref, true
	}
	return paneRef{}, false
}

// resetReclaimed puts a reclaimed pane back where a new pane would start: in
// this server's directory, with a clear screen.
//
// A reclaimed pane is reported as created, and the agent will treat it as one.
// The shell in it is not new, though: it was left by an earlier agent, which may
// have cd'd anywhere. The directory is the part that can be put right safely —
// one cd, typed at a prompt reclaim has already confirmed — so it is, and the
// screen is cleared the way write-to-display clears a pane we own. Exported
// variables, an activated virtualenv and shell options cannot be undone without
// knowing what set them, and stay as they were; the README says so.
func (s *slots) resetReclaimed(ctx context.Context, pane paneRef) error {
	if s.cwd == "" {
		return nil
	}
	if err := s.sendValidated(ctx, pane, "cd -- "+shellQuote(s.cwd), true, true); err != nil {
		return err
	}
	return s.clearForDisplay(ctx, paneTarget{Ref: pane, Owner: ownerAgent})
}

// shellQuote quotes a string as one word for any POSIX shell, and for fish:
// single quotes, with each embedded quote closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// carriesRecord reports whether a pane has a record of ours in the window
// registry. A failed read counts as no record.
func (s *slots) carriesRecord(ctx context.Context, self, pane paneRef) bool {
	reg, err := s.b.Records(ctx, self)
	if err != nil {
		return false
	}
	_, ok := reg[pane]
	return ok
}

// partialMarkUndoTimeout bounds a cleanup that runs on a context detached from
// the caller's. It is short because the cleanup is a handful of commands
// against a pane we have just been talking to, and because it runs under
// slotMu: a wedged multiplexer must not be able to hold the lock every other
// resolution is queued behind.
const partialMarkUndoTimeout = 2 * time.Second

// disownLocked drops this server's record from a pane the user opened, and
// leaves the pane and whatever runs in it exactly as they are. The caller must
// hold slotMu.
//
// It is what resolution does with a legacy ownerAcquired holder. Unlike
// releaseAcquiredLocked there is no C-c: close-pane is a request to stop using
// the pane, made by the agent that started something there, while this runs
// on the way to some unrelated request, and the process now in the pane may be
// the user's own. The cleanup runs on a detached context for the reason
// createIsolatedLocked gives, and its failures are ignored: the slot is
// resolved to a new pane either way, and a mark that survives is retried by the
// next resolution that meets it.
func (s *slots) disownLocked(ctx context.Context, rec paneRecord) {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), partialMarkUndoTimeout)
	defer cancel()
	_ = s.b.ClearMarks(undoCtx, rec.Ref)
	_ = s.b.SetTitle(undoCtx, rec.Ref, "")
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
// user's 501 and reclaim nothing at all, whereas comparing pane-to-pane would
// accept whatever identity the agent's pane happened to have.
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

// The server-decided geometry for a slot — which pane to split, which way, and
// how big — is the port's placement type. The agent never passes a direction,
// and this is where that decision lives instead.

// placementForSlot decides where a new helper pane goes.
//
//	slot 1  → split self beside: the helper lands right of the agent, which is
//	          where the user is already looking.
//	slot 2  → split the slot-1 pane below, so the two helpers stack in one
//	          column instead of squeezing the agent's own pane again. With no
//	          live slot-1 pane of ours it goes beside self instead: the spot
//	          slot 1 would have taken.
//	slot 3  → split self below: under the agent, in the agent's column.
//	slot ≥4 → split the largest helper we made, along its longer side.
//
// Every anchor in that table is self or a pane this server created. The owner
// check in case 2 is what makes that true rather than nearly true: a legacy
// ownerAcquired record in slot 1 names a pane the user opened, and halving it
// would rearrange a layout they built by hand.
//
// The table depends on nothing but which of our own helpers exist. That is the
// property that keeps the agent's pane at least half its height after slots 1
// to 3, and it is why the slot-2 fallback is "beside self" rather than "below
// self": below self is slot 3's place, and taking it there squeezes the agent
// twice.
//
// It deliberately does NOT create the missing anchor first. Resolving one slot
// creates at most one pane: creating slot 1 as a side effect of a slot-2 request
// would give the caller a pane it did not ask for and cannot name, and would
// make created ambiguous — the flag means "the pane for *this* slot is new", and
// it cannot mean that if the call also produced a second pane for another slot.
//
// It returns no error. Every failure inside degrades to the self anchor, which
// cannot be missing: resolveHelperLocked has already failed with errNoWindow if
// there were no self pane. Letting a cosmetic geometry decision fail a
// resolution that is otherwise able to proceed would trade a slightly worse
// layout for no pane at all.
func (s *slots) placementForSlot(
	ctx context.Context, reg map[paneRef]paneRecord, slot int, self paneRef,
) placement {
	switch slot {
	case 1:
		return placement{Anchor: self, Axis: splitBeside, SizePercent: 50}
	case 2:
		if rec, ok := aliveSlotPane(reg, slotDefault); ok && rec.Owner == ownerAgent && rec.Ref != self {
			return placement{Anchor: rec.Ref, Axis: splitBelow, SizePercent: 50}
		}
		return placement{Anchor: self, Axis: splitBeside, SizePercent: 50}
	case 3:
		return placement{Anchor: self, Axis: splitBelow, SizePercent: 50}
	default:
		helper, ok := s.largestHelper(ctx, reg, self)
		if !ok {
			return placement{Anchor: self, Axis: splitBeside, SizePercent: 50}
		}
		return placement{Anchor: helper.Ref, Axis: longerSide(helper), SizePercent: 50}
	}
}

// longerSide is the axis that halves a pane's longer visual side. A cell is
// about twice as tall as it is wide, so a pane is visually wider than tall when
// its width in cells exceeds twice its height.
func longerSide(p paneInfo) splitAxis {
	if p.Width > 2*p.Height {
		return splitBeside
	}
	return splitBelow
}

// aliveSlotPane returns the live pane holding the given slot, if any.
//
// It matches on slot and liveness and nothing else, and returns the whole record
// rather than an id so that its caller can apply the ownership question itself.
// "Which pane holds this slot" and "may we split it" are two questions, and
// answering the second one in here would hide it from the one place — placement
// — where it decides whether the user's layout gets rearranged.
func aliveSlotPane(reg map[paneRef]paneRecord, slot int) (paneRecord, bool) {
	for _, rec := range reg {
		if rec.Slot == slot && !rec.Dead {
			return rec, true
		}
	}
	return paneRecord{}, false
}

// largestHelper returns the agent-owned pane with the most room to give up.
//
// Panes are ranked by AREA — width × height. Area rather than either dimension
// alone is the right metric because a split halves one dimension, and the pane
// that can lose half of one and stay usable is the one with the most cells to
// start with. Ties go to the oldest pane purely so the choice is deterministic
// under test.
//
// Only panes this server made qualify. A legacy acquired pane is the user's,
// and halving it rearranges a layout the user built by hand; the agent's own
// pane is not a helper.
func (s *slots) largestHelper(
	ctx context.Context, reg map[paneRef]paneRecord, self paneRef,
) (paneInfo, bool) {
	panes, err := s.b.Siblings(ctx, self)
	if err != nil {
		return paneInfo{}, false
	}
	var best paneInfo
	found := false
	for _, p := range panes {
		rec, ok := reg[p.Ref]
		if !ok || rec.Owner != ownerAgent || rec.Dead || p.Ref == self {
			continue
		}
		area, bestArea := p.Width*p.Height, best.Width*best.Height
		if !found || area > bestArea || (area == bestArea && p.Seq < best.Seq) {
			best, found = p, true
		}
	}
	return best, found
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

// The self refusal used to be a sentinel error, errCloseSelfPane, because two
// call shapes needed the same refusal in different packaging: the explicit
// close-pane({paneId}) form raised it as the whole answer, while a batch
// teardown recorded it as one entry and carried on. There is one call shape now
// — every close is by slot — so the refusal is one entry with action:"error",
// and a sentinel nothing tests for is a sentinel that rots.

// closeHelperLocked releases one helper pane according to who owns it. The
// caller must hold slotMu and must pass the self pane it read inside that hold.
//
// The distinction is the entire reason the owner field carries a kind. Killing a
// pane the server created is a clean undo: nothing existed before we made it and
// nothing survives that the user wanted. Killing a pane the USER opened is the
// one unrecoverable action available anywhere in this design — whatever was in
// its scrollback, whatever they had half-typed, whatever ssh session it held, is
// gone with no way back. Releasing it — C-c and the marks removed — is not in
// that category: it interrupts at most what the agent left running there, and
// hands the pane back as an ordinary shell.
//
// # Why the self guard is here and not in the caller
//
// Every OTHER path already excludes this server's own pane — slotCandidateLocked
// drops it from reuse, reclaimPaneLocked skips it,
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
// read once per call, from the port, under the lock — and a future teardown path
// cannot compile without deciding what to pass, which is the compiler asking
// whether it has thought about this. An empty self means the server is not
// running in tmux at all, so there is no own pane to protect and the guard is
// correctly vacuous; closePanes is where that case is established.
//
// Both refusals below name the SLOT and nothing else. They reach the caller
// verbatim through closedPane.Detail, so an id in either would be an id in the
// model's context — and the previous text also pointed the agent at kill-pane,
// a tool this release deletes. An error that names a tool which does not exist
// is the same failure as an error that says only "no": it sends the agent
// looking for another route, and the route it finds is raw tmux.
func (s *slots) closeHelperLocked(ctx context.Context, rec paneRecord, self paneRef) (string, error) {
	if !self.empty() && rec.Ref == self {
		return actionError, fmt.Errorf(
			"slot %d is the pane this server is running in, and closing it would kill the session "+
				"this request arrived through. A pane created by an outer agent carries a perfectly "+
				"valid agent-owned record, so the record is not what makes a pane safe to close.",
			rec.Slot)
	}
	switch rec.Owner {
	case ownerAgent:
		if err := s.b.Close(ctx, rec.Ref); err != nil {
			return actionError, err
		}
		return actionKilled, nil
	case ownerAcquired:
		if err := s.releaseAcquiredLocked(ctx, rec); err != nil {
			return actionError, err
		}
		return actionReleased, nil
	default:
		// Unreachable: every caller checks for a record first, and a record with
		// an unrecognised owner never leaves the registry readers.
		return actionNone, fmt.Errorf("slot %d is held by a pane this binary does not manage", rec.Slot)
	}
}

// releaseAcquiredLocked hands a pane under an ownerAcquired record back to the
// user: interrupt
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
func (s *slots) releaseAcquiredLocked(ctx context.Context, rec paneRecord) error {
	if !rec.Dead {
		if err := s.b.SendKeys(ctx, rec.Ref, "C-c", false, false); err != nil {
			return err
		}
	}
	if err := s.b.ClearMarks(ctx, rec.Ref); err != nil {
		return err
	}
	_ = s.b.SetTitle(ctx, rec.Ref, "")
	return nil
}

// closeSelector names what close-pane was asked to close: every slot, or one of
// them (1 when the caller named nothing at all).
//
// The third form — an explicit pane the caller named — is gone with paneId, and
// what went with it is the refusal that made it safe: a pane carrying no
// registry record was REFUSED rather than killed, because killing it would have
// made close-pane a second kill-pane with a friendlier name and a wider blast
// radius. An agent reaching for "close the pane I am finished with" would
// eventually have pointed it at one of the user's. That whole class of mistake
// is now unreachable rather than guarded: a caller cannot name a pane, so it
// cannot name the wrong one.
type closeSelector struct {
	All  bool
	Slot int
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
// resolves to that same pane and types into it mid-release. That interleaving
// the lock does eliminate: the read and the mutations are one hold.
//
// The MIRROR ordering it does not. A close that kills a pane between a
// resolver's RETURN and the handler's SendKeys still lands the keystrokes in a
// pane that no longer exists (or, under remain-on-exit, in one that swallows
// them silently) — resolveSlot releases slotMu before the handler sends, so the
// window is narrowed from "the whole of resolution" to "return to send", not
// closed. Saying otherwise here would be the more dangerous kind of comment: one
// that stops the next reader looking.
//
// The stronger fix, if the residual window ever matters, is the one
// clearForDisplay already uses: carry the owner captured under the lock and
// re-check the mark before acting, so a send into a pane that was released
// underneath it refuses instead of typing. It is not done here because it
// touches every sending handler, and the exposure needs a concurrent close and
// send on the SAME slot from one agent's own worker pool.
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
func (s *slots) closePanes(ctx context.Context, sel closeSelector) ([]closedPane, error) {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()

	// One read of "which pane am I", used for the guard in closeHelperLocked and
	// as the scope the visible half is resolved around.
	//
	// No window SKIPS the visible half rather than refusing the call. There is no
	// window to enumerate and no visible slot to resolve — Records with an empty
	// self would run `list-panes -t ""` and fail — but a server that was never
	// inside tmux still has isolated panes of its own to close, and those are the
	// only panes it can have. Refusing here would make the one mode that works
	// outside tmux impossible to tidy up after.
	//
	// Any OTHER failure of Self refuses every form. It means we could not
	// establish which pane is ours, and the self guard in closeHelperLocked is
	// the only thing standing between close-pane and the agent's own session: a
	// guard that cannot be evaluated must not be skipped.
	self, err := s.b.Self(ctx)
	if err != nil && !errors.Is(err, errNoWindow) {
		return nil, visibleError(err)
	}

	switch {
	case sel.All:
		var closed []closedPane
		if !self.empty() {
			recs, err := s.visibleSlotsLocked(ctx, self)
			if err != nil {
				return nil, err
			}
			for _, rec := range recs {
				// A failure on one pane does not abort the loop. Teardown that
				// stops at the first problem leaves the caller worse off than one
				// that continues and reports: the panes after the failure would
				// stay open with no indication that they had not been considered.
				closed = append(closed, s.closeOneLocked(ctx, rec, self))
			}
		}
		// A failed sweep is an ENTRY, not a returned error, for the same reason
		// the loop above does not stop at its first failure — and for one more:
		// returning (nil, err) here throws away the visible panes this call has
		// ALREADY closed, so the agent is told nothing about work that really
		// happened. It also cannot be silence. The namespace read no longer
		// reports an unreadable socket as an empty one (see IsolatedPanes), so
		// the one answer left to give is "I could not look", said out loud.
		swept, err := s.sweepNamespaceLocked(ctx)
		closed = append(closed, swept...)
		if err != nil {
			closed = append(closed, closedPane{
				Slot:   0,
				Action: actionError,
				Detail: "could not read the isolated panes, so any that exist are still " +
					"open: " + err.Error(),
			})
		}
		if closed == nil {
			// A bare array, never null. "There was nothing to close" is a
			// perfectly ordinary answer, and a caller that gets null for it has
			// to special-case the value before iterating — which some will not.
			return []closedPane{}, nil
		}
		return closed, nil
	default:
		holder, found, err := s.lookupSlotLocked(ctx, sel.Slot, kindUnstated, self)
		if err != nil {
			return nil, err
		}
		if !found {
			// A slot that holds nothing is not an error. "Close slot 2" when slot
			// 2 was never opened is a request that has already been satisfied.
			// This is the ONE place the two callers of that lookup differ, and it
			// is the caller's difference: a reading tool errors here.
			return []closedPane{{Slot: sel.Slot, Action: actionNone}}, nil
		}
		return []closedPane{s.closeOneLocked(ctx, holder.Record, self)}, nil
	}
}

// sweepNamespaceLocked closes EVERY pane in this server's isolated namespace,
// slotted or not. The caller must hold slotMu.
//
// It is namespace-scoped where the visible half is slot-scoped, and the
// asymmetry follows from who can own a pane there: every pane in our namespace
// was created by this process, so a pane whose marks are
// missing or partial is still ours to close. In the user's window the opposite
// holds — an unmarked pane there is presumed the user's, and killing it is the
// one unrecoverable action in this design.
//
// Without this, a process that died between OpenIsolated and Claim leaves a live
// shell that nothing can list, reach or reap. It cannot be closed by hand
// either, because there is no window to close it in.
//
// No kill-server is issued, here or anywhere. tmux exits a server when its last
// pane dies, so closing our panes reclaims the server by itself — and a
// kill-server on this socket would be a cross-namespace kill, taking the panes
// of every other agent on the machine with it.
//
// The pane list is the authority and its failure is FATAL to the sweep: a
// namespace we could not read is not an empty one, and answering "nothing to
// close" for it is the one lie this function must never tell. The registry read
// that follows is decoration by comparison — it supplies slot numbers — and its
// own failure degrades to "empty" inside the port, which costs an accurate slot
// number in the report and closes every pane regardless.
func (s *slots) sweepNamespaceLocked(ctx context.Context) ([]closedPane, error) {
	panes, err := s.b.IsolatedPanes(ctx)
	if err != nil {
		return nil, err
	}
	if len(panes) == 0 {
		return nil, nil
	}
	records, err := s.b.IsolatedRecords(ctx)
	if err != nil {
		return nil, err
	}

	closed := make([]closedPane, 0, len(panes))
	for _, pane := range panes {
		rec, claimed := records[pane]
		entry := closedPane{Slot: rec.Slot, Action: actionKilled}
		if !claimed || rec.Slot < slotDefault {
			// Slot 0 is not a slot, and saying so is better than inventing one:
			// no number ever named this pane. The detail is what stops that entry
			// reading as a bug — and it names BOTH ways a pane gets here, because
			// the second one is not a fault at all: an execute-command running
			// isolated with no slot deliberately never claims its pane, so a
			// concurrent close-pane({slot:"all"}) sweeps it mid-command. Calling
			// that "never finished claiming" reported a deliberate design as a
			// crash. See runOneShot.
			entry.Slot = 0
			entry.Detail = "an isolated pane this server opened and did not claim: a one-shot " +
				"execute-command still running, or a pane left behind by a process that died " +
				"before it could claim one"
		}
		if err := s.b.Close(ctx, pane); err != nil {
			entry.Action = actionError
			entry.Detail = err.Error()
		}
		closed = append(closed, entry)
	}
	sort.Slice(closed, func(i, j int) bool { return closed[i].Slot < closed[j].Slot })
	return closed, nil
}

// closeOneLocked turns one record into one response entry. The caller must hold
// slotMu.
//
// It reports no error to its caller, and that is the whole teardown contract in
// one line: a refusal or a failure is an ENTRY, with action:"error" and the
// reason in detail, so a sweep of five slots reports five answers rather than
// stopping at the first. The explicit-paneId form used to need the error as
// well — a caller that named one pane and got back an array could read even an
// error entry as "teardown ran" — and it went with paneId.
func (s *slots) closeOneLocked(ctx context.Context, rec paneRecord, self paneRef) closedPane {
	action, err := s.closeHelperLocked(ctx, rec, self)
	entry := closedPane{Slot: rec.Slot, Action: action}
	if err != nil {
		entry.Action = actionError
		entry.Detail = err.Error()
	}
	return entry
}

// visibleSlotsLocked returns every slotted helper in the window this server runs
// in, ordered by slot and then by pane id. It is the visible half of
// close-pane({slot:"all"}); sweepNamespaceLocked is the other. The caller must
// hold slotMu, and passes the self it read from the port inside that hold.
//
// Three scoping decisions live here, and none of them is arbitrary.
//
// It is WINDOW-scoped, because resolution is: "all" undoes what the resolver
// did, and the resolver never reaches outside this window. An agent tearing down
// must not be able to touch the user's other windows.
//
// It covers SLOTTED records only, which is the one scoping rule the isolated
// sweep deliberately inverts. An unslotted agent-owned pane in the USER'S window
// is one they can see and close by hand — most often a duplicate-race loser this
// server unslotted rather than killed — and close-pane is the inverse of
// resolveHelper, so it undoes exactly what resolveHelper did. On the isolated
// socket nobody can see anything, so the sweep there takes every pane in the
// namespace; see sweepNamespaceLocked.
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
func (s *slots) visibleSlotsLocked(
	ctx context.Context, self paneRef,
) ([]paneRecord, error) {
	reg, err := s.b.Records(ctx, self)
	if err != nil {
		return nil, fmt.Errorf("read the pane registry: %w", err)
	}
	var recs []paneRecord
	for _, rec := range reg {
		if rec.Slot < slotDefault || rec.Ref == self {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Slot != recs[j].Slot {
			return recs[i].Slot < recs[j].Slot
		}
		return recs[i].Seq < recs[j].Seq
	})
	return recs, nil
}

// ---- The registry, as the caller sees it ----

// Origins, as reported by list-slots. They are WIRE words rather than the
// internal owner values, so the vocabulary teardown keys off — ownerAgent is
// killed, ownerAcquired is released — does not leak into a listing, where the
// only thing a caller needs to know is where the pane came from.
const (
	originCreated = "created"
	originAdopted = "adopted" // an ownerAcquired record, left by an older server
)

// listSlots answers list-slots, and takes slotMu because reading the registry
// here is mildly MUTATING. See listSlotsLocked.
func (s *slots) listSlots(ctx context.Context) ([]slotListing, error) {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()

	// No window skips the visible half, exactly as it does in closePanes and in
	// resolveHelper. A server outside tmux can still have isolated slots, and
	// they are the only slots it can have — answering "not running inside tmux"
	// there would make list-slots useless in precisely the mode isolated slots
	// exist for.
	self, err := s.b.Self(ctx)
	if err != nil && !errors.Is(err, errNoWindow) {
		return nil, visibleError(err)
	}
	return s.listSlotsLocked(ctx, self)
}

// listSlotsLocked returns one canonical entry per occupied slot, ascending. The
// caller must hold slotMu and pass the self it read inside that hold.
//
// Printing the registry as it is read would let this tool disagree with the
// resolver, which is worse than the tool not existing: two entries for one slot
// when two servers raced for it, and an entry for a slot whose only holder is
// this server's own pane — a record the next resolution would clear. An agent
// that then addressed that slot would be answered about a different pane than
// the one it had just been shown.
//
// So it runs the same two repairs resolution runs, through the same functions:
// self-exclusion clears a stale marker off our own pane, and duplicate healing
// keeps the oldest holder and retires the rest (releasing an acquired loser
// rather than stranding it). That makes a read mildly mutating, which is why
// list-slots carries no readOnlyHint.
//
// A slot holding only a dead pane is LISTED, with isAlive:false. The corpse is
// what the user is looking at and what close-pane exists to reap; answering
// "slot 2 is empty" would conflate a slot that was never opened with one whose
// process died, and those are the two facts a caller most needs to tell apart.
//
// Both registries are listed, and both entries survive in the degenerate case
// where one number is claimed on each side. Hiding one would tell the agent a
// kind of pane it still has is gone, and close-pane({slot:N}) would then find
// something the listing said was not there. isolated is what tells them apart,
// which is why that field carries no omitempty.
func (s *slots) listSlotsLocked(ctx context.Context, self paneRef) ([]slotListing, error) {
	var listings []slotListing

	if !self.empty() {
		reg, err := s.b.Records(ctx, self)
		if err != nil {
			return nil, fmt.Errorf("could not read the pane registry: %w", err)
		}
		listings = append(listings, s.canonicalListingsLocked(ctx, reg, self, false)...)
	}

	iso, err := s.b.IsolatedRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not read the pane registry: %w", err)
	}
	listings = append(listings, s.canonicalListingsLocked(ctx, iso, paneRef{}, true)...)

	sort.Slice(listings, func(i, j int) bool {
		if listings[i].Slot != listings[j].Slot {
			return listings[i].Slot < listings[j].Slot
		}
		return !listings[i].Isolated && listings[j].Isolated
	})
	if listings == nil {
		// A bare array, never null: a caller that gets null for "nothing open"
		// has to special-case it before iterating, and some will not.
		return []slotListing{}, nil
	}
	return listings, nil
}

// canonicalListingsLocked turns one registry into one entry per occupied slot,
// applying the same two repairs resolution applies. The caller must hold slotMu.
func (s *slots) canonicalListingsLocked(
	ctx context.Context, reg map[paneRef]paneRecord, self paneRef, isolated bool,
) []slotListing {
	// Every slot number the registry mentions, including any carried by our own
	// pane: those reach closeCandidateLocked precisely so the stale marker is
	// cleared rather than skipped and left to steer a later call.
	seen := map[int]bool{}
	numbers := make([]int, 0, len(reg))
	for _, rec := range reg {
		if rec.Slot < slotDefault || seen[rec.Slot] {
			continue
		}
		seen[rec.Slot] = true
		numbers = append(numbers, rec.Slot)
	}
	sort.Ints(numbers)

	listings := make([]slotListing, 0, len(numbers))
	for _, slot := range numbers {
		rec, ok := s.closeCandidateLocked(ctx, slot, self, reg, isolated)
		if !ok {
			continue
		}
		listings = append(listings, s.listingFor(ctx, rec, isolated))
	}
	return listings
}

// listingFor describes one slot for the caller.
//
// The record's own Dead flag is the fallback rather than the answer: it is a
// tmux fact about the pane, while isAlive is a question about the PROCESS, and
// the two differ under remain-on-exit — a pane that is very much present while
// what the caller started in it has exited. A failed process read leaves the
// record's answer standing, because "we could not look" must not be reported as
// "it is gone".
func (s *slots) listingFor(ctx context.Context, rec paneRecord, isolated bool) slotListing {
	entry := slotListing{Slot: rec.Slot, Isolated: isolated, Origin: originCreated, IsAlive: !rec.Dead}
	if rec.Owner == ownerAcquired {
		entry.Origin = originAdopted
	}
	if state, err := s.b.Foreground(ctx, rec.Ref); err == nil && state != nil {
		entry.ForegroundCmd = state.ForegroundCmd
		entry.IsAlive = state.IsAlive
	}
	return entry
}

// ---- Typing into a shell: readiness, and evidence that the input landed ----
//
// Everything below is written once, here, above the port, so every backend
// gets the same rules. A backend supplies the facts in PaneState — whose
// process is in the foreground, whether the tty is in canonical mode, whether
// that process is asleep reading the tty, how much is queued on it — and these
// functions decide what those facts permit.

// Timing for the readiness gate and the landing checks.
//
// newPaneReadyTimeout is how long a pane this call opened is given to finish
// its rc: a heavy interactive one (powerlevel10k, nvm, conda, a slow network
// mount) with room to spare. A shell slower than that fails the call with a
// message naming the slot, and the pane is closed rather than having keys
// typed into a terminal nobody reads. shellReadyTimeout is the shorter wait
// execute-command gives a pane it already holds, whose rc is long finished.
// opaqueBudget bounds the wait for a pane the probe cannot read. promptSettle
// and editorOffSettle are how long a screen must stay unchanged to count as a
// prompt when no positive answer exists; see settleFor. inputLandingTimeout is
// how long a line editor that is already reading is given to take what was
// sent: it consumes keys in microseconds, so two seconds only ever runs out
// when nothing is reading at all.
const (
	newPaneReadyTimeout = 30 * time.Second
	shellReadyTimeout   = 10 * time.Second
	opaqueBudget        = 3 * time.Second
	promptSettle        = 300 * time.Millisecond
	editorOffSettle     = 2 * time.Second
	inputLandingTimeout = 2 * time.Second
	readyPollInterval   = 50 * time.Millisecond
	readyStreak         = 2
)

// The ways typing into a shell can be refused. They name no pane; the handlers
// prefix the slot. Each says only what is provably true: errInputNotTaken is
// used only for a single line sent without Enter, which cannot have run.
var (
	errShellNotReady = errors.New("the shell did not become ready to read input, so nothing was typed")
	errInputNotTaken = errors.New("the shell did not read the keys that were sent, " +
		"so they were not run; nothing was submitted")
	errInputUnconfirmed = errors.New("could not confirm the shell read the input; " +
		"look at the pane before sending it again")
	errEnterNotTaken = errors.New("the shell did not read the Enter key, so the line it holds was not run")
)

// paneIsReadyShell reports whether a shell is at a prompt whose line editor is
// reading: the shell in the foreground (paneIsIdleShell), its tty in
// non-canonical mode, and nothing typed and still unread.
//
// A shell with no line editor of its own never leaves canonical mode, so for
// those the mode is not required and only the empty queue is. That makes this
// a statement about what the pane looks like, not proof it is reading: dash
// running a loop of builtins looks the same. paneIsReclaimable is the proof.
func paneIsReadyShell(s *PaneState) bool {
	if !paneIsIdleShell(s) || !s.InputKnown || s.PendingInput != 0 {
		return false
	}
	return s.LineEditing || !hasLineEditor(s.ForegroundCmd)
}

// paneIsReclaimable is reclaim's predicate, and it takes only a positive
// answer: paneIsReadyShell, and the kernel saying the shell is reading — a line
// editor holding the tty out of canonical mode, or the shell asleep in a read
// on it where the platform reports that (Linux). A canonical-mode shell gives
// no such answer on macOS, so there it is never reclaimed; a new pane is opened
// instead, which costs a pane and never types into a busy one.
func paneIsReclaimable(s *PaneState) bool {
	return paneIsReadyShell(s) && (s.LineEditing || s.ReadingTTY)
}

// hasLineEditor reports whether a shell reads its prompt through a line editor
// that switches the tty out of canonical mode. dash, plain sh, ksh and csh read
// lines through the kernel's own canonical mode.
func hasLineEditor(cmd string) bool {
	switch strings.TrimPrefix(cmd, "-") {
	case "zsh", "bash", "fish", "tcsh":
		return true
	}
	return false
}

// promptReading is what one sample of a pane says about whether it is reading.
type promptReading int

const (
	// promptNotYet: something else owns the tty, input is queued, or the
	// pane's shell is running rather than waiting — sourcing its rc.
	promptNotYet promptReading = iota
	// promptEditing: a line editor has the tty in non-canonical mode.
	promptEditing
	// promptCanonical: the kernel reports the pane's own process asleep in a
	// read on its tty (Linux shows this; macOS does not).
	promptCanonical
	// promptQuiet: a shell that reads its prompt in canonical mode — one with
	// no line editor, or one whose command line switched its editor off — is in
	// the foreground, asleep, with nothing queued. That is a prompt, or an rc
	// waiting on a timer, and only a settled screen tells them apart; see
	// waitForShellReady. A shell whose editor is on is never quiet: asleep in
	// canonical mode, it is still in its rc, however still its screen is.
	promptQuiet
	// promptOpaque: the pane's own process is in the foreground, but the probe
	// cannot give a positive answer about it: the platform has no probe, the
	// probe failed, or the program is not a shell the probe knows.
	promptOpaque
)

// readPrompt classifies one sample.
//
// The two positive answers are facts the kernel reports, not inferences from a
// name. A line editor clears ICANON for as long as it waits at the prompt; a
// canonical reader sleeps in the tty's read routine, where the platform says
// so. A shell sourcing its rc is neither: it is running, or asleep somewhere
// else (a timer, select, a child).
func readPrompt(s *PaneState) promptReading {
	if s == nil || !s.IsAlive || s.PanePID <= 0 || s.ForegroundPID != s.PanePID {
		return promptNotYet
	}
	if !ttyProbeSupported || !s.InputKnown {
		return promptOpaque
	}
	switch {
	case s.PendingInput != 0:
		return promptNotYet
	case s.LineEditing:
		return promptEditing
	case s.ReadingTTY:
		return promptCanonical
	case !isShellProcess(s.ForegroundCmd):
		return promptOpaque
	case s.WaitingForInput && (!hasLineEditor(s.ForegroundCmd) || s.EditorOff):
		return promptQuiet
	}
	return promptNotYet
}

// atPrompt is the positive answer: a line editor or a canonical reader.
func atPrompt(r promptReading) bool { return r == promptEditing || r == promptCanonical }

// settleFor is how long a pane's screen must stay unchanged, with something on
// it, before a reading that has no positive answer counts as a prompt.
//
// A shell started with its line editor switched off is given the long settle,
// because its rc may be as heavy as anyone's. A shell with no line editor, or
// a program the probe does not know, is given the short one. An empty screen
// never settles at all.
func settleFor(r promptReading, cmd string) time.Duration {
	if r == promptQuiet && hasLineEditor(cmd) {
		return editorOffSettle
	}
	return promptSettle
}

// waitForShellReady blocks until a pane is reading input at a prompt, and fails
// when a shell it can read does not get there.
//
// The multiplexer returns a new pane as soon as it exists, long before the shell
// inside it has finished its rc files. Keys that arrive first queue on the tty,
// and an rc that drains typeahead discards them: the command looks sent, the
// pane looks fine, and nothing ran. So a pane this resolution has just opened or
// reclaimed is not handed to a caller until it is reading, and execute-command
// waits the same way before it types its command line.
//
// A positive answer has to hold on two polls in a row. A line editor flips the
// tty into non-canonical mode for good; an rc that reads a key or two (`read
// -k`) flips it for an instant, and a single sample can land in that instant.
//
// A shell whose line editor is on has only the positive answer. Its editor
// clears ICANON the moment it reads, so until then it is in its rc, and a
// screen that stopped changing — a banner, then a timer — proves nothing.
//
// A shell that reads in canonical mode has no positive answer on macOS, where
// a canonical read is invisible to the probe. That is a shell with no line
// editor (dash), or one whose command line switched it off (bash --noediting,
// zsh +Z). It counts as ready once nothing is queued and its screen, with
// something on it, has not changed for settleFor. A pane the probe cannot
// read at all — no probe on this platform, a probe that failed, a program it
// does not know — is used after opaqueBudget at the latest, as it was before
// the probe existed. Any other pane that is not reading by the end of budget,
// or when ctx ends, is an error.
//
// A pane that vanishes ends the wait at once: no amount of waiting brings it
// back.
func (s *slots) waitForShellReady(ctx context.Context, pane paneRef, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	streak := 0
	var opaqueSince, settledSince time.Time
	var lastScreen string
	for {
		state, err := s.b.Foreground(ctx, pane)
		if errors.Is(err, errPaneGone) {
			return errShellNotReady
		}
		reading := promptNotYet
		if err == nil {
			reading = readPrompt(state)
		}
		now := time.Now()
		switch reading {
		case promptEditing, promptCanonical:
			streak++
			if streak >= readyStreak {
				return nil
			}
		case promptQuiet, promptOpaque:
			streak = 0
			if reading == promptOpaque {
				if opaqueSince.IsZero() {
					opaqueSince = now
				}
				if now.Sub(opaqueSince) >= opaqueBudget {
					return nil
				}
			}
			screen, capErr := s.b.Capture(ctx, pane, 0, false)
			switch {
			case capErr != nil || screen != lastScreen || settledSince.IsZero():
				lastScreen, settledSince = screen, now
			case strings.TrimSpace(screen) != "" && now.Sub(settledSince) >= settleFor(reading, state.ForegroundCmd):
				return nil
			}
		default:
			streak = 0
			opaqueSince, settledSince = time.Time{}, time.Time{}
		}
		if reading != promptOpaque {
			opaqueSince = time.Time{}
		}
		if reading != promptQuiet && reading != promptOpaque {
			settledSince = time.Time{}
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, readyPollInterval) {
			return errShellNotReady
		}
	}
}

// sleepCtx waits for d, and reports false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// atLineEditor reports whether a pane's line editor is reading right now, on
// the same two-sample rule waitForShellReady uses.
func (s *slots) atLineEditor(ctx context.Context, pane paneRef) bool {
	for i := 0; i < readyStreak; i++ {
		if i > 0 && !sleepCtx(ctx, readyPollInterval) {
			return false
		}
		state, err := s.b.Foreground(ctx, pane)
		if err != nil || readPrompt(state) != promptEditing {
			return false
		}
	}
	return true
}

// sendValidated types into a pane and, when a line editor is reading there,
// makes sure it took what was typed.
//
// It is the one path every keystroke-delivering tool uses for its payload. It
// does not wait for a prompt: a pane this call opened or reclaimed has already
// been waited for during resolution, and a send into an existing slot is meant
// for whatever owns its tty now — a REPL, an editor, a builtin loop the agent
// wants to interrupt, a `read` waiting for an answer. Those keys pass straight
// through, as they always have.
//
// The checks apply only where they mean something, which is a literal payload
// typed at a line editor:
//
//  1. Send the text WITHOUT Enter and wait until the queue is empty with the
//     editor still reading: the evidence it took the bytes into its buffer.
//     A single line still queued when the budget runs out has not run, because
//     Enter was never sent, and the call says so. A payload with a line break
//     in it may have run its first line, so the most that can be said is that
//     reading it could not be confirmed.
//  2. Send Enter on its own and wait until it is no longer queued. A CR the
//     editor consumed is accept-line; one left in the queue is a line that did
//     not run.
//
// A key sent with literal:false — C-c, Escape, Enter, any key name — is never
// checked as text. It is not text: it is an action for whatever reads the tty,
// and the state it leaves the shell in says nothing about whether it arrived.
func (s *slots) sendValidated(ctx context.Context, pane paneRef, text string, literal, enter bool) error {
	if !literal || !ttyProbeSupported || !s.atLineEditor(ctx, pane) {
		return s.b.SendKeys(ctx, pane, text, literal, enter)
	}
	if text != "" {
		multiLine := strings.ContainsAny(text, "\r\n")
		if err := s.b.SendKeys(ctx, pane, text, literal, false); err != nil {
			return err
		}
		if !s.awaitConsumed(ctx, pane, !multiLine) {
			if multiLine {
				return errInputUnconfirmed
			}
			return errInputNotTaken
		}
	}
	if !enter {
		return nil
	}
	if err := s.b.SendKeys(ctx, pane, "Enter", false, false); err != nil {
		return err
	}
	if !s.awaitConsumed(ctx, pane, false) {
		return errEnterNotTaken
	}
	return nil
}

// awaitConsumed polls until nothing typed is left unread on the pane's tty.
// With stillEditing it also requires the line editor to be reading, which is
// what tells "the editor has the bytes in its buffer" apart from "a canonical
// reader has not yet seen a complete line".
//
// A foreground that is no longer the shell counts as consumed: the input
// started something, which is the opposite of input left in the queue. So does
// a pane that is gone, because the send itself has already succeeded and the
// next call will find out.
func (s *slots) awaitConsumed(ctx context.Context, pane paneRef, stillEditing bool) bool {
	deadline := time.Now().Add(inputLandingTimeout)
	for {
		state, err := s.b.Foreground(ctx, pane)
		switch {
		case errors.Is(err, errPaneGone):
			return true
		case err == nil && !paneIsIdleShell(state):
			return true
		case err == nil && state.InputKnown && state.PendingInput == 0 &&
			(!stillEditing || state.LineEditing):
			return true
		}
		if time.Now().After(deadline) || !sleepCtx(ctx, readyPollInterval) {
			return false
		}
	}
}

// ---- Running one command to completion ----

// Timing for execInPane. execStartTimeout is how long a command typed into a
// ready shell is given to begin before the shell is looked at again;
// execPollInterval is how often the exit file is checked. exitStatusGrace is
// how long a shell back at its prompt is given to have written a status.
const (
	execStartTimeout = 3 * time.Second
	execPollInterval = 50 * time.Millisecond
	exitStatusGrace  = 2 * time.Second
)

// execInPane runs one shell command in a pane and waits for it to finish.
//
// The command is wrapped so that it marks its own start, tees its output to a
// file and writes its exit status to another (see wrapCommand), and completion
// is the exit file appearing. Polling a file is the one completion signal that
// works under every multiplexer; nothing here needs the multiplexer to wait.
//
// When the pane's own shell is in the foreground, the command line is not typed
// until that shell is reading (waitForShellReady): a shell still busy with
// something of its own would queue the line, and an rc can throw it away. When
// another program owns the tty, the line is for that program, and it is sent as
// it always has been.
//
// # A command that did not start is reported, never typed again
//
// The wrapper is typed through sendValidated, so a line editor has taken every
// byte and the Enter. That still does not prove the command started: a widget
// can take Enter for itself and leave the line in the buffer. So the start
// marker is watched, and if it has not appeared after execStartTimeout while
// the shell is back at a prompt with nothing running, the call fails with
// errNotStarted.
//
// The command is not sent a second time. A copy may still be in the editor's
// buffer, and no keystroke empties a line under every keymap, widget and pane
// width, so a second copy could run alongside the first. Running somebody's
// command twice is worse than reporting that it was not confirmed. Any sign
// that it started — the marker, or a foreground that is not the shell — keeps
// the call waiting for the exit file instead.
//
// A context that ends first returns timedOut with whatever output exists.
func (s *slots) execInPane(ctx context.Context, pane paneRef, command string) (*execOutcome, error) {
	id := uuid.New().String()
	base := filepath.Join("/tmp", "tmux-mcp-"+id)
	startFile, outFile, exitFile := base+".start", base+".out", base+".exit"
	statusFile := exitFile + ".tmp"
	defer os.Remove(startFile)
	defer os.Remove(outFile)
	defer os.Remove(statusFile)
	defer os.Remove(exitFile)

	if s.shellOwnsTTY(ctx, pane) {
		if err := s.waitForShellReady(ctx, pane, shellReadyTimeout); err != nil {
			return nil, err
		}
	}
	shell := wrapperShell(s.paneShell(ctx, pane), s.shell)
	wrapped := wrapCommand(shell, command, startFile, outFile, statusFile, exitFile)
	if err := s.sendValidated(ctx, pane, wrapped, true, true); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	startBy := time.Now().Add(execStartTimeout)
	var promptSince time.Time
	for {
		switch code, state := readExitFile(exitFile); state {
		case exitWritten:
			out, err := os.ReadFile(outFile)
			if err != nil {
				return nil, fmt.Errorf("read output file: %w", err)
			}
			return &execOutcome{Output: strings.TrimRight(string(out), "\n"), ExitCode: code}, nil
		case exitUnreadable:
			return unreadableOutcome(outFile), nil
		}
		state, err := s.b.Foreground(ctx, pane)
		// A shell that exited has ended the pipeline with it — errexit on a
		// failed pipeline does that — so the output is complete, and whatever
		// status was written is all there will be.
		if shellGone(state, err) {
			outcome := finishedOutcome(outFile, statusFile, exitFile)
			outcome.ShellExited = true
			return outcome, nil
		}
		// A command that started and whose shell is back at its prompt has
		// finished; a status it has not moved into place by now will not come.
		if err == nil && commandStarted(startFile) && backAtPrompt(state) {
			if promptSince.IsZero() {
				promptSince = time.Now()
			} else if time.Since(promptSince) >= exitStatusGrace {
				return finishedOutcome(outFile, statusFile, exitFile), nil
			}
		} else {
			promptSince = time.Time{}
		}
		if time.Now().After(startBy) && s.neverStarted(ctx, pane, startFile) {
			return nil, errNotStarted
		}
		if !sleepCtx(ctx, execPollInterval) {
			partial := ""
			if data, err := os.ReadFile(outFile); err == nil {
				partial = strings.TrimRight(string(data), "\n")
			}
			return &execOutcome{Output: partial, ExitCode: -1, TimedOut: true}, nil
		}
	}
}

// unreadableOutcome is the result for a command that finished without a status
// that can be read: exit code -1, whatever output it left, and the reason.
func unreadableOutcome(outFile string) *execOutcome {
	return &execOutcome{Output: readOutput(outFile), ExitCode: -1, StatusUnreadable: true}
}

// statusUnreadableNote is the text a result with StatusUnreadable carries.
const statusUnreadableNote = "the command finished, but its exit status could not be read; " +
	"exitCode -1 does not say whether it failed"

// shellExitedNote is the text a result with ShellExited carries.
const shellExitedNote = "the pane's shell exited when the command ended, so the pane is gone; " +
	"the next call into this slot opens a new one"

// finishedOutcome is the result for a command whose pipeline is over but whose
// exit file was not read on the way: the exit file if it has appeared since,
// otherwise the status file the wrapper writes before moving it, and an
// unreadable status only when neither holds a number. The status file is
// still there when the move never ran — the shell exited first, or a command
// that replaced the wrapper's EXIT trap was stopped by errexit after the trap
// had written it.
func finishedOutcome(outFile, statusFile, exitFile string) *execOutcome {
	for _, path := range []string{exitFile, statusFile} {
		if code, state := readExitFile(path); state == exitWritten {
			return &execOutcome{Output: readOutput(outFile), ExitCode: code}
		}
	}
	return unreadableOutcome(outFile)
}

// readOutput is whatever the command's output file holds, or "".
func readOutput(outFile string) string {
	data, err := os.ReadFile(outFile)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\n")
}

// shellGone reports whether a pane's shell has exited: the pane is gone, or
// still shown with its process dead.
func shellGone(state *PaneState, err error) bool {
	return errors.Is(err, errPaneGone) || (err == nil && state != nil && !state.IsAlive)
}

// commandStarted reports whether the wrapper's start marker exists.
func commandStarted(startFile string) bool {
	_, err := os.Stat(startFile)
	return err == nil
}

// backAtPrompt reports whether a shell is reading at a prompt, which after a
// command has started is the shell's word that the pipeline is over. Only a
// positive or quiet reading counts: a pane the probe cannot read never reports
// this, and its wait ends on the status, the shell exiting, or the timeout.
func backAtPrompt(state *PaneState) bool {
	switch readPrompt(state) {
	case promptEditing, promptCanonical, promptQuiet:
		return true
	}
	return false
}

// errNotStarted is execute-command's answer when its command never started.
// The line may still be in the editor's buffer, so the agent is told to look
// before running it again.
var errNotStarted = errors.New("the command was not confirmed to start, and it was not sent again; " +
	"look at the pane before running it again")

// shellOwnsTTY reports whether the pane's own process is in the foreground, so
// that what is typed there goes to the shell rather than to a program it runs.
func (s *slots) shellOwnsTTY(ctx context.Context, pane paneRef) bool {
	state, err := s.b.Foreground(ctx, pane)
	return err == nil && state != nil && state.IsAlive && state.PanePID > 0 &&
		state.ForegroundPID == state.PanePID
}

// neverStarted reports whether a sent command provably did not begin: the shell
// is at a prompt AND the start marker is absent. The shell is read first, so a
// command that starts and finishes between the two reads leaves its marker
// behind and is seen as started.
func (s *slots) neverStarted(ctx context.Context, pane paneRef, startFile string) bool {
	state, err := s.b.Foreground(ctx, pane)
	if err != nil || !atPrompt(readPrompt(state)) {
		return false
	}
	_, err = os.Stat(startFile)
	return errors.Is(err, os.ErrNotExist)
}

// exitRead is what the status file says so far.
type exitRead int

const (
	// exitPending: no file, or a line still being written.
	exitPending exitRead = iota
	// exitWritten: a whole line holding a number.
	exitWritten
	// exitUnreadable: a whole line that is not a number.
	exitUnreadable
)

// readExitFile reads the exit status a wrapped command wrote. A line counts as
// written once it ends in a newline, which echo always writes last: before
// that the shell may have created the file and not yet filled it. A written
// line that is not a number will never become one.
func readExitFile(path string) (code int, state exitRead) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		return 0, exitPending
	}
	code, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, exitUnreadable
	}
	return code, exitWritten
}

// paneShell names the shell a pane runs, read from the pane's own process: its
// command line where the platform gives it, otherwise its command name while it
// is in the foreground. It is empty when the process cannot be read or is not
// a shell the wrapper can be spelled for.
func (s *slots) paneShell(ctx context.Context, pane paneRef) string {
	state, err := s.b.Foreground(ctx, pane)
	if err != nil || state == nil || state.PanePID <= 0 {
		return ""
	}
	if argv, err := processArgs(state.PanePID); err == nil {
		return shellFromArgv(argv)
	}
	if state.ForegroundPID == state.PanePID {
		return shellFromArgv([]string{state.ForegroundCmd})
	}
	return ""
}

// shellFromArgv names the shell a command line runs: the base of argv[0],
// without the dash a login shell carries. A program that is not a shell, and
// csh or tcsh, whose syntax neither wrapper fits, name none.
func shellFromArgv(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	name := strings.TrimPrefix(filepath.Base(argv[0]), "-")
	switch name {
	case "bash", "zsh", "fish", "sh", "dash", "ksh":
		return name
	}
	return ""
}

// wrapperShell is the shell the wrapper is spelled for: the pane's real one
// when it could be named, and the -shell-type flag only when it could not.
func wrapperShell(real, flag string) string {
	if real != "" {
		return real
	}
	return flag
}

// wrapCommand builds the line execInPane types: mark the start, run the command
// with stdout and stderr through tee into outFile (so it also shows in the
// pane), write the command's own exit status, not tee's, into statusFile, and
// move that to exitFile once the whole pipeline has finished.
//
// The status is taken inside the group, before the pipe, so the form needs
// nothing but $? and works in every POSIX shell — bash, zsh, sh, dash, ksh —
// whatever their pipeline-status arrays are called. The command runs through
// eval, quoted as one word, so that one ending in a comment or in & still
// leaves the status write as a command of its own, and the whole wrapper stays
// one line.
//
// The eval runs in a subshell of its own, with an EXIT trap there that writes
// the subshell's status. It runs as a plain command, not as the condition of
// an if or one side of || or &&, because errexit is switched off in those
// places, subshells included, and a command under errexit has to stop where it
// would stop typed directly. However the subshell ends — the command's last
// status, exit, or errexit — the trap records it. A command that installs its
// own EXIT trap replaces ours, so the status is also taken after the subshell
// and written if the trap did not write it. The one case left without a status
// is that command failing under errexit, which ends the group before the
// fallback runs; it finishes as an unreadable status rather than a wait. The
// move is allowed to fail quietly: with no status to move, an interactive
// shell under errexit would otherwise exit on the failed mv, and with it the
// prompt the unreadable-status wait looks for.
//
// exitFile appears only after tee has exited, which is after everything that
// held the pipe open — a background job the command started included — is
// done, so the output file is complete once exitFile exists. The move is one
// rename, so exitFile never holds a partial line. It is spelled `command \mv`
// so that neither a function nor an alias named mv runs instead; ksh93 expands
// an alias after `command`, and the backslash stops that.
//
// fish has neither $? nor { }, so it gets begin/end and $status; there an
// alias is a function, which `command` already skips. printf with an empty
// format is the one way to create an empty file that all of them accept.
func wrapCommand(shell, command, startFile, outFile, statusFile, exitFile string) string {
	if shell == "fish" {
		return fmt.Sprintf(
			`printf '' > %s; begin; %s; echo $status > %s; end 2>&1 | tee %s; command mv %s %s`,
			startFile, command, statusFile, outFile, statusFile, exitFile,
		)
	}
	return fmt.Sprintf(
		`printf '' > %s; { ( trap 'echo $? > %s' EXIT; eval %s ); _s=$?; [ -f %s ] || echo $_s > %s; } 2>&1 | tee %s; command \mv %s %s 2>/dev/null || :`,
		startFile, statusFile, shellQuote(command), statusFile, statusFile, outFile, statusFile, exitFile,
	)
}
