# The overseer's msg fold over one bus, for `crew fleet stream --overseer`:
# every msg to $me above $since, in (ts, log index) order as watch.jq sorts,
# across every crew on the bus. A msg qualifies only from the dispatcher of the
# crew it names: `crew msg` takes crew_id from a free CREW_ID and accepts any
# `to`, but a worker cannot post as `dispatcher:`. Every other msg to $me is
# reported as {ts, crew_id, from}, never its body.
#
# cursor covers both sets, or a misrouted msg would be re-reported every poll.
# Nothing above $since emits no value at all: jqrun's "poll again".
#
# Only objects with a number ts are read: one bad append must not throw the
# fold and stop the overseer reading this bus.
def qualifies:
  (.crew_id | type) == "string" and .crew_id != "" and .from == "dispatcher:" + .crew_id;
to_entries
| map(select((.value | type) == "object"
    and .value.kind == "msg" and .value.to == $me
    and (.value.ts | type) == "number" and .value.ts > $since))
| sort_by(.value.ts, .key)
| map(.value)
| select(length > 0)
| {cursor: (.[length - 1].ts),
   events: map(select(qualifies)),
   misrouted: map(select(qualifies | not) | {ts, crew_id, from})}
