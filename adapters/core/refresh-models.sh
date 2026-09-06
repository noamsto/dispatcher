#!/usr/bin/env bash
# refresh-models — cache the cursor-agent CLI's dispatchable model slugs for
# dispatcher tier<->model judging.
#
# Writes ${XDG_DATA_HOME:-~/.local/share}/crew/cursor-models-cache.json. The
# dispatcher reads it when judging which cursor-agent model to dispatch to
# (see DISPATCHER_PROTOCOL.md); refresh by hand or cron — no daemon.
# `cursor-agent --list-models` is the only source (no JSON mode): it prints a
# header line, a blank line, then one `<slug> - <description>` line per
# model, plus a pseudo-entry `auto - Auto (default)` that is not a real
# dispatchable slug and a trailing `Tip: ...` line — none of those contain
# the ` - ` separator a real model line does, so filtering on it discards
# all three without special-casing each one.
set -euo pipefail

OUT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
OUT="$OUT_DIR/cursor-models-cache.json"

warn() { printf 'refresh-models: %s\n' "$*" >&2; }

main() {
  if ! command -v cursor-agent >/dev/null 2>&1; then
    warn "cursor-agent not found on PATH; keeping any existing cache"
    exit 1
  fi

  local raw
  if ! raw=$(cursor-agent --list-models 2>/dev/null); then
    warn "cursor-agent --list-models failed; keeping any existing cache"
    exit 1
  fi

  local slugs
  slugs=$(grep -F ' - ' <<<"$raw" | grep -v '^auto - ' | sed 's/ - .*//') || true
  if [[ -z $slugs ]]; then
    warn "cursor-agent --list-models produced no usable model lines; keeping any existing cache"
    exit 1
  fi

  local models
  models=$(jq -R -s 'split("\n") | map(select(length > 0)) | map({slug: .})' <<<"$slugs")

  mkdir -p "$OUT_DIR"
  local tmp="$OUT.tmp.$$"
  jq -n \
    --arg fetched_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson fetched_epoch "$(date +%s)" \
    --argjson models "$models" \
    '{
       fetched_at: $fetched_at,
       fetched_epoch: $fetched_epoch,
       models: $models
     }' >"$tmp"
  mv "$tmp" "$OUT"

  printf '%s\n' "$OUT"
  jq -r '"\(.models | length) models cached"' "$OUT"
}

main "$@"
