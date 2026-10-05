# shellcheck shell=bash
#
# Plugins that are installed for interactive Claude sessions but have no role in
# dispatcher workers.  Keep this list separate from the Nix settings overlay:
# `--settings` layers merge, so the worker-only override leaves hooks and the
# centrally managed plugin configuration intact.
readonly CLAUDE_WORKER_DISABLED_PLUGINS=(
  'superpowers@superpowers-dev'
  'agent-smith@agent-smith'
  'frontend-design@claude-plugins-official'
  'refactoring-agent@xdg-claude'
  'commit-commands@claude-code-plugins'
  'resolved@resolved'
  'context-efficient-tools@xdg-claude'
)

# claude_worker_plugin_settings — shell-quote the worker-only Claude settings
# JSON so callers can splice it into a constructed launch command.
claude_worker_plugin_settings() {
  local plugin json='{"enabledPlugins":{' first=1
  for plugin in "${CLAUDE_WORKER_DISABLED_PLUGINS[@]}"; do
    if [ "$first" -eq 0 ]; then json+=','; fi
    printf -v json '%s"%s":false' "$json" "$plugin"
    first=0
  done
  json+='}}'
  printf '%q' "$json"
}
