package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// herdrBackend is the herdr implementation of Backend.
//
// Every method is one or two calls to the herdr CLI. The CLI finds its server
// through HERDR_SOCKET_PATH, which herdr injects into its panes and this server
// inherits, so nothing here names a session: the server reached is the one
// whose pane this process runs in.
//
// What herdr does not have, this backend supplies or borrows:
//
//   - Pane options. The registry is a state file instead; see herdrRegistry.
//   - Invisible panes. Every herdr workspace shows in the user's sidebar, so
//     isolated slots are delegated to tmux's private namespace when tmux is
//     installed, and refused when it is not. A ref from that namespace keeps
//     its "headless:" prefix, and every method routes on it.
//   - A bell flag. Bell answers false.
//
// The tty facts the readiness gate reads come from the OS, exactly as under
// tmux: herdr reports the pane's shell pid, and the shell's controlling
// terminal is probed directly.
type herdrBackend struct {
	bin string
	reg *herdrRegistry

	// layouts holds the agent stamps and the grid modes, in a file of its own
	// that older binaries never rewrite. See herdrLayoutStore. It is nil in a
	// backend built only to probe a pane, and then nothing is stamped.
	layouts *herdrLayoutStore

	// iso serves isolated slots, or is nil when tmux is not installed.
	iso *tmuxBackend

	// cwd is where new panes start: this server's own working directory, which
	// is the agent's. herdr would otherwise start a split in the anchor's cwd,
	// which for slot 2 and up is a helper that may have cd'd anywhere.
	cwd string
}

var _ Backend = (*herdrBackend)(nil)

// herdrCallTimeout bounds one CLI call when the caller's context has no
// deadline of its own, so a wedged herdr server cannot hold slotMu forever.
const herdrCallTimeout = 15 * time.Second

// errNoIsolatedHere is what an isolated request answers under herdr on a
// machine without tmux. It names no multiplexer: the agent's tools are the same
// everywhere, and what it needs to know is that this kind of pane is not
// available.
var errNoIsolatedHere = errors.New("isolated panes are not available on this machine; " +
	"use a visible slot instead")

// errUnknownKeyName is a key name, sent with literal:false, that has no herdr
// key and no escape sequence. It is refused rather than typed: the characters
// of a key's name are never what the caller meant.
var errUnknownKeyName = errors.New("unknown key name")

// newHerdrBackend builds the backend for the herdr server behind
// HERDR_SOCKET_PATH.
func newHerdrBackend() (*herdrBackend, error) {
	path, err := herdrRegistryPath(os.Getenv("HERDR_SOCKET_PATH"))
	if err != nil {
		return nil, err
	}
	b := &herdrBackend{
		bin:     herdrBin(),
		reg:     &herdrRegistry{path: path},
		layouts: &herdrLayoutStore{path: herdrLayoutPath(path)},
	}
	if wd, err := os.Getwd(); err == nil {
		b.cwd = wd
	}
	if _, err := exec.LookPath("tmux"); err == nil {
		iso := newTmuxBackend(newTmuxClient())
		iso.noWindow = true
		b.iso = iso
	}
	return b, nil
}

// herdrBin is the herdr binary: the one herdr says it is, when it says so.
func herdrBin() string {
	if p := os.Getenv("HERDR_BIN_PATH"); p != "" {
		return p
	}
	return "herdr"
}

// ---- The CLI ----

// herdrIDInText matches herdr ids embedded in a sentence: w1, w1:t2, w1:p3, and
// terminal handles. Like scrubIDs for tmux, it is applied wherever herdr's own
// text becomes an error a caller can see.
var herdrIDInText = regexp.MustCompile(`\bw[0-9]+(:[pt][0-9]+)?\b|\bterm_[0-9a-f]+\b`)

func scrubHerdrIDs(s string) string { return herdrIDInText.ReplaceAllString(s, "<pane>") }

// herdrError is a failed CLI call, with herdr's machine-readable code.
type herdrError struct {
	Code    string
	Message string
}

func (e *herdrError) Error() string { return multiplexerRefused + ": " + e.Message }

// run executes one herdr command and returns its stdout.
//
// herdr reports a server-side failure as {"error":{"code","message"}} and exit
// status 1; the message is scrubbed of ids before it can become an error a
// caller sees. pane_not_found becomes errPaneGone, which the handlers turn
// into a sentence about the slot. Nothing returned names herdr or its
// subcommands: see multiplexerError.
func (b *herdrBackend) run(ctx context.Context, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, herdrCallTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, b.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if he := parseHerdrError(stderr.Bytes()); he != nil {
		return nil, he.asErr()
	}
	if he := parseHerdrError(out); he != nil {
		return nil, he.asErr()
	}
	return nil, multiplexerError(err, scrubHerdrIDs(strings.TrimSpace(stderr.String())))
}

func parseHerdrError(raw []byte) *herdrError {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &env) != nil || env.Error == nil {
		return nil
	}
	return &herdrError{Code: env.Error.Code, Message: scrubHerdrIDs(env.Error.Message)}
}

func (e *herdrError) asErr() error {
	if e.Code == "pane_not_found" {
		return errPaneGone
	}
	return e
}

// runJSON runs a command and decodes its .result into out.
func (b *herdrBackend) runJSON(ctx context.Context, out any, args ...string) error {
	raw, err := b.run(ctx, args...)
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: unreadable answer: %w", multiplexerRefused, err)
	}
	return json.Unmarshal(env.Result, out)
}

// herdrPane is the part of herdr's pane record this backend uses.
type herdrPane struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	TerminalID  string `json:"terminal_id"`
}

// herdrGeometry is a pane rectangle in cells, as herdr reports it.
type herdrGeometry struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// herdrTabLayout is a tab as herdr lays it out: its area, whether a pane in it
// is zoomed, which pane in it has the tab's focus, and every pane's rectangle
// in cells.
type herdrTabLayout struct {
	TabID   string
	Area    herdrGeometry
	Zoomed  bool
	Focused string
	Panes   map[string]herdrGeometry
}

// herdrLayoutJSON is herdr's layout object, which `pane layout` answers and
// every move carries for the tabs it touched.
type herdrLayoutJSON struct {
	TabID         string        `json:"tab_id"`
	Area          herdrGeometry `json:"area"`
	Zoomed        bool          `json:"zoomed"`
	FocusedPaneID string        `json:"focused_pane_id"`
	Panes         []struct {
		PaneID string        `json:"pane_id"`
		Rect   herdrGeometry `json:"rect"`
	} `json:"panes"`
}

func (l herdrLayoutJSON) tab() herdrTabLayout {
	tab := herdrTabLayout{
		TabID:   l.TabID,
		Area:    l.Area,
		Zoomed:  l.Zoomed,
		Focused: l.FocusedPaneID,
		Panes:   make(map[string]herdrGeometry, len(l.Panes)),
	}
	for _, p := range l.Panes {
		tab.Panes[p.PaneID] = p.Rect
	}
	return tab
}

// tabLayout reads the tab around a pane.
func (b *herdrBackend) tabLayout(ctx context.Context, of string) (herdrTabLayout, error) {
	var res struct {
		Layout herdrLayoutJSON `json:"layout"`
	}
	if err := b.runJSON(ctx, &res, "pane", "layout", "--pane", of); err != nil {
		return herdrTabLayout{}, err
	}
	return res.Layout.tab(), nil
}

// layout returns every pane in the tab around a pane, with geometry.
func (b *herdrBackend) layout(ctx context.Context, of string) (map[string]herdrGeometry, error) {
	tab, err := b.tabLayout(ctx, of)
	if err != nil {
		return nil, err
	}
	return tab.Panes, nil
}

// livePanes maps every pane the herdr server has to its terminal id. It is the
// witness the registry is checked against, so it is the whole server's list.
func (b *herdrBackend) livePanes(ctx context.Context) (map[string]string, error) {
	var res struct {
		Panes []herdrPane `json:"panes"`
	}
	if err := b.runJSON(ctx, &res, "pane", "list"); err != nil {
		return nil, err
	}
	live := make(map[string]string, len(res.Panes))
	for _, p := range res.Panes {
		live[p.PaneID] = p.TerminalID
	}
	return live, nil
}

// paneGet reads one pane's record.
func (b *herdrBackend) paneGet(ctx context.Context, id string) (herdrPane, error) {
	var res struct {
		Pane herdrPane `json:"pane"`
	}
	err := b.runJSON(ctx, &res, "pane", "get", id)
	return res.Pane, err
}

// herdrSeq is the creation ordinal of a herdr pane: the N of "w1:pN". herdr
// numbers panes in creation order and never reuses a number within a server.
// An id that does not parse sorts last, as under tmux.
func herdrSeq(id string) int {
	_, n, ok := strings.Cut(id, ":p")
	if !ok {
		return math.MaxInt
	}
	v, err := strconv.Atoi(n)
	if err != nil {
		return math.MaxInt
	}
	return v
}

// isolatedRef reports whether a ref belongs to the delegated tmux namespace.
func isolatedRef(p paneRef) bool { return strings.HasPrefix(p.target(), headlessPrefix) }

// ---- Identity ----

// Self reads HERDR_PANE_ID, the pane herdr started this process in. It is the
// only reader of that variable apart from detection, which reads it to decide
// whether to believe it — see TestOnlyPolicyCodeKnowsOurOwnPane.
func (b *herdrBackend) Self(_ context.Context) (paneRef, error) {
	id := os.Getenv("HERDR_PANE_ID")
	if id == "" {
		return paneRef{}, errNoWindow
	}
	return newPaneRef(id), nil
}

// ---- Structure ----

// OpenBeside splits the anchor and records the new pane as agent-owned at
// once, before policy claims it for a slot, for the reason SplitPane marks its
// pane under tmux: a pane that exists with no record of ours is the user's,
// and a server that dies between the two steps must not leave one behind.
//
// herdr's --ratio is the ANCHOR's share of the split, so it is the complement
// of the new pane's SizePercent. --no-focus keeps the user's cursor where it
// was, which is the job -d does under tmux.
func (b *herdrBackend) OpenBeside(ctx context.Context, place placement) (paneRef, error) {
	direction := "down"
	if place.Axis == splitBeside {
		direction = "right"
	}
	args := []string{"pane", "split", place.Anchor.target(), "--direction", direction, "--no-focus"}
	if place.SizePercent > 0 && place.SizePercent < 100 {
		args = append(args, "--ratio", strconv.FormatFloat(1-float64(place.SizePercent)/100, 'f', 2, 64))
	}
	if b.cwd != "" {
		args = append(args, "--cwd", b.cwd)
	}
	var res struct {
		Pane herdrPane `json:"pane"`
	}
	if err := b.runJSON(ctx, &res, args...); err != nil {
		return paneRef{}, err
	}
	if res.Pane.PaneID == "" {
		return paneRef{}, errors.New("the split reported no new pane")
	}
	// A failed write is not reported here. Policy claims the pane next, which
	// writes the same record again, and closes a new pane that ends up carrying
	// no record at all.
	_ = b.reg.upsert(herdrEntry{PaneID: res.Pane.PaneID, TerminalID: res.Pane.TerminalID, Owner: ownerAgent})
	return newPaneRef(res.Pane.PaneID), nil
}

func (b *herdrBackend) OpenIsolated(ctx context.Context) (paneRef, error) {
	if b.iso == nil {
		return paneRef{}, errNoIsolatedHere
	}
	return b.iso.OpenIsolated(ctx)
}

// Close closes the pane and then forgets it. A record left behind by a failed
// removal is harmless: the pane it names is gone, so the witness no longer
// confirms it and the next read drops it.
func (b *herdrBackend) Close(ctx context.Context, p paneRef) error {
	if isolatedRef(p) {
		return b.iso.Close(ctx, p)
	}
	if _, err := b.run(ctx, "pane", "close", p.target()); err != nil {
		return err
	}
	_ = b.reg.remove(p.target())
	return nil
}

// ---- Discovery ----

// Siblings lists the panes in the agent's tab, which is herdr's equivalent of a
// tmux window: the panes the user sees together. Positions are relative to the
// tab's area, as they are to the window under tmux.
func (b *herdrBackend) Siblings(ctx context.Context, of paneRef) ([]paneInfo, error) {
	tab, err := b.tabLayout(ctx, of.target())
	if err != nil {
		return nil, err
	}
	return herdrPaneInfos(tab), nil
}

// Records is the witnessed registry of the tab around a pane: the state file's
// records that herdr still confirms, narrowed to the panes in that tab.
func (b *herdrBackend) Records(ctx context.Context, of paneRef) (map[paneRef]paneRecord, error) {
	tab, err := b.layout(ctx, of.target())
	if err != nil {
		return nil, err
	}
	// The live list is taken inside the registry's lock; see validFrom.
	entries, err := b.reg.validFrom(func() (map[string]string, error) { return b.livePanes(ctx) })
	if err != nil {
		return nil, err
	}
	// A failed read of the layout store costs the stamps, which leaves every
	// helper out of the grid and changes nothing else.
	stamps := map[string]herdrAgentStamp{}
	if file, err := b.layouts.read(); err == nil {
		for _, a := range file.Agents {
			stamps[a.PaneID] = a
		}
	}
	reg := make(map[paneRef]paneRecord, len(entries))
	for _, e := range entries {
		if _, inTab := tab[e.PaneID]; !inTab {
			continue
		}
		ref := newPaneRef(e.PaneID)
		rec := paneRecord{Ref: ref, Owner: e.Owner, Slot: e.Slot, Seq: herdrSeq(e.PaneID)}
		if a, ok := stamps[e.PaneID]; ok && a.TerminalID == e.TerminalID && a.Agent != "" {
			rec.Agent = newPaneRef(a.Agent)
		}
		reg[ref] = rec
	}
	return reg, nil
}

func (b *herdrBackend) IsolatedRecords(ctx context.Context) (map[paneRef]paneRecord, error) {
	if b.iso == nil {
		return map[paneRef]paneRecord{}, nil
	}
	return b.iso.IsolatedRecords(ctx)
}

// IsolatedPanes answers "none" without tmux, and that is the truth rather than
// a degraded read: no isolated pane can have been opened.
func (b *herdrBackend) IsolatedPanes(ctx context.Context) ([]paneRef, error) {
	if b.iso == nil {
		return nil, nil
	}
	return b.iso.IsolatedPanes(ctx)
}

// ---- Registry writes ----

// Claim writes owner and slot in one record write, with the pane's current
// terminal id as its witness.
func (b *herdrBackend) Claim(ctx context.Context, p paneRef, owner string, slot int) error {
	if isolatedRef(p) {
		return b.iso.Claim(ctx, p, owner, slot)
	}
	pane, err := b.paneGet(ctx, p.target())
	if err != nil {
		return err
	}
	if slot < 0 {
		slot = 0
	}
	return b.reg.upsert(herdrEntry{PaneID: pane.PaneID, TerminalID: pane.TerminalID, Owner: owner, Slot: slot})
}

func (b *herdrBackend) SetSlot(ctx context.Context, p paneRef, slot int) error {
	if isolatedRef(p) {
		return b.iso.SetSlot(ctx, p, slot)
	}
	return b.reg.setSlot(p.target(), slot)
}

func (b *herdrBackend) ClearMarks(ctx context.Context, p paneRef) error {
	if isolatedRef(p) {
		return b.iso.ClearMarks(ctx, p)
	}
	return b.reg.remove(p.target())
}

// ---- IO ----

// SendKeys delivers text with send-text and named keys with send-keys, and
// Enter as a key of its own after the text, as under tmux.
//
// The tool surface speaks tmux's key names, so they are translated here; a
// name herdr has no logical key for is sent as its escape sequence, a string
// that is no key name at all is sent as text, which is what tmux does with it
// too, and a key name that cannot be translated is refused (errUnknownKeyName)
// rather than typed as its own characters.
//
// The text goes after the pane id with no `--`: herdr takes TEXT verbatim, so
// "-x" and "--help" arrive as typed, and a `--` would be typed with them.
// `pane run` is never used: it sends text and Enter in one step, which would
// leave policy no moment to check that the text landed before the line is
// submitted.
func (b *herdrBackend) SendKeys(ctx context.Context, p paneRef, keys string, literal, enter bool) error {
	if isolatedRef(p) {
		return b.iso.SendKeys(ctx, p, keys, literal, enter)
	}
	id := p.target()
	if keys != "" {
		var err error
		switch key, seq, named := herdrKey(keys); {
		case literal || !named:
			_, err = b.run(ctx, "pane", "send-text", id, keys)
		case key != "":
			_, err = b.run(ctx, "pane", "send-keys", id, key)
		case seq != "":
			_, err = b.run(ctx, "pane", "send-text", id, seq)
		default:
			return fmt.Errorf("%w: %s", errUnknownKeyName, keys)
		}
		if err != nil {
			return err
		}
	}
	if enter {
		if _, err := b.run(ctx, "pane", "send-keys", id, "enter"); err != nil {
			return err
		}
	}
	return nil
}

// herdrKey translates one tmux key name. It answers either a herdr logical key
// or, for the keys herdr has no name for, the escape sequence a terminal sends
// for them. named is false when the string is not a key name at all. A
// modified name it cannot translate answers named with neither, which the
// caller refuses.
//
// Modifiers are tmux's C-, M- and S- prefixes, in any combination. On a key
// herdr names they become its ctrl+, alt+ and shift+; on a key sent as an
// escape sequence they become the xterm modifier parameter, 1 + shift(1) +
// alt(2) + ctrl(4), so C-Home is ESC [1;5H and M-PageUp is ESC [5;3~.
func herdrKey(name string) (key, seq string, named bool) {
	if mods, base, ok := splitKeyModifiers(name); ok {
		return modifiedHerdrKey(mods, base)
	}
	return plainHerdrKey(name)
}

// keyModifier is one tmux modifier prefix and what it means elsewhere.
type keyModifier struct {
	herdr string // herdr's prefix for it
	xterm int    // its weight in the xterm modifier parameter
}

var keyModifiers = map[string]keyModifier{
	"C-": {"ctrl+", 4},
	"M-": {"alt+", 2},
	"S-": {"shift+", 1},
}

// keyNameShape is what a key name looks like once its modifiers are removed.
// Anything else after a modifier prefix is text that happens to start with
// one, and is sent as text.
var keyNameShape = regexp.MustCompile(`^[A-Za-z0-9]+$|^.$`)

// splitKeyModifiers peels tmux modifier prefixes off a name. ok is false when
// there are none, or when what is left does not look like a key.
func splitKeyModifiers(name string) (mods []keyModifier, base string, ok bool) {
	base = name
	for len(base) > 2 {
		m, isMod := keyModifiers[base[:2]]
		if !isMod {
			break
		}
		mods, base = append(mods, m), base[2:]
	}
	if len(mods) == 0 || !keyNameShape.MatchString(base) {
		return nil, "", false
	}
	return mods, base, true
}

// modifiedHerdrKey applies modifiers to a base key.
func modifiedHerdrKey(mods []keyModifier, base string) (key, seq string, named bool) {
	inner, innerSeq, isKey := plainHerdrKey(base)
	switch {
	case isKey && inner != "":
	case isKey && innerSeq != "":
		weight := 1
		for _, m := range mods {
			weight += m.xterm
		}
		if s, ok := xtermModified(innerSeq, weight); ok {
			return "", s, true
		}
		return "", "", true
	case len(base) == 1:
		inner = strings.ToLower(base)
	default:
		return "", "", true
	}
	prefix := ""
	for _, m := range mods {
		prefix += m.herdr
	}
	return prefix + inner, "", true
}

// xtermModified rewrites an unmodified CSI sequence into its modified form:
// ESC [ H becomes ESC [ 1 ; m H, and ESC [ n ~ becomes ESC [ n ; m ~.
func xtermModified(seq string, weight int) (string, bool) {
	body, ok := strings.CutPrefix(seq, "\x1b[")
	if !ok || body == "" {
		return "", false
	}
	final := body[len(body)-1:]
	params := body[:len(body)-1]
	if params == "" {
		params = "1"
	}
	return "\x1b[" + params + ";" + strconv.Itoa(weight) + final, true
}

// plainHerdrKey translates a key name with no modifiers.
func plainHerdrKey(name string) (key, seq string, named bool) {
	switch name {
	case "Enter":
		return "enter", "", true
	case "Escape":
		return "esc", "", true
	case "Tab":
		return "tab", "", true
	case "BTab":
		return "shift+tab", "", true
	case "Space":
		return "space", "", true
	case "BSpace":
		return "backspace", "", true
	case "Up", "Down", "Left", "Right":
		return strings.ToLower(name), "", true
	case "Home":
		return "", "\x1b[H", true
	case "End":
		return "", "\x1b[F", true
	case "PageUp", "PPage":
		return "", "\x1b[5~", true
	case "PageDown", "NPage":
		return "", "\x1b[6~", true
	case "DC":
		return "", "\x1b[3~", true
	case "IC":
		return "", "\x1b[2~", true
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(name, "F")); err == nil && strings.HasPrefix(name, "F") &&
		n >= 1 && n <= 12 {
		return "f" + strconv.Itoa(n), "", true
	}
	return "", "", false
}

// Capture reads the pane's text. lines <= 0 is the visible screen, as under
// tmux; otherwise the last lines rendered, soft wraps kept as they appear.
func (b *herdrBackend) Capture(ctx context.Context, p paneRef, lines int, colors bool) (string, error) {
	if isolatedRef(p) {
		return b.iso.Capture(ctx, p, lines, colors)
	}
	args := []string{"pane", "read", p.target()}
	if lines > 0 {
		args = append(args, "--source", "recent", "--lines", strconv.Itoa(lines))
	} else {
		args = append(args, "--source", "visible")
	}
	if colors {
		args = append(args, "--format", "ansi")
	}
	out, err := b.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// Screen is the visible screen with its styling, measured from the layout.
func (b *herdrBackend) Screen(ctx context.Context, p paneRef) (screen, error) {
	if isolatedRef(p) {
		return b.iso.Screen(ctx, p)
	}
	cols, rows := 80, 24
	if panes, err := b.layout(ctx, p.target()); err == nil {
		if r, ok := panes[p.target()]; ok && r.Width > 0 && r.Height > 0 {
			cols, rows = r.Width, r.Height
		}
	}
	out, err := b.run(ctx, "pane", "read", p.target(), "--source", "visible", "--format", "ansi")
	if err != nil {
		return screen{}, err
	}
	return screen{ANSI: strings.TrimRight(string(out), "\n"), Cols: cols, Rows: rows}, nil
}

// Foreground asks herdr for the pane's shell pid, and the OS for everything
// else: the foreground process group, liveness, and the tty's input state.
func (b *herdrBackend) Foreground(ctx context.Context, p paneRef) (*PaneState, error) {
	if isolatedRef(p) {
		return b.iso.Foreground(ctx, p)
	}
	shell, err := b.shellPID(ctx, p.target())
	if err != nil {
		return nil, err
	}
	state := &PaneState{PanePID: shell}
	if err := fillPaneState(ctx, state); err != nil {
		state.IsAlive = processIsRunning(shell)
		state.ForegroundPID = shell
	}
	if tty, err := ttyOfPID(shell); err == nil {
		fillInputState(state, tty)
	}
	return state, nil
}

func (b *herdrBackend) shellPID(ctx context.Context, pane string) (int, error) {
	var res struct {
		ProcessInfo struct {
			ShellPID int `json:"shell_pid"`
		} `json:"process_info"`
	}
	if err := b.runJSON(ctx, &res, "pane", "process-info", "--pane", pane); err != nil {
		// A server that is not running — its socket gone, or nothing answering
		// on it — has no panes, so for this read the pane is gone. Other calls
		// keep the error as it is.
		var he *herdrError
		if errors.As(err, &he) && he.Code == "server_not_running" {
			return 0, errPaneGone
		}
		return 0, err
	}
	if res.ProcessInfo.ShellPID <= 0 {
		return 0, errPaneGone
	}
	return res.ProcessInfo.ShellPID, nil
}

// Bell is always false: herdr exposes no bell flag.
func (b *herdrBackend) Bell(ctx context.Context, p paneRef) (bool, error) {
	if isolatedRef(p) {
		return b.iso.Bell(ctx, p)
	}
	return false, nil
}

// ---- UI ----

// SetTitle is herdr's pane label, which the user sees in the pane border. An
// empty title clears it.
func (b *herdrBackend) SetTitle(ctx context.Context, p paneRef, title string) error {
	if isolatedRef(p) {
		return b.iso.SetTitle(ctx, p, title)
	}
	if title == "" {
		_, err := b.run(ctx, "pane", "rename", p.target(), "--clear")
		return err
	}
	_, err := b.run(ctx, "pane", "rename", p.target(), title)
	return err
}

// Notify shows a herdr notification. herdr decides how long it stays up.
func (b *herdrBackend) Notify(ctx context.Context, message string, _ time.Duration) error {
	_, err := b.run(ctx, "notification", "show", message)
	return err
}

// ---- The agent grid ----

// herdrGridTolerance is how far a pane may land from its planned edge under
// herdr. Its splits take a ratio rather than a size in cells, and a ratio is
// rounded once per split on the way down the tree.
const herdrGridTolerance = 2

// Window describes the tab around a pane. herdr draws no border between panes,
// so the gap is zero, and its splits take a ratio rather than a size in cells,
// so a pane may land a cell further off than under tmux.
func (b *herdrBackend) Window(ctx context.Context, of paneRef) (windowInfo, error) {
	tab, err := b.tabLayout(ctx, of.target())
	if err != nil {
		return windowInfo{}, err
	}
	info := windowInfo{
		Width: tab.Area.Width, Height: tab.Area.Height,
		Gap: 0, Tolerance: herdrGridTolerance,
		Zoomed: tab.Zoomed,
	}
	pane, err := b.paneGet(ctx, of.target())
	if err != nil {
		return windowInfo{}, err
	}
	file, err := b.layouts.read()
	if err != nil {
		return windowInfo{}, err
	}
	for _, t := range file.Tabs {
		if t.TabID == tab.TabID && t.AgentPaneID == pane.PaneID && t.AgentTerminalID == pane.TerminalID &&
			t.Layout == string(layoutGrid) {
			info.Grid = true
		}
	}
	return info, nil
}

// SetGridMode records or removes this agent's mode for its tab, and drops
// every record herdr no longer confirms on the way past.
func (b *herdrBackend) SetGridMode(ctx context.Context, of paneRef, on bool) error {
	pane, err := b.paneGet(ctx, of.target())
	if err != nil {
		return err
	}
	live, err := b.livePanes(ctx)
	if err != nil {
		return err
	}
	return b.layouts.update(func(f *herdrLayoutFile) {
		f.prune(live)
		tabs := f.Tabs[:0]
		for _, t := range f.Tabs {
			if t.AgentPaneID != pane.PaneID {
				tabs = append(tabs, t)
			}
		}
		f.Tabs = tabs
		if on {
			f.Tabs = append(f.Tabs, herdrTabMode{
				TabID: pane.TabID, AgentPaneID: pane.PaneID, AgentTerminalID: pane.TerminalID,
				Layout: string(layoutGrid),
			})
		}
	})
}

// SetAgentPane records which agent a helper belongs to, witnessed by the
// helper's terminal id. Isolated panes are never stamped: they are in no tab.
func (b *herdrBackend) SetAgentPane(ctx context.Context, p, agent paneRef) error {
	if isolatedRef(p) {
		return nil
	}
	pane, err := b.paneGet(ctx, p.target())
	if err != nil {
		return err
	}
	live, err := b.livePanes(ctx)
	if err != nil {
		return err
	}
	return b.layouts.update(func(f *herdrLayoutFile) {
		f.prune(live)
		agents := f.Agents[:0]
		for _, a := range f.Agents {
			if a.PaneID != pane.PaneID {
				agents = append(agents, a)
			}
		}
		f.Agents = agents
		if !agent.empty() {
			f.Agents = append(f.Agents, herdrAgentStamp{
				PaneID: pane.PaneID, TerminalID: pane.TerminalID, Agent: agent.target(),
			})
		}
	})
}

// herdrFocusHidden is Arrange declining a tab whose focus is on a slot pane
// while the tab is not the one the user is looking at. Rebuilding the tab takes
// that pane out of it for a moment, which hands the tab's focus to the agent,
// and putting it back would bring the tab into view.
const herdrFocusHidden gridRefusal = "the cursor is in a slot pane of a window that is not in view, " +
	"so no pane was moved or resized"

// Arrange lays the tab out as the plan says, by rebuilding it.
//
// herdr can only add a pane beside or below another one, and a move within one
// tab changes nothing (herdr answers that the pane is already in that tab). So
// the slot panes are moved out to a tab of their own and moved back one at a
// time in the order herdrMoves gives, which splits the agent's pane and then
// the slots that are already back, each at the ratio its planned size needs.
// The agent's pane never moves: it is what the first slot is placed beside.
// The holding tab closes by itself once its last pane has left it.
//
// Every move passes --no-focus, so the user's focus stays where it was, with
// one exception herdr leaves no way around: a focused pane that leaves its tab
// hands the tab's focus to another pane in it. When the focused pane is a slot,
// the tab is rebuilt only while it is in view, and focus is put back on that
// pane afterwards with a step from a neighbour into it. In a tab out of view
// that step would bring the tab into view, so nothing is moved there at all.
//
// A tab that already matches the plan within the tolerance is left alone,
// which spares the user the holding tab's flicker. A tab whose panes are not
// exactly the agent and the slots has changed since the caller looked, and is
// left alone too. If a move fails half way, the panes still out are put back
// beside the others, so no slot is left in the holding tab; the error says the
// grid was not applied, and the caller measures what is there.
func (b *herdrBackend) Arrange(ctx context.Context, agent paneRef, slots []paneRef, plan gridPlan) (map[paneRef]paneRef, error) {
	if len(plan.Cells) != len(slots) {
		return nil, errWindowChanged
	}
	agentID := agent.target()
	tab, err := b.tabLayout(ctx, agentID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(slots))
	for i, s := range slots {
		ids[i] = s.target()
	}
	if !herdrTabHolds(tab, agentID, ids) {
		return nil, errWindowChanged
	}
	if !gridMismatch(plan, append([]paneRef{agent}, slots...), herdrPaneInfos(tab), herdrGridTolerance, borderOff) {
		return nil, nil
	}
	focused := ""
	if slices.Contains(ids, tab.Focused) {
		focused = tab.Focused
		visible, err := b.tabInView(ctx, tab.TabID)
		if err != nil {
			return nil, err
		}
		if !visible {
			return nil, herdrFocusHidden
		}
	}

	r := &herdrRebuild{b: b, tab: tab.TabID, agent: agentID, ids: ids, orig: slices.Clone(ids)}
	err = r.run(ctx, plan)
	if focused != "" {
		b.refocus(context.WithoutCancel(ctx), agentID, r.now(focused))
	}
	return r.renamed, err
}

// herdrTabHolds reports whether the tab holds the agent and the slots and no
// other pane.
func herdrTabHolds(tab herdrTabLayout, agent string, slots []string) bool {
	if len(tab.Panes) != len(slots)+1 {
		return false
	}
	for _, id := range append([]string{agent}, slots...) {
		if _, ok := tab.Panes[id]; !ok {
			return false
		}
	}
	return true
}

// herdrPaneInfos is the tab's panes as Siblings reports them, relative to the
// tab's area.
func herdrPaneInfos(tab herdrTabLayout) []paneInfo {
	infos := make([]paneInfo, 0, len(tab.Panes))
	for id, r := range tab.Panes {
		infos = append(infos, paneInfo{
			Ref: newPaneRef(id),
			X:   r.X - tab.Area.X, Y: r.Y - tab.Area.Y,
			Width: r.Width, Height: r.Height,
			Seq: herdrSeq(id),
		})
	}
	return infos
}

// tabInView reports whether a tab is the one the user is looking at: the
// active tab of the focused workspace.
func (b *herdrBackend) tabInView(ctx context.Context, tabID string) (bool, error) {
	var res struct {
		Tab struct {
			Focused bool `json:"focused"`
		} `json:"tab"`
	}
	if err := b.runJSON(ctx, &res, "tab", "get", tabID); err != nil {
		return false, err
	}
	return res.Tab.Focused, nil
}

// herdrRebuild is one rebuild of a tab: where every slot pane is, and what it
// is called now.
type herdrRebuild struct {
	b     *herdrBackend
	tab   string // the agent's tab
	agent string // the agent's pane, which never moves
	ids   []string
	orig  []string // ids as the caller named them, cell for cell
	hold  string   // the holding tab, once there is one
	out   []bool   // which cells are in the holding tab

	// holdLayout is the holding tab as the last move into it left it.
	holdLayout herdrTabLayout

	renamed map[paneRef]paneRef
}

// herdrMoveResult is the part of a move's answer the rebuild reads.
type herdrMoveResult struct {
	MoveResult struct {
		Changed      bool            `json:"changed"`
		Reason       string          `json:"reason"`
		Pane         herdrPane       `json:"pane"`
		TargetLayout herdrLayoutJSON `json:"target_layout"`
		CreatedTab   struct {
			TabID string `json:"tab_id"`
		} `json:"created_tab"`
	} `json:"move_result"`
}

func (r *herdrRebuild) run(ctx context.Context, plan gridPlan) error {
	r.out = make([]bool, len(r.ids))
	for i := range r.ids {
		if err := r.moveOut(ctx, i); err != nil {
			r.putBack(ctx)
			return err
		}
	}
	for _, m := range herdrMoves(plan) {
		target := r.agent
		if m.Target >= 0 {
			target = r.ids[m.Target]
		}
		ratio := strconv.FormatFloat(m.Ratio, 'f', 6, 64)
		if _, err := r.move(ctx, m.Cell,
			"--tab", r.tab, "--split", m.Dir, "--target-pane", target, "--ratio", ratio); err != nil {
			r.putBack(ctx)
			return err
		}
		r.out[m.Cell] = false
	}
	return nil
}

// moveOut moves one slot pane into the holding tab, opening it with the first.
// Each pane goes beside or below the largest pane already there, so none of
// them is squeezed below the size herdr can split.
func (r *herdrRebuild) moveOut(ctx context.Context, i int) error {
	var (
		res herdrMoveResult
		err error
	)
	if r.hold == "" {
		res, err = r.move(ctx, i, "--new-tab")
		if err == nil {
			r.hold = res.MoveResult.CreatedTab.TabID
			if r.hold == "" {
				r.hold = res.MoveResult.Pane.TabID
			}
		}
	} else {
		target, dir := largestPane(r.holdLayout)
		res, err = r.move(ctx, i, "--tab", r.hold, "--split", dir, "--target-pane", target)
	}
	if err != nil {
		return err
	}
	r.out[i] = true
	r.holdLayout = res.MoveResult.TargetLayout.tab()
	if len(r.holdLayout.Panes) == 0 {
		if r.holdLayout, err = r.b.tabLayout(ctx, r.ids[i]); err != nil {
			return err
		}
	}
	return nil
}

// move runs one pane move of cell i's pane, with --no-focus, and follows the
// pane if herdr gives it a new identity: the registry record and the agent
// stamp are carried over, and the rename is reported to the caller.
func (r *herdrRebuild) move(ctx context.Context, i int, args ...string) (herdrMoveResult, error) {
	var res herdrMoveResult
	full := append([]string{"pane", "move", r.ids[i]}, args...)
	if err := r.b.runJSON(ctx, &res, append(full, "--no-focus")...); err != nil {
		return res, err
	}
	if !res.MoveResult.Changed {
		return res, fmt.Errorf("%s: a pane was not moved (%s)", multiplexerRefused,
			scrubHerdrIDs(res.MoveResult.Reason))
	}
	if now := res.MoveResult.Pane.PaneID; now != "" && now != r.ids[i] {
		term := res.MoveResult.Pane.TerminalID
		_ = r.b.reg.rename(r.ids[i], now, term)
		_ = r.b.layouts.renamePane(r.ids[i], now, term)
		r.ids[i] = now
		if r.renamed == nil {
			r.renamed = map[paneRef]paneRef{}
		}
		r.renamed[newPaneRef(r.orig[i])] = newPaneRef(now)
	}
	return res, nil
}

// putBack returns every pane still in the holding tab to the agent's tab,
// beside or below the largest pane there, so a rebuild that failed half way
// leaves no slot out of sight. It runs on a context the caller's cancellation
// does not reach, because stopping half way is what it repairs, and it is best
// effort: there is nothing left to report a failure to.
func (r *herdrRebuild) putBack(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for i, out := range r.out {
		if !out {
			continue
		}
		tab, err := r.b.tabLayout(ctx, r.agent)
		if err != nil {
			return
		}
		target, dir := largestPane(tab)
		if _, err := r.move(ctx, i, "--tab", r.tab, "--split", dir, "--target-pane", target); err == nil {
			r.out[i] = false
		}
	}
}

// now is a pane's current id, given the id it had when the rebuild began.
func (r *herdrRebuild) now(id string) string {
	if i := slices.Index(r.orig, id); i >= 0 {
		return r.ids[i]
	}
	return id
}

// largestPane is the pane with the most cells in a tab, and the direction that
// halves it into the squarer pieces: a cell is about twice as tall as it is
// wide, so a pane more than twice as wide as it is tall is split beside.
func largestPane(tab herdrTabLayout) (id, dir string) {
	best := -1
	for pid, g := range tab.Panes {
		area := g.Width * g.Height
		if area > best || (area == best && pid < id) {
			best, id = area, pid
			dir = "down"
			if g.Width > 2*g.Height {
				dir = "right"
			}
		}
	}
	return id, dir
}

// refocus puts the tab's focus back on a pane, by stepping into it from a
// neighbour. herdr moves focus only by direction, so a neighbour that shares
// a whole edge with the pane is tried first: from there the step can land
// nowhere else. Every candidate is tried until focus lands. It is best effort,
// because the grid is already in place and nothing about it depends on focus.
func (b *herdrBackend) refocus(ctx context.Context, anyPane, want string) {
	tab, err := b.tabLayout(ctx, anyPane)
	if err != nil || tab.Focused == want {
		return
	}
	for _, step := range focusSteps(tab, want) {
		var res struct {
			Focus struct {
				FocusedPaneID string `json:"focused_pane_id"`
			} `json:"focus"`
		}
		if b.runJSON(ctx, &res, "pane", "focus", "--pane", step.from, "--direction", step.dir) == nil &&
			res.Focus.FocusedPaneID == want {
			return
		}
	}
}

// focusStep is one directional focus move: from a pane, in a direction.
type focusStep struct{ from, dir string }

// focusSteps lists the steps into a pane from its neighbours: first those
// whose edge against it is exactly as long as its own, then any that touch it.
func focusSteps(tab herdrTabLayout, want string) []focusStep {
	t, ok := tab.Panes[want]
	if !ok {
		return nil
	}
	var whole, partial []focusStep
	ids := make([]string, 0, len(tab.Panes))
	for id := range tab.Panes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == want {
			continue
		}
		p := tab.Panes[id]
		sameRow := p.Y == t.Y && p.Height == t.Height
		sameCol := p.X == t.X && p.Width == t.Width
		overlapY := p.Y < t.Y+t.Height && t.Y < p.Y+p.Height
		overlapX := p.X < t.X+t.Width && t.X < p.X+p.Width
		var dir string
		var exact bool
		switch {
		case p.X+p.Width == t.X && overlapY:
			dir, exact = "right", sameRow
		case t.X+t.Width == p.X && overlapY:
			dir, exact = "left", sameRow
		case p.Y+p.Height == t.Y && overlapX:
			dir, exact = "down", sameCol
		case t.Y+t.Height == p.Y && overlapX:
			dir, exact = "up", sameCol
		default:
			continue
		}
		if exact {
			whole = append(whole, focusStep{id, dir})
		} else {
			partial = append(partial, focusStep{id, dir})
		}
	}
	return append(whole, partial...)
}

// herdrPaneShellPID returns the pid of the shell herdr started in a pane. It
// is detection's half of the ancestry check, run before any backend exists.
func herdrPaneShellPID(pane string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), detectProbeTimeout)
	defer cancel()
	b := &herdrBackend{bin: herdrBin()}
	return b.shellPID(ctx, pane)
}
