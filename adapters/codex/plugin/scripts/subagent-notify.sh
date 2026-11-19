#!/usr/bin/env bash
# Subagent-notify hook: record a Claude Code execute subagent's spawn and finish
# on the crew bus, as `crew msg <worker> subagent:<crew> <json>` rows, so the dash
# can draw the subagents under their worker (#1582). The sink is a synthetic
# recipient like metrics:<crew>, so the row never wakes `crew watch`.
#
# Registered sync in the claude plugin's hooks.json: PreToolUse, PostToolUse and
# PostToolUseFailure on Agent|Task, and SubagentStop. It runs on the tool path, so
# it NEVER BLOCKS AND NEVER SPEAKS: stdout from SubagentStop can inject context
# and exit 2 can block, so every command's output goes to /dev/null and every path
# exits 0 (no `set -e`).
#
# Workers' leads only: a session without CREW_WORKER_ID, a grid role pane (it
# inherits the lead's CREW_WORKER_ID) and a pi `subagent` child (CREW_SUBAGENT)
# are not the lead's subagents.

[[ -n ${CREW_WORKER_ID:-} && -z ${CREW_ROLE_ID:-} && -z ${CREW_SUBAGENT:-} ]] || exit 0
command -v jq >/dev/null 2>&1 || exit 0
command -v crew >/dev/null 2>&1 || exit 0

input=$(cat)

# `{k: empty}` would drop the whole object, so absent fields become null and are
# filtered out afterwards.
# shellcheck disable=SC2016 # jq program, not a bash format string
body=$(jq -c --arg eff "${CLAUDE_CODE_SUBAGENT_EFFORT:-}" '
  def s: if type == "string" then . else null end;
  def obj: if type == "object" then . else {} end;
  def clean: with_entries(select(.value != null and .value != ""));
  if type != "object" then empty else
  (.tool_input | obj) as $ti
  | (.tool_response | obj) as $tr
  | (.tool_use_id | s) as $id
  | (.tool_name | s) as $tool
  | (($tool == "Agent" or $tool == "Task")) as $agent
  | (if .hook_event_name == "PreToolUse" and $agent and $id != null then
       {subagent: "spawn", id: $id, engine: "claude",
        label: (($ti.description | s) // null | if . == null then null else .[0:80] end),
        agent: ($ti.subagent_type | s), model: ($ti.model | s),
        effort: (($ti.effort | s) // $eff)}
     elif .hook_event_name == "PostToolUse" and $agent and $id != null then
       if ($tr.status | s) == "completed" then
         {subagent: "finish", id: $id, outcome: "ok",
          model: ($tr.resolvedModel | s), agent_id: ($tr.agentId | s)}
       elif ($tr.status | s) == "async_launched" or ($tr.status | s) == "teammate_spawned" then
         {subagent: "spawn", id: $id,
          agent_id: ($tr.agentId | s), model: ($tr.resolvedModel | s)}
       else empty end
     elif .hook_event_name == "PostToolUseFailure" and $agent and $id != null then
       {subagent: "finish", id: $id,
        outcome: (if .is_interrupt == true then "aborted" else "error" end)}
     elif .hook_event_name == "SubagentStop" then
       {subagent: "finish", agent_id: (.agent_id | s), outcome: "ok"}
     else empty end)
  | clean
  | select(.id != null or .agent_id != null)
  end' <<<"$input" 2>/dev/null) || exit 0
[[ -n $body ]] || exit 0

cwd=$(jq -r 'if type == "object" and (.cwd | type) == "string" then .cwd else empty end' <<<"$input" 2>/dev/null)
[[ -n $cwd ]] || cwd=$PWD
cd "$cwd" 2>/dev/null || exit 0

t=()
command -v timeout >/dev/null 2>&1 && t=(timeout 2)
crew_id=$("${t[@]}" crew id 2>/dev/null)
[[ -n $crew_id ]] || exit 0
"${t[@]}" crew msg "$CREW_WORKER_ID" "subagent:$crew_id" "$body" >/dev/null 2>&1
exit 0
