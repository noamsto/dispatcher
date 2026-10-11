# `_reap_issue_closed`'s claimed-issue fold from crew.sh's `reap)` arm
# (`jq -s -r --arg b`), verbatim but for one patch: the trailing `| .[]` is
# dropped, because jqrun yields exactly one value. Go joins the strings with
# newlines and strips trailing ones, which is what `$(jq -r …)` produced, then
# splits them again as the arm's `mapfile` did.
map(select(.kind == "claim-issue" and .branch == $b and .issue != null) | .issue | tostring)
| unique
