# Dispatch reuse-or-refuse + session-scoped bus identity — design

Date: 2026-08-04
Status: approved (pending plan)
Issue: #17

## Context

`dispatch` onto a branch that already has a worktree — `--pr N`, a re-dispatch after
`failed`, a fix worker after a review worker — scaffolds a **second agent in the same
worktree**. `wt switch` correctly lands in the existing tree (git allows one worktree per
branch), `WORKER_TASK.md` is overwritten, and `tmux new-window` + the engine launch run
unconditionally. Nothing reuses the existing window and nothing refuses. Observed: three
live agents rooted at `agents-spicedb-rel-migrate-88ee`, two at
`eng-7452-archer-id-migration`, seven agent processes across five worktrees.

Two defects, coupled:

1. **No reuse-or-refuse** (`adapters/core/dispatch.sh:284-328`, `:370`). Silent stacking
   risks an index/HEAD race between concurrent committers. In the observed run a codex
   session that had just pushed was still resident while the next claude session began
   committing in the same directory. They happened to be near-sequential — luck, not a
   guarantee.

2. **Bus identity is branch-keyed.** The worker derives its own id as
   `worker:$(git branch --show-current)` (`WORKER_PROTOCOL.md:12-13`), so N sessions on one
   branch share one roster row and one durable inbox. Consequences, both observed:
   - One identity's history read `done → working → pr_open → done → failed → working`,
     which looks like one worker flip-flopping but was three distinct workers.
   - **Correctness, not just observability:** a directive written for session 2 (_"STOP —
     DO NOT PUSH … finish the cross-engine second opinion"_) was drained at startup by
     session 3, a different task on the same branch. Session 3 burned ~4 minutes and ~81k
     tokens on two reviewers it was never assigned, and the stale _"DO NOT PUSH"_
     contradicted its actual instruction to push.

The root cause of (2) is that identity is **self-derived from ambient state**. The fix is to
make it **issued at dispatch time** and carried per session.

`crew reap` does not help: it is gated on the PR being merged/closed, so it deliberately
keeps a worktree whose PR is still open.

## Design principles applied

- **Define errors out of existence.** A stale directive is not detected and filtered; the
  new session simply _is not the addressee_, so delivery cannot happen.
- **Pull complexity downward.** One new seam (`crew sessions`) knows how to fold sessions
  out of the log. Three callers consume it instead of hand-rolling the same jq.
- **Don't ask agents to be reliable about cleanup.** Rather than making engines
  self-terminate after a terminal status (engine-specific, best-effort, races SessionEnd),
  the tree is released at the only moment it is contended — the next dispatch that needs
  it — plus a time-based sweep.

## Part 1 — `crew sessions` (new subcommand, the seam)

```
crew sessions <branch> [--crew ID]
```

Emits a JSON array, oldest → newest:

```json
[
  {
    "session": "s1780000000-4211",
    "worker_id": "worker:feat/17-x#s1780000000-4211",
    "state": "done",
    "ts": 1780000123456,
    "age_s": 412,
    "terminal": true
  }
]
```

- Folds `dispatch` events (which sessions exist on this branch — including one that has not
  posted a status yet) against `status` events keyed on `.from` (each session's latest
  state).
- Every fold is **per session**. Nothing is aggregated across sessions of a branch.
- `terminal` = state ∈ `done|failed|exited`. `pr_open` is **not** terminal: the worker posts
  `pr_open` then keeps running to `done`.
- A session that has a `dispatch` event but no `status` yet (still booting) reports
  `state: null`, `terminal: false`, and takes its `ts`/`age_s` from the dispatch event.
- A branch with no sessions at all emits `[]`.
- **No crew filter by default**, matching `reap`'s rationale — the sessions worth inspecting
  are precisely the ones from earlier dispatcher crews. `roster` passes its own `--crew`, so
  roster stays crew-scoped.
- Legacy `worker:<branch>` events (no `#`) fold in as `session: null`, so pre-change history
  stays readable rather than vanishing from every reader.

Consumers: the dispatch gate (Part 2), `crew reply` resolution (Part 3), `roster` row
enumeration (Part 3).

## Part 2 — The dispatch occupancy gate

`dispatch.sh` is restructured to _resolve branch → **gate** → `wt switch`_, so a refusal
costs nothing: no worktree, no tmux window, no minted GitHub issue. (Today branch
resolution and `wt switch` are interleaved in one if/else.)

**Occupancy is keyed on the worker _window_, not the running command.** dispatch already
stamps `@crew_name` on every worker window (`dispatch.sh:374`); it is readable as
`#{@crew_name}` and — load-bearing — it **survives the engine process exiting**. Matching on
`pane_current_command ∈ {claude, codex, cursor-agent}` (the idiom `roster`/`reap` use) would
miss a finished agent that has dropped to a shell prompt, which is exactly the issue's
"two tmux windows, one worktree" repro.

The target worktree is resolved from `git worktree list --porcelain` for
`refs/heads/$branch` **before** the switch. A branch with no worktree has no occupant and the
gate is a no-op — so the common fresh-branch dispatch pays one `git worktree list`.

```
tmux list-windows -a -F '#{window_id} #{@crew_name} #{pane_current_path}'
```

A window is an occupant when `pane_current_path` equals the target worktree **and**
`@crew_name` is non-empty, **excluding**:

- `@crew_name == dispatcher` — the dispatcher launcher sets the same option, and reclaiming
  it would kill the orchestrator's own window.
- the window owning `$TMUX_PANE` — never reclaim the caller.

Classification, given occupant window(s):

| Occupant state                                                                                   | Action                                                |
| ------------------------------------------------------------------------------------------------ | ----------------------------------------------------- |
| no occupant window                                                                               | proceed                                               |
| occupant + engine running + newest session **non-terminal** (`working`/`blocked`/`pr_open`/none) | **refuse**, exit 1                                    |
| occupant + engine running + newest session **terminal**                                          | **reclaim**                                           |
| occupant + **no engine running** (dropped to a shell)                                            | **reclaim** — the agent is gone, the window is litter |

Engine-running is read from the panes of that window
(`pane_current_command ∈ {claude, codex, cursor-agent}`).

**Refusal** exits 1 before any side effect, naming everything needed to act:

```
dispatch: sage (worker:feat/17-x#s1780000000-4211) is working in that worktree
  (age 412s, window @23, pane %33) — git allows one worktree per branch.
  Redirect it:  crew reply worker:feat/17-x "<directive>"
  Or take over: tmux kill-window -t @23, then re-dispatch.
```

**Reclaim** kills the occupant window(s), appends a `reclaim` event
(`{kind:"reclaim", branch, session, state, window, pane}`), prints one line, and proceeds.
Kills are best-effort (`|| true`), like reap's.

## Part 3 — Session identity end to end

Identity becomes `worker:<branch>#<sid>`, `sid = s<epoch>-<pid>`.

`<epoch>` alone collides for two same-second dispatches on a branch with no live worker
window, and an sid collision _is_ the inheritance bug; the dispatch pid closes it.
`DISPATCH_SESSION_ID` overrides the mint, for tests.

**Parsing rule:** split on the **last** `#`. `#` is legal in a git refname (verified:
`git check-ref-format --branch 'feat/12-a#b'` passes), so a first-`#` split would corrupt
such a branch. In jq: `ltrimstr("worker:") | sub("#[^#]*$";"")`.

### dispatch

- Mints the sid and `worker_id="worker:$branch#$sid"`.
- **Exports `CREW_WORKER_ID` in the launch command** for all three engines.
- Stamps `worker_id:` into `WORKER_TASK.md` as the human-readable record.
- Adds `session` and `worker_pane` to the `dispatch` bus event.
- **Prints the worker id on stdout**, so the dispatcher can address the session _before_ the
  worker boots — this preserves the documented "post a scoping note right after dispatch"
  behaviour, which relies on the worker's unbounded startup drain.

The env var, not the task doc, is the authority for the worker's own id. `WORKER_TASK.md` is
**overwritten by the next dispatch on that worktree**, so anything that re-reads it later can
attribute one session's event to another — the same misattribution class this spec exists to
remove. Process env is per-session, immutable, and dies with the session.

### Readers

- **`crew stall-watch`**: positional becomes the **worker id** (branch was only ever used to
  build `me="worker:$branch"`).
- **`dispatch-notify.sh`** (SessionEnd `exited` backstop): reads `$CREW_WORKER_ID`, not the
  branch and not the task doc. Wired as SessionEnd in both the claude-code and codex plugins,
  so both inherit the env. Cursor ships no hooks — a pre-existing gap, unchanged here.
- **`roster`**: one row per **branch** (not per `.from`). Row state/`pr_url`/`age_s` come from
  the newest session; a new `sessions: [{session, state, age_s}]` enumerates the rest. The
  false-`exited` resolution and its `prev_state` are computed **inside** each session —
  folding across a branch would let an earlier session's `working` resurrect a later
  session's `exited`. The codename stays **branch**-derived, so `crew identity <branch>`
  keeps its contract and the tmux tint still matches the window.
- **`report`, `rate`, `reap`**: strip `#<sid>` as well as the `worker:` prefix, keeping their
  branch-level semantics. `rate` additionally records `session` and keys each run's event
  fold on the session rather than the dispatch-to-dispatch time window; legacy session-less
  events keep the existing time-window segmentation, so old runs still fold correctly.
  `run_id` stays `repo:branch:ts0`, so the global ratings store is not invalidated.
- **`crew reply <target>`**: a branch-only target resolves to the newest session on that
  branch and **refuses if that session is terminal** ("no live session on <branch> —
  re-dispatch with the context baked in"); a branch with no sessions is likewise an error,
  not a silent append. An explicit `worker:<branch>#<sid>` is honoured verbatim. Resolution at send time means a message is always addressed to a session that
  exists now, so it can never be inherited.
- **`crew status`/`msg`/`inbox`/`await`/`watch`**: unchanged — they already treat the agent id
  as an opaque string.

### Protocols

- **`WORKER_PROTOCOL.md`** (~15 sites): every `worker:$(git branch --show-current)` becomes
  `$CREW_WORKER_ID`. Identity stops being self-derived, which is the root cause of defect 2.
- **`DISPATCHER_PROTOCOL.md`**: document the refusal and its two remedies; describe
  `sessions[]` in the roster. Rewrite the durability paragraph at `:198` — _"A reply you post
  after it stopped isn't lost … the worker picks it up on its next activation"_ promises
  exactly the inheritance being removed. Replace with: messages die with their session; to
  reach the next worker, re-dispatch with the context baked in. The pre-start case still
  holds, because dispatch prints the worker id.
- Regenerate `adapters/{claude-code,codex,cursor}` via `scripts/gen-adapters.sh` (CI already
  gates on no-diff).

## Part 4 — Idle release in `crew reap`

A new step **before** the existing PR gate. For a branch whose newest session is terminal,
whose terminal status is older than `--idle` (default **3600s**), and which still has an
occupant window: kill the **window only**, keep the worktree and branch, append a `release`
event.

- Skips the tmux _active_ window as a cheap "a human is in there" proxy.
- Reports through `say`, not `note` — dispatch calls `crew reap --quiet`, and a killed window
  must never be silent.
- **Worktree removal stays PR-merged/closed gated, exactly as today.** reap's argument that a
  time-based sweep would delete live work holds for the _tree_ (a worker sits in `done` for as
  long as its PR takes to merge); it does not hold for a _session_ that is finished by
  contract.

Since `dispatch` already runs `crew reap --quiet`, this sweep costs no new schedule.

## Out of scope

- **Making engines self-terminate** after a terminal status. Engine-specific, best-effort, and
  killing one's own pane mid-turn races the SessionEnd backstop. Parts 2 and 4 release the
  tree without it.
- **Converting a refused dispatch into a redirect.** `dispatch`'s postcondition stays honest:
  either a new worker exists, or it failed loudly. The refusal message names the redirect
  command; choosing it is the dispatcher's call.
- **A `--force`/`--adopt` override.** The one case that must never be overridable is the live
  committer, which is precisely what such a flag would override.
- **Per-session codenames.** With the gate in place at most one session per branch is live, so
  the branch-derived codename stays unambiguous and `crew identity` keeps its contract.
- **Cursor's missing SessionEnd hook.** Pre-existing; cursor workers have no `exited`
  backstop today and still won't.
- **A guard against dispatching into the main checkout.** Pre-existing behaviour, orthogonal
  to this issue.

## Validation

bats, extending the existing suites (`tests/dispatch.bats` already stubs `tmux` with a
scriptable stub, so faking `list-windows` is cheap):

**Gate (`tests/dispatch.bats`)**

- Occupant window + engine + bus `working` → exit 1, message names the worker id and window,
  and **no** `new-window`, no `wt switch`, no `gh issue create` ran.
- Occupant window + engine + bus `done` → window killed, `reclaim` event appended, dispatch
  completes.
- Occupant window, **no** engine (shell prompt) → reclaimed regardless of bus state. This is
  the F1 regression test.
- Occupant window with `@crew_name == dispatcher`, and the window owning `$TMUX_PANE` → never
  reclaimed.
- No occupant → unchanged behaviour.

**Identity (`tests/dispatch.bats`)**

- `worker_id:` stamped in `WORKER_TASK.md`, `CREW_WORKER_ID` present in the launch command for
  each of the three engines, worker id printed on stdout, `session` on the dispatch event, and
  the worker id passed to `crew stall-watch`.

**Bus (`tests/crew.bats`)**

- `sessions` folds three sessions on one branch, oldest → newest, with per-session states; a
  legacy `worker:<branch>` event folds in as `session: null`.
- **A directive addressed to session 1 is not returned by `inbox` for session 2** — the
  regression test for the observed correctness bug.
- `reply worker:<branch>` resolves to the newest live session; refuses when it is terminal;
  honours an explicit `#sid`.
- `roster` returns one row per branch with `sessions[]`, and a session's `exited` is not
  resolved from a _different_ session's `prev_state`.
- A branch containing `#` round-trips through every reader.
- `report`/`rate`/`reap` still key on branch when given sessioned ids.

**Idle release (`tests/crew.bats`)**

- Terminal + age past `--idle` + occupant window → window killed, worktree kept, `release`
  event appended, reported even under `--quiet`.
- Terminal but within `--idle`, and the active window → untouched.

**Adapters** — `scripts/gen-adapters.sh` regenerated; the existing no-diff gate covers it.
