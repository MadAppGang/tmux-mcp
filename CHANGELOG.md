# Changelog

All notable changes to tmux-mcp are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2.0.0] — 2026-09-04

**Breaking, on every axis. There is no compatibility path — see [Migration](#migration).**

The server used to hand out tmux pane identifiers and expect them back. An agent that
held one could address any pane on the machine, including the user's and its own, and an
agent that had lost one went looking for `$TMUX_PANE` and started driving raw tmux. Both
of those are now impossible: a **slot** — a number the caller picks, 1, 2, 3 — is the only
handle the server has. Twenty tools become thirteen, no schema declares an identifier, no
response or notification returns one, and a request that sends one is refused rather than
quietly redirected to slot 1.

### Changed

- **The slot is the only way to name a pane.** Every tool that touches a pane takes
  `slot`, and nothing else. Omitting it means slot 1. The same slot number returns the
  same pane on every call, so a process started there is still there next time.
- **A request carrying a pane, window or session id is REFUSED**, with
  `paneId is not accepted; address the pane by slot` — and the same sentence for the window
  and session forms. This is a safety rule, not strictness:
  an MCP server that drops an unknown property resolves the call to slot 1 and *succeeds*,
  so a caller that sent an identifier would get keystrokes in a pane it did not name. The
  retired `headless` argument is refused the same way, and for the same reason — a caller
  asking for an invisible pane must not silently be given a visible one beside the user.
- **The three tools that resolve no slot refuse a pane argument too.** `close-pane` closes
  whichever kind of pane the slot holds, so a stated `isolated` is refused rather than
  ignored — ignoring it would kill the visible pane for a caller who said it meant the
  invisible one, and report success. `list-slots` and `notify` address no pane at all, and
  refuse `slot` and `isolated` for the same reason.
- **`open-pane`** — renamed from `split-pane`. It no longer splits a pane you name; it
  returns the pane for a slot, opening it if the slot is empty.
- **`notify`** — renamed from `display-message`.
- **`close-pane` returns a bare array**, dropping the `{"closed": [...]}` wrapper, so both
  registry tools have one shape: `[{slot, action, detail}]`.
- **Reading no longer creates.** `capture-pane`, `screenshot-pane`, `pane-state` and
  `watch-pane` now error on a slot that was never opened, instead of splitting the user's
  window (or adopting one of their idle shells) to answer a question about a pane that did
  not exist. They declare no `isolated` argument and carry no `created` field.
- **`created` is present on every creating call**, as `true` or `false`, and absent from
  every reading call. The v1.7.x "absent on reuse, test for `=== true`" rule is gone.
  `created: true` on a slot you already had is the signal that the pane was closed and the
  process you started in it died.
- **`exited` (`run-in-repl`) and `timedOut` (`execute-command`) are always present.** An
  absent key made `result.exited === false` unsatisfiable and gave a model reading the
  response no way to learn the field exists.
- **`pane-state` answers about a dead pane** rather than refusing: `isAlive: false` with an
  exit code is the fact the caller is asking for.
- **Channel notifications carry `meta.slot`**, not a pane identifier, and their text reads
  `slot 2: exit — …`. The image caption from `screenshot-pane` and the progress
  notifications from the watching tools name the slot too.
- **The "not inside tmux" error names the one route that still exists**:
  `no window to place a pane in: this server is not running inside tmux — use isolated: true`.
  The previous text named three routes, all three of which this release deletes.
- **The Go module path is now `github.com/MadAppGang/tmux-mcp/v2`.** Go requires the
  `/vN` suffix at major version 2 and above; without it `go install …@v2.0.0` fails
  outright. The binary is still `tmux-mcp`.
- **Internal: a `Backend` port sits between policy and tmux.** `tmux.go` became
  `backend_tmux.go`, and it is now the only file that runs a tmux command or knows what a
  pane identifier looks like. A new `--backend` flag selects the implementation; `tmux` is
  the only one today.

### Added

- **`list-slots`** — what this agent has open and what is running in each:
  `[{slot, isolated, origin, foregroundCmd, isAlive}]`. `origin` distinguishes a pane the
  server created from one it adopted from an idle shell of the user's. Other agents' panes
  and the user's own panes are not listed.
- **`isolated: true`** on the six tools that can open a pane — `open-pane`, `send-keys`,
  `execute-command`, `run-in-repl`, `start-and-watch`, `write-to-display`. The slot is
  placed on a private tmux server with no window and no client attached, so nothing appears
  beside the user, and every later call reaches it by the same slot number. It requires a
  slot, because a pane nobody can see cannot be found again any other way. Omitting the
  argument means "whichever kind this slot already is" — never "visible".
- **An ephemeral form of `execute-command`**: `isolated: true` with no slot opens a pane,
  runs the command, destroys the pane inside the call, and returns `{output, exitCode,
  timedOut}` with no slot, because there is no pane left to address.
- **Orphaned isolated namespaces are reaped at startup** by process liveness. A crashed
  server used to leave live shells nobody could see, list or reach until reboot.

### Removed

**Eight tools**, deleted outright with no replacement argument:

- `list-sessions`
- `list-windows`
- `list-panes`
- `create-session`
- `create-headless`
- `kill-session`
- `kill-headless-server`
- `kill-pane`

**Both non-default tool scopes, and everything that selected them:**

- the `primitives` scope
- the `all` scope
- the `--scope` command-line flag — deleted, not kept as a no-op and not kept accepting
  `agentic` alone; a flag that accepts one value teaches a reader that others once existed
- the `TMUX_MCP_SCOPE` environment variable

The binary now registers exactly one surface. Four more tools existed only inside the
deleted scopes and go with them: `create-window`, `resize-pane`, `rename-session` and
`kill-window`.

**Both MCP resources**, along with the resource capability that advertised them:

- `tmux://sessions`
- the per-pane content resource template, whose URI was an identifier in the surface

**Arguments**, from every tool that had them:

- the pane, window and session id arguments. Sending one is now an error:
  `paneId is not accepted; address the pane by slot`
- `headless` — replaced by `isolated`, and likewise refused rather than ignored:
  `headless is not accepted; open an invisible pane with isolated: true and a slot number`
- `direction` and `size` on the tool now called `open-pane`, which no longer splits a pane
  the caller names

**Response fields:** every identifier field on every response type, and the
`{"closed": [...]}` wrapper on `close-pane`.

### Migration

**There is no compatibility path, and none is coming.** Nothing accepts the old arguments,
and no flag restores the old surface.

**If you hold a pane identifier: delete it.** There is nothing to translate it into. Use
the slot number the server gave you — the same number reaches the same pane on every call,
which is the whole reason the identifier is gone. If you do not have a slot number, you did
not open the pane, and this server will not address it.

| Was | Now |
|---|---|
| a call that named the pane by id | `{"slot": 2, "command": "…"}` — or omit `slot` for slot 1 |
| `split-pane` | `open-pane` |
| `display-message` | `notify` |
| `create-headless`, then `headless` on later calls | `isolated: true` on the first call for that slot; the slot alone after |
| `close-pane` → `{"closed": [...]}` | `close-pane` → `[...]` |
| `capture-pane` on an unopened slot returned a fresh pane | it is an error; open the slot first, or use `start-and-watch` to open and watch in one call |
| `created` absent on reuse | `created` is always present on a creating call, `true` or `false` |
| `list-panes` / `list-sessions` to see what you have | `list-slots` |

**If you used the `primitives` or `all` scope: use the `tmux` CLI directly.** That surface
was an experiment, and the thing it was for — an agent driving tmux itself — is already
served by `tmux` through a shell tool, with no MCP round trip and the full command set
rather than seventeen of them. The repo ships a project skill at
`.claude/skills/tmux-control/` that covers doing this well: capturing ids at creation,
marking your own panes, isolated servers that survive tmux-resurrect, and exit-code
capture.

**If you install via the terminal plugin or Homebrew**, nothing is required of you beyond
the usual upgrade. Note that a major tag deliberately does not auto-publish the terminal
plugin: the plugin's matching major ships separately, so the skills that describe these
tools land at the same time as the tools.

**If you depend on the Go module**, the import path is now
`github.com/MadAppGang/tmux-mcp/v2`.

---

## [1.7.1] and earlier

See the [GitHub releases](https://github.com/MadAppGang/tmux-mcp/releases) for the history
before this file existed.

[2.0.0]: https://github.com/MadAppGang/tmux-mcp/releases/tag/v2.0.0
[1.7.1]: https://github.com/MadAppGang/tmux-mcp/releases/tag/v1.7.1
