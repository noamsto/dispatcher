bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  CONFIG="$BATS_TEST_DIRNAME/../adapters/core/dispatch-config.sh"
  DEFAULTS="$BATS_TEST_DIRNAME/../adapters/core/defaults.json"
  unset DISPATCH_ENGINES DISPATCH_GRANT_ROOTS DISPATCH_OPENROUTER_MONTHLY_USD DISPATCH_OPENROUTER_KEY_FILE
  unset DISPATCH_PROFILE DISPATCH_REPO_TRACKERS DISPATCH_ORG_TRACKERS DISPATCH_ROSTER_AUTO_OPEN DISPATCH_DISPATCHER_MCP
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

@test "workerDeniedMcpServers must be an array of MCP server names" {
  for bad in '"x"' '[1]' '["a/b"]' '["a b"]' '[""]'; do
    user_settings "{\"workerDeniedMcpServers\":$bad}"
    run --separate-stderr "$CONFIG"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"workerDeniedMcpServers must be an array of MCP server names"* ]]
  done
}

@test "workerDeniedMcpServers defaults to the interactive browser MCP servers and takes an override" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.workerDeniedMcpServers == ["plugin_hm_playwright", "plugin_hm_firefox-devtools"]' <<<"$output"
  user_settings '{"workerDeniedMcpServers":["my_browser"]}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.workerDeniedMcpServers == ["my_browser"]' <<<"$output"
}

@test "dispatcherChrome defaults to false and takes a boolean from the user layer" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.dispatcherChrome == false' <<<"$output"
  user_settings '{"dispatcherChrome":true}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.dispatcherChrome == true' <<<"$output"
}

@test "dispatcherChrome must be a boolean" {
  for bad in '"yes"' '1'; do
    user_settings "{\"dispatcherChrome\":$bad}"
    run --separate-stderr "$CONFIG"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"dispatcherChrome must be a boolean"* ]]
  done
}

@test "freshSeams defaults to false and takes a boolean from the user layer" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.freshSeams == false' <<<"$output"
  user_settings '{"freshSeams":true}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.freshSeams == true' <<<"$output"
}

@test "freshSeams must be a boolean" {
  for bad in '"yes"' '1'; do
    user_settings "{\"freshSeams\":$bad}"
    run --separate-stderr "$CONFIG"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"freshSeams must be a boolean"* ]]
  done
}

@test "dispatcherMcp defaults to empty and merges per lowercased key across layers" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.dispatcherMcp == {}' <<<"$output"
  user_settings '{"dispatcherMcp":{"Factify-Inc":["claudeai"],"noamsto/dispatcher":["analytics"]}}'
  locked_settings '{"dispatcherMcp":{"factify-inc":["analytics"]}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.dispatcherMcp == {"factify-inc":["analytics"],"noamsto/dispatcher":["analytics"]}' <<<"$output"
}

@test "DISPATCH_DISPATCHER_MCP parses key=list pairs, an empty list included" {
  user_settings '{"dispatcherMcp":{"factify-inc":["claudeai"]}}'
  DISPATCH_DISPATCHER_MCP=$'Factify-Inc/Mono=claudeai,,analytics\n factify-inc= a/b=x a/b=analytics' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.dispatcherMcp == {"factify-inc/mono":["claudeai","analytics"],"factify-inc":[],"a/b":["analytics"]}' <<<"$output"
  DISPATCH_DISPATCHER_MCP='A/B=claudeai' run --separate-stderr "$CONFIG" --show-origin
  [ "$status" -eq 0 ]
  jq -e '.dispatcherMcp["a/b"] == {"value":["claudeai"],"origin":"env"} and .dispatcherMcp["factify-inc"].origin == "user"' <<<"$output"
}

@test "dispatcherMcp refuses an unknown opt-in, a non-array, a bad key and a bare env entry" {
  local bad
  for bad in '{"a/b":["nosuch"]}' '{"a/b":"claudeai"}' '{"a b":["claudeai"]}' '{"a/b/c":[]}' '["claudeai"]'; do
    user_settings "{\"dispatcherMcp\":$bad}"
    run --separate-stderr "$CONFIG"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"dispatcherMcp must be an object keyed owner/repo or org"* ]]
  done
  rm -f "$USER_FILE"
  DISPATCH_DISPATCHER_MCP='factify-inc' run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"dispatcherMcp must be an object keyed owner/repo or org"* ]]
}

# #1304: the mcpServers catalog. dispatcherMcp names a catalog entry the same
# way it names claudeai or analytics.
@test "mcpServers ships a default catalog that dispatcherMcp can name" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.mcpServers | keys == ["elevenlabs", "posthog", "sentry"]' <<<"$output"
  jq -e '.mcpServers.sentry == {"url": "https://mcp.sentry.dev/mcp", "description": "Sentry issues, events and releases"}' <<<"$output"
  user_settings '{"dispatcherMcp":{"noamsto/toddl":["sentry","posthog"],"factify-inc":["claudeai","analytics","grafana"]},"mcpServers":{"grafana":{"url":"https://mcp.grafana.example/mcp","headers":{"Authorization":"Bearer ${GRAFANA_TOKEN}","X-Org":"${GRAFANA_ORG}","X-Key":"!pass show grafana"}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.dispatcherMcp["noamsto/toddl"] == ["sentry", "posthog"]' <<<"$output"
  jq -e '.mcpServers | keys == ["elevenlabs", "grafana", "posthog", "sentry"]' <<<"$output"
  jq -e '.mcpServers.grafana.headers.Authorization == "Bearer ${GRAFANA_TOKEN}"' <<<"$output"
  # A later layer refines one field of a default entry.
  locked_settings '{"mcpServers":{"sentry":{"url":"https://sentry.example/mcp"}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.mcpServers.sentry == {"url": "https://sentry.example/mcp", "description": "Sentry issues, events and releases"}' <<<"$output"
  DISPATCH_DISPATCHER_MCP='A/B=sentry,claudeai' run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.dispatcherMcp["a/b"] == ["sentry", "claudeai"]' <<<"$output"
}

@test "dispatcherMcp refuses an unknown catalog name, naming the key and the valid names" {
  user_settings '{"dispatcherMcp":{"noamsto/toddl":["sentry","nosuch"]}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *'"noamsto/toddl" names unknown "nosuch" (valid: claudeai, analytics, elevenlabs, posthog, sentry)'* ]]
  rm -f "$USER_FILE"
  DISPATCH_DISPATCHER_MCP='a/b=sentry,linear' run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *'"a/b" names unknown "linear"'* ]]
}

@test "mcpServers refuses an invalid entry or a literal header value" {
  local row bad want
  begin_rows
  while IFS='|' read -r row bad want; do
    [ -n "$row" ] || continue
    keep_row "$row" mcp_server_refused_row "$bad" "$want"
  done <<'ROWS'
not-object|"x"|mcpServers must be an object
bad-name|{"a b":{"url":"https://a.example/mcp"}}|mcpServers.a b must be a name of letters
reserved|{"claudeai":{"url":"https://a.example/mcp"}}|mcpServers.claudeai must be a name other than claudeai or analytics
dash-underscore|{"my-x":{"url":"https://a.example/mcp"},"my_x":{"url":"https://b.example/mcp"}}|mcpServers.my_x must be a name distinct from "my-x"
entry-not-object|{"a":"https://a.example/mcp"}|mcpServers.a must be an object
no-url|{"a":{"description":"x"}}|mcpServers.a.url must be an https URL
http-url|{"a":{"url":"http://a.example/mcp"}}|mcpServers.a.url must be an https URL
quote-url|{"a":{"url":"https://a.example/\"x"}}|mcpServers.a.url must be an https URL
stdio|{"a":{"url":"https://a.example/mcp","command":"npx"}}|mcpServers.a.command must be one of url, description, headers
control-description|{"a":{"url":"https://a.example/mcp","description":"x\ny"}}|mcpServers.a.description must be a string
headers-not-object|{"a":{"url":"https://a.example/mcp","headers":["x"]}}|mcpServers.a.headers must be an object
bad-header-name|{"a":{"url":"https://a.example/mcp","headers":{"X Key":"${K}"}}}|mcpServers.a.headers.X Key must be a header name
literal-bearer|{"a":{"url":"https://a.example/mcp","headers":{"Authorization":"Bearer sk-live-abc123"}}}|mcpServers.a.headers.Authorization must be an env reference
literal-key|{"a":{"url":"https://a.example/mcp","headers":{"X-Api-Key":"abc123"}}}|mcpServers.a.headers.X-Api-Key must be an env reference
env-with-literal|{"a":{"url":"https://a.example/mcp","headers":{"X-Api-Key":"sk-${K}"}}}|mcpServers.a.headers.X-Api-Key must be an env reference
bare-bang|{"a":{"url":"https://a.example/mcp","headers":{"X-Api-Key":"!"}}}|mcpServers.a.headers.X-Api-Key must be an env reference
ROWS
  finish_rows 16
}

mcp_server_refused_row() { # catalog-json want
  user_settings "{\"mcpServers\":$1}"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$2"* ]]
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

  user_settings '{"orchestratorDefaults":{"claude":{"model":"opus","autoCompact":"lots"}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"orchestratorDefaults.claude.autoCompact must be a positive integer"* ]]

  user_settings '{"orchestratorDefaults":{"claude":{"model":"opus","autoCompact":0}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"orchestratorDefaults.claude.autoCompact must be a positive integer"* ]]
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
orch-ac|{"orchestratorDefaults":{"claude":{"model":"opus","autoCompact":250000}}}|.orchestratorDefaults.claude.autoCompact == 250000|.orchestratorDefaults.claude.effort == "high"
ROWS
  finish_rows 3
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
  [[ "$stderr" == *"(base layer): could not validate the modelMap / escalation / paceDowngrades / burnClasses / orchestratorDefaults / rosterDiagram shapes"* ]]
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

@test "fleetBudget defaults to 8; DISPATCH_FLEET_BUDGET and the user layer override it" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.fleetBudget == 8' <<<"$output"

  DISPATCH_FLEET_BUDGET=0 run --separate-stderr "$CONFIG" --show-origin
  [ "$status" -eq 0 ]
  jq -e '.fleetBudget == {"value":0,"origin":"env"}' <<<"$output"

  user_settings '{"fleetBudget":12}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.fleetBudget == 12' <<<"$output"
}

@test "a fleetBudget that is not a non-negative integer is refused" {
  DISPATCH_FLEET_BUDGET=lots run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"fleetBudget must be a non-negative integer (merged settings)"* ]]

  user_settings '{"fleetBudget":-1}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"fleetBudget must be a non-negative integer"* ]]
}

@test "rosterDiagram.autoOpen defaults to true" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.rosterDiagram.autoOpen == true' <<<"$output"
}

@test "DISPATCH_ROSTER_AUTO_OPEN false-like values turn rosterDiagram.autoOpen off, any other non-empty value on" {
  local v
  for v in 0 false no off OFF False; do
    DISPATCH_ROSTER_AUTO_OPEN="$v" run --separate-stderr "$CONFIG"
    [ "$status" -eq 0 ]
    jq -e '.rosterDiagram.autoOpen == false' <<<"$output"
  done

  user_settings '{"rosterDiagram":{"autoOpen":false}}'
  for v in 1 yes on anything; do
    DISPATCH_ROSTER_AUTO_OPEN="$v" run --separate-stderr "$CONFIG"
    [ "$status" -eq 0 ]
    jq -e '.rosterDiagram.autoOpen == true' <<<"$output"
  done

  DISPATCH_ROSTER_AUTO_OPEN= run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.rosterDiagram.autoOpen == false' <<<"$output"

  DISPATCH_ROSTER_AUTO_OPEN=off run --separate-stderr "$CONFIG" --show-origin
  [ "$status" -eq 0 ]
  jq -e '.rosterDiagram.autoOpen == {"value":false,"origin":"env"}' <<<"$output"
}

@test "a malformed rosterDiagram is refused, naming the layer and the path" {
  user_settings '{"rosterDiagram":"x"}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$USER_FILE (user layer): rosterDiagram must be an object"* ]]

  user_settings '{"rosterDiagram":{"autoOpen":"no"}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"$USER_FILE (user layer): rosterDiagram.autoOpen must be a boolean"* ]]
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
    "lemonade/Qwen3.8-Flash-Next-MTP":{"baseUrl":"http://halo:13305/v1","contextWindow":131072,"maxTokens":32768},
    "lemonade/Other":{"baseUrl":"http://halo:13305/v1","contextWindow":4096}}}'
  locked_settings '{"localModels":{"lemonade/Qwen3.8-Flash-Next-MTP":{"maxConcurrent":2}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.localModels == {
    "lemonade/Qwen3.8-Flash-Next-MTP":{"baseUrl":"http://halo:13305/v1","contextWindow":131072,"maxTokens":32768,"maxConcurrent":2},
    "lemonade/Other":{"baseUrl":"http://halo:13305/v1","contextWindow":4096}}' <<<"$output"

  unset DISPATCH_LOCKED_SETTINGS
  user_settings '{"localModels":{"lemonade/deep-one":{"baseUrl":"http://h:1/v1","contextWindow":8192,"maxConcurrent":3,"tiers":["deep"]}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]

  # Asserted by message, not only by path: an unknown field names the same path.
  user_settings '{"localModels":{"lemonade/deep-one":{"baseUrl":"http://h:1/v1","contextWindow":8192,"maxTokens":0}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"localModels.lemonade/deep-one.maxTokens must be a positive integer"* ]]

  # #1141: `input` is the pi input-modality declaration, so a vision endpoint
  # declares ["text","image"] and it survives the merge unchanged.
  user_settings '{"localModels":{"lemonade/vision":{"baseUrl":"http://h:1/v1","contextWindow":8192,"input":["text","image"]}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.localModels."lemonade/vision".input == ["text","image"]' <<<"$output"
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
{"lemonade/q":{$ok,"reasoning":"yes"}}|lemonade/q.reasoning
{"lemonade/q":{$ok,"input":"text"}}|lemonade/q.input
{"lemonade/q":{$ok,"input":[]}}|lemonade/q.input
{"lemonade/q":{$ok,"input":["image"]}}|lemonade/q.input
{"lemonade/q":{$ok,"input":["text","vision"]}}|lemonade/q.input
{"lemonade/q":{$ok,"input":["text","text"]}}|lemonade/q.input
{"lemonade/q":{$ok,"input":[7]}}|lemonade/q.input
{"lemonade/q":{$ok,"thinkingFormat":7}}|lemonade/q.thinkingFormat
{"lemonade/q":{$ok,"thinkingLevelMap":[]}}|lemonade/q.thinkingLevelMap
{"lemonade/q":{$ok,"thinkingLevelMap":{"high":5}}}|lemonade/q.thinkingLevelMap
{"lemonade/q":{$ok,"samplingParamsByThinkingLevel":{"off":0.7}}}|lemonade/q.samplingParamsByThinkingLevel
{"lemonade/q":{$ok,"samplingParams":"hot"}}|lemonade/q.samplingParams
{"lemonade/q":{$ok,"samplingParamsByThinkingLevel":1}}|lemonade/q.samplingParamsByThinkingLevel
{"lemonade/q":{$ok,"effortThinking":{"low":false}}}|lemonade/q.effortThinking
{"lemonade/q":{$ok,"effortThinking":{"ultra":"off"}}}|lemonade/q.effortThinking
{"lemonade/q":{$ok,"effortThinking":{"low":"off; id"}}}|lemonade/q.effortThinking
{"lemonade/q":{"baseUrl":"ftp://x","contextWindow":8192}}|lemonade/q.baseUrl
{"lemonade/q":{"baseUrl":"http://h:1/v1/","contextWindow":8192}}|lemonade/q.baseUrl
{"lemonade/q":{"baseUrl":"http://h:1/v1"}}|lemonade/q.contextWindow
{"lemonade/q":{"baseUrl":"http://h:1/v1","contextWindow":0}}|lemonade/q.contextWindow
{"lemonade/q":{$ok,"maxConcurrent":1.5}}|lemonade/q.maxConcurrent
{"lemonade/q":{$ok,"maxTokens":1.5}}|lemonade/q.maxTokens
{"lemonade/q":{$ok,"maxTokens":0}}|lemonade/q.maxTokens
{"lemonade/q":{$ok,"maxTokens":-1}}|lemonade/q.maxTokens
{"lemonade/q":{$ok,"maxTokens":"65536"}}|lemonade/q.maxTokens
{"lemonade/q":{$ok,"workerNotes":7}}|lemonade/q.workerNotes
{"lemonade/q":{$ok,"tiers":[]}}|lemonade/q.tiers
{"lemonade/q":{$ok,"tiers":["huge"]}}|lemonade/q.tiers
{"lemonade/q":{$ok,"maxconcurrent":1}}|lemonade/q
{"openrouter/foo":{$ok}}|openrouter/foo
{"OpenRouter/foo":{$ok}}|OpenRouter/foo
{"lemonade/a":{$ok},"lemonade/b":{"baseUrl":"http://other:1/v1","contextWindow":8192}}|lemonade/b
[]|localModels
EOF
  finish_rows 36
}

# #842 lane profiles: the settings half of the feature is a name plus optional
# notes text; `dispatch` reads them, so they are validated on the merged tree.
@test "laneProfiles merges per entry across layers and is validated" {
  user_settings '{"laneProfiles":{"claude":{"name":"sec"},"pi/lemonade/*":{"name":"local","notes":"- targeted only"}}}'
  locked_settings '{"laneProfiles":{"claude":{"name":"sec","notes":"- locked text"}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.laneProfiles == {
    "claude":{"name":"sec","notes":"- locked text"},
    "pi/lemonade/*":{"name":"local","notes":"- targeted only"}}' <<<"$output"

  unset DISPATCH_LOCKED_SETTINGS
  user_settings '{"laneProfiles":{"pi/lemonade/Qwen3.8-Flash":{"name":"local"}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
}

@test "malformed laneProfiles is refused, naming the path" {
  begin_rows
  local json fragment
  while IFS='|' read -r json fragment; do
    [ -n "$json" ] || continue
    keep_row "$fragment" laneprofiles_refuse_row "{\"laneProfiles\":$json}" "$fragment"
  done <<EOF
{"claude":{"notes":"- x"}}|claude.name
{"claude":{"name":"bad name"}}|claude.name
{"claude":{"name":"../evil"}}|claude.name
{"claude":{"name":"a/b"}}|claude.name
{"claude":{"name":""}}|claude.name
{"claude":{"name":"x","notes":7}}|claude.notes
{"claude":{"name":"x","tier":"deep"}}|claude.tier
{"claude":"sec"}|claude
[]|laneProfiles
EOF
  finish_rows 9
}

laneprofiles_refuse_row() { # json fragment
  user_settings "$1"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *laneProfiles* ]]
  [[ "$stderr" == *"$2"* ]]
}

# #1104 remote workers: `hosts` maps a name to an ssh destination and the
# remote clone of each repo; `crew remote-run` reads it, so it is validated on
# the merged tree.
@test "hosts from the user layer reaches the merged output" {
  user_settings '{"hosts":{"mini":{"ssh":"me@mini","repos":{"acme/widget":"/r/widget"}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.hosts == {"mini":{"ssh":"me@mini","repos":{"acme/widget":"/r/widget"}}}' <<<"$output"
}

@test "hosts chrome from the user layer reaches the merged output" {
  user_settings '{"hosts":{"mini":{"ssh":"me@mini","chrome":true,"repos":{"acme/widget":"/r/widget"}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.hosts == {"mini":{"ssh":"me@mini","chrome":true,"repos":{"acme/widget":"/r/widget"}}}' <<<"$output"
}

@test "hosts peer from the user layer reaches the merged output" {
  user_settings '{"hosts":{"mini":{"ssh":"me@mini","peer":true,"repos":{"acme/widget":"/r/widget"}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.hosts == {"mini":{"ssh":"me@mini","peer":true,"repos":{"acme/widget":"/r/widget"}}}' <<<"$output"
}

@test "malformed hosts is refused, naming the path" {
  begin_rows
  local json fragment
  while IFS='|' read -r json fragment; do
    [ -n "$json" ] || continue
    keep_row "$fragment" hosts_refuse_row "$json" "$fragment"
  done <<EOF
{"hosts":{"-x":{"ssh":"me@mini"}}}|-x
{"hosts":{"mini":{"ssh":"-oProxyCommand=x"}}}|mini.ssh
{"hosts":{"mini":{"ssh":"me@mini host"}}}|mini.ssh
{"hosts":{"mini":{"repos":{"acme/widget":"/r/widget"}}}}|mini.ssh
{"hosts":{"mini":{"ssh":"me@mini","repos":{"widget":"/r/widget"}}}}|mini.repos.widget
{"hosts":{"mini":{"ssh":"me@mini","repos":{"acme/widget":"r/widget"}}}}|mini.repos.acme/widget
{"hosts":{"mini":{"ssh":"me@mini","repos":{"acme/widget":"/r/wid\nget"}}}}|mini.repos.acme/widget
{"hosts":{"mini":{"ssh":"me@mini","repos":{"acme/widget":"/r/wid\tget"}}}}|mini.repos.acme/widget
{"hosts":{"mini":{"ssh":"me@mini","repos":{"acme/widget":"/r/wid\u007fget"}}}}|mini.repos.acme/widget
{"hosts":{"mini":{"ssh":"me@mini","repos":["/r"]}}}|mini.repos
{"hosts":{"mini":{"ssh":"me@mini","port":22}}}|mini.port
{"hosts":{"mini":{"ssh":"me@mini","chrome":"yes"}}}|mini.chrome
{"hosts":{"mini":{"ssh":"me@mini","chrome":1}}}|mini.chrome
{"hosts":{"mini":{"ssh":"me@mini","peer":"yes"}}}|mini.peer
{"hosts":{"mini":{"ssh":"me@mini","peer":1}}}|mini.peer
{"hosts":{"mini":"me@mini"}}|mini
{"hosts":[]}|hosts
EOF
  finish_rows 17
}

# The module's strMatching "/[^[:cntrl:]]*" is byte-based: a C1 code point is
# two non-control UTF-8 bytes there, so the resolver admits it too.
@test "a C1 control in a hosts repo path is accepted, as the module type admits it" {
  user_settings '{"hosts":{"mini":{"ssh":"me@mini","repos":{"acme/widget":"/r/wid\u0085get"}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.hosts.mini.repos["acme/widget"] == "/r/wid\u0085get"' <<<"$output"
}

hosts_refuse_row() { # json fragment
  user_settings "$1"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *hosts* ]]
  [[ "$stderr" == *"$2"* ]]
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

@test "shipped defaults carry a classes row with class and admits for every modelMap tier, silently" {
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  jq -e '.classes as $c | .modelMap | to_entries | length > 0 and all(.[]; .key as $e | .value | keys | all(.[]; . as $t | $c[$e].tiers[$t] as $r | ($r.class | IN("cheap", "standard", "premium")) and ($r.admits | type == "array" and index($r.class) != null)))' <<<"$output"
}

@test "a pattern overlapping another class warns and still resolves" {
  user_settings '{"classes":{"claude":{"cheap":["haiku","claude-*"]}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  [[ "$stderr" == *'dispatch-config: classes.claude: pattern "claude-*" (cheap) overlaps "claude-sonnet-*" (standard); an id matching both takes standard'* ]]
  [[ "$stderr" == *'pattern "claude-*" (cheap) overlaps "claude-opus-*" (premium)'* ]]
}

@test "a classes row missing admits resolves to [class] with a warning" {
  user_settings '{"modelMap":{"local":{"deep":{"default":"x","models":["x"]}}},"classes":{"local":{"standard":["x"],"tiers":{"deep":{"class":"standard"}}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]
  jq -e '.classes.local.tiers.deep.admits == ["standard"]' <<<"$output"
  [[ "$stderr" == *"classes.local.tiers.deep.admits is missing"* ]]
}

@test "a modelMap row with no classes row is refused" {
  user_settings '{"modelMap":{"local":{"deep":{"default":"x","models":["x"]}}}}'
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"classes.local.tiers.deep must be an object with class and admits"* ]]
}

@test "malformed classes are refused, naming the path" {
  begin_rows
  local row json fragment
  while IFS='|' read -r row json fragment; do
    [ -n "$row" ] || continue
    keep_row "$row" shape_refuse_row "$json" "$fragment"
  done <<'ROWS'
bad-class|{"classes":{"claude":{"tiers":{"deep":{"class":"huge"}}}}}|classes.claude.tiers.deep.class must be one of cheap, standard or premium
admits-lacks-class|{"classes":{"claude":{"tiers":{"deep":{"admits":["standard"]}}}}}|classes.claude.tiers.deep.admits must be a non-empty array
admits-unknown|{"classes":{"claude":{"tiers":{"deep":{"admits":["premium","free"]}}}}}|classes.claude.tiers.deep.admits must be a non-empty array
pattern-type|{"classes":{"claude":{"cheap":[7]}}}|classes.claude.cheap.0 must be a glob string or {glob, requires
requires-value|{"classes":{"claude":{"cheap":[{"glob":"x","requires":"other"}]}}}|classes.claude.cheap.0 must be a glob string or {glob, requires
pi-glob-provider|{"classes":{"pi":{"standard":["*/foo"]}}}|classes.pi.standard.0 must be a pi pattern with a literal provider
ROWS
  finish_rows 6
}

@test "a classes.pi provider is reserved against localModels" {
  local lm='"localModels":{"anthropic/foo":{"baseUrl":"http://h:1/v1","contextWindow":8192}}'
  user_settings "{$lm}"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 0 ]

  user_settings "{$lm,\"classes\":{\"pi\":{\"premium\":[\"anthropic/claude-opus-*\"]}}}"
  run --separate-stderr "$CONFIG"
  [ "$status" -eq 1 ]
  [[ "$stderr" == *"anthropic is reserved"* ]]
}
