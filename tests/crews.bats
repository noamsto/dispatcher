bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
}

teardown() {
  teardown_repo
}

# _crews_row <id> -> the matching TSV row from $output (set by a prior `run
# run_crew crews`), or empty if <id> has no row. Exact match on the first
# field, so ids never need regex escaping.
_crews_row() {
  awk -F'\t' -v id="$1" '$1==id' <<<"$output"
}

# _seed_crew_event <crew-id> — append one well-formed status event carrying
# <crew-id> straight to events.jsonl, bypassing `crew status`. That is the real
# attack path for a hostile id: `dispatch --crew-id` writes it unsanitised, and
# the log is `adopt`'s own "is this id known" source, so seeding it is what
# makes `adopt` reach the path/eval code under test instead of the unknown-id
# refusal.
_seed_crew_event() {
  local log
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc --arg id "$1" \
    '{ts:(now*1000|floor), kind:"status", crew_id:$id, from:"worker:feat/x#s1", state:"working"}' \
    >>"$log"
}

# _run_dispatch_no_crew_id — reaches dispatch.sh's crew-id abort. Needs
# tests/dispatch.bats's env hygiene: a dev shell exports CREW_ID and
# DISPATCH_PROFILE, which would otherwise suppress the very abort under test.
# A full arg set is required too — dispatch demands --effort before it ever
# looks at crew id.
_run_dispatch_no_crew_id() {
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE
  run bash -euo pipefail "$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh" trivial some-model --effort low
}

# _stub_launcher_agents — dispatcher.sh execs a real agent CLI and touches
# tmux before it ever reaches the discovery-notice logic under test; without
# these stubs (tests/dispatcher.bats's full setup) the launch dies or stamps
# the developer's live tmux window.
_stub_launcher_agents() {
  stub_bin tmux
  stub_bin claude
  stub_bin codex
  stub_bin cursor-agent
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
  unset TMUX
}

# stub_crew_launcher <crews-tsv> — a `crew` stub richer than the shared
# stub_bin (which prints nothing and exits 0, so `crew new` would return an
# empty id and `crew crews` no rows): `new` always mints the fixed id
# "c-self", and `crews` prints <crews-tsv> verbatim, so a test controls
# exactly what the launcher's discovery-notice count sees. The trailing `\n`
# is not decoration: the caller passes a `$(...)` command substitution, which
# strips its own trailing newline, and the launcher's `while read` silently
# drops a final row that isn't newline-terminated.
stub_crew_launcher() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  printf '%s\n' "$1" >"$STUB_DIR/crews.tsv"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
new) echo c-self ;;
crews) cat "$STUB_DIR/crews.tsv" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# ---- crews ------------------------------------------------------------

@test "crews: an events-only crew (never registered) appears with no pid" {
  CREW_ID=c-events run_crew status "worker:feat/x#s1" working
  run run_crew crews
  [ "$status" -eq 0 ]
  row="$(_crews_row c-events)"
  [ -n "$row" ]
  [ "$(cut -f4 <<<"$row")" = "1" ]
  [ "$(cut -f5 <<<"$row")" = "—" ]
  [ "$(cut -f6 <<<"$row")" = "—" ]
}

@test "crews: a crew-dir-only crew (no events) has zero workers and no timestamps" {
  (exit 0) & dead_pid=$!
  wait "$dead_pid" 2>/dev/null || true
  CREW_ID=c-dir run_crew register "$dead_pid"
  run run_crew crews
  [ "$status" -eq 0 ]
  row="$(_crews_row c-dir)"
  [ -n "$row" ]
  [ "$(cut -f2 <<<"$row")" = "—" ]
  [ "$(cut -f3 <<<"$row")" = "—" ]
  [ "$(cut -f4 <<<"$row")" = "0" ]
  [ "$(cut -f5 <<<"$row")" = "$dead_pid" ]
  [ "$(cut -f6 <<<"$row")" = "no" ]
}

@test "crews: a crew present in both sources appears exactly once" {
  CREW_ID=c-both run_crew register "$$"
  CREW_ID=c-both run_crew status "worker:feat/x#s1" working
  run run_crew crews
  [ "$status" -eq 0 ]
  count="$(awk -F'\t' -v id=c-both '$1==id' <<<"$output" | wc -l | tr -d ' ')"
  [ "$count" -eq 1 ]
}

@test "crews: workers counts distinct worker branches under one crew" {
  CREW_ID=c-workers run_crew status "worker:feat/a#s1" working
  CREW_ID=c-workers run_crew status "worker:feat/b#s1" working
  run run_crew crews
  row="$(_crews_row c-workers)"
  [ "$(cut -f4 <<<"$row")" = "2" ]
}

@test "crews: a live registered pid reads alive yes" {
  sleep 30 & pid=$!
  CREW_ID=c-live run_crew register "$pid"
  run run_crew crews
  row="$(_crews_row c-live)"
  [ "$(cut -f5 <<<"$row")" = "$pid" ]
  [ "$(cut -f6 <<<"$row")" = "yes" ]
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

@test "crews: a seeded crew-id-less reap event adds no phantom row" {
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:(now*1000|floor), kind:"reap", branch:"feat/x", pr:"", pr_state:"", worktree:""}' >>"$log"
  run run_crew crews
  [ "$status" -eq 0 ]
  expected=$'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive'
  [ "$output" = "$expected" ]
}

@test "crews: register then deregister leaves header-only output, no '*' row" {
  CREW_ID=c1 run_crew register "$$"
  CREW_ID=c1 run_crew deregister
  run run_crew crews
  [ "$status" -eq 0 ]
  expected=$'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive'
  [ "$output" = "$expected" ]
}

@test "crews: a pid file of 0 reads as not alive" {
  CREW_ID=c-zero run_crew register 0
  run run_crew crews
  [ "$status" -eq 0 ]
  row="$(_crews_row c-zero)"
  [ "$(cut -f5 <<<"$row")" = "0" ]
  [ "$(cut -f6 <<<"$row")" = "no" ]
}

@test "crews: a torn trailing line still lists the well-formed crews" {
  CREW_ID=c-intact run_crew status "worker:feat/x#s1" working
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  printf '{"ts":1785951264000,"crew_id":"c-to' >>"$log"
  run run_crew crews
  [ "$status" -eq 0 ]
  [ -n "$(_crews_row c-intact)" ]
}

@test "crews: no crew dir at all still exits 0 with just the header" {
  run run_crew crews
  [ "$status" -eq 0 ]
  expected=$'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive'
  [ "$output" = "$expected" ]
}

# ---- id -----------------------------------------------------------------

@test "id: two consecutive calls with CREW_ID unset both refuse, no id is minted" {
  CREW_ID= run --separate-stderr run_crew id
  [ "$status" -eq 1 ]
  [ -z "$output" ]
  first_stderr="$stderr"
  CREW_ID= run --separate-stderr run_crew id
  [ "$status" -eq 1 ]
  [ -z "$output" ]
  [ "$stderr" = "$first_stderr" ]
}

@test "id: the refusal names crew crews, crew adopt, and crew new" {
  CREW_ID= run run_crew id
  [ "$status" -eq 1 ]
  [[ "$output" == *"crew crews"* ]]
  [[ "$output" == *"crew adopt"* ]]
  [[ "$output" == *"crew new"* ]]
}

@test "id: honours CREW_ID when set" {
  CREW_ID=1720800000-12345 run run_crew id
  [ "$status" -eq 0 ]
  [ "$output" = "1720800000-12345" ]
}

@test "id: resolves from WORKER_TASK.md when CREW_ID is unset" {
  printf 'crew_id: 1785381776-999\n' >WORKER_TASK.md
  CREW_ID= run run_crew id
  [ "$status" -eq 0 ]
  [ "$output" = "1785381776-999" ]
}

# ---- new ------------------------------------------------------------------

@test "new: mints <epoch>-<pid>" {
  run run_crew new
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+-[0-9]+$ ]]
}

@test "new: two consecutive calls differ" {
  first="$(run_crew new)"
  second="$(run_crew new)"
  [ "$first" != "$second" ]
}

@test "new: works outside a git repo" {
  cd /
  run run_crew new
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+-[0-9]+$ ]]
}

# ---- adopt ------------------------------------------------------------

@test "adopt: refuses an unknown id" {
  run run_crew adopt nope-1234
  [ "$status" -eq 1 ]
  [[ "$output" == *"no crew 'nope-1234'"* ]]
}

@test "adopt: refuses a crew whose registered pid is alive, without leaking the pid" {
  sleep 30 & pid=$!
  CREW_ID=c-live run_crew register "$pid"
  run run_crew adopt c-live
  [ "$status" -eq 1 ]
  [[ "$output" == *"still has a live dispatcher"* ]]
  [[ "$output" != *"$pid"* ]]
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

@test "adopt: re-adopting this session's own live crew succeeds" {
  # $$ is the bats process — live, and an ancestor of the crew.sh shell, which
  # is the proof `adopt` accepts in place of a caller-supplied pid.
  CREW_ID=c-mine run_crew register "$$"
  run run_crew adopt c-mine "$$"
  [ "$status" -eq 0 ]
  [ "$output" = "c-mine" ]
}

@test "adopt: a registered pid of 0 does not read as a live dispatcher" {
  CREW_ID=c-zero run_crew register 0
  run run_crew adopt c-zero "$$"
  [ "$status" -eq 0 ]
  [ "$output" = "c-zero" ]
}

@test "adopt: prints the bare id, with no export prefix" {
  CREW_ID=c-bare run_crew status "worker:feat/x#s1" working
  run run_crew adopt c-bare "$$"
  [ "$status" -eq 0 ]
  [ "$output" = "c-bare" ]
  [[ "$output" != export* ]]
}

@test "adopt: refuses an injection-shaped id even when the bus log carries it" {
  marker="$BATS_TEST_TMPDIR/PWNED"
  id="x; touch $marker"
  _seed_crew_event "$id"
  run run_crew adopt "$id" "$$"
  [ "$status" -eq 1 ]
  [[ "$output" == *"invalid crew id"* ]]
  [ ! -e "$marker" ]
}

@test "adopt: refuses a traversal id and writes nothing outside the crew dir" {
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  _seed_crew_event "../../escaped"
  run run_crew adopt "../../escaped" "$$"
  [ "$status" -eq 1 ]
  [[ "$output" == *"invalid crew id"* ]]
  [ ! -e "$common/escaped" ]
  [ ! -e "$common/crew/escaped" ]
}

@test "adopt: --force overrides a live-pid refusal" {
  sleep 30 & pid=$!
  CREW_ID=c-live run_crew register "$pid"
  run run_crew adopt c-live --force "$$"
  [ "$status" -eq 0 ]
  [ "$output" = "c-live" ]
  cdir="$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/c-live"
  [ "$(cat "$cdir/pid")" = "$$" ]
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

@test "adopt: --force after the id does not write the literal --force as the pid" {
  sleep 30 & pid=$!
  CREW_ID=c-live run_crew register "$pid"
  run run_crew adopt c-live --force
  [ "$status" -eq 0 ]
  cdir="$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/c-live"
  written_pid="$(cat "$cdir/pid")"
  [ "$written_pid" != "--force" ]
  [[ "$written_pid" =~ ^[0-9]+$ ]]
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

@test "adopt: an events-only crew is adoptable" {
  CREW_ID=c-events run_crew status "worker:feat/x#s1" working
  run run_crew adopt c-events "$$"
  [ "$status" -eq 0 ]
  [ "$output" = "c-events" ]
}

@test "adopt: a dead-pid registered crew is adoptable without --force" {
  (exit 0) & dead_pid=$!
  wait "$dead_pid" 2>/dev/null || true
  CREW_ID=c-dead run_crew register "$dead_pid"
  run run_crew adopt c-dead "$$"
  [ "$status" -eq 0 ]
  [ "$output" = "c-dead" ]
}

@test "adopt: missing id is a usage error" {
  run run_crew adopt
  [ "$status" -eq 1 ]
  printf '%s' "$output" | grep -qF 'adopt [--force] <id> [pid]'
}

# ---- register -----------------------------------------------------------

@test "register: the refusal names the discovery commands" {
  CREW_ID= run run_crew register
  [ "$status" -eq 1 ]
  [[ "$output" == *"crew crews"* ]]
  [[ "$output" == *"crew adopt"* ]]
  [[ "$output" == *"crew new"* ]]
}

# ---- $PPID must reach every message unexpanded ---------------------------

@test "id, register, and dispatch's abort messages all spell \$PPID literally" {
  CREW_ID= run run_crew id
  [[ "$output" == *'crew adopt <id> $PPID'* ]]

  CREW_ID= run run_crew register
  [[ "$output" == *'crew adopt <id> $PPID'* ]]

  _run_dispatch_no_crew_id
  [[ "$output" == *'crew adopt <id> $PPID'* ]]
}

# ---- dispatch's abort message --------------------------------------------

@test "dispatch: aborts with no crew id and names the discovery commands" {
  _run_dispatch_no_crew_id
  [ "$status" -eq 1 ]
  [[ "$output" == *"crew crews"* ]]
  [[ "$output" == *"crew adopt"* ]]
  [[ "$output" == *"crew new"* ]]
}

# ---- dispatcher.sh: mint + discovery notice ------------------------------

@test "dispatcher: minting with other crews on disk prints the discovery notice" {
  _stub_launcher_agents
  stub_crew_launcher "$(printf 'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc-other\t5\t5\t1\t111\tno\n')"
  unset CREW_ID
  run bash -euo pipefail "$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh"
  [ "$status" -eq 0 ]
  [[ "$output" == *"minted a NEW crew"* ]]
  [[ "$output" == *"CREW_ID=<id> dispatcher"* ]]
}

@test "dispatcher: minting with no other crews prints nothing" {
  _stub_launcher_agents
  stub_crew_launcher "$(printf 'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n')"
  unset CREW_ID
  run bash -euo pipefail "$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh"
  [ "$status" -eq 0 ]
  [[ "$output" != *"minted a NEW crew"* ]]
}

@test "dispatcher: an inherited CREW_ID prints no discovery notice" {
  _stub_launcher_agents
  stub_crew_launcher "$(printf 'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\nc-other\t5\t5\t1\t111\tno\n')"
  CREW_ID=c-existing run bash -euo pipefail "$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh"
  [ "$status" -eq 0 ]
  [[ "$output" != *"minted a NEW crew"* ]]
  # The notice block is skipped entirely on an inherited id — it must never
  # even call `crew crews` to check.
  run grep -cx crews "$STUB_LOG"
  [ "$output" = "0" ]
}
