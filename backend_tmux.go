package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session represents a tmux session.
type Session struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached bool   `json:"attached"`
}

// Window represents a tmux window.
type Window struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Panes  int    `json:"panes"`
}

// Pane represents a tmux pane with extended info.
type Pane struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Active         bool   `json:"active"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	CurrentCommand string `json:"currentCommand"`
	CurrentPath    string `json:"currentPath"`
}

// CommandResult holds the output and exit code of a completed command.
//
// Slot and Created are set only when the caller let the server choose the pane;
// both are omitempty, so a call that named a paneId gets exactly the object it
// always got.
type CommandResult struct {
	PaneID   string `json:"paneId"`
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut,omitempty"`
	Slot     int    `json:"slot,omitempty"`
	Created  bool   `json:"created,omitempty"`
}

// CreatedSession holds the IDs returned when a new session is created.
type CreatedSession struct {
	SessionID   string `json:"sessionId"`
	SessionName string `json:"sessionName"`
	WindowID    string `json:"windowId"`
	PaneID      string `json:"paneId"`
}

// CreatedWindow holds the IDs returned when a new window is created.
type CreatedWindow struct {
	WindowID   string `json:"windowId"`
	WindowName string `json:"windowName"`
	PaneID     string `json:"paneId"`
}

// CreatedPane holds the IDs returned when a new pane is created.
//
// Reused and Created say almost the same thing from opposite ends, and both are
// kept on purpose. Reused predates slots and some consumer somewhere keys on it,
// so the slot path sets Reused = !Created rather than leaving it absent; Created
// is the field the rest of the slot-aware tools use, and it means "new to this
// slot", which on this tool is also "new pane".
type CreatedPane struct {
	PaneID   string `json:"paneId"`
	WindowID string `json:"windowId"`
	Reused   bool   `json:"reused,omitempty"`
	Slot     int    `json:"slot,omitempty"`
	Created  bool   `json:"created,omitempty"`
}

// headlessSocket is the tmux socket name used for isolated headless sessions.
const headlessSocket = "mcp-headless"

// Size given to sessions we create detached. tmux would otherwise use
// default-size (80x24), which wraps long output lines. See createSessionOnSocket.
const (
	detachedWidth  = 200
	detachedHeight = 50
)

// CleanStaleHeadlessSocket checks if the headless tmux socket exists but the
// server behind it is dead (e.g. after a crash). If so, it runs kill-server to
// clean up the stale socket so a fresh server can be started.
func CleanStaleHeadlessSocket() {
	// Try to list sessions on the headless socket. If the server is alive this
	// succeeds (or returns "no sessions"). If the socket is stale, tmux returns
	// an error like "no server running on ...".
	//
	// Both commands go through socketArgs because either can be the one that
	// starts the server, and a server started without -f /dev/null would load
	// the user's config. See socketArgs.
	cmd := exec.Command("tmux", append(socketArgs(headlessSocket), "list-sessions")...)
	if err := cmd.Run(); err != nil {
		// Server is not running — kill-server will clean up any stale socket file.
		_ = exec.Command("tmux", append(socketArgs(headlessSocket), "kill-server")...).Run()
	}
}

// headlessPrefix is the ID prefix that identifies headless targets.
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
// paneOptSlot or paneOptOwner goes through paneRegistryInWindow (a window) or
// paneRecordFor (a single pane), and both fetch paneOptWitness in the *same*
// tmux call and reject the record unless it equals the pane's own ID. Two
// readers, both here, both next to this comment — so "did this new read keep
// the witness?" is a question with only two places to look rather than one per
// caller.
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
			return "", fmt.Errorf("tmux %s: %w: %s", subCmd, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("tmux %s: %w", subCmd, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// ListSessions returns all active tmux sessions on the default server.
func (t *tmuxClient) ListSessions(ctx context.Context) ([]Session, error) {
	return t.listSessionsOnSocket(ctx, "")
}

// ListHeadlessSessions returns all active sessions on the headless server.
// IDs in the returned sessions are prefixed with "headless:".
func (t *tmuxClient) ListHeadlessSessions(ctx context.Context) ([]Session, error) {
	sessions, err := t.listSessionsOnSocket(ctx, headlessSocket)
	if err != nil {
		return sessions, err
	}
	for i := range sessions {
		sessions[i].ID = headlessPrefix + sessions[i].ID
	}
	return sessions, nil
}

// listSessionsOnSocket lists sessions on the given tmux socket (empty = default).
func (t *tmuxClient) listSessionsOnSocket(ctx context.Context, socket string) ([]Session, error) {
	out, err := t.runWithSocket(ctx, socket, "list-sessions",
		"-F", "#{session_id}\t#{session_name}\t#{session_windows}\t#{session_attached}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") ||
			strings.Contains(err.Error(), "no sessions") {
			return []Session{}, nil
		}
		return nil, err
	}
	if out == "" {
		return []Session{}, nil
	}

	var sessions []Session
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		windows, _ := strconv.Atoi(parts[2])
		sessions = append(sessions, Session{
			ID:       parts[0],
			Name:     parts[1],
			Windows:  windows,
			Attached: parts[3] != "0",
		})
	}
	return sessions, nil
}

// ListWindows returns windows in the given session.
func (t *tmuxClient) ListWindows(ctx context.Context, sessionID string) ([]Window, error) {
	socket, bareID := parseTarget(sessionID)
	out, err := t.runWithSocket(ctx, socket, "list-windows",
		"-t", bareID,
		"-F", "#{window_id}\t#{window_name}\t#{window_active}\t#{window_panes}")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []Window{}, nil
	}

	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	var windows []Window
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			continue
		}
		panes, _ := strconv.Atoi(parts[3])
		windows = append(windows, Window{
			ID:     prefix + parts[0],
			Name:   parts[1],
			Active: parts[2] == "1",
			Panes:  panes,
		})
	}
	return windows, nil
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

// CreateSession creates a new detached tmux session.
// Returns a CreatedSession with the session, window, and pane IDs.
func (t *tmuxClient) CreateSession(ctx context.Context, name string) (*CreatedSession, error) {
	return t.createSessionOnSocket(ctx, "", name, "")
}

// CreateHeadlessSession creates a new detached session on the headless tmux
// server. The returned IDs are prefixed with "headless:" so all subsequent
// tool calls are routed to the correct socket automatically.
func (t *tmuxClient) CreateHeadlessSession(ctx context.Context, name, command string) (*CreatedSession, error) {
	sess, err := t.createSessionOnSocket(ctx, headlessSocket, name, command)
	if err != nil {
		return nil, err
	}
	sess.SessionID = headlessPrefix + sess.SessionID
	sess.WindowID = headlessPrefix + sess.WindowID
	sess.PaneID = headlessPrefix + sess.PaneID
	return sess, nil
}

// createSessionOnSocket is the shared implementation for session creation.
// socket selects the tmux server; empty means the default server.
// command, if non-empty, is passed to the shell as the initial command.
// If a session with the given name already exists, it returns the existing
// session's IDs instead of erroring (idempotent create).
func (t *tmuxClient) createSessionOnSocket(ctx context.Context, socket, name, command string) (*CreatedSession, error) {
	// If a name was given, check if a session with that name already exists.
	if name != "" {
		if existing, err := t.findSessionByName(ctx, socket, name); err == nil {
			return existing, nil
		}
	}

	// A detached session has no client to take its size from, so tmux falls back
	// to default-size (80x24). At 80 columns a dev server's output wraps, which
	// silently breaks readiness regexes that expect a match on one line. Ask for
	// a size that behaves like a real terminal instead.
	args := []string{
		"new-session", "-d",
		"-x", strconv.Itoa(detachedWidth),
		"-y", strconv.Itoa(detachedHeight),
		"-P", "-F", "#{session_id}\t#{session_name}\t#{window_id}\t#{pane_id}",
	}
	if name != "" {
		args = append(args, "-s", name)
	}
	if command != "" {
		args = append(args, command)
	}
	out, err := t.runWithSocket(ctx, socket, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	created := &CreatedSession{
		SessionID:   parts[0],
		SessionName: parts[1],
		WindowID:    parts[2],
		PaneID:      parts[3],
	}
	// We made this pane, so claim it. Failure is not fatal: the pane works, it
	// just won't be a reuse candidate later.
	_ = t.markPaneOwned(ctx, socket, created.PaneID)
	return created, nil
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
//
// The write order is witness → owner → slot, and it is not arbitrary. tmux gives
// no way to set several options atomically, so any *prefix* of this sequence is
// a state a crashed or cancelled process can leave behind, and the order is
// chosen so that every prefix is inert. Witness-only, and witness+owner, are
// both rejected by slot resolution — which requires all three — so neither can
// steer a later call towards a pane that is only half claimed. The reverse order
// would publish a slot marker on a pane with no recorded owner, and the teardown
// rule keys off exactly that owner value to decide whether a pane may be killed
// or must merely be released: a slotted pane of unknown ownership is the one
// record we must never produce.
//
// slot <= 0 writes no slot marker at all rather than clearing one. A pane being
// claimed for the first time has nothing to clear, and adding an unconditional
// unset would put a third tmux call on every pane creation for no gain;
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
	if _, err := t.runWithSocket(ctx, socket,
		"set-option", "-p", "-t", paneID, paneOptWitness, paneID); err != nil {
		return err
	}
	if _, err := t.runWithSocket(ctx, socket,
		"set-option", "-p", "-t", paneID, paneOptOwner, owner); err != nil {
		return err
	}
	if slot <= 0 {
		return nil
	}
	_, err := t.runWithSocket(ctx, socket,
		"set-option", "-p", "-t", paneID, paneOptSlot, strconv.Itoa(slot))
	return err
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

// clearPaneRegistration removes all three markers, in the reverse of the write
// order — slot, then owner, then witness — for the same reason markPaneOwnedAs
// writes them forwards: every prefix of the teardown leaves the pane less
// claimed than before, never more. Stopping halfway can only ever produce a pane
// we have given up on, never one we still steer.
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
	for _, opt := range []string{paneOptSlot, paneOptOwner, paneOptWitness} {
		if _, err := t.runWithSocket(ctx, socket,
			"set-option", "-p", "-u", "-t", bareID, opt); err != nil {
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

// findSessionByName looks up a session by name on the given socket.
// Returns the session's IDs if found.
func (t *tmuxClient) findSessionByName(ctx context.Context, socket, name string) (*CreatedSession, error) {
	out, err := t.runWithSocket(ctx, socket, "list-sessions",
		"-F", "#{session_id}\t#{session_name}\t#{window_id}\t#{pane_id}",
		"-f", fmt.Sprintf("#{==:#{session_name},%s}", name))
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, fmt.Errorf("session not found: %s", name)
	}
	// Take the first matching line.
	line := strings.Split(out, "\n")[0]
	parts := strings.Split(line, "\t")
	if len(parts) != 4 {
		return nil, fmt.Errorf("unexpected tmux output: %q", line)
	}
	return &CreatedSession{
		SessionID:   parts[0],
		SessionName: parts[1],
		WindowID:    parts[2],
		PaneID:      parts[3],
	}, nil
}

// CreateWindow creates a new window in the given session.
// Returns a CreatedWindow with the window and pane IDs.
func (t *tmuxClient) CreateWindow(ctx context.Context, sessionID, name string) (*CreatedWindow, error) {
	socket, bareID := parseTarget(sessionID)
	args := []string{"new-window", "-t", bareID, "-P", "-F", "#{window_id}\t#{window_name}\t#{pane_id}"}
	if name != "" {
		args = append(args, "-n", name)
	}
	out, err := t.runWithSocket(ctx, socket, args...)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\t")
	if len(parts) != 3 {
		return nil, fmt.Errorf("unexpected tmux output: %q", out)
	}
	// A new window comes with a pane, and we made it — claim it, exactly as
	// SplitPane and createSessionOnSocket do. Without this the pane is
	// indistinguishable from one of the user's, so it could never be reused.
	// markPaneOwned wants the bare ID, so call it before prefixing.
	_ = t.markPaneOwned(ctx, socket, parts[2])

	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	return &CreatedWindow{
		WindowID:   prefix + parts[0],
		WindowName: parts[1],
		PaneID:     prefix + parts[2],
	}, nil
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
func (t *tmuxClient) ExecuteCommand(ctx context.Context, paneID, command string) (*CommandResult, error) {
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
			return &CommandResult{
				PaneID:   paneID,
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

	return &CommandResult{
		PaneID:   paneID,
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

// KillSession kills a tmux session and all its windows.
// It accepts session IDs ($N), prefixed IDs (headless:$N), or session names.
func (t *tmuxClient) KillSession(ctx context.Context, sessionID string) error {
	socket, bareID := parseTarget(sessionID)
	_, err := t.runWithSocket(ctx, socket, "kill-session", "-t", bareID)
	if err != nil {
		// If the direct target failed, try resolving by session name.
		resolved, resolveErr := t.resolveSessionTarget(ctx, socket, bareID)
		if resolveErr == nil {
			_, err = t.runWithSocket(ctx, socket, "kill-session", "-t", resolved)
		}
	}
	return err
}

// resolveSessionTarget attempts to resolve a session target that may be a name
// rather than a tmux session ID. It searches the session list on the given socket.
func (t *tmuxClient) resolveSessionTarget(ctx context.Context, socket, target string) (string, error) {
	out, err := t.runWithSocket(ctx, socket, "list-sessions",
		"-F", "#{session_id}\t#{session_name}")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			continue
		}
		sid, sname := parts[0], parts[1]
		if sname == target || sid == target {
			return sid, nil
		}
	}
	return "", fmt.Errorf("session not found: %s", target)
}

// KillPane kills a tmux pane.
func (t *tmuxClient) KillPane(ctx context.Context, paneID string) error {
	socket, bareID := parseTarget(paneID)
	_, err := t.runWithSocket(ctx, socket, "kill-pane", "-t", bareID)
	return err
}

// KillHeadlessServer shuts down the headless tmux server and all its sessions.
// Returns the number of sessions that were running before shutdown.
// It kills sessions individually rather than using kill-server to avoid any
// kill-server wrappers or guards in the environment.
func (t *tmuxClient) KillHeadlessServer(ctx context.Context) (int, error) {
	// List sessions on the headless socket (without the "headless:" prefix so we
	// can pass the bare IDs directly to kill-session on the headless socket).
	sessions, err := t.listSessionsOnSocket(ctx, headlessSocket)
	if err != nil {
		// No server running is not an error.
		return 0, nil
	}
	n := len(sessions)
	for _, s := range sessions {
		// Kill each session on the headless socket using its bare ID.
		_, _ = t.runWithSocket(ctx, headlessSocket, "kill-session", "-t", s.ID)
	}
	return n, nil
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

// getWindowIDForPane returns the window ID that contains the given pane.
// The returned ID is prefixed with "headless:" when the pane lives on the
// headless socket.
func (t *tmuxClient) getWindowIDForPane(ctx context.Context, paneID string) (string, error) {
	socket, bareID := parseTarget(paneID)
	out, err := t.runWithSocket(ctx, socket, "display-message", "-t", bareID, "-p", "#{window_id}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if socket != "" {
		id = headlessPrefix + id
	}
	return id, nil
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

// paneRecordFor returns the registry record for a single pane. found is false
// when the pane carries no valid record — which is the normal answer for one of
// the user's own panes, and the reason close-pane refuses to touch it.
//
// display-message -p evaluates the format in the target pane's context, so
// #{@mcp_pane} resolves exactly as it does inside list-panes and the witness
// comparison is the same comparison.
func (t *tmuxClient) paneRecordFor(ctx context.Context, paneID string) (paneRecord, bool, error) {
	socket, bareID := parseTarget(paneID)
	out, err := t.runWithSocket(ctx, socket, "display-message", "-t", bareID, "-p", registryFormat())
	if err != nil {
		return paneRecord{}, false, err
	}
	prefix := ""
	if socket != "" {
		prefix = headlessPrefix
	}
	rec, ok := parseRegistryLine(out, prefix)
	if !ok {
		return paneRecord{}, false, nil
	}
	return rec, true, nil
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

// findIdlePaneInWindow finds a pane in the same window as sourcePaneID that we
// created and that is now sitting idle, so it can be reused instead of piling
// up another split. It returns the pane ID, or empty string if there is none.
//
// Two conditions, and both are load-bearing.
//
// It must be *ours*. tmux records no creator, so we mark the panes we make
// (markPaneOwned) and treat every unmarked pane as the user's. Reusing an
// unmarked pane means typing into whatever shell the user left at a prompt —
// which may be sitting at an `ssh prod`, a `sudo -i`, or a psql session, where
// "reuse" would execute the agent's command in their privileged context, and a
// later teardown would kill the pane out from under them. Idle is not the same
// as free.
//
// It must be *idle*, which we take to mean alive with the shell itself as the
// foreground process — no child command has taken over the terminal. That is
// paneIsIdleShell rather than WaitingForInput, because WaitingForInput is not
// consistent across platforms: an idle interactive shell reports
// waitingForInput=false on Linux (readline blocks in poll/select, not
// n_tty_read) but true on macOS. The "shell is the foreground process" signal
// agrees on both, and is strictly more correct anyway — a pane running
// `bash script.sh` has a child in the foreground and is not idle.
func (t *tmuxClient) findIdlePaneInWindow(ctx context.Context, sourcePaneID string) (string, error) {
	windowID, err := t.getWindowIDForPane(ctx, sourcePaneID)
	if err != nil {
		return "", fmt.Errorf("get window for pane %s: %w", sourcePaneID, err)
	}

	owned, err := t.ownedPanesInWindow(ctx, windowID)
	if err != nil {
		return "", fmt.Errorf("list owned panes in window %s: %w", windowID, err)
	}
	if len(owned) == 0 {
		return "", nil
	}

	panes, err := t.ListPanes(ctx, windowID)
	if err != nil {
		return "", fmt.Errorf("list panes in window %s: %w", windowID, err)
	}

	for _, p := range panes {
		if p.ID == sourcePaneID || !owned[p.ID] {
			continue
		}
		state, err := t.GetPaneState(ctx, p.ID)
		if err != nil {
			continue
		}
		if paneIsIdleShell(state) {
			return p.ID, nil
		}
	}

	return "", nil
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

	// If the pane PID is empty the pane does not exist.
	if pidStr == "" {
		return nil, fmt.Errorf("pane %s does not exist or has no PID", paneID)
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
	// in the package (Invariant S). It returns errNotInTmux when there is none.
	Self(ctx context.Context) (paneRef, error)

	// ---- Structure ----

	OpenBeside(ctx context.Context, place placement) (paneRef, error)
	Close(ctx context.Context, p paneRef) error

	// ---- Discovery ----

	// Siblings lists every pane sharing a window with the given one, geometry
	// included. A failure here DEGRADES rather than being fatal — no adoption,
	// and the anchor falls back to self — which is why it is separate from
	// Records, whose failure is fatal to a resolution.
	Siblings(ctx context.Context, of paneRef) ([]paneInfo, error)

	// Records is the witnessed registry of the window around the given pane.
	Records(ctx context.Context, of paneRef) (map[paneRef]paneRecord, error)

	// RecordFor is the single-pane witnessed lookup, and it exists only while the
	// explicit-paneId path does: both remaining callers — close-pane's explicit
	// branch and clearForDisplay's re-read — are on that path, and the commit
	// that deletes it deletes this with it. It is named here rather than left
	// implicit so that "why does the port have a single-pane read?" has an answer
	// with an expiry date on it.
	RecordFor(ctx context.Context, p paneRef) (paneRecord, bool, error)

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
type tmuxBackend struct{ c *tmuxClient }

func newTmuxBackend(c *tmuxClient) *tmuxBackend { return &tmuxBackend{c: c} }

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
// failed", which is why the caller can turn it into errNotInTmux instead of a
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
		return paneRef{}, errNotInTmux
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

func (b *tmuxBackend) RecordFor(ctx context.Context, p paneRef) (paneRecord, bool, error) {
	return b.c.paneRecordFor(ctx, p.target())
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
	res, err := b.c.ExecuteCommand(ctx, p.target(), command)
	if err != nil {
		return nil, err
	}
	return &execOutcome{Output: res.Output, ExitCode: res.ExitCode, TimedOut: res.TimedOut}, nil
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
