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


# Each row overwrites the user settings file (user_settings truncates it).
# These rows do not touch STUB_DIR or PATH.
shape_refuse_row() { # json fragment
  local fragment=$2
  fragment=${fragment//@USER_FILE@/$USER_FILE}
  user_settings "$1"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$fragment"* ]]
}

shape_accept_row() { # json jq1 jq2
  user_settings "$1"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e "$2" <<<"$output"
  jq -e "$3" <<<"$output"
}

localmodels_refuse_row() { # json fragment
  user_settings "$1"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *localModels* ]]
  [[ "$stderr" == *"$2"* ]]
}

tracker_row() { # env jq
  DISPATCH_REPO_TRACKERS="$1" run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e "$2" <<<"$output"
}

# F22: malformed modelMap and paceDowngrades values, each refused with status 1
# and a path fragment. The first row of each group names the user layer.
@test "a malformed modelMap or paceDowngrades value is refused" {
  begin_rows
  local row json fragment
  while IFS='|' read -r row json fragment; do
    [ -n "$row" ] || continue
    keep_row "$row" shape_refuse_row "$json" "$fragment"
  done <<'ROWS'
models-type|{"modelMap":{"claude":{"deep":{"models":"x"}}}}|@USER_FILE@ (user layer): modelMap.claude.deep.models must be an array of strings
default-type|{"modelMap":{"claude":{"deep":{"default":1}}}}|modelMap.claude.deep.default must be a string
map-object|{"modelMap":{"claude":"x"}}|modelMap.claude must be an object
pace-array|{"paceDowngrades":{"claude":{"models":["opus"],"to":"sonnet"}}}|@USER_FILE@ (user layer): paceDowngrades.claude must be an array
pace-to|{"paceDowngrades":{"claude":[{"models":["opus"]}]}}|paceDowngrades.claude[0].to must be a string
pace-models|{"paceDowngrades":{"claude":[{"models":"opus","to":"sonnet"}]}}|paceDowngrades.claude[0].models must be an array of strings
ROWS
  finish_rows 6
}

@test "a malformed escalation rule is refused, naming the locked layer and the path" {
  locked_settings '{"escalation":{"claude":{"deep":[{"failed":["sonnet"],"baseline":"sonnet","inRow":["opus"],"outOfRow":["fable"]}]}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$LOCKED_FILE (locked layer): escalation.claude.deep[0] must be exactly one of inRow or outOfRow"* ]]

  locked_settings '{"escalation":{"claude":{"deep":[{"failed":"sonnet","baseline":"sonnet","inRow":["opus"]}]}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"escalation.claude.deep[0].failed must be an array of strings"* ]]
}

@test "a malformed burnClasses table is refused, naming the layer and the path" {
  user_settings '{"burnClasses":{"claude":{"model":"opus"}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$USER_FILE (user layer): burnClasses must be an array"* ]]

  user_settings '{"burnClasses":[{"class":"premium","weight":4}]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"burnClasses[0].match must be a string"* ]]

  user_settings '{"burnClasses":[{"match":"*opus*","class":"premium"}]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"burnClasses[0].weight must be a number"* ]]

  user_settings '{"burnClasses":[{"match":"*opus*","class":"premium","weight":4,"byEffort":{"default":{"class":"premium","weight":4}}}]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"burnClasses[0] must be exactly one of class or byEffort"* ]]

  user_settings '{"burnClasses":[{"match":"*opus*","byEffort":{"high":{"class":"premium","weight":4}}}]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"burnClasses[0].byEffort must be a default entry"* ]]

  user_settings '{"burnClasses":[{"match":"*opus*","byEffort":{"default":{"class":"premium","weight":"4"}}}]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"burnClasses[0].byEffort.default.weight must be a number"* ]]
}

@test "a malformed orchestratorDefaults is refused, naming the layer and the path" {
  user_settings '{"orchestratorDefaults":"x"}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$USER_FILE (user layer): orchestratorDefaults must be an object"* ]]

  user_settings '{"orchestratorDefaults":{"claude":"opus"}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"orchestratorDefaults.claude must be an object"* ]]

  user_settings '{"orchestratorDefaults":{"claude":{"effort":"high"}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"orchestratorDefaults.claude.model must be a string"* ]]

  user_settings '{"orchestratorDefaults":{"claude":{"model":"opus","effort":1}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"orchestratorDefaults.claude.effort must be a string"* ]]
}

# F23: a well-shaped partial layer is accepted. The burnClasses row overrides
# only the model; the base effort survives the deep merge.
@test "a well-shaped partial layer is accepted" {
  begin_rows
  local row json jq1 jq2
  while IFS='|' read -r row json jq1 jq2; do
    [ -n "$row" ] || continue
    keep_row "$row" shape_accept_row "$json" "$jq1" "$jq2"
  done <<'ROWS'
burn-orch|{"burnClasses":[{"match":"*opus*","byEffort":{"low":{"class":"standard","weight":2},"default":{"class":"premium","weight":4}}}],"orchestratorDefaults":{"pi":{"model":"openrouter/deepseek/deepseek-v4-flash"}}}|.orchestratorDefaults.pi.model == "openrouter/deepseek/deepseek-v4-flash"|.orchestratorDefaults.pi.effort == "high"
map-pace|{"modelMap":{"cursor":{"deep":{"regex":["^x$"]}}},"escalation":{"claude":{"deep":[{"failed":["sonnet"],"baseline":"sonnet","inRow":["opus"]}]}},"paceDowngrades":{"claude":[{"models":["opus"],"to":"sonnet"}]}}|.modelMap.cursor.deep.regex == ["^x$"]|.paceDowngrades.claude == [{"models":["opus"],"to":"sonnet"}]
ROWS
  finish_rows 2
}

@test "a malformed base defaults file is refused, naming the base layer" {
  local bad="$BATS_TEST_TMPDIR/bad-defaults.json"
  printf '%s\n' '{"modelMap":{"claude":{"deep":{"models":"x"}}}}' >"$bad"
  local baked="$BATS_TEST_TMPDIR/baked-bad-defaults.sh"
  sed -e "s|@lockedSettings@|$BATS_TEST_TMPDIR/nope.json|" -e "s|@defaultsJson@|$bad|" "$CONFIG" >"$baked"
  chmod +x "$baked"
  run --separate-stderr "$baked"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$bad (base layer): modelMap.claude.deep.models must be an array of strings"* ]]
}

@test "a jq failure during shape validation fails closed, naming the layer" {
  local shim="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$shim"
  local real_jq="$(command -v jq)"
  cat >"$shim/jq" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
  case "\$a" in *modelMap*) echo "jq: validation query failed" >&2; exit 3 ;; esac
done
exec "$real_jq" "\$@"
EOF
  chmod +x "$shim/jq"
  PATH="$shim:$PATH" run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"(base layer): could not validate the modelMap / escalation / paceDowngrades / burnClasses / orchestratorDefaults shapes"* ]]
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

# F24: one DISPATCH_REPO_TRACKERS value, one repoTrackers object.
@test "repo tracker env entries resolve per row" {
  begin_rows
  local row env expr
  while IFS='|' read -r row env expr; do
    [ -n "$row" ] || continue
    keep_row "$row" tracker_row "$env" "$expr"
  done <<'ROWS'
no-equals|junk a/b=github|.repoTrackers == {"junk":"junk","a/b":"github"}
last-wins|a/b=github a/b=linear:X|.repoTrackers["a/b"] == "linear:X"
case-fold|a/b=github A/B=linear:X a/b=github|.repoTrackers == {"a/b":"github"}
ROWS
  finish_rows 3
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

@test "localModels merges per entry across layers and is validated" {
  user_settings '{"localModels":{
    "lemonade/Qwen3.8-Flash-Next-MTP":{"baseUrl":"http://halo:13305/v1","contextWindow":131072},
    "lemonade/Other":{"baseUrl":"http://halo:13305/v1","contextWindow":4096}}}'
  locked_settings '{"localModels":{"lemonade/Qwen3.8-Flash-Next-MTP":{"maxConcurrent":2}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.localModels == {
    "lemonade/Qwen3.8-Flash-Next-MTP":{"baseUrl":"http://halo:13305/v1","contextWindow":131072,"maxConcurrent":2},
    "lemonade/Other":{"baseUrl":"http://halo:13305/v1","contextWindow":4096}}' <<<"$output"

  unset DISPATCH_LOCKED_SETTINGS
  user_settings '{"localModels":{"lemonade/deep-one":{"baseUrl":"http://h:1/v1","contextWindow":8192,"maxConcurrent":3,"tiers":["deep"]}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
}

@test "malformed localModels is refused, naming the path" {
  local ok='"baseUrl":"http://h:1/v1","contextWindow":8192'
  begin_rows
  local json fragment
  while IFS='|' read -r json fragment; do
    [ -n "$json" ] || continue
    keep_row "$fragment" localmodels_refuse_row "{\"localModels\":$json}" "$fragment"
  done <<EOF
{"lemonade/q:7b":{$ok}}|lemonade/q:7b
{"qwen":{$ok}}|qwen
{"lemonade/q":{"contextWindow":8192}}|lemonade/q.baseUrl
{"lemonade/q":{"baseUrl":"ftp://x","contextWindow":8192}}|lemonade/q.baseUrl
{"lemonade/q":{"baseUrl":"http://h:1/v1/","contextWindow":8192}}|lemonade/q.baseUrl
{"lemonade/q":{"baseUrl":"http://h:1/v1"}}|lemonade/q.contextWindow
{"lemonade/q":{"baseUrl":"http://h:1/v1","contextWindow":0}}|lemonade/q.contextWindow
{"lemonade/q":{$ok,"maxConcurrent":1.5}}|lemonade/q.maxConcurrent
{"lemonade/q":{$ok,"tiers":[]}}|lemonade/q.tiers
{"lemonade/q":{$ok,"tiers":["huge"]}}|lemonade/q.tiers
{"lemonade/q":{$ok,"maxconcurrent":1}}|lemonade/q
{"openrouter/foo":{$ok}}|openrouter/foo
{"OpenRouter/foo":{$ok}}|OpenRouter/foo
{"lemonade/a":{$ok},"lemonade/b":{"baseUrl":"http://other:1/v1","contextWindow":8192}}|lemonade/b
[]|localModels
EOF
  finish_rows 15
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

@test "--layers reports base, user absence, and no locked file" {
  run --separate-stderr "$CONFIG" --layers
  [ "$status" -eq 0 ]
  jq -e '(.base | endswith("/adapters/core/defaults.json")) and .user.present == false and (.user.path | endswith("/dispatcher/settings.json")) and .locked == null' <<<"$output"
}

@test "--layers reports user presence and the locked path" {
  user_settings '{"engines":["claude"]}'
  locked_settings '{"engines":["codex"]}'
  run --separate-stderr "$CONFIG" --layers
  [ "$status" -eq 0 ]
  jq -e --arg l "$LOCKED_FILE" '.user.present == true and .locked == $l' <<<"$output"
}

@test "--layers reads no layer contents, so a malformed user file doesn't fail" {
  user_settings '{'
  run --separate-stderr "$CONFIG" --layers
  [ "$status" -eq 0 ]
}

@test "--layers and --show-origin together exit 2 with usage" {
  run --separate-stderr "$CONFIG" --layers --show-origin
  [ "$status" -eq 2 ]
  [[ "$stderr" == *usage:* ]]

  run --separate-stderr "$CONFIG" --show-origin --layers
  [ "$status" -eq 2 ]
  [[ "$stderr" == *usage:* ]]
}
