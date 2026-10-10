# `crew watch`'s wake fold, ported from the arm's `jq -c -s` in
# adapters/core/crew.sh (before the #901 Go port), run through gojq by
# internal/jqrun. Two patches, and nothing else:
#
#   test(" cleared$")  →  test(" cleared\\n?\\z")
#     Go regexp's `$` is end-of-text; Oniguruma's also matches before a single
#     trailing newline, so the embedded copy spells the position out (the
#     sessions `capture` anchor is the same patch).
#   sort_by(.ts)       →  sort_by(.ts, .i)
#     The arm's `sort_by(.ts) | last` broke millisecond ties on sort
#     stability: gojq v0.12.19 sorts stably, but that is not contractual, so
#     the fold stamps each row with its log index and breaks the tie by it —
#     the row the arm returned on its own log, made deterministic.
#
# $crew and $me arrive as the arm's `--arg`s, $since and $states as its
# `--argjson`. The arm's `. as $all | map(...)` rescanned the slurped array
# per event for `prev_state`/`prev_status` — O(events²), ~0.3s per poll at
# 20k rows. #910 replaced that rescan with one pass: walk the rows in
# (ts, log index) order carrying each session's newest status row (`best`)
# and the row it displaced (`below`), so a candidate reads its predecessor in
# O(1) — the newest row strictly older than it.
#
# `norm` is the arm's repeated-`blocked` normalizer, extended by #910: the
# watchdog re-posts its `unread:` blocked while a msg stays unread, so the
# `undelivered for <N>s` age flattens like the cycle and awaited noise, and
# only a changed reason wakes again.
#
# `select(length>0)` is the arm's "nothing qualified" and it emits no value at
# all, which is what jqrun reports to the caller as "poll again" — exactly the
# arm's `2>/dev/null || true`, which read a type error the same way.
def detail_text: if type == "string" then . else "" end;
def norm:
  gsub(" *\\(cycle [0-9]+ of [0-9]+\\)"; "")
  | gsub("awaited [0-9]+s"; "awaited Ns")
  | gsub("undelivered for [0-9]+s"; "undelivered for Ns");
# bump records $row for session $c/$f in map $m; the caller admits only
# rows with crew_id == $crew and a string from, because look is never
# reached with anything else and jq object keys are strings. Rows arrive in
# (ts, index) order, so a newer row displaces `best` into `below` (the newest
# row strictly older than it) and a row that ties `best` (same millisecond,
# later in the log) just moves `best` later — the arm's `sort_by(.ts) | last`
# returned the same row.
def bump($m; $c; $f; $row):
  $m | .[$c][$f] as $cur
  | if $cur == null then setpath([$c, $f]; {best: $row, below: null})
    elif $cur.best.ts == $row.ts then setpath([$c, $f]; $cur | .best = $row)
    else setpath([$c, $f]; {best: $row, below: $cur.best})
    end;
# look is the arm's prev lookup: the newest row strictly older than $ts.
# When `best` ties $ts, `below` is by construction strictly older, so one
# entry is the whole history the arm's `sort_by(.ts) | last` could reach. A
# non-string crew_id or from never enters the map (bump skips it) and reads
# back null here, so a malformed session key cannot throw the fold the way
# it did before #910.
def look($m; $c; $f; $ts):
  if ($c | type) == "string" and ($f | type) == "string" then
    $m | .[$c][$f] as $cur
    | if $cur == null then null
      elif $cur.best.ts < $ts then $cur.best
      else $cur.below
      end
  else null end;
to_entries
| sort_by(.value.ts, .key)
| [ foreach .[] as $x
    ( {all: {}, live: {}, wake: false};
      $x.value as $e
      | ( if $e.kind == "msg" and $e.crew_id == $crew and $e.ts > $since and ($e.to == $me or $e.to == "*")
          then .wake = true
          elif $e.kind == "status" and $e.crew_id == $crew and $e.ts > $since then
            (look(.all; $e.crew_id; $e.from; $e.ts)) as $prev
            | (if $e.body.state == "exited"
               then (look(.live; $e.crew_id; $e.from; $e.ts) | .body.state)
               else null end) as $p
            | .wake = ( ($states | index($e.body.state))
                and (if $e.body.state == "exited"
                     then ($p == "working" or $p == "blocked")
                     else true end)
                and ( ( ( $e.body.state == "blocked"
                          and $prev != null
                          and $prev.body.state == "blocked"
                          and ((($e.body.detail | detail_text) | norm)
                               == (($prev.body.detail | detail_text) | norm)) )
                        or ( $e.body.source == "watchdog"
                             and $e.body.state == "working"
                             and (($e.body.detail | detail_text) | test(" cleared\\n?\\z")) ) )
                  | not ) )
          else .wake = false end
        | if $e.kind == "status" and $e.crew_id == $crew and ($e.from | type) == "string" then
            .all = bump(.all; $e.crew_id; $e.from; $e)
            | .live = (if $e.body.state == "exited" then .live
                       else bump(.live; $e.crew_id; $e.from; $e) end)
          else . end );
      if .wake then $x.value else empty end ) ]
| select(length > 0)
| {cursor: (.[length - 1].ts), events: .}
