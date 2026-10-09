# The `crews` final merge pass from adapters/core/crew.sh (before the #838
# Go port), run through gojq by internal/jqrun. The meta array is `.` and
# the stats map arrives as the $stats variable — the #838 fix: the arm passed
# it with `--argjson stats`, whose argv grew past MAX_ARG_STRLEN (~1700
# crews). One patch, no others: jq -r printed one @tsv row per line; jqrun
# wants exactly one value, so the rows are collected and joined. The Go
# caller adds the one trailing newline, byte-identically.
    (now*1000) as $now
    | (map(select($stats[.id] != null)) | sort_by(-$stats[.id].last)) as $with
    | (map(select($stats[.id] == null))) as $without
    | [ ($with + $without)[]
        | ($stats[.id]) as $s
        | [ .id,
            (if $s == null then "—" else (($now - $s.last)/1000 | floor | tostring) end),
            (if $s == null then "—" else (($now - $s.first)/1000 | floor | tostring) end),
            ($s.workers // 0 | tostring),
            (.pid // "—"),
            (if .alive == null then "—" elif .alive then "yes" else "no" end)
          ] | @tsv ]
    | join("\n")
