setup() {
  load helpers
  SCRIPT="$BATS_TEST_DIRNAME/../adapters/core/refresh-budget.sh"
  STUB_DIR="$(mktemp -d)"
  STUB_LOG="$STUB_DIR/calls.log"
  FIXTURE_DIR="$(mktemp -d)"
  export STUB_DIR STUB_LOG FIXTURE_DIR
  # The script reads $HOME/.claude/.credentials.json — give it a throwaway HOME.
  export HOME="$(mktemp -d)"
  mkdir -p "$HOME/.claude"
  printf '%s\n' '{"claudeAiOauth":{"accessToken":"test-token"}}' >"$HOME/.claude/.credentials.json"
  write_fixtures
  write_curl_shim
  write_codex_shim
  write_tmux_shim
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

# Fake tmux: list-windows / list-panes / capture-pane, dispatched on $1.
# Defaults to "no windows" (exit 1) when its controlling env vars are unset —
# load-bearing: every EXISTING test in this file (which sets none of them)
# must see the pane-scrape tier fail exactly like before this shim existed.
write_tmux_shim() {
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
case "$1" in
list-windows)
  [ -n "${SHIM_TMUX_WINDOWS:-}" ] || exit 1
  printf '%s\n' "$SHIM_TMUX_WINDOWS"
  ;;
list-panes)
  [ -n "${SHIM_TMUX_PANES:-}" ] || exit 1
  printf '%s\n' "$SHIM_TMUX_PANES"
  ;;
capture-pane)
  pane="" prev=""
  for a in "$@"; do
    [ "$prev" = "-t" ] && pane="$a"
    prev="$a"
  done
  id="${pane#%}"
  var="SHIM_TMUX_CAPTURE_P${id}"
  val="${!var:-}"
  [ -n "$val" ] || exit 1
  printf '%s\n' "$val"
  ;;
*) exit 1 ;;
esac
EOF
  chmod +x "$STUB_DIR/tmux"
}

@test "claude quota comes from the oauth endpoint with normalized windows" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  # The fixture's 7d window is 97.0% (>=85%) — the budget lever must visibly
  # fire a warning for it, and must NOT fire one for the 12.5% 5h window.
  [[ "$output" == *"budget lever: claude 7d at 97"*"approaching quota"* ]]
  [[ "$output" != *"budget lever: claude 5h at 12.5"* ]]
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

@test "pane-scrape fallback produces a populated cache when oauth+statusline both fail" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  🤖 Sonnet 5 🧠 high | 📊 170k/1M | ⚡ 89% (10m → 05:20) 7d 61% (9h49m)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '.engines.claude.source' "$cache"
  [ "$output" = "pane_scrape" ]
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "89" ]
  run jq '.engines.claude.windows["7d"].used_pct' "$cache"
  [ "$output" = "61" ]
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.claude.credits_cover' "$cache"
  [ "$output" = "null" ]
}

@test "pane-scrape aggregates by max across multiple worker windows" {
  SHIM_TMUX_WINDOWS=$'@1\tnova\n@2\tember' \
    SHIM_TMUX_PANES=$'@1\t%10\n@2\t%20' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 42% (10m)' \
    SHIM_TMUX_CAPTURE_P20=$'  ⚡ 89% (5m) 7d 70% (2h)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "89" ]
  run jq '.engines.claude.windows["7d"].used_pct' "$cache"
  [ "$output" = "70" ]
}

@test "pane-scrape excludes the dispatcher's own window even when it renders a statusline" {
  SHIM_TMUX_WINDOWS=$'@1\tdispatcher' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 97% (1m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "pane-scrape ignores a stray ⚡NN% sitting above the anchored tail" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'Reading WORKER_TASK.md — example line: ⚡ 97% (2h53m → 00:20)\n  ⎿  Done (3 tool uses)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "pane-scrape degrades to unknown with no error when no worker windows exist" {
  SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}
