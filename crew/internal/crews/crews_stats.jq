# The `crews` stats fold from adapters/core/crew.sh (before the #838 Go
# port), run through gojq by internal/jqrun. It is the body of the arm's
# `jq -c '...' -s "$log"`: the slurped event array is `.`, and the result is
# the per-crew {last, first, workers} map the final pass consumes as $stats.
# Verbatim; it uses no `now`.
        def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
        map(select(.crew_id != null and .crew_id != ""))
        | group_by(.crew_id)
        | map({key: .[0].crew_id,
               value: {last: (map(.ts) | max), first: (map(.ts) | min),
                       workers: (map(select(.kind=="status" and ((.from // "") | startswith("worker:"))))
                                 | map(.from | wid_branch) | unique | length)}})
        | from_entries
