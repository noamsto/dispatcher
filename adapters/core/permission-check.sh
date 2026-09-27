#!/usr/bin/env bash
# Dispatcher-side checker: may this Claude tool-permission dialog be answered
# "1. Yes" without the human? (#441)
#
#   permission-check --pane <%id> --branch <b> [--crew-dir <dir>] [--answer]
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
# With --pane the checker trusts no caller-given worktree: it takes the one
# worktree git lists for the branch, in the repo that owns the crew dir
# (<git-common-dir>/crew), and requires the pane to be running the lead's
# session.
#
# The decision takes two observations, PERMISSION_CHECK_SETTLE seconds apart:
# with --pane at least 10 (the env can only raise it; a value that is not one
# to six digits counts as unset), with --capture default 0 (a value that is not
# one to six digits is a usage error). --answer then re-captures and, only if the
# frame is unchanged, sends the one key `1`.
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

  # A relative tmux target is re-resolved by each tmux call, so the session
  # check, the frame and the keystroke could hit different panes.
  [[ -z $PANE || $PANE =~ ^%[0-9]+$ ]] || usage "--pane must be a %id"

  if [ -n "$PANE" ] && [ -n "$CAPTURE_FILE" ]; then
    usage "--pane and --capture are exclusive"
  fi
  [ -n "$PANE" ] || [ -n "$CAPTURE_FILE" ] || usage "one of --pane or --capture is required"
  [ -n "$BRANCH" ] || usage "--branch is required"
  if [ -n "$PANE" ]; then
    [ -z "$WORKTREE" ] || usage "--worktree is accepted with --capture only"
    [ "$projects_given" = 0 ] || usage "--projects-dir is accepted with --capture only"
    [ "${#RO_ROOTS[@]}" -eq 0 ] || usage "--ro-root is accepted with --capture only"
  else
    [ -n "$WORKTREE" ] || usage "--worktree is required with --capture"
    [ "$ANSWER" = 0 ] || usage "--answer is accepted with --pane only"
  fi
  if [ -z "$CREW_DIR" ]; then
    local common
    common=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) ||
      usage "--crew-dir not given and not inside a git repository"
    CREW_DIR="$common/crew"
  fi
  [ -n "$PROJECTS_DIR" ] || PROJECTS_DIR="$HOME/.claude/projects"

  local s=${PERMISSION_CHECK_SETTLE:-}
  if [ -n "$PANE" ]; then
    SETTLE=10
    if [[ $s =~ ^[0-9]{1,6}$ ]] && ((10#$s > SETTLE)); then SETTLE=$((10#$s)); fi
  elif [ -z "$s" ]; then
    SETTLE=0
  else
    [[ $s =~ ^[0-9]{1,6}$ ]] || usage "PERMISSION_CHECK_SETTLE is not one to six digits"
    SETTLE=$((10#$s))
  fi
}

# pane_worktree — the path of the one worktree `git worktree list` shows on
# refs/heads/<branch>, in the repo whose common dir holds the crew dir. -z so
# no path, however spelled, can forge a `branch` line. Sets WORKTREE.
pane_worktree() {
  local rec path="" n=0
  local -a recs
  mapfile -d '' -t recs < <(git -C "$(dirname -- "$CREW_DIR")" worktree list --porcelain -z)
  wait "$!" || refuse "worktree: git worktree list failed"
  for rec in "${recs[@]}"; do
    case $rec in
    "worktree "*) path=${rec#worktree } ;;
    "branch refs/heads/$BRANCH")
      n=$((n + 1))
      WORKTREE=$path
      ;;
    "") path="" ;;
    esac
  done
  [ "$n" -eq 1 ] || refuse "worktree: not exactly one worktree on the branch"
}

# pane_session — the pane runs the lead: below its pid there is a `claude`
# process, and every one there was started with `--session-id <SESSION>` or
# `--resume <SESSION>`, compared as whole words. The args hold the launch
# prompt, so no reason ever quotes them.
pane_session() {
  local pid table p pp rest q steps ok found=0
  local -A parent=() args=()
  local -a t
  pid=$(tmux display-message -p -t "$PANE" '#{pane_pid}') || refuse "pane: pane_pid unavailable"
  [[ $pid =~ ^[0-9]{1,10}$ ]] || refuse "pane: pane_pid is not a number"
  table=$(ps -ww -e -o pid=,ppid=,args=) || refuse "pane: process table unavailable"
  while read -r p pp rest; do
    [[ $p =~ ^[0-9]+$ && $pp =~ ^[0-9]+$ ]] || continue
    parent[$p]=$pp args[$p]=$rest
  done <<<"$table"

  for p in "${!args[@]}"; do
    q=$p steps=0
    while [ "$q" != "$pid" ] && [ -n "${parent[$q]:-}" ] && [ "$steps" -lt "${#parent[@]}" ]; do
      q=${parent[$q]} steps=$((steps + 1))
    done
    [ "$q" = "$pid" ] || continue
    read -ra t <<<"${args[$p]}"
    [ "${#t[@]}" -gt 0 ] && [ "${t[0]##*/}" = claude ] || continue
    found=1 ok=0
    for ((q = 1; q + 1 < ${#t[@]}; q++)); do
      if [[ ${t[q]} == --session-id || ${t[q]} == --resume ]] && [ "${t[q + 1]}" = "$SESSION" ]; then
        ok=1
      fi
    done
    [ "$ok" = 1 ] || refuse "pane: not running the lead session"
  done
  [ "$found" = 1 ] || refuse "pane: not running the lead session"
}

# The same stripping crew.sh's _permission_detail applies: OSC, CSI and other
# ESC sequences, then every control byte except tab and newline.
strip() {
  sed -E $'s/\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?//g; s/\x1b\\[[0-9;?]*[ -\\/]*[@-~]//g; s/\x1b[@-Z\\\\-_]//g' |
    tr -d '\000-\010\013-\037\177'
}

# capture — the plain, stripped frame text in CAPTURE, trailing newlines kept
# (the `.` sentinel) so observations compare byte for byte. Stripping happens
# in the pipe so a NUL never reaches a bash variable. --capture re-reads its
# file each time.
capture() {
  if [ -n "$PANE" ]; then
    CAPTURE=$(tmux capture-pane -p -t "$PANE" | strip && printf .) || refuse "capture of pane $PANE failed"
  else
    [ -f "$CAPTURE_FILE" ] && [ -r "$CAPTURE_FILE" ] || refuse "capture file unreadable"
    CAPTURE=$(strip <"$CAPTURE_FILE" && printf .) || refuse "capture file unreadable"
  fi
  CAPTURE=${CAPTURE%.}
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
  [[ ${ne[n - 3]} == "2. Yes, and don’t ask again for:"* ]] || refuse "frame: option 2 unrecognised"
  [ "${ne[n - 4]}" = "❯ 1. Yes" ] || refuse "frame: option 1 is not exactly Yes"
  [ "${ne[n - 5]}" = "Do you want to proceed?" ] || refuse "frame: question unrecognised"

  # A block line shaped like a header could shift the block boundary, so a
  # header match at both depths is refused rather than resolved.
  local header_re='^Bash command · from the ([A-Za-z0-9:_-]+) agent$' agent1="" agent2=""
  if [[ ${ne[n - 7]} =~ $header_re ]]; then agent1=${BASH_REMATCH[1]}; fi
  if [ "$n" -ge 8 ] && [[ ${ne[n - 8]} =~ $header_re ]]; then agent2=${BASH_REMATCH[1]}; fi
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

# branch_dirs_ok <base> <with-leaf> — every dir from <base> down to the
# branch's parent (or its leaf, when <with-leaf> is 1) is a dir, not a symlink.
branch_dirs_ok() {
  local dir=$1 part
  local -a parts
  IFS=/ read -ra parts <<<"$BRANCH"
  [ "$2" = 1 ] || parts=("${parts[@]:0:${#parts[@]}-1}")
  for part in "" "${parts[@]}"; do
    dir="$dir${part:+/$part}"
    [ -d "$dir" ] && [ ! -L "$dir" ] || return 1
  done
}

# lead_session — the lead's session id from <crew>/leads/<branch>, checked as
# dispatch.sh's _lead_record_safe checks it before writing. Sets SESSION.
lead_session() {
  case "/$BRANCH/" in
  *//* | */./* | */../*) refuse "lead record: unsafe branch name" ;;
  esac
  [[ $BRANCH != *[[:cntrl:]]* ]] || refuse "lead record: unsafe branch name"

  local rec="$CREW_DIR/leads/$BRANCH"
  branch_dirs_ok "$CREW_DIR/leads" 0 || refuse "lead record: a leads/ dir is a symlink or not a dir"
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
      elif [$pending[] | select(.i == 0)] | length != 1
        then refuse("transcript: the parent call is not pending")
      elif any($pending[]; .i == 0 and (.last | not))
        then refuse("transcript: the parent call is not the last tool_use in the lead transcript")
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

# lex — spec "Command grammar", characters and quoting: a hand-written scan of
# T_COMMAND, which bash itself never parses. Fills W (the de-quoted words), WQ
# (1 when a quote char appeared in the word) and WK (w for a word, s for a
# separator), so a quoted '&&' stays a word.
lex() {
  local s=$T_COMMAND n=${#T_COMMAND} i=0 c part w="" inword=0 q=0
  W=() WQ=() WK=()
  while [ "$i" -lt "$n" ]; do
    c=${s:i:1}
    case $c in
    "'")
      part=${s:i+1}
      [[ $part == *"'"* ]] || refuse "lex: unterminated single quote"
      part=${part%%"'"*}
      w+=$part inword=1 q=1
      i=$((i + ${#part} + 2))
      continue
      ;;
    ' ') lex_word ;;
    ';')
      lex_word
      lex_sep ';'
      ;;
    '|')
      case ${s:i+1:1} in
      '|' | '&') refuse "lex: || and |& are not allowed" ;;
      esac
      lex_word
      lex_sep '|'
      ;;
    '&')
      [ "${s:i+1:1}" = '&' ] || refuse "lex: a lone & is not allowed"
      lex_word
      lex_sep '&&'
      i=$((i + 1))
      ;;
    [A-Za-z0-9_./,:=@%+-]) w+=$c inword=1 ;;
    *) refuse "lex: a character outside the allowed set" ;;
    esac
    i=$((i + 1))
  done
  lex_word
}

# lex's helpers, on its locals: end the word in progress; append a separator.
lex_word() {
  [ "$inword" = 1 ] || return 0
  W+=("$w") WQ+=("$q") WK+=(w)
  w="" inword=0 q=0
}

lex_sep() {
  W+=("$1") WQ+=(0) WK+=(s)
}

# grammar_check — spec "Command grammar": the words split into simple commands
# at the separators, each checked against its command's flag table. Sets BASE
# (the leading cd's operand, or empty) and P_PATH/P_KIND (every path operand
# and the kind path_check holds it to).
grammar_check() {
  lex
  local -a starts=() ends=() seps=()
  local i s=0 n=${#W[@]} k prev next stage
  for ((i = 0; i <= n; i++)); do
    [ "$i" -eq "$n" ] || [ "${WK[i]}" = s ] || continue
    [ "$i" -gt "$s" ] || refuse "grammar: empty command"
    starts+=("$s") ends+=("$((i - 1))") seps+=("${W[i]:-}")
    s=$((i + 1))
  done

  BASE="" P_PATH=() P_KIND=()
  for k in "${!starts[@]}"; do
    prev=""
    [ "$k" -eq 0 ] || prev=${seps[k - 1]}
    next=${seps[k]}
    [ "${WQ[starts[k]]}" = 0 ] || refuse "grammar: a quoted command word"
    if [ "${W[starts[k]]}" = cd ]; then
      [ "$k" -eq 0 ] || refuse "grammar: cd only as the first command"
      [ "$next" = '&&' ] || refuse "grammar: cd must be followed by &&"
      [ "${ends[k]}" -eq "$((starts[k] + 1))" ] || refuse "grammar: cd takes exactly one operand"
      BASE=${W[ends[k]]}
      [[ $BASE == /* ]] || refuse "grammar: the cd operand is not absolute"
      P_PATH+=("$BASE") P_KIND+=(dir)
      continue
    fi
    # A failed cd skips its && chain but not what follows a `;`, which would
    # then resolve relative operands against an unknown cwd.
    [ -z "$BASE" ] || [ "$next" != ';' ] || refuse "grammar: ; after a leading cd"
    stage=first
    [ "$prev" != '|' ] || stage=later
    simple_command "${starts[k]}" "${ends[k]}" "$stage"
  done
}

# simple_command <first> <last> <first|later> — the command in W[first..last].
# Flags are checked de-quoted, each its own word. After the first operand, GNU
# getopt and rg still read a `-x` word as a flag; rather than model that
# permutation, a later word starting with `-` is refused unless it follows
# `--`. `-` (stdin) is never an operand.
simple_command() {
  local cmd=${W[$1]} i=$1 w kind flags_done=0 operand_seen=0 have_e=0 pattern=0 recursive=0
  local -a paths=()
  case $cmd in
  cat | head | tail | wc | grep | rg | ls) ;;
  *) refuse "grammar: command not allowed: $cmd" ;;
  esac

  while [ "$i" -lt "$2" ]; do
    i=$((i + 1))
    w=${W[i]}
    if [ "$flags_done" = 0 ] && [ "$operand_seen" = 0 ] && [[ $w == -?* ]]; then
      case "$cmd $w" in
      "grep --" | "rg --") flags_done=1 ;;
      "wc -l" | "wc -c" | "wc -w") ;;
      "ls -l" | "ls -a" | "ls -1" | "ls -d" | "ls -h") ;;
      "grep -n" | "grep -i" | "grep -l" | "grep -L" | "grep -c" | "grep -w" | "grep -F" | "grep -E" | "grep -H" | "grep -h" | "grep -s") ;;
      "rg -n" | "rg -i" | "rg -l" | "rg -c" | "rg -w" | "rg -F" | "rg -H" | "rg -s" | "rg --no-heading") ;;
      "grep -r") recursive=1 ;;
      "head -n" | "tail -n" | "grep -"[ABCm] | "rg -"[ABCm])
        [ "$i" -lt "$2" ] && [[ ${W[i + 1]} =~ ^[0-9]+$ ]] || refuse "grammar: $cmd $w needs a number"
        i=$((i + 1))
        ;;
      "grep -e" | "rg -e")
        [ "$i" -lt "$2" ] || refuse "grammar: $cmd -e needs a pattern"
        i=$((i + 1)) have_e=1
        ;;
      "rg -g" | "rg -t")
        [ "$i" -lt "$2" ] && [[ ${W[i + 1]} != -* ]] || refuse "grammar: rg $w needs a value"
        i=$((i + 1))
        ;;
      *) refuse "grammar: flag not allowed: $cmd $w" ;;
      esac
      continue
    fi
    [[ $w != -* ]] || [ "$flags_done" = 1 ] || refuse "grammar: an operand starts with -: $cmd $w"
    operand_seen=1
    if [[ $cmd == grep || $cmd == rg ]] && [ "$have_e" = 0 ] && [ "$pattern" = 0 ]; then
      pattern=1
      continue
    fi
    [ "$w" != - ] || refuse "grammar: - (stdin) is not an operand"
    paths+=("$w")
  done

  if [[ $cmd == grep || $cmd == rg ]] && [ "$have_e" = 0 ] && [ "$pattern" = 0 ]; then
    refuse "grammar: $cmd names no pattern"
  fi
  if [ "$3" = later ]; then
    case $cmd in
    cat | ls) refuse "grammar: $cmd cannot be a later pipeline stage" ;;
    esac
    [ "${#paths[@]}" -eq 0 ] || refuse "grammar: a file operand in a later pipeline stage"
    [ "$recursive" = 0 ] || refuse "grammar: grep -r in a later pipeline stage"
    return 0
  fi
  [ "${#paths[@]}" -gt 0 ] || refuse "grammar: the first pipeline stage names no file"

  case $cmd in
  grep) if [ "$recursive" = 1 ]; then kind=walk; else kind=file-or-walk; fi ;;
  rg) kind=file-or-walk ;;
  ls) kind=file-or-dir ;;
  *) kind="file" ;;
  esac
  for w in "${paths[@]}"; do
    P_PATH+=("$w") P_KIND+=("$kind")
  done
}

# lex_clean <path> — absolute, with no `.` or `..` component, no `//` and no
# trailing `/` (so `/` itself is not clean).
lex_clean() {
  [[ $1 == /* ]] || return 1
  case "$1/" in
  *//* | */./* | */../*) return 1 ;;
  esac
}

# secret_path <path> — spec "No secrets", matched case-blind: a
# case-insensitive filesystem (macOS) opens `.ENV` as `.env`.
secret_path() {
  local l=${1,,} part
  local -a parts
  case ${l##*/} in
  .env* | *.pem | *.key | id_* | *credentials* | *.netrc | *secret*) return 0 ;;
  esac
  IFS=/ read -ra parts <<<"$l"
  for part in "${parts[@]}"; do
    case $part in
    .ssh | .gnupg | .aws | .kube | .docker | .password-store | keyrings) return 0 ;;
    esac
  done
  return 1
}

# grant_ok <line> <canonical-home> — dispatch.sh's _add_dir_ok, plus: not a
# symlink and spelled cleanly. Prints the grant's canonical dir.
grant_ok() {
  local p h=$2 c s r
  [[ $1 != *[[:cntrl:]]* ]] && lex_clean "$1" || return 1
  [ -d "$1" ] && [ ! -L "$1" ] || return 1
  p=$(realpath -e -- "$1") || return 1
  [ "$p" != / ] || return 1
  [[ "$h/" != "$p/"* ]] || return 1
  c=$(realpath -m -- "$CREW_DIR")
  [[ "$p/" != "$c/"* && "$c/" != "$p/"* ]] || return 1
  for s in .ssh .gnupg .aws .config .claude .codex .kube .docker .password-store .local/share/keyrings; do
    for r in "$h/$s" "$(realpath -m -- "$h/$s")"; do
      [[ "$p/" != "$r/"* && "$r/" != "$p/"* ]] || return 1
    done
  done
  printf '%s\n' "$p"
}

# add_root <spelled> <canonical> <immutable> — a spelled form that is not clean
# matches as its canonical form only; a canonical form that is not clean (`/`,
# or empty after a failed realpath) drops the root.
add_root() {
  local spelled=$1
  lex_clean "$2" || return 0
  lex_clean "$spelled" || spelled=$2
  R_SPELLED+=("$spelled") R_CANON+=("$2") R_IMM+=("$3")
}

# roots — spec "Path rules", the allowed roots, each dropped when it fails its
# check: the worktree, the branch's artifacts dir, each grant, and the
# immutable dirs (the store dirs this build ships, and --ro-root in tests).
# WORKER_TASK.md is never read: the worker edits it.
roots() {
  local h c d line grants="$CREW_DIR/grants/$BRANCH"
  R_SPELLED=() R_CANON=() R_IMM=()
  h=$(realpath -e -- "$HOME") || refuse "path: HOME does not resolve"

  if [ -d "$WORKTREE" ] && [ ! -L "$WORKTREE" ] && c=$(realpath -e -- "$WORKTREE") &&
    [[ "$h/" != "$c/"* ]]; then
    add_root "$WORKTREE" "$c" 0
  fi

  d="$CREW_DIR/artifacts/$BRANCH"
  if branch_dirs_ok "$CREW_DIR/artifacts" 1 && c=$(realpath -e -- "$d"); then
    add_root "$d" "$c" 0
  fi

  if branch_dirs_ok "$CREW_DIR/grants" 0 && regular_file "$grants"; then
    while IFS= read -r line || [ -n "$line" ]; do
      [ -n "$line" ] || continue
      c=$(grant_ok "$line" "$h") || continue
      add_root "$line" "$c" 0
    done <"$grants"
  fi

  for d in "@protocolDir@" "@skillsDir@" "@reviewersDir@" "@criticsDir@"; do
    [[ $d != @* ]] && [ -d "$d" ] && c=$(realpath -e -- "$d") && [[ $c == /nix/store/?* ]] || continue
    add_root "$d" "$c" 1
  done
  for d in "${RO_ROOTS[@]}"; do
    if [ -d "$d" ] && c=$(realpath -e -- "$d"); then
      add_root "$d" "$c" 1
    fi
  done
}

# under_root <path> <remainder> <root-index> <kind> — <path> is <remainder>
# below that root: no symlink from the root's canonical form down, it resolves
# to exactly canonical root + remainder, and it is of <kind>. Sets FAIL when not.
under_root() {
  local canon=${R_CANON[$3]} kind=$4 q real part
  local -a parts
  q=$canon
  IFS=/ read -ra parts <<<"${2#/}"
  for part in "${parts[@]}"; do
    q=$q/$part
    if [ -L "$q" ]; then
      FAIL="path: a symlink component: $1"
      return 1
    fi
  done
  if ! real=$(realpath -e -- "$1" 2>/dev/null) || [ "$real" != "$canon$2" ]; then
    FAIL="path: does not resolve to itself: $1"
    return 1
  fi
  if secret_path "$real"; then
    FAIL="path: a secret: $1"
    return 1
  fi

  if [ "$kind" = file-or-walk ]; then
    kind="file"
    [ ! -d "$real" ] || kind=walk
  fi
  case $kind in
  file) [ -f "$real" ] || FAIL="path: not a regular file: $1" ;;
  dir) [ -d "$real" ] || FAIL="path: not a directory: $1" ;;
  file-or-dir) [ -f "$real" ] || [ -d "$real" ] || FAIL="path: not a regular file or directory: $1" ;;
  walk)
    if [ "${R_IMM[$3]}" != 1 ]; then
      FAIL="path: a directory walk outside an immutable root: $1"
    elif [ ! -f "$real" ] && [ ! -d "$real" ]; then
      FAIL="path: not a regular file or directory: $1"
    fi
    ;;
  esac

  # A hard link from a writable root to a file outside every root would pass
  # each rule above. Immutable roots are exempt: the Nix store hard-links
  # identical files.
  local links
  if [ -z "$FAIL" ] && [ "${R_IMM[$3]}" != 1 ] && [ -f "$real" ]; then
    if ! links=$(stat -c %h -- "$real") || [ "$links" != 1 ]; then
      FAIL="path: a hard-linked file: $1"
    fi
  fi
}

# path_check <operand> <kind> — spec "Path rules". <kind> is file, dir,
# file-or-dir, walk, or file-or-walk (a directory is a walk). Passes when the
# operand passes under_root for any root it lies under, as spelled or as
# canonicalised.
path_check() {
  local p=$1 j r rem reason="path: not under an allowed root: $1"
  if [[ $p != /* ]]; then
    [ -n "$BASE" ] || refuse "path: a relative operand without a leading cd: $1"
    p=$BASE/$p
  fi
  lex_clean "$p" || refuse "path: a . or .. component, // or a trailing /: $1"
  if secret_path "$p"; then refuse "path: a secret: $1"; fi

  for j in "${!R_CANON[@]}"; do
    for r in "${R_SPELLED[j]}" "${R_CANON[j]}"; do
      if [ "$p" = "$r" ]; then
        rem=""
      elif [[ $p == "$r"/* ]]; then
        rem=${p#"$r"}
      else
        continue
      fi
      FAIL=""
      under_root "$p" "$rem" "$j" "$2" && [ -z "$FAIL" ] && return 0
      reason=$FAIL
    done
  done
  refuse "$reason"
}

path_checks() {
  local i
  roots
  for i in "${!P_PATH[@]}"; do
    path_check "${P_PATH[i]}" "${P_KIND[i]}"
  done
}

# decide — one observation: the frame, the call it binds to, and that call's
# command. Any failed rule refuses, which exits: it runs in the main shell, so
# a refusal at either observation is the one stdout line.
decide() {
  capture
  frame_parse
  transcript_pending
  [ -z "$PANE" ] || pane_session
  grammar_check
  path_checks
}

verdict() {
  decided=1
  printf 'allow-once\n'
}

# main — spec "Settle": the second observation must see the same frame bytes
# and the same pending call, and pass on its own. A live call not yet flushed
# at the first scan shows up at the second as a second pending call.
main() {
  local frame id
  parse_args "$@"
  [ -z "$PANE" ] || pane_worktree
  decide
  frame=$CAPTURE id=$T_ID
  sleep "$SETTLE"
  decide
  [ "$CAPTURE" = "$frame" ] || refuse "settle: the frame changed"
  [ "$T_ID" = "$id" ] || refuse "settle: the pending call changed"

  if [ "$ANSWER" = 1 ]; then
    capture
    [ "$CAPTURE" = "$frame" ] || refuse "answer: the frame changed before the keystroke"
    # `1` picks option 1 whatever the cursor row. Nothing else is ever sent.
    tmux send-keys -t "$PANE" 1 >&2 || refuse "answer: send-keys failed"
  fi
  verdict
}

main "$@"
