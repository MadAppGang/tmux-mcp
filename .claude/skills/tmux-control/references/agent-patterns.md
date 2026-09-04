# Agent patterns with raw tmux

Composable recipes for the things agents repeatedly need. Each is built only
from the primitives in SKILL.md. Adapt freely — the point is the shape, not the
exact loop bounds.

## Contents
- [Run-and-capture (non-blocking command)](#run-and-capture)
- [Wait-for-ready (poll for a marker)](#wait-for-ready)
- [Capture a command's exit code](#exit-code)
- [Drive an interactive REPL or TUI](#repl)
- [Parallel isolated jobs](#parallel)
- [Cleanup discipline](#cleanup)
- [Choosing: in-place vs. isolated server](#choosing)

---

## Run-and-capture

Start a command in a pane, keep working, read its output later. The pane is your
durable output buffer.

```bash
# Make a pane next to your work and remember its id.
P=$(tmux split-window -v -P -F '#{pane_id}')
# Merge stderr so you capture errors too; tee to a file so output survives
# even if the pane closes.
tmux send-keys -t "$P" 'make build 2>&1 | tee /tmp/build.log' Enter

# Later — read the live pane, or the file if the pane is gone:
tmux capture-pane -t "$P" -p -S -2000 2>/dev/null || cat /tmp/build.log
```

Why `tee`: a pane discards its buffer when its command exits and the pane
closes. The file is a safety net; the pane is for live progress.

---

## Wait-for-ready

Block until a process signals readiness in its output. The marker is whatever
the tool reliably prints — "Listening on", "compiled successfully", "ready in".

```bash
P=$(tmux -L box -f /dev/null new-session -d -s dev -x 200 -y 50 -P -F '#{pane_id}')
tmux -L box send-keys -t "$P" 'npm run dev' Enter

ready=0
for i in $(seq 1 60); do          # cap the wait; never poll forever
  out=$(tmux -L box capture-pane -t "$P" -p -S -200)
  if printf '%s' "$out" | grep -qE 'Listening on|ready in [0-9]'; then
    ready=1; break
  fi
  # Bail early if the pane died (command crashed on startup).
  alive=$(tmux -L box display-message -p -t "$P" '#{pane_dead}' 2>/dev/null || echo 1)
  [ "$alive" = "1" ] && break
  sleep 1
done
[ "$ready" = "1" ] && echo "server up" || echo "timed out / crashed"
```

Two robustness habits: **a bounded loop** (so a never-ready process doesn't hang
you), and a **death check** (so a crash-on-boot exits immediately instead of
waiting the full timeout). Widen the virtual terminal (`-x 200`) so long lines
aren't wrapped before your `grep` sees them.

---

## Exit code

A pane doesn't return an exit code directly. Three reliable ways, best first:

```bash
# A) PRIMARY — anchored sentinel, polled. One line gives completion + exit code.
tmux send-keys -t "$P" 'mytool; echo "AGENT_DONE:$?"' Enter
rc=""
for i in $(seq 1 600); do
  line=$(tmux capture-pane -t "$P" -p -J | grep -oE '^AGENT_DONE:[0-9]+$' | tail -1)
  [ -n "$line" ] && { rc="${line#AGENT_DONE:}"; break; }
  sleep 0.5
done

# B) Signal a channel when done (no polling, but needs the pane shell to run tmux).
tmux send-keys -t "$P" 'mytool; echo "RC=$?"; tmux wait-for -S done' Enter
tmux wait-for done                                  # blocks until the command finishes
rc=$(tmux capture-pane -t "$P" -p | grep -oE 'RC=[0-9]+' | tail -1 | cut -d= -f2)

# C) remain-on-exit + dead-pane status — ONLY when the command is the pane's
#    top-level process (launched via new-session/split-window 'cmd'). Sent via
#    send-keys into a shell, the pane never dies and pane_dead_status stays empty.
tmux set-option -t "$P" remain-on-exit on
tmux split-window -t "$WIN" 'mytool'                # command IS the pane process
# …wait until #{pane_dead}==1…
rc=$(tmux display-message -p -t "$P" '#{pane_dead_status}')
```

Pattern A's anchoring matters: the pane also shows your *typed* command line,
which contains the literal `AGENT_DONE:$?`. Requiring `:[0-9]+$` means only real
output (where `$?` expanded) matches, and `-J` joins wrapped lines so a wrapped
command can't start a line with the sentinel. Piped commands: `$?` is the last
pipe stage's code — use `${pipestatus[1]}` (zsh) / `${PIPESTATUS[0]}` (bash).

---

## REPL

Interactive tools (`python`, `psql`, `node`, `irb`) own the pane and read
keystrokes. Drive them with `send-keys`, then capture once the prompt returns.

```bash
P=$(tmux split-window -v -P -F '#{pane_id}')
tmux send-keys -t "$P" 'python3' Enter
# wait for the >>> prompt
for i in $(seq 1 10); do tmux capture-pane -t "$P" -p | grep -q '>>>' && break; sleep 0.3; done

tmux send-keys -t "$P" 'print(2 + 2)' Enter
sleep 0.3
tmux capture-pane -t "$P" -p -S -5        # read the answer

tmux send-keys -t "$P" 'exit()' Enter      # leave cleanly
```

For TUIs (`vim`, `htop`, `lazygit`) the same idea applies with key names:
`send-keys -t "$P" ':wq' Enter` for vim, `send-keys -t "$P" 'q'` to quit most
full-screen tools. Detecting "the screen settled" is best done by capturing,
waiting a beat, capturing again, and comparing — if identical, it's idle.

---

## Parallel

Run several jobs at once, each in its own pane on an isolated server, and reap
them independently.

```bash
tmux -L jobs -f /dev/null new-session -d -s pool -P -F '#{pane_id}'   # first pane = %0
A=%0
B=$(tmux -L jobs split-window -v -P -F '#{pane_id}')
C=$(tmux -L jobs split-window -v -P -F '#{pane_id}')

tmux -L jobs send-keys -t "$A" 'pytest tests/a 2>&1 | tee /tmp/a.log; tmux wait-for -S a' Enter
tmux -L jobs send-keys -t "$B" 'pytest tests/b 2>&1 | tee /tmp/b.log; tmux wait-for -S b' Enter
tmux -L jobs send-keys -t "$C" 'pytest tests/c 2>&1 | tee /tmp/c.log; tmux wait-for -S c' Enter

tmux -L jobs wait-for a; tmux -L jobs wait-for b; tmux -L jobs wait-for c
echo "all done"; tmux -L jobs kill-server          # one teardown frees everything
```

An isolated server keeps this pool out of the user's terminal entirely, and a
single `kill-server` cleans up every pane and log-emitting process.

---

## Cleanup

Detached sessions live until killed — leaking them wastes memory and clutters
later `list-sessions`. Discipline:

- Kill the **specific** thing you made: `kill-session -t NAME` (in-place) or
  `kill-server -L SOCK` (isolated, nukes the whole sandbox).
- For isolated work, **always** `kill-server -L SOCK` at the end. If a run might
  crash before cleanup, check for a stale server first:
  `tmux -L SOCK kill-server 2>/dev/null` is a safe no-op when nothing's running.
- Don't kill the user's session or panes you didn't create. When in doubt about
  whether a pane is yours, you made it if you have its `%id` from a `-P -F`
  create; anything else is the user's.

---

## Choosing

**In-place (default server, no `-L`)** when the user should *see* the work or it
belongs beside their session: a dev server they'll watch, a build whose progress
is useful to them, a side panel. You're a guest in their window — create panes,
don't kill theirs.

**Isolated (`-L sock`)** when the work is yours alone: sandboxed test runs,
throwaway scripts, parallel jobs, anything that would clutter or disturb the
user's terminal. It's invisible to their `tmux ls`, fully under your control, and
disposable with one `kill-server`.

A good default: **use isolated unless the user benefits from watching.** It keeps
their environment clean and makes cleanup a single command.

**Before either, though:** if the tmux-mcp tools are available, a helper *slot*
covers both cases without any of this bookkeeping — a slot is a pane beside the
user by default, and `isolated: true` puts the same slot on a private server
instead. You get one number that reaches the same pane every call, no ids to
carry, and teardown via `close-pane`. Everything on this page is the manual
version, for the jobs a single pane cannot do.
