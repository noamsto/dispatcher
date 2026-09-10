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
# SHIM_CODEX_GENERIC swaps in a single 1440min (24h) window at 90% used — the
# static default below is pinned at 5h/7d buckets and can never emit a
# 1d/unknown/other key at >=85%, which the generic advisory wording needs.
write_codex_shim() {
  cat >"$STUB_DIR/codex" <<'EOF'
#!/usr/bin/env bash
if [[ -n "${SHIM_CODEX_FAIL:-}" ]]; then
  exit 1
fi
printf '%s\n' '{"id":1,"result":{}}'
if [[ -n "${SHIM_CODEX_GENERIC:-}" ]]; then
  printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":90,"windowDurationMins":1440},"credits":{"hasCredits":true},"planType":"team"}}}'
else
  printf '%s\n' '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":42,"windowDurationMins":300,"resetsAt":1785800000},"secondary":{"usedPercent":61,"windowDurationMins":10080,"resetsAt":1786200000},"credits":{"hasCredits":true},"planType":"team"}}}'
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

@test "claude quota comes from the oauth endpoint with normalized windows" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  # The fixture's 7d window is 97.0% (>=85%) — the budget lever must visibly
  # fire a warning for it, and must NOT fire one for the 12.5% 5h window.
  # Its resets_at (2026-08-09) is already in the past, so the line carries
  # no "(resets in ...)" parenthetical — verified by requiring the "%" to
  # butt directly against the em dash.
  [[ "$output" == *"budget lever: claude 7d at 97.0% — real budget: prefer a cheaper burn class or rotate engines"* ]]
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
  [[ "$output" == *"budget lever: claude 7d at 97% (resets in 3d 22h, 53 points ahead of pace) — real budget: prefer a cheaper burn class or rotate engines"* ]]
  [[ "$output" == *"budget lever: codex 1d at 90% — approaching quota (>=85%), prefer a cheaper burn class or rotate engines (see DISPATCHER_PROTOCOL.md)"* ]]
  # The summary line gains the relative remaining only on a future reset —
  # the oauth fixture's past-dated one has no ", in ..." tail by design.
  [[ "$output" == *"claude: "*"(resets "*", in "* ]]
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
