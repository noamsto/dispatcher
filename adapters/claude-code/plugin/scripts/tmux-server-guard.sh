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
# `NAME=value`, `NAME+=value`, array element, `for NAME in` word or
# `${NAME:=value}` anywhere on the line, whatever its order or
# scope (prefix, subshell, pipe element, branch, substitution). The delete is
# refused when any of them reaches the runtime dir, so rebinding an inherited
# name to a private dir does not make deleting through it safe; deleting
# through a fresh name (`d=$(mktemp -d); rm -rf "$d"`) is. A part it cannot know
# (an unset or read variable, `$1`, a glob, a command substitution)
# matches any one segment, but a relative path or one whose first part is
# unknown is allowed, since that is how a private scratch dir from `mktemp -d`
# looks. When the line has no `&& || | & ( ) { } <<`, backtick or control or
# variable keyword (`if`, `for`, `unset`, `read` …) and every binding of a name
# is a scalar statement of its own, the bindings replay in order: a `NAME+=value`
# extends the value the name holds at that point, and every earlier value stays
# a candidate, so an append chain grows linearly. Otherwise the append may be
# skipped and doubles the candidates. An operand that is a product of more than
# 16 candidate values, or a name with more than 64, a candidate over 512
# characters, or a lookup chain past 1500 steps is refused, each with its own reason: past the hook's timeout the guard
# would fail open. For the same reason a command over 8 KiB skips the
# analysis for a linear pass over its command words: a delete verb at command
# position is refused when its words hold a `$`, backtick, glob, `/run/user`,
# `user/`, `/proc/`, XDG_RUNTIME_DIR or TMUX_TMPDIR, or a `cd` came earlier; a
# tmux server call with no -L/-S is refused. Anything else, prose included, is
# allowed.
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
# commands, so a body line starting with `tmux kill-server` is denied.
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

emit_deny() { # <reason>
  case $shape in
  cursor) jq -cn --arg r "$1" '{permission: "deny", user_message: $r, agent_message: $r}' ;;
  *) jq -cn --arg r "$1" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}' ;;
  esac
}

# The tokenizer below is quadratic in the command's length: past the hook's
# timeout the guard would fail open. A command over this many bytes gets a
# linear awk pass over its command words instead of the analysis: a delete or
# tmux verb is only read at command position, so prose in a commit message or
# an echo argument names nothing. Here-doc bodies are read as commands, as the
# analyzer does: any shell may run them.
max_bytes=8192
if (($(printf '%s' "$command" | wc -c) > max_bytes)); then
  # shellcheck disable=SC2016
  gate_awk='
function reset_seg() { phase = 0; wrapper = ""; wval = 0; edur = 0; risky = 0; fdel = 0; isfind = 0; nt = 0 }
function deny() { print "deny"; denied = 1; exit }
function risky_word(w,   x) {
  x = w
  while (index(x, "/./")) gsub(/\/\.\//, "/", x)
  sub(/\/\.$/, "/", x)
  if (x ~ /^\/+$/ || x ~ /[$`*?[]/ || x ~ /user\/|\/proc\/|XDG_RUNTIME_DIR|TMUX_TMPDIR/ || x ~ /(^|[^[:alnum:]_-])\/run(\/+user|\/*$)/ || (x ~ /[.][.]/ && x ~ /(^|[^[:alnum:]_-])\/run\//)) risky = 1
}
function classify(   i, w, sock, sub_, skip, dw, rest, c) {
  sock = 0; sub_ = ""; skip = 0; dw = 0
  for (i = 1; i <= nt; i++) {
    w = targs[i]
    if (skip) { skip = 0; continue }
    if (sub_ != "") {
      if (sub_ ~ /^(set-option|set|set-window-option|setw|set-hook)$/ && w ~ /^-[A-Za-z]*g[A-Za-z]*$/) dw = 1
      if (sub_ ~ /^(set-option|set)$/ && w ~ /^-[A-Za-z]*s[A-Za-z]*$/) dw = 1
      continue
    }
    if (w !~ /^-/) { sub_ = w; continue }
    rest = substr(w, 2)
    while (rest != "") {
      c = substr(rest, 1, 1); rest = substr(rest, 2)
      if (c == "L" || c == "S") sock = 1
      if (index("LSfcT", c)) { if (rest == "") skip = 1; break }
    }
  }
  if (sock) return
  if (dw || sub_ ~ /^(kill-server|kill-session|source-file|source)$/) deny()
}
function seg_word(w,   base, rest, c) {
  if (phase == 0) {
    if (wval) { wval = 0; return }
    if (w ~ /^[A-Za-z_][A-Za-z0-9_]*[+]?=/) return
    if (wrapper != "" && w ~ /^-/) {
      if (w !~ /^--/) {
        rest = substr(w, 2)
        while (rest != "") {
          c = substr(rest, 1, 1); rest = substr(rest, 2)
          if (index(wtab, " " wrapper ":" c " ")) { if (rest == "") wval = 1; break }
        }
      }
      return
    }
    if (edur && w ~ /^[0-9]+([.][0-9]+)?[smhd]?$/) { edur = 0; return }
    base = w; sub(/^.*\//, "", base)
    if (base ~ /^(env|command|exec|nohup|time|nice|sudo|xargs|do|then|if|elif|else|while|until|!|[{])$/) { wrapper = base; return }
    if (base == "timeout") { wrapper = base; edur = 1; return }
    if (base == "cd" || base == "pushd") { seen_cd = 1; phase = 3; return }
    if (base == "tmux") { phase = 2; return }
    if (base ~ /^(rm|rmdir|unlink|trash|trash-put|gtrash|shred)$/) { phase = 1; return }
    if (base == "find") { phase = 1; isfind = 1; return }
    phase = 3
    return
  }
  if (phase == 1) {
    if (isfind && w ~ /^-(delete|exec|execdir|ok|okdir)$/) fdel = 1
    risky_word(w)
    return
  }
  if (phase == 2) targs[++nt] = w
}
function flush_word() {
  if (!inword) return
  inword = 0
  seg_word(word)
  word = ""
}
function end_seg() {
  flush_word()
  if (phase == 1 && (!isfind || fdel) && (risky || seen_cd)) deny()
  if (phase == 2) classify()
  reset_seg()
}
function parse_heredoc(i,   dash, e, c, d, qq) {
  dash = 0; d = ""; qq = ""
  if (ch[i] == "-") { dash = 1; i++ }
  while (ch[i] == " " || ch[i] == "\t") i++
  while (i <= n) {
    c = ch[i]
    if (qq != "") { if (c == qq) qq = ""; else d = d c }
    else if (c == "\047" || c == "\"") qq = c
    else if (c == "\\") { i++; d = d ch[i] }
    else if (c ~ /[ \t;|&()<>]/) break
    else d = d c
    i++
  }
  if (d != "") { hd_n++; hd_d[hd_n] = d; hd_s[hd_n] = dash }
  return i
}
# Past a redirection operator: the spaces and the target word, quotes included.
function skip_target(i,   c, qq) {
  while (ch[i] == " " || ch[i] == "\t") i++
  qq = ""
  while (i <= n) {
    c = ch[i]
    if (qq != "") { if (qq == "\"" && c == "\\") i++; else if (c == qq) qq = "" }
    else if (c == "\047" || c == "\"") qq = c
    else if (c == "\\") i++
    else if (c ~ /[ \t;|&()<>]/) break
    i++
  }
  if (qq != "") { q = qq; inword = 1 }
  return i
}
# Where the `((` at i closes, or 0 when it is nested subshells (`((a) )`), whose
# commands run, or never closes on this line.
function arith_end(i,   k, depth) {
  depth = 0
  for (k = i; k <= n; k++) {
    if (ch[k] == "(") depth++
    else if (ch[k] == ")" && --depth == 0) return (ch[k - 1] == ")" ? k : 0)
  }
  return 0
}
function scan_line(s,   i, j, c, d) {
  n = split(s, ch, "")
  i = 1; dollar_end = 0
  while (i <= n) {
    c = ch[i]
    pd = dollar_end; dollar_end = 0
    if (q == "\047") {
      if (c == "\047") q = ""; else word = word c
    } else if (q == "A") {
      if (c == "\047") q = ""
      else if (c == "\\") { i++; word = word ch[i] }
      else word = word c
    } else if (q == "\"") {
      if (c == "\"") q = ""
      else if (c == "\\") {
        d = ch[i + 1]
        if (d == "\"" || d == "\\" || d == "$" || d == "`") { word = word d; i++ }
        else if (d == "") cont = 1
        else word = word c
      } else word = word c
    } else if (c == "\047") {
      if (pd) q = "A"; else q = "\047"
      inword = 1
    } else if (c == "\"") { q = "\""; inword = 1 }
    else if (c == " " || c == "\t") flush_word()
    else if (c == "\\") {
      d = ch[i + 1]
      if (d == "") cont = 1
      else { word = word d; inword = 1; i++ }
    }
    else if (c == "(" && ch[i + 1] == "(" && (j = arith_end(i)) > 0) {
      # Arithmetic: its `<<` is a shift, not a here-doc.
      if (!pd) end_seg()
      i = j
    }
    else if (c == ";" || c == "|" || c == "&" || c == "(" || c == ")" || c == "`") end_seg()
    else if (c == "#" && !inword) break
    else if (c == "<" || c == ">") {
      if (ch[i + 1] == "(") { flush_word() }
      else if (c == "<" && ch[i + 1] == "<" && ch[i + 2] != "<" && !hd_active) { i = parse_heredoc(i + 2) - 1 }
      else {
        if (inword && word ~ /^[0-9]+$/) { word = ""; inword = 0 } else flush_word()
        while (ch[i + 1] == "<" || ch[i + 1] == ">" || ch[i + 1] == "&" || ch[i + 1] == "|") i++
        i = skip_target(i + 1) - 1
      }
    }
    else { word = word c; inword = 1; dollar_end = (c == "$" && !pd) }
    i++
  }
}
BEGIN {
  wtab = " env:a exec:a env:C env:u nice:n timeout:k timeout:s xargs:n xargs:I xargs:L xargs:P xargs:s xargs:d xargs:E xargs:a xargs:l "
  wtab = wtab "sudo:C sudo:g sudo:h sudo:p sudo:r sudo:t sudo:T sudo:U sudo:u "
  reset_seg()
}
{
  if (hd_n > 0 && hd_active) {
    t = $0
    if (hd_s[hd_cur]) sub(/^\t+/, "", t)
    if (t == hd_d[hd_cur]) {
      q = ""; cont = 0; end_seg(); inword = 0; word = ""
      hd_cur++
      if (hd_cur > hd_n) { hd_active = 0; hd_n = 0 }
      next
    }
  }
  scan_line($0)
  if (q != "") { if (!cont) word = word "\n"; inword = 1 }
  else if (!cont) { end_seg(); if (hd_n > 0 && !hd_active) { hd_cur = 1; hd_active = 1 } }
  cont = 0
}
END { if (!denied) end_seg() }
'
  verdict=$(printf '%s\n' "$command" | LC_ALL=C awk "$gate_awk") || verdict=deny
  if [[ $verdict == deny ]]; then
    emit_deny "This command is over $max_bytes bytes, too long for the guard to analyse within the hook's timeout, and it has a delete that may reach the per-user runtime dir (a \$ variable, a glob, a /run/user or /proc path, XDG_RUNTIME_DIR, TMUX_TMPDIR, or a cd earlier in the command) or a tmux server command with no -L/-S (kill-server, set -g, source-file). The guard refuses what it cannot check.
Fix: split the command into smaller ones, or write the long part to a script file and run that, keeping any delete and any tmux server call out of it ('d=\$(mktemp -d); rm -rf \"\$d\"' for scratch dirs, 'tmux -L <name>' for a private server)."
  fi
  exit 0
fi

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
        # Inside double quotes a backslash escapes only these four; before
        # anything else it is a literal backslash.
        if [[ $q == '"' && $d == \\ ]]; then
          case ${s:i:1} in
          '"' | \\ | '$' | '`')
            d=${s:i:1}
            i=$((i + 1))
            ;;
          $'\n')
            i=$((i + 1))
            continue
            ;;
          esac
        fi
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
            if [[ $q == '"' && $d == \\ ]]; then i=$((i + 1)); fi
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
# Most candidates one operand may expand to as a product of several values, and
# the most one name or a single-value operand may hold; past either the delete is
# refused. A list of paths is cheap, a product of lists is not.
max_cands=16
max_list=64
# Why the delete is refused: '' the runtime dir, `cap` too many candidates,
# `budget` the step budget ran out, `len` a candidate grew too long.
del_why=''
# Every NAME=value, NAME+=value, ${NAME:=value}, array element and `for NAME in`
# word on the line: names in $bn, raw values (quotes stripped, substitutions
# made $unk) in $bv, in $bk `a` for a scalar append and empty for a scalar
# assignment that is a statement of its own, `A` and `x` for those that may not
# run in order (a prefix, an argument), `x` also for an array element, a `for`
# word or a `:=` default.
bn=()
bv=()
bk=()
re_bind_word='(^|[[:space:];&|()`])(["'"'"']?)([A-Za-z_][A-Za-z0-9_]*)(\[([^][]|\[[^][]*\])*\])?([+]?)=$'
re_bind_brace='[$][{]([A-Za-z_][A-Za-z0-9_]*):?=$'
re_for='(^|[[:space:];&|()`])for[[:space:]]+([A-Za-z_][A-Za-z0-9_]*)[[:space:]]+in([[:space:]]|$)'
# A line with none of these runs top to bottom: every binding on it executes, in
# order. (Bindings that are not a statement of their own are told apart in
# collect_bindings.)
re_flow='[(){}&|`]|<<|(^|[[:space:];])(if|then|elif|else|fi|for|while|until|do|done|case|esac|select|function|unset|read|eval|declare|local|typeset|readonly|export|mapfile|readarray)([[:space:];]|$)'
straight=-1
collected=0
visiting=' '
over=0
# Lookups are memoised per name; a result that met a cycle is not, so a cyclic
# chain can still double per level. The step budget and the length cap bound that:
# past either the delete is refused, because running past the hook's timeout would
# fail open.
memo_n=()
memo_off=()
memo_cnt=()
memo_flat=()
cyc=0
steps=0
max_steps=1500
# Longest candidate value; a chain that doubles its value per level reaches
# megabytes, which the segment walk would not finish within the timeout.
max_len=512
budget=0

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
      while ((sp < n)) && [[ ${s:sp:1} != "$c" ]]; do
        if [[ $c == '"' && ${s:sp:1} == \\ ]]; then sp=$((sp + 1)); fi
        sp=$((sp + 1))
      done
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

# Read one word of a binding from $1 at index $2 ($3 is 1 when it starts inside
# double quotes) into $rv; $ri is where it ended. A word runs to the first blank
# or operator outside quotes and `$( … )`/backticks, which are opaque.
read_value() { # <string> <index> <dq>
  local rest=$1 i=$2 dq=$3 n=${#1} c q v=''
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
        strip_quotes "${rest:i:mb-i+1}"
        v+=$sq
        i=$((mb + 1))
        continue
      fi
      v+=$c
      ;;
    *) v+=$c ;;
    esac
    i=$((i + 1))
  done
  rv=$v
  ri=$i
}

# $1 without its quote characters, in $sq. The text of a `${X:-"w"}` is
# expanded as a plain word later, where a quote would be a literal character.
strip_quotes() { # <string>
  sq=${1//\"/}
  sq=${sq//"'"/}
}

# Bind NAME ($1) to each word of $2: the elements of an array, or the list of a
# `for NAME in`, which ends at the first operator or newline. Whichever element
# the shell hands out, the name may hold it.
bind_words() { # <name> <text> <list: 1 for a for-list, 0 for an array>
  local t=$2 i=0 n=${#2} c skipped=0
  ((n)) || return 0
  if [[ $3 == 0 ]]; then t=${t//$'\n'/ }; fi
  while ((i < n)); do
    c=${t:i:1}
    case $c in
    ' ' | $'\t')
      i=$((i + 1))
      continue
      ;;
    ';' | $'\n' | '&' | '|' | '(' | ')' | '<' | '>') break ;;
    esac
    read_value "$t" "$i" 0
    ((ri > i)) || break
    i=$ri
    if [[ $rv =~ ^\[[^]]*\]=(.*)$ ]]; then rv=${BASH_REMATCH[1]}; fi
    # A literal relative word cannot reach the runtime dir by itself, and a long
    # list of them would only run into the candidate cap; but it can be the tail
    # of an absolute path (`/run/$s`), so all of them stand as one unknown.
    case $rv in
    /* | *'$'* | *"$unk"* | *'`'*)
      bn+=("$1")
      bv+=("$rv")
      bk+=(x)
      ;;
    *) skipped=1 ;;
    esac
  done
  if ((skipped)); then
    bn+=("$1")
    bv+=("$unk")
    bk+=(x)
  fi
}

# Fill $bn/$bv/$bk from the raw command. A binding counts wherever it appears —
# prefix, subshell, pipe element, branch, inside a substitution — so a name's
# candidates are a superset of what the shell could hold.
collect_bindings() {
  local rest=$command pre='' name oq app m len ctx direct lead tail kind nl=$'\n'
  collected=1
  while [[ $rest =~ $re_for ]]; do
    name=${BASH_REMATCH[2]}
    m=${BASH_REMATCH[0]}
    rest=${rest#*"$m"}
    bind_words "$name" "$rest" 1
  done
  rest=$command
  # The text before each `=` decides whether it binds; only the `=` itself is
  # kept as context for the next one.
  while [[ $rest == *=* ]]; do
    pre+=${rest%%=*}=
    rest=${rest#*=}
    if [[ $pre =~ $re_bind_brace ]]; then
      match_brace "$rest" 0
      ((mb >= 0)) || mb=${#rest}
      name=${BASH_REMATCH[1]}
      strip_quotes "${rest:0:mb}"
      bn+=("$name")
      bv+=("$sq")
      bk+=(x)
      pre='='
      continue
    fi
    ctx=$pre
    direct=1
    if [[ ! $pre =~ $re_bind_word ]]; then
      # `export "D"=v` and `export D"=v"` hand the shell the word D=v.
      direct=0
      strip_quotes "$pre"
      if [[ ! $sq =~ $re_bind_word ]]; then
        pre='='
        continue
      fi
    fi
    oq=${BASH_REMATCH[2]}
    name=${BASH_REMATCH[3]}
    app=${BASH_REMATCH[6]}
    pre='='
    if [[ ${rest:0:1} == '(' ]]; then
      # An array: every element is a plain binding of the name, `+=` or not.
      skip_parens "$rest" 1
      len=$((sp - 2))
      ((len >= 0)) || len=0
      bind_words "$name" "${rest:1:len}" 0
      continue
    fi
    if [[ $oq == "'" ]]; then
      # `export 'D=value'`: the word is literal up to the closing quote.
      rv=${rest%%"'"*}
    elif [[ $oq == '"' ]]; then
      read_value "$rest" 0 1
    else
      read_value "$rest" 0 0
    fi
    # A binding that is a statement of its own (after `;` or a newline, before
    # `;`, a newline or the end) always runs and persists. `echo d=x`, `d=x cmd`
    # and a here-doc line do not, so the in-order replay must not trust them.
    kind=''
    lead=${ctx%"$name$app="}
    if ((direct)) && [[ -z $oq && $lead != "$ctx" ]]; then
      while [[ $lead == *' ' || $lead == *$'\t' ]]; do lead=${lead%?}; done
      tail=${rest:ri}
      if [[ ($lead == '' || $lead == *';' || $lead == *"$nl") && $tail =~ ^[[:blank:]]*(\;|$nl|$) ]]; then kind=ok; fi
    fi
    if [[ $kind == ok ]]; then kind=''; else kind=x; fi
    if [[ -n $app ]]; then
      if [[ -n $kind ]]; then kind=A; else kind=a; fi
    fi
    bn+=("$name")
    bv+=("$rv")
    bk+=("$kind")
  done
}

# $dd = the values of $2.. without repeats, first occurrence kept, and $dd_over
# set when more than $1 are left: it stops there, since the scan is quadratic. A
# value holding the separator is never dropped.
dedupe() { # <limit> <value>...
  local v sep=$'\003' seen=$'\003' lim=$1
  dd=()
  dd_over=0
  shift
  for v in "$@"; do
    case $v in
    *"$sep"*) ;;
    *)
      case $seen in *"$sep$v$sep"*) continue ;; esac
      seen+=$v$sep
      ;;
    esac
    if ((${#dd[@]} >= lim)); then
      dd_over=1
      break
    fi
    dd+=("$v")
  done
}

# Charge $1 candidate values to the step budget: a step handles up to $max_list
# of them, so counting lookups alone would not bound the run time.
charge() { # <count>
  steps=$((steps + $1))
  if ((steps > max_steps && !budget)); then budget=1; fi
}

# True when the line runs top to bottom (re_flow). A quote, backslash or `#`
# before the last `=` may hide a binding from the shell (`echo ";d=/tmp/;"`) that
# the text still shows, so it ends the replay too.
is_straight() {
  local t
  if ((straight < 0)); then
    # `2>&1`, `&>` and `${p}` are not control flow.
    t=${command//[0-9]>&[0-9]/}
    t=${t//&>/>}
    t=${t//\$\{/\$}
    t=${t//\}/}
    if [[ $t =~ $re_flow || ${t%=*} == *[\\#\"\']* ]]; then straight=0; else straight=1; fi
  fi
  ((straight))
}

# Sets $lv to NAME's candidate values: its exported value from the guard's
# environment, plus the expansion of every binding of NAME on the line. Only
# exported names are read from the environment: a guard-internal or shell-only
# variable says nothing about the worker's shell. An unset or empty value, or
# a name with no candidates, is unknown, never empty, so `rm -rf "$dir/"` is
# not `rm -rf /`. A binding that refers back to its own name (`d=$d/x`) reads
# that name as unknown. In a line that runs top to bottom, where every binding
# of NAME is a scalar, the bindings replay in order: an append extends the value
# the name holds at that point, and each value it held stays a candidate (a use
# may come at any point), so a chain of appends adds one candidate per append.
# Elsewhere an append (`d+=v`) extends every value collected so far and the empty
# value, whatever its order on the line: it may be skipped, or an earlier binding
# may run after it.
lookup_var() { # <name>
  local i e c v cyc0 mixed=0
  local -a out=() gen cur nxt pidx=() aidx=() allidx=()
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
  # A lookup scans every binding, so a long line charges for that.
  steps=$((steps + 1 + ${#bn[@]} / 16))
  if ((steps > max_steps && !budget)); then budget=1; fi
  if ((budget)); then
    lv=("$unk")
    return 0
  fi
  for ((i = 0; i < ${#memo_n[@]}; i++)); do
    [[ ${memo_n[i]} == "$1" ]] || continue
    lv=("${memo_flat[@]:${memo_off[i]}:${memo_cnt[i]}}")
    return 0
  done
  # export -p, not compgen -e: nixpkgs' non-interactive bash has no compgen.
  # Posix mode (POSIXLY_CORRECT) prints `export NAME=` lines.
  if [[ -z $exported ]]; then exported=$'\n'$(export -p)$'\n'; fi
  case $exported in
  *$'\n'"declare -x $1="* | *$'\n'"export $1="*) [[ -z ${!1:-} ]] || out=("${!1}") ;;
  esac
  ((collected)) || collect_bindings
  cyc0=$cyc
  case $visiting in
  *" $1 "*)
    cyc=$((cyc + 1))
    out+=("$unk")
    ;;
  *)
    visiting+="$1 "
    for ((i = 0; i < ${#bn[@]}; i++)); do
      [[ ${bn[i]} == "$1" ]] || continue
      allidx+=("$i")
      case ${bk[i]} in
      a) aidx+=("$i") ;;
      A)
        mixed=1
        aidx+=("$i")
        ;;
      x)
        mixed=1
        pidx+=("$i")
        ;;
      *) pidx+=("$i") ;;
      esac
    done
    if ((${#aidx[@]} && !mixed)) && is_straight; then
      # The base is the exported value, else the empty string an append starts
      # from; only the exported value is a candidate before the first binding.
      cur=("${out[0]-}")
      for i in "${allidx[@]}"; do
        if ((budget)); then break; fi
        expand_word "${bv[i]}"
        if [[ ${bk[i]} == a ]]; then
          if ((${#cur[@]} * ${#exs[@]} > 2 * max_list)); then
            over=1
            break
          fi
          nxt=()
          for c in "${cur[@]}"; do
            for e in "${exs[@]}"; do
              v=$c$e
              if ((${#v} > max_len && !budget)); then budget=2; fi
              nxt+=("$v")
            done
          done
          dedupe "$max_list" "${nxt[@]}"
        else
          dedupe "$max_list" "${exs[@]}"
        fi
        if ((dd_over)); then over=1; fi
        cur=("${dd[@]}")
        for c in "${cur[@]}"; do out+=("${c:-$unk}"); done
        charge $((${#cur[@]} + ${#exs[@]}))
        if ((${#out[@]} > max_list)); then
          dedupe "$max_list" "${out[@]}"
          out=("${dd[@]}")
          if ((dd_over)); then
            over=1
            break
          fi
        fi
      done
    else
      for i in ${pidx[@]+"${pidx[@]}"}; do
        if ((budget || over)); then break; fi
        expand_word "${bv[i]}"
        for e in "${exs[@]}"; do out+=("${e:-$unk}"); done
        charge ${#exs[@]}
        if ((${#out[@]} > max_list)); then
          dedupe "$max_list" "${out[@]}"
          out=("${dd[@]}")
          if ((dd_over)); then over=1; fi
        fi
      done
      for i in ${aidx[@]+"${aidx[@]}"}; do
        if ((budget || over)); then break; fi
        expand_word "${bv[i]}"
        if (((${#out[@]} + 1) * ${#exs[@]} > 2 * max_list)); then
          over=1
          break
        fi
        gen=()
        for c in ${out[@]+"${out[@]}"} ""; do
          for e in "${exs[@]}"; do
            v=$c$e
            if ((${#v} > max_len && !budget)); then budget=2; fi
            gen+=("${v:-$unk}")
          done
        done
        out+=("${gen[@]}")
        charge ${#gen[@]}
        if ((${#out[@]} > max_list)); then
          dedupe "$max_list" "${out[@]}"
          out=("${dd[@]}")
          if ((dd_over)); then over=1; fi
        fi
      done
    fi
    visiting=${visiting%"$1 "}
    ;;
  esac
  if ((${#out[@]})); then
    dedupe "$max_list" "${out[@]}"
    out=("${dd[@]}")
    if ((dd_over)); then over=1; fi
  fi
  ((${#out[@]})) || out=("$unk")
  lv=("${out[@]}")
  # Cached only when no cycle cut it short: a name read while an ancestor was
  # being visited may be missing that ancestor's values. Kept as offsets into
  # one flat array, since a value may hold any character, newline included.
  if ((cyc == cyc0 && !budget)); then
    memo_n+=("$1")
    memo_off+=("${#memo_flat[@]}")
    memo_cnt+=("${#out[@]}")
    memo_flat+=("${out[@]}")
  fi
}

# Expand the parameters in one word into $exs, the list of values it may take
# (the cross product of its parameters' candidates); whatever the guard cannot
# know (positional and special parameters, other ${…} forms, command
# substitutions) becomes $unk. A product of more than $max_cands values or a
# list of more than $max_list sets $over.
#   ${X:-w} ${X-w} ${X:=w} ${X=w}   candidates of X and of w
#   ${X:+w} ${X+w}                  candidates of w, or unknown
#   ${X:?w} ${X?w} ${X%w} ${X%%w}   candidates of X (a `%` strip leaves a
#                                   prefix, so the whole value leans to deny)
expand_word() { # <word>
  local s=$1 i=0 n=${#1} c inner name op lit='' a l cap
  local -a acc=('') vals next
  steps=$((steps + 1))
  if ((steps > max_steps && !budget)); then budget=1; fi
  if ((budget)); then
    exs=("$unk")
    return 0
  fi
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
            # An array subscript (`${a[@]}`, `${a[0]}`): any element.
            if [[ $op == \[* ]]; then op=${op#*]}; fi
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
            :* | \#* | /* | ^* | ,* | @*)
              # A slice or a rewrite of the value: whatever it leaves is a
              # piece of it, so lean to deny like `%`.
              lookup_var "$name"
              vals=("${lv[@]}" "$unk")
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
    dedupe "$max_list" "${vals[@]}"
    if ((dd_over)); then over=1; fi
    vals=("${dd[@]}")
    charge ${#vals[@]}
    # A product of two lists is bounded tighter than a list alone. The product is
    # sized before it is built, so a big one costs nothing.
    cap=$max_list
    if ((${#acc[@]} > 1 && ${#vals[@]} > 1)); then cap=$max_cands; fi
    next=()
    if ((${#acc[@]} * ${#vals[@]} > cap)); then
      over=1
      acc=("${acc[@]:0:cap}")
      vals=("${vals[@]:0:cap}")
    fi
    for a in "${acc[@]}"; do
      for l in "${vals[@]}"; do
        ((${#next[@]} < cap)) || break 2
        next+=("$a$lit$l")
        if ((${#a} + ${#lit} + ${#l} > max_len && !budget)); then budget=2; fi
      done
    done
    lit=''
    dedupe "$cap" "${next[@]}"
    acc=("${dd[@]}")
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
  charge ${#exs[@]}
  if ((budget)); then
    del_target=$1
    if ((budget == 2)); then del_why=len; else del_why=budget; fi
    return 0
  fi
  if ((over)); then
    del_target=$1
    del_why=cap
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
  if [[ $w =~ ^[A-Za-z_][A-Za-z0-9_]*[+]?= ]]; then continue; fi
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
elif [[ $del_why == cap ]]; then
  reason="This deletes '$del_target', which expands to too many candidate values (a product of more than $max_cands, or a list of more than $max_list) for the guard to check against the per-user runtime dir. The guard refuses what it cannot check.
Fix: delete one concrete path per command, or a private dir you created ('d=\$(mktemp -d); rm -rf \"\$d\"')."
elif [[ $del_why == budget ]]; then
  reason="This deletes '$del_target', whose variables chain too deeply for the guard to resolve within its expansion budget, so it cannot rule out the per-user runtime dir. The guard refuses what it cannot check.
Fix: delete a concrete path or a private dir you created ('d=\$(mktemp -d); rm -rf \"\$d\"'), without a long chain of variables."
elif [[ $del_why == len ]]; then
  reason="This deletes '$del_target', which expands to a path of more than $max_len characters, too long for the guard to check against the per-user runtime dir. The guard refuses what it cannot check.
Fix: delete a shorter path, or a private dir you created ('d=\$(mktemp -d); rm -rf \"\$d\"')."
elif [[ -n $del_target ]]; then
  reason="This deletes '$del_target', which resolves to the per-user runtime dir (\$XDG_RUNTIME_DIR, /run/user/<uid>) or one of its parents or children: it holds the D-Bus, systemd and tmux sockets, and deleting it breaks the owner's session until reboot.
Fix: make scratch dirs with 'mktemp -d' and delete them through a name you created, e.g. 'd=\$(mktemp -d); rm -rf \"\$d\"' (for a nested session: 'rt=\$(mktemp -d); XDG_RUNTIME_DIR=\$rt Hyprland; rm -rf \"\$rt\"'). Never delete through an inherited variable such as TMUX_TMPDIR or XDG_RUNTIME_DIR, even after rebinding it: the guard counts every value the name may hold."
else
  exit 0
fi

emit_deny "$reason"
