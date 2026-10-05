#!/usr/bin/env bash
set -euo pipefail

readonly header='harness	mode	rep	case_id	file	family	status	wall_ms	cpu_user_ms	cpu_sys_ms'

usage() {
  echo "usage: $0 --header | <case-id> <source-file> <family> -- <command> [args...]" >&2
  exit 2
}

if [[ ${1:-} == --header ]]; then
  (($# == 1)) || usage
  printf '%s\n' "$header"
  exit 0
fi

(($# >= 5)) || usage
readonly case_id=$1
readonly source_file=$2
readonly family=$3
shift 3
[[ $1 == -- ]] || usage
shift
(($# > 0)) || usage

readonly required=(
  HARNESS_BENCH_HARNESS
  HARNESS_BENCH_MODE
  HARNESS_BENCH_REP
)
for name in "${required[@]}"; do
  [[ -n ${!name:-} ]] || {
    echo "case-result: missing $name" >&2
    exit 2
  }
done

for value in "$HARNESS_BENCH_HARNESS" "$HARNESS_BENCH_MODE" "$case_id" "$source_file" "$family"; do
  [[ $value != *$'\t'* && $value != *$'\n'* ]] || {
    echo "case-result: metadata must not contain tabs or newlines" >&2
    exit 2
  }
done

time_bin=$(type -P time || true)
readonly time_bin
[[ -n $time_bin ]] || {
  echo "case-result: GNU time is required" >&2
  exit 2
}

set +e
timing=$("$time_bin" -q -f '%e\t%U\t%S' -o /dev/fd/3 -- "$@" 3>&1 >&2)
status=$?
set -e

IFS=$'\t' read -r wall_seconds user_seconds system_seconds <<<"$timing"
[[ $wall_seconds =~ ^[0-9]+([.][0-9]+)?$ && $user_seconds =~ ^[0-9]+([.][0-9]+)?$ && $system_seconds =~ ^[0-9]+([.][0-9]+)?$ ]] || {
  echo "case-result: GNU time returned an invalid measurement" >&2
  exit 2
}

read -r wall_ms user_ms system_ms < <(
  awk -v wall="$wall_seconds" -v user="$user_seconds" -v sys="$system_seconds" \
    'BEGIN { printf "%.0f %.0f %.0f\n", wall * 1000, user * 1000, sys * 1000 }'
)
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
  "$HARNESS_BENCH_HARNESS" "$HARNESS_BENCH_MODE" "$HARNESS_BENCH_REP" \
  "$case_id" "$source_file" "$family" "$status" "$wall_ms" "$user_ms" "$system_ms"
