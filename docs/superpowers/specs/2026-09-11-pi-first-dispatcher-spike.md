# A pi-first dispatcher — exploration spike

**Date:** 2026-09-11
**Status:** spike — exploration; evaluation findings added 2026-09-13
**Related:** #140, `docs/superpowers/specs/2026-09-11-pi-dispatch-engine-design.md`

## The question

pi is unusually flexible — extensions, an SDK, subagents, an event bus, session
trees, packages, RPC. Today dispatcher uses pi only as a _fourth engine_ (a CLI
launched in a tmux pane, exactly like claude/codex/cursor). What would the
harness look like if it were **based on** pi rather than merely _supporting_ it?

This spike maps the option space, names the invariants that must survive, and
proposes a time-boxed prototype. It is not a commitment to rewrite.

## The invariants (what makes dispatcher itself)

These come from the README and are the reason the harness exists. Any pi-first
design must preserve them:

1. **No daemon, no socket, no server** — state is an append-only JSONL log
   (`.git/crew/`), inspectable with `jq` from any shell.
2. **Workers are real processes that outlive the parent session**, each in its
   own git worktree, reporting to the bus.
3. **Engine-neutral** — claude/codex/cursor/pi mix in one crew.
4. **The protocols are the product** — markdown read by models, not code.

## What pi actually offers (verified surfaces, pi 0.85.1)

- **Extension API** — `pi.registerTool`, `pi.registerCommand`, `pi.events`
  (shared bus), and lifecycle hooks: `before_agent_start` (inject a message /
  modify the system prompt), `tool_call` (**can block**), `context` (rewrite the
  message list before each LLM call), `turn_start`/`turn_end`,
  `tool_execution_start`/`end`, `agent_settled`, `session_start`/`shutdown`.
- **SDK** — `createAgentSession`, `AgentSessionRuntime`, `session.subscribe`
  (event stream), `session.prompt/steer/followUp`, `SessionManager` (append-only
  session **trees**: branch/fork/clone), `ResourceLoader`, `SettingsManager`,
  `ModelRuntime`.
- **Modes** — TUI, `-p`, `--mode json` (JSONL events), `--mode rpc` (stdin/stdout
  protocol), Node SDK, `acp`.
- **Subagents** (`pi-subagents`) — foreground children run **inside the parent
  process**; background children run in a **detached runner process** that keeps
  working after control returns (mirrors `events.jsonl`, `output-*.log`,
  transcript; steerable). Worktree isolation, `maxSubagentDepth`,
  `maxSubagentSpawnsPerRun`, per-role model/thinking, JS workflow scripts.
- **Packages** — one installable unit bundling extensions, skills, prompts,
  themes (`pi install npm:/git:/path`, pinned refs; Nix-friendly as a path).

## Where pi changes the architecture

### 1. The bus becomes an extension — and `stall-watch` disappears

Today `crew` is a bash CLI that both roles shell out to, and liveness is inferred
from **tmux pane output** (`crew stall-watch`), because a pane is the only signal
available. A `crew-bus.ts` extension can expose the _same_ JSONL bus as native
tools (`crew_status`, `crew_msg`, `crew_inbox`, `crew_roster`, `crew_watch`) and,
critically, drive coordination from **events**:

- **Heartbeat → stall detection becomes real.** Hooks on `turn_start` /
  `tool_execution_start` / `agent_settled` write liveness to the bus; a stalled
  worker is "no event in N seconds", not "pane output went quiet". The heuristic
  and its false positives (`buffered output reads as a wedge`) go away.
- **Redirect latency collapses.** `before_agent_start` / `context` can inject a
  pending directive automatically; a `tool_call` hook can honor a `stop` by
  blocking. Today a redirect waits for the worker's _next pipeline seam_
  (the checkpoint-peek discipline); event-driven delivery is per-turn or
  per-tool-call.
- **Reporting becomes automatic.** `agent_settled` / `tool_execution_end` can
  post `working`/`done`/metrics without the worker protocol's mandatory
  `crew status` calls — the protocol gets smaller, not more magical.

**Invariant kept:** the file is still `.git/crew/*.jsonl`; the extension is a
thin typed facade, never a replacement store.

### 2. Fan-out becomes subagents, not tmux windows

Fan-out today is one tmux window + worktree + process per worker — the reason
`crew` and `stall-watch` exist. pi offers two fan-out planes:

- **Foreground subagents** — in-process children, streaming, per-role models,
  bounded by `maxSubagentDepth` / spawn budget. Right for the **critic/review
  pipeline** (spec-critic, plan-critic, reviewers) and for ephemeral fan-out.
- **Background subagents** — a detached runner process; the child survives the
  parent. This is the closest pi analog to "workers outlive the session" and
  could host **pi-native durable workers**.

So the fleet becomes a **hybrid**: pi-native workers on the subagent planes,
external-engine workers (claude/codex/cursor) on the existing tmux+worktree
plane. Same bus, same protocols, two execution substrates.

### 3. Sessions give durability, recovery, and audit for free

pi sessions are append-only JSONL **trees** (`id`/`parentId`, branch/fork/clone,
compaction). For pi-native workers, the session file _is_ the durable record;
"recover a crashed worker" becomes resume/fork rather than reconstruct-from-bus.
**But** a claude/codex/cursor worker has no pi session — so the **bus stays the
neutral substrate**, and pi sessions are an _additional_ layer for pi-native
workers, not a replacement. Unifying them would trade invariant #3 for elegance;
don't.

### 4. The adapter matrix collapses

`gen-adapters.sh` projects four command bodies into claude/codex/cursor shapes
and a codex-skill shape. A pi-first dispatcher ships **one pi package**
(extensions + skills + prompt templates + subagent agent definitions) and treats
the other engines as **external-runner adapters** — the `codex-exec` profile
already exists, and claude/cursor are wrappable. The matrix becomes
`pi-native + N external runners` instead of N parallel native integrations.

### 5. Commands become skills and prompt templates

`/dispatcher`, `/autopilot`, `/finish-prs`, `/project-autopilot` become pi
prompt templates + skills directly. No projection script, no per-engine naming
(`$autopilot` vs `/dispatcher:autopilot`). The codex "ships as skills" footnote
becomes the norm rather than a workaround.

## Three adoption levels

| Level                                 | What it means                                                                                                                   | Cost   | Risk                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- | ------ | ----------------------------------------------------------------------------- |
| **L1 — reference engine** _(shipped)_ | pi joins claude/codex/cursor as a peer CLI engine. Nothing else changes.                                                        | done   | none                                                                          |
| **L2 — dispatcher as a pi package**   | Bus + dispatch + fleet become a pi extension; protocols ship with it; subagents run the critic pipeline. External engines stay. | medium | extension-API churn, TS testing story, supply chain                           |
| **L3 — SDK runtime**                  | `dispatcher` is a Node/SDK app that _is_ the orchestrator, embedding `AgentSession`s and driving workers in-process/RPC.        | high   | loses the "agent session _is_ the orchestrator" simplicity; new process model |

L2 is the interesting, defensible target. L3 is a different product.

## Target sketch (L2)

```
dispatcher/                      # a pi package (pi install / Nix path)
├── extensions/
│   ├── crew-bus.ts              # tools + event-driven coordination over .git/crew/
│   ├── dispatch.ts             # dispatch_task(): worktree + worker (native | external)
│   └── fleet.ts                # roster, budget weights, PR-gated reap
├── skills/  dispatcher · autopilot · finish-prs · project-autopilot
├── prompts/ dispatcher.md · …
├── agents/  spec-critic · plan-critic · reviewer      # pi-subagents frontmatter
├── protocols/  DISPATCHER_PROTOCOL · WORKER_PROTOCOL · dispatch-orchestration
└── package.json  (pi manifest)
```

- **Orchestrator:** `pi --append-system-prompt dispatcher-protocol` with the
  package loaded; the bus loop is event-driven (no `crew watch` process, no
  `stall-watch`).
- **pi-native worker:** foreground/background subagent in an isolated worktree;
  status and metrics posted by the extension.
- **external worker:** launched as today (tmux + worktree), reporting through the
  same bus file.
- **`crew` stays a CLI** for humans and non-pi workers; the extension calls the
  same code path (shared library) so there is exactly one bus implementation.

## Prototype (time-boxed, smallest falsifiable slice)

**Spike A — bus extension + event liveness (proves the core claim).**

1. `crew-bus.ts`: wrap the existing `.git/crew/` JSONL in tools
   (`crew_roster`, `crew_status`, `crew_msg`, `crew_inbox`) — call the `crew`
   binary underneath first, so the file format is untouched.
2. Add event heartbeats (`turn_start` / `tool_execution_start` / `agent_settled`)
   and a watcher that wakes the orchestrator on an inbound message.
3. For a pi-native worker, **turn off `stall-watch`** and detect stalls from
   missing events.
4. Run one real `standard` task through this dispatcher and compare against the
   current shell dispatcher: worker-side `crew` calls, false stalls, redirect
   latency (seam → turn), and whether the mechanism can be unit-tested without
   tmux.

**Spike B — critic pipeline on subagents.** Port `spec-critic`/`plan-critic` to
`pi-subagents` agent markdown, run a `deep` task, and confirm non-null
`plan_critic_first_pass` / `review_high` / `review_mode` — i.e. pi stops being
process-light.

Success = A removes `stall-watch` and shrinks the worker protocol without
regressing the bus's inspectability; B produces real review metrics.

## What NOT to change

- **Engine neutrality.** External-runner adapters and the engine-independent bus
  stay. The moment the bus lives only in a pi extension, L2 becomes L3 and the
  product becomes pi-only.
- **Inspectability.** The bus remains plain JSONL reachable by `jq` from any
  shell; extension tools must not hide it behind memory.
- **Durability.** Durable workers stay detached (tmux and/or the subagent
  background runner); don't replace long-lived workers with foreground children.
- **Protocols as markdown.** The rubric is the product; it can gain tool-backed
  teeth but should stay readable and model-facing.
- **No daemon.** Extensions run inside the session; the detached runner is a
  worker process, not a server.

## Risks

- **Extension-API churn.** pi is at 0.85.1 and moving fast; a pi-coupled
  mechanism tracks a moving target. Pin and vendor.
- **Supply chain.** pi packages run with full system access, unattended, in
  arbitrary repos. Same trust posture as `--no-approve` workers.
- **Testing story.** The suite is bats; TS extensions need node/vitest + the SDK,
  a new toolchain in CI.
- **Failure blast radius.** A broken extension blocks the whole session; a broken
  bash tool fails one call. Prefer thin extensions over thick ones.
- **Two observability models.** tmux windows (human-legible, engine-neutral) vs
  FleetView/subagent status (pi-only). Keep tmux for durable/external workers.
- **Product coupling.** Making the _mechanism_ pi-only raises the contribution
  bar and risks the engine-neutral value proposition.

## Prior art (pi package ecosystem, searched 2026-09-11)

The `pi-package` keyword covers **~9,650 npm packages**, and the primitives this
spike proposes to build already exist — often more maturely. Closest matches:

| Project                                                                                                                 | What it is                                                                                                                                                                                                  | Overlaps                                                                    | Gap                                                                                     |
| ----------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| **`pi-tmux-orchestrator`**                                                                                              | Pi package + Python CLI coordinating agents in a **tmux grid**; Unix-socket broker, mandatory reviewer, budgets, durable bounded state.                                                                     | tmux fleet, role topology, broker, recovery, budget, review-by-construction | pi-only workers; no engine-neutral bus; no PR-gated reap; no tier/engine/model judgment |
| **`pi-crew`**                                                                                                           | Sub-agent orchestration with **durable state**, parallel execution, **worktree isolation**, detached runs surviving session switches, real child Pi processes, planner/verifier workflows, Prometheus/OTLP. | durable workers, worktrees, pipeline, background runs, observability        | pi-only; own README warns it is **AI-generated and unaudited**                          |
| **`pi-agent-board`**                                                                                                    | Full-screen TUI to **dispatch/monitor/peek/reply/attach/clean up durable background Pi sessions** across projects.                                                                                          | roster, reply, durable sessions, cleanup                                    | a board over sessions, not an orchestration protocol; pi-only                           |
| **`@edgehero/pi-dispatch`**                                                                                             | Self-hosted job harness: BullMQ worker, forge (GitHub/GitLab/…) triggers, container-per-job, spend caps, run history.                                                                                       | "a dispatcher for pi", queue, forge triggers, budget                        | **opposite architecture** (daemon + queue + containers); pi-only                        |
| **`@quintinshaw/pi-dynamic-workflows`**                                                                                 | Workflow-script fan-out, model tiers (`small/medium/big`), **git-worktree isolation**, resume, cost accounting, `/code-review` on a PR.                                                                     | tiers, worktree isolation, fan-out, review                                  | deterministic scripts, not a judging orchestrator; pi-only; no bus/reap                 |
| **`pi-subagents`**                                                                                                      | Subagents + workflows; foreground (in-process) and **background (detached runner)** children; **external runners** (codex-exec, read-only cursor).                                                          | durable background, per-role models, **multi-engine**                       | no bus, no roster/reap, no tier protocol                                                |
| **`pi-background-tasks`, `pi-loop`, `pi-muselinn-harness`, `@arhen/pi-core-subagent`, `@gjczone/pi-swarm`, `pi-fleet`** | Durable background jobs, durable loops, swarm+goal+budget+queue, mailbox/intercom, local fleets.                                                                                                            | durability, budget, coordination, messaging                                 | pieces, not the whole; mostly pi-only                                                   |

**What none has:** the dispatcher-specific combination — an **engine-neutral
crew** (claude/codex/cursor/pi as peers), a **git-backed append-only bus** in
`.git/crew/` (`jq`-inspectable, with `roster`/`watch`/`await`/`reap`),
**PR-gated reaping**, and **the judgment protocol as the product** (tier × engine
× model × effort, `--plan provided`). So there is no drop-in equivalent, but the
orchestration/durability/worktree/board primitives are solved elsewhere.

### The `pi-tmux-orchestrator` "tmux grid"

Worth explaining, since it is the closest architectural match and a different
coordination model than ours. `pi-tmux-orchestrator start` creates a **detached
tmux grid**: one pane per **role** — `implementer`, a mandatory `reviewer`, a
`broker/status` dashboard pane, and optional `probe` / `playwright` /
`django-expert` — each a native Pi session (or a headless RPC pane with
`--rpc-workers`), visible and directly steerable, with the layout adapting to the
enabled roles. Crucially, **tmux only hosts and displays panes; it does not
transport workflow messages.** A per-run, owner-only **Unix-socket broker**
authenticates role bridges, accepts bounded typed reports, schedules the
mandatory review, and fails ambiguous delivery to `uncertain` rather than
blindly replaying. The broker/status pane is event-driven (refresh on a state
transition or a resize signal, never polling or tailing worker output) and shows
per-role `LINK / LIVE / ASSIGNMENT / MODEL / THINK / TOKENS / CTX` rows.

Contrast with dispatcher: dispatcher is **one tmux window per worker** — a flat
tab per independent task, no role topology — coordinated through the append-only
`.git/crew/` JSONL bus, with liveness inferred from pane output. The grid is **one
pane layout per run** with a fixed role topology, coordinated through a socket
broker, with tmux demoted to a display layer. Both make the fleet visible; they
differ on whether **tmux** (dispatcher) or a **broker** (orchestrator) is the
coordination substrate, and on whether tmux carries state or only frames.

### Implication: compose before building

Before writing any L2 code, evaluate the existing packages as the substrate —
this reframes L2 from "build a pi package" to a **compose-vs-build** decision.
The supply-chain caveat is real: these run unattended with full user privileges,
and at least one (pi-crew) self-describes as unaudited AI-generated code.

## Recommendation (revised after prior art)

**Compose first; build only the thin differentiators.** Pursue L2 as a direction,
but gate it on evaluating prior art before writing orchestration from scratch:

1. **Evaluate `pi-tmux-orchestrator` first** — the closest substrate (tmux grid +
   broker + mandatory reviewer). Determine whether dispatcher's engine-neutral
   bus, PR-gated reap, and tier protocol can layer on its broker/grid instead of
   a new extension; its `uncertain`-on-ambiguous-delivery and budget enforcement
   are things we would otherwise rebuild.
2. **Evaluate `pi-crew`** for the durable/worktree-isolated worker plane — with a
   hard eye on its unaudited-code warning. `pi-subagents` remains the candidate
   for the **multi-engine** plane (external runners) and the critic pipeline.
3. **Spike A still stands** (bus extension + event liveness / kill `stall-watch`),
   but scope it as a _thin_ layer over whichever substrate wins, not a new fleet
   manager. It directly attacks dispatcher's biggest real cost.
4. **Keep L1 as the baseline** and the shell CLI in place, so nothing regresses
   if the spike loses. **Do not pursue L3** until L2 (composed or built) has
   proven both better and maintainable.

The strategic prize is unchanged — removing the pane-output liveness heuristic,
making the critic pipeline native, and collapsing the per-engine adapter matrix —
but the prior art makes it likely the orchestration **engine** should be adopted,
while dispatcher keeps the **judgment protocol, neutral bus, and PR-gated reap**
as its contribution.

## Evaluation findings (2026-09-13)

Evaluates the "compose before building" recommendation above against the three
closest packages, plus this repo's own bus/watchdog data. No package was
installed; nothing here changes `adapters/`, `tests/`, or the bus format.

### Method

Three packages fetched with `npm pack --ignore-scripts <pkg>@<version>` into a
scratch directory and read, read-only — nothing installed into `~/.pi/agent`,
no install hooks run: `pi-tmux-orchestrator` 0.9.5, `pi-crew` 0.10.6,
`pi-subagents` 0.67.0. Extension-API facts are against `@earendil-works/pi-coding-agent`
0.85.1 docs (the version already vendored by this repo's L1 integration). Bus
evidence is cited as `<repo>` + row `ts` (+ branch) against every local
`~/git/*/.git/crew/events.jsonl`, so it is reproducible with `jq` alone; nothing
was copied out of those logs beyond the cited rows.

### Per-package decision table

**Architecture & invariants** — invariant numbers are this doc's four
(1 = no daemon/socket/server, 2 = workers outlive the parent as real processes
each in its own worktree, 3 = engine-neutral, 4 = protocols are markdown, not code):

| Package              | Substrate shape                                                                                                                                               | Inv 1                                                                                                                                                     | Inv 2                                                                                                                                                      | Inv 3                                                                                                                      | Inv 4                                                                                                           | PR-gated reap                                                                                          | Tier protocol                                                      | Liveness mechanism                                                                                                                            | Decision                                                                                                   |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| pi-tmux-orchestrator | Per-run Python asyncio broker on an owner-only Unix socket [T1], hosted in the tmux monitor pane [T2]; roles are native `pi` sessions in one shared tmux grid | **Violates** — socket broker process + SQLite store [T1][T5]                                                                                              | **Violates** — all roles share ONE worktree, single writer [T6]; no push/merge/clean, stop = kill tmux session [T7]                                        | **Violates** — workers always `pi`, no claude/codex/cursor path [T8]                                                       | **Violates** — workflow state machine, mandatory reviewer, phased flows coded in the broker [T9]                | None — no reap hook, doesn't push/merge/clean [T7]                                                     | None — no tier/engine/model judgment                               | Event-driven, no heartbeat/poll [T10]; hooks `session_start`…`session_shutdown` [T11]; ambiguous delivery → `uncertain` [T12]                 | **Reject** as substrate; borrow the `uncertain` pattern                                                    |
| pi-crew              | Unix-socket broker inside the parent pi session [C1]; durable state in `.crew/`/`.pi/teams/` [C2]                                                             | **Violates** — socket broker server [C1]                                                                                                                  | **Violates** — detached runner self-terminates when the parent dies, so runs do _not_ outlive it [C3], despite owning worktrees [C4]                       | **Violates** — workers are `pi` only, `--mode json -p` [C5]                                                                | **Violates** — workflows are TS task graphs / `.dwf.ts` scripts [C6]                                            | Has a merge gate [C7] but its own stale-reconciler/crash-recovery, not dispatcher's PR-gated reap [C8] | None named                                                         | Heartbeat on child stdout JSON, `HeartbeatWatcher` stale at 30 s [C9]; worker events "informational only" [C10]                               | **Reject** — unaudited, out-of-package writes; nothing to borrow                                           |
| pi-subagents         | Foreground in-process children; one detached, `unref`'d background runner per run holding sessions inside it [S1]                                             | **Violates** — state split across a rewritten `status.json` + appended `events.jsonl` under `os.tmpdir()`, not an append-only log under `.git/crew/` [S2] | **Partial** — background runner is detached and outlives a turn, but completion notification needs a live parent [S3]; owns worktrees like dispatcher [S4] | **Violates** — external engines (codex/cursor/claude) run as one-shot, read-only/plan-only adapters [S5], not peer workers | **Violates** — orchestration is a JS `workflowScript`, "the sole public multi-agent orchestration surface" [S6] | None named                                                                                             | None named (per-role model config, not tier×engine×model judgment) | Event-based (`turn_start`/`agent_start`/`agent_settled`, `lastActivityAt`, per-tool timeouts) [S7]; background = PID probe, "no polling" [S8] | **Reject** as substrate; borrow structured-verdict/`outputSchema` and the external-runner invocation flags |

**Trust & upkeep:**

| Package              | License   | Maintenance                                                                                                                                                                                          | Supply chain                                                                                                                                                                                                                                                                                                                  | API pinning                                                                                                                                                       |
| -------------------- | --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| pi-tmux-orchestrator | MIT [T13] | npm 17 versions, 0.4.2→0.9.5 in under a month, 4 releases in one day, one breaking (0.9.0 removed commands), none in the last 11 days [T14]; GH 2 stars, 0 forks, 1 contributor, 2 open issues [T15] | No deps, no npm scripts [T16]; load-time HTTPS to `registry.npmjs.org` for an update notice (opt-out env) [T17]; worker panes set version-check/telemetry opt-outs [T18]; pre-1.0, only latest `main` supported, explicitly not an OS sandbox [T19]                                                                           | No peerDependencies, no runtime pi-version check; imports only `node:` built-ins; plain JSON Schema tool params [T20]                                             |
| pi-crew              | MIT [C11] | npm 204 versions, 0.1.0→0.10.6 in ~4.5 months, 7 releases in the last 30 days, hotfix streaks [C12]; GH 52 stars, 21 forks, 4 contributors [C13]                                                     | `postinstall` installs a font into the user's font directory (Linux/macOS/Windows registry) and deletes matching skill dirs under `~/.pi/agent/skills/` [C14]; ships with an explicit "developed almost entirely by AI… not a hardened, audited product" warning [C15]; runtime `esbuild` transform of workflow scripts [C16] | All 4 pi peer deps are `"*"` (optional), no runtime version check, feature-detect + try/catch [C17]; ~84 pi imports                                               |
| pi-subagents         | MIT [S9]  | npm 131 versions, 0.3.0→0.67.0 in ~7.5 months, 22 releases in the last 30 days, frequent 0.x breaks [S10]; GH 3,556 stars, 679 forks, 100+ contributors, 9 open issues [S11]                         | No install hooks; installer clones the **unpinned default branch** into `~/.pi/agent/extensions/subagent` (or `git pull`s it) [S12]; exact-pinned runtime deps; the background runner replaces the global HTTP dispatcher via `undici.install()` [S13]                                                                        | Peers `pi-ai >=0.80.0`, others `*`; runtime branch on host pi version; standalone background support is pinned to pi 0.85.1 linux-x64 only [S14]; ~115 pi imports |

**Citations** (package-relative paths; `pi-tmux-orchestrator` = T, `pi-crew` = C, `pi-subagents` = S):

1. [T1] `pi_tmux_orchestrator/broker.py:1,150-170`
2. [T2] `pi_tmux_orchestrator/commands.py:247-257` (`respawn-pane … _broker`)
3. [T5] `pi_tmux_orchestrator/broker_store.py:1,54-58,57` (socket path, SQLite `broker.sqlite3`)
4. [T6] `pi_tmux_orchestrator/prompts.py:45-47`, `context_capsules.py:120`, `README.md:212`
5. [T7] `pi_tmux_orchestrator/SECURITY.md:24`, `commands.py:1282-1289`
6. [T8] `pi_tmux_orchestrator/commands.py:1509-1572`, `tmux.py:208-209`
7. [T9] `pi_tmux_orchestrator/references/protocol-v1.md:96-140`, `broker.py:1022-1024`
8. [T10] `pi_tmux_orchestrator/SECURITY.md:127-128`
9. [T11] `pi_tmux_orchestrator/extensions/orchestrator-worker.js:1206-1229`; reconnect timer `broker.py:1077`
10. [T12] `pi_tmux_orchestrator/README.md:105-106`
11. [T13] `pi_tmux_orchestrator/LICENSE.md:1-3` (© Revaz Zakalashvili)
12. [T14] npm release history; `CHANGELOG.md:55-59`
13. [T15] GitHub `revazi/pi-tmux-orchestrator` (created 2026-08-02, pushed 2026-09-12)
14. [T16] `pi_tmux_orchestrator/package.json:1-73`, `SECURITY.md:179`
15. [T17] `pi_tmux_orchestrator/extensions/orchestrator-update.js:11,122`, `SECURITY.md:172-177`
16. [T18] `pi_tmux_orchestrator/commands.py:1561-1562`
17. [T19] `pi_tmux_orchestrator/README.md:23`, `package.json:32-38`, `SECURITY.md:5,17-23`
18. [T20] `pi_tmux_orchestrator/extensions/orchestrator-worker.js:364-400` (used at `:1093`)
19. [C1] `pi-crew/src/runtime/broker/crew-broker.ts:2-18,286`
20. [C2] `pi-crew/src/utils/paths.ts:238-249`, `docs/architecture.md:20-22,115-137,150`
21. [C3] `pi-crew/src/runtime/parent-guard.ts:4-9`, `background-runner.ts:508`
22. [C4] `pi-crew/src/worktree/worktree-manager.ts:784,900,746`
23. [C5] `pi-crew/src/runtime/pi-spawn.ts:295-318`, `model/pi-args.ts:267`
24. [C6] `pi-crew/src/runtime/merge-gate.ts:1-10`, `docs/dynamic-workflows.md:14`
25. [C7] `pi-crew/src/runtime/merge-gate.ts:1-10`
26. [C8] `pi-crew/src/runtime/stale-reconciler.ts:253`, `recovery/crash-recovery.ts:308`
27. [C9] `pi-crew/src/runtime/task-runner/child-executor.ts:420,651`, `heartbeat/heartbeat-watcher.ts:136-138`, `config/defaults.ts:52`
28. [C10] `pi-crew/src/prompt/worker-events-channel.ts:25-26`
29. [C11] `pi-crew/LICENSE:1-3` ("pi-crew contributors")
30. [C12] npm release history; `CHANGELOG.md:1104-1149`
31. [C13] GitHub `baphuongna/pi-crew` (pushed 2026-09-13)
32. [C14] `pi-crew/scripts/postinstall.mjs:47-86`, `scripts/install-crew-vibes-font.mjs:40,51,57-62,76`
33. [C15] `pi-crew/README.md:3-13,1089-1091`
34. [C16] `pi-crew/src/runtime/goal-workflow/dynamic-workflow-runner.ts:22` (`esbuild` `transformSync`); `dist/index.mjs` 3.38 MB / 90,810 lines, package 12 MB
35. [C17] `pi-crew/src/extension/registration/hook-registration.ts:60`
36. [S1] `pi-subagents/src/runs/shared/background-process-options.ts:6`, `runs/background/async-execution.ts:599,709`, `docs/observability.md:228`, `docs/standalone-background.md:5`
37. [S2] `pi-subagents/src/shared/types.ts:2733-2738`
38. [S3] `pi-subagents/docs/standalone-background.md:49`
39. [S4] `pi-subagents/src/runs/shared/worktree.ts:921`
40. [S5] `pi-subagents/src/runs/shared/codex-exec-adapter.ts:97-110`, `cursor-agent-adapter.ts:85-93`, `claude-code-adapter.ts:97-108`
41. [S6] `pi-subagents/CHANGELOG.md:1029`, `docs/workflows.md:46`
42. [S7] `pi-subagents/src/runs/foreground/execution.ts:1005-1065`
43. [S8] `pi-subagents/src/runs/background/async-execution.ts:461-468`, `docs/watchdog.md:14,123`
44. [S9] `pi-subagents/LICENSE:1-3` (© Nico Bailon)
45. [S10] npm release history; `CHANGELOG.md:953,955,1029`
46. [S11] GitHub `nicobailon/pi-subagents` (pushed 2026-09-13)
47. [S12] `pi-subagents/install.mjs:16-17,63,79`
48. [S13] pinned deps (pi-server 0.85.0, jiti 2.7.0, typebox 1.1.38, undici 8.10.0, yaml 2.8.3, acorn 8.18.0); `src/runs/background/subagent-runner.ts:19`
49. [S14] `pi-subagents/src/runs/background/runner-aliases.ts:145-151`, `docs/standalone-background.md:3`

### Verdict

Applying the rule from SPEC D1.3: every package violates invariants 1, 3, and 4
at its core (Table 1) — none passes all four — so this is **not "compose X"**.
Spike A below shows a real watchdog cost (houston `feat/28`'s false-positive
`failed`), so the verdict is **build thin** — Spike A's bus extension + event
liveness only, scoped to a no-op-unless-loaded prototype (D2). L2-as-a-full-package
(bus + dispatch + fleet as one pi extension, per the "Target sketch (L2)" section
above) is deferred; nothing evaluated here is a substrate for it.

Patterns worth borrowing regardless of the reject verdicts:

- **pi-tmux-orchestrator**'s `uncertain`-on-ambiguous-delivery (`README.md:105-106`)
  — failing a delivery to a named uncertain state instead of guessing or
  silently replaying is a better shape than dispatcher's current redirect
  discipline for the ambiguous case.
- **pi-subagents**' structured `verdict`/`outputSchema` (`docs/tool-reference.md:77,85,440`)
  and its codex/cursor/claude external-runner invocation flags
  (`codex-exec-adapter.ts:97-110`, `cursor-agent-adapter.ts:85-93`,
  `claude-code-adapter.ts:97-108`) — worth reading as reference the next time
  dispatcher's own per-engine launchers change, not worth depending on.
- **pi-crew**: nothing. It is unaudited, AI-generated code by its own README
  (`README.md:3-13,1089-1091`), and its install-time writes land outside the
  package (`~/.local/share/fonts`, `~/.pi/agent/skills/`) — a posture this repo
  does not want to inherit.

### Spike B status vs the role grid

Spike B's stated success criterion was non-null `plan_critic_first_pass` /
`review_high` / `review_mode` for a pi worker — proof pi's critic pipeline
produces real review metrics, not process-light noise. Comparing that against
every pi dispatch's metrics and role rows on local buses:

- `hookyard` `feat/11-decide-the-shape-of-the-aeye-migration` (deep, pi
  `opencode/deepseek-v4-pro`, dispatch ts 1789211568999): both metrics snapshots
  show `plan_critic_first_pass: "revise"`, `review_high: 1`, `review_mode: "full"`.
  8 worker status rows over ~1.3 h; role `msg` rows present from all three roles
  (4 spec-critic, 3 plan-critic, 3 reviewer, plus 2 `metrics:` msgs).
- `dispatcher` `feat/152-key-pi-s-tier-map-and-launcher-default-o` (standard, pi
  `opencode/minimax-m3`, dispatch ts 1789211406582): first metrics snapshot
  `review_mode: "full"` (`plan_critic_first_pass: null`, `review_high: 0`); second
  snapshot, after a replan (`approach_abandoned`), shows `review_mode: "none"`.
  **Anomaly, reported not smoothed**: WORKER_PROTOCOL treats `review_mode: "none"`
  as never valid for a standard `done` — this run's final metrics snapshot
  violates that. 7 worker status rows over ~2.2 h; role status rows present for
  reviewer and plan-critic (3 each). n = 2 pi dispatches total.

**Status: superseded by the role grid** for the stated criterion — both observed
pi runs produce non-null review metrics through the existing role grid
(GRID_PROTOCOL role panes), with no `pi-subagents` dependency. `pi-subagents`
would still add over the grid: fresh-context critics that don't consume a tmux
pane (`README.md:91`, `prompts/review-loop.md`), a schema-validated verdict
instead of a model-authored metrics blob (`structuredOutput.verdict`,
`docs/tool-reference.md:77,85`, `outputSchema` at `:440`), and a stalemate
watchdog (`docs/watchdog.md:58-63`). None of that is worth its invariant cost
right now (Table 1: inv 1/3/4 violations, mixed on-disk state, one-shot external
runners) when n = 2 already shows the grid clearing the bar on both a standard
and a deep run — including the one anomaly, which is a protocol-compliance bug
in the worker, not evidence the grid fails to produce metrics.

### Spike A measurements

What event liveness (heartbeats replacing pane-output `stall-watch`) would
actually remove, from every local bus:

| repo       | rows | watchdog rows | pi dispatches |
| ---------- | ---- | ------------- | ------------- |
| aeye       | 59   | 0             | 0             |
| dispatcher | 257  | 0             | 1             |
| hookyard   | 524  | 0             | 1             |
| houston    | 267  | 6             | 0             |
| lazytmux   | 449  | 13            | 0             |
| nix-amd-ai | 23   | 0             | 0             |
| tmux       | 14   | 0             | 0             |
| toddl      | 73   | 0             | 0             |

Totals: 1,666 rows, 19 watchdog rows, 2 pi dispatches (0 watchdog rows on either).
Every flagged worker across all repos is `claude` (houston 25/25 claude
dispatches flagged 6 watchdog rows; lazytmux 36/36 claude dispatches flagged 13).

Per-episode classification (5 flagged workers, 7 detector episodes that ended
or escalated), label justified only by cited follow-up rows from the same
worker:

- **houston `feat/28-design-spec-cross-engine-hook-registry-a`** (claude): a
  `quiet:` episode escalated to `failed` "dead: quiet: unchanged for 1803s" at
  ts 1788965694670 (2026-09-09 14:54); the **same session**, with no
  dispatch/claim between, posted `pr_open` at 18:01 and `done` at 18:02 →
  **false positive** — the worker was alive and finished ~3 h later. Two earlier
  `quiet:` blocked/cleared pairs on the same worker are pane-change clears,
  undetermined individually.
- **lazytmux `feat/545-report-a-missing-theme-toggle-and-fix-th`** (claude): last
  worker detail at 06:50 ("entering fast deterministic gate"); `quiet:` escalated
  to `failed` "dead" at 09:33 (ts 1788773609296); the worker exited at 10:29 and
  a resumed session posted "investigating apparent hang in nix" (11:14) then
  "found+fixed genuine hang bug" (11:27) → **true positive**: a genuinely hung
  gate command.
- **lazytmux `feat/603-status-tick-jobs-stop-firing-and-never-r`** (claude): a
  `prompt:` episode blocked at ts 1789038780283 (2026-09-10 11:13) and cleared
  15 s later; the worker continued to `pr_open`/`done` → **true positive**: a
  real interactive prompt that got answered.
- **lazytmux `feat/544-diagnose-the-2-5s-startup-input-stall-in`** and
  **`feat/320-relay-sixel-and-osc-1337-graphics-for-no`** (claude): both escalated
  to `failed`/exit, both resumed and completed normally, and neither has a row
  explaining the multi-hour gap → **undetermined** in both directions.

Summary: 1 false-positive `failed`, 2 true positives, 2 undetermined. 13 of the
19 watchdog rows are `quiet:` (D3) blocked/cleared/dead; the rest is one
`prompt:` pair. Every `quiet:` episode followed a pane static for ≥30 minutes.

**pi-specific**: 0 watchdog rows over the 2 pi dispatches on local buses. pi gets
only the D0 (stalled-at-startup)/D3 (quiet) detectors — `crew.sh:2822-2836`
deliberately withholds the prompt/meter detectors (D1/D1b/D2) from every engine
but `claude`, since a guessed signature for another engine is a false-positive
generator. n = 2 is far too small to claim a watchdog false-positive _rate_ for
pi; it only shows pi hasn't tripped either detector it's eligible for yet.

**Worker-side `crew` overhead** for the two observed pi runs: `dispatcher`
`feat/152` posted 7 status rows (working ×3, pr_open ×2, done ×2) plus 2
`metrics:` msgs over ~2.2 h, with 3 additional status rows each from the
reviewer and plan-critic role panes; `hookyard` `feat/11` posted 8 status rows
(working ×4, pr_open ×2, done ×2) plus 2 `metrics:` msgs over ~1.3 h, with 4/3/3
role `msg` rows from spec-critic/plan-critic/reviewer. `crew status`/`crew
inbox` call latency, measured in a scratch git repo on this host (20 calls each):
`crew status` ≈ 16 ms/call (328 ms / 20 calls); `crew inbox --since 0` against a
20-row log ≈ 10 ms/call.

**Conclusion**: wall-clock overhead per bus call is negligible (10–16 ms). The
real cost event liveness would remove is not latency but **one model tool-call
per protocol post** — roughly 7–8 status rows per run in the two pi dispatches
observed, each a full agentic turn spent shelling out to `crew` instead of doing
task work.

**Two things event liveness would need to be true, that this evidence does not
establish**:

1. Stall-watch's stale-episode check reads only `status` rows plus a pane
   content hash (`crew.sh:2902-2906`, `_bus_refresh`) — it never reads `msg`
   rows. A `heartbeat:` sink `msg` row therefore cannot reduce a single
   watchdog false positive **unless `stall-watch` itself is changed** to read
   it; shipping the extension alone changes nothing stall-watch does today.
2. A single long silent tool call — exactly houston `feat/28`'s and lazytmux
   `feat/545`'s failure mode — emits no `turn_start` and no
   `tool_execution_start` for its whole duration. Event heartbeats on those two
   hooks would be silent through it too. Only `tool_execution_update` (streamed
   partial output) or a start-without-end timer on `tool_execution_start` could
   distinguish "one long tool call in flight" from "no turn happening at all".
   So **"heartbeats remove stall-watch false positives" is not demonstrated by
   this evidence** — the classified episodes show the mechanism's blind spot
   overlaps exactly the false positive (houston `feat/28`) and one of the two
   true positives (lazytmux `feat/545`) that were found.

### Prototype

Pointer to `spikes/pi-bus/`: `crew-bus.ts`, a pi extension that is a no-op
unless `CREW_WORKER_ID` is set, wrapping the `crew` CLI via `pi.exec` in 4 tools
(`crew_status`, `crew_msg`, `crew_inbox`, `crew_roster`) with no direct file
access and no bus format change; throttled heartbeats posted as `msg` rows to
`heartbeat:<crew>` on `turn_start`/`tool_execution_start`/`agent_settled`, with
the sink id resolved lazily on the first heartbeat from `crew id` (the same
`WORKER_TASK.md`-first order the worker's own rows use) and then `CREW_ID`,
cached once resolved and retried on later heartbeats while neither yields an id
(an empty-id sink would be rejected). Nothing is awaited at load, so the
extension cannot delay pi's serial extension loading. `bus.ts` holds the pure arg
builders and throttle. `stall-check.sh` is a read-only `jq` query grouping
workers with ≥1 heartbeat that's gone stale, separately from workers with none
("no heartbeat — extension not loaded"). Tests run under Node's built-in
`node:test` runner. A README covers purpose, run instructions, and known
limits. **Not wired into `dispatch`; no bus format change** — it is an
optional, unloaded extension, matching the "build thin" verdict above.

Load smoke (2026-09-13, no provider call): in a scratch git repo with
`CREW_WORKER_ID` set, `printf '{"type":"get_state"}\n' | pi --mode rpc
--no-session --no-extensions -e spikes/pi-bus/crew-bus.ts` on pi 0.85.1 loaded
the extension (including the four plain-JSON-Schema `registerTool` calls),
answered `get_state` with `success: true`, and wrote nothing to stderr. No hook
fired (no prompt was sent), so the live heartbeat path is covered only by the
fake-`pi` round-trip tests.

### Deferred live experiment protocol (2026-09-13)

Runnable later, gated on explicit human go-ahead (this spends real provider
budget). Not run as part of this evaluation.

**Loading the extension without editing `dispatch`.** `dispatch` builds the pi
command line itself (`dispatch.sh:1559`, with `--no-approve`, so project-local
`.pi/extensions` are ignored for the run), but pi still auto-discovers **global**
extensions from `$PI_CODING_AGENT_DIR/extensions/`. The worker window is created
by `tmux new-window -e CREW_WORKER_ID -e CREW_ID` (`dispatch.sh:1446`), and a
resumed pi worker gets the same two vars re-exported inline on the command
itself (`dispatch-resume.sh:460`); both otherwise inherit tmux's environment.

Treatment recipe:

```sh
D=$(mktemp -d)
mkdir -p "$D/extensions"
ln -s ~/.pi/agent/auth.json ~/.pi/agent/settings.json "$D/"   # link whatever config files exist; never copy secrets into the repo
ln -s "$PWD/spikes/pi-bus/crew-bus.ts" "$D/extensions/"
tmux new-session -d -s pi-bus-treat -c <repo>
tmux set-environment -t pi-bus-treat PI_CODING_AGENT_DIR "$D"
```

Then, from a shell **inside** `pi-bus-treat` (so `$TMUX` resolves to that
session — running the dispatch from outside it lands the worker window in the
caller's session without the extension loaded, silently voiding the load gate
below):

```sh
dispatch --crew-id <id> standard <model> --agent pi "<title>"
```

Control arm: the identical command from a session with no
`PI_CODING_AGENT_DIR`. **Never `tmux set-environment -g`** — a global value
leaks into every window created meanwhile, including concurrent dispatches,
lazy `--spawn-role` panes, and `dispatch-resume` (which re-exports
`CREW_WORKER_ID`/`CREW_ID` inline at `dispatch-resume.sh:460`). Precondition: no
other dispatch/spawn/resume targets either session during the run. Dependency:
once #140 lands and dispatch sets a per-worker `PI_CODING_AGENT_DIR`, that
override beats the session-level one and this recipe silently becomes a no-op —
re-verify against #140 before reusing it; the fallback shape is "symlink the
extension into the per-worker agent dir's `extensions/` before the worker's
first turn".

**Load gate**: the treatment run is void unless it produced ≥1 row to
`heartbeat:<crew>`; the control run is void if it produced any.

**Row-shape parity**: all worker `status`/`msg`/`metrics:` rows in both arms
must match the normal dispatch shape (same key set, same `from`/`to` semantics)
— the only new rows allowed are `msg` rows to `heartbeat:<crew>`. Checked with a
`jq` key-set comparison between arms.

**Why heartbeat rows stay invisible to every existing reader** (verified in this
worktree, not merely "matches a prefix"): `watch`/`inbox`/`await` match `.to`
exactly, `==$me or "*"` (`crew.sh:965`, `1524-1527`) — `heartbeat:<crew>` matches
neither. `roster` and `report` (and stall-watch's own `_bus_refresh`) read only
`kind=="status"` rows (`crew.sh:1409` for roster, `1543` for report, `2903` for
`_bus_refresh`) — a `msg` row is invisible to all three regardless of `.to`.
`rate`/`retro` match the `metrics:`/`retro:` prefixes specifically
(`crew.sh:1829`, `2212`) — `heartbeat:` doesn't match either prefix, so it stays
out of those readers too, and any future reader that scans _all_ `msg` sinks
would need to explicitly exclude `heartbeat:`. `dispatch --role-watch` matches
on role ids (`role:<branch>:<role>`, `dispatch.sh:134`) — again no match. `crew
log` prints every row for a crew id unfiltered by kind (`crew.sh:1530-1533`), so
it **does** print heartbeat rows — the one reader that sees them today.

**What the treatment arm changes in the prompt**: nothing in `dispatch` itself;
the extension contributes `promptGuidelines` telling the model to call
`crew_status`/`crew_msg`/`crew_inbox` instead of shelling out to `crew`
directly. The metric is therefore bash `crew` tool calls vs `crew_*` tool calls,
counted from the pi session JSONL, plus total bus rows added by the worker.

**Task pair**: two comparable `standard` pi tasks, or one task run twice on
fresh branches, same model and effort in both arms.

**Metrics** (`jq` over bus + pi session files):

- worker `crew` invocations by path (bash vs `crew_*` tool), from the session JSONL
- watchdog rows, `.body.source=="watchdog"`
- heartbeat rows and the max inter-heartbeat gap, vs pane-quiet episode windows
- redirect latency: a planted `crew reply` row's `ts` → the first following worker row's `ts`
- review-metrics parity (non-null in both arms)
- total run wall time
- total bus rows added

**Pass/fail thresholds**: zero row-shape violations; zero dispatcher wakes
triggered by a heartbeat row; heartbeat rows ≤ `run_minutes + agent_settled`
count; review metrics non-null in both arms. Adopt-worthy, additionally, only
if every watchdog episode attributable to D0/D3 in the control arm is matched
by a heartbeat gap ≥ that episode's quiet window in the treatment arm — and
even then, a heartbeat-gap-correlates-with-a-quiet-episode result is
correlation only, not a reduction: reducing watchdog false positives requires
`stall-watch` itself to start reading `heartbeat:` rows, a separate change from
anything the prototype ships.

**Known cost**: log growth ≈1 row/minute per worker with heartbeats enabled,
against an append-only log that every reader still `jq -s` reads in full.
