#!/usr/bin/env bash
set -euo pipefail

module_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$module_dir/../../.." && pwd)
manifest="$repo_root/tests/harness/manifest.tsv"
assertions="$repo_root/tests/harness/assertions.tsv"
source_file="$module_dir/harness_test.go"
output=$(mktemp)
trap 'gtrash put "$output" >/dev/null 2>&1 || true' EXIT

HARNESS_BENCH_HARNESS=go \
  HARNESS_BENCH_MODE=prototype \
  HARNESS_BENCH_REP=1 \
  "$module_dir/adapter.sh" >"$output"

awk -F '\t' '
  NR == FNR { if (FNR > 1) expected[$1]++; next }
  FNR == 1 {
    if ($0 != "harness\tmode\trep\tcase_id\tfile\tfamily\tstatus\twall_ms\tcpu_user_ms\tcpu_sys_ms") exit 1
    next
  }
  NF != 10 || $1 != "go" || $2 != "prototype" || $3 != 1 || $7 != 0 { exit 1 }
  !($4 in expected) || seen[$4]++ { exit 1 }
  END {
    if (length(expected) != 31 || length(seen) != 31) exit 1
    for (id in expected) if (!(id in seen)) exit 1
  }
' "$manifest" "$output"

while IFS=$'\t' read -r _ assert_id _; do
  [[ $assert_id == assert_id ]] && continue
  grep -Fq "\"$assert_id\"" "$source_file" || {
    echo "go harness: missing assertion metadata: $assert_id" >&2
    exit 1
  }
done <"$assertions"

echo "go harness: verified 31 result rows and assertion metadata" >&2
