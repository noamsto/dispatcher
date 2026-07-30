# Fable orchestration consult — design

**Date:** 2026-07-16
**Status:** design (locked, pre-plan)
**Relates to:** #86 (dispatcher routing-judge exploration), #75 (worker pipeline speedups — opus plans / sonnet implements)

## Problem

The dispatcher→worker harness has critics for the _spec_ and the _plan_, and a
reviewer for the _code_, but nothing brings the strongest available model to
bear on **how a hard task should be decomposed into sub-agents**. Today the
worker session (opus) plans and `execute` sub-agents (sonnet) implement (#75) —
good enough for most work, but a genuinely architectural task gets an
opus-authored decomposition with no stronger second opinion, and no explicit
"should this be parallelised / is the split right" step.

We want Fable (`claude-fable-5`, strongest, but ~2× opus, minutes-long turns,
refusal-prone on security-adjacent code) available as a **consulting** agent for
decomposition — without putting it on the dispatcher's critical path.

## Non-goals

- **Fable as the dispatcher's seat.** Rejected in #86: the dispatcher is the
  longest-lived seat on the fleet's critical path (~90% mechanical +
  blocked-worker `await`/`watch`); a minutes-long Fable turn there stalls the
  whole fleet.
- **Across-worker "scout"** (splitting one task into several coordinated
  worktrees/PRs). Deferred — it violates the harness's "hand out disjoint work"
  contract and needs cross-PR sequencing/merge-order primitives the bus does not
  have. Its own spec later.
- **A live persistent Fable "oracle" seat.** Deferred behind #86's gate ("only
  build once opus is _caught_ misrouting in a real run"). This design makes that
  gate measurable for the first time (see Outcome logging).

## Key decisions (and why)

The design hinges on two constraints that recur throughout the harness:

- **Context locality.** A good decomposition depends on the _code_ — files,
  seams, coupling. The dispatcher judges from task **text only** and is never in
  a worktree; the worker **is**. So the decomposition — _and the decision to do
  one_ — must happen where the code is.
- **Critical-path load.** Anything synchronous and slow on the dispatcher stalls
  every worker's coordination. Anything inside a worker is already async from the
  dispatcher and parallel across workers.

### 1. Pull-biased, worker-triggered — the dispatcher's judge is unchanged

The consult self-triggers at the **worker's plan seam**, not at dispatch time.
The complexity decision is as code-dependent as the decomposition itself, so the
blind dispatcher is the wrong seat to make it — a textually simple task in a
tangled subsystem would be misjudged `simple` exactly where Fable would pay off
most. The dispatcher's existing `deep` tier already carries the "big /
architectural" signal, so we key the consult off `deep` + an in-worktree survey.

**No separate dispatcher hint in the first cut.** An earlier draft added an
`orchestrate: true` force-on hint, but it collapses on inspection: the
worker-session model (opus vs sonnet) is **baked at launch** by
`dispatch <tier> <model>`, so a running sonnet (standard-tier) session cannot
upgrade _itself_ to the opus session a Fable strategy needs — the "sonnet
stewards Fable" incoherence we reject. For a hint to mean anything on a
standard-judged task it would have to act at **dispatch time** (a `dispatch.sh`
change), which is a whole extra lever. So we drop it: **the dispatcher's lever
for forcing decomposition IS tiering the task `deep`** (the existing "ambiguous /
architectural / wide-blast" signal). No new hint, no `dispatch.sh` change, no
silent-no-op field. If a deep task's survey misses a subtle-but-few-files
decomposition, the false-negative recovery (Trigger logic, below) still catches
it one revision-cycle later — so we lose immediacy, not correctness.

The converse — a **standard worker that discovers true deep-complexity mid-run**
— does _not_ self-consult (a sonnet session can't coherently steward the result).
It escalates via the existing crew bus for **re-tier**, and the re-dispatched
deep worker takes the normal survey path. Re-tiering is the recovery, not an
in-place consult.

### 2. Ephemeral in-worktree Fable subagent, not a shared live seat

The worker spawns Fable via the Agent tool (`model: fable`) inside its own
worktree, so Fable reads real code natively. A shared live seat was rejected for
this workload: one seat serialises minutes-long turns across N parallel workers,
and a detached seat cannot see any worktree — reintroducing the context-blindness
that sank the dispatcher-side approach. (Idle cost is _not_ a factor — a live
seat on `crew watch` is zero-token; serialisation + blindness are the killers.)

### 3. SEED, not REPLACE, with a structural contract

Fable does **not** author the final plan (that would force the harness's plan
schema — `implement: opus` step tags etc. — onto Fable and balloon its turn).
Instead Fable writes a small typed artifact to the worktree:

It is a **file**, not just a returned message, because the subagent's relayed
final message is a lossy compression point and downstream stages should read the
artifact directly.

**The field set is locked here** (not deferred) — it is the interface three
components share, and "conforms" is undefinable without it. Fable emits, exactly:

- `components` — the units of work; each is one coherent change with a stable
  identifier.
- `boundaries` — per component, the files/packages it may and may not touch.
- `ordering` — the dependency order between components, and which may proceed in
  parallel.
- `interfaces` — the contracts (signatures / data shapes) that must stay stable
  _across_ the component split, so components compose.
- `risk` — a per-component tag (e.g. `security-adjacent`, `wide-blast`,
  `migration`) that feeds the review-gate lens selection.

(The on-disk serialisation — headings vs a fenced block — is a plan detail; the
field set above is not.)

Then — wired through the existing `spec-plan-critic` skill, which the worker
already invokes for the plan phase:

- **`spec-plan-critic/SKILL.md`** — the plan-draft step passes `DECOMPOSITION.md`
  (when present at the repo root) to the drafting agent as a **hard constraint**.
- **`agents/plan-critic.md`** — gains a **6th attack axis**: _decomposition
  conformance_. When `DECOMPOSITION.md` is present, the plan must _conform or
  justify each deviation_. **Conforms** is defined against the locked fields:
  every plan step maps to exactly one `component`; step order respects
  `ordering`; no step touches outside its component's `boundaries`; steps
  preserve the declared `interfaces`. Any unjustified deviation is blocking. This
  guards against the opus worker silently flattening the Fable strategy to appease
  the critic during the revision loop.

### 4. Provenance-blind critic

Neither the plan text nor `DECOMPOSITION.md` handed to `plan-critic` may
advertise "Fable produced this" — the critic reads `DECOMPOSITION.md` as _the
decomposition_, author-less — otherwise it performs deference instead of
verification. The
weaker-critic-vs-stronger-plan asymmetry is fine _because_ the critic checks
concrete, checkable properties (missed files, sequencing, untested edges), not
strategy elegance. The conforms-or-justifies check (§3) is what catches silent
downgrades.

### 5. Fallback is mandatory in the first cut

Fable refusal / timeout / unavailability must degrade to the plain
`writing-plans` path — **byte-identical to today**, never a worker failure. This
copies the existing "a should, not a blocker" pattern for the codex-diverse
reviewer (`WORKER_PROTOCOL.md:39`). Security-adjacent code — Fable's documented
refusal-risk zone (`dispatch-orchestration.md:41`) — is disproportionately the
`complex` population, so this is load-bearing, not defensive boilerplate.

### 6. Outcome logging — with a counterfactual

**Every deep worker** — consulted or not — emits a small metrics struct to the
crew bus at finish:

- `consulted` — bool (did the Fable consult fire).
- `plan_critic_first_pass` — the first-attempt verdict.
- `rework_count` — `execute`-stage rework.
- `review_high` — review-gate HIGH findings.

**Emit channel (no `crew.sh` change).** The bus has no metrics field on a
`status` event (its body carries only `state`/`detail`/`pr_url`), so the worker
appends the struct as a normal `crew msg` to a synthetic sink:
`crew msg "worker:<branch>" "metrics:<crew_id>" '<json>'`. This lands a
`kind:"msg"` event in `events.jsonl` and — crucially — does **not** wake the
dispatcher, whose `watch`/`inbox` filter `to==dispatcher:<crew>`/`*`, never
`metrics:<crew>`. Analysis is offline: `crew log <crew> | jq 'select(.to ==
"metrics:...")'` joined against the `kind:"dispatch"` events by branch. Surfacing
the struct as columns in `crew report` is a **deferred** nicety, not first-cut.

Logging the same metrics on **non-consulted deep workers is the baseline**:
without it there is nothing to compare consulted runs against, and "did Fable
beat opus-alone" — the entire 2× cost question and the whole point of #86's gate
— stays unanswerable. This is what makes that gate satisfiable for the first
time. It also tests the runner-up risk: that Fable's strategy survives three
lossy compressions (strategy→plan→critic-revision→sonnet-execution) with enough
value left to justify the cost.

## Components changed

| File                               | Change                                                                                                                                                                            |
| ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `WORKER_PROTOCOL.md`               | Plan-seam survey; gated ephemeral Fable consult (Agent `model: fable`, in-worktree) → `DECOMPOSITION.md`; plain-path fallback; false-negative recovery; deep-worker metrics emit. |
| `skills/spec-plan-critic/SKILL.md` | Plan-draft step passes `DECOMPOSITION.md` (when present) to the drafting agent as a hard constraint.                                                                              |
| `agents/plan-critic.md`            | 6th attack axis: decomposition conformance (conform-or-justify), author-less.                                                                                                     |
| `DISPATCHER_PROTOCOL.md`           | One line: decomposition-complex tasks are the existing `deep` signal — no new lever. (The worker owns the consult decision.)                                                      |
| `dispatch-orchestration.md`        | Note the consult in the choosing-tree; document the outcome-metrics struct.                                                                                                       |

No `dispatch.sh` / `crew.sh` change — the consult is internal to the worker's
Agent-tool calls, and metrics ride the existing `crew msg`. Every touched file
(protocol `.md`, the skill, the agent) is read-by-path / hot-loaded off the main
checkout's `.claude` symlinks, so this deploys on **merge to main**, no rebuild —
but for the same reason the edited skill/agent do **not** resolve while on this
worktree branch (see the skill-symlink-worktree gotcha), so live verification
must run after merge, or against the main checkout.

## Trigger logic (worker plan seam)

The consult decision is made **once, before planning** (the survey) — never
mid-revision, which would collide with the existing revision cap (`spec-plan-critic`
/ `WORKER_PROTOCOL.md` rule 3). The one retry path is a **false-negative
recovery**: if a deep worker exhausts the revision cap _without having consulted_,
that is the signal the survey missed a genuinely complex task, so it earns **one
consult-seeded attempt beyond the cap** — an explicit, bounded exception to rule
3 (total attempts stay finite: cap + 1). A worker that already consulted and
still exhausts the cap surfaces `escalations[]` as today — no extra attempt.

The loop below is **schematic** — the concrete revision-cap value lives in
`spec-plan-critic` (initial attempt + up to 2 revisions); the point here is the
_shape_ (consult-once, plain-path fallback, one recovery attempt beyond the cap),
not the counter.

```
if tier != deep:
    return plain path                        # unchanged

survey  = cheap in-worktree scan (modules touched, blast radius)
consult = survey.trips

decomp = None
if consult:
    try:    decomp = fable_subagent(worktree, task)   # ephemeral, in-worktree → DECOMPOSITION.md
    except (refuse | timeout | unavailable):
            consult = False                  # a should, not a blocker → plain path

plan = writing-plans(constraint = decomp)    # decomp None ⇒ plain path
verdict = plan-critic(plan)                  # provenance stripped, + conforms-or-justifies
# … spec-plan-critic runs its revision loop up to its own cap …

if verdict == reject and not consult:        # false-negative recovery, one shot beyond cap
    try:    decomp = fable_subagent(worktree, task)
    except (refuse | timeout | unavailable): surface escalations[]; return
    plan = writing-plans(constraint = decomp)
    verdict = plan-critic(plan)              # + its revision loop

if verdict == reject: surface escalations[]  # else → execute
```

## Deferred (explicitly)

- **Across-worker scout** — multi-PR decomposition + coordination. New subsystem.
- **Live Fable oracle** — dispatcher-side routing arbitration, behind #86's gate,
  now measurable via the outcome-log field.
- **Dispatch-time force-consult flag** — a `dispatch.sh` change that lets the
  dispatcher pre-empt the survey and force the consult on a deep task it knows is
  subtle-but-few-files. Deferred until the outcome log shows survey
  false-negatives (deep workers that needed the cap+1 recovery) are common enough
  to justify the extra lever. Until then, `deep` tier + the recovery cover it.

## Open questions for the plan

- Exact survey heuristic + the `>N modules` threshold (repo-relative). Tuning
  only: the plan needs the survey to emit a boolean from cheap in-worktree
  signals (modules touched, blast radius); the number itself settles in tuning.

(The `DECOMPOSITION.md` field set and the revision-cap retry path were open
questions in an earlier draft; both are now settled above — §3 and the Trigger
logic block — because deferring them would let the plan implement against an
undefined interface.)
