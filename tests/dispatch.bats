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

# Stubs that carry a `--pr N` attach all the way to send-keys: gh resolves the
# PR head, wt attaches a worktree to that existing branch (no -c), crew/tmux as
# in stub_launch_bins. $1 is the PR's head branch.
stub_pr_bins() { # <head-branch>
  git commit --allow-empty -qm init
  git branch "$1"
  export PR_HEAD="$1"

  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
pr\ view\ *)
  printf '{"headRefName":"%s","isCrossRepository":false}\n' "$PR_HEAD"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"

  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
# switch <branch> -y --config-set ...
branch="$2"
mkdir -p "$TEST_REPO/.worktrees"
git worktree add -q "$TEST_REPO/.worktrees/$branch" "$branch"
exit 0
EOF
  chmod +x "$STUB_DIR/wt"

  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
identity) printf '%s\n' '{"name":"coral-fox","tmux":"colour1"}' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"

  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
new-window) printf '%s\n' '%1 %2' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

# wait_for_log <pattern> — poll $STUB_LOG for a line written by a backgrounded
# stub (the nohup'd stall-watch). Fails the test after ~2s.
wait_for_log() {
  local i
  for i in $(seq 1 40); do
    grep -q "$1" "$STUB_LOG" && return 0
    sleep 0.05
  done
  echo "wait_for_log: never saw '$1' in $STUB_LOG" >&2
  cat "$STUB_LOG" >&2
  return 1
}

# Mirrors the real cache: `jq -r '.models[].slug' ~/.codex/models_cache.json`.
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

@test "task headers preserve the authoritative launch tuple for every engine" {
  stub_launch_bins

  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --effort medium --crew-id c1 42 "metadata claude"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-metadata-claude/WORKER_TASK.md"
  grep -Fx 'engine: claude' "$task"
  grep -Fx 'model: sonnet' "$task"
  grep -Fx 'effort: medium' "$task"

  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "metadata codex"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-metadata-codex/WORKER_TASK.md"
  grep -Fx 'engine: codex' "$task"
  grep -Fx 'model: gpt-5.6-terra' "$task"
  grep -Fx 'effort: high' "$task"

  DISPATCH_PROFILE=work run run_dispatch standard composer-2.5 --agent cursor --effort low --crew-id c1 42 "metadata cursor"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-metadata-cursor/WORKER_TASK.md"
  grep -Fx 'engine: cursor' "$task"
  grep -Fx 'model: composer-2.5' "$task"
  grep -Fx 'effort: low' "$task"
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

# Parses fine and the container shape is right, so a probe that stops at
# `.models|arrays` calls it usable — then `.slug` on a string is a hard jq error
# that set -e turns into a dead dispatch, rejecting a valid slug.
@test "a codex cache with drifted element shape never blocks codex dispatch" {
  stub_launch_bins
  mkdir -p "$HOME/.codex"
  printf '{"models":["gpt-5.6-sol","gpt-5.6-terra"]}' >"$HOME/.codex/models_cache.json"
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" != *"Cannot index string"* ]]
}

# The advertised list is echoed to a terminal, so a slug carrying an escape
# sequence would be interpreted rather than displayed. jq writes the control
# bytes here, keeping this file free of literal ones.
@test "control characters in a cached slug never reach the terminal" {
  mkdir -p "$HOME/.codex"
  jq -n '{models:[{slug:"gpt-5.6-sol"},{slug:("gpt-9.9-" + "" + "]0;title" + "" + "x")}]}' \
    >"$HOME/.codex/models_cache.json"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.7-sol --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"gpt-5.6-sol"* ]]
  [[ "$output" == *"gpt-9.9-]0;titlex"* ]]
  printf '%s' "$output" | grep -qP '[\x00-\x1f]' && return 1
  return 0
}

# The advertised-slug list is built after the membership check fails, so a
# non-string slug there crashes the rejection path itself: the caller gets jq's
# raw error and exit 5 instead of the actionable message.
@test "a non-string slug does not derail the rejection message" {
  mkdir -p "$HOME/.codex"
  printf '{"models":[{"slug":123},{"slug":"gpt-5.6-sol"}]}' >"$HOME/.codex/models_cache.json"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.7-sol --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not in this account's codex model list"* ]]
  [[ "$output" == *"gpt-5.6-sol"* ]]
  [[ "$output" != *"startswith() requires string inputs"* ]]
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

# Asserts only that the gate stayed silent — a full launch per model would need
# a distinct branch per row and buys nothing the acceptance tests do not cover.
assert_gate_silent() { # <engine> <model>
  DISPATCH_PROFILE=work run run_dispatch standard "$2" --agent "$1" --effort medium --crew-id c1 42 "map row $2"
  if [[ "$output" == *"Model gate"* ]]; then
    printf 'gate rejected %s/%s: %s\n' "$1" "$2" "$output" >&2
    return 1
  fi
}

@test "every model the docs name passes its engine's arm" {
  # Hand-copied from dispatch-orchestration.md: the model map, the cursor
  # alternatives prose, the codex legacy generations, and the orchestrator
  # table. Copied, so it makes drift loud rather than impossible.
  for m in opus sonnet haiku claude-fable-5; do
    assert_gate_silent claude "$m"
  done
  for m in gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-5.5 gpt-5.4 gpt-5.4-mini; do
    assert_gate_silent codex "$m"
  done
  for m in kimi-k3-high cursor-grok-4.5-high cursor-grok-4.5-medium-fast \
    cursor-grok-4.5-low-fast composer-2.5 composer-2.5-fast \
    claude-opus-4-8-high gpt-5.6-sol-high; do
    assert_gate_silent cursor "$m"
  done
}


@test "rejects --pr combined with a GitHub issue token" {
  run run_dispatch standard sonnet --effort medium --pr 12 34 "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--pr cannot combine"* ]]
}

@test "rejects --pr combined with a Linear id" {
  run run_dispatch standard sonnet --effort medium --pr 12 ENG-1 "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--pr cannot combine"* ]]
}

@test "rejects --pr without a positive integer" {
  run run_dispatch standard sonnet --effort medium --pr "" "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--pr needs"* ]]
}

@test "--pr path calls wt switch without -c and stamps pr: N" {
  # Real branch in the test repo so show-ref succeeds and the porcelain locate works.
  stub_pr_bins eng-7691-foo

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Review PR 99"
  [ "$status" -eq 0 ]
  grep -q 'pr view 99' "$STUB_LOG"
  ! grep -E '(^| )(-c|--create)( |$)' "$STUB_LOG"
  grep -q 'switch eng-7691-foo' "$STUB_LOG"

  wt_path="$TEST_REPO/.worktrees/eng-7691-foo"
  grep -qx 'pr: 99' "$wt_path/WORKER_TASK.md"
  ! grep -q 'Closes #' "$wt_path/WORKER_TASK.md"
}

@test "--review without --pr aborts before scaffolding" {
  run run_dispatch standard sonnet --effort medium --review --crew-id c1 "review something"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--review requires --pr N"* ]]
  # Same idiom as the profile gate: no stub ran, so $STUB_LOG may not exist.
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "--review rejects a tracker token in place of a PR" {
  run run_dispatch standard sonnet --effort medium --review ENG-1 "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--review requires --pr N"* ]]

  run run_dispatch standard sonnet --effort medium --review --pr 12 34 "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--pr cannot combine"* ]]
}

@test "--review aborts when the protocol dir carries no review contract" {
  # Non-vacuous: $DISPATCHER_PROTOCOL_DIR points at a checkout predating
  # REVIEW_TASK.md, and a review worker with no contract runs the implement
  # pipeline against someone else's PR head.
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/empty-protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  run run_dispatch standard sonnet --effort medium --pr 99 --review --crew-id c1 "review"
  [ "$status" -eq 1 ]
  [[ "$output" == *"no review contract"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "--review stamps kind: review, appends the contract, and drops the push mandate" {
  stub_pr_bins pr-head-review
  export DISPATCHER_PROTOCOL_DIR="$BATS_TEST_DIRNAME/../adapters/core/protocols"

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --review --crew-id c1 "Review PR 99"
  [ "$status" -eq 0 ]

  task="$TEST_REPO/.worktrees/pr-head-review/WORKER_TASK.md"
  grep -qx 'kind: review' "$task"
  # The contract body, not merely a pointer to it.
  grep -q 'The worktree is the PR head' "$task"
  grep -q 'Never `REQUEST_CHANGES`' "$task"

  launch="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$launch" == *"do not edit, commit, push, or open a PR"* ]]
  [[ "$launch" != *"Push when pre-push passes"* ]]
}

@test "an implement dispatch stamps kind: implement and keeps the push mandate" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "implement thing"
  [ "$status" -eq 0 ]
  grep -qx 'kind: implement' "$TEST_REPO/.dispatch-wt/feat-42-implement-thing/WORKER_TASK.md"
  launch="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$launch" == *"Push when pre-push passes; open a PR"* ]]
}

@test "session: stamps worker_id, exports CREW_WORKER_ID and prints the id" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"worker_id: worker:feat/42-do-a-thing#s7-7"* ]]
  wt_path="$TEST_REPO/.dispatch-wt/feat-42-do-a-thing"
  grep -qx 'worker_id: worker:feat/42-do-a-thing#s7-7' "$wt_path/WORKER_TASK.md"
  grep -q 'CREW_WORKER_ID=worker:feat/42-do-a-thing#s7-7 claude ' "$STUB_LOG"
}

@test "session: the dispatch event carries the session" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="dispatch") | .session' "$log"
  [ "$output" = "s7-7" ]
}

@test "session: stall-watch is handed the worker id, not the branch" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  wait_for_log 'stall-watch worker:feat/42-do-a-thing#s7-7 --pane'
}

@test "session: a minted id is epoch-pid shaped" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [[ "$output" =~ worker_id:\ worker:feat/42-do-a-thing#s[0-9]+-[0-9]+ ]]
}

# stub_crew_gate <occupants-json> <sessions-json> — a crew stub that feeds the
# gate fixed answers while still logging every call.
stub_crew_gate() {
  printf '%s' "$1" >"$STUB_DIR/occ.json"
  printf '%s' "$2" >"$STUB_DIR/sess.json"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "${1:-}" in
identity) printf '%s\n' '{"name":"sage","color":"green","tmux":"colour28"}' ;;
occupants) cat "$STUB_DIR/occ.json" ;;
sessions) cat "$STUB_DIR/sess.json" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

# An existing worktree for the branch the --pr path resolves to, so the gate has
# something to find.
setup_occupied_branch() {
  stub_launch_bins
  git -C "$TEST_REPO" branch eng-7691-foo
  mkdir -p "$TEST_REPO/.worktrees"
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.worktrees/eng-7691-foo" eng-7691-foo
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
pr\ view\ *) printf '%s\n' '{"headRefName":"eng-7691-foo","isCrossRepository":false}' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  # stub_launch_bins' wt only handles `switch -c` and exits 1 otherwise; the --pr
  # path switches by NAME, and the worktree already exists here, so switching is a
  # no-op success. Without this override every reclaim test dies at the switch.
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
}

@test "gate: refuses when a live engine occupies the target worktree" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":"working","ts":1,"age_s":412,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 1 ]
  [[ "$output" == *"worker:eng-7691-foo#s1-1"* ]]
  [[ "$output" == *"working"* ]]
  [[ "$output" == *"@23"* ]]
  [[ "$output" == *"crew reply worker:eng-7691-foo"* ]]
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q 'send-keys' "$STUB_LOG"
  ! grep -q 'kill-window' "$STUB_LOG"
  ! grep -q '^switch' "$STUB_LOG"
}

@test "gate: refuses a booting session that has posted no status yet" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":null,"ts":1,"age_s":3,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 1 ]
  ! grep -q 'new-window' "$STUB_LOG"
}

@test "gate: reclaims a finished session and proceeds" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":"done","ts":1,"age_s":900,"terminal":true}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  [[ "$output" == *"reclaimed @23"* ]]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="reclaim") | "\(.branch) \(.state) \(.windows[0])"' "$log"
  [ "$output" = "eng-7691-foo done @23" ]
}

@test "gate: reclaims a worker window whose engine already exited" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":null,"engine":false}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":"working","ts":1,"age_s":900,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  grep -q 'kill-window -t @23' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

@test "gate: an unoccupied existing worktree dispatches normally" {
  setup_occupied_branch
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  ! grep -q 'kill-window' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

@test "gate: a fresh branch never consults occupants" {
  stub_launch_bins
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  ! grep -q '^occupants' "$STUB_LOG"
}
