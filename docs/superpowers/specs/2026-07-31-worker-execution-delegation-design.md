# Worker execution delegation across codex and cursor — design

Date: 2026-07-31
Status: approved (pending plan)
Issue: #1

## Context

`WORKER_PROTOCOL.md` rule 1 today:

- A **claude** worker orchestrates. The session model (opus on deep) does spec / plan /
  reconcile / judging; implementation steps run as subagents capped at 3 concurrent,
  on **sonnet by default**, escalating an individual step to opus only when the plan
  tags it `implement: opus`.
- A **codex or cursor** worker is declared **single-agent** — it hand-writes every
  implementation step itself. The parenthetical says the split "is claude-specific".

That parenthetical was true when it was written and no longer is:

- **Codex** ships native subagents, on by default (`agents.enabled`), with an `[agents]`
  config block (`max_concurrent_threads_per_session`, `default_subagent_model`,
  `default_subagent_reasoning_effort`), per-spawn model and reasoning-effort overrides
  that beat those defaults, and subagents that **inherit the parent turn's sandbox and
  permission mode** — so under `--dangerously-bypass-approvals-and-sandbox` they are real
  implementers, not read-only explorers. Built-in agents: `default`, `worker`, `explorer`.
- **Cursor** shipped subagents in 2.4 — independent child agents with their own context
  window and tool access, "configured with custom prompts, tool access, and models",
  available in **both the editor and the CLI**. CLI Task-tool dispatch was missing at
  first and was fixed around March 2026; without it an agent works inline and sequentially.

So the gap is not that claude does something exotic. Claude's rule 1 **is** the pattern —
plan one rung above where you implement, escalate a plan-tagged high-risk step back up.
This spec brings codex and cursor up to it.

**Claude does not change.** Its ladder already reads the way the rule wants, on both tiers.

## Approved decisions (from the brainstorm)

1. **Execution delegation only.** Plan-critic and the code-review gate stay claude-only.
2. **Mandatory, not encouraged.** Rule 1 reads the same for every engine: you orchestrate,
   you do not hand-write the implementation yourself. Not a "should, not a blocker" like
   the orchestration consult — a codex/cursor worker that hand-writes standard/deep
   implementation steps is violating its protocol, same as a claude one.
3. **Built-in subagents, no custom agent definitions.** No `.codex/agents/*.toml`, no
   cursor subagent files, no new file kind in `gen-adapters.sh`.
4. **Constrain subagents via the spawn prompt**, rather than equipping them with a
   curated skill set.
5. **Claude `standard` stays sonnet → sonnet.** The rung-down applies where claude already
   applies it (deep), not as a new uniformity.
6. **Cursor `deep` plans on kimi and implements on Grok.**

## The ladder

| Tier       | claude                                        | codex                        | cursor                                                        |
| ---------- | --------------------------------------------- | ---------------------------- | ------------------------------------------------------------- |
| `deep`     | opus → **sonnet** → esc. opus *(unchanged)*   | sol → **terra** → esc. sol   | **kimi-k3-high** → **grok-4.5-medium-fast** → esc. **grok-4.5-high** |
| `standard` | sonnet → sonnet → esc. opus *(unchanged)*     | terra → **luna** → esc. terra | grok-4.5-medium-fast → **grok-4.5-low-fast** → esc. medium-fast |
| `trivial`  | no delegation                                 | no delegation                | no delegation                                                 |

Read each cell as *worker session → execute subagents (default) → escalated step*. The
escalation trigger is unchanged and engine-neutral: the plan's `implement: opus` tag,
which every engine reads as "this step is high-risk, run it a rung up".

`trivial` delegates nothing on every engine — rule 1 already scopes subagent execution to
standard/deep, and a tier defined as not needing decomposition does not need parallelism.

### Cursor `deep` is the one row where the escalated model is not the worker's own

Escalating a flagged step back to `kimi-k3-high` would put the planner family on an
implementation step. `dispatch-orchestration.md:109` makes cursor's value a genuinely
non-Claude **implementer**, and kimi is chosen here for the orchestration trait the repo
already credits it with ("third-family model, strong agentic tool use", where it is the
cursor *dispatcher* default). Kimi plans; Grok implements at every rung, escalation
included.

This moves cursor's `deep` worker slot in the model map from `cursor-grok-4.5-high` to
`kimi-k3-high`.

### Kimi is a `deep`-only lever

Cursor `standard` stays Grok-planned. Same logic that puts the orchestration consult and
the decomposition pass at `deep`: the extra machinery lives at the top tier.

## Effort

Effort moves one rung down for execute subagents — but only where the engine has the axis
to move it on.

| Engine | Model axis        | Effort axis                                                        |
| ------ | ----------------- | ------------------------------------------------------------------ |
| claude | Agent tool `model:` | none — the Agent tool has no per-spawn effort parameter; subagents inherit the session |
| codex  | per-spawn model   | per-spawn effort, plus `agents.default_subagent_reasoning_effort`   |
| cursor | model id          | *is* the model id (`-low`/`-medium`/`-high`, plus `-fast`)          |

Claude's missing effort axis is moot: claude does not change.

Codex rung-down, applied to the session `--effort`:

| Session `--effort` | Execute subagents |
| ------------------ | ----------------- |
| `ultra`            | `max`             |
| `max`              | `xhigh`           |
| `xhigh`            | `high`            |
| `high`             | `medium`          |
| `medium`           | `low`             |
| `low`              | `low` (floor)     |

**Escalated steps cap at `max`, never `ultra`.** `ultra` is itself maximum reasoning *with
automatic task delegation* (`DISPATCHER_PROTOCOL.md:52`), so an `ultra` execute subagent
would start orchestrating its own subagents two levels below the worker. The rung-down
already prevents this on the default path (`ultra` → `max`); the cap makes it hold on the
escalation path, where a flagged step would otherwise climb back to the session's `ultra`.
The worker keeps `ultra`; its subagents never inherit it.

## Where each knob lives

The division mirrors what `dispatch.sh` already does for claude, where **nothing** in the
launcher tells a claude worker to use sonnet subagents — `WORKER_PROTOCOL.md` does.

- **Launcher (`dispatch.sh`) owns guardrails**: things that are a pure function of the
  dispatch arguments and must not depend on a worker remembering them.
- **Protocol owns judgment**: which model implements a step, and when a step escalates.

This also keeps the `dispatch-orchestration.md:38` invariant intact — its tables stay the
only place concrete worker model versions appear. The effort rung-down carries no model
slug, so computing it in the launcher does not violate that.

### `dispatch.sh` — codex launch

Three `-c` flags, alongside the existing `service_tier` pin and for the same reason: an
unattended worker should not inherit whatever the nix-generated `--profile worker` happens
to carry.

- `-c agents.enabled=true`
- `-c agents.max_concurrent_threads_per_session=3` — the cap rule 1 already imposes
- `-c agents.default_subagent_reasoning_effort=<one rung below --effort>`

Deliberately **not** set: `agents.default_subagent_model`. That is a model slug, so it
belongs to the model map, and per-spawn model beats the config default anyway. Leaving it
unset also dodges codex's own documented default, which recommends a bare `gpt-5.6` — the
exact slug `dispatch-orchestration.md:52` records as a 400 on a ChatGPT account.

### `dispatch.sh` — spawn-prompt process authority (codex and cursor)

`WORKER_PROTOCOL.md:7` already supersedes `superpowers:using-superpowers` for
process/lifecycle skills — no `brainstorming`, `writing-plans`, `executing-plans`,
`requesting-code-review`, or `test-driven-development` as independent steps, because the
harness pipeline *is* the process. Implementation and domain skills stay available.

Execute subagents never read `WORKER_PROTOCOL.md`. They get a spawn prompt and nothing
else, and superpowers is not claude-only — it ships a codex mapping
(`Task`→`spawn_agent`, `wait_agent`, `close_agent`, `TodoWrite`→`update_plan`, "skills
load natively"), and cursor loads `SKILL.md` skills in the CLI. So a fresh subagent handed
one implementation step is the single most likely place for that step to turn into
`brainstorming` → `writing-plans` → a spec document.

The remedy is a clause in the launch prompt, carried into each spawn: the step is the
whole task, do not invoke process/lifecycle skills, do not re-plan or re-critique. The
risk is not that subagents lack skills — it is that they have unscoped ones.

The same structural gap exists for claude's execute subagents today. It is out of scope
here (claude does not change), but the clause is written to be portable if it needs to
move there later.

## Metrics semantics

`WORKER_PROTOCOL.md:143` currently explains the `null` metrics fields by calling codex and
cursor **"single-agent engines"**. That becomes wrong for both.

The `null`s stay — codex and cursor still run neither the plan-critic nor the claude
code-review gate — but the reason is rewritten: their runs are **ungated**, not
**unorchestrated**. A codex run with `plan_critic_first_pass: null` and
`review_high: null` is now an orchestrated run that no critic vetted, which is a different
thing from a one-shot. The distinction the paragraph exists to protect is unchanged: a
`null` must never read as `review_high: 0, review_mode: "full"`.

## Changes

| File | Change |
| ---- | ------ |
| `adapters/core/dispatch.sh` | three `-c agents.*` flags on the codex launch; effort rung-down mapping; process-authority clause in the codex and cursor launch prompts |
| `adapters/core/protocols/WORKER_PROTOCOL.md` | rule 1 becomes engine-general with a per-engine table; metrics paragraph rewording |
| `adapters/core/protocols/dispatch-orchestration.md` | execute-subagent column in the model map; drop "the split is claude-specific"; cursor `deep` worker model → `kimi-k3-high`; effort rung-down table |
| `adapters/core/protocols/DISPATCHER_PROTOCOL.md` | `ultra` clause (worker keeps it, subagents never inherit it); cursor `deep` model change |
| generated trees | `scripts/gen-adapters.sh` reprojects into `adapters/claude-code/` and `adapters/codex/` |
| `tests/dispatch.bats` | first assertions on the codex and cursor launch lines |

## Test plan

`tests/dispatch.bats` today asserts only the codex work-profile gate, never the launch
line itself. Add:

- codex launch carries all three `agents.*` flags
- the effort rung-down mapping, table-driven over the six `--effort` values, including the
  `low` floor
- codex and cursor launch prompts carry the process-authority clause
- claude's launch line is unchanged (regression guard — this spec touches three engines'
  docs and must not touch claude's behavior)

`tests/adapters.bats` already asserts the three trees stay in sync, so the doc edits are
covered once regenerated.

## Out of scope

- Plan-critic and the code-review gate stay claude-only.
- Claude's execute subagents — no model, effort, or prompt change.
- Custom subagent definitions for either engine.
- Cursor's `--model` alternatives (`composer-2.5`, effort-suffixed claude/gpt ids) keep
  their current accepted-and-unvalidated status.

## Open — verify on the work box

Both engines are work-profile gated (`dispatch.sh:127`) and neither CLI exists on a
personal-profile host, so none of this is testable where the spec was written.

1. **codex config key.** Current OpenAI docs give `[agents] enabled`; superpowers' codex
   mapping gives `[features] multi_agent = true`. One is likely stale — the same
   superpowers page carries a legacy note about pre-`rust-v0.115.0` tool naming
   (`wait` → `wait_agent`). Set the `agents.*` keys as primary; confirm whether
   `features.multi_agent` is also required, and whether an unknown `-c` key errors or is
   ignored.
2. **cursor per-subagent model.** The 2.4 changelog says subagents can be configured with
   models, but neither it nor the CLI forum thread documents the mechanism or a
   concurrency cap for `cursor-agent`. **If a cursor worker cannot pin a subagent model,
   the cursor column degrades to "delegate, inherit the worker's model" and
   kimi-plans/Grok-implements does not survive** — cursor `deep` would revert to
   `cursor-grok-4.5-high` throughout.
3. **kimi variants.** Whether `kimi-k3` has rungs below `-high`. Nothing depends on it
   today (cursor escalation uses Grok), but it would open a kimi-internal rung-down.
