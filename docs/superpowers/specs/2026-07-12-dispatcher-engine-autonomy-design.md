# Dispatcher engine autonomy, outcome logging, and cross-model review

**Status:** design — revised after adversarial review (spec-critic REVISE, 2026-07-12)
**Date:** 2026-07-12
**Repo:** nix-config (personal / GitHub)
**Artifacts touched:** `home/ai/claude-code/DISPATCHER_PROTOCOL.md`, `home/ai/claude-code/dispatch-orchestration.md`, `home/ai/claude-code/commands/dispatcher.md`, `home/ai/claude-code/WORKER_PROTOCOL.md`, `home/ai/claude-code/crew.sh`, `home/terminal/fish/default.nix`, nix plugin + MCP provisioning.

## Problem

The dispatcher harness treats the worker **engine** as a manual opt-in flag (`dispatch … --agent codex`), so codex never runs unless the human remembers to type it — in practice, never. The dispatcher should judge the engine autonomously, the same way it already judges tier and model. Three follow-on gaps surface once engine is a real lever:

1. There is **no data** on which engine/model wins for which task shape (verified: the repo has zero benchmarks, latency, or outcome records — routing is qualitative heuristic, and the fan-out cap comment even admits "the old fixed ≤4 was an unmeasured guess"). Autonomous routing should start capturing lightweight signal so the heuristic can improve.
2. The review gate reviews **same-family** (a claude worker reviews with claude subagents; a codex worker with codex), so the second engine's different blind spots never get exercised where they pay off most — review.
3. The codex model is **hard-pinned to `gpt-5.5`** in prose, which rots every time OpenAI ships a new model; the dispatcher should choose across gpt models the way it chooses across claude models.

## Decisions (locked with the user)

1. **Engine is a co-equal judged lever, no default.** Every task → `{tier, engine, model}`. claude ⇄ codex weighed equally by task fit, not a manual flag.
2. **Outcome logging rides the existing crew bus** (single `events.jsonl`); no new coordination system. Explicitly **not** MCP or A2A — the local, single-machine, token-free file bus is the correct layer.
3. **No single gpt pin** — a maintained model map holds two model ladders (claude + codex), tier→model; prose references the map, never a version string.
4. **Deep-tier cross-model review** — on the **work profile**, a deep worker's review gate should include an engine different from the implementer (currently only the **claude→codex** direction is proven; see §4), findings reconciled. Where a diverse engine isn't available or proven (personal profile; codex→claude until validated), the deep worker falls back to same-engine adversarial review — it never blocks. standard/trivial review unchanged.
5. **Fable is in, as a gated `deep`-only escalation above opus** — not a peer of opus.

## Non-goals

- Cross-model review for standard/trivial tiers.
- Cross-model `/design` or `/audit` (only review).
- Any auto-selection driven by the logged data — the human reviews the data; the rubric stays a human-tuned heuristic.
- Cross-repo rollup of the outcome log (the bus is per-repo; a global sink is a later, separate change).
- Capturing codex's async GitHub-App review findings into the log (they live on the PR, not the bus).

---

## Design

### 1. Routing model — three orthogonal levers

The dispatcher judges every task into **`{tier, engine, model}`**:

| Lever      | Controls                                                        | Values                                                                             |
| ---------- | --------------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| **Tier**   | pipeline depth (critics)                                        | `trivial` (no critics) · `standard` (+plan-critic) · `deep` (+spec & plan critics) |
| **Engine** | who implements                                                  | **claude** ⇄ **codex** — weighed equally, per task, no default                     |
| **Model**  | implementer strength, scaled by tier _within_ the chosen engine | see the model map (§2)                                                             |

Engine is chosen by task fit as an **active judgment**:

- claude leans: UI/frontend, nuanced reasoning, ambiguity.
- codex leans: large mechanical refactors, wide-but-shallow sweeps, deliberate second-engine perspective.
- Neutral fit → the dispatcher is free to pick either and states why.

**Profile constraint (unavoidable):** codex is **work-profile only** — not installed on personal hosts (g6, mbp), and `dispatch` hard-aborts `--agent codex` there. So "co-equal engines" holds only on the **work profile** (g5). On personal profiles the dispatcher is claude-only. This is documented in the protocol, not worked around.

### 2. Model map — the single source of truth

One table in `dispatch-orchestration.md` is authoritative for both ladders. Rubric prose says "the tier-appropriate model from the model map for the chosen engine" and never names a version, so it does not rot.

| Tier       | claude ladder                              | codex ladder                      |
| ---------- | ------------------------------------------ | --------------------------------- |
| `deep`     | **opus** _(→ escalate to Fable, §5)_       | current top gpt (seed: `gpt-5.6`) |
| `standard` | **sonnet**                                 | current mid gpt (seed: `gpt-5.5`) |
| `trivial`  | **sonnet** (or **haiku** if truly trivial) | current light gpt                 |

Concrete model names live **only in this table**. When OpenAI (or Anthropic) ships a new model, bump this one table; nothing else references a version. `dispatch` does not validate the model slot (verified — it passes `-m $model`/`--model $model` straight through), so the map is advisory to the dispatcher's judgment, enforced by nothing but the rubric.

Codex reasoning effort continues to scale with tier automatically (deep→high, standard→medium, trivial→low), independent of which gpt model is chosen.

### 3. Outcome logging — on the crew bus

**Why the bus, not MCP/A2A:** coordination is single-machine, single-repo, and must stay token-free. `crew watch`/`await` are held bash calls (zero tokens); an MCP/A2A layer would route coordination through the model's context (tokens) and add a daemon/port/auth failure surface. The bus is already a durable, per-repo, append-only `events.jsonl` keyed by `crew_id` that accumulates across runs. It is the right layer; MCP and A2A are explicitly rejected.

The outcome signal is already ~90% present — workers emit `pr_open`/`done`/`failed` status events with timestamps. Two additions complete the picture:

1. **`dispatch` stamps the decision.** On scaffold, append one `kind:"dispatch"` event to `events.jsonl`:
   ```json
   {"ts": <ms>, "crew_id": "<id>", "kind": "dispatch",
    "branch": "<branch>", "engine": "claude|codex", "model": "<model>",
    "tier": "trivial|standard|deep", "shape": "<one-word task-shape tag>"}
   ```
   Append with a jq-built line matching the `crew status` shape (`crew.sh:77-83`). Two implementation notes verified against the code: (a) `dispatch` must `mkdir -p "$crew_dir"` before the append — unlike `crew status` (`crew.sh:74`), the fish function does not create the dir, and the dispatch event may precede the worker's first `crew status`; (b) `shape` is read from a **`DISPATCH_SHAPE` env var** (blank if unset), threaded exactly like `DISPATCH_SPEC` (`fish/default.nix:338`) — it is the dispatcher's own one-word fit tag from the closed vocabulary in open-risk #3. All other fields (tier/model/agent/branch/`CREW_ID`) are already in scope at `fish/default.nix:322-337`.
2. **New `crew report` subcommand.** A jq join of `kind:"dispatch"` events ↔ terminal `kind:"status"` events, printing a table: `engine · model · tier · shape · outcome · duration_s`. Join and derived fields — verified against the actual event schema:
   - **Join key:** status events carry **no `branch` field** — the branch lives in `from` as `worker:<branch>` (`crew.sh:270`). Join is `dispatch.branch == (status.from | ltrimstr("worker:"))`, not a symmetric key.
   - `outcome` — the last terminal status for that worker (`done` / `failed`, and `pr_open` with a `pr_url`).
   - `duration_s` = `terminal_ts − first working_ts` (the first `working` status; the block-resume re-stamp at `WORKER_PROTOCOL.md:34` means "first" is load-bearing).
   - `gate_passed` — implied by reaching `pr_open`.
   - **Merged-vs-open** and **cross-review delta (`xreview`)** are deliberately **out of v1** — neither exists on the bus (merge state is on the PR; the review gate emits nothing about findings). See open-risks #4 and #5.

This adds one new **event kind** (`dispatch`) and one read-only subcommand — no schema migration, and `crew log` already dumps the same file. (The earlier draft claimed an `xreview` column with "no new store"; that was contradictory — the worker emits nothing about review findings today, so `xreview` is dropped from v1.)

### 4. Deep-tier cross-model review (engine-diverse)

For **deep tier only**, the code-review gate should include a reviewer engine **different from the implementer**, with findings reconciled (both / other-only / self-only). It is a should, not an unconditional MUST — it applies only where a diverse engine is available and the direction is proven; otherwise the worker falls back to same-engine adversarial review and never blocks (see Decision #4). standard/trivial review is unchanged.

- **claude implements → codex reviews (the tractable direction — v1 target):** the deep worker's review gate already dispatches a reviewer **subagent** (`WORKER_PROTOCOL.md:16-18`); the change is to dispatch a codex-diverse reviewer subagent that has the `mcp__codex__*` tool and drives codex through the read-only `codex` MCP server, reconciling against the same-model findings. **Not** via self-invoking the `/codex-review` slash command — an unattended worker's mechanism is the Task-tool subagent, matching the existing gate. The `pr-reviewers` plugin's `codex-reviewer` agent can be reused/adapted as that subagent. **Blocker to close in the plan:** workers run `--permission-mode auto` (`fish/default.nix:379`), which auto-denies any tool not on the allow-list, and `settings.json` allow-lists MCP tool groups individually (`settings.json:93-99`). The codex MCP tool prefix must be added to `permissions.allow` or the reviewer is silently denied. Registration alone is insufficient.
- **codex implements → claude reviews (deferred — not in v1):** a codex worker runs in the codex CLI and cannot dispatch a Claude Code subagent, so it would invoke claude out-of-process (`claude -p …`). ⚠️ **The earlier draft misattributed the risk here:** `settings.json:206` already sets `teammateMode: "in-process"`, which is the documented _fix_ for the `[[reference_claude_teammate_print_failure]]` bug — so that failure mode does not apply. The **real, unanalyzed** hazard is a fresh top-level `claude -p` process spawned from inside a `codex --dangerously-bypass-approvals-and-sandbox` session: does it inherit `~/.claude` auth and MCP config, does it run non-interactively without a TTY, does permission-mode-auto behave. Until the plan validates this from first principles, codex-implemented deep tasks use same-engine adversarial review.

**Provisioning (nix, work-profile-gated) — for the claude→codex direction:**

- **Add the `pr-reviewers` marketplace first, then enable the plugin.** `enabledPlugins` only toggles plugins from a known marketplace, and the factify `pr-reviewers` marketplace is not currently in `settings.json` — so provisioning is two steps: add the marketplace source, then the profile-gated `enabledPlugins` toggle. Per the CLAUDE.md settings split: profile-gated plugin enablement lives in the `--settings` overlay.
- **Register the read-only `codex` MCP server** (`codex mcp-server -c sandbox_mode="read-only" -c approval_policy="never" -c model_reasoning_effort="high"`) via `mcp-servers.nix`, **and add its tool prefix to `permissions.allow`** (plain JSON → `settings.json`, per the settings split) so an auto-mode worker isn't denied. The plan must confirm the exact resolved tool prefix.
- **Skip Cursor** — 2-way review (implementer + one diverse reviewer) only; `cursor-agent` is not installed and is out of scope.

### 5. Fable as a deep-only escalation

Fable (`claude-fable-5`) is the top rung of the **claude** ladder, gated to `deep`, and framed as an **escalation above opus** — not a peer:

> deep tier: default **opus**. Escalate to **Fable** only for a genuinely hard, well-specified, long-horizon task where opus is demonstrably not enough. Fable is ~2× opus cost, its safety classifiers can false-positive on security-adjacent code (a mid-run refusal stalls an unattended worker), and it runs minutes-long turns — treat it as the most expensive lever, used rarely.

**Pre-req to verify before wiring in:** confirm the `claude` CLI / subscription actually serves Fable (`claude --model claude-fable-5 -p "hi"`). `dispatch` passes the model through unvalidated, so an account without Fable access fails at runtime, unattended.

This keeps engines co-equal (claude ⇄ codex) while making Fable a within-claude _intensity_ choice — the mirror of codex's gpt ladder.

---

## Files touched

| File                                                         | Change                                                                                                                                                                                                                                                         |
| ------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| `DISPATCHER_PROTOCOL.md`                                     | Rewrite the engine framing: co-equal judged lever (no default); model-map reference; profile constraint; Fable escalation note.                                                                                                                                |
| `dispatch-orchestration.md`                                  | Add the model-map table (both ladders + Fable rung); update the choosing diagram so engine is a first-class judged branch.                                                                                                                                     |
| `commands/dispatcher.md`                                     | One-line update so the in-session rubric matches the baked protocol. **Also fix pre-existing drift** (`commands/dispatcher.md:11` still calls `crew` a fish function invoked via `fish -c`; it is a PATH CLI — fix in passing since the file is being edited). |
| `WORKER_PROTOCOL.md`                                         | Deep-tier cross-model review as a **scoped should** (claude→codex via a codex-diverse reviewer subagent; same-engine adversarial fallback otherwise) — not an unconditional MUST.                                                                              |
| `crew.sh`                                                    | Add `crew report` (jq join `dispatch.branch` ↔ `status.from                                                                                                                                                                                                    | ltrimstr("worker:")`) + usage string. Decide crew-scoping (open-risk #6). |
| `home/terminal/fish/default.nix`                             | `dispatch`: `mkdir -p "$crew_dir"` then append the `kind:"dispatch"` event; read `DISPATCH_SHAPE` from env (like `DISPATCH_SPEC`).                                                                                                                             |
| nix (settings overlay + `settings.json` + `mcp-servers.nix`) | Add `pr-reviewers` **marketplace** then work-profile-gated `enabledPlugins`; read-only `codex` MCP server; **allow-list its tool prefix** in `permissions.allow`.                                                                                              |

## Open risks to resolve in the plan

1. **codex MCP tool allow-list (blocks the v1 claude→codex direction).** Confirm the exact resolved tool prefix for the read-only `codex` MCP server and add it to `permissions.allow`, or an `--permission-mode auto` worker's reviewer subagent is silently denied. Verify the prefix against a live `claude mcp list` before relying on it.
2. **codex→claude review path (deferred direction).** Re-derive the real failure mode from first principles for a `claude -p` process spawned inside a `codex --dangerously-bypass-approvals-and-sandbox` session — auth/MCP-config inheritance, TTY/non-interactive behavior, permission-mode. Do **not** assume the mitigated `teammateMode` teammate bug is the hazard.
3. **Reviewer-subagent mechanism.** Confirm a deep worker's Task-tool reviewer subagent can carry `mcp__codex__*` and drive the codex MCP end-to-end under auto mode; decide whether to reuse the `pr-reviewers` `codex-reviewer` agent verbatim or adapt it.
4. **Fable CLI access.** Verify (`claude --model claude-fable-5 -p "hi"`) before adding the rung; `dispatch` passes the model through unvalidated, so no access = a runtime landmine on an unattended worker.
5. **`shape` tag vocabulary.** Settle a small closed set (e.g. `mechanical`/`ui`/`ambiguous`/`security`/`wide`) so the log is groupable, not free-text noise.
6. **`crew report` scope + merge state.** (a) `crew log` filters to one `crew_id` (one dispatcher shell); the "which engine wins" dataset spans all crews in the repo's `events.jsonl` — decide whether `report` aggregates all crews or the current one. (b) `pr_open` is on the bus but "merged" is not — decide whether `report` shells `gh pr view` for merge state or stops at `pr_open`.
7. **`xreview` as a future addition.** Recording whether the diverse reviewer caught something the same-model pass missed requires the worker to emit a new review-delta event/field to the bus. Out of v1 (see §3); revisit once the claude→codex path is proven, since that's when the data becomes meaningful.
