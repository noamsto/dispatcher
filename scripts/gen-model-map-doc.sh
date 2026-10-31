#!/usr/bin/env bash
# gen-model-map-doc.sh — render, or check, the generated regions of
# adapters/core/protocols/dispatch-orchestration.md from
# adapters/core/defaults.json (#560): the Model map's class table (#1391) and
# the Tier map's pace-downgrade table are pure data, so they are generated
# rather than hand-copied. `--check` also proves the hand-written Model map
# table (which carries execute/escalate ladders defaults.json does not hold,
# in class names) has not drifted from each tier row's
# `classes.<engine>.tiers.<tier>.class`.
#
# Usage: gen-model-map-doc.sh [--check] [<defaults.json> <doc.md>]
# With no paths, both default to this repo's own copies, resolved relative to
# this script so it works from any cwd.
#
# Regions are delimited by
#   <!-- BEGIN generated:<name> from adapters/core/defaults.json by scripts/gen-model-map-doc.sh -->
#   <!-- END generated:<name> -->
# for names `model-classes` and `pace-downgrades`; the markers themselves must
# already exist in the doc (this script fills what is between them, it does
# not place them).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

work="$(mktemp -d)"
tmp=""
trap 'rm -rf "$work"; [ -z "$tmp" ] || rm -f "$tmp"' EXIT

check=false
if [ "${1:-}" = --check ]; then
  check=true
  shift
fi

defaults="${1:-$root/adapters/core/defaults.json}"
doc="${2:-$root/adapters/core/protocols/dispatch-orchestration.md}"

# render_model_classes <defaults.json> — one row per engine x class (cheap,
# standard, premium) that has patterns or a tier row: the tiers whose default
# comes from the class, the tiers that admit it, its patterns (a gated pattern
# names its requirement) and its seed default, the modelMap default of its first
# tier row in trivial, standard, deep order (what a @class sigil names with no
# class-state file).
render_model_classes() {
  printf '| engine | class | default on | admitted on | patterns | seed default |\n'
  printf '| --- | --- | --- | --- | --- | --- |\n'
  jq -r '
    def code: "`" + (split("\\[") | join("[")) + "`";
    def list: if length == 0 then "—" else map(code) | join(", ") end;
    . as $root
    | ($root.modelMap | keys_unsorted[]) as $e
    | ($root.classes[$e] // {}) as $cl
    | ["trivial", "standard", "deep"] as $order
    | ("cheap", "standard", "premium") as $c
    | [$order[] | select($cl.tiers[.].class == $c)] as $defaults
    | [$order[] | select(($cl.tiers[.] // null) as $r | $r != null and (($r.admits // [$r.class]) | index($c)) != null)] as $admitted
    | ($cl[$c] // []) as $pats
    | select(($pats | length) > 0 or ($defaults | length) > 0)
    | ($pats | map(if type == "string" then code else (.glob | code) + " (needs " + .requires + ")" end)
        | if length == 0 then "—" else join(", ") end) as $patterns
    | (first($defaults[] | $root.modelMap[$e][.].default // empty) // null) as $seed
    | "| \($e) | `@\($c)` | \($defaults | list) | \($admitted | list) | \($patterns) | \(if $seed == null then "—" else ($seed | code) end) |"
  ' "$1"
}

# render_pace_downgrades <defaults.json> — one row per paceDowngrades entry;
# an engine with none is refused on effort only, so it gets a fixed row.
render_pace_downgrades() {
  printf '| engine | premium | downgrade target |\n'
  printf '| --- | --- | --- |\n'
  jq -r '
    . as $root
    | ($root.modelMap | keys_unsorted[]) as $engine
    | ($root.paceDowngrades[$engine] // null) as $entries
    | if $entries == null then
        "| \($engine) | — (effort only: `max`/`xhigh`→`high`) | — |"
      else
        ($entries[]
          | "| \($engine) | "
            + ([.models[] | "`" + (split("\\[") | join("[")) + "`"] | join(", "))
            + " | `" + .to + "` |")
      end
  ' "$1"
}

# splice <name> <content-file> <in-file> — everything between the BEGIN/END
# markers for <name> in <in-file> is replaced by <content-file>'s lines,
# printed to stdout. Fails when <in-file> does not have exactly one BEGIN and
# one END marker for <name>, in that order.
splice() {
  local name="$1" content="$2" in="$3"
  local begin="<!-- BEGIN generated:${name} from adapters/core/defaults.json by scripts/gen-model-map-doc.sh -->"
  local end="<!-- END generated:${name} -->"
  local begin_n end_n begin_ln end_ln
  begin_n="$(grep -cxF -- "$begin" "$in" || true)"
  end_n="$(grep -cxF -- "$end" "$in" || true)"
  if [ "$begin_n" -ne 1 ] || [ "$end_n" -ne 1 ]; then
    echo "gen-model-map-doc: region '$name' in $in must have exactly one BEGIN and one END marker (found $begin_n BEGIN, $end_n END)" >&2
    return 1
  fi
  begin_ln="$(grep -nxF -- "$begin" "$in" | cut -d: -f1)"
  end_ln="$(grep -nxF -- "$end" "$in" | cut -d: -f1)"
  if [ "$begin_ln" -ge "$end_ln" ]; then
    echo "gen-model-map-doc: region '$name' in $in has its END marker before its BEGIN marker" >&2
    return 1
  fi
  awk -v begin="$begin" -v end="$end" -v content="$content" '
    $0 == begin { print; print ""; while ((getline line < content) > 0) print line; print ""; skip = 1; next }
    $0 == end   { skip = 0 }
    skip        { next }
                { print }
  ' "$in"
}

# render_doc <defaults.json> <in-doc> — <in-doc> with every generated region
# freshly rendered from <defaults.json>, on stdout.
render_doc() {
  local defaults="$1" in="$2" model_classes pace_downgrades tmp1
  model_classes="$work/model_classes"
  pace_downgrades="$work/pace_downgrades"
  tmp1="$work/tmp1"
  render_model_classes "$defaults" >"$model_classes"
  render_pace_downgrades "$defaults" >"$pace_downgrades"
  splice model-classes "$model_classes" "$in" >"$tmp1"
  splice pace-downgrades "$pace_downgrades" "$tmp1"
}

# check_model_map_table <defaults.json> <doc> — the hand-written table under
# "## Model map": for its `deep`/`standard`/`trivial` rows, the first
# `**…**` token of each engine column (backticks stripped) must be `@` plus
# that engine/tier's `classes.<engine>.tiers.<tier>.class` in defaults.json,
# the class its default comes from. Prints every mismatch; also fails,
# naming the problem, when the "## Model map" heading or its `Tier` table
# header is not found, or when zero cells were compared.
check_model_map_table() {
  local defaults="$1" doc="$2" lut rc=0
  lut="$work/lut"
  jq -r '
    . as $root
    | .modelMap
    | to_entries[] as $e
    | $e.value
    | keys_unsorted[]
    | [$e.key, ., "@" + ($root.classes[$e.key].tiers[.].class // "?")] | @tsv
  ' "$defaults" >"$lut"
  awk '
    FNR == NR { lut[$1, $2] = $3; next }
    done      { next }
    !in_section {
      if ($0 ~ /^## Model map/) in_section = 1
      next
    }
    !found_header {
      if ($0 ~ /^\|/) {
        n = split($0, cells, "|")
        c2 = cells[2]
        gsub(/^[ \t]+|[ \t]+$/, "", c2)
        if (c2 == "Tier") {
          found_header = 1
          ncols = n
          for (j = 3; j <= n; j++) {
            cell = cells[j]
            gsub(/^[ \t]+|[ \t]+$/, "", cell)
            split(cell, w, /[ \t]/)
            colengine[j] = w[1]
          }
        }
      }
      next
    }
    !seen_sep { seen_sep = 1; next }
    $0 !~ /^\|/ { done = 1; next }
    {
      n = split($0, cells, "|")
      tier = cells[2]
      gsub(/^[ \t]+|[ \t]+$/, "", tier)
      gsub(/`/, "", tier)
      if (tier != "deep" && tier != "standard" && tier != "trivial") next
      for (j = 3; j <= ncols; j++) {
        cell = cells[j]
        if (match(cell, /\*\*[^*]+\*\*/)) {
          tok = substr(cell, RSTART + 2, RLENGTH - 4)
          gsub(/`/, "", tok)
          gsub(/^[ \t]+|[ \t]+$/, "", tok)
          eng = colengine[j]
          want = lut[eng, tier]
          compared++
          if (tok != want) {
            printf "Model map table mismatch: %s/%s: doc has %s, defaults.json class is %s\n", eng, tier, tok, want
            mism = 1
          }
        }
      }
    }
    END {
      if (!in_section) {
        print "check_model_map_table: \"## Model map\" heading not found in " ARGV[2] > "/dev/stderr"
        exit 1
      }
      if (!found_header) {
        print "check_model_map_table: Tier table header not found under \"## Model map\" in " ARGV[2] > "/dev/stderr"
        exit 1
      }
      if (compared == 0) {
        print "check_model_map_table: no cells were compared in " ARGV[2] > "/dev/stderr"
        exit 1
      }
      if (mism) exit 1
    }
  ' "$lut" "$doc" || rc=$?
  return "$rc"
}

# check_default_membership <defaults.json> — for every modelMap row, print a
# violation when its `default` is not exactly one of its own `models`.
check_default_membership() {
  jq -r '
    .modelMap
    | to_entries[] as $e
    | ($e.value | to_entries[]) as $t
    | $t.value as $row
    | select(($row.models // []) | index($row.default) == null)
    | "Model map default membership: \($e.key)/\($t.key): default \($row.default) is not in its models"
  ' "$1"
}

if [ "$check" = true ]; then
  fail=0
  expected="$work/expected"
  render_doc "$defaults" "$doc" >"$expected"
  if ! diff -u "$doc" "$expected"; then
    fail=1
  fi
  check_model_map_table "$defaults" "$doc" || fail=1
  membership_violations="$(check_default_membership "$defaults")"
  if [ -n "$membership_violations" ]; then
    printf '%s\n' "$membership_violations" >&2
    fail=1
  fi
  exit "$fail"
fi

tmp="$(mktemp "${doc}.XXXXXX")"
render_doc "$defaults" "$doc" >"$tmp"
cat "$tmp" >"$doc"
