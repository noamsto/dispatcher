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
  CREW_ID=c1 TMUX_PANE='%12' run_crew register 4242
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

@test "pi-agent-dir: literal keys are read through, never copied" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  run grep -rF SECRET-FIXTURE-123 "$WORKER"
  [ "$status" -eq 1 ]
  run grep -rF 'sk-a$b' "$WORKER"
  [ "$status" -eq 1 ]
  key=$(jq -r .opencode.key "$WORKER/auth.json")
  [[ "$key" == '!'* ]]
  [[ "$key" == *auth.json* ]]
  [ "$(bash -c "${key#!}")" = SECRET-FIXTURE-123 ]
  key=$(jq -r .lit.key "$WORKER/auth.json")
  [ "$(bash -c "${key#!}")" = 'sk-a$b' ]
}

@test "pi-agent-dir: references stay verbatim; oauth, bad names and env are dropped" {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  [ "$(jq -c .openrouter "$WORKER/auth.json")" = '{"type":"api_key","key":"$OPENROUTER_API_KEY"}' ]
  [ "$(jq -r .deepseek.key "$WORKER/auth.json")" = '!echo x' ]
  [ "$(jq 'has("anthropic")' "$WORKER/auth.json")" = false ]
  [ "$(jq 'has("a b")' "$WORKER/auth.json")" = false ]
}

@test "pi-agent-dir: auth is 600 and the ambient dir is untouched" {
  _pi_fixture
  before=$(sha256sum "$AMBIENT"/*)
  run_crew pi-agent-dir >/dev/null
  [ "$(stat -c %a "$WORKER/auth.json")" = 600 ]
  [ "$(sha256sum "$AMBIENT"/*)" = "$before" ]
  [ ! -e "$WORKER/trust.json" ]
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

@test "pi-agent-dir: no ambient auth.json seeds an empty auth" {
  _pi_fixture
  rm "$AMBIENT/auth.json"
  run_crew pi-agent-dir >/dev/null
  [ "$(jq -c . "$WORKER/auth.json")" = '{}' ]
}

# Seed once, break the ambient auth.json with $1, and expect a refusal that
# leaves the seeded worker auth.json byte-identical.
_pi_broken_auth() {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  before=$(sha256sum "$WORKER/auth.json")
  printf '%s\n' "$1" >"$AMBIENT/auth.json"
}

@test "pi-agent-dir: a malformed ambient auth.json is refused, worker auth kept" {
  _pi_broken_auth '{"opencode":{"ty'
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"$AMBIENT/auth.json is unreadable or not a JSON object"* ]]
  [ "$(sha256sum "$WORKER/auth.json")" = "$before" ]
}

@test "pi-agent-dir: a non-object ambient auth.json is refused" {
  _pi_broken_auth '[]'
  run --separate-stderr run_crew pi-agent-dir
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"not a JSON object"* ]]
  [ "$(sha256sum "$WORKER/auth.json")" = "$before" ]
}

@test "pi-agent-dir: an unreadable ambient auth.json is refused" {
  [ "$(id -u)" -eq 0 ] && skip "root reads mode 000 files"
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  before=$(sha256sum "$WORKER/auth.json")
  chmod 000 "$AMBIENT/auth.json"
  run --separate-stderr run_crew pi-agent-dir
  chmod 600 "$AMBIENT/auth.json"
  [ "$status" -ne 0 ]
  [[ "$stderr" == *"unreadable or not a JSON object"* ]]
  [ "$(sha256sum "$WORKER/auth.json")" = "$before" ]
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
  key=$(jq -r .opencode.key "$WORKER/auth.json")
  [[ "$key" == *"$AMBIENT/auth.json"* ]]
}

# Seed once, then drop the ambient auth.json so the test can put a non-regular
# entry (or nothing reachable) in its place.
_pi_seeded() {
  _pi_fixture
  run_crew pi-agent-dir >/dev/null
  before=$(sha256sum "$WORKER/auth.json")
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
  [ "$(sha256sum "$WORKER/auth.json")" = "$before" ]
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

@test "pi-agent-dir: a relative ambient auth.json symlink is read through" {
  _pi_fixture
  mkdir -p "$HOME/.pi/secrets"
  mv "$AMBIENT/auth.json" "$HOME/.pi/secrets/auth.json"
  ln -s ../secrets/auth.json "$AMBIENT/auth.json"
  run_crew pi-agent-dir >/dev/null
  key=$(jq -r .opencode.key "$WORKER/auth.json")
  [[ "$key" == *"$AMBIENT/auth.json"* ]]
  [ "$(cd / && bash -c "${key#!}")" = SECRET-FIXTURE-123 ]
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
  CREW_ID=c1 run_crew reply "worker:feat/x" "ship it after the fix" --no-wake
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
  CREW_ID=c1 run_crew reply "worker:feat/x" "resume-directive" --no-wake
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
  ! grep -q remove "$STUB_LOG"
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
  ! grep -q 'kill-window' "$STUB_LOG"
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
  ! grep -q 'kill-window' "$STUB_LOG"
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
  ! grep -q 'kill-window' "$STUB_LOG"
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
  ! grep -q 'kill-window' "$STUB_LOG"
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
  ! grep -q 'kill-window' "$STUB_LOG"
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
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/42-reap-me" done "" "https://example.com/pr/7"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/42-reap-me"* ]]
  grep -q 'pr view https://example.com/pr/7 --json closingIssuesReferences' "$STUB_LOG"
  grep -q 'issue edit 42 --remove-label dispatched' "$STUB_LOG"
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
  stub_bin wt
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
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/45-reap-me" done "" "https://example.com/pr/11"
  CREW_ID=c1 run run_crew reap
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/45-reap-me"* ]]
  [[ "$output" == *"could not resolve closing issues for PR https://example.com/pr/11 (feat/45-reap-me)"* ]]
  ! grep -q 'issue edit' "$STUB_LOG"
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
  stub_bin wt
  CREW_ID=c1 run_crew status "worker:feat/44-reap-me" done "" "https://example.com/pr/10"
  CREW_ID=c1 run run_crew reap --quiet
  [ "$status" -eq 0 ]
  [[ "$output" == *"reaped feat/44-reap-me"* ]]
  ! grep -q 'issue edit' "$STUB_LOG"
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
  ! grep -q 'remove' "$STUB_LOG"
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
  ! grep -q 'remove' "$STUB_LOG"
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
  ! grep -q 'remove' "$STUB_LOG"
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

@test "stall-watch: --engine codex gets no prompt or meter detector, and never failed" {
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  # --max-life 8: the single expected stalled: line must fire before the
  # top-of-loop exit; at 4 a stretched pre-sample gap could starve it (#185,
  # same as D0's fix above).
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 8
  # The static pane is not classifiable for codex, so it falls to D0s.
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [ "${lines[0]}" = "blocked|stalled: no output for 1s" ]
  run bash -c "bus | grep -c 'prompt:' || true"
  [ "$output" = "0" ]
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
    cursor-grok-4.6-low cursor-grok-4.6-medium cursor-grok-4.6-high \
    claude-fable-5; do
    grep -qF "$token" <<<"$doc_slice" || {
      printf 'token %s missing from the Model map/Burn classes doc slice\n' "$token" >&2
      return 1
    }
  done
}

# _burn_weight matches on glob families (`cursor-grok-4.[0-9]-medium`) so that
# 4.5 and 4.6 price alike, which a token grep of the function body cannot see.
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

# ---- wake (#186) ----
#
# Frame fixtures and a stateful tmux stub for `crew wake-class` / `crew nudge`.

WAKE_PROMPT='crew wake: read your crew inbox and continue'

# _wake_write_frames — write every pinned frame fixture under
# $WAKE_DIR/frames/<name>, escapes kept (as from `tmux capture-pane -e -p`).
_wake_write_frames() {
  local esc nbsp rule fg244 fg246 fgreset dim reset0
  esc=$'\033'
  nbsp=$'\302\240'
  rule=$(printf '─%.0s' $(seq 1 40))
  fg244="${esc}[38;5;244m"
  fg246="${esc}[38;5;246m"
  fgreset="${esc}[39m"
  dim="${esc}[2m"
  reset0="${esc}[0m"

  local top bottom status_insert status_normal box_empty
  top="${fg244}${rule}${fgreset}"
  bottom="$top"
  status_insert="  ${fg246}-- INSERT --${fgreset} ⏵⏵ auto mode on"
  status_normal="  ${fg246}-- NORMAL --${fgreset} ⏵⏵ auto mode on"
  box_empty="${fgreset}❯${nbsp}"

  printf '%s\n%s\n%s\n%s\n%s\n' \
    "● earlier answer" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/idle"

  local box_ghost="${fgreset}❯${nbsp}${dim}go ahead${reset0}"
  printf '%s\n%s\n%s\n%s\n%s\n' \
    "● earlier answer" "$top" "$box_ghost" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/idle_ghost"

  local box_unterminated="${fgreset}❯${nbsp}${dim}go ahead"
  printf '%s\n%s\n%s\n%s\n%s\n' \
    "● earlier answer" "$top" "$box_unterminated" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/ghost_unterminated"

  local box_typed="${fgreset}❯${nbsp}crew wake: read your crew inbox and continue"
  printf '%s\n%s\n%s\n%s\n' "$top" "$box_typed" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/typed"

  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "❯ crew wake: read your crew inbox and continue" "· Smooshing…" \
    "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/submitted"

  printf '%s\n%s\n%s\n%s\n%s\n' \
    "* Canoodling… (2m 39s · ↓ 10.7k tokens)" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/busy_meter"

  printf '%s\n%s\n%s\n%s\n%s\n' \
    "· Determining… (2s · thinking)" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/busy_early"

  {
    printf '%s\n' "✶ Hatching… (3m 1s · ↓ 73.2k tokens)"
    local n
    for n in 1 2 3 4 5 6; do
      printf '  ◯ general-purpose  Review part %d                    1m 2s · ↓ 12.0k tokens\n' "$n"
    done
    printf '  ⎿  Done (3 tool uses · 9.1k tokens · 20s)\n'
    printf '  ⎿  Done (3 tool uses · 9.1k tokens · 20s)\n'
    printf '%s\n' "$top"
    printf '%s\n' "$box_empty"
    printf '%s\n' "$bottom"
    printf '%s\n' "$status_insert"
  } >"$WAKE_DIR/frames/busy_subbatch_many"

  {
    printf '%s\n' "$top"
    printf '%s\n' "$box_empty"
    printf '%s\n' "$bottom"
    printf '%s\n' "$status_insert"
    printf '  ◯ general-purpose  Running tests                        4m 31s · ↓ 134.5k tokens\n'
  } >"$WAKE_DIR/frames/busy_subrow"

  {
    printf '%s\n' "✳ Perusing… (1m · ↓ 3k tokens)"
    local n
    for n in 1 2 3 4 5 6 7 8; do
      printf '● line %d\n' "$n"
    done
    printf '%s\n' "$top"
    printf '%s\n' "$box_empty"
    printf '%s\n' "$bottom"
    printf '%s\n' "$status_insert"
  } >"$WAKE_DIR/frames/busy_transcript"

  local box_unsent="${fgreset}❯${nbsp}please also check the logs"
  printf '%s\n%s\n%s\n%s\n' "$top" "$box_unsent" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/unsent"

  printf ' Quick safety check: Is this a project you created or one you trust?\n ❯ No, exit\n   Yes, I trust this folder\n Enter to confirm · Esc to cancel\n' \
    >"$WAKE_DIR/frames/trust_unnumbered"

  printf '%s\n%s\n%s\n%s\n%s\n' \
    "● earlier answer" "$top" "$box_empty" "$bottom" "$status_normal" \
    >"$WAKE_DIR/frames/normal_mode"

  printf 'x1 wrapped garbage\nx2 wrapped garbage\nx3 wrapped garbage\n' \
    >"$WAKE_DIR/frames/garbage"

  {
    printf "  ⎿  You've hit your session limit · resets 7pm\n"
    printf '     /upgrade to increase your usage limit.\n'
    printf '%s\n' "$top"
    printf '❯%s\n' "$nbsp"
    printf '%s\n' "$bottom"
    printf '  ⚠ /low-priority to continue now at lower priority · uses your weekly limit\n'
    printf '%s\n' "$status_insert"
  } >"$WAKE_DIR/frames/quota_limit"

  printf '%s\n%s\n%s\n%s\n%s\n' \
    "✻ Sautéing…" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/busy_accented"

  printf '%s\n%s\n%s\n%s\n%s\n' \
    "✢ Beboppin'… (12s · ↓ 1.2k tokens)" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/busy_apostrophe"

  printf '%s\n%s\n%s\n%s\n' "$top" "${fgreset}❯${nbsp}${dim}[Pasted text #1 +40 lines]${reset0}" \
    "$bottom" "$status_insert" >"$WAKE_DIR/frames/dim_placeholder"

  printf '%s\n%s\n%s\n%s\n' "$top" "${fgreset}❯${nbsp}hello ${dim}world${reset0}" \
    "$bottom" "$status_insert" >"$WAKE_DIR/frames/dim_after_typed"

  # An earlier wake's echo still inside the echo window over an idle box.
  local echo_line="❯ $WAKE_PROMPT"
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "$echo_line" "● ok, reading" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/stale_echo_idle"
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "$echo_line" "● ok, reading" "$top" "$box_typed" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/stale_echo_typed"
  printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' \
    "$echo_line" "● ok, reading" "$echo_line" "· Smooshing…" \
    "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/stale_echo_submitted"

  # An earlier wake's echo far above the window before typing, then collapsed
  # into it over an empty box with no spinner (an Enter that started nothing).
  local far_pad="● a" n2
  for n2 in 2 3 4 5 6 7 8; do far_pad+=$'\n'"● line $n2"; done
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "$echo_line" "$far_pad" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/far_echo_idle"
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "$echo_line" "$far_pad" "$top" "$box_typed" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/far_echo_typed"
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "$echo_line" "● ok, reading" "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/near_echo_idle"

  # A live turn whose tool lines push the echo far above its spinner.
  {
    printf '%s\n' "$echo_line"
    for n2 in 1 2 3 4 5 6 7; do printf '● Read(file%d)\n' "$n2"; done
    printf '%s\n' "✻ Reading… (5s · ↓ 1.0k tokens)"
    printf '%s\n%s\n%s\n%s\n' "$top" "$box_empty" "$bottom" "$status_insert"
  } >"$WAKE_DIR/frames/submitted_tools"

  # _wake_above_box <name> <line> — <line> directly above an empty box.
  _wake_above_box() {
    printf '%s\n%s\n%s\n%s\n%s\n' "$2" "$top" "$box_empty" "$bottom" "$status_insert" \
      >"$WAKE_DIR/frames/$1"
  }
  _wake_above_box busy_multiword "✻ Reticulating splines… (12s · ↓ 1.2k tokens)"
  _wake_above_box idle_path "  /very/long/path… (truncated)"
  _wake_above_box idle_etc "- etc… more"
  _wake_above_box idle_bullet "● Checking… done"
  _wake_above_box idle_hebrew "שלום עולם… טקסט"
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' \
    "  Reticulating splines… (12s · ↓ 1.2k tokens)" "  Tip: press ctrl+o to expand" \
    "$top" "$box_empty" "$bottom" "$status_insert" \
    >"$WAKE_DIR/frames/busy_meter_only"
  printf '%s\n%s\n%s\n%s\n' "$top" "${fgreset}❯${nbsp}${dim}go ${reset0}${dim}ahead${reset0}" \
    "$bottom" "$status_insert" >"$WAKE_DIR/frames/idle_ghost_split"

  cp "$WAKE_DIR/frames/idle" "$WAKE_DIR/frames/collide"
  printf '%s\n%s\n%s\n%s\n' "$top" "${fgreset}❯${nbsp}please also check the logs$WAKE_PROMPT" \
    "$bottom" "$status_insert" >"$WAKE_DIR/frames/collided"
}

# _wake_frames_only — just the frame fixtures, for `wake-class` unit tests
# that read a frame from stdin and need neither a worktree nor a tmux stub.
_wake_frames_only() {
  WAKE_DIR="$BATS_TEST_TMPDIR/wake"
  mkdir -p "$WAKE_DIR/frames"
  _wake_write_frames
}

# _wake_install_tmux_stub — a stateful tmux on PATH. Every call is logged to
# $WAKE_DIR/calls.log. It must not depend on any bats function (a separate
# process), so the bus append below is inlined rather than reusing seed_raw.
_wake_install_tmux_stub() {
  cat >"$WAKE_DIR/tmux" <<'WAKE_TMUX_EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$WAKE_DIR/calls.log"
sid="${WAKE_SID:-s1-1}"
case "$1" in
list-windows) cat "$WAKE_DIR/wins.txt" ;;
list-panes) cat "$WAKE_DIR/panes.txt" ;;
show-options)
  pane=""
  prev=""
  for a in "$@"; do
    [ "$prev" = "-t" ] && pane="$a"
    prev="$a"
  done
  [ -f "$WAKE_DIR/crew_state.$pane" ] && cat "$WAKE_DIR/crew_state.$pane"
  ;;
display-message) : ;;
capture-pane)
  n=$(($(cat "$WAKE_DIR/captures") + 1))
  printf '%s' "$n" >"$WAKE_DIR/captures"
  if [ -n "${WAKE_CONSUME_AFTER:-}" ] && [ "$n" -ge "$WAKE_CONSUME_AFTER" ] && [ ! -f "$WAKE_DIR/consumed" ]; then
    touch "$WAKE_DIR/consumed"
    common=$(git -C "$WAKE_REPO" rev-parse --path-format=absolute --git-common-dir)
    jq -nc --arg f "worker:feat/x#$sid" \
      '{ts:(now*1000|floor), crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status", body:{state:"working"}}' \
      >>"$common/crew/events.jsonl"
  fi
  st=$(cat "$WAKE_DIR/state")
  cat "$WAKE_DIR/frames/$st"
  ;;
send-keys)
  st=$(cat "$WAKE_DIR/state")
  lit=0
  enter=0
  for a in "$@"; do
    [ "$a" = "-l" ] && lit=1
    [ "$a" = "Enter" ] && enter=1
  done
  if [ "$lit" = 1 ]; then
    prev=""
    for a in "$@"; do
      [ "$prev" = "-l" ] && printf 'payload=%s\n' "$a" >>"$WAKE_DIR/calls.log"
      prev="$a"
    done
    case "$st" in
    *typed | *submitted) printf 'double-typed\n' >>"$WAKE_DIR/calls.log" ;;
    esac
    if [ -n "${WAKE_TYPE_NEXT:-}" ]; then
      printf '%s' "$WAKE_TYPE_NEXT" >"$WAKE_DIR/state"
    elif [ -z "${WAKE_TYPE_NOOP:-}" ]; then
      case "$st" in
      idle | idle_ghost | submitted | busy_transcript) printf 'typed' >"$WAKE_DIR/state" ;;
      stale_echo_idle) printf 'stale_echo_typed' >"$WAKE_DIR/state" ;;
      collide) printf 'collided' >"$WAKE_DIR/state" ;;
      esac
    fi
  elif [ "$enter" = 1 ] && [ -n "${WAKE_ENTER_NEXT:-}" ]; then
    # WAKE_ENTER_NEXT: Enter paints that frame; WAKE_ENTER_ROW=1 also has the
    # worker post a `working` row.
    printf '%s' "$WAKE_ENTER_NEXT" >"$WAKE_DIR/state"
    if [ "${WAKE_ENTER_ROW:-}" = 1 ]; then
      common=$(git -C "$WAKE_REPO" rev-parse --path-format=absolute --git-common-dir)
      jq -nc --arg f "worker:feat/x#$sid" \
        '{ts:(now*1000|floor), crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status", body:{state:"working"}}' \
        >>"$common/crew/events.jsonl"
    fi
  elif [ "$enter" = 1 ] && [ -z "${WAKE_ENTER_NOOP:-}" ]; then
    next=""
    case "$st" in
    typed) next=submitted ;;
    stale_echo_typed)
      # WAKE_ENTER_CLEARS: the box empties with no new echo (an Esc/clear).
      if [ -n "${WAKE_ENTER_CLEARS:-}" ]; then
        printf 'stale_echo_idle' >"$WAKE_DIR/state"
      else
        next=stale_echo_submitted
      fi
      ;;
    esac
    if [ -n "$next" ]; then
      printf '%s' "$next" >"$WAKE_DIR/state"
      common=$(git -C "$WAKE_REPO" rev-parse --path-format=absolute --git-common-dir)
      jq -nc --arg f "worker:feat/x#$sid" \
        '{ts:(now*1000|floor), crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status", body:{state:"working"}}' \
        >>"$common/crew/events.jsonl"
    fi
  fi
  ;;
esac
exit 0
WAKE_TMUX_EOF
  chmod +x "$WAKE_DIR/tmux"
}

# _wake_stub_base <state> — worktree, wins/panes fixtures, frames and the
# tmux stub, with no bus seeding (callers seed dispatch/status rows
# themselves). The worktree is real: only `git worktree list` can resolve
# `_wake_pane`'s branch -> path lookup.
_wake_stub_base() {
  local state="$1"
  WAKE_DIR="$BATS_TEST_TMPDIR/wake"
  mkdir -p "$WAKE_DIR/frames"
  : >"$WAKE_DIR/calls.log"
  WAKE_REPO="$TEST_REPO"

  git commit -q --allow-empty -m init
  git worktree add -q -b feat/x "$BATS_TEST_TMPDIR/wt-x"
  local wtpath
  wtpath="$(cd "$BATS_TEST_TMPDIR/wt-x" && pwd -P)"

  printf '@1\tsage\t%s\n' "$wtpath" >"$WAKE_DIR/wins.txt"
  printf '@1\t%%7\tclaude\n' >"$WAKE_DIR/panes.txt"

  _wake_write_frames
  printf '%s' "$state" >"$WAKE_DIR/state"
  printf '0' >"$WAKE_DIR/captures"
  _wake_install_tmux_stub

  export WAKE_DIR WAKE_REPO
  export PATH="$WAKE_DIR:$PATH"
}

# _wake_seed_dispatch/_wake_seed_resume <engine> <sid> [ts_ms] — a
# dispatch/resume row on feat/x, shaped like dispatch.sh's / dispatch-resume.sh's
# (with the `engine` field nudge's engine resolution reads).
_wake_seed_dispatch() {
  local engine="$1" sid="$2" ts="${3:-}" logf
  [ -n "$ts" ] || ts=$(($(date +%s) * 1000 - 5000))
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --argjson ts "$ts" --arg s "$sid" --arg e "$engine" \
    '{ts:$ts, crew_id:"c1", kind:"dispatch", branch:"feat/x", session:$s, engine:$e}' \
    >>"$logf"
}

_wake_seed_resume() {
  local engine="$1" sid="$2" ts="$3" logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --argjson ts "$ts" --arg s "$sid" --arg e "$engine" \
    '{ts:$ts, crew_id:"c1", kind:"resume", branch:"feat/x", session:$s, engine:$e}' \
    >>"$logf"
}

# wake_tmux_setup <state> — the common case: one claude session (s1-1, or
# $WAKE_SID) dispatched and blocked, pane %7 showing <state>.
wake_tmux_setup() {
  local state="$1"
  _wake_stub_base "$state"
  local sid="${WAKE_SID:-s1-1}"
  _wake_seed_dispatch claude "$sid"
  seed_raw "worker:feat/x#$sid" blocked "need a decision" ""
}

# _wake_calls <pattern> — how many calls.log lines match (0 when none).
_wake_calls() { grep -c -- "$1" "$WAKE_DIR/calls.log" || true; }

# _wake_fake_clock — a fake clock on PATH: `date +%s` reads the counter in
# $WAKE_CLOCK (seeded with the real time) and `sleep N` advances it by N,
# rounded up, without sleeping; every other `date` form runs the real one.
# WAKE_UNLOCK_AT/WAKE_UNLOCK_DIR: the first sleep reaching that fake second
# removes the lock dir once, standing in for a holder that finishes.
_wake_fake_clock() {
  mkdir -p "$WAKE_DIR/clock"
  WAKE_REAL_DATE=$(command -v date)
  WAKE_CLOCK="$WAKE_DIR/clock/now"
  "$WAKE_REAL_DATE" +%s >"$WAKE_CLOCK"
  cat >"$WAKE_DIR/clock/date" <<'WAKE_DATE_EOF'
#!/usr/bin/env bash
if [ "$#" = 1 ] && [ "$1" = +%s ]; then
  cat "$WAKE_CLOCK"
  exit 0
fi
exec "$WAKE_REAL_DATE" "$@"
WAKE_DATE_EOF
  cat >"$WAKE_DIR/clock/sleep" <<'WAKE_SLEEP_EOF'
#!/usr/bin/env bash
s=${1:-0}
whole=${s%%.*}
[ -n "$whole" ] || whole=0
case "$s" in
*.*[1-9]*) whole=$((whole + 1)) ;;
esac
now=$(($(cat "$WAKE_CLOCK") + whole))
printf '%s\n' "$now" >"$WAKE_CLOCK"
if [ -n "${WAKE_UNLOCK_AT:-}" ] && [ "$now" -ge "$WAKE_UNLOCK_AT" ] && [ ! -f "$WAKE_DIR/unlocked" ]; then
  touch "$WAKE_DIR/unlocked"
  rm -rf "$WAKE_UNLOCK_DIR"
fi
WAKE_SLEEP_EOF
  chmod +x "$WAKE_DIR/clock/date" "$WAKE_DIR/clock/sleep"
  export WAKE_REAL_DATE WAKE_CLOCK
  export PATH="$WAKE_DIR/clock:$PATH"
}

# _wake_now — the fake clock's current second.
_wake_now() { cat "$WAKE_CLOCK"; }

# _wake_hold_lock — a live holder ($$, the bats process) on s1-1's wake lock.
_wake_hold_lock() {
  WAKE_LOCK_DIR="$(git rev-parse --path-format=absolute --git-common-dir)/crew/wake/s1-1"
  mkdir -p "$WAKE_LOCK_DIR"
  printf '%s' "$$" >"$WAKE_LOCK_DIR/pid"
}

# _wake_seed_resumed — s1-1 dispatched, blocked, then working again, all in the
# past, so no default `--since` (the invocation time) can see the resume.
_wake_seed_resumed() {
  local t
  t=$(($(date +%s) * 1000))
  _wake_seed_dispatch claude s1-1 "$((t - 5000))"
  seed_raw "worker:feat/x#s1-1" blocked "need a decision" "" "$((t - 4000))"
  seed_raw "worker:feat/x#s1-1" working "" "" "$((t - 3000))"
}

# _wake_assert_typed_once [pane] — exactly one literal send, carrying exactly
# the wake prompt, never typed over already-typed text.
_wake_assert_typed_once() {
  [ "$(_wake_calls ' -l ')" -eq 1 ]
  grep -qF -- "send-keys -t ${1:-%7} -l $WAKE_PROMPT" "$WAKE_DIR/calls.log"
  [ "$(_wake_calls '^payload=')" -eq 1 ]
  [ "$(grep '^payload=' "$WAKE_DIR/calls.log")" = "payload=$WAKE_PROMPT" ]
  [ "$(_wake_calls '^double-typed')" -eq 0 ]
}

# _wake_class_locales <frame> <class> — the frame classifies <class> under the
# default locale, LC_ALL=C, and LC_ALL=C.UTF-8 when the system has it.
_wake_class_locales() {
  local f="$WAKE_DIR/frames/$1"
  [ "$(run_crew wake-class <"$f")" = "$2" ]
  [ "$(LC_ALL=C bash -euo pipefail "$CREW" wake-class <"$f")" = "$2" ]
  if locale -a 2>/dev/null | grep -qixE 'c\.utf-?8'; then
    [ "$(LC_ALL=C.UTF-8 bash -euo pipefail "$CREW" wake-class <"$f")" = "$2" ]
  fi
}

# ---- wake-class: one word per pinned frame -------------------------------

@test "wake-class: idle -> idle" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/idle"
  [ "$status" -eq 0 ]
  [ "$output" = idle ]
}

@test "wake-class: idle_ghost -> idle" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/idle_ghost"
  [ "$status" -eq 0 ]
  [ "$output" = idle ]
}

@test "wake-class: ghost_unterminated -> unsent" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/ghost_unterminated"
  [ "$status" -eq 0 ]
  [ "$output" = unsent ]
}

@test "wake-class: typed -> unsent" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/typed"
  [ "$status" -eq 0 ]
  [ "$output" = unsent ]
}

@test "wake-class: submitted -> busy" {
  # A spinner right above the top rule marks a started turn.
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/submitted"
  [ "$status" -eq 0 ]
  [ "$output" = busy ]
}

@test "wake-class: busy_meter -> busy" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/busy_meter"
  [ "$status" -eq 0 ]
  [ "$output" = busy ]
}

@test "wake-class: busy_early -> busy" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/busy_early"
  [ "$status" -eq 0 ]
  [ "$output" = busy ]
}

@test "wake-class: busy_subbatch_many -> busy" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/busy_subbatch_many"
  [ "$status" -eq 0 ]
  [ "$output" = busy ]
}

@test "wake-class: busy_subrow -> busy" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/busy_subrow"
  [ "$status" -eq 0 ]
  [ "$output" = busy ]
}

@test "wake-class: busy_transcript -> idle" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/busy_transcript"
  [ "$status" -eq 0 ]
  [ "$output" = idle ]
}

@test "wake-class: unsent -> unsent" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/unsent"
  [ "$status" -eq 0 ]
  [ "$output" = unsent ]
}

@test "wake-class: trust_unnumbered -> prompt" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/trust_unnumbered"
  [ "$status" -eq 0 ]
  [ "$output" = prompt ]
}

@test "wake-class: normal_mode -> vimmode" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/normal_mode"
  [ "$status" -eq 0 ]
  [ "$output" = vimmode ]
}

@test "wake-class: garbage -> unknown" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/garbage"
  [ "$status" -eq 0 ]
  [ "$output" = unknown ]
}

@test "wake-class: quota_limit -> quota" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/quota_limit"
  [ "$status" -eq 0 ]
  [ "$output" = quota ]
}

@test "wake-class: busy_accented -> busy in every locale" {
  _wake_frames_only
  _wake_class_locales busy_accented busy
}

@test "wake-class: busy_apostrophe -> busy in every locale" {
  _wake_frames_only
  _wake_class_locales busy_apostrophe busy
}

@test "wake-class: every pinned fixture classifies the same under LC_ALL=C" {
  _wake_frames_only
  local pair
  for pair in idle=idle idle_ghost=idle ghost_unterminated=unsent typed=unsent \
    submitted=busy busy_meter=busy busy_early=busy busy_subbatch_many=busy \
    busy_subrow=busy busy_transcript=idle unsent=unsent trust_unnumbered=prompt \
    normal_mode=vimmode garbage=unknown quota_limit=quota \
    dim_placeholder=unsent dim_after_typed=unsent stale_echo_idle=idle; do
    [ "$(run_crew wake-class <"$WAKE_DIR/frames/${pair%=*}")" = "${pair#*=}" ]
    [ "$(LC_ALL=C bash -euo pipefail "$CREW" wake-class <"$WAKE_DIR/frames/${pair%=*}")" = "${pair#*=}" ]
  done
}

@test "wake-class: dim_placeholder -> unsent" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/dim_placeholder"
  [ "$status" -eq 0 ]
  [ "$output" = unsent ]
}

@test "wake-class: dim_after_typed -> unsent" {
  _wake_frames_only
  run run_crew wake-class <"$WAKE_DIR/frames/dim_after_typed"
  [ "$status" -eq 0 ]
  [ "$output" = unsent ]
}

# ---- wake: reply wakes a blocked worker -----------------------------------

@test "wake: RED on main — reply to a blocked session whose await ended wakes an idle pane" {
  wake_tmux_setup idle
  CREW_ID=c1 run run_crew reply "worker:feat/x" "decision" --wake-timeout 60
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  msg_ts=$(jq -r 'select(.kind=="msg" and .body=="decision") | .ts' "$log" | tail -1)
  _wake_assert_typed_once
  run jq -s -r --argjson t "$msg_ts" \
    '[.[] | select(.kind=="status" and .from=="worker:feat/x#s1-1" and .body.state=="working" and .ts>=$t)] | length' \
    "$log"
  [ "$output" -ge 1 ]
  ! grep -qF "decision" "$WAKE_DIR/calls.log"
}

@test "wake: reply --wake-timeout under the typing floor is a usage error before any append" {
  wake_tmux_setup idle
  CREW_ID=c1 run run_crew reply "worker:feat/x" "decision" --wake-timeout 20
  [ "$status" -eq 1 ]
  [[ "$output" == *"--wake-timeout must be >= 38s (the typing floor)"* ]]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  [ "$(jq -s '[.[] | select(.kind == "msg")] | length' "$log")" -eq 0 ]
  [ ! -s "$WAKE_DIR/calls.log" ]
}

@test "wake: reply to a working session makes no tmux call" {
  wake_tmux_setup idle
  seed_raw "worker:feat/x#s1-1" working "" ""
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 0 ]
  [ ! -s "$WAKE_DIR/calls.log" ]
}

@test "wake: --no-wake appends without touching tmux" {
  wake_tmux_setup idle
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go" --no-wake
  [ "$status" -eq 0 ]
  [ ! -s "$WAKE_DIR/calls.log" ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .body' "$log"
  [[ "$output" == *"go"* ]]
}

@test "wake: reply to a codex-engine blocked session fails the wake but the message lands" {
  _wake_stub_base idle
  _wake_seed_dispatch codex s1-1
  seed_raw "worker:feat/x#s1-1" blocked "need a decision" ""
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 5 ]
  [[ "$output" == *"do not resend"* ]]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .body' "$log"
  [[ "$output" == *"go"* ]]
}

@test "wake: reply to an explicit unknown session id appends verbatim without waking" {
  wake_tmux_setup idle
  CREW_ID=c1 run run_crew reply "worker:feat/x#s9-9" "x"
  [ "$status" -eq 0 ]
  [ ! -s "$WAKE_DIR/calls.log" ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .to' "$log"
  [ "$output" = "worker:feat/x#s9-9" ]
}

@test "wake: reply to a quota-parked session still appends but fails the wake" {
  wake_tmux_setup idle
  seed_raw "worker:feat/x#s1-1" blocked "quota: session limit — do not re-dispatch" watchdog
  CREW_ID=c1 run run_crew reply "worker:feat/x" "go"
  [ "$status" -eq 5 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | .body' "$log"
  [[ "$output" == *"go"* ]]
}

# ---- nudge: transient / permanent / unverified outcomes -------------------

# The busy/unknown deadline tests run on the fake clock at the floor timeout, so
# they poll the whole budget instantly; a frame misread as idle would deliver
# and fail the refusal assertions.

@test "nudge: busy_meter keeps polling and refuses transient at the deadline" {
  wake_tmux_setup busy_meter
  _wake_fake_clock
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 37 --interval 1
  [ "$status" -eq 3 ]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: busy_early keeps polling and refuses transient at the deadline" {
  wake_tmux_setup busy_early
  _wake_fake_clock
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 37 --interval 1
  [ "$status" -eq 3 ]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: busy_subrow — a subagent row under the status bar is busy" {
  wake_tmux_setup busy_subrow
  _wake_fake_clock
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 37 --interval 1
  [ "$status" -eq 3 ]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: unsent refuses transient immediately with a single capture" {
  wake_tmux_setup unsent
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 60 --interval 0
  [ "$status" -eq 3 ]
  [ "$(grep -c '^capture-pane' "$WAKE_DIR/calls.log")" -eq 1 ]
  ! grep -q '^send-keys' "$WAKE_DIR/calls.log"
}

@test "nudge: an un-numbered trust prompt refuses permanently" {
  wake_tmux_setup trust_unnumbered
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 5 ]
}

@test "nudge: vim normal mode refuses transient" {
  wake_tmux_setup normal_mode
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 3 ]
}

@test "nudge: an unrecognized frame that never resolves is unknown-frame at the deadline" {
  wake_tmux_setup garbage
  _wake_fake_clock
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 37 --interval 1
  [ "$status" -eq 5 ]
  [[ "$output" == *"unknown-frame"* ]]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: idle_ghost delivers — literal keys then Enter" {
  wake_tmux_setup idle_ghost
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  lit_line=$(grep -n -- '-l' "$WAKE_DIR/calls.log" | head -1 | cut -d: -f1)
  enter_line=$(grep -n 'Enter' "$WAKE_DIR/calls.log" | head -1 | cut -d: -f1)
  [ -n "$lit_line" ]
  [ -n "$enter_line" ]
  [ "$lit_line" -lt "$enter_line" ]
  _wake_assert_typed_once
}

@test "nudge: text that collides with the typed wake refuses unverified without Enter" {
  wake_tmux_setup collide
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 4 ]
  [[ "$output" == *"input collided"* ]]
  [ "$(_wake_calls 'Enter')" -eq 0 ]
}

@test "nudge: typed text that never renders refuses unverified without Enter" {
  wake_tmux_setup idle
  WAKE_TYPE_NOOP=1
  export WAKE_TYPE_NOOP
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 4 ]
  [[ "$output" == *"typed text not visible"* ]]
  [ "$(_wake_calls 'Enter')" -eq 0 ]
}

@test "nudge: an earlier wake's echo does not verify an Enter that submitted nothing" {
  wake_tmux_setup stale_echo_idle
  WAKE_ENTER_CLEARS=1
  export WAKE_ENTER_CLEARS
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 4 ]
  [[ "$output" != *"delivered"* ]]
}

@test "nudge: a new echo under an earlier wake's echo delivers" {
  wake_tmux_setup stale_echo_idle
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: an Enter that never lands retries once then refuses unverified" {
  wake_tmux_setup idle
  WAKE_ENTER_NOOP=1
  export WAKE_ENTER_NOOP
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 4 ]
  [ "$(grep -c 'Enter' "$WAKE_DIR/calls.log")" -eq 2 ]
}

@test "nudge: a worker that starts working mid-poll is consumed, not woken" {
  wake_tmux_setup busy_meter
  WAKE_CONSUME_AFTER=2
  export WAKE_CONSUME_AFTER
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 60 --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"consumed"* ]]
  ! grep -q '^send-keys' "$WAKE_DIR/calls.log"
}

@test "nudge: a non-claude engine refuses permanently before any capture" {
  _wake_stub_base idle
  _wake_seed_dispatch codex s1-1
  seed_raw "worker:feat/x#s1-1" blocked "need a decision" ""
  CREW_ID=c1 run run_crew nudge "worker:feat/x"
  [ "$status" -eq 5 ]
  [[ "$output" == *"engine codex"* ]]
  ! grep -q '^capture-pane' "$WAKE_DIR/calls.log"
}

@test "nudge: a live lock holder is waited out, then the wake delivers" {
  wake_tmux_setup idle
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  mkdir -p "$common/crew/wake/s1-1"
  printf '%s' "$$" >"$common/crew/wake/s1-1/pid"
  (
    sleep 1
    rm -rf "$common/crew/wake/s1-1"
  ) >/dev/null 2>&1 3>&- &
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: a live lock holder past the deadline refuses transient lock" {
  wake_tmux_setup idle
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  mkdir -p "$common/crew/wake/s1-1"
  printf '%s' "$$" >"$common/crew/wake/s1-1/pid"
  _wake_fake_clock
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 37 --interval 0
  [ "$status" -eq 3 ]
  [[ "$output" == *"refused (transient): lock"* ]]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: a dead lock holder is reclaimed and delivers" {
  wake_tmux_setup idle
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  mkdir -p "$common/crew/wake/s1-1"
  (sleep 0) &
  dead_pid=$!
  wait "$dead_pid"
  printf '%s' "$dead_pid" >"$common/crew/wake/s1-1/pid"
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: a missing wake dir still delivers" {
  wake_tmux_setup idle
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  [ ! -d "$common/crew/wake" ]
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: --timeout under the 37s typing floor is a usage error" {
  wake_tmux_setup idle
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 20 --interval 0
  [ "$status" -eq 1 ]
  [[ "$output" == *"crew: nudge: --timeout must be >= 37s (the typing floor)"* ]]
  [ "$(_wake_calls '^capture-pane')" -eq 0 ]
}

@test "nudge: an explicit unknown session id refuses as a usage error" {
  wake_tmux_setup idle
  CREW_ID=c1 run run_crew nudge "worker:feat/x#s9-9"
  [ "$status" -eq 1 ]
}

@test "nudge: a second marked pane in the window is skipped for the unmarked lead" {
  wake_tmux_setup idle
  printf '@1\t%%7\tclaude\n@1\t%%8\tclaude\n' >"$WAKE_DIR/panes.txt"
  printf 'idle' >"$WAKE_DIR/crew_state.%8"
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  grep -qE '^(capture-pane|send-keys).* -t %7( |$)' "$WAKE_DIR/calls.log"
  [ "$(grep -cE '^(capture-pane|send-keys).* -t %8( |$)' "$WAKE_DIR/calls.log")" -eq 0 ]
  _wake_assert_typed_once %7
}

@test "nudge: two unmarked engine panes are ambiguous" {
  wake_tmux_setup idle
  printf '@1\t%%7\tclaude\n@1\t%%8\tclaude\n' >"$WAKE_DIR/panes.txt"
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 5 ]
  [[ "$output" == *"ambiguous"* ]]
}

@test "nudge: a watchdog 'prompt: cleared' row after since is not treated as consumed" {
  wake_tmux_setup idle
  since_ts=$(($(date +%s) * 1000))
  seed_raw "worker:feat/x#s1-1" working "prompt: cleared" watchdog "$((since_ts + 1000))"
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --since "$since_ts" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: a watchdog quota-parked status refuses permanently without a capture" {
  wake_tmux_setup idle
  seed_raw "worker:feat/x#s1-1" blocked "quota: session limit — do not re-dispatch" watchdog
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 5 ]
  [[ "$output" == *"quota"* ]]
  ! grep -q '^capture-pane' "$WAKE_DIR/calls.log"
}

@test "nudge: busy_transcript — a meter line beyond the window still classifies idle" {
  wake_tmux_setup busy_transcript
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: busy_subbatch_many — a live subagent batch above the box is busy" {
  wake_tmux_setup busy_subbatch_many
  _wake_fake_clock
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 37 --interval 1
  [ "$status" -eq 3 ]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: a resume row's engine supersedes an earlier dispatch's" {
  _wake_stub_base idle
  t=$(($(date +%s) * 1000 - 10000))
  _wake_seed_dispatch codex s1-1 "$t"
  _wake_seed_resume claude s2-2 "$((t + 1000))"
  seed_raw "worker:feat/x#s2-2" blocked "need a decision" "" "$((t + 2000))"
  WAKE_SID=s2-2
  export WAKE_SID
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: a dispatch row's engine wins when no later resume claims the session" {
  _wake_stub_base idle
  t=$(($(date +%s) * 1000 - 10000))
  _wake_seed_dispatch claude s1-1 "$t"
  _wake_seed_resume codex s2-2 "$((t + 1000))"
  seed_raw "worker:feat/x#s2-2" blocked "need a decision" "" "$((t + 2000))"
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 5 ]
  [[ "$output" == *"engine codex"* ]]
}

# ---- wake: submit verify, typing budget, consumed anchor, busy detection ----

@test "nudge: an old echo collapsed into the window with no spinner is unverified" {
  wake_tmux_setup far_echo_idle
  _wake_fake_clock
  WAKE_TYPE_NEXT=far_echo_typed WAKE_ENTER_NEXT=near_echo_idle
  export WAKE_TYPE_NEXT WAKE_ENTER_NEXT
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 1
  [ "$status" -eq 4 ]
  [[ "$output" == *"submit not verified"* ]]
  [ "$(_wake_calls 'Enter')" -eq 1 ]
}

@test "nudge: a short pane showing only the new echo over its spinner delivers" {
  wake_tmux_setup stale_echo_idle
  _wake_fake_clock
  WAKE_ENTER_NEXT=submitted
  export WAKE_ENTER_NEXT
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: a spinner under more than six tool lines verifies by the worker's working row" {
  wake_tmux_setup idle
  _wake_fake_clock
  WAKE_ENTER_NEXT=submitted_tools WAKE_ENTER_ROW=1
  export WAKE_ENTER_NEXT WAKE_ENTER_ROW
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 1
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "nudge: --timeout 38 --interval 2 on an idle pane delivers" {
  wake_tmux_setup idle
  _wake_fake_clock
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 38 --interval 2
  [ "$status" -eq 0 ]
  [[ "$output" == *"delivered"* ]]
  _wake_assert_typed_once
}

@test "wake: reply --wake-timeout 36 is under the 38s floor — usage error before any append" {
  wake_tmux_setup idle
  CREW_ID=c1 run run_crew reply "worker:feat/x" "decision" --wake-timeout 36
  [ "$status" -eq 1 ]
  [[ "$output" == *"--wake-timeout must be >= 38s (the typing floor)"* ]]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  [ "$(jq -s '[.[] | select(.kind == "msg")] | length' "$log")" -eq 0 ]
  [ ! -s "$WAKE_DIR/calls.log" ]
}

@test "nudge: CREW_WAKE_FLOOR in the environment cannot lower the typing floor" {
  wake_tmux_setup idle
  CREW_ID=c1 CREW_WAKE_FLOOR=0 run run_crew nudge "worker:feat/x" --timeout 2 --interval 0
  [ "$status" -eq 1 ]
  [[ "$output" == *"--timeout must be >= 37s (the typing floor)"* ]]
  [ "$(_wake_calls '^capture-pane')" -eq 0 ]
}

@test "nudge: a lock wait that runs out the typing budget refuses as lock, not busy" {
  wake_tmux_setup idle
  _wake_fake_clock
  _wake_hold_lock
  WAKE_UNLOCK_AT=$(($(_wake_now) + 30)) WAKE_UNLOCK_DIR=$WAKE_LOCK_DIR
  export WAKE_UNLOCK_AT WAKE_UNLOCK_DIR
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 60 --interval 1
  [ "$status" -eq 3 ]
  [[ "$output" == *"refused (transient): lock"* ]]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: an Enter that never lands at the floor timeout ends within the timeout" {
  wake_tmux_setup idle
  _wake_fake_clock
  WAKE_ENTER_NOOP=1
  export WAKE_ENTER_NOOP
  start=$(_wake_now)
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 38 --interval 2
  [ "$status" -eq 4 ]
  [ "$(_wake_calls 'Enter')" -eq 2 ]
  [ $(($(_wake_now) - start)) -le 39 ]
}

@test "nudge: a worker that resumed before the nudge is consumed without typing" {
  _wake_stub_base busy_meter
  _wake_fake_clock
  _wake_seed_resumed
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 38 --interval 2
  [ "$status" -eq 0 ]
  [[ "$output" == *"consumed"* ]]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: a resumed worker behind a live lock is consumed without typing" {
  _wake_stub_base busy_meter
  _wake_fake_clock
  _wake_seed_resumed
  _wake_hold_lock
  WAKE_UNLOCK_AT=$(($(_wake_now) + 1)) WAKE_UNLOCK_DIR=$WAKE_LOCK_DIR
  export WAKE_UNLOCK_AT WAKE_UNLOCK_DIR
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --timeout 38 --interval 2
  [ "$status" -eq 0 ]
  [[ "$output" == *"consumed"* ]]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "nudge: a session-less watchdog blocked row after working re-anchors — a trust frame refuses permanently" {
  _wake_stub_base trust_unnumbered
  _wake_seed_resumed
  seed_raw "worker:feat/x" blocked "prompt: trust" watchdog "$(($(date +%s) * 1000 - 2000))"
  CREW_ID=c1 run run_crew nudge "worker:feat/x" --interval 0
  [ "$status" -eq 5 ]
  [[ "$output" == *"prompt"* ]]
  [ "$(_wake_calls '^send-keys')" -eq 0 ]
}

@test "wake-class: a multi-word spinner verb is busy in every locale" {
  _wake_frames_only
  _wake_class_locales busy_multiword busy
}

@test "wake-class: a path ellipsis directly above the box is idle in every locale" {
  _wake_frames_only
  _wake_class_locales idle_path idle
}

@test "wake-class: a '- etc…' line directly above the box is idle in every locale" {
  _wake_frames_only
  _wake_class_locales idle_etc idle
}

@test "wake-class: an assistant bullet with an ellipsis above the box is idle in every locale" {
  _wake_frames_only
  _wake_class_locales idle_bullet idle
}

@test "wake-class: Hebrew text with an ellipsis above the box is idle in every locale" {
  _wake_frames_only
  _wake_class_locales idle_hebrew idle
}

@test "wake-class: a meter with a non-spinner line nearest the box is busy in every locale" {
  _wake_frames_only
  _wake_class_locales busy_meter_only busy
}

@test "wake-class: a ghost split into two adjacent dim spans is idle in every locale" {
  _wake_frames_only
  _wake_class_locales idle_ghost_split idle
}
