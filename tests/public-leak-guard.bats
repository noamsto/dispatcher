bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/public-leak-guard.sh: per-engine parsing and verdict shape,
# private-repo matching, body files, and the betterleaks scan.

setup() {
  GUARD="$BATS_TEST_DIRNAME/../adapters/core/public-leak-guard.sh"
  export XDG_CACHE_HOME="$BATS_TEST_TMPDIR/cache"
  mkdir -p "$XDG_CACHE_HOME/dispatcher"
  printf 'owner/secret\n' >"$XDG_CACHE_HOME/dispatcher/private-repos"

  # A fake betterleaks that reports one github-pat for any ghp_ token, a gh
  # that fails, and a curl that answers $CURL_CODE, so no test reaches the
  # network.
  BIN="$BATS_TEST_TMPDIR/bin"
  mkdir -p "$BIN"
  cat >"$BIN/betterleaks" <<'SH'
#!/usr/bin/env bash
if grep -q ghp_; then echo '[{"RuleID":"github-pat"}]'; else echo '[]'; fi
SH
  printf '#!/bin/sh\nexit 1\n' >"$BIN/gh"
  printf '#!/bin/sh\nprintf %%s "${CURL_CODE:-404}"\n' >"$BIN/curl"
  chmod +x "$BIN/betterleaks" "$BIN/gh" "$BIN/curl"
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

@test "public-leak-guard: denies claude a post that puts a private repo name in public" {
  run run_guard <<<"$(claude_bash "$LEAK")"
  assert_verdict deny owner/secret
}

@test "public-leak-guard: denies on codex" {
  run run_guard <<<"$(codex_bash "$LEAK")"
  assert_verdict deny owner/secret
}

@test "public-leak-guard: denies through hookyard for pi" {
  run run_guard <<<"$(pi_bash "$LEAK")"
  assert_verdict deny owner/secret
}

@test "public-leak-guard: denies in cursor's shape on both shell events" {
  for payload in "$(cursor_shell_exec "$LEAK")" "$(cursor_pre_shell "$LEAK")"; do
    run run_guard <<<"$payload"
    [ "$status" -eq 0 ]
    jq -e '.permission == "deny" and (.agent_message | contains("owner/secret"))' <<<"$output" >/dev/null
  done
}

@test "public-leak-guard: the deny reason says to rewrite and retry, or ask the user when the text must stay" {
  run run_guard <<<"$(claude_bash "$LEAK")"
  assert_verdict deny "Rewrite it for an outside reader"
  assert_verdict deny "run the command again; the guard re-checks it"
  assert_verdict deny "ask the user in chat instead of retrying"
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
  assert_verdict deny claude.ai/code/session_abc123
  assert_verdict deny "$HOME/notes"
}

@test "public-leak-guard: scans a body file's contents, not its path" {
  printf 'clean\n' >"$BATS_TEST_TMPDIR/clean.md"
  run run_guard <<<"$(claude_bash "gh pr create -R someone/public --body-file $BATS_TEST_TMPDIR/clean.md")"
  assert_silent

  printf 'see owner/secret\n' >"$BATS_TEST_TMPDIR/leak.md"
  run run_guard <<<"$(claude_bash "gh pr create -R someone/public -F $BATS_TEST_TMPDIR/leak.md")"
  assert_verdict deny owner/secret
}

@test "public-leak-guard: reads gh api fields, inline and from a file" {
  run run_guard <<<"$(claude_bash "gh api repos/someone/public/issues -F title=x -F body='see owner/secret'")"
  assert_verdict deny owner/secret

  printf 'see owner/secret\n' >"$BATS_TEST_TMPDIR/body.md"
  run run_guard <<<"$(claude_bash "gh api repos/someone/public/issues/1/comments -F body=@$BATS_TEST_TMPDIR/body.md")"
  assert_verdict deny owner/secret
}

@test "public-leak-guard: denies a secret going public" {
  run run_guard <<<"$(claude_bash "gh issue comment 1 -R someone/public --body 'token ghp_x'")"
  assert_verdict deny "secret: github-pat"
}

@test "public-leak-guard: without betterleaks, still checks names and says the scan was skipped" {
  rm "$BIN/betterleaks"
  PATH=$(path_without betterleaks)
  run --separate-stderr run_guard <<<"$(claude_bash "$LEAK")"
  assert_verdict deny owner/secret
  [[ $stderr == *"betterleaks not found"* ]]
}

@test "public-leak-guard: without a private-repo list, a public target still gets every other check" {
  rm "$XDG_CACHE_HOME/dispatcher/private-repos"
  export CURL_CODE=200
  run --separate-stderr run_guard <<<"$(claude_bash "gh issue create -R someone/public --body 'owner/secret, https://claude.ai/code/session_abc123'")"
  assert_verdict deny claude.ai/code/session_abc123
  [[ $output != *owner/secret* ]]
  [[ $stderr == *"private names not checked"* ]]
}

@test "public-leak-guard: without a private-repo list, abstains when the target isn't public" {
  rm "$XDG_CACHE_HOME/dispatcher/private-repos"
  export CURL_CODE=404
  run run_guard <<<"$(claude_bash "gh issue create -R owner/secret --body 'https://claude.ai/code/session_abc123'")"
  assert_silent
}

@test "public-leak-guard: without a private-repo list or a working lookup, abstains" {
  rm "$XDG_CACHE_HOME/dispatcher/private-repos"
  printf '#!/bin/sh\nexit 7\n' >"$BIN/curl"
  run run_guard <<<"$(claude_bash "gh issue create -R someone/public --body 'https://claude.ai/code/session_abc123'")"
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
