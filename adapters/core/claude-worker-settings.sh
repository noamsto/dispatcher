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

# claude_worker_plugin_settings <settings-json> — shell-quote the worker-only
# Claude settings JSON so callers can splice it into a constructed launch
# command. Besides the disabled plugins it denies every tool of each MCP server
# in workerDeniedMcpServers; the permissions key is omitted when none is set.
claude_worker_plugin_settings() {
  local plugins
  plugins="$(printf '%s\n' "${CLAUDE_WORKER_DISABLED_PLUGINS[@]}" | jq -R . | jq -sc 'map({(.): false}) | add')"
  jq -cn --argjson plugins "$plugins" --argjson settings "$1" '
    {enabledPlugins: $plugins}
    + (($settings.workerDeniedMcpServers // []) as $deny
       | if ($deny | length) > 0 then {permissions: {deny: ($deny | map("mcp__\(.)__*"))}} else {} end)' |
    { read -r json; printf '%q' "$json"; }
}

# worker_browser_env <branch> — print the AGENT_BROWSER_SESSION assignment, with
# a trailing space, that gives each branch its own agent-browser session. Role
# panes share their lead's session because they share its worktree. Session
# names allow only [A-Za-z0-9_-] and the socket path ~/.agent-browser/<session>.sock
# caps at 103 bytes, so the branch is slugged and cut; the cksum suffix keeps
# branches that sanitize alike distinct.
worker_browser_env() {
  local slug sum
  slug="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-' | sed -e 's/^-//' -e 's/-$//' | cut -c1-40)"
  sum="$(printf '%s' "$1" | cksum | cut -d' ' -f1)"
  printf 'AGENT_BROWSER_SESSION=dispatch-%s-%08x ' "$slug" "$sum"
}
