# Deep-Tier Worker Pipeline Speedups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Speed up the deep-tier crew worker pipeline with four correctness-preserving levers: an early fast deterministic gate, a parallelized+split review gate, a conditional second re-review, and an opus-plans / sonnet-implements model split.

**Architecture:** All four levers are expressed as edits to instruction/protocol **markdown** consumed by crew workers at runtime — there is no compiled artifact. `WORKER_PROTOCOL.md` owns the pipeline stages, gate ordering, review fan-out, and rules; `spec-plan-critic/SKILL.md` owns the plan schema (the per-step `implement:` tag); `dispatch-orchestration.md` documents the model map and must stay in sync. `dispatch.sh` (the mechanism) is **not** edited — the worker session already launches at the mapped model, and the sonnet/opus execute split is internal to how the worker spawns subagents.

**Tech Stack:** Markdown protocol docs under `home/ai/claude-code/`; Nix flake (`nix flake check` is an evaluation smoke test only — see caveat below); Claude Code Agent tool (`model:` param drives the execute-subagent split); the crew bus CLI.

> **Verification-gate reality (plan-critic note 1):** `nix flake check` is effectively a **no-op for these three edits** — none of the edited files' _content_ is read by Nix. `WORKER_PROTOCOL.md` and `dispatch-orchestration.md` aren't referenced in `default.nix`; `spec-plan-critic/SKILL.md` is symlinked by directory _name_ only (content never evaluated). So flake check only catches a broken skill _directory_, not prose. Treat it as a bare evaluation smoke test. The **real gate is Task 7 Step 3's internal-consistency dry review** — lean on that.

## Global Constraints

Copy these verbatim into every task's mental model — they are invariants that must survive unchanged:

- **Revision cap = 2.** The spec-plan-critic revision cap and the review→fix loop cap both stay at 2. The conditional second re-review only ever _removes_ a pass, never adds a third.
- **Checkpoint-peek seams preserved.** The non-blocking peek at each seam (after spec, after plan, after execute, after review) stays; the fast gate adds one new seam, it removes none.
- **Crew bus status contract preserved.** The `## Report to the bus` section (working / pr_open / done / blocked / failed statuses and the `crew status`/`crew msg`/`crew await` calls) is **not** touched by this change.
- **receiving-code-review discipline preserved** everywhere findings/directives are ingested.
- **Order before push preserved:** code-review gate → `/deslop` → `git push`; the pre-push gate still fires on push (rule 4 unchanged).
- **Language-agnostic wording.** The fast gate must never hardcode `go test` — it describes how the worker _discovers_ the changed-package command from repo conventions.
- **Model split is claude-specific.** The opus/sonnet split applies to claude workers (Agent-tool `model:` param). Codex workers scale reasoning effort by tier instead — say so, don't imply a codex model split.
- **Personal-repo git flow:** conventional commits, PR assigned to `@me`, `Closes #75` in the PR body.
- **The three edited files cross-reference each other** — `WORKER_PROTOCOL.md` rule 1, `spec-plan-critic/SKILL.md`, and `dispatch-orchestration.md` must all spell the tag identically as `implement: opus`.
- **Deprecated workflow path is out of scope (plan-critic note 2).** `home/ai/claude-code/workflows/spec-plan-critic.js` carries its own plan-draft prompt that will NOT learn the `implement: opus` tag. SKILL.md explicitly deprecates it ("use the Agent tool directly — never Workflow"), so it's off the live worker route. Leave it as-is; do not edit it. Just be aware Task 5 does not cover it.
- **Commits will trip the prettier pre-commit dance.** This repo runs prettier/alejandra/statix/deadnix as pre-commit hooks; the first `git commit` in each task may fail because a hook reformatted a file — `git add` the reformatted file and re-run the same commit (per CLAUDE.md). Not an error; expected once per commit.

**Deployment note (affects verification only):** `WORKER_PROTOCOL.md` is read live at dispatch time via an absolute `~/nix-config/...` path (`dispatch.sh` line 214); the skill/workflow files are `mkOutOfStoreSymlink`s pointing at the **main** `~/nix-config` checkout (see `home/ai/claude-code/default.nix`), not the worktree. So these markdown edits take effect **only once the branch is merged to main** — a rebuild from the worktree does not deploy them (the "skill symlink worktree gotcha"). Because `dispatch.sh` is unchanged, no rebuild is required for behavior at all. `nix flake check` is an evaluation smoke test (no-op for prose — see the verification-gate caveat above); the real pre-merge gate is the internal-consistency dry review. Behavioral verification (dispatch a live deep worker) is only possible post-merge.

---

## File Structure

- **Modify:** `home/ai/claude-code/WORKER_PROTOCOL.md` — Tasks 1–4 (pipeline-by-tier bullets, checkpoint-peek seam list, new fast-gate section, rewritten review gate, rule 1).
- **Modify:** `home/ai/claude-code/skills/spec-plan-critic/SKILL.md` — Task 5 (plan draft step: `implement: opus` tag convention).
- **Modify:** `home/ai/claude-code/dispatch-orchestration.md` — Task 6 (model-split note + stale `dispatch.fish`→`dispatch.sh` reference; keep in sync).
- **Not modified (verified in Task 6):** `home/ai/claude-code/dispatch.sh`.

Tasks 1–4 all touch `WORKER_PROTOCOL.md`; run them sequentially and **re-Read the target region immediately before each Edit** (the file changes under you between tasks). Each lever is independently reviewable, which is why they are separate tasks.

---

### Task 1: Fast deterministic gate (WORKER_PROTOCOL.md)

Insert the new pipeline seam between execute and the code-review gate, wire it into the pipeline-by-tier summary, and add it to the checkpoint-peek seam list.

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (pipeline-by-tier bullets ~11–13; checkpoint-peek seam line ~16; new section before `## Code review gate` ~26)

**Interfaces:**

- Produces: a named stage **"fast deterministic gate"** that Task 2's review gate consumes ("re-run the fast deterministic gate") and Task 3's conditional re-review reads ("fast gate is green"). Spell it consistently as **fast deterministic gate** / **fast gate**.

- [ ] **Step 1: Re-read the target regions**

Run: `Read` lines 10–24 and 26 of `home/ai/claude-code/WORKER_PROTOCOL.md` to confirm current text before editing.

- [ ] **Step 2: Rewrite the `standard` and `deep` pipeline-by-tier bullets**

Replace (lines ~12–13):

```markdown
- **standard** — run the `spec-plan-critic` workflow with `{ tier: 'standard', ... }` (plan + plan-critic only), then execute the returned plan via subagents, then the code-review gate (one pass), then gate + PR.
- **deep** — run `spec-plan-critic` with `{ tier: 'deep', ... }` (spec + spec-critic, then plan + plan-critic), then execute, then the code-review gate (adversarial), then gate + PR.
```

with:

```markdown
- **standard** — run the `spec-plan-critic` workflow with `{ tier: 'standard', ... }` (plan + plan-critic only), then execute the returned plan via subagents, then the **fast deterministic gate** (build+vet+lint+unit on changed packages, looped to green), then the code-review gate (one pass), then `/deslop` + push + PR.
- **deep** — run `spec-plan-critic` with `{ tier: 'deep', ... }` (spec + spec-critic, then plan + plan-critic), then execute, then the **fast deterministic gate**, then the code-review gate (one parallel review batch, reconciled once, then a **conditional** second re-review), then `/deslop` + push + PR.
```

- [ ] **Step 3: Add the fast-gate seam to the checkpoint-peek list**

Replace (line ~16):

```markdown
At each pipeline **seam** — after spec, after plan, after execute, after review, **before** sinking cost into the next stage — do a non-blocking peek for a dispatcher stop/redirect directive:
```

with:

```markdown
At each pipeline **seam** — after spec, after plan, after execute, after the fast gate, after review, **before** sinking cost into the next stage — do a non-blocking peek for a dispatcher stop/redirect directive:
```

- [ ] **Step 4: Insert the new `## Fast deterministic gate` section immediately before `## Code review gate (standard/deep)`**

Insert:

```markdown
## Fast deterministic gate (standard/deep)

After `execute` and **before** any model reviewer sees the diff, run the cheap deterministic checks and loop the worker to green. A test settles deterministically what a reviewer would otherwise re-litigate probabilistically (the "AC2 would fail if run" churn), and it moves any build/test failure _ahead_ of the expensive review instead of after it.

- **Discover the command from the repo — never assume a language.** This protocol serves any repo (Go today, others tomorrow), so do not hardcode `go test`. Read the repo's own conventions to find its build + vet/lint + unit commands: a `justfile`/`Makefile` target, `package.json` scripts, the pre-commit / CI config (`.pre-commit-config*`, `.github/workflows`, `treefmt`, `nix flake check`), or a project `verify` skill if one exists.
- **Scope to changed packages.** Get the changed files with `git diff --name-only <base>...HEAD` (base = the branch's merge-base with the default branch), map each to its module/package, and run build + vet/lint + unit scoped to just those — not the whole repo.
- **Loop to green here, cheaply.** On failure, fix (delegate to a subagent per rule 1) and re-run the scoped gate until green. This deterministic loop is **separate from and independent of** the review→fix loop (whose cap is 2, below) — it has no cap of its own.
- **Record the green result** — the reviewer batch consumes it (below) so reviewers verify against deterministic evidence, not speculation.
- If the change has **no runnable build/test surface** (a docs- or protocol-only diff), say so explicitly and fall through to the review gate — do not invent a command.
```

- [ ] **Step 5: Verify the edit is internally consistent**

Run: `rg -n "fast deterministic gate|fast gate|after the fast gate" home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: matches in the standard bullet, the deep bullet, the checkpoint-peek seam line, and the new section heading + body.

- [ ] **Step 6: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): add fast deterministic gate before code review"
```

---

### Task 2: Parallelize + split the review gate (WORKER_PROTOCOL.md)

Rewrite the `## Code review gate` section so the language reviewer, targeted test-runner, and (deep/work) codex-diverse reviewer run in ONE parallel batch, reconciled once, with a diff-scoped conditional security reviewer. Leave the deep re-review as a simple "once" bullet — Task 3 makes it conditional.

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (the `## Code review gate (standard/deep)` section, currently lines ~26–40)

**Interfaces:**

- Consumes: the **fast deterministic gate** from Task 1 ("re-run the fast deterministic gate").
- Produces: a bullet **"Scale the re-review by tier"** whose `deep` sub-bullet Task 3 replaces; the `standard` sub-bullet is final.

- [ ] **Step 1: Re-read the current review-gate section**

Run: `Read` the `## Code review gate (standard/deep)` section of `home/ai/claude-code/WORKER_PROTOCOL.md` to capture the exact current text (it starts at "After implementation is complete…").

- [ ] **Step 2: Replace the entire section body (from the heading through the "Cap the review→fix loop at 2" bullet) with the parallel-batch version**

Replace with:

```markdown
## Code review gate (standard/deep)

After the fast deterministic gate is green and **before** `/deslop` + push, get an **independent** review of your diff — never review your own work in your own context (same reason the critics run fresh; rule 2).

- **Dispatch the review as a single parallel batch** — one message, concurrent subagents — then reconcile once. Do not run reviewers serially.
  - **Language reviewer** — the agent matching the changed files' language (`go-reviewer`, `python-reviewer`, `typescript-reviewer`, `shell-reviewer`, the SQL reviewers, …). If none fits, a general agent running the `find-bugs` skill.
  - **Targeted test-runner** — a subagent that runs the change's acceptance-criteria / behavior-specific tests and reports pass/fail. This is what deterministically settles a reviewer's "this would fail if run" claim; its result feeds the reconcile.
  - **codex-diverse reviewer (deep tier, work profile, claude implementers only)** — a `codex-diverse` subagent (reuse/adapt the `pr-reviewers` `codex-reviewer`) that carries the `mcp__codex__*` tools and drives the read-only `codex` MCP server. This is a **should, not a blocker**: if the codex MCP server is unavailable, you are a codex implementer (codex→claude is not yet supported), or you are off the work profile, drop it and fall back to same-engine review — never stall the gate on a missing diverse engine.
  - **Security reviewer (conditional, both tiers)** — include a `security-reviewer` subagent **only if** the diff touches an auth, crypto, input-parsing, SQL, or network path. Conservative trigger: when in doubt, include it. Otherwise skip it.
- **Reconcile once.** Merge findings across the batch (both / language-only / codex-only / security), de-duplicated and checked against the test-runner's deterministic result. Ingest with **receiving-code-review** discipline: verify each finding before acting, don't perform agreement. Fix the real ones (delegate per rule 1), then re-run the **fast deterministic gate**.
- **Scale the re-review by tier:**
  - `standard` — one pass. No second review.
  - `deep` — review, fix, then re-review the fixes once.
- **Cap the review→fix loop at 2.** Anything still unresolved goes in the PR body under "## Review notes" — never silently drop it.
```

- [ ] **Step 3: Verify the reconcile references the fast gate and the batch members**

Run: `rg -n "parallel batch|Targeted test-runner|Security reviewer|re-run the \*\*fast deterministic gate|Scale the re-review" home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: the batch bullets, the reconcile-to-fast-gate line, and the "Scale the re-review by tier" bullet all present.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): parallelize review gate with diff-scoped security lens"
```

---

### Task 3: Conditional second re-review (WORKER_PROTOCOL.md)

Replace the `deep` sub-bullet under "Scale the re-review by tier" so the second pass is conditional on HIGH findings non-trivially fixed AND a green fast gate, while keeping the cap at 2.

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (the `deep` sub-bullet produced by Task 2)

**Interfaces:**

- Consumes: the **fast deterministic gate** (green check) from Task 1, and the "Scale the re-review by tier" bullet from Task 2.

- [ ] **Step 1: Re-read the "Scale the re-review by tier" bullet**

Run: `Read` the review-gate section to confirm the Task 2 text is present and locate the `deep` sub-bullet.

- [ ] **Step 2: Replace the `deep` sub-bullet**

Replace:

```markdown
- `deep` — review, fix, then re-review the fixes once.
```

with:

```markdown
- `deep` — **conditional** second re-review. After the fix + a green fast deterministic gate, run the second review pass **only if** the first pass produced **HIGH-severity** findings that were **non-trivially fixed** (the fix changed real logic — not a comment, rename, or doc tweak). If there were no HIGH findings, or they were only trivially fixed, or the fast gate is not green, **skip** the second pass and proceed to `/deslop` + push. This conditional only ever _removes_ the second pass — the review→fix loop is still capped at 2 (never a third).
```

- [ ] **Step 3: Verify cap-2 and conditional wording coexist**

Run: `rg -n "conditional|HIGH-severity|non-trivially fixed|capped at 2|Cap the review→fix loop at 2" home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: the conditional deep bullet AND the unchanged "Cap the review→fix loop at 2" bullet both appear.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): make deep second re-review conditional on HIGH fixes"
```

---

### Task 4: Model split — opus plans, sonnet implements (WORKER_PROTOCOL.md rule 1)

Rewrite rule 1 so the worker session (opus on deep) does spec/plan/reconcile/judging, while execute subagents default to sonnet, escalating a step to opus only when the plan tags it `implement: opus`.

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md` (rule 1, line ~62)

**Interfaces:**

- Consumes: the plan schema tag `implement: opus` defined in Task 5.
- Produces: the canonical `implement: opus` spelling that Tasks 5 and 6 must match.

- [ ] **Step 1: Re-read the `## Rules` block**

Run: `Read` lines ~61–68 of `home/ai/claude-code/WORKER_PROTOCOL.md`.

- [ ] **Step 2: Replace rule 1**

Replace:

```markdown
1. **Delegate execution.** For standard/deep, implementation steps run as subagents (subagent-driven-development), capped at 3 concurrent. You orchestrate; you do not hand-write the implementation yourself.
```

with:

```markdown
1. **Delegate execution — opus plans, sonnet implements.** The worker session (opus on deep) does spec / plan / reconcile / judging. For standard/deep, implementation steps run as subagents (subagent-driven-development), capped at 3 concurrent — and for **claude** workers those execute subagents run on **sonnet by default**, spawned with the Agent tool's `model: sonnet`. Escalate an individual step to opus **only** when the plan tags it `implement: opus` (a high-risk step — see the plan schema in `spec-plan-critic`). You orchestrate; you do not hand-write the implementation yourself. (Codex workers scale reasoning effort by tier instead — this model split is claude-specific.)
```

- [ ] **Step 3: Verify the tag spelling**

Run: `rg -n "implement: opus|sonnet by default|opus plans" home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: rule 1 shows `sonnet by default`, `implement: opus`, and the claude-specific caveat.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): opus orchestrates, sonnet implements by default"
```

---

### Task 5: Plan schema `implement:` tag (spec-plan-critic/SKILL.md)

Document the per-step `implement: opus` tag in the plan-draft step so the plan the worker produces can carry it, and the plan-critic understands it.

**Files:**

- Modify: `home/ai/claude-code/skills/spec-plan-critic/SKILL.md` (the "Draft plan" step under "Standard tier", line ~19)

**Interfaces:**

- Consumes: the `implement: opus` convention (must match Task 4 exactly).

- [ ] **Step 1: Re-read the "Standard tier (plan only)" block**

Run: `Read` lines ~15–27 of `home/ai/claude-code/skills/spec-plan-critic/SKILL.md`.

- [ ] **Step 2: Add the tag convention as a sub-bullet under the "Draft plan" step**

Replace:

```markdown
1. **Draft plan** — spawn an agent with the `superpowers:writing-plans` skill to produce a bite-sized implementation plan. Prompt: `"Write a bite-sized implementation plan using writing-plans discipline.\n\nTASK:\n<task>"`
```

with:

```markdown
1. **Draft plan** — spawn an agent with the `superpowers:writing-plans` skill to produce a bite-sized implementation plan. Prompt: `"Write a bite-sized implementation plan using writing-plans discipline.\n\nTASK:\n<task>"`
   - **Per-step implement-model tag.** The worker executes plan steps on **sonnet by default**. A genuinely high-risk step (subtle concurrency, security-sensitive, wide-blast refactor) may carry an inline `implement: opus` tag so the worker spawns _that_ step's subagent on opus. No tag = sonnet. Tell the drafting agent to add it **sparingly** — most steps stay untagged. Example step line: `- [ ] **Step 3: rework the token-refresh lock** (implement: opus)`.
```

- [ ] **Step 3: Verify the tag matches rule 1**

Run: `rg -n "implement: opus" home/ai/claude-code/skills/spec-plan-critic/SKILL.md home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: identical `implement: opus` spelling in both files.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/skills/spec-plan-critic/SKILL.md
git commit -m "feat(spec-plan-critic): add per-step implement: opus plan tag"
```

---

### Task 6: Sync dispatch-orchestration.md and confirm dispatch.sh is untouched

Document the worker-session-model vs execute-subagent-model split in the orchestration doc (keep it in sync with WORKER_PROTOCOL.md rule 1), correct the stale `dispatch.fish` reference to `dispatch.sh`, and verify `dispatch.sh` needs no change.

**Files:**

- Modify: `home/ai/claude-code/dispatch-orchestration.md` (opening line ~3; add a note after the model-map notes ~48)
- Verify only: `home/ai/claude-code/dispatch.sh`

**Interfaces:**

- Consumes: rule 1 wording and the `implement: opus` tag (must match Tasks 4 and 5).

- [ ] **Step 1: Confirm dispatch.sh needs no edit**

Run: `rg -n "model|--model|opus|sonnet" home/ai/claude-code/dispatch.sh`
Expected: model appears only as `$model` (positional arg 2) and `--model $model` / `-m $model` on the launch lines — there is **no** per-subagent model logic in the mechanism. The worker session launches at the mapped model; the sonnet/opus execute split lives entirely in the worker protocol + Agent tool. Conclusion: **no dispatch.sh edit** (record this in the final diff check).

- [ ] **Step 2: Re-read the head and model-map tail of dispatch-orchestration.md**

Run: `Read` lines 1–3 and 39–48 of `home/ai/claude-code/dispatch-orchestration.md`.

- [ ] **Step 3: Correct the stale mechanism reference on line ~3**

Replace:

```markdown
Canonical reference for how the dispatcher judges a task into **tier**, **engine**, and **model**. Keep it in sync with `DISPATCHER_PROTOCOL.md` (the baked rubric) and `dispatch.fish` (the mechanism).
```

with:

```markdown
Canonical reference for how the dispatcher judges a task into **tier**, **engine**, and **model**. Keep it in sync with `DISPATCHER_PROTOCOL.md` (the baked rubric) and `dispatch.sh` (the mechanism).
```

- [ ] **Step 4: Add the model-split note after the model-map notes**

After the paragraph ending `…the map is enforced by the dispatcher's judgment, not by code.` (line ~48), insert:

```markdown
**Worker-session model vs execute-subagent model (claude).** The `<model>` in the map above is the **worker session** model — it does spec / plan / reconcile / judging (opus on deep). The worker's **execute subagents default to sonnet**, escalating a single step to opus only when the plan tags it `implement: opus`. So a deep claude worker is opus-orchestrated but sonnet-implemented by default. See `WORKER_PROTOCOL.md` rule 1 (and the fast deterministic gate + parallel review gate it now describes). Codex workers scale reasoning effort by tier instead — the split is claude-specific.
```

- [ ] **Step 5: Verify cross-file consistency**

Run: `rg -n "implement: opus|sonnet|dispatch.sh|worker session" home/ai/claude-code/dispatch-orchestration.md`
Expected: the new note present, `dispatch.fish` no longer appears, `implement: opus` spelled identically to Tasks 4/5.

- [ ] **Step 6: Commit**

```bash
git add home/ai/claude-code/dispatch-orchestration.md
git commit -m "docs(dispatch): document opus/sonnet worker split, fix dispatch.sh ref"
```

---

### Task 7: Final consistency review, flake check, and PR

**Files:** none (verification + PR only).

- [ ] **Step 1: Run the correctness gate (nix-config pre-push gate)**

Run: `cd /home/noams/nix-config-worktrees/feat-75-worker-pipeline-speedups && nix flake check`
Expected: success. This proves the flake still evaluates (nothing in `default.nix`'s references to these files broke) and treefmt/pre-commit hooks are clean. (Markdown is not in this repo's treefmt formatter set, so `.md` content is not reformatted — the check validates evaluation, not prose style.)

- [ ] **Step 2: Confirm the diff scope**

Run: `git diff --stat main...HEAD`
Expected: exactly three files changed — `home/ai/claude-code/WORKER_PROTOCOL.md`, `home/ai/claude-code/skills/spec-plan-critic/SKILL.md`, `home/ai/claude-code/dispatch-orchestration.md`. **`dispatch.sh` must NOT appear.**

- [ ] **Step 3: Internal-consistency dry review** — read the edited `WORKER_PROTOCOL.md` top-to-bottom and confirm every referenced seam/stage still connects:
  - checkpoint-peek seam list includes "after the fast gate";
  - `## Fast deterministic gate` section sits between `## Checkpoint-peek` and `## Code review gate`;
  - pipeline-by-tier `standard` + `deep` bullets both name the fast gate;
  - review gate describes a single parallel batch + reconcile-once + the conditional deep re-review, and still says "Cap the review→fix loop at 2";
  - the security-reviewer trigger (auth/crypto/input-parsing/SQL/network) is present;
  - rule 1 states `sonnet by default` / `implement: opus` and the claude-specific caveat;
  - order before push (rule 4: gate → `/deslop` → `git push`) is unchanged;
  - the `## Report to the bus` crew-status contract is unchanged;
  - `implement: opus` is spelled identically across all three files (`rg -n "implement: opus" home/ai/claude-code/`).

  Expected: every check passes; fix inline if any reference dangles.

- [ ] **Step 4: Deslop the branch, then push**

```bash
/deslop
git push -u origin feat/75-worker-pipeline-speedups
```

Expected: `/deslop` clean (or applied); push triggers the pre-push hook and passes.

- [ ] **Step 5: Open the PR**

```bash
gh pr create --assignee @me \
  --title "feat(worker): speed up deep-tier pipeline (fast gate, parallel review, conditional re-review, opus/sonnet split)" \
  --body "Implements the four correctness-preserving levers from #75:

1. Fast deterministic gate (build+vet+lint+unit on changed packages) before any model reviewer — language-agnostic discovery, no hardcoded \`go test\`.
2. Review gate runs language reviewer + targeted test-runner + (deep/work) codex-diverse concurrently in one batch, reconciled once; diff-scoped conditional security reviewer.
3. Deep second re-review is conditional on HIGH findings non-trivially fixed AND a green fast gate; cap stays 2.
4. Opus plans / sonnet implements: worker session (opus) orchestrates, execute subagents default to sonnet, per-step \`implement: opus\` escalation.

Kept in sync: WORKER_PROTOCOL.md ⇄ dispatch-orchestration.md ⇄ spec-plan-critic. dispatch.sh unchanged (worker already launches at mapped model; split is internal). Invariants preserved: checkpoint-peek seams, revision cap 2, crew bus status contract, receiving-code-review, /deslop before push, pre-push gate on push.

Verification: \`nix flake check\` green + internal-consistency dry review (no runtime test harness exists for protocol markdown). Behavioral verification is post-merge only — the .md files are read live from the main checkout at dispatch time.

Closes #75"
```

Expected: PR created and assigned to @me.

---

## Overall Verification

There is **no unit test** for protocol markdown — do not invent a harness. Verification is:

1. **`nix flake check`** in the worktree (Task 7 Step 1) — the nix-config correctness/pre-push gate. Proves the flake still evaluates and treefmt/pre-commit are clean.
2. **Diff-scope check** (Task 7 Step 2) — exactly the three markdown files; `dispatch.sh` untouched.
3. **Internal-consistency dry review** (Task 7 Step 3) — every referenced seam/stage connects; all invariants from Global Constraints survive; the `implement: opus` tag is spelled identically in all three files.
4. **Deploy reality** — these files are read live from the **main** `~/nix-config` checkout at dispatch/worker runtime (absolute path + out-of-store symlinks), so the change goes live only on **merge to main**; a worktree rebuild does not deploy them, and no rebuild is needed since `dispatch.sh` is unchanged. Behavioral verification (dispatch a real deep worker and watch the new stage ordering) is therefore a **post-merge** step, not part of this branch's gate.

---

## Self-Review

- **Spec coverage** — Lever 1 (fast gate) → Task 1; Lever 2 (parallel + split + security lens) → Task 2; Lever 3 (conditional re-review) → Task 3; Lever 4 (model split) → Tasks 4+5+6; sync constraint → Task 6; verification stance → Task 7 + Overall Verification. All four levers, both cross-referenced docs, and the plan schema are covered.
- **Placeholder scan** — every code step shows exact old/new markdown; no "TBD"/"add appropriate…".
- **Type/name consistency** — the stage is named **fast deterministic gate** / **fast gate** everywhere; the tag is **`implement: opus`** in Tasks 4, 5, 6 and the consistency grep in Task 7.
