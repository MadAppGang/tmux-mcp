package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// This file is the single place that reads the pane-selecting arguments —
// paneId, slot, headless — of every tool that takes them. Nothing else parses
// them. See resolvePaneArg for why that restriction is the mechanism rather than
// a convention.

// ---- Schema ----

// jsonSchemaType sets the JSON Schema "type" keyword directly, which mcp-go's
// typed helpers do not let us do: each of them hard-codes one type, and neither
// of the two types we need is the one they pick.
//
// Two callers, for two different reasons:
//
//   - slotProperty narrows WithNumber's "number" to "integer". Slots are pane
//     indices; advertising them as "number" invites slot 1.5, which then has to
//     be rejected at runtime for no reason other than a loose schema.
//   - closeSlotProperty needs a genuine union, integer or the string "all".
//
// The union is expressed as a type array rather than anyOf/oneOf because several
// MCP clients convert tool schemas into their own model-facing format and handle
// the composition keywords poorly or not at all; "type": [...] survives that
// conversion. The cost is that "banana" is schema-valid for close-pane and has to
// be rejected at runtime — unavoidable either way, since no schema can express
// "an integer, or the single string all" without oneOf.
func jsonSchemaType(types ...string) mcp.PropertyOption {
	return func(schema map[string]any) {
		if len(types) == 1 {
			schema["type"] = types[0]
			return
		}
		schema["type"] = types
	}
}

// slotProperty is the single declaration of the slot argument, shared by every
// tool that takes one. One definition means one description, and the nine tools
// cannot drift apart — a caller that learns what slot means from one tool's
// schema has learned it for all of them.
//
// The bounds are declared as well as enforced. checkedSlot rejects out-of-range
// values anyway, but a caller that can see the range in the schema does not have
// to discover it by being refused.
func slotProperty() mcp.ToolOption {
	return mcp.WithNumber("slot",
		jsonSchemaType("integer"),
		mcp.Min(slotDefault),
		mcp.Max(maxSlot),
		mcp.Description(`Helper pane to use: 1 (the default), 2, 3, … Omit both slot and paneId `+
			`to use slot 1. Slot panes are created beside this agent's pane, are reused across `+
			`calls, and are never the agent's own pane. Cannot be combined with headless:true.`),
	)
}

// ---- Parsing ----

// parseSlotArg reads the "slot" argument. present is false when the caller
// omitted it.
//
// ONLY resolvePaneArg may call this; see resolvePaneArg for why.
//
// The raw argument is inspected rather than going through req.GetInt, and that
// is not fastidiousness. GetInt runs an unparseable value through strconv.Atoi
// and silently substitutes the default, so any typo — slot "tow", slot "1x" —
// becomes slot 0, which resolvePaneArg then reads as "omitted" and turns into
// slot 1. The caller's mistake would land as a real write to the shared default
// pane, with no error raised anywhere. Every malformed value has to fail loudly
// instead, which means parsing it here.
//
// A string that holds a number is still accepted, because some clients stringify
// every argument. What is rejected is a string that is not a number.
//
// A non-integral number is an error rather than a truncation, for the same
// reason: slot 1.5 means the caller believes something about slots that is not
// true, and rounding it to a real pane hides that.
func parseSlotArg(req mcp.CallToolRequest) (slot int, present bool, err error) {
	raw, ok := req.GetArguments()["slot"]
	if !ok || raw == nil {
		return 0, false, nil
	}

	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false, slotArgError(raw)
		}
		return checkedSlot(int(v), raw)
	case int:
		return checkedSlot(v, raw)
	case int64:
		return checkedSlot(int(v), raw)
	case json.Number:
		n, convErr := v.Int64()
		if convErr != nil {
			return 0, false, slotArgError(raw)
		}
		return checkedSlot(int(n), raw)
	case string:
		s := strings.TrimSpace(v)
		n, convErr := strconv.Atoi(s)
		if convErr != nil {
			return 0, false, slotArgError(raw)
		}
		return checkedSlot(n, raw)
	default:
		return 0, false, slotArgError(raw)
	}
}

// checkedSlot enforces the 1..maxSlot range. The upper bound is what turns a
// typo into an error instead of a pane: nothing sensible asks for slot 99999,
// and creating it would leave the user with a window they have to clean up by
// hand.
func checkedSlot(n int, raw any) (int, bool, error) {
	if n < slotDefault || n > maxSlot {
		return 0, false, slotArgError(raw)
	}
	return n, true, nil
}

// slotArgError is the one rejection message, so every bad slot value is
// explained the same way — including which values ARE accepted, because an error
// that only says "invalid" is what sends an agent looking for another route.
func slotArgError(raw any) error {
	shown := fmt.Sprintf("%v", raw)
	if s, ok := raw.(string); ok {
		shown = strconv.Quote(s)
	}
	return fmt.Errorf("slot must be an integer from 1 to %d, got %s", maxSlot, shown)
}

// ---- Resolution ----

// paneTarget is the outcome of the single resolution path shared by every
// pane-taking tool.
//
// Owner is the only field a handler cannot obtain for itself, and that is the
// reason it is carried here rather than looked up when needed. It is the
// registry's answer as resolveHelper read it under slotMu, in the same hold that
// chose the pane; a handler asking the same question afterwards asks it of a
// registry that a concurrent close-pane may already have erased, and gets "no
// record" for a pane that was ours a millisecond ago and is now the user's
// again. clearForDisplay is the consumer, and there the difference between those
// two answers is whether the user's half-typed command line survives.
//
// It is empty on the explicit-paneId path, where nothing was resolved and so
// nothing was read. Slot is what tells the two apart — 0 means the caller named
// the pane — and a consumer must branch on that rather than on Owner being
// empty, because "we did not look" and "we looked and found nothing" are
// different facts.
type paneTarget struct {
	Ref     paneRef // the handle; non-empty on success, unless Headless
	Slot    int     // 0 when the caller named a pane explicitly
	Created bool    // this call created or adopted the pane
	Owner   string  // ownerAgent or ownerAcquired as read under slotMu during slot
	// resolution; empty when the caller named the pane
	Headless bool // caller asked for a headless pane and named none;
	// only ever set for tools that declare AllowHeadless
}

// paneArgSpec declares what the calling tool actually implements, so that
// resolvePaneArg can reject an argument the tool does not honour instead of
// ignoring it.
type paneArgSpec struct {
	AllowHeadless bool
}

// Resolved reports whether this target came from slot resolution rather than a
// paneId the caller named.
//
// The fact itself is just Slot != 0, and the reason it earns a method is that
// four call sites across two files have to agree about it, and they decide
// different things with it: whether to attach resolution metadata to a response
// (capture-pane, split-pane, screenshot-pane) and whether it is safe to clear a
// pane's line (write-to-display). Slot 0 meaning "explicit paneId" is a sentinel
// this package chose, not something the type enforces, so spelling it out four
// times is four places to update if that ever changes and nothing linking them.
func (tgt paneTarget) Resolved() bool { return tgt.Slot != 0 }

// resolution projects a target into the fields tools put in their responses.
//
// This is the one place outside backend_tmux.go that turns a handle back into an
// id, and it is here because the WIRE still carries paneId: every response type
// below has the field. It dies with that field, in the commit that makes the
// slot the only handle a caller ever sees.
func (tgt paneTarget) resolution() paneResolution {
	return paneResolution{PaneID: tgt.Ref.target(), Slot: tgt.Slot, Created: tgt.Created}
}

// paneResolution is embedded in tool responses so a caller that named no pane
// can still see which one it got, and whether the pane is new.
//
// Slot and Created are omitempty, so a call that passed an explicit paneId
// produces byte-identical JSON to the version before slots existed: send-keys
// still answers exactly {"paneId":"%3"}. That is what "paneId keeps working
// everywhere" means at the wire level, and it is checkable rather than merely
// intended — see TestExplicitPaneIdResponsesAreUnchanged.
type paneResolution struct {
	PaneID  string `json:"paneId"`
	Slot    int    `json:"slot,omitempty"`
	Created bool   `json:"created,omitempty"`
}

// paneStateResult is pane-state's response: the process state it has always
// returned, plus the resolution. PaneState carries no paneId of its own, so the
// two embeddings cannot collide, and Go promotes the exported fields of an
// embedded unexported struct type — which is subtle enough to be pinned by
// TestPaneResolutionFlattens rather than trusted.
type paneStateResult struct {
	*PaneState
	paneResolution
}

// replResult is run-in-repl's response.
//
// It replaces three separate anonymous structs, each of which carried its own
// PaneID field. Keeping that field alongside the embedded paneResolution would
// have produced an object with two "paneId" keys — legal Go, invalid-ish JSON,
// and a bug that only shows up in whichever key the client's parser happens to
// keep. Exited is omitempty so the ordinary "prompt came back" answer is the
// same object it always was.
type replResult struct {
	paneResolution
	Output string `json:"output"`
	Exited bool   `json:"exited,omitempty"`
}

// checkHeadlessArg applies the two headless rules, and exists as its own
// function because close-pane needs them without needing resolution: it selects
// panes but never creates one, so it cannot go through resolvePaneArg, and a
// second hand-written copy of these rules is how the two would drift apart.
func checkHeadlessArg(req mcp.CallToolRequest, spec paneArgSpec, hasSlot bool) (bool, error) {
	headless := req.GetBool("headless", false)
	if headless && !spec.AllowHeadless {
		return false, errors.New(
			"this tool has no headless mode; omit headless, or create the pane with " +
				"create-headless / execute-command(headless:true) and pass the returned paneId")
	}
	if headless && hasSlot {
		return false, errors.New(
			"slot and headless:true cannot be combined: a slot names a pane in the window this " +
				"server runs in, while a headless pane lives on a separate tmux server with no " +
				"window at all — drop one")
	}
	return headless, nil
}

// closeSlotProperty is close-pane's variant of the slot argument.
//
// It is a separate declaration rather than a reuse of slotProperty because
// close-pane accepts one thing the others do not: "all". That makes its argument
// a genuine integer-or-string union, where every other tool's slot is a plain
// integer, so the two cannot share a schema.
func closeSlotProperty() mcp.ToolOption {
	return mcp.WithAny("slot",
		jsonSchemaType("integer", "string"),
		mcp.Description(`Helper slot to close: 1 (the default), 2, 3, … or "all" to close every `+
			`helper pane in this window. Panes the server created are killed; panes it adopted `+
			`from the user are interrupted and released, never killed.`),
	)
}

// parseCloseSlotArg reads close-pane's slot argument. all reports slot:"all";
// present is false when the caller omitted the argument entirely, which
// close-pane reads as slot 1.
func parseCloseSlotArg(req mcp.CallToolRequest) (slot int, all bool, present bool, err error) {
	if raw, ok := req.GetArguments()["slot"]; ok {
		if s, isString := raw.(string); isString && strings.EqualFold(strings.TrimSpace(s), "all") {
			return 0, true, true, nil
		}
	}
	slot, present, err = parseSlotArg(req)
	if err != nil {
		return 0, false, false, err
	}
	return slot, false, present, nil
}

// resolvePaneArg is the ONLY place that reads the paneId, slot and headless
// arguments of a pane-taking tool. Every such handler calls it, and no handler
// parses those arguments itself.
//
// That restriction is the mechanism, not a style preference. Three rules have to
// hold across nine handlers, and nine copies of a rule is nine chances to omit
// one:
//
//  1. slot together with headless:true is an error, never a silent preference.
//     Slots name panes in the window this server runs in; a headless pane lives
//     on a different tmux socket, in a session with no relation to that window.
//     Preferring either one would hand the caller a pane in the wrong universe
//     and no way to notice.
//  2. headless:true against a tool with no headless mode is an error, for the
//     same reason: silently giving that caller a visible slot pane is worse than
//     refusing, because it succeeds.
//  3. the pane returned for a slot is never this server's own pane — Invariant
//     R, enforced inside resolveHelper, which is reachable only through here.
//
// Resolution order: an explicit paneId wins verbatim, because a caller that
// names a pane has taken the safety burden for it; then slot; then the default
// slot 1. A caller that passes both paneId and slot gets the pane it named, and
// the response reports slot 0 — the slot is not resolved at all, so no pane is
// created for it.
func (s *slots) resolvePaneArg(
	ctx context.Context, req mcp.CallToolRequest, spec paneArgSpec,
) (paneTarget, error) {
	slot, hasSlot, err := parseSlotArg(req)
	if err != nil {
		return paneTarget{}, err
	}

	headless, err := checkHeadlessArg(req, spec, hasSlot)
	if err != nil {
		return paneTarget{}, err
	}

	if paneID := req.GetString("paneId", ""); paneID != "" {
		// No Owner, and no lookup to obtain one. Nothing was resolved, so the
		// registry was never consulted and there is no locked answer to carry —
		// the caller named a pane and took the safety burden for it, which is the
		// same trade this function's doc comment makes for every other rule. A
		// consumer that needs an owner on this path reads it itself, knowing it is
		// reading it now rather than at resolution time; clearForDisplay does
		// exactly that, and says why it is safe there.
		return paneTarget{Ref: newPaneRef(paneID)}, nil
	}
	if headless {
		return paneTarget{Headless: true}, nil
	}

	if !hasSlot {
		slot = slotDefault
	}
	pane, resolved, created, owner, err := s.resolveHelper(ctx, slot)
	if err != nil {
		return paneTarget{}, err
	}
	return paneTarget{Ref: pane, Slot: resolved, Created: created, Owner: owner}, nil
}
