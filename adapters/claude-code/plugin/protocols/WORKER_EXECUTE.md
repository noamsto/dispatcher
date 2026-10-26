# Worker protocol: execute and gate

Phase file of `WORKER_PROTOCOL.md`, binding like the core; "this protocol" and its sections resolve via the core's Phase files table.

## Fast deterministic gate (standard/deep)

After `execute`, before any model reviewer sees the diff: cheap deterministic checks.

- Discover build + vet/lint + unit commands from the repo (never assume a language or hardcode `go test`): `justfile`/`Makefile`, `package.json` scripts, pre-commit/CI config (`.pre-commit-config*`, `.github/workflows`, `treefmt`, `nix flake check`) or a project `verify` skill. Missing/unrunnable → `command_not_found` retro note.
- Scope: changed packages + affected consumers (+ unchanged ones `EVIDENCE_REVIEW.md` names): `git diff --name-only "$base_ref"...HEAD` (Base ref, else default branch) → modules/packages → run that set only (siblings share the CPU; whole-repo lint saturates every core).
- **Run the affected tests at every gate; CI runs the full suite.** A changed-file→test selector (dispatcher repo: `bash scripts/bats-affected.sh --base "$base_ref"`) picks tests for every run (iteration, review-fix, final pre-push). It only prints paths: run it alone, then run its output in calls of ≤ 3 files, `--filter` where useful. Its full-suite fallback: don't run the list; run test files naming the changed file (`grep -l`), none for shared test infrastructure. No selector: scoped gate stands. Never run the full suite locally or wait for CI. CI failure (dispatcher directive or resume): fix from log, gate to green, re-run `/deslop` on the fix, targeted review per rule 4 if behavioral.
- Lint whole via the canonical target when a scoped run would lie (scoped `golangci-lint` misses findings or invents phantom `typecheck` ones): scoped output looks off, or it is the only maintained target (a wrong lint verdict costs more than the cores).
- Loop to green here, cheaply: fix (subagent, rule 1), re-run; uncapped, independent of the review→fix loop (cap 2).
- Real test over synthetic demo: given a runnable behavior surface, write here a regression test pinning the acceptance criteria, esp. a reported edge input (`page > totalPages`), extending an existing test of that path (row/assertion) first. Manual/visual demos (throwaway Storybook story, screenshot walk-through) are never primary proof: only a fallback with no test surface, or a supplement when a reviewer must see rendered output.
- No runnable build/test surface (docs/protocol-only diff): say so, fall through to the review gate, invent no command.
- Green and committed, `fresh_seams: true` → the review-seam relaunch ("Fresh seam relaunch") before the code-review gate.

### Bounded plan-shaped recovery

Set execute-local `replan_used = false` at execute entry, not earlier; initial planning, critic revisions and deep consult false-negative recovery never consume it.

Episode: first scoped deterministic-gate failure → all discovered build/lint/unit/other subgates green (ends it, count dropped); switching command/subgate keeps both. Gate identity: exact command + stable subgate name. Target: most-specific stable deterministic id (named test/check, module/package, file+rule, file), volatile diagnostics stripped, sets sorted/deduped.

Before fixing, a row qualifies only with all of: confirmed deterministic failure, stable target, exact quoted old plan statement, one category (`scope`, `invariant/interface`, `dependency/order`). Record it:

```text
gate: <command/subgate>
target: <normalized target set>
old_plan: <exact quoted plan statement>
amendment: <replacement statement and scope|invariant|dependency>
```

After init or any reset, the first qualifying amendment seeds count 1 (no predecessor needed). A later row increments only if target and quoted plan element both differ from (no overlap with) the immediately previous qualifying row; else reset to zero, as does a mechanical fix, unclassifiable failure or actual intervening mechanical/unclassifiable observation. Subgate pass or unchanged flakiness probe: count kept, no row. Global uniqueness is irrelevant: `A(scope step 2) → B(interface step 4) → A(scope step 2)` reaches `1 → 2 → 3`. Rows 1-2: apply/amend/fix; row 3: record only, transfer to recovery. Classifier cases: healthy in-plan `A → B → C`, same-target `A → A`, cross-subgate, overlapping-plan, two-element oscillation. It covers worker-authored plans (`plan: required`, skipped-plan re-entry plans, accepted replacements); untouched provided/legacy plans take the direct contradiction fallback (`WORKER_PLAN` "Plan of record" → Re-entry).

| Transition | Rung | Budget |
|---|---|---|
| Missing lower execute rung | Same-rung implementation | Not consumed; `replanned` unchanged |
| Provided/legacy contradiction | Same-rung planning re-entry | Consume at episode start |
| Three qualifying amendments | Exactly one stronger planning rung | Consume at launch start |

Both planning transitions share this budget: take at most one autonomously. After replacement: clear only the count (keep ledger, `replan_used = true`) → checkpoint-peek → `gate_thrash` retro note with ledger rows, mid-execute path ("Retro notes") → execute. A later full three-row sequence blocks. Mechanical convergence stays uncapped throughout.

Three amendments: one fresh planning-only context strictly above `WORKER_TASK.md`'s authoritative engine/model/effort tuple; never change engine, skip a rung or guess an unlisted tuple. Top or unavailable rung blocks unlaunched. `replanned` stays false only if no earlier execute-time planning episode began. Any engine may use its bounded critic within this single episode (harness-shipped roster; per-engine rung/mechanism: `spec-plan-critic` critic table).

- Claude: Agent model override `haiku → sonnet → opus → fable`; `opus → fable` keeps the hard, well-specified, long-horizon eligibility check. Fable, ineligible opus, unknown full ids, unavailable launches block. Effort is metadata only.

Viable replacement: covers all three ledger rows, names allowed files/components, gives finite ordered implementation steps + deterministic validation commands, leaves nothing to improvise at execute. Refusal, timeout, failed extraction/critic, unavailable launch or non-viable output blocks (no fallback to original plan or a second planner) → `rung_blocked` retro note naming rung and reason.

### Plan-shaped gate rework

A block→await question ("Report to the bus"): `crew status "$CREW_WORKER_ID" blocked "plan-shaped gate rework: <reason>"` → `crew msg "$CREW_WORKER_ID" dispatcher:<crew_id> "<concrete question and evidence>"` → await. Include gate identity, all three ledger rows, fixes attempted, authoritative engine/model/effort, budget state, missing rung or failed viability condition; ask for a concrete replacement or supported higher rung. Dispatcher replacement = external direction: set `replanned = true`, keep the post-replan cap, checkpoint-peek, resume execute.

## Deliberate load (timing repros)

CPU-contention timing-flake repros are allowed but crew-wide on a shared host. Before starting load:

1. Cap it: `systemd-run --user --scope -p CPUQuota=<N>%` (N ≤ 50), never a bare `yes > /dev/null` per core (the cap, not a lock, protects).
2. Announce it: `crew msg "$CREW_WORKER_ID" dispatcher:<crew_id> "<starting a CPU-capped load test …>"`, then a matching `…ended` msg.
3. Register it under Rule 8's trap.
