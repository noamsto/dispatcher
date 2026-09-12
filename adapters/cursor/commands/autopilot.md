---
description: "Autonomous dev workflow: Linear ticket → implement → PR → CI green"
argument-hint: "[TICKET-ID]"
---

# Autopilot: Autonomous Development Workflow

You are running an autonomous development workflow from Linear ticket to merged-ready PR.

**Argument:** `$ARGUMENTS` (optional Linear ticket ID, e.g. `PL-344`)

---

## Step 1: Ticket Selection

**If a ticket ID was provided:** Fetch it from Linear.

**If no ticket ID:**
1. Fetch my assigned Linear issues with status "Todo"
2. Present them in a numbered list with title, priority, and labels
3. Ask which one to work on (or suggest the highest priority one)

Once the ticket is locked in, stamp it on the tmux status bar (skip silently if
the command isn't on PATH — e.g. outside a lazytmux tmux):

```bash
command -v claude-status-update >/dev/null && claude-status-update issue add <TICKET-ID>
```

## Step 2: Ticket Analysis

Read the ticket description and explore the relevant codebase areas.

### Size Assessment

**Small ticket** (single WT + PR):
- One clear deliverable
- Scoped to a single area of work
- Clear acceptance criteria

**Large ticket** (break into sub-tickets):
- Multiple distinct deliverables
- Ticket explicitly lists several items
- Work spans unrelated areas

### If Large: Break Down

1. Create sub-tickets on Linear as child issues of the parent
2. Each sub-ticket gets a clear title and scoped description
3. **PR strategy decision:**
   - **Independent PRs off main** (default): when sub-tasks don't touch the same files
   - **Parent branch**: when sub-tasks have file/dependency overlap — sub-PRs target the parent branch, final PR merges parent → main
4. Process each sub-ticket through Steps 3-9 below — `issue add` each sub-ticket as you start it, `issue done` when its PR is green
5. Present the breakdown plan before starting. Don't ask for approval — just announce what you're doing and proceed.

## Step 3: Plan

**Autonomous by default.** Read the ticket, explore the codebase, produce an implementation plan, and start.

**Pause and ask ONLY if:**
- The ticket is vague — no clear acceptance criteria, unclear scope, ambiguous requirements
- You genuinely don't understand what's being asked

Do NOT pause for:
- Large file counts
- Multiple services touched
- Complex but clear requirements

Keep the plan concise — a mental model, not a document. List the files to change and what to do in each.

For behavioral bugs, shared contracts, or PR feedback, read `EVIDENCE_REVIEW.md`
from `$DISPATCHER_PROTOCOL_DIR`, falling back to adapter-local `protocols/`
(inside the plugin on Claude/Codex, beside `commands/` on Cursor). Apply its
proof, review-risk, recurrence, and completion rules through Steps 3–10.

## Step 4: Implement

1. Create a worktree with [worktrunk](https://worktrunk.dev):
   ```bash
   WTPATH=$(wt switch --create <branch-name> --no-cd --format json -y | jq -r '.path')
   cd "$WTPATH"
   ```
   - Use the Linear branch name (copy from ticket with `Cmd+Shift+.`)
   - `wt switch` is idempotent: if the worktree already exists it just returns the path
   - lazytmux's post-switch hook short-circuits when `$CLAUDECODE` is set, so no spurious tmux window is spawned from inside Claude
2. Implement the plan
3. Commit incrementally as you go (small, logical commits)
4. Run relevant tests/checks as you work

## Step 5: Quality Pass

Run these skills on the branch diff:

1. **Invoke `/simplify`** — review for reuse, quality, efficiency
2. **Invoke `/deslop`** — remove AI-generated slop (unnecessary comments, defensive blocks, style inconsistencies)

Commit any fixes from these passes.

## Step 6: Code Review

Dispatch reviewer agents **in parallel** (single message, multiple Agent tool calls). Each reviewer sees the branch diff against the base.

### Reviewer roster

Reviewer bodies ship with the harness: `$DISPATCHER_REVIEWERS_DIR/*.md`, falling back to the adapter-local `reviewers/` when that variable is unset. Each carries `globs:` — the changed-file patterns that route a diff to it — and, where a pattern can't express the trigger, a `when:` line. Match your changed paths (`git diff --name-only main...HEAD`) against every roster `globs:`, honour each matched reviewer's `when:`, and that set is the batch. Nothing matched: one general reviewer running the `find-bugs` skill.

Spawn one Agent-tool subagent per matched roster entry, its body as the brief — prefer a native reviewer agent of the same name where the environment defines one.

### Conditional: `security-reviewer`

`security-reviewer` is the one roster entry with no `globs:`; its `when:` is the trigger. Include it only if the diff touches an auth, crypto, input-parsing, SQL, or network path. Conservative trigger: when in doubt, include it. Otherwise skip it.

### After reviewers return

1. Aggregate findings, deduplicate overlapping issues
2. Every roster body grades findings `CRITICAL` / `HIGH` / `MEDIUM` and ends with a `Block` / `Warning` / `Approve` verdict. Verify findings against the repo; confirmed correctness issues require a fix or an explicit unresolved disposition, regardless of severity.
3. Apply fixes, commit (`fix(review): address <reviewer> findings`)
4. Follow `EVIDENCE_REVIEW.md` for the fresh evidence packet, stronger cross-component reviewer, targeted re-review, and two-round fix cap. A blocked gate stops delivery; report the remaining evidence and decision needed.

## Step 7: Create PR

1. Push the branch: `git push -u origin <branch>`
2. Create the PR: `gh pr create --assignee @me --title "..." --body "..."`
   - Title: concise, under 70 chars
   - Body: summary bullets + test plan
   - Reference the Linear ticket (e.g., "Closes PL-344")

## Step 8: Watch Loop

Poll CI and PR comments until everything is clean. **Check every ~2 minutes.**

### Check CI

```bash
gh pr checks <PR-NUMBER> --repo factify-inc/mono
```

- **Ignore "Apps Sanity Gate"** — it's flaky, don't act on it
- For any other failing check: read the logs (`gh run view <run-id> --log-failed`), diagnose, fix, and run affected checks. Behavioral fixes also pass the evidence and targeted review gates before push.

### Check PR Comments

```bash
gh api repos/factify-inc/mono/pulls/<PR-NUMBER>/comments
gh api repos/factify-inc/mono/issues/<PR-NUMBER>/comments
```

- Follow `EVIDENCE_REVIEW.md` → PR feedback and completion: paginate all feedback,
  restore the finding ledger, verify current-head findings, batch fixes, test and
  re-review before replying with proof. Escalate recurring invariant failures
  before another patch. Product/architecture questions remain pending and go in
  the final report; they are not comments-clean.

### Loop Exit Conditions

Exit when ALL of these are true:
- All CI checks green (except Apps Sanity Gate), for the current head
- The `EVIDENCE_REVIEW.md` completion check passes for that same head

**Safety cap:** Stop after 30 iterations (~1 hour). Report status and ask for guidance.

## Step 9: Final Quality Pass

One last pass after all CI/reviewer fixes are done:

1. **Invoke `/simplify`**
2. **Invoke `/deslop`**

If this produces changes, rerun affected checks; behavioral edits also need the
targeted re-review in `EVIDENCE_REVIEW.md` before push. Refresh the current-head
completion check afterward. Exhausted review budgets remain exhausted.

## Step 10: Report

Unstamp the ticket — the work is handed off:

```bash
command -v claude-status-update >/dev/null && claude-status-update issue done <TICKET-ID>
```

Present a summary:

```
## Autopilot Complete

**Ticket:** [PL-XXX] Title
**PR:** <url>
**Status:** CI green, no open comments

### What was done
- <bullet summary of implementation>

### Judgment calls made
- <any ambiguous reviewer comments you addressed with best judgment>
- <any decisions you made autonomously>

### Unresolved
- <anything you couldn't handle, if any>
```

---

## Important Rules

- **Never merge PRs** — that's the user's call
- **Never skip tests** — if they fail, fix them
- **Commit messages should be clear and conventional** — match the repo's existing style
- **For large tickets with sub-PRs:** complete Step 10 once for the whole ticket, summarizing all sub-PRs
- **If stuck for >3 attempts on the same issue:** stop and ask for help instead of looping
- **Repo is `factify-inc/mono`** — always use this for gh commands
