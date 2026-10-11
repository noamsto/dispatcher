# The `crew nudge --wait` already-typed check from adapters/core/crew.sh, verbatim
# but for two patches: `inputs | fromjson? | objects` → `.[] | objects`, because jqrun
# hands one value (the log's last 2000 lines, each decoded by the tail reader, the
# arm's `-R` + `fromjson?`), and `last // empty` → `last // null`, because jqrun has no
# word for "no output" and a null is what the caller reads as "nothing typed yet".
# The row is the newest `nudge` for this session that covers the directive at $m: its
# result and detail are the outcome the wait reports instead of typing again.
[.[] | objects | select(.crew_id == $c and .kind == "nudge" and .to == $to and (.msg_ts // 0) >= $m)]
| last // null
