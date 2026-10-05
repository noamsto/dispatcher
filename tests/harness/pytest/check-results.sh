#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly root
readonly results=${1:?usage: check-results.sh RESULT_TSV}
readonly manifest="$root/tests/harness/manifest.tsv"

awk -F '\t' '
  NR == FNR { if (FNR > 1) expected[$1] = 1; next }
  FNR == 1 {
    if ($0 != "harness\tmode\trep\tcase_id\tfile\tfamily\tstatus\twall_ms\tcpu_user_ms\tcpu_sys_ms") exit 1
    next
  }
  NF != 10 || $1 != "pytest" || $2 != "prototype" || $7 != 0 || !($4 in expected) || seen[$4]++ { exit 1 }
  END {
    if (length(seen) != 31 || length(expected) != 31) exit 1
    for (id in expected) if (!(id in seen)) exit 1
  }
' "$manifest" "$results"
