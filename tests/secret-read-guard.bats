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

# cursor preToolUse Read/Grep keys confirmed by a live capture from
# cursor-agent 2026.09.26-dd393fe on 2026-09-27.
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
# cursor preToolUse Read/Grep
# cursor-agent 2026.09.26-dd393fe on 2026-09-27
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

# cursor-agent 2026.09.26-dd393fe on 2026-09-27. Fixtures are that capture with
# home paths rewritten to /scratch/fake/ and session fields stripped.
@test "secret-read-guard: captured cursor preToolUse Read of a fake path allows" {
  run run_guard < "$BATS_TEST_DIRNAME/fixtures/cursor-pretool-read.json"
  assert_allow
}

@test "secret-read-guard: captured cursor preToolUse Grep of a fake path allows" {
  run run_guard < "$BATS_TEST_DIRNAME/fixtures/cursor-pretool-grep.json"
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

# The per-awk bound is RELATIVE: K x a same-size, same-shape calibration input
# (a linear, trivially-judged variant of the test's own payload), plus an
# absolute ceiling for a hung guard. A fixed absolute ms was bumped repeatedly
# (#478, #491, #507, #511, #513, #515) because the slowest non-gawk awk drifts
# toward it on slow runners; the relative bound tracks the runner's speed.
#
# #514 showed a single shared calibration cannot catch the historical
# strip_templates regression on busybox-awk: that regression is a bash-sed
# quadratic whose cost is nearly awk-independent, while a shared prose
# calibration is awk-speed-bound, so busybox's inflated denominator masks it.
# The fix is a per-construct calibration — a same-size linear variant of the
# same payload, so the calibration traverses the same awk passes and the same
# strip path and the ratio is ~1 for legitimate runs on every awk.
#
# K=4 gives >=3x headroom over the worst legitimate payload:calibration ratio
# measured locally (~1.25); the pre-#476 sed -E revert measures ~4.3-5.0 on
# busybox-awk, ~7-9 on nawk, ~30 on mawk (see #514). The ceiling is a backstop
# only; the worst legitimate payload is ~1.4 s locally (~4.2 s on CI's ~3x
# slower runner), well under 8000 ms. See #514/#515.
SECRET_GUARD_TIMING_K=4
SECRET_GUARD_TIMING_CEILING_MS=8000

# assert_deny_relative <calibration-payload> <payload> — deny in claude shape,
# within K x the calibration and the ceiling. The calibration is timed first in
# this shell; any verdict it reaches is fine.
assert_deny_relative() {
  local calib_ms elapsed start bound
  start=$(date +%s%N)
  run run_guard <<<"$1"
  calib_ms=$((($(date +%s%N) - start) / 1000000))
  [ "$status" -eq 0 ] || return 1
  ((calib_ms < 1)) && calib_ms=1
  start=$(date +%s%N)
  run run_guard <<<"$2"
  elapsed=$((($(date +%s%N) - start) / 1000000))
  assert_deny_claude
  bound=$((calib_ms * SECRET_GUARD_TIMING_K))
  echo "elapsed ${elapsed}ms <= ${SECRET_GUARD_TIMING_K} x calibration ${calib_ms}ms = ${bound}ms (ceiling ${SECRET_GUARD_TIMING_CEILING_MS}ms)" >&2
  [ "$elapsed" -le "$SECRET_GUARD_TIMING_CEILING_MS" ] && [ "$elapsed" -le "$bound" ]
}

# assert_deny_within_each_awk <calibration-payload> <payload> — replay the
# relative bound under every non-GNU awk this host has installed (mawk, nawk,
# busybox-awk), each spliced onto PATH ahead of the real awk. Falls back to a
# single run under the default awk when none of those are installed.
assert_deny_within_each_awk() {
  local calib=$1 payload=$2 name awk_path found=0

  for name in mawk nawk busybox-awk; do
    awk_path=$(command -v "$name") || continue
    found=1
    mkdir -p "$BATS_TEST_TMPDIR/$name"
    ln -sf "$awk_path" "$BATS_TEST_TMPDIR/$name/awk"
    PATH="$BATS_TEST_TMPDIR/$name:$PATH" assert_deny_relative "$calib" "$payload"
  done

  [ "$found" -eq 1 ] || assert_deny_relative "$calib" "$payload"
}

# A catastrophic backtracking (the scenario these tests exist to catch) shows
# up as tens of seconds, not the ~1 s these cases take locally, so the relative
# headroom and the ceiling are both safe.

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

# assert_allow_relative <calibration-payload> <payload> — the allow twin of
# assert_deny_relative (see the K/ceiling comment there).
assert_allow_relative() {
  local calib_ms elapsed start bound
  start=$(date +%s%N)
  run run_guard <<<"$1"
  calib_ms=$((($(date +%s%N) - start) / 1000000))
  [ "$status" -eq 0 ] || return 1
  ((calib_ms < 1)) && calib_ms=1
  start=$(date +%s%N)
  run run_guard <<<"$2"
  elapsed=$((($(date +%s%N) - start) / 1000000))
  assert_allow
  bound=$((calib_ms * SECRET_GUARD_TIMING_K))
  echo "elapsed ${elapsed}ms <= ${SECRET_GUARD_TIMING_K} x calibration ${calib_ms}ms = ${bound}ms (ceiling ${SECRET_GUARD_TIMING_CEILING_MS}ms)" >&2
  [ "$elapsed" -le "$SECRET_GUARD_TIMING_CEILING_MS" ] && [ "$elapsed" -le "$bound" ]
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
@test "secret-read-guard: a 60 KB slash-free word beside a credential read denies within K x its calibration under every awk" {
  local hex benign
  hex=$(printf 'ab%.0s' $(seq 1 30000))
  benign=$(printf 'aa%.0s' $(seq 1 30000))
  assert_deny_within_each_awk \
    "$(claude_bash "head -c 64 .env && printf %s ${benign} | xxd -r -p > blob.bin")" \
    "$(claude_bash "head -c 64 .env && printf %s ${hex} | xxd -r -p > blob.bin")"
}

# bats test_tags=timing
@test "secret-read-guard: a 48 KB chain of credential names denies within K x its calibration under every awk" {
  local body benign
  body=$(printf '.env%.0s' $(seq 1 12000))
  benign=$(printf 'a.bc%.0s' $(seq 1 12000))
  assert_deny_within_each_awk \
    "$(claude_bash "cat .env ${benign}")" \
    "$(claude_bash "cat .env ${body}")"
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

# The `-c` finder matches on the dequoted view and decode_word starts at the raw
# index the view maps back to: a wrong view hides an interpreter, a wrong index
# decodes the wrong word. Rows: raw text, its dequoted view, then the dequoted
# indices to map and the raw indices they must map to.
@test "secret-read-guard: dequote and dequote_index give bash's quote removal and its raw offsets, under every awk" {
  local awk_chars awk_mask_cmd name awk_path row raw view ks want got
  eval "$(sed -n "/^awk_chars='/,/^}'\$/p" "$GUARD")"
  eval "$(sed -n "/^awk_mask_cmd='/,/^}'\$/p" "$GUARD")"
  eval "$(sed -n '/^dequote() {/,/^}/p; /^dequote_index() {/,/^}/p' "$GUARD")"
  eval "$(grep -E '^shell_c_(interp|word|flag|body|re)=' "$GUARD")"
  local -a rows=(
    $'b\\ash -c \'env\'\tbash -c env\t1 8\t2 10'
    $'"bash" \'-c\' x\tbash -c x\t8\t12'
    $'$\'bash\' -c env\tbash -c env\t0\t2'
    $'echo \'a b\'#c\techo a_b#c\t6\t7'
    $'cat <<EOF\nit\'s\nEOF\nb\\ash -c env\tcat <<   \nit\'s\nEOF\nbash -c env\t20\t21'
    $'echo "a $(b\\ash -c \'env\') c"\techo a_$(bash -c env)_c\t10\t12'
    $'bash -c -e env\tbash -c -e env\t10\t10'
  )
  local -a awks=("")
  for name in mawk nawk busybox-awk; do
    awk_path=$(command -v "$name") || continue
    mkdir -p "$BATS_TEST_TMPDIR/$name"
    ln -sf "$awk_path" "$BATS_TEST_TMPDIR/$name/awk"
    awks+=("$BATS_TEST_TMPDIR/$name")
  done
  for row in "${rows[@]}"; do
    IFS=$'\t' read -r -d '' raw view ks want <<<"$row" || true
    want=${want%$'\n'}
    for awk_path in "${awks[@]}"; do
      got=$(PATH="${awk_path:+$awk_path:}$PATH" dequote "$raw")
      [[ $got == "$view" ]] || {
        echo "${awk_path:-default awk}: dequote '$raw' -> '$got', want '$view'" >&2
        return 1
      }
      got=$(PATH="${awk_path:+$awk_path:}$PATH" dequote_index "$raw" "$ks")
      [[ ${got//$'\n'/ } == "$want" ]] || {
        echo "${awk_path:-default awk}: dequote_index '$raw' '$ks' -> '$got', want '$want'" >&2
        return 1
      }
    done
  done
  # The finder's match on `bash -c -e env` takes the `-e` option too, so the
  # body decode_word gets starts at the `e` of `env`.
  raw='bash -c -e env'
  [[ $raw =~ $shell_c_re ]]
  [[ ${BASH_REMATCH[0]} == 'bash -c -e ' ]]
  got=$(dequote_index "$raw" $((${#BASH_REMATCH[0]} - 1)))
  [[ ${raw:got+1} == env ]]
}

@test "secret-read-guard: strip_escapes removes unquoted backslashes the way bash does, under every awk" {
  local awk_chars awk_escapes name awk_path i got
  eval "$(sed -n "/^awk_chars='/,/^}'\$/p" "$GUARD")"
  eval "$(sed -n "/^awk_escapes='/,/^}'\$/p" "$GUARD")"
  eval "$(sed -n '/^strip_escapes() {/,/^}/p' "$GUARD")"
  local -a raws=(
    'p\rintenv'
    'a\\b'
    'a\\\b'
    $'a\\\nb'
    'tail\'
    'x\\'
    $'one\ntw\\o'
    'plain'
  )
  local -a wants=(
    'printenv'
    'a\b'
    'a\b'
    'ab'
    'tail\'
    'x\'
    $'one\ntwo'
    'plain'
  )
  local -a awks=("")
  for name in mawk nawk busybox-awk; do
    awk_path=$(command -v "$name") || continue
    mkdir -p "$BATS_TEST_TMPDIR/$name"
    ln -sf "$awk_path" "$BATS_TEST_TMPDIR/$name/awk"
    awks+=("$BATS_TEST_TMPDIR/$name")
  done
  for i in "${!raws[@]}"; do
    for awk_path in "${awks[@]}"; do
      got=$(PATH="${awk_path:+$awk_path:}$PATH" strip_escapes "${raws[i]}")
      [[ $got == "${wants[i]}" ]] || {
        echo "${awk_path:-default awk}: strip_escapes '${raws[i]}' -> '$got', want '${wants[i]}'" >&2
        return 1
      }
    done
  done
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB chain of credential names ahead of a template name denies within K x its calibration under every awk" {
  local body
  body=$(printf '.env%.0s' $(seq 1 25000))
  # Calibration neutralizes the template (12 chars to match `.env.example`): the
  # same strip pass runs, but the pre-#476 sed -E quadratic cannot trigger.
  assert_deny_within_each_awk \
    "$(claude_bash "cat .env ${body} aaaaaaaaaaaa")" \
    "$(claude_bash "cat .env ${body} .env.example")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB chain and a template name beside a bash -c credential read deny within K x its calibration under every awk" {
  local body
  body=$(printf '.env%.0s' $(seq 1 25000))
  assert_deny_within_each_awk \
    "$(claude_bash "bash -c cat\\ \\.env; x${body} aaaaaaaaaaaa")" \
    "$(claude_bash "bash -c cat\\ \\.env; x${body} .env.example")"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB chain and a template name in a Grep path deny on the glob within K x its calibration under every awk" {
  local body
  body=$(printf '.env%.0s' $(seq 1 25000))
  # A Grep-shaped calibration, not a Bash one: a bash command's calibration is
  # inflated by unrelated shell-branch awk passes and would mask the regression.
  assert_deny_within_each_awk \
    "$(claude_grep "x${body} aaaaaaaaaaaa" '.env' 'x' content)" \
    "$(claude_grep "x${body} .env.example" '.env' 'x' content)"
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

run_guard_bare() {
  env -i PATH="$PATH" bash "$GUARD"
}

@test "secret-read-guard: runs standalone with only PATH in the environment" {
  run --separate-stderr run_guard_bare <<<"$(claude_bash 'cat .env')"
  assert_deny_claude
  run --separate-stderr run_guard_bare <<<"$(claude_bash 'ls')"
  [ "$status" -eq 0 ]
  [ -z "$output" ]
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

# keep_row runs one row under set -e and keeps going. finish_rows fails once,
# naming every row that failed. A short read must not pass with zero rows.
begin_rows() {
  ROW_FAILS=()
  ROW_N=0
}

keep_row() {
  local id=$1 err rc
  shift
  local -a cmd=("$@")
  ROW_N=$((ROW_N + 1))
  set +e
  err=$(
    set -e
    trap - ERR
    "${cmd[@]}" 2>&1
  )
  rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    ROW_FAILS+=("$id")
    printf 'row %s failed\n' "$id" >&2
    if [ -n "$err" ]; then
      printf '%s\n' "$err" >&2
    fi
    BATS_ERROR_STATUS=
    BATS_ERROR_SUFFIX=
  fi
}

finish_rows() {
  local want=$1
  if [ "$ROW_N" -ne "$want" ]; then
    printf 'expected %s rows, ran %s\n' "$want" "$ROW_N" >&2
    return 1
  fi
  if [ "${#ROW_FAILS[@]}" -gt 0 ]; then
    printf 'failed rows: %s\n' "${ROW_FAILS[*]}" >&2
    return 1
  fi
}

# F76: egrep/zgrep and rg -L/-rl, folded. Stateless: run_guard, no shared fixture.
@test "secret-read-guard: denies egrep zgrep rg -L and rg -rl" {
  begin_rows
  local row cmd
  while IFS='|' read -r row cmd; do
    [ -n "$row" ] || continue
    keep_row "$row" deny_cmd "$cmd"
  done <<'ROWS'
egrep|egrep KEY .env
zgrep|zgrep KEY .env
rg-L|rg -L KEY .env
rg-rl|rg -rl KEY .env
ROWS
  finish_rows 4
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

# F77: deny_cmd rows folded from direnv exec, a shell-variable dump followed by
# a comment, a dump followed by a comment or a descriptor redirect, and a
# relative dumper path without '=', and a dump glued to an output redirect
# (#692). Stateless: run_guard, no shared fixture.
@test "secret-read-guard: denies direnv comment redirect and relative-path dumps" {
  begin_rows
  local row cmd
  while IFS='|' read -r row cmd; do
    [ -n "$row" ] || continue
    keep_row "$row" deny_cmd "$cmd"
  done <<'ROWS'
direnv-dot-env|direnv exec . env
direnv-dot-printenv|direnv exec . printenv
direnv-pwd-env|direnv exec "$PWD" env
set-comment|set # list
declare-p-comment|declare -p # list
export-p-comment|export -p # list
env-comment|env # show vars
env-fd-redirect|env 2>&1
printenv-devnull|printenv 2>/dev/null
bin-env|bin/env
bin-printenv|bin/printenv
x-env|x/env
env-glued-redirect|env>/tmp/x
env-glued-append|env>>/tmp/x
env-glued-clobber|env>|/tmp/x
env-glued-amp-redirect|env&>/tmp/x
env-fd2-file|env 2>/tmp/x
env-i-glued-redirect|env -i>/tmp/x
env-0-glued-redirect|env -0>/tmp/x
abs-env-glued-redirect|/usr/bin/env>/tmp/x
printenv-glued-redirect|printenv>/tmp/x
set-glued-redirect|set>/tmp/x
export-glued-redirect|export>/tmp/x
declare-glued-redirect|declare>/tmp/x
env-glued-dup|env>&2
ROWS
  finish_rows 25
}

@test "secret-read-guard: allows direnv without a dump" {
  allow_cmd 'direnv exec . make test'
  allow_cmd 'direnv allow'
  allow_cmd 'direnv exec . env FOO=1 mycmd'
}

# F78: deny_cmd rows folded from env/printenv options with no command, and a
# dumper behind env and sudo, and an env assignment whose name is not an
# identifier (#692). Stateless: run_guard, no shared fixture.
@test "secret-read-guard: denies option-only env and wrapped dumpers" {
  begin_rows
  local row cmd
  while IFS='|' read -r row cmd; do
    [ -n "$row" ] || continue
    keep_row "$row" deny_cmd "$cmd"
  done <<'ROWS'
env-0|env -0
env-u|env -u X
env-C|env -C /tmp
env-assign|env FOO=1
env-i-sort|env -i FOO=1 | sort
printenv-0|printenv -0
env-u-env|env -u X env
env-assign-printenv|env FOO=1 printenv
sudo-u-root|sudo -u root env
sudo-Eu|sudo -Eu root env
sudo-declare|sudo -u root declare -p X
sudo-quoted-root|sudo -u "root" env
env-nonident-assign|env a-b=1
env-nonident-two|env a-b=1 c.d=2
env-ident-then-nonident|env FOO=1 a-b=1
env-dot-assign-sort|env a.b=1 | sort
env-digit-assign|env 1x=2
env-i-nonident|env -i a-b=1
env-empty-name|env =x
env-nonident-printenv|env a-b=1 printenv
env-nonident-glued-redirect|env a-b=1>/tmp/x
ROWS
  finish_rows 21
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

# F79: deny_cmd rows folded from a wrapper with a backticked argument, a
# wrapper whose option argument is a substitution (#452, #484), and a relative
# dumper path holding '=' (#683) or shaped like an append or subscript
# assignment bash still runs as a command (#706). Stateless: run_guard, no
# shared fixture.
@test "secret-read-guard: denies substituted wrapper args and equals-paths" {
  begin_rows
  local row cmd
  while IFS='|' read -r row cmd; do
    [ -n "$row" ] || continue
    keep_row "$row" deny_cmd "$cmd"
  done <<'ROWS'
sudo-backtick|sudo -u `whoami` env
direnv-backtick|direnv exec `pwd` env
env-backtick|env -C `pwd` env
printenv-backtick|printenv `echo` GITHUB_TOKEN
sudo-sub-un|sudo -u $(id -un) env
sudo-sub-id|sudo -u $(id) env
direnv-sub|direnv exec $(pwd) env
printenv-sub|printenv $(echo) GITHUB_TOKEN
rel-dot-eq|./a=b/env
rel-tilde-eq|~/a=b/env
rel-mid-eq|a/b=c/env
rel-digit-eq|1a=b/env
sub-unclosed|a[/env
sub-unclosed-long|ab[c/env
sub-unclosed-printenv|x[/printenv MY_TOKEN
sub-unclosed-tmux|x[/tmux show-environment
sub-closed-no-eq|a[1]/env
sub-plus-no-eq|a[1]+/env
plus-mid-eq|a+b=c/env
sub-unclosed-eq|a[=b/env
sub-text-before-eq|a[1]x=deploy/env
sub-plus-plus-eq|a[1]++=deploy/env
sub-double-close|a[1]]=deploy/env
sub-nested|a[[]=d]/env
sub-escaped-close|a[1\]=d]/env
sub-backtick|a[`x]=d`]/env
sub-param-exp|a[${x=]=d}]/env
plus-no-eq|X+/env
plus-plus-eq|X++=deploy/env
brace-glued-sub|{a[1]=d/env
brace-glued-plain|{X=d/env
escaped-lead-sub|\a[1]=d/env
escaped-lead-append|\X+=d/env
ROWS
  finish_rows 33
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
  deny_cmd 'env >/tmp/x'
  deny_cmd 'env > /tmp/x'
  deny_cmd 'printenv >>f'
  deny_cmd 'env >| f'
  deny_cmd 'env &>f'
  deny_cmd 'env {fd}>&2'
}

@test "secret-read-guard: allows a backticked word that is not a dump" {
  allow_cmd $'git commit -m \'run `env` to list\''
  allow_cmd $'git commit -F - <<\'EOF\'\nfix: `env vars`\n`set -e`\n`export FOO=1`\n`ls`\nEOF'
  allow_cmd $'echo `date`\nls'
  allow_cmd $'echo `echo \\`echo \\\\\\`date\\\\\\`\\``'
}

# F80: allow_cmd rows folded from a redirected command that is not a dump, and
# env/sudo running a command, env running a command after a non-identifier
# assignment, and a glued redirect of a non-dumper (#692). Stateless: run_guard,
# no shared fixture.
@test "secret-read-guard: allows redirected and wrapped non-dump commands" {
  begin_rows
  local row cmd
  while IFS='|' read -r row cmd; do
    [ -n "$row" ] || continue
    keep_row "$row" allow_cmd "$cmd"
  done <<'ROWS'
env-i-redir|env -i mycmd >&2
set-redir|set -euo pipefail >&2
export-redir|export FOO=1 >&2
declare-redir|declare -a a 2>/dev/null
env-u-cmd|env -u X mycmd
env-i-cmd|env -i mycmd
env-0-cmd|env -0 mycmd
sudo-n-ls|sudo -n ls
env-nonident-cmd|env a-b=1 cmd
env-nonident-two-cmd|env a-b=1 c.d=2 mycmd
env-assign-cmd|env FOO=1 cmd
env-i-nonident-cmd-redir|env -i a-b=1 mycmd >/tmp/x
ls-glued-redirect|ls>/tmp/x
echo-glued-redirect|echo hi>/tmp/x
make-env-arg|make env=prod
envsubst-glued-redirect|envsubst>/tmp/x
setx-glued-redirect|setx>/tmp/x
env-assign-glued-redirect-cmd|env FOO=a>b cmd
env-i-glued-redirect-cmd|env -i>/tmp/x mycmd
ROWS
  finish_rows 19
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
@test "secret-read-guard: 14 nested levels of escaped backticks ahead of a dump deny within K x its calibration under every awk" {
  assert_deny_within_each_awk \
    "$(claude_bash "$(nested_backticks 14 "echo ok")")" \
    "$(claude_bash "$(nested_backticks 14)")"
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

# assert_allow_within_each_awk <calibration-payload> <payload> — the allow twin
# of assert_deny_within_each_awk.
assert_allow_within_each_awk() {
  local calib=$1 payload=$2 name awk_path found=0

  for name in mawk nawk busybox-awk; do
    awk_path=$(command -v "$name") || continue
    found=1
    mkdir -p "$BATS_TEST_TMPDIR/$name"
    ln -sf "$awk_path" "$BATS_TEST_TMPDIR/$name/awk"
    PATH="$BATS_TEST_TMPDIR/$name:$PATH" assert_allow_relative "$calib" "$payload"
  done

  [ "$found" -eq 1 ] || assert_allow_relative "$calib" "$payload"
}

# bats test_tags=timing
@test "secret-read-guard: a 100 KB backticked commit body allows within K x its calibration under every awk" {
  local body benign
  # 900 reps keeps the whole payload under Linux's 128 KiB single-argv-string
  # cap (MAX_ARG_STRLEN) that claude_bash's jq --arg would otherwise blow.
  body=$(printf 'fix(x): handle `foo` in `bar`\n\nReads `DISPATCHER_X` env var and `set -e`. A stray ` tick.\n`env vars` are documented; `export FOO=1` too.\n%.0s' $(seq 1 900))
  # Same-size, same-backtick-density benign text: the same mask and frame
  # passes run with the same frame-masker cost on every awk.
  benign=$(printf 'fix(x): handle `abc` in `def`\n\nReads `GHIJKLMNOPQR` env var and `set -e`. A stray ` tick.\n`env vars` are documented; `export FOO=1` too.\n%.0s' $(seq 1 900))
  assert_allow_within_each_awk \
    "$(claude_bash "git commit -F - <<'EOF'"$'\n'"$benign"$'\n'"EOF")" \
    "$(claude_bash "git commit -F - <<'EOF'"$'\n'"$body"$'\n'"EOF")"
}

# bats test_tags=timing
@test "secret-read-guard: 14 nested levels of backticks around a harmless command allow within K x its calibration under every awk" {
  assert_allow_within_each_awk \
    "$(claude_bash "$(nested_backticks 14 "echo ok")")" \
    "$(claude_bash "$(nested_backticks 14 date)")"
}

# bats test_tags=timing
@test "secret-read-guard: escape runs inside 14 nested backtick frames allow within K x its calibration under every awk" {
  local body benign
  body=$(printf '\\\\x %.0s' $(seq 1 20000))
  # Same-size escape-run shape with a benign escaped character: the frame pass
  # sees the same backslash/backtick structure, so the ratio is ~1 when linear.
  benign=$(printf '\\\\a %.0s' $(seq 1 20000))
  assert_allow_within_each_awk \
    "$(claude_bash "$(nested_backticks 14 "$benign")")" \
    "$(claude_bash "$(nested_backticks 14 "$body")")"
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

# ---------------------------------------------------------------------------
# Regression cases carried over from a downstream copy of this guard.
# ---------------------------------------------------------------------------

guard_bash() { run --separate-stderr run_guard <<<"$(claude_bash "$1")"; }

guard_json() {
  local p
  p=$(jq -c 'if type == "object" and has("tool_name") then .hook_event_name = "PreToolUse" else . end' <<<"$1")
  run --separate-stderr run_guard <<<"$p"
}

assert_deny() { assert_deny_claude; }

# --- deny: bare dumpers, wrapped and terminated in every accepted way ---

@test "secret-read-guard (merged): deny: bare env" {
  guard_bash 'env'
  assert_deny
}

@test "secret-read-guard (merged): deny: bare printenv" {
  guard_bash 'printenv'
  assert_deny
}

@test "secret-read-guard (merged): deny: absolute path to env" {
  guard_bash '/usr/bin/env'
  assert_deny
}

@test "secret-read-guard (merged): deny: relative path to printenv" {
  guard_bash './bin/printenv'
  assert_deny
}

@test "secret-read-guard (merged): deny: command env" {
  guard_bash 'command env'
  assert_deny
}

@test "secret-read-guard (merged): deny: command -- env" {
  guard_bash 'command -- env'
  assert_deny
}

@test "secret-read-guard (merged): deny: sudo env" {
  guard_bash 'sudo env'
  assert_deny
}

@test "secret-read-guard (merged): deny: sudo -u root env" {
  guard_bash 'sudo -u root env'
  assert_deny
}

@test "secret-read-guard (merged): deny: env terminated by semicolon" {
  guard_bash 'env; ls'
  assert_deny
}

@test "secret-read-guard (merged): deny: env piped to grep" {
  guard_bash 'env | grep X'
  assert_deny
}

@test "secret-read-guard (merged): deny: env after &&" {
  guard_bash 'ls && env'
  assert_deny
}

@test "secret-read-guard (merged): deny: env inside parens" {
  guard_bash '(env)'
  assert_deny
}

@test "secret-read-guard (merged): deny: env followed by comment" {
  guard_bash 'env # c'
  assert_deny
}

@test "secret-read-guard (merged): deny: env with output redirect" {
  guard_bash 'env >/tmp/x'
  assert_deny
}

@test "secret-read-guard (merged): deny: env with fd redirect" {
  guard_bash 'env 2>&1'
  assert_deny
}

@test "secret-read-guard (merged): deny: env on its own line after a newline" {
  guard_bash $'ls\nenv'
  assert_deny
}

@test "secret-read-guard (merged): deny: env -0 still dumps, just NUL-separated" {
  guard_bash 'env -0'
  assert_deny
}

@test "secret-read-guard (merged): deny: bare set" {
  guard_bash 'set'
  assert_deny
}

@test "secret-read-guard (merged): deny: declare -p with no name" {
  guard_bash 'declare -p'
  assert_deny
}

@test "secret-read-guard (merged): deny: declare -p NAME still dumps that variable" {
  guard_bash 'declare -p FOO'
  assert_deny
}

@test "secret-read-guard (merged): deny: export -p" {
  guard_bash 'export -p'
  assert_deny
}

@test "secret-read-guard (merged): deny: typeset -p" {
  guard_bash 'typeset -p'
  assert_deny
}

@test "secret-read-guard (merged): deny: declare -x with no name" {
  guard_bash 'declare -x'
  assert_deny
}

@test "secret-read-guard (merged): deny: set -S NAME always prints" {
  guard_bash 'set -S FOO'
  assert_deny
}

@test "secret-read-guard (merged): deny: tmux show-environment" {
  guard_bash 'tmux show-environment'
  assert_deny
}

@test "secret-read-guard (merged): deny: systemctl --user show-environment" {
  guard_bash 'systemctl --user show-environment'
  assert_deny
}

@test "secret-read-guard (merged): deny: launchctl getenv NAME" {
  guard_bash 'launchctl getenv FOO'
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c 'env'" {
  guard_bash $'bash -c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: sh -c \"printenv\"" {
  guard_bash 'sh -c "printenv"'
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c 'declare -p'" {
  guard_bash $'bash -c \'declare -p\''
  assert_deny
}

@test "secret-read-guard (merged): deny: fish -c 'set -gx' with no name" {
  guard_bash $'fish -c \'set -gx\''
  assert_deny
}

@test "secret-read-guard (merged): deny: fish -c 'set -S'" {
  guard_bash $'fish -c \'set -S\''
  assert_deny
}

@test "secret-read-guard (merged): deny: echo \"\$(env)\" dumps through a substitution" {
  # shellcheck disable=SC2016
  guard_bash 'echo "$(env)"'
  assert_deny
}

@test "secret-read-guard (merged): deny: echo backtick-printenv-backtick" {
  # shellcheck disable=SC2016
  guard_bash 'echo `printenv`'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat /proc/self/environ" {
  guard_bash 'cat /proc/self/environ'
  assert_deny
}

@test "secret-read-guard (merged): deny: echo of a secret-named variable" {
  # shellcheck disable=SC2016
  guard_bash 'echo $MY_API_KEY'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .env" {
  guard_bash 'cat .env'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep over .env with default output" {
  guard_bash 'grep FOO .env'
  assert_deny
}

# --- allow: ordinary, non-dumping uses of the same words ---

@test "secret-read-guard (merged): allow: env FOO=1 make runs make, not a dump" {
  guard_bash 'env FOO=1 make'
  assert_allow
}

@test "secret-read-guard (merged): allow: envsubst is not env" {
  guard_bash 'envsubst < a'
  assert_allow
}

@test "secret-read-guard (merged): allow: echo env, env is just an argument" {
  guard_bash 'echo env'
  assert_allow
}

@test "secret-read-guard (merged): allow: printenv HOME reads a single name" {
  guard_bash 'printenv HOME'
  assert_allow
}

@test "secret-read-guard (merged): allow: rg over a quoted pattern mentioning env" {
  guard_bash $'rg \'env|printenv\' f'
  assert_allow
}

@test "secret-read-guard (merged): allow: command -v env only checks for the binary" {
  guard_bash 'command -v env'
  assert_allow
}

@test "secret-read-guard (merged): allow: echo then env, then is not a real separator here" {
  guard_bash 'echo then env'
  assert_allow
}

@test "secret-read-guard (merged): allow: env -i make runs make in a cleared env" {
  guard_bash 'env -i make'
  assert_allow
}

@test "secret-read-guard (merged): allow: xargs env cmd runs cmd, not a dump" {
  guard_bash 'xargs env cmd'
  assert_allow
}

@test "secret-read-guard (merged): allow: set -x is bash xtrace, not a dump" {
  guard_bash 'set -x'
  assert_allow
}

@test "secret-read-guard (merged): allow: fish -c 'set -gx PATH a' is an assignment" {
  guard_bash $'fish -c \'set -gx PATH a\''
  assert_allow
}

@test "secret-read-guard (merged): allow: bash -c 'true' has no dumper" {
  guard_bash $'bash -c \'true\''
  assert_allow
}

@test "secret-read-guard (merged): allow: cat .env.example is a committed template" {
  guard_bash 'cat .env.example'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -c FOO .env only counts matches" {
  guard_bash 'grep -c FOO .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: test -f .env only checks existence" {
  guard_bash 'test -f .env'
  assert_allow
}

# --- dequoted view: backslash escapes and quote removal ---

@test "secret-read-guard (merged): deny: b\\ash -c 'env', a backslash-escaped interpreter" {
  guard_bash $'b\\ash -c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -\\c 'env', a backslash-escaped -c flag" {
  guard_bash $'bash -\\c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: f\\ish -c 'set -x', a backslash-escaped fish" {
  guard_bash $'f\\ish -c \'set -x\''
  assert_deny
}

@test "secret-read-guard (merged): deny: b''ash -c 'env', an empty single-quote pair in the interpreter" {
  guard_bash $'b\'\'ash -c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: b\"\"ash -c 'env', an empty double-quote pair in the interpreter" {
  guard_bash $'b""ash -c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: \"bash\" -c 'env', a quoted interpreter" {
  guard_bash $'"bash" -c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: bash '-c' 'env', a quoted -c flag" {
  guard_bash $'bash \'-c\' \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: echo 'bash' -c env, a quoted interpreter word" {
  guard_bash $'echo \'bash\' -c env'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat /pro\\c/1/environ, a backslash inside /proc" {
  guard_bash 'cat /pro\c/1/environ'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat /proc/1/env\\iron, a backslash inside environ" {
  guard_bash 'cat /proc/1/env\iron'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat /proc/1/e''nviron, an empty quote pair inside environ" {
  guard_bash $'cat /proc/1/e\'\'nviron'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat \\.env, a backslash before the dot" {
  guard_bash 'cat \.env'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .e''nv, an empty single-quote pair inside .env" {
  guard_bash $'cat .e\'\'nv'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat ./.e\"\"nv, an empty double-quote pair inside .env" {
  guard_bash 'cat ./.e""nv'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .n\\etrc, a backslash inside .netrc" {
  guard_bash 'cat .n\etrc'
  assert_deny
}

@test "secret-read-guard (merged): deny: head .env\\.local, a backslash inside .env.local" {
  guard_bash 'head .env\.local'
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c 'cat \\.env', an escaped .env inside an inner command" {
  guard_bash $'bash -c \'cat \\.env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c 'grep FOO .env', a content-reading grep inside an inner command" {
  guard_bash $'bash -c \'grep FOO .env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .env .env.ex\\ample, an exemption must not cover the plain .env" {
  guard_bash 'cat .env .env.ex\ample'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .env .e''nv.example, an exemption must not cover the plain .env" {
  guard_bash $'cat .env .e\'\'nv.example'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep FOO .env; bash -\\c true, an exemption in one view must not cover another" {
  guard_bash 'grep FOO .env; bash -\c true'
  assert_deny
}

@test "secret-read-guard (merged): deny: fish -c 'set -x', unescaped" {
  guard_bash $'fish -c \'set -x\''
  assert_deny
}

@test "secret-read-guard (merged): deny: cat /proc/1/environ, unescaped" {
  guard_bash 'cat /proc/1/environ'
  assert_deny
}

@test "secret-read-guard (merged): deny: ba\$''sh -c 'env', an empty ANSI-C quote pair in the interpreter" {
  guard_bash $'ba$\'\'sh -c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: \$'bash' -c 'env', an ANSI-C quoted interpreter" {
  guard_bash $'$\'bash\' -c \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: bash \$'-c' 'env', an ANSI-C quoted -c flag" {
  guard_bash $'bash $\'-c\' \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: bash \$\"-c\" 'env', a locale-quoted -c flag" {
  guard_bash $'bash $"-c" \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .e\$''nv, an empty ANSI-C quote pair inside .env" {
  guard_bash $'cat .e$\'\'nv'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat /proc/1/envi\$''ron, an empty ANSI-C quote pair inside environ" {
  guard_bash $'cat /proc/1/envi$\'\'ron'
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c \$\"env\", a locale-quoted -c body" {
  guard_bash 'bash -c $"env"'
  assert_deny
}

@test "secret-read-guard (merged): deny: an escaped interpreter on the line after a comment with an apostrophe" {
  guard_bash $'echo hi # it\'s done\nb\\ash -c env'
  assert_deny
}

@test "secret-read-guard (merged): deny: b\\ash -c with a backslash-newline before the body" {
  guard_bash $'b\\ash -c \\\n  \'env\''
  assert_deny
}

@test "secret-read-guard (merged): deny: x=\$(b\\ash -c env), an escaped interpreter in a substitution" {
  # shellcheck disable=SC2016
  guard_bash 'x=$(b\ash -c env)'
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c 'true'; b\\ash -c env, a second -c after a harmless one" {
  guard_bash $'bash -c \'true\'; b\\ash -c env'
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -o pipefail -c 'env', pipefail is an option argument" {
  guard_bash "bash -o pipefail -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -euo pipefail -c 'env', a cluster with o takes the next word" {
  guard_bash "bash -euo pipefail -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -ce 'env', c inside a cluster before another flag" {
  guard_bash "bash -ce 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c -e 'env', an option after -c before the body" {
  guard_bash "bash -c -e 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: bash --norc -c 'env', a long option before -c" {
  guard_bash "bash --norc -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: dash -c 'env'" {
  guard_bash "dash -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: ksh -c 'env'" {
  guard_bash "ksh -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: mksh -c 'env'" {
  guard_bash "mksh -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: ash -c 'env'" {
  guard_bash "ash -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: busybox sh -c 'env'" {
  guard_bash "busybox sh -c 'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -co pipefail 'env', o and c in one cluster" {
  guard_bash "bash -co pipefail 'env'"
  assert_deny
}

@test "secret-read-guard (merged): allow: bash script.sh -c foo, -c after the script is the script's" {
  guard_bash "bash script.sh -c foo"
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -c x file" {
  guard_bash "grep -c x file"
  assert_allow
}

@test "secret-read-guard (merged): allow: echo \"bash -c env\", a quoted mention" {
  guard_bash 'echo "bash -c env"'
  assert_allow
}

# The raw -c finder reads quoted text as written; kept because a dequoted-only
# finder lets real dumpers through wherever the masker misreads quoting.
@test "secret-read-guard (merged): deny (kept over-scan): echo 'see bash -c env here', a quoted mention" {
  guard_bash "echo 'see bash -c env here'"
  assert_deny
}

@test "secret-read-guard (merged): allow: true # see bash -c 'echo hi', a harmless bash -c mention in a comment" {
  guard_bash "true # see bash -c 'echo hi'"
  assert_allow
}

@test "secret-read-guard (merged): deny: a comment holding a full bash -c 'env'" {
  guard_bash "true # see bash -c 'env'"
  assert_deny
}

# shellcheck disable=SC2016
@test "secret-read-guard (merged): deny: a # inside backticks is not a comment" {
  guard_bash 'echo `true #`; bash -c '"'env'"
  assert_deny
}

# shellcheck disable=SC2016
@test "secret-read-guard (merged): deny: a # inside a nested \${} default is not a comment" {
  guard_bash 'echo ${x:-${y} #}; bash -c '"'env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: a # inside backticks in a nested bash -c body" {
  guard_bash "bash -c 'echo \`true #\`; bash -c env'"
  assert_deny
}

@test "secret-read-guard (merged): deny: bash -c - 'env', a lone - ends options" {
  guard_bash "bash -c - 'env'"
  assert_deny
}

@test "secret-read-guard (merged): allow: bash -o pipefail script.sh, no -c" {
  guard_bash "bash -o pipefail script.sh"
  assert_allow
}

@test "secret-read-guard (merged): deny: cat /proc/1/environ followed by a heredoc larger than the pipe buffer" {
  local body json
  body=$(printf 'line of filler text %s\n' {1..8000})
  ((${#body} > 120000))
  # Through stdin: one argv string this long exceeds the kernel's per-argument limit.
  json=$(printf '%s' "cat /proc/1/environ; cat <<'EOF'"$'\n'"$body"$'\n'"EOF" |
    jq -Rs '{hook_event_name:"PreToolUse",tool_name:"Bash",tool_input:{command:.}}')
  guard_json "$json"
  assert_deny
}

@test "secret-read-guard (merged): allow: bash -c 'echo hi'" {
  guard_bash $'bash -c \'echo hi\''
  assert_allow
}

@test "secret-read-guard (merged): allow: cat README.md" {
  guard_bash 'cat README.md'
  assert_allow
}

@test "secret-read-guard (merged): allow: ls /proc/1/" {
  guard_bash 'ls /proc/1/'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep '\\.env' file, a literal backslash in a pattern" {
  guard_bash $'grep \'\\.env\' file'
  assert_allow
}

@test "secret-read-guard (merged): allow: rg \"\\.env\" src, a literal backslash in a pattern" {
  guard_bash 'rg "\.env" src'
  assert_allow
}

@test "secret-read-guard (merged): allow: echo 'a\\.env', a literal backslash in data" {
  guard_bash $'echo \'a\\.env\''
  assert_allow
}

@test "secret-read-guard (merged): allow: cat .e''nv.example, a quoted-in-the-middle example file" {
  guard_bash $'cat .e\'\'nv.example'
  assert_allow
}

@test "secret-read-guard (merged): allow: rg 'foo|bash -c env x' ., the words sit inside one pattern" {
  guard_bash $'rg \'foo|bash -c env x\' .'
  assert_allow
}

@test "secret-read-guard (merged): allow: test -f \\.env only checks existence" {
  guard_bash 'test -f \.env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -c FOO \\.env only counts matches" {
  guard_bash 'grep -c FOO \.env'
  assert_allow
}

# The raw -c finder takes a CR as whitespace even inside quotes; kept for the
# same reason as the quoted mention above.
@test "secret-read-guard (merged): deny (kept over-scan): echo \"x<CR>bash<CR>-c<CR>env\", a quoted CR" {
  guard_bash $'echo "x\rbash\r-c\renv"'
  assert_deny
}

# --- rule 3: path anchors and per-word exemptions ---

@test "secret-read-guard (merged): deny: cat ~/.netrc, a slash before .netrc" {
  guard_bash 'cat ~/.netrc'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat /x/.netrc, a slash before .netrc" {
  guard_bash 'cat /x/.netrc'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat <.env, a redirect right before .env" {
  guard_bash 'cat <.env'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat<.env, no space around the redirect" {
  guard_bash 'cat<.env'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat {.env,}, a brace-expansion list" {
  guard_bash 'cat {.env,}'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .env .env.example, a template must not exempt the plain .env" {
  guard_bash 'cat .env .env.example'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep KEY .env --null, --null is not a quiet flag" {
  guard_bash 'grep KEY .env --null'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep KEY .env | tee out-full, -full is not a quiet flag" {
  guard_bash 'grep KEY .env | tee out-full'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .e''nv .env\$''.example, empty quote pairs hide the plain .env" {
  guard_bash $'cat .e\'\'nv .env$\'\'.example'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep KEY .e''nv x-\$'l', a literal -l inside a word is not a quiet flag" {
  guard_bash $'grep KEY .e\'\'nv x-$\'l\''
  assert_deny
}

@test "secret-read-guard (merged): deny: grep KEY .env then wc -l on the next line, the -l belongs to wc" {
  guard_bash $'grep KEY .env\nwc -l'
  assert_deny
}

@test "secret-read-guard (merged): deny: rg -L KEY .env, rg's -L is --follow, not quiet" {
  guard_bash 'rg -L KEY .env'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep KEY .env -- -c, nothing after -- is an option" {
  guard_bash 'grep KEY .env -- -c'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep --label -c KEY .env, -c is the value of --label" {
  guard_bash 'grep --label -c KEY .env'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep -ec .env, -c is the inline value of -e" {
  guard_bash 'grep -ec .env'
  assert_deny
}

@test "secret-read-guard (merged): deny: grep -e KEY -e -c .env, -c is the value of the second -e" {
  guard_bash 'grep -e KEY -e -c .env'
  assert_deny
}

@test "secret-read-guard (merged): deny: ag --silent KEY .env, ag --silent still prints matches" {
  guard_bash 'ag --silent KEY .env'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .env glued to a backtick substitution" {
  guard_bash 'cat .env`true`'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat .env glued to a \$( substitution" {
  guard_bash 'cat .env$(true)'
  assert_deny
}

@test "secret-read-guard (merged): deny: cat ~/.netrc glued to a variable expansion" {
  guard_bash 'cat ~/.netrc$x'
  assert_deny
}

@test "secret-read-guard (merged): allow: grep KEY .env 2>&1 -c, a redirect does not end the segment" {
  guard_bash 'grep KEY .env 2>&1 -c'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -rc KEY .env counts matches" {
  guard_bash 'grep -rc KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -Ec 'A' .env counts matches" {
  guard_bash $'grep -Ec \'A\' .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -i -c KEY .env counts matches" {
  guard_bash 'grep -i -c KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: rg -l KEY .env lists files only" {
  guard_bash 'rg -l KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: rg -c KEY .env counts matches" {
  guard_bash 'rg -c KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -e KEY -c .env counts matches" {
  guard_bash 'grep -e KEY -c .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: ag -l KEY .env lists files only" {
  guard_bash 'ag -l KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: ls -la .env only lists the file" {
  guard_bash 'ls -la .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -c '^KEY=' .env only counts matches" {
  guard_bash $'grep -c \'^KEY=\' .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -q KEY .env prints nothing" {
  guard_bash 'grep -q KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: git check-ignore .env prints no content" {
  guard_bash 'git check-ignore .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: cp .env.example .env writes, never prints" {
  guard_bash 'cp .env.example .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep --count KEY .env only counts matches" {
  guard_bash 'grep --count KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep -nc KEY .env only counts matches" {
  guard_bash 'grep -nc KEY .env'
  assert_allow
}

@test "secret-read-guard (merged): allow: grep KEY config/.env -c, the flag after the file" {
  guard_bash 'grep KEY config/.env -c'
  assert_allow
}

# --- non-Bash tools ---

@test "secret-read-guard (merged): deny: Read .env" {
  guard_json '{"tool_name":"Read","tool_input":{"file_path":".env"}}'
  assert_deny
}

@test "secret-read-guard (merged): allow: Read .env.example" {
  guard_json '{"tool_name":"Read","tool_input":{"file_path":".env.example"}}'
  assert_allow
}

@test "secret-read-guard (merged): deny: Grep .env with output_mode content" {
  guard_json '{"tool_name":"Grep","tool_input":{"path":".env","output_mode":"content"}}'
  assert_deny
}

@test "secret-read-guard (merged): allow: Grep .env with output_mode count" {
  guard_json '{"tool_name":"Grep","tool_input":{"path":".env","output_mode":"count"}}'
  assert_allow
}

@test "secret-read-guard (merged): allow: unrelated tool_name Write" {
  guard_json '{"tool_name":"Write","tool_input":{}}'
  assert_allow
}

@test "secret-read-guard (merged): allow: empty input" {
  guard_json '{}'
  assert_allow
}

# --- cases that need a fix to pass: assert the INTENDED deny ---

@test "secret-read-guard (merged): deny: env -u NAME still dumps the rest of the environment" {
  guard_bash 'env -u NAME'
  assert_deny
}

@test "secret-read-guard (merged): deny: p\\rintenv, a backslash-escaped printenv" {
  guard_bash 'p\rintenv'
  assert_deny
}

@test "secret-read-guard (merged): deny: timeout 5 env still dumps" {
  guard_bash 'timeout 5 env'
  assert_deny
}

# --- the -c finder searches the raw text beside the dequoted view ---

@test "secret-read-guard: -c body after a backslash-space misread as a comment denies" {
  deny_cmd $'echo a\\ #\'\nx\'; bash -c \'env\''
}

@test "secret-read-guard: -c body after an escaped paren and a misread comment denies" {
  deny_cmd $'echo \\(#\'\nx\'; bash -c \'env\''
}

@test "secret-read-guard: -c body after a lone quote inside backticks denies" {
  deny_cmd "echo \`echo '\`; bash -c env"
}

@test "secret-read-guard: -c body after fish's \\' inside single quotes denies" {
  deny_cmd "fish -c \"echo 'it\\'s'; bash -c 'env'\""
  deny_cmd "fish -c \"echo 'don\\'t'; sh -c 'declare -p'\""
}

@test "secret-read-guard: -c body inside a here-string fed to a shell denies" {
  deny_cmd "bash <<< 'true; bash -c env'"
}

@test "secret-read-guard: -c body inside a quoted argument piped into a shell denies" {
  deny_cmd "echo 'x; bash -c env' | bash"
}

@test "secret-read-guard: a bare -- after -c ends the options, so an option-like body is the body" {
  deny_cmd "bash -c -- '--x=;env' y"
}

# --- dumper words spliced by quotes are judged on the dequoted view ---

@test "secret-read-guard: quote-spliced dumper words deny" {
  deny_cmd '"env"'
  deny_cmd 'e""nv'
  deny_cmd "pr'i'ntenv"
  deny_cmd "'env' >/tmp/x"
  deny_cmd 'de"cl"are -p'
  deny_cmd 'ex"po"rt'
}

@test "secret-read-guard: a quoted dumper word as an argument stays allowed" {
  allow_cmd "echo 'see env'"
  allow_cmd 'git commit -m "env"'
}

@test "secret-read-guard: an assignment whose value ends in /env is not a dumper path" {
  allow_cmd 'CONFIG=deploy/env'
  allow_cmd 'DIR=services/env; ls $DIR'
}

@test "secret-read-guard: &> splits a grep stage under POSIX sh, so a later -q does not quiet it" {
  deny_cmd "sh -c 'grep KEY .env &>/dev/null -q'"
}

# claude_bash for a command past Linux's 128 KiB single-argument limit: the
# text reaches jq on stdin, not argv.
big_bash() { printf '%s' "$1" | jq -Rsc '{hook_event_name:"PreToolUse",tool_name:"Bash",tool_input:{command:.}}'; }

# bats test_tags=timing
@test "secret-read-guard: a 128 KiB bash -o chain then a dump denies in linear time" {
  local chain calib i
  chain='' calib=''
  for ((i = 0; i < 16384; i++)); do chain+='bash -o ' calib+='bash -x '; done
  assert_deny_relative "$(big_bash "$calib"$'\n'"env")" "$(big_bash "$chain"$'\n'"env")"
}

# bats test_tags=timing
@test "secret-read-guard: a 128 KiB =/proc/ chain then a credential read denies in linear time" {
  local chain calib i
  chain='' calib=''
  for ((i = 0; i < 18725; i++)); do chain+='=/proc/' calib+='=/proX/'; done
  assert_deny_relative "$(big_bash "$calib"$'\n'"cat .env")" "$(big_bash "$chain"$'\n'"cat .env")"
}

# --- the -c finder's bounds fail closed ---

@test "secret-read-guard: nine option words ahead of -c deny, since the finder stops at eight" {
  deny_cmd "bash -x -x -x -x -x -x -x -x -x -c 'env'"
  deny_cmd "sh -a -b -C -e -f -h -m -n -u -v -x -c 'declare -p'"
  deny_cmd "\"bash\" -x -x -x -x -x -x -x -x -x '-c' 'env'"
  deny_cmd "bash -c \"bash -x -x -x -x -x -x -x -x -x -c env\""
}

@test "secret-read-guard: nine option words after -c deny" {
  deny_cmd "bash -c -x -x -x -x -x -x -x -x -x 'env'"
  deny_cmd "bash -o pipefail -o pipefail -o pipefail -c -x -x -x -x -x -x -x -x -x 'env'"
}

@test "secret-read-guard: ordinary shell options around -c stay allowed" {
  allow_cmd "bash -euo pipefail -c 'make'"
  allow_cmd "bash -e -u -o pipefail -O extglob -c 'make test'"
  allow_cmd "bash -x -x -c 'make'"
  allow_cmd 'sh -e script.sh'
}

@test "secret-read-guard: quoted -c mentions filling the match cap ahead of a real body deny" {
  local pad
  pad=$(printf ' dash -c a%.0s' $(seq 1 21))
  deny_cmd "echo '${pad}'; bash -c 'env'"
  pad=$(printf ' bash -c a%.0s' $(seq 1 21))
  deny_cmd "echo '${pad}'; sh -c 'declare -p'"
}

# Fails closed: the twentieth body is queued but never searched itself.
@test "secret-read-guard: twenty real -c bodies reach the match cap and deny" {
  deny_cmd "$(printf "bash -c 'true'; %.0s" $(seq 1 20))true"
}

@test "secret-read-guard: a few quoted -c mentions beside a harmless body stay allowed" {
  allow_cmd "echo ' dash -c a dash -c a dash -c a'; bash -c 'true'"
}

@test "secret-read-guard: an absolute dumper path holding = denies" {
  deny_cmd '/nix/store/x-a=b/bin/env'
  deny_cmd '/nix/store/x-a=b/bin/printenv'
}

@test "secret-read-guard: a relative assignment ending in /env stays allowed" {
  begin_rows
  local row cmd
  while IFS='|' read -r row cmd; do
    [ -n "$row" ] || continue
    keep_row "$row" allow_cmd "$cmd"
  done <<'ROWS'
plain|a=b/env
append|X+=deploy/env
subscript|a[1]=deploy/env
subscript-append|a[1]+=deploy/env
subscript-empty|a[]=deploy/env
underscore-subscript|_[0]=deploy/env
arith-subscript|a[i+1]=deploy/env
printenv-secret|a[1]+=deploy/printenv GITHUB_TOKEN
after-then|then X+=deploy/env
ROWS
  finish_rows 9
}

# bats test_tags=timing
@test "secret-read-guard: a 128 KiB bash -x chain allows in linear time" {
  local chain calib i
  chain='' calib=''
  for ((i = 0; i < 16384; i++)); do chain+='bash -x ' calib+='bask -x '; done
  assert_allow_relative "$(big_bash "$calib")" "$(big_bash "$chain")"
}

# bats test_tags=timing
@test "secret-read-guard: a 128 KiB run of options after bash -c denies in linear time" {
  local chain calib i
  chain='' calib=''
  for ((i = 0; i < 32768; i++)); do chain+='-x ' calib+='-x '; done
  assert_deny_relative "$(big_bash "bask -c $calib")" "$(big_bash "bash -c $chain")"
}

# --- backtick command substitution holding a -c body (#682) ---

@test "secret-read-guard: #682 a bash -c body inside backticks denies" {
  deny_cmd '`bash -c '\''env'\''`'
  deny_cmd 'echo `bash -c '\''env'\''`'
  deny_cmd 'x=`bash -c '\''env'\''`'
  deny_cmd '`"bash" -c '\''env'\''`'
  deny_cmd 'echo "`bash -c '\''env'\''`"'
}

@test "secret-read-guard: a -c body after a backtick substitution in single quotes still denies" {
  deny_cmd 'bash -c '\''ec'\''`date`'\''; env'\'
}

@test "secret-read-guard: a harmless backtick -c mention or body stays allowed" {
  allow_cmd 'echo '\''run `bash -c make` first'\'
  allow_cmd 'x=`bash -c '\''true'\''`'
}

@test "secret-read-guard: #683 option words past the cap that cannot hide a -c body stay allowed" {
  allow_cmd "bash -x -x -x -x -x -x -x -x -c 'make'"
  allow_cmd "bash -c -x -x -x -x -x -x -x -x 'make'"
  allow_cmd "rg 'foo|bash -x -x -x -x -x -x -x -x -x'"
  allow_cmd "bash$(printf ' --rcfile bash%.0s' $(seq 1 9)) x"
}

@test "secret-read-guard: nine option-with-argument words ahead of -c still deny" {
  deny_cmd "bash$(printf ' -o pipefail%.0s' $(seq 1 9)) -c 'env'"
  deny_cmd "bash$(printf ' -O extglob%.0s' $(seq 1 9)) -c 'env'"
}

@test "secret-read-guard: an underscore-led assignment ending in /env stays allowed" {
  allow_cmd '_X=y/env'
}
