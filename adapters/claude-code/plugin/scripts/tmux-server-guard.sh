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
# sockets. Paths resolve lexically, never evaluated. A `$NAME` stands for every
# value it may hold: its exported value in the guard's environment plus every
# `NAME=value` or `${NAME:=value}` anywhere on the line, whatever its order or
# scope (prefix, subshell, pipe element, branch, substitution). The delete is
# refused when any of them reaches the runtime dir, so rebinding an inherited
# name to a private dir does not make deleting through it safe; deleting
# through a fresh name (`d=$(mktemp -d); rm -rf "$d"`) is. A part it cannot know
# (an unset, loop or read variable, `$1`, a glob, a command substitution)
# matches any one segment, but a relative path or one whose first part is
# unknown is allowed, since that is how a private scratch dir from `mktemp -d`
# looks. An operand with more than 16 candidate values is refused.
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
# shellcheck disable=SC2016
command=${command//'$(id -u)'/$UID}
# shellcheck disable=SC2016
command=${command//'$(id -ru)'/$UID}
# shellcheck disable=SC2016
command=${command//'`id -u`'/$UID}
# shellcheck disable=SC2016
command=${command//'`id -ru`'/$UID}

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
exported=''
xdg_root=()
xdg_done=0
del_target=''
# A path segment the guard cannot know; it matches any segment.
unk=$'\002'
# Most candidates one operand may expand to; past it the delete is refused.
max_cands=16
# Every NAME=value and ${NAME:=value} on the line: names in $bn, raw values
# (quotes stripped, substitutions made $unk) in $bv.
bn=()
bv=()
re_bind_word='(^|[[:space:];&|()`])([A-Za-z_][A-Za-z0-9_]*)=$'
re_bind_brace='[$][{]([A-Za-z_][A-Za-z0-9_]*):?=$'
collected=0
visiting=' '
over=0

# Index just past the `)` matching the `(` before $2 in $1, quotes skipped.
skip_parens() { # <string> <index>
  local s=$1 n=${#1} d=1 c
  sp=$2
  while ((sp < n && d > 0)); do
    c=${s:sp:1}
    sp=$((sp + 1))
    case $c in
    '(') d=$((d + 1)) ;;
    ')') d=$((d - 1)) ;;
    "'" | '"')
      while ((sp < n)) && [[ ${s:sp:1} != "$c" ]]; do sp=$((sp + 1)); done
      sp=$((sp + 1))
      ;;
    \\) sp=$((sp + 1)) ;;
    esac
  done
}

# Index of the `}` matching the `${` whose body starts at $2 in $1, nested
# braces counted; -1 when unmatched.
match_brace() { # <string> <index>
  local s=$1 n=${#1} d=1
  mb=$2
  while ((mb < n)); do
    case ${s:mb:1} in
    '{') d=$((d + 1)) ;;
    '}')
      d=$((d - 1))
      ((d > 0)) || return 0
      ;;
    esac
    mb=$((mb + 1))
  done
  mb=-1
}

# Fill $bn/$bv from the raw command. A binding counts wherever it appears —
# prefix, subshell, pipe element, branch, inside a substitution — so a name's
# candidates are a superset of what the shell could hold. `$( … )` and
# backticks are opaque, so a value runs to the first blank or operator outside
# them and quotes.
collect_bindings() {
  local rest=$command pre='' name v c i n q dq
  collected=1
  # The text before each `=` decides whether it binds; only the `=` itself is
  # kept as context for the next one.
  while [[ $rest == *=* ]]; do
    pre+=${rest%%=*}=
    rest=${rest#*=}
    if [[ $pre =~ $re_bind_brace ]]; then
      match_brace "$rest" 0
      ((mb >= 0)) || mb=${#rest}
      bn+=("${BASH_REMATCH[1]}")
      bv+=("${rest:0:mb}")
      pre='='
      continue
    fi
    if [[ ! $pre =~ $re_bind_word ]]; then
      pre='='
      continue
    fi
    name=${BASH_REMATCH[2]}
    pre='='
    v=''
    i=0
    n=${#rest}
    dq=0
    while ((i < n)); do
      c=${rest:i:1}
      case $c in
      ' ' | $'\t' | $'\n' | ';' | '|' | '&' | '(' | ')' | '<' | '>')
        ((dq)) || break
        v+=$c
        ;;
      '"') dq=$((1 - dq)) ;;
      "'")
        if ((dq)); then
          v+=$c
        else
          q=${rest:i+1}
          q=${q%%"'"*}
          v+=$q
          i=$((i + ${#q} + 1))
        fi
        ;;
      \\)
        i=$((i + 1))
        v+=${rest:i:1}
        ;;
      '`')
        v+=$unk
        i=$((i + 1))
        while ((i < n)) && [[ ${rest:i:1} != '`' ]]; do i=$((i + 1)); done
        ;;
      '$')
        if [[ ${rest:i+1:1} == '(' ]]; then
          v+=$unk
          skip_parens "$rest" $((i + 2))
          i=$sp
          continue
        fi
        if [[ ${rest:i+1:1} == '{' ]]; then
          match_brace "$rest" $((i + 2))
          ((mb >= 0)) || mb=$((n - 1))
          v+=${rest:i:mb-i+1}
          i=$((mb + 1))
          continue
        fi
        v+=$c
        ;;
      *) v+=$c ;;
      esac
      i=$((i + 1))
    done
    bn+=("$name")
    bv+=("$v")
  done
}

# Sets $lv to NAME's candidate values: its exported value from the guard's
# environment, plus the expansion of every binding of NAME on the line. Only
# exported names are read from the environment: a guard-internal or shell-only
# variable says nothing about the worker's shell. An unset or empty value, or
# a name with no candidates, is unknown, never empty, so `rm -rf "$dir/"` is
# not `rm -rf /`. A binding that refers back to its own name (`d=$d/x`) reads
# that name as unknown.
lookup_var() { # <name>
  local i e
  local -a out=()
  case $1 in
  UID)
    lv=("$UID")
    return 0
    ;;
  EUID)
    lv=("$EUID")
    return 0
    ;;
  esac
  # export -p, not compgen -e: nixpkgs' non-interactive bash has no compgen.
  # Posix mode (POSIXLY_CORRECT) prints `export NAME=` lines.
  if [[ -z $exported ]]; then exported=$'\n'$(export -p)$'\n'; fi
  case $exported in
  *$'\n'"declare -x $1="* | *$'\n'"export $1="*) [[ -z ${!1:-} ]] || out=("${!1}") ;;
  esac
  ((collected)) || collect_bindings
  case $visiting in
  *" $1 "*) out+=("$unk") ;;
  *)
    visiting+="$1 "
    for ((i = 0; i < ${#bn[@]}; i++)); do
      [[ ${bn[i]} == "$1" ]] || continue
      expand_word "${bv[i]}"
      for e in "${exs[@]}"; do out+=("${e:-$unk}"); done
    done
    visiting=${visiting%"$1 "}
    ;;
  esac
  if ((${#out[@]} > max_cands)); then over=1; fi
  ((${#out[@]})) || out=("$unk")
  lv=("${out[@]}")
}

# Expand the parameters in one word into $exs, the list of values it may take
# (the cross product of its parameters' candidates); whatever the guard cannot
# know (positional and special parameters, other ${…} forms, command
# substitutions) becomes $unk. More than $max_cands sets $over.
#   ${X:-w} ${X-w} ${X:=w} ${X=w}   candidates of X and of w
#   ${X:+w} ${X+w}                  candidates of w, or unknown
#   ${X:?w} ${X?w} ${X%w} ${X%%w}   candidates of X (a `%` strip leaves a
#                                   prefix, so the whole value leans to deny)
expand_word() { # <word>
  local s=$1 i=0 n=${#1} c inner name op lit='' a l
  local -a acc=('') vals next
  while ((i <= n)); do
    c=${s:i:1}
    i=$((i + 1))
    if ((i <= n)) && [[ $c != '$' && $c != '`' ]]; then
      lit+=$c
      continue
    fi
    if ((i > n)); then
      vals=('')
    elif [[ $c == '`' ]]; then
      vals=("$unk")
      while ((i < n)) && [[ ${s:i:1} != '`' ]]; do i=$((i + 1)); done
      i=$((i + 1))
    else
      c=${s:i:1}
      case $c in
      '{')
        match_brace "$s" $((i + 1))
        if ((mb < 0)); then
          vals=("$unk")
          i=$n
        else
          inner=${s:i+1:mb-i-1}
          i=$((mb + 1))
          vals=("$unk")
          if [[ $inner =~ ^([A-Za-z_][A-Za-z0-9_]*)(.*)$ ]]; then
            name=${BASH_REMATCH[1]}
            op=${BASH_REMATCH[2]}
            case $op in
            '' | :\?* | \?* | %*)
              lookup_var "$name"
              vals=("${lv[@]}")
              ;;
            :-* | :=* | -* | =*)
              lookup_var "$name"
              vals=("${lv[@]}")
              op=${op#:}
              expand_word "${op:1}"
              vals+=("${exs[@]}")
              ;;
            :+* | +*)
              op=${op#:}
              expand_word "${op:1}"
              vals=("${exs[@]}" "$unk")
              ;;
            esac
          fi
        fi
        ;;
      '(')
        vals=("$unk")
        skip_parens "$s" $((i + 1))
        i=$sp
        ;;
      [A-Za-z_])
        inner=${s:i}
        inner=${inner%%[!A-Za-z0-9_]*}
        i=$((i + ${#inner}))
        lookup_var "$inner"
        vals=("${lv[@]}")
        ;;
      [0-9@*#\$!?-])
        vals=("$unk")
        i=$((i + 1))
        ;;
      *)
        # A bare `$`: the remnant an unquoted `$(…)` leaves after the split.
        vals=("$unk")
        ;;
      esac
    fi
    next=()
    for a in "${acc[@]}"; do
      for l in "${vals[@]}"; do next+=("$a$lit$l"); done
    done
    lit=''
    if ((${#next[@]} > max_cands)); then
      over=1
      next=("${next[@]:0:max_cands}")
    fi
    acc=("${next[@]}")
  done
  exs=("${acc[@]}")
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
  local c
  [[ -z $del_target ]] || return 0
  expand_word "$1"
  if ((over)); then
    del_target=$1
    return 0
  fi
  for c in "${exs[@]}"; do
    [[ $c == /* ]] || continue
    norm_path "$c"
    if reaches_root run user "$unk" || { ((${#xdg_root[@]})) && reaches_root "${xdg_root[@]}"; }; then
      del_target=$1
      return 0
    fi
  done
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
  ((at_cmd)) || continue

  if ((wrapper_val)); then
    wrapper_val=0
    continue
  fi
  # A NAME=value prefix is not the command word; collect_bindings reads it.
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
  rm | rmdir | unlink | trash-put) in_del='rm' ;;
  gtrash) in_del='gtrash' ;;
  find) in_del='find' ;;
  esac
  at_cmd=0
done < <(split_words "$command")

if [[ -n $deny_what ]]; then
  reason="This tmux command reaches the live tmux server: '$deny_what' with no -L/-S. A worker's panes inherit \$TMUX, which tmux resolves before TMUX_TMPDIR, so a scratch server built with TMUX_TMPDIR alone does not isolate it.
Fix: run 'unset TMUX', point a private 'export TMUX_TMPDIR=\$(mktemp -d)', and pass '-L <name>' on every tmux call, including kill-server."
elif [[ -n $del_target ]]; then
  reason="This deletes '$del_target', which resolves to the per-user runtime dir (\$XDG_RUNTIME_DIR, /run/user/<uid>) or one of its parents or children: it holds the D-Bus, systemd and tmux sockets, and deleting it breaks the owner's session until reboot.
Fix: make scratch dirs with 'mktemp -d' and delete them through a name you created, e.g. 'd=\$(mktemp -d); rm -rf \"\$d\"' (for a nested session: 'rt=\$(mktemp -d); XDG_RUNTIME_DIR=\$rt Hyprland; rm -rf \"\$rt\"'). Never delete through an inherited variable such as TMUX_TMPDIR or XDG_RUNTIME_DIR, even after rebinding it: the guard counts every value the name may hold."
else
  exit 0
fi

case $shape in
cursor) jq -cn --arg r "$reason" '{permission: "deny", user_message: $r, agent_message: $r}' ;;
*) jq -cn --arg r "$reason" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}' ;;
esac
