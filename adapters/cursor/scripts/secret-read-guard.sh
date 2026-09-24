#!/usr/bin/env bash
# Pre-tool guard: stop an agent printing a secret into its own transcript.
#
# Three real incidents, none of them careless: a malformed ${VAR:-default}
# expansion echoed an API key, a read-only reviewer subagent grepped .env to
# ground its review, and `fish -ic 'set -S LINEAR_API_KEY' | grep -v -i value`
# printed the key on a `$NAME[1]: |…|` line the grep never touched. Each landed
# a live value in a transcript and forced a credential rotation. Guidance
# cannot fix an instinct; a guard can.
#
# Blocks the printing paths only. Writing, testing existence, ignoring and
# deleting stay allowed — `test -f .env`, direnv, update-env are ordinary work.
#
# One script for every engine. The caller is recognised by the event it names,
# and the deny is rendered in that caller's shape:
#
#   engine        event                  input keys                          deny shape
#   claude        PreToolUse Bash/Read/  tool_input.command | .file_path |   hookSpecificOutput
#                 Grep                   .{path,glob,pattern,output_mode}
#   codex         PreToolUse Bash        tool_input.command (+ turn_id)      hookSpecificOutput
#   pi(hookyard)  canonical_event        tool_input.command | .path (may     hookSpecificOutput
#                 pre_tool Bash/Read/    carry pi's "@" prefix) |
#                 Grep                   .{path,glob,pattern}
#   cursor        preToolUse Shell       tool_input.command                  permission/user_message/
#   cursor        beforeShellExecution   command                             agent_message
#   cursor        beforeReadFile         file_path (+ content)
#   cursor        preToolUse Read/Grep   tool_input.file_path |
#                                        .{pattern,file_path,glob,output_mode}
#
# claude/codex shapes: hookyard captures and the codex 0.154.0 embedded
# pre-tool-use schema. cursor: hookyard captures, plus createToolInput in the
# cursor-agent 2026.09.23 bundle for the Read/Grep keys (no live capture yet).
# Allow is always no stdout, exit 0. Stdout carries exactly one deny object or
# nothing: cursor blocks the call on any non-JSON stdout, so a stray echo here
# would block every cursor tool call.
#
# A payload the guard cannot parse fails open but loud — exit 1, message on
# stderr — rather than closed: blocking every call on a broken host would stop
# every worker, and a non-zero hook exit is shown by every engine while the call
# proceeds.

set -euo pipefail

# GNU grep and bash both treat [A-EG-Z]-style ranges as letter ranges only
# under a known collation; it also makes awk count bytes, as bash does.
export LC_ALL=C

command -v jq >/dev/null || {
  echo "secret-read-guard: jq not found; guard NOT enforcing" >&2
  exit 1
}

input=$(cat)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Engine data reaches bash as NUL-separated fields, never eval'd. A NUL inside
# a value would shift every later field, so it becomes a space (which only ever
# makes a command look more like separate words, never less).
# shellcheck disable=SC2016
normalise='
def s: if type == "string" then gsub("\u0000"; " ") else "" end;
def unat: if startswith("@") then .[1:] else . end;
if type != "object" then empty else
(.tool_input | if type == "object" then . else {} end) as $ti
| (.hook_event_name | s) as $ev
| (.tool_name | s) as $tool
| (if (.canonical_event | type) == "string" then "hookyard"
   elif $ev == "PreToolUse" then "claude"
   elif ($ev == "preToolUse" or $ev == "beforeShellExecution" or $ev == "beforeReadFile") then "cursor"
   else "" end) as $shape
| if $shape == "" then empty else
  (if $ev == "beforeShellExecution" and $shape == "cursor" then {kind: "shell", command: (.command | s)}
   elif $ev == "beforeReadFile" and $shape == "cursor" then {kind: "read", path: (.file_path | s)}
   elif $shape == "cursor" then
     if $tool == "Shell" then {kind: "shell", command: ($ti.command | s)}
     # Read/Grep keys are from the shipped bundle, not a live capture, so the
     # older path/target_file spellings stay as fallbacks.
     elif $tool == "Read" then {kind: "read", path: (first($ti.file_path, $ti.path, $ti.target_file | select(type == "string")) // "" | s)}
     elif $tool == "Grep" then {kind: "grep", path: (first($ti.file_path, $ti.path | select(type == "string")) // "" | s),
       glob: ($ti.glob | s), pattern: ($ti.pattern | s),
       mode: ($ti.output_mode | if type == "string" then . else "content" end)}
     else {kind: "none"} end
   elif $tool == "Bash" then {kind: "shell", command: ($ti.command | s)}
   elif $tool == "Read" then {kind: "read", path: (if $shape == "hookyard" then $ti.path | s | unat else $ti.file_path | s end)}
   elif $tool == "Grep" then {kind: "grep", path: ($ti.path | s | if $shape == "hookyard" then unat else . end),
     glob: ($ti.glob | s), pattern: ($ti.pattern | s),
     # pi grep has no files/count mode; it always prints matching lines.
     mode: (if $shape == "hookyard" then "content" else ($ti.output_mode | if type == "string" then . else "files_with_matches" end) end)}
   else {kind: "none"} end)
  | [$shape, .kind, .command, .path, .glob, .pattern, .mode][] | (. // "") + "\u0000"
  end
end'

# Through a file, not a process substitution, so jq's exit status survives.
# jq's own message is dropped: it can quote the payload.
if ! jq -j "$normalise" <<<"$input" >"$tmp/fields" 2>/dev/null; then
  echo "secret-read-guard: could not parse hook payload; guard NOT enforcing" >&2
  exit 1
fi
fields=()
while IFS= read -r -d '' field; do
  fields+=("$field")
done <"$tmp/fields"
((${#fields[@]} == 7)) || exit 0
shape=${fields[0]}
kind=${fields[1]}
command=${fields[2]}
path=${fields[3]}
grep_glob=${fields[4]}
pattern=${fields[5]}
mode=${fields[6]}

# Files whose whole content is credentials. Kept narrow on purpose: a broad
# pattern that catches `.env.example` (a committed template of op:// refs, not
# secrets) would train people to work around the guard. /proc/<pid>/environ is
# a process's whole environment, so a Read of it is an env dump.
secret_path_re='(^|/)\.env(\.[A-Za-z0-9_-]+)*$|(^|/)\.envrc\.local$|\.aws/credentials|(^|/)\.netrc$|(^|/)id_(rsa|ed25519|ecdsa)$|\.pem$|\.p12$|\.pfx$|(^|/)proc/[^/]+/environ$'
# Grep globs spell the same files by pattern (`.env*`, `.env.*`, `*.env`), so
# they get this looser test on top of secret_path_re.
glob_secret_re='(^|[/*{,[])\.env($|[]*.?,}])|\.(pem|p12|pfx)($|[]*?,}])|\.netrc($|[]*?,}])|id_(rsa|ed25519|ecdsa)($|[]*?,}])|\.aws(/|$)'
# On a command line the same path is preceded by a space, quote, = or / (~ for
# `~/.netrc`), and followed by whitespace, a quote, a redirect or a pipe —
# never by a line anchor, so the path-anchored pattern above would silently
# match nothing here.
cmd_secret_re='(^|[[:space:]"'"'"'=/])\.env([[:space:]"'"'"';|&)>]|$|\.[A-Za-z0-9_-]+)|\.aws/credentials|(^|[[:space:]"'"'"'=/~])\.netrc([[:space:]"'"'"';|&)>]|$)|id_(rsa|ed25519|ecdsa)([[:space:]]|$)|\.(pem|p12|pfx)([[:space:]]|$)'
# Committed templates of op:// refs, not resolved values.
template_re='\.env(\.[A-Za-z0-9_-]+)*\.(example|template|sample|dist)'

# Command position: start of line, or after ; & | ( { ! — then any run of
# keywords/wrappers that run the next word as a command (`then env`, `sudo
# env`) or of `NAME=value` prefixes. The keywords count only there, so `echo do
# env` is an argument, not a command. Dumpers are matched only at this anchor,
# against text with quoted spans masked (mask_quotes), so `rg 'env|printenv|x'
# file` stays allowed while a bare `env` is denied.
cmd_prefix='((then|do|else|if|elif|while|until|sudo|command|exec|time|nohup)[[:space:]]+|[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*'
cmd_start='(^[[:space:]]*|[;&|({!]+[[:space:]]*)'"$cmd_prefix"
env_dump_re="$cmd_start"'(printenv|env)[[:space:]]*($|[;&|)])'
# `printenv NAME` prints just that value — fine for HOME, a leak for a key.
printenv_secret_re="$cmd_start"'printenv([[:space:]]+[^[:space:];&|)]+)*[[:space:]]+[A-Za-z_]*(API_?KEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE_KEY)'
# Listing/show forms that print a value without echoing it — the gap behind
# the LINEAR_API_KEY rotation. `-S`/`--show`/`-p` always print, name or not;
# the rest dump only when bare: `export NAME=v` or `declare -x NAME=v` just
# marks NAME and prints nothing.
#
# declare/typeset: bare, or flag-only unless every flag is f/F (those list
# functions, not variables), or any flag cluster carrying `p` anywhere
# (`declare -xp NAME` and `declare -x -p NAME` both print). Bare `export` dumps
# every exported variable, same as `export -p`.
declare_dump='(declare|typeset)((([[:space:]]+-[A-Za-z]+)*[[:space:]]+-[A-Za-z]*[A-EG-Za-eg-z][A-Za-z]*([[:space:]]+-[A-Za-z]+)*)?[[:space:]]*($|[;&|)])|([[:space:]]+-[A-Za-z]+)*[[:space:]]+-[A-Za-z]*p[A-Za-z]*)'
builtin_dump_re="$cmd_start"'(set([[:space:]]+(-S|--show)([[:space:]]|$|[;&|)])|[[:space:]]*($|[;&|)]))|'"$declare_dump"'|export([[:space:]]+-p([[:space:]]|$|[;&|)])|[[:space:]]*($|[;&|)]))|tmux[[:space:]]+show-environment([[:space:]]|$|[;&|)])|systemctl([[:space:]]+--user)?[[:space:]]+show-environment([[:space:]]|$|[;&|)])|launchctl[[:space:]]+getenv([[:space:]]|$|[;&|)]))'
# fish's scope flags (-x export, -g/-U/-l global/universal/local, -u unexport,
# -L) list that scope when no name follows; with a name they're the ordinary
# `set -gx PATH …` idiom. Checked only inside a confirmed `fish -c` body —
# bash's `set -x` is the harmless xtrace toggle.
fish_dump_re="$cmd_start"'set([[:space:]]+(-[xguUlL]+|--export|--global|--universal))+[[:space:]]*($|[;&|)])'
# A `-c` argument is quoted, so mask_quotes alone would erase a dumper the
# shell actually runs. This only locates where the argument starts (through
# the interpreter, its flags and trailing whitespace); decode_word extracts it.
# `-[A-Za-z]*c` covers clusters like `fish -ic` / `bash -lc`; `/` and `(` ahead
# of the name cover `/usr/bin/fish -c` and `(bash -c …)`.
shell_c_re="(^|[[:space:]/(])(fish|bash|sh|zsh)[[:space:]]+(-[A-Za-z]+[[:space:]]+)*-[A-Za-z]*c[[:space:]]+"
# Any read of /proc/<pid>/environ is a whole-environment dump, so this denies
# without a printing-tool gate. Tested against the raw command, quotes and all,
# which also catches it inside `fish -c '…'` without the -c extraction.
proc_environ_re="(^|[[:space:]\"'<=])/proc/[^[:space:]\"']*/environ"
nl=$'\n'

# Every awk pass below reads its text on stdin through printf '%s\n' and sees
# it one character at a time through feed(c), newlines included — one linear
# pass, where a bash character loop is quadratic and blew hookyard's 4 s budget
# on a 90 KB command. The newline printf adds is not data, so a record's
# newline is fed only once the next record starts. Characters come from 512-byte
# chunks because BWK awk (macOS) rescans the whole string on every substr.
# shellcheck disable=SC2016
awk_chars='
{
  if (NR > 1) feed("\n")
  line = $0
  while (line != "") {
    chunk = substr(line, 1, 512)
    line = substr(line, 513)
    n = length(chunk)
    for (i = 1; i <= n; i++) feed(substr(chunk, i, 1))
  }
}'

# mask(c) replaces every character inside a '...' or "..." span (quotes
# included) with a space, so quoted data can never look like a command.
# Positional, not a strip. Backslash-aware like bash: outside quotes `\"` is not
# a quote-open; inside "..." a `\"` does not close the span; '...' has no
# escapes.
awk_mask='
BEGIN { SQ = sprintf("%c", 39); DQ = "\""; BS = "\\" }
function mask(c) {
  if (esc) {
    esc = 0
    return (q == "" ? c : " ")
  }
  if (q == "") {
    if (c == BS) esc = 1
    else if (c == SQ || c == DQ) {
      q = c
      return " "
    }
    return c
  }
  if (c == q) q = ""
  else if (q == DQ && c == BS) esc = 1
  return " "
}'

mask_quotes() {
  printf '%s\n' "$1" | awk "$awk_mask$awk_chars"'
function feed(c) { printf "%s", mask(c) }'
}

# Extracts one shell WORD starting at index `start` of `s`: concatenated
# unquoted / '…' / "…" / $'…' segments with quote removal applied, stopping at
# the first unquoted word terminator — `bash -c 'echo '\''hi'\''; env'` is ONE
# argument. No expansion is attempted; the result is only ever re-scanned,
# never executed. The $ of $'…' is dropped, or it breaks the command-position
# anchor.
#
# Sets DECODED_WORD and DECODE_WORD_END (one past the last consumed index)
# instead of printing, so the caller can keep scanning past this word for a
# sibling `-c` body; a `$(...)` return would lose the second value.
awk_decode='
BEGIN { SQ = sprintf("%c", 39); DQ = "\""; BS = "\\"; STOP = " \t\n;&|()" }
function feed(c) {
  if (esc) {
    esc = 0
    pos++
    printf "%s", c
    return
  }
  if (st == "q") {
    pos++
    if (c == SQ) st = ""
    else printf "%s", c
    return
  }
  if (st != "") {
    pos++
    if (c == BS) esc = 1
    else if (c == (st == "d" ? DQ : SQ)) st = ""
    else printf "%s", c
    return
  }
  if (dollar) {
    dollar = 0
    if (c == SQ) {
      pos++
      st = "a"
      return
    }
    printf "$"
  }
  if (index(STOP, c)) exit
  pos++
  if (c == BS) esc = 1
  else if (c == "$") dollar = 1
  else if (c == SQ) st = "q"
  else if (c == DQ) st = "d"
  else printf "%s", c
}
END {
  if (dollar) printf "$"
  if (esc && st != "") printf "%s", BS
  printf "\n%d", pos
}'

decode_word() {
  local res
  res=$(printf '%s\n' "${1:$2}" | awk "$awk_decode$awk_chars")
  DECODED_WORD=${res%"$nl"*}
  DECODE_WORD_END=$(($2 + ${res##*"$nl"}))
}

# Credential-file content (rule 3), decided per simple command: split at
# unquoted ; & | ( ) ` and newline, so `ls .env && grep -n API .env` is judged
# on its grep alone and a quoted `cat .env` in a commit message is never a
# command. A segment denies when a printing tool, or grep/rg without a
# non-printing flag, sits at its command position and its raw text — template
# names stripped — names a credential file. Prints `print`, `grep` or nothing.
seg_start='^[[:space:]]*([{!][[:space:]]*)*'"$cmd_prefix"'([^[:space:]]*/)?'
awk_cred='
BEGIN { SPLIT = ";&|()`\n" }
function feed(c, was, m) {
  was = esc
  m = mask(c)
  if (!was && index(SPLIT, m)) {
    check()
    return
  }
  rb = rb c
  mb = mb m
  if (++nb == 512) flush()
}
function flush() {
  raw = raw rb
  masked = masked mb
  rb = mb = ""
  nb = 0
}
function check(r, m, verdict) {
  flush()
  r = raw
  m = masked
  raw = masked = ""
  if (m ~ ENVIRON["RE_PRINT"]) verdict = "print"
  else if (m ~ ENVIRON["RE_GREP"] && m !~ ENVIRON["RE_QUIET"]) verdict = "grep"
  else return
  gsub(ENVIRON["RE_TEMPLATE"], "", r)
  if (r !~ ENVIRON["RE_SECRET"]) return
  print verdict
  exit
}
END { check() }'

credential_read() {
  printf '%s\n' "$1" |
    RE_PRINT="$seg_start"'(cat|bat|head|tail|less|more|strings|xxd|od|nl|tac|rev|cut|paste|sed|awk|dotenv|source)([[:space:]]|$)' \
      RE_GREP="$seg_start"'(grep|rg|ripgrep|ag)([[:space:]]|$)' \
      RE_QUIET='(^|[[:space:]])(-[A-Za-z]*[cqlL][A-Za-z]*|--count|--quiet|--silent|--files-with-matches|--files-without-match)([[:space:]]|$)' \
      RE_TEMPLATE=$template_re RE_SECRET=$cmd_secret_re \
      awk "$awk_mask$awk_chars$awk_cred"
}

deny() {
  if [[ $shape == cursor ]]; then
    jq -cn --arg r "$1" '{permission: "deny", user_message: $r, agent_message: $r}'
  else
    jq -cn --arg r "$1" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}'
  fi
  exit 0
}

case $kind in
read)
  [[ -n $path ]] || exit 0
  [[ $path =~ $template_re ]] && exit 0
  [[ $path =~ $secret_path_re ]] || exit 0
  deny "Reading $path would print live credentials into this transcript, which costs a rotation. Nothing needs the value: the tool that consumes it reads the environment itself, and a missing key produces a clear error — run the tool and read that instead. To know a key is merely present without seeing it: grep -c '^NAME=' (count, not content)."
  ;;
grep)
  # Files-only and count modes never emit file content.
  [[ $mode == content ]] || exit 0
  # secret_path_re is anchored, so path and glob are tested separately. Template
  # names are stripped rather than exempting the call, so `{.env.example,.env}`
  # still trips on its `.env`.
  path_left=$(sed -E "s/$template_re//g" <<<"$path")
  glob_left=$(sed -E "s/$template_re//g" <<<"$grep_glob")
  if [[ $path_left =~ $secret_path_re ]]; then
    deny "Grepping $path for content would print credential lines into this transcript. To confirm a key exists, count in the shell (grep -c / rg -c) or list only the matching files, or run the consuming tool and read its error."
  fi
  if [[ $glob_left =~ $secret_path_re || $glob_left =~ $glob_secret_re ]]; then
    deny "Grepping with glob $grep_glob for content would print credential lines into this transcript. To confirm a key exists, count in the shell (grep -c / rg -c) or list only the matching files, or run the consuming tool and read its error."
  fi
  # A secret-shaped pattern with content output leaks even when the path is broad.
  if [[ $pattern =~ (API_?KEY|SECRET|TOKEN|PASSWORD|CREDENTIAL|PRIVATE_KEY) ]]; then
    deny "Pattern '$pattern' with content output will print any matching credential line it finds. List only the matching files, or count in the shell (grep -c / rg -c) — you need to know where a key is configured, not what it is."
  fi
  ;;
shell)
  [[ -n $command ]] || exit 0

  # 1. Expanding a secret-named variable into output — incident one, including
  #    the malformed-default form.
  if grep -qE '(echo|printf|print)\b[^|;&]*\$\{?[A-Za-z_]*(API_?KEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE_KEY)[A-Za-z_]*' <<<"$command"; then
    deny "This expands a secret-named variable into stdout, which writes the live value into this transcript (the exact shape that already forced one rotation). To test presence instead, branch on an -n test of the variable and echo only the words set or unset — never the variable itself."
  fi

  # 2. Dumping the environment or listing shell variables, at command position
  #    only, in the masked top level and in every `-c` body. A worklist, not a
  #    single variable: each match queues the extracted body (nesting) and the
  #    rest of the string after it (siblings: `bash -c 'true'; bash -c
  #    'declare -p NAME'`). Bounded by total matches, so it always terminates.
  raw_spaces=("$command")
  search_spaces=("$(mask_quotes "$command")")
  fish_spaces=()
  worklist=("$command")
  wi=0
  matches=0
  while ((wi < ${#worklist[@]})) && ((matches < 20)); do
    cur=${worklist[wi]}
    wi=$((wi + 1))
    [[ $cur =~ $shell_c_re ]] || continue
    match=${BASH_REMATCH[0]}
    interpreter=${BASH_REMATCH[2]}
    prefix=${cur%%"$match"*}
    decode_word "$cur" $((${#prefix} + ${#match}))
    sub=$DECODED_WORD
    masked_sub=$(mask_quotes "$sub")
    raw_spaces+=("$sub")
    search_spaces+=("$masked_sub")
    [[ $interpreter == fish ]] && fish_spaces+=("$masked_sub")
    matches=$((matches + 1))
    worklist+=("$sub" "${cur:DECODE_WORD_END}")
  done
  for space in "${search_spaces[@]}"; do
    if grep -qE "$env_dump_re" <<<"$space"; then
      deny "A bare env/printenv prints every secret in scope into this transcript. Name the one variable you need and test it without echoing its value."
    fi
    if grep -qE "$printenv_secret_re" <<<"$space"; then
      deny "printenv with a secret-named variable prints its live value into this transcript. To test presence: set -q NAME (fish), or branch on an -n test of the variable and echo only the words set or unset (bash/sh) — never the variable itself."
    fi
    if grep -qE "$builtin_dump_re" <<<"$space"; then
      deny "This lists or shows shell variables, which prints live values into this transcript — the same leak as env/printenv, just through a different command (set -S, declare -p, show-environment, …). To test presence: set -q NAME (fish), or branch on an -n test of the variable and echo only the words set or unset (bash/sh) — never the variable itself."
    fi
  done
  for space in "${fish_spaces[@]}"; do
    if grep -qE "$fish_dump_re" <<<"$space"; then
      deny "set with only scope flags (-x, -g, -U, …) and no name lists that scope's variables in fish, printing live values into this transcript — same leak as set -S. To test presence: set -q NAME."
    fi
  done

  if grep -qE "$proc_environ_re" <<<"$command"; then
    deny "This reads a process's environment table directly, which prints every secret in scope into this transcript — same leak as env/printenv, just via /proc instead. To test presence: set -q NAME (fish), or branch on an -n test of the variable and echo only the words set or unset (bash/sh) — never the variable itself."
  fi

  # 3. Reading a credential file's content. Non-printing inspection (test, [,
  #    git check-ignore, ls, stat, wc, chmod, rm, gtrash, direnv) stays allowed.
  #    Judged per simple command in the top level and every `-c` body.
  for space in "${raw_spaces[@]}"; do
    verdict=$(credential_read "$space")
    case $verdict in
    print)
      deny "This prints credential-file content into the transcript. If you need to confirm a key is configured, use grep -c '^NAME=' (a count), or run the consuming tool and read its error — a missing key fails loudly and that failure is the signal."
      ;;
    grep)
      deny "grep/rg over a credential file prints the matching line, value included. Add -c (count) or -q (quiet) if you only need to know whether it is set."
      ;;
    esac
  done
  ;;
esac

exit 0
