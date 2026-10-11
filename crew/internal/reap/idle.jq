# The idle-release fold from crew.sh's `reap)` arm (the `jq -s -r` heredoc
# feeding `_release_windows`), verbatim but for two patches:
#
#   the main expression  →  [ … ]
#     jqrun yields exactly one value, so the row stream is collected into an
#     array; Go reads each row by position.
#   @tsv  →  _tsvcells
#     strings without @tsv's escaping, with its conversions (null "", numbers
#     and booleans as text) and its error on an array or object cell.
#
# `.` is the slurped bus, `now` the wall clock, $idle and $terminal the arm's
# `--argjson`s. Cells: branch session state ts.
def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
def _tsvcells: map(
    if type == "string" then .
    elif type == "null" then ""
    elif type == "number" or type == "boolean" then tostring
    else error("\(type) (\(tojson)) is not valid in a csv row") end);
[
# A dispatch posts a "claim" for a branch before its window even exists.
# It has no `body`, so it can never pass the terminal-state filter below —
# it can only mask a stale prior `done` by winning the per-branch
# max_by(.ts), never cause a release on its own.
map(select((.kind=="status" or .kind=="claim") and ((.from // "") | startswith("worker:")) and ((.from | wid_branch) != "")))
| group_by(.from) | map(max_by(.ts))
| group_by(.from | wid_branch) | map(max_by(.ts))
| map(select(.body.state as $st | ($terminal | index($st)) != null))
# A watchdog-posted failed marks a hung pane: keep it as evidence.
| map(select((.body.source // "") != "watchdog" or .body.state != "failed"))
| map(select((((now*1000) - .ts) / 1000) >= $idle))
| .[] | [(.from | wid_branch), (((.from | wid_session) // "") | if . == "" then "-" else . end), .body.state, .ts] | _tsvcells
]
