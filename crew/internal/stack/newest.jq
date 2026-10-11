# `_stack_newest` (adapters/core/crew.sh's `stack)` arm): the newest row of one
# branch that records a parent/cut pair, as the dispatcher wrote it.
#
# Vars: $crew crew id, $child branch, $df the dispatch field, $sf the stack_cut
# field. The arm ran this under `jq -Rr -n` over the bus file.
#
# Patch: `inputs` is `.[]` — jqrun takes the log's lines as `.` instead of
# reading the file with `-R`.
#
# Patch: `fromjson?` is `_jqfromjson?` (jqrun.WithJQFromJSON), jq's own
# fromjson over the repo's decoder: gojq's accepts a lone high-surrogate escape
# that jq refuses, and a forged row must not read as a record.
#
# Patch: the arm's `empty` output is `""` here. jqrun returns one value and
# counts no output as an error, and the arm's caller read the empty `$(…)` as
# "nothing recorded" — which is what "" means to every caller of this fold.
[.[] | _jqfromjson? | select(type == "object" and .crew_id == $crew and .branch == $child)
  | select((.kind == "dispatch" and (.base_oid | type) == "string" and (.[$df] | type) == "string")
        or (.kind == "stack_cut" and (.[$sf] | type) == "string"))]
| sort_by(.ts) | last
| ((.[$df] // .[$sf]) // "")
