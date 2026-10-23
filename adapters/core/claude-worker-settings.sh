# shellcheck shell=bash
#
# Launch settings shared by dispatcher, dispatch and dispatch-resume: the Claude
# worker settings layer (disabled plugins, denied MCP servers), the
# agent-browser session prefix for every engine, and the per-engine rendering
# of the mcpServers catalog.
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
  if [[ -n ${1:-} ]]; then printf ' --chrome'; else printf ' --no-chrome'; fi
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

# The mcpServers catalog (#1304): named remote MCP servers a dispatcher launch
# (dispatcherMcp) or a worker (`dispatch --mcp`) opts into, rendered into each
# engine's own per-launch mechanism. dispatch-config has validated every entry
# (https url, no literal header values), so nothing rendered here carries a
# secret. Generated files are content-addressed under the user cache dir, never
# a repo worktree: concurrent launches of the same set write the same bytes,
# and the file outlives the launcher for a worker pane and its resume. Every
# function takes the caller's message label first, prints an error on stderr
# and returns 1 on refusal.

# mcp_catalog_pick <label> <settings-json> <name>... — print the named catalog
# entries as one compact JSON object, deduplicated in first-seen order.
mcp_catalog_pick() {
  local label="$1" settings="$2"
  shift 2
  jq -cn --arg l "$label" --argjson s "$settings" '
    ($s.mcpServers // {}) as $cat
    | ([$ARGS.positional[] | . as $n | select($cat | has($n) | not)] | first) as $bad
    | if $bad != null then
        "\($l): unknown MCP server \"\($bad)\" (valid: \($cat | keys | join(", ")))\n" | halt_error(1)
      else reduce $ARGS.positional[] as $n ({}; . + {($n): $cat[$n]}) end' --args "$@"
}

# mcp_catalog_dir <label> — print the private (0700) dir the generated files
# live in.
mcp_catalog_dir() {
  local dir="${XDG_CACHE_HOME:-${HOME:?}/.cache}/dispatcher/mcp"
  # mkdir -p succeeds on a symlink to a dir, and every write would land in its target.
  if [ -L "$dir" ] || { [ -e "$dir" ] && [ ! -d "$dir" ]; }; then
    echo "$1: $dir is a symlink or not a directory — refusing to write an MCP config" >&2
    return 1
  fi
  (umask 077 && mkdir -p "$dir") || return 1
  chmod 700 "$dir" || return 1
  printf '%s' "$dir"
}

# _mcp_write <label> <prefix> <ext> <content> — write <content> to
# <dir>/<prefix>-<hash>.<ext> with mode 0600 (mktemp's), atomically, and print
# the path.
_mcp_write() {
  local dir sum tmp
  dir="$(mcp_catalog_dir "$1")" || return 1
  sum="$(printf '%s' "$4" | sha256sum)"
  tmp="$(mktemp "$dir/.$2.XXXXXX")" || return 1
  if ! { printf '%s\n' "$4" >"$tmp" && mv -f -- "$tmp" "$dir/$2-${sum:0:32}.$3"; }; then
    rm -f -- "$tmp"
    return 1
  fi
  printf '%s' "$dir/$2-${sum:0:32}.$3"
}

# mcp_render_claude <label> <servers-json> — write a --mcp-config file holding
# exactly these servers and print its path. claude expands ${NAME} in headers
# itself; a !command header has no claude form and is refused.
mcp_render_claude() {
  local bad
  bad="$(jq -r 'to_entries[] | .key as $n | (.value.headers // {}) | to_entries[]
    | select(.value | startswith("!")) | "mcpServers.\($n).headers.\(.key)"' <<<"$2" | head -n 1)"
  if [ -n "$bad" ]; then
    echo "$1: $bad is a !command header, which claude's --mcp-config cannot run" >&2
    return 1
  fi
  _mcp_write "$1" claude json "$(jq -c '{mcpServers: map_values({type: "http", url} + (if has("headers") then {headers} else {} end))}' <<<"$2")"
}

# mcp_render_pi <label> <servers-json> — write a pi extension (loaded with -e)
# that registers exactly these servers and print its path. pi resolves ${NAME}
# and !command header values itself. The catalog is one JSON line, which is a
# valid JavaScript expression.
mcp_render_pi() {
  local json
  json="$(jq -c 'map_values({url} + (if has("description") then {description} else {} end) + (if has("headers") then {headers} else {} end))' <<<"$2")"
  _mcp_write "$1" pi ts "// Generated by dispatcher from the mcpServers catalog: registers exactly these servers.
const servers = $json;
export default function (pi) {
  for (const [name, config] of Object.entries(servers)) pi.registerMcpServer(name, config);
}"
}

# mcp_codex_args <label> <servers-json> — print one `-c` override per line
# (TOML key=value) adding exactly these servers. A ${NAME} header becomes
# env_http_headers, an Authorization "Bearer ${NAME}" becomes
# bearer_token_env_var; any other header form has no codex equivalent and is
# refused. codex has no description field.
mcp_codex_args() {
  jq -r --arg l "$1" '
    def env_ref: "\\A\\$\\{[A-Za-z_][A-Za-z0-9_]*\\}\\z";
    to_entries[] | .key as $n | .value as $v
    | "mcp_servers.\($n).url=\($v.url | tojson)",
      (($v.headers // {}) | to_entries[] | .key as $h | .value as $hv
        | if ($h | ascii_downcase) == "authorization" and ($hv | startswith("Bearer ")) and ($hv[7:] | test(env_ref))
          then "mcp_servers.\($n).bearer_token_env_var=\($hv[9:-1] | tojson)"
          elif ($hv | test(env_ref)) then "mcp_servers.\($n).env_http_headers.\($h)=\($hv[2:-1] | tojson)"
          else "\($l): mcpServers.\($n).headers.\($h) has no codex form (codex takes only \"${NAME}\" or an Authorization \"Bearer ${NAME}\")\n" | halt_error(1)
          end)' <<<"$2"
}

# mcp_worker_optins <label> <settings-json> <agent> <list> — validate a worker's
# --mcp list (comma-separated, from `dispatch --mcp` or the task doc's mcp:
# line) and print it deduplicated, comma-joined, in first-seen order. analytics
# is the generated claude profile; every other name is a catalog entry, which
# cursor cannot load per launch. claudeai is a dispatcher-only opt-in.
mcp_worker_optins() {
  local label="$1" settings="$2" agent="$3" w valid
  local -a words=() out=()
  # One line of names and commas only: read -a stops at a newline, so anything
  # else would silently drop the names after it.
  [[ $4 =~ ^[A-Za-z0-9_,-]*$ ]] || {
    echo "$label: --mcp takes comma-separated names (letters, digits, \"_\", \"-\"), got: ${4@Q}" >&2
    return 1
  }
  IFS=, read -ra words <<<"$4"
  for w in "${words[@]}"; do
    [ -n "$w" ] || continue
    [[ " ${out[*]-} " == *" $w "* ]] || out+=("$w")
  done
  valid="$(jq -r '["analytics"] + (.mcpServers // {} | keys) | join(", ")' <<<"$settings")"
  for w in ${out[@]+"${out[@]}"}; do
    if [ "$w" = analytics ]; then
      [ "$agent" = claude ] || {
        echo "$label: --mcp analytics is claude-only; codex/cursor/pi base MCP comes from their own config" >&2
        return 1
      }
    elif ! jq -e --arg w "$w" '.mcpServers // {} | has($w)' <<<"$settings" >/dev/null; then
      echo "$label: unknown --mcp profile '$w' (valid: $valid)" >&2
      return 1
    elif [ "$agent" = cursor ]; then
      echo "$label: --mcp $w is unsupported for cursor: cursor-agent has no verified per-launch MCP config" >&2
      return 1
    fi
  done
  local IFS=,
  printf '%s' "${out[*]-}"
}

# mcp_worker_launch_args <label> <settings-json> <agent> <list> — print the
# shell-quoted launch-line fragment, with a leading space, that loads the
# list's catalog servers (analytics is the caller's): claude --mcp-config, pi
# -e, codex -c. An empty list prints nothing, so no catalog server reaches a
# worker that did not opt in.
mcp_worker_launch_args() {
  local label="$1" settings="$2" agent="$3" w catalog file lines line
  local -a words=() names=()
  IFS=, read -ra words <<<"$4"
  for w in ${words[@]+"${words[@]}"}; do
    [ -z "$w" ] || [ "$w" = analytics ] || names+=("$w")
  done
  [ "${#names[@]}" -gt 0 ] || return 0
  catalog="$(mcp_catalog_pick "$label" "$settings" "${names[@]}")" || return 1
  case "$agent" in
  claude)
    file="$(mcp_render_claude "$label" "$catalog")" || return 1
    printf ' --mcp-config %q' "$file"
    ;;
  pi)
    file="$(mcp_render_pi "$label" "$catalog")" || return 1
    printf ' -e %q' "$file"
    ;;
  codex)
    lines="$(mcp_codex_args "$label" "$catalog")" || return 1
    while IFS= read -r line; do printf ' -c %q' "$line"; done <<<"$lines"
    ;;
  *)
    echo "$label: --mcp catalog servers are unsupported for $agent" >&2
    return 1
    ;;
  esac
}
