# `crew stream`'s reap trigger, verbatim from the arm's `jq -e -s` in
# adapters/core/crew.sh (before the #914 Go port), run through gojq by
# internal/jqrun. Unpatched: no regex, and gojq's `fromjson?` and `startswith`
# are the same reads the arm's jq made.
#
# The input is the slurped batch, one event object per line, so `.` is the
# array the arm's `-s` built. True when the batch carries a terminal status, or
# a pr-watch msg reporting the PR itself landed — either is worth a reap now
# rather than at the next cadence tick.
any(.[]; .events[]? as $e |
  if $e.kind == "status" then
    (["done","failed","exited"] | index($e.body.state)) != null
  elif $e.kind == "msg" and (($e.from // "") | startswith("pr-watch:")) then
    ($e.body | fromjson? // {}) as $b |
    ((($b.changed // []) | index("state")) != null)
      and ((["MERGED","CLOSED"] | index($b.state.state)) != null)
  else false end
)
