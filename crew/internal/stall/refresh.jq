# `_bus_refresh`'s program from the stall-watch arm of adapters/core/crew.sh,
# verbatim but for four patches:
#   - jqrun hands the slurped log as `.` and returns one value, so the per-row
#     filter is collected with `[.[] | …]`;
#   - each input runs under `try`: jq reports a runtime error, skips that one
#     input and carries on with the next, so a malformed row (a string `body`,
#     a non-string `from`) drops out alone instead of blinding the view;
#   - each row is an array, not `@tsv` text, so Go reads the fields by
#     position. The arm's `IFS=$'\t' read` collapsed an empty `.body.source`
#     and shifted the detail into bus_source; that quirk is not mirrored;
#   - the capture's `$` anchor is `\n?\z`, Oniguruma's end-of-string in Go's
#     regexp dialect.
# Each row is [step-aside flag, ts, state, source, detail]: the flag is "1" for
# a post from a session of this branch at least as new as ours (INV-W0), which
# ends the watch.
[.[] | try (
    select(.crew_id==$c and .kind=="status" and .ts>=$t0
         and (.from==$m or (.from|startswith($m+"#"))))
  | ([.from | capture("^(?<w>.*)#s(?<e>[0-9]+)-[0-9]+\\n?\\z")] | first) as $sid
  | [(if $e0 != "" and .from != $f and $sid != null and $sid.w == $m
         and ($sid.e|tonumber) >= ($e0|tonumber) then "1" else "0" end),
     (.ts|tostring), .body.state, (.body.source // ""), (.body.detail // "")]
  )]
