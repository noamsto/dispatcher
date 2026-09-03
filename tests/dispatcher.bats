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
  unset TMUX CREW_ID
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
  # Match each call exactly. A bare `grep -c register` also matches
  # `deregister`, so a count of 2 could mean "registered twice, never
  # deregistered" — it would pass while the bus leaked stale entries.
  run grep -cx 'register [0-9][0-9]*' "$STUB_LOG"
  [ "$output" = "1" ]
  run grep -cx 'deregister' "$STUB_LOG"
  [ "$output" = "1" ]
}

@test "registers with a live pid whose liveness tracks the session" {
  # fish used $fish_pid; the port uses $$. Both must name a process that
  # outlives the agent launch, since crew's stale-reclaim keys on it.
  CREW_ID=c1 run_launcher
  pid="$(grep -x 'register [0-9][0-9]*' "$STUB_LOG" | awk '{print $2}')"
  [ -n "$pid" ]
  [ "$pid" -gt 0 ]
}

@test "survives a tmux without lazytmux's @reflow_bin" {
  # A missing reflow must not abort the launcher under `set -e`.
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 CREW_ID=c1 run run_launcher
  [ "$status" -eq 0 ]
}

@test "reflows through the path lazytmux stamps in @reflow_bin" {
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
[ "$1" = show-option ] && echo "$STUB_DIR/reflow"
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cat >"$STUB_DIR/reflow" <<'EOF'
#!/usr/bin/env bash
printf 'reflow %s\n' "$*" >>"$STUB_LOG"
EOF
  chmod +x "$STUB_DIR/reflow"

  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 CREW_ID=c1 run_launcher
  run grep -c '^reflow ' "$STUB_LOG"
  [ "$output" = "1" ]
}
