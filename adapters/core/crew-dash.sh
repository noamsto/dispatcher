#!/usr/bin/env bash
# crew-dash — read-only dashboard over the layered dispatcher settings,
# per-engine budget, and the last few runs' retro notes and ratings.
#
# Modes:
#   crew dash            interactive when stdin/stdout are TTYs (alternate
#                         screen, tab-switchable panes, `r` to re-collect);
#                         --once otherwise.
#   crew dash --once     all four panes as plain text, then exit.
#   crew dash --json     the collected model, the same document any renderer
#                         reads.
#
# Probes nothing itself — every figure comes from a cached or already-run
# source: `dispatch-config --show-origin` / `--layers`, `refresh-budget
# --report --json` (cache only), `crew retro --report --json`, `crew rate
# --report --json`, `crew crews` / `crew roster` / `crew hold list`. Each
# source's stdout/stderr is captured to a temp file and folded into the model
# as that pane's data/warnings/error; a source failing degrades only its own
# pane (`unavailable: <first stderr line>`) — the dashboard itself always
# exits 0 from a source failure.
#
# `NO_COLOR` (any non-empty value) or `CREW_DASH_COLOR=never` disables all
# SGR; `CREW_DASH_COLOR=always` forces it on even off a pipe; the default
# (`CREW_DASH_COLOR` unset or `auto`) colors only a real stdout TTY.
set -euo pipefail
export LC_ALL=C.UTF-8

# cp_width <codepoint> — sets $_w to the display width (0, 1 or 2) of one
# Unicode codepoint. Ranges per East-Asian-Wide/emoji + zero-width tables.
cp_width() {
  local cp=$1
  if ((cp >= 0x0300 && cp <= 0x036F || cp >= 0x200B && cp <= 0x200F || cp >= 0xFE00 && cp <= 0xFE0F)); then
    _w=0
  elif ((\
    cp >= 0x1100 && cp <= 0x115F || \
    cp >= 0x2E80 && cp <= 0x303E || \
    cp >= 0x3041 && cp <= 0x33FF || \
    cp >= 0x3400 && cp <= 0x4DBF || \
    cp >= 0x4E00 && cp <= 0x9FFF || \
    cp >= 0xA000 && cp <= 0xA4CF || \
    cp >= 0xAC00 && cp <= 0xD7A3 || \
    cp >= 0xF900 && cp <= 0xFAFF || \
    cp >= 0xFE30 && cp <= 0xFE4F || \
    cp >= 0xFF00 && cp <= 0xFF60 || \
    cp >= 0xFFE0 && cp <= 0xFFE6 || \
    cp >= 0x1F300 && cp <= 0x1F64F || \
    cp >= 0x1F900 && cp <= 0x1F9FF || \
    cp >= 0x20000 && cp <= 0x3FFFD)) \
      ; then
    _w=2
  else
    _w=1
  fi
}

# trunc <width> <text> — sets $TRUNC_RESULT to <text> cut to at most <width>
# display cells, ending in a single "…" when it had to cut. Pure bash (no
# forks): this runs on every line of every interactive repaint.
trunc() {
  local w=$1 s=$2
  if [[ "$s" != *[![:ascii:]]* ]]; then
    if ((${#s} <= w)); then
      TRUNC_RESULT="$s"
    elif ((w <= 1)); then
      TRUNC_RESULT="${s:0:w}"
    else
      TRUNC_RESULT="${s:0:w-1}…"
    fi
    return
  fi
  local len=${#s} i ch cp total=0 kept="" fits=1
  for ((i = 0; i < len; i++)); do
    ch="${s:i:1}"
    printf -v cp '%d' "'$ch"
    cp_width "$cp"
    if ((total + _w > w)); then
      fits=0
      break
    fi
    total=$((total + _w))
    kept+="$ch"
  done
  if ((fits == 1)); then
    TRUNC_RESULT="$s"
    return
  fi
  while ((total + 1 > w)) && [ -n "$kept" ]; do
    ch="${kept: -1}"
    printf -v cp '%d' "'$ch"
    cp_width "$cp"
    total=$((total - _w))
    kept="${kept%?}"
  done
  TRUNC_RESULT="${kept}…"
}

# Hidden test hook: exercise trunc() without a tty or the collector. Reads
# "<width>\t<text>" lines from stdin, prints trunc() of each, exits.
if [ "${CREW_DASH_TRUNC_TEST:-}" = 1 ]; then
  while IFS=$'\t' read -r _width _text; do
    trunc "$_width" "$_text"
    printf '%s\n' "$TRUNC_RESULT"
  done
  exit 0
fi

case "$#:${1:-}" in
"0:")
  if [ -t 0 ] && [ -t 1 ]; then mode=interactive; else mode=once; fi
  ;;
"1:--once") mode=once ;;
"1:--json") mode=json ;;
*)
  echo "usage: crew dash [--once | --json]" >&2
  exit 2
  ;;
esac

tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

# crew_cmd — the crew CLI to call back into. $CREW_BIN, when set, names the
# exact `crew` build that delegated here (crew.sh's own resolved path). A
# raw-source `bash crew.sh dash` leaves that file mode 644 (not executable),
# so a non-executable CREW_BIN runs through `bash -euo pipefail` instead of
# being execed directly; unset falls back to `crew` on PATH.
crew_cmd=(crew)
if [ -n "${CREW_BIN:-}" ]; then
  if [ -x "$CREW_BIN" ]; then
    crew_cmd=("$CREW_BIN")
  else
    crew_cmd=(bash -euo pipefail "$CREW_BIN")
  fi
fi

dcfg_bin="${DISPATCH_CONFIG_BIN:-@dispatchConfig@}"

# run_src <name> <cmd...> — run a read-only source with stdout/stderr each to
# their own temp file, and fold the result into {data, error, warnings}:
# rc 0 with JSON stdout -> data is that JSON, warnings are stderr's lines;
# anything else -> data null, error is stderr's first line (or "exit <rc>"
# when stderr is empty), warnings []. `retro` gets one more case: rc 0 with
# empty stdout (no events.jsonl at all) reads as the empty report, not a
# failure — `crew retro` prints nothing in that case.
run_src() {
  local name="$1"
  shift
  local out="$tmp_dir/$name.out" err="$tmp_dir/$name.err" rc=0
  "$@" >"$out" 2>"$err" || rc=$?
  jq -n --rawfile out "$out" --rawfile err "$err" --argjson rc "$rc" --arg name "$name" '
    ($err | split("\n") | map(select(. != ""))) as $errlines
    | ($out | (try fromjson catch null)) as $parsed
    | if $rc == 0 and $parsed != null then
        {data: $parsed, error: null, warnings: $errlines}
      elif $rc == 0 and $name == "retro" and ($out | gsub("\\s"; "")) == "" then
        {data: {tags: [], unknown: [], rows: []}, error: null, warnings: $errlines}
      else
        {data: null, error: ($errlines[0] // "exit \($rc)"), warnings: []}
      end
  '
}

# collect — run every source and set the global $model (and $now) from it.
# Re-run on demand by the interactive renderer's `r` key; --once/--json call
# it exactly once.
collect() {
  now=$(date +%s)
  now_clock=$(date +%H:%M:%S)

  # Each source's JSON goes straight to a file, never through --argjson: a
  # real settings/retro store can exceed the kernel's per-argument limit
  # (ARG_MAX), so the model build below slurps these files instead.
  run_src settings "$dcfg_bin" --show-origin >"$tmp_dir/settings_raw.json"
  run_src layers "$dcfg_bin" --layers >"$tmp_dir/layers_raw.json"
  run_src budget refresh-budget --report --json >"$tmp_dir/budget_raw.json"
  run_src retro "${crew_cmd[@]}" retro --report --json >"$tmp_dir/retro_raw.json"
  run_src ratings "${crew_cmd[@]}" rate --report --json >"$tmp_dir/ratings_raw.json"

  # crews/roster/holds: `crew crews` is a TSV, not JSON, so it is read outside
  # run_src. A per-crew `roster`/`hold list` failure degrades only that crew to
  # empty lists — these read-only calls have no documented failure worth
  # surfacing on its own, unlike the five JSON sources above.
  crews_out="$tmp_dir/crews.out"
  crews_err="$tmp_dir/crews.err"
  crews_rc=0
  "${crew_cmd[@]}" crews >"$crews_out" 2>"$crews_err" || crews_rc=$?

  # One compact JSON object per line; slurped as an array below instead of
  # accumulated through --argjson (which would re-pass every crew seen so
  # far, through argv, on every iteration).
  roster_lines="$tmp_dir/roster_crews.jsonl"
  : >"$roster_lines"
  roster_error=null
  if [ "$crews_rc" -eq 0 ]; then
    while IFS=$'\t' read -r cid _last _first _workers _pid alive; do
      [ "$alive" = yes ] || continue
      workers_json=$("${crew_cmd[@]}" roster "$cid" 2>/dev/null) || workers_json='[]'
      jq -e . >/dev/null 2>&1 <<<"$workers_json" || workers_json='[]'
      holds_json=$("${crew_cmd[@]}" hold list --crew "$cid" --json 2>/dev/null) || holds_json='[]'
      jq -e . >/dev/null 2>&1 <<<"$holds_json" || holds_json='[]'
      jq -nc --arg id "$cid" --argjson w "$workers_json" --argjson h "$holds_json" \
        '{id: $id, workers: $w, holds: $h}' >>"$roster_lines"
    done < <(tail -n +2 "$crews_out")
  else
    first_err=$(head -n1 "$crews_err")
    roster_error=$(jq -Rn --arg e "${first_err:-exit $crews_rc}" '$e')
  fi
  jq -c -s '.' "$roster_lines" >"$tmp_dir/roster_crews.json"

  model=$(jq -n \
    --argjson now "$now" \
    --argjson roster_error "$roster_error" \
    --slurpfile settings_raw_f "$tmp_dir/settings_raw.json" \
    --slurpfile layers_raw_f "$tmp_dir/layers_raw.json" \
    --slurpfile budget_raw_f "$tmp_dir/budget_raw.json" \
    --slurpfile retro_raw_f "$tmp_dir/retro_raw.json" \
    --slurpfile ratings_raw_f "$tmp_dir/ratings_raw.json" \
    --slurpfile roster_crews_f "$tmp_dir/roster_crews.json" '
    $settings_raw_f[0] as $settings_raw
    | $layers_raw_f[0] as $layers_raw
    | $budget_raw_f[0] as $budget_raw
    | $retro_raw_f[0] as $retro_raw
    | $ratings_raw_f[0] as $ratings_raw
    | $roster_crews_f[0] as $roster_crews
    | def is_leaf: type == "object" and (keys_unsorted | length) == 2 and has("origin") and has("value") and (.origin | type) == "string";
    def leaves($path):
      if is_leaf then {path: $path, value: .value, origin: .origin}
      else to_entries[] | (.key as $k | .value | leaves($path + [$k]))
      end;
    ($settings_raw.warnings + $layers_raw.warnings | unique) as $warn
    | {
        now: $now,
        settings: {
          layers: $layers_raw.data,
          rows: (
            if $settings_raw.data == null then []
            else
              ($settings_raw.data | [leaves([])])
              | map(
                  . as $leaf
                  | (($leaf.path == ["grantRoots"]) or ($leaf.path == ["openrouter", "keyFile"])) as $lo
                  | {
                      path: $leaf.path, value: $leaf.value, origin: $leaf.origin,
                      locked_only: $lo,
                      editable: (($leaf.origin == "base" or $leaf.origin == "user") and ($lo | not))
                    }
                )
            end
          ),
          warnings: $warn,
          error: $settings_raw.error
        },
        budget: {report: $budget_raw.data, warnings: $budget_raw.warnings, error: $budget_raw.error},
        runs: {
          retro: $retro_raw.data, retro_error: $retro_raw.error,
          ratings: $ratings_raw.data, ratings_error: $ratings_raw.error
        },
        roster: {
          crews: ($roster_crews | map(.workers |= map(.age_s = ($now - ((.ts // 0) / 1000 | floor))))),
          error: $roster_error
        }
      }
  ')
}

collect

if [ "$mode" = json ]; then
  jq '.' <<<"$model"
  exit 0
fi

# ---------------------------------------------------------------------------
# Renderer — one jq library producing STYLE\tROW\tTEXT lines over the model.
# STYLE: h (heading), l (locked settings row), n (normal). ROW is a settings
# row's index into .settings.rows for a future cursor; -1 everywhere else.
# ---------------------------------------------------------------------------

# shellcheck disable=SC2016 # this is jq program text, not shell
RENDER_DEFS='
def clean:
  gsub("[\t\n\r]"; " ")
  | gsub("[\\x00-\\x1f\\x7f\\x{0080}-\\x{009f}\\x{202a}-\\x{202e}\\x{2066}-\\x{2069}]"; "");

def reltime: . as $s
  | ($s / 86400 | floor) as $d
  | (($s % 86400) / 3600 | floor) as $h
  | (($s % 3600) / 60 | floor) as $m
  | if $d > 0 then "\($d)d \($h)h"
    elif $h > 0 then "\($h)h \($m)m"
    else "\($m)m" end;

def usd: (. * 100 | round) as $c
  | "\($c / 100 | floor).\(($c % 100) | tostring | if length == 1 then "0" + . else . end)";

def fmt1:
  if . == null then null
  else
    (. * 10 | round) as $t
    | (($t / 10) | floor) as $i
    | ($t - ($i * 10)) as $f
    | "\($i).\($f)"
  end;

def render_agg(k; n; text):
  if k == 0 then "—"
  else (text + (if k != n then "(\(k))" else "" end) + (if k < 5 then "!" else "" end))
  end;

def pad_row($cells; $widths; $left):
  (($cells | length) - 1) as $end
  | [ range(0; $cells | length) | . as $c
      | $cells[$c] as $cell
      | ($widths[$c] - ($cell | length)) as $p
      | if $left[$c] | not then ((" " * $p) + $cell)
        elif $c == $end then $cell
        else ($cell + (" " * $p)) end
    ] | join("  ");

def table_widths($rows):
  ($rows[0] | length) as $ncols
  | [range(0; $ncols) | . as $c | ($rows | map(.[$c] | length) | max)];

def common_len($a; $b):
  ([$a, $b] | map(length) | min) as $n
  | ([range(0; $n) | select($a[.] != $b[.])] | first) // $n;

def badge_of($origin):
  if $origin == "base" then "default"
  elif $origin == "user" then "user"
  elif $origin == "env" then "env"
  elif $origin == "locked" then "🔒 locked"
  else $origin end;

def leaf_prefix($row): ("  " * (($row.path | length) - 1)) + ($row.path[-1]) + ": " + ($row.value | tojson);

def settings_body($rows):
  ($rows | map(leaf_prefix(.))) as $prefixes
  | ($prefixes | map(length) | if length == 0 then 0 else max end) as $maxlen
  | ([$maxlen, 48] | min) as $cap
  | (reduce ($rows | to_entries[]) as $e
       ({prev: [], out: []};
          $e.value as $r
          | $e.key as $idx
          | ($r.path[0:-1]) as $prefix
          | common_len(.prev; $prefix) as $common
          | [range($common; $prefix | length) | {style: "n", row: -1, text: (("  " * .) + $prefix[.])}] as $branch_lines
          | ($prefixes[$idx]) as $pfx
          | (($cap + 2 - ($pfx | length)) as $pad0 | if $pad0 < 2 then 2 else $pad0 end) as $pad
          | badge_of($r.origin) as $badge
          | (if $r.origin == "locked" then "l" else "n" end) as $style
          | . + {
              prev: $r.path,
              out: (.out + $branch_lines + [{style: $style, row: $idx, text: ($pfx + (" " * $pad) + $badge)}])
            }
       )
    ) as $acc
  | $acc.out;

def layer_lines($layers):
  if $layers == null then []
  else [
    {style: "n", row: -1, text: ("layers: default " + $layers.base)},
    {style: "n", row: -1, text: ("        user    " + $layers.user.path + " (" + (if $layers.user.present then "present" else "absent" end) + ")")},
    {style: "n", row: -1, text: ("        locked  " + (if $layers.locked == null then "none" else $layers.locked end))}
  ] end;

def settings_pane($model):
  $model.settings as $s
  | layer_lines($s.layers)
  + ($s.warnings | map({style: "n", row: -1, text: ("warning: " + .)}))
  + (if $s.error != null then [{style: "n", row: -1, text: ("unavailable: " + $s.error)}]
     else settings_body($s.rows) end);

def window_row_cells($w):
  [ $w.key,
    "\($w.used_pct)%",
    (if $w.ahead_pts == null then "—" elif $w.ahead_pts > 0 then "+\($w.ahead_pts)" else ($w.ahead_pts | tostring) end),
    (if $w.resets_in_s == null then "—" else ($w.resets_in_s | reltime) end),
    ($w.verdict // "—")
  ];

def engine_lines($e; $v):
  if $v == null then [{style: "n", row: -1, text: ($e + ": unknown")}]
  else
    ($e + " (" + $v.source + ")" + (if $v.plan_type != null then " [" + $v.plan_type + "]" else "" end)) as $heading
    | ([["window", "used", "pace", "resets in", "verdict"]] + ($v.windows // [] | map(window_row_cells(.)))) as $table
    | table_widths($table) as $widths
    | ($table | map(pad_row(.; $widths; [true, false, false, true, true]) | ("  " + .))) as $tbl_lines
    | (if $v.source == "openrouter_key" then
         (if $v.target_usd != null then
            [("  spend $" + ($v.spend_usd | usd) + " of $" + ($v.target_usd | usd) + " target, " + ($v.elapsed_pct | tostring) + "% of month elapsed")]
          else
            [("  spend $" + ($v.spend_usd | usd) + " month-to-date (no target)")]
          end)
         + (if $v.projection != null then [("  " + $v.projection)] else [] end)
       else [] end) as $extra
    | [{style: "h", row: -1, text: $heading}]
    + ($tbl_lines | map({style: "n", row: -1, text: .}))
    + ($extra | map({style: "n", row: -1, text: .}))
  end;

def budget_pane($model):
  $model.budget as $b
  | if $b.error != null then [{style: "n", row: -1, text: ("unavailable: " + $b.error)}]
    else
      ($model.now - $b.report.fetched_epoch) as $age
      | ("fetched " + ($age | reltime) + " ago" + (if $age > 7200 then " (stale)" else "" end)) as $fetched_line
      | [{style: "n", row: -1, text: $fetched_line}]
      + ($b.report.engines | to_entries | map(engine_lines(.key; .value)) | add)
    end;

def tagstr_dash($notes):
  ($notes | map(.tag)) as $ts
  | (reduce $ts[] as $t ([]; if any(.[]; . == $t) then . else . + [$t] end)) as $order
  | ($order | map(. as $t | ($ts | map(select(. == $t)) | length) as $c | if $c > 1 then "\($t) x\($c)" else $t end))
  | join(", ");

def crews_grouped($rows):
  ($rows | map(select(.crew != null)) | group_by(.crew)) as $groups
  | ($groups | map({crew: .[0].crew, rows: (sort_by(.t0)), maxt0: (map(.t0) | max)}))
  | sort_by(-.maxt0) | .[0:5];

def crew_notes_lines($g):
  ($g.rows | map(.notes[])) as $notes
  | (($notes | map(select(.tag == "session_summary"))) | last) as $summary
  | ($notes | map(select(.tag != "session_summary"))) as $rest
  | [{style: "h", row: -1, text: ("crew " + $g.crew)}]
  + [{style: "n", row: -1, text: ("  summary: " + (if $summary == null then "(none)" else ($summary.detail // "" | clean) end))}]
  + (if ($rest | length) > 0 then [{style: "n", row: -1, text: ("  tags: " + tagstr_dash($rest))}] else [] end)
  + ($rest | .[-3:] | reverse | map({style: "n", row: -1, text: ("  " + (.tag | clean) + ": " + (.detail // "" | clean))}));

def ratings_table($groups):
  if ($groups | length) == 0 then [{style: "n", row: -1, text: "no runs swept for this repo yet"}]
  else
    (["tier", "engine", "model", "n", "pr%", "merge%", "burn(med)"]) as $headers
    | ($groups | sort_by([.tier, .engine, .model])) as $sorted
    | ($sorted | map([
        .tier, .engine, .model,
        render_agg(.n.k; .n.n; (.n.value | tostring)),
        render_agg(.pr_pct.k; .pr_pct.n; (.pr_pct.value | tostring)),
        render_agg(.merge_pct.k; .merge_pct.n; (.merge_pct.value | tostring)),
        render_agg(.burn_median.k; .burn_median.n; (.burn_median.value | fmt1))
      ])) as $rows
    | ([$headers] + $rows) as $all
    | table_widths($all) as $widths
    | ([true, true, true, false, false, false, false]) as $left
    | ([pad_row($headers; $widths; $left)] + ($rows | map(pad_row(.; $widths; $left))))
    | map({style: "n", row: -1, text: .})
  end;

def runs_pane($model):
  $model.runs as $r
  | (if $r.retro_error != null then [{style: "n", row: -1, text: ("unavailable: " + $r.retro_error)}]
     else
       crews_grouped($r.retro.rows // []) as $groups
       | if ($groups | length) == 0 then [{style: "n", row: -1, text: "no retro notes yet"}]
         else [$groups[] | crew_notes_lines(.)] | add
         end
     end) as $part_a
  | (if $r.ratings_error != null then [{style: "n", row: -1, text: ("unavailable: " + $r.ratings_error)}]
     else ratings_table($r.ratings // [])
     end) as $part_b
  | $part_a + [{style: "n", row: -1, text: ""}] + $part_b;

def worker_cells($w):
  [ ($w.name // $w.branch // "—" | clean),
    ($w.state // "—"),
    (($w.tier // "—") + "/" + ($w.engine // "—") + "/" + ($w.model // "—")),
    ($w.age_s | reltime),
    ($w.pr_url // "—")
  ];

def roster_crew_lines($c):
  [{style: "h", row: -1, text: ("crew " + $c.id)}] as $head
  | (["name", "state", "tier/engine/model", "age", "pr"]) as $headers
  | ($c.workers | map(worker_cells(.))) as $wrows
  | (if ($wrows | length) == 0 then []
     else
       ([$headers] + $wrows) as $all
       | table_widths($all) as $widths
       | ([true, true, true, true, true]) as $left
       | ([pad_row($headers; $widths; $left)] + ($wrows | map(pad_row(.; $widths; $left))))
       | map({style: "n", row: -1, text: ("  " + .)})
     end) as $wlines
  | ($c.holds | map({style: "n", row: -1, text: ("  hold " + (.id | tostring) + ": " + .wait.engine + " " + .wait.window + " until " + (.wait.resets_at | todateiso8601) + " — " + (.task.title // "" | clean))})) as $hlines
  | $head + $wlines + $hlines;

def roster_pane($model):
  $model.roster as $r
  | if $r.error != null then [{style: "n", row: -1, text: ("unavailable: " + $r.error)}]
    elif ($r.crews | length) == 0 then [{style: "n", row: -1, text: "no active crew"}]
    else [$r.crews[] | roster_crew_lines(.)] | add
    end;

def render_pane($model; $pane):
  if $pane == "settings" then settings_pane($model)
  elif $pane == "budget" then budget_pane($model)
  elif $pane == "runs" then runs_pane($model)
  elif $pane == "roster" then roster_pane($model)
  else [] end;

def once_lines($model):
  ["settings", "budget", "runs", "roster"] as $panes
  | {settings: "Settings", budget: "Budget", runs: "Runs", roster: "Roster"} as $titles
  | [range(0; $panes | length) as $i
     | ($panes[$i]) as $p
     | (if $i > 0 then [{style: "n", row: -1, text: ""}] else [] end)
     + [{style: "h", row: -1, text: ("== " + $titles[$p] + " ==")}]
     + render_pane($model; $p)
    ] | add;
'

color_on=false
if [ -n "${NO_COLOR:-}" ] || [ "${CREW_DASH_COLOR:-}" = never ]; then
  color_on=false
elif [ "${CREW_DASH_COLOR:-}" = always ]; then
  color_on=true
elif { [ -z "${CREW_DASH_COLOR:-}" ] || [ "${CREW_DASH_COLOR:-}" = auto ]; } && [ -t 1 ]; then
  color_on=true
fi

# ---------------------------------------------------------------------------
# Interactive TUI — tab-switchable panes over the same render_pane() library.
# ---------------------------------------------------------------------------

panes=(settings budget runs roster)
pane_titles=(Settings Budget Runs Roster)
# Only reached through `local -n` namerefs built from $name (render_panes,
# paint), so shellcheck can't see the uses.
# shellcheck disable=SC2034
declare -ag pane_style_settings pane_row_settings pane_text_settings
# shellcheck disable=SC2034
declare -ag pane_style_budget pane_row_budget pane_text_budget
# shellcheck disable=SC2034
declare -ag pane_style_runs pane_row_runs pane_text_runs
# shellcheck disable=SC2034
declare -ag pane_style_roster pane_row_roster pane_text_roster
declare -A offset=([settings]=0 [budget]=0 [runs]=0 [roster]=0)
active=0
cursor_settings=0
settings_nrows=0
resized=0

# render_panes — run render_pane() once per pane over the current $model into
# the pane_{style,row,text}_<name> arrays. Only called on collect/resize, not
# per keypress.
render_panes() {
  local i name st rw tx
  for i in 0 1 2 3; do
    name="${panes[$i]}"
    local -n _styles="pane_style_$name"
    local -n _rows="pane_row_$name"
    local -n _texts="pane_text_$name"
    _styles=()
    _rows=()
    _texts=()
    while IFS=$'\t' read -r st rw tx; do
      _styles+=("$st")
      _rows+=("$rw")
      _texts+=("$tx")
    done < <(jq -r --arg pane "$name" "$RENDER_DEFS"'
      render_pane(.; $pane)[] | [.style, (.row|tostring), (.text | gsub("[\t\n]"; " "))] | join("\t")
    ' <<<"$model")
  done
  settings_nrows=0
  for rw in "${pane_row_settings[@]}"; do
    [ "$rw" = -1 ] || settings_nrows=$((settings_nrows + 1))
  done
  if [ "$cursor_settings" -ge "$settings_nrows" ]; then
    cursor_settings=$((settings_nrows > 0 ? settings_nrows - 1 : 0))
  fi
}

# move_cursor down|up — settings: move the leaf-row cursor; other panes:
# scroll by one line (bounds are enforced in paint()).
move_cursor() {
  local dir="$1" name="${panes[$active]}"
  if [ "$name" = settings ]; then
    [ "$settings_nrows" -gt 0 ] || return 0
    if [ "$dir" = down ] && [ "$cursor_settings" -lt "$((settings_nrows - 1))" ]; then
      cursor_settings=$((cursor_settings + 1))
    elif [ "$dir" = up ] && [ "$cursor_settings" -gt 0 ]; then
      cursor_settings=$((cursor_settings - 1))
    fi
  elif [ "$dir" = down ]; then
    offset[$name]=$((offset[$name] + 1))
  else
    offset[$name]=$((offset[$name] - 1))
    [ "${offset[$name]}" -ge 0 ] || offset[$name]=0
  fi
}

# page down|up — one body-height step; scroll_top/scroll_bottom jump to the
# ends (paint() clamps offsets to their pane's bounds).
page() {
  local dir="$1" name="${panes[$active]}" rows cols
  read -r rows cols < <(stty size </dev/tty)
  local step=$((rows - 2))
  [ "$step" -gt 0 ] || step=1
  if [ "$name" = settings ]; then
    [ "$settings_nrows" -gt 0 ] || return 0
    if [ "$dir" = down ]; then
      cursor_settings=$((cursor_settings + step))
      [ "$cursor_settings" -lt "$settings_nrows" ] || cursor_settings=$((settings_nrows - 1))
    else
      cursor_settings=$((cursor_settings - step))
      [ "$cursor_settings" -ge 0 ] || cursor_settings=0
    fi
  elif [ "$dir" = down ]; then
    offset[$name]=$((offset[$name] + step))
  else
    offset[$name]=$((offset[$name] - step))
    [ "${offset[$name]}" -ge 0 ] || offset[$name]=0
  fi
}

scroll_top() {
  local name="${panes[$active]}"
  if [ "$name" = settings ]; then
    cursor_settings=0
  else
    offset[$name]=0
  fi
}

scroll_bottom() {
  local name="${panes[$active]}"
  if [ "$name" = settings ]; then
    [ "$settings_nrows" -gt 0 ] && cursor_settings=$((settings_nrows - 1))
  else
    offset[$name]=999999
  fi
}

# paint — build one frame (tab bar, active pane body, status line) and write
# it in a single printf. Truncation happens before any SGR is added.
paint() {
  local rows cols
  read -r rows cols < <(stty size </dev/tty)

  local frame=$'\e[H'
  if [ "$rows" -lt 10 ] || [ "$cols" -lt 40 ]; then
    # Erase the whole screen before writing: erasing *after* the text would
    # erase the text itself when it reaches the bottom-right cell (autowrap
    # is off, so the cursor stays pinned there instead of advancing).
    trunc "$cols" "terminal too small"
    frame+=$'\e[2J\e[1;1H'"$TRUNC_RESULT"
    printf '%s' "$frame" >/dev/tty
    return
  fi

  local body=$((rows - 2))
  local name="${panes[$active]}"
  # shellcheck disable=SC2178 # nameref onto an array, not a string
  local -n _texts="pane_text_$name"
  # shellcheck disable=SC2178
  local -n _styles="pane_style_$name"
  # shellcheck disable=SC2178
  local -n _rows="pane_row_$name"
  local n=${#_texts[@]}

  if [ "$name" = settings ]; then
    local idx=-1 j
    for ((j = 0; j < ${#_rows[@]}; j++)); do
      if [ "${_rows[$j]}" = "$cursor_settings" ]; then
        idx=$j
        break
      fi
    done
    if [ "$idx" -ge 0 ]; then
      [ "$idx" -ge "${offset[settings]}" ] || offset[settings]=$idx
      [ "$idx" -lt "$((offset[settings] + body))" ] || offset[settings]=$((idx - body + 1))
    fi
  fi
  local maxoff=$((n - body))
  [ "$maxoff" -ge 0 ] || maxoff=0
  [ "${offset[$name]}" -le "$maxoff" ] || offset[$name]=$maxoff
  [ "${offset[$name]}" -ge 0 ] || offset[$name]=0
  local off=${offset[$name]}

  # row 1: tab bar
  local i label tabtext=""
  for i in 0 1 2 3; do
    label=" $((i + 1)) ${pane_titles[$i]} "
    if [ "$i" -eq "$active" ] && [ "$color_on" != true ]; then
      label="[$label]"
    fi
    tabtext+="$label"
  done
  trunc "$cols" "$tabtext"
  local kept="$TRUNC_RESULT"
  local tabline
  if [ "$color_on" = true ]; then
    local has_ellipsis=0 prefix_len=${#kept}
    case "$kept" in *…) has_ellipsis=1 ;; esac
    [ "$has_ellipsis" -eq 0 ] || prefix_len=$((prefix_len - 1))
    tabline=""
    local pos=0 seglen segstart take piece
    for i in 0 1 2 3; do
      label=" $((i + 1)) ${pane_titles[$i]} "
      seglen=${#label}
      segstart=$pos
      pos=$((pos + seglen))
      [ "$segstart" -lt "$prefix_len" ] || continue
      take=$seglen
      [ "$((segstart + seglen))" -le "$prefix_len" ] || take=$((prefix_len - segstart))
      piece="${label:0:take}"
      if [ "$i" -eq "$active" ]; then
        tabline+=$'\e[7m'"$piece"$'\e[27m'
      else
        tabline+="$piece"
      fi
    done
    [ "$has_ellipsis" -eq 0 ] || tabline+="…"
  else
    tabline="$kept"
  fi
  frame+=$'\e[1;1H\e[K'"$tabline"

  local r line_idx text style is_cursor sgr text_out
  for ((r = 2; r < rows; r++)); do
    line_idx=$((off + r - 2))
    if [ "$line_idx" -lt "$n" ]; then
      text="${_texts[$line_idx]}"
      style="${_styles[$line_idx]}"
      is_cursor=0
      if [ "$name" = settings ] && [ "${_rows[$line_idx]}" = "$cursor_settings" ] && [ "${_rows[$line_idx]}" != -1 ]; then
        is_cursor=1
      fi
      trunc "$cols" "$text"
      text_out="$TRUNC_RESULT"
      if [ "$is_cursor" -eq 1 ] && [ "$color_on" != true ]; then
        text_out=">${text_out:1}"
      fi
      sgr=""
      case "$style" in
      h) sgr=1 ;;
      l) sgr="1;33" ;;
      esac
      if [ "$is_cursor" -eq 1 ]; then
        if [ -n "$sgr" ]; then sgr="$sgr;7"; else sgr=7; fi
      fi
      if [ -n "$sgr" ] && [ "$color_on" = true ]; then
        text_out=$'\e['"$sgr"'m'"$text_out"$'\e[0m'
      fi
    else
      text_out=""
    fi
    frame+=$'\e['"$r"$';1H\e[K'"$text_out"
  done

  # Every row 1..rows was just erased and rewritten above (including this
  # last one), so nothing needs a trailing \e[J: issuing it here would erase
  # the character just written to the bottom-right cell (same pinned-cursor
  # hazard as above).
  trunc "$cols" " r refresh · tab/1-4 pane · j/k move · q quit · refreshed $now_clock"
  frame+=$'\e['"$rows"$';1H\e[K'"$TRUNC_RESULT"
  printf '%s' "$frame" >/dev/tty
}

case "$mode" in
once)
  jq -r "$RENDER_DEFS"'
    once_lines(.)[] | [.style, (.row|tostring), (.text | gsub("[\t\n]"; " "))] | join("\t")
  ' <<<"$model" | while IFS=$'\t' read -r style _row text; do
    case "$style" in
    h)
      if [ "$color_on" = true ]; then printf '\e[1m%s\e[0m\n' "$text"; else printf '%s\n' "$text"; fi
      ;;
    l)
      if [ "$color_on" = true ]; then printf '\e[1;33m%s\e[0m\n' "$text"; else printf '%s\n' "$text"; fi
      ;;
    *)
      printf '%s\n' "$text"
      ;;
    esac
  done
  ;;
interactive)
  saved_stty=$(stty -g </dev/tty)
  tui_cleanup() {
    printf '\e[?7h\e[?25h\e[?1049l' >/dev/tty
    stty "$saved_stty" </dev/tty
    rm -rf "$tmp_dir"
  }
  trap tui_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 0' TERM
  trap 'resized=1' WINCH

  stty -echo -icanon min 1 time 0 </dev/tty
  printf '\e[?1049h\e[?25l\e[?7l' >/dev/tty

  render_panes
  paint

  while true; do
    if IFS= read -rsn1 -t 1 key </dev/tty; then
      case "$key" in
      q) exit 0 ;;
      r)
        collect
        render_panes
        paint
        ;;
      1 | 2 | 3 | 4)
        active=$((key - 1))
        paint
        ;;
      $'\t')
        active=$(((active + 1) % 4))
        paint
        ;;
      j)
        move_cursor down
        paint
        ;;
      k)
        move_cursor up
        paint
        ;;
      g)
        scroll_top
        paint
        ;;
      G)
        scroll_bottom
        paint
        ;;
      $'\e')
        tail=""
        if IFS= read -rsn2 -t 0.05 tail </dev/tty; then
          case "$tail" in
          '[C')
            active=$(((active + 1) % 4))
            paint
            ;;
          '[D')
            active=$(((active + 3) % 4))
            paint
            ;;
          '[A')
            move_cursor up
            paint
            ;;
          '[B')
            move_cursor down
            paint
            ;;
          '[5')
            IFS= read -rsn1 -t 0.05 _tilde </dev/tty || true
            page up
            paint
            ;;
          '[6')
            IFS= read -rsn1 -t 0.05 _tilde </dev/tty || true
            page down
            paint
            ;;
          esac
        fi
        ;;
      esac
    elif [ "$resized" -eq 1 ]; then
      resized=0
      paint
    fi
  done
  ;;
esac
