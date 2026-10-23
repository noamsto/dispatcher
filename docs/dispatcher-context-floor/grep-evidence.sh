#!/usr/bin/env bash
# Grep evidence for the dispatcher launch profile (#1271, Q2): how often
# DISPATCHER_PROTOCOL.md names each tool, MCP server, plugin and skill the
# profile would remove or keep. Prints TSV: kind, name, removed-by-profile,
# protocol hits and the hit line numbers. Tool names are matched
# case-sensitively as whole words (`rg -c -w -F`); MCP, plugin and skill names
# case-insensitively as substrings (`rg -c -i -F`). Hits are then read by hand:
# the report says which are the name in another sense.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
proto="$here/../../adapters/core/protocols/DISPATCHER_PROTOCOL.md"

row() {
  local kind="$1" name="$2" removed="$3" term="${4:-$2}" n lines
  local -a flags=(-i -F)
  [ "$kind" = tool ] && flags=(-w -F)
  n="$(rg -c "${flags[@]}" -- "$term" "$proto" || echo 0)"
  lines="$(rg -n "${flags[@]}" -- "$term" "$proto" | cut -d: -f1 | head -n 8 | paste -sd, -)" || true
  printf '%s\t%s\t%s\t%s\t%s\n' "$kind" "$name" "$removed" "$n" "${lines:--}"
}

printf 'kind\tname\tremoved\tprotocol_hits\tlines\n'
for t in CronCreate CronDelete CronList DesignSync EnterWorktree ExitWorktree ListAgents LSP NotebookEdit \
  RemoteTrigger ReportFindings ScheduleWakeup SendMessage Workflow ListMcpResourcesTool ReadMcpResourceTool \
  ReadMcpResourceDirTool SendFeedback EndConversation EnterPlanMode ExitPlanMode; do
  row tool "$t" yes
done
for t in Artifact ArtifactComments ArtifactData WebSearch PushNotification; do
  row tool "$t" "profile only (kept by profile-rec)"
done
for t in Bash Read Write Edit Monitor TaskStop Agent AskUserQuestion WebFetch Skill ToolSearch; do
  row tool "$t" no
done
row mcp context7 yes
row mcp playwright yes
row mcp firefox-devtools yes
for s in 'Claude Docs' ElevenLabs 'Google Calendar' 'Google Drive' Gmail PostHog Cloudflare Sentry; do
  row mcp "claude.ai $s" yes "$s"
done
row mcp 'claude.ai (any connector)' yes 'claude.ai'
row mcp 'mcp__ (any MCP tool name)' n/a 'mcp__'
for p in superpowers agent-smith frontend-design refactoring-agent commit-commands resolved context-efficient-tools; do
  row plugin "$p" yes
done
row skill 'skill (the word)' n/a skill
row skill 'dispatcher: plugin skills (deslop, spec-plan-critic, agent-browser)' n/a 'deslop'
