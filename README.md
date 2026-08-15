# tmux-mcp

Agent-oriented MCP server for tmux — native process detection, smart triggers, zero-lookup chaining.

## Quick start

**Prerequisites**: tmux installed, Go 1.21+ or a pre-built binary.

```bash
# Install via Go
go install github.com/MadAppGang/tmux-mcp@latest

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
      "args": ["--shell-type", "zsh", "--scope", "agentic"]
    }
  }
}
```

Create a session and run a command in two calls:

```json
// Returns sessionId, windowId, and paneId — no lookup needed
{"tool": "create-session", "params": {"name": "dev"}}
// → {"sessionId": "$3", "sessionName": "dev", "windowId": "@5", "paneId": "%6"}

// Synchronous: blocks until done, returns output + exit code
{"tool": "execute-command", "params": {"paneId": "%6", "command": "go build ./..."}}
// → {"paneId": "%6", "output": "", "exitCode": 0}
```

## Why this exists

AI coding agents (Claude Code, Codex CLI, Gemini CLI, opencode) need terminal control. Existing MCP servers treat tmux as a dumb pipe: send a command, poll for output, repeat.

That model breaks down in four ways:

- An agent starts `npm run dev` and needs to know when the server is **ready** — not when it produced any output.
- An agent runs a database migration and needs to detect when psql is **waiting for a password** — not guess from screen text.
- An agent chains five operations and cannot afford a lookup round-trip between each step.
- An agent watches a build in a background pane while doing other work.

This project solves those four problems directly.

## Comparison

| | nickgnd/tmux-mcp | ht-mcp | mcp-interactive-terminal | **tmux-mcp (this)** |
|---|---|---|---|---|
| Language | TypeScript | Rust | Node.js | **Go** |
| Tools | 13 | 6 | 7 | **20** |
| Dependencies | Node.js / npx | None | Node.js + node-pty | **None (single binary)** |
| Binary size | ~node_modules | ~4MB | ~node_modules | **~7MB** |
| Async monitoring | No | No | No | **Yes (blocking tools with progress notifications)** |
| Process input detection | No | No | Heuristic | **Native OS (kernel-level)** |
| Structured JSON output | Partial | No | No | **Yes (all tools)** |
| Zero-lookup chaining | No | No | No | **Yes (IDs in every response)** |
| Split-screen / pane layout | Yes | No | No | **Yes** |
| Tmux integration | Yes | No | No | **Yes** |
| execute-command | Polling | N/A | Heuristic | **Synchronous (tmux wait-for)** |

**ht-mcp** is fast and dependency-free but works only with headless terminals it creates itself. It cannot attach to existing sessions or manage pane layouts.

**mcp-interactive-terminal** detects prompt readiness via output-settling heuristics, which fail when prompts vary or output is slow.

**nickgnd/tmux-mcp** pioneered the concept but has known limitations: [#31](https://github.com/nickgnd/tmux-mcp/issues/31) (C-c sent as literal text), [#28](https://github.com/nickgnd/tmux-mcp/issues/28) (no consecutive command support), [#36](https://github.com/nickgnd/tmux-mcp/issues/36) (capture-pane returns too many lines). `execute-command` returns a task ID; the agent must call `get-command-result` in a polling loop.

## Architecture: two layers

```
Layer 2: Agent Workflows
  start-and-watch   watch-pane    run-in-repl
  pane-state        write-to-display   display-message
  close-pane

Layer 1: Tmux Primitives
  create-session  create-window  split-pane
  list-sessions   list-windows   list-panes
  execute-command send-keys      capture-pane
  screenshot-pane resize-pane    rename-session
  kill-session    kill-window    kill-pane
```

**Layer 1** gives you full tmux control. Every response includes structured JSON with IDs so you never need a separate lookup call to chain operations.

**Layer 2** provides tools designed around agent workflows: starting a process and waiting for readiness, monitoring a pane for errors asynchronously, interacting with a running REPL, writing coaching text to a display pane, and releasing the panes the agent used.

### Helper slots: naming a pane without knowing its ID

Every tool that takes a `paneId` also accepts a `slot`, and needs neither. Omit both and
the call goes to **slot 1**: a pane beside the agent, in the window the user is already
looking at, created on first use and reused by every later call.

```json
{"tool": "execute-command", "params": {"command": "npm test"}}
{"tool": "capture-pane",    "params": {}}
{"tool": "close-pane",      "params": {"slot": "all"}}
```

| `slot` | Meaning |
|---|---|
| omitted | Slot 1 — the default helper pane |
| `2`, `3`, … | A second, third, … helper pane, for running things side by side |
| `"all"` | close-pane only: release every helper pane in this window |

Slots are plain numbers you choose. There is no "allocate me a free one" form: a caller
that needs more than one pane already knows how many, and picking the numbers itself is
what lets it address the same pane again on the next call.

Three properties are worth knowing:

- **A slot is never the agent's own pane.** Resolution can create a pane, reuse one, or
  adopt an idle unused shell already open in the window — but the pane the server itself
  runs in is excluded from all three, so a tool call can never type into the conversation.
- **Responses tell you what you got.** A resolved call answers with `paneId`, `slot`, and
  `created`. `created: true` means the pane is new *to that slot* — which is how you learn
  that the process you started there earlier is gone.
- **Explicit `paneId` still wins, and still answers exactly as it always did.** Naming a
  pane bypasses slot resolution entirely; `slot` and `created` are omitted from the
  response.

`close-pane` is the inverse: it kills panes the server created and merely interrupts and
releases panes it adopted from the user. It refuses any `paneId` it does not recognise as
its own, and it refuses the pane the server is running in even when that pane carries a
valid agent-owned record — which is what a subagent started into an outer agent's split
looks like, and closing it would destroy the session the request arrived through.
`kill-pane` remains the blunt instrument for everything else, including that one.

## Tool reference

### Layer 1: primitives

| Tool | Purpose | Key params | Returns |
|---|---|---|---|
| `list-sessions` | List all tmux sessions | — | `[{id, name, windows, attached}]` |
| `list-windows` | List windows in a session | `sessionId` | `[{id, name, active, panes}]` |
| `list-panes` | List panes with dimensions and current path | `windowId` | `[{id, title, active, width, height, currentCommand, currentPath}]` |
| `capture-pane` | Read terminal content | `slot`, `paneId`, `lines`, `colors` | raw text (+ `structuredContent`) |
| `screenshot-pane` | Visual screenshot opened in browser | `slot`, `paneId`, `theme`, `output` | image, file path, or HTML |
| `create-session` | Create a detached session | `name` (optional) | `{sessionId, sessionName, windowId, paneId}` |
| `create-window` | Add a window to a session | `sessionId`, `name` | `{windowId, windowName, paneId}` |
| `split-pane` | Get a pane to work in beside the agent. With no arguments returns helper slot 1, creating or reusing it; `paneId` splits a specific pane instead | `slot`, `paneId`, `direction`, `size` | `{paneId, windowId, reused?, slot?, created?}` |
| `send-keys` | Send keystrokes or text to a pane | `keys`, `slot`, `paneId`, `literal`, `enter` | `{paneId, slot?, created?}` |
| `execute-command` | Run a command and wait for completion | `command`, `slot`, `paneId`, `headless` | `{paneId, output, exitCode, slot?, created?}` |
| `resize-pane` | Resize absolutely or relatively | `paneId`, `width`+`height` or `direction`+`amount` | `{paneId}` |
| `rename-session` | Rename a session | `sessionId`, `newName` | `{sessionId, name}` |
| `kill-session` | Kill a session and all its windows | `sessionId` | `{killed}` |
| `kill-window` | Kill a window and all its panes | `windowId` | `{killed}` |
| `kill-pane` | Kill a single pane | `paneId` | `{killed}` |

**execute-command** wraps your command with `tee` and `tmux wait-for` so it blocks synchronously until the command finishes. Output and exit code come back in one response — no polling, no separate result-fetch call.

**send-keys** separates literal text (`literal=true`, the default) from tmux key names (`literal=false`). This fixes the original project's issue where `C-c` was sent as five literal characters instead of an interrupt signal. To cancel a running process:

```json
{"tool": "send-keys", "params": {"paneId": "%6", "keys": "C-c", "literal": false}}
```

### Layer 2: agent workflows

| Tool | Purpose | Key params | Returns |
|---|---|---|---|
| `start-and-watch` | Start a command and monitor until a readiness pattern matches | `command`, `pattern`, `slot`, `paneId`, `headless`, `mode`, `triggers`, `timeout` | `WatchResult` |
| `watch-pane` | Monitor an existing pane until a trigger fires | `slot`, `paneId`, `mode`, `triggers`, `timeout` | `WatchResult` |
| `pane-state` | Get OS-level process state | `slot`, `paneId` | `{paneId, panePid, foregroundPid, foregroundCmd, isAlive, waitingForInput, exitCode}` |
| `run-in-repl` | Send input to a running REPL and wait for the prompt | `input`, `promptPattern`, `slot`, `paneId`, `timeout` | `{paneId, output, slot?, created?}` |
| `write-to-display` | Write coaching text to a side pane without entering agent context. `clear` wipes the line buffer on a pane the server created, and never on one adopted from the user | `text`, `slot`, `paneId`, `clear` | `{paneId, slot?, created?}` |
| `display-message` | Show a one-way notification in the user's status bar. Not a query — it cannot report anything about the terminal | `message`, `duration` | text |
| `close-pane` | Close a helper pane: kills panes the server created, releases panes it adopted | `slot`, `paneId` | `{closed: [{paneId?, slot?, action}]}` |

`start-and-watch` and `watch-pane` block until a trigger fires or the timeout expires, then return the result directly. Progress notifications are sent while monitoring.

`WatchResult` structure:

```json
{
  "paneId": "%6",
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

## Smart trigger system

Triggers control when `start-and-watch` and `watch-pane` stop monitoring. Pass them as a comma-separated string in the `triggers` parameter.

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
    "paneId": "%6",
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
    "paneId": "%6",
    "command": "npm run dev",
    "pattern": "Local:.*http|ready in|listening on",
    "mode": "quick",
    "triggers": "exit,error",
    "timeout": 60
  }
}
```

## Native process detection

`pane-state` and the `user_input` trigger use OS-level process inspection — not regex pattern matching on screen output.

**Linux** reads `/proc/<pid>/wchan`. When a process blocks in `n_tty_read`, the kernel writes that function name to wchan. The server also checks `/proc/<pid>/syscall`: syscall number `0` (read) with file descriptor `0x0` (stdin) confirms the process is waiting for terminal input.

**macOS** uses `sysctl kern.proc.pid` to fetch `kinfo_proc`. Two signals are combined: the kernel wait message field (`Wmesg == "ttyin"`) and a structural check — when the terminal foreground process group ID equals the shell's own process group and the shell is in interruptible sleep, no child has seized the terminal.

Both platforms identify the **foreground process** by scanning the terminal foreground process group (`TPGID`), not just the pane's shell PID.

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
  "panePid": 8421,
  "foregroundPid": 8456,
  "foregroundCmd": "sudo",
  "isAlive": true,
  "waitingForInput": true
}
```

## Agent scenarios

### 1. Dev server with split-pane error monitoring

```json
// Create session — one call returns session, window, and pane IDs
{"tool": "create-session", "params": {"name": "webapp"}}
// → {"sessionId": "$4", "windowId": "@6", "paneId": "%8"}

// Split for log monitoring (30% width on the right)
{"tool": "split-pane", "params": {"paneId": "%8", "direction": "horizontal", "size": 30}}
// → {"paneId": "%9", "windowId": "@6"}

// Start server and wait for readiness (async MCP task)
{"tool": "start-and-watch", "params": {
  "paneId": "%8",
  "command": "npm run dev",
  "pattern": "Local:.*http|ready in",
  "timeout": 60
}}
// → {"event": "pattern:ready in", "elapsed": 1.8, "output": "ready in 843ms"}

// Watch side pane for errors while agent does other work
{"tool": "watch-pane", "params": {
  "paneId": "%9",
  "mode": "medium",
  "triggers": "error,pattern:UnhandledPromiseRejection",
  "timeout": 300
}}
```

### 2. REPL session with multiple queries

```json
// Start psql — execute-command blocks until the shell prompt returns
{"tool": "execute-command", "params": {"paneId": "%5", "command": "psql -U app mydb"}}

// First query — returns output between input and next prompt
{"tool": "run-in-repl", "params": {
  "paneId": "%5",
  "input": "SELECT count(*) FROM users;",
  "promptPattern": "mydb=#",
  "timeout": 10
}}
// → {"paneId": "%5", "output": " count \n-------\n  1247"}

// Second query, same session
{"tool": "run-in-repl", "params": {
  "paneId": "%5",
  "input": "SELECT id, email FROM users LIMIT 5;",
  "promptPattern": "mydb=#"
}}
```

### 3. Build with exit code check

```json
{"tool": "execute-command", "params": {"paneId": "%5", "command": "go build ./..."}}
// success → {"paneId": "%5", "output": "", "exitCode": 0}
// failure → {"paneId": "%5", "output": "./main.go:12: syntax error", "exitCode": 1}
```

No polling. No parsing return values from a separate call. The exit code is in the response.

`execute-command` tees stdout+stderr to a temp file and signals completion via `tmux wait-for`, so the exit code accurately reflects the original command even through pipelines.

### 4. Coaching display pane

```json
// Split off a narrow display pane
{"tool": "split-pane", "params": {"paneId": "%5", "direction": "horizontal", "size": 25}}
// → {"paneId": "%6", "windowId": "@3"}

// Write text the user sees — returns only paneId, text stays out of model context
{"tool": "write-to-display", "params": {
  "paneId": "%6",
  "text": "Running database migration — do not interrupt",
  "clear": true
}}
// → {"paneId": "%6"}

// Run the migration in the main pane
{"tool": "execute-command", "params": {
  "paneId": "%5",
  "command": "migrate -path ./migrations -database $DATABASE_URL up"
}}
```

## Comparison with the original TypeScript implementation

This project started as a port of [nickgnd/tmux-mcp](https://github.com/nickgnd/tmux-mcp) (TypeScript, 239 stars) and became a different design.

| Area | nickgnd/tmux-mcp | This project |
|---|---|---|
| execute-command | Returns task ID; agent polls `get-command-result` | Synchronous via `tmux wait-for`; output + exit code in one response |
| Input detection | None; agent regexes screen text | Native OS: `/proc/wchan` (Linux), `sysctl kern.proc.pid` (macOS) |
| send-keys vs execute | Overloaded `execute-command` with `rawMode`+`noEnter` flags | Separate `send-keys` (text or key names) and `execute-command` |
| C-c handling | Issue #31: sends literal "C-c" instead of SIGINT | `send-keys` with `literal=false` interprets `C-c` as interrupt |
| Consecutive commands | Issue #28: unreliable | Each call gets a unique UUID wait channel |
| Response IDs | Inconsistent | All tools return structured JSON with IDs; zero lookup calls needed |
| Async monitoring | None | Smart triggers with progress notifications and optional channel push |
| Runtime | Node.js / npx | Single 7MB Go binary |
| Tool count | 13 | 20 (14 primitives + 6 agent tools) |

The polling model requires an agent to call `get-command-result` in a loop, wasting round trips and complicating timeout handling. `tmux wait-for` blocks inside the server process instead, so the agent gets the result in a single call.

## Configuration

```
tmux-mcp [--shell-type bash|zsh|fish] [--scope agentic|primitives|all] [--channel]
```

`--shell-type` controls how `execute-command` captures exit codes from a pipeline:

| Shell | Exit code expression |
|---|---|
| `bash` (default) | `${PIPESTATUS[0]}` |
| `zsh` | `${pipestatus[1]}` |
| `fish` | `$status` captured before the pipe |

Match this to the shell running inside your tmux panes. A mismatch causes `execute-command` to report exit code `0` for every command.

`--scope` controls which tool groups are registered:

| Value | Tools registered |
|---|---|
| `agentic` (default) | 7 Layer 2 tools + 13 essential Layer 1 tools |
| `primitives` | All 17 Layer 1 tools only |
| `all` | All Layer 1 and Layer 2 tools |

The scope can also be set via the `TMUX_MCP_SCOPE` environment variable. The flag takes precedence.

`--channel` enables Claude Code channel mode. See the [Channel mode](#channel-mode) section below.

**MCP resources** are also exposed:

- `tmux://sessions` — JSON list of all active sessions
- `tmux://pane/{paneId}` — current terminal content of a pane

## Channel mode

tmux-mcp can act as a [Claude Code channel](https://code.claude.com/docs/en/channels), pushing terminal events into your session proactively — without a pending tool call.

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
      "args": ["--shell-type", "zsh", "--scope", "agentic", "--channel"]
    }
  }
}
```

When a trigger fires during `watch-pane` or `start-and-watch`, the tool result is returned as usual **and** a `notifications/claude/channel` notification is pushed. Claude sees the event even while working on something else.

Channel notifications include:
- `content`: human-readable event summary
- `meta.paneId`: which pane fired
- `meta.event`: trigger type (`exit`, `error`, `user_input`, `timeout`, etc.)
- `meta.detail`: explanation
- `meta.exitCode` / `meta.isAlive`: process state (for exit events)

Without `--channel`, behavior is identical to previous versions. No channel capability is declared and no notifications are sent.

## Development

**Prerequisites:** Go 1.21+, tmux 3.2+

```bash
go build -o tmux-mcp .
go test -short ./...   # skip tests requiring tmux
go test ./...          # full suite (tmux must be running)
```

**Project structure:**

| File | Purpose |
|---|---|
| `main.go` | MCP server setup, Layer 1 tool and resource registration |
| `tmux.go` | `tmuxClient`: tmux CLI wrappers, data types, `ExecuteCommand` with wait-for |
| `agent_tools.go` | Layer 2 tool registration: `start-and-watch`, `watch-pane`, `pane-state`, `run-in-repl`, `write-to-display`, `display-message`, `close-pane` |
| `helper_panes.go` | Helper-pane policy: slot resolution, placement, acquisition, teardown |
| `pane_arg.go` | The single `paneId`/`slot`/`headless` argument parser every pane-taking tool uses |
| `channel.go` | Channel mode: `ChannelEmitter`, notifications, instructions |
| `triggers.go` | `NotificationMode`, `Trigger`, `monitorPane` loop, `parseTriggers`, `WatchResult` |
| `process.go` | `PaneState` struct, `GetPaneState` (tmux query + OS dispatch) |
| `process_darwin.go` | macOS: `sysctl kern.proc.pid`, `wmesg ttyin` detection |
| `process_linux.go` | Linux: `/proc/wchan`, `/proc/syscall` detection |
| `process_other.go` | Stub for other platforms (`WaitingForInput` always false) |
| `e2e_test.go` | MCP client harness and individual tool tests (50 E2E tests across 3 files) |
| `scenarios_test.go` | Multi-step scenario tests including channel mode |
| `missing_tests_test.go` | Bug fix coverage, scope filtering, trigger and negative tests |

## Troubleshooting

### execute-command always returns exit code 0

`--shell-type` does not match the shell in the pane.

```bash
tmux-mcp --shell-type=zsh    # for zsh panes
tmux-mcp --shell-type=fish   # for fish panes
```

### start-and-watch times out without firing

The pane ID does not exist, or the readiness pattern never appears in output. Verify the pane ID with `list-panes`. Test the regex against real output with `capture-pane` before adding it to `start-and-watch`.

### pane-state reports waitingForInput=false when a prompt is visible

On Linux, some shells use `select()` or `poll()` rather than blocking `read()`, so `/proc/wchan` shows `ep_poll` instead of `n_tty_read`. The tool falls back to `isAlive=true, waitingForInput=false`. Use `idle:N` as a complement when precise input detection is required.

### list-sessions returns an empty array

No tmux server is running, or the server has no sessions. `list-sessions` returns `[]` rather than an error when tmux reports "no server running". Call `create-session` to start one.

### MCP client does not see the tools

The binary path in the MCP config is wrong or the binary is not executable.

```bash
chmod +x /path/to/tmux-mcp
/path/to/tmux-mcp --help    # verify it starts
```

## Bundled agent skill: tmux-control

The repo ships a Claude Code project skill at `.claude/skills/tmux-control/` that
teaches an agent to drive **raw tmux from the shell** — complementary to this MCP
server. Use the MCP tools for structured, watched interactions; use the skill
when an agent needs direct tmux primitives (relative-to-self pane management,
anchored exit-sentinel run-and-watch, isolated `-L` servers that survive
tmux-resurrect/continuum via `-f /dev/null`, pane ownership marking with
pane-scoped options).

The skill's guidance is empirically verified on tmux 3.6a and was refined
through blind multi-model review (see `.claude/skills/tmux-control/evals/`).

## License

MIT
