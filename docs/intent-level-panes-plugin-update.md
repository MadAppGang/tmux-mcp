# tmux-mcp: intent-level pane management — plugin update

**v1.6.3 → v1.7.0** · additive, no breaking changes
**Consumer:** `plugins/terminal` in magus-src — the `terminal-interaction` skill

---

## What it does

The server now reads its own `$TMUX_PANE` and keeps a **default helper pane per window**, recorded
in tmux pane options so the record dies with the pane it describes. The common case — *"run this
beside me"* — is one call with **no pane argument**:

```json
{"tool": "send-keys", "params": {"keys": "npm run dev", "enter": true}}
```

```json
{"paneId": "%81", "slot": 1, "created": true}
```

Before this change the same thing took four steps, two of them raw `tmux` used as an adapter for
tmux-mcp:

```
Bash: echo "$TMUX_PANE"                                   → %74
Bash: tmux display-message -t %74 -p '#{window_id}'
split-pane {paneId: "%74", direction: "horizontal"}
send-keys  {paneId: "%81", keys: "npm run dev", enter: true}
```

The point is not convenience. **An agent cannot target the user's pane if it never names a pane** —
enforced at six points in the code plus an AST test. That converts a safety rule the agent had to
remember into one it cannot break.

---

## Tool surface

**New — `close-pane`.** Owner-aware teardown: it *kills* panes the server created, and *interrupts
and releases* panes it adopted from the user, never killing them. `slot:"all"` lets go of
everything in the window.

**Ten tools gain optional `paneId` + `slot`:**
`send-keys`, `execute-command`, `capture-pane`, `pane-state`, `run-in-repl`, `watch-pane`,
`start-and-watch`, `screenshot-pane`, `split-pane`, `write-to-display`

Resolution order is the same everywhere: explicit `paneId` wins verbatim → then `slot` → then, with
neither, **slot 1**.

**`slot` is a plain integer, 1–64.** There is no `"new"` and no allocate-me-one form: a caller that
wants several panes asks for 1, 2, 3, and picking the numbers itself is what lets it address the
same pane again on the next call. Only `close-pane` takes a string, for `"all"`.

**`kill-pane` is unchanged** — still blunt, `paneId` still required. There must exist no
argument-less call that destroys something. `close-pane` is the resolvable, owner-aware sibling.

**Agentic scope: 19 → 20 tools.**

---

## Behaviour fixes worth knowing

- **`start-and-watch` with no arguments** used to create a detached *session* the user could not
  see. It now uses slot 1, in the window the user is looking at.
- **`write-to-display` with `clear:true`** used to send `C-u` (kill line). On a pane adopted from
  the user that destroys a half-typed command. It now sends `C-l` there instead: the line editor
  repaints, the screen clears, the buffer survives, nothing is submitted. Cost: on an adopted pane
  successive writes append rather than replace.
- **`capture-pane` / `screenshot-pane` / `pane-state` no longer advertise `readOnlyHint`.** They can
  create panes on the no-`paneId` path now, and MCP clients may use that hint to skip confirmation
  or prefetch — an auto-approving client would otherwise rearrange the user's window silently.

---

## Compatibility

Explicit-`paneId` responses are **byte-identical** to v1.6.3, pinned by test
(`TestExplicitPaneIdResponsesAreUnchanged`). One additive exception: `pane-state` gains a `paneId`
key it never had. Nothing else changes shape, and `paneId` keeps working on every tool.

---

## What the plugin needs to do

1. **Bump the binary pin** — one line, `plugins/terminal/plugin.json:48`:
   `github.com/MadAppGang/tmux-mcp@v1.6.3` → `@v1.7.0`

2. **Delete the sections this makes obsolete** in `terminal-interaction/SKILL.md`
   (~290 lines become ~95, with zero raw `tmux`):

   | Section | Line | Why it goes |
   |---|---|---|
   | §1b Current Pane Detection, incl. the reliability table | 37–59 | the server reads `$TMUX_PANE` itself |
   | Helper Pane Reuse + the `claude-helper` label convention | 61–74 | replaced by slots; the server sets pane titles |
   | Split Ordering Strategy | 121–152 | the ASCII progression is now code |
   | Layout Presets and Pane Labels | 153–183 | five raw `tmux` calls; titles are server-set |
   | Example F, steps 1, 2 and 4 | 543–582 | the three raw-`tmux` calls on the critical path |

   **Revise rather than delete** the occupancy note at line 99. It says `send-keys` and `kill-pane`
   have no occupancy guard so a manual `pane-state` check is mandatory. That is now half true: a
   call naming *no* pane resolves to a helper and is safe; one passing an explicit `paneId` is still
   unguarded. That distinction is the strongest remaining reason to prefer the no-pane form.

3. **Fix two defects independent of this change:**
   - **§3 is false.** It claims `start-and-watch` and `watch-pane` return
     `-32601: requires task augmentation` on Claude Code. Both were tested working — they are
     synchronous blocking calls, and the async/task-id model no longer matches the schemas. This
     actively steers agents away from the two tools that replace polling loops.
   - **§4's tool table is wrong three ways.** Titled "20 Tools", lists 22, and the shipped agentic
     scope exposed 19 — documenting four tools unreachable at that scope while omitting
     `screenshot-pane`. It is now genuinely 20, so the title is accidentally right and the body is
     still wrong.

4. **Regenerate that table from `tools/list` in CI**, so it cannot drift a third time.

---

## What the skill should now teach

The default flow is one line, and the interesting part is the response:

```json
{"paneId": "%81", "slot": 1, "created": true}
```

**`created` is the field that matters.** It means the pane is new *to that slot*. If a caller sent a
dev server to the helper pane and a later call comes back `created: true`, the user closed that pane
and the process died with it. Without checking it, an agent keeps reporting a dev server that
stopped ten minutes ago. It is also `true` when a pane was *adopted* rather than freshly created —
so "new to this slot", not "I made a pane".

Properties worth stating explicitly:

- **A slot is never the agent's own pane.** By construction, not convention.
- **Repeated calls for the same slot return the same pane**, so a process started there is still
  there next time.
- **Explicit `paneId` bypasses slot resolution entirely** and answers exactly as it always did.
- **Slots are visible panes only.** `slot` with `headless:true` is an error, not a silent
  preference — a headless pane lives on a separate tmux socket with no relation to the window.
- **Not running under tmux is a clear error** naming `headless:true` and `create-headless`. No pane
  is invented.
- **`close-pane` refuses any `paneId` it does not recognise as its own.** `kill-pane` remains the
  deliberate escape hatch.

---

## Hazards to document, not hide

**Adoption can inherit unsubmitted input.** When no helper pane exists, resolution may *adopt* an
idle pane the user left open, guarded by a same-uid check and a "shell is the foreground process"
check. Neither can see the tty input buffer. If the user typed `rm -rf /data/` and never pressed
Enter, that pane looks perfectly idle, and sending `ls\n` executes the *concatenation*. This is not
detectable from outside the process — tmux exposes cursor position but not where the prompt ends, so
pending input cannot be told apart from a wide prompt. It is a known accepted trade-off with **no
opt-out**, recorded in `canAcquire`. Adopted panes are marked `acquired` and are never killed by
`close-pane`, only interrupted and released.

**The uid guard proves *who*, not *what*.** A shell inside a user namespace, a virtualenv, a
container exec session, or one with `AWS_PROFILE=production` exported passes every check — right
user, at a prompt, doing nothing. Commands then run in that context.

**No slot value avoids adoption.** Every numbered slot goes through the same resolution, and
adoption sits ahead of creation in it, so a fresh slot number is exactly as likely to adopt as slot 1
is. The only ways to guarantee a pane with no inherited context are an explicit `paneId` you already
trust, or `headless:true`. Worth saying out loud — an agent needing a clean environment will
otherwise pick a high slot number and believe it got one.

---

## Verification

- **112 top-level tests and 69 subtests green**, `-race` clean, `go build` / `go vet` / `gofmt` clean.
- **10/10 acceptance checks against real tmux 3.7b**, driving the shipped binary over MCP stdio in a
  live pane — run twice, on an isolated socket, with the user's own sessions verified untouched.
- **Negative control:** the same checks re-run against the pre-fix binary reproduced both
  high-severity bugs, confirming the harness can actually detect what it claims to.
- Reviewed by a three-model adversarial panel, which found two HIGH defects that a full green suite
  had not: `close-pane({paneId})` could kill the agent's own session in the nested-subagent case, and
  teardown mutated tmux state outside the slot lock. Both fixed and covered by tests.
- The **test suite itself** was then reviewed for whether it tests the specification or the
  implementation. That round replaced two structural tests that would have broken under a correct
  rewrite, and added coverage for four things the design requires that nothing checked — most
  importantly that the helper registry lives in tmux pane options rather than in process memory
  (§2.4), which a plain in-process map would previously have satisfied. Each new test was verified
  to actually fail against a deliberately wrong implementation before being kept.

~4,000 lines changed across 19 files.

---

## Closed since the design doc

**§13.1 — is the MCP server shared across subagents?** Measured: one server per session (38 live
servers, 38 distinct parent PIDs; spawning a subagent adds none), and in this harness subagents have
no tmux MCP tools at all. `slot:"new"` was removed on the strength of that, and **nothing in the
skill needs to describe slot contention.**

**§13.2 — session-end teardown.** There is no session-end signal in MCP, so helper panes outlive the
agent session until something calls `close-pane`. Leaving them is the safer default;
`close-pane({slot:"all"})` is the mitigation, and the skill should offer it as an end-of-task
courtesy rather than a requirement.

**§13.3 — pane titles.** Confirmed and shipped: `agent` for slot 1, `agent:2` and up for the rest.
They show in `list-panes` and in the pane border where the user has `pane-border-status` enabled.
