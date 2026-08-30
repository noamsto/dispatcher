# Plan: dispatch resumes an existing branch (#73)

Spec: `docs/superpowers/specs/2026-08-30-dispatch-resume-existing-branch-design.md`
**The spec is authoritative for every message string.**

Sequencing: A (hoist + claim gate) before B (switch mode) before C (step 8 reads
`switch_mode`, which B/step 4 sets — not A's hoist). E before F only so the
adapters are regenerated ahead of the drift gate.

Every code step ends with `bash -n <file>` + `shellcheck <file>`; steps touching
dispatch.sh also re-run one named existing bats test to prove nothing regressed.

## A. dispatch.sh — ordering, claim gate, claim record

- [ ] **Step 1: hoist `slug`, `crew_dir`, and the issue branch above the claim gate**
  - Move `slug=`, the `crew_dir=` + `mkdir -p` pair, and — for a dispatch carrying
    `$gh_issue` — `branch="feat/$gh_issue-$slug"` to just above the
    `if [ -n "$gh_issue" ]` claim gate. Safe: `--pr` and an issue token are already
    mutually exclusive, `crew_id` and `title` both exist by then.
  - Leave `crew reap --quiet` where it is (after the gate): the claim-gate comment
    documents that read-then-claim runs before reap's sweep.
  - The branch-naming block reuses the computed `branch` for the `$gh_issue` case.
  - Comment why the hoist is safe: pure string work + one `git rev-parse` +
    `mkdir -p`, so the gate keeps "before ANY scaffolding".
  - _Verify:_ `bats tests/dispatch.bats -f "claim:"` — `claim: adds dispatched to
a free existing issue before crew reap` asserts `claim_line < reap_line`, so it
    pins this hoist directly.

- [ ] **Step 2: claim-gate resume exemption**
  - When the label is present, do not refuse if
    `git show-ref --verify --quiet "refs/heads/$branch"`; print the spec's stderr
    line. Refuse exactly as today when the branch does not exist.
  - `gh issue edit --add-label dispatched` runs on **both** paths (idempotent) —
    this is what makes a later reap-driven `resume→create` downgrade harmless.
  - _Verify:_ `bats tests/dispatch.bats -f "claim:"` (the _occupancy_ `gate:` tests
    never touch the claim gate).

- [ ] **Step 3: `claim-issue` bus row at claim time**
  - Immediately after each successful `--add-label`, append
    `{ts, crew_id, kind:"claim-issue", issue, branch}` to `$crew_dir/events.jsonl`.
  - Two sites: the passed-token gate, and the minted-issue path — reorder the
    latter so `branch="feat/$num-$slug"` is assigned _before_ the record is written.
  - A new `kind` is safe against every existing jq filter (they match `worker:`
    prefixes on `.from`, or `"claim"` exactly).
  - _Verify:_ `bats tests/dispatch.bats -f "claim:"`.

## B. dispatch.sh — resume mode, guard, fork warning

- [ ] **Step 4: resolve `switch_mode` by ref existence**
  - Replace the unconditional `switch_mode=create` with the `git show-ref` branch;
    move the `gh repo view` / `git fetch` / `rev-parse` default-base resolution
    into the `else`, still pre-lock. `default_base_oid`/`_label`/`_short` are read
    only inside the `create)` arm, so `set -u` is not tripped.
  - _Verify:_ `bats tests/dispatch.bats -f "stale default"`.

- [ ] **Step 5: `resume)` arm in the switch `case`**
  - `wt switch "$branch" -y --config-set "$wt_post_switch"` — no `-c`, no `-b`.
  - Echo the branch and its short HEAD. Keep it separate from `name)`.
  - _Verify:_ `bats tests/dispatch.bats -f "gate:"`.

- [ ] **Step 6: resume guard against non-worker worktrees** (implement: opus)
  - Sits in the `resume)` arm, i.e. **after** the post-lock occupancy gate, so a
    live worker is still refused by that gate first and this only ever sees a
    worktree the gate let through.
  - Resolve the branch's existing worktree (`git worktree list --porcelain | awk`,
    the idiom already used twice in this file). The **primary** worktree is the
    first `worktree ` record of `git worktree list --porcelain`.
  - Refuse when it is that primary worktree, when `$PWD` is inside it (reap's
    `case "$PWD/" in "$wtpath"/*)`), or when a tmux window has that exact
    `pane_current_path` with an **empty** `@crew_name`. The tmux scan complements
    `_occupants` (which requires a non-empty `@crew_name`) — it must not reuse it.
  - Known seam, accepted: a window whose `@crew_name` is literally `dispatcher`
    at the target path is skipped by both `_occupants` and this scan. The `$PWD`
    check covers the case that matters (the dispatcher running there).
  - Message names the offending path/window and what to do.
  - _Verify:_ `bats tests/dispatch.bats -f "gate:"`.

- [ ] **Step 7: sibling-branch warning in create mode** (implement: opus)
  - Emit inside the `create)` case arm, after the branch is created, so a run that
    refuses earlier never warns about a fork it did not make.
  - `git for-each-ref` on `refs/heads/feat/<id>-*` (or `<linear-id>-*`); warn when
    any exists, naming them.
  - Recover the title from the newest `kind:"dispatch"` bus row for **the sibling
    branch** (the one already on disk), not the branch being created.
    The log may not exist yet (a Linear dispatch appends nothing before this
    point), and bare `jq` on a missing file exits 2 — fatal under
    `set -euo pipefail`. Guard with `2>/dev/null || true` and route an empty
    result into the spec's "no bus row carries the title" wording.
  - _Verify:_ `bats tests/dispatch.bats -f "claim: a Linear-tracked dispatch"`.
  - Strip control chars (`gsub("[[:cntrl:]]";"")`); print the title as **data on
    its own line**, never interpolated into a command. Warn only; status unchanged.

## C. dispatch.sh — task doc and launch prompt

- [ ] **Step 8: carry the task body forward, stamp `resume: true`** (implement: opus)
  - Capture `carried` with `sed -n '/^## Task$/,$p'` **above** the stamp block
    (the `>` redirect truncates before the block runs), only when
    `switch_mode=resume`, `DISPATCH_SPEC` is unset, and a prior file exists.
  - `carried` already includes its own `## Task` heading, so the emit site must
    **not** wrap it in `printf '\n## Task\n\n'` the way the `DISPATCH_SPEC` branch
    does.
  - Emit `resume: true` in the header when `switch_mode=resume`.
  - _Verify:_ `bats tests/dispatch.bats -f "task headers preserve"`.

- [ ] **Step 9: `resume_note` in the launch prompt**
  - Mirror `plan_note`, appended to all three engine launch strings. It must
    carry: don't re-run spec/plan, read `SPEC.md`/`PLAN.md` and `git status`
    first, and check for an open PR before pushing. This is load-bearing — the
    launch prompt is a user-turn instruction that outranks the protocol.
  - **No apostrophes.** All three launch strings wrap the prompt in single quotes
    inside a double-quoted `tmux send-keys` argument; an apostrophe silently
    breaks the line. `plan_note` avoids them for exactly this reason.
  - _Verify:_ `bats tests/dispatch.bats -f "launch"`.

## D. crew.sh — release stale claims on adopt

- [ ] **Step 10: adopt-time claim release** (implement: opus)
  - Hoist the `epid`/`live` liveness computation out of the `if [ -z "$force" ]`
    guard so it exists on the `--force` path; release only when the pre-existing
    pid was absent or dead.
  - Candidates: `claim-issue` rows filtered **newest-claim-wins** —
    `group_by(.issue) | map(max_by(.ts))` across all crews, _then_ keep only rows
    whose `crew_id` is the adopted crew.
  - Resolve each row's branch → worktree path (same `git worktree list` idiom)
    before calling `_occupants`, which takes a path, not a branch. Skip a branch
    with a live occupant.
  - Skip a branch that heads an open PR: one `gh pr list --state open
--json headRefName` with an explicit `--limit` (the default 30 would read a
    busy repo's PR-bearing branch as PR-less and release its label).
  - `gh issue edit <n> --remove-label dispatched`, best-effort. Skip **all** `gh`
    work when there are no candidates, so the existing adopt tests stay offline.
  - **Guard the candidate query on `[ -f "$log" ]`.** `crew register` writes only
    `crews/<id>/pid`, never the bus log, so `events.jsonl` is routinely absent —
    and bare `jq` on a missing file exits 2, fatal under `bash -euo pipefail`.
    Unguarded this turns two currently-green tests red: `tests/crews.bats`
    "adopt: a registered pid of 0 does not read as a live dispatcher" and
    "adopt: a dead-pid registered crew is adoptable without --force", both of
    which register a pid, write no events, and take the dead-pid release branch.
    Use the file's own idiom (`[ -f "$log" ]` plus `2>/dev/null || true`) and
    route a missing/empty log to "no candidates", which already short-circuits
    every `gh` call.
  - Every message to **stderr**; stdout stays exactly the bare crew id.
  - _Verify:_ `bats tests/crews.bats -f "adopt:"`.

## E. protocols

- [ ] **Step 11: `WORKER_PROTOCOL.md` amendments**
  - _First action_: add `resume:` to the stamped-field list.
  - New **Resuming a killed run** subsection next to _Plan of record_: establishes
    `SPEC.md`/`PLAN.md` (root or `docs/superpowers/`; `DECOMPOSITION.md` is
    already named elsewhere in the protocol), says do not re-run spec or plan,
    continue from the first unfinished step, fall back when the artifacts are
    absent/contradicted, and check for an existing open PR before pushing — push
    to it, skip `gh pr create`, report its url on `pr_open`.
  - Amend the four conflicting rules: the _Pipeline by tier_ **deep** bullet and
    the _Orchestration consult_ preamble (route through the resume check first,
    same clause shape the **standard** bullet already uses), the `plan: required`
    bullet (a `resume: true` precedence sentence at the top of _Plan of record_),
    the _Scope_ bullet (carve out resume), and rule 5 (`Plan: recovered (resume)`).
  - Note `plan_critic_first_pass: null` for a resume-skipped plan phase.

- [ ] **Step 12: `DISPATCHER_PROTOCOL.md` — Claim bullet**
  - Amend to state the resume exemption (an already-labelled issue still
    dispatches when the resolved branch exists; the label is re-added on that
    path) and that `crew adopt` on a dead-pid crew releases that crew's recorded
    claims, alongside the existing `crew reap` sentence.

- [ ] **Step 13: regenerate adapters** — `./scripts/gen-adapters.sh`.

## F. tests

- [ ] **Step 14: `tests/adapters.bats` — protocol content**
  - One `@test` with a `grep -F` statement list in the style of the existing
    "worker protocol defines bounded plan-shaped gate recovery" test, pinning the
    load-bearing sentences of **Resuming a killed run** (including the open-PR
    clause — this is acceptance 12's only possible regression test), plus one
    assertion per amended rule and the `resume:` field in _First action_, and the
    amended `DISPATCHER_PROTOCOL.md` Claim bullet.

- [ ] **Step 15: `tests/dispatch.bats` — resume fixture** (implement: opus)
  - Build on the existing **non-`--pr`** create-mode gate test ("gate: refuses a
    live occupant on a create-mode re-dispatch") — `stub_launch_bins` + a
    hand-made `git branch` + `git worktree add` + `stub_crew_gate`. That pattern,
    not a from-scratch fixture, is the base.
  - `stub_launch_bins`' `wt` stub only handles `switch -c`, so the resume arm
    needs an override — and it must come in **two** shapes, because a single
    no-op stub cannot serve both of Step 16's first two tests:
    - _branch **with** a worktree_ (acceptance 1, 3, 6): a no-op `wt` is correct,
      since the worktree already exists on disk.
    - _branch with **no** worktree_ (acceptance 2): the stub must materialise one
      on a bare `switch <branch>` —
      `git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.dispatch-wt/${br//\//-}" "$br"`
      — mirroring what `stub_launch_bins` already does for `switch -c`. A no-op
      here leaves `wt_path` empty and dispatch exits 1 with
      `could not locate worktree`, failing the very criterion that proves the
      gate is ref existence rather than the reclaim.

- [ ] **Step 16: resume mechanics tests** — acceptance 1, 2, 3, 6
  - Reclaim-then-resume calls `wt switch` without `-c` and launches, with a dirty
    file in the worktree surviving; an existing branch with no worktree resumes;
    a resume succeeds with `origin` removed (so any fetch would fail — the
    non-vacuous form of "no default-branch resolution"); `resume: true` and the
    launch note present on resume and absent on create.

- [ ] **Step 17: claim-gate and task-doc tests** — acceptance 4, 5, 9
  - Exemption granted when the branch exists, refused when it does not, label
    re-added on the exempted path; `## Task` body carried forward under a fresh
    header with no `DISPATCH_SPEC`; `claim-issue` row written for an issue
    dispatch and not for `--pr`/Linear.

- [ ] **Step 18: guard and warning tests** — acceptance 7, 8, 11
  - The three resume refusals (primary checkout, dispatch's cwd, non-`@crew_name`
    tmux window) switch and launch nothing; a live worker still refuses on the
    non-`--pr` path, including #72's terminal-`exited`-with-live-engine case;
    fork warning names the sibling branch, prints the title as data, exits 0.

- [ ] **Step 19: `tests/crews.bats` — adopt release** — acceptance 10
  - `setup()` stubs nothing, so these tests need `stub_bin gh` **and** a `tmux`
    stub with a `list-windows -a -F` response, since the live-occupant case runs
    through `_occupants`.
  - Cover: release of this crew's recorded claims; another crew's claims,
    `--pr`/Linear rows, live-occupant branches, open-PR branches and a
    re-taken claim all left alone; stdout still exactly the bare crew id.

## G. gate

- [ ] **Step 20: full gate** — `shellcheck adapters/core/*.sh scripts/*.sh`,
      `bats tests/`, and `./scripts/gen-adapters.sh && git diff --exit-code`.
