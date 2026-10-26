bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# The launcher picks DISPATCHER_PROTOCOL.<engine>.md for claude and pi and the
# core file for codex and cursor, and refuses when the chosen file is missing.
_protocol_fixture() { # dir content
  mkdir -p "$1"
  printf '%s\n' "$2" | tee "$1/DISPATCHER_PROTOCOL.md" "$1/DISPATCHER_PROTOCOL.claude.md" "$1/DISPATCHER_PROTOCOL.pi.md" >/dev/null
}

setup() {
  load helpers
  LAUNCHER="$BATS_TEST_DIRNAME/../adapters/core/dispatcher.sh"
  run_launcher() { bash -euo pipefail "$LAUNCHER" "$@"; }
  setup_repo
  stub_bin tmux
  stub_bin crew
  export DISPATCHER_PROTOCOL_DIR="$BATS_TEST_TMPDIR/protocols"
  _protocol_fixture "$DISPATCHER_PROTOCOL_DIR" core
  # The launcher sources the worker-settings lib; copied-out launchers
  # (_store_launcher) have no sibling file and an unsubstituted placeholder.
  export CLAUDE_WORKER_SETTINGS_LIB="$BATS_TEST_DIRNAME/../adapters/core/claude-worker-settings.sh"
  unset TMUX CREW_ID DISPATCH_DISPATCHER_MCP ENABLE_CLAUDEAI_MCP_SERVERS
  unset DISPATCHER_LAUNCHER_PID DISPATCHER_HANDOFF_DIR DISPATCHER_HANDOFF_CREW
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
  _protocol_fixture "$DISPATCHER_PROTOCOL_DIR" "$3"
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
  done <<ROWS
protocol-file|--append-system-prompt-file $DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.claude.md
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
  run grep -F -- "Read $DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md" "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "bakes the protocol into pi via --append-system-prompt" {
  CREW_ID=c1 run_launcher --agent pi
  run grep -F -- "--append-system-prompt $DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.pi.md" "$STUB_LOG"
  [ "$status" -eq 0 ]
}

@test "claude and pi refuse, launch nothing and register nothing when their render is missing" {
  local engine render
  for engine in claude pi; do
    render="$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.$engine.md"
    : >"$STUB_LOG"
    rm "$render"
    CREW_ID=c1 run --separate-stderr run_launcher --agent "$engine"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"dispatcher: missing protocol file $render"* ]]
    [[ "$stderr" == *"gen-adapters.sh"* ]]
    [[ "$stderr" == *"DISPATCHER_PROTOCOL_DIR"* ]]
    # The engine stub logs every call it gets, as do the crew and tmux stubs.
    [ ! -s "$STUB_LOG" ]
    # The other engines' files are still there: only the missing render refuses.
    [ -f "$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md" ]
    printf 'core\n' >"$render"
  done
}

@test "codex and cursor launch from the core file without any render" {
  local engine
  rm "$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.claude.md" "$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.pi.md"
  for engine in codex cursor; do
    : >"$STUB_LOG"
    CREW_ID=c1 run run_launcher --agent "$engine"
    [ "$status" -eq 0 ]
    run grep -F -- "$DISPATCHER_PROTOCOL_DIR/DISPATCHER_PROTOCOL.md" "$STUB_LOG"
    [ "$status" -eq 0 ]
  done
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
  # A claude worker's env carries the switch as false; the opt-in clears it.
  ENABLE_CLAUDEAI_MCP_SERVERS=false DISPATCH_DISPATCHER_MCP="$3" _profile_launch
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
  ENABLE_CLAUDEAI_MCP_SERVERS=false _profile_launch
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

# #1304: the mcpServers catalog. dispatcherMcp names catalog entries, and each
# engine gets exactly those servers through its own per-launch mechanism.
_argv_stub() { # engine-binary
  cat >"$STUB_DIR/$1" <<'STUB'
#!/usr/bin/env bash
printf 'ARG %s\n' "$@" >>"$STUB_LOG"
STUB
  chmod +x "$STUB_DIR/$1"
}

_catalog_launch() { # agent [settings-json]
  local bin=$1
  [ "$1" != cursor ] || bin=cursor-agent
  git remote remove origin 2>/dev/null || true
  git remote add origin git@github.com:noamsto/toddl.git
  local settings='{"dispatcherMcp":{"noamsto/toddl":["sentry","posthog"]}}'
  [ $# -lt 2 ] || settings=$2
  _dispatcher_settings "$settings"
  _argv_stub "$bin"
  : >"$STUB_LOG"
  DISPATCH_PROFILE=work CREW_ID=c1 run --separate-stderr run_launcher --agent "$1"
}

# The word after a flag word in the argv log.
_arg_after() { sed -n "/^ARG $1\$/{n;s/^ARG //;p;}" "$STUB_LOG"; }

# A generated file is private and lives under the cache dir, never the repo.
_assert_private_cache_file() {
  [[ "$1" == "$XDG_CACHE_HOME/dispatcher/mcp/"* ]]
  [ "$(stat -c %a "$1")" = 600 ]
  [ "$(stat -c %a "${1%/*}")" = 700 ]
  [ -z "$(git -C "$TEST_REPO" status --porcelain)" ]
}

_ext_servers() { sed -n 's/^const servers = \(.*\);$/\1/p' "$1"; }

@test "1304: a pi dispatcher loads exactly the opted-in catalog servers through -e" {
  _catalog_launch pi
  [ "$status" -eq 0 ]
  grep -qx 'ARG --no-approve' "$STUB_LOG"
  [ "$(grep -cx 'ARG -e' "$STUB_LOG")" = 1 ]
  ext="$(_arg_after -e)"
  _assert_private_cache_file "$ext"
  grep -qF 'pi.registerMcpServer(name, config)' "$ext"
  servers="$(_ext_servers "$ext")"
  jq -e 'keys_unsorted == ["sentry", "posthog"]' <<<"$servers"
  jq -e '.sentry == {"url": "https://mcp.sentry.dev/mcp", "description": "Sentry issues, events and releases"}' <<<"$servers"
  jq -e '.posthog.url == "https://mcp.posthog.com/mcp"' <<<"$servers"
  # No opt-in: no extension.
  _catalog_launch pi '{}'
  [ "$status" -eq 0 ]
  [ "$(grep -cx 'ARG -e' "$STUB_LOG")" = 0 ]
}

@test "1304: a claude dispatcher gets one --mcp-config holding exactly the catalog servers" {
  _catalog_launch claude
  [ "$status" -eq 0 ]
  grep -qx 'ARG --strict-mcp-config' "$STUB_LOG"
  [ "$(grep -c '^ARG --mcp-config=' "$STUB_LOG")" = 1 ]
  cfg="$(sed -n 's/^ARG --mcp-config=//p' "$STUB_LOG")"
  _assert_private_cache_file "$cfg"
  jq -e '.mcpServers | keys_unsorted == ["sentry", "posthog"]' "$cfg"
  jq -e '.mcpServers.sentry == {"type": "http", "url": "https://mcp.sentry.dev/mcp"}' "$cfg"
  grep -qxF -- "$_lean_disallowed" "$STUB_LOG"
}

@test "1304: claudeai and analytics compose with catalog servers on claude" {
  export HOME="$BATS_TEST_TMPDIR/home"
  mkdir -p "$HOME/.config/claude-code"
  printf '{"mcpServers":{}}\n' >"$HOME/.config/claude-code/mcp-posthog.json"
  _catalog_launch claude '{"dispatcherMcp":{"noamsto/toddl":["claudeai","analytics","sentry"]}}'
  [ "$status" -eq 0 ]
  [ "$(grep -c -- '--strict-mcp-config' "$STUB_LOG")" = 0 ]
  grep -qxF "ARG --mcp-config=$HOME/.config/claude-code/mcp-posthog.json" "$STUB_LOG"
  [ "$(grep -c '^ARG --mcp-config=' "$STUB_LOG")" = 2 ]
  cfg="$(sed -n 's/^ARG --mcp-config=//p' "$STUB_LOG" | grep -vF mcp-posthog.json)"
  jq -e '.mcpServers | keys == ["sentry"]' "$cfg"
}

@test "1304: a codex dispatcher gets one -c override per catalog server" {
  _catalog_launch codex
  [ "$status" -eq 0 ]
  run _arg_after -c
  [[ "$output" == *'mcp_servers.sentry.url="https://mcp.sentry.dev/mcp"'* ]]
  [[ "$output" == *'mcp_servers.posthog.url="https://mcp.posthog.com/mcp"'* ]]
  [ "$(grep -c '^mcp_servers\.' <<<"$output")" = 2 ]
  # Env-reference headers map onto codex's env header keys.
  _catalog_launch codex '{"dispatcherMcp":{"noamsto/toddl":["grafana"]},"mcpServers":{"grafana":{"url":"https://g.example/mcp","headers":{"Authorization":"Bearer ${GRAFANA_TOKEN}","X-Org":"${GRAFANA_ORG}"}}}}'
  [ "$status" -eq 0 ]
  run _arg_after -c
  [[ "$output" == *'mcp_servers.grafana.bearer_token_env_var="GRAFANA_TOKEN"'* ]]
  [[ "$output" == *'mcp_servers.grafana.env_http_headers.X-Org="GRAFANA_ORG"'* ]]
  [ "$(grep -c '^mcp_servers\.' <<<"$output")" = 3 ]
}

@test "1304: a !command header renders for pi and is refused where the engine cannot run it" {
  local settings='{"dispatcherMcp":{"noamsto/toddl":["grafana"]},"mcpServers":{"grafana":{"url":"https://g.example/mcp","headers":{"X-Key":"!pass show grafana"}}}}'
  _catalog_launch pi "$settings"
  [ "$status" -eq 0 ]
  ext="$(_arg_after -e)"
  _ext_servers "$ext" | jq -e '.grafana.headers["X-Key"] == "!pass show grafana"'
  local agent
  for agent in claude codex; do
    _catalog_launch "$agent" "$settings"
    [ "$status" -eq 1 ]
    [[ "$stderr" == *"mcpServers.grafana.headers.X-Key"* ]]
    [ "$(grep -c '^ARG ' "$STUB_LOG")" = 0 ]
  done
}

@test "1304: an unknown dispatcherMcp name fails the launch, naming the key and the valid names" {
  local agent
  for agent in claude pi; do
    _catalog_launch "$agent" '{"dispatcherMcp":{"noamsto/toddl":["sentry","nosuch"]}}'
    [ "$status" -eq 1 ]
    [[ "$stderr" == *'"noamsto/toddl" names unknown "nosuch" (valid: claudeai, analytics, elevenlabs, posthog, sentry)'* ]]
    [ "$(grep -c '^ARG ' "$STUB_LOG")" = 0 ]
  done
}

@test "1304: a cursor dispatcher warns that catalog servers are unsupported and launches" {
  _catalog_launch cursor
  [ "$status" -eq 0 ]
  [[ "$stderr" == *"cursor has no verified per-launch MCP config; dispatcherMcp servers for noamsto/toddl (sentry posthog) are not loaded"* ]]
  grep -qx 'ARG --approve-mcps' "$STUB_LOG"
  [ ! -e "$XDG_CACHE_HOME/dispatcher/mcp" ]
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
  _protocol_fixture "$BAKED_PROTOCOLS" new
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
  _protocol_fixture "$DISPATCHER_PROTOCOL_DIR" old
  CREW_ID=c1 run bash -euo pipefail "$BATS_TEST_TMPDIR/launcher-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatcher: ignoring stale DISPATCHER_PROTOCOL_DIR"* ]]
  grep -qx "claude env DISPATCHER_PROTOCOL_DIR=$BAKED_PROTOCOLS" "$STUB_LOG"
  grep -qF -- "--append-system-prompt-file $BAKED_PROTOCOLS/DISPATCHER_PROTOCOL.claude.md" "$STUB_LOG"
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
  mkdir -p "$BAKED_SKILLS" "$BAKED_REVIEWERS" "$BAKED_CRITICS"
  _protocol_fixture "$BAKED_PROTOCOLS" new
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
  grep -qx "claude env $DISPATCHER_PROTOCOL_DIR UNSET UNSET UNSET" "$STUB_LOG"
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

# ---- #701: hand a crew off to a fresh dispatcher session ----

# stub_crew_handoff — a `crew` stub for the handoff paths. Every call is logged;
# `register <pid>` records the pid, and `crews` answers crew c1 alive under that
# pid once one is recorded, dead before. `roster` answers one blocked, one
# working and one done worker, and `inbox dispatcher:*` holds one question from
# the blocked worker. HANDOFF_ROSTER=fail makes `roster` exit 3;
# HANDOFF_INBOX=torn prints the question then exits 5, =fail exits 5 bare.
stub_crew_handoff() {
  export HANDOFF_QUESTION='rebase feat/12-x onto main, or merge it?'
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
new) echo c-minted ;;
register) printf '%s\n' "$2" >"$STUB_DIR/registered.pid" ;;
crews)
  printf 'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n'
  if [ -s "$STUB_DIR/registered.pid" ]; then
    printf 'c1\t5\t5\t3\t%s\tyes\n' "$(cat "$STUB_DIR/registered.pid")"
  else
    printf 'c1\t5\t5\t3\t998\tno\n'
  fi
  ;;
roster)
  [ "${HANDOFF_ROSTER:-}" != fail ] || exit 3
  printf '%s\n' '[{"name":"fern","branch":"feat/12-x","state":"blocked"},{"name":"oak","branch":"feat/13-y","state":"working"},{"name":"ash","branch":"feat/14-z","state":"done"}]'
  ;;
inbox)
  [ "${HANDOFF_INBOX:-}" != fail ] || exit 5
  case "$2" in
  dispatcher:*) printf '{"from":"worker:feat/12-x#s1","to":"%s","body":"%s"}\n' "$2" "$HANDOFF_QUESTION" ;;
  esac
  [ "${HANDOFF_INBOX:-}" != torn ] || exit 5
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

# _engine_stub <bin> [first-launch body] — an engine stub that counts its
# launches in $STUB_DIR/launches and keeps each launch's argv (argv.N, one
# `ARG` line per word) and last word (last.N). Only the first launch runs the
# body; every later one exits 0, so a wrong relaunch shows as a count.
_engine_stub() {
  {
    cat <<'EOF'
#!/usr/bin/env bash
n=$(($(cat "$STUB_DIR/launches" 2>/dev/null || echo 0) + 1))
echo "$n" >"$STUB_DIR/launches"
printf 'ARG %s\n' "$@" >"$STUB_DIR/argv.$n"
printf '%s' "${@: -1}" >"$STUB_DIR/last.$n"
[ "$n" = 1 ] || exit 0
EOF
    printf '%s\n' "${2:-}"
  } >"$STUB_DIR/$1"
  chmod +x "$STUB_DIR/$1"
}

# The first session writes a note and hands off from inside itself, so the real
# chain is launcher → engine stub → inner launcher, walked with the real ps.
_handoff_body='printf "queue: #12 then #13\n" >"$HANDOFF_NOTE_SRC"
bash -euo pipefail "$LAUNCHER" --fresh --note "$HANDOFF_NOTE_SRC"'

_launches() { cat "$STUB_DIR/launches" 2>/dev/null || echo 0; }

_handoff_dir() {
  echo "$(git rev-parse --path-format=absolute --git-common-dir)/crew/handoff"
}

_mode() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

_no_markers() { ! compgen -G "$HANDOFF/*.restart" >/dev/null; }

_handoff_setup() {
  stub_crew_handoff
  export LAUNCHER HANDOFF_NOTE_SRC="$BATS_TEST_TMPDIR/note.md"
  HANDOFF="$(_handoff_dir)"
}

@test "701: a session that runs --fresh is relaunched with the handoff prompt" {
  _handoff_setup
  _engine_stub claude "$_handoff_body"
  CREW_ID=c1 run --separate-stderr run_launcher
  [ "$status" -eq 0 ]
  cmp "$HANDOFF_NOTE_SRC" "$HANDOFF/c1.md"
  [ "$(_mode "$HANDOFF/c1.md")" = 600 ]
  [ "$(_mode "$HANDOFF")" = 700 ]
  [ "$(_launches)" = 2 ]
  prompt=$(cat "$STUB_DIR/last.2")
  [[ "$prompt" == *'"Hand off to a fresh session"'* ]]
  [[ "$prompt" == *"re-arm your bus lane"* && "$prompt" != *"only after step 5"* ]]
  [[ "$prompt" == *"blocked at launch: fern (feat/12-x)"* ]]
  [[ "$prompt" == *"$HANDOFF/c1.md"* ]]
  [[ "$prompt" == *"1 done"* ]]
  # Worker-written inbox bodies reach the human, never the prompt.
  if grep -qF -- "$HANDOFF_QUESTION" "$STUB_DIR"/argv.*; then return 1; fi
  [[ "$stderr" == *"blocked: fern (feat/12-x) asks: $HANDOFF_QUESTION"* ]]
  run grep -cx 'register [0-9][0-9]*' "$STUB_LOG"
  [ "$output" = 1 ]
  run grep -cx deregister "$STUB_LOG"
  [ "$output" = 1 ]
  _no_markers
}

@test "701: a pi session that runs --fresh is relaunched with the prompt last" {
  _handoff_setup
  _engine_stub pi "$_handoff_body"
  CREW_ID=c1 run --separate-stderr run_launcher --agent pi
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 2 ]
  prompt=$(cat "$STUB_DIR/last.2")
  [[ "$prompt" == "Handoff: "* ]]
  [[ "$prompt" == *'"Hand off to a fresh session"'* && "$prompt" == *"$HANDOFF/c1.md"* ]]
  # pi's park blocks its turn, so it parks after the blocked workers are answered.
  [[ "$prompt" == *"re-arm your bus lane only after step 5"* ]]
  _no_markers
}

@test "701: a fresh marker for another launcher is left alone: one launch" {
  _handoff_setup
  _engine_stub claude
  mkdir -p "$HANDOFF"
  # No launcher runs as pid 1, so this is never this launcher's marker.
  date +%s >"$HANDOFF/c1.1.restart"
  CREW_ID=c1 run run_launcher
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 1 ]
  [ -e "$HANDOFF/c1.1.restart" ]
}

@test "701: an own marker 300s old is stale: one launch, marker deleted" {
  _handoff_setup
  # Written by the session itself, since only it knows the launcher's pid.
  _engine_stub claude 'mkdir -p "$DISPATCHER_HANDOFF_DIR"
echo "$(($(date +%s) - 300))" >"$DISPATCHER_HANDOFF_DIR/$CREW_ID.$DISPATCHER_LAUNCHER_PID.restart"'
  CREW_ID=c1 run run_launcher
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 1 ]
  _no_markers
}

@test "701: a manual --fresh launches once with the stored note" {
  _handoff_setup
  _engine_stub claude
  mkdir -p "$HANDOFF"
  printf 'queue: #12\n' >"$HANDOFF/c1.md"
  CREW_ID=c1 run --separate-stderr run_launcher --fresh
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 1 ]
  prompt=$(cat "$STUB_DIR/last.1")
  [[ "$prompt" == *'"Hand off to a fresh session"'* ]]
  [[ "$prompt" == *"blocked at launch: fern (feat/12-x)"* && "$prompt" == *"$HANDOFF/c1.md"* ]]
  [[ "$stderr" == *"blocked: fern (feat/12-x) asks: $HANDOFF_QUESTION"* ]]
  run grep -cx 'register [0-9][0-9]*' "$STUB_LOG"
  [ "$output" = 1 ]
}

@test "701: a manual --fresh --note stores the note before the launch" {
  _handoff_setup
  _engine_stub claude 'cp "$DISPATCHER_HANDOFF_DIR/$CREW_ID.md" "$STUB_DIR/note-at-launch"'
  printf 'queue: #13\n' >"$HANDOFF_NOTE_SRC"
  CREW_ID=c1 run run_launcher --fresh --note "$HANDOFF_NOTE_SRC"
  [ "$status" -eq 0 ]
  [ "$(_launches)" = 1 ]
  cmp "$HANDOFF_NOTE_SRC" "$HANDOFF/c1.md"
  cmp "$HANDOFF_NOTE_SRC" "$STUB_DIR/note-at-launch"
  [ "$(_mode "$HANDOFF/c1.md")" = 600 ]
  [[ "$(cat "$STUB_DIR/last.1")" == *"$HANDOFF/c1.md"* ]]
}

@test "701: a manual --fresh with no note refuses, naming the path" {
  _handoff_setup
  _engine_stub claude
  CREW_ID=c1 run run_launcher --fresh
  [ "$status" -eq 1 ]
  [[ "$output" == *"no handoff note for crew c1"* && "$output" == *"$HANDOFF/c1.md"* ]]
  [ "$(_launches)" = 0 ]
  run grep -c '^register ' "$STUB_LOG"
  [ "$output" = 0 ]
}

@test "701: a manual --fresh refuses while the crew's dispatcher is alive" {
  _handoff_setup
  _engine_stub claude
  mkdir -p "$HANDOFF"
  printf 'queue: #12\n' >"$HANDOFF/c1.md"
  echo 4321 >"$STUB_DIR/registered.pid"
  CREW_ID=c1 run run_launcher --fresh
  [ "$status" -eq 1 ]
  [[ "$output" == *"crew c1's dispatcher (pid 4321) is still live"* ]]
  [ "$(_launches)" = 0 ]
  run grep -c '^register ' "$STUB_LOG"
  [ "$output" = 0 ]
}

@test "701: a manual --fresh refuses a crew id that 'crew crews' does not list" {
  _handoff_setup
  _engine_stub claude
  mkdir -p "$HANDOFF"
  printf 'queue: #12\n' >"$HANDOFF/c9.md"
  CREW_ID=c9 run run_launcher --fresh
  [ "$status" -eq 1 ]
  [[ "$output" == *"crew c9 is not in 'crew crews'"* ]]
  [ "$(_launches)" = 0 ]
  run grep -c '^register ' "$STUB_LOG"
  [ "$output" = 0 ]
}

@test "701: a roster read that fails says so in the prompt" {
  _handoff_setup
  _engine_stub claude
  mkdir -p "$HANDOFF"
  printf 'queue\n' >"$HANDOFF/c1.md"
  HANDOFF_ROSTER=fail CREW_ID=c1 run run_launcher --fresh
  [ "$status" -eq 0 ]
  [[ "$(cat "$STUB_DIR/last.1")" == *"crew roster failed at launch"* ]]
}

@test "701: an inbox read that fails keeps its rows, and says so when it has none" {
  _handoff_setup
  _engine_stub claude
  mkdir -p "$HANDOFF"
  printf 'queue\n' >"$HANDOFF/c1.md"
  HANDOFF_INBOX=torn CREW_ID=c1 run --separate-stderr run_launcher --fresh
  [ "$status" -eq 0 ]
  [[ "$stderr" == *"blocked: fern (feat/12-x) asks: $HANDOFF_QUESTION"* ]]
  # The first launch registered c1 alive; the second needs it dead again.
  rm "$STUB_DIR/registered.pid"
  HANDOFF_INBOX=fail CREW_ID=c1 run --separate-stderr run_launcher --fresh
  [ "$status" -eq 0 ]
  [[ "$stderr" == *"blocked: fern (feat/12-x) asks: (inbox read failed — run crew inbox dispatcher:c1)"* ]]
  [[ "$stderr" != *"no question on the bus"* ]]
}

# Every refusal fires before the first stub call, so an empty log means nothing
# launched, minted or registered.
_fresh_refusal_row() { # want-status message args...
  local want=$1 msg=$2
  shift 2
  : >"$STUB_LOG"
  run run_launcher "$@"
  [ "$status" -eq "$want" ]
  [[ "$output" == *"$msg"* ]]
  [ ! -s "$STUB_LOG" ]
}

@test "701: --fresh refuses bad combinations and notes, launching nothing" {
  : >"$BATS_TEST_TMPDIR/empty.md"
  head -c 65537 /dev/zero | tr '\0' x >"$BATS_TEST_TMPDIR/big.md"
  printf 'queue\n' >"$BATS_TEST_TMPDIR/ok.md"
  begin_rows
  keep_row new _fresh_refusal_row 2 "cannot be combined with --new" --fresh --new
  keep_row task _fresh_refusal_row 2 "--fresh takes no task words" --fresh fix the test
  keep_row codex _fresh_refusal_row 2 "--fresh is claude and pi only" --fresh --agent codex
  keep_row cursor _fresh_refusal_row 2 "--fresh is claude and pi only" --fresh --agent cursor
  keep_row note-alone _fresh_refusal_row 2 "--note only goes with --fresh" --note "$BATS_TEST_TMPDIR/ok.md"
  keep_row empty-note _fresh_refusal_row 1 "is not a readable, non-empty file" --fresh --note "$BATS_TEST_TMPDIR/empty.md"
  keep_row big-note _fresh_refusal_row 1 "is over 65536 bytes" --fresh --note "$BATS_TEST_TMPDIR/big.md"
  finish_rows 7
}

@test "701: an inner --fresh whose launcher pid is no ancestor refuses and kills nothing" {
  _handoff_setup
  printf 'queue\n' >"$HANDOFF_NOTE_SRC"
  sleep 30 &
  live_pid=$!
  DISPATCHER_LAUNCHER_PID=$live_pid DISPATCHER_HANDOFF_DIR=$HANDOFF DISPATCHER_HANDOFF_CREW=c1 CREW_ID=c1 \
    run run_launcher --fresh --note "$HANDOFF_NOTE_SRC"
  [ "$status" -eq 1 ]
  [[ "$output" == *"DISPATCHER_LAUNCHER_PID=$live_pid is not an ancestor"* ]]
  kill -0 "$live_pid"
  _no_markers
  [ ! -e "$HANDOFF/c1.md" ]
  kill "$live_pid"
  wait "$live_pid" || true
  live_pid=""
}

@test "701: an engine that ignores SIGTERM is killed after ~10s and relaunched" {
  _handoff_setup
  # The subshell outlives the KILLed engine to record the inner call's exit, and
  # holds run's stdout open until it has.
  _engine_stub claude 'trap "" TERM
printf "queue\n" >"$HANDOFF_NOTE_SRC"
(
  rc=0
  bash -euo pipefail "$LAUNCHER" --fresh --note "$HANDOFF_NOTE_SRC" 2>"$STUB_DIR/inner.err" || rc=$?
  echo "$rc" >"$STUB_DIR/inner.rc"
)'
  start=$SECONDS
  CREW_ID=c1 run run_launcher
  elapsed=$((SECONDS - start))
  [ "$status" -eq 0 ]
  [ "$(cat "$STUB_DIR/inner.rc")" = 0 ]
  [[ "$(cat "$STUB_DIR/inner.err")" != *"handoff failed"* ]]
  [ "$elapsed" -ge 9 ] && [ "$elapsed" -lt 20 ]
  [ "$(_launches)" = 2 ]
  [[ "$(cat "$STUB_DIR/last.2")" == "Handoff: "* ]]
  _no_markers
}

@test "701: an inner --fresh for another crew refuses and ends nothing" {
  _handoff_setup
  _engine_stub claude 'printf "queue\n" >"$HANDOFF_NOTE_SRC"
rc=0
CREW_ID=c2 bash -euo pipefail "$LAUNCHER" --fresh --note "$HANDOFF_NOTE_SRC" 2>"$STUB_DIR/inner.err" || rc=$?
echo "$rc" >"$STUB_DIR/inner.rc"'
  CREW_ID=c1 run run_launcher
  [ "$status" -eq 0 ]
  [ "$(cat "$STUB_DIR/inner.rc")" = 1 ]
  [[ "$(cat "$STUB_DIR/inner.err")" == *"CREW_ID=c2 is not the crew this session's launcher runs"* ]]
  [ "$(_launches)" = 1 ]
  _no_markers
  [ ! -e "$HANDOFF/c2.md" ]
}

@test "701: an inner --fresh refuses launcher pid 1 before walking the chain" {
  _handoff_setup
  printf 'queue\n' >"$HANDOFF_NOTE_SRC"
  DISPATCHER_LAUNCHER_PID=1 DISPATCHER_HANDOFF_DIR=$HANDOFF DISPATCHER_HANDOFF_CREW=c1 CREW_ID=c1 \
    run run_launcher --fresh --note "$HANDOFF_NOTE_SRC"
  [ "$status" -eq 1 ]
  [[ "$output" == *"DISPATCHER_LAUNCHER_PID=1 is not a launcher pid"* ]]
  _no_markers
  [ ! -e "$HANDOFF/c1.md" ]
}

@test "701: an inner --fresh refuses an ancestor that is not a dispatcher launcher" {
  _handoff_setup
  printf 'queue\n' >"$HANDOFF_NOTE_SRC"
  export DISPATCHER_HANDOFF_DIR=$HANDOFF DISPATCHER_HANDOFF_CREW=c1 CREW_ID=c1
  # The target is this bash, whose args are its script text, with no lowercase
  # "dispatcher". Its child is the launcher itself, so a pass would TERM it (143).
  run bash -c 'DISPATCHER_LAUNCHER_PID=$$ bash -euo pipefail "$LAUNCHER" --fresh --note "$HANDOFF_NOTE_SRC"; echo "rc=$?"'
  [[ "$output" == *"is an ancestor but not a dispatcher launcher"* ]]
  [[ "$output" == *"rc=1"* ]]
  _no_markers
  [ ! -e "$HANDOFF/c1.md" ]
}

@test "701: codex and cursor launch without the handoff pair, even when inherited" {
  _handoff_setup
  local bin
  for bin in claude codex cursor-agent; do
    cat >"$STUB_DIR/$bin" <<EOF
#!/usr/bin/env bash
printf '$bin LPID=%s HDIR=%s HCREW=%s\n' "\${DISPATCHER_LAUNCHER_PID-UNSET}" "\${DISPATCHER_HANDOFF_DIR-UNSET}" "\${DISPATCHER_HANDOFF_CREW-UNSET}" >>"\$STUB_LOG"
EOF
    chmod +x "$STUB_DIR/$bin"
  done
  export DISPATCHER_LAUNCHER_PID=4242 DISPATCHER_HANDOFF_DIR=/tmp/inherited DISPATCHER_HANDOFF_CREW=c9
  CREW_ID=c1 run_launcher --agent codex
  CREW_ID=c1 run_launcher --agent cursor
  grep -qx 'codex LPID=UNSET HDIR=UNSET HCREW=UNSET' "$STUB_LOG"
  grep -qx 'cursor-agent LPID=UNSET HDIR=UNSET HCREW=UNSET' "$STUB_LOG"
  # Control: claude gets this launcher's own pair in place of the inherited one.
  : >"$STUB_LOG"
  rm -f "$STUB_DIR/registered.pid"
  CREW_ID=c1 run_launcher
  grep -qx "claude LPID=$(cat "$STUB_DIR/registered.pid") HDIR=$HANDOFF HCREW=c1" "$STUB_LOG"
}

@test "701: the section the handoff prompt cites exists in the protocol and both renders" {
  _handoff_setup
  _engine_stub claude
  mkdir -p "$HANDOFF"
  printf 'queue\n' >"$HANDOFF/c1.md"
  CREW_ID=c1 run run_launcher --fresh
  [ "$status" -eq 0 ]
  prompt=$(cat "$STUB_DIR/last.1")
  [[ "$prompt" =~ DISPATCHER_PROTOCOL\.md\ \"([^\"]+)\"\ →\ \"([^\"]+)\" ]]
  section=${BASH_REMATCH[1]}
  para=${BASH_REMATCH[2]}
  [ "$section" = "Hand off to a fresh session" ]
  local f
  for f in DISPATCHER_PROTOCOL.md DISPATCHER_PROTOCOL.claude.md DISPATCHER_PROTOCOL.pi.md; do
    awk -v h="## $section" -v p="**$para**" '
      $0 == h { s = 1; next }
      s && /^## / { s = 0 }
      s && index($0, p) == 1 { found = 1 }
      END { exit !found }' "$BATS_TEST_DIRNAME/../adapters/core/protocols/$f" || {
      echo "$f has no '$para' paragraph under '## $section'" >&2
      return 1
    }
  done
}
