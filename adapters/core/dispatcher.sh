# shellcheck shell=bash
# dispatcher — start an orchestrator session with DISPATCHER_PROTOCOL baked in.
# Ported from a fish autoload function; `dispatch` made the same move earlier.
# One behaviour change is unavoidable: a script cannot export CREW_ID back into
# the caller's interactive shell the way `set -gx` did, so the id is printed.

# Duplicated from dispatch.sh (standalone build); tests/dispatch.bats pins the
# copies together.
#
# _resolve_dir <OUT_VAR> <ENV_VAR> <baked> <label> — resolve a DISPATCHER_*_DIR
# override against the baked default (#303). A shell or tmux server that
# outlives a rebuild keeps the previous build's export, so an override under the
# baked path's store root is kept only when its content equals the baked dir's:
# the current build's export sits at a different store path than the baked
# projection but holds the same files. A checkout override always wins, as does
# any override in a raw script (baked is not absolute). diff sits in an `if`
# because its exit 1 means "differs", not failure. A relative override is
# refused: a launched session runs in a task worktree, whose files a diff
# controls, and would resolve it there.
# A stale value is ignored with a notice; the caller re-exports the resolved dir
# for the launched session, which reads DISPATCHER_PROTOCOL_DIR from markdown.
_resolve_dir() {
  local out="$1" var="$2" baked="$3" label="$4" val="${!2:-}"
  if [ -z "$val" ]; then
    printf -v "$out" '%s' "$baked"
    return 0
  fi
  if [[ $val != /* ]]; then
    echo "$label: $var must be an absolute path, got: $val" >&2
    exit 1
  fi
  # The dirs are spliced into launch scripts and their prompts, and a role
  # launched by --spawn-role inherits this env from the worker (#470).
  if [[ ! $val =~ ^/[A-Za-z0-9._/+@-]*$ ]]; then
    echo "$label: $var must not contain shell metacharacters or spaces, got: ${val@Q}" >&2
    exit 1
  fi
  if [[ $baked == /* && $val == "${baked%/*}"/* && $val != "$baked" ]] &&
    ! diff -rq -- "$val" "$baked" >/dev/null 2>&1; then
    echo "$label: ignoring stale $var from a previous build: $val; using $baked" >&2
    printf -v "$out" '%s' "$baked"
    return 0
  fi
  printf -v "$out" '%s' "$val"
}

_resolve_dir PROTOCOL_DIR DISPATCHER_PROTOCOL_DIR "@protocolDir@" dispatcher
# Harness skill, reviewer and critic dirs. #377 guarded these in dispatch.sh and
# dispatch-resume.sh; this launcher left all three inherited, so a stale or
# relative export in the tmux server reached the launched orchestrator session
# unchecked. Same _resolve_dir treatment here, then pin the resolved dirs into
# the launched session's environment: a relative value (an unsubstituted raw
# checkout) is dropped rather than exported, since the launched session runs in
# the operator's worktree and would resolve it against that cwd.
_resolve_dir SKILLS_DIR DISPATCHER_SKILLS_DIR "@skillsDir@" dispatcher
_resolve_dir REVIEWERS_DIR DISPATCHER_REVIEWERS_DIR "@reviewersDir@" dispatcher
_resolve_dir CRITICS_DIR DISPATCHER_CRITICS_DIR "@criticsDir@" dispatcher
for _n in PROTOCOL SKILLS REVIEWERS CRITICS; do
  _v="${_n}_DIR"
  _v="${!_v:-}"
  if [[ $_v == /* ]]; then
    printf -v "DISPATCHER_${_n}_DIR" '%s' "$_v"
    export "DISPATCHER_${_n}_DIR"
  else
    unset "DISPATCHER_${_n}_DIR"
  fi
done

agent=claude
model=""
effort=""
autocompact=""
task=""
# --new: mint a crew of this launcher's own even when this repo already has one.
new_crew=0

while [ $# -gt 0 ]; do
  case "$1" in
  # Each flag guards its value explicitly. Without the guard a trailing
  # `--agent` makes `shift 2` fail, and under `set -e` the script dies
  # SILENTLY — worse than the fish original, where an out-of-range index
  # yielded empty and fell through to the validation message. Same shape
  # dispatch.sh uses for --mcp/--crew-id.
  --agent)
    agent="${2:-}"
    [ -n "$agent" ] || {
      echo "dispatcher: --agent needs a value (claude, codex, cursor, or pi)" >&2
      exit 1
    }
    shift 2
    ;;
  --model)
    model="${2:-}"
    [ -n "$model" ] || {
      echo "dispatcher: --model needs a value" >&2
      exit 1
    }
    shift 2
    ;;
  --effort)
    effort="${2:-}"
    [ -n "$effort" ] || {
      echo "dispatcher: --effort needs a value (low, medium, high, xhigh, max, or ultra)" >&2
      exit 1
    }
    shift 2
    ;;
  --autocompact)
    autocompact="${2:-}"
    [ -n "$autocompact" ] || {
      echo "dispatcher: --autocompact needs a value (auto, or a token count)" >&2
      exit 1
    }
    shift 2
    ;;
  --new)
    new_crew=1
    shift
    ;;
  *)
    task="${task:+$task }$1"
    shift
    ;;
  esac
done

# Checked before the settings read: an exported CREW_ID already answers what
# --new asks for, and answering both is how a second crew appears by accident.
if [ "$new_crew" = 1 ] && [ -n "${CREW_ID:-}" ]; then
  echo "dispatcher: --new cannot be combined with an exported CREW_ID" >&2
  exit 2
fi

case "$agent" in
claude | codex | cursor | pi) ;;
*)
  echo "dispatcher: --agent must be claude, codex, cursor, or pi" >&2
  exit 1
  ;;
esac

# Checked before the crew is minted: pi's --append-system-prompt takes text or a
# file, so a missing render path would be appended as literal text.
case "$agent" in
claude | pi) protocol="$PROTOCOL_DIR/DISPATCHER_PROTOCOL.$agent.md" ;;
*) protocol="$PROTOCOL_DIR/DISPATCHER_PROTOCOL.md" ;;
esac
if [ ! -f "$protocol" ]; then
  echo "dispatcher: missing protocol file $protocol${DISPATCHER_PROTOCOL_DIR:+ (DISPATCHER_PROTOCOL_DIR=$DISPATCHER_PROTOCOL_DIR)}; regenerate with scripts/gen-adapters.sh or point DISPATCHER_PROTOCOL_DIR at a dir that has it" >&2
  exit 1
fi

# DISPATCH_ENGINES may come from the settings resolver (dispatch-config) when
# unset in the environment — see dispatch-config.sh for the layer order.
settings="$("${DISPATCH_CONFIG_BIN:-@dispatchConfig@}")"
[ -n "${DISPATCH_ENGINES:-}" ] || DISPATCH_ENGINES="$(jq -r '.engines // [] | join(" ")' <<<"$settings")"

# Worker-only Claude settings (disabled plugins, denied MCP servers), shared
# with dispatch and dispatch-resume: the claude dispatcher launch profile below
# reuses the same --settings layer. Same lookup as dispatch.sh.
claude_worker_settings_lib="${CLAUDE_WORKER_SETTINGS_LIB:-@claudeWorkerSettingsLib@}"
# replaceStrings rewrites the token on both sides of an equality check, so a
# string compare against the placeholder always matches after bake.
if [ ! -f "$claude_worker_settings_lib" ]; then
  claude_worker_settings_lib="$(dirname -- "${BASH_SOURCE[0]}")/claude-worker-settings.sh"
fi
# shellcheck source=/dev/null
. "$claude_worker_settings_lib"

# The origin's owner/repo, for the per-project dispatcherMcp lookup. Copied
# from dispatch.sh (standalone build); tests/adapters.bats pins the copies.
# ssh Host aliases are written github.com-<name>. Anything else (a lookalike
# host, an extra @, a slash) is not GitHub.
_github_ssh_host() {
  local host
  host="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  [ "$host" = github.com ] && return 0
  [[ $host =~ ^github\.com-[^@/]+$ ]]
}

# owner/repo from a GitHub path. A trailing slash may sit before or after
# `.git` (`repo/.git`, `repo.git/`, `repo/`). Empty when it is not one slug.
_tracker_slug_from_path() {
  local path="$1"
  while [[ $path == */ ]]; do
    path="${path%/}"
  done
  if [[ $path == *.git ]]; then
    path="${path%.git}"
    while [[ $path == */ ]]; do
      path="${path%/}"
    done
  fi
  if [[ $path =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]]; then
    printf '%s\n' "$path"
  fi
  return 0
}

# _origin_slug — owner/repo of the configured origin when it is a GitHub URL,
# else nothing. Configured origin, not `git remote get-url`: that applies
# insteadOf and would hide a GitHub slug behind a rewritten local path. https
# userinfo is discarded and never printed.
_origin_slug() {
  local url="" rest="" host="" path=""
  url="$(git config --get remote.origin.url 2>/dev/null || true)"
  case "$url" in
  https://*)
    rest="${url#https://}"
    rest="${rest##*@}"
    host="${rest%%/*}"
    host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
    if [ "$host" = github.com ] && [ "$rest" != "$host" ]; then
      path="${rest#*/}"
    fi
    ;;
  ssh://git@*/*)
    rest="${url#ssh://git@}"
    host="${rest%%/*}"
    if _github_ssh_host "$host"; then
      path="${rest#*/}"
    fi
    ;;
  git://*/*)
    rest="${url#git://}"
    host="${rest%%/*}"
    host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
    if [ "$host" = github.com ]; then
      path="${rest#*/}"
    fi
    ;;
  git@*:*)
    rest="${url#git@}"
    host="${rest%%:*}"
    if _github_ssh_host "$host"; then
      path="${rest#*:}"
    fi
    ;;
  *) path="" ;;
  esac
  if [ -n "$path" ]; then
    _tracker_slug_from_path "$path"
  fi
  return 0
}

# Orchestrator model/effort defaults are data (defaults.json →
# "orchestratorDefaults"), so a ladder bump edits defaults.json and not this
# launcher. --model/--effort still override per launch.
orch_default() { jq -r --arg a "$agent" --arg k "$1" '.orchestratorDefaults[$a][$k] // empty' <<<"$settings"; }

# Engine gate. Enabled (this machine's roster) and available (CLI installed).
# Unset $DISPATCH_ENGINES means every engine. Duplicated from dispatch.sh on
# purpose: adapters/core has no shared library, each script bakes standalone.
ENGINES_ALL="claude codex cursor pi"

engine_cli() {
  case "$1" in
  cursor) printf 'cursor-agent' ;;
  *) printf '%s' "$1" ;;
  esac
}

engine_enabled() {
  case " ${DISPATCH_ENGINES:-$ENGINES_ALL} " in
  *" $1 "*) return 0 ;;
  esac
  return 1
}

engine_enabled "$agent" || {
  echo "dispatcher: --agent $agent is not enabled here (enabled: ${DISPATCH_ENGINES:-$ENGINES_ALL})" >&2
  exit 1
}
cli="$(engine_cli "$agent")"
command -v "$cli" >/dev/null 2>&1 || {
  echo "dispatcher: --agent $agent is enabled but not installed (no '$cli' on PATH)" >&2
  exit 1
}

if [ -n "$effort" ]; then
  case "$effort" in
  low | medium | high | xhigh | max | ultra) ;;
  *)
    echo "dispatcher: --effort must be low, medium, high, xhigh, max, or ultra" >&2
    exit 1
    ;;
  esac
fi

if [ -n "$autocompact" ] && [ "$autocompact" != auto ]; then
  [[ $autocompact =~ ^[1-9][0-9]*$ ]] || {
    echo "dispatcher: --autocompact must be 'auto' or a positive integer" >&2
    exit 1
  }
fi

# Auto-compact window: orchestratorDefaults.<engine>.autoCompact, or the
# --autocompact flag. `auto` keeps the engine default (no knob passed).
ac="${autocompact:-$(orch_default autoCompact)}"

# ---- Re-attach before minting (#929) -------------------------------------
# Two dispatchers in one repo split attention: a worker's question reaches only
# the dispatcher that started it, and each counts only its own workers against
# the fan-out budget. So read `crew crews` first and mint only when there is
# nothing to re-attach to; `--new` is the explicit choice to mint anyway.
#
# `alive` and `last_event_s` come off the table itself — the launcher never
# re-implements the live-pid test.

# _pane_for_pid <pid> — the tmux pane whose pane_pid is <pid> or one of its
# ancestors, empty for none. Bounded: 64 hops, stops at the first `ps` that
# fails or answers non-numeric, and never climbs to init, so a recycled ppid
# chain can neither spin nor match a shared ancestor.
_pane_for_pid() {
  local pid=$1 entry pane ppid hop=0
  local -a panes=()
  mapfile -t panes < <(tmux list-panes -a -F '#{pane_id} #{pane_pid}' 2>/dev/null)
  while [ -n "$pid" ] && [ "$pid" != 0 ] && [ "$pid" != 1 ] && [ "$hop" -lt 64 ]; do
    for entry in "${panes[@]}"; do
      pane=${entry%% *}
      ppid=${entry##* }
      if [ "$ppid" = "$pid" ]; then
        printf '%s' "$pane"
        return 0
      fi
    done
    ppid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -cd '0-9') || return 0
    [ -n "$ppid" ] || return 0
    pid=$ppid
    hop=$((hop + 1))
  done
  return 0
}

# _reattach_or_switch — one read of the table, three outcomes: hand the client
# to a live dispatcher's pane and exit, adopt the most recently active crew by
# exporting CREW_ID, or fall through (no crews) so the caller mints.
_reattach_or_switch() {
  local table row_id row_last _row_first _row_workers row_pid row_alive
  local first_id='' newest='' live_id='' live_pid='' pane='' adopt
  local header=1
  # A failed read must not read as an empty table: that would mint beside crews
  # that are still there.
  if ! table=$(crew crews 2>/dev/null); then
    echo "dispatcher: 'crew crews' failed — cannot tell which crews exist, so starting a fresh one; check this repo's crew bus" >&2
    return 0
  fi
  while IFS=$'\t' read -r row_id row_last _row_first _row_workers row_pid row_alive; do
    if [ "$header" = 1 ]; then
      header=0
      continue
    fi
    [ -n "$row_id" ] || continue
    [ -n "$first_id" ] || first_id=$row_id
    # Rows come back sorted by recency, so the first numeric last_event_s is the
    # smallest. `—` means the crew has no events at all: last, never first.
    if [ -z "$newest" ] && [[ $row_last =~ ^[0-9]+$ ]]; then
      newest=$row_id
    fi
    if [ -z "$live_id" ] && [ "$row_alive" = yes ] && [[ $row_pid =~ ^[1-9][0-9]*$ ]]; then
      live_id=$row_id
      live_pid=$row_pid
    fi
  done <<<"$table"

  if [ -n "$live_id" ]; then
    pane=$(_pane_for_pid "$live_pid") || pane=''
    if [ -n "$pane" ]; then
      if [ -n "$task" ]; then
        echo "dispatcher: crew $live_id has a live dispatcher in pane $pane — send the task there, or pass --new to start a second crew" >&2
        exit 1
      fi
      if [ -n "${TMUX:-}" ]; then
        tmux switch-client -t "$pane" || {
          echo "dispatcher: could not switch the client to pane $pane" >&2
          exit 1
        }
      fi
      echo "dispatcher: crew $live_id already has a live dispatcher in pane $pane — nothing started"
      exit 0
    fi
    # Alive but no pane (detached client, killed window): not live for the
    # switch, so the crew is adopted below.
    echo "dispatcher: crew $live_id's dispatcher pid $live_pid has no tmux pane — re-attaching to the crew instead" >&2
  fi

  adopt=$newest
  [ -n "$adopt" ] || adopt=$first_id
  if [ -n "$adopt" ]; then
    # Only the crew that still records a live pid gets `crew register`'s refusal
    # tolerated below, and only that one is worth naming the takeover for.
    if [ "$adopt" = "$live_id" ]; then
      adopted_live=1
    fi
    CREW_ID=$adopt
    export CREW_ID
    echo "dispatcher: re-attached to crew $adopt" >&2
  fi
  return 0
}

# An exported CREW_ID already means "this crew", and a mint outside a git repo
# has no per-repo crew table to consult (same guard `crew register` uses).
adopted_live=0
if [ -z "${CREW_ID:-}" ] && [ "$new_crew" = 0 ] &&
  git rev-parse --git-common-dir >/dev/null 2>&1; then
  _reattach_or_switch
fi

# Fixed identity for the orchestrator window so it stands out from the
# per-branch worker windows it spawns. Guarded on $TMUX — the dispatcher can be
# launched outside a tmux pane.
if [ -n "${TMUX:-}" ]; then
  tmux set-window-option -t "$TMUX_PANE" pane-border-style "bg=#{@thm_bg},fg=#{@thm_mauve}"
  tmux set-window-option -t "$TMUX_PANE" pane-active-border-style "bg=#{@thm_bg},fg=#{@thm_mauve},bold"
  tmux set-window-option -t "$TMUX_PANE" pane-border-format " #[bold]dispatcher#[nobold] "
  # Same @crew_* options `dispatch` stamps on workers, so lazytmux renders the
  # orchestrator badge too. colour99 clears contrast on both Catppuccin themes.
  tmux set-window-option -t "$TMUX_PANE" @crew_name dispatcher
  tmux set-window-option -t "$TMUX_PANE" @crew_color colour99
  # No tmux event fires on a user-option set, and lazytmux's per-tick poll only
  # runs for the session a client is viewing — kick a reflow or the badge won't
  # render until you next switch to this window. The reflow script is never on
  # PATH — lazytmux's tmux config calls it by nix-store path — so take the path
  # from the @reflow_bin option it stamps.
  reflow_bin=$(tmux show-option -gqv @reflow_bin)
  if [ -n "$reflow_bin" ]; then
    "$reflow_bin" \
      "$(tmux display-message -p -t "$TMUX_PANE" '#{session_name}')" \
      "$(tmux display-message -p -t "$TMUX_PANE" '#{window_width}')"
  fi
fi

# Only claude and pi have a name flag; codex and cursor sessions stay unnamed.
if [ -n "$task" ]; then
  session_name="dispatcher: $task"
else
  if common_dir=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null); then
    repo=$(basename "$common_dir")
    if [ "$repo" = .git ]; then
      repo=$(basename "$(dirname "$common_dir")")
    else
      repo=${repo%.git}
    fi
  else
    repo=$(basename "$PWD")
  fi
  session_name="dispatcher · $repo · $(date '+%m-%d %H:%M')"
fi

# Mint + export the crew id once at launch (mirrors `dispatch`), so the launched
# agent and every child `dispatch` inherit the SAME crew. `crew new` needs no
# git repo. Printed because a script cannot export back to the calling shell.
# Above this, the #929 pass already adopted a crew when this repo had one, so a
# mint here means there was nothing to re-attach to — or --new asked for one.
: "${CREW_ID:=$(crew new)}"
export CREW_ID
echo "crew id: $CREW_ID"

if git rev-parse --git-common-dir >/dev/null 2>&1; then
  if [ "$adopted_live" = 1 ]; then
    crew register $$ ||
      echo "dispatcher: crew $CREW_ID keeps its live dispatcher pid — 'crew adopt --force $CREW_ID' takes it over" >&2
  else
    crew register $$
  fi

  # --new confirmation: minting next to crews the caller chose not to adopt is
  # worth one line. `crew adopt` is the wrong remedy on this entrance — CREW_ID
  # is already exported into the launched agent's env, so adopt's export line
  # would land in the wrong shell; only relaunching with it set re-attaches.
  if [ "$new_crew" = 1 ]; then
    {
      other=0
      header=1
      while IFS=$'\t' read -r row_id _; do
        if [ "$header" = 1 ]; then
          header=0
          continue
        fi
        [ -n "$row_id" ] || continue
        [ "$row_id" = "$CREW_ID" ] && continue
        other=$((other + 1))
      done < <(crew crews)
      if [ "$other" -gt 0 ]; then
        echo "dispatcher: minted a NEW crew (--new); this repo has $other other(s) — 'crew crews' lists them; to launch against one of those instead, relaunch as: CREW_ID=<id> dispatcher (drop --new)" >&2
      fi
    } || true
  fi
fi

# #936: @launcherRuntimePath@, substituted by flake.nix's launcherPath, is this
# launcher's own pinned tool PATH — the store dirs its preamble prepends.
# engine_path prints the caller's PATH with exactly those dirs dropped, every
# other entry in order; the launches below run under `env PATH=…`, so the engine
# resolves `crew` through the user profile while every launcher lookup above
# stays pinned. A raw run from a checkout leaves the placeholder literal and
# leaves PATH untouched.
_launcher_runtime_path='@launcherRuntimePath@'
engine_path() {
  local pinned="$_launcher_runtime_path" entry dir keep
  local -a parts=() pinned_parts=() kept=()
  # A substituted value is a colon-joined list of store dirs, so it starts with
  # `/`; anything else is the raw script, where there is nothing to remove. Same
  # "not absolute means unsubstituted" test _resolve_dir uses.
  if [ "${pinned:0:1}" != '/' ]; then
    printf '%s' "${PATH-}"
    return 0
  fi
  IFS=: read -ra parts <<<"${PATH-}" || true
  IFS=: read -ra pinned_parts <<<"$pinned" || true
  for entry in "${parts[@]}"; do
    keep=1
    for dir in "${pinned_parts[@]}"; do
      if [ -n "$dir" ] && [ "$entry" = "$dir" ]; then
        keep=0
        break
      fi
    done
    if [ "$keep" = 1 ]; then
      kept+=("$entry")
    fi
  done
  local IFS=:
  printf '%s' "${kept[*]}"
}

# Computed while the pinned PATH is still in effect; `env` itself and the tools
# the launcher still calls after the launch resolve as before.
engine_env_path="$(engine_path)"

# Per-project MCP (#1277, #1304): dispatcherMcp's owner/repo entry for this
# repo, else its org's, else none. claudeai and analytics are claude-only words
# the other engines ignore; every other word names an mcpServers catalog entry
# (dispatch-config refused unknown names), rendered below into the engine's own
# per-launch mechanism.
slug="$(_origin_slug)"
optins=""
if [ -n "$slug" ]; then
  optins="$(jq -r --arg r "$(printf '%s' "$slug" | tr '[:upper:]' '[:lower:]')" '
    .dispatcherMcp // {} | (.[$r] // .[$r | split("/")[0]] // []) | join(" ")' <<<"$settings")"
fi
catalog_names=()
for optin in $optins; do
  case "$optin" in
  claudeai | analytics) ;;
  *) catalog_names+=("$optin") ;;
  esac
done
catalog=""
if [ "${#catalog_names[@]}" -gt 0 ]; then
  catalog="$(mcp_catalog_pick "dispatcher: dispatcherMcp for $slug" "$settings" "${catalog_names[@]}")" || exit 1
fi

case "$agent" in
claude)
  # Pinned, not inherited: /model and /effort persist across sessions, so an
  # unpinned dispatcher judges tier+engine+model on whatever the last cheap
  # session was toggled to. high, not xhigh — same reason codex holds at high
  # below: blocked workers wait on a bounded ~2h in-band window.
  set -- --name "$session_name" --append-system-prompt-file "$protocol" \
    --model "${model:-$(orch_default model)}" --effort "${effort:-$(orch_default effort)}"
  [ -n "$ac" ] && [ "$ac" != auto ] && set -- "$@" --autocompact "$ac"
  # Claude in Chrome drives the owner's authenticated browser, so it is opt-in:
  # off passes no --chrome and the plugin layer below denies its tools even
  # where Claude's own default enables it. On, --strict-mcp-config keeps it (it
  # is not an MCP-config server) and only the plugin layer's deny is lifted.
  chrome=""
  if [ "$(jq -r '.dispatcherChrome == true' <<<"$settings")" = true ]; then
    chrome=1
    set -- "$@" --chrome
  fi

  # Launch profile (#1277, docs/dispatcher-context-floor.md "profile-rec"): the
  # tools the protocol never names are disallowed, and MCP is opt-in per
  # project through dispatcherMcp (looked up above). claudeai restores every
  # claude.ai connector: they load all or none (one env switch, and
  # --strict-mcp-config drops them all), so that opt-in skips strict MCP and the
  # switch. A generated --mcp profile, and the selected catalog servers, are
  # each one more --mcp-config. Variadic flags take one --flag=value word each,
  # so a trailing task is never read as a tool name or a config.
  # env takes -u before any NAME=value.
  launch_env=(PATH="$engine_env_path")
  mcp_configs=()
  claudeai=0
  for optin in $optins; do
    case "$optin" in
    claudeai) claudeai=1 ;;
    analytics)
      mcp_file="$HOME/.config/claude-code/mcp-posthog.json"
      [ -f "$mcp_file" ] || {
        echo "dispatcher: dispatcherMcp opts $slug into $optin, but its config is missing at $mcp_file" >&2
        exit 1
      }
      mcp_configs+=("--mcp-config=$mcp_file")
      ;;
    esac
  done
  if [ -n "$catalog" ]; then
    mcp_file="$(mcp_render_claude dispatcher "$catalog")" || exit 1
    mcp_configs+=("--mcp-config=$mcp_file")
  fi
  if [ "$claudeai" = 1 ]; then
    # An inherited switch (a claude worker exports it as false) would undo the
    # opt-in.
    launch_env=(-u ENABLE_CLAUDEAI_MCP_SERVERS "${launch_env[@]}")
  else
    launch_env+=(ENABLE_CLAUDEAI_MCP_SERVERS=false)
    [ "${#mcp_configs[@]}" -gt 0 ] || mcp_configs=('--mcp-config={"mcpServers":{}}')
    set -- "$@" --strict-mcp-config
  fi
  set -- "$@" "${mcp_configs[@]}" \
    --disallowedTools=CronCreate,CronDelete,CronList,DesignSync,EnterWorktree,ExitWorktree,ListAgents,LSP,NotebookEdit,RemoteTrigger,ReportFindings,ScheduleWakeup,SendMessage,Workflow,ListMcpResourcesTool,ReadMcpResourceTool,ReadMcpResourceDirTool,SendFeedback,EndConversation,EnterPlanMode,ExitPlanMode
  # The worker plugin layer, printed shell-quoted for dispatch's launch scripts.
  plugin_layer=""
  eval "plugin_layer=$(claude_worker_plugin_settings "$settings" "$chrome")"
  set -- "$@" --settings "$plugin_layer"
  env "${launch_env[@]}" claude "$@" ${task:+"$task"}
  ;;
codex | cursor)
  # Neither CLI has --append-system-prompt-file — inject the protocol as the
  # first prompt (the same pattern dispatch.sh uses for codex/cursor workers).
  prompt="Read $protocol and adopt the dispatcher role for the rest of this session: judge each task into tier + engine + model + effort per the rubric, scaffold one worker per task via dispatch, and run the crew-watch loop per YOUR engine's section of the protocol (you are a $agent dispatcher). CREW_ID is already exported in this environment, so dispatch and crew calls inherit it."
  [ -n "$task" ] && prompt="$prompt First task: $task"
  if [ "$agent" = codex ]; then
    # Effort high, not xhigh: blocked workers wait on a bounded ~2h in-band
    # window; xhigh turns would let blocks go stale. service_tier pinned — the
    # interactive /fast toggle persists locally and would otherwise leak into
    # the unattended dispatcher at 2.5x cost.
    ac_flag=()
    [ -n "$ac" ] && [ "$ac" != auto ] && ac_flag=(-c "model_auto_compact_token_limit=$ac")
    # Catalog servers: one -c override per TOML key.
    mcp_flags=()
    if [ -n "$catalog" ]; then
      mcp_lines="$(mcp_codex_args dispatcher "$catalog")" || exit 1
      while IFS= read -r line; do mcp_flags+=(-c "$line"); done <<<"$mcp_lines"
    fi
    env PATH="$engine_env_path" codex --profile worker \
      -m "${model:-$(orch_default model)}" \
      -c "model_reasoning_effort=\"${effort:-$(orch_default effort)}\"" \
      -c 'service_tier="default"' \
      "${ac_flag[@]}" "${mcp_flags[@]}" \
      --dangerously-bypass-approvals-and-sandbox "$prompt"
  else
    # kimi-k3-high: third-family model, strong agentic tool use. --effort is
    # accepted-and-ignored (cursor encodes effort in the model id).
    [ -n "$effort" ] && echo "dispatcher: --effort is ignored for cursor (effort lives in the model id)" >&2
    [ -n "$ac" ] && [ "$ac" != auto ] && echo "dispatcher: --autocompact is ignored for cursor (no compaction knob)" >&2
    # cursor's MCP config is ~/.cursor/mcp.json (every cursor session, workers
    # included) or the repo's .cursor/mcp.json, so neither is a per-launch
    # path. --plugin-dir may load a plugin dir's mcp.json, but that is
    # unverified, so catalog opt-ins stay unsupported here.
    [ -z "$catalog" ] || echo "dispatcher: cursor has no verified per-launch MCP config; dispatcherMcp servers for $slug (${catalog_names[*]}) are not loaded" >&2
    env PATH="$engine_env_path" cursor-agent --model "${model:-$(orch_default model)}" \
      --force --trust --approve-mcps --disable-indexing --disable-codebase-ref "$prompt"
  fi
  ;;
pi)
  # pi has a real --append-system-prompt (text or file contents), so the
  # protocol is baked as a system prompt rather than injected as a first user
  # prompt like codex/cursor — sturdier across compaction. --thinking is a real
  # effort knob. --no-approve ignores a target project's local resources; the
  # orchestrator runs in the dispatcher repo, and global ~/.pi/agent config
  # (auth, packages) still loads.
  # See dispatch-orchestration.md → "Orchestrator engines".
  [ -n "$ac" ] && [ "$ac" != auto ] && echo "dispatcher: --autocompact is ignored for pi (no per-launch compaction knob)" >&2
  set -- --name "$session_name" \
    --model "${model:-$(orch_default model)}" \
    --thinking "${effort:-$(orch_default effort)}" \
    --no-approve \
    --append-system-prompt "$protocol"
  # pi has no --mcp-config: catalog servers come from a generated extension
  # that registers exactly them for this launch.
  if [ -n "$catalog" ]; then
    mcp_file="$(mcp_render_pi dispatcher "$catalog")" || exit 1
    set -- "$@" -e "$mcp_file"
  fi
  [ -n "$task" ] && set -- "$@" "$task"
  env PATH="$engine_env_path" pi "$@"
  ;;
esac

# The launches above are children, not exec — deregister so the bus doesn't
# accumulate stale entries.
if git rev-parse --git-common-dir >/dev/null 2>&1; then
  crew deregister
fi
