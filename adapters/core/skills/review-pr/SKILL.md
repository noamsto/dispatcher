---
name: review-pr
description: reviewing an existing PR (--pr, --review)
---

# review-pr

- **Review attach.** To review an existing GitHub PR N, pass `--pr N` (not an issue number, not a title that would mint `feat/N-review-…`). `dispatch` resolves the PR's `headRefName`, `headRefOid` and `baseRefName`, attaches with `wt switch` (no `-c`), and checks the worktree's `HEAD` against `headRefOid`: a clean mismatch is fetched and hard-reset to the PR head; a dirty one aborts before any worker launches. The header stamps `pr: N` and `base: <baseRefName>` (no `Closes #N` from the PR number); the worker reads `base:` instead of assuming the default branch. `--pr` cannot combine with a Linear id or GitHub issue token.
- **Review mode.** `--review` (requires `--pr N`) makes a review-only worker: it stamps `kind: review`, appends `REVIEW_TASK.md` (the durable review contract) to the task doc, and drops the push/PR mandate from the launch prompt. Do not re-author that contract as per-worker prose; it already covers: no edit/commit/push/PR, the PR-head worktree, direct reviewer dispatch (never through a meta-agent), refuting every finding, one `COMMENT` review, approval only when nothing survives and never of a draft, a tally. Your `DISPATCH_SPEC` carries only what is specific to *this* PR (what to look at, prior findings to re-verify). Frame its questions as "report in your tally: …" (answered there, not on the PR), and never ask the worker to write context (bench evidence, sibling-PR composition) onto the PR. Tier still sizes the reviewer fan-out.
