# `_rr_d2` from adapters/core/crew.sh (the roster renderer's model -> D2 text),
# run through gojq by internal/jqrun. Two patches, both mechanical and neither
# touching a label, a key or a colour:
#
#   * `.[0]` — jqrun hands the decoded values over as the array `.`, where the
#     arm piped the model object into `jq -r`; and
#   * the `[ ... ]` wrap — jqrun returns one value, where `jq -r` emitted one
#     line per output. Go trims every trailing newline off the joined lines and
#     appends exactly one, which is `$(...)` + `printf '%s\n'` in the arm.
#
# `--argjson palette` and `--argjson hex` arrive as $palette and $hex. The
# program is otherwise verbatim, including the rule that every bus-, record- or
# tmux-sourced string reaches the output only inside a quoted label. The hex
# table travels as hex.json, looked up by palette name and never iterated, so a
# name with no entry gets no stroke.
[ .[0] | (
def cap($n): (if type == "string" then . elif . == null then "" else tojson end) | .[0:$n];
    # Cut at a word boundary: a mid-word cut drops the partial word.
    def trunc($n): (if type == "string" then . elif . == null then "" else tojson end)
      | if length <= $n then .
        else .[0:$n - 1] as $c
          | ($c | sub("\\s+\\S*$"; "")) as $w
          | (if (.[$n - 1:$n] | test("\\s")) or ($w | length) < ($n / 2) then $c else $w end
             | sub("\\s+$"; "")) + "…"
        end;
    def orq: if . == "" then "?" else . end;
    def q: gsub("[\u0000-\u0009\u000b-\u001f\u007f-\u009f]"; "")
      | gsub("\\\\"; "\\\\") | gsub("\""; "\\\"") | gsub("\\$"; "\\$") | gsub("\n"; "\\n")
      | "\"" + . + "\"";
    def hhmm: if type == "number" then . / 1000 | floor | strflocaltime("%H:%M") else "?" end;
    def loop: test("(^|[^A-Za-z0-9])r[0-9]+($|[^A-Za-z0-9])|revision [0-9]+|(^|[^A-Za-z])fix($|[^A-Za-z])|re-review");
    def among($s): . as $x | any($s[]; . == $x);
    .roles as $roles
    | .rows as $rows
    | def cnt($s): [$rows[] | select(.state | among($s))] | length;
    ($rows | to_entries | map(.value + {key: "w\(.key + 1)"})) as $w
    | cnt(["failed", "exited"]) as $f
    | "title: \"Crew roster\" {near: top-center; shape: text}",
      "legend: \"\(cnt(["working", "dispatched"])) active · \(cnt(["blocked"])) blocked · \(cnt(["pr_open", "done"])) done\(if $f > 0 then " · \($f) failed" else "" end)\" {near: bottom-center; shape: text}",
      "dispatcher: \"dispatcher\" {style.bold: true}",
      ($w[] | .key as $k
        | (.detail | if type == "string" then . elif . == null then "" else tojson end) as $full
        | ((.state | cap(24) | orq) + (if .source == "watchdog" then " (watchdog)" else "" end)) as $pre
        | (" · since " + (.ts | hhmm) + (.sessions | if length > 1 then " · \(length) sessions" else "" end)) as $post
        | ($full | loop) as $lp
        | (if $lp then " (loop)" else "" end) as $mark
        | (80 - ($pre | length) - ($post | length) - ($mark | length) - 3) as $room
        | (if $room < 10 then "" else $full | trunc($room) end) as $detail
        | ([(.name | cap(60) | orq),
            (.title | trunc(80) | orq),
            ([.tier, .engine, .model] | map(cap(60) | orq) | join("·") | trunc(80)),
            ($pre + (if $detail == "" then "" else " · " + $detail + $mark end) + $post)]
           | join("\n") | q) as $label
        | (if .state | among(["working", "blocked", "dispatched"]) then
             .branch as $b | [$roles[] | select(.branch == $b)] | sort_by(.role)
           else [] end) as $rp
        | "\($k): \($label) {",
          (if ($rp | length) > 0 then "  grid-rows: 1" else empty end),
          "  style: {fill: transparent; \(if .color | among($palette) then ($hex[.color] | if . then "stroke: \"\(.)\"; " else "" end) else "" end)stroke-width: 3; font-size: 16\(if .source == "watchdog" then "; stroke-dash: 3" else "" end)}",
          ($rp | to_entries[]
           | "  r\(.key + 1): \(.value | .role + "\n" + .engine + (if .state != "" then " · " + (.state | cap(32)) else "" end) | q)"),
          "}",
          "dispatcher -> \($k)",
          ((.pr_url | cap(200)) as $u
           | if (.state | among(["pr_open", "done"])) and $u != "" then
               first(($u | capture("/pull/(?<n>[0-9]+)") | .n), "") as $n
               | "\($k)_pr: \(if $n == "" then "PR" else "PR #" + $n end | q) {shape: page}",
                 "\($k) -> \($k)_pr\(if $n == "" then "" else ": " + ("#" + $n | q) end)"
             else empty end)),
      ($w[] | .key as $k | .base as $base
        | first($w[] | select(.branch == $base and .key != $k
                              and (.state | among(["working", "blocked", "pr_open", "dispatched"]))))
        | "\($k) -> \(.key): \"stacked on\""),
      (.holds | sort_by(.id) | to_entries[] | "h\(.key + 1)" as $k | .value
        | ([(("hold " + (.task.ref | cap(60))) | trunc(80)),
            (("waiting on " + (.wait.engine | cap(60)) + " " + (.wait.window | cap(60))) | trunc(80)),
            ("until " + (.wait.resets_at | if type == "number" then strflocaltime("%m-%d %H:%M") else "?" end))]
           | join("\n")) as $label
        | "\($k): \($label | q) {shape: hexagon}",
          "dispatcher -> \($k): {style.stroke-dash: 3}")
) ]
