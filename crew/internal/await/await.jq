# `crew await`'s candidate fold, verbatim from the arm's `jq -Rnc` in
# adapters/core/crew.sh (before the #884 Go port), run through gojq by
# internal/jqrun. Two patches, and nothing else:
#
#   reduce (inputs | fromjson?) as $e  →  reduce .[] as $e
#     jqrun hands one value: the array of rows the caller already decoded. The
#     caller's per-line decode IS the arm's `-R` + `fromjson?` skip, and
#     `fromjson?` cannot survive it — on a non-string it errors and `?` swallows
#     that, so the reduce would see zero candidates forever.
#   sort_by(.ts)[]  →  [sort_by(.ts)[]]
#     jqrun takes exactly one output value (hold/render.jq's join("\n") is the
#     precedent); the Go caller prints the array one compact line per element.
#
# $crew, $me and $from arrive as the arm's `--arg`s and $got as its `--argjson`:
# the marks read once before the loop. A msg is due when it is newer than the
# newest msg from its sender this session was handed; the last due row in log
# order picks the sender whose whole due backlog then prints, oldest first, so
# the mark raised for that batch can never skip a sibling (#466). `empty` is
# "nothing due", which the caller reads as "poll again".

reduce .[] as $e (
  {cands: []};
  if ($e.crew_id == $crew and $e.kind == "msg" and $e.to == $me and ($from == "" or $e.from == $from))
  then .cands += [$e]
  else . end
)
| .cands
| map(select(.ts > ($got[.from] // 0)))
| if length == 0 then empty
  else (.[-1].from) as $s
  | [map(select(.from == $s)) | sort_by(.ts)[]]
  end
