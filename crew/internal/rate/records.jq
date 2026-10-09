# The arm's run fold, verbatim from adapters/core/crew.sh (the `rate)` sweep
# body). One record per RUN: a dispatch plus the branch events until the next
# dispatch on that branch. $repo and $costmap arrive as jqrun vars — the arm
# passed the costmap through --slurpfile only to dodge execve's E2BIG, and
# jqrun hands values in memory.
(map(select(.kind=="dispatch"))) as $disp
  | [ ($disp | map(.branch) | unique)[] as $b
      | ($disp | map(select(.branch==$b)) | sort_by(.ts)) as $runs
      | ($runs|length) as $n
      # Branch reuse (spec §Branch reuse and t2): the PR owner is the
      # earliest run on this branch whose OWN window contains a status
      # carrying a pr_url. Computed once per branch, entirely bus-derived,
      # before any per-run field depends on it.
      | ([ range(0; $n) as $k
          | $runs[$k] as $dk
          | $dk.ts as $tk0
          | (if $k+1 < $n then $runs[$k+1].ts else 9999999999999 end) as $tk1
          | (map(select(
                .kind!="dispatch"
                and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
                and .ts >= $tk0 and .ts < $tk1))
             | map(select(.kind=="status"))
             | map(.body.pr_url) | map(select(.!=null)) | last)
        ]) as $owner_prs
      | ($owner_prs | to_entries | map(select(.value != null)) | (.[0].key // null)) as $owner_idx
      | range(0; $n) as $i
      | $runs[$i] as $d
      | ($d.ts) as $t0
      | (if $i+1 < $n then $runs[$i+1].ts else 9999999999999 end) as $t1
      | (if $i+1 < $n then $runs[$i+1].ts else null end) as $window_end
      | (map(select(
            .kind!="dispatch"
            and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
            and .ts >= $t0 and .ts < $t1))) as $ev
      | ($ev | map(select(.kind=="status"))) as $st
      | ($st | sort_by(.ts) | (.[-1] // null)) as $last
      | ($last.body.state) as $ls
      | ($st | map(.body.pr_url) | map(select(.!=null)) | last) as $pr
      | ($st | map(select(.body.state=="pr_open")) | sort_by(.ts) | (.[0] // null)) as $propen
      | ($pr != null and $i == $owner_idx) as $owns_pr
      # Narrower than the terminal set below on purpose (spec §Cost): a
      # zombie has an unbounded wall clock, not a zero one.
      | ($st | map(select(.body.state=="done" or .body.state=="failed")) | sort_by(.ts) | (.[-1] // null)) as $term
      | (if $term != null then ($term.ts - $t0) else null end) as $wall
      | (($d.shape // null) | if . == "" then null else . end) as $shape
      | ($costmap[($d.model // "") + "\t" + ($d.effort // "")] // null) as $cw
      | (null) as $pr_state
      # Split rather than replace. `blocked_count` feeds an append-only,
      # cross-run ratings store whose rows are compared against rows written
      # before the watchdog existed; changing what populates that field in place
      # would make old and new rows silently non-comparable in the same field.
      # A new field leaves historical rows simply absent (null), which every
      # reader there already tolerates — and watchdog_blocked_count is direct
      # evidence of how often a model/tier wedges. A budget: row is a host-wide
      # quota crossing, not a model/tier wedge, so it stays out of
      # watchdog_blocked_count.
      | ($st | map(select(.body.state=="blocked" and (.body.source // "") != "watchdog")) | length) as $blocked
      | ($st | map(select(.body.state=="blocked" and (.body.source // "") == "watchdog"
                          and (((.body.detail // "") | if type == "string" then startswith("budget:") else false end) | not))) | length) as $wblocked
      # `try fromjson catch null` so ONE unparseable body cannot abort the whole
      # sweep and lose every other run with it (#25). Such a run folds with null
      # metrics — the same shape a run that never emitted metrics already takes.
      | ($ev | map(select(.kind=="msg" and ((.to // "") | startswith("metrics:"))))
             | sort_by(.ts) | (.[-1] // null)
             | if . == null then null else (.body | try fromjson catch null) end) as $m
      | {
          run_id: ($repo + ":" + $b + ":" + ($t0|tostring)),
          repo: $repo, branch: $b,
          session: ($d.session // null),
          engine: $d.engine, model: $d.model, tier: $d.tier,
          effort: $d.effort, title: $d.title,
          shape: $shape,
          # null for a run scaffolded by a `dispatch` older than this field.
          task_kind: ($d.task_kind // null),
          t0_ms: $t0,
          window_end_ms: $window_end,
          reached_pr: ($propen != null),
          pr_url: $pr,
          owns_pr: $owns_pr,
          time_to_pr_ms: (if $owns_pr and $propen != null then ($propen.ts - $t0) else null end),
          wall_clock_ms: $wall,
          cost_class: (if $cw == null then null else $cw[0] end),
          cost_proxy: (if $cw == null or $wall == null then null else ($cw[1] * $wall) end),
          # `done` is the success signal a worker posts for itself, and plenty
          # of dispatched work (reviews, measurements, experiments) has no PR
          # as its deliverable — so a missing PR is not evidence of failure.
          # Only a worker-reported `failed` is. Rows swept before this split
          # carry "failed" for both, so a reader spanning them must fold on
          # terminal_state, not on outcome.
          outcome: (
            if ($owns_pr and $pr_state == "MERGED") then "merged"
            elif ($ls == "working" or $ls == "blocked") then "running"
            elif ($pr != null) then "pr_open"
            elif ($ls == "done") then "done"
            elif ($ls == "failed") then "failed"
            else "incomplete" end),
          terminal_state: ($ls // null),
          rework_count: ($m.rework_count // null),
          replanned: (
            if $m == null or (($m | has("replanned")) | not)
            then null
            else $m.replanned
            end
          ),
          review_high: ($m.review_high // null),
          review_mode: ($m.review_mode // null),
          plan_critic_first_pass: ($m.plan_critic_first_pass // null),
          consulted: (if $m == null then null else $m.consulted end),
          blocked_count: $blocked,
          watchdog_blocked_count: $wblocked,
          reported_ok: (($m != null) and ($ls != null)),
          # t2 placeholders. Emitted null by the local fold so every row has
          # one shape from its first sweep; the reconcile below fills them.
          pr_state: $pr_state,
          last_query_ok: null,
          merged: null,
          closed_at_ms: null,
          merged_at_ms: null,
          merge_commit: null,
          time_to_merge_ms: null,
          review_rounds: null,
          first_ci_green: null,
          unresolved_notes: null,
          reverted: null,
          swept_at: (now*1000|floor)
} ]
