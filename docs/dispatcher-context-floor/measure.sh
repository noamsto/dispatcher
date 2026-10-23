#!/usr/bin/env bash
# Turn-1 context probes for the dispatcher launch (#1271).
#
# Usage: docs/dispatcher-context-floor/measure.sh <variant>...
#        docs/dispatcher-context-floor/measure.sh --list
#
# Each variant is one `claude -p` first turn. The turn-1 size is
# input + cache_read + cache_creation tokens from the result usage. Every run
# appends one JSON line to runs.jsonl beside this script. The prompt is a fixed
# "Reply with just OK." (chunk variants prepend a text chunk to it, so their
# delta against `bare-prompt` is the chunk's real token count).
#
# Environment:
#   MAIN    cwd for every probe (default: the main checkout the dispatcher runs
#           in). Used read-only.
#   MODEL   default opus; EFFORT default high (dispatcher orchestratorDefaults).
#   CHUNK   file to prepend to the prompt for the `chunk` variant.
#   LABEL   label stored with a `chunk` run.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
proto="$repo/adapters/core/protocols/DISPATCHER_PROTOCOL.md"
out="$here/runs.jsonl"
MAIN="${MAIN:-$(git -C "$repo" rev-parse --path-format=absolute --git-common-dir | sed 's|/\.git$||')}"
MODEL="${MODEL:-opus}"
EFFORT="${EFFORT:-high}"

# shellcheck source=/dev/null
source "$repo/adapters/core/claude-worker-settings.sh"

empty_mcp='{"mcpServers":{}}'
# Built-in tools the dispatcher protocol never names (grep evidence in the
# report). The first rows are in the 2.1.307 `-p` init tool list; the last row
# exists only in interactive sessions (unknown names are ignored under -p).
noncore=(CronCreate CronDelete CronList DesignSync EnterWorktree ExitWorktree ListAgents LSP NotebookEdit
  PushNotification RemoteTrigger ReportFindings ScheduleWakeup SendMessage WebSearch Workflow
  ListMcpResourcesTool ReadMcpResourceTool ReadMcpResourceDirTool
  Artifact ArtifactComments ArtifactData SendFeedback EndConversation EnterPlanMode ExitPlanMode)
# The recommended profile keeps the non-core tools dispatcher sessions call
# often in the 7-day fleet (Artifact family, WebSearch, PushNotification;
# fleet-tools.py counts them) and drops RemoteTrigger and SendMessage (rare).
noncore_rec=(CronCreate CronDelete CronList DesignSync EnterWorktree ExitWorktree ListAgents LSP NotebookEdit
  RemoteTrigger ReportFindings ScheduleWakeup SendMessage Workflow
  ListMcpResourcesTool ReadMcpResourceTool ReadMcpResourceDirTool
  SendFeedback EndConversation EnterPlanMode ExitPlanMode)
core_tools='Bash,Read,Edit,Write'
# Every built-in in the 2.1.307 init tool list except ToolSearch (needed to keep
# MCP tools deferred), Task (the Agent tool: own row) and Skill (skills row).
builtins=(Bash Read Edit Write Monitor TaskStop WebFetch "${noncore[@]}")

# Worker plugin layer (claude-worker-settings.sh), with workerDeniedMcpServers
# from defaults.json. printf %q output is evaluated back to the raw JSON.
plugin_layer() {
  local q
  q="$(claude_worker_plugin_settings "$(jq -c . "$repo/adapters/core/defaults.json")")"
  eval "printf '%s' $q"
}

# Hooks off. The Nix claude wrapper merges caller --settings layers into its
# overlay and refuses a layer that sets disableAllHooks, so a hooks-off probe
# does what the wrapper does by hand: merge the overlay, the variant's layers
# and {"disableAllHooks":true} into ONE settings file (Claude keeps only the
# last --settings) and exec the wrapped binary with the wrapper's --plugin-dir
# arguments. Everything else matches the wrapped launch.
wrapper_exec() {
  local wrapper overlay line bin settings_file
  local -a words plugin_args=() layers=() rest=()
  wrapper="$(readlink -f "$(command -v claude)")"
  overlay="$(sed -nE 's/^claude_resolve_args ([^ ]+) .*/\1/p' "$wrapper")"
  line="$(sed -nE 's/^exec -a claude (.*) "\$\{args\[@\]\}"$/\1/p' "$wrapper")"
  [ -n "$overlay" ] && [ -n "$line" ] || {
    echo "measure.sh: cannot parse the claude wrapper $wrapper" >&2
    return 1
  }
  read -ra words <<<"$line"
  bin="${words[0]}"
  for ((i = 1; i < ${#words[@]}; i++)); do
    [ "${words[i]}" = --plugin-dir ] && plugin_args+=(--plugin-dir "${words[i + 1]}")
  done
  while [ $# -gt 0 ]; do
    if [ "$1" = --settings ]; then
      layers+=("$2")
      shift 2
    else
      rest+=("$1")
      shift
    fi
  done
  settings_file="$(mktemp)"
  {
    cat "$overlay"
    printf '%s\n' "${layers[@]}" '{"disableAllHooks":true}'
  } |
    jq -s 'reduce .[] as $l ({}; . * $l)' >"$settings_file"
  local rc=0
  "$bin" --settings "$settings_file" "${plugin_args[@]}" "${rest[@]}" || rc=$?
  rm -f "$settings_file"
  return "$rc"
}

# Each variant sets: env_extra (array), args (array), with_proto (0/1), chunk.
variant() {
  env_extra=()
  args=()
  with_proto=1
  hooks=1
  chunk=''
  case "$1" in
  # ---- full analog and one-flag-off ablations ----
  full) ;;
  off-proto) with_proto=0 ;;
  off-mcp) args=(--strict-mcp-config --mcp-config "$empty_mcp") ;;
  off-claudeai) env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  off-skills) args=(--disable-slash-commands) ;;
  off-agent) args=(--disallowedTools Task) ;;
  off-hooks) hooks=0 ;;
  off-tools) args=(--tools '') ;;
  off-noncore) args=(--disallowedTools "${noncore[@]}") ;;
  off-artifact) args=(--disallowedTools Artifact ArtifactComments ArtifactData) ;;
  off-workflow) args=(--disallowedTools Workflow) ;;
  off-ask) args=(--disallowedTools AskUserQuestion) ;;
  off-builtins) args=(--disallowedTools "${builtins[@]}") ;;
  tools-bash) args=(--tools Bash) ;;
  # ---- incremental build-up from all-off (fixed order, no overlap) ----
  inc0-alloff) with_proto=0 args=(--tools '' --strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  inc1-proto) args=(--tools '' --strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  inc2-core) args=(--tools "$core_tools" --strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  inc3-builtins) args=(--tools default --disallowedTools Task --strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  inc4-agent) args=(--tools default --strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  inc5-skills) args=(--tools default --strict-mcp-config --mcp-config "$empty_mcp") hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  inc6-pluginmcp) args=(--tools default) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  inc7-claudeai) args=(--tools default) hooks=0 ;;
  # inc8 is `full`.
  # ---- dispatcher launch profile candidates ----
  profile)
    args=(--strict-mcp-config --mcp-config "$empty_mcp" --disallowedTools "${noncore[@]}" --settings "$(plugin_layer)") hooks=0
    env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false)
    ;;
  profile-hooks)
    args=(--strict-mcp-config --mcp-config "$empty_mcp" --disallowedTools "${noncore[@]}" --settings "$(plugin_layer)")
    env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false)
    ;;
  profile-rec)
    args=(--strict-mcp-config --mcp-config "$empty_mcp" --disallowedTools "${noncore_rec[@]}" --settings "$(plugin_layer)")
    env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false)
    ;;
  profile-noskills)
    args=(--strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands --disallowedTools "${noncore[@]}" --settings "$(plugin_layer)") hooks=0
    env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false)
    ;;
  profile-max)
    args=(--strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands --disallowedTools Task "${noncore[@]}" --settings "$(plugin_layer)") hooks=0
    env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false)
    ;;
  plugins-off) args=(--settings "$(plugin_layer)") ;;
  # ---- text chunks: real tokens of a file, appended to the prompt ----
  bare-prompt) with_proto=0 args=(--tools '' --strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  chunk) with_proto=0 chunk="${CHUNK:?CHUNK=<file> required}" args=(--tools '' --strict-mcp-config --mcp-config "$empty_mcp" --disable-slash-commands) hooks=0 env_extra=(ENABLE_CLAUDEAI_MCP_SERVERS=false) ;;
  *)
    echo "measure.sh: unknown variant $1" >&2
    return 1
    ;;
  esac
}

list() { sed -nE 's/^  ([a-z0-9-]+)\).*/\1/p' "${BASH_SOURCE[0]}"; }

# Interactive-shaped run (INTERACTIVE=1): the same argv without -p, in a
# detached tmux window, cwd $MAIN. Types the prompt once the input box is up,
# reads the first assistant usage from the session transcript, then kills the
# window and deletes the probe transcript so it never enters fleet statistics.
run_interactive() {
  local runner="$1" res sid win launch transcript i
  shift
  sid="$(cat /proc/sys/kernel/random/uuid)"
  transcript="$HOME/.claude/projects/$(printf '%s' "$MAIN" | tr '/.' '--')/$sid.jsonl"
  launch="$(mktemp)"
  {
    printf 'cd %q\n' "$MAIN"
    printf 'exec env -u CREW_WORKER_ID -u CREW_ID -u TMUX_PANE'
    printf ' %q' "${env_extra[@]}" "$here/measure.sh" --exec "$runner" "$@" --session-id "$sid"
    printf '\n'
  } >"$launch"
  win="$(tmux new-window -d -P -F '#{window_id}' -n floor-probe "bash $launch")"
  for ((i = 0; i < 60; i++)); do
    sleep 2
    tmux capture-pane -p -t "$win" | grep -q '❯' && break
  done
  sleep 3
  tmux send-keys -t "$win" -l "$prompt"
  sleep 1
  tmux send-keys -t "$win" Enter
  for ((i = 0; i < 90; i++)); do
    sleep 2
    res="$(jq -c 'select(.type == "assistant" and .message.usage) | {usage: .message.usage, is_error: false}' "$transcript" 2>/dev/null | head -n1)"
    [ -n "$res" ] && break
  done
  tmux kill-window -t "$win" 2>/dev/null || true
  rm -f "$launch" "$transcript"
  [ -n "$res" ] || {
    echo "measure.sh: no assistant turn in $transcript" >&2
    return 1
  }
  printf '%s\n' "$res"
}

run() {
  local name="$1" res prompt="Reply with just OK." label="${LABEL:-}"
  variant "$name"
  if [ "${INTERACTIVE:-0}" = 1 ]; then
    local runner=claude
    [ "$hooks" = 0 ] && runner=wrapper_exec
    local -a icmd=(--name dispatcher-floor-probe --model "$MODEL" --effort "$EFFORT")
    [ "$with_proto" = 1 ] && icmd+=(--append-system-prompt-file "$proto")
    icmd+=("${args[@]}")
    res="$(run_interactive "$runner" "${icmd[@]}")"
    name="interactive:$name"
    label=''
  else
    local -a cmd=(-p --name dispatcher-floor-probe --model "$MODEL" --effort "$EFFORT"
      --max-turns 1 --output-format json --no-session-persistence)
    [ "$with_proto" = 1 ] && cmd+=(--append-system-prompt-file "$proto")
    cmd+=("${args[@]}")
    if [ -n "$chunk" ]; then
      prompt="$(cat "$chunk")"$'\n\nReply with just OK.'
    else
      label=''
    fi
    local runner=claude
    [ "$hooks" = 0 ] && runner=wrapper_exec
    res="$(cd "$MAIN" && printf '%s' "$prompt" |
      env -u CREW_WORKER_ID -u CREW_ID "${env_extra[@]}" timeout 300 "$here/measure.sh" --exec "$runner" "${cmd[@]}")"
  fi
  jq -c --arg v "$name" --arg label "$label" --arg model "$MODEL" --arg effort "$EFFORT" \
    --arg ver "$(claude --version | cut -d' ' -f1)" --arg md5 "$(md5sum "$proto" | cut -c1-8)" \
    --arg date "$(date -u +%FT%TZ)" '
    .usage as $u | {
      variant: $v, label: (if $label == "" then null else $label end),
      total: ($u.input_tokens + $u.cache_read_input_tokens + $u.cache_creation_input_tokens),
      input: $u.input_tokens, cache_read: $u.cache_read_input_tokens,
      cache_write: $u.cache_creation_input_tokens, is_error, model: $model,
      effort: $effort, claude: $ver, protocol_md5: $md5, date: $date
    }' <<<"$res" | tee -a "$out"
}

if [ "${1:-}" = --exec ]; then
  shift
  runner="$1"
  shift
  "$runner" "$@"
  exit
fi
if [ "${1:-}" = --list ]; then
  list
  exit 0
fi
for v in "$@"; do run "$v"; done
