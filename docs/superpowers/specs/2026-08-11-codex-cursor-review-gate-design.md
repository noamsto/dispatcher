# Independent code-review gate for codex and cursor workers

**Status:** design — revision 3 (post-critic, second pass)
**Date:** 2026-08-11
**Repo:** dispatcher (personal / GitHub) · default branch `extract`
**Closes:** #7

**Artifacts touched:**

| file                                         | what changes                                                                                                                                                                                                                                  |
| -------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `adapters/core/protocols/WORKER_PROTOCOL.md` | code-review gate (per-engine mechanism, fresh-context contract, **the `review_mode` enum at `:137`**, unavailability path), the `trivial`/`standard` safe-default-on-timeout bullet, retro-note vocabulary, metrics snapshot template + prose |
| `adapters/cursor/rules/dispatcher.mdc`       | the carve-out bullet — **hand-maintained, never regenerated** (see D6)                                                                                                                                                                        |
| `adapters/core/dispatch.sh`                  | the `process_authority` clause, one string, both non-claude launches                                                                                                                                                                          |
| `scripts/gen-adapters.sh`                    | one stale comment asserting codex workers "stay ungated"                                                                                                                                                                                      |
| `README.md`                                  | capability table rows `:134` (`Subagents`) and `:136` (row split), footnote 3, the "process-light" paragraph, the roadmap bullet                                                                                                              |
| `tests/adapters.bats`                        | protocol + `.mdc` assertions, and the widened metrics template at line 245                                                                                                                                                                    |
| `tests/dispatch.bats`                        | review-subagent authority in both non-claude launch prompts                                                                                                                                                                                   |
| `tests/crew.bats`                            | end-of-pipe: an `unavailable` run lands in the ratings store as such                                                                                                                                                                          |
| generated trees                              | `scripts/gen-adapters.sh`                                                                                                                                                                                                                     |

## Problem

`tier` is documented as the pipeline-depth lever — `standard` buys a plan critic plus a review gate, `deep` buys spec+plan critics plus a review gate. `WORKER_PROTOCOL.md` then exempts codex and cursor workers from **both**, and instructs them to close out with:

```json
{ "plan_critic_first_pass": null, "review_high": null, "review_mode": "none" }
```

So tier is **misleading across engines**. A dispatcher that picks `deep` for a security-sensitive change believes it bought two critics and a review gate; routed to codex it bought neither. Measured, not hypothetical — one crew dispatched lazytmux #243 (standard/codex), #167 (deep/cursor), #158 (standard/codex). All three produced green PRs, none ran an independent reviewer, all three emitted `review_mode: "none"`. The deep cursor worker consulted Fable, but a decomposition consult is planning support, not post-implementation review.

The exemption was correct when written: codex and cursor genuinely ran single-agent, so there was no fresh context to review from. Issue #1 removed that constraint. For codex the mechanism is wired at launch — `dispatch.sh` pins `agents.enabled=true` and `agents.max_concurrent_threads_per_session=3`. For cursor it is available but **not** pinned: the launch line passes no concurrency or delegation flag, and rule 1 already says outright that cursor's cap of 3 is protocol-only. Either way the capability exists; only the protocol text still says otherwise.

## Goal

Make `tier` mean the same thing on every engine, for the **post-implementation code-review gate only**:

- `trivial` may keep `review_mode: "none"`.
- `standard` and `deep` codex/cursor workers **must** run an independent, fresh-context review before pushing.
- A required reviewer that cannot be spawned surfaces as **blocked/failed** — never a silent degrade to `none`.

That last point is the heart of it. A gate that quietly downgrades when it cannot run is _worse_ than no gate: the metric then reports a clean run, and `crew rate`'s rollup reads the absence of findings as quality. Design so the degraded path is loud in three places at once — the bus state, the metrics record, and the retro stream.

## Non-goals

- **Plan and spec critics for codex/cursor.** Deliberately out of scope per the issue. `plan_critic_first_pass` stays `null` on both engines, and every doc edit below preserves that half of the carve-out while deleting the review half.
- `DISPATCHER_PROTOCOL.md` (a worker is live in its budget section, #58), `adapters/core/crew.sh`'s stall-watch region (same worker), `_bus_append` (PR #60), `crew rate` (PR #62 — read to understand `review_mode`, never edited; D3 is designed so it needs no edit).
- Cross-engine diverse review (codex→claude, cursor→claude). Stays optional and unwired; same-engine independent review is the required baseline.
- **Redefining `review_high` for `trivial` claude workers.** They keep `0`. Codex/cursor trivial workers do change — today's carve-out is engine-conditioned, not tier-conditioned, so they emit `null`; they move to `0` for parity with claude. See D3 for why, and Risks for the store consequence.

## Design

### D1 — The gate binds to the role, the engine supplies the mechanism

Today the "Code review gate" section names Claude Code agents directly (`go-reviewer`, `security-reviewer`, `find-bugs`). Those are a **claude plugin registry** concept; codex has no declarable named agents at all, and cursor's Task tool takes a model slug, not an agent name. So the section reads as claude-specific even where its rules are engine-neutral, which is exactly how the metrics carve-out came to contradict it.

Keep the **roles** engine-neutral and unchanged (language reviewer, targeted test-runner, conditional security reviewer, optional diverse reviewer), and add a **per-engine mechanism table** saying how to spawn one — mirroring how rule 1 already handles the execute ladder:

| engine | reviewer mechanism                                                                                                                                  | reviewer rung                                                                                                                             |
| ------ | --------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| claude | Agent tool, the named `*-reviewer` agent matching the diff's language; `find-bugs` in a general agent when none fits                                | unchanged — each agent definition owns its model                                                                                          |
| codex  | native subagent (`agents.enabled`, cap 3), the role brief written into its prompt — codex has no named-agent registry, so the brief _is_ the prompt | the tier's **execute** model (deep → terra, standard → luna); effort is whatever `dispatch` pinned, since codex has no per-spawn override |
| cursor | Task-tool subagent with an explicit model slug, same inline role brief                                                                              | the tier's **execute** slug (deep → `cursor-grok-4.5-medium-fast`, standard → `cursor-grok-4.5-low-fast`)                                 |

**Why name the rung at all:** the _model_ is the ambiguity, and it bites hardest on cursor, whose `deep` ladder is asymmetric — Kimi plans, Grok implements — so "a subagent" does not say which family reviews. Reusing the execute rung resolves it without inventing a fourth ladder. Effort is named for codex only to be explicit that there is nothing to choose: `agents.default_subagent_reasoning_effort` is session-wide, already pinned one rung below the session by `dispatch`, and a reviewer inherits it whether or not the protocol says so. The claude column is deliberately untouched — its review path is not the bug, and re-plumbing it would widen the blast radius for nothing.

Concurrency stays capped at 3 on every engine. One parallel batch, reconciled once — unchanged. Everything else in the section stays byte-stable where it can: repo-aware downgrade, reconcile-once, tier-scaled re-review (`standard` one pass, `deep` conditional second), review→fix loop capped at 2, unresolved findings into `## Review notes`.

### D2 — Fresh context is a structural requirement, not a style note

A reviewer that can see the implementer's reasoning is not an independent check; it inherits the author's blind spots and rubber-stamps them. The protocol already says this for critics (rule 2); the review gate says "never review your own work in your own context" but does not say what a compliant spawn looks like on an engine with no named agents — which is where a worker under time pressure improvises a self-review.

State the spawn contract explicitly. A reviewer subagent receives **only**: the task doc, the diff (or the command to compute it), and its role brief. It must **not** receive the plan rationale, the spec, the implementer's notes, or a transcript of the execute stage. It carries **review authority only** — it does not fix, commit, push, open PRs, or act as the worker (the mirror of rule 1's implementation-authority clamp on execute subagents; D5 carries the same words into the launch prompt).

Then close the escape hatches **by name**, because each is cheaper than compliance:

1. **"Review it yourself in this context" is not a permitted fallback** on `standard`/`deep`. If no fresh context can be spawned, D3 fires.
2. **The `trivial`/`standard` safe-default-on-timeout allowance does not cover this blocker.** `WORKER_PROTOCOL.md` currently lets a blocked `trivial`/`standard` worker pick a safe default on timeout, document it under `## Assumptions`, and continue. A standard codex worker could classify "skip the review" as low-risk and reach `done` — defeating the acceptance criterion outright. Carve it out: **a missing review gate is never low-risk**; it follows block→await→`failed` on every tier.

This is what "structurally impossible to self-review" buys: not a stronger instruction, but the removal of every path that would have satisfied the instruction cheaply.

### D3 — Unavailability is loud in three places

When the engine cannot spawn a fresh-context reviewer — delegation disabled, Agent/Task spawn refused, the subagent dying before it reports — retry **once**, then stop. Do not push, do not open a PR, do not emit `none`.

1. **Bus state.** `crew status blocked "review gate unavailable: <what>"` → `crew msg` the dispatcher → `crew await`, on the block→await path the protocol already defines (cap 2 cycles, then `failed`).
2. **Metrics.** A fourth `review_mode` value: **`unavailable`**, on a `blocked`/`failed` snapshot only — never alongside `done`.
3. **Retro stream.** A new closed-vocabulary tag **`review_unavailable`**; detail = engine, mechanism attempted, how it failed.

**The `crew msg` body is specified, not left to taste.** `DISPATCHER_PROTOCOL.md` is a forbidden file and carries no review-gate text at all, so its only guidance for a blocked worker is "re-dispatch, intervene, or drop it" — nothing that would stop a dispatcher replying "just push it". The worker's own message is therefore the _only_ carrier of this rule, and it must be as prescribed as the plan-shaped-gate-rework message one section away already is. It must name: the engine and tier, the reviewer mechanism attempted, how it failed (including the one retry), and the two legal replies — **retry**, or **re-dispatch** (to an engine that can review, or as `tier: trivial`). It must state that proceeding unreviewed at this tier is not one of them.

**What the dispatcher may answer, and what it may not.** The legal replies are _retry_ or _re-dispatch_. The dispatcher may **not** authorize "proceed unreviewed at this tier" — that would need a `done` row with no honest `review_mode`, which is the original bug wearing a permission slip. If the work should land unreviewed, re-dispatch it as **`tier: trivial`**, where `none` is both legal and true. The tier is the lever; downgrade the tier, never the metric. This is why `unavailable` never co-occurs with `done`.

**Why a fourth enum value rather than reusing `none`.** `none` means "no reviewer was supposed to run" (trivial). Overloading it with "a reviewer was required and could not run" is precisely the bug this issue is about: `crew rate` and every human reading the rollup would see the two as identical. `crew rate` passes `review_mode` through untouched (`($m.review_mode // null)`), so a new value reaches the ratings store with no `crew.sh` edit — which is what keeps this a protocol-only merge.

**The enum has three sites, and the least obvious one is the actual hatch.** All three move together or the protocol contradicts itself:

- `:137` defines the enum inline and glosses the third value as **"`none` (trivial / no reviewer)"**. That parenthetical is the textual permission for the exact degrade D3 exists to close — a codex worker that failed to spawn a reviewer reads "no reviewer → `none`" and is _compliant_. Deleting the carve-out at `:263` while leaving this intact would leave the hatch open inside the very section that mandates the gate. The gloss narrows to **trivial only**, and `unavailable` joins the enum here.
- `:261` is the copy-paste template workers emit from; it becomes `"review_mode":"<full|downgraded|none|unavailable>"`. `tests/adapters.bats:245` asserts that exact substring and is updated in the same commit.
- `:263` is the prose definition plus the engine carve-out (D4).

**`review_high` on an `unavailable` snapshot is `null`, and must be said out loud.** The surviving prose glosses `0` as "no reviewer ran", which would make an implementer emit `0` on the very record D3 built to be loud — a zero-findings row for a run that found nothing because it never looked. `null` is the honest value: not "clean", but "unmeasured".

**The one-line invariant that makes the acceptance criteria testable:** on `standard` and `deep`, a `kind: implement` worker never validly emits `review_mode: "none"`, on any engine. (`kind: review` workers are excluded by construction — they run no review gate and emit the review contract's tally instead of this record.)

**`review_high` — what actually changes, corrected.** The current carve-out is **engine-conditioned, not tier-conditioned**: codex and cursor are told to emit `review_high: null` at _every_ tier, while claude's rule is "`0` if no reviewer ran, e.g. trivial". Removing the engine condition therefore does two things, and both are intended:

- `standard`/`deep` codex/cursor emit an integer, like any other engine — the point of the change;
- `trivial` codex/cursor move from `null` to `0`, reaching parity with trivial claude.

The alternative — special-casing trivial codex/cursor to keep `null` — would preserve one engine-conditioned branch in the name of removing engine-conditioned branches, and would leave two engines' trivial runs permanently non-comparable with claude's. The cost is a discontinuity against historical rows, bounded and stated in Risks. Flipping trivial _claude_ from `0` to `null` was considered and rejected outright: it is outside this issue and would break averages over the existing store.

### D4 — Delete the carve-out where it lives, keeping the critic half

The metrics section's closing sentence — codex and cursor "run neither the plan-critic nor the claude code-review gate (those stay Claude-only) … marking the run **ungated**" — is the sentence the incident traces to. Split it: critics stay claude-only, the review gate does not. Its **absence** is a regression test, since the bug was a sentence's presence.

The same claim is duplicated in four places outside the protocol, each edited the same way:

- `scripts/gen-adapters.sh` — the comment that codex workers "stay ungated (no plan-critic / code-review)".
- `README.md:136` — the table row `Worker: critics + review gate` conflates the two halves; split into `Worker: spec/plan critics` (✅ / ❌ / ❌) and `Worker: code-review gate` (✅ / ✅ / ✅). The `Subagents` row at `:134` needs its own fix — see below.
- `README.md:144-146` — "Process-light is a promise, not an omission … `review_mode: "none"`".
- `README.md:313` — the roadmap bullet "Closing the process-light gap so Codex and Cursor workers get a critic pipeline and a review gate" is now half-done; it narrows to the critic pipeline.

**The capability table's `Subagents` row (`README.md:134`) is the other half of the fix, and it is easy to target wrongly.** Footnote ² belongs to the _Slash commands_ row, not to subagents; the codex `Subagents` cell is a bare `❌` with no marker at all. Left alone, the most-read file in the repo would assert **Codex: Subagents ❌** on the same page as a protocol requiring a codex native subagent reviewer — the exact cross-document contradiction this issue is about. Fix the cell, not the wrong footnote: codex's `Subagents` cell becomes `⚠️ native only³`, and footnote ³ (today "Cursor has subagents, but not the model this pipeline is built on") widens to cover both engines — codex has native ad-hoc subagents but no _declarable_ plugin agents, cursor has both but not this pipeline's model. Reusing ³ avoids renumbering a footnote sequence for one added cell.

### D5 — The launch prompt carries authority for reviewers too

`dispatch.sh` stamps a `process_authority` clause into the codex and cursor launch prompts, because for those engines the spawn prompt is the sole carrier — their subagents never read `WORKER_PROTOCOL.md`. It currently says "When spawning **execute** subagents, grant **implementation authority only**".

Broaden it by role, not by blanket: execute subagents keep implementation authority only; review subagents get **review authority only**. Extending "implementation authority" to a reviewer would tell it to fix the code it was asked to judge. The protocol side of the same rule lives in D2's spawn contract, so prompt and protocol say the same thing in the same words.

### D6 — The one file the drift gate cannot see

`adapters/cursor/rules/dispatcher.mdc` carries the carve-out verbatim, including `review_mode: "none"`, and it has `alwaysApply: true` — so it is in **every** cursor session's context. It is the highest-authority stale text for one of the two engines this issue is about.

Critically, it is **not generated**: `scripts/gen-adapters.sh` rewrites `adapters/claude-code/plugin/commands`, `adapters/cursor/commands`, `adapters/codex/plugin/skills`, and the two plugin `protocols/` copies — `adapters/cursor/rules/` is hand-maintained and never touched. So "regenerate and the drift gate catches the rest" is **false for this file**, and nothing else in the repo would notice it going stale. Its bullet gets the same split as D4, and a `tests/adapters.bats` assertion — positive on the new text, negative on `review_mode: "none"` — is the only protection it will ever have.

## Verification boundary — stated plainly

Both non-claude engines are **work-profile only** and `dispatch` hard-aborts them on this personal host, so **no codex or cursor worker can be executed from here**. There is no end-to-end run behind this change and the PR body must say so.

**Asserted mechanically, in CI:**

- the protocol carries the per-engine mechanism table, the fresh-context spawn contract, the two named escape-hatch closures, the `review_mode: "none"` invariant, the blocked/failed unavailability path with its prescribed `crew msg` body, and the widened metrics template;
- all three enum sites agree: the narrowed `:137` gloss is present, and the old gloss — the literal text `` `none` (trivial / no reviewer) `` as it appears at `:137` today — is **absent**;
- the stale carve-out sentence is **gone** from the protocol, and `review_mode: "none"` is gone from `adapters/cursor/rules/dispatcher.mdc`;
- `review_unavailable` is in the closed retro vocabulary and its branch points back at it;
- both codex and cursor launch prompts carry review-subagent authority (`tests/dispatch.bats`);
- **end-of-pipe, not just prose:** a synthetic bus log with a `failed` status and `{"review_mode":"unavailable"}` metrics, run through `crew rate` (read-only — no forbidden file is edited), lands a ratings row carrying `review_mode:"unavailable"` and `outcome:"failed"`. `tests/crew.bats` already builds logs this way for the `replanned` projection, so the idiom exists. Note precisely what this pins: `crew rate` passes any `review_mode` string through today, so the test would pass before the change. It is a **contract pin** — it stops a later `crew rate` edit silently swallowing the new value — and it is the only mechanically reachable half of the unavailability criterion. A grep over markdown is the other half;
- generated adapter trees match the canonical protocol byte-for-byte (existing gate).

**Resting on the text being followed, and unasserted:** that a codex or cursor worker actually spawns the reviewer, actually blocks when it cannot, and actually _chooses_ to emit `full`/`downgraded`/`unavailable` truthfully. No test can reach an engine this host refuses to launch. The first real signal is the `review_mode` distribution in `crew rate`'s store once a work-profile crew runs codex/cursor workers — the same evidence path the incident in "Problem" came from.

## Risks

- **Enum widening without a validator.** Nothing validates `review_mode` on write (`_bus_append` is off-limits, `crew rate` passes through), so a worker could emit garbage uncaught. Accepted: already true of every metrics field, and the alternative is editing two files the task forbids.
- **A blocked worker costs more than a degraded one.** Making unavailability terminal converts a silent bad PR into a stalled worker needing dispatcher attention. Intended — the issue argues the silent path is worse — and block→await plus the re-dispatch-as-trivial route keep the recovery cheap.
- **Prose growth in a file that is a prompt.** `WORKER_PROTOCOL.md` is context every worker pays for. Offset by folding the inline claude agent names into the mechanism table, so the section grows by about a table rather than a section.
- **A real discontinuity in `review_high` for trivial codex/cursor rows.** Historical rows carry `null`, new ones carry `0` (D3). State the consequence precisely rather than waving at it: the ratings design's consumer rule keys exclusion on `review_high == null`, so these rows move _out_ of the unmeasured set and into findings means as clean zeros. That is the same treatment trivial claude rows already get, and `engine` + `tier` remain in every row to disambiguate — so the trade is a one-time discontinuity in exchange for three engines finally sharing one convention. Accepted deliberately, not overlooked.
- **`tests/crew.bats` is a contended file.** `crew rate` landed in PR #62 and the `bus: concurrent oversized writes` fix is unmerged in PR #60, both touching it. The new test is **appended**, never interleaved into the existing `rate:` block, to keep the merge conflict surface to one hunk.
- **Two engines specified from documentation, not observation.** The codex/cursor rungs in D1 come from `dispatch.sh` and rule 1's ladder, not from a live pane. If a codex subagent spawn takes a different shape than the protocol describes, the first work-profile worker discovers it — and, by design, blocks loudly rather than degrading.
