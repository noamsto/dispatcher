#!/usr/bin/env bash
set -euo pipefail

root=$(git rev-parse --show-toplevel)
readonly root
# shellcheck source=/dev/null
source "$root/tests/harness/ensure-crew-go.sh"
trap cleanup_crew_go_bin EXIT
ensure_crew_go_bin

# Not `exec`: the cleanup trap above only runs if this shell survives shellspec.
shellspec --directory "$root/tests/harness/shellspec" \
  --default-path "$root/tests/harness/shellspec/manifest_spec.sh" "$@"
