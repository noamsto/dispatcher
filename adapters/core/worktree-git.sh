#!/usr/bin/env bash
# Anchored git for dispatcher-context commands inside a worker's worktree
# (#539). The worktree's `.git` gitlink is worker-writable, and git discovery
# through it reads whatever config the worker's chosen gitdir holds — status,
# ls-files and reset then run config-named programs (fsmonitor, filters,
# hooks) in the dispatcher's shell. These functions take the git dir from the
# main repo's own worktrees/*/gitdir back-pointer instead.
#
# The anchored git still reads the COMMON config, which a worker can also
# write (`git config filter.x.clean …`). _wt_git therefore refuses when an
# exec-capable or redirecting (`url.*.insteadOf`, `http.proxy`, #678)
# (key,value) pair has drifted from a baseline recorded while the repo was
# still trusted (#557). The anchored git also reads no in-tree
# .gitattributes and ignores core.attributesFile, so a baselined driver whose
# program is a worktree-relative path the worker rewrote never runs (#578).
#
# That config check — the key tables, URL-credential masking, the baseline and
# the relative `core.hooksPath` equivalence — lives in crew/internal/gitguard
# (Go) and runs as `crew-go git-guard`; this file keeps the anchored git
# invocation around it.
#
# Baked into dispatch and dispatch-resume as @worktreeGitLib@ by flake.nix,
# with @crewGoBin@ substituted there (`crew` carried the lib until #1004 ported
# its last bash caller, and substitutes @crewGoBin@ for itself); raw-source runs
# (bats) point $WORKTREE_GIT_LIB at this file and export CREW_GO_BIN.
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

# A missing crew-go binary exits 127, which every caller treats as a refusal:
# the guard fails closed.
# <common> — record the baseline once; never overwrite, only add coverage classes
_wt_cfg_baseline_init() { "${CREW_GO_BIN:-@crewGoBin@}" git-guard init "$1"; }
# <common> [<git-dir>] — refuse exec-capable config and legacy-file drift
_wt_cfg_guard() { "${CREW_GO_BIN:-@crewGoBin@}" git-guard check "$1" "${2:-$1}"; }
# <common> — _wt_cfg_guard for <common> and the git dir the caller's cwd resolves to
_wt_cfg_guard_cwd() { "${CREW_GO_BIN:-@crewGoBin@}" git-guard check-cwd "$1"; }
# <common> — refuse a planted commondir file that redirects git's common dir (#993)
_wt_cfg_commondir() { "${CREW_GO_BIN:-@crewGoBin@}" git-guard commondir "$1"; }

_wt_admin_dir() { # <common-dir> <worktree> -> realpath of <common>/worktrees/<id>
  local common="$1" wt_git gitdir_file back admin_dir real
  wt_git="$(realpath -m -- "$2/.git")"
  for gitdir_file in "$common"/worktrees/*/gitdir; do
    [ -f "$gitdir_file" ] || continue
    back="$(head -n1 -- "$gitdir_file")"
    admin_dir="$(dirname -- "$gitdir_file")"
    [[ $back == /* ]] || back="$admin_dir/$back"
    if [ "$(realpath -m -- "$back")" = "$wt_git" ]; then
      real="$(realpath -e -- "$admin_dir")" || return 1
      # A symlinked worktrees/ or worktrees/<id> moves the admin dir — and the
      # config it reads — outside the main repo.
      [ "$real" = "$(realpath -e -- "$common")/worktrees/${admin_dir##*/}" ] || return 1
      printf '%s\n' "$real"
      return
    fi
  done
  return 1
}
# -c overrides that still matter past the guard: a baselined
# core.hooksPath=.husky resolves inside the worker's tree. hooksPath does not
# reach config-defined hooks (hook.<name>.command), so the events these calls
# trigger are switched off. core.attributesFile is not a guarded key, so a
# worker could point it at its own file and select a driver past
# --attr-source (#578).
_wt_neutral_cfg=(
  core.fsmonitor=false core.hooksPath=/dev/null core.attributesFile=/dev/null
  hook.reference-transaction.enabled=false hook.post-checkout.enabled=false
  hook.post-index-change.enabled=false submodule.recurse=false
)
_wt_empty_tree() { # <git-dir> -> the repo's empty-tree oid (sha1 or sha256)
  git --git-dir="$1" hash-object --no-filters -t tree /dev/null
}
_wt_neutral() { # <git-dir> <cmd…> — run a tool that spawns its own git (wt) with _wt_git's overrides, via env
  local gitdir="$1" empty kv key value n=0
  local -a kv_env=()
  shift
  empty="$(_wt_empty_tree "$gitdir")" || {
    echo "refusing git: cannot compute the empty tree of $gitdir" >&2
    return 1
  }
  for kv in "${_wt_neutral_cfg[@]}"; do
    key="${kv%%=*}" value="${kv#*=}"
    kv_env+=("GIT_CONFIG_KEY_$n=$key" "GIT_CONFIG_VALUE_$n=$value")
    n=$((n + 1))
  done
  env GIT_ATTR_SOURCE="$empty" GIT_CONFIG_COUNT="$n" "${kv_env[@]}" "$@"
}
# Anchor, don't discover (#633): --git-dir=<common> reads no cwd's `.git`, so a
# worker-swapped or standalone one is never consulted. A builder, not a runner,
# so a caller can background git itself and keep its pid.
_wt_common_argv() { # <common> <array-var>
  local -n _wt_argv="$2"
  local kv
  _wt_argv=(git --git-dir="$1")
  for kv in "${_wt_neutral_cfg[@]}" gc.auto=0 maintenance.auto=false; do _wt_argv+=(-c "$kv"); done
}
_wt_git_common() { # <common> <git args…> — guard <common>, then git anchored on it
  local common="$1"
  local -a argv
  shift
  _wt_cfg_guard "$common" || return 1
  _wt_common_argv "$common" argv
  "${argv[@]}" "$@"
}
# Path-keyed on the dispatcher record of the cwd's own ancestor, so there is no
# check-then-use gap: swapping `.git` cannot hide the worktree (#633). Moving or
# renaming the worktree can — a documented residual.
_wt_trusted_cwd() { # <common> — cd out of a dispatched worker's worktree to <common>'s main checkout
  local common="$1" d rec line primary=
  local -a lines wl
  d="$(pwd -P)" || return 1
  while :; do
    rec="$(_worktree_anchor_path "$d")"
    [ ! -e "$rec" ] && [ ! -L "$rec" ] || break
    if [ "$d" = / ]; then
      _wt_cfg_commondir "$common"
      return
    fi
    d="$(dirname -- "$d")"
  done
  if [ -L "$rec" ] || [ ! -f "$rec" ]; then
    printf 'refusing git: the dispatcher record for %q is not a regular file: %q\n' "$d" "$rec" >&2
    return 1
  fi
  mapfile -t lines <"$rec"
  if [ "${lines[1]:-}" != "$(realpath -m -- "$common/crew")" ]; then
    printf 'refusing git: %q is inside the worker worktree %q, whose git resolves to %q, not the recorded %q — its .git no longer matches its dispatcher record (possible tampering): stop and tell the human\n' \
      "$(pwd -P)" "$d" "$common" "${lines[1]%/crew}" >&2
    return 1
  fi
  # The record checks read no git; the worktree list would follow a planted commondir (#993).
  _wt_cfg_commondir "$common" || return 1
  mapfile -t wl < <(git --git-dir="$common" worktree list --porcelain)
  for line in "${wl[@]}"; do
    [ -n "$line" ] || break
    case $line in
    'worktree '*) primary="${line#worktree }" ;;
    bare)
      primary=
      break
      ;;
    esac
  done
  # A separate-git-dir layout reports the git dir itself as the main worktree.
  if [ -z "$primary" ] || [ "$(realpath -m -- "$primary")" = "$(realpath -m -- "$common")" ] || ! cd -- "$primary"; then
    printf 'refusing git: %q is a worker worktree and %q has no main checkout to run from — run from a directory outside any worker worktree\n' "$d" "$common" >&2
    return 1
  fi
  # The primary checkout's own discovery is pinned too, not just the worker's.
  _wt_cfg_commondir "$common"
}
_wt_git() { # <admin-dir> <worktree> <git args…>
  local admin="$1" wt="$2" empty kv
  local -a cfg=()
  shift 2
  # config.worktree is still read with an anchored --git-dir, and its keys
  # (include.path, filter drivers) cannot be enumerated away with -c.
  if [ -e "$admin/config.worktree" ] || [ -L "$admin/config.worktree" ]; then
    echo "refusing git in $wt: $admin/config.worktree exists — per-worktree config is worker-writable" >&2
    return 1
  fi
  if [[ $admin != */worktrees/?* || ${admin##*/worktrees/} == */* ]]; then
    echo "refusing git in $wt: $admin is not a linked-worktree admin dir" >&2
    return 1
  fi
  _wt_cfg_guard "${admin%/worktrees/*}" "$admin" || return 1
  empty="$(_wt_empty_tree "$admin")" || {
    echo "refusing git: cannot compute the empty tree of $admin" >&2
    return 1
  }
  for kv in "${_wt_neutral_cfg[@]}"; do cfg+=(-c "$kv"); done
  # -C: with the caller's cwd inside <wt>, git would resolve pathspecs against
  # that subdirectory rather than the work-tree root.
  git -C "$wt" --no-optional-locks "${cfg[@]}" --attr-source="$empty" \
    --git-dir="$admin" --work-tree="$wt" "$@"
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

# _wt_fetch_origin_branch <common> <name> — fetch an untrusted (gh/stamp-derived) branch
# name into refs/remotes/origin/<name> only, always in the dispatcher's own
# repo. A bare positional would parse the name as a refspec
# (`+refs/heads/x:refs/remotes/origin/main` force-updates origin/main) or a
# fetch option (`--upload-pack=...`), so it must be a plain branch name and is
# spelled as an explicit refspec. Anchored on the common dir, so it runs the
# same from any cwd and no worker's `.git` is consulted (#539, #633). Sets
# fetched_oid from the fetch's own porcelain output, never a re-read: refs are
# shared with workers, which can rewrite refs/remotes/origin/<name> between the
# fetch and a later rev-parse (#695).
_wt_plain_branch_name() {
  [[ $1 == *:* || $1 == +* ]] && return 1
  git check-ref-format --branch "$1" >/dev/null
}
# shellcheck disable=SC2034 # fetched_oid is the caller-visible result
_wt_fetch_origin_branch() {
  local common=$1 name=$2
  local out line flag new ref oid=
  fetched_oid=
  _wt_plain_branch_name "$name" || return 1
  out="$(_wt_git_common "$common" fetch --porcelain -v --no-recurse-submodules origin "+refs/heads/$name:refs/remotes/origin/$name")" || return 1
  while IFS= read -r line; do
    flag=${line:0:1}
    read -r _ new ref <<<"${line:2}"
    [ "$ref" = "refs/remotes/origin/$name" ] || continue
    case $flag in ' ' | + | '*' | = | t) oid=$new ;; esac
  done <<<"$out"
  [ -n "$oid" ] || return 1
  _wt_git_common "$common" cat-file -e "$oid^{commit}" 2>/dev/null || return 1
  fetched_oid=$oid
}
