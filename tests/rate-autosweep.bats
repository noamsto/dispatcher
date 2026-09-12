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
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  # No test may make a real network call — gh is stubbed on PATH for every
  # test, even ones that never expect to invoke it.
  stub_gh
  unset CREW_ID
}

teardown() {
  teardown_repo
}

# ---------------------------------------------------------------------------
# Copied from rate.bats — that file defines these at its own top level, not
# in tests/helpers.bash, so this file does not inherit them.
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

# await_sweep — block until a forked async sweep has finished. The child is a
# grandchild of the test shell, so `wait` cannot reach it; poll its lock
# instead. `_rate_autosweep` runs `_lock_acquire` → `bash "$0" rate` (writes
# the store) → `_lock_release`, so holding the lock means started and dropping
# it means exited. Returns 1 rather than hanging if the child never appears.
await_sweep() {
  local i started="" had_store=""
  # A store left by an earlier sweep in the same test is not evidence that
  # this one started, so only trust that sentinel when the store is new.
  [ -f "$XDG_DATA_HOME/crew/ratings.jsonl" ] && had_store=1
  # Phase 1 — started. Both sentinels are needed: the lock does not exist
  # until the child reaches `_lock_acquire`, and a child that finished before
  # the first poll leaves only the store behind.
  for ((i = 0; i < 100; i++)); do
    if [ -d "$XDG_DATA_HOME/crew/ratings.sweep.lock.d" ]; then
      started=1
      break
    fi
    if [ -z "$had_store" ] && [ -f "$XDG_DATA_HOME/crew/ratings.jsonl" ]; then
      started=1
      break
    fi
    sleep 0.1
  done
  [ -n "$started" ] || return 1
  # Phase 2 — finished. The store appears before the lock drops, so returning
  # on the store alone would leave a live child racing teardown.
  for ((i = 0; i < 100; i++)); do
    [ -d "$XDG_DATA_HOME/crew/ratings.sweep.lock.d" ] || return 0
    sleep 0.1
  done
  return 1
}

# ---------------------------------------------------------------------------
# The CREW_RATE_AUTOSWEEP tests
# ---------------------------------------------------------------------------

@test "autosweep: sync reap fills the store, one row per run" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  [ "$status" -eq 0 ]
  [ -f "$XDG_DATA_HOME/crew/ratings.jsonl" ]
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]
}

@test "autosweep: a second reap leaves the store byte-identical" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  [ "$status" -eq 0 ]
  cp "$XDG_DATA_HOME/crew/ratings.jsonl" "$BATS_TEST_TMPDIR/before.jsonl"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  [ "$status" -eq 0 ]
  cmp "$BATS_TEST_TMPDIR/before.jsonl" "$XDG_DATA_HOME/crew/ratings.jsonl"
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]
}

@test "autosweep: --dry-run runs no sweep and says so" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [ ! -e "$XDG_DATA_HOME/crew/ratings.jsonl" ]
  [[ "$output" == *"dry run"* ]]
}

@test "autosweep: CREW_RATE_AUTOSWEEP=0 runs no sweep" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=0 run run_crew reap
  [ "$status" -eq 0 ]
  [ ! -e "$XDG_DATA_HOME/crew/ratings.jsonl" ]
}

@test "autosweep: a failing sweep does not change reap's exit status" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  # The store path is a DIRECTORY, so rate's final `… | dd … >>"$store"`
  # cannot open its target, the pipeline fails under pipefail, and the
  # `|| true` in _rate_autosweep absorbs the failure.
  mkdir -p "$XDG_DATA_HOME/crew/ratings.jsonl"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  [ "$status" -eq 0 ]
  # No ratings row was written: the store path is still the directory we made
  # it, never a regular file, because dd could not open it.
  [ ! -f "$XDG_DATA_HOME/crew/ratings.jsonl" ]
  # The check above holds whether the sweep failed or never ran, so pin that
  # it ran: reaching the gh stub means it got to the reconcile before the
  # append killed it.
  [ -s "$STUB_LOG" ]
}

@test "autosweep: the hook fires when \$XDG_DATA_HOME/crew does not exist" {
  # Seed the pair FIRST — reap's bus guard `[ -f "$log" ] || exit 0` tests
  # <git-common-dir>/crew/events.jsonl, a different path entirely, and sits
  # above the hook; without a seeded event reap exits there and this test
  # would pass vacuously.
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  [ ! -e "$XDG_DATA_HOME/crew" ]
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  [ "$status" -eq 0 ]
  [ -f "$XDG_DATA_HOME/crew/ratings.jsonl" ]
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]
}

@test "autosweep: no reap candidates, and the sweep still runs" {
  # pr_open is not one of reap's terminal states (done/failed/exited), so
  # there are no candidates and reap takes its "nothing done to reap" exit —
  # which sits below the hook.
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  [ "$status" -eq 0 ]
  # Assert the row count, not mere file existence: `>>` creates the file even
  # for an empty pipeline.
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]
}

@test "autosweep: a first sweep ingests every historical dispatch" {
  seed_dispatch feat/h1 1000
  seed_status worker:feat/h1 1500 pr_open "https://github.com/acme/widgets/pull/1"
  seed_dispatch feat/h2 2000
  seed_status worker:feat/h2 2500 pr_open "https://github.com/acme/widgets/pull/2"
  seed_dispatch feat/h3 3000
  seed_status worker:feat/h3 3500 pr_open "https://github.com/acme/widgets/pull/3"
  seed_dispatch feat/h4 4000
  seed_status worker:feat/h4 4500 pr_open "https://github.com/acme/widgets/pull/4"
  seed_dispatch feat/h5 5000
  seed_status worker:feat/h5 5500 pr_open "https://github.com/acme/widgets/pull/5"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  [ "$status" -eq 0 ]
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 5 ]
}

@test "autosweep: two overlapping sweeps collapse to one" {
  mkdir -p "$XDG_DATA_HOME/crew"
  sleep 100 &
  live_pid=$!
  mkdir "$XDG_DATA_HOME/crew/ratings.sweep.lock.d"
  printf '%s\n' "$live_pid" >"$XDG_DATA_HOME/crew/ratings.sweep.lock.d/pid"

  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=sync run run_crew reap
  kill "$live_pid" 2>/dev/null || true

  [ "$status" -eq 0 ]
  [ -d "$XDG_DATA_HOME/crew/ratings.sweep.lock.d" ]
  [ "$(cat "$XDG_DATA_HOME/crew/ratings.sweep.lock.d/pid")" = "$live_pid" ]
  [ ! -e "$XDG_DATA_HOME/crew/ratings.jsonl" ]
  # Without this the test also passes when the hook never ran at all.
  [[ "$output" == *"a ratings sweep is already running — skipped"* ]]
}

@test "autosweep: async mode leaves stdout and exit status unchanged" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=0 run run_crew reap
  disabled_output="$output"
  disabled_status="$status"

  CREW_RATE_AUTOSWEEP=1 run run_crew reap
  async_output="$output"
  async_status="$status"

  [ "$disabled_output" = "$async_output" ]
  [ "$disabled_status" -eq 0 ]
  [ "$async_status" -eq 0 ]

  await_sweep
}

@test "autosweep: the default is ON when the variable is unset" {
  unset CREW_RATE_AUTOSWEEP
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  run run_crew reap --dry-run
  [[ "$output" == *"dry run — skipping ratings sweep"* ]]
}

@test "autosweep: the flag and the fork compose" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=1 run run_crew reap
  [ "$status" -eq 0 ]
  await_sweep
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]
  [ ! -d "$XDG_DATA_HOME/crew/ratings.sweep.lock.d" ]
}

# Call-site guard, like the two bus-append guards in tests/adapters.bats
# (#55, #61). No behavioural test can see this: a bare `>>"$store"` fails
# identically to the `dd` form, so reverting it would leave the suite green.
# Hence a grep on the call site's shape.
@test "guard: the ratings store append goes through the atomic dd primitive" {
  CREW_SRC="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  appends="$(grep -nE '>>[[:space:]]*"?\$\{?store\}?"?' "$CREW_SRC" |
    grep -vE '^[0-9]+:[[:space:]]*#' || true)"
  # If this trips, the store grew a second append site — extend the guard
  # rather than deleting it.
  [ "$(printf '%s\n' "$appends" | wc -l)" -eq 1 ]
  [[ "$appends" == *"dd bs=1048576 iflag=fullblock status=none"* ]]
}

@test "autosweep: an unrecognised value warns on stderr and still sweeps" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  # `false` is the trap: the natural boolean spelling for "off".
  CREW_RATE_AUTOSWEEP=false run --separate-stderr run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [[ "$stderr" == *"CREW_RATE_AUTOSWEEP='false' is not 0, 1 or sync"* ]]
  # Took the default rather than disabling; --dry-run keeps this fork-free.
  [[ "$output" == *"dry run — skipping ratings sweep"* ]]
}

@test "autosweep: the three documented values produce no warning" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  for v in 0 1 sync; do
    CREW_RATE_AUTOSWEEP="$v" run --separate-stderr run_crew reap --dry-run
    [ "$status" -eq 0 ]
    [ -z "$stderr" ]
  done
  # And unset, the production default, is likewise silent.
  unset CREW_RATE_AUTOSWEEP
  run --separate-stderr run_crew reap --dry-run
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
}

@test "autosweep: two async sweeps in one test each complete (await_sweep is reusable)" {
  seed_dispatch feat/x 1000
  seed_status worker:feat/x 1500 pr_open "https://github.com/acme/widgets/pull/1"
  CREW_RATE_AUTOSWEEP=1 run run_crew reap
  [ "$status" -eq 0 ]
  await_sweep
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]

  # The second call is the point: the store now exists, so a sentinel that
  # accepted it unconditionally would return having waited for nothing.
  CREW_RATE_AUTOSWEEP=1 run run_crew reap
  [ "$status" -eq 0 ]
  await_sweep
  # Still one row: the sweep is idempotent, so this pass appends nothing.
  [ "$(jq -s 'length' "$XDG_DATA_HOME/crew/ratings.jsonl")" -eq 1 ]
  [ ! -d "$XDG_DATA_HOME/crew/ratings.sweep.lock.d" ]
}
