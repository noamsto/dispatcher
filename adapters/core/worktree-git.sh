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
# exec-capable (key,value) pair has drifted from a baseline recorded while
# the repo was still trusted (#557).
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

# Config keys whose value git may run as a program, or that pull in more
# config. `<key-glob> [<value-glob>]`, matched against git's canonical key
# (section and final key lowercase, subsection verbatim).
_wt_exec_keys=(
  'include.path' 'includeif.*.path'
  'core.fsmonitor' 'core.hookspath' 'core.sshcommand' 'core.gitproxy'
  'core.pager' 'core.editor' 'core.askpass' 'core.alternaterefscommand'
  'sequence.editor' 'pager.*' 'hook.*.command' 'hook.*.event' 'gc.recentobjectshook'
  'filter.*.clean' 'filter.*.smudge' 'filter.*.process'
  'diff.external' 'diff.*.textconv' 'diff.*.command' 'merge.*.driver'
  'difftool.*.cmd' 'mergetool.*.cmd'
  'credential.helper' 'credential.*.helper'
  'gpg.program' 'gpg.*.program' 'gpg.ssh.defaultkeycommand'
  'alias.* !*' 'submodule.*.update !*'
  'remote.*.uploadpack' 'remote.*.receivepack' 'remote.*.vcs'
  'protocol.allow' 'protocol.*.allow' 'uploadpack.packobjectshook'
  'interactive.difffilter' 'web.browser' 'browser.*.cmd' 'man.*.cmd'
  'tar.*.command' 'sendemail.smtpserver' 'sendemail.*cmd'
  'lfs.customtransfer.*.path' 'lfs.extension.*.clean' 'lfs.extension.*.smudge'
)
_wt_cfg_pairs() { # <git-dir> -> sorted `key\nvalue\0` exec-capable local/worktree pairs
  local -a recs
  local i scope key value entry
  # Listing config never runs a configured program.
  mapfile -d '' recs < <(git --git-dir="$1" config --list --show-scope -z)
  wait $! || return 1
  for ((i = 0; i + 1 < ${#recs[@]}; i += 2)); do
    scope="${recs[i]}"
    [[ $scope == local || $scope == worktree ]] || continue
    # A valueless key (`[core] fsmonitor`) has no newline: value stays empty.
    key="${recs[i + 1]%%$'\n'*}"
    value="${recs[i + 1]#"$key"}"
    value="${value#$'\n'}"
    for entry in "${_wt_exec_keys[@]}"; do
      # shellcheck disable=SC2053 # the table holds globs, matched unquoted
      if [[ $key == ${entry%% *} ]] && { [[ $entry != *' '* ]] || [[ $value == ${entry#* } ]]; }; then
        printf '%s\n%s\0' "$key" "$value"
        break
      fi
    done
  done | LC_ALL=C sort -z -u
}
_wt_cfg_union() { # <common> -> _wt_cfg_pairs over <common> and each linked admin dir
  local common="$1" head dir
  local -a dirs=("$common") pairs=() got
  for head in "$common"/worktrees/*/HEAD; do
    [ -f "$head" ] && dirs+=("${head%/HEAD}")
  done
  for dir in "${dirs[@]}"; do
    mapfile -d '' got < <(_wt_cfg_pairs "$dir")
    wait $! || return 1
    pairs+=("${got[@]}")
  done
  ((${#pairs[@]})) || return 0
  printf '%s\0' "${pairs[@]}" | LC_ALL=C sort -z -u
}
_wt_cfg_baseline_init() { # <common> — record the baseline once; never overwrite
  local common="$1" file tmp rec keys=
  local -a recs
  file="$common/crew/git-config-baseline"
  if [ -e "$file" ] || [ -L "$file" ]; then
    return 0
  fi
  mkdir -p -- "$common/crew" || return 1
  tmp="$(mktemp "$file.XXXXXX")" || return 1
  if ! _wt_cfg_union "$common" >"$tmp" || ! mv -f -- "$tmp" "$file"; then
    rm -f -- "$tmp"
    return 1
  fi
  mapfile -d '' recs <"$file"
  ((${#recs[@]})) || return 0
  # Keys only: a credential.helper value may carry a token.
  for rec in "${recs[@]}"; do
    rec="${rec%%$'\n'*}"
    [[ ", $keys, " == *", $rec, "* ]] || keys="${keys:+$keys, }$rec"
  done
  echo "git-config baseline: recorded $file: $keys" >&2
}
_wt_cfg_guard() { # <common> [<git-dir>] — refuse exec-capable config drift from the baseline
  local common="$1" gitdir="${2:-$1}" file rec key origin found i
  local -a pairs listing drift=()
  local -A base=() bad=() seen=()
  file="$common/crew/git-config-baseline"
  if [ ! -f "$file" ]; then
    echo "refusing git: no git-config baseline at $file — the next dispatch records it" >&2
    return 1
  fi
  mapfile -d '' pairs <"$file" || return 1
  for rec in "${pairs[@]}"; do
    base["$rec"]=1
  done
  mapfile -d '' pairs < <(_wt_cfg_pairs "$gitdir")
  if ! wait $!; then
    echo "refusing git: cannot list the git config of $gitdir" >&2
    return 1
  fi
  for rec in "${pairs[@]}"; do
    [ -z "${base["$rec"]+x}" ] || continue
    key="${rec%%$'\n'*}"
    [ -n "${seen["$key"]+x}" ] || drift+=("$key")
    seen["$key"]=1
    bad["$rec"]=1
  done
  ((${#drift[@]})) || return 0
  mapfile -d '' listing < <(git --git-dir="$gitdir" config --list --show-origin --show-scope -z)
  for key in "${drift[@]}"; do
    found=
    for ((i = 0; i + 2 < ${#listing[@]}; i += 3)); do
      [[ ${listing[i]} == local || ${listing[i]} == worktree ]] || continue
      rec="${listing[i + 2]}"
      [[ $rec == *$'\n'* ]] || rec+=$'\n'
      [[ $rec == "$key"$'\n'* && -n ${bad["$rec"]+x} ]] || continue
      origin="${listing[i + 1]#file:}"
      [[ " $found " != *" $origin "* ]] || continue
      found+=" $origin"
      printf 'refusing git: %q (from %q) is not in the git-config baseline %s\n' "$key" "$origin" "$file" >&2
      printf '  remove it: git config --file %q --unset-all %q\n' "$origin" "$key" >&2
    done
    [ -n "$found" ] || printf 'refusing git: %q is not in the git-config baseline %s\n' "$key" "$file" >&2
  done
  printf "  to clear it: list drift with \`crew git-baseline\`, remove any key you did not set, then delete %q — the next dispatch re-records the baseline and accepts EVERYTHING present then, so inspect first\n" "$file" >&2
  return 1
}
_wt_cfg_guard_cwd() { # <common> — _wt_cfg_guard for <common> and the git dir the caller's cwd resolves to
  local own
  _wt_cfg_guard "$1" || return 1
  # A linked worktree's cwd adds its config.worktree to what plain git reads.
  own="$(git rev-parse --absolute-git-dir)" || {
    echo "refusing git: cannot resolve the git dir of $PWD" >&2
    return 1
  }
  [ "$own" = "$1" ] || _wt_cfg_guard "$1" "$own"
}

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
_wt_git() { # <admin-dir> <worktree> <git args…>
  local admin="$1" wt="$2"
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
  # -C: with the caller's cwd inside <wt>, git would resolve pathspecs against
  # that subdirectory rather than the work-tree root. The -c overrides still
  # matter past the guard: a baselined core.hooksPath=.husky resolves inside
  # the worker's tree. hooksPath does not reach config-defined hooks
  # (hook.<name>.command), so the events these calls trigger are switched off.
  git -C "$wt" --no-optional-locks -c core.fsmonitor=false -c core.hooksPath=/dev/null \
    -c hook.reference-transaction.enabled=false -c hook.post-checkout.enabled=false \
    -c hook.post-index-change.enabled=false \
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
