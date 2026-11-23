#!/usr/bin/env bash
# Hand-kept (not generated from adapters/core). Plugin monitor: keeps `crew stream`
# running for a dispatcher session, so its bus watch outlives the Monitor tool's 30
# minute deadline. Each stdout line reaches Claude as a notification.
#
# Silent exit 0 for anything but a dispatcher session. `--promoted` serves an
# in-session promotion, which has no launcher env: the crew is found by the engine's
# pid in `crew crews`.
#
# Only a `dead` lane is taken over; a live or stale hand-armed lane is left alone, and
# the stream's own lock still prevents double delivery.

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=dispatcher-session.sh disable=SC1091
source "$here/dispatcher-session.sh"

command -v crew >/dev/null 2>&1 || exit 0
command -v jq >/dev/null 2>&1 || exit 0

min_delay=${CREW_BUS_MONITOR_MIN_DELAY:-5}
max_delay=${CREW_BUS_MONITOR_MAX_DELAY:-60}

# ancestors — this process's ancestor pids, one per line.
ancestors() {
  local p=$$
  while [[ $p =~ ^[0-9]+$ && $p -gt 1 ]]; do
    printf '%s\n' "$p"
    p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d ' ')
  done
}

# promoted_crew — wait for `crew register` to record this engine, then print its crew.
promoted_crew() {
  local deadline=$((SECONDS + ${CREW_BUS_MONITOR_PROMOTE_WAIT:-300})) anc cid pid
  while :; do
    anc=$(ancestors)
    while IFS=$'\t' read -r cid _ _ _ pid _; do
      [[ $cid == crew_id || -z $pid ]] && continue
      if grep -qxF -- "$pid" <<<"$anc"; then
        printf '%s\n' "$cid"
        return 0
      fi
    done < <(crew crews 2>/dev/null)
    ((SECONDS < deadline)) || return 1
    sleep "${CREW_BUS_MONITOR_PROMOTE_STEP:-5}"
  done
}

if [[ ${1:-} == --promoted ]]; then
  [[ -z ${CREW_WORKER_ID:-} && -z ${CREW_ROLE_ID:-} ]] || exit 0
  ! dispatcher_crew_id >/dev/null || exit 0
  id=$(promoted_crew) || exit 0
  [[ $id =~ ^[A-Za-z0-9._-]+$ && $id != . && $id != .. ]] || exit 0
else
  id=$(dispatcher_crew_id) || exit 0
fi
crew_dir=$(dispatcher_crew_dir "$id") || exit 0

child=
nap=
trap 'kill "$child" "$nap" 2>/dev/null; exit 0' TERM INT

delay=$min_delay
backoff() { ((delay = delay * 2 > max_delay ? max_delay : delay * 2)); }

while [[ -d $crew_dir ]]; do
  state=$(crew stream --status --crew "$id" 2>/dev/null | jq -r '.state // empty' 2>/dev/null)
  # --status and the stream both MkdirAll the crew dir, so re-check before arming.
  if [[ $state == dead && -d $crew_dir ]]; then
    started=$SECONDS
    crew stream --crew "$id" &
    child=$!
    wait "$child"
    child=
    if ((SECONDS - started < 30)); then backoff; else delay=$min_delay; fi
  else
    backoff
  fi
  [[ -d $crew_dir ]] || break
  sleep "$delay" &
  nap=$!
  wait "$nap"
  nap=
done
exit 0
