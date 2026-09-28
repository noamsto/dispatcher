bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CONFIG="$BATS_TEST_DIRNAME/../adapters/core/dispatch-config.sh"
  DEFAULTS="$BATS_TEST_DIRNAME/../adapters/core/defaults.json"
  unset DISPATCH_ENGINES DISPATCH_GRANT_ROOTS DISPATCH_OPENROUTER_MONTHLY_USD DISPATCH_OPENROUTER_KEY_FILE
  unset DISPATCH_PROFILE DISPATCH_REPO_TRACKERS DISPATCH_ORG_TRACKERS
  export XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/config"
  USER_FILE="$XDG_CONFIG_HOME/dispatcher/settings.json"
  LOCKED_FILE="$BATS_TEST_TMPDIR/locked.json"
  unset DISPATCH_LOCKED_SETTINGS
}

# user_settings <json> — write the user layer.
user_settings() {
  mkdir -p "${USER_FILE%/*}"
  printf '%s\n' "$1" >"$USER_FILE"
}

# locked_settings <json> — write the locked layer and point the resolver at it.
locked_settings() {
  printf '%s\n' "$1" >"$LOCKED_FILE"
  export DISPATCH_LOCKED_SETTINGS="$LOCKED_FILE"
}

@test "no user, locked or env layer: output is defaults.json" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(jq -S . <<<"$output")" = "$(jq -S . "$DEFAULTS")" ]
}

@test "user layer sets a key and leaves the base model map" {
  user_settings '{"engines":["claude"]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.engines == ["claude"]' <<<"$output"
  [ "$(jq -S .modelMap <<<"$output")" = "$(jq -S .modelMap "$DEFAULTS")" ]
}

@test "objects merge deeply; arrays are replaced wholesale" {
  user_settings '{"modelMap":{"pi":{"deep":{"default":"x"},"standard":{"models":["y"]}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.modelMap.pi.deep.default == "x"' <<<"$output"
  [ "$(jq -c .modelMap.pi.deep.models <<<"$output")" = "$(jq -c .modelMap.pi.deep.models "$DEFAULTS")" ]
  jq -e '.modelMap.pi.standard.models == ["y"]' <<<"$output"
}

@test "locked layer overrides the user layer" {
  user_settings '{"engines":["claude"]}'
  locked_settings '{"engines":["codex"]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.engines == ["codex"]' <<<"$output"
}

@test "env layer overrides every file layer" {
  user_settings '{"engines":["claude"],"openrouter":{"monthlyUsd":10}}'
  locked_settings '{"engines":["codex"],"grantRoots":["/l"],"openrouter":{"keyFile":"/lk"}}'
  DISPATCH_ENGINES="pi  claude" DISPATCH_GRANT_ROOTS="/a::/b" \
    DISPATCH_OPENROUTER_MONTHLY_USD=50 DISPATCH_OPENROUTER_KEY_FILE=/k \
    run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.engines == ["pi","claude"]' <<<"$output"
  jq -e '.grantRoots == ["/a","/b"]' <<<"$output"
  jq -e '.openrouter.monthlyUsd == "50"' <<<"$output"
  jq -e '.openrouter.keyFile == "/k"' <<<"$output"
}

@test "an empty env var contributes nothing" {
  locked_settings '{"engines":["codex"]}'
  DISPATCH_ENGINES="" run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.engines == ["codex"]' <<<"$output"
}

@test "grantRoots and openrouter.keyFile are ignored from the user layer, kept from locked" {
  user_settings '{"grantRoots":["/u"],"openrouter":{"keyFile":"/uk","monthlyUsd":25}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e 'has("grantRoots") | not' <<<"$output"
  jq -e '.openrouter | has("keyFile") | not' <<<"$output"
  jq -e '.openrouter.monthlyUsd == 25' <<<"$output"
  [[ "$stderr" == *"ignoring grantRoots from $USER_FILE"* ]]
  [[ "$stderr" == *"ignoring openrouter.keyFile from $USER_FILE"* ]]

  locked_settings '{"grantRoots":["/l"],"openrouter":{"keyFile":"/lk"}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.grantRoots == ["/l"]' <<<"$output"
  jq -e '.openrouter.keyFile == "/lk"' <<<"$output"
  [[ "$stderr" != *"$LOCKED_FILE"* ]]
}

@test "--show-origin names the last layer holding each leaf" {
  user_settings '{"modelMap":{"pi":{"deep":{"default":"u"}}}}'
  locked_settings '{"openrouter":{"monthlyUsd":5}}'
  DISPATCH_ENGINES="claude pi" run --separate-stderr "$CONFIG" --show-origin
  [ "$status" -eq 0 ]
  jq -e '.modelMap.claude.deep.default == {"value":"opus","origin":"base"}' <<<"$output"
  jq -e '.modelMap.pi.deep.default == {"value":"u","origin":"user"}' <<<"$output"
  jq -e '.modelMap.pi.deep.models.origin == "base"' <<<"$output"
  jq -e '.modelMap.pi.deep.models.value | type == "array"' <<<"$output"
  jq -e '.openrouter.monthlyUsd == {"value":5,"origin":"locked"}' <<<"$output"
  jq -e '.engines == {"value":["claude","pi"],"origin":"env"}' <<<"$output"
}

@test "a malformed or non-object user file is refused, naming it" {
  user_settings '{'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$USER_FILE"* ]]

  user_settings '[1]'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$USER_FILE is not a JSON object"* ]]
}

@test "a missing locked settings file is refused, naming it" {
  export DISPATCH_LOCKED_SETTINGS="$BATS_TEST_TMPDIR/nope.json"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$BATS_TEST_TMPDIR/nope.json"* ]]
}

@test "merged settings are validated, naming the key" {
  locked_settings '{"grantRoots":["/a:b"]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *grantRoots* ]]

  unset DISPATCH_LOCKED_SETTINGS
  user_settings '{"engines":"claude"}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *engines* ]]
}

@test "an unknown argument exits 2 with a usage line" {
  run --separate-stderr "$CONFIG" --bogus
  [ "$status" -eq 2 ]
  [[ "$stderr" == *usage:* ]]
}

@test "engines must be a non-empty array of strings" {
  user_settings '{"engines":[]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *engines* ]]
}

@test "openrouter must be an object" {
  user_settings '{"openrouter":"x"}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *openrouter* ]]
}

@test "a non-object locked openrouter is refused" {
  locked_settings '{"openrouter":[]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *openrouter* ]]
}

@test "a whitespace-only DISPATCH_ENGINES contributes no engines layer" {
  DISPATCH_ENGINES=" " run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  [ "$(jq -c .engines <<<"$output")" = "$(jq -c .engines "$DEFAULTS")" ]
}

@test "a blank engine name in the user or locked layer is refused, naming that layer's file" {
  user_settings '{"engines":[""]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$USER_FILE sets a blank engine name in engines"* ]]

  unset DISPATCH_LOCKED_SETTINGS
  user_settings '{"engines":["claude"]}'
  locked_settings '{"engines":["claude"," "]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$LOCKED_FILE sets a blank engine name in engines"* ]]
}

@test "DISPATCH_PROFILE sets profile" {
  DISPATCH_PROFILE=work run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.profile == "work"' <<<"$output"

  DISPATCH_PROFILE=work run --separate-stderr "$CONFIG" --show-origin
  [ "$status" -eq 0 ]
  jq -e '.profile == {"value":"work","origin":"env"}' <<<"$output"
}

@test "DISPATCH_REPO_TRACKERS and DISPATCH_ORG_TRACKERS parse whitespace-separated key=value pairs" {
  DISPATCH_REPO_TRACKERS=$'a/b=github\n  C/D=linear:ENG' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.repoTrackers == {"a/b":"github","c/d":"linear:ENG"}' <<<"$output"

  DISPATCH_ORG_TRACKERS='org=linear:X' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.orgTrackers == {"org":"linear:X"}' <<<"$output"
}

@test "a tracker entry without '=' maps to itself, never a valid tracker" {
  DISPATCH_REPO_TRACKERS='junk a/b=github' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.repoTrackers == {"junk":"junk","a/b":"github"}' <<<"$output"
}

@test "a tracker key repeated in one env var is last-wins" {
  DISPATCH_REPO_TRACKERS='a/b=github a/b=linear:X' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.repoTrackers["a/b"] == "linear:X"' <<<"$output"
}

@test "a tracker key repeated with different case in one env var is still last-wins" {
  DISPATCH_REPO_TRACKERS='a/b=github A/B=linear:X a/b=github' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.repoTrackers == {"a/b":"github"}' <<<"$output"
}

@test "a whitespace-only tracker env var contributes nothing" {
  DISPATCH_REPO_TRACKERS=$'  \n ' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e 'has("repoTrackers") | not' <<<"$output"
}

@test "tracker maps merge per key across layers, case-insensitively" {
  user_settings '{"repoTrackers":{"Own/Repo":"github","x/y":"github"}}'
  locked_settings '{"repoTrackers":{"own/repo":"linear:ENG"}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.repoTrackers == {"own/repo":"linear:ENG","x/y":"github"}' <<<"$output"

  DISPATCH_REPO_TRACKERS='OWN/REPO=github' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.repoTrackers == {"own/repo":"github","x/y":"github"}' <<<"$output"
}

@test "profile and tracker maps are validated, naming the key" {
  user_settings '{"profile":1}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *profile* ]]

  user_settings '{"repoTrackers":["x"]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *repoTrackers* ]]

  user_settings '{"orgTrackers":{"o":1}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *orgTrackers* ]]
}

@test "a baked build ignores DISPATCH_LOCKED_SETTINGS, warning when it differs" {
  locked_settings '{"engines":["codex"]}'
  local baked="$BATS_TEST_TMPDIR/baked.sh"
  sed -e "s|@lockedSettings@|$LOCKED_FILE|" -e "s|@defaultsJson@|$DEFAULTS|" "$CONFIG" >"$baked"
  chmod +x "$baked"

  local other="$BATS_TEST_TMPDIR/other.json"
  printf '%s\n' '{"engines":["pi"]}' >"$other"

  DISPATCH_LOCKED_SETTINGS="$other" run --separate-stderr "$baked"
  [ "$status" -eq 0 ]
  jq -e '.engines == ["codex"]' <<<"$output"
  [[ "$stderr" == *"ignoring DISPATCH_LOCKED_SETTINGS"* ]]

  unset DISPATCH_LOCKED_SETTINGS
  run --separate-stderr "$baked"
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]

  DISPATCH_LOCKED_SETTINGS="$LOCKED_FILE" run --separate-stderr "$baked"
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
}

@test "a baked build with an unreadable locked file is refused, naming it" {
  local missing="$BATS_TEST_TMPDIR/nope.json"
  local baked="$BATS_TEST_TMPDIR/baked-missing.sh"
  sed -e "s|@lockedSettings@|$missing|" -e "s|@defaultsJson@|$DEFAULTS|" "$CONFIG" >"$baked"
  chmod +x "$baked"

  run --separate-stderr "$baked"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$missing"* ]]
}
