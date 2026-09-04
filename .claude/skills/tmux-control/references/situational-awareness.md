# Situational awareness: knowing your terminal surroundings

Before an agent splits, reuses, or kills a pane, it should know where it is and
what's around it. This file expands the "Know where you are" section of SKILL.md
with copy-pasteable helpers and the edge cases.

## Contents
- [Where am I? (`$TMUX_PANE`)](#where-am-i)
- [Survey the window](#survey)
- [Free vs. busy: the heuristic and its limits](#free-vs-busy)
- [Ownership: marking your own panes](#ownership)
- [Ready-made: find-or-create your work pane](#find-or-create)
- [Cross-window / cross-session awareness](#cross)

---

## Where am I?

A process running inside tmux inherits **`$TMUX_PANE`** (the pane id) and
**`$TMUX`** (`socket,pid,session` — proof you're in tmux at all). Your agent
shell has these; use them instead of assuming.

```bash
[ -n "$TMUX" ] || { echo "not inside tmux"; }      # no current pane to reason about
MYPANE="$TMUX_PANE"                                  # e.g. %2 — your anchor
MYWIN=$(tmux display-message -p -t "$MYPANE" '#{window_id}')        # YOUR window
MYSESSION=$(tmux display-message -p -t "$MYPANE" '#{session_name}') # YOUR session
```

Always pass `-t "$MYPANE"` rather than relying on a bare `display-message`. With
no `-t`, tmux reports about the server's *active* pane — which is wherever the
user last focused, not necessarily where you are. Anchoring on `$TMUX_PANE`
makes "me" explicit and correct even when the user is looking at another pane.

If `$TMUX_PANE` ever looks wrong, ground-truth it by PID: the pane whose
`#{pane_pid}` is an ancestor of your shell's `$$` is truly yours. Use this only
as a tiebreaker — env-var anchoring is the normal path.

---

## Survey

One `list-panes` call, formatted, is your whole-window situational snapshot.
Request exactly the fields you'll branch on:

```bash
tmux list-panes -t "$MYWIN" -F \
  '#{pane_id}|#{pane_active}|#{pane_current_command}|#{pane_pid}|#{pane_current_path}|#{@owned_by}'
```

Field-by-field, the useful ones:

| Field | Tells you |
|-------|-----------|
| `#{pane_id}` | stable handle to address it |
| `#{pane_active}` | `1` = the pane the user is focused on |
| `#{pane_current_command}` | foreground app (`node`) or shell (`zsh`) → busy/free |
| `#{pane_pid}` | the shell's PID (root of the process tree in that pane) |
| `#{pane_current_path}` | working directory — useful to spot which project a pane is in |
| `#{pane_start_command}` | the command the pane was launched with, if any |
| `#{@owned_by}` | your marker (empty = not yours) |
| `#{pane_dead}` / `#{pane_dead_status}` | a finished pane held by `remain-on-exit`, and its exit code |

To survey **every** pane across all windows of the session, add `-s`:
`tmux list-panes -s -t "$MYSESSION" -F '...'`.

---

## Free vs. busy

tmux exposes no "idle" boolean. Infer from the foreground command:

- **Foreground is a shell** (`zsh bash fish sh dash ksh`, and login forms
  `-zsh`/`-bash`) → the pane is sitting at a prompt → **free**.
- **Anything else** → an app owns the terminal → **busy**.

```bash
is_free() {  # is_free <pane-id> -> exit 0 if free
  case "$(tmux display-message -p -t "$1" '#{pane_current_command}')" in
    zsh|-zsh|bash|-bash|fish|sh|-sh|dash|ksh|tcsh|csh) return 0;;
    *) return 1;;
  esac
}
```

**Limits, so you don't over-trust it:**
- A shell running a script (`bash deploy.sh`) reports `bash` but is busy. If you
  need certainty, also check whether the shell has a child:
  `pgrep -P "$(tmux display-message -p -t %1 '#{pane_pid}')"` — output means a
  child process is running, so it's busy despite a shell name.
- "Free" means "at a prompt," not "safe to commandeer." A pane the user left at
  a prompt is still theirs. Combine free-ness with **ownership** before reusing.
- The OS "waiting for input" signal is unreliable across platforms (an idle
  shell can report not-waiting on Linux), so prefer the foreground-command
  heuristic above over trying to read process wait-state.

---

## Ownership

tmux has no creator attribute. Establish one yourself with a **pane-scoped**
user option at creation. The `-p` flag is mandatory — without it, `set-option`
writes a session option that every pane then appears to share.

```bash
mark_mine()  { tmux set-option -p -t "$1" '@owned_by' agent; }   # NOTE: -p
is_mine()    { [ "$(tmux display-message -p -t "$1" '#{@owned_by}')" = agent ]; }

P=$(tmux split-window -v -P -F '#{pane_id}'); mark_mine "$P"
```

Rule of thumb: **only resize, reuse, or kill panes you marked.** Unmarked panes
belong to the user — read them when asked, but don't disturb them. The marker
also survives across your own commands, so a later step can safely discover
which panes are yours to clean up:

```bash
# kill every pane you created in this window, leave the user's untouched
tmux list-panes -t "$MYWIN" -F '#{pane_id} #{@owned_by}' \
  | awk '$2=="agent"{print $1}' | while read -r p; do tmux kill-pane -t "$p"; done
```

---

## Find-or-create

The decision that prevents pane sprawl: reuse a free pane you already own,
otherwise make (and mark) a new one. Drop-in:

```bash
# work_pane <window-id> -> prints a pane id you own and is free to use
work_pane() {
  local win="$1" p owner cmd
  while read -r p owner cmd; do
    [ "$owner" = agent ] || continue                 # must be yours
    case "$cmd" in zsh|-zsh|bash|-bash|fish|sh|-sh|dash|ksh) ;; *) continue;; esac  # must be free
    echo "$p"; return 0
  done < <(tmux list-panes -t "$win" -F '#{pane_id} #{@owned_by} #{pane_current_command}')

  # none free → create one and mark it
  p=$(tmux split-window -v -t "$win" -P -F '#{pane_id}')
  tmux set-option -p -t "$p" '@owned_by' agent
  echo "$p"
}

P=$(work_pane "$MYWIN")
tmux send-keys -t "$P" 'your-command' Enter
```

This is the raw-tmux equivalent of the reuse logic the tmux-mcp server builds
into a helper **slot** (`open-pane`, and every other tool that takes `slot`) —
there the server owns the policy and hands you a number; here you own the policy
explicitly and hold the pane id yourself.

---

## Cross

Sometimes you need the bigger picture — every window and session, not just
yours:

```bash
tmux list-windows -t "$MYSESSION" -F '#{window_id} #{window_name} active=#{window_active} panes=#{window_panes}'
tmux list-sessions -F '#{session_name} windows=#{session_windows} attached=#{session_attached}'
```

`#{session_attached}` == 0 means no client is watching that session — typical for
detached/background work. `#{window_active}`/`#{pane_active}` point at exactly
where the user's eyes are, which is the one place to be most careful.
