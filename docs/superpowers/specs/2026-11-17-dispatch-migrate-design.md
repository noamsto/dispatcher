# `dispatch migrate`: move a running worker to another host — design

Closes #1599.

## Problem

A worker that is already running cannot move to another host. Today's manual
path (directive → commit/push → `failed "migrating"` → `dispatch --host H …`)
is refused by `_remote_dup_holder` (`adapters/core/dispatch.sh` ≈2577), because
the only release of a local holder is `_branch_finished` (≈386): newest
worker's last status `reaped` with `source:"reap"` **and** no local branch ref.
`crew reap` keeps a `failed` no-PR worker while its issue is OPEN, and
`--discard` needs the issue CLOSED, so an open issue has no supported path. The
`failed` status also pollutes attention and ratings.

## Goal

`dispatch migrate <target> --host <name> [--now] [--wait S]` pauses a local
running worker, carries its branch, uncommitted state, task settings and (for
claude) its session transcript to configured host `<name>`, releases the local
claim to that host only, and resumes the worker there under the same issue and
branch, mirrored locally like any `dispatch --host` worker.

**Out of scope:** moving the dispatcher session; cross-engine migration;
migrating a worker that is already remote (host→host or back home: refused,
named in the message); `kind: review` workers; stacked (`base:`) workers.

## Measured facts the design rests on

- **Claude session lookup (claude 2.1.325, measured in this worktree's
  scratchpad).** A session created with `--session-id <id>` in dir A is written
  to `~/.claude/projects/<A with every non-alphanumeric → '-'>/<id>.jsonl`.
  `claude -p --resume <id>` run from dir B found and continued it both
  _before_ the file was copied into B's project dir and after; lines appended
  from B carry `"cwd":"<B>"` while the older lines keep `<A>`. So a carried
  transcript resumes from a different worktree path; we still place it at the
  canonical key for the remote worktree
  (`${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects/<encoded remote wt>/<id>.jsonl`,
  the same encoding `crew/internal/reap/release.go` `transcriptDir` uses) so it
  does not depend on the cross-dir fallback.
- **Transcript sizes** (991 worktree sessions, last 30 days, this machine):
  p50 1.4 MB, p90 5.6 MB, p99 19.9 MB, max 49 MB. JSONL gzips roughly 8–10×.
- **`crew remote-exec`** caps a request at 1 MiB (`crew/internal/remote/exec.go`
  `MaxRequest`) and allows `dispatch …` with any args; the spec travels as a
  JSON field written to a 0600 scratch file (`DISPATCH_SPEC`).
- **Remote resume mode.** A remote `dispatch #N -- <title>` takes
  `switch_mode=resume` and the claim gate's resume exemption only when the
  remote clone has `refs/heads/<branch>` (≈4093, ≈4267); otherwise it creates
  a fresh branch off main or refuses "already claimed". The branch name is
  `feat/<N>-<slug(title)>` (slug ≈3953) or `<linear-id lowercased>-<slug>`.

## Components

### C1. `migrated` terminal state (crew)

- `crew status <id> migrated --to-host <h> [detail]`: new state; `--to-host`
  is required with `migrated` and refused with every other state; `<h>` must
  match the host-name regex `^[A-Za-z0-9][A-Za-z0-9._-]*$`. Body gains
  `to_host` (`omitempty`). Posted by the worker itself in cooperative mode.
  Terminal dedupe applies like `done`/`failed`; for `migrated` the dedupe key
  includes `to_host`.
- **Directive-gated.** A worker's `migrated --to-host H` is refused unless the
  newest dispatcher `migrate`/`migrate-cancel` directive msg to that session
  is `{"directive":"migrate","to_host":"H"}`. `{"directive":"migrate-cancel",
"to_host":"H"}` withdraws it; `dispatch migrate` sends it on a cooperative
  timeout (exit 3). `crew watch` does not wake on `migrated`.
- `bus.State` gains `StateMigrated`; `Terminal()` includes it.
- **Authoritative row.** The release evidence is a `migrated` status row with
  `source:"migrate"`, written only by `crew migrate mark` (C3) under the
  worker's newest session id, the way `crew reap` writes `reaped` +
  `source:"reap"`. `crew status` has no `--source`, so a worker can post
  `migrated` but never the releasing form.
- Every hand-copied state list that enumerates terminal states learns
  `migrated` (survey list in the plan): sessions.jq, reap folds, roster live /
  waiting states, rosterrender classify, peer snapshot states, fleet far-wire
  states and finished window, stall terminal checks, wants_reap.jq,
  `dispatch-notify.sh` silent list (all copies via gen-adapters),
  `launcher-common.sh` status format, `dispatcher.sh` counts,
  `phase-status.sh`.
- `clearRelaunch` clears the remux relaunch stamp for `migrated` too.
- **No penalty, no attention.** `crew rate` (`rate/records.jq`) and
  `crew retro` (`retro/draft.go`) skip a session whose last state is
  `migrated` (the run continues on the other host, rated there). The dash
  draws `migrated` as a terminal "moved" row: glyph `→`, detail
  `moved to <to_host>`, board group done, neutral colour; `stateItem` already
  returns no item for it, and a test pins that.
- `crew roster` forwards `to_host` on the row (roster_base.jq).

### C2. Claim release to the target host only

Shared rule, bash and Go: **a branch is released to host H** when the newest
worker (following `kind:"resume"` rows as `_branch_finished` does) has, as its
newest status row that is neither an `exited` SessionEnd backstop nor a
watchdog row (`source "watchdog"`), `state "migrated"`, `source "migrate"`,
`to_host H`. Ignoring those two kinds matters because killing the window
fires the SessionEnd hook and the orphaned `crew stall-watch` can still post a
`quiet:`/`dead:` row afterwards; neither is the worker's word (a Go and a
bats test pin both).

- `_branch_released_to <branch> <host> [--git-dir]` in dispatch.sh, next to
  `_branch_finished`, fail-closed like it.
- `_remote_dup_holder <issue> <target-host>`: a local row passes when
  `_branch_finished` **or** `_branch_released_to <branch> <target-host>`. A row
  released to H still blocks every other target host K (holder printed as
  `<branch> on host <local> (migrating to H)`). Once the remote resume
  completes, the newest dispatch row for the branch is the `remote:true`
  row on H, which the existing rule already treats as "passes H, blocks K for
  24 h". **Accepted residual:** after the local worktree is reaped, the
  third-host block rests on that 24 h freshness alone, exactly as for any
  `dispatch --host` worker today; a migrated worker running past 24 h stops
  blocking K. Requirement 4 holds while the local mark or a fresh remote row
  exists, not unconditionally.
- **Local re-dispatch guard.** A plain (no `--host`) `dispatch` of an issue
  whose branch exists locally takes the resume exemption today. It now refuses
  when the branch's newest dispatch row is `remote:true` on host H, or when
  the branch is released to some host H: `dispatch: <branch> was migrated to
host H — dispatch --host H … to resume it there, or dispatch resume <branch>
here to abandon an unfinished migration`. `dispatch resume <branch>` keeps
  working: for a `remote:true` newest row it already routes to H; for a
  released-but-unconfirmed migration it resumes locally, and its `working`
  status ends the release (the abandon path).

### C3. `crew migrate` (Go, `crew/internal/migrate`)

Dispatcher-only (`sender.Check("dispatcher:<crew>")`, refused with
`CREW_WORKER_ID`/`CREW_ROLE_ID` set), run from the primary checkout
(`wtgit.TrustedCwd`). Branch resolved to its worktree through the anchored
worktree record, never a bus path.

- `crew migrate stop --crew C <branch> [--timeout S]` — kills the worker's
  window(s) (reap's stamp-anchored `killWindows`/`killLeftovers`, never a
  name substitution) and waits until no engine process runs at the worktree
  (reap's `idleEngines` sampling), up to S (default 30). Exit 1 naming the
  live process when it does not stop.
- `crew migrate snapshot --crew C <branch>` — reuses reap `--discard`'s patch
  machinery, refactored into an exported function rather than copied:
  `scratchTree` (private index, `read-tree` + `add -A`: tracked, staged and
  untracked, plus a forced add of root scaffold the repo gitignores —
  `SPEC.md`, `PLAN.md`, `DECOMPOSITION.md`, `REVIEW_NOTES.md`,
  `PLAN_ROUND<n>.md`, never `WORKER_TASK.md` — so a mid-run worker keeps
  them), `hasGitlink` (embedded-repo refusal), `writeDiff`
  (`--binary --full-index`), with a fresh symlink-guarded bundle dir in place
  of the artifacts dir. Same safety refusals as `--discard`: live engine at the worktree, cwd
  inside it, no admin dir, gitlink mismatch, `config.worktree`, submodules,
  git-config drift (`gitguard`). Never removes or modifies anything. Always
  creates the bundle dir `<crew_dir>/migrate/<branch>/<UTC ts>` (0700, fresh:
  refused when it exists; never under the worker-writable `artifacts/`),
  writes `<dir>/patch` when non-empty and prints one JSON line
  `{"dir":…,"patch":<path|"">,"tree":<sha>,"tip":<sha>}`.
- `crew migrate mark --crew C <branch> --to-host H --mode coop|now --tip T
--tree S --dir D [--patch P]` — D is the bundle dir snapshot printed
  (a real directory, no symlink on its path) and the row records it as `dir`;
  appends the authoritative `migrated` row (C1) as the
  branch's newest worker session: `{state:"migrated", to_host, source:
"migrate", mode, tip, tree, patch, detail:"moved to H"}`.

### C4. Transport: `migrate` payload in `crew remote-run` / `remote-exec`

- Client: `crew remote-run … --migrate-dir <dir> -- dispatch …` reads
  `<dir>/meta.json` (required), `<dir>/patch` and `<dir>/transcript.jsonl`
  (optional), gzips + base64s the two files and sends request key
  `migrate: {meta:{…}, patch_gz?:…, transcript_gz?:…}`.
- Server: `migrate` is allowed only with argv `dispatch <tier> …` (not
  `--engines`, `resume`, or a `crew …` argv). A request carrying `migrate`
  may be up to 32 MiB; every other request keeps the 1 MiB cap and message.
  Each blob decompresses through a 128 MiB bounded reader. `meta` is strict:
  `v:1`, `branch` (ref-format-safe, no `..`, no leading `-`), `tip`
  (`^[0-9a-f]{40}([0-9a-f]{24})?$`), `from_host` (host regex), `mode`
  (`coop|now`), optional `session` (UUID regex) present only with a
  transcript, `handoff` (≤ 64 KiB, no NUL). Files land 0600 in the scratch
  dir `migrate/`, exported to the child as `DISPATCH_MIGRATE_DIR`.
- The local side of `dispatch` (`_dispatch_remote`) forwards
  `--migrate-dir "$DISPATCH_MIGRATE_BUNDLE"` when that env is set. The bundle
  must realpath to `<crew_dir>/migrate/<branch>/<UTC ts>` (never under the
  worker-writable `artifacts/`); `DISPATCH_MIGRATE_BUNDLE` without `--host` is
  refused, and
  `DISPATCH_MIGRATE_DIR` with `--host` is refused.
- Transcript budget: when the gzipped+base64 transcript would push the
  request over 30 MiB, it is dropped and the launch goes fresh (recorded,
  C6).

### C5. `dispatch migrate` (local orchestration, `adapters/core/dispatch-migrate.sh`)

Entry: `dispatch migrate …` arm in dispatch.sh before the tier parse, `exec`s
`dispatch-migrate` like `resume`. Steps:

1. **Lock.** `ln -s $$ <crew_dir>/migrate-<cksum(branch)>.lock` (the
   dispatch lock's atomic-create idiom, cleared by an EXIT/INT/TERM trap; a
   live holder → exit 2 naming its pid, a dead one → exit 2 naming the file
   to remove). Two concurrent migrations of one branch can never both reach
   the mark.
2. **Refusals before anything stops** (exit 2, nothing written):
   `CREW_WORKER_ID`/`CREW_ROLE_ID` set or a `@crew_branch`-stamped pane
   (`_refuse_chrome_from_worker` style); `_wt_trusted_cwd`; `--host` missing,
   unknown, or without a repo mapping (step 3 surfaces it); target resolved
   with `crew resolve-target` (branch, codename, `#N`) — a `remote:true` row is
   refused ("already on host X"); no local worktree record; task doc
   `kind: review`, `base:`, `add_dir:` ("--add-dir grants do not exist on host
   H: <dirs>"), `lazy: 1`, `code_mode:`, `profile:` (lane profile), or an
   `## Owner authorization` section (not re-assertable by the dispatcher);
   the branch the remote would derive from `title:` + the `Closes` line
   differs from the worker's branch; newest status `done` or `reaped`.
   Cooperative mode additionally needs a live engine pane and a non-terminal
   status.
3. **Preflight, before the worker is touched:**
   - `crew remote-run --host H --env CREW_ID=<c> -- dispatch --engines` lists
     the lead engine (the same call `dispatch --host` makes; unknown host, no
     mapping, no clone and ssh failure surface here);
   - `crew remote-run --host H --env CREW_ID=<c> -- dispatch --migrate-check
<branch>` — a new read-only arm on the remote that prints
     `migrate-check: v1 ok` after checking that the remote clone has no
     local `<branch>` ref that is checked out in a worktree or not an
     ancestor of `origin/<branch>` once fetched (it runs
     `git fetch origin refs/heads/<b>` into `FETCH_HEAD` only when origin has
     the branch; no branch ref is written; only `FETCH_HEAD` moves). A remote whose `dispatch` predates this
     feature fails the unknown arm, so migrate refuses before stopping
     anything: `host H runs a dispatch without migrate support — update it
first`.
4. **Quiesce.**
   - _Cooperative (default):_ `crew reply <worker> <directive JSON>`
     (`{"directive":"migrate","to_host":"H"}`) goes with `--no-nudge`,
     because migrate runs its own budgeted `crew nudge <target> --wait <S>`
     (a reply's own wait defaults to 1800 s and only one wait per lead
     joins); that nudge is best effort, its result is logged, then poll the
     worker's status until S (default 1800, `--wait`) elapses. Outcomes: - worker posts `migrated` → go on (its detail may say uncommitted work
     was left for the patch, C8); - worker posts `blocked` whose detail starts `migrate:` (its push failed)
     → print that detail verbatim, exit 1, worker untouched and live; - worker reaches `done`/`failed`/`exited` → exit 1 naming it; - timeout → send `{"directive":"migrate-cancel","to_host":"H"}` (C1) and
     exit 3: `worker did not quiesce within Ns — ask the human;
dispatch migrate --now only if they are leaving now`. Never falls
     through to `--now`.
   - _Forced (`--now`):_ no directive.
5. `crew migrate stop` (both modes: a cooperative worker's engine is idle but
   still running).
6. `crew migrate snapshot` → patch, tree, tip.
7. **Push.** `--now`: `git --git-dir <admin dir> -C <wt> push origin <branch>`
   (anchored on the repo's admin dir, not the worker's gitlink; plain; the
   hooks run on purpose, an owner requirement; never `--no-verify`, never
   force); on failure print git's output verbatim,
   print `the worker is stopped; nothing was marked — resume it here with:
dispatch resume <branch>` and exit 1. Cooperative: the worker pushed; the
   dispatcher only verifies. Both: after the push, `refs/remotes/origin/<b>`
   must equal the local tip, else exit 1 the same way. A tip that was never
   pushed and has no new commits (fresh worker, nothing committed) pushes the
   branch as is. The dispatcher never commits.
8. **Transcript (claude only).** The lead record `<crew_dir>/leads/<branch>`
   names `claude <uuid>`; the file is
   `${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects/<encoded wt>/<uuid>.jsonl`.
   Missing record, `-`, other engine, missing file or over budget → no
   transcript, reason recorded.
9. **Bundle**, the dir snapshot made (`<crew_dir>/migrate/<branch>/<UTC ts>`, C3): `meta.json`
   (C4 fields), `patch`, `transcript.jsonl`, and the `handoff` text. The
   handoff is crew-dir data that travels only over ssh, never to GitHub:
   - the dispatcher's part: from host, mode, time, tip, whether a patch
     travels, the worker's last status and detail, its last unanswered
     `blocked` question if any, and the msgs addressed to its old worker id
     that it never pulled (`crew inbox <old id> --undelivered`, which marks
     them delivered for that old session only — harmless, it never runs again; an abandon resumes as a new session) — so open questions and unread verdicts
     reach the new session instead of staying on the local bus;
   - the worker's part (cooperative): the worker's own
     `<crew_dir>/artifacts/<branch>/handoff.md` (C8), when present, appended
     verbatim.
10. **Size precheck**, then `crew migrate mark … --dir <bundle dir>` — a
    payload over the transport limit exits 1 before the mark, the worker
    stopped but not released. The branch is then released to H.
11. **Remote resume** through the existing remote path:
    `DISPATCH_SPEC=<task body> DISPATCH_SKIP_SPEC_LINT=1
    DISPATCH_MIGRATE_BUNDLE=<bundle> [DISPATCH_DRAFT_PR=1] dispatch <tier>
    <model> --host H --agent <engine> --effort <effort> --plan <plan> [--mcp
    <mcp>] [--roles <spec>] [--chrome] [--also-closes N …] <#N|LIN-ID> --
    <title>`, all values from the old header. `--roles` carries each role's
    recorded spec from `<crew_dir>/artifacts/<branch>/roles.json` as
    `name=agent:model@effort` (the header's `roles:` holds names only); with
    no `roles.json` it passes the names and the remote uses its default role
    engines. `_dispatch_remote` keeps every guard (dup holder, budgets,
    identity avoid, link-loss recovery, row validation, `_remote_finish`:
    local `remote:true` row + claims index + `og-remote-open …
    --window @crew_id=<id>` mirror). The task body is everything after the
    doc's `## Task` line, verbatim.
12. **Output:** `dispatch migrate: <branch> → host H (engine context:
resumed|fresh: <reason>; patch: <path>|none)`.

**Re-run / recovery.** Newest dispatch row `remote:true` on H → print "already
migrated to H", exit 0. Branch released to H but not yet remote (step 11
failed) → skip steps 4–10, verify the worktree's scratch tree and tip still
equal the mark's `tree`/`tip` (else refuse), rebuild the bundle from the
mark's recorded `dir` (patch and transcript), retry step 11. Released to H, asked for K →
refuse naming H. A failed step 11 exits 1: `migration to H failed (exit rc); the
worker is stopped and marked migrating to H — retry: dispatch migrate <b>
--host H; abandon: dispatch resume <b>`.

### C6. Remote half (`dispatch` with `DISPATCH_MIGRATE_DIR`)

The remote runs its own `dispatch`, unchanged except (`--migrate-check` runs
the same config guard first):

1. **Before the claim gate**, with `meta.branch`: `git fetch origin
refs/heads/<b>:refs/heads/<b>` (non-force; refuse a diverged or
   checked-out local ref with git's message; a retry after the remote
   worktree exists is accepted when the branch is checked out in a
   dispatcher-recorded worktree at the tip), then require
   `refs/heads/<b>` = `meta.tip`. Load-bearing: it puts the run on
   `switch_mode=resume` and the claim gate's resume exemption.
2. After branch derivation: the derived branch must equal `meta.branch`, and
   `switch_mode` must be `resume`; else refuse.
3. **After the worktree and task doc exist, before the launch:** HEAD must be
   `meta.tip`; apply the patch with `git apply --binary` (all-or-nothing;
   `git apply --check` first; a tree where `--reverse --check` passes is
   already applied and skipped, for retries). Write `meta.handoff` to
   `<remote crew_dir>/artifacts/<branch>/migrated.md`. Header gains
   `migrated_from: <from_host>`.
4. **Launch.** claude with a carried transcript: write it 0600 to the
   canonical project path for the remote worktree (an existing file there →
   do not overwrite, launch fresh, reason `transcript already present`);
   `lead_sid=<meta.session>`, `--resume <sid>` in place of `--session-id`,
   `_record_lead_session claude <sid>` (so a later remote `dispatch resume`
   resumes the same session); prompt: the migration note (moved from host X,
   new worktree path, re-read WORKER_TASK.md for the new `worker_id`, read
   `migrated.md`, uncommitted work was applied from the patch, continue where
   you stopped). Fresh seams are off for a migrated launch. Otherwise (fresh):
   the normal resume prompt plus the migration note. Every engine: the
   dispatch row carries `migrated_from` and `engine_context`
   (`resumed`|`fresh`) and, for fresh, `engine_context_why`; it reaches the
   local bus through `dispatch_row:` → `_remote_finish`.

### C7. Reap of the migrated local worktree

`crew reap` takes a session whose last state is `migrated` (`source
"migrate"`) as a candidate and removes its worktree with the existing anchored
removal only when **all** hold:

- the branch's newest dispatch row is `remote:true` on the mark's `to_host`
  (the remote resume was confirmed);
- no engine is live at the worktree; cwd not inside it;
- `refs/heads/<b>` equals the mark's `tip`, and that tip is an ancestor of
  `refs/remotes/origin/<b>` (the remote worker may have pushed past it and a
  local fetch moved the tracking ref);
- the tree is clean beyond scaffold, or its scratch tree equals the mark's
  `tree` (nothing beyond the travelling patch);
- the usual tree gates (admin dir, gitlink, `config.worktree`, submodules,
  git-config drift).

Otherwise it keeps the worktree with a reason. No PR/issue gate applies. It
deletes the local branch ref but **never** the origin branch (the remote
worker owns it), then posts `reaped` as today. The mark's patch stays in the
bundle dir (`<crew_dir>/migrate/<branch>/<ts>`), not artifacts.

### C8. Worker half and docs

- `WORKER_PROTOCOL.md`: a "Migrate directive" paragraph. On a dispatcher msg
  `{"directive":"migrate","to_host":"H"}` at any peek seam:
  1. start no new stage or subagent; stop background processes you started;
  2. write the handoff note to `<crew_dir>/artifacts/<branch>/handoff.md`
     (stage, seen-cursor, counters, next step, retro notes, open questions —
     the "Fresh seam relaunch" note's fields). It is crew-dir data: it
     travels to H over ssh in the dispatcher's bundle and never reaches
     GitHub;
  3. commit the work that belongs in the branch (never root scaffolding) with
     the fixed message `wip: checkpoint before host migration` and **no
     body** — commit messages reach GitHub and can fold into a squash, so
     they carry nothing from the handoff (no paths, ids, host names);
  4. **a commit hook refuses the commit** → no fix attempt, no bypass: leave
     that work uncommitted (the dispatcher's snapshot patch carries it) and
     go on with what is already committed;
  5. push plainly through the hooks (never `--no-verify`, never force);
     push failure → `crew status "$CREW_WORKER_ID" blocked "migrate: push
failed: <git's words>"` and block→await (C5 step 4 surfaces it
     verbatim);
  6. success → `crew status "$CREW_WORKER_ID" migrated --to-host H "<one
line; say 'uncommitted work travels as a patch' when step 4 fired>"`, no
     metrics snapshot (the run continues on H), stop. A later
     `{"directive":"migrate-cancel"}` withdraws the directive: a worker that
     has not posted `migrated` carries on (re-stamps `working`), and
     `migrated` is refused after a cancel (C1).
- `adapters/core/skills/remote-host/SKILL.md` and `resume/SKILL.md`: when and
  how to migrate, the exit codes, recovery and abandon paths.
- `DISPATCHER_PROTOCOL.md` terminal-state sentences learn `migrated`;
  `dispatch --help`, `completions/dispatch.fish`, `dispatch-migrate --help`.
- Generated copies via `scripts/gen-adapters.sh`.

## Trust boundary

- Host ssh target and repo path only from `hosts.<name>` settings (`crew
remote-run`), never a bus row; `to_host` from the bus only selects a name
  that is re-resolved through settings and regex-checked.
- No caller data in a remote shell line (still the literal `crew
remote-exec`); the payload is JSON fields with strict validation and size
  bounds; files 0600 in the private scratch dir.
- Sender identity: `crew migrate *` and the routed calls run the dispatcher
  sender check; a worker cannot write the releasing `migrated` row
  (`source:"migrate"` is not reachable through `crew status`).
- Git-config baseline: snapshot and reap reuse `gitguard`; the remote
  dispatch runs its own guards unchanged; worktree records anchor every path.
- Public-leak guard: `dispatch migrate` posts nothing through `gh`. What
  reaches GitHub is only the worker's own commits, pushed through its hooks
  and never bypassed. The cooperative checkpoint commit has a fixed, bodiless
  message (C8), so no handoff text, path, session id or host name reaches a
  commit; the handoff, patch and transcript are crew-dir data that travel only
  over ssh to a configured host of the same owner.
- Transcript safety: same owner, same uid on both hosts, ssh with forwarding
  off; written 0600; never overwrites an existing remote transcript; never
  printed.

## Testing

- **bats, fake ssh host** (`tests/remote-host.bats` harness; setup moved to a
  shared helper loaded by a new `tests/dispatch-migrate.bats`):
  cooperative end-to-end (local `migrated` row with `source:"migrate"`, claim
  released, remote dispatch row on the same branch and issue with
  `migrated_from`, remote worktree at the tip, the worker's `handoff.md`
  in the remote `migrated.md`); cooperative with work the worker left
  uncommitted (commit hook refused) travelling as the patch; `--now` with
  tracked, staged and untracked changes arriving intact on the remote —
  "intact" means the remote worktree's scratch tree (`read-tree <tip>` +
  `add -A` in a private index) equals the mark's `tree` sha, since applying a
  patch collapses staged vs unstaged; third-host dispatch of the issue
  refused; a watchdog/`exited` row after the mark does not end the release;
  reap keeps the worktree until the remote row exists and the tip is pushed,
  then removes it; a failing pre-push hook's message surfaces verbatim,
  nothing marked, worktree intact and resumable; a worker's `blocked
"migrate: push failed: …"` surfacing verbatim (exit 1); refusals (worker
  env, `add_dir:`, remote worker, a remote without `--migrate-check`, a held
  migrate lock, cooperative timeout exit 3).
- **Go:** status `migrated`/`--to-host` validation; migrate stop/snapshot/mark
  (incl. refusals and the sender check); remote-exec `migrate` key (allowlist,
  size caps, bounded decompress, meta validation, files written); reap
  migrated candidate gates; rate/retro skip; roster `to_host`; dash: no
  attention item for `migrated` in overview and fleet, glyph/group.

## Acceptance mapping

| acceptance item                                                                           | evidence                                                              |
| ----------------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| bats coverage (5 scenarios)                                                               | `tests/dispatch-migrate.bats`                                         |
| no attention item for `migrated`                                                          | dash `attention_test.go` case                                         |
| shellcheck, go vet, golangci-lint, go test, gen-adapters diff, bats-shard, harness checks | the task's commands, run before push                                  |
| spec + plan committed                                                                     | this file and `docs/superpowers/plans/2026-11-17-dispatch-migrate.md` |
