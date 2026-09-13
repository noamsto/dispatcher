#!/usr/bin/env bash
set -euo pipefail

if [ $# -lt 1 ]; then
  echo "usage: stall-check.sh <threshold_s> [events.jsonl]" >&2
  exit 1
fi

threshold_s="$1"
log="${2:-$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl}"

[ -f "$log" ] || exit 0

now_ms=$(($(date +%s) * 1000))

# Read-only jq query over the bus: group worker ids (session suffix stripped,
# so a resumed worker is one identity) by their latest status row, then join
# against `heartbeat:*` msg rows to tell "stale" from "extension not loaded".
jq -s -r \
  --argjson threshold "$threshold_s" \
  --argjson now "$now_ms" '
  def norm: sub("#[^#]*$"; "");
  (map(select(.kind == "status" and ((.from // "") | startswith("worker:"))))
    | group_by(.from | norm)
    | map({worker: (.[0].from | norm),
           latest_state: (max_by(.ts).body | if type == "object" then .state else null end)})
  ) as $workers
  | (map(select(.kind == "msg" and ((.to // "") | startswith("heartbeat:"))))
    | group_by(.from | norm)
    | map({key: (.[0].from | norm), value: (max_by(.ts).ts)})
    | from_entries
  ) as $heartbeats
  | $workers[]
  | select(.latest_state != "done" and .latest_state != "failed" and .latest_state != "exited")
  | ($heartbeats[.worker] // null) as $last_ts
  | if $last_ts == null then
      "no-heartbeat\t\(.worker)"
    else
      (($now - $last_ts) / 1000) as $age_s
      | if $age_s > $threshold then "stale\t\(.worker)\t\($age_s | floor)" else empty end
    end
' "$log" |
  while IFS=$'\t' read -r kind worker age_s; do
    if [ "$kind" = "stale" ]; then
      printf 'stale %s %s\n' "$worker" "$age_s"
    else
      printf 'no-heartbeat %s\n' "$worker"
    fi
  done
