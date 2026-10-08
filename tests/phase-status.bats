bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/phase-status.sh: a pi worker's phase, derived from its tool
# calls and posted as `crew status … working "<phase> (auto)"` on every phase
# change (#839). Never over the worker's own non-working state, never after a
# terminal one.

# shellcheck source=/dev/null
source "$BATS_TEST_DIRNAME/coverage.bash"

setup() {
  load helpers
  HANDLER="$BATS_TEST_DIRNAME/../adapters/core/phase-status.sh"
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  setup_repo
  # setup_repo leaves HEAD unborn; the handler resolves the branch for nothing,
  # but a real branch keeps the fixture ids honest.
  git commit --allow-empty -qm init
  git switch -qc feat/839-x
  printf 'crew_id: c1\ntier: standard\nkind: implement\n' >WORKER_TASK.md
  export CREW_WORKER_ID='worker:feat/839-x#s1-1'
  export CREW_ID=c1
  COMMON="$(git rev-parse --path-format=absolute --git-common-dir)"
  LOG="$COMMON/crew/events.jsonl"
  # `crew` is stubbed so every post is one line of argv in $STUB_LOG.
  stub_bin crew
}

teardown() {
  teardown_repo
}

# Envelope-shaped payloads: hookyard hands handlers json.Marshal(envelope)
# (internal/router/router.go), so `native` carries the engine payload and pi's
# tool_name arrives normalized (bash -> Bash, write -> Write; pi's `edit` has no
# mapping row and passes through).
envelope() { # <when: pre|post> <tool_name> <tool_input-json>
  jq -nc --arg w "$1" --arg t "$2" --argjson ti "$3" --arg d "$PWD" '
    {engine: "pi",
     canonical_event: (if $w == "pre" then "pre_tool" else "post_tool" end),
     native_event: (if $w == "pre" then "tool_call" else "tool_result" end),
     session_id: "sess-1", cwd: $d, protocol: "shell",
     tool_name: $t, tool_input: $ti,
     native: {hook_event_name: (if $w == "pre" then "tool_call" else "tool_result" end),
              cwd: $d, tool_name: $t, tool_input: $ti,
              tool_response: {content: [], is_error: false}}}'
}

post_bash() { envelope post Bash "$(jq -nc --arg c "$1" '{command: $c}')"; }
pre_bash() { envelope pre Bash "$(jq -nc --arg c "$1" '{command: $c}')"; }
post_edit() { # <tool_name> <path>
  envelope post "$1" "$(jq -nc --arg p "$2" '{path: $p, content: "x"}')"
}
# The raw Claude Code shape, unwired today (#839: "Claude workers could get the
# same from a PostToolUse hook later"), so the parse stays shape-generic.
claude_post_bash() {
  jq -nc --arg c "$1" --arg d "$PWD" \
    '{hook_event_name: "PostToolUse", session_id: "s", cwd: $d,
      tool_name: "Bash", tool_input: {command: $c}}'
}

run_handler() { bash -euo pipefail "$HANDLER"; }

# seed_row <state> [source] — a status row of this session. Straight to the bus
# because `crew status … pr_open` is refused on a standard implement run until
# the review and deslop seams exist, and the row is what the gate reads.
seed_row() {
  mkdir -p "$(dirname "$LOG")"
  jq -nc --arg c c1 --arg m "$CREW_WORKER_ID" --arg s "$1" --arg src "${2:-}" \
    '{ts: (now * 1000 | floor), crew_id: $c, from: $m, to: ("dispatcher:" + $c),
      kind: "status",
      body: ({state: $s} + (if $src == "" then {} else {source: $src} end))}' >>"$LOG"
}

# real_crew — put a `crew` on PATH that runs THIS tree's crew.sh, so the post and
# the roster read the same bus (case 12, the AC2 mechanism).
real_crew() {
  local bin="$BATS_TEST_TMPDIR/realbin"
  mkdir -p "$bin"
  printf '#!/usr/bin/env bash\nexec bash "%s" "$@"\n' "$CREW" >"$bin/crew"
  chmod +x "$bin/crew"
  export PATH="$bin:$PATH"
}

posts() { [ -s "$STUB_LOG" ] && cat "$STUB_LOG" || true; }

assert_posted() { # <detail>
  posts | grep -qxF -- "status $CREW_WORKER_ID working $1"
}

assert_no_posts() {
  [ -z "$(posts)" ]
}

assert_post_order() { # details, one per line
  [ "$(posts | sed "s|^status $CREW_WORKER_ID working ||")" = "$1" ]
}

@test "phase-status: the sequence posts one row per phase, in order" {
  run_handler <<<"$(post_edit Write "$PWD/plan.md")"
  run_handler <<<"$(post_edit edit "$PWD/src/a.py")"
  run_handler <<<"$(post_bash 'bats tests/a.bats')"
  run_handler <<<"$(post_bash 'git commit -am feat')"
  run_handler <<<"$(post_bash 'git push --force-with-lease origin head')"
  run_handler <<<"$(post_bash 'gh run watch 12345 --exit-status')"
  assert_post_order "$(printf '%s\n' 'plan (auto)' 'implement (auto)' \
    'test (auto)' 'commit (auto)' 'pr (auto)' 'ci (auto)')"
}

@test "phase-status: a loop of test calls posts once" {
  for _ in 1 2 3; do
    run_handler <<<"$(post_bash 'bats tests/a.bats')"
  done
  assert_posted 'test (auto)'
  [ "$(posts | grep -c .)" -eq 1 ]
}

@test "phase-status: a real transition back to plan posts again" {
  run_handler <<<"$(post_edit write "$PWD/.git/crew/artifacts/feat/839-x/plan.md")"
  run_handler <<<"$(post_edit edit "$PWD/src/a.py")"
  run_handler <<<"$(post_edit edit "$PWD/.git/crew/artifacts/feat/839-x/plan.md")"
  assert_post_order "$(printf '%s\n' 'plan (auto)' 'implement (auto)' 'plan (auto)')"
}

@test "phase-status: nothing posts after a terminal row, and blocked suppresses" {
  for state in pr_open done failed blocked; do
    : >"$STUB_LOG"
    seed_row "$state"
    run_handler <<<"$(post_bash 'bats tests/a.bats')"
    run_handler <<<"$(post_edit edit "$PWD/src/a.py")"
    assert_no_posts
  done
  seed_row working
  run_handler <<<"$(post_bash 'bats tests/a.bats')"
  assert_posted 'test (auto)'
}

@test "phase-status: a refused terminal attempt does not latch the handler" {
  # The tool call ran and FAILED (`crew status … pr_open` refused: no seams
  # yet), so no row exists and the handler must keep working.
  run_handler <<<"$(post_bash 'crew status \"$CREW_WORKER_ID\" pr_open \"AC1 pass(x)\" https://x/1')"
  assert_no_posts
  run_handler <<<"$(post_bash 'bats tests/a.bats')"
  assert_posted 'test (auto)'
}

@test "phase-status: crew status itself is control-plane, never a phase" {
  run_handler <<<"$(post_bash "crew status '$CREW_WORKER_ID' working 'execute: gate'")"
  assert_no_posts
}

@test "phase-status: awaiting comes from pre_tool, not post_tool" {
  run_handler <<<"$(pre_bash "crew await '$CREW_WORKER_ID' --from 'role:feat/839-x:plan-critic' --timeout 300")"
  assert_posted 'awaiting plan-critic (auto)'
  : >"$STUB_LOG"
  run_handler <<<"$(post_bash "crew await '$CREW_WORKER_ID' --from 'role:feat/839-x:plan-critic' --timeout 300")"
  assert_no_posts
  # A dispatcher await is not a role seam.
  : >"$STUB_LOG"
  run_handler <<<"$(pre_bash "crew await '$CREW_WORKER_ID' --from 'dispatcher:c1' --timeout 300")"
  assert_no_posts
}

@test "phase-status: a plan-critic assignment is plan" {
  run_handler <<<"$(post_bash "crew msg '$CREW_WORKER_ID' 'role:feat/839-x:plan-critic' '{\"seam\":\"plan\"}'")"
  assert_posted 'plan (auto)'
  : >"$STUB_LOG"
  run_handler <<<"$(post_bash "crew msg '$CREW_WORKER_ID' 'role:feat/839-x:reviewer' '{\"seam\":\"review\"}'")"
  assert_no_posts
}

@test "phase-status: a grid role pane posts nothing" {
  CREW_ROLE_ID='role:feat/839-x:reviewer' run_handler <<<"$(post_bash 'bats tests/a.bats')"
  CREW_ROLE_ID='role:feat/839-x:reviewer' run_handler <<<"$(pre_bash "crew await '$CREW_WORKER_ID' --from 'role:feat/839-x:plan-critic'")"
  assert_no_posts
}

@test "phase-status: a session that cannot name itself posts nothing" {
  run bash -eu -c 'unset CREW_WORKER_ID; bash -euo pipefail "$0"' "$HANDLER" <<<"$(post_bash 'bats tests/a.bats')"
  CREW_WORKER_ID= run_handler <<<"$(post_bash 'bats tests/a.bats')"
  assert_no_posts
}

@test "phase-status: inert commands and quoted strings post nothing" {
  for cmd in 'git status' 'ls -la' 'gh pr view 839' "jq -n '1'" 'git log --oneline -3' \
    'echo "git push"' 'cat plan.md' 'gh pr merge --squash' 'crew roster c1' 'nix develop'; do
    run_handler <<<"$(post_bash "$cmd")"
  done
  assert_no_posts
}

# reset_state — clear both the post log and the throttle, so one classification
# case cannot be silenced by the phase the case before it posted.
reset_state() {
  rm -rf "$COMMON/crew/phase-status"
  : >"$STUB_LOG"
}

@test "phase-status: wrappers and chains still classify" {
  run_handler <<<"$(post_bash 'CREW_ID=c1 timeout 300 bats tests/a.bats')"
  assert_posted 'test (auto)'
  reset_state
  run_handler <<<"$(post_bash 'git add -A && git commit -m x && git push')"
  assert_posted 'pr (auto)'
  reset_state
  run_handler <<<"$(post_bash 'go test ./internal/... ; shellcheck -x scripts/x.sh')"
  assert_posted 'test (auto)'
  reset_state
  run_handler <<<"$(post_bash 'nix flake check')"
  assert_posted 'test (auto)'
  reset_state
  run_handler <<<"$(post_bash 'npm test')"
  assert_posted 'test (auto)'
  reset_state
  run_handler <<<"$(post_bash 'just test')"
  assert_posted 'test (auto)'
  reset_state
  run_handler <<<"$(post_bash 'cargo test --release')"
  assert_posted 'test (auto)'
  reset_state
  run_handler <<<"$(post_bash 'bash scripts/bats-affected.sh --base main')"
  assert_posted 'test (auto)'
}

@test "phase-status: a here-document body is data, not commands" {
  heredoc_cmd="cat > /tmp/notes.md <<'EOF'
git push origin main
bats tests/a.bats
EOF"
  run_handler <<<"$(post_bash "$heredoc_cmd")"
  assert_no_posts

  # The line that opens the heredoc is still the command: this is a commit.
  commit_cmd="git commit -F - <<'MSG'
feat: something

It replaces the manual git push step entirely.
MSG"
  run_handler <<<"$(post_bash "$commit_cmd")"
  assert_posted 'commit (auto)'
}

@test "phase-status: only the plan artifact and the plan doc are plan" {
  run_handler <<<"$(post_edit edit "$PWD/src/plan.md")"
  assert_posted 'implement (auto)'
  reset_state
  run_handler <<<"$(post_edit edit "$PWD/docs/plan.md")"
  assert_posted 'implement (auto)'
  reset_state
  run_handler <<<"$(post_edit Write "$PWD/PLAN.md")"
  assert_posted 'plan (auto)'
  reset_state
  run_handler <<<"$(post_edit edit "$PWD/docs/superpowers/PLAN.md")"
  assert_posted 'plan (auto)'
}

@test "phase-status: foreign events and junk input exit silently" {
  run_handler <<<"$(jq -nc --arg d "$PWD" '{canonical_event:"turn_end",native_event:"turn_end",cwd:$d,tool_name:"Bash",tool_input:{command:"bats tests/a.bats"}}')"
  run_handler <<<''
  run_handler <<<'not json at all'
  run_handler <<<'"a string"'
  assert_no_posts
}

@test "phase-status: the claude PostToolUse shape classifies too" {
  run_handler <<<"$(claude_post_bash 'bats tests/a.bats')"
  assert_posted 'test (auto)'
}

@test "phase-status: the gate runs on the pre_tool path as well" {
  seed_row working
  run_handler <<<"$(pre_bash "crew await '$CREW_WORKER_ID' --from 'role:feat/839-x:reviewer'")"
  assert_posted 'awaiting reviewer (auto)'
  : >"$STUB_LOG"
  seed_row blocked
  run_handler <<<"$(pre_bash "crew await '$CREW_WORKER_ID' --from 'role:feat/839-x:reviewer'")"
  assert_no_posts
}

@test "phase-status: a watchdog blocked does not suppress, a watchdog failed does" {
  seed_row blocked watchdog
  run_handler <<<"$(post_bash 'bats tests/a.bats')"
  assert_posted 'test (auto)'
  : >"$STUB_LOG"
  seed_row failed watchdog
  run_handler <<<"$(post_bash 'git commit -m x')"
  assert_no_posts
}

@test "phase-status: the state file is written and leaves no temp behind" {
  run_handler <<<"$(post_bash 'bats tests/a.bats')"
  assert_posted 'test (auto)'
  dir="$COMMON/crew/phase-status"
  [ -d "$dir" ]
  [ "$(find "$dir" -type f | wc -l)" -eq 1 ]
  run find "$dir" -name '.st.*' -o -name '*.tmp'
  [ -z "$output" ]
  grep -q '^phase=test$' "$(find "$dir" -type f)"
}

@test "phase-status: crew roster reports the phase the hook posted" {
  # AC2's mechanism on the real bus: the handler posts through crew.sh, and the
  # roster the dispatcher reads carries the detail.
  real_crew
  run_handler <<<"$(post_edit Write "$PWD/plan.md")"
  run_handler <<<"$(post_bash 'bats tests/a.bats')"
  roster="$(CREW_ID=c1 bash -euo pipefail "$CREW" roster c1)"
  printf '%s' "$roster" | jq -e --arg m "$CREW_WORKER_ID" \
    'length == 1 and .[0].state == "working" and (.[0].detail == "test (auto)")'
}

@test "phase-status: no crew on PATH means no post and no failure" {
  rm -f "$STUB_DIR/crew"
  PATH="$(path_without_real crew)" run_handler <<<"$(post_bash 'bats tests/a.bats')"
  assert_no_posts
}
