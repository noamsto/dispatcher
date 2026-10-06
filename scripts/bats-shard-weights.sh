#!/usr/bin/env bash
# Aggregate Bats timings into shard weights. With --run, download the JUnit
# artifacts emitted by CI and require each sample to contain the full !timing
# (file, test) inventory before averaging its case timings.
set -euo pipefail

usage() {
  printf 'usage: %s [--run RUN]...\n' "${0##*/}" >&2
  exit 2
}

aggregate() {
  awk -F '\t' -v OFS='\t' '
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
  ' | {
    printf 'file\tfamily\tfilter\tweight_ms\n'
    LC_ALL=C sort -t $'\t' -k1,1 -k2,2 -k3,3
  }
}

expected_inventory() {
  find tests -maxdepth 1 -name '*.bats' ! -name module.bats -print | LC_ALL=C sort | awk '
    FNR == 1 { file_tags = ""; test_tags = "" }
    /^# bats file_tags=/ { file_tags = $0; sub(/^# bats file_tags=/, "", file_tags); next }
    /^# bats test_tags=/ { test_tags = $0; sub(/^# bats test_tags=/, "", test_tags); next }
    /^@test / {
      if ($0 !~ /^@test ".*" \{[[:space:]]*$/) { exit 1 }
      tags = file_tags "," test_tags
      name = $0; sub(/^@test "/, "", name); sub(/" \{[[:space:]]*$/, "", name)
      if (tags !~ /(^|,)[[:space:]]*timing([[:space:]]*,|$)/) print FILENAME "\t" name
      test_tags = ""
    }
  '
}

runs=()
while (($#)); do
  case $1 in
    --run) (($# >= 2)) || usage; runs+=("$2"); shift 2 ;;
    *) usage ;;
  esac
done

if ((${#runs[@]} == 0)); then
  aggregate
  exit 0
fi

command -v gh >/dev/null || { echo 'bats-shard-weights: gh is required for --run' >&2; exit 1; }
command -v yq >/dev/null || { echo 'bats-shard-weights: yq is required for --run' >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
expected="$work/expected.tsv"
expected_inventory >"$expected"
case_samples="$work/cases.tsv"
: >"$case_samples"

for run in "${runs[@]}"; do
  sample="$work/$run"
  mkdir -p "$sample"
  for shard in 1 2 3 4; do
    gh run download "$run" -n "bats-timing-shard-$shard" --dir "$sample"
  done
  mapfile -t reports < <(find "$sample" -name '*.xml' -type f | LC_ALL=C sort)
  ((${#reports[@]})) || { echo "bats-shard-weights: run $run has no JUnit reports" >&2; exit 1; }
  actual="$sample/actual.tsv"
  : >"$actual"
  for report in "${reports[@]}"; do
    if yq -r '.. | select(has("+@failures") or has("+@errors")) | [ ."+@failures", ."+@errors" ] | @tsv' "$report" |
      awk -F '\t' '$1 != "0" || $2 != "0" { exit 1 }'; then :; else
      echo "bats-shard-weights: run $run includes failed tests" >&2; exit 1
    fi
    # shellcheck disable=SC2016 # yq expression deliberately contains $suite.
    yq -r '.. | select(has("testcase")) | . as $suite | $suite.testcase[] | [$suite."+@name", ."+@name", ."+@time"] | @tsv' "$report" |
      awk -F '\t' -v OFS='\t' '
        NF != 3 || $1 == "" || $2 == "" || $3 !~ /^[0-9]+(\.[0-9]+)?$/ { exit 1 }
        { print $1, $2, int(($3 * 1000) + 0.5) }
      ' >>"$actual" || { echo "bats-shard-weights: invalid JUnit report $report" >&2; exit 1; }
  done
  # JUnit names a suite by basename. Resolve it only after proving basenames
  # are unique in the source inventory.
  awk -F '\t' -v OFS='\t' '
    NR == FNR { base = $1; sub(/^.*\//, "", base); if (base in path && path[base] != $1) exit 2; path[base] = $1; next }
    !($1 in path) { exit 3 }
    { print path[$1], $2, $3 }
  ' "$expected" "$actual" | LC_ALL=C sort >"$sample/actual-paths.tsv" || {
    echo "bats-shard-weights: run $run names an unknown or ambiguous test file" >&2; exit 1
  }
  LC_ALL=C sort "$expected" >"$sample/expected-sorted.tsv"
  cut -f1,2 "$sample/actual-paths.tsv" | uniq -c | awk '$1 != 1 { exit 1 }' || {
    echo "bats-shard-weights: run $run duplicates a test case" >&2; exit 1
  }
  cut -f1,2 "$sample/actual-paths.tsv" >"$sample/actual-inventory.tsv"
  if ! diff -u "$sample/expected-sorted.tsv" "$sample/actual-inventory.tsv" >/dev/null; then
    echo "bats-shard-weights: run $run does not match the current !timing test inventory" >&2; exit 1
  fi
  awk -F '\t' -v OFS='\t' '{ print $1, "ok", $3, $2 }' "$sample/actual-paths.tsv" >>"$case_samples"
done

# Average each (file, test) across samples before accumulating family weights.
awk -F '\t' -v OFS='\t' '{ key = $1 SUBSEP $4; total[key] += $3; count[key]++; file[key] = $1; name[key] = $4 }
  END { for (key in total) print file[key], "ok", int((total[key] / count[key]) + 0.5), name[key] }' "$case_samples" | aggregate
