#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

readonly result_header='harness	mode	rep	case_id	file	family	status	wall_ms	cpu_user_ms	cpu_sys_ms'
readonly metadata_header='harness	mode	rep	revision	timestamp	load_1	nproc	taskset_mask'
readonly summary_header='harness	mode	file	family	samples	cases	failures	wall_median_ms	wall_iqr_ms	cpu_user_median_ms	cpu_user_iqr_ms	cpu_sys_median_ms	cpu_sys_iqr_ms'

usage() {
  cat >&2 <<EOF
usage: $0 --harness <name> --mode <name> --adapter <path> [--repetitions <count>] [--cpu-set <list>] [--output <directory>]
       $0 --self-test
EOF
  exit 2
}

expand_cpu_list() {
  awk -v list="$1" 'BEGIN {
    count = split(list, parts, ",")
    for (i = 1; i <= count; i++) {
      if (parts[i] ~ /^[0-9]+$/) {
        print parts[i]
      } else if (parts[i] ~ /^[0-9]+-[0-9]+$/) {
        split(parts[i], limits, "-")
        for (cpu = limits[1]; cpu <= limits[2]; cpu++) print cpu
      } else {
        exit 1
      }
    }
  }'
}

default_cpu_set() {
  local allowed
  allowed=$(awk '$1 == "Cpus_allowed_list:" { print $2 }' /proc/self/status)
  expand_cpu_list "$allowed" | awk 'NR <= 4 { values[NR] = $1 } END {
    if (NR < 4) exit 1
    printf "%s,%s,%s,%s\n", values[1], values[2], values[3], values[4]
  }'
}

validate_cpu_set() {
  local cpu_set=$1
  local expanded count effective_list effective unique
  expanded=$(expand_cpu_list "$cpu_set") || return 1
  count=$(wc -l <<<"$expanded")
  unique=$(sort -n -u <<<"$expanded" | wc -l)
  [[ $count == 4 && $unique == 4 ]] || return 1
  effective_list=$(taskset -c "$cpu_set" bash -c "awk '\$1 == \"Cpus_allowed_list:\" { print \$2 }' /proc/self/status" 2>/dev/null) || return 1
  effective=$(expand_cpu_list "$effective_list" | sort -n -u) || return 1
  [[ $(sort -n -u <<<"$expanded") == "$effective" ]]
}

validate_result_file() {
  local file=$1 harness=$2 mode=$3 repetition=$4
  awk -F '\t' -v header="$result_header" -v harness="$harness" -v mode="$mode" -v repetition="$repetition" '
    NR == 1 { if ($0 != header) exit 1; next }
    NF != 10 { exit 1 }
    $1 != harness || $2 != mode || $3 != repetition || $4 == "" || $5 == "" || $6 == "" { exit 1 }
    $7 !~ /^[0-9]+$/ || $8 !~ /^[0-9]+$/ || $9 !~ /^[0-9]+$/ || $10 !~ /^[0-9]+$/ { exit 1 }
    seen[$4]++ { exit 1 }
    END { if (NR == 1) exit 1 }
  ' "$file"
}

validate_repetitions() {
  local file=$1 repetitions=$2
  awk -F '\t' -v repetitions="$repetitions" '
    NR == 1 { next }
    {
      count[$4]++
      identity = $5 FS $6
      if ($4 in identities && identities[$4] != identity) exit 1
      identities[$4] = identity
    }
    END {
      for (case_id in count) if (count[case_id] != repetitions) exit 1
    }
  ' "$file"
}

aggregate() {
  local results=$1 summary=$2
  awk -F '\t' -v OFS='\t' -v header="$summary_header" '
    function sorted(list, values,    n, i, j, item) {
      n = split(list, values, ",")
      for (i = 2; i <= n; i++) {
        item = values[i] + 0
        j = i - 1
        while (j >= 1 && values[j] + 0 > item) {
          values[j + 1] = values[j]
          j--
        }
        values[j + 1] = item
      }
      return n
    }
    function median(values, first, last,    span, middle) {
      span = last - first + 1
      middle = int(span / 2)
      if (span % 2) return values[first + middle]
      return (values[first + middle - 1] + values[first + middle]) / 2
    }
    function stats(list,    values, n, overall, lower_last, upper_first, q1, q3) {
      delete values
      n = sorted(list, values)
      overall = median(values, 1, n)
      if (n == 1) return sprintf("%.6f\t0.000000", overall)
      lower_last = int(n / 2)
      upper_first = int((n + 1) / 2) + 1
      q1 = median(values, 1, lower_last)
      q3 = median(values, upper_first, n)
      return sprintf("%.6f\t%.6f", overall, q3 - q1)
    }
    NR == 1 { next }
    {
      key = $1 SUBSEP $2 SUBSEP $5 SUBSEP $6
      if (!(key in seen_group)) {
        seen_group[key] = 1
        order[++groups] = key
        harness[key] = $1
        mode[key] = $2
        source[key] = $5
        family[key] = $6
      }
      samples[key]++
      if (!((key SUBSEP $4) in seen_case)) {
        seen_case[key SUBSEP $4] = 1
        cases[key]++
      }
      if ($7 != 0) failures[key]++
      wall[key] = wall[key] (wall[key] == "" ? "" : ",") $8
      user[key] = user[key] (user[key] == "" ? "" : ",") $9
      sys_cpu[key] = sys_cpu[key] (sys_cpu[key] == "" ? "" : ",") $10
    }
    END {
      print header
      for (i = 1; i <= groups; i++) {
        key = order[i]
        print harness[key], mode[key], source[key], family[key], samples[key], cases[key], failures[key] + 0,
          stats(wall[key]), stats(user[key]), stats(sys_cpu[key])
      }
    }
  ' "$results" >"$summary"
}

run_benchmark() {
  local harness=$1 mode=$2 adapter=$3 repetitions=$4 cpu_set=$5 output=$6
  local revision host_nproc affinity_mask repetition timestamp load repetition_file failures adapter_status
  local adapter_failures=0

  [[ $harness != *$'\t'* && $harness != *$'\n'* && -n $harness && $mode != *$'\t'* && $mode != *$'\n'* && -n $mode ]] || {
    echo "harness-bench: harness and mode must be non-empty TSV-safe text" >&2
    return 2
  }
  [[ -x $adapter ]] || {
    echo "harness-bench: adapter is not executable: $adapter" >&2
    return 2
  }
  [[ $repetitions =~ ^[1-9][0-9]*$ ]] || {
    echo "harness-bench: repetitions must be a positive integer" >&2
    return 2
  }
  validate_cpu_set "$cpu_set" || {
    echo "harness-bench: CPU set must name four distinct usable CPUs: $cpu_set" >&2
    return 2
  }
  mkdir -p "$output"
  [[ ! -e $output/results.tsv && ! -e $output/metadata.tsv && ! -e $output/summary.tsv ]] || {
    echo "harness-bench: output already contains benchmark results: $output" >&2
    return 2
  }

  revision=$(git rev-parse HEAD)
  host_nproc=$(nproc)
  affinity_mask=$(taskset -c "$cpu_set" bash -c "awk '\$1 == \"Cpus_allowed:\" { print \$2 }' /proc/self/status")
  printf '%s\n' "$result_header" >"$output/results.tsv"
  printf '%s\n' "$metadata_header" >"$output/metadata.tsv"
  for ((repetition = 1; repetition <= repetitions; repetition++)); do
    timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    read -r load _ </proc/loadavg
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$harness" "$mode" "$repetition" "$revision" "$timestamp" "$load" "$host_nproc" "$affinity_mask" \
      >>"$output/metadata.tsv"
    repetition_file="$output/repetition-$repetition.tsv"
    set +e
    HARNESS_BENCH_HARNESS=$harness \
      HARNESS_BENCH_MODE=$mode \
      HARNESS_BENCH_REP=$repetition \
      taskset -c "$cpu_set" "$adapter" >"$repetition_file"
    adapter_status=$?
    set -e
    if ((adapter_status != 0)); then
      adapter_failures=$((adapter_failures + 1))
    fi
    validate_result_file "$repetition_file" "$harness" "$mode" "$repetition" || {
      echo "harness-bench: adapter emitted an invalid result for repetition $repetition" >&2
      return 1
    }
    tail -n +2 "$repetition_file" >>"$output/results.tsv"
  done

  validate_repetitions "$output/results.tsv" "$repetitions" || {
    echo "harness-bench: adapter case identities differ between repetitions" >&2
    return 1
  }
  aggregate "$output/results.tsv" "$output/summary.tsv"
  failures=$(awk -F '\t' 'NR > 1 && $7 != 0 { count++ } END { print count + 0 }' "$output/results.tsv")
  printf 'harness-bench: wrote %s, %s, and %s (CPU set %s)\n' \
    "$output/results.tsv" "$output/metadata.tsv" "$output/summary.tsv" "$cpu_set" >&2
  ((failures == 0)) || {
    echo "harness-bench: $failures case measurements failed" >&2
    return 1
  }
  ((adapter_failures == 0)) || {
    echo "harness-bench: adapter failed in $adapter_failures repetitions" >&2
    return 1
  }
}

self_test() {
  local test_dir output cpu_set wrapper_row
  test_dir=$(mktemp -d)
  output=$test_dir/result
  cpu_set=$(default_cpu_set) || {
    echo "harness-bench: self-test needs four available CPUs" >&2
    return 1
  }

  run_benchmark fixture deterministic tests/harness/bench/fixture-runner.sh 4 "$cpu_set" "$output"
  awk -F '\t' '
    NR == 1 { next }
    NR == 2 {
      if ($1 != "fixture" || $2 != "deterministic" || $3 != "tests/harness/bench/fixture.bats" || $4 != "fixture" ||
          $5 != 8 || $6 != 2 || $7 != 0 || $8 != "4500.000000" || $9 != "4000.000000" ||
          $10 != "450.000000" || $11 != "400.000000" || $12 != "45.000000" || $13 != "40.000000") exit 1
      found = 1
    }
    END { if (!found || NR != 2) exit 1 }
  ' "$output/summary.tsv" || {
    echo "harness-bench: deterministic aggregation self-test failed" >&2
    return 1
  }
  awk -F '\t' '
    NR == 1 {
      if ($0 != "harness\tmode\trep\trevision\ttimestamp\tload_1\tnproc\ttaskset_mask") exit 1
      next
    }
    NF != 8 || $1 != "fixture" || $2 != "deterministic" || $3 != NR - 1 ||
      $4 !~ /^[0-9a-f]+$/ || $5 !~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T/ ||
      $6 !~ /^[0-9]+([.][0-9]+)?$/ || $7 !~ /^[1-9][0-9]*$/ || $8 !~ /^[0-9a-f,]+$/ { exit 1 }
    END { if (NR != 5) exit 1 }
  ' "$output/metadata.tsv" || {
    echo "harness-bench: repetition metadata self-test failed" >&2
    return 1
  }

  wrapper_row=$(
    HARNESS_BENCH_HARNESS=fixture HARNESS_BENCH_MODE=deterministic HARNESS_BENCH_REP=1 \
      tests/harness/bench/case-result.sh fixture-case fixture.bats fixture -- true
  )
  awk -F '\t' 'NF != 10 || $1 != "fixture" || $2 != "deterministic" || $4 != "fixture-case" || $7 != 0 ||
    $8 !~ /^[0-9]+$/ || $9 !~ /^[0-9]+$/ || $10 !~ /^[0-9]+$/ { exit 1 }' <<<"$wrapper_row" || {
    echo "harness-bench: per-case wrapper self-test failed" >&2
    return 1
  }
  wrapper_row=$(
    HARNESS_BENCH_HARNESS=fixture HARNESS_BENCH_MODE=deterministic HARNESS_BENCH_REP=1 \
      tests/harness/bench/case-result.sh fixture-case fixture.bats fixture -- bash -c 'exit 7'
  )
  [[ $(cut -f7 <<<"$wrapper_row") == 7 ]] || {
    echo "harness-bench: per-case wrapper lost the command status" >&2
    return 1
  }
  echo "harness-bench: self-test passed" >&2
}

if [[ ${1:-} == --self-test ]]; then
  (($# == 1)) || usage
  self_test
  exit
fi

harness=
mode=
adapter=
repetitions=5
cpu_set=
output=result-harness-bench
while (($#)); do
  case $1 in
  --harness | --mode | --adapter | --repetitions | --cpu-set | --output)
    (($# >= 2)) || usage
    case $1 in
    --harness) harness=$2 ;;
    --mode) mode=$2 ;;
    --adapter) adapter=$2 ;;
    --repetitions) repetitions=$2 ;;
    --cpu-set) cpu_set=$2 ;;
    --output) output=$2 ;;
    esac
    shift 2
    ;;
  *) usage ;;
  esac
done

[[ -n $harness && -n $mode && -n $adapter ]] || usage
if [[ -z $cpu_set ]]; then
  cpu_set=$(default_cpu_set) || {
    echo "harness-bench: four available CPUs are required" >&2
    exit 2
  }
fi
run_benchmark "$harness" "$mode" "$adapter" "$repetitions" "$cpu_set" "$output"
