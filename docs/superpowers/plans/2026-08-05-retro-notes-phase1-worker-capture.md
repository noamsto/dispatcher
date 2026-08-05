# Retro Notes — Phase 1 (Worker Capture) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make workers record **why** a run went wrong — tagged, groupable notes carrying the reasons behind metrics fields that today are bare numbers and booleans.

**Architecture:** Prose-only change to `WORKER_PROTOCOL.md` plus regenerated adapter trees. A new `## Retro notes (all tiers)` section defines a closed tag vocabulary, each tag bound to a protocol branch that already exists. Notes learned at a stopping path ride a new `notes` array inside the metrics snapshot that already fires there; notes learned mid-execute are emitted immediately to the `retro:<crew>` synthetic sink, because a `tmux kill-window` or stall-watch hang never reaches a stopping path. Each branch that can produce a note gets a pointer back to the section, so the rule fires where the worker actually is. **No `crew.sh` change in this phase** — notes flow on a protocol merge alone, with no rebuild.

**Tech Stack:** Markdown (`adapters/core/protocols/WORKER_PROTOCOL.md`), `scripts/gen-adapters.sh` (bash + jq + yq), bats.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-08-05-retro-notes-design.md`. This plan implements **Phase 1** only (worker capture). Phase 2 (`crew retro` read path) and Phase 3 (dispatcher drain synthesis) are separate plans.
- **Canonical source is `adapters/core/`.** Never edit `adapters/claude-code/plugin/protocols/`, `adapters/codex/plugin/protocols/`, or any generated tree by hand. Edit `adapters/core/protocols/WORKER_PROTOCOL.md`, then run `scripts/gen-adapters.sh`. The test `every canonical protocol exactly matches both shipped protocol trees` fails otherwise, and CI has a separate drift gate.
- **`adapters/` is excluded from formatters** (prettier/shfmt) on purpose — these files are model-facing prompts and reformatting them has corrupted nested fences before. Do not reformat surrounding prose; match the file's existing voice: dense, WHY-forward, key terms bolded.
- **Closed vocabulary, exactly these tags:** `command_not_found`, `gate_thrash`, `approach_abandoned`, `consult_failed`, `rung_blocked`, `other`. The dispatcher-side tags (`misrouted`, `fanout_binder`, `spec_too_thin`, `session_summary`) are **Phase 3** — do not add them to `WORKER_PROTOCOL.md`.
- **Note JSON shape:** `{"seam":"<stage>","tag":"<tag>","detail":"<what>"}`. Snapshot notes omit `ts` (they inherit the snapshot's); seam notes get `.ts` from the `msg` event itself.
- **Silence is the healthy case.** A note is written **only** when its branch is taken. Never write a note to report success, and never write one per seam unconditionally.
- **No writer-side tag validation.** Writes reuse the existing `metrics:`-style synthetic sink, so `crew.sh` is untouched; Phase 2's reader surfaces unknown tags in an explicit `unknown` bucket. Do not add a `crew note` subcommand.
- **Test idiom for protocol prose:** `tests/adapters.bats` asserts exact strings with `run grep -F "$statement" "$protocol"` where `protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"`. Every asserted string must match the committed prose **byte for byte** — copy it, don't retype it.
- **Full suite must be green at every commit:** `bats tests/` (**140** tests on this branch before this plan; each task adds exactly one).

---

### Task 1: Define the retro-note section and vocabulary

**Files:**

- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md` (insert a new `## Retro notes (all tiers)` section immediately **before** the `## Report to the bus (mandatory)` heading)
- Modify: `adapters/claude-code/plugin/protocols/WORKER_PROTOCOL.md`, `adapters/codex/plugin/protocols/WORKER_PROTOCOL.md` (generated — via `scripts/gen-adapters.sh`, never by hand)
- Test: `tests/adapters.bats`

**Interfaces:**

- Consumes: nothing (first task).
- Produces: the section heading `## Retro notes (all tiers)`, the six tag names, and the note JSON shape `{"seam":"<stage>","tag":"<tag>","detail":"<what>"}`. Tasks 2-4 reference this section by name and reuse that exact JSON shape.

- [ ] **Step 1: Write the failing test**

Append to `tests/adapters.bats`:

```bash
@test "worker protocol defines the retro-note vocabulary" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    '## Retro notes (all tiers)' \
    '**Write a note only when one of the branches below is taken.**' \
    '`command_not_found`' \
    '`gate_thrash`' \
    '`approach_abandoned`' \
    '`consult_failed`' \
    '`rung_blocked`' \
    '{"seam":"<stage>","tag":"<tag>","detail":"<what>"}'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bats tests/adapters.bats --filter 'retro-note vocabulary'`

Expected: `not ok 1 worker protocol defines the retro-note vocabulary` — the section does not exist yet.

- [ ] **Step 3: Insert the section**

In `adapters/core/protocols/WORKER_PROTOCOL.md`, immediately **before** the line `## Report to the bus (mandatory)`, insert:

```markdown
## Retro notes (all tiers)

A note records **why** something went wrong, in your own words, tagged so notes group across runs. The metrics fields say a gate looped or a consult failed; a note says _which_ gate and _why_ the consult failed. That reason is otherwise lost — the plan-shaped ledger lives only in your context, and `consult_engine` goes `null` on exactly the failure you would want explained.

**Write a note only when one of the branches below is taken.** A run that takes none writes none: silence is the healthy case and costs nothing. Never write a note to report success, and never write one per seam unconditionally.

| tag                  | write it when                                                                                                                                                                                         |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `command_not_found`  | a build/vet/lint/unit command you discovered from the repo does not exist or will not run. Detail: the command, and how it failed.                                                                    |
| `gate_thrash`        | three qualifying ledger rows force a replacement, so the episode escalated rather than converged. Detail: for each row, its `gate`, normalized `target`, quoted `old_plan`, and `amendment` category. |
| `approach_abandoned` | the plan of record is contradicted by the repo and you re-enter planning. Detail: which named file or approach did not hold.                                                                          |
| `consult_failed`     | a consultant refuses, times out, or is unavailable. Detail: which consultant, and which of the three.                                                                                                 |
| `rung_blocked`       | a recovery transition blocks. Detail: which rung, and why — top or unavailable rung, ineligible opus, failed extraction/critic, or non-viable output.                                                 |
| `other`              | something went wrong that no tag above covers. Detail: what.                                                                                                                                          |

Each note is one object: `{"seam":"<stage>","tag":"<tag>","detail":"<what>"}` — `seam` is the stage you were in (`spec`, `plan`, `execute`, `gate`, `review`).

Keep a detail under ~2 KB. The bus writer truncates an oversized line rather than corrupting the log, so a very long paste loses its tail.
```

- [ ] **Step 4: Regenerate the adapter trees**

Run: `scripts/gen-adapters.sh`

Expected: no output on success. This copies the canonical protocol into both plugin trees; skipping it fails `every canonical protocol exactly matches both shipped protocol trees`.

- [ ] **Step 5: Run the test to verify it passes, then the full suite**

Run: `bats tests/adapters.bats --filter 'retro-note vocabulary'`
Expected: `ok 1 worker protocol defines the retro-note vocabulary`

Run: `bats tests/`
Expected: all tests pass, `not ok` appears zero times.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/protocols/WORKER_PROTOCOL.md adapters/claude-code/plugin/protocols/WORKER_PROTOCOL.md adapters/codex/plugin/protocols/WORKER_PROTOCOL.md tests/adapters.bats
git commit -m "feat(protocols): define the retro-note vocabulary for workers"
```

---

### Task 2: Carry notes in the metrics snapshot

**Files:**

- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md` (the `## When done` metrics-record template and the field prose that follows it)
- Modify: generated protocol trees (via `scripts/gen-adapters.sh`)
- Test: `tests/adapters.bats`

**Interfaces:**

- Consumes: the tag vocabulary and note JSON shape from Task 1.
- Produces: the `"notes":[…]` field in the metrics-record template. Task 3 relies on this being the stopping-path path, so its own rule can be scoped to execute only.

- [ ] **Step 1: Write the failing test**

Append to `tests/adapters.bats`:

```bash
@test "worker protocol carries retro notes in the metrics snapshot" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    '"review_mode":"<full|downgraded|none>","notes":[]' \
    '`notes` = the retro notes you accumulated this run' \
    'An empty array is the healthy case.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bats tests/adapters.bats --filter 'metrics snapshot'`
Expected: FAIL — the template has no `notes` field.

- [ ] **Step 3: Add the field to the template**

In `adapters/core/protocols/WORKER_PROTOCOL.md`, find this exact substring inside the metrics-record fenced block under `## When done`:

```text
"review_mode":"<full|downgraded|none>"}'
```

Replace it with:

```text
"review_mode":"<full|downgraded|none>","notes":[]}'
```

- [ ] **Step 4: Document the field**

In the same `## When done` bullet, immediately after the sentence ending `never compared across mismatched depths.`, insert this sentence:

```text
`notes` = the retro notes you accumulated this run (see "Retro notes"), as an array of `{"seam","tag","detail"}` objects. An empty array is the healthy case. Notes you already emitted mid-execute stay in the array too, so one snapshot is the complete record of the run — a resumed run's newer snapshot supersedes the older one, and duplicates across the two paths are expected and deduplicated on read.
```

- [ ] **Step 5: Regenerate, verify, run the full suite**

Run: `scripts/gen-adapters.sh`
Run: `bats tests/adapters.bats --filter 'metrics snapshot'`
Expected: `ok 1 worker protocol carries retro notes in the metrics snapshot`

Run: `bats tests/`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/protocols/WORKER_PROTOCOL.md adapters/claude-code/plugin/protocols/WORKER_PROTOCOL.md adapters/codex/plugin/protocols/WORKER_PROTOCOL.md tests/adapters.bats
git commit -m "feat(protocols): carry retro notes in the metrics snapshot"
```

---

### Task 3: Emit mid-execute notes immediately

**Files:**

- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md` (extend the `## Retro notes (all tiers)` section from Task 1)
- Modify: generated protocol trees (via `scripts/gen-adapters.sh`)
- Test: `tests/adapters.bats`

**Interfaces:**

- Consumes: the `## Retro notes (all tiers)` section (Task 1) and the snapshot path (Task 2).
- Produces: the `retro:$CREW_ID` sink name, which Phase 2's reader queries.

- [ ] **Step 1: Write the failing test**

Append to `tests/adapters.bats`:

```bash
@test "worker protocol emits mid-execute retro notes immediately" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    'crew msg "$CREW_WORKER_ID" "retro:$CREW_ID"' \
    '**Execute is the only stage that emits early**' \
    'a `tmux kill-window` or a stall-watch hang never reaches a stopping path' \
    'Like `metrics:`, `retro:` is a synthetic sink — it never wakes the dispatcher.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bats tests/adapters.bats --filter 'mid-execute'`
Expected: FAIL — no `retro:` sink is documented.

- [ ] **Step 3: Extend the section**

In `adapters/core/protocols/WORKER_PROTOCOL.md`, at the end of the `## Retro notes (all tiers)` section (immediately after the `Keep a detail under ~2 KB.` paragraph), append:

````markdown
**Where a note goes depends on when you learn it.**

- **You are at a stopping path** — put it in the metrics snapshot's `notes` array (see "Report to the bus"). The snapshot already fires before every stopping path, so this costs no extra write and inherits its supersede-on-resume semantics.
- **You are mid-execute** — emit it immediately, then also keep it for the snapshot:

  ```
  crew msg "$CREW_WORKER_ID" "retro:$CREW_ID" '{"seam":"execute","tag":"<tag>","detail":"<what>"}'
  ```

**Execute is the only stage that emits early**, and the reason is narrow: a `tmux kill-window` or a stall-watch hang never reaches a stopping path, so a note held back for the snapshot would die with the session — and execute is where that risk concentrates. Every other seam ends in a stopping path that snapshots anyway, so holding those costs nothing.

Like `metrics:`, `retro:` is a synthetic sink — it never wakes the dispatcher.
````

- [ ] **Step 4: Regenerate, verify, run the full suite**

Run: `scripts/gen-adapters.sh`
Run: `bats tests/adapters.bats --filter 'mid-execute'`
Expected: `ok 1 worker protocol emits mid-execute retro notes immediately`

Run: `bats tests/`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add adapters/core/protocols/WORKER_PROTOCOL.md adapters/claude-code/plugin/protocols/WORKER_PROTOCOL.md adapters/codex/plugin/protocols/WORKER_PROTOCOL.md tests/adapters.bats
git commit -m "feat(protocols): emit mid-execute retro notes so a killed worker still reports"
```

---

### Task 4: Point each branch at its tag

**Files:**

- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md` (five existing branches: Plan-of-record re-entry, Orchestration-consult fallback, Fast-deterministic-gate command discovery, Bounded-plan-shaped-recovery ledger, recovery-rung blocks)
- Modify: generated protocol trees (via `scripts/gen-adapters.sh`)
- Test: `tests/adapters.bats`

**Interfaces:**

- Consumes: all six tags (Task 1) and both emit paths (Tasks 2-3).
- Produces: nothing new — this is the compliance wiring that makes the vocabulary fire.

**Why this task exists:** a table 100 lines away from the branch will not fire. The worker reads §52 while it is abandoning an approach; the instruction has to be there.

- [ ] **Step 1: Write the failing test**

Append to `tests/adapters.bats`:

```bash
@test "worker protocol points each branch at its retro tag" {
  protocol="$ROOT/adapters/core/protocols/WORKER_PROTOCOL.md"
  for statement in \
    'Write an `approach_abandoned` retro note.' \
    'Write a `consult_failed` retro note naming the consultant and the reason.' \
    'write a `command_not_found` retro note.' \
    'Write a `gate_thrash` retro note carrying the ledger rows.' \
    'Write a `rung_blocked` retro note naming the rung and the reason.'; do
    run grep -F "$statement" "$protocol"
    [ "$status" -eq 0 ]
  done
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bats tests/adapters.bats --filter 'points each branch'`
Expected: FAIL — none of the five pointers exist.

- [ ] **Step 3: Add the five pointers**

Make five edits in `adapters/core/protocols/WORKER_PROTOCOL.md`. Each appends one sentence to an existing bullet — do not restructure the surrounding prose.

1. In the **Re-entry (both skip paths)** bullet, after `This is a single explicit fallback (mirrors the deep false-negative recovery), not a loop.`, append:

   ```text
    Write an `approach_abandoned` retro note.
   ```

2. In the **Fallback — a should, not a blocker** item, after `Never fail the worker on a missing consult (same rule as the codex-diverse reviewer, "Code review gate").`, append:

   ```text
    Write a `consult_failed` retro note naming the consultant and the reason.
   ```

3. In the **Discover the command from the repo** bullet, after this exact sentence ending:

   ```text
   or a project `verify` skill if one exists.
   ```

   append:

   ```text
    If a command you settled on then does not exist or will not run, write a `command_not_found` retro note.
   ```

4. In the **Bounded plan-shaped recovery** section, after this exact sentence:

   ```text
   After a replacement, clear only the consecutive count, preserve the ledger and `replan_used = true`, checkpoint-peek, then execute.
   ```

   append:

   ```text
    Write a `gate_thrash` retro note carrying the ledger rows.
   ```

5. In the same section, after `Refusal, timeout, failed extraction/critic, unavailable launch, or non-viable output blocks without falling back to the original plan or a second planner.`, append:

   ```text
    Write a `rung_blocked` retro note naming the rung and the reason.
   ```

- [ ] **Step 4: Regenerate, verify, run the full suite**

Run: `scripts/gen-adapters.sh`
Run: `bats tests/adapters.bats --filter 'points each branch'`
Expected: `ok 1 worker protocol points each branch at its retro tag`

Run: `bats tests/`
Expected: all pass, `not ok` count zero.

- [ ] **Step 5: Verify the drift gate is genuinely clean**

Run: `scripts/gen-adapters.sh && git status --short`
Expected: empty output. A non-empty result means generated output drifted from its source and CI's drift gate would fail.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/protocols/WORKER_PROTOCOL.md adapters/claude-code/plugin/protocols/WORKER_PROTOCOL.md adapters/codex/plugin/protocols/WORKER_PROTOCOL.md tests/adapters.bats
git commit -m "feat(protocols): point each trouble branch at its retro tag"
```

---

## Done when

- `bats tests/` is green with four new tests (144 total).
- `scripts/gen-adapters.sh && git status --short` prints nothing.
- Every tag in the vocabulary has a pointer at its branch, and every pointer names a tag in the vocabulary.
- `crew.sh` is untouched — confirm with `git diff origin/extract --name-only`, which must not list `adapters/core/crew.sh`.

## Deployment

Protocols are read-by-path off the default branch, so this **deploys on merge** with no rebuild. Phase 2 changes `crew.sh` and will need a `dispatcher` flake-input bump in nix-config plus `nh home switch`.

## Not in this phase

- `crew retro` and any reading, grouping, or reporting of notes (Phase 2).
- `duckdb` in `runtimeInputs` (Phase 2).
- Dispatcher-side tags and the DRAINED synthesis trigger (Phase 3).
- Any `crew.sh` change, including a `crew note` subcommand or writer-side tag validation — deliberately excluded; see the spec's "Tag validation is reader-side".
