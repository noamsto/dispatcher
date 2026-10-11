# Ported from crew.sh's `_human_present` transcript filter, which ran under
# `jq -Rr … | head -n 1`: once per transcript line, the first surviving row's
# "x" deciding. One patch: the leading `fromjson? |` is gone, because Go decodes
# each line before running this — a line that does not decode is skipped there,
# as `fromjson?` skipped it. So `.` is one decoded row, and the output is
# non-empty iff that row is a newer typed user turn. Go also keeps jq's per-line
# error isolation: a row whose evaluation errors (a numeric .timestamp, a string
# .message) is skipped and the next line still counts, as jq reported the error
# and moved on to its next input. $ts is the status row's ts in milliseconds.
#
# Only text a person typed counts: not tool results, isMeta injections, or the
# harness's own task-notification/bash-output turns.

select(.type == "user" and .timestamp != null and ((.isMeta // false) | not))
| ((.message.content | if type == "string" then . else ([.[]? | select(.type == "text") | .text] | join("\n")) end)) as $t
| select($t != "" and ($t | test("^\\s*<(task-notification|bash-stdout|bash-stderr|local-command-stdout)>") | not))
| select((.timestamp | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) * 1000 > $ts)
| "x"
