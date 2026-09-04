package main

import (
	"context"
	"encoding/json"
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

// TestEveryPaneWeMakeIsAttributable is the ownership invariant at both of the
// two places this server can now produce a pane, which is the whole of it: a
// visible split, and an isolated session.
//
// It replaces a test that pinned create-window, a path that no longer exists —
// the tool went with the primitives surface and the client method went with it.
// The invariant did not go anywhere, and neither did the reason: a pane we made
// that carries no attribution can never be reused, never be listed, and never be
// closed by the tool whose job that is.
//
// The two halves are attributed by DIFFERENT mechanisms, which is why both are
// asserted here rather than either standing for the other. A split is attributed
// by pane options written after the fact, so the assertion is that the marks are
// there. An isolated pane is attributed by the NAME of the session it is created
// in, atomically with its creation, and carries no marks at all until Claim
// writes them — so asserting marks on it would be asserting the wrong thing.
func TestEveryPaneWeMakeIsAttributable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	client := newTmuxClient("bash")
	backend := newTmuxBackend(client)
	ctx := context.Background()

	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-x", "200", "-y", "50", "-s", name)
	defer exec.Command("tmux", "kill-session", "-t", name).Run() //nolint:errcheck
	anchor := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")

	split, err := client.SplitPane(ctx, anchor, "vertical", 50)
	if err != nil {
		t.Fatalf("split pane: %v", err)
	}
	owned, err := client.ownedPanesInWindow(ctx, anchor)
	if err != nil {
		t.Fatalf("ownedPanesInWindow: %v", err)
	}
	if !owned[split.PaneID] {
		t.Errorf("a pane this server split is not marked owned, so nothing can ever reuse, list "+
			"or close it: %v", owned)
	}

	pane, err := backend.OpenIsolated(ctx)
	if err != nil {
		t.Fatalf("open an isolated pane: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close(context.Background(), pane) })

	panes, err := backend.IsolatedPanes(ctx)
	if err != nil {
		t.Fatalf("list this namespace's panes: %v", err)
	}
	if !slices.Contains(panes, pane) {
		t.Errorf("an isolated pane this server opened is not in its own namespace, so the sweep "+
			"that reclaims unclaimed panes would never find it: %v", panes)
	}

	// And it carries no marks yet, which is the half that is easy to "fix" into
	// a second mark writer. Attribution here is the session name; Claim is the
	// one thing that writes an option, and it has not run.
	records, err := backend.IsolatedRecords(ctx)
	if err != nil {
		t.Fatalf("read this namespace's registry: %v", err)
	}
	if _, claimed := records[pane]; claimed {
		t.Error("a freshly opened isolated pane already carries registry marks: something other " +
			"than Claim is writing them, which is what makes the write order unanalysable")
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

// TestOpenPaneDefaultsToOwnWindow covers the self-location default: open-pane
// with no arguments puts a pane beside the one the server itself runs in.
//
// The fixture is built with raw tmux rather than through the server because the
// pane has to exist *before* the server starts — TMUX_PANE is read from the
// environment at spawn, exactly as it is in production, and a pane the server
// created for itself would not be the situation this default exists for.
//
// Two assertions, and the first is the safety one. The pane behind the slot must
// not be the server's own: the agent's whole session lives in that pane, and a
// resolution that answered with it would hand every later send-keys a licence to
// type into the conversation the user is having. It cannot happen — a split
// never returns the pane it split, and helperResult refuses self besides — and
// that is precisely the property being pinned, because it is what makes
// "defaults to my own window" safe at all. The second says the new pane landed
// in the user's window rather than in some session of the server's own making,
// which is the behaviour that makes the default useful.
func TestOpenPaneDefaultsToOwnWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	name := uniqueSession(t)
	tmuxExec(t, "new-session", "-d", "-s", name)
	defer exec.Command("tmux", "kill-session", "-t", name).Run() //nolint:errcheck

	self := tmuxExec(t, "display-message", "-p", "-t", name, "#{pane_id}")
	wantWindow := tmuxExec(t, "display-message", "-p", "-t", self, "#{window_id}")

	c := newMCPClientInPane(t, self)

	var opened map[string]any
	c.callToolJSON(t, "open-pane", map[string]any{}, &opened)
	if opened["created"] != true {
		t.Fatalf("the first open-pane must report created:true, got %v", opened)
	}
	if _, hasPaneID := opened["paneId"]; hasPaneID {
		t.Fatalf("open-pane answered with a paneId: %v", opened)
	}

	got := slotPaneID(t, self, 1)
	if got == self {
		t.Fatalf("slot 1 resolved to the server's own pane %s; the pane it splits must never be the pane it returns", got)
	}
	if panes := tmuxExec(t, "list-panes", "-t", wantWindow, "-F", "#{pane_id}"); !strings.Contains(panes, got) {
		t.Errorf("pane %s is not in window %s (window holds %q)", got, wantWindow, panes)
	}
}

// TestOpenPaneOutsideTmuxNamesTheWayForward pins what happens when there is no
// self pane to default to.
//
// isolateTmux clears TMUX_PANE for the whole test process and newMCPClient does
// not put it back, so this server genuinely has no pane — the same position a
// server started by a launcher outside tmux is in.
//
// The assertion is on the error *text*, not merely on isError, and that is the
// point of the test. The incident this feature comes from began with an agent
// that could not distinguish "I am not in tmux" from "I passed a bad argument",
// and started shelling out to raw tmux to work out which. A message that names
// the way forward is what ends that guessing, so the wording is part of the
// contract rather than decoration — and the wording had to change, because the
// three routes the old sentence named (paneId, headless:true, create-headless)
// are all deleted by this release. An error naming a tool that does not exist is
// the same failure as an error that says only "no".
func TestOpenPaneOutsideTmuxNamesTheWayForward(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}

	c := newMCPClient(t)

	raw := c.callToolRaw(t, "open-pane", map[string]any{})

	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal open-pane result: %v\nraw: %s", err, raw)
	}
	if !result.IsError {
		t.Fatalf("open-pane with no TMUX_PANE must be an error, got: %s", raw)
	}
	if len(result.Content) == 0 {
		t.Fatalf("error result carried no message: %s", raw)
	}
	text := result.Content[0].Text
	if text != errNoWindowText {
		t.Errorf("error message is %q, want the one sentence this case has:\n%q", text, errNoWindowText)
	}
	for _, gone := range []string{"paneId", "create-headless", "$TMUX_PANE"} {
		if strings.Contains(text, gone) {
			t.Errorf("error message names %q, which this release deletes: %q", gone, text)
		}
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

// TestHeadlessServerIgnoresUserConfig is the regression for the isolated server
// not actually being isolated.
//
// tmux reads a config file when a server starts, so without -f /dev/null the
// first command on the isolated socket loads the user's ~/.tmux.conf. A config
// running tmux-resurrect/tmux-continuum — or, as here, any run-shell that creates
// a session — then restores the user's workspace *into* the sandbox. It stops
// being isolated, and a sweep that iterates and closes every pane it finds there
// starts closing the user's real work.
//
// It drives the backend rather than a tool, because the invariant is about the
// SOCKET and not about any one tool: every isolated slot on the surface reaches
// this socket through OpenIsolated, so proving it here proves it for all of
// them.
func TestIsolatedServerIgnoresUserConfig(t *testing.T) {
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

	// The isolated server may already be up from an earlier test, in which case
	// it would not re-read any config. Tear it down so this test observes a real
	// server start. This is the ONE place that kills that server, and it is a
	// test on an isolated TMUX_TMPDIR: the product never does it, because the
	// socket is shared and the window in which "is it up?" fails is exactly the
	// window in which a neighbour is starting one.
	_ = exec.Command("tmux", "-L", headlessSocket, "kill-server").Run()

	// tmux resolves ~ from $HOME.
	t.Setenv("HOME", home)

	backend := newTmuxBackend(newTmuxClient("bash"))
	ctx := context.Background()
	pane, err := backend.OpenIsolated(ctx)
	if err != nil {
		t.Fatalf("open an isolated pane: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", headlessSocket, "kill-server").Run() })

	out, err := exec.Command("tmux", append(socketArgs(headlessSocket),
		"list-sessions", "-F", "#{session_name}")...).Output()
	if err != nil {
		t.Fatalf("list sessions on the isolated socket: %v", err)
	}
	names := strings.Split(strings.TrimSpace(string(out)), "\n")

	for _, name := range names {
		if name == "pwned" {
			t.Fatal("the user's tmux.conf was loaded into the isolated server: it is not isolated")
		}
	}
	if len(names) != 1 {
		t.Fatalf("the isolated server should hold exactly the 1 session opening %v created, got %d: %v",
			pane, len(names), names)
	}
	if !strings.HasPrefix(names[0], backend.namespacePrefix()) {
		t.Errorf("the session is named %q, which does not carry this server's namespace %q — "+
			"attribution is the session name, so a pane in a session we cannot recognise is a "+
			"pane no sweep will ever reclaim", names[0], backend.namespacePrefix())
	}
}

// TestSplitDoesNotStealFocus covers the -d flag on split-window.
//
// Without it, every pane the agent opens becomes the active pane, which yanks the
// user's cursor out of whatever they were typing in — typically their
// conversation with the agent.
func TestSplitDoesNotStealFocus(t *testing.T) {
	c, self := agentPaneFixture(t)

	newPane := openSlot(t, c, self, 1)

	active := tmuxExec(t, "display-message", "-p", "-t", self, "#{pane_id}")
	if active == newPane {
		t.Errorf("open-pane made the new pane %s active, stealing focus from %s", newPane, self)
	}
	if active != self {
		t.Errorf("active pane is %s, want the agent's own pane %s", active, self)
	}
}

// TestDetachedSessionIsNotEightyColumns covers sizing sessions we create
// detached.
//
// tmux falls back to default-size (80x24) for a session with no attached client.
// At 80 columns a dev server's output wraps mid-line, which silently defeats
// readiness patterns that expect their match on a single line — a failure that
// looks like the pattern being wrong rather than the pane being narrow.
//
// Every detached session this server creates is now an ISOLATED one — that is
// the only path left that creates a session at all — so the test drives
// OpenIsolated. The sizing rule and the failure it prevents are unchanged; only
// the caller is.
func TestDetachedSessionIsNotEightyColumns(t *testing.T) {
	if testing.Short() {
		t.Skip("requires tmux")
	}
	backend := newTmuxBackend(newTmuxClient("bash"))
	ctx := context.Background()

	pane, err := backend.OpenIsolated(ctx)
	if err != nil {
		t.Fatalf("open an isolated pane: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close(context.Background(), pane) })

	cols, rows, err := backend.c.GetPaneDimensions(ctx, pane.target())
	if err != nil {
		t.Fatalf("read the pane's dimensions: %v", err)
	}
	if cols != detachedWidth {
		t.Errorf("detached pane is %d columns wide, want %d (80 wraps long output lines)",
			cols, detachedWidth)
	}
	if rows != detachedHeight {
		t.Errorf("detached pane is %d rows tall, want %d (24 gives almost no scrollback to read)",
			rows, detachedHeight)
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
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)

	// Run it repeatedly: this is a race, and a race that passes once proves
	// nothing.
	for i := range 10 {
		waitForPaneIdle(t, pane)

		var res map[string]any
		c.callToolJSON(t, "start-and-watch", map[string]any{
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
	c, self := agentPaneFixture(t)
	pane := openSlot(t, c, self, 1)
	waitForPaneIdle(t, pane)

	// "listening" appears in the command line but never in the output. If the
	// echo reaches the triggers, the pattern fires immediately and wrongly.
	var res map[string]any
	c.callToolJSON(t, "start-and-watch", map[string]any{
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
