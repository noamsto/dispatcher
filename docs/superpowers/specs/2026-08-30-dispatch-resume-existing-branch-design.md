# dispatch: resume an existing branch instead of dying on `wt switch -c`

Issue: #73

## Problem

`dispatch` has two paths that disagree about whether a branch may already exist.

The **reuse-or-refuse gate** (#17, hardened by #71/#72) explicitly handles a
branch that already has a worktree: it refuses when a worker is live there, and
_reclaims_ — kills the window, logs a `reclaim` event — when the newest bus row
is terminal. Reclaim exists precisely so a dead worker's tree can be taken over.

The **switch** immediately below it then runs, unconditionally for every non-`--pr`
dispatch:

    wt switch -c "$branch" -b "$default_base_oid" …

`wt switch -c` refuses any branch whose ref already exists. So the reclaim
succeeds, prints `dispatch: reclaimed @6 at …`, and the very next line kills the
dispatch:

    ✗ Branch feat/83-… already exists

No worker launches, the window is already gone, and the only way back to the
uncommitted work is to hand-commit it, push, open a draft PR and use `--pr`.

Two smaller defects compound it:

1. **The `dispatched` claim label outlives the run that took it.** The claim is
   written before scaffolding and released only by `crew reap`, when the closing
   PR merges. A worker killed before a PR exists leaves the label stuck, so the
   retry fails on the claim check _before_ it ever reaches the resume path —
   which makes the label gate part of the primary defect, not a side issue.
2. **A reworded retry silently forks a second branch.** The branch name is
   `feat/<id>-<slug(title)>`, and the slug is a lossy 40-character projection of
   the free-form title. Re-dispatching the same issue with different wording
   produces a _different_ branch name, which does not exist, so the create path
   succeeds — off a clean default branch — and strands the partial work in the
   original worktree with nothing said.

## Goals

- A dispatch onto an existing branch resumes it **end to end**: past the claim
  gate, no `wt switch -c` failure, worker launched in the existing worktree with
  uncommitted work intact and its task text preserved.
- The resumed worker is _told_ it is resuming, so it continues from the
  `SPEC.md` / `PLAN.md` already in the tree instead of re-running spec and plan.
- A dead crew's stale `dispatched` labels are released when that crew is adopted,
  and only the ones that crew provably took.
- A same-issue, differently-worded dispatch warns, with an actionable remedy.

## Non-goals

- Changing the occupancy / reclaim gate. #72 made a terminal `exited` row with a
  live engine pane refuse rather than reclaim; that stays exactly as it is. The
  resume decision lives strictly _after_ the gate and only decides how `wt` is
  invoked once the gate has already said yes.
- Rebasing, merging or resetting a resumed branch. Whatever is in the tree is
  what the worker resumes onto.
- Any change to `--pr` semantics, to Linear claim handling (Linear dispatches
  never take a `dispatched` label), or to issue #69's notify-hook / worker-id
  resolution.

## Ordering (settled once, because three sections depend on it)

Today: claim gate (`:328`) → `crew reap --quiet` (`:350`) → `slug` (`:353`) →
`crew_dir` (`:355`) → branch naming (`:380`+) → dispatch lock → occupancy gate →
switch.

`slug`, `crew_dir` and — for a dispatch carrying an issue token — `branch` are
**hoisted above the claim gate**. All three are pure (string work, one
`git rev-parse`, one `mkdir -p`); nothing is scaffolded, so the claim gate keeps
its stated property of running "before ANY scaffolding, reap's sweep included".
The branch-naming block below then reuses the already-computed `branch` for that
case rather than recomputing it.

`switch_mode` is resolved in exactly **one** place — the branch-naming block,
after `crew reap`. It is deliberately _not_ hoisted: `crew reap` calls
`wt remove` and can delete a merged branch, so a value computed before it could
be stale by the time the switch runs.

Two residuals are accepted rather than closed, consistent with the repo's stated
race posture (the claim gate's own comment says it narrows rather than closes
races). First, `switch_mode` resolves pre-lock but `wt switch` runs after the lock
is taken, so a concurrent `crew reap` in that window can delete the branch and
turn a `resume` into a switch onto a missing ref. Second, `_occupants` matches
`pane_current_path` exactly and never by prefix, so a live worker whose pane sits
in a _subdirectory_ of the worktree reads as unoccupied — pre-existing, but
`wt switch -c` was its accidental backstop and this change removes it.

That leaves one disagreement to define: the claim gate (above reap) may grant its
resume exemption for a branch that reap then deletes, so the run downgrades to
`create`. This is made harmless rather than detected — see §2: the exemption
skips only the _refusal_, never the `--add-label`. Whichever mode the run ends in,
the issue ends up labelled, so no branch is ever scaffolded against an unclaimed
issue. (Reap deleting the branch means its PR merged, and reap's own release
block drops the label in that case — so proceeding is correct, not merely safe.)

## Design

### 1. Resume: key on the branch ref, not on the reclaim

The issue suggests inferring resume from "the worktree was reclaimed _and_ the
branch resolves". We key on the **branch ref alone**, because that is exactly the
condition `wt switch -c` fails on:

- `wt switch -c` refuses on ref existence, whether or not a worktree hangs off
  it. A branch whose worktree was pruned or `wt remove`d has no `prev_wt`, so the
  reclaim never fires — and `-c` still dies. Keying on the reclaim leaves that
  case broken.
- Whether it is _legal_ to take the branch over stays with the untouched
  occupancy gate: a live worker, or #72's terminal-`exited`-plus-live-engine
  case, still `exit 1`s before the switch runs.
- It needs no new flag. A `--resume` flag is more explicit but adds a mode the
  repo can infer, and gets it wrong whenever the operator forgets it — the
  failure the issue reports.

In the non-`--pr` arm, replacing the unconditional `switch_mode=create`:

    if git show-ref --verify --quiet "refs/heads/$branch"; then
      switch_mode=resume
    else
      switch_mode=create
      # …unchanged gh repo view / git fetch / rev-parse, still pre-lock…
    fi

The `default_base_oid` resolution stays exactly where it is — inside the `else`,
**before** the dispatch lock — preserving the invariant in its own comment
("Resolved before the dispatch lock below, so a failure here costs no worktree
and no window"). It is skipped on a resume because it only ever pins a base for a
_new_ branch, so a resume needs neither `gh repo view` nor a fetch.

A new `resume)` arm calls `wt switch "$branch" -y --config-set "$wt_post_switch"`
— no `-c`, no `-b`, but the same `-y --config-set` every other arm carries, since
blanking worktrunk's tmux post-switch hook is what stops a second undecorated
window opening (#123). It echoes the branch with its current short HEAD so the
operator sees what is being resumed onto. It stays separate from `name)`, which
is the `--pr` attach and must stay silent and unstamped. No behind-count is
printed — that needs the fetch a resume deliberately skips.

#### 1a. Guard: resume must not attach to a non-worker worktree

`wt switch -c` was also, accidentally, what refused a branch checked out
somewhere a worker has no business opening. Occupancy cannot replace it:
`_occupants` matches only windows carrying `@crew_name` and skips the
`dispatcher` window and the caller's own, so three cases read as empty — the
primary checkout, the directory dispatch runs in, and a worktree a human is
sitting in with a plain shell.

All three are guarded. The first two are a path comparison (the primary worktree
from `git worktree list`, and reap's existing `case "$PWD/" in "$wtpath"/*)`
idiom). The third — the one that actually bites, since a worker window would open
and start editing under someone — is a tmux scan complementing `_occupants`
rather than reusing it: any window whose `pane_current_path` is that worktree and
whose `@crew_name` is **empty** is a non-worker occupant, and the resume refuses
with the window id. A killed worker leaves no window at all, so the repro this
change exists to serve is untouched; a live worker's window has `@crew_name` set
and is caught by the occupancy gate above instead.

### 2. Getting past the claim gate

The claim check refuses any dispatch whose issue already carries `dispatched`,
long before the switch. In the issue's headline repro the _worker_ died while the
dispatcher lived, so no crew is dead, nothing releases the label, and the retry
dies there — the resume path is never reached. Fixing only the dead-crew half
(§5) leaves Goal 1 unreachable in the exact scenario reported.

So the claim gate gains one narrow exemption: **an already-claimed issue may
still be dispatched when the branch this dispatch resolves to already exists.**

- The label stops two crews forking work on one issue. Resuming the _same_ branch
  is not a fork: the claim is not taken twice, it is reused by the run continuing
  it.
- Keyed on the exact resolved branch, never on "some `feat/<id>-*` exists". A
  differently-worded dispatch onto a live worker's issue resolves to a name that
  does not exist and is still refused — the fork the label exists to prevent.
- Safety is unchanged: who is live on that branch is decided by the occupancy
  gate downstream, as for every other dispatch.
- **The exemption skips the refusal only.** `--add-label` still runs; it is
  idempotent on an already-labelled issue, and always running it is what makes
  the reap-downgrade case in _Ordering_ harmless.

It reports on stderr that it is proceeding onto an existing branch for an
already-claimed issue, so the operator sees the guard was consciously bypassed.

### 3. Preserving the task doc across a resume

`WORKER_TASK.md` is truncated and rewritten on every dispatch, and its `## Task`
body is only written when `DISPATCH_SPEC` points at a file. A resume issued
without re-passing `DISPATCH_SPEC` would leave a header-only doc, destroying the
task text — and, on a `plan: provided` run, the plan of record — of the run it is
meant to continue.

**The capture must precede the redirection.** `{ … } >"$wt_path/WORKER_TASK.md"`
truncates the target before the block's first command runs, so reading the old
file inside the block reads zero bytes. The prior body is therefore captured into
a variable _above_ the block:

    carried=""
    if [ "$switch_mode" = resume ] && [ -z "${DISPATCH_SPEC:-}" ] \
       && [ -f "$wt_path/WORKER_TASK.md" ]; then
      carried="$(sed -n '/^## Task$/,$p' "$wt_path/WORKER_TASK.md")"
    fi

and emitted inside it when non-empty. A supplied `DISPATCH_SPEC` still wins, so
an operator can deliberately re-scope a resumed worker. `--review` requires
`--pr`, which never takes the resume path, so the appended review contract cannot
interact with this.

### 4. Telling the worker it is resuming

Two carriers, matching how `plan: provided` is already signalled:

- **`WORKER_TASK.md` header** — a `resume: true` line, stamped only when
  `switch_mode=resume`, alongside the existing `pr:` / `base:` optional lines.
- **Launch prompt** — a `resume_note`, mirroring the existing `plan_note`. The
  launch prompt is a user-turn instruction, which outranks skills; it is what
  actually stops a fresh session from re-specifying the task.

`--pr` dispatches also attach to an existing branch but are _not_ stamped
`resume: true`: they carry `pr:`/`base:`, their worktree is verified against the
PR head, and `kind: review` workers have no spec/plan phase to skip.

`WORKER_PROTOCOL.md` gains a **Resuming a killed run** subsection next to _Plan
of record_. `SPEC.md` and `PLAN.md` are named nowhere in the protocol today (only
in reap's scaffold list; `DECOMPOSITION.md` alone already appears, in
_Orchestration consult_), so the subsection establishes them rather than assuming
them: `SPEC.md` / `PLAN.md` / `DECOMPOSITION.md` at the worktree root, or their
`docs/superpowers/` equivalents. It states:

- Read those artifacts and `git status` / `git diff` to see how far the previous
  session got; the uncommitted work is prior progress, not scaffolding to discard.
- Do **not** re-run the spec or plan phases. Continue from the first unfinished
  step.
- If they are absent or contradicted by the tree, fall back to the tier's normal
  phases — the same re-entry rule _Plan of record_ already states.
- **Before pushing, check whether this branch already has an open PR**
  (`gh pr view --json url,state`). A resume can land on a branch that already
  reached `pr_open`, which `wt switch -c` used to make unreachable; the protocol's
  terminal step ("open a PR, and stop") and the launch prompt's push mandate are
  both unconditional, so an unguarded resumed worker runs a full pipeline and then
  hard-fails on `gh pr create`. When a PR is already open, push to it, skip
  `gh pr create`, and report `crew status … pr_open "" <existing url>` with that
  url — a missing or wrong url there mis-drives reap, which reads that row to
  decide reaping and label release.

`DISPATCHER_PROTOCOL.md` is amended too. Its **Claim** bullet is the only place
in the repo that documents the claim gate, and it currently says an existing-issue
dispatch "aborts naming the issue (no branch/worktree/window)" and that `crew reap`
is what removes the label. §2 makes the first clause false and §5 makes the second
incomplete, so a dispatcher agent reading it would keep telling operators to remove
the label by hand — the workflow this change exists to delete.

**Four existing `WORKER_PROTOCOL.md` rules are amended in the same edit, so no two
rules survive giving opposite instructions:**

- **_Pipeline by tier_, the `deep` bullet — and the `Orchestration consult`
  preamble.** The **standard** bullet already says "consult **Plan of record**
  (below) first"; the **deep** bullet does not, and runs straight into
  `spec-plan-critic` with `{ tier: 'deep' }` (spec + spec-critic) and then the
  consult. A deep resumed worker — the reported repro is itself `tier: deep` —
  would therefore re-litigate its settled `SPEC.md` _before_ ever reaching the
  `resume: true` carve-out further down. Both get the same clause shape the
  standard bullet uses, routing through the resume check first. This is **not**
  symmetric with `plan: provided`, where leaving the deep bullet alone is
  correct: that worker is supposed to still run its spec-critic.

- **_Plan of record_, the `plan: required` bullet.** `plan:` is re-stamped from
  the _new_ invocation on every dispatch and defaults to `required`, so a resumed
  worker would read "run the tier's plan phase" while the new subsection says not
  to. A precedence sentence at the top of _Plan of record_ states that
  `resume: true` is read first and outranks `plan:`, with the "absent or
  contradicted by the tree" fallback as the escape.
- **_Plan of record → Scope_** says a deep worker may never skip its spec-critic.
  That rule is about a dispatcher-authored task doc, which has never faced a
  critic. A recovered `SPEC.md` is the _output_ of a spec-critic gate in the
  interrupted run of this same task, so re-running it re-litigates settled work.
  Scope is amended to carve out `resume: true` explicitly.
- **_Rule 5_** mandates a `## Plan` line of `task doc (provided)` or
  `(self-gate)` whenever the plan phase was skipped; a resume matches neither, so
  the audit requirement is unsatisfiable as written. It gains
  `Plan: recovered (resume)`.

The metrics record reports `plan_critic_first_pass: null` for a run that skipped
the plan phase this way, so the ratings store is not told a critic ran.
`resume:` is added to the header-field list in _First action_.

### 5. Releasing a dead crew's stale claims on adopt

`crew adopt <id>` already refuses when the crew has a live dispatcher (absent
`--force`), so reaching the release code means the pid was dead — the condition
the issue names (`crew crews` → `alive: no`). The release runs only when the
_pre-existing_ pid was absent or dead, so `--force` over a genuinely live
dispatcher releases nothing. That liveness value is currently computed _inside_
the `if [ -z "$force" ]` guard, so it must be hoisted out of it to exist on the
`--force` path at all.

**Claims are read from an explicit record written at claim time.** Two properties
force this. First, the `kind:"dispatch"` row carries no issue number and cannot
distinguish an issue dispatch from a `--pr` review dispatch onto someone else's
`feat/<id>-…` head, so a branch-name regex would release claims the crew never
took. Second, that row is written ~330 lines after the label is added — every
failure in between (`gh repo view`, `git fetch`, lock contention, the occupancy
refusal, `wt switch`, `direnv allow`, claude pre-trust) would leave a label set
with no record of it, permanently unreleasable.

So `dispatch` appends `{kind:"claim-issue", crew_id, issue, branch}` to the bus
**immediately after the successful `--add-label`**, at both claim sites (a passed
issue token and a freshly minted one). For a passed token `branch` is available
because of the hoist in _Ordering_; the minted site instead needs a one-line
reorder, since today its `--add-label` runs just _before_ `branch` is assigned.
This is why `crew_dir` is hoisted too. Adopt keys on this
row, which excludes Linear and `--pr` by construction. Rows predating this change
carry no claim record, so they release nothing — the release never acts on a
claim it cannot prove, and that residual is bounded to already-existing claims.

**A claim record proves the crew once took a claim, never that it still holds
one.** Nothing invalidates a `claim-issue` row: reap releases the label on merge
and adopt releases too, but neither writes a counter-record. So crew C's row for
issue #83 outlives C's own release, and if crew D later re-claims #83 and is
working it now, adopting the long-dead C would strip D's live claim — the exact
fork the label prevents, and a supported topology (registration is explicitly
non-exclusive). The filter is therefore **newest-claim-wins**: a row is a release
candidate only when it is the newest `claim-issue` row for that issue across
_all_ crews **and** its `crew_id` is the adopted crew — one
`group_by(.issue|tostring) | map(max_by(.ts))` ahead of the crew filter. Grouping
normalises the issue to a string: `group_by` compares raw JSON, so a numeric `73`
and a string `"73"` would form separate groups while `gh` targets one issue.

Newest-claim-wins decides _candidacy_ only. The occupancy and open-PR checks below
run against **every** branch ever claimed for that issue, by any crew — not just
the newest row's. One issue can carry rows from two crews on two branches (a state
this design permits: §6 warns about a fork rather than refusing it), and checking
only the newest row's branch would release a label while another crew is live on
the other branch.

This narrows the race rather than closing it, in the same sense the claim gate
already documents: the candidate set is a snapshot, `gh` has no compare-and-swap,
and a dispatch that claims the issue between the snapshot and the
`gh issue edit --remove-label` will have its fresh label stripped. Accepted, not
solved — the window is one operator-invoked command, and the alternative is a
distributed lock this repo deliberately does not have.

Candidates are further filtered: a branch still occupied by a live worker window
is skipped, and so is one that is the head of an **open PR** — an open PR means
the work landed far enough to still hold the issue, and `crew reap` releases it
when the PR merges. Releasing there would let a second dispatch fork a rival
branch for an issue that already has a PR.

`gh issue edit <n> --remove-label dispatched` on what remains, best-effort: adopt
is a recovery command and must not fail because `gh` is unavailable. All `gh`
work is skipped entirely when there are no candidates, so the ~10 existing adopt
tests — and the common adopt path — make no network call.

**Every message goes to stderr.** `adopt`'s stdout is a machine channel — the
documented caller is `CREW_ID=$(crew adopt <id> $PPID)` — and one extra stdout
line would make `CREW_ID` multi-line, which then becomes a path component and the
`crew_id` on every bus event. Stdout stays exactly one line: the bare id.

### 6. Warning on a sibling branch for the same id

Only in the `create` arm — by definition the branch we are about to create does
not exist, so any _other_ branch for the same id is a fork in the making. In the
`resume` arm the exact branch matched, so there is nothing to warn about.

Prefix is `feat/<gh_issue>-` for a GitHub issue and `<lowercased linear-id>-` for
a Linear ticket. Local heads only: the stranded work the issue describes is
uncommitted and local.

**The remedy must be actionable, and the title is untrusted text.** The slug is a
lossy 40-character projection, so the operator cannot reconstruct the title from
the branch name; the original survives on the `kind:"dispatch"` bus row. But that
title is free-form operator text being replayed to a terminal, so it is rendered
as **data on its own line, never interpolated into a paste-ready command** —
building correct shell quoting for an arbitrary title is exactly where this would
break. Control characters are stripped with `gsub("[[:cntrl:]]";"")`, the same
discipline the model gate already applies when echoing a slug.

    dispatch: branch(es) for this id already exist:
      feat/83-feat-ui-give-the-issue-board-a-sections
    dispatch: creating feat/83-resume-issue-board-sections-view instead — the work
      in the branch above will be left behind. To resume it, re-dispatch with its
      original title:
        feat(ui): give the issue board a sections view (Mine / Others)

With §2 in place that re-dispatch resolves to the existing branch and resumes, so
the offered alternative actually works. When no bus row carries the title, the
warning says so rather than implying one can be recovered.

Warn, do not refuse and do not silently reuse: the operator may legitimately want
a second branch, and picking for them is what the issue asks us not to do.

## Acceptance

1. Dispatching onto an existing branch whose session is terminal reclaims the
   window, calls `wt switch` **without** `-c`, and launches a worker in the
   existing worktree. Uncommitted work in that worktree survives the dispatch.
2. An existing branch with **no** worktree also resumes (no `-c`), proving the
   gate is ref existence rather than the reclaim.
3. A resume needs no default-branch resolution — provable non-vacuously by
   removing the `origin` remote in the fixture, so any `git fetch origin` would
   fail the dispatch, and asserting it still succeeds.
4. A dispatch whose issue already carries `dispatched` proceeds when the resolved
   branch exists, and is still refused when it does not. The label is (re-)added
   on the exempted path.
5. A resume with no `DISPATCH_SPEC` leaves a `WORKER_TASK.md` that still has its
   original `## Task` body under a freshly stamped header.
6. A resumed dispatch stamps `resume: true` and puts a resume instruction in the
   launch prompt; a create-mode dispatch stamps neither.
7. Resume refuses when the branch's worktree is the primary checkout, the
   directory dispatch runs in, or carries a tmux window with no `@crew_name` —
   nothing switched, nothing launched.
8. A live worker on the branch still refuses, unreclaimed and unlaunched, on the
   **non-`--pr`** path — the `working` case and #72's terminal-`exited`-with-
   live-engine case both.
9. A successful claim writes a `claim-issue` bus row carrying the issue and
   branch; a `--pr` or Linear dispatch writes none.
10. `crew adopt` on a dead-pid crew removes `dispatched` from the issues that
    crew recorded claiming, and leaves alone: another crew's claims, `--pr` and
    Linear dispatches, branches with a live occupant, branches with an open PR,
    and an issue whose claim was later re-taken by another crew. Its stdout is
    still exactly the bare crew id.
11. A create-mode dispatch for an id that already has a differently-named branch
    warns, names that branch, prints the original title from the bus as data with
    control characters stripped, and still creates and launches (exit 0).
12. A resume onto a branch that already has an open PR is told so by the
    protocol: the worker pushes to the existing PR rather than calling
    `gh pr create`.
13. bats regression tests for criteria 1–12. The non-`--pr` fixture extends the
    existing create-mode gate test ("gate: refuses a live occupant on a
    create-mode re-dispatch") rather than being built from scratch; criterion 12
    is a protocol-text assertion in `tests/adapters.bats`, the only place this
    repo pins protocol wording. `tests/crews.bats` stubs nothing in `setup()`, so
    criterion 10 needs both `gh` and `tmux` stubs. `shellcheck adapters/core/*.sh
scripts/*.sh` clean; `bats tests/` green; adapters in sync.
