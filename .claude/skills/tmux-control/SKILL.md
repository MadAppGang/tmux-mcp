---
name: tmux-control
description: >-
  Drive tmux from the command line to manage terminal sessions, windows, and
  panes — split the current pane, run a command in a new pane, capture output,
  send keystrokes, resize/navigate panes, or spin up a fully isolated background
  tmux server for sandboxed work. Use this whenever you need to run something in
  a terminal pane beside your work, watch a long-running process (dev server,
  build, test watcher, log tail), drive an interactive CLI/REPL, set up a
  multi-pane layout, or run commands in a tmux server that stays invisible to
  the user's own `tmux ls`. Trigger on any mention of tmux, splitting/managing
  panes or windows, "run this in a pane", "watch the output", side-by-side
  terminals, or background/isolated terminal sessions — even when tmux isn't
  named explicitly but the task clearly needs terminal-pane control. If the
  tmux-mcp tools (`open-pane`, `execute-command`, `start-and-watch`,
  `capture-pane`, …) are available, prefer them for the ordinary "give me a pane
  and run something in it" case and use this skill for what they do not expose:
  windows, sessions, layouts, and reading panes the agent does not own.
---

# tmux control for agents

tmux lets you place commands in terminal **panes** you can read, drive, and tear
down programmatically — instead of blocking on a single shell. This skill is the
command vocabulary for doing that well as an agent.

## First: is the tmux-mcp server available? Then use it instead

If your tools include `open-pane`, `execute-command`, `start-and-watch`,
`capture-pane` and friends, you are talking to **tmux-mcp**, and that is the
better road for the ordinary case. It gives you a pane beside the user, keeps
the same pane under the same **slot** number across calls, watches processes
with OS-level triggers instead of screen-scraping, refuses to touch the user's
panes or your own, and cleans up after itself. Everything below you would have
to get right by hand.

Come here when the job genuinely needs tmux itself, which tmux-mcp deliberately
does not expose:

- windows, sessions, and layouts (`new-window`, `select-layout`, `resize-pane`)
- inspecting or reading **someone else's** panes — the user's, another agent's
- anything on a tmux server this process did not create

**The two vocabularies do not mix.** tmux-mcp names a pane by a slot number and
nothing else; it has no argument that takes a pane, window or session id, and a
call carrying one is refused. Every `%…`/`@…` id on this page is a **`tmux` CLI
target** — a value you pass to `tmux -t`, never to a tool. If you find yourself
wanting to hand a captured id to an MCP tool, that is the signal you want a slot,
not this skill.

## The mental model (read this first)

tmux nests three things: a **server** owns **sessions**, a session owns
**windows** (full-screen tabs), a window owns **panes** (the split rectangles).
Everything is addressed by an **ID**: panes are `%0 %1 …`, windows `@0 @1 …`,
sessions by name. IDs are stable for the life of the object — capture them once
and reuse them; never parse them out of human-formatted output.

Two facts make tmux ideal for agents, and shape everything below:

1. **Sessions can be detached** (`-d`). A detached session runs with no terminal
   attached — the process lives, you read/drive it over the CLI, and the user
   need not see it. This is how you run work "in the background" without a job
   control mess.
2. **The foreground process holds the pane.** When you start `vim`/`python`/a
   dev server in a pane, *it* owns the terminal until it exits. That's why you
   drive interactive tools with `send-keys` rather than piping.

## The golden rule: capture IDs at creation

Every create command can print exactly the IDs you need with `-P -F`
(**P**rint, **F**ormat). Always do this — it removes all guessing about which
pane you just made:

```bash
# Create a detached session, print its IDs in one shot
tmux new-session -d -s work -P -F '#{session_name} #{window_id} #{pane_id}'
# -> work @0 %0

# Split the current pane, print the NEW pane's id
tmux split-window -h -P -F '#{pane_id}'
# -> %3
```

Store the printed `%id` and address every later command with `-t %id`. This is
the single most important habit — pane indices shift when panes close, but IDs
never do.

## Start here: you are *inside* a pane — orient first

You are not an outside operator typing at named panes. You are a process running
**inside one specific pane right now**, and almost everything you do is
**relative to that pane** — split next to it, look at its siblings, reuse a pane
you own. So step one, before any tmux action, is to find your own context.

**Your anchor is `$TMUX_PANE`.** tmux sets this environment variable for the
process running in a pane — it is *your* pane id. Capture it once and derive
your window and session from it. Address it **explicitly with `-t`** every time;
don't rely on tmux's no-`-t` default, which resolves to the server's *active*
pane — not necessarily you.

```bash
[ -n "$TMUX" ] || echo "NOT in tmux — there is no current pane; create your own session instead"

MYPANE="$TMUX_PANE"                                             # you are here, e.g. %2
MYWIN=$(tmux display-message -p -t "$MYPANE" '#{window_id}')    # your window,  e.g. @0
MYSESSION=$(tmux display-message -p -t "$MYPANE" '#{session_name}')  # your session, e.g. work
```

Now everything is relative to `$MYPANE` / `$MYWIN`. Two distinct ways to work,
and you should consciously pick one:

- **Relative / in-place** — operate *around your own pane* in the user's session:
  split next to `$MYPANE`, survey `$MYWIN`, reuse a pane you own. The user sees
  it. This is the default for "run this beside me / show me output."
- **Isolated** — a *separate disposable server* of your own
  (`-L sock -f /dev/null new-session`), do the work, then `kill-server`. The user
  never sees it. This is for sandboxed/throwaway jobs. (See "Isolated" below.)
  Isolated means *a separate server you spin up and tear down*, NOT a split of
  your current window. If tmux-mcp is available, `isolated: true` on a slot does
  this for you, including the teardown — reach for the raw form only when you
  need the whole server, not one pane in it.

> Edge case: if `$TMUX_PANE` ever seems wrong (rare — usually it's exactly your
> pane), ground-truth it by PID: find the pane whose `#{pane_pid}` is an ancestor
> of your shell's `$$`. A PID can't lie; env vars and active-flags occasionally
> can. Use this only as a tiebreaker, not the default path.

## Survey your surroundings before you act

With your anchor known, build a picture of what's around you. Acting blind is how
agents stomp on the user's panes or pile up duplicates.

**What other panes share my window, and what is each doing?** One
`list-panes` gives the whole picture — id, whether it's the active pane, the
foreground command, its PID, working dir, and size:

```bash
tmux list-panes -t "$MYWIN" -F \
  '#{pane_id} active=#{pane_active} cmd=#{pane_current_command} pid=#{pane_pid} cwd=#{pane_current_path} #{pane_width}x#{pane_height}'
# %0 active=1 cmd=zsh   pid=2010 cwd=/home/u/app  120x50
# %1 active=0 cmd=node  pid=2044 cwd=/home/u/app  120x24   <- a dev server
# %2 active=0 cmd=vim   pid=2099 cwd=/home/u/app  120x24
```

**Is a given pane free, or busy with an app?** tmux has no "idle" field —
*infer* it from `#{pane_current_command}`. If it's the **shell** (`zsh`, `bash`,
`fish`, `sh`…), the pane is at a prompt and **free**. Anything else (`node`,
`vim`, `python`, `npm`…) means an app holds it — **busy, leave it alone**. This
is how you answer "is the CLI running in that split still going, or done?".

```bash
cmd=$(tmux display-message -p -t %1 '#{pane_current_command}')
case "$cmd" in
  zsh|bash|fish|sh|-zsh|-bash) echo "free (shell at prompt)";;
  *) echo "busy: $cmd";;
esac
```

Caveat: this is a heuristic. A shell running a *script* (`bash deploy.sh`) shows
`bash` yet is busy; a pane could be at a shell prompt mid-way through your own
workflow. It's right the vast majority of the time — pair it with intent (did
*you* put something there?) for the rest.

**Which pane is active (where the user is looking)?** The one with
`#{pane_active}` == 1. Don't disrupt it without reason.

**Did *I* create this pane, or is it the user's?** tmux records no "creator",
so **mark your own panes** when you make them, using a pane-scoped user option
(`-p` is essential — without it the option is set session-wide and leaks to
every pane). Split **relative to your own pane** (`$MYPANE`) and mark the result:

```bash
NEW=$(tmux split-window -v -t "$MYPANE" -P -F '#{pane_id}')   # split next to ME
tmux set-option -p -t "$NEW" '@owned_by' agent               # mark it mine (NOTE the -p)

# Later, decide if a pane is safe to reuse or kill:
owner=$(tmux display-message -p -t "$NEW" '#{@owned_by}')     # 'agent' = yours, '' = user's
```

This is the reliable answer to "is this mine to reuse/kill?" — far safer than
guessing from layout. Treat unmarked panes as the user's: read them if asked,
but don't resize, reuse, or kill them.

**Putting it together — the safe-split decision.** Before creating yet another
pane, check whether you already have a free one of your own to reuse: a pane you
marked `@owned_by agent` whose `pane_current_command` is a shell. If so, reuse
it; otherwise split a new one and mark it. This keeps a window from filling with
abandoned agent panes. (`references/situational-awareness.md` has a ready-made
function for this.)

## The core relative workflow

The thing you do most: orient, open a working pane next to yourself, run a CLI in
it, watch whether it's still busy, read its output, then clean up. End to end:

```bash
# 1. orient — who am I?
MYPANE="$TMUX_PANE"
MYWIN=$(tmux display-message -p -t "$MYPANE" '#{window_id}')

# 2. open a pane next to me (relative to MYPANE), remember + mark it
WORK=$(tmux split-window -v -t "$MYPANE" -P -F '#{pane_id}')
tmux set-option -p -t "$WORK" '@owned_by' agent

# 3. run the CLI in MY pane (not the user's), appending an EXIT SENTINEL —
#    the completion signal AND the exit code in one line of output.
tmux send-keys -t "$WORK" 'npm run build 2>&1; echo "AGENT_DONE:$?"' Enter

# 4. wait for the sentinel. Match it ANCHORED WITH DIGITS: the pane also shows
#    the command line you just typed, which contains the literal text
#    AGENT_DONE:$? — requiring ^...:[0-9]+$ means only real output (where $?
#    expanded to a number) can match. -J joins wrapped lines so a wrapped
#    command line can't put the literal at column 0 either.
rc=""
for i in $(seq 1 600); do   # bound the wait: 600 x 0.5s = 5 min
  line=$(tmux capture-pane -t "$WORK" -p -J | grep -oE '^AGENT_DONE:[0-9]+$' | tail -1)
  [ -n "$line" ] && { rc="${line#AGENT_DONE:}"; break; }
  sleep 0.5
done
echo "exit code: ${rc:-timed-out}"

# 5. read what it produced
tmux capture-pane -t "$WORK" -p -S -2000

# 6. done with it — kill only the pane I made (the user's panes are untouched)
tmux kill-pane -t "$WORK"
```

Every `-t` here is an explicit id you captured — never a bare default. You split
relative to where *you* are, you only drive and kill the pane *you* own, and the
sentinel gives you both "it finished" and the exit code in one robust signal.

Notes on the sentinel:
- Need the exit code of a *piped* command (`build 2>&1 | tee log`)? `$?` would be
  tee's. Use `echo "AGENT_DONE:${pipestatus[1]:-${PIPESTATUS[0]}}"` (zsh/bash).
- The **foreground-command probe** (`#{pane_current_command}` — shell name means
  idle) remains the right tool for the *other* question — "is that pane busy
  right now?" during a survey. Use the sentinel to wait for completion of a
  command *you* launched; use the probe to assess panes you didn't.

## In-place: manage the pane/window you're in

These act on the current server (the one the user is attached to). Use them to
work *beside* the user.

| Goal | Command |
|------|---------|
| Split current pane, right (vertical divider) | `tmux split-window -h -P -F '#{pane_id}'` |
| Split current pane, below (horizontal divider) | `tmux split-window -v -P -F '#{pane_id}'` |
| Split a *specific* pane | `tmux split-window -h -t %2 -P -F '#{pane_id}'` |
| Run a command in the new pane | `tmux split-window -h 'npm run dev'` *(pane closes when cmd exits unless `remain-on-exit`)* |
| New window (tab) | `tmux new-window -n build -P -F '#{window_id} #{pane_id}'` |
| List panes w/ size, command, path | `tmux list-panes -t @0 -F '#{pane_id} #{pane_width}x#{pane_height} #{pane_current_command} #{pane_current_path}'` |
| List windows | `tmux list-windows -F '#{window_id} #{window_name} #{window_active}'` |
| Resize a pane (absolute) | `tmux resize-pane -t %3 -x 100 -y 30` |
| Resize a pane (relative) | `tmux resize-pane -t %3 -R 10` *(also `-L -U -D`)* |
| Move focus between panes | `tmux select-pane -t %3` *(or `-L/-R/-U/-D`)* |
| Even out a layout | `tmux select-layout -t @0 tiled` *(also `even-horizontal`, `main-vertical`)* |
| Rename window / session | `tmux rename-window -t @0 logs` · `tmux rename-session -t work api` |
| Kill one pane / window / session | `tmux kill-pane -t %3` · `tmux kill-window -t @1` · `tmux kill-session -t work` |

**Send input to a running thing** (the pane's foreground process receives it):

```bash
tmux send-keys -t %3 'echo hello' Enter   # type text + press Return
tmux send-keys -t %3 C-c                   # Ctrl-C
tmux send-keys -t %3 q                      # a single 'q' (e.g. quit a pager)
```

Note `send-keys` interprets key *names* (`Enter`, `C-c`, `Tab`, `Escape`). To
send text that might collide with a key name, pass `-l` (literal):
`tmux send-keys -t %3 -l 'Enter'` types the five letters, not Return.

**Read what's on a pane** (the workhorse for seeing output):

```bash
tmux capture-pane -t %3 -p                  # visible screen as plain text
tmux capture-pane -t %3 -p -S -200          # include 200 lines of scrollback
tmux capture-pane -t %3 -p -e               # keep ANSI color escape codes
```

## Isolated: a separate, invisible tmux server

When work should NOT touch the user's terminal — sandboxed commands, throwaway
background jobs, parallel agents — start a **second tmux server** on its own
socket with `-L <name>`, and load **no user config** with `-f /dev/null` on the
command that creates it. Every command targeting it must repeat the same `-L`.

```bash
# Start an isolated server + detached session in one go.
# -f /dev/null is what actually keeps it clean (see the warning below).
tmux -L agentbox -f /dev/null new-session -d -s job -P -F '#{pane_id}'
# -> %0   (this %0 lives ONLY on the agentbox socket)

tmux -L agentbox send-keys -t %0 'pytest -q' Enter
tmux -L agentbox capture-pane -t %0 -p -S -500
tmux -L agentbox list-sessions                 # only the sessions YOU created

# Tear the whole thing down when done — frees every pane and the server
tmux -L agentbox kill-server
```

The `-L` socket is the isolation boundary: forget it on one command and you'll
hit the user's real server (or "no server running"). Pick a memorable socket
name and reuse it. **Always `kill-server` when finished** so you don't leak
detached sessions.

> **Critical:** `-L` alone does **not** guarantee an empty server. If the user
> runs **tmux-resurrect / tmux-continuum** (very common), their `~/.tmux.conf`
> auto-restores *all their saved sessions* into any new server — so your "clean"
> sandbox suddenly contains copies of their work, and `list-sessions` is
> polluted. Passing **`-f /dev/null`** on the `new-session` skips their config
> (and therefore the restore hooks), giving a genuinely empty, isolated server.
> Verify with `tmux -L sock list-sessions` — you should see only what you made.

## Patterns agents actually need

These compose the primitives above. For fuller treatments (readiness polling,
exit codes, REPL driving), read `references/agent-patterns.md`.

**Run a command and collect its output** without blocking a shell:

```bash
P=$(tmux split-window -v -P -F '#{pane_id}')   # make a pane, remember its id
tmux send-keys -t "$P" 'make build 2>&1' Enter
# …do other work, then read whenever…
tmux capture-pane -t "$P" -p -S -1000
```

**Wait until a process is ready** by polling its output for a marker (a dev
server printing "Listening on", a build printing "compiled"):

```bash
P=$(tmux -L agentbox -f /dev/null new-session -d -s dev -P -F '#{pane_id}')
tmux -L agentbox send-keys -t "$P" 'npm run dev' Enter
for i in $(seq 1 30); do
  tmux -L agentbox capture-pane -t "$P" -p | grep -q 'Listening on' && break
  sleep 1
done
```

**Detect a pane finished / went idle:** `pane_current_command` returns to the
shell name (`bash`/`zsh`) when the foreground command exits:

```bash
tmux list-panes -t %3 -F '#{pane_current_command}'   # 'node' = busy, 'zsh' = idle
```

## Gotchas that bite agents

- **A pane closes when its command exits**, discarding output. To inspect a
  finished command's last screen, set `tmux set-option -t %3 remain-on-exit on`
  *before* it exits, or capture into a file: `… 'make 2>&1 | tee /tmp/out.log'`.
- **No server yet?** The first `new-session` starts one. Targeting a socket with
  no server ("no server running on …") just means nothing's there yet — create,
  don't panic.
- **`send-keys` needs `Enter`** as a separate argument to actually run a typed
  command — `send-keys 'ls'` types `ls` but never presses Return.
- **Prefer IDs over names/indices.** `kill-pane -t 1` targets pane *index* 1
  which shifts as panes close; `kill-pane -t %5` is unambiguous forever. If you
  must target a session by name, `-t =name` (leading `=`) forces an exact match
  on tmux versions where bare names can prefix/fuzzy-match.
- **"The first pane" is not necessarily `.0` — and `.0` may not even error.**
  With `pane-base-index 1` (common in user configs), panes are indexed 1,2,….
  Verified on tmux 3.6a: `-t sess:1.0` did NOT fail — it silently resolved to
  the *second* pane (`%53` when panes were `1:%52 2:%53`). A wrong-but-valid
  answer is worse than an error. Never assume index `0`; get real ids from
  `list-panes -F '#{pane_id}'` (or `$TMUX_PANE` for yourself).
- **Sentinel greps can match the command you typed.** The pane shows your typed
  line (`…; echo "AGENT_DONE:$?"`) as well as its output (`AGENT_DONE:0`).
  A naive `grep AGENT_DONE` fires on the typed line and reports completion
  instantly. Defuse it by requiring the expanded exit code:
  `grep -E '^AGENT_DONE:[0-9]+$'` — the typed line has a literal `$?`, never
  digits — and capture with `-J` so a wrapped command line can't start a line
  with the sentinel text.
- **Don't chain tmux commands with `;` in one invocation.** In
  `tmux cmd1 \; cmd2` the escaped `;` separates *tmux* commands, and quoting
  layers (your shell → tmux → the pane's shell) get confusing fast. Run one tmux
  command per invocation, and single-quote `-F '#{...}'` format strings so your
  shell doesn't eat the braces.

## Reference material

- `references/situational-awareness.md` — knowing where you are: `$TMUX_PANE`,
  surveying the window, free-vs-busy, pane ownership markers, and a ready-made
  find-or-create-your-work-pane function.
- `references/command-reference.md` — exhaustive flag/format-variable tables for
  every command, including all `#{...}` format variables worth knowing.
- `references/agent-patterns.md` — robust readiness polling, capturing exit
  codes, driving REPLs/TUIs, running parallel isolated jobs, cleanup discipline.
