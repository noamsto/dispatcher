#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

usage() {
  echo "usage: $0 <compatibility|tuned>" >&2
  exit 2
}

(($# == 1)) || usage
readonly mode=$1
case $mode in
compatibility | tuned) ;;
*) usage ;;
esac

readonly manifest=tests/harness/manifest.tsv
readonly result_wrapper=tests/harness/bench/case-result.sh
readonly case_runner=tests/harness/bats/run-case.sh

if [[ $mode == tuned ]]; then
  tuned_tmpdir=$(mktemp -d)
  readonly tuned_tmpdir
  cleanup() {
    # CI's nix develop shell has no gtrash; the dir is our own mktemp either way.
    if command -v gtrash >/dev/null 2>&1; then
      gtrash put "$tuned_tmpdir" >&2
    else
      rm -rf "$tuned_tmpdir"
    fi
  }
  trap cleanup EXIT
  export TMPDIR=$tuned_tmpdir
fi

"$result_wrapper" --header

rows=0
while IFS=$'\t' read -r case_id source_file _ family test_name _; do
  [[ $case_id != case_id ]] || continue
  rows=$((rows + 1))
  "$result_wrapper" "$case_id" "$source_file" "$family" -- \
    "$case_runner" "$mode" "$source_file" "$test_name"
done <"$manifest"

expected=$(($(wc -l <"$manifest") - 1))
((rows == expected)) || {
  echo "bats adapter: expected $expected manifest cases, found $rows" >&2
  exit 1
}
