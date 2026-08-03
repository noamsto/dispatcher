#!/usr/bin/env bash
# refresh-budget — probe per-engine subscription quota for budget-aware
# dispatching.
#
# Writes ${XDG_DATA_HOME:-~/.local/share}/crew/engine-budget.json. The
# dispatcher reads it when judging engine/rung, and dispatch.sh enforces the
# >=95% gate (see DISPATCHER_PROTOCOL.md). Refresh by hand or at dispatcher
# session start — no daemon. Every probe degrades to null on failure: a
# missing engine entry is "unknown", never "exhausted".
#
#   claude — GET /api/oauth/usage with the access token from
#     ~/.claude/.credentials.json (stays local, never printed). Fallback: a
#     statusline-dumped rate_limits payload at $XDG_DATA_HOME/crew/claude-
#     statusline.json, used only when its mtime is <2h old.
#   codex  — `codex app-server --stdio` JSON-RPC account/rateLimits/read
#     (experimental API; any failure -> null).
#   cursor — always null. Probed cursor-agent 2026.07: `status` is auth-only,
#     `about` shows the tier string but no numbers, and no usage/quota
#     subcommand exists. Plan usage is dashboard-only; the Enterprise Admin
#     API reports org-wide consumption events (admin key required), never a
#     member's remaining allowance.
set -euo pipefail

OUT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
OUT="$OUT_DIR/engine-budget.json"
STATUSLINE_CACHE="$OUT_DIR/claude-statusline.json"
CREDENTIALS="$HOME/.claude/.credentials.json"
STALE_AFTER_S=7200

warn() { printf 'refresh-budget: %s\n' "$*" >&2; }

# probe_claude — print the claude engine object via the OAuth usage endpoint,
# falling back to a fresh statusline dump; return 1 when neither works.
probe_claude() {
  local resp out
  if [[ -f $CREDENTIALS ]]; then
    local token
    token=$(jq -r '.claudeAiOauth.accessToken // empty' "$CREDENTIALS")
    # The endpoint rate-limits hard (429 or empty 200 after ~1 call/min) —
    # either failure falls through to the statusline cache below.
    if [[ -n $token ]] && resp=$(curl -sf --max-time 15 \
      -H "Authorization: Bearer $token" \
      -H "anthropic-beta: oauth-2025-04-20" \
      "https://api.anthropic.com/api/oauth/usage") && [[ -n $resp ]]; then
      # resets_at arrives as 2026-08-03T18:59:59.991098+00:00 — fromdateiso8601
      # only accepts Zulu whole seconds, so normalize first; unparseable -> null.
      if ! out=$(jq '
        def toepoch: if . == null then null
          else (try (sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601) catch null) end;
        {
          source: "oauth_usage",
          credits_cover: (.extra_usage // {} | (.is_enabled // false) and ((.spend_limit_reached // true) | not)),
          windows: (
            {}
            + (if .five_hour.utilization != null then {"5h": {used_pct: .five_hour.utilization, resets_at: (.five_hour.resets_at | toepoch)}} else {} end)
            + (if .seven_day.utilization != null then {"7d": {used_pct: .seven_day.utilization, resets_at: (.seven_day.resets_at | toepoch)}} else {} end)
            + (if .seven_day_opus.utilization != null then {"7d_opus": {used_pct: .seven_day_opus.utilization, resets_at: (.seven_day_opus.resets_at | toepoch)}} else {} end)
            + (if .seven_day_sonnet.utilization != null then {"7d_sonnet": {used_pct: .seven_day_sonnet.utilization, resets_at: (.seven_day_sonnet.resets_at | toepoch)}} else {} end)
          )
        }' <<<"$resp"); then
        return 1
      fi
      printf '%s\n' "$out"
      return 0
    fi
  fi
  # Fallback: statusline dump, fresh only — an old dump describes a quota that
  # has since drained or reset, which is worse than "unknown".
  if [[ -f $STATUSLINE_CACHE ]] &&
    (($(date +%s) - $(stat -c %Y "$STATUSLINE_CACHE") < STALE_AFTER_S)); then
    jq -e '.rate_limits | {
      source: "statusline_cache",
      credits_cover: null,
      windows: (
        {}
        + (if .five_hour.used_percentage != null then {"5h": {used_pct: .five_hour.used_percentage, resets_at: .five_hour.resets_at}} else {} end)
        + (if .seven_day.used_percentage != null then {"7d": {used_pct: .seven_day.used_percentage, resets_at: .seven_day.resets_at}} else {} end)
      )
    }' "$STATUSLINE_CACHE" 2>/dev/null && return 0
  fi
  return 1
}

# probe_codex — print the codex engine object via app-server JSON-RPC; return
# 1 when codex is absent or the experimental call changes shape.
probe_codex() {
  command -v codex >/dev/null 2>&1 || return 1
  local resp
  resp=$({
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"refresh-budget","version":"1"}}}'
    sleep 1
    printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"account/rateLimits/read","params":{}}'
    sleep 3
  } | timeout 20 codex app-server --stdio 2>/dev/null) || true
  [[ -n $resp ]] || return 1
  # Window names come from the duration, not the primary/secondary label — the
  # backend has shipped 5h and 7d windows in the same slots over time.
  jq -es '
    def wname($s):
      if $s <= 18600 then "5h" elif $s <= 90000 then "1d" elif $s <= 691200 then "7d" else "other" end;
    (map(select(.id == 2)) | .[0].result.rateLimits) as $r
    | {
        source: "app-server",
        credits_cover: ($r.credits.hasCredits // false),
        windows: (
          {}
          + (if $r.primary.usedPercent != null then {(wname(($r.primary.windowDurationMins // 0) * 60)): {used_pct: $r.primary.usedPercent, resets_at: $r.primary.resetsAt}} else {} end)
          + (if $r.secondary != null and $r.secondary.usedPercent != null then {(wname(($r.secondary.windowDurationMins // 0) * 60)): {used_pct: $r.secondary.usedPercent, resets_at: $r.secondary.resetsAt}} else {} end)
        )
      }
  ' <<<"$resp" 2>/dev/null
}

main() {
  local claude='null' codex='null' probe
  if probe=$(probe_claude); then
    claude=$probe
  else
    warn "claude quota unknown (oauth + statusline both unavailable)"
  fi
  if probe=$(probe_codex); then
    codex=$probe
  else
    warn "codex quota unknown (no codex CLI or app-server call failed)"
  fi

  mkdir -p "$OUT_DIR"
  local tmp="$OUT.tmp.$$"
  jq -n \
    --arg fetched_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson fetched_epoch "$(date +%s)" \
    --argjson claude "$claude" \
    --argjson codex "$codex" \
    '{
       fetched_at: $fetched_at,
       fetched_epoch: $fetched_epoch,
       engines: {claude: $claude, codex: $codex, cursor: null}
     }' >"$tmp"
  mv "$tmp" "$OUT"

  printf '%s\n' "$OUT"
  jq -r '.engines | to_entries[] | .key as $e |
    if .value == null then "\($e): unknown"
    else "\($e): " + ([.value.windows | to_entries[] | "\(.key) \(.value.used_pct)% used\(if .value.resets_at then " (resets \(.value.resets_at | todateiso8601))" else "" end)"] | join(", ")) + (if .value.credits_cover then " [credits cover]" else "" end)
    end' "$OUT"
}

main "$@"
