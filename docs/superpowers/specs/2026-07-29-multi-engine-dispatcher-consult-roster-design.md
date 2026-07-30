# Multi-engine dispatcher + deep-consult roster — design

Date: 2026-07-29
Status: approved (pending plan)

## Context

The dispatch system today:

- The **dispatcher** (orchestrator) is always a claude session: the `dispatcher` fish
  launcher bakes `DISPATCHER_PROTOCOL.md` via `claude --append-system-prompt-file`
  (default model opus), and `/dispatcher` promotes a running claude session in place.
- **Workers** already fan out across three engines — `dispatch --agent claude|codex|cursor`
  with a per-engine model map in `dispatch-orchestration.md`.
- The **deep-tier orchestration consult** (a decomposition pass before the plan phase) is
  Fable-only: a deep claude worker spawns an ephemeral `model: fable` subagent that writes
  `DECOMPOSITION.md`, decided in the worktree at the plan seam, never at dispatch time.
- Cross-model infra already exists: deep claude workers get a **read-only codex MCP**
  injected (`mcp-codex.json`, work-profile gated) for the diverse code-review pass.

Two expansions, one spec:

1. The **dispatcher session itself** can run on codex or cursor with fitting models,
   not just claude/opus.
2. The **deep consult** can draw on a roster of top-tier consultants (Fable,
   gpt-5.6-sol, grok-4.5-high), not just Fable.

Approved decisions (from the brainstorm): scope is the dispatcher session (not worker
orchestration models); the consult consultant is **picked by the worker at the plan
seam**, never pinned at dispatch time — extending the existing
"decided in the worktree" philosophy.

## Part 1 — Multi-engine dispatcher launcher

`home/terminal/fish/default.nix` → `dispatcher.fish` gains flags:

```
dispatcher [--agent claude|codex|cursor] [--model <id>] [--effort <low|medium|high|xhigh>] [first task…]
```

- `--agent` defaults to `claude`; `--model`/`--effort` override the per-engine defaults
  below. `--effort` is accepted-and-ignored for cursor (effort lives in the cursor model
  id — same convention as `dispatch`). Everything else in the function is
  engine-agnostic and unchanged: tmux badge
  (`@crew_name dispatcher`, colour99), `$CREW_ID` minting, `crew register $fish_pid`,
  `crew deregister` on exit.

Launch lines (mirror the worker-launch patterns in `dispatch.sh`):

| engine | launch                                                                                                                                  | default model / effort        |
| ------ | --------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------- |
| claude | unchanged: `claude --name <session> --append-system-prompt-file …/DISPATCHER_PROTOCOL.md <task>`                                        | opus (settings default)       |
| codex  | `codex --profile worker -m gpt-5.6-sol -c model_reasoning_effort='"high"' --dangerously-bypass-approvals-and-sandbox '<inject prompt>'` | gpt-5.6-sol, high             |
| cursor | `cursor-agent --model kimi-k3-high --force --trust --approve-mcps --disable-indexing --disable-codebase-ref '<inject prompt>'`          | kimi-k3-high (no effort knob) |

- **Inject prompt** (codex/cursor): "Read `~/nix-config/home/ai/claude-code/DISPATCHER_PROTOCOL.md`,
  follow your engine's watch-loop section, and adopt the dispatcher role for the rest of
  this session" + the first task when given. This is the same injection pattern worker
  launches already use (`WORKER_PROTOCOL.md`); neither CLI has an
  `--append-system-prompt-file` analog. Trade-off accepted: prompt-injected protocols are
  less sturdy across compaction than a baked system prompt; workers already live with this.
- **`--profile worker`** layers the nix-generated base MCP stack for codex; cursor gets
  the same stack from the shared `~/.cursor/mcp.json` automatically.
- **Work-profile gate** mirrors `dispatch.sh`: `--agent codex|cursor` aborts with a clear
  message when `$DISPATCH_PROFILE != work`.
- **codex effort is `high`, not xhigh** (the local codex default): blocked workers wait on
  a bounded ~300s in-band window; xhigh's minutes-long turns would let blocks go stale.
- **cursor dispatcher is `kimi-k3-high`** — a genuinely third-family model (Moonshot; not
  Claude/OpenAI) with a strong agentic tool-use record, and the dispatcher's core value is
  judgment, not turn speed: the park loop is idle most of the time, so kimi's lack of a
  `-fast`/`-low` variant (effort is fixed at `high`, baked into the id) costs latency only
  on wake-handling turns. `composer-2.5` / `cursor-grok-4.5-*` stay available via
  `--model`.
- **Session naming degrades**: codex/cursor have no `--name` launch flag, so the
  `dispatcher: <task>` resume-picker suffix is claude-only.

`/dispatcher` (the in-session command, `home/ai/claude-code/commands/dispatcher.md`)
stays claude-only — promotion is inherently a claude-session concept. It gains a one-line
note that codex/cursor dispatchers are launcher-only.

## Part 2 — `DISPATCHER_PROTOCOL.md`: engine adaptations

The judging rubric (tier / engine / model / effort / plan-depth) is engine-agnostic and
stays single-sourced in one protocol file. The watch-loop section splits per engine:

- **claude / cursor** — unchanged: arm exactly one `crew watch` as a background shell call,
  handle the completion notification, INV-1 (exactly one outstanding watch) with the
  arm-token discipline as written. cursor-agent's shell tool has the same
  background-with-completion-notification primitive, so the loop ports verbatim.
- **codex** — no background-notify primitive, but also no short foreground tool timeout:
  the park is a **blocking** `crew watch --timeout 270` foreground call in a loop. Human
  input typed during the park queues in the TUI and is delivered when the turn ends.
  INV-1 is vacuous for a foreground call (it cannot double-arm), so the arm-token
  machinery does not apply. **Amended at final review:** codex parks at 270 even when
  drained — the park _is_ the turn, so the 3300s drained park would leave the human
  queued behind a foreground call for ~55 minutes exactly when their input is the only
  thing that can arrive.

Also generalized:

- The self-similarity bias note ("You are a claude session…") becomes
  "You are a _{engine}_ session — do not let 'X is what I am' become 'X is the fit'",
  since a codex/cursor dispatcher judging engines has the same failure mode toward its
  own engine.
- The roster-diagram (D2) instructions stay — all three engines can write files; the aeye
  carousel render itself is claude/codex-hook territory (see Out of scope).

## Part 3 — Deep consult roster (worker picks at the plan seam)

`home/ai/claude-code/WORKER_PROTOCOL.md` → "Orchestration consult (deep only)": step 2
("If it trips, consult Fable") becomes a pick from a roster. The step-1 survey boolean is
unchanged (still decides _whether_); the new choice is _which_:

| consultant               | mechanism                                                                                                                                                                                | lean                                                                                          |
| ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| **fable** (default)      | Agent tool ephemeral subagent (`model: fable`) in the worktree; writes `DECOMPOSITION.md` directly                                                                                       | hardest decompositions; the only consultant with in-worktree write access                     |
| **gpt-5.6-sol**          | the read-only codex MCP already injected into deep claude workers (`mcp-codex.json`, effort high); the worker asks for the decomposition and writes `DECOMPOSITION.md` from the response | a non-claude-family decomposition perspective, diverse from the opus planner that consumes it |
| **cursor-grok-4.5-high** | `cursor-agent -p` one-shot in the worktree (same launch flags as cursor workers, minus the stream-json pane pipe); the worker writes `DECOMPOSITION.md` from stdout                      | third-family perspective                                                                      |

Invariants preserved:

- `DECOMPOSITION.md` keeps the exact required structure (components / ordering /
  interfaces) and stays **author-less** — now also hiding _which engine_ authored it from
  the plan-critic (rule-2 discipline).
- **Fallback stays "a should, not a blocker"** (step 3): consultant refuses / times out /
  is unavailable → drop the consult and proceed byte-identical to a non-consulted deep
  worker. "Unavailable" includes the profile gate: codex/cursor consults are work-profile
  only (the codex MCP file is work-gated at generation; the consult roster applies the
  same work-profile rule to `cursor-agent` that `dispatch` does), so a personal-profile
  worker's roster collapses to fable.
- **Neutral fit → fable**: cheapest, runs in-worktree, and is the only consultant that can
  author the file directly (the MCP/one-shot consultants return text the worker must
  transcribe).
- Step 4 (seed the plan) and step 5 (false-negative recovery) are unchanged — a recovery
  consult also picks from the roster.
- The roster is a table in the protocol; adding a consultant is one row. That is the
  "other top-tier models" escape hatch.

The consult lives in the claude-worker pipeline only. Codex/cursor **workers** stay
process-light (no spec-plan-critic, no consult) — unchanged.

## Part 4 — Metrics + reference docs

- `WORKER_PROTOCOL.md` metrics record gains a field:
  `consult_engine: "fable"|"codex"|"cursor"|null`, emitted alongside `consulted`.
  `consulted` stays the boolean; `consult_engine` is `null` when `consulted` is `false`
  and on non-deep tiers / single-agent engines (same null-not-"null" rule). The existing
  consulted-vs-not A/B (#86's oracle gate) gains a per-engine dimension instead of
  conflating every consult as "Fable".
- `dispatch-orchestration.md`:
  - gains a short "orchestrator engines" subsection with Part 1's launcher model table
    (claude → opus default; codex → gpt-5.6-sol high; cursor → kimi-k3-high), so concrete
    versions keep living in exactly one reference file;
  - the "Orchestration consult" paragraph is updated to the roster + `consult_engine`.

## Out of scope

- Codex/cursor deep **workers** gaining critics or consults (they stay process-light).
- aeye roster-diagram rendering for a cursor dispatcher (claude and codex have aeye
  adapters/hooks; cursor has none — the D2 file is still written, the carousel just may
  not refresh for a cursor dispatcher).
- `/dispatcher` in-session promotion for codex/cursor (launcher-only by design).
- Changing the worker engine model map (claude/codex/cursor ladders are untouched).

## Validation

- `dispatcher --agent codex` / `--agent cursor` on a work-profile host: session starts
  with the protocol injected, mints/registers a crew id, badge renders; a first task
  dispatches a worker; the watch loop wakes on a worker event (background-notification
  for cursor, blocking park for codex).
- Profile gate: `DISPATCH_PROFILE=personal dispatcher --agent codex` aborts before launch.
- Consult roster: on a `deep` claude worker whose survey trips, the worker picks a
  consultant, `DECOMPOSITION.md` appears with the required structure and no author named;
  on a personal-profile host the roster collapses to fable; a refused/timed-out consult
  falls through to the plain path without failing the worker.
- Metrics: a consulted deep run emits `consult_engine` matching the consultant used;
  non-consulted runs emit `null`.
- `nh home switch` builds (fish function syntax, no new packages required — codex and
  cursor-agent are already installed).
