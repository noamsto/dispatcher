# adapters/core/secret-read-guard.sh: per-engine payload parsing (AC2) and the
# incident-ported deny/allow logic (AC1). See spec.md / plan.md #402.

setup() {
  load helpers
  GUARD="$BATS_TEST_DIRNAME/../adapters/core/secret-read-guard.sh"
}

# run_guard — pipe a built payload into the guard, the way every wired hook does.
run_guard() {
  bash -euo pipefail "$GUARD"
}

# ---------------------------------------------------------------------------
# Payload builders — one per wired engine/event shape.
# ---------------------------------------------------------------------------

claude_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"PreToolUse",prompt_id:"p",session_id:"s",tool_name:"Bash",tool_input:{command:$cmd}}'
}

claude_read() { # <path>
  jq -nc --arg p "$1" \
    '{hook_event_name:"PreToolUse",prompt_id:"p",session_id:"s",tool_name:"Read",tool_input:{file_path:$p}}'
}

claude_grep() { # <path> <glob> <pattern> <mode> — empty args are omitted, not sent as ""
  jq -nc --arg path "$1" --arg glob "$2" --arg pattern "$3" --arg mode "$4" \
    '{hook_event_name:"PreToolUse",prompt_id:"p",session_id:"s",tool_name:"Grep",
      tool_input: ({pattern:$pattern}
        + (if $path != "" then {path:$path} else {} end)
        + (if $glob != "" then {glob:$glob} else {} end)
        + (if $mode != "" then {output_mode:$mode} else {} end))}'
}

codex_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"PreToolUse",model:"m",permission_mode:"default",session_id:"s",
      tool_name:"Bash",tool_input:{command:$cmd},tool_use_id:"t",transcript_path:null,
      turn_id:"u",cwd:"/w"}'
}

cursor_pre_shell() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"preToolUse",cursor_version:"2026.09.08",tool_name:"Shell",
      tool_input:{command:$cmd,cwd:"",timeout:30000},cwd:""}'
}

cursor_shell_exec() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"beforeShellExecution",cursor_version:"2026.09.08",command:$cmd,cwd:"",sandbox:false}'
}

cursor_read_file() { # <path>
  jq -nc --arg p "$1" \
    '{hook_event_name:"beforeReadFile",cursor_version:"2026.09.23",file_path:$p,content:"FAKE=1",attachments:[]}'
}

pi_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{engine:"pi",canonical_event:"pre_tool",native_event:"tool_call",session_id:"s",cwd:"/w",
      protocol:"",tool_name:"Bash",tool_input:{command:$cmd},native:{}}'
}

pi_read() { # <path>
  jq -nc --arg p "$1" \
    '{engine:"pi",canonical_event:"pre_tool",native_event:"tool_call",session_id:"s",cwd:"/w",
      protocol:"",tool_name:"Read",tool_input:{path:$p},native:{}}'
}

pi_grep() { # <path> <pattern>
  jq -nc --arg p "$1" --arg pat "$2" \
    '{engine:"pi",canonical_event:"pre_tool",native_event:"tool_call",session_id:"s",cwd:"/w",
      protocol:"",tool_name:"Grep",tool_input:{pattern:$pat,path:$p},native:{}}'
}

# ---------------------------------------------------------------------------
# Assertions — compare the WHOLE object; a wrong or missing hookEventName is a
# silent fail-open on claude (it ignores the deny and lets the call through).
# ---------------------------------------------------------------------------

assert_deny_claude() {
  [ "$status" -eq 0 ]
  printf '%s' "$output" | jq -e '
    (keys == ["hookSpecificOutput"])
    and (.hookSpecificOutput | keys == ["hookEventName","permissionDecision","permissionDecisionReason"])
    and .hookSpecificOutput.hookEventName == "PreToolUse"
    and .hookSpecificOutput.permissionDecision == "deny"
    and (.hookSpecificOutput.permissionDecisionReason | type == "string" and length > 0)
  ' >/dev/null
}

assert_deny_cursor() {
  [ "$status" -eq 0 ]
  printf '%s' "$output" | jq -e '
    (keys == ["agent_message","permission","user_message"])
    and .permission == "deny"
    and (.user_message | type == "string" and length > 0)
    and (.agent_message | type == "string" and length > 0)
  ' >/dev/null
}

assert_allow() {
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

# ---------------------------------------------------------------------------
# AC1 — deny cases (claude Bash unless noted)
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies cat .env" {
  run run_guard <<<"$(claude_bash 'cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies grep KEY .env" {
  run run_guard <<<"$(claude_bash 'grep KEY .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies rg TOKEN .env.local" {
  run run_guard <<<"$(claude_bash 'rg TOKEN .env.local')"
  assert_deny_claude
}

@test "secret-read-guard: denies an unset-default expansion of a secret-shaped var" {
  run run_guard <<<"$(claude_bash 'echo "${X_API_KEY:-x}"')"
  assert_deny_claude
}

@test "secret-read-guard: denies bare env" {
  run run_guard <<<"$(claude_bash 'env')"
  assert_deny_claude
}

@test "secret-read-guard: denies bare printenv" {
  run run_guard <<<"$(claude_bash 'printenv')"
  assert_deny_claude
}

@test "secret-read-guard: denies a dump chained after another command" {
  run run_guard <<<"$(claude_bash 'ls; env')"
  assert_deny_claude
}

@test "secret-read-guard: denies bare set" {
  run run_guard <<<"$(claude_bash 'set')"
  assert_deny_claude
}

@test "secret-read-guard: denies fish set -S NAME" {
  run run_guard <<<"$(claude_bash 'set -S LINEAR_API_KEY')"
  assert_deny_claude
}

@test "secret-read-guard: denies fish set -S NAME piped through a filter" {
  run run_guard <<<"$(claude_bash 'set -S LINEAR_API_KEY | grep -v value')"
  assert_deny_claude
}

@test "secret-read-guard: denies declare -p NAME" {
  run run_guard <<<"$(claude_bash 'declare -p NAME')"
  assert_deny_claude
}

@test "secret-read-guard: denies declare -xp NAME" {
  run run_guard <<<"$(claude_bash 'declare -xp NAME')"
  assert_deny_claude
}

@test "secret-read-guard: denies bare declare" {
  run run_guard <<<"$(claude_bash 'declare')"
  assert_deny_claude
}

@test "secret-read-guard: denies bare declare -x" {
  run run_guard <<<"$(claude_bash 'declare -x')"
  assert_deny_claude
}

@test "secret-read-guard: denies bare typeset" {
  run run_guard <<<"$(claude_bash 'typeset')"
  assert_deny_claude
}

@test "secret-read-guard: denies typeset -p NAME" {
  run run_guard <<<"$(claude_bash 'typeset -p NAME')"
  assert_deny_claude
}

@test "secret-read-guard: denies export -p" {
  run run_guard <<<"$(claude_bash 'export -p')"
  assert_deny_claude
}

@test "secret-read-guard: denies bare export" {
  run run_guard <<<"$(claude_bash 'export')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat /proc/1/environ" {
  run run_guard <<<"$(claude_bash 'cat /proc/1/environ')"
  assert_deny_claude
}

@test "secret-read-guard: denies tmux show-environment" {
  run run_guard <<<"$(claude_bash 'tmux show-environment')"
  assert_deny_claude
}

@test "secret-read-guard: denies a fish -c dump" {
  run run_guard <<<"$(claude_bash "fish -c 'set -S NAME'")"
  assert_deny_claude
}

@test "secret-read-guard: denies a bash -c dump" {
  run run_guard <<<"$(claude_bash 'bash -c "declare -p NAME"')"
  assert_deny_claude
}

@test "secret-read-guard: denies a fish -ic dump" {
  run run_guard <<<"$(claude_bash "fish -ic 'set -gx'")"
  assert_deny_claude
}

@test "secret-read-guard: denies declare -x -p NAME (split flags)" {
  run run_guard <<<"$(claude_bash 'declare -x -p NAME')"
  assert_deny_claude
}

@test "secret-read-guard: denies Read of .env" {
  run run_guard <<<"$(claude_read '.env')"
  assert_deny_claude
}

@test "secret-read-guard: denies Read of /repo/.env.local" {
  run run_guard <<<"$(claude_read '/repo/.env.local')"
  assert_deny_claude
}

@test "secret-read-guard: denies Read of ~/.aws/credentials" {
  run run_guard <<<"$(claude_read '/home/u/.aws/credentials')"
  assert_deny_claude
}

@test "secret-read-guard: denies Read of .netrc" {
  run run_guard <<<"$(claude_read '/home/u/.netrc')"
  assert_deny_claude
}

@test "secret-read-guard: denies Read of an ssh private key" {
  run run_guard <<<"$(claude_read '/home/u/.ssh/id_ed25519')"
  assert_deny_claude
}

@test "secret-read-guard: denies Read of /proc/self/environ" {
  run run_guard <<<"$(claude_read '/proc/self/environ')"
  assert_deny_claude
}

@test "secret-read-guard: denies Grep content-mode over .env" {
  run run_guard <<<"$(claude_grep '.env' '' 'KEY' 'content')"
  assert_deny_claude
}

# ---------------------------------------------------------------------------
# AC1 — allow cases
# ---------------------------------------------------------------------------

@test "secret-read-guard: allows grep -c over .env" {
  run run_guard <<<"$(claude_bash "grep -c '^NAME=' .env")"
  assert_allow
}

@test "secret-read-guard: allows test -f .env" {
  run run_guard <<<"$(claude_bash 'test -f .env')"
  assert_allow
}

@test "secret-read-guard: allows fish set -q NAME" {
  run run_guard <<<"$(claude_bash 'set -q NAME')"
  assert_allow
}

@test "secret-read-guard: allows cat .env.example" {
  run run_guard <<<"$(claude_bash 'cat .env.example')"
  assert_allow
}

@test "secret-read-guard: allows export NAME=v" {
  run run_guard <<<"$(claude_bash 'export NAME=v')"
  assert_allow
}

@test "secret-read-guard: allows declare -x NAME=v" {
  run run_guard <<<"$(claude_bash 'declare -x NAME=v')"
  assert_allow
}

@test "secret-read-guard: allows declare -a arr" {
  run run_guard <<<"$(claude_bash 'declare -a arr')"
  assert_allow
}

@test "secret-read-guard: allows typeset -f" {
  run run_guard <<<"$(claude_bash 'typeset -f')"
  assert_allow
}

@test "secret-read-guard: allows declare -F" {
  run run_guard <<<"$(claude_bash 'declare -F')"
  assert_allow
}

@test "secret-read-guard: allows typeset -A map" {
  run run_guard <<<"$(claude_bash 'typeset -A map')"
  assert_allow
}

@test "secret-read-guard: allows declare -x PATHX=1" {
  run run_guard <<<"$(claude_bash 'declare -x PATHX=1')"
  assert_allow
}

@test "secret-read-guard: allows rg over the words env/printenv in ordinary text" {
  run run_guard <<<"$(claude_bash "rg 'env|printenv' x")"
  assert_allow
}

@test "secret-read-guard: allows bash -c 'set -x' (tracing flag, not a dump)" {
  run run_guard <<<"$(claude_bash "bash -c 'set -x'")"
  assert_allow
}

@test "secret-read-guard: allows git status" {
  run run_guard <<<"$(claude_bash 'git status')"
  assert_allow
}

@test "secret-read-guard: allows Read of README.md" {
  run run_guard <<<"$(claude_read 'README.md')"
  assert_allow
}

@test "secret-read-guard: allows Read of .env.example" {
  run run_guard <<<"$(claude_read '.env.example')"
  assert_allow
}

@test "secret-read-guard: allows Grep files_with_matches over .env" {
  run run_guard <<<"$(claude_grep '.env' '' 'KEY' 'files_with_matches')"
  assert_allow
}

@test "secret-read-guard: allows Grep content over src/ with an unrelated pattern" {
  run run_guard <<<"$(claude_grep 'src/' '' 'foo' 'content')"
  assert_allow
}

# ---------------------------------------------------------------------------
# AC2 — per-engine payload parsing
# ---------------------------------------------------------------------------

@test "secret-read-guard: codex Bash cat .env denies in claude shape" {
  run run_guard <<<"$(codex_bash 'cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: codex Bash ls allows" {
  run run_guard <<<"$(codex_bash 'ls')"
  assert_allow
}

@test "secret-read-guard: cursor preToolUse Shell cat .env denies in cursor shape" {
  run run_guard <<<"$(cursor_pre_shell 'cat .env')"
  assert_deny_cursor
}

@test "secret-read-guard: cursor preToolUse Shell ls allows" {
  run run_guard <<<"$(cursor_pre_shell 'ls')"
  assert_allow
}

@test "secret-read-guard: cursor beforeShellExecution env denies in cursor shape" {
  run run_guard <<<"$(cursor_shell_exec 'env')"
  assert_deny_cursor
}

@test "secret-read-guard: cursor beforeShellExecution ls allows" {
  run run_guard <<<"$(cursor_shell_exec 'ls')"
  assert_allow
}

@test "secret-read-guard: cursor beforeReadFile of .env denies in cursor shape" {
  run run_guard <<<"$(cursor_read_file '/w/.env')"
  assert_deny_cursor
}

@test "secret-read-guard: cursor beforeReadFile of README.md allows" {
  run run_guard <<<"$(cursor_read_file '/w/README.md')"
  assert_allow
}

@test "secret-read-guard: pi Bash env denies in claude shape" {
  run run_guard <<<"$(pi_bash 'env')"
  assert_deny_claude
}

@test "secret-read-guard: pi Bash ls allows" {
  run run_guard <<<"$(pi_bash 'ls')"
  assert_allow
}

@test "secret-read-guard: pi Read of @.env (mention-prefixed) denies" {
  run run_guard <<<"$(pi_read '@.env')"
  assert_deny_claude
}

@test "secret-read-guard: pi Read of ~/.netrc denies" {
  run run_guard <<<"$(pi_read '~/.netrc')"
  assert_deny_claude
}

@test "secret-read-guard: pi Read of an ordinary source file allows" {
  run run_guard <<<"$(pi_read 'src/a.go')"
  assert_allow
}

@test "secret-read-guard: pi Grep of @.env denies (mode forced to content)" {
  run run_guard <<<"$(pi_grep '@.env' 'X')"
  assert_deny_claude
}

@test "secret-read-guard: pi Grep over src/ with an unrelated pattern allows" {
  run run_guard <<<"$(pi_grep 'src/' 'foo')"
  assert_allow
}

@test "secret-read-guard: an unrecognised hook event abstains" {
  payload="$(jq -nc '{hook_event_name:"PostToolUse",tool_name:"Bash",tool_input:{command:"cat .env"}}')"
  run run_guard <<<"$payload"
  assert_allow
}

@test "secret-read-guard: malformed tool_input (non-string command) allows without crashing" {
  payload="$(jq -nc '{hook_event_name:"PreToolUse",prompt_id:"p",session_id:"s",tool_name:"Bash",tool_input:{command:123}}')"
  run run_guard <<<"$payload"
  assert_allow
}
