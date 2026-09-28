#!/usr/bin/env bash
# gen-model-map-doc.sh — render, or check, the generated regions of
# adapters/core/protocols/dispatch-orchestration.md from
# adapters/core/defaults.json (#560): the Tier map's tier-rows table and its
# pace-downgrade table are pure data, so they are generated rather than
# hand-copied. `--check` also proves the hand-written Model map table (which
# carries execute/escalate ladders defaults.json does not hold) has not
# drifted from the map's `default` model per tier.
#
# Usage: gen-model-map-doc.sh [--check] [<defaults.json> <doc.md>]
# With no paths, both default to this repo's own copies, resolved relative to
# this script so it works from any cwd.
#
# Regions are delimited by
#   <!-- BEGIN generated:<name> from adapters/core/defaults.json by scripts/gen-model-map-doc.sh -->
#   <!-- END generated:<name> -->
# for names `tier-rows` and `pace-downgrades`; the markers themselves must
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

# render_tier_rows <defaults.json> — the "| engine | tier | typical launch
# model | the gate admits |" table, one row per engine x tier in data order;
# the last column is the row's `models` (globs backticked, the literal-bracket
# escape `\[` shown as `[`), plus a note when the row also admits ids by
# `regex`.
render_tier_rows() {
  printf '| engine | tier | typical launch model | the gate admits |\n'
  printf '| --- | --- | --- | --- |\n'
  jq -r '
    .modelMap
    | to_entries[]
    | .key as $engine
    | .value
    | to_entries[]
    | .key as $tier
    | .value as $row
    | ([$row.models[] | "`" + (split("\\[") | join("[")) + "`"] | join(", ")) as $models
    | ($models + (if ($row.regex // [] | length) > 0 then ", plus ids matching the row'\''s regex" else "" end)) as $admits
    | "| \($engine) | `\($tier)` | `\($row.default)` | \($admits) |"
  ' "$1"
}

# render_pace_downgrades <defaults.json> — the "| engine | premium |
# downgrade target |" table, one row per paceDowngrades entry (globs
# backticked, the literal-bracket escape `\[` shown as `[`); an engine in
# modelMap with no paceDowngrades entry (pi) gets a fixed effort-only row.
render_pace_downgrades() {
  printf '| engine | premium | downgrade target |\n'
  printf '| --- | --- | --- |\n'
  jq -r '
    . as $root
    | ($root.modelMap | keys_unsorted[]) as $engine
    | ($root.paceDowngrades[$engine] // null) as $entries
    | if $entries == null then
        "| \($engine) | — (effort only: `max`→`xhigh`→`high`) | — |"
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

# render_doc <defaults.json> <in-doc> — <in-doc> with both generated regions
# freshly rendered from <defaults.json>, on stdout.
render_doc() {
  local defaults="$1" in="$2" tier_rows pace_downgrades tmp1
  tier_rows="$work/tier_rows"
  pace_downgrades="$work/pace_downgrades"
  tmp1="$work/tmp1"
  render_tier_rows "$defaults" >"$tier_rows"
  render_pace_downgrades "$defaults" >"$pace_downgrades"
  splice tier-rows "$tier_rows" "$in" >"$tmp1"
  splice pace-downgrades "$pace_downgrades" "$tmp1"
}

# check_model_map_table <defaults.json> <doc> — the hand-written table under
# "## Model map": for its `deep`/`standard`/`trivial` rows, the first
# `**…**` token of each engine column (backticks stripped) must equal that
# engine/tier's `default` in defaults.json. Prints every mismatch; also fails,
# naming the problem, when the "## Model map" heading or its `Tier` table
# header is not found, or when zero cells were compared.
check_model_map_table() {
  local defaults="$1" doc="$2" lut rc=0
  lut="$work/lut"
  jq -r '
    .modelMap
    | to_entries[] as $e
    | $e.value
    | to_entries[]
    | [$e.key, .key, .value.default] | @tsv
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
            printf "Model map table mismatch: %s/%s: doc has %s, defaults.json default is %s\n", eng, tier, tok, want
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
