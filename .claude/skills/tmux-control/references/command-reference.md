# tmux command reference (agent-oriented)

Exhaustive flags and format variables for the commands in SKILL.md. Read the
section you need; you don't need to load the whole file.

## Contents
- [Targeting (`-t`) and the `-L` socket](#targeting)
- [Creating: new-session / new-window / split-window](#creating)
- [Reading: capture-pane / list-* / display-message](#reading)
- [Driving: send-keys](#driving)
- [Layout: resize-pane / select-pane / select-layout](#layout)
- [Lifecycle: kill-* / set-option](#lifecycle)
- [Format variables (`#{...}`)](#format-variables)
- [wait-for: cross-process synchronization](#wait-for)

---

## Targeting

Every command takes `-t <target>`. Targets resolve in this order of preference:

- **Pane ID** `%5` — unambiguous, stable for the pane's life. **Prefer this.**
- **Window ID** `@2` — stable window reference.
- `session:window.pane` — e.g. `work:0.1`, by name+index. Indices shift; avoid.
- Bare index `-t 1` — pane/window *index*, brittle. Avoid for anything you'll
  reference again.

**`-L <socket-name>`** selects which server. Omit it for the user's default
server. Provide it (consistently, on *every* call) for an isolated server. There
is also `-S <socket-path>` for an explicit path; `-L` (name under the tmux tmp
dir) is simpler and what you want.

**`-f <config-file>`** picks the config to load when the server starts. Pass
**`-f /dev/null`** on the `new-session` that first creates an isolated server so
it loads none of the user's `~/.tmux.conf`. This matters because plugins like
tmux-resurrect/continuum hook session creation to auto-restore the user's saved
sessions into *any* fresh server — `-f /dev/null` skips that, keeping your
sandbox actually empty. Only the server-creating command needs `-f`; later calls
on the same socket inherit it.

---

## Creating

### new-session
```
tmux new-session -d -s NAME [-x COLS -y ROWS] [-P -F 'FMT'] [shell-command]
```
- `-d` detached — **always use this for agent work**; the session runs with no
  attached client.
- `-s NAME` session name. Omit to get an auto-numbered name.
- `-x/-y` set the virtual terminal size of a detached session (default 80x24).
  Bump these if you're capturing wide output.
- `-P -F 'FMT'` print the new IDs. Good FMT:
  `'#{session_name} #{window_id} #{pane_id}'`.
- trailing `shell-command` runs instead of a shell; the session ends when it
  exits (unless `remain-on-exit`).

### new-window
```
tmux new-window [-t SESSION] [-n NAME] [-P -F 'FMT'] [shell-command]
```
- `-n NAME` window name.
- `-a`/`-b` insert after/before the current window.

### split-window
```
tmux split-window [-h|-v] [-t TARGET-PANE] [-l SIZE] [-P -F 'FMT'] [shell-command]
```
- `-h` split horizontally → panes side by side (vertical divider).
- `-v` split vertically → panes stacked (horizontal divider). *(This naming
  trips everyone up; `-h` = left/right.)*
- `-l SIZE` size of the new pane: `-l 40` (cells) or `-l 30%`.
- `-d` keep focus on the original pane.

---

## Reading

### capture-pane
```
tmux capture-pane -t TARGET -p [-S START] [-E END] [-e] [-J]
```
- `-p` print to stdout (otherwise it goes to a paste buffer).
- `-S START` start line: `-S -200` = 200 lines into scrollback; `-S -` = all
  scrollback.
- `-E END` end line (default = bottom of visible screen).
- `-e` include ANSI escape sequences (colors). Omit for clean text.
- `-J` join wrapped lines (preserves long lines instead of hard-wrapping).

### list-panes / list-windows / list-sessions
```
tmux list-panes   -t TARGET -F 'FMT'
tmux list-windows -t SESSION -F 'FMT'
tmux list-sessions -F 'FMT'
```
Add `-a` to `list-panes`/`list-windows` to list across all sessions/windows.

### display-message
```
tmux display-message -p -t TARGET '#{...}'
```
Best way to read a *single* value about one target without listing:
`tmux display-message -p -t %3 '#{pane_current_command}'`.

---

## Driving

### send-keys
```
tmux send-keys -t TARGET [-l] KEY-OR-TEXT [KEY-OR-TEXT ...]
```
- Multiple arguments are sent in sequence: `send-keys -t %3 'ls -la' Enter`.
- Key names: `Enter`/`C-m` (Return), `C-c` (Ctrl-C), `C-d` (EOF), `Escape`,
  `Tab`, `Space`, `BSpace`, `Up`/`Down`/`Left`/`Right`, `Home`, `End`,
  `PageUp`, `F1`…`F12`. Modifiers: `C-` (Ctrl), `M-` (Alt/Meta), `S-` (Shift).
- `-l` literal: send the argument as raw characters, never interpreted as a key
  name. Use when typing text that could look like a key (`-l 'Enter'`).
- `-H` send a hex byte. `-R` reset terminal state.

Common interactive moves: answer a prompt `send-keys -t %3 'y' Enter`; quit a
pager `send-keys -t %3 'q'`; exit a shell `send-keys -t %3 'exit' Enter` or
`send-keys -t %3 C-d`.

---

## Layout

### resize-pane
```
tmux resize-pane -t %3 [-x COLS -y ROWS] | [-L|-R|-U|-D [AMOUNT]]
```
Absolute (`-x/-y`) or relative by direction. `-Z` toggles zoom (fullscreen one
pane temporarily).

### select-pane / select-window
```
tmux select-pane -t %3            # focus a pane by id
tmux select-pane -L|-R|-U|-D       # focus the pane in that direction
tmux select-window -t @2
```

### select-layout
```
tmux select-layout -t @0 LAYOUT
```
LAYOUT ∈ `even-horizontal`, `even-vertical`, `main-horizontal`, `main-vertical`,
`tiled`. Quickest way to make a readable multi-pane window without manual sizing.

---

## Lifecycle

### kill-*
```
tmux kill-pane    -t %3      # one pane (window/session survive if others remain)
tmux kill-window  -t @1      # window + its panes
tmux kill-session -t work    # session + its windows
tmux kill-server  [-L sock]  # entire server: every session on that socket
```
For an isolated server, `kill-server -L <sock>` is the clean teardown.

### set-option / set-window-option
```
tmux set-option -t %3 remain-on-exit on     # keep pane after its command exits
tmux set-window-option -t @0 synchronize-panes on   # type to all panes at once
```
`remain-on-exit on` is the key one for agents: it lets you `capture-pane` a
command's final output instead of losing it when the pane auto-closes. The pane
shows "Pane is dead"; reset with `respawn-pane -t %3` or kill it.

---

## Format variables

Pass these inside `-F`/`-p` strings. The high-value set for agents:

| Variable | Meaning |
|----------|---------|
| `#{pane_id}` | `%5` — the stable pane id |
| `#{window_id}` | `@2` — the stable window id |
| `#{session_name}` | session name |
| `#{pane_pid}` | PID of the pane's shell |
| `#{pane_current_command}` | foreground command name (`node`, `zsh`, …) |
| `#{pane_current_path}` | pane's working directory |
| `#{pane_width}` / `#{pane_height}` | size in cells |
| `#{pane_active}` | 1 if this is the focused pane |
| `#{pane_dead}` | 1 if the command exited and `remain-on-exit` held it |
| `#{pane_dead_status}` | exit code of the dead pane's command |
| `#{window_active}` | 1 if this is the current window |
| `#{session_attached}` | number of clients attached (0 = nobody watching) |
| `#{history_size}` | lines currently in scrollback |

Conditionals exist too: `#{?pane_active,ACTIVE,}` prints `ACTIVE` only for the
active pane. Rarely needed but handy for compact status lines.

---

## wait-for

`wait-for` is a server-side signal/lock primitive — one process blocks on a
channel until another wakes it. Useful to know a backgrounded command finished
without polling:

```bash
# In the pane: run the command, then signal a channel when it's done
tmux send-keys -t %3 'make build; tmux wait-for -S build-done' Enter
# In your script: block until that signal arrives
tmux wait-for build-done
echo "build finished"
```

`-S` signals (and wakes one waiter), `-L`/`-U` lock/unlock a channel. This is
how the tmux-mcp server implements `execute-command`'s "wait until done"
behavior. Remember to repeat `-L <socket>` on both sides for an isolated server.
