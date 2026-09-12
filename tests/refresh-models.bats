setup() {
  load helpers
  SCRIPT="$BATS_TEST_DIRNAME/../adapters/core/refresh-models.sh"
  STUB_DIR="$(mktemp -d)"
  STUB_LOG="$STUB_DIR/calls.log"
  export STUB_DIR STUB_LOG
  # `load helpers` above already isolates $XDG_DATA_HOME under
  # $BATS_TEST_TMPDIR — the script only writes under there. A throwaway HOME
  # too, matching refresh-budget.bats's isolation, in case that ever changes.
  export HOME="$(mktemp -d)"
  write_cursor_agent_shim
  export PATH="$STUB_DIR:$PATH"
}

CACHE="cursor-models-cache.json"

# Fake `cursor-agent --list-models`: prints a realistic fixture (header,
# blank line, several slug lines including the `auto` pseudo-entry, and the
# trailing `Tip:` line the real CLI appends). Exits nonzero when
# SHIM_CURSOR_FAIL is set.
write_cursor_agent_shim() {
  cat >"$STUB_DIR/cursor-agent" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [[ -n "${SHIM_CURSOR_FAIL:-}" ]]; then
  exit 1
fi
cat <<'MODELS'
Available models

auto - Auto (default)
gpt-5.3-codex-low - Codex 5.3 Low
cursor-grok-4.6-high - Cursor Grok 4.6
cursor-grok-4.6-medium-fast - Cursor Grok 4.6 Medium Fast
cursor-grok-4.6-low-fast - Cursor Grok 4.6 Low Fast
claude-opus-5-high - Claude Opus 5 1M

Tip: use --model <id> (or /model <id> in interactive mode) to switch. Parameterized models also accept quoted overrides, e.g. --model 'claude-opus-4-8[context=1m,effort=high,fast=false]'.
MODELS
EOF
  chmod +x "$STUB_DIR/cursor-agent"
}

@test "a realistic --list-models fixture parses into the expected cache shape" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/$CACHE"
  run jq '[.models[]?|.slug?|strings]' "$cache"
  [[ "$output" == *"cursor-grok-4.6-high"* ]]
  [[ "$output" == *"cursor-grok-4.6-medium-fast"* ]]
  [[ "$output" == *"cursor-grok-4.6-low-fast"* ]]
  [[ "$output" != *'"auto"'* ]]
  run jq '.fetched_epoch | type' "$cache"
  [ "$output" = '"number"' ]
}

@test "the write is atomic and lands at the cursor models cache path" {
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/$CACHE"
  [ -f "$cache" ]
  # No leftover tmp file: mv is atomic, and the write never truncates in place.
  run find "$XDG_DATA_HOME/crew" -maxdepth 1 -name '*.tmp.*'
  [ -z "$output" ]
  run jq -r '.models | length' "$cache"
  [ "$output" = "5" ]
}

@test "a stubbed cursor-agent failure exits nonzero and leaves an existing cache untouched" {
  mkdir -p "$XDG_DATA_HOME/crew"
  cache="$XDG_DATA_HOME/crew/$CACHE"
  printf '%s\n' '{"fetched_at":"stale","fetched_epoch":1,"models":[{"slug":"stale-model"}]}' >"$cache"
  before="$(cat "$cache")"
  SHIM_CURSOR_FAIL=1 run bash "$SCRIPT"
  [ "$status" -ne 0 ]
  after="$(cat "$cache")"
  [ "$before" = "$after" ]
}

@test "cursor-agent absent from PATH exits nonzero and leaves an existing cache untouched" {
  mkdir -p "$XDG_DATA_HOME/crew"
  cache="$XDG_DATA_HOME/crew/$CACHE"
  printf '%s\n' '{"fetched_at":"stale","fetched_epoch":1,"models":[{"slug":"stale-model"}]}' >"$cache"
  before="$(cat "$cache")"
  # Drop every PATH entry that provides a cursor-agent (the stub dir, and any
  # real install), keeping bash/jq/coreutils reachable for the script itself.
  local_path=""
  IFS=: read -ra dirs <<<"$PATH"
  for d in "${dirs[@]}"; do
    [ -x "$d/cursor-agent" ] && continue
    local_path="$local_path:$d"
  done
  run env PATH="${local_path#:}" bash "$SCRIPT"
  [ "$status" -ne 0 ]
  after="$(cat "$cache")"
  [ "$before" = "$after" ]
}
