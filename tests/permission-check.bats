bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/permission-check.sh: the dispatcher's auto-approve checker for
# Claude permission dialogs. See the #441 spec and plan.

FIXTURE_DIR="$BATS_TEST_DIRNAME/fixtures/permission"
CLASSIFIER_FRAME="$FIXTURE_DIR/classifier-escalation.txt"
DENIAL_FIXTURE="$FIXTURE_DIR/classifier-denial.txt"
DENIAL=$(<"$DENIAL_FIXTURE")

# A world the checker binds a dialog against: a lead record, artifacts and
# grants for a slashed branch, a worktree, a grant dir, an immutable root, and
# the lead's transcript with one foreground subagent whose parent `Agent` call
# is pending while it runs. The transcripts live in a tmp HOME's
# `.claude/projects`, where --pane mode finds them; that HOME is no ancestor of
# the worktree or grant.
setup() {
  load helpers
  CHECK="$BATS_TEST_DIRNAME/../adapters/core/permission-check.sh"
  BRANCH=feat/441-x
  SID=3f2a9c1e-5b7d-4e8f-9a0b-1c2d3e4f5a6b
  CREW="$BATS_TEST_TMPDIR/crew"
  WT="$BATS_TEST_TMPDIR/wt"
  GRANT="$BATS_TEST_TMPDIR/grant"
  RO="$BATS_TEST_TMPDIR/ro"
  PANE_HOME="$BATS_TEST_TMPDIR/home"
  PROJECTS="$PANE_HOME/.claude/projects"
  FRAME="$BATS_TEST_TMPDIR/frame.txt"
  F="$WT/README.md"
  stubs
  ps_table
  worktrees

  mkdir -p "$CREW/leads/${BRANCH%/*}" "$CREW/artifacts/$BRANCH" "$CREW/grants/${BRANCH%/*}"
  printf 'claude %s\n' "$SID" >"$CREW/leads/$BRANCH"
  printf '%s\n' "$GRANT" >"$CREW/grants/$BRANCH"

  mkdir -p "$WT/docs" "$GRANT" "$RO"
  printf 'readme\n' >"$WT/README.md"
  printf 'doc foo\n' >"$WT/docs/x.md"
  printf 'a\n' >"$WT/a"
  printf 'notes foo\n' >"$GRANT/notes.md"
  printf 'FAKE=1\n' >"$GRANT/.env"
  printf 'ro foo\n' >"$RO/r.md"

  local slug
  slug=$(realpath -e "$WT" | sed 's/[^A-Za-z0-9]/-/g')
  PROJ="$PROJECTS/$slug"
  LEAD_LOG="$PROJ/$SID.jsonl"
  SUB_LOG="$PROJ/$SID/subagents/agent-a1.jsonl"
  mkdir -p "$PROJ/$SID/subagents"

  jq -nc --arg sid "$SID" --arg cwd "$WT" \
    '{type:"user",sessionId:$sid,cwd:$cwd,uuid:"u0",
      message:{role:"user",content:"review the branch"}}' >"$LEAD_LOG"
  jq -nc --arg sid "$SID" --arg cwd "$WT" \
    '{type:"assistant",sessionId:$sid,cwd:$cwd,uuid:"u1",parentUuid:"u0",
      message:{role:"assistant",content:[{type:"tool_use",id:"toolu_parent",name:"Agent",
        input:{description:"Review: shell",subagent_type:"shell-reviewer",prompt:"review it"}}]}}' \
    >>"$LEAD_LOG"

  jq -nc --arg sid "$SID" --arg cwd "$WT" \
    '{type:"user",sessionId:$sid,cwd:$cwd,agentId:"a1",isSidechain:true,uuid:"s0",
      message:{role:"user",content:"review it"}}' >"$SUB_LOG"
  jq -nc '{agentType:"shell-reviewer",toolUseId:"toolu_parent",spawnDepth:1}' \
    >"$PROJ/$SID/subagents/agent-a1.meta.json"
}

# stubs — `tmux`, `ps`, `git` and `sleep` on STUBS, which the checker runs on
# PATH. tmux logs its argv to TMUX_LOG; capture-pane prints PANE_FRAME, or
# PANE_FRAME_ALT on call number PANE_FRAME_ALT_AT; display-message prints
# PANE_PID and exits DISPLAY_STATUS; send-keys exits SEND_KEYS_STATUS. ps prints
# PS_TABLE. git logs a `worktree list` to GIT_LOG and prints GIT_WORKTREES
# (a porcelain listing, one field per line) NUL-terminated as -z does; every
# other git call goes to the real git. sleep logs its argument to SLEEP_LOG,
# runs SLEEP_HOOK (a shell command that changes the world mid-settle) and
# returns at once.
#
# git is stubbed, not run against a throwaway repo, because real git refuses
# to check one branch out in two worktrees; one real-git row covers the rest.
stubs() {
  STUBS="$BATS_TEST_TMPDIR/stubs"
  mkdir -p "$STUBS"
  cat >"$STUBS/tmux" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$TMUX_LOG"
case $1 in
capture-pane)
  n=$(($(cat "$TMUX_LOG.captures" 2>/dev/null || echo 0) + 1))
  printf '%s\n' "$n" >"$TMUX_LOG.captures"
  if [ "$n" = "${PANE_FRAME_ALT_AT:-}" ]; then cat "$PANE_FRAME_ALT"; else cat "$PANE_FRAME"; fi
  ;;
display-message)
  printf '%s\n' "${PANE_PID-4242}"
  exit "${DISPLAY_STATUS:-0}"
  ;;
send-keys) exit "${SEND_KEYS_STATUS:-0}" ;;
esac
STUB
  cat >"$STUBS/ps" <<'STUB'
#!/usr/bin/env bash
cat "$PS_TABLE"
STUB
  cat >"$STUBS/git" <<'STUB'
#!/usr/bin/env bash
if [ "$1" = -C ] && [ "${*:3}" = "worktree list --porcelain -z" ]; then
  printf '%s\n' "$*" >>"$GIT_LOG"
  tr '\n' '\0' <"$GIT_WORKTREES"
  exit 0
fi
exec "$REAL_GIT" "$@"
STUB
  cat >"$STUBS/sleep" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SLEEP_LOG"
if [ -n "${SLEEP_HOOK:-}" ]; then eval "$SLEEP_HOOK"; fi
STUB
  chmod +x "$STUBS/tmux" "$STUBS/ps" "$STUBS/git" "$STUBS/sleep"
  REAL_GIT=$(command -v git)
  export REAL_GIT TMUX_LOG="$BATS_TEST_TMPDIR/tmux.log" SLEEP_LOG="$BATS_TEST_TMPDIR/sleep.log"
  export GIT_LOG="$BATS_TEST_TMPDIR/git.log" PANE_FRAME="$FRAME"
  export PS_TABLE="$BATS_TEST_TMPDIR/ps.txt" GIT_WORKTREES="$BATS_TEST_TMPDIR/worktrees.txt"
  unset PANE_FRAME_ALT PANE_FRAME_ALT_AT SEND_KEYS_STATUS SLEEP_HOOK PERMISSION_CHECK_SETTLE
  unset PANE_PID DISPLAY_STATUS
}

# ps_table [claude-args...] — the pane's process tree: bash (4242, the pane
# pid) runs a launch script that runs the given claude command lines, one
# process each (default: one started on the lead's --session-id). A claude on
# another session outside the pane is always present.
ps_table() {
  local -a cmds=("$@")
  local pid=4244 c
  [ "$#" -gt 0 ] || cmds=("claude --name x --model opus --session-id $SID --permission-mode auto 'prompt'")
  {
    printf '%7s %7s %s\n' 1 0 init 4242 4241 bash 4243 4242 'bash /tmp/launch.sh'
    for c in "${cmds[@]}"; do
      printf '%7s %7s %s\n' "$pid" 4243 "$c"
      pid=$((pid + 1))
    done
    printf '%7s %7s %s\n' 5000 1 'claude --session-id 00000000-0000-4000-8000-000000000000'
  } >"$PS_TABLE"
}

# worktrees [branch-ref...] — the porcelain listing: the main worktree on
# main, then WT on each given ref (default: the world's branch).
worktrees() {
  local ref
  [ "$#" -gt 0 ] || set -- "refs/heads/$BRANCH"
  {
    printf 'worktree %s\nHEAD %s\nbranch refs/heads/main\n\n' "$BATS_TEST_TMPDIR/main" "$(printf '1%.0s' {1..40})"
    for ref in "$@"; do
      printf 'worktree %s\nHEAD %s\nbranch %s\n\n' "$WT" "$(printf '2%.0s' {1..40})" "$ref"
    done
  } >"$GIT_WORKTREES"
}

# pending_bash <command> [description] — the subagent asks for a Bash call
# (id toolu_live) that has no result yet.
pending_bash() {
  local input
  if [ "$#" -ge 2 ]; then
    input=$(jq -nc --arg c "$1" --arg d "$2" '{command:$c,description:$d}')
  else
    input=$(jq -nc --arg c "$1" '{command:$c}')
  fi
  pending_input "$input"
}

# pending_input <input-json> [wire-json] — pending_bash with a raw `input`;
# wireToolInputs records <wire-json> (default: the same input).
pending_input() {
  jq -nc --arg sid "$SID" --arg cwd "$WT" --argjson input "$1" --argjson wire "${2:-$1}" \
    '{type:"assistant",sessionId:$sid,cwd:$cwd,agentId:"a1",isSidechain:true,uuid:"s1",
      message:{role:"assistant",content:[{type:"tool_use",id:"toolu_live",name:"Bash",input:$input}]},
      wireToolInputs:{toolu_live:$wire}}' >>"$SUB_LOG"
}

# tool_use <id> <name> [log] — an assistant tool_use appended to <log>
# (default: the subagent file).
tool_use() {
  jq -nc --arg sid "$SID" --arg id "$1" --arg name "$2" \
    '{type:"assistant",sessionId:$sid,isSidechain:true,uuid:"s3",
      message:{role:"assistant",content:[{type:"tool_use",id:$id,name:$name,
        input:{command:"true"}}]}}' >>"${3:-$SUB_LOG}"
}

# result_for <id> [text] [log] — a tool_result for <id> lands in <log>
# (default: the subagent file).
result_for() {
  jq -nc --arg sid "$SID" --arg id "$1" --arg text "${2:-ok}" \
    '{type:"user",sessionId:$sid,agentId:"a1",isSidechain:true,uuid:"s2",
      message:{role:"user",content:[{type:"tool_result",tool_use_id:$id,content:$text}]}}' \
    >>"${3:-$SUB_LOG}"
}

# frame <line1> [line2] — the real capture's dialog with the classifier's `│`
# box removed and the request block replaced. FRAME_HEADER overrides the
# header line.
frame() {
  local sep header
  sep=$(grep '^─' "$CLASSIFIER_FRAME" | tail -n 1)
  header=${FRAME_HEADER:-Bash command · from the shell-reviewer agent}
  {
    printf '%s\n' "$sep"
    printf ' %s\n' "$header"
    printf '\n'
    printf '   %s\n' "$@"
    printf '\n'
    printf ' Do you want to proceed?\n'
    printf ' ❯ 1. Yes\n'
    printf '   2. Yes, and don’t ask again for: %s *\n' "${1%% *}"
    printf '   3. No\n'
    printf '\n'
    printf ' Esc to cancel · Tab to amend\n'
  } >"$FRAME"
}

# check [frame-file] — the checker in --capture mode against the world.
check() {
  run --separate-stderr env PATH="$STUBS:$PATH" bash "$CHECK" --capture "${1:-$FRAME}" \
    --branch "$BRANCH" --worktree "$WT" --crew-dir "$CREW" \
    --projects-dir "$PROJECTS" --ro-root "$RO"
}

# pane [arg...] — the checker in --pane mode on %9 through the tmux stub,
# finding the worktree through the git stub and the transcripts under the tmp
# HOME.
pane() {
  run --separate-stderr env PATH="$STUBS:$PATH" HOME="$PANE_HOME" bash "$CHECK" --pane %9 \
    --branch "$BRANCH" --crew-dir "$CREW" "$@"
}

# no_send_keys — the tmux stub never saw send-keys.
no_send_keys() {
  ! grep -q '^send-keys' "$TMUX_LOG"
}

assert_human() {
  [ "$status" -eq 1 ]
  [ "${#lines[@]}" -eq 1 ]
  [[ "$output" == "human: "* ]]
}

# assert_refused <reason-fragment> — refused, naming the rule.
assert_refused() {
  assert_human
  [[ "$output" == *"$1"* ]]
}

assert_allowed() {
  [ "$status" -eq 0 ]
  [ "$output" = allow-once ]
}

# try <command> — the subagent asks for <command> with no description, under
# a matching frame, and the checker decides.
try() {
  pending_bash "$1"
  frame "$1"
  check
}

# ---------------------------------------------------------------------------
# Positive
# ---------------------------------------------------------------------------

@test "permission-check: cat of a worktree file is allowed once" {
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  [ "$status" -eq 0 ]
  [ "$output" = allow-once ]
}

@test "permission-check: rg -n of a worktree file is allowed" {
  try "rg -n foo $WT/README.md"
  assert_allowed
}

@test "permission-check: a relative cat after a leading cd is allowed" {
  try "cd $WT && cat docs/x.md"
  assert_allowed
}

@test "permission-check: cat of a granted file is allowed" {
  try "cat $GRANT/notes.md"
  assert_allowed
}

@test "permission-check: cat of an artifacts file is allowed" {
  printf 'art\n' >"$CREW/artifacts/$BRANCH/n.md"
  try "cat $CREW/artifacts/$BRANCH/n.md"
  assert_allowed
}

@test "permission-check: head piped to wc is allowed" {
  try "head -n 5 $WT/a | wc -l"
  assert_allowed
}

@test "permission-check: grep -r under an immutable root is allowed" {
  try "grep -r -n foo $RO"
  assert_allowed
}

@test "permission-check: ls -l of a worktree dir is allowed" {
  try "ls -l $WT/docs"
  assert_allowed
}

@test "permission-check: a quoted pattern starting with a dash via -e is allowed" {
  try "rg -e '-x' $WT/README.md"
  assert_allowed
}

@test "permission-check: a quoted pattern with a space is allowed" {
  try "rg 'two words' $WT/README.md"
  assert_allowed
}

# ---------------------------------------------------------------------------
# Frame gate
# ---------------------------------------------------------------------------

@test "permission-check: the real classifier escalation goes to the human" {
  check "$CLASSIFIER_FRAME"
  assert_human
}

@test "permission-check: a three-line request block goes to the human" {
  pending_bash "cat $WT/README.md" "Show the readme"
  frame "cat $WT/README.md" "Show the readme" "and more"
  check
  assert_human
}

@test "permission-check: a top-level Bash command header goes to the human" {
  pending_bash "cat $WT/README.md"
  FRAME_HEADER="Bash command" frame "cat $WT/README.md"
  check
  assert_human
}

@test "permission-check: a Read file header goes to the human" {
  pending_bash "cat $WT/README.md"
  FRAME_HEADER="Read file · from the x agent" frame "cat $WT/README.md"
  check
  assert_human
}

@test "permission-check: a frame whose footer is not last goes to the human" {
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  printf ' > \n' >>"$FRAME"
  check
  assert_human
}

@test "permission-check: option 1 other than exactly Yes goes to the human" {
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  sed -i 's/❯ 1\. Yes$/❯ 1. Yes, allow/' "$FRAME"
  grep -qF '❯ 1. Yes, allow' "$FRAME"
  check
  assert_human
}

# ---------------------------------------------------------------------------
# Transcript binding
# ---------------------------------------------------------------------------

@test "permission-check: the single pending subagent Bash call binds" {
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_allowed
}

@test "permission-check: a pending call with a description binds to a two-line block" {
  pending_bash "cat $WT/README.md" "Show the readme"
  frame "cat $WT/README.md" "Show the readme"
  check
  assert_allowed
}

@test "permission-check: a classifier denial older than the last five results does not refuse" {
  result_for toolu_d0 "$DENIAL"
  for i in 1 2 3 4 5; do result_for "toolu_r$i"; done
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_allowed
}

@test "permission-check: no lead record goes to the human" {
  rm "$CREW/leads/$BRANCH"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "lead record"
}

@test "permission-check: a symlinked lead record goes to the human" {
  mv "$CREW/leads/$BRANCH" "$BATS_TEST_TMPDIR/lead-real"
  ln -s "$BATS_TEST_TMPDIR/lead-real" "$CREW/leads/$BRANCH"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "lead record"
}

@test "permission-check: a codex lead record goes to the human" {
  printf 'codex %s\n' "$SID" >"$CREW/leads/$BRANCH"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "lead record: not claude <uuid>"
}

@test "permission-check: a symlinked leads/ parent dir goes to the human" {
  mv "$CREW/leads/${BRANCH%/*}" "$BATS_TEST_TMPDIR/leads-real"
  ln -s "$BATS_TEST_TMPDIR/leads-real" "$CREW/leads/${BRANCH%/*}"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "leads/ dir"
}

@test "permission-check: an orphan pending call in another subagent goes to the human" {
  local orphan="$PROJ/$SID/subagents/agent-a2.jsonl"
  jq -nc '{agentType:"shell-reviewer",toolUseId:"toolu_parent2",spawnDepth:1}' \
    >"$PROJ/$SID/subagents/agent-a2.meta.json"
  tool_use toolu_orphan Bash "$orphan"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "more than one pending call"
}

@test "permission-check: the parent call is exempt only by its exact toolUseId" {
  jq -nc '{agentType:"shell-reviewer",toolUseId:"toolu_parentX",spawnDepth:1}' \
    >"$PROJ/$SID/subagents/agent-a1.meta.json"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "more than one pending call"
}

@test "permission-check: a pending subagent call under a finished parent goes to the human" {
  result_for toolu_parent ok "$LEAD_LOG"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "the parent call is not pending"
}

@test "permission-check: a pending lead call after the parent goes to the human" {
  tool_use toolu_extra Bash "$LEAD_LOG"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "more than one pending call"
}

@test "permission-check: a parent call that is not the lead's last tool_use goes to the human" {
  tool_use toolu_done Read "$LEAD_LOG"
  result_for toolu_done ok "$LEAD_LOG"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "the parent call is not the last tool_use in the lead transcript"
}

@test "permission-check: a spawnDepth 2 subagent goes to the human" {
  jq -nc '{agentType:"shell-reviewer",toolUseId:"toolu_parent",spawnDepth:2}' \
    >"$PROJ/$SID/subagents/agent-a1.meta.json"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "spawnDepth"
}

@test "permission-check: an agentType other than the header's goes to the human" {
  pending_bash "cat $WT/README.md"
  FRAME_HEADER="Bash command · from the go-reviewer agent" frame "cat $WT/README.md"
  check
  assert_refused "agentType"
}

@test "permission-check: a pending call that is not the last tool_use goes to the human" {
  pending_bash "cat $WT/README.md"
  tool_use toolu_later Read
  result_for toolu_later
  frame "cat $WT/README.md"
  check
  assert_refused "not the last tool_use"
}

@test "permission-check: a dangerouslyDisableSandbox input key goes to the human" {
  pending_input "$(jq -nc --arg wt "$WT" \
    '{command:"cat \($wt)/README.md",dangerouslyDisableSandbox:true}')"
  frame "cat $WT/README.md"
  check
  assert_refused "input is not command"
}

@test "permission-check: a run_in_background input key goes to the human" {
  pending_input "$(jq -nc --arg wt "$WT" '{command:"cat \($wt)/README.md",run_in_background:true}')"
  frame "cat $WT/README.md"
  check
  assert_refused "input is not command"
}

@test "permission-check: wireToolInputs differing from input goes to the human" {
  pending_input "$(jq -nc --arg wt "$WT" '{command:"cat \($wt)/README.md"}')" \
    "$(jq -nc --arg wt "$WT" '{command:"cat \($wt)/a"}')"
  frame "cat $WT/README.md"
  check
  assert_refused "wireToolInputs"
}

@test "permission-check: a command containing a newline goes to the human" {
  pending_input "$(jq -nc --arg wt "$WT" '{command:"cat \($wt)/README.md\nid"}')"
  frame "cat $WT/README.md"
  check
  assert_refused "printable ASCII"
}

@test "permission-check: a command containing a CR goes to the human" {
  pending_input "$(jq -nc --arg wt "$WT" '{command:"cat \($wt)/README.md\rid"}')"
  frame "cat $WT/README.md"
  check
  assert_refused "printable ASCII"
}

@test "permission-check: a command containing ESC goes to the human" {
  pending_input "$(jq -nc --arg wt "$WT" '{command:"cat \($wt)/README.md\u001b[2K"}')"
  frame "cat $WT/README.md"
  check
  assert_refused "printable ASCII"
}

@test "permission-check: a command containing non-ASCII goes to the human" {
  pending_input "$(jq -nc --arg wt "$WT" '{command:"cat \($wt)/ré.md"}')"
  frame "cat $WT/r.md"
  check
  assert_refused "printable ASCII"
}

@test "permission-check: a block line 2 other than the description goes to the human" {
  pending_bash "cat $WT/README.md" "Show the readme"
  frame "cat $WT/README.md" "Show something else"
  check
  assert_refused "request block"
}

@test "permission-check: a block that is not the command goes to the human" {
  pending_bash "cat $WT/README.md"
  frame "cat $WT/a"
  check
  assert_refused "request block"
}

@test "permission-check: a recent classifier denial goes to the human" {
  result_for toolu_d0 "$DENIAL"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "classifier denial"
}

@test "permission-check: a recent classifier denial in array content goes to the human" {
  jq -nc --arg text "$DENIAL" \
    '{type:"user",message:{role:"user",content:[{type:"tool_result",tool_use_id:"toolu_d0",
      content:[{type:"text",text:$text}]}]}}' >>"$SUB_LOG"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "classifier denial"
}

@test "permission-check: the matched denial phrase occurs in the real denial text" {
  local phrase
  phrase=$(sed -n 's/.*result_text | contains("\([^"]*\)").*/\1/p' "$CHECK")
  [ -n "$phrase" ]
  grep -qF -- "$phrase" "$DENIAL_FIXTURE"
}

@test "permission-check: a malformed JSON line goes to the human" {
  printf '%s\n' '{"type":"user"' >>"$SUB_LOG"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "not one JSON object"
}

@test "permission-check: a non-object JSON line mid-file goes to the human" {
  printf '%s\n' '[1]' >>"$LEAD_LOG"
  tool_use toolu_x Read "$LEAD_LOG"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "not one JSON object"
}

@test "permission-check: two JSON objects on one line go to the human" {
  printf '%s\n' '{"type":"user"}{"type":"user"}' >>"$SUB_LOG"
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
  check
  assert_refused "not one JSON object"
}

# ---------------------------------------------------------------------------
# Command grammar
# ---------------------------------------------------------------------------

@test "permission-check: a cd after the first command goes to the human" {
  try "cd $WT/a && cd $WT && cat ../x"
  assert_refused "grammar: cd only as the first command"
}

@test "permission-check: a .. operand after a leading cd goes to the human" {
  try "cd $WT && cat ../README.md"
  assert_refused "path: a . or .. component"
}

@test "permission-check: rg -E goes to the human" {
  try "rg -E utf8 notes.md"
  assert_refused "grammar: flag not allowed: rg -E"
}

@test "permission-check: rg -r goes to the human" {
  try "rg -r x pat $F"
  assert_refused "grammar: flag not allowed: rg -r"
}

@test "permission-check: a VAR= prefix goes to the human" {
  try "RIPGREP_CONFIG_PATH=./r rg x $F"
  assert_refused "grammar: command not allowed"
}

@test "permission-check: a * glob goes to the human" {
  try "cat $WT/*.md"
  assert_refused "lex: a character outside"
}

@test "permission-check: a ? glob goes to the human" {
  try "rg x $WT/?"
  assert_refused "lex: a character outside"
}

@test "permission-check: a bare cd goes to the human" {
  try "cd && cat x"
  assert_refused "grammar: cd takes exactly one operand"
}

@test "permission-check: cd - goes to the human" {
  try "cd - && cat x"
  assert_refused "grammar: the cd operand is not absolute"
}

@test "permission-check: cd ~ goes to the human" {
  try "cd ~ && cat x"
  assert_refused "lex: a character outside"
}

@test "permission-check: cd followed by ; goes to the human" {
  try "cd $WT ; cat README.md"
  assert_refused "grammar: cd must be followed by &&"
}

@test "permission-check: a ; after a leading cd goes to the human" {
  try "cd $WT && cat a ; cat README.md"
  assert_refused "grammar: ; after a leading cd"
}

@test "permission-check: rg -z goes to the human" {
  try "rg -z x $F"
  assert_refused "grammar: flag not allowed: rg -z"
}

@test "permission-check: rg --hostname-bin= goes to the human" {
  try "rg --hostname-bin=x p $F"
  assert_refused "grammar: flag not allowed"
}

@test "permission-check: rg -L goes to the human" {
  try "rg -L p $F"
  assert_refused "grammar: flag not allowed: rg -L"
}

@test "permission-check: find goes to the human" {
  try "find $WT"
  assert_refused "grammar: command not allowed: find"
}

@test "permission-check: diff goes to the human" {
  try "diff $F $F"
  assert_refused "grammar: command not allowed: diff"
}

@test "permission-check: a quoted '-r' is the flag -r and goes to the human" {
  try "rg '-r' x $F"
  assert_refused "grammar: flag not allowed: rg -r"
}

@test "permission-check: a flag joined across a quote goes to the human" {
  try "rg -'-pre=x' p $F"
  assert_refused "grammar: flag not allowed: rg --pre=x"
}

@test "permission-check: a quoted command word goes to the human" {
  try "c'at' $F"
  assert_refused "grammar: a quoted command word"
}

@test "permission-check: a redirect goes to the human" {
  try "cat $F > o"
  assert_refused "lex: a character outside"
}

@test "permission-check: a pipe to sh goes to the human" {
  try "cat $F | sh"
  assert_refused "grammar: command not allowed: sh"
}

@test "permission-check: || goes to the human" {
  try "cat $F || true"
  assert_refused "lex: || and |& are not allowed"
}

@test "permission-check: |& goes to the human" {
  try "cat $F |& wc -l"
  assert_refused "lex: || and |& are not allowed"
}

@test "permission-check: a lone & goes to the human" {
  try "cat $F &"
  assert_refused "lex: a lone &"
}

@test "permission-check: a command substitution goes to the human" {
  try "cat \$(echo $F)"
  assert_refused "lex: a character outside"
}

@test "permission-check: a double quote goes to the human" {
  try "cat \"$F\""
  assert_refused "lex: a character outside"
}

@test "permission-check: an unterminated single quote goes to the human" {
  try "cat '$F"
  assert_refused "lex: unterminated single quote"
}

@test "permission-check: a command path goes to the human" {
  try "/bin/cat $F"
  assert_refused "grammar: command not allowed"
}

@test "permission-check: an env wrapper goes to the human" {
  try "env cat $F"
  assert_refused "grammar: command not allowed: env"
}

@test "permission-check: a numeric argument joined to its flag goes to the human" {
  try "head -n5 $F"
  assert_refused "grammar: flag not allowed: head -n5"
}

@test "permission-check: bundled flags go to the human" {
  try "head -ni $F"
  assert_refused "grammar: flag not allowed: head -ni"
}

@test "permission-check: a non-numeric -n argument goes to the human" {
  try "head -n x $F"
  assert_refused "grammar: head -n needs a number"
}

@test "permission-check: a flag after an operand goes to the human" {
  try "cat $F -n"
  assert_refused "grammar: an operand starts with -"
}

@test "permission-check: - (stdin) after -- goes to the human" {
  try "grep -- foo -"
  assert_refused "grammar: - (stdin)"
}

@test "permission-check: rg with no path goes to the human" {
  try "rg p"
  assert_refused "grammar: the first pipeline stage names no file"
}

@test "permission-check: cat as a later pipeline stage goes to the human" {
  try "cat $F | cat"
  assert_refused "grammar: cat cannot be a later pipeline stage"
}

@test "permission-check: a file operand in a later pipeline stage goes to the human" {
  try "cat $F | head -n 1 $F"
  assert_refused "grammar: a file operand in a later pipeline stage"
}

@test "permission-check: grep -r as a later pipeline stage goes to the human" {
  try "cat $F | grep -r foo"
  assert_refused "grammar: grep -r in a later pipeline stage"
}

@test "permission-check: a trailing separator goes to the human" {
  try "cat $F ;"
  assert_refused "grammar: empty command"
}

@test "permission-check: a quoted separator is a word, not a separator" {
  try "cat $F '&&' cat $F"
  assert_refused "path: a relative operand without a leading cd: &&"
}

# ---------------------------------------------------------------------------
# Path rules
# ---------------------------------------------------------------------------

@test "permission-check: a relative operand without a leading cd goes to the human" {
  try "cat README.md"
  assert_refused "path: a relative operand without a leading cd"
}

@test "permission-check: a .. component in an absolute operand goes to the human" {
  try "cat $WT/docs/../README.md"
  assert_refused "path: a . or .. component"
}

@test "permission-check: a symlinked file goes to the human" {
  ln -s README.md "$WT/link"
  try "cat $WT/link"
  assert_refused "path: a symlink component"
}

@test "permission-check: a symlinked dir component goes to the human" {
  ln -s docs "$WT/ldir"
  try "cat $WT/ldir/x.md"
  assert_refused "path: a symlink component"
}

@test "permission-check: a FIFO goes to the human" {
  mkfifo "$WT/fifo"
  try "cat $WT/fifo"
  assert_refused "path: not a regular file"
}

@test "permission-check: a path outside every root goes to the human" {
  try "cat /etc/hostname"
  assert_refused "path: not under an allowed root"
}

@test "permission-check: a .env under a grant goes to the human" {
  try "cat $GRANT/.env"
  assert_refused "path: a secret"
}

@test "permission-check: a key file in the worktree goes to the human" {
  printf 'dummy\n' >"$WT/id_ed25519"
  try "cat $WT/id_ed25519"
  assert_refused "path: a secret"
}

@test "permission-check: a grant that is a symlink is dropped" {
  ln -s "$GRANT" "$BATS_TEST_TMPDIR/glink"
  printf '%s\n' "$BATS_TEST_TMPDIR/glink" >"$CREW/grants/$BRANCH"
  try "cat $BATS_TEST_TMPDIR/glink/notes.md"
  assert_refused "path: not under an allowed root"
}

@test "permission-check: a grant that is HOME is dropped" {
  export HOME="$GRANT"
  try "cat $GRANT/notes.md"
  assert_refused "path: not under an allowed root"
}

@test "permission-check: a WORKER_TASK.md add_dir: line grants nothing" {
  mkdir "$BATS_TEST_TMPDIR/other"
  printf 'other\n' >"$BATS_TEST_TMPDIR/other/f"
  printf 'add_dir: %s\n' "$BATS_TEST_TMPDIR/other" >"$WT/WORKER_TASK.md"
  try "cat $BATS_TEST_TMPDIR/other/f"
  assert_refused "path: not under an allowed root"
}

@test "permission-check: grep -r in the worktree goes to the human" {
  try "grep -r -n foo $WT"
  assert_refused "path: a directory walk outside an immutable root"
}

@test "permission-check: rg of a worktree dir goes to the human" {
  try "rg foo $WT/docs"
  assert_refused "path: a directory walk outside an immutable root"
}

@test "permission-check: cat of a dir goes to the human" {
  try "cat $WT/docs"
  assert_refused "path: not a regular file"
}

@test "permission-check: a hard-linked worktree file goes to the human" {
  ln "$WT/README.md" "$WT/hl"
  try "cat $WT/hl"
  assert_refused "path: a hard-linked file"
}

# ---------------------------------------------------------------------------
# Settle and --answer
# ---------------------------------------------------------------------------

# pane_ready — a pending cat of the readme and its matching frame.
pane_ready() {
  pending_bash "cat $WT/README.md"
  frame "cat $WT/README.md"
}

# alt_frame — the same dialog with one more line of output above it: a frame
# that still parses, but not byte-identical.
alt_frame() {
  export PANE_FRAME_ALT="$BATS_TEST_TMPDIR/frame-alt.txt"
  { printf 'earlier output\n'; cat "$FRAME"; } >"$PANE_FRAME_ALT"
}

@test "permission-check: pane mode with --answer sends exactly 1 after three captures" {
  pane_ready
  pane --answer
  assert_allowed
  mapfile -t log <"$TMUX_LOG"
  [ "${#log[@]}" -eq 6 ]
  for i in 0 2 4; do [ "${log[i]}" = "capture-pane -p -t %9" ]; done
  for i in 1 3; do [ "${log[i]}" = "display-message -p -t %9 #{pane_pid}" ]; done
  [ "${log[5]}" = "send-keys -t %9 1" ]
  [ "$(cat "$GIT_LOG")" = "-C $BATS_TEST_TMPDIR worktree list --porcelain -z" ]
}

@test "permission-check: pane mode without --answer allows and sends nothing" {
  pane_ready
  pane
  assert_allowed
  [ "$(grep -c '^capture-pane' "$TMUX_LOG")" -eq 2 ]
  no_send_keys
}

@test "permission-check: a frame that changes at the second capture goes to the human" {
  pane_ready
  alt_frame
  export PANE_FRAME_ALT_AT=2
  pane --answer
  assert_refused "settle: the frame changed"
  no_send_keys
}

@test "permission-check: a frame that changes at the third capture goes to the human" {
  pane_ready
  alt_frame
  export PANE_FRAME_ALT_AT=3
  pane --answer
  assert_refused "answer: the frame changed"
  no_send_keys
}

@test "permission-check: a second pending call appended during the settle goes to the human" {
  local late="$BATS_TEST_TMPDIR/late.jsonl" subs="$PROJ/$SID/subagents"
  jq -nc '{agentType:"shell-reviewer",toolUseId:"toolu_parent2",spawnDepth:1}' >"$subs/agent-a2.meta.json"
  tool_use toolu_late Bash "$late"
  export SLEEP_HOOK="cat $(printf %q "$late") >>$(printf %q "$subs/agent-a2.jsonl")"
  export PERMISSION_CHECK_SETTLE=2
  try "cat $WT/README.md"
  assert_refused "more than one pending call"
  [ "$(cat "$SLEEP_LOG")" = 2 ]
}

@test "permission-check: a pending call id that changes during the settle goes to the human" {
  pane_ready
  local next="$BATS_TEST_TMPDIR/next.jsonl"
  sed 's/toolu_live/toolu_next/g' "$SUB_LOG" >"$next"
  export SLEEP_HOOK="cp $(printf %q "$next") $(printf %q "$SUB_LOG")"
  pane --answer
  assert_refused "settle: the pending call changed"
  no_send_keys
}

@test "permission-check: a failing send-keys goes to the human" {
  pane_ready
  export SEND_KEYS_STATUS=1
  pane --answer
  assert_refused "answer: send-keys failed"
}

@test "permission-check: a refusal in pane mode never calls send-keys" {
  pane_ready
  export PANE_FRAME="$CLASSIFIER_FRAME"
  pane --answer
  assert_human
  [ "$(grep -c '^capture-pane' "$TMUX_LOG")" -eq 1 ]
  no_send_keys
}

@test "permission-check: PERMISSION_CHECK_SETTLE=0 in pane mode still settles 10" {
  pane_ready
  export PERMISSION_CHECK_SETTLE=0
  pane
  assert_allowed
  [ "$(cat "$SLEEP_LOG")" = 10 ]
}

@test "permission-check: a non-numeric PERMISSION_CHECK_SETTLE in pane mode settles 10" {
  pane_ready
  export PERMISSION_CHECK_SETTLE=5s
  pane
  assert_allowed
  [ "$(cat "$SLEEP_LOG")" = 10 ]
}

@test "permission-check: PERMISSION_CHECK_SETTLE=30 in pane mode settles 30" {
  pane_ready
  export PERMISSION_CHECK_SETTLE=30
  pane
  assert_allowed
  [ "$(cat "$SLEEP_LOG")" = 30 ]
}

@test "permission-check: capture mode settles 0 by default" {
  try "cat $WT/README.md"
  assert_allowed
  [ "$(cat "$SLEEP_LOG")" = 0 ]
}

# ---------------------------------------------------------------------------
# Pane binding
# ---------------------------------------------------------------------------

# assert_pane_refused — refused as not the lead session, without quoting the
# process args (they carry the launch prompt).
assert_pane_refused() {
  assert_refused "pane: not running the lead session"
  [[ "$output" != *opus* && "$output" != *prompt* ]]
  no_send_keys
}

@test "permission-check: a pane claude on another session goes to the human" {
  pane_ready
  ps_table "claude --name x --model opus --session-id 11111111-2222-4333-8444-555555555555 'prompt'"
  pane --answer
  assert_pane_refused
}

@test "permission-check: a pane claude resumed on the lead session is allowed" {
  pane_ready
  ps_table "claude --model opus --resume $SID 'prompt'"
  pane --answer
  assert_allowed
}

@test "permission-check: a pane with no claude process goes to the human" {
  pane_ready
  ps_table "node server.js --session-id $SID"
  pane --answer
  assert_pane_refused
}

@test "permission-check: a pane with a second claude on another session goes to the human" {
  pane_ready
  ps_table "claude --model opus --session-id $SID 'prompt'" \
    "claude --model opus --session-id 11111111-2222-4333-8444-555555555555 'prompt'"
  pane --answer
  assert_pane_refused
}

@test "permission-check: --session-id only as part of a longer word goes to the human" {
  pane_ready
  ps_table "claude --model opus --session-idX $SID 'prompt'"
  pane --answer
  assert_pane_refused
}

@test "permission-check: a session id only as part of a longer word goes to the human" {
  pane_ready
  ps_table "claude --model opus --session-id ${SID}0 'prompt'"
  pane --answer
  assert_pane_refused
}

@test "permission-check: a pane swapped to another session during the settle goes to the human" {
  pane_ready
  export SLEEP_HOOK="ps_table 'claude --session-id 11111111-2222-4333-8444-555555555555'"
  export -f ps_table
  export SID
  pane --answer
  assert_pane_refused
  [ "$(grep -c '^display-message' "$TMUX_LOG")" -eq 2 ]
}

@test "permission-check: a failing display-message goes to the human" {
  pane_ready
  export DISPLAY_STATUS=1
  pane --answer
  assert_refused "pane: pane_pid unavailable"
  no_send_keys
}

@test "permission-check: a non-numeric pane_pid goes to the human" {
  pane_ready
  export PANE_PID=abc
  pane --answer
  assert_refused "pane: pane_pid is not a number"
  no_send_keys
}

@test "permission-check: a branch with no worktree goes to the human" {
  pane_ready
  worktrees refs/heads/other
  pane --answer
  assert_refused "worktree: not exactly one worktree on the branch"
  no_send_keys
}

@test "permission-check: a worktree on a longer branch name does not match" {
  pane_ready
  worktrees "refs/heads/${BRANCH}y"
  pane --answer
  assert_refused "worktree: not exactly one worktree on the branch"
  no_send_keys
}

@test "permission-check: two worktrees claiming the branch go to the human" {
  pane_ready
  worktrees "refs/heads/$BRANCH" "refs/heads/$BRANCH"
  pane --answer
  assert_refused "worktree: not exactly one worktree on the branch"
  no_send_keys
}

@test "permission-check: pane mode finds the worktree with the real git of the cwd repo" {
  pane_ready
  git init -q -b "$BRANCH" "$WT"
  mv "$CREW" "$WT/.git/crew"
  rm "$STUBS/git"
  cd "$WT"
  run --separate-stderr env PATH="$STUBS:$PATH" HOME="$PANE_HOME" bash "$CHECK" --pane %9 \
    --branch "$BRANCH"
  assert_allowed
}

@test "permission-check: a crew dir outside any git repo goes to the human" {
  pane_ready
  rm "$STUBS/git"
  pane --answer
  assert_refused "worktree: git worktree list failed"
  no_send_keys
}

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------

@test "permission-check: --answer with --capture is a usage error" {
  frame "cat $WT/README.md"
  run --separate-stderr bash "$CHECK" --capture "$FRAME" --branch "$BRANCH" \
    --worktree "$WT" --crew-dir "$CREW" --answer
  [ "$status" -eq 2 ]
  [ "${#lines[@]}" -eq 1 ]
  [[ "$output" == "human: usage: "* ]]
}

@test "permission-check: --projects-dir with --pane is a usage error" {
  run --separate-stderr bash "$CHECK" --pane %9 --branch "$BRANCH" \
    --crew-dir "$CREW" --projects-dir "$PROJECTS"
  [ "$status" -eq 2 ]
  [ "${#lines[@]}" -eq 1 ]
  [[ "$output" == "human: usage: --projects-dir"* ]]
}

@test "permission-check: --pane '{last}' is a usage error" {
  run --separate-stderr bash "$CHECK" --pane '{last}' --branch "$BRANCH" --crew-dir "$CREW"
  [ "$status" -eq 2 ]
  [ "$output" = "human: usage: --pane must be a %id" ]
}

@test "permission-check: --pane '!' is a usage error" {
  run --separate-stderr bash "$CHECK" --pane '!' --branch "$BRANCH" --crew-dir "$CREW"
  [ "$status" -eq 2 ]
  [ "$output" = "human: usage: --pane must be a %id" ]
}

@test "permission-check: --pane 'sess:' is a usage error" {
  run --separate-stderr bash "$CHECK" --pane 'sess:' --branch "$BRANCH" --crew-dir "$CREW"
  [ "$status" -eq 2 ]
  [ "$output" = "human: usage: --pane must be a %id" ]
}

@test "permission-check: --pane '%9x' is a usage error" {
  run --separate-stderr bash "$CHECK" --pane '%9x' --branch "$BRANCH" --crew-dir "$CREW"
  [ "$status" -eq 2 ]
  [ "$output" = "human: usage: --pane must be a %id" ]
}

@test "permission-check: --worktree with --pane is a usage error" {
  run --separate-stderr bash "$CHECK" --pane %9 --branch "$BRANCH" \
    --worktree "$WT" --crew-dir "$CREW"
  [ "$status" -eq 2 ]
  [ "$output" = "human: usage: --worktree is accepted with --capture only" ]
}

@test "permission-check: --capture without --worktree is a usage error" {
  frame "cat $WT/README.md"
  run --separate-stderr bash "$CHECK" --capture "$FRAME" --branch "$BRANCH" --crew-dir "$CREW"
  [ "$status" -eq 2 ]
  [ "$output" = "human: usage: --worktree is required with --capture" ]
}

@test "permission-check: a non-numeric PERMISSION_CHECK_SETTLE in capture mode is a usage error" {
  frame "cat $WT/README.md"
  export PERMISSION_CHECK_SETTLE=x
  check
  [ "$status" -eq 2 ]
  [ "${#lines[@]}" -eq 1 ]
  [[ "$output" == "human: usage: "* ]]
}
