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

setting_root=""
if settings="$("${DISPATCH_CONFIG_BIN:-@dispatchConfig@}" 2>/dev/null)"; then
  setting_root="$(jq -r '.overseer.root // empty' <<<"$settings" 2>/dev/null || true)"
fi

if [ -n "$setting_root" ]; then
  root="$setting_root"
elif [ -n "${CREW_SWEEP_ROOTS:-}" ]; then
  root="${CREW_SWEEP_ROOTS%%:*}"
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
