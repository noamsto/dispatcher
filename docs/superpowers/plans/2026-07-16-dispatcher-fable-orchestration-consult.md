# Fable Orchestration Consult Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give deep-tier workers a gated, ephemeral in-worktree Fable consult that seeds decomposition into the existing plan pipeline, with a plain-path fallback and per-deep-worker outcome metrics.

**Architecture:** Pure prompt-artifact change — no shell/code. A deep worker runs a cheap in-worktree survey at its plan seam; if it trips, it spawns an ephemeral Fable subagent (Agent `model: fable`) that writes `DECOMPOSITION.md`; the existing `spec-plan-critic` skill consumes it as a hard constraint and `plan-critic` checks conformance; Fable refusal/timeout falls back to today's plain path; every deep worker emits an outcome-metrics `crew msg` at finish for the #86 counterfactual.

**Tech Stack:** Markdown instruction artifacts under `home/ai/claude-code/` (`WORKER_PROTOCOL.md`, `DISPATCHER_PROTOCOL.md`, `dispatch-orchestration.md`, `skills/spec-plan-critic/SKILL.md`, `agents/plan-critic.md`). All read-by-path / hot-loaded off the main checkout's `.claude` symlinks.

## Global Constraints

- **No `dispatch.sh` / `crew.sh` change.** Consult is internal to the worker's Agent-tool calls; metrics ride the existing `crew msg <from> <to> <body>`.
- **Deploys on merge to main, no rebuild** — every touched file is read-by-path / hot-loaded off `.claude` symlinks. For the same reason, the edited **skill/agent do not resolve while on this worktree branch** (skill-symlink-worktree gotcha) — live verification (Task 7) runs **after merge** or against the main checkout.
- **Trigger is `deep` tier only.** No new dispatcher lever; `standard`/`trivial` pipelines stay byte-identical.
- **Fable consult is "a should, not a blocker"** — refusal/timeout/unavailable degrades to the plain `writing-plans` path, never a worker failure (mirror `WORKER_PROTOCOL.md` codex-diverse reviewer pattern).
- **Provenance-blind** — neither the plan text nor `DECOMPOSITION.md` names Fable as author.
- **Spec:** `docs/superpowers/specs/2026-07-16-dispatcher-fable-orchestration-consult-design.md`.
- **This is documentation-artifact work** — "tests" are internal cross-reference consistency plus one post-merge live smoke run; there is no unit-test surface. Each task's verification step reflects that.

## File Structure

- `home/ai/claude-code/WORKER_PROTOCOL.md` — owns the consult flow (new section), the deep-pipeline wiring, and the metrics emit.
- `home/ai/claude-code/skills/spec-plan-critic/SKILL.md` — owns passing `DECOMPOSITION.md` into the plan-draft step.
- `home/ai/claude-code/agents/plan-critic.md` — owns the conformance attack axis.
- `home/ai/claude-code/dispatch-orchestration.md` — owns the choosing-tree note + the outcome-metrics documentation.
- `home/ai/claude-code/DISPATCHER_PROTOCOL.md` — owns the one-line deep-signal note.

## The DECOMPOSITION.md serialisation (used by Tasks 1, 3, 4)

Locked field set from the spec, serialised as markdown at the worktree root:

```markdown
# Decomposition

## Components

- **<component-id>**: <one-line description of this unit of work>
  - boundaries: may touch `<paths/globs>`; must not touch `<paths/globs>`
  - risk: <security-adjacent | wide-blast | migration | none>

## Ordering

- <component-id> before <component-id>
- <component-id> ∥ <component-id> # parallel-safe

## Interfaces

- `<name>`: <signature / data shape that must stay stable across components>
```

---

### Task 1: WORKER_PROTOCOL.md — the orchestration consult (deep)

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (deep pipeline line 13; new section after the "Pipeline by tier" block; rule 3 cross-reference)

**Interfaces:**

- Produces: a `DECOMPOSITION.md` at the worktree root (schema above) consumed by Task 3.
- Produces: the term "orchestration consult" referenced by Tasks 3, 4, 5.
- Consumes: existing `spec-plan-critic` invocation (deep pipeline), rule 3 revision cap.

- [ ] **Step 1: Read the current deep-pipeline line and the "Pipeline by tier" block**

Read: `home/ai/claude-code/WORKER_PROTOCOL.md:10-13` and `:66-69` (rules 1–3).

- [ ] **Step 2: Add the consult section** immediately after the "Pipeline by tier" block (after line 13, before "## Checkpoint-peek")

Insert:

```markdown
## Orchestration consult (deep only)

Before the plan phase, decide **once** whether to bring Fable in to decompose the task. This decision is made in the worktree (where the code is), never at dispatch time.

1. **Survey (cheap, in-worktree).** Scan the task against the repo: how many modules/packages it plausibly touches, and its blast radius (shared interfaces, cross-cutting seams). Emit a single boolean — _does this need a stronger decomposition than an opus plan alone?_ Keep it cheap: a few `Grep`/`Glob` passes, no subagent.
2. **If it trips, consult Fable.** Spawn an **ephemeral** subagent with the Agent tool, `model: fable`, running in this worktree. Prompt it to read `WORKER_TASK.md` and the relevant code and write `DECOMPOSITION.md` at the worktree root using this exact structure — `components` (each a stable id + one-line + `boundaries` may/​must-not-touch + `risk` tag), `ordering` (dependency order, `∥` for parallel-safe), `interfaces` (contracts that must stay stable across the split). It authors the **decomposition, not the plan** — no plan-schema step tags. `DECOMPOSITION.md` must **not** name its author (the plan-critic reads it author-less; rule 2 discipline).
3. **Fallback — a should, not a blocker.** If Fable refuses, times out, or is unavailable, drop the consult and proceed on the plain `writing-plans` path — **byte-identical to a non-consulted deep worker**. Never fail the worker on a missing consult (same rule as the codex-diverse reviewer, "Code review gate").
4. **Seed the plan.** When `DECOMPOSITION.md` exists, the `spec-plan-critic` plan phase consumes it as a hard constraint and `plan-critic` checks conformance (see that skill/agent). You do not hand-author the plan.
5. **False-negative recovery.** If the survey did **not** trip (no consult) and the plain-path plan then **exhausts the revision cap** (rule 3) without an accepted plan, that is the signal the survey missed a genuinely complex task: run the consult **once now** and re-plan from the resulting `DECOMPOSITION.md`. This is a single explicit attempt **beyond** the cap (total stays finite: cap + 1). A worker that **already** consulted and still exhausts the cap surfaces `escalations[]` as today — no extra attempt.
```

- [ ] **Step 3: Wire the consult into the deep pipeline line** (line 13)

Modify line 13 so the deep pipeline names the consult at the plan seam. Change:

`- **deep** — run \`spec-plan-critic\` with \`{ tier: 'deep', ... }\` (spec + spec-critic, then plan + plan-critic), then execute, …`

to:

`- **deep** — run \`spec-plan-critic\` with \`{ tier: 'deep', ... }\` (spec + spec-critic, then — see **Orchestration consult** — an optional Fable decomposition seeds plan + plan-critic), then execute, …`

- [ ] **Step 4: Verify internal consistency**

Run: `rg -n 'Orchestration consult|DECOMPOSITION|spec-plan-critic|cap' home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: the new section references `spec-plan-critic`, `DECOMPOSITION.md`, and rule 3's cap; the deep-pipeline line links the section. No dangling reference to a removed "hint" or "orchestrate" flag.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): orchestration consult section (deep) + false-negative recovery"
```

---

### Task 2: WORKER_PROTOCOL.md — deep-worker outcome metrics

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` ("When done" block, line 74–75)

**Interfaces:**

- Consumes: `crew msg <from> <to> <body>` (existing CLI), `crew_id` from `WORKER_TASK.md`.
- Produces: a `kind:"msg"` bus event to `metrics:<crew_id>` documented by Task 5.

- [ ] **Step 1: Read the "When done" block**

Read: `home/ai/claude-code/WORKER_PROTOCOL.md:74-75`.

- [ ] **Step 2: Add the metrics emit** to the "When done" block (deep tier), before the final `Then stop.`

Insert:

```markdown
- **Deep workers — emit outcome metrics (consulted or not).** Before you stop, append a metrics record to the bus so the consult can be evaluated against a baseline:
```

crew msg "worker:$(git branch --show-current)" "metrics:$CREW_ID" '{"consulted":<true|false>,"plan_critic_first_pass":"<accept|revise|reject>","rework_count":<int>,"review_high":<int>}'

```
(`$CREW_ID` is the `crew_id:` from `WORKER_TASK.md`.) `consulted` is whether the Fable consult ran; `plan_critic_first_pass` is the plan-critic's verdict on the **first** plan draft; `rework_count` is how many execute-stage fixes the gates forced; `review_high` is HIGH-severity review-gate findings. This is a plain `msg` to a synthetic sink — it does **not** wake the dispatcher (its `watch`/`inbox` filter `to==dispatcher:<crew>`/`*`, never `metrics:<crew>`). **Every deep worker emits this**, so non-consulted deep workers form the baseline.
```

- [ ] **Step 3: Verify the sink is inert to the dispatcher**

Run: `rg -n 'to==\$me or .to=="\*"|dispatcher:' home/ai/claude-code/crew.sh`
Expected: `watch`/`inbox`/reply filters match `dispatcher:<crew>` or `*` only — confirming a `metrics:<crew>` recipient never matches (no `crew.sh` change needed).

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): deep-worker outcome metrics emit for consult counterfactual"
```

---

### Task 3: spec-plan-critic/SKILL.md — consume DECOMPOSITION.md as a hard constraint

**Files:**

- Modify: `home/ai/claude-code/skills/spec-plan-critic/SKILL.md` (Standard-tier step 1 draft-plan prompt, lines 19–20; the deep-tier phase reuses the same loop)

**Interfaces:**

- Consumes: `DECOMPOSITION.md` at repo root (Task 1).
- Produces: a plan draft constrained by the decomposition, reviewed by Task 4.

- [ ] **Step 1: Read the draft-plan step**

Read: `home/ai/claude-code/skills/spec-plan-critic/SKILL.md:19-24`.

- [ ] **Step 2: Add the constraint to the draft-plan step** (after the `implement: opus` sub-bullet, still inside step 1)

Insert:

```markdown
- **Decomposition constraint (if present).** If a `DECOMPOSITION.md` exists at the repo root, pass its full contents to the drafting agent as a **hard constraint**: every plan step must map to exactly one `component`; step order must respect `ordering`; no step may touch outside its component's `boundaries`; the declared `interfaces` must be preserved. Any deviation must be justified inline in the plan. Do **not** attribute the decomposition to any author — treat it as the task's given structure.
```

- [ ] **Step 3: Verify the deep-tier phase inherits it**

Run: `rg -n 'plan phase|same 3-step|DECOMPOSITION' home/ai/claude-code/skills/spec-plan-critic/SKILL.md`
Expected: deep-tier "Feed accepted spec into plan phase (same 3-step loop above)" — confirming the new constraint applies to deep (the tier the consult runs on) with no separate edit.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/skills/spec-plan-critic/SKILL.md
git commit -m "feat(spec-plan-critic): consume DECOMPOSITION.md as a hard plan constraint"
```

---

### Task 4: plan-critic.md — decomposition-conformance attack axis

**Files:**

- Modify: `home/ai/claude-code/agents/plan-critic.md` ("Attack the plan on these axes", after axis 5, line 17)

**Interfaces:**

- Consumes: `DECOMPOSITION.md` at repo root (Task 1); the plan draft (Task 3).
- Produces: a `verdict` that blocks on unjustified decomposition deviation.

- [ ] **Step 1: Read the axes list**

Read: `home/ai/claude-code/agents/plan-critic.md:12-22`.

- [ ] **Step 2: Add axis 6** after axis 5 (line 17)

Insert:

```markdown
6. **Decomposition conformance** — _only if `DECOMPOSITION.md` is present at the repo root._ Read it as the task's given structure (it is author-less; do not defer to it as an authority — verify against it). Every plan step must map to exactly one `component`; step order must respect `ordering`; no step may touch outside its component's `boundaries`; the declared `interfaces` must be preserved. An unjustified deviation is **blocking**; a deviation with an explicit inline justification is acceptable if the justification holds.
```

- [ ] **Step 3: Verify axis numbering + provenance discipline**

Run: `rg -n '^[0-9]\.|author-less|DECOMPOSITION' home/ai/claude-code/agents/plan-critic.md`
Expected: axes numbered 1–6 contiguously; axis 6 present; "author-less" phrasing present (provenance-blind).

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/agents/plan-critic.md
git commit -m "feat(plan-critic): decomposition-conformance attack axis"
```

---

### Task 5: dispatch-orchestration.md — choosing-tree note + outcome-metrics doc

**Files:**

- Modify: `home/ai/claude-code/dispatch-orchestration.md` (after the "Shape-tag vocabulary" note, line 52–53)

**Interfaces:**

- Consumes: the metrics `crew msg` format (Task 2), the consult (Task 1).

- [ ] **Step 1: Read the shape-tag / lever section**

Read: `home/ai/claude-code/dispatch-orchestration.md:52-59`.

- [ ] **Step 2: Add the consult + metrics note** after the "Shape-tag vocabulary" paragraph (after line 53)

Insert:

```markdown
**Orchestration consult (worker-side, deep).** Decomposition help from Fable is decided **in the worker's worktree** at the plan seam, not by the dispatcher — the dispatcher's only lever is tiering the task `deep` (its existing "architectural / wide-blast" signal). See `WORKER_PROTOCOL.md` → "Orchestration consult". Every deep worker emits an outcome-metrics record to the bus at finish:
`crew msg worker:<branch> metrics:<crew_id> '{"consulted":…,"plan_critic_first_pass":…,"rework_count":…,"review_high":…}'`.
It rides `crew msg` (no `crew.sh` change) and never wakes the dispatcher. Consulted vs non-consulted deep workers are the A/B for whether the Fable lever pays — the counterfactual #86's oracle gate needs. Read it offline: `crew log <crew> | jq 'select(.to|startswith("metrics:"))'`.
```

- [ ] **Step 3: Verify no contradiction with the model map / levers**

Run: `rg -n 'Fable|consult|metrics|deep' home/ai/claude-code/dispatch-orchestration.md`
Expected: the new note is consistent with the existing model-map row (Fable = deep escalation) and the "three orthogonal levers" section; no claim that the dispatcher decides the consult.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/dispatch-orchestration.md
git commit -m "docs(dispatch): worker-side consult + outcome-metrics in the choosing tree"
```

---

### Task 6: DISPATCHER_PROTOCOL.md — one-line deep-signal note

**Files:**

- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md` (after the tier/model paragraph, line 17)

- [ ] **Step 1: Read the tier guidance paragraph**

Read: `home/ai/claude-code/DISPATCHER_PROTOCOL.md:11-17`.

- [ ] **Step 2: Append one sentence** to the end of line 17 (the "raise the tier" paragraph)

Append:

` A task whose **decomposition** is the hard part (many interacting components, subtle split) is a \`deep\` signal too — the deep worker decides in its worktree whether to bring Fable in to decompose it (see \`WORKER_PROTOCOL.md\` → "Orchestration consult"); you do not make that call.`

- [ ] **Step 3: Verify**

Run: `rg -n 'Orchestration consult|decomposition' home/ai/claude-code/DISPATCHER_PROTOCOL.md`
Expected: one reference, in the tier paragraph; no new lever/flag introduced.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/DISPATCHER_PROTOCOL.md
git commit -m "docs(dispatcher): note decomposition-hard tasks are a deep signal"
```

---

### Task 7: Verification — consistency scan + post-merge live smoke

**Files:** none (verification only)

- [ ] **Step 1: Whole-change cross-reference scan**

Run: `rg -n -i 'orchestrate: true|force-on hint' home/ai/claude-code/`
Expected: **no matches** (the dropped hint left no residue in any artifact).

Run: `rg -n 'DECOMPOSITION|Orchestration consult|metrics:' home/ai/claude-code/`
Expected: mutually consistent references across `WORKER_PROTOCOL.md`, `spec-plan-critic/SKILL.md`, `plan-critic.md`, `dispatch-orchestration.md`, `DISPATCHER_PROTOCOL.md`.

- [ ] **Step 2: Open the PR** (branch already pushed by subagent-driven-development flow)

```bash
gh pr create --assignee @me --title "feat(dispatcher): Fable orchestration consult (deep)" --body "Implements docs/superpowers/specs/2026-07-16-dispatcher-fable-orchestration-consult-design.md. Deep workers gain a gated ephemeral in-worktree Fable decomposition consult that seeds spec-plan-critic; plain-path fallback; per-deep-worker outcome metrics for the #86 counterfactual. No dispatch.sh/crew.sh change."
```

- [ ] **Step 3: Merge to main** (required before the edited skill/agent resolve — skill-symlink-worktree gotcha)

Merge via the normal review flow. The skill/agent/protocol changes hot-load off the main checkout's `.claude` symlinks; no `nh` rebuild is needed.

- [ ] **Step 4: Post-merge live smoke — consult fires + seeds + metrics** (throwaway repo, mirrors prior harness smoke tests)

From a dispatcher against a throwaway repo, dispatch a genuinely architectural task as `deep`. Verify, in order:

1. The worker's survey trips and an Agent subagent with `model: fable` runs in the worktree.
2. `DECOMPOSITION.md` appears at the worktree root with the four field groups.
3. The plan draft maps steps to components; `plan-critic` runs axis 6 (a deliberately non-conforming plan is rejected).
4. At finish, `crew log <crew> | jq 'select(.to|startswith("metrics:"))'` shows one record with `consulted:true`.
   Expected: all four observed.

- [ ] **Step 5: Post-merge live smoke — fallback + baseline**
  1. Dispatch a deep task whose survey does **not** trip (few files) → confirm no consult, plain path, and a metrics record with `consulted:false` (the baseline).
  2. Simulate Fable unavailability (e.g. force the subagent to error) on a survey-tripping task → confirm the worker degrades to the plain path and still opens a PR (a should, not a blocker), with `consulted:false`.
     Expected: both observed; no worker failure attributable to the consult.

## Self-Review

**1. Spec coverage:**

- §1 pull-biased/worker-triggered, no hint → Task 1 (survey at plan seam), Task 6 (dispatcher note, no lever). ✓
- §2 ephemeral in-worktree Fable subagent → Task 1 step 2. ✓
- §3 SEED + DECOMPOSITION.md schema + conforms-or-justifies → Task 1 (schema/write), Task 3 (constraint), Task 4 (axis 6). ✓
- §4 provenance-blind → Task 1 (author-less write), Task 3/4 ("do not attribute" / "author-less"). ✓
- §5 fallback mandatory → Task 1 step 2 point 3; Task 7 step 5. ✓
- §6 outcome metrics on every deep worker + emit channel → Task 2, Task 5. ✓
- Trigger logic (consult-once, cap+1 recovery) → Task 1 step 2 points 1–5. ✓
- Deferred (scout, live oracle, dispatch-time flag) → out of scope, no task. ✓

**2. Placeholder scan:** every edit step carries the exact text to insert; no "TBD"/"add appropriate…". Survey threshold (`>N modules`) is intentionally a tuning value per spec, and the survey is specified to emit a boolean — not a placeholder.

**3. Type/name consistency:** `DECOMPOSITION.md`, the four metrics keys (`consulted`/`plan_critic_first_pass`/`rework_count`/`review_high`), `metrics:<crew_id>` sink, and "Orchestration consult" section name are used identically across Tasks 1–7.
