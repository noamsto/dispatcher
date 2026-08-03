# Review-PR attach (`dispatch --pr N`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `dispatch --pr N` attach a review worker to the PR's `headRefName` instead of minting a sibling branch off main.

**Architecture:** Extend `dispatch.sh` with `--pr N` that resolves `headRefName` once via `gh`, fetches if needed, then `wt switch "$head"` (no `-c`); fall back to `wt switch pr:N` only for fork/missing-ref. Document the path in `DISPATCHER_PROTOCOL.md` and regenerate adapter protocol copies.

**Tech Stack:** bash (`dispatch.sh`), bats, `gh`, worktrunk (`wt`), `scripts/gen-adapters.sh`.

## Global Constraints

- Prefer `wt switch "$headRefName"` when the name is known; use `wt switch pr:N` only as fallback.
- Never `wt switch -c` on the `--pr` path.
- Stamp `pr: N`, not `Closes #N`, for review attach.
- Mutual exclusion: `--pr` cannot combine with Linear id or GitHub issue token.
- Collision = share existing worktree (no refuse path).
- Edit canonical protocols under `adapters/core/protocols/`; run `gen-adapters.sh` so plugin copies stay in sync.

---

### Task 1: `--pr` parsing + attach path in `dispatch.sh`

**Files:**

- Modify: `adapters/core/dispatch.sh`
- Modify: `tests/dispatch.bats`
- Modify: `tests/helpers.bash` (only if a richer stub helper is needed)

**Interfaces:**

- Consumes: existing option loop, `wt switch` / worktree locate / task stamp / launch
- Produces: `--pr <N>` flag; `branch=$headRefName`; header field `pr: N` instead of `Closes #…`

- [ ] **Step 1: Write failing bats for `--pr`**

Add to `tests/dispatch.bats`:

```bash
@test "rejects --pr combined with a GitHub issue token" {
  run run_dispatch standard sonnet --effort medium --pr 12 34 "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--pr"* ]]
}

@test "rejects --pr combined with a Linear id" {
  run run_dispatch standard sonnet --effort medium --pr 12 ENG-1 "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--pr"* ]]
}

@test "rejects --pr without a positive integer" {
  run run_dispatch standard sonnet --effort medium --pr "" "review"
  [ "$status" -eq 1 ]
}

@test "--pr path calls wt switch without -c and uses headRefName" {
  # Custom gh stub returns JSON; custom wt stub records argv and creates a
  # fake worktree entry so dispatch can locate the path.
  …
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Review PR 99"
  [ "$status" -eq 0 ]
  grep -q 'pr view 99' "$STUB_LOG" || grep -q 'pr view' "$STUB_LOG"
  ! grep -E 'switch -c |switch --create' "$STUB_LOG"
  grep -E 'switch .*eng-7691-foo|switch eng-7691-foo' "$STUB_LOG"
  grep -q 'pr: 99' "$wt_path/WORKER_TASK.md"
  ! grep -q 'Closes #' "$wt_path/WORKER_TASK.md"
}
```

Implement the last test with per-test stubs that:

1. `gh pr view` prints `{"headRefName":"eng-7691-foo","isCrossRepository":false}`
2. `wt switch` creates `$TEST_REPO/.wt-eng-7691-foo`, checks out/creates branch name as needed for porcelain listing, or appends a porcelain-shaped worktree the script can find — simplest: make `wt` create a real `git worktree add` at a temp path on branch `eng-7691-foo` after ensuring the ref exists in the test repo.

- [ ] **Step 2: Run tests — expect fail**

Run: `bats tests/dispatch.bats`
Expected: new tests FAIL (unknown `--pr` / no attach path).

- [ ] **Step 3: Implement `--pr` in `dispatch.sh`**

In the option loop, add:

```bash
--pr)
  pr_number="${2:-}"
  …
  shift 2
  ;;
```

After parsing title / before issue minting:

```bash
if [ -n "$pr_number" ]; then
  if [ -n "$linear_id" ] || [ -n "$gh_issue" ]; then
    echo "dispatch: --pr cannot combine with a Linear id or GitHub issue token" >&2
    exit 1
  fi
  # validate pr_number is digits
  pr_json=$(gh pr view "$pr_number" --json headRefName,isCrossRepository)
  head=$(printf '%s' "$pr_json" | jq -r .headRefName)
  cross=$(printf '%s' "$pr_json" | jq -r .isCrossRepository)
  branch="$head"
  closes="pr: $pr_number"   # stamped as its own header line — adjust printf
  # ensure ref, then:
  if git show-ref --verify --quiet "refs/heads/$head" ||
     git show-ref --verify --quiet "refs/remotes/origin/$head"; then
    wt switch "$head" -y --config-set 'post-switch.tmux=""'
  elif [ "$cross" = false ]; then
    git fetch origin "$head"
    wt switch "$head" -y --config-set 'post-switch.tmux=""'
  else
    wt switch "pr:$pr_number" -y --config-set 'post-switch.tmux=""'
  fi
else
  # existing linear / gh_issue / mint issue + wt switch -c path
fi
```

Refactor the stamp block so `closes` may be either `Closes #N` / `Closes ENG-…` **or** the `pr: N` line is emitted as its own header field (prefer a dedicated `pr_line` / keep `closes` empty and print `pr: N` when set — pick one shape and use it consistently in the test).

Recommended stamp shape:

```
tier: …
effort: …
plan: …
title: …
pr: 99          # when --pr
Closes #…       # when implement path
dispatcher_pane: …
…
```

Update `usage()` to mention `--pr <N>`.

- [ ] **Step 4: Run bats — expect pass**

Run: `bats tests/dispatch.bats`
Expected: all PASS.

- [ ] **Step 5: shellcheck**

Run: `shellcheck adapters/core/dispatch.sh`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add adapters/core/dispatch.sh tests/dispatch.bats tests/helpers.bash
git commit -m "$(cat <<'EOF'
fix(dispatch): attach review workers with --pr to the PR head

EOF
)"
```

---

### Task 2: Document review-attach in the dispatcher protocol

**Files:**

- Modify: `adapters/core/protocols/DISPATCHER_PROTOCOL.md`
- Run: `scripts/gen-adapters.sh` (updates `adapters/*/plugin/protocols/`)

**Interfaces:**

- Consumes: Task 1's `dispatch … --pr N` contract
- Produces: written rules for "review PR N" tasks

- [ ] **Step 1: Edit `DISPATCHER_PROTOCOL.md`**

In **Scaffold one worker per task**, after the Tracker bullet, add a **Review attach** bullet:

- For reviewing an existing GitHub PR N: pass `--pr N` (not an issue number, not a fresh title that mints `feat/N-review-…`).
- `dispatch` resolves `headRefName` and attaches with `wt switch` (no `-c`); the worktree's current branch is the PR head.
- Do not author REVIEW-ONLY specs that say the worktree sits on main or that reconstruct the tree via `gh pr diff` / `git fetch origin pull/N/head`.
- Task header stamps `pr: N` (no `Closes #N` from the PR number).

Also extend the usage fence to include `[--pr N]`.

- [ ] **Step 2: Regenerate adapters**

Run: `./scripts/gen-adapters.sh`
Expected: plugin protocol copies match core.

- [ ] **Step 3: Verify**

Run: `bats tests/adapters.bats tests/dispatch.bats`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add adapters/core/protocols/DISPATCHER_PROTOCOL.md \
  adapters/claude-code/plugin/protocols adapters/codex/plugin/protocols
git commit -m "$(cat <<'EOF'
docs(dispatcher): document --pr review-attach path

EOF
)"
```

---

### Task 3: Spec + plan already on branch; final verify

**Files:**

- Create (already): `docs/superpowers/specs/2026-08-03-review-pr-attach-design.md`
- Create: `docs/superpowers/plans/2026-08-03-review-pr-attach.md` (this file)

- [ ] **Step 1: Commit design + plan if not yet committed**
- [ ] **Step 2: Full test pass** — `bats tests/dispatch.bats tests/adapters.bats`
- [ ] **Step 3: Confirm acceptance** — `--pr` path has no `-c`; protocol mentions review attach; no review-sibling branch minting on that path.
