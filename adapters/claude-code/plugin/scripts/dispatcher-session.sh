# Hand-kept (not generated from adapters/core): shared gate for the dispatcher-session
# monitor and hooks. Source it; it defines functions only.
# shellcheck shell=bash

# dispatcher_crew_id — print $CREW_ID and return 0 only inside a dispatcher launched
# by `dispatcher`. A worker carries CREW_WORKER_ID, a role pane CREW_ROLE_ID and a
# subagent CREW_SUBAGENT, and all inherit CREW_ID, so none may count.
dispatcher_crew_id() {
  [[ -z ${CREW_WORKER_ID:-} && -z ${CREW_ROLE_ID:-} && -z ${CREW_SUBAGENT:-} ]] || return 1
  [[ -n ${DISPATCHER_LAUNCHER_PID:-} ]] || return 1
  [[ ${CREW_ID:-} =~ ^[A-Za-z0-9._-]+$ && $CREW_ID != . && $CREW_ID != .. ]] || return 1
  printf '%s\n' "$CREW_ID"
}

# dispatcher_crew_dir <id> — the crew's bus directory, or fail outside a git checkout.
dispatcher_crew_dir() {
  local common
  common=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || return 1
  printf '%s\n' "$common/crew/crews/$1"
}

# _bounded <secs> cmd... — run cmd, killing it after secs. Without timeout(1) (stock
# macOS) a watchdog does the killing; the hook's own timeout is the outer backstop.
_bounded() {
  local secs=$1 t pid dog rc
  shift
  for t in timeout gtimeout; do
    if command -v "$t" >/dev/null 2>&1; then
      "$t" "$secs" "$@"
      return
    fi
  done
  "$@" &
  pid=$!
  (sleep "$secs" && kill "$pid") >/dev/null 2>&1 &
  dog=$!
  wait "$pid"
  rc=$?
  kill "$dog" >/dev/null 2>&1
  return "$rc"
}
