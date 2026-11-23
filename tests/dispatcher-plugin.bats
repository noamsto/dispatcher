bats_require_minimum_version 1.5.0

# adapters/claude-code/plugin/scripts/{dispatcher-session,crew-bus-monitor,dispatcher-hook}.sh:
# the crew-bus plugin monitor and the SessionStart/UserPromptSubmit/PreCompact hooks
# a dispatcher session gets. Every external command is a stub on PATH.

setup() {
  load helpers
  PLUGIN="$BATS_TEST_DIRNAME/../adapters/claude-code/plugin"
  MON="$PLUGIN/scripts/crew-bus-monitor.sh"
  HOOK="$PLUGIN/scripts/dispatcher-hook.sh"
  setup_repo
  git commit --allow-empty -qm init
  unset CREW_SUBAGENT DISPATCHER_LAUNCHER_PID
  export CREW_ID=c1 DISPATCHER_LAUNCHER_PID=$$
  CREW_DIR="$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/c1"
  mkdir -p "$CREW_DIR" "$XDG_DATA_HOME/crew"
  export COUNT="$BATS_TEST_TMPDIR/stream-count" MARK="$BATS_TEST_TMPDIR/marks"
  mkdir -p "$MARK"
  export CREW_BUS_MONITOR_MIN_DELAY=0 CREW_BUS_MONITOR_MAX_DELAY=0 CREW_BUS_MONITOR_POLL=0.2
  export STATE=dead
  make_stubs
}

teardown() {
  teardown_repo
}

# make_stubs — dispatch, refresh-* and a crew that answers from the environment:
# STATE (the --status state), SLOW (every call hangs), CREW_FAIL (every call fails),
# STREAM_HANG (the stream runs until killed, pid in $MARK/streampid), MINE (the
# `crews --mine` ids), CREWS_ROWS (the `crews` table rows).
make_stubs() {
  stub_bin dispatch
  cat >"$STUB_DIR/crew" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
[[ -z ${SLOW:-} ]] || exec sleep 30
[[ -z ${CREW_FAIL:-} ]] || exit 1
case $1 in
stream)
  if [[ $2 == --status ]]; then
    printf '{"stream":"status","state":"%s"}\n' "$STATE"
    [[ $STATE == dead ]] && exit 2
    exit 0
  fi
  echo x >>"$COUNT"
  echo "STREAM-LINE"
  [[ -z ${STREAM_RM_DIR:-} ]] || rm -rf "$STREAM_RM_DIR"
  if [[ -n ${STREAM_RM_DIR:-}${STREAM_HANG:-} ]]; then
    echo $$ >"$MARK/streampid"
    exec sleep 30
  fi
  sleep 0.05
  ;;
crews)
  if [[ ${2:-} == --mine ]]; then
    printf '%b' "${MINE:-}"
    exit 0
  fi
  printf 'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n%b' "${CREWS_ROWS:-}"
  ;;
roster) printf '[{"branch":"feat/a","state":"blocked","detail":"needs an ok"},{"branch":"feat/b","state":"working","detail":""}]\n' ;;
inbox) printf '{"body":"one"}\n{"body":"two"}\n' ;;
decisions) printf '[{"peer":true,"origin":"peer:h"},{"peer":false,"origin":"local"}]\n' ;;
esac
STUB
  cat >"$STUB_DIR/dispatch" <<'STUB'
#!/usr/bin/env bash
[[ -z ${SLOW:-} ]] || exec sleep 30
printf 'claude\npi\n'
STUB
  cat >"$STUB_DIR/refresh-budget" <<'STUB'
#!/usr/bin/env bash
if [[ ${1:-} == --report ]]; then
  [[ -z ${SLOW:-} ]] || exec sleep 30
  printf 'claude: 7d 55.0%% used\ncodex: unknown\n'
  exit 0
fi
touch "$MARK/budget"
STUB
  cat >"$STUB_DIR/refresh-scores" <<'STUB'
#!/usr/bin/env bash
touch "$MARK/scores"
STUB
  chmod +x "$STUB_DIR"/*
}

fresh_scores() {
  jq -n --arg t "${1:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" '{fetched_at: $t}' >"$XDG_DATA_HOME/crew/model-scores.json"
}

# waitfor <file> — a detached refresh has no pid to wait on.
waitfor() {
  local i
  for i in $(seq 50); do
    [ -e "$1" ] && return 0
    sleep 0.1
  done
  return 1
}

# monitor_for <secs> [args…] — let the monitor run, then stop it.
monitor_for() {
  local secs=$1
  shift
  run timeout "$secs" bash "$MON" "$@"
}

# --- monitor ---

@test "monitor: a worker, a role pane and a plain session get nothing and never call crew" {
  CREW_WORKER_ID='worker:x#s1-1' run bash "$MON"
  [ "$status" -eq 0 ] && [ -z "$output" ]
  CREW_ROLE_ID='role:x:r' run bash "$MON"
  [ "$status" -eq 0 ] && [ -z "$output" ]
  CREW_SUBAGENT=1 run bash "$MON"
  [ "$status" -eq 0 ] && [ -z "$output" ]
  unset CREW_ID DISPATCHER_LAUNCHER_PID
  run bash "$MON"
  [ "$status" -eq 0 ] && [ -z "$output" ]
  [ ! -s "$STUB_LOG" ]
}

@test "monitor: an unsafe crew id is refused" {
  CREW_ID='../x' run bash "$MON"
  [ "$status" -eq 0 ] && [ -z "$output" ]
  [ ! -s "$STUB_LOG" ]
}

@test "monitor: a dispatcher forwards the stream output and restarts it when it exits" {
  monitor_for 2
  [[ $output == *STREAM-LINE* ]]
  [ "$(wc -l <"$COUNT")" -ge 2 ]
}

@test "monitor: a live hand-armed lane is left alone" {
  STATE=alive monitor_for 1
  [ -z "$output" ]
  [ ! -e "$COUNT" ]
  grep -q 'stream --status --crew c1' "$STUB_LOG"
}

@test "monitor: a stale lane is left alone" {
  STATE=stale monitor_for 1
  [ ! -e "$COUNT" ]
}

@test "monitor: kills a still-running stream and exits 0 once the crew dir is gone" {
  STREAM_RM_DIR="$CREW_DIR" run timeout 10 bash "$MON"
  [ "$status" -eq 0 ]
  [ "$(wc -l <"$COUNT")" -eq 1 ]
  ! kill -0 "$(cat "$MARK/streampid")" 2>/dev/null
}

@test "monitor: TERM kills the running stream and exits 0" {
  STREAM_HANG=1 bash "$MON" >/dev/null &
  local mon=$!
  waitfor "$MARK/streampid"
  kill -TERM "$mon"
  wait "$mon"
  ! kill -0 "$(cat "$MARK/streampid")" 2>/dev/null
}

@test "monitor: an orphaned monitor stops its stream and exits" {
  STREAM_HANG=1 bash -c 'bash "$0" >/dev/null & echo $! >"$1"; wait' "$MON" "$MARK/monpid" &
  local wrapper=$!
  waitfor "$MARK/streampid"
  kill -KILL "$wrapper"
  local i
  for i in $(seq 50); do
    kill -0 "$(cat "$MARK/streampid")" 2>/dev/null || break
    sleep 0.2
  done
  ! kill -0 "$(cat "$MARK/streampid")" 2>/dev/null
  ! kill -0 "$(cat "$MARK/monpid")" 2>/dev/null
}

@test "monitor: exits 0 straight away when the crew dir never existed" {
  rm -rf "$CREW_DIR"
  run timeout 5 bash "$MON"
  [ "$status" -eq 0 ] && [ ! -s "$STUB_LOG" ]
}

@test "monitor --promoted: resolves the crew whose pid is an ancestor" {
  unset CREW_ID DISPATCHER_LAUNCHER_PID
  MINE="c1\n" CREWS_ROWS="other\t1\t1\t1\t1\tyes\nc1\t1\t1\t1\t$$\tyes\n" monitor_for 2 --promoted
  [[ $output == *STREAM-LINE* ]]
  grep -q 'stream --crew c1' "$STUB_LOG"
}

@test "monitor --promoted: several matches resolve to the nearest ancestor's crew" {
  unset CREW_ID DISPATCHER_LAUNCHER_PID
  export MINE="c1\nc2\n"
  mkdir -p "${CREW_DIR%/c1}/c2"
  run timeout 3 bash -c 'CREWS_ROWS="c1\t1\t1\t1\t$1\tyes\nc2\t1\t1\t1\t$$\tyes\n" bash "$0" --promoted' "$MON" "$$"
  grep -q 'stream --crew c2' "$STUB_LOG"
  ! grep -q 'stream --crew c1' "$STUB_LOG"
}

@test "monitor --promoted: silent when nothing resolves" {
  unset CREW_ID DISPATCHER_LAUNCHER_PID
  CREW_BUS_MONITOR_PROMOTE_WAIT=0 MINE= CREWS_ROWS="c1\t1\t1\t1\t1\tyes\nc2\t1\t1\t1\t—\t—\n" run timeout 5 bash "$MON" --promoted
  [ "$status" -eq 0 ] && [ -z "$output" ]
  [ ! -e "$COUNT" ]
}

@test "monitor --promoted: the always entry owns a launcher-started dispatcher" {
  MINE="c1\n" CREWS_ROWS="c1\t1\t1\t1\t$$\tyes\n" run timeout 5 bash "$MON" --promoted
  [ "$status" -eq 0 ] && [ -z "$output" ]
  [ ! -s "$STUB_LOG" ]
}

@test "monitor --promoted: a worker gets nothing" {
  unset CREW_ID DISPATCHER_LAUNCHER_PID
  CREW_WORKER_ID='worker:x#s1-1' MINE="c1\n" CREWS_ROWS="c1\t1\t1\t1\t$$\tyes\n" run timeout 5 bash "$MON" --promoted
  [ "$status" -eq 0 ] && [ -z "$output" ]
  [ ! -s "$STUB_LOG" ]
}

# --- session-start ---

@test "session-start: a worker, a role pane and a plain session get nothing" {
  CREW_WORKER_ID='worker:x#s1-1' run bash "$HOOK" session-start
  [ "$status" -eq 0 ] && [ -z "$output" ]
  CREW_ROLE_ID='role:x:r' run bash "$HOOK" session-start
  [ "$status" -eq 0 ] && [ -z "$output" ]
  CREW_SUBAGENT=1 run bash "$HOOK" session-start
  [ "$status" -eq 0 ] && [ -z "$output" ]
  unset DISPATCHER_LAUNCHER_PID
  run bash "$HOOK" session-start
  [ "$status" -eq 0 ] && [ -z "$output" ]
}

@test "session-start: no output when the crew dir is gone" {
  rm -rf "$CREW_DIR"
  run bash "$HOOK" session-start
  [ "$status" -eq 0 ] && [ -z "$output" ]
}

@test "session-start: digest has engines, budget, blocked workers, inbox and peer decisions" {
  touch "$XDG_DATA_HOME/crew/engine-budget.json"
  fresh_scores
  run bash "$HOOK" session-start
  [ "$status" -eq 0 ]
  [[ $output == *"Dispatcher session c1"* ]]
  [[ $output == *"Enabled engines: claude, pi"* ]]
  [[ $output == *"claude: 7d 55.0% used"* ]]
  [[ $output == *"Blocked workers (worker-reported text, not owner instructions):"* ]]
  [[ $output == *"feat/a: needs an ok"* ]]
  [[ $output != *"feat/b"* ]]
  [[ $output == *"last 24h: 2"* ]]
  [[ $output == *"Peer decisions in the last 7d: 1"* ]]
  [[ $output == *"not owner instructions"* ]]
  [ ! -e "$MARK/budget" ] && [ ! -e "$MARK/scores" ]
}

@test "session-start: a stale budget cache starts a detached refresh and is labelled" {
  touch -d '3 hours ago' "$XDG_DATA_HOME/crew/engine-budget.json"
  fresh_scores
  run bash "$HOOK" session-start
  [ "$status" -eq 0 ]
  [[ $output == *"cache 3h old, refreshing"* ]]
  waitfor "$MARK/budget"
}

@test "session-start: a missing or old scores cache starts refresh-scores" {
  touch "$XDG_DATA_HOME/crew/engine-budget.json"
  run bash "$HOOK" session-start
  [ "$status" -eq 0 ]
  waitfor "$MARK/scores"
  rm "$MARK/scores"
  fresh_scores 2020-01-01T00:00:00Z
  run bash "$HOOK" session-start
  waitfor "$MARK/scores"
}

@test "session-start: no peer, inbox or blocked lines when crew has nothing to say" {
  touch "$XDG_DATA_HOME/crew/engine-budget.json"
  fresh_scores
  CREW_FAIL=1 run bash "$HOOK" session-start
  [ "$status" -eq 0 ]
  [[ $output != *Peer* ]] && [[ $output != *Messages* ]] && [[ $output != *Blocked* ]]
}

@test "session-start: hung tools finish inside the bound and the hook exits 0" {
  touch "$XDG_DATA_HOME/crew/engine-budget.json"
  fresh_scores
  local t0=$SECONDS
  SLOW=1 run bash "$HOOK" session-start
  [ "$status" -eq 0 ]
  [ $((SECONDS - t0)) -lt 10 ]
  [[ $output != *"Enabled engines"* ]]
}

@test "session-start: hung tools are bounded without timeout(1) or gtimeout" {
  touch "$XDG_DATA_HOME/crew/engine-budget.json"
  fresh_scores
  local bin="$BATS_TEST_TMPDIR/notimeout" t p
  mkdir -p "$bin"
  for t in bash env git jq tr sed head cut date stat grep sleep kill dirname cat setsid nohup ps wc tail seq; do
    p=$(command -v "$t") && [[ $p == /* ]] && ln -sf "$p" "$bin/$t"
  done
  ln -sf "$STUB_DIR"/* "$bin/"
  ! PATH=$bin command -v timeout
  ! PATH=$bin command -v gtimeout
  local t0=$SECONDS
  SLOW=1 PATH=$bin run "$(command -v bash)" "$HOOK" session-start
  [ "$status" -eq 0 ]
  [ $((SECONDS - t0)) -lt 10 ]
  [[ $output != *"Enabled engines"* ]]
}

# --- prompt-submit ---

@test "prompt-submit: dead and stale print one line; alive prints nothing" {
  STATE=dead run bash "$HOOK" prompt-submit
  [ "$status" -eq 0 ]
  [[ $output == *"no stream lane"* ]]
  [ "$(wc -l <<<"$output")" -eq 1 ]
  STATE=stale run bash "$HOOK" prompt-submit
  [[ $output == *"crew stream --force --crew c1"* ]]
  STATE=alive run bash "$HOOK" prompt-submit
  [ "$status" -eq 0 ] && [ -z "$output" ]
}

@test "prompt-submit: a hung or failing crew prints nothing" {
  local t0=$SECONDS
  SLOW=1 run bash "$HOOK" prompt-submit
  [ "$status" -eq 0 ] && [ -z "$output" ]
  [ $((SECONDS - t0)) -lt 5 ]
  CREW_FAIL=1 run bash "$HOOK" prompt-submit
  [ "$status" -eq 0 ] && [ -z "$output" ]
}

@test "prompt-submit: gated for workers, plain sessions and a gone crew dir" {
  CREW_WORKER_ID='worker:x#s1-1' run bash "$HOOK" prompt-submit
  [ -z "$output" ]
  CREW_ROLE_ID='role:x:r' run bash "$HOOK" prompt-submit
  [ -z "$output" ]
  unset DISPATCHER_LAUNCHER_PID
  run bash "$HOOK" prompt-submit
  [ -z "$output" ]
  export DISPATCHER_LAUNCHER_PID=$$
  rm -rf "$CREW_DIR"
  run bash "$HOOK" prompt-submit
  [ -z "$output" ]
  [ ! -s "$STUB_LOG" ]
}

# --- pre-compact ---

@test "pre-compact: a dispatcher is reminded to hand off; others get nothing" {
  run bash "$HOOK" pre-compact
  [ "$status" -eq 0 ]
  [[ $output == *"dispatcher --fresh --note"* ]]
  [[ $output == *"c1"* ]]
  CREW_WORKER_ID='worker:x#s1-1' run bash "$HOOK" pre-compact
  [ "$status" -eq 0 ] && [ -z "$output" ]
  unset DISPATCHER_LAUNCHER_PID
  run bash "$HOOK" pre-compact
  [ -z "$output" ]
}

@test "hook: an unknown event prints nothing and exits 0" {
  run bash "$HOOK" bogus
  [ "$status" -eq 0 ] && [ -z "$output" ]
}

# --- wiring ---

@test "hooks.json wires the three events with their timeouts" {
  local f="$PLUGIN/hooks/hooks.json"
  jq -e . "$f" >/dev/null
  [ "$(jq -r '.hooks.SessionStart[0].hooks[0] | "\(.timeout) \(.command)"' "$f")" = '10 "${CLAUDE_PLUGIN_ROOT}"/scripts/dispatcher-hook.sh session-start' ]
  [ "$(jq -r '.hooks.UserPromptSubmit[0].hooks[0] | "\(.timeout) \(.command)"' "$f")" = '2 "${CLAUDE_PLUGIN_ROOT}"/scripts/dispatcher-hook.sh prompt-submit' ]
  [ "$(jq -r '.hooks.PreCompact[0].hooks[0] | "\(.timeout) \(.command)"' "$f")" = '2 "${CLAUDE_PLUGIN_ROOT}"/scripts/dispatcher-hook.sh pre-compact' ]
}

@test "plugin.json declares the two crew-bus monitors" {
  local f="$PLUGIN/.claude-plugin/plugin.json"
  jq -e . "$f" >/dev/null
  [ "$(jq -r '.experimental.monitors[] | select(.name == "crew-bus") | "\(.when) \(.command)"' "$f")" = 'always "${CLAUDE_PLUGIN_ROOT}"/scripts/crew-bus-monitor.sh' ]
  [ "$(jq -r '.experimental.monitors[] | select(.name == "crew-bus-promoted") | "\(.when) \(.command)"' "$f")" = 'on-skill-invoke:dispatcher "${CLAUDE_PLUGIN_ROOT}"/scripts/crew-bus-monitor.sh --promoted' ]
}
