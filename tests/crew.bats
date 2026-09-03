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
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 4
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
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 3
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
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 999 --max-life 4
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 1 ]
  [[ "${lines[0]}" == "blocked|quiet: pane unchanged for "* ]]
}

@test "stall-watch: healthy subagent batch produces ZERO events" {
  # A1's measured false-positive driver, verbatim: meter present, clock rising,
  # parent token string static at 73.2k, live subagent row throughout. D2 is
  # vetoed by the row; D1 is vetoed by the meter; D3 by the byte changes.
  a=$(fx_subbatch "26m 57s" "3m 29s")
  b=$(fx_subbatch "27m 12s" "3m 45s")
  c=$(fx_subbatch "27m 27s" "4m 0s")
  d=$(fx_subbatch "27m 42s" "4m 15s")
  e=$(fx_subbatch "27m 58s" "4m 30s")
  stall_sampler "$a" "$b" "$c" "$d" "$e" "$e"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 6
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
  stall_sampler "$a" "$b" "$c" "$d"
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
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine codex \
    --grace 0 --interval 1 --window 60 --stall 1 --idle 999 --dead 999 --max-life 4
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
  p=$(fx_prompt_trust)
  q=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$q" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 7
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 3 ]
  [[ "${lines[0]}" == blocked\|prompt:* ]]
  [ "${lines[1]}" = "working|prompt: cleared" ]
  [[ "${lines[2]}" == blocked\|prompt:* ]]
}

@test "stall-watch: a quiet: episode escalates to failed after --dead" {
  p=$(fx_idle_box)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 2 --max-life 9
  run bash -c "bus | jq -r 'select(.kind==\"status\") | \"\(.body.state)|\(.body.detail)\"'"
  [ "${#lines[@]}" -eq 2 ]
  [[ "${lines[0]}" == blocked\|quiet:* ]]
  [[ "${lines[1]}" == "failed|dead: quiet: unchanged for "* ]]
}

@test "stall-watch: a clearance before --dead cancels the escalation" {
  p=$(fx_idle_box)
  q=$(fx_meter "5m 29s" "25.0k")
  stall_sampler "$p" "$p" "$p" "$q" "$q" "$q"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 2 --dead 3 --max-life 6
  run bash -c "bus | grep -c '\"state\":\"failed\"' || true"
  [ "$output" = "0" ]
  run bash -c "bus | grep -c 'quiet: cleared' || true"
  [ "$output" = "1" ]
}

@test "stall-watch: a prompt: episode NEVER escalates (C-1)" {
  # An unanswered answerable question is waiting work, not death. Escalating it
  # would reproduce session 3 with a 30-minute delay.
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 8
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

@test "stall-watch: INV-W0 d — writes normalise from, so roster shows ONE row" {
  CREW_ID=c1 run_crew status worker:feat/x working
  p=$(fx_prompt_trust)
  stall_sampler "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch "worker:feat/x#s1786338213-54181" --pane %9 \
    --engine claude --grace 0 --interval 1 --window 0 --idle 999 --dead 999 --max-life 3
  run bash -c "bus | jq -r 'select(.body.source==\"watchdog\") | .from'"
  [ "$output" = "worker:feat/x" ]
  CREW_ID=c1 run run_crew roster c1
  run bash -c "printf '%s' '$output' | jq 'length'"
  [ "$output" = "1" ]
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
  p=$(fx_prompt_quota)
  stall_sampler "$p" "$p" "$p" "$p" "$p" "$p" "$p" "$p"
  CREW_ID=c1 run run_crew stall-watch worker:feat/x --pane %9 --engine claude \
    --grace 0 --interval 1 --window 0 --idle 999 --dead 2 --max-life 8
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
