# dispatch resume — continue a worker's own session from its worktree

**Status:** design, awaiting implementation
**Date:** 2026-09-09

## Problem

You are sitting in a worker's worktree and want that worker running again. Today
the only way is to re-dispatch it:

```bash
dispatch --crew-id <id> standard sonnet --effort medium "the exact original title"
```

Three costs, all avoidable:

1. **You must reconstruct the arguments.** Tier, model, effort, engine and crew
   id are all recorded in the worktree's own `WORKER_TASK.md`
   (`dispatch.sh:988`), and you retype them from memory anyway.
2. **You must reproduce the original title verbatim**, because the branch name
   is derived from it. `dispatch` says so itself when it cannot help you:
   _"No bus row carries its original title, so resuming it means reconstructing
   the wording that produced its name"_ (`dispatch.sh:769`). From inside the
   worktree the branch is `git rev-parse --abbrev-ref HEAD` — the title is
   irrelevant.
3. **The engine starts from zero context.** `dispatch` reuses the branch and
   worktree, carries the task body forward (`dispatch.sh:977-980`) and appends a
   `resume_note` telling the worker to reconstruct its position from `SPEC.md`,
   `PLAN.md` and `git status` (`dispatch.sh:1078-1081`). That is a _branch_
   resume. The engine's own conversation is discarded, so an hour of accumulated
   reasoning is re-derived from files.

What is wanted is a **session** resume: the same worker, same crew, its own
transcript restored.

## Terminology

`dispatch` already has an internal `switch_mode=resume` (`dispatch.sh:637`),
meaning _the branch ref already exists, reuse it rather than create_. This
document calls that **branch resume**, and the new capability **session
resume**. `dispatch resume` performs both.

## What already exists

Worth stating precisely, because most of the machinery is in place and this
design's job is to stop re-deriving what is already on disk.

| capability                                                                                                            | where                        | status        |
| --------------------------------------------------------------------------------------------------------------------- | ---------------------------- | ------------- |
| Branch/worktree reuse when the ref exists                                                                             | `dispatch.sh:637-661`        | shipped       |
| Task body carried forward on resume                                                                                   | `dispatch.sh:977-980`        | shipped       |
| A resume hint in the launch prompt                                                                                    | `dispatch.sh:1078-1081`      | shipped       |
| `engine`/`model`/`effort`/`tier`/`kind`/`draft`/`plan`/`crew_id`/`agent_name`/`worker_id` stamped in `WORKER_TASK.md` | `dispatch.sh:988`            | shipped       |
| Crew id resolved task-document-first, env as fallback                                                                 | `crew.sh:_crew_id`           | shipped       |
| Crew discovery and dispatcher re-attach (`crew crews`, `crew adopt`)                                                  | `crew.sh:536`, `crew.sh:608` | shipped (#29) |
| Worker-window occupancy detection (`crew occupants`)                                                                  | `crew.sh:76`, `crew.sh:322`  | shipped       |
| Engine session continuation                                                                                           | —                            | **missing**   |
| Resolving the launch from the worktree instead of argv                                                                | —                            | **missing**   |

The dispatcher-side recovery story is done: `crew adopt` re-attaches a restarted
dispatcher to an on-disk crew, and already distinguishes "a live dispatcher holds
this crew" from "a stale pid" by walking its own ancestry (`crew.sh:660-681`).
**This design must not reimplement any of that.** A resumed worker needs no
adoption: `crew_id` in its task document is what `crew status` resolves from.

## Engine continuation surfaces

All three verified against the installed CLIs:

| engine         | flag             | notes                                                                                                                                                                                                                                                                                                                                                                                |
| -------------- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `claude`       | `-c, --continue` | "most recent conversation in the current directory". Degrades to a fresh session when the cwd has no conversation — verified: `claude -c -p 'say ok'` in an empty dir answers rather than erroring. Because `--append-system-prompt*` forces `--system-prompt-snapshot` off, re-passing `WORKER_PROTOCOL.md` applies **fresh** on a resume, so the protocol is genuinely reasserted. |
| `codex`        | `resume --last`  | cwd-filtered by default (`--all` is what _disables_ cwd filtering). Accepts `-m`, `-p/--profile`, `-c`, `--dangerously-bypass-approvals-and-sandbox` and a positional prompt, so the existing launch line survives intact.                                                                                                                                                           |
| `cursor-agent` | `--continue`     | Also `resume` / `--resume [chatId]`. Positional prompt accepted.                                                                                                                                                                                                                                                                                                                     |

Each takes a prompt alongside the continue flag, so the reorient text below is
delivered the same way the current launch prompt is.

## Surface

```
dispatch resume [--agent E] [--model M] [--effort E] [--mcp P] [--fresh] [extra prompt…]
```

`resume` is intercepted as `$1` before the positional tier/model parse at
`dispatch.sh:30-42`, which otherwise rejects it as an unknown tier. No
`--crew-id`: it comes from the task document, and that is the point of the
command.

Resolution, in order — every field from `WORKER_TASK.md`, with an explicit flag
overriding:

| field                                                        | source                            |
| ------------------------------------------------------------ | --------------------------------- |
| `branch`                                                     | `git rev-parse --abbrev-ref HEAD` |
| `engine`, `model`, `effort`, `tier`, `kind`, `draft`, `plan` | `WORKER_TASK.md` header           |
| `crew_id`, `agent_name`, `worker_id`                         | `WORKER_TASK.md` header           |
| task body                                                    | left in place, never rewritten    |

**Gap to close:** the header does not record `--mcp`, so an `--mcp analytics`
worker cannot be faithfully resumed. Stamp `mcp:` alongside `engine:`/`model:` in
`dispatch.sh:988` going forward, and accept `--mcp` on resume for workers
dispatched before that. The deep-tier `xreview_mcp` needs no recording — it is
derivable from `tier` plus `$DISPATCH_PROFILE`.

Absent `WORKER_TASK.md` is a hard error naming `dispatch <tier> <model>`; a
detached HEAD or the default branch is refused.

## The task document is not rewritten

`dispatch` truncates and re-stamps `WORKER_TASK.md` on every run
(`dispatch.sh:1006`). `dispatch resume` must not: the worker may have been
handed a spec, and the header is the record this command is _reading_.

Two exceptions, applied as line edits rather than a rewrite:

- `dispatcher_pane:` — updated when reattaching to a live dispatcher (below).
- `resume: true` — already stamped on a branch resume (`dispatch.sh:992-994`);
  keep it, and add nothing else.

## Placement — reuse the window, do not open a second one

`dispatch` always opens a fresh window (`dispatch.sh:1038`), and its resume-mode
gates actively refuse when anything already sits at the worktree:

- a _worker_ window still at that path is refused unless the bus shows a
  terminal state (`dispatch.sh:699-711`);
- a pane at that path with an **empty** `@crew_name` — a human in a plain shell,
  which is exactly you running this command — is refused outright
  (`dispatch.sh:799-806`).

So inheriting those gates makes `dispatch resume` refuse in its primary use
case. It must instead **reuse the window that is already there**: find it by
`pane_current_path` (the technique `_occupants` and the gate above both use,
because lazytmux renames worker windows and the dispatch-assigned name is long
gone), and `send-keys` into that pane — including your own, when you are sitting
in it. Only when no pane is at the worktree does it `tmux new-window`, applying
the same `@crew_name`/`@crew_color`/border stamping and pre-launch window sizing
`dispatch` does (`dispatch.sh:1008`), since a hand-made window carries none.

Consequence worth accepting explicitly: `dispatch resume` deliberately does
**not** enforce the anti-stacking occupancy refusal, because reusing the
occupant's own pane is not stacking. It must still refuse the two cases that are
never legitimate: the branch checked out in the primary worktree, and a resume
launched from the worktree it targets while a _different_ live engine holds it.

## Gates — which still apply

Reusing a recorded tier/model pair is not the same act as choosing one, so the
gates split:

| gate                                             | on resume                              | why                                                                                                                               |
| ------------------------------------------------ | -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| Work-profile engine gate (`dispatch.sh:211-215`) | **runs**                               | codex/cursor availability is a property of now, not of the original dispatch                                                      |
| `--effort ultra` on claude (`dispatch.sh:419`)   | **runs**                               | same                                                                                                                              |
| Budget / rung refusal (`dispatch.sh:427-448`)    | **runs**                               | quota is a property of now; resuming adds load                                                                                    |
| Model dispatchability (`dispatch.sh:219-226`)    | **runs**                               | a model id can be retired between dispatch and resume                                                                             |
| Tier↔model map (`dispatch.sh:319-322`)           | **skipped** unless `--model` is passed | the pair was adjudicated at first dispatch; re-gating would refuse a resume because the map changed under it, stranding live work |

`--ignore-budget` / `--ignore-map` keep their meanings.

## Dispatcher liveness — reattach or run solo

Probed from `crews/<crew_id>/`, whose pid `crew register` writes
(`crew.sh:531`):

| state                       | meaning                 | behaviour    |
| --------------------------- | ----------------------- | ------------ |
| directory absent            | clean `crew deregister` | **solo**     |
| present, `kill -0` fails    | dispatcher crashed      | **solo**     |
| present, `kill -0` succeeds | live dispatcher         | **reattach** |

**Solo** keeps the same `crew_id` and keeps posting to the bus. Nothing is lost:
the log is durable, and a later dispatcher can `crew adopt` that id and read the
whole history. `dispatch resume` never mints a crew and never adopts one — that
is `crew adopt`'s job, and minting here is the exact footgun #29 was filed
about.

**Reattach** additionally rewrites `dispatcher_pane:` in `WORKER_TASK.md` and
sends `crew msg <worker_id> dispatcher:<crew_id> "resumed"`.

The message is deliberate, and is the one interrupt this design accepts. `crew
watch` wakes on a message to the dispatcher, but its default `--states` exclude
`working` — so a status post alone would leave a live dispatcher still believing
a worker it wrote off as `failed` is dead, and it may re-dispatch the same task
onto a branch this command just revived.

**Gap to close:** `crew register` records only a pid, no pane, so the live
dispatcher's `dispatcher_pane` is not discoverable. Have `crew register` write
`$TMUX_PANE` beside the pid. `dispatcher.sh:114` already calls `crew register

$$
` from a context where `TMUX_PANE` is set, so this needs no change there.
Missing pane file → leave `dispatcher_pane:` as it stands.

## Launch

The three branches of `dispatch.sh:1121-1141`, each gaining its continue flag
and swapping `resume_note` for the reorient text below. `--fresh` drops the
continue flag and keeps today's `resume_note` verbatim — the escape hatch for a
session that resumes into a wedged state.

The reorient prompt replaces the current `resume_note` in continuation mode
because that note assumes no transcript: it sends the worker to `SPEC.md` and
`PLAN.md` to rebuild a position it already holds. With the conversation restored
the risk inverts — the danger is a worker trusting a stale last plan and redoing
finished work:

> You were interrupted mid-task and this session has been resumed. Before
> anything else, establish where you actually got to from `git log`, `git status`
> and any open PR on this branch — do not trust your transcript's last plan as
> your current position. Then continue from the first genuinely unfinished step.
> If this branch already has an open PR, push to it rather than opening a second.

Same no-apostrophe constraint as the existing launch strings
(`dispatch.sh:1075-1077`): all three single-quote the prompt inside a
double-quoted `tmux send-keys` argument.

Trailing free-form arguments append to this prompt, which is how you steer a
resume ("the review comments are the priority").

## Bus writes and the watchdog

In order:

1. A `kind:"resume"` row — `{crew_id, branch, worker_id, engine, model, session,
   continued:bool}`. New. It distinguishes a one-shot worker from one resumed
   four times, which is signal the ratings work
   (`2026-07-22-dispatch-worker-model-ratings-design.md`) currently cannot see:
   a resumed worker's cost and latency are otherwise attributed to a single run.
   Written via `_bus_append`, like every other row (`dispatch.sh:20-24`).
2. `crew status <worker_id> working "resumed"` — clears a stale
   `exited`/`failed`/`done` roster row.
3. The reattach message, when a live dispatcher was found.

Then `nohup crew stall-watch "$worker_id" --pane "$pane" --engine "$agent"` is
re-armed exactly as `dispatch.sh:1153` does. The original watchdog self-exited
when it saw the terminal state, and a resumed worker can wedge identically.

A new dispatch `session` id is issued (`dispatch.sh:831`), because the pane and
watchdog are new even when the conversation is not; `continued:true` on the
resume row is what ties it to the prior session.

## Testing

`tests/dispatch.bats` stubs `tmux`, `crew`, `gh` and `wt` with argv logging, so
the whole command is testable by hand-writing a `WORKER_TASK.md` and asserting
on the logged `send-keys` argv.

Cases:

- resolves engine/model/effort/tier/crew from the header; explicit flags override
- each engine's continue flag appears in the launch line; `--fresh` omits it
- `--mcp` reconstructed from the header once stamped; accepted as a flag otherwise
- missing `WORKER_TASK.md`, detached HEAD, default branch → refusals
- reuses an existing pane at the worktree; opens a window only when none exists
- liveness: absent crew dir / dead pid / live pid (via `$$`) → solo, solo, reattach
- reattach rewrites `dispatcher_pane:` and sends the message; solo does neither
- the task body is byte-identical after a resume
- the tier↔model gate is skipped without `--model` and enforced with it
- `stall-watch` is re-armed with the new pane

Harness note: the `crew` stub returns nothing, so `crew identity`'s JSON is
empty under test — assert on the continue flag, model, effort and protocol path,
not on `agent_name`. `crew register` recording a pane belongs in
`tests/crew.bats`.

## Out of scope

- Resuming a worker from outside its worktree (`dispatch resume <branch>`).
  Deferred: the resolution rules are identical, but placement and the "am I
  standing in it" refusals are not, and the felt need is the in-worktree case.
- Any change to `crew adopt` / `crew crews`. The dispatcher-side story is done.
- Reviving a reaped worktree. `crew reap` gates on the PR having landed; a
  resume after that is a new dispatch, not a continuation.
$$
