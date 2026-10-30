bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/tmux-server-guard.sh: a dispatched worker's tmux calls that
# reach the live server (#1332) are blocked. Calls carrying -L/-S, read-only
# calls, window/pane-scoped calls and a human session are not.

# shellcheck source=/dev/null
source "$BATS_TEST_DIRNAME/coverage.bash"

setup() {
  GUARD="$BATS_TEST_DIRNAME/../adapters/core/tmux-server-guard.sh"
  export CREW_WORKER_ID='worker:feat/1332-guard#s1'
}

run_guard() {
  bash -euo pipefail "$GUARD"
}

claude_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"PreToolUse",session_id:"s",tool_name:"Bash",tool_input:{command:$cmd},cwd:"/tmp"}'
}

pi_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{canonical_event:"pre_tool",hook_event_name:"tool_call",tool_name:"Bash",tool_input:{command:$cmd},cwd:"/tmp"}'
}

cursor_shell_exec() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"beforeShellExecution",cursor_version:"2026.09.08",command:$cmd,cwd:"/tmp"}'
}

assert_allow() {
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

assert_block() { # <substring of the reason>
  [ "$status" -eq 0 ]
  jq -e --arg w "$1" \
    '.hookSpecificOutput.permissionDecision == "deny" and
     (.hookSpecificOutput.permissionDecisionReason | contains($w))' <<<"$output" >/dev/null
}

PROBE1='D=/tmp/ogprobe2-$$; mkdir -p $D; export TMUX_TMPDIR=$D
tmux -f /dev/null new-session -d -s S -x 200 -y 40
tmux set -g base-index 0
tmux -L outer new-session -d -s wide -x 200 -y 40 "env -u TMUX tmux attach -t S"
...
tmux -L outer kill-server 2>/dev/null
tmux kill-server 2>/dev/null'

PROBE2='cd /tmp && rm -rf og-pc-probe && mkdir -p og-pc-probe && export TMUX_TMPDIR=/tmp/og-pc-probe
tmux -f /dev/null new-session -d -s S -x 200 -y 40
tmux set-hook -g client-detached[0] '"'"'display-message "zero"'"'"'
tmux set-hook -gu client-detached
tmux kill-server 2>/dev/null || true'

@test "tmux-server-guard: blocks incident probe 1 (claude and pi)" {
  run run_guard <<<"$(claude_bash "$PROBE1")"
  assert_block 'set -g'
  run run_guard <<<"$(pi_bash "$PROBE1")"
  assert_block 'set -g'
}

@test "tmux-server-guard: blocks incident probe 2 (claude and pi)" {
  run run_guard <<<"$(claude_bash "$PROBE2")"
  assert_block 'set-hook'
  run run_guard <<<"$(pi_bash "$PROBE2")"
  assert_block 'set-hook'
}

@test "tmux-server-guard: blocks a bare kill-server" {
  run run_guard <<<"$(pi_bash 'tmux kill-server')"
  assert_block 'kill-server'
  run run_guard <<<"$(claude_bash 'tmux kill-server 2>/dev/null || true')"
  assert_block 'kill-server'
}

@test "tmux-server-guard: allows an explicit -L or -S" {
  for cmd in 'tmux -L probe kill-server' 'tmux -Lprobe kill-server' 'tmux -S /tmp/x/s kill-server' \
    'tmux -f /dev/null -L p set -g base-index 0' 'tmux -L p source-file x' 'tmux -L p kill-session -t x'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "tmux-server-guard: allows read-only and window or pane scoped calls" {
  for cmd in "tmux display-message -p '#{pid}'" 'tmux rename-window foo' 'tmux kill-window -t @3' \
    'tmux set -w x y' 'tmux set -p x y' 'tmux list-sessions' 'tmux select-pane -t %1' \
    'tmux -f /dev/null new-session -d -s S' 'tmux show-options -g' 'tmux capture-pane -p'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "tmux-server-guard: blocks server-global mutations" {
  for cmd in 'tmux set -g x y' 'tmux set-option -gu x' 'tmux set -ga x y' 'tmux set-hook -g x y' \
    'tmux set-window-option -g x y' 'tmux setw -g x y' 'tmux set -s x y' \
    'tmux source-file x' 'tmux source x' 'tmux kill-session -t x'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block 'tmux'
  done
}

@test "tmux-server-guard: judges each invocation of a compound command" {
  run run_guard <<<"$(pi_bash 'echo hi && tmux kill-server')"
  assert_block 'kill-server'
  run run_guard <<<"$(pi_bash 'tmux -L x kill-server; tmux kill-server')"
  assert_block 'kill-server'
  run run_guard <<<"$(pi_bash 'tmux kill-server; tmux -L x kill-server')"
  assert_block 'kill-server'
  run run_guard <<<"$(pi_bash $'tmux -L x kill-server\ntmux kill-server')"
  assert_block 'kill-server'
}

@test "tmux-server-guard: sees tmux behind prefixes and wrappers" {
  for cmd in 'env -u TMUX tmux kill-server' 'TMUX_TMPDIR=/tmp/x tmux kill-server' \
    'FOO=1 env BAR=2 tmux kill-server' 'timeout 5 tmux kill-server' 'sudo -u me tmux kill-server' \
    '/usr/bin/tmux kill-server' 'command tmux kill-server' 'timeout -s KILL 5 tmux kill-server' \
    'exec -a x tmux kill-server' 'if tmux kill-server; then :; fi' '! tmux kill-server' \
    '{ tmux kill-server; }' 'while tmux kill-server; do :; done' 'until tmux kill-server; do :; done' \
    'if :; then :; elif tmux kill-server; then :; else :; fi'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block 'kill-server'
  done
}

@test "tmux-server-guard: sees through line continuations and here-doc bodies" {
  run run_guard <<<"$(pi_bash $'tmux \\\n  kill-server')"
  assert_block 'kill-server'
  run run_guard <<<"$(pi_bash $'tmux -f /dev/null \\\n set -g x y')"
  assert_block 'set -g'
  run run_guard <<<"$(pi_bash $'cat <<EOF\ntmux kill-server\nEOF')"
  assert_block 'kill-server'
}

@test "tmux-server-guard: a session without CREW_WORKER_ID is allowed unconditionally" {
  unset CREW_WORKER_ID
  run run_guard <<<"$(pi_bash 'tmux kill-server')"
  assert_allow
  run run_guard <<<"$(claude_bash "$PROBE1")"
  assert_allow
}

@test "tmux-server-guard: the deny reason carries the fix" {
  run run_guard <<<"$(claude_bash 'tmux kill-server')"
  assert_block 'unset TMUX'
  assert_block 'TMUX_TMPDIR'
  assert_block '-L'
}

@test "tmux-server-guard: cursor shapes get the cursor deny" {
  run run_guard <<<"$(cursor_shell_exec 'tmux kill-server')"
  [ "$status" -eq 0 ]
  jq -e '.permission == "deny" and (.agent_message | contains("unset TMUX"))' <<<"$output" >/dev/null
  run run_guard <<<"$(cursor_shell_exec 'tmux -L x kill-server')"
  assert_allow
}
