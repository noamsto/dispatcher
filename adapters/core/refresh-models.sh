#!/usr/bin/env bash
# refresh-models — cache each engine's dispatchable model IDs for dispatcher
# tier<->model judging.
#
# Writes ${XDG_DATA_HOME:-~/.local/share}/crew/cursor-models-cache.json (the
# dispatcher reads it when judging which cursor-agent model to dispatch to, see
# DISPATCHER_PROTOCOL.md) and model-catalog.json, the per-engine catalog
# (docs/model-classes.md §5) that dispatch's tier
# gate reads through model-resolve. Refresh by hand or cron —
# no daemon. An engine whose probe fails keeps its previous catalog snapshot.
#
# `cursor-agent --list-models` is the only cursor source (no JSON mode): it
# prints a header line, a blank line, then one `<slug> - <description>` line per
# model, plus a pseudo-entry `auto - Auto (default)` that is not a real
# dispatchable slug and a trailing `Tip: ...` line — none of those contain
# the ` - ` separator a real model line does, so filtering on it discards
# all three without special-casing each one.
# The in-session Task tool's subagent roster is narrower and is not probed
# here (dispatch-orchestration.md -> "Cursor Task-spawn slugs").
set -euo pipefail

OUT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
OUT="$OUT_DIR/cursor-models-cache.json"
CATALOG="$OUT_DIR/model-catalog.json"
OPENROUTER_URL="https://openrouter.ai/api/v1/models"
MAX_MODELS=2000

warn() { printf 'refresh-models: %s\n' "$*" >&2; }

# Catalog text is untrusted: an ID holding a control byte or outside the
# conservative shape is dropped, and the list is capped.
# stdin: JSON array of {id, created?, price?}; stdout: the sanitised array.
sanitize_models() {
  jq -c --argjson max "$MAX_MODELS" '
    map(select(
      (.id | type == "string")
      and (.id | test("[[:cntrl:]]") | not)
      and (.id | test("^[A-Za-z0-9._:/@-]{1,128}$"))
    ))
    | unique_by(.id)
    | .[:$max]'
}

# stdin: newline-separated IDs.
ids_to_models() {
  jq -R -s -c 'split("\n") | map(select(length > 0)) | map({id: .})'
}

# Each *_models function sets MODELS (JSON array) and SOURCE and returns 0, or
# warns and returns 1 so the engine keeps its previous snapshot.
cursor_models() {
  local raw
  if ! command -v cursor-agent >/dev/null 2>&1; then
    warn "cursor-agent not found on PATH; keeping any existing cache"
    return 1
  fi
  if ! raw=$(cursor-agent --list-models 2>/dev/null); then
    warn "cursor-agent --list-models failed; keeping any existing cache"
    return 1
  fi

  local slugs
  slugs=$(grep -F ' - ' <<<"$raw" | grep -v '^auto - ' | sed 's/ - .*//') || true
  if [[ -z $slugs ]]; then
    warn "cursor-agent --list-models produced no usable model lines; keeping any existing cache"
    return 1
  fi

  local models
  models=$(jq -R -s 'split("\n") | map(select(length > 0)) | map({slug: .})' <<<"$slugs")

  local tmp="$OUT.tmp.$$"
  jq -n \
    --arg fetched_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson fetched_epoch "$(date +%s)" \
    --argjson models "$models" \
    '{
       fetched_at: $fetched_at,
       fetched_epoch: $fetched_epoch,
       models: $models
     }' >"$tmp" || {
    rm -f "$tmp"
    warn "cursor cache write failed; keeping any existing cache"
    return 1
  }
  mv "$tmp" "$OUT"

  printf '%s\n' "$OUT"
  jq -r '"\(.models | length) models cached"' "$OUT"

  SOURCE="cursor-agent --list-models"
  MODELS=$(ids_to_models <<<"$slugs")
}

# Claude Code has no model-list command (2.1.310), so the aliases are recorded;
# a models subcommand is used only when `claude --help` advertises one.
claude_models() {
  command -v claude >/dev/null 2>&1 || {
    warn "claude not found on PATH; keeping any existing snapshot"
    return 1
  }
  SOURCE="aliases"
  MODELS=$(printf '%s\n' haiku sonnet opus fable | ids_to_models)

  local sub listed
  sub=$(claude --help 2>/dev/null | sed -nE 's/^[[:space:]]+(models|list-models)([[:space:]].*)?$/\1/p' | head -n1) || true
  [[ -n $sub ]] || return 0
  if listed=$(claude "$sub" 2>/dev/null | sed -nE 's/^[[:space:]]*([^[:space:]]+).*/\1/p' | ids_to_models) &&
    [[ $(jq length <<<"$listed") -gt 0 ]]; then
    SOURCE="claude $sub"
    MODELS=$listed
  fi
}

codex_models() {
  local cache="$HOME/.codex/models_cache.json"
  command -v codex >/dev/null 2>&1 || {
    warn "codex not found on PATH; keeping any existing snapshot"
    return 1
  }
  if ! MODELS=$(jq -c '[.models[]?|.slug?|strings|{id: .}]' "$cache" 2>/dev/null); then
    warn "$cache missing or unreadable; keeping any existing snapshot"
    return 1
  fi
  SOURCE="codex models_cache.json"
}

pi_models() {
  command -v pi >/dev/null 2>&1 || {
    warn "pi not found on PATH; keeping any existing snapshot"
    return 1
  }
  local raw
  if ! raw=$(pi --list-models 2>/dev/null); then
    warn "pi --list-models failed; keeping any existing snapshot"
    return 1
  fi
  # Table rows: `<provider> <model> <context> ...`; the header row starts `provider`.
  MODELS=$(sed -nE '/^provider[[:space:]]/d; s|^([^[:space:]]+)[[:space:]]+([^[:space:]]+).*|\1/\2|p' <<<"$raw" | ids_to_models)
  SOURCE="pi --list-models"

  # OpenRouter's public endpoint adds `created` and per-token price to the
  # openrouter/* IDs; unreachable, the IDs stand without them.
  command -v curl >/dev/null 2>&1 || return 0
  local orouter joined
  orouter=$(curl -fsS --max-time 20 "$OPENROUTER_URL" 2>/dev/null) || {
    warn "openrouter models fetch failed; pi snapshot has no created/price"
    return 0
  }
  # The response is far past the 128 KiB single-argument limit, so it goes in
  # as a file rather than --argjson.
  joined=$(jq -c --slurpfile or <(printf '%s' "$orouter") '
    ($or[0].data // [] | map({key: .id, value: .}) | from_entries) as $by
    | map(
        . as $m
        | ($by[($m.id | ltrimstr("openrouter/"))] // null) as $o
        | if ($m.id | startswith("openrouter/")) and $o != null then
            $m
            + (if ($o.created | type) == "number" then {created: $o.created} else {} end)
            + (
                ([$o.pricing.prompt, $o.pricing.completion] | map(tonumber? // null)) as $p
                | if $p[0] != null and $p[1] != null and $p[0] >= 0 and $p[1] >= 0 then {price: {in: $p[0], out: $p[1]}} else {} end
              )
          else $m end
      )' <<<"$MODELS" 2>/dev/null) || {
    warn "openrouter models response unusable; pi snapshot has no created/price"
    return 0
  }
  MODELS=$joined
  SOURCE="pi --list-models; openrouter /api/v1/models"
}

main() {
  mkdir -p "$OUT_DIR"

  local merged='{}'
  if [[ -f $CATALOG ]] && jq -e 'type == "object"' "$CATALOG" >/dev/null 2>&1; then
    merged=$(jq -c . "$CATALOG")
  elif [[ -e $CATALOG ]]; then
    warn "$CATALOG unreadable; starting a fresh catalog"
  fi

  local refreshed=0 dropped engine clean now MODELS SOURCE
  now=$(date +%s)
  for engine in claude codex cursor pi; do
    MODELS='' SOURCE=''
    "${engine}_models" || continue
    clean=$(sanitize_models <<<"$MODELS") || clean='[]'
    dropped=$(($(jq length <<<"$MODELS") - $(jq length <<<"$clean")))
    ((dropped == 0)) || warn "$engine: dropped $dropped IDs outside the accepted shape or over the cap"
    if [[ $(jq length <<<"$clean") -eq 0 ]]; then
      warn "$engine produced no valid model IDs; keeping any existing snapshot"
      continue
    fi
    merged=$(jq -c --arg e "$engine" --arg s "$SOURCE" --argjson t "$now" --slurpfile m <(printf '%s' "$clean") \
      '.[$e] = {fetched_epoch: $t, source: $s, models: $m[0]}' <<<"$merged")
    refreshed=$((refreshed + 1))
  done

  if ((refreshed == 0)); then
    warn "no engine refreshed; keeping any existing catalog"
    exit 1
  fi

  local tmp="$CATALOG.tmp.$$"
  jq . <<<"$merged" >"$tmp"
  mv "$tmp" "$CATALOG"
  printf '%s\n' "$CATALOG"
}

main "$@"
