#!/usr/bin/env bash
set -uo pipefail

readonly CASE_ID=${1:?usage: case-runner.sh CASE_ID}
ROOT=$(git rev-parse --show-toplevel)
readonly ROOT
readonly MANIFEST="$ROOT/tests/harness/manifest.tsv"
readonly ASSERTIONS="$ROOT/tests/harness/assertions.tsv"
readonly CREW="$ROOT/adapters/core/crew.sh"
readonly REFRESH_MODELS="$ROOT/adapters/core/refresh-models.sh"
readonly PR_WATCH="$ROOT/adapters/core/pr-watch.sh"
CASE_TMP=$(mktemp -d)
readonly CASE_TMP
readonly ACTUAL_IDS="$CASE_TMP/assertion-ids"
readonly STDOUT_FILE="$CASE_TMP/stdout"
readonly STDERR_FILE="$CASE_TMP/stderr"
touch "$ACTUAL_IDS"
failures=0

cleanup() {
  XDG_DATA_HOME="$(dirname "$CASE_TMP")/shellspec-trash" gtrash put "$CASE_TMP"
}
trap cleanup EXIT

fail() {
  printf '%s\n' "$*" >&2
  failures=$((failures + 1))
}

record() {
  printf '%s\n' "$1" >>"$ACTUAL_IDS"
}

check_eq() {
  local id=$1 actual=$2 expected=$3
  record "$id"
  [[ $actual == "$expected" ]] || fail "$id: expected '$expected', got '$actual'"
}

check_ne() {
  local id=$1 actual=$2 unexpected=$3
  record "$id"
  [[ $actual != "$unexpected" ]] || fail "$id: unexpectedly got '$unexpected'"
}

check_contains() {
  local id=$1 actual=$2 expected=$3
  record "$id"
  [[ $actual == *"$expected"* ]] || fail "$id: output does not contain '$expected'"
}

check_not_contains() {
  local id=$1 actual=$2 unexpected=$3
  record "$id"
  [[ $actual != *"$unexpected"* ]] || fail "$id: output contains '$unexpected'"
}

check_file() {
  local id=$1 path=$2
  record "$id"
  [[ -f $path ]] || fail "$id: expected file '$path'"
}

check_no_path() {
  local id=$1 path=$2
  record "$id"
  [[ ! -e $path ]] || fail "$id: unexpected path '$path'"
}

check_json() {
  local id=$1 json=$2 filter=$3
  record "$id"
  jq -e "$filter" <<<"$json" >/dev/null || fail "$id: jq filter failed: $filter"
}

check_json_file() {
  local id=$1 path=$2 filter=$3
  record "$id"
  jq -e "$filter" "$path" >/dev/null || fail "$id: jq filter failed for '$path': $filter"
}

capture() {
  : >"$STDOUT_FILE"
  : >"$STDERR_FILE"
  "$@" >"$STDOUT_FILE" 2>"$STDERR_FILE"
  CAPTURE_STATUS=$?
  CAPTURE_STDOUT=$(<"$STDOUT_FILE")
  CAPTURE_STDERR=$(<"$STDERR_FILE")
  CAPTURE_MERGED=$CAPTURE_STDOUT
  [[ -z $CAPTURE_STDERR ]] || CAPTURE_MERGED+="${CAPTURE_MERGED:+$'\n'}$CAPTURE_STDERR"
}

stub_bin() {
  local name=$1
  cat >"$STUB_DIR/$name" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/$name"
}

setup_repo() {
  TEST_REPO="$CASE_TMP/repo"
  STUB_DIR="$CASE_TMP/stubs"
  STUB_LOG="$STUB_DIR/calls.log"
  mkdir -p "$TEST_REPO" "$STUB_DIR" "$CASE_TMP/data" "$CASE_TMP/config" "$CASE_TMP/tmux"
  export TEST_REPO STUB_DIR STUB_LOG
  export XDG_DATA_HOME="$CASE_TMP/data" XDG_CONFIG_HOME="$CASE_TMP/config" TMUX_TMPDIR="$CASE_TMP/tmux"
  export GIT_CONFIG_GLOBAL=/dev/null CREW_RATE_AUTOSWEEP=0
  export WORKTREE_GIT_LIB="$ROOT/adapters/core/worktree-git.sh"
  unset CREW_ID CREW_WORKER_ID CREW_ROLE_ID TMUX TMUX_PANE DISPATCH_ENGINES
  git -C "$TEST_REPO" init -q -b main
  git -C "$TEST_REPO" config user.email test@example.com
  git -C "$TEST_REPO" config user.name test
  stub_bin claude
  stub_bin codex
  stub_bin cursor-agent
  stub_bin pi
  export PATH="$STUB_DIR:$PATH"
  cd "$TEST_REPO" || return 1
}

setup_refresh_models() {
  STUB_DIR="$CASE_TMP/stubs"
  STUB_LOG="$STUB_DIR/calls.log"
  mkdir -p "$STUB_DIR" "$CASE_TMP/data" "$CASE_TMP/home"
  export STUB_DIR STUB_LOG HOME="$CASE_TMP/home" XDG_DATA_HOME="$CASE_TMP/data"
  export PATH="$STUB_DIR:$PATH"
  cat >"$STUB_DIR/cursor-agent" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [[ -n ${SHIM_CURSOR_FAIL:-} ]]; then
  exit 1
fi
cat <<'MODELS'
Available models

auto - Auto (default)
gpt-5.3-codex-low - Codex 5.3 Low
cursor-grok-4.6-high - Cursor Grok 4.6
cursor-grok-4.6-medium-fast - Cursor Grok 4.6 Medium Fast
cursor-grok-4.6-low-fast - Cursor Grok 4.6 Low Fast
claude-opus-5-high - Claude Opus 5 1M

Tip: use --model <id>
MODELS
EOF
  chmod +x "$STUB_DIR/cursor-agent"
}

setup_pr_watch() {
  setup_repo
  GH_VIEW="$CASE_TMP/view.json"
  GH_THREADS="$CASE_TMP/threads.txt"
  export GH_VIEW GH_THREADS
  : >"$GH_THREADS"
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
  set_view aaa11 OPEN SUCCESS
}

set_view() {
  cat >"$GH_VIEW" <<EOF
{"headRefOid":"$1","state":"$2","reviewDecision":"","latestReviews":[],
 "statusCheckRollup":[{"name":"check","status":"COMPLETED","conclusion":"$3"}],
 "comments":[]}
EOF
}

run_pr_watch() {
  capture bash -euo pipefail "$PR_WATCH" "$@"
}

seed() {
  local prefix=$1
  run_pr_watch 42 --repo o/r --timeout 1 --interval 1
  check_eq "$prefix-seed-status" "$CAPTURE_STATUS" 0
  check_eq "$prefix-seed-empty-stdout" "$CAPTURE_STDOUT" ''
  check_contains "$prefix-seed-timeout-stderr" "$CAPTURE_STDERR" 'park ended after 1s'
}

run_crew_id_cases() {
  setup_repo
  case "$CASE_ID" in
  crew-id-crew-id-resolves-from-worker-task-md-with-no-crew-id-in-the-environment-at-all)
    printf 'crew_id: c-from-taskdoc\n' >WORKER_TASK.md
    capture env -u CREW_ID bash -euo pipefail "$CREW" id
    check_eq cid-taskdoc-unset-status "$CAPTURE_STATUS" 0
    check_eq cid-taskdoc-unset-stdout "$CAPTURE_STDOUT" c-from-taskdoc
    ;;
  crew-id-crew-id-the-task-document-wins-over-a-disagreeing-crew-id)
    printf 'crew_id: c-from-taskdoc\n' >WORKER_TASK.md
    capture env CREW_ID=c-from-env bash -euo pipefail "$CREW" id
    check_eq cid-taskdoc-wins-status "$CAPTURE_STATUS" 0
    check_eq cid-taskdoc-wins-stdout "$CAPTURE_STDOUT" c-from-taskdoc
    ;;
  crew-id-crew-id-resolves-from-a-subdirectory-of-the-worktree)
    printf 'crew_id: c-from-taskdoc\n' >WORKER_TASK.md
    mkdir -p a/b/c
    cd a/b/c || return 1
    capture env -u CREW_ID bash -euo pipefail "$CREW" id
    check_eq cid-subdir-status "$CAPTURE_STATUS" 0
    check_eq cid-subdir-stdout "$CAPTURE_STDOUT" c-from-taskdoc
    ;;
  crew-id-crew-id-crew-id-still-resolves-when-no-worker-task-md-exists)
    capture env CREW_ID=c-from-env bash -euo pipefail "$CREW" id
    check_eq cid-env-status "$CAPTURE_STATUS" 0
    check_eq cid-env-stdout "$CAPTURE_STDOUT" c-from-env
    ;;
  crew-id-crew-id-falls-back-to-crew-id-when-worker-task-md-has-no-crew-id-line)
    printf 'title: some task\n' >WORKER_TASK.md
    capture env CREW_ID=c-from-env bash -euo pipefail "$CREW" id
    check_eq cid-taskdoc-missing-status "$CAPTURE_STATUS" 0
    check_eq cid-taskdoc-missing-stdout "$CAPTURE_STDOUT" c-from-env
    ;;
  esac
}

run_refresh_models_cases() {
  local cache before after local_path='' dir
  setup_refresh_models
  cache="$XDG_DATA_HOME/crew/cursor-models-cache.json"
  case "$CASE_ID" in
  refresh-models-a-realistic-list-models-fixture-parses-into-the-expected-cache-shape)
    capture bash "$REFRESH_MODELS"
    check_eq rm-shape-script-status "$CAPTURE_STATUS" 0
    capture jq '[.models[]?|.slug?|strings]' "$cache"
    check_contains rm-shape-high-slug "$CAPTURE_STDOUT" cursor-grok-4.6-high
    check_contains rm-shape-medium-fast-slug "$CAPTURE_STDOUT" cursor-grok-4.6-medium-fast
    check_contains rm-shape-low-fast-slug "$CAPTURE_STDOUT" cursor-grok-4.6-low-fast
    check_not_contains rm-shape-excludes-auto "$CAPTURE_STDOUT" '"auto"'
    capture jq '.fetched_epoch | type' "$cache"
    check_eq rm-shape-epoch-type "$CAPTURE_STDOUT" '"number"'
    ;;
  refresh-models-the-write-is-atomic-and-lands-at-the-cursor-models-cache-path)
    capture bash "$REFRESH_MODELS"
    check_eq rm-atomic-script-status "$CAPTURE_STATUS" 0
    check_file rm-atomic-cache-exists "$cache"
    capture find "$XDG_DATA_HOME/crew" -maxdepth 1 -name '*.tmp.*'
    check_eq rm-atomic-no-temp-file "$CAPTURE_STDOUT" ''
    capture jq -r '.models | length' "$cache"
    check_eq rm-atomic-model-count "$CAPTURE_STDOUT" 5
    ;;
  refresh-models-a-stubbed-cursor-agent-failure-exits-nonzero-and-leaves-an-existing-cache-untouched)
    mkdir -p "$(dirname "$cache")"
    printf '%s\n' '{"fetched_at":"stale","fetched_epoch":1,"models":[{"slug":"stale-model"}]}' >"$cache"
    before=$(<"$cache")
    capture env SHIM_CURSOR_FAIL=1 bash "$REFRESH_MODELS"
    check_ne rm-stub-failure-status "$CAPTURE_STATUS" 0
    after=$(<"$cache")
    check_eq rm-stub-failure-cache-unchanged "$after" "$before"
    ;;
  refresh-models-cursor-agent-absent-from-path-exits-nonzero-and-leaves-an-existing-cache-untouched)
    mkdir -p "$(dirname "$cache")"
    printf '%s\n' '{"fetched_at":"stale","fetched_epoch":1,"models":[{"slug":"stale-model"}]}' >"$cache"
    before=$(<"$cache")
    IFS=: read -r -a dirs <<<"$PATH"
    for dir in "${dirs[@]}"; do
      [[ -x $dir/cursor-agent ]] && continue
      local_path+="${local_path:+:}$dir"
    done
    capture env PATH="$local_path" bash "$REFRESH_MODELS"
    check_ne rm-absent-status "$CAPTURE_STATUS" 0
    after=$(<"$cache")
    check_eq rm-absent-cache-unchanged "$after" "$before"
    ;;
  esac
}

install_pr_watch_shim() {
  cat >"$STUB_DIR/pr-watch" <<EOF
#!/usr/bin/env bash
exec bash -euo pipefail "$PR_WATCH" "\$@"
EOF
  chmod +x "$STUB_DIR/pr-watch"
}

run_pr_watch_cases() {
  local common log bus_row
  setup_pr_watch
  case "$CASE_ID" in
  pr-watch-the-first-park-seeds-the-cursor-and-reports-nothing)
    seed pw
    check_file pw-first-cursor-exists "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json"
    ;;
  pr-watch-fires-on-a-head-sha-move)
    seed pw-head
    set_view bbb22 OPEN SUCCESS
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-head-status "$CAPTURE_STATUS" 0
    check_json pw-head-event-shape "$CAPTURE_STDOUT" '.changed == ["head_sha"] and .pr == 42 and .repo == "o/r"'
    check_json pw-head-state-transition "$CAPTURE_STDOUT" '.state.head_sha == "bbb22" and .was.head_sha == "aaa11"'
    ;;
  pr-watch-fires-on-a-new-review-thread-reply)
    seed pw-thread
    printf '%s\n' '2026-08-04T10:00:00Z' >"$GH_THREADS"
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-thread-status "$CAPTURE_STATUS" 0
    check_json pw-thread-change-set "$CAPTURE_STDOUT" '.changed | sort == ["thread_at","thread_n"]'
    check_json pw-thread-count "$CAPTURE_STDOUT" '.state.thread_n == 1'
    ;;
  pr-watch-fires-when-the-check-rollup-conclusion-flips)
    seed pw-checks
    set_view aaa11 OPEN FAILURE
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-checks-status "$CAPTURE_STATUS" 0
    check_json pw-checks-event "$CAPTURE_STDOUT" '.changed == ["checks"] and .state.checks == "FAILURE"'
    ;;
  pr-watch-one-in-flight-check-keeps-the-rollup-pending-not-successful)
    cat >"$GH_VIEW" <<'EOF'
{"headRefOid":"aaa11","state":"OPEN","reviewDecision":"","latestReviews":[],
 "statusCheckRollup":[{"name":"a","status":"COMPLETED","conclusion":"SUCCESS"},
                      {"name":"b","status":"IN_PROGRESS","conclusion":""}],
 "comments":[]}
EOF
    seed pw-pending
    check_json_file pw-pending-state "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json" '.checks == "PENDING"'
    ;;
  pr-watch-fires-when-the-pr-merges)
    seed pw-merged
    set_view aaa11 MERGED SUCCESS
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-merged-status "$CAPTURE_STATUS" 0
    check_json pw-merged-event "$CAPTURE_STDOUT" '.changed == ["state"] and .state.state == "MERGED"'
    ;;
  pr-watch-an-unchanged-poll-reports-nothing-and-exits-0)
    seed pw-unchanged
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-unchanged-status "$CAPTURE_STATUS" 0
    check_eq pw-unchanged-empty-stdout "$CAPTURE_STDOUT" ''
    ;;
  pr-watch-the-cursor-prevents-re-delivery-across-restarts)
    seed pw-restart
    set_view bbb22 OPEN SUCCESS
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-restart-event-status "$CAPTURE_STATUS" 0
    check_ne pw-restart-event-stdout "$CAPTURE_STDOUT" ''
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-restart-repeat-status "$CAPTURE_STATUS" 0
    check_eq pw-restart-repeat-empty-stdout "$CAPTURE_STDOUT" ''
    ;;
  pr-watch-runs-with-crew-id-unset-and-never-touches-the-bus)
    seed pw-no-crew
    set_view bbb22 OPEN SUCCESS
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-no-crew-status "$CAPTURE_STATUS" 0
    check_ne pw-no-crew-event "$CAPTURE_STDOUT" ''
    common=$(git rev-parse --path-format=absolute --git-common-dir) ||
      fail "pw-no-crew-no-bus: git rev-parse failed"
    check_no_path pw-no-crew-no-bus "$common/crew"
    ;;
  pr-watch-works-with-no-git-repo-at-all-when-repo-is-given)
    cd / || return 1
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-explicit-repo-status "$CAPTURE_STATUS" 0
    check_file pw-explicit-repo-cursor "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json"
    ;;
  pr-watch-derives-the-repo-from-the-origin-remote)
    git remote add origin git@github.com:o/derived.git
    run_pr_watch 42 --timeout 1 --interval 1
    check_eq pw-derived-repo-status "$CAPTURE_STATUS" 0
    check_file pw-derived-repo-cursor "$XDG_DATA_HOME/crew/pr-watch/o/derived/42.json"
    ;;
  pr-watch-aborts-without-a-pr-number)
    run_pr_watch --repo o/r
    check_eq pw-missing-pr-status "$CAPTURE_STATUS" 1
    check_contains pw-missing-pr-usage "$CAPTURE_MERGED" 'usage: pr-watch'
    ;;
  pr-watch-rejects-an-unbounded-park)
    run_pr_watch 42 --repo o/r --timeout 0
    check_eq pw-unbounded-status "$CAPTURE_STATUS" 1
    check_contains pw-unbounded-message "$CAPTURE_MERGED" 'must be > 0'
    ;;
  pr-watch-rejects-an-unknown-flag)
    run_pr_watch 42 --bogus
    check_eq pw-unknown-status "$CAPTURE_STATUS" 1
    check_contains pw-unknown-message "$CAPTURE_MERGED" 'unknown arg'
    ;;
  pr-watch-a-first-poll-that-cannot-read-the-pr-fails-loudly)
    cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
    chmod +x "$STUB_DIR/gh"
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-gh-failure-status "$CAPTURE_STATUS" 1
    check_contains pw-gh-failure-message "$CAPTURE_MERGED" 'could not read PR 42'
    ;;
  pr-watch-crew-pr-watch-posts-the-event-to-the-crew-s-dispatcher)
    seed pw-crew-event
    set_view bbb22 OPEN SUCCESS
    install_pr_watch_shim
    capture env CREW_ID=c1 bash -euo pipefail "$CREW" pr-watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-crew-event-status "$CAPTURE_STATUS" 0
    check_ne pw-crew-event-stdout "$CAPTURE_STDOUT" ''
    log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
    capture jq -r 'select(.kind=="msg") | "\(.from)|\(.to)|\(.body | fromjson | .changed[0])"' "$log"
    bus_row=$CAPTURE_STDOUT
    check_eq pw-crew-event-bus-row "$bus_row" 'pr-watch:42|dispatcher:c1|head_sha'
    ;;
  pr-watch-crew-pr-watch-posts-nothing-when-the-park-times-out)
    seed pw-crew-timeout
    install_pr_watch_shim
    capture env CREW_ID=c1 bash -euo pipefail "$CREW" pr-watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-crew-timeout-status "$CAPTURE_STATUS" 0
    check_eq pw-crew-timeout-empty-stdout "$CAPTURE_STDOUT" ''
    log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl" ||
      fail "pw-crew-timeout-no-bus-row: git rev-parse failed"
    check_no_path pw-crew-timeout-no-bus-row "$log"
    ;;
  esac
}

validate_assertion_ids() {
  local expected="$CASE_TMP/expected-ids" actual="$CASE_TMP/actual-ids-sorted"
  awk -F '\t' -v case_id="$CASE_ID" 'NR > 1 && $1 == case_id { print $2 }' "$ASSERTIONS" | sort >"$expected"
  sort "$ACTUAL_IDS" >"$actual"
  if ! diff -u "$expected" "$actual" >&2; then
    fail "$CASE_ID: assertion IDs differ from assertions.tsv"
  fi
}

if ! awk -F '\t' -v case_id="$CASE_ID" 'NR > 1 && $1 == case_id { found = 1 } END { exit !found }' "$MANIFEST"; then
  printf 'unknown manifest case: %s\n' "$CASE_ID" >&2
  exit 2
fi

case "$CASE_ID" in
crew-id-*) run_crew_id_cases ;;
refresh-models-*) run_refresh_models_cases ;;
pr-watch-*) run_pr_watch_cases ;;
esac

validate_assertion_ids
((failures == 0))
