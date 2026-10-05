#!/usr/bin/env bash
set -euo pipefail

module_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$module_dir/../../.." && pwd)
manifest="$repo_root/tests/harness/manifest.tsv"
result="$repo_root/tests/harness/bench/case-result.sh"

"$result" --header
failures=0
while IFS=$'\t' read -r case_id source_file _ family _; do
  [[ $case_id == case_id ]] && continue
  set +e
  row=$(
    "$result" "$case_id" "$source_file" "$family" -- \
      env GO_HARNESS_CASE="$case_id" \
      go test -C "$module_dir" -count=1 -run "^TestManifest/^${case_id}$" .
  )
  set -e
  printf '%s\n' "$row"
  # case-result.sh exits 0 even when the measured command fails; the case
  # status lives in column 7 of the row.
  IFS=$'\t' read -r _ _ _ _ _ _ case_status _ <<<"$row"
  [[ ${case_status:-1} == 0 ]] || failures=$((failures + 1))
done <"$manifest"

((failures == 0))
