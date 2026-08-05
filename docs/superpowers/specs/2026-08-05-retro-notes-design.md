# Retro notes — design

**Issue:** #21 · **Prerequisite:** #20 (merged) · **Date:** 2026-08-05

> Revised after reading the current `WORKER_PROTOCOL.md` on `extract`. The first
> framing claimed "nothing records qualitative failure" and justified per-seam
> notes by survivorship bias. Both were overstated: the protocol now emits a
> complete metrics snapshot **before every stopping path**, and issue #3 landed a
> heavily instrumented plan-shaped recovery ledger. The honest target is narrower
> and better: **capture the WHY behind numbers already collected**, plus the two
> cases nothing observes (hard kill, hang), plus the dispatcher's own judgement.

## Problem

`crew rate` folds one record per run with real outcome fields — `rework_count`,
`review_high`, `review_mode`, `plan_critic_first_pass`, `consulted`,
`consult_engine`, `replanned`. Every one is a **number or a boolean**. None says
_why_.

Concretely, three things are structurally lost today:

1. **The plan-shaped recovery ledger.** `WORKER_PROTOCOL.md` §98-115 has the
   worker record `gate` / `target` / `old_plan` / `amendment` rows and run a
   consecutive-count classifier over them. Those rows live in the worker's
   context; the bus receives the single boolean `replanned`. So _what_ the worker
   circled on — which target, which plan statement it kept rewriting — dies with
   the session every time.
2. **Reasons behind null fields.** `consult_engine` records who consulted, but is
   `null` on failure, so refused-vs-timed-out is lost (§70). The recovery ladder
   blocks for several distinct reasons — top rung, unavailable launch, ineligible
   opus, non-viable planner output (§118-135) — all of which reach the bus as a
   bare `failed`.
3. **The dispatcher's own judgement.** Whether a `{tier, engine, model, plan}`
   call was right is observable only by comparing the call to the outcome, and
   only the dispatcher holds both.

## What is already covered (do not rebuild)

- **Stopping paths self-report.** §153: a complete latest-state metrics snapshot
  is emitted before _every_ stopping path — startup-drain dispatcher stop; spec,
  plan or consult terminal failure; done; terminal gate failure;
  dispatcher-requested stop; permission stop; the first blocked-timeout stop; the
  final failed stop. `crew rate` selects the latest timestamp.
- **Permission and await detail.** `crew status blocked "permission: <what>"`
  (§170) and `"<why> — awaited 300s, no reply"` (§167) already carry their reason
  on the bus. No tag needed.
- **Unresolved review findings and escalations** already land in the PR body under
  `## Review notes` (§147) and `## Escalated` (rule 3).

**The only genuinely unobserved terminations are a hard `tmux kill-window` and a
stall-watch hang** — neither reaches a stopping path, so neither emits a snapshot.
That, and nothing else, is what per-seam notes buy.

## Observability boundary

Each observer writes only what it uniquely knows (the principle the ratings design
already uses for t0/t1/t2):

| observer   | writes                                        | blind to                   |
| ---------- | --------------------------------------------- | -------------------------- |
| worker     | tagged notes about its own pipeline           | the other workers          |
| dispatcher | cross-worker patterns + its own routing calls | anything inside a worktree |

## Mechanism — reuse the `metrics:<crew>` idiom

No new event kind and no new **write** subcommand — `crew retro` in phase 2 is
read-only. All three writers use the existing
synthetic-sink `msg` pattern, which already provably does not wake the dispatcher
(its `watch`/`inbox` filter is `to==dispatcher:<crew>`/`*`).

| writer                        | how                                                        |
| ----------------------------- | ---------------------------------------------------------- |
| worker, at a stopping path    | a `notes[]` array **inside the existing metrics snapshot** |
| worker, mid-execute seam      | `crew msg worker:<branch> retro:<crew> '<note json>'`      |
| dispatcher, at roster DRAINED | `crew msg dispatcher:<crew> retro:<crew> '<note json>'`    |

Putting stopping-path notes _inside_ the snapshot is the load-bearing choice: it
inherits the resumed-run/latest-timestamp semantics `crew rate` already
implements, so a resumed worker's notes supersede rather than duplicate.

**Seam notes are execute-only.** Execute is the long stage where kill/hang risk
concentrates; every other seam ends in a stopping path that already snapshots.

### Note schema

```json
{ "seam": "execute", "tag": "gate_thrash", "detail": "…", "ts": 1785917772000 }
```

`ts` is only present on seam notes (`msg` events carry their own `.ts`); notes
inside a snapshot inherit the snapshot's timestamp.

### Tag validation is reader-side — a deliberate step down from `status`

`crew status` rejects an unknown state **at the writer** (crew.sh:117-123)
because a junk value silently corrupted a user-visible surface: `{"state":""}`
rendered as a blank roster row and every reader absorbed it. A mistyped retro tag
is not that: `crew retro` surfaces unrecognized tags in an explicit `unknown`
bucket **with the offending values**, so drift is visible rather than silent, and
no note is ever rejected or lost. That visibility is what justifies reusing `msg`
instead of adding a validating subcommand.

## Vocabulary

Every tag maps to a branch the protocol already defines. A worker writes a note
**only when that branch is taken** — clean runs are silent end-to-end.

| tag                  | fires on                                                          | why it is additive                                    |
| -------------------- | ----------------------------------------------------------------- | ----------------------------------------------------- |
| `command_not_found`  | a discovered gate command is missing or not runnable (§92, §96)   | nothing captures it; harness bugs surface here        |
| `gate_thrash`        | three qualifying ledger rows force a replacement (§98)            | ledger is context-only; bus gets one boolean          |
| `approach_abandoned` | provided/legacy contradiction re-entry (§52)                      | `replanned` says it happened, never which file or why |
| `consult_failed`     | a consultant refuses, times out, or is unavailable (§70)          | `consult_engine` is `null` on failure — reason lost   |
| `rung_blocked`       | any recovery-ladder block (§118-135)                              | all surface as a bare `failed`                        |
| `misrouted`          | dispatcher: an outcome contradicts its `{tier,engine,model,plan}` | its own judgement, unobservable elsewhere             |
| `fanout_binder`      | dispatcher: a concrete binder capped fan-out (429, drag, pile-up) | `DISPATCHER_PROTOCOL.md` calls this the real cap      |
| `spec_too_thin`      | dispatcher: a worker blocked on what the inlined spec should say  | closes the loop on dispatch-time spec quality         |
| `session_summary`    | dispatcher: one narrative per drained session                     | the cross-worker view                                 |
| `other`              | anything else                                                     | the vocabulary must never block a report              |

**`gate_thrash` detail carries the ledger rows themselves** — `gate`, normalized
`target`, quoted `old_plan`, `amendment` category — truncated by the #20 guard.
This is the highest-value payload in the design.

Explicitly **not** tags, because they are already on the bus or in the PR:
`permission_denied`, `blocked_no_reply`, `review_cap_hit`, `revision_cap_hit`.

## Dispatcher synthesis — the anti-slop rules

At roster DRAINED (nothing `working`/`blocked` — the partition
`DISPATCHER_PROTOCOL.md` already computes to choose park length), **before**
re-arming the long park:

1. Read the crew's notes and roster.
2. For each terminal worker, compare outcome against the dispatch call. Emit
   `misrouted` **only** when the outcome contradicts it, naming the contradiction
   (e.g. "trivial, but the diff touched an auth path — no review gate ran").
3. Emit `fanout_binder` if a concrete binder was hit; `spec_too_thin` if a worker
   blocked on something the inlined spec should have answered.
4. Emit one `session_summary` whose detail states, per worker: codename,
   `{tier, engine, model}`, outcome, and the tags its notes carried — plus any tag
   appearing on **≥2 workers**, which is the pattern only the dispatcher sees.

Two rules keep this from generating generic summaries:

- **Every note must quote a specific observable** — a tag, a status detail, an
  outcome, a dispatch field. Never a general impression.
- **A clean drained roster writes nothing at all.** No notes, no summary.

## Read path — `crew retro`

DuckDB SQL over the bus. Globbed across repos via
`read_json('…/crew/events.jsonl', filename=true)`, so cross-repo rollup needs **no
derived store, no lock, and no sweep** — and `crew rate` is not touched.

- `crew retro` — this repo, human table: one row per run with its tags.
- `crew retro --report` — grouped by tag: `hits`, `runs_affected`, engines
  affected, share on non-`done` runs, a sample detail, plus the `unknown` bucket.
- `crew retro --json` — same, machine-readable.

Run attribution reuses the dispatch-boundary windowing, expressed as
`LEAD(ts) OVER (PARTITION BY branch ORDER BY ts)`.

Two verified gotchas: use `json_extract_string(body,'$.state')`, **not** `->>`,
which fails on the bus's mixed-type `body` (object for `status`, JSON-_string_ for
`metrics` — the wart jq handles with `fromjson` at crew.sh:538); and `to`/`from`
need quoting as reserved words.

## Storage decision (spiked)

Measured JSONL vs SQLite before committing; the spike also found #20.

- **Reads are not the constraint.** jq folds 5,000 events in 66 ms — over a year
  of runs at this fan-out. (100k: jq 1222 ms, sqlite 67 ms.)
- **SQLite's write path is a downgrade here.** Without `busy_timeout`, 286 of 320
  concurrent writes were **lost**; with it, 3.3× slower than the append. JSONL
  fails as one visible corrupt line; naive-SQLite fails as a silently dropped
  terminal status the dispatcher is parked on.
- **DuckDB gives the SQL without the migration** — 77 ms rollup at 100k events —
  and can `ATTACH` a SQLite file later, so this is not a bet against SQLite.
- **Revisit if** #86 needs to query at dispatch time (a full fold in the hot
  path). Volume will not be the trigger.

## Phases

Each independently mergeable, per the one-PR rule.

1. **Worker capture** — `notes[]` in the snapshot + the execute-seam note +
   vocabulary, in `WORKER_PROTOCOL.md`; regenerate adapters. No `crew.sh` change,
   so notes start flowing with a protocol merge alone.
2. **Read path** — `crew retro` / `--report` / `--json`; `duckdb` into
   `runtimeInputs`.
3. **Dispatcher synthesis** — the DRAINED trigger and anti-slop rules in
   `DISPATCHER_PROTOCOL.md` + `dispatch-orchestration.md`.

## Testing

- **Fold + windowing** — synthetic `events.jsonl` with two dispatches on one
  branch; assert notes attribute to the correct run and re-dispatch does not
  cross-contaminate.
- **Killed worker** — dispatch + seam notes + no terminal status; assert
  `outcome=incomplete` **with its notes retained**. This is the case seam notes
  exist for.
- **Snapshot supersede** — a resumed run's later snapshot wins; notes are not
  duplicated across both snapshots.
- **Unknown tag** — lands in the `unknown` bucket with its value visible, and is
  never dropped.
- **Silence on a clean run** — a run that takes no trouble branch produces zero
  notes. Asserted, so the zero-cost claim cannot rot.
- **Oversized ledger detail** — a `gate_thrash` detail well over 4 KB still yields
  a parseable single-record line (the #20 guard, exercised through this path).

## Non-goals

- Auto-routing off retro data (that is #86).
- Any derived store, sweep, lock, or daemon.
- Re-instrumenting what the ledger, snapshot, or PR body already record.
- Worker self-assessment of its own quality. Every tag is a protocol branch, not
  an opinion.

## Deployment

Protocols are read-by-path off the default branch → deploy on merge. `crew.sh` is
a `writeShellApplication`, so phase 2 needs a `dispatcher` flake-input bump in
nix-config plus `nh home switch`. Protocol edits go in `adapters/core/` and
regenerate via `scripts/gen-adapters.sh`; CI's drift gate fails if committed
output differs.
