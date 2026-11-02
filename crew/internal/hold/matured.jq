# `crew hold due`'s maturity filter, verbatim from the arm's
# `jq -c --argjson now "$(_clock_now_f)"` (adapters/core/crew.sh, before #882),
# plus the fleet exclusion. The input is the outstanding array, not the bus: the
# arm piped one into the other. Matured is `<=`, not `<`: a hold whose window
# resets this second is dispatchable now. A fleet hold has no resets_at, and null
# sorts before every number, so the filter excludes it; the fleet queue decides
# when it is due (hold.go's `due`).
[.[] | select(.wait.kind != "fleet" and .wait.resets_at <= $now)]
