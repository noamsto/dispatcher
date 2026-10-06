#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly root
exec shellspec --directory "$root/tests/harness/shellspec" \
  --default-path "$root/tests/harness/shellspec/manifest_spec.sh" "$@"
