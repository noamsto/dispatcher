# The overseer's msg fold over one bus, for `crew fleet stream --overseer`:
# every msg to $me above $since, in (ts, log index) order as watch.jq sorts,
# across every crew on the bus. A msg qualifies only from the dispatcher of the
# crew it names: `crew msg` takes crew_id from a free CREW_ID and accepts any
# `to`, but a worker cannot post as `dispatcher:`. Every other msg to $me is
# reported as {ts, crew_id, from}, never its body.
#
# cursor covers both sets, or a misrouted msg would be re-reported every poll.
# Nothing above $since is null: poll again.
#
# Only objects whose ts is a whole number of ms below 2^53 are read, so the
# cursor is always an int64: a row that is no object, or carries any other ts,
# is neither delivered nor misrouted, and cannot stall the bus. The bounds run
# before floor, which throws on a number past float64 such as 1e400.
def qualifies:
  (.crew_id | type) == "string" and .crew_id != "" and .from == "dispatcher:" + .crew_id;
def whole_ms:
  type == "number" and . >= 0 and . < 9007199254740992 and . == floor;
to_entries
| map(select((.value | type) == "object"
    and .value.kind == "msg" and .value.to == $me
    and (.value.ts | whole_ms) and .value.ts > $since))
| sort_by(.value.ts, .key)
| map(.value)
| if length == 0 then null
  else {cursor: (.[length - 1].ts),
        events: map(select(qualifies)),
        misrouted: map(select(qualifies | not) | {ts, crew_id, from})}
  end
