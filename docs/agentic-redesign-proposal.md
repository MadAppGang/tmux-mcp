# Proposal: Agentic-first tmux-mcp surface

**Status:** Layer 2 (the `tmux-control` skill) shipped in v1.6.0. Layer 1 (the
handle-based ~8-tool MCP surface) remains a draft — this is the v2.0.0 roadmap.
· **Date:** 2026-06-05, updated 2026-07-13

## Problem

The MCP server exposes **23 tools**, ~half of which are 1:1 wrappers around raw
`tmux` verbs (`create-session`, `split-pane`, `resize-pane`, `kill-window`,
`list-windows`, …). This leaks tmux's topology model onto the agent: to do
anything the agent must reason about sessions, windows, pane IDs (`%5`), indices
and split direction — bookkeeping it doesn't actually care about.

Symptoms:
- **Token tax / wrong-tool churn.** 23 schemas in context; agents pick the wrong
  one or chain three calls (`create-session` → `split-pane` → `send-keys`) to do
  one thing.
- **Pane littering.** Agents call `split-pane` repeatedly and leave dead panes.
  v1.5.0's *idle-pane reuse* is a band-aid for this; the real fix is to stop the
  agent naming panes at all.
- **Topology ≠ intent.** An agent thinks "run this and tell me when it's ready,"
  not "give me window 3, pane %5, split vertically at 30%."

## Direction

Split the surface into two layers by audience:

1. **Agentic MCP tools** — a small set of intent verbs. The server owns all tmux
   topology; the agent references opaque **handles**, never pane IDs.
2. **A `tmux-control` skill** — a markdown command table for the rare case an
   agent (or a human) genuinely needs explicit window/pane/session control. It
   teaches running raw `tmux` via Bash. No MCP tools for this.

Core idea: **addressing → handles.** `run(...)` returns `handle: "h_3"`; every
follow-up references `h_3`. "Input pane optional", "default to split-pane", and
"headless isolated mode" all fall out naturally instead of being bolted on.

---

## Proposed MCP tool surface (target: ~8 tools, down from 23)

| Tool | Purpose | Key params | Returns |
|------|---------|-----------|---------|
| `run` | Start a command in an auto-managed pane and (optionally) watch it. **Default entry point.** | `cmd`, `watch_for?` (regex), `isolated?`, `timeout?` | `{ handle, ready, output, exitCode? }` |
| `follow` | Keep watching an already-started handle until a trigger fires. | `handle`, `until?` (`pattern\|exit\|idle:N`), `timeout?` | `{ event, output }` |
| `input` | Send keystrokes/text to a handle (REPL answers, prompts, Ctrl-C). | `handle`, `text`, `enter?`, `control?` | `{ output? }` |
| `read` | Read current pane content (text) or a visual PNG. | `handle`, `as?` (`text\|image`) | text or image |
| `status` | Native process state for a handle: alive, foreground cmd, idle/busy. | `handle` | `{ alive, foregroundCmd, idle }` |
| `set_active` | Designate one handle as the default target so later calls can omit `handle`. | `handle` | `{ active }` |
| `release` | Terminate a handle's pane/session and free it. | `handle?` (default: active) | `{ released }` |
| `notify` | Side-channel display to the human (status bar / coaching pane). | `text`, `where?` | `{ shown }` |

Notes:
- **`handle` is optional everywhere** once `set_active` has been called — matches
  "make input pane optional / set active once if needed."
- **Default placement = split pane** in the user's current window, reusing an
  idle pane (the v1.5.0 logic, now internal and never surfaced).
- **`isolated: true`** routes to the headless server (today's `create-headless`),
  invisible to the user's `tmux ls`. This is the explicit isolated mode.
- **`release`** is the "terminate when we don't need it" verb. A handle may also
  auto-release on process exit (configurable).

### Mapping old → new (nothing capability is lost)

| Old tools | New |
|-----------|-----|
| `start-and-watch`, `execute-command` | `run` (watch_for omitted = run-to-exit) |
| `watch-pane` | `follow` |
| `send-keys`, `run-in-repl` | `input` |
| `capture-pane`, `screenshot-pane` | `read` (`as: text\|image`) |
| `pane-state` | `status` |
| `display-message`, `write-to-display` | `notify` |
| `create-headless` / `kill-headless-server` | `run({isolated:true})` / `release` |
| `create-session`, `create-window`, `split-pane`, `resize-pane`, `rename-session`, `kill-session`, `kill-window`, `kill-pane`, `list-sessions`, `list-windows`, `list-panes` | **skill** (raw `tmux` table) — only when explicit topology control is wanted |

---

## The `tmux-control` skill (topology, on demand)

A markdown table the agent can pull in when it truly needs explicit control —
multi-window dashboards, exact layouts, pane synchronization. It runs raw `tmux`
via Bash, so the MCP server carries none of this.

| Goal | tmux command |
|------|--------------|
| List sessions | `tmux ls` |
| New detached session | `tmux new-session -d -s NAME` |
| New window | `tmux new-window -t SESSION -n NAME` |
| Split current pane | `tmux split-window -h` (or `-v`) |
| List panes w/ size | `tmux list-panes -t WINDOW -F '#{pane_id} #{pane_width}x#{pane_height}'` |
| Resize pane | `tmux resize-pane -t PANE -x 80 -y 24` |
| Rename session | `tmux rename-session -t OLD NEW` |
| Kill pane / window / session | `tmux kill-pane -t PANE` / `kill-window` / `kill-session` |
| Sync panes (type to all) | `tmux setw synchronize-panes on` |

(Trigger phrases, examples, and a one-paragraph "when to reach for raw tmux vs.
the `run` verb" go in the skill body.)

---

## Open questions / risks

1. **Backward compatibility.** Dropping 11 tools is a breaking change → **v2.0.0**.
   Option: keep them under `--scope=all` for one release with a deprecation note.
2. **Handle lifecycle.** Where do handles live? In-memory map in the server
   process (lost on restart) vs. derivable from a tmux pane option we set. The
   latter survives restarts and is inspectable — likely better.
3. **`read` returning images.** Keep the v1.4 PNG path; it's just `as: image`.
4. **Auto-release policy.** On process exit, auto-release the handle? Default on
   for `run`-created panes, off for ones the user pre-existing-attached to.
5. **Does the human ever see the work?** Default split-pane = visible. Isolated =
   not. `set_active` on a user-visible pane is the "watch me work" mode.

## Migration sketch

1. Implement the 8 agentic tools as a thin layer over existing internals
   (`SplitPane`+reuse, `monitorPane`, `GetPaneState`, `ExecuteCommand`).
2. Add a handle registry (pane option `@mcp_handle` so it's restart-safe).
3. Author the `tmux-control` skill.
4. Gate old tools behind `--scope=all`, mark deprecated.
5. v2.0.0 once validated on CI (clean Linux shell — the real gate).
