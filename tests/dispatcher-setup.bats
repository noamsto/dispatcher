bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  SETUP="$BATS_TEST_DIRNAME/../adapters/core/dispatcher-setup.sh"
  export HOME="$BATS_TEST_TMPDIR/home"
  mkdir -p "$HOME"
  USER_FILE="$XDG_CONFIG_HOME/dispatcher/settings.json"
  # A closed PATH: the host's own engine CLIs must not count as installed.
  TOOLS="$BATS_TEST_TMPDIR/tools"
  STUB_DIR="$BATS_TEST_TMPDIR/stubs"
  mkdir -p "$TOOLS" "$STUB_DIR"
  local t
  for t in bash env jq awk gawk diff nproc sort mktemp timeout grep cat mv cp mkdir rm dirname chmod ln; do
    ln -s "$(command -v "$t")" "$TOOLS/$t" 2>/dev/null || true
  done
  export PATH="$TOOLS"
  stub_bin claude
  stub_bin pi
  unset OPENROUTER_API_KEY DISPATCH_OPENROUTER_KEY_FILE DISPATCH_FLEET_BUDGET DISPATCH_OPENROUTER_MONTHLY_USD DISPATCH_LOCKED_SETTINGS
  export PI_CODING_AGENT_DIR="$HOME/.pi/agent"
}

run_setup() { bash "$SETUP" "$@" </dev/null; }

write_user() {
  mkdir -p "$(dirname "$USER_FILE")"
  printf '%s\n' "$1" >"$USER_FILE"
}

@test "--yes writes the detected engines and fleetBudget, keeps an unknown key, and reads back clean" {
  write_user '{"custom": {"x": 1}}'
  run run_setup --yes
  [ "$status" -eq 0 ]
  [ "$(jq -c .engines "$USER_FILE")" = '["claude","pi"]' ]
  jq -e '.fleetBudget | type == "number" and . >= 2 and . <= 16' "$USER_FILE"
  [ "$(jq -c .custom "$USER_FILE")" = '{"x":1}' ]
  run --separate-stderr bash "$DISPATCH_CONFIG_BIN"
  [ "$status" -eq 0 ]
  [ -z "$stderr" ]
  [ "$(jq -c .engines <<<"$output")" = '["claude","pi"]' ]
}

@test "--yes with nothing to change reports up to date and exits 0" {
  run run_setup --yes
  [ "$status" -eq 0 ]
  run run_setup --yes
  [ "$status" -eq 0 ]
  [[ "$output" == *"already up to date"* ]]
}

@test "setup never writes grantRoots or openrouter.keyFile, even when env offers them" {
  echo sk-or-v1-abc >"$BATS_TEST_TMPDIR/key"
  DISPATCH_GRANT_ROOTS=/srv/a DISPATCH_OPENROUTER_KEY_FILE="$BATS_TEST_TMPDIR/key" run run_setup --yes
  [ "$status" -eq 0 ]
  jq -e 'has("grantRoots") | not' "$USER_FILE"
  jq -e '(.openrouter // {}) | has("keyFile") | not' "$USER_FILE"
}

@test "setup drops grantRoots and openrouter.keyFile already in the user file, keeping the rest" {
  write_user '{"grantRoots": ["/a"], "openrouter": {"keyFile": "/k", "monthlyUsd": 5}}'
  run run_setup --yes
  [ "$status" -eq 0 ]
  jq -e 'has("grantRoots") | not' "$USER_FILE"
  jq -e '.openrouter | has("keyFile") | not' "$USER_FILE"
  [ "$(jq .openrouter.monthlyUsd "$USER_FILE")" = 5 ]
}

@test "a symlinked user file is not written and the JSON snippet is printed" {
  mkdir -p "$XDG_CONFIG_HOME/dispatcher" "$BATS_TEST_TMPDIR/managed"
  echo '{"custom": 1}' >"$BATS_TEST_TMPDIR/managed/settings.json"
  ln -s "$BATS_TEST_TMPDIR/managed/settings.json" "$USER_FILE"
  run run_setup --yes
  [ "$status" -eq 0 ]
  [ "$(cat "$BATS_TEST_TMPDIR/managed/settings.json")" = '{"custom": 1}' ]
  [ -L "$USER_FILE" ]
  [[ "$output" == *"managed"* ]]
  awk '/^\{/ { p = 1 } p' <<<"$output" | jq -e '.engines == ["claude","pi"]'
}

@test "a dangling symlinked user file is not created" {
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  ln -s "$BATS_TEST_TMPDIR/nowhere/settings.json" "$USER_FILE"
  run run_setup --yes
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/nowhere" ]
}

@test "non-interactive stdin without --yes writes nothing and exits non-zero" {
  run run_setup
  [ "$status" -eq 2 ]
  [ ! -e "$USER_FILE" ]
  [[ "$output" == *"pass --yes"* ]]
}

@test "--check exits 1 with a gap line when pi has a key but no monthly target" {
  write_user '{"engines": ["claude","pi"], "fleetBudget": 4}'
  OPENROUTER_API_KEY=sk-or-v1-SECRETVALUE run run_setup --check
  [ "$status" -eq 1 ]
  [[ "$output" == *"pi has no monthly target, so the pace gate cannot protect it"* ]]
  [[ "$output" != *SECRETVALUE* ]]
}

@test "--check exits 0 on a complete config" {
  write_user '{"engines": ["claude","pi"], "fleetBudget": 4}'
  run run_setup --check
  [ "$status" -eq 0 ]
}

@test "--check --json is valid JSON with the gaps and keys" {
  write_user '{"engines": ["claude","pi"], "fleetBudget": 4}'
  OPENROUTER_API_KEY=sk-or-v1-x run run_setup --check --json
  [ "$status" -eq 1 ]
  jq -e '(.gaps | length) == 1 and .managed == false and .openrouterKey.found == true
    and (.keys | map(.key) | index("fleetBudget"))' <<<"$output"
}

@test "an openrouter.keyFile from the locked layer counts as a key source" {
  echo sk-or-v1-abc >"$BATS_TEST_TMPDIR/key"
  jq -n --arg f "$BATS_TEST_TMPDIR/key" '{openrouter: {keyFile: $f}}' >"$BATS_TEST_TMPDIR/locked.json"
  write_user '{"engines": ["claude","pi"], "fleetBudget": 4}'
  DISPATCH_LOCKED_SETTINGS="$BATS_TEST_TMPDIR/locked.json" run run_setup --check --json
  [ "$status" -eq 1 ]
  jq -e '.openrouterKey.found == true and (.openrouterKey.source | test("openrouter.keyFile \\(locked\\)"))' <<<"$output"
}

@test "--check lists ssh config hosts but --yes never adds one" {
  stub_bin ssh
  mkdir -p "$HOME/.ssh"
  printf 'Host box *.wild\n  HostName 10.0.0.1\n' >"$HOME/.ssh/config"
  run run_setup --check --json
  jq -e '.keys[] | select(.key == "hosts") | .detected == ["box"]' <<<"$output"
  run run_setup --yes
  [ "$status" -eq 0 ]
  jq -e 'has("hosts") | not' "$USER_FILE"
}
