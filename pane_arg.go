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

// isolatedProperty is the single declaration of the isolated argument, shared by
// the six tools that can CREATE a pane. The four reading tools do not declare it
// at all, and a request that sends one to them is refused rather than ignored —
// see resolveSlot.
//
// The description has to carry one fact the schema cannot: omitting it is not
// the same as sending false. Absent means "whichever kind of pane this slot
// already is", which is what makes an invisible slot addressable by its number
// on every later call; false is a claim that the slot is the visible one, and a
// claim can conflict.
func isolatedProperty() mcp.ToolOption {
	return mcp.WithBoolean("isolated",
		mcp.Description(`Open the pane where nobody can see it — on a private terminal server `+
			`with no window, so nothing appears beside the user. Use it for work they should not `+
			`have to watch. It needs a slot number, because a pane you cannot see has to be `+
			`addressable later; every tool then reaches it by that number alone. Omit this `+
			`argument to use whichever kind of pane the slot already holds, or to get a visible `+
			`one when the slot is empty.`),
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
	Ref      paneRef
	Slot     int
	Created  bool
	Owner    string
	Isolated bool
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

// oneShotResult is the ephemeral execute-command's response, and what it OMITS
// is the contract: no slot, and no created.
//
// The pane was opened, used and destroyed inside the call, so there is no number
// that would reach it again. Reporting one would be an invitation to address a
// pane that no longer exists, and reporting created:true would say a slot had
// been opened when none was.
type oneShotResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut"`
}

// openedPane is open-pane's response.
//
// It reports the kind as well as the slot because open-pane is the one tool
// whose entire job is to hand back a pane: an agent that asked for an invisible
// one has no other way to confirm it got one, since there is nothing to look at,
// and an agent that omitted the argument learns which kind the slot already was.
// It carries no omitempty for the same reason created does not: a field that
// disappears when false teaches the model that false cannot happen.
type openedPane struct {
	slotResolution
	Isolated bool `json:"isolated"`
}

// paneArgSpec declares what the calling tool implements, so that resolveSlot can
// reject an argument the tool does not honour instead of ignoring it.
//
// Ignoring is the failure both fields exist to prevent. An MCP server that drops
// an unknown property resolves the call to slot 1 and SUCCEEDS, so a caller that
// asked for a pane nobody can see would get a visible one beside the user and be
// told it worked.
type paneArgSpec struct {
	// NoCreate marks a READING tool. Two consequences follow from the one fact:
	// the tool declares no isolated argument, so a request carrying one is
	// refused; and a slot that does not exist is an error rather than a pane.
	NoCreate bool

	// AllowEphemeral marks execute-command, the only tool where isolated with no
	// slot means something: a pane created, used and destroyed inside the call.
	// Everywhere else that combination is the refusal in isolatedNeedsSlotText,
	// because a pane you cannot see and cannot name again is a pane nothing can
	// ever reach.
	AllowEphemeral bool
}

// isolatedNeedsSlotText is the refusal for isolated with no slot.
//
// It says WHY rather than merely "slot is required", because the reason is the
// rule the caller has to internalise: a visible pane can be found again by
// looking at the screen, and an invisible one cannot be found again by any means
// except its number.
const isolatedNeedsSlotText = "isolated needs a slot number, because a pane you cannot see " +
	"must be addressable later"

// isolatedNotAcceptedText is the refusal for an isolated argument on a reading
// tool. Ignoring it would be the worse answer: a caller that asked for an
// invisible pane and was quietly answered about a visible one has been told its
// request succeeded.
const isolatedNotAcceptedText = "isolated is not accepted here; this tool only reads a slot " +
	"that already exists"

// parseKindArg reads the "isolated" argument into the tri-state the resolver
// speaks. hasSlot decides whether the isolated form is even legal, so the two
// arguments are parsed together rather than independently.
//
// ephemeral is true only for the one call shape that means "a pane for this
// command and nothing else": execute-command, isolated, no slot. Every other
// tool refuses that shape.
func parseKindArg(
	req mcp.CallToolRequest, spec paneArgSpec, hasSlot bool,
) (kind kindRequest, ephemeral bool, err error) {
	raw, present := req.GetArguments()["isolated"]
	if !present || raw == nil {
		return kindUnstated, false, nil
	}
	if spec.NoCreate {
		return kindUnstated, false, errors.New(isolatedNotAcceptedText)
	}
	isolated, ok := raw.(bool)
	if !ok {
		// Some clients stringify every argument, and "true"/"false" are the only
		// two spellings that can mean anything here. Anything else is a caller
		// belief about this argument that is not true, and rounding it to false
		// would hand back a visible pane to someone who asked for an invisible
		// one — the exact failure the rejection exists to prevent.
		s, isString := raw.(string)
		if !isString {
			return kindUnstated, false, fmt.Errorf("isolated must be true or false, got %v", raw)
		}
		parsed, parseErr := strconv.ParseBool(strings.TrimSpace(s))
		if parseErr != nil {
			return kindUnstated, false, fmt.Errorf("isolated must be true or false, got %q", s)
		}
		isolated = parsed
	}
	if !isolated {
		return kindVisible, false, nil
	}
	if hasSlot {
		return kindIsolated, false, nil
	}
	if spec.AllowEphemeral {
		return kindIsolated, true, nil
	}
	return kindUnstated, false, errors.New(isolatedNeedsSlotText)
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
		// "all" reaches both kinds, and saying so is not decoration: an agent
		// that reads "every helper pane in this window" reasonably concludes the
		// invisible ones survive, and then leaves live shells behind on a server
		// nobody can see. It also takes no isolated argument, deliberately —
		// there is no version of "close everything" that should leave some of it
		// open — and one sent anyway is REFUSED rather than ignored, which is the
		// half that was missing: see parseCloseSlotArg.
		mcp.Description(`Which helper slot to close: 1 (the default), 2, 3, … or "all" to close `+
			`every helper pane this server opened, visible and isolated alike. Panes the server `+
			`created are killed; panes it adopted from the user are interrupted and released, `+
			`never killed.`),
	)
}

// closeKindNotAcceptedText is close-pane's refusal for an isolated argument.
//
// It names the way out, because the request behind it is a real one: an agent
// that opened an invisible pane and wants it gone has to be told how, or it will
// send the argument again in another shape.
const closeKindNotAcceptedText = "isolated is not accepted here; close-pane closes whichever " +
	"kind of pane the slot holds, and slot: \"all\" closes both kinds"

// noPaneArgumentText is the refusal for a pane-selecting argument on the two
// tools that select no pane.
const noPaneArgumentText = "%s is not accepted here; this tool takes no pane argument"

// rejectPaneArgs refuses slot and isolated on list-slots and notify.
//
// Neither tool declares either argument, and neither resolves a slot, so without
// this the rule stops one short again: notify({slot:2}) would put the message on
// the user's status line — which is where notify ALWAYS puts it — and report
// success to a caller who believed it had addressed a pane.
//
// It is deliberately narrow. Only the two argument names that mean "I am
// naming a pane" are refused; an unknown property is left alone, because
// refusing every extra key is a different and much larger promise than the one
// this contract makes.
func rejectPaneArgs(req mcp.CallToolRequest) error {
	args := req.GetArguments()
	for _, name := range []string{"slot", "isolated"} {
		if _, ok := args[name]; ok {
			return fmt.Errorf(noPaneArgumentText, name)
		}
	}
	return nil
}

// parseCloseSlotArg reads close-pane's slot argument. all reports slot:"all";
// present is false when the caller omitted the argument entirely, which
// close-pane reads as slot 1.
//
// It also refuses isolated, and that refusal has to live here because close-pane
// never reaches parseKindArg: it does not resolve a slot, it tears one down. An
// ignored isolated is the destructive version of the failure the whole rejection
// rule exists for — close-pane({slot:2, isolated:true}) on a VISIBLE slot 2 kills
// the pane beside the user, leaves the invisible one running, and reports
// success. The caller stated a belief about which pane it meant and the server
// acted on the other one.
//
// Any present value is refused, false included. isolated:false is as much a
// claim about the kind as isolated:true, and close-pane honours neither: it
// closes what the slot holds.
func parseCloseSlotArg(req mcp.CallToolRequest) (slot int, all bool, present bool, err error) {
	if _, stated := req.GetArguments()["isolated"]; stated {
		return 0, false, false, errors.New(closeKindNotAcceptedText)
	}
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
//
// The ephemeral form of execute-command is the one call shape this function
// cannot answer, because there is no slot to resolve and nothing to reuse. It is
// reported through the second return value rather than handled here: the pane's
// whole lifetime is one handler body, and putting a create-use-destroy sequence
// behind the resolver would make "resolveSlot returned a pane" mean two
// different things about who has to clean it up.
func (s *slots) resolveSlot(
	ctx context.Context, req mcp.CallToolRequest, spec paneArgSpec,
) (paneTarget, error) {
	tgt, ephemeral, err := s.resolveSlotOrEphemeral(ctx, req, spec)
	if err != nil {
		return paneTarget{}, err
	}
	if ephemeral {
		// Unreachable: only execute-command sets AllowEphemeral, and it calls
		// resolveSlotOrEphemeral directly.
		return paneTarget{}, errors.New(isolatedNeedsSlotText)
	}
	return tgt, nil
}

// resolveSlotOrEphemeral is resolveSlot plus the one-shot answer. ephemeral is
// true when the caller asked for an isolated pane with no slot on the only tool
// that allows it; tgt is then the zero value and the caller opens its own pane.
func (s *slots) resolveSlotOrEphemeral(
	ctx context.Context, req mcp.CallToolRequest, spec paneArgSpec,
) (tgt paneTarget, ephemeral bool, err error) {
	slot, hasSlot, err := parseSlotArg(req)
	if err != nil {
		return paneTarget{}, false, err
	}
	kind, ephemeral, err := parseKindArg(req, spec, hasSlot)
	if err != nil {
		return paneTarget{}, false, err
	}
	if ephemeral {
		return paneTarget{}, true, nil
	}
	if !hasSlot {
		slot = slotDefault
	}
	if spec.NoCreate {
		tgt, err = s.lookupOnly(ctx, slot, kind)
		return tgt, false, err
	}
	tgt, err = s.resolveHelper(ctx, slot, kind)
	if err != nil {
		return paneTarget{}, false, visibleError(err)
	}
	return tgt, false, nil
}

// lookupOnly answers a READING tool: it finds the pane occupying a slot and
// never makes one.
//
// A read that creates is the defect this removes, and it was not a small one.
// capture-pane({slot:3}) on an empty slot used to SPLIT the user's window, or
// ADOPT one of their idle shells — writing three tmux options into a pane they
// opened and renaming it — in order to answer a question about a pane that did
// not exist. The answer was then a screenshot of a fresh prompt, which tells the
// caller nothing and costs the user a pane they have to close.
//
// Created is false and stays false. Reading tools do not report it at all (the
// field is nil on the wire), and a value here would be a value nothing consumes.
func (s *slots) lookupOnly(ctx context.Context, slot int, kind kindRequest) (paneTarget, error) {
	holder, found, err := s.lookupSlot(ctx, slot, kind)
	if err != nil {
		return paneTarget{}, visibleError(err)
	}
	if !found {
		return paneTarget{}, fmt.Errorf(missingSlotText, slot)
	}
	return paneTarget{
		Ref:      holder.Record.Ref,
		Slot:     slot,
		Owner:    holder.Record.Owner,
		Isolated: holder.Isolated,
	}, nil
}
