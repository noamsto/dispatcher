# `_stack_align_record` (adapters/core/crew.sh's `stack)` arm): one layer's
# record — the newest row carrying BOTH a parent and a cut, so a `--pr` retarget
# row (`base_oid: null`) never pairs its base with an older row's cut.
#
# Vars: $crew crew id, $b branch. The arm ran this under `jq -Rc -n` over the
# bus file and read `.p`/`.c` back with two more jq calls.
#
# Patch: `inputs` is `.[]`, and `fromjson?` is `_jqfromjson?`
# (jqrun.WithJQFromJSON) — see newest.jq for both.
#
# Patch: the arm's `last // empty` produced no output, which its caller read as
# an empty `$(…)`. jqrun returns one value, so the empty case is the empty
# record: every caller treats an empty parent and an empty cut exactly as the
# arm treated no row.
[.[] | _jqfromjson? | select(type == "object" and .crew_id == $crew and .branch == $b)
  | select((.kind == "dispatch" and (.base | type) == "string" and (.base_oid | type) == "string")
        or (.kind == "stack_cut" and (.parent | type) == "string" and (.cut | type) == "string"))]
| sort_by(.ts) | last
| if . == null then {p: "", c: ""}
  elif .kind == "dispatch" then {p: .base, c: .base_oid}
  else {p: .parent, c: .cut} end
