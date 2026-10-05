#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

actual="$(mktemp)"
trap 'gtrash put "$actual"' EXIT

run_fixture() {
  local fixture=$1 wall=$2
  python3 tests/harness/profile/parse_trace.py \
    --trace "tests/harness/profile/${fixture}.trace" \
    --repo-root /repo \
    --case-id "fixture-${fixture}" \
    --source-file tests/fixture.bats \
    --family fixture \
    --status 0 \
    --wall "$wall" \
    --user 1 \
    --sys 0.5 \
    --header >"$actual"
  diff -u "tests/harness/profile/${fixture}.expected.tsv" "$actual"
}

run_fixture overlap 14
run_fixture split 8
