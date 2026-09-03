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

// This file is the single place that reads the pane-selecting argument — the
// slot — of every tool that takes one, and the single place that refuses the
// arguments this contract does not have. Nothing else parses either.
//
// The paneId branch is gone, not deprecated. A slot is the only handle at the
// wire, so there is no id to prefer over a slot, no "explicit wins" ordering,
// and no Slot == 0 sentinel meaning "the caller named the pane" — the four call
// sites that had to agree about what a zero meant are gone with it.

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
// tool that takes one. One definition means one description, and the twelve
// tools cannot drift apart — a caller that learns what a slot means from one
// tool's schema has learned it for all of them.
//
// The bounds are declared as well as enforced. checkedSlot rejects out-of-range
// values anyway, but a caller that can see the range in the schema does not have
// to discover it by being refused.
func slotProperty() mcp.ToolOption {
	return mcp.WithNumber("slot",
		jsonSchemaType("integer"),
		mcp.Min(slotDefault),
		mcp.Max(maxSlot),
		mcp.Description(`Which helper pane to use: 1 (the default), 2, 3, … Omit it to use slot 1. `+
			`A slot is a pane beside this agent in the window the user is looking at, and the same `+
			`slot number returns the SAME pane every time — so a process started in slot 2 is still `+
			`running there on the next call. It is never the agent's own pane.`),
	)
}

// ---- Argument rejection ----

// rejectIdArgs refuses the four arguments this surface does not have.
//
// Refusing rather than ignoring is the safety rule, not a strictness
// preference. An MCP server that drops an unknown property resolves the call to
// slot 1 and SUCCEEDS: a caller that sent paneId gets keystrokes delivered to a
// pane it did not name, and a caller that sent headless:true — asking for a pane
// nobody can see — gets a visible one beside the user. Both are the failure a
// refusal makes impossible.
//
// The refusal names the property that was sent: a caller that gets "paneId is
// not accepted" after sending windowId learns nothing about its own request.
// The order is fixed so the message is deterministic when more than one is
// present.
//
// It runs from addAgenticTool, before any handler body — see there for why the
// check cannot live in the resolver.
func rejectIdArgs(req mcp.CallToolRequest) error {
	args := req.GetArguments()
	for _, name := range []string{"paneId", "windowId", "sessionId"} {
		if _, ok := args[name]; ok {
			return fmt.Errorf("%s is not accepted; address the pane by slot", name)
		}
	}
	if _, ok := args["headless"]; ok {
		// Deliberately not the literal "headless:" — the release's grep gate
		// scans the shipped strings for it, and this sentence is the one place
		// the word has to appear at all.
		return errors.New("headless is not accepted; open an invisible pane with " +
			"isolated: true and a slot number")
	}
	return nil
}

// ---- Parsing ----

// parseSlotArg reads the "slot" argument. present is false when the caller
// omitted it.
//
// ONLY resolveSlot and parseCloseSlotArg may call this.
//
// The raw argument is inspected rather than going through req.GetInt, and that
// is not fastidiousness. GetInt runs an unparseable value through strconv.Atoi
// and silently substitutes the default, so any typo — slot "tow", slot "1x" —
// becomes slot 0, which resolveSlot then reads as "omitted" and turns into
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

// paneTarget is the outcome of the one resolution path every slot-taking tool
// shares.
//
// Slot is always 1..maxSlot. The old "Slot == 0 means the caller named a pane"
// sentinel is gone with the paneId path that produced it, and so are Resolved()
// and the four call sites that had to agree about what a zero meant.
//
// Owner is carried rather than looked up, and the reason has not changed: it is
// the registry's answer as resolution read it under slotMu, in the same hold
// that chose the pane. A handler that asks again after the lock is released can
// be told "no record" for a pane that was ours a millisecond ago, because a
// concurrent close-pane released it — and in clearForDisplay the difference
// between those two answers is whether the user's half-typed command line
// survives.
type paneTarget struct {
	Ref     paneRef
	Slot    int
	Created bool
	Owner   string
}

// slotResolution is what a CREATING tool puts in its response.
//
// Created is a POINTER, and "absent" and "false" have to be different answers on
// the wire. created is a discontinuity signal, not a success flag: every call
// succeeds, and what created reports is whether this slot already had a pane. An
// agent that started a dev server in slot 1 and later sees created:true has
// learned the only way it can that the user closed that pane and its process
// died with it.
//
// On a reading tool the question does not arise, and a field that is
// structurally always false would teach the model that a read might create. nil
// means "not applicable"; it is never nil on a creating tool, which is what
// keeps "created is always present" true for the callers that rule was written
// for. See TestCreatedIsPresentOnCreatingToolsAndAbsentOnReading, which cannot
// be satisfied by a plain bool.
type slotResolution struct {
	Slot    int   `json:"slot"`
	Created *bool `json:"created,omitempty"`
}

// creating is the pointer a creating tool puts in slotResolution.Created. It
// exists so no handler has to take the address of a local, which is the shape a
// future edit accidentally shares between two responses.
func creating(created bool) *bool { return &created }

// slotRef is what a READING tool puts in its response: the slot, and nothing
// else.
type slotRef struct {
	Slot int `json:"slot"`
}

// paneStateResult is pane-state's response: the process state it has always
// returned, plus the slot it was read from. PaneState carries no slot of its
// own, so the two embeddings cannot collide, and Go promotes the exported fields
// of an embedded unexported struct type — which is subtle enough to be pinned by
// TestSlotRefFlattens rather than trusted.
type paneStateResult struct {
	*PaneState
	slotRef
}

// replResult is run-in-repl's response.
//
// Exited has no omitempty, deliberately. An absent key makes
// `result.exited === false` unsatisfiable and gives a model reading the response
// no way to learn the field exists at all — the same defect the pointer above
// removes from created, sitting on a field nobody enumerated.
type replResult struct {
	slotResolution
	Output string `json:"output"`
	Exited bool   `json:"exited"`
}

// execResult is execute-command's response. timedOut loses its omitempty for
// the reason replResult.Exited does.
type execResult struct {
	slotResolution
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut"`
}

// paneArgSpec declares what the calling tool implements, so that resolveSlot can
// reject an argument the tool does not honour instead of ignoring it.
//
// It is EMPTY at this commit, and that is a fact about the surface rather than a
// placeholder: its only field was AllowHeadless, and headless is now refused for
// every tool by rejectIdArgs. It is kept as a parameter because the two
// capabilities that do vary between tools — the ephemeral execute-command form,
// and a reading tool that must never create — arrive as fields here, and a
// signature that loses the argument now would have every call site edited twice.
type paneArgSpec struct{}

// closeSlotProperty is close-pane's variant of the slot argument.
//
// It is a separate declaration rather than a reuse of slotProperty because
// close-pane accepts one thing the others do not: "all". That makes its argument
// a genuine integer-or-string union, where every other tool's slot is a plain
// integer, so the two cannot share a schema.
func closeSlotProperty() mcp.ToolOption {
	return mcp.WithAny("slot",
		jsonSchemaType("integer", "string"),
		mcp.Description(`Which helper slot to close: 1 (the default), 2, 3, … or "all" to close `+
			`every helper pane this server opened. Panes the server created are killed; panes it `+
			`adopted from the user are interrupted and released, never killed.`),
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

// resolveSlot is the ONLY place that turns a tool's arguments into a pane.
// Every slot-taking handler calls it, and no handler resolves for itself.
//
// That restriction is the mechanism, not a style preference. Two rules have to
// hold across eleven handlers, and eleven copies of a rule is eleven chances to
// omit one:
//
//  1. a malformed slot is an error rather than slot 1 — see parseSlotArg, where
//     the silent default would land a caller's typo as a real write to the
//     shared default pane;
//  2. the pane returned for a slot is never this server's own pane — Invariant
//     R, enforced inside resolveHelper, which is reachable only through here.
//
// A missing slot argument means slot 1, so the overwhelmingly common request —
// "run this somewhere I can see" — needs no argument at all.
func (s *slots) resolveSlot(
	ctx context.Context, req mcp.CallToolRequest, _ paneArgSpec,
) (paneTarget, error) {
	slot, hasSlot, err := parseSlotArg(req)
	if err != nil {
		return paneTarget{}, err
	}
	if !hasSlot {
		slot = slotDefault
	}
	pane, resolved, created, owner, err := s.resolveHelper(ctx, slot)
	if err != nil {
		return paneTarget{}, visibleError(err)
	}
	return paneTarget{Ref: pane, Slot: resolved, Created: created, Owner: owner}, nil
}
