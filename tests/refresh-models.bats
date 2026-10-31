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
  # Only the tools the script needs, so a real claude/pi/codex/curl on the
  # host never leaks into a run; engine stubs are added per test.
  TOOLS="$(mktemp -d)"
  for t in bash jq grep sed head date mkdir mv cat find env chmod tr; do
    ln -s "$(command -v "$t")" "$TOOLS/$t"
  done
  export PATH="$STUB_DIR:$TOOLS"
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

CATALOG="model-catalog.json"

# Fake claude / codex / pi / curl for the catalog. SHIM_PI_FAIL makes the pi
# probe exit nonzero; SHIM_PI_EXTRA appends raw rows to the pi table.
write_other_engine_shims() {
  printf '#!/usr/bin/env bash\necho "Usage: claude [options]"\n' >"$STUB_DIR/claude"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$STUB_DIR/codex"
  cat >"$STUB_DIR/pi" <<'EOF'
#!/usr/bin/env bash
[[ -n "${SHIM_PI_FAIL:-}" ]] && exit 1
printf 'provider    model    context  max-out\n'
printf 'openrouter  deepseek/deepseek-v4.1-flash  1M  64K\n'
printf 'anthropic   claude-haiku-4-5  200K  64K\n'
[[ -n "${SHIM_PI_EXTRA:-}" ]] && printf '%b\n' "$SHIM_PI_EXTRA"
if [[ -n "${SHIM_PI_MANY:-}" ]]; then
  for ((i = 0; i < 2100; i++)); do
    printf 'p  vendor/a-fairly-long-model-name-for-size-padding-padding-padding-%d  1M  64K\n' "$i"
  done
fi
exit 0
EOF
  cat >"$STUB_DIR/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
filler=$(head -c 200000 /dev/zero | tr '\0' 'a')
echo '{"data":[{"id":"deepseek/deepseek-v4.1-flash","created":1789000000,"pricing":{"prompt":"0.0000001","completion":"0.0000004"}},{"id":"filler","description":"'"$filler"'"}]}'
EOF
  chmod +x "$STUB_DIR/claude" "$STUB_DIR/codex" "$STUB_DIR/pi" "$STUB_DIR/curl"
  mkdir -p "$HOME/.codex"
  printf '%s\n' '{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.5"}]}' >"$HOME/.codex/models_cache.json"
}

@test "the catalog holds all four engines in the section 5 shape" {
  write_other_engine_shims
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cat="$XDG_DATA_HOME/crew/$CATALOG"
  [ "$(jq -r '.claude.source' "$cat")" = "aliases" ]
  [ "$(jq -r '.claude.models | map(.id) | join(",")' "$cat")" = "fable,haiku,opus,sonnet" ]
  [ "$(jq -r '.codex.source' "$cat")" = "codex models_cache.json" ]
  [ "$(jq -r '.codex.models[0].id' "$cat")" = "gpt-5.5" ]
  [ "$(jq -r '.cursor.source' "$cat")" = "cursor-agent --list-models" ]
  [ "$(jq -r '.cursor.models | map(.id) | index("cursor-grok-4.6-high") != null' "$cat")" = "true" ]
  [ "$(jq -r '.pi.source' "$cat")" = "pi --list-models; openrouter /api/v1/models" ]
  [ "$(jq -r '.pi.models[] | select(.id == "openrouter/deepseek/deepseek-v4.1-flash") | [.created, .price.in == 0.0000001, .price.out == 0.0000004] | join(",")' "$cat")" = "1789000000,true,true" ]
  [ "$(jq -c '.pi.models[] | select(.id == "anthropic/claude-haiku-4-5")' "$cat")" = '{"id":"anthropic/claude-haiku-4-5"}' ]
  [ "$(jq -r '[.[] | .fetched_epoch | type] | unique | join(",")' "$cat")" = "number" ]
}

@test "a failing pi probe keeps pi's previous snapshot and refreshes the rest" {
  write_other_engine_shims
  mkdir -p "$XDG_DATA_HOME/crew"
  printf '%s\n' '{"pi":{"fetched_epoch":1,"source":"old","models":[{"id":"old/pi-model"}]}}' \
    >"$XDG_DATA_HOME/crew/$CATALOG"
  SHIM_PI_FAIL=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"pi --list-models failed"* ]]
  cat="$XDG_DATA_HOME/crew/$CATALOG"
  [ "$(jq -c '.pi' "$cat")" = '{"fetched_epoch":1,"source":"old","models":[{"id":"old/pi-model"}]}' ]
  [ "$(jq -r '.cursor.source' "$cat")" = "cursor-agent --list-models" ]
  [ "$(jq -r '.claude.source' "$cat")" = "aliases" ]
}

@test "ids with control bytes or outside the shape regex are dropped" {
  write_other_engine_shims
  SHIM_PI_EXTRA='bad  evil\x1b[31mid  1M\nbad  has$dollar  1M\nbad  spa<ce  1M\nok  good.id:1@x  1M' run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cat="$XDG_DATA_HOME/crew/$CATALOG"
  ids=$(jq -r '.pi.models | map(.id) | join("\n")' "$cat")
  [[ "$ids" == *"ok/good.id:1@x"* ]]
  [[ "$ids" != *"evil"* ]]
  [[ "$ids" != *"dollar"* ]]
  [[ "$ids" != *"spa<ce"* ]]
  [ "$(jq -r '[.[].models[].id | select(test("[[:cntrl:]]"))] | length' "$cat")" = "0" ]
}

@test "an engine list past 128 KiB is capped at 2000 models and still written with the other engines" {
  write_other_engine_shims
  SHIM_PI_MANY=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"pi: dropped"* ]]
  cat="$XDG_DATA_HOME/crew/$CATALOG"
  [ "$(jq -r '.pi.models | length' "$cat")" = "2000" ]
  [ "$(jq -r '.claude.source' "$cat")" = "aliases" ]
}

@test "the cursor cache is unchanged by the catalog and the openrouter fetch goes through the stub" {
  write_other_engine_shims
  run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  cache="$XDG_DATA_HOME/crew/$CACHE"
  [ "$(jq -c 'del(.fetched_at, .fetched_epoch)' "$cache")" = \
    '{"models":[{"slug":"gpt-5.3-codex-low"},{"slug":"cursor-grok-4.6-high"},{"slug":"cursor-grok-4.6-medium-fast"},{"slug":"cursor-grok-4.6-low-fast"},{"slug":"claude-opus-5-high"}]}' ]
  grep -q 'openrouter.ai/api/v1/models' "$STUB_LOG"
}

@test "cursor failing while another engine refreshes still exits zero and warns" {
  write_other_engine_shims
  SHIM_CURSOR_FAIL=1 run bash "$SCRIPT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"cursor-agent --list-models failed"* ]]
  [ "$(jq -r 'has("cursor")' "$XDG_DATA_HOME/crew/$CATALOG")" = "false" ]
}
