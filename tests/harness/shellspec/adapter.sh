#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly root
readonly manifest="$root/tests/harness/manifest.tsv"
readonly result="$root/tests/harness/bench/case-result.sh"
readonly runner="$root/tests/harness/shellspec/run.sh"
failures=0
# shellcheck source=/dev/null
source "$root/tests/harness/ensure-crew-go.sh"
trap cleanup_crew_go_bin EXIT
ensure_crew_go_bin

"$result" --header
while IFS=$'\t' read -r case_id source_file _source_line family _rest; do
  [[ $case_id == case_id ]] && continue
  # case-result.sh exits 0 even when the measured command fails; the case
  # status lives in column 7 of the row.
  row=$("$result" "$case_id" "$source_file" "$family" -- \
    "$runner" --example "$case_id")
  printf '%s\n' "$row"
  IFS=$'\t' read -r _ _ _ _ _ _ case_status _ <<<"$row"
  [[ ${case_status:-1} == 0 ]] || failures=$((failures + 1))
done <"$manifest"

((failures == 0))
