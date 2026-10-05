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
      go test -C "$module_dir" -count=1 -run "^TestManifest/${case_id}$" .
  )
  status=$?
  set -e
  printf '%s\n' "$row"
  ((status == 0)) || failures=$((failures + 1))
done <"$manifest"

((failures == 0))
