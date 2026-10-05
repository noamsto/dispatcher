#!/usr/bin/env bash
set -uo pipefail

readonly CASE_ID=${1:?usage: case-runner.sh CASE_ID}
ROOT=$(git rev-parse --show-toplevel)
readonly ROOT
readonly MANIFEST="$ROOT/tests/harness/manifest.tsv"
readonly ASSERTIONS="$ROOT/tests/harness/assertions.tsv"
readonly CREW="$ROOT/adapters/core/crew.sh"
readonly DISPATCH="$ROOT/adapters/core/dispatch.sh"
readonly REFRESH_MODELS="$ROOT/adapters/core/refresh-models.sh"
readonly PR_WATCH="$ROOT/adapters/core/pr-watch.sh"
CASE_TMP=$(mktemp -d)
readonly CASE_TMP
readonly ACTUAL_IDS="$CASE_TMP/assertion-ids"
readonly STDOUT_FILE="$CASE_TMP/stdout"
readonly STDERR_FILE="$CASE_TMP/stderr"
touch "$ACTUAL_IDS"
failures=0
RW_PID=""

cleanup() {
  if [[ -n $RW_PID ]]; then
    kill "$RW_PID" 2>/dev/null || true
    wait "$RW_PID" 2>/dev/null || true
  fi
  cd / || true
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

check_ge() {
  local id=$1 actual=$2 expected=$3
  record "$id"
  [[ $actual =~ ^[0-9]+$ && $actual -ge $expected ]] || fail "$id: expected >= $expected, got '$actual'"
}

check_lt() {
  local id=$1 actual=$2 bound=$3
  record "$id"
  [[ $actual =~ ^[0-9]+$ && $bound =~ ^[0-9]+$ && $actual -lt $bound ]] ||
    fail "$id: expected $actual < $bound"
}

# Bats assertions that assertions.tsv does not map. Checked, not recorded.
expect_absent() {
  local detail=$1
  shift
  if "$@"; then
    fail "$detail"
  fi
}

expect_present() {
  local detail=$1
  shift
  "$@" || fail "$detail"
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
  export PR_WATCH_CLOCK="$CASE_TMP/clock"
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
  local common log bus_row t0
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
  pr-watch-default-clock-a-1s-park-really-waits)
    unset PR_WATCH_CLOCK
    t0=$SECONDS
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-default-clock-status "$CAPTURE_STATUS" 0
    check_contains pw-default-clock-timeout-stderr "$CAPTURE_STDERR" 'park ended after 1s'
    check_ge pw-default-clock-elapsed "$((SECONDS - t0))" 1
    ;;
  esac
}

rw_frame_permission() {
  cat <<'EOF'

● Waiting on the shell reviewer and test-runner.

✻ Waiting for 1 background agent to finish

› Message from @a6fd725715048d707 (ctrl+o to expand)

● The test-runner confirmed every acceptance criterion passes on the branch, and
  the allowlist tests fail on main. Only the shell reviewer is still out.

✻ Waiting for 2 background agents to finish

● Agent "Review: targeted test-runner" finished · 16m 44s

● Waiting on the shell reviewer.

✻ Waiting for 1 background agent to finish

──────────────────────────────────────────────────────────────────────────────────
 Bash command · from the shell-reviewer agent

   bats --filter grant tests/dispatch-resume.bats 2>&1 | tail -30
   Run bats tests matching grant filter in dispatch-resume.bats

 │ Auto mode classifier requires confirmation for this command.
 │ 3 consecutive actions were blocked. Please review the transcript before
 │ continuing.
 │
 │ Latest blocked action: [Irreversible Local Destruction]

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don’t ask again for: bats *
   3. No

 Esc to cancel · Tab to amend
EOF
}

rw_frame_select() {
  cat <<'EOF'
  2. Gate everything on 3.8
     Detect tmux version once in tmux-remux.tmux; emit the 3.8 hook set.
  3. Require 3.8, drop legacy
  4. Type something.
──────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
EOF
}

rw_frame_quota() {
  cat <<'EOF'
What do you want to do?
❯ 1. Stop and wait for limit to reset
  2. Upgrade your plan
  3. Upgrade to Team plan
Enter to select · Esc to cancel
EOF
}

rw_frame_idle() {
  cat <<'EOF'
✻ Churned for 36s · done 11:20 AM · 1 shell still running
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · 1 shell · ← for agents
EOF
}

# fx_subbatch shape above a bordered idle box: a live turn keeps the box drawn.
rw_frame_live() {
  cat <<'EOF'
  ⎿  Done (15 tool uses · 77.2k tokens · 5m 53s)
✶ Hatching… (6m 1s · ↓ 73.2k tokens)
──────────────────
❯
──────────────────
  -- INSERT -- ⏵⏵ auto mode on · ← for agents
EOF
}

# Not a prompt and not a box: a claude frame nothing recognises must not be typed into.
rw_frame_unknown() {
  cat <<'EOF'
● Some transcript line
  ⎿  Done (14 tool uses · 58.2k tokens · 1m 9s)
EOF
}

# _rw_stub <frame-fn> — tmux stub: capture-pane prints $STUB_DIR/frame, the
# pane exists until $STUB_DIR/stop appears, and every call is logged.
_rw_stub() {
  "$1" >"$STUB_DIR/frame"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
esc=$'\033'
case "$1" in
display-message)
  case "$*" in
  *'#{@crew_exited}'*) printf '%s\n' 0 ;;
  *'#{@crew_role}|#{window_id}'*) printf '%s\n' 'reviewer|@1' ;;
  *)
    [ -e "$STUB_DIR/stop" ] && exit 1
    printf '%s\n' '%6'
    ;;
  esac
  ;;
show-options)
  case "${*: -1}" in
  @crew_branch) printf '%s\n' feat/9-x ;;
  @crew_id) [ -e "$STUB_DIR/no_crew_id" ] || printf '%s\n' c1 ;;
  esac
  ;;
capture-pane)
  case " $* " in
  *' -e '*) cat "$STUB_DIR/frame" ;;
  *) sed -E "s/${esc}\\[[0-9;]*m//g" "$STUB_DIR/frame" ;;
  esac
  ;;
send-keys)
  # dispatch.sh no longer types the assignment via send-keys -l (delivery is
  # paste-buffer below), but the flip trigger stays here too so a fixture that
  # still exercises literal typing (e.g. the bare Enter/C-u keystrokes) has a
  # frame-swap path to hook into.
  if [ "$2 $3 $4" = "-t %6 -l" ] && [ -e "$STUB_DIR/flip" ]; then
    rm -f "$STUB_DIR/flip"
    cp "$STUB_DIR/frame_after" "$STUB_DIR/frame"
  fi
  [ -x "$STUB_DIR/hook" ] && "$STUB_DIR/hook" "$@"
  ;;
load-buffer)
  [ -e "$STUB_DIR/load_buffer_fail" ] && exit 1
  cat >"$STUB_DIR/paste_payload"
  ;;
paste-buffer)
  [ -e "$STUB_DIR/paste_buffer_fail" ] && exit 1
  printf 'paste %s\n' "$(cat "$STUB_DIR/paste_payload" 2>/dev/null)" >>"$STUB_LOG"
  if [ -e "$STUB_DIR/flip" ]; then
    rm -f "$STUB_DIR/flip"
    cp "$STUB_DIR/frame_after" "$STUB_DIR/frame"
  fi
  [ -x "$STUB_DIR/hook" ] && "$STUB_DIR/hook" "$@"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

setup_role_watch() {
  setup_repo
  export HOME="$TEST_REPO"
  export STUB_PANE_PID=$$
  export CREW_REAL="$ROOT/adapters/core/crew.sh"
  export GRANT_CHECK_LIB="$ROOT/adapters/core/grant-check.sh"
  export DISPATCH_CONFIG_BIN="$ROOT/adapters/core/dispatch-config.sh"
  export CROSS_REPO_HINT_LIB="$ROOT/adapters/core/cross-repo-hint.sh"
  unset DISPATCH_PROFILE DISPATCH_SKIP_MODEL_CHECK DISPATCH_IGNORE_RUNG DISPATCH_SPEC \
    DISPATCH_SHAPE DISPATCH_DRAFT_PR DISPATCH_REPO_TRACKERS DISPATCH_ORG_TRACKERS \
    DISPATCH_GRANT_ROOTS DISPATCH_CLAUDE_CONNECTORS DISPATCH_LOCKED_SETTINGS \
    DISPATCHER_CRITICS_DIR DISPATCHER_REVIEWERS_DIR RW_EXTRA
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR"/{WORKER_PROTOCOL.md,EVIDENCE_REVIEW.md,GRID_PROTOCOL.md,REVIEW_TASK.md}
  export DISPATCHER_SKILLS_DIR="$TEST_REPO/harness-skills"
  mkdir -p "$DISPATCHER_SKILLS_DIR/spec-plan-critic"
  printf -- '---\nname: spec-plan-critic\ndescription: seeded\n---\n' \
    >"$DISPATCHER_SKILLS_DIR/spec-plan-critic/SKILL.md"
  stub_bin tmux
  stub_bin crew
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  stub_bin gh
  stub_bin wt
  stub_bin direnv
}

# _write_dirs_record <protocol> <skills> <reviewers> <critics> — the
# protocol-dirs record dispatch writes for feat/9-x, bound to this worktree.
_write_dirs_record() {
  local protocol=$1 skills=$2 reviewers=$3 critics=$4 common_dir
  common_dir="$(git rev-parse --path-format=absolute --git-common-dir)"
  mkdir -p "$common_dir/crew/protocol-dirs/feat"
  printf '%s\n' "$protocol" "$skills" "$reviewers" "$critics" "$(realpath "$PWD")" \
    >"$common_dir/crew/protocol-dirs/feat/9-x"
}

_spawn_role_fixture() {
  git commit -q --allow-empty -m init || return 1
  git worktree add -q -b feat/9-x "$TEST_REPO/.dispatch-wt/feat-9-x" || return 1
  cd "$TEST_REPO/.dispatch-wt/feat-9-x" || return 1
  printf 'agent_name: iris\neffort: high\nworker_id: worker:feat/9-x#s1-1\ncrew_id: c1\n' >WORKER_TASK.md
  export TMUX_PANE=%5
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  roles_dir="$common/crew/artifacts/feat/9-x"
  mkdir -p "$roles_dir"
  printf '{"reviewer":{"agent":"pi","model":"openrouter/deepseek/deepseek-v4-flash"}}\n' >"$roles_dir/roles.json"
  _write_dirs_record "$DISPATCHER_PROTOCOL_DIR" "$DISPATCHER_SKILLS_DIR" "" ""
  # The window options dispatch stamps; --spawn-role's only anchor.
  export STUB_CREW_DIR="$common/crew" STUB_CREW_BRANCH=feat/9-x

  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message)
  case "${*: -1}" in
  '#{pane_pid}') printf '%s\n' "$STUB_PANE_PID" ;;
  *) printf '%s\n' '@1' ;;
  esac
  ;;
show-options)
  case "${*: -1}" in
  @crew_dir) printf '%s\n' "$STUB_CREW_DIR" ;;
  @crew_branch) printf '%s\n' "$STUB_CREW_BRANCH" ;;
  esac
  ;;
list-panes) ;;
split-window) printf '%s\n' '%6' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

# _rw_start <engine> [body] — run the watcher in the background, then post one
# assignment to the role from the lead. $2 defaults to "go".
_rw_start() {
  local engine=$1 body=${2:-go}
  export STUB_DIR STUB_LOG
  bash "$DISPATCH" --role-watch reviewer --pane %6 --engine "$engine" --branch feat/9-x --interval 0.2 >/dev/null 2>&1 &
  RW_PID=$!
  sleep 0.6
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  mkdir -p "$common/crew"
  jq -nc --arg body "$body" '{ts: (now*1000|floor), crew_id: "c1", kind: "msg", from: "worker:feat/9-x#s1-1", to: "role:feat/9-x:reviewer", body: $body}' >>"$common/crew/events.jsonl"
}

_rw_stop() {
  touch "$STUB_DIR/stop"
  wait "$RW_PID" 2>/dev/null || true
  RW_PID=""
}

_rw_sends() { grep -cE '^(paste Assignment: go|send-keys -t %6 -l Assignment: go)$' "$STUB_LOG" || true; }
_rw_captures() { grep -c '^capture-pane' "$STUB_LOG" || true; }

_rw_wait_until() {
  local tries=$1
  shift
  local i=0
  while ((i < tries)); do
    i=$((i + 1))
    "$@" && return 0
    sleep 0.1
  done
  return 1
}

_rw_wait_sends() {
  local want=$1 i=0
  while ((i < 40)); do
    i=$((i + 1))
    [[ $(_rw_sends) -ge $want ]] && return 0
    sleep 0.1
  done
  return 1
}

_rw_wait_captures() {
  local want=$1 i=0
  while ((i < 60)); do
    i=$((i + 1))
    [[ $(_rw_captures) -ge $want ]] && return 0
    sleep 0.1
  done
  return 1
}

_rw_send_count() {
  local pattern=$1
  grep -cE "$pattern" "$STUB_LOG" || true
}

_rw_send_line() {
  local pattern=$1
  grep -nE "$pattern" "$STUB_LOG" | head -1 | cut -d: -f1 || true
}

run_role_watch_cases() {
  local fn captures min_captures go_line two_line
  setup_role_watch
  _spawn_role_fixture || return 1
  case "$CASE_ID" in
  role-watch-role-watch-a-permission-dialog-receives-no-keys-until-it-clears-then-the-assignment-lands-once)
    _rw_stub rw_frame_permission
    _rw_start claude
    expect_present "permission dialog: timed out waiting for captures" _rw_wait_captures 3
    check_eq rw-dialog-clear-no-sends "$(_rw_sends)" 0
    expect_absent "permission dialog received send-keys" grep -q '^send-keys' "$STUB_LOG"
    rw_frame_idle >"$STUB_DIR/frame"
    expect_present "permission dialog: assignment was not delivered" _rw_wait_sends 1
    sleep 0.8
    _rw_stop
    check_eq rw-dialog-clear-one-send "$(_rw_sends)" 1
    expect_present "permission dialog: missing Enter" grep -qx 'send-keys -t %6 Enter' "$STUB_LOG"
    ;;
  role-watch-role-watch-option-select-quota-live-turn-and-unrecognised-claude-frames-defer)
    min_captures=""
    for fn in rw_frame_select rw_frame_quota rw_frame_live rw_frame_unknown; do
      : >"$STUB_LOG"
      rm -f "$STUB_DIR/stop"
      _rw_stub "$fn"
      _rw_start claude
      if ! _rw_wait_captures 2; then
        fail "$fn: timed out waiting for captures"
      fi
      _rw_stop
      captures=$(_rw_captures)
      if [[ -z $min_captures || $captures -lt $min_captures ]]; then
        min_captures=$captures
      fi
      if ((captures < 2)); then
        fail "$fn: never captured"
      fi
      expect_absent "$fn: deferred frame received keys" grep -qE '^(send-keys|load-buffer|paste-buffer)' "$STUB_LOG"
      rm -f "$common/crew/events.jsonl"
    done
    check_ge rw-defer-frames-captured "${min_captures:-0}" 2
    ;;
  role-watch-role-watch-an-idle-claude-input-box-receives-the-assignment)
    _rw_stub rw_frame_idle
    _rw_start claude
    expect_present "idle box: assignment was not delivered" _rw_wait_sends 1
    sleep 0.8
    _rw_stop
    check_eq rw-idle-deliver-one-send "$(_rw_sends)" 1
    ;;
  role-watch-role-watch-queued-assignments-go-out-one-per-tick-in-order)
    _rw_stub rw_frame_idle
    _rw_start claude
    jq -nc '{ts: (now*1000|floor), crew_id: "c1", kind: "msg", from: "worker:feat/9-x#s1-1", to: "role:feat/9-x:reviewer", body: "two"}' >>"$common/crew/events.jsonl"
    expect_present "queue: first assignment was not delivered" _rw_wait_sends 1
    _rw_wait_until 150 grep -qE '^(paste Assignment: two|send-keys -t %6 -l Assignment: two)$' "$STUB_LOG" || true
    sleep 1.2
    _rw_stop
    go_line=$(_rw_send_line '^(paste Assignment: go|send-keys -t %6 -l Assignment: go)$')
    two_line=$(_rw_send_line '^(paste Assignment: two|send-keys -t %6 -l Assignment: two)$')
    check_eq rw-queue-order-first-send "$(_rw_sends)" 1
    check_eq rw-queue-order-second-once "$(_rw_send_count '^(paste Assignment: two|send-keys -t %6 -l Assignment: two)$')" 1
    check_lt rw-queue-order-ordering "$go_line" "$two_line"
    ;;
  role-watch-role-watch-a-dialog-raised-after-the-text-is-typed-is-never-confirmed)
    _rw_stub rw_frame_idle
    rw_frame_permission >"$STUB_DIR/frame_after"
    touch "$STUB_DIR/flip"
    _rw_start claude
    expect_present "late dialog: assignment was not typed" _rw_wait_sends 1
    sleep 0.8
    expect_absent "late dialog confirmed the permission prompt" grep -qx 'send-keys -t %6 Enter' "$STUB_LOG"
    rw_frame_idle >"$STUB_DIR/frame"
    _rw_wait_until 40 grep -qx 'send-keys -t %6 Enter' "$STUB_LOG" || true
    _rw_stop
    expect_present "late dialog: missing Enter" grep -qx 'send-keys -t %6 Enter' "$STUB_LOG"
    expect_present "late dialog: missing C-u" grep -qx 'send-keys -t %6 C-u' "$STUB_LOG"
    check_eq rw-late-dialog-sends-two "$(_rw_sends)" 2
    check_eq rw-late-dialog-single-enter "$(_rw_send_count '^send-keys -t %6 Enter$')" 1
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
role-watch-*) run_role_watch_cases ;;
esac

validate_assertion_ids
((failures == 0))
