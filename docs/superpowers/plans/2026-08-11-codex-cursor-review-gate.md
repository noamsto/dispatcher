# Independent code-review gate for codex and cursor workers — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `tier` mean the same thing on every engine for the post-implementation code-review gate. A `standard`/`deep` codex or cursor worker must run an independent, fresh-context review before pushing, and a reviewer it cannot spawn must surface as blocked/failed — never a silent `review_mode: "none"`.

**Architecture:** Prose-only in its load-bearing half. `WORKER_PROTOCOL.md`'s "Code review gate" gains a per-engine reviewer-mechanism table (roles stay engine-neutral; only the spawn mechanism differs), an explicit fresh-context spawn contract, and a loud unavailability path — `blocked`/`failed` + a fourth `review_mode` value `unavailable` + a new `review_unavailable` retro tag. The engine carve-out in the metrics section is split: critics stay claude-only (out of scope), the review gate does not. Four stale duplicates of that carve-out outside the protocol are corrected, one of which (`adapters/cursor/rules/dispatcher.mdc`) is hand-maintained and invisible to the drift gate. `dispatch.sh`'s `process_authority` clause broadens to cover review subagents by role. No `crew.sh` change: `crew rate` already passes `review_mode` through.

**Tech Stack:** Markdown (`adapters/core/protocols/WORKER_PROTOCOL.md`, `adapters/cursor/rules/dispatcher.mdc`, `README.md`), bash (`adapters/core/dispatch.sh`, `scripts/gen-adapters.sh`), bats.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-08-11-codex-cursor-review-gate-design.md` (revision 3, accepted). Every design decision below traces to a `D<n>` there; when this plan and the spec disagree, the spec wins.
- **Canonical source is `adapters/core/`.** Never hand-edit `adapters/claude-code/plugin/protocols/` or `adapters/codex/plugin/protocols/`. Edit `adapters/core/protocols/WORKER_PROTOCOL.md`, then run `scripts/gen-adapters.sh`. The test `every canonical protocol exactly matches both shipped protocol trees` and CI's separate drift gate both fail otherwise.
- **`adapters/cursor/rules/dispatcher.mdc` is the exception — it is NOT generated.** `gen-adapters.sh` clears only `adapters/claude-code/plugin/commands`, `adapters/cursor/commands`, `adapters/codex/plugin/skills`, and the two plugin `protocols/` trees. Edit the `.mdc` by hand; nothing else will.
- **`adapters/` is excluded from formatters** (prettier/shfmt) on purpose — these are model-facing prompts and reformatting has corrupted nested fences before. Do not reformat surrounding prose. Match the file's voice: dense, WHY-forward, key terms bolded.
- **Forbidden files, no exceptions:** `adapters/core/protocols/DISPATCHER_PROTOCOL.md` (#58 live), `adapters/core/crew.sh` (stall-watch region — #58; `_bus_append` — PR #60; `crew rate` — PR #62). `crew rate` may be **read** and exercised by a test, never edited.
- **Out of scope:** plan/spec critics for codex/cursor. `plan_critic_first_pass` stays `null` on both engines. Every edit preserves that half of the carve-out.
- **Test idiom for protocol prose:** `tests/adapters.bats` asserts exact strings via `run grep -F "$statement" "$protocol"`. Every asserted string must match the committed prose **byte for byte** — copy it out of the file, never retype it. Negative assertions use `[ "$status" -ne 0 ]`.
- **Known-failing tests that are not yours** — **three**, all in `tests/crew.bats` (do not fix, do not let them block): the two `reap:` release tests (fixed by unmerged PR #51) and `bus: concurrent oversized writes` (fixed by unmerged PR #60). Expect three failures there, not two.
- **Full suite green at every commit otherwise:** `bats tests/`.
- **Commit protocol (wave 1 runs three subagents against one git index).** One commit per task, staging **only** that task's listed files by path. Never `git commit -a` and never `git add -A` — a sibling task's half-finished work is in the same worktree, and `.git/index.lock` contention is real.
- **Scope fence.** `adapters/core/protocols/dispatch-orchestration.md` also carries a metrics template, and it omits `review_mode` entirely. It contains no carve-out and no enum, so it is **not** stale w.r.t. this change and is **out of scope** — do not "fix" it while hunting duplicates.

## Task order and concurrency

Three waves. Wave 1's three tasks touch disjoint files and run concurrently (cap 3). Wave 2 waits only because it appends to `tests/adapters.bats`, which Task 1 owns until it lands.

```
wave 1:  Task 1 (protocol + its tests)  ∥  Task 2 (dispatch.sh)  ∥  Task 3 (crew.bats pin)
wave 2:  Task 4 (the four stale duplicates + .mdc test)
wave 3:  Task 5 (regenerate + full suite)
```

---

### Task 1: The review gate binds to every engine (implement: opus)

**Tagged `implement: opus` deliberately.** This is not fiddly prose — `WORKER_PROTOCOL.md` is the system prompt of every worker on every engine, the tests grep it byte-for-byte, and the defect being fixed _was itself a sentence_. Wrong wording here misdirects every future worker, which is the wide-blast-radius bar rule 1 names. No other step qualifies.

**Files:**

- Modify: `adapters/core/protocols/WORKER_PROTOCOL.md`
- Modify: `tests/adapters.bats`

**Steps:**

- [ ] **1a — Per-engine reviewer mechanism (D1).** In "Code review gate (standard/deep)", keep the four role bullets engine-neutral and add a mechanism table directly under the "Dispatch the review as a single parallel batch" bullet:
  - claude → Agent tool, the named `*-reviewer` matching the diff's language, `find-bugs` in a general agent when none fits (**unchanged behaviour** — fold today's inline agent names into the table rather than restating them in both places, so the section grows by a table, not a section);
  - codex → native subagent (`agents.enabled`, cap 3) with the role brief written into its prompt, since codex has no named-agent registry; model = the tier's execute rung (deep → terra, standard → luna), effort = whatever `dispatch` pinned (no per-spawn override exists);
  - cursor → Task-tool subagent with an explicit model slug = the tier's execute rung (deep → `cursor-grok-4.5-medium-fast`, standard → `cursor-grok-4.5-low-fast`), same inline brief.
    Rungs must match rule 1's ladder exactly — read it, don't recall it.
- [ ] **1b — Fresh-context spawn contract (D2).** State that a reviewer subagent receives **only** the task doc, the diff (or the command to compute it), and its role brief — never the plan rationale, the spec, the implementer's notes, or an execute-stage transcript. State that it carries **review authority only**: it does not fix, commit, push, open PRs, or act as the worker. Then close the hatch by name: _"review it yourself in this context" is not a permitted fallback on `standard`/`deep`._
- [ ] **1c — Unavailability is terminal and loud (D3).** When no fresh-context reviewer can be spawned **at all** (delegation disabled, spawn refused, subagent dies before reporting): retry **once**, then `crew status "$CREW_WORKER_ID" blocked "review gate unavailable: <what>"` → `crew msg` → `crew await` on the existing block→await path. Do not push, do not open a PR, do not emit `none`. Prescribe the `crew msg` body the way the plan-shaped-gate-rework bullet already does: engine and tier, mechanism attempted, how it failed including the retry, and the two legal replies — **retry** or **re-dispatch** (to an engine that can review, or as `tier: trivial`). State that proceeding unreviewed at this tier is not a legal reply. Also state that `unavailable` never co-occurs with `done` — it appears on a `blocked`/`failed` snapshot only.
  - **Copy the `crew status` invocation shape from an existing call site, do not compose it.** `crew`'s signature is `status <from> <state> [detail] [pr_url]` and it validates the second positional against `working|blocked|pr_open|done|failed|exited`. Every call in this protocol reads `crew status "$CREW_WORKER_ID" blocked "<why>"`. Dropping the id makes `from=blocked`, crew exits 1, and the unavailability path never reaches the bus — the one path this task exists to build.
  - **Scope it against the two existing non-blocking carve-outs.** The codex-diverse reviewer bullet and the orchestration consult both cite the review gate as precedent for _not_ blocking on an unavailable reviewer ("never stall the gate on a missing diverse engine"). Those stay true: a missing **diverse** reviewer is still a should. What is terminal is having **no fresh-context reviewer at all**. Word 1c so the section cannot be read both ways.
- [ ] **1d — The enum's third site (D3).** At the "Record which mode ran as `review_mode`" sentence, add `unavailable` and narrow the third value's gloss from `` `none` (trivial / no reviewer) `` to trivial-only. **This parenthetical is the actual escape hatch** — "no reviewer → `none`" reads as permission to a worker whose reviewer failed. Also state the invariant: on `standard`/`deep`, a `kind: implement` worker never validly emits `review_mode: "none"`, on any engine.
- [ ] **1e — Close the timeout hatch (D2.2).** The `trivial`/`standard` safe-default-on-timeout bullet under "Report to the bus" lets a blocked worker pick a default and continue — which a standard codex worker could use to skip review and reach `done`. Carve it out: a missing review gate is never low-risk; it follows block→await→`failed` on every tier.
- [ ] **1f — Retro tag (D3).** Add `review_unavailable` to the closed vocabulary table (detail: engine, mechanism attempted, how it failed), and add the branch pointer in 1c's text, matching how every other tag is bound to its branch.
- [ ] **1g — Metrics section (D3, D4).** Widen the template literal to `"review_mode":"<full|downgraded|none|unavailable>"`.
  - **Widen the inline gloss in the same paragraph too — it is a separate sentence and easy to miss.** The prose definition reads ``review_mode` = which review depth actually ran (`full`|`downgraded`|`none`, per the Code review gate's repo-aware scaling)`; the fourth value joins that list. The carve-out sentence later in the same bullet is the _other_ half of this site — both move, or the protocol defines a three-value enum one line below a template offering four.
  - **Leave the `"review_high":<int>` placeholder alone.** An `unavailable` snapshot emits `null`, but the bullet's existing "Emit real `null` (not the string `"null"`) for fields a tier or engine never produces" clause already covers that. Editing the placeholder to `<int|null>` breaks the byte-exact assertion at `tests/adapters.bats:245` a second way.

  Then rewrite the closing carve-out sentence, **scoped by tier** — an unscoped version would tell a `trivial` codex worker to emit `full`/`downgraded` for a gate whose own section heading exempts it, reproducing the self-contradiction this change removes:
  - critics stay claude-only, so `plan_critic_first_pass` stays `null` on codex/cursor at every tier;
  - on **`standard`/`deep`**, codex and cursor run the review gate like any other engine and emit a real `review_high` integer with a `full`/`downgraded` `review_mode`;
  - on **`trivial`**, codex and cursor keep `review_mode: "none"` and now emit `review_high: 0` — the parity move: today's carve-out is engine-conditioned, so they emit `null` at every tier, while trivial claude already emits `0`. Leave claude's trivial `0` gloss itself untouched;
  - on an **`unavailable`** snapshot, `review_high` is `null` — `0` there would read as a clean run that never looked.

- [ ] **1h — Tests.** Add to `tests/adapters.bats`:
  - the per-engine mechanism strings (all three rows);
  - the fresh-context spawn contract, and **both** named escape-hatch closures — the self-review fallback (1b) **and** the timeout carve-out (1e). Two closures, two assertions: 1e is the single line that would otherwise let a standard codex worker default its way to `done`, and a prose-only edit with no grep behind it is exactly what a later reword silently reopens;
  - the `review_mode: "none"` invariant, and the "`unavailable` never co-occurs with `done`" clause;
  - the unavailability path including the literal `crew status "$CREW_WORKER_ID" blocked` form (assert the **shape**, not just that the path exists) and the prescribed `crew msg` contents;
  - the narrowed `:137` gloss present **and** the old gloss (`` `none` (trivial / no reviewer) ``) absent;
  - the widened template — update the existing assertion at line 245 in place, do not duplicate it — **and** a byte-exact positive assertion on the widened inline gloss, which line 245 does not cover;
  - a positive assertion on the new tier-scoped metrics sentence, so a future reword that drops it fails loudly instead of silently;
  - `review_unavailable` — extend the two existing vocabulary/pointer test lists rather than adding parallel tests;
  - a **negative** assertion that the old engine carve-out sentence is gone. Target a phrase unique to the _deleted_ text (e.g. `nor the claude code-review gate`), not a fragment like `Codex and cursor` that the new tier-scoped sentence legitimately contains — otherwise the negative assertion fails for the wrong reason;
  - one `grep -F` on `Cap the review→fix loop at 2.` — the "unresolved findings appear under `## Review notes`" acceptance criterion is satisfied by construction (the roles stay engine-neutral, so codex/cursor inherit it), but it is the one criterion with no mechanical anchor, and this gives it one at zero cost.

**Verification:** `bats tests/adapters.bats` green. Note this suite's first test runs `scripts/gen-adapters.sh` against the live tree, so running it already refreshes `adapters/{claude-code,codex}/plugin/protocols/` — which is why the byte-for-byte drift test passes here. **Leave those regenerated files for Task 5 to commit**; they are not in this task's Files list. Then re-read the whole "Code review gate" section top to bottom as a worker would: a codex worker must be able to answer "what do I spawn, with what, and what do I do when it won't spawn" without inference.

---

### Task 2: The launch prompt grants reviewers review authority (D5)

**Files:**

- Modify: `adapters/core/dispatch.sh` (the `process_authority` string only — one string, feeding both the codex and cursor launches)
- Modify: `tests/dispatch.bats`

**Steps:**

- [ ] Broaden the clause **by role, not by blanket**: execute subagents keep implementation authority only; review subagents get **review authority only**. Extending "implementation authority" to a reviewer would tell it to fix the code it was asked to judge. Keep the existing "not to re-derive worker process via skills, not to open PRs, not to act as the worker" clamp applying to both.
- [ ] **No apostrophes, no backticks, no `$` in the new wording.** Two hazards stack here. `$process_authority` is assigned as a _double_-quoted bash string, so a backtick or a bare `$word` expands at assignment time — a stray `$reviewer` silently becomes empty, and shellcheck flags backticks but not the expansion. It is then interpolated into a single-quoted `tmux send-keys … '…'` argument on both non-claude launch lines, where one apostrophe ("the reviewer's role", "don't fix") terminates the quote and corrupts every codex and cursor launch. `tests/dispatch.bats` only substring-matches the logged argv, so no test catches either. Match the existing clause's plain voice.
- [ ] Leave the codex-`ultra` clause untouched — it is scoped to execute-subagent orchestration and does not collide with a review batch.
- [ ] Extend the two existing assertions (`codex launch pins agents.* guardrails and process authority`, `cursor launch includes process authority`) to require the review-authority substring in each engine's launch line.

**Verification:** `bats tests/dispatch.bats` green; `shellcheck adapters/core/dispatch.sh` clean.

---

### Task 3: Pin the `review_mode` pass-through end-to-end

**Files:**

- Modify: `tests/crew.bats` (**append at end of file** — there are several `rate:` test blocks, so "after the rate tests" is ambiguous; end-of-file also keeps the conflict surface against PRs #60 and #62 to a single hunk)

**Steps:**

- [ ] Following the idiom of `rate: projects replanned booleans and legacy null while retaining rework count`, build a synthetic `events.jsonl` with a `dispatch` row (engine `codex`, tier `standard`), a `failed` status, and a metrics `msg` carrying `{"review_mode":"unavailable","review_high":null}`. Run `crew rate` and assert the store row carries `review_mode:"unavailable"`, `outcome:"failed"`, and `reached_pr:false`.
- [ ] **Say what this pins, in the test's comment.** `crew rate` passes any `review_mode` string through today, so this test would pass before the change. It is a contract pin — it stops a later `crew rate` edit silently swallowing the new value — and it is the only mechanically reachable half of the "with a test" acceptance criterion. `crew.sh` is **not** edited.

**Verification:** `bats tests/crew.bats` green apart from the **three** known-failing tests named in Global Constraints — the two `reap:` release tests and `bus: concurrent oversized writes`. Three failures is the expected baseline in this file; a fourth is yours.

---

### Task 4: The four stale carve-out duplicates (D4, D6)

**Files:**

- Modify: `adapters/cursor/rules/dispatcher.mdc` (hand-maintained — see Global Constraints)
- Modify: `scripts/gen-adapters.sh` (the comment above the `spec-plan-critic` copy)
- Modify: `README.md`
- Modify: `tests/adapters.bats` (append only — Task 1 owns the rest of this file)

**Steps:**

- [ ] **`.mdc` (highest priority of the four).** It has `alwaysApply: true`, so it is in every cursor session's context, and it currently instructs the exact emission the acceptance criteria forbid. Split its bullet: cursor workers still skip the plan-critic (claude-only), but **do** run the code-review gate per `WORKER_PROTOCOL.md`; drop `review_high: null` and `review_mode: "none"` from it.
- [ ] **`gen-adapters.sh`.** The comment claims codex workers "stay ungated (no plan-critic / code-review)". Narrow it to the critic half; the codex adapter still ships no agents/workflows, which is why critics stay claude-only — the review gate now runs on native subagents instead.
- [ ] **`README.md`, four spots:** split the `Worker: critics + review gate` row into `Worker: spec/plan critics` (✅ / ❌ / ❌) and `Worker: code-review gate` (✅ / ✅ / ✅); change the codex `Subagents` cell to `⚠️ native only³` and widen footnote ³ to cover both engines (codex has native ad-hoc subagents but no _declarable_ plugin agents; cursor has both but not this pipeline's model) — **footnote ² belongs to the Slash commands row, do not touch it**; rewrite the "Process-light is a promise" paragraph so it promises the critic gap only; narrow the roadmap bullet to the critic pipeline.
- [ ] **Test.** Append a `tests/adapters.bats` test asserting the `.mdc`'s new text **and** the absence of `review_mode: "none"` in it. This is the file's only protection — the generator never rewrites it and no drift gate sees it.

**Verification:** `bats tests/adapters.bats` green; `shellcheck scripts/gen-adapters.sh` clean; `rg 'review_mode: "none"' adapters/cursor/` returns nothing.

**Expect prettier to rewrite `README.md` on commit.** The pre-commit hook covers it (only `^adapters/` and `*.bats` are excluded), so editing the capability table will trigger a table realignment: the commit aborts, the file is rewritten, re-stage and commit again. That is the hook working, not a failure.

---

### Task 5: Regenerate and verify

**Files:** generated trees only.

**Steps:**

- [ ] Run `scripts/gen-adapters.sh`.
- [ ] Run the full `bats tests/` suite. Everything green except the three known-failing tests named in Global Constraints — confirm those three are the _only_ failures and that each fails for its documented pre-existing reason, not because of this change.
- [ ] Confirm `git status` shows the regenerated protocol copies updated in both plugin trees, and that no generated file was hand-edited. Commit the regenerated trees here (Task 1 deliberately left them).
- [ ] **Record the verification boundary for the PR body.** Both `WORKER_TASK.md` and the spec require the PR to state plainly which behaviour is asserted by tests and which rests on the protocol text being followed. Write down the two lists as they actually ended up — not as planned — so the PR body is not reconstructed from memory. Both non-claude engines are work-profile only and cannot be launched from this host: there is **no** end-to-end run behind this change, and the PR must say so rather than imply otherwise.

**Verification:** `bats tests/` — failures limited to the three known ones; `git diff --stat` matches the spec's artifact table with no surprises.
