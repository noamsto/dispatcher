# Worker protocol: plan

Phase file of `WORKER_PROTOCOL.md`: it binds like the core. References to "this protocol" or a section of it resolve through the core's Phase files table.

## Plan of record (does the plan already exist?)

`resume: true` is read first and outranks `plan:` (Resuming a killed run); so this applies only when not resuming or when resume artifacts are absent or contradicted by the tree. `plan:` is re-stamped on every dispatch, default `required`; read it from `WORKER_TASK.md` before any plan phase:

- `plan: provided`: the task doc holds the dispatcher's plan (root cause/mechanism, explicit file list, named approach, acceptance criteria) and is your plan of record. Skip the `spec-plan-critic` plan phase (see Scope): extract a bite-sized step list → execute → fast gate → review; both gates still run before push. Per Process authority (and the launch prompt), no override into re-planning.
- `plan: required` is binding: always run the tier's plan phase; a detailed doc is plan-drafting input, not a skip to execute. Plan phase redundant? Raise it on the bus (block→await, "Report to the bus"); never downgrade `required` to `provided` on your own judgement. After reading the plan critic's first verdict (`spec-plan-critic` subagent or `plan-critic` pane), on every engine, post the plan seam: `crew msg "$CREW_WORKER_ID" "review:$(crew id)" '{"seam":"plan","plan_critic_first_pass":"<accept|revise|reject>"}'`. `crew status` refuses `pr_open`/`done` without it (not under `resume: true` or after a `dispatch resume` of this branch), and `crew msg` refuses to post your review seam without it, so a skipped plan phase surfaces at the review gate rather than after the push. A skipped plan phase is never papered over: block→await.
- Field absent (legacy / hand-authored doc; the only case for extraction): self-assess by extraction, not judgement: (i) quote the exact file list, (ii) state the mechanism in one sentence quoting the doc, (iii) enumerate acceptance criteria as checkboxes. All three succeed → the extraction is your plan of record; proceed as `provided`, audit as `self-gate`, not `provided` (rule 5). Any fails → `required`.
- Re-entry (both skip paths): repo contradicts the plan of record at execute time (named file missing, approach doesn't fit) → stop improvising. Per Bounded plan-shaped recovery (provided/legacy contradiction transition): at the current rung consume the shared execute-time budget, set `replan_used = true` and `replanned = true` when the planning episode begins, run `spec-plan-critic` once, normally, write an `approach_abandoned` retro note. Budget spent → block instead, no `approach_abandoned` note. One fallback (like deep false-negative recovery), not a loop.
- Scope: only the plan phase is skipped. Deep may skip the plan-critic, never its spec-critic / orchestration consult (a task doc doesn't settle framing), except under `resume: true` (a recovered `SPEC.md` already passed spec-critic in the interrupted run of this task).
- Retain the checkpoint-peek after the extracted plan of record.

## Orchestration consult (deep only)

Check Resuming a killed run first. Unless resuming, before the plan phase decide once, in the worktree (never at dispatch time), whether a top-tier consultant decomposes the task, and which.

1. Survey (cheap, in-worktree): modules/packages touched, blast radius (shared interfaces, cross-cutting seams). One boolean: _does this need a stronger decomposition than an opus plan alone?_ A few `Grep`/`Glob` passes, no subagent.
2. If it trips, pick a consultant and consult. Judge fit per task; one line in the plan: which, why. Only consultants whose engine (in parentheses) passes the `dispatch --engines --in-budget` gate; neutral fit → opus if `claude` is in the roster, else the first available. One-shots work from any lead.

   | consultant | mechanism | lean |
   | ---------- | --------- | ---- |
   | opus (default; `claude`) | claude one-shot `--model opus`; claude lead: ephemeral Agent-tool subagent, `model: opus` | neutral fit |
   | fable (`claude`) | claude one-shot `--model fable`; claude lead: ephemeral Agent-tool subagent, `model: fable`, in this worktree (the only form with in-worktree write access) | architecture-heavy decompositions |
   | gpt-5.6-sol (`codex`) | codex one-shot; claude lead: read-only codex MCP injected into claude deep work-profile workers | non-claude-family decomposition, diverse from the opus planner |
   | grok-4.7-high (`cursor`) | cursor one-shot | third-family perspective |

   Commands and gate: Cross-engine one-shots. `consult_engine` names the family (`opus`|`fable`|`codex`|`cursor`). Same ask for every mechanism: read `WORKER_TASK.md` and the relevant code, produce the `DECOMPOSITION.md` body as `components` (each a stable id + one-line + `boundaries` may/must-not-touch + `risk` tag), `ordering` (dependency order, `∥` = parallel-safe), `interfaces` (contracts stable across the split). The Agent-tool form writes `DECOMPOSITION.md` at the worktree root itself; otherwise you write it there from the reply. A decomposition, not the plan: no plan-schema step tags. `DECOMPOSITION.md` must not name its author or the consulting engine (plan-critic reads it author-less; rule 2).
3. Fallback, a should, not a blocker: refusal, timeout or unavailable engine (not in `dispatch --engines --in-budget`) → drop the consult; plain plan path (`spec-plan-critic` plan schema), byte-identical to a non-consulted deep worker. A failed codex/cursor pick may first retry once with `opus` (`fable` if architecture-heavy) if `claude` is in `dispatch --engines --in-budget`. Never fail the worker on a missing consult (as the diverse-engine reviewer, "Code review gate"). Write a `consult_failed` retro note: consultant, reason.
4. Seed the plan: an existing `DECOMPOSITION.md` is a hard constraint for the `spec-plan-critic` plan phase; `plan-critic` checks conformance. You do not hand-author the plan.
5. False-negative recovery: no trip, then the plain plan exhausts the revision cap (rule 3) unaccepted → consult once now, re-plan from `DECOMPOSITION.md`: one attempt beyond the cap (cap + 1 total). Already consulted and cap exhausted → `escalations[]`, no extra attempt. Recovery consult refuses or times out → `escalations[]` and stop, no plain-path re-loop.

## Cross-engine one-shots (consult and diverse reviewer)

Any lead can reach another engine's model as a stateless, read-only shell one-shot, for the Orchestration consult and the diverse-engine reviewer ("Code review gate").

- Gate: `dispatch --engines --in-budget | grep -qx <engine>` (one per line, enabled and installed here, and not at ≥95% of a quota window or a limit per the budget cache). Absent engine, or a refusal, timeout, sandbox, network or auth failure → unavailable, a should, not a blocker: drop it (consult step 3; diverse-engine reviewer bullet).
- Read-only by construction; run from the worktree root. Keep the `env -u` prefix (else the nested SessionEnd/stop hook, `dispatch-notify.sh`, posts `exited` for the lead) and coreutils `timeout 540` (`gtimeout` on macOS; neither on PATH → unavailable). Shell tool timeout above it; lower ceiling → background and poll. Prompt as argument, context (a diff) on stdin (the claude one-shot has no Bash): `git diff "$base_ref"...HEAD | <one-shot> '<prompt>'`. The final message is stdout. Every prompt carries: never read `.env*`, credentials, or keys; never print a secret value.

  | engine | one-shot |
  | ------ | -------- |
  | claude | `env -u CREW_WORKER_ID -u CREW_ID timeout 540 claude -p --model <fable\|opus> --tools "Read,Grep,Glob" --no-session-persistence --no-chrome '<prompt>'` |
  | codex | `env -u CREW_WORKER_ID -u CREW_ID timeout 540 codex exec -m gpt-5.6-sol -s read-only --ephemeral '<prompt>'`; stdin is appended as a `<stdin>` block |
  | cursor | `env -u CREW_WORKER_ID -u CREW_ID timeout 540 cursor-agent -p --mode ask --trust --model grok-4.7-high '<prompt>'`; `--mode ask` keeps it read-only; never add `--force` (not read-only) |

- Consult use: the prompt is the consult step 2 ask (read `WORKER_TASK.md` and the relevant code); the reply is the `DECOMPOSITION.md` body itself (`components` / `ordering` / `interfaces`), not a plan-mode plan. You write `DECOMPOSITION.md` from it, author- and engine-free.
- Diverse reviewer pick: a different family from the lead, first available in `dispatch --engines --in-budget`. A reply citing no changed file is a failed one-shot: drop it.
  - pi lead → claude (`--model opus`), else codex.
- A `--roles reviewer=<other-engine>` pane is an explicit opt-in, not a default on non-claude deep leads: one-shots are stateless, bounded, need no extra tmux pane per deep worker, and work for `kind: review` workers (no grid).
- On pi the reviewer pane is the review gate, so a default pane would silently become the gate instead of an additive second opinion.
