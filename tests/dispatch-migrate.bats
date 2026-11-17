bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# dispatch migrate against the fake-ssh-host harness: tests/remote-host-helpers.bash.

load remote-host-helpers

setup() { remote_host_setup; }
teardown() { remote_host_teardown; }

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
  (cd "$TEST_REPO" && crew migrate mark --crew c1 "$BRANCH" --to-host "$1" --mode coop \
    --tip "$(git rev-parse HEAD)" --tree "$(git rev-parse 'HEAD^{tree}')")
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

scratch_tree() { # <worktree> <tip> — read-tree <tip> + add -A in a private index
  local idx
  idx="$(mktemp -u "$BATS_TEST_TMPDIR/idx.XXXXXX")"
  (cd "$1" && GIT_INDEX_FILE="$idx" git read-tree "$2" && GIT_INDEX_FILE="$idx" git add -A && GIT_INDEX_FILE="$idx" git write-tree)
}

# make_bundle [--no-transcript] [--tip <oid>] — BUNDLE under the local crew dir's
# artifacts, as dispatch migrate builds it.
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
  BUNDLE="${LBUS%/*}/artifacts/$BRANCH/migrate-20261117T000000Z"
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

@test "a bundle outside the crew dir's artifacts is refused before the remote is called" {
  seed_pushed_branch
  make_bundle
  outside="$BATS_TEST_TMPDIR/migrate-outside"
  cp -r "$BUNDLE" "$outside"
  BUNDLE="$outside"

  migrate_dispatch
  [ "$status" -eq 1 ]
  [[ $stderr == *"DISPATCH_MIGRATE_BUNDLE"*"artifacts"* ]]
  [ ! -s "$SSH_CMDS" ]
  assert_nothing_recorded_locally
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
