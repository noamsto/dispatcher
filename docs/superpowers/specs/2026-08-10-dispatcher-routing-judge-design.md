# A stronger judge for dispatcher routing — design (exploration, not a commitment)

**Issue:** #37 · **Related:** #36 (worker/model ratings, open, unimplemented) · **Date:** 2026-08-10

> Per #37's own framing: this document answers the question, it does not open a
> PR against `dispatch.sh`, `DISPATCHER_PROTOCOL.md`, or the launcher. If the
> recommendation below implies a protocol change, it is described, not made.

## Problem

The `dispatcher` launcher pins no model — the dispatcher session runs on the
settings default, opus. Every dispatched task passes through one judgment call:
`{tier, engine, model, effort}`. Tier is the load-bearing one — it sets pipeline
depth, and `trivial` runs **no critics at all**. So a tier misjudgment on a
security-sensitive task doesn't just ship a worse fix, it ships an **unreviewed**
one: the gate that would have caught it was never armed.

That asymmetry is real. The question is whether it's worth paying for a
stronger judge, and if so, which shape of "stronger" actually buys it.

## Options

### (a) Leave the dispatcher on opus — status quo

No change. Routing stays a rubric applied by an opus session that is also doing
~everything else in the fan-out: parsing `crew watch` batches, INV-1 re-arm,
roster-diagram re-renders, answering `blocked` workers inside their bounded
`crew await` window.

### (b) Pin the dispatcher session to a stronger model (Fable)

`dispatcher --agent claude` (or the launcher) starts on `claude-fable-5`
instead of opus. Every turn in the session — not just routing turns — now runs
at Fable's cost and latency.

### (c) Delegate only the routing decision to a per-task classifier subagent

Dispatcher session stays on opus. At each `dispatch` call, spawn a one-shot
Fable subagent (`Agent` tool, `model: fable`) that reads the task text and
returns `{tier, engine, model, effort}` + rationale as structured output. The
dispatcher acts on it. Nothing else about the session changes.

This is not a new idea in this repo: `2026-07-16-dispatcher-fable-orchestration-consult-design.md`
already built the identical shape one layer down — an ephemeral, in-worktree
Fable subagent that _seeds_ a worker's plan without becoming the worker's
session — and explicitly named "Live Fable oracle — dispatcher-side routing
arbitration" as deferred **behind this issue's gate**. Option (c) is that
deferred idea, now examined on its own.

### (d) Route only _uncertain_ tasks to the classifier

A refinement of (c): most tasks aren't actually close calls — the rubric in
`DISPATCHER_PROTOCOL.md` resolves them cleanly (single-file rename → trivial,
obviously). Spend the classifier call only when an explicit uncertainty trigger
fires — e.g. the task text matches cues for **two** tiers at once (e.g. "few
files" _and_ "auth"/"crypto"/"payment"/"migration"), or the dispatcher's own
first pass lands on a tier boundary it can't confidently justify in one
sentence. Everything else routes on the existing rubric, unchanged.

## Cost model

All three engines here are subscriptions — cost is quota burn, not a metered
API dollar figure (`dispatch-orchestration.md` → "Burn classes"). The only hard
number the model map gives is **Fable ≈ 2× opus per turn**. Everything below is
built from that one ratio plus a turn count derived from the protocol's own
documented behavior — it's an illustrative order-of-magnitude model, not a
measured one (there is no telemetry to measure it with yet — see "Sequencing"
below).

**Turn accounting for one fan-out.** Take a representative fan-out at the
protocol's own soft cap — 8 trivial-weight-equivalent tasks
(`DISPATCHER_PROTOCOL.md` → "Cap fan-out by tier weight"). Per task:

- 1 routing judgment at dispatch time.
- The watch loop parks at `--timeout 270` while the roster is ACTIVE and wakes
  on terminal states or questions, handling a whole batch in **one** turn
  (`DISPATCHER_PROTOCOL.md` → "Read the bus" step 2) — so turns don't scale
  1:1 with worker count, they scale with batch arrivals. For a fan-out running
  active for roughly 30–45 minutes, that's ~7–10 heartbeat/event wakes, plus a
  handful of `blocked` replies (say 2–3 for 8 tasks).

Rough total: **8 routing turns + ~12–15 mechanical turns ≈ 20–23 turns**, of
which routing is roughly a third.

**Per-fan-out burn, in opus-turn-equivalent units:**

| Option                                          | Routing turns      | Mechanical turns | Total burn    |
| ----------------------------------------------- | ------------------ | ---------------- | ------------- |
| (a) opus session, unchanged                     | 8 × 1 = 8          | ~15 × 1 = 15     | **~23 units** |
| (b) Fable-pinned session                        | 8 × 2 = 16         | ~15 × 2 = 30     | **~46 units** |
| (c) opus session + per-task Fable classifier    | 8 × (1 + 2) = 24†  | ~15 × 1 = 15     | **~39 units** |
| (d) opus session + classifier on ~25% uncertain | 6×1 + 2×(1+2) = 12 | ~15 × 1 = 15     | **~27 units** |

† (c)'s routing cost is the opus wrapper turn (invoke + read the subagent's
structured output, ~1 unit) plus the Fable subagent's own classification turn
(~2 units) — the dispatcher doesn't stop doing the judgment turn, it now also
pays for a second one.

The comparison that matters isn't (a) vs (b) in isolation, it's **where the
extra spend lands**. (b) pays 2× on every mechanical turn — batch-parsing,
re-arming, roster diagrams, blocked-worker replies — none of which benefit from
a stronger model; they're bookkeeping. (c) and (d) pay the premium rate
**only** on the routing decisions, which is the entire point of #37's framing.
(d) is the cheapest of the three non-status-quo options because it doesn't
spend on decisions the existing rubric already resolves cleanly.

**Per-decision cost** makes the same point more sharply: (a) = 1 unit/decision;
(b) = 2 units/decision **and** 2 units on every mechanical turn that comes with
it; (c)/(d) = 3 units/decision, 0 extra on mechanical turns. (b) is worse than
(c) on a per-decision basis _and_ taxes work that was never in question.

## Latency

(c) and (d) add a **blocking call before `dispatch`** — a Fable subagent turn,
documented as "minutes-long." For a single ad-hoc task arriving on its own,
that's minutes added to the front of that task's wall-clock, though it's small
next to the worker's own run time (tens of minutes for `standard`/`deep`). For
the harness's actual multi-task usage pattern — `project-autopilot` fanning a
whole Linear project, or a human handing over a batch — the classifier calls
for the whole batch can be dispatched as **parallel subagent calls in one
message** before the sequential `dispatch` calls begin, collapsing N sequential
minutes-long waits into one overlapped wait. That mitigation is real but only
applies when tasks arrive in a batch; it does nothing for the steady trickle of
one-off tasks.

(b) adds no _per-decision_ latency — routing happens inline, no extra call —
but it makes **every** turn minutes-long, including replies to `blocked`
workers. Those replies sit on the worker's bounded `crew await --timeout 300`
window (`WORKER_PROTOCOL.md`); a Fable-pinned dispatcher turn that takes
several minutes to compose a reply risks blowing through that window,
producing a spurious `blocked, no dispatcher reply` failure that has nothing to
do with the routing question. This is the sharpest argument against (b): it
degrades a latency-sensitive path (unblocking a worker) to buy speed on a path
(routing) that doesn't need Fable's specific strength.

## The failure mode that motivates this — and the sequencing question

**There is currently no measurement of routing quality in this repo.** #36
(worker/model ratings) is unimplemented — it would give a per-`(engine, model,
tier)` rollup of `rework_count`, `review_high`, `plan_critic_first_pass`,
`review_rounds` (post-hoc GitHub reconcile), `merged`/`reverted`. That rollup
is close to a routing-quality signal, not just a worker-quality one: a
`trivial`-tier task that then racks up multiple `review_rounds` on its PR, or
gets reverted, is exactly the fingerprint of "the review gate that should have
caught this was never armed" — the failure mode #37 is worried about. Without
#36, we have zero evidence of how often that actually happens; we only have the
theoretical asymmetry the issue describes.

This is the same gate the Fable-consult design doc already drew: it explicitly
deferred a "Live Fable oracle — dispatcher-side routing arbitration" behind
this issue, made measurable via its own outcome-log fields, and #36 is the
mechanism that would supply the counterfactual. Building a stronger router now
means spending real quota burn (see above) against a failure mode we can't yet
size, on top of a mechanism (#36) that already exists on paper to size it
first.

**Sequencing answer: #36 before this.** Land #36, let it accumulate enough
dispatched-task volume to be readable (the ratings design's own honesty about
volume applies here too — a handful of runs a week means weeks, not days,
before the rollup means anything), then come back to this question with actual
numbers instead of a plausible-sounding asymmetry. That is a defensible "not
yet," not a dodge — see Recommendation.

## Refusal risk

The model map's refusal-classifier warning on Fable is scoped to
security-adjacent **code generation/editing** — an agent asked to write,
modify, or reason concretely about exploit-shaped code in a live worktree. A
router in options (c)/(d) does none of that: it reads a task **description**
(natural-language prose, at most referencing "auth" or "the login endpoint" by
name) and emits a four-field structured tuple plus a rationale sentence. That's
a classification task, not code authorship, and it's the same category of
content a human security triager reads constantly without incident. The
inherited warning doesn't meaningfully transfer to this use.

The residual risk — a task description that happens to paste raw
exploit/vulnerability code inline (unusual, but not impossible for a
security-fix ticket) — is already covered by the same pattern this harness uses
everywhere else a top-tier consult can decline: "a should, not a blocker." A
classifier refusal or timeout falls back to the existing rubric-only judgment
(option (a)'s behavior), never fails the dispatch.

## Recommendation

**Leave the dispatcher on opus. Do not build a per-task classifier or pin a
stronger model yet.** Every one of the cost/latency arguments above favors (c)
or (d) _over_ (b) — a full Fable pin is worse on both quota burn and
blocked-worker latency, with no compensating benefit, and should be considered
closed regardless of what #36 eventually shows. But (c)/(d) still cost real
premium-class burn (§Cost model) against a failure mode with **zero current
evidence** (§Sequencing) — #37's own text says as much: "confirm opus is
actually misrouting in a real run. No evidence of that yet." Spending quota to
sharpen an unmeasured decision isn't a defensible trade.

**The trigger to revisit:** #36 lands and its rollup, over enough dispatched
volume to be readable, shows a tier-mismatch fingerprint — `trivial`/`standard`
tasks with disproportionate `review_rounds`, `reverted`, or
escalate-and-redispatch-stronger rates relative to `deep` tasks of similar
size. If that shows up, the lever to reach for is **(d)**, not (b): a per-task
classifier gated on an explicit uncertainty trigger, never a full session pin.

**One cheaper, zero-cost lever worth flagging for a separate follow-up** (out
of this document's scope — it would touch `DISPATCHER_PROTOCOL.md`, which this
worker does not own): the rubric currently reads "when genuinely on the fence,
pick the cheaper tier/model and say why" (`DISPATCHER_PROTOCOL.md` line 17).
#37's own first candidate — bias the fence-sitting case toward _tier_, not
model, since tier is what removes the review gate — is a same-day, zero-latency
change that doesn't wait on #36 or cost any quota. It's a different lever than
the one this issue asks about (a rubric tweak, not a stronger judge), which is
exactly why it's worth doing regardless of how the routing-judge question
resolves.
