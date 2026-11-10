# shellcheck shell=bash disable=SC2154
# Helpers the launchers (dispatch, dispatch-resume, dispatcher, lead-launcher)
# share, so a fix lands once. Sourced, never run. It carries no build-time
# substitution token: each launcher keeps its own baked values and this file
# reads them as plain variables (engine_path reads $_launcher_runtime_path;
# _settings_load reads $dispatch_config_default, the baked fallback for the
# call-time DISPATCH_CONFIG_BIN, which a --spawn-role replay sets between loads;
# the model-class helpers read $model_resolve_bin; _check_protocol_rev reads
# $stamped_rev; the lead and remote helpers read $crew_dir, $branch and the
# --host settings the caller set). dispatch-notify.sh ships as raw plugin text
# and keeps its own _bus_append, compared whole against this one.

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

# _wire_side — which side of a --host dispatch this process is on, read once
# from the env it started with: `remote` when a --host launch delivered
# DISPATCH_TMUX_SESSION, else `local`. The controller side (--host given) is not
# in the env, so dispatch.sh sets `controller` after parsing. Callers branch on
# the value, never on DISPATCH_TMUX_SESSION itself.
_wire_side() {
  if [ -n "${DISPATCH_TMUX_SESSION:-}" ]; then
    printf 'remote\n'
  else
    printf 'local\n'
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
# it. Never fails: the remote worker exists either way. A plain crew-<id>
# session holds only this crew's windows and an older build mirrored it whole,
# so it is mirrored whole again: a filtered mirror there would be a second one.
_remote_window() {
  local into bin reason=""
  local -a launcher=() selector=(--window "@crew_id=$2")
  [ "$3" != "crew-$2" ] || selector=()
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
    elif "${launcher[@]}" "$1" "$3" --into "$into" ${selector[@]+"${selector[@]}"} >&2; then
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

# _resolve_dir <OUT_VAR> <ENV_VAR> <baked> <label> — resolve a DISPATCHER_*_DIR
# override against the baked default (#303). A shell or tmux server that
# outlives a rebuild keeps the previous build's export, so an override under the
# baked path's store root is kept only when its content equals the baked dir's:
# the current build's export sits at a different store path than the baked
# projection but holds the same files. A checkout override always wins, as does
# any override in a raw script (baked is not absolute). diff sits in an `if`
# because its exit 1 means "differs", not failure. A relative override is
# refused: a launched session runs in a task worktree, whose files a diff
# controls, and would resolve it there.
# A stale value is ignored with a notice and unset, so later diagnostics do not
# name it.
_resolve_dir() {
  local out="$1" var="$2" baked="$3" label="$4" val="${!2:-}"
  if [ -z "$val" ]; then
    printf -v "$out" '%s' "$baked"
    return 0
  fi
  if [[ $val != /* ]]; then
    echo "$label: $var must be an absolute path, got: $val" >&2
    exit 1
  fi
  # The dirs are spliced into launch scripts and their prompts, and a role
  # launched by --spawn-role inherits this env from the worker (#470).
  if [[ ! $val =~ ^/[A-Za-z0-9._/+@-]*$ ]]; then
    echo "$label: $var must not contain shell metacharacters or spaces, got: ${val@Q}" >&2
    exit 1
  fi
  if [[ $baked == /* && $val == "${baked%/*}"/* && $val != "$baked" ]] &&
    ! diff -rq -- "$val" "$baked" >/dev/null 2>&1; then
    echo "$label: ignoring stale $var from a previous build: $val; using $baked" >&2
    unset "$var"
    printf -v "$out" '%s' "$baked"
    return 0
  fi
  printf -v "$out" '%s' "$val"
}

engine_cli() {
  case "$1" in
  cursor) printf 'cursor-agent' ;;
  *) printf '%s' "$1" ;;
  esac
}

engine_enabled() {
  case " ${DISPATCH_ENGINES:-$ENGINES_ALL} " in
  *" $1 "*) return 0 ;;
  esac
  return 1
}

# _settings_load — resolve the settings (dispatch-config) into $settings. A
# set env var is kept verbatim, so a whitespace-only DISPATCH_ENGINES still
# enables nothing; the resolver fills only what env leaves empty, unexported.
_settings_load() {
  settings="$("${DISPATCH_CONFIG_BIN:-$dispatch_config_default}")"
  [ -n "${DISPATCH_ENGINES:-}" ] || DISPATCH_ENGINES="$(jq -r '.engines // [] | join(" ")' <<<"$settings")"
  [ -n "${DISPATCH_GRANT_ROOTS:-}" ] || DISPATCH_GRANT_ROOTS="$(jq -r '.grantRoots // [] | join(":")' <<<"$settings")"
}

# _glob_match <model> — true when <model> matches one of the bash globs on
# stdin, one per line.
_glob_match() {
  local glob
  while IFS= read -r glob; do
    # shellcheck disable=SC2053 # the unquoted RHS is the glob
    if [[ $1 == $glob ]]; then return 0; fi
  done
  return 1
}

# _exact_match <model> — true when <model> equals one of the lines on stdin,
# one per line.
_exact_match() {
  local x
  while IFS= read -r x; do
    if [ "$x" = "$1" ]; then return 0; fi
  done
  return 1
}

# _model_class <agent> <model> — <model>'s class (cheap|standard|premium) when
# the model catalog lists it under a `classes` pattern (model-resolve's
# class_of), else nothing. No catalog file means nothing, without running the
# resolver.
_model_class() {
  local t out
  [ -e "$model_catalog" ] || return 0
  t="$(jq -r --arg a "$1" '. as $s | first(.modelMap[$a] // {} | keys_unsorted[] | select($s.classes[$a].tiers[.] != null)) // empty' <<<"$settings")"
  [ -n "$t" ] || return 0
  out="$("$BASH" "$model_resolve_bin" "$1" "$t" "$model_catalog" "$model_class_state" "$2" <<<"$settings" 2>/dev/null)" || {
    echo "dispatch: warning: model-resolve failed for $1; no @class ladder item matches '$2'" >&2
    return 0
  }
  jq -r '.class_of | strings' <<<"$out" 2>/dev/null || true
}

# _class_item <model> <item> <class> — true when an @class ladder item admits
# <model>: `@C` when <class> (the model's _model_class) is C, `@C:<glob>` when
# it is and the glob matches too.
_class_item() {
  local c="${2#@}" g=""
  if [[ $c == *:* ]]; then
    g="${c#*:}" c="${c%%:*}"
  fi
  [ -n "$3" ] && [ "$c" = "$3" ] || return 1
  # shellcheck disable=SC2053 # the unquoted RHS is the glob
  [ -z "$g" ] || [[ $1 == $g ]]
}

# _escalation_hop <agent> <tier> <failed> <model> <inRow|outOfRow> — the
# baseline of the first escalation rule whose `failed` items match <failed>,
# when that rule's <kind> list admits <model> (inRow: a glob or @class item;
# outOfRow: an exact id, never a class, since it widens what the tier gate
# admits). Prints nothing otherwise. An @class item (_class_item) is consulted
# only when a model catalog exists, so with none every rule resolves exactly as
# its globs and ids do; no model id starts with @, so a glob match never takes
# one.
_escalation_hop() {
  local rule="" i glob baseline item c3="" c4="" k3="" k4=""
  while IFS=$'\t' read -r i glob; do
    # shellcheck disable=SC2053 # the unquoted RHS is the glob
    if [[ $glob == @* ]]; then
      [ -e "${model_catalog:-}" ] || continue
      [ -n "$k3" ] || { c3="$(_model_class "$1" "$3")" k3=1; }
      _class_item "$3" "$glob" "$c3" || continue
      rule="$i"
      break
    elif [[ $3 == $glob ]]; then
      rule="$i"
      break
    fi
  done < <(jq -r --arg a "$1" --arg t "$2" '.escalation[$a][$t] // [] | to_entries[] | "\(.key)\t\(.value.failed[])"' <<<"$settings")
  [ -n "$rule" ] || return 0
  if {
    read -r baseline
    if [ "$5" = inRow ]; then _glob_match "$4"; else _exact_match "$4"; fi
  } < <(jq -r --arg a "$1" --arg t "$2" --argjson i "$rule" --arg k "$5" '.escalation[$a][$t][$i] | .baseline, (.[$k] // [])[]' <<<"$settings"); then
    printf '%s' "$baseline"
    return 0
  fi
  [ "$5" = inRow ] && [ -e "${model_catalog:-}" ] || return 0
  while IFS= read -r item; do
    [ -n "$k4" ] || { c4="$(_model_class "$1" "$4")" k4=1; }
    if _class_item "$4" "$item" "$c4"; then
      printf '%s' "$baseline"
      return 0
    fi
  done < <(jq -r --arg a "$1" --arg t "$2" --argjson i "$rule" '.escalation[$a][$t][$i] | (.inRow // [])[] | select(startswith("@"))' <<<"$settings")
}

# _class_sigil <agent> <tier> <@class> — the model id a positional or role
# @cheap, @standard or @premium names (docs/model-classes.md D6): model-resolve's
# default for <tier>'s row when that row's class is <class>, else for <agent>'s
# first row of that class in trivial, standard, deep order. The catalog and
# class-state caches feed it; with neither it is the seed default (claude, which
# has no catalog, gets its alias: D8). Admission is not checked here: the tier
# gate does that. Prints the id, or says why on stderr and fails: an unknown
# class, no tier row taking its default from the class (pi has none for
# premium, though deep admits premium ids), or a resolver that fails.
_class_sigil() {
  local c="${3#@}" t out
  case "$c" in
  cheap | standard | premium) ;;
  *)
    echo "dispatch: unknown model class '$3' — a class sigil is @cheap, @standard or @premium" >&2
    return 1
    ;;
  esac
  t="$(jq -r --arg a "$1" --arg t "$2" --arg c "$c" '. as $s | ($s.classes[$a].tiers // {}) as $r
    | first(($t, "trivial", "standard", "deep") | select($r[.].class == $c and ($s.modelMap[$a][.] | type) == "object")) // empty' <<<"$settings")"
  if [ -z "$t" ]; then
    echo "dispatch: $3 names no $1 model — no $1 tier row takes its default from class $c (classes.$1.tiers), so the class has no default; name a model id instead (dispatch --models lists them)" >&2
    return 1
  fi
  out="$("$BASH" "$model_resolve_bin" "$1" "$t" "$model_catalog" "$model_class_state" <<<"$settings")" || out=""
  out="$(jq -ces 'if length == 1 and (.[0] | type) == "object" then .[0] else empty end
    | walk(if type == "string" then gsub("[[:cntrl:]\\p{Cf}]"; "") else . end)' <<<"$out" 2>/dev/null)" || out=""
  jq -r '.warnings[]? | strings | "dispatch: warning: " + .' <<<"$out" >&2 2>/dev/null || true
  out="$(jq -r '.default | strings' <<<"$out" 2>/dev/null)" || out=""
  if [ -z "$out" ]; then
    echo "dispatch: $3 names no $1 model — model-resolve gave no default for the $1 $t row" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

# _worker_protocol_files <agent> — the worker protocol files a lead of <agent>
# reads: the core and every phase file, as the claude or pi render, or the
# sources for codex and cursor.
_worker_protocol_files() {
  local stem suffix=
  case $1 in
  claude) suffix=.claude ;;
  pi) suffix=.pi ;;
  esac
  for stem in WORKER_PROTOCOL WORKER_GRID WORKER_REBASE WORKER_PLAN WORKER_EXECUTE WORKER_REVIEW WORKER_SHIP; do
    printf '%s\n' "$stem$suffix.md"
  done
}

# _require_protocol_files <dir> <label> <file...> — abort before any scaffolding if
# a required protocol file is missing from $PROTOCOL_DIR. $DISPATCHER_PROTOCOL_DIR
# can point at a stale checkout (#177); this stops the launch instead of
# spawning an engine against a missing --append-system-prompt(-file) target.
_require_protocol_files() {
  local dir="$1" label="$2" f missing=()
  shift 2
  for f in "$@"; do
    [ -f "$dir/$f" ] || missing+=("$f")
  done
  [ "${#missing[@]}" -eq 0 ] && return 0
  local override=""
  [ -n "${DISPATCHER_PROTOCOL_DIR:-}" ] && override=" (DISPATCHER_PROTOCOL_DIR=$DISPATCHER_PROTOCOL_DIR)"
  echo "$label: missing protocol file(s) in \$PROTOCOL_DIR ($dir)${override}: ${missing[*]} — refusing to launch" >&2
  exit 1
}

# _check_protocol_rev <dir> <label> — refuse a protocol directory whose content
# does not match the launcher's baked revision, read from $stamped_rev (#184,
# #193). The build substitutes the content hash of adapters/core/protocols into
# the launcher's own `stamped_rev=` line (this file carries no token itself);
# at runtime this recomputes the same hash from the files actually in $dir and
# refuses a mismatch before any scaffolding. The rule is byte-identical to
# flake.nix's: the directory's files (dotfiles included, matching readDir),
# names sorted byte-wise (matching builtins.attrNames), each hashed —
# `name:sha256;` entries, sha256 of the concatenation, first 16 hex chars. It
# is pinned against the Nix implementation by tests/module.bats, including an
# edge-case dir (dotfile, prefix-named pair) where a naive line-sort or a
# non-dotglob glob would diverge. A stale store path held by a long-lived
# DISPATCHER_PROTOCOL_DIR export is ignored by _resolve_dir before this check;
# what is refused here is a checkout whose content drifted from the script's
# build — it hashes differently. There is no committed PROTOCOL_REV file left
# to go stale, so two PRs editing different protocol files can merge in either
# order. Six small files hash in a few milliseconds. A raw checkout script
# (marker unsubstituted) cannot bind a revision and skips with a one-line
# warning.
_check_protocol_rev() {
  local dir="$1" label="$2" dir_rev entries=""
  local names=() file name sig
  # The sentinel is the placeholder's *shape*, not the literal: flake.nix's
  # replaceStrings (and a test's sed) rewrite every occurrence of the token,
  # so a literal comparison would make a substituted script skip its own
  # check. A baked rev (16 hex chars) never starts with @.
  if [[ $stamped_rev == @* ]]; then
    echo "$label: unsubstituted protocol revision (raw checkout script) — skipping the protocol revision consistency check" >&2
    return 0
  fi
  # Names only, not `name:hash;` lines: sorting full lines diverges from
  # attrNames when one name is a prefix of another ('X' vs 'X1' — line-sort
  # puts X1 first because '1' < ':'). dotglob makes the glob see dotfiles the
  # way readDir does; nullglob keeps an empty dir from globbing a literal '*'
  # into the file set. Sorting by name then hashing in that order mirrors
  # flake.nix's attrNames + map exactly.
  shopt -s dotglob nullglob
  for file in "$dir"/*; do
    [ -f "$file" ] || continue
    names+=("$(basename "$file")")
  done
  shopt -u dotglob nullglob
  if [ ${#names[@]} -gt 0 ]; then
    mapfile -t names < <(printf '%s\n' "${names[@]}" | LC_ALL=C sort)
  fi
  for name in "${names[@]}"; do
    sig="$(sha256sum "$dir/$name" | cut -d' ' -f1)"
    entries+="${name}:${sig};"$'\n'
  done
  # The newlines are a construction convenience only — strip them before
  # hashing, matching flake.nix's concatStringsSep "" (clean line-join with no
  # separator).
  dir_rev="$(printf '%s' "$entries" | tr -d '\n' | sha256sum | cut -d' ' -f1 | cut -c1-16)"
  if [ "$dir_rev" != "$stamped_rev" ]; then
    echo "$label: protocol directory version mismatch — refusing to launch" >&2
    echo "$label:   script protocol revision: $stamped_rev" >&2
    echo "$label:   \$PROTOCOL_DIR content revision: $dir_rev" >&2
    echo "$label:   \$PROTOCOL_DIR: $dir" >&2
    if [ -n "${DISPATCHER_PROTOCOL_DIR:-}" ]; then
      echo "$label:   DISPATCHER_PROTOCOL_DIR override: $DISPATCHER_PROTOCOL_DIR" >&2
      echo "$label:   remedy: unset DISPATCHER_PROTOCOL_DIR, or point it at a checkout matching this build" >&2
    fi
    exit 1
  fi
}

# theme_colour <thm-name> <fallback> — a tmux colour expression that prefers
# the theme's #{@thm_<name>} and falls back to <fallback> when tmux-og has not
# set it (tmux expands an unset option to empty, so `fg=` would be dropped
# silently and the glyph would inherit whatever colour preceded it).
theme_colour() { printf '#{?#{@thm_%s},#{@thm_%s},%s}' "$1" "$1" "$2"; }

# state_glyph <restore> — the state→glyph+colour widget shared by the lead's
# window border and every role pane's border. <restore> is re-emitted after the
# glyph so the rest of the label keeps the border's own tint. tmux evaluates
# the #{?} branches at render time, so @crew_state can change without the
# format being re-set. Unknown/empty state renders the idle glyph.
state_glyph() {
  local restore="$1" c_work c_idle c_block c_done c_fail
  c_work="$(theme_colour green green)"
  c_idle="$(theme_colour overlay_1 colour240)"
  c_block="$(theme_colour peach colour180)"
  c_done="$(theme_colour green green)"
  c_fail="$(theme_colour red red)"
  printf '#{?#{==:#{@crew_state},working},#[fg=%s]●#[fg=%s],#{?#{==:#{@crew_state},idle},#[fg=%s]○#[fg=%s],#{?#{==:#{@crew_state},blocked},#[fg=%s]⚠#[fg=%s],#{?#{==:#{@crew_state},done},#[fg=%s]✓#[fg=%s],#{?#{==:#{@crew_state},pr_open},#[fg=%s]✓#[fg=%s],#{?#{==:#{@crew_state},failed},#[fg=%s]✗#[fg=%s],#{?#{==:#{@crew_state},exited},#[fg=%s]✗#[fg=%s],#[fg=%s]○#[fg=%s]}}}}}}}' \
    "$c_work" "$restore" "$c_idle" "$restore" "$c_block" "$restore" \
    "$c_done" "$restore" "$c_done" "$restore" "$c_fail" "$restore" \
    "$c_fail" "$restore" "$c_idle" "$restore"
}

grid_lead_format() {
  printf ' %s #[bold]#{@crew_name}#[nobold] lead · #{@crew_state}#{?#{==:#{@crew_source},watchdog}, (watchdog),}#{?#{@crew_detail}, · #{@crew_detail},}#{?#{@crew_dispatcher_host}, · ⇄ #{@crew_dispatcher_host},} ' "$(state_glyph '#{@crew_color}')"
}

# _ensure_roster_render <crew_id> <pane or empty> — start the crew's roster
# renderer, or retarget a running one. crew accepts the pane only when its shell
# is an ancestor of the crew process, so the call is synchronous (--detach
# returns at once): a nohup'd child can be reparented after this script exits,
# which would defeat that check. `if . == false` because `// true` would turn
# an explicit false into true.
_ensure_roster_render() {
  local flags=()
  [ -z "$2" ] || flags+=(--pane "$2")
  if [ "$(jq -r '.rosterDiagram.autoOpen | if . == false then "false" else "true" end' <<<"$settings")" = false ]; then
    flags+=(--no-open)
  fi
  CREW_ID="$1" crew roster-render --crew "$1" ${flags[@]+"${flags[@]}"} --detach >/dev/null 2>&1 || true
}

# shell_quote <var> <text> — set <var> to <text> as ONE single-quoted shell word,
# for splicing into a tmux send-keys command line the pane's own shell re-parses.
# ' and \ are closed out of the quotes and escaped outside them, the one form
# bash and fish agree on: fish (the pane shell) reads \\ and \' as escapes even
# inside single quotes. Not printf %q: that emits $'…' for non-printables or
# under a C locale, which fish cannot parse.
shell_quote() {
  local -n _out="$1"
  local _text="$2" _res="" _c _i
  for ((_i = 0; _i < ${#_text}; _i++)); do
    _c="${_text:_i:1}"
    case "$_c" in
    "'") _res+="'\\''" ;;
    \\) _res+="'\\\\'" ;;
    *) _res+="$_c" ;;
    esac
  done
  _out="'$_res'"
}

# write_launch_script <var> <cmdline> [exit] — write <cmdline> into a fresh 0700
# script under $crew_dir/launch and set <var> to the short line that runs it. A
# new pane's shell is often not reading yet when send-keys types into it, and
# the tty's canonical buffer (1024 bytes on macOS) cuts a longer line and drops
# its Enter (#298) — so a pane is only ever typed `bash <path>`. `exec env`
# makes the engine the pane's foreground process itself, which every
# #{pane_current_command} liveness check reads. Files older than a week are
# pruned on the way in.
#
# With `exit` the script is named exit.* and deletes itself when it runs: a
# role's exit hook runs only when its engine returns, possibly weeks later, so
# the age prune never reaches it while its pane is alive. A reaped pane never
# runs the hook, so an old exit.* whose pane is gone is reclaimed instead
# (issue #343).
#
# The launching process's resolved DISPATCH_GRANT_ROOTS is always pinned — `:`
# when empty, which every reader takes as no roots — so a lead's
# `dispatch --spawn-role` validates grants against the roots its dispatcher
# used, never re-resolving them from the pane's (tmux server's) env or locked
# layer (#572).
write_launch_script() {
  local -n _launch="$1"
  local _dir="$crew_dir/launch" _file _quoted
  # mkdir -p succeeds on a symlink to a dir, and every write would land in its target.
  if [ -L "$_dir" ] || { [ -e "$_dir" ] && [ ! -d "$_dir" ]; }; then
    echo "dispatch: $_dir is a symlink or not a directory — refusing to write a launch script" >&2
    exit 1
  fi
  # shellcheck disable=SC2174 # $crew_dir already exists; -m only needs to reach the new leaf, and chmod below covers a pre-existing one too
  mkdir -p -m 700 "$_dir"
  chmod 700 "$_dir"
  find "$_dir" -type f -name 'launch.*' -mtime +7 -delete 2>/dev/null || true
  # An exit.* deletes itself when its engine returns, but a reaped pane never
  # runs it, so the file leaks (#343). Reclaim an old exit.* only once the pane
  # it names is gone; a reused pane id just delays deletion. tmux failing, or
  # listing no panes, deletes nothing (fail-safe).
  local _panes _ef _epane
  if _panes="$(tmux list-panes -a -F '#{pane_id}' 2>/dev/null)" && [ -n "$_panes" ]; then
    while IFS= read -r _ef; do
      _epane="$(sed -n "s/.*--pane '\(%[0-9][0-9]*\)'.*/\1/p" "$_ef" 2>/dev/null || true)"
      _epane="${_epane%%$'\n'*}"
      [ -n "$_epane" ] || continue
      printf '%s\n' "$_panes" | grep -qxF -- "$_epane" || rm -f -- "$_ef"
    done < <(find "$_dir" -type f -name 'exit.*' -mtime +7 2>/dev/null)
  fi
  if [ "${3:-}" = exit ]; then
    _file="$(mktemp "$_dir/exit.XXXXXX")"
    # shellcheck disable=SC2016 # the literal "$0" is the generated script's own, expanded when IT runs, not now
    printf '#!/usr/bin/env bash\nrm -f -- "$0"\nexec env -u DISPATCHER_PROTOCOL_DIR -u DISPATCHER_SKILLS_DIR -u DISPATCHER_REVIEWERS_DIR -u DISPATCHER_CRITICS_DIR -u DISPATCHER_PI_DIR %s\n' "$2" >"$_file"
  else
    local _dirs="" _unset="" _n _v
    for _n in PROTOCOL SKILLS REVIEWERS CRITICS PI; do
      _v="${_n}_DIR"
      _v="${!_v:-}"
      if [[ $_v == /* ]]; then
        printf -v _dirs '%sDISPATCHER_%s_DIR=%q ' "$_dirs" "$_n" "$_v"
      else
        printf -v _unset '%s-u DISPATCHER_%s_DIR ' "$_unset" "$_n"
      fi
    done
    printf -v _dirs '%sDISPATCH_GRANT_ROOTS=%q ' "$_dirs" "${DISPATCH_GRANT_ROOTS:-:}"
    _file="$(mktemp "$_dir/launch.XXXXXX")"
    printf '#!/usr/bin/env bash\nexec env %s%s%s\n' "$_unset" "$_dirs" "$2" >"$_file"
  fi
  chmod 700 "$_file"
  shell_quote _quoted "$_file"
  _launch="bash $_quoted"
}

# _artifacts_dir_bad <branch> — succeed, printing the first offender, when a
# component from $crew_dir/artifacts down to the branch's leaf is a symlink or
# exists as a non-directory. mkdir -p and a redirect follow a symlink planted at
# any of them, including the parents of a slashed branch (#446).
_artifacts_dir_bad() {
  local p="$crew_dir/artifacts" part
  local -a parts
  IFS=/ read -ra parts <<<"$1"
  for part in "" "${parts[@]}"; do
    p="$p${part:+/$part}"
    if [ -L "$p" ] || { [ -e "$p" ] && [ ! -d "$p" ]; }; then
      printf '%s\n' "$p"
      return 0
    fi
  done
  return 1
}

# _protocol_dirs_record_bad — succeed, printing the first offender, when
# $crew_dir/protocol-dirs, a dir above a slashed branch's leaf, or the record
# itself is a symlink or the wrong type (mkdir/mv/reads follow a planted link).
_protocol_dirs_record_bad() {
  local p="$crew_dir/protocol-dirs" part rec="$crew_dir/protocol-dirs/$branch"
  local -a parts
  IFS=/ read -ra parts <<<"$branch"
  for part in "" "${parts[@]:0:${#parts[@]}-1}"; do
    p="$p${part:+/$part}"
    if [ -L "$p" ] || { [ -e "$p" ] && [ ! -d "$p" ]; }; then
      printf '%s\n' "$p"
      return 0
    fi
  done
  if [ -L "$rec" ] || { [ -e "$rec" ] && [ ! -f "$rec" ]; }; then
    printf '%s\n' "$rec"
    return 0
  fi
  return 1
}

# _settings_env_names — the env vars that steer _settings_load and the pi seed.
# LOCAL_MODELS_LIB and DISPATCH_CONFIG_BIN are pinned for the `crew pi-agent-dir`
# child, which sources/runs them to write the shared models.json (dispatch.sh's
# own startup sourcing is the caller's own code).
_settings_env_names() {
  printf '%s\n' XDG_CONFIG_HOME DISPATCH_LOCKED_SETTINGS DISPATCH_ENGINES \
    DISPATCH_GRANT_ROOTS DISPATCH_OPENROUTER_MONTHLY_USD DISPATCH_OPENROUTER_KEY_FILE \
    DISPATCH_PROFILE DISPATCH_REPO_TRACKERS DISPATCH_ORG_TRACKERS \
    DISPATCH_ROSTER_AUTO_OPEN PI_CODING_AGENT_DIR DISPATCH_CONFIG_BIN LOCAL_MODELS_LIB
}

# _settings_env_json — those vars as one JSON object, null when unset, except
# XDG_CONFIG_HOME, PI_CODING_AGENT_DIR (a leading ~/ expanded here)
# and DISPATCH_GRANT_ROOTS, recorded with their defaults so a reader never falls
# back to its caller's HOME or locked grantRoots. Path values are recorded absolute.
# DISPATCH_ENGINES and DISPATCH_GRANT_ROOTS are the already-resolved values
# (_settings_load ran first): a deliberate snapshot. The rest are inputs, so a
# reader re-resolves settings from the dispatcher's own files.
_settings_env_json() {
  local n v
  local -a args=()
  while IFS= read -r n; do
    case "$n" in
    XDG_CONFIG_HOME)
      v="${XDG_CONFIG_HOME:-$HOME/.config}"
      [[ $v == /* ]] || v="$HOME/.config"
      args+=(--arg "$n" "$v")
      ;;
    DISPATCH_LOCKED_SETTINGS | DISPATCH_OPENROUTER_KEY_FILE)
      if [ -z "${!n+x}" ]; then
        args+=(--argjson "$n" null)
      else
        v="${!n}"
        [[ -z $v || $v == /* ]] || v="$(realpath -m -- "$v")"
        args+=(--arg "$n" "$v")
      fi
      ;;
    PI_CODING_AGENT_DIR)
      v="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"
      case "$v" in \~/*) v="$HOME/${v#\~/}" ;; esac
      [[ $v == /* ]] || v="$HOME/.pi/agent"
      args+=(--arg "$n" "$v")
      ;;
    DISPATCH_GRANT_ROOTS) args+=(--arg "$n" "${DISPATCH_GRANT_ROOTS:-:}") ;;
    *)
      if [ -n "${!n+x}" ]; then
        args+=(--arg "$n" "${!n}")
      else
        args+=(--argjson "$n" null)
      fi
      ;;
    esac
  done < <(_settings_env_names)
  jq -cn '$ARGS.named' "${args[@]}"
}

# _record_protocol_dirs <worktree> — record the resolved protocol dirs, the
# worktree they belong to, the settings env (_settings_env_json), and the pi
# adapter dir (last, so older records keep their indices), for --spawn-role
# (#496, #733): dispatcher-written, outside every prompt-free write grant a
# worker holds.
_record_protocol_dirs() {
  local rec="$crew_dir/protocol-dirs/$branch" n v tmp
  local -a lines=()
  for n in PROTOCOL_DIR SKILLS_DIR REVIEWERS_DIR CRITICS_DIR; do
    v="${!n}"
    [[ $v == /* ]] || v=""
    lines+=("$v")
  done
  lines+=("$(realpath -e -- "$1")" "$(_settings_env_json)")
  v="$PI_DIR"
  [[ $v == /* ]] || v=""
  lines+=("$v")
  (
    umask 077
    mkdir -p "$(dirname "$rec")"
    tmp="$(mktemp "$(dirname "$rec")/.dirs.XXXXXX")"
    printf '%s\n' "${lines[@]}" >"$tmp"
    mv -f -- "$tmp" "$rec"
  )
}

# claude_lean_env — emit the env prefix of a claude worker launch that drops the
# claude.ai account connectors (Gmail, Drive, Sentry, ...). Workers never use
# them and their tool names ride every turn's context. DISPATCH_CLAUDE_CONNECTORS=1
# opts out. Plugin and base MCP servers are untouched. Prompt suggestions are ghost
# text no worker pane reader wants, and each one is a background model request.
claude_lean_env() {
  if [ "${DISPATCH_CLAUDE_CONNECTORS:-}" = 1 ]; then
    printf 'DISPATCH_CLAUDE_CONNECTORS=1 '
  else
    printf 'ENABLE_CLAUDEAI_MCP_SERVERS=false '
  fi
  printf 'CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false '
}

# launch_dir_args <engine> <branch> — emit the ` --add-dir <dir>` flags a claude
# launch needs so its tool calls never stop on a permission dialog nobody
# watches: the protocol, skills, reviewers and critics dirs, the branch's own
# artifacts dir, and the dispatch-time grants in $crew_dir/grants/<branch>.
#
# --add-dir makes a working directory: reads are prompt-free and edits follow
# the permission mode, which auto allows. The four protocol dirs are read-only
# (#442): a worker must never edit its own reviewer briefs, critics, skills or
# WORKER_PROTOCOL.md/GRID_PROTOCOL.md, so each gets `--add-dir` (reads stay
# prompt-free) plus an `Edit(//<dir>/**)` deny rule via `--disallowedTools`
# (verified: a `Read` allow rule does not override
# blockReadsOutsideWorkingDirectories, so `--add-dir` stays the only way to
# keep reads prompt-free). The artifacts dir and explicit grants stay plain
# write-capable `--add-dir`, re-validated on every launch. The record is the
# only authority: the worker edits WORKER_TASK.md, so its add_dir: header
# lines are a mirror, never read.
#
# claude's --add-dir and --disallowedTools are both variadic and would swallow
# the positional prompt, so callers splice this in right before
# --append-system-prompt-file, terminating both lists.
#
# Other engines get nothing: codex runs with
# --dangerously-bypass-approvals-and-sandbox and cursor with --force, so neither
# gates paths, and pi has no tool-permission layer at all.
launch_dir_args() {
  [ "$1" = claude ] || return 0
  local a c d bad line dirs=() rules=()
  local -A seen=()
  for d in "$PROTOCOL_DIR" "$SKILLS_DIR" "$REVIEWERS_DIR" "$CRITICS_DIR"; do
    [[ $d == /* ]] && [ -d "$d" ] || continue
    d="${d%/}"
    c="$(realpath -e -- "$d")"
    if [[ ! $d =~ ^/[A-Za-z0-9._/+@-]*$ || ! $c =~ ^/[A-Za-z0-9._/+@-]*$ ]]; then
      echo "dispatch: not granting $d — its path cannot be written as a read-only rule" >&2
      continue
    fi
    dirs+=("$d")
    rules+=("Edit(/$d/**)")
    [ "$c" = "$d" ] || rules+=("Edit(/$c/**)")
  done
  a="$crew_dir/artifacts/$2"
  if bad="$(_artifacts_dir_bad "$2")"; then
    echo "dispatch: $bad is a symlink or not a directory — not granting $a to $2" >&2
  else
    mkdir -p -- "$a"
    dirs+=("$a")
  fi
  if [ -f "$crew_dir/grants/$2" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      [ -n "$line" ] || continue
      if d="$(_add_dir_ok "$line")"; then
        dirs+=("$d")
      else
        echo "dispatch: dropping invalid grant '$line' for $2" >&2
      fi
    done <"$crew_dir/grants/$2"
  fi
  for d in "${dirs[@]}"; do
    [ -z "${seen[$d]:-}" ] || continue
    seen[$d]=1
    printf ' --add-dir %q' "$d"
  done
  if [ "${#rules[@]}" -gt 0 ]; then
    printf ' --disallowedTools'
    printf ' %q' "${rules[@]}"
  fi
}

# pi_skill_args <worktree> — emit --skill flags for the worktree's own project
# skill dirs (pi's project skill locations) and for the harness's own skills.
# The pi launches below pass --no-approve, which disables project discovery
# wholesale, so a worker must be handed its skills explicitly; --skill is
# additive and a missing path is only a warning. Only skills cross this line —
# project .pi settings, packages and extensions stay blocked, which is why this
# isn't just dropping --no-approve.
#
# $SKILLS_DIR carries the harness's own skills, which WORKER_PROTOCOL cites as
# the authority for the plan schema and the critic table. pi is the only engine
# with no adapter tree of its own to load them from.
pi_skill_args() {
  local wt="$1" d
  for d in "$wt/.pi/skills" "$wt/.agents/skills" "$SKILLS_DIR"; do
    [ -d "$d" ] && printf ' --skill %q' "$d"
  done
  return 0
}

# Escalation helpers — query the bus for prior failed workers. The
# one-rung-up hops themselves live in defaults.json (_escalation_hop). The
# rules there encode three limits: a trivial row never escalates above itself
# (#249); standard/trivial opus has no in-row rung above it, so the dispatcher
# re-tiers that failure to deep; and pi deep has none, so a failed deep pi
# worker is re-dispatched on another engine.
# _prior_failed_model <branch> <crew_dir> <tier> — prints the model of the
# dispatch that the branch's latest terminal worker status (failed/done/pr_open)
# ended, provided that status is `failed` and that dispatch ran at <tier>. The
# dispatch is the failing worker's own session; a resume-only session falls back
# to the latest dispatch before the failure. Prints nothing otherwise.
_prior_failed_model() {
  local branch="$1" dir="$2" tier="$3" events
  events="$dir/events.jsonl"
  [ -f "$events" ] || return 0
  jq -r --arg b "$branch" --arg t "$tier" '
    [., inputs] | . as $all
    | ([$all[] | select(.kind == "status" and
        ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b
        and (.body.state == "failed" or .body.state == "done" or .body.state == "pr_open"))]
        | sort_by(.ts) | last) as $last
    | if $last == null or $last.body.state != "failed" then empty
      else ($last.from | sub("^worker:[^#]*#"; "")) as $sess
      | ($all | map(select(.kind == "dispatch" and .branch == $b and .ts < $last.ts))) as $ds
      | ((($ds | map(select(.session == $sess)) | last)
          // ($ds | sort_by(.ts) | last))) as $d
      | if $d != null and $d.tier == $t then $d.model // empty else empty end
      end
  ' "$events" 2>/dev/null || true
}

# _prior_failed_escalation_available <branch> <crew_dir> — returns 0 if:
# 1. The branch's latest terminal worker status (failed/done/pr_open) is
#    `failed`, AND that worker's session has a matching dispatch or resume
#    event on the same branch (anti-spoofing), AND
# 2. No dispatch or resume event on this branch carries escalated_from, AND no
#    dispatch followed the first failure (an unstamped record-only hop or a
#    same-model retry is an attempt too).
_prior_failed_escalation_available() {
  local branch="$1" dir="$2" events
  events="$dir/events.jsonl"
  [ -f "$events" ] || return 1
  # Check 1: the latest terminal status is a failure posted by a session that
  # was really dispatched (or resumed) on this branch.
  jq -e --arg b "$branch" '
    [., inputs] | . as $all
    | ([$all[] | select((.kind == "dispatch" or .kind == "resume") and .branch == $b) | .session]) as $sessions
    | [$all[] | select(.kind == "status" and .from != null
        and ((.from | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b)
        and (.body.state == "failed" or .body.state == "done" or .body.state == "pr_open"))]
    | sort_by(.ts) | last as $last
    | $last != null and $last.body.state == "failed"
      and ($sessions | index($last.from | sub("^worker:[^#]*#"; ""))) != null
  ' "$events" >/dev/null 2>&1 || return 1
  # Check 2: one-shot. No escalation stamp yet, and no dispatch after the
  # first failure (dispatch rows cover in-row hops, which are never stamped).
  jq -e --arg b "$branch" '
    [., inputs] | . as $all
    | ([$all[] | select(.kind == "status" and ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b
        and .body.state == "failed") | .ts] | min) as $first
    | ([$all[] | select((.kind == "dispatch" or .kind == "resume") and .branch == $b and (.escalated_from // "" | length > 0))] | length) as $stamped
    | ([$all[] | select(.kind == "dispatch" and .branch == $b and .ts > $first)] | length) as $later
    | $stamped == 0 and $later == 0
  ' "$events" >/dev/null 2>&1 || return 1
  return 0
}

# _child_on_chain <pid> — the pid on this process's ancestor chain whose parent
# is <pid>, i.e. <pid>'s direct child above us. The walk is bounded at 64 hops.
_child_on_chain() {
  local target=$1 pid=$$ ppid hop=0
  while [ -n "$pid" ] && [ "$pid" != 0 ] && [ "$pid" != 1 ] && [ "$hop" -lt 64 ]; do
    ppid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -cd '0-9') || return 1
    [ -n "$ppid" ] || return 1
    if [ "$ppid" = "$target" ]; then
      printf '%s' "$pid"
      return 0
    fi
    pid=$ppid
    hop=$((hop + 1))
  done
  return 1
}
