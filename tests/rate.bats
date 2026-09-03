bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  setup_repo
  # setup_repo leaves no origin remote, but the reconcile gate (a run may only
  # ever spend gh credentials on the repo it was dispatched from) reads
  # `$repo` from remote.origin.url — so every pr_url fixture below targets
  # this same owner/repo.
  git remote add origin https://github.com/acme/widgets.git
  # The store lives under $XDG_DATA_HOME — keep it out of the developer's own
  # (spec: --report reads the global store, never the local bus).
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  # No test may make a real network call — gh is stubbed on PATH for every
  # test, even ones that never expect to invoke it (e.g. --report).
  stub_gh
  unset CREW_ID
}

teardown() {
  teardown_repo
}

# ---------------------------------------------------------------------------
# t2 reconcile harness — a gh stub serving the three calls the sweep can make
# (pr view / actions runs / graphql review threads), logging every
# invocation's argv to $STUB_LOG. Modelled on tests/pr-watch.bats's stub_gh;
# set_view/set_runs/set_threads let each test rewrite exactly the response it
# needs before sweeping.
# ---------------------------------------------------------------------------
stub_gh() {
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
pr) cat "$GH_VIEW" ;;
api)
  # Match on the "graphql" token itself, not its position: the Actions call
  # now carries `--method GET … -f branch=… -F per_page=100` ahead of the
  # path, so `$2` is no longer the URL.
  is_graphql=false
  for a in "$@"; do
    [ "$a" = graphql ] && is_graphql=true
  done
  if [ "$is_graphql" = true ]; then
    cat "$GH_THREADS"
  else
    cat "$GH_RUNS"
  fi
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  GH_VIEW="$BATS_TEST_TMPDIR/view.json"
  GH_RUNS="$BATS_TEST_TMPDIR/runs.json"
  GH_THREADS="$BATS_TEST_TMPDIR/threads.json"
  export GH_VIEW GH_RUNS GH_THREADS
  set_view '{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}'
  set_runs '{"workflow_runs":[]}'
  set_threads '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}'
}

set_view() { printf '%s' "$1" >"$GH_VIEW"; }      # next `gh pr view` response
set_runs() { printf '%s' "$1" >"$GH_RUNS"; }       # next `gh api .../actions/runs` response
set_threads() { printf '%s' "$1" >"$GH_THREADS"; } # next `gh api graphql` response

# seed_dispatch <branch> <ts_ms> [engine] [model] [tier] — a dispatch event
# with an explicit ts, so branch-reuse and CI-window fixtures control run
# ordering exactly (crew.sh has no `dispatch` subcommand of its own — dispatch
# events are written by dispatch.sh, so tests seed them directly, same as
# tests/crew.bats does).
seed_dispatch() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg b "$1" --argjson ts "$2" --arg e "${3:-claude}" --arg m "${4:-sonnet}" --arg t "${5:-standard}" \
    '{ts:$ts, crew_id:"c1", kind:"dispatch", branch:$b, engine:$e, model:$m, tier:$t, effort:"medium", title:"t"}' >>"$logf"
}

# seed_status <from> <ts_ms> <state> [pr_url] — a status event with an
# explicit ts (`crew status` always stamps `now`, too coarse for the
# windowing fixtures below).
seed_status() {
  local logf
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc --arg f "$1" --argjson ts "$2" --arg s "$3" --arg pr "${4:-}" \
    '{ts:$ts, crew_id:"c1", from:$f, to:"dispatcher:c1", kind:"status",
      body:({state:$s} + (if $pr!="" then {pr_url:$pr} else {} end))}' >>"$logf"
}

# store_rows — the global ratings store, folded last-wins by run_id (the same
# `crew roster` idiom --report uses), as JSON on stdout.
store_rows() { jq -s -c 'group_by(.run_id) | map(max_by(.swept_at))' "$XDG_DATA_HOME/crew/ratings.jsonl"; }

# The spec's worked example (docs/superpowers/specs/
# 2026-08-10-crew-rate-reconcile-report-design.md § crew rate --report),
# built from a hand-authored store rather than a swept bus: --report folds
# the global store last-wins by run_id and never touches the local bus or the
# network, so the store IS the input under test. 19 rows across 3 groups
# reach every marker this table can render: "value", "value(k)", "!" on both
# an aggregate and on n itself, and "—" for an unmeasured aggregate.
#
# claude/opus/deep (14 rows, n=12): 1 incomplete, 1 running, 9 merged, 1
# closed-not-merged, 2 open — giving pr%=100 (k=12), merge%=90(10) (9/10),
# ttpr=38m (all 12), ttmerge=2.1h(9) (the 9 merged), rework=0.6(11) (one run
# has no metrics), high=0.5(2)! (only 2 rows ran a review), rounds=1.1 (all
# 12), blocked=0.3 (all 12), ci1=89(9) (3 rows had no Actions run in window),
# notes=0.2 (all 12), cost=4.2 (all 12, weight x wall-clock in hours).
#
# claude/sonnet/standard (4 rows, n=3): 1 running, 2 merged, 1
# closed-not-merged — small enough (n<5) that every aggregate in the row
# takes "!". ttmerge's median of the 2 merged rows lands exactly on the
# humanised-duration boundary (90m = 1.5h) to prove the boundary is handled.
#
# codex/sol/deep (1 row, n=1): a single open PR — merge%, ttmerge and high
# have no queryable denominator at all (k=0), rendering "—".
@test "rate --report: renders the spec's worked example byte-for-byte" {
  mkdir -p "$XDG_DATA_HOME/crew"
  cat >"$XDG_DATA_HOME/crew/ratings.jsonl" <<'EOF'
{"run_id":"g1-r1","engine":"claude","model":"opus","tier":"deep","outcome":"incomplete","reached_pr":false,"time_to_pr_ms":null,"pr_state":null,"time_to_merge_ms":null,"rework_count":null,"review_high":null,"review_mode":null,"review_rounds":null,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":null,"reverted":null,"cost_proxy":null,"swept_at":1}
{"run_id":"g1-r2","engine":"claude","model":"opus","tier":"deep","outcome":"running","reached_pr":false,"time_to_pr_ms":null,"pr_state":null,"time_to_merge_ms":null,"rework_count":null,"review_high":null,"review_mode":null,"review_rounds":null,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":null,"reverted":null,"cost_proxy":null,"swept_at":1}
{"run_id":"g1-r3","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":1,"review_high":1,"review_mode":"high","review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":1,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r4","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":1,"review_high":0,"review_mode":"high","review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":1,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r5","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":1,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":4,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r6","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":1,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r7","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":1,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r8","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":1,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r9","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":1,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r10","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":0,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r11","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"MERGED","time_to_merge_ms":7560000,"rework_count":0,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":false,"unresolved_notes":0,"reverted":false,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r12","engine":"claude","model":"opus","tier":"deep","outcome":"pr_open","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"CLOSED","time_to_merge_ms":null,"rework_count":null,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":0,"reverted":null,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r13","engine":"claude","model":"opus","tier":"deep","outcome":"pr_open","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"OPEN","time_to_merge_ms":null,"rework_count":0,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":0,"reverted":null,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g1-r14","engine":"claude","model":"opus","tier":"deep","outcome":"pr_open","reached_pr":true,"time_to_pr_ms":2280000,"pr_state":"OPEN","time_to_merge_ms":null,"rework_count":0,"review_high":null,"review_mode":null,"review_rounds":2,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":0,"reverted":null,"cost_proxy":15120000,"swept_at":1}
{"run_id":"g2-r15","engine":"claude","model":"sonnet","tier":"standard","outcome":"running","reached_pr":false,"time_to_pr_ms":null,"pr_state":null,"time_to_merge_ms":null,"rework_count":null,"review_high":null,"review_mode":null,"review_rounds":null,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":null,"reverted":null,"cost_proxy":null,"swept_at":1}
{"run_id":"g2-r16","engine":"claude","model":"sonnet","tier":"standard","outcome":"merged","reached_pr":true,"time_to_pr_ms":1260000,"pr_state":"MERGED","time_to_merge_ms":5400000,"rework_count":1,"review_high":1,"review_mode":"high","review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":1,"reverted":false,"cost_proxy":3960000,"swept_at":1}
{"run_id":"g2-r17","engine":"claude","model":"sonnet","tier":"standard","outcome":"merged","reached_pr":true,"time_to_pr_ms":1260000,"pr_state":"MERGED","time_to_merge_ms":5400000,"rework_count":1,"review_high":1,"review_mode":"high","review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":1,"reverted":false,"cost_proxy":3960000,"swept_at":1}
{"run_id":"g2-r18","engine":"claude","model":"sonnet","tier":"standard","outcome":"pr_open","reached_pr":true,"time_to_pr_ms":1260000,"pr_state":"CLOSED","time_to_merge_ms":null,"rework_count":2,"review_high":0,"review_mode":"high","review_rounds":0,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":false,"unresolved_notes":0,"reverted":null,"cost_proxy":3960000,"swept_at":1}
{"run_id":"g3-r19","engine":"codex","model":"sol","tier":"deep","outcome":"pr_open","reached_pr":true,"time_to_pr_ms":3300000,"pr_state":"OPEN","time_to_merge_ms":null,"rework_count":2,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":1,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":3,"reverted":null,"cost_proxy":21600000,"swept_at":1}
EOF
  run run_crew rate --report
  [ "$status" -eq 0 ]

  expected=$(cat <<'EOF'
engine  model   tier       n  inc  run  pend   pr%  merge%  ttpr   ttmerge   rework     high  rounds  blocked    ci1  notes  rev  cost
claude  opus    deep      12    1    1     2   100  90(10)   38m   2.1h(9)  0.6(11)  0.5(2)!     1.1      0.3  89(9)    0.2    0   4.2
claude  sonnet  standard  3!    0    1     0  100!     67!  21m!  1.5h(2)!     1.3!     0.7!    0.7!     0.0!    67!   0.7!    0  1.1!
codex   sol     deep      1!    0    0     1  100!       —  55m!         —     2.0!        —    1.0!     1.0!   100!   3.0!    0  6.0!

! own sample < 5 — anecdote, not evidence.   value(k) = measured over k of n runs.   — = unmeasured.
3 of 3 rows carry at least one small-sample quantity.
EOF
  )
  [ "$output" = "$expected" ]
}

# ---------------------------------------------------------------------------
# Store semantics
# ---------------------------------------------------------------------------

@test "sweep: a second sweep with nothing new appends nothing" {
  seed_dispatch feat/x 1000
  CREW_ID=c1 run_crew status "worker:feat/x" done
  run run_crew rate
  [ "$status" -eq 0 ]
  before="$(cat "$XDG_DATA_HOME/crew/ratings.jsonl")"
  run run_crew rate
  [ "$status" -eq 0 ]
  after="$(cat "$XDG_DATA_HOME/crew/ratings.jsonl")"
  [ "$before" = "$after" ]
  [ "$(wc -l <"$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]
}

@test "--report: a stale row is superseded by a fresh row sharing run_id" {
  mkdir -p "$XDG_DATA_HOME/crew"
  cat >"$XDG_DATA_HOME/crew/ratings.jsonl" <<'EOF'
{"run_id":"r1","engine":"claude","model":"opus","tier":"deep","outcome":"incomplete","reached_pr":false,"time_to_pr_ms":null,"pr_state":null,"time_to_merge_ms":null,"rework_count":null,"review_high":null,"review_mode":null,"review_rounds":null,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":null,"reverted":null,"cost_proxy":null,"swept_at":1}
{"run_id":"r1","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":100,"pr_state":"MERGED","time_to_merge_ms":200,"rework_count":0,"review_high":null,"review_mode":null,"review_rounds":0,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":100,"swept_at":2}
EOF
  run run_crew rate --report --json
  [ "$status" -eq 0 ]
  run jq -e '.[0].n.value == 1 and .[0].merge_pct.value == 100' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "sweep: an OPEN PR upgrades to MERGED with time_to_merge_ms on the next sweep" {
  seed_dispatch feat/upgrade 1000
  seed_status worker:feat/upgrade 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view '{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0] | "\(.outcome) \(.merged) \(.time_to_merge_ms)"' <<<"$(store_rows)"
  [ "$output" = "pr_open false null" ]

  set_view '{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:10Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0] | "\(.outcome) \(.merged) \(.time_to_merge_ms)"' <<<"$(store_rows)"
  [ "$output" = "merged true 9000" ]
}

@test "sweep: a failing gh preserves stored t2 and marks last_query_ok false" {
  seed_dispatch feat/mf 1000
  seed_status worker:feat/mf 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view '{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}'
  set_threads '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false}]}}}}}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0] | "\(.pr_state) \(.unresolved_notes) \(.last_query_ok)"' <<<"$(store_rows)"
  [ "$output" = "OPEN 1 true" ]

  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
  chmod +x "$STUB_DIR/gh"
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0] | "\(.pr_state) \(.unresolved_notes) \(.last_query_ok)"' <<<"$(store_rows)"
  [ "$output" = "OPEN 1 false" ]
}

@test "sweep: every gh call failing surfaces a count on stderr; stdout stays clean" {
  seed_dispatch feat/gh-fail 1000
  seed_status worker:feat/gh-fail 1500 pr_open "https://github.com/acme/widgets/pull/1"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
  chmod +x "$STUB_DIR/gh"
  run --separate-stderr run_crew rate
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [ "$stderr" = "crew: rate: 3 gh call(s) failed; those rows kept their stored t2" ]
}

# ---------------------------------------------------------------------------
# Dual-lock / trap regression (FIX 3): `_lock_release` is an unconditional
# `rm -rf` with no owner check, so a trap left armed across the unlocked
# network phase would delete a CONCURRENT sweep's lock dir. These pin that a
# live foreign lock is refused, not stomped — in both windows.
# ---------------------------------------------------------------------------

@test "lock: a live foreign lock held before the sweep starts refuses window 1, leaves the lock and the store untouched" {
  seed_dispatch feat/lock-w1 1000
  seed_status worker:feat/lock-w1 1500 pr_open "https://github.com/acme/widgets/pull/1"
  mkdir -p "$XDG_DATA_HOME/crew"
  sleep 100 &
  live_pid=$!
  mkdir "$XDG_DATA_HOME/crew/ratings.lock.d"
  printf '%s\n' "$live_pid" >"$XDG_DATA_HOME/crew/ratings.lock.d/pid"

  run --separate-stderr run_crew rate
  kill "$live_pid" 2>/dev/null || true

  [ "$status" -eq 1 ]
  [ "$stderr" = "crew: ratings store busy" ]
  [ -d "$XDG_DATA_HOME/crew/ratings.lock.d" ]
  [ "$(cat "$XDG_DATA_HOME/crew/ratings.lock.d/pid")" = "$live_pid" ]
  [ ! -e "$XDG_DATA_HOME/crew/ratings.jsonl" ]
  [ ! -s "$STUB_LOG" ]
}

@test "lock: a live foreign lock taken during the unlocked network phase refuses window 2, leaves the lock and the store untouched" {
  seed_dispatch feat/lock-w2 1000
  seed_status worker:feat/lock-w2 1500 pr_open "https://github.com/acme/widgets/pull/1"
  sleep 100 &
  live_pid=$!
  # Simulates a concurrent sweep grabbing the lock while THIS sweep is doing
  # its unlocked gh calls, between window 1 (released) and window 2 (about to
  # re-acquire) — the scenario the hand-armed EXIT trap must not corrupt.
  cat >"$STUB_DIR/gh" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$STUB_LOG"
mkdir -p "$XDG_DATA_HOME/crew"
mkdir "$XDG_DATA_HOME/crew/ratings.lock.d" 2>/dev/null
printf '%s\n' "$live_pid" >"$XDG_DATA_HOME/crew/ratings.lock.d/pid"
case "\$1" in
pr) cat "$GH_VIEW" ;;
api) cat "$GH_RUNS" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"

  run --separate-stderr run_crew rate
  kill "$live_pid" 2>/dev/null || true

  [ "$status" -eq 1 ]
  [ "$stderr" = "crew: ratings store busy" ]
  [ -d "$XDG_DATA_HOME/crew/ratings.lock.d" ]
  [ "$(cat "$XDG_DATA_HOME/crew/ratings.lock.d/pid")" = "$live_pid" ]
  [ ! -e "$XDG_DATA_HOME/crew/ratings.jsonl" ]
}

@test "--report on an empty store prints the header and exits 0" {
  run run_crew rate --report
  [ "$status" -eq 0 ]
  [ "$output" = "engine  model  tier  n  inc  run  pend  pr%  merge%  ttpr  ttmerge  rework  high  rounds  blocked  ci1  notes  rev  cost" ]
}

@test "--json without --report is an error" {
  run run_crew rate --json
  [ "$status" -eq 1 ]
  [[ "$output" == *"rate takes --report and --json"* ]]
}

# ---------------------------------------------------------------------------
# Reconcile behaviour
# ---------------------------------------------------------------------------

@test "reconcile: a MERGED row skips pr view but keeps querying Actions while first_ci_green is null" {
  seed_dispatch feat/finality 1000
  seed_status worker:feat/finality 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view '{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:05Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0] | "\(.pr_state) \(.first_ci_green) \(.unresolved_notes)"' <<<"$(store_rows)"
  [ "$output" = "MERGED null 0" ]

  : >"$STUB_LOG"
  run run_crew rate
  [ "$status" -eq 0 ]
  ! grep -q '^pr view' "$STUB_LOG"
  grep -q 'actions/runs' "$STUB_LOG"
  ! grep -q 'graphql' "$STUB_LOG"
}

@test "reconcile: a CLOSED PR inside 30 days is re-queried on the next sweep" {
  seed_dispatch feat/closed-recent 1000
  seed_status worker:feat/closed-recent 1500 pr_open "https://github.com/acme/widgets/pull/1"
  now_ms=$(jq -nc 'now*1000|floor')
  closed_ms=$((now_ms - 10 * 86400 * 1000))
  closed_iso=$(jq -nr --argjson t "$closed_ms" '($t/1000)|todate')
  set_view "$(jq -nc --arg c "$closed_iso" '{state:"CLOSED", closedAt:$c, mergedAt:null, mergeCommit:null, commits:[], reviews:[]}')"
  run run_crew rate
  [ "$status" -eq 0 ]
  : >"$STUB_LOG"
  run run_crew rate
  [ "$status" -eq 0 ]
  grep -q '^pr view' "$STUB_LOG"
}

@test "reconcile: a CLOSED PR outside 30 days is not re-queried" {
  seed_dispatch feat/closed-old 1000
  seed_status worker:feat/closed-old 1500 pr_open "https://github.com/acme/widgets/pull/1"
  now_ms=$(jq -nc 'now*1000|floor')
  closed_ms=$((now_ms - 40 * 86400 * 1000))
  closed_iso=$(jq -nr --argjson t "$closed_ms" '($t/1000)|todate')
  set_view "$(jq -nc --arg c "$closed_iso" '{state:"CLOSED", closedAt:$c, mergedAt:null, mergeCommit:null, commits:[], reviews:[]}')"
  run run_crew rate
  [ "$status" -eq 0 ]
  : >"$STUB_LOG"
  run run_crew rate
  [ "$status" -eq 0 ]
  ! grep -q '^pr view' "$STUB_LOG"
}

@test "reconcile: a pr_url naming a different repo is skipped with zero gh calls; a same-repo run's pr view always carries an explicit URL" {
  seed_dispatch feat/same-repo 1000
  seed_status worker:feat/same-repo 1500 pr_open "https://github.com/acme/widgets/pull/1"
  seed_dispatch feat/other-repo 2000
  seed_status worker:feat/other-repo 2500 pr_open "https://github.com/someone-else/other-repo/pull/2"
  run run_crew rate
  [ "$status" -eq 0 ]
  grep -q '^pr view https://github.com/acme/widgets/pull/1 ' "$STUB_LOG"
  ! grep -q 'other-repo' "$STUB_LOG"
}

@test "reconcile: branch reuse — the earlier run owns the PR and its CI window excludes the later run's" {
  seed_dispatch feat/reuse 1000
  seed_status worker:feat/reuse 1500 pr_open "https://github.com/acme/widgets/pull/1"
  seed_dispatch feat/reuse 3000
  seed_status worker:feat/reuse 3500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view '{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:10Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}'
  set_runs '{"workflow_runs":[
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:01Z","status":"completed","conclusion":"success"},
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:04Z","status":"completed","conclusion":"failure"}
  ]}'
  run run_crew rate
  [ "$status" -eq 0 ]
  rows="$(store_rows)"
  run jq -r 'sort_by(.t0_ms) | .[0] | "\(.owns_pr) \(.first_ci_green) \(.time_to_pr_ms != null)"' <<<"$rows"
  [ "$output" = "true true true" ]
  run jq -r 'sort_by(.t0_ms) | .[1] | "\(.owns_pr) \(.pr_state) \(.time_to_pr_ms)"' <<<"$rows"
  [ "$output" = "false null null" ]
}

@test "reconcile: a non-GitHub pr_url issues zero gh calls" {
  seed_dispatch feat/external 1000
  seed_status worker:feat/external 1500 pr_open "https://example.com/pr/1"
  run run_crew rate
  [ "$status" -eq 0 ]
  [ ! -s "$STUB_LOG" ]
}

# ---------------------------------------------------------------------------
# Derived fields and the report
# ---------------------------------------------------------------------------

@test "review_rounds: batches reviews into review-to-fix cycles, excludes APPROVED and trailing reviews" {
  seed_dispatch feat/rounds 1000
  seed_status worker:feat/rounds 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view '{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,
    "commits":[{"committedDate":"1970-01-01T00:00:04Z"},{"committedDate":"1970-01-01T00:00:07Z"}],
    "reviews":[
      {"state":"CHANGES_REQUESTED","submittedAt":"1970-01-01T00:00:01Z"},
      {"state":"COMMENTED","submittedAt":"1970-01-01T00:00:02Z"},
      {"state":"CHANGES_REQUESTED","submittedAt":"1970-01-01T00:00:03Z"},
      {"state":"APPROVED","submittedAt":"1970-01-01T00:00:05Z"},
      {"state":"CHANGES_REQUESTED","submittedAt":"1970-01-01T00:00:09Z"}
    ]}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0].review_rounds' <<<"$(store_rows)"
  [ "$output" = "1" ]
}

@test "first_ci_green: mixed conclusions on the earliest sha render false" {
  seed_dispatch feat/ci-mixed 1000
  seed_status worker:feat/ci-mixed 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_runs '{"workflow_runs":[
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:02Z","status":"completed","conclusion":"success"},
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:03Z","status":"completed","conclusion":"failure"}
  ]}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0].first_ci_green' <<<"$(store_rows)"
  [ "$output" = "false" ]
}

@test "first_ci_green: no Actions runs in the window is null, not false" {
  seed_dispatch feat/ci-none 1000
  seed_status worker:feat/ci-none 1500 pr_open "https://github.com/acme/widgets/pull/1"
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0].first_ci_green' <<<"$(store_rows)"
  [ "$output" = "null" ]
}

@test "unresolved_notes: counts threads with isResolved false" {
  seed_dispatch feat/notes 1000
  seed_status worker:feat/notes 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_threads '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
    {"isResolved":true},{"isResolved":false},{"isResolved":false}
  ]}}}}}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0].unresolved_notes' <<<"$(store_rows)"
  [ "$output" = "2" ]
}

@test "unresolved_notes: stays null when the first attempt to query it fails" {
  seed_dispatch feat/notes-fail 1000
  seed_status worker:feat/notes-fail 1500 pr_open "https://github.com/acme/widgets/pull/1"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
  chmod +x "$STUB_DIR/gh"
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0] | "\(.unresolved_notes) \(.last_query_ok)"' <<<"$(store_rows)"
  [ "$output" = "null false" ]
}

@test "reverted: null when the merge commit is absent from local history" {
  seed_dispatch feat/reverted-null 1000
  seed_status worker:feat/reverted-null 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view '{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:05Z","mergeCommit":{"oid":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},"commits":[],"reviews":[]}'
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0].reverted' <<<"$(store_rows)"
  [ "$output" = "null" ]
}

@test "reverted: false when the merge commit is present locally with no revert" {
  git commit -q --allow-empty -m base
  sha="$(git rev-parse HEAD)"
  seed_dispatch feat/reverted-false 1000
  seed_status worker:feat/reverted-false 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view "$(jq -nc --arg s "$sha" '{state:"MERGED", closedAt:null, mergedAt:"1970-01-01T00:00:05Z", mergeCommit:{oid:$s}, commits:[], reviews:[]}')"
  run run_crew rate
  [ "$status" -eq 0 ]
  run jq -r '.[0].reverted' <<<"$(store_rows)"
  [ "$output" = "false" ]
}

@test "outcome: full precedence — merged, running, pr_open, failed, incomplete" {
  seed_dispatch outcome-merged 1000
  seed_status worker:outcome-merged 1500 pr_open "https://github.com/acme/widgets/pull/1"
  set_view '{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:05Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}'

  seed_dispatch outcome-running 2000
  seed_status worker:outcome-running 2500 working

  seed_dispatch outcome-pr_open 3000
  seed_status worker:outcome-pr_open 3500 pr_open "https://example.com/pr/9"

  seed_dispatch outcome-failed 4000
  seed_status worker:outcome-failed 4500 done

  seed_dispatch outcome-incomplete 5000
  seed_status worker:outcome-incomplete 5500 exited

  run run_crew rate
  [ "$status" -eq 0 ]
  rows="$(store_rows)"
  run jq -r 'map(select(.branch=="outcome-merged"))[0].outcome' <<<"$rows"
  [ "$output" = "merged" ]
  run jq -r 'map(select(.branch=="outcome-running"))[0].outcome' <<<"$rows"
  [ "$output" = "running" ]
  run jq -r 'map(select(.branch=="outcome-pr_open"))[0].outcome' <<<"$rows"
  [ "$output" = "pr_open" ]
  run jq -r 'map(select(.branch=="outcome-failed"))[0].outcome' <<<"$rows"
  [ "$output" = "failed" ]
  run jq -r 'map(select(.branch=="outcome-incomplete"))[0].outcome' <<<"$rows"
  [ "$output" = "incomplete" ]
}

@test "cost: a known model's proxy is weight times wall clock; kimi-k3-high is null" {
  seed_dispatch cost-opus 1000 claude opus deep
  seed_status worker:cost-opus 601000 done
  seed_dispatch cost-kimi 1000 cursor kimi-k3-high deep
  seed_status worker:cost-kimi 601000 done
  run run_crew rate
  [ "$status" -eq 0 ]
  rows="$(store_rows)"
  run jq -r 'map(select(.branch=="cost-opus"))[0] | "\(.cost_class) \(.cost_proxy)"' <<<"$rows"
  [ "$output" = "premium 2400000" ]
  run jq -r 'map(select(.branch=="cost-kimi"))[0] | "\(.cost_class) \(.cost_proxy)"' <<<"$rows"
  [ "$output" = "null null" ]
}

@test "--report --json: aggregates carry {value,k,n}; raw counts stay plain numbers" {
  mkdir -p "$XDG_DATA_HOME/crew"
  cat >"$XDG_DATA_HOME/crew/ratings.jsonl" <<'EOF'
{"run_id":"g1","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"time_to_pr_ms":100,"pr_state":"MERGED","time_to_merge_ms":200,"rework_count":0,"review_high":null,"review_mode":null,"review_rounds":0,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":100,"swept_at":1}
{"run_id":"g2","engine":"claude","model":"opus","tier":"deep","outcome":"pr_open","reached_pr":true,"time_to_pr_ms":150,"pr_state":"OPEN","time_to_merge_ms":null,"rework_count":1,"review_high":null,"review_mode":null,"review_rounds":1,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":null,"unresolved_notes":1,"reverted":null,"cost_proxy":300,"swept_at":1}
EOF
  run run_crew rate --report --json
  [ "$status" -eq 0 ]
  run jq -e '.[0].merge_pct.k == 1 and .[0].merge_pct.n == 2 and (.[0].inc|type) == "number"' <<<"$output"
  [ "$status" -eq 0 ]
}

# ---------------------------------------------------------------------------
# crew watch
# ---------------------------------------------------------------------------

@test "crew watch: a metrics-addressed message does not wake the watch" {
  CREW_ID=c1 run_crew msg worker "metrics:c1" '{"tier":"deep"}'
  CREW_ID=c1 run --separate-stderr run_crew watch --since 0 --timeout 1 --interval 1
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}
