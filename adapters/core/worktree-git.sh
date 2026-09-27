#!/usr/bin/env bash
# Anchored git for dispatcher-context reads inside a worker's worktree (#539).
# Plain `git -C <worktree> …` discovers the worktree's git dir by reading its
# `.git` gitlink — a file a claude worker can rewrite with an ordinary
# Edit/Write to point at a gitdir it built (in its worktree or its artifacts
# dir), or by adding a submodule gitlink (mode 160000) entry that redirects
# discovery one level deeper. Git then reads that gitdir's `config`, and
# several commands execute config-named programs there: `core.fsmonitor`,
# `filter.<d>.clean/smudge/process`, `core.hooksPath` hooks,
# `core.sshCommand`/`remote.*.uploadpack` on fetch — in the dispatcher's own
# shell, outside the classifier. The functions below pin git to the
# worktree's real admin dir (`--git-dir`/`--work-tree`, resolved by following
# the main repo's own `worktrees/*/gitdir` back-pointers, which a worker
# cannot reach with an unnamed call) instead of letting it discover one.
#
# crew.sh, dispatch.sh and dispatch-resume.sh are standalone
# writeShellApplication builds with no shared library, so flake.nix bakes this
# file's store path into each as @worktreeGitLib@ and each sources it at the
# call site; a raw-source run (bats, a checkout without the file wired)
# overrides the path with $WORKTREE_GIT_LIB. Sourced, never executed — the
# shebang only keeps the CI `shellcheck adapters/core/*.sh` glob happy. Unlike
# the advisory cross-repo-hint lib, sourcing here is unconditional: a missing
# lib must abort, never silently fall back to discovery.

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
  if [ -e "$admin/config.worktree" ] || [ -L "$admin/config.worktree" ]; then
    echo "refusing git in $wt: $admin/config.worktree exists — per-worktree config is worker-writable" >&2
    return 1
  fi
  git --no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null \
    -c submodule.recurse=false --git-dir="$admin" --work-tree="$wt" "$@"
}
_wt_status() {
  local admin="$1" wt="$2"
  shift 2
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
