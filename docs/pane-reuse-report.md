# Pane Reuse in `split-pane` — Report for Terminal Skill Developers

## TL;DR

`split-pane` now **reuses an existing idle pane** in the same window instead of
always creating a new split. When it reuses a pane, the response includes
`"reused": true`. This prevents windows from accumulating dozens of stale,
empty panes when an agent repeatedly splits to "get a fresh pane to work in."

If you orchestrate terminals via this MCP server, **nothing in your tool calls
needs to change** — but your mental model should: `split-pane` is now
*idempotent-ish*. Calling it twice from the same source pane may return the same
target pane the second time.

---

## What changed

### Behavior

Before: every `split-pane` call created a brand-new pane.

Now: `split-pane` first scans the **sibling panes in the same window** for one
that is genuinely idle. If it finds one, it returns that pane (with
`"reused": true`) and does **not** create a new split. Otherwise it splits as
before.

A pane counts as "idle" only when **all** of these hold:

1. The pane is **alive** (`isAlive`).
2. The OS reports it as **waiting for input** (`waitingForInput`).
3. Its foreground process is a **shell** (`zsh`, `bash`, `fish`, `sh`, `dash`,
   `ksh`, `csh`, `tcsh`, including login `-zsh`/`-bash` variants).

The **source pane** you split from is always excluded — splitting pane A never
"reuses" pane A.

### Why the shell check matters

`waitingForInput` alone is **not reliable** for "is this pane free?". A pane
running `yes > /dev/null` (busy-looping) or `cat` (blocked on a read) can report
`waitingForInput=true` even though it is occupied. The shell-name check is the
real safety gate: a pane is only reusable if a *shell* is in the foreground, i.e.
it is genuinely sitting at a prompt. Without this, the reuse logic would hijack
panes that are running a user's process.

### Response shape

```jsonc
// New split (no idle sibling available):
{ "paneId": "%5", "windowId": "@1" }

// Reused an existing idle sibling:
{ "paneId": "%3", "windowId": "@1", "reused": true }
```

`reused` is omitted (not `false`) when a new pane is created, so existing code
that ignores the field is unaffected.

---

## How to use `split-pane` now

### Get a working pane (most common)

```jsonc
{ "tool": "split-pane", "params": { "paneId": "%0", "direction": "horizontal" } }
```

You get a pane to work in. If you already split off an idle pane earlier and
left it at a shell prompt, you get that one back (`reused: true`) instead of
piling up a new one. This is the intended win for agent loops.

### Force a genuinely new pane

If you specifically need a **distinct** pane (e.g. side-by-side panels that must
coexist), make sure the candidate panes are **not idle** before you split, or
split from a pane whose only sibling is busy. The simplest pattern: start your
long-running process in the pane *before* splitting again, so it is no longer a
shell at a prompt and won't be reused.

```jsonc
// Pane A → split → pane B
{ "tool": "split-pane", "params": { "paneId": "%0", "direction": "horizontal" } }
// Start work in B so it's "busy" (its foreground process is no longer a shell)
{ "tool": "send-keys", "params": { "paneId": "%1", "keys": "npm run dev", "enter": true } }
// Split again → guaranteed NEW pane C, because B is busy and A is the source
{ "tool": "split-pane", "params": { "paneId": "%0", "direction": "horizontal" } }
```

### Check whether you got a reused pane

Read `reused` in the response. If `true`, the pane may already contain prior
scrollback — clear it (`send-keys` `clear`, Enter) if you need a clean slate.

---

## Gotchas worth knowing

### 1. `start-and-watch` baseline is captured *after* the command is sent

`start-and-watch` sends the command, then snapshots the pane as its diff
baseline. An **instantaneous** command (e.g. `echo done`) can finish *before*
the baseline snapshot, so its output lands in the baseline and never registers
as "new" — the pattern never matches and you get a `timeout`.

**Fix when watching a fast command:** make the output arrive *during*
monitoring, e.g. `sleep 0.3 && echo done`, or watch a longer-lived process. Our
test suite uses exactly this pattern.

### 2. A live-clock shell prompt breaks output diffing

If the shell prompt repaints every second (e.g. powerlevel10k with a
right-aligned clock), `watch-pane`/`start-and-watch` see "new output" on every
poll. This makes **idle** triggers never fire and can confuse pattern matching.
For deterministic monitoring, prefer a static prompt in panes you watch. (This
is an environment property of the shell, not the server.)

### 3. Reuse only looks within the same window

Idle panes in *other* windows are never reused. Reuse is scoped to siblings of
the source pane's window.

---

## Test coverage

New scenario tests in `scenarios_test.go`:

- `TestScenario_SplitPaneReusesIdlePane` — idle sibling is reused (`reused:true`).
- `TestScenario_SplitPaneCreatesNewWhenAllBusy` — busy sibling (`yes`) is skipped;
  a new pane is created.
- `TestScenario_SplitPaneReusesCorrectPaneAmongMultiple` — among mixed
  busy/idle siblings, the idle one is selected.

Test reliability improvements:

- Replaced fixed `sleep(300ms)` setup waits with `waitForPaneIdle` /
  `waitForPaneBusy` helpers that **poll `pane-state`** until the pane reaches
  the expected condition. Fixed sleeps raced against shell-prompt redraw under
  load; polling is deterministic and fatals on a 5s timeout.

---

## Implementation pointers

- `tmux.go` — `findIdlePaneInWindow`, `getWindowIDForPane`, `isShellProcess`,
  and the `Reused` field on `CreatedPane`.
- `main.go` — `registerSplitPane` checks `findIdlePaneInWindow` before splitting.
- `scenarios_test.go` — reuse tests and the polling helpers.
