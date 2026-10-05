#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly root
cd "$root"

case ${1:-serial} in
serial)
  (($# <= 1)) || exit 2
  exec pytest -q tests/harness/pytest
  ;;
xdist)
  (($# <= 1)) || exit 2
  exec pytest -q -n 4 --dist load tests/harness/pytest
  ;;
case)
  (($# == 2)) || exit 2
  exec pytest -q tests/harness/pytest --case-id "$2"
  ;;
*)
  echo "usage: $0 <serial|xdist|case CASE_ID>" >&2
  exit 2
  ;;
esac
