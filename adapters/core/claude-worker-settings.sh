# shellcheck shell=bash
#
# Worker-only launch settings shared by dispatch and dispatch-resume: the Claude
# settings layer (disabled plugins, denied MCP servers) and the agent-browser
# session prefix for every engine.
#
# Plugins that are installed for interactive Claude sessions but have no role in
# dispatcher workers.  Keep this list separate from the Nix settings overlay:
# Claude keeps only the last `--settings`; the overlay survives only because the
# Nix wrapper merges caller layers into it.  A bare `claude` would replace the
# user's settings with this layer.
readonly CLAUDE_WORKER_DISABLED_PLUGINS=(
  'superpowers@superpowers-dev'
  'agent-smith@agent-smith'
  'frontend-design@claude-plugins-official'
  'refactoring-agent@xdg-claude'
  'commit-commands@claude-code-plugins'
  'resolved@resolved'
  'context-efficient-tools@xdg-claude'
)

# Claude's own "enabled by default" setting would hand a worker the owner's
# browser, so this deny is hard-coded where a workerDeniedMcpServers override
# cannot drop it.
readonly CLAUDE_CHROME_MCP_SERVER='claude-in-chrome'

# claude_worker_plugin_settings <settings-json> [chrome] — shell-quote the
# worker-only Claude settings JSON so callers can splice it into a constructed
# launch command. Besides the disabled plugins it denies every tool of each MCP
# server in workerDeniedMcpServers plus Claude in Chrome; a non-empty second
# argument is a --chrome lead, which skips only that added deny, so an owner's
# explicit listing still denies it. The permissions key is omitted when the deny
# list ends up empty.
claude_worker_plugin_settings() {
  local plugins
  plugins="$(printf '%s\n' "${CLAUDE_WORKER_DISABLED_PLUGINS[@]}" | jq -R . | jq -sc 'map({(.): false}) | add')"
  jq -cn --argjson plugins "$plugins" --argjson settings "$1" --arg chrome "${2:-}" --arg c "$CLAUDE_CHROME_MCP_SERVER" '
    {enabledPlugins: $plugins}
    + ((($settings.workerDeniedMcpServers // [])
        | if $chrome == "" and (index($c) | not) then . + [$c] else . end) as $deny
       | if ($deny | length) > 0 then {permissions: {deny: ($deny | map("mcp__\(.)__*"))}} else {} end)' |
    {
      read -r json
      printf '%q' "$json"
    }
}

# claude_chrome_flag [chrome] — print the launch flag, with a leading space, that
# turns Claude in Chrome on for a --chrome lead and off for every other worker.
claude_chrome_flag() {
  if [[ -n "${1:-}" ]]; then printf ' --chrome'; else printf ' --no-chrome'; fi
}

# worker_browser_env <branch> <worktree> <config> — print an `env` prefix, with a
# trailing space, that gives each worktree its own agent-browser session, pins the
# config file and strips the AGENT_BROWSER_* variables that can redirect a worker.
# agent-browser merges ./agent-browser.json from the repo under work over the
# owner's ~/.agent-browser/config.json, so either could point a worker at the
# owner's browser (profile, autoConnect, cdp, args); naming a config makes it the
# only one read. It also reads AGENT_BROWSER_* variables on top of any config, and
# a worker inherits the owner's shell and tmux environment, so the ones that pick
# another browser or session state are unset. AGENT_BROWSER_EXECUTABLE_PATH stays:
# the Nix wrapper defaults it. Role panes share their lead's session
# because they share its worktree. The worktree path, not the branch, is hashed so
# two repos on the same branch name never share a session. Session names allow
# only [A-Za-z0-9_-] and the socket path <socket dir>/<session>.sock caps at 103
# bytes (the socket dir is $HOME/.agent-browser on darwin, so the budget depends
# on it), hence the short slug.
readonly -a WORKER_BROWSER_STRIPPED_ENV=(
  AGENT_BROWSER_AUTO_CONNECT AGENT_BROWSER_CDP AGENT_BROWSER_PROFILE
  AGENT_BROWSER_PROVIDER AGENT_BROWSER_EXTENSIONS AGENT_BROWSER_ARGS
  AGENT_BROWSER_INIT_SCRIPTS AGENT_BROWSER_PLUGINS AGENT_BROWSER_STATE
  AGENT_BROWSER_RESTORE AGENT_BROWSER_SESSION_NAME AGENT_BROWSER_NAMESPACE
  AGENT_BROWSER_SOCKET_DIR
)

worker_browser_env() {
  local slug sum var
  slug="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-' | sed -e 's/^-//' -e 's/-$//' | cut -c1-20)"
  sum="$(printf '%s' "$2" | cksum | cut -d' ' -f1)"
  printf 'env '
  for var in "${WORKER_BROWSER_STRIPPED_ENV[@]}"; do printf -- '-u %s ' "$var"; done
  printf 'AGENT_BROWSER_SESSION=dispatch-%s-%08x AGENT_BROWSER_CONFIG=%q ' "$slug" "$sum" "$3"
}
