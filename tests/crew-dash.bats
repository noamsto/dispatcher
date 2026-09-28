bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  SCRIPT="$BATS_TEST_DIRNAME/../adapters/core/crew-dash.sh"
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
  "$REAL_TMUX" -L dash kill-server 2>/dev/null || true
  teardown_repo
}

# _dash_launcher — write a script that sets up the same environment as the
# other tests (STUB_DIR on PATH, XDG_*, dispatch-config/locked-settings,
# SHIM_NOW, NO_COLOR unset) and execs crew-dash.sh from the test repo. Prints
# its path. Used only by the real-tmux interactive tests below.
_dash_launcher() {
  local out="$BATS_TEST_TMPDIR/dash-launch.sh"
  cat >"$out" <<EOF
#!/usr/bin/env bash
export PATH="$PATH"
export XDG_CONFIG_HOME="$XDG_CONFIG_HOME"
export XDG_DATA_HOME="$XDG_DATA_HOME"
export DISPATCH_CONFIG_BIN="$DISPATCH_CONFIG_BIN"
export DISPATCH_LOCKED_SETTINGS="$DISPATCH_LOCKED_SETTINGS"
export SHIM_NOW="$SHIM_NOW"
unset NO_COLOR
cd "$TEST_REPO"
exec bash "$SCRIPT"
EOF
  chmod +x "$out"
  printf '%s' "$out"
}

# _dash_wait_ready <sock> <target> — poll capture-pane until the status line
# (last line, starting " r refresh") shows up, i.e. the first frame painted.
# Leaves the last capture in $DASH_CAP.
_dash_wait_ready() {
  local sock="$1" target="$2" tries=0
  while [ "$tries" -lt 50 ]; do
    DASH_CAP="$("$REAL_TMUX" -L "$sock" capture-pane -p -t "$target")"
    case "$(printf '%s\n' "$DASH_CAP" | tail -n1)" in
    " r refresh"*) return 0 ;;
    esac
    tries=$((tries + 1))
    sleep 0.2
  done
  return 1
}

# _dash_wait_contains <sock> <target> <needle> — poll until a capture
# contains $needle anywhere. Leaves the last capture in $DASH_CAP.
_dash_wait_contains() {
  local sock="$1" target="$2" needle="$3" tries=0
  while [ "$tries" -lt 50 ]; do
    DASH_CAP="$("$REAL_TMUX" -L "$sock" capture-pane -p -t "$target")"
    case "$DASH_CAP" in
    *"$needle"*) return 0 ;;
    esac
    tries=$((tries + 1))
    sleep 0.2
  done
  return 1
}

# _dash_wait_lines <sock> <target> <n> — poll until a capture has exactly $n
# lines and looks like a settled frame (status line last). Leaves the last
# capture in $DASH_CAP.
_dash_wait_lines() {
  local sock="$1" target="$2" n="$3" tries=0
  while [ "$tries" -lt 50 ]; do
    DASH_CAP="$("$REAL_TMUX" -L "$sock" capture-pane -p -t "$target")"
    if [ "$(printf '%s\n' "$DASH_CAP" | wc -l)" -eq "$n" ]; then
      case "$(printf '%s\n' "$DASH_CAP" | tail -n1)" in
      " r refresh"*) return 0 ;;
      esac
    fi
    tries=$((tries + 1))
    sleep 0.2
  done
  return 1
}

# _dash_wait_row_ellipsis <sock> <target> <needle> — poll until the line
# containing $needle, right-trimmed, ends in "…". A resize's repaint lands up
# to ~1s after the WINCH (the key-read loop's timeout), so a single capture
# right after resize-window can still show the pre-resize frame cropped by
# tmux to the new pane size — which trivially has the right line/col counts
# without proving a real repaint happened; this polls past that window.
# Leaves the last capture in $DASH_CAP.
_dash_wait_row_ellipsis() {
  local sock="$1" target="$2" needle="$3" tries=0 line trimmed
  while [ "$tries" -lt 50 ]; do
    DASH_CAP="$("$REAL_TMUX" -L "$sock" capture-pane -p -t "$target")"
    line="$(printf '%s\n' "$DASH_CAP" | grep -F "$needle" | head -1)"
    if [ -n "$line" ]; then
      trimmed="$(_rtrim "$line")"
      case "$trimmed" in
      *…) return 0 ;;
      esac
    fi
    tries=$((tries + 1))
    sleep 0.2
  done
  return 1
}

# _rtrim <text> — trailing whitespace stripped.
_rtrim() {
  local s="$1"
  printf '%s' "${s%"${s##*[![:space:]]}"}"
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
  NO_COLOR=1 run bash "$SCRIPT" --once
  [ "$status" -eq 0 ]
  diff <(printf '%s\n' "$output" | normalize_golden) "$FIXTURES/once.golden"
}

# ---------------------------------------------------------------------------
# 2. Color
# ---------------------------------------------------------------------------

@test "--once to a pipe carries no color by default; CREW_DASH_COLOR=always matches the color golden" {
  run bash -c "bash '$SCRIPT' --once | cat"
  [ "$status" -eq 0 ]
  [[ "$output" != *$'\e['* ]]

  CREW_DASH_COLOR=always run bash "$SCRIPT" --once
  [ "$status" -eq 0 ]
  diff <(printf '%s\n' "$output" | normalize_golden) "$FIXTURES/once-color.golden"
  [[ "$output" == *$'\e[1;33m'* ]]
}

# ---------------------------------------------------------------------------
# 3. --json shape
# ---------------------------------------------------------------------------

@test "--json shape: settings origin/editable, layers, top-level keys" {
  run bash "$SCRIPT" --json
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
  DISPATCH_CONFIG_BIN=false run bash "$SCRIPT" --once
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
  run --separate-stderr bash "$SCRIPT" --once
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

  run bash "$SCRIPT" --once
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
  run bash "$SCRIPT" --once
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
  run --separate-stderr bash "$SCRIPT" --bogus
  [ "$status" -eq 2 ]
  [[ "$stderr" == "usage: crew dash [--once | --json]" ]]
}

# ---------------------------------------------------------------------------
# 10. Backslash escaping
# ---------------------------------------------------------------------------

@test "--once shows a backslash in a settings value once-escaped, not doubled" {
  # repoTrackers is user-only (no locked/default layer contests it), so this
  # value survives unchanged. It's one literal backslash; tojson re-escapes
  # it as the 6 chars "a\\b" (quote a backslash backslash b quote). The old
  # @tsv-based renderer doubled it to "a\\\\b". grep -F, not [[ == glob ]]
  # (which treats \\ as an escaped single backslash), so the backslash
  # counts stay exact.
  jq -n '{repoTrackers: {"noamsto/dispatcher": "a\\b"}}' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  run bash "$SCRIPT" --once
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
  run bash "$SCRIPT" --once
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
  run bash "$SCRIPT" --once
  [ "$status" -eq 0 ]
  [[ "$output" == *"no retro notes yet"* ]]
}

# ---------------------------------------------------------------------------
# 13. trunc() unit checks (no tty, no collector)
# ---------------------------------------------------------------------------

@test "trunc: ascii, locked-emoji, CJK, short and exact-fit widths" {
  input=$'10\tabcdefghijkl\n6\t🔒 locked-row\n7\tディレクトリ\n10\tshort\n6\tディレ\n'
  run env CREW_DASH_TRUNC_TEST=1 bash "$SCRIPT" <<<"$input"
  [ "$status" -eq 0 ]
  mapfile -t lines <<<"$output"
  [ "${lines[0]}" = "abcdefghi…" ]
  [ "${lines[1]}" = "🔒 lo…" ]
  [ "${lines[2]}" = "ディレ…" ]
  [ "${lines[3]}" = "short" ]
  [ "${lines[4]}" = "ディレ" ]
}

# ---------------------------------------------------------------------------
# 14. Interactive: first frame at 80x24
# ---------------------------------------------------------------------------

@test "interactive renders at 80x24 with truncation, not wrap" {
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"
  launcher="$(_dash_launcher)"
  "$REAL_TMUX" -L dash -f /dev/null new-session -d -s d -x 80 -y 24 "$launcher"
  "$REAL_TMUX" -L dash set-option -t d remain-on-exit on
  _dash_wait_ready dash d

  [ "$(printf '%s\n' "$DASH_CAP" | wc -l)" -eq 24 ]
  [[ "$(printf '%s\n' "$DASH_CAP" | sed -n '1p')" == *"1 Settings"* ]]

  maxw="$(printf '%s\n' "$DASH_CAP" | LC_ALL=C.UTF-8 wc -L)"
  [ "$maxw" -le 80 ]

  kf_num="$(printf '%s\n' "$DASH_CAP" | grep -n 'keyFile:' | head -1 | cut -d: -f1)"
  [ -n "$kf_num" ]
  kf_line="$(printf '%s\n' "$DASH_CAP" | sed -n "${kf_num}p")"
  trimmed="$(_rtrim "$kf_line")"
  case "$trimmed" in
  *…) ;;
  *)
    echo "keyFile line does not end with an ellipsis: [$trimmed]" >&2
    return 1
    ;;
  esac
  next_num=$((kf_num + 1))
  next_line="$(printf '%s\n' "$DASH_CAP" | sed -n "${next_num}p")"
  [[ "$next_line" == *"repoTrackers"* ]]

  [[ "$(printf '%s\n' "$DASH_CAP" | sed -n '24p')" == " r refresh"* ]]
}

# ---------------------------------------------------------------------------
# 15. Interactive: pane switch + resize
# ---------------------------------------------------------------------------

@test "switching pane and resizing repaints within the new size" {
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"
  launcher="$(_dash_launcher)"
  "$REAL_TMUX" -L dash -f /dev/null new-session -d -s d -x 80 -y 24 "$launcher"
  "$REAL_TMUX" -L dash set-option -t d remain-on-exit on
  _dash_wait_ready dash d

  "$REAL_TMUX" -L dash send-keys -t d 2
  _dash_wait_contains dash d "claude (oauth_usage)"

  "$REAL_TMUX" -L dash resize-window -t d -x 60 -y 20
  _dash_wait_lines dash d 20
  _dash_wait_row_ellipsis dash d "3d 12h"

  [ "$(printf '%s\n' "$DASH_CAP" | wc -l)" -eq 20 ]
  maxw="$(printf '%s\n' "$DASH_CAP" | LC_ALL=C.UTF-8 wc -L)"
  [ "$maxw" -le 60 ]
}

# ---------------------------------------------------------------------------
# 16. Interactive: quit
# ---------------------------------------------------------------------------

@test "q exits cleanly and leaves the alternate screen" {
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"
  launcher="$(_dash_launcher)"
  "$REAL_TMUX" -L dash -f /dev/null new-session -d -s d -x 80 -y 24 "$launcher"
  "$REAL_TMUX" -L dash set-option -t d remain-on-exit on
  _dash_wait_ready dash d

  "$REAL_TMUX" -L dash send-keys -t d q

  tries=0
  status_out=""
  while [ "$tries" -lt 50 ]; do
    status_out="$("$REAL_TMUX" -L dash display -p -t d '#{pane_dead} #{pane_dead_status}')"
    [ "$status_out" = "1 0" ] && break
    tries=$((tries + 1))
    sleep 0.2
  done
  [ "$status_out" = "1 0" ]
}
