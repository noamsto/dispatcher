# The stall-watch arm's already-nudged check from adapters/core/crew.sh (D6),
# verbatim but for one patch: `inputs | fromjson? | objects` → `.[] | objects`.
# jqrun hands one value, the array of the log's last 2000 lines that the tail
# reader decoded one by one, skipping the undecodable — the arm's `-R` +
# `fromjson?`. `any` already emits one value, so nothing else changes.
# true when a nudge for this session already covers the directive at $m.
any(.[] | objects; .crew_id == $c and .kind == "nudge" and .to == $to and (.msg_ts // 0) >= $m)
