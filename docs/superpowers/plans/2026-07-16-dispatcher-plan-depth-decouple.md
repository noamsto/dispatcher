# Decouple plan-depth from tier — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `plan: provided|required` axis so a dispatcher that already wrote a complete plan into `WORKER_TASK.md` stops the worker from re-deriving it, and make the worker's process authority override the superpowers always-invoke mandate.

**Architecture:** Six edits across one shell script (`dispatch.sh`) and four prompt artifacts (`DISPATCHER_PROTOCOL.md`, `WORKER_PROTOCOL.md`, `spec-plan-critic/SKILL.md`, `agents/plan-critic.md`), plus a live smoke + rebuild. The dispatcher stamps `plan:` and varies the launch prompt; the worker reads `plan:` and skips/keeps its plan phase; the plan-critic gains a blocking rubric so nitpick `revise`s don't cost a second round.

**Tech Stack:** bash (writeShellApplication-wrapped), Markdown protocol/skill/agent artifacts, Nix Home Manager (`nh home switch`), `shellcheck`, `shfmt`.

## Global Constraints

- All files live under `home/ai/claude-code/` in the nix-config repo. Edit in the worktree at `~/nix-config-worktrees/feat-90-decouple-plan-depth-from-tier`.
- `dispatch.sh` is the **function body only** — no shebang, no `set -euo pipefail` (writeShellApplication prepends them). Keep it that way.
- `dispatch.sh` must stay `shellcheck` clean **and** `shfmt -i 2` clean (the repo's `nix flake check` runs `shfmt`; a pre-existing offender here is exactly what this plan must not add to).
- Prompt artifacts (`*.md`) are read by **absolute path** and take effect on merge to the main checkout — they need no rebuild. `dispatch.sh` is a PATH binary via `writeShellApplication`, so it needs `nh home switch` to deploy.
- `--plan` defaults to `required` — omitting it must preserve today's behavior exactly (worker plans as before).
- Do not restructure files or refactor beyond the edits named. No drive-by reformatting of the `.md` files.

---

### Task 1: `dispatch.sh` — `--plan` flag, stamp field, conditional launch prompt

**Files:**

- Modify: `home/ai/claude-code/dispatch.sh`

**Interfaces:**

- Produces: a `plan: <provided|required>` line in `WORKER_TASK.md` (consumed by Task 4's worker logic); a `--plan` CLI flag (documented in Task 2); a launch-prompt suffix when `plan=provided`.

- [ ] **Step 1: Add `--plan` to the usage string**

In `usage()` (line 10), insert `[--plan provided|required]` after `[--mcp <profile>]`:

```bash
  echo "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh> [--agent claude|codex] [--mcp <profile>] [--plan provided|required] [--crew-id <id>] [LINEAR-ID|#N] <title...>" >&2
```

- [ ] **Step 2: Add the `plan_val` default**

After the line `crew_id_flag=""` (line 36), add:

```bash
plan_val="required"
```

- [ ] **Step 3: Parse `--plan` in the arg loop**

In the `while [ $# -gt 0 ]` loop, add this case immediately **after** the `--crew-id) ... ;;` block (before the final `*)` catch-all):

```bash
  --plan)
    plan_val="${2:-}"
    case "$plan_val" in
    provided | required) ;;
    *)
      echo "dispatch: --plan must be provided or required" >&2
      exit 1
      ;;
    esac
    shift 2
    ;;
```

- [ ] **Step 4: Stamp `plan:` into WORKER_TASK.md**

Replace the stamp `printf` (lines 194-195) — add `plan: %s\n` right after `effort:` and thread `$plan_val` as the third arg:

```bash
  printf 'tier: %s\neffort: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\n' \
    "$tier" "$effort" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name"
```

- [ ] **Step 5: Build the conditional launch-prompt suffix**

Immediately **before** the `if [ "$agent" = codex ]; then` block (line 219), add:

```bash
# When the dispatcher already wrote the plan into the task doc, say so in the
# launch prompt. A launch-prompt (user-turn) instruction is a "direct request",
# which satisfies using-superpowers' own escape hatch — so the worker skips the
# plan phase instead of re-deriving it.
plan_note=""
if [ "$plan_val" = provided ]; then
  plan_note=" The task doc is your plan of record — extract the steps and implement; do not re-plan or re-critique it."
fi
```

- [ ] **Step 6: Append the suffix to both launch prompts**

In the `codex` branch, append `${plan_note}` inside the single-quoted prompt, before its closing `'`:

```bash
    "codex --profile worker -m $model -c model_reasoning_effort=$effort --dangerously-bypass-approvals-and-sandbox 'Read ~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
```

In the `claude` branch, likewise:

```bash
    "claude --name $agent_name --model $model --effort $effort $mcp_flag $xreview_mcp --append-system-prompt-file ~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
```

- [ ] **Step 7: Format and lint**

Run: `shfmt -i 2 -w home/ai/claude-code/dispatch.sh`
Run: `shellcheck home/ai/claude-code/dispatch.sh`
Expected: `shfmt` makes no further changes on a second run; `shellcheck` exits 0 with no output.

- [ ] **Step 8: Syntax check**

Run: `bash -n home/ai/claude-code/dispatch.sh`
Expected: exit 0, no output (the file is a body fragment, but `bash -n` still parses it).

- [ ] **Step 9: Commit**

```bash
git add home/ai/claude-code/dispatch.sh
git commit -m "feat(dispatcher): --plan provided|required flag, stamp + launch-prompt (#90)"
```

---

### Task 2: `DISPATCHER_PROTOCOL.md` — document the `plan:` judgement

**Files:**

- Modify: `home/ai/claude-code/DISPATCHER_PROTOCOL.md`

**Interfaces:**

- Consumes: the `--plan` flag from Task 1.

- [ ] **Step 1: Add the plan-depth decoupling paragraph**

Immediately **after** the "Tier and model control different things" paragraph (line 17), insert a new paragraph:

```markdown
**Plan-depth is a fourth lever — decouple it from tier.** Tier sets _review_ rigor; whether the worker runs a _pre-implementation plan phase_ is a separate judgement. When you inline a spec (`DISPATCH_SPEC`) that already contains **all four** of — root cause/mechanism, an explicit file list, a named approach, and acceptance criteria — you have already done the plan phase yourself; pass `--plan provided` so the worker treats the doc as its plan of record and skips `spec-plan-critic`. If the task still needs design work the doc doesn't settle, pass `--plan required` (the default). This is independent of tier: a `standard` + `--plan provided` task still gets a full standard _review_; it just isn't re-planned. Do **not** drop to `trivial` to skip planning — `trivial` also drops the review gate. `--plan` is judged like `--effort`: by the doc you wrote, not by the tier.
```

- [ ] **Step 2: Add `--plan` to the dispatch usage block**

In the fenced `dispatch` usage (line 40), insert `[--plan provided|required]` after `[--mcp <profile>]`:

```
dispatch <tier> <model> --effort <low|medium|high|xhigh> [--agent claude|codex] [--mcp <profile>] [--plan provided|required] [LINEAR-ID] <title…>
```

- [ ] **Step 3: Mention `plan` in the stamps description**

In the paragraph at line 43, change `stamps `WORKER_TASK.md` (tier, crew_id, dispatcher_pane, closes line, task body)` to include `plan`:

```markdown
`dispatch` is the dumb mechanism — it creates the worktree and tmux window, stamps `WORKER_TASK.md` (tier, plan, crew_id, dispatcher_pane, closes line, task body), and launches the worker with `WORKER_PROTOCOL.md` baked. You supply the tier + model + effort you judged.
```

- [ ] **Step 4: Verify consistency**

Run: `grep -n "\-\-plan\|Plan-depth\|plan," home/ai/claude-code/DISPATCHER_PROTOCOL.md`
Expected: the new paragraph, the usage flag, and the stamps mention all appear; no contradiction with the existing tier text (tier still owns review, plan owns the pre-impl phase).

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/DISPATCHER_PROTOCOL.md
git commit -m "docs(dispatcher): document plan-depth decoupling + --plan (#90)"
```

---

### Task 3: `WORKER_PROTOCOL.md` — process-authority category rule

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md`

**Interfaces:**

- Produces: the "Process authority" section that Tasks 4-6 rely on (it authorizes skipping process skills).

- [ ] **Step 1: Insert the Process authority section**

Immediately **after** the intro paragraph (line 3) and **before** `## First action`, insert:

```markdown
## Process authority

This protocol governs your process end-to-end and is your **human partner's explicit instruction**. It **supersedes `superpowers:using-superpowers`** for process/lifecycle skills: the harness pipeline (`spec-plan-critic` + the code-review gate + the gates below) **is** your process — do **not** separately invoke `brainstorming`, `writing-plans`, `executing-plans`, `requesting-code-review`, or `test-driven-development` as independent process steps, and do not treat "a skill exists, so I must run it" as binding here. Implementation and domain skills (`systematic-debugging`, `charm-tui`, the language `*-reviewer`s, `frontend-design`, …) remain fully available — use them freely.
```

- [ ] **Step 2: Verify**

Run: `grep -n "Process authority\|supersedes" home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: the section is present before `## First action`.

- [ ] **Step 3: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): process authority overrides using-superpowers (#90)"
```

---

### Task 4: `WORKER_PROTOCOL.md` — plan-of-record skip, fallback, re-entry, skip audit

**Files:**

- Modify: `home/ai/claude-code/WORKER_PROTOCOL.md`

**Interfaces:**

- Consumes: `plan:` stamp (Task 1), Process authority section (Task 3).

- [ ] **Step 1: Point the standard pipeline bullet at the plan-of-record gate**

In `## Pipeline by tier`, change the `standard` bullet (line 12) opening from `run the `spec-plan-critic` workflow with …` to gate on the plan of record:

```markdown
- **standard** — consult **Plan of record** (below) first; unless the plan already exists, run the `spec-plan-critic` workflow with `{ tier: 'standard', ... }` (plan + plan-critic only). Then execute the plan (of record, or returned by the workflow) via subagents, then the **fast deterministic gate** (build+vet+lint+unit on changed packages, looped to green), then the code-review gate (one pass), then `/deslop` + push + PR.
```

- [ ] **Step 2: Add the Plan of record section**

Immediately **after** the `## Pipeline by tier` list (after line 13) and **before** `## Orchestration consult`, insert:

```markdown
## Plan of record (does the plan already exist?)

Before running any plan phase, read `plan:` from `WORKER_TASK.md`:

- **`plan: provided`** — the dispatcher wrote the plan into the task doc (root cause/mechanism, explicit file list, named approach, acceptance criteria). The doc **is** your plan of record. Do **not** run `spec-plan-critic`. Extract the doc into a bite-sized step list and go straight to execute → fast gate → review. (Your launch prompt says the same thing; per **Process authority** this is not a skill you may override into re-planning.)
- **`plan: required`** — run the tier's plan phase as described above.
- **Field absent (legacy / hand-authored doc)** — self-assess by _extraction_, not judgement: can you (i) quote the exact file list, (ii) state the mechanism in one sentence quoting the doc, and (iii) enumerate the acceptance criteria as checkboxes? If **all three** succeed, that extraction **is** your plan of record — proceed as `provided`. If **any** fails, treat it as `required` and run the plan phase.
- **Re-entry (both skip paths).** If, at execute time, the plan of record is contradicted by the repo (a named file doesn't exist, the approach doesn't fit the code), stop improvising: run `spec-plan-critic` once, normally, from what you now know. This is a single explicit fallback (mirrors the deep false-negative recovery), not a loop.
- **Scope.** The skip applies to the **plan** phase only. A **deep** worker may skip the plan-critic under these rules but **never** its spec-critic / Fable consult — deep is chosen when the _framing_ needs adversarial pressure, which a task doc doesn't settle.
- **Retain the checkpoint-peek** after you produce the extracted plan of record, same as after a normal plan.
```

- [ ] **Step 3: Add the approach-sanity requirement to the review gate**

In `## Code review gate`, under the "Language reviewer" bullet (line 46), append a sentence:

```markdown
- **Language reviewer** — the agent matching the changed files' language (`go-reviewer`, `python-reviewer`, `typescript-reviewer`, `shell-reviewer`, the SQL reviewers, …). If none fits, a general agent running the `find-bugs` skill. **If the plan phase was skipped** (plan of record), instruct this reviewer to add an explicit **approach-sanity** check against the task doc — is this the _right_ fix, not merely a faithful one? — since no plan-critic vetted the approach.
```

- [ ] **Step 4: Add the skip-audit stamp to the PR-body rule**

In `## Rules`, rule 5 (line 80), append the skip stamp requirement:

```markdown
5. **PR body must include the closes line from your task file** (`Closes #<N>` for a GitHub issue, or `Closes ENG-<N>` for a Linear ticket — copy it verbatim), any escalations, and any unresolved review notes. If you **skipped the plan phase** (plan of record), add a `## Plan` heading with the line `Plan: task doc (provided)` so the skip is auditable.
```

- [ ] **Step 5: Verify consistency**

Run: `grep -n "Plan of record\|plan: provided\|approach-sanity\|Plan: task doc" home/ai/claude-code/WORKER_PROTOCOL.md`
Expected: standard bullet references Plan of record; the section defines provided/required/absent/re-entry/scope; the review gate and rule 5 additions are present. Confirm the `deep` bullet (line 13) is unchanged except that it still routes its plan phase through Orchestration consult (deep never skips spec-critic).

- [ ] **Step 6: Commit**

```bash
git add home/ai/claude-code/WORKER_PROTOCOL.md
git commit -m "feat(worker): plan-of-record skip, extraction fallback, re-entry, skip audit (#90)"
```

---

### Task 5: `spec-plan-critic/SKILL.md` — blocking-only revisions + tighter opus bar

**Files:**

- Modify: `home/ai/claude-code/skills/spec-plan-critic/SKILL.md`

**Interfaces:**

- Consumes: the plan-critic blocking rubric (Task 6) — this task references it; keep the wording aligned with Task 6's definition.

- [ ] **Step 1: Tighten the `implement: opus` bar**

In the Standard tier section, the "Per-step implement-model tag" bullet (line 20), replace the parenthetical describing when to tag with an explicit include/exclude:

```markdown
- **Per-step implement-model tag.** The worker executes plan steps on **sonnet by default**. A genuinely high-risk step may carry an inline `implement: opus` tag so the worker spawns _that_ step's subagent on opus. The bar is narrow — **only** subtle concurrency, security-sensitive logic, or a wide-blast-radius refactor qualifies. Do **not** tag pure CSS/layout/styling, straightforward refactors, or test-only steps — fiddly is not high-risk. No tag = sonnet. Tell the drafting agent to tag **sparingly**; most steps stay untagged. Example step line: `- [ ] **Step 3: rework the token-refresh lock** (implement: opus)`.
```

- [ ] **Step 2: Gate the revision loop on blocking findings**

In the Standard tier section, step 3 ("If `revise`", line 25), replace it with a blocking-gated version:

```markdown
3. **If `revise` with ≥1 `blocking` finding** — spawn an agent to revise the plan incorporating **only the blocking** findings. Then re-run step 2. Cap at **2 revisions total**. A `revise`/`accept` verdict that carries only **non-blocking `notes[]`** does **not** trigger a revision round: apply those notes at the implementer's discretion during execution and proceed. (The `plan-critic` guarantees `revise` ⇒ ≥1 blocking finding — see that agent.)
```

- [ ] **Step 3: Note the plan-of-record short-circuit**

At the top of the `## Standard tier (plan only)` section (after line 15's heading), add one line so the skill states its own precondition:

```markdown
The worker invokes this skill only when a plan is **required** — it gates on the "Plan of record" check in `WORKER_PROTOCOL.md` first, so a pre-specified task never reaches here.
```

- [ ] **Step 4: Verify**

Run: `grep -n "implement: opus\|blocking\|Plan of record\|non-blocking" home/ai/claude-code/skills/spec-plan-critic/SKILL.md`
Expected: opus bar lists the non-qualifiers; step 3 is blocking-gated; the precondition line is present.

- [ ] **Step 5: Commit**

```bash
git add home/ai/claude-code/skills/spec-plan-critic/SKILL.md
git commit -m "feat(spec-plan-critic): blocking-only revisions + narrow implement:opus bar (#90)"
```

---

### Task 6: `agents/plan-critic.md` — define blocking + accept-with-notes

**Files:**

- Modify: `home/ai/claude-code/agents/plan-critic.md`

**Interfaces:**

- Produces: the `blocking`/`notes[]` semantics that Task 5's step-3 gate relies on. Keep names identical: `verdict` (`accept`/`revise`/`reject`), `blocking[]`, `notes[]`.

- [ ] **Step 1: Add the blocking rubric to Discipline**

In `## Discipline` (after line 23, "Clean plan after a genuine read → `accept`."), add:

```markdown
- **`revise` REQUIRES ≥1 `blocking` finding.** Blocking = the plan, as written, would **fail an acceptance criterion, break the build, or touch files outside the task's scope**. Sequencing/​missed-call-site/​placeholder/​type-drift/​decomposition-conformance findings are blocking. Anything else — style, polish, "could be clearer", optional hardening — is a **non-blocking note**: return `verdict: accept` with `notes[]`, never `revise`. Do **not** emit `revise` with an empty `blocking[]`, and do not inflate a note to blocking to force a round.
```

- [ ] **Step 2: Document the output shape**

Replace the `## Output` body (lines 25-26) so the fields are explicit:

```markdown
## Output

Return ONLY the structured verdict object: `verdict` (`accept` | `revise` | `reject`), `blocking[]` (each `issue` / `why` / `fix`), and `notes[]` (non-blocking suggestions, may be empty). `revise` ⇒ `blocking[]` non-empty. `accept` may still carry `notes[]` for the implementer to apply at discretion.
```

- [ ] **Step 3: Verify**

Run: `grep -n "blocking\|notes\|verdict" home/ai/claude-code/agents/plan-critic.md`
Expected: the rubric and the output shape both name `verdict`/`blocking[]`/`notes[]` consistently with Task 5.

- [ ] **Step 4: Commit**

```bash
git add home/ai/claude-code/agents/plan-critic.md
git commit -m "feat(plan-critic): blocking rubric — revise requires a real blocker (#90)"
```

---

### Task 7: Deploy, smoke-test, and gate

**Files:** none (verification only)

- [ ] **Step 1: Rebuild to place the new `dispatch.sh` on PATH**

Run: `nh home switch`
Expected: build succeeds; `dispatch` on PATH is the new binary. (Prompt `.md` artifacts are read by absolute path from the worktree/branch — but note they only resolve off the **main checkout** on merge; for the pre-merge smoke, the worker reads `~/nix-config/home/ai/claude-code/WORKER_PROTOCOL.md`, i.e. main. See Step 5.)

- [ ] **Step 2: Assert the stamp + prompt behave (no worker spawned)**

Verify the two pure-string behaviors without launching a real worker. Inspect a `provided` invocation's would-be output by reading the code path, or run a scoped check: create a scratch dir, and confirm `dispatch --plan bogus …` is rejected:

Run: `dispatch standard sonnet --effort high --plan bogus test 2>&1 | grep -q "must be provided or required"; echo $status`
Expected: `0` (fish) — the bad value is rejected before any scaffolding.

- [ ] **Step 3: `nix flake check` stays green for the shell change**

Run: `nix flake check 2>&1 | grep -iE "shfmt|shellcheck|dispatch" || echo "no shell complaints"`
Expected: no new `shfmt`/`shellcheck` complaint about `dispatch.sh`. (If the tree was already red for unrelated reasons, confirm `dispatch.sh` is not among the offenders.)

- [ ] **Step 4: Live smoke — provided path skips planning** _(manual gate; needs the branch merged to main first so the worker reads the updated `WORKER_PROTOCOL.md`, or point the worker at the worktree copy)_

Dispatch a fully-specified `standard` task with `--plan provided` (use a throwaway repo or a real bounded ticket). Watch the worker pane: it must go **straight to execute** (no `spec-plan-critic` / `plan-critic` agent), and its PR body must carry `Plan: task doc (provided)`.

- [ ] **Step 5: Live smoke — required path still plans; deep still specs** _(manual gate)_

Dispatch the same task with `--plan required` (or omit `--plan`): the worker runs the plan phase as before. Dispatch a genuinely underspecified `deep` task: it still runs its spec-critic even though `--plan` may be `provided` (deep never skips spec-critic — Task 4 scope rule).

- [ ] **Step 6: Push and open the PR**

```bash
git push -u origin feat/90-decouple-plan-depth-from-tier
gh pr create --assignee @me --title "feat(dispatcher): decouple plan-depth from tier (#90)" --body "Closes #90. See docs/superpowers/specs/2026-07-16-dispatcher-plan-depth-decouple-design.md. Fable-reviewed (REVISE → incorporated)."
```

---

## Notes for the executor

- Tasks 1-6 are independent edits to distinct files and can each be reviewed/rejected on their own; keep the commit-per-task discipline.
- The prompt-artifact tasks (2-6) have no runnable unit test — their verification is the `grep` consistency check plus the Task 7 live smoke. Do not fabricate a test harness for Markdown.
- After merge, update the `dispatch-tier-vs-spec-plan-critic` memory: the "drop to standard when the spec is thorough" guidance is now superseded by `--plan provided` (which keeps review rigor while skipping planning).
