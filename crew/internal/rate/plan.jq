# The sweep's skip-decision fold: which gh calls each run still needs.
# $records and $snap arrive as jqrun vars, and one object per run is collected
# into an array. do_actions settles a MERGED row 7 d after its merge (see
# below).
($snap | map({key: .run_id, value: .}) | from_entries) as $S
| (now * 1000) as $now
| [ $records[] as $r
  | ($S[$r.run_id] // {}) as $s
  | select($r.owns_pr)
  # Per-call finality (spec §Per-call finality): a run-level skip would
  # strand any field whose own call failed on the sweep that first saw the
  # merge, with no path back.
  | { run_id: $r.run_id,
      pr_url: $r.pr_url,
      branch: $r.branch,
      t0_ms: $r.t0_ms,
      win_end: $r.window_end_ms,
      do_view: (($s.pr_state == "MERGED")
                or ($s.pr_state == "CLOSED" and $s.closed_at_ms != null
                    and ($now - $s.closed_at_ms) >= 2592000000) | not),
      # first_ci_green reads runs from the run's window, which exist before the
      # merge, so an empty result a week after it is final; the 7 d grace covers
      # sweep downtime. `!= false` keeps a failed call retried: `== true` would
      # flip-flop, since a settled row tries nothing and so stores null. A
      # MERGED row with a null merged_at_ms stays queried.
      do_actions: ($s.first_ci_green == null
                   and (($s.pr_state == "MERGED" and $s.merged_at_ms != null
                         and ($now - $s.merged_at_ms) >= 604800000
                         and $s.last_query_ok != false) | not)),
      do_threads: (($s.pr_state == "MERGED" and $s.unresolved_notes != null) | not),
      s_commit: $s.merge_commit,
      s_merged: $s.merged_at_ms } ]
