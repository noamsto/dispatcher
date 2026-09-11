# pi as a fourth dispatch engine (+ deepseek models)

**Date:** 2026-09-11
**Status:** draft — spike pending; `omp` bake-off scoped but not committed

## Problem

The dispatcher routes each task along three orthogonal levers — **tier**
(pipeline depth), **engine** (who implements), **model/effort** (how strong).
The engine lever has three values today: `claude` (default), `codex`
(gpt-5.x, work-profile only) and `cursor` (`cursor-agent`, work-profile only).

We want a fourth: **`pi`** (the pi coding agent harness). pi is the only engine
that can front **deepseek** and other non-Claude/non-OpenAI/non-Cursor families
through OpenRouter — Claude is Anthropic-only, Codex is OpenAI-only, and Cursor's
catalog (Composer/Grok/Kimi) doesn't include deepseek. So "pi + deepseek" is one
change, not two: pi is the engine, deepseek is a model-map entry.

This spec also settles three questions that came up while scoping:

1. Whether to build MCP support for pi (we said we'd have to; we were wrong).
2. Whether to make pi a **full-pipeline** engine or accept codex/cursor's
   process-light path.
3. Whether `omp` (`can1357/oh-my-pi`, a pi fork) should be the baseline instead.

## What already exists (verified 2026-09-11)

Ground truth, so none of this is re-derived:

- **The engine CLI is already Nix-packaged.** `noamsto/llm-agents.nix`
  (fork of `numtide/llm-agents.nix`) ships `packages/pi/package.nix` — a clean
  `buildNpmPackage` of the prebuilt `@mariozechner/pi-coding-agent` tarball
  (`pname = "pi"`, `mainProgram = "pi"`). `nix-config` consumes that overlay, as
  it already does for `cursor-agent`. Locally installed: pi 0.85.1 under
  `/etc/profiles/per-user/$USER/bin/pi`. **No packaging work is needed for v1.**
  (Minor drift: current npm scope is `@earendil-works/pi-coding-agent`; the
  package/`update.py` still track the older `@mariozechner` scope — worth a bump,
  not a blocker.)
- **pi's launch surface** (from `pi --help`, 0.85.1):
  - `--provider <name>` + `--model <pattern>` (supports `provider/id` and an
    optional `:<thinking>` suffix), `--list-models`.
  - `--thinking off|minimal|low|medium|high|xhigh|max` — a **real** effort knob
    (unlike cursor, where effort is baked into the model id).
  - `--append-system-prompt <text>` — append to the system prompt. This is
    strictly better than codex/cursor, which must inject the protocol as the
    first user prompt.
  - `-a/--approve` / `-na/--no-approve` — project-resource trust for one run.
    Non-interactive modes (`-p`, `--mode json`, `--mode rpc`) never prompt.
  - `PI_CODING_AGENT_DIR` overrides the config dir (default `~/.pi/agent`) —
    settings, skills, extensions, packages all live under it.
  - Modes: interactive TUI (default; `pi "<initial prompt>"` auto-submits and
    repaints), `-p/--print` (buffered — prints only at end), `--mode json`
    (JSONL event stream), `--mode rpc` (stdin/stdout JSON protocol).
  - **No built-in MCP** — by design ("You can build or install those workflows
    as extensions or packages"). Also no built-in sub-agents, permission
    popups, plan mode, or background bash.
- **The two packages that matter exist and are mature:**
  - `pi-mcp-adapter` — MCP bridge. **Lazy by default** (servers don't connect
    until a tool is called), exposes **one proxy tool (~200 tokens)** instead of
    hundreds, caches tool metadata, reads `.mcp.json` /
    `~/.config/mcp/mcp.json`, supports `configPath`, per-server `lifecycle` /
    `directTools` / `inheritEnv`, and can adopt Cursor/Claude/Codex host configs.
    This preserves the exact property the harness cares about — schemas
    deferred, ~free until used.
  - `pi-subagents` — subagent delegation. Builtin `scout` / `researcher` /
    `worker` / `reviewer` / `oracle` / `delegate`; **custom agents are markdown
    frontmatter** loaded from `~/.pi/agent/agents/**` and project `.pi/agents/**`;
    parallel review, workflows, per-role model/thinking config; foreground
    children stream in-session.
  - `pi-web-access` — web/docs/PDF research; dependency of `researcher` /
    `evidence-auditor`.
- **A colleague's shared harness exists** (`asaf-s-factify/pi-coding-agent`) —
  one `.pi/settings.json` pinning 23 npm packages + interactive defaults
  (`defaultProvider: cursor`, `grok-4.5:fast`, everforest theme). Useful as a
  **market survey**; its defaults are personal-interactive and must not be
  imported into unattended workers.

## Decisions

### D1 — pi is the engine; deepseek is a model-map entry

`--agent pi` is added alongside `claude|codex|cursor`. The `<model>` slot takes
a pi model pattern (`provider/id`, optional `:thinking`). The tier→model ladder
for pi lives in `dispatch-orchestration.md`'s model map, like every other
engine. No new engine abstraction — the harness is already engine-neutral.

### D2 — MCP is adopted via `pi-mcp-adapter`, not built

The earlier assumption ("we'd have to write an MCP bridge") was wrong: it is a
pinned npm package with the laziness the harness requires. Adoption is a config
decision, not a project. `--mcp <profile>` stays claude-only (it is a claude
`--mcp-config` flag); pi's base stack comes from its own config, same shape as
codex/cursor.

### D3 — pi workers are **full-pipeline**, not process-light

This is the differentiator versus codex/cursor. `pi-subagents` provides the
mechanism the claude-only critic pipeline needs (fresh-context children), and
custom agents are markdown — so `spec-critic` / `plan-critic` can be **ported**
as pi agent definitions + a `spec-plan-critic` skill/prompt-template rather than
written as a new extension. Role→model mapping replaces claude's
opus-plans/sonnet-implements split (e.g. `oracle`→deepseek-pro,
`worker`→deepseek-flash).

**Consequence:** pi is the first non-claude engine that can emit real
`plan_critic_first_pass` / `review_high` / `review_mode` metrics instead of
`null`/`"none"`. The `WORKER_PROTOCOL.md` carve-out must therefore **not** lump
pi in with codex/cursor.

### D4 — Ownership / install surface

Following the README's existing split (_"Prerequisites this module deliberately
does not manage: … Engine CLIs and auth"_), the pi work divides four ways:

| Layer                                                                                    | Owner                                                                                            | Notes                                                                                                                                                                                     |
| ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `pi` binary                                                                              | **`llm-agents.nix`**                                                                             | already packaged; just consume the overlay                                                                                                                                                |
| personal interactive pi (creds, default provider/model, personal package list)           | **`nix-config`**                                                                                 | out-of-band, like `codex login` / `cursor-agent login`. Nix must **not** own the mutable `~/.pi/agent/settings.json` (`pi install`, `lastChangelogVersion`, theme, trust all rewrite it). |
| dispatcher↔pi integration (protocols, launch branches, `adapters/pi/`, worker agent dir) | **this repo**                                                                                    | same precedent as the claude/codex/cursor adapters                                                                                                                                        |
| worker-required packages (`pi-mcp-adapter`, `pi-subagents`, `pi-web-access`)             | **this repo, worker-scoped** (see below); optionally promoted to `llm-agents.nix` packages later | not personal; not the binary                                                                                                                                                              |

**Worker-scoped agent dir.** Workers launch with
`PI_CODING_AGENT_DIR=~/.pi/dispatcher-worker` (a writable dir seeded from a
dispatcher-generated `settings.json`), containing: the pinned worker package
set, the `subagents.*` role→model map, `defaultProjectTrust: "never"`, and an
MCP config pointing at the existing `mcp-servers.nix` output. This keeps worker
semantics independent of the user's interactive pi config, and avoids two
owners writing one settings file. The user's global `~/.pi/agent/` is never
touched by dispatcher.

### D5 — Trust boundary: workers run `--no-approve`

Unattended workers execute in arbitrary target repos. `--no-approve` (and/or
`defaultProjectTrust: "never"` in the worker settings) prevents a target repo's
project-local `.pi/settings.json` / `.pi/extensions` / `.agents/skills` from
being auto-installed and executed. We want **global (worker-dir) resources, not
project resources**. `hostConfigDiscovery` stays `"off"` for the MCP adapter so
a repo can't inject MCP servers into an unattended run. This is a security
requirement, not a preference — pi packages run with full system access.

### D6 — `omp` is **not** the baseline; it is a scoped experiment

`omp` (`can1357/oh-my-pi`) is a fork/superset of pi that already bundles
subagents (`task`, `orchestrate`, `workflowz`, role-based models), MCP, LSP+DAP,
persistent Python/Bun execution, and a declarative `programs.omp.settings` Home
Manager module. It is a serious candidate, but not the baseline:

- **It is a fork, not upstream.** Our plan is built on upstream pi 0.85.1
  conventions (`PI_CODING_AGENT_DIR`, `--append-system-prompt`, `--thinking`,
  `--no-approve`, `pi-subagents`, `pi-mcp-adapter`). omp has its own package
  scope, extension API, and native equivalents — every convention would need
  re-verification (the "guessing it bit codex" lesson).
- **Maintenance/trajectory risk.** ~2.7k open issues vs upstream's ~200; a
  single opinionated maintainer with a vouch-based PR policy; 103k vs 30k stars
  on the respective upstreams.
- **Heavier + patch-fragile Nix package.** `packages/omp/package.nix` patches
  around upstream bugs (nightly-Rust bootstrap, tree-sitter link args,
  peer-dep rewrite, stats-bundle placeholder, bun-registry sandbox workaround).
  The `pi` package is a clean prebuilt-tarball build.
- **Package-ecosystem fork.** Upstream `pi-subagents`/`pi-mcp-adapter` likely do
  not apply to omp; you'd use omp's native subagents/MCP. So the two baselines
  are not interchangeable — conflating them is the main risk.

**The one claim that would override this:** omp's "harness problem" pitch — that
its edit format / read summarization / tool design makes _weak models_ land
edits (advertised 2.1× pass rate, −61% tokens). Worker reliability-per-token is
the dispatcher's core metric, and dispatcher runs fleets of cheap deepseek
workers. That is an empirical question, so it gets measured (below), not
assumed either way.

## Hard prerequisite: measurement spike

Guessing bit codex; the cursor spec made the spike task 1 for the same reason.
Before any launch-line code is written, resolve these (record findings
KEY-FINDINGS-style in this doc so they're never re-derived):

1. **Streaming worker mode (THE blocker).** The stall watchdog infers liveness
   from pane output, so `pi -p` (buffered) is wrong — the exact bug that bit
   cursor (#103). Confirm interactive `pi "<initial prompt>"` auto-submits and
   repaints, or route `--mode json` through a formatter into the pane. Without
   this there is no reliable unattended worker.
2. **System-prompt injection.** Confirm `--append-system-prompt` accepts the
   protocol **file** (help says "text or file contents") or pass
   `"$(cat WORKER_PROTOCOL.md)"`.
3. **Tool auto-approval.** pi has no sandbox or per-tool permission prompt;
   confirm an unattended worker never blocks on one. Confirm `--no-approve`
   loads the worker-dir resources while ignoring project resources.
4. **Provider + auth.** Which route (openrouter vs deepseek direct), which env
   var, and does the credential survive a non-interactive tmux pane.
5. **`--thinking` maps** per the deepseek model's `thinkingLevelMap` (levels may
   be clamped/unsupported).
6. **Package install under `PI_CODING_AGENT_DIR`.** Verify pi installs/finds
   `pi-mcp-adapter` / `pi-subagents` in the worker dir, and what happens on a
   cold start (network, trust, timing) inside a worker pane.
7. **MCP wiring.** Point the adapter at the existing `mcp-servers.nix` output
   and verify the base stack (context7, playwright, firefox-devtools) is lazy
   and ~free when unused.

## Scoped experiment: upstream pi vs `omp`

Run the **same** real dispatched task (a `standard` bug fix with a non-trivial
edit) through both, unattended, and compare:

| Metric                                  | Source                                      |
| --------------------------------------- | ------------------------------------------- |
| PR correctness / gates passed           | review gate + fast deterministic gate       |
| Review-gate findings, rework count      | bus metrics (`review_high`, `rework_count`) |
| Tokens / cost                           | pi session footer / `get_session_stats`     |
| Failure mode (wedge, permission, trust) | stall-watch + pane capture                  |

Decision rule: adopt omp only if it shows a **material** correctness or
cost-per-success advantage on the same model, and its conventions hold up under
item 1–6 of the spike. Otherwise keep upstream pi as the baseline and revisit
omp as a separate fifth engine.

## Components that change

| File                                                | Change                                                                                                                                                                                                                                                                                             |
| --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `adapters/core/dispatch.sh`                         | `--agent` case list + error text; a `pi` launch branch (`--provider`/`--model`, `--thinking`, `--append-system-prompt`/protocol, `--no-approve`, `PI_CODING_AGENT_DIR`); generalize the `--mcp` rejection; decide the profile gate (pi+deepseek is likely a personal engine — see Open questions). |
| `adapters/core/dispatcher.sh`                       | same case-list/gate edits + a `pi` orchestrator branch (pi as dispatcher session).                                                                                                                                                                                                                 |
| `adapters/core/protocols/DISPATCHER_PROTOCOL.md`    | engine-lever candidate list; scaffold line; profile constraint; a pi section under "Read the bus" (park primitive).                                                                                                                                                                                |
| `adapters/core/protocols/dispatch-orchestration.md` | choosing-tree diagram; **pi ladder** column in the model map; orchestrator-engines row; effort prose (`--thinking` is real, unlike cursor); MCP section (adapter, lazy).                                                                                                                           |
| `adapters/core/protocols/WORKER_PROTOCOL.md`        | **full-pipeline** pi carve-out (D3): ported critic agents, role→model map, not the codex/cursor process-light path; consult roster gains pi; metrics section gains pi.                                                                                                                             |
| `adapters/pi/` (new)                                | projected commands (prompt-templates / skills), ported `spec-critic` / `plan-critic` agent markdown, `spec-plan-critic` skill, worker `settings.json` + MCP config.                                                                                                                                |
| `scripts/gen-adapters.sh`                           | project `core/commands/` into pi's shape; ship protocols in the pi tree.                                                                                                                                                                                                                           |
| `nix/hm-module.nix`                                 | write the pi adapter tree and the worker-scoped `PI_CODING_AGENT_DIR` seed.                                                                                                                                                                                                                        |
| `flake.nix`                                         | no engine packaging (ambient PATH); possibly nothing.                                                                                                                                                                                                                                              |
| `tests/*.bats`                                      | dispatch (unknown agent, gate, launch, `--thinking` mapping), dispatcher (stub `pi`), adapters (pi tree, idempotence), module.                                                                                                                                                                     |
| `README.md`                                         | engine matrix + badge; install prerequisites (pi package set, auth).                                                                                                                                                                                                                               |

## Data flow

```
dispatch <tier> <pi-model> --effort <e> --agent pi [id] <title>
  → worktree + tmux window + WORKER_TASK.md
  → PI_CODING_AGENT_DIR=~/.pi/dispatcher-worker pi --no-approve
       --provider <p> --model <m> --thinking <e>
       --append-system-prompt-file WORKER_PROTOCOL.md "<launch prompt>"
  → full pipeline via pi-subagents (plan-critic → execute → gates → review)
  → reports working → pr_open → done via `crew` (engine-agnostic)
  → opens a PR
```

## Open questions (need owner answers)

1. **Scope:** pi as worker only, or also as orchestrator (`dispatcher --agent pi`)?
2. **Profile gate:** codex/cursor are work-only; pi+deepseek is presumably a
   personal engine. Gate it, un-gate it, or gate on provider?
3. **Provider + tier map:** openrouter (`deepseek/...`) vs deepseek direct, and
   the `deep`/`standard`/`trivial` model ids.
4. **Auth:** which env var / secret path, and is it visible to tmux workers?
5. **Critic port:** port `spec-critic`/`plan-critic` to `pi-subagents` agents in
   v1, or ship parity first and port in v2?

## Out of scope (YAGNI)

- Making `omp` the baseline (D6 — experiment only).
- Importing `asaf-s-factify/pi-coding-agent` wholesale (personal interactive
  defaults; use it as a package survey only).
- A nix-config-owned global `~/.pi/agent/settings.json` (mutable-file conflict).
- Packaging `pi-mcp-adapter`/`pi-subagents` in `llm-agents.nix` — deferred until
  hermetic worker behavior is actually needed (runtime npm fetch is acceptable
  for v1).
