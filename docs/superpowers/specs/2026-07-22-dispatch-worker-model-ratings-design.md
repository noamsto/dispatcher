# Dispatch worker/model rating system — design (honest v1)

**Issue:** #102 · **Consumer:** #86 (dispatcher routing judge) · **Date:** 2026-07-22

> Revised after an adversarial spec-critic pass. The first draft promised
> normalized, shape-stratified routing evidence with statistical shrinkage; the
> critic showed that stratification is unbacked here (`shape` is unpopulated) and
> the run volume is too low for shrinkage to mean anything (see "Reality
> constraints"). This v1 is deliberately smaller and truthful: **rate
> engine×model×tier runs on a flat tally of cheap, actually-observable signals**,
> get real data flowing, and let #86 decide later whether the data warrants more.
> (This harness runs on a **work** machine, so all three engines — claude, codex,
> cursor — dispatch and the engine axis is real; see the engine constraint below.)

## Problem

The dispatcher judges each task → `(tier, model, engine, effort)` from a static
rubric with no feedback loop: nothing measures whether opus-deep,
sonnet-standard, etc. actually did well. #86 is gated on that evidence. PR #89
took a first step — deep workers emit a `metrics:<crew>` bus record — but it is
deep-tier-only, per-crew, and never rolled up. We want a persistent, cross-run
tally so a rollup exists for #86 to consult.

## Reality constraints (what the critic established)

These bound the honest scope:

- **Engine variety is gated on the work profile — and this is a work machine.**
  `dispatch.sh:122-129` rejects `codex`/`cursor` only _off_ the work profile;
  here `$DISPATCH_PROFILE == work`, so all three engines (claude, codex/gpt-5.5,
  cursor/grok) dispatch. So **engine is a live rating axis** and cross-engine
  comparison is exactly the routing evidence #86 needs — the fold records
  `engine` unconditionally and the report groups by `(engine, model, tier)`. (On
  a personal box the same code collapses to claude-only rows; that's a data fact,
  not a scope limit — nothing engine-specific is cut.)
- **`shape` is unpopulated.** Nothing sets `$DISPATCH_SHAPE` (no `--shape` flag;
  the dispatcher never exports it; `crew report` already shows `—`). So
  stratifying by `shape` would collapse to a constant. v1 **does not normalize by
  shape**; a `--shape` capture path is a prerequisite for any shape-aware rating
  and is deferred.
- **Low volume.** Fan-out is capped ~8 weighted (`DISPATCHER_PROTOCOL.md:71`);
  this harness produces a handful of runs/week. Splitting those across
  `(engine, model, tier)` — now three engines — makes cells _sparser_, n=0/1/2.
  This is the strongest reason for the descope: v1 reports a **flat descriptive
  tally with raw counts and visible `n`** — no statistical shrinkage, no
  significance claims.
- **External review IS meaningful here.** This harness drives work repos
  (e.g. `factify/mono`), whose PRs get real review (the pr-reviewers suite +
  human review), so `review_rounds` (Phase 2) is a real signal — not the
  ~always-0 self-merge degenerate it would be on a solo personal repo.

## The measurement boundary (unchanged, and correct)

Data arrives in waves; no single observer sees all of it. Design principle:
**each observer writes only what it uniquely knows.**

- **t0 dispatch** — dimensions (`engine/model/tier/effort/title/branch`). Already
  on the bus (`dispatch.sh:200-205`, `kind:"dispatch"`).
- **t1 close** — fields only the worker knows cheaply (`rework_count`,
  `review_high`, `plan_critic` verdict). Worker already emits a subset (deep
  only); v1 extends it to all tiers.
- **t2 resolve** — fields only GitHub knows, read post-session by the sweep.

## Rating record

One record per **run**, keyed `run_id = "<repo>:<branch>:<t0_ms>"`.

### run_id and the branch-reuse problem — resolved in the sweep

status/metrics/blocked events carry no run_id; they key only on
`from: worker:<branch>` (`crew.sh:107-118`). The roster fold explicitly expects
branch reuse (last-wins on re-dispatch, `crew.sh:343-347`). Naively folding by
branch misattributes t1 fields when a branch is dispatched twice.

**Fix — dispatch-boundary windowing, entirely in the sweep, no worker change:**
for each branch, take its ordered dispatch timestamps `d₁<d₂<…<dₙ`; a non-dispatch
event with `dᵢ ≤ ts < dᵢ₊₁` belongs to run `i` (`run_id` = repo:branch:`dᵢ`). This
is fully derivable from existing bus data and attributes each t1/status/blocked
event to exactly one run. Complexity is pulled down into the collector where it
belongs.

**Window boundaries are clean by construction:** `dispatch.sh:186` runs
`wt switch -c "$branch"`, which fails if the branch/worktree already exists — so
run `i+1` on a branch cannot be dispatched until run `i`'s worktree is removed,
i.e. after run `i` terminated and its synchronous close-emit already fired. A late
t1 event can therefore never cross `dᵢ₊₁`. Events with `ts < d₁` (impossible in
practice — a worker exists only post-dispatch) fall into no window and are
harmlessly dropped.

### Fields

| Group      | Field                                                      | Source                              | Notes                                                                                                                                                                                                                                                                                                                                                                                                                |
| ---------- | ---------------------------------------------------------- | ----------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| identity   | `run_id, repo, branch, engine, model, tier, effort, title` | t0 dispatch                         | `engine` is a live axis here (work machine → all 3 engines dispatch)                                                                                                                                                                                                                                                                                                                                                 |
| lifecycle  | `outcome` ∈ {`merged`,`pr_open`,`failed`,`incomplete`}     | bus + t2                            | see outcome rules below                                                                                                                                                                                                                                                                                                                                                                                              |
|            | `reached_pr` (bool)                                        | bus `status`→`pr_open`              |                                                                                                                                                                                                                                                                                                                                                                                                                      |
|            | `time_to_pr_ms` (`pr_open.ts − t0`)                        | bus                                 |                                                                                                                                                                                                                                                                                                                                                                                                                      |
| t1 close   | `rework_count`                                             | worker `metrics`                    | execute-stage gate fixes                                                                                                                                                                                                                                                                                                                                                                                             |
|            | `review_high`                                              | worker `metrics`                    | internal reviewer HIGH findings                                                                                                                                                                                                                                                                                                                                                                                      |
|            | `review_mode` ∈ {full,downgraded,none}                     | worker `metrics`                    | review depth that ran (repo-aware downgrade); read `review_high` in this context, never across mismatched depths. **Consumer rule:** `review_high==null` (`review_mode` `none`/absent — e.g. every codex/cursor run, which runs no claude review gate) is **unmeasured** — exclude from findings means, never score as a clean `0`. `engine`+`tier` disambiguate a process-light codex run from a claude trivial run |
|            | `plan_critic_first_pass` ∈ {accept,revise,reject,null}     | worker `metrics`                    | null if no plan phase (trivial)                                                                                                                                                                                                                                                                                                                                                                                      |
|            | `blocked_count`                                            | bus `blocked` events                | dispatcher-attention proxy                                                                                                                                                                                                                                                                                                                                                                                           |
|            | `reported_ok` (bool)                                       | derived                             | emitted metrics **and** reached terminal status (NOT deslop — deslop isn't on the bus)                                                                                                                                                                                                                                                                                                                               |
| t2 resolve | `merged` (bool), `time_to_merge_ms`                        | `gh pr view --json state,mergedAt`  | cheap + real                                                                                                                                                                                                                                                                                                                                                                                                         |
|            | `review_rounds`                                            | `gh pr view --json reviews,commits` | count of review submissions followed by ≥1 new commit; **~0 on self-merge**, meaningful on work PRs — this is the user's "fix rounds after review"                                                                                                                                                                                                                                                                   |
| cost       | `cost_class, wall_clock_ms, cost_proxy`                    | sweep                               | `price_class(model) × wall_clock`; relative signal, not dollars                                                                                                                                                                                                                                                                                                                                                      |

**Deferred to follow-ups** (critic findings 3, 5): `first_ci_green` (needs
check-runs keyed to the first commit SHA, not `gh pr checks`' current status —
spec it precisely or not at all), `reverted`/follow-up-bugfix (noisy heuristic,
degenerate on solo repo), and any `deslop_hits` (needs deslop-guard logging).

### Outcome rules — zombie ≠ trivial (critic finding 6)

Absent metrics are ambiguous, so classify by **terminal status observed**, not by
null fields:

- terminal `status` = `done`/`pr_open` reached → real run; null `rework_count`
  from a trivial tier is genuine `0`-signal.
- **no terminal status** (dispatch present, then silence — e.g. `kill-window`) →
  `outcome=incomplete`; **excluded from rate denominators**, counted separately
  as an incomplete/zombie tally. A worker that pushed a PR but died before
  stamping `pr_open` leaves no `pr_url` on the bus and is `incomplete`, not
  `failed` — the report shows it as unattributable rather than silently scoring 0.

## Components (phased — each an independently mergeable PR, per the one-PR rule)

**Phase 1 — data capture** (the foundation; ship first):

1. **All-tier close-emit** (`WORKER_PROTOCOL.md:114-118`) — extend the deep-only
   `metrics:<crew>` emit to every tier, wider field set. Cheap local bus write;
   no global-file contention (this write is per-repo).
2. **`crew rate` sweep** (`crew.sh`) — fold the current repo's `events.jsonl` per
   run via dispatch-boundary windowing (§run_id); join `dispatch` +
   terminal `status` + `metrics` + `blocked`. **Carry forward the last non-null
   `pr_url`** (the `done` event drops it — reuse the idiom at `crew.sh:355`, the
   exact non-obvious thing the roster fold already handles).
3. **Global store** — append one record/run to
   `${XDG_DATA_HOME:-~/.local/share}/crew/ratings.jsonl`, **last-wins by
   `run_id`**, `mkdir -p` lazily. Concurrent `crew rate` from two repos contend
   on this file → guard with the existing `_lock_acquire`/`_lock_release`
   (`crew.sh:33-52`), same as `crew watch`.

**Phase 2 — GitHub reconcile** (t2 fields; own PR, may need a small spike): 4. In the sweep, for runs with a carried-forward `pr_url`, `gh pr view --json
   state,mergedAt,reviews,commits` → `merged`, `time_to_merge_ms`,
`review_rounds`. Idempotent: a still-open PR writes a t1-only record; a later
re-sweep upgrades it (last-wins by `run_id`). Stub `gh` (PATH shim) for tests.

**Phase 3 — cost + report** (own PR): 5. **Cost price table** — static `model → price_class` map in `crew.sh`. 6. **`crew rate --report`** — jq over `ratings.jsonl`, folded last-wins by
`run_id`, grouped by `(engine, model, tier)`. Flat descriptive tally: per cell
surface `n` (+ separate `incomplete` count), `merged_rate`, `reached_pr_rate`,
median `time_to_pr_ms`/`time_to_merge_ms`, avg `rework_count`/`review_high`/
`review_rounds`/`blocked_count`, avg `cost_proxy`. Human table + `--json`.
This is the artifact #86 reads. 7. **Docs** — `DISPATCHER_PROTOCOL.md` / `dispatch-orchestration.md`: how to run
`crew rate` and read the tally. No routing behavior change in v1 (that's #86).

### Cross-repo finalization gap (known limitation)

`crew rate` sweeps one repo; the report is global. A run in a repo you never
revisit is never finalized (no daemon — explicit non-goal). Mitigation: the
dispatcher may run `crew rate` for the active repo in its watch-completion
handler, so at least dispatched-from repos get swept. Documented, not solved.

## Non-goals (v1)

- Auto-routing (that's #86 — v1 only surfaces the tally).
- Real per-run token accounting (Claude transcript parse; codex/cursor spike).
- `shape`-aware or shrinkage-based normalization; `first_ci_green`; `reverted`;
  deslop logging. All follow-ups.
- Any new daemon/service.

## Deployment

- `WORKER_PROTOCOL.md` / `DISPATCHER_PROTOCOL.md` / `dispatch-orchestration.md`
  read-by-path off `main` → deploy on **merge**, no rebuild.
- `crew.sh` is a `writeShellApplication` PATH binary → needs `nh home switch`
  after merge to place the `crew rate` subcommand.

## Testing

- **Sweep fold + windowing** — shell-TDD against a synthetic `events.jsonl`
  (two dispatches on one branch + interleaved status/metrics/blocked); assert
  events attribute to the correct run and re-dispatch does not cross-contaminate.
- **pr_url carry-forward** — fixture where `done` drops the URL; assert t2 still
  has a PR to reconcile.
- **Idempotency + lock** — sweep twice; assert one record/run, stable t1→t2
  upgrade; assert the global-store lock serializes concurrent sweeps.
- **`gh` reconcile** — PATH-shim `gh` returning canned JSON; assert
  `merged`/`review_rounds` parsing (incl. self-merge → `review_rounds=0`). The
  fixture **pins the `review_rounds` batching rule**: N reviews before a single
  fix commit counts as one round (a review→fix _cycle_), not N — assert this
  explicitly so the semantics can't drift.
- **Outcome classification** — fixture with a dispatch-then-silence branch;
  assert `outcome=incomplete` and exclusion from rate denominators.
- **Live smoke (post-merge)** — one real claude worker → `crew rate` → inspect
  record → merge PR → re-sweep → assert t2 fills in.

## Follow-ups (out of scope, filed separately)

- Real per-run token/cost accounting (Claude transcript; codex/cursor spike).
- `--shape` capture (flag + dispatcher instruction) → shape-aware rating.
- `first_ci_green` (first-SHA check-runs), `reverted`/follow-up detection.
- #86: wire the tally into automated routing; add shrinkage once volume warrants.
