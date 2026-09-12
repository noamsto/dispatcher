#!/usr/bin/env bash
# refresh-scores — cache external LLM leaderboard standings for dispatcher
# model judging.
#
# Writes ${XDG_DATA_HOME:-~/.local/share}/crew/model-scores.json. The
# dispatcher LLM reads it at judge time (see DISPATCHER_PROTOCOL.md); refresh
# by hand or cron — no daemon. Primary source is LMArena's official Hugging
# Face dataset via the datasets-server JSON API (no auth); OpenRouter's
# benchmarks API (Artificial Analysis indices) is layered on when
# OPENROUTER_API_KEY is set, skipped otherwise.
set -euo pipefail

HF_BASE="https://datasets-server.huggingface.co/filter"
DATASET="lmarena-ai%2Fleaderboard-dataset"
OR_BASE="https://openrouter.ai/api/v1/benchmarks"
TOP_N=40
PAGE=100

OUT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
OUT="$OUT_DIR/model-scores.json"

warn() { printf 'refresh-scores: %s\n' "$*" >&2; }

# fetch_board <config> <category> — print {"published","models"} with the top
# $TOP_N rows by rating; return 1 when the board can't be fetched at all.
fetch_board() {
  local config=$1 category=$2
  local offset=0 total=1 rows acc='[]' resp
  while ((offset < total)); do
    if ! resp=$(curl -sf --max-time 30 "$HF_BASE?dataset=$DATASET&config=$config&split=latest&where=%22category%22%3D%27$category%27&offset=$offset&length=$PAGE"); then
      return 1
    fi
    total=$(jq '.num_rows_total // 0' <<<"$resp")
    rows=$(jq '.rows // [] | length' <<<"$resp")
    ((rows == 0)) && break
    acc=$(jq --argjson acc "$acc" \
      '$acc + [.rows[].row | {model: .model_name, org: .organization, rating: .rating, rank: .rank, votes: .vote_count, date: .leaderboard_publish_date}]' \
      <<<"$resp")
    offset=$((offset + PAGE))
  done
  # Agent Arena publishes rank only (rating is null), so the sort key falls
  # back to rank: rating boards sort rating-desc, rank-only boards rank-asc.
  jq --argjson n "$TOP_N" \
    '{
       published: ([.[].date] | max),
       models: (sort_by(-(.rating // (-.rank))) | .[:$n]
                | [.[] | del(.date) | .rating |= (if . then floor else null end)])
     }' <<<"$acc"
}

# fetch_openrouter — Artificial Analysis indices via OpenRouter; needs
# $OPENROUTER_API_KEY. Prints {"as_of","models"} sorted by agentic index.
fetch_openrouter() {
  local resp
  if ! resp=$(curl -sf --max-time 30 \
    -H "Authorization: Bearer $OPENROUTER_API_KEY" \
    "$OR_BASE?source=artificial-analysis&max_results=100"); then
    return 1
  fi
  jq --argjson n "$TOP_N" \
    '{
       as_of: .meta.as_of,
       models: ([.data[] | {model: .model_permaslug, agentic: .agentic_index, coding: .coding_index, intelligence: .intelligence_index}]
                | sort_by(-.agentic) | .[:$n])
     }' <<<"$resp"
}

main() {
  local boards='{}' board openrouter='null' fetched=0
  local -a specs=(
    "arena_text_overall text overall"
    "arena_text_coding text coding"
    "arena_webdev webdev overall"
    "arena_agent agent overall"
  )
  local spec name config category
  for spec in "${specs[@]}"; do
    read -r name config category <<<"$spec"
    if board=$(fetch_board "$config" "$category"); then
      boards=$(jq --argjson b "$board" --arg k "$name" '.[$k] = $b' <<<"$boards")
      fetched=$((fetched + 1))
    else
      warn "skipping $name (fetch failed)"
    fi
  done
  if ((fetched == 0)); then
    warn "all arena boards failed; keeping any existing cache"
    exit 1
  fi

  if [[ -n ${OPENROUTER_API_KEY:-} ]]; then
    if openrouter=$(fetch_openrouter); then
      :
    else
      warn "openrouter fetch failed; omitting"
      openrouter='null'
    fi
  else
    warn "OPENROUTER_API_KEY unset; skipping openrouter benchmarks"
  fi

  mkdir -p "$OUT_DIR"
  local tmp="$OUT.tmp.$$"
  jq -n \
    --arg fetched_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson boards "$boards" \
    --argjson openrouter "$openrouter" \
    '{
       fetched_at: $fetched_at,
       source: "https://huggingface.co/datasets/lmarena-ai/leaderboard-dataset",
       boards: $boards
     } + (if $openrouter then {openrouter: $openrouter} else {} end)' >"$tmp"
  mv "$tmp" "$OUT"

  printf '%s\n' "$OUT"
  jq -r '.boards | to_entries[] | "\(.key): \(.value.models | length) models (published \(.value.published))"' "$OUT"
}

main "$@"
