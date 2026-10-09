# The arm's skip-decision fold, verbatim from adapters/core/crew.sh apart from
# two patches: $records and $snap arrive as jqrun vars (the arm piped the two
# values and read them with `input`), and each run is one object collected into
# an array instead of an `@tsv` row — the `-` sentinels existed only to carry
# nulls through bash's `while read`.
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
      do_actions: ($s.first_ci_green == null),
      do_threads: (($s.pr_state == "MERGED" and $s.unresolved_notes != null) | not),
      s_commit: $s.merge_commit,
      s_merged: $s.merged_at_ms } ]
