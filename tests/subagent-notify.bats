bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/subagent-notify.sh: a Claude Code execute subagent's spawn and
# finish, posted as `crew msg <worker> subagent:<crew> <json>` rows that never
# wake `crew watch` (#1582). Hook JSON on stdin; the hook prints nothing and
# exits 0 on every path.

setup() {
  load helpers
  HOOK="$BATS_TEST_DIRNAME/../adapters/core/subagent-notify.sh"
  HOOKS_JSON="$BATS_TEST_DIRNAME/../adapters/claude-code/plugin/hooks/hooks.json"
  setup_repo
  unset CREW_WORKER_ID CREW_ID CREW_ROLE_ID CREW_SUBAGENT CLAUDE_CODE_SUBAGENT_EFFORT
  git commit --allow-empty -qm init
  git switch -qc feat/x
  printf 'crew_id: c1\n' >WORKER_TASK.md
  export CREW_WORKER_ID='worker:feat/x#s1-1'
  LOG="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  # The hook runs `crew` from PATH; helpers only export $CREW.
  mkdir -p "$BATS_TEST_TMPDIR/bin"
  printf '#!/usr/bin/env bash\nexec "$CREW" "$@"\n' >"$BATS_TEST_TMPDIR/bin/crew"
  chmod +x "$BATS_TEST_TMPDIR/bin/crew"
  export CREW
  PATH="$BATS_TEST_TMPDIR/bin:$PATH"
}

teardown() {
  teardown_repo
}

# hook <jq-object-expr> — the payload is `{cwd: $PWD} + <expr>`.
hook() {
  jq -nc --arg c "$PWD" "{cwd: \$c} + $1" | bash "$HOOK"
}

# rows — the bodies posted to the sink, one compact object per line.
rows() {
  [ -f "$LOG" ] || return 0
  jq -c 'select(.to == "subagent:c1" and .kind == "msg" and .from == "worker:feat/x#s1-1") | .body | fromjson' "$LOG"
}

quiet_exit0() {
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

no_rows() {
  [ -z "$(rows)" ]
}

@test "PreToolUse Agent posts a spawn with id, label, agent, model and effort" {
  run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1",tool_input:{description:"explore",subagent_type:"Explore",model:"haiku",effort:"low"}}'
  quiet_exit0
  run rows
  [ "$output" = '{"subagent":"spawn","id":"toolu_1","engine":"claude","label":"explore","agent":"Explore","model":"haiku","effort":"low"}' ]
}

@test "PreToolUse falls back to CLAUDE_CODE_SUBAGENT_EFFORT and omits absent fields" {
  CLAUDE_CODE_SUBAGENT_EFFORT=high run hook '{hook_event_name:"PreToolUse",tool_name:"Task",tool_use_id:"toolu_1",tool_input:{description:"d"}}'
  quiet_exit0
  run rows
  [ "$output" = '{"subagent":"spawn","id":"toolu_1","engine":"claude","label":"d","effort":"high"}' ]
}

@test "PreToolUse clips a long description to 80 characters" {
  run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1",tool_input:{description:("x" * 200)}}'
  quiet_exit0
  run rows
  [ "$(jq -r .label <<<"$output" | wc -c)" -eq 81 ]
}

@test "PreToolUse tolerates a non-object tool_input" {
  run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1",tool_input:"oops"}'
  quiet_exit0
  run rows
  [ "$output" = '{"subagent":"spawn","id":"toolu_1","engine":"claude"}' ]
}

@test "PostToolUse completed posts a finish ok with agent_id and model" {
  run hook '{hook_event_name:"PostToolUse",tool_name:"Agent",tool_use_id:"toolu_1",tool_response:{status:"completed",agentId:"a1",resolvedModel:"claude-haiku-4-5"}}'
  quiet_exit0
  run rows
  [ "$output" = '{"subagent":"finish","id":"toolu_1","outcome":"ok","model":"claude-haiku-4-5","agent_id":"a1"}' ]
}

@test "PostToolUse async_launched posts a spawn update binding agent_id" {
  run hook '{hook_event_name:"PostToolUse",tool_name:"Agent",tool_use_id:"toolu_1",tool_response:{status:"async_launched",agentId:"a1",resolvedModel:"m"}}'
  quiet_exit0
  run rows
  [ "$output" = '{"subagent":"spawn","id":"toolu_1","agent_id":"a1","model":"m"}' ]
}

@test "PostToolUse with another status posts nothing" {
  run hook '{hook_event_name:"PostToolUse",tool_name:"Agent",tool_use_id:"toolu_1",tool_response:{status:"weird"}}'
  quiet_exit0
  no_rows
}

@test "PostToolUseFailure posts a finish error, aborted on an interrupt" {
  run hook '{hook_event_name:"PostToolUseFailure",tool_name:"Agent",tool_use_id:"toolu_1"}'
  quiet_exit0
  run hook '{hook_event_name:"PostToolUseFailure",tool_name:"Agent",tool_use_id:"toolu_2",is_interrupt:true}'
  quiet_exit0
  run rows
  [ "${lines[0]}" = '{"subagent":"finish","id":"toolu_1","outcome":"error"}' ]
  [ "${lines[1]}" = '{"subagent":"finish","id":"toolu_2","outcome":"aborted"}' ]
}

@test "SubagentStop posts a finish by agent_id without an id" {
  run hook '{hook_event_name:"SubagentStop",agent_id:"a1",agent_type:"Explore"}'
  quiet_exit0
  run rows
  [ "$output" = '{"subagent":"finish","agent_id":"a1","outcome":"ok"}' ]
}

@test "no post: a tool other than Agent or Task" {
  run hook '{hook_event_name:"PreToolUse",tool_name:"Bash",tool_use_id:"toolu_1",tool_input:{command:"ls"}}'
  quiet_exit0
  no_rows
}

@test "no post: an unknown event" {
  run hook '{hook_event_name:"SessionStart",tool_name:"Agent",tool_use_id:"toolu_1"}'
  quiet_exit0
  no_rows
}

@test "no post: a tool event without tool_use_id, a SubagentStop without agent_id" {
  run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_input:{description:"d"}}'
  quiet_exit0
  run hook '{hook_event_name:"SubagentStop"}'
  quiet_exit0
  no_rows
}

@test "no post: garbage stdin" {
  run bash -c 'printf "not json" | bash "$1"' _ "$HOOK"
  quiet_exit0
  no_rows
}

@test "no post: CREW_WORKER_ID unset" {
  unset CREW_WORKER_ID
  run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1"}'
  quiet_exit0
  no_rows
}

@test "no post: a role pane or a pi subagent child" {
  CREW_ROLE_ID=r1 run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1"}'
  quiet_exit0
  CREW_SUBAGENT=1 run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1"}'
  quiet_exit0
  no_rows
}

@test "no post: no crew id resolves" {
  rm WORKER_TASK.md
  run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1"}'
  quiet_exit0
  no_rows
}

@test "no post: crew is not on PATH" {
  local d="$BATS_TEST_TMPDIR/nocrew" tool
  mkdir -p "$d"
  for tool in bash jq cat dirname; do
    ln -s "$(command -v "$tool")" "$d/$tool"
  done
  run env PATH="$d" bash -c 'jq -nc --arg c "$PWD" "{cwd:\$c,hook_event_name:\"PreToolUse\",tool_name:\"Agent\",tool_use_id:\"t\"}" | "$1" "$2"' _ "$(command -v bash)" "$HOOK"
  quiet_exit0
  no_rows
}

@test "a spawn row never wakes crew watch" {
  run hook '{hook_event_name:"PreToolUse",tool_name:"Agent",tool_use_id:"toolu_1",tool_input:{description:"d"}}'
  quiet_exit0
  run rows
  [ -n "$output" ]
  run --separate-stderr crew watch --since 0 --timeout 1 --interval 1
  [ -z "$output" ]
}

@test "hooks.json wires the four events to subagent-notify.sh with timeout 5" {
  run jq -r '
    .hooks as $h
    | [ ($h.PreToolUse[-1], $h.PostToolUse[0], $h.PostToolUseFailure[0]) | select(.matcher == "Agent|Task") ] | length' "$HOOKS_JSON"
  [ "$output" = 3 ]
  run jq -e '
    .hooks as $h
    | all(($h.PreToolUse[-1], $h.PostToolUse[0], $h.PostToolUseFailure[0], $h.SubagentStop[0]).hooks[0];
          (.command | endswith("/scripts/subagent-notify.sh")) and .timeout == 5)
    and ($h.SubagentStop[0] | has("matcher") | not)' "$HOOKS_JSON"
  [ "$status" -eq 0 ]
}
