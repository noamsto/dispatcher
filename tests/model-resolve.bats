#!/usr/bin/env bats
# model-resolve (#1370): the pure resolver of one engine/tier row against a
# catalog snapshot and the class-state file. Catalogs and state files are
# hand-built in the docs/model-classes.md section 5 shape.

bats_require_minimum_version 1.5.0

setup() {
  load helpers
  RESOLVE="$BATS_TEST_DIRNAME/../adapters/core/model-resolve.sh"
  SETTINGS="$BATS_TEST_TMPDIR/settings.json"
  cat >"$SETTINGS" <<'JSON'
{
  "modelMap": {
    "claude": {
      "deep": {"default": "opus", "models": ["opus"]},
      "standard": {"default": "sonnet", "models": ["opus", "claude-opus-*", "sonnet", "claude-sonnet-*", "haiku", "claude-haiku-*"]}
    },
    "pi": {
      "deep": {"default": "openrouter/deepseek/deepseek-v4.1-flash", "models": ["openrouter/deepseek/deepseek-v4.1-flash"]},
      "standard": {"default": "openrouter/deepseek/deepseek-v4.1-flash", "models": ["openrouter/deepseek/deepseek-v4.1-flash", "openrouter/deepseek/deepseek-v4.1-flash-1017"]}
    }
  },
  "classes": {
    "claude": {
      "cheap": ["haiku", "claude-haiku-*"],
      "standard": ["sonnet", "claude-sonnet-*"],
      "premium": ["opus", "claude-opus-*"],
      "tiers": {
        "deep": {"class": "premium", "admits": ["standard", "premium"]},
        "standard": {"class": "standard", "admits": ["cheap", "standard", "premium"]}
      }
    },
    "pi": {
      "standard": ["openrouter/deepseek/deepseek-v*-flash", "openrouter/z-ai/glm-*-flash", "openrouter/qwen/qwen*-flash"],
      "premium": [{"glob": "openrouter/anthropic/claude-opus-*", "requires": "pi_month_target"}],
      "tiers": {
        "deep": {"class": "standard", "admits": ["standard", "premium"]},
        "standard": {"class": "standard", "admits": ["standard"]}
      }
    }
  }
}
JSON
  NONE="$BATS_TEST_TMPDIR/absent.json"
  PI_DEEPSEEK=openrouter/deepseek/deepseek-v4.1-flash
}

# catalog <engine> <id-or-json-entry>... — write the catalog file; a bare id
# becomes {"id": id}.
catalog() {
  local engine=$1 entries=() a
  shift
  for a in "$@"; do
    if [[ $a == '{'* || $a == '"'* ]]; then entries+=("$a"); else entries+=("{\"id\":\"$a\"}"); fi
  done
  CATALOG="$BATS_TEST_TMPDIR/catalog.json"
  printf '{"%s":{"fetched_epoch":1,"source":"test","models":[%s]}}\n' "$engine" \
    "$(IFS=,; echo "${entries[*]}")" >"$CATALOG"
}

state() {
  STATE="$BATS_TEST_TMPDIR/state.json"
  printf '%s\n' "$1" >"$STATE"
}

# resolve <engine> <tier> — run against $SETTINGS, $CATALOG and $STATE (absent
# when unset).
resolve() {
  run --separate-stderr "$RESOLVE" "$1" "$2" "${CATALOG:-$NONE}" "${STATE:-$NONE}" <"$SETTINGS"
}

field() { jq -c "$1" <<<"$output"; }

@test "model-resolve: the first pattern outranks a newer match of a later one, newest within it" {
  catalog pi openrouter/z-ai/glm-6.0-flash openrouter/deepseek/deepseek-v4.2-flash \
    openrouter/deepseek/deepseek-v4.1-flash openrouter/deepseek/deepseek-v5-flash
  resolve pi standard
  [ "$status" -eq 0 ]
  [ "$(field .admitted)" = '["openrouter/deepseek/deepseek-v4.1-flash","openrouter/deepseek/deepseek-v4.1-flash-1017","openrouter/deepseek/deepseek-v5-flash","openrouter/deepseek/deepseek-v4.2-flash","openrouter/z-ai/glm-6.0-flash"]' ]
  [ "$(field .trial)" = '"openrouter/deepseek/deepseek-v5-flash"' ]
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]

  catalog pi openrouter/z-ai/glm-6.0-flash openrouter/deepseek/deepseek-v4.2-flash
  resolve pi standard
  [ "$(field .trial)" = '"openrouter/deepseek/deepseek-v4.2-flash"' ]
}

@test "model-resolve: pi orders by created before version, entries without created last" {
  catalog pi '{"id":"openrouter/deepseek/deepseek-v5-flash"}' \
    '{"id":"openrouter/deepseek/deepseek-v4.2-flash","created":100}' \
    '{"id":"openrouter/deepseek/deepseek-v4.3-flash","created":50}'
  resolve pi standard
  [ "$(field '.admitted[2:]')" = '["openrouter/deepseek/deepseek-v4.2-flash","openrouter/deepseek/deepseek-v4.3-flash","openrouter/deepseek/deepseek-v5-flash"]' ]
  [ "$(field .trial)" = '"openrouter/deepseek/deepseek-v4.2-flash"' ]
}

@test "model-resolve: claude orders by version before created" {
  catalog claude '{"id":"claude-opus-4-0"}' '{"id":"claude-opus-4-1","created":999}' '{"id":"claude-opus-5-0","created":1}'
  state '{"claude":{"premium":{"default":"claude-opus-4-0"}}}'
  resolve claude deep
  [ "$(field .trial)" = '"claude-opus-5-0"' ]
}

@test "model-resolve: trial is null when the newest match is the default, else the newer match" {
  catalog pi "$PI_DEEPSEEK"
  resolve pi standard
  [ "$(field .trial)" = null ]

  catalog pi "$PI_DEEPSEEK" openrouter/deepseek/deepseek-v4.2-flash
  resolve pi standard
  [ "$(field .trial)" = '"openrouter/deepseek/deepseek-v4.2-flash"' ]
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [ "$(field .warnings)" = '[]' ]
}

@test "model-resolve: a trial must be newer than the default" {
  catalog pi openrouter/deepseek/deepseek-v3-flash
  resolve pi standard
  [ "$(field .trial)" = null ]
  [[ "$(field .admitted)" == *deepseek-v3-flash* ]]
}

@test "model-resolve: an alias default outranks a versioned id of a later pattern, so there is no trial" {
  catalog claude claude-opus-5-0
  resolve claude deep
  [ "$(field .trial)" = null ]
}

@test "model-resolve: an incumbent missing from the catalog is kept when the seed lists it" {
  catalog pi openrouter/deepseek/deepseek-v4.2-flash
  state "{\"pi\":{\"standard\":{\"default\":\"$PI_DEEPSEEK\"}}}"
  resolve pi standard
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [[ "$(field '.warnings[0]')" == *"not in the catalog"* ]]
}

@test "model-resolve: an incumbent only the catalog supplied falls back to the seed default when it leaves" {
  state '{"pi":{"standard":{"default":"openrouter/deepseek/deepseek-v4.2-flash"}}}'
  catalog pi openrouter/deepseek/deepseek-v4.2-flash
  resolve pi standard
  [ "$(field .default)" = '"openrouter/deepseek/deepseek-v4.2-flash"' ]
  [ "$(field .warnings)" = '[]' ]

  catalog pi openrouter/deepseek/deepseek-v4.3-flash
  resolve pi standard
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [[ "$(field '.warnings[0]')" == *"model-resolve: pi standard"* ]]
  [[ "$(field '.warnings[0]')" == *"deepseek-v4.2-flash"* ]]
}

@test "model-resolve: an absent class-state file means the seed default, with no warnings" {
  catalog pi "$PI_DEEPSEEK" openrouter/deepseek/deepseek-v4.2-flash
  resolve pi standard
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [ "$(field .warnings)" = '[]' ]
}

@test "model-resolve: a garbled class-state file or a non-string default falls back with a warning" {
  printf 'not json' >"$BATS_TEST_TMPDIR/state.json"
  STATE="$BATS_TEST_TMPDIR/state.json"
  resolve pi standard
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [ "$(field '.warnings | length')" -eq 1 ]

  state '{"pi":{"standard":{"default":7}}}'
  resolve pi standard
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [ "$(field '.warnings | length')" -eq 1 ]
}

@test "model-resolve: a garbled catalog resolves to the seed with a warning" {
  printf '{"pi": ' >"$BATS_TEST_TMPDIR/catalog.json"
  CATALOG="$BATS_TEST_TMPDIR/catalog.json"
  resolve pi standard
  [ "$status" -eq 0 ]
  [ "$(field .admitted)" = '["openrouter/deepseek/deepseek-v4.1-flash","openrouter/deepseek/deepseek-v4.1-flash-1017"]' ]
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [ "$(field .trial)" = null ]
  [ "$(field '.warnings | length')" -eq 1 ]
}

@test "model-resolve: with no catalog a recorded default that passes the class check is honoured" {
  state '{"claude":{"standard":{"default":"claude-sonnet-5-0"}}}'
  resolve claude standard
  [ "$status" -eq 0 ]
  [ "$(field .default)" = '"claude-sonnet-5-0"' ]
  [ "$(field .warnings)" = '[]' ]
}

@test "model-resolve: a catalog entry of more than 5000 models resolves to the seed with a warning" {
  CATALOG="$BATS_TEST_TMPDIR/catalog.json"
  jq -nc '{pi: {models: [range(0; 5001) | {id: "openrouter/deepseek/deepseek-v\(. + 5)-flash"}]}}' >"$CATALOG"
  resolve pi standard
  [ "$status" -eq 0 ]
  [ "$(field .admitted | jq length)" -eq 2 ]
  [ "$(field .trial)" = null ]
  [ "$(field .warnings)" = '["model-resolve: catalog entry for engine pi has more than 5000 models; ignoring it"]' ]
}

@test "model-resolve: an oversized catalog resolves to the seed with a warning" {
  CATALOG="$BATS_TEST_TMPDIR/catalog.json"
  {
    printf '{"pi":{"models":[{"id":"openrouter/deepseek/deepseek-v9-flash","pad":"'
    head -c 4194400 /dev/zero | tr '\0' x
    printf '"}]}}'
  } >"$CATALOG"
  resolve pi standard
  [ "$status" -eq 0 ]
  [ "$(field .admitted | jq length)" -eq 2 ]
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
  [ "$(field .trial)" = null ]
  [ "$(field '.warnings | length')" -eq 1 ]
}

@test "model-resolve: an engine absent from the catalog is silent, a malformed engine entry warns" {
  catalog claude opus
  resolve pi standard
  [ "$(field .warnings)" = '[]' ]
  [ "$(field .trial)" = null ]

  printf '{"pi":"nope"}' >"$BATS_TEST_TMPDIR/catalog.json"
  CATALOG="$BATS_TEST_TMPDIR/catalog.json"
  resolve pi standard
  [ "$(field '.warnings | length')" -eq 1 ]
  [ "$(field .default)" = "\"$PI_DEEPSEEK\"" ]
}

@test "model-resolve: a forged premium default in a cheaper class is refused" {
  local forged
  for forged in opus claude-opus-5-5; do
    state "{\"claude\":{\"standard\":{\"default\":\"$forged\"}}}"
    resolve claude standard
    [ "$status" -eq 0 ]
    [ "$(field .default)" = '"sonnet"' ]
    [ "$(field '.warnings | length')" -eq 1 ]
    [[ "$(field '.warnings[0]')" == *"$forged"* ]]
    [[ "$(field .admitted)" == *'"opus"'* ]]
  done
}

@test "model-resolve: control characters in a recorded id never reach a warning" {
  state "$(jq -nc '{pi: {standard: {default: "x\u001b[31mred\u202e"}}}')"
  resolve pi standard
  [ "$(field '.warnings | length')" -eq 1 ]
  ! grep -q $'\x1b' <<<"$output"
  [[ "$(field '.warnings[0]')" != *$'\u202e'* ]]
}

@test "model-resolve: dated and preview ids are admitted but never the trial head" {
  catalog claude claude-opus-4-0 claude-opus-9-9-20261017 claude-opus-9-9-preview claude-opus-6-0 claude-opus-9-8-1017
  state '{"claude":{"premium":{"default":"claude-opus-4-0"}}}'
  resolve claude deep
  [ "$(field .trial)" = '"claude-opus-6-0"' ]
  [ "$(field '.admitted | index("claude-opus-9-9-20261017") != null and index("claude-opus-9-9-preview") != null')" = true ]
}

@test "model-resolve: ids that fail the engine shape check are never candidates" {
  catalog claude claude-opus-9-high 'claude-opus-9;x' '{"id":7}' '"junk"' claude-opus-5-0 claude-opus-4-0
  state '{"claude":{"premium":{"default":"claude-opus-4-0"}}}'
  resolve claude deep
  [ "$(field .admitted)" = '["opus","claude-opus-5-0","claude-opus-4-0"]' ]
  [ "$(field .trial)" = '"claude-opus-5-0"' ]
}

@test "model-resolve: a gated pattern lands in conditional, only in a row that admits its class" {
  catalog pi openrouter/anthropic/claude-opus-5-5
  resolve pi deep
  [ "$(field .conditional)" = '[{"id":"openrouter/anthropic/claude-opus-5-5","requires":"pi_month_target"}]' ]
  [[ "$(field .admitted)" != *claude-opus-5-5* ]]

  resolve pi standard
  [ "$(field .conditional)" = '[]' ]
  [[ "$(field .admitted)" != *claude-opus-5-5* ]]
}

@test "model-resolve: with no catalog, admitted and default are every shipped row's seed" {
  settings="$("$DISPATCH_CONFIG_BIN")"
  printf '%s\n' "$settings" >"$SETTINGS"
  mismatches=()
  rows=0
  while IFS=$'\t' read -r e t; do
    rows=$((rows + 1))
    resolve "$e" "$t"
    if [ "$status" -ne 0 ]; then
      mismatches+=("$e $t: exit $status: $stderr")
      continue
    fi
    want="$(jq -c --arg e "$e" --arg t "$t" '.modelMap[$e][$t] | [.models, .default]' "$SETTINGS")"
    got="$(jq -c '[.admitted, .default]' <<<"$output")"
    [ "$got" = "$want" ] || mismatches+=("$e $t: want $want, got $got")
  done < <(jq -r '.modelMap | to_entries[] | .key as $e | .value | keys[] | [$e, .] | @tsv' "$SETTINGS")
  [ "$rows" -gt 0 ]
  [ "$rows" -eq "$(jq '[.modelMap[] | keys[]] | length' "$SETTINGS")" ]
  [ "${#mismatches[@]}" -eq 0 ] && return 0
  printf '%s\n' "${#mismatches[@]} mismatches:" "${mismatches[@]}" >&2
  return 1
}

@test "model-resolve: a wrong argument count is a usage error" {
  run --separate-stderr "$RESOLVE" pi standard
  [ "$status" -eq 2 ]
  [[ "$stderr" == usage:* ]]
}

@test "model-resolve: stdin that is not one settings object is a usage error" {
  run --separate-stderr "$RESOLVE" pi standard "$NONE" "$NONE" </dev/null
  [ "$status" -eq 2 ]
  [[ "$stderr" == "model-resolve: stdin must be one settings JSON object"* ]]
}

@test "model-resolve: an unknown engine or tier is an error" {
  resolve pi nosuchtier
  [ "$status" -eq 1 ]
  [[ "$stderr" == "model-resolve: "* ]]
  resolve nosuchengine standard
  [ "$status" -eq 1 ]
}
