# The `hold` read folds from adapters/core/crew.sh (before the #882 Go port),
# run through gojq by internal/jqrun. It is the one implementation of
# `_hold_outstanding`'s fold since #935 ported `roster-render`, the helper's last
# bash caller: `crew hold list` runs it through `folds.outstanding` and
# `roster-render` through the exported `hold.Outstanding`. The slurped event
# array is `.`, and the arm's two `--arg`s arrive as $crew and $to. One patch:
# the select also tests `.from`, because only `dispatcher:<crew>` writes hold
# rows (`add`, `release`) and `crew msg` lets a worker post any `hold:<crew>` row,
# forging a hold or a release that hides a real one. Otherwise the program is
# verbatim, including the `try fromjson catch null` that lets one unparseable
# body cost only its own row.
    map(select(.crew_id==$crew and .kind=="msg" and .to==$to
               and .from == ("dispatcher:" + $crew))
        | .body | (try fromjson catch null)) | map(select(. != null)) as $bodies
    | ($bodies | map(select(.released == true) | .id)) as $released
    | $bodies | map(select(.released != true
                           and (([.id] - $released) | length) > 0))
