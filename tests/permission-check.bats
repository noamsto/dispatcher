bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# adapters/core/permission-check.sh: the dispatcher's auto-approve checker for
# Claude permission dialogs. See the #441 spec and plan.

FIXTURE_DIR="$BATS_TEST_DIRNAME/fixtures/permission"
CLASSIFIER_FRAME="$FIXTURE_DIR/classifier-escalation.txt"

# A world the checker binds a dialog against: a lead record, artifacts and
# grants for a slashed branch, a worktree, a grant dir, an immutable root, and
# the lead's transcript with one foreground subagent whose parent `Agent` call
# is pending while it runs.
setup() {
  load helpers
  CHECK="$BATS_TEST_DIRNAME/../adapters/core/permission-check.sh"
  BRANCH=feat/441-x
  SID=3f2a9c1e-5b7d-4e8f-9a0b-1c2d3e4f5a6b
  CREW="$BATS_TEST_TMPDIR/crew"
  WT="$BATS_TEST_TMPDIR/wt"
  GRANT="$BATS_TEST_TMPDIR/grant"
  RO="$BATS_TEST_TMPDIR/ro"
  PROJECTS="$BATS_TEST_TMPDIR/projects"
  FRAME="$BATS_TEST_TMPDIR/frame.txt"
  F="$WT/README.md"

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

# result_for <id> [text] — a tool_result for <id> lands in the subagent file.
result_for() {
  jq -nc --arg sid "$SID" --arg id "$1" --arg text "${2:-ok}" \
    '{type:"user",sessionId:$sid,agentId:"a1",isSidechain:true,uuid:"s2",
      message:{role:"user",content:[{type:"tool_result",tool_use_id:$id,content:$text}]}}' \
    >>"$SUB_LOG"
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
  run --separate-stderr bash "$CHECK" --capture "${1:-$FRAME}" \
    --branch "$BRANCH" --worktree "$WT" --crew-dir "$CREW" \
    --projects-dir "$PROJECTS" --ro-root "$RO"
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

DENIAL="Permission for this action has been denied by the Claude Code auto mode classifier."

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
    --worktree "$WT" --crew-dir "$CREW" --projects-dir "$PROJECTS"
  [ "$status" -eq 2 ]
  [ "${#lines[@]}" -eq 1 ]
  [[ "$output" == "human: usage: "* ]]
}
