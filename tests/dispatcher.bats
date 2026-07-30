setup() {
  load helpers
  LAUNCHER="$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh"
  run_launcher() { bash -euo pipefail "$LAUNCHER" "$@"; }
  setup_repo
  stub_bin tmux
  stub_bin crew
  stub_bin claude
  stub_bin codex
  stub_bin cursor-agent
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
  unset TMUX
}

teardown() {
  teardown_repo
}

@test "rejects an unknown agent" {
  run run_launcher --agent bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent must be claude, codex, or cursor"* ]]
}

@test "gates codex behind the work profile" {
  DISPATCH_PROFILE=personal run run_launcher --agent codex
  [ "$status" -eq 1 ]
  [[ "$output" == *"work-profile only"* ]]
}

@test "rejects an unknown effort" {
  run run_launcher --effort bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"--effort must be"* ]]
}

@test "a trailing flag fails loudly, not silently" {
  # Without an explicit value guard, `shift 2` fails and `set -e` kills the
  # script with no message — a regression against the fish original, which
  # fell through to its validation error. Each flag must say what it needs.
  for flag in --agent --model --effort; do
    run run_launcher "$flag"
    [ "$status" -eq 1 ]
    [[ "$output" == *"$flag needs a value"* ]]
  done
}

@test "prints the crew id it minted" {
  CREW_ID=1720800000-99 run run_launcher
  [ "$status" -eq 0 ]
  [[ "$output" == *"crew id: 1720800000-99"* ]]
}

@test "passes the protocol to claude as an appended system prompt" {
  CREW_ID=c1 run_launcher
  run grep -F -- '--append-system-prompt-file /opt/protocols/DISPATCHER_PROTOCOL.md' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "the bare form treats all non-flag args as one task" {
  CREW_ID=c1 run_launcher fix the flaky test
  run grep -F -- '--name dispatcher: fix the flaky test' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "injects the protocol as a first prompt for codex" {
  DISPATCH_PROFILE=work CREW_ID=c1 run_launcher --agent codex
  run grep -F -- 'Read /opt/protocols/DISPATCHER_PROTOCOL.md' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "warns that effort is ignored for cursor" {
  DISPATCH_PROFILE=work CREW_ID=c1 run run_launcher --agent cursor --effort high
  [[ "$output" == *"--effort is ignored for cursor"* ]]
}

@test "skips tmux window stamping when not inside tmux" {
  CREW_ID=c1 run_launcher
  run grep -c 'set-window-option' "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "registers and deregisters around the agent launch" {
  CREW_ID=c1 run_launcher
  run grep -c 'register' "$STUB_LOG"
  [ "$output" -ge 2 ]
}
