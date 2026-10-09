# The arm's merge-forward fold, verbatim from adapters/core/crew.sh, against a
# FRESH store read: the window-1 snapshot decided which calls to skip, and
# carrying values forward from it would drop a t2 upgrade another sweep
# landed while this one was on the network. $records, $stored and $patches
# arrive as jqrun vars (the arm piped three values and read them with `input`)
# and the selected rows are collected into one array, because jqrun wants one
# value. The batch's single-write append is sweep.go's, same guarantee as the
# arm's `dd bs=1048576`: whole-batch atomicity while one write() holds, and a
# torn line stays unrecoverable because every reader folds a parse error to [].
[
  ($stored | map({key: .run_id, value: .}) | from_entries) as $S
  | (now * 1000 | floor) as $sw
  | ["pr_state","closed_at_ms","merged_at_ms","merge_commit",
     "review_rounds","first_ci_green","unresolved_notes","reverted"] as $t2keys
  | $records[] as $r
  | ($S[$r.run_id] // {}) as $s
  | ($patches[$r.run_id] // {}) as $p
  # For every field whose call did not succeed, the STORED value carries
  # forward — writing null instead would let one expired token permanently
  # degrade every already-reconciled row.
  | ($t2keys | map(. as $k | {key: $k, value: (
        if ($r.owns_pr | not) then null
        elif ($p | has($k)) then $p[$k]
        else $s[$k] end)}) | from_entries) as $t2
  | ($r + $t2 + {
      merged: (if $t2.pr_state == null then null else ($t2.pr_state == "MERGED") end),
      time_to_merge_ms: (if $t2.merged_at_ms == null then null else ($t2.merged_at_ms - $r.t0_ms) end),
      last_query_ok: $p.last_query_ok,
      outcome: (if ($r.owns_pr and $t2.pr_state == "MERGED") then "merged" else $r.outcome end),
      swept_at: $sw
    }) as $new
  # Ignoring swept_at is what makes an unchanged sweep a byte-level no-op;
  # without it every sweep would append every row forever.
  | select($S[$r.run_id] == null
           or ($S[$r.run_id] | del(.swept_at)) != ($new | del(.swept_at)))
  | $new
]
