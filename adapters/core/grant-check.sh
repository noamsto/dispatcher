#!/usr/bin/env bash
# dispatch's --add-dir grant validator (_add_dir_ok) and its helpers. One
# definition sourced by dispatch, dispatch-resume and permission-check (the
# auto-approve checker), so the three cannot drift.
#
# Baked into those as @grantCheckLib@ by flake.nix; raw-source runs (bats)
# point $GRANT_CHECK_LIB at this file — the override is for raw-source test
# runs only, set from the dispatcher's own env, never the worker's. Sourced,
# never executed — the shebang keeps the CI shellcheck glob happy.

# _symlink_chain_hops <path> — print, NUL-terminated, every symlink's own
# discovered location while walking <path> component by component (not
# `realpath`: a raw component-at-a-time walk, so a `..` in a target is
# resolved against whatever is *actually* resolved so far, even when that
# required following a symlink first), every directory a `..` backs out of,
# and the fully resolved path last. <path> need not exist past its last real
# component. Paths and link targets are split and captured NUL-safely, so a
# component holding a literal newline stays one component. Fails closed
# (silent, nonzero) on a readlink failure or past a bounded hop budget (a
# symlink cycle) — a caller that used a partial hop list on failure would be
# less safe than refusing outright. A component this user cannot search
# reads as "not a symlink" and is walked past textually, same as it would be
# for anyone without search access to it.
_symlink_chain_hops() {
  local resolved="" comp target budget=40
  local -a queue tcomps
  IFS=/ read -r -d '' -a queue < <(printf '%s\0' "${1#/}")
  while [ "${#queue[@]}" -gt 0 ]; do
    comp="${queue[0]}"
    queue=("${queue[@]:1}")
    case "$comp" in
    '' | '.') continue ;;
    '..')
      [ -z "$resolved" ] || printf '%s\0' "$resolved"
      resolved="${resolved%/*}"
      continue
      ;;
    esac
    if [ -L "$resolved/$comp" ]; then
      budget=$((budget - 1))
      [ "$budget" -gt 0 ] || return 1
      printf '%s\0' "$resolved/$comp"
      # the x sentinel keeps a target's trailing newline from $(...) stripping
      target="$(readlink -n -- "$resolved/$comp" && printf x)" || return 1
      target="${target%x}"
      [[ $target == /* ]] || target="$resolved/$target"
      resolved=""
      IFS=/ read -r -d '' -a tcomps < <(printf '%s\0' "${target#/}")
      queue=("${tcomps[@]}" "${queue[@]}")
    else
      resolved="$resolved/$comp"
    fi
  done
  printf '%s\0' "$resolved"
}

# _git_config_target <base-file> <raw-value> — print, NUL-terminated, the
# include target <raw-value> names: a `~` or `%(prefix)` value expanded by git
# itself, any other relative value joined to <base-file>'s dir. Fails closed,
# silently; the caller words the refusal.
_git_config_target() {
  local f="$1" v="$2" x
  case "$v" in
  '~'* | '%(prefix)'*)
    x="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
      GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=probe.v GIT_CONFIG_VALUE_0="$v" \
      git -C / config --type=path --get probe.v && printf x)" || return 1
    v="${x%x}"
    v="${v%$'\n'}"
    ;;
  esac
  [[ $v == /* ]] || v="${f%/*}/$v"
  printf '%s\0' "$v"
}

# _git_config_files <dir> — print, NUL-terminated, every config file git reads
# for <dir>, as git spells its origin (a relative origin joined to <dir>;
# non-file origins skipped), then every include.path/includeIf.*.path target
# whether or not it exists, since a worker could create it (a relative target
# joined to its including file's dir). Include values are read raw (not via
# `--path`, which reports a `:(optional)<missing file>` value as unset): for a
# `:(optional)` value both the literal form and the form with the prefix
# stripped are emitted, since git 2.55 reads the former literally while
# pathname semantics read the latter. Env overrides are dropped as in
# _git_protected_dirs. Fails closed, printing why.
_git_config_files() {
  local a="$1" f last="" kv v tf rc
  tf=$(mktemp) || return 1
  if ! env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
    git -C "$a" config --show-origin -z --list >"$tf"; then
    rm -f "$tf"
    printf >&2 'dispatch: git cannot list the config files of %s; refusing the grant\n' "$a"
    return 1
  fi
  # -z output: an origin record, then a key<newline>value record
  while IFS= read -r -d '' f && IFS= read -r -d '' kv; do
    [[ $f == file:* && $f != "$last" ]] || continue
    last="$f"
    f="${f#file:}"
    [[ $f == /* ]] || f="$a/$f"
    printf '%s\0' "$f"
  done <"$tf"
  rc=0
  env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
    git -C "$a" config --show-origin -z --get-regexp '^include(if\..*)?\.path$' >"$tf" || rc=$?
  # exit 1: no include is set
  if [ "$rc" -gt 1 ]; then
    rm -f "$tf"
    printf >&2 'dispatch: git cannot list the config includes of %s; refusing the grant\n' "$a"
    return 1
  fi
  rc=0
  while IFS= read -r -d '' f && IFS= read -r -d '' kv; do
    [[ $f == file:* ]] || continue
    f="${f#file:}"
    [[ $f == /* ]] || f="$a/$f"
    v="${kv#*$'\n'}"
    _git_config_target "$f" "$v" || {
      rc=1
      break
    }
    while [[ $v == ':(optional)'* ]]; do
      v="${v#:(optional)}"
      _git_config_target "$f" "$v" || {
        rc=1
        break
      }
    done
    [ "$rc" -eq 0 ] || break
  done <"$tf"
  rm -f "$tf"
  if [ "$rc" -ne 0 ]; then
    printf >&2 'dispatch: git cannot list the config includes of %s; refusing the grant\n' "$a"
    return 1
  fi
}

# _hook_entries <hooks-dir> — print, NUL-terminated, every entry directly in
# <hooks-dir> that is a symlink or a hard-linked regular file: git runs
# <hooks-dir>/<name> through the link, so its target or other links to it are
# as protected as the hooks dir. Fails closed, printing why.
_hook_entries() {
  [ -d "$1" ] || return 0
  find -H "$1" -mindepth 1 -maxdepth 1 \( -type l -o \( -type f -links +1 \) \) -print0 2>/dev/null || {
    printf >&2 'dispatch: find failed scanning hooks dir %s; refusing the grant\n' "$1"
    return 1
  }
}

# _git_protected_dirs <path> — print, NUL-terminated, the hooks dir, git dir,
# common dir and .git entry of every repo whose worktree contains <path>, a
# gitfile's gitdir: and a git dir's commondir as spelled, and the config
# files and include targets git reads there, as git spells them (see
# _git_config_files); then the global core.hooksPath when it is absolute,
# the global and system config files and include targets, and the global
# config candidates git reads when present, since a grant could create a
# missing one. A grant overlapping any of these is a grant on some repo's
# hooks or config: Husky's .husky via core.hooksPath, a submodule's git dir
# under .git/modules, an included file a worker could fill with
# core.hooksPath. Each hooks dir's symlinked or hard-linked entries are
# printed too (see _hook_entries). git only reads config here, and the
# caller's repo-location, GIT_CONFIG and -c env overrides are dropped so the
# answer comes from the human's own config. Fails closed, printing why.
_git_protected_dirs() {
  local a="$1" out gd rc
  local -a lines
  while :; do
    if [ -e "$a/.git" ] || [ -L "$a/.git" ]; then
      # the x sentinel keeps a path's trailing newline from $(...) stripping
      out="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
        git -C "$a" rev-parse --git-path hooks --git-dir --git-common-dir && printf x)" || {
        printf >&2 'dispatch: git cannot resolve the hooks and git dirs of %s; refusing the grant\n' "$a"
        return 1
      }
      out="${out%x}"
      out="${out%$'\n'}"
      mapfile -t lines <<<"$out"
      # a path holding a newline splits into extra lines
      if [ "${#lines[@]}" -ne 3 ]; then
        printf >&2 'dispatch: cannot parse the hooks and git dirs of %s; refusing the grant\n' "$a"
        return 1
      fi
      # relative to $a, and spelled as configured: an absolute format would
      # resolve the symlinks the caller must walk hop by hop
      for out in "${lines[@]}"; do
        [[ $out == /* ]] || out="$a/$out"
        printf '%s\0' "$out"
      done
      out="${lines[0]}"
      [[ $out == /* ]] || out="$a/$out"
      _hook_entries "$out" || return 1
      printf '%s\0' "$a/.git"
      # git prints both dirs resolved; walk a gitfile's gitdir: and the
      # commondir as spelled so a link on either path is a hop
      gd="$a/.git"
      if [ -f "$a/.git" ]; then
        IFS= read -r -d '' out <"$a/.git" || :
        while [[ $out == *[$'\r\n'] ]]; do out="${out%?}"; done
        out="${out#gitdir: }"
        [[ $out == /* ]] || out="$a/$out"
        printf '%s\0' "$out"
        gd="$out"
      fi
      if [ -f "$gd/commondir" ]; then
        IFS= read -r -d '' out <"$gd/commondir" || :
        while [[ $out == *[$'\r\n'] ]]; do out="${out%?}"; done
        [[ $out == /* ]] || out="$gd/$out"
        printf '%s\0' "$out"
      fi
      _git_config_files "$a" || return 1
    fi
    [ "$a" != / ] || break
    a="${a%/*}"
    a="${a:-/}"
  done
  rc=0
  out="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
    git -C / config --path --get core.hooksPath && printf x)" || rc=$?
  case $rc in
  0)
    out="${out%x}"
    out="${out%$'\n'}"
    if [[ $out == /* ]]; then
      printf '%s\0' "$out"
      _hook_entries "$out" || return 1
    fi
    ;;
  # exit 1: core.hooksPath is unset
  1) ;;
  *)
    printf >&2 'dispatch: git cannot read the global core.hooksPath; refusing the grant\n'
    return 1
    ;;
  esac
  _git_config_files / || return 1
  if [ -n "${GIT_CONFIG_GLOBAL:-}" ]; then
    [[ $GIT_CONFIG_GLOBAL != /* ]] || printf '%s\0' "$GIT_CONFIG_GLOBAL"
  else
    for out in "$HOME/.gitconfig" "${XDG_CONFIG_HOME:-$HOME/.config}/git/config"; do
      [[ $out != /* ]] || printf '%s\0' "$out"
    done
  fi
}

# _add_dir_ok <path> — print <path>'s canonical form if it may be granted to a
# worker as an extra directory, else fail. The root, $HOME, $crew_dir and
# secrets-dir checks fail silently (the dispatch-time caller words its own
# refusal); a symlink chain too deep or unreadable, a .git/.claude hit, and a
# failed find scan also print their own reason to stderr. Grantable only when
# <path> resolves inside a resolved root from $DISPATCH_GRANT_ROOTS
# (colon-separated); unset or empty grants nothing, so an unconfigured machine
# refuses every --add-dir. A root is skipped if it isn't absolute or existing,
# is /, or is $HOME or an ancestor of it under either spelling. Inside a root,
# defence in depth still refuses /, $HOME or an ancestor of it, anything
# inside or above $crew_dir (the grant records, bus log and launch scripts
# would become writable), and anything inside or above a secrets/credentials
# dir under $HOME, matched both as spelled and as resolved, so a ~/.ssh
# symlinked into /persist is still caught. ~/.config is refused whole: gh,
# gcloud and most other CLIs keep their credentials under it. Symlinks up to
# two levels deep inside a secrets dir are found and every hop of each one's
# chain is walked: a grant containing any hop is refused (the worker could
# retarget or replace it), so a home-manager/stow link into a root is caught
# even when it reaches the root through an intermediate directory symlink,
# and a grant inside the chain's final target is refused. A grant merely
# inside an intermediate hop is not: that cannot retarget the hop itself.
# A later check also refuses a grant containing a .git or .claude entry, then
# one equal to, inside or above the hooks dir, git dir or common dir of any
# repo whose worktree contains it, a config file or include target git reads
# for it, or an absolute global core.hooksPath (see _git_protected_dirs).
# Every symlink inside the grant is resolved too, and one whose target lies
# outside the grant and contains a hop of, or lies inside, any protected
# chain is refused: git reports those dirs resolved, so whichever file
# spelled a path through the grant, git's answer lands under the link's
# target. A protected regular file with other hard links on the grant's
# filesystem is refused when one of them lies inside the grant.
_add_dir_ok() {
  local p h hs c s r g ok=""
  local -a roots
  [[ $1 == /* && $1 != *$'\n'* ]] || return 1
  [ -d "$1" ] || return 1
  p="$(realpath -e -- "$1")" || return 1
  h="$(realpath -e -- "$HOME")" || return 1
  hs="${HOME%/}"
  [ "$p" != / ] || return 1
  [[ "$h/" != "$p/"* && "$hs/" != "$p/"* ]] || return 1
  IFS=: read -ra roots <<<"${DISPATCH_GRANT_ROOTS:-}"
  for g in "${roots[@]}"; do
    [[ $g == /* ]] || continue
    r="$(realpath -e -- "$g" 2>/dev/null)" || continue
    [[ $r != / && "$h/" != "$r/"* && "$hs/" != "$r/"* ]] || continue
    if [[ "$p/" == "$r/"* ]]; then ok=1; fi
  done
  [ -n "$ok" ] || return 1
  if [ -n "${crew_dir:-}" ]; then
    c="$(realpath -m -- "$crew_dir")"
    [[ "$p/" != "$c/"* && "$c/" != "$p/"* ]] || return 1
  fi
  for s in .ssh .gnupg .aws .config .claude .codex .kube .docker .password-store .local/share/keyrings .cargo .azure .terraform.d .gradle .m2 .mozilla .var; do
    for r in "$h/$s" "$(realpath -m -- "$h/$s")"; do
      [[ "$p/" != "$r/"* && "$r/" != "$p/"* ]] || return 1
    done
    if [ -d "$h/$s" ]; then
      local _f _l _hop _hf _n
      local -a _hops
      _f=$(mktemp) || return 1
      find -H "$h/$s" -maxdepth 2 -type l -print0 >"$_f" 2>/dev/null || {
        rm -f "$_f"
        printf >&2 'dispatch: find failed scanning %s for symlinks; refusing the grant\n' "$h/$s"
        return 1
      }
      # shellcheck disable=SC2094 # rm only in the early-exit || branch, not while reading
      while IFS= read -r -d '' _l; do
        _hf=$(mktemp) || {
          rm -f "$_f"
          return 1
        }
        if ! _symlink_chain_hops "$_l" >"$_hf"; then
          rm -f "$_f" "$_hf"
          printf >&2 'dispatch: symlink chain too deep or unreadable resolving %s; refusing the grant\n' "$_l"
          return 1
        fi
        _hops=()
        while IFS= read -r -d '' _hop; do
          _hops+=("$_hop")
        done <"$_hf"
        rm -f "$_hf"
        for _hop in "${_hops[@]}"; do
          [[ "$_hop/" != "$p/"* ]] || {
            rm -f "$_f"
            return 1
          }
        done
        _n="${#_hops[@]}"
        if [ "$_n" -gt 0 ]; then
          _hop="${_hops[$((_n - 1))]}"
          [[ "$p/" != "$_hop/"* ]] || {
            rm -f "$_f"
            return 1
          }
        fi
      done <"$_f"
      rm -f "$_f"
    fi
  done
  local _lf _le _li _lt _gi _gown
  local -a _links=() _lres=()
  _lf=$(mktemp) || return 1
  if ! find "$p" -xdev \( \( -name .git -o -name .claude \) -print0 -quit \) -o \( -type l -print0 \) >"$_lf" 2>/dev/null; then
    rm -f "$_lf"
    printf >&2 'dispatch: find failed scanning %s for embedded repos; refusing the grant\n' "$p"
    return 1
  fi
  # shellcheck disable=SC2094 # rm only in the early-exit branch, not while reading
  while IFS= read -r -d '' _le; do
    case "${_le##*/}" in
    .git | .claude)
      rm -f "$_lf"
      printf >&2 'dispatch: %s contains a .git or .claude entry (%s); grant its narrowest subdir instead\n' "$p" "$_le"
      return 1
      ;;
    esac
    _links+=("$_le")
  done <"$_lf"
  local _gf _gd _ghf _ghop _gn _ghit
  # every hop and every final target, each beside the protected path it came from
  local -a _ghops _ghall=() _ghallo=() _gres=() _greso=()
  _gf=$(mktemp) || {
    rm -f "$_lf"
    return 1
  }
  _git_protected_dirs "$p" >"$_gf" || {
    rm -f "$_gf" "$_lf"
    return 1
  }
  # shellcheck disable=SC2094 # rm only in the early-exit branches, not while reading
  while IFS= read -r -d '' _gd; do
    _ghf=$(mktemp) || {
      rm -f "$_gf" "$_lf"
      return 1
    }
    if ! _symlink_chain_hops "$_gd" >"$_ghf"; then
      rm -f "$_gf" "$_ghf" "$_lf"
      printf >&2 'dispatch: symlink chain too deep or unreadable resolving %s; refusing the grant\n' "$_gd"
      return 1
    fi
    _ghops=()
    while IFS= read -r -d '' _ghop; do
      _ghops+=("$_ghop")
    done <"$_ghf"
    rm -f "$_ghf"
    _ghit=""
    for _ghop in "${_ghops[@]}"; do
      [[ "$_ghop/" != "$p/"* ]] || _ghit=1
      _ghall+=("$_ghop")
      _ghallo+=("$_gd")
    done
    _gn="${#_ghops[@]}"
    [[ "$p/" != "${_ghops[$((_gn - 1))]}/"* ]] || _ghit=1
    _gres+=("${_ghops[$((_gn - 1))]}")
    _greso+=("$_gd")
    if [ -n "$_ghit" ]; then
      rm -f "$_gf" "$_lf"
      printf >&2 'dispatch: %s overlaps git hooks, git dir or config file %s; grant a dir outside it\n' "$p" "$_gd"
      return 1
    fi
  done <"$_gf"
  rm -f "$_gf"
  local _hd _hs _hk _hl
  local -a _hfiles=() _hfown=() _hst=()
  local -A _hino=()
  for _gi in "${!_gres[@]}"; do
    [ -f "${_gres[_gi]}" ] || continue
    _hfiles+=("${_gres[_gi]}")
    _hfown+=("${_greso[_gi]}")
  done
  if [ "${#_hfiles[@]}" -gt 0 ]; then
    if ! _hd="$(stat -c %d -- "$p")" || ! _hs="$(stat --printf '%h %d:%i\n' -- "${_hfiles[@]}")"; then
      rm -f "$_lf"
      printf >&2 'dispatch: cannot stat the git hooks and config files for %s; refusing the grant\n' "$p"
      return 1
    fi
    mapfile -t _hst <<<"$_hs"
    for _gi in "${!_hfiles[@]}"; do
      _hk="${_hst[_gi]#* }"
      [ "${_hst[_gi]%% *}" -gt 1 ] && [ "${_hk%%:*}" = "$_hd" ] || continue
      _hino[$_hk]="${_hfown[_gi]}"
    done
  fi
  if [ "${#_hino[@]}" -gt 0 ]; then
    if ! find "$p" -xdev -type f -links +1 -printf '%D:%i\0%p\0' >"$_lf" 2>/dev/null; then
      rm -f "$_lf"
      printf >&2 'dispatch: find failed scanning %s for hard links; refusing the grant\n' "$p"
      return 1
    fi
    # shellcheck disable=SC2094 # rm only in the early-exit branch, not while reading
    while IFS= read -r -d '' _hk && IFS= read -r -d '' _hl; do
      [ -n "${_hino[$_hk]+x}" ] || continue
      rm -f "$_lf"
      printf >&2 'dispatch: %s holds hard link %s to git hooks or config file %s; grant a dir without it\n' "$p" "$_hl" "${_hino[$_hk]}"
      return 1
    done <"$_lf"
  fi
  if [ "${#_links[@]}" -gt 0 ]; then
    if ! printf '%s\0' "${_links[@]}" | xargs -0 realpath -m -z -- >"$_lf"; then
      rm -f "$_lf"
      printf >&2 'dispatch: cannot resolve the symlinks in %s; refusing the grant\n' "$p"
      return 1
    fi
    while IFS= read -r -d '' _lt; do
      _lres+=("$_lt")
    done <"$_lf"
  fi
  rm -f "$_lf"
  if [ "${#_lres[@]}" -ne "${#_links[@]}" ]; then
    printf >&2 'dispatch: cannot resolve the symlinks in %s; refusing the grant\n' "$p"
    return 1
  fi
  for _li in "${!_links[@]}"; do
    # the trailing slash makes a link to / compare as /
    _lt="${_lres[_li]%/}/"
    # a target inside the grant is covered by the grant's own checks
    [[ $_lt != "$p/"* ]] || continue
    _gown=""
    for _gi in "${!_ghall[@]}"; do
      [[ "${_ghall[_gi]%/}/" != "$_lt"* ]] || {
        _gown="${_ghallo[_gi]}"
        break
      }
    done
    if [ -z "$_gown" ]; then
      for _gi in "${!_gres[@]}"; do
        [[ $_lt != "${_gres[_gi]%/}/"* ]] || {
          _gown="${_greso[_gi]}"
          break
        }
      done
    fi
    if [ -n "$_gown" ]; then
      printf >&2 'dispatch: %s holds symlink %s to %s, which overlaps git hooks, git dir or config file %s; grant a dir without it\n' "$p" "${_links[_li]}" "${_lres[_li]}" "$_gown"
      return 1
    fi
  done
  printf '%s\n' "$p"
}
