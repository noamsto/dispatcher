bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# lead-launcher (#700): the worker lead's engine runs in a loop, and a session
# that runs `lead-launcher --fresh <execute|review>` is relaunched fresh in the
# same launcher process, keeping its environment.

setup() {
  load helpers
  LAUNCHER="$BATS_TEST_DIRNAME/../adapters/core/lead-launcher.sh"
  setup_repo
  stub_bin crew
  CREW_DIR="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  BRANCH=feat/7-a-thing
  NOTE="$CREW_DIR/artifacts/$BRANCH/handoff.md"
  mkdir -p "$CREW_DIR/launch" "${NOTE%/*}"
  SID=11111111-2222-4333-8444-555555555555
  ENGINE=claude
  ENGINE_ARGS=(--name iris --model opus)
  FIRST_PROMPT='first prompt'
  EXEC_PROMPT='relaunched fresh at the execute seam'
  REVIEW_PROMPT='relaunched fresh at the review seam'
  export LAUNCHER CREW_WORKER_ID='worker:feat/7-a-thing#s1' CREW_ID=c1
  unset CREW_LEAD_LAUNCHER_PID CREW_LEAD_RESTART CREW_LEAD_NOTE CREW_LEAD_SEAMS CREW_SUBAGENT
}

teardown() {
  [ -z "${live_pid:-}" ] || kill "$live_pid" 2>/dev/null || true
  [ -z "${sock:-}" ] || "$REAL_TMUX" -L "$sock" kill-server 2>/dev/null || true
  teardown_repo
}

run_lead() {
  bash "$LAUNCHER" --engine "$ENGINE" --crew-dir "$CREW_DIR" --branch "$BRANCH" \
    --note "$NOTE" --sid "$SID" --prompt "$FIRST_PROMPT" \
    --execute-prompt "$EXEC_PROMPT" --review-prompt "$REVIEW_PROMPT" \
    -- "$ENGINE" "${ENGINE_ARGS[@]}"
}

# _engine_stub <bin> — an engine stub that counts its launches in
# $STUB_DIR/launches and keeps each launch's argv (argv.N, one `ARG` line per
# word), last word (last.N) and the named variables it was given (env.N). Launch
# N then runs $STUB_DIR/body.N when one exists, and exits 0 otherwise.
_engine_stub() {
  cat >"$STUB_DIR/$1" <<'EOF'
#!/usr/bin/env bash
n=$(($(cat "$STUB_DIR/launches" 2>/dev/null || echo 0) + 1))
echo "$n" >"$STUB_DIR/launches"
printf 'ARG %s\n' "$@" >"$STUB_DIR/argv.$n"
printf '%s' "${@: -1}" >"$STUB_DIR/last.$n"
printf 'CREW_WORKER_ID=%s\nCREW_ID=%s\nCREW_LEAD_SEAMS=%s\nPATH=%s\n' \
  "${CREW_WORKER_ID-UNSET}" "${CREW_ID-UNSET}" "${CREW_LEAD_SEAMS-UNSET}" "${PATH-UNSET}" >"$STUB_DIR/env.$n"
[ -f "$STUB_DIR/body.$n" ] || exit 0
. "$STUB_DIR/body.$n"
EOF
  chmod +x "$STUB_DIR/$1"
}

_body() { printf '%s\n' "$2" >"$STUB_DIR/body.$1"; }

# The session writes its note and triggers from inside itself, so the chain is
# launcher → engine stub → inner launcher, walked with the real ps.
_write_note='printf "seen: 42\n" >"$CREW_LEAD_NOTE"'
_seam_body() { printf '%s\nbash "$LAUNCHER" --fresh %s\n' "$_write_note" "$1"; }

# _inner_body <call> — run <call> (an inner --fresh) and keep its exit and stderr.
_inner_body() {
  printf '%s\n' 'rc=0' "$1"' 2>"$STUB_DIR/inner.err" || rc=$?' 'echo "$rc" >"$STUB_DIR/inner.rc"'
}

_launches() { cat "$STUB_DIR/launches" 2>/dev/null || echo 0; }

_sid_of() { # launch number — the word after --session-id
  sed -n '/^ARG --session-id$/{n;s/^ARG //;p;}' "$STUB_DIR/argv.$1"
}

_env_of() { sed -n "s/^$2=//p" "$STUB_DIR/env.$1"; }

_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

_no_markers() { ! compgen -G "$CREW_DIR/launch/*.restart" >/dev/null; }

_relaunch_rows() { grep -h '"kind":"relaunch"' "$CREW_DIR/events.jsonl" 2>/dev/null || true; }

_uuid_re='^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'

# _refused <message> — the inner call recorded by _inner_body was refused with
# <message>, and nothing was relaunched, marked or posted.
_refused() {
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 1 ]
  [ "$(cat "$STUB_DIR/inner.rc")" = 1 ]
  [[ "$(cat "$STUB_DIR/inner.err")" == *"$1"* ]]
  _no_markers
  run ! grep -q '^status ' "$STUB_LOG"
}

@test "lead-launcher: with no marker the engine runs once with the session id and prompt last" {
  _engine_stub claude
  run --separate-stderr run_lead
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 1 ]
  [ "$(cat "$STUB_DIR/argv.1")" = "$(printf 'ARG %s\n' --name iris --model opus --session-id "$SID" "$FIRST_PROMPT")" ]
  [ "$(_env_of 1 CREW_LEAD_SEAMS)" = "" ]
  [ ! -e "$CREW_DIR/leads/$BRANCH" ]
  [ ! -e "$CREW_DIR/events.jsonl" ]
}

@test "lead-launcher: a nonzero engine exit with no marker is the launcher's exit" {
  _engine_stub claude
  _body 1 'exit 7'
  run run_lead
  [ "$status" -eq 7 ]
  [ "$(_launches)" = 1 ]
}

@test "lead-launcher: --fresh execute relaunches with a new session, the execute prompt and the same identity" {
  _engine_stub claude
  _body 1 "$(_seam_body execute)"
  run --separate-stderr run_lead
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 2 ]
  sid2=$(_sid_of 2)
  [[ "$sid2" =~ $_uuid_re ]]
  [ "$sid2" != "$SID" ]
  [ "$(cat "$STUB_DIR/last.2")" = "$EXEC_PROMPT" ]
  [ "$(sed -n '1,4p' "$STUB_DIR/argv.2")" = "$(sed -n '1,4p' "$STUB_DIR/argv.1")" ]
  [ "$(cat "$CREW_DIR/leads/$BRANCH")" = "claude $sid2" ]
  [ "$(_mode "$CREW_DIR/leads/$BRANCH")" = 600 ]
  rows=$(_relaunch_rows)
  [ "$(printf '%s\n' "$rows" | wc -l | tr -d ' ')" = 1 ]
  jq -e --arg sid "$sid2" --arg w "$CREW_WORKER_ID" --arg b "$BRANCH" \
    '.stage == "execute" and .engine_session == $sid and .worker_id == $w and .crew_id == "c1"
     and .branch == $b and .engine == "claude" and (.ts | type) == "number"' <<<"$rows"
  for v in CREW_WORKER_ID CREW_ID; do
    [ "$(_env_of 1 "$v")" = "$(_env_of 2 "$v")" ]
  done
  [ "$(_env_of 2 CREW_WORKER_ID)" = "$CREW_WORKER_ID" ]
  [ "$(_env_of 2 CREW_LEAD_SEAMS)" = execute ]
  _no_markers
  run ! grep -q '^status ' "$STUB_LOG"
}

@test "lead-launcher: execute then review relaunches twice, in order" {
  _engine_stub claude
  _body 1 "$(_seam_body execute)"
  _body 2 "$(_seam_body review)"
  run --separate-stderr run_lead
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 3 ]
  sid3=$(_sid_of 3)
  [[ "$sid3" =~ $_uuid_re ]]
  [ "$sid3" != "$(_sid_of 2)" ]
  [ "$(cat "$STUB_DIR/last.3")" = "$REVIEW_PROMPT" ]
  [ "$(cat "$CREW_DIR/leads/$BRANCH")" = "claude $sid3" ]
  [ "$(_env_of 3 CREW_LEAD_SEAMS)" = execute,review ]
  [ "$(_env_of 3 CREW_WORKER_ID)" = "$CREW_WORKER_ID" ]
  [ "$(_relaunch_rows | jq -r .stage | paste -sd, -)" = execute,review ]
  _no_markers
}

@test "lead-launcher: a pi lead is relaunched with --session-id before the prompt, its extension kept" {
  ENGINE=pi
  ENGINE_ARGS=(--name iris -e /ext/subagent.ts --no-approve)
  _engine_stub pi
  _body 1 "$(_seam_body execute)"
  run --separate-stderr run_lead
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 2 ]
  sid2=$(_sid_of 2)
  [ "$(tail -n 3 "$STUB_DIR/argv.2")" = "$(printf 'ARG %s\n' --session-id "$sid2" "$EXEC_PROMPT")" ]
  grep -qx 'ARG /ext/subagent.ts' "$STUB_DIR/argv.2"
  [ "$(cat "$CREW_DIR/leads/$BRANCH")" = "pi $sid2" ]
  [ "$(_relaunch_rows | jq -r .engine)" = pi ]
}

@test "lead-launcher: inner --fresh refuses a stage already relaunched" {
  _engine_stub claude
  _body 1 "$_write_note
$(_inner_body 'CREW_LEAD_SEAMS=execute bash "$LAUNCHER" --fresh execute')"
  run run_lead
  _refused "already relaunched at the execute seam"
}

@test "lead-launcher: inner --fresh refuses execute after review" {
  _engine_stub claude
  _body 1 "$_write_note
$(_inner_body 'CREW_LEAD_SEAMS=review bash "$LAUNCHER" --fresh execute')"
  run run_lead
  _refused "execute seam after the review seam"
}

@test "lead-launcher: inner --fresh refuses an unknown stage" {
  _engine_stub claude
  _body 1 "$_write_note
$(_inner_body 'bash "$LAUNCHER" --fresh plan')"
  run run_lead
  _refused "unknown stage"
}

@test "lead-launcher: inner --fresh refuses from a subagent" {
  _engine_stub claude
  _body 1 "$_write_note
$(_inner_body 'CREW_SUBAGENT=1 bash "$LAUNCHER" --fresh execute')"
  run run_lead
  _refused "CREW_SUBAGENT"
}

@test "lead-launcher: inner --fresh refuses a missing note" {
  _engine_stub claude
  _body 1 "$(_inner_body 'bash "$LAUNCHER" --fresh execute')"
  run run_lead
  _refused "$NOTE"
}

@test "lead-launcher: inner --fresh refuses an empty note" {
  _engine_stub claude
  _body 1 ': >"$CREW_LEAD_NOTE"
'"$(_inner_body 'bash "$LAUNCHER" --fresh execute')"
  run run_lead
  _refused "not a non-empty regular file"
}

@test "lead-launcher: inner --fresh refuses a note older than 15 minutes" {
  _engine_stub claude
  OLD_STAMP=$(date -v-20M +%Y%m%d%H%M 2>/dev/null || date -d '-20 min' +%Y%m%d%H%M)
  export OLD_STAMP
  _body 1 "$_write_note"'
touch -t "$OLD_STAMP" "$CREW_LEAD_NOTE"
'"$(_inner_body 'bash "$LAUNCHER" --fresh execute')"
  run run_lead
  _refused "not modified in the last 15 minutes"
}

@test "lead-launcher: inner --fresh whose launcher pid is no ancestor refuses and kills nothing" {
  printf 'seen: 42\n' >"$NOTE"
  sleep 30 &
  live_pid=$!
  CREW_LEAD_LAUNCHER_PID=$live_pid CREW_LEAD_RESTART="$CREW_DIR/launch/lead.$live_pid.restart" CREW_LEAD_NOTE=$NOTE \
    run bash "$LAUNCHER" --fresh execute
  [ "$status" -eq 1 ]
  [[ "$output" == *"CREW_LEAD_LAUNCHER_PID=$live_pid is not an ancestor"* ]]
  kill -0 "$live_pid"
  _no_markers
}

@test "lead-launcher: inner --fresh refuses launcher pid 1 before walking the chain" {
  printf 'seen: 42\n' >"$NOTE"
  CREW_LEAD_LAUNCHER_PID=1 CREW_LEAD_RESTART="$CREW_DIR/launch/lead.1.restart" CREW_LEAD_NOTE=$NOTE \
    run bash "$LAUNCHER" --fresh execute
  [ "$status" -eq 1 ]
  [[ "$output" == *"CREW_LEAD_LAUNCHER_PID=1 is not a launcher pid"* ]]
  _no_markers
}

@test "lead-launcher: inner --fresh refuses a relative restart path or note" {
  printf 'seen: 42\n' >"$NOTE"
  CREW_LEAD_LAUNCHER_PID=$$ CREW_LEAD_RESTART=lead.restart CREW_LEAD_NOTE=$NOTE \
    run bash "$LAUNCHER" --fresh execute
  [ "$status" -eq 1 ]
  [[ "$output" == *"absolute"* ]]
  _no_markers
}

@test "lead-launcher: inner --fresh refuses an ancestor that is not a lead-launcher" {
  printf 'seen: 42\n' >"$NOTE"
  export CREW_LEAD_NOTE=$NOTE CREW_LEAD_RESTART="$CREW_DIR/launch/lead.x.restart"
  # The target is this bash, whose args are its script text, with no
  # "lead-launcher". Its child is the inner call itself, so a pass would TERM it.
  run bash -c 'CREW_LEAD_LAUNCHER_PID=$$ bash "$LAUNCHER" --fresh execute; echo "rc=$?"'
  [[ "$output" == *"is an ancestor but not a lead-launcher"* ]]
  [[ "$output" == *"rc=1"* ]]
  _no_markers
}

@test "lead-launcher: inner --fresh refuses an engine pid whose args name the launcher" {
  printf 'seen: 42\n' >"$NOTE"
  export CREW_LEAD_NOTE=$NOTE CREW_LEAD_RESTART="$CREW_DIR/launch/lead.x.restart"
  # A bash copy named like an engine: comm is not bash, and its -c text carries
  # "lead-launcher" the way the lead's prompt does.
  cp "$(command -v bash)" "$STUB_DIR/fakeengine"
  run "$STUB_DIR/fakeengine" -c '# lead-launcher
CREW_LEAD_LAUNCHER_PID=$$ bash "$LAUNCHER" --fresh execute; echo "rc=$?"'
  [[ "$output" == *"is an ancestor but not a lead-launcher"* ]]
  [[ "$output" == *"rc=1"* ]]
  _no_markers
}

@test "lead-launcher: a marker for a stage already relaunched posts failed and ends the loop" {
  _engine_stub claude
  _body 1 "$(_seam_body execute)"
  # Written by the session itself, bypassing the inner check.
  _body 2 'printf "%s execute\n" "$(date +%s)" >"$CREW_LEAD_RESTART"'
  run --separate-stderr run_lead
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 2 ]
  grep -qF "status $CREW_WORKER_ID failed lead-launcher: already relaunched at the execute seam" "$STUB_LOG"
  [ "$(_relaunch_rows | wc -l | tr -d ' ')" = 1 ]
  _no_markers
}

@test "lead-launcher: a marker for execute after review posts failed and ends the loop" {
  _engine_stub claude
  _body 1 "$(_seam_body review)"
  _body 2 'printf "%s execute\n" "$(date +%s)" >"$CREW_LEAD_RESTART"'
  run --separate-stderr run_lead
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 2 ]
  grep -qF "status $CREW_WORKER_ID failed lead-launcher: execute seam after the review seam" "$STUB_LOG"
  _no_markers
}

@test "lead-launcher: a marker 300s old posts failed: one launch, marker deleted" {
  _engine_stub claude
  _body 1 'printf "%s execute\n" "$(($(date +%s) - 300))" >"$CREW_LEAD_RESTART"
exit 3'
  run --separate-stderr run_lead
  [ "$status" -eq 3 ]
  [ "$(_launches)" = 1 ]
  grep -qF "status $CREW_WORKER_ID failed lead-launcher: stale restart marker" "$STUB_LOG"
  [ ! -e "$CREW_DIR/leads/$BRANCH" ]
  _no_markers
}

@test "lead-launcher: a marker naming an unknown stage posts failed" {
  _engine_stub claude
  _body 1 'printf "%s plan\n" "$(date +%s)" >"$CREW_LEAD_RESTART"'
  run --separate-stderr run_lead
  [ "$(_launches)" = 1 ]
  grep -qF "status $CREW_WORKER_ID failed lead-launcher: unknown stage" "$STUB_LOG"
  _no_markers
}

# bats test_tags=timing
@test "lead-launcher: an engine that ignores SIGTERM is killed after ~10s and relaunched" {
  _engine_stub claude
  # The subshell outlives the KILLed engine to record the inner call's exit, and
  # holds run's stdout open until it has.
  _body 1 'trap "" TERM
'"$_write_note"'
(
'"$(_inner_body 'bash "$LAUNCHER" --fresh execute')"'
)'
  start=$SECONDS
  run --separate-stderr run_lead
  elapsed=$((SECONDS - start))
  [ "$status" -eq 0 ]
  [ "$(cat "$STUB_DIR/inner.rc")" = 0 ]
  [[ "$(cat "$STUB_DIR/inner.err")" != *"relaunch failed"* ]]
  [ "$elapsed" -ge 9 ] && [ "$elapsed" -lt 20 ]
  [ "$(_launches)" = 2 ]
  [ "$(cat "$STUB_DIR/last.2")" = "$EXEC_PROMPT" ]
  _no_markers
}

@test "lead-launcher: a symlinked leads record is left alone, and the relaunch still happens" {
  _engine_stub claude
  _body 1 "$(_seam_body execute)"
  mkdir -p "$CREW_DIR/leads/feat" "$BATS_TEST_TMPDIR/elsewhere"
  printf 'claude keep\n' >"$BATS_TEST_TMPDIR/elsewhere/rec"
  ln -s "$BATS_TEST_TMPDIR/elsewhere/rec" "$CREW_DIR/leads/$BRANCH"
  run --separate-stderr run_lead
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 2 ]
  [ -L "$CREW_DIR/leads/$BRANCH" ]
  [ "$(cat "$BATS_TEST_TMPDIR/elsewhere/rec")" = "claude keep" ]
  [[ "$stderr" == *"dispatch: $CREW_DIR/leads/$BRANCH is a symlink or not a regular file"* ]]
}

@test "lead-launcher: the engine's PATH drops the launcher's pinned dirs" {
  _engine_stub claude
  local pinned="$BATS_TEST_TMPDIR/store/h-launcher/bin"
  mkdir -p "$pinned"
  sed "s|@launcherRuntimePath@|$pinned|" "$LAUNCHER" >"$BATS_TEST_TMPDIR/lead-launcher-pinned.sh"
  LAUNCHER="$BATS_TEST_TMPDIR/lead-launcher-pinned.sh"
  PATH="$pinned:$PATH" run run_lead
  [ "$status" -eq 0 ]
  path1=$(_env_of 1 PATH)
  [[ ":$path1:" != *":$pinned:"* ]]
  [[ ":$path1:" == *":$STUB_DIR:"* ]]
}

@test "lead-launcher: in a real tmux pane the engine is the pane's foreground command" {
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"
  sock="ll$$"
  "$REAL_TMUX" -L "$sock" -f /dev/null new-session -d -s t -x 200 -y 50 sleep 60
  "$REAL_TMUX" -L "$sock" set-option -g remain-on-exit on
  # A bash copy under its own name: nix's sleep is the multicall coreutils
  # binary, which macOS names coreutils. The trailing `:` keeps it from exec'ing.
  cp "$(command -v bash)" "$STUB_DIR/fakeengine"
  # More than one argument: tmux execs the launcher itself, so pane_pid is it.
  pane=$("$REAL_TMUX" -L "$sock" new-window -P -F '#{pane_id}' -t t: \
    bash "$LAUNCHER" --engine claude --crew-dir "$CREW_DIR" --branch "$BRANCH" \
    --note "$NOTE" --sid "$SID" --prompt p --execute-prompt e --review-prompt r \
    -- "$STUB_DIR/fakeengine" -c 'sleep 10; :')
  launcher_pid=$("$REAL_TMUX" -L "$sock" display-message -p -t "$pane" '#{pane_pid}')
  child="" cmd=""
  for _ in $(seq 40); do
    child=$(ps -A -o pid=,ppid= | awk -v p="$launcher_pid" '$2 == p { print $1; exit }')
    cmd=$("$REAL_TMUX" -L "$sock" display-message -p -t "$pane" '#{pane_current_command}')
    [ -n "$child" ] && [ "$cmd" = fakeengine ] && break
    sleep 0.1
  done
  [ -n "$child" ]
  comm=$(ps -o comm= -p "$child")
  [ "$cmd" != bash ]
  [ "$cmd" = "${comm##*/}" ]
}
