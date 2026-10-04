bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  SCRIPT="$BATS_TEST_DIRNAME/../adapters/core/refresh-budget.sh"
  STUB_DIR="$(mktemp -d)"
  STUB_LOG="$STUB_DIR/calls.log"
  FIXTURE_DIR="$(mktemp -d)"
  export STUB_DIR STUB_LOG FIXTURE_DIR
  # Resolved before $STUB_DIR is prepended to PATH, so the date and uname
  # shims below can fall through to the genuine binary for every call they
  # don't fake.
  REAL_DATE="$(command -v date)"
  REAL_UNAME="$(command -v uname)"
  export REAL_DATE REAL_UNAME
  # The script reads $HOME/.claude/.credentials.json — give it a throwaway HOME.
  export HOME="$(mktemp -d)"
  mkdir -p "$HOME/.claude"
  printf '%s\n' '{"claudeAiOauth":{"accessToken":"test-token"}}' >"$HOME/.claude/.credentials.json"
  unset OPENROUTER_API_KEY DISPATCH_OPENROUTER_KEY_FILE DISPATCH_OPENROUTER_MONTHLY_USD PI_CODING_AGENT_DIR
  write_fixtures
  write_curl_shim
  write_codex_shim
  write_tmux_shim
  write_date_shim
  write_cursor_shims
  export PATH="$STUB_DIR:$PATH"
}

# or_key_fixture <usage_monthly> — a stubbed /api/v1/key response shaped like
# OpenRouter's documented GET /api/v1/key.
or_key_fixture() {
  jq -n --argjson u "$1" '{
    data: {
      label: "sk-or-v1-x...", usage: 999, usage_daily: 1, usage_weekly: 2,
      usage_monthly: $u, limit: null, limit_remaining: null, limit_reset: null,
      is_management_key: false, is_provisioning_key: false
    }
  }' >"$FIXTURE_DIR/or_key.json"
}

# write_date_shim — a `date` that answers `+%s` from $SHIM_NOW when set, so
# refresh-budget's month math runs against a chosen instant; every other
# invocation (formats, -d, -u, ...) execs the real binary unchanged.
write_date_shim() {
  cat >"$STUB_DIR/date" <<EOF
#!/usr/bin/env bash
if [[ -n "\${SHIM_NOW:-}" && "\$#" -eq 1 && "\$1" == "+%s" ]]; then
  printf '%s\n' "\$SHIM_NOW"
  exit 0
fi
exec "$REAL_DATE" "\$@"
EOF
  chmod +x "$STUB_DIR/date"
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
  *openrouter.ai/api/v1/key*)
    # Consumed, never logged: the key arrives on stdin (-K -), and the whole
    # point of this shim is to prove it never lands anywhere else.
    stdin_content="$(cat)"
    if [[ -n "${SHIM_OR_FAIL:-}" ]]; then
      exit 22
    fi
    expect="${SHIM_OR_EXPECT_KEY:-sk-or-v1-SENTINELKEY123}"
    if [[ "$stdin_content" == *"Authorization: Bearer $expect"* ]]; then
      printf 'openrouter_key_on_stdin=yes\n' >>"$STUB_LOG"
    else
      printf 'openrouter_key_on_stdin=no\n' >>"$STUB_LOG"
    fi
    cat "$FIXTURE_DIR/or_key.json"
    ;;
  *cursor.com/api/usage-summary*)
    # Same contract as the OpenRouter arm: the session cookie arrives on
    # stdin and is never logged.
    stdin_content="$(cat)"
    if [[ -n "${SHIM_CURSOR_FAIL:-}" ]]; then
      exit 22
    fi
    expect="${SHIM_CURSOR_EXPECT:-user_TESTACCOUNT::SENTINELCURSORTOKEN}"
    if [[ "$stdin_content" == *"Cookie: WorkosCursorSessionToken=$expect\""* ]]; then
      printf 'cursor_cookie_on_stdin=yes\n' >>"$STUB_LOG"
    else
      printf 'cursor_cookie_on_stdin=no\n' >>"$STUB_LOG"
    fi
    cat "$FIXTURE_DIR/cursor_usage.json"
    ;;
  *) exit 22 ;;
esac
EOF
  chmod +x "$STUB_DIR/curl"
}

# Fake `codex app-server --stdio`: ignores the requests, emits canned frames.
# SHIM_CODEX_GENERIC swaps in a single 1440min (24h) window at 90% used — the
# static default below is pinned at 5h/7d buckets and can never emit a
# 1d/unknown/other key at >=85%, which the generic advisory wording needs.
# SHIM_CODEX_CUSTOM swaps in a caller-controlled window (usedPercent,
# windowDurationMins, and an optional resetsAt) — neither fixed frame can
# reach the >=95% gating verdicts, which need specific percentages and
# reset times chosen per test. resetsAt is computed from $(date +%s) at
# shim invocation time (offset by SHIM_CODEX_RESETS_IN seconds) rather than
# a baked-in epoch, so a test built on it can't age into failure; omitting
# the offset omits resetsAt entirely, for the null-reset case.
# SHIM_CODEX_LEGACY emits the pre-#201 frame (no planType, no top-level
# ordinaryUsageAllowed, no credits.unlimited/balance, no individualLimit,
# no spendControlReached/rateLimitReachedType): what an old codex build or
# an older cached response carries.
write_codex_shim() {
  cat >"$STUB_DIR/codex" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${SHIM_CODEX_FAIL:-}" ]]; then
  exit 1
fi
printf '%s\n' '{"id":1,"result":{}}'
if [[ -n "${SHIM_CODEX_LEGACY:-}" ]]; then
  printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":42,"windowDurationMins":300,"resetsAt":1785800000},"secondary":{"usedPercent":61,"windowDurationMins":10080,"resetsAt":1786200000},"credits":{"hasCredits":true}}}}'
elif [[ -n "${SHIM_CODEX_GENERIC:-}" ]]; then
  printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":90,"windowDurationMins":1440},"credits":{"hasCredits":true},"planType":"team"}}}'
elif [[ -n "${SHIM_CODEX_CUSTOM:-}" ]]; then
  resets_field=""
  if [[ -n "${SHIM_CODEX_RESETS_IN:-}" ]]; then
    resets_field=",\"resetsAt\":$(($(date +%s) + SHIM_CODEX_RESETS_IN))"
  fi
  printf '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":%s,"windowDurationMins":%s%s},"credits":{"hasCredits":true},"planType":"team"}}}\n' \
    "$SHIM_CODEX_USED_PCT" "$SHIM_CODEX_WINDOW_MINS" "$resets_field"
else
  printf '%s\n' '{"id":2,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"primary":{"usedPercent":42,"windowDurationMins":300,"resetsAt":1785800000},"secondary":{"usedPercent":61,"windowDurationMins":10080,"resetsAt":1786200000},"credits":{"hasCredits":true,"unlimited":false,"balance":"12.34"},"individualLimit":null,"spendControlReached":null,"planType":"team","rateLimitReachedType":null}}}'
fi
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

# write_cursor_shims — probe_cursor's host edges, all inert by default. A
# `cursor-agent` whose presence satisfies `command -v` but which marks the log
# if anything runs it (the probe never may); a `uname` answering Linux to -s
# unless SHIM_UNAME says otherwise, so a macOS dev host never takes the
# Darwin branch; a `security` that fails unless SHIM_SECURITY_TOKEN opts in,
# so no test ever reaches a real keychain.
write_cursor_shims() {
  cat >"$STUB_DIR/cursor-agent" <<'EOF'
#!/usr/bin/env bash
printf 'cursor-agent-executed\n' >>"$STUB_LOG"
exit 1
EOF
  cat >"$STUB_DIR/uname" <<EOF
#!/usr/bin/env bash
if [[ "\$#" -eq 1 && "\$1" == "-s" ]]; then
  printf '%s\n' "\${SHIM_UNAME:-Linux}"
  exit 0
fi
exec "$REAL_UNAME" "\$@"
EOF
  cat >"$STUB_DIR/security" <<'EOF'
#!/usr/bin/env bash
printf 'security %s\n' "$*" >>"$STUB_LOG"
if [[ -n "${SHIM_SECURITY_TOKEN:-}" ]]; then
  printf '%s\n' "$SHIM_SECURITY_TOKEN"
  exit 0
fi
exit 44
EOF
  chmod +x "$STUB_DIR/cursor-agent" "$STUB_DIR/uname" "$STUB_DIR/security"
}

# cursor_auth <field> [token] — a synthetic Linux auth.json holding <token>
# (default the sentinel) under <field>.
cursor_auth() {
  mkdir -p "$XDG_CONFIG_HOME/cursor"
  jq -n --arg f "$1" --arg t "${2:-SENTINELCURSORTOKEN}" '{($f): $t}' >"$XDG_CONFIG_HOME/cursor/auth.json"
}

# cursor_config [dir] — a synthetic cli-config.json whose authInfo.authId
# carries the provider prefix the probe strips.
cursor_config() {
  local dir="${1:-$XDG_CONFIG_HOME/cursor}"
  mkdir -p "$dir"
  printf '%s\n' '{"version":1,"authInfo":{"authId":"auth0|user_TESTACCOUNT"}}' >"$dir/cli-config.json"
}

# cursor_jwt <sub> — a synthetic JWT-shaped token: base64url header and
# payload, unpadded, signature SENTINELSIG. With the sub the tests use, the
# extra claim makes the payload need both padding and the -/_ translation.
cursor_jwt() {
  local header payload
  header=$(printf '%s' '{"alg":"none","typ":"JWT"}' | base64 | tr -d '\n=' | tr '+/' '-_')
  payload=$(jq -cnj --arg s "$1" '{sub: $s, x: "???>>>?"}' | base64 | tr -d '\n=' | tr '+/' '-_')
  printf '%s.%s.SENTINELSIG' "$header" "$payload"
}

# cursor_usage <json> — the stubbed usage-summary response body.
cursor_usage() {
  printf '%s\n' "$1" >"$FIXTURE_DIR/cursor_usage.json"
}

# cursor_individual_usage — an individual-plan response: two plan pools, ISO
# dates with fractional seconds, on-demand off.
cursor_individual_usage() {
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31.000Z",
    "billingCycleEnd": "2026-10-14T08:12:31.000Z",
    "membershipType": "pro",
    "isUnlimited": false,
    "individualUsage": {
      "plan": {"enabled": true, "used": 1269, "limit": 2000, "remaining": 731,
               "autoPercentUsed": 41.25, "apiPercentUsed": 63.46, "totalPercentUsed": 52.36},
      "onDemand": {"enabled": false, "used": 0, "limit": null, "remaining": null}
    },
    "teamUsage": {}
  }'
}

@test "claude quota comes from the oauth endpoint with normalized windows" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  # The fixture's 7d window is 97.0% (>=85%) — the budget lever must visibly
  # fire a warning for it, and must NOT fire one for the 12.5% 5h window.
  # Its resets_at (2026-08-09) is already in the past, so the window does not
  # gate — the cache predates the reset (same exemption as dispatch's own
  # >=95% stop) — and the line carries no "(resets in ...)" parenthetical,
  # verified by requiring the "%" to butt directly against the em dash.
  [[ "$output" == *"budget lever: claude 7d at 97.0% — not binding: window has already reset"* ]]
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

@test "codex records plan_type and the absolute-limit signals from the response" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '.engines.codex.plan_type' "$cache"
  [ "$output" = "team" ]
  run jq '.engines.codex.limit_reached.ordinary_usage_allowed' "$cache"
  [ "$output" = "true" ]
  run jq '.engines.codex.limit_reached.rate_limit_reached_type' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.spend_control_reached' "$cache"
  [ "$output" = "null" ]
  # credits.unlimited is a boolean false, not the `// null` collapse of it.
  run jq '.engines.codex.limit_reached.credits_unlimited' "$cache"
  [ "$output" = "false" ]
  run jq -r '.engines.codex.limit_reached.credits_balance' "$cache"
  [ "$output" = "12.34" ]
  run jq '.engines.codex.limit_reached.individual_remaining_percent' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.individual_resets_at' "$cache"
  [ "$output" = "null" ]
}

@test "claude plan_type stays null: the oauth payload carries no plan key" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.engines.claude.plan_type' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "the summary line prints the codex plan tier in brackets" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"codex: [team] 5h 42% used"* ]]
}

@test "a legacy codex response records a null tier and null absolute-limit fields" {
  SHIM_CODEX_LEGACY=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.codex.plan_type' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.ordinary_usage_allowed' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.rate_limit_reached_type' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.spend_control_reached' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.credits_unlimited' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.credits_balance' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.individual_remaining_percent' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.codex.limit_reached.individual_resets_at' "$cache"
  [ "$output" = "null" ]
}

@test "a codex failure degrades to null, never to exhausted" {
  SHIM_CODEX_FAIL=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"codex quota unknown"* ]]
  run jq '.engines.codex' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "pane-scrape derives resets_at from the parsed countdown, for both windows" {
  now=$(date +%s)
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
  # "10m -> 05:20" parses to a 600s countdown despite the arrow tail, and
  # "9h49m" (9*3600 + 49*60 = 35340s) parses without one.
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" -ge $((now + 540)) ]
  [ "$output" -le $((now + 660)) ]
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" -ge $((now + 35240)) ]
  [ "$output" -le $((now + 35440)) ]
  run jq '.engines.claude.credits_cover' "$cache"
  [ "$output" = "null" ]
}

@test "pane-scrape aggregates by max across multiple worker windows" {
  now=$(date +%s)
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
  # The winning percentage (89%, from pane %20) carries pane %20's own
  # countdown (5m = 300s), not pane %10's higher 10m — the max comparison
  # picks the pane, not the longest countdown.
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" -ge $((now + 240)) ]
  [ "$output" -le $((now + 420)) ]
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" -ge $((now + 7140)) ]
  [ "$output" -le $((now + 7320)) ]
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

# Fixtures below are trimmed from live claude worker panes
# (`tmux capture-pane -p -t <pane>`, 2026-09-24): subagent rows render BELOW
# the statusline and mode line.
@test "pane-scrape finds the statusline above a mode line with subagent rows below" {
  now=$(date +%s)
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⎿  Done (3 tool uses)\n\n  🤖 Sonnet 5 🧠 med | 📊 120k/1M | ⚡ 2% (4h44m → 13:10)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · PR #308 · ← for agents\n\n  ● main\n  ◯ shell-reviewer  Review dispatch.sh diff        13s · ↓ 25.3k tokens' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '.engines.claude.source' "$cache"
  [ "$output" = "pane_scrape" ]
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "2" ]
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" -ge $((now + 16940)) ]
  [ "$output" -le $((now + 17140)) ]
  run jq '.engines.claude.windows | has("7d")' "$cache"
  [ "$output" = "false" ]
}

@test "pane-scrape parses the 7d day-form countdown with subagent rows below" {
  now=$(date +%s)
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  🤖 Sonnet 5 🧠 med | 📊 120k/1M | ⚡ 13% (2h4m → 23:20) 7d 95% (3d14h)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← for agents\n\n  ● main\n  ◯ go-reviewer  Review diff        9s · ↓ 4k tokens' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "13" ]
  run jq '.engines.claude.windows["7d"].used_pct' "$cache"
  [ "$output" = "95" ]
  # 3d14h = 3*86400 + 14*3600 = 309600s
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" -ge $((now + 309500)) ]
  [ "$output" -le $((now + 309700)) ]
}

@test "pane-scrape reads the statusline of a pane with no subagent rows and no 7d" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  🤖 Sonnet 5 🧠 med | 📊 120k/1M | ⚡ 31% (1h0m → 13:10)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.engines.claude.windows["5h"].used_pct' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "31" ]
}

@test "pane-scrape reads the bottom block, not a pasted statusline higher in the scrollback" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  🤖 Sonnet 5 | ⚡ 97% (2h53m → 00:20) 7d 99% (1d1h)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)\n  ⎿  Done (3 tool uses)\n  🤖 Sonnet 5 | ⚡ 13% (2h4m → 23:20)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)\n\n  ● main\n  ◯ go-reviewer  Review diff        9s' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "13" ]
  run jq '.engines.claude.windows | has("7d")' "$cache"
  [ "$output" = "false" ]
}

@test "pane-scrape is not moved by a mode glyph inside a subagent row" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  🤖 Sonnet 5 | ⚡ 44% (1h0m → 13:10)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)\n\n  ● main\n  ◯ go-reviewer  saw ⏵⏵ in output        9s' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.engines.claude.windows["5h"].used_pct' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "44" ]
}

@test "pane-scrape degrades to unknown with no error when no worker windows exist" {
  SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

# Neither existing fixture crosses the >=85% advisory filter, so a fresh
# fixture (future epochs, computed at write time — the oauth fixture is
# only a "past reset" case because its hardcoded 2026-08 dates aged into
# one) is the only way to exercise the window-differentiated wording, the
# pace clause, and the "in <reltime>" summary tail together.
@test "budget lever advisory is window-differentiated, with time and pace" {
  mkdir -p "$XDG_DATA_HOME/crew"
  now=$(date +%s)
  # 5h: 91% used, ~35m to reset (2130s, not the exact minute boundary —
  # leaves margin so the script's own later `date +%s` can't round it down
  # to 34m). 7d: 97% used, 338688s (3d 22h) to reset, chosen so elapsed_pct
  # lands on an exact 44.0 and "ahead" rounds to a clean 53.
  cat >"$XDG_DATA_HOME/crew/claude-statusline.json" <<EOF
{"rate_limits": {
  "five_hour": {"used_percentage": 91, "resets_at": $((now + 2130))},
  "seven_day": {"used_percentage": 97, "resets_at": $((now + 338688))}
}}
EOF
  SHIM_CLAUDE_429=1 SHIM_CODEX_GENERIC=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"budget lever: claude 5h at 91% (resets in 35m) — short window: prefer waiting past the reset to shedding burn class"* ]]
  [[ "$output" == *"budget lever: claude 7d at 97% (resets in 3d 22h, 53 points ahead of pace) — binding window; not holdable: 3d 22h is outside the window's last 15%, hand the task back"* ]]
  [[ "$output" == *"budget lever: codex 1d at 90% — approaching quota (>=85%), prefer a cheaper burn class or rotate engines (see DISPATCHER_PROTOCOL.md)"* ]]
  # The summary line gains the relative remaining only on a future reset —
  # the oauth fixture's past-dated one has no ", in ..." tail by design.
  [[ "$output" == *"claude: "*"(resets "*", in "* ]]
}

@test "the nearer of two >=95% windows on one engine defers, never reads holdable" {
  mkdir -p "$XDG_DATA_HOME/crew"
  now=$(date +%s)
  # Both windows clear the 95% gate; 7d's resets_at is the later one, so
  # rule 3 makes it the sole gating window. The nearer 5h window must defer
  # to it by key, not be judged on its own (much sooner) elapsed time — the
  # wrong-window bug this fold exists to prevent. Pinning the 5h row's full
  # text (not just a loose "not binding" substring) is what rules out
  # "holdable" leaking in from the wrong window. 330s (not the exact 5m
  # boundary) leaves margin so the script's own later `date +%s` can't round
  # it down to 4m.
  cat >"$XDG_DATA_HOME/crew/claude-statusline.json" <<EOF
{"rate_limits": {
  "five_hour": {"used_percentage": 96, "resets_at": $((now + 330))},
  "seven_day": {"used_percentage": 97, "resets_at": $((now + 300000))}
}}
EOF
  SHIM_CLAUDE_429=1 SHIM_CODEX_GENERIC=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"budget lever: claude 5h at 96% (resets in 5m) — not binding: claude is gated until 7d resets"* ]]
}

@test "a sized window at >=95% with a null resets_at is unholdable, with no binding-window prefix" {
  mkdir -p "$XDG_DATA_HOME/crew"
  # 5h has a nominal length (wsecs) but no resets_at at all — rule 1 (no
  # usable deadline) must still catch it even though the window is sized,
  # not just the unsized rule 2 case below.
  cat >"$XDG_DATA_HOME/crew/claude-statusline.json" <<'EOF'
{"rate_limits": {
  "five_hour": {"used_percentage": 96, "resets_at": null}
}}
EOF
  SHIM_CLAUDE_429=1 SHIM_CODEX_GENERIC=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"budget lever: claude 5h at 96% — not holdable: no reset time, hand the task back"* ]]
}

@test "a >=95% window whose reset has passed does not gate, a future sibling does" {
  mkdir -p "$XDG_DATA_HOME/crew"
  now=$(date +%s)
  # The 5h window is exhausted but its reset already passed (#548) — the
  # cache predates the rollover, so it must not gate. The 7d window is real,
  # future, and sized, so it binds and the 5h sibling defers to it.
  cat >"$XDG_DATA_HOME/crew/claude-statusline.json" <<EOF
{"rate_limits": {
  "five_hour": {"used_percentage": 97, "resets_at": $((now - 100))},
  "seven_day": {"used_percentage": 96, "resets_at": $((now + 300000))}
}}
EOF
  SHIM_CLAUDE_429=1 SHIM_CODEX_GENERIC=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"budget lever: claude 5h at 97% — not binding: claude is gated until 7d resets"* ]]
  [[ "$output" == *"budget lever: claude 7d at 96% (resets in 3d 11h"*"binding window; not holdable"* ]]
}

@test "an unsized window at >=95% is unholdable even with a real reset time" {
  # 1440min (1d bucket) has no wsecs entry, so rule 2 fires despite a
  # perfectly good future resetsAt — nominal length, not deadline, is what's
  # missing here. 22350s (not the exact 6h12m boundary) leaves margin so the
  # sleeps inside the real probe_codex's pipe can't round it down to 6h11m.
  SHIM_CLAUDE_429=1 SHIM_CODEX_CUSTOM=1 SHIM_CODEX_USED_PCT=99 \
    SHIM_CODEX_WINDOW_MINS=1440 SHIM_CODEX_RESETS_IN=22350 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"budget lever: codex 1d at 99% (resets in 6h 12m) — not holdable: window has no nominal length, hand the task back"* ]]
}

@test "a >=95% window inside its last 15% is the holdable case" {
  # A sized 5h window, 99% used, ~38m to reset —
  # elapsed_pct lands north of the 85 floor, so it's holdable. 2310s (not
  # the exact 38m boundary) leaves the same rounding margin as above.
  SHIM_CLAUDE_429=1 SHIM_CODEX_CUSTOM=1 SHIM_CODEX_USED_PCT=99 \
    SHIM_CODEX_WINDOW_MINS=300 SHIM_CODEX_RESETS_IN=2310 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"budget lever: codex 5h at 99% (resets in 38m) — binding window; holdable: inside the window's last 15%, wait past the reset"* ]]
}

@test "pane-scrape parses a day-form countdown" {
  now=$(date +%s)
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 50% (2h) 7d 91% (4d3h)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["7d"].used_pct' "$cache"
  [ "$output" = "91" ]
  # 4d3h = 4*86400 + 3*3600 = 356400s, inside the 604800s 7d window.
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" -ge $((now + 356280)) ]
  [ "$output" -le $((now + 356520)) ]
}

@test "pane-scrape leaves resets_at null when no countdown parenthetical is present" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 50% 7d 61%' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" = "null" ]
}

@test "pane-scrape leaves resets_at null for an unparseable parenthetical" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 50% (soon) 7d 61% (later today)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" = "null" ]
}

@test "pane-scrape rejects a countdown longer than the window's nominal length" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 50% (6h) 7d 61% (8d)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" = "null" ]
}

@test "pane-scrape parses a zero-padded countdown without crashing" {
  now=$(date +%s)
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 42% (08m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "42" ]
  # "08m" must parse as 8 minutes (480s), not crash on the leading zero.
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" -ge $((now + 420)) ]
  [ "$output" -le $((now + 540)) ]
}

@test "pane-scrape parses a zero-padded percentage without crashing" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 08% (10m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "8" ]
}

@test "pane-scrape treats a leading-zero multi-digit component as base 10" {
  now=$(date +%s)
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 50% (10m) 7d 61% (010h)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  # "010h" is read as base-10 10 (36000s), not octal 8 — well inside the 7d
  # window's nominal length, so it survives the guard.
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" -ge $((now + 35880)) ]
  [ "$output" -le $((now + 36120)) ]
}

@test "pane-scrape yields a null resets_at for an absurd day count, not a wrong clock" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 50% (10m) 7d 61% (213503982334602d)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["7d"].used_pct' "$cache"
  [ "$output" = "61" ]
  run jq '.engines.claude.windows["7d"].resets_at' "$cache"
  [ "$output" = "null" ]
}

@test "pane-scrape clamps a percentage over 100" {
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 999% (10m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "100" ]
}

@test "pane-scrape tie-break picks the larger remaining when percentages match" {
  now=$(date +%s)
  SHIM_TMUX_WINDOWS=$'@1\tnova\n@2\tember' \
    SHIM_TMUX_PANES=$'@1\t%10\n@2\t%20' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 70% (5m)' \
    SHIM_TMUX_CAPTURE_P20=$'  ⚡ 70% (20m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "70" ]
  # Both panes report 70% — the tie-break must keep the larger remaining
  # (pane %20's 20m), not whichever pane happened to be scanned first.
  run jq '.engines.claude.windows["5h"].resets_at' "$cache"
  [ "$output" -ge $((now + 1140)) ]
  [ "$output" -le $((now + 1260)) ]
}

@test "pane-scrape rejects an oversized percentage instead of wrapping through 10#" {
  # 55340232221128654890 is the value that "10#$p" silently wraps to 42
  # (verified: bash -c 'p=55340232221128654890; echo $((10#$p))' -> 42).
  # A garbled render that wraps into the middle of the range would be
  # indistinguishable from a real 42% reading and could mask an exhausted
  # budget — the marker must be skipped instead, same as an unparseable
  # countdown already is.
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 55340232221128654890% (10m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "pane-scrape rejects a 4-digit percentage" {
  # Deliberate choice: the marker regex is bounded to {1,3} digits, so a
  # 4-digit run (even one well within int64 range) fails the same way the
  # oversized run above does — it never reaches arithmetic to be judged
  # "too large", it just doesn't match.
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 1234% (10m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "pane-scrape still parses a one-digit percentage" {
  # Regression check alongside the existing 2-digit ("08%", zero-padded)
  # and 3-digit ("999%", clamped) cases: the {1,3} bound must not exclude
  # the short end of the legitimate range.
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10=$'  ⚡ 5% (10m)' \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.claude.windows["5h"].used_pct' "$cache"
  [ "$output" = "5" ]
}

@test "no OpenRouter key leaves pi unknown without blocking" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"pi spend unknown"* ]]
  [[ "$output" == *"OPENROUTER_API_KEY"* ]]
  [[ "$output" == *"keyFile"* ]]
  [[ "$output" == *"pi: unknown"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  ! grep -q openrouter "$STUB_LOG"
}

@test "pi spend and target render mid-month with a projection" {
  or_key_fixture 40
  now=$("$REAL_DATE" -u -d '2026-09-10T00:00:00Z' +%s)
  start_epoch=$("$REAL_DATE" -u -d '2026-09-01T00:00:00Z' +%s)
  reset_epoch=$("$REAL_DATE" -u -d '2026-10-01T00:00:00Z' +%s)
  SHIM_NOW="$now" DISPATCH_OPENROUTER_MONTHLY_USD=50 \
    OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *'pi: openrouter $40.00 of $50.00 monthly target'* ]]
  [[ "$output" == *'projected $133.33 at month end'* ]]
  [[ "$output" == *'budget lever: pi projected $133.33 at month end, over the $50.00 monthly target'* ]]
  [[ "$output" != *"SENTINELKEY123"* ]]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '.engines.pi.source' "$cache"
  [ "$output" = "openrouter_key" ]
  run jq '.engines.pi.spend_usd' "$cache"
  [ "$output" = "40" ]
  run jq '.engines.pi.target_usd' "$cache"
  [ "$output" = "50" ]
  run jq '.engines.pi.windows.month.used_pct' "$cache"
  [ "$output" = "80" ]
  run jq '.engines.pi.windows.month.starts_at' "$cache"
  [ "$output" = "$start_epoch" ]
  run jq '.engines.pi.windows.month.resets_at' "$cache"
  [ "$output" = "$reset_epoch" ]
  run jq '.engines.pi.elapsed_pct' "$cache"
  [ "$output" = "30" ]
  run jq '(.engines.pi.projected_month_end_usd * 100 | round)' "$cache"
  [ "$output" = "13333" ]
  ! grep -q SENTINELKEY123 "$cache"
  ! grep -q SENTINELKEY123 "$STUB_LOG"
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
}

@test "the key file wins over OPENROUTER_API_KEY" {
  or_key_fixture 10
  keyfile="$BATS_TEST_TMPDIR/or-key"
  printf 'sk-or-v1-FILEKEY\r\n' >"$keyfile"
  DISPATCH_OPENROUTER_KEY_FILE="$keyfile" OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 \
    SHIM_OR_EXPECT_KEY=sk-or-v1-FILEKEY run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" != *"SENTINELKEY123"* ]]
  [[ "$output" != *"FILEKEY"* ]]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
}

@test "an unreadable key file with no env leaves pi unknown" {
  keyfile="$BATS_TEST_TMPDIR/or-key-missing"
  DISPATCH_OPENROUTER_KEY_FILE="$keyfile" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"pi spend unknown"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "an unreadable key file never falls back to OPENROUTER_API_KEY" {
  or_key_fixture 10
  keyfile="$BATS_TEST_TMPDIR/or-key-missing"
  DISPATCH_OPENROUTER_KEY_FILE="$keyfile" OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 \
    run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"is unreadable or empty"* ]]
  [[ "$output" != *"SENTINELKEY123"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  ! grep -q openrouter "$STUB_LOG"
}

@test "an empty key file never falls back to OPENROUTER_API_KEY" {
  or_key_fixture 10
  keyfile="$BATS_TEST_TMPDIR/or-key-empty"
  : >"$keyfile"
  DISPATCH_OPENROUTER_KEY_FILE="$keyfile" OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 \
    run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"is unreadable or empty"* ]]
  [[ "$output" != *"SENTINELKEY123"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  ! grep -q openrouter "$STUB_LOG"
}

@test "a malformed key response leaves pi unknown" {
  jq -n '{data: {}}' >"$FIXTURE_DIR/or_key.json"
  OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"call failed"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "no target records spend without a gating window" {
  or_key_fixture 40
  now=$("$REAL_DATE" -u -d '2026-09-10T00:00:00Z' +%s)
  reset_epoch=$("$REAL_DATE" -u -d '2026-10-01T00:00:00Z' +%s)
  SHIM_NOW="$now" OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"month-to-date (no monthly target"* ]]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.pi.spend_usd' "$cache"
  [ "$output" = "40" ]
  run jq '.engines.pi.target_usd' "$cache"
  [ "$output" = "null" ]
  run jq '.engines.pi.windows' "$cache"
  [ "$output" = "{}" ]
  run jq '.engines.pi.resets_at' "$cache"
  [ "$output" = "$reset_epoch" ]
}

@test "the first 24h of the month suppress the projection" {
  or_key_fixture 5
  now=$("$REAL_DATE" -u -d '2026-09-01T06:00:00Z' +%s)
  SHIM_NOW="$now" DISPATCH_OPENROUTER_MONTHLY_USD=50 \
    OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"too early to project"* ]]
  [[ "$output" != *'budget lever: pi projected $'* ]]
  run jq '.engines.pi.projected_month_end_usd' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "December rolls the reset over to next January" {
  or_key_fixture 1
  now=$("$REAL_DATE" -u -d '2026-12-15T00:00:00Z' +%s)
  reset_epoch=$("$REAL_DATE" -u -d '2027-01-01T00:00:00Z' +%s)
  SHIM_NOW="$now" OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.engines.pi.resets_at' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "$reset_epoch" ]
}

@test "a failed OpenRouter call leaves pi unknown, not exhausted" {
  SHIM_OR_FAIL=1 OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"pi spend unknown"* ]]
  [[ "$output" == *"failed"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "a malformed monthly target is ignored with a warning" {
  or_key_fixture 40
  DISPATCH_OPENROUTER_MONTHLY_USD=abc OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"DISPATCH_OPENROUTER_MONTHLY_USD"* ]]
  [[ "$output" == *"not a positive number"* ]]
  run jq '.engines.pi.target_usd' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  run jq '.engines.pi.windows' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "{}" ]
}

@test "a zero monthly target is ignored with a warning" {
  or_key_fixture 40
  DISPATCH_OPENROUTER_MONTHLY_USD=0 OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"DISPATCH_OPENROUTER_MONTHLY_USD"* ]]
  run jq '.engines.pi.target_usd' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "a month window at 96% fires the budget lever" {
  or_key_fixture 48
  DISPATCH_OPENROUTER_MONTHLY_USD=50 OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"budget lever: pi month at 96"* ]]
}

@test "a user-layer monthly target is used when the env target is unset" {
  or_key_fixture 40
  now=$("$REAL_DATE" -u -d '2026-09-10T00:00:00Z' +%s)
  start_epoch=$("$REAL_DATE" -u -d '2026-09-01T00:00:00Z' +%s)
  reset_epoch=$("$REAL_DATE" -u -d '2026-10-01T00:00:00Z' +%s)
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"openrouter":{"monthlyUsd":50}}\n' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  SHIM_NOW="$now" OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *'pi: openrouter $40.00 of $50.00 monthly target'* ]]
  [[ "$output" == *'projected $133.33 at month end'* ]]
  [[ "$output" == *'budget lever: pi projected $133.33 at month end, over the $50.00 monthly target'* ]]
  [[ "$output" != *"SENTINELKEY123"* ]]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq '.engines.pi.target_usd' "$cache"
  [ "$output" = "50" ]
  run jq '.engines.pi.windows.month.starts_at' "$cache"
  [ "$output" = "$start_epoch" ]
  run jq '.engines.pi.windows.month.resets_at' "$cache"
  [ "$output" = "$reset_epoch" ]
}

@test "a user-layer keyFile is ignored" {
  keyfile="$BATS_TEST_TMPDIR/or-key"
  printf 'sk-or-v1-FILEKEY\n' >"$keyfile"
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"openrouter":{"keyFile":"%s"}}\n' "$keyfile" >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"pi spend unknown — set OPENROUTER_API_KEY"* ]]
  [[ "$output" == *"ignoring openrouter.keyFile"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  ! grep -q openrouter "$STUB_LOG"
}

# ---------------------------------------------------------------------------
# cursor (usage-summary probe)
# ---------------------------------------------------------------------------

@test "cursor individual plan: month window from the plan pools, cookie on stdin only" {
  cursor_auth accessToken
  cursor_config
  cursor_individual_usage
  auth_sum=$(sha256sum "$XDG_CONFIG_HOME/cursor/auth.json")
  conf_sum=$(sha256sum "$XDG_CONFIG_HOME/cursor/cli-config.json")
  start_epoch=$(jq -n '"2026-09-14T08:12:31Z" | fromdateiso8601')
  reset_epoch=$(jq -n '"2026-10-14T08:12:31Z" | fromdateiso8601')
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 63.5% used (resets "* ]]
  [[ "$output" != *"cursor quota unknown"* ]]
  [[ "$output" != *"SENTINELCURSORTOKEN"* ]]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  # max(auto 41.25, api 63.46), rounded to one decimal.
  run jq -e --argjson s "$start_epoch" --argjson r "$reset_epoch" '.engines.cursor == {
    source: "usage_summary", plan_type: "pro", credits_cover: false, unlimited: false,
    windows: {month: {used_pct: 63.5, starts_at: $s, resets_at: $r}}, limit_reached: null}' "$cache"
  [ "$status" -eq 0 ]
  grep -q "cursor_cookie_on_stdin=yes" "$STUB_LOG"
  run ! grep -q SENTINELCURSORTOKEN "$cache"
  run ! grep -q SENTINELCURSORTOKEN "$STUB_LOG"
  run ! grep -q cursor-agent-executed "$STUB_LOG"
  run ! grep -q '^security' "$STUB_LOG"
  [ "$(sha256sum "$XDG_CONFIG_HOME/cursor/auth.json")" = "$auth_sum" ]
  [ "$(sha256sum "$XDG_CONFIG_HOME/cursor/cli-config.json")" = "$conf_sum" ]
}

@test "cursor team plan: overall used/limit, epoch-millisecond dates, on-demand room covers" {
  cursor_auth accessToken
  cursor_config
  cursor_usage '{
    "billingCycleStart": 1788000000123,
    "billingCycleEnd": 1790592000999,
    "membershipType": "enterprise",
    "isUnlimited": false,
    "individualUsage": {"overall": {"enabled": true, "used": 30, "limit": 120, "remaining": 90}},
    "teamUsage": {"onDemand": {"enabled": true, "used": 5, "limit": 50, "remaining": 45}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [enterprise] month 25% used (resets "*") [credits cover]"* ]]
  run jq -e '.engines.cursor == {
    source: "usage_summary", plan_type: "enterprise", credits_cover: true, unlimited: false,
    windows: {month: {used_pct: 25, starts_at: 1788000000, resets_at: 1790592000}}, limit_reached: null}' \
    "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$status" -eq 0 ]
}

@test "cursor plan pool at 100% sets limit_reached, compared on the raw percentages" {
  cursor_auth accessToken
  cursor_config
  reset_epoch=$(jq -n '"2026-10-14T08:12:31Z" | fromdateiso8601')
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  # Whole-second Z and +00:00 both parse.
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31+00:00",
    "membershipType": "pro", "isUnlimited": false,
    "individualUsage": {"plan": {"enabled": true, "autoPercentUsed": 12, "apiPercentUsed": 100},
                        "onDemand": {"enabled": false}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 100% used (resets "*") [limit reached: plan usage at 100%]"* ]]
  run jq -e --argjson r "$reset_epoch" '.engines.cursor.limit_reached == {reason: "plan usage at 100%", resets_at: $r}
    and .engines.cursor.windows.month.resets_at == $r' "$cache"
  [ "$status" -eq 0 ]

  # 99.96 displays as 100 but has not reached the limit.
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31Z",
    "membershipType": "pro", "isUnlimited": false,
    "individualUsage": {"plan": {"enabled": true, "autoPercentUsed": 12, "apiPercentUsed": 99.96}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" != *"[limit reached:"* ]]
  run jq -e '.engines.cursor.windows.month.used_pct == 100 and .engines.cursor.limit_reached == null' "$cache"
  [ "$status" -eq 0 ]

  # Team shape: used_pct is the overall figure, yet an exhausted plan pool
  # still sets the limit.
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31Z",
    "membershipType": "enterprise", "isUnlimited": false,
    "individualUsage": {"overall": {"used": 48, "limit": 120}, "plan": {"apiPercentUsed": 100}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.windows.month.used_pct == 40
    and .engines.cursor.limit_reached.reason == "plan usage at 100%"' "$cache"
  [ "$status" -eq 0 ]
}

@test "cursor billing dates are both kept or both dropped, even at a 100% plan pool" {
  cursor_auth accessToken
  cursor_config
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  # A missing start, then a +02:00 start the parser doesn't accept: the valid
  # end must not survive alone as a half-sized window or a limit reset.
  for start in '' '"billingCycleStart": "2026-09-14T08:12:31+02:00",'; do
    cursor_usage '{
      '"$start"'
      "billingCycleEnd": "2026-10-14T08:12:31Z",
      "membershipType": "pro", "isUnlimited": false,
      "individualUsage": {"plan": {"enabled": true, "autoPercentUsed": 12, "apiPercentUsed": 100},
                          "onDemand": {"enabled": false}}
    }'
    run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    run jq -e '.engines.cursor.windows.month.starts_at == null
      and .engines.cursor.windows.month.resets_at == null
      and .engines.cursor.limit_reached.resets_at == null
      and .engines.cursor.limit_reached.reason == "plan usage at 100%"' "$cache"
    [ "$status" -eq 0 ]
  done
}

@test "cursor exhausted on-demand block sets limit_reached; a 0 or null limit never does" {
  cursor_auth accessToken
  cursor_config
  reset_epoch=$(jq -n '"2026-10-14T08:12:31Z" | fromdateiso8601')
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31Z",
    "membershipType": "enterprise", "isUnlimited": false,
    "individualUsage": {"overall": {"used": 72, "limit": 120},
                        "onDemand": {"enabled": true, "used": 50, "limit": 50}},
    "teamUsage": {"onDemand": {"enabled": false, "used": 0, "limit": null}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"[limit reached: on-demand limit reached]"* ]]
  run jq -e --argjson r "$reset_epoch" '.engines.cursor.windows.month.used_pct == 60
    and .engines.cursor.limit_reached == {reason: "on-demand limit reached", resets_at: $r}
    and .engines.cursor.credits_cover == false' "$cache"
  [ "$status" -eq 0 ]

  # A 0 limit is "off": neither exhausted nor cover.
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31Z",
    "membershipType": "enterprise", "isUnlimited": false,
    "individualUsage": {"overall": {"used": 72, "limit": 120},
                        "onDemand": {"enabled": true, "used": 50, "limit": 0}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.limit_reached == null and .engines.cursor.credits_cover == false' "$cache"
  [ "$status" -eq 0 ]

  # A null limit is uncapped: never exhausted, and it covers.
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31Z",
    "membershipType": "enterprise", "isUnlimited": false,
    "individualUsage": {"overall": {"used": 72, "limit": 120},
                        "onDemand": {"enabled": true, "used": 50, "limit": null}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.limit_reached == null and .engines.cursor.credits_cover == true' "$cache"
  [ "$status" -eq 0 ]
}

@test "cursor unlimited plan records no window and no limit, with or without percentages" {
  cursor_auth accessToken
  cursor_config
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31Z",
    "membershipType": "enterprise", "isUnlimited": true,
    "individualUsage": {"plan": {"autoPercentUsed": 100, "apiPercentUsed": 100}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [enterprise] unlimited"* ]]
  [[ "$output" != *"[limit reached:"* ]]
  run jq -e '.engines.cursor.unlimited == true and .engines.cursor.windows == {}
    and .engines.cursor.limit_reached == null' "$cache"
  [ "$status" -eq 0 ]

  cursor_usage '{"membershipType": "enterprise", "isUnlimited": true}'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [enterprise] unlimited"* ]]
  run jq -e '.engines.cursor.unlimited == true and .engines.cursor.windows == {}
    and .engines.cursor.limit_reached == null' "$cache"
  [ "$status" -eq 0 ]
}

@test "a failed usage-summary call leaves cursor unknown" {
  cursor_auth accessToken
  cursor_config
  cursor_individual_usage
  SHIM_CURSOR_FAIL=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — usage-summary call failed"* ]]
  [[ "$output" == *"cursor: unknown"* ]]
  [[ "$output" != *"SENTINELCURSORTOKEN"* ]]
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "no cursor auth.json leaves cursor unknown without calling usage-summary" {
  cursor_config
  cursor_individual_usage
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — no usable cursor-agent access token"* ]]
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  run ! grep -q usage-summary "$STUB_LOG"
}

@test "an auth.json with no candidate token field leaves cursor unknown" {
  cursor_auth refreshToken
  cursor_config
  cursor_individual_usage
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — no usable cursor-agent access token"* ]]
  [[ "$output" != *"SENTINELCURSORTOKEN"* ]]
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  run ! grep -q usage-summary "$STUB_LOG"
}

@test "the access_token field is a token candidate too" {
  cursor_auth access_token
  cursor_config
  cursor_individual_usage
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 63.5% used"* ]]
  grep -q "cursor_cookie_on_stdin=yes" "$STUB_LOG"
}

@test "a token outside the JWT charset never reaches the curl config" {
  cursor_config
  cursor_individual_usage
  for bad in 'SENTINEL"CURSORTOKEN' 'SENTINEL CURSORTOKEN'; do
    : >"$STUB_LOG"
    cursor_auth accessToken "$bad"
    run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [[ "$output" == *"cursor quota unknown — no usable cursor-agent access token"* ]]
    [[ "$output" != *"CURSORTOKEN"* ]]
    run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
    [ "$output" = "null" ]
    run ! grep -q usage-summary "$STUB_LOG"
  done
}

@test "with no cli-config.json the account id comes from the JWT sub" {
  jwt=$(cursor_jwt 'auth0|user_TESTACCOUNT')
  cursor_auth accessToken "$jwt"
  cursor_individual_usage
  SHIM_CURSOR_EXPECT="user_TESTACCOUNT::$jwt" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 63.5% used"* ]]
  [[ "$output" != *"SENTINELSIG"* ]]
  grep -q "cursor_cookie_on_stdin=yes" "$STUB_LOG"
  run ! grep -q SENTINELSIG "$STUB_LOG"
  run ! grep -q SENTINELSIG "$XDG_DATA_HOME/crew/engine-budget.json"
}

@test "no usable cursor account id leaves cursor unknown without calling usage-summary" {
  cursor_individual_usage
  # No cli-config and a token with no JWT payload to decode.
  cursor_auth accessToken
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — no cursor account id"* ]]
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  run ! grep -q usage-summary "$STUB_LOG"

  # An account id outside the charset after the prefix strip.
  mkdir -p "$XDG_CONFIG_HOME/cursor"
  printf '%s\n' '{"authInfo":{"authId":"auth0|user TESTACCOUNT"}}' >"$XDG_CONFIG_HOME/cursor/cli-config.json"
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — no cursor account id"* ]]
  run ! grep -q usage-summary "$STUB_LOG"
}

@test "an unrecognised usage-summary response leaves cursor unknown" {
  cursor_auth accessToken
  cursor_config
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  cursor_usage '<html>sign in</html>'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — usage-summary response not recognised"* ]]
  run jq '.engines.cursor' "$cache"
  [ "$output" = "null" ]

  # JSON, not unlimited, but no percentage — even with an exhausted-looking
  # on-demand block, the shape is unrecognised.
  cursor_usage '{
    "billingCycleStart": "2026-09-14T08:12:31Z", "billingCycleEnd": "2026-10-14T08:12:31Z",
    "membershipType": "pro", "isUnlimited": false,
    "individualUsage": {"plan": {"enabled": true, "totalPercentUsed": 50}},
    "teamUsage": {"onDemand": {"enabled": true, "used": 50, "limit": 50}}
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — usage-summary response not recognised"* ]]
  run jq '.engines.cursor' "$cache"
  [ "$output" = "null" ]
}

@test "no cursor-agent CLI leaves cursor unknown without reading auth or calling out" {
  cursor_auth accessToken
  cursor_config
  cursor_individual_usage
  rm "$STUB_DIR/cursor-agent"
  PATH="$(path_without_real cursor-agent)" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown (no cursor-agent CLI)"* ]]
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  run ! grep -q usage-summary "$STUB_LOG"
}

@test "macOS reads the token from the keychain item and cli-config from ~/.cursor" {
  cursor_config "$HOME/.cursor"
  cursor_individual_usage
  SHIM_UNAME=Darwin SHIM_SECURITY_TOKEN=SENTINELCURSORTOKEN run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 63.5% used"* ]]
  [[ "$output" != *"SENTINELCURSORTOKEN"* ]]
  grep -q "cursor_cookie_on_stdin=yes" "$STUB_LOG"
  grep -qx "security find-generic-password -s cursor-access-token -a cursor-user -w" "$STUB_LOG"
  run grep -c '^security ' "$STUB_LOG"
  [ "$output" = "1" ]
  run ! grep -q SENTINELCURSORTOKEN "$STUB_LOG"
  run ! grep -q SENTINELCURSORTOKEN "$XDG_DATA_HOME/crew/engine-budget.json"
}

@test "macOS with a denied keychain leaves cursor unknown without calling usage-summary" {
  cursor_config "$HOME/.cursor"
  cursor_individual_usage
  SHIM_UNAME=Darwin run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — no usable cursor-agent access token"* ]]
  grep -qx "security find-generic-password -s cursor-access-token -a cursor-user -w" "$STUB_LOG"
  run ! grep -q 'cursor.com/api/usage-summary' "$STUB_LOG"
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

# ---------------------------------------------------------------------------
# --report / --report --json (render-only, no probing)
# ---------------------------------------------------------------------------

@test "--report renders the cached lever with no probing" {
  mkdir -p "$XDG_DATA_HOME/crew"
  SHIM_NOW=1700000000
  export SHIM_NOW
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<EOF
{
  "fetched_at": "2026-01-01T00:00:00Z",
  "fetched_epoch": $SHIM_NOW,
  "engines": {
    "claude": {
      "source": "oauth_usage",
      "plan_type": null,
      "credits_cover": true,
      "windows": {
        "7d": {"used_pct": 90, "resets_at": $((SHIM_NOW + 302400))},
        "5h": {"used_pct": 10, "resets_at": $((SHIM_NOW + 5000))}
      }
    },
    "codex": null,
    "cursor": null,
    "pi": null
  }
}
EOF
  run --separate-stderr bash "$SCRIPT" --report
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude: "* ]]
  [ "$stderr" = "refresh-budget: budget lever: claude 7d at 90% (resets in 3d 12h, 40 points ahead of pace) — real budget: prefer a cheaper burn class or rotate engines" ]
  [ ! -s "$STUB_LOG" ]
  [[ "$output" != *"engine-budget.json"* ]]
}

@test "--report --json carries ahead_pts, resets_in_s, and verdict per window" {
  mkdir -p "$XDG_DATA_HOME/crew"
  SHIM_NOW=1700000000
  export SHIM_NOW
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<EOF
{
  "fetched_at": "2026-01-01T00:00:00Z",
  "fetched_epoch": $SHIM_NOW,
  "engines": {
    "claude": {
      "source": "oauth_usage",
      "plan_type": null,
      "credits_cover": true,
      "windows": {
        "7d": {"used_pct": 90, "resets_at": $((SHIM_NOW + 302400))},
        "5h": {"used_pct": 10, "resets_at": $((SHIM_NOW + 5000))}
      }
    },
    "codex": null,
    "cursor": null,
    "pi": null
  }
}
EOF
  run bash "$SCRIPT" --report --json
  [ "$status" -eq 0 ]
  json_out="$output"
  run jq -e '.engines.claude.windows[] | select(.key=="7d") | .ahead_pts == 40 and .resets_in_s == 302400 and (.verdict | startswith("real budget"))' <<<"$json_out"
  [ "$status" -eq 0 ]
  run jq -e '.engines.claude.windows[] | select(.key=="5h") | .verdict == null and .ahead_pts == null' <<<"$json_out"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor == null' <<<"$json_out"
  [ "$status" -eq 0 ]
  run jq -e --argjson fe "$SHIM_NOW" '.fetched_epoch == $fe' <<<"$json_out"
  [ "$status" -eq 0 ]
}

@test "--report --json matches the text lever's pi projection line, and is null under target" {
  mkdir -p "$XDG_DATA_HOME/crew"
  SHIM_NOW=1700000000
  export SHIM_NOW
  start_epoch=$((SHIM_NOW - 900000))
  reset_epoch=$((SHIM_NOW + 1800000))
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<EOF
{
  "fetched_at": "2026-01-01T00:00:00Z",
  "fetched_epoch": $SHIM_NOW,
  "engines": {
    "claude": null,
    "codex": null,
    "cursor": null,
    "pi": {
      "source": "openrouter_key",
      "plan_type": null,
      "credits_cover": null,
      "spend_usd": 10,
      "target_usd": 50,
      "elapsed_pct": 5,
      "projected_month_end_usd": 200,
      "key_limit_usd": null,
      "key_limit_remaining_usd": null,
      "starts_at": $start_epoch,
      "resets_at": $reset_epoch,
      "windows": {
        "month": {"used_pct": 20, "starts_at": $start_epoch, "resets_at": $reset_epoch}
      }
    }
  }
}
EOF
  run --separate-stderr bash "$SCRIPT" --report
  [ "$status" -eq 0 ]
  [ "$stderr" = "refresh-budget: budget lever: pi projected \$200.00 at month end, over the \$50.00 monthly target — size pi fan-out down" ]
  lever_line="${stderr#refresh-budget: budget lever: }"

  run bash "$SCRIPT" --report --json
  [ "$status" -eq 0 ]
  run jq -e --arg p "$lever_line" '
    .engines.pi.spend_usd == 10 and .engines.pi.target_usd == 50 and
    .engines.pi.elapsed_pct == 5 and .engines.pi.projected_month_end_usd == 200 and
    .engines.pi.projection == $p
  ' <<<"$output"
  [ "$status" -eq 0 ]

  # Under target: no projection line, and the json field is null.
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<EOF
{
  "fetched_at": "2026-01-01T00:00:00Z",
  "fetched_epoch": $SHIM_NOW,
  "engines": {
    "claude": null,
    "codex": null,
    "cursor": null,
    "pi": {
      "source": "openrouter_key",
      "plan_type": null,
      "credits_cover": null,
      "spend_usd": 10,
      "target_usd": 50,
      "elapsed_pct": 30,
      "projected_month_end_usd": 30,
      "key_limit_usd": null,
      "key_limit_remaining_usd": null,
      "starts_at": $start_epoch,
      "resets_at": $reset_epoch,
      "windows": {
        "month": {"used_pct": 20, "starts_at": $start_epoch, "resets_at": $reset_epoch}
      }
    }
  }
}
EOF
  run --separate-stderr bash "$SCRIPT" --report
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  run bash "$SCRIPT" --report --json
  [ "$status" -eq 0 ]
  run jq -e '.engines.pi.projection == null' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "--report gives a cursor month at 85-95% the plan-quota advice, and pi keeps its own" {
  mkdir -p "$XDG_DATA_HOME/crew"
  SHIM_NOW=1700000000
  export SHIM_NOW
  # A 30-day billing cycle, half elapsed: 90% used is 40 points ahead.
  start_epoch=$((SHIM_NOW - 1296000))
  reset_epoch=$((SHIM_NOW + 1296000))
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<EOF
{
  "fetched_at": "2026-01-01T00:00:00Z",
  "fetched_epoch": $SHIM_NOW,
  "engines": {
    "claude": null,
    "codex": null,
    "cursor": {
      "source": "usage_summary",
      "plan_type": "pro",
      "credits_cover": false,
      "unlimited": false,
      "windows": {
        "month": {"used_pct": 90, "starts_at": $start_epoch, "resets_at": $reset_epoch}
      },
      "limit_reached": null
    },
    "pi": {
      "source": "openrouter_key",
      "plan_type": null,
      "credits_cover": null,
      "spend_usd": 45,
      "target_usd": 50,
      "elapsed_pct": 50,
      "projected_month_end_usd": null,
      "key_limit_usd": null,
      "key_limit_remaining_usd": null,
      "starts_at": $start_epoch,
      "resets_at": $reset_epoch,
      "windows": {
        "month": {"used_pct": 90, "starts_at": $start_epoch, "resets_at": $reset_epoch}
      }
    }
  }
}
EOF
  run --separate-stderr bash "$SCRIPT" --report
  [ "$status" -eq 0 ]
  [[ "$stderr" == *"budget lever: cursor month at 90% (resets in 15d 0h, 40 points ahead of pace) — monthly plan quota: prefer a cheaper burn class or rotate engines"* ]]
  [[ "$stderr" == *"budget lever: pi month at 90% (resets in 15d 0h, 40 points ahead of pace) — monthly spend target: keep standard/trivial work off pi and shed pi fan-out"* ]]
  [[ "$output" == *"cursor: [pro] month 90% used (resets "* ]]

  run --separate-stderr bash "$SCRIPT" --report --json
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  json_out="$output"
  run jq -e '.engines.cursor.windows[] | select(.key == "month")
    | .ahead_pts == 40 and .resets_in_s == 1296000
      and .verdict == "monthly plan quota: prefer a cheaper burn class or rotate engines"' <<<"$json_out"
  [ "$status" -eq 0 ]
  run jq -e '.engines.pi.windows[] | select(.key == "month") | .verdict | startswith("monthly spend target")' <<<"$json_out"
  [ "$status" -eq 0 ]
}

@test "--report --json carries limit_reached for codex/cursor and unlimited for an unlimited plan" {
  mkdir -p "$XDG_DATA_HOME/crew"
  SHIM_NOW=1700000000
  export SHIM_NOW
  start_epoch=$((SHIM_NOW - 1296000))
  reset_epoch=$((SHIM_NOW + 1296000))
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<EOF
{
  "fetched_at": "2026-01-01T00:00:00Z",
  "fetched_epoch": $SHIM_NOW,
  "engines": {
    "claude": null,
    "codex": {
      "source": "app-server",
      "credits_cover": true,
      "plan_type": "team",
      "windows": {"5h": {"used_pct": 100, "resets_at": $reset_epoch}},
      "limit_reached": {
        "ordinary_usage_allowed": false,
        "rate_limit_reached_type": "primary",
        "spend_control_reached": null,
        "credits_unlimited": false,
        "credits_balance": "0",
        "individual_remaining_percent": null,
        "individual_resets_at": null
      }
    },
    "cursor": {
      "source": "usage_summary",
      "plan_type": "pro",
      "credits_cover": false,
      "unlimited": false,
      "windows": {"month": {"used_pct": 100, "starts_at": $start_epoch, "resets_at": $reset_epoch}},
      "limit_reached": {"reason": "plan usage at 100%", "resets_at": $reset_epoch}
    },
    "pi": null
  }
}
EOF
  run bash "$SCRIPT" --report --json
  [ "$status" -eq 0 ]
  run jq -e --argjson r "$reset_epoch" '
    .engines.codex.limit_reached.ordinary_usage_allowed == false and
    .engines.codex.limit_reached.rate_limit_reached_type == "primary" and
    .engines.codex.limit_reached.credits_balance == "0" and
    .engines.codex.unlimited == false and
    .engines.cursor.limit_reached == {reason: "plan usage at 100%", resets_at: $r} and
    .engines.cursor.unlimited == false
  ' <<<"$output"
  [ "$status" -eq 0 ]

  # An engine with no unlimited field reads as false; a null limit_reached
  # stays null; an unlimited plan flips unlimited true and drops its window.
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<EOF
{
  "fetched_at": "2026-01-01T00:00:00Z",
  "fetched_epoch": $SHIM_NOW,
  "engines": {
    "claude": null,
    "codex": {
      "source": "app-server",
      "credits_cover": false,
      "plan_type": null,
      "windows": {},
      "limit_reached": null
    },
    "cursor": {
      "source": "usage_summary",
      "plan_type": "enterprise",
      "credits_cover": true,
      "unlimited": true,
      "windows": {},
      "limit_reached": null
    },
    "pi": null
  }
}
EOF
  run bash "$SCRIPT" --report --json
  [ "$status" -eq 0 ]
  run jq -e '.engines.codex.unlimited == false and .engines.codex.limit_reached == null
    and .engines.cursor.unlimited == true and .engines.cursor.windows == []
    and .engines.cursor.limit_reached == null' <<<"$output"
  [ "$status" -eq 0 ]
}

@test "--report with no cached budget exits 1" {
  run --separate-stderr bash "$SCRIPT" --report
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"no cached budget at"* ]]
}

@test "--report skips the settings call, even when dispatch-config is broken" {
  mkdir -p "$XDG_DATA_HOME/crew"
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<'EOF'
{"fetched_at":"2026-01-01T00:00:00Z","fetched_epoch":1700000000,"engines":{"claude":null,"codex":null,"cursor":null,"pi":null}}
EOF
  DISPATCH_CONFIG_BIN=false run bash "$SCRIPT" --report
  [ "$status" -eq 0 ]
}

@test "--report usage: --json alone or an unknown flag exits 2" {
  run --separate-stderr bash "$SCRIPT" --json
  [ "$status" -eq 2 ]
  [[ "$stderr" == usage:* ]]

  run --separate-stderr bash "$SCRIPT" --bogus
  [ "$status" -eq 2 ]
  [[ "$stderr" == usage:* ]]
}

# write_pi_auth <json> — a synthetic pi auth store under the throwaway HOME.
write_pi_auth() {
  mkdir -p "$HOME/.pi/agent"
  printf '%s\n' "$1" >"$HOME/.pi/agent/auth.json"
}

@test "pi's auth store is the last-resort OpenRouter key, reaching curl only on stdin" {
  or_key_fixture 10
  write_pi_auth '{"openrouter":{"type":"oauth","access":"sk-or-v1-TESTSENTINELPI"}}'
  SHIM_OR_EXPECT_KEY=sk-or-v1-TESTSENTINELPI run --separate-stderr bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output$stderr" != *"TESTSENTINELPI"* ]]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
  run jq -r '.engines.pi.source' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "openrouter_key" ]
  run ! grep -q "TESTSENTINELPI" "$STUB_LOG"
  run ! grep -rq "TESTSENTINELPI" "$XDG_DATA_HOME/crew"
}

@test "PI_CODING_AGENT_DIR relocates pi's auth store" {
  or_key_fixture 10
  dir="$BATS_TEST_TMPDIR/piagent"
  mkdir -p "$dir"
  printf '%s\n' '{"openrouter":{"type":"oauth","access":"sk-or-v1-TESTSENTINELDIR"}}' >"$dir/auth.json"
  PI_CODING_AGENT_DIR="$dir" SHIM_OR_EXPECT_KEY=sk-or-v1-TESTSENTINELDIR run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
}

@test "OPENROUTER_API_KEY wins over pi's auth store" {
  or_key_fixture 10
  write_pi_auth '{"openrouter":{"type":"oauth","access":"sk-or-v1-TESTSENTINELPI"}}'
  OPENROUTER_API_KEY=sk-or-v1-TESTSENTINELENV SHIM_OR_EXPECT_KEY=sk-or-v1-TESTSENTINELENV \
    run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
}

@test "the key file is exclusive: pi's auth store is not consulted" {
  or_key_fixture 10
  write_pi_auth '{"openrouter":{"type":"oauth","access":"sk-or-v1-TESTSENTINELPI"}}'
  keyfile="$BATS_TEST_TMPDIR/missing-key"
  DISPATCH_OPENROUTER_KEY_FILE="$keyfile" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  ! grep -q openrouter "$STUB_LOG"
}

@test "a pi auth store without a usable key leaves pi unknown" {
  or_key_fixture 10
  for body in '{"openrouter":{"type":"oauth","access":"not-a-key"}}' 'not json' '{"openrouter":{"type":"api_key","access":"sk-or-v1-TESTSENTINELPI"}}' '{"openrouter":{"access":"sk-or-v1-TESTSENTINELPI"}}' '{"openrouter":{}}' '{"other":{"access":"sk-or-v1-TESTSENTINELPI"}}' '{"openrouter":{"type":"oauth","access":"sk-or-v1-bad key"}}'; do
    write_pi_auth "$body"
    run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [[ "$output" == *"log pi in to OpenRouter"* ]]
    run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
    [ "$output" = "null" ]
  done
  ! grep -q openrouter "$STUB_LOG"
}

@test "an exhausted key credit limit sets pi limit_reached and the summary says so" {
  jq -n '{data: {usage_monthly: 10, limit: 10, limit_remaining: 0}}' >"$FIXTURE_DIR/or_key.json"
  OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -r '.engines.pi.limit_reached.reason' "$XDG_DATA_HOME/crew/engine-budget.json"
  [[ "$output" == *"credit limit reached"* ]]
  run bash "$SCRIPT" --report
  [[ "$output" == *"pi: openrouter"*"LIMIT REACHED"* ]]
}

@test "a null or positive key credit limit sets no pi limit_reached" {
  for body in '{"data":{"usage_monthly":10,"limit":null,"limit_remaining":null}}' '{"data":{"usage_monthly":10,"limit":10,"limit_remaining":3}}'; do
    printf '%s\n' "$body" >"$FIXTURE_DIR/or_key.json"
    OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    run jq '.engines.pi.limit_reached' "$XDG_DATA_HOME/crew/engine-budget.json"
    [ "$output" = "null" ]
  done
}

@test "a monthly key limit becomes the target when no config target is set" {
  jq -n '{data: {usage_monthly: 10, limit: 40, limit_remaining: 30, limit_reset: "monthly"}}' >"$FIXTURE_DIR/or_key.json"
  OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"of \$40.00 monthly target (key limit)"* ]]
  [[ "$output" == *"[key limit resets: monthly]"* ]]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq -r '[.engines.pi.target_usd, .engines.pi.target_source, .engines.pi.limit_reset, (.engines.pi.windows.month.used_pct)] | @tsv' "$cache"
  [ "$output" = "$(printf '40\tkey_limit\tmonthly\t25')" ]
}

@test "a configured target overrides the key limit" {
  jq -n '{data: {usage_monthly: 10, limit: 40, limit_remaining: 30, limit_reset: "monthly"}}' >"$FIXTURE_DIR/or_key.json"
  DISPATCH_OPENROUTER_MONTHLY_USD=50 OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -r '[.engines.pi.target_usd, .engines.pi.target_source] | @tsv' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "$(printf '50\tconfig')" ]
  [[ "$(bash "$SCRIPT" --report 2>&1)" != *"(key limit)"* ]]
  run bash "$SCRIPT" --report --json
  run jq -r '.engines.pi | [.target_source, .limit_reset] | @tsv' <<<"$output"
  [ "$output" = "$(printf 'config\tmonthly')" ]
}

@test "a monthly reset with a null or non-positive limit sets no target" {
  for lim in null 0 -5; do
    jq -n --argjson l "$lim" '{data: {usage_monthly: 10, limit: $l, limit_remaining: 5, limit_reset: "monthly"}}' >"$FIXTURE_DIR/or_key.json"
    OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    run jq -r '.engines.pi.target_usd | tostring' "$XDG_DATA_HOME/crew/engine-budget.json"
    [ "$output" = "null" ]
  done
}

@test "a non-monthly or null limit_reset sets no target" {
  for reset in '"weekly"' '"daily"' 'null'; do
    jq -n --argjson r "$reset" '{data: {usage_monthly: 10, limit: 40, limit_remaining: 30, limit_reset: $r}}' >"$FIXTURE_DIR/or_key.json"
    OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    run jq -r '[(.engines.pi.target_usd | tostring), (.engines.pi.target_source | tostring), (.engines.pi.windows | length | tostring)] | @tsv' "$XDG_DATA_HOME/crew/engine-budget.json"
    [ "$output" = "$(printf 'null\tnull\t0')" ]
  done
}

@test "limit_remaining <= 0 trips limit_reached whatever the reset type" {
  for reset in '"monthly"' '"weekly"' 'null'; do
    jq -n --argjson r "$reset" '{data: {usage_monthly: 10, limit: 10, limit_remaining: 0, limit_reset: $r}}' >"$FIXTURE_DIR/or_key.json"
    OPENROUTER_API_KEY=sk-or-v1-SENTINELKEY123 run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    run jq -e '.engines.pi.limit_reached.reason != null' "$XDG_DATA_HOME/crew/engine-budget.json"
    [ "$status" -eq 0 ]
  done
}
