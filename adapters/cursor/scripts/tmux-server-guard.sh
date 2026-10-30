#!/usr/bin/env bash
# Pre-tool guard: stop a dispatched worker from running a tmux command that
# reaches the user's live tmux server. A worker's panes inherit $TMUX, which
# tmux resolves before $TMUX_TMPDIR, so a "scratch" server built with
# TMUX_TMPDIR alone is not isolated: a bare `tmux kill-server` or `tmux set -g`
# at the end of a probe hits the live server and every worker in it (#1332).
#
# Workers only: a session without CREW_WORKER_ID (a human at a prompt, a
# dispatcher) exits before jq and the parse, so the hook costs a human
# nothing. Role panes inherit the lead's CREW_WORKER_ID, so they are guarded
# too.
#
# Denied, per tmux invocation with no -L/-S of its own: kill-server,
# kill-session, source-file/source, set-option/set-window-option/set-hook (and
# their short forms) with a -g flag, and set-option/set with -s. Everything
# else passes: read-only calls and window- or pane-scoped ones (rename-window,
# kill-window -t @N, set -w, set -p, select-pane).
#
#   engine        event                  command at             verdict shape
#   claude        PreToolUse Bash        tool_input.command     hookSpecificOutput deny
#   pi(hookyard)  canonical_event        tool_input.command     hookSpecificOutput deny
#                 pre_tool Bash
#
# Allow is no stdout, exit 0; a deny's reason is handed back to the agent, so
# it retries against a private server.
#
# A speed bump over the command string, not a sandbox. Not classified, and so
# not caught: a command word built from a variable (`$T kill-server`,
# `T=tmux; $T kill-server`); a quoted payload such as `bash -c 'tmux
# kill-server'`, or tmux's own argument strings (`tmux new-session "tmux
# kill-server"`, `tmux new -d \; kill-server`); a unique-prefix abbreviation
# (`kill-serv`); `xargs tmux kill-server`; a long wrapper option with a separate
# value (`env --unset TMUX tmux kill-server`). Here-doc bodies are read as
# commands, so a body line starting with `tmux kill-server` is denied, and an
# escaped quote inside a double-quoted string ends the quote early (also a deny).
#
# Portable to macOS's bash 3.2 and BSD userland: no mapfile, no ${var,,}.

set -euo pipefail

# Workers only, ahead of everything else.
[[ -n ${CREW_WORKER_ID:-} ]] || exit 0

command -v jq >/dev/null || {
  echo "tmux-server-guard: jq not found; guard NOT enforcing" >&2
  exit 1
}

input=$(cat)

# shellcheck disable=SC2016
normalise='
def s: if type == "string" then . else "" end;
if type != "object" then empty else
(.tool_input | if type == "object" then . else {} end) as $ti
| (.hook_event_name | s) as $ev
| (.tool_name | s) as $tool
| (if (.canonical_event | type) == "string" then "hookyard"
   elif $ev == "PreToolUse" and has("turn_id") then "codex"
   elif $ev == "PreToolUse" then "claude"
   elif ($ev == "preToolUse" or $ev == "beforeShellExecution") then "cursor"
   else "" end) as $shape
| (if $shape == "cursor" and $ev == "beforeShellExecution" then .command | s
   elif $shape == "cursor" and $tool == "Shell" then $ti.command | s
   elif $shape != "" and $shape != "cursor" and $tool == "Bash" then $ti.command | s
   else "" end) as $command
| select($command != "")
| [$shape, $command]
end'

parsed=$(jq -c "$normalise" <<<"$input" 2>/dev/null) || {
  echo "tmux-server-guard: could not parse hook payload; guard NOT enforcing" >&2
  exit 1
}
[[ -n $parsed ]] || exit 0
shape=$(jq -r '.[0]' <<<"$parsed")
command=$(jq -r '.[1]' <<<"$parsed")

# Quote-aware split of the command line: one word per line, an empty line
# wherever a shell control operator (`; && || | ( )`) ends a command, and
# $nl_mark wherever a newline ends one — a harder boundary, since the next line
# is a separate command. Quotes are stripped, never interpreted — the guard
# never evaluates the command it is checking.
nl_mark=$'\001'

split_words() { # <command>
  local s=$1 i=0 n=${#1} c d q w=''
  while ((i < n)); do
    c=${s:i:1}
    case $c in
    ' ' | $'\t')
      if [[ -n $w ]]; then
        printf '%s\n' "$w"
        w=''
      fi
      ;;
    ';' | '|' | '&' | '(' | ')' | '`')
      # The empty line is what puts the next word in command position: without
      # it, a `bats tests/` on line 2 is still an argument of line 1's command.
      if [[ -n $w ]]; then
        printf '%s\n' "$w"
        w=''
      fi
      printf '\n'
      ;;
    $'\n')
      if [[ -n $w ]]; then
        printf '%s\n' "$w"
        w=''
      fi
      printf '%s\n' "$nl_mark"
      ;;
    "'" | '"')
      q=$c
      i=$((i + 1))
      while ((i < n)); do
        d=${s:i:1}
        i=$((i + 1))
        if [[ $d == "$q" ]]; then break; fi
        w+=$d
      done
      continue
      ;;
    \\)
      i=$((i + 1))
      # A backslash-newline is a line continuation: it joins, adds nothing.
      if [[ ${s:i:1} != $'\n' ]] && ((i < n)); then w+=${s:i:1}; fi
      i=$((i + 1))
      continue
      ;;
    '#')
      # A word-initial `#` comments out the rest of the line; mid-word it is
      # an ordinary character.
      if [[ -z $w ]]; then
        while ((i < n)); do
          if [[ ${s:i:1} == $'\n' ]]; then break; fi
          i=$((i + 1))
        done
        continue
      fi
      w+=$c
      ;;
    '<' | '>')
      # A redirection and its target are never operands: `2>&1`, `> log`,
      # `< /dev/null`, `<<EOF`. A bare fd number glued to the operator goes
      # with it; anything else before the operator was a word.
      if [[ -n $w ]]; then
        if [[ $w == *[0-9] && $w != *[!0-9]* ]]; then
          w=''
        else
          printf '%s\n' "$w"
        fi
      fi
      i=$((i + 1))
      if [[ ${s:i:1} == "$c" ]]; then i=$((i + 1)); fi
      while ((i < n)); do
        if [[ ${s:i:1} != ' ' && ${s:i:1} != $'\t' ]]; then break; fi
        i=$((i + 1))
      done
      if [[ ${s:i:1} == '&' ]]; then i=$((i + 1)); fi
      # The target is one word, quotes and escapes included: `> "/tmp/o f"`,
      # `2> 'error log'`, `> /tmp/o\ f`.
      while ((i < n)); do
        d=${s:i:1}
        case $d in
        ' ' | $'\t' | $'\n' | ';' | '|' | '&' | '(' | ')' | '<' | '>' | '`') break ;;
        "'" | '"')
          q=$d
          i=$((i + 1))
          while ((i < n)); do
            d=${s:i:1}
            i=$((i + 1))
            if [[ $d == "$q" ]]; then break; fi
          done
          continue
          ;;
        \\)
          i=$((i + 1))
          ;;
        esac
        i=$((i + 1))
      done
      continue
      ;;
    *) w+=$c ;;
    esac
    i=$((i + 1))
  done
  if [[ -n $w ]]; then printf '%s\n' "$w"; fi
  printf '\n'
}

# Wrapper options whose value is the NEXT word, so it is never the command
# (`env -u TMUX tmux`, `sudo -u me tmux`). Entries are `<wrapper>:<letter>`.
wrapper_value_table=' env:a exec:a env:C env:u nice:n timeout:k timeout:s '
wrapper_value_table+='sudo:C sudo:g sudo:h sudo:p sudo:r sudo:t sudo:T sudo:U sudo:u '

# tmux global options that take a value.
tmux_value_letters=LSfcT

# Judge one invocation; $@ is everything after the `tmux` word. Sets
# $deny_what to the offending subcommand when it has no -L/-S and reaches the
# server.
classify_tmux() {
  local w rest c sock=0 sub='' skip=0
  local g_re='^-[A-Za-z]*g[A-Za-z]*$' s_re='^-[A-Za-z]*s[A-Za-z]*$'
  deny_what=''
  for w in "$@"; do
    if ((skip)); then
      skip=0
      continue
    fi
    if [[ -n $sub ]]; then
      case $sub in
      set-option | set | set-window-option | setw | set-hook)
        if [[ $w =~ $g_re ]]; then deny_what="$sub -g"; fi
        ;;
      esac
      case $sub in
      set-option | set)
        if [[ $w =~ $s_re ]]; then deny_what="$sub -s"; fi
        ;;
      esac
      continue
    fi
    if [[ $w != -* ]]; then
      sub=$w
      continue
    fi
    # A cluster is read as getopt does: the first letter that takes a value
    # owns the rest of the word, or the next word when it is the last letter.
    rest=${w#-}
    while [[ -n $rest ]]; do
      c=${rest:0:1}
      rest=${rest:1}
      case $c in
      L | S) sock=1 ;;
      esac
      case $tmux_value_letters in
      *"$c"*)
        if [[ -z $rest ]]; then skip=1; fi
        break
        ;;
      esac
    done
  done
  if ((sock)); then
    deny_what=''
    return 0
  fi
  case $sub in
  kill-server | kill-session | source-file | source) deny_what=$sub ;;
  esac
}

at_cmd=1
expect_duration=0
wrapper=''
wrapper_val=0
in_tmux=0
targs=()
deny_what=''

# Finish the tmux invocation being collected, if any.
flush_tmux() {
  if ((in_tmux)); then
    classify_tmux ${targs[@]+"${targs[@]}"}
    in_tmux=0
    targs=()
  fi
}

while IFS= read -r w; do
  if [[ -z $w || $w == "$nl_mark" ]]; then
    flush_tmux
    [[ -z $deny_what ]] || break
    at_cmd=1
    wrapper=''
    wrapper_val=0
    expect_duration=0
    continue
  fi

  if ((in_tmux)); then
    targs+=("$w")
    continue
  fi
  ((at_cmd)) || continue

  if ((wrapper_val)); then
    wrapper_val=0
    continue
  fi
  if [[ $w =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then continue; fi
  if [[ -n $wrapper && $w == -* ]]; then
    if [[ $w != --* ]]; then
      rest=${w#-}
      while [[ -n $rest ]]; do
        c=${rest:0:1}
        rest=${rest:1}
        case $wrapper_value_table in
        *" ${wrapper}:$c "*)
          if [[ -z $rest ]]; then wrapper_val=1; fi
          break
          ;;
        esac
      done
    fi
    continue
  fi
  if ((expect_duration)) && [[ $w =~ ^[0-9]+(\.[0-9]+)?[smhd]?$ ]]; then
    expect_duration=0
    continue
  fi
  case ${w##*/} in
  env | command | exec | nohup | time | nice | sudo | do | then | if | elif | else | while | until | '!' | '{')
    wrapper=${w##*/}
    continue
    ;;
  timeout)
    wrapper=timeout
    expect_duration=1
    continue
    ;;
  tmux)
    in_tmux=1
    continue
    ;;
  esac
  at_cmd=0
done < <(split_words "$command")

[[ -n $deny_what ]] || exit 0

reason="This tmux command reaches the live tmux server: '$deny_what' with no -L/-S. A worker's panes inherit \$TMUX, which tmux resolves before TMUX_TMPDIR, so a scratch server built with TMUX_TMPDIR alone does not isolate it.
Fix: run 'unset TMUX', point a private 'export TMUX_TMPDIR=\$(mktemp -d)', and pass '-L <name>' on every tmux call, including kill-server."

case $shape in
cursor) jq -cn --arg r "$reason" '{permission: "deny", user_message: $r, agent_message: $r}' ;;
*) jq -cn --arg r "$reason" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}' ;;
esac
