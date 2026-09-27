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

# assert_deny_within_each_awk <max-ms> <payload> — replay assert_deny_within
# under every non-GNU awk this host has installed (mawk, nawk, busybox-awk),
# each spliced onto PATH ahead of the real awk. Falls back to a single run
# under the default awk when none of those are installed.
assert_deny_within_each_awk() {
  local max_ms=$1 payload=$2 name awk_path found=0

  for name in mawk nawk busybox-awk; do
    awk_path=$(command -v "$name") || continue
    found=1
    mkdir -p "$BATS_TEST_TMPDIR/$name"
    ln -sf "$awk_path" "$BATS_TEST_TMPDIR/$name/awk"
    PATH="$BATS_TEST_TMPDIR/$name:$PATH" assert_deny_within "$max_ms" "$payload"
  done

  [ "$found" -eq 1 ] || assert_deny_within "$max_ms" "$payload"
}

# Bound is loose (5 s, not the ~150 ms this case actually takes locally) because CI
# runs `bats --jobs 16` on a 4-core runner, and the wall-clock budget includes
# process startup under that contention. A catastrophic backtracking (the scenario
# these tests exist to catch) shows up as tens of seconds, not 5 s, so the headroom
# is safe.

# bats test_tags=timing
@test "secret-read-guard: a 100 KB heredoc followed by a dump denies in under 5 s" {
  local body
  body=$(printf "a 'b' \"c\"\n%.0s" $(seq 1 10240))
  assert_deny_within 5000 "$(claude_bash "cat > f <<'X'"$'\n'"$body"$'\n'"X"$'\n'"env")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB bash -c body ending in a dump denies in under 5 s" {
  local body
  body=$(printf 'echo hi; %.0s' $(seq 1 10240))
  assert_deny_within 5000 "$(claude_bash "bash -c '${body}env'")"
}

# heredoc_100k — a quoted heredoc of >= 100 KB whose lines carry an apostrophe,
# enough to outlast the pipe buffer if an awk pass ever exits early.
heredoc_100k() {
  local body
  body=$(printf "we don't stop here\n%.0s" $(seq 1 6000))
  printf '%s' "cat > f <<'X'"$'\n'"$body"$'\n'"X"
}

# bats test_tags=timing
@test "secret-read-guard: cat .env ahead of a 100 KB heredoc denies in under 5 s" {
  assert_deny_within 5000 "$(claude_bash "cat .env"$'\n'"$(heredoc_100k)")"
}

# bats test_tags=timing
@test "secret-read-guard: bash -c true, a 100 KB heredoc, then env denies in under 5 s" {
  assert_deny_within 5000 "$(claude_bash "bash -c true"$'\n'"$(heredoc_100k)"$'\n'"env")"
}

# bats test_tags=timing
@test "secret-read-guard: bash -c env ahead of a 100 KB heredoc denies in under 5 s" {
  assert_deny_within 5000 "$(claude_bash "bash -c env; $(heredoc_100k)")"
}

# assert_allow_within <max-ms> <payload> — allow in claude shape, fast enough.
assert_allow_within() {
  local start elapsed
  start=$(date +%s%N)
  run run_guard <<<"$2"
  elapsed=$((($(date +%s%N) - start) / 1000000))
  assert_allow
  echo "elapsed ${elapsed}ms" >&2
  [ "$elapsed" -lt "$1" ]
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB bash heredoc of commands ending in a credential read denies in under 3.5 s" {
  local body
  body=$(printf 'echo hi; %.0s' $(seq 1 10240))
  assert_deny_within 3500 "$(claude_bash "bash <<'X'"$'\n'"$body"$'\n'"cat .env"$'\n'"X")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB grep with 24000 quoted words allows in under 3.5 s" {
  local body
  body=$(printf "'a' \"b\" %.0s" $(seq 1 12000))
  assert_allow_within 3500 "$(claude_bash "grep -c ${body}.env")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB data heredoc then ls allows in under 3.5 s" {
  assert_allow_within 3500 "$(claude_bash "$(heredoc_100k)"$'\n'"ls")"
}

# bats test_tags=timing
@test "secret-read-guard: 2000 nested command substitutions ahead of a credential read deny in under 3.5 s" {
  local opens closes
  opens=$(printf '$(%.0s' $(seq 1 2000))
  closes=$(printf ')%.0s' $(seq 1 2000))
  assert_deny_within 3500 "$(claude_bash "echo ${opens}${closes}"$'\n'"cat .env")"
}

# bats test_tags=timing
@test "secret-read-guard: 24000 in words ahead of a credential read deny in under 3.5 s" {
  local body
  body=$(printf 'in %.0s' $(seq 1 24000))
  assert_deny_within 3500 "$(claude_bash "echo ${body}"$'\n'"cat .env")"
}

# bats test_tags=timing
@test "secret-read-guard: a 60 KB slash-free word beside a credential read denies in under 5 s under every awk" {
  local hex
  hex=$(printf 'ab%.0s' $(seq 1 30000))
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware (measured 3059/3768 ms there, see #513/#491); a quadratic regression still costs ~10x the fixed case.
  assert_deny_within_each_awk 5000 "$(claude_bash "head -c 64 .env && printf %s ${hex} | xxd -r -p > blob.bin")"
}

# bats test_tags=timing
@test "secret-read-guard: a 48 KB chain of credential names denies in under 5 s under every awk" {
  local body
  body=$(printf '.env%.0s' $(seq 1 12000))
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware (measured 3059/3768 ms there, see #513/#491); a quadratic regression still costs ~10x the fixed case.
  assert_deny_within_each_awk 5000 "$(claude_bash "cat .env ${body}")"
}

# The awk template strip must stay byte-for-byte what sed -E did with
# template_re: an over-strip there is an allow. sed is the oracle, built from the
# guard's own template_re so a word added to one and not the other fails here.
@test "secret-read-guard: the awk template strip equals the sed -E strip it replaced, under every awk" {
  local template_re awk_strip line mode name awk_path expected got
  eval "$(grep -m1 '^template_re=' "$GUARD")"
  eval "$(sed -n "/^awk_strip='/,/^}'\$/p" "$GUARD")"
  local -a lines=(
    'cat .env.example' 'cat .env.template .env.sample .env.dist' '.env.env.example'
    '.env..example' '.env.example.example' '.env.examples' '.env.examples.foo'
    '.env.example.local' '.env.example*' '.env.example?' '.env.example[' '.env.example,x'
    '.env.example/.env.example' 'x.env.sample-' '.foo/.env.dist' 'a.env.example.' '.env.'
    '.env.example.env' '.example.env.example' '.env.x.env.example.y.dist' '.env.dist2.dist'
    '.env.local .env.example {.env.example,.env}' '.env .env.example .env' 'env.example'
    '.env.example.template.' 'cat .env.example; .env.sample' '.env.a.b.c.example' '' '.' '..'
  )
  local -a modes=(0 1) awks=("")
  for name in mawk nawk busybox-awk; do
    awk_path=$(command -v "$name") || continue
    mkdir -p "$BATS_TEST_TMPDIR/$name"
    ln -sf "$awk_path" "$BATS_TEST_TMPDIR/$name/awk"
    awks+=("$BATS_TEST_TMPDIR/$name")
  done
  for line in "${lines[@]}"; do
    for mode in "${modes[@]}"; do
      if [[ $mode == 0 ]]; then
        expected=$(sed -E "s/$template_re//g" <<<"$line")
      else
        expected=$(sed -E "s/($template_re)([^A-Za-z0-9_.*?[-])/\\4/g; s/($template_re)\$//" <<<"$line")
      fi
      for awk_path in "${awks[@]}"; do
        got=$(PATH="${awk_path:+$awk_path:}$PATH" awk -v wide="$mode" "$awk_strip" <<<"$line")
        [[ $got == "$expected" ]] || {
          echo "wide=$mode ${awk_path:-default awk}: '$line' -> '$got', sed gives '$expected'" >&2
          return 1
        }
      done
    done
  done
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB chain of credential names ahead of a template name denies in under 5 s under every awk" {
  local body
  body=$(printf '.env%.0s' $(seq 1 25000))
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware; the pre-fix quadratic case (~3.4 s local / ~10 s CI) is still reliably caught on CI even at 5000 ms, though it may not exceed 5000 ms on a fast local machine.
  assert_deny_within_each_awk 5000 "$(claude_bash "cat .env ${body} .env.example")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB chain and a template name beside a bash -c credential read deny in under 5 s under every awk" {
  local body
  body=$(printf '.env%.0s' $(seq 1 25000))
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware; the pre-fix quadratic case (~3.4 s local / ~10 s CI) is still reliably caught on CI even at 5000 ms, though it may not exceed 5000 ms on a fast local machine.
  assert_deny_within_each_awk 5000 "$(claude_bash "bash -c cat\\ \\.env; x${body} .env.example")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB chain and a template name in a Grep path deny on the glob in under 5 s under every awk" {
  local body
  body=$(printf '.env%.0s' $(seq 1 25000))
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware; the pre-fix quadratic case (~3.4 s local / ~10 s CI) is still reliably caught on CI even at 5000 ms, though it may not exceed 5000 ms on a fast local machine.
  assert_deny_within_each_awk 5000 "$(claude_grep "x${body} .env.example" '.env' 'x' content)"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB chain and a template name with no credential read allow in under 3 s" {
  local body
  body=$(printf '.env%.0s' $(seq 1 25000))
  # CI runners ~3× slower; pre-fix was ~3.4 s local (~10 s CI), so 3000 ms still catches regressions.
  assert_allow_within 3000 "$(claude_bash "echo x${body} y.env.example")"
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
# Credential-file content is judged on the whole command, in every -c body
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

@test "secret-read-guard: allows rg -l TOKEN .env" {
  run run_guard <<<"$(claude_bash 'rg -l TOKEN .env')"
  assert_allow
}

# A quiet flag must appear in EVERY grep stage, on an unquoted word: one quiet
# grep must not excuse a printing one, and a flag spelled inside a quoted
# pattern is not a flag.
@test "secret-read-guard: denies grep -q KEY .env && grep KEY .env (check-then-show)" {
  run run_guard <<<"$(claude_bash 'grep -q KEY .env && grep KEY .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies if grep -q KEY .env; then grep KEY .env; fi" {
  run run_guard <<<"$(claude_bash 'if grep -q KEY .env; then grep KEY .env; fi')"
  assert_deny_claude
}

@test "secret-read-guard: denies grep -c KEY .env; grep -n KEY .env" {
  run run_guard <<<"$(claude_bash 'grep -c KEY .env; grep -n KEY .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies rg -l TOKEN . ; rg TOKEN .env" {
  run run_guard <<<"$(claude_bash 'rg -l TOKEN . ; rg TOKEN .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies grep KEY .env | xargs grep -l x" {
  run run_guard <<<"$(claude_bash 'grep KEY .env | xargs grep -l x')"
  assert_deny_claude
}

@test "secret-read-guard: denies a nested quiet grep excusing the outer one" {
  run run_guard <<<"$(claude_bash 'grep KEY .env $(grep -q x y)')"
  assert_deny_claude
}

@test "secret-read-guard: denies a flag inside a quoted pattern (grep 'x -c' .env)" {
  run run_guard <<<"$(claude_bash "grep 'x -c' .env")"
  assert_deny_claude
}

@test "secret-read-guard: denies a flag inside a double-quoted pattern with trailing space" {
  run run_guard <<<"$(claude_bash 'grep "API -l " .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies a flag behind a quoted pipe (grep -E 'x -c |KEY' .env)" {
  run run_guard <<<"$(claude_bash "grep -E 'x -c |KEY' .env")"
  assert_deny_claude
}

@test "secret-read-guard: denies a flag hidden by mixed quotes" {
  run run_guard <<<"$(claude_bash "grep -e KEY -e \"'\" 'x \" -c ' .env")"
  assert_deny_claude
}

@test "secret-read-guard: denies -iesecret (the c/q/l/L letter is an -e argument)" {
  run run_guard <<<"$(claude_bash 'grep -iesecret .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies a flag in a trailing comment (grep KEY .env # -c)" {
  run run_guard <<<"$(claude_bash 'grep KEY .env # -c')"
  assert_deny_claude
}

@test "secret-read-guard: denies a flag that is a filename after -- (grep KEY .env -- -c)" {
  run run_guard <<<"$(claude_bash 'grep KEY .env -- -c')"
  assert_deny_claude
}

@test "secret-read-guard: denies a flag that is an -e argument (grep -e -c -e KEY .env)" {
  run run_guard <<<"$(claude_bash 'grep -e -c -e KEY .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies a quiet grep excusing a second grep in the same stage" {
  run run_guard <<<"$(claude_bash 'find . -exec grep -l x {} + -exec grep KEY .env {} +')"
  assert_deny_claude
}

@test "secret-read-guard: denies a find primary after an -exec grep ... + (-quit is not -q)" {
  run run_guard <<<"$(claude_bash 'find . -name .env -exec grep KEY {} + -quit')"
  assert_deny_claude
}

@test "secret-read-guard: allows a quiet -exec grep ... +" {
  run run_guard <<<"$(claude_bash 'find . -name .env -exec grep -l KEY {} +')"
  assert_allow
}

@test "secret-read-guard: denies egrep and zgrep without a quiet flag" {
  run run_guard <<<"$(claude_bash 'egrep KEY .env')"
  assert_deny_claude
  run run_guard <<<"$(claude_bash 'zgrep KEY .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies rg -L and rg -rl (-L follows, -r replaces)" {
  run run_guard <<<"$(claude_bash 'rg -L KEY .env')"
  assert_deny_claude
  run run_guard <<<"$(claude_bash 'rg -rl KEY .env')"
  assert_deny_claude
}

@test "secret-read-guard: allows grep -lr KEY .env" {
  run run_guard <<<"$(claude_bash 'grep -lr KEY .env')"
  assert_allow
}

@test "secret-read-guard: allows grep -rl API_KEY --include=.env ." {
  run run_guard <<<"$(claude_bash 'grep -rl API_KEY --include=.env .')"
  assert_allow
}

@test "secret-read-guard: allows grep -il KEY .env" {
  run run_guard <<<"$(claude_bash 'grep -il KEY .env')"
  assert_allow
}

@test "secret-read-guard: allows grep --count KEY .env" {
  run run_guard <<<"$(claude_bash 'grep --count KEY .env')"
  assert_allow
}

@test "secret-read-guard: allows a trailing -c (grep KEY .env -c)" {
  run run_guard <<<"$(claude_bash 'grep KEY .env -c')"
  assert_allow
}

@test "secret-read-guard: allows a quoted pattern next to a real -c" {
  run run_guard <<<"$(claude_bash "grep -c 'a b' .env")"
  assert_allow
}

@test "secret-read-guard: allows two quiet greps in one command" {
  run run_guard <<<"$(claude_bash 'grep -c KEY .env && rg -l KEY .')"
  assert_allow
}

@test "secret-read-guard: allows test -f .env && echo yes" {
  run run_guard <<<"$(claude_bash 'test -f .env && echo yes')"
  assert_allow
}

# ---------------------------------------------------------------------------
# Credential reads behind wrappers that run the next word as a command
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies sudo -u app cat /srv/app/.env" {
  run run_guard <<<"$(claude_bash 'sudo -u app cat /srv/app/.env')"
  assert_deny_claude
}

@test "secret-read-guard: denies timeout 5 cat .env" {
  run run_guard <<<"$(claude_bash 'timeout 5 cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies nice cat .env" {
  run run_guard <<<"$(claude_bash 'nice cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies stdbuf -oL cat .env" {
  run run_guard <<<"$(claude_bash 'stdbuf -oL cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies env -i cat .env" {
  run run_guard <<<"$(claude_bash 'env -i cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies watch cat .env" {
  run run_guard <<<"$(claude_bash 'watch cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies builtin source .env" {
  run run_guard <<<"$(claude_bash 'builtin source .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies echo .env | xargs cat" {
  run run_guard <<<"$(claude_bash 'echo .env | xargs cat')"
  assert_deny_claude
}

@test "secret-read-guard: denies find -name .env -exec cat" {
  run run_guard <<<"$(claude_bash 'find . -name .env -exec cat {} \;')"
  assert_deny_claude
}

@test "secret-read-guard: denies eval 'cat .env'" {
  run run_guard <<<"$(claude_bash "eval 'cat .env'")"
  assert_deny_claude
}

@test "secret-read-guard: denies ssh host cat /srv/.env" {
  run run_guard <<<"$(claude_bash 'ssh host cat /srv/.env')"
  assert_deny_claude
}

@test "secret-read-guard: denies ssh host 'cat /srv/.env'" {
  run run_guard <<<"$(claude_bash "ssh host 'cat /srv/.env'")"
  assert_deny_claude
}

@test "secret-read-guard: denies docker compose exec app cat .env" {
  run run_guard <<<"$(claude_bash 'docker compose exec app cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies kubectl exec pod -- cat .env" {
  run run_guard <<<"$(claude_bash 'kubectl exec pod -- cat .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies stdbuf -oL grep KEY .env" {
  run run_guard <<<"$(claude_bash 'stdbuf -oL grep KEY .env')"
  assert_deny_claude
}

@test "secret-read-guard: denies sudo grep KEY .env" {
  run run_guard <<<"$(claude_bash 'sudo grep KEY .env')"
  assert_deny_claude
}

# ---------------------------------------------------------------------------
# An unbalanced apostrophe cannot hide a later credential read
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies cat .env after a heredoc commit message with an apostrophe" {
  run run_guard <<<"$(claude_bash "git commit -F - <<'X'"$'\n'"fix: don't leak"$'\n'"X"$'\n'"cat .env")"
  assert_deny_claude
}

@test "secret-read-guard: denies cat .env after a comment with an apostrophe" {
  run run_guard <<<"$(claude_bash "ls # don't"$'\n'"cat .env")"
  assert_deny_claude
}

@test "secret-read-guard: denies cat .env after an ANSI-C quoted apostrophe" {
  run run_guard <<<"$(claude_bash "echo \$'don\\'t' ; cat .env")"
  assert_deny_claude
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

# ---------------------------------------------------------------------------
# Rule 2 blind spots (#429): comments, heredocs, substitutions, wrappers
# ---------------------------------------------------------------------------

deny_cmd() { # <command>
  run run_guard <<<"$(claude_bash "$1")"
  assert_deny_claude
}

allow_cmd() { # <command>
  run run_guard <<<"$(claude_bash "$1")"
  assert_allow
}

@test "secret-read-guard: denies a dump after a comment with an apostrophe" {
  deny_cmd $'# what\'s configured?\nenv'
  deny_cmd $'echo hi # don\'t\nenv | sort'
}

@test "secret-read-guard: denies a dump after a heredoc body with an apostrophe" {
  deny_cmd $'cat <<EOF\nit\'s here\nEOF\nenv'
  deny_cmd $'git commit -F - <<\'X\'\nfix: don\'t leak\nX\nenv'
  deny_cmd $'cat <<-EOF\n\tdon\'t\n\tEOF\nenv'
  deny_cmd $'cat << \'EOF\'\nit\'s\nEOF\nenv'
  deny_cmd $'cat <<A <<B\nit\'s\nA\nb\nB\nenv'
}

@test "secret-read-guard: denies a dump after an apostrophe inside a bash -c body" {
  deny_cmd $'bash -c "echo hi # it\'s\nenv"'
}

@test "secret-read-guard: denies a dump after an ANSI-C quoted apostrophe" {
  deny_cmd $'echo $\'don\\\'t\'; env'
}

@test "secret-read-guard: denies a dumper inside a heredoc body" {
  deny_cmd $'cat <<EOF\n# $(env)\nEOF'
  deny_cmd $'cat <<EOF\nit\'s $(env)\nEOF'
  deny_cmd $'bash <<EOF\n# x\nenv\nEOF'
  deny_cmd $'cat <<EOF\n`env`\nEOF'
  deny_cmd $'cat <<EOF\n"$(env)"\nEOF'
}

@test "secret-read-guard: denies a dump right after a heredoc delimiter" {
  deny_cmd $'cat <<X;env\nx\nX'
}

@test "secret-read-guard: a fake heredoc from arithmetic cannot hide a dump" {
  deny_cmd $': $((1<<env))\nenv'
  deny_cmd $'echo $((1<<20))\nFOO="a b" env'
}

@test "secret-read-guard: a wrongly detected comment cannot hide a dump" {
  deny_cmd $'echo a\\ #; env'
  deny_cmd $'echo ${x%% #*}; env'
  deny_cmd $'echo \\#; env'
  deny_cmd 'echo $(date)#x; env'
  deny_cmd 'echo "a"#; env'
}

@test "secret-read-guard: allows prose and quoted heredocs that only mention a dumper" {
  allow_cmd 'echo hi # env'
  allow_cmd $'cat <<EOF\nrun env to see\nEOF\nls'
  allow_cmd $'echo \'a\nenv\''
}

@test "secret-read-guard: denies a dump inside a double-quoted command substitution" {
  deny_cmd 'echo "$(env)"'
  deny_cmd 'echo "$(printenv)"'
  deny_cmd 'echo "x $(FOO=1 env | sort)"'
  deny_cmd 'echo "a $(echo "$(env)")"'
  deny_cmd 'echo "x$'"'"'"; env'
}

@test "secret-read-guard: denies a dump inside backticks" {
  deny_cmd 'echo `env`'
  deny_cmd 'echo "`env`"'
}

@test "secret-read-guard: allows quoted text that only looks like a substitution" {
  allow_cmd 'echo "$(pwd)"'
  allow_cmd 'echo "env"'
  allow_cmd 'echo "\$(env)"'
  allow_cmd "echo '\$(env)'"
  allow_cmd 'echo "$(echo env)"'
}

@test "secret-read-guard: denies a dump behind direnv exec" {
  deny_cmd 'direnv exec . env'
  deny_cmd 'direnv exec . printenv'
  deny_cmd 'direnv exec "$PWD" env'
}

@test "secret-read-guard: allows direnv without a dump" {
  allow_cmd 'direnv exec . make test'
  allow_cmd 'direnv allow'
  allow_cmd 'direnv exec . env FOO=1 mycmd'
}

@test "secret-read-guard: denies env and printenv options with no command" {
  deny_cmd 'env -0'
  deny_cmd 'env -u X'
  deny_cmd 'env -C /tmp'
  deny_cmd 'env FOO=1'
  deny_cmd 'env -i FOO=1 | sort'
  deny_cmd 'printenv -0'
}

@test "secret-read-guard: a comment right after a closing parenthesis cannot hide a dump" {
  deny_cmd $'(echo x)# it\'s\nenv'
  deny_cmd $'case x in x)# it\'s\nenv\n;; esac'
}

@test "secret-read-guard: an empty heredoc delimiter cannot hide a dump" {
  deny_cmd $'cat <<""\nit\'s\n\nenv'
}

@test "secret-read-guard: denies a dump inside escaped nested backticks" {
  deny_cmd 'echo `echo \`env\``'
  deny_cmd 'echo "`echo \`env\``"'
}

@test "secret-read-guard: a comment right after an opening backtick cannot hide a dump" {
  deny_cmd $'echo `# it\'s\nenv'
  deny_cmd $'echo `# it\'s`\nenv'
  deny_cmd $'echo "`# it\'s`"\nenv'
  deny_cmd $'echo "`# it\'s\nenv\n`"'
}

@test "secret-read-guard: a backtick comment still denies a dump next to it" {
  deny_cmd 'echo `#` `env`'
  deny_cmd $'echo "`# x`"\necho `env`'
  deny_cmd 'echo `date`# x `env`'
}

@test "secret-read-guard: denies a dump in a backticked command inside a quoted heredoc" {
  deny_cmd $'cat <<\'EOF\'\n`env`\nEOF'
  deny_cmd $'bash <<\'EOF\'\n`env`\nEOF'
  deny_cmd $'bash <<\'EOF\'\necho hi `printenv`\nEOF'
  deny_cmd $'bash <<-\'EOF\'\n\t`set`\n\tEOF'
}

@test "secret-read-guard: denies a wrapper with a backticked argument inside a quoted heredoc" {
  deny_cmd $'bash <<\'EOF\'\nit\'s\nsudo -u `whoami` env\nEOF'
  deny_cmd $'bash <<\'EOF\'\nit\'s\ndirenv exec `pwd` env\nEOF'
  deny_cmd $'bash <<\'EOF\'\nit\'s\nenv -C `pwd` env\nEOF'
  deny_cmd $'bash <<\'EOF\'\nit\'s\nprintenv `echo` GITHUB_TOKEN\nEOF'
}

@test "secret-read-guard: denies a wrapper with a backticked argument" {
  deny_cmd 'sudo -u `whoami` env'
  deny_cmd 'direnv exec `pwd` env'
  deny_cmd 'env -C `pwd` env'
  deny_cmd 'printenv `echo` GITHUB_TOKEN'
}

@test "secret-read-guard: denies a dump inside three and four levels of escaped backticks" {
  deny_cmd $'echo `echo \\`echo \\\\\\`env\\\\\\`\\``'
  deny_cmd $'echo `echo \\`echo \\\\\\`echo \\\\\\\\\\\\\\`env\\\\\\\\\\\\\\`\\\\\\`\\``'
  deny_cmd $'echo "`echo \\`echo \\\\\\`env\\\\\\`\\``"'
  deny_cmd $'echo `echo \\`echo \\\\\\`# it\'s\n\\\\\\`\\``; echo `env`'
}

@test "secret-read-guard: denies a dump behind a bare backtick or a quote inside escaped backticks" {
  deny_cmd $'echo `true \\`x `env`'
  deny_cmd $'echo `echo \\"`date`\\"; env`'
}

@test "secret-read-guard: denies a dump followed by an output redirect to a descriptor" {
  local dumper
  for dumper in env printenv set declare export typeset; do
    deny_cmd "$dumper >&2"
    deny_cmd "$dumper 2>&1"
    deny_cmd "$dumper 2>/dev/null"
    deny_cmd "$dumper >& 2"
  done
  deny_cmd "fish -c 'set -x >&2'"
  deny_cmd 'env >&2 # c'
  deny_cmd 'set -S>&2'
  deny_cmd 'export -p>&2'
}

@test "secret-read-guard: allows a backticked word that is not a dump" {
  allow_cmd $'git commit -m \'run `env` to list\''
  allow_cmd $'git commit -F - <<\'EOF\'\nfix: `env vars`\n`set -e`\n`export FOO=1`\n`ls`\nEOF'
  allow_cmd $'echo `date`\nls'
  allow_cmd $'echo `echo \\`echo \\\\\\`date\\\\\\`\\``'
}

@test "secret-read-guard: allows a redirected command that is not a dump" {
  allow_cmd 'env -i mycmd >&2'
  allow_cmd 'set -euo pipefail >&2'
  allow_cmd 'export FOO=1 >&2'
  allow_cmd 'declare -a a 2>/dev/null'
  allow_cmd 'env >/tmp/x'
}

# backtick_level <k> — the backtick that opens or closes nesting level k: bash
# escapes it with 2^(k-1)-1 backslashes.
backtick_level() {
  local n=$(((1 << ($1 - 1)) - 1))
  printf '%*s' "$n" '' | tr ' ' '\\'
  printf '`'
}

# nested_backticks <depth> [inner-command] — `echo ` plus a level-k backtick
# per level, a newline and the inner command (default env) at the deepest
# level, then the closers.
nested_backticks() {
  local k out='' inner="${2:-env}"
  for ((k = 1; k <= $1; k++)); do
    out+="echo $(backtick_level "$k")"
  done
  out+=$'\n'"$inner"
  for ((k = $1; k >= 1; k--)); do
    out+=$(backtick_level "$k")
  done
  printf '%s' "$out"
}

# bats test_tags=timing
@test "secret-read-guard: 14 nested levels of escaped backticks ahead of a dump deny in under 5 s under every awk" {
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware (measured 3059/3768 ms there, see #513/#491); a quadratic regression still costs ~10x the fixed case.
  assert_deny_within_each_awk 5000 "$(claude_bash "$(nested_backticks 14)")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB quoted heredoc of backticks ahead of a dump denies in under 3.5 s" {
  local body
  body=$(printf '`a` `b`\n%.0s' $(seq 1 12800))
  assert_deny_within 3500 "$(claude_bash "bash <<'X'"$'\n'"$body"$'\n`env`\nX')"
  assert_deny_within 3500 "$(claude_bash "cat <<'X'"$'\n'"$body"$'\nX\nenv')"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB flat run of backticked words ahead of a dump denies in under 3.5 s" {
  local body
  body=$(printf '`a`;%.0s' $(seq 1 25600))
  assert_deny_within 3500 "$(claude_bash "echo ${body}env")"
}

# ---------------------------------------------------------------------------
# Rule 2's backtick-frame masker treats a frame as opaque: it ends at the first
# backtick behind an even backslash run, as in bash's raw scan, and shows
# everything inside except quotes and backslashes, so no quote, comment or
# heredoc inside a frame can hide a dumper (#481, #482, #483).
# ---------------------------------------------------------------------------

@test "secret-read-guard: denies a backticked dump spanning lines in a quoted heredoc body" {
  deny_cmd $'bash <<\'EOF\'\n`# it\'s\nenv`\nEOF'
  deny_cmd $'bash <<\'EOF\'\n`echo a\nenv`\nEOF'
  deny_cmd $'bash <<\'EOF\'\n`echo \\`env\\``\nEOF'
  deny_cmd $'bash <<\'EOF\'\n# don\'t use ` here\n`echo a\nenv`\nEOF'
  deny_cmd $'bash <<-\'EOF\'\n\t`echo a\n\tenv`\n\tEOF'
}

@test "secret-read-guard: denies a backticked dump spanning lines in an unquoted heredoc body" {
  deny_cmd $'bash <<EOF\n`# it\'s\nenv`\nEOF'
  deny_cmd $'bash <<EOF\n`echo a\nenv`\nEOF'
  deny_cmd $'bash <<EOF\n`echo \\`env\\``\nEOF'
  deny_cmd $'cat <<EOF\n`echo a\nenv`\nEOF'
}

@test "secret-read-guard: denies an escaped backtick opening a substitution inside double quotes in a backtick frame" {
  deny_cmd $'`echo "\\`env\\`"`'
  deny_cmd $': $(echo `echo "\\`env\\`"`)'
  deny_cmd $'echo "`echo "\\`env\\`"`"'
}

@test "secret-read-guard: denies a dump after a comment that a backtick closes" {
  deny_cmd $'`true # x`\necho `env`'
  deny_cmd $'echo `true # x`; echo `env`'
}

@test "secret-read-guard: denies a dump behind a backtick that closes its frame inside quotes or is escaped inside it" {
  deny_cmd $'( : `echo \'a` ); env'
  deny_cmd $'`echo \\\\\\` ; env`'
}

@test "secret-read-guard: denies a dump behind escaped quotes inside a backtick frame" {
  deny_cmd $'echo `echo \\\\\' ; env`'
  deny_cmd $'echo `echo \\\\" ; env`'
  deny_cmd $'echo `echo \\\\\\\' x \' ; env`'
  deny_cmd $'echo `echo \\\\\\" x " ; env`'
  deny_cmd $'echo "`echo \\\\\' ; env`"'
  deny_cmd $'echo "`echo \\\\\\\' x \' ; env`"'
  deny_cmd $'echo $(`echo \\\\\' ; env`)'
  deny_cmd $'echo $(`echo \\\\" ; env`)'
  deny_cmd $'echo $(: `echo \\\\\\\' x \' ; env`)'
  deny_cmd $'echo `echo \\`echo \\\\\\\\\' ; env\\``'
  deny_cmd $'echo `echo $\'\\\\\\\' ; env`'
  deny_cmd $'echo "$(echo `echo \\\\\\" x " ; env`)"'
}

@test "secret-read-guard: denies a dump after a heredoc opened inside a backtick frame whose body closes the frame" {
  deny_cmd $'`cat <<EOF\nx`\nEOF\n` echo \\\\\' ; env `'
  deny_cmd $'`echo "`; `cat <<EOF\nit\'s\nEOF\nenv`'
  deny_cmd $'`echo "`; `cat <<\'EOF\'\nit\'s\nEOF\nenv`'
}

@test "secret-read-guard: denies a dump behind a backslash-newline inside a backtick frame" {
  deny_cmd $'`echo \\\\\\\n\' ; env`'
}

@test "secret-read-guard: denies a dump behind a heredoc operator or delimiter that a backtick frame escapes" {
  deny_cmd $'echo `echo <<\'EOF\'\\\\\' ; env`'
  deny_cmd $'`cat \\\\<<EOF `\n: `\nEOF\necho \\\\\' ; env `'
}

@test "secret-read-guard: denies a dump behind a heredoc delimiter word holding a backtick span" {
  deny_cmd $'( : <<EOF`echo \'(a` ); env'
  deny_cmd $': <<"a`echo "`" ; env'
  deny_cmd $'echo $((1<<`env`))'
  deny_cmd $'echo "$((1<< `env`))"'
}

@test "secret-read-guard: allows quoted text and heredoc prose inside backtick frames with no dump" {
  allow_cmd $'echo "Built at `date \'+%F %T\'`"'
  allow_cmd $'echo `grep -c \'foo; env\' notes.txt`'
  allow_cmd $'echo `echo "it\'s fine"`'
  allow_cmd $'echo `cat <<\'EOF\'\nit\'s; fine\nEOF\n`'
  allow_cmd $'X=`printf \'%s\\n\' "a b"`; echo "$X"'
}

@test "secret-read-guard: denies a dumper word at a pseudo command start in quoted text inside a backtick frame (accepted over-deny)" {
  deny_cmd $'echo `grep -E \'^(export|set) \' ~/.bashrc | wc -l`'
  deny_cmd $'echo "`echo \\\\\\" x " ; env`"'
}

# Every heredoc body backtick is read as both a command start and a command
# end so a stray backtick cannot hide a frame; the cost is that a dumper word
# after a code span — at a line start, mid-line, or after a keyword like
# then — or right before one at a line start, denies.
@test "secret-read-guard: denies a dumper word next to a heredoc body backtick (accepted over-deny)" {
  deny_cmd $'git commit -F - <<\'EOF\'\nexport `FOO` in your rc\nEOF'
  deny_cmd $'git commit -F - <<\'EOF\'\nIt reads `DISPATCHER_X` env\nEOF'
  deny_cmd $'git commit -F - <<\'EOF\'\nfix: read `FOO` then set `BAR` too\nEOF'
}

@test "secret-read-guard: allows commit and PR heredoc bodies with prose backticks and a stray one" {
  allow_cmd $'git commit -F - <<\'EOF\'\nfix(x): handle `foo` in `bar`\n\nReads `DISPATCHER_X` env var and `set -e`. A stray ` tick.\n`env vars` are documented; `export FOO=1` too.\nEOF'
  allow_cmd $'cat >/dev/null <<EOF\nfix(x): handle `true` in `:`\n\nReads `true` env var and `:`. A stray ` tick.\nEOF'
  allow_cmd $'gh pr create --body-file - <<\'EOF\'\n## Summary\nThe `set -e` line, a stray ` tick,\nand `env vars` docs.\nEOF'
}

@test "secret-read-guard: allows literal backticked dumpers in single quotes and escaped in double quotes" {
  allow_cmd $'echo \'`env`\''
  allow_cmd $'echo "\\`env\\`"'
}

# assert_allow_within_each_awk <max-ms> <payload> — the allow twin of
# assert_deny_within_each_awk.
assert_allow_within_each_awk() {
  local max_ms=$1 payload=$2 name awk_path found=0

  for name in mawk nawk busybox-awk; do
    awk_path=$(command -v "$name") || continue
    found=1
    mkdir -p "$BATS_TEST_TMPDIR/$name"
    ln -sf "$awk_path" "$BATS_TEST_TMPDIR/$name/awk"
    PATH="$BATS_TEST_TMPDIR/$name:$PATH" assert_allow_within "$max_ms" "$payload"
  done

  [ "$found" -eq 1 ] || assert_allow_within "$max_ms" "$payload"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB backticked commit body allows in under 5 s under every awk" {
  local body
  # 900 reps keeps the whole payload under Linux's 128 KiB single-argv-string
  # cap (MAX_ARG_STRLEN) that claude_bash's jq --arg would otherwise blow.
  body=$(printf 'fix(x): handle `foo` in `bar`\n\nReads `DISPATCHER_X` env var and `set -e`. A stray ` tick.\n`env vars` are documented; `export FOO=1` too.\n%.0s' $(seq 1 900))
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware (measured 3059/3768 ms there, see #513/#491); a quadratic regression still costs ~10x the fixed case.
  assert_allow_within_each_awk 5000 "$(claude_bash "git commit -F - <<'EOF'"$'\n'"$body"$'\n'"EOF")"
}

# bats test_tags=timing
@test "secret-read-guard: 14 nested levels of backticks around a harmless command allow in under 5 s under every awk" {
  # Bound 5000 ms: CI's serial timing step runs alone on slow runner hardware (measured 3059/3768 ms there, see #513/#491); a quadratic regression still costs ~10x the fixed case.
  assert_allow_within_each_awk 5000 "$(claude_bash "$(nested_backticks 14 date)")"
}

# bats test_tags=timing
@test "secret-read-guard: escape runs inside 14 nested backtick frames allow in under 5 s under every awk" {
  local body
  body=$(printf '\\\\x %.0s' $(seq 1 20000))
  # Bound 5000 ms (was 3500): the slowest awk (nawk) measured at 173 ms median / 179 ms max locally
  # at depth=14, runlen=20000. Growth is ~linear in both depth (ratio ~1.5-2.7 for 14->20) and
  # runlen (ratio ~1.8-2.0 for each doubling). CI runs bats with --jobs 16 on a 4-core runner,
  # so wall-clock headroom must absorb contention. The 5000 ms bound is generous; a catastrophic
  # backtrack (>30 s) would still fail it. See #507.
  assert_allow_within_each_awk 5000 "$(claude_bash "$(nested_backticks 14 "$body")")"
}

# bats test_tags=timing
@test "secret-read-guard: rule 2 — a 100 KB run of wrapper options ahead of a dump denies in under 5 s (#452)" {
  local body
  body=$(printf -- '-n 1 %.0s' $(seq 1 20000))
  assert_deny_within 5000 "$(claude_bash "nice ${body}env")"
}

# bats test_tags=timing
@test "secret-read-guard: rule 2 — a dump continued across 100 KB of backslash-newlines denies in under 5 s (#492)" {
  local body
  # Only the joined reading sees one command here, so this times that pass.
  body=$(printf -- '-u A \\\n%.0s' $(seq 1 16000))
  assert_deny_within 5000 "$(claude_bash "env ${body}"$'\n'"| sort")"
}

@test "secret-read-guard: denies printenv with a bare double dash" {
  deny_cmd 'printenv --'
}

@test "secret-read-guard: denies a shell-variable dump followed by a comment" {
  deny_cmd 'set # list'
  deny_cmd 'declare -p # list'
  deny_cmd 'export -p # list'
}

@test "secret-read-guard: denies a dump followed by a comment or a descriptor redirect" {
  deny_cmd 'env # show vars'
  deny_cmd 'env 2>&1'
  deny_cmd 'printenv 2>/dev/null'
}

@test "secret-read-guard: denies a dumper behind env and sudo wrappers" {
  deny_cmd 'env -u X env'
  deny_cmd 'env FOO=1 printenv'
  deny_cmd 'sudo -u root env'
  deny_cmd 'sudo -Eu root env'
  deny_cmd 'sudo -u root declare -p X'
  deny_cmd 'sudo -u "root" env'
}

@test "secret-read-guard: allows env and sudo running a command" {
  allow_cmd 'env -u X mycmd'
  allow_cmd 'env -i mycmd'
  allow_cmd 'env -0 mycmd'
  allow_cmd 'sudo -n ls'
}

@test "secret-read-guard: denies a negated dump" {
  deny_cmd '! env'
  deny_cmd $'!\tenv'
  deny_cmd 'if ! env; then true; fi'
  deny_cmd 'true; ! set -S X'
  deny_cmd '! printenv API_KEY'
  deny_cmd $'echo wow!\nenv'
}

@test "secret-read-guard: allows a dumper word after an exclamation mark in an argument" {
  allow_cmd 'echo wow! env'
}

@test "secret-read-guard: rule 2 — denies a dumper behind common wrappers (#452)" {
  deny_cmd 'timeout 5 env'
  deny_cmd 'timeout -s KILL 5 env'
  deny_cmd 'nice env'
  deny_cmd 'nice -n 10 env'
  deny_cmd 'echo x | xargs env'
  deny_cmd 'stdbuf -oL env'
  deny_cmd 'setsid env'
  deny_cmd 'watch env'
  deny_cmd 'ssh host env'
  deny_cmd 'ssh -i k host env'
  deny_cmd 'docker exec c env'
  deny_cmd 'docker exec -it c env'
  deny_cmd 'docker compose exec app env'
  deny_cmd 'docker compose -f x.yml exec app env'
  deny_cmd 'docker run --rm img env'
  deny_cmd 'podman exec c env'
  deny_cmd 'kubectl exec pod -- env'
  deny_cmd 'kubectl exec -it pod -c ctr -- env'
  deny_cmd 'mise exec -- env'
  deny_cmd 'nix develop -c env'
  deny_cmd 'command -p env'
  deny_cmd 'time -p env'
  deny_cmd 'exec -a x env'
}

@test "secret-read-guard: rule 2 — denies path-qualified and builtin dumpers (#452)" {
  deny_cmd '/usr/bin/env'
  deny_cmd 'builtin set'
}

@test "secret-read-guard: rule 2 — denies a wrapper whose option argument is a substitution (#452, #484)" {
  deny_cmd 'sudo -u $(id -un) env'
  deny_cmd 'sudo -u $(id) env'
  deny_cmd 'direnv exec $(pwd) env'
  deny_cmd 'printenv $(echo) GITHUB_TOKEN'
}

@test "secret-read-guard: rule 2 — denies a dumper in a case arm (#452)" {
  deny_cmd 'case x in x) env;; esac'
  deny_cmd $'case x in\n  debug) env ;;\nesac'
}

@test "secret-read-guard: rule 2 — denies a dumper reading stdin or a heredoc (#493)" {
  deny_cmd 'env </dev/null'
  deny_cmd $'env <<E\nx\nE'
}

@test "secret-read-guard: rule 2 — denies a dump split by a backslash-newline continuation (#492)" {
  deny_cmd $'env \\\n| sort'
  deny_cmd $'bash -c \'printenv \\\n| sort\''
  deny_cmd $'cat <<EOF\na \\\nEOF\nit\'s `env`\nEOF'
}

@test "secret-read-guard: rule 2 — denies a dump behind a misread comment (#494)" {
  deny_cmd $'echo \\ # `env`'
  deny_cmd 'echo $((# `env`))'
  deny_cmd $'tee a\\ # <<E\nit\'s `env`\nE'
  deny_cmd $'ls # don\'t\nenv'
}

@test "secret-read-guard: rule 2 — allows commands that only name env as an argument or run a command" {
  allow_cmd 'sudo rm -rf env'
  allow_cmd 'python3 -m venv env'
  allow_cmd 'uv venv env'
  allow_cmd 'command -v env'
  allow_cmd 'timeout 5 make env'
  allow_cmd 'ssh host ls env'
  allow_cmd 'ssh -t host ls env'
  allow_cmd 'docker run --rm img ls env'
  allow_cmd 'docker exec -it c ls env'
  allow_cmd 'kubectl exec pod -- ls env'
  allow_cmd 'env FOO=1 make'
  allow_cmd 'env <(true)'
  allow_cmd 'ls # just a comment'
  allow_cmd $'echo a \\\n  b'
}

@test "secret-read-guard: rule 2 — allows commit prose and shebangs in quoted heredocs" {
  allow_cmd $'git commit -F - <<\'EOF\'\nThe variable was (re)set\nLeave PATH as the shell (already) set\n- `env -u X Y=<empty> bats`\nEOF'
  allow_cmd $'cat > s <<\'EOF\'\n#!/usr/bin/env bash\nEOF'
  allow_cmd $'cat > s <<\'EOF\'\n#!/usr/bin/env -S bash -e\nEOF'
}

@test "secret-read-guard: rule 2 — denies a dumper after a separator inside an inline comment (accepted over-deny)" {
  deny_cmd 'ls # then; env'
}

# ---------------------------------------------------------------------------
# Rule 3: credential-file reads, by case table
# ---------------------------------------------------------------------------

# check_rows — run a flat table of <kind> <expect> <arg…> records through the
# guard. kind is bash (1 arg: a command), read (1 arg: a path), or grep (2
# args: a path and a glob, sent through claude_grep with pattern "x" and mode
# "content"). @E@ becomes .env and @NL@ becomes a real newline in every arg
# before the payload is built. Non-empty stdout is a deny; a nonzero exit is
# folded into the verdict as "<verdict>(rc=N)", which never matches a plain
# expectation. Mismatches are collected and echoed (bats only shows a failing
# test's own output) before check_rows returns non-zero.
check_rows() {
  local mismatches='' kind exp a1 a2 payload out rc got
  while [ "$#" -gt 0 ]; do
    kind=$1 exp=$2
    shift 2
    case "$kind" in
      bash)
        a1=$1
        shift
        a1=${a1//@E@/.env}
        a1=${a1//@NL@/$'\n'}
        payload=$(claude_bash "$a1")
        ;;
      read)
        a1=$1
        shift
        a1=${a1//@E@/.env}
        a1=${a1//@NL@/$'\n'}
        payload=$(claude_read "$a1")
        ;;
      grep)
        a1=$1 a2=$2
        shift 2
        a1=${a1//@E@/.env}
        a1=${a1//@NL@/$'\n'}
        a2=${a2//@E@/.env}
        a2=${a2//@NL@/$'\n'}
        payload=$(claude_grep "$a1" "$a2" x content)
        ;;
    esac
    out=$(printf '%s' "$payload" | run_guard 2>/dev/null)
    rc=$?
    if [ -n "$out" ]; then got=deny; else got=allow; fi
    [ "$rc" -ne 0 ] && got="$got(rc=$rc)"
    if [ "$got" != "$exp" ]; then
      mismatches+="expected $exp got $got: $(printf '%q' "${a1:0:120}")"$'\n'
    fi
  done
  if [ -n "$mismatches" ]; then
    echo "$mismatches"
    return 1
  fi
}

rows_interpreters=(
  bash deny "python3 -c \"print(open('@E@').read())\""
  bash deny "python -c 'import sys; print(open(sys.argv[1]).read())' @E@"
  bash deny "node -e \"console.log(require('fs').readFileSync('@E@','utf8'))\""
  bash deny "ruby -e 'puts File.read(\"@E@\")'"
  bash deny "perl -ne print @E@"
  bash deny "python3 - <<'EOF'@NL@print(open('@E@').read())@NL@EOF"
  bash deny "echo \"print(open('@E@').read())\" | python3"
  bash deny "python3 <<< \"print(open('@E@').read())\""
  bash deny "uv run python -c \"print(open('@E@').read())\""
  bash allow "python -m venv @E@"
  bash allow "node --env-file=@E@ server.js"
)

@test "secret-read-guard: rule 3 — interpreters read .env content (#428)" {
  check_rows "${rows_interpreters[@]}"
}

rows_lt_no_space=(
  bash deny "cat<@E@"
  bash deny "cat <@E@"
  bash deny "<@E@ cat"
  bash deny "grep KEY<@E@"
  bash allow "grep -c KEY <@E@"
)

rows_redirect_target_not_a_flag=(
  bash deny "grep KEY @E@ 2> -c"
  bash deny "grep KEY @E@ > -q"
  bash deny "grep KEY @E@ >> -c"
  bash deny "grep KEY @E@ 2>-q"
  bash deny "grep KEY @E@ < -q"
  bash deny "grep KEY @E@ <<< -q"
  bash deny "grep KEY @E@ << -q"
  bash deny "grep KEY @E@ <<< -c"
  bash deny "grep KEY @E@ >| -q"
  bash deny "grep KEY @E@ <> -q"
  bash deny "grep KEY @E@ <& -q"
  bash allow "grep -q KEY @E@"
  bash allow "grep -c KEY @E@ 2>/dev/null"
  bash allow "grep -c KEY @E@ > -o"
)

@test "secret-read-guard: rule 3 — a redirect target is never a quiet flag (#439)" {
  check_rows "${rows_redirect_target_not_a_flag[@]}"
}

@test "secret-read-guard: rule 3 — < without a space (#428)" {
  check_rows "${rows_lt_no_space[@]}"
}

rows_dot_sourcing=(
  bash deny ". @E@"
  bash deny ". ./@E@ && echo ok"
  bash deny "set -a; . @E@; set +a"
)

@test "secret-read-guard: rule 3 — dot-sourcing (#428)" {
  check_rows "${rows_dot_sourcing[@]}"
}

rows_git_reads=(
  bash deny "git show HEAD:@E@"
  bash deny "git show main:config/@E@"
  bash deny "git cat-file -p HEAD:@E@"
  bash deny "git grep KEY -- @E@"
  bash allow "git grep -c KEY -- @E@"
  bash deny "git diff --no-index /dev/null @E@"
  bash allow "git diff --stat -- @E@"
  bash allow "git show HEAD:@E@.example"
  bash deny "git log -p -- @E@"
  bash allow "git log --oneline -- @E@"
  bash allow "git rm --cached @E@"
  bash allow "git check-ignore -q @E@"
)

@test "secret-read-guard: rule 3 — git reads (#428)" {
  check_rows "${rows_git_reads[@]}"
}

rows_grep_e_q=(
  bash deny "grep -e -q KEY @E@"
)

@test "secret-read-guard: rule 3 — grep -e -q (#428)" {
  check_rows "${rows_grep_e_q[@]}"
}

rows_template_anchoring=(
  bash deny "cat @E@.distro"
  bash deny "cat @E@.examples"
  bash deny "cat @E@.example.local"
  bash allow "cat @E@.local.example"
  bash allow "cat @E@.dist"
  read deny "/repo/@E@.example/../@E@"
  read deny "/repo/@E@.examples"
  read deny "/repo/@E@.example.local"
  read deny "/repo/@E@.distro"
  read allow "/repo/@E@.dist"
  read allow "/repo/@E@.local.example"
  grep deny "/repo/@E@.examples" ""
  grep deny "" "@E@.example.local"
  grep allow "/repo/@E@.example" ""
)

@test "secret-read-guard: rule 3 — template anchoring (#428)" {
  check_rows "${rows_template_anchoring[@]}"
}

rows_residuals=(
  bash deny "echo \"\$(cat <<'EOF'@NL@cat @E@@NL@EOF@NL@)\" | bash"
  bash deny "echo \"\$(case x in a) cat @E@;; esac)\""
  bash deny "echo \"\$(case x in (a) cat @E@;; esac)\""
  bash deny "x=\$(case y in a|b) echo;; esac; cat @E@)"
  bash allow "echo \"\$(case \$x in a) echo hi;; esac)\" done"
)

@test "secret-read-guard: rule 3 — #427 residuals" {
  check_rows "${rows_residuals[@]}"
}

rows_benign_heads=(
  bash deny "git -c alias.x='!cat @E@' x"
  bash deny "git -c core.pager='cat @E@' log"
  bash deny "GIT_PAGER='cat @E@' git log"
  bash deny "git config alias.x '!cat @E@'"
  bash deny "git rebase --exec 'cat @E@' HEAD~1"
  bash deny "gh alias set x --shell 'cat @E@'"
)

@test "secret-read-guard: rule 3 — adversarial: benign heads that run text" {
  check_rows "${rows_benign_heads[@]}"
}

rows_reach_through=(
  bash deny "f=@E@; cat \"\$f\""
  bash deny "for f in @E@ @E@.local; do head -3 \"\$f\"; done"
  bash deny "cp @E@ /tmp/e && cat /tmp/e"
  bash deny "ln -s @E@ /tmp/e; cat /tmp/e"
  bash deny "echo 'cat @E@' > >(bash)"
  bash deny "echo 'cat @E@' | tee /dev/stderr | bash"
)

@test "secret-read-guard: rule 3 — adversarial: reach through a variable, loop, copy" {
  check_rows "${rows_reach_through[@]}"
}

rows_path_spellings=(
  bash deny "cat @E@*"
  bash deny "head -5 @E@.*"
  bash deny "cat {@E@,@E@.local}"
  bash deny "cat \"\$HOME/.ssh/id_rsa\""
  bash deny "cat .envrc.local"
  bash deny "echo \`cat @E@\`"
)

@test "secret-read-guard: rule 3 — adversarial: path spellings" {
  check_rows "${rows_path_spellings[@]}"
}

rows_must_allow=(
  bash allow "ls @E@*"
  bash allow "rm -f @E@.*"
  bash allow "echo '@E@*' >> .gitignore"
  bash allow "f=README.md; cat \"\$f\""
  bash allow "cat ~/.ssh/id_rsa.pub"
  bash allow "echo KEY=1 >> @E@"
  bash allow "cp @E@.example @E@"
)

@test "secret-read-guard: rule 3 — allows that must hold" {
  check_rows "${rows_must_allow[@]}"
}

rows_write_then_run=(
  bash deny "echo 'cat @E@' > x.sh && bash x.sh"
  bash deny "cat > run.sh <<'X'@NL@cat @E@@NL@X@NL@bash run.sh"
  bash deny "printf -v c 'cat @E@'; \$c"
)

@test "secret-read-guard: rule 3 — writes of a read, run later in the same command" {
  check_rows "${rows_write_then_run[@]}"
}

rows_redirects_amp_pipe=(
  bash deny "agenix -d x.age -i ~/.ssh/id_ed25519 2>&1 | tail -3"
  bash deny "cat 2>&1 @E@"
  bash deny "grep KEY 2>&1 @E@"
  bash deny "cat &>/dev/stderr @E@"
  bash deny "cat @E@ >| out"
  bash deny "echo \"\`cat @E@\`\""
  bash allow "crew msg w d \"files like (@E@, credentials) and \`true\`\""
)

@test "secret-read-guard: rule 3 — redirects that hold & or |" {
  check_rows "${rows_redirects_amp_pipe[@]}"
}

rows_subst_and_patch=(
  bash deny "echo \"\$(<@E@)\""
  bash deny "x=\$(< @E@)"
  bash deny "git stash show -p -- @E@"
  bash deny "git format-patch --stdout -1 -- @E@"
  bash allow "git push -u origin feat/x"
  bash deny "git commit -m 'never cat @E@' && npm test"
  bash deny "cat > notes.md <<'X'@NL@cat @E@@NL@X@NL@make"
)

@test "secret-read-guard: rule 3 — input substitutions, git patch output and write-then-run" {
  check_rows "${rows_subst_and_patch[@]}"
}

rows_laundering_and_patch_flags=(
  bash deny "echo @E@ > l; xargs cat < l"
  bash deny "echo @E@ > l; xargs -a l head"
  bash deny "ls @E@ && timeout 5 sed -n 1p README.md"
  bash deny "git log -U3 -- @E@"
  bash deny "git log --unified=1 -- @E@"
  bash deny "git log --patch-with-stat -- @E@"
  bash deny "git log --cc -- @E@"
  bash deny "git log -c -1 -- @E@"
  bash deny "git log --binary -- @E@"
  bash deny "git range-diff main...feat -- @E@"
  bash allow "cp @E@.example @E@ && git push -u origin feat/x"
  bash allow "cp @E@.example @E@ && git stash -u"
  bash allow "ls @E@ && git fetch -p"
  bash allow "test -f @E@ || git switch -c \"\$b\""
  bash allow "git stash show -- @E@"
  bash allow "git log --oneline -- @E@"
)

@test "secret-read-guard: rule 3 — xargs laundering and git patch flags by subcommand" {
  check_rows "${rows_laundering_and_patch_flags[@]}"
}

rows_merge_diff_flags=(
  bash deny "git log --dd -- @E@"
  bash deny "git log --remerge-diff -- @E@"
  bash deny "git reflog --remerge-diff @E@"
  bash deny "git log --diff-merges=on -- @E@"
)

@test "secret-read-guard: rule 3 — git merge-diff flags" {
  check_rows "${rows_merge_diff_flags[@]}"
}

rows_proposal=(
  bash deny "ssh -l u host grep KEY @E@"
  bash deny "echo \"\$(cat @E@)\""
  bash deny "echo \$(cat @E@)"
  bash deny "cat \"\$(echo @E@)\""
  bash deny "echo cat @E@ | bash"
  bash deny "bash <<'X'@NL@cat @E@@NL@X"
  bash deny "cat <<'X' | bash@NL@cat @E@@NL@X"
  bash deny "bash -c \"\$(cat <<'EOF'@NL@cat @E@@NL@EOF@NL@)\""
  bash deny "cat <<X@NL@\$(cat @E@)@NL@X"
  bash deny "(cd /w && cat @E@)"
  bash deny "echo @E@ |@NL@xargs cat"
  bash deny "printf '%s\\n' @E@ | xargs -I{} cat {}"
  bash deny "\"cat\" echo @E@"
  bash deny "bash <<< 'cat @E@'"
  bash deny "echo \$((1<<2))@NL@cat @E@"
  bash deny "{ cat @E@; }"
  bash deny "if grep KEY @E@; then :; fi"
  bash deny "FOO=1 cat @E@"
  bash deny "/bin/cat @E@"
  bash deny "find . -name @E@ | xargs grep KEY"
  bash allow "grep -qx '@E@' .gitignore"
  bash deny "ls -la @E@; sed -n 1,20p README.md"
  bash allow "bash -lc 'grep -c KEY @E@'"
  bash allow "echo @E@ >> .gitignore"
  bash allow "timeout 5 rg -c TOKEN @E@"
  bash allow "find . -name @E@ | xargs grep -l KEY"
  bash deny "cat <<'A' <<'B'@NL@x@NL@A@NL@y@NL@B@NL@cat @E@"
  bash deny "x=\$(cat @E@)"
  bash deny "diff <(cat @E@) <(cat @E@.example)"
  bash deny "echo \`cat @E@\`"
  bash deny "\"\$(which cat)\" @E@"
  bash deny "cat <<X > out; cat @E@@NL@body@NL@X"
  bash deny "bash -c 'true' && cat @E@"
  bash allow "docker compose --env-file @E@ up -d"
  bash deny "python3 - <<'EOF'@NL@import subprocess; subprocess.run(['cat', '@E@'])@NL@EOF"
  bash allow "echo \"\${#arr[@]}\" && ls @E@"
  bash deny "cat - @E@ <<X@NL@x@NL@X"
)

@test "secret-read-guard: rule 3 — case-table proposal (cases.sh + extra.sh)" {
  check_rows "${rows_proposal[@]}"
}

rows_prose_denied=(
  bash deny "gh pr create --title t --body \"\$(cat <<'EOF'@NL@use python3 -c 'open(\"@E@\")'@NL@EOF@NL@)\""
  bash deny "ls @E@ && cat README.md"
  bash deny "ls @E@ 2>&1 | head -3"
  bash deny "cat README.md 2>&1 | tail -3; test -f @E@"
  bash deny "git commit -m 'docs: never cat @E@' && git push"
  bash deny "cat > docs/setup.md <<'X'@NL@Run source @E@ before starting.@NL@X@NL@git add docs"
  bash deny "git commit -m 'docs: never cat @E@'"
  bash deny "gh pr create --body 'Guard blocks cat @E@ and grep over @E@'"
  bash deny "cat > docs/setup.md <<'X'@NL@Run source @E@ before starting.@NL@X"
  bash deny "git commit -m \"\$(cat <<'EOF'@NL@fix: \"don't\" cat @E@ ever@NL@EOF@NL@)\" && git push"
  bash deny "gh pr create --title x --body \"\$(cat <<'EOF'@NL@## Summary@NL@Blocks \`cat @E@\` and \"grep @E@\"; don't.@NL@EOF@NL@)\""
  bash deny "echo 'never cat @E@' | tee -a notes.md"
  bash deny "ls -la @E@ | head"
  bash deny "crew status w blocked 'a key surfaced via cat @E@; rotate it'"
  bash deny "git log --oneline | head -3; ls @E@"
  bash deny "cat <<EOF@NL@\$(date) see @E@ docs, cat it never@NL@EOF"
  bash deny "test -f @E@ || cat README.md"
  bash deny "cat <<'A' <<'B'@NL@x@NL@A@NL@y cat @E@@NL@B"
  bash deny "echo x \\@NL@cat @E@"
  bash deny "git commit -m fix -m \"\$(printf 'cat @E@')\""
  bash deny "cat README.md # see @E@"
  bash deny "cat > .gitignore <<EOF@NL@@E@@NL@node_modules@NL@EOF"
  bash deny "ls -la @E@; head -20 README.md"
  bash deny "git commit -m \"fix: python -c open('@E@') was wrong\""
  bash deny "cat > .gitignore <<'EOF'@NL@@E@*@NL@EOF"
)

@test "secret-read-guard: rule 3 — prose naming a read stays denied" {
  check_rows "${rows_prose_denied[@]}"
}



rows_readers_and_git_forms=(
  bash deny "sort @E@ | head"
  bash deny "sort @E@ | grep KEY"
  bash deny "uniq @E@ | cat"
  bash deny "tr -d '\r' < @E@ | grep API"
  bash deny "column -t -s= @E@ | head -5"
  bash deny "fold -w 80 @E@ | head"
  bash deny "tee /dev/null < @E@ | cat"
  bash deny "sort @E@ > /tmp/x; cat /tmp/x"
  bash deny "dd if=@E@ of=/tmp/x; cat /tmp/x"
  bash deny "base64 -d < @E@ > /tmp/x && head /tmp/x"
  bash deny "git diff --exit-code -- @E@ | head"
  bash deny "git diff --stat -p -- @E@"
  bash deny "git status -vv -- @E@ | head"
  bash deny "git commit --dry-run -v -- @E@"
  bash deny "date -f @E@"
  bash deny "file -f @E@"
  bash deny "wc --files0-from=@E@ 2>&1 | cat"
  bash deny "git -c color.ui=never show HEAD:@E@"
  bash deny "git --config-env core.pager=P show HEAD:@E@"
  bash allow "grep -c KEY @E@; echo ok"
  bash allow "git diff --stat -- @E@"
  bash allow "git diff --quiet -- @E@ && echo same"
  bash allow "date +%s; ls @E@"
  bash allow "wc -l @E@"
  bash allow "git status --short"
  bash allow "git -c user.name=x commit -m 'fix: docs'"
  bash allow "date -d yesterday +%F"
)

@test "secret-read-guard: rule 3 — readers without a printing word, printing git forms, git global options" {
  check_rows "${rows_readers_and_git_forms[@]}"
}

rows_tokeniser_edges=(
  bash deny $'echo $$\'\\\' ; cat @E@ #\''
  bash deny $'cat <<"a\'b"@NL@x@NL@a\'b@NL@cat @E@'
  bash deny $'cat <<\'a\\b\'@NL@x@NL@a\\b@NL@cat @E@'
  bash deny $'cat <<"a\\\\b"@NL@x@NL@a\\b@NL@cat @E@'
  bash deny $'cat <<EOF@NL@hi@NL@EO\\@NL@F@NL@cat @E@'
  bash deny $'echo $[1<<2]@NL@cat @E@'
  bash deny $'echo ${x:-<<b}@NL@cat @E@'
  bash deny $'echo ${a[1<<2]}@NL@cat @E@'
  bash deny $'a[1<<2]=3@NL@cat @E@'
  bash deny $'echo `echo \'`; cat @E@ # \'`'
  bash deny $'echo "`echo "a`"; cat @E@'
  bash deny $'cat <<$\'EOF\'@NL@x@NL@EOF@NL@cat @E@'
  bash deny $'cat <<$\'E\\x4fF\'@NL@x@NL@EOF@NL@cat @E@'
  bash deny $'cat > x.sh <<\'EOF\'@NL@cat @E@@NL@EOF@NL@. x.sh'
  bash deny $'echo \'cat @E@\' > x.sh; . x.sh'
  bash deny $'x=$(( 1 <<2 ))@NL@cat @E@'
  bash deny $'echo "$(cat <<\'EOF\'@NL@hi@NL@EOF)"@NL@cat @E@'
  bash deny $'git commit -m "$(cat <<\'EOF\'@NL@fix: thing@NL@EOF)"@NL@head -3 @E@'
  bash deny $'cat <<EOF@NL@x\\@NL@EOF@NL@echo it\'s@NL@EOF@NL@cat @E@ #\''
  bash deny $'echo "${x:-\'"\'}" ; cat @E@ #\''
  bash deny $'echo "`echo "`echo "; cat @E@ #"`'
  bash deny $'echo $((echo <<\'EOF\'@NL@it\'s@NL@EOF@NL@) ); cat @E@ #\''
  bash deny $'echo \'cat @E@\' > x.sh && . ./x.sh'
  bash deny $'printf \'cat %s\\n\' @E@ > x.sh; . x.sh'
  bash deny $'echo \'cat @E@\' > x.sh; source x.sh'
  bash deny $'echo \'cat @E@\' > x.sh; bash x.sh'
  bash deny $'echo "`cat <<\'EOF\'@NL@hi@NL@EOF`"@NL@cat @E@'
  bash deny $'gh pr create --title t --body "$(cat <<\'EOF\'@NL@body@NL@EOF)"@NL@grep KEY @E@'
  bash deny $'grep KEY\r-e -c @E@'
  bash deny $'grep KEY\r-- -c @E@'
  bash deny $'grep KEY\x0b-e -c @E@'
)

@test "secret-read-guard: rule 3 — heredocs, substitutions and quoting a tokeniser can misread" {
  check_rows "${rows_tokeniser_edges[@]}"
}

rows_ctrl_chars_and_grep_paths=(
  bash deny $'grep\r-s\rKEY\r@E@\rx\v-c'
  bash deny $'fish -c "grep\r-s\rKEY\r@E@\rx\v-c"'
  bash deny $'c=grep\r-s\rKEY\r@E@\rx\v-c; IFS='"\$'\\r'"'; '"\$c"
  bash deny $'grep\vKEY\v@E@ -c'
  bash deny $'grep\f-q KEY @E@'
  bash deny $'grep KEY @E@ x\v-c'
  grep deny "id_@E@.examplersa" ""
  grep deny "x.p@E@.exampleem" ""
  grep deny "a/.aw@E@.samples/credentials" ""
  grep deny "a" "*.p@E@.distem"
  grep deny "a" "@E@.example*"
  bash allow "rg -n '\\@E@' docs/"
  bash allow "grep -rn '\\@E@' src/"
  bash allow "grep -c KEY @E@"
)

@test "secret-read-guard: rule 3 — control characters in grep stages, Grep-tool template spellings" {
  check_rows "${rows_ctrl_chars_and_grep_paths[@]}"
}

# check_all_rows_under <awk-cmd> — replay every rows_* table through check_rows
# with <awk-cmd> spliced onto PATH as `awk`, to confirm rule 3's verdicts hold
# under a non-GNU awk. Skips when <awk-cmd> isn't installed.
check_all_rows_under() {
  local awk_cmd=$1 awk_path name out mismatches=''
  local -a arr_names

  awk_path=$(command -v "$awk_cmd") || skip "$awk_cmd not installed"

  mkdir -p "$BATS_TEST_TMPDIR/alt"
  ln -s "$awk_path" "$BATS_TEST_TMPDIR/alt/awk"

  mapfile -t arr_names < <(compgen -A variable rows_)
  [ "${#arr_names[@]}" -gt 0 ]

  for name in "${arr_names[@]}"; do
    local -n rows="$name"
    if ! out=$(PATH="$BATS_TEST_TMPDIR/alt:$PATH" check_rows "${rows[@]}"); then
      mismatches+=$(printf '%s\n' "$out" | sed "s/^/$name: /")
      mismatches+=$'\n'
    fi
  done

  if [ -n "$mismatches" ]; then
    echo "$mismatches"
    return 1
  fi
}

@test "secret-read-guard: rule 3 agrees under mawk" {
  check_all_rows_under mawk
}

@test "secret-read-guard: rule 3 agrees under nawk" {
  check_all_rows_under nawk
}

@test "secret-read-guard: rule 3 agrees under busybox awk" {
  check_all_rows_under busybox-awk
}

@test "secret-read-guard: a rule-3 awk failure fails loud" {
  local real_awk old_path
  real_awk=$(command -v awk)
  mkdir -p "$BATS_TEST_TMPDIR/bin"
  cat > "$BATS_TEST_TMPDIR/bin/awk" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
  case "\$a" in
    *'function grep_loud'*) exit 2 ;;
  esac
done
exec "$real_awk" "\$@"
EOF
  chmod +x "$BATS_TEST_TMPDIR/bin/awk"

  old_path=$PATH
  PATH="$BATS_TEST_TMPDIR/bin:$PATH"
  run --separate-stderr run_guard <<<"$(claude_bash 'cat .env')"
  PATH=$old_path
  [ "$status" -eq 1 ]
  [ -z "$output" ]
  [[ $stderr == *"credential-read check failed; guard NOT enforcing"* ]]
}

# The wide name strip is the one template-stripping awk run with -v wide=1 (the
# narrow one gets wide=0), so an awk shim's log tells them apart.
@test "secret-read-guard: the wide name strip runs only when the template-anywhere name test misses" {
  local real_awk log
  real_awk=$(command -v awk)
  log="$BATS_TEST_TMPDIR/awk.log"
  mkdir -p "$BATS_TEST_TMPDIR/bin"
  cat >"$BATS_TEST_TMPDIR/bin/awk" <<EOF
#!/usr/bin/env bash
[[ \${2:-} == wide=1 ]] && echo wide >>"$log"
exec "$real_awk" "\$@"
EOF
  chmod +x "$BATS_TEST_TMPDIR/bin/awk"

  local old_path=$PATH
  PATH="$BATS_TEST_TMPDIR/bin:$PATH"

  # (a) Both `.env` names are caught by the narrow regex once the template
  # suffix is stripped, so the wide strip never runs.
  : >"$log"
  run --separate-stderr run_guard <<<"$(claude_bash 'cat .env .env.example')"
  assert_deny_claude
  run grep -qx wide "$log"
  [ "$status" -ne 0 ]

  # (b) The narrow regex requires a space/quote/=// before `.env`; `{.env,`
  # has none, so only the wide strip (run exactly once) catches it.
  : >"$log"
  run --separate-stderr run_guard <<<"$(claude_bash 'cat {.env,.env.local}')"
  assert_deny_claude
  [ "$(grep -cx wide "$log")" -eq 1 ]

  # (c) Grep tool: the narrow regex hits on both the path and the glob
  # (a bare `.env` needs no wide fallback here), so the wide strip never runs
  # for either field.
  : >"$log"
  run --separate-stderr run_guard <<<"$(claude_grep "config/.env" "*.env" "x" content)"
  assert_deny_claude
  run grep -qx wide "$log"
  [ "$status" -ne 0 ]

  # (d) Grep tool, glob field: `.env.examples` is not a template name (it
  # ends in "examples", not "example"), so the narrow regex misses and the
  # wide strip is needed to catch it.
  : >"$log"
  run --separate-stderr run_guard <<<"$(claude_grep "" ".env.examples" "x" content)"
  assert_deny_claude
  run grep -qx wide "$log"
  [ "$status" -eq 0 ]

  # (e) Bash, across spaces: the top level misses the narrow test (`x.env` has
  # no leading separator) but the `-c` body hits it, so the wide strip must not
  # run on the top level once another space is denied.
  : >"$log"
  run --separate-stderr run_guard <<<"$(claude_bash 'bash -c cat\ \.env; x.env')"
  assert_deny_claude
  run grep -qx wide "$log"
  [ "$status" -ne 0 ]

  # (f) Grep tool, across fields: the path hits the narrow test, so the wide
  # strip must not run on the glob that missed it.
  : >"$log"
  run --separate-stderr run_guard <<<"$(claude_grep "config/.env" "*.ts" "x" content)"
  assert_deny_claude
  run grep -qx wide "$log"
  [ "$status" -ne 0 ]

  PATH=$old_path
}

@test "secret-read-guard: a failing template strip in the name test fails loud" {
  local real_awk
  real_awk=$(command -v awk)
  mkdir -p "$BATS_TEST_TMPDIR/bin"
  cat >"$BATS_TEST_TMPDIR/bin/awk" <<EOF
#!/usr/bin/env bash
for a in "\$@"; do
  case "\$a" in
    *'function tword'*) exit 4 ;;
  esac
done
exec "$real_awk" "\$@"
EOF
  chmod +x "$BATS_TEST_TMPDIR/bin/awk"

  local old_path=$PATH
  PATH="$BATS_TEST_TMPDIR/bin:$PATH"
  run --separate-stderr run_guard <<<"$(claude_bash 'cat .env')"
  PATH=$old_path
  [ "$status" -eq 1 ]
  [ -z "$output" ]
  [[ $stderr == *"credential-read check failed; guard NOT enforcing"* ]]
}
