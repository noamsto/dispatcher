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
# `crew crews --mine` lists the crews whose recorded live pid is an ancestor; with
# several, the one registered by the nearest ancestor wins, and none an ancestor of
# this session means no crew (never another dispatcher's bus).
promoted_crew() {
  local deadline=$((SECONDS + ${CREW_BUS_MONITOR_PROMOTE_WAIT:-300})) mine rows anc cid pid first
  while :; do
    mine=$(crew crews --mine 2>/dev/null)
    first=${mine%%$'\n'*}
    if [[ -n $first && $mine == "$first" ]]; then
      printf '%s\n' "$first"
      return 0
    fi
    if [[ -n $first ]]; then
      rows=$(crew crews 2>/dev/null) || {
        ((SECONDS < deadline)) || return 1
        sleep "${CREW_BUS_MONITOR_PROMOTE_STEP:-5}"
        continue
      }
      while read -r anc; do
        while IFS=$'\t' read -r cid _ _ _ pid _; do
          if [[ $pid == "$anc" ]] && grep -qxF -- "$cid" <<<"$mine"; then
            printf '%s\n' "$cid"
            return 0
          fi
        done < <(tail -n +2 <<<"$rows")
      done < <(ancestors)
      return 1
    fi
    ((SECONDS < deadline)) || return 1
    sleep "${CREW_BUS_MONITOR_PROMOTE_STEP:-5}"
  done
}

if [[ ${1:-} == --promoted ]]; then
  [[ -z ${CREW_WORKER_ID:-} && -z ${CREW_ROLE_ID:-} && -z ${CREW_SUBAGENT:-} ]] || exit 0
  ! dispatcher_crew_id >/dev/null || exit 0
  id=$(promoted_crew) || exit 0
  [[ $id =~ ^[A-Za-z0-9._-]+$ && $id != . && $id != .. ]] || exit 0
else
  id=$(dispatcher_crew_id) || exit 0
fi
crew_dir=$(dispatcher_crew_dir "$id") || exit 0

parent=$PPID
poll=${CREW_BUS_MONITOR_POLL:-5}
child=
nap=
trap 'kill "$child" "$nap" 2>/dev/null; [[ -z $child ]] || wait "$child"; exit 0' TERM INT HUP

delay=$min_delay
backoff() { ((delay = delay * 2 > max_delay ? max_delay : delay * 2)); }

# alive — the monitor's reason to run: Claude still up and the crew dir still there.
# The real stream never exits on dir removal and re-creates the dir, so this is the
# only way an orphaned monitor stops.
alive() { kill -0 "$parent" 2>/dev/null && [[ -d $crew_dir ]]; }

while alive; do
  # --status and the stream both MkdirAll the crew dir, so test it right before.
  state=$(crew stream --status --crew "$id" 2>/dev/null | jq -r '.state // empty' 2>/dev/null)
  if [[ $state == dead ]]; then
    started=$SECONDS
    crew stream --crew "$id" &
    child=$!
    while kill -0 "$child" 2>/dev/null; do
      sleep "$poll" &
      nap=$!
      wait "$nap"
      nap=
      alive || {
        kill "$child" 2>/dev/null
        wait "$child"
        exit 0
      }
    done
    child=
    if ((SECONDS - started < 30)); then backoff; else delay=$min_delay; fi
  else
    backoff
  fi
  alive || break
  sleep "$delay" &
  nap=$!
  wait "$nap"
  nap=
done
exit 0
