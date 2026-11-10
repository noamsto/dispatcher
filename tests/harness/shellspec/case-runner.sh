#!/usr/bin/env bash
set -uo pipefail

readonly CASE_ID=${1:?usage: case-runner.sh CASE_ID}
ROOT=$(git rev-parse --show-toplevel)
readonly ROOT
readonly MANIFEST="$ROOT/tests/harness/manifest.tsv"
readonly ASSERTIONS="$ROOT/tests/harness/assertions.tsv"
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
  cleanup_crew_go_bin
  cd / || true
  XDG_DATA_HOME="$(dirname "$CASE_TMP")/shellspec-trash" gtrash put "$CASE_TMP"
}
trap cleanup EXIT

# The crew entrypoint is the Go binary, and a bare `shellspec` run reaches here
# without run.sh having built it. This file runs without `set -e`: fail the case.
# shellcheck source=/dev/null
source "$ROOT/tests/harness/ensure-crew-go.sh"
ensure_crew_go_bin || exit 1
readonly CREW="$CREW_GO_BIN"

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

# Bats assertions that assertions.tsv does not map. Checked, not recorded.
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
  local tools="$CASE_TMP/tools" tool
  mkdir -p "$STUB_DIR" "$CASE_TMP/data" "$CASE_TMP/home" "$tools"
  # Only the tools the script needs, so a real claude/pi/codex/curl on the host
  # never leaks into a run; engine stubs are added per case.
  for tool in bash jq grep sed head date mkdir mv cat find env chmod tr; do
    ln -s "$(command -v "$tool")" "$tools/$tool"
  done
  export STUB_DIR STUB_LOG HOME="$CASE_TMP/home" XDG_DATA_HOME="$CASE_TMP/data"
  RM_PATH="$STUB_DIR:$tools"
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

write_other_engine_shims() {
  printf '#!/usr/bin/env bash\necho "Usage: claude [options]"\n' >"$STUB_DIR/claude"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$STUB_DIR/codex"
  cat >"$STUB_DIR/pi" <<'EOF'
#!/usr/bin/env bash
[[ -n "${SHIM_PI_FAIL:-}" ]] && exit 1
printf 'provider    model    context  max-out\n'
printf 'openrouter  deepseek/deepseek-v4.1-flash  1M  64K\n'
printf 'anthropic   claude-haiku-4-5  200K  64K\n'
[[ -n "${SHIM_PI_EXTRA:-}" ]] && printf '%b\n' "$SHIM_PI_EXTRA"
if [[ -n "${SHIM_PI_MANY:-}" ]]; then
  for ((i = 0; i < 2100; i++)); do
    printf 'p  vendor/a-fairly-long-model-name-for-size-padding-padding-padding-%d  1M  64K\n' "$i"
  done
fi
exit 0
EOF
  cat >"$STUB_DIR/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
filler=$(head -c 200000 /dev/zero | tr '\0' 'a')
echo '{"data":[{"id":"deepseek/deepseek-v4.1-flash","created":1789000000,"pricing":{"prompt":"0.0000001","completion":"0.0000004"}},{"id":"filler","description":"'"$filler"'"}]}'
EOF
  chmod +x "$STUB_DIR/claude" "$STUB_DIR/codex" "$STUB_DIR/pi" "$STUB_DIR/curl"
  mkdir -p "$HOME/.codex"
  printf '%s\n' '{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.5"}]}' >"$HOME/.codex/models_cache.json"
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

set_view() { # <head-sha> <state> <check-conclusion> [base-sha] [mergeable]
  local base=${4:-baseaaa} mergeable=${5:-MERGEABLE}
  cat >"$GH_VIEW" <<EOF
{"headRefOid":"$1","state":"$2","reviewDecision":"","latestReviews":[],
 "statusCheckRollup":[{"name":"check","status":"COMPLETED","conclusion":"$3"}],
 "comments":[],"baseRefOid":"$base","baseRefName":"main","mergeable":"$mergeable"}
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

run_refresh_models_cases() {
  local cache catalog ids before after local_path='' dir
  setup_refresh_models
  cache="$XDG_DATA_HOME/crew/cursor-models-cache.json"
  catalog="$XDG_DATA_HOME/crew/model-catalog.json"
  case "$CASE_ID" in
  refresh-models-a-realistic-list-models-fixture-parses-into-the-expected-cache-shape)
    capture env PATH="$RM_PATH" bash "$REFRESH_MODELS"
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
    capture env PATH="$RM_PATH" bash "$REFRESH_MODELS"
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
    capture env PATH="$RM_PATH" SHIM_CURSOR_FAIL=1 bash "$REFRESH_MODELS"
    check_ne rm-stub-failure-status "$CAPTURE_STATUS" 0
    after=$(<"$cache")
    check_eq rm-stub-failure-cache-unchanged "$after" "$before"
    ;;
  refresh-models-cursor-agent-absent-from-path-exits-nonzero-and-leaves-an-existing-cache-untouched)
    mkdir -p "$(dirname "$cache")"
    printf '%s\n' '{"fetched_at":"stale","fetched_epoch":1,"models":[{"slug":"stale-model"}]}' >"$cache"
    before=$(<"$cache")
    IFS=: read -r -a dirs <<<"$RM_PATH"
    for dir in "${dirs[@]}"; do
      [[ -x $dir/cursor-agent ]] && continue
      local_path+="${local_path:+:}$dir"
    done
    capture env PATH="$local_path" bash "$REFRESH_MODELS"
    check_ne rm-absent-status "$CAPTURE_STATUS" 0
    after=$(<"$cache")
    check_eq rm-absent-cache-unchanged "$after" "$before"
    ;;
  refresh-models-the-catalog-holds-all-four-engines-in-the-section-5-shape)
    write_other_engine_shims
    capture env PATH="$RM_PATH" bash "$REFRESH_MODELS"
    check_eq rm-catalog-1 "$CAPTURE_STATUS" 0
    check_eq rm-catalog-2 "$(jq -r '.claude.source' "$catalog")" aliases
    check_eq rm-catalog-3 "$(jq -r '.claude.models | map(.id) | join(",")' "$catalog")" fable,haiku,opus,sonnet
    check_eq rm-catalog-4 "$(jq -r '.codex.source' "$catalog")" 'codex models_cache.json'
    check_eq rm-catalog-5 "$(jq -r '.codex.models[0].id' "$catalog")" gpt-5.5
    check_eq rm-catalog-6 "$(jq -r '.cursor.source' "$catalog")" 'cursor-agent --list-models'
    check_eq rm-catalog-7 "$(jq -r '.cursor.models | map(.id) | index("cursor-grok-4.6-high") != null' "$catalog")" true
    check_eq rm-catalog-8 "$(jq -r '.pi.source' "$catalog")" 'pi --list-models; openrouter /api/v1/models'
    check_eq rm-catalog-9 "$(jq -r '.pi.models[] | select(.id == "openrouter/deepseek/deepseek-v4.1-flash") | [.created, .price.in == 0.0000001, .price.out == 0.0000004] | join(",")' "$catalog")" 1789000000,true,true
    check_eq rm-catalog-11 "$(jq -c '.pi.models[] | select(.id == "anthropic/claude-haiku-4-5")' "$catalog")" '{"id":"anthropic/claude-haiku-4-5"}'
    check_eq rm-catalog-12 "$(jq -r '[.[] | .fetched_epoch | type] | unique | join(",")' "$catalog")" number
    ;;
  refresh-models-a-failing-pi-probe-keeps-pi-s-previous-snapshot-and-refreshes-the-rest)
    write_other_engine_shims
    mkdir -p "$(dirname "$catalog")"
    printf '%s\n' '{"pi":{"fetched_epoch":1,"source":"old","models":[{"id":"old/pi-model"}]}}' >"$catalog"
    capture env PATH="$RM_PATH" SHIM_PI_FAIL=1 bash "$REFRESH_MODELS"
    check_eq rm-pi-fail-1 "$CAPTURE_STATUS" 0
    check_contains rm-pi-fail-2 "$CAPTURE_MERGED" 'pi --list-models failed'
    check_eq rm-pi-fail-3 "$(jq -c '.pi' "$catalog")" '{"fetched_epoch":1,"source":"old","models":[{"id":"old/pi-model"}]}'
    check_eq rm-pi-fail-4 "$(jq -r '.cursor.source' "$catalog")" 'cursor-agent --list-models'
    check_eq rm-pi-fail-5 "$(jq -r '.claude.source' "$catalog")" aliases
    ;;
  refresh-models-ids-with-control-bytes-or-outside-the-shape-regex-are-dropped)
    write_other_engine_shims
    # shellcheck disable=SC2016 # the stub's printf %b expands the escapes, not this shell
    capture env PATH="$RM_PATH" SHIM_PI_EXTRA='bad  evil\x1b[31mid  1M\nbad  has$dollar  1M\nbad  spa<ce  1M\nok  good.id:1@x  1M' bash "$REFRESH_MODELS"
    check_eq rm-ids-drop-1 "$CAPTURE_STATUS" 0
    ids=$(jq -r '.pi.models | map(.id) | join("\n")' "$catalog")
    check_contains rm-ids-drop-2 "$ids" 'ok/good.id:1@x'
    check_not_contains rm-ids-drop-3 "$ids" evil
    check_not_contains rm-ids-drop-4 "$ids" dollar
    check_not_contains rm-ids-drop-5 "$ids" 'spa<ce'
    check_eq rm-ids-drop-6 "$(jq -r '[.[].models[].id | select(test("[[:cntrl:]]"))] | length' "$catalog")" 0
    ;;
  refresh-models-an-engine-list-past-128-kib-is-capped-at-2000-models-and-still-written-with-the-other-engines)
    write_other_engine_shims
    capture env PATH="$RM_PATH" SHIM_PI_MANY=1 bash "$REFRESH_MODELS"
    check_eq rm-cap-1 "$CAPTURE_STATUS" 0
    check_contains rm-cap-2 "$CAPTURE_MERGED" 'pi: dropped'
    check_eq rm-cap-3 "$(jq -r '.pi.models | length' "$catalog")" 2000
    check_eq rm-cap-4 "$(jq -r '.claude.source' "$catalog")" aliases
    ;;
  refresh-models-the-cursor-cache-is-unchanged-by-the-catalog-and-the-openrouter-fetch-goes-through-the-stub)
    write_other_engine_shims
    capture env PATH="$RM_PATH" bash "$REFRESH_MODELS"
    check_eq rm-cache-compat-1 "$CAPTURE_STATUS" 0
    check_eq rm-cache-compat-2 "$(jq -c 'del(.fetched_at, .fetched_epoch)' "$cache")" \
      '{"models":[{"slug":"gpt-5.3-codex-low"},{"slug":"cursor-grok-4.6-high"},{"slug":"cursor-grok-4.6-medium-fast"},{"slug":"cursor-grok-4.6-low-fast"},{"slug":"claude-opus-5-high"}]}'
    expect_present 'openrouter fetch did not reach the curl stub' grep -q 'openrouter.ai/api/v1/models' "$STUB_LOG"
    ;;
  refresh-models-cursor-failing-while-another-engine-refreshes-still-exits-zero-and-warns)
    write_other_engine_shims
    capture env PATH="$RM_PATH" SHIM_CURSOR_FAIL=1 bash "$REFRESH_MODELS"
    check_eq rm-cursor-fail-1 "$CAPTURE_STATUS" 0
    check_contains rm-cursor-fail-2 "$CAPTURE_MERGED" 'cursor-agent --list-models failed'
    check_eq rm-cursor-fail-3 "$(jq -r 'has("cursor")' "$catalog")" false
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
  pr-watch-fires-when-the-base-tip-advances)
    seed pw-base
    set_view aaa11 OPEN SUCCESS basebbb MERGEABLE
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-base-status "$CAPTURE_STATUS" 0
    check_json pw-base-event "$CAPTURE_STDOUT" '.changed == ["base_sha"]'
    check_json pw-base-state-transition "$CAPTURE_STDOUT" '.state.base_sha == "basebbb" and .was.base_sha == "baseaaa"'
    ;;
  pr-watch-fires-when-mergeable-flips-to-conflicting)
    seed pw-conflict
    set_view aaa11 OPEN SUCCESS baseaaa CONFLICTING
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-conflict-status "$CAPTURE_STATUS" 0
    check_json pw-conflict-event "$CAPTURE_STDOUT" '.changed == ["mergeable"]'
    check_json pw-conflict-state-transition "$CAPTURE_STDOUT" '.state.mergeable == "CONFLICTING" and .was.mergeable == "MERGEABLE"'
    ;;
  pr-watch-an-old-cursor-missing-base-sha-stays-quiet-until-the-base-moves)
    mkdir -p "$XDG_DATA_HOME/crew/pr-watch/o/r"
    jq -nc '{head_sha:"aaa11",state:"OPEN",review_decision:"",review_at:"",checks:"SUCCESS",thread_at:"",thread_n:0}' \
      >"$XDG_DATA_HOME/crew/pr-watch/o/r/42.json"
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-oldcursor-quiet-status "$CAPTURE_STATUS" 0
    check_eq pw-oldcursor-quiet-empty-stdout "$CAPTURE_STDOUT" ''
    check_json_file pw-oldcursor-base-backfilled "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json" '.base_sha == "baseaaa"'
    set_view aaa11 OPEN SUCCESS basebbb MERGEABLE
    run_pr_watch 42 --repo o/r --timeout 30 --interval 1
    check_eq pw-oldcursor-event-status "$CAPTURE_STATUS" 0
    check_json pw-oldcursor-event "$CAPTURE_STDOUT" '.changed == ["base_sha"] and .was.base_sha == "baseaaa"'
    ;;
  pr-watch-unknown-mergeable-is-not-an-event-and-the-cursor-keeps-the-last-known-value)
    seed pw-unknown-mergeable
    set_view aaa11 OPEN SUCCESS baseaaa UNKNOWN
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-unknown-mergeable-status "$CAPTURE_STATUS" 0
    check_eq pw-unknown-mergeable-empty-stdout "$CAPTURE_STDOUT" ''
    check_json_file pw-unknown-mergeable-cursor-kept "$XDG_DATA_HOME/crew/pr-watch/o/r/42.json" '.mergeable == "MERGEABLE"'
    set_view aaa11 OPEN SUCCESS baseaaa MERGEABLE
    run_pr_watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-unknown-mergeable-recover-status "$CAPTURE_STATUS" 0
    check_eq pw-unknown-mergeable-recover-empty-stdout "$CAPTURE_STDOUT" ''
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
    capture env CREW_ID=c1 "$CREW" pr-watch 42 --repo o/r --timeout 30 --interval 1
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
    capture env CREW_ID=c1 "$CREW" pr-watch 42 --repo o/r --timeout 1 --interval 1
    check_eq pw-crew-timeout-status "$CAPTURE_STATUS" 0
    check_eq pw-crew-timeout-empty-stdout "$CAPTURE_STDOUT" ''
    log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl" ||
      fail "pw-crew-timeout-no-bus-row: git rev-parse failed"
    check_no_path pw-crew-timeout-no-bus-row "$log"
    ;;
  pr-watch-crew-pr-watch-exits-with-the-child-s-status-and-posts-nothing)
    cat >"$STUB_DIR/pr-watch" <<'EOF'
#!/usr/bin/env bash
echo '{"pr":42,"changed":["head_sha"]}'
echo 'gh: something went wrong' >&2
exit 3
EOF
    chmod +x "$STUB_DIR/pr-watch"
    capture env CREW_ID=c1 "$CREW" pr-watch 42
    check_eq pw-crew-child-status "$CAPTURE_STATUS" 3
    check_contains pw-crew-child-stderr "$CAPTURE_STDERR" 'gh: something went wrong'
    check_not_contains pw-crew-child-no-partial-stdout "$CAPTURE_STDOUT" '"changed"'
    log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl" ||
      fail "pw-crew-child-no-bus-row: git rev-parse failed"
    check_no_path pw-crew-child-no-bus-row "$log"
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
refresh-models-*) run_refresh_models_cases ;;
pr-watch-*) run_pr_watch_cases ;;
esac

validate_assertion_ids
((failures == 0))
