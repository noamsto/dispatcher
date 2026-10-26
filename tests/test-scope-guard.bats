bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/test-scope-guard.sh: a dispatched worker's full-suite bats runs
# are blocked (#841). Targeted files, a human session, and a task that stamps
# `tests: full` are not.

# shellcheck source=/dev/null
source "$BATS_TEST_DIRNAME/coverage.bash"

setup() {
  GUARD="$BATS_TEST_DIRNAME/../adapters/core/test-scope-guard.sh"
  # A checkout-shaped cwd: the guard probes `tests/` on disk and reads the
  # opt-out from this directory's git toplevel.
  REPO="$BATS_TEST_TMPDIR/repo"
  mkdir -p "$REPO/tests"
  git -C "$REPO" init -q
  export CREW_WORKER_ID='worker:feat/841-guard#s1'
}

run_guard() {
  bash -euo pipefail "$GUARD"
}

# The header block, so a `tests: full` line after the first blank line is prose.
task_with() { # <header-line>
  printf 'tier: standard\n%s\n\n## Task\nRun %s\n' "${1-tests: full}" 'tests: full in prose' >"$REPO/WORKER_TASK.md"
}

claude_bash() { # <command>
  jq -nc --arg cmd "$1" --arg d "$REPO" \
    '{hook_event_name:"PreToolUse",session_id:"s",tool_name:"Bash",tool_input:{command:$cmd},cwd:$d}'
}

pi_bash() { # <command>
  jq -nc --arg cmd "$1" --arg d "$REPO" \
    '{canonical_event:"pre_tool",hook_event_name:"tool_call",tool_name:"Bash",tool_input:{command:$cmd},cwd:$d}'
}

cursor_shell_exec() { # <command>
  jq -nc --arg cmd "$1" --arg d "$REPO" \
    '{hook_event_name:"beforeShellExecution",cursor_version:"2026.09.08",command:$cmd,cwd:$d}'
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

WHY='run targeted files (bats tests/<file>.bats --filter <pattern>); CI runs the full suite'

@test "test-scope-guard: allows a targeted file with a filter" {
  run run_guard <<<"$(pi_bash 'bats tests/crew.bats --filter reap')"
  assert_allow
}

@test "test-scope-guard: allows a handful of named files" {
  run run_guard <<<"$(pi_bash 'bats tests/a.bats tests/b.bats')"
  assert_allow
  run run_guard <<<"$(pi_bash 'bats tests/a.bats tests/b.bats tests/c.bats')"
  assert_allow
}

@test "test-scope-guard: blocks a directory" {
  for cmd in 'bats tests/' 'bats tests' 'bats ./tests' 'bats ./tests/'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

@test "test-scope-guard: a pi subagent child (CREW_SUBAGENT) is still blocked" {
  CREW_SUBAGENT=1 run run_guard <<<"$(pi_bash 'bats tests/')"
  assert_block "$WHY"
}

@test "test-scope-guard: blocks a directory whose name has a dot" {
  mkdir -p "$REPO/tests.v2"
  run run_guard <<<"$(pi_bash 'bats tests.v2')"
  assert_block "$WHY"
}

@test "test-scope-guard: blocks --recursive, bare and clustered" {
  for cmd in 'bats -r tests' 'bats --recursive tests' 'bats -rT tests/a.bats'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

@test "test-scope-guard: allows the bats-affected selector, however it is invoked" {
  for cmd in 'bats-affected' 'bats-affected --base main' 'bash scripts/bats-affected.sh --base main' \
    './scripts/bats-affected.sh' 'BATS_SHARD=1 bats-affected' \
    'go build ./... && bash scripts/bats-affected.sh --base main'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "test-scope-guard: blocks the selector sharing a line with a bats run" {
  for cmd in 'bats $(bash scripts/bats-affected.sh --base main)' 'bats `bash scripts/bats-affected.sh --base main`' \
    'bash scripts/bats-affected.sh --base main | xargs bats' \
    'bash scripts/bats-affected.sh --base main | xargs -n3 bats' \
    'bash scripts/bats-affected.sh --base main | xargs -n 3 bats'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

@test "test-scope-guard: blocks a glob over the suite" {
  for cmd in 'bats tests/*.bats' 'bats tests/bats-*.bats --filter x'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

@test "test-scope-guard: blocks a long list of files in one call" {
  run run_guard <<<"$(pi_bash 'bats tests/a.bats tests/b.bats tests/c.bats tests/d.bats')"
  assert_block '4 .bats files'
}

@test "test-scope-guard: the reason says what a targeted run looks like" {
  run run_guard <<<"$(pi_bash 'bats tests/')"
  assert_block "$WHY"
  assert_block 'stamp a '"'"'tests: full'"'"' line'
}

@test "test-scope-guard: denies in the claude and hookyard shapes" {
  run run_guard <<<"$(claude_bash 'bats tests/')"
  assert_block "$WHY"
  run run_guard <<<"$(pi_bash 'bats tests/')"
  assert_block "$WHY"
}

@test "test-scope-guard: denies in cursor's shape" {
  run run_guard <<<"$(cursor_shell_exec 'bats tests/')"
  [ "$status" -eq 0 ]
  jq -e --arg w "$WHY" '.permission == "deny" and (.agent_message | contains($w))' <<<"$output" >/dev/null
}

@test "test-scope-guard: a human session runs whatever it wants" {
  run bash -eu -c 'unset CREW_WORKER_ID; bash -euo pipefail "$0"' "$GUARD" <<<"$(pi_bash 'bats tests/')"
  assert_allow
}

@test "test-scope-guard: a task that stamps tests: full runs the suite" {
  task_with 'tests: full'
  run run_guard <<<"$(pi_bash 'bats tests/')"
  assert_allow
}

@test "test-scope-guard: tests: full below the header is prose, not an opt-out" {
  task_with 'tier: deep'
  run run_guard <<<"$(pi_bash 'bats tests/')"
  assert_block "$WHY"
}

@test "test-scope-guard: no WORKER_TASK.md at all still blocks" {
  run run_guard <<<"$(pi_bash 'bats tests/')"
  assert_block "$WHY"
}

@test "test-scope-guard: a filter value is not mistaken for a test operand" {
  for cmd in 'bats tests/a.bats --filter tests' 'bats tests/a.bats -f tests' \
    "bats tests/a.bats --filter 'reap and friends'" 'bats tests/a.bats --filter=tests' \
    'bats tests/a.bats --filter-tags !timing' 'bats --jobs 4 tests/a.bats'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "test-scope-guard: only inspects commands that actually run bats" {
  for cmd in 'echo bats tests/' 'grep -rn "bats tests/" docs' 'git log --oneline tests/' \
    'cat tests/crew.bats' 'bats tests/crew.bats | tail -3' 'bats $SUITE' \
    'bats tests/bats-affected.bats --filter x'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "test-scope-guard: checks every command in a compound line" {
  for cmd in 'cd /tmp && bats tests/' 'go build ./... && bats tests/' 'bats tests/a.bats; bats tests/'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

@test "test-scope-guard: sees bats behind a wrapper" {
  for cmd in 'timeout 300 bats tests/' 'bash -c bats tests/' 'env CREW=x bats tests/' \
    'env -i bats tests/' 'A=/x/y bats tests/' 'env -u FOO bats tests/' \
    'timeout -s KILL 300 bats tests/' 'timeout 300 -s KILL bats tests/' \
    'xargs -n 3 bats tests/' 'nice -n 5 bats tests/' 'stdbuf -o 0 bats tests/' \
    'env -C /tmp bats tests/' 'env -u FOO timeout 300 bats tests/' \
    'time -f %s bats tests/' 'timeout --signal KILL 300 bats tests/' \
    'env --unset FOO bats tests/' 'xargs --max-args 3 bats tests/' \
    'nice --adjustment 5 bats tests/' 'stdbuf --output 0 bats tests/' \
    'env -S VAR=1 bats tests/'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

# Fence for the other direction: skipping an option's value may only ever
# swallow a value, never the runner. An option that takes none, one that already
# carries its value (`xargs -d'\n'`, `env -uC`), a long option, and a command
# boundary all stay out of it.
@test "test-scope-guard: a wrapper option never swallows the run" {
  for cmd in 'xargs -n3 bats tests/' 'env -uFOO bats tests/' 'env -v bats tests/' \
    'xargs -o bats tests/' 'env -0 bats tests/' 'env -* bats tests/' \
    'env --null bats tests/' 'xargs --show-limits bats tests/' 'env -u ; bats tests/' \
    "xargs -d'\\n' bats tests/" 'xargs -d\n bats tests/' 'xargs -dn bats tests/' \
    'xargs -Is bats tests/' 'env -uC bats tests/' 'env -uS bats tests/' \
    'env -iuC bats tests/' 'env -u C bats tests/' 'timeout -sKILL 300 bats tests/' \
    'xargs --replace bats tests/' 'env --split-string=VAR=1 bats tests/' \
    'xargs --max-args=3 bats tests/' 'xargs --max-lines bats tests/' \
    'xargs -ln bats tests/' 'xargs -id bats tests/' 'env -S bats tests/' \
    'env --split-string bats tests/'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

@test "test-scope-guard: a targeted run behind an assignment or a wrapper value stays allowed" {
  for cmd in 'A=/x/y bats tests/test-scope-guard.bats' 'env -u FOO bats tests/a.bats --filter x' \
    'timeout -s KILL 300 bats tests/a.bats' 'xargs -n 3 bats tests/a.bats' \
    'timeout --signal KILL 300 bats tests/a.bats' 'nice -n 5 bats tests/a.bats'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
  # `./x=y` is not an assignment — no valid NAME — so it is the program being
  # run, and it runs no bats: the guard classifies nothing here.
  run run_guard <<<"$(pi_bash './x=y bats tests/')"
  assert_allow
}

@test "test-scope-guard: an option that swallows the runner leaves no run to classify" {
  # GNU time takes `bats` as -f's format string and runs `tests/` instead, so
  # there is no bats run behind this line even though a directory follows.
  run run_guard <<<"$(pi_bash 'time -f bats tests/')"
  assert_allow
}

@test "test-scope-guard: a flag is never mistaken for a test operand" {
  for cmd in 'bats -T tests/a.bats' 'bats -o reports tests/a.bats' 'bats --count tests/a.bats' \
    'bats -x -T tests/a.bats tests/b.bats' 'bats --timing --print-output-on-failure tests/a.bats'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "test-scope-guard: a redirection or a comment is not a test operand" {
  for cmd in 'bats tests/crew.bats 2>&1' 'bats tests/crew.bats 2>/dev/null' \
    'bats tests/crew.bats > /tmp/log' 'bats tests/crew.bats </dev/null' \
    'bats tests/crew.bats # a note' 'bats tests/crew.bats 2>&1 | tail -3' \
    'bats tests/a.bats tests/b.bats 2>&1' 'bats tests/a.bats --filter reap >>out.log'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
  for cmd in 'bats tests/ 2>&1' 'bats tests/*.bats > /tmp/log' 'bats -r tests 2>&1'; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
}

@test "test-scope-guard: a newline ends one command and starts the next" {
  for cmd in "$(printf 'git status\nbats tests/')" "$(printf 'cd /tmp\nbats tests/')" \
    "$(printf 'go build ./...\nbats -r tests 2>&1')"; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_block "$WHY"
  done
  for cmd in "$(printf 'bats tests/a.bats\nbats tests/b.bats')" \
    "$(printf 'set -euo pipefail\nbats tests/crew.bats --filter reap')" \
    "$(printf 'bash scripts/bats-affected.sh --base main\nbats tests/a.bats tests/b.bats')"; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "test-scope-guard: a quoted or escaped redirection target is one word" {
  for cmd in 'bats tests/a.bats > "/tmp/out file"' 'bats tests/a.bats 2>"error log"' \
    'bats tests/a.bats > /tmp/out\ file' "bats tests/a.bats > '/tmp/out file' 2>&1"; do
    run run_guard <<<"$(pi_bash "$cmd")"
    assert_allow
  done
}

@test "test-scope-guard: a backslash escape keeps a path whole" {
  run run_guard <<<"$(pi_bash 'bats tests/a\ b.bats')"
  assert_allow
}

@test "test-scope-guard: an unparsable or empty payload allows" {
  run run_guard <<<'{}'
  assert_allow
  run run_guard <<<''
  assert_allow
  run run_guard <<<'"a string"'
  assert_allow
}

@test "test-scope-guard: without jq it fails open and says so" {
  PATH=$(path_without jq)
  run --separate-stderr run_guard <<<"$(pi_bash 'bats tests/')"
  [ "$status" -eq 1 ]
  [[ $stderr == *'jq not found; guard NOT enforcing'* ]]
}

# path_without <cmd> — PATH minus every directory that holds <cmd>, so a real
# install on the host can't stand in for the stub a test removed.
path_without() {
  local dir out=
  local IFS=:
  for dir in $PATH; do
    [ -x "$dir/$1" ] && continue
    out=${out:+$out:}$dir
  done
  printf '%s' "$out"
}
