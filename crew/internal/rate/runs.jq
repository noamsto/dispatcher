# The sweep-all loop's per-repo count, the arm's `jq -s --arg r <slug> …`
# verbatim: the distinct run ids the store holds for one repo, over its raw rows.
[.[] | select(.repo == $r) | .run_id] | unique | length
