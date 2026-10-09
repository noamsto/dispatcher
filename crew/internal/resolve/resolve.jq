# `crew resolve-target`'s fold — the `_resolve_target` jq from
# adapters/core/crew.sh (before the #902 Go port), run through gojq by
# internal/jqrun. Three patches for that engine, one per bullet:
#
#   [inputs | fromjson? | objects …]  →  [.[] | objects …]
#     jqrun hands one value: the array of rows the caller decoded one per line
#     (Rows). `fromjson?` cannot survive that — on a decoded non-string it
#     errors and `?` swallows it, so the fold would see no rows forever.
#     (await.jq's patch.)
#   the trailing `@tsv` stream  →  [ … ]
#     jqrun takes exactly one output value; the Go caller prints the array one
#     element per line, which is what `jq -r` printed.
#   test("^[0-9]+$") / test("^[A-Za-z]+-[0-9]+$")  →  "\\n?\\z" anchors
#     Oniguruma's `$` also matches before one trailing newline, Go regexp's is
#     absolute end-of-text. `\\n?\\z` (the regex the engine sees; doubled in this
#     jq string literal, sessions.jq's precedent) reproduces it, so a target of
#     "9\n" is still the issue kind. The interpolated
#     `test("(^|/)" + $s + "-")` needs no patch: `$s` is digits or
#     `[A-Za-z]+-[0-9]+` (plus at most that newline) whenever it runs, so no
#     metacharacter reaches the pattern.
#
# $t and $c are the arm's `--arg t`/`--arg c`: the target, and the crew filter
# that is empty for "every crew on the bus". `group_by(.branch)` sorts the
# branches, so the single row and the ambiguity list both come out in branch
# order, and `max_by(.ts)` keeps the newest dispatch row per branch.

($t | ltrimstr("#")) as $s
| ($s | ascii_downcase) as $l
| (if $t | startswith("worker:") then $t | ltrimstr("worker:") | sub("#[^#]*$"; "") else $t end) as $b
| (if $s | test("^[0-9]+\\n?\\z") then "issue"
   elif $s | test("^[A-Za-z]+-[0-9]+\\n?\\z") then "linear" else "name" end) as $k
| [ .[] | objects
    | select(.kind == "dispatch" and ((.branch // null) | type) == "string" and ($c == "" or .crew_id == $c))]
| group_by(.branch) | map(max_by(.ts))
| [ .[]
    | select(.branch == $b or (.name // "") == $t
        or ($k == "issue" and ((.branch | test("(^|/)" + $s + "-")) or any(((.also_closes // []) | if type == "array" then .[] else empty end); tostring == $s)))
        or ($k == "linear" and ((.branch | ascii_downcase | test("(^|/)" + $l + "-")) or any(((.also_closes // []) | if type == "array" then .[] else empty end); tostring | ascii_downcase == $l))))
    | [.branch, (.name // ""), (.host // ""), (.crew_id // "")] | @tsv ]
