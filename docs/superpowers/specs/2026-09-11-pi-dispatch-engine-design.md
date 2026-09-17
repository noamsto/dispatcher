# pi as a fourth dispatch engine (+ deepseek models)

**Date:** 2026-09-11
**Status:** v1 shipped (#141); worker-scoped agent dir, trust hardening and the
profile-keyed tier gate built under #140; `omp` bake-off still pending.

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

**Finding (2026-09-13) — not adopted yet.** `pi-mcp-adapter@2.33.0` depends on
`@modelcontextprotocol/client` and `/core` via `https://pkg.pr.new` commit-preview
tarball URLs — outside the npm registry's integrity/provenance — plus native
addons (`@napi-rs/keyring`, `fs-native-extensions`). Adopting it now would mean a
runtime npm fetch plus native builds on an unattended worker's cold start, and
concurrent worker panes racing one install in the shared dir. The ambient MCP
config (`~/.config/mcp/mcp.json`, Nix-generated) lists only `context7`. Unblock
path: package it in `llm-agents.nix`, then load it with `-e <store path>` and a
worker settings stanza with `configPath` + `hostConfigDiscovery: "off"`. The
worker seed (D4) deliberately leaves `packages` unset.

### D3 — pi workers are **full-pipeline**, not process-light

**Superseded (2026-09-13).** The role grid (#146–#155) gives standard/deep pi
dispatches external critic/reviewer panes (`plan-critic`, `spec-critic`,
`reviewer`), so the `pi-subagents` critic port below is not built. Keep the
original text for context.

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

**As built (#140)**

- **Location:** `~/.pi/dispatcher-worker`, one dir shared by all pi workers.
- **Writer:** seeded by `crew pi-agent-dir` on every `dispatch` / `dispatch
resume` / `dispatch --spawn-role` that launches pi. It is not written by
  hm-module, because the dir is mutable (pi writes `settings.json`,
  `models-store.json`, sessions) and D4 forbids Nix owning a mutable pi
  settings file.
- **Seed contents:** `settings.json` merges in `defaultProjectTrust: "never"`
  and keeps pi-written keys. `auth.json` is 0600 and regenerated each launch
  but replaced only when it differs.
- **Credential provisioning rationale:** a fresh dir is credential-less.
  Copying `auth.json` would duplicate secrets. A symlink would let an OAuth
  refresh or `/login` write through into `~/.pi/agent`. So each ambient
  `api_key` entry becomes a
  `"!<abs jq path> -r '.[\"<name>\"].key' <ambient auth.json>"` read-through,
  using pi's documented `!command` key resolution. `!cmd` / single `$VAR`
  references are copied verbatim.
- **Limits:** OAuth entries and provider-scoped `env` maps are not
  provisioned (use the env-var route). An ambient key changed into a
  reference after seeding reads back literally until the next launch
  re-seeds.
  - The read-through pins the absolute `jq` store path resolved at seed
    time; a `nix-collect-garbage` that removes that `jq` before the next
    re-seed makes key lookups fail until the next dispatch re-seeds.
  - The seeder fails closed unless the ambient `auth.json` is a readable
    regular file holding a JSON object; only a genuinely missing file (its
    nearest existing ancestor a searchable directory) seeds an empty auth. A
    dangling or looping symlink, a FIFO, a directory, or a path behind an
    unsearchable directory is refused. A read-through that fails later at
    request time leaves that pi process without the file key for its
    lifetime (pi caches the failed result and falls back to the provider env
    var).
  - The seeder refuses when `~/.pi/dispatcher-worker` is a symlink, is not
    a directory, or resolves to the same place as, or nested with, the
    ambient agent dir.
- **Atomic writes** (temp file + `mv`). Benign race: the settings merge can
  drop a pi bookkeeping key written between the read and the `mv`.
- **Launch sites:** the worker launch, up-front role panes, the lazy
  `--spawn-role`, and resume. Each prefixes `PI_CODING_AGENT_DIR=<dir>` and
  fails closed (refuses to launch) if the seeder yields no directory, because
  an empty value would fall back to `~/.pi/agent`.
- **One-time break:** a pi worker started before this change keeps its
  session under `~/.pi/agent/sessions`, so resume it with `dispatch resume
--fresh`.
- A refused dispatch may still have seeded the dir (idempotent, harmless).

### D5 — Trust boundary: workers run `--no-approve`

Unattended workers execute in arbitrary target repos. `--no-approve` (and/or
`defaultProjectTrust: "never"` in the worker settings) prevents a target repo's
project-local `.pi/settings.json` / `.pi/extensions` / `.agents/skills` from
being auto-installed and executed. We want **global (worker-dir) resources, not
project resources**. `hostConfigDiscovery` stays `"off"` for the MCP adapter so
a repo can't inject MCP servers into an unattended run. This is a security
requirement, not a preference — pi packages run with full system access.

One deliberate carve-out: a worker _is_ handed its own worktree's skill dirs
(`.pi/skills`, `.agents/skills`) through explicit `--skill` flags. Skills are
declarative instruction text in a system prompt, not code that installs itself;
the worker already reads the repo with full tool access, so this is parity with
the claude/cursor workers, which get real worktree trust. `--no-approve` still
blocks project settings, extensions and packages, which is where auto-execution
lives. See `pi_skill_args` in `dispatch.sh` / `dispatch-resume.sh`.

**As built (#140)** — three layers:

- `--no-approve` on every pi launch;
- `defaultProjectTrust: "never"` in the worker settings;
- the worker dir's own empty trust store, so the user's saved `/trust`
  decisions never apply to workers.

Coverage: bats with a stubbed pi/tmux asserts the launch env/flags and that
`~/.pi/agent` is untouched, plus the live probe in KEY FINDINGS.

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

## KEY FINDINGS (measured 2026-09-11, pi 0.85.1 + deepseek via OpenRouter)

Recorded so they are never re-derived. The launch line is validated end-to-end.

- **Streaming worker mode: WORKS.** A real dispatch —
  `dispatch trivial openrouter/deepseek/deepseek-v4-flash --agent pi --effort low --plan provided "#1" "add hello-pi markdown"`
  on `noamsto/crew-smoke` — ran the full crew-bus lifecycle `working → pr_open → done`,
  opened [PR #4](https://github.com/noamsto/crew-smoke/pull/4), and emitted the
  single-agent metrics `{consulted:false, consult_engine:null, plan_critic_first_pass:null, rework_count:0, review_high:null, review_mode:"none"}`.
  The interactive TUI (`pi "<initial prompt>"`) auto-submits and repaints, so
  `crew stall-watch` gets a truthful liveness signal — `pi -p` would not.
- **`--append-system-prompt <path>` reads the file.** Confirmed in
  `packages/coding-agent/src/core/resource-loader.ts` (`resolvePromptInput` returns
  `readFileSync(input)` when `existsSync(input)`) and live: a file saying "always
  answer BANANA" produced `BANANA`. The protocol is a real system prompt, not a
  first-prompt injection.
- **Auth: ambient `~/.pi/agent/auth.json`.** `openrouter/deepseek/*` works from a
  non-interactive shell with the user's global pi config. A fresh
  `PI_CODING_AGENT_DIR` is credential-less (`auth.json = {}`), so v1 uses the ambient
  config — consistent with codex/cursor using the user's global engine config. The
  worker-scoped dir (D4) therefore needs credential provisioning.
- **`--no-approve` accepted; the worker stayed fully unattended** — no permission
  prompt, no trust block.
- **`--thinking` is real but clamped per model.** deepseek-v4-flash/pro ship
  `thinkingLevelMap = {off:"none", minimal:null, low:null, medium:null, high:"high", xhigh:"xhigh", max:null}`;
  `null` = unsupported. Measured: `off→off`, `high→high`, `xhigh→xhigh`, but
  `minimal`/`low`/`medium`→`high`. So for pi+deepseek `--effort` effectively
  selects `high` (default) vs `xhigh` (deep); the lower rungs are a clamped no-op.
  `dispatch` still rejects `ultra` for pi.
- **No MCP/subagents in v1.** Neither `pi-mcp-adapter` nor `pi-subagents` is
  installed, so the worker is single-agent — exactly as the protocol states.
  See the D2 finding (2026-09-13) for why the adapter still isn't adopted, and
  D3 for the role-grid supersession of the critic port.

## KEY FINDINGS (measured 2026-09-13, pi 0.85.1)

- **opencode ladder gap.** The opencode catalog has `deepseek-v4-pro` and
  `deepseek-v4-flash` but no `deepseek-v4.1-flash`, so the personal standard
  rung collapses onto `opencode/deepseek-v4-flash`. `dispatch`'s tier gate is
  profile-keyed: personal accepts opencode and OpenRouter; work is OpenRouter
  only.
- **`PI_CODING_AGENT_DIR` read-through probe.** A scratch dir holding only
  settings `{defaultProjectTrust:"never"}` and a `!jq` read-through `auth.json`
  answered `pi -p --no-approve --model opencode/deepseek-v4-flash` → `HELLO`;
  pi did not rewrite `auth.json` on startup.
- **Trust probe.** A hostile repo held `.pi/extensions/marker.ts` (writes a
  marker on load) and `.pi/APPEND_SYSTEM.md`. Markers written were 0 with
  `--no-approve`, 0 with no flag plus `defaultProjectTrust: "never"`, and 1
  with `--approve` (the control). A fingerprint of `~/.pi/agent`
  (files/sizes/mtimes, sessions excluded, plus session count) was identical
  before and after. Two approve/default reruns timed out at 150s on the
  provider, and only their marker results are used.
- **pi-mcp-adapter deps** — as in the D2 finding.
- pi scopes `--continue` lookups in a custom session dir to the cwd (pi
  CHANGELOG).

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
| `adapters/pi/` (new)                                | projected commands (prompt-templates / skills), ported `spec-critic` / `plan-critic` agent markdown, `spec-plan-critic` skill, worker `settings.json` + MCP config. **Not built** — superseded by the role grid (D3) and the `crew pi-agent-dir` seed (D4).                                        |
| `scripts/gen-adapters.sh`                           | project `core/commands/` into pi's shape; ship protocols in the pi tree.                                                                                                                                                                                                                           |
| `nix/hm-module.nix`                                 | write the pi adapter tree and the worker-scoped `PI_CODING_AGENT_DIR` seed. **Not built** — the dir is mutable, so `crew pi-agent-dir` seeds it at launch instead (D4).                                                                                                                            |
| `flake.nix`                                         | no engine packaging (ambient PATH); possibly nothing.                                                                                                                                                                                                                                              |
| `tests/*.bats`                                      | dispatch (unknown agent, gate, launch, `--thinking` mapping), dispatcher (stub `pi`), adapters (pi tree, idempotence), module.                                                                                                                                                                     |
| `README.md`                                         | engine matrix + badge; install prerequisites (pi package set, auth).                                                                                                                                                                                                                               |

## Data flow

```
dispatch <tier> <pi-model> --effort <e> --agent pi [id] <title>
  → worktree + tmux window + WORKER_TASK.md
  → PI_CODING_AGENT_DIR=~/.pi/dispatcher-worker pi --no-approve
       --model <provider/id> --thinking <e>
       --append-system-prompt WORKER_PROTOCOL.md "<launch prompt>"
  → standard/deep: role-grid critic/reviewer panes (plan-critic → execute → gates → review)
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
