# The `report` fold from adapters/core/crew.sh (before the #874 Go port), run
# through gojq by internal/jqrun. The slurped event array is `.` and the crew
# arrives as the $crew variable — the arm passed it with `--arg crew`. One
# patch, no others: `jq -r` printed one @tsv row per line; jqrun wants exactly
# one value, so the rows are collected and joined. The Go caller adds the one
# trailing newline, byte-identically, and prints nothing for the empty string,
# which only happens when there are no rows at all (the outcome and duration
# columns are never both empty).
    [ map(select(.crew_id == $crew)) as $all
    | ($all | map(select(.kind == "dispatch")))[]
    | .branch as $b
    | ($all | map(select(.kind == "status" and ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b))) as $st
    | ($st | map(select(.body.state == "working")) | sort_by(.ts) | (.[0].ts // null)) as $start
    | ($st | sort_by(.ts) | (.[-1] // null)) as $last
    | [ .engine, .model, .tier, (.shape // "—"),
        ($last.body.state // "—"),
        (if ($start != null and $last != null) then (($last.ts - $start) / 1000 | floor | tostring) else "—" end)
      ] | @tsv
    ] | join("\n")
