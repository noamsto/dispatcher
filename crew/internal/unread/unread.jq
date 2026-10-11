# crew.sh's `_unread_scan` program, verbatim but for its input: the helper
# piped `tail -n 2000 "$log"` into `jq -Rnr` and collected the decoded lines
# with [inputs | fromjson? | select …], while the Go caller hands over the
# already-decoded tail (probe.BusRows drops exactly the lines fromjson?
# dropped). So the collect is [.[] | select …] and every other token is the
# helper's.

def lead($x): if $f == $me then ($x == $me or ($x | startswith($me + "#")))
              else $x == $f end;
[.[] | select(.crew_id == $c and .kind == "msg" and (.ts >= $t0 or $f != $me))] as $m
| [$m[] | select(lead(.from))] as $sent
| [$m[] | select(((.from | strings | startswith($b)) or .from == $d) and lead(.to) and .ts > ($marks[.from] // 0))
   | . as $r
   | select($r.from == $d or (any($sent[]; .to == $r.from and .ts > $r.ts) | not))] as $u
| if $mode == "dispatcher" then
    [$u[] | select(.from == $d)] | if length == 0 then empty else "\(min_by(.ts).ts) \(max_by(.ts).ts)" end
  else
    ($u | min_by(.ts) // empty) | "\(.ts) \(if .from == $d then "dispatcher" else "role" end)"
  end
