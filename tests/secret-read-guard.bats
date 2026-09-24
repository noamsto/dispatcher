bats_require_minimum_version 1.5.0 # `run --separate-stderr`

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

# cursor preToolUse Read/Grep keys: createToolInput in the cursor-agent
# 2026.09.23 bundle.
cursor_pre_read() { # <path>
  jq -nc --arg p "$1" \
    '{hook_event_name:"preToolUse",cursor_version:"2026.09.23",tool_name:"Read",tool_input:{file_path:$p},cwd:""}'
}

cursor_pre_grep() { # <path> <pattern> <mode> — empty mode is omitted
  jq -nc --arg p "$1" --arg pat "$2" --arg mode "$3" \
    '{hook_event_name:"preToolUse",cursor_version:"2026.09.23",tool_name:"Grep",
      tool_input: ({pattern:$pat,file_path:$p} + (if $mode != "" then {output_mode:$mode} else {} end)),cwd:""}'
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

# ---------------------------------------------------------------------------
# cursor preToolUse Read/Grep (keys from the shipped bundle)
# ---------------------------------------------------------------------------

@test "secret-read-guard: cursor preToolUse Grep content over .env denies in cursor shape" {
  run run_guard <<<"$(cursor_pre_grep '/repo/.env' '.' 'content')"
  assert_deny_cursor
}

@test "secret-read-guard: cursor preToolUse Grep over .env with no output_mode denies" {
  run run_guard <<<"$(cursor_pre_grep '/repo/.env' '.' '')"
  assert_deny_cursor
}

@test "secret-read-guard: cursor preToolUse Grep over src/ with an unrelated pattern allows" {
  run run_guard <<<"$(cursor_pre_grep 'src/' 'foo' '')"
  assert_allow
}

@test "secret-read-guard: cursor preToolUse Read of .env denies in cursor shape" {
  run run_guard <<<"$(cursor_pre_read '/w/.env')"
  assert_deny_cursor
}

@test "secret-read-guard: cursor preToolUse Read of a source file allows" {
  run run_guard <<<"$(cursor_pre_read '/w/a.go')"
  assert_allow
}

# ---------------------------------------------------------------------------
# Large commands stay well inside hookyard's 4 s budget (a timeout is an allow)
# ---------------------------------------------------------------------------

# assert_deny_within <max-ms> <payload> — deny in claude shape, fast enough.
assert_deny_within() {
  local start elapsed
  start=$(date +%s%N)
  run run_guard <<<"$2"
  elapsed=$((($(date +%s%N) - start) / 1000000))
  assert_deny_claude
  echo "elapsed ${elapsed}ms" >&2
  [ "$elapsed" -lt "$1" ]
}

@test "secret-read-guard: a 100 KB heredoc followed by a dump denies in under 2 s" {
  local body
  body=$(printf "a 'b' \"c\"\n%.0s" $(seq 1 10240))
  assert_deny_within 2000 "$(claude_bash "cat > f <<'X'"$'\n'"$body"$'\n'"X"$'\n'"env")"
}

@test "secret-read-guard: a 100 KB bash -c body ending in a dump denies in under 2 s" {
  local body
  body=$(printf 'echo hi; %.0s' $(seq 1 10240))
  assert_deny_within 2000 "$(claude_bash "bash -c '${body}env'")"
}

# ---------------------------------------------------------------------------
# Unparseable payloads fail open but loud: exit 1, stderr, nothing on stdout
# ---------------------------------------------------------------------------

@test "secret-read-guard: a non-JSON payload exits 1 with a stderr notice and no stdout" {
  run --separate-stderr run_guard <<<'not json {'
  [ "$status" -eq 1 ]
  [ -z "$output" ]
  [[ $stderr == *secret-read-guard* ]]
}

@test "secret-read-guard: a host without jq exits 1 with a stderr notice and no stdout" {
  local bin="$BATS_TEST_TMPDIR/bin" tool
  mkdir -p "$bin"
  for tool in bash cat grep sed awk mktemp rm; do
    ln -s "$(command -v "$tool")" "$bin/$tool"
  done
  local bash_path
  bash_path=$(command -v bash)
  payload="$(claude_bash 'cat .env')"
  run --separate-stderr env PATH="$bin" "$bash_path" "$GUARD" <<<"$payload"
  [ "$status" -eq 1 ]
  [ -z "$output" ]
  [[ $stderr == *"jq not found"* ]]
}

# ---------------------------------------------------------------------------
# Grep glob branch
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies Grep content with glob .env*" {
  run run_guard <<<"$(claude_grep '' '.env*' 'KEY' 'content')"
  assert_deny_claude
}

@test "secret-read-guard: denies Grep content with glob *.pem" {
  run run_guard <<<"$(claude_grep '' '*.pem' 'BEGIN' 'content')"
  assert_deny_claude
}

@test "secret-read-guard: allows Grep content with glob *.go" {
  run run_guard <<<"$(claude_grep '' '*.go' 'foo' 'content')"
  assert_allow
}

@test "secret-read-guard: allows Grep files_with_matches with glob .env*" {
  run run_guard <<<"$(claude_grep '' '.env*' 'KEY' 'files_with_matches')"
  assert_allow
}

# ---------------------------------------------------------------------------
# printenv NAME for a secret-shaped name
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies printenv OPENAI_API_KEY" {
  run run_guard <<<"$(claude_bash 'printenv OPENAI_API_KEY')"
  assert_deny_claude
}

@test "secret-read-guard: denies printenv GITHUB_TOKEN piped to wc" {
  run run_guard <<<"$(claude_bash 'printenv GITHUB_TOKEN | wc -c')"
  assert_deny_claude
}

@test "secret-read-guard: denies printenv of a secret name inside bash -c" {
  run run_guard <<<"$(claude_bash "bash -c 'printenv GITHUB_TOKEN'")"
  assert_deny_claude
}

@test "secret-read-guard: allows printenv HOME" {
  run run_guard <<<"$(claude_bash 'printenv HOME')"
  assert_allow
}

@test "secret-read-guard: allows printenv PATH" {
  run run_guard <<<"$(claude_bash 'printenv PATH')"
  assert_allow
}

# ---------------------------------------------------------------------------
# Multi-suffix .env files
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies Read of /repo/.env.production.local" {
  run run_guard <<<"$(claude_read '/repo/.env.production.local')"
  assert_deny_claude
}

@test "secret-read-guard: cursor beforeReadFile of .env.development.local denies" {
  run run_guard <<<"$(cursor_read_file '/repo/.env.development.local')"
  assert_deny_cursor
}

@test "secret-read-guard: pi Read of .env.production.local denies" {
  run run_guard <<<"$(pi_read '.env.production.local')"
  assert_deny_claude
}

@test "secret-read-guard: denies Grep content over /repo/.env.production.local" {
  run run_guard <<<"$(claude_grep '/repo/.env.production.local' '' 'X' 'content')"
  assert_deny_claude
}

@test "secret-read-guard: allows Read of /repo/.env.example" {
  run run_guard <<<"$(claude_read '/repo/.env.example')"
  assert_allow
}

@test "secret-read-guard: allows Read of /repo/.env.local.example (multi-suffix template)" {
  run run_guard <<<"$(claude_read '/repo/.env.local.example')"
  assert_allow
}

@test "secret-read-guard: allows Read of /repo/.envrc" {
  run run_guard <<<"$(claude_read '/repo/.envrc')"
  assert_allow
}

# ---------------------------------------------------------------------------
# .netrc spelled through ~ or $HOME
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies cat ~/.netrc" {
  run run_guard <<<"$(claude_bash 'cat ~/.netrc')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat \$HOME/.netrc" {
  run run_guard <<<"$(claude_bash 'cat $HOME/.netrc')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat .netrc" {
  run run_guard <<<"$(claude_bash 'cat .netrc')"
  assert_deny_claude
}

@test "secret-read-guard: allows ls ~/.netrc" {
  run run_guard <<<"$(claude_bash 'ls ~/.netrc')"
  assert_allow
}

# ---------------------------------------------------------------------------
# Credential-file content is judged per simple command, in every -c body
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies grep over .env inside fish -c" {
  run run_guard <<<"$(claude_bash "fish -c 'grep API .env'")"
  assert_deny_claude
}

@test "secret-read-guard: denies grep over .env inside bash -lc" {
  run run_guard <<<"$(claude_bash "bash -lc 'grep KEY .env'")"
  assert_deny_claude
}

@test "secret-read-guard: denies a printing grep after an ls -l of .env" {
  run run_guard <<<"$(claude_bash 'ls -l .env && grep -n API .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat of a template and .env together" {
  run run_guard <<<"$(claude_bash 'cat .env.example .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat .env after cat of a template" {
  run run_guard <<<"$(claude_bash 'cat .env.example; cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies head of .env beside a template" {
  run run_guard <<<"$(claude_bash 'head -50 .env .env.sample')"
  assert_deny_claude
}

@test "secret-read-guard: denies head -5 .env" {
  run run_guard <<<"$(claude_bash 'head -5 .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies sed -n 1p .env" {
  run run_guard <<<"$(claude_bash 'sed -n 1p .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat ~/.aws/credentials" {
  run run_guard <<<"$(claude_bash 'cat ~/.aws/credentials')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat ~/.ssh/id_ed25519" {
  run run_guard <<<"$(claude_bash 'cat ~/.ssh/id_ed25519')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat .env piped into grep -c (cat still prints)" {
  run run_guard <<<"$(claude_bash 'cat .env | grep -c X')"
  assert_deny_claude
}

@test "secret-read-guard: denies cat of a quoted .env path" {
  run run_guard <<<"$(claude_bash "cat '.env'")"
  assert_deny_claude
}

@test "secret-read-guard: allows a commit message that mentions cat .env" {
  run run_guard <<<"$(claude_bash "git commit -m 'docs: never cat .env'")"
  assert_allow
}

@test "secret-read-guard: allows a PR body that mentions cat and grep over .env" {
  run run_guard <<<"$(claude_bash "gh pr create --body 'Guard blocks cat .env and grep over .env'")"
  assert_allow
}

@test "secret-read-guard: allows a heredoc write whose body mentions source .env" {
  run run_guard <<<"$(claude_bash "cat > docs/setup.md <<'X'"$'\n'"Run source .env before starting."$'\n'"X")"
  assert_allow
}

@test "secret-read-guard: allows grep -qx over .gitignore for the .env entry" {
  run run_guard <<<"$(claude_bash "grep -qx '.env' .gitignore")"
  assert_allow
}

@test "secret-read-guard: allows ls of .env then sed over README" {
  run run_guard <<<"$(claude_bash 'ls -la .env; sed -n 1,20p README.md')"
  assert_allow
}

@test "secret-read-guard: allows rg -l TOKEN .env" {
  run run_guard <<<"$(claude_bash 'rg -l TOKEN .env')"
  assert_allow
}

@test "secret-read-guard: allows test -f .env && echo yes" {
  run run_guard <<<"$(claude_bash 'test -f .env && echo yes')"
  assert_allow
}

# ---------------------------------------------------------------------------
# Command position through keywords, wrappers, groups and assignments
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies env behind leading spaces" {
  run run_guard <<<"$(claude_bash '  env | grep -i api')"
  assert_deny_claude
}

@test "secret-read-guard: denies env after then" {
  run run_guard <<<"$(claude_bash 'if true; then env | grep KEY; fi')"
  assert_deny_claude
}

@test "secret-read-guard: denies env on its own line in a loop body" {
  run run_guard <<<"$(claude_bash $'for x in 1; do\n  env | sort\ndone')"
  assert_deny_claude
}

@test "secret-read-guard: denies env in a brace group" {
  run run_guard <<<"$(claude_bash '{ env; }')"
  assert_deny_claude
}

@test "secret-read-guard: denies sudo env" {
  run run_guard <<<"$(claude_bash 'sudo env')"
  assert_deny_claude
}

@test "secret-read-guard: denies command env" {
  run run_guard <<<"$(claude_bash 'command env')"
  assert_deny_claude
}

@test "secret-read-guard: denies env behind an assignment prefix" {
  run run_guard <<<"$(claude_bash 'FOO=1 env')"
  assert_deny_claude
}

@test "secret-read-guard: denies fish -c 'set -xg'" {
  run run_guard <<<"$(claude_bash "fish -c 'set -xg'")"
  assert_deny_claude
}

@test "secret-read-guard: denies fish -c 'set -Ux'" {
  run run_guard <<<"$(claude_bash "fish -c 'set -Ux'")"
  assert_deny_claude
}

@test "secret-read-guard: denies fish -c 'set --export'" {
  run run_guard <<<"$(claude_bash "fish -c 'set --export'")"
  assert_deny_claude
}

@test "secret-read-guard: allows echo do env (keyword as an argument)" {
  run run_guard <<<"$(claude_bash 'echo do env')"
  assert_allow
}

@test "secret-read-guard: allows set -euo pipefail" {
  run run_guard <<<"$(claude_bash 'set -euo pipefail')"
  assert_allow
}

@test "secret-read-guard: allows export FOO=bar" {
  run run_guard <<<"$(claude_bash 'export FOO=bar')"
  assert_allow
}

@test "secret-read-guard: allows fish -c 'set -gx PATH /x'" {
  run run_guard <<<"$(claude_bash "fish -c 'set -gx PATH /x'")"
  assert_allow
}

@test "secret-read-guard: allows env running a command" {
  run run_guard <<<"$(claude_bash 'env FOO=1 mycmd')"
  assert_allow
}

@test "secret-read-guard: denies set --show NAME" {
  run run_guard <<<"$(claude_bash 'set --show NAME')"
  assert_deny_claude
}
