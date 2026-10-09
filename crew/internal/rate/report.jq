# The `rate --report` render fold from adapters/core/crew.sh (before the #890 Go
# port), run through gojq by internal/jqrun. The slurped ratings array is `.` —
# jqrun's input is the array itself, so the deduped store goes over as its
# elements — and the three flags arrive as $want_json/$current_repo/$pooled,
# the arm's `--argjson`/`--arg` pair.
#
# Verbatim apart from the header: no jqrun patch is needed. The table branch
# already ended in a `join("\n")` and `--json` emits one value, so the program
# produces exactly one value on every path (the retro/report precedent), and it
# reads no `now`.
      . as $recs |
      # scoped mode intentionally drops rows with no/null .repo — an unlabeled row cannot be claimed to belong to "this repo".
      (if $pooled then $recs else ($recs | map(select(.repo == $current_repo))) end) as $recs |
      def median:
        sort | length as $l
        | if $l == 0 then null
          elif ($l % 2) == 1 then .[($l - 1) / 2 | floor]
          else (.[($l / 2 | floor) - 1] + .[$l / 2 | floor]) / 2
          end;
      def mean: if length == 0 then null else add / length end;
      def nrows: map(select(.outcome != "running" and .outcome != "incomplete"));
      def agg(k; n; v): {value: v, k: k, n: n};
      def fmt1:
        if . == null then null
        else
          (. * 10 | round) as $t
          | (($t / 10) | floor) as $i
          | ($t - ($i * 10)) as $f
          | "\($i).\($f)"
        end;
      # One humanised-duration rule, shared by ttpr/ttmerge.
      def humanize_ms:
        if . == null then null
        elif . < 5400000 then ((. / 60000 | round | tostring) + "m")
        else (((. / 3600000) | fmt1) + "h")
        end;
      # Marker rules (spec §Making small samples impossible to miss):
      # "—" when unmeasured, "(k)" when the own denominator differs from n,
      # "!" when that denominator is below 5.
      def render_agg(k; n; text):
        if k == 0 then "—"
        else (text + (if k != n then "(\(k))" else "" end) + (if k < 5 then "!" else "" end))
        end;

      def footer_line:
        if $pooled then
          ($recs | map(.repo // "unknown") | unique) as $buckets
          | ($buckets | length) as $r
          | ($buckets | sort) as $sorted
          | (if $r > 10 then (($sorted[0:10] | join(", ")) + ", and \($r - 10) more") else ($sorted | join(", ")) end) as $names
          | "Pooled over \($recs | length) runs from \($r) repo\(if $r == 1 then "" else "s" end): \($names)."
        else
          "Scoped to \($current_repo): \($recs | length) runs."
        end;

      # Tier x effort cross-tab, computed from the same $recs as a second
      # aggregation. known_efforts is the full --effort vocabulary (dispatch.sh:455);
      # tier_baseline names the typical rung per tier, marked with a trailing
      # "*" in the rendered cell (same suffix-on-cell-text idiom as the
      # "!"/"(k)" suffixes render_agg already appends). Any effort outside
      # known_efforts, or missing entirely, buckets to "unknown" — the
      # columns are always all 7, in this order, so the shape is stable
      # even when every row is "unknown".
      def known_efforts: ["low", "medium", "high", "xhigh", "max", "ultra"];
      def tier_baseline: {trivial: "low", standard: "medium", deep: "high"};
      def bucket_effort($e): if ($e != null) and (known_efforts | index($e)) then $e else "unknown" end;

      # Renders the whole cross-tab section (leading blank line through the
      # trailing legend line) as an array of lines, or [] when there are no
      # records — kept separate from the main table width/pad computation
      # above so that render path does not regress.
      def cross_tab_lines:
        if ($recs | length) == 0 then []
        else
          (known_efforts + ["unknown"]) as $cols
          | ($recs | map(.tier) | unique) as $tiers
          | ($tiers | map(. as $t
              | $cols | map(. as $c
                  | ($recs | map(select(.tier == $t and (bucket_effort(.effort) == $c))) | length)
                )
            )) as $counts
          | (["tier"] + $cols) as $ct_headers
          | ([true] + ($cols | map(false))) as $ct_left
          | ($tiers | to_entries | map(
              .key as $ti | .value as $t
              | [$t] + ($cols | to_entries | map(
                  .key as $ci | .value as $c
                  | ($counts[$ti][$ci] | tostring) as $base
                  | if (tier_baseline[$t] // null) == $c then ($base + "*") else $base end
                ))
            )) as $ct_rows
          | ([$ct_headers] + $ct_rows) as $ct_all
          | ($ct_headers | length) as $ct_ncols
          | ([range(0; $ct_ncols) | . as $c | ($ct_all | map(.[$c] | length) | max)]) as $ct_widths
          | ([range(0; $ct_ncols) | . as $c
              | $ct_headers[$c] as $cell
              | ($ct_widths[$c] - ($cell|length)) as $pad
              | if $ct_left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
             ] | join("  ")) as $ct_header_line
          | ($ct_rows | map(
              . as $row
              | [range(0; $ct_ncols) | . as $c
                 | $row[$c] as $cell
                 | ($ct_widths[$c] - ($cell|length)) as $pad
                 | if $ct_left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
                ] | join("  ")
            )) as $ct_body_lines
          | ([""] + [$ct_header_line] + $ct_body_lines + ["* = tier-typical effort rung"])
        end;

      def stats:
        $recs
        | group_by([.engine, .model, .tier])
        | map(
            . as $g
            | ($g | nrows) as $n
            | ($n | length) as $ncount
            | {
                engine: $g[0].engine, model: $g[0].model, tier: $g[0].tier,
                n: agg($ncount; $ncount; $ncount),
                inc: ($g | map(select(.outcome == "incomplete")) | length),
                run: ($g | map(select(.outcome == "running")) | length),
                pend: ($n | map(select(.pr_state == "OPEN")) | length),
                pr_pct: (
                  ($n | map(select(.reached_pr == true)) | length) as $num
                  | agg($ncount; $ncount; (if $ncount == 0 then null else (($num / $ncount) * 100 | round) end))
                ),
                merge_pct: (
                  ($n | map(select(.pr_state == "MERGED" or .pr_state == "CLOSED"))) as $settled
                  | ($settled | length) as $k
                  | ($settled | map(select(.pr_state == "MERGED")) | length) as $num
                  | agg($k; $ncount; (if $k == 0 then null else (($num / $k) * 100 | round) end))
                ),
                ttpr_ms: (
                  ($n | map(.time_to_pr_ms) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | median))
                ),
                ttmerge_ms: (
                  ($n | map(.time_to_merge_ms) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | median))
                ),
                rework: (
                  ($n | map(.rework_count) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                # review_mode ∉ {none, null} — a downgraded-but-run review
                # still counts, only "no review happened" is excluded.
                high: (
                  ($n | map(select((.review_mode != null) and (.review_mode != "none") and (.review_high != null))) | map(.review_high)) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                rounds: (
                  ($n | map(.review_rounds) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                # blocked_count/watchdog_blocked_count are bus-derived, never
                # null, so this is the one aggregate whose own denominator is
                # always n — it never carries a "(k)".
                blocked: (
                  ($n | map((.blocked_count // 0) + (.watchdog_blocked_count // 0))) as $vals
                  | agg($ncount; $ncount; ($vals | mean))
                ),
                ci1_pct: (
                  ($n | map(.first_ci_green) | map(select(. != null))) as $vals
                  | ($vals | length) as $k
                  | ($vals | map(select(. == true)) | length) as $num
                  | agg($k; $ncount; (if $k == 0 then null else (($num / $k) * 100 | round) end))
                ),
                notes: (
                  ($n | map(.unresolved_notes) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                rev: ($g | map(select(.reverted == true)) | length),
                cost_hours: (
                  ($n | map(.cost_proxy) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; (($vals | mean) | if . == null then null else . / 3600000 end))
                ),
                burn_median: (
                  ($n | map(.cost_proxy) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; (($vals | median) | if . == null then null else . / 3600000 end))
                )
              }
          )
        | sort_by([.engine, .model, .tier]);

      ["engine","model","tier","n","inc","run","pend","pr%","merge%","ttpr","ttmerge","rework","high","rounds","blocked","ci1","notes","rev","cost"] as $headers
      | [true,true,true,false,false,false,false,false,false,false,false,false,false,false,false,false,false,false,false] as $left
      | stats as $groups
      | if $want_json then $groups
        else
          ($groups | map(
              [
                .engine, .model, .tier,
                render_agg(.n.k; .n.n; (.n.value|tostring)),
                (.inc|tostring), (.run|tostring), (.pend|tostring),
                render_agg(.pr_pct.k; .pr_pct.n; (.pr_pct.value|tostring)),
                render_agg(.merge_pct.k; .merge_pct.n; (.merge_pct.value|tostring)),
                render_agg(.ttpr_ms.k; .ttpr_ms.n; (.ttpr_ms.value|humanize_ms)),
                render_agg(.ttmerge_ms.k; .ttmerge_ms.n; (.ttmerge_ms.value|humanize_ms)),
                render_agg(.rework.k; .rework.n; (.rework.value|fmt1)),
                render_agg(.high.k; .high.n; (.high.value|fmt1)),
                render_agg(.rounds.k; .rounds.n; (.rounds.value|fmt1)),
                render_agg(.blocked.k; .blocked.n; (.blocked.value|fmt1)),
                render_agg(.ci1_pct.k; .ci1_pct.n; (.ci1_pct.value|tostring)),
                render_agg(.notes.k; .notes.n; (.notes.value|fmt1)),
                (.rev|tostring),
                render_agg(.cost_hours.k; .cost_hours.n; (.cost_hours.value|fmt1))
              ]
            )) as $rows
          | ([$headers] + $rows) as $all
          | ($headers | length) as $ncols
          # Widths and padding computed here — never `column -t`: it is not in
          # runtimeInputs, and BSD vs util-linux pad "—" differently.
          | ([range(0; $ncols) | . as $c | ($all | map(.[$c] | length) | max)]) as $widths
          | ([range(0; $ncols) | . as $c
              | $headers[$c] as $cell
              | ($widths[$c] - ($cell|length)) as $pad
              | if $left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
             ] | join("  ")) as $header_line
          | if ($groups | length) == 0 then
              (if $pooled then $header_line else "\($current_repo): no runs swept for this repo yet" end)
            else
              ($rows | map(
                  . as $row
                  | [range(0; $ncols) | . as $c
                     | $row[$c] as $cell
                     | ($widths[$c] - ($cell|length)) as $pad
                     | if $left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
                    ] | join("  ")
                )) as $body_lines
              | ($groups | map(
                  [.n.k, .pr_pct.k, .merge_pct.k, .ttpr_ms.k, .ttmerge_ms.k, .rework.k, .high.k,
                   .rounds.k, .blocked.k, .ci1_pct.k, .notes.k, .cost_hours.k]
                  | any(. < 5)
                ) | map(select(.)) | length) as $flagged_count
              | ($groups | length) as $total
              | ([$header_line] + $body_lines
                 + ["", "! own sample < 5 — anecdote, not evidence.   value(k) = measured over k of n runs.   — = unmeasured."]
                 + ["\($flagged_count) of \($total) rows carry at least one small-sample quantity."]
                 + [footer_line]
                 + cross_tab_lines
                ) | join("\n")
            end
        end
