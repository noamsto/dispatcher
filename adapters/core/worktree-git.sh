#!/usr/bin/env bash
# Anchored git for dispatcher-context commands inside a worker's worktree
# (#539). The worktree's `.git` gitlink is worker-writable, and git discovery
# through it reads whatever config the worker's chosen gitdir holds — status,
# ls-files and reset then run config-named programs (fsmonitor, filters,
# hooks) in the dispatcher's shell. These functions take the git dir from the
# main repo's own worktrees/*/gitdir back-pointer instead.
#
# Baked into crew, dispatch and dispatch-resume as @worktreeGitLib@ by
# flake.nix; raw-source runs (bats) point $WORKTREE_GIT_LIB at this file.
# Sourced, never executed — the shebang keeps the CI shellcheck glob happy.

# _worktree_anchor_path <wt> — the dispatcher-owned anchor file recording <wt>'s
# genuine crew dir, branch and gitdir, keyed by <wt>'s own realpath. One shared
# definition for dispatch (writer), dispatch-resume (reader) and crew reap
# (pruner), so the key format can never drift between them.
_worktree_anchor_path() {
  local key
  key="$(printf %s "$(realpath -e -- "$1")" | sha256sum | cut -c1-64)"
  printf '%s/crew/worktrees/%s\n' "${XDG_DATA_HOME:-$HOME/.local/share}" "$key"
}

_wt_admin_dir() { # <common-dir> <worktree> -> realpath of <common>/worktrees/<id>
  local common="$1" wt_git gitdir_file back admin_dir
  wt_git="$(realpath -m -- "$2/.git")"
  for gitdir_file in "$common"/worktrees/*/gitdir; do
    [ -f "$gitdir_file" ] || continue
    back="$(head -n1 -- "$gitdir_file")"
    admin_dir="$(dirname -- "$gitdir_file")"
    [[ $back == /* ]] || back="$admin_dir/$back"
    if [ "$(realpath -m -- "$back")" = "$wt_git" ]; then
      realpath -e -- "$admin_dir"
      return
    fi
  done
  return 1
}
_wt_git() { # <admin-dir> <worktree> <git args…>
  local admin="$1" wt="$2"
  shift 2
  # config.worktree is still read with an anchored --git-dir, and its keys
  # (include.path, filter drivers) cannot be enumerated away with -c.
  if [ -e "$admin/config.worktree" ] || [ -L "$admin/config.worktree" ]; then
    echo "refusing git in $wt: $admin/config.worktree exists — per-worktree config is worker-writable" >&2
    return 1
  fi
  # -C: with the caller's cwd inside <wt>, git would resolve pathspecs against
  # that subdirectory rather than the work-tree root.
  git -C "$wt" --no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null \
    -c submodule.recurse=false --git-dir="$admin" --work-tree="$wt" "$@"
}
_wt_status() {
  local admin="$1" wt="$2"
  shift 2
  # A submodule's status runs by discovery through its own worker-writable
  # .git; only the command-line flag beats a .gitmodules `ignore = none`.
  _wt_git "$admin" "$wt" status --porcelain --ignore-submodules=all "$@"
}
_wt_gitlink_ok() { # <admin-dir> <worktree>
  local line target
  [ -f "$2/.git" ] && [ ! -L "$2/.git" ] || return 1
  IFS= read -r line <"$2/.git" || [ -n "$line" ] || return 1
  [[ $line == 'gitdir: '* ]] || return 1
  target="${line#gitdir: }"
  [[ $target == /* ]] || target="$2/$target"
  [ "$(realpath -e -- "$target" 2>/dev/null)" = "$1" ]
}
