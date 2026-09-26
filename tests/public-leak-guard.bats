bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/public-leak-guard.sh: per-engine parsing and verdict shape,
# private-repo matching, body files, and the betterleaks scan.

setup() {
  GUARD="$BATS_TEST_DIRNAME/../adapters/core/public-leak-guard.sh"
  export XDG_CACHE_HOME="$BATS_TEST_TMPDIR/cache"
  mkdir -p "$XDG_CACHE_HOME/dispatcher"
  printf 'owner/secret\n' >"$XDG_CACHE_HOME/dispatcher/private-repos"

  # A fake betterleaks that reports one github-pat for any ghp_ token, and a gh
  # that fails, so no test reaches the network.
  BIN="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$BIN"
  cat >"$BIN/betterleaks" <<'SH'
#!/usr/bin/env bash
if grep -q ghp_; then echo '[{"RuleID":"github-pat"}]'; else echo '[]'; fi
SH
  printf '#!/bin/sh\nexit 1\n' >"$BIN/gh"
  chmod +x "$BIN/betterleaks" "$BIN/gh"
  export PATH="$BIN:$PATH"
}

run_guard() {
  bash -euo pipefail "$GUARD"
}

claude_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"PreToolUse",session_id:"s",tool_name:"Bash",tool_input:{command:$cmd},cwd:"/tmp"}'
}

codex_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"PreToolUse",session_id:"s",tool_name:"Bash",tool_input:{command:$cmd},turn_id:"u",cwd:"/tmp"}'
}

pi_bash() { # <command>
  jq -nc --arg cmd "$1" \
    '{canonical_event:"pre_tool",hook_event_name:"tool_call",tool_name:"Bash",tool_input:{command:$cmd},cwd:"/tmp"}'
}

cursor_shell_exec() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"beforeShellExecution",cursor_version:"2026.09.08",command:$cmd,cwd:""}'
}

cursor_pre_shell() { # <command>
  jq -nc --arg cmd "$1" \
    '{hook_event_name:"preToolUse",tool_name:"Shell",tool_input:{command:$cmd,cwd:""},cwd:""}'
}

LEAK="gh issue create -R someone/public --title t --body 'Filed from owner/secret#8'"

assert_verdict() { # <decision> <substring>
  [ "$status" -eq 0 ]
  jq -e --arg d "$1" --arg w "$2" \
    '.hookSpecificOutput.permissionDecision == $d and (.hookSpecificOutput.permissionDecisionReason | contains($w))' \
    <<<"$output" >/dev/null
}

assert_silent() {
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "public-leak-guard: asks claude before a private repo name goes public" {
  run run_guard <<<"$(claude_bash "$LEAK")"
  assert_verdict ask owner/secret
}

@test "public-leak-guard: denies on codex, which has no ask channel" {
  run run_guard <<<"$(codex_bash "$LEAK")"
  assert_verdict deny owner/secret
}

@test "public-leak-guard: asks through hookyard for pi" {
  run run_guard <<<"$(pi_bash "$LEAK")"
  assert_verdict ask owner/secret
}

@test "public-leak-guard: asks in cursor's shape on both shell events" {
  for payload in "$(cursor_shell_exec "$LEAK")" "$(cursor_pre_shell "$LEAK")"; do
    run run_guard <<<"$payload"
    [ "$status" -eq 0 ]
    jq -e '.permission == "ask" and (.agent_message | contains("owner/secret"))' <<<"$output" >/dev/null
  done
}

@test "public-leak-guard: lets a post to a private repo through" {
  run run_guard <<<"$(claude_bash "gh issue create -R owner/secret --title t --body 'Filed from owner/secret#8'")"
  assert_silent
}

@test "public-leak-guard: lets a clean public post through" {
  run run_guard <<<"$(claude_bash "gh issue comment 1 -R someone/public --body 'decode is 22 tok/s'")"
  assert_silent
}

@test "public-leak-guard: ignores commands that post nothing" {
  run run_guard <<<"$(claude_bash "gh api repos/someone/public/issues")"
  assert_silent
  run run_guard <<<"$(claude_bash "echo owner/secret")"
  assert_silent
}

@test "public-leak-guard: flags session URLs and home paths" {
  run run_guard <<<"$(claude_bash "gh pr create -R someone/public --body 'https://claude.ai/code/session_abc123 and $HOME/notes'")"
  assert_verdict ask claude.ai/code/session_abc123
  assert_verdict ask "$HOME/notes"
}

@test "public-leak-guard: scans a body file's contents, not its path" {
  printf 'clean\n' >"$BATS_TEST_TMPDIR/clean.md"
  run run_guard <<<"$(claude_bash "gh pr create -R someone/public --body-file $BATS_TEST_TMPDIR/clean.md")"
  assert_silent

  printf 'see owner/secret\n' >"$BATS_TEST_TMPDIR/leak.md"
  run run_guard <<<"$(claude_bash "gh pr create -R someone/public -F $BATS_TEST_TMPDIR/leak.md")"
  assert_verdict ask owner/secret
}

@test "public-leak-guard: reads gh api fields, inline and from a file" {
  run run_guard <<<"$(claude_bash "gh api repos/someone/public/issues -F title=x -F body='see owner/secret'")"
  assert_verdict ask owner/secret

  printf 'see owner/secret\n' >"$BATS_TEST_TMPDIR/body.md"
  run run_guard <<<"$(claude_bash "gh api repos/someone/public/issues/1/comments -F body=@$BATS_TEST_TMPDIR/body.md")"
  assert_verdict ask owner/secret
}

@test "public-leak-guard: asks before a secret goes public" {
  run run_guard <<<"$(claude_bash "gh issue comment 1 -R someone/public --body 'token ghp_x'")"
  assert_verdict ask "secret: github-pat"
}

@test "public-leak-guard: without betterleaks, still checks names and says the scan was skipped" {
  rm "$BIN/betterleaks"
  PATH=$(path_without betterleaks)
  run --separate-stderr run_guard <<<"$(claude_bash "$LEAK")"
  assert_verdict ask owner/secret
  [[ $stderr == *"betterleaks not found"* ]]
}

@test "public-leak-guard: abstains with no private-repo list and no way to fetch one" {
  rm "$XDG_CACHE_HOME/dispatcher/private-repos"
  run run_guard <<<"$(claude_bash "$LEAK")"
  assert_silent
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
