# `_await_record`'s program from adapters/core/crew.sh, verbatim. The slurped
# msgs the caller printed are `.` and the marks already held arrive as the $old
# variable — the helper passed them with `--argjson old`. Each sender's mark
# moves up to the newest of its msgs and never down, and `max` is jq's total
# order, so a mark a hand-edited file left as a string beats a number ts. A
# msg whose `from` is absent, null or numeric makes jq fail to index, and the
# whole write is lost rather than that one msg: the Go caller keeps the old
# file, as the helper's `if jq ...` did.

reduce .[] as $m ($old; .[$m.from] = ([(.[$m.from] // 0), $m.ts] | max))
