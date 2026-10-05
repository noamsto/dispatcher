#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly root
readonly manifest="$root/tests/harness/manifest.tsv"
readonly result="$root/tests/harness/bench/case-result.sh"
readonly runner="$root/tests/harness/pytest/run.sh"
failures=0
rows=0

"$result" --header
while IFS=$'\t' read -r case_id source_file _ family _ _; do
  [[ $case_id == case_id ]] && continue
  rows=$((rows + 1))
  if ! "$result" "$case_id" "$source_file" "$family" -- \
    "$runner" case "$case_id"; then
    failures=$((failures + 1))
  fi
done <"$manifest"

((rows == 26 && failures == 0))
