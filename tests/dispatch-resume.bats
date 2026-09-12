setup() {
  load helpers
  RESUME="$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  run_resume() { bash -euo pipefail "$RESUME" "$@"; }
  setup_repo
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID TMUX_PANE
  stub_tmux_no_pane
  stub_bin crew
  # engine-cmd needs real matching (mirrors crew.sh's own _is_engine_cmd,
  # #111): everything else in dispatch-resume.sh only cares that the call was
  # made, so it keeps the generic log-and-succeed behaviour stub_bin gave it.
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = engine-cmd ]; then
  c="${2#.}"
  c="${c%-wrapped}"
  case "$c" in
  claude | codex | cursor-agent | node | pi) exit 0 ;;
  esac
  exit 1
fi
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
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

# wait_for_log <pattern> — poll $STUB_LOG for a line written by a backgrounded
# stub (the nohup'd stall-watch). Fails the test after ~2s.
wait_for_log() {
  local i
  for i in $(seq 1 40); do
    grep -qE "$1" "$STUB_LOG" && return 0
    sleep 0.05
  done
  echo "wait_for_log: never saw '$1' in $STUB_LOG" >&2
  cat "$STUB_LOG" >&2
  return 1
}

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
# $4 (pane_current_command) defaults to empty — a plain shell, i.e. nothing an
# engine matcher would claim — so existing callers that omit it keep meaning
# "a human sitting there".
stub_tmux_with_pane_at_wt() { # $1=window id  $2=pane id  $3=@crew_name value  $4=pane_current_command
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1" in
list-panes) printf '%s\t%s\t%s\t%s\t%s\n' '$1' '$2' '$WT' '${4:-}' '$3' ;;
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

@test "refuses a pane still running a live claude engine" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris claude
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"already alive there"* ]]
  [[ "$output" == *"tmux select-window -t @4"* ]]
  run grep -c send-keys "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "refuses a pane running a nix-wrapped engine name" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris .claude-wrapped
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"already alive there"* ]]
}

@test "refuses the guard on --print too, before any placement is reported" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris codex
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 1 ]
  [[ "$output" != *"placement:"* ]]
}

@test "still resumes into a plain shell at the worktree (the primary use case)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' '' fish
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
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
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ claude --continue' "$STUB_LOG"
  grep -q 'CREW_WORKER_ID=worker:feat/7-a-thing#s2-100 CREW_ID=c1 claude --continue' "$STUB_LOG"
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
  grep -qE 'send-keys -t %8 CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ claude ' "$STUB_LOG"
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

@test "codex --fresh drops the resume flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ codex ' "$STUB_LOG"
  run grep -c -- 'resume --last' "$STUB_LOG"
  [ "$status" -ne 0 ]
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

@test "cursor --fresh drops the continue flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: cursor/' -e 's/^model: sonnet/model: composer-2.5/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ CURSOR_CLI_INDEXED_GREP=0 cursor-agent ' "$STUB_LOG"
  run grep -c -- '--continue' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "pi resume launches with --continue and reapplies the protocol" {
  setup_worker_wt 'roles: plan-critic,reviewer'
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'pi --continue' "$STUB_LOG"
  grep -q -- '--append-system-prompt /opt/protocols/WORKER_PROTOCOL.md' "$STUB_LOG"
  grep -q -- '--no-approve' "$STUB_LOG"
  grep -q 'role panes (plan-critic,reviewer) may still be parked' "$STUB_LOG"
}

@test "pi --fresh drops the continue flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ pi ' "$STUB_LOG"
  run grep -c -- '--continue' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "the reorient prompt tells the worker not to trust its last plan" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'do not trust the last plan in your transcript' "$STUB_LOG"
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

bus_log() { printf '%s/.git/crew/events.jsonl' "$TEST_REPO"; }

@test "writes a resume row naming both worker identities" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  row="$(jq -c 'select(.kind == "resume")' "$(bus_log)" | tail -1)"
  [ "$(jq -r .crew_id <<<"$row")" = c1 ]
  [ "$(jq -r .branch <<<"$row")" = feat/7-a-thing ]
  [ "$(jq -r .worker_id <<<"$row")" = 'worker:feat/7-a-thing#s2-100' ]
  [ "$(jq -r .prev_worker_id <<<"$row")" = 'worker:feat/7-a-thing#s1-99' ]
  [ "$(jq -r .continued <<<"$row")" = true ]
  [ "$(jq -r .engine <<<"$row")" = claude ]
}

@test "--fresh records continued false" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  [ "$(jq -r 'select(.kind == "resume") | .continued' "$(bus_log)" | tail -1)" = false ]
}

@test "posts a working status under the NEW worker id" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -q 'status worker:feat/7-a-thing#s2-100 working resumed' "$STUB_LOG"
}

@test "updates worker_id in the task doc and leaves the body alone" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'worker_id: worker:feat/7-a-thing#s2-100' "$WT/WORKER_TASK.md"
  grep -qx 'the original body' "$WT/WORKER_TASK.md"
  grep -qx 'title: a thing' "$WT/WORKER_TASK.md"
}

@test "stamps resume: true when the header has no resume field yet (#112)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'resume: true' "$WT/WORKER_TASK.md"
  run grep -c '^resume: ' "$WT/WORKER_TASK.md"
  [ "$output" = 1 ]
}

@test "rewrites an existing resume field to true rather than duplicating it" {
  setup_worker_wt 'resume: false'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'resume: true' "$WT/WORKER_TASK.md"
  run grep -c '^resume: ' "$WT/WORKER_TASK.md"
  [ "$output" = 1 ]
}

@test "inserting resume: true leaves a body line that happens to read resume: false alone" {
  setup_worker_wt
  printf 'the resume field is documented as\nresume: false\nby default\n' >>"$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  # exactly one header-position "resume: true" line, and the body's own
  # "resume: false" line is untouched, not overwritten by the header rewrite.
  run grep -n '^resume: ' "$WT/WORKER_TASK.md"
  [ "$status" -eq 0 ]
  [ "$(printf '%s\n' "$output" | wc -l)" = 2 ]
  grep -qx 'resume: false' "$WT/WORKER_TASK.md"
}

@test "runs solo when the crew has no registered dispatcher" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"solo"* ]]
  run grep -c 'crew msg' "$STUB_LOG"
  [ "$status" -ne 0 ]
  grep -qx 'dispatcher_pane: %3' "$WT/WORKER_TASK.md"
}

@test "reattaches to a live dispatcher: retargets the pane and messages it" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '%s\n' "$$" >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"reattached"* ]]
  grep -qx 'dispatcher_pane: %77' "$WT/WORKER_TASK.md"
  grep -q 'msg .* dispatcher:c1' "$STUB_LOG"
}

@test "runs solo when the registered dispatcher pid is dead" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '999999999\n' >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"solo"* ]]
  grep -qx 'dispatcher_pane: %3' "$WT/WORKER_TASK.md"
}

@test "re-arms the stall watchdog on the resumed pane" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  wait_for_log 'stall-watch worker:feat/7-a-thing#s[0-9]+-[0-9]+ --pane %8 --engine claude'
}
