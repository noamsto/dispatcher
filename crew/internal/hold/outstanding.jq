# The `hold` read folds from adapters/core/crew.sh (before the #882 Go port),
# run through gojq by internal/jqrun. `_hold_outstanding` itself STAYS in bash —
# `roster-render`'s `_rr_model` calls it — so this file is the copy of it that
# `crew hold list` runs, and the drift guard
# `hold: _hold_outstanding and the Go list agree on one bus` in crew.bats keeps
# the two equal. The slurped event array is `.`, and the arm's two `--arg`s
# arrive as $crew and $to. No patch: the program is verbatim, including the
# `try fromjson catch null` that lets one unparseable body cost only its own row.
    map(select(.crew_id==$crew and .kind=="msg" and .to==$to)
        | .body | (try fromjson catch null)) | map(select(. != null)) as $bodies
    | ($bodies | map(select(.released == true) | .id)) as $released
    | $bodies | map(select(.released != true
                           and (([.id] - $released) | length) > 0))
