# The parked block of `crew hold due`'s table: render.jq's shape (one value, rows
# joined) over the fleet holds the queue parked, each carrying its added `reason`.
[ .[] | [ .id, .reason, .task.branch, .task.title ] | @tsv ] | join("\n")
