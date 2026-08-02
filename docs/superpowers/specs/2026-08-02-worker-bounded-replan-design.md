# Bounded worker re-plan design

## Goal

Keep deterministic gate repair cheap and uncapped while it remains inside the current plan, but stop a worker whose fixes repeatedly require the plan itself to change. An execute-stage worker may autonomously re-plan at most once. A self-authored `plan: required` plan may use that budget only through a strictly stronger planner; the existing `plan: provided` and legacy execute-time contradiction fallback remains a same-rung exception. Once the budget is spent, another plan-shaped sequence blocks for dispatcher input.

This is a protocol feature. It adds no retry daemon, persisted classifier state, or new crew command.

## Existing contracts to preserve

- The fast deterministic gate remains uncapped for ordinary build, lint, and test convergence.
- The review-to-fix loop retains its separate cap of two.
- Initial plan drafting, critic revisions, and deep false-negative consult recovery happen before execute and do not consume the execute-time recovery budget.
- `plan: provided` and qualifying legacy task docs may re-enter planning once when the repository contradicts the plan at execute time. That skipped-plan fallback runs at the worker's current rung.
- Standard and deep Claude, Codex, and Cursor workers continue to delegate implementation according to the worker-to-execute ladder. The protocol must make issue #1's missing-lower-rung rule explicit: when no lower execute rung exists, implementation may run at the worker's same rung and does not consume the re-plan budget. That permissive implementation fallback is not valid for the new re-plan path.
- Codex and Cursor count fixes made by their execute subagents in `rework_count`; they do not acquire Claude-only critic machinery.
- `WORKER_TASK.md` is generated launch metadata. `dispatch.sh` must stamp authoritative `engine:` and `model:` fields beside the existing `effort:` field so the worker never infers its current planner tuple from prose, aliases, or process inspection.

## Designs considered

### A. Raw target rotation

Normalize each gate failure to a target and trigger after a sequence such as `A → B → C → D`. This has a small interface, but it mistakes healthy fail-fast convergence for plan failure: fixing the first compiler error naturally reveals the next target even when every fix is already authorized by one correct plan step. Disjoint targets alone say nothing about whether the plan changed.

### B. Auditable plan amendments

Use gate targets only to identify movement, then require objective evidence that each fix changes the current self-authored plan. A qualifying record contains the gate target, the exact old plan statement, and one concrete amendment: expanding a named file/module scope, changing a declared invariant or interface, or reopening/reordering a completed step because a declared dependency was wrong. Three consecutive qualifying amendments across distinct plan elements trigger recovery.

This design keeps the policy behind the worker-protocol seam while exposing an auditable record to the dispatcher. It distinguishes a wrong plan from ordinary fail-fast mechanics without adding runtime classifier machinery.

### Decision

Choose design B. It has the deeper interface: the protocol owns classification and recovery policy, while gate runners and `crew rate` remain simple evidence producers/consumers. Design A is rejected because target movement without a plan delta is not evidence of a bad plan.

## Objective classifier

The classifier applies only while executing a worker-authored plan: the plan produced for `plan: required`, the self-authored plan produced by skipped-plan re-entry, or a later accepted replacement. It does not reinterpret an untouched provided/legacy plan; a direct repository contradiction there uses the existing re-entry rule.

The worker keeps an execute-local amendment ledger for the whole scoped deterministic-gate episode, from its first failure until build, lint, unit, and any other discovered subgates are all green. There is no daemon or persisted state. A **gate identity** is the exact discovered command plus its stable subgate name and is recorded as evidence in each row; movement from build to lint to unit does not reset the episode or its qualifying count. A **gate target** is the most specific stable identifier in confirmed deterministic output, in this order: named test/check, package/module, file plus rule, then file. Strip line numbers, timestamps, stack frames, temporary paths, and diagnostic prose. If multiple failures are reported, sort and deduplicate them into a target set.

Before applying a fix, the worker compares the required change with the current plan and records a qualifying amendment only when all of the following are available:

- the gate failure is confirmed by rerunning the same gate against the same tree, or by output that deterministically identifies the failure without a flakiness rerun;
- the stable gate target is known;
- an exact existing plan statement can be quoted; and
- the required fix forces one of three concrete plan deltas:
  1. expand or cross a named file, module, or component scope;
  2. change a declared invariant or interface; or
  3. reopen or reorder a completed plan step because a declared dependency or ordering statement was wrong.

Each ledger row is:

```text
gate: <command/subgate>
target: <normalized target set>
old_plan: <exact quoted boundary, invariant/interface, or dependency/step statement>
amendment: <replacement statement and one of scope|invariant|dependency>
```

For the first two qualifying rows, the worker amends the working plan before making the fix. On the third consecutive qualifying row, it records the proposed amendment but does not apply it or fix the target; recovery owns the replacement plan. A missing quote, merely implicit expectation, or prose judgment such as "the plan feels wrong" is unclassifiable and cannot count.

### Deterministic decision table

| Observation after a confirmed failure                                                                                                             | Classification                     | Consecutive qualifying count                                |
| ------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------- | ----------------------------------------------------------- |
| Required fix stays within the named scope and preserves declared interfaces, invariants, dependencies, and step order                             | Mechanical/fail-fast convergence   | Reset to `0`; repair remains uncapped                       |
| Same target or an overlapping target set recurs, even if the repair text changes                                                                  | Same-step convergence              | Reset to `0`; repair remains uncapped                       |
| No stable target or no exact old plan statement can be recorded                                                                                   | Unclassifiable                     | Reset to `0`; do not guess                                  |
| One command/subgate passes, gate/subgate identity changes, or the tree is rerun unchanged only to probe flakiness                                 | No amendment row                   | Preserve the current count                                  |
| The entire scoped deterministic gate is green                                                                                                     | Episode complete                   | Reset to `0` and discard the episode counter                |
| Fix requires one allowed amendment category, and target and quoted plan element are both distinct from the immediately previous qualifying row    | Plan-shaped amendment              | Increment by `1` and append the auditable row               |
| Fix requires an amendment but target or quoted plan element overlaps the previous row                                                             | Continued work on one plan element | Reset to `0`; append for context but do not count           |
| Three consecutive plan-shaped amendments in one deterministic-gate episode, across three distinct targets and three distinct quoted plan elements | Plan-shaped circling               | Trigger the execute-time recovery rule before the third fix |

Examples:

- Healthy fail-fast `A → B → C → D` never triggers when all four fixes are already authorized by one plan step and remain within its named scope. Each observation is mechanical and leaves the count at zero.
- Repeated `A → A → A …` remains uncapped because the target overlaps and the current plan element remains valid.
- `A(scope step 2) → B(interface step 4) → C(dependency step 6)` triggers before fixing `C` when each failure forces the recorded amendment shown in parentheses.
- `build:A(scope step 2) → lint:B(interface step 4) → unit:C(dependency step 6)` also triggers; passing build and moving between subgates preserves the episode-wide count.
- `A(scope step 2) → B(scope step 2) → C(interface step 4)` does not trigger: the overlap on one plan element resets the sequence.
- An actual intervening mechanical failure or unclassifiable tool failure resets the sequence. Merely passing one subgate does not; only a wholly green scoped deterministic gate ends the episode.

The threshold is three consecutive auditable amendments. One amendment can be ordinary repository discovery; two can expose one shallow dependency mistake across adjacent elements. A third distinct plan delta shows that execution is repeatedly redesigning the plan. The current ratings interface stores only aggregate `rework_count`, not amendment histories, so it cannot justify a data-derived threshold. `replanned` makes the chosen threshold measurable going forward.

## One execute-time recovery budget

The worker initializes `replan_used = false` on entry to execute. Initial planning and its bounded critic/consult revisions do not alter it.

Three superficially similar transitions have different contracts:

| Transition                                         | Trigger                                                                                                                | Rung                                                        | Execute-time re-plan budget                                |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------- | ---------------------------------------------------------- |
| Missing lower execute rung (issue #1 prerequisite) | The implementation ladder has no lower model/intensity available                                                       | Implement at the worker's same rung                         | Not consumed; this is implementation, not planning         |
| Skipped-plan repository contradiction              | `plan: provided` or extracted legacy plan names a missing file or an approach the repository disproves at execute time | Re-enter planning once at the current worker rung           | Consumed; set `replan_used = true` when the episode begins |
| Plan-shaped gate circling                          | Three consecutive qualifying amendments in one deterministic-gate episode                                              | Planning-only context at exactly one declared rung stronger | Consumed; set `replan_used = true` when the launch begins  |

The same-rung skipped-plan re-entry and strict-upward plan-shaped recovery share one execute-time budget. A worker can therefore never perform both autonomously in one execute phase. The missing-lower-rung implementation fallback is independent and never changes `replan_used` or `replanned`.

If the amendment trigger fires when `replan_used` is already true, the worker does not plan again. It reports `blocked`. This is the hard post-replan cap. The classifier still requires three qualifying amendments after a re-plan, so one ordinary repository discovery does not abort a healthy continuation; a second complete plan-amendment sequence cannot loop back into planning.

Same-target mechanical repair remains uncapped before and after re-planning. The finite guarantee applies to plan-shaped movement, not every gate retry.

## Authoritative launch metadata and stronger planner rungs

`dispatch.sh` stamps these fields into every generated `WORKER_TASK.md`:

```yaml
engine: <claude|codex|cursor>
model: <exact dispatched model id or alias>
effort: <exact dispatched effort>
```

They are authoritative for recovery. `tests/dispatch.bats` pins all three fields for each engine path and ensures model aliases/ids are copied exactly. Existing issue #2 validation proves only that the original model slug has the right engine-specific shape; it does not validate an arbitrary tuple constructed later. Recovery therefore launches only the finite canonical rungs below. Unknown models, unlisted suffixes, unsupported tuples, or unavailable planning tools block instead of guessing.

Recovery launches one fresh planning-only context. It never changes engines, skips a rung, or silently reuses the current planning tuple.

| Engine | Supported planning-only launch                                                                                                                                                                                                                                                                                      | Top/no-rung behavior                                                                                                                                                                                                      |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Claude | Use the Agent tool's model-only override: `haiku → sonnet → opus → fable` where haiku applies. The recovery cannot override effort, so `effort:` is metadata only for this engine. The existing fable eligibility rule remains: `opus → fable` requires a genuinely hard, well-specified task.                      | `fable`, an ineligible `opus → fable`, an unknown full Claude id, or unavailable Agent model launch blocks. Claude may run its existing critic workflow inside the single episode; its internal revision cap remains two. |
| Codex  | Use a native planning subagent. On the same exact model, raise effort one step: `low → medium → high → xhigh → max`. At `max`, move one canonical family step `gpt-5.6-luna → gpt-5.6-terra → gpt-5.6-sol`, preserving `max`. Never use `ultra`, because it adds automatic delegation and would nest orchestration. | `gpt-5.6-sol/max`, a legacy/unknown family, an unavailable native launch, or any tuple outside this table blocks. Codex performs plain replanning, not Claude's `spec-plan-critic`.                                       |
| Cursor | Use a Task-tool model-only override on the canonical Grok sequence `cursor-grok-4.5-low-fast → cursor-grok-4.5-medium-fast → cursor-grok-4.5-high`. Cursor has no separate effort launch parameter.                                                                                                                 | `cursor-grok-4.5-high`, `kimi-k3-high`, Composer 2.5/2.5-fast (no intensity axis), cross-vendor Claude/GPT ids, unknown ids, or an unavailable Task model launch block. Cursor performs plain replanning.                 |

This strict rule intentionally differs from issue #1's implementation fallback. Using the worker's same rung when no lower execute rung exists preserves implementation availability; using the same planner after its plan has demonstrated circling supplies no stronger reasoning. A missing upward planning rung is therefore a blocker.

## Recovery-plan contract

One recovery **episode** may contain Claude's already-bounded critic revisions, but it produces at most one replacement plan. Codex and Cursor get one fresh plain-plan attempt.

A replacement is viable only if it:

- explicitly accounts for every target, quoted old plan statement, and required amendment in the three-row triggering ledger;
- names the files or components it may change;
- gives a finite ordered implementation step list and the deterministic commands that validate it; and
- exposes no unresolved choice that would require the worker to improvise during execute.

Claude additionally requires the normal workflow's accepted result. Codex and Cursor validate the four structural properties by extraction, matching the existing legacy-plan self-gate style. Refusal, timeout, unavailable higher-rung launch, exhausted Claude critic episode, failed extraction, or an unresolved choice all report `blocked`; none falls back to the original plan or another planner.

After adopting a viable replacement, clear the consecutive amendment counters, preserve the ledger for dispatcher evidence, preserve `replan_used = true`, checkpoint-peek, and execute the replacement. Clearing counters prevents pre-plan evidence from immediately tripping the post-plan detector; preserving the budget enforces the cap.

## Blocked and dispatcher behavior

The worker uses the existing bus contract:

```text
crew status worker:<branch> blocked "plan-shaped gate rework: <reason>"
crew msg worker:<branch> dispatcher:<crew_id> "<question and evidence>"
crew await worker:<branch> --timeout 300
```

The message includes the gate identity, the three amendment-ledger rows, fixes already attempted, authoritative `engine`/`model`/`effort`, whether the budget was already spent, and either the missing canonical higher rung or the failed viability condition. It asks the dispatcher for a concrete replacement plan or an explicit supported higher planner rung; it does not ask a generic "what now?".

The existing await, straggler-fold, two-timeout, and deep/security no-default rules remain unchanged. A dispatcher-supplied replacement plan is external direction, not another autonomous re-plan; adopting it sets `replanned = true` and retains the post-replan cap. A reply that merely requests the same autonomous planning attempt again is insufficient under the strict-upward rule.

## Metrics and ratings

Every outcome-metrics body adds required boolean `"replanned": <true|false>` beside `rework_count`.

- `false`: no execute-time replacement-planning episode began and no dispatcher-supplied replacement was adopted. This includes a top/no-rung block before launch and the missing-lower-rung implementation fallback.
- `true`: a same-rung skipped-plan re-entry or strict-upward recovery episode actually began, even if it later failed viability; it is also true when a dispatcher-supplied replacement plan was adopted.
- Initial plan drafts, plan-critic revisions, and deep false-negative consult recovery do not set it; those happen before execute and are already represented by planning metrics.
- `rework_count` continues to count gate-forced execute fixes and is not incremented merely for invoking a planner.

The worker emits one complete metrics snapshot immediately before every stopping path: `done`, terminal `failed`, dispatcher-requested or permission-caused stop, and stop-after-block-timeout (including the final failed state after the existing second timeout). It does not emit merely because it temporarily stamps `blocked` and continues waiting. If a stopped blocked run later resumes, it emits another complete snapshot at its next stop; `crew rate` already sorts metrics messages by timestamp and consumes the latest one, so the later snapshot supersedes the earlier state without an update command.

`crew rate` copies the field into each ratings record. It must preserve boolean `false`; the jq projection must not use `$m.replanned // null`, because jq's alternative operator converts `false` to the fallback. A missing metrics record or a legacy metrics body without the field projects `null`, while explicit `false` remains `false`.

`dispatch-orchestration.md` updates its example metrics record and documents the new interpretation and stop-path snapshot rule so ratings consumers read `rework_count` together with `replanned`.

## Canonical and generated files

Implementation changes are limited to:

- `adapters/core/protocols/WORKER_PROTOCOL.md`: issue #1 same-rung implementation fallback, classifier, recovery budget, engine rung rules, blocked behavior, and all-stop metric definition;
- `adapters/core/protocols/dispatch-orchestration.md`: canonical rung/metrics cross-reference;
- `adapters/core/dispatch.sh`: stamp authoritative `engine:` and `model:` launch metadata beside `effort:`;
- `adapters/core/crew.sh`: add the ratings projection only;
- generated Claude and Codex worker-protocol projections produced by `scripts/gen-adapters.sh`;
- `tests/crew.bats`, `tests/adapters.bats`, and `tests/dispatch.bats`: focused ratings, projection, and task-metadata coverage.

Do not hand-edit generated projections. Do not add shell classifier state: engines execute this policy from the canonical worker protocol. The launcher change only supplies authoritative metadata.

## Focused verification

### `tests/crew.bats`

Add one table-style ratings test covering three runs:

- metrics with `replanned: true` projects `true`;
- metrics with `replanned: false` projects `false` rather than `null`;
- a legacy metrics body without `replanned` projects `null`.

Keep the existing `rework_count` assertions in the same fixture so the test proves both fields coexist. No classifier behavior belongs in `crew.sh` tests.

### `tests/adapters.bats`

After generation, assert that each shipped Claude and Codex worker protocol matches the canonical worker protocol and contains the load-bearing classifier, top/no-rung `replanned: false`, shared-budget, and stop-path snapshot language. This is the strongest repository-level classifier test because the classifier is intentionally protocol-driven. It pins source authority and projection consistency without inventing runtime machinery. `dispatch.sh` is shared and tested directly in `tests/dispatch.bats`; there are no generated launcher-script copies. Cursor consumes the canonical command/protocol path rather than a generated worker-protocol copy, so do not invent a Cursor projection.

### `tests/dispatch.bats`

For Claude, Codex, and Cursor launch fixtures, assert that generated `WORKER_TASK.md` contains the exact dispatched `engine:` and `model:` plus the existing `effort:`. Include at least one alias/model-id case per engine. Keep issue #2's slug-shape rejection tests unchanged: task metadata is stamped only after the original dispatch tuple passes that existing gate.

The top-rung behavior remains protocol content coverage rather than a fake launcher execution of an agent. Legacy ratings compatibility is the missing-`replanned` → `null` case in `tests/crew.bats`.

### Commands

Run the adapter generator, the three focused Bats files, the repository's complete applicable test target, and `shellcheck adapters/core/crew.sh adapters/core/dispatch.sh`. Generated drift must be clean after a second generator run.

## Acceptance traceability

| Acceptance criterion                                   | Design element                                                                                                                                                                                  | Verification                                         |
| ------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| Engine-general objective trigger                       | Three consecutive amendment-ledger rows, each with stable target, exact old plan statement, and allowed concrete delta across distinct plan elements                                            | Canonical protocol/projection content assertions     |
| At most one re-plan and finite continuation            | Shared execute-time budget; second trigger blocks                                                                                                                                               | Protocol state rules and generated equality          |
| Mechanical and fail-fast convergence remains uncapped  | In-plan `A → B → C → D`, overlaps, same target, and actual intervening mechanical/unclassifiable failures reset; build/lint/unit movement carries qualifying rows until the whole gate is green | Protocol content assertion; no shell retry cap       |
| Three fallback/recovery transitions remain distinct    | Missing lower execute rung uses same-rung implementation/no budget; skipped-plan contradiction uses same-rung re-entry/budget; circling requires higher-rung planning                           | Canonical and generated protocol assertions          |
| Engine tuple is authoritative and launchable           | `engine`/`model`/`effort` task metadata plus finite Claude/Codex/Cursor planning-only tables                                                                                                    | `tests/dispatch.bats`; protocol projection assertion |
| Top/no-rung terminates usefully                        | Unknown/unsupported/current-top tuple blocks with ledger and tuple evidence; no launch means `replanned: false`                                                                                 | Protocol projection assertion                        |
| `replanned` emitted and documented with `rework_count` | Required all-stop snapshot and latest-message semantics                                                                                                                                         | True/false/legacy-null `crew rate` test              |
| Canonical and generated artifacts agree                | Generator-owned Claude/Codex protocol copies only                                                                                                                                               | `tests/adapters.bats` equality check and idempotence |
| Protocol-driven scope                                  | Amendment ledger lives in worker reasoning; shell changes only stamp metadata and project metrics                                                                                               | Diff review and shellcheck                           |
