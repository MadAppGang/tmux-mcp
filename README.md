# tmux-mcp

MCP server giving AI agents intelligent control over tmux terminal sessions. Single Go binary, no runtime dependencies.

## Quick start

```bash
# Build
git clone https://github.com/MadAppGang/tmux-mcp
cd tmux-mcp
go build -o tmux-mcp .
```

Configure in Claude Code (`~/.claude/settings.json`) or any MCP client:

```json
{
  "mcpServers": {
    "tmux": {
      "command": "/path/to/tmux-mcp",
      "args": ["--shell-type=zsh"]
    }
  }
}
```

Restart your MCP client. The server exposes 20 tools immediately.

## Features

- **Single binary** — no Node.js, no npm, no runtime. One file to deploy.
- **Two-layer tool design** — Layer 1 wraps tmux primitives; Layer 2 adds agent-oriented workflows.
- **Native process state detection** — reads `/proc/wchan` on Linux and `sysctl kern.proc.pid` on macOS to detect when a process is blocked waiting for terminal input. No regex heuristics.
- **Smart trigger system** — six notification presets plus named triggers (`exit`, `shell`, `idle:N`, `user_input`, `error`, `bell`, `pattern:REGEX`).
- **MCP Tasks API** — `start-and-watch` and `watch-pane` run as async tasks with streaming progress notifications.
- **Structured JSON output** — every tool returns IDs (`sessionId`, `windowId`, `paneId`) so you can chain calls without lookups.

## Tools reference

### Layer 1: primitives

| Tool | Description |
|---|---|
| `list-sessions` | List all active tmux sessions |
| `list-windows` | List windows in a session |
| `list-panes` | List panes with dimensions, current command, and path |
| `capture-pane` | Read terminal content from a pane (raw or ANSI-colored) |
| `create-session` | Create a detached session; returns `sessionId`, `windowId`, `paneId` |
| `create-window` | Create a window in a session; returns `windowId`, `paneId` |
| `split-pane` | Split a pane horizontally or vertically; returns new `paneId` |
| `send-keys` | Send literal text or tmux key names (`C-c`, `Enter`, `Escape`) to a pane |
| `execute-command` | Run a command synchronously; returns full output and exit code |
| `resize-pane` | Resize by absolute dimensions or relative direction/amount |
| `rename-session` | Rename a session |
| `kill-session` | Kill a session and all its windows |
| `kill-window` | Kill a window and all its panes |
| `kill-pane` | Kill a single pane |

### Layer 2: agent workflows

| Tool | Description |
|---|---|
| `start-and-watch` | Start a command and monitor until a readiness pattern matches or a trigger fires (async task) |
| `watch-pane` | Monitor an existing pane until a trigger fires (async task) |
| `pane-state` | Get OS-level process state: `isAlive`, `waitingForInput`, `foregroundCmd`, `foregroundPid` |
| `run-in-repl` | Send input to a running REPL and wait for the prompt to reappear |
| `write-to-display` | Write text to a pane as a side-channel display; returns only `paneId` so the text stays out of model context |
| `display-message` | Show a transient notification in the tmux status bar |

## Trigger system

`start-and-watch` and `watch-pane` fire when a named trigger matches. Pass triggers as a comma-separated string.

### Notification modes

Controls how often the monitor polls and when it sends progress notifications:

| Mode | Poll interval | Time threshold | Line threshold |
|---|---|---|---|
| `quick` | 500ms | 1s | 10 lines |
| `medium` | 1s | 5s | 40 lines |
| `slow` | 2s | 30s | 100 lines |
| `line` | 200ms | — | 1 line |
| `bunch` | 500ms | — | 10 lines |
| `screen` | 1s | — | 40 lines |

### Named triggers

| Trigger | Fires when |
|---|---|
| `exit` | The pane's process group exits (detected via `pane_dead` flag) |
| `shell` | The foreground command returns to a shell (`bash`, `zsh`, `fish`, etc.) |
| `idle:N` | No new output for N seconds |
| `user_input` | OS-level detection: foreground process is blocked reading from the terminal |
| `error` | New output matches `error:|fatal|panic|exception|failed|FAIL` |
| `bell` | tmux window bell flag is set |
| `pattern:REGEX` | New output line matches a custom regular expression |

`start-and-watch` defaults to `exit,error`. `watch-pane` defaults to `exit,user_input,error`.

The `WatchResult` returned by both tools contains:

```json
{
  "paneId": "%3",
  "event": "pattern:Serving HTTP",
  "detail": "Ready — matched: Serving HTTP on 0.0.0.0 port 8765",
  "elapsed": 1.24,
  "output": "Serving HTTP on 0.0.0.0 port 8765 ...",
  "paneState": {
    "panePid": 12345,
    "foregroundPid": 12346,
    "foregroundCmd": "python3",
    "isAlive": true,
    "waitingForInput": false
  }
}
```

## Native process detection

`pane-state` and the `user_input` trigger detect whether a process is blocked waiting for terminal input at the OS level, without parsing output.

**Linux** reads `/proc/<pid>/wchan` for `n_tty_read` or `wait_woken`, and `/proc/<pid>/syscall` for `SYS_READ` (`0`) with `fd=0`. Uses `github.com/prometheus/procfs` for process enumeration.

**macOS** calls `sysctl kern.proc.pid` to get `kinfo_proc`, then checks `Wmesg == "ttyin"` (the kernel wait channel set when a process is blocked in `n_tty_read`). Falls back to a structural heuristic: if the terminal foreground process group matches the shell's own process group and the shell is in interruptible sleep, it is waiting at a prompt.

Both platforms identify the foreground process by scanning the terminal foreground process group (`TPGID`), not just the pane's shell PID.

## Agent scenarios

### Dev server: start-and-watch → execute-command

```
create-session
  → sessionId: "$3", windowId: "@2", paneId: "%5"

split-pane paneId="%5" direction="horizontal"
  → paneId: "%6"

start-and-watch paneId="%5" command="npm run dev"
  pattern="ready in|listening on|Local:"
  mode="quick" timeout=60
  → event: "pattern:ready in", elapsed: 3.1

execute-command paneId="%6" command="curl -s http://localhost:5173/api/health"
  → output: '{"status":"ok"}', exitCode: 0
```

### REPL session: run-in-repl

```
send-keys paneId="%5" keys="python3 -q" enter=true

run-in-repl paneId="%5" input="import math"
  promptPattern="^>>> " timeout=10
  → output: ""

run-in-repl paneId="%5" input="math.sqrt(144)"
  promptPattern="^>>> " timeout=10
  → output: "12.0"
```

### Build monitoring: execute-command with exit code

```
execute-command paneId="%5" command="go build ./..."
  → output: "", exitCode: 0

execute-command paneId="%5" command="go test ./..."
  → output: "FAIL\tgithub.com/example/pkg\t0.142s", exitCode: 1
```

`execute-command` blocks until the command completes. It tees stdout+stderr to a temp file and signals completion via `tmux wait-for`, so the exit code is always accurate regardless of pipelines.

### Coaching display: split-pane → write-to-display

```
split-pane paneId="%5" direction="horizontal" size=30
  → paneId: "%6"  (narrow display pane on the right)

write-to-display paneId="%6" text="Running migrations..."

execute-command paneId="%5" command="./migrate up"
  → exitCode: 0

write-to-display paneId="%6" text="Migrations complete." clear=true
```

`write-to-display` writes literal text to the pane without capturing it back into the model's context window. The user sees it in their terminal; the tool returns only `{"paneId": "%6"}`.

## Configuration

```
tmux-mcp [--shell-type=SHELL]
```

| Flag | Values | Default | Description |
|---|---|---|---|
| `--shell-type` | `bash`, `zsh`, `fish` | `bash` | Shell used in the pane where `execute-command` runs. Controls how the exit code is captured from a pipeline (`PIPESTATUS[0]`, `pipestatus[1]`, `$pipestatus[1]`). |

Set `--shell-type` to match the shell running in your tmux panes. A mismatch causes `execute-command` to report exit code 0 for all commands.

## Development

**Prerequisites:** Go 1.21+, tmux 3.2+

```bash
# Build
go build -o tmux-mcp .

# Unit tests (no tmux required)
go test -short ./...

# Integration and scenario tests (tmux must be running)
go test ./...
```

**Project structure:**

```
main.go            — MCP server setup, Layer 1 tool registration, resource registration
tmux.go            — tmuxClient: all tmux CLI wrappers, data types
agent_tools.go     — Layer 2 tool registration (start-and-watch, watch-pane, pane-state, run-in-repl, write-to-display, display-message)
triggers.go        — Trigger definitions, notification modes, monitorPane loop, WatchResult
process.go         — GetPaneState: tmux query + OS dispatch
process_darwin.go  — macOS implementation via sysctl/kinfo_proc
process_linux.go   — Linux implementation via /proc filesystem
process_other.go   — Stub for unsupported platforms
e2e_test.go        — MCP client harness for integration tests
scenarios_test.go  — End-to-end scenario tests
```

**Resources exposed:**

- `tmux://sessions` — static resource listing all sessions as JSON
- `tmux://pane/{paneId}` — template resource returning pane terminal content

## Troubleshooting

### execute-command always returns exit code 0

**Cause:** `--shell-type` does not match the shell in the pane.

**Fix:** Pass the correct shell:
```bash
tmux-mcp --shell-type=zsh   # for zsh panes
tmux-mcp --shell-type=fish  # for fish panes
```

### start-and-watch times out immediately

**Cause:** The pane ID does not exist, or the pattern never appears in output.

**Fix:** Verify the pane ID with `list-panes`. Test the pattern against actual command output with `capture-pane` before using `start-and-watch`.

### pane-state reports waitingForInput=false when a prompt is visible

**Cause:** On Linux, some shells use `select()` or `poll()` instead of blocking `read()`, so `wchan` may show `ep_poll` rather than `n_tty_read`.

**Behavior:** The tool falls back to returning `isAlive=true` with `waitingForInput=false`. Use the `idle:N` trigger as a complement when precise input detection is required.

### No sessions returned from list-sessions

**Cause:** No tmux server is running, or the server has no sessions.

**Behavior:** `list-sessions` returns an empty array rather than an error when tmux reports "no server running" or "no sessions". Start a session with `create-session` to proceed.

### MCP client does not see the tools

**Cause:** Binary path in the MCP config is wrong, or the binary is not executable.

**Fix:**
```bash
chmod +x /path/to/tmux-mcp
/path/to/tmux-mcp --help   # verify it runs
```

## License

MIT
