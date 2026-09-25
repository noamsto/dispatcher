bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
  unset CREW_ID
  # Absent unless a test sets it, so pane-recording stays deterministic
  # regardless of whether bats itself runs inside a tmux pane.
  unset TMUX_PANE
  # No engine process by default, so quiet:->dead: escalation stays
  # deterministic regardless of what runs on the host tmux server.
  export CREW_STALL_PROC_CMD='printf ""'
}

teardown() {
  # Before teardown_repo: a leaked `crew stream` (and the `crew watch` it owns)
  # would race its rm -rf. See the stream harness at the bottom of this file.
  stop_stream
  if [ -n "${HOLDER_PID:-}" ]; then
    kill -KILL "$HOLDER_PID" 2>/dev/null || true
  fi
  teardown_repo
}

# Real tmux pane_current_path is kernel-canonical (pwd -P); git worktree paths
# resolve symlinks too (/var → /private/var on macOS) but $BATS_TEST_TMPDIR does
# not — canonicalize existing dirs in fixture text so absence-of-release tests
# can fail when occupancy silently misses (#52).
_canon_stub_path() {
  if [ -d "$1" ]; then
    (cd "$1" && pwd -P)
  else
    printf '%s' "$1"
  fi
}

_canon_stub_wins_body() {
  local body="$1"
  [ -n "$body" ] || return 0
  local line f1 f2 f3 rest
  while IFS= read -r line || [ -n "$line" ]; do
    if [ -z "$line" ]; then
      printf '\n'
      continue
    fi
    IFS=$'\t' read -r f1 f2 f3 rest <<< "$line"
    if [ -n "$f3" ]; then
      f3="$(_canon_stub_path "$f3")"
    fi
    printf '%s\t%s\t%s' "$f1" "$f2" "$f3"
    [ -n "$rest" ] && printf '\t%s' "$rest"
    printf '\n'
  done <<< "$body"
}

_canon_stub_panes_body() {
  local body="$1"
  [ -n "$body" ] || return 0
  local line cmd path
  while IFS= read -r line || [ -n "$line" ]; do
    if [ -z "$line" ]; then
      printf '\n'
      continue
    fi
    if [[ "$line" == *$'\t'* ]]; then
      printf '%s\n' "$line"
      continue
    fi
    cmd="${line%% *}"
    path="${line#* }"
    if [ "$path" != "$line" ] && [ -n "$path" ]; then
      path="$(_canon_stub_path "$path")"
      printf '%s %s\n' "$cmd" "$path"
    else
      printf '%s\n' "$line"
    fi
  done <<< "$body"
}

# stub_tmux <list-windows-body> <list-panes-body> — a tmux whose list output is
# fixed text. crew's real tmux calls are `|| true`-tolerant, so without this the
# occupancy tests would read the developer's live server and flake.
stub_tmux() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  _canon_stub_wins_body "$1" >"$STUB_DIR/wins.txt"
  _canon_stub_panes_body "$2" >"$STUB_DIR/panes.txt"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# stub_wt_removes — a wt whose `remove` REALLY removes the worktree (like the
# real binary: it deletes the working tree and its registration) but exits
# non-zero WITHOUT deleting the branch — the exact squash-merge shape #194
# fixes. This repo squash-merges, so a merged PR's branch is never an ancestor
# of main: `wt remove` exits non-zero because it refuses to delete the branch
# it reads as unmerged, even though the removal it was asked for succeeded.
# reap must judge success by the observable outcome (worktree gone), not by
# the exit status, and reap (not wt) deletes the branch for a MERGED PR.
stub_wt_removes() {
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = remove ]; then
  # `wt remove --foreground --no-hooks <branch>` — the branch is the last arg.
  branch="${!#}"
  wtp=$(git worktree list --porcelain | awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
  [ -n "$wtp" ] && rm -rf "$wtp"
  git worktree prune
  echo "Branch unmerged; to delete, run wt remove -D" >&2
  exit 1
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
}

@test "id: honours CREW_ID when set" {
  CREW_ID=1720800000-12345 run run_crew id
  [ "$status" -eq 0 ]
  [ "$output" = "1720800000-12345" ]
}

@test "identity: is deterministic over the branch" {
  run run_crew identity feat/foo
  [ "$status" -eq 0 ]
  first="$output"
  run run_crew identity feat/foo
  [ "$output" = "$first" ]
}

@test "identity: emits name, color and tmux keys" {
  run run_crew identity feat/foo
  [ "$status" -eq 0 ]
  echo "$output" | jq -e 'has("name") and has("color") and has("tmux")'
}

@test "identity: two live branches on one hash slot get different codenames" {
  a=feat/33-thing
  b=feat/38-thing
  [ "$(run_crew identity $a | jq -r .name)" = "$(run_crew identity $b | jq -r .name)" ]
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  ida="$(run_crew identity $a c1)"
  jq -nc --argjson i "$ida" '{ts:1,crew_id:"c1",kind:"dispatch",branch:"feat/33-thing"} + $i' >>"$dir/events.jsonl"
  idb="$(run_crew identity $b c1)"
  [ "$(echo "$ida" | jq -r .name)" != "$(echo "$idb" | jq -r .name)" ]
  [ "$(echo "$ida" | jq -r .tmux)" != "$(echo "$idb" | jq -r .tmux)" ]
}

@test "identity: a branch keeps its recorded identity, and a finished worker frees its slot" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/33-thing","name":"nova","color":"magenta","tmux":"colour127"}' >>"$dir/events.jsonl"
  [ "$(run_crew identity feat/33-thing c1 | jq -r .name)" = "nova" ]
  # 38 hashes to sage; a finished sage-holder must not push it forward.
  printf '%s\n' '{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/9-x","name":"sage","color":"green","tmux":"colour28"}' >>"$dir/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/9-x#s1-1" working
  [ "$(run_crew identity feat/38-thing c1 | jq -r .name)" != "sage" ]
  CREW_ID=c1 run_crew status "worker:feat/9-x#s1-1" done
  [ "$(run_crew identity feat/38-thing c1 | jq -r .name)" = "sage" ]
}

@test "roster: shows the recorded codename without a suffix" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/33-thing","name":"sage","color":"green","tmux":"colour28"}' >>"$dir/events.jsonl"
  printf '%s\n' '{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/38-thing","name":"atlas","color":"blue","tmux":"colour32"}' >>"$dir/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/33-thing#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/38-thing#s1-1" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '[.[].name] | sort | join(",")')" = "atlas,sage" ]
}

@test "repo-keyed subcommands refuse to run outside a git repo" {
  cd /
  CREW_ID=c1 run run_crew status worker working
  [ "$status" -eq 1 ]
  [[ "$output" == *"not in a git repo"* ]]
}

@test "status: rejects an unknown state" {
  CREW_ID=c1 run run_crew status worker bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"status state must be one of"* ]]
}

@test "status: accepts every documented state" {
  for s in working blocked pr_open done failed exited; do
    CREW_ID=c1 run run_crew status "worker-$s" "$s"
    [ "$status" -eq 0 ]
  done
}

@test "status: a worker's status lands on its lead pane" {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' 'lead' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export TMUX_PANE=%9
  CREW_ID=c1 run run_crew status "worker:feat/x#s1-1" blocked "waiting on #273"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-option -p -t %9 @crew_state blocked' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-option -p -t %9 @crew_detail waiting on #273' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-option -p -t %9 @crew_source ' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.state'"
  [ "$output" = blocked ]
}

@test "status: the lead pane detail is truncated to 40 characters" {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' 'lead' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export TMUX_PANE=%9
  long="$(printf 'x%.0s' {1..80})"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working "$long"
  run grep -oF -- "set-option -p -t %9 @crew_detail $(printf 'x%.0s' {1..40})" "$STUB_LOG"
  [ "$status" -eq 0 ]
  run ! grep -qF "$(printf 'x%.0s' {1..41})" "$STUB_LOG"
}

@test "status: a role pane's post never touches the lead pane" {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' 'reviewer' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export TMUX_PANE=%9 CREW_ROLE_ID=role:feat/x:reviewer
  CREW_ID=c1 run run_crew status "role:feat/x:reviewer" working
  [ "$status" -eq 0 ]
  run ! grep -q '@crew_state' "$STUB_LOG"
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.state'"
  [ "$output" = working ]
}

@test "status: outside tmux the pane publish is a silent no-op" {
  unset TMUX_PANE
  CREW_ID=c1 run run_crew status "worker:feat/x#s1-1" working plan
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "$output" = plan ]
}

@test "status: a repeated terminal state is written once" {
  CREW_ID=c1 run_crew status worker done
  CREW_ID=c1 run_crew status worker done
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run grep -c '"state":"done"' "$log"
  [ "$output" = "1" ]
}

@test "status: a repeated non-terminal state is NOT deduped" {
  CREW_ID=c1 run_crew status worker working
  CREW_ID=c1 run_crew status worker working
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run grep -c '"state":"working"' "$log"
  [ "$output" = "2" ]
}

@test "status: is addressed to the crew dispatcher" {
  CREW_ID=c1 run_crew status worker working
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="status") | .to' "$log"
  [ "$output" = "dispatcher:c1" ]
}

@test "status: fails when no crew id can be resolved" {
  CREW_ID= run run_crew status worker working
  [ "$status" -eq 1 ]
  [[ "$output" == *"CREW_ID unset"* ]]
}

@test "crew id resolves from WORKER_TASK.md when CREW_ID is unset" {
  printf 'crew_id: c-from-file\n' >WORKER_TASK.md
  CREW_ID= run_crew status worker working
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r '.crew_id' "$log"
  [ "$output" = "c-from-file" ]
}

@test "msg: records from, to and body verbatim" {
  CREW_ID=c1 run_crew msg alice bob "ship it"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | "\(.from)|\(.to)|\(.body)"' "$log"
  [ "$output" = "alice|bob|ship it" ]
}

# #46: a shell expanding an unset $CREW_ID into "dispatcher:$CREW_ID" leaves
# a bare trailing colon — msg must fail loudly instead of logging it.
@test "msg: rejects dispatcher: with an empty id" {
  CREW_ID=c1 run run_crew msg worker "dispatcher:" "hi"
  [ "$status" -eq 1 ]
  [[ "$output" == *"missing an id after the colon"* ]]
}

@test "msg: rejects worker: with an empty id" {
  CREW_ID=c1 run run_crew msg dispatcher:c1 "worker:" "hi"
  [ "$status" -eq 1 ]
  [[ "$output" == *"missing an id after the colon"* ]]
}

@test "msg: rejects retro: with an empty id" {
  CREW_ID=c1 run run_crew msg worker "retro:" "hi"
  [ "$status" -eq 1 ]
  [[ "$output" == *"missing an id after the colon"* ]]
}

@test "msg: still accepts every currently-valid recipient shape" {
  for to in bob 'worker:feat/1' 'worker:feat/1#s1-1' 'dispatcher:c1' 'retro:c1' 'metrics:c1' '*'; do
    CREW_ID=c1 run run_crew msg worker "$to" "hi"
    [ "$status" -eq 0 ]
  done
}

@test "status: an oversized detail is clipped so the line stays one atomic write" {
  big="$(head -c 8192 /dev/zero | tr '\0' x)"
  CREW_ID=c1 run_crew status worker failed "$big"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  line_bytes="$(printf '%s' "$(head -1 "$log")" | wc -c | tr -d ' ')"
  [ "$line_bytes" -le 4096 ]
  run jq -e '.body.state == "failed" and (.body.detail | endswith("[elided]"))' "$log"
  [ "$status" -eq 0 ]
}

@test "msg: an oversized body is clipped so the line stays one atomic write" {
  big="$(head -c 8192 /dev/zero | tr '\0' x)"
  CREW_ID=c1 run_crew msg alice bob "$big"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  line_bytes="$(printf '%s' "$(head -1 "$log")" | wc -c | tr -d ' ')"
  [ "$line_bytes" -le 4096 ]
  run jq -e '.from == "alice" and .to == "bob" and (.body | endswith("[elided]"))' "$log"
  [ "$status" -eq 0 ]
}

@test "status: quote-heavy text still fits (JSON escaping is nonlinear)" {
  big="$(head -c 4000 /dev/zero | tr '\0' '"')"
  CREW_ID=c1 run_crew status worker failed "$big"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  line_bytes="$(printf '%s' "$(head -1 "$log")" | wc -c | tr -d ' ')"
  [ "$line_bytes" -le 4096 ]
  run jq -e . "$log"
  [ "$status" -eq 0 ]
}

@test "bus: concurrent oversized writes never splice two records into one line" {
  big="$(head -c 8192 /dev/zero | tr '\0' x)"
  for i in 1 2 3 4 5 6 7 8; do
    (
      for _ in 1 2 3 4 5; do
        CREW_ID=c1 bash -euo pipefail "$CREW" status "worker:b$i" working "$big"
      done
    ) &
  done
  wait
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  [ "$(wc -l <"$log" | tr -d ' ')" -eq 40 ]
  while IFS= read -r l; do
    printf '%s' "$l" | jq -e . >/dev/null
  done <"$log"
  # A line count alone cannot see a splice: each append contributes exactly one
  # newline either way, so assert one record per line too.
  [ "$(grep -o '{"ts"' "$log" | wc -l | tr -d ' ')" -eq 40 ]
}

@test "bus: concurrent reply appends never splice (#61's converted site)" {
  # Same shape as the "concurrent oversized writes" test above, but through
  # `reply` — one of the five sites #61 converted to _bus_append. `reply`
  # doesn't need a live session to target as long as `to` isn't `worker:*`
  # (that prefix triggers session resolution this test isn't exercising).
  # 4002 x's lands the finished line (with trailing newline) at 4097 bytes —
  # one byte over `_LINE_MAX`, too small for `_fit_line`'s shrink loop to ever
  # engage (it measures the line *without* the newline `_bus_append` adds, so
  # it sees 4096 and calls that done) but enough to force bash's `printf`
  # builtin to split the write into two syscalls (4096 + 1) instead of one —
  # exactly the multi-write window `_bus_append`'s `dd` closes.
  big="$(head -c 4002 /dev/zero | tr '\0' x)"
  for i in 1 2 3 4 5 6 7 8; do
    (
      for _ in 1 2 3 4 5; do
        CREW_ID=c1 bash -euo pipefail "$CREW" reply "peer$i" "$big"
      done
    ) &
  done
  wait
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  [ "$(wc -l <"$log" | tr -d ' ')" -eq 40 ]
  while IFS= read -r l; do
    printf '%s' "$l" | jq -e . >/dev/null
  done <"$log"
  [ "$(grep -o '{"ts"' "$log" | wc -l | tr -d ' ')" -eq 40 ]
}

# #391: a hard kill mid-append leaves a trailing line with no newline; the next
# `_bus_append` must start on a fresh line instead of gluing a whole valid
# record onto the torn fragment, which a reader would lose as one unparsable
# line. The new record must be readable as its own row after the fragment.
@test "bus: an append after a crash-torn trailing line starts on a fresh line" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew status "$id" working
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  # The crash-torn fragment, with no trailing newline.
  printf '{"ts":1785951264000,"crew_id":"c-to' >>"$log"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "hello"
  # The torn fragment stays its own line — never spliced with the new record.
  [ "$(sed -n '2p' "$log")" = '{"ts":1785951264000,"crew_id":"c-to' ]
  # The new record starts its own line and parses.
  printf '%s' "$(sed -n '3p' "$log")" | jq -e '.kind=="msg" and .from=="worker:feat/x#s1-1" and .body=="hello"' >/dev/null
  [ "$(wc -l <"$log" | tr -d ' ')" -eq 3 ]
}

@test "register: is idempotent for the same pid" {
  CREW_ID=c1 run run_crew register $$
  [ "$status" -eq 0 ]
  CREW_ID=c1 run run_crew register $$
  [ "$status" -eq 0 ]
}

@test "register: is non-exclusive across crews" {
  CREW_ID=c1 run run_crew register $$
  [ "$status" -eq 0 ]
  CREW_ID=c2 run run_crew register $$
  [ "$status" -eq 0 ]
}

@test "register records the dispatcher pane when in tmux" {
  cdir="$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/c1"
  CREW_ID=c1 TMUX_PANE='%12' run_crew register 4242
  [ "$(cat "$cdir/pid")" = 4242 ]
  [ "$(cat "$cdir/pane")" = '%12' ]
}

@test "register writes no pane file outside tmux" {
  cdir="$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/c1"
  CREW_ID=c1 run_crew register 4242
  [ -f "$cdir/pid" ]
  [ ! -f "$cdir/pane" ]
}

@test "deregister removes the pane file with the crew dir" {
  cdir="$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/c1"
  (exit 0) & dead_pid=$!
  wait "$dead_pid" 2>/dev/null || true
  CREW_ID=c1 TMUX_PANE='%12' run_crew register "$dead_pid"
  CREW_ID=c1 run_crew deregister
  [ ! -e "$cdir" ]
}

@test "inbox: does not deliver a metrics-addressed message to the dispatcher" {
  CREW_ID=c1 run_crew msg worker "metrics:c1" '{"tier":"deep"}'
  CREW_ID=c1 run run_crew inbox "dispatcher:c1"
  [ "$status" -eq 0 ]
  [[ "$output" != *"deep"* ]]
}

@test "rate: projects replanned booleans and legacy null while retaining rework count" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  cat >"$log" <<'EOF'
{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/replan-true","engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"replans"}
{"ts":1001,"crew_id":"c1","kind":"status","from":"worker:feat/replan-true","body":{"state":"done"}}
{"ts":1002,"crew_id":"c1","kind":"msg","from":"worker:feat/replan-true","to":"metrics:c1","body":"{\"replanned\":true,\"rework_count\":3}"}
{"ts":2000,"crew_id":"c1","kind":"dispatch","branch":"feat/replan-false","engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"does not replan"}
{"ts":2001,"crew_id":"c1","kind":"status","from":"worker:feat/replan-false","body":{"state":"done"}}
{"ts":2002,"crew_id":"c1","kind":"msg","from":"worker:feat/replan-false","to":"metrics:c1","body":"{\"replanned\":false,\"rework_count\":1}"}
{"ts":3000,"crew_id":"c1","kind":"dispatch","branch":"feat/replan-legacy","engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"legacy metrics"}
{"ts":3001,"crew_id":"c1","kind":"status","from":"worker:feat/replan-legacy","body":{"state":"done"}}
{"ts":3002,"crew_id":"c1","kind":"msg","from":"worker:feat/replan-legacy","to":"metrics:c1","body":"{\"rework_count\":2}"}
EOF
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"

  run run_crew rate
  [ "$status" -eq 0 ]

  run jq -s -e '
    map({branch, replanned, rework_count, replanned_present: has("replanned")})
    | sort_by(.branch) == [
        {branch:"feat/replan-false", replanned:false, rework_count:1, replanned_present:true},
        {branch:"feat/replan-legacy", replanned:null, rework_count:2, replanned_present:true},
        {branch:"feat/replan-true", replanned:true, rework_count:3, replanned_present:true}
      ]
  ' "$XDG_DATA_HOME/crew/ratings.jsonl"
  [ "$status" -eq 0 ]
}

@test "reap: rejects an unknown flag" {
  CREW_ID=c1 run run_crew reap --bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"reap takes --quiet, --dry-run and --idle S"* ]]
}

@test "reap: is a no-op when the bus has no events" {
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
}

@test "reap: --dry-run removes no worktree" {
  # A candidate only enters reap's sweep when `from` carries the real
  # `worker:<branch>` convention (WORKER_PROTOCOL.md: `n worker:<branch> done`)
  # AND a worktree for that branch actually exists AND its PR reads back as
  # merged/closed — a bare `status worker done` (no prefix, no worktree, no
  # PR) is filtered out before reap ever calls gh or wt, so the stub log is
  # never created and the original assertion errors on a missing file rather
  # than proving anything about --dry-run. Wire up a real candidate so the
  # test exercises the actual dry-run gate.
  git commit -q --allow-empty -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
echo MERGED
exit 0
EOF
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/reap-me" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/reap-me"* ]]
  run grep -c 'remove' "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "occupants: reports a worker window with a live engine" {
  stub_tmux "$(printf '@23\tsage\t/wt/a\n@9\t\t/wt/a\n')" "$(printf '@23\t%%33\tclaude\n')"
  run run_crew occupants /wt/a
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].window')" = "@23" ]
  [ "$(echo "$output" | jq -r '.[0].name')" = "sage" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "%33" ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "true" ]
}

@test "occupants: a nix-wrapped claude pane is an engine" {
  stub_tmux "$(printf '@1\tsage\t/wt/a\n')" "$(printf '@1\t%%1\t.claude-wrapped\n')"
  run run_crew occupants /wt/a
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "true" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "%1" ]
}

@test "occupants: a cursor-agent pane reports node and is an engine" {
  stub_tmux "$(printf '@1\tsage\t/wt/a\n')" "$(printf '@1\t%%1\tnode\n')"
  run run_crew occupants /wt/a
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "true" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "%1" ]
}

# Measured on a live Nix pane (`tmux display -p '#{pane_current_command}'` on a
# running codex worker): unlike claude/cursor-agent, codex's Nix wrapper
# re-execs under its own literal name, so no wrapper-strip is needed for it.
@test "occupants: a codex pane reports its own literal name and is an engine" {
  stub_tmux "$(printf '@1\tsage\t/wt/a\n')" "$(printf '@1\t%%1\tcodex\n')"
  run run_crew occupants /wt/a
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "true" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "%1" ]
}

@test "occupants: a pi pane reports its own literal name and is an engine" {
  stub_tmux "$(printf '@1\tsage\t/wt/a\n')" "$(printf '@1\t%%1\tpi\n')"
  run run_crew occupants /wt/a
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "true" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "%1" ]
}

@test "occupants: a finished agent that dropped to a shell is still an occupant" {
  stub_tmux "$(printf '@23\tsage\t/wt/a\n')" "$(printf '@23\t%%33\tfish\n')"
  run run_crew occupants /wt/a
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].engine')" = "false" ]
  [ "$(echo "$output" | jq -r '.[0].pane')" = "null" ]
}

@test "occupants: ignores other paths, unnamed windows and the dispatcher" {
  stub_tmux "$(printf '@1\tsage\t/wt/b\n@2\t\t/wt/a\n@3\tdispatcher\t/wt/a\n')" "$(printf '@1\t%%1\tclaude\n@3\t%%3\tclaude\n')"
  run run_crew occupants /wt/a
  [ "$output" = "[]" ]
}

@test "occupants: never reports the caller's own window" {
  stub_tmux "$(printf '@23\tsage\t/wt/a\n')" "$(printf '@23\t%%33\tclaude\n')"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
case "$1" in
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
display-message) printf '%s\n' '@23' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  TMUX_PANE=%33 run run_crew occupants /wt/a
  [ "$output" = "[]" ]
}

@test "occupants: needs a path" {
  run run_crew occupants
  [ "$status" -eq 1 ]
  [[ "$output" == *"occupants <worktree-path>"* ]]
}

@test "engine-cmd: claude is a live engine" {
  run run_crew engine-cmd claude
  [ "$status" -eq 0 ]
}

@test "engine-cmd: a nix-wrapped claude pane is a live engine" {
  run run_crew engine-cmd .claude-wrapped
  [ "$status" -eq 0 ]
}

@test "engine-cmd: node (cursor-agent's wrapper) is a live engine" {
  run run_crew engine-cmd node
  [ "$status" -eq 0 ]
}

@test "engine-cmd: codex is a live engine" {
  run run_crew engine-cmd codex
  [ "$status" -eq 0 ]
}

@test "engine-cmd: pi is a live engine" {
  run run_crew engine-cmd pi
  [ "$status" -eq 0 ]
}

@test "engine-cmd: a plain shell is not an engine" {
  run run_crew engine-cmd fish
  [ "$status" -eq 1 ]
}

@test "engine-cmd: needs a command" {
  run run_crew engine-cmd
  [ "$status" -eq 1 ]
  [[ "$output" == *"engine-cmd <pane_current_command>"* ]]
}

# Ambient pi dir with one entry per auth.json case the seeder distinguishes.
_pi_fixture() {
  export HOME="$BATS_TEST_TMPDIR/home"
  unset PI_CODING_AGENT_DIR
  AMBIENT="$HOME/.pi/agent"
  WORKER="$HOME/.pi/dispatcher-worker"
  mkdir -p "$AMBIENT"
  cat >"$AMBIENT/auth.json" <<'EOF'
{
  "opencode": {"type": "api_key", "key": "SECRET-FIXTURE-123"},
  "openrouter": {"type": "api_key", "key": "$OPENROUTER_API_KEY", "env": {"X": "y"}},
  "deepseek": {"type": "api_key", "key": "!echo x"},
  "anthropic": {"type": "oauth", "access": "a", "refresh": "r", "expires": 1},
  "a b": {"type": "api_key", "key": "bad-name"},
  "lit": {"type": "api_key", "key": "sk-a$b"}
}
EOF
  printf '{"defaultProjectTrust":"always"}\n' >"$AMBIENT/settings.json"
  printf '{}\n' >"$AMBIENT/trust.json"
  printf '{"models":[{"id":"deepseek/deepseek-v4.1-flash"}]}\n' >"$AMBIENT/models-store.json"
}

@test "pi-agent-dir: prints only the worker dir and seeds never-trust settings" {
  _pi_fixture
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 1 ]
  [ "$output" = "$WORKER" ]
  [ -d "$WORKER" ]
  [ "$(stat -c %a "$WORKER")" = 700 ]
  [ "$(jq -r .defaultProjectTrust "$WORKER/settings.json")" = never ]
}

@test "pi-agent-dir: auth links to the ambient file, never copies it" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  [ -L "$WORKER/auth.json" ]
  [ "$(readlink "$WORKER/auth.json")" = "$AMBIENT/auth.json" ]
  # A copy is what leaked a secret into the worker dir; a link holds a path.
  run grep -rF SECRET-FIXTURE-123 "$WORKER" --exclude-dir=.
  [ "$status" -eq 1 ]
}

@test "pi-agent-dir: every entry stays reachable through the link, oauth included" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  # The api_key-only filter dropped these, leaving an oauth-only machine with {} (#198).
  [ "$(jq -r .anthropic.type "$WORKER/auth.json")" = oauth ]
  [ "$(jq -r .anthropic.access "$WORKER/auth.json")" = a ]
  [ "$(jq -r .opencode.key "$WORKER/auth.json")" = SECRET-FIXTURE-123 ]
  [ "$(jq -c .openrouter "$WORKER/auth.json")" = '{"type":"api_key","key":"$OPENROUTER_API_KEY","env":{"X":"y"}}' ]
}

@test "pi-agent-dir: a pi-side token refresh writes through to the ambient file" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  # pi rewrites auth.json in place on refresh; through the link that must land
  # in the ambient file, not strand a divergent copy in the shared worker dir.
  jq '.anthropic.access = "refreshed"' "$WORKER/auth.json" >"$WORKER/auth.next"
  cat "$WORKER/auth.next" >"$WORKER/auth.json"
  rm "$WORKER/auth.next"
  [ -L "$WORKER/auth.json" ]
  [ "$(jq -r .anthropic.access "$AMBIENT/auth.json")" = refreshed ]
}

@test "pi-agent-dir: the ambient dir is untouched and trust.json is not seeded" {
  _pi_fixture
  before=$(sha256sum "$AMBIENT"/*)
  mode=$(stat -c %a "$AMBIENT/auth.json")
  run_crew pi-agent-dir >/dev/null
  [ "$(sha256sum "$AMBIENT"/*)" = "$before" ]
  [ "$(stat -c %a "$AMBIENT/auth.json")" = "$mode" ]
  [ ! -e "$WORKER/trust.json" ]
}

@test "pi-agent-dir: the model catalog is copied, not linked" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  # Workers share this dir; a link would aim N concurrent refreshes at the real catalog.
  [ ! -L "$WORKER/models-store.json" ]
  [ "$(jq -r '.models[0].id' "$WORKER/models-store.json")" = deepseek/deepseek-v4.1-flash ]
  [ "$(stat -c %a "$WORKER/models-store.json")" = 644 ]
}

@test "pi-agent-dir: a re-seed refreshes a stale or corrupted catalog copy" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  printf 'torn{\n' >"$WORKER/models-store.json"
  run_crew pi-agent-dir >/dev/null
  [ "$(jq -r '.models[0].id' "$WORKER/models-store.json")" = deepseek/deepseek-v4.1-flash ]
}

@test "pi-agent-dir: no ambient catalog leaves the worker without one" {
  _pi_fixture
  rm "$AMBIENT/models-store.json"
  run_crew pi-agent-dir >/dev/null
  [ ! -e "$WORKER/models-store.json" ]
}

@test "pi-agent-dir: re-seed keeps pi-written settings and forces never" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  printf '{"lastChangelogVersion":"9","defaultProjectTrust":"always"}\n' >"$WORKER/settings.json"
  run_crew pi-agent-dir >/dev/null
  [ "$(jq -r .lastChangelogVersion "$WORKER/settings.json")" = 9 ]
  [ "$(jq -r .defaultProjectTrust "$WORKER/settings.json")" = never ]
}

@test "pi-agent-dir: a no-op re-seed leaves auth in place and no temp files" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  before=$(stat -c '%i %Y' "$WORKER/auth.json" "$WORKER/settings.json")
  run_crew pi-agent-dir >/dev/null
  [ "$(stat -c '%i %Y' "$WORKER/auth.json" "$WORKER/settings.json")" = "$before" ]
  [ -z "$(find "$WORKER" -name '.seed.*')" ]
}

@test "pi-agent-dir: seeds the ambient hookyard bridge and registers it" {
  _pi_fixture
  mkdir -p "$AMBIENT/bin"
  printf '// hookyard bridge\n' >"$AMBIENT/bin/hookyard-bridge.ts"
  run_crew pi-agent-dir >/dev/null
  [ -f "$WORKER/bin/hookyard-bridge.ts" ]
  cmp -s "$AMBIENT/bin/hookyard-bridge.ts" "$WORKER/bin/hookyard-bridge.ts"
  [ "$(jq -r '.extensions[0]' "$WORKER/settings.json")" = "$WORKER/bin/hookyard-bridge.ts" ]
  [ "$(jq -r .defaultProjectTrust "$WORKER/settings.json")" = never ]
}

@test "pi-agent-dir: no ambient hookyard bridge leaves the worker unhooked" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  [ ! -e "$WORKER/bin/hookyard-bridge.ts" ]
  [ "$(jq -c '.extensions // "absent"' "$WORKER/settings.json")" = '"absent"' ]
}

@test "pi-agent-dir: a re-seed appends the bridge once and keeps a pi-written order" {
  _pi_fixture
  mkdir -p "$AMBIENT/bin"
  printf '// hookyard bridge\n' >"$AMBIENT/bin/hookyard-bridge.ts"
  run_crew pi-agent-dir >/dev/null
  # pi (or a user) may prepend its own extension; a reseed must not reorder the
  # list nor duplicate hookyard's entry.
  printf '{"extensions":["/some/other.ts","%s"]}\n' "$WORKER/bin/hookyard-bridge.ts" >"$WORKER/settings.json"
  run_crew pi-agent-dir >/dev/null
  [ "$(jq -c .extensions "$WORKER/settings.json")" = "[\"/some/other.ts\",\"$WORKER/bin/hookyard-bridge.ts\"]" ]
  [ -z "$(find "$WORKER" -name '.seed.*')" ]
}

@test "pi-agent-dir: a non-array extensions value is refused, not a jq crash" {
  _pi_fixture
  mkdir -p "$AMBIENT/bin"
  printf '// hookyard bridge\n' >"$AMBIENT/bin/hookyard-bridge.ts"
  run_crew pi-agent-dir >/dev/null
  # A hand-edited/future-schema settings.json must refuse legibly, not die on an
  # opaque jq `cannot be added` and take dispatch/dispatch-resume down with it.
  # `false` is included: jq's `//` would fold it into null and slip the guard.
  for bad in '{"not":"an array"}' 'false' '"a string"' '3'; do
    printf '{"extensions":%s}\n' "$bad" >"$WORKER/settings.json"
    run --separate-stderr run_crew pi-agent-dir
    [ "$status" -ne 0 ]
    [ -z "$output" ]
    [[ "$stderr" == *"non-array extensions value"* ]]
    [ "$(jq -c .extensions "$WORKER/settings.json")" = "$bad" ]
  done
}

@test "pi-agent-dir: no ambient auth.json seeds an empty auth" {
  _pi_fixture
  rm "$AMBIENT/auth.json"
  run_crew pi-agent-dir >/dev/null
  [ "$(jq -c . "$WORKER/auth.json")" = '{}' ]
}

# Seed once, break the ambient auth.json with $1, and expect a refusal. The
# worker file is a link, so "kept" is about the link, not its content.
_pi_broken_auth() {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  printf '%s\n' "$1" >"$AMBIENT/auth.json"
}

_pi_assert_link_kept() {
  [ -L "$WORKER/auth.json" ]
  [ "$(readlink "$WORKER/auth.json")" = "$AMBIENT/auth.json" ]
}

@test "pi-agent-dir: a malformed ambient auth.json is refused, worker link kept" {
  _pi_broken_auth '{"opencode":{"ty'
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"$AMBIENT/auth.json is unreadable or not a JSON object"* ]]
  _pi_assert_link_kept
}

@test "pi-agent-dir: a non-object ambient auth.json is refused" {
  _pi_broken_auth '[]'
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"not a JSON object"* ]]
  _pi_assert_link_kept
}

@test "pi-agent-dir: an unreadable ambient auth.json is refused" {
  [ "$(id -u)" -eq 0 ] && skip "root reads mode 000 files"
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  chmod 000 "$AMBIENT/auth.json"
  run --separate-stderr run_crew pi-agent-dir
  chmod 600 "$AMBIENT/auth.json"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"unreadable or not a JSON object"* ]]
  _pi_assert_link_kept
}

@test "pi-agent-dir: a worker dir symlinked to the ambient dir is refused" {
  _pi_fixture
  ln -s "$AMBIENT" "$WORKER"
  before=$(sha256sum "$AMBIENT/auth.json" "$AMBIENT/settings.json")
  mode=$(stat -c %a "$AMBIENT")
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"symlink or not a directory"* ]]
  [ "$(sha256sum "$AMBIENT/auth.json" "$AMBIENT/settings.json")" = "$before" ]
  [ "$(stat -c %a "$AMBIENT")" = "$mode" ]
}

@test "pi-agent-dir: a worker dir that is a regular file is refused" {
  _pi_fixture
  printf 'x\n' >"$WORKER"
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [ -z "$output" ]
  [ "$(cat "$WORKER")" = x ]
}

@test "pi-agent-dir: a PI_CODING_AGENT_DIR aliasing the worker dir is refused" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  ln -s "$WORKER" "$HOME/alias"
  before=$(sha256sum "$WORKER/auth.json")
  PI_CODING_AGENT_DIR="$HOME/alias" run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"refusing to seed pi worker dir"* ]]
  [ "$(sha256sum "$WORKER/auth.json")" = "$before" ]
}

@test "pi-agent-dir: a relative PI_CODING_AGENT_DIR falls back to ~/.pi/agent" {
  _pi_fixture
  mkdir -p rel
  printf '{"opencode":{"type":"api_key","key":"!echo rel"}}\n' >rel/auth.json
  PI_CODING_AGENT_DIR=rel run_crew pi-agent-dir >/dev/null
  [ "$(readlink "$WORKER/auth.json")" = "$AMBIENT/auth.json" ]
}

# Seed once, then drop the ambient auth.json so the test can put a non-regular
# entry (or nothing reachable) in its place.
_pi_seeded() {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  rm "$AMBIENT/auth.json"
}

# The timeout turns a seeder blocked on open() into a failure instead of a hung suite.
_pi_run_seed() {
  run --separate-stderr timeout 10 bash -euo pipefail "$CREW" pi-agent-dir
}

_pi_assert_refused() {
  [ "$status" -ne 0 ]
  [ "$status" -ne 124 ]
  [ -z "$output" ]
  [[ "$stderr" == *"refusing to seed pi credentials"* ]]
  _pi_assert_link_kept
}

@test "pi-agent-dir: a dangling ambient auth.json symlink is refused, worker auth kept" {
  _pi_seeded
  ln -s "$HOME/unmounted/auth.json" "$AMBIENT/auth.json"
  _pi_run_seed
  _pi_assert_refused
}

@test "pi-agent-dir: a looping ambient auth.json symlink is refused" {
  _pi_seeded
  ln -s auth.json "$AMBIENT/auth.json"
  _pi_run_seed
  _pi_assert_refused
}

@test "pi-agent-dir: an ambient auth.json symlinked to a directory is refused" {
  _pi_seeded
  mkdir "$AMBIENT/d"
  ln -s d "$AMBIENT/auth.json"
  _pi_run_seed
  _pi_assert_refused
}

@test "pi-agent-dir: a FIFO ambient auth.json is refused without blocking" {
  _pi_seeded
  mkfifo "$AMBIENT/auth.json"
  _pi_run_seed
  _pi_assert_refused
}

@test "pi-agent-dir: an ambient auth.json behind an unsearchable dir is refused" {
  [ "$(id -u)" -eq 0 ] && skip "root searches mode 000 dirs"
  _pi_seeded
  mkdir "$HOME/locked"
  printf '{}\n' >"$HOME/locked/auth.json"
  ln -s "$HOME/locked/auth.json" "$AMBIENT/auth.json"
  chmod 000 "$HOME/locked"
  _pi_run_seed
  # Before any assertion, so a failure cannot leave a dir teardown's rm -rf trips on.
  chmod 700 "$HOME/locked"
  _pi_assert_refused
}

@test "pi-agent-dir: a dangling ambient dir symlink is refused" {
  _pi_seeded
  rm -r "$AMBIENT"
  ln -s "$HOME/unmounted" "$AMBIENT"
  _pi_run_seed
  _pi_assert_refused
}

# "weird" is a real searchable dir; "weird\n" is a dangling symlink one level
# up in the real (uncorrupted) ancestor walk — see crew.sh's dirname comment.
@test "pi-agent-dir: an ambient path component ending in a newline is refused, not stripped" {
  _pi_seeded
  mkdir -p "$HOME/.pi/weird"
  ln -s "$HOME/unmounted" "$HOME/.pi/weird"$'\n'
  export PI_CODING_AGENT_DIR="$HOME/.pi/weird"$'\n'"/agent"
  _pi_run_seed
  _pi_assert_refused
}

@test "pi-agent-dir: a missing ambient dir seeds an empty auth" {
  _pi_fixture
  rm -r "$AMBIENT"
  run_crew pi-agent-dir >/dev/null
  [ "$(jq -c . "$WORKER/auth.json")" = '{}' ]
}

@test "pi-agent-dir: a relative ambient auth.json symlink still resolves" {
  _pi_fixture
  mkdir -p "$HOME/.pi/secrets"
  mv "$AMBIENT/auth.json" "$HOME/.pi/secrets/auth.json"
  ln -s ../secrets/auth.json "$AMBIENT/auth.json"
  run_crew pi-agent-dir >/dev/null
  # The worker link is absolute, so the ambient link's relative target resolves
  # against the ambient dir — not the worker dir and not pi's cwd.
  [ "$(readlink "$WORKER/auth.json")" = "$AMBIENT/auth.json" ]
  [ "$(cd / && jq -r .opencode.key "$WORKER/auth.json")" = SECRET-FIXTURE-123 ]
}

@test "sessions: folds each session separately, oldest first" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  run run_crew sessions feat/x
  [ "$status" -eq 0 ]
  [ "$(echo "$output" | jq -r 'length')" = "2" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s1-1" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "done" ]
  [ "$(echo "$output" | jq -r '.[0].terminal')" = "true" ]
  [ "$(echo "$output" | jq -r '.[1].session')" = "s2-2" ]
  [ "$(echo "$output" | jq -r '.[1].state')" = "working" ]
  [ "$(echo "$output" | jq -r '.[1].terminal')" = "false" ]
  [ "$(echo "$output" | jq -r '.[1].worker_id')" = "worker:feat/x#s2-2" ]
}

@test "sessions: pr_open is not terminal" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r '.[0].terminal')" = "false" ]
}

@test "sessions: a branch with a '#' in its name folds on the last '#'" {
  CREW_ID=c1 run_crew status "worker:feat/a#b#s1-1" working
  run run_crew sessions 'feat/a#b'
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s1-1" ]
}

@test "sessions: legacy branch-keyed events fold in as a null session" {
  CREW_ID=c1 run_crew status "worker:feat/x" done
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r '.[0].session')" = "null" ]
  [ "$(echo "$output" | jq -r '.[0].worker_id')" = "worker:feat/x" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "done" ]
}

@test "sessions: a dispatched session with no status yet has a null state" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:(now*1000|floor), crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s9-9"}' >>"$log"
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s9-9" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "null" ]
  [ "$(echo "$output" | jq -r '.[0].terminal')" = "false" ]
}

@test "sessions: a watchdog's session-less row folds into the live session" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" working "" "" "$t"
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %9" watchdog "$((t + 1000))"
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s1-1" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "blocked" ]
  [ "$(echo "$output" | jq -r '.[0].worker_id')" = "worker:feat/x#s1-1" ]
}

@test "sessions: a session-less heartbeat after a newer dispatch folds into that session" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" done "" "" "$t"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc --argjson ts "$((t + 1000))" '{ts:$ts, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s2-2"}' >>"$log"
  seed_raw worker:feat/x working "" "" "$((t + 2000))"
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r 'length')" = "2" ]
  [ "$(echo "$output" | jq -r 'last.session')" = "s2-2" ]
  [ "$(echo "$output" | jq -r 'last.state')" = "working" ]
}

@test "sessions: a session-less row on a '#' branch keys on the full branch" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/a#b#s1-1" working "" "" "$t"
  seed_raw "worker:feat/a#b" blocked "" watchdog "$((t + 1000))"
  run run_crew sessions 'feat/a#b'
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s1-1" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "blocked" ]
}

@test "sessions: a session-less row after a terminal session does not revive it" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" done "" "" "$t"
  seed_raw worker:feat/x working "" "" "$((t + 1000))"
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r 'length')" = "2" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s1-1" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "done" ]
  [ "$(echo "$output" | jq -r 'last.session')" = "null" ]
}

@test "sessions: a session-less row in the same millisecond as a terminal session does not revive it" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" done "" "" "$t"
  seed_raw worker:feat/x working "" "" "$t"
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r 'length')" = "2" ]
  [ "$(echo "$output" | jq -r '.[] | select(.session == "s1-1") | .state')" = "done" ]
  [ "$(echo "$output" | jq -r '[.[] | select(.session == "s1-1" and .terminal)] | length')" = "1" ]
  [ "$(echo "$output" | jq -r '[.[] | select(.session != null and .state == "working")] | length')" = "0" ]
}

@test "sessions: a resume row starts its session" {
  t=$(($(date +%s) * 1000))
  seed_start dispatch s1-1 "$t"
  seed_raw "worker:feat/x#s1-1" working "" "" "$((t + 1000))"
  seed_start resume s2-2 "$((t + 2000))"
  seed_raw worker:feat/x blocked "" "" "$((t + 3000))"
  run run_crew sessions feat/x
  [ "$(echo "$output" | jq -r 'length')" = "2" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "working" ]
  [ "$(echo "$output" | jq -r 'last.session')" = "s2-2" ]
  [ "$(echo "$output" | jq -r 'last.state')" = "blocked" ]
}

@test "sessions: --crew scopes the fold" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c2 run_crew status "worker:feat/x#s2-2" working
  run run_crew sessions feat/x --crew c2
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s2-2" ]
}

@test "sessions: an unknown branch is an empty array" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  run run_crew sessions feat/nope
  [ "$output" = "[]" ]
}

@test "sessions: needs a branch" {
  run run_crew sessions
  [ "$status" -eq 1 ]
  [[ "$output" == *"sessions <branch>"* ]]
}

@test "reply: a branch-only worker target resolves to the newest live session" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  CREW_ID=c1 run_crew reply "worker:feat/x" "go"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "worker:feat/x#s2-2" ]
}

@test "reply: refuses when the newest session is terminal" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"re-dispatch"* ]]
}

@test "reply: CREW_ID unset resolves the single crew with a live session (#302)" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  run env -u CREW_ID bash -euo pipefail "$CREW" reply "worker:feat/x" "go"
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | "\(.crew_id) \(.to)"' "$log"
  [ "$output" = "c1 worker:feat/x#s1-1" ]
}

@test "reply: --crew works with CREW_ID unset (#302)" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c2 run_crew status "worker:feat/x#s2-2" working
  run env -u CREW_ID bash -euo pipefail "$CREW" reply "worker:feat/x" "go" --crew c2
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "worker:feat/x#s2-2" ]
}

@test "reply: CREW_ID unset with live sessions in two crews refuses naming both (#302)" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c2 run_crew status "worker:feat/x#s2-2" working
  run env -u CREW_ID bash -euo pipefail "$CREW" reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"c1"* && "$output" == *"c2"* && "$output" == *"--crew"* ]]
}

@test "reply: CREW_ID unset with only a terminal session names the unset crew (#302)" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  run env -u CREW_ID bash -euo pipefail "$CREW" reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"CREW_ID not set"* ]]
}

@test "reply: CREW_ID unset ignores a crashed session in an old crew (#327)" {
  # old crew: a registered dead pid with a non-terminal session that only
  # lingers because the crew crashed before posting a terminal status.
  (exit 0) & dead_pid=$!
  wait "$dead_pid" 2>/dev/null || true
  CREW_ID=c-old run_crew register "$dead_pid"
  CREW_ID=c-old run_crew status "worker:feat/x#s1-1" working
  # current crew: a live registered pid and a newer live session on the branch.
  CREW_ID=c-cur run_crew register "$$"
  CREW_ID=c-cur run_crew status "worker:feat/x#s2-2" working
  run env -u CREW_ID bash -euo pipefail "$CREW" reply "worker:feat/x" "go"
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | "\(.crew_id) \(.to)"' "$log"
  [ "$output" = "c-cur worker:feat/x#s2-2" ]
}

@test "reply: CREW_ID unset still refuses two genuinely live crews (#327)" {
  sleep 30 & live_a=$!
  sleep 30 & live_b=$!
  CREW_ID=c1 run_crew register "$live_a"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c2 run_crew register "$live_b"
  CREW_ID=c2 run_crew status "worker:feat/x#s2-2" working
  run env -u CREW_ID bash -euo pipefail "$CREW" reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"c1"* && "$output" == *"c2"* && "$output" == *"--crew"* ]]
  kill "$live_a" "$live_b" 2>/dev/null || true
  wait "$live_a" "$live_b" 2>/dev/null || true
}

@test "reply: CREW_ID unset with only crashed crews still refuses naming both (#327)" {
  # Every candidate's crew is dead: with no live crew to prefer, the fail-safe
  # is to keep both and refuse, never to guess one.
  (exit 0) & dead_a=$!
  wait "$dead_a" 2>/dev/null || true
  (exit 0) & dead_b=$!
  wait "$dead_b" 2>/dev/null || true
  CREW_ID=c1 run_crew register "$dead_a"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c2 run_crew register "$dead_b"
  CREW_ID=c2 run_crew status "worker:feat/x#s2-2" working
  run env -u CREW_ID bash -euo pipefail "$CREW" reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"c1"* && "$output" == *"c2"* && "$output" == *"--crew"* ]]
}

@test "reply: refuses a branch with no sessions" {
  CREW_ID=c1 run run_crew reply "worker:feat/nope" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"no session"* ]]
}

@test "reply: an explicit session id is honoured verbatim" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew reply "worker:feat/x#s1-1" "go"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "worker:feat/x#s1-1" ]
}

@test "reply: a non-worker target is untouched" {
  CREW_ID=c1 run_crew reply "metrics:c1" "{}"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "metrics:c1" ]
}

@test "reply: a directive for session 1 is not delivered to session 2" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew reply "worker:feat/x" "STOP - do not push"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  run run_crew inbox "worker:feat/x#s2-2" c1
  [ -z "$output" ]
  run run_crew inbox "worker:feat/x#s1-1" c1
  [[ "$output" == *"STOP - do not push"* ]]
}

@test "reply: branch-only address where branch contains '#' resolves via _sessions (not treated as sid)" {
  local branch="feat/12-a#b"
  local sid="s1234567890-12345"
  local worker_id="worker:${branch}#${sid}"

  # seed a live session for the branch
  CREW_ID=c1 run_crew status "$worker_id" working
  CREW_ID=c1 run_crew reply "worker:${branch}" "go"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "$worker_id" ]
}

@test "reply: a session-less watchdog post after the live session does not strand the reply (#173)" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1789446258-532666" working
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %186" watchdog "$(($(date +%s) * 1000 + 1000))"
  CREW_ID=c1 run_crew reply "worker:feat/x" "ship it after the fix"
  run run_crew inbox "worker:feat/x#s1789446258-532666" c1
  [[ "$output" == *"ship it after the fix"* ]]
}

@test "reply: a session-less failed after a live session refuses as terminal" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" working "" "" "$t"
  seed_raw worker:feat/x failed "dead: quiet: unchanged for 1800s" watchdog "$((t + 1000))"
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ -z "$output" ]
}

@test "reply: a branch with only session-less non-terminal rows exits non-zero and writes nothing" {
  seed_raw worker:feat/x working "" ""
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  [[ "$output" == *"no session id"* ]]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ -z "$output" ]
}

@test "reply: a session-less heartbeat after a finished session refuses" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" done "" "" "$t"
  seed_raw worker:feat/x working "" "" "$((t + 1000))"
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ -z "$output" ]
}

@test "reply: a session-less row in the same millisecond as a finished session refuses" {
  t=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" done "" "" "$t"
  seed_raw worker:feat/x working "" "" "$t"
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 1 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ -z "$output" ]
}

@test "reply: a session-less row after a resume row reaches the resumed session" {
  t=$(($(date +%s) * 1000))
  seed_start dispatch s1-1 "$t"
  seed_raw "worker:feat/x#s1-1" working "" "" "$((t + 1000))"
  seed_start resume s2-2 "$((t + 2000))"
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %9" watchdog "$((t + 3000))"
  CREW_ID=c1 run_crew reply "worker:feat/x" "resume-directive"
  run run_crew inbox "worker:feat/x#s2-2" c1
  [[ "$output" == *"resume-directive"* ]]
}

@test "await: a branch-only worker id exits non-zero" {
  CREW_ID=c1 run run_crew await "worker:feat/x" --timeout 1
  [ "$status" -eq 1 ]
  [[ "$output" == *"no session suffix"* ]]
}

@test "await: a sessioned id still receives a reply" {
  (
    sleep 1
    CREW_ID=c1 bash -euo pipefail "$CREW" reply "worker:feat/x#s1-1" hi
  ) >/dev/null 2>&1 &
  CREW_ID=c1 run run_crew await "worker:feat/x#s1-1" --timeout 5 --interval 1
  wait
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"hi"'* ]]
}

# #240: a reply appended after the worker's question but before `crew await`
# started was hidden behind await's own `start=now` cursor and lost. The wait now
# anchors on the session's own latest outbound question *per counterpart*, so a
# reply that landed in that gap is still delivered.
@test "await: a reply that landed before await starts is delivered" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
}

# The anchor is per counterpart, not "latest outbound to anyone": a later
# outbound to a third party must not hide an earlier reply from the dispatcher.
@test "await: a later outbound to a third party does not hide an earlier reply" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "answer"
  sleep 1
  CREW_ID=c1 run_crew msg "$id" "worker:feat/y#s2-2" "unrelated"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
}

@test "await: an answer to an earlier question is not redelivered after a newer question" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q1"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "A1"
  sleep 1
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q2"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "A2"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"A2"'* ]]
}

# #290: a reply await has already handed this session is not handed back by a
# later await. The delivered mark lives per session and per sender, so a fresh
# question to the same counterpart (or a reply from anyone else) still delivers.
@test "await: a second await with no new question does not re-deliver the reply" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "await: a delivered critic verdict is not re-delivered while awaiting the next critic" {
  id="worker:feat/x#s1-1"
  spec="role:feat/x:spec-critic"
  plan="role:feat/x:plan-critic"
  CREW_ID=c1 run_crew msg "$id" "$spec" "review spec"
  sleep 1
  CREW_ID=c1 run_crew msg "$spec" "$id" "spec verdict"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"spec verdict"'* ]]
  sleep 1
  CREW_ID=c1 run_crew msg "$id" "$plan" "review plan"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  sleep 1
  CREW_ID=c1 run_crew msg "$plan" "$id" "plan verdict"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"plan verdict"'* ]]
}

@test "await: a delivered dispatcher reply is not re-delivered after critic traffic" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer"'* ]]
  sleep 1
  CREW_ID=c1 run_crew msg "$id" "role:feat/x:plan-critic" "review plan"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ -z "$output" ]
}

# The mark is per sender: delivering A's reply must not swallow B's reply that
# landed earlier and is still undelivered (parallel assignments, #240).
@test "await: delivering one sender's reply leaves another sender's earlier reply deliverable" {
  id="worker:feat/x#s1-1"
  a="role:feat/x:spec-critic"
  b="role:feat/x:reviewer"
  CREW_ID=c1 run_crew msg "$id" "$a" "qa"
  CREW_ID=c1 run_crew msg "$id" "$b" "qb"
  sleep 1
  CREW_ID=c1 run_crew msg "$b" "$id" "vb"
  sleep 1
  CREW_ID=c1 run_crew msg "$a" "$id" "va"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"va"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"vb"'* ]]
}

# #300: a lead waiting on one role's verdict must not be released by another
# sender's reply. --from restricts the candidates to one exact sender; the other
# sender's msg is left undelivered for a later plain await.
@test "await --from: another sender's newer-so-far reply neither returns nor is consumed" {
  id="worker:feat/x#s1-1"
  plan="role:feat/x:plan-critic"
  rev="role:feat/x:reviewer"
  CREW_ID=c1 run_crew msg "$id" "$plan" "review plan"
  CREW_ID=c1 run_crew msg "$id" "$rev" "review diff"
  sleep 1
  CREW_ID=c1 run_crew msg "$plan" "$id" "plan verdict"
  (
    sleep 2
    CREW_ID=c1 bash -euo pipefail "$CREW" msg "$rev" "$id" "review verdict"
  ) >/dev/null 2>&1 &
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 5 --interval 1
  wait
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"review verdict"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [[ "$output" == *'"body":"plan verdict"'* ]]
}

@test "await --from: a timeout names the sender and exits 0 with empty stdout" {
  id="worker:feat/x#s1-1"
  plan="role:feat/x:plan-critic"
  rev="role:feat/x:reviewer"
  CREW_ID=c1 run_crew msg "$id" "$plan" "review plan"
  sleep 1
  CREW_ID=c1 run_crew msg "$plan" "$id" "plan verdict"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"$rev"* ]]
}

@test "await --from: a reply older than this session's question to that sender is not returned" {
  id="worker:feat/x#s1-1"
  rev="role:feat/x:reviewer"
  CREW_ID=c1 run_crew msg "$rev" "$id" "stale verdict"
  sleep 1
  CREW_ID=c1 run_crew msg "$id" "$rev" "review diff"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "await --from: requires a value" {
  CREW_ID=c1 run --separate-stderr run_crew await "worker:feat/x#s1-1" --from
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"--from needs a value"* ]]
}

# The straggler fold (`crew inbox --since`) is how a reply that missed a timed-out
# await is taken; it must count as delivered too, or the next await hands it back.
@test "await: a reply taken through the inbox fold is not re-delivered by the next await" {
  id="worker:feat/x#s1-1"
  spec="role:feat/x:spec-critic"
  plan="role:feat/x:plan-critic"
  CREW_ID=c1 run_crew msg "$id" "$spec" "review spec"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ -z "$output" ]
  sleep 1
  CREW_ID=c1 run_crew msg "$spec" "$id" "spec verdict"
  CREW_ID=c1 run --separate-stderr run_crew inbox "$id" c1 --since 0
  [[ "$output" == *'"body":"spec verdict"'* ]]
  sleep 1
  CREW_ID=c1 run_crew msg "$id" "$plan" "review plan"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ -z "$output" ]
}

# A torn or garbage delivered-marks file must not blind await: it reads as empty
# and the reply is still delivered.
@test "await: an unreadable delivered-marks file does not hide a reply" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer"'* ]]
  state=$(git rev-parse --path-format=absolute --git-common-dir)/crew/await
  for f in "$state"/*; do printf '{"dispatcher:c1":5}"x":6}' >"$f"; done
  sleep 1
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why2?"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "answer2"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer2"'* ]]
}

# Delivered state is per session: a resumed session (new id) starts clean.
@test "await: delivered state does not carry to another session" {
  CREW_ID=c1 run_crew msg "worker:feat/x#s1-1" "dispatcher:c1" "why?"
  sleep 1
  CREW_ID=c1 run_crew reply "worker:feat/x#s1-1" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "worker:feat/x#s1-1" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "worker:feat/x#s2-2" --timeout 0
  [ -z "$output" ]
  CREW_ID=c1 run_crew msg "worker:feat/x#s2-2" "dispatcher:c1" "why2?"
  sleep 1
  CREW_ID=c1 run_crew reply "worker:feat/x#s2-2" "answer2"
  CREW_ID=c1 run --separate-stderr run_crew await "worker:feat/x#s2-2" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer2"'* ]]
}

# Sensitive by design: this fails if any status row (watchdog or plain blocked)
# is re-admitted to the anchor computation. A status row is *not* a question, so
# a reply posted before the await must not become deliverable through it.
@test "await: a watchdog-sourced blocked status never anchors the wait" {
  id="worker:feat/x#s1-1"
  seed_raw "$id" blocked "prompt: interactive prompt in pane %9" watchdog
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

# A hard kill mid-append leaves a torn trailing line (crew.sh documents this
# crash mode; tests/crews.bats pins the same tolerance for `crews`). await must
# still read the well-formed reply above it, not abort the whole log read.
@test "await: a torn trailing log line does not hide a reply" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  sleep 1
  CREW_ID=c1 run_crew reply "$id" "answer"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  printf '{"ts":1785951264000,"crew_id":"c-to' >>"$log"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
}

# #186: the WORKER_PROTOCOL "Report to the bus" blocked→await loop keeps a
# blocked worker inside `crew await` in bounded cycles, so a dispatcher reply
# is delivered in-band instead of stranding the worker. These tests pin the
# composition of the crew commands that loop relies on, using `--timeout 0`
# (an instant timeout) and future-dated seeded rows — the fake-clock pattern,
# no real sleeps. The `crew status`/`crew await`/`crew inbox` commands
# themselves are covered by their own tests; here the loop's delivery paths
# and its ending are pinned.

@test "blocked-cycle: a reply arriving in a later cycle is still delivered in-band" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run run_crew status "$id" working
  CREW_ID=c1 run run_crew status "$id" blocked "why?"
  # Cycle 1: the window has nothing in it, so it times out (exit 0, empty
  # stdout — the timeout marker). --separate-stderr: the "await ended" note
  # goes to stderr, so $output is genuinely the reply stream.
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  # `crew await` delivers a reply only when .ts > `start`, where `start` is
  # `now*1000|floor` captured the instant cycle 2's await begins — `t` below
  # uses that same expression, in real ms rather than `date +%s` (whole-second
  # truncation). Delivery is checked on await's first loop iteration, before
  # any sleep, so a margin wider than the actual write-to-snapshot gap costs
  # nothing in test runtime.
  CREW_ID=c1 run run_crew status "$id" blocked "why? (cycle 1 of 24)"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  t=$(jq -nc 'now*1000|floor')
  jq -nc --arg to "$id" --argjson ts "$((t + 2000))" \
    '{ts:$ts, crew_id:"c1", from:"dispatcher:c1", to:$to, kind:"msg", body:"answer"}' >>"$log"
  # Cycle 2: the reply is delivered in-band and the worker resumes in place.
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
  CREW_ID=c1 run run_crew status "$id" working "resumed"
  # No failure was ever posted.
  run jq -sr '[.[] | select(.kind=="status" and .body.state=="failed")] | length' "$log"
  [ "$output" = "0" ]
}

@test "blocked-cycle: a reply that missed the await is caught by the straggler fold" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run run_crew status "$id" working
  CREW_ID=c1 run run_crew status "$id" blocked "why?"
  # Cycle 1 times out empty...
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  # ...and the dispatcher's reply lands after that window closed: it is older
  # than the NEXT cycle's start, so no future `crew await` can deliver it. The
  # worker protocol therefore folds stragglers after every timeout —
  # `crew inbox --since <seen>` — which returns it and resumes the worker.
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  t=$(($(date +%s) * 1000))
  jq -nc --arg to "$id" --argjson ts "$t" \
    '{ts:$ts, crew_id:"c1", from:"dispatcher:c1", to:$to, kind:"msg", body:"answer"}' >>"$log"
  CREW_ID=c1 run --separate-stderr run_crew inbox "$id" c1 --since 0
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
  CREW_ID=c1 run run_crew status "$id" working "resumed"
  run jq -sr '[.[] | select(.kind=="status" and .body.state=="failed")] | length' "$log"
  [ "$output" = "0" ]
}

@test "blocked-cycle: budget exhaustion fails exactly once after every cycle re-stamped blocked" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run run_crew status "$id" working
  CREW_ID=c1 run run_crew status "$id" blocked "why?"
  # A 3-cycle budget on an empty bus: every cycle re-stamps blocked (the
  # per-cycle liveness signal), and the budget's exhaustion posts exactly one
  # failed — never one per cycle, never zero.
  i=1
  while [ "$i" -le 3 ]; do
    CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
    [ "$status" -eq 0 ]
    [ -z "$output" ]
    CREW_ID=c1 run run_crew status "$id" blocked "why? (cycle $i of 24)"
    i=$((i + 1))
  done
  CREW_ID=c1 run run_crew status "$id" failed "blocked, no dispatcher reply"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  # every cycle re-stamped blocked (3 cycle rows), and the budget's
  # exhaustion posted exactly one failed with the canonical detail.
  run jq -sr '[.[] | select(.kind=="status" and .body.state=="blocked") | select(.body.detail | contains("cycle"))] | length' "$log"
  [ "$output" = "3" ]
  run jq -sr '[.[] | select(.kind=="status" and .body.state=="failed")] | length' "$log"
  [ "$output" = "1" ]
  run jq -sr '[.[] | select(.kind=="status" and .body.state=="failed")][0].body.detail' "$log"
  [ "$output" = "blocked, no dispatcher reply" ]
}

# A torn trailing line (hard-kill crash mode) must not blind the straggler fold:
# inbox still prints the msgs it could parse, as it did before it recorded marks.
@test "inbox: a torn log line does not hide the msgs above it" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew reply "$id" "answer"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  printf '{"ts":1785951264000,"crew_id":"c-to' >>"$log"
  CREW_ID=c1 run --separate-stderr run_crew inbox "$id" c1 --since 0
  [[ "$output" == *'"body":"answer"'* ]]
}

@test "inbox: a branch-only worker id exits non-zero" {
  run run_crew inbox "worker:feat/x" c1
  [ "$status" -eq 1 ]
  [[ "$output" == *"no session suffix"* ]]
}

@test "roster: collapses sessions of one branch into a single row" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].branch')" = "feat/x" ]
  [ "$(echo "$output" | jq -r '.[0].state')" = "working" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "s2-2" ]
  [ "$(echo "$output" | jq -r '.[0].sessions | length')" = "2" ]
  [ "$(echo "$output" | jq -r '.[0].sessions[0].state')" = "done" ]
  [ "$(echo "$output" | jq -r '.[0].sessions[1].state')" = "working" ]
}

@test "roster: the codename still derives from the branch" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  expected="$(run_crew identity feat/x | jq -r .name)"
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].name')" = "$expected" ]
}

@test "roster: one session's exited is not resolved from another's history" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s2-2" exited
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].state')" = "exited" ]
  [ "$(echo "$output" | jq -r '.[0].prev_state // "none"')" = "none" ]
}

@test "roster: a false exited with a live wrapped pane resolves to the previous state" {
  git commit --allow-empty -q -m init
  git branch feat/roster-live
  wt_path="$BATS_TEST_TMPDIR/roster-live-wt"
  git worktree add -q "$wt_path" feat/roster-live
  # $BATS_TEST_TMPDIR sits under macOS's /var, a symlink to /private/var; git
  # resolves it but a hardcoded stub path here wouldn't, so canonicalize to
  # match what a real tmux pane_current_path (kernel cwd) always reports.
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_tmux "" "$(printf '.claude-wrapped %s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/roster-live#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/roster-live#s1-1" exited
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].state')" = "working" ]
  [ "$(echo "$output" | jq -r '.[0].exit_suspect')" = "true" ]
}

@test "roster: an exited session with no engine pane stays exited" {
  git commit --allow-empty -q -m init
  git branch feat/roster-dead
  wt_path="$BATS_TEST_TMPDIR/roster-dead-wt"
  git worktree add -q "$wt_path" feat/roster-dead
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_tmux "" "$(printf 'fish %s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/roster-dead#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/roster-dead#s1-1" exited
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].state')" = "exited" ]
  [ "$(echo "$output" | jq -r '.[0].exit_suspect // "none"')" = "none" ]
}

# The path match is exact, never a prefix (crew.sh:64): the old `"claude $wtpath"*`
# glob matched /wt/foobar when $wtpath was /wt/foo. A pane sitting at a sibling
# path that merely shares the prefix must not resolve the exited row.
@test "roster: a live engine at a sibling path does not resolve the exited row" {
  git commit --allow-empty -q -m init
  git branch feat/roster-sib
  wt_path="$BATS_TEST_TMPDIR/roster-sib-wt"
  git worktree add -q "$wt_path" feat/roster-sib
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_tmux "" "$(printf '.claude-wrapped %s-sibling\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/roster-sib#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/roster-sib#s1-1" exited
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].state')" = "exited" ]
  [ "$(echo "$output" | jq -r '.[0].exit_suspect // "none"')" = "none" ]
}

@test "roster: joins the title on the branch" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1", title:"Do a thing"}' >>"$log"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].title')" = "Do a thing" ]
}

@test "roster: joins engine/model/tier on the branch" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1",
           engine:"claude", model:"sonnet", tier:"standard", title:"T"}' >>"$log"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].engine')" = "claude" ]
  [ "$(echo "$output" | jq -r '.[0].model')" = "sonnet" ]
  [ "$(echo "$output" | jq -r '.[0].tier')" = "standard" ]
}

@test "roster: engine/model/tier default to null with no dispatch event" {
  CREW_ID=c1 run_crew status "worker:feat/x" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].engine')" = "null" ]
  [ "$(echo "$output" | jq -r '.[0].model')" = "null" ]
  [ "$(echo "$output" | jq -r '.[0].tier')" = "null" ]
}

@test "roster: a legacy branch-keyed row still renders" {
  CREW_ID=c1 run_crew status "worker:feat/x" working
  run run_crew roster c1
  [ "$(echo "$output" | jq -r 'length')" = "1" ]
  [ "$(echo "$output" | jq -r '.[0].branch')" = "feat/x" ]
  [ "$(echo "$output" | jq -r '.[0].session')" = "null" ]
}

@test "report: rows resolve for a sessioned worker id" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1",
           engine:"claude", model:"sonnet", tier:"standard", effort:"medium", title:"T"}' >>"$log"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
  run run_crew report c1
  [[ "$output" == *"done"* ]]
}

@test "rate: sweeps a sessioned worker and records its session" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1",
           engine:"claude", model:"sonnet", tier:"standard", effort:"medium", title:"T"}' >>"$log"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/share"
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '"\(.branch) \(.session) \(.reached_pr)"' "$XDG_DATA_HOME/crew/ratings.jsonl"
  [ "$output" = "feat/x s1-1 true" ]
}

@test "reap: a sessioned worker becomes a candidate" {
  git commit --allow-empty -q -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [[ "$output" == *"would reap feat/reap-me"* ]]
}

@test "reap: dry-run reports leftover processes whose cwd is in the worktree" {
  git commit --allow-empty -q -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-proc-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  export CREW_REAP_PROC_CMD="printf '4242\t$wt_path/subdir\n9999\t/elsewhere\n'"
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [[ "$output" == *"would kill pid 4242"* ]]
  [[ "$output" != *"would kill pid 9999"* ]]
}

@test "reap: a real run kills (best-effort) leftover worktree processes" {
  # kill is best-effort: the fake pid below does not exist, so `kill 4242 || true`
  # succeeds via the || true and the say still fires. The assertion is the wiring:
  # the step runs before wt remove and names the right pid, never the /elsewhere one.
  git commit --allow-empty -q -m init
  git branch feat/reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-proc-real-wt"
  git worktree add -q "$wt_path" feat/reap-me
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  export CREW_REAP_PROC_CMD="printf '4242\t$wt_path/subdir\n9999\t/elsewhere\n'"
  CREW_ID=c1 run_crew status "worker:feat/reap-me#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [[ "$output" == *"killed pid 4242"* ]]
  [[ "$output" != *"killed pid 9999"* ]]
}

@test "reap: keeps a worktree whose pane runs the wrapped engine" {
  git commit --allow-empty -q -m init
  git branch feat/reap-live
  wt_path="$BATS_TEST_TMPDIR/reap-live-wt"
  git worktree add -q "$wt_path" feat/reap-live
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  stub_tmux "" "$(printf '.claude-wrapped %s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/reap-live#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [[ "$output" == *"an engine is still running there"* ]]
  run ! grep -q remove "$STUB_LOG"
}

# Same exact-path requirement as the roster sibling-path test above, exercised
# through reap's own _pane_is_engine_at call site.
@test "reap: a sibling-path engine pane does not keep the worktree" {
  git commit --allow-empty -q -m init
  git branch feat/reap-sib
  wt_path="$BATS_TEST_TMPDIR/reap-sib-wt"
  git worktree add -q "$wt_path" feat/reap-sib
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  stub_tmux "" "$(printf '.claude-wrapped %s-sibling\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/reap-sib#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [[ "$output" != *"an engine is still running there"* ]]
}

@test "reap: releases a terminal session's window past --idle, keeping the worktree" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-me#s1-1" failed "gate never passed"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  [ -d "$wt_path" ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="release") | "\(.branch) \(.session) \(.state)"' "$log"
  [ "$output" = "feat/idle-me s1-1 failed" ]
}

@test "reap: releases an idle done window even with a live engine" {
  git commit --allow-empty -q -m init
  git branch feat/idle-done-live
  wt_path="$BATS_TEST_TMPDIR/idle-done-live-wt"
  git worktree add -q "$wt_path" feat/idle-done-live
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\t.claude-wrapped\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "reap: keeps an idle exited window when an engine is still live" {
  git commit --allow-empty -q -m init
  git branch feat/idle-exited-live
  wt_path="$BATS_TEST_TMPDIR/idle-exited-live-wt"
  git worktree add -q "$wt_path" feat/idle-exited-live
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\t.claude-wrapped\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-exited-live#s1-1" exited
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-exited-live — exited but an engine is still running there"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: leaves a terminal session inside --idle alone" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-me#s1-1" failed
  CREW_ID=c1 run run_crew reap --idle 3600
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: never releases a non-terminal session" {
  git commit --allow-empty -q -m init
  git branch feat/busy
  wt_path="$BATS_TEST_TMPDIR/busy-wt"
  git worktree add -q "$wt_path" feat/busy
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tclaude\n')"
  CREW_ID=c1 run_crew status "worker:feat/busy#s1-1" working
  CREW_ID=c1 run run_crew reap --idle 0
  run ! grep -q 'kill-window' "$STUB_LOG"
}

# One worktree hosts at most one live window, so idle release must collapse the
# branch to its NEWEST session before testing terminality. An older `done`
# session must not release a window a newer `working` session still owns.
@test "reap: a newer working session masks an older done one on the same branch" {
  git commit --allow-empty -q -m init
  git branch feat/two-sess
  wt_path="$BATS_TEST_TMPDIR/two-sess-wt"
  git worktree add -q "$wt_path" feat/two-sess
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tclaude\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1000, crew_id:"c1", from:"worker:feat/two-sess#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  jq -nc '{ts:2000, crew_id:"c1", from:"worker:feat/two-sess#s2-2", to:"dispatcher:c1", kind:"status", body:{state:"working"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: --dry-run only reports the release" {
  git commit --allow-empty -q -m init
  git branch feat/idle-me
  wt_path="$BATS_TEST_TMPDIR/idle-wt"
  git worktree add -q "$wt_path" feat/idle-me
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/idle-me#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0 --dry-run
  [[ "$output" == *"would release @23"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: rejects a non-numeric --idle" {
  CREW_ID=c1 run run_crew reap --idle nope
  [ "$status" -eq 1 ]
  [[ "$output" == *"--idle"* ]]
}

@test "reap: removes the dispatched label from the issue a merged PR closes" {
  git commit -q --allow-empty -m init
  git branch feat/42-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt"
  git worktree add -q "$wt_path" feat/42-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '42' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/42-reap-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_wt_removes
  CREW_ID=c1 run_crew status "worker:feat/42-reap-me" done "" "https://example.com/pr/7"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/42-reap-me"* ]]
  grep -q 'pr view https://example.com/pr/7 --json closingIssuesReferences' "$STUB_LOG"
  grep -q 'issue edit 42 --remove-label dispatched' "$STUB_LOG"
  # A merged PR's squash-merged branch is not an ancestor of main — reap
  # deletes it deliberately once gh confirms the merge (#194). The stub's
  # headRefOid answers the local tip so the delete guard passes.
  run ! git show-ref --verify --quiet refs/heads/feat/42-reap-me
}

@test "reap: a dispatched label-removal failure does not abort the sweep" {
  git commit -q --allow-empty -m init
  git branch feat/43-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt2"
  git worktree add -q "$wt_path" feat/43-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '43' ;;
*remove-label*) exit 1 ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_wt_removes
  CREW_ID=c1 run_crew status "worker:feat/43-reap-me" done "" "https://example.com/pr/9"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/43-reap-me"* ]]
  [[ "$output" == *"could not remove the dispatched label from #43"* ]]
}

@test "reap: a failed closing-issue resolution is logged, not treated as no closing issue" {
  git commit -q --allow-empty -m init
  git branch feat/45-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt4"
  git worktree add -q "$wt_path" feat/45-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*)
  echo "gh: rate limited" >&2
  exit 1
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_wt_removes
  CREW_ID=c1 run_crew status "worker:feat/45-reap-me" done "" "https://example.com/pr/11"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/45-reap-me"* ]]
  [[ "$output" == *"could not resolve closing issues for PR https://example.com/pr/11 (feat/45-reap-me)"* ]]
  run ! grep -q 'issue edit' "$STUB_LOG"
}

@test "reap: a PR with no closing issue reaps without attempting a label removal" {
  git commit -q --allow-empty -m init
  git branch feat/44-reap-me
  wt_path="$BATS_TEST_TMPDIR/reap-me-wt3"
  git worktree add -q "$wt_path" feat/44-reap-me
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_wt_removes
  CREW_ID=c1 run_crew status "worker:feat/44-reap-me" done "" "https://example.com/pr/10"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/44-reap-me"* ]]
  run ! grep -q 'issue edit' "$STUB_LOG"
}

@test "reap: a squash-merged PR is reaped by outcome — reap row, label, branch deletion" {
  # #194 Gap 1: this repo squash-merges, so a merged PR's branch is never an
  # ancestor of main. `wt remove` removes the worktree, refuses to delete the
  # "unmerged" branch, and can exit non-zero even so. reap must judge success
  # by the observable outcome (the worktree is gone), write the reap row,
  # release the dispatched label, and delete the local branch deliberately.
  git commit -q --allow-empty -m init
  git branch feat/squash-me
  wt_path="$BATS_TEST_TMPDIR/squash-wt"
  git worktree add -q "$wt_path" feat/squash-me
  wt_path=$(cd "$wt_path" && pwd -P)
  # The squash shape from the wild: the branch really is ahead of main (its
  # commits are gone from the squash merge), which is why wt refuses to
  # delete it and why reap must do so deliberately.
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "squash-me work"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/squash-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_wt_removes
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/squash-me" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/squash-me (MERGED)"* ]]
  grep -q 'pr view https://example.com/pr/8 --json closingIssuesReferences' "$STUB_LOG"
  grep -q 'issue edit 99 --remove-label dispatched' "$STUB_LOG"
  jq -e 'select(.kind=="reap" and .branch=="feat/squash-me")' "$log" >/dev/null
  [ ! -d "$wt_path" ]
  run ! git show-ref --verify --quiet refs/heads/feat/squash-me
}

@test "reap: a merged branch whose local tip diverges from the PR head is kept" {
  # Review-fix guard: the branch is only force-deleted when the local tip is
  # exactly the merged PR head. A resumed run or a human that committed past
  # the merge would otherwise have those commits orphaned by -D. The worktree
  # is still reaped and the label still released — only the branch survives.
  git commit -q --allow-empty -m init
  git branch feat/diverged
  wt_path="$BATS_TEST_TMPDIR/diverged-wt"
  git worktree add -q "$wt_path" feat/diverged
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "diverged work"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' '0000000000000000000000000000000000000000' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_wt_removes
  CREW_ID=c1 run_crew status "worker:feat/diverged" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/diverged (MERGED)"* ]]
  [[ "$output" == *"kept local branch feat/diverged — tip diverges from the merged PR head"* ]]
  grep -q 'issue edit 99 --remove-label dispatched' "$STUB_LOG"
  [ ! -d "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/diverged
}

@test "reap: a genuinely failed removal is still kept — no reap row, no label, no branch delete" {
  # #194 Gap 1 guard: a stale `wt remove` failure is judged by the worktree
  # still being present — the branch is reported kept, no reap row is written,
  # the dispatched label stays, and the local branch survives.
  git commit -q --allow-empty -m init
  git branch feat/stuck
  wt_path="$BATS_TEST_TMPDIR/stuck-wt"
  git worktree add -q "$wt_path" feat/stuck
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
echo "Cannot remove worktree: feat/stuck has uncommitted changes" >&2
exit 1
EOF
  chmod +x "$STUB_DIR/wt"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/stuck" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/stuck — wt remove failed"* ]]
  run ! grep -q 'remove-label' "$STUB_LOG"
  [ -d "$wt_path" ]
  git show-ref --verify --quiet refs/heads/feat/stuck
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: REVIEW_NOTES.md and round plans are scaffold — trashed, not deleted, worktree reaped" {
  # #194 Gap 2: EVIDENCE_REVIEW.md's worktree-root REVIEW_NOTES.md ledger and
  # the superpowers writing-plans round artifacts (PLAN_ROUND4.md observed in
  # a real worker tree) are untracked scaffold "our own pipeline wrote". They
  # must neither read as uncommitted work nor survive as physical files that
  # block `wt remove` — they are gtrash'd like WORKER_TASK.md, so a
  # post-mortem can still recover them.
  git commit -q --allow-empty -m init
  git branch feat/noted
  wt_path="$BATS_TEST_TMPDIR/noted-wt"
  git worktree add -q "$wt_path" feat/noted
  wt_path=$(cd "$wt_path" && pwd -P)
  : >"$wt_path/WORKER_TASK.md"
  : >"$wt_path/REVIEW_NOTES.md"
  : >"$wt_path/PLAN_ROUND4.md"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  # gtrash that really moves the file into a trash dir, so the test proves
  # "trashed, not deleted" — the files must survive for recovery.
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = put ]; then
  mkdir -p "$STUB_DIR/trash"
  mv "$2" "$STUB_DIR/trash/"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
  stub_wt_removes
  CREW_ID=c1 run_crew status "worker:feat/noted" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/noted (MERGED)"* ]]
  [ -f "$STUB_DIR/trash/WORKER_TASK.md" ]
  [ -f "$STUB_DIR/trash/REVIEW_NOTES.md" ]
  [ -f "$STUB_DIR/trash/PLAN_ROUND4.md" ]
  [ ! -d "$wt_path" ]
}

@test "reap: a finished grid window (lead + role panes) is released, then its worktree reclaimed" {
  # #194 Gap 3: grid leads never send the {"final":true} release, so a
  # finished grid window — a lead pane and role panes, all engine commands —
  # idles in place. reap must reclaim it without a manual tmux kill-window:
  # the idle-release phase kills the window (engine or not, once the lead's
  # status is terminal), and the reclaim phase of the SAME pass removes the
  # worktree once the PR merged and no pane is live. The tmux stub is
  # stateful: kill-window removes the window's rows, exactly like the real
  # server, so the post-release engine scan sees no panes.
  git commit -q --allow-empty -m init
  git branch feat/grid-me
  wt_path="$BATS_TEST_TMPDIR/grid-wt"
  git worktree add -q "$wt_path" feat/grid-me
  wt_path=$(cd "$wt_path" && pwd -P)
  : >"$wt_path/WORKER_TASK.md"
  : >"$wt_path/REVIEW_NOTES.md"
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tpi\n@23\t%%34\tpi\n')"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
kill-window) : >"$STUB_DIR/wins.txt"; : >"$STUB_DIR/panes.txt" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/grid-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = put ]; then
  mkdir -p "$STUB_DIR/trash"
  mv "$2" "$STUB_DIR/trash/"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
  stub_wt_removes
  CREW_ID=c1 run_crew status "worker:feat/grid-me#s1-1" done "" "https://example.com/pr/30"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  [[ "$output" == *"reaped feat/grid-me (MERGED)"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  [ -f "$STUB_DIR/trash/REVIEW_NOTES.md" ]
  [ ! -d "$wt_path" ]
  run ! git show-ref --verify --quiet refs/heads/feat/grid-me
  # A later pass is a clean no-op — the worktree is already gone.
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
}


@test "reap: an exited worker with a merged PR is reclaimed" {
  # #68 defect 1: the reclaim filter used to accept only "done", pinning the
  # worktree of every worker that ended via `exited` (compaction, context
  # limit, or a human taking the pane) forever even after its PR merged.
  git commit -q --allow-empty -m init
  git branch feat/exited-me
  wt_path="$BATS_TEST_TMPDIR/exited-wt"
  git worktree add -q "$wt_path" feat/exited-me
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/exited-me" exited "" "https://example.com/pr/20"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/exited-me"* ]]
}

@test "reap: a failed worker with an open PR is kept" {
  # The PR gate — not the worker's own terminal state — is what must decide
  # keep vs. reclaim; widening the state filter must not let an open PR through.
  git commit -q --allow-empty -m init
  git branch feat/failed-open
  wt_path="$BATS_TEST_TMPDIR/failed-open-wt"
  git worktree add -q "$wt_path" feat/failed-open
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'OPEN' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/failed-open" failed "" "https://example.com/pr/21"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/failed-open — PR OPEN"* ]]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: a worktree dirty only with pipeline scaffold is reclaimed" {
  # #68 defect 2: SPEC.md/PLAN.md/DECOMPOSITION.md/WORKER_TASK.md and a
  # superpowers plan doc are all untracked files OUR OWN pipeline writes;
  # none of them should make a finished worktree read as dirty.
  git commit -q --allow-empty -m init
  git branch feat/scaffold-me
  wt_path="$BATS_TEST_TMPDIR/scaffold-wt"
  git worktree add -q "$wt_path" feat/scaffold-me
  : >"$wt_path/WORKER_TASK.md"
  : >"$wt_path/SPEC.md"
  : >"$wt_path/PLAN.md"
  : >"$wt_path/DECOMPOSITION.md"
  mkdir -p "$wt_path/docs/superpowers/plans"
  : >"$wt_path/docs/superpowers/plans/2026-08-29-scaffold.md"
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/scaffold-me" done "" "https://example.com/pr/22"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/scaffold-me"* ]]
}

@test "reap: a tracked modification keeps the worktree" {
  # A TRACKED change to one of the scaffold names (e.g. a real src/PLAN.md)
  # must still count as dirt — only untracked scaffold is ever ignored.
  echo one >README.md
  git add README.md
  git commit -q -m init
  git branch feat/tracked-dirty
  wt_path="$BATS_TEST_TMPDIR/tracked-dirty-wt"
  git worktree add -q "$wt_path" feat/tracked-dirty
  echo two >"$wt_path/README.md"
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/tracked-dirty" done "" "https://example.com/pr/23"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/tracked-dirty — uncommitted changes"* ]]
}

@test "reap: an untracked non-scaffold file keeps the worktree" {
  git commit -q --allow-empty -m init
  git branch feat/stray-file
  wt_path="$BATS_TEST_TMPDIR/stray-wt"
  git worktree add -q "$wt_path" feat/stray-file
  : >"$wt_path/notes.txt"
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/stray-file" done "" "https://example.com/pr/24"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/stray-file — uncommitted changes"* ]]
}

@test "reap: a live engine pane keeps a terminal, merged-PR worktree" {
  # Widening the terminal-state filter (defect 1) must not let an exited
  # worker whose PR merged get reclaimed out from under a human (or another
  # session) still sitting at the pane. The live-pane check runs unconditionally,
  # after the terminal-state filter, so it must still guard exited/failed the
  # same way it already guarded done.
  git commit -q --allow-empty -m init
  git branch feat/live-engine-me
  wt_path="$BATS_TEST_TMPDIR/live-engine-wt"
  git worktree add -q "$wt_path" feat/live-engine-me
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note in the idle-release tests
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  stub_tmux "" "$(printf 'claude %s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/live-engine-me" exited "" "https://example.com/pr/25"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/live-engine-me — an engine is still running there"* ]]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: an exited worker with no PR is kept" {
  # Same guard as the long-standing "done but no PR" keep, now exercised for
  # a state defect 1 newly admits into the candidate pool.
  git commit -q --allow-empty -m init
  git branch feat/exited-no-pr
  wt_path="$BATS_TEST_TMPDIR/exited-no-pr-wt"
  git worktree add -q "$wt_path" feat/exited-no-pr
  stub_bin gh
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/exited-no-pr" exited
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/exited-no-pr — exited but no PR on the bus"* ]]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "msg: an oversized JSON body stays parseable JSON" {
  big="$(head -c 6000 /dev/zero | tr '\0' x)"
  body="$(jq -nc --arg d "$big" '{seam:"execute",tag:"gate_thrash",detail:$d}')"
  CREW_ID=c1 run_crew msg worker:feat/1 "retro:c1" "$body"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  line_bytes="$(printf '%s' "$(head -1 "$log")" | wc -c | tr -d ' ')"
  [ "$line_bytes" -le 4096 ]
  # The body must still parse, and keep every key — a blob cut loses the tag.
  run jq -e '(.body | fromjson) | .seam == "execute" and .tag == "gate_thrash" and (.detail | endswith("[elided]"))' "$log"
  [ "$status" -eq 0 ]
}

@test "msg: a non-JSON oversized body is still clipped as plain text" {
  big="$(head -c 6000 /dev/zero | tr '\0' y)"
  CREW_ID=c1 run_crew msg worker:feat/1 dispatcher:c1 "$big"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  line_bytes="$(printf '%s' "$(head -1 "$log")" | wc -c | tr -d ' ')"
  [ "$line_bytes" -le 4096 ]
  run jq -e '.body | endswith("[elided]")' "$log"
  [ "$status" -eq 0 ]
}

@test "rate: one unparseable metrics body does not kill the sweep" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  cat >"$log" <<'EOF'
{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/bad","engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"bad body"}
{"ts":1001,"crew_id":"c1","kind":"status","from":"worker:feat/bad","body":{"state":"done"}}
{"ts":1002,"crew_id":"c1","kind":"msg","from":"worker:feat/bad","to":"metrics:c1","body":"{\"rework_count\":3,\"detail\":\"trunc …[elided]"}
{"ts":2000,"crew_id":"c1","kind":"dispatch","branch":"feat/good","engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"good body"}
{"ts":2001,"crew_id":"c1","kind":"status","from":"worker:feat/good","body":{"state":"done"}}
{"ts":2002,"crew_id":"c1","kind":"msg","from":"worker:feat/good","to":"metrics:c1","body":"{\"rework_count\":7}"}
EOF
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  run run_crew rate
  [ "$status" -eq 0 ]
  # Both runs must be recorded; the bad body folds to null metrics, not a crash.
  run jq -s -e 'map({branch, rework_count}) | sort_by(.branch)
    == [{branch:"feat/bad",rework_count:null},{branch:"feat/good",rework_count:7}]' \
    "$XDG_DATA_HOME/crew/ratings.jsonl"
  [ "$status" -eq 0 ]
}

# ---------------------------------------------------------------------------
# stall-watch harness
# ---------------------------------------------------------------------------

# bus — the raw event log for the test repo. Exported so `run bash -c "bus | ..."`
# (a new bash process, not a fork) can see it.
bus() {
  cat "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl" 2>/dev/null || true
}
export -f bus

# seed_raw <from> <state> <detail> <source> [ts_ms] — append a status event
# directly, so a test can plant a `source:"watchdog"` event or one dated into a
# previous run (things `crew status` cannot express).
seed_raw() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg f "$1" --arg s "$2" --arg d "$3" --arg src "$4" \
    --argjson ts "${5:-$(($(date +%s) * 1000))}" \
    '{ts:$ts, crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status",
      body:({state:$s}
            + (if $d!="" then {detail:$d} else {} end)
            + (if $src!="" then {source:$src} else {} end))}' >>"$logf"
}

# seed_msg <from> <to> <age_s> — append a msg event dated <age_s> seconds ago.
seed_msg() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg f "$1" --arg t "$2" --argjson ts "$((($(date +%s) - $3) * 1000))" \
    '{ts:$ts, crew_id:"c1", from:$f, to:$t, kind:"msg", body:"{\"verdict\":\"accept\"}"}' >>"$logf"
}

# seed_start <dispatch|resume> <session> <ts_ms> — a session-start row on feat/x,
# shaped like dispatch.sh's and dispatch-resume.sh's, which `crew` cannot write.
seed_start() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg k "$1" --arg s "$2" --argjson ts "$3" \
    '{ts:$ts, crew_id:"c1", kind:$k, branch:"feat/x", session:$s,
      worker_id:("worker:feat/x#" + $s)}' >>"$logf"
}

# stall_sampler <frame-file>... — install a CREW_STALL_SAMPLE_CMD that emits the
# given frames one per call and repeats the last one forever. The literal token
# GONE makes the sampler exit non-zero from that call on (pane vanished).
stall_sampler() {
  SAMPLER_DIR="$BATS_TEST_TMPDIR/sampler.$$"
  mkdir -p "$SAMPLER_DIR"
  printf '%s\n' "$@" >"$SAMPLER_DIR/frames"
  printf '0' >"$SAMPLER_DIR/n"
  cat >"$SAMPLER_DIR/sample" <<'EOS'
#!/usr/bin/env bash
d="$(dirname "$0")"
n=$(cat "$d/n")
n=$((n + 1))
printf '%s' "$n" >"$d/n"
total=$(wc -l <"$d/frames")
i="$n"
[ "$i" -gt "$total" ] && i="$total"
f=$(sed -n "${i}p" "$d/frames")
[ "$f" = GONE ] && exit 1
cat "$f"
EOS
  chmod +x "$SAMPLER_DIR/sample"
  export CREW_STALL_SAMPLE_CMD="$SAMPLER_DIR/sample"
}

# stall_load_sampler <"load1 cores"-line>... — install a CREW_STALL_LOAD_CMD that
# emits the given lines one per call and repeats the last one forever. Used by
# the D4 host-load tests so no real load is ever generated (the host is shared with
# live sibling workers).
stall_load_sampler() {
  LOAD_DIR="$BATS_TEST_TMPDIR/load.$$"
  mkdir -p "$LOAD_DIR"
  printf '%s\n' "$@" >"$LOAD_DIR/frames"
  printf '0' >"$LOAD_DIR/n"
  cat >"$LOAD_DIR/load" <<'EOS'
#!/usr/bin/env bash
d="$(dirname "$0")"
n=$(cat "$d/n")
n=$((n + 1))
printf '%s' "$n" >"$d/n"
total=$(wc -l <"$d/frames")
i="$n"
[ "$i" -gt "$total" ] && i="$total"
sed -n "${i}p" "$d/frames"
EOS
  chmod +x "$LOAD_DIR/load"
  export CREW_STALL_LOAD_CMD="$LOAD_DIR/load"
}

# frame_file <name> — read a frame from stdin, write it, echo its path.
frame_file() {
  local f="$BATS_TEST_TMPDIR/frame.$1"
  cat >"$f"
  printf '%s' "$f"
}

# ---- fixtures, pinned from EVIDENCE-2026-08-10.txt ------------------------

# A3/A4 gate capture, pane %118 — a live option-select prompt frame. The footer
# is the last non-empty line; the nearest numbered option is 2 lines above it.
fx_prompt_select() {
  frame_file prompt_select <<'EOF'
  2. Gate everything on 3.8
     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.
  3. Require 3.8, drop legacy
  4. Type something.
──────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
EOF
}

# [field] session 3 — the workspace-trust frame that wedged three healthy
# workers. Footer is `Enter to confirm`, marker is ASCII `>`, no `Esc to cancel`.
fx_prompt_trust() {
  frame_file prompt_trust <<'EOF'
Quick safety check: Is this a project you created or one you trust?
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
EOF
}

# Captured from the Codex worker hook-review prompt in WORKER_TASK.md. This is
# intentionally verbatim: Codex gets no broader prompt signature without a
# captured frame.
fx_codex_hooks_review() {
  frame_file codex_hooks_review <<'EOF'
Hooks need review
  1 hook is new or changed.
  Hooks can run outside the sandbox after you trust them.
› 1. Review hooks  2. Trust all and continue  3. Continue without trusting
EOF
}

fx_codex_hooks_review_reordered() {
  frame_file codex_hooks_review_reordered <<'EOF'
Hooks need review
  Hooks can run outside the sandbox after you trust them.
  1 hook is new or changed.
› 1. Review hooks  2. Trust all and continue  3. Continue without trusting
EOF
}

# The same trust frame scrolled into the transcript: the input box is the last
# non-empty line, so the geometry anchor must reject it (this is what keeps this
# very test file from being a false-positive source).
fx_prompt_scrollback() {
  frame_file prompt_scrollback <<'EOF'
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
EOF
}

# Same, with the option-select footer, to assert the widening is symmetric.
fx_select_scrollback() {
  frame_file select_scrollback <<'EOF'
  5. Chat about this
Enter to select · Tab/Arrow keys to navigate · Esc to cancel
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
EOF
}

# fx_bgwait_* — finished claude turns parked on a background shell, captured
# from healthy workers (claude Code v2.1.280, #353). The `❯` line is a prompt
# SUGGESTION, not unsent input.
fx_bgwait_crunched() {
  frame_file bgwait_crunched <<'EOF'
✻ Crunched for 1m 12s · done 11:16 AM · 1 shell still running
────────────────── reef ─
❯ push it and re-post pr_open once CI is green
──────────────────
  🤖 Opus 5.5 🧠 high | 📊 350k/1M | ⚡ 29% (1h53m → 13:10)
  -- INSERT -- ⏵⏵ auto mode on · PR #331 · 1 shell · ← for agents
EOF
}

fx_bgwait_churned() {
  frame_file bgwait_churned <<'EOF'
✻ Churned for 36s · done 11:20 AM · 1 shell still running
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
}

# The same finished turn with the shell gone: nothing is being waited on.
fx_bgwait_noshell() {
  frame_file bgwait_noshell <<'EOF'
✻ Churned for 36s · done 11:20 AM
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
}

# A3 — a finished/idle pane: no meter, no prompt, ends on the input box.
fx_idle_box() {
  frame_file idle_box <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents
EOF
}

# fx_meter <clock> <tokens> — D2's shape: a live meter with NO live subagent
# row. The `⎿ Done` history line is included on purpose: it is the decoy that
# must NOT read as a subagent row, or D2 would be neutered forever.
fx_meter() {
  frame_file "meter.$1.$2" <<EOF
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✳ Perusing… ($1 · ↓ $2 tokens · thinking more with high effort)
EOF
}

# fx_subbatch <parent-clock> <subagent-elapsed> — A1's measured false-positive
# driver, pane %129, verbatim shape: meter present, clock rising, parent token
# string static at 73.2k, and a LIVE subagent row painted throughout.
fx_subbatch() {
  frame_file "subbatch.$1.$2" <<EOF
  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)
✶ Hatching… ($1 · ↓ 73.2k tokens)
  ◯ general-purpose  Revise spec per critic                              $2 · ↓ 71.5k tokens
EOF
}

# [#31]'s transcription — ASCII-fied `·`/`↓`/`…`. Must NOT match the meter ERE.
fx_meter_transcribed() {
  frame_file meter_transcribed <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
Considering... 1h 20m - 28.0k tokens
EOF
}

# fx_meter_hours <clock> — the reconstructed ≥1h wire form (A2, uncaptured).
fx_meter_hours() {
  frame_file "meter_hours.$1" <<EOF
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
Considering… ($1 · ↓ 28.0k tokens)
EOF
}

# fx_prompt_quota — the rate-limit prompt from issue #58's transcript.
# RECONSTRUCTED, not captured: no frame of this prompt exists in
# EVIDENCE-2026-08-10.txt (same caveat as fx_meter_hours's "uncaptured"
# note above). Footer/geometry assumed to match every other measured
# option-select frame in this file.
fx_prompt_quota() {
  frame_file prompt_quota <<'EOF'
What do you want to do?
❯ 1. Stop and wait for limit to reset
  2. Upgrade your plan
  3. Upgrade to Team plan
Enter to select · Esc to cancel
EOF
}

# fx_prompt_trust_with_distant_quota_text — a GENUINE, different prompt (the
# workspace-trust frame) whose visible screen also happens to contain the
# literal quota phrase, but more than 12 non-empty lines above the trust
# prompt's own footer — i.e. outside _is_quota_prompt's search window.
# Guards against classifying a real, answerable prompt as quota: merely
# because that phrase is visible somewhere higher on the same screen (e.g. a
# worker with DISPATCHER_PROTOCOL.md scrolled into view).
fx_prompt_trust_with_distant_quota_text() {
  frame_file prompt_trust_distant <<'EOF'
Reviewing DISPATCHER_PROTOCOL.md: `quota:` fires on Stop and wait for limit to reset.
line 2 filler
line 3 filler
line 4 filler
line 5 filler
line 6 filler
line 7 filler
line 8 filler
line 9 filler
line 10 filler
line 11 filler
line 12 filler
Quick safety check: Is this a project you created or one you trust?
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
EOF
}

# fx_session_limit_refusal — captured verbatim from a real wedged worker in
# issue #93, not reconstructed (contrast fx_prompt_quota's note above). A
# working-pane shape: normal status bar, no option-select prompt.
fx_session_limit_refusal() {
  frame_file session_limit_refusal <<'EOF'
⏺ Monitor event: "idle wait for wave 2 reports"
  ⎿  You've hit your session limit · resets 7pm (Asia/Jerusalem)
     /upgrade to increase your usage limit.

──────────────────────────────────────────── bronze ─
❯ keep going
─────────────────────────────────────────────────────
  ⚠ /low-priority to continue now at lower priority · uses your weekly limit
  🤖 Opus 5 🧠 high | 📊 350k/1M                                    /rc
  ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents
EOF
}

# fx_prompt_trust_with_distant_session_limit_text — mirrors
# fx_prompt_trust_with_distant_quota_text for the other quota variant: a real
# workspace-trust prompt with the three session-limit anchors pushed above
# _is_quota_session_limit's tail window, as they would be for a worker that
# merely has this repo's own doc text on screen.
fx_prompt_trust_with_distant_session_limit_text() {
  frame_file prompt_trust_distant_session_limit <<'EOF'
⏺ Monitor event: "idle wait for wave 2 reports"
  ⎿  You've hit your session limit · resets 7pm (Asia/Jerusalem)
     /upgrade to increase your usage limit.
  ⚠ /low-priority to continue now at lower priority · uses your weekly limit
line 1 filler
line 2 filler
line 3 filler
line 4 filler
line 5 filler
line 6 filler
line 7 filler
line 8 filler
line 9 filler
line 10 filler
line 11 filler
line 12 filler
line 13 filler
line 14 filler
line 15 filler
Quick safety check: Is this a project you created or one you trust?
> 1. Yes, I trust this folder
2. No, exit
Enter to confirm
EOF
}

@test "stall-watch: D1 posts blocked/prompt: on the option-select frame" {
  p=$(fx_prompt_select)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|prompt: interactive prompt in pane %9 —"* ]]
}

@test "stall-watch: a blocked post marks the pane source watchdog" {
  stub_bin tmux
  p=$(fx_prompt_select)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run grep -F -- 'set-option -p -t %9 @crew_state blocked' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-option -p -t %9 @crew_source watchdog' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "stall-watch: D1 fires on the workspace-trust frame outside the startup window" {
  # Criterion 4b — the widened footer set lives in D1's own signature set, not
  # only in D0's classifier.
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == prompt:* ]]
}

@test "stall-watch: session-3 regression — a static trust prompt is prompt:, never failed or stalled:" {
  # The measured 3/3 false positive. A pane byte-static across the whole --stall
  # window, inside --window, carrying the trust frame.
  # --max-life 8: the single expected event must fire before the top-of-loop
  # exit; at 4 a stretched pre-sample gap could starve it (#185, same as D0's
  # fix below).
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == prompt:* ]]
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'stalled:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D0 posts blocked with a diagnosis-free stalled: detail" {
  # Full-string match on purpose: a reintroduced `(suspected …)` fails CI.
  # --max-life 8 gives D0 (`--stall 1`) headroom: at `--max-life 3` one
  # stretched pre-sample gap could exit the loop before the second sample and
  # starve the single expected event (#185).
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: D2 posts blocked/turn-stall: when the clock advances and tokens do not" {
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  d=$(fx_meter "6m 14s" "25.0k")
  stall_sampler "$a" "$b" "$c" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|watchdog" ]
  run bash -c "bus | jq -r '.body.detail'"
  [[ "$output" == "turn-stall: token count static at 25.0k for "* ]]
}

@test "stall-watch: D2 reads the reconstructed 1h meter and ignores #31's transcription" {
  # The hours alternative of the meter ERE (A2 is a documented false-negative
  # risk per C-4, not a gate) — and the ASCII-fied paste must stay unmatched.
  a=$(fx_meter_hours "1h 20m")
  stall_sampler "$a" "$a" "$a" "$a"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c 'turn-stall:' || true"
  [ "$output" = "0" ] # clock never changed: a static capture is not evidence

  rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  a=$(fx_meter_hours "1h 20m")
  b=$(fx_meter_hours "1h 21m")
  c=$(fx_meter_hours "1h 22m")
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c 'turn-stall:' || true"
  [ "$output" = "1" ]

  rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  t=$(fx_meter_transcribed)
  stall_sampler "$t" "$t" "$t" "$t"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c 'turn-stall:' || true"
  [ "$output" = "0" ] # no meter matched → D2 has nothing to read
}

@test "stall-watch: D3 posts blocked/quiet: on a byte-identical pane in steady state" {
  # --max-life 8: D3 needs `quiet_for >= --idle 2`, and the run must still be
  # alive when it fires; at 4 one stretched pre-sample gap could exit the loop
  # first and starve the single expected event (#185, same as D0's fix above).
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|quiet: pane unchanged for "* ]]
}

@test "stall-watch: D5 flags a pane still a shell past --launch as launch-not-started" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD='printf fish'
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 2 --window 60 --stall 999 --idle 999 --dead 999 --max-life 6
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|stalled: launch-not-started"* ]]
}

@test "stall-watch: D5 clears itself when the engine appears late" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD="[ -f $BATS_TEST_TMPDIR/up ] && printf claude || printf fish"
  (sleep 4 && touch "$BATS_TEST_TMPDIR/up") &
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 1 --window 60 --stall 999 --idle 999 --dead 999 --max-life 9
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "blocked|stalled: launch-not-started"* ]]
  [ "${lines[1]}" = "working|stalled: launch-not-started cleared" ]
}

@test "stall-watch: D5 is silent when the engine is the pane's command" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD='printf .claude-wrapped'
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 1 --window 60 --stall 999 --idle 999 --dead 999 --max-life 5
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D5 is silent while a shell pane is inside --launch" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD='printf fish'
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --launch 999 --window 60 --stall 999 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D6 flags a working lead with an undelivered role verdict" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 30
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|unread: "* ]]
}

@test "stall-watch: D6 is silent once the verdict is delivered" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 30
  run run_crew inbox worker:feat/x#s1-1 c1
  [ "$status" -eq 0 ]
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\")' | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D6 is silent while the verdict is younger than --unread" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 1
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --unread 999 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\")' | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D6 is silent when the lead is not working" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 pr_open "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 30
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\")' | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D6 ignores a verdict the lead already answered" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 30
  seed_msg worker:feat/x#s1-1 role:feat/x:reviewer 20
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\")' | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D6 clears itself once the verdict is delivered" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 30
  (sleep 2 && run_crew inbox worker:feat/x#s1-1 c1 >/dev/null) &
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 9
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "blocked|unread: "* ]]
  [ "${lines[1]}" = "working|unread: cleared" ]
}

@test "stall-watch: D6 is silent for a branch-keyed watchdog" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x working "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 30
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\")' | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a finished turn waiting on a background shell posts nothing" {
  a=$(fx_bgwait_crunched)
  b=$(fx_bgwait_churned)
  stall_sampler "$a" "$a" "$a" "$b" "$b" "$b" "$b" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 2 --dead 2 --max-life 15
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a finished turn with no background shell still posts stalled:" {
  p=$(fx_bgwait_noshell)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: a live spinner under a stale done line is not a background-shell wait" {
  p=$(frame_file bgwait_spinner <<'EOF'
✻ Crunched for 1m 12s · done 11:16 AM · 1 shell still running
✻ Pondering… (3s)
──────────────────
❯
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: a background-shell wait past --bg-wait still reaches quiet:" {
  p=$(fx_bgwait_crunched)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --bg-wait 3 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|quiet: pane unchanged for "* ]]
}

@test "stall-watch: healthy subagent batch produces ZERO events" {
  # A1's measured false-positive driver, verbatim: meter present, clock rising,
  # parent token string static at 73.2k, live subagent row throughout. D2 is
  # vetoed by the row, D3 by the byte changes, D1 by the meter.
  # Every frame before GONE is byte-distinct ON PURPOSE: the detector
  # thresholds are wall-clock (`date +%s`) deltas, so one stretched
  # inter-sample gap satisfies `--idle 2` — a repeated final frame let D3 post
  # `quiet:` and flake this zero-events oracle under `bats --jobs` load (#185).
  a=$(fx_subbatch "26m 57s" "3m 29s")
  b=$(fx_subbatch "27m 12s" "3m 45s")
  c=$(fx_subbatch "27m 27s" "4m 0s")
  d=$(fx_subbatch "27m 42s" "4m 15s")
  e=$(fx_subbatch "27m 58s" "4m 30s")
  f=$(fx_subbatch "28m 13s" "4m 45s")
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  stall_sampler "$a" "$b" "$c" "$d" "$e" "$f" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 15
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: the subagent-row veto survives LC_ALL=C" {
  # C-5: a single-character bracket expression consumes one BYTE under LC_ALL=C,
  # so a non-multibyte-safe class would never match `◯` and D2 would lose its
  # only measured guard — silently.
  a=$(fx_subbatch "26m 57s" "3m 29s")
  b=$(fx_subbatch "27m 12s" "3m 45s")
  c=$(fx_subbatch "27m 27s" "4m 0s")
  # The sampler repeats its last frame, so the pane goes byte-static here where
  # test 3a's does not: --idle 3 against a 4s life keeps that from arming D3, so
  # a non-zero count can only mean the veto failed.
  stall_sampler "$a" "$b" "$c" "$c" "$c"
  LC_ALL=C CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 3 --dead 999 --max-life 4
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a meter with rising tokens produces ZERO events" {
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "26.1k")
  c=$(fx_meter "5m 59s" "27.4k")
  d=$(fx_meter "6m 14s" "28.8k")
  # GONE stops the sampler repeating its last frame: an un-GONE'd tail would
  # sample `d` twice, and one stretched wall-clock gap meets `--idle 2` and
  # lets D3 post, breaking the zero-events oracle (#185). The run exits on
  # --max-life 5 with fails=1 here, which is fine — the oracle only needs the
  # detectors silent.
  stall_sampler "$a" "$b" "$c" "$d" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 5
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a working heartbeat damps D2" {
  CREW_ID=c1 run_crew status worker:feat/x working
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  # A heartbeat buys the window exactly one tick, so the tick has to be wider
  # than the one-second truncation slop for the difference to be observable:
  # without the damping the second sample fires at 3s, with it nothing fires
  # before the 6s life runs out.
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 3 --window 0 --idle 3 --dead 999 --max-life 6
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a self-reported blocked suppresses every detector" {
  CREW_ID=c1 run_crew status worker:feat/x blocked "which approach?"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 2 --dead 999 --max-life 4
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a prompt frame in scrollback produces ZERO events, both footers" {
  for fx in fx_prompt_scrollback fx_select_scrollback; do
    rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
    p=$($fx)
    stall_sampler "$p" "$p" "$p" "$p"
    CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
      --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
    run bash -c "bus | grep -c . || true"
    [ "$output" = "0" ]
  done
}

@test "stall-watch: codex leaves an uncaptured Claude prompt frame to startup silence" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  # --max-life 8: the single expected stalled: line must fire before the
  # top-of-loop exit; at 4 a stretched pre-sample gap could starve it (#185,
  # same as D0's fix above).
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  # The static Claude frame is not classifiable for Codex, so it falls to D0.
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
  run bash -c "bus | grep -c 'prompt:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: codex Hooks need review capture posts blocked/prompt:" {
  p=$(fx_codex_hooks_review)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|prompt: interactive prompt in pane %9"* ]]
}

@test "stall-watch: codex reordered Hooks need review lines fall through to startup silence" {
  p=$(fx_codex_hooks_review_reordered)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
}

@test "stall-watch: a missing --engine enables no signature detector" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: one post per episode, then a working clearance that re-arms" {
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  p=$(fx_prompt_trust)
  q=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$q" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 20
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 3 ]
  [[ "${lines[0]}" == blocked\|prompt:* ]]
  [ "${lines[1]}" = "working|prompt: cleared" ]
  [[ "${lines[2]}" == blocked\|prompt:* ]]
}

@test "stall-watch: a quiet: episode escalates to failed after --dead" {
  # --max-life 15 keeps the `blocked`→`failed` (+--dead 2) sequence inside the
  # run even when stretched samples push the D3 fire late (#185).
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 15
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == blocked\|quiet:* ]]
  [[ "${lines[1]}" == "failed|dead: quiet: unchanged for "* ]]
}

@test "stall-watch: a clearance before --dead cancels the escalation" {
  p=$(fx_idle_box)
  q=$(fx_meter "5m 29s" "25.0k")
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  stall_sampler "$p" "$p" "$p" "$q" "$q" "$q" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 3 --max-life 15
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet: cleared' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a prompt: episode NEVER escalates (C-1)" {
  # An unanswered answerable question is waiting work, not death. Escalating it
  # would reproduce session 3 with a 30-minute delay.
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 20
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'prompt:' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a prompt: episode held past --idle and --dead still never escalates or gets superseded by quiet:" {
  # C-1's real shape. A frozen prompt frame is byte-identical by construction, so
  # it satisfies D3 too — and the C-1 test above pins --idle above --max-life, so
  # it never reaches that. Here --idle and --dead are both crossed while the
  # prompt: episode is open: quiet: must not supersede it, and the timer must not
  # launder it into a failed.
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 8
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet:' || true"
  [ "$output" = "0" ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == blocked\|prompt:* ]]
}

@test "stall-watch: INV-W0 a — a bare-id worker terminal state mutes a suffixed watchdog" {
  CREW_ID=c1 run_crew status worker:feat/x done
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1786338213-54181" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 b — a suffixed-id worker terminal state mutes a bare watchdog" {
  seed_raw "worker:feat/x#s99" done "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 c — a suffixed heartbeat damps a bare-invoked watchdog" {
  seed_raw "worker:feat/x#s99" working "" ""
  a=$(fx_meter "5m 29s" "25.0k")
  b=$(fx_meter "5m 44s" "25.0k")
  c=$(fx_meter "5m 59s" "25.0k")
  stall_sampler "$a" "$b" "$c" "$c"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 --engine claude \
    --grace 0 --interval 3 --window 0 --idle 3 --dead 999 --max-life 6
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W0 d — writes carry the invoking session id, roster still shows ONE row" {
  CREW_ID=c1 run_crew status worker:feat/x working
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1786338213-54181" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x#s1786338213-54181" ]
  CREW_ID=c1 run run_crew roster c1
  run bash -c "printf '%s' '$output' | jq 'length'"
  [ "$output" = "1" ]
}

@test "stall-watch: a bare invocation writes a bare from" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch feat/x --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x" ]
}

@test "stall-watch: a '#' branch keeps its full branch key" {
  seed_raw "worker:feat/a#zz#s2-2" done "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/a#b#s1-1" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/a#b#s1-1" ]
}

@test "stall-watch: a watchdog steps aside once a newer session posts on its branch" {
  seed_raw "worker:feat/x#s2-2" working "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1-1" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: an older session's post does not disarm a newer watchdog" {
  seed_raw "worker:feat/x#s1-1" working "" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s2-2" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x#s2-2" ]
}

@test "stall-watch: a resumed worker's stale watchdog cannot capture a branch-only reply" {
  t=$((($(date +%s) - 60) * 1000))
  seed_start dispatch s1-1 "$t"
  seed_raw "worker:feat/x#s1-1" working "" "" "$((t + 1000))"
  seed_start resume s2-2 "$((t + 2000))"
  seed_raw "worker:feat/x#s2-2" working resumed ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1-1" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  CREW_ID=c1 run_crew reply "worker:feat/x" "resume-directive"
  run run_crew inbox "worker:feat/x#s2-2" c1
  [[ "$output" == *"resume-directive"* ]]
}

@test "stall-watch: INV-W1 — a terminal state already on the bus produces zero writes" {
  CREW_ID=c1 run_crew status worker:feat/x failed "gate red"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: INV-W3 — a second watchdog does not re-post an open episode" {
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %9 — worker is waiting on input nobody can give" watchdog
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: C-3 — a previous run's terminal state does not mute a new watchdog" {
  seed_raw worker:feat/x failed "dead: quiet: unchanged for 1800s" watchdog "$((($(date +%s) - 3600) * 1000))"
  seed_raw worker:feat/x exited "" "" "$((($(date +%s) - 3500) * 1000))"
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: pr_open does not exit the watchdog, done does" {
  CREW_ID=c1 run_crew status worker:feat/x pr_open "" https://example.com/pr/1
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: survives 2 sample failures and exits after the 3rd" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" GONE GONE "$p" "$p" "$p" GONE GONE GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 30
  [ "$status" -eq 0 ]
  # It survived the pair at samples 2-3 (the prompt confirmed on 4+5 and posted),
  # then exited on the triple rather than running to --max-life 30.
  run bash -c "bus | grep -c 'prompt: interactive' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: writes zero msg events" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | grep -c '\"kind\":\"msg\"' || true"
  [ "$output" = "0" ]
  CREW_ID=c1 run run_crew inbox "dispatcher:c1"
  [ -z "$output" ]
}

@test "stall-watch: rejects an unknown flag" {
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --bogus 1
  [ "$status" -eq 1 ]
  [[ "$output" == *"unknown arg"* ]]
}

@test "stall-watch: D1 posts blocked/quota: on the rate-limit prompt, not prompt:" {
  p=$(fx_prompt_quota)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|quota: quota exhausted"* ]]
}

@test "stall-watch: a quota: episode NEVER escalates (C-1 quota variant)" {
  # Mirrors "a prompt: episode NEVER escalates (C-1)" for the quota discriminator.
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  p=$(fx_prompt_quota)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 20
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quota:' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a quota: episode held past --idle and --dead still never escalates or gets superseded by quiet:" {
  # Mirrors the equivalent prompt: test. A frozen quota frame is byte-identical
  # by construction, so it satisfies D3 too: quiet: must not supersede quota:,
  # and the timer must not launder it into a failed.
  p=$(fx_prompt_quota)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 8
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet:' || true"
  [ "$output" = "0" ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == blocked\|quota:* ]]
}

@test "stall-watch: a genuine prompt with the quota phrase far up-screen still classifies as prompt:, not quota:" {
  p=$(fx_prompt_trust_with_distant_quota_text)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == prompt:* ]]
}

@test "stall-watch: D1b posts blocked/quota: on the session-limit refusal, not quiet:" {
  p=$(fx_session_limit_refusal)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  want="blocked|watchdog|quota: session limit — do not re-dispatch; wait for the reset shown in pane %9, or a human can run /low-priority there (spends weekly budget) — Esc/Enter will not submit a queued prompt while the limit holds"
  [ "${lines[0]}" = "$want" ]
  # roster truncates detail to 120 chars, so the actionable guidance must
  # survive the cut.
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail | .[0:120]'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == *"do not re-dispatch"* ]]
}

@test "stall-watch: a session-limit quota: episode NEVER escalates" {
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  p=$(fx_session_limit_refusal)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 20
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quota:' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a session-limit quota: episode held past --idle and --dead still never escalates or gets superseded by quiet:" {
  # A frozen session-limit frame is byte-identical by construction, so it
  # satisfies D3 too: quiet: must not supersede quota:.
  p=$(fx_session_limit_refusal)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 8
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet:' || true"
  [ "$output" = "0" ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == blocked\|quota:* ]]
}

@test "stall-watch: the session-limit phrase far up-screen classifies as prompt:, not quota:" {
  p=$(fx_prompt_trust_with_distant_session_limit_text)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == prompt:* ]]
}

@test "stall-watch: D1's rate-limit quota: transitions cleanly to D1b's session-limit quota: and back" {
  q=$(fx_prompt_quota)
  s=$(fx_session_limit_refusal)
  stall_sampler "$q" "$q" "$s" "$s" "$q" "$q"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 18
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 5 ]
  [[ "${lines[0]}" == "blocked|quota: quota exhausted"* ]]
  [ "${lines[1]}" = "working|quota: cleared" ]
  [[ "${lines[2]}" == "blocked|quota: session limit"* ]]
  [ "${lines[3]}" = "working|quota: cleared" ]
  [[ "${lines[4]}" == "blocked|quota: quota exhausted"* ]]
}

@test "stall-watch: a quiet: episode with a dead engine process still escalates to failed" {
  export CREW_STALL_PROC_CMD='printf ""'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 15
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == blocked\|quiet:* ]]
  [[ "${lines[1]}" == "failed|dead: quiet: unchanged for "* ]]
}

@test "stall-watch: a quiet: episode with a live engine process does NOT escalate" {
  export CREW_STALL_PROC_CMD='printf claude'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 15
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == blocked\|quiet:* ]]
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D4 posts one blocked/load: once when 1m load exceeds cores for --load" {
  export CREW_STALL_LOAD_CMD='printf "99.9 32\n"'
  export CREW_STALL_TOP_CMD='printf "dispatcher/feat-187 yes 4242 99.0\n/tmp/x cc1 9999 44.0\n"'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --load 2 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|load: 1m load 99.9 on 32 cores for "*" (top: dispatcher/feat-187 yes 4242 99.0 | /tmp/x cc1 9999 44.0)" ]]
}

@test "stall-watch: D4 stays silent when the load is at or below the core count" {
  export CREW_STALL_LOAD_CMD='printf "1.0 32\n"'
  export CREW_STALL_TOP_CMD='printf ""'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --load 2 --max-life 5
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D4 ignores a transient burst shorter than --load" {
  stall_load_sampler "99.9 32" "1.0 32"
  export CREW_STALL_TOP_CMD='printf ""'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --load 5 --max-life 6
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D4 clears itself when the load drops back to the cores" {
  stall_load_sampler "99.9 32" "99.9 32" "99.9 32" "99.9 32" "99.9 32" "99.9 32" "99.9 32" "99.9 32" "1.0 32"
  export CREW_STALL_TOP_CMD='printf ""'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --load 2 --max-life 12
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == blocked\|load:* ]]
  [[ "${lines[1]}" == "working|load: cleared" ]]
}

@test "stall-watch: D4 load: is supersedable by quiet: and does not re-post stale" {
  # load: and quiet: are mutually non-sticky: a static pane lets quiet: overwrite
  # the load: detail; when load then drops, _post_clear "load:" no-ops on the
  # wrong prefix but d4_at still resets, so a later single high sample does NOT
  # re-post until the sustained --load window elapses again.
  stall_load_sampler "99.9 32" "99.9 32" "99.9 32" "99.9 32" "99.9 32" "99.9 32" "1.0 32" "99.9 32" "1.0 32"
  export CREW_STALL_TOP_CMD='printf ""'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --load 2 --max-life 14
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == blocked\|load:* ]]
  [[ "${lines[1]}" == blocked\|quiet:* ]]
  run bash -c "bus | grep -c 'load: cleared' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D4 stays silent on a malformed load read" {
  export CREW_STALL_LOAD_CMD='printf "garbage\n"'
  export CREW_STALL_TOP_CMD='printf ""'
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --load 2 --max-life 4
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D4 runs engine-independent (codex and no --engine)" {
  for eng in codex ""; do
    rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
    export CREW_STALL_LOAD_CMD='printf "99.9 32\n"'
    export CREW_STALL_TOP_CMD='printf ""'
    p=$(fx_idle_box)
    stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
    if [ -n "$eng" ]; then
      CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine "$eng" \
        --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --load 2 --max-life 8
    else
      CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 \
        --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --load 2 --max-life 8
    fi
    run bash -c "bus | grep -c 'blocked' || true"
    [ "$output" = "1" ]
    run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
    [[ "$output" == load:* ]]
  done
}

# ---------------------------------------------------------------------------
# stall-watch: role mode
# ---------------------------------------------------------------------------

@test "stall-watch: role mode posts blocked/prompt: under the role id, never a worker row" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.from)|\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "role:feat/x:reviewer|blocked|watchdog|prompt: interactive prompt in pane %9 —"* ]]
  run bash -c "bus | grep -c '\"from\":\"worker:feat/x\"' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: role mode posts nothing for a static pane past --idle and --window" {
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine claude \
    --grace 0 --interval 1 --window 1 --stall 1 --idle 1 --dead 999 --max-life 20
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: role mode clears prompt: to working under the role id" {
  p=$(fx_prompt_trust)
  q=$(fx_idle_box)
  stall_sampler "$p" "$p" "$q" GONE
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 20
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.from)|\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == role:feat/x:reviewer\|blocked\|prompt:* ]]
  [ "${lines[1]}" = "role:feat/x:reviewer|working|prompt: cleared" ]
}

@test "stall-watch: a role failed status on the bus exits the watch immediately" {
  seed_raw role:feat/x:reviewer failed "gate red" ""
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'watchdog' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: role mode exits once the engine pane returns to a bare shell" {
  # The pane stays sampleable forever and --max-life is far off, so only the
  # bare-shell check can end the watch within a tick of the shell appearing.
  # The shell is keyed to the sample count, not wall time: the count left in
  # $SAMPLER_DIR/n says exactly which tick the watch stopped on.
  p=$(fx_idle_box)
  stall_sampler "$p"
  export CREW_STALL_PROC_CMD="[ \"\$(cat '$SAMPLER_DIR/n')\" -ge 3 ] && printf fish || printf claude"
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 30
  [ "$status" -eq 0 ]
  n="$(cat "$SAMPLER_DIR/n")"
  [ "$n" -ge 3 ]
  [ "$n" -le 4 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: role mode never calls tmux set-option for @crew_state" {
  stub_bin tmux
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run ! grep -q '@crew_state' "$STUB_LOG"
}

@test "roster: carries source and truncates detail to 120 chars" {
  long=$(printf 'quiet: %0.sx' $(seq 1 200))
  seed_raw worker:feat/x blocked "$long" watchdog
  CREW_ID=c1 run run_crew roster c1
  [ "$status" -eq 0 ]
  run bash -c "printf '%s' '$output' | jq -r '.[0] | \"\(.source)|\(.detail|length)\"'"
  [ "$output" = "watchdog|120" ]
}

@test "roster: a worker-posted row has a null source" {
  CREW_ID=c1 run_crew status worker:feat/x blocked "which approach?"
  CREW_ID=c1 run run_crew roster c1
  run bash -c "printf '%s' '$output' | jq -r '.[0] | \"\(.source)|\(.detail)\"'"
  [ "$output" = "null|which approach?" ]
}

@test "rate: blocked_count excludes watchdog blocks, watchdog_blocked_count counts them" {
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc '{ts:1,crew_id:"c1",kind:"dispatch",branch:"feat/x",engine:"claude",
           model:"opus",tier:"deep",effort:"high",title:"t"}' >>"$logf"
  seed_raw worker:feat/x blocked "which approach?" "" 2
  seed_raw worker:feat/x blocked "prompt: interactive prompt in pane %9 — waiting" watchdog 3
  seed_raw worker:feat/x blocked "quiet: pane unchanged for 1800s" watchdog 4
  store="$BATS_TEST_TMPDIR/xdg"
  XDG_DATA_HOME="$store" CREW_ID=c1 run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '"\(.blocked_count)|\(.watchdog_blocked_count)"' "$store/crew/ratings.jsonl"
  [ "$output" = "1|2" ]
}

# Contract pin, not a behaviour test: rate already passes any review_mode
# string through, so this passed before the value existed. It stops a later
# rate edit swallowing `unavailable` — it does not verify a worker emits it.
@test "rate: passes review_mode unavailable through untouched" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  cat >"$log" <<'EOF'
{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/review-unavailable","engine":"codex","model":"terra","tier":"standard","effort":"medium","title":"review gate unavailable"}
{"ts":1001,"crew_id":"c1","kind":"status","from":"worker:feat/review-unavailable","body":{"state":"failed"}}
{"ts":1002,"crew_id":"c1","kind":"msg","from":"worker:feat/review-unavailable","to":"metrics:c1","body":"{\"review_mode\":\"unavailable\",\"review_high\":null}"}
EOF
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"

  run run_crew rate
  [ "$status" -eq 0 ]

  run jq -s -e '
    map(select(.branch=="feat/review-unavailable"))
    | .[0]
    | {review_mode, outcome, reached_pr} == {review_mode:"unavailable", outcome:"failed", reached_pr:false}
  ' "$XDG_DATA_HOME/crew/ratings.jsonl"
  [ "$status" -eq 0 ]
}

@test "burn map conformance: the doc slice names every classed rung" {
  # Copied from the Model map/Burn classes table, so it makes drift loud
  # rather than impossible — same tolerance as dispatch.bats's precedent
  # "tier map conformance" test. kimi-k3* is deliberately excluded: the burn
  # doc does not class it and _burn_weight falls it through to "" on purpose.
  # The cursor rungs listed are the non-fast defaults; `-fast` is a price
  # multiplier the sibling behavioural test below pins.
  doc="$BATS_TEST_DIRNAME/../adapters/core/protocols/dispatch-orchestration.md"
  doc_slice="$(sed -n '/^## Model map/,/^### Tier map/p' "$doc")"
  for token in opus sonnet haiku fable composer-2.5 \
    gpt-5.6-luna gpt-5.6-terra gpt-5.6-sol \
    grok-4.7-low grok-4.7-medium grok-4.7-high \
    claude-fable-5; do
    grep -qF "$token" <<<"$doc_slice" || {
      printf 'token %s missing from the Model map/Burn classes doc slice\n' "$token" >&2
      return 1
    }
  done
}

# _burn_weight matches on glob families (`*grok-4.[0-9]-medium`) so that 4.5,
# 4.6, and the prefix-less 4.7 ids price alike, which a token grep of the
# function body cannot see.
# Assert the rule by calling it.
@test "burn map conformance: _burn_weight prices effort and -fast on separate axes" {
  weight() {
    bash -c 'source /dev/stdin <<<"$(sed -n "/^_burn_weight() {/,/^}/p" "$1")"; _burn_weight "$2"' _ "$CREW" "$1"
  }
  while read -r model expected; do
    [ -n "$model" ] || continue
    got="$(weight "$model" | tr '\t' ' ')"
    [ "$got" = "$expected" ] || {
      printf '_burn_weight %s = "%s", expected "%s"\n' "$model" "$got" "$expected" >&2
      return 1
    }
  done <<'EOF'
opus premium 4
sonnet standard 2
haiku cheap 1
claude-fable-5 fable 8
gpt-5.6-sol premium 4
gpt-5.6-terra standard 2
gpt-5.6-luna cheap 1
composer-2.5 free 0
composer-2.5-fast free 0
cursor-grok-4.6-low cheap 1
cursor-grok-4.6-medium standard 2
cursor-grok-4.6-high premium 4
cursor-grok-4.6-xhigh premium 6
cursor-grok-4.5-low cheap 1
cursor-grok-4.5-medium standard 2
cursor-grok-4.5-high premium 4
cursor-grok-4.6-low-fast standard 2
cursor-grok-4.6-medium-fast premium 4
cursor-grok-4.6-high-fast premium 8
cursor-grok-4.6-xhigh-fast premium 12
grok-4.7-low cheap 1
grok-4.7-medium standard 2
grok-4.7-high premium 4
grok-4.7-xhigh premium 6
grok-4.7-low-fast standard 2
grok-4.7-medium-fast premium 4
grok-4.7-high-fast premium 8
grok-4.7-xhigh-fast premium 12
EOF
  # kimi-k3-high and an unknown id stay unclassed rather than guessed.
  [ -z "$(weight kimi-k3-high)" ]
  [ -z "$(weight some-unknown-model)" ]
}

# ---------------------------------------------------------------------------
# watch --crew / stream harness
# ---------------------------------------------------------------------------

# crew_dir [id] — the per-crew state dir `watch` and `stream` share.
crew_dir() {
  printf '%s' "$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/${1:-c1}"
}

# poll_for <tries> <cmd…> — run cmd every 0.1s until it succeeds, at most
# <tries> times. Every wait below is a bounded poll rather than a fixed sleep:
# a state change that already happened costs nothing, and one that never
# happens fails the test instead of hanging a --jobs 16 run.
poll_for() {
  local tries="$1" i=0
  shift
  while [ "$i" -lt "$tries" ]; do
    "$@" && return 0
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# start_stream <args…> — launch `crew stream` as a REAL process. Never through
# run_crew: backgrounding a shell function makes $! the subshell's pid, so the
# --status pid comparison, the --force reclaim and every kill below would name
# the wrong process. </dev/null keeps it off bats' own pipes.
start_stream() {
  STREAM_N=$((${STREAM_N:-0} + 1))
  STREAM_OUT="$BATS_TEST_TMPDIR/stream.$STREAM_N.out"
  STREAM_ERR="$BATS_TEST_TMPDIR/stream.$STREAM_N.err"
  # Recorded so stop_stream polls the right crew's lock dir instead of always
  # c1's — a future test using --crew c2 would otherwise wait uselessly on c1
  # and then force-KILL a stream mid-shutdown.
  local args=("$@") i
  for ((i = 0; i < ${#args[@]}; i++)); do
    [ "${args[i]}" = --crew ] && STREAM_CREW_ID="${args[i + 1]}"
  done
  bash -euo pipefail "${STREAM_CREW:-$CREW}" stream "$@" >"$STREAM_OUT" 2>"$STREAM_ERR" </dev/null &
  STREAM_PID=$!
  STREAM_PIDS="${STREAM_PIDS:-} $STREAM_PID"
}

# stop_stream — TERM every stream this test started, then reap it. The lock dir
# is the last thing the cleanup handler removes before its `exit 0`, so polling
# that bounds the wait; `kill -0` cannot, because a TERM'd child stays a
# not-yet-reaped zombie that still answers it.
stop_stream() {
  local spid
  [ -n "${STREAM_PIDS:-}" ] || return 0
  for spid in $STREAM_PIDS; do
    kill -TERM "$spid" 2>/dev/null || true
  done
  poll_for 60 test ! -d "$(crew_dir "${STREAM_CREW_ID:-c1}")/stream.lock.d" || {
    for spid in $STREAM_PIDS; do
      kill -KILL "$spid" 2>/dev/null || true
    done
  }
  for spid in $STREAM_PIDS; do
    wait "$spid" 2>/dev/null || true
  done
  STREAM_PIDS=""
  STREAM_PID=""
  STREAM_CREW_ID=""
}

# spawn_holder — set HOLDER_PID to a live pid that is NOT a child of this
# shell, for the lock `--force` has to reclaim. A child TERM'd by --force would
# sit unreaped, and `kill -0` on that zombie still succeeds, so --force's
# bounded wait could never see it clear. Called plainly and never through a
# command substitution: under bats the holder does not survive one.
spawn_holder() {
  local pf="$BATS_TEST_TMPDIR/holder.pid"
  (
    sleep 30 >/dev/null 2>&1 </dev/null &
    printf '%s' "$!" >"$pf"
  )
  HOLDER_PID="$(cat "$pf")"
}

# Predicates for poll_for.
stream_lines() { grep -c . "$STREAM_OUT" 2>/dev/null || true; }
at_least_lines() { [ "$(stream_lines)" -ge "$1" ]; }
lock_pid_is() { [ "$(cat "$(crew_dir)/stream.lock.d/pid" 2>/dev/null || true)" = "$1" ]; }
tick_ts() { jq -r '.ts // empty' "$(crew_dir)/stream.tick" 2>/dev/null || true; }
tick_after() {
  local t
  t="$(tick_ts)"
  [ -n "$t" ] && [ "$t" -gt "$1" ]
}
proc_gone() { ! kill -0 "$1" 2>/dev/null; }

# add_hold <resets_at> [title] — crew hold add for crew c1, every other
# required flag pinned to an arbitrary fixed value: only --resets-at and the
# title (to tell holds apart) vary per test. Prints the minted id.
add_hold() {
  run_crew hold add --crew c1 --engine claude --window 5h --resets-at "$1" \
    --agent claude --ref FOO-1 --branch feat/x --tier standard --model sonnet \
    --effort medium "${2:-hold}"
}

# Predicates and readers for the hold_due stream tests below.
hold_due_lines() { grep -c '"stream":"hold_due"' "$STREAM_OUT" 2>/dev/null || true; }
at_least_hold_due_lines() { [ "$(hold_due_lines)" -ge "$1" ]; }
heartbeat_seen() { grep -q '"stream":"heartbeat"' "$STREAM_OUT" 2>/dev/null; }
heartbeat_line() { grep '"stream":"heartbeat"' "$STREAM_OUT" | head -n1; }

@test "watch: --crew resolves that crew with CREW_ID unset and no WORKER_TASK.md" {
  CREW_ID=c1 run_crew status worker:feat/x done

  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].crew_id == "c1"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "watch: an explicit --crew beats a WORKER_TASK.md at the repo top" {
  CREW_ID=c1 run_crew status worker:feat/x done
  printf 'crew_id: c9\n' >WORKER_TASK.md

  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '.events[0].crew_id == "c1"' <<<"$output"
  [ "$status" -eq 0 ]

  # Without the flag the same call resolves c9 and sees nothing.
  run --separate-stderr run_crew watch --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "watch: --crew leaves the wake set alone — a working status still does not wake it" {
  CREW_ID=c1 run_crew status worker:feat/x working

  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "watch: --crew uses that crew's cursor file and watch lock" {
  cdir="$(crew_dir c1)"
  mkdir -p "$cdir/watch.lock.d"
  printf '%s\n' "$$" >"$cdir/watch.lock.d/pid"

  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 1 ]
  [ "$stderr" = "crew: another watch is already running for this crew (c1)" ]

  rm -rf "$cdir/watch.lock.d"
  CREW_ID=c1 run_crew status worker:feat/x done
  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e --argjson cursor "$(cat "$cdir/cursor")" '.cursor == $cursor' <<<"$output"
  [ "$status" -eq 0 ]
  [ ! -d "$cdir/watch.lock.d" ]
}

# The wake gate for `exited` (#396): a mid-run engine death (its previous state
# was working/blocked) must wake the park, while the ordinary SessionEnd backstop
# posted after the session's own terminal state must stay silent.
@test "watch: a mid-run exited wakes — working then exited" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" working "" "" "$t"
  seed_raw "worker:feat/x#s1-1" exited "" "" "$((t + 1000))"

  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.state == "exited"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "watch: an exited after the session's own done does not wake" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" working "" "" "$t"
  seed_raw "worker:feat/x#s1-1" done "" "" "$((t + 1000))"
  seed_raw "worker:feat/x#s1-1" exited "" "" "$((t + 2000))"

  # Only the `exited` is newer than the cursor the dispatcher would hold after
  # handling `done` — it must not re-wake for the backstop.
  run --separate-stderr run_crew watch --crew c1 --since "$((t + 1000))" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "watch: a mid-run exited wakes — blocked then exited" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "" "" "$t"
  seed_raw "worker:feat/x#s1-1" exited "" "" "$((t + 1000))"

  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.state == "exited"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "stream: a batch line passes through verbatim and parses as {cursor, events}" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  CREW_ID=c1 run_crew status worker:feat/x done
  poll_for 100 at_least_lines 1

  line="$(head -n1 "$STREAM_OUT")"
  # Verbatim: the compact single line `watch` printed, not a re-encoding.
  [ "$line" = "$(jq -c . <<<"$line")" ]
  run jq -e '(.events | length) == 1 and .events[0].body.state == "done"
    and .cursor == .events[0].ts' <<<"$line"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream: a mid-run exited wakes the lane" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" exited
  poll_for 100 at_least_lines 1

  line="$(head -n1 "$STREAM_OUT")"
  run jq -e '(.events | length) == 1 and .events[0].body.state == "exited"' <<<"$line"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream: events posted inside the coalesce window arrive as ONE batch line [F1]" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 3 --heartbeat 3600 --retry 1
  CREW_ID=c1 run_crew status worker:feat/a done
  poll_for 100 at_least_lines 1
  t0="$(jq -nc 'now*1000|floor')"

  # The stream is now inside its --coalesce sleep.
  CREW_ID=c1 run_crew status worker:feat/b done
  CREW_ID=c1 run_crew status worker:feat/c done
  CREW_ID=c1 run_crew status worker:feat/d done

  poll_for 100 at_least_lines 2
  t1="$(jq -nc 'now*1000|floor')"
  [ "$(stream_lines)" -eq 2 ]
  run jq -e '(.events | length) == 3' <<<"$(tail -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]
  # And it is the --coalesce floor that made it one line, not the inner watch's
  # own --interval, which coalesces a burst for free: the second batch cannot
  # arrive before the sleep ends. Half a second of slack for the poll that
  # observed the first line — a lower bound never flakes upward.
  [ "$((t1 - t0))" -ge 2500 ]
  stop_stream
}

@test "stream: a heartbeat appears only after --heartbeat of quiet, not once per park [F6]" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3 --retry 1
  poll_for 200 at_least_lines 1

  # quiet_s, never wall clock: `watch` checks its deadline only after sleeping
  # --interval, so an inner --park 1 costs ~2s and elapsed time diverges from
  # the counter. Three parks' worth of quiet, one line.
  [ "$(stream_lines)" -eq 1 ]
  run jq -e '.stream == "heartbeat" and .crew == "c1" and .quiet_s == 3' <<<"$(head -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream: an inner failure does not kill the loop — one line per error key, re-emitted when the key changes [F8]" {
  cdir="$(crew_dir c1)"
  # A live holder of the crew's watch lock: every inner watch exits 1 with the
  # same first stderr line, so the suppression key is stable.
  mkdir -p "$cdir/watch.lock.d"
  printf '%s\n' "$$" >"$cdir/watch.lock.d/pid"
  # Run the stream from a copy, so the fault below can be swapped in without
  # touching the file the rest of the suite runs.
  STREAM_CREW="$BATS_TEST_TMPDIR/crew-copy.sh"
  cp "$CREW" "$STREAM_CREW"

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 200 at_least_lines 1
  run jq -e '.stream == "error" and .rc == 1
    and (.detail | test("another watch is already running"))' <<<"$(head -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]

  # Two more iterations of the same failure emit nothing: the tick, rewritten at
  # the top of every iteration, is the evidence the loop is still turning.
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  [ "$(stream_lines)" -eq 1 ]

  # A different key IS re-emitted. The replacement must be atomic (write a
  # temp file, then `mv -f` it into place) and scoped to `watch` only: an
  # unlink-then-create leaves a window where $STREAM_CREW is briefly absent,
  # which a fork landing there turns into a THIRD, unrelated error key
  # (rc=127, "No such file or directory"); and faulting every subcommand
  # (not just `watch`) makes the loop's own `hold due` pre-check fail too,
  # under an independent suppression key, so its "hold due: ..." line can
  # race ahead of the "crew: fault injected" line this test asserts on. Not
  # by making the inner `mkdir -p "$cdir"` fail: the stream's own tick write
  # and its $outf redirect are in that same directory, so a file there kills
  # the loop under `set -e` instead of failing one iteration of it — and the
  # assertion below would then hold vacuously.
  rm -rf "$cdir/watch.lock.d"
  tmp=$(mktemp "$BATS_TEST_TMPDIR/crew-copy.XXXXXX")
  printf '%s\n' '#!/usr/bin/env bash' \
    'case "${1:-}" in' \
    '  watch) echo "crew: fault injected" >&2; exit 3 ;;' \
    '  *) exit 0 ;;' \
    'esac' >"$tmp"
  mv -f "$tmp" "$STREAM_CREW"

  poll_for 200 at_least_lines 2
  [ "$(stream_lines)" -eq 2 ]
  run jq -e '.stream == "error" and .rc == 3 and .detail == "crew: fault injected"' <<<"$(tail -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream: a matured hold produces exactly one hold_due line, not one per park" {
  # Seeded already-matured rather than added a second in the future: `hold add`
  # refuses a past --resets-at, so a near-future add races its own fork+jq cost
  # against the deadline it just set, and loses under --jobs 16.
  id=h1
  seed_hold c1 "$id" "$(($(date +%s) - 5))"

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 200 at_least_hold_due_lines 1

  # Two more iterations of the same matured hold emit nothing further — the
  # tick, rewritten at the top of every iteration, is the evidence the loop
  # is still turning (same idiom as the error-suppression test above).
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  [ "$(hold_due_lines)" -eq 1 ]

  run jq -e --arg id "$id" \
    '.stream == "hold_due" and .crew == "c1" and (.holds | map(.id) == [$id])' \
    <<<"$(head -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream: an unmatured hold produces no hold_due line" {
  now=$(date +%s)
  add_hold "$((now + 3600))" future

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 100 lock_pid_is "$STREAM_PID"
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"

  [ "$(hold_due_lines)" -eq 0 ]
  [ "$(stream_lines)" -eq 0 ]
  stop_stream
}

@test "stream: releasing one of two matured holds re-announces the other on the next iteration, no restart" {
  # Both seeded to the same past instant, so they mature together by
  # construction — two near-future adds cannot be made simultaneous.
  id1=h1
  id2=h2
  matured_at=$(($(date +%s) - 5))
  seed_hold c1 "$id1" "$matured_at"
  seed_hold c1 "$id2" "$matured_at"
  expected=$(jq -nr --arg a "$id1" --arg b "$id2" '[$a, $b] | sort | join(",")')

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 200 at_least_hold_due_lines 1

  ids1=$(jq -r '.holds | map(.id) | sort | join(",")' <<<"$(head -n1 "$STREAM_OUT")")
  [ "$ids1" = "$expected" ]

  run_crew hold release "$id1" --crew c1
  poll_for 200 at_least_hold_due_lines 2

  # The re-announcement is keyed on the matured id SET changing, not on a
  # count — assert the surviving id, not just that a second line arrived.
  line2=$(grep '"stream":"hold_due"' "$STREAM_OUT" | sed -n '2p')
  ids2=$(jq -r '.holds | map(.id) | sort | join(",")' <<<"$line2")
  [ "$ids2" = "$id2" ]

  # No further re-announcement while the set stays put and --heartbeat has
  # not elapsed.
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  [ "$(hold_due_lines)" -eq 2 ]
  stop_stream
}

@test "stream: hold_due does not reset quiet — the heartbeat still fires on schedule" {
  seed_hold c1 h1 "$(($(date +%s) - 5))"

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3 --retry 1
  poll_for 200 at_least_hold_due_lines 1
  poll_for 300 heartbeat_seen

  # quiet_s == 3, the same value the plain heartbeat test (--heartbeat 3,
  # --park 1) asserts: three parks' worth of quiet, undisturbed by hold_due
  # firing on the same iterations.
  run jq -e '.stream == "heartbeat" and .quiet_s == 3' <<<"$(heartbeat_line)"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream: a failing hold due is announced as an error, not swallowed as no-holds" {
  # `hold due` exits 1 by design when nothing is matured, so the loop cannot
  # treat every nonzero rc as silence — a real failure there is the one branch
  # nothing else watches. Fault only `hold due`: the stream re-execs the same
  # $0 for `watch` too, and failing both would leave the inner-watch error
  # line indistinguishable from this one.
  STREAM_CREW="$BATS_TEST_TMPDIR/crew-holdfault.sh"
  {
    head -n1 "$CREW"
    printf '%s\n' 'if [ "${1:-}" = hold ] && [ "${2:-}" = due ]; then echo "crew: hold fault injected" >&2; exit 3; fi'
    tail -n +2 "$CREW"
  } >"$STREAM_CREW"
  chmod +x "$STREAM_CREW"

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 200 at_least_lines 1

  run jq -e '.stream == "error" and .rc == 3
    and .detail == "hold due: crew: hold fault injected"' <<<"$(head -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]

  # Suppressed like the inner-watch error path: the same failure on later
  # iterations adds no line, and the tick proves the loop is still turning.
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  t="$(tick_ts)"
  poll_for 100 tick_after "$t"
  [ "$(stream_lines)" -eq 1 ]
  stop_stream
}

@test "stream: a crew with no holds streams batches and heartbeats unchanged" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3 --retry 1
  CREW_ID=c1 run_crew status worker:feat/x done
  poll_for 100 at_least_lines 1
  run jq -e '(.events | length) == 1 and .events[0].body.state == "done"' \
    <<<"$(head -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]

  poll_for 200 at_least_lines 2
  run jq -e '.stream == "heartbeat" and .quiet_s == 3' <<<"$(tail -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]
  [ "$(stream_lines)" -eq 2 ]
  [ "$(hold_due_lines)" -eq 0 ]
  stop_stream
}

@test "stream --status: no lock is dead, exit 2" {
  run --separate-stderr run_crew stream --status --crew c1
  [ "$status" -eq 2 ]
  run jq -e '.stream == "status" and .state == "dead" and .crew == "c1"
    and .pid == null and .age_s == null' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "stream --status: a live stream is alive, exit 0" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 100 lock_pid_is "$STREAM_PID"

  run --separate-stderr run_crew stream --status --crew c1
  [ "$status" -eq 0 ]
  run jq -e --argjson pid "$STREAM_PID" '.state == "alive" and .pid == $pid and .age_s >= 0' <<<"$output"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream --status: a live pid whose tick stopped moving is stale, exit 1 [F4]" {
  cdir="$(crew_dir c1)"
  mkdir -p "$cdir/stream.lock.d"
  printf '%s\n' "$$" >"$cdir/stream.lock.d/pid"

  # A live lock with no tick yet counts as stale — the safe direction.
  run --separate-stderr run_crew stream --status --crew c1
  [ "$status" -eq 1 ]
  run jq -e '.state == "stale" and .age_s == null' <<<"$output"
  [ "$status" -eq 0 ]

  # …and so does one aged past 2 × the park the tick itself recorded, + 60.
  now="$(jq -nc 'now*1000|floor')"
  jq -nc --argjson pid "$$" --argjson ts "$((now - 121000))" '{pid:$pid, ts:$ts, park:1}' >"$cdir/stream.tick"
  run --separate-stderr run_crew stream --status --crew c1
  [ "$status" -eq 1 ]
  run jq -e '.state == "stale" and .age_s >= 121' <<<"$output"
  [ "$status" -eq 0 ]

  # Same live pid, fresh tick: alive. So it is the tick that decides.
  jq -nc --argjson pid "$$" --argjson ts "$now" '{pid:$pid, ts:$ts, park:1}' >"$cdir/stream.tick"
  run --separate-stderr run_crew stream --status --crew c1
  [ "$status" -eq 0 ]
  run jq -e '.state == "alive"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "stream: crew-resolution and usage failures exit 64, not 1 [C2]" {
  run --separate-stderr run_crew stream --status
  [ "$status" -eq 64 ]
  [ "$stderr" = "crew: CREW_ID unset and no WORKER_TASK.md crew_id" ]

  run --separate-stderr run_crew stream --crew c1 --bogus
  [ "$status" -eq 64 ]
  [ "$stderr" = "crew: stream: unknown arg '--bogus'" ]

  run --separate-stderr run_crew stream --crew c1 --park 0
  [ "$status" -eq 64 ]
}

@test "stream: a second stream for one crew is refused" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 100 lock_pid_is "$STREAM_PID"

  run --separate-stderr run_crew stream --crew c1 --park 1 --interval 1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"another stream is already running for this crew (c1)"* ]]
  [[ "$stderr" == *"pid $STREAM_PID"* ]]

  # The refusal must leave the incumbent's lock alone.
  lock_pid_is "$STREAM_PID"
  stop_stream
}

@test "stream: --force reclaims a lock held by a live pid [B3]" {
  cdir="$(crew_dir c1)"
  mkdir -p "$cdir/stream.lock.d"
  spawn_holder
  # A dead holder would be reclaimed by _lock_acquire itself and --force would
  # never be exercised, so the liveness is an assertion, not an assumption.
  kill -0 "$HOLDER_PID"
  printf '%s\n' "$HOLDER_PID" >"$cdir/stream.lock.d/pid"

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1 --force
  poll_for 100 lock_pid_is "$STREAM_PID"
  proc_gone "$HOLDER_PID"
  stop_stream
}

@test "stream: --force refuses to signal a lock pid file containing 0" {
  cdir="$(crew_dir c1)"
  mkdir -p "$cdir/stream.lock.d"
  printf '%s\n' 0 >"$cdir/stream.lock.d/pid"

  # 0 as a signal target hits our whole process group, and `_lock_acquire`'s
  # own `kill -0` reads it as live — so --force must refuse before it ever
  # calls `kill -TERM` on it.
  run --separate-stderr run_crew stream --crew c1 --force --park 1 --interval 1
  [ "$status" -eq 1 ]
  [ "$stderr" = "crew: --force found no valid holder pid for crew (c1) stream lock (got '0') — refusing to signal" ]

  # The lock is untouched — refused, not reclaimed.
  [ -d "$cdir/stream.lock.d" ]
  [ "$(cat "$cdir/stream.lock.d/pid")" = 0 ]
}

@test "watch and stream: a traversal-shaped --crew is refused, writing nothing outside the bus dir" {
  common="$(git rev-parse --path-format=absolute --git-common-dir)"

  run --separate-stderr run_crew watch --crew '../../escaped' --timeout 1 --interval 1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"invalid crew id"* ]]
  [ ! -e "$common/escaped" ]
  [ ! -e "$common/crew/escaped" ]

  run --separate-stderr run_crew stream --crew '../../escaped' --park 1 --interval 1
  [ "$status" -eq 64 ]
  [[ "$stderr" == *"invalid crew id"* ]]
  [ ! -e "$common/escaped" ]
  [ ! -e "$common/crew/escaped" ]
}

@test "stream: a TERM mid-park leaves no live watch, a released lock and an unadvanced cursor [F2]" {
  cdir="$(crew_dir c1)"
  # --park well past the assertions below, so an orphan that outlives the
  # stream is still alive to be caught: a park short enough to expire during
  # the test would release the lock and stop advancing the cursor on its own,
  # and every assertion here would pass with no child kill at all.
  start_stream --crew c1 --park 10 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 100 test -f "$cdir/watch.lock.d/pid"
  wpid="$(cat "$cdir/watch.lock.d/pid")"

  kill -TERM "$STREAM_PID"
  wait "$STREAM_PID" 2>/dev/null || true
  STREAM_PIDS=""
  STREAM_PID=""

  # Bounded by one --interval: the inner watch runs its EXIT trap only once its
  # sleep returns, so an immediate check would pass on a watch still alive.
  poll_for 20 proc_gone "$wpid"
  [ ! -d "$cdir/watch.lock.d" ]
  [ ! -e "$cdir/cursor" ]

  # The assertion that matters: an orphan would print a batch to a stdout
  # nobody reads and still mv its cursor into place, which a lock check cannot
  # see — a watch can advance the cursor and release the lock on the same exit.
  CREW_ID=c1 run_crew status worker:feat/x done
  sleep 2
  [ ! -e "$cdir/cursor" ]
}

@test "stream: a TERM with an undrained stream.out still emits that batch [B1]" {
  cdir="$(crew_dir c1)"
  # No qualifying event, so the inner watch is parked in its sleep with nothing
  # written — the natural window is microseconds wide, so the test plants the
  # undrained batch itself.
  start_stream --crew c1 --park 5 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  poll_for 100 test -f "$cdir/watch.lock.d/pid"

  batch='{"cursor":1,"events":[{"ts":1,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}]}'
  printf '%s\n' "$batch" >"$cdir/stream.out"

  kill -TERM "$STREAM_PID"
  wait "$STREAM_PID" 2>/dev/null || true
  STREAM_PIDS=""
  STREAM_PID=""

  # Sound only because the cleanup kills AND reaps the child before draining:
  # an unreaped child could overwrite the file from its offset-0 fd mid-drain.
  [ "$(cat "$STREAM_OUT")" = "$batch" ]
  [ ! -e "$cdir/stream.out" ]
}

@test "stream: writes nothing to its own stderr in normal operation [F8]" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 2 --retry 1
  CREW_ID=c1 run_crew status worker:feat/x done

  # Both printing paths, and the park-expiry line `watch` writes to ITS stderr
  # on every iteration.
  poll_for 200 at_least_lines 2
  run jq -e -s '.[0].cursor != null and .[1].stream == "heartbeat"' "$STREAM_OUT"
  [ "$status" -eq 0 ]
  stop_stream
  [ ! -s "$STREAM_ERR" ]
}

@test "stream: a pre-seeded cursor is honoured, so --since is never passed" {
  cdir="$(crew_dir c1)"
  CREW_ID=c1 run_crew status worker:feat/old done
  mkdir -p "$cdir"
  bus | jq -r '.ts' | tail -n1 >"$cdir/cursor"

  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  CREW_ID=c1 run_crew status worker:feat/new done
  poll_for 100 at_least_lines 1

  [ "$(stream_lines)" -eq 1 ]
  run jq -e '(.events | length) == 1 and .events[0].from == "worker:feat/new"' <<<"$(head -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]
  run jq -e --argjson cursor "$(cat "$cdir/cursor")" '.cursor == $cursor' <<<"$(head -n1 "$STREAM_OUT")"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "identity: a recorded name another live worker now holds is not reused" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/1-a","name":"nova","color":"magenta","tmux":"colour127"}' >>"$dir/events.jsonl"
  printf '%s\n' '{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/2-b","name":"nova","color":"magenta","tmux":"colour127"}' >>"$dir/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/1-a#s1-1" working
  [ "$(run_crew identity feat/2-b c1 | jq -r .name)" != "nova" ]
  CREW_ID=c1 run_crew status "worker:feat/1-a#s1-1" done
  [ "$(run_crew identity feat/2-b c1 | jq -r .name)" = "nova" ]
}

@test "identity: an unrecorded branch resolves with no tmux available" {
  mkdir -p "$BATS_TEST_TMPDIR/fakebin"
  printf '#!/bin/sh\nexit 1\n' >"$BATS_TEST_TMPDIR/fakebin/tmux"
  chmod +x "$BATS_TEST_TMPDIR/fakebin/tmux"
  PATH="$BATS_TEST_TMPDIR/fakebin:$PATH" run run_crew identity feat/9-new c1
  [ "$status" -eq 0 ]
  echo "$output" | jq -e 'has("name")'
}

@test "identity: of two live branches sharing a recorded name the earlier keeps it" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/1-a","name":"nova","color":"magenta","tmux":"colour127"}' >>"$dir/events.jsonl"
  printf '%s\n' '{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/2-b","name":"nova","color":"magenta","tmux":"colour127"}' >>"$dir/events.jsonl"
  [ "$(run_crew identity feat/1-a c1 | jq -r .name)" = "nova" ]
  [ "$(run_crew identity feat/2-b c1 | jq -r .name)" != "nova" ]
}

# --- terminal status gate: review seam (#241, #306) ---

_task_doc() { printf 'tier: %s\nkind: %s\nengine: %s\ncrew_id: c1\n' "$1" "${2:-implement}" "${3:-claude}" >WORKER_TASK.md; }
_acceptance_doc() {
  _task_doc "$@"
  printf '\n## Acceptance\n- AC1\n' >>WORKER_TASK.md
}
_status_rows() { jq -c 'select(.kind=="status")' "$(git rev-parse --git-common-dir)/crew/events.jsonl" 2>/dev/null | wc -l; }
_refused() {
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$1"* ]]
  [ "$(_status_rows)" -eq 0 ]
}
_deslop_seam() { run_crew msg "${1:-worker:feat/x#s1-1}" "review:c1" '{"seam":"deslop"}'; }

@test "pr_open: standard with no review seam is refused and not written" {
  _task_doc standard
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "done: standard with no review seam is refused and not written" {
  _task_doc standard
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: deep with no review seam is refused and not written" {
  _task_doc deep
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: standard with a review seam posts silently" {
  _task_doc standard
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"review","review_mode":"full"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats)" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(jq -r 'select(.kind=="status") | .body.state' "$(git rev-parse --git-common-dir)/crew/events.jsonl")" = pr_open ]
}

@test "pr_open: a resumed session inherits the branch's earlier review seam" {
  _task_doc deep
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"review"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  run --separate-stderr run_crew status "worker:feat/x#s2-2" done "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ "$(jq -r 'select(.kind=="status") | .body.state' "$(git rev-parse --git-common-dir)/crew/events.jsonl" | tr '\n' ' ')" = "pr_open done " ]
}

@test "pr_open: the grid reviewer assignment alone is not a review seam" {
  _task_doc standard implement pi
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"seam":"review","artifact":"/a/review.diff","roster":"/a/roster.json","question":"Review this diff."}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a pi reviewer accept alone counts as a review seam" {
  _task_doc standard implement pi
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"role":"reviewer","seam":"review","verdict":"accept","findings":[],"evidence":"x"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ "$(_status_rows)" -eq 1 ]
}

_verdict() { run_crew msg "role:feat/x:reviewer" "${2:-worker:feat/x#s1-1}" "{\"role\":\"reviewer\",\"seam\":\"review\",\"verdict\":\"$1\"}"; }
_assign() { run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"seam":"review","artifact":"/a/review.diff","question":"Review this diff."}'; }
_lead_seam() {
  local body='{"seam":"review","review_mode":"full"}'
  run_crew msg "worker:feat/x#s1-1" "review:c1" "${1:-$body}"
}
_gate() { run --separate-stderr run_crew status "worker:feat/x#${1:-s1-1}" pr_open "" https://example.com/pr/1; }
_allowed() {
  [ "$status" -eq 0 ]
  [ "$(_status_rows)" -eq 1 ]
}

@test "pr_open: a pi reviewer revise alone is refused" {
  _task_doc standard implement pi
  _verdict revise
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi reviewer reject alone is refused" {
  _task_doc standard implement pi
  _verdict reject
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi reject is not cleared by the lead's own review seam" {
  _task_doc standard implement pi
  _verdict reject
  _lead_seam
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi reject is cleared by a later accept" {
  _task_doc standard implement pi
  _verdict reject
  _verdict accept
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi reject then revise then the lead's own review seam is allowed" {
  _task_doc standard implement pi
  _verdict reject
  _verdict revise
  _lead_seam
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi revise then the lead's own review seam is allowed" {
  _task_doc standard implement pi
  _verdict revise
  _lead_seam
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi revise then a later accept is allowed" {
  _task_doc standard implement pi
  _verdict revise
  _verdict accept
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi accept followed by a later reject is refused" {
  _task_doc standard implement pi
  _verdict accept
  _verdict reject
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi verdict for an older head (review re-requested) is refused" {
  _task_doc standard implement pi
  _verdict accept
  _assign
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept after the review was re-requested is allowed" {
  _task_doc standard implement pi
  _verdict accept
  _assign
  _verdict accept
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a re-requested pi review is not satisfied by the lead's own review seam" {
  _task_doc standard implement pi
  _verdict accept
  _assign
  _lead_seam
  _gate
  _refused "no review seam"
}

@test "pr_open: a re-requested pi review answered by an elided or invalid verdict is refused" {
  _task_doc standard implement pi
  _verdict accept
  _assign
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"role":"reviewer","seam":"review"}'
  _verdict maybe
  _gate
  _refused "no review seam"
}

@test "pr_open: a resumed session inherits the latest pi verdict, accept or reject" {
  _task_doc deep implement pi
  _verdict accept worker:feat/x#s1-1
  _deslop_seam
  _gate s2-2
  _allowed
  rm -f "$(git rev-parse --git-common-dir)/crew/events.jsonl"
  _verdict accept worker:feat/x#s1-1
  _verdict reject worker:feat/x#s1-1
  _gate s2-2
  _refused "no review seam"
}

@test "pr_open: a torn trailing line does not change the pi verdict outcome" {
  _task_doc standard implement pi
  _verdict accept
  _deslop_seam
  printf 'torn{' >>"$(git rev-parse --git-common-dir)/crew/events.jsonl"
  _gate
  [ "$status" -eq 0 ]
  rm -f "$(git rev-parse --git-common-dir)/crew/events.jsonl"
  _verdict reject
  printf 'torn{' >>"$(git rev-parse --git-common-dir)/crew/events.jsonl"
  _gate
  _refused "no review seam"
}

_events() { printf '%s' "$(git rev-parse --git-common-dir)/crew/events.jsonl"; }

@test "pr_open: a pi accept then an oversized reviewer reply whose seam and verdict got elided is refused" {
  _task_doc standard implement pi
  _verdict accept
  local big
  big=$(jq -nc '{role:"reviewer",seam:"review",verdict:"accept",findings:[range(0;400)|"finding number \(.) with some text"]}')
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" "$big"
  [ "$(tail -1 "$(_events)" | jq -r '.body | fromjson | .verdict')" != accept ]
  [ "$(tail -1 "$(_events)" | jq -r '.body | fromjson | .seam')" != review ]
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept then a reject addressed to the dispatcher is refused" {
  _task_doc standard implement pi
  _verdict accept
  _verdict reject dispatcher:c1
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi reject then a revise addressed to the dispatcher stays refused" {
  _task_doc standard implement pi
  _verdict reject
  _verdict revise dispatcher:c1
  _lead_seam
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept then an unparseable lead assignment to the reviewer is refused" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" 'not json'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept then a markdown-fenced reviewer reject is refused" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '```json {"seam":"review","verdict":"reject"} ```'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept then a reviewer msg whose body is a JSON array is refused" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '[{"seam":"review","verdict":"reject"}]'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept survives role_exited and final objects on the bus" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"event":"role_exited"}'
  run_crew msg "worker:feat/x#s1-1" "dispatcher:c1" '{"final":true}'
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi accept then a reviewer object with neither seam nor verdict is refused" {
  # #392: a verdict-less reviewer reply on the pi path fails closed, so an
  # earlier accept must not survive it; the next exact accept clears it.
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"decision":"reject"}'
  _gate
  _refused "no review seam"
  _verdict accept
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi reviewer object with neither seam nor verdict alone is refused" {
  _task_doc standard implement pi
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"decision":"accept"}'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept survives a reviewer role_exited event with no seam or verdict" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"role":"reviewer","event":"role_exited","pane":"%3","detail":"engine exited before a verdict"}'
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi accept survives a reviewer tag-only note with no seam" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"tag":"other","detail":"x"}'
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi accept then a wrong-case Reject verdict is refused" {
  _task_doc standard implement pi
  _verdict accept
  _verdict Reject
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept then a reject carrying a tag key is refused" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"role":"reviewer","seam":"review","verdict":"reject","tag":"x"}'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept addressed to another worker as the only verdict is refused" {
  _task_doc standard implement pi
  _verdict accept dispatcher:c1
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept then a mis-addressed accept still stands" {
  _task_doc standard implement pi
  _verdict accept
  _verdict accept dispatcher:c1
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi accept survives a numeric-from object line from another crew" {
  _task_doc standard implement pi
  _verdict accept
  printf '%s\n' '{"ts":1,"crew_id":"other","from":7,"to":8,"kind":"msg","body":"{}"}' >>"$(_events)"
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi reject then a new assignment then the lead's own seam is refused" {
  _task_doc standard implement pi
  _verdict reject
  _assign
  _lead_seam
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi lead seam with review_mode false or null is refused" {
  _task_doc standard implement pi
  _lead_seam '{"seam":"review","review_mode":false}'
  _gate
  _refused "no review seam"
  _lead_seam '{"seam":"review","review_mode":null}'
  _gate
  _refused "no review seam"
}

@test "pr_open: off pi a re-assignment does not cancel the lead's own seam" {
  _task_doc standard implement claude
  _assign
  _lead_seam
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi assignment with an elided artifact still cancels the lead's seam" {
  _task_doc standard implement pi
  _lead_seam
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"artifact":"…[elided]"}'
  _gate
  _refused "no review seam"
}

@test "pr_open: a lead final release to the reviewer does not cancel an accept" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"final":true}'
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a scalar JSON line in the log is tolerated" {
  _task_doc standard implement pi
  _verdict accept
  printf '42\n"x"\nnull\n' >>"$(git rev-parse --git-common-dir)/crew/events.jsonl"
  _deslop_seam
  _gate
  _allowed
}

# A hard kill mid-append leaves a line with no newline; the next append then
# splices onto it, so one unparsable line carries a whole valid record. #391
# fixed the writer so it no longer splices, so this builds that crash-splice
# directly: the reader's fail-closed content heuristic must still catch a valid
# reject hidden inside an unparsable torn line.
@test "pr_open: a pi accept then a reject spliced onto a torn line is refused until a fresh verdict" {
  _task_doc standard implement pi
  _verdict accept
  spliced="$(jq -nc --argjson ts "$(jq -nc 'now*1000|floor')" \
    '{ts:$ts, crew_id:"c1", from:"role:feat/x:reviewer", to:"worker:feat/x#s1-1",
      kind:"msg",
      body:"{\"role\":\"reviewer\",\"seam\":\"review\",\"verdict\":\"reject\"}"}')"
  printf 'torn{%s\n' "$spliced" >>"$(_events)"
  [ "$(tail -1 "$(_events)" | jq -R 'fromjson? // "unparsable"')" = '"unparsable"' ]
  _gate
  _refused "no review seam"
  _verdict accept
  _deslop_seam
  _gate
  [ "$status" -eq 0 ]
}

@test "pr_open: a pi accept then a torn line cut inside a reviewer reply is refused" {
  _task_doc standard implement pi
  _verdict accept
  printf '{"ts":1,"crew_id":"c1","from":"role:feat/x:reviewer","to":"wor' >>"$(_events)"
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept then a lead assignment spliced onto a torn line is refused" {
  _task_doc standard implement pi
  _verdict accept
  # #391: build the crash-splice directly (the writer no longer glues) so the
  # reader's content heuristic still has to catch a re-request buried in a torn
  # line — a re-request cancels the accept.
  spliced="$(jq -nc --argjson ts "$(jq -nc 'now*1000|floor')" \
    '{ts:$ts, crew_id:"c1", from:"worker:feat/x#s1-1", to:"role:feat/x:reviewer",
      kind:"msg", body:"{\"seam\":\"review\",\"artifact\":\"/a/review.diff\",\"question\":\"Review this diff.\"}"}')"
  printf 'torn{%s\n' "$spliced" >>"$(_events)"
  [ "$(tail -1 "$(_events)" | jq -R 'fromjson? // "unparsable"')" = '"unparsable"' ]
  _gate
  _refused "no review seam"
}

@test "pr_open: a torn-line reject is detected for a branch whose reviewer id is JSON-escaped" {
  _task_doc standard implement pi
  run_crew msg 'role:feat/a"b:reviewer' 'worker:feat/a"b#s1-1' '{"seam":"review","verdict":"accept"}'
  # #391: build the crash-splice directly (the writer no longer glues) so the
  # reader's content heuristic still has to unescape the reviewer id.
  spliced="$(jq -nc --argjson ts "$(jq -nc 'now*1000|floor')" \
    '{ts:$ts, crew_id:"c1", from:"role:feat/a\"b:reviewer", to:"worker:feat/a\"b#s1-1",
      kind:"msg", body:"{\"seam\":\"review\",\"verdict\":\"reject\"}"}')"
  printf 'torn{%s\n' "$spliced" >>"$(_events)"
  run --separate-stderr run_crew status 'worker:feat/a"b#s1-1' pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a pi accept survives an unrelated record spliced onto a torn line" {
  _task_doc standard implement pi
  _verdict accept
  printf 'torn{' >>"$(_events)"
  run_crew msg "worker:feat/x#s1-1" "dispatcher:c1" '{"note":"x"}'
  printf 'torn{{"ts":1,"crew_id":"other","from":"role:feat/x:reviewer","to":"worker:feat/x","kind":"msg","body":"{}"}\n' >>"$(_events)"
  _deslop_seam
  _gate
  [ "$status" -eq 0 ]
}

# A re-request is any lead -> reviewer msg except the release: it does not
# have to carry seam or artifact.
@test "pr_open: a pi accept then a lead question to the reviewer with no seam or artifact is refused" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"question":"look again please"}'
  _gate
  _refused "no review seam"
  _verdict accept
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi lead question to the reviewer cancels the lead's own review seam" {
  _task_doc standard implement pi
  _lead_seam
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"question":"look again please"}'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept survives a lead release that carries only final" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"final":true}'
  run_crew msg "worker:feat/x#s1-1" "role:feat/other:reviewer" '{"question":"another branch"}'
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi accept then a final release that also carries a question is refused" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"final":true,"question":"re-review please"}'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi accept survives a reviewer retro-style note" {
  _task_doc standard implement pi
  _verdict accept
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"review","tag":"other","detail":"x"}'
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a reviewer retro-style note is not a review seam" {
  _task_doc standard implement pi
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"review","tag":"other","detail":"x"}'
  _gate
  _refused "no review seam"
}

@test "pr_open: off pi a pane reject or accept alone is refused" {
  _task_doc standard implement claude
  _verdict reject
  _gate
  _refused "no review seam"
  _verdict accept
  _gate
  _refused "no review seam"
}

@test "pr_open: off pi the lead's own review seam is allowed even after a pane reject" {
  _task_doc standard implement claude
  _verdict reject
  _lead_seam
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a review seam with review_mode false or null is refused" {
  _task_doc standard
  _lead_seam '{"seam":"review","review_mode":false}'
  _gate
  _refused "no review seam"
  _lead_seam '{"seam":"review","review_mode":null}'
  _gate
  _refused "no review seam"
}

@test "pr_open: a pi verdict addressed to another branch's worker is refused" {
  _task_doc standard implement pi
  _verdict accept worker:feat/other#s1-1
  _gate
  _refused "no review seam"
}

@test "done: a pi reviewer verdict and the lead's own review seam both count" {
  _task_doc standard implement pi
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"review","verdict":"accept"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ "$(_status_rows)" -eq 1 ]
  rm -f "$(git rev-parse --git-common-dir)/crew/events.jsonl"
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"review","review_mode":"full"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: a reviewer verdict from an earlier session still counts" {
  _task_doc deep implement pi
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"review","verdict":"accept"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
}

@test "pr_open: a reviewer verdict does not stand in for the native batch off pi" {
  _task_doc standard implement claude
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"review","verdict":"accept"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a reviewer msg without a verdict is not a review seam" {
  _task_doc standard implement pi
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"role":"reviewer","event":"role_exited","pane":"%3"}'
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"review","verdict":"maybe"}'
  run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"plan","verdict":"accept"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a verdict forged under the worker id or another branch's reviewer does not count" {
  _task_doc standard implement pi
  run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" '{"seam":"review","verdict":"accept"}'
  run_crew msg "role:feat/other:reviewer" "worker:feat/x#s1-1" '{"seam":"review","verdict":"accept"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a tagged retro note is not a review seam" {
  _task_doc standard
  run_crew msg "worker:feat/x#s1-1" "retro:c1" '{"seam":"review","tag":"other","detail":"x"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a review seam with review_mode none or unavailable does not count" {
  _task_doc standard
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"review","review_mode":"none"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"review","review_mode":"unavailable"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a review seam sent to metrics does not count" {
  _task_doc standard
  run_crew msg "worker:feat/x#s1-1" "metrics:c1" '{"seam":"review","review_mode":"full"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a review seam from another branch does not count" {
  _task_doc standard
  run_crew msg "worker:feat/other#s1-1" "review:c1" '{"seam":"review"}'
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open: a downgraded review seam counts like a full one" {
  _task_doc standard
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"review","review_mode":"downgraded"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats)" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(jq -r 'select(.kind=="status") | .body.state' "$(git rev-parse --git-common-dir)/crew/events.jsonl")" = pr_open ]
}

@test "pr_open: a reviewer verdict posted under a different crew does not count" {
  # Written before WORKER_TASK.md exists so CREW_ID=c2 wins; only the crew_id
  # filter excludes it.
  CREW_ID=c2 run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" '{"seam":"review","verdict":"accept"}'
  seam_crew=$(jq -r 'select(.kind=="msg" and .from=="role:feat/x:reviewer") | .crew_id' "$(git rev-parse --git-common-dir)/crew/events.jsonl")
  [ "$seam_crew" = c2 ]
  _task_doc standard implement pi
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "no review seam"
}

@test "pr_open/done: trivial with no review seam posts silently" {
  _task_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 2 ]
}

@test "done: a kind review session needs no review seam" {
  _task_doc standard review
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "reviewed PR 9 — https://x"
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 1 ]
}

@test "done/failed: a role caller and a failed worker are not gated" {
  _task_doc standard
  run --separate-stderr run_crew status "role:feat/x:reviewer" done ""
  [ "$status" -eq 0 ]
  run --separate-stderr run_crew status "worker:feat/x#s1-1" failed "tests red"
  [ "$status" -eq 0 ]
  [ "$(_status_rows)" -eq 2 ]
}

@test "pr_open: the acceptance ledger rides in the status detail" {
  _task_doc trivial
  run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats) AC2 waived(dispatcher)" https://example.com/pr/1
  [ "$(jq -r 'select(.kind=="status") | .body.detail' "$(git rev-parse --git-common-dir)/crew/events.jsonl")" = "AC1 pass(bats) AC2 waived(dispatcher)" ]
}

@test "pr_open: no WORKER_TASK.md applies no gate" {
  CREW_ID=c1 run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pending(sim)" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ "$(_status_rows)" -eq 1 ]
}

@test "pr_open: standard with a review seam but no deslop seam is refused" {
  _task_doc standard
  _lead_seam
  _gate
  _refused "no deslop seam"
}

@test "done: standard with a review seam but no deslop seam is refused" {
  _task_doc standard
  _lead_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "" https://example.com/pr/1
  _refused "no deslop seam"
}

@test "pr_open: deep with a review seam but no deslop seam is refused" {
  _task_doc deep
  _lead_seam
  _gate
  _refused "no deslop seam"
}

@test "pr_open: standard with review and deslop seams posts silently" {
  _task_doc standard
  _lead_seam
  _deslop_seam
  _gate
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(jq -r 'select(.kind=="status") | .body.state' "$(git rev-parse --git-common-dir)/crew/events.jsonl")" = pr_open ]
}

@test "pr_open/done: a resumed session inherits the branch's earlier review and deslop seams" {
  _task_doc deep
  _lead_seam
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  run --separate-stderr run_crew status "worker:feat/x#s2-2" done "" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: a deslop seam sent to metrics does not count" {
  _task_doc standard
  _lead_seam
  run_crew msg "worker:feat/x#s1-1" "metrics:c1" '{"seam":"deslop"}'
  _gate
  _refused "no deslop seam"
}

@test "pr_open: a deslop seam from another branch does not count" {
  _task_doc standard
  _lead_seam
  run_crew msg "worker:feat/other#s1-1" "review:c1" '{"seam":"deslop"}'
  _gate
  _refused "no deslop seam"
}

@test "pr_open: a tagged deslop seam does not count" {
  _task_doc standard
  _lead_seam
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"deslop","tag":"x"}'
  _gate
  _refused "no deslop seam"
}

@test "pr_open: a deslop seam alone without a review seam is refused for the review seam first" {
  _task_doc standard
  _deslop_seam
  _gate
  _refused "no review seam"
}

@test "pr_open: a kind review deep session needs no review or deslop seam" {
  _task_doc deep review
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
}

@test "pr_open: a pi reviewer accept with the lead's deslop seam passes" {
  _task_doc standard implement pi
  _verdict accept
  _deslop_seam
  _gate
  _allowed
}

@test "pr_open: a pi reviewer accept without a deslop seam is refused" {
  _task_doc standard implement pi
  _verdict accept
  _gate
  _refused "no deslop seam"
}

@test "pr_open: an unparsable log line does not break the deslop check" {
  _task_doc standard
  _lead_seam
  printf 'garbage\n' >>"$(git rev-parse --git-common-dir)/crew/events.jsonl"
  _deslop_seam
  _gate
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
}

# --- terminal status gate: acceptance ledger grammar (#332) ---

@test "pr_open: every truth-table ledger the grammar refuses is refused on every tier" {
  _task_doc trivial
  local -a details=(
    'AC1 pass(bats); AC2 skipped (not run: no simulator)'
    'AC2 (pending on device)'
    'AC3 deferred (not done yet)'
    '(AC2 pending)'
    'AC2 pending(sim)'
    'AC1 pass(bats); AC2 pending(sim)'
    'AC3 NOT DONE'
    'AC4 not run'
    'AC1 pending(sim); AC2 waived(dispatcher)'
    'AC1 pass(x) AC2 pending'
    'AC1 pass(x AC2 pending(sim)'
    'AC2 waived(dispatcher) not run on CI'
    'AC1 pass(x) AC2 pending AC3 pass(y)'
    'AC1 pass(x); AC2 pending; AC3 pass(y)'
    'AC2 pass()'
    'AC2 waived(self)'
    'AC2 waived(low risk)'
    'AC2 n/a(no ui)'
    'A7b partial(flag yes, viewer unit-only)'
    'acceptance ledger: all pass, see PR body/comment'
    'AC1 pass(x)) AC2 pending'
    'AC1 pass(x); AC2'
    'AC1 passed (no pending migrations)'
    'AC2 waived(dispatcher) (not run on CI)'
  )
  for d in "${details[@]}"; do
    run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "$d" https://example.com/pr/1
    _refused "every acceptance ledger item"
  done
}

@test "pr_open: every truth-table ledger the grammar accepts is written" {
  _task_doc trivial
  local -a details=(
    'AC1 pass(bats) AC2 waived(dispatcher)'
    'AC5 pass(bats: pending ledger refused)'
    'PASS(ci pending)'
    'AC1 pass(nix build (x86) not run twice)'
    'AC1 pass(bats: 3 pending sub-tests skipped (see log (details))); AC2 pass(nix build)'
    'AC2 waived(dispatcher: not run on CI)'
    ''
    '   '
    'AC1 pass(bats); AC2 pass(nix build); AC3 waived(dispatcher)'
    'AC1 pass(bats); AC2 pass(nix build); AC3 waived(dispatcher);'
    'AC1-3 pass(vitest), AC4 pass(test+emu log)'
    'AC1: pass(bats)'
    'AC2 WAIVED(Dispatcher)'
  )
  local n=0
  for d in "${details[@]}"; do
    n=$((n + 1))
    run --separate-stderr run_crew status "worker:feat/x#s1-$n" pr_open "$d" https://example.com/pr/1
    [ "$status" -eq 0 ]
    [ -z "$stderr" ]
  done
  [ "$(_status_rows)" -eq "$n" ]
}

@test "pr_open: a lone state word before a valid item reads as its id (known gap)" {
  # Known accepted gap: a single-token state word with no parens reads as the id
  # of the next item. A drifting agent is unlikely to write it.
  _task_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(x) pending pass(y)" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 1 ]
}

@test "pr_open: an empty ledger is refused when the task doc has an ## Acceptance list" {
  _acceptance_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "acceptance list"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "   " https://example.com/pr/1
  _refused "acceptance list"
}

@test "pr_open: the refusal hint points a no-acceptance-list worker at an empty detail" {
  # No acceptance list: a free-text detail is refused, but the hint must name
  # the empty-detail post, not steer the worker to invent `AC1 pass(n/a)`.
  _task_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "no acceptance list"
  [[ "$stderr" == *'pr_open ""'* ]]
  [[ "$stderr" == *'an empty detail is the correct pr_open'* ]]
}

@test "pr_open: the refusal hint keeps the ledger grammar when an acceptance list exists" {
  _acceptance_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "every acceptance ledger item"
  [[ "$stderr" != *'no acceptance list'* ]]
}

@test "pr_open: a ### Acceptance criteria list keeps the ledger hint (not the empty-detail steer)" {
  # Widened detection: a `###` heading is an acceptance list, so a free-text
  # detail keeps the ledger grammar and an empty detail is refused.
  _task_doc trivial
  printf '\n### Acceptance criteria\n- AC1\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "every acceptance ledger item"
  [[ "$stderr" != *'no acceptance list'* ]]
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
  _refused "acceptance list"
}

@test "pr_open: a bold **Acceptance:** list keeps the ledger hint (not the empty-detail steer)" {
  # #386: bold spellings are equivalent acceptance lists, so a free-text
  # detail keeps the ledger grammar and an empty detail is refused.
  _task_doc trivial
  printf '\n**Acceptance:**\n- AC1\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "every acceptance ledger item"
  [[ "$stderr" != *'no acceptance list'* ]]
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
  _refused "acceptance list"
}

@test "pr_open: a bold **Acceptance criteria** list keeps the ledger hint" {
  _task_doc trivial
  printf '\n**Acceptance criteria**\n- AC1\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "every acceptance ledger item"
  [[ "$stderr" != *'no acceptance list'* ]]
}

@test "pr_open: a line-start Acceptance: is detected case-insensitively" {
  _task_doc trivial
  printf '\nacceptance: AC1 pass(x)\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "every acceptance ledger item"
  [[ "$stderr" != *'no acceptance list'* ]]
}

@test "pr_open: an acceptance word mid-sentence is not read as a list" {
  # The match stays anchored to a heading/marker/colon, so prose that merely
  # mentions the word does not force a ledger onto a doc that has none.
  _task_doc trivial
  printf '\nSee the acceptance list in the issue for the details.\n' >>WORKER_TASK.md
  printf 'acceptance is mentioned here in passing.\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "no acceptance list"
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 1 ]
}

@test "pr_open: an inline-bold ### **Acceptance criteria** heading is detected" {
  # #395: a heading that wraps the word in bold on the same line is an
  # acceptance list, so a free-text detail keeps the ledger grammar and an
  # empty detail is refused.
  _task_doc trivial
  printf '\n### **Acceptance criteria**\n- AC1\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
  _refused "every acceptance ledger item"
  [[ "$stderr" != *'no acceptance list'* ]]
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
  _refused "acceptance list"
}

@test "pr_open: a heading whose bold word only resembles Acceptance is not a list" {
  # The widened heading match still requires the word itself directly after the
  # optional bold marker, so a heading that opens with a marker but a different
  # word is not read as an acceptance list.
  _task_doc trivial
  printf '\n### **Accepted**\n- one\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 1 ]
}

@test "pr_open: a jq failure on the ledger check refuses (fail closed)" {
  _task_doc trivial
  mkdir -p "$BATS_TEST_TMPDIR/jqstub"
  export REAL_JQ
  REAL_JQ=$(command -v jq)
  cat >"$BATS_TEST_TMPDIR/jqstub/jq" <<'EOF'
#!/usr/bin/env bash
case "$*" in *'?<b>'*) exit 3 ;; esac; exec "$REAL_JQ" "$@"
EOF
  chmod +x "$BATS_TEST_TMPDIR/jqstub/jq"
  PATH="$BATS_TEST_TMPDIR/jqstub:$PATH" run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats)" https://example.com/pr/1
  _refused "could not check the acceptance ledger (jq exit 3)"
}

@test "pr_open: a jq failure on the deslop seam check refuses (fail closed)" {
  _task_doc standard
  _lead_seam
  mkdir -p "$BATS_TEST_TMPDIR/jqstub"
  export REAL_JQ
  REAL_JQ=$(command -v jq)
  cat >"$BATS_TEST_TMPDIR/jqstub/jq" <<'EOF'
#!/usr/bin/env bash
case "$*" in *'"deslop" and (has("tag")'*) exit 3 ;; esac; exec "$REAL_JQ" "$@"
EOF
  chmod +x "$BATS_TEST_TMPDIR/jqstub/jq"
  PATH="$BATS_TEST_TMPDIR/jqstub:$PATH" run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
  _refused "could not read the crew log for the deslop seam (jq exit 3)"
}

@test "pr_open: the ledger is checked before the review seam" {
  _task_doc deep
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC2 pending(sim)" https://example.com/pr/1
  _refused "every acceptance ledger item"
  run_crew msg "worker:feat/x#s1-1" "review:c1" '{"seam":"review","review_mode":"full"}'
  _deslop_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats)" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 1 ]
}

@test "pr_open: the protocol's example ledgers pass the gate" {
  local -a examples=()
  while IFS= read -r line; do
    examples+=("$line")
  done < <(grep -oE 'pr_open "[^"<][^"]*"' "$BATS_TEST_DIRNAME/../adapters/core/protocols/WORKER_PROTOCOL.md")
  [ "${#examples[@]}" -ge 1 ]
  _acceptance_doc trivial
  local n=0
  for ex in "${examples[@]}"; do
    n=$((n + 1))
    d="${ex#pr_open \"}"
    d="${d%\"}"
    run --separate-stderr run_crew status "worker:feat/x#s1-$n" pr_open "$d" https://example.com/pr/1
    [ "$status" -eq 0 ]
    [ -z "$stderr" ]
  done
  run ! grep -F 'pr_open "" ' "$BATS_TEST_DIRNAME/../adapters/core/protocols/WORKER_PROTOCOL.md"
}

@test "done/pr_open: done, kind review and role callers are not ledger-checked" {
  _task_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "AC2 pending(sim)" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  run --separate-stderr run_crew status "worker:feat/x#s2-2" done "follow-ups: #312 (upstream fix pending)"
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  run --separate-stderr run_crew status "role:feat/x:reviewer" pr_open "AC2 pending" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  _task_doc trivial review
  run --separate-stderr run_crew status "worker:feat/x#s3-3" pr_open "2 findings pending" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  run --separate-stderr run_crew status "worker:feat/x#s3-3" done "2 findings pending author reply" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 5 ]
}

@test "pr_open: no detail argument at all" {
  # _acceptance_doc first: _refused asserts zero total rows, so the refusal
  # case must run before any accepted write in this test.
  _acceptance_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open
  _refused "acceptance list"
  _task_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(_status_rows)" -eq 1 ]
}

# --- reap: stacked-base compatibility (#274) ---

@test "reap: keeps an open-PR parent worktree with a stacked child in place" {
  git commit -q --allow-empty -m init
  git branch feat/10-parent
  parent_wt="$BATS_TEST_TMPDIR/parent-wt"
  git worktree add -q "$parent_wt" feat/10-parent

  git branch feat/11-child
  child_wt="$BATS_TEST_TMPDIR/child-wt"
  git worktree add -q "$child_wt" feat/11-child
  printf 'base: feat/10-parent\n' >"$child_wt/WORKER_TASK.md"

  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'OPEN' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/10-parent" done "" "https://example.com/pr/30"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/10-parent — PR OPEN"* ]]
  [ -d "$parent_wt" ]
  [ -d "$child_wt" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: reaps a merged parent without touching its stacked child" {
  git commit -q --allow-empty -m init
  git branch feat/10-parent
  parent_wt="$BATS_TEST_TMPDIR/parent-wt2"
  git worktree add -q "$parent_wt" feat/10-parent
  parent_wt=$(cd "$parent_wt" && pwd -P)
  echo unique >"$parent_wt/work.txt"
  git -C "$parent_wt" add work.txt
  git -C "$parent_wt" commit -q -m "parent work"

  git branch feat/11-child
  child_wt="$BATS_TEST_TMPDIR/child-wt2"
  git worktree add -q "$child_wt" feat/11-child
  printf 'base: feat/10-parent\n' >"$child_wt/WORKER_TASK.md"
  child_task_before="$(cat "$child_wt/WORKER_TASK.md")"

  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/10-parent)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_wt_removes
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/10-parent" done "" "https://example.com/pr/31"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/10-parent (MERGED)"* ]]
  [ ! -d "$parent_wt" ]
  run ! git show-ref --verify --quiet refs/heads/feat/10-parent

  [ -d "$child_wt" ]
  git show-ref --verify --quiet refs/heads/feat/11-child
  [ "$(cat "$child_wt/WORKER_TASK.md")" = "$child_task_before" ]
  jq -e 'select(.kind=="reap" and .branch=="feat/10-parent")' "$log" >/dev/null
}
