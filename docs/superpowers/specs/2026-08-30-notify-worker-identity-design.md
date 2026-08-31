# SessionEnd backstop: only a session that can name itself may speak for the branch

Issue: #69

## Problem

`adapters/core/dispatch-notify.sh` is a `SessionEnd` backstop. It fires for **any**
session whose `cwd` holds a `WORKER_TASK.md`, and posts `exited` to the crew bus so a
crashed worker does not strand a `working` row on the roster.

Its identity resolution has a fallback:

    me="${CREW_WORKER_ID:-worker:$branch}"

A session that is _not_ the dispatched worker takes that fallback and posts a
**session-less** `worker:$branch` status of `exited` while the real worker is alive and
`working`. The dedupe guard below filters on that same session-less `$me`, finds no
prior status, and so cannot suppress the post.

`roster` then folds `group_by(.from)` per session and `group_by(.branch) | sort_by(.ts)
| last` per branch (`crew.sh:835`, `824-825`), so the newest event wins the displayed
row — and the false `exited` is newest. `reap`'s idle-release folds the same two stages
onto the branch (`crew.sh:2142-2143`). Either way the live session is masked.

Observed consequences: `crew reply` refuses to talk to a live worker
(`crew.sh:417-420`); nothing surfaces it because `crew watch` ignores `exited`; and it
let the #68 reap widening (PR #70, since merged) reclaim a live worker's worktree.

## What is actually established about the trigger

The hook fires for _any_ session in the worktree, so the producer set is open-ended.
Three things are established, and the design rests only on these:

1.  **The producer had no usable `CREW_WORKER_ID`** — unset or empty, the two values that
    share the fallback branch at `dispatch-notify.sh:28`. The raw events quoted in #69
    carry `"from":"worker:feat/68-…"` with **no `#session` suffix**, while the same
    branch's claim and `working` carry one. A dispatched worker's id always contains `#`
    (`dispatch.sh:523`), so whatever that session was, it took the fallback — the code
    path this spec removes.
2.  **The defect reproduces on today's code.** Feeding the hook a `SessionEnd` payload
    with `CREW_WORKER_ID` unset, against a bus where `worker:feat/x#s1-1` last posted
    `working`, appends `{"from":"worker:feat/x","state":"exited"}` and `crew roster`
    then reports `"state":"exited","session":null` for the branch with the live
    `working` session demoted into `sessions[]` — the exact shape #69 reports.

3.  **It fired live, on this branch, during this task.** The bus holds

        {"ts":1788098200278,"crew_id":"1788096669-90897",
         "from":"worker:feat/69-fix-crew-stop-subagent-sessionend-postin",
         "to":"dispatcher:1788096669-90897","kind":"status","body":{"state":"exited"}}

    — no `#session` suffix — sitting **between two of this worker's own `working`
    heartbeats** (ts 1788098055662 and 1788098288257), i.e. posted while the session was
    demonstrably alive and mid-pipeline. `crew roster` reports the branch `working` only
    because the next heartbeat out-timestamped it; a worker that did not heartbeat at
    every seam would read `exited`. This is the defect, in this crew, not a reconstruction.

What is **not** established, and is deliberately not load-bearing:

- **Which** session type produced either event. This was investigated and left
  unresolved on purpose rather than guessed:
  - #69 attributes it to Claude spec/plan critics. That is wrong: Claude Task subagents
    fire `SubagentStop`, not `SessionEnd`, and subagents ran to completion in this
    worktree appending zero bus events.
  - Codex native subagents were the leading candidate — the feat/68 worktree has four
    codex session rollouts and no Claude transcript dir, and
    `adapters/codex/plugin/hooks/hooks.json` registers the same hook. But
    `dispatch.sh:749` pins `agents.max_concurrent_threads_per_session=3` ("threads _per
    session_"), and three direct attempts to make a real `codex exec` session in a
    scratch worktree fire this hook produced **no** event at all. Unconfirmed.
  - `crew.sh:860-863` names a third candidate in its own comment: "a human closing an
    auxiliary pane".

  The live capture above therefore proves the _defect_, not its author. That is
  precisely why the fix keys on the observable rather than on a culprit.

- Whether the _historical_ bus corroborates #69's own capture. It cannot: the bus spans
  ts 1785662230-1785672503 and 1788096820-now, while #69's observation sits at
  ts ≈1788034653000 — in the gap. Absence there is a coverage artifact, not evidence.

The fix is therefore keyed on the **observable** (a session that cannot name itself),
not on identifying the culprit. That is what makes it robust to the open-ended producer
set — including the codex disjunction above resolving either way.

## Decision

**Option 1: a session with no `CREW_WORKER_ID` posts nothing, and pings nothing.**
"No" means unset **or empty**. Empty must take the silent path too: `{"from":""}` is
silently dropped by every reader (`crew.sh:834`, `2080`) while still sitting in the log
defeating the next run's dedupe guard — and **no writer anywhere validates `from`**
(`crew status` takes it verbatim at `crew.sh:355`; its `case` at `crew.sh:335-341`
validates the _state_ only, and the recipient guard at `crew.sh:372-377` is on the `msg`
branch's `to`). Nothing downstream will catch an empty `from`, so the hook must refuse
to write one.

`dispatch` puts `CREW_WORKER_ID` into the worker window's environment
(`dispatch.sh:694`), and a hook process inherits its parent's environment, so the
dispatched worker session always has it. A session that does not cannot know which
worker it belongs to, and guessing `worker:$branch` is what manufactures a terminal
state for a live session.

The guard is an early exit placed with the other preconditions, **before** the tmux
ping, so a non-worker session is silent on both channels. The ping's only current gate
is `dispatcher_pane` read from `WORKER_TASK.md` — which every session in the worktree
can read — so a false "worker exited" toast reaches the dispatcher's pane on exactly the
same wrong trigger. Same hook, same misidentified session, same lie.

Moving a guard in front of the ping can also delete the ping for real workers, which is
the hook's original purpose (`dispatch-notify.sh:2`, `12-14`). Acceptance pins that it
survives.

### This guard is necessary, not sufficient

It eliminates the **session-less** class only. It does **not** cover:

- **Any process sharing the worker's environment.** `tmux new-window -e` sets the
  _window_ environment, inherited by the pane's shell and by everything launched in that
  window thereafter. An in-process subagent, a forked helper, or a human's later
  `claude`/`codex` in that same pane all carry `CREW_WORKER_ID` and would post `exited`
  under the _sessioned_ id — indistinguishable from a real crash.
- If that residual class is non-empty, its roster shape is **worse** than today's: the
  false event shares the live worker's `.from`, so `group_by(.from)` (`crew.sh:835`)
  merges them and `max_by(.ts)` flips the worker's own row to `exited` with no `working`
  remnant left in `sessions[]` to notice. Recovery then depends entirely on the
  `exit_suspect` live-pane rewrite (`crew.sh:860-893`), whose nix-wrapped-engine matcher
  #71/PR #72 has since fixed (`_pane_is_engine_at`, `crew.sh:879`) — so that remedy is
  now live rather than inert. But recovery is roster-only: `exit_suspect` is set only in
  the roster rewrite; `crew reply` resolves via `_sessions` (`crew.sh:114-141`), which
  reads the raw latest status event with no live-pane rewrite, so a sessioned false
  `exited` still makes branch-only reply refuse. That partial coverage is what makes
  leaving the residual class to it defensible.
- **Cross-worktree.** A shell carrying `CREW_WORKER_ID=worker:A#s…` whose `SessionEnd`
  fires with `cwd` inside worktree B posts `exited` for A against B's `crew_id`. A cheap
  narrowing exists — compare the `wid_branch` of `$CREW_WORKER_ID` against `$branch`
  from `dispatch-notify.sh:11` — and is deliberately **not** adopted here: it is
  speculative robustness against a case nobody has observed, and it would add a second
  silent-exit path to reason about. Recorded so a later reader knows it was considered.
- That class is not addressable in the hook at all: `crew.sh:860-866` records why an
  in-hook liveness probe is impossible ("the worker's real SessionEnd blocks on its own
  hooks, so probing liveness there deadlocks").

Stated plainly so no reader mistakes this for a complete filter.

### Why not the alternatives

- **Option 2 (branch-scoped guard + add `working` to the don't-override set).** Adding
  `working` destroys the feature: a worker that crashes mid-run has `working` as its
  last state, which is exactly when the backstop must fire. It trades a false `exited`
  for a never-`exited` — the failure mode #69 explicitly forbids. Widening the guard to
  the branch without that addition fixes nothing either, since the live session's newest
  state _is_ `working`. And it keeps writing session-less ids, so the roster still shows
  `session: null` rows belonging to nobody.
- **Option 3 (exclude subagent sessions at the top of the hook).** Unimplementable
  engine-neutrally from one shared script: Claude subagents never reach this hook, codex
  exposes no documented subagent marker on `SessionEnd`, and it would still miss the
  human auxiliary pane. It targets one member of an open-ended set; option 1 targets the
  invariant.

### Not the wrong problem

The branch-keyed fold amplifies this but is not the defect. After option 1 the hook
stops writing a session-less `worker:<branch>` `from`, and every protocol caller already
passes `$CREW_WORKER_ID`; `crew reply`'s branch-only resolution is on the `to` side
(`crew.sh:400-421`). Fixing at the producer is the right end.

One session-less producer remains, deliberately: `crew stall-watch` strips the `#session`
suffix into `me="worker:$branch"` under INV-W0 (`crew.sh:1556-1558`, written by `_post`
at `crew.sh:1693-1703`) because a watchdog cannot know which id shape launched it. That
is a designed branch-keyed identity carrying `source:"watchdog"`, not this defect — and
`crew.sh` is out of scope regardless. Noted so a later reader does not conclude the
session-less producer set is now empty. Pre-#17 legacy rows
already in the bus stay session-less and still fold that way; they are history, not new
writes.

### What happens to the compat path

The fallback arrived in `6b0f51c` (2026-08-05) as a migration shim — "A session launched
before this change has no `CREW_WORKER_ID` and keeps the old id" — covering sessions
already in flight when session-scoped ids landed. Twenty-five days on that constituency
is empty, and the shim now serves only sessions that are not workers.

Silence does remove a diagnostic: today an env-loss regression announces itself as a
`session: null` roster row, afterwards it looks like the hook never ran. That is
acceptable because the producer invariant is test-enforced — `tests/dispatch.bats:878-886`
pins `CREW_WORKER_ID=` into the `new-window` invocation, so a regression that stopped
exporting it fails the suite rather than degrading silently on the bus.

### Coverage of the backstop, stated honestly

Claude and codex only. `adapters/cursor/` ships no `hooks.json` at all, so cursor
workers have never had this backstop; that gap is pre-existing and out of scope.

### What happens to a genuinely crashed worker

Unchanged, and verified by direct invocation: with `CREW_WORKER_ID` set and the last
state `working`, the hook records `exited` under the sessioned id, and the
`done|failed|pr_open` dedupe guard still suppresses an override. Two things are _not_
verified and nothing here depends on them: that `tmux kill-window` reaches `SessionEnd`
at all, and that the backstop has ever fired correctly in production — the bus segment
that would show it is missing (above).

## Scope

In scope: `adapters/core/dispatch-notify.sh`, its two generated copies (regenerated by
`scripts/gen-adapters.sh`; CI asserts no drift), and a new `tests/dispatch-notify.bats`.

Out of scope: `crew.sh` entirely — including `exit_suspect` (#71/PR #72) — and `reap`
(#68/PR #70). Both of those PRs have now merged and this branch is rebased on them; the
fix still touches neither file, so the separation held. `tests/helpers.bash` must not be
modified either: `stub_tmux` lives in
`tests/crew.bats:15`, and promoting it into the shared helper would touch a file both
adjacent PRs also edit. Use the existing `stub_bin` (`tests/helpers.bash:24`) for
tmux instead.

## Acceptance

1. A `SessionEnd` in a worker worktree from a session with **unset** `CREW_WORKER_ID`
   leaves the bus file byte-unchanged and invokes no `tmux display-message`. Assert the
   _absence of a `display-message` invocation_ specifically, not an empty stub log.
2. Same, with `CREW_WORKER_ID` set to the **empty string**. (An implementation using
   `[ -z "${CREW_WORKER_ID+x}" ]` passes 1 and fails here — this is what makes the
   unset-or-empty decision independently falsifiable.)
3. After such an event, `crew roster` returns exactly one row for the branch, with
   `session` non-null and `state: "working"` — i.e. no `session: null` row exists.
   Achievable without `stub_tmux`: `roster` calls `tmux list-panes` unconditionally
   (`crew.sh:867`), but that call is `2>/dev/null || true`, so an absent or stubbed tmux
   is harmless; only the worktree/pane resolution at `crew.sh:875-887` is gated on
   `state == "exited"`. Note this criterion asserts a property of an unmodified fold over
   a fixture — `WORKER_TASK.md` demands it, but it is not coverage of the fix itself.
4. A `SessionEnd` from the dispatched worker session (`CREW_WORKER_ID` set) whose last
   state is `working` still records `exited`, under the sessioned id.
5. The same real-worker `SessionEnd`, with `dispatcher_pane` present in
   `WORKER_TASK.md`, still invokes `tmux display-message -t <pane>` exactly once — the
   guard must not delete the hook's original purpose along with the false one.
6. A worker that already reported `done` / `failed` / `pr_open` is still not overridden.
7. `shellcheck` clean; generated adapters in sync; full bats suite green.
