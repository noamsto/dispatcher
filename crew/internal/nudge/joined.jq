# The `crew nudge --wait` joiner check from adapters/core/crew.sh, with the same two
# patches already.jq documents: rows arrive as one array, and no row is a null.
# A waiter that had to join reports the first waiter's outcome only when it covers the
# caller's own directive — posted after the joiner started ($t) and for a msg at or
# newer than the caller's ($m) — so a newer msg is never answered by an older nudge.
[.[] | objects | select(.crew_id == $c and .kind == "nudge_wait" and .state == "resolved"
  and .to == $to and .ts >= $t and (.msg_ts // 0) >= $m)]
| last // null
