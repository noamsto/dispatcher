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
@test "tmux-server-guard: a delete after a 6 KB here-doc is judged in under 5 s" {
  runtime_env
  local body='' i start elapsed
  for ((i = 0; i < 140; i++)); do body+="line $i of the here-doc body, plain prose text"$'\n'; done
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
  for ((i = 0; i < 20; i++)); do appends+=" d+=/x$i;"; done
  assert_within appends-runtime 3000 "if :; then d=\$XDG_RUNTIME_DIR;${appends} fi; rm -rf \"\$d\""
  assert_block 'candidate values'
  assert_within appends-tmp 3000 "if :; then d=x;${appends} fi; rm -rf \"/tmp/\$d\""
  assert_block 'candidate values'
  assert_within appends-plain 3000 "d=\$XDG_RUNTIME_DIR;${appends} rm -rf \"\$d\""
  assert_block 'runtime dir'
  assert_within appends-plain-tmp 3000 "d=/tmp;${appends} rm -rf \"\$d\""
  assert_allow
}

# bats test_tags=timing
@test "tmux-server-guard: a cyclic chain among many bindings is refused within the hook timeout" {
  runtime_env
  local pad='' i
  for ((i = 0; i < 1400; i++)); do pad+="z=1;"; done
  assert_within padded-cycle 3000 "${pad}$(doubling_chain 25 '$a0')"
  assert_block 'expansion budget'
}

# A command past the size limit skips the quadratic analysis: a delete or tmux
# verb at command position is refused when its words hold a variable, glob,
# runtime-dir path or a cd came before it, or when it is a tmux server call
# with no -L/-S; prose and other commands' arguments are allowed.
long_command() { # <tail>: filler over the limit, then <tail>
  local filler
  filler=$(printf 'echo plain filler text, not a command of interest\n%.0s' $(seq 200))
  printf '%s\n%s' "$filler" "$1"
}

@test "tmux-server-guard: a command over the size limit with a runtime-dir delete is refused with the size reason" {
  runtime_env
  local tail
  for tail in 'rm -rf "$XDG_RUNTIME_DIR"' 'rm -rf /run/user/1000/tmux-1000' 'gtrash put "$TMUX_TMPDIR"' \
    'find /run/user -delete' 'tmux kill-server' 'tmux set -g base-index 0'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_block 'split the command'
    [[ $output == *'bytes'* ]]
    run run_guard <<<"$(pi_bash "$(long_command "$tail")")"
    echo "pi: $tail"
    assert_block 'split the command'
    run run_guard <<<"$(cursor_shell_exec "$(long_command "$tail")")"
    echo "cursor: $tail"
    [ "$status" -eq 0 ]
    jq -e '.permission == "deny"' <<<"$output" >/dev/null
  done
}

@test "tmux-server-guard: a command over the size limit that deletes nothing at the runtime dir is allowed" {
  runtime_env
  local tail
  for tail in 'echo done' 'rm -rf /tmp/x' 'echo "$XDG_RUNTIME_DIR" firmware format' 'tmux -L s list-sessions' \
    'tmux list-sessions; echo source'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_allow
  done
}

# bats test_tags=timing
@test "tmux-server-guard: the worst case at the size limit is judged in under 2 s" {
  runtime_env
  local unit='echo a > /dev/null 2>&1;' cmd='' tail=$'\n''rm -rf "$XDG_RUNTIME_DIR"'
  while ((${#cmd} + ${#unit} + ${#tail} <= 8192)); do cmd+=$unit; done
  cmd+=$tail
  assert_within at-limit 2000 "$cmd"
  assert_block 'runtime dir'
  cmd=''
  while ((${#cmd} + ${#unit} + ${#tail} <= 8192 + 4096)); do cmd+=$unit; done
  assert_within over-limit 2000 "$cmd$tail"
  assert_block 'split the command'
}

@test "tmux-server-guard: a += chain concatenates instead of fanning out" {
  runtime_env
  local cmd i chain='' rebinds='' paths='' rt_paths=''
  for ((i = 1; i <= 30; i++)); do chain+=" p+=/s$i;"; done
  for ((i = 1; i <= 17; i++)); do rebinds+=" p=/tmp/r$i;"; done
  for ((i = 1; i <= 20; i++)); do paths+=" /tmp/d$i"; done
  for ((i = 1; i <= 19; i++)); do rt_paths+=" /tmp/d$i"; done
  for cmd in 'p=/tmp; p+=/s1; p+=/s2; p+=/s3; p+=/s4; rm -rf "$p"' "p=/tmp;${chain} rm -rf \"\$p\"" \
    "${rebinds} rm -rf \"\$p\"" "for d in${paths}; do rm -rf \"\$d\"; done" \
    'p=/tmp; p+=/s1; p+=/s1; p=/tmp; p+=/s1; rm -rf "$p"' 'for d in /tmp/a /tmp/a /tmp/a; do rm -rf "$d"; done'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: ${cmd:0:80}"
    assert_allow
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
  for cmd in 'p=/tmp; p+=/s1; p+=/s2; p+=/s3; p+=/s4; p=/run/user/1000; rm -rf "$p"' \
    'p=/run; p+=/user; p+=/1000; rm -rf "$p"' 'p=$XDG_RUNTIME_DIR; p+=/s1; p+=/s2; p+=/s3; p+=/s4; rm -rf "$p"' \
    'p=/tmp; p+=/s1; p+=/s2; p=/ru; p+=n/user/1000; rm -rf "$p"' 'p+=/run/user/1000; p+=/x; rm -rf "$p"' \
    "${rebinds} p=/run/user/1000; rm -rf \"\$p\"" "for d in${rt_paths} /run/user/1000; do rm -rf \"\$d\"; done"; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: ${cmd:0:80}"
    assert_block 'runtime dir'
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block 'runtime dir'
  done
}

@test "tmux-server-guard: a name past the list cap keeps its own reason" {
  runtime_env
  local rebinds='' i
  for ((i = 1; i <= 70; i++)); do rebinds+=" p=/tmp/r$i;"; done
  run run_guard <<<"$(claude_bash "${rebinds} rm -rf \"\$p\"")"
  assert_block 'candidate values'
}

# Candidates dropped by a cap or hidden by a binding that does not run in order
# must still be refused: each reached the runtime dir before the dedupe and the
# in-order replay existed.
@test "tmux-server-guard: a deduped overflow, a prefix, an argument or an unset never hides a runtime-dir value" {
  runtime_env
  local cmd i dup='' rebinds='' same='' appends=''
  for ((i = 0; i < 65; i++)); do dup+=' /tmp/a'; done
  for ((i = 0; i < 70; i++)); do rebinds+=' p=/tmp/a;'; done
  for ((i = 0; i < 33; i++)); do same+=' p+=/a; p=/tmp;'; done
  for ((i = 0; i < 6; i++)); do appends+=' d+=/x;'; done
  for cmd in "for d in${dup} /run/user/1000; do rm -rf \"\$d\"; done" "${rebinds} p=/run/user/1000; rm -rf \"\$p\"" \
    "p=/tmp;${same} p=/run/user/1000; p+=/x; rm -rf \"\$p\"" "if c; then d=/tmp/a;${appends} else d+=/run/user/1000; fi; rm -rf \"\$d\"" \
    'echo d=/tmp; d+=/run/user/1000; rm -rf "$d"' 'd=/tmp/x; unset d; d+=/run/user/1000; rm -rf "$d"' \
    'd=/tmp/x true; d+=/run/user/1000; rm -rf "$d"' 'p=/ru; echo p=/tmp; p+=n/user/1000; rm -rf "$p"' \
    $'cat <<EOF >/dev/null\nd=/tmp\nEOF\nd+=/run/user/1000\nrm -rf "$d"'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: ${cmd:0:90}"
    [ "$status" -eq 0 ]
    jq -e '.hookSpecificOutput.permissionDecision == "deny"' <<<"$output" >/dev/null
  done
}

@test "tmux-server-guard: a += chain stays a chain beside redirections and braces" {
  runtime_env
  local cmd
  for cmd in 'p=/tmp; p+=/s1; p+=/s2; p+=/s3; p+=/s4; p+=/s5; p+=/s6; p+=/s7; rm -rf "$p" 2>&1' \
    'p=/tmp; p+=/s1; p+=/s2; p+=/s3; p+=/s4; p+=/s5; p+=/s6; p+=/s7; rm -rf "${p}" &>/dev/null'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: $cmd"
    assert_allow
  done
}

@test "tmux-server-guard: the size gate reads quotes, ancestors and every delete and tmux form" {
  runtime_env
  local tail
  for tail in 'find "$XDG_RUNTIME_DIR" -name x -delete; echo ok' 'find /run/user/1000 -delete&& echo ok' \
    'tmux source ~/.tmux.conf' 'tmux set -t main -g status off' 'tmux setw -t main:1 -g mode-keys vi' \
    'rm -rf /run//user/1000' 'rm -rf "/run"/user/1000' 'rm -rf /run' 'rm -rf "/run/"'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_block 'split the command'
  done
  for tail in 'source ~/.bashrc; tmux list-sessions' 'rm -rf /tmp/run/x' 'tmux set -t main status off'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_allow
  done
}

# Products and lists are sized before they are built: these ran past 4 s, which
# fails open, when they were built and then deduped.
# bats test_tags=timing
@test "tmux-server-guard: a product of two 64-value names is refused within the hook timeout" {
  runtime_env
  local cmd='' i q
  q=$(printf 'q%.0s' $(seq 100))
  for ((i = 0; i < 63; i++)); do cmd+="a=/tmp/$q$i;"; done
  assert_within product-a-a 2000 "${cmd}a=/run/user/1000; rm -rf \"\$a\$a\""
  assert_block 'candidate values'
  cmd=''
  for ((i = 0; i < 64; i++)); do cmd+="a=/tmp/${q:0:30}$i;b=/tmp/${q:0:30}$i;"; done
  assert_within product-a-b 2000 "${cmd}x=\$a/\$b; rm -rf \"\$x\" /run/user/1000"
  assert_block 'candidate values'
  assert_within append-product 2000 "${cmd}d=\$a; d+=\$b; rm -rf \"\$d\" /run/user/1000"
  assert_block 'candidate values'
}

@test "tmux-server-guard: a quoted, escaped or commented decoy binding never replaces the base of an append" {
  runtime_env
  local cmd
  for cmd in 'd=/run/us; echo \; d=/tmp/; d+=er/1000; rm -rf "$d"' $'d=/run/us; echo \\\nd=/tmp/; d+=er/1000; rm -rf "$d"' \
    $'d=/run/us; echo "\nd=/tmp/\n"; d+=er/1000; rm -rf "$d"' $'d=/run/us\n#;d=/tmp/\nd+=er/1000; rm -rf "$d"' \
    'd=/run/us; echo ";d=/tmp/;"; d+=er/1000; rm -rf "$d"' 'echo \; d=/tmp/; d+=$XDG_RUNTIME_DIR; rm -rf "${d#/tmp/}"'; do
    run run_guard <<<"$(claude_bash "$cmd")"
    echo "claude: $cmd"
    assert_block 'runtime dir'
  done
}

@test "tmux-server-guard: the size gate folds dot segments and backslashes" {
  runtime_env
  local tail
  for tail in 'rm -rf /./run/user/1000' 'rm -rf /tmp/../run/user/1000' 'rm -rf /run/./user/1000' \
    'r\m -rf /run/user/1000' 'rm -rf /run/us\er/1000' 't\mux kill-server' 'rm -rf /run/./'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_block 'split the command'
  done
}

# Each value a step handles is charged to the step budget: 64 values of 500
# characters per step otherwise outran the hook timeout.
# bats test_tags=timing
@test "tmux-server-guard: many appends or operands over 64-value names are refused within the hook timeout" {
  runtime_env
  local cmd='' i p q
  p=$(printf 'a%.0s' $(seq 490))
  cmd="p=/$p;"
  for ((i = 10; i < 74; i++)); do cmd+="q=/$i;"; done
  cmd+='d=$p$q;'
  for ((i = 0; i < 1000; i++)); do cmd+='d+=;'; done
  assert_within empty-appends 2000 "$cmd rm -rf \"\$d\" /run/user/1000"
  assert_block 'rm -rf'
  cmd=''
  q=$(printf '/s%.0s' $(seq 120))
  for ((i = 0; i < 64; i++)); do cmd+="a=$q$i;"; done
  assert_within many-operands 2000 "${cmd}rm -rf$(printf ' "$a"%.0s' $(seq 30)) /run/user/1000"
  [ "$status" -eq 0 ]
  jq -e '.hookSpecificOutput.permissionDecision == "deny"' <<<"$output" >/dev/null
}

@test "tmux-server-guard: the size gate refuses globbed, split, relative and rebound runtime-dir deletes" {
  runtime_env
  local tail
  for tail in 'rm -rf ru?/user/1000' 'rm -rf */user/1000' 'rm -rf ru*/*/1000' 'd=/ru; rm -rf "$d"n/user/1000' \
    'rm -rf var/run/user/1000' 'rm -rf proc/self/root/run/user/1000' $'cd /\nrm -rf run/user/1000' \
    'cd /; rm -rf run' 'tmp=$(mktemp -d); rm -rf "$tmp"' 'sudo rm -rf /run/user/1000' 'env -u TMUX tmux kill-server' \
    'x=1 command rm -rf "${XDG_RUNTIME_DIR}"' 'find / -delete' 'rm -rf /' 'echo ok && rm -rf /run/user/1000' \
    'for d in a b; do rm -rf "$d"; done' $'rm -rf /run/user/1000 \\\n  /tmp/x' \
    $'cat <<EOF\nbody\nEOF\nrm -rf /run/user/1000' $'cat <<\'EOF\'\nit\'s prose\nEOF\nrm -rf /run/user/1000' \
    $'echo "it\'s"; rm -rf /run/user/1000' $'rm -rf $\'a\\\'b\' /run/user/1000'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_block 'split the command'
  done
}

@test "tmux-server-guard: the size gate reads prose, other commands' arguments and private servers as allowed" {
  runtime_env
  local tail prose
  prose=$(printf 'The guard denies a tmux call such as kill-session, and rm of XDG_RUNTIME_DIR.\n%.0s' 1 2 3)
  for tail in $'cat <<EOF\n'"$prose"$'\nEOF\necho done' \
    "git commit -m \"fix(guard): $prose"$'\nrm -rf /run/user/1000 is what it stops"' \
    'tmux -L scratch kill-server' 'tmux -S /tmp/sock kill-server' 'tmux -f /dev/null -L s kill-server' \
    'tmux -LS kill-server' 'rm -f build/out.o' 'rm -rf /tmp/x /tmp/y' \
    'echo rm -rf "$XDG_RUNTIME_DIR" tmux kill-server' 'grep -n "rm -rf" notes.txt' \
    'tmux display-message "tmux kill-server"' $'cat <<EOF\nit\'s prose\nEOF\necho done' \
    "echo \"it's\" # tmux kill-server" 'tmux set -t main status off; rm -f a.o' 'tmux -L s kill-server 2>&1' \
    $'jq -r .x <<<"$json"\ngit commit -m "fix(guard): stop deletes\ntmux kill-server is what it stops"' \
    $'cat > /tmp/s.sh <<\'XEOF\'\nd=$(mktemp -d)\nrm -rf "$d"\nXEOF\necho written'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_allow
  done
}

@test "tmux-server-guard: the size gate reads here-strings, redirections, shell here-docs and quoting like the analyzer" {
  runtime_env
  local tail
  for tail in $'jq -r . <<<"$json"\nrm -rf /tmp/build \\\n  "$XDG_RUNTIME_DIR/tmux-1000"' \
    $'echo $((1<<2))\nrm -rf /tmp/build \\\n  /run/user/1000' $'wc -l <<< "$out"\nrm -rf \\\n  "$TMUX_TMPDIR"' \
    $'jq -r .id <<<"$json"\ntmux -f /dev/null \\\n  kill-server' \
    '2>/dev/null rm -rf "$XDG_RUNTIME_DIR/tmux-1000"' '</dev/null rm -rf /run/user/1000/tmux-1000' \
    'sudo 2>/dev/null rm -rf /run/user/1000' '>/dev/null tmux kill-server' 'tmux 2>/dev/null kill-server' \
    'tmux -f /dev/null >/dev/null set -g mouse on' 'rm -rf 2>&1 /run/user/1000' 'rm -rf /run/x/../user' \
    $'bash <<EOF\nrm -rf /tmp/a \\\n  /run/user/1000\nEOF' $'cat <<E"OF"\nbody\nEOF\nrm -rf /tmp/a \\\n  /run/user/1000' \
    $'echo "$"\'\\\'; rm -rf /run/user/1000'; do
    run run_guard <<<"$(claude_bash "$(long_command "$tail")")"
    echo "claude: $tail"
    assert_block 'split the command'
  done
}

# bats test_tags=timing
@test "tmux-server-guard: one very long line is judged within the hook timeout" {
  runtime_env
  assert_within long-quoted 2000 "echo \"$(printf 'a%.0s' $(seq 65536))\""
  assert_allow
  assert_within long-words 2000 "echo $(printf 'a %.0s' $(seq 32768))"
  assert_allow
}
