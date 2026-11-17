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
# Also denied: a delete (rm, rmdir, unlink, trash-put, gtrash put, find
# -delete) of a path at, below or above the per-user runtime dir
# ($XDG_RUNTIME_DIR, /run/user/<x>), which holds the D-Bus, systemd and tmux
# sockets. Paths resolve lexically, never evaluated: exported variables from
# the guard's environment, NAME=literal bindings earlier in the line, and the
# uid. A part it cannot know (an unset, loop or read variable, `$1`, a glob, a
# command substitution) matches any one segment, but a relative path or one
# whose first part is unknown is allowed, since that is how a private scratch
# dir from `mktemp -d` looks.
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
# For deletes: symlinks into the runtime dir; `cd` then a relative path (`cd
# "$XDG_RUNTIME_DIR" && rm -rf tmux-1000`); `mv`, `shred`, `rsync --delete`;
# `xargs rm` and `find -exec rm`; operands after an unquoted command
# substitution (`rm -rf $(f) /run/user/1000`), since the split ends the command
# there.
#
# Portable to macOS's bash 3.2 and BSD userland: no mapfile, no ${var,,}, no
# associative arrays.

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

# The uid substitutions become their digits before the split, so the quoted and
# the unquoted `/run/user/$(id -u)` reach the delete check as one plain word.
# They hold no tmux word, so the tmux check sees an equivalent command.
command=${command//"\$(id -u)"/$UID}
command=${command//"\$(id -ru)"/$UID}
command=${command//"\`id -u\`"/$UID}
command=${command//"\`id -ru\`"/$UID}

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

in_del=''
dargs=()
bind_mode=''
bound_names=' '
bound_n=()
bound_v=()
exported=''
xdg_root=()
xdg_done=0
del_target=''
# A path segment the guard cannot know; it matches any segment.
unk=$'\002'
re_name='^[A-Za-z_][A-Za-z0-9_]*$'
re_brace='^([A-Za-z_][A-Za-z0-9_]*)((:-|-|:=).*)?$'

# Only a plain NAME=literal binding is known; any other binding of a name the
# line uses later (export, read, for, unset, NAME=$(…)) makes it unknown.
bind_word() { # <word>
  local v
  if [[ $1 =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
    v=${1#*=}
    case $v in *'$'* | *'`'*) v=$unk ;; esac
    bound_names+="${1%%=*} "
    bound_n+=("${1%%=*}")
    bound_v+=("$v")
  elif [[ $1 =~ $re_name ]]; then
    bound_names+="$1 "
    bound_n+=("$1")
    bound_v+=("$unk")
  fi
}

# Sets $lv to NAME's value, or $unk. Only exported names are read from the
# guard's environment: a guard-internal or shell-only variable says nothing
# about the worker's shell. An unset or empty one is unknown, never empty, so
# `rm -rf "$dir/"` is not `rm -rf /`.
lookup_var() { # <name>
  local i
  lv=$unk
  case $bound_names in
  *" $1 "*)
    for ((i = ${#bound_n[@]} - 1; i >= 0; i--)); do
      if [[ ${bound_n[i]} == "$1" ]]; then
        lv=${bound_v[i]}
        return 0
      fi
    done
    ;;
  esac
  case $1 in
  UID)
    lv=$UID
    return 0
    ;;
  EUID)
    lv=$EUID
    return 0
    ;;
  esac
  [[ $1 =~ $re_name ]] || return 0
  # export -p, not compgen -e: nixpkgs' non-interactive bash has no compgen.
  if [[ -z $exported ]]; then exported=$'\n'$(export -p)$'\n'; fi
  case $exported in
  *$'\n'"declare -x $1="*) [[ -z ${!1:-} ]] || lv=${!1} ;;
  esac
}

# Expand the parameters in one operand into $ex, left to right; whatever the
# guard cannot know (positional and special parameters, other ${…} forms,
# command substitutions) becomes $unk.
expand_word() { # <word>
  local s=$1 i=0 n=${#1} c inner depth
  ex=''
  while ((i < n)); do
    c=${s:i:1}
    i=$((i + 1))
    if [[ $c == '`' ]]; then
      ex+=$unk
      while ((i < n)) && [[ ${s:i:1} != '`' ]]; do i=$((i + 1)); done
      i=$((i + 1))
      continue
    fi
    if [[ $c != '$' ]]; then
      ex+=$c
      continue
    fi
    c=${s:i:1}
    case $c in
    '{')
      inner=${s:i+1}
      if [[ $inner != *'}'* ]]; then
        ex+=$unk
        return 0
      fi
      inner=${inner%%'}'*}
      i=$((i + ${#inner} + 2))
      if [[ $inner =~ $re_brace ]]; then
        lookup_var "${BASH_REMATCH[1]}"
        ex+=$lv
      else
        ex+=$unk
      fi
      ;;
    '(')
      ex+=$unk
      depth=1
      i=$((i + 1))
      while ((i < n && depth > 0)); do
        case ${s:i:1} in
        '(') depth=$((depth + 1)) ;;
        ')') depth=$((depth - 1)) ;;
        esac
        i=$((i + 1))
      done
      ;;
    [A-Za-z_])
      inner=${s:i}
      inner=${inner%%[!A-Za-z0-9_]*}
      i=$((i + ${#inner}))
      lookup_var "$inner"
      ex+=$lv
      ;;
    [0-9@*#\$!?-])
      ex+=$unk
      i=$((i + 1))
      ;;
    *)
      # A bare `$`: the remnant an unquoted `$(…)` leaves after the split.
      ex+=$unk
      ;;
    esac
  done
}

# Lexical normalisation into $segs[0..nseg): empty and `.` dropped, `..`
# folded, a segment holding $unk or a glob character made $unk. `..` after an
# unknown segment leaves it: the unknown part may have been several segments.
norm_path() { # <expanded path>
  local p=$1 seg
  segs=()
  nseg=0
  while [[ -n $p ]]; do
    seg=${p%%/*}
    if [[ $p == */* ]]; then p=${p#*/}; else p=''; fi
    case $seg in
    '' | .) ;;
    ..)
      if ((nseg > 0)) && [[ ${segs[nseg - 1]} != "$unk" ]]; then nseg=$((nseg - 1)); fi
      ;;
    *"$unk"* | *[*?[]*)
      segs[nseg]=$unk
      nseg=$((nseg + 1))
      ;;
    *)
      segs[nseg]=$seg
      nseg=$((nseg + 1))
      ;;
    esac
  done
}

# True when the target ($segs) and the root agree on every segment both have:
# the target is at, below or above the root. Deleting an ancestor empties the
# runtime dir too, so above counts.
reaches_root() { # <root segment>...
  local i=0 r
  for r in "$@"; do
    ((i < nseg)) || return 0
    [[ $r == "$unk" || ${segs[i]} == "$unk" || ${segs[i]} == "$r" ]] || return 1
    i=$((i + 1))
  done
}

# A relative target, or one whose first segment is unknown, is allowed: it is
# most likely a private scratch dir the guard cannot see.
judge_target() { # <operand>
  [[ -z $del_target ]] || return 0
  expand_word "$1"
  [[ $ex == /* ]] || return 0
  norm_path "$ex"
  if reaches_root run user "$unk" || { ((${#xdg_root[@]})) && reaches_root "${xdg_root[@]}"; }; then
    del_target=$1
  fi
}

# Judge the operands of the delete being collected, if any.
flush_del() {
  local w opts=1 put=0 skip=0 paths=0
  [[ -n $in_del ]] || return 0
  if ((!xdg_done)); then
    xdg_done=1
    if [[ ${XDG_RUNTIME_DIR:-} == /* ]]; then
      norm_path "$XDG_RUNTIME_DIR"
      ((nseg == 0)) || xdg_root=("${segs[@]:0:nseg}")
    fi
  fi
  case $in_del in
  rm)
    for w in ${dargs[@]+"${dargs[@]}"}; do
      if ((opts)); then
        if [[ $w == -- ]]; then
          opts=0
          continue
        fi
        if [[ $w == -* ]]; then continue; fi
      fi
      judge_target "$w"
    done
    ;;
  gtrash)
    for w in ${dargs[@]+"${dargs[@]}"}; do
      if [[ $w == -* ]]; then continue; fi
      if ((put)); then
        judge_target "$w"
        continue
      fi
      [[ $w == put ]] || break
      put=1
    done
    ;;
  find)
    case " ${dargs[*]-} " in
    *" -delete "*)
      for w in ${dargs[@]+"${dargs[@]}"}; do
        if ((skip)); then
          skip=0
          continue
        fi
        if ((paths)); then
          judge_target "$w"
          paths=0
          continue
        fi
        if ((opts)); then
          case $w in
          -H | -L | -P | -E | -X | -s | -x | -O*) continue ;;
          -f)
            paths=1
            continue
            ;;
          -D)
            skip=1
            continue
            ;;
          esac
          opts=0
        fi
        case $w in -* | '('* | '!'*) break ;; esac
        judge_target "$w"
      done
      ;;
    esac
    ;;
  esac
  in_del=''
  dargs=()
}

while IFS= read -r w; do
  if [[ -z $w || $w == "$nl_mark" ]]; then
    flush_tmux
    flush_del
    [[ -z $deny_what && -z $del_target ]] || break
    at_cmd=1
    wrapper=''
    wrapper_val=0
    expect_duration=0
    bind_mode=''
    continue
  fi

  if ((in_tmux)); then
    targs+=("$w")
    continue
  fi
  if [[ -n $in_del ]]; then
    dargs+=("$w")
    continue
  fi
  case $bind_mode in
  for)
    bind_word "$w"
    bind_mode=''
    continue
    ;;
  binder)
    [[ $w == -* ]] || bind_word "$w"
    continue
    ;;
  esac
  ((at_cmd)) || continue

  if ((wrapper_val)); then
    wrapper_val=0
    continue
  fi
  if [[ $w =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; then
    bind_word "$w"
    continue
  fi
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
  rm | rmdir | unlink | trash-put) in_del='rm' ;;
  gtrash) in_del='gtrash' ;;
  find) in_del='find' ;;
  export | local | declare | typeset | readonly | unset | read) bind_mode=binder ;;
  for) bind_mode=for ;;
  esac
  at_cmd=0
done < <(split_words "$command")

if [[ -n $deny_what ]]; then
  reason="This tmux command reaches the live tmux server: '$deny_what' with no -L/-S. A worker's panes inherit \$TMUX, which tmux resolves before TMUX_TMPDIR, so a scratch server built with TMUX_TMPDIR alone does not isolate it.
Fix: run 'unset TMUX', point a private 'export TMUX_TMPDIR=\$(mktemp -d)', and pass '-L <name>' on every tmux call, including kill-server."
elif [[ -n $del_target ]]; then
  reason="This deletes '$del_target', which resolves to the per-user runtime dir (\$XDG_RUNTIME_DIR, /run/user/<uid>) or one of its parents or children: it holds the D-Bus, systemd and tmux sockets, and deleting it breaks the owner's session until reboot.
Fix: make scratch dirs with 'mktemp -d' and delete only paths you created; never delete a path taken from an inherited variable such as TMUX_TMPDIR."
else
  exit 0
fi

case $shape in
cursor) jq -cn --arg r "$reason" '{permission: "deny", user_message: $r, agent_message: $r}' ;;
*) jq -cn --arg r "$reason" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}' ;;
esac
