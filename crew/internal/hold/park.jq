# `crew hold park`'s program, verbatim from the arm's
# `jq -r --argjson default ... --argjson now "$(_clock_now_f)"`
# (adapters/core/crew.sh, before #882). The input is the outstanding array.
# The branch default when nothing is outstanding or the earliest is already
# matured; otherwise min(default, earliest - now). Never below 1 — `crew watch`
# rejects `--timeout 0` and a 0 here would fail the cursor re-arm.
([.[] | .wait.resets_at] | min) as $earliest
| (if ($earliest == null or $earliest <= $now) then $default
   else ([$default, ($earliest - $now)] | min) end) as $raw
| ([$raw, 1] | max) | floor
