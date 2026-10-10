# The plan-seam fold of `crew status <from> pr_open|done` for a standard or
# deep implement session whose WORKER_TASK.md says `plan: required`. Unlike
# seam.jq and deslop.jq it has no bash original; it keeps their dialect so
# TestFoldsMatchJQ can still run it as `jq -Rnr` over the raw log.
# Vars: $c crew, $b branch session id. Output: 1 when the bus holds a plan
# seam for this branch or a resume of it after its latest dispatch, else 0.
#
# A plan seam is a {"seam":"plan","plan_critic_first_pass":"accept"|"revise"|
# "reject"} msg to review:<crew> from this branch. One from an earlier session
# of the same dispatch counts, but unlike the review and deslop seams a new
# dispatch row resets it: retro windows runs by dispatch. `dispatch resume`
# posts a kind:"resume" row and does not rewrite WORKER_TASK.md, hence the bus
# check: a resumed branch skips the plan phase.
# Resume rows are crew-scoped on purpose (retro's rule is per-run, so it needs
# no crew check), and a resume counts only after the branch's latest dispatch.
#
# Under jqrun `.` is the array of the log's lines, and every `fromjson` is
# `_jqfromjson` (jqrun.WithJQFromJSON), jq's own fromjson: gojq's accepts a
# lone high surrogate escape that jq refuses, and a refused line or body must
# not count as a seam.
($b | ltrimstr("worker:")) as $n
| reduce .[] as $line (
  {seam: false, resumed: false};
  (try [$line | _jqfromjson] catch null) as $p
  | if $p == null or ($p[0] | type) != "object" then .
    else
      $p[0] as $m
      | if $m.crew_id != $c then .
        elif $m.kind == "dispatch" and $m.branch == $n then .resumed = false | .seam = false
        elif $m.kind == "resume" and $m.branch == $n then .resumed = true
        elif $m.kind == "msg" and $m.to == ("review:" + $c)
             and (($m.from // "") | tostring | sub("#s[^#]*$"; "")) == $b then
          (($m.body | _jqfromjson?) // null) as $o
          | if ($o | type) == "object" and $o.seam == "plan" and ($o | has("tag") | not)
               and ($o.plan_critic_first_pass | IN("accept", "revise", "reject"))
            then .seam = true else . end
        else . end
    end
) | if .seam or .resumed then 1 else 0 end
