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
  # case-result.sh exits 0 even when the measured command fails; the case
  # status lives in column 7 of the row.
  row=$("$result" "$case_id" "$source_file" "$family" -- \
    "$runner" case "$case_id")
  printf '%s\n' "$row"
  IFS=$'\t' read -r _ _ _ _ _ _ case_status _ <<<"$row"
  [[ ${case_status:-1} == 0 ]] || failures=$((failures + 1))
done <"$manifest"

expected=$(($(wc -l <"$manifest") - 1))
((rows == expected && failures == 0))
