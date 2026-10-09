# `crew hold due`'s maturity filter, verbatim from the arm's
# `jq -c --argjson now "$(_clock_now_f)"` (adapters/core/crew.sh, before #882).
# The input is the outstanding array, not the bus: the arm piped one into the
# other. Matured is `<=`, not `<`: a hold whose window resets this second is
# dispatchable now.
[.[] | select(.wait.resets_at <= $now)]
