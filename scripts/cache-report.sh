#!/usr/bin/env bash
# cache-report — read-only pi prompt-cache hit report across dispatcher sessions.
#
# Prompt caching dominates a pi worker's cost: in the first survey ~93% of
# prompt tokens were cache reads. The only place the real hit rate is visible is
# pi's session log (`message.usage.cacheRead`); pi's own `cost` field is
# computed from the model's canonical price metadata, NOT the endpoint
# OpenRouter actually routed to, so it reads low whenever the router picks a
# pricier reseller.
#
# Read-only: this never writes a session file and never routes a request.
#
#   cache-report                 per-model summary across pi worker sessions
#   cache-report --sessions      one row per session
#   cache-report --all           also include ~/.pi/agent/sessions
#   cache-report --since 7       only sessions touched in the last 7 days
#   cache-report --dir DIR       override the session root (repeatable)
#   cache-report --json          machine-readable
set -euo pipefail

usage() {
  echo "usage: cache-report [--sessions] [--all] [--since DAYS] [--dir DIR]... [--json]" >&2
}

out_fmt=summary
since=""
all=0
dirs=()

while [ $# -gt 0 ]; do
  case "$1" in
  --sessions) out_fmt=sessions ;;
  --json) out_fmt=json ;;
  --all) all=1 ;;
  --since)
    since="${2:-}"
    [ -n "$since" ] || {
      usage
      exit 1
    }
    shift
    ;;
  --dir)
    dirs+=("${2:-}")
    [ -n "${2:-}" ] || {
      usage
      exit 1
    }
    shift
    ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    echo "cache-report: unknown argument '$1'" >&2
    usage
    exit 1
    ;;
  esac
  shift
done

case "$since" in '' | *[!0-9]*)
  [ -z "$since" ] || {
    echo "cache-report: --since must be a whole number of days" >&2
    exit 1
  }
  ;;
esac

# Default to the shared pi worker dir — that is the fleet. The ambient agent dir
# is opt-in because it also holds every non-dispatcher pi session on the host.
if [ "${#dirs[@]}" -eq 0 ]; then
  dirs+=("$HOME/.pi/dispatcher-worker/sessions")
  [ "$all" = 1 ] && dirs+=("$HOME/.pi/agent/sessions")
fi

find_expr=(-type f -name '*.jsonl')
[ -n "$since" ] && find_expr+=(-newermt "$since days ago")

files=()
for d in "${dirs[@]}"; do
  [ -d "$d" ] || continue
  while IFS= read -r -d '' f; do files+=("$f"); done \
    < <(find "$d" "${find_expr[@]}" -print0 2>/dev/null)
done

if [ "${#files[@]}" -eq 0 ]; then
  echo "cache-report: no pi sessions found under: ${dirs[*]}" >&2
  exit 0
fi

# TSV rows: session \t model \t input \t cacheRead \t cost
tmp="$(mktemp "${TMPDIR:-/tmp}/cache-report.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

for f in "${files[@]}"; do
  model="$(jq -r 'select(.type=="model_change") | .modelId // empty' "$f" 2>/dev/null | tail -1)"
  [ -n "$model" ] || model=unknown
  jq -r --arg m "$model" --arg s "$f" \
    'select(.type=="message" and .message.role=="assistant") | .message.usage // empty
     | [$s, $m, (.input // 0), (.cacheRead // 0), (.cost.total // 0)] | @tsv' \
    "$f" 2>/dev/null >>"$tmp" || true
done

if [ ! -s "$tmp" ]; then
  echo "cache-report: no assistant usage recorded in ${#files[@]} session file(s)" >&2
  exit 0
fi

case "$out_fmt" in
summary)
  printf '%-40s %6s %11s %13s %7s %10s\n' "model" "turns" "input" "cacheRead" "hit%" 'cost($)'
  awk -F'\t' '{n[$2]++; i[$2]+=$3; c[$2]+=$4; t[$2]+=$5}
    END {
      for (m in n) {
        tot = i[m] + c[m]
        hp = (tot > 0) ? 100 * c[m] / tot : 0
        printf "%-40s %6d %11d %13d %6.1f%% %10.2f\n", m, n[m], i[m], c[m], hp, t[m]
      }
    }' "$tmp" | LC_ALL=C sort
  ;;
sessions)
  awk -F'\t' '{n[$1]++; m[$1]=$2; i[$1]+=$3; c[$1]+=$4; t[$1]+=$5}
    END {
      for (s in n) {
        tot = i[s] + c[s]
        hp = (tot > 0) ? 100 * c[s] / tot : 0
        printf "%6.1f%%\tturns=%-4d in=%-9d cache=%-10d cost=%8.2f\t%s\t%s\n", hp, n[s], i[s], c[s], t[s], m[s], s
      }
    }' "$tmp" | LC_ALL=C sort -t$'\t' -k1,1n
  ;;
json)
  awk -F'\t' '{n[$2]++; i[$2]+=$3; c[$2]+=$4; t[$2]+=$5}
    END { for (m in n) printf "%s\t%d\t%d\t%d\t%.10f\n", m, n[m], i[m], c[m], t[m] }' "$tmp" |
    jq -Rn '
      [inputs | split("\t")
       | {model: .[0], turns: (.[1] | tonumber), input: (.[2] | tonumber),
          cacheRead: (.[3] | tonumber), cost: (.[4] | tonumber)}
       | . + {hitPct: (if (.input + .cacheRead) > 0
                       then (100 * .cacheRead / (.input + .cacheRead)) else null end)}]
      | sort_by(.model)'
  ;;
esac
