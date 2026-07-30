# Decouple plan-depth from tier — stop re-planning pre-specified worker tasks

Closes #90. Fable-reviewed (verdict REVISE → findings incorporated).

## Problem

A `standard`-tier worker spent ~25 min and ~400k tokens running the full
`spec-plan-critic` loop (draft plan → plan-critic → revise → re-critique) on
ENG-7358, a task whose `WORKER_TASK.md` was **already a complete plan**:
root-cause/mechanism, an explicit file list, a named approach (CSS subgrid +
`min-width:0`), and per-render-path acceptance criteria. The plan phase
re-derived what the dispatcher had already written on disk.

A second symptom on the same run: the plan tagged a fiddly-but-bounded CSS step
`implement: opus`, so the sonnet worker spawned an opus executor for it —
escalating cost past the tier the dispatcher deliberately chose.

## Root cause

`tier` conflates two independent axes:

1. **pre-implementation plan-depth** — should the plan/critic loop run at all,
   and how deep;
2. **post-implementation review-rigor** — how hard the code-review gate works
   (`trivial` = none, `standard` = one pass, `deep` = adversarial).

Today `standard` unconditionally couples "one plan-critic pass" to "one review
pass". Nothing keys off _"is the plan already written in the task doc"_ — so a
fully-specified task still triggers the whole plan phase. Lowering the tier is
**not** a safe workaround: dropping to `trivial` to skip the plan also drops the
review gate entirely (`trivial` has no reviewer).

Reinforcing the redundant run: `superpowers:using-superpowers` is injected at
SessionStart into every worker. Workers are **top-level sessions**, so the
skill's `SUBAGENT-STOP` exemption does not apply — the worker receives the
full-strength mandate ("if there's even a 1% chance a skill applies you MUST use
it", with a red-flag table that explicitly rebuts "this is overkill"). This
biases the worker toward the pipeline and against any skip.

## Design

Seven coordinated changes. The centerpiece is a dispatcher-side stamp
(`plan: provided|required`) — because the dispatcher **authored** the task doc
and therefore knows the answer as ground truth, whereas the worker (the seat
that just demonstrated unreliable judgment here, running under a pro-pipeline
mandate) can only infer it.

### A — Decouple plan-depth from tier (dispatcher)

`WORKER_TASK.md` gains a `plan:` field, judged independently of tier (like
`--effort` is):

- `plan: provided` — the dispatcher wrote root-cause + file list + approach +
  acceptance criteria into the doc. The doc **is** the plan of record.
- `plan: required` — the worker must produce the plan via its tier's pipeline.

Tier continues to govern review-rigor and (when `plan: required`) plan-critic
depth. `plan:` governs whether the pre-implementation plan phase runs at all.

Touches: `dispatch.sh` (new field + arg), `DISPATCHER_PROTOCOL.md` (judgement
rubric: stamp `provided` iff the doc contains all four elements).

### B — Stamp-conditional launch prompt (dispatcher)

When `plan: provided`, `dispatch.sh` changes the worker's launch prompt (the
first user turn) to include: _"The task doc is your plan of record — extract the
steps and implement; do not re-plan or re-critique it."_

This is the structurally-strong superpowers override: a **user-turn direct
request** satisfies `using-superpowers`' own escape hatch verbatim ("Only skip
skill workflows when your human partner has explicitly told you to" / "direct
requests take precedence"). An appended system prompt does not literally match
that hatch wording; a launch-prompt instruction does.

Touches: `dispatch.sh`.

### C — Worker fallback + re-entry (worker)

- **Fallback** (unstamped/legacy docs): the worker runs an _extraction-based_
  self-check at the plan seam — can it (i) quote the file list verbatim, (ii)
  state the mechanism in one sentence quoting the doc, (iii) enumerate the
  acceptance criteria as checkboxes? If extraction succeeds, that artifact **is**
  the bite-sized plan of record → implement. If any extraction fails → plan
  normally. This is a fallback and a **sanity veto**, not the primary signal.
- **Re-entry**: if the plan of record is contradicted by the repo at implement
  time (stale file list, approach doesn't fit), the worker runs `spec-plan-critic`
  once, normally — mirroring the existing deep-tier false-negative recovery.
- **Scope**: the skip applies to `standard`. `deep` may skip the _plan_-critic
  under the same conditions but never the spec-critic / Fable consult (deep is
  chosen precisely when the framing needs adversarial pressure).
- Retain the post-plan checkpoint-peek seam after extraction.

Touches: `WORKER_PROTOCOL.md`, `spec-plan-critic/SKILL.md`.

### D — Process authority as a category rule (worker)

Top of `WORKER_PROTOCOL.md`, phrased as the human-partner instruction: _this
protocol governs the worker's process end-to-end and supersedes
`using-superpowers` for process/lifecycle skills. The harness pipeline
(`spec-plan-critic` + code-review gate) is the process — do not separately invoke
brainstorming, writing-plans, executing-plans, requesting-code-review, or
test-driven-development as independent process steps. Implementation and domain
skills (systematic-debugging, charm-tui, language reviewers, frontend-design)
remain fully available._

A **category rule**, not a blacklist — a blacklist rots as superpowers adds
skills and leaves e.g. `test-driven-development` mandate-compulsory. This is
secondary hardening; B is the primary binder.

Touches: `WORKER_PROTOCOL.md`.

### E — Revision gate as a plan-critic rubric (critic)

Define "blocking" at the critic, not as a workflow-side filter on an undefined
field: _`verdict: revise` REQUIRES ≥1 `blocking[]` finding. Blocking = would fail
an acceptance criterion, break the build, or touch files outside scope.
Everything else → `accept` with non-blocking notes._ Non-blocking notes are
passed to the implementer to apply at discretion, with no re-critique round.

This removes the circular "revise-with-empty-blocking" state and the severity-
inflation loophole, and makes the second critic round fire only on real blockers.

Touches: `agents/plan-critic.md`, `spec-plan-critic/SKILL.md`.

### F — Skip audit + review compensation (worker)

When the plan phase was skipped: (i) stamp `plan: task-doc (provided/self-gate)`
into the PR body for auditability, and (ii) add one line to the code-review-gate
prompt — _"the plan phase was skipped; include an explicit approach-sanity check
against the task doc, not just conformance."_ This compensates for the one thing
the skip loses: a reviewer anchored on the diff rarely relitigates a wrong
approach that the (skipped) plan-critic might have caught.

Touches: `WORKER_PROTOCOL.md`.

### G — Tighten the `implement: opus` bar (plan)

Restate the tag's bar in `spec-plan-critic/SKILL.md` with explicit
non-qualifiers: pure CSS / layout / styling, straightforward refactors, and
test-only steps do **not** qualify. The tag stays reserved for subtle
concurrency, security-sensitive, or wide-blast-radius work. This stops the
plan-drafting agent from escalating fiddly-but-bounded steps to opus.

Touches: `spec-plan-critic/SKILL.md`.

## Deferred (not in this change)

- **SessionStart worker-mode injection suppression.** A hook that detects worker
  mode (`WORKER_TASK.md` present) and skips/replaces the superpowers injection is
  the most robust override, but adds a hook + a settings change (breaks the
  read-by-path deploy simplicity). Build only if B+D prove insufficient in a real
  run.
- **Standard-tier metrics.** Outcome metrics are deep-only today (#89), so no
  data stream exists to measure standard-tier planning waste. Extending the
  metrics emit to standard workers is worthwhile but out of scope here; without
  it, any future "let metrics decide" gate cannot fire for standard tier.

## Non-goals

- No tier-rubric rewrite — the tier system is sound; it needed the
  planning-already-done escape valve, not replacement.
- No `crew.sh` change.
- Not stripping superpowers from workers (kills the useful skills too).

## Deployment

Not prompt-artifact-only: A, B, G's mechanism touch `dispatch.sh`, so this
deploys via `nh home switch` (writeShellApplication rebuild), not pure
read-by-path. The pure-prompt artifacts (`WORKER_PROTOCOL.md`,
`DISPATCHER_PROTOCOL.md`, `SKILL.md`, `plan-critic.md`) are read by absolute path
and take effect on merge to the main checkout.

## Verification

- Unit-shaped: `dispatch.sh` stamps `plan:` and varies the launch prompt (assert
  both `provided` and `required` paths via a dry-run / arg-echo).
- Live smoke: dispatch one `standard` + `plan: provided` task with a
  fully-specified doc → worker implements without a plan-critic round; dispatch
  one `plan: required` → worker runs the plan phase. Confirm the PR-body skip
  stamp appears on the former.
- Regression: a `deep` task still runs its spec-critic even with a detailed doc.
