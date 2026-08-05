setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
}

teardown() {
  teardown_repo
}

# stub_tmux <list-windows-body> <list-panes-body> — a tmux whose list output is
# fixed text. crew's real tmux calls are `|| true`-tolerant, so without this the
# occupancy tests would read the developer's live server and flake.
stub_tmux() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  printf '%s' "$1" >"$STUB_DIR/wins.txt"
  printf '%s' "$2" >"$STUB_DIR/panes.txt"
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

@test "id: mints <epoch>-<pid> when CREW_ID is unset" {
  CREW_ID= run run_crew id
  [ "$status" -eq 0 ]
  [[ "$output" =~ ^[0-9]+-[0-9]+$ ]]
}

@test "id: works outside a git repo" {
  cd /
  CREW_ID= run run_crew id
  [ "$status" -eq 0 ]
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
  [[ "$output" == *"reap takes --quiet and --dry-run"* ]]
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
