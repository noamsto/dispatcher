# `dispatch migrate` — implementation plan

Spec (binding; C-numbers refer to it):
`docs/superpowers/specs/2026-11-17-dispatch-migrate-design.md`. Task:
`WORKER_TASK.md` (#1599). Go commands run in `crew/` or `dash/` under
`nix develop -c`; bats runs targeted files only (never the full suite
locally).

## File list

Crew (Go):

- `crew/internal/bus/bus.go` — `StateMigrated`; `Terminal()` includes it.
- `crew/internal/status/status.go`, `status_test.go` — accept `migrated`,
  require/refuse `--to-host`, body `to_host`, `clearRelaunch` for migrated.
- `crew/main.go` — `status --to-host`; `migrate stop|snapshot|mark`
  registration; `remote-run --migrate-dir`.
- `crew/internal/entry/texts/status.txt`, `remote-run.txt`, new `migrate.txt`
  — help text (help-screen tests read them).
- State lists learning `migrated`: `crew/internal/sessions/sessions.jq`,
  `crew/internal/reap/folds.go`, `crew/internal/roster/live.go`,
  `crew/internal/roster/roster_base.jq` (also forwards `to_host`),
  `crew/internal/rosterrender/planets.go`, `crew/internal/peer/snapshot.go`,
  `crew/internal/fleet/farwire.go`, `crew/internal/fleet/status.go`,
  `crew/internal/stall/post.go`, `crew/internal/stall/stall.go`,
  `crew/internal/stall/frame_detectors.go`,
  `crew/internal/stream/wants_reap.jq`; plus their existing `_test.go`
  files for the new cases.
- `crew/internal/rate/records.jq`, `crew/internal/retro/draft.go` (+ tests) —
  skip sessions whose last state is `migrated`.
- `crew/internal/reap/discard.go`, `remove.go` — export the patch snapshot
  (`Snapshot`) and stamp-anchored window kill (`KillWorkerWindows`) for reuse;
  `--discard` keeps calling them, behaviour unchanged.
- `crew/internal/reap/migrated.go` (new), `candidate.go`, `remove.go`,
  `reap_test.go`/new `migrated_test.go` — C7 candidate gates, no origin
  delete.
- `crew/internal/migrate/` (new: `migrate.go`, `migrate_test.go`) — C3
  `stop`, `snapshot`, `mark`, `Released` (the C2 rule in Go, used by reap).
- `crew/internal/remote/remote.go`, `exec.go`, `remote_test.go`,
  `exec_test.go` — C4 payload.

Dash (Go):

- `dash/internal/view/overview.go`, `dash/internal/data/digest.go`,
  `dash/internal/ui/board.go`, `dash/internal/ui/overview_render.go`,
  `dash/internal/ui/fleet_render.go` — terminal "moved" rendering.
- `dash/internal/view/attention_test.go` (+ fixture in
  `dash/internal/view/viewtest`) — no attention item for `migrated`.

Bash / packaging:

- `adapters/core/dispatch.sh` — `migrate` arm (exec `dispatch-migrate`),
  `--migrate-check` arm, `_branch_released_to`, `_remote_dup_holder`
  target-host rule, local re-dispatch guard, `DISPATCH_MIGRATE_BUNDLE`
  forward + env refusals, remote half (`DISPATCH_MIGRATE_DIR`), help text.
- `adapters/core/dispatch-migrate.sh` (new) — C5 orchestration.
- `adapters/core/dispatch-notify.sh`, `launcher-common.sh`, `dispatcher.sh`,
  `phase-status.sh` — `migrated` in their state cases.
- `flake.nix` — `dispatch-migrate` package (dispatch-resume pattern), in
  `dispatch`'s runtimeInputs **only**: it is reached as `dispatch migrate`,
  never shipped standalone, so it is not added to the default symlinkJoin
  (flake.nix ≈341) or `nix/hm-module.nix` packages, and gets no
  `completions/dispatch-migrate.fish` (the `completions.bats` "every launcher
  has a completion file and nothing else is shipped" names list stays as is).
- `completions/dispatch.fish` — `migrate` subcommand with `--host`, `--now`,
  `--wait`.
- `scripts/bats-affected.sh` — map `dispatch-migrate.sh` →
  `dispatch-migrate remote-host`.
- `tests/remote-host-helpers.bash` (new; setup/stubs moved verbatim from
  `tests/remote-host.bats`, plus a `dispatch-migrate` shim in `make_bins`'
  tool loop, an `SSH_FAIL_MIGRATE` shim knob and a remote-only bins dir),
  `tests/remote-host.bats` (loads it), `tests/dispatch-migrate.bats` (new).

Docs:

- `adapters/core/protocols/WORKER_PROTOCOL.md` — "Migrate directive" (C8).
- `adapters/core/protocols/DISPATCHER_PROTOCOL.md` — `migrated` in the
  terminal-state sentences; `dispatch migrate` pointer.
- `adapters/core/skills/remote-host/SKILL.md`, `adapters/core/skills/resume/SKILL.md`.
- Generated outputs of `scripts/gen-adapters.sh` (never hand-edited).
- This plan and the spec.

## Steps

### Phase A — the `migrated` state (C1)

- [ ] **Step 1: failing status tests.** `crew/internal/status/status_test.go`:
      `migrated --to-host mini` from a worker posts a row whose body has
      `state:"migrated"`, `to_host:"mini"`, no `source`; `migrated` without
      `--to-host` → exit 2; `--to-host` with `working` → exit 2; a bad host
      (`-x`, `a b`) → exit 2; a repeat of the same `migrated` is a silent
      no-op (terminal dedupe). `bus.State("migrated").Terminal()` is true
      (`bus_test.go`). Run `cd crew && go test ./internal/status/
./internal/bus/` → FAIL.
- [ ] **Step 2: implement.** `bus.go` `StateMigrated` + `Terminal()`;
      `status.go`: state case, `ToHost` on `StatusRequest`/`statusBody`
      (`json:"to_host,omitempty"`), host regex
      `^[A-Za-z0-9][A-Za-z0-9._-]*$`, the two `--to-host` refusals, add
      `migrated` to the terminal-dedupe set and to `clearRelaunch`;
      `main.go` `--to-host` flag on `status`; `texts/status.txt` lists
      `migrated` and `--to-host`. Run step 1's command → PASS; `go test
./internal/entry/` → PASS (help screens).
- [ ] **Step 3: failing state-list tests.** One new case each in the
      existing tests of `sessions` (migrated is `terminal:true`), `roster`
      (row forwards `to_host`; `Drawn` treats `migrated` like the waiting
      states), `rosterrender` (classify → done sector, not failed),
      `peer` snapshot (a `migrated` latest row is kept), `fleet` far-wire
      (accepts `migrated`) and finished window, `stall` (a `migrated`
      worker is terminal: no `quiet:`/`dead:` post), `stream` wants_reap
      (migrated wants reap), `rate` (a session ending `migrated` yields no
      record and no zombie), `retro` (skipped). Run `go test
./internal/{sessions,roster,rosterrender,peer,fleet,stall,stream,rate,retro}/`
      → FAIL.
- [ ] **Step 4: implement the state lists** in the files listed under
      "State lists" and `rate/records.jq` / `retro/draft.go`. The rate
      `migrated` case lives in its own test, not in
      `records_index_test.go` (that test holds `records.jq` to the
      `testdata/records_scan.jq` oracle); if the oracle test fails anyway,
      give `records_scan.jq` the same skip. Run step 3's command → PASS.
- [ ] **Step 5: bash state cases.** `dispatch-notify.sh` silent case gains
      `*:migrated` (so the SessionEnd hook posts no `exited` after a
      worker-posted `migrated`); `launcher-common.sh` status format shows
      `migrated` neutrally (not the failed colour) and its relaunch logic
      never relaunches a `migrated` lead; `dispatcher.sh` counts and
      `phase-status.sh` treat it as terminal. Add one bats case to
      `tests/dispatch-notify.bats`: last state `migrated` → no `exited`
      row. Run `nix develop -c bats tests/dispatch-notify.bats` and
      `nix develop -c shellcheck adapters/core/dispatch-notify.sh
adapters/core/launcher-common.sh adapters/core/dispatcher.sh
adapters/core/phase-status.sh` → PASS.
- [ ] **Step 6: dash.** Failing first: `dash/internal/view/attention_test.go`
      case with a `migrated` worker (fixture row with `to_host`) asserting no
      attention item in overview and fleet attention; render tests: glyph
      `→`, board group done, not red, detail `moved to <host>`. Then
      implement in `overview.go` / `digest.go` (`terminalStates`),
      `board.go`, `overview_render.go`, `fleet_render.go` (counter + legend).
      Run `cd dash && go test ./...` → PASS (update goldens only for the
      new fixture rows, never unrelated ones).

### Phase B — crew primitives (C3, C4, C2 in Go)

- [ ] **Step 7: refactor reap's snapshot and window kill (no behaviour
      change).** Extract from `reap/discard.go` an exported
      `Snapshot(ctx, SnapshotRequest{Common, Admin, WtPath, Branch, Dir,
Name}) (SnapshotResult{Patch, Tree, Tip}, error)` wrapping
      `scratchTree` + `hasGitlink` + `writeDiff` + symlink guard +
      no-clobber link, and an exported `KillWorkerWindows(ctx, crew,
branch, wt)` from `killWindows`/`killLeftovers`. `--discard` calls
      them. `SnapshotRequest` also carries `IgnoredScaffold bool`
      (default false, which `--discard` and `discardRecheck` keep, so
      their behaviour is unchanged): when true, the scratch tree also
      force-adds the ignored root scaffold names that **exist** (filtered
      with `ls-files -o -i --exclude-standard` or a stat, never a
      pathspec for a missing file). Export the tree-only half too
      (`ScratchTree(…, ignoredScaffold)`) so C7 computes its comparison
      tree exactly as migrate did. Run `cd crew && go test
  ./internal/reap/` → PASS unchanged.
- [ ] **Step 8: failing `crew migrate` tests** (`internal/migrate/
migrate_test.go`, temp repos like reap's tests): `snapshot` of a
      worktree with a modified tracked file, a staged new file and an
      untracked file writes `<artifacts>/migrate-<ts>/patch` whose
      `git apply` onto a fresh checkout of the tip reproduces the scratch
      tree sha; root scaffold the repo gitignores (`SPEC.md`, `PLAN.md`,
      `DECOMPOSITION.md`, `REVIEW_NOTES.md`, `PLAN_ROUND<n>.md` — the
      `scaffoldRe` names in `reap/candidate.go:56` minus `WORKER_TASK.md`,
      which the remote writes itself) is force-added to the scratch index so
      it travels in the patch, while `WORKER_TASK.md` never does; a worktree
      with only `SPEC.md` present (the other scaffold names absent) snapshots
      fine; clean tree
      → `patch:""`; embedded git repo → refusal; live
      engine at the worktree → refusal; git-config drift → refusal;
      `mark` appends a `migrated` row with `source:"migrate"`, `to_host`,
      `mode`, `tip`, `tree`, `patch` under the newest session id; `mark`
      and `snapshot` refused when `CREW_WORKER_ID` is set; `Released(rows,
branch, host)` true for the mark, true when an `exited` or
      `source:"watchdog"` row follows it, false after a later `working`,
      false for another host, false for a worker-posted `migrated` (no
      source); `stop` returns once the fake engine process is gone and
      fails naming it when it survives the timeout. Run `go test
./internal/migrate/` → FAIL.
- [ ] **Step 9: implement `internal/migrate`** and register `crew migrate
stop|snapshot|mark` in `main.go` (subcommand lists, kong struct,
      `texts/migrate.txt`), dispatcher sender check + `wtgit.TrustedCwd`,
      worktree from the anchored record. Run step 8's command, `go test
./internal/entry/ .` → PASS. (implement: escalated)
- [ ] **Step 10: failing remote payload tests.** `remote_test.go`:
      `--migrate-dir` builds `migrate:{meta, patch_gz, transcript_gz}`
      (gzip+base64), omits absent files, refuses a missing `meta.json`.
      `exec_test.go`: `migrate` with argv `dispatch standard opus …` admitted
      and files written 0600 under scratch `migrate/` with
      `DISPATCH_MIGRATE_DIR` exported; refused with `dispatch --engines`,
      `dispatch resume`, `crew reply`; request 2 MiB without `migrate` →
      "request over 1 MiB"; 20 MiB with `migrate` admitted, 33 MiB refused;
      a blob decompressing past 128 MiB refused; meta refusals (bad branch
      `-x`/`a..b`, bad tip, bad session, bad mode, unknown key, handoff over
      64 KiB). Run `go test ./internal/remote/` → FAIL.
- [ ] **Step 11: implement the payload** (`remote.go` `Call.MigrateDir`,
      `exec.go` `requestKeys`, two-tier cap, bounded gunzip, meta struct
      strict decode, file writes); `main.go` `--migrate-dir` on
      `remote-run`; `texts/remote-run.txt`. Run step 10's command → PASS.
      (implement: escalated)
- [ ] **Step 12: failing reap tests** (`reap/migrated_test.go`): a
      `migrated`+`source:"migrate"` worker whose worktree is clean and
      pushed, with a newer `remote:true` dispatch row on the mark's host →
      removed, `reaped` posted, local branch deleted, origin branch NOT
      deleted; kept (with reason) when: no remote row, remote row on another
      host, local tip not equal to the mark's `tip`, mark tip not an
      ancestor of `refs/remotes/origin/<b>`, tree dirty beyond the mark's
      tree, live engine. Removed when `origin/<b>` is **ahead** of the mark
      tip (the remote worker pushed and a local fetch ran). A worktree whose
      scratch tree equals the mark's `tree` (the patch's content still
      present) is removed, including one holding an ignored `SPEC.md` that
      the mark's tree included (C7 uses `ScratchTree(…, true)`). A
      worker-posted `migrated` (no source) is kept.
      Run `go test ./internal/reap/` → FAIL.
- [ ] **Step 13: implement C7** in `reap/migrated.go` + hooks in `consider`
      / `remove` (skip PR/issue gates for this candidate kind, skip
      `deleteOriginBranch`), add `migrated` to the candidate terminal set.
      Run step 12's command and `go test ./...` in `crew/` → PASS.
      (implement: escalated)

### Phase C — dispatch.sh (C2 bash, C4 local, C6)

- [ ] **Step 14: test harness extraction (no behaviour change).** Move
      `tests/remote-host.bats`'s `setup`, `teardown`, `write_remote_env`,
      `make_bins` and helpers verbatim into `tests/remote-host-helpers.bash`
      (functions named `remote_host_setup`/`remote_host_teardown`, called
      from the bats file's `setup`/`teardown`). Additive harness pieces, each
      inert for the existing tests: `dispatch-migrate:dispatch-migrate.sh`
      in `make_bins`' wrapper loop (beside `dispatch-resume`); an
      `SSH_FAIL_MIGRATE` knob in the ssh shim (like
      `SSH_DROP_AFTER_DISPATCH`, it reads the request into a temp file and
      exits 255 without running it when `jq -e 'has("migrate")'` holds, so
      preflights still pass); a remote-only bins dir `$RBINS` prepended to
      the remote `PATH` in `write_remote_env` (empty by default) so a test
      can stub a remote-side `dispatch` without touching the local one. Run
      `nix develop -c bats tests/remote-host.bats` → same pass count as
      before the move (record both counts in the step report).
- [ ] **Step 15: failing bats for the claim rule** in new
      `tests/dispatch-migrate.bats` (loads the helper): with a local
      dispatch row for `feat/7-x` and a `migrated` `source:"migrate"`
      `to_host:mini` status row appended by `crew migrate mark`:
      `dispatch … --host mini 7 -- x` passes the dup guard (reaches the
      remote), `--host chromebox` is refused naming `migrating to mini`; an
      `exited` row and a `source:"watchdog"` `blocked` row after the mark
      keep the release; a later `working` row ends it; a plain local
      `dispatch … 7 -- x` is refused with the migrated message. Run
      `nix develop -c bats tests/dispatch-migrate.bats` → FAIL.
- [ ] **Step 16: implement C2 in dispatch.sh**: `_branch_released_to`
      (newest worker via `kind:"resume"` chain as `_branch_finished`;
      newest status ignoring `exited` and `source:"watchdog"`;
      fail-closed), `_remote_dup_holder` takes the target host and passes
      released rows only for it, the holder text, and the local re-dispatch
      guard next to the claim gate's resume exemption (newest dispatch row
      `remote:true`, or released to any host). Run step 15's command,
      `nix develop -c bats tests/remote-host.bats` and
      `nix develop -c shellcheck adapters/core/dispatch.sh` → PASS.
- [ ] **Step 17: failing bats for the remote half**: a hand-built bundle
      (meta for `feat/7-x` at the pushed tip, a patch adding an untracked
      file and modifying a tracked one, a fake transcript, session uuid)
      passed via `DISPATCH_MIGRATE_BUNDLE` to `dispatch standard opus --host
mini 7 -- x`: remote clone gets `refs/heads/feat/7-x` at the tip;
      remote worktree's scratch tree equals the meta's tree; transcript at
      `$RHOME/.claude/projects/<encoded remote wt>/<uuid>.jsonl` mode 0600;
      remote `leads/feat/7-x` = `claude <uuid>`; the stub claude's argv has
      `--resume <uuid>` and no `--session-id`; remote header has
      `migrated_from:` and `resume: true`; `migrated.md` holds the handoff;
      local dispatch row has `remote:true`, `migrated_from`,
      `engine_context:"resumed"`. Variants: no transcript →
      `engine_context:"fresh"` with `--session-id`; pre-existing remote
      transcript → fresh, `engine_context_why` says so; tip mismatch →
      refused, nothing recorded locally; `DISPATCH_MIGRATE_BUNDLE` without
      `--host` refused; bundle outside `<crew_dir>/artifacts/` refused;
      `dispatch --migrate-check feat/7-x` on the remote prints
      `migrate-check: v1 ok`, and refuses a remote local branch that is
      checked out. Run `nix develop -c bats tests/dispatch-migrate.bats` →
      FAIL.
- [ ] **Step 18: implement the remote half and local forward** in
      dispatch.sh: env refusals; `_dispatch_remote` adds `--migrate-dir`;
      `--migrate-check` arm; `DISPATCH_MIGRATE_DIR` handling — fetch before
      the claim gate, branch/switch_mode checks, HEAD check + `git apply
--check`/apply/already-applied skip after the task doc, `migrated.md`,
      header `migrated_from:`, claude `--resume` launch with the migration
      prompt (fresh seams off; mirror `dispatch-resume.sh`'s claude
      continuation at ≈883–928 and its guards rather than inventing a second
      variant), fresh fallback with reasons, dispatch row
      fields `migrated_from`/`engine_context`/`engine_context_why`. Run
      step 17's command, `tests/remote-host.bats`, `tests/dispatch.bats`
      (help-line test) and shellcheck → PASS. (implement: escalated)

### Phase D — `dispatch migrate` (C5)

- [ ] **Step 19: failing end-to-end bats** in `tests/dispatch-migrate.bats`.
      Each starts a real local worker with `dispatch standard opus 7 -- x`
      (stub engine), then:
  1. cooperative: a background helper waits for the directive msg on the
     local bus, commits + pushes in the worktree with the fixed checkpoint
     message, writes `handoff.md`, posts `crew status <worker> migrated
--to-host mini` under the worker env; `dispatch migrate x --host mini
--wait 60` exits 0; local bus has the `source:"migrate"` row; local
     `remote:true` row on mini with the same branch; remote worktree at
     the tip; remote `migrated.md` contains the handoff; `og-remote-open`
     log shows `--window @crew_id=<id>`;
  2. cooperative with the helper leaving a tracked edit uncommitted → it
     arrives on the remote (scratch-tree sha equals the mark's tree);
  3. `--now` with a modified tracked file, a staged new file and an
     untracked file, nothing pushed before → branch pushed, remote
     scratch-tree sha equals the mark's tree;
  4. after 3, `dispatch standard opus --host chromebox 7 -- x` refused;
  5. `crew reap` keeps the local worktree while the remote row is absent:
     run `--now` with `SSH_FAIL_MIGRATE` set and
     `DISPATCH_HOST_RECOVER_TRIES=1 DISPATCH_HOST_RECOVER_SLEEP=0` — the
     preflights pass, the remote dispatch call exits 255, migrate exits
     non-zero with the retry/abandon message; assert the `source:"migrate"`
     row exists, then `crew reap` keeps the worktree and its keep reason
     names the missing remote row; unset the knob, re-run `dispatch migrate
x --host mini` (recovery path: no second stop/snapshot/mark — exactly
     one `source:"migrate"` row), then `crew reap` removes the worktree and
     `git ls-remote origin feat/7-x` still lists the branch;
  6. a pre-push hook that prints `HOOK SAYS NO` and exits 1 → `--now`
     exits 1, stderr contains `HOOK SAYS NO` verbatim, no `migrated` row,
     worktree and its changes intact, `dispatch resume x` hint printed;
  7. cooperative helper posts `blocked "migrate: push failed: hook said
no"` → exit 1, stderr has that detail verbatim, worker status still
     `blocked`;
  8. refusals: `CREW_WORKER_ID` set; `add_dir:` header (message names
     the dir); remote worker target; remote dispatch without
     `--migrate-check` (a stub `dispatch` in `$RBINS` that execs the real
     one except for `--migrate-check`, where it exits 2 like an unknown
     arm) → refused before any kill (window still alive); held migrate lock;
     cooperative timeout (`--wait 2`, helper silent) → exit 3 with the
     human-escalation message and the worker untouched.
     Run `nix develop -c bats tests/dispatch-migrate.bats` → FAIL.
- [ ] **Step 20: implement `adapters/core/dispatch-migrate.sh`** (C5 steps
      1–12, re-run/recovery), the `migrate` arm in dispatch.sh (exec like
      `resume`), `flake.nix` package + `dispatch` runtimeInputs only, `scripts/bats-affected.sh` mapping, `--help` for both.
      The cooperative wait uses one deadline: `crew nudge --wait <S>` runs
      in the background (its pid recorded, reaped by the EXIT trap and
      waited before exit), the status poll runs concurrently against the
      same deadline, and the status is read once more after the deadline
      before declaring exit 3, so a `migrated` posted while the nudge waits
      on a live turn is never missed.
      Run step 19's command, `nix develop -c shellcheck adapters/core/*.sh` and
      `nix build .#dispatch-migrate .#dispatch` → PASS. (implement:
      escalated)
- [ ] **Step 21: completions and help.** `dispatch --help` usage + "Other
      entry points" line for `migrate`; `completions/dispatch.fish`
      `migrate` subcommand with `--host`, `--now`, `--wait` (no standalone
      completion file; see the file list). Run `nix develop -c bats
tests/completions.bats tests/dispatch.bats` → PASS.

### Phase E — docs and gates

- [ ] **Step 22: protocol and skills.** `WORKER_PROTOCOL.md` "Migrate
      directive" (C8 steps 1–6, verbatim intent); `DISPATCHER_PROTOCOL.md`
      terminal states + one `dispatch migrate` line; `skills/remote-host/
SKILL.md` (when to migrate, cooperative vs `--now`, exit codes 0/1/2/3,
      retry/abandon, reap behaviour, 24 h residual); `skills/resume/
SKILL.md` (abandon path; resume of a migrated branch routes to the
      host). Then `nix develop -c ./scripts/gen-adapters.sh` and
      `git diff --stat` shows only generated mirrors of these edits; run
      `nix develop -c bats tests/module.bats` (protocol rev pin) and update
      the pin if the test says so. The remote-host skill also names what does
      not travel: crew-dir critic artifacts and the role panes (the remote
      grid starts fresh from `roles.json`). Owner memory says claude
      classifiers have blocked protocol-doc edits: a denial here is a
      permission block (block→await the dispatcher), never worked around.
- [ ] **Step 23: full gate.** Run every acceptance command below; fix at
      the source.

## Acceptance

- [ ] bats scenarios (cooperative e2e, `--now` intact, third host refused,
      reap after remote tip, push-hook verbatim) → `nix develop -c bats
tests/dispatch-migrate.bats` (steps 15, 17, 19).
- [ ] no attention item for `migrated` → `cd dash && go test
./internal/view/` (step 6).
- [ ] `nix develop -c shellcheck adapters/core/*.sh
adapters/core/reviewers/*.sh scripts/*.sh`.
- [ ] `nix develop -c sh -c 'cd crew && go vet ./... && golangci-lint run
./... && cd ../dash && go vet ./... && golangci-lint run ./...'`.
- [ ] `nix develop -c sh -c 'cd crew && CREW_DRIFT_REQUIRED=1 go test ./...'`.
- [ ] `nix develop -c ./scripts/gen-adapters.sh && git diff --exit-code`.
- [ ] `nix develop -c bash scripts/bats-shard.sh --check`.
- [ ] `nix develop -c bash scripts/harness-manifest.sh --check` and
      `nix develop -c bash scripts/harness-inventory.sh --check`.
- [ ] spec + plan committed under `docs/superpowers/`.
- [ ] targeted bats for touched scripts: `tests/remote-host.bats`,
      `tests/dispatch-notify.bats`, `tests/dispatch.bats`,
      `tests/completions.bats`, `tests/module.bats`; `nix flake check` before
      push (CI gate).
