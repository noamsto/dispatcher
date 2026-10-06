#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <compatibility|tuned> <source-file> <test-name>" >&2
  exit 2
}

(($# == 3)) || usage
readonly mode=$1
readonly source_file=$2
readonly test_name=$3

case $mode in
compatibility) extra_args=() ;;
tuned) extra_args=(--no-tempdir-cleanup) ;;
*) usage ;;
esac

# Bats accepts a regular expression rather than an exact-name option. Escape
# every ERE metacharacter so manifest names remain literal selectors.
filter=$(printf '%s\n' "$test_name" | sed 's/[][(){}.^$*+?|\\]/\\&/g')
readonly filter

set +e
output=$(bats --jobs 16 "${extra_args[@]}" --formatter tap --filter "^${filter}$" "$source_file" 2>&1)
status=$?
set -e
printf '%s\n' "$output"
((status == 0)) || exit "$status"

awk -v expected="ok 1 $test_name" '
  $0 == "1..1" { plans++ }
  $0 == expected { passes++ }
  END { if (plans != 1 || passes != 1) exit 1 }
' <<<"$output"
