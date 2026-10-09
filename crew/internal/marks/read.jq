# `_await_marks`' program from adapters/core/crew.sh, verbatim. The slurped
# file values are `.` — the helper ran `jq -cs` over the marks file — and the
# result is the one object `--argjson` then carries into record.jq. A file with
# no object in it reads as {} because `add` of [] is null and `//` takes null as
# absent; a file jq cannot parse never reaches here (the Go caller reads {} for
# it, as the helper's `&&` chain does).

map(objects) | add // {}
