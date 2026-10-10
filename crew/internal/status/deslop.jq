# The deslop-seam fold of `crew status <from> pr_open|done` for a standard or
# deep implement session (adapters/core/crew.sh's status arm, `jq -Rnr`).
# Vars: $c crew, $b branch session id. Output: 1 when the lead posted a
# {"seam":"deslop"} msg to review:<crew> for this branch, else 0.
#
# Patch: the arm ran `jq -R -n` and read the log with `inputs`; this program
# runs through jqrun with `.` = the array of the log's lines, so
# `first(inputs | ...)` is `first(.[] | ...)`.
#
# Patch: every `fromjson` is `_jqfromjson` (jqrun.WithJQFromJSON), jq's own
# fromjson: gojq's accepts a lone high surrogate escape that jq refuses, and
# a refused line or body must not count as a seam. Nothing else differs.
first(.[]
  | (try _jqfromjson catch null)
  | select(type == "object" and .crew_id == $c and .kind == "msg"
           and .to == ("review:" + $c)
           and ((.from // "") | tostring | sub("#s[^#]*$"; "")) == $b)
  | (.body | _jqfromjson? // null)
  | select(type == "object" and .seam == "deslop" and (has("tag") | not))
  | 1) // 0
