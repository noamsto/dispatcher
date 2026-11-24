bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# dispatch migrate against the fake-ssh-host harness: tests/remote-host-helpers.bash.

load remote-host-helpers

setup() { remote_host_setup; }
teardown() {
  [ -z "${COOP_PID:-}" ] || kill "$COOP_PID" 2>/dev/null || true
  # The local worker's tmux server: the private TMUX_TMPDIR tests/helpers.bash set.
  "$REAL_TMUX" kill-server 2>/dev/null || true
  remote_host_teardown
}

BRANCH=feat/7-x

# seed_local_worker — feat/7-x exists here, dispatched on host laptop as w-first
# and resumed as w-local, the session crew migrate mark posts under.
seed_local_worker() {
  git -C "$TEST_REPO" branch "$BRANCH"
  mkdir -p "${LBUS%/*}"
  local now
  now="$(($(date +%s) * 1000))"
  jq -nc --arg b "$BRANCH" --argjson ts "$now" \
    '{ts: $ts, crew_id: "c1", kind: "dispatch", branch: $b, worker_id: "w-first", host: "laptop"}' >>"$LBUS"
  jq -nc --arg b "$BRANCH" --argjson ts "$((now + 1))" \
    '{ts: $ts, crew_id: "c1", kind: "resume", branch: $b, worker_id: "w-local", prev_worker_id: "w-first"}' >>"$LBUS"
}

mark_migrated_to() { # <host>
  local dir="${LBUS%/*}/migrate/$BRANCH/20261117T000000Z"
  mkdir -p "$dir"
  (cd "$TEST_REPO" && crew migrate mark --crew c1 "$BRANCH" --to-host "$1" --mode coop \
    --tip "$(git rev-parse HEAD)" --tree "$(git rev-parse 'HEAD^{tree}')" --dir "$dir")
}

worker_status() { # <body json>
  jq -nc --argjson body "$1" '{ts: (now * 1000 | floor), crew_id: "c1", kind: "status", from: "w-local", to: "dispatcher:c1", body: $body}' >>"$LBUS"
}

refused_as_held_by() { # <holder text>
  [ "$status" -eq 1 ]
  [[ $stderr == *"issue #7 is already held by $1 — "* ]]
}

@test "a migrate mark releases the branch to its target host: --host mini passes the dup guard" {
  seed_local_worker
  mark_migrated_to mini
  [ "$(jq -c 'select(.kind == "status" and .body.source == "migrate") | [.from, .body.state, .body.to_host]' "$LBUS")" = '["w-local","migrated","mini"]' ]

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 7 x
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  jq -e --arg b "$BRANCH" 'select(.kind == "dispatch" and .remote == true) | .host == "mini" and .branch == $b' "$LBUS"
}

@test "a branch released to mini still blocks every other host" {
  seed_local_worker
  mark_migrated_to mini

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host chromebox 7 x
  refused_as_held_by "$BRANCH on host laptop (migrating to mini)"
}

@test "an exited backstop and a watchdog row after the mark keep the release" {
  seed_local_worker
  mark_migrated_to mini
  worker_status '{"state": "exited", "detail": "SessionEnd"}'
  worker_status '{"state": "blocked", "source": "watchdog", "detail": "quiet: no events for 10m"}'

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host chromebox 7 x
  refused_as_held_by "$BRANCH on host laptop (migrating to mini)"
}

@test "a working row from the worker after the mark ends the release" {
  seed_local_worker
  mark_migrated_to mini
  worker_status '{"state": "working", "detail": "resumed here"}'

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 7 x
  refused_as_held_by "$BRANCH on host laptop"
  [[ $stderr != *migrating* ]]
}

@test "a worker-posted migrated row without source migrate does not release" {
  seed_local_worker
  worker_status '{"state": "migrated", "to_host": "mini", "detail": "moving to mini"}'

  TMUX_PANE=%0 run --separate-stderr dispatch_remote --host mini 7 x
  refused_as_held_by "$BRANCH on host laptop"
  [[ $stderr != *migrating* ]]
}

@test "a plain local dispatch of a branch released to mini is refused before any side effect" {
  seed_local_worker
  mark_migrated_to mini
  before="$(wc -l <"$LBUS")"

  TMUX_PANE=%0 run --separate-stderr dispatch standard sonnet --effort medium --crew-id c1 7 x
  [ "$status" -eq 1 ]
  [[ $stderr == *"dispatch: $BRANCH was migrated to host mini — dispatch --host mini … to resume it there, or dispatch resume $BRANCH here to abandon an unfinished migration"* ]]
  [ "$(wc -l <"$LBUS")" -eq "$before" ]
  [ ! -s "$CLAIMS" ]
}

@test "a plain local dispatch of a branch whose newest dispatch row is remote on mini is refused" {
  seed_local_worker
  jq -nc --arg b "$BRANCH" '{ts: (now * 1000 | floor), crew_id: "c1", kind: "dispatch", branch: $b, worker_id: "w-remote", host: "mini", remote: true}' >>"$LBUS"

  TMUX_PANE=%0 run --separate-stderr dispatch standard sonnet --effort medium --crew-id c1 7 x
  [ "$status" -eq 1 ]
  [[ $stderr == *"dispatch: $BRANCH runs on host mini — dispatch --host mini … to resume it there"* ]]
}

@test "a remote dispatch row older than a day no longer blocks a plain local dispatch" {
  seed_local_worker
  jq -nc --arg b "$BRANCH" --argjson ts "$((($(date +%s) - 25 * 3600) * 1000))" \
    '{ts: $ts, crew_id: "c1", kind: "dispatch", branch: $b, worker_id: "w-remote", host: "mini", remote: true}' >>"$LBUS"

  run --separate-stderr dispatch standard sonnet --effort medium --crew-id c1 7 x
  [[ $stderr != *"runs on host"* ]]
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
}

@test "a plain local dispatch of a Linear branch released to mini is refused before any side effect" {
  BRANCH=eng-7-x
  seed_local_worker
  mark_migrated_to mini
  before="$(wc -l <"$LBUS")"

  TMUX_PANE=%0 run --separate-stderr dispatch standard sonnet --effort medium --crew-id c1 ENG-7 x
  [ "$status" -eq 1 ]
  [[ $stderr == *"dispatch: $BRANCH was migrated to host mini — dispatch --host mini …"* ]]
  [ "$(wc -l <"$LBUS")" -eq "$before" ]
}

# --- the remote half: a hand-built bundle carried by DISPATCH_MIGRATE_BUNDLE ---

SID=0b5c2d3e-1f2a-4b3c-8d4e-5f6a7b8c9d0e

# seed_pushed_branch — feat/7-x one commit past main on origin (TIP), checked out
# in a local worktree holding a tracked edit and an untracked file. TREE is that
# worktree's scratch tree, PATCH the diff from TIP to it.
seed_pushed_branch() {
  LWT="$BATS_TEST_TMPDIR/lwt"
  git -C "$TEST_REPO" worktree add -q -b "$BRANCH" "$LWT" main
  printf 'one\n' >"$LWT/tracked.txt"
  git -C "$LWT" add tracked.txt
  git -C "$LWT" commit -q -m tracked
  git -C "$LWT" push -q origin "$BRANCH"
  TIP="$(git -C "$LWT" rev-parse HEAD)"
  printf 'two\n' >>"$LWT/tracked.txt"
  printf 'new\n' >"$LWT/untracked.txt"
  TREE="$(scratch_tree "$LWT" "$TIP")"
  PATCH="$BATS_TEST_TMPDIR/patch"
  git -C "$LWT" diff --binary "$TIP" "$TREE" >"$PATCH"
  [ -s "$PATCH" ]
}

scratch_tree() { # <worktree> <tip> [ignored path...] — read-tree <tip> + add -A (+ add -f of the ignored paths) in a private index
  local idx wt="$1" tip="$2"
  shift 2
  idx="$(mktemp -u "$BATS_TEST_TMPDIR/idx.XXXXXX")"
  (cd "$wt" && GIT_INDEX_FILE="$idx" git read-tree "$tip" && GIT_INDEX_FILE="$idx" git add -A &&
    { [ $# -eq 0 ] || GIT_INDEX_FILE="$idx" git add -f -- "$@"; } && GIT_INDEX_FILE="$idx" git write-tree)
}

# make_bundle [--no-transcript] [--tip <oid>] — BUNDLE under the local crew dir's
# migrate tree, as dispatch migrate builds it.
make_bundle() {
  local transcript=1 tip="$TIP"
  while [ $# -gt 0 ]; do
    case "$1" in
    --no-transcript) transcript="" ;;
    --tip)
      tip="$2"
      shift
      ;;
    esac
    shift
  done
  BUNDLE="${LBUS%/*}/migrate/$BRANCH/20261117T000000Z"
  mkdir -p "$BUNDLE"
  cp "$PATCH" "$BUNDLE/patch"
  if [ -n "$transcript" ]; then
    printf '{"type":"fake-transcript"}\n' >"$BUNDLE/transcript.jsonl"
    jq -nc --arg b "$BRANCH" --arg t "$tip" --arg s "$SID" \
      '{v: 1, branch: $b, tip: $t, from_host: "laptop", mode: "now", session: $s, handoff: "handoff from laptop"}' >"$BUNDLE/meta.json"
  else
    jq -nc --arg b "$BRANCH" --arg t "$tip" \
      '{v: 1, branch: $b, tip: $t, from_host: "laptop", mode: "now", handoff: "handoff from laptop"}' >"$BUNDLE/meta.json"
  fi
}

remote_crew_dir() { printf '%s\n' "$REMOTE_CLONE/.git/crew"; }

# remote_transcript <worktree> — where claude keeps <worktree>'s session $SID.
remote_transcript() { printf '%s\n' "$RHOME/.claude/projects/${1//[^A-Za-z0-9]/-}/$SID.jsonl"; }

# lead_launch — the remote lead's launch script, the command its pane runs.
lead_launch() { cat "$(remote_crew_dir)"/launch/launch.*; }

migrate_dispatch() { # dispatch_remote --host mini 7 x carrying $BUNDLE
  TMUX_PANE=%0 DISPATCH_MIGRATE_BUNDLE="$BUNDLE" run --separate-stderr dispatch_remote --host mini 7 x
}

@test "a migrated worker's symlinked artifacts parent is refused and its target left empty" {
  seed_pushed_branch
  make_bundle
  victim="$BATS_TEST_TMPDIR/victim"
  mkdir -p "$victim" "$(remote_crew_dir)/artifacts"
  ln -s "$victim" "$(remote_crew_dir)/artifacts/feat"

  migrate_dispatch
  [ "$status" -eq 1 ]
  [[ $stderr == *"is a symlink or not a directory — refusing to write migrated.md"* ]]
  [ -z "$(ls -A "$victim")" ]
}

@test "a migrated claude worker lands on the remote at the tip with the patch applied and its transcript resumed" {
  seed_pushed_branch
  make_bundle

  migrate_dispatch
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(git -C "$REMOTE_CLONE" rev-parse "refs/heads/$BRANCH")" = "$TIP" ]
  wt="$(remote_wt "$BRANCH")"
  [ -d "$wt" ]
  [ "$(scratch_tree "$wt" "$TIP")" = "$TREE" ]

  transcript="$(remote_transcript "$wt")"
  [ "$(cat "$transcript")" = '{"type":"fake-transcript"}' ]
  [ "$(stat -c %a "$transcript")" = 600 ]
  [ "$(cat "$(remote_crew_dir)/leads/$BRANCH")" = "claude $SID" ]
  lead_launch | grep -qF -- " --resume $SID "
  run ! grep -qF -- --session-id <<<"$(lead_launch)"
  lead_launch | grep -qF 'This worker moved from host laptop to this host'

  grep -qx 'migrated_from: laptop' "$wt/WORKER_TASK.md"
  grep -qx 'resume: true' "$wt/WORKER_TASK.md"
  [ "$(cat "$(remote_crew_dir)/artifacts/$BRANCH/migrated.md")" = "handoff from laptop" ]

  row="$(local_dispatch_rows)"
  [ "$(wc -l <<<"$row")" -eq 1 ]
  jq -e --arg b "$BRANCH" '.remote == true and .host == "mini" and .branch == $b and .migrated_from == "laptop"
    and .engine_context == "resumed" and (has("engine_context_why") | not) and .engine_session == "'"$SID"'"' <<<"$row"
}

@test "a migrated worker with no transcript launches fresh with a session id and says why" {
  seed_pushed_branch
  make_bundle --no-transcript

  migrate_dispatch
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  wt="$(remote_wt "$BRANCH")"
  [ "$(scratch_tree "$wt" "$TIP")" = "$TREE" ]
  run ! grep -qF -- --resume <<<"$(lead_launch)"
  lead_launch | grep -qF -- ' --session-id '
  lead_launch | grep -qF 'Read WORKER_TASK.md and run it end-to-end.'
  lead_launch | grep -qF 'This worker moved from host laptop to this host'
  jq -e '.migrated_from == "laptop" and .engine_context == "fresh" and .engine_context_why == "no transcript travelled"' <<<"$(local_dispatch_rows)"
}

@test "a transcript already present on the remote is never overwritten: the launch goes fresh" {
  seed_pushed_branch
  make_bundle
  transcript="$(remote_transcript "$REMOTE_CLONE/.worktrees/${BRANCH//\//-}")"
  mkdir -p "${transcript%/*}"
  printf 'theirs\n' >"$transcript"

  migrate_dispatch
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(remote_wt "$BRANCH")" = "$REMOTE_CLONE/.worktrees/${BRANCH//\//-}" ]
  [ "$(cat "$transcript")" = theirs ]
  run ! grep -qF -- --resume <<<"$(lead_launch)"
  run ! grep -qF -- "$SID" <<<"$(lead_launch)"
  jq -e '.engine_context == "fresh" and .engine_context_why == "transcript already present"' <<<"$(local_dispatch_rows)"
}

@test "a bundle whose tip is not origin's branch tip is refused on the remote with nothing recorded" {
  seed_pushed_branch
  make_bundle --tip "$(git -C "$TEST_REPO" rev-parse main)"

  migrate_dispatch
  [ "$status" -ne 0 ]
  [[ $stderr == *"not the migrated tip"* ]]
  assert_nothing_recorded_locally
  [ -z "$(remote_wt "$BRANCH")" ]
}

@test "DISPATCH_MIGRATE_BUNDLE without --host is refused" {
  seed_pushed_branch
  make_bundle

  TMUX_PANE=%0 DISPATCH_MIGRATE_BUNDLE="$BUNDLE" run --separate-stderr dispatch standard sonnet --effort medium --crew-id c1 7 x
  [ "$status" -eq 1 ]
  [[ $stderr == *"DISPATCH_MIGRATE_BUNDLE needs --host"* ]]
  [ ! -s "$CLAIMS" ]
}

@test "DISPATCH_MIGRATE_DIR with --host is refused" {
  seed_pushed_branch
  make_bundle

  TMUX_PANE=%0 DISPATCH_MIGRATE_DIR="$BUNDLE" run --separate-stderr dispatch_remote --host mini 7 x
  [ "$status" -eq 1 ]
  [[ $stderr == *"DISPATCH_MIGRATE_DIR"*"--host"* ]]
  assert_nothing_recorded_locally
}

@test "a bundle outside the crew dir's migrate tree, the worker-writable artifacts included, is refused before the remote is called" {
  seed_pushed_branch
  make_bundle
  built="$BUNDLE"
  for outside in "$BATS_TEST_TMPDIR/20261117T000000Z" "${LBUS%/*}/artifacts/$BRANCH/migrate-20261117T000000Z"; do
    mkdir -p "${outside%/*}"
    cp -r "$built" "$outside"
    BUNDLE="$outside"

    migrate_dispatch
    [ "$status" -eq 1 ]
    [[ $stderr == *"DISPATCH_MIGRATE_BUNDLE"*"/migrate/"* ]]
    [ ! -s "$SSH_CMDS" ]
    assert_nothing_recorded_locally
  done
}

@test "dispatch --migrate-check passes a clone that can take the branch, and refuses one that has it checked out" {
  seed_pushed_branch

  run --separate-stderr on_remote "$REMOTE_CLONE" dispatch --migrate-check "$BRANCH"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$output" = "migrate-check: v1 ok" ]
  # Read-only: no ref was written.
  run git -C "$REMOTE_CLONE" show-ref --verify --quiet "refs/heads/$BRANCH"
  [ "$status" -ne 0 ]
  run git -C "$REMOTE_CLONE" show-ref --verify --quiet "refs/remotes/origin/$BRANCH"
  [ "$status" -ne 0 ]

  git -C "$REMOTE_CLONE" fetch -q origin "refs/heads/$BRANCH:refs/heads/$BRANCH"
  git -C "$REMOTE_CLONE" worktree add -q "$BATS_TEST_TMPDIR/remote-human" "$BRANCH"
  run --separate-stderr on_remote "$REMOTE_CLONE" dispatch --migrate-check "$BRANCH"
  [ "$status" -eq 2 ]
  [[ $stderr == *"checked out"* ]]
}

@test "dispatch --migrate-check refuses a remote local branch that is not an ancestor of origin's" {
  seed_pushed_branch
  diverged="$(git -C "$REMOTE_CLONE" commit-tree -p main -m diverged "$(git -C "$REMOTE_CLONE" rev-parse 'main^{tree}')")"
  git -C "$REMOTE_CLONE" update-ref "refs/heads/$BRANCH" "$diverged"

  run --separate-stderr on_remote "$REMOTE_CLONE" dispatch --migrate-check "$BRANCH"
  [ "$status" -eq 2 ]
  [[ $stderr == *"not an ancestor"* ]]
}

# fail_remote_window_once — the remote's first tmux new-window/new-session fails,
# so a migrate-in dies after its worktree is built and its patch applied.
fail_remote_window_once() {
  cat >"$RBINS/tmux" <<EOF
#!/usr/bin/env bash
case "\${1:-}" in
new-window | new-session)
  if [ ! -e "$BATS_TEST_TMPDIR/window-failed" ]; then
    : >"$BATS_TEST_TMPDIR/window-failed"
    echo "tmux: simulated failure" >&2
    exit 1
  fi
  ;;
esac
exec "$STUB_DIR/tmux" "\$@"
EOF
  chmod +x "$RBINS/tmux"
}

@test "a migrate-in that died after building its worktree retries: migrate-check passes and the re-run applies nothing twice" {
  seed_pushed_branch
  # A gitignored scaffold doc rides the patch force-added, as crew migrate snapshot builds it.
  printf 'PLAN.md\n' >>"$TEST_REPO/.git/info/exclude"
  printf 'plan\n' >"$LWT/PLAN.md"
  TREE="$(scratch_tree "$LWT" "$TIP" PLAN.md)"
  git -C "$LWT" diff --binary "$TIP" "$TREE" >"$PATCH"
  printf 'PLAN.md\n' >>"$REMOTE_CLONE/.git/info/exclude"
  make_bundle
  fail_remote_window_once

  migrate_dispatch
  [ "$status" -ne 0 ]
  [ -e "$BATS_TEST_TMPDIR/window-failed" ]
  wt="$(remote_wt "$BRANCH")"
  [ -d "$wt" ]
  [ "$(scratch_tree "$wt" "$TIP" PLAN.md)" = "$TREE" ]
  [ -z "$(local_dispatch_rows)" ]
  # The record dispatch wrote carries the crew id as a fifth line; the retry check accepts it.
  [ "$(wc -l <"$RHOME/data/crew/worktrees/$(printf %s "$(realpath -e -- "$wt")" | sha256sum | cut -c1-64)")" -eq 5 ]

  run --separate-stderr on_remote "$REMOTE_CLONE" dispatch --migrate-check "$BRANCH"
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$output" = "migrate-check: v1 ok" ]

  migrate_dispatch
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(remote_wt "$BRANCH")" = "$wt" ]
  [ "$(git -C "$REMOTE_CLONE" rev-parse "refs/heads/$BRANCH")" = "$TIP" ]
  [ "$(scratch_tree "$wt" "$TIP" PLAN.md)" = "$TREE" ]
  [ "$(cat "$wt/tracked.txt")" = $'one\ntwo' ]
  [ "$(cat "$wt/PLAN.md")" = plan ]
  [ "$(local_dispatch_rows | wc -l)" -eq 1 ]
}

@test "a migrate-in retry whose dirty worktree lacks a scaffold file the patch carries refuses cleanly" {
  seed_pushed_branch
  printf 'PLAN.md\n' >>"$TEST_REPO/.git/info/exclude"
  printf 'plan\n' >"$LWT/PLAN.md"
  TREE="$(scratch_tree "$LWT" "$TIP" PLAN.md)"
  git -C "$LWT" diff --binary "$TIP" "$TREE" >"$PATCH"
  printf 'PLAN.md\n' >>"$REMOTE_CLONE/.git/info/exclude"
  make_bundle
  fail_remote_window_once

  migrate_dispatch
  [ "$status" -ne 0 ]
  wt="$(remote_wt "$BRANCH")"
  [ -f "$wt/PLAN.md" ]
  rm "$wt/PLAN.md"

  export TMPDIR="$BATS_TEST_TMPDIR/private-tmp"
  mkdir -p "$TMPDIR"
  write_remote_env
  migrate_dispatch
  [ "$status" -eq 1 ]
  [[ $stderr == *"does not apply to $wt — nothing was applied"* ]]
  [[ $stderr != *pathspec* ]]
  [ -z "$(find "$TMPDIR" -mindepth 1 -maxdepth 1 -name 'tmp.*')" ]
}

@test "a migrate-in onto a branch checked out in a worktree dispatch did not build is refused" {
  seed_pushed_branch
  make_bundle
  git -C "$REMOTE_CLONE" fetch -q origin "refs/heads/$BRANCH:refs/heads/$BRANCH"
  git -C "$REMOTE_CLONE" worktree add -q "$BATS_TEST_TMPDIR/remote-human" "$BRANCH"

  migrate_dispatch
  [ "$status" -ne 0 ]
  [[ $stderr == *"checked out"* ]]
  [ -z "$(local_dispatch_rows)" ]
  [ ! -e "$BATS_TEST_TMPDIR/remote-human/untracked.txt" ]
}

@test "a migrate-in is not refused by the target's own record of the branch running on another host" {
  seed_pushed_branch
  make_bundle
  mkdir -p "$(remote_crew_dir)"
  jq -nc --arg b "$BRANCH" '{ts: (now * 1000 | floor), crew_id: "c1", kind: "dispatch", branch: $b, worker_id: "w-there", host: "laptop", remote: true}' \
    >>"$(remote_crew_dir)/events.jsonl"

  migrate_dispatch
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(scratch_tree "$(remote_wt "$BRANCH")" "$TIP")" = "$TREE" ]
}

@test "the migrate-in fetch runs no reference-transaction hook" {
  seed_pushed_branch
  make_bundle
  hooks="$(git -C "$REMOTE_CLONE" rev-parse --path-format=absolute --git-common-dir)/hooks"
  mkdir -p "$hooks"
  printf '#!/bin/sh\necho "$@" >>%q\n' "$BATS_TEST_TMPDIR/hook-ran" >"$hooks/reference-transaction"
  chmod +x "$hooks/reference-transaction"

  migrate_dispatch
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  [ "$(git -C "$REMOTE_CLONE" rev-parse "refs/heads/$BRANCH")" = "$TIP" ]
  [ ! -e "$BATS_TEST_TMPDIR/hook-ran" ]
}

@test "dispatch --migrate-check refuses a git config that drifted from the baseline" {
  seed_pushed_branch
  run --separate-stderr on_remote "$REMOTE_CLONE" dispatch --migrate-check "$BRANCH"
  [ "$status" -eq 0 ]
  git -C "$REMOTE_CLONE" config core.fsmonitor "touch $BATS_TEST_TMPDIR/fsmonitor-ran"

  run --separate-stderr on_remote "$REMOTE_CLONE" dispatch --migrate-check "$BRANCH"
  [ "$status" -eq 2 ]
  [ "$output" != "migrate-check: v1 ok" ]
  grep -q '^migrate-check: ' <<<"$stderr"
}

@test "dispatch --migrate-check names a failed fetch of origin's branch" {
  seed_pushed_branch
  cat >"$RBINS/git" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
  [ "\$a" != fetch ] || exit 128
done
exec $(command -v git) "\$@"
EOF
  chmod +x "$RBINS/git"

  run --separate-stderr on_remote "$REMOTE_CLONE" dispatch --migrate-check "$BRANCH"
  [ "$status" -eq 2 ]
  [[ $stderr == *"migrate-check: could not fetch origin's $BRANCH"* ]]
}

# --- dispatch migrate: a real local worker moved to mini ---

# start_local_worker — dispatch feat/7-x here (issue 7, title x). The lead is a
# stub whose argv[0] is claude, so its pane counts as a live engine; it never
# exits on its own. Sets LWT (its worktree) and WID (its worker id).
start_local_worker() {
  printf '#!/usr/bin/env bash\nexec -a claude bash -c "while :; do sleep 1; done"\n' >"$STUB_DIR/claude"
  run --separate-stderr dispatch standard opus --effort medium --crew-id c1 7 -- x
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    false
  }
  WID="$(sed -n 's/^worker_id: //p' <<<"$output")"
  LWT="$(git -C "$TEST_REPO" worktree list --porcelain | awk -v b="branch refs/heads/$BRANCH" '/^worktree /{p = $2} $0 == b {print p}')"
  [ -n "$WID" ] && [ -d "$LWT" ]
  poll_for 100 local_engine_live
}

local_engine_live() {
  "$REAL_TMUX" list-panes -a -F '#{@crew_branch} #{pane_current_command}' 2>/dev/null | grep -qxF "$BRANCH claude"
}

directive_posted() {
  jq -e --arg w "$WID" 'select(.kind == "msg" and .to == $w and (.body | fromjson? | .directive) == "migrate" and (.body | fromjson | .to_host) == "mini")' \
    "$LBUS" >/dev/null 2>&1
}

worker_says() { # <crew status args...> — posted as the local worker
  (cd "$TEST_REPO" && CREW_ID=c1 CREW_WORKER_ID="$WID" crew status "$WID" "$@")
}

# coop_worker <clean|uncommitted|blocked|symlinked> — the worker's half of a
# cooperative migration, in the background: on the directive it commits and
# pushes the fixed checkpoint, writes its handoff note and posts migrated.
# `uncommitted` also leaves a tracked edit behind; `blocked` only reports a
# failed push; `symlinked` plants its handoff note as a symlink to another file.
coop_worker() {
  (
    poll_for 600 directive_posted || exit 1
    if [ "$1" = blocked ]; then
      worker_says blocked "migrate: push failed: hook said no"
      exit 0
    fi
    printf 'work\n' >"$LWT/work.txt"
    git -C "$LWT" add work.txt
    git -C "$LWT" commit -q -m 'wip: checkpoint before host migration'
    git -C "$LWT" push -q origin "$BRANCH"
    [ "$1" != uncommitted ] || printf 'more\n' >>"$LWT/work.txt"
    mkdir -p "${LBUS%/*}/artifacts/$BRANCH"
    if [ "$1" = symlinked ]; then
      printf 'NOT THE WORKER NOTE\n' >"$BATS_TEST_TMPDIR/elsewhere"
      ln -s "$BATS_TEST_TMPDIR/elsewhere" "${LBUS%/*}/artifacts/$BRANCH/handoff.md"
    else
      printf 'handoff from the worker\n' >"${LBUS%/*}/artifacts/$BRANCH/handoff.md"
    fi
    worker_says migrated --to-host mini "checkpoint pushed"
  ) >"$BATS_TEST_TMPDIR/coop.log" 2>&1 3>&- &
  COOP_PID=$!
}

migrate() { run --separate-stderr dispatch migrate "$@"; }

# crew_intercept <pattern> <body> — crew runs <body> (with $real, the crew it
# wraps) when its argv joined by spaces matches the case <pattern> (spaces
# quoted), else the crew it wraps.
crew_intercept() {
  local real
  real="$(mktemp "$STUB_DIR/crew.XXXXXX")"
  mv -f "$STUB_DIR/crew" "$real"
  cat >"$STUB_DIR/crew" <<EOF
#!/usr/bin/env bash
real='$real'
case "\$*" in
$1) $2 ;;
esac
exec "\$real" "\$@"
EOF
  chmod +x "$STUB_DIR/crew"
}

migrated_ok() {
  [ "$status" -eq 0 ] || {
    echo "$stderr" >&2
    cat "$BATS_TEST_TMPDIR/coop.log" >&2 2>/dev/null || true
    false
  }
}

mark_rows() { jq -c 'select(.kind == "status" and .body.source == "migrate")' "$LBUS"; }

remote_rows() { jq -c 'select(.kind == "dispatch" and .remote == true)' "$LBUS"; }

# commit_unpushed_and_dirty — one commit origin has not seen, then a modified
# tracked file, a staged new file and an untracked file on top of it.
commit_unpushed_and_dirty() {
  printf 'one\n' >"$LWT/tracked.txt"
  git -C "$LWT" add tracked.txt
  git -C "$LWT" commit -q -m tracked
  printf 'two\n' >>"$LWT/tracked.txt"
  printf 'staged\n' >"$LWT/staged.txt"
  git -C "$LWT" add staged.txt
  printf 'untracked\n' >"$LWT/untracked.txt"
}

@test "dispatch migrate moves a cooperative worker to mini: claim released, resumed there at the tip with its handoff and transcript" {
  start_local_worker
  SID="$(cut -d' ' -f2 "${LBUS%/*}/leads/$BRANCH")"
  local_transcript="$LHOME/.claude/projects/${LWT//[^A-Za-z0-9]/-}/$SID.jsonl"
  mkdir -p "${local_transcript%/*}"
  printf '{"type":"local-transcript"}\n' >"$local_transcript"
  # A verdict the worker never pulled travels in the handoff.
  (cd "$TEST_REPO" && crew reply --crew c1 "$WID" "unread verdict") >/dev/null
  coop_worker clean
  # The dispatcher's own pane, unstamped: the remote window is mirrored into it.
  dispatcher_pane="$(tmux new-session -d -s main -P -F '#{pane_id}')"

  TMUX_PANE="$dispatcher_pane" migrate 7 --host mini --wait 60
  migrated_ok
  tip="$(git -C "$TEST_REPO" rev-parse "refs/heads/$BRANCH")"
  [ "$(mark_rows | jq -r '[.from, .body.to_host, .body.mode, .body.tip, .body.patch] | @tsv')" = "$WID	mini	coop	$tip	" ]
  bundle="$(mark_rows | jq -r .body.dir)"
  [[ $bundle == "${LBUS%/*}/migrate/$BRANCH/"* ]]
  [ "$(stat -c %a "$bundle/meta.json")" = 600 ]
  [ "$(cat "$bundle/transcript.jsonl")" = '{"type":"local-transcript"}' ]
  [ -z "$(find "$bundle" -name '.*')" ]
  [ "$(remote_rows | wc -l)" -eq 1 ]
  jq -e --arg b "$BRANCH" --arg h "$(uname -n | cut -d. -f1)" \
    '.host == "mini" and .branch == $b and .migrated_from == $h and .engine_context == "resumed"' <<<"$(remote_rows)"
  wt="$(remote_wt "$BRANCH")"
  [ "$(git -C "$wt" rev-parse HEAD)" = "$tip" ]
  [ "$(cat "$wt/work.txt")" = work ]
  handoff="$(remote_crew_dir)/artifacts/$BRANCH/migrated.md"
  grep -qx -- '- mode: coop' "$handoff"
  grep -qx -- '- last status: migrated — checkpoint pushed' "$handoff"
  grep -qF 'dispatcher:c1 at ' "$handoff"
  grep -qF ': unread verdict' "$handoff"
  grep -qx 'handoff from the worker' "$handoff"
  [ "$(cat "$(remote_transcript "$wt")")" = '{"type":"local-transcript"}' ]
  grep -qF -- '--window @crew_id=c1' "$OG_LOG"
  [ "${lines[-1]}" = "dispatch migrate: $BRANCH → host mini (engine context: resumed; patch: none)" ]
  run ! local_engine_live

  # A re-run finds the move done.
  migrate 7 --host mini
  migrated_ok
  [ "$output" = "dispatch migrate: $BRANCH already migrated to host mini" ]
  [ "$(mark_rows | wc -l)" -eq 1 ]
}

@test "dispatch migrate carries the work a cooperative worker left uncommitted as the patch" {
  start_local_worker
  coop_worker uncommitted

  migrate 7 --host mini --wait 60
  migrated_ok
  mark="$(mark_rows)"
  patch="$(jq -r .body.patch <<<"$mark")"
  [ "$patch" = "$(jq -r .body.dir <<<"$mark")/patch" ]
  [[ $patch == "${LBUS%/*}/migrate/$BRANCH/"*/patch ]]
  wt="$(remote_wt "$BRANCH")"
  [ "$(scratch_tree "$wt" "$(jq -r .body.tip <<<"$mark")")" = "$(jq -r .body.tree <<<"$mark")" ]
  [ "$(cat "$wt/work.txt")" = "$(printf 'work\nmore')" ]
  [ "${lines[-1]}" = "dispatch migrate: $BRANCH → host mini (engine context: fresh: no transcript travelled; patch: $patch)" ]
}

@test "dispatch migrate --now pushes the branch and lands tracked, staged and untracked changes intact on mini" {
  start_local_worker
  commit_unpushed_and_dirty
  tip="$(git -C "$LWT" rev-parse HEAD)"
  [ "$(git -C "$TEST_REPO" ls-remote origin "refs/heads/$BRANCH" | cut -f1)" != "$tip" ]

  migrate 7 --host mini --now
  migrated_ok
  [ "$(git -C "$TEST_REPO" ls-remote origin "refs/heads/$BRANCH" | cut -f1)" = "$tip" ]
  mark="$(mark_rows)"
  jq -e --arg t "$tip" '.body.mode == "now" and .body.tip == $t' <<<"$mark"
  wt="$(remote_wt "$BRANCH")"
  [ "$(scratch_tree "$wt" "$tip")" = "$(jq -r .body.tree <<<"$mark")" ]
  [ "$(cat "$wt/tracked.txt")" = "$(printf 'one\ntwo')" ]
  [ "$(cat "$wt/staged.txt")" = staged ]
  [ "$(cat "$wt/untracked.txt")" = untracked ]
}

@test "after a migration to mini, a dispatch of the issue to a third host is refused" {
  start_local_worker
  migrate 7 --host mini --now
  migrated_ok

  run --separate-stderr dispatch standard opus --effort medium --crew-id c1 --host chromebox 7 -- x
  [ "$status" -eq 1 ]
  [[ $stderr == *"issue #7 is already held by $BRANCH on host mini — "* ]]
}

@test "a failed remote step leaves the worker marked and kept; the re-run retries only that step, then reap removes the worktree" {
  start_local_worker
  SSH_FAIL_MIGRATE=1 DISPATCH_HOST_RECOVER_TRIES=1 DISPATCH_HOST_RECOVER_SLEEP=0 migrate 7 --host mini --now
  [ "$status" -eq 1 ]
  [[ $stderr == *"dispatch migrate: migration to mini failed (exit "[0-9]*"); the worker is stopped and marked migrating to mini — retry: dispatch migrate $BRANCH --host mini; abandon: dispatch resume $BRANCH"* ]]
  [ "$(mark_rows | wc -l)" -eq 1 ]
  [ -z "$(remote_rows)" ]

  run --separate-stderr crew reap --no-wait
  [ -d "$LWT" ]
  [[ "$output$stderr" == *"no remote dispatch row on mini yet"* ]]

  migrate 7 --host mini
  migrated_ok
  [ "$(mark_rows | wc -l)" -eq 1 ]
  [ "$(remote_rows | wc -l)" -eq 1 ]

  run --separate-stderr crew reap --no-wait
  [ ! -d "$LWT" ]
  [ -n "$(git -C "$TEST_REPO" ls-remote origin "refs/heads/$BRANCH")" ]
}

@test "a pre-push hook refusing the --now push surfaces verbatim, seeing only allowlisted env; nothing is marked and the worktree is intact" {
  start_local_worker
  commit_unpushed_and_dirty
  # The hook records variable names only, never values.
  cat >"$TEST_REPO/.git/hooks/pre-push" <<EOF
#!/bin/sh
env | cut -d= -f1 | sort >"$BATS_TEST_TMPDIR/hook.env"
echo "HOOK SAYS NO" >&2
exit 1
EOF
  chmod +x "$TEST_REPO/.git/hooks/pre-push"
  dispatcher_pane="$(tmux new-session -d -s main -P -F '#{pane_id}')"

  : >"$BATS_TEST_TMPDIR/gitconfig-global"
  : >"$BATS_TEST_TMPDIR/gitconfig-system"

  GIT_CONFIG_GLOBAL="$BATS_TEST_TMPDIR/gitconfig-global" \
    GIT_CONFIG_SYSTEM="$BATS_TEST_TMPDIR/gitconfig-system" GIT_CONFIG_NOSYSTEM=1 \
    GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.fake GIT_CONFIG_VALUE_0=fake \
    GH_TOKEN=fake-token-for-test DISPATCHER_ONLY_SECRET=fake CREW_ID=c1 TMUX_PANE="$dispatcher_pane" \
    migrate 7 --host mini --now
  [ "$status" -eq 1 ]
  [[ $stderr == *"HOOK SAYS NO"* ]]
  [ -z "$(grep -xE 'GH_TOKEN|DISPATCHER_ONLY_SECRET|CREW_ID|CREW_WORKER_ID|TMUX|TMUX_PANE' "$BATS_TEST_TMPDIR/hook.env")" ]
  # The harness exports XDG_CONFIG_HOME. Git's config locators survive so a global pre-push guard still runs; token-capable config vars do not.
  [ "$(grep -xcE 'XDG_CONFIG_HOME|GIT_CONFIG_GLOBAL|GIT_CONFIG_SYSTEM|GIT_CONFIG_NOSYSTEM' "$BATS_TEST_TMPDIR/hook.env")" -eq 4 ]
  [ -z "$(grep -xE 'GIT_CONFIG_COUNT|GIT_CONFIG_KEY_0|GIT_CONFIG_VALUE_0' "$BATS_TEST_TMPDIR/hook.env")" ]
  grep -qx PATH "$BATS_TEST_TMPDIR/hook.env"
  grep -qx HOME "$BATS_TEST_TMPDIR/hook.env"
  [[ $stderr == *"the worker is stopped; nothing was marked — resume it here with: dispatch resume $BRANCH"* ]]
  [ -z "$(jq -c 'select(.kind == "status" and .body.state == "migrated")' "$LBUS")" ]
  [ -z "$(remote_rows)" ]
  [ "$(cat "$LWT/tracked.txt")" = "$(printf 'one\ntwo')" ]
  [ "$(cat "$LWT/staged.txt")" = staged ]
  [ "$(cat "$LWT/untracked.txt")" = untracked ]
}

@test "a relative core.hooksPath pre-push hook runs for a --now push started from a subdirectory of the main checkout" {
  git -C "$TEST_REPO" config core.hooksPath .husky
  mkdir -p "$TEST_REPO/.husky" "$TEST_REPO/sub"
  printf '#!/bin/sh\necho "HUSKY SAYS NO" >&2\nexit 1\n' >"$TEST_REPO/.husky/pre-push"
  chmod +x "$TEST_REPO/.husky/pre-push"
  start_local_worker
  commit_unpushed_and_dirty

  cd "$TEST_REPO/sub"
  migrate 7 --host mini --now
  [ "$status" -eq 1 ]
  [[ $stderr == *"HUSKY SAYS NO"* ]]
  [[ $stderr == *"the worker is stopped; nothing was marked — resume it here with: dispatch resume $BRANCH"* ]]
  [ -z "$(mark_rows)" ]
  [ "$(git -C "$TEST_REPO" ls-remote origin "refs/heads/$BRANCH" | cut -f1)" != "$(git -C "$LWT" rev-parse HEAD)" ]
}

@test "dispatch migrate --now refuses a worktree whose .git no longer points at its admin dir; nothing is marked" {
  start_local_worker
  # Past the snapshot, which refuses a swapped .git itself.
  crew_intercept "'migrate snapshot '*" "\"\$real\" \"\$@\" || exit; printf 'gitdir: %s\n' '$TEST_REPO/.git' >'$LWT/.git'; exit 0"

  migrate 7 --host mini --now
  [ "$status" -eq 1 ]
  [[ $stderr == *"$LWT/.git no longer points at "*" — the worker is stopped; nothing was marked — resume it here with: dispatch resume $BRANCH"* ]]
  [ -z "$(mark_rows)" ]
  [ -z "$(remote_rows)" ]
}

@test "a cooperative worker whose push failed blocks with migrate:, surfaced verbatim; the worker stays live and blocked" {
  start_local_worker
  coop_worker blocked

  migrate 7 --host mini --wait 60
  [ "$status" -eq 1 ]
  [[ $stderr == *"migrate: push failed: hook said no"* ]]
  [ "$(jq -r --arg w "$WID" 'select(.kind == "status" and .from == $w) | .body.state' "$LBUS" | tail -1)" = blocked ]
  local_engine_live
  [ -z "$(mark_rows)" ]
  [ -z "$(remote_rows)" ]
}

@test "dispatch migrate is refused from a worker's environment before anything is read or called" {
  seed_local_worker
  CREW_WORKER_ID="worker:feat/9-y#s1-1" migrate 7 --host mini
  [ "$status" -eq 2 ]
  [[ $stderr == *"dispatcher-only"* ]]
  [ ! -s "$SSH_CMDS" ]
}

@test "dispatch migrate refuses a worker with --add-dir grants, naming them" {
  start_local_worker
  sed -i "/^crew_id: /a add_dir: $BATS_TEST_TMPDIR/granted" "$LWT/WORKER_TASK.md"

  migrate 7 --host mini --now
  [ "$status" -eq 2 ]
  [[ $stderr == *"--add-dir grants do not exist on host mini: $BATS_TEST_TMPDIR/granted"* ]]
  [ ! -s "$SSH_CMDS" ]
  local_engine_live
}

@test "dispatch migrate refuses a worker that already runs on another host" {
  seed_local_worker
  jq -nc --arg b "$BRANCH" '{ts: (now * 1000 | floor), crew_id: "c1", kind: "dispatch", branch: $b, worker_id: "worker:feat/7-x#s1-1", host: "chromebox", remote: true}' >>"$LBUS"

  migrate 7 --host mini
  [ "$status" -eq 2 ]
  [[ $stderr == *"$BRANCH is already on host chromebox"* ]]
  [ ! -s "$SSH_CMDS" ]
}

@test "dispatch migrate refuses a host whose dispatch has no --migrate-check, before the worker is touched" {
  cat >"$RBINS/dispatch" <<EOF
#!/usr/bin/env bash
if [ "\${1:-}" = --migrate-check ]; then
  echo "usage: dispatch <trivial|standard|deep> <model> --effort E [flags] [--] <title...> — see: dispatch --help" >&2
  exit 2
fi
exec "$STUB_DIR/dispatch" "\$@"
EOF
  chmod +x "$RBINS/dispatch"
  start_local_worker

  migrate 7 --host mini --now
  [ "$status" -eq 2 ]
  [[ $stderr == *"host mini runs a dispatch without migrate support — update it first"* ]]
  local_engine_live
  [ -z "$(mark_rows)" ]
}

@test "dispatch migrate refuses while another migration of the branch holds its lock, live or dead" {
  seed_local_worker
  lock="${LBUS%/*}/migrate-$(printf '%s' "$BRANCH" | cksum | cut -d' ' -f1).lock"
  ln -s "$$" "$lock"

  migrate 7 --host mini
  [ "$status" -eq 2 ]
  [[ $stderr == *"(pid $$)"* ]]
  [ "$(readlink "$lock")" = "$$" ]

  rm -f "$lock"
  true &
  dead=$!
  wait "$dead"
  ln -s "$dead" "$lock"
  migrate 7 --host mini
  [ "$status" -eq 2 ]
  [[ $stderr == *"remove $lock"* ]]
  [ ! -s "$SSH_CMDS" ]
}

@test "a cooperative worker that does not quiesce in time exits 3 with the human escalation, untouched, its directive withdrawn" {
  start_local_worker

  DISPATCH_MIGRATE_CANCEL_GRACE=1 migrate 7 --host mini --wait 2
  [ "$status" -eq 3 ]
  [[ $stderr == *"worker did not quiesce within 2s; the migrate directive was withdrawn — ask the human; dispatch migrate --now only if they are leaving now"* ]]
  jq -e --arg w "$WID" 'select(.kind == "msg" and .to == $w and .from == "dispatcher:c1") | .body | fromjson
    | select(.directive == "migrate-cancel" and .to_host == "mini")' "$LBUS"
  local_engine_live
  [ -z "$(mark_rows)" ]
  [ -z "$(find "${LBUS%/*}" -maxdepth 1 -name 'migrate-*.lock')" ]

  # The late worker can no longer report it migrated.
  run --separate-stderr worker_says migrated --to-host mini "checkpoint pushed"
  [ "$status" -ne 0 ]
  [[ $stderr == *"migrated needs a dispatcher migrate directive naming --to-host mini"* ]]

  # A withdrawal that could not be sent is said to be possibly still pending.
  crew_intercept "'reply '*migrate-cancel*" "exit 1"
  DISPATCH_MIGRATE_CANCEL_GRACE=0 migrate 7 --host mini --wait 1
  [ "$status" -eq 3 ]
  [[ $stderr == *"worker did not quiesce within 1s; withdrawing the migrate directive failed, so it may still be pending — ask the human"* ]]
  [ -z "$(mark_rows)" ]
}

@test "a migrated that lands just after the directive is withdrawn, within the cancel grace, still moves the worker" {
  start_local_worker
  # Mimics crew status having read the directive before the cancel and
  # appended its row after it.
  local late
  late="$(jq -nc --arg w "$WID" '{crew_id: "c1", kind: "status", from: $w, body: {state: "migrated", to_host: "mini", detail: "checkpoint pushed"}}')"
  crew_intercept "'reply '*migrate-cancel*" "\"\$real\" \"\$@\" || exit; (sleep 1; jq -c '.ts = (now * 1000 | floor)' <<<'$late' >>'$LBUS') >/dev/null 2>&1 </dev/null & exit 0"

  DISPATCH_MIGRATE_CANCEL_GRACE=3 migrate 7 --host mini --wait 1
  migrated_ok
  [ "$(mark_rows | jq -r .body.to_host)" = mini ]
  [ "$(remote_rows | wc -l)" -eq 1 ]
}

@test "a cooperative worker's symlinked handoff note is not carried; unreadable messages are said so" {
  start_local_worker
  coop_worker symlinked
  crew_intercept "'inbox '*--undelivered*" "exit 1"

  migrate 7 --host mini --wait 60
  migrated_ok
  [[ $stderr == *"could not read the messages $WID never pulled"* ]]
  handoff="$(remote_crew_dir)/artifacts/$BRANCH/migrated.md"
  grep -qx -- '- mode: coop' "$handoff"
  grep -qx 'messages could not be read' "$handoff"
  run ! grep -qF 'NOT THE WORKER NOTE' "$handoff"
  run ! grep -qF "The worker's handoff note" "$handoff"
}

@test "dispatch migrate --now does not carry a transcript that is a symlink" {
  start_local_worker
  SID="$(cut -d' ' -f2 "${LBUS%/*}/leads/$BRANCH")"
  local_transcript="$LHOME/.claude/projects/${LWT//[^A-Za-z0-9]/-}/$SID.jsonl"
  mkdir -p "${local_transcript%/*}"
  printf '{"type":"elsewhere"}\n' >"$BATS_TEST_TMPDIR/elsewhere.jsonl"
  ln -s "$BATS_TEST_TMPDIR/elsewhere.jsonl" "$local_transcript"

  migrate 7 --host mini --now
  migrated_ok
  [[ $stderr == *"no transcript travels: the lead's claude session has no transcript"* ]]
  [ ! -e "$(mark_rows | jq -r .body.dir)/transcript.jsonl" ]
  jq -e '.engine_context == "fresh"' <<<"$(remote_rows)"
  [ ! -e "$(remote_transcript "$(remote_wt "$BRANCH")")" ]
}

@test "a transcript that would push the bundle over the request budget is dropped; the worker still moves" {
  start_local_worker
  SID="$(cut -d' ' -f2 "${LBUS%/*}/leads/$BRANCH")"
  local_transcript="$LHOME/.claude/projects/${LWT//[^A-Za-z0-9]/-}/$SID.jsonl"
  mkdir -p "${local_transcript%/*}"
  head -c 200000 /dev/urandom | base64 >"$local_transcript"

  DISPATCH_MIGRATE_MAX_BYTES=$((96 << 10)) migrate 7 --host mini --now
  migrated_ok
  [[ $stderr == *"no transcript travels: transcript dropped: bundle over budget"* ]]
  bundle="$(mark_rows | jq -r .body.dir)"
  [ ! -e "$bundle/transcript.jsonl" ]
  jq -e 'has("session") | not' "$bundle/meta.json"
  jq -e '.engine_context == "fresh"' <<<"$(remote_rows)"
}

@test "a bundle over the request budget without its transcript is refused before the mark; the worker stays stopped here" {
  start_local_worker
  head -c 200000 /dev/urandom >"$LWT/noise.bin"

  DISPATCH_MIGRATE_MAX_BYTES=$((96 << 10)) migrate 7 --host mini --now
  [ "$status" -eq 1 ]
  [[ $stderr == *"bundle too large for one request ("[0-9]*" MiB) — nothing was marked; the worker is stopped — resume it here with: dispatch resume $BRANCH"* ]]
  [ -z "$(mark_rows)" ]
  [ -z "$(remote_rows)" ]
  [ -f "$LWT/noise.bin" ]
}

# dispatch-migrate --help names every flag its parse loop has an arm for.
@test "dispatch migrate --help lists every flag it parses, and dispatch --help lists dispatch migrate" {
  migrate --help
  [ "$status" -eq 0 ]
  [[ ${lines[0]} == "usage: dispatch migrate "* ]]
  for f in $(grep -oE '^  --[a-z-]+\)' "$REPO_ROOT/adapters/core/dispatch-migrate.sh" | tr -d ' )'); do
    grep -qE -- "^  $f( |$)" <<<"$output" || {
      echo "no help line for $f"
      false
    }
  done
  run dispatch --help
  grep -qE '^       dispatch migrate <target> --host' <<<"$output"
  grep -qE '^  dispatch migrate \.\.\. ' <<<"$output"
}
