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
     # Read/Grep keys are from the shipped bundle, not a live capture, so
     # path/target_file stay as fallbacks for a build that spells them otherwise.
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
# Committed templates of op:// refs, not resolved values. A template counts
# only where its name ends, so `.env.examples` and `.env.example.local` are not
# templates.
template_re='\.env(\.[A-Za-z0-9_-]+)*\.(example|template|sample|dist)'

# Drops the template names from each line in one linear pass. sed -E with
# template_re does the same in quadratic time: the chain `.env.env.env…` is
# rescanned from every `.env` when a template name elsewhere in the text defeats
# its prefilter, and hookyard allows on a timeout. Here the line is split at its
# dots, and every `.env` piece inside one unbroken run of dotted words shares
# that run's last template word, so a run holds at most one match: from its
# first `.env` piece to that word — what sed -E takes, leftmost and longest.
#   wide=0: a word beginning example|template|sample|dist ends a name, and the
#           rest of that word stays (`.env.examples` leaves `s`).
#   wide=1: only a word that is exactly one of them, followed by a character
#           outside [A-Za-z0-9_.*?[-] or by the end of the line, ends a name —
#           the character stays.
# Splitting at "." is literal in every awk. The leading piece has no dot before
# it, so it is neither a start nor an end.
# shellcheck disable=SC2016
awk_strip='
# The words and the character class repeat template_re; a test holds them equal.
function tword(p) {
  if (substr(p, 1, 7) == "example") return 7
  if (substr(p, 1, 8) == "template") return 8
  if (substr(p, 1, 6) == "sample") return 6
  if (substr(p, 1, 4) == "dist") return 4
  return 0
}
function finish() {
  if (first && last > first) { cut[first] = last; drop[last] = len }
  first = last = 0
}
{
  n = split($0, P, ".")
  delete cut
  delete drop
  first = last = 0
  for (j = 2; j <= n; j++) {
    p = P[j]
    match(p, /^[A-Za-z0-9_-]*/)
    c = RLENGTH
    if (c == 0) { finish(); continue }
    full = (c == length(p))
    if (!first && p == "env") first = j
    w = tword(p)
    if (w && (!wide || (c == w && (full ? j == n : index("*?[", substr(p, c + 1, 1)) == 0)))) { last = j; len = w }
    if (!full) finish()
  }
  finish()
  printf "%s", P[1]
  stop = 0
  for (j = 2; j <= n; j++) {
    if (j in cut) stop = cut[j]
    if (!stop) printf ".%s", P[j]
    else if (j == stop) { printf "%s", substr(P[j], drop[j] + 1); stop = 0 }
  }
  printf "\n"
}'

# strip_templates <wide: 0|1> <text>
strip_templates() {
  awk -v wide="$1" "$awk_strip" <<<"$2"
}

# Command position: start of line, or after ; & | ( { — then any run of
# keywords/wrappers that run the next word as a command (`then env`, `! env`,
# `sudo -u root env`, `direnv exec . env`) or of `NAME=value` prefixes. The
# keywords count only there, so `echo do env` and `echo wow! env` are arguments,
# not commands. Dumpers are matched only at this anchor, against text with
# quoted spans masked (mask_cmd, mask_quotes), so `rg 'env|printenv|x' file`
# stays allowed while a bare `env` is denied. A masked argument is blank, so a
# wrapper's option argument is optional wherever the option could be argument-less;
# the converse over-denies (`env -u "X" cmd` reads cmd as -u's argument), which
# fails closed.
#
# wrap_word/wrap_rest keep `)` in the class: mask_cmd rewrites a backtick group
# to `(`…`)`, and a raw `$(...)` is left as code, so either can fill a wrapper's
# option argument (`sudo -u $(id -un) env`) instead of stopping the argument at
# its own closing paren.
wrap_word='[^[:space:];&|]+'
wrap_rest='[^[:space:];&|]*'
sudo_opts='(([[:space:]]+-[A-Za-z]*[ughpCDrtUTR][[:space:]]+'"$wrap_word"')|([[:space:]]+--(user|group|host|prompt|chdir|role|type|close-from|other-user|command-timeout)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
# env's options and NAME=value words: what remains when no command follows is a
# dump (`env -0`, `env -u X`, `env FOO=1`).
env_opts='(([[:space:]]+-[A-Za-z]*[uCSaP][[:space:]]+'"$wrap_word"')|([[:space:]]+--(unset|chdir|split-string|argv0|block-signal|default-signal|ignore-signal)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z0-9]'"$wrap_rest"')?)|([[:space:]]+[A-Za-z_][A-Za-z0-9_]*='"$wrap_rest"'))*'
# A slot-free wrapper's option: a dash flag, then optionally one following word
# that isn't itself a flag — its argument wherever the option has one,
# argument-less otherwise (same over-scan-toward-allow as sudo_opts/env_opts).
opt_arg='([[:space:]]+-[^[:space:];&|]*([[:space:]]+[^-[:space:];&|][^[:space:];&|]*)?)'
# ssh/container/kube take a target slot (host, container, pod) that must not be
# swallowed as a preceding option's argument, so their options are named
# explicitly, sudo_opts-style, instead of using opt_arg.
wrap_host='[^-[:space:];&|][^[:space:];&|]*'
ssh_opts='(([[:space:]]+-[A-Za-z]*[bcDEeFIiJLlmOoPpRSWw][[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
ctr_opts='(([[:space:]]+-[A-Za-z]*[ewuvplfcH][[:space:]]+'"$wrap_word"')|([[:space:]]+--(env|env-file|volume|workdir|user|name|network|entrypoint|publish|mount|platform|label|file|project-name|profile|context|host)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
kube_opts='(([[:space:]]+-[A-Za-z]*[cn][[:space:]]+'"$wrap_word"')|([[:space:]]+--(container|namespace|context|kubeconfig)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
cmd_prefix='((then|do|else|if|elif|while|until|!|command|exec|time|nohup|builtin)[[:space:]]+|(sudo|doas)'"$sudo_opts"'[[:space:]]+|env'"$env_opts"'[[:space:]]+|direnv[[:space:]]+exec[[:space:]]+('"$wrap_word"'[[:space:]]+)?|command([[:space:]]+-p)+[[:space:]]+|time([[:space:]]+-p)+[[:space:]]+|exec([[:space:]]+-[cl]+|[[:space:]]+-a[[:space:]]+'"$wrap_word"')+[[:space:]]+|timeout'"$opt_arg"'*[[:space:]]+[0-9][^[:space:];&|]*[[:space:]]+|(nice|ionice|stdbuf|setsid|xargs|watch)'"$opt_arg"'*[[:space:]]+|ssh'"${ssh_opts}"'[[:space:]]+'"${wrap_host}"'[[:space:]]+|(docker|podman)'"$ctr_opts"'([[:space:]]+compose'"$ctr_opts"')?[[:space:]]+(exec|run)'"${ctr_opts}"'[[:space:]]+'"${wrap_host}"'[[:space:]]+|docker-compose'"${ctr_opts}"'[[:space:]]+(exec|run)'"${ctr_opts}"'[[:space:]]+'"${wrap_host}"'[[:space:]]+|kubectl'"${kube_opts}"'[[:space:]]+exec'"${kube_opts}"'[[:space:]]+'"${wrap_host}""$kube_opts"'([[:space:]]+--)?[[:space:]]+|mise'"$opt_arg"'*[[:space:]]+(exec|x)([[:space:]]+[^[:space:];&|]+)*[[:space:]]+--[[:space:]]+|nix'"$opt_arg"'*[[:space:]]+(develop|shell)([[:space:]]+[^[:space:];&|]+)*[[:space:]]+(-c|--command)[[:space:]]+|[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*'
# A bare `)` never starts this anchor (`echo (re)set` stays prose): the case-arm
# reading requires being preceded by ^, ; & or ` in `, so an unrelated paren
# elsewhere on the line cannot seed it.
cmd_start='(^[[:space:]]*|[;&|({]+[[:space:]]*|(^|[;&]|[[:space:]]in[[:space:]])[[:space:]]*\(?[^[:space:]();&]+\)[[:space:]]*)'"$cmd_prefix"
# Where a bare dumper may end: a separator, a comment, a redirect (`env >&2`,
# `env 2>&1`, `env 2>/dev/null`), or stdin (`env <file`, `env <<EOF`) — every
# path still lands the dump somewhere legible. `<(` is a process substitution,
# not a redirect, so it is excluded.
dump_end='$|[;&|)#]|[0-9]+>|>&[[:space:]]*[0-9]|<([^(]|$)'
env_dump_re="$cmd_start"'(/[^[:space:];&|()]*/)?(printenv([[:space:]]+--?([A-Za-z0-9]'"$wrap_rest"')?)*|env'"$env_opts"')[[:space:]]*('"$dump_end"')'
# `printenv NAME` prints just that value — fine for HOME, a leak for a key.
printenv_secret_re="$cmd_start"'(/[^[:space:];&|()]*/)?printenv([[:space:]]+[^[:space:];&|]+)*[[:space:]]+[A-Za-z_]*(API_?KEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE_KEY)'
# Listing/show forms that print a value without echoing it — the gap behind
# the LINEAR_API_KEY rotation. `-S`/`--show`/`-p` always print, name or not;
# the rest dump only when bare: `export NAME=v` or `declare -x NAME=v` just
# marks NAME and prints nothing.
#
# declare/typeset: bare, or flag-only unless every flag is f/F (those list
# functions, not variables), or any flag cluster carrying `p` anywhere
# (`declare -xp NAME` and `declare -x -p NAME` both print). Bare `export` dumps
# every exported variable, same as `export -p`.
declare_dump='(declare|typeset)((([[:space:]]+-[A-Za-z]+)*[[:space:]]+-[A-Za-z]*[A-EG-Za-eg-z][A-Za-z]*([[:space:]]+-[A-Za-z]+)*)?[[:space:]]*('"$dump_end"')|([[:space:]]+-[A-Za-z]+)*[[:space:]]+-[A-Za-z]*p[A-Za-z]*)'
builtin_dump_re="$cmd_start"'(set([[:space:]]+(-S|--show)([[:space:]]|'"$dump_end"')|[[:space:]]*('"$dump_end"'))|'"$declare_dump"'|export([[:space:]]+-p([[:space:]]|'"$dump_end"')|[[:space:]]*('"$dump_end"'))|tmux[[:space:]]+show-environment([[:space:]]|'"$dump_end"')|systemctl([[:space:]]+--user)?[[:space:]]+show-environment([[:space:]]|'"$dump_end"')|launchctl[[:space:]]+getenv([[:space:]]|'"$dump_end"'))'
# fish's scope flags (-x export, -g/-U/-l global/universal/local, -u unexport,
# -L) list that scope when no name follows; with a name they're the ordinary
# `set -gx PATH …` idiom. Checked only inside a confirmed `fish -c` body —
# bash's `set -x` is the harmless xtrace toggle.
fish_dump_re="$cmd_start"'set([[:space:]]+(-[xguUlL]+|--export|--global|--universal))+[[:space:]]*('"$dump_end"')'
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

# Every awk pass below reads its text from a here-string and sees it one
# character at a time through feed(c), newlines included — one linear pass,
# where a bash character loop is quadratic and blew hookyard's 4 s budget on a
# 90 KB command. A here-string, not a pipe: there is no writer process, so an
# awk that exits early cannot SIGPIPE a pipefail'd writer and end the guard with
# no output — a silent allow. The newline the here-string adds is not data, so
# a record's newline is fed only once the next record starts. Characters come
# from 512-byte chunks because BWK awk (macOS) rescans the whole string on
# every substr.
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
  awk "$awk_mask$awk_chars"'
function feed(c) { printf "%s", mask(c) }' <<<"$1"
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
  res=$(awk "$awk_decode$awk_chars" <<<"${1:$2}")
  DECODED_WORD=${res%"$nl"*}
  DECODE_WORD_END=$(($2 + ${res##*"$nl"}))
}

# Rule 2's masker: what mask_quotes does, plus the shell structure it cannot see,
# so a quote character in prose cannot hide a later line and quoted code still
# shows. Every deviation from mask_quotes errs toward showing text (over-scan):
#   - `$'…'` is a quote whose `\'` does not close it.
#   - an unquoted `#` at word start begins a comment, emitted RAW with all quote,
#     heredoc and substitution openers suppressed to the newline — a comment
#     mis-detected (`a\ #`, `${x%% #*}`) can only over-scan, never hide.
#   - `$(` and backticks inside "…" open a frame whose text is code again; a
#     backtick is emitted as `(` … `)` so the command-start anchor sees it, in
#     double quotes and bare alike.
#   - a heredoc body (`<<[-]WORD`, several per line queue in order) is emitted
#     raw, quotes and `#` included, because bash treats them as literal there
#     and a body fed to a shell runs whole. Only an unquoted delimiter rewrites
#     backticks. The terminator line is not blanked: an arithmetic `1<<X` that
#     merely looks like a heredoc must not be able to hide a real `X` line.
# Length-preserving, one pass, like mask_quotes.
#
# W=1 (mask_cmd_wide) adds three readings on top, each of which could hide text
# the W=0 reading shows — so rule 2 searches both, never the wide one alone:
#   - a `#` right after an opening backtick starts a comment, as bash reads it.
#   - a heredoc body rewrites backticks, quoted delimiter or not — paired per
#     line, so no nesting and no span across lines.
#   - escaped backticks outside heredoc bodies nest to any depth: bash writes a
#     level-k backtick behind 2^(k-1) - 1 backslashes (0, 1, 3, 7, …). Inside a
#     backtick frame the same level closes it and any other level opens one.
#
# W=2 (mask_cmd_frames) is a third reading, searched beside the other two, that
# reads like W=0 outside backtick frames and treats each frame as opaque:
#   - a frame ends, as in bash's raw scan, at the first backtick behind an even
#     unbroken run of backslashes. A backslash consumes the next character,
#     newline included; quotes, comments, heredocs and `$(` do not matter.
#   - inside a frame every character shows except `\`, `'` and `"` (spaces) and
#     non-closing backticks (`;`), so nothing inside a frame can hide text.
#   - a backtick span in a heredoc delimiter word is shown the same way, and
#     that heredoc's body runs to end of input.
#   - a heredoc body emits every backtick as `;` and every backslash as a
#     space, so a dumper word right after a body code span (e.g. `then set
#     `X`) reads as a command — an accepted over-scan.
# shellcheck disable=SC2016
awk_mask_cmd='
BEGIN {
  SQ = sprintf("%c", 39); DQ = "\""; BS = "\\"; BT = "`"
  HDSTOP = " \t\n;&|()<>"
  sp = 0; pd = 0; hd_i = 0; hd_n = 0
}
function wordstart(p) { return p == "" || index(" \t\n;&|()", p) > 0 }
function push(k, saved) {
  sk[sp] = k; sv[sp] = saved; spd[sp] = pd
  sp++
  pd = 0
  q = ""
}
function pop() {
  sp--
  q = sv[sp]
  pd = spd[sp]
}
function tick(n,    k, r, lv) {
  k = 1
  r = n
  while (r % 2 == 1) { r = (r - 1) / 2; k++ }
  lv = (sp > 0 && (sk[sp - 1] == BT || sk[sp - 1] == "E")) ? slv[sp - 1] : 0
  if (lv == 0 && k > 1) return BT
  if (k == lv) { pop(); return ")" }
  push(k == 1 ? BT : "E", "")
  slv[sp - 1] = k
  opn = 1
  return "("
}
function frameout(c) {
  if (c == BS) { fb++; printf " "; return 0 }
  if (c == BT && fb % 2 == 0) { fb = 0; printf ")"; return 1 }
  fb = 0
  if (c == BT) printf ";"
  else if (c == SQ || c == DQ) printf " "
  else printf "%s", c
  return 0
}
function framefeed(c) {
  prev = c
  if (frameout(c)) { fr = 0; pop() }
}
function code(c,    d, o, n) {
  d = dl
  dl = 0
  o = " "
  if (W == 1) {
    wo = opn
    opn = 0
    if (c == BS) bsr++
    else { n = bsr; bsr = 0 }
  }
  if (esc) {
    esc = 0
    if (q == "") {
      o = c
      if (W == 1) {
        if (c == BT) o = tick(n)
        else if (c == BS && sp > 0 && (sk[sp - 1] == BT || sk[sp - 1] == "E")) o = " "
      }
      else if (!W && c == BT && sp > 0 && sk[sp - 1] == "E") { pop(); o = ")" }
      else if (!W && c == BT && sp > 0 && sk[sp - 1] == BT) { push("E", ""); o = "(" }
    }
  } else if (cm) {
    o = c
    if (c == "\n") cm = 0
  } else if (q == SQ) {
    if (c == SQ) q = ""
  } else if (q == "A") {
    if (c == BS) esc = 1
    else if (c == SQ) q = ""
  } else if (q == DQ) {
    if (c == BS) esc = 1
    else if (c == DQ) q = ""
    else if (d && c == "(") { push("(", DQ); o = "(" }
    else if (c == BT) {
      push(BT, DQ)
      o = "("
      if (W == 1) { slv[sp - 1] = 1; opn = 1 }
      else if (W == 2) { fr = 1; fb = 0 }
    }
    else if (c == "$") dl = 1
  } else {
    o = c
    if (c == BS) {
      esc = 1
      if (sp > 0 && (sk[sp - 1] == BT || sk[sp - 1] == "E")) o = " "
    } else if (c == SQ) { q = (d ? "A" : SQ); o = " " }
    else if (c == DQ) { q = DQ; o = " " }
    else if (c == BT) {
      if (W == 1) o = tick(n)
      else if (W == 2) { push(BT, ""); fr = 1; fb = 0; o = "(" }
      else if (sp > 0 && sk[sp - 1] == BT) { pop(); o = ")" }
      else { push(BT, ""); o = "(" }
    } else if (c == "(") pd++
    else if (c == ")") {
      if (pd > 0) pd--
      else if (sp > 0 && sk[sp - 1] == "(") pop()
    } else if (c == "#" && (wordstart(prev) || (W == 1 && wo))) cm = 1
    else if (c == "$") dl = 1
  }
  return o
}
function enterbody() {
  hd_i++
  body = 1
  bok = 1
  bpos = 0
  bbt = 0
}
function bodyfeed(c,    w, term) {
  w = hd_w[hd_i]
  if (c == "\n") {
    printf "\n"
    term = (bok && bpos == length(w))
    bok = 1
    bpos = 0
    bbt = 0
    if (term) {
      body = 0
      prev = "\n"
      if (hd_i < hd_n) enterbody()
    }
    return
  }
  if (bok) {
    if (hd_d[hd_i] && bpos == 0 && c == "\t") { }
    else if (bpos < length(w) && c == substr(w, bpos + 1, 1)) bpos++
    else bok = 0
  }
  if (W == 2 && (c == BT || c == BS)) { printf "%s", (c == BT ? ";" : " "); return }
  if (c == BT && (W || !hd_q[hd_i])) {
    bbt = !bbt
    printf "%s", (bbt ? "(" : ")")
    return
  }
  printf "%s", c
}
function feed(c,    arm, wasesc) {
  if (fr) { framefeed(c); return }
  if (body) { bodyfeed(c); return }
  if (hs == 3) {
    if (hbt) { if (frameout(c)) hbt = 0; return }
    if (hbs) { hbs = 0; hw = hw c; printf " "; return }
    if (W == 2 && c == BT && hq != SQ) { hbt = 1; fb = 0; hspan = 1; printf "("; return }
    if (hq != "") {
      if (c == hq) hq = ""
      else if (W == 2 && hq == DQ && c == BS) hbs = 1
      else hw = hw c
      printf " "
      return
    }
    if (c == BS) { hbs = 1; hquo = 1; printf " "; return }
    if (c == SQ || c == DQ) { hq = c; hquo = 1; printf " "; return }
    if (!index(HDSTOP, c)) { hw = hw c; printf " "; return }
    if (hw != "" || hquo || hspan) {
      hd_n++
      hd_w[hd_n] = hspan ? "\n" : hw
      hd_d[hd_n] = hdash
      hd_q[hd_n] = hquo
    }
    hs = 0
  } else if (hs == 2) {
    if (c == " " || c == "\t") { printf "%s", c; return }
    if (c == "-" && !hdash) { hdash = 1; printf " "; return }
    if (c == "<") { hs = 0; printf "<"; prev = c; return }
    if (index(HDSTOP, c)) hs = 0
    else { hs = 3; hw = ""; hq = ""; hbs = 0; hquo = 0; hbt = 0; hspan = 0; feed(c); return }
  }
  arm = (q == "" && !esc && !cm)
  wasesc = esc
  printf "%s", code(c)
  if (arm && c == "<") {
    if (hs == 1) { hs = 2; hdash = 0 }
    else hs = 1
  } else if (hs == 1) hs = 0
  if (c == "\n" && q == "" && !wasesc && hd_i < hd_n) enterbody()
  prev = c
}'

mask_cmd() {
  awk -v W=0 "$awk_mask_cmd$awk_chars" <<<"$1"
}

mask_cmd_wide() {
  awk -v W=1 "$awk_mask_cmd$awk_chars" <<<"$1"
}

mask_cmd_frames() {
  awk -v W=2 "$awk_mask_cmd$awk_chars" <<<"$1"
}

# Credential-file reads (rule 3): a credential file named anywhere in the
# space, plus anywhere a printing word, a printing command form, inline
# interpreter code, or a grep without a quiet flag. Co-occurrence, not
# parsing, on purpose: judging each command separately meant re-implementing
# the shell's tokeniser, and every place it disagreed with bash turned a read
# into an allow. So prose that only mentions a read is denied too — the
# accepted cost for a secret guard. The name test is grep -E; the rest is one
# awk pass, line by line, so it stays linear.
#   - Names (credential_read): cmd_secret_re after dropping template names
#     anywhere, or cmd_secret_wide_re (after < : { , ( ` and before globs,
#     braces and backticks too) after dropping them only where the name ends
#     (.env.examples and .env.example.local are not templates).
#   - Printing: the word list; $(<file); dot-sourcing; git show, cat-file,
#     blame, diff (unless --stat and the like, without a patch flag),
#     range-diff, log/reflog with a patch flag, add/checkout/commit/reset/
#     restore/stash with -p, format-patch --stdout, status/commit/stash -v;
#     date -f, file -f and --files0-from.
#   - Interpreters: given -c/-e/-p (a cluster ending in one), --eval, --print,
#     - or eval before their first operand, a heredoc or here-string, or a pipe
#     into them.
#   - grep: each grep word opens a stage that runs to the next ; & | ( ) or
#     backtick, whose quoted text is blanked and comment dropped; every grep in
#     it must carry a quiet flag, not counting words after -- or the argument
#     of -e/-f/-m/-A/-B/-C/-d/-D; ripgrep and ag get a stricter class. A CR,
#     VT or FF in the stage makes it loud: some shells split words there.
# Prints `print`, `interp`, `grep` or nothing.
#
# Regexes are literals (compiled once; BusyBox recompiles a dynamic one on
# every use). \047 is the apostrophe the single-quoted program cannot hold.
# shellcheck disable=SC2016
awk_cred='
BEGIN {
  SQ = sprintf("%c", 39); DQ = "\""; BS = "\\"
  n = split("add am apply branch check-attr check-ignore checkout cherry-pick clean clone commit describe fetch format-patch gc init log ls-files ls-remote ls-tree merge merge-base mv notes pull push reflog remote reset restore revert rev-list rev-parse rm shortlog sparse-checkout stash status switch symbolic-ref tag update-index whatchanged worktree", a, " ")
  for (i = 1; i <= n; i++) GIT[a[i]] = "B"
  n = split("show cat-file blame annotate diff diff-files diff-index diff-tree range-diff", a, " ")
  for (i = 1; i <= n; i++) GIT[a[i]] = "F"
  n = split("log whatchanged reflog", a, " ")
  for (i = 1; i <= n; i++) GLOG[a[i]] = 1
  n = split("add checkout commit reset restore stash", a, " ")
  for (i = 1; i <= n; i++) GHUNK[a[i]] = 1
}
{
  if ($0 ~ /(^|[^A-Za-z0-9_])(cat|bat|head|tail|less|more|strings|xxd|od|nl|tac|rev|cut|paste|sed|awk|dotenv|source)([^A-Za-z0-9_]|$)/ || $0 ~ /\$\([[:space:]]*</) P = 1
  if ($0 ~ /(^|[^|])\|([^;&|]*[^A-Za-z0-9_.;&|-])?(python[0-9.]*|pypy[0-9.]*|node|nodejs|bun|deno|ruby|perl|php|lua[0-9.]*|luajit|Rscript|osascript|pwsh)([^A-Za-z0-9_.-]|$)/) I = 1
  ns = split($0, SEG, /[;&|()`]/)
  for (s = 1; s <= ns; s++) segment(SEG[s])
}
function segment(g, W, nw, j, w, b) {
  if (g ~ /^[[:space:]]*(([{!]|then|do|else|if|elif|while|until)[[:space:]]+)*\.[[:space:]]/) P = 1
  if (match(g, /(^|[^A-Za-z0-9_])((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)([^A-Za-z0-9_]|$)/)) {
    G = 1
    if (substr(g, RSTART, 1) !~ /[A-Za-z0-9_]/) RSTART++
    if (grep_loud(substr(g, RSTART))) L = 1
  }
  nw = split(g, W, /[[:space:]]+/)
  mode = ""
  for (j = 1; j <= nw; j++) {
    w = W[j]
    if (w == "") continue
    b = w
    if (index(b, "/")) b = BP[split(b, BP, "/")]
    if (b == "git") { git_end(); mode = "git"; gs = ""; gskip = glog = ghunk = gpatch = gstat = gv = gso = 0; continue }
    if (b ~ /^(python[0-9.]*|pypy[0-9.]*|node|nodejs|bun|deno|ruby|perl|php|lua[0-9.]*|luajit|Rscript|osascript|pwsh)(<<.*)?$/) { git_end(); mode = "interp"; if (index(b, "<<")) I = 1; continue }
    if (b == "date" || b == "file") { git_end(); mode = "df"; continue }
    if (index(w, "--files0-from")) P = 1
    if (mode == "git") git_word(w)
    else if (mode == "interp") {
      if (index(w, "<<")) I = 1
      else if (w ~ /^(-[A-Za-z]*[ceEp]|--eval|--print|-|eval)$/) { I = 1; mode = "" }
      else if (w !~ /^-/) mode = "args"
    } else if (mode == "args") { if (index(w, "<<")) I = 1 }
    else if (mode == "df") { if (w ~ /^(-[A-Za-z]*f.*|--file(s-from)?(=.*)?)$/) P = 1 }
  }
  git_end()
}
# The first non-option word after git is its subcommand; -c, --config-env,
# -C, --git-dir, --work-tree and --namespace take an argument.
function git_word(w) {
  if (gs == "") {
    if (gskip) gskip = 0
    else if (w == "-c" || w == "--config-env" || w == "-C" || w == "--git-dir" || w == "--work-tree" || w == "--namespace") gskip = 1
    else if (w !~ /^-/) gs = w
    return
  }
  if (w ~ /^(-[pucL].*|-U[0-9]*|--patch|--patch-with-(stat|raw)|--unified(=.*)?|--cc|--dd|--remerge-diff|--diff-merges(=.*)?|--binary)$/) glog = 1
  if (w ~ /^(-p|--patch|-U[0-9]*|--unified(=.*)?|--binary)$/) ghunk = 1
  if (w ~ /^(-p|-u|-U[0-9]*|--patch.*|--unified(=.*)?)$/) gpatch = 1
  if (w ~ /^--(stat|numstat|shortstat|name-only|name-status|quiet|dirstat)(=.*)?$/) gstat = 1
  if (w ~ /^(-v+|--verbose)$/) gv = 1
  if (w == "--stdout") gso = 1
}
function git_end(c) {
  if (mode != "git" || gs == "") { mode = ""; return }
  mode = ""
  c = (gs in GIT) ? GIT[gs] : "U"
  if (c == "B" && ((gs in GLOG) && glog || (gs in GHUNK) && ghunk || gs == "format-patch" && gso || (gs == "status" || gs == "commit" || gs == "stash") && gv)) c = "F"
  if (c == "F" && gs ~ /^diff/ && gstat && !gpatch) c = "B"
  if (c == "F") P = 1
}
# 1 when a grep in stage s lacks a quiet flag (or no grep word survives the
# quote mask).
function grep_loud(s, m, W, n, j, x, e, rg, found) {
  m = mask(s)
  sub(/[[:space:]]#.*$/, "", m)
  if (m ~ /[\r\013\f]/) return 1
  gsub(/[0-9]*(>>?[|&]?|<[<>&]?<?-?)[[:space:]]*[^[:space:]]+/, " ", m)
  n = split(m, W, /[[:space:]]+/)
  for (j = 1; j <= n; j++) {
    if (W[j] !~ /^((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)$/) continue
    found = 1
    rg = (W[j] ~ /^(rg|ripgrep|ag)$/)
    for (e = j + 1; e <= n && W[e] != "+" && W[e] !~ /^((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)$/; e++) ;
    for (x = j + 1; x < e && W[x] != "--"; x++) ;
    for (j++; j < x; j++) {
      if (W[j] ~ /^-(-regexp|-file|[efmABCdD])$/ && j + 1 < x) j++
      else if (rg ? W[j] ~ /^(-[abFhHiInPsSuUvwxzN]*[cql][abFhHiInPsSuUvwxzNcql]*|--count|--quiet|--files-with-matches|--files-without-match)$/ : W[j] ~ /^(-[abEFGhHiInoPrRsTUvVwxyzZ]*[cqlL][abEFGhHiInoPrRsTUvVwxyzZcqlL]*|--count|--quiet|--silent|--files-with-matches|--files-without-match)$/) break
    }
    if (j >= x) return 1
    j = e - 1
  }
  return !found
}
# Quoted text (quotes included) blanked, backslash-aware as in mask_quotes.
function mask(s, out, ch, k, i, c, mb, q, esc) {
  out = q = ""
  while (s != "") {
    ch = substr(s, 1, 512)
    s = substr(s, 513)
    k = length(ch)
    mb = ""
    for (i = 1; i <= k; i++) {
      c = substr(ch, i, 1)
      if (esc) { esc = 0; mb = mb (q == "" ? c : " "); continue }
      if (q == "") {
        if (c == BS) esc = 1
        else if (c == SQ || c == DQ) { q = c; c = " " }
        mb = mb c
        continue
      }
      if (c == q) q = ""
      else if (q == DQ && c == BS) esc = 1
      mb = mb " "
    }
    out = out mb
  }
  return out
}
END {
  if (P) print "print"
  else if (I) print "interp"
  else if (G && L) print "grep"
}'

# On a command line the same path is preceded by a space, quote, = or / (~ for
# `~/.netrc`), and followed by whitespace, a quote, a redirect or a pipe —
# never by a line anchor, so the path-anchored pattern above would silently
# match nothing here.
cmd_secret_re='(^|[[:space:]"'"'"'=/])\.env([[:space:]"'"'"';|&)>]|$|\.[A-Za-z0-9_-]+)|\.aws/credentials|(^|[[:space:]"'"'"'=/~])\.netrc([[:space:]"'"'"';|&)>]|$)|id_(rsa|ed25519|ecdsa)([[:space:]]|$)|\.(pem|p12|pfx)([[:space:]]|$)'
# The same names as the shell also spells them: after < : { , ( or a
# backtick, and before a glob, a brace, a backtick or a redirect.
cmd_secret_wide_re='(^|[[:space:]"'"'"'=/<:{,(`])\.env([[:space:]"'"'"';|&)><*?[{},`]|$|\.[A-Za-z0-9_*?[{-])|(^|[[:space:]"'"'"'=/<:{,(`])\.envrc\.local([[:space:]"'"'"';|&)><*?[{},`]|$)|\.aws/credentials|(^|[[:space:]"'"'"'=/<:{,(`~])\.netrc([[:space:]"'"'"';|&)><*?[{},`]|$)|id_(rsa|ed25519|ecdsa)([[:space:]"'"'"';|&)><*?[{},`]|$)|\.(pem|p12|pfx)([[:space:]"'"'"';|&)><*?[{},`]|$)'

# Here-strings, not pipes: under pipefail a grep -q that exits early could
# fail its writer, and a failed test reads as "no name" — an allow.
credential_read() {
  local left wide
  case $1 in
  *.env* | *netrc* | *id_rsa* | *id_ed25519* | *id_ecdsa* | *.aws/credentials* | *.pem* | *.p12* | *.pfx*) ;;
  *) return 0 ;;
  esac
  if [[ $2 == narrow ]]; then
    left=$(strip_templates 0 "$1") || return
    grep -qE "$cmd_secret_re" <<<"$left" || {
      echo miss
      return 0
    }
  else
    wide=$(strip_templates 1 "$1") || return
    grep -qE "$cmd_secret_wide_re" <<<"$wide" || return 0
  fi
  awk "$awk_cred" <<<"$1"
}

deny() {
  if [[ $shape == cursor ]]; then
    jq -cn --arg r "$1" '{permission: "deny", user_message: $r, agent_message: $r}'
  else
    jq -cn --arg r "$1" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}'
  fi
  exit 0
}

check_dump_spaces() {
  local space
  for space in "$@"; do
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
}

check_fish_spaces() {
  local space
  for space in "$@"; do
    if grep -qE "$fish_dump_re" <<<"$space"; then
      deny "set with only scope flags (-x, -g, -U, …) and no name lists that scope's variables in fish, printing live values into this transcript — same leak as set -S. To test presence: set -q NAME."
    fi
  done
}

case $kind in
read)
  [[ -n $path ]] || exit 0
  [[ $path =~ (^|/)$template_re$ ]] && exit 0
  [[ $path =~ $secret_path_re ]] || exit 0
  deny "Reading $path would print live credentials into this transcript, which costs a rotation. Nothing needs the value: the tool that consumes it reads the environment itself, and a missing key produces a clear error — run the tool and read that instead. To know a key is merely present without seeing it: grep -c '^NAME=' (count, not content)."
  ;;
grep)
  # Files-only and count modes never emit file content.
  [[ $mode == content ]] || exit 0
  # secret_path_re is anchored, so path and glob are tested separately. Template
  # names are stripped rather than exempting the call, so `{.env.example,.env}`
  # still trips on its `.env`: once anywhere, and once only where the name ends,
  # so `.env.examples` trips too.
  path_any=$(strip_templates 0 "$path")
  glob_any=$(strip_templates 0 "$grep_glob")
  [[ $path_any =~ $secret_path_re ]] && deny "Grepping $path for content would print credential lines into this transcript. To confirm a key exists, count in the shell (grep -c / rg -c) or list only the matching files, or run the consuming tool and read its error."
  [[ $glob_any =~ $secret_path_re || $glob_any =~ $glob_secret_re ]] && deny "Grepping with glob $grep_glob for content would print credential lines into this transcript. To confirm a key exists, count in the shell (grep -c / rg -c) or list only the matching files, or run the consuming tool and read its error."
  # A secret-shaped pattern with content output leaks even when the path is broad.
  if [[ $pattern =~ (API_?KEY|SECRET|TOKEN|PASSWORD|CREDENTIAL|PRIVATE_KEY) ]]; then
    deny "Pattern '$pattern' with content output will print any matching credential line it finds. List only the matching files, or count in the shell (grep -c / rg -c) — you need to know where a key is configured, not what it is."
  fi
  # The wide strips run only after every cheaper deny above has missed.
  path_left=$(strip_templates 1 "$path")
  [[ $path_left =~ $secret_path_re ]] && deny "Grepping $path for content would print credential lines into this transcript. To confirm a key exists, count in the shell (grep -c / rg -c) or list only the matching files, or run the consuming tool and read its error."
  glob_left=$(strip_templates 1 "$grep_glob")
  [[ $glob_left =~ $secret_path_re || $glob_left =~ $glob_secret_re ]] && deny "Grepping with glob $grep_glob for content would print credential lines into this transcript. To confirm a key exists, count in the shell (grep -c / rg -c) or list only the matching files, or run the consuming tool and read its error."
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
  search_spaces=("$(mask_cmd "$command")" "$(mask_quotes "$command")")
  fish_spaces=()
  subs=()
  sub_fish=()
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
    masked_sub=$(mask_cmd "$sub")
    quoted_sub=$(mask_quotes "$sub")
    raw_spaces+=("$sub")
    search_spaces+=("$masked_sub" "$quoted_sub")
    subs+=("$sub")
    if [[ $interpreter == fish ]]; then
      fish_spaces+=("$masked_sub" "$quoted_sub")
      sub_fish+=(1)
    else
      sub_fish+=(0)
    fi
    matches=$((matches + 1))
    worklist+=("$sub" "${cur:DECODE_WORD_END}")
  done
  check_dump_spaces "${search_spaces[@]}"
  check_fish_spaces ${fish_spaces[@]+"${fish_spaces[@]}"}

  if grep -qE "$proc_environ_re" <<<"$command"; then
    deny "This reads a process's environment table directly, which prints every secret in scope into this transcript — same leak as env/printenv, just via /proc instead. To test presence: set -q NAME (fish), or branch on an -n test of the variable and echo only the words set or unset (bash/sh) — never the variable itself."
  fi

  # The wide masker adds to the base one and never replaces it; it runs last
  # because it costs one more awk pass per space.
  wide_spaces=("$(mask_cmd_wide "$command")")
  wide_fish_spaces=()
  for ((i = 0; i < ${#subs[@]}; i++)); do
    wide_sub=$(mask_cmd_wide "${subs[i]}")
    wide_spaces+=("$wide_sub")
    if ((sub_fish[i])); then wide_fish_spaces+=("$wide_sub"); fi
  done
  check_dump_spaces "${wide_spaces[@]}"
  check_fish_spaces ${wide_fish_spaces[@]+"${wide_fish_spaces[@]}"}

  # 3. Reading a credential file's content (credential_read, above deny). A
  #    failed check fails loud rather than allowing. Every space is judged by
  #    the narrow name test first; the wide strip runs only on the spaces that
  #    missed it, so a deny elsewhere never pays for it.
  missed=()
  for pass in narrow wide; do
    if [[ $pass == narrow ]]; then spaces=("${raw_spaces[@]}"); else spaces=("${missed[@]}"); fi
    for space in "${spaces[@]}"; do
      verdict=$(credential_read "$space" "$pass") || {
        echo "secret-read-guard: credential-read check failed; guard NOT enforcing" >&2
        exit 1
      }
      case $verdict in
      miss)
        missed+=("$space")
        ;;
      interp)
        deny "Inline interpreter code that names a credential file can print it into this transcript, and the guard cannot tell whether the code reads it. If it only writes or edits the file, run the code from a script file; to confirm a key is configured, use grep -c '^NAME=' (a count)."
        ;;
      print)
        deny "This prints credential-file content into the transcript. If you need to confirm a key is configured, use grep -c '^NAME=' (a count), or run the consuming tool and read its error — a missing key fails loudly and that failure is the signal."
        ;;
      grep)
        deny "grep/rg over a credential file prints the matching line, value included. Add -c (count) or -q (quiet) to every grep if you only need to know whether it is set."
        ;;
      esac
    done
  done

  # The frame reading differs from W=0 only at a backslash, quote, `#` or `<`
  # inside or around a backtick, so it runs only on text holding a backtick
  # and one of those. It runs after rule 3 so a large rule-3 deny stays inside
  # hookyard's 4 s budget (a timeout is an allow); each check denies on its
  # own, so the order only decides which deny fires.
  frame_spaces=()
  frame_fish_spaces=()
  if [[ $command == *'`'* && $command == *[\\\'\"#\<]* ]]; then frame_spaces+=("$(mask_cmd_frames "$command")"); fi
  for ((i = 0; i < ${#subs[@]}; i++)); do
    [[ ${subs[i]} == *'`'* && ${subs[i]} == *[\\\'\"#\<]* ]] || continue
    frame_sub=$(mask_cmd_frames "${subs[i]}")
    frame_spaces+=("$frame_sub")
    if ((sub_fish[i])); then frame_fish_spaces+=("$frame_sub"); fi
  done
  check_dump_spaces ${frame_spaces[@]+"${frame_spaces[@]}"}
  check_fish_spaces ${frame_fish_spaces[@]+"${frame_fish_spaces[@]}"}
  ;;
esac

exit 0
