# Review workers attach to the PR head branch

Closes #8.

## Problem

When the dispatcher scaffolds a REVIEW-ONLY worker for an existing PR,
`dispatch` always runs `wt switch -c` and mints a new branch off the default
branch (e.g. `feat/3071-review-pr-3071-…`). The authored `WORKER_TASK.md` then
tells the worker to stay on main and inspect the PR via `gh pr diff` /
`git fetch origin pull/N/head`.

That breaks:

1. **lazytmux PR decorations** — `tmux-pr-enrich` matches with
   `gh pr list --head <local-branch>`. A sibling review branch is never a PR
   head, so `@pr_number` stays `none`.
2. **Review quality / ergonomics** — the worktree is not on the PR commits; the
   worker reconstructs file contents through fetch/show instead of a normal
   checkout.

## Design

### Mechanism: `dispatch --pr N`

Add an explicit review-attach flag. Mutually exclusive with a Linear id and with
a GitHub issue token (`#N` / `N`).

```
dispatch <tier> <model> --effort <…> --pr N [--agent …] [--plan …] [--crew-id …] <title…>
```

Behaviour:

1. Resolve once:
   `gh pr view N --json headRefName,isCrossRepository`
2. Prefer switching by the known branch name (no unnecessary forge round-trip
   inside worktrunk):
   - If `refs/heads/$head` or `refs/remotes/origin/$head` exists →
     `wt switch "$head" -y --config-set 'post-switch.tmux=""'` (**no** `-c`).
   - Else if same-repo (`isCrossRepository=false`) →
     `git fetch origin "$head"` then the same `wt switch "$head" …`.
   - Else (fork / still missing) → fall back to `wt switch pr:N …` (worktrunk
     fetches `refs/pull/N/head`).
3. Scaffold the rest as today (locate worktree, stamp `WORKER_TASK.md`, tmux
   window, launch, stall-watch), but:
   - `branch` = `headRefName` (never `feat/N-review-…`).
   - Stamp `pr: N` in the task header instead of `Closes #N`. Do **not** mint a
     GitHub issue. Do **not** invent a closes line from the PR number.
4. Collision: if `$head` already has a worktree, `wt switch` reuses it (share).
   Review-only work accepts that; refuse-on-collision is out of scope.

### Protocol

In `DISPATCHER_PROTOCOL.md` (canonical under `adapters/core/protocols/`;
regenerate adapter copies via `scripts/gen-adapters.sh`):

- For **"review PR N"** tasks: call `dispatch … --pr N …`.
- Do **not** mint `feat/N-review-…` off main.
- Do **not** author REVIEW-ONLY specs that say "worktree sits on main" or that
  reconstruct the tree via `gh pr diff` / `git fetch origin pull/N/head`.
- The worker lands on the PR head; review the tree in place. Specs describe the
  review job (skills, lenses, what to post), not a checkout workaround.

### Out of scope

- Changing the generic launch prompt ("open a PR") — the task doc / review
  skills own review-only behaviour.
- Teaching lazytmux a PR-number fallback when the branch name does not match.
- Refuse-on-collision isolation from the author's live worktree.

## Acceptance criteria

- `dispatch … --pr N …` yields a worktree whose
  `git branch --show-current` equals the PR's `headRefName` (or otherwise makes
  `gh pr list --head <branch>` return that PR).
- No `feat/N-review-pr-N-…` (or similar) branch is created solely to host a
  review of PR N.
- `DISPATCHER_PROTOCOL.md` documents the review-attach path; projected plugin
  protocol copies stay in sync via `gen-adapters.sh`.
- Unit tests cover: `--pr` rejects co-use with issue/Linear tokens; attach path
  calls `wt switch` without `-c` and uses `headRefName`.
