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

# --- deletes that reach the per-user runtime dir ---------------------------
# Every command below is a string fed to the guard; nothing is executed.

codex_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"PreToolUse",session_id:"s",turn_id:"t",tool_name:"Bash",tool_input:{command:$cmd},cwd:"/tmp"}'
}

assert_cursor_block() { # <substring of the reason>
  [ "$status" -eq 0 ]
  jq -e --arg w "$1" '.permission == "deny" and (.agent_message | contains($w))' <<<"$output" >/dev/null
}

runtime_env() {
  export XDG_RUNTIME_DIR=/run/user/1000 TMUX_TMPDIR=/run/user/1000
}

@test "tmux-server-guard: refuses deletes that reach the runtime dir (claude and pi)" {
  runtime_env
  local cmd
  for cmd in 'rm -rf "$TMUX_TMPDIR"' 'rm -rf /run/user/$(id -u)' 'rm -rf "/run/user/$(id -u)"' \
    'rm -rf "$XDG_RUNTIME_DIR"' 'rm -rf ${XDG_RUNTIME_DIR}' 'rmdir "$XDG_RUNTIME_DIR"' \
    'unlink "$XDG_RUNTIME_DIR/bus"' 'trash-put "$XDG_RUNTIME_DIR"' 'gtrash put "$TMUX_TMPDIR"' \
    'find "$XDG_RUNTIME_DIR" -name x -delete' 'find -L /run/user/1000 -delete' \
    'rm -rf "$XDG_RUNTIME_DIR/bus"' 'rm -rf "$TMUX_TMPDIR/tmux-$(id -u)"' 'rm -rf /run/user' \
    'rm -rf /run' 'rm -rf /run/user/$(whoami)' 'rm -rf "/run/user/$(whoami)"' 'rm -rf /run/user/*' \
    'rm -rf "$XDG_RUNTIME_DIR"/*' 'rm -rf /run/user/1000/../1000/' 'sudo rm -rf /run/user/1000' \
    'command rm -r -- /run/user/1000' 'X=/run/user/1000; rm -rf "$X"' \
    'cd /tmp && rm -rf "$XDG_RUNTIME_DIR"' \
    'XDG_RUNTIME_DIR=$(mktemp -d) dbus-run-session -- true; rm -rf "$XDG_RUNTIME_DIR"' \
    'XDG_RUNTIME_DIR=/tmp/xdg-test Hyprland; rm -rf "$XDG_RUNTIME_DIR"' \
    'env XDG_RUNTIME_DIR=/tmp/x foo; rm -rf "$XDG_RUNTIME_DIR"' \
    'TMUX_TMPDIR=/tmp/t tmux -L t new -d; rm -rf "$TMUX_TMPDIR"' \
    'rm -rf "${XDG_RUNTIME_DIR:?}/tmux-1000"' 'rm -rf "${XDG_RUNTIME_DIR:?}"' \
    'rm -rf "${XDG_RUNTIME_DIR?}"' 'rm -rf "${XDG_RUNTIME_DIR=x}"' \
    'd=$XDG_RUNTIME_DIR; rm -rf "$d"' 'd=${XDG_RUNTIME_DIR}/tmux-1000; rm -rf "$d"' \
    'export D=$XDG_RUNTIME_DIR; rm -rf "$D"' \
    'XDG_RUNTIME_DIR=$(cd /tmp && mktemp -d) X; rm -rf "$XDG_RUNTIME_DIR"' \
    'XDG_RUNTIME_DIR=$(mktemp -d || exit 1) Hyprland; rm -rf "$XDG_RUNTIME_DIR"' \
    'XDG_RUNTIME_DIR=/tmp/xdg-`whoami` cmd; rm -rf "$XDG_RUNTIME_DIR"' \
    '(export XDG_RUNTIME_DIR=$(mktemp -d); Hyprland); rm -rf "$XDG_RUNTIME_DIR"' \
    'XDG_RUNTIME_DIR=/tmp/x | true; rm -rf "$XDG_RUNTIME_DIR"' \
    'XDG_RUNTIME_DIR=/tmp/x & rm -rf "$XDG_RUNTIME_DIR"' \
    'echo $(XDG_RUNTIME_DIR=/tmp/x); rm -rf "$XDG_RUNTIME_DIR"' \
    '[ -d "$XDG_RUNTIME_DIR" ] || XDG_RUNTIME_DIR=$(mktemp -d); rm -rf "$XDG_RUNTIME_DIR"' \
    'rm -rf "${unset:-$XDG_RUNTIME_DIR}"' 'rm -rf "${a:-${XDG_RUNTIME_DIR}}"' \
    'X=/run/$(echo user)/1000; rm -rf "$X"' 'd=$d/x; d=$XDG_RUNTIME_DIR; rm -rf "$d"' \
    'export XDG_RUNTIME_DIR=$(mktemp -d); rm -rf "$XDG_RUNTIME_DIR"' \
    'export TMUX_TMPDIR=$(mktemp -d); rm -rf "$TMUX_TMPDIR"'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: $cmd"
    assert_block 'runtime dir'
    run run_guard <<<"$(pi_bash "$cmd")"
    echo "pi: $cmd"
    assert_block 'runtime dir'
  done
}

@test "tmux-server-guard: refuses a delete of a prefix-stripped \$TMUX" {
  runtime_env
  export TMUX=/run/user/1000/tmux-1000/default,1,0
  run run_guard <<<"$(claude_bash 'rm -rf "${TMUX%%,*}"')"
  assert_block 'runtime dir'
}

@test "tmux-server-guard: refuses a runtime-dir delete under POSIXLY_CORRECT" {
  runtime_env
  export POSIXLY_CORRECT=1
  run run_guard <<<"$(claude_bash 'rm -rf "$XDG_RUNTIME_DIR"')"
  assert_block 'runtime dir'
}

@test "tmux-server-guard: refuses a runtime-dir delete in the codex and cursor shapes" {
  runtime_env
  run run_guard <<<"$(codex_bash 'rm -rf "$XDG_RUNTIME_DIR"')"
  assert_block 'runtime dir'
  run run_guard <<<"$(cursor_shell_exec 'rm -rf "$XDG_RUNTIME_DIR"')"
  assert_cursor_block 'runtime dir'
}

@test "tmux-server-guard: the delete deny reason carries the fix" {
  runtime_env
  run run_guard <<<"$(claude_bash 'rm -rf "$TMUX_TMPDIR"')"
  assert_block "'\$TMUX_TMPDIR'"
  assert_block 'mktemp -d'
  assert_block 'D-Bus'
  assert_block 'name you created'
  assert_block 'd=$(mktemp -d); rm -rf "$d"'
}

@test "tmux-server-guard: allows deletes it cannot tie to the runtime dir" {
  runtime_env
  local cmd
  for cmd in 'd=$(mktemp -d); touch "$d/x"; rm -rf "$d"' \
    'rm -rf "$unsetvar/"' 'for d in a b; do rm -rf "$d/"; done' \
    'rm -rf "$1"' 'rm -rf "$shape/x"' 'rm -rf "$command"' 'rm -rf /tmp/x /home/u/scratch' 'rm -rf build' \
    "find . -name '*.o' -delete" 'find "$XDG_RUNTIME_DIR" -name x' 'gtrash restore "$XDG_RUNTIME_DIR"' \
    'echo rm -rf "$XDG_RUNTIME_DIR"' 'ls /run/user/1000' \
    'd=`mktemp -d`; rm -rf "$d/"' 'd=$(mktemp -d); rm -rf "$d"/*'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: $cmd"
    assert_allow
    run run_guard <<<"$(pi_bash "$cmd")"
    echo "pi: $cmd"
    assert_allow
  done
}

# The env a dispatched worker launches with: a private TMUX_TMPDIR.
worker_env() {
  export XDG_RUNTIME_DIR=/run/user/1000 TMUX_TMPDIR="$BATS_TEST_TMPDIR/priv"
}

@test "tmux-server-guard: allows deleting private scratch dirs in the launched env" {
  worker_env
  local cmd
  for cmd in 'unset TMUX; export TMUX_TMPDIR=$(mktemp -d); tmux -L x new-session -d; tmux -L x kill-server; rm -rf "$TMUX_TMPDIR"' \
    'read -r TMUX_TMPDIR <f; rm -rf "$TMUX_TMPDIR"' 'd=$(mktemp -d); touch "$d/x"; rm -rf "$d"' \
    'd=$(mktemp -d | tee log); rm -rf "$d"' 'TMUX_TMPDIR=$(mktemp -d | tail -1); rm -rf "$TMUX_TMPDIR"' \
    'd=$(mktemp -d; true); rm -rf "$d"' 'rt=$(mktemp -d); XDG_RUNTIME_DIR=$rt Hyprland; rm -rf "$rt"' \
    '[ -d "$X" ] || X=$(mktemp -d); rm -rf "$X"' 'd=`mktemp -d`; rm -rf "$d/"'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: $cmd"
    assert_allow
    run run_guard <<<"$(pi_bash "$cmd")"
    echo "pi: $cmd"
    assert_allow
  done
}

@test "tmux-server-guard: allows deleting a private TMUX_TMPDIR from the hook env" {
  export XDG_RUNTIME_DIR=/run/user/1000 TMUX_TMPDIR="$BATS_TEST_TMPDIR/priv"
  run run_guard <<<"$(claude_bash 'rm -rf "$TMUX_TMPDIR"')"
  assert_allow
}

@test "tmux-server-guard: a runtime-dir delete without CREW_WORKER_ID is allowed" {
  runtime_env
  unset CREW_WORKER_ID
  run run_guard <<<"$(claude_bash 'rm -rf "$XDG_RUNTIME_DIR"')"
  assert_allow
}

@test "tmux-server-guard: the uid pre-pass keeps the tmux check intact" {
  runtime_env
  run run_guard <<<"$(claude_bash 'echo $(tmux kill-server)')"
  assert_block 'kill-server'
  run run_guard <<<"$(claude_bash 'echo $(id -u) tmux kill-server')"
  assert_allow
}

# bats test_tags=timing
@test "tmux-server-guard: a delete after a 20 KB here-doc is judged in under 5 s" {
  runtime_env
  local body='' i start elapsed
  for ((i = 0; i < 400; i++)); do body+="line $i of the here-doc body, plain prose text"$'\n'; done
  start=$(date +%s%N)
  run run_guard <<<"$(claude_bash "cat <<'EOF' >/dev/null"$'\n'"${body}EOF"$'\n''rm -rf "$XDG_RUNTIME_DIR"')"
  elapsed=$((($(date +%s%N) - start) / 1000000))
  echo "elapsed ${elapsed}ms" >&2
  assert_block 'runtime dir'
  [ "$elapsed" -lt 5000 ]
}

@test "tmux-server-guard: refuses runtime-dir deletes through quoted defaults, quoted assignments, arrays, += and for variables" {
  runtime_env
  local cmd
  for cmd in 'd=${D:-"$XDG_RUNTIME_DIR"}; rm -rf "$d"' ": \${D:='/run/user/1000'}; rm -rf \"\$D\"" \
    'export "D=$XDG_RUNTIME_DIR"; rm -rf "$D"' "export 'D=/run/user/1000'; echo 'x'; rm -rf \"\$D\"" \
    'd=; d+=/run/user/1000; rm -rf "$d"' 'd=/run/user; d+=/1000; rm -rf "$d"' \
    'a=("$XDG_RUNTIME_DIR"); rm -rf "${a[@]}"' 'a=(x "$XDG_RUNTIME_DIR"); rm -rf "${a[1]}"' \
    'a=(); a+=(/run/user/1000); rm -rf "$a"' 'dirs[0]="$XDG_RUNTIME_DIR/tmux-1000"; rm -rf "${dirs[0]}"' \
    'a[0]=/run/user/1000; rm -rf "$a"' 'export "D"=/run/user/1000; rm -rf "$D"' 'export D"=$XDG_RUNTIME_DIR"; rm -rf "$D"' \
    'a=(x /run/user/1000); rm -rf "${a[@]:1}"' 'd=$XDG_RUNTIME_DIR; rm -rf "${d:0}"' 'd=$XDG_RUNTIME_DIR; rm -rf "${d//x/y}"' \
    $'x="a\nb"; x=$XDG_RUNTIME_DIR; rm -rf ./"$x" "$x"' \
    'if [ -n "$X" ]; then d+=/cache; fi; d+=$XDG_RUNTIME_DIR; rm -rf "$d"' 'false && d+=/x; d+=/run/user/1000; rm -rf "$d"' \
    '(d+=/x); d+=/run/user/1000; rm -rf "$d"' 'd=/tmp; d+=/x; d=/ru; d+=n/user/1000; rm -rf "$d"' \
    'for s in lock /tmp user; do rm -rf "/run/$s"; done' 'a=(/x run); rm -rf "/${a[1]}/user/1000"' \
    'a=(/tmp/x); a+=(run); rm -rf "/${a[1]}/user/1000"' 'd=/tmp; for d in run; do rm -rf "/$d/user/1000"; done' \
    'd=$XDG_RUNTIME_DIR; rm -rf "${d:x}"' 'd=$XDG_RUNTIME_DIR; n=0; rm -rf "${d:n}"' 'd=$XDG_RUNTIME_DIR; rm -rf "${d:(0)}"' \
    'dirs=(); dirs[${#dirs[@]}]="$XDG_RUNTIME_DIR/tmux-1000"; rm -rf "${dirs[@]}"' 'a[b[1]]=/run/user/1000; rm -rf "$a"' \
    'x[0] y[1]=/run/user/1000; rm -rf "$y"' 'd+=/x rm -rf /run/user/1000' \
    'for d in "$XDG_RUNTIME_DIR"/tmux-*; do rm -rf "$d"; done' \
    $'for d in a /run/user/1000\ndo rm -rf "$d"; done' \
    'git commit -m "fix \"x\""; rm -rf /run/user/1000' 'echo "a \\" ; rm -rf /run/user/1000'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: $cmd"
    assert_block 'runtime dir'
    run run_guard <<<"$(pi_bash "$cmd")"
    echo "pi: $cmd"
    assert_block 'runtime dir'
  done
}

@test "tmux-server-guard: allows deletes through the same constructs when no value reaches the runtime dir" {
  runtime_env
  local cmd
  for cmd in 'for d in a b; do rm -rf "$d/"; done' 'a=(x y); rm -rf "${a[@]}"' \
    'export "D=/tmp/x"; rm -rf "$D"' 'd=${D:-"/tmp/x"}; rm -rf "$d"' 'd=/tmp; d+=/x; rm -rf "$d"' \
    'git commit -m "fix \"x\""; rm -rf /tmp/x' 'for d in $(ls "$XDG_RUNTIME_DIR"); do rm -rf "$d"; done' \
    'd=$(mktemp -d); d+=/x; rm -rf "$d"' 'files=(a b c d e f g h i j k l m n o p q); rm -f "${files[@]}"' \
    'for f in a.o b.o c.o d.o e.o f.o g.o h.o i.o j.o k.o l.o m.o n.o o.o p.o q.o; do rm -f "$f"; done'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: $cmd"
    assert_allow
  done
}

@test "tmux-server-guard: the candidate cap has its own reason" {
  runtime_env
  run run_guard <<<"$(claude_bash 'rm -rf "${a:-x}${b:-x}${c:-x}${d:-x}${e:-x}"')"
  assert_block 'candidate values'
  [[ $output != *'resolves to the per-user'* ]]
}

# A chain a0=$a1/$a1 … doubles its value per level: unmemoised it ran for
# minutes, past the hook timeout (4 s pi), which fails open.
doubling_chain() { # <levels> <leaf>
  local i out=''
  for ((i = 0; i < $1; i++)); do out+="a$i=\$a$((i + 1))/\$a$((i + 1)); "; done
  printf '%s' "${out}a$1=$2; rm -rf \"\$a0\""
}

assert_within() { # <name> <ms> <command>
  local start elapsed
  start=$(date +%s%N)
  run run_guard <<<"$(claude_bash "$3")"
  elapsed=$((($(date +%s%N) - start) / 1000000))
  echo "$1: elapsed ${elapsed}ms" >&2
  [ "$elapsed" -lt "$2" ]
}

# bats test_tags=timing
@test "tmux-server-guard: a doubling variable chain is refused within the hook timeout" {
  runtime_env
  assert_within acyclic 3000 "$(doubling_chain 12 /tmp)"
  assert_block 'characters'
  assert_within runtime 3000 "$(doubling_chain 12 '$XDG_RUNTIME_DIR')"
  assert_block 'characters'
}

# bats test_tags=timing
@test "tmux-server-guard: a cyclic doubling chain exhausts the step budget and is refused" {
  runtime_env
  assert_within cyclic 3000 "$(doubling_chain 25 '$a0')"
  assert_block 'expansion budget'
}

@test "tmux-server-guard: a candidate over the length cap has its own reason" {
  runtime_env
  run run_guard <<<"$(claude_bash "rm -rf /tmp/$(printf 'a%.0s' $(seq 600))")"
  assert_block 'characters'
  [[ $output != *'resolves to the per-user'* ]]
}

# bats test_tags=timing
@test "tmux-server-guard: a long run of += appends is refused within the hook timeout" {
  runtime_env
  local appends='' i
  for ((i = 0; i < 20; i++)); do appends+=' d+=/x;'; done
  assert_within appends-runtime 3000 "d=\$XDG_RUNTIME_DIR;${appends} rm -rf \"\$d\""
  assert_block 'candidate values'
  assert_within appends-tmp 3000 "d=x;${appends} rm -rf \"/tmp/\$d\""
  assert_block 'candidate values'
}

# bats test_tags=timing
@test "tmux-server-guard: a cyclic chain among many bindings is refused within the hook timeout" {
  runtime_env
  local pad='' i
  for ((i = 0; i < 1500; i++)); do pad+="z$i=1; "; done
  assert_within padded-cycle 3000 "${pad}$(doubling_chain 25 '$a0')"
  assert_block 'expansion budget'
}
