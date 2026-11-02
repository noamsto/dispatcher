# `crew hold park`'s program, from the arm's
# `jq -r --argjson default ... --argjson now "$(_clock_now_f)"`
# (adapters/core/crew.sh, before #882). The input is the outstanding array.
# The branch default when nothing is outstanding or the earliest is already
# matured; otherwise min(default, earliest - now). The earliest reset counts
# quota holds only: a fleet hold has none. While any fleet hold is outstanding
# the answer is capped at 60, so a park lane re-checks the fleet queue at least
# once a minute. Never below 1 — `crew watch` rejects `--timeout 0` and a 0 here
# would fail the cursor re-arm.
([.[] | select(.wait.kind != "fleet") | .wait.resets_at] | min) as $earliest
| (if ($earliest == null or $earliest <= $now) then $default
   else ([$default, ($earliest - $now)] | min) end) as $raw
| (if any(.[]; .wait.kind == "fleet") then ([$raw, 60] | min) else $raw end) as $capped
| ([$capped, 1] | max) | floor
