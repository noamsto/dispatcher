setup() {
  load helpers
  SCRIPT="$BATS_TEST_DIRNAME/../adapters/core/refresh-budget.sh"
  STUB_DIR="$(mktemp -d)"
  STUB_LOG="$STUB_DIR/calls.log"
  FIXTURE_DIR="$(mktemp -d)"
  export XDG_DATA_HOME="$(mktemp -d)" STUB_DIR STUB_LOG FIXTURE_DIR
  # The script reads $HOME/.claude/.credentials.json — give it a throwaway HOME.
  export HOME="$(mktemp -d)"
  mkdir -p "$HOME/.claude"
  printf '%s\n' '{"claudeAiOauth":{"accessToken":"test-token"}}' >"$HOME/.claude/.credentials.json"
  write_fixtures
  write_curl_shim
  write_codex_shim
  export PATH="$STUB_DIR:$PATH"
}

write_fixtures() {
  cat >"$FIXTURE_DIR/claude_usage.json" <<'EOF'
{
  "five_hour": {"utilization": 12.5, "resets_at": "2026-08-03T22:00:00.296585+00:00"},
  "seven_day": {"utilization": 97.0, "resets_at": "2026-08-09T19:00:00.296585+00:00"},
  "seven_day_opus": null,
  "seven_day_sonnet": null,
  "extra_usage": {"is_enabled": true, "spend_limit_reached": false}
}
EOF
  cat >"$FIXTURE_DIR/statusline.json" <<'EOF'
{"rate_limits": {
  "five_hour": {"used_percentage": 55, "resets_at": 1785800000},
  "seven_day": {"used_percentage": 30, "resets_at": 1786200000}
}}
EOF
}

# The script's last curl argument is always the URL.
write_curl_shim() {
  cat >"$STUB_DIR/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [[ -n "${SHIM_CLAUDE_429:-}" ]]; then
  exit 22
fi
case "${@: -1}" in
  *api.anthropic.com/api/oauth/usage*) cat "$FIXTURE_DIR/claude_usage.json" ;;
  *) exit 22 ;;
esac
EOF
  chmod +x "$STUB_DIR/curl"
}

# Fake `codex app-server --stdio`: ignores the requests, emits canned frames.
write_codex_shim() {
  cat >"$STUB_DIR/codex" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${SHIM_CODEX_FAIL:-}" ]]; then
  exit 1
fi
printf '%s\n' '{"id":1,"result":{}}'
printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":42,"windowDurationMins":300,"resetsAt":1785800000},"secondary":{"usedPercent":61,"windowDurationMins":10080,"resetsAt":1786200000},"credits":{"hasCredits":true},"planType":"team"}}}'
EOF
  chmod +x "$STUB_DIR/codex"
}

@test "claude quota comes from the oauth endpoint with normalized windows" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '.engines.claude.source' "$cache"
  [ "$output" = "oauth_usage" ]
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "12.5" ]
  run jq '.engines.claude.windows["7d"].used_pct' "$cache"
  [ "$output" = "97.0" ]
  # Fractional +00:00 timestamps convert to epoch seconds.
  expected=$(jq -n '"2026-08-09T19:00:00Z" | fromdateiso8601')
  run jq ".engines.claude.windows[\"7d\"].resets_at == $expected" "$cache"
  [ "$output" = "true" ]
  run jq '.engines.claude.credits_cover' "$cache"
  [ "$output" = "true" ]
  # Null upstream windows are omitted.
  run jq '.engines.claude.windows | has("7d_opus")' "$cache"
  [ "$output" = "false" ]
  run jq '.engines.cursor' "$cache"
  [ "$output" = "null" ]
}

@test "claude falls back to a fresh statusline cache when the endpoint fails" {
  cp "$FIXTURE_DIR/statusline.json" "$XDG_DATA_HOME/crew/claude-statusline.json" 2>/dev/null || {
    mkdir -p "$XDG_DATA_HOME/crew"
    cp "$FIXTURE_DIR/statusline.json" "$XDG_DATA_HOME/crew/claude-statusline.json"
  }
  SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '.engines.claude.source' "$cache"
  [ "$output" = "statusline_cache" ]
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "55" ]
}

@test "a stale statusline cache is worse than unknown" {
  mkdir -p "$XDG_DATA_HOME/crew"
  cp "$FIXTURE_DIR/statusline.json" "$XDG_DATA_HOME/crew/claude-statusline.json"
  touch -d '3 hours ago' "$XDG_DATA_HOME/crew/claude-statusline.json"
  SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "codex windows are named by duration, not by primary/secondary slot" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '.engines.codex.source' "$cache"
  [ "$output" = "app-server" ]
  run jq '.engines.codex.windows["5h"].used_pct' "$cache"
  [ "$output" = "42" ]
  run jq '.engines.codex.windows["7d"].used_pct' "$cache"
  [ "$output" = "61" ]
  run jq '.engines.codex.credits_cover' "$cache"
  [ "$output" = "true" ]
}

@test "a codex failure degrades to null, never to exhausted" {
  SHIM_CODEX_FAIL=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"codex quota unknown"* ]]
  run jq '.engines.codex' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}
