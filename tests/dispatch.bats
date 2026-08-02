setup() {
  load helpers
  DISPATCH="$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
  run_dispatch() { bash -euo pipefail "$DISPATCH" "$@"; }
  setup_repo
  # A work shell exports DISPATCH_PROFILE and any dispatcher session exports
  # CREW_ID; bats inherits both, so without this the suite passes on a work box
  # and fails on a personal one. HOME points at the throwaway repo so the codex
  # cache fixture and the --mcp config paths cannot reach the developer's own.
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE
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

# The slug set this account actually has, as of writing.
write_codex_cache() {
  mkdir -p "$HOME/.codex"
  cat >"$HOME/.codex/models_cache.json" <<'EOF'
{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.6-terra"},{"slug":"codex-auto-review"},{"slug":"gpt-5.6-luna"},{"slug":"gpt-5.5"},{"slug":"gpt-5.4"},{"slug":"gpt-5.4-mini"}]}
EOF
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
  [[ "$launch" == *"--model 'kimi-k3-high'"* ]]
  [[ "$launch" == *'Process authority:'* ]]
}

@test "rejects a codex slug on --agent claude" {
  run run_dispatch standard kimi-k3-high --agent claude --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent claude"* ]]
}

@test "rejects an effort-suffixed cursor id on --agent claude" {
  run run_dispatch standard claude-opus-4-8-high --agent claude --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"effort-suffixed cursor id"* ]]
}

@test "the model gate outranks the effort-ultra gate" {
  # Ordering pin: the mistake is the engine/model pairing, not the effort, so
  # moving the gate below the ultra check masks it.
  run run_dispatch deep gpt-5.6-sol --agent claude --effort ultra --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent claude"* ]]
  [[ "$output" != *"effort ultra is codex-only"* ]]
}

@test "accepts claude aliases and full claude-* ids" {
  # Distinct titles are load-bearing: the title becomes the branch, and a reused
  # one makes the second `git worktree add -b` collide.
  stub_launch_bins
  run run_dispatch deep claude-fable-5 --agent claude --effort high --crew-id c1 42 "fable row"
  [ "$status" -eq 0 ]
  run run_dispatch trivial haiku --agent claude --effort low --crew-id c1 42 "haiku row"
  [ "$status" -eq 0 ]
}

@test "DISPATCH_SKIP_MODEL_CHECK bypasses the gate for that exact model" {
  stub_launch_bins
  DISPATCH_SKIP_MODEL_CHECK=kimi-k3-high run run_dispatch standard kimi-k3-high --agent claude --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" == *"model check skipped"* ]]
}

@test "rejects a bare gpt generation on --agent codex" {
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"there is no bare gpt-5.6"* ]]
}

@test "the model gate fires before any worktree is scaffolded" {
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  # Mirrors the profile-gate test: the gate rejects before any stub runs, so
  # $STUB_LOG may not exist at all. Non-vacuous for *this* gate because the row
  # reaches it (profile is work, crew id supplied) — move the gate below
  # dispatch.sh's `wt switch -c` and `switch` lands in the log.
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "rejects a claude alias on --agent codex" {
  DISPATCH_PROFILE=work run run_dispatch standard opus --agent codex --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent codex"* ]]
}

@test "rejects a cursor-shaped id on --agent codex" {
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol-high --agent codex --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent codex"* ]]
}

@test "accepts codex variant slugs and the legacy bare generations" {
  # Distinct titles: see the claude-alias test.
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 42 "terra row"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.5 --agent codex --effort medium --crew-id c1 42 "legacy row"
  [ "$status" -eq 0 ]
}

@test "DISPATCH_SKIP_MODEL_CHECK is exact-match, not a boolean" {
  DISPATCH_PROFILE=work DISPATCH_SKIP_MODEL_CHECK=1 run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"there is no bare gpt-5.6"* ]]
}

@test "an exported DISPATCH_SKIP_MODEL_CHECK does not blanket-disable the gate" {
  DISPATCH_PROFILE=work DISPATCH_SKIP_MODEL_CHECK=gpt-5.6 run run_dispatch standard opus --agent codex --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent codex"* ]]
}

@test "DISPATCH_SKIP_MODEL_CHECK lets its own model through to launch" {
  stub_launch_bins
  DISPATCH_PROFILE=work DISPATCH_SKIP_MODEL_CHECK=gpt-5.6 run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" == *"model check skipped"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "the codex cache rejects a well-shaped slug this account lacks" {
  write_codex_cache
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.7-sol --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"models_cache.json"* ]]
  [[ "$output" == *"gpt-5.6-sol"* ]]
  # The advertised list is filtered to what the grammar accepts, so it must not
  # suggest the internal review model.
  [[ "$output" != *"codex-auto-review"* ]]
}

@test "the codex cache admits a slug it holds" {
  stub_launch_bins
  write_codex_cache
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
}

@test "the grammar floor holds with no codex cache" {
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6 --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"there is no bare gpt-5.6"* ]]
}

@test "an absent codex cache is a skip, not a hard fail" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "an unparseable codex cache never blocks codex dispatch" {
  stub_launch_bins
  mkdir -p "$HOME/.codex"
  printf 'not json' >"$HOME/.codex/models_cache.json"
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
}

@test "rejects a claude alias on --agent cursor" {
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent cursor"* ]]
}

@test "rejects a cursor claude-*/gpt-* id with no effort rung" {
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"effort suffix"* ]]
}

@test "a bracket block exempts the effort rule only by naming effort" {
  DISPATCH_PROFILE=work run run_dispatch standard 'gpt-5.6[detail=x]' --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"effort suffix"* ]]
}

@test "cursor accepts a no-effort-variant id and single-quotes it at launch" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard composer-2.5 --agent cursor --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$launch" == *"--model 'composer-2.5'"* ]]
}

@test "cursor accepts the parameterised bracket form" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep 'claude-opus-4-8[context=1m,effort=high,fast=false]' --agent cursor --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  # Single-quoted, so the glob-active brackets never reach the worker's shell.
  [[ "$launch" == *"--model 'claude-opus-4-8[context=1m,effort=high,fast=false]'"* ]]
}
