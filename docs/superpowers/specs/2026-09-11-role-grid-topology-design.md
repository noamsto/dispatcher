# Role-grid topology — a crew per task window

**Date:** 2026-09-11
**Status:** design — phase 1 (mechanics) implemented; coordination protocol is phase 2
**Related:** #140 (pi engine), #142 (pi-first spike), `docs/superpowers/specs/2026-09-11-pi-first-dispatcher-spike.md`

## Problem

Today a dispatched task is **one tmux window with one pane running one agent**:

```
window = task → pane = worker process
```

The tier pipeline (spec-critic → plan-critic → implement → review gate) exists
only _inside_ that one process, and only for **claude** — its subagents and
workflows are the mechanism. codex, cursor and pi run **single-agent**
("process-light") precisely because they have no equivalent. Consequences:

- **Cross-model review is nearly impossible.** The codex-diverse reviewer is
  claude-only and "not yet wired" for other implementers. Nothing lets a
  pi/deepseek implementer be reviewed by a claude or codex pane.
- **Roles are invisible and unsteerable.** A reviewer is a hidden subagent
  inside one context; you cannot watch it, redirect it, or see it stall.
- **The critic pipeline is a per-engine integration** rather than an
  engine-neutral mechanism, so it must be ported engine by engine.

## Idea

Turn the topology inside out:

```
window = task → grid of panes = one role per pane
```

Each pane is an ordinary process that can run **any** engine. The pipeline is
then expressed as _processes on the bus_, not as any engine's subagent feature —
so it works for claude, codex, cursor and pi alike, and roles become visible,
steerable and independently recoverable.

```
┌────────────────────┬────────────────────┐
│ implementer (lead) │ plan-critic        │
├────────────────────┼────────────────────┤
│ reviewer           │ security           │
└────────────────────┴────────────────────┘
        same worktree · same crew bus · role-scoped ids
```

## Topology is tier-proportional

The grid size _is_ the tier. Trivial stays a single pane; only the heavier tiers
grow panes.

| Tier       | Topology                                                                              |
| ---------- | ------------------------------------------------------------------------------------- |
| `trivial`  | 1 pane — implementer only (today's behavior)                                          |
| `standard` | lead + plan-critic + reviewer                                                         |
| `deep`     | lead + spec-critic + plan-critic + reviewer + optional diverse-engine / security pane |

## Why externalizing roles is the right move

1. **Engine-neutral pipeline.** A role is a process; the mechanism no longer
   depends on opus/sonnet subagents or a workflow JS file. This is how
   codex/cursor/pi stop being process-light.
2. **Cross-model review becomes structural.** The implementer and reviewer are
   different processes, so they can trivially run different engines — the
   diverse reviewer stops being a claude-only, "not yet wired" special case.
3. **Visible and steerable.** Watch a critic think, steer it, kill a wedged one.
   The codename/color fleet vocabulary extends from windows to panes.
4. **Recoverable.** A dead role pane is re-dispatched without touching the lead.

## Coordination

Panes share the **worktree** (same `cwd`) and the **crew bus**. No new daemon —
the file bus carries signals, exactly as it does for windows.

- **Identities.** `role:<branch>:<role>` (e.g. `role:feat-x:plan-critic`),
  alongside the existing `worker:<branch>`. The bus already accepts arbitrary
  `from`/`to` ids; only the roster/protocol vocabulary grows.
- **Artifacts are files in the worktree**, not context: the lead writes the
  plan / spec / diff where a role can read it (e.g. `.dispatcher/plan.md`), so
  handoff does not depend on any engine's context sharing.
- **Park-until-assigned.** A role pane launches, then immediately parks on
  `crew await "role:<branch>:<role>"`. The lead sends the role its assignment at
  the right seam, the role works, posts a verdict to `worker:<branch>`, and
  re-parks (or exits). This is the existing `crew await` primitive — no new
  coordination machinery.
- **Lead owns the sequence** in phase 1 (the same seam discipline the worker
  protocol already uses). A dedicated broker/status pane is a later option for
  `deep`, mirroring `pi-tmux-orchestrator`.

### Materialize on demand, not a fixed grid

A fixed grid launches every role up front and leaves panes idle — a wall of
processes for work that is mostly sequential. Instead, create a role pane **at
its seam** (plan-critic when the plan exists, reviewer when the code is ready)
and reap it after its verdict. The topology table above is a _ceiling_, not a
simultaneous layout. This keeps scale honest and reuses `reap`.

## tmux mechanics

- Create the task window, then `tmux split-window -t <win> -c <worktree>` per
  role; `tmux select-layout -t <win> tiled` to keep the grid legible.
- Per-pane identity: a `@crew_role` pane option + `pane-border-format`, so a
  pane is labelled by role the way a window is labelled by codename.
- Read-only roles get a read-only posture (`--tools read,grep,find,ls` for pi;
  the reviewer agents for claude).

## Engine diversity (the payoff)

A role→engine map lets a single task span vendors:

| Role        | Engine (example)                |
| ----------- | ------------------------------- |
| implementer | pi · `deepseek/deepseek-v4-pro` |
| plan-critic | claude · opus                   |
| reviewer    | codex · `gpt-5.6-sol`           |
| security    | claude · opus                   |

This is the cross-model review the `WORKER_PROTOCOL` describes as desirable and
marks "not yet wired". With role panes it is just a different `--agent` per pane.

## Protocol changes

- A new `GRID_PROTOCOL.md`: a role's duties, the artifact paths, the bus signals
  (assignment / verdict), and the verdict schema. It is what a role pane is
  launched with.
- `WORKER_PROTOCOL.md`: when grid mode is on, the lead does **not** spawn
  in-process critics; it writes the artifact, signals the role pane, and awaits
  the verdict. Grid mode is additive — non-grid workers are unchanged.
- `crew`: role-scoped `roster` rows; the rest already fits.

## Phases

1. **Mechanics (implemented).** `dispatch --roles <list>` creates the task window
   with a lead pane plus one pane per role, each launched with `GRID_PROTOCOL.md`,
   a `@crew_role` label, and a role-scoped identity. Default behavior unchanged.
2. **Coordination.** `GRID_PROTOCOL.md` fleshed out; the lead sends assignments
   and awaits verdicts; roles park and post. One `standard` task end-to-end.
3. **On-demand materialization + broker/status pane** for `deep`; `reap` role
   panes.
4. **Tier defaults + role→engine map**, so `dispatch` materializes the right
   topology automatically.

## Open questions

1. **Lead-coordinated vs broker-coordinated.** Phase 1 makes the lead the
   sequencer. A broker pane (pi-tmux-orchestrator style) removes sequencing from
   the model but adds a component.
2. **Seam artifacts: files vs bus payloads.** Files are simple and diffable;
   bus payloads are unified but bloat the log.
3. **Idle panes vs churn.** Park-until-assigned keeps panes cheap but idle;
   materialize-on-demand is leaner but adds window mutation.
4. **How a role proves it ran** (verdict schema, evidence, metrics) reuses the
   outcome-metrics record shape.

## Relationship to the pi-first spike

Orthogonal but complementary. The grid is **engine-agnostic** — it can ship on
today's engines with no pi dependency. If the roles are pi panes, the pi-first
work (event-based liveness, bus as a tool) upgrades the grid's coordination
quality; if they are claude/codex panes, they fall back to the file bus and pane
liveness. Grid first is therefore the safer, more broadly useful order.
