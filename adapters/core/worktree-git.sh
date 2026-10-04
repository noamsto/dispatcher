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
# Config keys that choose which endpoint git talks to, or whether it trusts it
# (#678). Same format as _wt_exec_keys.
_wt_redirect_keys=(
  'url.*.insteadof' 'url.*.pushinsteadof'
  'remote.*.url' 'remote.*.pushurl' 'remote.*.proxy'
  'http.proxy' 'http.*.proxy'
  'http.sslverify' 'http.*.sslverify'
  'http.sslcainfo' 'http.*.sslcainfo' 'http.sslcapath' 'http.*.sslcapath'
  'http.curloptresolve' 'http.*.curloptresolve'
  'fetch.bundleuri'
)
# A baseline record saying the baseline covers _wt_redirect_keys: without it the
# baseline predates #678. No config key can start with `#`.
_wt_cfg_redirect_mark=$'#covers\nredirect'
_wt_cfg_match() { # <table-name> <key> <value> — status 0 when the table names the pair
  local -n _wt_tbl="$1"
  local entry
  for entry in "${_wt_tbl[@]}"; do
    # shellcheck disable=SC2053 # the table holds globs, matched unquoted
    if [[ $2 == ${entry%% *} ]] && { [[ $entry != *' '* ]] || [[ $3 == ${entry#* } ]]; }; then
      return 0
    fi
  done
  return 1
}
_wt_cfg_guarded() { # <key> <value> — status 0 when the baseline guards the pair
  _wt_cfg_match _wt_exec_keys "$1" "$2" || _wt_cfg_match _wt_redirect_keys "$1" "$2"
}
# git-hooks.nix spells the shared core.hooksPath `.git/hooks` from the main
# checkout and `<common>/hooks` from a linked worktree (#628). Only that exact
# relative string maps to `<R>/hooks`, R the realpath of the caller's anchored
# <common>: any other spelling (`.husky`, `./.git/hooks`) may name a
# worker-editable dir. git resolves a relative value against a context the
# worker controls (a `.git` dir or symlink, core.bare, core.worktree), so
# _wt_cfg_guard_cwd — run only from a trusted cwd: dispatch entry, reap's
# window-kill gate and the `wt` gates, which cannot take --git-dir — asks git's own
# `--git-path hooks` whether the spellings resolve to one dir. Any
# core.worktree turns the equivalence off: work-tree commands resolve the value
# there, which rev-parse does not show. Anchored calls (_wt_git, _wt_git_common)
# pin core.hooksPath=/dev/null (_wt_neutral_cfg) and resolve nothing from a cwd.
_wt_cfg_canon() { # <R> <rec> <var> — set <var> to <rec>, the exact relative `.git/hooks` core.hookspath made `<R>/hooks`
  printf -v "$3" '%s' "$2"
  [[ $2 == core.hookspath$'\n'.git/hooks && -n $1 ]] || return 0
  printf -v "$3" 'core.hookspath\n%s/hooks' "$1"
}
_wt_cfg_pairs() { # <git-dir> -> sorted `key\nvalue\0` guarded local/worktree pairs
  local -a recs
  local i scope key value
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
    if _wt_cfg_guarded "$key" "$value"; then printf '%s\n%s\0' "$key" "$value"; fi
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
# A key like url.<base>.insteadof carries its URL, and a URL may carry a token.
_wt_cfg_redact() { # <var> <string> — set <var> to <string> with each URL's userinfo shown as ***
  local rest="$2" auth out=
  while [[ $rest == *://* ]]; do
    out+="${rest%%://*}://"
    rest="${rest#*://}"
    auth="${rest%%[/?#]*}"
    [[ $auth == *@* ]] || continue
    out+='***@'
    rest="${rest#"${auth%@*}"@}"
  done
  printf -v "$1" '%s' "$out$rest"
}
_wt_cfg_note_keys() { # <what> <rec>... — print the records' keys, never their values
  local what="$1" rec keys=
  shift
  for rec in "$@"; do
    [ "$rec" != "$_wt_cfg_redirect_mark" ] || continue
    rec="${rec%%$'\n'*}"
    [[ ", $keys, " == *", $rec, "* ]] || keys="${keys:+$keys, }$rec"
  done
  # Keys only: a credential.helper value may carry a token.
  [ -n "$keys" ] || return 0
  _wt_cfg_redact keys "$keys"
  echo "git-config baseline: recorded $what: $keys" >&2
}
_wt_cfg_baseline_init() { # <common> — record the baseline once; never overwrite, only add the redirect class (#678)
  local common="$1" file tmp rec key value
  local -a recs union add=()
  file="$common/crew/git-config-baseline"
  if [ -L "$file" ]; then
    return 0
  fi
  if [ -e "$file" ]; then
    [ -f "$file" ] || return 0
    mapfile -d '' recs <"$file" || return 1
    for rec in "${recs[@]}"; do
      [ "$rec" != "$_wt_cfg_redirect_mark" ] || return 0
    done
    # TOFU, as at creation — but never exec-class pairs, which would launder drift.
    mapfile -d '' union < <(_wt_cfg_union "$common")
    wait $! || return 1
    for rec in "${union[@]}"; do
      key="${rec%%$'\n'*}"
      value="${rec#"$key"}"
      value="${value#$'\n'}"
      if _wt_cfg_match _wt_redirect_keys "$key" "$value"; then add+=("$rec"); fi
    done
    tmp="$(mktemp "$file.XXXXXX")" || return 1
    if ! printf '%s\0' "${recs[@]}" "${add[@]}" "$_wt_cfg_redirect_mark" | LC_ALL=C sort -z -u >"$tmp" || ! mv -f -- "$tmp" "$file"; then
      rm -f -- "$tmp"
      return 1
    fi
    _wt_cfg_note_keys "redirect keys in $file" "${add[@]}"
    return 0
  fi
  mkdir -p -- "$common/crew" || return 1
  tmp="$(mktemp "$file.XXXXXX")" || return 1
  if ! { _wt_cfg_union "$common" && printf '%s\0' "$_wt_cfg_redirect_mark"; } | LC_ALL=C sort -z -u >"$tmp" || ! mv -f -- "$tmp" "$file"; then
    rm -f -- "$tmp"
    return 1
  fi
  mapfile -d '' recs <"$file"
  _wt_cfg_note_keys "$file" "${recs[@]}"
}
_wt_cfg_guard() { # <common> [<git-dir>] — refuse exec-capable config drift from the baseline
  local common="$1" gitdir="${2:-$1}" file rec key value origin found i R canon shown line hint redirect=
  local -a pairs listing drift=()
  local -A base=() bad=() seen=()
  file="$common/crew/git-config-baseline"
  if [ ! -f "$file" ]; then
    echo "refusing git: no git-config baseline at $file — the next dispatch records it, or run \`crew git-baseline --accept\` from your own terminal" >&2
    return 1
  fi
  R="$(realpath -e -- "$common")" || R=
  mapfile -d '' pairs <"$file" || return 1
  for rec in "${pairs[@]}"; do
    [ -n "$rec" ] || continue
    [ "$rec" != "$_wt_cfg_redirect_mark" ] || redirect=1
    _wt_cfg_canon "$R" "$rec" canon
    base["$canon"]=1
  done
  mapfile -d '' pairs < <(_wt_cfg_pairs "$gitdir")
  if ! wait $!; then
    echo "refusing git: cannot list the git config of $gitdir" >&2
    return 1
  fi
  for rec in "${pairs[@]}"; do
    _wt_cfg_canon "$R" "$rec" canon
    [ -z "${base["$canon"]+x}" ] || continue
    key="${rec%%$'\n'*}"
    # A pre-#678 baseline guards the exec class only until dispatch's init migrates it.
    if [ -z "$redirect" ]; then
      value="${rec#"$key"}"
      value="${value#$'\n'}"
      ! _wt_cfg_match _wt_redirect_keys "$key" "$value" || continue
    fi
    [ -n "${seen["$key"]+x}" ] || drift+=("$key")
    seen["$key"]=1
    bad["$rec"]=1
  done
  ((${#drift[@]})) || return 0
  mapfile -d '' listing < <(git --git-dir="$gitdir" config --list --show-origin --show-scope -z)
  for key in "${drift[@]}"; do
    found=
    _wt_cfg_redact shown "$key"
    for ((i = 0; i + 2 < ${#listing[@]}; i += 3)); do
      [[ ${listing[i]} == local || ${listing[i]} == worktree ]] || continue
      rec="${listing[i + 2]}"
      [[ $rec == *$'\n'* ]] || rec+=$'\n'
      [[ $rec == "$key"$'\n'* && -n ${bad["$rec"]+x} ]] || continue
      origin="${listing[i + 1]#file:}"
      [[ " $found " != *" $origin "* ]] || continue
      found+=" $origin"
      printf -v line 'refusing git: %q (from %q) is not in the git-config baseline %s' "$key" "$origin" "$file"
      _wt_cfg_redact line "$line"
      printf '%s\n' "$line" >&2
      hint='remove it'
      # Unsetting a replaced remote.origin.url would delete origin.
      ! _wt_cfg_match _wt_redirect_keys "$key" "${rec#"$key"$'\n'}" || hint='restore the baselined value or remove it'
      if [ "$shown" != "$key" ]; then
        printf "  %s: edit %q (the key's URL userinfo is redacted here)\n" "$hint" "$origin" >&2
      else
        printf '  %s: git config --file %q --unset-all %q\n' "$hint" "$origin" "$key" >&2
      fi
    done
    [ -z "$found" ] || continue
    printf -v line 'refusing git: %q is not in the git-config baseline %s' "$key" "$file"
    _wt_cfg_redact line "$line"
    printf '%s\n' "$line" >&2
  done
  printf "  to clear it: list drift with \`crew git-baseline\`, remove any key you did not set (git config --unset-all), then accept the rest from your own terminal: \`crew git-baseline --accept\` (deleting %q instead re-records EVERYTHING present at the next dispatch)\n" "$file" >&2
  return 1
}
_wt_cfg_guard_cwd() { # <common> — _wt_cfg_guard for <common> and the git dir the caller's cwd resolves to
  local own v rc=0 rec R h
  local -a recs
  _wt_cfg_guard "$1" || return 1
  # A linked worktree's cwd adds its config.worktree to what plain git reads.
  own="$(git rev-parse --absolute-git-dir)" || {
    echo "refusing git: cannot resolve the git dir of $PWD" >&2
    return 1
  }
  if [ "$own" != "$1" ]; then _wt_cfg_guard "$1" "$own" || return 1; fi
  # The guards took a relative .git/hooks as <R>/hooks; from here git must too.
  v="$(git config --get core.hooksPath)" || rc=$?
  case $rc in
  0) ;;
  1) return 0 ;;
  *)
    printf 'refusing git: cannot read core.hooksPath in %q\n' "$PWD" >&2
    return 1
    ;;
  esac
  [ "$v" = .git/hooks ] || return 0
  mapfile -d '' recs <"$1/crew/git-config-baseline" || return 1
  for rec in "${recs[@]}"; do
    [ "$rec" != core.hookspath$'\n'.git/hooks ] || return 0
  done
  R="$(realpath -e -- "$1")" || R=
  rc=0
  git config --get core.worktree >/dev/null || rc=$?
  case $rc in
  0)
    printf 'refusing git: core.hooksPath .git/hooks in %q resolves under core.worktree, not against the baselined %q\n' "$PWD" "$R/hooks" >&2
    printf '  set the absolute spelling: git config core.hooksPath %q\n' "$R/hooks" >&2
    return 1
    ;;
  1) ;;
  *)
    printf 'refusing git: cannot read core.worktree in %q\n' "$PWD" >&2
    return 1
    ;;
  esac
  if h="$(git rev-parse --path-format=absolute --git-path hooks 2>/dev/null)"; then
    [ "$h" != "$(realpath -m -- "$R/hooks")" ] || return 0
    printf 'refusing git: core.hooksPath .git/hooks in %q names %q, not the baselined %q\n' "$PWD" "$h" "$R/hooks" >&2
  else
    printf 'refusing git: core.hooksPath .git/hooks in %q names no hooks dir git can use, not the baselined %q\n' "$PWD" "$R/hooks" >&2
  fi
  if [ -n "$R" ] && [ "$(realpath -e -- "$own")" = "$R" ]; then
    printf '  run from the main checkout, or set the absolute spelling: git config core.hooksPath %q\n' "$R/hooks" >&2
  else
    printf '  set the absolute spelling: git config core.hooksPath %q\n' "$R/hooks" >&2
  fi
  return 1
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
    [ "$d" != / ] || return 0
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
