# Frozen copy of watch.jq as of #912, kept only as the equivalence
# oracle for TestFoldEquivalentToLegacyProgram (#910). The single-pass
# fold must decide identically to this program on every bus that does not
# re-post an `unread:` blocked with a changed age — the one row where norm
# behaviour changed. Do not edit; do not embed.
# `crew watch`'s wake fold, verbatim from the arm's `jq -c -s` in
# adapters/core/crew.sh (before the #901 Go port), run through gojq by
# internal/jqrun. One patch, and nothing else:
#
#   test(" cleared$")  →  test(" cleared\\n?\\z")
#     Go regexp's `$` is end-of-text; Oniguruma's also matches before a single
#     trailing newline, so the embedded copy spells the position out (the
#     sessions `capture` anchor is the same patch).
#
# $crew and $me arrive as the arm's `--arg`s, $since and $states as its
# `--argjson`. `. as $all | map(...)` is the arm's own shape: jqrun hands one
# value, the slurped array, so `$all` and `.` are the same array and the fold
# keeps rescanning it per event for `prev_state`/`prev_status` — the O(events²)
# the arm paid, kept verbatim rather than hand-optimised.
#
# `select(length>0)` is the arm's "nothing qualified" and it emits no value at
# all, which is what jqrun reports to the caller as "poll again" — exactly the
# arm's `2>/dev/null || true`, which read a type error the same way.
def prev_state($all; $e):
  ([ $all[]
     | select(.crew_id==$e.crew_id and .kind=="status" and .from==$e.from and .ts < $e.ts)
     | select(.body.state != "exited")
     | {ts: .ts, state: .body.state} ]
   | sort_by(.ts) | last | .state);
def detail_text: if type == "string" then . else "" end;
def norm: gsub(" *\\(cycle [0-9]+ of [0-9]+\\)"; "") | gsub("awaited [0-9]+s"; "awaited Ns");
def prev_status($all; $e):
  ([ $all[]
     | select(.crew_id==$e.crew_id and .kind=="status" and .from==$e.from and .ts < $e.ts) ]
   | sort_by(.ts) | last);
. as $all
| map(. as $e
      | select($e.crew_id==$crew and $e.ts>$since)
      | select(
          ( $e.kind=="status"
            and ($e.body.state as $s | $states | index($s))
            and ( if $e.body.state == "exited"
                  then (prev_state($all; $e) as $p | ($p == "working" or $p == "blocked"))
                  else true end )
            and (prev_status($all; $e) as $prev
                 | ( ( $e.body.state == "blocked"
                       and $prev != null
                       and $prev.body.state == "blocked"
                       and ((($e.body.detail | detail_text) | norm)
                            == (($prev.body.detail | detail_text) | norm)) )
                     or ( $e.body.source == "watchdog"
                          and $e.body.state == "working"
                          and (($e.body.detail | detail_text) | test(" cleared\\n?\\z")) ) )
                 | not) )
          or ( $e.kind=="msg" and ($e.to==$me or $e.to=="*") ) ))
| sort_by(.ts)
| select(length>0)
| {cursor:(.[-1].ts), events:.}
