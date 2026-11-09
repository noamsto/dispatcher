#!/usr/bin/env bash
# lead-launcher — run a worker lead's engine in a loop in its own pane, so the
# lead can be relaunched in a fresh engine session at the execute and review
# seams (#700) while its pane, environment, worker id, crew and role panes stay.
#
# Outer mode (written into the lead's launch script by dispatch):
#   lead-launcher --engine <claude|pi> --crew-dir <dir> --branch <branch> \
#     --note <file> --sid <uuid> --prompt <text> \
#     --execute-prompt <text> --review-prompt <text> -- <engine> <args…>
# Inner mode (run by the lead from its shell tool, after writing the note; the
# outer mode exports its own absolute path, since the engine's PATH lacks it):
#   "$CREW_LEAD_LAUNCHER" --fresh <execute|review>
set -euo pipefail

# #936: @launcherRuntimePath@, substituted by flake.nix's launcherPath, is this
# launcher's own pinned tool PATH — the store dirs its preamble prepends.
# engine_path prints the caller's PATH with exactly those dirs dropped, every
# other entry in order; the launches below run under `env PATH=…`, so the engine
# resolves `crew` through the user profile while every launcher lookup above
# stays pinned. A raw run from a checkout leaves the placeholder literal and
# leaves PATH untouched.
_launcher_runtime_path='@launcherRuntimePath@'

# Helpers every launcher shares (launcher-common.sh). A run from a checkout, or a
# copy of this script, finds it through LAUNCHER_COMMON_LIB or beside the script.
launcher_common_lib="${LAUNCHER_COMMON_LIB:-@launcherCommonLib@}"
# replaceStrings rewrites the token on both sides of an equality check, so a
# string compare against the placeholder always matches after bake.
if [ ! -f "$launcher_common_lib" ]; then
  launcher_common_lib="$(dirname -- "${BASH_SOURCE[0]}")/launcher-common.sh"
fi
# shellcheck source=/dev/null
. "$launcher_common_lib"
# _child_on_chain <pid> — the pid on this process's ancestor chain whose parent
# is <pid>, i.e. <pid>'s direct child above us. Copied from dispatcher.sh.
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

# _seam_refusal <done> <stage> — why <stage> may not relaunch after the
# comma-joined stages in <done>; prints nothing when it may. Once per stage, and
# never execute after review: the review seam recurs on every review→fix round.
_seam_refusal() {
  case "$2" in
  execute | review) ;;
  *)
    printf 'unknown stage %s (execute or review)' "${2@Q}"
    return 0
    ;;
  esac
  if [[ ,$1, == *",$2,"* ]]; then
    printf 'already relaunched at the %s seam' "$2"
  elif [ "$2" = execute ] && [[ ,$1, == *,review,* ]]; then
    printf 'execute seam after the review seam'
  fi
}

_refuse() {
  echo "lead-launcher: $1 — not relaunching; continue in this session" >&2
  exit 1
}

if [ "${1:-}" = --fresh ]; then
  if [ "$#" -ne 2 ]; then
    echo 'lead-launcher: usage: lead-launcher --fresh <execute|review>' >&2
    exit 2
  fi
  stage=$2
  # A pi subagent child inherits the lead's env, and ending the lead from there
  # would drop the subagent's caller mid-turn.
  [ -z "${CREW_SUBAGENT:-}" ] || _refuse "CREW_SUBAGENT is set: only the lead relaunches, never a subagent"
  why=$(_seam_refusal "${CREW_LEAD_SEAMS:-}" "$stage")
  [ -z "$why" ] || _refuse "$why"
  launcher_pid=${CREW_LEAD_LAUNCHER_PID:-}
  [ -n "$launcher_pid" ] || _refuse "CREW_LEAD_LAUNCHER_PID is unset: this session does not run under a lead-launcher"
  # _child_on_chain matches the target before its init guard, so pid 1 would
  # resolve to the top-most ancestor (the tmux server).
  if ! [[ $launcher_pid =~ ^[1-9][0-9]*$ ]] || [ "$launcher_pid" = 1 ]; then
    _refuse "CREW_LEAD_LAUNCHER_PID=$launcher_pid is not a launcher pid (an integer above 1)"
  fi
  marker=${CREW_LEAD_RESTART:-}
  note=${CREW_LEAD_NOTE:-}
  since=${CREW_LEAD_SINCE:-}
  if [[ $marker != /* || $note != /* || $since != /* ]]; then
    _refuse "CREW_LEAD_RESTART, CREW_LEAD_NOTE and CREW_LEAD_SINCE must be absolute paths"
  fi
  engine_pid=$(_child_on_chain "$launcher_pid") ||
    _refuse "CREW_LEAD_LAUNCHER_PID=$launcher_pid is not an ancestor of this process"
  # The engine's args carry "lead-launcher" too (its prompt names the command):
  # only the launcher is one, whose comm is bash (macOS) or its script name
  # (Linux).
  launcher_comm=$(ps -o comm= -p "$launcher_pid" 2>/dev/null) || launcher_comm=""
  launcher_comm=${launcher_comm##*/}
  launcher_comm=${launcher_comm#-}
  case "$launcher_comm" in bash | lead-launcher) ;; *) launcher_comm="" ;; esac
  if [[ $(ps -o args= -p "$launcher_pid" 2>/dev/null) != *lead-launcher* ]] || [ -z "$launcher_comm" ]; then
    _refuse "CREW_LEAD_LAUNCHER_PID=$launcher_pid is an ancestor but not a lead-launcher"
  fi
  [ ! -L "$note" ] || _refuse "handoff note $note is a symlink — write a regular file"
  if [ ! -f "$note" ] || [ ! -s "$note" ]; then
    _refuse "handoff note $note is not a non-empty regular file — write it first"
  fi
  [ -f "$since" ] || _refuse "session stamp $since is missing: this session does not run under a lead-launcher"
  # A note from an earlier session (the previous seam's, or a past run's) is
  # strictly older than this session's stamp; one written in the stamp's own
  # mtime tick ties it and passes.
  if [ -n "$(find "$since" -prune -newer "$note" 2>/dev/null)" ]; then
    _refuse "handoff note $note predates this session — rewrite it for this seam"
  fi
  marker_tmp=$(mktemp "$(dirname "$marker")/.restart.XXXXXX")
  printf '%s %s\n' "$(date +%s)" "$stage" >"$marker_tmp"
  mv -f -- "$marker_tmp" "$marker"
  echo "lead-launcher: relaunching this lead fresh at the $stage seam (note: $note); this session ends now" >&2
  # The engine may already be gone; the polls below read that as success. The
  # marker stays through the KILL: an engine that exits late still relaunches.
  kill -TERM "$engine_pid" 2>/dev/null || true
  for _ in $(seq 20); do
    sleep 0.5
    kill -0 "$engine_pid" 2>/dev/null || exit 0
  done
  kill -KILL "$engine_pid" 2>/dev/null || true
  for _ in $(seq 4); do
    sleep 0.5
    kill -0 "$engine_pid" 2>/dev/null || exit 0
  done
  rm -f -- "$marker"
  echo "lead-launcher: relaunch failed — engine pid $engine_pid still runs after SIGKILL; marker removed, this session continues" >&2
  exit 1
fi

engine="" crew_dir="" branch="" note="" sid="" prompt="" execute_prompt="" review_prompt=""
while [ $# -gt 0 ]; do
  case "$1" in
  --)
    shift
    break
    ;;
  --engine | --crew-dir | --branch | --note | --sid | --prompt | --execute-prompt | --review-prompt)
    [ $# -ge 2 ] && [ -n "$2" ] || {
      echo "lead-launcher: $1 needs a value" >&2
      exit 2
    }
    case "$1" in
    --engine) engine=$2 ;;
    --crew-dir) crew_dir=$2 ;;
    --branch) branch=$2 ;;
    --note) note=$2 ;;
    --sid) sid=$2 ;;
    --prompt) prompt=$2 ;;
    --execute-prompt) execute_prompt=$2 ;;
    --review-prompt) review_prompt=$2 ;;
    esac
    shift 2
    ;;
  *)
    echo "lead-launcher: unknown argument ${1@Q}" >&2
    exit 2
    ;;
  esac
done
engine_cmd=("$@")
case "$engine" in
claude | pi) ;;
*)
  echo "lead-launcher: --engine must be claude or pi" >&2
  exit 2
  ;;
esac
if [[ $crew_dir != /* || $note != /* ]] || [ -z "$branch" ] || [ -z "$sid" ] ||
  [ -z "$prompt" ] || [ -z "$execute_prompt" ] || [ -z "$review_prompt" ] || [ "${#engine_cmd[@]}" -eq 0 ]; then
  echo "lead-launcher: needs an absolute --crew-dir and --note, --branch, --sid, --prompt, --execute-prompt, --review-prompt, and -- <engine> <args>" >&2
  exit 2
fi

# Computed while the pinned PATH is still in effect; `env` itself and the tools
# the launcher still calls after the launch resolve as before.
engine_env_path="$(engine_path)"

self=$0
[[ $self == /* ]] || self="$PWD/$self"
marker="$crew_dir/launch/lead.$$.restart"
since="$crew_dir/launch/lead.$$.since"
mkdir -p "$crew_dir/launch"
# A recycled pid could inherit a dead launcher's marker.
rm -f -- "$marker" "$since"
trap 'rm -f -- "$since"' EXIT
seams=""
export CREW_LEAD_LAUNCHER="$self" CREW_LEAD_LAUNCHER_PID=$$ CREW_LEAD_RESTART="$marker" \
  CREW_LEAD_NOTE="$note" CREW_LEAD_SINCE="$since" CREW_LEAD_SEAMS="$seams"

# Its own process group makes the engine the tty's foreground job, so
# #{pane_current_command} names the engine, not this bash: every pane-liveness
# reader would take `bash` for a dead engine.
set -m
# Inline, not a function: a function left of || runs with errexit off for its
# whole body, and only the engine's own exit may be tolerated.
while :; do
  rc=0
  : >"$since"
  # At 1s mtime granularity the previous session's note can tie the new stamp,
  # and a tie passes; dated far back, it is strictly older than any stamp.
  # -h back-dates a planted symlink itself, never its target.
  if [ -e "$note" ] || [ -L "$note" ]; then touch -h -t 200001010000 -- "$note"; fi
  env PATH="$engine_env_path" "${engine_cmd[@]}" --session-id "$sid" "$prompt" || rc=$?

  [ -f "$marker" ] || break
  marker_ts="" stage=""
  read -r marker_ts stage _ <"$marker" || true
  rm -f -- "$marker"
  why=""
  if ! [[ $marker_ts =~ ^[0-9]+$ ]]; then
    why="stale restart marker (no timestamp)"
  else
    marker_age=$(($(date +%s) - 10#$marker_ts))
    if [ "${marker_age#-}" -gt 120 ]; then
      why="stale restart marker (${marker_age}s old)"
    else
      why=$(_seam_refusal "$seams" "$stage")
    fi
  fi
  if [ -n "$why" ]; then
    # The SessionEnd hook skipped `exited` because the marker existed, so this
    # is the only post the run's end gets.
    echo "lead-launcher: $why — the engine ended without a relaunch" >&2
    crew status "${CREW_WORKER_ID:-}" failed "lead-launcher: $why" || true
    break
  fi

  sid=$(_uuid)
  _record_lead_session "$engine" "$sid" || true
  row=$(jq -nc --arg crew "${CREW_ID:-}" --arg branch "$branch" --arg worker "${CREW_WORKER_ID:-}" \
    --arg engine "$engine" --arg stage "$stage" --arg sid "$sid" \
    '{ts:(now*1000|floor), crew_id:$crew, kind:"relaunch", branch:$branch, worker_id:$worker, engine:$engine, stage:$stage, engine_session:$sid}') &&
    _bus_append "$crew_dir/events.jsonl" "$row" || true
  seams=${seams:+$seams,}$stage
  export CREW_LEAD_SEAMS="$seams"
  case "$stage" in
  execute) prompt=$execute_prompt ;;
  review) prompt=$review_prompt ;;
  esac
  echo "lead-launcher: relaunching $engine fresh at the $stage seam (session $sid)" >&2
done
exit "$rc"
