bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# `crew overseer msg`: the overseer session's one verb, posted from outside the
# repo onto a crew's bus (#1419). The built binary and a throwaway repo, with
# tmux stubbed to an empty, readable pane listing — the overseer sender check
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
  HOST="$(uname -n)"
  HOST="${HOST%%.*}"
  WATCH_PID=""
  CREW_ID=c1 "$CREW" status 'worker:feat/x#s1-1' working
  SEED_TS="$(newest_ts)"
}

teardown() {
  if [ -n "$WATCH_PID" ]; then
    kill "$WATCH_PID" 2>/dev/null || true
    wait "$WATCH_PID" 2>/dev/null || true
  fi
  teardown_repo
}

newest_ts() { jq -s 'map(.ts) | max' "$LOG"; }

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
