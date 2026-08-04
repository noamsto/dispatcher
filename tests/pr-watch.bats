bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  PR_WATCH="$BATS_TEST_DIRNAME/../adapters/core/pr-watch.sh"
  run_pr_watch() { bash -euo pipefail "$PR_WATCH" "$@"; }
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
  # The cursor lives under $XDG_DATA_HOME — keep it out of the developer's own.
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  unset CREW_ID
  stub_gh
}

teardown() {
  teardown_repo
}

# gh stub serving the two calls a poll makes, each from a file the test rewrites
# between runs. $GH_THREADS holds what the real `gh api -q` would print — one
# timestamp per review-thread comment — because gh, not pr-watch, applies -q.
stub_gh() {
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
pr) cat "$GH_VIEW" ;;
api) cat "$GH_THREADS" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  GH_VIEW="$BATS_TEST_TMPDIR/view.json"
  GH_THREADS="$BATS_TEST_TMPDIR/threads.txt"
  export GH_VIEW GH_THREADS
  : >"$GH_THREADS"
  set_view aaa11 OPEN SUCCESS
}

set_view() { # <head-sha> <state> <check-conclusion>
  cat >"$GH_VIEW" <<EOF
{"headRefOid":"$1","state":"$2","reviewDecision":"","latestReviews":[],
 "statusCheckRollup":[{"name":"check","status":"COMPLETED","conclusion":"$3"}],
 "comments":[]}
EOF
}

# Seed the cursor: the first ever poll is a baseline, never an event.
# --separate-stderr: the park's end-of-timeout notice goes to stderr, and empty
# *stdout* is the contract a caller keys off.
seed() {
  run --separate-stderr run_pr_watch 42 --repo o/r --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [[ "$stderr" == *"park ended after 1s"* ]]
}

@test "the first park seeds the cursor and reports nothing" {
  seed
  [ -f "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json" ]
}

@test "fires on a head SHA move" {
  seed
  set_view bbb22 OPEN SUCCESS
  run run_pr_watch 42 --repo o/r --timeout 30 --interval 1
  [ "$status" -eq 0 ]
  echo "$output" | jq -e '.changed == ["head_sha"] and .pr == 42 and .repo == "o/r"'
  echo "$output" | jq -e '.state.head_sha == "bbb22" and .was.head_sha == "aaa11"'
}

@test "fires on a new review-thread reply" {
  seed
  printf '%s\n' '2026-08-04T10:00:00Z' >"$GH_THREADS"
  run run_pr_watch 42 --repo o/r --timeout 30 --interval 1
  [ "$status" -eq 0 ]
  echo "$output" | jq -e '.changed | sort == ["thread_at","thread_n"]'
  echo "$output" | jq -e '.state.thread_n == 1'
}

@test "fires when the check rollup conclusion flips" {
  seed
  set_view aaa11 OPEN FAILURE
  run run_pr_watch 42 --repo o/r --timeout 30 --interval 1
  [ "$status" -eq 0 ]
  echo "$output" | jq -e '.changed == ["checks"] and .state.checks == "FAILURE"'
}

@test "one in-flight check keeps the rollup pending, not successful" {
  cat >"$GH_VIEW" <<'EOF'
{"headRefOid":"aaa11","state":"OPEN","reviewDecision":"","latestReviews":[],
 "statusCheckRollup":[{"name":"a","status":"COMPLETED","conclusion":"SUCCESS"},
                      {"name":"b","status":"IN_PROGRESS","conclusion":""}],
 "comments":[]}
EOF
  seed
  jq -e '.checks == "PENDING"' "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json"
}

@test "fires when the PR merges" {
  seed
  set_view aaa11 MERGED SUCCESS
  run run_pr_watch 42 --repo o/r --timeout 30 --interval 1
  [ "$status" -eq 0 ]
  echo "$output" | jq -e '.changed == ["state"] and .state.state == "MERGED"'
}

@test "an unchanged poll reports nothing and exits 0" {
  seed
  run --separate-stderr run_pr_watch 42 --repo o/r --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "the cursor prevents re-delivery across restarts" {
  seed
  set_view bbb22 OPEN SUCCESS
  run run_pr_watch 42 --repo o/r --timeout 30 --interval 1
  [ "$status" -eq 0 ]
  [ -n "$output" ]
  # Same state, fresh process: the event was already handed to a caller once.
  run --separate-stderr run_pr_watch 42 --repo o/r --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "runs with CREW_ID unset and never touches the bus" {
  seed
  set_view bbb22 OPEN SUCCESS
  run run_pr_watch 42 --repo o/r --timeout 30 --interval 1
  [ "$status" -eq 0 ]
  [ -n "$output" ]
  [ ! -e "$(git rev-parse --path-format=absolute --git-common-dir)/crew" ]
}

@test "works with no git repo at all when --repo is given" {
  cd /
  run run_pr_watch 42 --repo o/r --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -f "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json" ]
}

@test "derives the repo from the origin remote" {
  git remote add origin git@github.com:o/derived.git
  run run_pr_watch 42 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -f "$XDG_DATA_HOME/crew/pr-watch/o/derived/42.json" ]
}

@test "aborts without a PR number" {
  run run_pr_watch --repo o/r
  [ "$status" -eq 1 ]
  [[ "$output" == *"usage: pr-watch"* ]]
}

@test "rejects an unbounded park" {
  run run_pr_watch 42 --repo o/r --timeout 0
  [ "$status" -eq 1 ]
  [[ "$output" == *"must be > 0"* ]]
}

@test "rejects an unknown flag" {
  run run_pr_watch 42 --bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"unknown arg"* ]]
}

@test "a first poll that cannot read the PR fails loudly" {
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
  chmod +x "$STUB_DIR/gh"
  run run_pr_watch 42 --repo o/r --timeout 1 --interval 1
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not read PR 42"* ]]
}

@test "crew pr-watch posts the event to the crew's dispatcher" {
  seed
  set_view bbb22 OPEN SUCCESS
  # crew shells out to `pr-watch` on PATH; point that at the source under test.
  cat >"$STUB_DIR/pr-watch" <<EOF
#!/usr/bin/env bash
exec bash -euo pipefail "$PR_WATCH" "\$@"
EOF
  chmod +x "$STUB_DIR/pr-watch"

  CREW_ID=c1 run run_crew pr-watch 42 --repo o/r --timeout 30 --interval 1
  [ "$status" -eq 0 ]
  [ -n "$output" ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="msg") | "\(.from)|\(.to)|\(.body | fromjson | .changed[0])"' "$log"
  [ "$output" = "pr-watch:42|dispatcher:c1|head_sha" ]
}

@test "crew pr-watch posts nothing when the park times out" {
  seed
  cat >"$STUB_DIR/pr-watch" <<EOF
#!/usr/bin/env bash
exec bash -euo pipefail "$PR_WATCH" "\$@"
EOF
  chmod +x "$STUB_DIR/pr-watch"

  CREW_ID=c1 run --separate-stderr run_crew pr-watch 42 --repo o/r --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [ ! -f "$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl" ]
}
