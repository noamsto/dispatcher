# shellcheck shell=bash disable=SC2154
# Helpers the launchers (dispatch, dispatch-resume, dispatcher, lead-launcher)
# share, so a fix lands once. Sourced, never run. It carries no build-time
# substitution token: each launcher keeps its own baked values and this file
# reads them as plain variables (engine_path reads $_launcher_runtime_path; the
# lead and remote helpers read $crew_dir, $branch and the --host settings the
# caller set). dispatch-notify.sh ships as raw plugin text and keeps its own
# _bus_append, compared whole against this one.

# crew.sh's atomic-append helper. A bare `printf >>` isn't one write(2), so
# concurrent writers to this shared log can splice a large line with another
# process's append (#55, #61).
_bus_append() {
  local p='' s1='' s2='' c=''
  if [ -s "$1" ]; then
    s1="$(wc -c <"$1" 2>/dev/null)" || true
    c="$(dd if="$1" bs=1 skip=$((s1 - 1)) count=1 2>/dev/null)" || true
    if [ -n "$c" ]; then
      s2="$(wc -c <"$1" 2>/dev/null)" || true
      [ "$s1" != "$s2" ] || p=$'\n'
    fi
  fi
  printf '%s%s\n' "$p" "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"
}

# _pid_alive <pid> — 0 when a process with this pid exists, under any uid.
# `kill -0 0` signals the caller's own process group and `kill -0 -1`
# broadcasts, so only a positive integer is a liveness probe. A failed signal
# is not proof of death: another uid's process returns EPERM, and the signal
# merely being refused proves it exists, so EPERM reads alive. `ps -p` is the
# fallback for any other error (and where `kill` cannot name the error we
# parse). Fail closed: anything unreadable stays alive.
_pid_alive() {
  case "$1" in '' | *[!0-9]* | 0) return 1 ;; esac
  local kmsg
  kmsg="$(LC_ALL=C kill -0 "$1" 2>&1)" && return 0
  case "$kmsg" in
  *"not permitted"* | *"not allowed"*) return 0 ;; # EPERM: the process exists
  esac
  ps -p "$1" -o pid= >/dev/null 2>&1
}

# _file_mtime_s <file> — the file's mtime in epoch seconds, or return 1. GNU
# stat takes `-c %Y`; BSD/macOS takes `-f %m`. GNU first: its `-f` means
# "filesystem" and would print a filesystem block for a `-f %m` invocation.
_file_mtime_s() {
  stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null
}

# _ps_elapsed_s <pid> — the process's elapsed seconds, or return 1 when ps can
# not say. `etimes` is exact where the platform has it; `etime` parses the
# [[dd-]hh:]mm:ss macOS prints. Both are locale- and timezone-independent.
_ps_elapsed_s() {
  local pid="$1" out d h m s rest
  out="$(ps -o etimes= -p "$pid" 2>/dev/null | tr -d '[:space:]')"
  case "$out" in
  '' | *[!0-9]*) ;;
  *)
    printf '%s' "$out"
    return 0
    ;;
  esac
  out="$(ps -o etime= -p "$pid" 2>/dev/null | tr -d '[:space:]')"
  case "$out" in
  '' | *[!0-9:-]*) return 1 ;;
  esac
  d=0
  case "$out" in
  *-*)
    d="${out%%-*}"
    out="${out#*-}"
    ;;
  esac
  [ -n "$d" ] || d=0
  case "$out" in
  *:*:*)
    h="${out%%:*}"
    rest="${out#*:}"
    m="${rest%%:*}"
    s="${rest#*:}"
    ;;
  *:*)
    h=0
    m="${out%%:*}"
    s="${out#*:}"
    ;;
  *)
    h=0
    m=0
    s="$out"
    ;;
  esac
  printf '%s' "$((10#$d * 86400 + 10#$h * 3600 + 10#$m * 60 + 10#$s))"
}

# _pid_recycled <pid> <pidfile> — 0 when the process now holding <pid> started
# after <pidfile> was last written (plus 2s of slack for ps' second
# granularity), so it cannot be the dispatcher the file records: that process
# existed when the file was written, and a later start inherited the number.
# Returns 1 when either timestamp is unreadable, so a live process is never
# judged dead; a coarse-mtime filesystem only errs the same, safe way.
_pid_recycled() {
  local pid="$1" file="$2" elapsed file_s
  [ -f "$file" ] || return 1
  elapsed="$(_ps_elapsed_s "$pid")" || return 1
  file_s="$(_file_mtime_s "$file")" || return 1
  case "$elapsed" in '' | *[!0-9]*) return 1 ;; esac
  case "$file_s" in '' | *[!0-9]*) return 1 ;; esac
  [ "$(($(date +%s) - 10#$elapsed))" -gt "$((10#$file_s + 2))" ]
}

# _recorded_pid_live <pid> <pidfile> — 0 when <pid> can still be the dispatcher
# <pidfile> records: live and not a later-recycled pid.
_recorded_pid_live() {
  _pid_alive "$1" || return 1
  _pid_recycled "$1" "$2" && return 1
  return 0
}

engine_path() {
  local pinned="$_launcher_runtime_path" entry dir keep
  local -a parts=() pinned_parts=() kept=()
  # A substituted value is a colon-joined list of store dirs, so it starts with
  # `/`; anything else is the raw script, where there is nothing to remove. Same
  # "not absolute means unsubstituted" test _resolve_dir uses.
  if [ "${pinned:0:1}" != '/' ]; then
    printf '%s' "${PATH-}"
    return 0
  fi
  IFS=: read -ra parts <<<"${PATH-}" || true
  IFS=: read -ra pinned_parts <<<"$pinned" || true
  for entry in "${parts[@]}"; do
    keep=1
    for dir in "${pinned_parts[@]}"; do
      if [ -n "$dir" ] && [ "$entry" = "$dir" ]; then
        keep=0
        break
      fi
    done
    if [ "$keep" = 1 ]; then
      kept+=("$entry")
    fi
  done
  local IFS=:
  printf '%s' "${kept[*]}"
}

# _pane_client <tmux args...> — the tmux client that opens a window or a role
# pane, run under engine_path. Only these two calls take it: a pane inherits the
# PATH of the client that opens it, and no other tmux call here opens one. tmux is
# resolved before the swap, while the pinned dirs still hold; a box without tmux,
# or a raw run with nothing to strip, takes plain tmux. The client also drops
# every CREW_ and DISPATCH_ var: a client that starts the tmux server seeds the
# server's global environment with its own, and every later window there inherits
# it — a later dispatch would reuse this crew or session id, or a deleted spec
# path. Each pane gets its CREW_ID and CREW_WORKER_ID explicitly instead.
_pane_client() {
  local cleaned tmux_bin v
  local -a scrub=()
  for v in "${!CREW_@}" "${!DISPATCH_@}"; do
    scrub+=(-u "$v")
  done
  cleaned="$(engine_path)"
  tmux_bin="$(command -v tmux || true)"
  if [ -n "$tmux_bin" ] && [ -n "$cleaned" ] && [ "$cleaned" != "$PATH" ]; then
    env "${scrub[@]}" PATH="$cleaned" "$tmux_bin" "$@"
  else
    env "${scrub[@]}" tmux "$@"
  fi
}

_remote_session_ok() {
  [[ $1 =~ ^[A-Za-z0-9_][A-Za-z0-9_-]*$ ]]
}

# prints this tmux session's name, or crew-<id> outside tmux
_remote_target_session() {
  local name=""
  [ -z "${TMUX_PANE:-}" ] || name="$(tmux display-message -p -t "$TMUX_PANE" '#{session_name}' 2>/dev/null || true)"
  if [ -z "$name" ]; then
    printf 'crew-%s\n' "$1"
    return 0
  fi
  _remote_session_ok "$name" || {
    printf "dispatch: tmux session '%s' cannot name a remote session: it must match ^[A-Za-z0-9_][A-Za-z0-9_-]*\$\n" "$name" >&2
    return 1
  }
  printf '%s\n' "$name"
}

# <host> <crew> <branch> <fallback>: the session the window's stamps put it in
_remote_worker_session() {
  local line
  line="$(crew remote-run --host "$1" --env "CREW_ID=$2" -- crew where "$3" --crew "$2" 2>/dev/null)" || line=""
  if [[ $line =~ ^[^[:space:]]+\ —\ ([A-Za-z0-9_][A-Za-z0-9_-]*):[0-9]+\.[0-9]+\  ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  echo "dispatch: could not ask $1 where $3 is; assuming remote session $4" >&2
  printf '%s\n' "$4"
}

# _remote_window <ssh> <crew_id> <session> — mirror the remote worker window
# (selected by its @crew_id stamp) of <session> into this tmux session, else say
# how to attach. og-remote-open is idempotent, so every dispatch and resume runs
# it. Never fails: the remote worker exists either way.
_remote_window() {
  local into bin reason=""
  local -a launcher=()
  if [ -z "${TMUX_PANE:-}" ]; then
    reason="not inside tmux"
  else
    # tmux-og keeps its launcher off PATH: og-remote-open, else `og remote
    # open`, else the executable the @remote_open_bin tmux option names.
    if command -v og-remote-open >/dev/null 2>&1; then
      launcher=(og-remote-open)
    elif command -v og >/dev/null 2>&1; then
      launcher=(og remote open)
    else
      bin="$(tmux show -gv @remote_open_bin 2>/dev/null || true)"
      if [ -n "$bin" ] && [ -f "$bin" ] && [ -x "$bin" ]; then launcher=("$bin"); fi
    fi
  fi
  if [ -z "$reason" ] && [ "${#launcher[@]}" -eq 0 ]; then
    reason="no og-remote-open launcher (PATH, og remote open, @remote_open_bin)"
  elif [ -z "$reason" ]; then
    into="$(tmux display-message -p -t "$TMUX_PANE" '#{session_name}' 2>/dev/null || true)"
    if [ -z "$into" ]; then
      reason="no tmux session for $TMUX_PANE"
    elif "${launcher[@]}" "$1" "$3" --into "$into" --window "@crew_id=$2" >&2; then
      return 0
    else
      reason="${launcher[*]} failed"
    fi
  fi
  echo "dispatch: remote window not mirrored ($reason); attach with: ssh -t $1 tmux attach -t $3" >&2
}

# _uuid — a random lowercase v4 uuid, the id a claude or pi lead is launched
# with (--session-id) so a resume can find its own session again.
_uuid() {
  local h
  h="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s-%s-4%s-%x%s-%s\n' "${h:0:8}" "${h:8:4}" "${h:13:3}" $(((0x${h:16:1} & 3) | 8)) "${h:17:3}" "${h:20:12}"
}

# _lead_record_safe — succeed when $crew_dir/leads/<branch> and every dir above
# it may be written: mkdir, mktemp and mv all follow a symlink planted at any of
# them. Mirrors the grant-record checks in the dispatch path.
_lead_record_safe() {
  local dir="$crew_dir/leads" part rec="$crew_dir/leads/$branch"
  local -a parts
  IFS=/ read -ra parts <<<"$branch"
  for part in "" "${parts[@]:0:${#parts[@]}-1}"; do
    dir="$dir${part:+/$part}"
    if [ -L "$dir" ] || { [ -e "$dir" ] && [ ! -d "$dir" ]; }; then
      return 1
    fi
  done
  if [ -L "$rec" ] || { [ -e "$rec" ] && [ ! -f "$rec" ]; }; then
    return 1
  fi
}

# _record_lead_session <engine> <id> — record which engine session is the
# lead's, as `<engine> <id>` in $crew_dir/leads/<branch>. Not under artifacts/,
# which is granted to workers. Only a lead launch writes it: role panes share
# the worktree and must never claim the lead's session.
_record_lead_session() {
  local rec="$crew_dir/leads/$branch" tmp
  if ! _lead_record_safe; then
    echo "dispatch: $rec is a symlink or not a regular file — not recording the lead session" >&2
    return 1
  fi
  (
    umask 077
    mkdir -p "$(dirname "$rec")"
    tmp="$(mktemp "$(dirname "$rec")/.lead.XXXXXX")"
    printf '%s %s\n' "$1" "$2" >"$tmp"
    mv -f -- "$tmp" "$rec"
  )
}

# ssh Host aliases are written github.com-<name>. Anything else (a lookalike
# host, an extra @, a slash) is not GitHub.
_github_ssh_host() {
  local host
  host="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  [ "$host" = github.com ] && return 0
  [[ $host =~ ^github\.com-[^@/]+$ ]]
}

# owner/repo from a GitHub path. A trailing slash may sit before or after
# `.git` (`repo/.git`, `repo.git/`, `repo/`). Empty when it is not one slug.
_tracker_slug_from_path() {
  local path="$1"
  while [[ $path == */ ]]; do
    path="${path%/}"
  done
  if [[ $path == *.git ]]; then
    path="${path%.git}"
    while [[ $path == */ ]]; do
      path="${path%/}"
    done
  fi
  if [[ $path =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]]; then
    printf '%s\n' "$path"
  fi
  return 0
}

# _origin_slug — owner/repo of the configured origin when it is a GitHub URL,
# else nothing. Configured origin, not `git remote get-url`: that applies
# insteadOf and would hide a GitHub slug behind a rewritten local path. https
# userinfo is discarded and never printed.
_origin_slug() {
  local url="" rest="" host="" path=""
  url="$(git config --get remote.origin.url 2>/dev/null || true)"
  case "$url" in
  https://*)
    rest="${url#https://}"
    rest="${rest##*@}"
    host="${rest%%/*}"
    host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
    if [ "$host" = github.com ] && [ "$rest" != "$host" ]; then
      path="${rest#*/}"
    fi
    ;;
  ssh://git@*/*)
    rest="${url#ssh://git@}"
    host="${rest%%/*}"
    if _github_ssh_host "$host"; then
      path="${rest#*/}"
    fi
    ;;
  git://*/*)
    rest="${url#git://}"
    host="${rest%%/*}"
    host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
    if [ "$host" = github.com ]; then
      path="${rest#*/}"
    fi
    ;;
  git@*:*)
    rest="${url#git@}"
    host="${rest%%:*}"
    if _github_ssh_host "$host"; then
      path="${rest#*:}"
    fi
    ;;
  *) path="" ;;
  esac
  if [ -n "$path" ]; then
    _tracker_slug_from_path "$path"
  fi
  return 0
}
