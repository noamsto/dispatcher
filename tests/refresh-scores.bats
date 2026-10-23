bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  SCRIPT="$BATS_TEST_DIRNAME/../adapters/core/refresh-scores.sh"
  STUB_DIR="$(mktemp -d)"
  STUB_LOG="$STUB_DIR/calls.log"
  STUB_STDIN_LOG="$STUB_DIR/stdin.log"
  FIXTURE_DIR="$(mktemp -d)"
  export STUB_DIR STUB_LOG STUB_STDIN_LOG FIXTURE_DIR
  # Throwaway HOME + no pi overrides: the host's real pi OpenRouter login
  # must never resolve in these tests, and no dispatch env may leak in.
  export HOME="$(mktemp -d)"
  unset OPENROUTER_API_KEY DISPATCH_OPENROUTER_KEY_FILE PI_CODING_AGENT_DIR
  write_fixtures
  write_curl_shim
  write_pi_shim
  export PATH="$STUB_DIR:$PATH"
}

# write_pi_shim — a fail-closed `pi`: every call exits 1, so a host pi can
# never supply a real key (or get prompted) from these tests.
write_pi_shim() {
  cat >"$STUB_DIR/pi" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
  chmod +x "$STUB_DIR/pi"
}

write_fixtures() {
  cat >"$FIXTURE_DIR/text_overall.json" <<'EOF'
{"num_rows_total": 2, "rows": [
  {"row": {"model_name": "model-b", "organization": "org", "rating": 1490.7, "rank": 2, "vote_count": 100, "leaderboard_publish_date": "2026-08-01"}},
  {"row": {"model_name": "model-a", "organization": "org", "rating": 1501.2, "rank": 1, "vote_count": 200, "leaderboard_publish_date": "2026-08-02"}}
]}
EOF
  # Two pages: total says 101, page 1 carries 100 rows, page 2 the last one.
  # Page size is 100, so only a total above 100 actually exercises paging.
  jq -n '{num_rows_total: 101, rows: [range(0; 100) | {row: {model_name: "code-\(.)", organization: "org", rating: 2000 - ., rank: . + 1, vote_count: 50, leaderboard_publish_date: "2026-08-02"}}]}' \
    >"$FIXTURE_DIR/coding_p1.json"
  jq -n '{num_rows_total: 101, rows: [{row: {model_name: "code-100", organization: "org", rating: 1900, rank: 101, vote_count: 50, leaderboard_publish_date: "2026-08-02"}}]}' \
    >"$FIXTURE_DIR/coding_p2.json"
  cat >"$FIXTURE_DIR/webdev.json" <<'EOF'
{"num_rows_total": 1, "rows": [
  {"row": {"model_name": "web-a", "organization": "org", "rating": 1400.0, "rank": 1, "vote_count": 10, "leaderboard_publish_date": "2026-08-02"}}
]}
EOF
  # Agent Arena publishes rank only — ratings are null (matches the real feed).
  cat >"$FIXTURE_DIR/agent.json" <<'EOF'
{"num_rows_total": 2, "rows": [
  {"row": {"model_name": "agent-b", "organization": "org", "rating": null, "rank": 2, "vote_count": null, "leaderboard_publish_date": "2026-08-02"}},
  {"row": {"model_name": "agent-a", "organization": "org", "rating": null, "rank": 1, "vote_count": null, "leaderboard_publish_date": "2026-08-02"}}
]}
EOF
  # Intentionally unsorted so the agentic-desc sort is exercised.
  cat >"$FIXTURE_DIR/openrouter.json" <<'EOF'
{"data": [
  {"model_permaslug": "b/low", "agentic_index": 40.0, "coding_index": 65.0, "intelligence_index": 70.0},
  {"model_permaslug": "a/high", "agentic_index": 55.0, "coding_index": 60.0, "intelligence_index": 72.0}
], "meta": {"as_of": "2026-08-01T00:00:00Z"}}
EOF
}

# The script's last curl argument is always the URL; the full argv is logged
# so tests can assert on it, and the OpenRouter call's stdin is recorded
# separately — the key must arrive there, never in argv.
write_curl_shim() {
  cat >"$STUB_DIR/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
url="${@: -1}"
if [[ -n "${SHIM_FAIL_ALL:-}" ]]; then
  exit 22
fi
case "$url" in
  *config=text*coding*)
    if [[ "$url" == *offset=0* ]]; then
      cat "$FIXTURE_DIR/coding_p1.json"
    else
      cat "$FIXTURE_DIR/coding_p2.json"
    fi
    ;;
  *config=text*overall*) cat "$FIXTURE_DIR/text_overall.json" ;;
  *config=webdev*) cat "$FIXTURE_DIR/webdev.json" ;;
  *config=agent*) cat "$FIXTURE_DIR/agent.json" ;;
  *openrouter.ai*)
    # Consumed, never logged to $STUB_LOG: proves the key arrives on stdin
    # (-K -) and lets tests grep for it there only.
    cat >>"$STUB_STDIN_LOG"
    cat "$FIXTURE_DIR/openrouter.json"
    ;;
  *) exit 22 ;;
esac
EOF
  chmod +x "$STUB_DIR/curl"
}

cache() {
  cat "$XDG_DATA_HOME/crew/model-scores.json"
}

@test "writes all four arena boards, sorted by rating with floored scores" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -r '.boards | keys_unsorted | join(",")' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "arena_text_overall,arena_text_coding,arena_webdev,arena_agent" ]
  run jq -c '.boards.arena_text_overall.models | map(.model)' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = '["model-a","model-b"]' ]
  run jq '.boards.arena_text_overall.models[0].rating' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "1501" ]
  run jq -r '.boards.arena_text_overall.published' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "2026-08-02" ]
}

@test "pages until num_rows_total is covered, then trims to the top 40" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq '.boards.arena_text_coding.models | length' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "40" ]
  run jq '.boards.arena_text_coding.models[0].rating' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "2000" ]
  run grep -c 'offset=100' "$STUB_LOG"
  [ "$output" = "1" ]
}

@test "rank-only boards sort by rank ascending and keep a null rating" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -c '.boards.arena_agent.models | map(.model)' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = '["agent-a","agent-b"]' ]
  run jq '.boards.arena_agent.models[0].rating' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "null" ]
}

@test "omits the openrouter section without a key and says so" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"skipping openrouter"* ]]
  run jq 'has("openrouter")' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "false" ]
}

@test "includes openrouter benchmarks, sorted by agentic index, when keyed" {
  OPENROUTER_API_KEY=sk-or-v1-TESTSENTINELKEY run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run jq -c '.openrouter.models | map(.model)' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = '["a/high","b/low"]' ]
  run grep -c 'Authorization: Bearer sk-or-v1-TESTSENTINELKEY' "$STUB_STDIN_LOG"
  [ "$output" = "1" ]
}

@test "resolves the key from pi's auth.json when OPENROUTER_API_KEY is unset" {
  mkdir -p "$HOME/pi-agent"
  printf '%s\n' '{"openrouter":{"type":"api_key","key":"sk-or-v1-TESTSENTINELPI"}}' >"$HOME/pi-agent/auth.json"
  PI_CODING_AGENT_DIR="$HOME/pi-agent" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" != *"skipping openrouter"* ]]
  run jq -c '.openrouter.models | map(.model)' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = '["a/high","b/low"]' ]
  grep -q 'Authorization: Bearer sk-or-v1-TESTSENTINELPI' "$STUB_STDIN_LOG"
}

@test "the key reaches curl on stdin, never in argv" {
  OPENROUTER_API_KEY=sk-or-v1-TESTSENTINELKEY run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  run ! grep -q 'sk-or-v1-TESTSENTINELKEY' "$STUB_LOG"
  grep -q 'Authorization: Bearer sk-or-v1-TESTSENTINELKEY' "$STUB_STDIN_LOG"
}

@test "the key never appears in stdout or stderr" {
  OPENROUTER_API_KEY=sk-or-v1-TESTSENTINELKEY run --separate-stderr bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" != *"sk-or-v1-TESTSENTINELKEY"* ]]
  [[ "$stderr" != *"sk-or-v1-TESTSENTINELKEY"* ]]
}

@test "an unreadable DISPATCH_OPENROUTER_KEY_FILE skips with a warning, no fallback" {
  OPENROUTER_API_KEY=sk-or-v1-TESTSENTINELKEY \
    DISPATCH_OPENROUTER_KEY_FILE="$STUB_DIR/nope" run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"skipping openrouter"* ]]
  run jq 'has("openrouter")' "$XDG_DATA_HOME/crew/model-scores.json"
  [ "$output" = "false" ]
}

@test "exits 1 and keeps the old cache when every fetch fails" {
  mkdir -p "$XDG_DATA_HOME/crew"
  printf '{"old":true}\n' >"$XDG_DATA_HOME/crew/model-scores.json"
  SHIM_FAIL_ALL=1 run bash "$SCRIPT"
  [ "$status" -eq 1 ]
  [ "$(cache)" = '{"old":true}' ]
}
