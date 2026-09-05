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
  unset DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE DISPATCH_DRAFT_PR
  stub_bin tmux
  stub_bin crew
  stub_bin gh
  stub_bin wt
  stub_bin direnv
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
}

teardown() {
  teardown_repo
}

# Overwrite the generic stubs with ones that can finish a launch far enough
# to reach tmux send-keys (worktree + identity + new-window).
stub_launch_bins() {
  git -C "$TEST_REPO" commit --allow-empty -q -m init

  # A real, fetchable origin: the create path now runs `gh repo view` +
  # `git fetch origin` before branching (#41), so both need to resolve to
  # something real rather than the generic no-op stubs from setup().
  git init -q --bare "$TEST_REPO/origin.git"
  git -C "$TEST_REPO" remote add origin "$TEST_REPO/origin.git"
  git -C "$TEST_REPO" push -q origin main

  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
repo\ view\ *) printf '%s\n' "${STUB_DEFAULT_BRANCH:-main}" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"

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
# in stub_launch_bins. $1 is the PR's head branch. headRefOid is the branch's
# current tip, so the worktree-verification step sees a match by default —
# tests that want a mismatch override $PR_HEAD_OID after calling this.
stub_pr_bins() { # <head-branch> [base-branch]
  git commit --allow-empty -qm init
  git branch "$1"
  export PR_HEAD="$1"
  export PR_HEAD_OID
  PR_HEAD_OID="$(git rev-parse "$1")"
  export PR_BASE="${2:-extract}"

  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
pr\ view\ *)
  printf '{"headRefName":"%s","headRefOid":"%s","baseRefName":"%s","isCrossRepository":false}\n' "$PR_HEAD" "$PR_HEAD_OID" "$PR_BASE"
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

# An existing --pr worktree that is behind the PR's real head: a real `origin`
# remote (a bare repo) carries a commit the local worktree never fetched —
# exactly the staleness #19 describes, where `wt switch` attached to an
# existing worktree without fetching or resetting it. $STALE_OLD_OID is what
# the worktree has checked out; $STALE_NEW_OID is what `gh pr view` reports as
# headRefOid. wt/crew/tmux are the no-op attach stubs from setup_occupied_branch
# (the worktree already exists); gh and git are real.
setup_stale_pr_worktree() { # <branch>
  stub_launch_bins
  git -C "$TEST_REPO" branch "$1"
  mkdir -p "$TEST_REPO/.worktrees"
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.worktrees/$1" "$1"
  export STALE_OLD_OID
  STALE_OLD_OID="$(git -C "$TEST_REPO" rev-parse "$1")"

  # stub_launch_bins already wired up a real `origin` bare repo; reuse it
  # rather than colliding with a second `remote add origin`.
  git -C "$TEST_REPO" push -q origin "$1:refs/heads/$1"

  scratch="$(mktemp -d)"
  git clone -q "$TEST_REPO/origin.git" "$scratch"
  git -C "$scratch" -c user.email=test@example.com -c user.name=test checkout -q "$1"
  git -C "$scratch" -c user.email=test@example.com -c user.name=test commit --allow-empty -qm "pr head advances"
  export STALE_NEW_OID
  STALE_NEW_OID="$(git -C "$scratch" rev-parse HEAD)"
  git -C "$scratch" push -q origin "$1"
  rm -rf "$scratch"

  export STALE_HEAD="$1"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
pr\ view\ *)
  printf '{"headRefName":"%s","headRefOid":"%s","baseRefName":"extract","isCrossRepository":false}\n' "$STALE_HEAD" "$STALE_NEW_OID"
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"

  # stub_launch_bins' wt only handles `switch -c`; the --pr path switches by
  # NAME onto the already-existing worktree, so switching is a no-op success.
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/wt"

  # The worktree already exists, so the reuse-or-refuse gate (#17) runs before
  # the head check; tell it the worktree is unoccupied.
  stub_crew_gate '[]' '[]'
}

# A real `origin` whose default branch has advanced past the local `main` —
# the staleness #41 describes: nothing here fetches or fast-forwards the
# local ref before a new worker branches from it. $STALE_LOCAL_OID is what
# local `main` is stuck at; $STALE_REMOTE_OID is origin's real tip. gh is
# stubbed to report the default branch name (the one gh call this fixture
# needs); git is real, including the fetch dispatch.sh itself runs. The wt
# stub honors -b (unlike stub_launch_bins' generic one, which always bases on
# HEAD) so the test can see which commit the worktree actually landed on.
setup_stale_default_branch() {
  stub_launch_bins
  export STALE_LOCAL_OID
  STALE_LOCAL_OID="$(git -C "$TEST_REPO" rev-parse main)"

  scratch="$(mktemp -d)"
  git clone -q "$TEST_REPO/origin.git" "$scratch"
  git -C "$scratch" -c user.email=test@example.com -c user.name=test checkout -q main
  git -C "$scratch" -c user.email=test@example.com -c user.name=test commit --allow-empty -qm "origin advances"
  export STALE_REMOTE_OID
  STALE_REMOTE_OID="$(git -C "$scratch" rev-parse HEAD)"
  git -C "$scratch" push -q origin main
  rm -rf "$scratch"

  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = switch ]; then
  br="" base=""
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
    -c) br="$2"; shift 2 ;;
    -b) base="$2"; shift 2 ;;
    *) shift ;;
    esac
  done
  [ -n "$br" ] || exit 1
  dest="$TEST_REPO/.dispatch-wt/${br//\//-}"
  mkdir -p "$(dirname "$dest")"
  git -C "$TEST_REPO" worktree add -b "$br" "$dest" "${base:-HEAD}" >/dev/null
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
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

# Write a budget cache with one claude 7d window at the given utilization.
budget_json() {
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson pct "$1" --argjson epoch "$2" \
    '{fetched_epoch: $epoch, engines: {claude: {source: "t", windows: {"7d": {used_pct: $pct, resets_at: null}}}, codex: null, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
}

# Write a budget cache with one codex 7d window at the given utilization —
# the gate-1/gate-2 ordering test and the gate-2 budget-rung tests both need
# this. Parallel to budget_json() above, not a change to its signature: that
# helper has 5 existing claude-only call sites.
codex_budget_json() { # <pct> <epoch>
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson pct "$1" --argjson epoch "$2" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: {source: "t", windows: {"7d": {used_pct: $pct, resets_at: null}}}, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
}

@test "refuses to dispatch on an engine at >=95% with a fresh budget cache" {
  budget_json 97 "$(date +%s)"
  run run_dispatch standard sonnet --effort medium --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"claude quota exhausted"* ]]
  # The gate rejects before scaffolding, same property as the profile gate.
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "budget gate passes below the threshold" {
  stub_launch_bins
  budget_json 94 "$(date +%s)"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" != *"quota exhausted"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "budget gate fails open on a stale cache" {
  stub_launch_bins
  budget_json 100 "$(($(date +%s) - 10000))"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" != *"quota exhausted"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "budget gate fails open when the cache is silent on the engine" {
  stub_launch_bins
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: null, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" != *"quota exhausted"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "--ignore-budget bypasses the gate" {
  stub_launch_bins
  budget_json 100 "$(date +%s)"
  run run_dispatch standard sonnet --effort medium --crew-id c1 --ignore-budget 42 "title"
  [ "$status" -eq 0 ]
  [[ "$output" != *"quota exhausted"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "worker window starts at the invoking client size" {
  stub_launch_bins

  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' '200 50 off' ;;
show-option) printf '%s\n' latest ;;
new-window) printf '%s %s\n' '%1' '%1' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"

  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "sized worker"
  [ "$status" -eq 0 ]
  grep -q 'new-window -d -c .* -n feat-42-sized-worker .* -P' "$STUB_LOG"
  grep -q 'resize-window -t %1 -x 200 -y 50' "$STUB_LOG"
  grep -q 'set-option -t %1 window-size latest' "$STUB_LOG"
  resize_line="$(grep -n 'resize-window -t %1 -x 200 -y 50' "$STUB_LOG" | cut -d: -f1)"
  launch_line="$(grep -n 'send-keys' "$STUB_LOG" | cut -d: -f1)"
  [ "$resize_line" -lt "$launch_line" ]
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
  [[ "$launch" == *'review authority only'* ]]
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
  [[ "$launch" == *'review authority only'* ]]
}

# #46: a codex/cursor worker's shell expands `$CREW_ID` itself (the launch
# prompt tells it to report via `dispatcher:$CREW_ID`), so the id must be a
# real env var in the worker's tmux window, not merely known to dispatch.sh.
@test "codex launch exports CREW_ID into the worker window" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  win="$(grep 'new-window' "$STUB_LOG")"
  [[ "$win" == *'CREW_ID=c1'* ]]
}

@test "cursor launch exports CREW_ID into the worker window" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep kimi-k3-high --agent cursor --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  win="$(grep 'new-window' "$STUB_LOG")"
  [[ "$win" == *'CREW_ID=c1'* ]]
}

@test "claude launch also exports CREW_ID into the worker window" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  win="$(grep 'new-window' "$STUB_LOG")"
  [[ "$win" == *'CREW_ID=c1'* ]]
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

@test "draft defaults to false" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "draft default"
  [ "$status" -eq 0 ]
  grep -Fx 'draft: false' "$TEST_REPO/.dispatch-wt/feat-42-draft-default/WORKER_TASK.md"
}

@test "--draft stamps a draft worker task" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --draft --crew-id c1 42 "draft flag"
  [ "$status" -eq 0 ]
  grep -Fx 'draft: true' "$TEST_REPO/.dispatch-wt/feat-42-draft-flag/WORKER_TASK.md"
}

@test "DISPATCH_DRAFT_PR enables draft PRs" {
  stub_launch_bins
  DISPATCH_PROFILE=personal DISPATCH_DRAFT_PR=1 run run_dispatch standard sonnet --effort medium --crew-id c1 42 "draft env"
  [ "$status" -eq 0 ]
  grep -Fx 'draft: true' "$TEST_REPO/.dispatch-wt/feat-42-draft-env/WORKER_TASK.md"
}

@test "--no-draft overrides DISPATCH_DRAFT_PR" {
  stub_launch_bins
  DISPATCH_PROFILE=personal DISPATCH_DRAFT_PR=1 run run_dispatch standard sonnet --effort medium --no-draft --crew-id c1 42 "ready override"
  [ "$status" -eq 0 ]
  grep -Fx 'draft: false' "$TEST_REPO/.dispatch-wt/feat-42-ready-override/WORKER_TASK.md"
}

@test "--draft with --review aborts before scaffolding" {
  run run_dispatch standard sonnet --effort medium --draft --review --pr 12 --crew-id c1 "draft review"
  [ "$status" -ne 0 ]
  [[ "$output" == *"--draft cannot be combined with --review"* ]]
}

@test "rejects a codex slug on --agent claude" {
  run run_dispatch standard kimi-k3-high --agent claude --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match --agent claude"* ]]
}

@test "rejects an effort-suffixed cursor id on --agent claude" {
  run run_dispatch standard claude-opus-5-high --agent claude --effort medium --crew-id c1 42 "title"
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
  run run_dispatch deep claude-fable-5-1 --agent claude --effort high --crew-id c1 42 "fable row"
  [ "$status" -eq 0 ]
  run run_dispatch trivial haiku --agent claude --effort low --crew-id c1 42 "haiku row"
  [ "$status" -eq 0 ]
}

@test "DISPATCH_SKIP_MODEL_CHECK bypasses the gate for that exact model" {
  stub_launch_bins
  DISPATCH_SKIP_MODEL_CHECK=kimi-k3-high run run_dispatch standard kimi-k3-high --agent claude --effort medium --ignore-map --crew-id c1 42 "title"
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
  DISPATCH_PROFILE=work DISPATCH_SKIP_MODEL_CHECK=gpt-5.6 run run_dispatch deep gpt-5.6 --agent codex --effort high --ignore-map --crew-id c1 42 "title"
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
  DISPATCH_PROFILE=work run run_dispatch deep 'claude-opus-5[context=1m,effort=high,fast=false]' --agent cursor --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  launch="$(grep 'send-keys' "$STUB_LOG")"
  # Single-quoted, so the glob-active brackets never reach the worker's shell.
  [[ "$launch" == *"--model 'claude-opus-5[context=1m,effort=high,fast=false]'"* ]]
}

# Asserts only that the gate stayed silent — a full launch per model would need
# a distinct branch per row and buys nothing the acceptance tests do not cover.
assert_gate_silent() { # <engine> <model>
  DISPATCH_PROFILE=work run run_dispatch standard "$2" --agent "$1" --effort medium --ignore-map --crew-id c1 42 "map row $2"
  if [[ "$output" == *"Model gate"* ]]; then
    printf 'gate rejected %s/%s: %s\n' "$1" "$2" "$output" >&2
    return 1
  fi
  # Non-vacuous: no stub_launch_bins here, so every row already dies
  # downstream at dispatch.sh's `gh repo view` resolution regardless of
  # gate 1/gate 2 — reaching that specific failure proves the run cleared
  # BOTH the dispatchability gate and the new tier gate.
  if [[ "$output" != *"could not resolve the default branch"* ]]; then
    printf 'gate stopped %s/%s before reaching gh repo view: %s\n' "$1" "$2" "$output" >&2
    return 1
  fi
}

@test "every model the docs name passes its engine's arm" {
  # Hand-copied from dispatch-orchestration.md: the model map, the cursor
  # alternatives prose, the codex legacy generations, and the orchestrator
  # table. Copied, so it makes drift loud rather than impossible.
  for m in opus sonnet haiku claude-fable-5-1; do
    assert_gate_silent claude "$m"
  done
  for m in gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-5.5 gpt-5.4 gpt-5.4-mini; do
    assert_gate_silent codex "$m"
  done
  for m in kimi-k3-high cursor-grok-4.6-high cursor-grok-4.6-medium-fast \
    cursor-grok-4.6-low-fast composer-2.5 composer-2.5-fast \
    claude-opus-5-high gpt-5.6-sol-high; do
    assert_gate_silent cursor "$m"
  done
}

@test "tier gate accepts every claude table cell" {
  # Distinct titles: see the claude-alias test at :549.
  stub_launch_bins
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "tier claude deep opus"
  [ "$status" -eq 0 ]
  run run_dispatch deep claude-opus-5-1 --agent claude --effort high --crew-id c1 42 "tier claude deep opus pinned"
  [ "$status" -eq 0 ]
  run run_dispatch deep sonnet --agent claude --effort high --crew-id c1 42 "tier claude deep sonnet"
  [ "$status" -eq 0 ]
  run run_dispatch deep claude-sonnet-4-5 --agent claude --effort high --crew-id c1 42 "tier claude deep sonnet pinned"
  [ "$status" -eq 0 ]
  run run_dispatch deep fable --agent claude --effort high --crew-id c1 42 "tier claude deep fable"
  [ "$status" -eq 0 ]
  run run_dispatch deep claude-fable-5-1 --agent claude --effort high --crew-id c1 42 "tier claude deep fable pinned"
  [ "$status" -eq 0 ]
  run run_dispatch standard sonnet --agent claude --effort medium --crew-id c1 42 "tier claude standard sonnet"
  [ "$status" -eq 0 ]
  run run_dispatch standard claude-sonnet-4-5 --agent claude --effort medium --crew-id c1 42 "tier claude standard sonnet pinned"
  [ "$status" -eq 0 ]
  run run_dispatch trivial sonnet --agent claude --effort low --crew-id c1 42 "tier claude trivial sonnet"
  [ "$status" -eq 0 ]
  run run_dispatch trivial haiku --agent claude --effort low --crew-id c1 42 "tier claude trivial haiku"
  [ "$status" -eq 0 ]
}

@test "tier gate accepts every codex table cell" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "tier codex deep sol"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "tier codex deep terra"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.5 --agent codex --effort high --crew-id c1 42 "tier codex deep legacy 5.5"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.4 --agent codex --effort high --crew-id c1 42 "tier codex deep legacy 5.4"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.4-mini --agent codex --effort high --crew-id c1 42 "tier codex deep legacy 5.4 mini"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 42 "tier codex standard terra"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-luna --agent codex --effort medium --crew-id c1 42 "tier codex standard luna"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.5 --agent codex --effort medium --crew-id c1 42 "tier codex standard legacy 5.5"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.4 --agent codex --effort medium --crew-id c1 42 "tier codex standard legacy 5.4"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.4-mini --agent codex --effort medium --crew-id c1 42 "tier codex standard legacy 5.4 mini"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial gpt-5.6-luna --agent codex --effort low --crew-id c1 42 "tier codex trivial luna"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial gpt-5.5 --agent codex --effort low --crew-id c1 42 "tier codex trivial legacy 5.5"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial gpt-5.4 --agent codex --effort low --crew-id c1 42 "tier codex trivial legacy 5.4"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial gpt-5.4-mini --agent codex --effort low --crew-id c1 42 "tier codex trivial legacy 5.4 mini"
  [ "$status" -eq 0 ]
}

@test "tier gate accepts every cursor table cell" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep kimi-k3-high --agent cursor --effort high --crew-id c1 42 "tier cursor deep kimi"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep cursor-grok-4.6-medium-fast --agent cursor --effort high --crew-id c1 42 "tier cursor deep grok medium"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep cursor-grok-4.6-high --agent cursor --effort high --crew-id c1 42 "tier cursor deep grok high"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep composer-2.5 --agent cursor --effort high --crew-id c1 42 "tier cursor deep composer"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep 'claude-opus-5[context=1m,effort=high,fast=false]' --agent cursor --effort high --crew-id c1 42 "tier cursor deep bracket opus"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.6-medium-fast --agent cursor --effort medium --crew-id c1 42 "tier cursor standard grok medium"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.6-low-fast --agent cursor --effort medium --crew-id c1 42 "tier cursor standard grok low"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard composer-2.5 --agent cursor --effort medium --crew-id c1 42 "tier cursor standard composer"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial cursor-grok-4.6-low-fast --agent cursor --effort low --crew-id c1 42 "tier cursor trivial grok low"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial composer-2.5 --agent cursor --effort low --crew-id c1 42 "tier cursor trivial composer"
  [ "$status" -eq 0 ]
}

@test "tier gate rejects a bracketed composer id on every tier" {
  # composer-2.5[-fast] has no effort variants (dispatch-orchestration.md
  # "composer-2.5 ... no effort variants") — a bracket block on it is never
  # legitimate, so it must not slip through as if it were the plain,
  # no-effort-variant composer alternative.
  DISPATCH_PROFILE=work run run_dispatch deep 'composer-2.5[effort=max]' --agent cursor --effort high --crew-id c1 42 "tier cursor deep composer bracket rejected"
  [ "$status" -eq 1 ]
  [[ "$output" == *"deep"* ]]
  [[ "$output" == *"--ignore-map"* ]]

  DISPATCH_PROFILE=work run run_dispatch standard 'composer-2.5-fast[foo=bar]' --agent cursor --effort medium --crew-id c1 42 "tier cursor standard composer fast bracket rejected"
  [ "$status" -eq 1 ]
  [[ "$output" == *"standard"* ]]
  [[ "$output" == *"--ignore-map"* ]]
}

@test "budget rung gate also matches a bracketed premium cursor id" {
  # Pins the case pattern itself, not live cursor budget data (cursor's
  # cache is always null today — see the dispatch.sh comment above this arm).
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: null, cursor: {source: "t", windows: {"7d": {used_pct: 80, resets_at: null}}}}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  DISPATCH_PROFILE=work run run_dispatch deep 'cursor-grok-4.6-high[effort=high]' --agent cursor --effort high --crew-id c1 42 "rung bracket cursor"
  [ "$status" -eq 1 ]
  [[ "$output" == *"cursor-grok-4.6-medium-fast"* ]]
}

@test "claude standard rejects opus" {
  run run_dispatch standard opus --agent claude --effort medium --crew-id c1 42 "tier claude standard opus rejected"
  [ "$status" -eq 1 ]
  [[ "$output" == *"standard"* ]]
  [[ "$output" == *"opus"* ]]
  [[ "$output" == *"sonnet"* ]]
  [[ "$output" == *"--ignore-map"* ]]
}

@test "cursor confines effort-suffixed cross-vendor ids to deep" {
  # claude-*/gpt-* effort-suffixed ids are cursor arguments only on deep —
  # standard/trivial reject them (dispatch-orchestration.md "Tier map").
  DISPATCH_PROFILE=work run run_dispatch standard claude-opus-5-high --agent cursor --effort medium --crew-id c1 42 "tier cursor standard leak opus"
  [ "$status" -eq 1 ]
  [[ "$output" == *"standard"* ]]
  [[ "$output" == *"--ignore-map"* ]]

  DISPATCH_PROFILE=work run run_dispatch trivial gpt-5.6-sol-high --agent cursor --effort low --crew-id c1 42 "tier cursor trivial leak sol"
  [ "$status" -eq 1 ]
  [[ "$output" == *"trivial"* ]]
  [[ "$output" == *"--ignore-map"* ]]

  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep claude-opus-5-high --agent cursor --effort high --crew-id c1 42 "tier cursor deep suffix opus"
  [ "$status" -eq 0 ]
}

@test "--ignore-map bypasses the tier gate silently" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol --agent codex --effort medium --ignore-map --crew-id c1 42 "tier ignore map bypass"
  [ "$status" -eq 0 ]
  [[ "$output" != *"'s row for --agent"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "the tier gate outranks the budget rung gate" {
  # Ordering pin: a model that is both off-row and premium must be rejected
  # for being off-row, not for its budget rung — mirrors "the model gate
  # outranks the effort-ultra gate" shape at :540.
  codex_budget_json 80 "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol --agent codex --effort medium --crew-id c1 42 "tier vs budget ordering"
  [ "$status" -eq 1 ]
  [[ "$output" == *"standard"* ]]
  [[ "$output" == *"--ignore-map"* ]]
  [[ "$output" != *"the premium rung"* ]]
}

@test "an off-map --review --pr dispatch is rejected by the tier gate" {
  stub_pr_bins pr-head-review-offmap
  export DISPATCHER_PROTOCOL_DIR="$BATS_TEST_DIRNAME/../adapters/core/protocols"
  DISPATCH_PROFILE=work run run_dispatch standard opus --agent claude --effort medium --pr 99 --review --crew-id c1 "review off map"
  [ "$status" -eq 1 ]
  [[ "$output" == *"standard"* ]]
  [[ "$output" == *"opus"* ]]
  [[ "$output" == *"--ignore-map"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "tier map conformance: dispatch.sh matches the documented rule" {
  # Copied from the Model map/Burn classes table, so it makes drift loud
  # rather than impossible — same tolerance as :747's precedent test.
  # dispatch.sh's side matches through the human-readable tier_expected
  # strings gate 1 builds, not the bash regexes (which carry escaped `\.`
  # and would not literal-match e.g. gpt-5.6-sol) — a future edit that
  # removes those strings without keeping some literal occurrence of each id
  # would silently gut this tripwire.
  doc="$BATS_TEST_DIRNAME/../adapters/core/protocols/dispatch-orchestration.md"
  # Model map through the Model gate section only — stops before the new
  # Tier map subsection, whose own downgrade-target table repeats most of
  # these same tokens and would let this test pass even if the ORIGINAL
  # Model map/Burn classes text were deleted.
  doc_slice="$(sed -n '/^## Model map/,/^### Tier map/p' "$doc")"
  for token in opus sonnet haiku fable \
    gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-5.5 gpt-5.4 gpt-5.4-mini \
    kimi-k3-high cursor-grok-4.6-high cursor-grok-4.6-medium-fast cursor-grok-4.6-low-fast \
    composer-2.5 claude-fable-5-1; do
    grep -qF "$token" <<<"$doc_slice" || {
      printf 'token %s missing from the Model map/Burn classes doc slice\n' "$token" >&2
      return 1
    }
    grep -qF "$token" "$DISPATCH" || {
      printf 'token %s missing from dispatch.sh\n' "$token" >&2
      return 1
    }
  done
}

@test "budget rung gate refuses codex sol at 70% 7d" {
  codex_budget_json 70 "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "rung refuse 70"
  [ "$status" -eq 1 ]
  [[ "$output" == *"gpt-5.6-terra"* ]]
  [[ "$output" != *"quota exhausted"* ]]
}

@test "budget rung gate refuses codex sol at 84% 7d" {
  codex_budget_json 84 "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "rung refuse 84"
  [ "$status" -eq 1 ]
  [[ "$output" == *"gpt-5.6-terra"* ]]
  [[ "$output" != *"quota exhausted"* ]]
}

@test "the exhaustion gate outranks the budget rung gate at 95%" {
  codex_budget_json 95 "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "rung vs exhaustion 95"
  [ "$status" -eq 1 ]
  [[ "$output" == *"quota exhausted"* ]]
  [[ "$output" != *"the premium rung"* ]]
}

@test "a 5h spike with 7d low does not trigger the budget rung gate" {
  stub_launch_bins
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: {source: "t", windows: {"5h": {used_pct: 90, resets_at: null}, "7d": {used_pct: 30, resets_at: null}}}, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "5h spike 7d low"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "a drifted window key with no 7d does not trigger the budget rung gate" {
  stub_launch_bins
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: {source: "t", windows: {"other": {used_pct: 80, resets_at: null}}}, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "drifted window key"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
  [[ "$output" != *"quota exhausted"* ]]
  [[ "$output" != *"the premium rung"* ]]
}

@test "--ignore-budget bypasses the budget rung gate" {
  stub_launch_bins
  codex_budget_json 90 "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --ignore-budget --crew-id c1 42 "rung ignore budget"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "a non-premium codex model is not refused by the budget rung gate" {
  stub_launch_bins
  codex_budget_json 90 "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 42 "non premium rung"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "budget rung gate refuses claude opus at 75% 7d" {
  budget_json 75 "$(date +%s)"
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "claude rung refuse"
  [ "$status" -eq 1 ]
  [[ "$output" == *"sonnet"* ]]
  [[ "$output" != *"quota exhausted"* ]]
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
  run ! grep -E '^switch [^ ]+ (-c|--create)( |$)' "$STUB_LOG"
  grep -q 'switch eng-7691-foo' "$STUB_LOG"

  wt_path="$TEST_REPO/.worktrees/eng-7691-foo"
  grep -qx 'pr: 99' "$wt_path/WORKER_TASK.md"
  run ! grep -q 'Closes #' "$wt_path/WORKER_TASK.md"
}

@test "--pr stamps base: from baseRefName" {
  stub_pr_bins eng-7691-foo stacked-base
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Review PR 99"
  [ "$status" -eq 0 ]
  grep -qx 'base: stacked-base' "$TEST_REPO/.worktrees/eng-7691-foo/WORKER_TASK.md"
}

@test "a non-pr dispatch never stamps base:" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "implement thing"
  [ "$status" -eq 0 ]
  ! grep -q '^base:' "$TEST_REPO/.dispatch-wt/feat-42-implement-thing/WORKER_TASK.md"
}

# --pr HEAD verification (#19): `wt switch` attaches to an existing worktree
# without fetching or resetting it, so dispatch itself must confirm the
# worktree actually matches the PR's headRefOid before a worker ever launches
# against it.

@test "--pr worktree already at the PR head launches unchanged" {
  setup_occupied_branch
  stub_crew_gate '[]' '[]'
  wt_path="$TEST_REPO/.worktrees/eng-7691-foo"
  before="$(git -C "$wt_path" rev-parse HEAD)"

  # No `origin` remote exists in this fixture — if dispatch mistakenly
  # attempted a fetch on the matching-head fast path, it would fail here.
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  [ "$(git -C "$wt_path" rev-parse HEAD)" = "$before" ]
  grep -q 'new-window' "$STUB_LOG"
  grep -q 'send-keys' "$STUB_LOG"
}

@test "--pr fetches and hard-resets a clean stale worktree to the PR head" {
  setup_stale_pr_worktree eng-7691-stale
  wt_path="$TEST_REPO/.worktrees/eng-7691-stale"

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  [[ "$output" == *"$STALE_OLD_OID"* ]]
  [[ "$output" == *"$STALE_NEW_OID"* ]]
  [ "$(git -C "$wt_path" rev-parse HEAD)" = "$STALE_NEW_OID" ]
  grep -q 'new-window' "$STUB_LOG"
  grep -q 'send-keys' "$STUB_LOG"
}

@test "--pr treats a leftover WORKER_TASK.md alone as clean, not dirty" {
  # WORKER_TASK.md is intentionally untracked and is never cleaned up on
  # reclaim (only `crew reap` trashes it) — a re-dispatch onto a --pr worktree
  # whose PR has since advanced must not treat its own prior task file as
  # uncommitted work and refuse to reset.
  setup_stale_pr_worktree eng-7691-leftover
  wt_path="$TEST_REPO/.worktrees/eng-7691-leftover"
  printf 'tier: standard\n' >"$wt_path/WORKER_TASK.md"

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  [ "$(git -C "$wt_path" rev-parse HEAD)" = "$STALE_NEW_OID" ]
  grep -q 'new-window' "$STUB_LOG"
}

@test "--pr refuses to reset a dirty stale worktree, and launches nothing" {
  setup_stale_pr_worktree eng-7691-dirty
  wt_path="$TEST_REPO/.worktrees/eng-7691-dirty"
  echo "local edit" >"$wt_path/dirty.txt"

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 1 ]
  [[ "$output" == *"$STALE_OLD_OID"* ]]
  [[ "$output" == *"$STALE_NEW_OID"* ]]
  [[ "$output" == *"uncommitted"* ]]
  [ "$(git -C "$wt_path" rev-parse HEAD)" = "$STALE_OLD_OID" ]
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q 'send-keys' "$STUB_LOG"
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
  grep -q 'CREW_WORKER_ID=worker:feat/42-do-a-thing#s7-7' "$STUB_LOG"
}

@test "session: the dispatch event carries the session" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="dispatch") | .session' "$log"
  [ "$output" = "s7-7" ]
}

@test "session: claims the branch on the bus before the window exists (#32)" {
  stub_launch_bins
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run_dispatch \
    standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="claim") | .from' "$log"
  [ "$output" = "worker:feat/42-do-a-thing#s7-7" ]
  # Millisecond scale, matching every other bus event — a seconds-scale `ts`
  # would never win max_by(.ts) against a real status timestamp, silently
  # defeating the fix while this assertion alone would still pass.
  run jq -s -r '(map(select(.kind=="claim")) | first | .ts) > 1000000000000' "$log"
  [ "$output" = "true" ]
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
# something to find. headRefOid matches the worktree's actual HEAD so the
# verification step sees a match (not the concern of these gate tests).
setup_occupied_branch() {
  stub_launch_bins
  # All consumers of this fixture dispatch via --pr, which never touches the
  # create-mode default-branch fetch stub_launch_bins now wires up — drop it
  # so "no origin remote" stays true for the fast-path regression check below.
  git -C "$TEST_REPO" remote remove origin
  git -C "$TEST_REPO" branch eng-7691-foo
  mkdir -p "$TEST_REPO/.worktrees"
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.worktrees/eng-7691-foo" eng-7691-foo
  export OCCUPIED_HEAD_OID
  OCCUPIED_HEAD_OID="$(git -C "$TEST_REPO" rev-parse eng-7691-foo)"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
pr\ view\ *) printf '{"headRefName":"eng-7691-foo","headRefOid":"%s","baseRefName":"extract","isCrossRepository":false}\n' "$OCCUPIED_HEAD_OID" ;;
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

# A bare `exited` is the SessionEnd backstop, not the worker's own word, and per
# #69 it can post under the bare worker:$branch id for a SUBAGENT while the real
# #session row is still `working` — so `last.terminal` reads true on a branch
# that is actually live. Defence-in-depth: a live engine pane still refuses the
# dispatch here, same as reap keeps such a worker rather than releasing it.
@test "gate: refuses a terminal exited session with a live engine pane (#69)" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo","state":"exited","ts":1,"age_s":5,"terminal":true}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 1 ]
  [[ "$output" == *"exited"* ]]
  [[ "$output" == *"crew reply worker:eng-7691-foo"* ]]
  ! grep -q 'kill-window' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

# An engine-less occupant is not grounds to reclaim (#71): that reading goes false
# on a live worker whenever its wrapper is unrecognised, so killing on it takes the
# tree out from under a working agent. Only a terminal bus state licenses a kill.
@test "gate: refuses a non-terminal session even when no engine is detected" {
  setup_occupied_branch
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":null,"engine":false}]' \
    '[{"session":"s1-1","worker_id":"worker:eng-7691-foo#s1-1","state":"working","ts":1,"age_s":900,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 1 ]
  ! grep -q 'kill-window' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
  [[ "$output" == *"no engine pane detected"* ]]
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

# The gate must fire on the create-mode re-dispatch too (same issue/id again),
# not only on --pr — that is the path #17 was originally reported through.
@test "gate: refuses a live occupant on a create-mode re-dispatch" {
  stub_launch_bins
  git -C "$TEST_REPO" branch feat/42-do-a-thing
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.dispatch-wt/feat-42-do-a-thing" feat/42-do-a-thing
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:feat/42-do-a-thing#s1-1","state":"working","ts":1,"age_s":412,"terminal":false}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"worker:feat/42-do-a-thing#s1-1"* ]]
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q '^switch' "$STUB_LOG"
}

# lock_path <branch> — the per-branch dispatch lock symlink, keyed exactly as
# dispatch keys it (cksum of the branch), under the same git-common-dir.
lock_path() { # <branch>
  printf '%s/crew/dispatch-%s.lock' \
    "$(git rev-parse --path-format=absolute --git-common-dir)" \
    "$(printf '%s' "$1" | cksum | cut -d' ' -f1)"
}

@test "lock: a second dispatch on a branch already being scaffolded refuses" {
  stub_launch_bins
  stub_crew_gate '[]' '[]'
  # Pre-hold the lock as a symlink to a live pid (this shell's) so dispatch hits it.
  lock="$(lock_path feat/42-do-a-thing)"
  mkdir -p "$(dirname "$lock")"
  ln -s "$$" "$lock"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"already scaffolding feat/42-do-a-thing"* ]]
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q '^switch' "$STUB_LOG"
}

@test "lock: a stale lock refuses with a remediation hint, never auto-reclaims" {
  stub_launch_bins
  stub_crew_gate '[]' '[]'
  # PID 2^31-1 is never a live process. Auto-reclaim can't be made race-free in
  # portable shell, so a dead-owner lock refuses rather than silently retake it.
  lock="$(lock_path feat/42-do-a-thing)"
  mkdir -p "$(dirname "$lock")"
  ln -s 2147483647 "$lock"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"stale dispatch lock"* ]]
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q '^switch' "$STUB_LOG"
}

@test "lock: is released after a successful dispatch" {
  stub_launch_bins
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [ ! -L "$(lock_path feat/42-do-a-thing)" ]
}

# Create-mode base resolution (#41): `wt switch -c` with no -b bases off the
# LOCAL default branch, which nothing here fetches or fast-forwards first —
# routine staleness on a machine that dispatches more than it pulls. These
# tests pin that the new worktree lands on the fetched origin ref instead.

@test "create-mode branches from the fetched origin ref, not a stale local branch of the same name" {
  setup_stale_default_branch
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "implement thing"
  [ "$status" -eq 0 ]

  wt_path="$TEST_REPO/.dispatch-wt/feat-42-implement-thing"
  [ "$(git -C "$wt_path" rev-parse HEAD)" = "$STALE_REMOTE_OID" ]
  [ "$(git -C "$wt_path" rev-parse HEAD)" != "$STALE_LOCAL_OID" ]

  short="$(git -C "$TEST_REPO" rev-parse --short "$STALE_REMOTE_OID")"
  [[ "$output" == *"created branch feat/42-implement-thing from origin/main ($short)"* ]]
  # The oid is pinned at fetch time and passed to `-b` directly (not the
  # floating origin/main ref) so the branch actually created can never drift
  # from what the success line reports.
  grep -q "switch -c feat/42-implement-thing -b $STALE_REMOTE_OID" "$STUB_LOG"
}

@test "create-mode base resolution works the same for a Linear-tracked dispatch" {
  setup_stale_default_branch
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 ENG-1234 "implement thing"
  [ "$status" -eq 0 ]

  wt_path="$TEST_REPO/.dispatch-wt/eng-1234-implement-thing"
  [ "$(git -C "$wt_path" rev-parse HEAD)" = "$STALE_REMOTE_OID" ]
  grep -qx 'Closes ENG-1234' "$wt_path/WORKER_TASK.md"
}

@test "aborts before scaffolding when gh cannot resolve the default branch" {
  stub_launch_bins
  # Revert stub_launch_bins' gh override back to the generic no-op stub, so
  # `repo view` resolves to nothing while wt/tmux/crew stay real enough that
  # a genuine scaffold attempt would show up in $STUB_LOG.
  stub_bin gh
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "implement thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not resolve the default branch"* ]]
  ! grep -q 'switch' "$STUB_LOG"
  [ ! -d "$TEST_REPO/.dispatch-wt" ]
}

@test "--pr dispatch never calls gh repo view or fetches a default branch" {
  stub_pr_bins eng-7691-foo
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Review PR 99"
  [ "$status" -eq 0 ]
  ! grep -q 'repo view' "$STUB_LOG"
}

# stub_gh_claim <existing-issue-labels> <mint-issue-number> — a gh stub for the
# claim path: `issue view --json labels` on an existing token echoes back $1
# (empty = no labels), `issue create` on a mint mints $2. label create/issue
# edit calls just log and succeed. `repo view` still answers with the default
# branch name, same as stub_launch_bins, so callers that go on to reach the
# create-mode base-resolution path (#41) don't abort for want of it.
stub_gh_claim() {
  printf '%s' "$1" >"$STUB_DIR/gh_labels.txt"
  printf '%s' "$2" >"$STUB_DIR/gh_mint_num.txt"
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
issue\ view\ *)
  cat "$STUB_DIR/gh_labels.txt"
  ;;
issue\ create\ *)
  num="$(cat "$STUB_DIR/gh_mint_num.txt")"
  printf 'https://github.com/o/r/issues/%s\n' "$num"
  ;;
repo\ view\ *) printf '%s\n' "${STUB_DEFAULT_BRANCH:-main}" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
}

@test "claim: aborts when the target issue already carries dispatched, before any scaffolding" {
  stub_gh_claim dispatched ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"#42"* ]]
  [[ "$output" == *"already claimed"* ]]
  ! grep -q '^reap' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q 'switch' "$STUB_LOG"
}

@test "claim: adds dispatched to a free existing issue before crew reap, then proceeds" {
  stub_launch_bins
  stub_gh_claim "" ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  claim_line=$(grep -n 'issue edit 42 --add-label dispatched' "$STUB_LOG" | head -1 | cut -d: -f1)
  reap_line=$(grep -n '^reap --quiet' "$STUB_LOG" | head -1 | cut -d: -f1)
  switch_line=$(grep -n 'switch -c' "$STUB_LOG" | head -1 | cut -d: -f1)
  [ -n "$claim_line" ]
  [ -n "$reap_line" ]
  [ -n "$switch_line" ]
  [ "$claim_line" -lt "$reap_line" ]
  [ "$claim_line" -lt "$switch_line" ]
  grep -q 'new-window' "$STUB_LOG"
}

@test "claim: a failed label write fails the dispatch instead of proceeding unclaimed" {
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
issue\ edit\ *) exit 1 ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not claim issue #42"* ]]
  ! grep -q '^reap' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

@test "claim: a minted issue is stamped with dispatched at creation" {
  stub_launch_bins
  stub_gh_claim "" 77
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 "mint me"
  [ "$status" -eq 0 ]
  grep -q 'issue edit 77 --add-label dispatched' "$STUB_LOG"
  grep -qx 'Closes #77' "$TEST_REPO/.dispatch-wt/feat-77-mint-me/WORKER_TASK.md"
}

@test "claim: a failed mint claim write fails the dispatch instead of proceeding unclaimed" {
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
issue\ create\ *) printf 'https://github.com/o/r/issues/88\n' ;;
issue\ edit\ *) exit 1 ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 "mint me"
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not claim it"* ]]
  ! grep -q 'new-window' "$STUB_LOG"
}

@test "claim: a Linear-tracked dispatch never touches gh (byte-for-byte unaffected)" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 ENG-1234 "linear thing"
  [ "$status" -eq 0 ]
  grep -qx 'Closes ENG-1234' "$TEST_REPO/.dispatch-wt/eng-1234-linear-thing/WORKER_TASK.md"
  ! grep -q '^issue' "$STUB_LOG"
  ! grep -q '^label' "$STUB_LOG"
}

@test "claim: a --pr dispatch never touches gh issue/label calls" {
  stub_pr_bins eng-7691-foo
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  ! grep -q '^issue' "$STUB_LOG"
  ! grep -q '^label' "$STUB_LOG"
}

# trust/direnv (#40): a fresh worktree is unknown to Claude Code's per-project
# trust store and to direnv's allow list, so an unattended worker wedges on
# one dialog or the other before it ever reads WORKER_TASK.md.

# wt_path_for <branch> — the same `git worktree list --porcelain` lookup
# dispatch.sh itself uses for $wt_path. $TEST_REPO comes from mktemp -d, which
# on macOS returns a path through the /var -> /private/var symlink; git
# reports the resolved realpath, so string-building "$TEST_REPO/..." by hand
# would silently never match what dispatch.sh actually stamped.
wt_path_for() {
  git -C "$TEST_REPO" worktree list --porcelain |
    awk -v b="refs/heads/$1" '/^worktree /{p=$2} $0=="branch "b{print p}'
}

@test "trust: a claude dispatch stamps hasTrustDialogAccepted for the new worktree" {
  stub_launch_bins
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  wt="$(wt_path_for feat/42-title)"
  run jq -e --arg p "$wt" '.projects[$p].hasTrustDialogAccepted == true' "$HOME/.claude.json"
  [ "$status" -eq 0 ]
}

@test "trust: stamping preserves unrelated projects and top-level keys" {
  stub_launch_bins
  jq -n '{userID: "u1", projects: {"/somewhere/else": {hasTrustDialogAccepted: true, allowedTools: ["Bash"]}}}' \
    >"$TEST_REPO/.claude.json"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  run jq -e '.userID == "u1" and (.projects["/somewhere/else"].allowedTools == ["Bash"])' "$HOME/.claude.json"
  [ "$status" -eq 0 ]
}

@test "trust: a codex dispatch never touches ~/.claude.json" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  [ ! -e "$HOME/.claude.json" ]
}

@test "trust: an unparseable existing ~/.claude.json aborts the dispatch, never launches" {
  stub_launch_bins
  printf 'not json' >"$TEST_REPO/.claude.json"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not pre-trust worktree"* ]]
  ! grep -q 'send-keys' "$STUB_LOG"
}

@test "direnv: allow is called with the new worktree path" {
  stub_launch_bins
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  wt="$(wt_path_for feat/42-title)"
  grep -qx "allow $wt" "$STUB_LOG"
}

@test "direnv: allow is called for codex and cursor dispatches too" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  wt="$(wt_path_for feat/42-title)"
  grep -qx "allow $wt" "$STUB_LOG"
}

@test "direnv: allow failure aborts the dispatch, never launches" {
  stub_launch_bins
  cat >"$STUB_DIR/direnv" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 1
EOF
  chmod +x "$STUB_DIR/direnv"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"direnv allow failed"* ]]
  ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q 'send-keys' "$STUB_LOG"
}

@test "direnv: a --pr dispatch never auto-approves, warns instead, and still launches" {
  stub_pr_bins eng-7691-foo
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Review PR 99"
  [ "$status" -eq 0 ]
  [[ "$output" == *"not auto-approving direnv"* ]]
  ! grep -q '^allow ' "$STUB_LOG"
  grep -q 'send-keys' "$STUB_LOG"
}

@test "trust: a claude_json_lock held by a live process is never deleted by a losing racer" {
  stub_launch_bins
  sleep 100 &
  holder_pid=$!
  ln -s "$holder_pid" "$HOME/.claude.json.dispatch.lock"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  kill "$holder_pid" 2>/dev/null || true
  [ "$status" -eq 1 ]
  [[ "$output" == *"held by pid $holder_pid"* ]]
  [ -L "$HOME/.claude.json.dispatch.lock" ]
  [ "$(readlink "$HOME/.claude.json.dispatch.lock")" = "$holder_pid" ]
  ! grep -q 'send-keys' "$STUB_LOG"
}

# resume (#73): dispatching onto a branch that already exists continues the
# interrupted run in place rather than dying on `wt switch -c`. Every fixture
# below extends the create-mode gate pattern above — stub_launch_bins plus a
# hand-made branch/worktree — because that is the shape the bug was reported in.

# setup_resume_branch <branch> — an existing branch WITH its worktree already on
# disk. stub_launch_bins' wt stub only understands `switch -c` and exits 1 on
# anything else; a resume switches by name onto a tree that already exists, so a
# no-op wt is the correct override here.
setup_resume_branch() { # <branch>
  stub_launch_bins
  git -C "$TEST_REPO" branch "$1"
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.dispatch-wt/${1//\//-}" "$1"
  stub_crew_gate '[]' '[]'
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
}

# setup_resume_branch_no_worktree <branch> — an existing branch with NO worktree,
# the case that proves the resume gate is ref existence and not the reclaim. The
# wt stub must MATERIALISE the worktree on a bare `switch <branch>`, mirroring
# what stub_launch_bins does for `switch -c`: a no-op leaves $wt_path empty and
# dispatch dies at `could not locate worktree`, which would satisfy a status
# assertion for entirely the wrong reason.
setup_resume_branch_no_worktree() { # <branch>
  stub_launch_bins
  git -C "$TEST_REPO" branch "$1"
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = switch ] && [ "${2#-}" = "$2" ]; then
  br="$2"
  git -C "$TEST_REPO" worktree add -q "$TEST_REPO/.dispatch-wt/${br//\//-}" "$br"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
}

# The repro #73 was filed for: a killed worker leaves a terminal session on a
# branch whose worktree still holds uncommitted work. The gate reclaims the
# window, and the switch must attach to the branch rather than re-create it —
# `wt switch -c` fails outright, and any create would strand that work.
@test "resume: reclaims a terminal session, switches without -c, and keeps dirty work" {
  setup_resume_branch feat/42-do-a-thing
  wt="$TEST_REPO/.dispatch-wt/feat-42-do-a-thing"
  printf 'half-done\n' >"$wt/scratch.txt"
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:feat/42-do-a-thing#s1-1","state":"done","ts":1,"age_s":900,"terminal":true}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"reclaimed @23"* ]]
  [[ "$output" == *"resuming branch feat/42-do-a-thing"* ]]
  grep -q '^switch feat/42-do-a-thing -y' "$STUB_LOG"
  ! grep -q 'switch -c' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
  [ -f "$wt/scratch.txt" ]
}

# `wt remove` and a pruned worktree both leave the ref behind, and `switch -c`
# died on those too — so the resume gate keys on the ref, not on finding a tree.
@test "resume: an existing branch with no worktree resumes too" {
  setup_resume_branch_no_worktree feat/42-do-a-thing
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"resuming branch feat/42-do-a-thing"* ]]
  ! grep -q 'switch -c' "$STUB_LOG"
  ! grep -q '^occupants' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

# Non-vacuous form of "a resume resolves no default base": with `origin` gone,
# the `git fetch origin` the create path runs would fail the dispatch outright,
# so a green run here can only mean that whole block was skipped.
@test "resume: resolves no default branch — succeeds with origin removed" {
  setup_resume_branch feat/42-do-a-thing
  git -C "$TEST_REPO" remote remove origin
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  ! grep -q 'repo view' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

# The worker has to know it is continuing rather than starting, in both places it
# reads instructions from: the stamped header and the launch prompt.
@test "resume: stamps resume: true and carries the resume note into the launch string" {
  setup_resume_branch feat/42-do-a-thing
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  grep -Fx 'resume: true' "$TEST_REPO/.dispatch-wt/feat-42-do-a-thing/WORKER_TASK.md"
  keys="$(grep 'send-keys' "$STUB_LOG")"
  [[ "$keys" == *"You are resuming an interrupted run on this branch"* ]]
  [[ "$keys" == *"do not re-run the spec or plan phases"* ]]
  [[ "$keys" == *"open PR before you push"* ]]
}

@test "resume: a create-mode dispatch stamps neither resume: true nor the resume note" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  ! grep -q '^resume:' "$TEST_REPO/.dispatch-wt/feat-42-do-a-thing/WORKER_TASK.md"
  ! grep -q 'You are resuming an interrupted run' "$STUB_LOG"
}

# The claim gate's own label is what a re-dispatch of an interrupted run trips
# over: the issue is still labelled from the first run. The resolved branch
# existing is what separates that from a second crew forking the same issue.
@test "claim: an already-dispatched issue proceeds when the resolved branch exists" {
  setup_resume_branch feat/42-do-a-thing
  stub_gh_claim dispatched ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"already claimed, but branch feat/42-do-a-thing exists"* ]]
  grep -q 'issue edit 42 --add-label dispatched' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

@test "claim: an already-dispatched issue is still refused when the branch does not exist" {
  stub_launch_bins
  stub_gh_claim dispatched ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"already claimed"* ]]
  ! grep -q 'add-label' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

# The exemption is keyed on the EXACT resolved branch, not "a branch for this
# issue". A sibling from an earlier, differently-worded dispatch of the same
# issue (feat/42-something-else) must not satisfy it in place of the branch
# this title actually resolves to (feat/42-do-a-thing) — otherwise a later
# loosening to a `feat/<id>-*` glob would let a second crew fork the issue
# without anything here catching it (#73).
@test "claim: an already-dispatched issue is still refused when only a sibling branch exists" {
  stub_launch_bins
  git -C "$TEST_REPO" branch feat/42-something-else
  stub_gh_claim dispatched ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"already claimed"* ]]
  ! grep -q 'add-label' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

# A resume is normally issued without re-passing $DISPATCH_SPEC, and the stamp
# block truncates the file — so the run's task text (and, on a `plan: provided`
# run, its plan of record) has to be read back before the redirect opens.
@test "resume: carries the prior ## Task body forward under a fresh header" {
  setup_resume_branch feat/42-do-a-thing
  task="$TEST_REPO/.dispatch-wt/feat-42-do-a-thing/WORKER_TASK.md"
  printf 'stale_header: yes\n\n## Task\n\nThe original body.\n' >"$task"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  grep -Fx 'The original body.' "$task"
  grep -Fx 'resume: true' "$task"
  grep -q '^worker_id: worker:feat/42-do-a-thing#' "$task"
  ! grep -q '^stale_header:' "$task"
  [ "$(grep -cFx '## Task' "$task")" -eq 1 ]
}

# `crew adopt` cannot infer a claim from the kind:"dispatch" row — that row
# carries no issue number and is written far later — so the claim records itself
# at claim time. The issue is a JSON string (jq --arg), not a number.
@test "claim: an issue dispatch writes a claim-issue bus row" {
  stub_launch_bins
  stub_gh_claim "" ""
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="claim-issue") | "\(.crew_id) \(.issue) \(.issue|type) \(.branch)"' "$log"
  [ "$output" = "c1 42 string feat/42-do-a-thing" ]
}

@test "claim: a Linear dispatch writes no claim-issue row" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 ENG-1234 "linear thing"
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  ! grep -q 'claim-issue' "$log"
}

@test "claim: a --pr dispatch writes no claim-issue row" {
  stub_pr_bins eng-7691-foo
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  ! grep -q 'claim-issue' "$log"
}

# `wt switch -c` was also, accidentally, the thing that refused a branch checked
# out where a worker has no business opening. Occupancy keys on @crew_name, so it
# reads all three of these as empty — the resume arm has to refuse them itself.
@test "resume: refuses when the branch is checked out in the primary worktree" {
  stub_launch_bins
  git -C "$TEST_REPO" branch feat/42-do-a-thing
  git -C "$TEST_REPO" checkout -q feat/42-do-a-thing
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"primary worktree"* ]]
  ! grep -q '^switch' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

@test "resume: refuses when the worktree is the directory dispatch runs from" {
  setup_resume_branch feat/42-do-a-thing
  cd "$TEST_REPO/.dispatch-wt/feat-42-do-a-thing"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"the worktree this dispatch is running from"* ]]
  ! grep -q '^switch' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

# The scan's field order is window_id / pane_current_path / @crew_name —
# deliberately NOT `crew occupants`' order. Tab is IFS whitespace, so an empty
# middle field collapses and `read` shifts the path into it; @crew_name goes last
# precisely because empty is the value being matched on. This stub mirrors that.
# list-panes, not list-windows (#73): a window format only resolves the ACTIVE
# pane's path, missing a human in an inactive pane at the same path.
@test "resume: refuses a tmux window sitting in the worktree with no worker identity" {
  setup_resume_branch feat/42-do-a-thing
  export RESUME_WT="$TEST_REPO/.dispatch-wt/feat-42-do-a-thing"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-panes) printf '%s\t%s\t\n' '@9' "$RESUME_WT" ;;
new-window) printf '%s %s\n' '%1' '%1' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"window @9 is sitting in $RESUME_WT"* ]]
  [[ "$output" == *"no worker identity"* ]]
  ! grep -q '^switch' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

# The occupancy gate runs BEFORE the resume arm, so a live worker is still
# refused rather than resumed on top of. #72's case: a bare `exited` row can be
# the newest event while the session is live, so a live engine pane still
# refuses. The `working` case is pinned by "gate: refuses a live occupant on a
# create-mode re-dispatch" above, which now takes this same resume path.
@test "gate: refuses a terminal exited session with a live engine on a resume dispatch" {
  setup_resume_branch feat/42-do-a-thing
  stub_crew_gate \
    '[{"window":"@23","name":"sage","pane":"%33","engine":true}]' \
    '[{"session":"s1-1","worker_id":"worker:feat/42-do-a-thing","state":"exited","ts":1,"age_s":5,"terminal":true}]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"exited"* ]]
  [[ "$output" == *"crew reply worker:feat/42-do-a-thing"* ]]
  ! grep -q 'kill-window' "$STUB_LOG"
  ! grep -q '^switch' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

# The slug is a lossy projection of the title, so a reworded re-dispatch of the
# same id resolves to a NEW name, creates cleanly, and silently strands the first
# branch's uncommitted work. Warn and name it — the original title only survives
# on the sibling's own bus row, and is printed as data, never as a command. The
# seeded title carries an escaped control char, which must not reach the terminal.
@test "create: warns about a sibling branch for the same id and still launches" {
  stub_launch_bins
  git -C "$TEST_REPO" branch feat/42-old-wording
  crew_dir="$TEST_REPO/.git/crew"
  mkdir -p "$crew_dir"
  printf '%s\n' '{"ts":100,"crew_id":"c0","kind":"dispatch","branch":"feat/42-old-wording","title":"Old\u0007 wording"}' >"$crew_dir/events.jsonl"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"branch(es) for this id already exist:"* ]]
  [[ "$output" == *"feat/42-old-wording"* ]]
  [[ "$output" == *$'\n    Old wording'* ]]
  grep -q 'switch -c feat/42-do-a-thing' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

@test "create: says so when no bus row carries the sibling's title" {
  stub_launch_bins
  git -C "$TEST_REPO" branch feat/42-old-wording
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 0 ]
  [[ "$output" == *"feat/42-old-wording"* ]]
  [[ "$output" == *"No bus row carries its original title"* ]]
  grep -q 'new-window' "$STUB_LOG"
}
