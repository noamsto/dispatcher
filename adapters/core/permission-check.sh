#!/usr/bin/env bash
# Dispatcher-side checker: may this Claude tool-permission dialog be answered
# "1. Yes" without the human? (#441)
#
#   permission-check --pane <%id> --branch <b> --worktree <dir> [--crew-dir <dir>] [--answer]
#   permission-check --capture <file> --branch <b> --worktree <dir> [--crew-dir <dir>]
#                    [--projects-dir <dir>] [--ro-root <dir>]...
#
# The pane is attacker-influenceable text: a worker can print anything,
# including something shaped like a dialog. So the pane alone never decides.
# The frame must match the one dialog shape we have captured, bind to the single
# pending Bash call in the worker's own session transcript, and that command
# must be a read of an allowed root in a grammar small enough to check
# completely. See docs/superpowers/specs/2026-09-27-permission-auto-approve-design.md.
#
# Fail closed: stdout carries exactly one line — `allow-once` (exit 0) or
# `human: <reason>` (exit 1); usage errors are `human: usage: …` (exit 2).
# Any unexpected failure also ends as a `human:` line, never as an allow.
# Diagnostics, if any, go to stderr.

set -euo pipefail

# Byte-wise matching: the frame's `·`, `❯` and `’` are compared as UTF-8 bytes,
# and no locale may widen a bracket range.
export LC_ALL=C

decided=0
trap '[ "$decided" = 1 ] || { printf "human: internal error\n"; exit 1; }' EXIT

usage() {
  decided=1
  printf 'human: usage: %s\n' "$1"
  exit 2
}

refuse() {
  decided=1
  printf 'human: %s\n' "$1"
  exit 1
}

command -v jq >/dev/null || usage "jq not found"

PANE="" CAPTURE_FILE="" BRANCH="" WORKTREE="" CREW_DIR="" PROJECTS_DIR=""
RO_ROOTS=()
ANSWER=0

parse_args() {
  local projects_given=0
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --answer)
      ANSWER=1
      shift
      continue
      ;;
    --pane | --capture | --branch | --worktree | --crew-dir | --projects-dir | --ro-root)
      [ "$#" -ge 2 ] && [ -n "$2" ] || usage "$1 needs a value"
      ;;
    *) usage "unknown argument" ;;
    esac
    case "$1" in
    --pane)
      [ -z "$PANE" ] || usage "--pane given twice"
      PANE=$2
      ;;
    --capture)
      [ -z "$CAPTURE_FILE" ] || usage "--capture given twice"
      CAPTURE_FILE=$2
      ;;
    --branch) BRANCH=$2 ;;
    --worktree) WORKTREE=$2 ;;
    --crew-dir) CREW_DIR=$2 ;;
    --projects-dir)
      PROJECTS_DIR=$2
      projects_given=1
      ;;
    --ro-root) RO_ROOTS+=("$2") ;;
    esac
    shift 2
  done

  if [ -n "$PANE" ] && [ -n "$CAPTURE_FILE" ]; then
    usage "--pane and --capture are exclusive"
  fi
  [ -n "$PANE" ] || [ -n "$CAPTURE_FILE" ] || usage "one of --pane or --capture is required"
  [ -n "$BRANCH" ] || usage "--branch is required"
  [ -n "$WORKTREE" ] || usage "--worktree is required"
  if [ -n "$PANE" ]; then
    [ "$projects_given" = 0 ] || usage "--projects-dir is accepted with --capture only"
    [ "${#RO_ROOTS[@]}" -eq 0 ] || usage "--ro-root is accepted with --capture only"
  else
    [ "$ANSWER" = 0 ] || usage "--answer is accepted with --pane only"
  fi
  if [ -z "$CREW_DIR" ]; then
    local common
    common=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) ||
      usage "--crew-dir not given and not inside a git repository"
    CREW_DIR="$common/crew"
  fi
  [ -n "$PROJECTS_DIR" ] || PROJECTS_DIR="$HOME/.claude/projects"
}

# The same stripping crew.sh's _permission_detail applies: OSC, CSI and other
# ESC sequences, then every control byte except tab and newline.
strip() {
  sed -E $'s/\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?//g; s/\x1b\\[[0-9;?]*[ -\\/]*[@-~]//g; s/\x1b[@-Z\\\\-_]//g' |
    tr -d '\000-\010\013-\037\177'
}

# capture — the plain, stripped frame text in CAPTURE. Stripping happens in the
# pipe so a NUL never reaches a bash variable.
capture() {
  if [ -n "$PANE" ]; then
    CAPTURE=$(tmux capture-pane -p -t "$PANE" | strip) || refuse "capture of pane $PANE failed"
  else
    [ -f "$CAPTURE_FILE" ] && [ -r "$CAPTURE_FILE" ] || refuse "capture file unreadable"
    CAPTURE=$(strip <"$CAPTURE_FILE") || refuse "capture file unreadable"
  fi
}

# frame_parse — spec "Frame gate". Walks the non-empty lines up from the
# bottom: footer, the three options, the question, a one- or two-line request
# block, the subagent Bash header. Sets F_AGENT, F_BLOCK1, F_BLOCK2, F_NBLOCK.
frame_parse() {
  local lines=() ne=() line t
  mapfile -t lines <<<"$CAPTURE"
  for line in "${lines[@]}"; do
    t=${line#"${line%%[![:space:]]*}"}
    t=${t%"${t##*[![:space:]]}"}
    [ -z "$t" ] || ne+=("$t")
  done

  local n=${#ne[@]}
  [ "$n" -ge 7 ] || refuse "frame: not a permission dialog"
  [ "${ne[n - 1]}" = "Esc to cancel · Tab to amend" ] || refuse "frame: footer is not the last line"
  [ "${ne[n - 2]}" = "3. No" ] || refuse "frame: option 3 is not No"
  [[ "${ne[n - 3]}" == "2. Yes, and don’t ask again for:"* ]] || refuse "frame: option 2 unrecognised"
  [ "${ne[n - 4]}" = "❯ 1. Yes" ] || refuse "frame: option 1 is not exactly Yes"
  [ "${ne[n - 5]}" = "Do you want to proceed?" ] || refuse "frame: question unrecognised"

  # A block line shaped like a header could shift the block boundary, so a
  # header match at both depths is refused rather than resolved.
  local header_re='^Bash command · from the ([A-Za-z0-9:_-]+) agent$' agent1="" agent2=""
  if [[ "${ne[n - 7]}" =~ $header_re ]]; then agent1=${BASH_REMATCH[1]}; fi
  if [ "$n" -ge 8 ] && [[ "${ne[n - 8]}" =~ $header_re ]]; then agent2=${BASH_REMATCH[1]}; fi
  if [ -n "$agent1" ] && [ -n "$agent2" ]; then
    refuse "frame: ambiguous header"
  elif [ -n "$agent1" ]; then
    F_AGENT=$agent1 F_NBLOCK=1 F_BLOCK1=${ne[n - 6]} F_BLOCK2=""
  elif [ -n "$agent2" ]; then
    F_AGENT=$agent2 F_NBLOCK=2 F_BLOCK1=${ne[n - 7]} F_BLOCK2=${ne[n - 6]}
  else
    refuse "frame: no subagent Bash header above a one- or two-line request"
  fi
}

regular_file() {
  [ -f "$1" ] && [ ! -L "$1" ]
}

# lead_session — the lead's session id from <crew>/leads/<branch>, checked as
# dispatch.sh's _lead_record_safe checks it before writing. Sets SESSION.
lead_session() {
  case "/$BRANCH/" in
  *//* | */./* | */../*) refuse "lead record: unsafe branch name" ;;
  esac
  [[ "$BRANCH" != *[[:cntrl:]]* ]] || refuse "lead record: unsafe branch name"

  local dir="$CREW_DIR/leads" rec="$CREW_DIR/leads/$BRANCH" part
  local -a parts
  IFS=/ read -ra parts <<<"$BRANCH"
  for part in "" "${parts[@]:0:${#parts[@]}-1}"; do
    dir="$dir${part:+/$part}"
    [ -d "$dir" ] && [ ! -L "$dir" ] || refuse "lead record: a leads/ dir is a symlink or not a dir"
  done
  regular_file "$rec" || refuse "lead record: missing, a symlink or not a regular file"

  # The whole file, byte for byte, as dispatch writes it.
  SESSION=$(jq -Rrse 'if test("\\Aclaude [0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\n\\z")
    then .[7:43] else false end' <"$rec") || refuse "lead record: not claude <uuid>"
}

# meta_json <agent-x.jsonl> — its agent-x.meta.json as one compact object, or
# null when it is missing, a symlink, not a regular file or not one object.
meta_json() {
  local m=${1%.jsonl}.meta.json
  if regular_file "$m"; then
    jq -sc 'if length == 1 and (.[0] | type) == "object" then .[0] else null end' <"$m" 2>/dev/null ||
      printf 'null\n'
  else
    printf 'null\n'
  fi
}

# Input: one {meta, entries} per transcript file, the lead's first; `bad` marks
# a file with a line that is not exactly one JSON object.
# Output: {refuse: <reason>} or {id, command}.
# shellcheck disable=SC2016 # a jq program, not a shell expansion
BINDING_JQ='
def trim_sp: until(startswith(" ") | not; .[1:]) | until(endswith(" ") | not; .[:-1]);
def printable: explode | all(. >= 32 and . <= 126);
def result_text:
  .content
  | if type == "string" then .
    elif type == "array" then [.[] | objects | select(.type == "text") | .text | strings] | join("\n")
    else "" end;
def refuse($r): {refuse: $r};

if any(.[]; .bad) then refuse("transcript: a line is not one JSON object") else

[to_entries[] | .key as $i | .value as $f
  | [$f.entries[] as $e | $e.message | objects | .content | arrays | .[] | objects | {b: ., e: $e}] as $bl
  | {i: $i, meta: $f.meta,
     uses: [$bl[] | select(.b.type == "tool_use")],
     results: [$bl[] | select(.b.type == "tool_result") | .b]}] as $files

| [$files[] as $f
    | ($f.results | map(.tool_use_id | strings | {key: ., value: true}) | from_entries) as $done
    | $f.uses | to_entries[] | .key as $k | .value as $u
    | select(($u.b.id | type) == "string" and ($done | has($u.b.id)) | not)
    | {i: $f.i, last: ($k == ($f.uses | length) - 1), id: $u.b.id, name: $u.b.name,
       input: $u.b.input, entry: $u.e}] as $pending

| [$pending[] | select(.i > 0)] as $cands
| if ($cands | length) == 0 then refuse("transcript: no pending subagent call")
  elif ($cands | length) > 1 then refuse("transcript: more than one pending call")
  else
    $cands[0] as $c | $files[$c.i].meta as $m | $c.input as $in
    | if ($m | type) != "object" then refuse("transcript: subagent meta missing or invalid")
      elif any($pending[]; .i == 0
          and ((.name == "Agent" or .name == "Task") and ($m.toolUseId | type) == "string"
               and .id == $m.toolUseId | not))
        then refuse("transcript: more than one pending call")
      elif $m.spawnDepth != 1 then refuse("transcript: subagent spawnDepth is not 1")
      elif $m.agentType != $agent then refuse("transcript: agentType does not match the header")
      elif ($c.id | type) != "string" then refuse("transcript: pending call has no id")
      elif ($c.last | not) then refuse("transcript: pending call is not the last tool_use in its file")
      elif $c.name != "Bash" then refuse("transcript: pending call is not Bash")
      elif ($in | type) != "object" or (($in | keys) - ["command", "description"]) != []
        or ($in.command | type) != "string"
        or (($in | has("description")) and ($in.description | type) != "string")
        then refuse("transcript: input is not command plus optional description")
      elif ($c.entry | has("wireToolInputs"))
        and (($c.entry.wireToolInputs | type) != "object"
             or (($c.entry.wireToolInputs | has($c.id)) and $c.entry.wireToolInputs[$c.id] != $in))
        then refuse("transcript: wireToolInputs differs from input")
      elif ($in.command | printable | not)
        or (($in | has("description")) and ($in.description | printable | not))
        then refuse("transcript: command or description is not printable ASCII")
      elif ($in.command | trim_sp) as $cmd | ($in.description // "" | trim_sp) as $desc
        | if $nblock == "1" then $desc != "" or $block1 != $cmd
          else $block1 != $cmd or $block2 != $desc end
        then refuse("transcript: request block does not match the pending call")
      elif any($files[$c.i].results[-5:][]; result_text | contains("denied by the Claude Code auto mode classifier"))
        then refuse("transcript: recent classifier denial")
      else {id: $c.id, command: $in.command}
      end
  end
end
'

# transcript_pending — spec "Transcript binding": the dialog must be the single
# pending Bash call of a depth-1 subagent in the lead's own session. Sets T_ID
# and T_COMMAND (the exact command bytes).
# shellcheck disable=SC2034 # T_* are read by grammar_check, still a stub
transcript_pending() {
  lead_session

  local canon proj lead_log f meta res reason
  canon=$(realpath -e -- "$WORKTREE") || refuse "worktree does not resolve"
  proj="$PROJECTS_DIR/${canon//[^A-Za-z0-9]/-}"
  lead_log="$proj/$SESSION.jsonl"
  regular_file "$lead_log" || refuse "transcript: lead transcript missing, a symlink or not a regular file"

  local -a files=("$lead_log") subs
  shopt -s nullglob
  subs=("$proj/$SESSION/subagents"/agent-*.jsonl)
  shopt -u nullglob
  for f in "${subs[@]}"; do
    regular_file "$f" || refuse "transcript: a subagent transcript is a symlink or not a regular file"
    files+=("$f")
  done

  # One read per file: lines are parsed one by one, so a line holding two
  # objects, a fragment or a non-object is caught, not re-framed by the parser.
  res=$(
    for f in "${files[@]}"; do
      meta=null
      [ "$f" = "$lead_log" ] || meta=$(meta_json "$f")
      jq -nRc --argjson meta "$meta" \
        '[inputs | try fromjson catch null] as $l
         | if all($l[]; type == "object") then {meta: $meta, entries: $l} else {bad: true} end' \
        <"$f" || exit 1
    done | jq -sc --arg agent "$F_AGENT" --arg nblock "$F_NBLOCK" \
      --arg block1 "$F_BLOCK1" --arg block2 "$F_BLOCK2" "$BINDING_JQ"
  ) || refuse "transcript: unreadable"

  reason=$(jq -r '.refuse // empty' <<<"$res")
  [ -z "$reason" ] || refuse "$reason"
  T_ID=$(jq -j '.id' <<<"$res")
  T_COMMAND=$(jq -j '.command' <<<"$res")
}

grammar_check() {
  refuse "grammar not implemented"
}

path_checks() {
  refuse "path rules not implemented"
}

verdict() {
  decided=1
  printf 'allow-once\n'
}

# shellcheck disable=SC2317 # stages after the stub stay unreachable until built
main() {
  parse_args "$@"
  capture
  frame_parse
  transcript_pending
  grammar_check
  path_checks
  verdict
}

main "$@"
