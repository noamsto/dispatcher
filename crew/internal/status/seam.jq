# The review-seam fold of `crew status <from> pr_open|done` for a standard or
# deep implement session (adapters/core/crew.sh's status arm, `jq -Rnr`).
# Vars: $c crew, $b branch session id, $r reviewer role id, $e engine.
# Output: 1 when the branch has a live review seam, else 0.
#
# Patch: the arm ran `jq -R -n` and read the log with `inputs`; this program
# runs through jqrun with `.` = the array of the log's lines, so
# `reduce inputs as $line` is `reduce .[] as $line`.
#
# Patch: every `fromjson` is `_jqfromjson` (jqrun.WithJQFromJSON), jq's own
# fromjson: gojq's accepts a lone high surrogate escape that jq refuses, and
# a refused line or body must not count as a seam. Nothing else differs.
reduce .[] as $line (
  {ok: false, rejected: false, pending: false};
  (try [$line | _jqfromjson] catch null) as $p
  | if $p == null then
      if $e == "pi" and ($line | test("\\S")) and ($line | contains("\"crew_id\":" + ($c | tojson)) and contains($r | tojson | .[1:-1]))
      then .ok = false | .rejected = true
      else . end
    elif ($p[0] | type) != "object" then .
    else
      $p[0] as $m
      | ($m.crew_id == $c and $m.kind == "msg") as $mine
      | (if $mine then (($m.body | _jqfromjson?) // null) else null end) as $o
      | (($m.from // "") | tostring | sub("#s[^#]*$"; "")) as $f
      | (($m.to // "") | tostring | sub("#s[^#]*$"; "")) as $t
      | if $e == "pi" and $mine and ($o | type) != "object" then
          if $f == $r then .ok = false | .rejected = true
          elif $f == $b and $m.to == $r then .pending = true | .ok = false
          else . end
        elif ($o | type) != "object" then .
        elif $e == "pi" and $f == $r and ($o | has("seam") or has("verdict"))
             and (($o | has("tag") and (has("verdict") | not)) | not) then
          if $o.seam == "review" and $o.verdict == "accept" then
            if $t == $b then .pending = false | .ok = true | .rejected = false else . end
          elif $o.seam == "review" and $o.verdict == "revise" then
            if $t == $b then .pending = false | .ok = false | .rejected = false else .ok = false end
          else .ok = false | .rejected = true end
        elif $e == "pi" and $f == $r
             and (($o | has("seam") or has("verdict")) | not)
             and (($o | has("event") or has("tag")) | not) then
          .ok = false | .rejected = true
        elif $e == "pi" and $f == $b and $m.to == $r
             and (($o | keys) == ["final"] and $o.final == true | not) then
          .pending = true | .ok = false
        elif $o.seam != "review" or ($o | has("tag")) then .
        elif $f == $b and $m.to == ("review:" + $c)
             and (($o | has("review_mode") | not) or ($o.review_mode | IN("full", "downgraded"))) then
          if .rejected or .pending then . else .ok = true end
        else . end
    end
) | if .ok then 1 else 0 end
