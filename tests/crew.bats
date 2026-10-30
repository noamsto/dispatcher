bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  run_crew() { "$CREW" "$@"; }
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
  # await and the hold paths run on a virtual clock: their waits
  # advance this file instead of sleeping.
  export CREW_CLOCK="$BATS_TEST_TMPDIR/clock"
  # The real refresh-budget probes live accounts, which no test may do.
  export CREW_BUDGET_REFRESH_CMD=:
}

teardown() {
  # Before teardown_repo: a leaked fleet lane would race its rm -rf.
  stop_fleet_stream
  if [ -n "${HOLDER_PID:-}" ]; then
    kill -KILL "$HOLDER_PID" 2>/dev/null || true
  fi
  # A test that leaves a read-only dir names it here, so bats can delete it.
  if [ -n "${RESTORE_WRITE:-}" ]; then
    chmod -R u+w "$RESTORE_WRITE" 2>/dev/null || true
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

# #908: the sender check. A tmux stub lists the bats process as a pane, so the
# crew process (a descendant of this test) is anchored to that pane's stamps.
# _stamp_pane <branch> <role> <crew> — every field may be empty (unstamped).
# FAKE_EXTRA holds extra pane rows (target panes) in the same US-separated shape.
_stamp_pane() {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
case "$1" in
list-panes)
  printf '%s\037%s\037%s\037%s\037@1\037%%1\n' "$FAKE_PANE_PID" "$FAKE_BRANCH" "$FAKE_ROLE" "$FAKE_CREW"
  printf '%s' "${FAKE_EXTRA:-}"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  export FAKE_PANE_PID=$$ FAKE_BRANCH="$1" FAKE_ROLE="$2" FAKE_CREW="$3"
}

_msg_rows() { bus | jq -s '[.[] | select(.kind=="msg")] | length'; }

@test "reply: a worker or role session cannot reply as the dispatcher (#908)" {
  CREW_WORKER_ID='worker:feat/x#s1-1' run run_crew reply 'worker:feat/x#s1-1' go --crew c1
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing sender"* ]]
  CREW_ROLE_ID='role:feat/x:reviewer' run run_crew reply 'worker:feat/x#s1-1' go --crew c1
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing sender"* ]]
  [ "$(_msg_rows)" = 0 ]
}

@test "msg/reply: the dispatcher still posts (#908)" {
  CREW_ID=c1 run run_crew msg dispatcher:c1 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  run bash -c "bus | jq -r 'select(.kind==\"msg\") | .from'"
  [ "$output" = dispatcher:c1 ]
  CREW_ID=c1 run run_crew reply 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  CREW_ID= run run_crew reply 'worker:feat/x#s1-1' go --crew c1
  [ "$status" -eq 0 ]
  [ "$(_msg_rows)" = 3 ]

  # A dispatcher pane is anchored but unstamped.
  _stamp_pane '' '' ''
  CREW_ID=c1 run run_crew msg dispatcher:c1 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  CREW_ID=c1 run run_crew reply 'worker:feat/x#s1-1' go
  [ "$status" -eq 0 ]
  [ "$(_msg_rows)" = 5 ]
}

# ---------------------------------------------------------------------------
# bus helpers
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

# ---------------------------------------------------------------------------
# watch --crew harness
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

# Predicates for poll_for.
proc_gone() { ! kill -0 "$1" 2>/dev/null; }

# The lock belongs to the park, so a park killed mid-sleep has to drop it: bash
# ran its EXIT trap for TERM, INT and HUP, and `crew stream` stops and retries
# its inner watch that way. A leaked watch.lock.d would refuse every later watch
# of this crew until the dead pid was reclaimed.
@test "watch: a TERM mid-park releases the lock and the next watch starts at once" {
  cdir="$(crew_dir c1)"

  # Not `run_crew … &`: backgrounding the helper function backgrounds a subshell
  # of its own, and its pid is not the one the park writes in the lock.
  "$CREW" watch --crew c1 --timeout 30 --interval 1 >"$BATS_TEST_TMPDIR/park.out" 2>&1 &
  local pid=$!
  poll_for 100 test -f "$cdir/watch.lock.d/pid"
  # The pid the arm wrote is `$$`, and the exec'd Go binary keeps it.
  [ "$(cat "$cdir/watch.lock.d/pid")" = "$pid" ]

  kill -TERM "$pid"
  local rc=0
  wait "$pid" || rc=$?
  [ "$rc" -eq 143 ]

  [ ! -d "$cdir/watch.lock.d" ]
  [ ! -e "$cdir/cursor" ]

  # The group-shaped signal: a terminal's Ctrl-C reaches the park and its
  # in-flight `sleep` child, and `set -e` exited on the killed child, so the
  # status is the signal's. Killing the child alone is the same shape.
  "$CREW" watch --crew c1 --timeout 60 --interval 5 >/dev/null 2>&1 &
  local pid2=$!
  poll_for 100 test -f "$cdir/watch.lock.d/pid"
  local wpid
  wpid="$(cat "$cdir/watch.lock.d/pid")"
  poll_for 50 pkill -TERM -P "$wpid" sleep
  local rc2=0
  wait "$pid2" || rc2=$?
  [ "$rc2" -eq 143 ]
  [ ! -d "$cdir/watch.lock.d" ]

  # The next watch starts rather than refusing.
  CREW_ID=c1 run_crew status worker:feat/x done
  run --separate-stderr run_crew watch --crew c1 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  run jq -e '(.events | length) == 1 and .events[0].body.state == "done"' <<<"$output"
  [ "$status" -eq 0 ]
}


# start_fleet_stream <args…> — launch `crew fleet stream` as a REAL process.
# Never through run_crew: backgrounding a shell function makes $! the subshell's
# pid, so the lock's pid comparison and every kill below would name the wrong
# process, and </dev/null keeps the lane off bats' own pipes. It adds no
# --reap-every, which the fleet lane refuses, and its lock lives under
# $XDG_STATE_HOME, not in a repo. The state and sweep-root defaults keep a test
# that forgets them off the real home.
start_fleet_stream() {
  export XDG_STATE_HOME="${XDG_STATE_HOME:-$BATS_TEST_TMPDIR/state}"
  export CREW_SWEEP_ROOTS="${CREW_SWEEP_ROOTS:-$BATS_TEST_TMPDIR/fleetroot}"
  FLEET_N=$((${FLEET_N:-0} + 1))
  FLEET_OUT="$BATS_TEST_TMPDIR/fleet-stream.$FLEET_N.out"
  FLEET_ERR="$BATS_TEST_TMPDIR/fleet-stream.$FLEET_N.err"
  local args=("$@") i crew_id=""
  for ((i = 0; i < ${#args[@]}; i++)); do
    [ "${args[i]}" = --crew ] && crew_id="${args[i + 1]}"
  done
  FLEET_LOCK="$XDG_STATE_HOME/crew/fleet-stream/$crew_id/stream.lock.d"
  "$CREW" fleet stream "${args[@]}" >"$FLEET_OUT" 2>"$FLEET_ERR" </dev/null &
  FLEET_PID=$!
  FLEET_PIDS="${FLEET_PIDS:-} $FLEET_PID"
}

# stop_fleet_stream — TERM every lane this test started and wait for the lock
# dir, the last thing a shutdown removes, then reap them.
stop_fleet_stream() {
  local fpid
  [ -n "${FLEET_PIDS:-}" ] || return 0
  for fpid in $FLEET_PIDS; do
    kill -TERM "$fpid" 2>/dev/null || true
  done
  poll_for 60 test ! -d "$FLEET_LOCK" || {
    for fpid in $FLEET_PIDS; do
      kill -KILL "$fpid" 2>/dev/null || true
    done
  }
  for fpid in $FLEET_PIDS; do
    wait "$fpid" 2>/dev/null || true
  done
  FLEET_PIDS=""
  FLEET_PID=""
}

fleet_locked() { [ -d "$FLEET_LOCK" ]; }

# seed_fleet_repo <dir> — a git repo with a c1 dispatch and a working worker.
seed_fleet_repo() {
  mkdir -p "$1/.git/crew"
  git init -q -b main "$1"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", engine:"claude", model:"sonnet", tier:"standard", effort:"medium", title:"t"}' \
    >>"$1/.git/crew/events.jsonl"
  jq -nc '{ts:1500, crew_id:"c1", from:"worker:feat/x", to:"dispatcher:c1", kind:"status", body:{state:"working"}}' \
    >>"$1/.git/crew/events.jsonl"
}

# post_status <dir> <state> — a status row for the worker seed_fleet_repo made.
post_status() {
  jq -nc --argjson ts "$(($(date +%s) * 1000))" --arg st "$2" \
    '{ts:$ts, crew_id:"c1", from:"worker:feat/x", to:"dispatcher:c1", kind:"status", body:{state:$st, detail:"PR"}}' \
    >>"$1/.git/crew/events.jsonl"
}

@test "fleet stream: a hard-killed lane's orphaned watch does not swallow the next status" {
  root="$BATS_TEST_TMPDIR/fleetroot"
  seed_fleet_repo "$root/alpha"
  seed_fleet_repo "$root/beta"
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  export XDG_STATE_HOME="$BATS_TEST_TMPDIR/state"
  export CREW_SWEEP_ROOTS="$root"
  outside="$BATS_TEST_TMPDIR/outside"
  mkdir -p "$outside"
  cd "$outside"

  start_fleet_stream --crew c1 --park 30 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1 --rediscover 1
  poll_for 100 fleet_locked
  post_status "$root/beta" pr_open
  poll_for 200 grep -q '"state":"pr_open"' "$FLEET_OUT"
  lane1_out="$FLEET_OUT"
  # The batch is printed during --coalesce while no watch holds the lock, so wait
  # for the loop to re-park its next watch: killing then orphans a live watch.
  poll_for 200 test -d "$root/beta/.git/crew/crews/c1/watch.lock.d"
  [ -d "$root/beta/.git/crew/crews/c1/watch.lock.d" ]

  # Hard-kill the lane only. The parked inner watch orphans; before the fix it
  # kept beta's watch lock and swallowed the next status.
  kill -KILL "$FLEET_PID" 2>/dev/null || true
  wait "$FLEET_PID" 2>/dev/null || true

  start_fleet_stream --crew c1 --park 30 --interval 1 --coalesce 1 --heartbeat 3600 --retry 1 --rediscover 1
  poll_for 100 fleet_locked
  post_status "$root/beta" done
  poll_for 400 grep -q '"state":"done"' "$FLEET_OUT"

  # The restarted lane delivered beta's next status, exactly once; the killed
  # lane replayed the backlog and stopped at pr_open.
  run jq -s -e '[.[] | select(.repo == "beta" and .events != null) | .events[] | select(.body.state == "done")] | length == 1' "$FLEET_OUT"
  [ "$status" -eq 0 ]
  run jq -s -e '[.[] | select(.repo == "beta" and .events != null) | .events[] | select(.body.state == "done")] | length == 0' "$lane1_out"
  [ "$status" -eq 0 ]
  stop_fleet_stream
}
