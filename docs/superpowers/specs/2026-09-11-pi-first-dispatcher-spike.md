# A pi-first dispatcher — exploration spike

**Date:** 2026-09-11
**Status:** spike — exploration, no implementation committed
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

## Recommendation

Pursue **L2 as a direction**, not a rewrite, gated on two contained spikes:

1. **Spike A first** (bus extension + event liveness). It attacks the biggest
   real cost — `stall-watch` and pane-scraping — and its win is measurable
   against the current harness on one task.
2. **Spike B next** (critic pipeline via subagents) — already the tracked #140
   follow-up; it is the natural first consumer of the pi package.
3. **Keep L1 as the baseline** and leave the shell CLI in place; the extension
   should wrap the same bus, so nothing regresses if the spike loses.
4. **Do not pursue L3** until L2 has proven it is both better and maintainable.

The strategic prize is not "run pi as an engine" — that is done. It is removing
the pane-output liveness heuristic, making the critic pipeline native, and
collapsing the per-engine adapter matrix into one pi package plus thin external
runners.
