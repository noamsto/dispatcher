#!/usr/bin/env bash
# Shared cross-repo lane hint for dispatch.sh and dispatch-resume.sh (#398,
# #420). Both are standalone writeShellApplication builds with no shared
# library, so flake.nix bakes this file's store path into each script as
# @crossRepoHintLib@ and each sources it at the call site; a raw-source run
# (bats, a checkout without the file wired) overrides the path with
# $CROSS_REPO_HINT_LIB. Sourced, never executed — the shebang only keeps the
# CI `shellcheck adapters/core/*.sh` glob happy.

# cross_repo_hint <worker_common> <crew_id> — the crew bus is per repo, so a
# dispatcher whose tmux pane sits in another checkout watches the wrong bus and
# never sees this worker's pr_open/done. Print the exact lane command for the
# worker's repo when the pane sits elsewhere and that bus is not already being
# streamed. `crew stream --status` is the single source of truth for liveness
# (rc 0 alive, 1 stale, 2 dead), so anything but alive means the dispatcher
# cannot see this worker here. Read-only; must run from the worker repo.
cross_repo_hint() {
  local worker_common="$1" crew_id="$2" dpane_path dcommon worker_top
  [ -n "${TMUX_PANE:-}" ] || return 0
  dpane_path=$(tmux display-message -p -t "$TMUX_PANE" '#{pane_current_path}' 2>/dev/null || true)
  dcommon=""
  if [ -n "$dpane_path" ] && [ -d "$dpane_path" ]; then
    dcommon=$(git -C "$dpane_path" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)
  fi
  [ -n "$dcommon" ] && [ "$dcommon" != "$worker_common" ] || return 0
  crew stream --status --crew "$crew_id" >/dev/null 2>&1 && return 0
  worker_top=$(git rev-parse --show-toplevel 2>/dev/null || true)
  # Inside the git common dir `--show-toplevel` fails while `crew` still
  # resolves the same bus, so that is the correct fallback — never the
  # dispatcher's checkout, which would name the wrong bus.
  [ -n "$worker_top" ] || worker_top="$worker_common"
  echo "dispatch: cross-repo worker — its bus is $worker_common/crew, not the dispatcher checkout's ($dcommon)."
  echo "  arm/adjust the lane: cd $worker_top && crew stream --crew $crew_id"
}
