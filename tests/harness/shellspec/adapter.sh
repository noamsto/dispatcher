#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly root
readonly manifest="$root/tests/harness/manifest.tsv"
readonly result="$root/tests/harness/bench/case-result.sh"
readonly runner="$root/tests/harness/shellspec/run.sh"
failures=0

"$result" --header
while IFS=$'\t' read -r case_id source_file _source_line family _rest; do
  [[ $case_id == case_id ]] && continue
  if ! "$result" "$case_id" "$source_file" "$family" -- \
    "$runner" --example "$case_id"; then
    failures=$((failures + 1))
  fi
done <"$manifest"

((failures == 0))
