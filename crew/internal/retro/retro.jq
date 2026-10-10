# The `retro` fold from adapters/core/crew.sh (before the #887 Go port), run
# through gojq by internal/jqrun. The slurped event array is `.` and the two
# flags arrive as $want_report/$want_json — the arm passed them with `--argjson`.
# One patch, no others: bare mode's `jq -r` printed one @tsv row per line and
# jqrun wants exactly one value, so those rows are collected and joined — the
# report.jq patch, same reason — while `--report` already ended in a
# `join("\n")` and `--report --json` emits one object. The Go caller adds the one
# trailing newline, and prints nothing for the empty string, which is only ever
# zero rows: a @tsv row always carries its five tabs.

    # unique_by would sort, and both the tag list on a row and the note order
    # within a run are read in first-appearance order.
    def uniq_order: reduce .[] as $x ([]; if any(.[]; . == $x) then . else . + [$x] end);
    # The same note reaches the reader twice by design: a worker keeps its
    # mid-execute seam note in the final snapshot too. Dedupe on the whole
    # triple, since {seam,tag} repeats legitimately with a different detail.
    def dedupe_notes:
      reduce .[] as $x ([];
        if any(.[]; [.seam, .tag, .detail] == [$x.seam, $x.tag, $x.detail])
        then . else . + [$x] end);
    def tagstr:
      (map(.tag | tostring)) as $ts
      | ($ts | uniq_order)
      | map(. as $t
            | ($ts | map(select(. == $t)) | length) as $c
            | if $c > 1 then "\($t) x\($c)" else $t end)
      | join(",");
    # `try fromjson catch null` so ONE unparseable body cannot abort the fold
    # and lose every other run with it (#25).
    def body_obj: (try fromjson catch null) | if type == "object" then . else null end;
    def note_list: if type == "array" then map(select(type == "object")) else [] end;
    # Tags and details are agent-authored free text, and a gate_thrash detail
    # quotes arbitrary source and command output. Tab/newline/CR become a space
    # so a padded row keeps its columns; the rest go, because a raw ESC
    # repaints the table — one note can erase or forge the row above it — and
    # the bidi overrides let a string misrepresent itself in place.
    def clean:
      gsub("[\t\n\r]"; " ")
      | gsub("[\\x00-\\x1f\\x7f\\x{0080}-\\x{009f}\\x{202a}-\\x{202e}\\x{2066}-\\x{2069}]"; "");
    def sample: clean | if length > 60 then (.[0:60] + "…") else . end;
    def pad_row($cells; $widths; $left):
      # A right-aligned column pads before its value, so only a left-aligned
      # LAST column would trail the row with spaces. It gets none.
      (($cells | length) - 1) as $end
      | [ range(0; $cells | length) | . as $c
          | $cells[$c] as $cell
          | ($widths[$c] - ($cell | length)) as $p
          | if $left[$c] | not then ((" " * $p) + $cell)
            elif $c == $end then $cell
            else ($cell + (" " * $p)) end
        ] | join("  ");

    # Unlike every other tag here, "plan_required_unaudited" is not authored by
    # a worker or the dispatcher — `crew retro` synthesizes it itself from the
    # bus below. Do not add it to a WORKER_PROTOCOL.md/DISPATCHER_PROTOCOL.md
    # retro-notes table; there is no writer to document.
    ["command_not_found", "gate_thrash", "approach_abandoned", "consult_failed",
     "rung_blocked", "review_unavailable", "plan_required_unaudited", "other",
     "misrouted", "fanout_binder", "spec_too_thin", "session_summary"] as $vocab
    | . as $all
    | ($all | map(select(.kind == "dispatch"))) as $disp
    # Dispatch-boundary windowing, as `crew rate` folds it: run i owns
    # [runs[i].ts, runs[i+1].ts), so a re-dispatch on a reused branch cannot
    # inherit the notes of the run before it.
    | [ ($disp | map(.branch) | unique)[] as $b
        | ($disp | map(select(.branch == $b)) | sort_by(.ts)) as $runs
        | ($runs | length) as $n
        | range(0; $n) as $i
        | $runs[$i] as $d
        | $d.ts as $t0
        | (if $i + 1 < $n then $runs[$i+1].ts else 9999999999999 end) as $t1
        # A `dispatch resume` row has no `from` field, so it is invisible to
        # $ev (which is filtered by `from`) — match it directly off $all instead.
        | ($all | any(.kind == "resume" and .branch == $b and .ts >= $t0 and .ts < $t1)) as $resumed
        | ($all | map(select(
              .kind != "dispatch"
              and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b)
              and .ts >= $t0 and .ts < $t1))) as $ev
        | ($ev | map(select(.kind == "status")) | sort_by(.ts) | (.[-1] // null)) as $last
        # Latest snapshot only: putting notes INSIDE the snapshot is what makes
        # a resumed run supersede rather than duplicate (design §Mechanism).
        | ($ev | map(select(.kind == "msg" and ((.to // "") | startswith("metrics:"))))
               | sort_by(.ts) | (.[-1] // null)
               | if . == null then null else (.body | body_obj) end) as $m
        | (if $m == null then [] else ($m.notes | note_list) end) as $snap
        # `plan: required` with a null plan-critic verdict flags a skipped gate
        # (#179) — except tier/task_kind runs with no plan phase, a resumed run
        # (either shape — a `dispatch resume` row has no `from`, hence $resumed
        # above), or a run that never reached the review gate (review_mode ==
        # "none"), which never got far enough to skip anything. A valid plan seam
        # (review:<crew>) also counts: the snapshot may have forgotten the verdict.
        | ($ev | any(.[]; .kind == "msg"
                          and ((.to // "") | startswith("review:"))
                          and ((.body | body_obj) as $o
                               | $o != null and $o.seam == "plan"
                                 and ($o | has("tag") | not)
                                 and ($o.plan_critic_first_pass
                                      | IN("accept", "revise", "reject"))))) as $plan_seam
        | (if $d.plan == "required"
              and ($d.tier // null) != "trivial"
              and (($d.task_kind // "implement") != "review")
              and ($resumed | not)
              and (($d.resume // false) != true)
              and $m != null
              and (($m.plan_critic_first_pass // null) == null)
              and ($plan_seam | not)
              and (($m.review_mode // null) != "none")
           then [{seam: "plan", tag: "plan_required_unaudited",
                  detail: "plan: required but plan_critic_first_pass is null in the metrics snapshot"}]
           else [] end) as $flag
        | ($ev | map(select(.kind == "msg"
                            and ((.to // "") | startswith("retro:"))
                            and ((.from // "") | startswith("worker:"))))
               | sort_by(.ts) | map(.body | body_obj) | map(select(. != null))) as $seam
        | { branch: $b, crew: ($d.crew_id // null), engine: ($d.engine // "—"), model: ($d.model // "—"),
            tier: ($d.tier // "—"),
            # Type-guarded for the same reason as body_obj: a status whose body
            # is not an object, or whose state is not a scalar, must cost this
            # one run its outcome, not abort the fold and take every other row
            # down with it (#25).
            outcome: (($last.body | if type == "object" then .state else null end)
                      | if (type == "object" or type == "array") then null else . end
                      // "—"),
            t0: $t0, is_run: true,
            notes: (($seam + $snap + $flag) | dedupe_notes) }
      ] as $wrows
    # Dispatcher notes are crew-scoped, not run-scoped: their `from` matches no
    # branch, so no window can hold them.
    | ($all | map(select(.kind == "msg"
                         and ((.to // "") | startswith("retro:"))
                         and ((.from // "") | startswith("dispatcher:"))))
            | group_by(.to)
            | map({ branch: ("dispatcher:" + (.[0].to | ltrimstr("retro:"))),
                    crew: (.[0].to | ltrimstr("retro:")),
                    engine: "—", model: "—", tier: "—", outcome: "—",
                    t0: (map(.ts) | min), is_run: false,
                    notes: (sort_by(.ts) | map(.body | body_obj)
                            | map(select(. != null)) | dedupe_notes) })) as $drows
    # A clean run is silent: no notes, no row.
    # Tags are cleaned here, once, so the grouping key, the vocabulary check,
    # the first report column, the bare-mode tag list and the `unknown` array
    # all name the same value. Details stay verbatim — JSON encoding renders
    # their control bytes inert — so only `sample` cleans them.
    | (($wrows | map(select((.notes | length) > 0)) | sort_by([.branch, .t0]))
       + ($drows | map(select((.notes | length) > 0)))
       | map(.notes |= map(.tag |= (tostring | clean)))) as $rows
    | [ $rows[] as $r
        | $r.notes[] as $note
        | { tag: ($note.tag | tostring),
            detail: (($note.detail // "") | tostring),
            run: (if $r.is_run then ($r.branch + "@" + ($r.t0 | tostring)) else null end),
            engine: (if $r.is_run then $r.engine else null end),
            outcome: $r.outcome } ] as $flat
    | ($flat | group_by(.tag) | map(
        .[0].tag as $t
        | (map(select(.run != null))) as $wn
        | ($wn | map(.run) | unique) as $rl
        | { tag: $t,
            known: (($vocab | index($t)) != null),
            hits: length,
            runs: ($rl | length),
            engines: ($wn | map(.engine) | unique),
            nondone_pct: (
              if ($rl | length) == 0 then null
              else ((($wn | group_by(.run) | map(.[0])
                          | map(select(.outcome != "done")) | length) * 100)
                    / ($rl | length) | round)
              end),
            details: map(.detail) })) as $groups
    # Known tags in vocabulary order, then unknown ones alphabetically.
    | (($groups | map(select(.known)) | sort_by(. as $g | $vocab | index($g.tag)))
       + ($groups | map(select(.known | not)) | sort_by(.tag))) as $sorted
    | ($sorted | map(select(.known | not)) | map(.tag)) as $unknown
    | if $want_json then
        { tags: $sorted, unknown: $unknown,
          rows: ($rows | map({
              kind: (if .is_run then "run" else "dispatcher" end),
              crew, branch, engine, model, tier, outcome, t0,
              notes: (.notes | map({seam, tag, detail}))
            })) }
      elif $want_report then
        ["tag", "hits", "runs", "engines", "nondone", "sample"] as $headers
        | [true, false, false, true, false, true] as $left
        | ($sorted | map([
              .tag, (.hits | tostring), (.runs | tostring),
              (if (.engines | length) == 0 then "—" else (.engines | join(",")) end),
              (if .nondone_pct == null then "—" else (.nondone_pct | tostring) end),
              ((.details | map(select(. != "")) | (.[0] // "")) | sample)
            ])) as $trows
        # Widths and padding computed here — never `column -t`: it is not in
        # runtimeInputs, and BSD vs util-linux pad "—" differently.
        | ([range(0; $headers | length) | . as $c
            | (([$headers] + $trows) | map(.[$c] | length) | max)]) as $widths
        | pad_row($headers; $widths; $left) as $header_line
        | if ($sorted | length) == 0 then $header_line
          else
            ([$header_line]
             + ($trows | map(pad_row(.; $widths; $left)))
             + (if ($unknown | length) > 0
                then ["\($unknown | length) unrecognized tag(s): \($unknown | join(", "))"]
                else [] end)) | join("\n")
          end
      else [$rows[] | [.branch, .engine, .model, .tier, .outcome, (.notes | tagstr)] | @tsv] | join("\n")
      end
