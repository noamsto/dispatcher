bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# setup_file — build the Go binary once for the whole suite. $CREW_DASH_BIN,
# when the caller already set it (e.g. `nix build .#crew-dash`), is used as
# is and works offline; otherwise `go build` produces one into
# $BATS_FILE_TMPDIR, which every test's setup() below points CREW_DASH_BIN at.
setup_file() {
  if [ -z "${CREW_DASH_BIN:-}" ]; then
    (cd "$BATS_TEST_DIRNAME/../dash" && go build -o "$BATS_FILE_TMPDIR/crew-dash" .)
  fi
}


setup() {
  load helpers
  unset NO_COLOR FORCE_COLOR CLICOLOR CLICOLOR_FORCE COLORTERM
  CREW_DASH_BIN="${CREW_DASH_BIN:-$BATS_FILE_TMPDIR/crew-dash}"
  export CREW_DASH_BIN
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  FIXTURES="$BATS_TEST_DIRNAME/fixtures/crew-dash"
  setup_repo
  git remote add origin git@github.com:example/dash.git

  # Resolved before $STUB_DIR gets a `date` shim, so it can fall through.
  REAL_DATE="$(command -v date)"

  cat >"$STUB_DIR/crew" <<EOF
#!/usr/bin/env bash
exec bash -euo pipefail "$CREW" "\$@"
EOF
  chmod +x "$STUB_DIR/crew"

  cat >"$STUB_DIR/refresh-budget" <<EOF
#!/usr/bin/env bash
exec bash "$BATS_TEST_DIRNAME/../adapters/core/refresh-budget.sh" "\$@"
EOF
  chmod +x "$STUB_DIR/refresh-budget"

  SHIM_NOW=1790000000
  export SHIM_NOW
  # The Go binary reads its own clock from CREW_DASH_NOW (refresh-budget and
  # crew roster still go through the date shim above for theirs).
  export CREW_DASH_NOW=1790000000
  cat >"$STUB_DIR/date" <<EOF
#!/usr/bin/env bash
if [[ -n "\${SHIM_NOW:-}" && "\$#" -eq 1 && "\$1" == "+%s" ]]; then
  printf '%s\n' "\$SHIM_NOW"
  exit 0
fi
exec "$REAL_DATE" "\$@"
EOF
  chmod +x "$STUB_DIR/date"

  # The raw-source resolver reads defaults.json beside itself.
  mkdir -p "$BATS_TEST_TMPDIR/cfg"
  cp "$BATS_TEST_DIRNAME/../adapters/core/dispatch-config.sh" "$BATS_TEST_TMPDIR/cfg/dispatch-config.sh"
  cp "$FIXTURES/defaults.json" "$BATS_TEST_TMPDIR/cfg/defaults.json"
  chmod +x "$BATS_TEST_TMPDIR/cfg/dispatch-config.sh"
  export DISPATCH_CONFIG_BIN="$BATS_TEST_TMPDIR/cfg/dispatch-config.sh"

  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  cp "$FIXTURES/user-settings.json" "$XDG_CONFIG_HOME/dispatcher/settings.json"
  export DISPATCH_LOCKED_SETTINGS="$FIXTURES/locked.json"

  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson now "$SHIM_NOW" '{
    fetched_at: "2026-01-01T00:00:00Z",
    fetched_epoch: ($now - 600),
    engines: {
      claude: {source: "oauth_usage", plan_type: null, credits_cover: false,
        windows: {"5h": {used_pct: 10, resets_at: ($now + 7200)},
                  "7d": {used_pct: 90, resets_at: ($now + 302400)}}},
      codex: {source: "app_server", plan_type: "pro", credits_cover: null,
        windows: {"5h": {used_pct: 30, resets_at: ($now + 3600)}}},
      cursor: null,
      pi: null
    }
  }' >"$XDG_DATA_HOME/crew/engine-budget.json"
  cp "$FIXTURES/ratings.jsonl" "$XDG_DATA_HOME/crew/ratings.jsonl"

  unset CREW_ID CREW_BIN
}

teardown() {
  teardown_repo
}

# normalize_golden — the layer paths live under $BATS_TEST_TMPDIR (unique per
# run) or the checkout (unique per machine), so the golden carries fixed
# placeholders; substitute them into the actual output before diffing.
normalize_golden() {
  sed \
    -e "s|$BATS_TEST_TMPDIR/cfg/defaults.json|<BASE>|" \
    -e "s|$XDG_CONFIG_HOME/dispatcher/settings.json|<USER>|" \
    -e "s|$FIXTURES/locked.json|<LOCKED>|"
}

# ---------------------------------------------------------------------------
# 1. --once golden
# ---------------------------------------------------------------------------

@test "--once with NO_COLOR matches the golden" {
  NO_COLOR=1 run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  diff <(printf '%s\n' "$output" | normalize_golden) "$FIXTURES/once.golden"
}

# ---------------------------------------------------------------------------
# 2. Color
# ---------------------------------------------------------------------------

@test "--once to a pipe carries no color by default; CREW_DASH_COLOR=always matches the color golden" {
  run bash -c "'$CREW_DASH_BIN' --once | cat"
  [ "$status" -eq 0 ]
  [[ "$output" != *$'\e['* ]]

  CREW_DASH_COLOR=always run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  diff <(printf '%s\n' "$output" | normalize_golden) "$FIXTURES/once-color.golden"
  [[ "$output" == *$'\e[1;33m'* ]]
}

# ---------------------------------------------------------------------------
# 3. --json shape
# ---------------------------------------------------------------------------

@test "--json shape: settings origin/editable, layers, top-level keys" {
  run "$CREW_DASH_BIN" --json
  [ "$status" -eq 0 ]
  json_out="$output"

  run jq -e '.settings.rows[] | select(.path == ["profile"]) | .origin == "locked" and .editable == false' <<<"$json_out"
  [ "$status" -eq 0 ]

  run jq -e '.settings.rows[] | select(.path == ["engines"]) | .editable == true' <<<"$json_out"
  [ "$status" -eq 0 ]

  run jq -e --arg l "$FIXTURES/locked.json" '.settings.layers.locked == $l' <<<"$json_out"
  [ "$status" -eq 0 ]

  run jq -e '(keys) == ["budget","now","roster","runs","settings"]' <<<"$json_out"
  [ "$status" -eq 0 ]
}

# ---------------------------------------------------------------------------
# 4. Degraded source
# ---------------------------------------------------------------------------

@test "a broken dispatch-config degrades only the settings pane" {
  DISPATCH_CONFIG_BIN=false run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  settings_section="$(printf '%s\n' "$output" | sed -n '/== Settings ==/,/== Budget ==/p')"
  [[ "$settings_section" == *"unavailable: exit 1"* ]]
  budget_section="$(printf '%s\n' "$output" | sed -n '/== Budget ==/,/== Runs ==/p')"
  [[ "$budget_section" == *"claude (oauth_usage)"* ]]
}

# ---------------------------------------------------------------------------
# 5. Warnings
# ---------------------------------------------------------------------------

@test "a stripped grantRoots key surfaces as a settings warning, never on stderr" {
  cp "$FIXTURES/user-settings-grantroots.json" "$XDG_CONFIG_HOME/dispatcher/settings.json"
  run --separate-stderr "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  [[ "$output" == *"warning: dispatch-config: ignoring grantRoots from "* ]]
  [ -z "$stderr" ]
}

# ---------------------------------------------------------------------------
# 6. Retro rows
# ---------------------------------------------------------------------------

@test "retro notes render as a crew section with summary and tags" {
  logf="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc '{ts: 1000, crew_id: "c1", kind: "dispatch", branch: "feat/x", engine: "claude", model: "opus", tier: "deep", effort: "medium", title: "t"}' >>"$logf"
  jq -nc '{ts: 1200, crew_id: "c1", from: "worker:feat/x#s1", to: "retro:c1", kind: "msg", body: ("{\"seam\":\"execute\",\"tag\":\"gate_thrash\",\"detail\":\"circled build\"}")}' >>"$logf"
  jq -nc '{ts: 5000, crew_id: "c1", from: "dispatcher:c1", to: "retro:c1", kind: "msg", body: ("{\"seam\":\"drained\",\"tag\":\"misrouted\",\"detail\":\"trivial\"}")}' >>"$logf"
  jq -nc '{ts: 5100, crew_id: "c1", from: "dispatcher:c1", to: "retro:c1", kind: "msg", body: ("{\"seam\":\"drained\",\"tag\":\"session_summary\",\"detail\":\"khaki: deep/claude/opus done\"}")}' >>"$logf"

  run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  [[ "$output" == *"crew c1"* ]]
  [[ "$output" == *"summary: khaki: deep/claude/opus done"* ]]
  [[ "$output" == *"misrouted"* ]]
  [[ "$output" == *"gate_thrash"* ]]
  [[ "$output" != *"session_summary"* ]]
  [[ "$output" == *"misrouted: trivial"* ]]
  [[ "$output" == *"gate_thrash: circled build"* ]]
}

# ---------------------------------------------------------------------------
# 7. Untruncated locked value
# ---------------------------------------------------------------------------

@test "--once never truncates a long locked value" {
  run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  [[ "$output" == *"ディレクトリ/and-more"* ]]
}

# ---------------------------------------------------------------------------
# 8. Delegation
# ---------------------------------------------------------------------------

@test "crew dash delegates to crew-dash with CREW_BIN set to crew.sh's own path" {
  workdir="$(mktemp -d)"
  cat >"$STUB_DIR/crew-dash" <<'EOF'
#!/usr/bin/env bash
printf '%s|%s\n' "$CREW_BIN" "$*"
EOF
  chmod +x "$STUB_DIR/crew-dash"
  run bash -c "cd '$workdir' && bash -euo pipefail '$CREW' dash --once"
  [ "$status" -eq 0 ]
  [[ "$output" == *"crew.sh|--once" ]]
  rm -rf "$workdir"
}

# ---------------------------------------------------------------------------
# 9. Usage
# ---------------------------------------------------------------------------

@test "an unknown argument exits 2 with usage" {
  run --separate-stderr "$CREW_DASH_BIN" --bogus
  [ "$status" -eq 2 ]
  [[ "$stderr" == "usage: crew dash [--once | --json]" ]]
}

# ---------------------------------------------------------------------------
# 10. Backslash escaping
# ---------------------------------------------------------------------------

@test "--once shows a backslash in a settings value once-escaped, not doubled" {
  # repoTrackers is user-only (no locked/default layer contests it), so this
  # value survives unchanged. It's one literal backslash; tojson re-escapes
  # it as the 6 chars "a\\b" (quote a backslash backslash b quote). grep -F,
  # not [[ == glob ]] (which treats \\ as an escaped single backslash), so
  # the backslash counts stay exact.
  jq -n '{repoTrackers: {"noamsto/dispatcher": "a\\b"}}' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  grep -qF 'noamsto/dispatcher: "a\\b"' <<<"$output"
  ! grep -qF 'noamsto/dispatcher: "a\\\\b"' <<<"$output"
}

# ---------------------------------------------------------------------------
# 11. Large payloads never go through argv
# ---------------------------------------------------------------------------

@test "--once handles a large settings value without hitting ARG_MAX" {
  bigfile="$BATS_TEST_TMPDIR/big.txt"
  head -c 300000 /dev/zero | tr '\0' 'a' >"$bigfile"
  jq -n --rawfile big "$bigfile" '{profile: $big}' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  [[ "$output" == *"== Settings =="* ]]
  [[ "$output" == *"== Budget =="* ]]
  [[ "$output" == *"== Runs =="* ]]
  [[ "$output" == *"== Roster =="* ]]
}

# ---------------------------------------------------------------------------
# 12. Older crew build, missing report fields
# ---------------------------------------------------------------------------

@test "a retro report with no rows field still renders 'no retro notes yet'" {
  cat >"$STUB_DIR/crew" <<EOF
#!/usr/bin/env bash
case "\$1" in
retro) echo '{"tags":[],"unknown":[]}' ;;
*) exec bash -euo pipefail "$CREW" "\$@" ;;
esac
EOF
  chmod +x "$STUB_DIR/crew"
  run "$CREW_DASH_BIN" --once
  [ "$status" -eq 0 ]
  [[ "$output" == *"no retro notes yet"* ]]
}
