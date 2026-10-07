bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  SCRIPT="$BATS_TEST_DIRNAME/../adapters/core/refresh-budget.sh"
  STUB_DIR="$(mktemp -d)"
  STUB_LOG="$STUB_DIR/calls.log"
  FIXTURE_DIR="$(mktemp -d)"
  PI_LOG="$STUB_DIR/pi.log"
  export STUB_DIR STUB_LOG FIXTURE_DIR PI_LOG
  # Resolved before $STUB_DIR is prepended to PATH, so the date shim below
  # can fall through to the genuine binary for every call it doesn't fake.
  REAL_DATE="$(command -v date)"
  export REAL_DATE
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
  write_pi_shim
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

# write_pi_shim — a fail-closed `pi`: answers `--version` ($SHIM_PI_VERSION,
# default 1.0.2) and `auth print-api-key --provider openrouter` (with
# $SHIM_PI_KEY when set, after sleeping $SHIM_PI_SLEEP seconds when set);
# everything else exits $SHIM_PI_RC (default 1). Logs to $PI_LOG,
# never $STUB_LOG. The real pi must never be reachable from these tests.
write_pi_shim() {
  cat >"$STUB_DIR/pi" <<'EOF'
#!/usr/bin/env bash
printf 'pi %s\n' "$*" >>"$PI_LOG"
[[ "$1" == auth ]] && printf 'pi-cwd %s\n' "$PWD" >>"$PI_LOG"
if [[ "$*" == "--version" ]]; then
  printf '%s\n' "${SHIM_PI_VERSION:-1.0.2}"
  exit 0
fi
if [[ -n "${SHIM_PI_SLEEP:-}" && "$*" == "auth print-api-key --provider openrouter" ]]; then
  exec sleep "$SHIM_PI_SLEEP"
fi
if [[ -n "${SHIM_PI_KEY:-}" && "$*" == "auth print-api-key --provider openrouter" ]]; then
  printf '%s\n' "$SHIM_PI_KEY"
  exit 0
fi
echo "pi stub: unsupported" >&2
exit "${SHIM_PI_RC:-1}"
EOF
  chmod +x "$STUB_DIR/pi"
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

# Fake tmux: list-windows / list-panes / capture-pane / run-shell, dispatched
# on $1. run-shell echoes $SHIM_TMUX_RUNSHELL (the cursor tool lookup) and
# logs its arguments.
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
run-shell)
  printf 'tmux %s\n' "$*" >>"$STUB_LOG"
  [ -n "${SHIM_TMUX_RUNSHELL:-}" ] || exit 1
  printf '%s\n' "$SHIM_TMUX_RUNSHELL"
  ;;
*) exit 1 ;;
esac
EOF
  chmod +x "$STUB_DIR/tmux"
}

# write_cursor_shims — probe_cursor's host edge, inert by default: a
# `tmux-agent-usage-cursor` that logs "cursor-print <args>", exits
# $SHIM_CURSOR_RC when set, and otherwise prints $FIXTURE_DIR/cursor_print.json
# (exit 4, a failed fetch, when no test has written one). The real tool must
# never be reachable from these tests; a test that needs it absent strips it
# with path_without_real and removes this stub.
write_cursor_shims() {
  cat >"$STUB_DIR/tmux-agent-usage-cursor" <<'EOF'
#!/usr/bin/env bash
printf 'cursor-print %s\n' "$*" >>"$STUB_LOG"
[[ -z "${SHIM_CURSOR_RC:-}" || "$SHIM_CURSOR_RC" == 0 ]] || exit "$SHIM_CURSOR_RC"
[[ -f "$FIXTURE_DIR/cursor_print.json" ]] || exit 4
cat "$FIXTURE_DIR/cursor_print.json"
EOF
  chmod +x "$STUB_DIR/tmux-agent-usage-cursor"
}

# cursor_auth <field> [token] — a sentinel Linux auth.json holding <token>
# (default the sentinel) under <field>. The probe must never read or write it.
cursor_auth() {
  mkdir -p "$XDG_CONFIG_HOME/cursor"
  jq -n --arg f "$1" --arg t "${2:-SENTINELCURSORTOKEN}" '{($f): $t}' >"$XDG_CONFIG_HOME/cursor/auth.json"
}

# cursor_config — a sentinel cli-config.json, likewise never touched.
cursor_config() {
  mkdir -p "$XDG_CONFIG_HOME/cursor"
  printf '%s\n' '{"version":1,"authInfo":{"authId":"auth0|user_TESTACCOUNT"}}' >"$XDG_CONFIG_HOME/cursor/cli-config.json"
}

# cursor_print <json> — the stubbed `--print` stdout.
cursor_print() {
  printf '%s\n' "$1" >"$FIXTURE_DIR/cursor_print.json"
}

# cursor_individual_print — an individual-plan `--print` object: two plan
# pools, no plan dollars, on-demand off. The cycle is 2026-09-14T08:12:31Z to
# 2026-10-14T08:12:31Z.
cursor_individual_print() {
  cursor_print '{
    "plan_type": "pro", "unlimited": false,
    "cycle": {"starts_at": 1789373551, "resets_at": 1791965551},
    "plan": {"used_pct": null, "used_usd": null, "limit_usd": null},
    "pools": {"auto_pct": 41.25, "api_pct": 63.46},
    "on_demand": [{"scope": "individual", "enabled": false, "used_usd": 0, "limit_usd": null}]
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

# keep_row runs one row under set -e and keeps going. finish_rows fails once,
# naming every row that failed. A short read must not pass with zero rows.
begin_rows() {
  ROW_FAILS=()
  ROW_N=0
}

keep_row() {
  local id=$1 err rc
  shift
  local -a cmd=("$@")
  ROW_N=$((ROW_N + 1))
  set +e
  err=$(
    set -e
    trap - ERR
    "${cmd[@]}" 2>&1
  )
  rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    ROW_FAILS+=("$id")
    printf 'row %s failed\n' "$id" >&2
    if [ -n "$err" ]; then
      printf '%s\n' "$err" >&2
    fi
    BATS_ERROR_STATUS=
    BATS_ERROR_SUFFIX=
  fi
}

finish_rows() {
  local want=$1
  if [ "$ROW_N" -ne "$want" ]; then
    printf 'expected %s rows, ran %s\n' "$want" "$ROW_N" >&2
    return 1
  fi
  if [ "${#ROW_FAILS[@]}" -gt 0 ]; then
    printf 'failed rows: %s\n' "${ROW_FAILS[*]}" >&2
    return 1
  fi
}


# Pane-scrape rows share the quota-unknown or used_pct assertions. Each row
# gets its own XDG_DATA_HOME so a previous row's engine-budget.json cannot
# satisfy the next. The tmux/curl shims come from setup(); these rows do not
# call stub_bin, and they do not change PATH (the row body runs in keep_row's
# subshell).
quota_unknown_row() { # windows panes capture id
  local windows panes capture
  windows=$(printf '%b' "$1")
  panes=$(printf '%b' "$2")
  capture=$(printf '%b' "$3")
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data-$4"
  mkdir -p "$XDG_DATA_HOME"
  SHIM_TMUX_WINDOWS="$windows" \
    SHIM_TMUX_PANES="$panes" \
    SHIM_TMUX_CAPTURE_P10="$capture" \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"claude quota unknown"* ]]
  run jq '.engines.claude' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

used_pct_row() { # pct capture id
  local capture
  capture=$(printf '%b' "$2")
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data-$3"
  mkdir -p "$XDG_DATA_HOME"
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10="$capture" \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.engines.claude.windows["5h"].used_pct' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "$1" ]
}

# 22350s / 2310s sit off the exact minute boundary so probe_codex's own later
# date cannot round the rendered reset down a minute.
codex_hold_row() { # mins resets fragment id
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data-$4"
  mkdir -p "$XDG_DATA_HOME"
  SHIM_CLAUDE_429=1 SHIM_CODEX_CUSTOM=1 SHIM_CODEX_USED_PCT=99 \
    SHIM_CODEX_WINDOW_MINS="$1" SHIM_CODEX_RESETS_IN="$2" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"$3"* ]]
}

# Offsets are seconds from now. 4d3h = 356400s (row day-form, checked on 7d).
# "08m" is 480s (row zero-pad, checked on 5h) — a leading zero must not crash.
countdown_row() { # window pct lo hi capture id
  local window=$1 pct=$2 lo=$3 hi=$4 capture now cache
  capture=$(printf '%b' "$5")
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data-$6"
  mkdir -p "$XDG_DATA_HOME"
  now=$(date +%s)
  SHIM_TMUX_WINDOWS=$'@1\tnova' \
    SHIM_TMUX_PANES=$'@1\t%10' \
    SHIM_TMUX_CAPTURE_P10="$capture" \
    SHIM_CLAUDE_429=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  run jq --arg w "$window" '.engines.claude.windows[$w].used_pct' "$cache"
  [ "$output" = "$pct" ]
  run jq --arg w "$window" '.engines.claude.windows[$w].resets_at' "$cache"
  [ "$output" -ge $((now + lo)) ]
  [ "$output" -le $((now + hi)) ]
}

# F69: captures that must not yield a quota. The oversized row is the value
# "10#$p" wraps to 42; a 4-digit run never matches the {1,3} marker. Per-row
# XDG_DATA_HOME (see quota_unknown_row).
@test "pane-scrape reports quota unknown for a non-reading capture" {
  begin_rows
  local row windows panes capture
  while IFS='|' read -r row windows panes capture; do
    [ -n "$row" ] || continue
    keep_row "$row" quota_unknown_row "$windows" "$panes" "$capture" "$row"
  done <<'ROWS'
dispatcher|@1\tdispatcher|@1\t%10|  ⚡ 97% (1m)
stray|@1\tnova|@1\t%10|Reading WORKER_TASK.md — example line: ⚡ 97% (2h53m → 00:20)\n  ⎿  Done (3 tool uses)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
oversize|@1\tnova|@1\t%10|  ⚡ 55340232221128654890% (10m)
four-digit|@1\tnova|@1\t%10|  ⚡ 1234% (10m)
ROWS
  finish_rows 4
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

# F70: one anchored 5h percentage. Per-row XDG_DATA_HOME (see used_pct_row).
@test "pane-scrape reads a single 5h percentage from the anchored statusline" {
  begin_rows
  local row pct capture
  while IFS='|' read -r row pct capture; do
    [ -n "$row" ] || continue
    keep_row "$row" used_pct_row "$pct" "$capture" "$row"
  done <<'ROWS'
no-subagents|31|  🤖 Sonnet 5 🧠 med | 📊 120k/1M | ⚡ 31% (1h0m → 13:10)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)
mode-glyph|44|  🤖 Sonnet 5 | ⚡ 44% (1h0m → 13:10)\n  -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle)\n\n  ● main\n  ◯ go-reviewer  saw ⏵⏵ in output        9s
ROWS
  finish_rows 2
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

# F73: unsized 1d window is unholdable; a sized 5h window inside its last 15%
# is holdable. Per-row XDG_DATA_HOME (see codex_hold_row).
@test "codex hold wording follows nominal length and the last 15 percent" {
  begin_rows
  local row mins resets fragment
  while IFS='|' read -r row mins resets fragment; do
    [ -n "$row" ] || continue
    keep_row "$row" codex_hold_row "$mins" "$resets" "$fragment" "$row"
  done <<'ROWS'
unsized|1440|22350|budget lever: codex 1d at 99% (resets in 6h 12m) — not holdable: window has no nominal length, hand the task back
holdable|300|2310|budget lever: codex 5h at 99% (resets in 38m) — binding window; holdable: inside the window's last 15%, wait past the reset
ROWS
  finish_rows 2
}

# F74: a day-form countdown and a zero-padded minute countdown. Per-row
# XDG_DATA_HOME (see countdown_row).
@test "pane-scrape parses a countdown into used_pct and resets_at" {
  begin_rows
  local row window pct lo hi capture
  while IFS='|' read -r row window pct lo hi capture; do
    [ -n "$row" ] || continue
    keep_row "$row" countdown_row "$window" "$pct" "$lo" "$hi" "$capture" "$row"
  done <<'ROWS'
day-form|7d|91|356280|356520|  ⚡ 50% (2h) 7d 91% (4d3h)
zero-pad|5h|42|420|540|  ⚡ 42% (08m)
ROWS
  finish_rows 2
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

# F75: padded, clamped, and one-digit 5h percentages. The one-digit row keeps
# the {1,3} marker from dropping the short end of the range. Per-row
# XDG_DATA_HOME (see used_pct_row).
@test "pane-scrape reads a padded clamped or one-digit 5h percentage" {
  begin_rows
  local row pct capture
  while IFS='|' read -r row pct capture; do
    [ -n "$row" ] || continue
    keep_row "$row" used_pct_row "$pct" "$capture" "$row"
  done <<'ROWS'
zero-padded|8|  ⚡ 08% (10m)
clamped|100|  ⚡ 999% (10m)
one-digit|5|  ⚡ 5% (10m)
ROWS
  finish_rows 3
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
  run ! grep -q SENTINELKEY123 "$cache"
  run ! grep -q SENTINELKEY123 "$STUB_LOG"
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
# cursor (delegated to tmux-agent-usage-cursor --print)
# ---------------------------------------------------------------------------

@test "cursor individual plan: month window from the pools, never touching the token" {
  cursor_auth accessToken
  cursor_config
  cursor_individual_print
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
  # max(auto 41.25, api 63.46), rounded to one decimal; no plan dollars, so no spend.
  run jq -e --argjson s "$start_epoch" --argjson r "$reset_epoch" '.engines.cursor == {
    source: "usage_summary", plan_type: "pro", credits_cover: false, unlimited: false,
    windows: {month: {used_pct: 63.5, starts_at: $s, resets_at: $r}}, limit_reached: null}' "$cache"
  [ "$status" -eq 0 ]
  grep -qx "cursor-print --print" "$STUB_LOG"
  run ! grep -q SENTINELCURSORTOKEN "$cache"
  run ! grep -q SENTINELCURSORTOKEN "$STUB_LOG"
  run ! grep -q '^security' "$STUB_LOG"
  run ! grep -q 'cursor.com' "$STUB_LOG"
  [ "$(sha256sum "$XDG_CONFIG_HOME/cursor/auth.json")" = "$auth_sum" ]
  [ "$(sha256sum "$XDG_CONFIG_HOME/cursor/cli-config.json")" = "$conf_sum" ]
}

@test "refresh-budget carries no cursor token, account or usage-summary code of its own" {
  run ! grep -E 'cursor\.com|WorkosCursor|cli-config|security find' "$SCRIPT"
  body=$(sed -n '/^probe_cursor()/,/^}/p' "$SCRIPT")
  [ -n "$body" ]
  run ! grep -E 'security|curl|auth\.json|cli-config|accessToken|cursor-agent|uname' <<<"$body"
}

@test "cursor team plan: dollars on the entry and the summary line, on-demand room covers" {
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 25, "used_usd": 30, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": null},
    "on_demand": [{"scope": "team", "enabled": true, "used_usd": 5, "limit_usd": 50}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [enterprise] month 25% used (\$30/\$120) (resets "*") [credits cover]"* ]]
  run jq -e '.engines.cursor == {
    source: "usage_summary", plan_type: "enterprise", credits_cover: true, unlimited: false,
    windows: {month: {used_pct: 25, starts_at: 1788000000, resets_at: 1790592000}}, limit_reached: null,
    spend: {used_usd: 30, limit_usd: 120}}' \
    "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$status" -eq 0 ]
}

@test "cursor enterprise plan: measured dollars round to whole dollars in the summary" {
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 38.669523809, "used_usd": 406.03, "limit_usd": 1050},
    "pools": {"auto_pct": null, "api_pct": null},
    "on_demand": [{"scope": "team", "enabled": true, "used_usd": 861.55, "limit_usd": 6000}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [enterprise] month 38.7% used (\$406/\$1050) (resets "* ]]
  run jq -e '.engines.cursor.spend == {used_usd: 406.03, limit_usd: 1050}
    and .engines.cursor.windows.month.used_pct == 38.7
    and .engines.cursor.credits_cover == true and .engines.cursor.limit_reached == null' \
    "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$status" -eq 0 ]
}

@test "cursor spend is omitted unless both plan dollar figures are numbers" {
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  for plan in '{"used_pct": 40, "used_usd": null, "limit_usd": null}' '{"used_pct": 40, "used_usd": 30}' '{"used_pct": 40, "used_usd": "30", "limit_usd": 120}'; do
    cursor_print '{
      "plan_type": "enterprise", "unlimited": false,
      "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
      "plan": '"$plan"',
      "pools": {"auto_pct": null, "api_pct": null}, "on_demand": []
    }'
    run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [[ "$output" == *"cursor: [enterprise] month 40% used (resets "* ]]
    run jq -e '.engines.cursor | has("spend") | not' "$cache"
    [ "$status" -eq 0 ]
  done
}

@test "cursor plan pool at 100% sets limit_reached, compared on the raw percentages" {
  reset_epoch=$(jq -n '"2026-10-14T08:12:31Z" | fromdateiso8601')
  start_epoch=$(jq -n '"2026-09-14T08:12:31Z" | fromdateiso8601')
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  cursor_print '{
    "plan_type": "pro", "unlimited": false,
    "cycle": {"starts_at": '"$start_epoch"', "resets_at": '"$reset_epoch"'},
    "plan": {"used_pct": null, "used_usd": null, "limit_usd": null},
    "pools": {"auto_pct": 12, "api_pct": 100},
    "on_demand": [{"scope": "individual", "enabled": false, "used_usd": 0, "limit_usd": null}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 100% used (resets "*") [limit reached: plan usage at 100%]"* ]]
  run jq -e --argjson r "$reset_epoch" '.engines.cursor.limit_reached == {reason: "plan usage at 100%", resets_at: $r}
    and .engines.cursor.windows.month.resets_at == $r' "$cache"
  [ "$status" -eq 0 ]

  # 99.96 displays as 100 but has not reached the limit.
  cursor_print '{
    "plan_type": "pro", "unlimited": false,
    "cycle": {"starts_at": '"$start_epoch"', "resets_at": '"$reset_epoch"'},
    "plan": {"used_pct": null}, "pools": {"auto_pct": 12, "api_pct": 99.96}, "on_demand": []
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" != *"[limit reached:"* ]]
  run jq -e '.engines.cursor.windows.month.used_pct == 100 and .engines.cursor.limit_reached == null' "$cache"
  [ "$status" -eq 0 ]

  # Team shape: used_pct is the plan figure, yet an exhausted pool still sets
  # the limit.
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": '"$start_epoch"', "resets_at": '"$reset_epoch"'},
    "plan": {"used_pct": 40, "used_usd": 48, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": 100}, "on_demand": []
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.windows.month.used_pct == 40
    and .engines.cursor.limit_reached.reason == "plan usage at 100%"' "$cache"
  [ "$status" -eq 0 ]

  # The plan percentage itself at 100.
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": '"$start_epoch"', "resets_at": '"$reset_epoch"'},
    "plan": {"used_pct": 100, "used_usd": 120, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": null}, "on_demand": []
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.limit_reached.reason == "plan usage at 100%"' "$cache"
  [ "$status" -eq 0 ]
}

@test "cursor billing cycle bounds are both kept or both dropped, even at a 100% plan pool" {
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  # One bound null, then an inverted cycle: the surviving bound must not
  # become a half-sized window or a limit reset.
  for cycle in '{"starts_at": null, "resets_at": 1790592000}' '{"starts_at": 1790592000, "resets_at": null}' '{"starts_at": 1790592000, "resets_at": 1788000000}' '{"starts_at": "x", "resets_at": 1790592000}' 'null'; do
    cursor_print '{
      "plan_type": "pro", "unlimited": false, "cycle": '"$cycle"',
      "plan": {"used_pct": null}, "pools": {"auto_pct": 12, "api_pct": 100},
      "on_demand": [{"scope": "individual", "enabled": false, "used_usd": 0, "limit_usd": null}]
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
  reset_epoch=1790592000
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 60, "used_usd": 72, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": null},
    "on_demand": [{"scope": "individual", "enabled": true, "used_usd": 50, "limit_usd": 50},
                  {"scope": "team", "enabled": false, "used_usd": 0, "limit_usd": null}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"[limit reached: on-demand limit reached]"* ]]
  run jq -e --argjson r "$reset_epoch" '.engines.cursor.windows.month.used_pct == 60
    and .engines.cursor.limit_reached == {reason: "on-demand limit reached", resets_at: $r}
    and .engines.cursor.credits_cover == false' "$cache"
  [ "$status" -eq 0 ]

  # A 0 limit is "off": neither exhausted nor cover.
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 60, "used_usd": 72, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": null},
    "on_demand": [{"scope": "individual", "enabled": true, "used_usd": 50, "limit_usd": 0}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.limit_reached == null and .engines.cursor.credits_cover == false' "$cache"
  [ "$status" -eq 0 ]

  # A null limit is uncapped: never exhausted, and it covers.
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 60, "used_usd": 72, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": null},
    "on_demand": [{"scope": "individual", "enabled": true, "used_usd": 50, "limit_usd": null}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.limit_reached == null and .engines.cursor.credits_cover == true' "$cache"
  [ "$status" -eq 0 ]

  # An enabled bounded pool with a null used_usd counts as unspent: it covers.
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 60, "used_usd": 72, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": null},
    "on_demand": [{"scope": "team", "enabled": true, "used_usd": null, "limit_usd": 50}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.limit_reached == null and .engines.cursor.credits_cover == true' "$cache"
  [ "$status" -eq 0 ]

  # A disabled pool never covers, whatever its numbers.
  cursor_print '{
    "plan_type": "enterprise", "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 60, "used_usd": 72, "limit_usd": 120},
    "pools": {"auto_pct": null, "api_pct": null},
    "on_demand": [{"scope": "team", "enabled": false, "used_usd": 1, "limit_usd": 50}]
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -e '.engines.cursor.limit_reached == null and .engines.cursor.credits_cover == false' "$cache"
  [ "$status" -eq 0 ]
}

@test "cursor unlimited plan records no window and no limit, with or without percentages" {
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  cursor_print '{
    "plan_type": "enterprise", "unlimited": true,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": null, "used_usd": null, "limit_usd": null},
    "pools": {"auto_pct": 100, "api_pct": 100}, "on_demand": []
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [enterprise] unlimited"* ]]
  [[ "$output" != *"[limit reached:"* ]]
  run jq -e '.engines.cursor.unlimited == true and .engines.cursor.windows == {}
    and .engines.cursor.limit_reached == null' "$cache"
  [ "$status" -eq 0 ]

  cursor_print '{"plan_type": "enterprise", "unlimited": true, "cycle": {"starts_at": null, "resets_at": null},
    "plan": {"used_pct": null, "used_usd": null, "limit_usd": null},
    "pools": {"auto_pct": null, "api_pct": null}, "on_demand": []}'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [enterprise] unlimited"* ]]
  run jq -e '.engines.cursor.unlimited == true and .engines.cursor.windows == {}
    and .engines.cursor.limit_reached == null' "$cache"
  [ "$status" -eq 0 ]
}

@test "a null plan_type is carried as null and left out of the summary prefix" {
  cursor_print '{
    "plan_type": null, "unlimited": false,
    "cycle": {"starts_at": 1788000000, "resets_at": 1790592000},
    "plan": {"used_pct": 10}, "pools": {"auto_pct": null, "api_pct": null}, "on_demand": []
  }'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: month 10% used (resets "* ]]
  run jq -e '.engines.cursor.plan_type == null' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$status" -eq 0 ]
}

@test "each --print failure exit code maps to its warning and leaves cursor unknown" {
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  cursor_individual_print
  for pair in '2:no Cursor access token' '3:no Cursor account id' '4:usage-summary call failed' '5:response not recognised'; do
    SHIM_CURSOR_RC="${pair%%:*}" run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [[ "$output" == *"cursor quota unknown — "*"${pair#*:}"* ]]
    [[ "$output" == *"cursor: unknown"* ]]
    run jq '.engines.cursor' "$cache"
    [ "$output" = "null" ]
  done
  # Any other nonzero exit is an unrecognised result, never free quota.
  for rc in 1 7; do
    SHIM_CURSOR_RC="$rc" run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [[ "$output" == *"cursor quota unknown — "*"response not recognised"* ]]
    run jq '.engines.cursor' "$cache"
    [ "$output" = "null" ]
  done
  # Our own timeout is a failed fetch, not a malformed response.
  SHIM_CURSOR_RC=124 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — usage-summary call failed"* ]]
}

@test "an unusable --print stdout leaves cursor unknown" {
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  # Not JSON, an array, a scalar, then objects with the wrong types or no
  # usable percentage on a limited plan.
  for body in '<html>sign in</html>' '[]' '"x"' '{}' \
    '{"unlimited": true} {"unlimited": true}' \
    '{"unlimited": false, "plan": {"used_pct": "50"}, "pools": {}, "on_demand": []}' \
    '{"unlimited": false, "plan": {"used_pct": null}, "pools": {"auto_pct": "3", "api_pct": null}, "on_demand": []}' \
    '{"unlimited": false, "plan": {}, "pools": {}, "on_demand": [{"scope": "team", "enabled": true, "used_usd": 50, "limit_usd": 50}]}'; do
    cursor_print "$body"
    run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [[ "$output" == *"cursor quota unknown — "*"response not recognised"* ]]
    run jq '.engines.cursor' "$cache"
    [ "$output" = "null" ]
  done
}

@test "a tool that ignores --print and prints nothing gets the not-found warning" {
  cursor_print ''
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"tmux-agent-usage-cursor not found"* ]]
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "stdout that is not plain contract JSON never leaks into the cache or output" {
  cursor_print '{"plan_type": {"x": "SENTINELCURSORTOKEN"}, "unlimited": false, "plan": {"used_pct": 10},
    "pools": {}, "on_demand": [], "cycle": {"starts_at": 1788000000, "resets_at": 1790592000}}'
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/engine-budget.json"
  # A non-string plan_type degrades to null; the object is never spliced in.
  run jq -e '.engines.cursor.plan_type == null' "$cache"
  [ "$status" -eq 0 ]
  run ! grep -q SENTINELCURSORTOKEN "$cache"
  [[ "$output" != *"SENTINELCURSORTOKEN"* ]]
}

@test "no tmux-agent-usage-cursor leaves cursor unknown and names the tmux-og requirement" {
  cursor_individual_print
  rm "$STUB_DIR/tmux-agent-usage-cursor"
  PATH="$(path_without_real tmux-agent-usage-cursor)" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor quota unknown — tmux-agent-usage-cursor not found (needs tmux-og with \`--print\`, noamsto/tmux-og#945 or later)"* ]]
  [[ "$output" == *"cursor: unknown"* ]]
  run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  grep -qx "tmux run-shell command -v tmux-agent-usage-cursor" "$STUB_LOG"
  run ! grep -q cursor-print "$STUB_LOG"
}

@test "the tool is resolved through the tmux server's PATH when it is not on ours" {
  cursor_individual_print
  via="$BATS_TEST_TMPDIR/via-tmux"
  mkdir -p "$via"
  mv "$STUB_DIR/tmux-agent-usage-cursor" "$via/"
  PATH="$(path_without_real tmux-agent-usage-cursor)" SHIM_TMUX_RUNSHELL="$via/tmux-agent-usage-cursor" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 63.5% used (resets "* ]]
  grep -qx "tmux run-shell command -v tmux-agent-usage-cursor" "$STUB_LOG"
  grep -qx "cursor-print --print" "$STUB_LOG"
}

@test "a tool on our PATH wins without asking the tmux server" {
  cursor_individual_print
  SHIM_TMUX_RUNSHELL="$BATS_TEST_TMPDIR/elsewhere" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor: [pro] month 63.5% used"* ]]
  run ! grep -q '^tmux run-shell' "$STUB_LOG"
}

@test "a tmux lookup answer that is not an absolute executable path is ignored" {
  cursor_individual_print
  rm "$STUB_DIR/tmux-agent-usage-cursor"
  printf 'x\n' >"$BATS_TEST_TMPDIR/not-exec"
  for answer in 'tmux-agent-usage-cursor' "$BATS_TEST_TMPDIR/not-exec" "$BATS_TEST_TMPDIR/missing" '' '-flag'; do
    PATH="$(path_without_real tmux-agent-usage-cursor)" SHIM_TMUX_RUNSHELL="$answer" run bash "$SCRIPT"
    [ "$status" -eq 0 ]
    [[ "$output" == *"tmux-agent-usage-cursor not found"* ]]
    run jq '.engines.cursor' "$XDG_DATA_HOME/crew/engine-budget.json"
    [ "$output" = "null" ]
  done
  run ! grep -q cursor-print "$STUB_LOG"
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

write_local_models_settings() {
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  cat >"$XDG_CONFIG_HOME/dispatcher/settings.json" <<'EOF'
{"localModels":{
  "lemonade/Qwen3.8-Flash-Next-MTP":{"baseUrl":"http://halo.test:13305/v1","contextWindow":131072,"maxConcurrent":2},
  "lemonade/Idle":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}
}}
EOF
}

@test "local slots: the probing run prints each localModels entry's slot use" {
  write_local_models_settings
  SHIM_TMUX_PANES=$'lemonade/Qwen3.8-Flash-Next-MTP\x1fpi\x1fslate\x1ffeat/1-x\x1f' run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"local: lemonade/Qwen3.8-Flash-Next-MTP 1/2 in use (slate (feat/1-x lead))"* ]]
  [[ "$output" == *"local: lemonade/Idle 0/1 in use"* ]]
  [[ "$output" != *"local: lemonade/Idle 0/1 in use ("* ]]
}

@test "local slots: no localModels prints no local line" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" != *"local:"* ]]
}

@test "local slots: --report never prints a local line" {
  write_local_models_settings
  mkdir -p "$XDG_DATA_HOME/crew"
  cat >"$XDG_DATA_HOME/crew/engine-budget.json" <<'EOF'
{"fetched_at":"2026-01-01T00:00:00Z","fetched_epoch":1700000000,"engines":{"claude":null,"codex":null,"cursor":null,"pi":null}}
EOF
  run bash "$SCRIPT" --report
  [ "$status" -eq 0 ]
  [[ "$output" != *"local:"* ]]
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

@test "pi's auth store supplies the OpenRouter key, reaching curl only on stdin" {
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

@test "pi's auth CLI supplies the OpenRouter key, reaching curl only on stdin" {
  or_key_fixture 10
  SHIM_PI_KEY=sk-or-v1-TESTSENTINELCLI SHIM_OR_EXPECT_KEY=sk-or-v1-TESTSENTINELCLI \
    run --separate-stderr bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output$stderr" != *"TESTSENTINELCLI"* ]]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
  run jq -r '.engines.pi.source' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "openrouter_key" ]
  run ! grep -q "TESTSENTINELCLI" "$STUB_LOG"
  run ! grep -rq "TESTSENTINELCLI" "$XDG_DATA_HOME/crew"
  grep -q "pi auth print-api-key --provider openrouter" "$PI_LOG"
  grep -qx "pi-cwd /" "$PI_LOG"
}

@test "a failing pi auth CLI leaves pi unknown" {
  or_key_fixture 10
  SHIM_PI_RC=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"log pi in to OpenRouter"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  run ! grep -q openrouter "$STUB_LOG"
  grep -q "pi auth print-api-key --provider openrouter" "$PI_LOG"
}

@test "no pi on PATH leaves pi unknown" {
  or_key_fixture 10
  rm "$STUB_DIR/pi"
  PATH="$(path_without_real pi)" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"log pi in to OpenRouter"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  ! grep -q openrouter "$STUB_LOG"
}

@test "an auth store without a usable key falls back to pi's CLI" {
  or_key_fixture 10
  write_pi_auth '{"openrouter":{"type":"oauth","access":"not-a-key"}}'
  SHIM_PI_KEY=sk-or-v1-TESTSENTINELCLI SHIM_OR_EXPECT_KEY=sk-or-v1-TESTSENTINELCLI run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
}

@test "pi CLI output that isn't an OpenRouter key leaves pi unknown" {
  or_key_fixture 10
  SHIM_PI_KEY=not-a-key run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"log pi in to OpenRouter"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "a pi older than 0.83 is never asked for the key" {
  or_key_fixture 10
  SHIM_PI_VERSION=0.82.9 SHIM_PI_KEY=sk-or-v1-TESTSENTINELCLI run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"log pi in to OpenRouter"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  grep -q -- '--version' "$PI_LOG"
  ! grep -q 'auth' "$PI_LOG"
}

@test "a hanging pi auth call is bounded and leaves pi unknown" {
  or_key_fixture 10
  SECONDS=0
  SHIM_PI_SLEEP=60 SHIM_PI_KEY=sk-or-v1-TESTSENTINELCLI run bash "$SCRIPT"
  [ "$SECONDS" -lt 30 ]
  [ "$status" -eq 0 ]
  [[ "$output" == *"log pi in to OpenRouter"* ]]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
}

@test "pi's api_key auth-store shape supplies the key without calling the CLI" {
  or_key_fixture 10
  write_pi_auth '{"openrouter":{"type":"api_key","key":"sk-or-v1-TESTSENTINELPI"}}'
  SHIM_OR_EXPECT_KEY=sk-or-v1-TESTSENTINELPI run --separate-stderr bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output$stderr" != *"TESTSENTINELPI"* ]]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
  run jq -r '.engines.pi.source' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "openrouter_key" ]
  run ! grep -rq "TESTSENTINELPI" "$XDG_DATA_HOME/crew"
  run ! grep -q auth "$PI_LOG"
}

@test "pi's auth store wins over its CLI" {
  or_key_fixture 10
  write_pi_auth '{"openrouter":{"type":"oauth","access":"sk-or-v1-TESTSENTINELPI"}}'
  SHIM_PI_KEY=sk-or-v1-TESTSENTINELCLI SHIM_OR_EXPECT_KEY=sk-or-v1-TESTSENTINELPI run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
  ! grep -q 'auth' "$PI_LOG"
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
    SHIM_PI_KEY=sk-or-v1-TESTSENTINELCLI run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  grep -q "openrouter_key_on_stdin=yes" "$STUB_LOG"
  [ ! -s "$PI_LOG" ]
}

@test "the key file is exclusive: pi's auth store is not consulted" {
  or_key_fixture 10
  write_pi_auth '{"openrouter":{"type":"oauth","access":"sk-or-v1-TESTSENTINELPI"}}'
  keyfile="$BATS_TEST_TMPDIR/missing-key"
  DISPATCH_OPENROUTER_KEY_FILE="$keyfile" SHIM_PI_KEY=sk-or-v1-TESTSENTINELCLI run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.engines.pi' "$XDG_DATA_HOME/crew/engine-budget.json"
  [ "$output" = "null" ]
  run ! grep -q openrouter "$STUB_LOG"
  [ ! -s "$PI_LOG" ]
}

@test "a pi auth store without a usable key leaves pi unknown" {
  or_key_fixture 10
  for body in '{"openrouter":{"type":"oauth","access":"not-a-key"}}' 'not json' '{"openrouter":{"type":"api_key","access":"sk-or-v1-TESTSENTINELPI"}}' '{"openrouter":{"access":"sk-or-v1-TESTSENTINELPI"}}' '{"openrouter":{}}' '{"other":{"access":"sk-or-v1-TESTSENTINELPI"}}' '{"openrouter":{"type":"oauth","access":"sk-or-v1-bad key"}}' '{"openrouter":{"type":"api_key","key":"not-a-key"}}' '{"openrouter":{"type":"api_key","key":"sk-or-v1-bad key"}}'; do
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
