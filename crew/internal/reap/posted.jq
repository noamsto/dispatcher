# The "posted since" check from crew.sh's `reap)` arm (`jq -n --arg w
# --argjson ts`), verbatim but for one patch: `any(inputs; …)` is
# `any(.[]; …)`, because jqrun hands the bus over as one decoded array rather
# than as jq's input stream. `any` still stops at the first match. The caller
# keeps the worktree on anything but a clean `false`, a bus it cannot read
# included, as the arm did.
any(.[]; (.kind == "status" or .kind == "msg")
  and ((.from // "") as $f | $f == $w or ($f | startswith($w + "#")))
  and .ts > $ts)
