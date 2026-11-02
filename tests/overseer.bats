bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# `crew overseer msg`: the overseer session's one verb, posted from outside the
# repo onto a crew's bus (#1419), and `crew fleet stream --overseer`, the lane
# that wakes the overseer. The built binary and a throwaway repo, with tmux
# stubbed to an empty, readable pane listing — the overseer sender check
# refuses without a readable probe.

# shellcheck source=/dev/null
source "$BATS_TEST_DIRNAME/coverage.bash"

setup() {
  load helpers
  setup_repo
  stub_bin tmux
  COMMON="$(git rev-parse --path-format=absolute --git-common-dir)"
  LOG="$COMMON/crew/events.jsonl"
  # crew's repo discovery reads the registry and these roots only, never $HOME.
  mkdir -p "$XDG_DATA_HOME/crew" "$BATS_TEST_TMPDIR/roots"
  printf '%s\n' "$COMMON" >"$XDG_DATA_HOME/crew/repos"
  export CREW_SWEEP_ROOTS="$BATS_TEST_TMPDIR/roots"
  export XDG_STATE_HOME="$BATS_TEST_TMPDIR/state"
  HOST="$(uname -n)"
  HOST="${HOST%%.*}"
  WATCH_PID="" LANE_PID="" DISPATCHER_PID=""
  CREW_ID=c1 "$CREW" status 'worker:feat/x#s1-1' working
  SEED_TS="$(newest_ts)"
}

teardown() {
  local pid
  for pid in "$WATCH_PID" "$LANE_PID" "$DISPATCHER_PID"; do
    [ -n "$pid" ] || continue
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  teardown_repo
}

newest_ts() { jq -s 'map(.ts) | max' "$LOG"; }

# poll_for <tries> <cmd...> — retry every 0.1s
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

# overseer_msg <json> — the overseer's post to crew c1, from a directory that
# is no repo.
overseer_msg() {
  (cd "$BATS_TEST_TMPDIR" && CREW_OVERSEER="$HOST" "$CREW" overseer msg "$COMMON" c1 "$1")
}

@test "overseer msg: a parked crew watch wakes on it" {
  CREW_ID=c1 "$CREW" watch --crew c1 --since "$SEED_TS" --timeout 30 --interval 1 \
    >"$BATS_TEST_TMPDIR/watch.out" 2>"$BATS_TEST_TMPDIR/watch.err" 3>&- &
  WATCH_PID=$!
  # No marker says the watch is parked; --since makes the order irrelevant, the
  # settle only proves it did not exit on its own first.
  sleep 1
  kill -0 "$WATCH_PID"

  overseer_msg '{"kind":"notice","detail":"reaped your windows"}'

  wait "$WATCH_PID"
  WATCH_PID=""
  jq -e --arg from "overseer:$HOST" '
    (.events | length) == 1
    and (.events[0] | .from == $from and .to == "dispatcher:c1" and .kind == "msg"
         and .crew_id == "c1" and (.body | fromjson | .kind == "notice"))' \
    "$BATS_TEST_TMPDIR/watch.out"
}

@test "overseer msg: a dispatcher's reply to the overseer is accepted" {
  overseer_msg '{"kind":"ask","detail":"worker blocked 45 min — handling it?"}'
  ts="$(jq -r 'select(.from | startswith("overseer:")) | .ts' "$LOG")"

  CREW_ID=c1 "$CREW" msg dispatcher:c1 "overseer:$HOST" \
    "$(jq -nc --argjson re "$ts" '{re: $re, kind: "ack", detail: "ok"}')"

  tail -n 1 "$LOG" | jq -e --arg host "overseer:$HOST" \
    '.from == "dispatcher:c1" and .to == $host'
}

@test "overseer msg: a session without CREW_OVERSEER cannot post as the overseer" {
  before="$(cksum <"$LOG")"
  cd "$BATS_TEST_TMPDIR"
  run --separate-stderr "$CREW" overseer msg "$COMMON" c1 '{"kind":"notice"}'
  [ "$status" -eq 1 ]
  [[ "$stderr" == "crew: overseer msg: refusing sender 'overseer:$HOST'"*'only the overseer session (CREW_OVERSEER set to'* ]]
  [ "$(cksum <"$LOG")" = "$before" ]
}

# tick_ts — the lane's last tick stamp, "" while the file is missing or mid-write.
tick_ts() { jq -r '.ts // empty' "$XDG_STATE_HOME/crew/fleet-stream/overseer:$HOST/stream.tick" 2>/dev/null || true; }

# ticked_past <ts> — the lane stamped a tick other than <ts> ("" for any). It
# stamps at the top of each iteration, so a second stamp means the first
# iteration finished, its status baseline included.
ticked_past() {
  local ts
  ts="$(tick_ts)"
  [ -n "$ts" ] && [ "$ts" != "$1" ]
}

lane_said_down() { jq -e 'select(.event == "dispatcher_down")' "$BATS_TEST_TMPDIR/lane.out" >/dev/null 2>&1; }

@test "fleet stream --overseer: a dispatcher dying under a live worker prints one dispatcher_down" {
  # The stubbed tmux lists no window to vouch for the worker, so a worktree for
  # its branch keeps the row live and the crew unattended once its dispatcher is gone.
  git commit --allow-empty -qm init
  git worktree add -q -b feat/x "$BATS_TEST_TMPDIR/wt-x"
  # The pid file is written after the process starts: a process that started
  # after the file's mtime reads as a recycled pid.
  sleep 300 3>&- &
  DISPATCHER_PID=$!
  mkdir -p "$COMMON/crew/crews/c1"
  printf '%s\n' "$DISPATCHER_PID" >"$COMMON/crew/crews/c1/pid"

  "$CREW" fleet stream --overseer --interval 1 \
    >"$BATS_TEST_TMPDIR/lane.out" 2>"$BATS_TEST_TMPDIR/lane.err" 3>&- &
  LANE_PID=$!
  poll_for 100 ticked_past ""
  first="$(tick_ts)"
  poll_for 100 ticked_past "$first"
  kill -0 "$LANE_PID"
  run ! lane_said_down

  # Reaped as well as killed: a zombie still answers kill -0 as alive.
  kill "$DISPATCHER_PID"
  wait "$DISPATCHER_PID" || true
  DISPATCHER_PID=""
  poll_for 150 lane_said_down || {
    cat "$BATS_TEST_TMPDIR/lane.out" "$BATS_TEST_TMPDIR/lane.err" >&2
    false
  }
  # Another iteration passes before the count, so a repeat would be seen.
  seen="$(tick_ts)"
  poll_for 100 ticked_past "$seen"

  repo="$(basename "${COMMON%/.git}")"
  [ "$(jq -c 'select(.event == "dispatcher_down")' "$BATS_TEST_TMPDIR/lane.out" | wc -l)" -eq 1 ]
  jq -e --arg repo "$repo" 'select(.event == "dispatcher_down")
    | .stream == "overseer" and .repo == $repo and .crew == "c1" and .state == "dead"' \
    "$BATS_TEST_TMPDIR/lane.out"
}
