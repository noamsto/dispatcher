setup() {
  load helpers
  RESUME="$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  export CREW_REAL="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
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
if [ "$1" = pi-agent-dir ]; then exec bash -euo pipefail "$CREW_REAL" pi-agent-dir; fi
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
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR"/{WORKER_PROTOCOL.md,EVIDENCE_REVIEW.md,GRID_PROTOCOL.md,REVIEW_TASK.md}
  # Stands in for the store path flake.nix bakes as @skillsDir@ (#225).
  export DISPATCHER_SKILLS_DIR="$TEST_REPO/harness-skills"
  mkdir -p "$DISPATCHER_SKILLS_DIR/spec-plan-critic"
  printf -- '---\nname: spec-plan-critic\ndescription: seeded\n---\n' \
    >"$DISPATCHER_SKILLS_DIR/spec-plan-critic/SKILL.md"
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

@test "aborts before scaffolding when a required protocol file is missing" {
  setup_worker_wt
  cd "$WT"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-incomplete"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"EVIDENCE_REVIEW.md"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
}

@test "refuses a protocol dir whose content hashes to a different revision than the script marker" {
  setup_worker_wt
  cd "$WT"
  # Baked-marker simulation, mirroring flake.nix's replaceStrings (see
  # _substituted_dispatch in dispatch.bats).
  sed 's/@protocolRev@/0123456789abcdef/' "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-mismatch"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev_dir="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-subst.sh"
  [ "$status" -eq 1 ]
  [[ "$output" == *"protocol directory version mismatch"* ]]
  [[ "$output" == *"0123456789abcdef"* ]]
  [[ "$output" == *"$rev_dir"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
}

@test "resume proceeds when the protocol dir hashes to the script marker" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' '' fish
  cd "$WT"
  sed 's/@protocolRev@/0123456789abcdef/' "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-matching"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  sed "s/@protocolRev@/$rev/" "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-subst.sh"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "a raw (unsubstituted) resume script skips the revision check with a one-line warning" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' '' fish
  cd "$WT"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-rev"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"unsubstituted protocol revision"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

# #253: same guard, resume side. The guard runs at dispatch-resume.sh:242-243,
# before the engine precheck at :415, so it is engine-independent; parametrize
# over the engines the issue names as untested (codex, cursor, pi). The header
# rewrite uses anchored wildcards so every iteration actually changes the
# engine rather than no-opping after the first.
@test "resume aborts on a missing protocol file for codex, cursor and pi" {
  setup_worker_wt
  cd "$WT"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-incomplete"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    DISPATCH_PROFILE="$profile" run run_resume
    [ "$status" -eq 1 ]
    [[ "$output" == *"EVIDENCE_REVIEW.md"* ]]
    [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
    [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
    [ ! -f "$STUB_LOG" ] || ! grep -q 'send-keys' "$STUB_LOG"
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

@test "resume refuses a stale protocol dir for codex, cursor and pi" {
  setup_worker_wt
  cd "$WT"
  sed 's/@protocolRev@/0123456789abcdef/' "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-mismatch"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev_dir="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    DISPATCH_PROFILE="$profile" run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-subst.sh"
    [ "$status" -eq 1 ]
    [[ "$output" == *"protocol directory version mismatch"* ]]
    [[ "$output" == *"0123456789abcdef"* ]]
    [[ "$output" == *"$rev_dir"* ]]
    [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
    [ ! -f "$STUB_LOG" ] || ! grep -q 'new-window' "$STUB_LOG"
    [ ! -f "$STUB_LOG" ] || ! grep -q 'send-keys' "$STUB_LOG"
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

# Happy path: the resume launch prompt names the protocol dir. codex/cursor
# carry it in the prompt string; pi carries it in --append-system-prompt. All
# three also carry the protocol_note in the prompt. Assert each engine's actual
# carrier rather than assuming symmetry.
@test "resume names the protocol dir in the codex, cursor and pi launch" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    : >"$STUB_LOG"
    DISPATCH_PROFILE="$profile" run run_resume
    [ "$status" -eq 0 ]
    case "$eng" in
    pi) grep -q -- "--append-system-prompt $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" <(launch_log) ;;
    *) grep -q -- "Read $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md" <(launch_log) ;;
    esac
    grep -q -- "live in $DISPATCHER_PROTOCOL_DIR" <(launch_log)
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

# _assert_resume_bound <marker> — the resumed lead's send-keys line is only
# `bash '<launch script>'` (#298), under 512 bytes, and the script carries
# <marker> (the engine's continue/resume flag).
_assert_resume_bound() {
  local marker="$1" crew_launch_dir pattern line
  crew_launch_dir="$(git -C "$TEST_REPO" rev-parse --path-format=absolute --git-common-dir)/crew/launch"
  pattern="^send-keys -t %8 bash '${crew_launch_dir}/launch\.[A-Za-z0-9]{6}' Enter\$"
  line="$(grep '^send-keys' "$STUB_LOG")"
  [[ "$line" =~ $pattern ]]
  [ "${#line}" -lt 512 ]
  grep -q -F -- "$marker" <(launch_log)
}

# #298: a fresh pane's tty truncates typed-ahead input at 1024 bytes on macOS,
# so resume must type a short line for every engine, however long the prompt.
@test "bound: resume's send-keys line stays short and shaped for every engine" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  _assert_resume_bound 'claude --continue'

  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    : >"$STUB_LOG"
    DISPATCH_PROFILE="$profile" run run_resume
    [ "$status" -eq 0 ]
    case "$eng" in
    codex) _assert_resume_bound 'codex resume --last' ;;
    cursor) _assert_resume_bound 'cursor-agent --continue' ;;
    pi) _assert_resume_bound 'pi --continue' ;;
    esac
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

# dispatch-resume.sh is a standalone build, so it carries its own copies.
@test "shell_quote and write_launch_script are byte-identical between dispatch.sh and dispatch-resume.sh" {
  for fn in shell_quote write_launch_script; do
    a="$(sed -n "/^${fn}() {/,/^}/p" "$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh")"
    b="$(sed -n "/^${fn}() {/,/^}/p" "$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh")"
    [ -n "$a" ]
    [ "$a" = "$b" ]
  done
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

@test "resume preserves a stacked base: through the header rewrite" {
  # A real resume (not --print) so the _hdr_set rewrite actually runs; base: is
  # not in its field list, and must stay that way for a stacked worker.
  setup_worker_wt 'base: feat/parent'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'base: feat/parent' "$WT/WORKER_TASK.md"
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
  grep -qE 'send-keys -t %8 GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ claude --continue' <(launch_log)
  grep -q 'CREW_WORKER_ID=worker:feat/7-a-thing#s2-100 CREW_ID=c1 claude --continue' <(launch_log)
  grep -q -- '--model sonnet' <(launch_log)
  grep -q -- '--effort medium' <(launch_log)
  grep -q -- "--append-system-prompt-file $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" <(launch_log)
}

@test "restamps protocol_dir into an older task doc and names it in the claude prompt" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx "protocol_dir: $DISPATCHER_PROTOCOL_DIR" "$WT/WORKER_TASK.md"
  grep -q -- "live in $DISPATCHER_PROTOCOL_DIR" <(launch_log)
}

@test "--fresh drops the continue flag" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ claude ' <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "codex resume launches resume --last" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'codex resume --last' <(launch_log)
  grep -q -- '--profile worker' <(launch_log)
}

@test "codex --fresh drops the resume flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ codex ' <(launch_log)
  run grep -c -- 'resume --last' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "cursor resume launches with --continue" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: cursor/' -e 's/^model: sonnet/model: composer-2.5/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'cursor-agent --continue' <(launch_log)
}

@test "cursor --fresh drops the continue flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: cursor/' -e 's/^model: sonnet/model: composer-2.5/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ CURSOR_CLI_INDEXED_GREP=0 cursor-agent ' <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "pi resume launches with --continue and reapplies the protocol" {
  setup_worker_wt 'roles: plan-critic,reviewer'
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q "PI_CODING_AGENT_DIR=$HOME/.pi/dispatcher-worker pi --continue" <(launch_log)
  grep -q -- "--append-system-prompt $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" <(launch_log)
  grep -q -- '--no-approve' <(launch_log)
  grep -q 'role panes (plan-critic,reviewer) may still be parked' <(launch_log)
  [ "$(jq -r .defaultProjectTrust "$HOME/.pi/dispatcher-worker/settings.json")" = never ]
}

@test "pi resume passes the worktree's project skills and the harness skills with --skill" {
  setup_worker_wt
  mkdir -p "$WT/.agents/skills/preview"
  printf -- '---\nname: preview\ndescription: seeded\n---\n' >"$WT/.agents/skills/preview/SKILL.md"
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- "--no-approve --skill $WT/.agents/skills --skill $DISPATCHER_SKILLS_DIR " <(launch_log)
}

@test "pi resume passes only the harness skills when the worktree has none" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- "--no-approve --skill $DISPATCHER_SKILLS_DIR " <(launch_log)
  [ "$(grep -cF -- '--skill' <(launch_log))" -eq 1 ]
}

# A non-Nix install leaves @skillsDir@ unsubstituted, so the path is not a
# directory and the probe has to drop it rather than hand pi a literal.
@test "pi resume omits --skill when neither the worktree nor the harness dir exists" {
  setup_worker_wt
  export DISPATCHER_SKILLS_DIR="$TEST_REPO/no-such-skills"
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -F -- '--skill' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "pi --fresh drops the continue flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ PI_CODING_AGENT_DIR=[^ ]+ pi ' <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "pi resume fails closed when the agent dir cannot be seeded" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = pi-agent-dir ]; then exit 0; fi
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
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not seed the pi worker agent dir"* ]]
  run grep -c -- 'send-keys' "$STUB_LOG"
  [ "$status" -ne 0 ]
  run grep -c -- 'new-window' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "pi resume --print does not seed the agent dir" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [ ! -d "$HOME/.pi/dispatcher-worker" ]
}

@test "pi resume leaves the ambient ~/.pi/agent fixture untouched and does not leak secrets" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$HOME/.pi/agent"
  printf '{"opencode":{"type":"api_key","key":"SECRET-RESUME-FIXTURE"}}\n' >"$HOME/.pi/agent/auth.json"
  printf '{"defaultProjectTrust":"always"}\n' >"$HOME/.pi/agent/settings.json"
  before_auth=$(sha256sum "$HOME/.pi/agent/auth.json")
  before_settings=$(sha256sum "$HOME/.pi/agent/settings.json")
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [ "$(sha256sum "$HOME/.pi/agent/auth.json")" = "$before_auth" ]
  [ "$(sha256sum "$HOME/.pi/agent/settings.json")" = "$before_settings" ]
  run grep -rF SECRET-RESUME-FIXTURE "$HOME/.pi/dispatcher-worker"
  [ "$status" -ne 0 ]
}

@test "the reorient prompt tells the worker not to trust its last plan" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'do not trust the last plan in your transcript' <(launch_log)
}

@test "trailing arguments are appended to the prompt" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume the review comments are the priority
  [ "$status" -eq 0 ]
  grep -q 'the review comments are the priority' <(launch_log)
}

# The prompt lives in the launch script; the typed line carries only the
# script's one quoted path.
@test "no launch string contains an apostrophe" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c "send-keys.*'.*'.*'" <(launch_log)
  [ "$status" -ne 0 ]
  raw="$(grep 'send-keys' "$STUB_LOG")"
  [ "$(grep -o "'" <<<"$raw" | wc -l)" -eq 2 ]
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

_resume_esc_seed() { # [failed-session] [failed-ts]
  local fs="${1:-s1-99}" fts="${2:-200}"
  crew_dir="$TEST_REPO/.git/crew"
  mkdir -p "$crew_dir"
  jq -nc --arg b "feat/7-a-thing" '
    {ts: 100, kind:"dispatch", branch:$b, session:"s1-99",
     engine:"claude", model:"sonnet", tier:"standard", effort:"medium",
     shape:"", task_kind:"implement", title:"a thing", plan:"required", resume:false}
  ' >>"$crew_dir/events.jsonl"
  jq -nc --arg b "feat/7-a-thing" --arg s "$fs" --argjson ts "$fts" '
    {ts: $ts, kind:"status",
     from:("worker:"+$b+"#"+$s),
     body:{state:"failed", detail:"test failure"}}
  ' >>"$crew_dir/events.jsonl"
}

# The dispatch stub accepts everything, so a refused escalation shows up as a
# precheck that was NOT handed --ignore-map (the real gate would exit 1).
_precheck_ignores_map() { grep 'resume precheck' "$STUB_LOG" | grep -q -- '--ignore-map'; }

@test "resume escalation: --model opus succeeds after prior failed with matching dispatch" {
  setup_worker_wt
  _resume_esc_seed
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  _precheck_ignores_map
  run jq -r 'select(.kind == "resume") | .escalated_from' "$crew_dir/events.jsonl"
  [ "$output" = "sonnet" ]
  grep -qx 'model: opus' "$WT/WORKER_TASK.md"
  grep -qx 'escalated_from: sonnet' "$WT/WORKER_TASK.md"
  # The header now records the escalated model, so a later plain resume
  # relaunches on it rather than silently falling back to sonnet.
  run ! grep -qx 'model: sonnet' "$WT/WORKER_TASK.md"
}

@test "resume escalation: a failure posted only by the resumed session still escalates" {
  setup_worker_wt
  _resume_esc_seed s2-99 200
  # s1 never failed; s2 exists only in a resume row.
  jq -c 'select(.kind == "status") | .from = "worker:feat/7-a-thing#s2-99"' "$crew_dir/events.jsonl" >"$crew_dir/x"
  jq -c 'select(.kind == "dispatch")' "$crew_dir/events.jsonl" >"$crew_dir/y"
  jq -nc '{ts:150, kind:"resume", branch:"feat/7-a-thing", session:"s2-99", engine:"claude", model:"sonnet"}' >>"$crew_dir/y"
  cat "$crew_dir/x" >>"$crew_dir/y"
  mv "$crew_dir/y" "$crew_dir/events.jsonl"
  rm -f "$crew_dir/x"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  _precheck_ignores_map
  run jq -r 'select(.kind == "resume" and .session != null) | .escalated_from // "none"' "$crew_dir/events.jsonl"
  [[ "$output" == *"sonnet"* ]]
}

@test "resume escalation: a spoofed failed status does not unlock --model opus" {
  setup_worker_wt
  _resume_esc_seed s-nonexistent
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
  grep -qx 'model: sonnet' "$WT/WORKER_TASK.md"
}

@test "resume escalation: an already-escalated branch refuses a second --model opus" {
  setup_worker_wt
  _resume_esc_seed
  jq -nc '{ts:250, kind:"resume", branch:"feat/7-a-thing", session:"s2-99", engine:"claude", model:"opus", escalated_from:"sonnet"}' >>"$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: a branch that failed and later finished does not escalate" {
  setup_worker_wt
  _resume_esc_seed
  jq -nc '{ts:300, kind:"status", from:"worker:feat/7-a-thing#s1-99", body:{state:"done"}}' >>"$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: a trivial-tier worker cannot reach opus" {
  setup_worker_wt
  sed -i 's/^tier: standard/tier: trivial/' "$WT/WORKER_TASK.md"
  _resume_esc_seed
  sed -i 's/"tier":"standard"/"tier":"trivial"/' "$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: a header claiming sonnet/standard cannot borrow a trivial-tier failure" {
  setup_worker_wt
  _resume_esc_seed
  sed -i 's/"model":"sonnet","tier":"standard"/"model":"haiku","tier":"trivial"/' "$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: an opus id that only shares the target as a prefix is refused" {
  setup_worker_wt
  sed -i 's/^engine: claude/engine: cursor/; s/^model: sonnet/model: cursor-grok-4.6-medium/' "$WT/WORKER_TASK.md"
  _resume_esc_seed
  sed -i 's/"engine":"claude"/"engine":"cursor"/; s/"model":"sonnet"/"model":"cursor-grok-4.6-medium"/' "$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model cursor-grok-4.6-highfoo
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "re-arms the stall watchdog on the resumed pane" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  wait_for_log 'stall-watch worker:feat/7-a-thing#s[0-9]+-[0-9]+ --pane %8 --engine claude'
}
