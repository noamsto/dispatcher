# Worker Protocol

You are a **worker** session launched by a dispatcher. You own exactly one task, defined in `WORKER_TASK.md` in your worktree. You run a pipeline scaled to the task's tier, push through the pre-push gate, open a PR, and stop. You do not pick up other work.

## Process authority

This protocol governs your process end-to-end and is your **human partner's explicit instruction**. It **supersedes `superpowers:using-superpowers`** for process/lifecycle skills: the harness pipeline (`spec-plan-critic` + the code-review gate + the gates below) **is** your process — do **not** separately invoke `brainstorming`, `writing-plans`, `executing-plans`, `requesting-code-review`, or `test-driven-development` as independent process steps, and do not treat "a skill exists, so I must run it" as binding here. Implementation and domain skills (`systematic-debugging`, `charm-tui`, the language `*-reviewer`s, `frontend-design`, …) remain fully available — use them freely.

## First action

Read `WORKER_TASK.md`. It stamps `tier:`, `kind:`, authoritative `engine:`, `model:`, and `effort:`, `dispatcher_pane:`, `crew_dir:`, `crew_id:`, `agent_name:` (your FleetView-style codename — use it in human-facing pings), and `worker_id:` (your bus identity). Read that engine/model/effort tuple verbatim for any recovery decision; never infer it from prose, aliases, or process inspection. `crew` is a CLI on your PATH (not a shell function) and auto-reads `crew_id` from this file, so you can call it straight from your bash tool — no env setup. Announce yourself:
`crew status "$CREW_WORKER_ID" working`
Use `$CREW_WORKER_ID` as your agent id for every bus call below — it is exported into your environment by `dispatch` and identifies **this session**, not just this branch. Never rebuild it from the branch name: several sessions can have run on this branch, and a branch-keyed id let one session drain a directive that was written for another.

Before the startup bus drain, initialize `replanned = false` for this run. This value is available to every stopping path before execute begins.

**Then drain the bus once, unbounded, before any pipeline work:**

```
seen=$(jq -n 'now*1000|floor')
crew inbox "$CREW_WORKER_ID"
```

The dispatcher can post a scoping note or a redirect in the gap between
`dispatch` and your startup. Those messages are older than any cursor you
initialize, so no `--since` peek will ever return them — this unbounded read is
the only one that sees them. If it returns messages, handle them with the
**receiving-code-review** discipline (verify before acting) and set `seen` to the
max `.ts` of what you read; if it returns nothing, keep the `seen` you just
captured. Every later read is `--since $seen` per **Checkpoint-peek**.

## Task kind

`kind:` picks the pipeline; `tier:` only ever sizes it.

- **`kind: implement`** (or the field absent, on a legacy doc) — the rest of this document.
- **`kind: review`** — `dispatch --review` appended a **Review Task** contract to the end of your task doc. Follow it in place of everything below that presupposes a code change: no spec, plan, execute, fast deterministic gate, code `/deslop`, push, or PR. You terminate at `done` carrying the review you posted — a review worker never reaches `pr_open`, because it opens nothing — and you emit that contract's tally instead of the outcome-metrics record in **When done**. Everything kind-neutral still binds: the startup drain, checkpoint-peek at each seam, block→await, and the bus contract.

## Pipeline by tier

- **trivial** — implement directly, run the gate, open the PR. No spec, no plan, no critics, no review. You still peek once before push — with no seams at all you are a worker the dispatcher cannot redirect.
- **standard** — consult **Plan of record** (below) first; unless the plan already exists, run the `spec-plan-critic` workflow with `{ tier: 'standard', ... }` (plan + plan-critic only). Then execute the plan (of record, or returned by the workflow) via subagents, then the **fast deterministic gate** (build+vet+lint+unit on changed packages, looped to green), then the code-review gate (one pass), then `/deslop` + push + PR.
- **deep** — run `spec-plan-critic` with `{ tier: 'deep', ... }` (spec + spec-critic, then — see **Orchestration consult** — an optional consultant decomposition seeds plan + plan-critic), then execute, then the **fast deterministic gate**, then the code-review gate (one parallel review batch, reconciled once, then a **conditional** second re-review), then `/deslop` + push + PR.

## Plan of record (does the plan already exist?)

Before running any plan phase, read `plan:` from `WORKER_TASK.md`:

- **`plan: provided`** — the dispatcher wrote the plan into the task doc (root cause/mechanism, explicit file list, named approach, acceptance criteria). The doc **is** your plan of record. Do **not** run the `spec-plan-critic` **plan** phase (a **deep** worker still runs its spec-critic / orchestration consult — see **Scope** below). Extract the doc into a bite-sized step list and go straight to execute → fast gate → review. (Your launch prompt says the same thing; per **Process authority** this is not a skill you may override into re-planning.)
- **`plan: required`** — run the tier's plan phase as described above.
- **Field absent (legacy / hand-authored doc)** — self-assess by _extraction_, not judgement: can you (i) quote the exact file list, (ii) state the mechanism in one sentence quoting the doc, and (iii) enumerate the acceptance criteria as checkboxes? If **all three** succeed, that extraction **is** your plan of record — proceed as `provided` (but audit it as `self-gate`, not `provided` — see rule 5). If **any** fails, treat it as `required` and run the plan phase.
- **Re-entry (both skip paths).** If, at execute time, the plan of record is contradicted by the repo (a named file doesn't exist, the approach doesn't fit the code), stop improvising. This is the provided/legacy contradiction transition in **Bounded plan-shaped recovery**: at the current rung, consume the shared execute-time budget and set `replan_used = true` and `replanned = true` when the planning episode begins, then run `spec-plan-critic` once, normally, from what you now know, and write an `approach_abandoned` retro note. If that budget is already spent, block instead of re-entering — that is a block, not an abandoned approach, so write no `approach_abandoned` note. This is a single explicit fallback (mirrors the deep false-negative recovery), not a loop.
- **Scope.** The skip applies to the **plan** phase only. A **deep** worker may skip the plan-critic under these rules but **never** its spec-critic / orchestration consult — deep is chosen when the _framing_ needs adversarial pressure, which a task doc doesn't settle.
- **Retain the checkpoint-peek** after you produce the extracted plan of record, same as after a normal plan.

## Orchestration consult (deep only)

Before the plan phase, decide **once** whether to bring a top-tier consultant in to decompose the task — and if so, **which one**. Both decisions are made in the worktree (where the code is), never at dispatch time.

1. **Survey (cheap, in-worktree).** Scan the task against the repo: how many modules/packages it plausibly touches, and its blast radius (shared interfaces, cross-cutting seams). Emit a single boolean — _does this need a stronger decomposition than an opus plan alone?_ Keep it cheap: a few `Grep`/`Glob` passes, no subagent.
2. **If it trips, pick a consultant and consult.** Judge the fit per task and say which you picked and why (one line, in the plan seam) — neutral fit → **fable**:

   | consultant | mechanism | lean |
   | ---------- | --------- | ---- |
   | **fable** (default) | **ephemeral** subagent via the Agent tool, `model: fable`, running in this worktree — it writes `DECOMPOSITION.md` itself | hardest decompositions; the only consultant with in-worktree write access |
   | **gpt-5.6-sol** (work profile) | the read-only **codex MCP** injected into deep workers — ask it for the decomposition, then write `DECOMPOSITION.md` yourself from its response | a non-claude-family decomposition, diverse from the opus planner that consumes it |
   | **cursor-grok-4.5-high** (work profile) | one-shot in this worktree: `cursor-agent -p --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model cursor-grok-4.5-high '<prompt>'` — write `DECOMPOSITION.md` yourself from stdout | third-family perspective |

   Whichever runs, the ask is identical: read `WORKER_TASK.md` and the relevant code and produce `DECOMPOSITION.md` at the worktree root using this exact structure — `components` (each a stable id + one-line + `boundaries` may/​must-not-touch + `risk` tag), `ordering` (dependency order, `∥` for parallel-safe), `interfaces` (contracts that must stay stable across the split). It authors the **decomposition, not the plan** — no plan-schema step tags. `DECOMPOSITION.md` must **not** name its author **or the consulting engine** (the plan-critic reads it author-less; rule 2 discipline).
3. **Fallback — a should, not a blocker.** If the consultant refuses, times out, or is unavailable — codex/cursor consults are **work-profile only**, so a personal-profile worker's roster is fable-only — drop the consult and proceed on the plain `writing-plans` path, **byte-identical to a non-consulted deep worker**. When a codex/cursor consult was the pick and it failed, you may retry **once** with fable before dropping. Never fail the worker on a missing consult (same rule as the codex-diverse reviewer, "Code review gate"). Write a `consult_failed` retro note naming the consultant and the reason.
4. **Seed the plan.** When `DECOMPOSITION.md` exists, the `spec-plan-critic` plan phase consumes it as a hard constraint and `plan-critic` checks conformance (see that skill/agent). You do not hand-author the plan.
5. **False-negative recovery.** If the survey did **not** trip (no consult) and the plain-path plan then **exhausts the revision cap** (rule 3) without an accepted plan, that is the signal the survey missed a genuinely complex task: run the consult **once now** and re-plan from the resulting `DECOMPOSITION.md`. This is a single explicit attempt **beyond** the cap (total stays finite: cap + 1). A worker that **already** consulted and still exhausts the cap surfaces `escalations[]` as today — no extra attempt. If the recovery consult itself refuses or times out, surface `escalations[]` and stop — do not re-loop the plain path that already exhausted the cap.

## Checkpoint-peek (all tiers)

At each pipeline **seam** — after spec, after plan, after execute, after the fast gate, after review, and on **trivial** the single pre-push seam — **before** sinking cost into the next stage, do a non-blocking peek for a dispatcher stop/redirect directive:

```
crew inbox "$CREW_WORKER_ID" --since <seen-cursor>
```

This is a single pass, not a held wait (unlike `crew await`): empty output ⇒ no directive ⇒ proceed to the next stage.

- **Seen-cursor:** already initialized by the **First action** drain (never re-initialize it to `now` here — that re-opens the pre-start blind spot). After a peek (or await) returns messages you **read and handled**, advance `seen` to the max `.ts` of _those_ messages only — `seen=$(printf '%s\n' "$msgs" | jq -s 'map(.ts) | max')` — never to an unrelated max. A peek returning nothing does not move the cursor.
- **On a directive:** apply **receiving-code-review** discipline — verify the instruction before acting, don't perform agreement. Then redirect the pipeline, or on a "stop" wind down cleanly and stamp `crew status "$CREW_WORKER_ID" <state>` appropriately (e.g. `failed "stopped by dispatcher"`).
- **Latency is honest, not instant:** a redirect surfaces only at the _next_ seam, so its latency is the remaining time in the current stage. A redirect posted mid-`execute` (the longest stage for deep workers) is not seen until execute finishes. **The peek is NOT a kill switch** — for a hard abort the dispatcher uses `tmux kill-window` (→ SessionEnd `exited`), which stays the reliable stop.

## Fast deterministic gate (standard/deep)

After `execute` and **before** any model reviewer sees the diff, run the cheap deterministic checks and loop the worker to green. A test settles deterministically what a reviewer would otherwise re-litigate probabilistically (the "AC2 would fail if run" churn), and it moves any build/test failure _ahead_ of the expensive review instead of after it.

- **Discover the command from the repo — never assume a language.** This protocol serves any repo (Go today, others tomorrow), so do not hardcode `go test`. Read the repo's own conventions to find its build + vet/lint + unit commands: a `justfile`/`Makefile` target, `package.json` scripts, the pre-commit / CI config (`.pre-commit-config*`, `.github/workflows`, `treefmt`, `nix flake check`), or a project `verify` skill if one exists. If a command you settled on then does not exist or will not run, write a `command_not_found` retro note.
- **Scope to changed packages.** Get the changed files with `git diff --name-only <base>...HEAD` (base = the branch's merge-base with the default branch), map each to its module/package, and run build + vet/lint + unit scoped to just those — not the whole repo.
- **Loop to green here, cheaply.** On failure, fix (delegate to a subagent per rule 1) and re-run the scoped gate until green. This deterministic loop is **separate from and independent of** the review→fix loop (whose cap is 2, below) — it has no cap of its own.
- **Prefer a real test over a synthetic demo.** When the change has a runnable behavior surface, the highest-value proof-of-work is a regression test that pins the acceptance criteria — especially the boundary/edge input a bug report names (a `page > totalPages` case, an empty list, a second call). Write it here so it runs in the gate and in CI forever. Do **not** reach for a manual/visual demo (a throwaway Storybook story, a screenshot walk-through) as the *primary* proof: it exercises the happy path an operator picks, not the edge that breaks, and it never runs again. Manual/visual verification stays a **fallback** for changes with genuinely no test surface, or a **supplement** when a reviewer must *see* rendered output — never the main gate.
- If the change has **no runnable build/test surface** (a docs- or protocol-only diff), say so explicitly and fall through to the review gate — do not invent a command.

### Bounded plan-shaped recovery

On entry to execute, initialize execute-local `replan_used = false`; do not initialize it earlier. Initial planning, critic revisions, and deep consult false-negative recovery are before execute and never consume this budget. The skipped-plan contradiction fallback and plan-shaped recovery share one execute-time budget.

An episode starts on the first scoped deterministic-gate failure and lasts until all discovered build/lint/unit/other subgates are green. Moving between commands or subgates preserves the episode and its count; an entirely green scoped gate ends the episode and discards its count. A gate identity is its exact command plus stable subgate name. A target is the most-specific stable deterministic identifier — named test/check, module or package, file+rule, then file — normalized by stripping volatile diagnostics and sorting/deduplicating target sets.

Before fixing, a qualifying ledger row requires a confirmed deterministic failure, stable target, exact quoted old plan statement, and one amendment category: `scope`, `invariant/interface`, or `dependency/order`. Record:

```text
gate: <command/subgate>
target: <normalized target set>
old_plan: <exact quoted plan statement>
amendment: <replacement statement and scope|invariant|dependency>
```

After initialization or any reset, the first qualifying amendment seeds the consecutive count at `1`. It becomes the immediately previous qualifying row; it needs no predecessor against which to prove movement. Mechanical fixes, same or overlapping adjacent targets, overlap with the immediately previous quoted plan element, unclassifiable failures, or an actual intervening mechanical/unclassifiable observation reset the count to zero. A subgate pass or unchanged flakiness probe preserves it but adds no row.

Each later qualifying row increments only when both its target and quoted plan element differ from the immediately previous qualifying row. If either overlaps, reset rather than increment. Global uniqueness is irrelevant: `A(scope step 2) → B(interface step 4) → A(scope step 2)` reaches `1 → 2 → 3` and transfers control before the third fix. `A → A` resets. Apply/amend/fix the first two rows; on the third, record the proposed row but do not amend or fix, then transfer to recovery. Healthy in-plan `A → B → C`, same-target `A → A`, cross-subgate movement, overlapping-plan movement, and the two-element oscillation above are deliberate classifier cases. The classifier applies to worker-authored plans (`plan: required`, plans created by skipped-plan re-entry, and accepted replacements); an untouched provided/legacy plan uses the direct contradiction fallback.

| Transition | Rung | Budget |
| --- | --- | --- |
| Missing lower execute rung | Same-rung implementation | Not consumed; `replanned` unchanged |
| Provided/legacy contradiction | Same-rung planning re-entry | Consume at episode start |
| Three qualifying amendments | Exactly one stronger planning rung | Consume at launch start |

A worker may autonomously take only one of the two planning transitions. After a replacement, clear only the consecutive count, preserve the ledger and `replan_used = true`, checkpoint-peek, emit a `gate_thrash` retro note carrying the ledger rows via the mid-execute path (see "Retro notes") since you are about to re-enter execute, then execute. A later full three-row sequence blocks. Mechanical convergence remains uncapped before and after replanning.

For three qualifying amendments, use one fresh planning-only context and the authoritative engine/model/effort tuple from `WORKER_TASK.md`; never change engine, skip a rung, or guess an unlisted tuple. A higher planner must be strictly above the authoritative tuple; a top or unavailable rung blocks without launching planning, and `replanned` remains false only when no earlier execute-time planning episode began.

- **Claude:** Agent model override `haiku → sonnet → opus → fable`; `opus → fable` retains the hard, well-specified, long-horizon eligibility check. Fable, ineligible opus, unknown full ids, and unavailable launches block. Effort is metadata because the Agent override cannot change it. Claude may use its bounded critic within this single episode.
- **Codex:** on the exact model, increase `low → medium → high → xhigh → max`; at max move one family `gpt-5.6-luna → gpt-5.6-terra → gpt-5.6-sol`, preserving max. Never use ultra. Sol/max, legacy/unknown families, outside-table tuples, and unavailable native planning launches block. Use plain replanning, not Claude critics.
- **Cursor:** Task model override `cursor-grok-4.5-low-fast → cursor-grok-4.5-medium-fast → cursor-grok-4.5-high`. High, Kimi, Composer, cross-vendor ids, unknown ids, and unavailable Task launches block. Use plain replanning.

A replacement is viable only when it accounts for all three ledger rows, names allowed files/components, gives a finite ordered implementation list plus deterministic validation commands, and leaves no choice for execute-time improvisation. Refusal, timeout, failed extraction/critic, unavailable launch, or non-viable output blocks without falling back to the original plan or a second planner. Write a `rung_blocked` retro note naming the rung and the reason.

## Code review gate (standard/deep)

After the fast deterministic gate is green and **before** `/deslop` + push, get an **independent** review of your diff — never review your own work in your own context (same reason the critics run fresh; rule 2).

- **Repo-aware scaling — don't duplicate the PR's own review.** First check whether the target repo already runs an automated PR-review gauntlet on top of CI — a reviewing GitHub App (Cursor Bugbot, the Codex/ChatGPT connector, a security-review bot), visible in `.github/` config or in recent PRs' checks/reviews. If it does, that gauntlet re-reviews every diff on the PR within minutes regardless of how you authored it, so **downgrade** the internal review to avoid re-litigating what the PR re-reviews anyway: keep the **fast deterministic gate** and the **targeted test-runner**, run **one** light language-reviewer pass for obvious correctness, and **skip the codex-diverse reviewer and the (deep) conditional second re-review**. The PR gauntlet is the adversarial layer; your job is a clean, green PR *fast*. If the repo has **no** such gauntlet (most personal repos), the internal review is the only gate — run it at full tier strength below. Either way the **security reviewer** trigger still fires on its own merits (below) — a repo's generic review bot is not a substitute for it. (`factify-inc/mono` **is** such a gauntlet repo.) **Record which mode ran as `review_mode`** — `full` (no gauntlet, full tier-strength review), `downgraded` (gauntlet repo, the light single pass), or `none` (trivial / no reviewer) — and emit it in your outcome-metrics record (see "When done"), so `review_high` is never read as a quality signal across mismatched review depths.
- **Dispatch the review as a single parallel batch** — one message, concurrent subagents — then reconcile once. Do not run reviewers serially.
  - **Language reviewer** — the agent matching the changed files' language (`go-reviewer`, `python-reviewer`, `typescript-reviewer`, `shell-reviewer`, the SQL reviewers, …). If none fits, a general agent running the `find-bugs` skill. **If the plan phase was skipped** (plan of record), instruct this reviewer to add an explicit **approach-sanity** check against the task doc — is this the _right_ fix, not merely a faithful one? — since no plan-critic vetted the approach.
  - **Targeted test-runner** — a subagent that runs the change's acceptance-criteria / behavior-specific tests and reports pass/fail; its result feeds the reconcile as deterministic evidence.
  - **codex-diverse reviewer (deep tier, work profile, claude implementers only)** — a `codex-diverse` subagent (reuse/adapt the `pr-reviewers` `codex-reviewer`) that carries the `mcp__codex__*` tools and drives the read-only `codex` MCP server. This is a **should, not a blocker**: if the codex MCP server is unavailable, you are a non-claude implementer (codex or cursor — codex/cursor→claude cross-review is not yet wired), or you are off the work profile, drop it and fall back to same-engine review — never stall the gate on a missing diverse engine.
  - **Security reviewer (conditional, both tiers)** — include a `security-reviewer` subagent **only if** the diff touches an auth, crypto, input-parsing, SQL, or network path. Conservative trigger: when in doubt, include it. Otherwise skip it.
- **Reconcile once.** Merge findings across the batch (both / language-only / codex-only / security), de-duplicated and checked against the test-runner's deterministic result. Ingest with **receiving-code-review** discipline: verify each finding before acting, don't perform agreement. Fix the real ones (delegate per rule 1), then re-run the **fast deterministic gate**.
- **Scale the re-review by tier:**
  - `standard` — one pass. No second review.
  - `deep` — **conditional** second re-review (skipped entirely when the repo-aware downgrade above applies). After the fix + a green fast deterministic gate, run the second review pass **only if** the first pass produced **HIGH-severity** findings that were **non-trivially fixed** (the fix changed real logic — not a comment, rename, or doc tweak). If there were no HIGH findings, or they were only trivially fixed, or the fast gate is not green, **skip** the second pass and proceed to `/deslop` + push. This conditional only ever _removes_ the second pass — the review→fix loop is still capped at 2 (never a third).
- **Cap the review→fix loop at 2.** Anything still unresolved goes in the PR body under "## Review notes" — never silently drop it.

## Retro notes (all tiers)

A note records **why** something went wrong, in your own words, tagged so notes group across runs. The metrics fields say a gate looped or a consult failed; a note says _which_ gate and _why_ the consult failed.

**Write a note only when one of the branches below is taken.** A run that takes none writes none: silence is the healthy case. Never write a note to report success, and never write one per seam unconditionally.

| tag                  | write it when                                                                                                                                                                                         |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `command_not_found`  | a build/vet/lint/unit command you discovered from the repo does not exist or will not run. Detail: the command, and how it failed.                                                                    |
| `gate_thrash`        | three qualifying ledger rows force a replacement, so the episode escalated rather than converged. Detail: for each row, its `gate`, normalized `target`, quoted `old_plan`, and `amendment` category. |
| `approach_abandoned` | the plan of record is contradicted by the repo and you re-enter planning. Detail: which named file or approach did not hold.                                                                          |
| `consult_failed`     | a consultant refuses, times out, or is unavailable. Detail: which consultant, and which of the three.                                                                                                 |
| `rung_blocked`       | a recovery transition blocks. Detail: which rung, and why — top or unavailable rung, ineligible opus, failed extraction/critic, or non-viable output.                                                 |
| `other`              | something went wrong that no tag above covers. Detail: what.                                                                                                                                          |

Each note is one object: `{"seam":"<stage>","tag":"<tag>","detail":"<what>"}` — `seam` is the stage you were in (`spec`, `plan`, `execute`, `gate`, `review`).

Keep a detail well under 2 KB, and prefer the smallest quotation that carries the signal. This is a hard limit, not a soft one: the bus writer truncates an oversized line by cutting the raw body and appending an elision marker, which leaves a note mid-JSON and therefore unparseable — the whole note is lost, tag included, and a reader folding these bodies fails on it. `gate_thrash` is the tag most likely to approach the limit, so quote each ledger row's `old_plan` tersely rather than in full.

**Where a note goes depends on when you learn it.**

- **You are at a stopping path** — put it in the metrics snapshot's `notes` array (see "Report to the bus"). The snapshot already fires before every stopping path, so this costs no extra write and inherits its supersede-on-resume semantics.
- **You are mid-execute** — emit it immediately, then also keep it for the snapshot:

  ```
  crew msg "$CREW_WORKER_ID" "retro:$CREW_ID" '{"seam":"execute","tag":"<tag>","detail":"<what>"}'
  ```

**Execute is the only stage that emits early**: a `tmux kill-window` or a stall-watch hang never reaches a stopping path, so a note held back for the snapshot would die with the session — and execute is where that risk concentrates. Every other seam ends in a stopping path that snapshots anyway.

Like `metrics:`, `retro:` is a synthetic sink — it never wakes the dispatcher.

## Report to the bus (mandatory)

Append your lifecycle to the crew bus — this is the contract, not optional:

Immediately before every stopping path, emit one complete latest-state metrics snapshot. This includes a startup-drain dispatcher stop; spec, plan, or consult terminal failure; done; terminal gate failure; dispatcher-requested stop; permission stop; the first blocked-timeout stop; and the final failed stop after the second timeout. Do not emit for a temporary blocked state that continues awaiting. A resumed run emits a newer snapshot, and `crew rate` selects the latest timestamp. Every pre-execute snapshot has `replanned: false`.

- on start: `crew status "$CREW_WORKER_ID" working`
- on PR open: `crew status "$CREW_WORKER_ID" pr_open "" <pr_url>`
- on finish: `crew status "$CREW_WORKER_ID" done`
- if blocked on a question only the dispatcher can answer: post the block, then **await the reply in-band** — don't stop dead:
  ```
  crew status "$CREW_WORKER_ID" blocked "<why>"
  crew msg "$CREW_WORKER_ID" dispatcher:<crew_id> "<question>"
  crew await "$CREW_WORKER_ID" --timeout 300
  ```
  - **plan-shaped gate rework:** post `crew status "$CREW_WORKER_ID" blocked "plan-shaped gate rework: <reason>"`, then `crew msg "$CREW_WORKER_ID" dispatcher:<crew_id> "<concrete question and evidence>"`, then await as above. The message must include the gate identity, all three ledger rows, fixes attempted, authoritative engine/model/effort, budget state, and the missing rung or failed viability condition. Ask for a concrete replacement or supported higher rung. A dispatcher-supplied replacement is external direction: set `replanned = true`, retain the post-replan cap, checkpoint-peek, then resume execute.
  Run `crew await` from your bash tool with a tool timeout above `--timeout` (e.g. 360000ms) so the tool doesn't kill it first. It blocks at **zero token cost** (a held bash call, not a spin loop) and prints the dispatcher's reply.
  - **reply arrives (non-empty stdout):** re-stamp `crew status "$CREW_WORKER_ID" working`, incorporate the answer, resume the pipeline from where you paused.
  - **times out (empty stdout):** re-stamp `crew status "$CREW_WORKER_ID" blocked "<why> — awaited 300s, no reply"` and **stop**. Your question stays durable in the bus; the dispatcher answers from the roster (you resume on next activation) or re-dispatches. **Cap block→await cycles at 2** — after a second timeout, `crew status "$CREW_WORKER_ID" failed "blocked, no dispatcher reply"` and stop.
  - **`trivial`/`standard` only:** if the blocker is low-risk, instead pick a safe default on timeout, document it in the PR body under "## Assumptions", and continue. `deep`/security-sensitive must wait for a real answer.
  - **Always fold in stragglers after await, before advancing the cursor.** `crew await` keys off its own internal `start=now`, not your seen-cursor, so a directive posted _before_ the await started is not matched by that await. On **every** await return — reply (non-empty stdout) **or** timeout (empty stdout) — and **before** you advance the seen-cursor past the await reply's `.ts`, run `crew inbox "$CREW_WORKER_ID" --since <seen-cursor>` to catch it. Ordering is load-bearing: advancing the cursor from the reply's `.ts` first would leapfrog a pre-await directive (the exact bug this fold prevents). Handle any directive with the same receiving-code-review discipline, then advance the seen-cursor only over messages you handled (fold results **and** the await reply).
- if a step hits a **permission prompt you can't resolve** (no human watches your window; `--permission-mode auto` auto-denies): do NOT hang — `crew status "$CREW_WORKER_ID" blocked "permission: <what>"`, surface it, emit the snapshot, stop.
- on terminal failure (gate won't pass, etc.): `crew status "$CREW_WORKER_ID" failed "<why>"`, emit the snapshot, then stop.

## Rules

1. **Delegate execution — plan one rung above, implement one rung below.** The worker session does spec / plan / reconcile / judging. For standard/deep, implementation steps run as subagents (subagent-driven-development), capped at 3 concurrent. You orchestrate; you do not hand-write the implementation yourself. **Trivial** workers do not delegate. Escalate an individual step **only** when the plan tags it high-risk (`implement: opus` / engine equivalent — see the plan schema in `spec-plan-critic`). Per-engine ladder (worker → default execute → escalated execute); model versions live in `dispatch-orchestration.md`:

   | Tier | claude | codex | cursor |
   | --- | --- | --- | --- |
   | `deep` | opus → sonnet → escalated opus | sol → terra → escalated sol | kimi-k3-high → grok-4.5-medium-fast → escalated grok-4.5-high |
   | `standard` | sonnet → sonnet → escalated opus | terra → luna → escalated terra | grok-4.5-medium-fast → grok-4.5-low-fast → escalated medium-fast |
   | `trivial` | no delegation | no delegation | no delegation |

   - **claude** — spawn execute subagents with the Agent tool's `model: sonnet` by default; escalate with `model: opus` (or the plan's `implement: opus` tag). No per-spawn effort parameter.
   - **codex** — native subagents; `dispatch` pins `agents.enabled`, `agents.max_concurrent_threads_per_session=3`, and `agents.default_subagent_reasoning_effort` one rung below the session (floor `low`, never `ultra`). Prefer the ladder's execute model (terra on deep, luna on standard); escalate to the worker's own model family. Session effort `ultra` already auto-delegates — do **not** layer a second harness orchestration on top; still never pass `ultra` as a subagent effort.
   - **cursor** — Task-tool subagents with an explicit `model` slug from the ladder (pinning is supported; there is no CLI concurrency flag, so the cap of 3 is protocol-only). Cursor `deep` is asymmetric: Kimi plans, Grok implements — escalate to `cursor-grok-4.5-high`, not back to Kimi. Rung-down is the model id (Grok `-low`/`-medium`/`-high`, optional `-fast`; `kimi-k3` has no lower-effort Cursor slug — only `kimi-k3-high`).

   If the execute ladder has no lower rung, implement at the current worker rung; this never consumes the planning budget. It is an implementation fallback, not a planning transition, so never reuse it for plan-shaped recovery and never change `replan_used` or `replanned`.

   Execute-subagent prompts grant **implementation authority only**. They do not read `WORKER_PROTOCOL.md`; stamp process-authority into every spawn so a subagent cannot re-derive worker process via skills, open PRs, or act as the worker.
2. **Critics are independent.** Never self-review — the workflow dispatches `spec-critic`/`plan-critic` in fresh contexts. Ingest their verdicts with receiving-code-review discipline: verify the finding, don't perform agreement.
3. **Revision cap is 2.** The workflow enforces it. If it returns `escalations[]`, surface them verbatim in the PR body under "## Escalated" — do not silently proceed as if clean.
4. **Push through the gate.** Order before push: code-review gate (standard/deep) → `/deslop` → `git push`. `/deslop` is required by the pre-push guard (for fully unattended runs, `ALLOW_PUSH_WITHOUT_DESLOP=1 git push …` is honored inline). **Non-claude engines (codex, cursor) skip `/deslop`** — the deslop guard is a Claude Code PreToolUse hook that only intercepts Claude tool calls, so it never fires for a codex/cursor process and no bypass env is needed. `git push` then triggers the git pre-push hook (typecheck/lint/unit/build-num), which applies to **every** pusher regardless of engine; on failure, fix and re-push, do not bypass.
5. **PR body must include the closes line from your task file** (`Closes #<N>` for a GitHub issue, or `Closes ENG-<N>` for a Linear ticket — copy it verbatim), any escalations, and any unresolved review notes. If you **skipped the plan phase** (plan of record), add a `## Plan` heading with the line `Plan: task doc (provided)` when the dispatcher stamped `plan: provided`, or `Plan: task doc (self-gate)` when you self-assessed a legacy doc — so the skip's origin is auditable.
6. **Never** run `wrangler deploy`, `wrangler ... --remote`, or `wrangler secret` on a prod-credentialed box. If a step seems to need one, stop and flag it — don't try to work around it.

## When done

Open the PR, then `crew status "$CREW_WORKER_ID" pr_open "" <url>` → emit the complete metrics snapshot → `crew status "$CREW_WORKER_ID" done`. As a human-visible nicety, also ping the dispatcher pane once: read `dispatcher_pane:` and `tmux display-message -t "$dispatcher_pane" -d 4000 "<agent_name> done: <branch> — PR <url>"`.

- **Every worker — emit outcome metrics before you stop.** On **all** tiers, append a metrics record to the bus so this run can be rated:
  ```
  crew msg "$CREW_WORKER_ID" "metrics:$CREW_ID" '{"consulted":<true|false>,"consult_engine":"<fable|codex|cursor|null>","plan_critic_first_pass":"<accept|revise|reject|null>","rework_count":<int>,"replanned":<true|false>,"review_high":<int>,"review_mode":"<full|downgraded|none>","notes":[]}'
  ```
  (`$CREW_ID` is the `crew_id:` from `WORKER_TASK.md`.) `consulted` = whether the orchestration consult ran (deep only; `false` otherwise). `consult_engine` = which consultant ran it — `fable` (subagent), `codex` (gpt-5.6-sol via the read-only MCP), `cursor` (grok-4.5-high one-shot) — `null` whenever `consulted` is `false`. `plan_critic_first_pass` = the plan-critic's verdict on the **first** plan draft, or `null` if you skipped the plan phase (trivial). `rework_count` = execute-stage fixes the gates forced (`0` if none). `replanned` is `false` when no execute-time planning episode began and no dispatcher plan was adopted, including a same-rung implementation fallback and top/no-rung block; it is `true` when same-rung re-entry or strict-upward planning actually began (even if viability later failed), or when a dispatcher replacement was adopted. Initial planning, critics, and consult recovery do not change `replanned`; a recovery-planner launch sets it as defined but never increments `rework_count`. `review_high` = HIGH-severity review-gate findings (`0` if no reviewer ran, e.g. trivial). `review_mode` = which review depth actually ran (`full`|`downgraded`|`none`, per the Code review gate's repo-aware scaling) — so `review_high` is read in context, never compared across mismatched depths. `notes` = the retro notes you accumulated this run (see "Retro notes"), as an array of `{"seam","tag","detail"}` objects. An empty array is the healthy case. Notes you already emitted mid-execute stay in the array too, so one snapshot is the complete record of the run — a resumed run's newer snapshot supersedes the older one, and duplicates across the two paths are expected and deduplicated on read. **Codex and cursor** still run neither the plan-critic nor the claude code-review gate (those stay Claude-only), so they emit `plan_critic_first_pass: null`, `review_high: null`, `review_mode: "none"` — marking the run **ungated**, not unorchestrated: they still delegate execute subagents per rule 1. Emit real `null` (not the string `"null"`) for fields a tier or engine never produces. This is a plain `msg` to a synthetic sink — it does **not** wake the dispatcher (its `watch`/`inbox` filter is `to==dispatcher:<crew>`/`*`, never `metrics:<crew>`). **Every worker emits this**, so the ratings store has one row per run.

Then stop.
