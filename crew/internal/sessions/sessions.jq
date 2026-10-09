      # The `_sessions` fold from adapters/core/crew.sh (before the #829 Go port),
# run through gojq by internal/jqrun. `now` is rewritten to `$now` and
# `$b`/`$crew` are passed as variables.
#
# One engine patch: the split_wid anchor below. jq/Oniguruma `$` also matches
# before one trailing newline; gojq uses Go regexp, where `$` is absolute
# end-of-text. `\n?\z` (the regex the engine sees; doubled in this jq string
# literal) reproduces Oniguruma exactly — as the hand port's sessionSuffix
# once did.
def split_wid: ltrimstr("worker:") as $r
        | ($r | capture("#(?<s>s[0-9]+-[0-9]+)\\n?\\z").s // null) as $s
        | {branch: (if $s == null then $r else ($r | rtrimstr("#" + $s)) end), session: $s};
      def is_terminal: . as $x | (["done","failed","exited"] | index($x // "")) != null;
      map(select($crew=="" or .crew_id==$crew))
      | ( map(select((.kind=="dispatch" or .kind=="resume") and .branch==$b))
          | map({session:(.session // null), ts:.ts}) ) as $disp
      | ( map(select(.kind=="status" and ((.from // "") | startswith("worker:")))
              | ((.from) | split_wid) as $w
              | select($w.branch == $b)
              | {session:$w.session, state:.body.state, ts:.ts}) ) as $raw
      | ( ($disp + $raw) | map(select(.session != null)) | group_by(.session)
          | map({session:.[0].session, start:(map(.ts) | min)}) ) as $starts
      | ( $raw | map(select(.session != null)) ) as $sessioned
      | ( $raw | map(if .session != null then . else
            .ts as $t
            | ($starts | map(select(.start <= $t)) | max_by(.start) | .session) as $s
            | ($sessioned | map(select(.session == $s and .ts <= $t)) | max_by(.ts) | .state) as $prev
            | if ($prev | is_terminal) then . else .session = $s end
          end) ) as $st
      | ( ($disp + $st) | map(.session) | unique ) as $ids
      | [ $ids[] as $s
          | ($st | map(select(.session == $s)) | sort_by(.ts) | last) as $latest
          | ($disp | map(select(.session == $s)) | sort_by(.ts) | last) as $d
          | { session: $s,
              worker_id: ("worker:" + $b + (if $s == null then "" else "#" + $s end)),
              state: ($latest.state // null),
              ts: ($latest.ts // $d.ts),
              terminal: ((["done","failed","exited"] | index($latest.state // "")) != null) } ]
      | sort_by(.ts)
      | map(. + {age_s: (((now*1000) - .ts) / 1000 | floor)})
