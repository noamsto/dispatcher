# Worker bounded re-plan — Implementation Plan

> **For agentic workers:** Execute this plan task-by-task. Keep the classifier in
> protocol prose; do not add runtime state or a new crew command. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve uncapped mechanical fast-gate convergence while objectively
detecting three consecutive plan amendments, spending at most one shared
execute-time re-plan budget, and reporting whether a run replanned.

**Architecture:** `WORKER_PROTOCOL.md` owns the execute-local amendment ledger,
the adjacent-movement classifier, the shared budget, per-engine higher-planner
ladders, and stopping-path metrics. `dispatch.sh` only stamps the authoritative
engine/model tuple into `WORKER_TASK.md`; `crew.sh` only projects the new metrics
boolean. `dispatch-orchestration.md` documents the same ladders and ratings
contract. The existing generator copies all canonical protocols into the Claude
and Codex plugin trees; Cursor reads the canonical protocols and has no generated
worker-protocol projection.

**Tech Stack:** Markdown protocols, Bash 5, jq, Bats, `scripts/gen-adapters.sh`,
shellcheck, Nix flake checks.

**Planning context:** No `DECOMPOSITION.md` exists, and the orchestration survey
did not justify a consultant. This plan follows the accepted design and current
repository seams directly.

## Design twice

1. **Runtime retry/classifier machinery:** add shell state around gate commands
   and persist amendment counts. Rejected: the shell cannot objectively know
   whether a fix changed a quoted plan boundary, and this would turn a protocol
   feature into a new runtime subsystem.
2. **Protocol-owned auditable ledger:** require the worker to record a stable
   gate target, exact old plan statement, and concrete amendment category; use
   shell only for immutable launch metadata and ratings projection. Chosen: it
   keeps classification at the seam that can inspect both plan and fix while
   making every trigger explainable to the dispatcher.

For generated coverage, compare all canonical core protocols to both shipped
trees rather than naming only one projection. This matches what
`scripts/gen-adapters.sh` actually copies and makes later canonical protocol
edits inherit the same drift protection.

## Global constraints

- Do not edit `WORKER_TASK.md` or the accepted design spec. They are inputs.
- Do not hand-edit anything under
  `adapters/claude-code/plugin/protocols/` or
  `adapters/codex/plugin/protocols/`; run `scripts/gen-adapters.sh` after editing
  canonical protocols.
- Do not create runtime classifier state, persisted amendment history, a retry
  daemon, or a new `crew` subcommand.
- Keep ordinary in-plan, same-target, overlapping-target, and unclassifiable
  repairs uncapped. Do not add a raw retry-count ceiling.
- The first qualifying amendment after initialization/reset seeds count `1`;
  each later row increments only on target-and-plan-element movement from the
  immediately previous qualifying row. It does not require three globally
  distinct targets or plan elements.
- Initial planning, critic revisions, and deep false-negative consult recovery
  happen before execute and do not spend the execute-time re-plan budget.
- The skipped-plan contradiction fallback and plan-shaped recovery share one
  execute-time budget. The issue-#1 same-rung implementation fallback is not
  planning and does not spend it.
- Use `$m | has("replanned")` when projecting the metric. Never use
  `$m.replanned // null`, because jq treats explicit `false` as a request for the
  fallback.
- Match the surrounding shell style. Run shellcheck after each shell edit.
- No task needs a high-risk `implement:` tag. The two shell changes are small,
  pinned data plumbing; the policy remains protocol text.

## Escalations carried

1. **"The classifier leaves a two-element amendment cycle unbounded."**
   Correct the protocol so the first qualifying row after initialization/reset
   seeds count `1`; each later qualifying row increments only when it moves away
   from the immediately preceding target and quoted plan element. A globally
   earlier element may recur: the auditable sequence
   `A(scope step 2) → B(interface step 4) → A(scope step 2)` reaches counts
   `1 → 2 → 3` and triggers before the third fix. Pin initialization and that
   example as protocol assertions.
2. **"The generated-artifact scope does not match the actual generator."**
   The generator copies the complete canonical protocol directory into both the
   Claude and Codex plugin trees. Compare every canonical core protocol (which
   includes both changed protocols) to both shipped trees, and include both
   generated protocol directories in generator-idempotence checks.

## File structure

| File                                                              | Responsibility                                                        | Task |
| ----------------------------------------------------------------- | --------------------------------------------------------------------- | ---- |
| `tests/crew.bats`                                                 | True/false/legacy-null ratings fixture                                | 1    |
| `adapters/core/crew.sh`                                           | Preserve and project `replanned`                                      | 1    |
| `tests/dispatch.bats`                                             | Exact engine/model/effort task metadata for all engines               | 2    |
| `adapters/core/dispatch.sh`                                       | Stamp authoritative worker tuple                                      | 2    |
| `tests/adapters.bats`                                             | Protocol semantics, source/projection equality, generator idempotence | 3    |
| `adapters/core/protocols/WORKER_PROTOCOL.md`                      | Classifier, budget, recovery, blocking, metrics                       | 3    |
| `adapters/core/protocols/dispatch-orchestration.md`               | Planner ladders and ratings interpretation                            | 3    |
| `adapters/claude-code/plugin/protocols/WORKER_PROTOCOL.md`        | Generated projection                                                  | 3    |
| `adapters/claude-code/plugin/protocols/dispatch-orchestration.md` | Generated projection                                                  | 3    |
| `adapters/codex/plugin/protocols/WORKER_PROTOCOL.md`              | Generated projection                                                  | 3    |
| `adapters/codex/plugin/protocols/dispatch-orchestration.md`       | Generated projection                                                  | 3    |

---

### Task 1: Preserve `replanned` in ratings

**Files:**

- Modify: `tests/crew.bats` — append a focused `rate` fixture
- Modify: `adapters/core/crew.sh:539-559` — ratings object projection

**Interface:** The latest metrics body in a run projects explicit boolean
`replanned`; a missing metrics body or legacy body without the key projects
JSON `null`. Existing `rework_count` behavior is unchanged.

- [ ] **Step 1: Add a three-run ratings test**

Create one table-style Bats test that writes three dispatch/status/metrics runs
to the repo's `crew/events.jsonl`, points `XDG_DATA_HOME` at
`$BATS_TEST_TMPDIR/data`, runs `crew rate`, and reads
`$XDG_DATA_HOME/crew/ratings.jsonl`:

| Branch               | Metrics body                           | Expected `replanned` | Expected `rework_count` |
| -------------------- | -------------------------------------- | -------------------- | ----------------------- |
| `feat/replan-true`   | `{"replanned":true,"rework_count":3}`  | `true`               | `3`                     |
| `feat/replan-false`  | `{"replanned":false,"rework_count":1}` | `false`              | `1`                     |
| `feat/replan-legacy` | `{"rework_count":2}`                   | `null`               | `2`                     |

Use fixed, increasing timestamps and one terminal `done` status per branch so
the runs are reported and easy to query by `.branch`. Assert all three records
in one jq expression, including `has("replanned")`, so a missing output key does
not accidentally compare like null.

- [ ] **Step 2: Run the focused test and observe red**

```bash
bats tests/crew.bats
```

Expected: the new test fails because ratings records do not contain
`replanned`; existing tests remain green.

- [ ] **Step 3: Add the null-safe boolean projection**

In the ratings object beside `rework_count`, add:

```jq
replanned: (
  if $m == null or (($m | has("replanned")) | not)
  then null
  else $m.replanned
  end
),
```

Do not change metrics parsing, latest-message selection, outcome folding, or any
other ratings field.

- [ ] **Step 4: Run the focused gate**

```bash
bats tests/crew.bats
shellcheck adapters/core/crew.sh
```

Expected: all `crew.bats` tests pass, explicit `false` survives as false, and
shellcheck is silent.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/crew.sh tests/crew.bats
git commit -m "feat(crew): project bounded replan outcomes"
```

---

### Task 2: Stamp authoritative planner metadata

**Files:**

- Modify: `tests/dispatch.bats` — generated task metadata coverage
- Modify: `adapters/core/dispatch.sh:314-323` — `WORKER_TASK.md` header

**Interface:** Every dispatched worker receives exact `engine:`, `model:`, and
`effort:` fields. Values are copied from the already-validated launch tuple;
there is no second alias resolution or inferred model family.

- [ ] **Step 1: Add the failing launch-metadata test**

Using the existing `stub_launch_bins`, dispatch three uniquely titled issue-42
workers and inspect the corresponding files under
`$TEST_REPO/.dispatch-wt/`:

| Engine   | Model           | Effort   | Exact header assertions                                 |
| -------- | --------------- | -------- | ------------------------------------------------------- |
| `claude` | `sonnet`        | `medium` | `engine: claude`, `model: sonnet`, `effort: medium`     |
| `codex`  | `gpt-5.6-terra` | `high`   | `engine: codex`, `model: gpt-5.6-terra`, `effort: high` |
| `cursor` | `composer-2.5`  | `low`    | `engine: cursor`, `model: composer-2.5`, `effort: low`  |

Set `DISPATCH_PROFILE=work` for the rows so all engines reach launch. Use
distinct titles/branches because the stub creates real git worktrees. Assert
each field with `grep -Fx`, which proves aliases and full ids are copied byte for
byte rather than normalized.

- [ ] **Step 2: Run the focused test and observe red**

```bash
bats tests/dispatch.bats
```

Expected: each new row finds the existing effort line but fails on missing
engine/model lines. Existing model-gate rejection tests remain unchanged.

- [ ] **Step 3: Extend the task header**

Change the task-file `printf` so its stable header begins:

```text
tier: <tier>
engine: <agent>
model: <model>
effort: <effort>
plan: <provided|required>
```

Pass `"$agent"` and `"$model"` as ordinary format arguments. Keep the closes
line, dispatcher pane, crew directory/id, agent name, and optional task body
unchanged.

- [ ] **Step 4: Run the focused gate**

```bash
bats tests/dispatch.bats
shellcheck adapters/core/dispatch.sh
```

Expected: all dispatch tests pass for Claude, Codex, and Cursor; shellcheck is
silent.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/dispatch.sh tests/dispatch.bats
git commit -m "feat(dispatch): stamp worker engine and model"
```

---

### Task 3: Define and project the bounded re-plan protocol

**Files:**

- Modify: `tests/adapters.bats:6-25` — idempotence paths
- Modify: `tests/adapters.bats` — canonical/shipped protocol and semantic checks
- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md`
- Modify: `adapters/core/protocols/dispatch-orchestration.md`
- Generate: the four changed Claude/Codex protocol projections listed in the
  file-structure table

**Interface:** The worker initializes run-wide `replanned = false` before its
startup bus drain, then maintains execute-local `replan_used` and the consecutive
amendment count in its reasoning, not in shell. A trigger either launches one
permitted planning-only context or blocks with auditable evidence. Generated
protocols are exact copies of the canonical source.

- [ ] **Step 1: Expand generator idempotence coverage**

Add these paths to `gen_paths` in the existing idempotence test:

```bash
adapters/claude-code/plugin/protocols
adapters/codex/plugin/protocols
```

This checksum comparison now covers everything the generator rewrites: command
projections, skills, scripts, and the complete protocol trees.

- [ ] **Step 2: Add canonical/projection and semantic assertions**

Add one test that loops over every `adapters/core/protocols/*.md` file and uses
`cmp -s` to compare it with the same basename under both:

```text
adapters/claude-code/plugin/protocols/
adapters/codex/plugin/protocols/
```

Add a second focused test against canonical `WORKER_PROTOCOL.md` for stable,
load-bearing statements covering:

- `replanned = false` is initialized in **First action** before the startup bus
  drain, and a pre-execute stop emits a complete metrics snapshot with false;
- after initialization or any reset, the first qualifying amendment seeds the
  consecutive count at `1`;
- the adjacent-movement example
  `A(scope step 2) → B(interface step 4) → A(scope step 2)` triggers before
  the third fix;
- the skipped-plan contradiction and plan-shaped recovery share exactly one
  execute-time budget;
- a missing lower execute rung permits same-rung implementation without
  consuming that budget;
- strict higher-rung recovery and top/no-rung blocking leave `replanned` false
  when no planning episode launches;
- the worker emits a complete metrics snapshot immediately before every
  stopping path, including paths before execute begins.

Use `grep -F` for exact sentinel sentences, not a broad keyword count. Equality
to both shipped trees then proves the same assertions ship without duplicating
the prose checks six times.

- [ ] **Step 3: Run the adapter test and observe red**

```bash
bats tests/adapters.bats
```

Expected: the semantic test fails because the bounded-replan contract and
two-element oscillation example do not exist yet. Existing generated files are
still equal to canonical at this point.

- [ ] **Step 4: Update task metadata authority and execute initialization**

In `WORKER_PROTOCOL.md`:

- extend **First action** to read authoritative `engine:`, `model:`, and
  `effort:` from `WORKER_TASK.md` alongside the existing fields; forbid planner
  recovery from inferring the tuple from prose, aliases, or process inspection;
- in **First action**, initialize run-wide `replanned = false` before the
  startup bus drain and therefore before the first possible dispatcher-directed
  stop; this value is available to every pre-execute stopping path;
- only on entry to execute, initialize `replan_used = false`; explicitly exclude
  initial planning, critic revisions, and deep consult false-negative recovery
  from the execute-time budget;
- make the existing provided/legacy execute-time contradiction re-entry consume
  the shared budget at the current rung, setting both booleans true when the
  planning episode begins. If the budget is already spent, block instead of
  re-entering.

- [ ] **Step 5: Add the objective whole-episode classifier**

In or directly after **Fast deterministic gate**, define:

1. The episode starts on the first scoped deterministic-gate failure and lasts
   until all discovered build/lint/unit/other subgates are green. Moving between
   commands/subgates preserves the count; an entirely green scoped gate ends
   and discards it.
2. A gate identity is exact command plus stable subgate name. A target is the
   most specific stable deterministic identifier (named test/check, module or
   package, file+rule, then file), normalized by stripping volatile diagnostics
   and sorting/deduplicating target sets.
3. Before fixing, a qualifying row requires confirmed deterministic failure, a
   stable target, an exact quoted old plan statement, and one amendment:
   `scope`, `invariant/interface`, or `dependency/order`. Record:

   ```text
   gate: <command/subgate>
   target: <normalized target set>
   old_plan: <exact quoted plan statement>
   amendment: <replacement statement and scope|invariant|dependency>
   ```

4. After initialization or any reset, the next qualifying amendment seeds the
   consecutive count at `1` and becomes the immediately previous qualifying row.
   It does not need a predecessor against which to prove movement.
5. Mechanical fixes, same/overlapping adjacent targets, overlap with the
   immediately previous quoted plan element, unclassifiable failures, or an
   actual intervening mechanical/unclassifiable observation reset the
   consecutive count to zero. A subgate pass or unchanged flakiness probe
   preserves it but adds no row.
6. Each later qualifying row increments only when both its target and quoted
   plan element differ from the **immediately previous qualifying row**. If
   either overlaps, reset according to the decision table rather than increment.
   Do not require global uniqueness. Therefore a seeded `A` row has count `1`,
   `B` increments it to `2`, and a later `A` differs from adjacent `B`, increments
   to `3`, and triggers; `A → A` resets.
7. Apply/amend/fix the first two rows. On the third, record the proposed row but
   do not amend or fix; transfer control to recovery. Include healthy in-plan
   `A → B → C`, same-target `A → A`, cross-subgate, overlapping-plan,
   and two-element oscillation examples.

The classifier applies to worker-authored plans: `plan: required`, the plan
created by skipped-plan re-entry, and accepted replacements. An untouched
provided/legacy plan uses the direct contradiction fallback instead.

- [ ] **Step 6: Define the one shared recovery budget**

Add the three-way transition table:

| Transition                    | Rung                               | Budget                              |
| ----------------------------- | ---------------------------------- | ----------------------------------- |
| Missing lower execute rung    | Same-rung implementation           | Not consumed; `replanned` unchanged |
| Provided/legacy contradiction | Same-rung planning re-entry        | Consume at episode start            |
| Three qualifying amendments   | Exactly one stronger planning rung | Consume at launch start             |

State that a worker may autonomously take only one of the two planning
transitions. After a replacement, clear only the consecutive counter, preserve
the ledger and `replan_used = true`, checkpoint-peek, then execute. A later full
three-row sequence blocks. Mechanical convergence remains uncapped both before
and after replanning.

In rule 1, make issue #1's floor explicit: if the execute ladder has no lower
model/intensity, implement at the worker's current rung. This is implementation,
not planning, and never changes `replan_used` or `replanned`. Do not reuse this
fallback for a plan-shaped recovery.

- [ ] **Step 7: Add strict per-engine higher-planner ladders**

Use only one fresh planning-only context, never change engines, skip a rung, or
guess an unlisted tuple:

- **Claude:** Agent model override
  `haiku → sonnet → opus → fable`; `opus → fable` retains the existing
  hard/well-specified eligibility check. Fable, ineligible opus, unknown full
  ids, and unavailable launches block. Effort is metadata because the Agent
  override cannot change it. Claude may use its existing bounded critic within
  this single episode.
- **Codex:** on the exact model, increase
  `low → medium → high → xhigh → max`; at max move one family
  `gpt-5.6-luna → gpt-5.6-terra → gpt-5.6-sol`, preserving max. Never use
  ultra. Sol/max, legacy/unknown families, outside-table tuples, and unavailable
  native planning launches block. Use plain replanning, not Claude critics.
- **Cursor:** Task model override
  `cursor-grok-4.5-low-fast → cursor-grok-4.5-medium-fast → cursor-grok-4.5-high`.
  High, Kimi, Composer, cross-vendor ids, unknown ids, and unavailable Task
  launches block. Use plain replanning.

Define a viable replacement as one that accounts for all three ledger rows,
names allowed files/components, gives a finite ordered implementation list plus
deterministic validation commands, and leaves no choice for execute-time
improvisation. Refusal, timeout, failed extraction/critic, unavailable launch,
or non-viable output blocks without falling back to the original plan or a
second planner.

- [ ] **Step 8: Define blocked evidence and stopping-path metrics**

Extend the existing bus contract without adding commands. A plan-shaped block
must post:

```text
crew status worker:<branch> blocked "plan-shaped gate rework: <reason>"
crew msg worker:<branch> dispatcher:<crew_id> "<concrete question and evidence>"
crew await worker:<branch> --timeout 300
```

The message includes gate identity, all three ledger rows, fixes attempted,
authoritative engine/model/effort, budget state, and the missing rung or failed
viability condition. Ask for a concrete replacement or supported higher rung.
Keep existing straggler-fold and two-timeout rules. A dispatcher-supplied
replacement is external direction, sets `replanned = true`, and retains the
post-replan cap.

Add required boolean `"replanned": <true|false>` beside `rework_count` in the
metrics example and definitions:

- initialize it to false before the startup bus drain, so plan/spec/consult
  failures and dispatcher stops before execute still have a defined value;
- false when no execute-time planning episode began and no dispatcher plan was
  adopted, including same-rung implementation fallback and top/no-rung block;
- true when same-rung re-entry or strict-upward planning actually began, even if
  viability later failed, or when a dispatcher replacement was adopted;
- unchanged by initial planning, critics, consult recovery, or planner invocation
  alone in `rework_count`.

Require one complete latest-state snapshot immediately before every stopping
path, whether it occurs before or after execute: startup-drain dispatcher stop,
spec/plan/consult terminal failure, done, terminal gate failure,
dispatcher-requested stop, permission stop, first blocked timeout stop, and
final failed stop after the second timeout. Every pre-execute snapshot emits
`replanned: false`. Do not emit for a temporary blocked state that continues
awaiting. A resumed run emits a newer snapshot; `crew rate` already selects the
latest timestamp.

- [ ] **Step 9: Synchronize `dispatch-orchestration.md`**

Update its worker/session model section and metrics example to document:

- the separate same-rung execute fallback versus strict-upward planner ladders;
- the finite Claude/Codex/Cursor recovery tuples and top/no-rung blocking;
- the shared execute-time budget;
- `replanned` read together with `rework_count`;
- all-stopping-path snapshots, latest-message semantics, and legacy null.

Keep this a cross-reference/consumer document; the full ledger algorithm stays
in `WORKER_PROTOCOL.md`.

- [ ] **Step 10: Confirm canonical edits make projections red, then generate**

```bash
bats tests/adapters.bats
```

Expected now: semantic assertions pass against canonical, but the all-protocol
`cmp` test fails because both shipped trees still contain the old generated
copies.

```bash
./scripts/gen-adapters.sh
bats tests/adapters.bats
```

Expected: generator reports `adapters regenerated`; idempotence, exact equality
for every core protocol in both trees, and bounded-replan sentinels all pass.

- [ ] **Step 11: Commit**

```bash
git add adapters/core/protocols/WORKER_PROTOCOL.md \
  adapters/core/protocols/dispatch-orchestration.md \
  adapters/claude-code/plugin/protocols \
  adapters/codex/plugin/protocols \
  tests/adapters.bats
git commit -m "feat(protocols): bound plan-shaped worker replanning"
```

---

### Task 4: Full verification and acceptance audit

**Files:** Review only; no new implementation scope.

- [ ] **Step 1: Regenerate twice and run focused tests**

```bash
./scripts/gen-adapters.sh
./scripts/gen-adapters.sh
bats tests/crew.bats tests/dispatch.bats tests/adapters.bats
```

Expected: both generations complete without content drift; all focused tests
pass.

- [ ] **Step 2: Run every shell and repository gate**

```bash
shellcheck adapters/core/*.sh scripts/*.sh
bats tests/
nix flake check
git diff --check
```

Expected: shellcheck is silent, the complete Bats suite and flake checks pass,
and git reports no whitespace errors.

- [ ] **Step 3: Audit generated scope and forbidden machinery**

```bash
git diff --name-only origin/feat/2-dispatch-validate-the-model-slot-against...HEAD
rg -n "replan_used|replanned|amendment" adapters/core/crew.sh adapters/core/dispatch.sh
```

Expected file scope is the two core shell files, two canonical protocols, three
tests, and generator-produced protocol copies. In shell, only the
`crew.sh` ratings projection should mention `replanned`; `dispatch.sh` should
only stamp engine/model metadata. There must be no shell amendment counter,
retry cap, classifier, or planner launcher.

- [ ] **Step 4: Trace every acceptance criterion**

| Acceptance                                   | Implemented by                                                                                  | Verified by                                                         |
| -------------------------------------------- | ----------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| Objective engine-general trigger             | Confirmed gate + stable target + exact quote + allowed amendment row; first row seeds count `1` | Initialization sentinel and shipped equality                        |
| Two-element oscillation terminates           | Seed `A = 1`; adjacent `B = 2`; globally repeated but adjacent-distinct `A = 3` triggers        | Dedicated initialization plus `A → B → A` protocol assertions       |
| One autonomous recovery maximum              | Shared `replan_used`; later three-row trigger blocks                                            | Protocol shared-budget assertion                                    |
| Mechanical convergence remains uncapped      | Reset/preserve decision table; no shell retry ceiling                                           | Protocol assertion plus forbidden-machinery audit                   |
| Missing lower execute rung remains available | Same-rung implementation, no budget                                                             | Protocol sentinel                                                   |
| Skipped-plan fallback shares the budget      | Same-rung planning sets used/replanned at episode start                                         | Protocol sentinel                                                   |
| Recovery is strictly stronger and finite     | Canonical per-engine ladders and top/no-rung block                                              | Protocol/projection test                                            |
| Authoritative current tuple                  | Exact engine/model/effort task header                                                           | Three-engine `dispatch.bats` table                                  |
| Useful blocked behavior                      | Ledger, tuple, attempts, budget, concrete question                                              | Protocol/projection test                                            |
| `replanned` defined before any stop          | Initialize false before startup drain; `replan_used` waits for execute                          | First-action ordering and pre-execute-stop semantic sentinels       |
| `replanned` emitted on stopping paths        | Complete latest snapshot before every pre/post-execute stop                                     | Protocol all-stop-path sentinel                                     |
| Ratings preserve false and legacy data       | `has("replanned")` projection                                                                   | True/false/missing `crew.bats` table                                |
| Canonical and generated artifacts agree      | Generator-owned complete protocol copies                                                        | All-core `cmp` to both shipped trees and expanded idempotence paths |
| No runtime classifier machinery              | Policy only in protocol; shell is metadata/projection                                           | Diff and `rg` audit                                                 |

- [ ] **Step 5: Commit any verification-only correction**

Only if a gate required an in-scope correction:

```bash
git add adapters tests
git commit -m "fix(protocols): align bounded replan verification"
```

Do not create an empty commit.
