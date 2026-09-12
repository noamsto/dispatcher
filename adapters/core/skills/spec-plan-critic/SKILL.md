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

For behavioral bugs or shared contract changes, read `EVIDENCE_REVIEW.md` from
`$DISPATCHER_PROTOCOL_DIR`, falling back to the adapter-local `protocols/`
directory. Include its regression-proof steps and consumer map in the draft.

Run these steps in order:

1. **Draft plan** — using `writing-plans` discipline (or the closest equivalent this engine has), produce a bite-sized implementation plan. Prompt: `"Write a bite-sized implementation plan using writing-plans discipline.\n\nTASK:\n<task>"`
   - **Per-step implement-model tag.** The worker executes plan steps on the **default** execute model/effort for its tier. A genuinely high-risk step may carry an inline `implement: <escalated>` tag so the worker executes *that* step at the escalated rung instead. The bar is narrow — **only** subtle concurrency, security-sensitive logic, or a wide-blast-radius refactor qualifies. Do **not** tag pure CSS/layout/styling, straightforward refactors, or test-only steps — fiddly is not high-risk. No tag = default rung. Tell the drafting step to tag **sparingly**; most steps stay untagged. Example step line: `- [ ] **Step 3: rework the token-refresh lock** (implement: escalated)`.
   - **Decomposition constraint (if present).** If a `DECOMPOSITION.md` exists at the repo root, pass its full contents to the drafting step as a **hard constraint**: every plan step must map to exactly one `component`; step order must respect `ordering`; no step may touch outside its component's `boundaries`; the declared `interfaces` must be preserved. Any deviation must be justified inline in the plan. Do **not** attribute the decomposition to any author — treat it as the task's given structure.

2. **Critique** — run the `plan-critic` role in a **fresh context**, spawned per the table below. Expect structured output with `verdict` (`accept` / `revise` / `reject`) and `blocking[]` findings.
   - **The critics themselves ship with the harness.** Bodies live at `$DISPATCHER_CRITICS_DIR/*.md`, falling back to the adapter-local `critics/` when that variable is unset (a non-Nix install) — inside the plugin tree on codex, beside `commands/` on cursor; on claude they are the plugin's own `spec-critic` / `plan-critic` agents. A body **is** the role brief — it is read, never paraphrased, which is what makes the gate the same text on every engine.
   - **Spawn it per your engine.** The roles are identical everywhere; the engine only decides the mechanism and which rung critiques:

     | engine | critic mechanism | critic rung |
     | ------ | ---------------- | ----------- |
     | **claude** | the named `spec-critic` / `plan-critic` agent | unchanged — each agent definition owns its model |
     | **codex** | native subagent (`agents.enabled`, cap 3) with the roster body written into its prompt — codex has no named-agent registry, so the roster entry **is** the prompt | the tier's **escalate** rung (deep → `gpt-5.6-sol`, standard → `gpt-5.6-terra`); effort is whatever `dispatch` pinned, since codex has no per-spawn override |
     | **cursor** | Task-tool subagent with an explicit model slug, the same roster body inline | the tier's **escalate** slug (deep → `cursor-grok-4.6-high`, standard → `cursor-grok-4.6-medium`) |

     The escalate rung, not the worker's own: a critic that cannot out-think the draft it gates agrees with it. That is what claude's static `model: opus` pin has always encoded — the other two engines now read the same way off the model map in `dispatch-orchestration.md`.
   - **What isolation buys you, and what you lose without it.** A fresh-context critic has no stake in the draft it's reviewing, so it catches what the author is blind to. All three engines can spawn one, so a critic sharing the author's context is a **degraded fallback**, taken only when the spawn is refused: it is no longer independent of the author, so it will be more agreeable and more likely to rubber-stamp its own reasoning — read that verdict more skeptically, and say so in the escalation if one follows. Either way, the revision cap and escalate-on-exhaustion rule below still bind.

3. **If `revise` (with ≥1 `blocking` finding) or `reject`** — revise the plan. For `revise`, incorporate **only the blocking** findings; for `reject` (the plan is fundamentally unsound), revise to address the reject rationale wholesale. Then re-run step 2. Cap at **2 revisions total**. A `revise`/`accept` verdict that carries only **non-blocking `notes[]`** does **not** trigger a revision round: apply those notes at the implementer's discretion during execution and proceed. (The `plan-critic` guarantees `revise` ⇒ ≥1 blocking finding — see that roster entry.)

4. **If still unresolved after 2 revisions** — surface the escalation; add it to the PR body under `## Escalated`. Do not silently proceed. A plan the critic still `reject`s after the cap must **not** be executed — escalate and stop.

5. **Return** the final plan text and any escalations to the caller.

## Deep tier (spec then plan)

Same as standard but run a spec phase first:

1. Spec draft → `spec-critic` (same roster, spawn table and fresh-context rule as step 2 above) → up to 2 revisions
2. Feed accepted spec into plan phase (same loop above)

## Rules

- **Critics run in a context independent of the author** — a separate subagent, never a self-review folded into the same pass that wrote the draft. Every engine has a mechanism for it (the table in step 2); a same-context pass is the degraded fallback for a refused spawn, not a per-engine default.
- **Ingest critic verdicts with receiving-code-review discipline** — verify each finding before acting; don't perform agreement.
- **Revision cap is 2** — stop and escalate if the critic hasn't accepted after 2 passes.
