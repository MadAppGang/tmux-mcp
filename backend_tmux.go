package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// The two types below carry NO json tags, and their absence is the point.
//
// Both of them used to be a tool response — list-panes answered with Pane,
// split-pane with CreatedPane — which is why they carried a `json:"paneId"`
// field. Those tools are gone, and nothing in this package marshals these types
// any more: they are the adapter's own vocabulary for what tmux just told it,
// and they reach policy only as paneRef and paneInfo.
//
// Leaving the tags would leave a response shape lying around with a pane id
// already named in it, one `jsonResult(...)` away from being on the wire again.
// A type that cannot be serialised into a tool result without someone writing
// the tags back is a type that cannot leak by accident.

// Pane is a tmux pane with the extended info list-panes reports.
type Pane struct {
	ID             string
	Title          string
	Active         bool
	Width          int
	Height         int
	CurrentCommand string
	CurrentPath    string
}

// CreatedPane holds the IDs tmux returns when a new pane is created.
//
// Reused, Slot and Created went with the tags: they existed to shape split-pane's
// response, and the tool that answered with them is gone. What the adapter needs
// from a split is the pane it made.
type CreatedPane struct {
	PaneID   string
	WindowID string
}

// errPaneGone reports that a pane a caller named no longer exists. It is a
// sentinel so that the handler can name the SLOT — the only handle the caller
// has — instead of the adapter naming the pane.
var errPaneGone = errors.New("the pane is gone")

// headlessSocket is the tmux socket every isolated pane lives on: a second tmux
// server, with no client attached and no window anyone can see.
//
// It is SHARED between the several tmux-mcp servers a machine may be running,
// which is what makes the namespace in a session name (see namespacePrefix) load
// bearing rather than decorative. Nothing here may ever run kill-server on it:
// the window in which "is the server up?" fails is exactly the window in which a
// neighbour is starting one, and killing it takes that neighbour's panes with it.
const headlessSocket = "mcp-headless"

// Size given to isolated sessions, which are created detached. tmux would
// otherwise use default-size (80x24), and at 80 columns a dev server's output
// wraps mid-line — which silently breaks readiness regexes that expect their
// match on one line, and looks like the pattern being wrong rather than the pane
// being narrow. Measured; keep it. See openIsolatedSession.
const (
	detachedWidth  = 200
	detachedHeight = 50
)

// headlessPrefix is the ID prefix that identifies isolated targets, so that one
// paneRef can address either server and parseTarget routes it.
const headlessPrefix = "headless:"

// Pane options we set on every pane we create. They are read back to decide
// whether a pane is ours to reuse or kill.
//
// paneOptWitness holds the pane's own ID and exists to make a record
// self-certifying: a record counts only when its witness equals the pane it was
// read from. That is not redundant with the -p (pane-scoped) flag — it is the
// backstop for a missing one. tmux user options inherit down the scope chain
// when interpolated in a pane-context format string, so an option set at
// session scope (any set-option that forgets -p) resolves for *every* pane in
// that session. Without the witness, one such slip would make all of the user's
// panes look agent-owned, and pane reuse would then run commands in their
// shell. A single option value can equal only one pane's ID, so the witness
// makes that failure structurally impossible rather than merely unlikely.
//
// The same hazard applies verbatim to paneOptSlot, and with a sharper edge. A
// slot marker leaked to session scope would make *every* pane in the user's
// session answer to slot 1, so the next resolution would hand the agent one of
// the user's shells and call it "the helper pane" — the witness failure of the
// original bug, now on a path that types into the pane rather than merely
// listing it. That is why the slot is never read on its own: every read of
// paneOptSlot or paneOptOwner goes through paneRegistryAround, which fetches
// paneOptWitness in the *same* tmux call and rejects the record unless it
// equals the pane's own ID. There is ONE such reader now — the single-pane
// lookup went with the explicit-paneId path — so "did this new read keep the
// witness?" is a question with one place to look.
//
// There is exactly one further reader, paneOwnerMark, and it deliberately does
// not apply the witness. It asks the opposite question — "is this pane wholly
// unclaimed?" — where a leaked or unrecognised value must mean "hands off". The
// witness exists to stop a leak making a pane look OURS; there it would make a
// leak look like permission. See that function for why the inversion is safe.
const (
	paneOptWitness = "@mcp_pane"
	paneOptOwner   = "@mcp_owner"
	paneOptSlot    = "@mcp_slot" // "1", "2", … — the default helper is slot 1
	ownerAgent     = "agent"
	ownerAcquired  = "acquired" // the user opened the pane; we adopted it
)

// markOrder is the write order, expressed as DATA rather than as statement
// order, so "does the slot marker still go last?" is a question a test can ask
// instead of a property a reviewer has to notice. TestMarkWriteOrder asserts the
// whole slice — not its ends, which would pass with the middle two swapped — and
// asserts that the single set-option in each walker is driven by this slice.
//
// tmux cannot set several options atomically, so every PREFIX of this sequence
// is a state a crashed or cancelled process can leave behind, and the order is
// chosen so that every prefix is inert. Resolution requires all three, so
// witness-only and witness+owner can never steer a later call towards a
// half-claimed pane. The reverse order would publish a slot marker on a pane of
// unknown ownership, and the teardown rule keys off exactly that owner value to
// decide whether a pane may be killed or must merely be released: a slotted pane
// nobody can attribute is the one record this server must never produce.
//
// There are exactly THREE marks. The isolated namespace is deliberately not a
// fourth: it is the NAME of the session an isolated pane lives in, so no path
// that claims a pane in the user's window can write one — see namespacePrefix,
// where the alternative (a fourth pane option, stamped by the same single
// walker) would have landed a namespace on adopted USER panes.
var markOrder = []string{paneOptWitness, paneOptOwner, paneOptSlot}

// tmuxClient wraps tmux CLI interactions.
type tmuxClient struct {
	shellType string

	// slotMu serialises helper-pane slot resolution inside this process. It
	// lives on the client rather than in a package-level var because the server
	// builds exactly one client (main.go) and the tests that construct their own
	// are single-threaded; see resolveHelper for what the lock does and does not
	// protect against.
	slotMu sync.Mutex
}

func newTmuxClient(shellType string) *tmuxClient {
	return &tmuxClient{shellType: shellType}
}

// parseTarget splits a target ID into its socket and bare ID.
// IDs prefixed with "headless:" are routed to the mcp-headless socket.
// All other IDs use the default tmux server (empty socket string).
func parseTarget(target string) (socket string, id string) {
	if strings.HasPrefix(target, headlessPrefix) {
		return headlessSocket, strings.TrimPrefix(target, headlessPrefix)
	}
	return "", target
}

// paneRef is a pane handle that is opaque to policy: hold it, pass it back,
// compare it. It is what the port speaks instead of a tmux id.
//
// It is a STRUCT rather than a defined string type, and the String method below
// is the whole reason. A defined string type stops an id reaching a response
// STRUCT and stops nothing else: %s and %v on a defined string type are silent,
// and every id leak this design has to prevent escapes through a path no
// response type covers — an image caption, a progress notification, a channel
// text line, an fmt.Errorf in policy code. Each of those is a format verb the
// type system is perfectly happy about.
//
// Wrapping the id in a struct with a redacting String makes that class of
// mistake visible instead of silent: a leak prints "<pane>" in the output a
// human or a test is reading, rather than "%73" in the model's context. It is
// defence in depth and not the primary control — the primary control is that
// only this file can turn a handle back into an id — but it is the only control
// that works on code nobody thought to check.
//
// The struct is comparable, so it still keys the registry map, and it costs
// nothing at runtime. It marshals to {} if a response type ever embeds one,
// which is the safe direction.
type paneRef struct{ id string }

// String is deliberately lossy. Anything that formats a paneRef is a bug, and
// this is what that bug looks like when it reaches a log, a test failure or a
// response body.
func (p paneRef) String() string { return "<pane>" }

// target is the real id, for this file's tmux invocations. It is the single
// accessor, so "who can turn a handle back into an id" is a question with one
// answer.
func (p paneRef) target() string { return p.id }

// newPaneRef is the only constructor, and it lives here beside target for the
// same reason.
func newPaneRef(id string) paneRef { return paneRef{id: id} }

// empty reports whether this is the zero handle — no pane, as opposed to a pane
// whose id we happen not to like.
func (p paneRef) empty() bool { return p.id == "" }

// socketArgs returns the global tmux flags that must precede a subcommand in
// order to target the given socket. An empty socket means the default server,
// which needs no flags.
//
// The "-f /dev/null" is what makes the headless server actually isolated. tmux
// reads a configuration file when a server first starts, so without this flag
// the very first command on the headless socket loads the user's ~/.tmux.conf —
// and a config using tmux-resurrect/tmux-continuum (or any run-shell that
// creates sessions) then restores the user's entire workspace *into* the
// supposedly isolated server. Passing the flag on every command is harmless,
// because a running server does not re-read its config, and it removes any
// question of which command happens to start the server.
//
// The flag must go here, in the global prefix, and nowhere else: tmux overloads
// -f by position. "tmux -f /dev/null new-session" names a config file, but
// "new-session -f /dev/null" would parse /dev/null as the new session's *client
// flags*, and "split-window -f" is a full-width split.

// paneSeq is the creation ordinal of a pane, extracted from its tmux id
// ("%12" → 12). Policy may only COMPARE it; this is the one place that parses.
//
// It exists because three rules in helper_panes.go are about age: keep the
// OLDEST pane when two servers race for a slot, consider adoption candidates
// oldest-first, and break geometry ties deterministically. Every one of them
// used to parse "%12" itself, which is a tmux id in policy code.
//
// Sorting pane ids as strings is wrong in a way that only appears after a
// session has been running a while: "%10" sorts before "%9" lexically, so the
// "keep the lowest id" rules — which mean "keep the oldest pane", the one most
// likely to have the caller's process in it — would start preferring the newest
// pane once the window had passed ten panes. An unparseable id sorts last, so a
// pane we cannot rank can never win a tie-break by accident.
func paneSeq(paneID string) int {
	_, bare := parseTarget(paneID)
	n, err := strconv.Atoi(strings.TrimPrefix(bare, "%"))
	if err != nil {
		return math.MaxInt
	}
	return n
}

func socketArgs(socket string) []string {
	if socket == "" {
		return nil
	}
	return []string{"-L", socket, "-f", os.DevNull}
}

// run executes a tmux command and returns its combined output.
func (t *tmuxClient) run(ctx context.Context, args ...string) (string, error) {
	return t.runWithSocket(ctx, "", args...)
}

// runWithSocket executes a tmux command, routing it to the given socket.
// An empty socket uses the default tmux server.
func (t *tmuxClient) runWithSocket(ctx context.Context, socket string, args ...string) (string, error) {
	full := append(socketArgs(socket), args...)
	cmd := exec.CommandContext(ctx, "tmux", full...)
	out, err := cmd.Output()
	if err != nil {
		// Report against args, not full, so the subcommand name is args[0]
		// regardless of how many global flags the socket prefixed.
		subCmd := args[0]
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("tmux %s: %w: %s", subCmd, err, scrubIDs(strings.TrimSpace(string(exitErr.Stderr))))
		}
		return "", fmt.Errorf("tmux %s: %w", subCmd, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// idInText matches a multiplexer id embedded anywhere in a sentence: %3 for a
// pane, @2 for a window, $1 for a session. The leading group keeps it from
// firing inside an identifier or a path (mcp_pane%3 is not an id we wrote), and
// it is deliberately UNANCHORED, because every leak this scrub exists for is an
// id in the middle of a sentence rather than a whole value.
var idInText = regexp.MustCompile(`(^|[^A-Za-z0-9_])[%@$][0-9]+`)

// scrubIDs removes multiplexer ids from text that is about to become an error a
// caller can see. It is applied in runWithSocket, the single place tmux's own
// stderr enters this process, so every wrapped tmux error in the package flows
// through it.
//
// It is not cosmetic. tmux says "can't find pane %3", and that sentence reaches
// the model through closedPane.Detail and every NewToolResultErrorFromErr in
// the package — a path no response type covers and no schema check can see. The
// VALUE is replaced rather than the message dropped, because the rest of the
// sentence is the only diagnostic the caller gets: "can't find pane <pane>"
// still says what went wrong.
func scrubIDs(s string) string {
	return idInText.ReplaceAllString(s, "${1}<pane>")
}

// ListPanes returns panes in the given window with extended info.
func (t *tmuxClient) ListPanes(ctx context.Context, windowID string) ([]Pane, error) {
	socket, bareID := parseTarget(windowID)
	out, err := t.runWithSocket(ctx, socket, "list-panes",
		"-t", bareID,
		"-F", "#{pane_id}\t#{pane_title}\t#{pane_active}\t#{pane_width}\t#{pane_height}\t#{pane_current_command}\t#{pane_current_path}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []Pane{}, nil
	}

	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 7 {
			continue
		}
		width, _ := strconv.Atoi(parts[3])
		height, _ := strconv.Atoi(parts[4])
		panes = append(panes, Pane{
			ID:             prefix + parts[0],
			Title:          parts[1],
			Active:         parts[2] == "1",
			Width:          width,
			Height:         height,
			CurrentCommand: parts[5],
			CurrentPath:    parts[6],
		})
	}
	return panes, nil
}

// CapturePane returns the terminal content of a pane.
// lines <= 0 uses the pane's default history size.
// colors controls whether ANSI escape sequences are preserved.
func (t *tmuxClient) CapturePane(ctx context.Context, paneID string, lines int, colors bool) (string, error) {
	socket, bareID := parseTarget(paneID)
	args := []string{"capture-pane", "-t", bareID, "-p"}
	if lines > 0 {
		args = append(args, "-S", strconv.Itoa(-lines))
	}
	if colors {
		args = append(args, "-e")
	}
	return t.runWithSocket(ctx, socket, args...)
}

// markPaneOwned records that we created a pane, so that reuse and teardown can
// tell our panes from the user's. tmux itself stores no creator.
//
// The -p flag is what scopes an option to a single pane; paneOptWitness is the
// backstop for ever omitting it. See the constant's doc comment — without the
// witness, one session-scoped write would make every pane in the user's session
// look agent-owned.
//
// paneID must be a bare tmux ID (no "headless:" prefix), because it is compared
// against #{pane_id}, which tmux always reports bare.
func (t *tmuxClient) markPaneOwned(ctx context.Context, socket, paneID string) error {
	return t.markPaneOwnedAs(ctx, socket, paneID, ownerAgent, 0)
}

// markPaneOwnedAs records a pane in the registry under a given owner and slot.
// It is the ONE walker of markOrder, and the port calls it Claim.
//
// The order is not restated here: it is markOrder, as data, with the whole
// argument for it written on the variable. What matters at this call site is
// that the loop is the only way an option gets written, so a future edit that
// adds a fourth mark has to add it to the slice — where the prefix argument is —
// rather than as a fourth statement nobody re-derives the safety of.
//
// It returns on the FIRST failure, so a cancelled context or one transient tmux
// error leaves a prefix of markOrder on the pane. Callers that claim a pane the
// user owns must undo that prefix; see adoptCandidateLocked.
//
// slot <= 0 writes no slot marker at all rather than clearing one. A pane being
// claimed for the first time has nothing to clear, and an unconditional unset
// would put a third tmux call on every pane creation for no gain;
// setPaneSlot(id, 0) is the explicit way to remove a marker from a pane that has
// one.
//
// paneID must be a bare tmux ID (no "headless:" prefix). This differs from
// setPaneSlot, clearPaneRegistration and setPaneTitle, which take the prefixed
// ID and call parseTarget themselves — the split exists because this function's
// callers are mid-creation and already hold both halves, while the others are
// handed an ID from a response. It is a latent trap (a prefixed ID here would be
// written as a witness that can never equal #{pane_id}, producing a pane that is
// permanently invisible to the registry), which is why it is restated on all
// five functions rather than assumed.
func (t *tmuxClient) markPaneOwnedAs(ctx context.Context, socket, paneID, owner string, slot int) error {
	for _, opt := range markOrder {
		var value string
		switch opt {
		case paneOptWitness:
			value = paneID
		case paneOptOwner:
			value = owner
		case paneOptSlot:
			if slot <= 0 {
				continue
			}
			value = strconv.Itoa(slot)
		}
		if _, err := t.runWithSocket(ctx, socket,
			"set-option", "-p", "-t", paneID, opt, value); err != nil {
			return err
		}
	}
	return nil
}

// setPaneSlot sets or (slot <= 0) unsets the slot marker on an already-owned
// pane. It is the healing half of slot resolution — clearing the marker from the
// losers of a duplicate-slot race, and from a pane being released — and so it
// deliberately does not touch the witness or the owner: the pane stays ours, it
// just stops answering to a slot.
//
// set-option -p -u is idempotent (rc=0 even when the option was never set,
// verified on tmux 3.7b), which is what lets callers clear a marker without
// first reading whether there is one.
//
// paneID is the prefixed ID as responses report it; parseTarget routes it.
func (t *tmuxClient) setPaneSlot(ctx context.Context, paneID string, slot int) error {
	socket, bareID := parseTarget(paneID)
	if slot <= 0 {
		_, err := t.runWithSocket(ctx, socket,
			"set-option", "-p", "-u", "-t", bareID, paneOptSlot)
		return err
	}
	_, err := t.runWithSocket(ctx, socket,
		"set-option", "-p", "-t", bareID, paneOptSlot, strconv.Itoa(slot))
	return err
}

// clearPaneRegistration removes every marker, walking markOrder in REVERSE for
// the same reason markPaneOwnedAs walks it forwards: every prefix of the
// teardown leaves the pane less claimed than before, never more. Stopping
// halfway can only ever produce a pane we have given up on, never one we still
// steer.
//
// Deriving the order from the same slice rather than restating it is what makes
// that guarantee survive a fourth mark: reversing a hand-written list is a step
// a future edit can forget, and the forgetting looks like nothing.
//
// It is used when releasing an acquired pane. Clearing all three, rather than
// only the slot, is the truthful state: we no longer own the pane. Leaving
// @mcp_owner=acquired behind would retire the pane forever, because the
// acquisition predicate requires the owner option to be *unset* — the pane would
// be both unusable by us and, to the user, an ordinary shell that the agent
// mysteriously refuses to touch again.
//
// paneID is the prefixed ID as responses report it; parseTarget routes it.
func (t *tmuxClient) clearPaneRegistration(ctx context.Context, paneID string) error {
	socket, bareID := parseTarget(paneID)
	for i := len(markOrder) - 1; i >= 0; i-- {
		if _, err := t.runWithSocket(ctx, socket,
			"set-option", "-p", "-u", "-t", bareID, markOrder[i]); err != nil {
			return err
		}
	}
	return nil
}

// setPaneTitle labels a helper pane so the user can see which panes are the
// agent's ("agent", "agent:2", …).
//
// select-pane -T sets the title and returns without selecting: verified on tmux
// 3.7b, where the active pane was unchanged after titling an inactive one. That
// check matters as much as the -d flag on split-window above — stealing focus in
// order to write a label would move the user's cursor out of whatever they were
// typing, which is the precise interruption -d exists to prevent, and it would
// arrive from a purely cosmetic operation.
//
// paneID is the prefixed ID as responses report it; parseTarget routes it.
func (t *tmuxClient) setPaneTitle(ctx context.Context, paneID, title string) error {
	socket, bareID := parseTarget(paneID)
	_, err := t.runWithSocket(ctx, socket, "select-pane", "-t", bareID, "-T", title)
	return err
}

// SplitPane splits the given pane. direction is "horizontal" or "vertical".
// size is a percentage (0 means use tmux default of 50%).
// Returns a CreatedPane with the new pane ID and its parent window ID.
func (t *tmuxClient) SplitPane(ctx context.Context, paneID, direction string, size int) (*CreatedPane, error) {
	socket, bareID := parseTarget(paneID)
	// -d leaves the source pane active. Without it every split moves the user's
	// cursor into the pane the agent just made, interrupting whatever they were
	// typing — including, typically, their conversation with the agent.
	args := []string{"split-window", "-d", "-t", bareID, "-P", "-F", "#{pane_id}\t#{window_id}"}
	if direction == "horizontal" {
		args = append(args, "-h")
	}
	if size > 0 {
		// "-l N%" and not "-p N". They mean the same thing, but -p is the
		// deprecated spelling and modern tmux has dropped it: on tmux 3.4
		// (Ubuntu 24.04, and so CI) "split-window -p 50" fails with "size
		// missing", because the flag no longer takes a value and 50 is then
		// parsed as the shell command to run. -l accepts a percentage suffix
		// and has done since tmux 3.1, so it is the spelling that works on both.
		//
		// This was survivable while size was an argument callers rarely passed —
		// split-pane defaults it to 0, which appends neither flag. It stopped
		// being survivable when helper placement started requesting 50% for
		// every slot, which put the deprecated flag on the default path of every
		// resolution. Local tmux was new enough to accept it and CI was not,
		// which is exactly the split a version-sensitive flag produces.
		args = append(args, "-l", strconv.Itoa(size)+"%")
	}
	out, err := t.runWithSocket(ctx, socket, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 2 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	// The mark's error is deliberately dropped: a split that worked must not be
	// reported as a failure because its registry markers did not stick. The pane
	// is real and usable either way; what it loses is its record, so slot
	// resolution will not reuse it and close-pane will refuse to touch it — a
	// pane the user has to close by hand, which is a smaller harm than no pane at
	// all. It is not logged either: this server speaks JSON-RPC on stdout and
	// shares the host agent's stderr, where a stray line is noise in someone
	// else's log.
	_ = t.markPaneOwned(ctx, socket, parts[0])

	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	return &CreatedPane{
		PaneID:   prefix + parts[0],
		WindowID: prefix + parts[1],
	}, nil
}

// SendKeys sends keystrokes to a pane.
// When literal is true, -l flag is used so tmux treats input as literal text
// rather than interpreting key names like "Enter" or "C-c".
// When enter is true, a real Enter keystroke is appended after the keys.
func (t *tmuxClient) SendKeys(ctx context.Context, paneID, keys string, literal, enter bool) error {
	socket, bareID := parseTarget(paneID)
	args := []string{"send-keys", "-t", bareID}
	if literal {
		args = append(args, "-l")
	}
	args = append(args, keys)
	if _, err := t.runWithSocket(ctx, socket, args...); err != nil {
		return err
	}
	if enter {
		// Send Enter as a separate call so that the -l flag (if set above)
		// does not cause "Enter" to be treated as the five literal characters
		// E-n-t-e-r. This second call intentionally omits -l so tmux
		// interprets "Enter" as the Return key.
		if _, err := t.runWithSocket(ctx, socket, "send-keys", "-t", bareID, "Enter"); err != nil {
			return err
		}
	}
	return nil
}

// ExecuteCommand runs a shell command synchronously in the given pane.
// It wraps the command so output is tee'd to a temp file and the exit code is
// written to a separate file, then uses "tmux wait-for" to block until the
// command finishes. The context deadline controls the overall timeout.
func (t *tmuxClient) ExecuteCommand(ctx context.Context, paneID, command string) (*execOutcome, error) {
	socket, _ := parseTarget(paneID)

	id := uuid.New().String()
	outFile := fmt.Sprintf("/tmp/tmux-mcp-%s.out", id)
	exitFile := fmt.Sprintf("/tmp/tmux-mcp-%s.exit", id)
	waitChannel := "tmux-mcp-" + id

	defer os.Remove(outFile)
	defer os.Remove(exitFile)

	wrapped := t.wrapCommand(command, outFile, exitFile, waitChannel)

	// Send the wrapped command to the pane (literal=false so Enter sends a real Enter).
	if err := t.SendKeys(ctx, paneID, wrapped, false, true); err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}

	// Block until the command signals completion or the context is cancelled.
	// The wait-for must target the same tmux server that the pane lives on, and
	// goes through socketArgs because wait-for will itself start a server if
	// none is running. See socketArgs.
	waitArgs := append(socketArgs(socket), "wait-for", waitChannel)
	waitErr := exec.CommandContext(ctx, "tmux", waitArgs...).Run()
	if waitErr != nil {
		if ctx.Err() != nil {
			// Bug 2 fix: on timeout, return partial output instead of a bare error.
			partialOutput := ""
			if data, err := os.ReadFile(outFile); err == nil {
				partialOutput = strings.TrimRight(string(data), "\n")
			}
			return &execOutcome{
				Output:   partialOutput,
				ExitCode: -1,
				TimedOut: true,
			}, nil
		}
		return nil, fmt.Errorf("wait-for: %w", waitErr)
	}

	outputBytes, err := os.ReadFile(outFile)
	if err != nil {
		return nil, fmt.Errorf("read output file: %w", err)
	}

	// Bug 3 fix: retry reading exit file to handle race with fast-completing commands.
	var exitBytes []byte
	for attempt := 0; attempt < 4; attempt++ {
		exitBytes, err = os.ReadFile(exitFile)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read exit file: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("read exit file: %w", err)
	}

	exitCode, _ := strconv.Atoi(strings.TrimSpace(string(exitBytes)))

	return &execOutcome{
		Output:   strings.TrimRight(string(outputBytes), "\n"),
		ExitCode: exitCode,
	}, nil
}

// wrapCommand builds a shell command that:
//  1. Pipes stdout+stderr through tee into outFile (so it shows in the pane)
//  2. Captures the exit code into exitFile
//  3. Signals waitChannel via "tmux wait-for -S" when done
func (t *tmuxClient) wrapCommand(command, outFile, exitFile, waitChannel string) string {
	switch t.shellType {
	case "fish":
		// fish uses $pipestatus[1] (1-indexed) after a pipe, but pipestatus is
		// only set inside a pipeline. Capture exit status before the pipe instead.
		return fmt.Sprintf(
			`begin; %s; set _exit $status; end 2>&1 | tee %s; echo $_exit > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	case "zsh":
		return fmt.Sprintf(
			`{ %s } 2>&1 | tee %s; echo ${pipestatus[1]} > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	default: // bash
		return fmt.Sprintf(
			`{ %s; } 2>&1 | tee %s; echo ${PIPESTATUS[0]} > %s; tmux wait-for -S %s`,
			command, outFile, exitFile, waitChannel,
		)
	}
}

// KillPane kills a tmux pane.
func (t *tmuxClient) KillPane(ctx context.Context, paneID string) error {
	socket, bareID := parseTarget(paneID)
	_, err := t.runWithSocket(ctx, socket, "kill-pane", "-t", bareID)
	return err
}

// Bell reports the multiplexer's CURRENT bell indication for the pane's window.
//
// It is NOT read-and-clear, and the name is the trap: tmux does not clear
// #{window_bell_flag} when a client reads it, and neither does this. The bell
// trigger fires on every poll until something else clears the flag, which is
// what the code has always done — the contract is written down here so the next
// reader does not infer a consume from the verb.
//
// A tmux failure is reported as an error and never as "no bell": the trigger
// that owns the decision is the one place that knows a failed read must not be
// mistaken for a quiet pane.
func (t *tmuxClient) Bell(ctx context.Context, paneID string) (bool, error) {
	socket, bareID := parseTarget(paneID)
	out, err := t.runWithSocket(ctx, socket, "display-message", "-p", "-t", bareID,
		"#{window_bell_flag}")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "1", nil
}

// DisplayMessage shows a transient message in the tmux status bar.
// durationMs controls how long (in milliseconds) the message is shown.
func (t *tmuxClient) DisplayMessage(ctx context.Context, message string, durationMs int) error {
	_, err := t.run(ctx, "display-message", "-d", strconv.Itoa(durationMs), message)
	return err
}

// screen is one visual snapshot of a pane: what it is showing, and how big it
// is. It exists so the two tmux commands behind a screenshot stay on this side
// of the file boundary.
type screen struct {
	ANSI       string
	Cols, Rows int
}

// Screen takes the snapshot the screenshot renderer works from.
//
// It is BEST-EFFORT and explicitly NOT atomic: two tmux commands behind one
// method are still two commands, and a pane resized between them yields a
// capture measured against the wrong geometry. Saying so here is cheaper than
// implying an atomicity the tmux CLI cannot give.
//
// The failure behaviour is the renderer's, moved verbatim rather than
// redesigned: a dimension failure falls back to 80x24, because a screenshot at
// the wrong size beats no screenshot; a capture failure is fatal, because there
// is nothing left to draw. Keeping both here is what lets "-N" (preserve
// trailing whitespace, without which the layout collapses) stay a detail of this
// file instead of becoming a boolean flag every caller has to know about.
func (t *tmuxClient) Screen(ctx context.Context, paneID string) (screen, error) {
	cols, rows, err := t.GetPaneDimensions(ctx, paneID)
	if err != nil {
		cols, rows = 80, 24
	}
	ansi, err := t.CapturePaneRaw(ctx, paneID)
	if err != nil {
		return screen{}, err
	}
	return screen{ANSI: ansi, Cols: cols, Rows: rows}, nil
}

// CapturePaneRaw captures pane content with ANSI escape codes and preserved
// trailing whitespace. Unlike CapturePane, it always uses -e -p -N flags:
// -e preserves color/style escape codes, -p prints to stdout, -N preserves
// trailing spaces. This is intended for visual reproduction (screenshots)
// where whitespace layout matters.
func (t *tmuxClient) CapturePaneRaw(ctx context.Context, paneID string) (string, error) {
	socket, bareID := parseTarget(paneID)
	args := []string{"capture-pane", "-t", bareID, "-e", "-p", "-N"}
	return t.runWithSocket(ctx, socket, args...)
}

// GetPaneDimensions queries a pane's current width and height.
func (t *tmuxClient) GetPaneDimensions(ctx context.Context, paneID string) (cols, rows int, err error) {
	socket, bareID := parseTarget(paneID)
	out, err := t.runWithSocket(ctx, socket, "display-message",
		"-t", bareID, "-p", "#{pane_width}\t#{pane_height}")
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(strings.TrimSpace(out), "\t")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected display-message output: %q", out)
	}
	cols, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse pane width %q: %w", parts[0], err)
	}
	rows, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse pane height %q: %w", parts[1], err)
	}
	return cols, rows, nil
}

// paneRecord is one pane's registry entry, as read back from its tmux options.
// A zero Slot means "no slot marker"; slots start at 1.
type paneRecord struct {
	Ref   paneRef // the handle; the id inside it never leaves this file
	Owner string  // ownerAgent or ownerAcquired; never empty in a valid record
	Slot  int     // 0 when the pane carries no slot marker
	Dead  bool    // #{pane_dead}: the pane exists but its process has exited
	Seq   int     // creation ordinal — see paneSeq. Policy compares, never parses.
}

// registryFormat is the tmux format string both registry readers use. Keeping
// it in one place is what makes the witness impossible to drop from one of
// them: there is a single expression naming all three option variables, and it
// always names paneOptWitness alongside the other two.
func registryFormat() string {
	return fmt.Sprintf("#{pane_id}\t#{%s}\t#{%s}\t#{%s}\t#{pane_dead}",
		paneOptWitness, paneOptOwner, paneOptSlot)
}

// parseRegistryLine turns one line of registryFormat output into a record.
// ok is false when the line is malformed or when the pane carries no valid
// registry record. prefix is "headless:" for panes on the headless socket.
//
// The witness test lives here rather than in each caller for the reason given
// on paneOptWitness: a record is only a record when the pane says so about
// itself, in the same read.
//
// An unrecognised owner is treated as "not ours" rather than as an error. A
// later version may add owner kinds, and an old binary must never adopt, reuse
// or kill a pane whose ownership semantics it does not implement — forward
// compatibility here means doing nothing, not guessing.
func parseRegistryLine(line, prefix string) (paneRecord, bool) {
	// SplitN with the exact field count, not Split: an option that is unset
	// renders as an empty field, so a valid line can have empty middles and
	// must still parse. runWithSocket trims only "\n", never tabs, so the
	// field count is stable.
	parts := strings.SplitN(line, "\t", 5)
	if len(parts) != 5 {
		return paneRecord{}, false
	}
	paneID, witness, owner, slotField, dead := parts[0], parts[1], parts[2], parts[3], parts[4]
	if paneID == "" || witness != paneID {
		return paneRecord{}, false
	}
	if owner != ownerAgent && owner != ownerAcquired {
		return paneRecord{}, false
	}
	// A garbage or negative slot marker degrades to "unslotted agent pane"
	// rather than failing the read. Atoi's error is deliberately dropped: one
	// unparseable option value must not take down every resolution in the
	// window, and an owned pane with no usable slot is still a legitimate,
	// safely handled state.
	slot, _ := strconv.Atoi(strings.TrimSpace(slotField))
	if slot < 0 {
		slot = 0
	}
	return paneRecord{
		Ref:   newPaneRef(prefix + paneID),
		Owner: owner,
		Slot:  slot,
		Dead:  dead == "1",
		Seq:   paneSeq(paneID),
	}, true
}

// paneRegistryAround returns every pane in the window CONTAINING the given
// target that carries a valid registry record. One tmux call, five format
// variables.
//
// The target is a pane, not a window, and that is what lets a window id stay out
// of the port: "list-panes -t %5" enumerates %5's whole window, exactly as
// "list-panes -t @1" does (verified on tmux 3.7c). The scope is unchanged and
// still deliberate — helper panes belong beside the agent, in the window the
// user is looking at, and a resolution that could reach into their other windows
// could hand back a pane they are not watching, which from the agent's side is
// indistinguishable from a pane it made.
//
// A record counts only when the witness equals the pane's own ID and the owner
// is a value this binary recognises — see parseRegistryLine.
//
// Dead is carried because a pane whose process has exited still appears in
// list-panes when the user has remain-on-exit set. Such a pane accepts
// send-keys and silently swallows every keystroke, which is the worst possible
// failure for a helper pane: no error, no output, no clue.
func (t *tmuxClient) paneRegistryAround(ctx context.Context, target string) (map[paneRef]paneRecord, error) {
	socket, bareID := parseTarget(target)
	out, err := t.runWithSocket(ctx, socket, "list-panes", "-t", bareID, "-F", registryFormat())
	if err != nil {
		return nil, err
	}
	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	reg := make(map[paneRef]paneRecord)
	for _, line := range strings.Split(out, "\n") {
		rec, ok := parseRegistryLine(line, prefix)
		if !ok {
			continue
		}
		reg[rec.Ref] = rec
	}
	return reg, nil
}

// paneOwnerMark returns the raw @mcp_owner value a pane renders, whatever it is.
//
// This is deliberately NOT a registry read, and it is the one place in the file
// that is allowed not to be. Its question is the opposite of the registry's: not
// "is this pane ours?" — which requires the witness, because a leaked option
// could otherwise make the user's shells look like ours — but "is this pane
// completely unclaimed?", where every answer other than the empty string means
// "leave it alone".
//
// Reading it without the witness is therefore the conservative direction, and
// closes two holes the witnessed read cannot. A pane marked by a NEWER version
// of this binary carries an owner kind parseRegistryLine does not recognise, so
// it is absent from the registry and would otherwise look unclaimed and be
// adopted — exactly the forward-compatibility failure the registry comment warns
// against. And an @mcp_owner leaked to session scope makes every pane in the
// session answer here, which stops acquisition dead rather than enabling it:
// wrong in the direction of doing nothing.
func (t *tmuxClient) paneOwnerMark(ctx context.Context, paneID string) (string, error) {
	socket, bareID := parseTarget(paneID)
	out, err := t.runWithSocket(ctx, socket, "display-message", "-t", bareID, "-p",
		fmt.Sprintf("#{%s}", paneOptOwner))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ---- The isolated namespace ----

// Isolated panes are namespaced by the NAME of the session they live in, not by
// a pane option, and that is what makes the namespace isolated-only by
// construction rather than by convention.
//
// Three things follow, and each was a defect in the design this replaces:
//
//  1. There is no fourth mark, so markOrder is exactly what it has always been —
//     witness, owner, slot — with one walker. No path that claims a pane in the
//     USER'S window can write a namespace, because there is no namespace to
//     write. The earlier design made it a fourth pane option stamped by that
//     same single walker, which would have landed a namespace on every pane the
//     adopter touched, including the user's own shells.
//  2. Attribution is ATOMIC with creation. The session name is set by the same
//     new-session command that makes the pane, so there is no window in which a
//     live isolated pane exists that nobody can attribute. A pane option cannot
//     achieve that: it is always a second command, and a process that dies
//     between the two leaves a shell no tool can see or reach.
//  3. A session name cannot leak down the option scope chain. The witness exists
//     because pane options inherit when interpolated in a pane-context format,
//     so one set-option that forgets -p makes every pane in a session look
//     owned. A session has exactly one name and a pane belongs to exactly one
//     session, so "panes whose session name carries our prefix" is structurally
//     one-to-one and needs no witness to be trustworthy.
//
// The witness still guards the OWNER and SLOT marks on those panes, exactly as
// it does in the user's window, because those marks are still pane options.
func (b *tmuxBackend) namespacePrefix() string { return "mcp-" + b.nsKey + "-" }

// newNamespaceKey identifies this server among the several that share the
// isolated socket. It is "<pid>-<nonce>", fixed at construction.
//
// It does NOT read $TMUX_PANE, which is the obvious alternative. Two reasons,
// and the second is the one that decides it. A pane id does not identify a tmux
// server, so the same %N is reissued by a restarted or a different server and
// the key collides anyway — it is not a better key. And reading TMUX_PANE here
// would add a second reader of "which pane am I", which is exactly the closed
// set Invariant S exists to keep closed; a namespace generator is a poor reason
// to widen it. TestOnlyPolicyCodeKnowsOurOwnPane is what keeps that set closed.
//
// The nonce is what makes inheritance impossible. A pid alone is reusable: a
// server that restarts can be handed a dead server's pid, inherit its slots, and
// then close-pane("all") kills panes it never created while a send-keys to
// slot 1 types into another session's REPL. Orphaning — old panes nobody
// reclaims — is a resource leak; inheritance is a wrong-pane write. The nonce
// trades the second for the first, which is the safe direction, and it keeps the
// pid in the name so reapOrphanedNamespaces can ask whether that process is
// still alive.
func newNamespaceKey() string {
	return fmt.Sprintf("%d-%s", os.Getpid(), strings.SplitN(uuid.New().String(), "-", 2)[0])
}

// namespaceReapTimeout bounds the startup sweep. It is short because the sweep
// runs before the server answers its first request and must never be the reason
// a session takes visibly long to start; an orphan that survives one start is
// reaped by the next.
const namespaceReapTimeout = 3 * time.Second

// reapOrphanedNamespaces closes isolated namespaces whose server process is
// gone. It runs once, at construction.
//
// An isolated namespace outlives the server that made it: a crashed or killed
// server leaves live shells in a named session that nothing reclaims until the
// machine reboots. Nobody can see them — there is no window — and no tool can
// reach them, because the nonce in the name means no later server inherits the
// namespace.
//
// It fails safe in the only direction that matters. A live server's pid is by
// definition running, so a live namespace is never reaped. The residual risk is
// pid REUSE by an unrelated process, which can only make the reaper DECLINE and
// leave the orphan — the same outcome as not reaping at all.
//
// This is what CleanStaleHeadlessSocket used to do, minus the kill-server that
// made it dangerous: that ran list-sessions on the shared socket and, if it
// failed, killed the server — and the window in which list-sessions fails is
// exactly the window in which a neighbour is starting one. Under a design whose
// premise is several servers sharing this socket, that is the routine case.
// Nothing in this file runs kill-server.
func (b *tmuxBackend) reapOrphanedNamespaces() {
	ctx, cancel := context.WithTimeout(context.Background(), namespaceReapTimeout)
	defer cancel()

	// A failure here means "no server on that socket", which is the ordinary
	// case on a machine that has not used an isolated slot yet. It is not
	// reported: this server speaks JSON-RPC on stdout and shares the host
	// agent's stderr, where a stray line is noise in someone else's log.
	out, err := b.c.runWithSocket(ctx, headlessSocket, "list-sessions", "-F", "#{session_name}")
	if err != nil || out == "" {
		return
	}
	for _, name := range strings.Split(out, "\n") {
		pid, ok := namespacePID(name)
		if !ok || processIsRunning(pid) {
			continue
		}
		_, _ = b.c.runWithSocket(ctx, headlessSocket, "kill-session", "-t", name)
	}
}

// namespacePID extracts the pid from a session name this package wrote —
// "mcp-<pid>-<nonce>-<uuid>". ok is false for any name that is not ours,
// including a session some other program happens to have created on this socket:
// an unrecognised name is left alone, because the only thing this file knows
// about a name it did not write is that it must not touch it.
func namespacePID(session string) (int, bool) {
	rest, ok := strings.CutPrefix(session, "mcp-")
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok || digits == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(digits)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processIsRunning reports whether a pid names a live process.
//
// Signal 0 is the portable "does this exist?" probe: it performs the permission
// check and delivers nothing. A permission error is an ANSWER, not a failure —
// the process exists and belongs to someone else — and it is reported as alive,
// which is the safe direction for every caller here: the cost of a false "alive"
// is one orphan left behind, and the cost of a false "dead" is killing a live
// server's panes.
func processIsRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM)
}

// openIsolatedSession creates one detached session on the isolated socket whose
// NAME carries this server's namespace, and returns the pane inside it.
//
// It writes NO registry marks. Claiming is markPaneOwnedAs's job and there must
// be one walker of markOrder: the session creator this replaces wrote
// witness → owner before returning, which produced the total order
// witness → owner → … → slot from two different writers and made the prefix
// argument on markOrder unanalysable.
//
// The pane id comes back prefixed, so one paneRef addresses either server and
// parseTarget routes every later command to the right socket.
func (t *tmuxClient) openIsolatedSession(ctx context.Context, session string) (string, error) {
	out, err := t.runWithSocket(ctx, headlessSocket,
		"new-session", "-d",
		"-s", session,
		"-x", strconv.Itoa(detachedWidth),
		"-y", strconv.Itoa(detachedHeight),
		"-P", "-F", "#{pane_id}")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("tmux new-session reported no pane for session %q", session)
	}
	return headlessPrefix + out, nil
}

// isolatedFormat prefixes registryFormat with the session name, so one
// list-panes answers both questions this file asks of that socket: which panes
// are in our namespace, and which of those carry a registry record.
func isolatedFormat() string {
	return "#{session_name}\t" + registryFormat()
}

// scanNamespace reads every pane in this server's namespace in ONE tmux call,
// and returns both views of it.
//
// panes is EVERY pane in the namespace, marked or not: on this socket every pane
// in our namespace is ours by construction — the same argument that abolishes
// adoption there — so a pane whose marks are missing or partial is still ours to
// close. Without that view, a process that died between creating a pane and
// claiming it leaves a live shell no tool can see or reach.
//
// records is the witnessed subset, parsed exactly as the visible registry is:
// the same registryFormat, the same parseRegistryLine, the same rejection of a
// line whose witness is not the pane's own id. There is one format expression in
// this file naming the three options, which is what makes the witness impossible
// to drop from one reader and not the other.
func (b *tmuxBackend) scanNamespace(ctx context.Context) ([]paneRef, map[paneRef]paneRecord, error) {
	out, err := b.c.runWithSocket(ctx, headlessSocket, "list-panes", "-a", "-F", isolatedFormat())
	if err != nil {
		// "No server running on that socket" is not a failure: it is what "this
		// agent has opened no invisible panes" looks like, and it is the state
		// every machine starts in. tmux reports it as an ordinary command
		// failure, indistinguishable at this layer from a transient one.
		//
		// So EVERY failure is read as "empty", and the alternative is worse in a
		// way that is easy to miss: an unstated kind consults this registry on
		// its way past, so returning an error here would fail a plain
		// send-keys — the most common call this server serves — on any machine
		// that has never used an isolated slot.
		//
		// Reading a transient failure as "empty" costs a duplicate pane and
		// nothing else, and the duplicate repairs itself: the next successful
		// read finds two panes claiming one slot, keeps the oldest and CLOSES the
		// other, and close-pane({slot:"all"}) sweeps the namespace whatever its
		// marks say.
		return nil, map[paneRef]paneRecord{}, nil
	}
	prefix := b.namespacePrefix()
	var panes []paneRef
	records := map[paneRef]paneRecord{}
	for _, line := range strings.Split(out, "\n") {
		session, rest, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(session, prefix) {
			continue
		}
		bareID, _, _ := strings.Cut(rest, "\t")
		if bareID == "" {
			continue
		}
		panes = append(panes, newPaneRef(headlessPrefix+bareID))
		if rec, ok := parseRegistryLine(rest, headlessPrefix); ok {
			records[rec.Ref] = rec
		}
	}
	return panes, records, nil
}

// ownedPanesInWindow returns the panes in the window that we *created*, keyed by
// the same (possibly "headless:"-prefixed) ID that ListPanes reports.
//
// This is deliberately narrower than paneRegistryAround: it excludes panes
// with owner ownerAcquired. Its only caller is findIdlePaneInWindow, which backs
// split-pane's reuse, and split-pane's contract is that a pane reported
// "reused": true was created by the server and is therefore safe to kill. An
// acquired pane is one the *user* opened; widening this set would silently
// convert "reused": true into a licence to kill a pane the user is using, which
// is the one unrecoverable half of the whole hazard.
//
// Slot resolution needs both owner kinds and reaches the same read through the
// port's Records. The two callers want genuinely different sets, so they get two
// functions rather than one function with a flag.
//
// Dead panes are deliberately NOT filtered here even though the record now
// carries the flag. A dead pane reaches GetPaneState today, which reports
// IsAlive: false, and paneIsIdleShell rejects it; filtering earlier would be
// equivalent *today* but is still a behaviour change to a path with existing
// tests. The Dead flag is for the new resolver, which has no GetPaneState call
// to lean on.
func (t *tmuxClient) ownedPanesInWindow(ctx context.Context, windowID string) (map[string]bool, error) {
	reg, err := t.paneRegistryAround(ctx, windowID)
	if err != nil {
		return nil, err
	}
	owned := make(map[string]bool, len(reg))
	for ref, rec := range reg {
		if rec.Owner == ownerAgent {
			owned[ref.target()] = true
		}
	}
	return owned, nil
}

// GetPaneState returns the native OS-level state of the process in a tmux pane.
// It queries tmux for the pane's PID, dead flag, and dead status in a single
// call, then uses OS-specific inspection to determine whether the foreground
// process is alive and waiting for input.
func (t *tmuxClient) GetPaneState(ctx context.Context, paneID string) (*PaneState, error) {
	socket, bareID := parseTarget(paneID)
	// Query pid, dead flag, and dead exit status in a single tmux call.
	out, err := t.runWithSocket(ctx, socket, "display-message", "-p", "-t", bareID,
		"#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}")
	if err != nil {
		return nil, fmt.Errorf("get pane state: %w", err)
	}

	// run() already strips trailing newlines; split on tab to get the three fields.
	// Do NOT TrimSpace the whole string since the trailing tab (empty dead_status)
	// would be stripped, yielding only 2 fields instead of 3.
	parts := strings.SplitN(out, "\t", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("unexpected tmux output for pane state: %q", out)
	}

	pidStr := strings.TrimSpace(parts[0])
	deadFlag := strings.TrimSpace(parts[1])
	deadStatusStr := strings.TrimSpace(parts[2])

	// If the pane PID is empty the pane does not exist. This is a SENTINEL and
	// not a sentence, because the adapter cannot write the sentence: the caller
	// is the only party that knows the slot number the user should be told
	// about, and "pane %3 does not exist" is an id in the model's context.
	if pidStr == "" {
		return nil, errPaneGone
	}

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return nil, fmt.Errorf("parse pane pid %q: %w", pidStr, err)
	}

	// If the pane is dead, return immediately without OS-level process inspection.
	if deadFlag == "1" {
		exitCode, _ := strconv.Atoi(deadStatusStr)
		return &PaneState{
			PanePID:  pid,
			IsAlive:  false,
			ExitCode: exitCode,
		}, nil
	}

	state := &PaneState{PanePID: pid}
	if err := fillPaneState(ctx, state); err != nil {
		// Treat inspection errors as "alive, no input-wait detected" rather
		// than hard failures — the pane is still usable even if we cannot
		// determine the precise state.
		state.IsAlive = true
	}
	return state, nil
}

// paneIsIdleShell reports whether a pane is an idle shell at its prompt: alive,
// with a shell as the foreground process and no child command running. The
// "no child" part is the foreground PID matching the pane's own (shell) PID —
// when a command like `cat` or `yes` runs, it becomes the foreground process
// and ForegroundPID diverges from PanePID. This signal is reliable on both
// macOS and Linux, unlike PaneState.WaitingForInput.
func paneIsIdleShell(state *PaneState) bool {
	if state == nil || !state.IsAlive {
		return false
	}
	if !isShellProcess(state.ForegroundCmd) {
		return false
	}
	// The shell must be the foreground process (no child command running).
	return state.ForegroundPID == state.PanePID
}

// isShellProcess returns true if the command name is a known shell.
func isShellProcess(cmd string) bool {
	switch cmd {
	case "zsh", "-zsh", "bash", "-bash", "fish", "sh", "-sh",
		"dash", "ksh", "-ksh", "csh", "-csh", "tcsh", "-tcsh":
		return true
	}
	return false
}

// ---- The port ----
//
// Everything below is the boundary between policy and the multiplexer. Policy
// holds a Backend and nothing else; this file is the only implementation, and
// the only code in the package that knows what a "%" is.

// splitAxis is which way a new pane goes relative to its anchor. It is typed
// because "horizontal" and "vertical" are tmux's words for it, and a second
// multiplexer would have its own.
type splitAxis int

const (
	splitBeside splitAxis = iota // side by side
	splitBelow                   // stacked
)

// tmuxDirection is the only place the two words are spelled.
func (a splitAxis) tmuxDirection() string {
	if a == splitBeside {
		return "horizontal"
	}
	return "vertical"
}

// placement is the server-decided geometry for a new visible pane. The agent
// never passes any of it — placementForSlot decides — and this is how that
// decision crosses the seam.
type placement struct {
	Anchor      paneRef
	Axis        splitAxis
	SizePercent int // 0 means the backend's default
}

// paneInfo is geometry and label, for the two decisions that need them: which
// pane has the most room to give up, and what order to consider adoption
// candidates in. Seq is the same creation ordinal paneRecord carries, so both
// discovery views rank panes the same way.
type paneInfo struct {
	Ref           paneRef
	Title         string
	Width, Height int
	Seq           int
}

// execOutcome is what running one command in a pane produced.
type execOutcome struct {
	Output   string
	ExitCode int
	TimedOut bool
}

// Backend is the port. Policy calls only this; only this file implements it.
//
// It is one interface rather than four because a resolution needs identity,
// discovery, registry writes and IO inside a single slotMu hold: segregating
// them would move the fan-in out one level and buy nothing while there is one
// consumer and one implementation.
type Backend interface {
	// ---- Identity ----

	// Self is the pane this server runs in, and is the ONLY reader of TMUX_PANE
	// in the package (Invariant S). It returns errNoWindow when there is none.
	Self(ctx context.Context) (paneRef, error)

	// ---- Structure ----

	OpenBeside(ctx context.Context, place placement) (paneRef, error)

	// OpenIsolated creates a pane with no window, inside a namespace that
	// belongs to this server. It writes no registry marks: Claim is the single
	// mark writer, and a second writer is what made the previous design's write
	// order unanalysable.
	//
	// It returns a non-empty ref whenever the pane was created, even if a later
	// step of creation failed, so the caller can always clean up what exists.
	OpenIsolated(ctx context.Context) (paneRef, error)

	Close(ctx context.Context, p paneRef) error

	// ---- Discovery ----

	// Siblings lists every pane sharing a window with the given one, geometry
	// included. A failure here DEGRADES rather than being fatal — no adoption,
	// and the anchor falls back to self — which is why it is separate from
	// Records, whose failure is fatal to a resolution.
	Siblings(ctx context.Context, of paneRef) ([]paneInfo, error)

	// Records is the witnessed registry of the window around the given pane.
	Records(ctx context.Context, of paneRef) (map[paneRef]paneRecord, error)

	// IsolatedRecords is the witnessed registry of this server's isolated
	// namespace — the same question Records answers, asked of the panes nobody
	// can see.
	IsolatedRecords(ctx context.Context) (map[paneRef]paneRecord, error)

	// IsolatedPanes returns EVERY pane in this server's namespace, marked or
	// not. It is the reclamation view: on that socket every pane in our
	// namespace is ours by construction, so a pane whose marks are missing or
	// partial is still ours to close. Without it, a process that died between
	// creating a pane and claiming it leaves a live shell no tool can reach.
	IsolatedPanes(ctx context.Context) ([]paneRef, error)

	// OwnerMark asks the OPPOSITE question from Records and is deliberately not
	// witnessed: not "is this pane ours?" but "is it wholly unclaimed?", where
	// every answer other than the empty string means hands off. See
	// paneOwnerMark's comment; the inversion is load-bearing.
	OwnerMark(ctx context.Context, p paneRef) (string, error)

	// ---- Registry writes ----

	// Claim writes the ownership marks in the order the backend chooses.
	Claim(ctx context.Context, p paneRef, owner string, slot int) error

	// SetSlot sets or (slot <= 0) unsets ONLY the slot marker on an
	// already-claimed pane. The healing half of resolution: the pane stays ours,
	// it stops answering to a slot.
	SetSlot(ctx context.Context, p paneRef, slot int) error

	ClearMarks(ctx context.Context, p paneRef) error

	// ---- IO ----

	SendKeys(ctx context.Context, p paneRef, keys string, literal, enter bool) error
	Capture(ctx context.Context, p paneRef, lines int, colors bool) (string, error)
	Screen(ctx context.Context, p paneRef) (screen, error)
	Exec(ctx context.Context, p paneRef, command string) (*execOutcome, error)

	// Foreground is the OS-level state of the pane's process.
	Foreground(ctx context.Context, p paneRef) (*PaneState, error)

	// Bell reports the backend's CURRENT bell indication for the pane. It is not
	// read-and-clear — see the implementation.
	Bell(ctx context.Context, p paneRef) (bool, error)

	// ---- UI ----

	SetTitle(ctx context.Context, p paneRef, title string) error
	Notify(ctx context.Context, message string, d time.Duration) error
}

// tmuxBackend is the tmux implementation of Backend.
//
// It wraps tmuxClient rather than being it, and the split is the point: the
// client's methods take strings, because they are a thin transcription of a CLI
// that takes strings, while the port takes handles. One type carrying both
// would be two overlapping APIs on one receiver, which is exactly the ambiguity
// the seam exists to remove — a policy author would have a stringly-typed
// escape hatch on the same value.
type tmuxBackend struct {
	c *tmuxClient

	// nsKey is this server's isolated namespace, fixed for the process
	// lifetime. See newNamespaceKey for why it is "<pid>-<nonce>" and why it
	// does not read TMUX_PANE.
	nsKey string
}

// newTmuxBackend fixes the namespace and reclaims the orphans of servers that
// are gone. Both happen HERE, once, rather than lazily on first use: a namespace
// that could change mid-process would make close-pane("all") sweep a different
// set than the one it created, and a reaper that ran on first use would run
// while another resolution held the slot lock.
func newTmuxBackend(c *tmuxClient) *tmuxBackend {
	b := &tmuxBackend{c: c, nsKey: newNamespaceKey()}
	b.reapOrphanedNamespaces()
	return b
}

// Compile-time proof that the adapter still satisfies the port. Without it, a
// method whose signature drifts produces an error at the construction site in
// main.go, several files away from the change that caused it.
var _ Backend = (*tmuxBackend)(nil)

// Self reads TMUX_PANE, and is the ONLY place in the package that does.
//
// The value is inherited from the environment at spawn and is stable for the
// process lifetime, which is the entire point: it cannot race with the user
// switching windows, panes or sessions, unlike any query of tmux's "active"
// pane, which answers a question about the user's cursor rather than about this
// process. A resolution that consulted the active pane would place the agent's
// helper next to wherever the user happened to be looking at that instant.
//
// It is also not a guess: every agent runtime that starts this server starts it
// inside the pane the user is looking at, so an empty answer means something
// specific and reportable — "this process is not in tmux" — rather than "lookup
// failed", which is why the caller can turn it into errNoWindow instead of a
// fallback.
//
// Reading it here rather than at each call site is deliberate: the set of
// callers permitted to ask "which pane am I?" is a safety property (a pane the
// agent may split, never a pane the agent may type into), and a single named
// accessor is what makes that set auditable — see
// TestOnlyPolicyCodeKnowsOurOwnPane.
func (b *tmuxBackend) Self(_ context.Context) (paneRef, error) {
	id := os.Getenv("TMUX_PANE")
	if id == "" {
		return paneRef{}, errNoWindow
	}
	return newPaneRef(id), nil
}

func (b *tmuxBackend) OpenBeside(ctx context.Context, place placement) (paneRef, error) {
	cp, err := b.c.SplitPane(ctx, place.Anchor.target(), place.Axis.tmuxDirection(), place.SizePercent)
	if err != nil {
		return paneRef{}, err
	}
	return newPaneRef(cp.PaneID), nil
}

// OpenIsolated names the session after the namespace and a fresh uuid, so two
// isolated slots in one namespace are two sessions rather than two panes in one:
// killing the last pane of a session ends that session, and tmux exits the
// server by itself when its last session goes — which is how the isolated server
// is reclaimed without anyone running kill-server on a socket other servers
// share.
func (b *tmuxBackend) OpenIsolated(ctx context.Context) (paneRef, error) {
	id, err := b.c.openIsolatedSession(ctx, b.namespacePrefix()+uuid.New().String())
	if err != nil {
		return paneRef{}, err
	}
	return newPaneRef(id), nil
}

func (b *tmuxBackend) Close(ctx context.Context, p paneRef) error {
	return b.c.KillPane(ctx, p.target())
}

func (b *tmuxBackend) Siblings(ctx context.Context, of paneRef) ([]paneInfo, error) {
	// A pane target resolves to its window, so the window id never reaches the
	// port — see paneRegistryAround for the verification.
	panes, err := b.c.ListPanes(ctx, of.target())
	if err != nil {
		return nil, err
	}
	infos := make([]paneInfo, 0, len(panes))
	for _, p := range panes {
		infos = append(infos, paneInfo{
			Ref:    newPaneRef(p.ID),
			Title:  p.Title,
			Width:  p.Width,
			Height: p.Height,
			Seq:    paneSeq(p.ID),
		})
	}
	return infos, nil
}

func (b *tmuxBackend) Records(ctx context.Context, of paneRef) (map[paneRef]paneRecord, error) {
	return b.c.paneRegistryAround(ctx, of.target())
}

func (b *tmuxBackend) IsolatedRecords(ctx context.Context) (map[paneRef]paneRecord, error) {
	_, records, err := b.scanNamespace(ctx)
	return records, err
}

func (b *tmuxBackend) IsolatedPanes(ctx context.Context) ([]paneRef, error) {
	panes, _, err := b.scanNamespace(ctx)
	return panes, err
}

func (b *tmuxBackend) OwnerMark(ctx context.Context, p paneRef) (string, error) {
	return b.c.paneOwnerMark(ctx, p.target())
}

// Claim splits the handle back into socket and bare id, which is the step that
// used to happen in policy: markPaneOwnedAs takes them separately because its
// original callers are mid-creation and hold both.
func (b *tmuxBackend) Claim(ctx context.Context, p paneRef, owner string, slot int) error {
	socket, bareID := parseTarget(p.target())
	return b.c.markPaneOwnedAs(ctx, socket, bareID, owner, slot)
}

func (b *tmuxBackend) SetSlot(ctx context.Context, p paneRef, slot int) error {
	return b.c.setPaneSlot(ctx, p.target(), slot)
}

func (b *tmuxBackend) ClearMarks(ctx context.Context, p paneRef) error {
	return b.c.clearPaneRegistration(ctx, p.target())
}

func (b *tmuxBackend) SendKeys(ctx context.Context, p paneRef, keys string, literal, enter bool) error {
	return b.c.SendKeys(ctx, p.target(), keys, literal, enter)
}

func (b *tmuxBackend) Capture(ctx context.Context, p paneRef, lines int, colors bool) (string, error) {
	return b.c.CapturePane(ctx, p.target(), lines, colors)
}

func (b *tmuxBackend) Screen(ctx context.Context, p paneRef) (screen, error) {
	return b.c.Screen(ctx, p.target())
}

func (b *tmuxBackend) Exec(ctx context.Context, p paneRef, command string) (*execOutcome, error) {
	// No translation left to do: ExecuteCommand answers in execOutcome directly
	// now that CommandResult is gone. That type carried a paneId json tag — the
	// last one in the package — and the wire it served no longer exists.
	return b.c.ExecuteCommand(ctx, p.target(), command)
}

func (b *tmuxBackend) Foreground(ctx context.Context, p paneRef) (*PaneState, error) {
	return b.c.GetPaneState(ctx, p.target())
}

func (b *tmuxBackend) Bell(ctx context.Context, p paneRef) (bool, error) {
	return b.c.Bell(ctx, p.target())
}

func (b *tmuxBackend) SetTitle(ctx context.Context, p paneRef, title string) error {
	return b.c.setPaneTitle(ctx, p.target(), title)
}

func (b *tmuxBackend) Notify(ctx context.Context, message string, d time.Duration) error {
	return b.c.DisplayMessage(ctx, message, int(d.Milliseconds()))
}
