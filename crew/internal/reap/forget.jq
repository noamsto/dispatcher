# `_reap_forget`'s session-pairs fold from crew.sh's `reap)` arm
# (`jq -rs --arg b`), verbatim but for one patch:
#
#   | .[] | @tsv  →  | map(_tsvcells)
#     jqrun yields exactly one value, so the pairs come back as one array of
#     [crew, id] string pairs that Go reads by position. _tsvcells keeps @tsv's
#     conversions (null "", numbers and booleans as text) and its error on an
#     array or object cell, but not its escaping: a tab or newline inside an id
#     reaches Go as itself, where the arm derived paths from the escaped text.
#
# `.` is the slurped bus (`jq -s`); $b is the reaped branch.
def wid_branch: ltrimstr("worker:") | sub("#[^#]*$"; "");
def _tsvcells: map(
    if type == "string" then .
    elif type == "null" then ""
    elif type == "number" or type == "boolean" then tostring
    else error("\(type) (\(tojson)) is not valid in a csv row") end);
[ (.[] | select((.kind == "status" or .kind == "msg") and ((.from // "") | type == "string")
      and ((.from // "") | startswith("worker:")) and ((.from // "") | wid_branch) == $b)
    | [.crew_id, .from]),
  (.[] | select((.kind == "dispatch" or .kind == "resume") and .branch == $b
      and ((.session // "") | type == "string") and (.session // "") != "")
    | [.crew_id, "worker:" + $b + "#" + .session]),
  (.[] | select(((.from // "") | type == "string") and ((.from // "") | startswith("role:" + $b + ":"))
      and ((.from // "")[("role:" + $b + ":" | length):] | contains(":") | not))
    | [.crew_id, .from]) ]
| map(select(.[0] != null and .[0] != "" and .[1] != ""))
| unique | map(_tsvcells)
