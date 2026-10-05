#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

actual="$(mktemp)"
trap 'gtrash put "$actual"' EXIT

python3 tests/harness/profile/parse_trace.py \
  --trace tests/harness/profile/overlap.trace \
  --repo-root /repo \
  --case-id fixture-overlap \
  --source-file tests/fixture.bats \
  --family fixture \
  --status 0 \
  --wall 14 \
  --user 1 \
  --sys 0.5 \
  --header >"$actual"

diff -u tests/harness/profile/overlap.expected.tsv "$actual"
