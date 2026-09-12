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
  [[ "$output" == *"--agent must be claude, codex, cursor, or pi"* ]]
}

@test "rejects ultra for pi (codex-only)" {
  run run_dispatch deep openrouter/deepseek/deepseek-v4-pro --agent pi --effort ultra --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"ultra is codex-only"* ]]
}

@test "pi is not work-profile gated" {
  # pi+deepseek is a personal engine (OpenRouter), unlike the work-only
  # codex/cursor accounts. Assert the gate does NOT fire: the run proceeds past
  # it and fails later for an unrelated reason (no worktree in the test repo).
  DISPATCH_PROFILE=personal run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 "title"
  [[ "$output" != *"work-profile only"* ]]
}

@test "the pi worker launch streams via the TUI with the protocol appended" {
  # pi -p is buffered and would read as a wedge to stall-watch; the worker must
  # use the streaming TUI, append the protocol as a real system prompt, and
  # ignore project-local resources in an unattended run.
  run grep -F -- '--append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.md' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- '--no-approve' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- '--thinking $effort' "$DISPATCH"
  [ "$status" -eq 0 ]
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

@test "DISPATCHER_PROTOCOL_DIR overrides the baked default" {
  run grep -c 'DISPATCHER_PROTOCOL_DIR:-@protocolDir@' "$DISPATCH"
  [ "$output" = "1" ]
}
