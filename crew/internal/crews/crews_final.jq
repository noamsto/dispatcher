# The `crews` final merge pass from adapters/core/crew.sh (before the #838
# Go port), run through gojq by internal/jqrun. The meta array is `.` and
# the stats map arrives as the $stats variable — the #838 fix: the arm passed
# it with `--argjson stats`, whose argv grew past MAX_ARG_STRLEN (~1700
# crews). Two patches, no others:
#
# 1. `as $now` became `as $nowms`. jqrun rewrites the bare `now` builtin to
#    the injected `$now`, on a byte scan that would also rewrite the literal
#    `$now` text below into `$$now`; `$nowms` survives untouched, and after
#    injection the first line reads `($now*1000) as $nowms` — the same value.
# 2. jq -r printed one @tsv row per line; jqrun wants exactly one value, so
#    the rows are collected and joined. The Go caller adds the one trailing
#    newline, byte-identically.
    (now*1000) as $nowms
    | (map(select($stats[.id] != null)) | sort_by(-$stats[.id].last)) as $with
    | (map(select($stats[.id] == null))) as $without
    | [ ($with + $without)[]
        | ($stats[.id]) as $s
        | [ .id,
            (if $s == null then "—" else (($nowms - $s.last)/1000 | floor | tostring) end),
            (if $s == null then "—" else (($nowms - $s.first)/1000 | floor | tostring) end),
            ($s.workers // 0 | tostring),
            (.pid // "—"),
            (if .alive == null then "—" elif .alive then "yes" else "no" end)
          ] | @tsv ]
    | join("\n")
