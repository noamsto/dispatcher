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

check=false
if [ "${1:-}" = --check ]; then
  check=true
  shift
fi

defaults="${1:-$root/adapters/core/defaults.json}"
doc="${2:-$root/adapters/core/protocols/dispatch-orchestration.md}"

# render_tier_rows <defaults.json> — the "| engine | tier | typical launch
# model | the gate accepts |" table, one row per engine x tier in data order.
render_tier_rows() {
  printf '| engine | tier | typical launch model | the gate accepts |\n'
  printf '| --- | --- | --- | --- |\n'
  jq -r '
    .modelMap
    | to_entries[]
    | .key as $engine
    | .value
    | to_entries[]
    | "| \($engine) | `\(.key)` | `\(.value.default)` | \(.value.expected | split("*") | join("\\*")) |"
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
# printed to stdout.
splice() {
  local name="$1" content="$2" in="$3"
  local begin="<!-- BEGIN generated:${name} from adapters/core/defaults.json by scripts/gen-model-map-doc.sh -->"
  local end="<!-- END generated:${name} -->"
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
  tier_rows="$(mktemp)"
  pace_downgrades="$(mktemp)"
  tmp1="$(mktemp)"
  render_tier_rows "$defaults" >"$tier_rows"
  render_pace_downgrades "$defaults" >"$pace_downgrades"
  splice tier-rows "$tier_rows" "$in" >"$tmp1"
  splice pace-downgrades "$pace_downgrades" "$tmp1"
  rm -f "$tier_rows" "$pace_downgrades" "$tmp1"
}

# check_model_map_table <defaults.json> <doc> — the hand-written table under
# "## Model map": for its `deep`/`standard`/`trivial` rows, the first
# `**…**` token of each engine column (backticks stripped) must equal that
# engine/tier's `default` in defaults.json. Prints every mismatch; returns
# non-zero if any were found.
check_model_map_table() {
  local defaults="$1" doc="$2" lut rc=0
  lut="$(mktemp)"
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
          if (tok != want) {
            printf "Model map table mismatch: %s/%s: doc has %s, defaults.json default is %s\n", eng, tier, tok, want
            mism = 1
          }
        }
      }
    }
    END { if (mism) exit 1 }
  ' "$lut" "$doc" || rc=$?
  rm -f "$lut"
  return "$rc"
}

if [ "$check" = true ]; then
  fail=0
  expected="$(mktemp)"
  render_doc "$defaults" "$doc" >"$expected"
  if ! diff -u "$doc" "$expected"; then
    fail=1
  fi
  check_model_map_table "$defaults" "$doc" || fail=1
  rm -f "$expected"
  exit "$fail"
fi

tmp="$(mktemp "${doc}.XXXXXX")"
render_doc "$defaults" "$doc" >"$tmp"
chmod --reference="$doc" "$tmp"
mv "$tmp" "$doc"
