# Ported from crew.sh's `_human_present` transcript filter (jq -Rr, one JSON
# line per input line, `head -n 1` taking the first row), with one patch: `.` is
# the array of transcript lines already decoded in Go — lines that do not decode
# are dropped there, as `fromjson?` dropped them — and the program yields
# whether any row survives instead of printing "x" for the first one. `any`
# stops at the first row, as `head -n 1` did, so a malformed row after a
# matching one is never reached. $ts is the status row's ts in milliseconds.
#
# Only text a person typed counts: not tool results, isMeta injections, or the
# harness's own task-notification/bash-output turns.

any(.[]
  | select(.type == "user" and .timestamp != null and ((.isMeta // false) | not))
  | ((.message.content | if type == "string" then . else ([.[]? | select(.type == "text") | .text] | join("\n")) end)) as $t
  | select($t != "" and ($t | test("^\\s*<(task-notification|bash-stdout|bash-stderr|local-command-stdout)>") | not))
  | select((.timestamp | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) * 1000 > $ts)
  | "x"; true)
