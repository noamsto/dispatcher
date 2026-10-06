#!/usr/bin/env bash
# Aggregate scripts/bats-timing.sh TSV (file, status, ms, test name) into
# shard weights: file, family, filter, weight_ms.
# The timing phase is labeled tests/secret-read-guard.bats[timing].
set -euo pipefail

data=$(awk -F '\t' -v OFS='\t' '
  function family_of(name,    i) {
    i = index(name, ":")
    if (i == 0) return "*"
    return substr(name, 1, i - 1)
  }
  NF < 4 {
    printf "bats-shard-weights: invalid row %d\n", NR > "/dev/stderr"
    exit 1
  }
  {
    file = $1
    status = $2
    ms = $3
    name = $4
    for (i = 5; i <= NF; i++) name = name "\t" $i
    if (status == "not-ok") {
      printf "bats-shard-weights: not-ok: %s / %s\n", file, name > "/dev/stderr"
      exit 1
    }
    if (status != "ok" && status != "skip") {
      printf "bats-shard-weights: bad status at row %d\n", NR > "/dev/stderr"
      exit 1
    }
    if (ms !~ /^[0-9]+$/) {
      printf "bats-shard-weights: bad ms at row %d\n", NR > "/dev/stderr"
      exit 1
    }
    if (index(name, "\t")) {
      printf "bats-shard-weights: unsafe test name at row %d\n", NR > "/dev/stderr"
      exit 1
    }
    filter = "!timing"
    if (file ~ /\[timing\]$/) {
      sub(/\[timing\]$/, "", file)
      filter = "timing"
    }
    if (file ~ /(^|\/)module\.bats$/) next
    family = family_of(name)
    key = file SUBSEP family SUBSEP filter
    weight[key] += ms
    seen[key] = 1
  }
  END {
    for (key in seen) {
      split(key, fields, SUBSEP)
      print fields[1], fields[2], fields[3], weight[key]
    }
  }
')

printf 'file\tfamily\tfilter\tweight_ms\n'
if [[ -n $data ]]; then
  printf '%s\n' "$data" | LC_ALL=C sort -t $'\t' -k1,1 -k2,2 -k3,3
fi
