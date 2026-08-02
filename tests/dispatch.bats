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

# Overwrite the generic stubs with ones that can finish a launch far enough
# to reach tmux send-keys (worktree + identity + new-window).
stub_launch_bins() {
  git -C "$TEST_REPO" commit --allow-empty -q -m init

  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = switch ]; then
  br=""
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
    -c)
      br="$2"
      shift 2
      ;;
    *) shift ;;
    esac
  done
  [ -n "$br" ] || exit 1
  dest="$TEST_REPO/.dispatch-wt/${br//\//-}"
  mkdir -p "$(dirname "$dest")"
  git -C "$TEST_REPO" worktree add -b "$br" "$dest" HEAD >/dev/null
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"

  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = new-window ]; then
  printf '%s %s\n' '%1' '%1'
fi
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"

  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
identity) printf '%s\n' '{"name":"iris","color":"blue","tmux":"colour33"}' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
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

@test "DISPATCHER_PROTOCOL_DIR overrides the baked default" {
  run grep -c 'DISPATCHER_PROTOCOL_DIR:-@protocolDir@' "$DISPATCH"
  [ "$output" = "1" ]
}

@test "codex launch pins agents.* guardrails and process authority" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$launch" == *'agents.enabled=true'* ]]
  [[ "$launch" == *'agents.max_concurrent_threads_per_session=3'* ]]
  # session high → subagent medium (one rung down)
  [[ "$launch" == *'agents.default_subagent_reasoning_effort=medium'* ]]
  [[ "$launch" == *'Process authority:'* ]]
  [[ "$launch" != *'default_subagent_reasoning_effort=ultra'* ]]
}

@test "codex ultra maps subagent effort to max without nesting ultra" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort ultra --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$launch" == *'model_reasoning_effort=ultra'* ]]
  [[ "$launch" == *'agents.default_subagent_reasoning_effort=max'* ]]
  [[ "$launch" == *'do not add a second harness'* ]]
}

@test "cursor launch includes process authority" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep kimi-k3-high --agent cursor --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$launch" == *'cursor-agent'* ]]
  [[ "$launch" == *'--model kimi-k3-high'* ]]
  [[ "$launch" == *'Process authority:'* ]]
}
