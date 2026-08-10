---
name: spec-plan-critic
description: Write a plan (standard tier) or spec+plan (deep tier), each gated by an adversarial critic with a 2-revision cap. Use when you have a task description and need a reviewed implementation plan before coding.
---

# spec-plan-critic

Orchestrate plan (and optional spec) writing with adversarial critic gates: draft, adversarially
critique, revise up to 2 times, then accept or escalate.

## Inputs (from task context or args)
- `tier`: `standard` or `deep`
- task description (from WORKER_TASK.md or user message)
- repo path

## Standard tier (plan only)

The worker invokes this skill only when a plan is **required** — it gates on the "Plan of record" check in `WORKER_PROTOCOL.md` first, so a pre-specified task never reaches here.

Run these steps in order:

1. **Draft plan** — using `writing-plans` discipline (or the closest equivalent this engine has), produce a bite-sized implementation plan. Prompt: `"Write a bite-sized implementation plan using writing-plans discipline.\n\nTASK:\n<task>"`
   - **Per-step implement-model tag.** The worker executes plan steps on the **default** execute model/effort for its tier. A genuinely high-risk step may carry an inline `implement: <escalated>` tag so the worker executes *that* step at the escalated rung instead. The bar is narrow — **only** subtle concurrency, security-sensitive logic, or a wide-blast-radius refactor qualifies. Do **not** tag pure CSS/layout/styling, straightforward refactors, or test-only steps — fiddly is not high-risk. No tag = default rung. Tell the drafting step to tag **sparingly**; most steps stay untagged. Example step line: `- [ ] **Step 3: rework the token-refresh lock** (implement: escalated)`.
   - **Decomposition constraint (if present).** If a `DECOMPOSITION.md` exists at the repo root, pass its full contents to the drafting step as a **hard constraint**: every plan step must map to exactly one `component`; step order must respect `ordering`; no step may touch outside its component's `boundaries`; the declared `interfaces` must be preserved. Any deviation must be justified inline in the plan. Do **not** attribute the decomposition to any author — treat it as the task's given structure.

2. **Critique** — run the `plan-critic` role in a **fresh context**: a separate agent where the engine supports spawning one, otherwise a clean single-context pass that reviews the draft as adversarially as it can. Expect structured output with `verdict` (`accept` / `revise` / `reject`) and `blocking[]` findings.
   - **What isolation buys you, and what you lose without it.** A fresh-context critic has no stake in the draft it's reviewing, so it catches what the author is blind to. Where the engine can't isolate — the critic runs in the same context as the draft — the critic is no longer independent of the author: it will be more agreeable and more likely to rubber-stamp its own reasoning. Read its verdict more skeptically in that case. Either way, the revision cap and escalate-on-exhaustion rule below still bind.

3. **If `revise` (with ≥1 `blocking` finding) or `reject`** — revise the plan. For `revise`, incorporate **only the blocking** findings; for `reject` (the plan is fundamentally unsound), revise to address the reject rationale wholesale. Then re-run step 2. Cap at **2 revisions total**. A `revise`/`accept` verdict that carries only **non-blocking `notes[]`** does **not** trigger a revision round: apply those notes at the implementer's discretion during execution and proceed. (The `plan-critic` guarantees `revise` ⇒ ≥1 blocking finding — see that agent.)

4. **If still unresolved after 2 revisions** — surface the escalation; add it to the PR body under `## Escalated`. Do not silently proceed. A plan the critic still `reject`s after the cap must **not** be executed — escalate and stop.

5. **Return** the final plan text and any escalations to the caller.

## Deep tier (spec then plan)

Same as standard but run a spec phase first:

1. Spec draft → `spec-critic` (same fresh-context rule as step 2 above) → up to 2 revisions
2. Feed accepted spec into plan phase (same loop above)

## Rules

- **Critics run independent of the author wherever the engine allows it** — a separate agent context, never a self-review folded into the same pass that wrote the draft. Where the engine has no such mechanism, see the isolation note in step 2.
- **Ingest critic verdicts with receiving-code-review discipline** — verify each finding before acting; don't perform agreement.
- **Revision cap is 2** — stop and escalate if the critic hasn't accepted after 2 passes.
