# tmux-mcp

Agent-oriented MCP server for tmux and herdr — native process detection, smart triggers,
and one handle: the slot.

## Quick start

**Prerequisites**: tmux or [herdr](#running-under-herdr) installed, Go 1.26.1+ or a
pre-built binary.

```bash
# Install via Go
go install github.com/MadAppGang/tmux-mcp/v2@latest

# Or build from source
git clone https://github.com/MadAppGang/tmux-mcp
cd tmux-mcp && go build -o tmux-mcp .
```

Add to your MCP client config (`~/.claude/settings.json` for Claude Code):

```json
{
  "mcpServers": {
    "tmux": {
      "command": "tmux-mcp",
      "args": ["--shell-type", "zsh"]
    }
  }
}
```

Get a terminal and run something in it — no setup call, no handle to carry:

```json
// A pane beside the agent is created on first use. Synchronous: blocks until
// the command finishes, and returns output plus the exit code.
{"tool": "execute-command", "params": {"command": "go build ./..."}}
// → {"slot": 1, "created": true, "output": "", "exitCode": 0, "timedOut": false}

// The same slot is the same pane next time.
{"tool": "capture-pane", "params": {}}
// → the pane's text, with structuredContent {"slot": 1}
```

## Why this exists

AI coding agents (Claude Code, Codex CLI, Gemini CLI, opencode) need terminal control.
Existing MCP servers treat tmux as a dumb pipe: send a command, poll for output, repeat.

That model breaks down in four ways:

- An agent starts `npm run dev` and needs to know when the server is **ready** — not when
  it produced any output.
- An agent runs a database migration and needs to detect when psql is **waiting for a
  password** — not guess from screen text.
- An agent chains five operations and cannot afford a lookup round-trip between each step.
- An agent watches a build in a background pane while doing other work.

This project solves those four problems directly.

## Comparison

| | nickgnd/tmux-mcp | ht-mcp | mcp-interactive-terminal | **tmux-mcp (this)** |
|---|---|---|---|---|
| Language | TypeScript | Rust | Node.js | **Go** |
| Tools | 13 | 6 | 7 | **13** |
| Dependencies | Node.js / npx | None | Node.js + node-pty | **None (single binary)** |
| Binary size | ~node_modules | ~4MB | ~node_modules | **~7MB** |
| Async monitoring | No | No | No | **Yes (blocking tools with progress notifications)** |
| Process input detection | No | No | Heuristic | **Native OS (kernel-level)** |
| Structured JSON output | Partial | No | No | **Yes (all tools)** |
| Addressing | pane IDs the agent must track | one session | one shell | **Stable slot numbers; no IDs anywhere** |
| Invisible panes | No | Only invisible | Only invisible | **Either, per slot (`isolated`)** |
| Works beside the user | No | No | No | **Yes — panes open in the window they are looking at** |
| execute-command | Polling | N/A | Heuristic | **Synchronous (blocks until the exit code is written)** |

**ht-mcp** is fast and dependency-free but works only with headless terminals it creates
itself. It cannot attach to existing sessions or manage pane layouts.

**mcp-interactive-terminal** detects prompt readiness via output-settling heuristics, which
fail when prompts vary or output is slow.

**nickgnd/tmux-mcp** pioneered the concept but has known limitations:
[#31](https://github.com/nickgnd/tmux-mcp/issues/31) (C-c sent as literal text),
[#28](https://github.com/nickgnd/tmux-mcp/issues/28) (no consecutive command support),
[#36](https://github.com/nickgnd/tmux-mcp/issues/36) (capture-pane returns too many lines).
`execute-command` returns a task ID; the agent must call `get-command-result` in a polling
loop.

## Helper slots: the only way to name a pane

A **slot** is a small number — 1, 2, 3 — that stands for a pane. It is the only handle
this server has. No tool accepts a pane, window or session identifier, no response returns
one, and a request that sends one is refused rather than quietly redirected.

Omit the argument entirely and the call goes to **slot 1**: a pane beside the agent, in
the window the user is already looking at, opened on first use and reused by every later
call.

```json
{"tool": "execute-command", "params": {"command": "npm test"}}
{"tool": "capture-pane",    "params": {}}
{"tool": "close-pane",      "params": {"slot": "all"}}
```

| `slot` | Meaning |
|---|---|
| omitted | Slot 1 — the default helper pane |
| `2`, `3`, … | A second, third, … helper pane, for running things side by side |
| `"all"` | `close-pane` only: close every helper pane this server opened |

Slots are plain numbers you choose. There is no "allocate me a free one" form: a caller
that needs more than one pane already knows how many, and picking the numbers itself is
what lets it address the same pane again on the next call.

Four properties are worth knowing.

- **The same slot is the same pane, every time.** A dev server started in slot 2 is still
  running in slot 2 on the next call. That is what makes a bare number sufficient.
- **A slot is never the agent's own pane, and never one the user opened.** Resolution
  reuses the pane already in the slot, reclaims an idle pane this server made earlier
  that holds no slot, or creates a new one. A pane counts as the server's only when it
  carries the server's marks; a shell the user opened carries none, so it is never taken,
  however idle it looks. A reclaimed pane must be an idle shell at its prompt, owned by
  the same user, with nothing waiting in its input — a TUI, a running command or a shell
  still starting up is never taken. The server has to see the shell reading, so a shell
  that reads its prompt in canonical mode (`dash`, `bash --noediting`) is never reclaimed
  on macOS, where that read cannot be seen: a busy one looks the same as an idle one, and
  a new pane is opened instead. A reclaimed pane is put back in the server's working
  directory and its screen is cleared, so `created: true` means a fresh slot in a fresh
  directory — not a fresh shell process: variables an earlier agent exported, or a
  virtualenv it activated, may still be set. The pane the server itself runs in is
  excluded from all of it, so a tool call can never type into the conversation.
- **Typing waits for the shell, and checks that it landed.** A pane a call has just
  created or reclaimed is not typed into until its shell is reading input, so a slow
  `.zshrc` cannot swallow the first command; `execute-command` waits the same way before
  every command. A shell with a line editor (zsh, bash, fish) counts as reading only once
  its editor has taken the terminal, however still its screen is. A new pane gets 30
  seconds for that; one that does not get there is closed and the call fails, so a retry
  starts from a fresh pane rather than typing into the one still starting. Where the shell has a line editor, `send-keys`, `run-in-repl`,
  `write-to-display` and `start-and-watch` send the text, confirm the editor took it, then
  send Enter and confirm that too; a call whose text was not taken fails rather than
  reporting success. A shell without a line editor (`dash`), one started with it switched
  off (`bash --noediting`, `zsh +Z`) or one the server does not recognise is used once its
  screen has settled. When `execute-command`'s command never started, it is typed once
  more, but only after the line is cleared and the screen shows the first copy gone;
  otherwise the call fails with "could not confirm the command started; it was not re-sent". Key names sent with
  `literal: false` — `C-c`, `Escape`, `Enter` — are never held back or checked, so an
  interrupt always reaches a busy pane.
- **`created` tells you when a slot changed underneath you.** Every creating call answers
  with `slot` and `created`. `created: true` means the pane is new *to that slot*, which is
  how an agent learns that the process it started there earlier is gone — the user closed
  the pane, and the process died with it.
- **Reading does not create.** `capture-pane`, `screenshot-pane`, `pane-state` and
  `watch-pane` error on a slot that was never opened, rather than splitting the user's
  window to answer a question about a pane that did not exist. They carry no `created`
  field, because nothing they do can create.

### Where slots open

Placement depends only on the slot number, so the layout is the same whichever slots
already exist:

| Slot | Opens |
|---|---|
| 1 | Beside the agent, to its right |
| 2 | Under slot 1, or beside the agent when slot 1 is not open |
| 3 | Under the agent |
| 4 and up | By halving the largest helper pane this server opened, along its longer side |

A pane the user opened is never split to make room.

### Isolated slots: a pane nobody can see

The six tools that can open a pane also take `isolated: true`. The pane is then created on
a private tmux server with no window and no client attached — nothing appears beside the
user. Everything after that is identical: the same slot number reaches it, through tools
that never mention the kind again.

```json
// First call declares the kind. It needs a slot, because a pane you cannot see
// cannot be found again by looking at the screen.
{"tool": "start-and-watch", "params": {
  "slot": 4, "isolated": true,
  "command": "npm run build", "pattern": "compiled successfully"
}}

// Later calls just use the number.
{"tool": "capture-pane", "params": {"slot": 4}}
```

Omitting `isolated` means "whichever kind this slot already is" — not "visible". Asking
for the wrong kind on an existing slot is an error, never a silent swap. `execute-command`
is the one exception to the slot requirement: `isolated: true` with no slot opens a pane,
runs the command, destroys the pane, and returns output with no slot at all.

### Closing

`close-pane` kills panes the server created. A slot record that says the user opened the
pane — older versions of this server took idle shells over as slots — is never killed: the
pane is interrupted (`C-c`) and released, and the user's shell keeps running in it.
Closing a slot that was never opened is not an error; it answers `action: "none"`. It
refuses the pane the server itself is running in even when that pane carries a valid
agent-owned record, which is what a subagent started inside an outer agent's helper pane
looks like: closing it would destroy the session the request arrived through.

## Tool reference

Thirteen tools. The six that can open a pane take `isolated`; the four that read take a
slot that must already exist; `list-slots` and `notify` take no pane at all.

### Working in a pane

| Tool | Purpose | Arguments | Returns |
|---|---|---|---|
| `open-pane` | Get a pane to work in. With no arguments returns slot 1, opening it if needed. Repeated calls for the same slot return the same pane (`created: false`) | `slot`, `isolated` | `{slot, created, isolated}` |
| `execute-command` | Run a shell command and wait for it to finish | `command` *(required)*, `slot`, `isolated`, `timeoutSeconds` | `{slot, created, output, exitCode, timedOut}` — or `{output, exitCode, timedOut}` for the ephemeral `isolated`-with-no-`slot` form |
| `send-keys` | Send keystrokes or literal text | `keys` *(required)*, `slot`, `isolated`, `literal`, `enter` | `{slot, created}` |
| `run-in-repl` | Send input to a running REPL and wait for its prompt to reappear | `input` *(required)*, `promptPattern` *(required)*, `slot`, `isolated`, `timeout` | `{slot, created, output, exited}` |
| `start-and-watch` | Start a command and monitor until a readiness pattern matches, a trigger fires, or the timeout expires | `command` *(required)*, `pattern` *(required)*, `slot`, `isolated`, `mode`, `triggers`, `timeout` | `WatchResult` |
| `write-to-display` | Write coaching text the user sees, without it entering the model's context | `text` *(required)*, `slot`, `isolated`, `clear` | `{slot, created}` |

### Reading a pane

These four error on a slot that has not been opened. None of them accepts `isolated`: they
read whichever kind the slot already is.

| Tool | Purpose | Arguments | Returns |
|---|---|---|---|
| `capture-pane` | Read terminal text. The preferred tool for output, logs and anything textual | `slot`, `lines`, `colors` | raw text, plus `structuredContent` `{slot}` |
| `screenshot-pane` | Render a PNG with full ANSI colors and layout via xterm.js. Use only when visual appearance matters | `slot`, `theme`, `output` | image, file path or HTML, plus `structuredContent` `{slot}` |
| `pane-state` | OS-level process state — alive, waiting for input, exit code | `slot` | `{slot, panePid, foregroundPid, foregroundCmd, isAlive, waitingForInput, exitCode}` |
| `watch-pane` | Monitor an already-open pane until a trigger fires | `slot`, `mode`, `triggers`, `timeout` | `WatchResult` (no `created`) |

### Slots and the user

| Tool | Purpose | Arguments | Returns |
|---|---|---|---|
| `list-slots` | What this agent has open, and what is running in each. Other agents' panes and the user's own panes are not listed | — | `[{slot, isolated, origin, foregroundCmd, isAlive}]` |
| `close-pane` | Close a helper pane. Created panes are killed; a pane the user opened is interrupted and released, never killed | `slot` (a number or `"all"`) | `[{slot, action, detail}]` — `action` is `killed`, `released`, `none` or `error` |
| `notify` | Show a transient message to the user. One-way: it cannot report anything about the terminal | `message` *(required)*, `duration` | text |

**execute-command** wraps your command with `tee` and an exit-code file, and blocks
synchronously until the command finishes and that file is written. Output and exit code come back in one response —
no polling, no separate result-fetch call.

**send-keys** separates literal text (`literal: true`, the default) from tmux key names
(`literal: false`). This fixes the original project's issue where `C-c` was sent as five
literal characters instead of an interrupt signal. To cancel a running process:

```json
{"tool": "send-keys", "params": {"keys": "C-c", "literal": false}}
```

`start-and-watch` and `watch-pane` block until a trigger fires or the timeout expires, then
return the result directly. Progress notifications are sent while monitoring.

`WatchResult` structure:

```json
{
  "slot": 2,
  "created": true,
  "event": "pattern:Serving HTTP",
  "detail": "Ready — matched: Serving HTTP on port 8765",
  "elapsed": 2.14,
  "output": "Serving HTTP on port 8765 ...",
  "paneState": {
    "panePid": 12345,
    "foregroundPid": 12347,
    "foregroundCmd": "python3",
    "isAlive": true,
    "waitingForInput": false
  }
}
```

`created` appears on `start-and-watch`, which can open a pane, and never on `watch-pane`,
which cannot.

## Smart trigger system

Triggers control when `start-and-watch` and `watch-pane` stop monitoring. Pass them as a
comma-separated string in the `triggers` parameter.

### Notification modes

| Mode | Poll interval | Notify after |
|---|---|---|
| `quick` | 500ms | 1s elapsed or 10 new lines |
| `medium` | 1s | 5s elapsed or 40 new lines |
| `slow` | 2s | 30s elapsed or 100 new lines |
| `line` | 200ms | every new line |
| `bunch` | 500ms | every 10 new lines |
| `screen` | 1s | every 40 new lines |

### Named triggers

| Trigger | Fires when |
|---|---|
| `exit` | Foreground process exits |
| `shell` | Terminal foreground command returns to an interactive shell |
| `user_input` | OS kernel reports the foreground process is blocked reading from the tty |
| `error` | New output matches `error:|fatal|panic|exception|failed|FAIL` |
| `bell` | tmux window bell flag is set |
| `idle:N` | No new output for N seconds |
| `pattern:REGEX` | A new output line matches the regex |

`start-and-watch` defaults to `exit,error`. `watch-pane` defaults to `exit,user_input,error`.

Watch a build, stop on error or after 10 seconds of silence:

```json
{
  "tool": "watch-pane",
  "params": {
    "slot": 2,
    "mode": "medium",
    "triggers": "exit,error,idle:10",
    "timeout": 120
  }
}
```

Start a dev server, stop when it prints a ready message:

```json
{
  "tool": "start-and-watch",
  "params": {
    "command": "npm run dev",
    "pattern": "Local:.*http|ready in|listening on",
    "mode": "quick",
    "triggers": "exit,error",
    "timeout": 60
  }
}
```

## Native process detection

`pane-state` and the `user_input` trigger use OS-level process inspection — not regex
pattern matching on screen output.

**Linux** reads `/proc/<pid>/wchan`. When a process blocks in `n_tty_read`, the kernel
writes that function name to wchan. The server also checks `/proc/<pid>/syscall`: syscall
number `0` (read) with file descriptor `0x0` (stdin) confirms the process is waiting for
terminal input.

**macOS** uses `sysctl kern.proc.pid` to fetch `kinfo_proc`. Two signals are combined: the
kernel wait message field (`Wmesg == "ttyin"`) and a structural check — when the terminal
foreground process group ID equals the shell's own process group and the shell is in
interruptible sleep, no child has seized the terminal.

Both platforms identify the **foreground process** by scanning the terminal foreground
process group (`TPGID`), not just the pane's shell PID.

Why this matters:

```
# Regex-based approach guesses from screen text:
"Enter password:"                    → maybe waiting for input?
"[sudo] password for jack:"          → probably?
"Password:"                          → could be a log line

# Native detection is definitive:
pane-state → {"waitingForInput": true, "foregroundCmd": "sudo"}
```

No false positives from log messages. No missed prompts from non-standard prompt formats.

`pane-state` response:

```json
{
  "slot": 1,
  "panePid": 8421,
  "foregroundPid": 8456,
  "foregroundCmd": "sudo",
  "isAlive": true,
  "waitingForInput": true
}
```

## Agent scenarios

### 1. Dev server in one slot, error monitoring in another

```json
// Slot 1 gets the server. The pane is opened beside the agent, in the window
// the user is looking at, and start-and-watch blocks until it is ready.
{"tool": "start-and-watch", "params": {
  "command": "npm run dev",
  "pattern": "Local:.*http|ready in",
  "timeout": 60
}}
// → {"slot": 1, "created": true, "event": "pattern:ready in", "elapsed": 1.8,
//    "output": "ready in 843ms"}

// Slot 2 tails the log in a second pane, side by side with the first.
{"tool": "execute-command", "params": {"slot": 2, "command": "touch dev.log"}}
{"tool": "start-and-watch", "params": {
  "slot": 2,
  "command": "tail -f dev.log",
  "pattern": "never-matches-on-purpose",
  "triggers": "error,pattern:UnhandledPromiseRejection",
  "mode": "medium",
  "timeout": 300
}}

// The server is still in slot 1 whenever you come back to it.
{"tool": "capture-pane", "params": {"slot": 1, "lines": 200}}
```

### 2. REPL session with multiple queries

```json
// Start psql in slot 2 — execute-command blocks until the shell prompt returns.
{"tool": "execute-command", "params": {"slot": 2, "command": "psql -U app mydb"}}

// First query — returns the output between the input and the next prompt.
{"tool": "run-in-repl", "params": {
  "slot": 2,
  "input": "SELECT count(*) FROM users;",
  "promptPattern": "mydb=#",
  "timeout": 10
}}
// → {"slot": 2, "created": false, "output": " count \n-------\n  1247", "exited": false}

// Second query, same REPL, same slot.
{"tool": "run-in-repl", "params": {
  "slot": 2,
  "input": "SELECT id, email FROM users LIMIT 5;",
  "promptPattern": "mydb=#"
}}
```

If a later call comes back with `created: true`, the REPL is gone — the user closed that
pane — and the next `run-in-repl` would be talking to a bare shell.

### 3. Build with an exit code check, out of sight

```json
// isolated:true with no slot opens a pane, runs the command, and destroys the
// pane inside the call. Nothing appears beside the user, and there is no slot
// afterwards because there is no pane left to address.
{"tool": "execute-command", "params": {
  "command": "go build ./...",
  "isolated": true
}}
// success → {"output": "", "exitCode": 0, "timedOut": false}
// failure → {"output": "./main.go:12: syntax error", "exitCode": 1, "timedOut": false}
```

No polling. No parsing return values from a separate call. The exit code is in the response.

`execute-command` tees stdout+stderr to a temp file and writes the exit code to another,
whose appearance is what completes the call, so the exit code accurately reflects the
original command even through pipelines.

### 4. Coaching display pane

```json
// Slot 3 becomes a display pane. The text goes on the user's screen and the
// tool returns only the slot, so it never enters the model's context.
{"tool": "write-to-display", "params": {
  "slot": 3,
  "text": "Running database migration — do not interrupt",
  "clear": true
}}
// → {"slot": 3, "created": true}

// Run the migration in slot 1 while the message stays up.
{"tool": "execute-command", "params": {
  "command": "migrate -path ./migrations -database $DATABASE_URL up"
}}

// Tell the user it is done, and hand the panes back.
{"tool": "notify",     "params": {"message": "Migration complete", "duration": 4}}
{"tool": "close-pane", "params": {"slot": "all"}}
```

`clear` wipes the pane's line buffer only on a pane the server created. On a pane the user
opened it never does: a half-typed command line belongs to them, so the screen is redrawn
around it and successive writes append.

## Comparison with the original TypeScript implementation

This project started as a port of [nickgnd/tmux-mcp](https://github.com/nickgnd/tmux-mcp)
(TypeScript, 239 stars) and became a different design.

| Area | nickgnd/tmux-mcp | This project |
|---|---|---|
| execute-command | Returns task ID; agent polls `get-command-result` | Synchronous; output + exit code in one response |
| Input detection | None; agent regexes screen text | Native OS: `/proc/wchan` (Linux), `sysctl kern.proc.pid` (macOS) |
| send-keys vs execute | Overloaded `execute-command` with `rawMode`+`noEnter` flags | Separate `send-keys` (text or key names) and `execute-command` |
| C-c handling | Issue #31: sends literal "C-c" instead of SIGINT | `send-keys` with `literal: false` interprets `C-c` as interrupt |
| Consecutive commands | Issue #28: unreliable | Each call gets its own UUID-named output and exit-code files |
| Addressing | Agent tracks pane IDs and passes them back | A slot number the caller picks; the server holds the mapping |
| Async monitoring | None | Smart triggers with progress notifications and optional channel push |
| Runtime | Node.js / npx | Single 7MB Go binary |
| Tool count | 13 | 13 |

The polling model requires an agent to call `get-command-result` in a loop, wasting round
trips and complicating timeout handling. Here the wait happens inside the server process
instead — it watches for the command's exit-code file — so the agent gets the result in a
single call.

## Configuration

```
tmux-mcp [--shell-type bash|zsh|fish] [--backend auto|tmux|herdr] [--channel] [--version]
```

`--shell-type` controls how `execute-command` captures exit codes from a pipeline:

| Shell | Exit code expression |
|---|---|
| `bash` (default) | `${PIPESTATUS[0]}` |
| `zsh` | `${pipestatus[1]}` |
| `fish` | `$status` captured before the pipe |

Match this to the shell running inside your tmux panes. A mismatch causes
`execute-command` to report exit code `0` for every command.

`--backend` selects the multiplexer. The default, `auto`, detects it at startup (see
[Running under herdr](#running-under-herdr)); `tmux` or `herdr` forces one. The tools are
the same under either: no tool, argument or response says which multiplexer is running.

`--channel` enables Claude Code channel mode. See the [Channel mode](#channel-mode)
section below.

There are no MCP resources and no tool-group switch: the server registers one surface of
thirteen tools and nothing else.

## Running under herdr

The server also runs inside herdr, the terminal multiplexer, with the same
thirteen tools and the same slots. Nothing in the tool surface changes; slot 1 opens beside
the agent's herdr pane exactly as it does beside a tmux pane.

**Detection.** With `--backend auto`, each multiplexer's claim is read from the
environment: tmux sets `TMUX_PANE`, herdr sets `HERDR_ENV=1` and `HERDR_PANE_ID`.

- One claim present: that multiplexer is used, unless the check below disproves it — the
  claimed pane's shell is found and is not an ancestor of this process, which is what a
  variable inherited by an app launched from a pane looks like. Then there is no window.
  When the check cannot run (no `TMUX` or `HERDR_SOCKET_PATH` naming a server, the
  multiplexer not answering), the claim is believed. A server started outside its pane's
  process tree on purpose should pass `--backend tmux` or `--backend herdr`.
- Both present — a herdr started from a tmux pane, or the reverse, passes the outer
  multiplexer's variables down — and each claim is checked: the claimed pane's shell must
  be an ancestor of this process. The nearest verified one wins, since the innermost
  multiplexer is the one whose window holds the agent.
- Neither verified, or no claim at all: tmux with no window. Visible slots are refused
  with a message saying so, and isolated slots still work.

**Requirements.** herdr 0.9.1 (the version this is tested against), with `herdr` on
`PATH` or its location in `HERDR_BIN_PATH`. The server reaches the herdr instance it runs
in through the `HERDR_SOCKET_PATH` herdr sets in its panes. tmux is optional: isolated
slots run on a private tmux server even under herdr, so without tmux on `PATH` an
`isolated: true` request fails and visible slots keep working.

**Where slot records live.** tmux keeps them as pane options. herdr has no equivalent, so
the server writes them to a state file, one per herdr instance:

```
$XDG_STATE_HOME/tmux-mcp/herdr/<sha256 of the herdr socket path>.json
```

`$XDG_STATE_HOME` defaults to `~/.local/state`. The file is
`{"version": 1, "panes": [{"pane_id", "terminal_id", "owner", "slot"}]}`. A record counts
only while herdr still lists that pane with the same terminal id, so a restarted herdr that
reuses pane numbers never hands a slot a stranger's pane; stale records are dropped on the
next read. Writes take a file lock and replace the file atomically, so several servers can
share one herdr. A file that does not parse is moved aside to `<name>.json.corrupt-<unix
time>` and the server starts with no records; the panes those records named are then left
alone, like any pane the user opened.

**Restored sessions.** When herdr restores a saved session, it recreates panes with new
terminal ids. The records for the old ones no longer match, so restored helper panes are
unmarked — they count as the user's, and the server opens fresh panes rather than reusing
them.

## Channel mode

tmux-mcp can act as a [Claude Code channel](https://code.claude.com/docs/en/channels),
pushing terminal events into your session proactively — without a pending tool call.

Start with `--channel`:

```bash
claude --channels server:tmux-mcp
```

Or via `.mcp.json`:

```json
{
  "mcpServers": {
    "tmux": {
      "command": "tmux-mcp",
      "args": ["--shell-type", "zsh", "--channel"]
    }
  }
}
```

When a trigger fires during `watch-pane` or `start-and-watch`, the tool result is returned
as usual **and** a `notifications/claude/channel` notification is pushed. Claude sees the
event even while working on something else.

Channel notifications include:
- `content`: human-readable event summary, e.g. `slot 2: exit — process exited (code 1)`
- `meta.slot`: which helper slot fired — the same number every tool takes
- `meta.event`: trigger type (`exit`, `error`, `user_input`, `timeout`, etc.)
- `meta.detail`: explanation
- `meta.exitCode` / `meta.isAlive`: process state (for exit events)

Without `--channel`, no channel capability is declared and no notifications are sent.

## Development

**Prerequisites:** Go 1.26.1+, tmux 3.2+; herdr 0.9.1 for the herdr scenarios, which are
skipped without it

```bash
go build -o tmux-mcp .
go test -short ./...   # skip tests requiring tmux
go test ./...          # full suite (tmux must be running)
```

**Project structure:**

| File | Purpose |
|---|---|
| `main.go` | MCP server setup, flags, tool registration |
| `detect.go` | Startup choice of multiplexer: the environment claims, the ancestry check, the `--backend` flag |
| `backend_tmux.go` | The tmux `Backend`: every tmux invocation in the program lives here |
| `backend_herdr.go` | The herdr `Backend`: every herdr invocation lives here; isolated slots are handed to a private tmux server |
| `herdr_registry.go`, `herdr_lock_*.go` | The herdr slot-record state file, its witness check, and the file lock that serialises writes |
| `agent_tools.go` | The thirteen tool registrations and their handlers |
| `helper_panes.go` | Helper-pane policy: slot resolution, reclaim, placement, ownership marks, teardown, waiting for a shell and confirming typed input landed, `execute-command`'s run-and-wait |
| `pane_arg.go` | The shared `slot`/`isolated` argument parser, the retired-argument rejection, and the response types |
| `channel.go` | Channel mode: `ChannelEmitter`, notifications, instructions |
| `triggers.go` | `NotificationMode`, `Trigger`, `monitorPane` loop, `parseTriggers`, `WatchResult` |
| `screenshot.go` | xterm.js rendering for `screenshot-pane` |
| `process.go` | `PaneState` struct, `GetPaneState` (OS dispatch) |
| `tty_darwin.go`, `tty_linux.go`, `tty_other.go` | Terminal line-discipline probe: whether the line editor is reading, and how many typed bytes are still unread |
| `process_darwin.go` | macOS: `sysctl kern.proc.pid`, `wmesg ttyin` detection |
| `process_linux.go` | Linux: `/proc/wchan`, `/proc/syscall` detection |
| `process_other.go` | Stub for other platforms (`WaitingForInput` always false) |

| Test file | Covers |
|---|---|
| `contract_test.go` | The wire contract: no schema declares a retired argument, no response carries an identifier, retired arguments are refused rather than ignored, `created` and `isolated` appear on exactly the right tools |
| `isolated_test.go` | Invisible slots: round trip, namespace isolation, the fixed kind, duplicate healing, orphan reaping |
| `reading_test.go` | The four reading tools do not create, with the pane-count control that proves the probe can see a pane appear |
| `slot_tools_test.go` | Slot resolution per tool, ownership-aware clearing, records living in tmux rather than in this process |
| `pane_safety_test.go` | Idle-shell detection, attribution of every pane the server makes |
| `readiness_test.go` | A new pane is not typed into until its shell's line editor is reading |
| `reclaim_test.go` | An unmarked user shell is never taken; a busy or TUI pane is never reclaimed; slot 1 survives a server restart |
| `placement_test.go` | The placement table, slot by slot |
| `shell_input_test.go` | Shells without a line editor or with an unknown name, C-c into a busy shell, named keys and multi-line text never reported as unsent, a single run on re-send, a reclaimed pane's directory, a new pane with no record |
| `strict_readiness_test.go` | A banner then a quiet rc is not a prompt; a slow rc never loses a retried command; a pane that never gets ready is closed; a failed reclaim is unslotted; the re-send runs at most once; canonical shells are never reclaimed |
| `detect_test.go`, `detect_probe_test.go` | The detection rule: both claims present, and a lone claim the ancestry check disproves or cannot run |
| `backend_herdr_test.go`, `herdr_send_test.go` | The same scenarios against a private herdr session (skipped without herdr); text starting with a dash |
| `herdr_registry_test.go`, `herdr_unit_test.go` | The state file's witness, locking, snapshot under the lock, write-only-on-change and corrupt-file recovery; key-name mapping; error text naming no multiplexer |
| `helper_panes_test.go`, `pane_arg_test.go` | Policy and argument-parsing units |
| `e2e_test.go`, `scenarios_test.go` | MCP client harness, individual tools, multi-step scenarios including channel mode |
| `missing_tests_test.go`, `process_test.go`, `screenshot_test.go` | Review-driven gap coverage, OS process inspection, renderer |

## Troubleshooting

### execute-command always returns exit code 0

`--shell-type` does not match the shell in the pane.

```bash
tmux-mcp --shell-type=zsh    # for zsh panes
tmux-mcp --shell-type=fish   # for fish panes
```

### A tool says "slot N does not exist"

The four reading tools do not open panes. Open the slot first with `open-pane`, or run
something in it with `execute-command` or `start-and-watch`, then read it. `list-slots`
shows every slot this agent currently has.

### A call comes back with created: true when you expected a running process

The pane that was in that slot is gone — usually the user closed it — and the process died
with it. `created: true` is the only signal you get, so treat it as "restart whatever was
running there".

### start-and-watch times out without firing

The readiness pattern never appeared in the output. Check what the pane actually printed
with `capture-pane` on the same slot, and test the regex against that before putting it
back in `start-and-watch`.

### A request is refused with "… is not accepted; address the pane by slot"

The call carried a pane, window or session identifier. This server has no such argument:
use `slot`. The refusal is deliberate — an ignored identifier would send the call to slot
1 and report success.

### pane-state reports waitingForInput=false when a prompt is visible

On Linux, some shells use `select()` or `poll()` rather than blocking `read()`, so
`/proc/wchan` shows `ep_poll` instead of `n_tty_read`. The tool falls back to
`isAlive: true, waitingForInput: false`. Use `idle:N` as a complement when precise input
detection is required.

### MCP client does not see the tools

The binary path in the MCP config is wrong or the binary is not executable.

```bash
chmod +x /path/to/tmux-mcp
/path/to/tmux-mcp --help    # verify it starts
```

## Bundled agent skill: tmux-control

The repo ships a Claude Code project skill at `.claude/skills/tmux-control/` that teaches
an agent to drive **raw tmux from the shell** — a different capability from this MCP
server, not an overlapping one.

Use the MCP tools whenever a slot will do: they are safe beside the user, they monitor,
and they never hand out a raw identifier. Reach for the skill when a task genuinely needs
tmux itself — layouts, windows, sessions, other people's panes, or anything this server
deliberately does not expose. That is also the migration path for anyone who used the
raw-tmux tool group removed in v2.0.0; see `CHANGELOG.md`.

The skill's guidance is empirically verified on tmux 3.6a and was refined through blind
multi-model review (see `.claude/skills/tmux-control/evals/`).

## License

MIT
