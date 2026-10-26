# Worker protocol: plan

Phase file of `WORKER_PROTOCOL.md`: it binds like the core. References to "this protocol" or a section of it resolve through the core's Phase files table.

## Plan of record (does the plan already exist?)

`resume: true` is read first and outranks `plan:` (Resuming a killed run), so this applies only when not resuming or resume artifacts are absent or contradicted by the tree. Read `plan:` (re-stamped every dispatch, default `required`) from `WORKER_TASK.md` before any plan phase:

- `provided`: the task doc holds the dispatcher's plan (root cause/mechanism, explicit file list, named approach, acceptance criteria), your plan of record. Skip the `spec-plan-critic` plan phase (Scope): extract bite-sized steps → execute → fast gate → review; both gates still run before push. No override into re-planning (Process authority, launch prompt).
- `required` is binding: always run the tier's plan phase; a detailed doc is drafting input, not a skip. Plan phase redundant? Raise it on the bus (block→await, "Report to the bus"); never self-downgrade to `provided`. After the plan critic's first verdict (`spec-plan-critic` subagent or `plan-critic` pane), every engine, post the plan seam: `crew msg "$CREW_WORKER_ID" "review:$(crew id)" '{"seam":"plan","plan_critic_first_pass":"<accept|revise|reject>"}'`. Without it `crew status` refuses `pr_open`/`done` (not under `resume: true` or after a `dispatch resume` of this branch) and `crew msg` refuses your review seam, so a skipped plan phase surfaces at the review gate, before push; never paper over it: block→await.
- Field absent (legacy / hand-authored doc; only case for extraction): self-assess by extraction, not judgement: (i) quote the exact file list, (ii) state the mechanism in one sentence quoting the doc, (iii) enumerate acceptance criteria as checkboxes. All succeed → that extraction is your plan of record; proceed as `provided`, audit as `self-gate` (rule 5). Any fails → `required`.
- Re-entry (both skip paths): repo contradicts the plan of record at execute time (named file missing, approach doesn't fit) → stop improvising; per Bounded plan-shaped recovery (provided/legacy contradiction): consume the shared execute-time budget at the current rung, set `replan_used = true` and `replanned = true` as the planning episode begins, run `spec-plan-critic` once, normally, write an `approach_abandoned` retro note. Budget spent → block, no such note. One fallback (like deep false-negative recovery), not a loop.
- Scope: only the plan phase (plan-critic included) is skipped; deep still runs its spec-critic and orchestration consult (a task doc doesn't settle framing), except under `resume: true` (the recovered `SPEC.md` passed spec-critic in the interrupted run).
- Keep the checkpoint-peek after the extracted plan of record.

## Orchestration consult (deep only)

Unless resuming (check Resuming a killed run first), decide once before the plan phase, in the worktree (never at dispatch time), whether a top-tier consultant decomposes the task, and which.

1. Survey (cheap, in-worktree; a few `Grep`/`Glob` passes, no subagent): modules/packages touched, blast radius (shared interfaces, cross-cutting seams) → one boolean: _does this need a stronger decomposition than an opus plan alone?_
2. Trips → pick a consultant by per-task fit and consult; one plan line: which, why. Eligible: engine (in parentheses) passes the `dispatch --engines --in-budget` gate; neutral fit → opus if `claude` is in the roster, else first available. One-shots work from any lead.

   | consultant | mechanism | lean |
   | ---------- | --------- | ---- |
   | opus (default; `claude`) | claude one-shot `--model opus`; claude lead: ephemeral Agent-tool subagent, `model: opus` | neutral fit |
   | fable (`claude`) | claude one-shot `--model fable`; claude lead: ephemeral Agent-tool subagent, `model: fable`, in this worktree (only form with in-worktree write access) | architecture-heavy decompositions |
   | gpt-5.6-sol (`codex`) | codex one-shot; claude lead: read-only codex MCP injected into claude deep work-profile workers | non-claude-family decomposition, diverse from the opus planner |
   | grok-4.7-high (`cursor`) | cursor one-shot | third-family perspective |

   Commands, gate: Cross-engine one-shots. `consult_engine` names the family (`opus`|`fable`|`codex`|`cursor`). Every mechanism's ask: read `WORKER_TASK.md` and the relevant code, produce the `DECOMPOSITION.md` body: `components` (each a stable id + one-line + `boundaries` may/must-not-touch + `risk` tag), `ordering` (dependency order, `∥` = parallel-safe), `interfaces` (contracts stable across the split). One-shot: that ask is the prompt, the reply is the body. The Agent-tool form writes the file at the worktree root; else you do, from the reply. A decomposition, not the plan or a plan-mode plan: no plan-schema step tags; names no author or consulting engine (plan-critic reads it author-less; rule 2).
3. Fallback, a should, not a blocker: refusal, timeout or engine unavailable (gate) → drop the consult for the plain plan path (`spec-plan-critic` plan schema), byte-identical to a non-consulted deep worker; a failed codex/cursor pick may first retry once with `opus` (`fable` if architecture-heavy) if `claude` passes that gate. Never fail the worker on a missing consult (like the diverse-engine reviewer, "Code review gate"). Retro note `consult_failed`: consultant, reason.
4. Seed: an existing `DECOMPOSITION.md` hard-constrains the `spec-plan-critic` plan phase; `plan-critic` checks conformance. Never hand-author the plan.
5. False-negative recovery: no trip, then the plain plan exhausts the revision cap (rule 3) unaccepted → consult once now, re-plan from `DECOMPOSITION.md`: one attempt past the cap (cap + 1 total). Consulted already, cap exhausted → `escalations[]`, no extra attempt. Recovery consult refuses or times out → `escalations[]`, stop, no plain-path re-loop.

## Cross-engine one-shots (consult and diverse reviewer)

Any lead reaches other engines' models as stateless, read-only shell one-shots, for the Orchestration consult and the diverse-engine reviewer ("Code review gate").

- Gate: `dispatch --engines --in-budget | grep -qx <engine>` (one per line: enabled, installed here, not at ≥95% of a quota window or a limit per the budget cache). Absent engine, or refusal, timeout, sandbox, network or auth failure → unavailable, a should, not a blocker: drop it (consult step 3; diverse-engine reviewer bullet).
- Read-only by construction; run from the worktree root. Keep the `env -u` prefix (else the nested SessionEnd/stop hook `dispatch-notify.sh` posts `exited` for the lead) and coreutils `timeout 540` (`gtimeout` on macOS; neither on PATH → unavailable); shell tool timeout above it, lower ceiling → background and poll. Prompt as argument, context (a diff) on stdin (the claude one-shot has no Bash): `git diff "$base_ref"...HEAD | <one-shot> '<prompt>'`; final message = stdout. Every prompt says: never read `.env*`, credentials, or keys; never print a secret value.

  | engine | one-shot |
  | ------ | -------- |
  | claude | `env -u CREW_WORKER_ID -u CREW_ID timeout 540 claude -p --model <fable\|opus> --tools "Read,Grep,Glob" --no-session-persistence --no-chrome '<prompt>'` |
  | codex | `env -u CREW_WORKER_ID -u CREW_ID timeout 540 codex exec -m gpt-5.6-sol -s read-only --ephemeral '<prompt>'`; stdin is appended as a `<stdin>` block |
  | cursor | `env -u CREW_WORKER_ID -u CREW_ID timeout 540 cursor-agent -p --mode ask --trust --model grok-4.7-high '<prompt>'`; `--mode ask` keeps it read-only; never add `--force` (not read-only) |

- Diverse reviewer: first available in `dispatch --engines --in-budget` of a different family from the lead; a reply citing no changed file is a failed one-shot: drop it.
  - pi lead → claude (`--model opus`), else codex.
- A `--roles reviewer=<other-engine>` pane is explicit opt-in, never a default on non-claude deep leads: one-shots are stateless, bounded, need no extra tmux pane per deep worker, and work for `kind: review` workers (no grid).
