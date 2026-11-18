# The roster base fold (the `base=$(jq -c -s ...)` step of the `roster)` arm
# in adapters/core/crew.sh, before the #829 Go port), run through gojq by
# internal/jqrun. `now` is rewritten to `$now`; `$crew` is passed as a
# variable. Unpatched: `sub("#[^#]*$";"")` needs no Oniguruma anchor fix —
# the greedy `[^#]*` absorbs a trailing newline in both engines.
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
      # title/engine/model/tier live on the dispatch event (keyed by branch);
      # join them per branch. last wins on re-dispatch. missing (pre-title
      # dispatch events, or no dispatch event at all) -> null.
      (map(select(.crew_id==$crew and .kind=="dispatch"))
        | map({key:.branch, value:{title:(.title // null), engine:(.engine // null), model:(.model // null), tier:(.tier // null)}}) | from_entries) as $dispatch
      |
      (map(select(.crew_id==$crew and (.kind=="dispatch" or .kind=="resume" or .kind=="relaunch")))
        | group_by(.branch)
        | map({key:.[0].branch, value:(sort_by(.ts) | last | .engine_session // null)}) | from_entries) as $esess
      | (map(select(.kind=="reap" and (.branch // "") != "")) | group_by(.branch)
          | map({key:.[0].branch, value:(max_by(.ts) | {ts, pr, pr_state, issues})}) | from_entries) as $reaps
      | . as $all
      | map(select(.crew_id==$crew and .kind=="status"
                   and ((.from // "") | startswith("worker:"))))
      | group_by(.from)
      | map(
          (max_by(.ts)) as $latest
          # A blocked episode is the trailing run of worker-posted blocked
          # rows; a row opens a new one unless it repeats the previous
          # detail or is a --restamp. Watchdog rows never count.
          | (map(select(.body.source != "watchdog")) | sort_by(.ts)) as $ws
          | ($ws | to_entries | map(select(.value.body.state != "blocked")) | (last // {key: -1}) | .key) as $cut
          | ($ws[$cut + 1:]) as $run
          | (if ($latest.body.state == "blocked" and $latest.body.source != "watchdog" and ($run | length) > 0)
             then ($run | to_entries
                   | map(select(.key == 0 or (.value.body.restamp != true
                                              and .value.body.detail != $run[.key - 1].body.detail)))
                   | last | .value.ts) else null end) as $since
          | (if $since == null then null
             else ($all | map(select(.crew_id==$crew and .kind=="msg"
                                     and .from == "dispatcher:" + $crew and .to == $latest.from
                                     and .ts > ($run | last | .ts)))
                   | map(.ts) | max) end) as $answered
          | {from: $latest.from,
             branch: ($latest.from | wid_branch),
             session: ($latest.from | wid_session),
             state: (if $answered != null then "answered" else $latest.body.state end),
             blocked_since: $since,
             answered_at: $answered,
             # `detail` is unbounded free text and the roster is an LLM-read
             # dashboard, so an untruncated row can crowd out the table. 120
             # fits every reserved prefix plus its payload; the full string
             # stays in the log. `source` tells a watchdog-posted `blocked`
             # (nobody is listening) from one the worker posts itself (someone is in await).
             detail: (($latest.body.detail // null) | if . == null then null else .[0:120] end),
             source: ($latest.body.source // null),
             ts: $latest.ts,
             # carry forward last-known pr_url — the terminal `done` event drops it
             pr_url: (map(.body.pr_url) | map(select(. != null)) | last),
             # last state that was NOT the exited backstop, so a spurious exited
             # can be resolved back to what the worker itself last reported.
             prev_state: (map(select(.body.state != "exited")) | max_by(.ts) | .body.state),
             age_s: ((now - ($latest.ts/1000))|floor)})
      | group_by(.branch)
      | map((sort_by(.ts) | last)
            + {title:  (.[0].branch as $b | $dispatch[$b].title  // null),
               engine: (.[0].branch as $b | $dispatch[$b].engine // null),
               model:  (.[0].branch as $b | $dispatch[$b].model  // null),
               tier:   (.[0].branch as $b | $dispatch[$b].tier   // null),
               engine_session: (.[0].branch as $b | $esess[$b] // null),
               sessions: (sort_by(.ts) | map({session, state, age_s}))})
      # reap stamps `reaped` only on the newest session's crew, so an older crew's
      # finished row for the branch is superseded by a later branch-level reap row.
      | map(. as $r | $reaps[.branch] as $rp
            | if (.state == "done" or .state == "failed" or .state == "exited") and $rp != null and $rp.ts > .ts
              then . + {state: "reaped", detail: ($rp | if (.pr_state == "ISSUE_CLOSED") and ((.issues // []) | length > 0) then "issue #" + ((.issues | map(tostring)) | join(", #")) + " CLOSED"
                                       elif (.pr // "") != "" and (.pr_state // "") != "" then "PR #" + (.pr | split("/") | last) + " " + .pr_state
                                       else (.pr_state // "reaped") end), source: "reap", ts: $rp.ts, age_s: ((now - ($rp.ts/1000))|floor)}
              else . end)
      # `crew reap` posts a `stale` row when it keeps a finished worker's tree
      # for a human; the newest one per branch flags the row until a
      # `stale:false` row or a newer status supersedes it.
      | (($all | map(select(.crew_id==$crew and .kind=="stale" and (.branch // "") != "")) | group_by(.branch)
          | map({key:.[0].branch, value:(max_by(.ts))}) | from_entries)) as $stale
      | map(. as $r | $stale[.branch] as $s
            | if (.state == "done" or .state == "failed" or .state == "exited")
                 and $s != null and $s.stale == true and $s.ts > .ts
              then . + {needs_cleanup: true, cleanup_reason: $s.reason, cleanup_worktree: $s.worktree,
                        cleanup_inspect: $s.inspect, cleanup_remove: $s.remove}
              else . end)
