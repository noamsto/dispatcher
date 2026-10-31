---
name: follow-ups
description: Use when a worker msg opens a follow-ups (untracked) batch, to dedupe, file and link the entries
---

# follow-ups

A worker `msg` opening `follow-ups (untracked):` is no question (nobody awaits it) but the one reporting path for every tracker: collect the entries, then draft the batch. `tracker: github`: your own judgment under the repo owner's standing approval, no human step (the only follow-up standing approval; workers hold none). `tracker: linear <TEAM>`: one human approval per batch. `crew followups <PR> --entries <file.json>` prints the plan for the steps' mechanical part (dedupe candidates, same-area, threshold, filing, the PR-body swap); `--apply` writes it (GitHub-tracked PRs only; `--tracker linear`: plan only); `--draft` turns the worker's msg into a skeleton entries file. Steps:

- Dedupe first; prefer commenting on a covering issue. GitHub: `gh issue list --search "<key terms>" --state open`. Linear: the team's open tickets under the parent or by key terms (Linear tools).
- `same-area`: no new issue; comment on the origin (first `Closes`) or its open parent, first line `Follow-up of #<origin> (PR #<pr>)`, reopening a closed origin. No `Closes` or parent: a worker error; treat it as a normal entry. Linear: comment on the parent ticket.
- GitHub redesign threshold: `gh issue list --state all --limit 200 --search '"Follow-up of #" in:body,comments "<file path>" updated:>=<today minus 30 days>' --json number`, `<file path>` = the entry's `file`. Count distinct issues; marker issues and marker comments both count, so every follow-up comment or fold you write opens with the marker line. 3 or more → one redesign issue for the area, not another patch issue, comment or reopen. Linear: no threshold.
- New issues: self-contained title and body, evidence, why deferred, PR link. GitHub: body opens with the marker; `--assignee @me`; no `dispatched` label. Linear: labels; parent = the task header's first `Closes <TEAM>-<N>`, or for header `pr: N` the first one in that PR's body; team = the `tracker: linear <TEAM>` stamp; no closes line → no parent.
- GitHub: do the batch yourself (file; comment or fold; the redesign issue when tripped; `gh issue close --comment "<superseded by #N>"` for duplicated or subsumed issues), then report in one line (e.g. `follow-ups for PR #<pr>: filed #A, commented #B, closed #C`). Linear: show the human the whole batch (new tickets and comments); write only after one approval.
- Link each result under the PR's `## Follow-ups` (`#N — short title`), creating it and removing `## Follow-ups (untracked)` when only the latter exists.

A worker `done` with detail `follow-ups: untracked` means a batch was sent: nothing to do.
