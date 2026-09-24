setup() {
  load helpers
  LAUNCHER="$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh"
  run_launcher() { bash -euo pipefail "$LAUNCHER" "$@"; }
  setup_repo
  stub_bin tmux
  stub_bin crew
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
  unset TMUX CREW_ID
}

teardown() {
  teardown_repo
}

@test "rejects an unknown agent" {
  run run_launcher --agent bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent must be claude, codex, cursor, or pi"* ]]
}

@test "rejects an engine that is not on the roster" {
  DISPATCH_ENGINES="claude pi" run run_launcher --agent codex
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent codex is not enabled here (enabled: claude pi)"* ]]
}

@test "rejects an enabled engine whose CLI is missing" {
  rm "$STUB_DIR/codex"
  PATH="$(path_without_real codex)" DISPATCH_ENGINES="claude codex pi" run run_launcher --agent codex
  [ "$status" -eq 1 ]
  [[ "$output" == *"is enabled but not installed (no 'codex' on PATH)"* ]]
}

@test "an unset roster admits every engine" {
  CREW_ID=c1 run run_launcher --agent cursor
  [ "$status" -eq 0 ]
}

@test "rejects an unknown effort" {
  run run_launcher --effort bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"--effort must be"* ]]
}

@test "a trailing flag fails loudly, not silently" {
  # Without an explicit value guard, `shift 2` fails and `set -e` kills the
  # script with no message — a regression against the fish original, which
  # fell through to its validation error. Each flag must say what it needs.
  for flag in --agent --model --effort; do
    run run_launcher "$flag"
    [ "$status" -eq 1 ]
    [[ "$output" == *"$flag needs a value"* ]]
  done
}

@test "prints the crew id it minted" {
  CREW_ID=1720800000-99 run run_launcher
  [ "$status" -eq 0 ]
  [[ "$output" == *"crew id: 1720800000-99"* ]]
}

@test "passes the protocol to claude as an appended system prompt" {
  CREW_ID=c1 run_launcher
  run grep -F -- '--append-system-prompt-file /opt/protocols/DISPATCHER_PROTOCOL.md' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "pins the claude orchestrator model and effort" {
  # Unpinned, the launcher inherited the persisted /model and /effort toggles,
  # so a dispatcher could silently judge a whole fan-out on a cheap session's
  # leftovers. codex and cursor were already pinned; claude was the gap.
  CREW_ID=c1 run_launcher
  run grep -F -- '--model opus --effort high' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "an explicit model and effort still override the claude pins" {
  CREW_ID=c1 run_launcher --model sonnet --effort max
  run grep -F -- '--model sonnet --effort max' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "the bare form treats all non-flag args as one task" {
  CREW_ID=c1 run_launcher fix the flaky test
  run grep -F -- '--name dispatcher: fix the flaky test' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "a bare launch names the session by repo and launch minute" {
  CREW_ID=c1 run_launcher
  run grep -E -- "--name dispatcher · $(basename "$TEST_REPO") · [0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}( |$)" "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "the repo in the name is the main checkout, not a worktree dir" {
  git commit -q --allow-empty -m init
  git worktree add -q "$BATS_TEST_TMPDIR/wt-elsewhere"
  cd "$BATS_TEST_TMPDIR/wt-elsewhere"
  CREW_ID=c1 run_launcher
  run grep -F -- "--name dispatcher · $(basename "$TEST_REPO") · " "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "the repo in the name is foo for a bare foo.git repo" {
  git init -q --bare "$BATS_TEST_TMPDIR/foo.git"
  cd "$BATS_TEST_TMPDIR/foo.git"
  CREW_ID=c1 run_launcher
  run grep -F -- "--name dispatcher · foo · " "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "the repo in the name is the submodule's own name" {
  git commit -q --allow-empty -m init
  git init -q "$BATS_TEST_TMPDIR/sub-src"
  git -C "$BATS_TEST_TMPDIR/sub-src" -c user.name=t -c user.email=t@t commit -q --allow-empty -m init
  git -c user.name=t -c user.email=t@t -c protocol.file.allow=always submodule -q add "$BATS_TEST_TMPDIR/sub-src" mysub
  cd mysub
  CREW_ID=c1 run_launcher
  run grep -F -- "--name dispatcher · mysub · " "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "a bare launch outside a git repo still names the session" {
  cd "$BATS_TEST_TMPDIR"
  CREW_ID=c1 run run_launcher
  [ "$status" -eq 0 ]
  run grep -E -- "--name dispatcher · $(basename "$BATS_TEST_TMPDIR") · [0-9]{2}-" "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "pi gets the same unique name" {
  CREW_ID=c1 run_launcher --agent pi
  run grep -E -- "--name dispatcher · $(basename "$TEST_REPO") · [0-9]{2}-" "$STUB_LOG"
  [ "$status" -eq 0 ]
  CREW_ID=c1 run_launcher --agent pi fix it
  run grep -F -- '--name dispatcher: fix it' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "injects the protocol as a first prompt for codex" {
  DISPATCH_PROFILE=work CREW_ID=c1 run_launcher --agent codex
  run grep -F -- 'Read /opt/protocols/DISPATCHER_PROTOCOL.md' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "bakes the protocol into pi via --append-system-prompt" {
  CREW_ID=c1 run_launcher --agent pi
  run grep -F -- '--append-system-prompt /opt/protocols/DISPATCHER_PROTOCOL.md' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "pi is not work-profile gated as an orchestrator" {
  CREW_ID=c1 run run_launcher --agent pi
  [ "$status" -eq 0 ]
  [[ "$output" != *"not enabled here"* ]]
}

@test "pi launcher defaults to the OpenRouter deep row on either profile" {
  # Stub `pi` echoes its argv; grep the launch command's --model id.
  for p in work personal; do
    DISPATCH_PROFILE=$p CREW_ID=c1 run run_launcher --agent pi
    [ "$status" -eq 0 ]
    run grep -F -- '--model openrouter/deepseek/deepseek-v4.1-flash --thinking high' "$STUB_LOG"
    [ "$status" -eq 0 ]
  done
}

@test "--model overrides the pi launcher default" {
  DISPATCH_PROFILE=personal CREW_ID=c1 run run_launcher --agent pi --model openrouter/deepseek/deepseek-v4-flash
  [ "$status" -eq 0 ]
  run grep -F -- '--model openrouter/deepseek/deepseek-v4-flash --thinking high' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "warns that effort is ignored for cursor" {
  DISPATCH_PROFILE=work CREW_ID=c1 run run_launcher --agent cursor --effort high
  [[ "$output" == *"--effort is ignored for cursor"* ]]
}

@test "skips tmux window stamping when not inside tmux" {
  CREW_ID=c1 run_launcher
  run grep -c 'set-window-option' "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "registers and deregisters around the agent launch" {
  CREW_ID=c1 run_launcher
  # Match each call exactly. A bare `grep -c register` also matches
  # `deregister`, so a count of 2 could mean "registered twice, never
  # deregistered" — it would pass while the bus leaked stale entries.
  run grep -cx 'register [0-9][0-9]*' "$STUB_LOG"
  [ "$output" = "1" ]
  run grep -cx 'deregister' "$STUB_LOG"
  [ "$output" = "1" ]
}

@test "registers with a live pid whose liveness tracks the session" {
  # fish used $fish_pid; the port uses $$. Both must name a process that
  # outlives the agent launch, since crew's stale-reclaim keys on it.
  CREW_ID=c1 run_launcher
  pid="$(grep -x 'register [0-9][0-9]*' "$STUB_LOG" | awk '{print $2}')"
  [ -n "$pid" ]
  [ "$pid" -gt 0 ]
}

@test "survives a tmux without lazytmux's @reflow_bin" {
  # A missing reflow must not abort the launcher under `set -e`.
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 CREW_ID=c1 run run_launcher
  [ "$status" -eq 0 ]
}

@test "reflows through the path lazytmux stamps in @reflow_bin" {
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
[ "$1" = show-option ] && echo "$STUB_DIR/reflow"
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cat >"$STUB_DIR/reflow" <<'EOF'
#!/usr/bin/env bash
printf 'reflow %s\n' "$*" >>"$STUB_LOG"
EOF
  chmod +x "$STUB_DIR/reflow"

  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 CREW_ID=c1 run_launcher
  run grep -c '^reflow ' "$STUB_LOG"
  [ "$output" = "1" ]
}

# #303: a fake Nix store under $TEST_REPO/store; the scratch copy bakes the
# "new" build's projected protocol dir the way flake.nix's replaceStrings does.
_store_launcher() {
  STORE="$TEST_REPO/store"
  BAKED_PROTOCOLS="$STORE/h-new-protocols"
  mkdir -p "$BAKED_PROTOCOLS"
  printf 'new\n' >"$BAKED_PROTOCOLS/DISPATCHER_PROTOCOL.md"
  sed "s|@protocolDir@|$BAKED_PROTOCOLS|" "$LAUNCHER" >"$BATS_TEST_TMPDIR/launcher-store.sh"
  cat >"$STUB_DIR/claude" <<'EOF'
#!/usr/bin/env bash
printf 'claude env DISPATCHER_PROTOCOL_DIR=%s\n' "${DISPATCHER_PROTOCOL_DIR-UNSET}" >>"$STUB_LOG"
printf '%s\n' "$*" >>"$STUB_LOG"
EOF
  chmod +x "$STUB_DIR/claude"
}

@test "a stale store-path DISPATCHER_PROTOCOL_DIR is ignored and the launched session sees the baked dir" {
  _store_launcher
  export DISPATCHER_PROTOCOL_DIR="$STORE/h-old-source/adapters/core/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  printf 'old\n' >"$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md"
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatcher: ignoring stale DISPATCHER_PROTOCOL_DIR"* ]]
  grep -qx "claude env DISPATCHER_PROTOCOL_DIR=$BAKED_PROTOCOLS" "$STUB_LOG"
  grep -qF -- "--append-system-prompt-file $BAKED_PROTOCOLS/DISPATCHER_PROTOCOL.md" "$STUB_LOG"
}

@test "a store-path DISPATCHER_PROTOCOL_DIR with the baked content is kept silently" {
  _store_launcher
  export DISPATCHER_PROTOCOL_DIR="$STORE/h-cur-source/adapters/core/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  printf 'new\n' >"$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md"
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" != *"ignoring stale"* ]]
  grep -qx "claude env DISPATCHER_PROTOCOL_DIR=$DISPATCHER_PROTOCOL_DIR" "$STUB_LOG"
}

@test "a checkout override outside the store wins over the baked dir" {
  _store_launcher
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/checkout/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  printf 'dev\n' >"$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md"
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" != *"ignoring stale"* ]]
  grep -qx "claude env DISPATCHER_PROTOCOL_DIR=$DISPATCHER_PROTOCOL_DIR" "$STUB_LOG"
}

# #375: the launcher resolves all four DISPATCHER_*_DIR and pins the resolved
# values into the launched session. Same fake-store trick as _store_launcher,
# but every placeholder is substituted and the stub logs all four env vars.
_log_dir_env_stub() {
  cat >"$STUB_DIR/claude" <<'EOF'
#!/usr/bin/env bash
printf 'claude env %s %s %s %s\n' \
  "${DISPATCHER_PROTOCOL_DIR-UNSET}" "${DISPATCHER_SKILLS_DIR-UNSET}" \
  "${DISPATCHER_REVIEWERS_DIR-UNSET}" "${DISPATCHER_CRITICS_DIR-UNSET}" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/claude"
}

_store_launcher_dirs() {
  STORE="$TEST_REPO/store"
  BAKED_PROTOCOLS="$STORE/h-new-protocols"
  BAKED_SKILLS="$STORE/h-new-skills"
  BAKED_REVIEWERS="$STORE/h-new-reviewers"
  BAKED_CRITICS="$STORE/h-new-critics"
  mkdir -p "$BAKED_PROTOCOLS" "$BAKED_SKILLS" "$BAKED_REVIEWERS" "$BAKED_CRITICS"
  printf 'new\n' >"$BAKED_PROTOCOLS/DISPATCHER_PROTOCOL.md"
  sed -e "s|@protocolDir@|$BAKED_PROTOCOLS|" \
    -e "s|@skillsDir@|$BAKED_SKILLS|" \
    -e "s|@reviewersDir@|$BAKED_REVIEWERS|" \
    -e "s|@criticsDir@|$BAKED_CRITICS|" \
    "$LAUNCHER" >"$BATS_TEST_TMPDIR/launcher-dirs.sh"
  _log_dir_env_stub
}

@test "a relative DISPATCHER_*_DIR override is refused before the orchestrator launches" {
  local var
  for var in DISPATCHER_SKILLS_DIR DISPATCHER_REVIEWERS_DIR DISPATCHER_CRITICS_DIR; do
    run env "$var=relative/dir" CREW_ID=c1 bash -euo pipefail "$LAUNCHER"
    [ "$status" -eq 1 ]
    [[ "$output" == *"dispatcher: $var must be an absolute path, got: relative/dir"* ]]
    ! grep -q -- '--name' "$STUB_LOG" 2>/dev/null
  done
}

@test "the launcher pins every resolved DISPATCHER_*_DIR into the launched session" {
  _store_launcher_dirs
  unset DISPATCHER_PROTOCOL_DIR DISPATCHER_SKILLS_DIR DISPATCHER_REVIEWERS_DIR DISPATCHER_CRITICS_DIR
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-dirs.sh"
  [ "$status" -eq 0 ]
  grep -qx "claude env $BAKED_PROTOCOLS $BAKED_SKILLS $BAKED_REVIEWERS $BAKED_CRITICS" "$STUB_LOG"
}

@test "a stale store-path DISPATCHER_REVIEWERS_DIR is ignored and the launched session sees the baked dir" {
  _store_launcher_dirs
  unset DISPATCHER_PROTOCOL_DIR
  export DISPATCHER_REVIEWERS_DIR="$STORE/h-old-source/reviewers"
  mkdir -p "$DISPATCHER_REVIEWERS_DIR" "$BAKED_REVIEWERS"
  printf 'old\n' >"$DISPATCHER_REVIEWERS_DIR/old.md"
  printf 'new\n' >"$BAKED_REVIEWERS/go-reviewer.md"
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-dirs.sh"
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatcher: ignoring stale DISPATCHER_REVIEWERS_DIR"* ]]
  grep -qx "claude env $BAKED_PROTOCOLS $BAKED_SKILLS $BAKED_REVIEWERS $BAKED_CRITICS" "$STUB_LOG"
}

@test "a raw launcher leaves an unsubstituted resolved dir out of the launched session" {
  # The non-Nix dev loop runs the raw script, where @skillsDir@ is not a
  # directory; the launcher must drop the placeholder rather than export a
  # relative path the launched session would resolve in its own worktree.
  _log_dir_env_stub
  unset DISPATCHER_SKILLS_DIR DISPATCHER_REVIEWERS_DIR DISPATCHER_CRITICS_DIR
  CREW_ID=c1 run bash -euo pipefail "$LAUNCHER"
  [ "$status" -eq 0 ]
  grep -qx 'claude env /opt/protocols UNSET UNSET UNSET' "$STUB_LOG"
}
