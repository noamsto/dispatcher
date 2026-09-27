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
# shellcheck disable=SC2034 # F_* are read by transcript_pending, still a stub
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

transcript_pending() {
  refuse "transcript binding not implemented"
}

grammar_check() {
  refuse "command grammar not implemented"
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
