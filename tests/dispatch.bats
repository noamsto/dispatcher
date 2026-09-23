bats_require_minimum_version 1.5.0 # `run !`

setup() {
  load helpers
  DISPATCH="$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
  export CREW_REAL="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_dispatch() { bash -euo pipefail "$DISPATCH" "$@"; }
  setup_repo
  # A work shell exports DISPATCH_PROFILE and any dispatcher session exports
  # CREW_ID; bats inherits both, so without this the suite passes on a work box
  # and fails on a personal one. HOME points at the throwaway repo so the codex
  # cache fixture and the --mcp config paths cannot reach the developer's own.
  export HOME="$TEST_REPO"
  unset CREW_WORKER_ID DISPATCH_PROFILE CREW_ID DISPATCH_SKIP_MODEL_CHECK DISPATCH_IGNORE_RUNG DISPATCH_SPEC DISPATCH_SHAPE TMUX_PANE DISPATCH_DRAFT_PR
  stub_bin tmux
  stub_bin crew
  # pi-agent-dir delegates to the real crew.sh so pi launch tests exercise a
  # real seed; every other subcommand keeps stub_bin's generic log-and-succeed.
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  stub_bin gh
  stub_bin wt
  stub_bin direnv
  # Engine CLIs are stubbed by setup_repo (they are never executed by a
  # launch — dispatch hands tmux a command string — but dispatch probes them
  # for availability).
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR"/{WORKER_PROTOCOL.md,EVIDENCE_REVIEW.md,GRID_PROTOCOL.md,REVIEW_TASK.md}
  # Stands in for the store path flake.nix bakes as @skillsDir@; pi launches
  # pass it with --skill (#225).
  export DISPATCHER_SKILLS_DIR="$TEST_REPO/harness-skills"
  mkdir -p "$DISPATCHER_SKILLS_DIR/spec-plan-critic"
  printf -- '---\nname: spec-plan-critic\ndescription: seeded\n---\n' \
    >"$DISPATCHER_SKILLS_DIR/spec-plan-critic/SKILL.md"
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
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

# A substituted-build simulation for the protocol-rev guard (#184, #193):
# flake.nix's replaceStrings bakes the content hash into the built scripts as a
# literal. A bats run of the raw script cannot carry one, so the guard tests run
# a scratch copy with a marker baked the same way. $1 is the rev to bake
# (default: one that no real content hashes to, so "refuses" tests need not
# precompute anything); a matching test computes the fixture dir's runtime hash
# via _protocol_dir_rev and bakes that instead. The red tests prove the guard
# refuses on the substituted path; the raw-script path skips with a warning.
_substituted_dispatch() { # [rev]
  local rev="${1:-0123456789abcdef}"
  sed "s/@protocolRev@/$rev/" "$DISPATCH" >"$BATS_TEST_TMPDIR/dispatch-subst.sh"
  export DISPATCH_SUBST="$BATS_TEST_TMPDIR/dispatch-subst.sh"
  run_subst_dispatch() { bash -euo pipefail "$DISPATCH_SUBST" "$@"; }
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

# Mirrors the real cache: $XDG_DATA_HOME/crew/cursor-models-cache.json, as
# produced by refresh-models.sh. $1 is fetched_epoch (so tests can control
# freshness); the slug list is a small fixed catalog that deliberately does
# NOT include cursor-grok-4.5-high or a bare claude-opus-5.
write_cursor_models_cache() { # <fetched_epoch>
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$1" \
    '{fetched_at: "t", fetched_epoch: $epoch, models: [{slug:"cursor-grok-4.6-high"},{slug:"cursor-grok-4.6-medium"},{slug:"cursor-grok-4.6-medium-fast"},{slug:"cursor-grok-4.6-low"},{slug:"cursor-grok-4.6-low-fast"},{slug:"grok-4.7-high"},{slug:"grok-4.7-medium"},{slug:"grok-4.7-low"},{slug:"claude-opus-5-high"}]}' \
    >"$XDG_DATA_HOME/crew/cursor-models-cache.json"
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

@test "resume intercepts before the positional tier parse and execs dispatch-resume" {
  stub_bin dispatch-resume
  run run_dispatch resume --print
  [ "$status" -eq 0 ]
  grep -Fq -- '--print' "$STUB_LOG"
  [[ "$output" != *"usage: dispatch"* ]]
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

@test "rejects an engine that is not on the roster" {
  DISPATCH_ENGINES="claude pi" run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "roster test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent codex is not enabled here (enabled: claude pi)"* ]]
}

@test "an unset roster admits every engine" {
  # The compatibility contract: a non-Nix checkout exports nothing.
  run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "roster test"
  [[ "$output" != *"not enabled here"* ]]
}

@test "the roster gate replaces the work-profile gate" {
  # codex off a work profile used to be rejected on profile alone; with a
  # roster that lists it, profile is no longer the gate.
  DISPATCH_PROFILE=personal DISPATCH_ENGINES="claude codex pi" run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "roster test"
  [[ "$output" != *"work-profile only"* ]]
  [[ "$output" != *"not enabled here"* ]]
}

@test "rejects an enabled engine whose CLI is missing" {
  # PATH keeps the stub dir (tmux, crew, gh, wt are needed to get this far)
  # but the engine stub is removed, so only the probe can fail.
  rm "$STUB_DIR/codex"
  PATH="$(path_without_real codex)" DISPATCH_ENGINES="claude codex pi" run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 "probe test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent codex is enabled but not installed (no 'codex' on PATH)"* ]]
}

@test "the probe looks for cursor-agent, not cursor" {
  rm "$STUB_DIR/cursor-agent"
  PATH="$(path_without_real cursor-agent)" DISPATCH_ENGINES="claude cursor pi" run run_dispatch standard composer-2.5 --agent cursor --effort medium --crew-id c1 "probe test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"no 'cursor-agent' on PATH"* ]]
}

@test "--engines prints the effective roster" {
  DISPATCH_ENGINES="claude codex pi" run run_dispatch --engines
  [ "$status" -eq 0 ]
  [[ "$output" == $'claude\ncodex\npi' ]]
}

@test "--engines omits an engine whose CLI is missing" {
  rm "$STUB_DIR/codex"
  PATH="$(path_without_real codex)" DISPATCH_ENGINES="claude codex pi" run run_dispatch --engines
  [ "$status" -eq 0 ]
  [[ "$output" == $'claude\npi' ]]
}

@test "--engines needs no crew id, worktree or tmux" {
  run run_dispatch --engines
  [ "$status" -eq 0 ]
  [[ "$output" != *"no crew id"* ]]
}

@test "the pi worker launch and its role panes use the worker agent dir with --no-approve" {
  # Personal pi standard auto-enables the plan-critic,reviewer grid, which is
  # what gives the role-pane assertions below real launches to check.
  stub_launch_bins
  mkdir -p "$HOME/.pi/agent"
  printf '{"opencode":{"type":"api_key","key":"SECRET-DISPATCH-FIXTURE"}}\n' >"$HOME/.pi/agent/auth.json"
  printf '{"defaultProjectTrust":"always"}\n' >"$HOME/.pi/agent/settings.json"
  before_auth=$(sha256sum "$HOME/.pi/agent/auth.json")
  before_settings=$(sha256sum "$HOME/.pi/agent/settings.json")

  DISPATCH_PROFILE=personal run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "worker agent dir trust test"
  [ "$status" -eq 0 ]

  worker_dir="$HOME/.pi/dispatcher-worker"
  lead_line=$(grep -F -- "PI_CODING_AGENT_DIR=$worker_dir pi --name iris --model" "$STUB_LOG")
  [[ "$lead_line" == *"--no-approve"* ]]
  [[ "$lead_line" == *"--append-system-prompt $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md"* ]]
  [[ "$lead_line" == *"--thinking high"* ]]
  # No project skills in this worktree, so the harness dir is the only --skill.
  [[ "$lead_line" == *"--no-approve --skill $DISPATCHER_SKILLS_DIR "* ]]

  plan_critic_line=$(grep -F -- "PI_CODING_AGENT_DIR=$worker_dir pi --name iris-plan-critic" "$STUB_LOG")
  reviewer_line=$(grep -F -- "PI_CODING_AGENT_DIR=$worker_dir pi --name iris-reviewer" "$STUB_LOG")
  [[ "$plan_critic_line" == *"--no-approve"* ]]
  [[ "$reviewer_line" == *"--no-approve"* ]]

  # No pi send-keys line escaped the worker-dir prefix.
  total_pi_lines=$(grep -cF -- ' pi --name' "$STUB_LOG" || true)
  prefixed_pi_lines=$(grep -cF -- "PI_CODING_AGENT_DIR=$worker_dir pi --name" "$STUB_LOG" || true)
  [ "$total_pi_lines" = "$prefixed_pi_lines" ]
  [ "$total_pi_lines" -eq 3 ]

  [ "$(jq -r .defaultProjectTrust "$worker_dir/settings.json")" = never ]

  [ "$(sha256sum "$HOME/.pi/agent/auth.json")" = "$before_auth" ]
  [ "$(sha256sum "$HOME/.pi/agent/settings.json")" = "$before_settings" ]

  run grep -rF SECRET-DISPATCH-FIXTURE "$worker_dir"
  [ "$status" -ne 0 ]
}

@test "pi worker launch passes the worktree's project skills and the harness skills with --skill" {
  stub_launch_bins
  # Seed a project skill into the worktree the wt stub creates. dispatch looks
  # the worktree path up after `wt switch`, so it must exist by then. A real
  # git-tracked skill would do too; untracked is enough for the directory probe.
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = switch ]; then
  br=""
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
    -c) br="$2"; shift 2 ;;
    *) shift ;;
    esac
  done
  [ -n "$br" ] || exit 1
  dest="$TEST_REPO/.dispatch-wt/${br//\//-}"
  mkdir -p "$(dirname "$dest")"
  git -C "$TEST_REPO" worktree add -b "$br" "$dest" HEAD >/dev/null
  mkdir -p "$dest/.agents/skills/preview"
  printf -- '---\nname: preview\ndescription: seeded\n---\n' >"$dest/.agents/skills/preview/SKILL.md"
fi
exit 0
EOF
  chmod +x "$STUB_DIR/wt"

  mkdir -p "$HOME/.pi/agent"
  printf '{"opencode":{"type":"api_key","key":"x"}}\n' >"$HOME/.pi/agent/auth.json"
  DISPATCH_PROFILE=personal run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "skill pass-through"
  [ "$status" -eq 0 ]

  # The lead carries WORKER_PROTOCOL.md; each grid role pane gets its own
  # --skill too, but this asserts the worker launch specifically.
  lead_line=$(grep -F -- "--append-system-prompt $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$STUB_LOG")
  [[ "$lead_line" == *"--no-approve --skill $TEST_REPO/.dispatch-wt/"*"/.agents/skills --skill $DISPATCHER_SKILLS_DIR "* ]]

  # The role pane goes through launch_role, whose worktree arrives as an
  # argument — assert the grid path passes it through.
  role_line=$(grep -F -- "PI_CODING_AGENT_DIR=$HOME/.pi/dispatcher-worker pi --name iris-plan-critic" "$STUB_LOG")
  [[ "$role_line" == *"--no-approve --skill $TEST_REPO/.dispatch-wt/"*"/.agents/skills --skill $DISPATCHER_SKILLS_DIR "* ]]
}

@test "pi dispatch refuses to launch when the agent dir cannot be seeded" {
  stub_launch_bins
  # A crew stub whose pi-agent-dir prints nothing (the generic log-and-succeed
  # behaviour), so the seed comes back empty and the launch must abort first.
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
identity) printf '%s\n' '{"name":"iris","color":"blue","tmux":"colour33"}' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"

  DISPATCH_PROFILE=personal run run_dispatch trivial openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "fail closed on unseedable dir"
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not seed the pi worker agent dir"* ]]
  run grep -c -- 'send-keys' "$STUB_LOG"
  [ "$status" -ne 0 ]
  run grep -c -- 'new-window' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "a non-pi lead with a pi role in an up-front grid still seeds the pi worker agent dir" {
  stub_launch_bins

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --agent claude --roles "reviewer=pi:openrouter/deepseek/deepseek-v4-flash" --effort high --crew-id c1 42 "claude lead with pi reviewer role"
  [ "$status" -eq 0 ]

  worker_dir="$HOME/.pi/dispatcher-worker"
  reviewer_line=$(grep -F -- "PI_CODING_AGENT_DIR=$worker_dir pi --name" "$STUB_LOG")
  [[ "$reviewer_line" == *"-reviewer"* ]]
  [[ "$reviewer_line" == *"--no-approve"* ]]
}

@test "pi dispatch refuses to launch a non-pi lead's pi role when the agent dir cannot be seeded" {
  stub_launch_bins
  # A crew stub whose pi-agent-dir prints nothing (the generic log-and-succeed
  # behaviour), so the seed comes back empty and the launch must abort first.
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
identity) printf '%s\n' '{"name":"iris","color":"blue","tmux":"colour33"}' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"

  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --agent claude --roles "reviewer=pi:openrouter/deepseek/deepseek-v4-flash" --effort high --crew-id c1 42 "fail closed on unseedable dir for a role"
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not seed the pi worker agent dir"* ]]
  run grep -c -- 'send-keys' "$STUB_LOG"
  [ "$status" -ne 0 ]
  run grep -c -- 'new-window' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "--roles needs a value" {
  run run_dispatch standard sonnet --roles
  [ "$status" -eq 1 ]
  [[ "$output" == *"--roles needs a comma-separated list"* ]]
}

@test "a role cannot use an engine that is off the roster" {
  DISPATCH_ENGINES="claude pi" run run_dispatch standard sonnet --agent claude --effort medium --crew-id c1 --roles reviewer=cursor:composer-2.5 "role roster test"
  [ "$status" -eq 1 ]
  [[ "$output" == *"role 'reviewer' uses --agent cursor is not enabled here"* ]]
}

@test "--roles rejects a role agent not in the roster" {
  DISPATCH_ENGINES="pi" run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --roles "reviewer=codex:gpt-5.6-sol" --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not enabled here"* ]]
}

@test "rejects an invalid role name" {
  run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --roles "reviewer,bad role" --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"invalid role 'bad role'"* ]]
}

@test "rejects empty and duplicate role entries" {
  run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --roles "reviewer,,plan-critic" --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"empty entry"* ]]

  run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --roles "reviewer,reviewer" --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"duplicate role 'reviewer'"* ]]
}

@test "the role grid splits the task window and labels panes by role" {
  run grep -F -- 'split-window -t "$win"' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- '@crew_role' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'GRID_PROTOCOL.md' "$DISPATCH"
  [ "$status" -eq 0 ]
}

# _grid_tmux_stub — stub_launch_bins' tmux, but split-window returns a pane id.
_grid_tmux_stub() {
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
new-window) printf '%s %s\n' '%1' '%1' ;;
split-window) printf '%s\n' '%6' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

# _env_of <name> <line> — the value of `-e <name>=…` on a logged tmux command.
_env_of() {
  local rest="${2#*-e $1=}"
  printf '%s' "${rest%% -*}"
}

@test "grid: every eager role pane gets the lead's CREW_WORKER_ID and CREW_ID" {
  stub_launch_bins
  _grid_tmux_stub
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run run_dispatch \
    standard sonnet --agent claude --roles "reviewer,plan-critic" --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]

  lead="$(grep '^new-window' "$STUB_LOG")"
  [ "$(_env_of CREW_WORKER_ID "$lead")" = "worker:feat/42-do-a-thing#s7-7" ]
  splits="$(grep '^split-window' "$STUB_LOG")"
  [ "$(printf '%s\n' "$splits" | wc -l)" -eq 2 ]
  while IFS= read -r line; do
    [ "$(_env_of CREW_WORKER_ID "$line")" = "$(_env_of CREW_WORKER_ID "$lead")" ]
    [ "$(_env_of CREW_ID "$line")" = c1 ]
    [ "$(_env_of CREW_ID "$line")" = "$(_env_of CREW_ID "$lead")" ]
  done <<<"$splits"
  [[ "$splits" == *"-e CREW_ROLE_ID=role:feat/42-do-a-thing:reviewer "* ]]
  [[ "$splits" == *"-e CREW_ROLE_ID=role:feat/42-do-a-thing:plan-critic "* ]]
}

@test "grid: the --status pane gets the lead's identity too" {
  stub_launch_bins
  _grid_tmux_stub
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run run_dispatch \
    standard sonnet --agent claude --roles reviewer --status --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]
  [ "$(grep -c '^split-window.* -e CREW_WORKER_ID=worker:feat/42-do-a-thing#s7-7 -e CREW_ID=c1 ' "$STUB_LOG")" -eq 2 ]
}

@test "a grid lead's window border carries the lead marker and state" {
  stub_launch_bins
  # Lazy grid: role_names is populated and roles.json recorded, but no role
  # pane spawns up front, so the run stays quiet (no watch_role loops).
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --lazy --roles reviewer --effort high --crew-id c1 42 "grid lead border"
  [ "$status" -eq 0 ]
  # Window-level border labels the lead with its live state; the pane-level role
  # border format is untouched (it would regress role panes).
  run grep -F -- '#{@crew_name}#[nobold] lead · #{@crew_state}' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'pane-border-format " #[bold]#{@crew_role}#[nobold] #{@crew_state} "' "$DISPATCH"
  [ "$status" -ne 0 ]
  run grep -F -- 'pane-border-format " $(state_glyph' "$DISPATCH"
  [ "$status" -eq 0 ]
  # The lead advertises @crew_role=lead so the status-publish guard can find it.
  run grep -F -- 'set-option -p -t %1 @crew_role lead' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "a non-grid dispatch keeps the lead's border unmarked" {
  stub_launch_bins
  # Standard claude has no default grid, so this reaches window
  # creation as a plain single-pane worker window.
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --effort high --crew-id c1 42 "non-grid lead border"
  [ "$status" -eq 0 ]
  run grep -F -- 'pane-border-format  #[bold]#{@crew_name}#[nobold] lead' "$STUB_LOG"
  [ "$status" -ne 0 ]
  run grep -F -- 'set-window-option -t %1 pane-border-format  #[bold]#{@crew_name}#[nobold]' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "a grid lead's @crew_name stays the bare codename" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --lazy --roles reviewer --effort high --crew-id c1 42 "grid crew name unchanged"
  [ "$status" -eq 0 ]
  # The join key is set verbatim, and the marker never lands in any @crew_name
  # value — the border format line is the only place it belongs.
  run grep -F -- 'set-window-option -t %1 @crew_name iris' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -E -- '@crew_name[^}]* lead' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "grid: creation publishes the window and lead-pane hint options" {
  stub_launch_bins
  _grid_tmux_stub
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --roles reviewer --effort high --crew-id c1 42 "grid hints"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-window-option -t %1 @crew_grid 1' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-window-option -t %1 @crew_grid_main_pct 60' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-option -p -t %1 @crew_role lead' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "grid: refit uses tmux-grid-refit when present, main-vertical otherwise" {
  stub_launch_bins
  _grid_tmux_stub
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --roles reviewer --effort high --crew-id c1 42 "no refit"
  [ "$status" -eq 0 ]
  run grep -F -- 'select-layout -t %1 main-vertical' "$STUB_LOG"
  [ "$status" -eq 0 ]

  : >"$STUB_LOG"
  cat >"$STUB_DIR/tmux-grid-refit" <<'EOF'
#!/usr/bin/env bash
printf 'tmux-grid-refit %s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/tmux-grid-refit"
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --roles reviewer --effort high --crew-id c1 42 "with refit"
  [ "$status" -eq 0 ]
  run grep -F -- 'tmux-grid-refit %1' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run ! grep -q -- 'select-layout' "$STUB_LOG"
}

@test "grid: --spawn-role publishes @crew_grid and refits" {
  _spawn_role_fixture
  cat >"$STUB_DIR/tmux-grid-refit" <<'EOF'
#!/usr/bin/env bash
printf 'tmux-grid-refit %s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/tmux-grid-refit"
  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 0 ]
  run grep -F -- 'set-window-option -t @1 @crew_grid 1' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-option -p -t %5 @crew_role lead' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'tmux-grid-refit @1' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "grid: --reap-roles unsets @crew_grid once the last role pane is gone" {
  _spawn_role_fixture
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' '@1' ;;
list-panes)
  case " $* " in
  *'#{pane_id}'*) printf '%s\n' '%5 ' '%6 reviewer' ;;
  *)
    if [ -f "$STUB_LOG.killed" ]; then printf '%s\n' 'lead'; else printf '%s\n' 'lead' 'reviewer'; fi
    ;;
  esac
  ;;
kill-pane) touch "$STUB_LOG.killed" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  run run_dispatch --reap-roles
  [ "$status" -eq 0 ]
  run grep -F -- 'kill-pane -t %6' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'set-window-option -t @1 -u @crew_grid' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "grid: --reap-roles never kills a lead pane row" {
  _spawn_role_fixture
  export TMUX_PANE=%9
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' '@1' ;;
list-panes)
  case " $* " in
  *'#{pane_id}'*) printf '%s\n' '%5 lead' '%9 ' ;;
  *) printf '%s\n' 'lead' ;;
  esac
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  run run_dispatch --reap-roles
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-pane' "$STUB_LOG"
}

@test "grid: a real tmux server stores the hint contract, and reap unsets it" {
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"
  stub_launch_bins
  sock="gh$$"
  "$REAL_TMUX" -L "$sock" -f /dev/null new-session -d -s t -x 200 -y 50
  win_real="$("$REAL_TMUX" -L "$sock" list-windows -t t -F '#{window_id}' | head -1)"
  pane_real="$("$REAL_TMUX" -L "$sock" list-panes -t t -F '#{pane_id}' | head -1)"
  [ -n "$win_real" ]
  [ -n "$pane_real" ]
  # A tmux shim: log everything, hand dispatch the REAL window/pane ids, and
  # forward the option-setting calls that target them to the real server. Role
  # panes get a canned id the shim never forwards, so decorate_pane cannot
  # overwrite the lead's @crew_role. display-message exits 1 so the nohup'd role
  # watcher's loop ends at once.
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1" in
new-window) printf '%s %s\n' '$win_real' '$pane_real' ;;
split-window) printf '%s\n' '%99' ;;
display-message) exit 1 ;;
set-option|set-window-option)
  for a in "\$@"; do
    case "\$a" in
    '$win_real'|'$pane_real') exec "$REAL_TMUX" -L '$sock' "\$@" ;;
    esac
  done
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"

  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --roles reviewer --effort high --crew-id c1 42 "real grid hints"
  [ "$status" -eq 0 ]
  [ "$("$REAL_TMUX" -L "$sock" show-options -w -v -q -t "$win_real" @crew_grid)" = 1 ]
  [ "$("$REAL_TMUX" -L "$sock" show-options -w -v -q -t "$win_real" @crew_grid_main_pct)" = 60 ]
  [ "$("$REAL_TMUX" -L "$sock" show-options -p -v -q -t "$pane_real" @crew_role)" = lead ]
  [ "$("$REAL_TMUX" -L "$sock" show-options -p -v -q -t "$pane_real" @crew_state)" = working ]

  # Reap against the real server: its only pane is the lead, so the last role
  # pane is already gone and the grid hint must be unset.
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1" in
display-message|list-panes|set-option|set-window-option) exec "$REAL_TMUX" -L '$sock' "\$@" ;;
kill-pane) ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  TMUX_PANE="$pane_real" run run_dispatch --reap-roles
  [ "$status" -eq 0 ]
  [ -z "$("$REAL_TMUX" -L "$sock" show-options -w -v -q -t "$win_real" @crew_grid)" ]

  "$REAL_TMUX" -L "$sock" kill-server 2>/dev/null || true
}

@test "grid: the border formats expand to a glyph per state (real tmux)" {
  [ -n "$REAL_TMUX" ] || skip "tmux not installed"
  stub_launch_bins
  _grid_tmux_stub
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --roles reviewer --effort high --crew-id c1 42 "grid glyphs"
  [ "$status" -eq 0 ]
  lead_fmt="$(grep -F 'pane-border-format' "$STUB_LOG" | grep -F '@crew_name' | head -1)"
  lead_fmt="${lead_fmt#*pane-border-format }"
  role_fmt="$(grep -F 'set-option -p -t %6 pane-border-format' "$STUB_LOG" | head -1)"
  role_fmt="${role_fmt#*pane-border-format }"
  [ -n "$lead_fmt" ]
  [ -n "$role_fmt" ]

  sock="gl$$"
  "$REAL_TMUX" -L "$sock" -f /dev/null new-session -d -s t -x 200 -y 50
  pane="$("$REAL_TMUX" -L "$sock" list-panes -t t -F '#{pane_id}' | head -1)"
  "$REAL_TMUX" -L "$sock" set-option -w -t "$pane" @crew_name test
  "$REAL_TMUX" -L "$sock" set-option -w -t "$pane" @crew_color colour250
  "$REAL_TMUX" -L "$sock" set-option -p -t "$pane" @crew_role reviewer
  "$REAL_TMUX" -L "$sock" set-option -p -t "$pane" @crew_role_color colour250
  for spec in 'working:●' 'idle:○' 'blocked:⚠' 'done:✓' 'pr_open:✓' 'failed:✗' 'exited:✗'; do
    st="${spec%%:*}" g="${spec##*:}"
    "$REAL_TMUX" -L "$sock" set-option -p -t "$pane" @crew_state "$st"
    out="$("$REAL_TMUX" -L "$sock" display-message -p -t "$pane" "$lead_fmt")"
    case "$out" in *"$g"*) ;; *) echo "lead $st -> $out"; return 1 ;; esac
    out="$("$REAL_TMUX" -L "$sock" display-message -p -t "$pane" "$role_fmt")"
    case "$out" in *"$g"*) ;; *) echo "role $st -> $out"; return 1 ;; esac
  done
  # A watchdog blocked renders distinctly and carries the phase detail.
  "$REAL_TMUX" -L "$sock" set-option -p -t "$pane" @crew_state blocked
  "$REAL_TMUX" -L "$sock" set-option -p -t "$pane" @crew_source watchdog
  "$REAL_TMUX" -L "$sock" set-option -p -t "$pane" @crew_detail "stalled: no output"
  out="$("$REAL_TMUX" -L "$sock" display-message -p -t "$pane" "$lead_fmt")"
  [[ "$out" == *"blocked (watchdog)"* ]]
  [[ "$out" == *"stalled: no output"* ]]
  "$REAL_TMUX" -L "$sock" kill-server 2>/dev/null || true
}

@test "grid: the lead border builders stay identical in dispatch and resume" {
  resume="$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  for fn in state_glyph grid_lead_format; do
    a="$(sed -n "/^$fn()/,/^}/p" "$DISPATCH")"
    b="$(sed -n "/^$fn()/,/^}/p" "$resume")"
    [ -n "$a" ]
    [ "$a" = "$b" ] || { echo "$fn drifted"; return 1; }
  done
  [ "$(grep -F 'theme_colour() { printf' "$DISPATCH")" = "$(grep -F 'theme_colour() { printf' "$resume")" ]
}

@test "grid mode is stamped into the task doc and the lead prompt" {
  # The lead must know it has role panes (roles: line in WORKER_TASK.md) and be
  # told to delegate the critic/review phases (grid_note in its prompt).
  run grep -F -- 'roles: %s' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'grid_note' "$DISPATCH"
  [ "$status" -eq 0 ]
  [ "$(grep -c '\${grid_note}' "$DISPATCH")" -eq 4 ]
}

@test "grid_note never tells the lead the review gate is substitutive" {
  # The injected launch-prompt text is what actually drives worker behavior;
  # it must not resurrect the old "delegate review instead of running it
  # in-process" framing the additive-review-gate fix removed everywhere else.
  run grep -F -- 'instead of running them in-process' "$DISPATCH"
  [ "$status" -ne 0 ]
  run grep -F -- 'delegate to the bus only the phases that have a pane' "$DISPATCH"
  [ "$status" -eq 0 ]
}

# #216: grid_note's literal `'Grid mode'` apostrophes sit inside a manually
# single-quoted chunk of the tmux send-keys argument, so the pane's own shell
# sees them as real quoting, not literal text. This replays the exact launch
# string dispatch.sh builds through bash — the devShell/CI-guaranteed shell,
# not fish (the real pane shell): the POSIX single-quote-escape idiom being
# tested is shell-agnostic, so bash is sufficient proof of the escaping itself.
@test "grid_note's embedded apostrophes survive intact into the codex lead's argv" {
  stub_launch_bins

  # A throwaway codex stub: dump the argv it receives, NUL-separated, so the
  # test can tell one intact argument from several broken ones without any
  # shell re-quoting of its own getting in the way.
  cat >"$STUB_DIR/codex" <<'EOF'
#!/usr/bin/env bash
printf '%s\0' "$@" >"$STUB_DIR/codex_argv"
exit 0
EOF
  chmod +x "$STUB_DIR/codex"

  # deep + --agent codex with no --roles defaults to the critics-only grid
  # (spec-critic,plan-critic), which is what sets grid_note in the lead's
  # prompt — the exact repro shape from the bug report.
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "grid note apostrophe repro"
  [ "$status" -eq 0 ]

  # The same grid also spawns two role-pane codex launches via launch_role;
  # those have no agents.* flags (verified by reading launch_role's codex
  # branch), so this marker isolates the lead's line alone.
  launch="$(grep 'send-keys' "$STUB_LOG" | grep -F 'agents.enabled=true')"
  [ -n "$launch" ]
  [ "$(printf '%s\n' "$launch" | wc -l)" -eq 1 ]

  cmd="${launch#send-keys -t %1 }"
  cmd="${cmd% Enter}"
  bash -c "$cmd"

  argv=()
  while IFS= read -r -d '' arg; do
    argv+=("$arg")
  done <"$STUB_DIR/codex_argv"

  # Fixed flags today: --profile worker -m <model> -c model_reasoning_effort=
  # <effort> -c service_tier=default -c agents.enabled=true -c
  # agents.max_concurrent_threads_per_session=3 -c
  # agents.default_subagent_reasoning_effort=<subagent effort>
  # --dangerously-bypass-approvals-and-sandbox — 15 tokens, followed by
  # exactly one correctly-quoted prompt argument (16 total).
  [ "${#argv[@]}" -eq 16 ]

  # The exact contiguous span the bug splits, apostrophes intact.
  [[ "${argv[15]}" == *"Follow WORKER_PROTOCOL.md 'Grid mode' — delegate to the bus only the phases that have a pane"* ]]
}

# #216/#217/#230: dispatch.sh's shell_quote helper is the one thing standing
# between six call sites (the four main launch branches plus launch_role's
# prompt and first) and shell injection. This proves the MECHANISM itself is
# sound: it runs the real helper against a string hostile enough to have broken
# naive escaping — quotes, a semicolon, a backtick command substitution, a
# $(...) command substitution, a variable, an em dash — and checks what a real
# shell does with the result.
@test "shell_quote neutralizes a hostile prompt string" {
  marker1="$BATS_TEST_TMPDIR/pwned_marker"
  marker2="$BATS_TEST_TMPDIR/pwned_marker2"
  rm -f "$marker1" "$marker2"

  # A throwaway argv-dump stub: it creates no file itself, so any marker that
  # shows up afterward can only have come from the payload actually running.
  stub="$BATS_TEST_TMPDIR/argv_dump"
  argv_file="$BATS_TEST_TMPDIR/argv_dump.out"
  cat >"$stub" <<EOF
#!/usr/bin/env bash
printf '%s\0' "\$@" >"$argv_file"
exit 0
EOF
  chmod +x "$stub"

  # Backslashes are the fish trap: fish reads \\ and \' as escapes inside
  # single quotes, bash does not, so they must be quoted out.
  prompt="it's \"over\"; \`touch $marker1\`; \$(touch $marker2) — \$HOME a\\\\b x\\'y end\\"
  eval "$(sed -n '/^shell_quote() {/,/^}/p' "$DISPATCH")"
  shell_quote quoted_prompt "$prompt"

  # bash is the CI shell; fish is the real pane shell, so replay through it too
  # wherever it is installed.
  shells=(bash)
  command -v fish >/dev/null && shells+=(fish)
  for sh in "${shells[@]}"; do
    rm -f "$argv_file"
    "$sh" -c "$stub $quoted_prompt"

    argv=()
    while IFS= read -r -d '' arg; do
      argv+=("$arg")
    done <"$argv_file"

    # Exactly one argument reached the stub — no word-splitting on the
    # semicolon or the quote — and the text made it through byte-for-byte.
    [ "${#argv[@]}" -eq 1 ]
    [ "${argv[0]}" = "$prompt" ]
    # Neither command substitution ran.
    [ ! -e "$marker1" ]
    [ ! -e "$marker2" ]
  done
}

@test "shell_quote is applied at every prompt-quoting call site" {
  # Six call sites build their tmux send-keys prompt this way: the four main
  # launch branches (codex, cursor, pi, claude) plus launch_role's $prompt and
  # $first. A change to this count means a call site was added, removed, or
  # reverted to hand-rolled quoting — worth a second look either way.
  [ "$(grep -cF -- 'shell_quote quoted_' "$DISPATCH")" -eq 6 ]
  [ "$(grep -cF -- 'escaped=${' "$DISPATCH")" -eq 0 ]
}

# Replays the lead's send-keys command through bash with a stub engine binary
# first on PATH, leaving the argv the stub received in the global `argv`.
# $1 is the engine binary, $2 a fixed substring selecting the lead's line
# (role panes launch the same binary).
_replay_lead_launch() {
  local bin="$1" marker="$2" launch cmd arg
  cat >"$STUB_DIR/$bin" <<'EOF'
#!/usr/bin/env bash
printf '%s\0' "$@" >"$STUB_DIR/engine_argv"
exit 0
EOF
  chmod +x "$STUB_DIR/$bin"
  launch="$(grep 'send-keys' "$STUB_LOG" | grep -F -- "$marker")"
  [ -n "$launch" ]
  [ "$(printf '%s\n' "$launch" | wc -l)" -eq 1 ]
  cmd="${launch#send-keys -t %1 }"
  cmd="${cmd% Enter}"
  bash -c "$cmd"
  argv=()
  while IFS= read -r -d '' arg; do
    argv+=("$arg")
  done <"$STUB_DIR/engine_argv"
}

# #230: every engine's lead launch must hand the engine the whole prompt —
# apostrophes from grid_note included — as exactly one trailing argument.
@test "grid_note's apostrophes survive into the claude lead's argv as one prompt argument" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "claude grid apostrophe repro"
  [ "$status" -eq 0 ]

  _replay_lead_launch claude "claude --name iris --model"
  last=$((${#argv[@]} - 1))
  [[ "${argv[last]}" == "Read WORKER_TASK.md and run it end-to-end."* ]]
  [[ "${argv[last]}" == *"Follow WORKER_PROTOCOL.md 'Grid mode' — delegate to the bus only the phases that have a pane"* ]]
  [[ "${argv[last]}" == *"(a reviewer pane is additive, except on pi where it is the gate)."* ]]
  # No fragment of the prompt leaked into an earlier argument.
  for ((i = 0; i < last; i++)); do
    [[ "${argv[i]}" != *"Grid mode"* && "${argv[i]}" != "Read "* ]]
  done
}

@test "grid_note's apostrophes survive into the cursor lead's argv as one prompt argument" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep kimi-k3-high --agent cursor --effort high --crew-id c1 42 "cursor grid apostrophe repro"
  [ "$status" -eq 0 ]

  _replay_lead_launch cursor-agent "--model 'kimi-k3-high' 'Read"
  last=$((${#argv[@]} - 1))
  [[ "${argv[last]}" == "Read $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end."* ]]
  [[ "${argv[last]}" == *"Follow WORKER_PROTOCOL.md 'Grid mode' — delegate to the bus only the phases that have a pane"* ]]
  for ((i = 0; i < last; i++)); do
    [[ "${argv[i]}" != *"Grid mode"* && "${argv[i]}" != "Read "* ]]
  done
}

@test "grid_note's apostrophes survive into the pi lead's argv as one prompt argument" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "pi grid apostrophe repro"
  [ "$status" -eq 0 ]

  # The lead is the one pi launch without a role suffix on --name.
  _replay_lead_launch pi "pi --name iris --model"
  last=$((${#argv[@]} - 1))
  [[ "${argv[last]}" == "Read WORKER_TASK.md and run it end-to-end."* ]]
  [[ "${argv[last]}" == *"Follow WORKER_PROTOCOL.md 'Grid mode' — delegate to the bus only the phases that have a pane"* ]]
  for ((i = 0; i < last; i++)); do
    [[ "${argv[i]}" != *"Grid mode"* && "${argv[i]}" != "Read "* ]]
  done
}

@test "claude lead: protocol_dir is stamped in the task doc and named in the prompt" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --agent claude --effort medium --crew-id c1 42 "claude protocol dir"
  [ "$status" -eq 0 ]
  grep -qx "protocol_dir: $DISPATCHER_PROTOCOL_DIR" "$TEST_REPO/.dispatch-wt/feat-42-claude-protocol-dir/WORKER_TASK.md"
  _replay_lead_launch claude "claude --name iris --model"
  [[ "${argv[$((${#argv[@]} - 1))]}" == *"live in $DISPATCHER_PROTOCOL_DIR"* ]]
}

@test "pi lead: protocol_dir is stamped in the task doc and named in the prompt" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "pi protocol dir"
  [ "$status" -eq 0 ]
  grep -qx "protocol_dir: $DISPATCHER_PROTOCOL_DIR" "$TEST_REPO/.dispatch-wt/feat-42-pi-protocol-dir/WORKER_TASK.md"
  _replay_lead_launch pi "pi --name iris --model"
  [[ "${argv[$((${#argv[@]} - 1))]}" == *"live in $DISPATCHER_PROTOCOL_DIR"* ]]
}

@test "--grid derives the role topology from the tier" {
  run grep -F -- 'standard) grid_roles="plan-critic,reviewer"' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'deep) grid_roles="spec-critic,plan-critic,reviewer"' "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "pi standard and deep workers get the required grid by default" {
  run grep -F -- '[ "$agent" = pi ] && [ "$tier" != trivial ]' "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "deep dispatches on every engine default to the grid too" {
  run grep -F -- 'elif [ "$tier" = deep ] && [ "$plan_val" != provided ]; then' "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "deep claude defaults to a critics-only grid on the lead's own engine and model" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "deep claude grid default"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-deep-claude-grid-default/WORKER_TASK.md"
  grep -Fx 'roles: spec-critic,plan-critic' "$task"
  run grep -F -- 'spec-critic' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "deep codex (work profile) defaults to a critics-only grid on the lead's own engine and model" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "deep codex grid default"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-deep-codex-grid-default/WORKER_TASK.md"
  grep -Fx 'roles: spec-critic,plan-critic' "$task"
}

@test "deep cursor (work profile) defaults to a critics-only grid on the lead's own engine and model" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep kimi-k3-high --agent cursor --effort high --crew-id c1 42 "deep cursor grid default"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-deep-cursor-grid-default/WORKER_TASK.md"
  grep -Fx 'roles: spec-critic,plan-critic' "$task"
}

@test "a plain non-grid dispatch aborts before scaffolding when WORKER_PROTOCOL.md is missing" {
  stub_launch_bins
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-worker"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --effort medium --no-grid --crew-id c1 42 "no worker protocol"
  [ "$status" -ne 0 ]
  [[ "$output" == *"WORKER_PROTOCOL.md"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
  [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
}

@test "deep dispatch aborts before scaffolding when GRID_PROTOCOL.md is missing from the protocol dir" {
  stub_launch_bins
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-grid"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "deep grid missing protocol file"
  [ "$status" -ne 0 ]
  [[ "$output" == *"GRID_PROTOCOL.md"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
  [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
}

@test "refuses a protocol dir whose content hashes to a different revision than the script marker" {
  stub_launch_bins
  _substituted_dispatch
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-mismatch"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev_dir="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  DISPATCH_PROFILE=work run run_subst_dispatch standard sonnet --agent claude --effort medium --no-grid --crew-id c1 42 "rev mismatch"
  [ "$status" -ne 0 ]
  [[ "$output" == *"protocol directory version mismatch"* ]]
  [[ "$output" == *"0123456789abcdef"* ]]
  [[ "$output" == *"$rev_dir"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
  [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
}

@test "refuses a stale protocol dir whose content differs from the script marker" {
  stub_launch_bins
  _substituted_dispatch
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-rev"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  DISPATCH_PROFILE=work run run_subst_dispatch standard sonnet --agent claude --effort medium --no-grid --crew-id c1 42 "stale protocols"
  [ "$status" -ne 0 ]
  [[ "$output" == *"protocol directory version mismatch"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
  [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
}

@test "a protocol dir hashing to the script marker dispatches normally" {
  stub_launch_bins
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-matching"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  _substituted_dispatch "$rev"
  DISPATCH_PROFILE=work run run_subst_dispatch standard sonnet --agent claude --effort medium --no-grid --crew-id c1 42 "rev match"
  [ "$status" -eq 0 ]
  grep -q 'switch' "$STUB_LOG"
  grep -q 'new-window' "$STUB_LOG"
}

@test "a raw (unsubstituted) script skips the revision check with a one-line warning" {
  stub_launch_bins
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-rev"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --effort medium --no-grid --crew-id c1 42 "checkout dev loop"
  [ "$status" -eq 0 ]
  [[ "$output" == *"unsubstituted protocol revision"* ]]
  grep -q 'new-window' "$STUB_LOG"
}

# #253: the protocol-dir guard is engine-independent, but reaching it requires a
# valid tier/model/effort tuple — a bad one aborts earlier for the wrong reason.
# Parametrize the guard coverage over the engines the issue names as untested
# for fresh dispatch (codex, cursor). claude is covered above; pi cannot take
# --no-grid on standard/deep (it needs the grid for its native-subagent-free
# critic/review phases), and pi's fresh happy path is covered above too.
@test "fresh dispatch aborts on a missing protocol file for codex and cursor" {
  stub_launch_bins
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-worker"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    DISPATCH_PROFILE="$profile" run run_dispatch standard "$model" --agent "$eng" --effort "$effort" --no-grid --crew-id c1 42 "no worker $eng"
    [ "$status" -ne 0 ]
    [[ "$output" == *"WORKER_PROTOCOL.md"* ]]
    [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
    [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
    [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
  done < <(protocol_engine_specs codex cursor)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 2 ]
}

@test "fresh dispatch refuses a stale protocol dir for codex and cursor" {
  stub_launch_bins
  _substituted_dispatch
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-mismatch"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev_dir="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    DISPATCH_PROFILE="$profile" run run_subst_dispatch standard "$model" --agent "$eng" --effort "$effort" --no-grid --crew-id c1 42 "stale $eng"
    [ "$status" -ne 0 ]
    [[ "$output" == *"protocol directory version mismatch"* ]]
    [[ "$output" == *"0123456789abcdef"* ]]
    [[ "$output" == *"$rev_dir"* ]]
    [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
    [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
    [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
  done < <(protocol_engine_specs codex cursor)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 2 ]
}

# The other half of #253's requirement: on the happy path the launch command
# itself carries the correct protocol path. codex and cursor pass it inside the
# prompt string (unlike claude's --append-system-prompt-file and pi's
# --append-system-prompt), so assert that exact carrier, not a symmetric shape.
@test "fresh dispatch names the protocol dir in the codex and cursor launch" {
  stub_launch_bins
  n=0
  while IFS='|' read -r eng model effort profile bin marker; do
    n=$((n + 1))
    DISPATCH_PROFILE="$profile" run run_dispatch standard "$model" --agent "$eng" --effort "$effort" --no-grid --crew-id c1 42 "protocol prompt $eng"
    [ "$status" -eq 0 ]
    _replay_lead_launch "$bin" "$marker"
    last=$((${#argv[@]} - 1))
    [[ "${argv[last]}" == "Read $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end."* ]]
    [[ "${argv[last]}" == *"live in $DISPATCHER_PROTOCOL_DIR"* ]]
  done < <(protocol_engine_specs codex cursor)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 2 ]
}

@test "pi still defaults to the full spec-critic,plan-critic,reviewer grid on deep" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep openrouter/deepseek/deepseek-v4-pro --agent pi --effort high --crew-id c1 42 "pi deep full grid default"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-pi-deep-full-grid-default/WORKER_TASK.md"
  grep -Fx 'roles: spec-critic,plan-critic,reviewer' "$task"
}

@test "single-engine fallback: personal-profile deep claude grids critics-only, entirely on claude, no refusal" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "single engine fallback"
  [ "$status" -eq 0 ]
  [[ "$output" != *"work-profile only"* ]]
  task="$TEST_REPO/.dispatch-wt/feat-42-single-engine-fallback/WORKER_TASK.md"
  grep -Fx 'roles: spec-critic,plan-critic' "$task"
  # No role spec named a foreign agent/model — every pane launches as claude.
  run grep -c -- ' codex ' "$STUB_LOG"
  [ "$status" -ne 0 ]
  run grep -c -- 'cursor-agent' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "kind=review workers get no default grid" {
  stub_pr_bins review-target
  export DISPATCHER_PROTOCOL_DIR="$BATS_TEST_DIRNAME/../adapters/core/protocols"
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --effort high --pr 99 --review --crew-id c1 "review kind no grid"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.worktrees/review-target/WORKER_TASK.md"
  run grep -q '^roles:' "$task"
  [ "$status" -ne 0 ]
}

@test "non-pi deep with --plan provided gets no default grid" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --plan provided --effort high --crew-id c1 42 "plan provided no grid"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-plan-provided-no-grid/WORKER_TASK.md"
  run grep -q '^roles:' "$task"
  [ "$status" -ne 0 ]
}

@test "pi deep with --plan provided still grids (reviewer is its review gate)" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep openrouter/deepseek/deepseek-v4-pro --agent pi --plan provided --effort high --crew-id c1 42 "pi plan provided still grids"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-pi-plan-provided-still-grids/WORKER_TASK.md"
  grep -Fx 'roles: spec-critic,plan-critic,reviewer' "$task"
}

@test "explicit --grid keeps the full topology on non-pi deep regardless of --plan provided" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --plan provided --grid --effort high --crew-id c1 42 "explicit grid full topology"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-explicit-grid-full-topology/WORKER_TASK.md"
  grep -Fx 'roles: spec-critic,plan-critic,reviewer' "$task"
}

@test "--roles reviewer on a claude deep lead adds an additive pane, not a substitute" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --roles reviewer --effort high --crew-id c1 42 "additive reviewer pane"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-additive-reviewer-pane/WORKER_TASK.md"
  grep -Fx 'roles: reviewer' "$task"
}

@test "standard claude has no default grid" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard sonnet --agent claude --effort medium --crew-id c1 42 "standard claude no grid"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-standard-claude-no-grid/WORKER_TASK.md"
  run grep -q '^roles:' "$task"
  [ "$status" -ne 0 ]
}

@test "trivial never grids, on any engine" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch trivial sonnet --agent claude --effort low --crew-id c1 42 "trivial claude no grid"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-trivial-claude-no-grid/WORKER_TASK.md"
  run grep -q '^roles:' "$task"
  [ "$status" -ne 0 ]
}

@test "--no-grid suppresses the default grid for deep claude" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --no-grid --effort high --crew-id c1 42 "no grid deep claude"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-no-grid-deep-claude/WORKER_TASK.md"
  run grep -q '^roles:' "$task"
  [ "$status" -ne 0 ]
}

@test "--no-grid is refused for pi standard and deep" {
  run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --no-grid --effort high --crew-id c1 "no grid pi standard"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--no-grid cannot be used with --agent pi"* ]]

  run run_dispatch deep openrouter/deepseek/deepseek-v4-pro --agent pi --no-grid --effort high --crew-id c1 "no grid pi deep"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--no-grid cannot be used with --agent pi"* ]]
}

@test "--no-grid conflicts with --grid and with --roles" {
  run run_dispatch deep opus --agent claude --no-grid --grid --effort high --crew-id c1 "no grid vs grid"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--no-grid conflicts with --grid/--roles"* ]]

  run run_dispatch deep opus --agent claude --no-grid --roles reviewer --effort high --crew-id c1 "no grid vs roles"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--no-grid conflicts with --grid/--roles"* ]]
}

@test "--roles still overrides the default grid" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch deep opus --agent claude --roles reviewer --effort high --crew-id c1 42 "roles override default"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-roles-override-default/WORKER_TASK.md"
  grep -Fx 'roles: reviewer' "$task"
}

@test "a role spec can pick its own engine and model" {
  run grep -F -- 'role_agent="${rest%%:*}"' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- '--append-system-prompt-file $PROTOCOL_DIR/GRID_PROTOCOL.md' "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "a role spec rejects an empty model for an explicit engine" {
  run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --roles "reviewer=claude:" --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"needs a model after 'claude:'"* ]]
}

@test "a role spec rejects shell syntax in a model" {
  run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --roles 'reviewer=pi:model;touch-bad' --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"invalid model"* ]]
}

@test "role effort suffixes launch independently and persist their resolved values" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol --agent codex --ignore-map --roles "reviewer=claude:sonnet@low,critic@ultra" --effort high --crew-id c1 42 "per-role effort"
  [ "$status" -eq 0 ]
  run grep -F -- 'claude --name iris-reviewer --model sonnet --effort low' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- 'codex --profile worker -m gpt-5.6-sol -c model_reasoning_effort=ultra' "$STUB_LOG"
  [ "$status" -eq 0 ]
  task="$TEST_REPO/.dispatch-wt/feat-42-per-role-effort/WORKER_TASK.md"
  roles="$(sed -n 's/^crew_dir: //p' "$task")/artifacts/feat/42-per-role-effort/roles.json"
  run jq -r '.reviewer.effort + ":" + .critic.effort' "$roles"
  [ "$status" -eq 0 ]
  [ "$output" = 'low:ultra' ]
}

@test "role effort suffix grammar rejects malformed and cursor forms" {
  for roles in 'reviewer@' '@high' 'reviewer@high@low' 'reviewer@high=sonnet'; do
    run run_dispatch standard sonnet --roles "$roles" --effort high --crew-id c1 "bad role effort"
    [ "$status" -eq 1 ]
    [[ "$output" == *"invalid role effort suffix"* || "$output" == *"invalid effort"* ]]
  done

  DISPATCH_PROFILE=work run run_dispatch standard sonnet --roles 'reviewer=cursor:claude-opus-5-high@high' --effort high --crew-id c1 "cursor effort"
  [ "$status" -eq 1 ]
  [[ "$output" == *"bracketed model"* ]]
}

@test "role-local ultra is rejected for claude and pi" {
  for roles in \
    'reviewer=claude:sonnet@ultra' \
    'reviewer=pi:openrouter/deepseek/deepseek-v4-flash@ultra'; do
    DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol --agent codex --ignore-map --roles "$roles" --effort high --crew-id c1 "role ultra"
    [ "$status" -eq 1 ]
    [[ "$output" == *"does not support --effort ultra"* ]]
  done
}

@test "an eager role without an effort suffix inherits the lead effort" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-sol --agent codex --ignore-map --roles 'reviewer=claude:sonnet' --effort max --crew-id c1 42 "inherited role effort"
  [ "$status" -eq 0 ]
  run grep -F -- 'claude --name iris-reviewer --model sonnet --effort max' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "an ultra codex lead may give claude and pi roles non-ultra effort" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch trivial gpt-5.6-sol --agent codex --ignore-map --roles "reviewer=claude:sonnet@max,critic=pi:openrouter/deepseek/deepseek-v4-flash@high" --effort ultra --crew-id c1 42 "ultra lead roles"
  [ "$status" -eq 0 ]
  run grep -F -- 'claude --name iris-reviewer --model sonnet --effort max' "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- '--thinking high' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "rejects an unknown effort" {
  run run_dispatch standard sonnet --effort bogus "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--effort must be low, medium, high, xhigh, max, or ultra"* ]]
}

@test "the roster gate fires before any worktree is scaffolded" {
  DISPATCH_ENGINES="claude pi" run run_dispatch standard gpt-5.6-sol --agent codex --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  # The gate rejects before ANY stubbed binary runs, so $STUB_LOG is never
  # created — `grep -c` on a missing file errors rather than printing 0.
  # Assert the real property instead: no `wt switch` scaffolded a worktree.
  # Non-vacuous: move the gate below worktree creation and `wt switch --create`
  # lands in the log, failing this.
  [ ! -f "$STUB_LOG" ] || ! grep -q 'switch' "$STUB_LOG"
}

@test "DISPATCH_PRECHECK runs every gate and exits before any scaffolding" {
  # Same fixture as the mint-and-claim test below, so the flow it exercises
  # really would reach `gh issue create`, `gh issue edit`, `crew reap` and
  # `tmux new-window` if the precheck exit didn't sit above all of them —
  # every one of those is stubbed here, so a real invocation of any of them
  # lands in $STUB_LOG. Assert the file was never even created: none of
  # `gh`, `wt`, `tmux`, `crew` ran, which is a strict superset of "none of the
  # four specific calls ran" and, unlike a `grep` on a maybe-absent file,
  # can't be defeated by bash's errexit skipping a negated command.
  stub_launch_bins
  stub_gh_claim "" 77
  DISPATCH_PRECHECK=1 run run_dispatch standard sonnet --effort medium --crew-id c1 "mint me"
  [ "$status" -eq 0 ]
  [ ! -f "$STUB_LOG" ]
  [ ! -d "$TEST_REPO/.dispatch-wt" ]
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

# Write a budget cache with one <engine> 7d window at <pct>, with resets_at
# <resets_in_s> seconds from now, or the literal string "null". fetched_epoch
# is always now. Parallel to budget_json()/codex_budget_json() above, not a
# change to either signature — this is the pace-aware sibling the gate-2
# pace tests need, able to place a window anywhere in its span.
budget_json_at() { # <engine> <pct> <resets_in_s|null>
  local engine="$1" pct="$2" resets_in="$3" now resets_arg
  now="$(date +%s)"
  if [ "$resets_in" = null ]; then
    resets_arg=null
  else
    resets_arg=$((now + resets_in))
  fi
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --arg engine "$engine" --argjson pct "$pct" --argjson epoch "$now" --argjson resets "$resets_arg" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: null, cursor: null} + {($engine): {source: "t", windows: {"7d": {used_pct: $pct, resets_at: $resets}}}}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
}

# Write a fresh codex cache whose limit_reached block is the caller's jq
# object literal (unquoted keys are jq object syntax, so it is spliced into
# the jq program, not passed as JSON), with an EMPTY windows map so the
# >=95% gate is inert and only the #201 absolute-limit gate can fire.
# Parallel to codex_budget_json() above.
codex_limit_json() { # <limit_reached jq object literal>
  local lr="$1"
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: {source: "t", windows: {}, limit_reached: '"$lr"'}, cursor: null}}' \
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

@test "the cursor cache rejects a well-shaped slug this account lacks" {
  write_cursor_models_cache "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.5-high --agent cursor --effort medium --ignore-map --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"cursor-grok-4.5-high"* ]]
  [[ "$output" == *"cursor-models-cache.json"* ]]
  [[ "$output" == *"refresh-models"* ]]
  [[ "$output" == *"DISPATCH_SKIP_MODEL_CHECK"* ]]
}

@test "the cursor cache admits a slug it holds" {
  stub_launch_bins
  write_cursor_models_cache "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.6-high --agent cursor --effort medium --ignore-map --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "an absent cursor models cache is a skip, not a hard fail" {
  stub_launch_bins
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.5-high --agent cursor --effort medium --ignore-map --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "a stale cursor models cache degrades to shape-only" {
  stub_launch_bins
  write_cursor_models_cache "$(($(date +%s) - 90000))"
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.5-high --agent cursor --effort medium --ignore-map --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "a malformed cursor models cache never blocks cursor dispatch" {
  stub_launch_bins
  mkdir -p "$XDG_DATA_HOME/crew"
  printf 'not json' >"$XDG_DATA_HOME/crew/cursor-models-cache.json"
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.5-high --agent cursor --effort medium --ignore-map --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "a bracketed id is exempt from cursor cache membership checking" {
  stub_launch_bins
  write_cursor_models_cache "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch deep 'claude-opus-5[context=1m,effort=high,fast=false]' --agent cursor --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
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
assert_gate_silent() { # <engine> <model> [profile]
  DISPATCH_PROFILE="${3:-work}" run run_dispatch standard "$2" --agent "$1" --effort medium --ignore-map --crew-id c1 42 "map row $2"
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
  # alternatives prose, the codex legacy generations, the orchestrator
  # table, and pi's OpenRouter route. Copied, so it makes drift loud rather
  # than impossible.
  for m in opus sonnet haiku claude-fable-5-1; do
    assert_gate_silent claude "$m"
  done
  for m in gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-5.5 gpt-5.4 gpt-5.4-mini; do
    assert_gate_silent codex "$m"
  done
  for m in kimi-k3-high cursor-grok-4.6-high cursor-grok-4.6-medium \
    cursor-grok-4.6-low cursor-grok-4.6-medium-fast cursor-grok-4.6-low-fast \
    grok-4.7-high grok-4.7-medium grok-4.7-low grok-4.7-medium-fast grok-4.7-low-fast \
    composer-2.5 composer-2.5-fast \
    claude-opus-5-high gpt-5.6-sol-high; do
    assert_gate_silent cursor "$m"
  done
  for m in openrouter/deepseek/deepseek-v4-pro openrouter/deepseek/deepseek-v4.1-flash \
    openrouter/deepseek/deepseek-v4-flash; do
    assert_gate_silent pi "$m" work
    assert_gate_silent pi "$m" personal
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
  DISPATCH_PROFILE=work run run_dispatch deep grok-4.7-medium --agent cursor --effort high --crew-id c1 42 "tier cursor deep grok medium"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep grok-4.7-high --agent cursor --effort high --crew-id c1 42 "tier cursor deep grok high"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep composer-2.5 --agent cursor --effort high --crew-id c1 42 "tier cursor deep composer"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch deep 'claude-opus-5[context=1m,effort=high,fast=false]' --agent cursor --effort high --crew-id c1 42 "tier cursor deep bracket opus"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard grok-4.7-medium --agent cursor --effort medium --crew-id c1 42 "tier cursor standard grok medium"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard grok-4.7-low --agent cursor --effort medium --crew-id c1 42 "tier cursor standard grok low"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch standard composer-2.5 --agent cursor --effort medium --crew-id c1 42 "tier cursor standard composer"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial grok-4.7-low --agent cursor --effort low --crew-id c1 42 "tier cursor trivial grok low"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work run run_dispatch trivial composer-2.5 --agent cursor --effort low --crew-id c1 42 "tier cursor trivial composer"
  [ "$status" -eq 0 ]
  # 4.6 keeps its old cursor- prefix and stays dispatchable after the move to 4.7.
  DISPATCH_PROFILE=work run run_dispatch standard cursor-grok-4.6-medium --agent cursor --effort medium --crew-id c1 42 "tier cursor standard grok 4.6"
  [ "$status" -eq 0 ]
}

@test "every profile refuses the retired opencode route on pi" {
  for p in work personal; do
    DISPATCH_PROFILE=$p run run_dispatch deep opencode/deepseek-v4-pro --agent pi --effort high --crew-id c1 42 "tier pi deep opencode rejected on $p"
    [ "$status" -eq 1 ]
    [[ "$output" == *"is not deep's row"* ]]
    [[ "$output" == *"--ignore-map"* ]]

    DISPATCH_PROFILE=$p run run_dispatch standard opencode/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "tier pi standard opencode rejected on $p"
    [ "$status" -eq 1 ]
    [[ "$output" == *"is not standard's row"* ]]
  done
}

@test "tier gate accepts every pi table cell on both profiles" {
  stub_launch_bins
  for p in work personal; do
    DISPATCH_PROFILE=$p run run_dispatch deep openrouter/deepseek/deepseek-v4-pro --agent pi --effort high --crew-id c1 42 "tier pi deep pro $p"
    [ "$status" -eq 0 ]
    DISPATCH_PROFILE=$p run run_dispatch deep openrouter/deepseek/deepseek-v4.1-flash --agent pi --effort high --crew-id c1 42 "tier pi deep v41 flash $p"
    [ "$status" -eq 0 ]
    DISPATCH_PROFILE=$p run run_dispatch standard openrouter/deepseek/deepseek-v4.1-flash --agent pi --effort high --crew-id c1 42 "tier pi standard v41 flash $p"
    [ "$status" -eq 0 ]
    DISPATCH_PROFILE=$p run run_dispatch standard openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "tier pi standard flash $p"
    [ "$status" -eq 0 ]
    DISPATCH_PROFILE=$p run run_dispatch trivial openrouter/deepseek/deepseek-v4-flash --agent pi --effort high --crew-id c1 42 "tier pi trivial flash $p"
    [ "$status" -eq 0 ]
  done
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
  DISPATCH_PROFILE=work run run_dispatch deep 'grok-4.7-high[effort=high]' --agent cursor --effort high --crew-id c1 42 "rung bracket cursor"
  [ "$status" -eq 1 ]
  [[ "$output" == *"grok-4.7-medium"* ]]
}

@test "budget rung gate also matches the bare premium cursor id" {
  # Sibling of the bracketed-id test above, same fixture — but the bare form
  # also has to clear the cache-membership check ahead of this gate, so the
  # cache here lists it: proves the rung gate itself rejects it, not the
  # cache-membership check.
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: null, cursor: {source: "t", windows: {"7d": {used_pct: 80, resets_at: null}}}}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  write_cursor_models_cache "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch deep grok-4.7-high --agent cursor --effort high --crew-id c1 42 "rung bare cursor"
  [ "$status" -eq 1 ]
  [[ "$output" == *"grok-4.7-medium"* ]]
  [[ "$output" == *"the premium rung"* ]]
}

@test "budget rung gate still matches the legacy prefixed 4.6 cursor id" {
  # Sibling of the two tests above, same fixture — but the legacy prefixed
  # form also has to clear the cache-membership check ahead of this gate, so
  # the cache here lists it: proves the rung gate itself rejects it, not the
  # cache-membership check.
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: null, cursor: {source: "t", windows: {"7d": {used_pct: 80, resets_at: null}}}}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  write_cursor_models_cache "$(date +%s)"
  DISPATCH_PROFILE=work run run_dispatch deep cursor-grok-4.6-high --agent cursor --effort high --crew-id c1 42 "rung legacy prefixed cursor"
  [ "$status" -eq 1 ]
  [[ "$output" == *"cursor-grok-4.6-medium"* ]]
  [[ "$output" == *"the premium rung"* ]]
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
    kimi-k3-high grok-4.7-high grok-4.7-medium grok-4.7-low \
    composer-2.5 claude-fable-5-1 \
    openrouter/deepseek/deepseek-v4-pro openrouter/deepseek/deepseek-v4.1-flash \
    openrouter/deepseek/deepseek-v4-flash; do
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

@test "codex absolute limit refuses on a named rate-limit-reached type" {
  codex_limit_json '{rate_limit_reached_type: "workspace_owner_credits_depleted"}'
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "abs rate limit reached"
  [ "$status" -eq 1 ]
  [[ "$output" == *"quota exhausted (absolute limit: workspace_owner_credits_depleted)"* ]]
  [[ "$output" == *"--ignore-budget"* ]]
}

@test "codex absolute limit refuses on a zeroed individual spend limit" {
  codex_limit_json '{individual_remaining_percent: 0}'
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "abs individual drained"
  [ "$status" -eq 1 ]
  [[ "$output" == *"quota exhausted (absolute limit: spend control: 0% remaining)"* ]]
  [[ "$output" == *"--ignore-budget"* ]]
}

@test "codex absolute limit refuses on spend control reached" {
  codex_limit_json '{spend_control_reached: true}'
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "abs spend control"
  [ "$status" -eq 1 ]
  [[ "$output" == *"quota exhausted (absolute limit: spend control reached)"* ]]
  [[ "$output" == *"--ignore-budget"* ]]
}

@test "codex absolute limit refuses when ordinary use is denied" {
  codex_limit_json '{ordinary_usage_allowed: false}'
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "abs ordinary denied"
  [ "$status" -eq 1 ]
  [[ "$output" == *"quota exhausted (absolute limit: ordinary use not allowed)"* ]]
  [[ "$output" == *"--ignore-budget"* ]]
}

@test "codex absolute-limit gate passes when the signals are healthy" {
  stub_launch_bins
  codex_limit_json '{ordinary_usage_allowed: true, rate_limit_reached_type: null, spend_control_reached: null, individual_remaining_percent: 100}'
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "abs healthy"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
  [[ "$output" != *"quota exhausted"* ]]
}

@test "a legacy codex cache without limit_reached still passes" {
  stub_launch_bins
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(date +%s)" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: {source: "t", windows: {"7d": {used_pct: 30, resets_at: null}}}, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort medium --crew-id c1 42 "legacy codex cache"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
  [[ "$output" != *"quota exhausted"* ]]
}

@test "--ignore-budget bypasses the codex absolute-limit gate" {
  stub_launch_bins
  codex_limit_json '{ordinary_usage_allowed: false}'
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --ignore-budget --crew-id c1 42 "abs ignore budget"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "a stale codex cache with an absolute limit fails open" {
  stub_launch_bins
  mkdir -p "$XDG_DATA_HOME/crew"
  jq -n --argjson epoch "$(($(date +%s) - 10000))" \
    '{fetched_epoch: $epoch, engines: {claude: null, codex: {source: "t", windows: {}, limit_reached: {ordinary_usage_allowed: false}}, cursor: null}}' \
    >"$XDG_DATA_HOME/crew/engine-budget.json"
  DISPATCH_PROFILE=work run run_dispatch deep gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "stale abs limit"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
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

@test "budget rung gate refuses when 7d burn is ahead of pace" {
  # 77% used, 4 days left on the 7-day window: 43% elapsed, +34 ahead —
  # #113's live case, and the pace rule's own worked example.
  budget_json_at claude 77 345600
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "pace ahead refuses"
  [ "$status" -eq 1 ]
  [[ "$output" == *"sonnet"* ]]
  [[ "$output" == *"34 points ahead of pace"* ]]
}

@test "pace gate refuses premium effort, but allows high and --ignore-budget" {
  stub_launch_bins
  budget_json_at claude 77 345600
  run run_dispatch deep sonnet --effort xhigh --crew-id c1 42 "effort refuses"
  [ "$status" -eq 1 ]
  [[ "$output" == *"premium effort (xhigh)"* ]]
  [[ "$output" == *"use high instead"* ]]

  budget_json_at claude 77 345600
  run run_dispatch deep sonnet --effort high --crew-id c1 42 "high allows"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"

  budget_json_at claude 77 345600
  run run_dispatch deep sonnet --effort xhigh --ignore-budget --crew-id c1 42 "ignore effort"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "pace gate effort escape is exact and null reset still refuses" {
  stub_launch_bins
  budget_json_at claude 77 345600
  DISPATCH_IGNORE_RUNG=xhigh run run_dispatch deep sonnet --effort xhigh --crew-id c1 42 "exact effort"
  [ "$status" -eq 0 ]
  [[ "$output" == *"effort refusal skipped"* ]]

  budget_json_at claude 77 345600
  DISPATCH_IGNORE_RUNG=max run run_dispatch deep sonnet --effort xhigh --crew-id c1 42 "mismatched effort"
  [ "$status" -eq 1 ]
  [[ "$output" == *"premium effort"* ]]

  budget_json_at claude 90 null
  run run_dispatch deep sonnet --effort max --crew-id c1 42 "null effort"
  [ "$status" -eq 1 ]
  [[ "$output" == *"premium effort (max)"* ]]
  [[ "$output" != *"ahead of pace"* ]]
}

@test "pace gate checks explicit and inherited eager role effort before panes" {
  budget_json_at claude 77 345600
  run run_dispatch deep sonnet --roles 'reviewer@xhigh' --effort high --crew-id c1 42 "explicit role effort"
  [ "$status" -eq 1 ]
  [[ "$output" == *"premium effort (xhigh)"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -qE 'split-window|send-keys' "$STUB_LOG"

  budget_json_at claude 77 345600
  run run_dispatch deep sonnet --roles reviewer --effort max --crew-id c1 42 "inherited role effort"
  [ "$status" -eq 1 ]
  [[ "$output" == *"premium effort (max)"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -qE 'split-window|send-keys' "$STUB_LOG"
}

@test "budget rung gate allows 7d burn that is at or behind pace" {
  stub_launch_bins
  budget_json_at claude 77 86400
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "at pace allows"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "budget rung gate allows a high 7d window near its reset" {
  stub_launch_bins
  budget_json_at claude 94 7200
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "near reset allows"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "budget rung gate allows below the 70% floor regardless of pace" {
  stub_launch_bins
  budget_json_at claude 69 432000
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "below floor allows"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "budget rung gate falls back to the flat rule when resets_at is null" {
  budget_json_at claude 90 null
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "flat fallback refuses"
  [ "$status" -eq 1 ]
  [[ "$output" == *"sonnet"* ]]
  [[ "$output" != *"ahead of pace"* ]]
}

@test "budget rung gate allows a resets_at already in the past" {
  stub_launch_bins
  budget_json_at claude 90 -3600
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "past reset allows"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "DISPATCH_IGNORE_RUNG bypasses the rung gate for the exact model" {
  stub_launch_bins
  budget_json_at claude 77 345600
  DISPATCH_IGNORE_RUNG=opus run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "ignore rung bypass"
  [ "$status" -eq 0 ]
  [[ "$output" == *"rung refusal skipped (DISPATCH_IGNORE_RUNG)"* ]]
  [[ "$output" == *"'opus' on --agent claude at 7d 77%"* ]]
  [[ "$output" == *"34 ahead of pace"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "DISPATCH_IGNORE_RUNG set to a different model does not bypass" {
  budget_json_at claude 77 345600
  DISPATCH_IGNORE_RUNG=sonnet run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "ignore rung mismatch"
  [ "$status" -eq 1 ]
  [[ "$output" == *"sonnet"* ]]
  [[ "$output" != *"rung refusal skipped"* ]]
}

@test "DISPATCH_IGNORE_RUNG does not bypass the 95% exhaustion gate" {
  budget_json_at claude 97 345600
  DISPATCH_IGNORE_RUNG=opus run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "ignore rung vs exhaustion"
  [ "$status" -eq 1 ]
  [[ "$output" == *"quota exhausted"* ]]
}

@test "the rung refusal message names DISPATCH_IGNORE_RUNG" {
  budget_json_at claude 77 345600
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "message names ignore rung"
  [ "$status" -eq 1 ]
  [[ "$output" == *"DISPATCH_IGNORE_RUNG=opus"* ]]
  [[ "$output" == *"--ignore-budget"* ]]
}

@test "the pace clause is present on the pace path and absent on the flat path" {
  budget_json_at claude 77 345600
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "pace clause present"
  [ "$status" -eq 1 ]
  [[ "$output" == *"points ahead of pace"* ]]

  budget_json_at claude 90 null
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "pace clause absent"
  [ "$status" -eq 1 ]
  [[ "$output" != *"points ahead of pace"* ]]
}

@test "pace never disarms the 95% hard stop" {
  # A near-future resets_at that would put the rung gate at or behind pace —
  # the hard stop still fires first, untouched by pace.
  budget_json_at claude 97 7200
  run run_dispatch deep opus --agent claude --effort high --crew-id c1 42 "pace vs hard stop"
  [ "$status" -eq 1 ]
  [[ "$output" == *"quota exhausted"* ]]
  [[ "$output" != *"the premium rung"* ]]
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
  run ! grep -q 'new-window' "$STUB_LOG"
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

@test "dispatch event carries plan" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run_dispatch standard sonnet --effort medium --crew-id c1 42 "implement thing"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="dispatch") | .plan' "$log"
  [ "$output" = "required" ]
}

@test "dispatch event carries plan: provided when --plan provided is passed" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run_dispatch standard sonnet --effort medium --plan provided --crew-id c1 42 "implement thing"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -r 'select(.kind=="dispatch") | .plan' "$log"
  [ "$output" = "provided" ]
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
  run ! grep -q 'new-window' "$STUB_LOG"
  run ! grep -q 'send-keys' "$STUB_LOG"
  run ! grep -q 'kill-window' "$STUB_LOG"
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
  run ! grep -q 'kill-window' "$STUB_LOG"
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
  [[ "$output" == *"no engine pane detected"* ]]
  run ! grep -q 'kill-window' "$STUB_LOG"
  run ! grep -q 'new-window' "$STUB_LOG"
}

@test "gate: an unoccupied existing worktree dispatches normally" {
  setup_occupied_branch
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  run ! grep -q 'kill-window' "$STUB_LOG"
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
  run ! grep -q 'new-window' "$STUB_LOG"
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
  run ! grep -q 'new-window' "$STUB_LOG"
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
  run ! grep -q 'new-window' "$STUB_LOG"
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
  run ! grep -q 'switch' "$STUB_LOG"
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
  run ! grep -q '^reap' "$STUB_LOG"
  run ! grep -q 'new-window' "$STUB_LOG"
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
  run ! grep -q '^reap' "$STUB_LOG"
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
  run ! grep -q '^issue' "$STUB_LOG"
  ! grep -q '^label' "$STUB_LOG"
}

@test "claim: a --pr dispatch never touches gh issue/label calls" {
  stub_pr_bins eng-7691-foo
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Fix it"
  [ "$status" -eq 0 ]
  run ! grep -q '^issue' "$STUB_LOG"
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

# commit_envrc — track a real .envrc on the source branch so a worktree cut
# from it (via `wt`'s `git worktree add -b ... HEAD`) actually has one; the
# direnv-allow guard now skips entirely when .envrc is absent, so tests that
# mean to exercise `direnv allow` itself need this.
commit_envrc() {
  echo 'use nix' >"$TEST_REPO/.envrc"
  git -C "$TEST_REPO" add .envrc
  git -C "$TEST_REPO" commit -q -m envrc
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
  commit_envrc
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  wt="$(wt_path_for feat/42-title)"
  grep -qx "allow $wt" "$STUB_LOG"
}

@test "direnv: allow is called for codex and cursor dispatches too" {
  stub_launch_bins
  commit_envrc
  DISPATCH_PROFILE=work run run_dispatch standard gpt-5.6-terra --agent codex --effort high --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  wt="$(wt_path_for feat/42-title)"
  grep -qx "allow $wt" "$STUB_LOG"
}

@test "direnv: allow failure aborts the dispatch, never launches" {
  stub_launch_bins
  commit_envrc
  cat >"$STUB_DIR/direnv" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 1
EOF
  chmod +x "$STUB_DIR/direnv"
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"direnv allow failed"* ]]
  run ! grep -q 'new-window' "$STUB_LOG"
  ! grep -q 'send-keys' "$STUB_LOG"
}

@test "direnv: no .envrc in the worktree skips direnv allow entirely and still launches" {
  stub_launch_bins
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "title"
  [ "$status" -eq 0 ]
  ! grep -q '^allow ' "$STUB_LOG"
  grep -q 'send-keys' "$STUB_LOG"
}

@test "direnv: a --pr dispatch never auto-approves, warns instead, and still launches" {
  stub_pr_bins eng-7691-foo
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --pr 99 --crew-id c1 "Review PR 99"
  [ "$status" -eq 0 ]
  [[ "$output" == *"not auto-approving direnv"* ]]
  run ! grep -q '^allow ' "$STUB_LOG"
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
  run ! grep -q 'switch -c' "$STUB_LOG"
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
  run ! grep -q 'switch -c' "$STUB_LOG"
  run ! grep -q '^occupants' "$STUB_LOG"
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
  run ! grep -q 'repo view' "$STUB_LOG"
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
  run ! grep -q '^resume:' "$TEST_REPO/.dispatch-wt/feat-42-do-a-thing/WORKER_TASK.md"
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
  run ! grep -q 'add-label' "$STUB_LOG"
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
  run ! grep -q 'add-label' "$STUB_LOG"
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
  run ! grep -q '^stale_header:' "$task"
  [ "$(grep -cFx '## Task' "$task")" -eq 1 ]
}

# resume:false on the create-mode row, resume:true on the row for the
# re-dispatch that resumes onto the branch the first row created.
@test "resume: dispatch event carries resume:false on create and resume:true on re-dispatch" {
  stub_launch_bins
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "implement thing"
  [ "$status" -eq 0 ]
  cat >"$STUB_DIR/wt" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/wt"
  stub_crew_gate '[]' '[]'
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "implement thing"
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -s -r '[.[] | select(.kind=="dispatch")] | sort_by(.ts) | .[0].resume' "$log"
  [ "$output" = "false" ]
  run jq -s -r '[.[] | select(.kind=="dispatch")] | sort_by(.ts) | .[1].resume' "$log"
  [ "$output" = "true" ]
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
  run ! grep -q '^switch' "$STUB_LOG"
  ! grep -q 'new-window' "$STUB_LOG"
}

@test "resume: refuses when the worktree is the directory dispatch runs from" {
  setup_resume_branch feat/42-do-a-thing
  cd "$TEST_REPO/.dispatch-wt/feat-42-do-a-thing"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium 42 --crew-id c1 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"the worktree this dispatch is running from"* ]]
  run ! grep -q '^switch' "$STUB_LOG"
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
  run ! grep -q '^switch' "$STUB_LOG"
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
  run ! grep -q 'kill-window' "$STUB_LOG"
  run ! grep -q '^switch' "$STUB_LOG"
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

@test "stamps the mcp profile in the task header" {
  stub_launch_bins
  export DISPATCH_PROFILE=work
  mkdir -p "$HOME/.config/claude-code"
  printf '{}' >"$HOME/.config/claude-code/mcp-posthog.json"
  run run_dispatch standard sonnet --effort medium --mcp analytics --crew-id c1 42 "add a flag"
  [ "$status" -eq 0 ]
  doc="$(find "$TEST_REPO/.dispatch-wt" -name WORKER_TASK.md | head -1)"
  grep -qx 'mcp: analytics' "$doc"
}

@test "stamps an empty mcp line when no profile was given" {
  stub_launch_bins
  run run_dispatch standard sonnet --effort medium --crew-id c1 42 "add a flag"
  [ "$status" -eq 0 ]
  doc="$(find "$TEST_REPO/.dispatch-wt" -name WORKER_TASK.md | head -1)"
  grep -qE '^mcp: ?$' "$doc"
}

@test "grid: role panes are decorated with a stable per-role colour and label" {
  run grep -F -- 'role_color' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'decorate_pane' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'pane-active-border-style' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- '#{@crew_state}' "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "grid: known roles map to fixed colours" {
  run grep -F -- "plan-critic) printf 'colour111'" "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- "reviewer) printf 'colour114'" "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "grid: role panes spawn the engine-agnostic bus watcher" {
  run grep -F -- '--role-watch' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'watch_role' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'send-keys -t "$watch_pane" -l' "$DISPATCH"
  [ "$status" -eq 0 ]
  # The mechanism is the tmux watcher, not an engine extension.
  run grep -c 'crew-bus.ts' "$DISPATCH"
  [ "$output" = "0" ]
}

@test "grid: --role-watch needs a role name and a pane" {
  run run_dispatch --role-watch
  [ "$status" -eq 1 ]
  [[ "$output" == *"--role-watch needs a role name"* ]]
  run run_dispatch --role-watch reviewer
  [ "$status" -eq 1 ]
  [[ "$output" == *"--role-watch needs --pane"* ]]
}

@test "grid: --lazy needs a role topology" {
  run run_dispatch standard sonnet --lazy --effort high --crew-id c1 "title"
  [ "$status" -eq 1 ]
  [[ "$output" == *"--lazy needs --grid or --roles"* ]]
}

@test "grid: lazy records role specs and spawns on demand" {
  run grep -F -- 'roles.json' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- '--spawn-role' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- '--reap-roles' "$DISPATCH"
  [ "$status" -eq 0 ]
  run grep -F -- 'lazy: 1' "$DISPATCH"
  [ "$status" -eq 0 ]
}

@test "grid: --spawn-role needs a role name and a worker worktree" {
  run run_dispatch --spawn-role
  [ "$status" -eq 1 ]
  [[ "$output" == *"--spawn-role needs a role name"* ]]
  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 1 ]
  [[ "$output" == *"must run inside a worker worktree"* ]]
}

# _spawn_role_fixture — TEST_REPO as a worker worktree with a recorded pi
# reviewer role, ready for `dispatch --spawn-role reviewer`.
_spawn_role_fixture() {
  git switch -q -c feat/9-x
  git commit -q --allow-empty -m init
  printf 'agent_name: iris\neffort: high\nworker_id: worker:feat/9-x#s1-1\ncrew_id: c1\n' >WORKER_TASK.md
  export TMUX_PANE=%5
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  roles_dir="$common/crew/artifacts/feat/9-x"
  mkdir -p "$roles_dir"
  printf '{"reviewer":{"agent":"pi","model":"openrouter/deepseek/deepseek-v4-flash"}}\n' >"$roles_dir/roles.json"

  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' '@1' ;;
list-panes) ;;
split-window) printf '%s\n' '%6' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

@test "grid: --spawn-role seeds the worker agent dir before launching a pi role" {
  _spawn_role_fixture
  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 0 ]
  run grep -F -- "PI_CODING_AGENT_DIR=$HOME/.pi/dispatcher-worker pi --name iris-reviewer" "$STUB_LOG"
  [ "$status" -eq 0 ]
  run grep -F -- '--no-approve' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "grid: --spawn-role gives the role pane the lead's CREW_WORKER_ID and CREW_ID" {
  _spawn_role_fixture
  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 0 ]
  line="$(grep '^split-window' "$STUB_LOG")"
  [ "$(_env_of CREW_WORKER_ID "$line")" = "worker:feat/9-x#s1-1" ]
  [ "$(_env_of CREW_ID "$line")" = c1 ]
  [ "$(_env_of CREW_ROLE_ID "$line")" = "role:feat/9-x:reviewer" ]
}

@test "grid: --spawn-role prefers the lead's own environment over the task doc" {
  _spawn_role_fixture
  CREW_WORKER_ID='worker:feat/9-x#s2-2' CREW_ID=c2 run run_dispatch --spawn-role reviewer
  [ "$status" -eq 0 ]
  line="$(grep '^split-window' "$STUB_LOG")"
  [ "$(_env_of CREW_WORKER_ID "$line")" = "worker:feat/9-x#s2-2" ]
  [ "$(_env_of CREW_ID "$line")" = c2 ]
}

@test "grid: --spawn-role refuses to split a pane that would have no worker identity" {
  _spawn_role_fixture
  printf 'agent_name: iris\neffort: high\n' >WORKER_TASK.md
  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 1 ]
  [[ "$output" == *"no worker_id/crew_id"* ]]
  run ! grep -q 'split-window' "$STUB_LOG"
}

@test "grid: --spawn-role respawns a role whose pane exited, but not a live one" {
  _spawn_role_fixture
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' '@1' ;;
list-panes) printf '%s\n' "%5||live" "%4|reviewer|${STUB_PANE_STATE}" ;;
split-window) printf '%s\n' '%6' ;;
esac
exit 0
EOF
  STUB_PANE_STATE=idle run run_dispatch --spawn-role reviewer
  [ "$status" -eq 0 ]
  [[ "$output" == *"already running in pane %4"* ]]
  run ! grep -q '^split-window' "$STUB_LOG"

  STUB_PANE_STATE=exited run run_dispatch --spawn-role reviewer
  [ "$status" -eq 0 ]
  [[ "$output" == *"spawned role reviewer"* ]]
  grep -q '^split-window' "$STUB_LOG"
}

# _exit_hook_fixture — run an eager reviewer role launch, then take the line
# typed into its pane and make it runnable: dispatch gets the shebang the Nix
# build prepends (the typed continuation execs it directly), and the engine
# stub exits at once.
_exit_hook_fixture() {
  stub_launch_bins
  _grid_tmux_stub
  DISPATCH_SESSION_ID=s7-7 DISPATCH_PROFILE=personal run run_dispatch \
    standard sonnet --agent claude --roles reviewer --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]
  wt_path="$TEST_REPO/.dispatch-wt/feat-42-do-a-thing"
  { printf '#!/usr/bin/env bash\nset -euo pipefail\n'; cat "$DISPATCH"; } >"$BATS_TEST_TMPDIR/dispatch-exec"
  chmod +x "$BATS_TEST_TMPDIR/dispatch-exec"
  role_line="$(grep '^send-keys -t %6 claude' "$STUB_LOG")"
  [ -n "$role_line" ]
  cmd="${role_line#send-keys -t %6 }"
  cmd="${cmd% Enter}"
  cmd="${cmd//"$(realpath "$DISPATCH")"/$BATS_TEST_TMPDIR/dispatch-exec}"
  : >"$STUB_LOG"
  printf '#!/usr/bin/env bash\nexit 1\n' >"$STUB_DIR/claude"
  chmod +x "$STUB_DIR/claude"
}

@test "grid: a role pane whose engine exits immediately posts a blocked status naming the role" {
  _exit_hook_fixture
  cd "$wt_path"
  run bash -c "$cmd"
  [ "$status" -eq 0 ]
  grep -qxF 'status role:feat/42-do-a-thing:reviewer blocked role reviewer engine exited (pane %6)' "$STUB_LOG"
  grep -qF 'msg role:feat/42-do-a-thing:reviewer worker:feat/42-do-a-thing#s7-7' "$STUB_LOG"
  grep -qF 'set-option -p -t %6 @crew_exited 1' "$STUB_LOG"
}

@test "grid: --role-exited stays silent after the lead's final release" {
  _exit_hook_fixture
  cd "$wt_path"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:9999999999999,crew_id:"c1",from:"worker:feat/42-do-a-thing#s7-7",to:"role:feat/42-do-a-thing:reviewer",kind:"msg",body:"{\"final\":true}"}' >>"$log"
  jq -nc '{ts:9999999999999,crew_id:"c1",from:"x",to:"role:feat/42-do-a-thing:reviewer",kind:"msg",body:"[1]"}' >>"$log"
  run bash -c "$cmd"
  [ "$status" -eq 0 ]
  run ! grep -qE '^(status|msg) ' "$STUB_LOG"
  grep -qF '@crew_exited 1' "$STUB_LOG"
}

@test "grid: --role-exited is not silenced by a final sent to an earlier incarnation of the role" {
  _exit_hook_fixture
  cd "$wt_path"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$log")"
  jq -nc '{ts:1,crew_id:"c1",from:"worker:feat/42-do-a-thing#s7-7",to:"role:feat/42-do-a-thing:reviewer",kind:"msg",body:"{\"final\":true}"}' >>"$log"
  run bash -c "$cmd"
  [ "$status" -eq 0 ]
  grep -qF 'status role:feat/42-do-a-thing:reviewer blocked' "$STUB_LOG"
}

@test "grid: --role-exited still posts when WORKER_TASK.md is gone" {
  _exit_hook_fixture
  cd "$wt_path"
  rm WORKER_TASK.md
  run bash -c "$cmd"
  [ "$status" -eq 0 ]
  grep -qF 'status role:feat/42-do-a-thing:reviewer blocked' "$STUB_LOG"
  run ! grep -qF 'msg role:' "$STUB_LOG"
}

@test "grid: --reap-roles itself kills role panes and posts nothing" {
  _spawn_role_fixture
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message) printf '%s\n' '@1' ;;
list-panes) printf '%s\n' '%5 ' '%6 reviewer' ;;
esac
exit 0
EOF
  run run_dispatch --reap-roles
  [ "$status" -eq 0 ]
  grep -qF 'kill-pane -t %6' "$STUB_LOG"
  run ! grep -qE 'role-exited|^(status|msg) ' "$STUB_LOG"
}

@test "grid: a real reap kills the pane before the typed continuation can run" {
  command -v tmux >/dev/null || skip "tmux not installed"
  sock="rp$$"
  marker="$BATS_TEST_TMPDIR/hook-ran"
  tmux -L "$sock" -f /dev/null new-session -d -s t -x 80 -y 24 "bash -c 'sleep 30 ; touch $marker'"
  pane="$(tmux -L "$sock" list-panes -t t -F '#{pane_id}')"
  tmux -L "$sock" kill-pane -t "$pane" || true
  sleep 1
  tmux -L "$sock" kill-server 2>/dev/null || true
  [ ! -e "$marker" ]
}

@test "grid: --role-watch stops once its pane is marked exited and types nothing" {
  _spawn_role_fixture
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
display-message)
  case "$*" in
  *'#{@crew_exited}'*) printf '%s\n' 1 ;;
  *) printf '%s\n' '%6' ;;
  esac
  ;;
esac
exit 0
EOF
  run timeout 10 bash -euo pipefail "$DISPATCH" --role-watch reviewer --pane %6 --branch feat/9-x --interval 1
  [ "$status" -eq 0 ]
  run ! grep -qE 'send-keys|set-option -p -t %6 @crew_state (idle|working)' "$STUB_LOG"
}

@test "grid: --spawn-role uses persisted effort, CLI override, and legacy task fallback" {
  _spawn_role_fixture
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  roles="$common/crew/artifacts/feat/9-x/roles.json"
  printf '{"reviewer":{"agent":"pi","model":"openrouter/deepseek/deepseek-v4-flash","effort":"low"},"critic":{"agent":"pi","model":"openrouter/deepseek/deepseek-v4-flash","effort":"low"},"legacy":{"agent":"pi","model":"openrouter/deepseek/deepseek-v4-flash"}}\n' >"$roles"
  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 0 ]
  run grep -F -- '--thinking low' "$STUB_LOG"
  [ "$status" -eq 0 ]

  run run_dispatch --spawn-role critic --effort max
  [ "$status" -eq 0 ]
  run grep -F -- '--thinking max' "$STUB_LOG"
  [ "$status" -eq 0 ]

  run run_dispatch --spawn-role legacy
  [ "$status" -eq 0 ]
  run grep -F -- '--thinking high' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "grid: lazy pace gate checks final effort before splitting and honors --ignore-budget" {
  _spawn_role_fixture
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  roles="$common/crew/artifacts/feat/9-x/roles.json"
  printf '{"reviewer":{"agent":"claude","model":"sonnet","effort":"high"}}\n' >"$roles"
  budget_json_at claude 77 345600

  run run_dispatch --spawn-role reviewer --effort xhigh
  [ "$status" -eq 1 ]
  [[ "$output" == *"premium effort (xhigh)"* ]]
  run grep -c -- 'split-window' "$STUB_LOG"
  [ "$output" -eq 0 ]

  run run_dispatch --spawn-role reviewer --effort xhigh --ignore-budget
  [ "$status" -eq 0 ]
  run grep -q -- 'split-window' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "grid: --spawn-role validates final agent, model, and effort overrides" {
  _spawn_role_fixture
  run run_dispatch --spawn-role reviewer --agent nope
  [ "$status" -eq 1 ]
  [[ "$output" == *"invalid agent"* ]]

  run run_dispatch --spawn-role reviewer --agent claude --model gpt-5.6-sol
  [ "$status" -eq 1 ]
  [[ "$output" == *"invalid model"* ]]

  run run_dispatch --spawn-role reviewer --effort nope
  [ "$status" -eq 1 ]
  [[ "$output" == *"invalid effort"* ]]
}

@test "grid: --spawn-role rejects an engine outside the roster before splitting" {
  _spawn_role_fixture
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  roles="$common/crew/artifacts/feat/9-x/roles.json"
  printf '{"reviewer":{"agent":"codex","model":"gpt-5.6-terra","effort":"medium"}}\n' >"$roles"

  DISPATCH_ENGINES="claude pi" run run_dispatch --spawn-role reviewer
  [ "$status" -eq 1 ]
  [[ "$output" == *"role 'reviewer' uses --agent codex is not enabled here"* ]]
  [ ! -e "$STUB_LOG" ]
}

@test "grid: --spawn-role rejects an explicit effort for the final cursor agent" {
  _spawn_role_fixture
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  roles="$common/crew/artifacts/feat/9-x/roles.json"
  printf '{"reviewer":{"agent":"cursor","model":"composer-2.5","effort":"low"}}\n' >"$roles"

  run run_dispatch --spawn-role reviewer --effort high
  [ "$status" -eq 1 ]
  [[ "$output" == *"uses --agent cursor, which has no --effort"* ]]
  [[ "$output" == *"bracketed model"* ]]
}

@test "grid: --spawn-role aborts a pi role before split-window when the agent dir cannot be seeded" {
  _spawn_role_fixture
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/crew"

  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not seed the pi worker agent dir"* ]]
  run grep -c -- 'split-window' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "grid: --spawn-role refuses a stale protocol dir whose content differs from the script marker" {
  _spawn_role_fixture
  _substituted_dispatch
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-rev"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md" "$DISPATCHER_PROTOCOL_DIR/GRID_PROTOCOL.md"
  run run_subst_dispatch --spawn-role reviewer
  [ "$status" -eq 1 ]
  [[ "$output" == *"protocol directory version mismatch"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  run grep -c -- 'split-window' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "grid: --spawn-role aborts before split-window when GRID_PROTOCOL.md is missing" {
  _spawn_role_fixture
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-grid"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  run run_dispatch --spawn-role reviewer
  [ "$status" -eq 1 ]
  [[ "$output" == *"GRID_PROTOCOL.md"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  run grep -c -- 'split-window' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "the suite resolves engine CLIs from the stub dir, not the developer's machine" {
  # Without this, a PATH probe in dispatch.sh passes locally (real engines
  # installed) and fails on a bare CI runner. Pin the dependency here.
  for cli in claude codex cursor-agent pi; do
    run command -v "$cli"
    [ "$status" -eq 0 ]
    [[ "$output" == "$STUB_DIR/$cli" ]]
  done
}

@test "identity: two branches on one hash slot dispatched into one crew get different names" {
  stub_launch_bins
  cat >"$STUB_DIR/crew" <<'EOF2'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
identity) exec bash -euo pipefail "$CREW_REAL" "$@" ;;
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
EOF2
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 33 "thing"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 38 "thing"
  [ "$status" -eq 0 ]
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  run jq -s -r '[.[] | select(.kind=="dispatch")] | [.[].name] | unique | length' "$log"
  [ "$output" = "2" ]
  run jq -s -r '[.[] | select(.kind=="dispatch")] | [.[].tmux] | unique | length' "$log"
  [ "$output" = "2" ]
  n2="$(jq -s -r '[.[] | select(.kind=="dispatch")] | .[1].tmux' "$log")"
  grep -q "@crew_color $n2" "$STUB_LOG"
}

@test "identity: re-dispatching a branch keeps its recorded name" {
  stub_launch_bins
  cat >"$STUB_DIR/crew" <<'EOF2'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
identity) exec bash -euo pipefail "$CREW_REAL" "$@" ;;
pi-agent-dir) exec bash -euo pipefail "$CREW_REAL" pi-agent-dir ;;
esac
exit 0
EOF2
  crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  mkdir -p "$crew_dir"
  printf '%s\n' '{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/42-thing","name":"cobalt","color":"royalblue","tmux":"colour68"}' >"$crew_dir/events.jsonl"
  DISPATCH_PROFILE=personal run run_dispatch standard sonnet --effort medium --crew-id c1 42 "thing"
  [ "$status" -eq 0 ]
  grep -q 'set-window-option -t %1 @crew_name cobalt' "$STUB_LOG"
}

# ── Escalation tests ────────────────────────────────────────────────

# _esc_dispatch <branch> <session> <model> <tier> <ts> [engine] [extra-json]
_esc_dispatch() {
  local branch="$1" session="$2" model="$3" tier="$4" ts="$5" engine="${6:-claude}" extra="${7:-{\}}"
  local crew_dir="$TEST_REPO/.git/crew"
  mkdir -p "$crew_dir"
  jq -nc --arg b "$branch" --arg s "$session" --arg m "$model" --arg t "$tier" \
    --arg e "$engine" --argjson ts "$ts" --argjson x "$extra" '
    {ts:$ts, kind:"dispatch", branch:$b, session:$s,
     engine:$e, model:$m, tier:$t, effort:"high",
     shape:"", task_kind:"implement", title:"test", plan:"required", resume:false} + $x
  ' >>"$crew_dir/events.jsonl"
}

# _esc_status <branch> <session> <state> <ts>
_esc_status() {
  local branch="$1" session="$2" state="$3" ts="$4"
  local crew_dir="$TEST_REPO/.git/crew"
  mkdir -p "$crew_dir"
  jq -nc --arg b "$branch" --arg s "$session" --arg st "$state" --argjson ts "$ts" '
    {ts:$ts, kind:"status", from:("worker:"+$b+"#"+$s),
     body:{state:$st, detail:"test"}}
  ' >>"$crew_dir/events.jsonl"
}

_escalation_seed() {
  local branch="$1" model="$2" tier="$3" session="${4:-s-test}" engine="${5:-claude}"
  _esc_dispatch "$branch" "$session" "$model" "$tier" 100 "$engine"
  _esc_status "$branch" "$session" failed 200
}

_escalation_seed_spoof() {
  local branch="$1"
  local crew_dir="$TEST_REPO/.git/crew"
  mkdir -p "$crew_dir"
  jq -nc --arg b "$branch" '
    {ts: 200, kind:"status",
     from:("worker:"+$b+"#s-nonexistent"),
     body:{state:"failed", detail:"fake failure"}}
  ' >>"$crew_dir/events.jsonl"
}

@test "escalation: second dispatch after failed accepts standard opus and records escalated_from" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet standard
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]
  events="$TEST_REPO/.git/crew/events.jsonl"
  run jq -r --arg b "feat/42-do-a-thing" '
    [., inputs] | map(select(.kind == "dispatch" and .branch == $b and .escalated_from == "sonnet")) | length
  ' "$events"
  [ "$output" -gt 0 ]
}

@test "escalation: failed status with no matching dispatch row does NOT unlock escalation" {
  stub_launch_bins
  _escalation_seed_spoof "feat/42-do-a-thing"
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: trivial→opus refuses" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet trivial
  run run_dispatch trivial opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not trivial's row"* ]]
}

@test "escalation: two-rung jump refuses" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" haiku trivial
  run run_dispatch trivial opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not trivial's row"* ]]
}

@test "escalation: third attempt refuses (already escalated)" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet standard
  _esc_dispatch "feat/42-do-a-thing" s-escalated opus standard 250 claude '{"escalated_from":"sonnet"}'
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: an unstamped second dispatch after the failure also refuses" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet standard
  _esc_dispatch "feat/42-do-a-thing" s-escalated opus standard 250
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: standard-tier two-rung jump refuses" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" gpt-5.6-luna standard s-test codex
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_dispatch standard gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: record-only hop counts — luna, terra fail, then sol refuses" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" gpt-5.6-luna standard s1 codex
  _esc_dispatch "feat/42-do-a-thing" s2 gpt-5.6-terra standard 250 codex
  _esc_status "feat/42-do-a-thing" s2 failed 300
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_dispatch standard gpt-5.6-sol --agent codex --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: same-model retry counts — sonnet fails twice, then opus refuses" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet standard s1
  _esc_dispatch "feat/42-do-a-thing" s2 sonnet standard 250
  _esc_status "feat/42-do-a-thing" s2 failed 300
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: a branch that failed and later finished does not escalate" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet standard
  _esc_status "feat/42-do-a-thing" s-test done 300
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: a failed worker that was resumed (resume row only) still escalates" {
  stub_launch_bins
  _esc_dispatch "feat/42-do-a-thing" s1 sonnet standard 100
  jq -nc '{ts:160, kind:"resume", branch:"feat/42-do-a-thing", session:"s2", engine:"claude", model:"sonnet"}' \
    >>"$TEST_REPO/.git/crew/events.jsonl"
  _esc_status "feat/42-do-a-thing" s2 failed 200
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]
}

@test "escalation: claude-opus-* id is accepted as the escalation target" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet standard
  run run_dispatch standard claude-opus-5 --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 0 ]
}

@test "escalation: pi target must match exactly, not as a prefix" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" openrouter/deepseek/deepseek-v4.1-flash standard s-test pi
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_dispatch standard openrouter/deepseek/deepseek-v4-pro-evil/x --agent pi --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: cursor target must match exactly, not as a prefix" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" cursor-grok-4.6-medium standard s-test cursor
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_dispatch standard cursor-grok-4.6-highfoo --agent cursor --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: a failed trivial job does not unlock a standard-tier escalation" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet trivial
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: a Linear id alongside an issue number does not borrow the issue's failed history" {
  stub_launch_bins
  _escalation_seed "feat/42-do-a-thing" sonnet standard
  run run_dispatch standard opus --effort high --crew-id c1 ENG-9 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}

@test "escalation: a trivial-tier failure after a finished standard run does not unlock standard opus" {
  stub_launch_bins
  _esc_dispatch "feat/42-do-a-thing" s1 sonnet standard 100
  _esc_status "feat/42-do-a-thing" s1 pr_open 150
  _esc_dispatch "feat/42-do-a-thing" s2 haiku trivial 300
  _esc_status "feat/42-do-a-thing" s2 failed 400
  run run_dispatch standard opus --effort high --crew-id c1 42 "Do a thing"
  [ "$status" -eq 1 ]
  [[ "$output" == *"is not standard's row"* ]]
}
