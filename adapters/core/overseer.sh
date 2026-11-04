#!/usr/bin/env bash
# overseer — resolve the folder that holds the user's repos and start the
# overseer session there.
#
# Resolution order: the `overseer.root` setting, then the first
# $CREW_SWEEP_ROOTS entry, then $HOME. The resolved root must exist and must
# not sit inside a git repo — the overseer sweeps many repos, so a root that is
# itself a repo would hide them. Then cd there and exec
# `dispatcher --overseer "$@"`; the wrapper adds root resolution only, all
# --overseer validation stays in dispatcher.sh.

set -euo pipefail

# First non-empty colon- or newline-separated field, matching crew's own
# CREW_SWEEP_ROOTS discovery (crew/internal/repos/repos.go), which skips
# empty entries. Returns 1 when every field is empty.
_first_sweep_root() { # roots
  local field
  while IFS= read -r field; do
    if [ -n "$field" ]; then
      printf '%s\n' "$field"
      return 0
    fi
  done < <(printf '%s\n' "$1" | tr ':' '\n')
  return 1
}

setting_root=""
config_bin="${DISPATCH_CONFIG_BIN:-@dispatchConfig@}"
if command -v "$config_bin" >/dev/null 2>&1; then
  if ! settings="$("$config_bin")"; then
    printf 'overseer: cannot read settings from %s\n' "$config_bin" >&2
    exit 1
  fi
  setting_root="$(jq -r '.overseer.root // empty' <<<"$settings" 2>/dev/null || true)"
fi

if [ -n "$setting_root" ]; then
  root="$setting_root"
elif [ -n "${CREW_SWEEP_ROOTS:-}" ]; then
  if ! root="$(_first_sweep_root "$CREW_SWEEP_ROOTS")"; then
    root="${HOME:?}"
  fi
else
  root="${HOME:?}"
fi

if [ ! -d "$root" ]; then
  printf 'overseer: resolved root %s is not a directory\n' "$root" >&2
  exit 2
fi
if git -C "$root" rev-parse --git-dir >/dev/null 2>&1; then
  printf 'overseer: resolved root %s is inside a git repo — set overseer.root or CREW_SWEEP_ROOTS to the folder that holds your repos\n' "$root" >&2
  exit 2
fi

cd "$root"
exec dispatcher --overseer "$@"
