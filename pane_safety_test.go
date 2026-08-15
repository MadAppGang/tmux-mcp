package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ---- Pure unit tests (no tmux required) ----

// TestSocketArgsPutsConfigFlagInGlobalPrefix pins the argv layout that makes the
// headless server isolated.
//
// tmux overloads -f by position, and only one of the three meanings is the one
// we want:
//
//	tmux -f /dev/null new-session   → config file          (correct)
//	new-session -f /dev/null        → client flags         (silently wrong)
//	split-window -f                 → full-width split     (silently wrong)
//
// Both wrong forms parse without error, so nothing but this test would catch a
// regression that moves the flag after the subcommand.
func TestSocketArgsPutsConfigFlagInGlobalPrefix(t *testing.T) {
	if got := socketArgs(""); got != nil {
		t.Errorf("default server should take no global flags, got %q", got)
	}

	args := socketArgs(headlessSocket)
	want := []string{"-L", headlessSocket, "-f", os.DevNull}
	if !slices.Equal(args, want) {
		t.Fatalf("socketArgs(%q) = %q, want %q", headlessSocket, args, want)
	}

	// The flag must precede the subcommand in a fully assembled command line.
	full := append(socketArgs(headlessSocket), "new-session", "-d")
	fIdx := slices.Index(full, "-f")
	cmdIdx := slices.Index(full, "new-session")
	if fIdx < 0 || cmdIdx < 0 {
		t.Fatalf("assembled argv missing -f or subcommand: %q", full)
	}
	if fIdx > cmdIdx {
		t.Errorf("-f must come before the subcommand, or tmux reads it as client flags: %q", full)
	}
}

// TestErrorTriggerIgnoresFailWithinAWord is the regression for the error trigger
// firing on ordinary command names.
//
// The pattern used to apply (?i) to every alternative, which made the bare FAIL
// match "fail" as a substring anywhere. Watching a command whose own name
// contains it — `npm run test:failfast`, anything carrying --fail-fast — fired an
// error event against the echoed command line before a single byte of output
// existed.
func TestErrorTriggerIgnoresFailWithinAWord(t *testing.T) {
	shouldNotMatch := []string{
		"npm run test:failfast",
		"go test -failfast ./...",
		"cargo test -- --fail-fast",
		"deploy --failsafe",
		"opening failover.log",
	}
	for _, s := range shouldNotMatch {
		if errorRe.MatchString(s) {
			t.Errorf("errorRe matched %q, but 'fail' inside a word is not an error", s)
		}
	}

	// The alternatives that earn their keep. FAIL stays case-sensitive and
	// word-bounded because that is the form test runners actually print.
	shouldMatch := []string{
		"FAIL src/app.test.ts",
		"FAIL\tgithub.com/foo/bar\t0.2s",
		"Error: connection refused",
		"error: no such file",
		"panic: runtime error",
		"FATAL: could not bind",
		"npm ERR! Exit status 1: failed to compile",
		"Unhandled exception in thread main",
	}
	for _, s := range shouldMatch {
		if !errorRe.MatchString(s) {
			t.Errorf("errorRe did not match %q, but it is an error", s)
		}
	}
}

// TestDropEcho covers stripping the shell's echo of the command we typed.
//
// The hard half is not removing the echo — it is knowing where the echo stops
// and the command's own output begins.
func TestDropEcho(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		in   []string
		want []string
	}{
		{
			name: "the echo at a prompt is stripped",
			cmd:  "npm run test:failfast",
			in:   []string{"~/app $ npm run test:failfast", "PASS src/a.test.ts"},
			want: []string{"PASS src/a.test.ts"},
		},
		{
			name: "a bare echo with no prompt in front of it is stripped",
			cmd:  "npm run dev",
			in:   []string{"npm run dev", "ready in 300ms"},
			want: []string{"ready in 300ms"},
		},
		{
			// What a powerlevel10k pane actually produces: the raw echo, and then
			// the prompt redrawing the accepted line. Both have to go.
			name: "both copies of a redrawn echo are stripped",
			cmd:  `printf 'server up\n'`,
			in: []string{
				`printf 'server up\n'`,
				`❯ printf 'server up\n'`,
				"server up",
			},
			want: []string{"server up"},
		},
		{
			// The regression. Genuine output may contain the command verbatim.
			// "ready:" is not a prompt, so this line is output and must survive —
			// it is the very line a caller watching for "^ready:" is waiting on,
			// and deleting it makes a healthy server look like a timeout.
			name: "output containing the command is not an echo",
			cmd:  "./serve",
			in:   []string{"❯ ./serve", "ready: ./serve"},
			want: []string{"ready: ./serve"},
		},
		{
			name: "a redraw decorated with a trailing clock is still an echo",
			cmd:  "make build",
			in:   []string{"~/p main ❯ make build                    11:59:03 PM", "compiling"},
			want: []string{"compiling"},
		},
		{
			name: "a later line repeating the command is real output",
			cmd:  "npm run dev",
			in:   []string{"ready in 300ms", "hint: rerun with npm run dev"},
			want: []string{"ready in 300ms", "hint: rerun with npm run dev"},
		},
		{
			name: "no command (watch-pane) changes nothing",
			cmd:  "",
			in:   []string{"npm run dev", "ready"},
			want: []string{"npm run dev", "ready"},
		},
		{
			name: "no lines",
			cmd:  "npm run dev",
			in:   nil,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dropEcho(tc.in, tc.cmd)
			if !slices.Equal(got, tc.want) {
				t.Errorf("dropEcho(%q, %q)\n got: %q\nwant: %q", tc.in, tc.cmd, got, tc.want)
			}
		})
	}
}

// TestCreateWindowMarksItsPane closes the last gap in the ownership invariant:
// every pane the server creates must be marked, or it can never be reused.
// SplitPane and createSessionOnSocket did this; CreateWindow did not.
func TestCreateWindowMarksItsPane(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	client := newTmuxClient("bash")
	ctx := context.Background()

	name := uniqueSession(t)
	sess, err := client.CreateSession(ctx, name)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer exec.Command("tmux", "kill-session", "-t", name).Run() //nolint:errcheck

	win, err := client.CreateWindow(ctx, sess.SessionID, "work")
	if err != nil {
		t.Fatalf("create window: %v", err)
	}

	owned, err := client.ownedPanesInWindow(ctx, win.WindowID)
	if err != nil {
		t.Fatalf("ownedPanesInWindow: %v", err)
	}
	if !owned[win.PaneID] {
		t.Errorf("pane %s came from create-window, so it is ours, but it is not marked owned", win.PaneID)
	}
}

// TestForegroundOfGroupPrefersTheLeader is the regression for a pane at an idle
// prompt reporting someone else's command as its foreground process.
//
// Both platform implementations used to take the highest PID in the terminal's
// foreground process group — the most recently spawned process. A shell with a
// rich prompt forks helpers from its precmd hooks (powerlevel10k shells out to
// git for the VCS segment, and to language toolchains for the version segments),
// and those children run inside the shell's *own* process group rather than a
// job of their own. So the newest PID in the group was almost always a prompt
// helper, and a pane sitting idle at its prompt reported foregroundCmd "git".
//
// The effect is that every caller asking "is this pane free?" is told no. It is
// invisible in CI, whose bare shell forks nothing from its prompt, and constant
// on a developer's machine — which is precisely the shape of the flakiness this
// project has been carrying.
func TestForegroundOfGroupPrefersTheLeader(t *testing.T) {
	tests := []struct {
		name     string
		members  []procInfo
		pgid     int
		wantPID  int
		wantComm string
	}{
		{
			name: "idle shell whose prompt hook forked a helper",
			// zsh leads the group; git is a precmd child with a higher PID.
			members:  []procInfo{{PID: 100, Comm: "zsh"}, {PID: 205, Comm: "git"}},
			pgid:     100,
			wantPID:  100,
			wantComm: "zsh",
		},
		{
			name:     "leader is reported however the members are ordered",
			members:  []procInfo{{PID: 900, Comm: "go"}, {PID: 100, Comm: "zsh"}, {PID: 205, Comm: "git"}},
			pgid:     100,
			wantPID:  100,
			wantComm: "zsh",
		},
		{
			name:     "a real job owns the terminal, so the job is the foreground",
			members:  []procInfo{{PID: 300, Comm: "yes"}},
			pgid:     300,
			wantPID:  300,
			wantComm: "yes",
		},
		{
			name: "pipeline head exited, so fall back to the newest survivor",
			// The leader (400) is gone; nothing better to report than the rest.
			members:  []procInfo{{PID: 401, Comm: "sort"}, {PID: 402, Comm: "uniq"}},
			pgid:     400,
			wantPID:  402,
			wantComm: "uniq",
		},
		{
			name:     "empty group",
			members:  nil,
			pgid:     100,
			wantPID:  0,
			wantComm: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pid, comm := foregroundOfGroup(tc.members, tc.pgid)
			if pid != tc.wantPID || comm != tc.wantComm {
				t.Errorf("foregroundOfGroup(%v, %d) = (%d, %q), want (%d, %q)",
					tc.members, tc.pgid, pid, comm, tc.wantPID, tc.wantComm)
			}
		})
	}
}

// TestIdleShellSurvivesAPromptHookChild ties the fix above back to the question
// callers actually ask. paneIsIdleShell must say yes for a shell at its prompt,
// even while one of its prompt hooks still has a child alive.
func TestIdleShellSurvivesAPromptHookChild(t *testing.T) {
	// What fillPaneState now produces for a pane whose p10k prompt is mid-refresh.
	pid, comm := foregroundOfGroup(
		[]procInfo{{PID: 100, Comm: "zsh"}, {PID: 205, Comm: "git"}}, 100)
	state := &PaneState{PanePID: 100, ForegroundPID: pid, ForegroundCmd: comm, IsAlive: true}

	if !paneIsIdleShell(state) {
		t.Errorf("a shell at its prompt with a live prompt-hook child must be idle, got %+v", state)
	}

	// And the converse still holds: a real foreground job means busy.
	pid, comm = foregroundOfGroup([]procInfo{{PID: 300, Comm: "yes"}}, 300)
	state = &PaneState{PanePID: 100, ForegroundPID: pid, ForegroundCmd: comm, IsAlive: true}
	if paneIsIdleShell(state) {
		t.Errorf("a pane running yes(1) must not be idle, got %+v", state)
	}
}

// TestDiffContentSurvivesAStaticPrompt is the regression for instant commands
// being lost between two identical prompts.
//
// The monitor takes a baseline before running a command and then diffs the
// screen against it. When the prompt carries a live clock every prompt line is
// unique, so the baseline matches only where the command was typed and the
// output that follows is correctly reported as new. Under a static prompt — a
// clean shell, or CI — the prompt before and after the command are byte for
// byte identical, and anchoring on the *last* occurrence skips straight past the
// output. That is the difference between an instant command's readiness pattern
// matching and the tool waiting out its whole timeout.
func TestDiffContentSurvivesAStaticPrompt(t *testing.T) {
	const prompt = "user@host ~ %"

	// Baseline is the idle prompt; the command ran and the same prompt returned.
	baseline := prompt
	current := prompt + " expr 40 + 2\n42\n" + prompt

	got := diffContent(baseline, current)
	if !strings.Contains(got, "42") {
		t.Fatalf("diffContent dropped the command output between two identical prompts:\n%q", got)
	}

	// The append case in general: the baseline is a prefix of the new screen, so
	// the diff is exactly the appended tail.
	if got := diffContent("line1\nline2", "line1\nline2\nline3\nline4"); got != "line3\nline4" {
		t.Errorf("diffContent append case = %q, want %q", got, "line3\nline4")
	}

	// A stable screen yields no new content, which is what lets idle fire.
	if got := diffContent(current, current); got != "" {
		t.Errorf("diffContent of an unchanged screen = %q, want empty", got)
	}

	// When the screen has scrolled the baseline is no longer a prefix; fall back
	// to whatever follows its last occurrence.
	if got := diffContent("old", "scrolled\nold\nnew"); got != "new" {
		t.Errorf("diffContent scroll fallback = %q, want %q", got, "new")
	}
}

// TestVersionFlagIsNotHardcoded guards the automatic version wiring. The server
// version used to be the literal "1.0.0" in main.go, stale across six releases.
// It now comes from resolveVersion (GoReleaser's ldflags at release time, VCS
// build info otherwise), so --version must report something real — and never the
// old placeholder.
func TestVersionFlagIsNotHardcoded(t *testing.T) {
	out, err := exec.Command(testBinaryPath, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("tmux-mcp --version failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got == "" {
		t.Fatal("--version printed nothing")
	}
	if got == "1.0.0" {
		t.Errorf("--version is the old hardcoded placeholder %q; version injection is not wired", got)
	}
}

// ---- tmux-backed tests ----

// tmuxExec runs a raw tmux command, bypassing the MCP server. Used to build
// situations the server would never create for itself — most importantly, panes
// it does not own.
func tmuxExec(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		t.Fatalf("tmux %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// TestReuseIgnoresPanesWeDoNotOwn is the regression for pane reuse hijacking the
// user's shell.
//
// Reuse used to accept any idle pane in the window. A user's shell sitting at a
// prompt is idle but emphatically not free: it may be parked at an `ssh prod`, a
// `sudo -i`, or a psql session, where "reusing" it runs the agent's command in
// that context — and a later teardown kills the pane out from under them.
func TestReuseIgnoresPanesWeDoNotOwn(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	defer c.close()

	sess := createSession(t, c, uniqueSession(t))
	sessionID := sess["sessionId"].(string)
	defer c.callToolJSON(t, "kill-session", map[string]any{"sessionId": sessionID}, &map[string]any{})
	sourcePane := sess["paneId"].(string)

	// Stand in for the user's own pane: split with raw tmux, so the server never
	// sees it and never marks it. It settles at an idle shell prompt — a perfect
	// reuse candidate by the old rules.
	usersPane := tmuxExec(t, "split-window", "-d", "-t", sourcePane, "-P", "-F", "#{pane_id}")
	waitForPaneIdle(t, c, usersPane)

	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{"paneId": sourcePane}, &split)

	if got := split["paneId"].(string); got == usersPane {
		t.Fatalf("split-pane reused the user's pane %s; it must only ever reuse panes it created", got)
	}
	if reused, _ := split["reused"].(bool); reused {
		t.Errorf("split-pane reported reused=true, but the only idle pane was the user's")
	}
}

// TestReuseStillWorksForOurOwnPanes guards the fix above against overcorrecting:
// a pane the server made and left idle must still be recycled rather than piling
// up another split.
func TestReuseStillWorksForOurOwnPanes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	defer c.close()

	sess := createSession(t, c, uniqueSession(t))
	sessionID := sess["sessionId"].(string)
	defer c.callToolJSON(t, "kill-session", map[string]any{"sessionId": sessionID}, &map[string]any{})
	sourcePane := sess["paneId"].(string)

	var first map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{"paneId": sourcePane}, &first)
	ourPane := first["paneId"].(string)
	waitForPaneIdle(t, c, ourPane)

	var second map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{"paneId": sourcePane}, &second)

	if got := second["paneId"].(string); got != ourPane {
		t.Errorf("split-pane created pane %s instead of reusing our idle pane %s", got, ourPane)
	}
	if reused, _ := second["reused"].(bool); !reused {
		t.Error("split-pane should have reported reused=true")
	}
}

// TestSplitPaneDefaultsToOwnPane covers the self-location default: split-pane
// with no paneId splits the pane the server itself runs in.
//
// The fixture is built with raw tmux rather than through the server because the
// pane has to exist *before* the server starts — TMUX_PANE is read from the
// environment at spawn, exactly as it is in production, and a pane the server
// created for itself would not be the situation this default exists for.
//
// Two assertions, and the first is the safety one. The returned pane must not be
// the server's own: the agent's whole session lives in that pane, and a tool
// that answered with it would hand every later send-keys a licence to type into
// the conversation the user is having. It cannot happen here — a split never
// returns the pane it split — and that is precisely the property being pinned,
// because it is what makes "defaults to my own pane" safe at all. The second
// says the new pane landed in the user's window rather than in some session of
// the server's own making, which is the behaviour that makes the default useful.
func TestSplitPaneDefaultsToOwnPane(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-s", name)
	defer exec.Command("tmux", "kill-session", "-t", name).Run() //nolint:errcheck

	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
	wantWindow := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	c := newMCPClientInPane(t, self)

	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{}, &split)

	got, _ := split["paneId"].(string)
	if got == "" {
		t.Fatalf("split-pane with no paneId returned no pane: %v", split)
	}
	if got == self {
		t.Fatalf("split-pane returned the server's own pane %s; the pane it splits must never be the pane it returns", got)
	}
	if gotWindow, _ := split["windowId"].(string); gotWindow != wantWindow {
		t.Errorf("split-pane put the new pane in window %s, want %s — the window the server's own pane lives in", gotWindow, wantWindow)
	}
	if panes := tmuxExec(t, "list-panes", "-t", wantWindow, "-F", "#{pane_id}"); !strings.Contains(panes, got) {
		t.Errorf("pane %s is not in window %s (window holds %q)", got, wantWindow, panes)
	}
}

// TestSplitPaneOutsideTmuxNamesHeadless pins what happens when there is no self
// pane to default to.
//
// isolateTmux clears TMUX_PANE for the whole test process and newMCPClient does
// not put it back, so this server genuinely has no pane — the same position a
// server started by a launcher outside tmux is in.
//
// The assertion is on the error *text*, not merely on isError, and that is the
// point of the test. The incident this feature comes from began with an agent
// that could not distinguish "I am not in tmux" from "I passed a bad argument",
// and started shelling out to raw tmux to work out which. A message that names
// the way forward — headless — is what ends that guessing, so the wording is
// part of the contract rather than decoration.
func TestSplitPaneOutsideTmuxNamesHeadless(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	c := newMCPClient(t)

	raw := c.callToolRaw(t, "split-pane", map[string]any{})

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal split-pane result: %v\nraw: %s", err, raw)
	}
	if !result.IsError {
		t.Fatalf("split-pane with no paneId and no TMUX_PANE must be an error, got: %s", raw)
	}
	if len(result.Content) == 0 {
		t.Fatalf("error result carried no message: %s", raw)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "headless") {
		t.Errorf("error message must name the headless alternative, got %q", text)
	}
	if !strings.Contains(text, "paneId") {
		t.Errorf("error message must name the explicit-paneId alternative, got %q", text)
	}
}

// TestOwnershipSurvivesSessionScopedOptionLeak is the one that matters most.
//
// tmux user options inherit down the scope chain when interpolated in a
// pane-context format string. So a set-option that forgets -p lands at session
// scope, and #{@mcp_owner} then resolves to "agent" for EVERY pane in the user's
// session. Ownership alone would mark all of them ours — and reuse would start
// typing into the user's shells.
//
// The witness is what makes that impossible: a pane counts as ours only when
// @mcp_pane equals its own ID, and a single option value can equal only one
// pane's ID. Here the owner is leaked to session scope with no witness anywhere,
// and the correct answer is that nothing is owned.
func TestOwnershipSurvivesSessionScopedOptionLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	client := newTmuxClient("bash")
	ctx := context.Background()

	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-s", name)
	defer exec.Command("tmux", "kill-session", "-t", name).Run()

	windowID := tmuxExec(t, "display-message", "-p", "-t", name, "#{window_id}")
	tmuxExec(t, "split-window", "-d", "-t", windowID)
	tmuxExec(t, "split-window", "-d", "-t", windowID)

	panes, err := client.ListPanes(ctx, windowID)
	if err != nil {
		t.Fatalf("list panes: %v", err)
	}
	if len(panes) != 3 {
		t.Fatalf("expected 3 panes in the fixture window, got %d", len(panes))
	}

	// The slip: session scope, because -p was omitted.
	tmuxExec(t, "set-option", "-t", name, paneOptOwner, ownerAgent)

	// Sanity-check that the leak is real and would have been catastrophic, so
	// this test cannot quietly pass because the option failed to apply.
	leaked := tmuxExec(t, "list-panes", "-t", windowID, "-F", "#{"+paneOptOwner+"}")
	if got := strings.Count(leaked, ownerAgent); got != 3 {
		t.Fatalf("fixture is not exercising the leak: %d of 3 panes see the session-scoped option", got)
	}

	owned, err := client.ownedPanesInWindow(ctx, windowID)
	if err != nil {
		t.Fatalf("ownedPanesInWindow: %v", err)
	}
	if len(owned) != 0 {
		t.Fatalf("a session-scoped @mcp_owner marked %d panes as ours; the witness must reject all of them", len(owned))
	}
}

// TestHeadlessServerIgnoresUserConfig is the regression for the headless server
// not actually being isolated.
//
// tmux reads a config file when a server starts, so without -f /dev/null the
// first command on the headless socket loads the user's ~/.tmux.conf. A config
// running tmux-resurrect/tmux-continuum — or, as here, any run-shell that creates
// a session — then restores the user's workspace *into* the sandbox. It stops
// being isolated, and kill-headless-server, which iterates and kills every
// session it finds, starts killing the user's real work.
func TestHeadlessServerIgnoresUserConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	// A hostile-but-realistic config: something that materialises a session as a
	// side effect of the server starting. This is what a resurrect hook does.
	home := t.TempDir()
	conf := "new-session -d -s pwned\n"
	if err := os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte(conf), 0o600); err != nil {
		t.Fatalf("write fixture config: %v", err)
	}

	// The headless server may already be up from an earlier test, in which case
	// it would not re-read any config. Tear it down so this test observes a real
	// server start.
	_ = exec.Command("tmux", "-L", headlessSocket, "kill-server").Run()

	// The child inherits our environment, and tmux resolves ~ from $HOME.
	t.Setenv("HOME", home)

	c := newMCPClient(t)
	defer c.close()

	var created map[string]any
	c.callToolJSON(t, "create-headless", map[string]any{}, &created)
	defer c.callToolJSON(t, "kill-headless-server", map[string]any{}, &map[string]any{})

	var sessions []map[string]any
	c.callToolJSON(t, "list-sessions", map[string]any{"headless": true}, &sessions)

	for _, s := range sessions {
		if name, _ := s["name"].(string); name == "pwned" {
			t.Fatal("the user's tmux.conf was loaded into the headless server: it is not isolated")
		}
	}
	if len(sessions) != 1 {
		names := make([]string, 0, len(sessions))
		for _, s := range sessions {
			names = append(names, fmt.Sprint(s["name"]))
		}
		t.Fatalf("headless server should hold exactly the 1 session we created, got %d: %v", len(sessions), names)
	}
}

// TestSplitDoesNotStealFocus covers the -d flag on split-window.
//
// Without it, every pane the agent opens becomes the active pane, which yanks the
// user's cursor out of whatever they were typing in — typically their
// conversation with the agent.
func TestSplitDoesNotStealFocus(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	defer c.close()

	sess := createSession(t, c, uniqueSession(t))
	sessionID := sess["sessionId"].(string)
	defer c.callToolJSON(t, "kill-session", map[string]any{"sessionId": sessionID}, &map[string]any{})
	sourcePane := sess["paneId"].(string)

	var split map[string]any
	c.callToolJSON(t, "split-pane", map[string]any{"paneId": sourcePane}, &split)
	newPane := split["paneId"].(string)

	active := tmuxExec(t, "display-message", "-p", "-t", sessionID, "#{pane_id}")
	if active == newPane {
		t.Errorf("split made the new pane %s active, stealing focus from %s", newPane, sourcePane)
	}
	if active != sourcePane {
		t.Errorf("active pane is %s, want the source pane %s", active, sourcePane)
	}
}

// TestDetachedSessionIsNotEightyColumns covers sizing sessions we create
// detached.
//
// tmux falls back to default-size (80x24) for a session with no attached client.
// At 80 columns a dev server's output wraps mid-line, which silently defeats
// readiness patterns that expect their match on a single line — a failure that
// looks like the pattern being wrong rather than the pane being narrow.
func TestDetachedSessionIsNotEightyColumns(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	defer c.close()

	sess := createSession(t, c, uniqueSession(t))
	sessionID := sess["sessionId"].(string)
	defer c.callToolJSON(t, "kill-session", map[string]any{"sessionId": sessionID}, &map[string]any{})

	var panes []map[string]any
	c.callToolJSON(t, "list-panes", map[string]any{"windowId": sess["windowId"].(string)}, &panes)
	if len(panes) != 1 {
		t.Fatalf("expected 1 pane, got %d", len(panes))
	}

	width := int(panes[0]["width"].(float64))
	if width != detachedWidth {
		t.Errorf("detached pane is %d columns wide, want %d (80 wraps long output lines)", width, detachedWidth)
	}
	height := int(panes[0]["height"].(float64))
	if height != detachedHeight {
		t.Errorf("detached pane is %d rows tall, want %d (24 gives almost no scrollback to read)", height, detachedHeight)
	}
}

// TestStartAndWatchMatchesAnInstantCommand is the regression for the baseline
// race.
//
// start-and-watch used to send the command and only then let the monitor snapshot
// its diff baseline. A command that finishes in under a millisecond had already
// printed by then, so its output *was* the baseline, never counted as new, never
// matched the pattern — and the tool reported a timeout for a command that had
// succeeded instantly. The suite worked around it by padding commands with
// `sleep 0.3 &&`; this test deliberately does not.
//
// The command and the pattern share no text, so a match can only come from real
// output and never from the shell's echo of the command line. That makes this a
// test of dropEcho at the same time: were the echo still reaching the triggers,
// a pattern like "42" would need to come from the command's result either way.
func TestStartAndWatchMatchesAnInstantCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	defer c.close()

	sess := createSession(t, c, uniqueSession(t))
	sessionID := sess["sessionId"].(string)
	defer c.callToolJSON(t, "kill-session", map[string]any{"sessionId": sessionID}, &map[string]any{})
	paneID := sess["paneId"].(string)

	// Run it repeatedly: this is a race, and a race that passes once proves
	// nothing.
	for i := range 10 {
		waitForPaneIdle(t, c, paneID)

		var res map[string]any
		c.callToolJSON(t, "start-and-watch", map[string]any{
			"paneId":  paneID,
			"command": "expr 40 + 2", // echoes as "expr 40 + 2"; prints "42"
			"pattern": "^42$",
			"timeout": 10,
		}, &res)

		event, _ := res["event"].(string)
		if event == "timeout" {
			t.Fatalf("iteration %d: instant command timed out; its output landed in the baseline\noutput: %q",
				i, res["output"])
		}
		if !strings.HasPrefix(event, "pattern:") {
			t.Fatalf("iteration %d: expected the readiness pattern to fire, got event %q (detail: %v)",
				i, event, res["detail"])
		}
	}
}

// TestStartAndWatchDoesNotMatchItsOwnCommandLine is the other half of the baseline
// fix.
//
// Taking the baseline before the send is correct, but it exposes something the
// old ordering hid: the shell echoes the command line, so the command's own text
// becomes the first line of "new" output. The error and pattern triggers would
// then match against it — a command merely *named* after a failure would report an
// error before running, and a readiness pattern occurring in the command string
// would report ready before anything started. One bug was concealing the other.
func TestStartAndWatchDoesNotMatchItsOwnCommandLine(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	c := newMCPClient(t)
	defer c.close()

	sess := createSession(t, c, uniqueSession(t))
	sessionID := sess["sessionId"].(string)
	defer c.callToolJSON(t, "kill-session", map[string]any{"sessionId": sessionID}, &map[string]any{})
	paneID := sess["paneId"].(string)
	waitForPaneIdle(t, c, paneID)

	// "listening" appears in the command line but never in the output. If the
	// echo reaches the triggers, the pattern fires immediately and wrongly.
	var res map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
		"paneId":   paneID,
		"command":  "printf 'server up\\n' # listening on :3000",
		"pattern":  "listening on",
		"triggers": "idle:2",
		"timeout":  10,
	}, &res)

	if event, _ := res["event"].(string); strings.HasPrefix(event, "pattern:") {
		t.Fatalf("the readiness pattern matched the echoed command line, not real output\noutput: %q",
			res["output"])
	}
}
