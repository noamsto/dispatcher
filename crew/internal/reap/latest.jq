# `_reap_latest` from crew.sh's `reap)` arm, verbatim but for two patches:
#
#   the main expression  →  [ … ]
#     jqrun yields exactly one value, so the row stream is collected into an
#     array; Go reads each row by position, which also fixes the arm's
#     IFS-tab `read` collapsing an empty field into the next one.
#   @tsv  →  _tsvcells
#     the cells come back as strings without @tsv's escaping (Go reads them
#     by position, not as TSV text) but with its conversions — null "",
#     numbers and booleans as text — and its error on an array or object cell.
#
# `.` is the slurped bus (`jq -s`), `now` the wall clock. $only is "" for the
# candidate fold (terminal or pr_open rows) or a branch for --discard (that
# branch's row whatever its state, "none" when it has no status). $terminal and
# $claim_ttl are the arm's `--argjson`s.
#
# Cells: branch state own earlier reaprow ts later session crew.
def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
def _tsvcells: map(
    if type == "string" then .
    elif type == "null" then ""
    elif type == "number" or type == "boolean" then tostring
    else error("\(type) (\(tojson)) is not valid in a csv row") end);
[
(map(select((.kind == "msg" or .kind == "status") and ((.from // "") | startswith("worker:")))
    | {branch: (.from | wid_branch), ts})
  | group_by(.branch) | map({key: .[0].branch, value: (map(.ts) | max)}) | from_entries) as $last
# The latest session of a re-dispatched branch may not carry the PR.
| (map(select(.kind == "reap" and ((.pr // "") | type == "string" and . != ""))
      | {branch, ts, pr}) | group_by(.branch)
    | map({key: .[0].branch, value: (max_by(.ts) | .pr)}) | from_entries) as $reapprs
| (map(select(.kind == "status" and ((.from // "") | startswith("worker:")) and (.body.pr_url | type == "string" and . != ""))
      | {branch: (.from | wid_branch), from, ts, pr: .body.pr_url})) as $statprs
| (map(select(
        ((.from // "") | startswith("worker:"))
        and (
          .kind == "status"
          or (.kind == "claim" and ((((now*1000) - .ts) / 1000) as $age | $age >= 0 and $age < $claim_ttl))
        )))
    | group_by(.from) | map(
        (max_by(.ts)) as $latest
        | {branch: ($latest.from | wid_branch),
           session: $latest.from,
           crew: $latest.crew_id,
           ts: $latest.ts,
           state: $latest.body.state,
           pr_url: (map(.body.pr_url) | map(select(type == "string" and . != "")) | last)})
    | map(select(.branch != ""))
    | group_by(.branch) | map(sort_by(.ts) | last)
    | if $only == "" then map(select(.state as $st | (($terminal + ["pr_open"]) | index($st)) != null))
      else map(select(.branch == $only)) end
  )
| .[] | . as $c
| [.branch, (.state | if . == null or . == "" then "none" else . end), (.pr_url // "-"),
    ([$statprs[] | select(.branch == $c.branch and .from != $c.session)] | if length == 0 then "-" else max_by(.ts) | .pr end),
    ($reapprs[.branch] // "-"), (.ts // 0), (if ($last[.branch] // 0) > .ts then "1" else "0" end),
    (.session // ""), (.crew // "")] | _tsvcells
]
