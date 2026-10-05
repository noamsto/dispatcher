#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

readonly result_wrapper=tests/harness/bench/case-result.sh
"$result_wrapper" --header

emit() {
  local case_id=$1
  local wall=$2
  local user system

  user=$((wall / 10))
  system=$((wall / 100))
  printf '%s\t%s\t%s\t%s\t%s\t%s\t0\t%s\t%s\t%s\n' \
    "$HARNESS_BENCH_HARNESS" "$HARNESS_BENCH_MODE" "$HARNESS_BENCH_REP" \
    "$case_id" tests/harness/bench/fixture.bats fixture "$wall" "$user" "$system"
}

emit fixture-a "$((HARNESS_BENCH_REP * 2000 - 1000))"
emit fixture-b "$((HARNESS_BENCH_REP * 2000))"
