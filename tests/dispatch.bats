setup() {
  load helpers
  DISPATCH="$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
  run_dispatch() { bash -euo pipefail "$DISPATCH" "$@"; }
  setup_repo
  stub_bin tmux
  stub_bin crew
  stub_bin gh
  stub_bin wt
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
}

teardown() {
  teardown_repo
}

@test "rejects an unknown tier" {
  run run_dispatch bogus sonnet --effort medium "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"usage: dispatch"* ]]
}

@test "requires a model" {
  run run_dispatch standard
  [ "$status" -eq 1 ]
  [[ "$output" == *"usage: dispatch"* ]]
}

@test "rejects an unknown agent" {
  run run_dispatch standard sonnet --agent bogus --effort medium "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent must be claude, codex, or cursor"* ]]
}

@test "rejects an unknown effort" {
  run run_dispatch standard sonnet --effort bogus "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--effort must be low, medium, high, xhigh, max, or ultra"* ]]
}

@test "gates codex behind the work profile" {
  DISPATCH_PROFILE=personal run run_dispatch standard gpt-5.6-sol --agent codex --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"codex is work-profile only"* ]]
}

@test "gates cursor behind the work profile" {
  DISPATCH_PROFILE=personal run run_dispatch standard kimi-k3-high --agent cursor --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"work-profile only"* ]]
}

@test "the profile gate fires before any worktree is scaffolded" {
  DISPATCH_PROFILE=personal run run_dispatch standard gpt-5.6-sol --agent codex --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  # The gate rejects before ANY stubbed binary runs, so $STUB_LOG is never
  # created — `grep -c` on a missing file errors rather than printing 0.
  # Assert the real property instead: no `wt switch` scaffolded a worktree.
  # Non-vacuous: move the gate below worktree creation and `wt switch --create`
  # lands in the log, failing this.
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "no launch string references nix-config" {
  run grep -c 'nix-config' "$DISPATCH"
  [ "$output" = "0" ]
}

# Write a budget cache with one claude 7d window at the given utilization.
budget_json() {
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson pct "$1" --argjson epoch "$2" \
    '{fetched_epoch: $epoch, engines: {claude: {source: "t", windows: {"7d": {used_pct: $pct, resets_at: null}}}, codex: null, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
}

@test "refuses to dispatch on an engine at >=95% with a fresh budget cache" {
  export XDG_DATA_HOME="$(mktemp -d)"
  budget_json 97 "$(date +%s)"
  run run_dispatch standard sonnet --effort medium --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"claude quota exhausted"* ]]
  # The gate rejects before scaffolding, same property as the profile gate.
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "budget gate passes below the threshold" {
  export XDG_DATA_HOME="$(mktemp -d)"
  budget_json 94 "$(date +%s)"
  run run_dispatch standard sonnet --effort medium --crew-id c1 "title"
  [[ "$output" != *"quota exhausted"* ]]
}

@test "budget gate fails open on a stale cache" {
  export XDG_DATA_HOME="$(mktemp -d)"
  budget_json 100 "$(($(date +%s) - 10000))"
  run run_dispatch standard sonnet --effort medium --crew-id c1 "title"
  [[ "$output" != *"quota exhausted"* ]]
}

@test "budget gate fails open when the cache is silent on the engine" {
  export XDG_DATA_HOME="$(mktemp -d)"
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: null, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  run run_dispatch standard sonnet --effort medium --crew-id c1 "title"
  [[ "$output" != *"quota exhausted"* ]]
}

@test "--ignore-budget bypasses the gate" {
  export XDG_DATA_HOME="$(mktemp -d)"
  budget_json 100 "$(date +%s)"
  run run_dispatch standard sonnet --effort medium --crew-id c1 --ignore-budget "title"
  [[ "$output" != *"quota exhausted"* ]]
}

@test "DISPATCHER_PROTOCOL_DIR overrides the baked default" {
  run grep -c 'DISPATCHER_PROTOCOL_DIR:-@protocolDir@' "$DISPATCH"
  [ "$output" = "1" ]
}
