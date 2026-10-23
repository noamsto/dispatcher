setup() {
  load helpers
  LAUNCHER="$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh"
  run_launcher() { bash -euo pipefail "$LAUNCHER" "$@"; }
  setup_repo
  stub_bin tmux
  stub_bin crew
  export DISPATCHER_PROTOCOL_DIR=/opt/protocols
  # The launcher sources the worker-settings lib; copied-out launchers
  # (_store_launcher) have no sibling file and an unsubstituted placeholder.
  export CLAUDE_WORKER_SETTINGS_LIB="$BATS_TEST_DIRNAME/../adapters/core/claude-worker-settings.sh"
  unset TMUX CREW_ID DISPATCH_DISPATCHER_MCP
}

teardown() {
  [ -z "${live_pid:-}" ] || kill "$live_pid" 2>/dev/null || true
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
  for flag in --agent --model --effort --autocompact; do
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


# The launch appends to STUB_LOG. Truncate it per row so a needle from the
# previous launch cannot satisfy this one. setup()'s stubs stay; these rows
# do not call stub_bin. PATH is not modified.
launcher_log_row() { # needle
  : >"$STUB_LOG"
  CREW_ID=c1 run_launcher
  run grep -F -- "$1" "$STUB_LOG"
  [ "$status" -eq 0 ]
}

protocol_dir_kept_row() { # kind relpath content
  : >"$STUB_LOG"
  _store_launcher
  case "$1" in
    store) export DISPATCHER_PROTOCOL_DIR="$STORE/$2" ;;
    checkout) export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/$2" ;;
    *) printf 'bad kind %s\n' "$1" >&2; return 1 ;;
  esac
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  printf '%s\n' "$3" >"$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md"
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" != *"ignoring stale"* ]]
  grep -qx "claude env DISPATCHER_PROTOCOL_DIR=$DISPATCHER_PROTOCOL_DIR" "$STUB_LOG"
}

# F53: the claude launch line carries the protocol file and the model/effort
# pin. Unpinned, the launcher inherited persisted /model and /effort toggles.
@test "the claude launch line carries the protocol file and the model pin" {
  begin_rows
  local row needle
  while IFS='|' read -r row needle; do
    [ -n "$row" ] || continue
    keep_row "$row" launcher_log_row "$needle"
  done <<'ROWS'
protocol-file|--append-system-prompt-file /opt/protocols/DISPATCHER_PROTOCOL.md
model-pin|--model opus --effort high
ROWS
  finish_rows 2
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

@test "orchestrator defaults are data: a settings override changes the launch" {
  # #575: the per-engine model/effort pins live in defaults.json's
  # orchestratorDefaults, so a ladder bump edits defaults.json and not this
  # launcher. A user layer must be able to refine them.
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"orchestratorDefaults":{"claude":{"model":"sonnet","effort":"medium"}}}\n' \
    >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  CREW_ID=c1 run run_launcher
  [ "$status" -eq 0 ]
  run grep -F -- '--model sonnet --effort medium' "$STUB_LOG"
  [ "$status" -eq 0 ]
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

@test "the claude launch carries the default 300k auto-compact window" {
  CREW_ID=c1 run_launcher
  run grep -F -- '--autocompact 300000' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "--autocompact auto keeps the engine default (no flag)" {
  CREW_ID=c1 run_launcher --autocompact auto
  run grep -c -- '--autocompact' "$STUB_LOG"
  [ "$output" = "0" ]
  DISPATCH_PROFILE=work CREW_ID=c1 run_launcher --agent codex --autocompact auto
  run grep -c 'model_auto_compact_token_limit' "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "--autocompact overrides the default window" {
  CREW_ID=c1 run_launcher --autocompact 250000
  run grep -F -- '--autocompact 250000' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "the settings layer overrides the auto-compact window" {
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"orchestratorDefaults":{"claude":{"model":"opus","autoCompact":250000}}}\n' \
    >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  CREW_ID=c1 run_launcher
  run grep -F -- '--autocompact 250000' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "the claude dispatcher launch adds --chrome only when dispatcherChrome is on" {
  CREW_ID=c1 run_launcher
  run grep -c -- '--chrome' "$STUB_LOG"
  [ "$output" = "0" ]
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"dispatcherChrome":true}\n' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  CREW_ID=c1 run_launcher
  run grep -F -- '--chrome' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "a non-claude dispatcher ignores dispatcherChrome" {
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"dispatcherChrome":true}\n' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  DISPATCH_PROFILE=work CREW_ID=c1 run_launcher --agent codex
  run grep -c -- '--chrome' "$STUB_LOG"
  [ "$output" = "0" ]
}

# #1277: the claude dispatcher's launch profile. The stub logs one argv word
# per line plus the connector switch, so a row can match whole words.
_argv_claude_stub() {
  cat >"$STUB_DIR/claude" <<'EOF'
#!/usr/bin/env bash
printf 'ENABLE_CLAUDEAI_MCP_SERVERS=%s\n' "${ENABLE_CLAUDEAI_MCP_SERVERS-UNSET}" >>"$STUB_LOG"
printf 'ARG %s\n' "$@" >>"$STUB_LOG"
EOF
  chmod +x "$STUB_DIR/claude"
}

_dispatcher_settings() { # json
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '%s\n' "$1" >"$XDG_CONFIG_HOME/dispatcher/settings.json"
}

_profile_launch() { # [launcher args...]
  _argv_claude_stub
  : >"$STUB_LOG"
  CREW_ID=c1 run run_launcher "$@"
  [ "$status" -eq 0 ]
}

_lean_disallowed='ARG --disallowedTools=CronCreate,CronDelete,CronList,DesignSync,EnterWorktree,ExitWorktree,ListAgents,LSP,NotebookEdit,RemoteTrigger,ReportFindings,ScheduleWakeup,SendMessage,Workflow,ListMcpResourcesTool,ReadMcpResourceTool,ReadMcpResourceDirTool,SendFeedback,EndConversation,EnterPlanMode,ExitPlanMode'

_assert_lean_mcp() {
  grep -qx 'ENABLE_CLAUDEAI_MCP_SERVERS=false' "$STUB_LOG"
  grep -qx 'ARG --strict-mcp-config' "$STUB_LOG"
  grep -qxF 'ARG --mcp-config={"mcpServers":{}}' "$STUB_LOG"
  [ "$(grep -c '^ARG --mcp-config=' "$STUB_LOG")" = 1 ]
}

_settings_word() { sed -n '/^ARG --settings$/{n;s/^ARG //;p;}' "$STUB_LOG"; }

@test "1277: the default claude dispatcher launch applies the lean profile" {
  _profile_launch
  _assert_lean_mcp
  grep -qxF -- "$_lean_disallowed" "$STUB_LOG"
  # Tools the dispatcher uses stay available.
  local keep
  for keep in Artifact ArtifactComments ArtifactData WebSearch PushNotification AskUserQuestion Monitor Skill Task Agent; do
    run grep -E "^ARG --disallowedTools=(.*,)?$keep(,|$)" "$STUB_LOG"
    [ "$status" -eq 1 ]
  done
  # The worker plugin layer, from claude-worker-settings.sh, with its
  # claude-in-chrome and workerDeniedMcpServers denies.
  run _settings_word
  [ "$status" -eq 0 ]
  jq -e '.enabledPlugins["superpowers@superpowers-dev"] == false' <<<"$output"
  jq -e '.permissions.deny | index("mcp__claude-in-chrome__*") and index("mcp__plugin_hm_playwright__*")' <<<"$output"
  [ "$(grep -c '^ARG --chrome$' "$STUB_LOG")" = 0 ]
}

@test "1277: a task stays the last argv word, outside the variadic flags" {
  _profile_launch fix the flaky test
  [ "$(grep '^ARG ' "$STUB_LOG" | tail -n 1)" = 'ARG fix the flaky test' ]
  # Each variadic list is one --flag=value word, never a bare flag a later
  # word could join.
  [ "$(grep -cx 'ARG --disallowedTools' "$STUB_LOG")" = 0 ]
  [ "$(grep -cx 'ARG --mcp-config' "$STUB_LOG")" = 0 ]
}

@test "1277: an unmatched repo stays lean while other repos opt in" {
  git remote add origin git@github.com:noamsto/tmux-og.git
  _dispatcher_settings '{"dispatcherMcp":{"noamsto/dispatcher":["claudeai","analytics"],"factify-inc":["claudeai"]}}'
  _profile_launch
  _assert_lean_mcp
}

_profile_optin_row() { # url settings env
  git remote remove origin 2>/dev/null || true
  git remote add origin "$1"
  rm -f "$XDG_CONFIG_HOME/dispatcher/settings.json"
  [ -z "$2" ] || _dispatcher_settings "$2"
  DISPATCH_DISPATCHER_MCP="$3" _profile_launch
  grep -qx 'ENABLE_CLAUDEAI_MCP_SERVERS=UNSET' "$STUB_LOG"
  [ "$(grep -c -- '--strict-mcp-config\|--mcp-config' "$STUB_LOG")" = 0 ]
  # The rest of the profile stays.
  grep -qxF -- "$_lean_disallowed" "$STUB_LOG"
  grep -qx 'ARG --settings' "$STUB_LOG"
}

@test "1277: a repo matched by dispatcherMcp gets its connectors back" {
  begin_rows
  local row url settings env_var
  while IFS='|' read -r row url settings env_var; do
    [ -n "$row" ] || continue
    keep_row "$row" _profile_optin_row "$url" "$settings" "$env_var"
  done <<'ROWS'
repo-claudeai|git@github.com:Noamsto/Dispatcher.git|{"dispatcherMcp":{"noamsto/dispatcher":["claudeai"]}}|
org-claudeai|https://github.com/factify-inc/mono|{"dispatcherMcp":{"factify-inc":["claudeai"]}}|
env-claudeai|ssh://git@github.com/factify-inc/mono.git||factify-inc/MONO=claudeai
ROWS
  finish_rows 3
}

@test "1277: an analytics opt-in adds its generated config under strict MCP" {
  export HOME="$BATS_TEST_TMPDIR/home"
  git remote add origin git@github.com:factify-inc/mono.git
  _dispatcher_settings '{"dispatcherMcp":{"factify-inc":["claudeai"],"factify-inc/mono":["analytics"]}}'
  # Missing config: refused, naming the file.
  CREW_ID=c1 run run_launcher
  [ "$status" -eq 1 ]
  [[ "$output" == *"config is missing at $HOME/.config/claude-code/mcp-posthog.json"* ]]
  mkdir -p "$HOME/.config/claude-code"
  printf '{"mcpServers":{}}\n' >"$HOME/.config/claude-code/mcp-posthog.json"
  _profile_launch
  grep -qx 'ENABLE_CLAUDEAI_MCP_SERVERS=false' "$STUB_LOG"
  grep -qx 'ARG --strict-mcp-config' "$STUB_LOG"
  grep -qxF "ARG --mcp-config=$HOME/.config/claude-code/mcp-posthog.json" "$STUB_LOG"
  [ "$(grep -c '^ARG --mcp-config=' "$STUB_LOG")" = 1 ]
}

@test "1277: claudeai plus analytics restores both" {
  export HOME="$BATS_TEST_TMPDIR/home"
  mkdir -p "$HOME/.config/claude-code"
  printf '{"mcpServers":{}}\n' >"$HOME/.config/claude-code/mcp-posthog.json"
  git remote add origin git@github.com:factify-inc/mono.git
  _dispatcher_settings '{"dispatcherMcp":{"factify-inc/mono":["claudeai","analytics"]}}'
  _profile_launch
  grep -qx 'ENABLE_CLAUDEAI_MCP_SERVERS=UNSET' "$STUB_LOG"
  [ "$(grep -c -- '--strict-mcp-config' "$STUB_LOG")" = 0 ]
  grep -qxF "ARG --mcp-config=$HOME/.config/claude-code/mcp-posthog.json" "$STUB_LOG"
}

@test "1277: an empty repo entry keeps a repo lean under an opted-in org" {
  git remote add origin git@github.com:factify-inc/mono.git
  _dispatcher_settings '{"dispatcherMcp":{"factify-inc":["claudeai"],"factify-inc/mono":[]}}'
  _profile_launch
  _assert_lean_mcp
  _dispatcher_settings '{"dispatcherMcp":{"factify-inc":["claudeai"]}}'
  DISPATCH_DISPATCHER_MCP='factify-inc/mono=' _profile_launch
  _assert_lean_mcp
}

@test "1277: dispatcherChrome keeps strict MCP and lifts only the chrome deny" {
  _dispatcher_settings '{"dispatcherChrome":true}'
  _profile_launch
  grep -qx 'ARG --chrome' "$STUB_LOG"
  _assert_lean_mcp
  run _settings_word
  jq -e '.permissions.deny | (index("mcp__claude-in-chrome__*") | not) and index("mcp__plugin_hm_playwright__*")' <<<"$output"
}

@test "1277: codex, cursor and pi dispatcher launches carry no profile flags" {
  _dispatcher_settings '{"dispatcherMcp":{"factify-inc":["claudeai"]}}'
  git remote add origin git@github.com:factify-inc/mono.git
  local agent marker
  while read -r agent marker; do
    : >"$STUB_LOG"
    DISPATCH_PROFILE=work CREW_ID=c1 run run_launcher --agent "$agent"
    [ "$status" -eq 0 ]
    grep -qF -- "$marker" "$STUB_LOG"
    [ "$(grep -c -- '--strict-mcp-config\|--mcp-config\|--disallowedTools\|--settings\|enabledPlugins' "$STUB_LOG")" = 0 ]
  done <<'AGENTS'
codex --profile worker
cursor --disable-indexing
pi --thinking
AGENTS
}

@test "codex gets the auto-compact window through -c" {
  DISPATCH_PROFILE=work CREW_ID=c1 run_launcher --agent codex
  run grep -F -- 'model_auto_compact_token_limit=300000' "$STUB_LOG"
  [ "$status" -eq 0 ]
  DISPATCH_PROFILE=work CREW_ID=c1 run_launcher --agent codex --autocompact 250000
  run grep -F -- 'model_auto_compact_token_limit=250000' "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "warns that auto-compact is ignored for pi and cursor" {
  CREW_ID=c1 run run_launcher --agent pi --autocompact 250000
  [[ "$output" == *"--autocompact is ignored for pi"* ]]
  DISPATCH_PROFILE=work CREW_ID=c1 run run_launcher --agent cursor --autocompact 250000
  [[ "$output" == *"--autocompact is ignored for cursor"* ]]
}

@test "rejects a bad --autocompact value" {
  run run_launcher --autocompact bogus
  [ "$status" -eq 1 ]
  [[ "$output" == *"--autocompact must be"* ]]
}

@test "skips tmux window stamping when not inside tmux" {
  CREW_ID=c1 run_launcher
  run grep -c 'set-window-option' "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "stamps its own window, not the current one, via -t \$TMUX_PANE" {
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 CREW_ID=c1 run_launcher
  # All five set-window-option calls must include -t %1 as the second and third args.
  run grep -c 'set-window-option -t %1 ' "$STUB_LOG"
  [ "$output" = "5" ]
  # Exactly 5 set-window-option calls total — none untargeted.
  run grep -c 'set-window-option' "$STUB_LOG"
  [ "$output" = "5" ]
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

# F54: a store path whose content matches the bake, and a checkout path
# outside the store, are both kept. STUB_LOG is truncated per row.
@test "a matching store dir and a checkout override are both kept" {
  begin_rows
  local row kind rel content
  while IFS='|' read -r row kind rel content; do
    [ -n "$row" ] || continue
    keep_row "$row" protocol_dir_kept_row "$kind" "$rel" "$content"
  done <<'ROWS'
store-match|store|h-cur-source/adapters/core/protocols|new
checkout-override|checkout|checkout/protocols|dev
ROWS
  finish_rows 2
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
    run grep -q -- '--name' "$STUB_LOG" 2>/dev/null
    [ "$status" -ne 0 ]
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

@test "a user-layer engine roster applies when the env roster is unset" {
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"engines":["claude","pi"]}\n' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  run run_launcher --agent codex
  [ "$status" -eq 1 ]
  [[ "$output" == *"--agent codex is not enabled here (enabled: claude pi)"* ]]
}

@test "the env roster outranks the user layer" {
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"engines":["claude","pi"]}\n' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  DISPATCH_ENGINES="claude codex pi" CREW_ID=c1 run run_launcher --agent codex
  [ "$status" -eq 0 ]
}

# #936: a fake store dir stands in for the launcher's pinned runtimeInputs bin —
# the dirs flake.nix bakes into @launcherRuntimePath@ and that writeShellApplication
# prepends to PATH. $PINNED_BIN goes first on PATH, so the launcher resolves
# `crew` there; $PROFILE_BIN is the later entry the launched engine must resolve
# instead.
_pinned_launcher() {
  PINNED_BIN="$TEST_REPO/store/h-pinned/bin"
  PROFILE_BIN="$TEST_REPO/profile/bin"
  mkdir -p "$PINNED_BIN" "$PROFILE_BIN"
  cat >"$PINNED_BIN/crew" <<'EOF'
#!/usr/bin/env bash
printf 'pinned crew %s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  cat >"$PROFILE_BIN/crew" <<'EOF'
#!/usr/bin/env bash
printf 'profile crew %s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  # The engine reports the crew IT resolves.
  cat >"$STUB_DIR/claude" <<'EOF'
#!/usr/bin/env bash
printf 'engine crew=%s\n' "$(command -v crew)" >>"$STUB_LOG"
printf 'engine PATH=%s\n' "$PATH" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$PINNED_BIN/crew" "$PROFILE_BIN/crew" "$STUB_DIR/claude"
  # setup()'s crew stub sits between the two dirs; dropped so the launcher's own
  # crew calls land on the pinned one and the engine's on the profile one.
  rm -f "$STUB_DIR/crew"
  export PATH="$PINNED_BIN:$STUB_DIR:$PROFILE_BIN:$PATH"
}

@test "the launched engine resolves crew through the profile, not the launcher's pinned store dir" {
  _pinned_launcher
  sed "s|@launcherRuntimePath@|$PINNED_BIN|" "$LAUNCHER" >"$BATS_TEST_TMPDIR/launcher-pinned.sh"
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-pinned.sh"
  [ "$status" -eq 0 ]
  grep -qx "engine crew=$PROFILE_BIN/crew" "$STUB_LOG"
  # register is the launcher's own call: it must still land on the pinned crew.
  grep -q '^pinned crew register' "$STUB_LOG"
}

@test "only the launcher's own dirs drop from the engine's PATH, and the rest keep their order" {
  _pinned_launcher
  local other="$TEST_REPO/store/h-other/bin" keep="$TEST_REPO/keep-me"
  mkdir -p "$other" "$keep"
  sed "s|@launcherRuntimePath@|$PINNED_BIN:$other|" "$LAUNCHER" >"$BATS_TEST_TMPDIR/launcher-two.sh"
  PATH="$PINNED_BIN:$keep:$other:$PINNED_BIN/extra:$STUB_DIR:$PROFILE_BIN:$PATH" \
    CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-two.sh"
  [ "$status" -eq 0 ]
  # $PINNED_BIN/extra shares a prefix but is not an entry: it stays, in place.
  grep -qF "engine PATH=$keep:$PINNED_BIN/extra:$STUB_DIR:$PROFILE_BIN:" "$STUB_LOG"
}

@test "an unsubstituted placeholder leaves the engine's PATH untouched" {
  _pinned_launcher
  CREW_ID=c1 run bash -euo pipefail "$LAUNCHER"
  [ "$status" -eq 0 ]
  grep -qF "engine PATH=$PINNED_BIN:$STUB_DIR:$PROFILE_BIN:" "$STUB_LOG"
  grep -qx "engine crew=$PINNED_BIN/crew" "$STUB_LOG"
}

# engine_path is duplicated per launcher for the same reason; one test diffs the
# three so a fix in one file cannot silently miss the others.
@test "engine_path is byte-identical across the three launchers" {
  local a b f
  a="$(sed -n '/^engine_path() {/,/^}/p' "$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh")"
  [ -n "$a" ]
  for f in dispatch dispatch-resume; do
    b="$(sed -n '/^engine_path() {/,/^}/p' "$BATS_TEST_DIRNAME/../adapters/core/$f.sh")"
    [ -n "$b" ]
    [ "$a" = "$b" ]
  done
}

# ---- #929: re-attach to this repo's live dispatcher ----------------------

# stub_crew_table <tsv> — a `crew` stub richer than setup()'s, which prints
# nothing (so `crew new` would return an empty id and `crew crews` no rows):
# `new` mints the fixed id c-minted, `crews` answers <tsv>, and every call is
# logged.
stub_crew_table() {
  printf '%s\n' "$1" >"$STUB_DIR/crews.tsv"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
new) echo c-minted ;;
crews) cat "$STUB_DIR/crews.tsv" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

# crews_tsv <row>… — the header, then one tab-joined row per argument, ending in
# the newline the launcher's `while read` needs to see the last row.
crews_tsv() {
  printf 'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n'
  printf '%s\n' "$@"
}

# stub_tmux_panes <pane_id> <pane_pid> — a tmux whose `list-panes -a` answers
# exactly one pane, and that logs every call so switch-client is assertable.
stub_tmux_panes() {
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1 \$2" in
"list-panes -a") printf '%s %s\n' '$1' '$2' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

# stub_ps_parent <child> <parent> — the one ancestor edge the pane walk takes.
stub_ps_parent() {
  cat >"$STUB_DIR/ps" <<EOF
#!/usr/bin/env bash
case "\$*" in
"-o ppid= -p $1") printf '  %s\n' '$2' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/ps"
}

# stub_engine_env — an engine stub that logs the CREW_ID it was launched with.
stub_engine_env() {
  cat >"$STUB_DIR/claude" <<'EOF'
#!/usr/bin/env bash
printf 'claude env CREW_ID=%s\n' "${CREW_ID-UNSET}" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/claude"
}

@test "929: a live dispatcher in a pane gets the client and nothing is minted" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t4321\tyes')")"
  stub_tmux_panes '%7' 4321
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"c-live"* && "$out" == *"%7"* ]]
  run grep -cx 'switch-client -t %7' "$STUB_LOG"
  [ "$output" = "1" ]
  # AC1: neither half of a mint may happen on the exit-0 switch path.
  run grep -cx new "$STUB_LOG"
  [ "$output" = "0" ]
  run grep -cx 'register [0-9][0-9]*' "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "929: outside tmux a live dispatcher prints the crew and pane, starting nothing" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t4321\tyes')")"
  stub_tmux_panes '%7' 4321
  run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"c-live"* && "$out" == *"%7"* ]]
  run grep -c switch-client "$STUB_LOG"
  [ "$output" = "0" ]
  run grep -cx new "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "929: task words with a live dispatcher refuse and name its pane" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t4321\tyes')")"
  stub_tmux_panes '%7' 4321
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 run run_launcher fix the flaky test
  [ "$status" -eq 1 ]
  out="$output"
  [[ "$out" == *"c-live"* && "$out" == *"%7"* ]]
  run grep -c switch-client "$STUB_LOG"
  [ "$output" = "0" ]
  run grep -cx new "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "929: the pane is found through an ancestor pid" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t5000\tyes')")"
  stub_tmux_panes '%3' 4000
  stub_ps_parent 5000 4000
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 run run_launcher
  [ "$status" -eq 0 ]
  run grep -cx 'switch-client -t %3' "$STUB_LOG"
  [ "$output" = "1" ]
}

@test "929: dead crews are adopted by smallest last_event_s and registered" {
  stub_crew_table "$(crews_tsv "$(printf 'c-new\t5\t5\t0\t999\tno')" "$(printf 'c-old\t5000\t5000\t0\t998\tno')")"
  stub_engine_env
  run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"re-attached to crew c-new"* && "$out" == *"crew id: c-new"* ]]
  grep -qx 'claude env CREW_ID=c-new' "$STUB_LOG"
  run grep -cx new "$STUB_LOG"
  [ "$output" = "0" ]
  run grep -cx 'register [0-9][0-9]*' "$STUB_LOG"
  [ "$output" = "1" ]
}

@test "929: a live pid with no pane re-attaches, with the stderr note" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t4321\tyes')" "$(printf 'c-old\t5000\t5000\t0\t998\tno')")"
  stub_tmux_panes '%7' 111
  run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"4321 has no tmux pane"* ]]
  [[ "$out" == *"re-attached to crew c-live"* && "$out" == *"crew id: c-live"* ]]
  run grep -cx 'register [0-9][0-9]*' "$STUB_LOG"
  [ "$output" = "1" ]
  run grep -cx new "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "929: --new mints even beside a live dispatcher's pane" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t4321\tyes')")"
  stub_tmux_panes '%7' 4321
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 run run_launcher --new
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"crew id: c-minted"* && "$out" == *"minted a NEW crew"* ]]
  run grep -cx new "$STUB_LOG"
  [ "$output" = "1" ]
  run grep -c switch-client "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "929: --new with an exported CREW_ID is a usage error" {
  : >"$STUB_LOG"
  CREW_ID=c1 run run_launcher --new
  [ "$status" -eq 2 ]
  out="$output"
  [[ "$out" == *"--new cannot be combined with an exported CREW_ID"* ]]
  run grep -cx new "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "929: no crews mints as today" {
  stub_crew_table "$(crews_tsv)"
  run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"crew id: c-minted"* ]]
  [[ "$out" != *"re-attached"* && "$out" != *"minted a NEW crew"* ]]
  run grep -cx new "$STUB_LOG"
  [ "$output" = "1" ]
}

@test "929: an exported CREW_ID never reads the crew table" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t4321\tyes')")"
  CREW_ID=c-existing run run_launcher
  [ "$status" -eq 0 ]
  run grep -cx crews "$STUB_LOG"
  [ "$output" = "0" ]
}

@test "929: the newest crew is adopted when the pane-less live one is not newest" {
  stub_crew_table "$(crews_tsv "$(printf 'c-fresh\t3\t3\t0\t998\tno')" "$(printf 'c-live\t500\t500\t1\t4321\tyes')")"
  run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"4321 has no tmux pane"* ]]
  [[ "$out" == *"re-attached to crew c-fresh"* && "$out" == *"crew id: c-fresh"* ]]
}

@test "929: a failed switch-client fails the launcher instead of claiming a switch" {
  stub_crew_table "$(crews_tsv "$(printf 'c-live\t5\t5\t1\t4321\tyes')")"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1 $2" in
"list-panes -a") printf '%%7 4321\n' ;;
"switch-client -t") exit 1 ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  TMUX=/tmp/fake,1,0 TMUX_PANE=%1 run run_launcher
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not switch the client to pane %7"* ]]
}

@test "929: a failing crew crews warns instead of minting in silence" {
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
crews) exit 5 ;;
new) echo c-minted ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"'crew crews' failed"* && "$out" == *"crew id: c-minted"* ]]
}

# The AC3 case a stub cannot show: `crew register` refuses a crew whose recorded
# pid is alive, so this row drives the real crew, the binary production runs.
@test "929: a live pid with no pane still launches when crew register refuses it" {
  local crew_bin="$CREW_GO_BIN"
  sleep 30 &
  live_pid=$!
  cat >"$STUB_DIR/crew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
exec '$crew_bin' "\$@"
EOF
  chmod +x "$STUB_DIR/crew"
  CREW_ID=c-live "$crew_bin" register "$live_pid"
  run run_launcher
  [ "$status" -eq 0 ]
  out="$output"
  [[ "$out" == *"re-attached to crew c-live"* ]]
  [[ "$out" == *"crew adopt --force c-live"* ]]
  [[ "$out" == *"crew id: c-live"* ]]
}

@test "929: a register failure is not tolerated for a crew with no live pid" {
  stub_crew_table "$(crews_tsv "$(printf 'c-fresh\t3\t3\t0\t998\tno')" "$(printf 'c-live\t500\t500\t1\t4321\tyes')")"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
new) echo c-minted ;;
crews) cat "$STUB_DIR/crews.tsv" ;;
register)
  echo "crew: crew 'c-fresh' still has a live dispatcher (pid 998)" >&2
  exit 1
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  run run_launcher
  # The dead crew is adopted, so `crew register`'s refusal is a real failure and
  # the launcher dies on it rather than naming a takeover for the wrong crew.
  [ "$status" -ne 0 ]
  out="$output"
  [[ "$out" == *"re-attached to crew c-fresh"* ]]
  [[ "$out" != *"keeps its live dispatcher pid"* ]]
}

# ---- launcher crew lifecycle ----

_crew_dir() {
  echo "$(git rev-parse --path-format=absolute --git-common-dir)/crew/crews/$1"
}

_pidfile_log_path() {
  echo "$(git rev-parse --path-format=absolute --git-common-dir)/crew/pidfile.log"
}

# #929 moved the default off minting, so the notice is a --new confirmation:
# this row's one dead crew is what the launcher would have adopted instead.
@test "dispatcher: --new with other crews on disk prints the discovery notice" {
  stub_crew_table "$(crews_tsv "$(printf 'c-other\t5\t5\t1\t111\tno')")"
  run run_launcher --new
  [ "$status" -eq 0 ]
  [[ "$output" == *"minted a NEW crew"* ]]
  [[ "$output" == *"CREW_ID=<id> dispatcher"* ]]
}

# The nested launcher runs the REAL dispatcher.sh under a live "outer" dispatcher
# that owns c-live. Its `crew register $$` aborts it (errexit) before an agent
# launches and before it ever reaches its exit-time deregister, so the outer
# crew is untouched.
@test "dispatcher: a nested launcher inheriting a live CREW_ID aborts and keeps the crew" {
  printf '#!/usr/bin/env bash\nexec "$CREW" "$@"\n' >"$STUB_DIR/crew"
  chmod +x "$STUB_DIR/crew"
  export CREW LAUNCHER
  run bash -c '
    echo $$ >"$BATS_TEST_TMPDIR/outer.pid"
    export CREW_ID=c-live
    crew register $$
    bash -euo pipefail "$LAUNCHER"
    echo $? >"$BATS_TEST_TMPDIR/rc"
    true'
  [ "$(cat "$BATS_TEST_TMPDIR/rc")" -ne 0 ]
  [[ "$output" == *"still has a live dispatcher"* ]]
  if grep -q -- --append-system-prompt-file "$STUB_LOG"; then return 1; fi
  [ "$(cat "$(_crew_dir c-live)/pid")" = "$(cat "$BATS_TEST_TMPDIR/outer.pid")" ]
}

# The counterpart to the nested test: a launcher that mints its own crew must
# have its exit-time `crew deregister` (PPID == the recorded launcher $$) remove
# it. Only the launcher's `crew id:` line is trusted for the id.
@test "dispatcher: a normal launcher exit deregisters the crew it minted" {
  printf '#!/usr/bin/env bash\nexec "$CREW" "$@"\n' >"$STUB_DIR/crew"
  chmod +x "$STUB_DIR/crew"
  export CREW
  run run_launcher
  [ "$status" -eq 0 ]
  id="$(sed -n 's/^crew id: //p' <<<"$output")"
  [ -n "$id" ]
  [ ! -e "$(_crew_dir "$id")" ]
  run cat "$(_pidfile_log_path)"
  [[ "$output" == *"register ok crew=$id"* ]]
  [[ "$output" == *"deregister removed crew=$id"* ]]
}
