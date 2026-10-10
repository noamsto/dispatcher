# The `pending` fold of `_rr_model` from adapters/core/crew.sh, verbatim: one
# entry per branch the bus dispatched, with the launch stamp, the base the
# newest dispatch that named one recorded, and whether the branch has said
# anything since it was launched (`crew roster` folds status rows only, so a
# launched session reads as nothing until it posts).
#
# No patch: the arm ran it as `jq -c -s --arg crew`, and jqrun hands the decoded
# rows as `.` with $crew.
map(select(.crew_id == $crew)) as $ev
      | [$ev[] | select(.kind == "status" and (.from | type) == "string")] as $st
      | [$ev[] | select(.kind == "dispatch" and (.branch | type) == "string")]
      | group_by(.branch)
      | map(max_by(.ts) as $d | $d.branch as $b
          | ([$ev[] | select((.kind == "dispatch" or .kind == "resume") and .branch == $b) | .ts] | max) as $launch
          | ([$st[] | select(.from == "worker:" + $b or (.from | startswith("worker:" + $b + "#"))) | .ts] | max) as $last
          # A re-dispatch without --base keeps the base an earlier dispatch named.
          | (map(select((.base | type) == "string")) | max_by(.ts) | .base) as $base
          | {branch: $b, ts: $launch, base: $base, title: ($d.title // null),
             tier: ($d.tier // null), engine: ($d.engine // null), model: ($d.model // null),
             pending: ($last == null or $launch > $last)})
