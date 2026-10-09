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
      (map(select(.crew_id==$crew and (.kind=="dispatch" or .kind=="resume")))
        | group_by(.branch)
        | map({key:.[0].branch, value:(sort_by(.ts) | last | .engine_session // null)}) | from_entries) as $esess
      | map(select(.crew_id==$crew and .kind=="status"
                   and ((.from // "") | startswith("worker:"))))
      | group_by(.from)
      | map(
          (max_by(.ts)) as $latest
          | {from: $latest.from,
             branch: ($latest.from | wid_branch),
             session: ($latest.from | wid_session),
             state: $latest.body.state,
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
