package main

import (
	"context"
	"encoding/json"
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
//
// extraArgs are passed to the server process — "--channel" is the only one any
// test uses.
func agentPaneFixture(t *testing.T, extraArgs ...string) (*mcpClient, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires tmux")
	}
	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	t.Cleanup(func() { _ = exec.Command("tmux", "kill-session", "-t", name).Run() })

	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
	return newMCPClientInPane(t, self, extraArgs...), self
}

// ---- Descriptions must not advertise what the parser refuses ----

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
	// would otherwise look like a clean run. open-pane alone shows two.
	if checked < 3 {
		t.Errorf("only %d slot literals were found in any tool description; the descriptions that "+
			"teach an agent how to name a pane have gone missing, or this test has stopped "+
			"recognising them", checked)
	}
}

// ---- The default target ----

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

// containsString reports whether a list holds an exact string.
func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// TestSendKeysWithNoSlotUsesSlotOne is the behaviour change stated as a test.
//
// A bare send-keys({keys}) used to be an error, and that refusal is what the
// incident behind this design began with: an agent told "paneId is required"
// went looking for $TMUX_PANE and started driving raw tmux. It now lands in
// helper slot 1 — never in the agent's own pane, which is Invariant R and is
// asserted here at the tool level as well as inside the resolver.
func TestSendKeysWithNoSlotUsesSlotOne(t *testing.T) {
	c, self := agentPaneFixture(t)

	var first map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo one"}, &first)

	if slot, _ := first["slot"].(float64); int(slot) != 1 {
		t.Errorf("send-keys reported slot %v, want 1", first["slot"])
	}
	if first["created"] != true {
		t.Error("the first send-keys had to create the slot-1 pane, so created must be true")
	}
	pane := slotPaneID(t, self, 1)
	if pane == self {
		t.Fatalf("send-keys resolved to the agent's own pane %s — it would be typing into the "+
			"conversation the user is having", pane)
	}

	// The second call must land in the same pane, and must NOT claim to have
	// created it: created is the only signal an agent gets that the process it
	// left running there is gone.
	var second map[string]any
	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo two"}, &second)
	if second["created"] != false {
		t.Errorf("second send-keys reported created=%v for a pane it reused", second["created"])
	}
	if again := slotPaneID(t, self, 1); again != pane {
		t.Errorf("second send-keys went to %s, want the same pane %s", again, pane)
	}
}

// TestExecuteCommandWithNoSlotUsesSlotOne covers the tool the plan originally
// left out. execute-command delivers keystrokes exactly as send-keys does, and a
// tool that still answered "paneId is required" while its siblings defaulted to
// slot 1 would be the loophole that sends an agent hunting for the terminal by
// other means.
func TestExecuteCommandWithNoSlotUsesSlotOne(t *testing.T) {
	c, self := agentPaneFixture(t)

	var out map[string]any
	c.callToolJSON(t, "execute-command", map[string]any{"command": "echo slotted-output"}, &out)

	if slot, _ := out["slot"].(float64); int(slot) != 1 {
		t.Errorf("execute-command reported slot %v, want 1", out["slot"])
	}
	if pane := slotPaneID(t, self, 1); pane == self {
		t.Fatalf("execute-command resolved to the agent's own pane %s", pane)
	}
	if output, _ := out["output"].(string); !strings.Contains(output, "slotted-output") {
		t.Errorf("command did not run in the helper pane; output was %q", output)
	}
	if code, _ := out["exitCode"].(float64); code != 0 {
		t.Errorf("exit code %v, want 0", out["exitCode"])
	}
	// timedOut is always present, never omitted: an absent key makes
	// `result.timedOut === false` unsatisfiable for the caller.
	if _, ok := out["timedOut"]; !ok {
		t.Errorf("execute-command must always report timedOut: %v", out)
	}
}

// TestWriteToDisplayWithNoSlotUsesSlotOne is the same behaviour change for the
// last keystroke-delivering tool that still demanded an explicit pane.
//
// write-to-display calls SendKeys like the others, so refusing a bare call had
// exactly the same consequence: an agent told "paneId is required" goes hunting
// for $TMUX_PANE. Landing in slot 1 is the fix, and slot 1 is never the agent's
// own pane — which for this tool means the coaching text cannot end up in the
// conversation it is designed to stay out of.
func TestWriteToDisplayWithNoSlotUsesSlotOne(t *testing.T) {
	c, self := agentPaneFixture(t)

	var first map[string]any
	c.callToolJSON(t, "write-to-display", map[string]any{"text": "coaching-one"}, &first)

	if slot, _ := first["slot"].(float64); int(slot) != 1 {
		t.Errorf("write-to-display reported slot %v, want 1", first["slot"])
	}
	if first["created"] != true {
		t.Error("the first write-to-display had to create the slot-1 pane, so created must be true")
	}
	pane := slotPaneID(t, self, 1)
	if pane == self {
		t.Fatalf("write-to-display resolved to the agent's own pane %s — the text would land in "+
			"the conversation it exists to stay out of", pane)
	}
	// The one thing this tool must never return is the text itself: keeping it
	// out of the model's context is the whole reason the tool exists.
	if _, hasText := first["text"]; hasText {
		t.Errorf("write-to-display echoed its text back in the response: %v", first)
	}

	// Whitespace is stripped before matching: a helper pane is half the window
	// wide and can wrap a word mid-character.
	sleep(300 * time.Millisecond)
	captured := c.callToolText(t, "capture-pane", map[string]any{})
	if !strings.Contains(flattenPane(captured), "coaching-one") {
		t.Errorf("the text never reached the slot-1 pane; it holds:\n%s", captured)
	}

	// The second call must land in the same pane and must not claim to have
	// created it — created is the only signal that the pane is new to the slot.
	var second map[string]any
	c.callToolJSON(t, "write-to-display", map[string]any{"text": "coaching-two"}, &second)
	if second["created"] != false {
		t.Errorf("second write-to-display reported created=%v for a pane it reused", second["created"])
	}
	if again := slotPaneID(t, self, 1); again != pane {
		t.Errorf("second write-to-display went to %s, want the same pane %s", again, pane)
	}
}

// TestStartAndWatchWithNoArgumentsUsesSlotOne covers the default path, and it is
// the one with the most history behind it.
//
// start-and-watch with no slot used to create its own detached session on a
// socket of its own. The command ran, the pattern matched, the response was
// correct in every field — and the pane was somewhere the user could not see, in
// a session no tool of theirs listed, holding a dev server that survived until
// something eventually killed the session. That behaviour would pass every OTHER
// test in this file, because the ones that exercise the slot path all pass a
// slot; this is the default path, and it is the path an agent following the tool
// description actually takes.
//
// So the assertions are about WHERE the pane is, not just which fields came
// back: slot 1, reported as created, in the agent's own window rather than a
// session of the server's own making. The event assertion is what proves the
// command ran in the pane being talked about — a response naming a slot the
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

	if slot, _ := watch["slot"].(float64); int(slot) != 1 {
		t.Errorf("start-and-watch reported slot %v, want 1", watch["slot"])
	}
	if watch["created"] != true {
		t.Error("the first start-and-watch had to create the slot-1 pane, so created must be true")
	}
	if event, _ := watch["event"].(string); !strings.Contains(event, "default-path-ready") {
		t.Fatalf("start-and-watch did not match its own pattern (event %q): the command did not run "+
			"in the pane it reported, so the assertions above are about a pane nothing happened in: %v",
			event, watch)
	}

	pane := slotPaneID(t, self, 1)
	if pane == self {
		t.Fatalf("start-and-watch ran the command in the agent's own pane %s", pane)
	}
	if panes := panesInWindow(t, window); !containsString(panes, pane) {
		t.Errorf("the slot-1 pane %s is not in the agent's own window %s (which holds %v) — the "+
			"command is running somewhere the user cannot see it, which is the behaviour this "+
			"default replaced", pane, window, panes)
	}
}

// ---- The registry is in tmux, not in this process ----

// TestSlotRecordLivesInTmuxNotInThisProcess is the test that makes the storage
// decision falsifiable, and without it the decision is only a comment.
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
	first.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &opened)
	if opened["created"] != true {
		t.Fatalf("slot 1 already existed in a fresh window, so this test is not exercising "+
			"anything: %v", opened)
	}
	pane := slotPaneID(t, self, 1)
	if pane == self {
		t.Fatalf("the first server resolved slot 1 to its own pane %s", self)
	}
	before := panesInWindow(t, window)

	// A second server process, in the same pane the first one runs in: what a
	// restart looks like, and what a second agent in the window looks like. It
	// shares nothing with the first but the tmux server.
	second := newMCPClientInPane(t, self)

	var found map[string]any
	second.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &found)
	if found["created"] != false {
		t.Errorf("the second server reported created=%v for a pane that already existed (%v); "+
			"created is what tells an agent its process is gone, so a false positive says a "+
			"running dev server died", found["created"], found)
	}
	if got := slotPaneID(t, self, 1); got != pane {
		t.Errorf("the second server resolved slot 1 to %s, want %s — the pane the first server "+
			"created. A registry held in the process cannot be read by another process, and the "+
			"user gets a second identical pane", got, pane)
	}
	if after := panesInWindow(t, window); len(after) != len(before) {
		t.Errorf("the window went from %d panes to %d (%v → %v) when a second server resolved a "+
			"slot the first had already opened", len(before), len(after), before, after)
	}

	// The user closes the pane. Raw tmux, because this is the one event the server
	// never performs and cannot be told about.
	tmuxExec(t, "kill-pane", "-t", pane)

	var again map[string]any
	second.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &again)
	if again["created"] != true {
		t.Errorf("slot 1 reported created=%v after its pane was killed (%v); the agent is not told "+
			"that whatever it left running there is gone", again["created"], again)
	}
	got := slotPaneID(t, self, 1)
	if got == pane {
		t.Errorf("slot 1 still resolves to %s after the pane was killed: the record outlived the "+
			"pane, which it cannot do if it lives on the pane — and keystrokes sent to a dead pane "+
			"are accepted and discarded in silence", pane)
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

// ---- write-to-display's clear ----

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
		waitForPaneIdle(t, usersPane)

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
		if got := slotPaneID(t, self, 1); got != usersPane {
			t.Fatalf("the fixture did not exercise adoption: slot 1 is %s, want the user's pane %s",
				got, usersPane)
		}

		sleep(300 * time.Millisecond)
		flat := flattenPane(c.callToolText(t, "capture-pane", map[string]any{}))
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
		if first["created"] != true {
			t.Fatalf("the fixture did not create a pane of ours: %v", first)
		}
		sleep(300 * time.Millisecond)

		c.callToolJSON(t, "write-to-display", map[string]any{
			"text": "FRESH-TEXT", "clear": true,
		}, &first)
		sleep(300 * time.Millisecond)

		flat := flattenPane(c.callToolText(t, "capture-pane", map[string]any{}))
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
// unsubmitted command line, so it is walked in full rather than sampled. The
// rows that matter are the ones where nothing positive is known: an empty owner,
// or an owner kind this binary does not recognise. Those must be the REDRAW,
// because a resolution that reported no owner is a resolution that told us
// nothing, and "no evidence" is not evidence that the pane is ours.
//
// The bySlot column is gone with the explicit-paneId path. It used to say that a
// pane with NO record was ours to clear — true when the caller had named a
// display pane it split itself, and unreachable now that every target is
// resolved by slot.
func TestClearDecisionNeverKillsALineOnEvidenceItDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner string
		want  bool
		why   string
	}{
		{"a pane we made", ownerAgent, true,
			"the display pane is ours; each write must replace the last, which needs the kill"},
		{"a pane we adopted", ownerAcquired, false,
			"the line in it is the user's half-typed command"},
		{"resolved but reported no owner", "", false,
			"a resolution that says nothing is not permission to destroy a line"},
		{"an owner kind we do not know", "future-owner-kind", false,
			"an unrecognised owner is a pane whose semantics this binary does not implement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clearKillsTheLine(tc.owner); got != tc.want {
				verb := map[bool]string{true: "C-u + clear + Enter", false: "C-l (redraw)"}
				t.Errorf("owner=%q chose %s, want %s — %s",
					tc.owner, verb[got], verb[tc.want], tc.why)
			}
		})
	}
}

// TestClearForDisplayTrustsTheOwnerCapturedUnderTheLock is the race, reproduced
// by its state rather than by its timing.
//
// # What is being reproduced
//
// resolveSlot resolves a slot under slotMu and RELEASES the lock before
// returning. write-to-display({clear:true}) then acts on that pane with no lock
// held. A concurrent close-pane on the same slot releases the adopted pane —
// releaseAcquiredLocked wipes @mcp_pane/@mcp_owner/@mcp_slot and leaves the pane
// alive, handed back to the user. Land it in that window and a clear which reads
// the registry AGAIN sees no record at all, reads that as a pane nobody owns,
// and sends C-u + `clear` + Enter into a shell the user is typing in: their line
// destroyed, and Enter pressed on what is left.
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
	sl, self := slotFixture(t)
	ctx := context.Background()

	// The user's own pane, in a scratch directory so that a line submitted by
	// accident leaves its file there rather than in the repository.
	dir := t.TempDir()
	usersPane := newPaneRef(tmuxExec(t, "split-window", "-d", "-c", dir, "-t", self.target(), "-P", "-F", "#{pane_id}"))
	waitForClientPaneIdle(t, sl.b, usersPane)

	pane, slot, created, owner, err := sl.resolveHelper(ctx, slotDefault)
	if err != nil {
		t.Fatalf("resolveHelper: %v", err)
	}
	if pane != usersPane || owner != ownerAcquired {
		t.Fatalf("the fixture did not exercise adoption: slot 1 resolved to %s (owner %q), want the "+
			"user's pane %s adopted as %q", pane.target(), owner, usersPane.target(), ownerAcquired)
	}
	// The target the handler is holding when the lock goes away.
	tgt := paneTarget{Ref: pane, Slot: slot, Created: created, Owner: owner}

	// The concurrent close-pane lands. This is the mutation, verbatim: the
	// registration is erased and the pane is left alive, which is what "released
	// back to the user" means.
	if err := sl.b.ClearMarks(ctx, usersPane); err != nil {
		t.Fatalf("release the adopted pane: %v", err)
	}
	if _, found := recordFor(t, sl, self, usersPane); found {
		t.Fatal("the pane still carries a registry record; a re-read would find it and this test " +
			"would not be reproducing the race at all")
	}

	// The pane is theirs again, and they start typing. The trailing space is
	// load-bearing for the same reason it is in the test above: without it a
	// "clear" appended to the line would run `touch <sentinel>clear` and create a
	// different file, so a submitted line would leave the sentinel absent and the
	// assertion below would report success.
	sentinel := filepath.Join(dir, "submitted")
	tmuxExec(t, "send-keys", "-t", usersPane.target(), "-l", "touch "+sentinel+" ")
	sleep(300 * time.Millisecond)

	if err := sl.clearForDisplay(ctx, tgt); err != nil {
		t.Fatalf("clearForDisplay: %v", err)
	}
	sleep(300 * time.Millisecond)

	content, err := sl.b.Capture(ctx, usersPane, 0, false)
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
//
// The response is a BARE ARRAY. It used to be wrapped in {"closed": […]}, and
// the wrapper went with the release that made list-slots its neighbour: two
// registry tools with two shapes is one more thing the model has to remember for
// no gain.
func closedEntries(t *testing.T, c *mcpClient, args map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	c.callToolJSON(t, "close-pane", args, &out)
	return out
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
	c, self := agentPaneFixture(t)

	pane := openSlot(t, c, self, 1)

	entries := closedEntries(t, c, map[string]any{})
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %v", entries)
	}
	if entries[0]["action"] != actionKilled {
		t.Errorf("action is %v, want %q — the server created this pane", entries[0]["action"], actionKilled)
	}
	if slot, _ := entries[0]["slot"].(float64); int(slot) != 1 {
		t.Errorf("closed slot %v, want 1", entries[0]["slot"])
	}
	if _, hasPaneID := entries[0]["paneId"]; hasPaneID {
		t.Errorf("close-pane answered with a paneId: %v", entries[0])
	}
	if paneExists(t, pane) {
		t.Errorf("pane %s still exists after close-pane reported killing it", pane)
	}

	// Closing an empty slot is a satisfied request, not an error: an agent must
	// be able to tear down unconditionally without first checking what it opened.
	again := closedEntries(t, c, map[string]any{})
	if len(again) != 1 || again[0]["action"] != actionNone {
		t.Errorf("closing an empty slot answered %v, want a single action:%q entry", again, actionNone)
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
	waitForPaneIdle(t, usersPane)

	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo adopted", "enter": true},
		&map[string]any{})
	if got := slotPaneID(t, self, 1); got != usersPane {
		t.Fatalf("the fixture did not exercise adoption: slot 1 is %s, want %s", got, usersPane)
	}
	waitForPaneIdle(t, usersPane)

	entries := closedEntries(t, c, map[string]any{})
	if len(entries) != 1 || entries[0]["action"] != actionReleased {
		t.Fatalf("close-pane answered %v, want a single action:%q entry", entries, actionReleased)
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

// TestClosePaneRefusesSelfPane is the regression for the one hole every other
// path had already closed — and the rewrite is the point of it.
//
// The fixture is the ordinary nested case, not a contrivance: an outer agent's
// open-pane creates a pane, marks it agent-owned in slot 1 and titles it
// "agent"; a subagent's server is started inside that pane and inherits
// TMUX_PANE. From the inner server the pane is indistinguishable from a helper —
// the witness matches, the owner is "agent", the title says so — which is exactly
// why the record cannot be the thing that authorises the close. The markers are
// written with raw tmux because the outer server is not part of this test.
//
// It used to drive close-pane({paneId}), the branch that went straight from "a
// record exists" to KillPane. That argument is gone, so the guard's only
// remaining route is the SLOT path: close-pane({slot:1}) on a server whose own
// pane carries slot 1's marks. Deleting the old test with the old argument would
// have left the guard live and untested — which is how a guard rots.
//
// The refusal must name the slot and must NOT name kill-pane, a tool this
// release deletes: an error that points at something which does not exist is the
// same failure as an error that says only "no".
func TestClosePaneRefusesSelfPane(t *testing.T) {
	c, self := agentPaneFixture(t)

	tmuxExec(t, "set-option", "-p", "-t", self, paneOptWitness, self)
	tmuxExec(t, "set-option", "-p", "-t", self, paneOptOwner, ownerAgent)
	tmuxExec(t, "set-option", "-p", "-t", self, paneOptSlot, "1")

	entries := closedEntries(t, c, map[string]any{"slot": 1})
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %v", entries)
	}
	if entries[0]["action"] != actionError {
		t.Fatalf("close-pane({slot:1}) on the server's own pane answered action %v, want %q",
			entries[0]["action"], actionError)
	}
	detail, _ := entries[0]["detail"].(string)
	if !strings.Contains(detail, "slot 1 is the pane this server is running in") {
		t.Errorf("the refusal does not explain that this is the server's own pane, so it reads "+
			"like an ordinary failure instead: %q", detail)
	}
	if strings.Contains(detail, "kill-pane") {
		t.Errorf("the refusal points at kill-pane, which this release deletes: %q", detail)
	}
	if idInText.MatchString(detail) {
		t.Errorf("the refusal carries a pane id, which reaches the model verbatim through "+
			"detail: %q", detail)
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
			c, self := agentPaneFixture(t)

			pane := openSlot(t, c, self, 1)

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
			if entries[0]["action"] != actionKilled {
				t.Errorf("close-pane%v answered action %v for a dead slot-1 pane, want %q — "+
					"\"none\" would mean \"this slot is empty\", which is not what the user is "+
					"looking at", tc.args, entries[0]["action"], actionKilled)
			}
			if paneExists(t, pane) {
				t.Errorf("dead pane %s is still in the window; a corpse nobody can close also "+
					"consumes its slot number forever", pane)
			}

			// Slot 1 has to be usable again, which is the reusability half: with
			// the corpse gone, resolving slot 1 must produce a fresh live pane
			// rather than rediscovering the body or failing.
			var next map[string]any
			c.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &next)
			if slot, _ := next["slot"].(float64); int(slot) != 1 {
				t.Errorf("slot 1 resolved to slot %v after the corpse was reaped", next["slot"])
			}
			if next["created"] != true {
				t.Error("slot 1 reported created=false after its pane was reaped — it must be a new pane")
			}
			if got := slotPaneID(t, self, 1); got == pane {
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
	waitForPaneIdle(t, usersPane)
	c.callToolJSON(t, "open-pane", map[string]any{"slot": 1}, &map[string]any{})
	if got := slotPaneID(t, self, 1); got != usersPane {
		t.Fatalf("slot 1 resolved to %s, want the adopted pane %s", got, usersPane)
	}

	// Slot 2: created by the server.
	madePane := openSlot(t, c, self, 2)

	entries := closedEntries(t, c, map[string]any{"slot": "all"})
	if len(entries) != 2 {
		t.Fatalf("expected two entries, got %v", entries)
	}
	bySlot := map[int]string{}
	for _, e := range entries {
		slot, _ := e["slot"].(float64)
		action, _ := e["action"].(string)
		bySlot[int(slot)] = action
	}
	if bySlot[1] != actionReleased {
		t.Errorf("slot 1 (adopted pane %s) got action %q, want %q", usersPane, bySlot[1], actionReleased)
	}
	if bySlot[2] != actionKilled {
		t.Errorf("slot 2 (created pane %s) got action %q, want %q", madePane, bySlot[2], actionKilled)
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

// ---- list-slots ----

// TestListSlotsReportsWhatTheAgentHasOpen covers the tool that replaces
// list-panes for an agent: not "what is on screen", but "what have I got".
//
// The distinction is the assertion about the user's pane. list-panes answered
// with every pane in the window, including ones the agent must never touch;
// list-slots answers only for panes this server holds, and each entry carries
// the number the agent passes back to every other tool.
func TestListSlotsReportsWhatTheAgentHasOpen(t *testing.T) {
	c, self := agentPaneFixture(t)

	// A pane of the user's that we must not adopt: busy, so canAcquire refuses
	// it, which keeps it out of the listing for a reason the test states.
	usersPane := tmuxExec(t, "split-window", "-d", "-t", self, "-P", "-F", "#{pane_id}")
	tmuxExec(t, "send-keys", "-t", usersPane, "cat", "Enter")
	waitForPaneBusy(t, usersPane)

	var empty []map[string]any
	c.callToolJSON(t, "list-slots", map[string]any{}, &empty)
	if len(empty) != 0 {
		t.Fatalf("a server that has opened nothing must list nothing, got %v", empty)
	}

	openSlot(t, c, self, 1)
	c.callToolJSON(t, "execute-command", map[string]any{"slot": 2, "command": "echo two"},
		&map[string]any{})

	var listed []map[string]any
	c.callToolJSON(t, "list-slots", map[string]any{}, &listed)
	if len(listed) != 2 {
		t.Fatalf("list-slots reported %d entries, want 2: %v", len(listed), listed)
	}
	for i, want := range []int{1, 2} {
		got, _ := listed[i]["slot"].(float64)
		if int(got) != want {
			t.Errorf("entry %d is slot %v, want %d — list-slots is sorted by slot", i, listed[i]["slot"], want)
		}
		if listed[i]["origin"] != originCreated {
			t.Errorf("slot %d origin is %v, want %q", want, listed[i]["origin"], originCreated)
		}
		if listed[i]["isAlive"] != true {
			t.Errorf("slot %d is not alive: %v", want, listed[i])
		}
		if listed[i]["isolated"] != false {
			t.Errorf("slot %d reports isolated=%v; every slot is visible in this release",
				want, listed[i]["isolated"])
		}
		if cmd, _ := listed[i]["foregroundCmd"].(string); cmd == "" {
			t.Errorf("slot %d reports no foregroundCmd; the field exists to say what is running "+
				"in the pane: %v", want, listed[i])
		}
		if _, hasPaneID := listed[i]["paneId"]; hasPaneID {
			t.Errorf("list-slots answered with a paneId: %v", listed[i])
		}
	}

	// The user's pane is not ours and is not listed — and the control for that
	// assertion is that it is still there to have been missed.
	if !paneExists(t, usersPane) {
		t.Fatal("the user's pane is gone, so 'list-slots does not report it' proves nothing")
	}

	// A stale slot marker on the agent's OWN pane is what a subagent inherits
	// when it is started into an outer agent's helper. list-slots must neither
	// report it — the agent would address slot 3 and type into its own session —
	// nor leave it in place, because a record nobody clears goes on steering
	// every later call. Reading the registry is mildly mutating for exactly this
	// reason, which is why the tool carries no readOnlyHint.
	//
	// close-pane answers differently for the same record, and deliberately: there
	// it becomes an entry saying "slot 3 is the pane this server is running in",
	// because "none" would tell the agent the slot is free.
	tmuxExec(t, "set-option", "-p", "-t", self, paneOptWitness, self)
	tmuxExec(t, "set-option", "-p", "-t", self, paneOptOwner, ownerAgent)
	tmuxExec(t, "set-option", "-p", "-t", self, paneOptSlot, "3")

	c.callToolJSON(t, "list-slots", map[string]any{}, &listed)
	if listingHasSlot(listed, 3) {
		t.Errorf("list-slots reported slot 3, which is the agent's own pane: %v", listed)
	}
	if got := tmuxExec(t, "display-message", "-p", "-t", self, "#{"+paneOptSlot+"}"); got != "" {
		t.Errorf("the agent's own pane still carries %s=%q after list-slots; the stale claim goes "+
			"on steering every later resolution", paneOptSlot, got)
	}
}

// ---- the two tools whose body is not JSON ----

// TestCapturePaneKeepsItsTextBody guards the one tool whose entire contract is
// "the pane's exact content". The slot must ride in structuredContent and never
// be prepended to the text, because a header there would corrupt the answer for
// every existing caller and be indistinguishable from pane output.
func TestCapturePaneKeepsItsTextBody(t *testing.T) {
	c, self := agentPaneFixture(t)

	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo capture-marker", "enter": true},
		&map[string]any{})
	waitForPaneIdle(t, slotPaneID(t, self, 1))

	res := callTool(t, c, "capture-pane", map[string]any{})
	body := res.text(t, "capture-pane")
	if !strings.Contains(body, "capture-marker") {
		t.Errorf("capture-pane did not read the slot-1 pane; body was:\n%s", body)
	}
	if strings.Contains(body, `"slot"`) {
		t.Errorf("capture-pane wrote resolution metadata into its text body:\n%s", body)
	}
	if slot, _ := res.StructuredContent["slot"].(float64); int(slot) != 1 {
		t.Errorf("structuredContent slot is %v, want 1", res.StructuredContent["slot"])
	}
	if _, hasPaneID := res.StructuredContent["paneId"]; hasPaneID {
		t.Errorf("structuredContent carries a paneId: %v", res.StructuredContent)
	}
}

// TestScreenshotPaneNamesTheSlotAndNotThePane is the caption leak, pinned.
//
// A rendered pane cannot say which pane it is, which is why the slot rides
// alongside the image in structuredContent — and why the image CAPTION names it
// too. That caption used to read "Terminal screenshot of pane %3", which is a
// pane id in the model's context arriving through a path no response type covers
// and no schema check can see.
//
// html output rather than the default PNG: the assertion is about the metadata,
// and the PNG path needs headless Chrome, which would turn this into a skip on
// half the machines that run it.
func TestScreenshotPaneNamesTheSlotAndNotThePane(t *testing.T) {
	c, self := agentPaneFixture(t)

	c.callToolJSON(t, "send-keys", map[string]any{"keys": "echo shot-marker", "enter": true},
		&map[string]any{})
	waitForPaneIdle(t, slotPaneID(t, self, 1))

	res := callTool(t, c, "screenshot-pane", map[string]any{"output": "html"})
	if res.IsError {
		t.Fatalf("screenshot-pane errored: %s", res.text(t, "screenshot-pane"))
	}
	if slot, _ := res.StructuredContent["slot"].(float64); int(slot) != 1 {
		t.Errorf("structuredContent slot is %v, want 1; a rendered pane cannot name itself, so "+
			"this key is the only answer the caller gets", res.StructuredContent["slot"])
	}
	if _, hasPaneID := res.StructuredContent["paneId"]; hasPaneID {
		t.Errorf("structuredContent carries a paneId: %v", res.StructuredContent)
	}
}
