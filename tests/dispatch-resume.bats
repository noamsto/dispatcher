setup() {
  load helpers
  RESUME="$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  run_resume() { bash -euo pipefail "$RESUME" "$@"; }
  setup_repo
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID TMUX_PANE
  stub_tmux_no_pane
  stub_bin crew
  stub_bin gh
  cat >"$STUB_DIR/dispatch" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
[ -n "${DISPATCH_PRECHECK:-}" ] || exit 0
[ -z "${STUB_PRECHECK_FAIL:-}" ] || {
  echo "dispatch: --effort ultra is codex-only; claude tops out at max" >&2
  exit 1
}
exit 0
EOF
  chmod +x "$STUB_DIR/dispatch"
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
  git commit --allow-empty -qm init
}

teardown() { teardown_repo; }

# Default tmux stub: no pane sits at the worktree, so placement resolution
# takes the create-a-window path. Individual tests override $STUB_DIR/tmux
# when they need the reuse path instead.
stub_tmux_no_pane() {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-panes) : ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

# A worktree that looks like a live worker's: its own branch, its own
# directory, and a task document with a full header.
setup_worker_wt() { # [extra header lines...]
  git -C "$TEST_REPO" worktree add -q -b feat/7-a-thing "$TEST_REPO/wt" HEAD
  WT="$TEST_REPO/wt"
  {
    printf 'tier: standard\nkind: implement\ndraft: false\n'
    printf 'engine: claude\nmodel: sonnet\neffort: medium\nmcp: \n'
    printf 'plan: required\ntitle: a thing\nCloses #7\n'
    printf 'dispatcher_pane: %%3\ncrew_dir: %s/.git/crew\ncrew_id: c1\n' "$TEST_REPO"
    printf 'agent_name: iris\nworker_id: worker:feat/7-a-thing#s1-99\n'
    for extra in "$@"; do printf '%s\n' "$extra"; done
    printf '\n## Task\n\nthe original body\n'
  } >"$WT/WORKER_TASK.md"
  export WT
}

@test "refuses outside a worktree carrying a task document" {
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"no WORKER_TASK.md"* ]]
  [[ "$output" == *"dispatch <tier> <model>"* ]]
}

@test "refuses in the primary worktree even with a task document" {
  printf 'engine: claude\nmodel: sonnet\neffort: medium\ncrew_id: c1\n' >"$TEST_REPO/WORKER_TASK.md"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"primary worktree"* ]]
}

@test "refuses on a detached HEAD" {
  setup_worker_wt
  git -C "$WT" checkout -q --detach
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"detached HEAD"* ]]
}

@test "refuses when the header lacks the launch tuple" {
  setup_worker_wt
  printf 'tier: standard\ncrew_id: c1\n' >"$WT/WORKER_TASK.md"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"header is missing"* ]]
  [[ "$output" == *"engine"* ]]
}

@test "--print reports the resolved launch and does not launch" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"branch: feat/7-a-thing"* ]]
  [[ "$output" == *"engine: claude"* ]]
  [[ "$output" == *"model: sonnet"* ]]
  [[ "$output" == *"effort: medium"* ]]
  [[ "$output" == *"crew_id: c1"* ]]
  run cat "$STUB_LOG"
  [[ "$output" != *send-keys* ]]
}

@test "explicit flags override the recorded header" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print --model opus --effort high
  [ "$status" -eq 0 ]
  [[ "$output" == *"model: opus"* ]]
  [[ "$output" == *"effort: high"* ]]
}

@test "the task body is left byte-identical" {
  setup_worker_wt
  cd "$WT"
  before="$(md5sum <"$WT/WORKER_TASK.md")"
  run run_resume --print
  [ "$status" -eq 0 ]
  [ "$(md5sum <"$WT/WORKER_TASK.md")" = "$before" ]
}

# tmux stub that reports one pane sitting at $WT, so the reuse path fires.
stub_tmux_with_pane_at_wt() { # $1=window id  $2=pane id  $3=@crew_name value
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1" in
list-panes) printf '%s\t%s\t%s\t%s\n' '$1' '$2' '$WT' '$3' ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

@test "--print names the existing pane at the worktree" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"window: @4"* ]]
  [[ "$output" == *"pane: %8"* ]]
  [[ "$output" == *"placement: reuse"* ]]
}

@test "--print reuses a pane with no worker identity (a human sitting there)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' ''
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"pane: %8"* ]]
  [[ "$output" == *"placement: reuse"* ]]
}

@test "--print reports a fresh window when nothing sits at the worktree" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"placement: create"* ]]
  [[ "$output" == *"window: -"* ]]
  [[ "$output" == *"pane: -"* ]]
}

@test "--print on the create path opens no window and stamps nothing" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  run cat "$STUB_LOG"
  [[ "$output" != *new-window* ]]
  [[ "$output" != *set-window-option* ]]
}

@test "--print on the reuse path stamps nothing" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  run cat "$STUB_LOG"
  [[ "$output" != *set-window-option* ]]
}

@test "creating a window stamps the crew identity on it" {
  setup_worker_wt
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-panes) : ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'set-window-option -t %99 @crew_name iris' "$STUB_LOG"
}

@test "reusing a pane does not open a window" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c new-window "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "runs the dispatch precheck with the recorded tuple" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qE 'standard sonnet .*--effort medium' "$STUB_LOG"
  grep -q -- '--agent claude' "$STUB_LOG"
  grep -q -- '--crew-id c1' "$STUB_LOG"
}

@test "suppresses the tier-model map when no model was named" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- '--ignore-map' "$STUB_LOG"
}

@test "re-arms the tier-model map when --model is passed" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run grep -c -- '--ignore-map' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "forwards --ignore-budget to the precheck" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --ignore-budget
  [ "$status" -eq 0 ]
  grep -q -- '--ignore-budget' "$STUB_LOG"
}

@test "a refused precheck aborts before any launch" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  STUB_PRECHECK_FAIL=1 run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"ultra is codex-only"* ]]
  run grep -c send-keys "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "refuses an mcp profile on a non-claude engine" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^mcp: $/mcp: analytics/' "$WT/WORKER_TASK.md"
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"claude-only"* ]]
}

@test "claude resume launches with --continue and the recorded tuple" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'send-keys -t %8 claude --continue' "$STUB_LOG"
  grep -q -- '--model sonnet' "$STUB_LOG"
  grep -q -- '--effort medium' "$STUB_LOG"
  grep -q -- '--append-system-prompt-file /opt/protocols/WORKER_PROTOCOL.md' "$STUB_LOG"
}

@test "--fresh drops the continue flag" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -q 'send-keys -t %8 claude ' "$STUB_LOG"
  run grep -c -- '--continue' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "codex resume launches resume --last" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'codex resume --last' "$STUB_LOG"
  grep -q -- '--profile worker' "$STUB_LOG"
}

@test "cursor resume launches with --continue" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: cursor/' -e 's/^model: sonnet/model: composer-2.5/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'cursor-agent --continue' "$STUB_LOG"
}

@test "the reorient prompt tells the worker not to trust its last plan" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'do not trust your transcript' "$STUB_LOG"
}

@test "trailing arguments are appended to the prompt" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume the review comments are the priority
  [ "$status" -eq 0 ]
  grep -q 'the review comments are the priority' "$STUB_LOG"
}

@test "no launch string contains an apostrophe" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c "send-keys.*'.*'.*'" "$STUB_LOG"
  [ "$status" -ne 0 ]
}
