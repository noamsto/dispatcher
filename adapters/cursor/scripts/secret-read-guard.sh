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
wrap_word='[^[:space:];&|)]+'
wrap_rest='[^[:space:];&|)]*'
sudo_opts='(([[:space:]]+-[A-Za-z]*[ughpCDrtUTR][[:space:]]+'"$wrap_word"')|([[:space:]]+--(user|group|host|prompt|chdir|role|type|close-from|other-user|command-timeout)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
# env's options and NAME=value words: what remains when no command follows is a
# dump (`env -0`, `env -u X`, `env FOO=1`).
env_opts='(([[:space:]]+-[A-Za-z]*[uCSaP][[:space:]]+'"$wrap_word"')|([[:space:]]+--(unset|chdir|split-string|argv0|block-signal|default-signal|ignore-signal)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z0-9]'"$wrap_rest"')?)|([[:space:]]+[A-Za-z_][A-Za-z0-9_]*='"$wrap_rest"'))*'
cmd_prefix='((then|do|else|if|elif|while|until|!|command|exec|time|nohup)[[:space:]]+|(sudo|doas)'"$sudo_opts"'[[:space:]]+|env'"$env_opts"'[[:space:]]+|direnv[[:space:]]+exec[[:space:]]+('"$wrap_word"'[[:space:]]+)?|[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*'
cmd_start='(^[[:space:]]*|[;&|({]+[[:space:]]*)'"$cmd_prefix"
env_dump_re="$cmd_start"'(printenv([[:space:]]+--?([A-Za-z0-9]'"$wrap_rest"')?)*|env'"$env_opts"')[[:space:]]*($|[;&|)#]|[0-9]+>)'
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
declare_dump='(declare|typeset)((([[:space:]]+-[A-Za-z]+)*[[:space:]]+-[A-Za-z]*[A-EG-Za-eg-z][A-Za-z]*([[:space:]]+-[A-Za-z]+)*)?[[:space:]]*($|[;&|)#])|([[:space:]]+-[A-Za-z]+)*[[:space:]]+-[A-Za-z]*p[A-Za-z]*)'
builtin_dump_re="$cmd_start"'(set([[:space:]]+(-S|--show)([[:space:]]|$|[;&|)#])|[[:space:]]*($|[;&|)#]))|'"$declare_dump"'|export([[:space:]]+-p([[:space:]]|$|[;&|)#])|[[:space:]]*($|[;&|)#]))|tmux[[:space:]]+show-environment([[:space:]]|$|[;&|)#])|systemctl([[:space:]]+--user)?[[:space:]]+show-environment([[:space:]]|$|[;&|)#])|launchctl[[:space:]]+getenv([[:space:]]|$|[;&|)#]))'
# fish's scope flags (-x export, -g/-U/-l global/universal/local, -u unexport,
# -L) list that scope when no name follows; with a name they're the ordinary
# `set -gx PATH …` idiom. Checked only inside a confirmed `fish -c` body —
# bash's `set -x` is the harmless xtrace toggle.
fish_dump_re="$cmd_start"'set([[:space:]]+(-[xguUlL]+|--export|--global|--universal))+[[:space:]]*($|[;&|)#])'
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
function code(c,    d, o) {
  d = dl
  dl = 0
  o = " "
  if (esc) {
    esc = 0
    if (q == "") {
      o = c
      if (c == BT && sp > 0 && sk[sp - 1] == "E") { pop(); o = ")" }
      else if (c == BT && sp > 0 && sk[sp - 1] == BT) { push("E", ""); o = "(" }
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
    else if (c == BT) { push(BT, DQ); o = "(" }
    else if (c == "$") dl = 1
  } else {
    o = c
    if (c == BS) {
      esc = 1
      if (sp > 0 && (sk[sp - 1] == BT || sk[sp - 1] == "E")) o = " "
    } else if (c == SQ) { q = (d ? "A" : SQ); o = " " }
    else if (c == DQ) { q = DQ; o = " " }
    else if (c == BT) {
      if (sp > 0 && sk[sp - 1] == BT) { pop(); o = ")" }
      else { push(BT, ""); o = "(" }
    } else if (c == "(") pd++
    else if (c == ")") {
      if (pd > 0) pd--
      else if (sp > 0 && sk[sp - 1] == "(") pop()
    } else if (c == "#" && wordstart(prev)) cm = 1
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
  if (c == BT && !hd_q[hd_i]) {
    bbt = !bbt
    printf "%s", (bbt ? "(" : ")")
    return
  }
  printf "%s", c
}
function feed(c,    arm, wasesc) {
  if (body) { bodyfeed(c); return }
  if (hs == 3) {
    if (hbs) { hbs = 0; hw = hw c; printf " "; return }
    if (hq != "") {
      if (c == hq) hq = ""
      else hw = hw c
      printf " "
      return
    }
    if (c == BS) { hbs = 1; hquo = 1; printf " "; return }
    if (c == SQ || c == DQ) { hq = c; hquo = 1; printf " "; return }
    if (!index(HDSTOP, c)) { hw = hw c; printf " "; return }
    if (hw != "" || hquo) {
      hd_n++
      hd_w[hd_n] = hw
      hd_d[hd_n] = hdash
      hd_q[hd_n] = hquo
    }
    hs = 0
  } else if (hs == 2) {
    if (c == " " || c == "\t") { printf "%s", c; return }
    if (c == "-" && !hdash) { hdash = 1; printf " "; return }
    if (c == "<") { hs = 0; printf "<"; prev = c; return }
    if (index(HDSTOP, c)) hs = 0
    else { hs = 3; hw = ""; hq = ""; hbs = 0; hquo = 0; feed(c); return }
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
  awk "$awk_mask_cmd$awk_chars" <<<"$1"
}

# Credential-file reads (rule 3): one linear lexer pass over each space.
# Quotes, $'…', "$(…)", backticks, subshells, process substitutions, case
# patterns, comments and heredocs are tracked, and the text is cut into
# pipelines of simple commands (stages). A stage is classed by its head word —
# the first word after { ! shell keywords, as a basename:
#   B  benign: never prints a file nor runs an argument (echo, ls, test, crew;
#      git and gh by subcommand)
#   C  copy: benign, but a credential file it copies or links taints the space
#   F  filter: prints what it reads (cat, head, `.`, git show/diff/blame, …)
#   G  grep family: prints matches unless each grep carries a quiet flag
#   U  anything else, or an assignment prefix — it may run its arguments
# A stage's verdict comes from its raw text, quotes included, so no wrapper
# list is needed: a printing word anywhere, an interpreter given inline code
# or stdin, a loud grep, or an F head that reads by itself. A stage names a
# credential file when the file's name is spelled in it (templates dropped).
# Deny when
#   - an F/G stage with a verdict names a file itself;
#   - a pipeline holding a U stage has a verdict and names a file anywhere
#     (`echo .env | xargs cat`, `echo cat .env | bash`);
#   - the space names a file anywhere and a non-benign stage with a verdict
#     expands something or is an interpreter (`f=.env; cat "$f"`, loops);
#   - a C stage copied a named file and any non-benign stage has a verdict.
# B stages are otherwise exempt, so prose that mentions a read (commit
# messages, PR bodies, echo text) is allowed. A heredoc body is data when no
# stage on its line is U, and code otherwise; the delimiter is found line by
# line, as the shell does, so no quote in a body leaks past it. Data inside a
# $(…) still reaches the stage that expands it (`echo "$(cat <<X …)" | bash`).
# Prints `print`, `grep` or nothing.
#
# Regexes are literals (compiled once; BusyBox recompiles a dynamic one on
# every use). \047 is the apostrophe the single-quoted program cannot hold.
# shellcheck disable=SC2016
awk_cred='
BEGIN {
  SQ = sprintf("%c", 39); DQ = "\""; BS = "\\"; BT = "`"
  sp = cd = 1; F[1] = "U"; K[1] = "top"; ws = 1; safe = 1
  n = split("echo printf test [ [[ ls stat wc file du df crew true false : cd pwd date sleep mkdir rmdir touch rm gtrash chmod chown realpath readlink basename dirname", a, " ")
  for (i = 1; i <= n; i++) CLS[a[i]] = "B"
  n = split("cp ln mv install rsync", a, " ")
  for (i = 1; i <= n; i++) CLS[a[i]] = "C"
  n = split("cat bat head tail less more strings xxd od nl tac rev cut paste tee sort uniq tr column fold .", a, " ")
  for (i = 1; i <= n; i++) CLS[a[i]] = "F"
  n = split("grep egrep fgrep zgrep ugrep zegrep zfgrep bzgrep xzgrep rg ripgrep ag", a, " ")
  for (i = 1; i <= n; i++) CLS[a[i]] = "G"
  n = split("add am apply branch check-attr check-ignore checkout cherry-pick clean clone commit describe fetch format-patch gc init log ls-files ls-remote ls-tree merge merge-base mv notes pull push reflog remote reset restore revert rev-list rev-parse rm shortlog sparse-checkout stash status switch symbolic-ref tag update-index whatchanged worktree", a, " ")
  for (i = 1; i <= n; i++) GIT[a[i]] = "B"
  n = split("show cat-file blame annotate diff diff-files diff-index diff-tree range-diff", a, " ")
  for (i = 1; i <= n; i++) GIT[a[i]] = "F"
  GIT["grep"] = "G"
  n = split("log whatchanged reflog", a, " ")
  for (i = 1; i <= n; i++) GLOG[a[i]] = 1
  n = split("add checkout commit reset restore stash", a, " ")
  for (i = 1; i <= n; i++) GHUNK[a[i]] = 1
  n = split("api attestation auth browse cache completion config gist gpg-key issue label org pr project release repo ruleset run search secret ssh-key status variable workflow", a, " ")
  for (i = 1; i <= n; i++) GH[a[i]] = "B"
}
function append(c) {
  rb = rb c
  if (++nb == 512) flush()
}
function data(c) {
  fb = fb c
  if (++nf == 512) flush()
}
function flush() {
  RAW[cd] = RAW[cd] rb
  DB = DB fb
  if (cd > 1) FULL[cd] = FULL[cd] rb fb
  rb = fb = ""
  nb = nf = 0
}
function emit(v) {
  print v
  done = 1
  exit
}
function head(r, w) {
  sub(/^([[:space:]]|[{!]|(then|do|else|elif|if|while|until|time)[[:space:]])*/, "", r)
  if (r ~ /^[A-Za-z_][A-Za-z0-9_]*=/) return "="
  match(r, /^[^[:space:]<>]*/)
  w = substr(r, 1, RLENGTH)
  HREST = substr(r, RLENGTH + 1)
  if (index(w, SQ) || index(w, DQ) || index(w, BS) || index(w, "$") || index(w, BT)) return ""
  sub(/.*\//, "", w)
  return w
}
# The first non-option word after git/gh. GITU: an option before it can run
# text (-c alias.x=!…, --config-env, --exec-path).
function subcmd(r, n, W, i, w) {
  GITU = 0
  n = split(r, W, /[[:space:]]+/)
  for (i = 1; i <= n; i++) {
    w = W[i]
    if (w == "") continue
    if (w == "-c" || w ~ /^--(config-env|exec-path)/) { GITU = 1; continue }
    if (w == "-C" || w == "--git-dir" || w == "--work-tree" || w == "--namespace" || w == "-R" || w == "--repo") { i++; continue }
    if (w ~ /^-/) continue
    return w
  }
  return ""
}
# GV: the class itself is a printing verdict (git show, dot-sourcing). The
# head is read from the first 1 KB of the stage: a longer run of prefixes leaves it
# unreadable, and so U.
function klass(r, h, s, c) {
  GV = 0
  h = head(substr(r, 1, 1024))
  if (h == "." || (h == "" && r ~ /^[[:space:]]*<[^<(&]/)) { GV = 1; return "F" }
  if (h == "git") {
    s = subcmd(HREST)
    c = (s in GIT) ? GIT[s] : "U"
    if (GITU) return "U"
    if (c == "B" && (s in GLOG) && r ~ /[[:space:]](-[pucL][^[:space:]]*|-U[0-9]*|--patch|--patch-with-(stat|raw)|--unified(=[^[:space:]]*)?|--cc|--dd|--remerge-diff|--diff-merges(=[^[:space:]]*)?|--binary)([[:space:]]|$)/) c = "F"
    if (c == "B" && (s in GHUNK) && r ~ /[[:space:]](-p|--patch|-U[0-9]*|--unified(=[^[:space:]]*)?|--binary)([[:space:]]|$)/) c = "F"
    if (c == "B" && s == "format-patch" && r ~ /[[:space:]]--stdout([[:space:]]|$)/) c = "F"
    if (c == "F" && s ~ /^diff/ && r ~ /[[:space:]]--(stat|numstat|shortstat|name-only|name-status|quiet|exit-code|dirstat)([[:space:]=]|$)/) c = "B"
    if (c == "F") GV = 1
    return c
  }
  if (h == "gh") return (subcmd(HREST) in GH) ? "B" : "U"
  return (h in CLS) ? CLS[h] : "U"
}
# Files whose whole content is credentials, as a command line spells them:
# after a space, quote, = / < : { , ( or backtick (~ for ~/.netrc), and before
# whitespace, a quote, an operator, a redirect, a glob or a brace — so .env*,
# {.env,.env.local}, HEAD:.env and cat<.env all count. A template name
# (.env.example, .env.local.sample) is dropped first, but only where the name
# ends: .env.examples and .env.example.local are not templates.
function names(r) {
  if (!index(r, ".env") && !index(r, "netrc") && !index(r, "id_") && !index(r, ".aws/credentials") && !index(r, ".p")) return 0
  gsub(/'"$template_re"'([^A-Za-z0-9_.*?[-]|$)/, " ", r)
  return r ~ /(^|[[:space:]"\047=\/<:{,(`])\.env([[:space:]"\047;|&)><*?[{},`]|$|\.[A-Za-z0-9_*?[{-])|(^|[[:space:]"\047=\/<:{,(`])\.envrc\.local([[:space:]"\047;|&)><*?[{},`]|$)|\.aws\/credentials|(^|[[:space:]"\047=\/<:{,(`~])\.netrc([[:space:]"\047;|&)><*?[{},`]|$)|id_(rsa|ed25519|ecdsa)([[:space:]"\047;|&)><*?[{},`]|$)|\.(pem|p12|pfx)([[:space:]"\047;|&)><*?[{},`]|$)/
}
# One grep is quiet when a count/quiet/files flag follows its word; a stage is
# loud when any grep in it is. A grep runs to the next ; & | ( ) or backtick,
# even a quoted one, and its words are read from there with quotes removed
# and quoted whitespace kept inside the word, so a flag inside a pattern
# ("x -c") is no flag. A comment, the words after "--" and the argument of
# -e/-f/-m/-A/-B/-C/-d/-D are not flags; a find "+" ends an -exec grep.
# ripgrep and ag differ (-r/-T/-E take an argument, -L means --follow), so
# they get a stricter class; a quiet letter may only follow letters that take
# no argument, or -iesecret would pass.
function grep_loud(r, n, S, i, p) {
  n = split(r, S, /[;&|()`]/)
  for (i = 1; i <= n; i++) {
    p = S[i]
    if (!match(p, /(^|[^A-Za-z0-9_])((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)([^A-Za-z0-9_]|$)/)) continue
    if (substr(p, RSTART, 1) !~ /[A-Za-z]/) RSTART++
    if (!quiet(substr(p, RSTART))) return 1
  }
  return 0
}
function quiet(s, m, mb, q, e, ch, k, i, c, n, W, j, a, x, rg, found) {
  m = q = ""
  while (s != "") {
    ch = substr(s, 1, 512)
    s = substr(s, 513)
    k = length(ch)
    mb = ""
    for (i = 1; i <= k; i++) {
      c = substr(ch, i, 1)
      if (e) e = 0
      else if (c == BS && q != SQ) { e = 1; continue }
      else if (q == "" && (c == SQ || c == DQ)) { q = c; continue }
      else if (c == q) { q = ""; continue }
      if (q != "" && (c == " " || c == "\t" || c == "\n")) c = "_"
      mb = mb c
    }
    m = m mb
  }
  sub(/[[:space:]]#.*$/, "", m)
  n = split(m, W, /[[:space:]]+/)
  for (j = 1; j <= n; j++) {
    if (W[j] !~ /^((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)$/) continue
    rg = (W[j] ~ /^(rg|ripgrep|ag)$/)
    for (a = j + 1; a <= n && W[a] != "+" && W[a] !~ /^((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)$/; a++) ;
    for (x = j + 1; x < a && W[x] != "--"; x++) ;
    for (j++; j < x; j++) {
      if (W[j] ~ /^-(-regexp|-file|[efmABCdD])$/) j++
      else if (rg ? W[j] ~ /^(-[abFhHiInPsSuUvwxzN]*[cql][abFhHiInPsSuUvwxzNcql]*|--count|--quiet|--files-with-matches|--files-without-match)$/ : W[j] ~ /^(-[abEFGhHiInoPrRsTUvVwxyzZ]*[cqlL][abEFGhHiInoPrRsTUvVwxyzZcqlL]*|--count|--quiet|--silent|--files-with-matches|--files-without-match)$/) break
    }
    if (j >= x) return 0
    j = a - 1
    found = 1
  }
  return found
}
# An interpreter counts when it is given code inline (-c, -e, -p, eval, -),
# on stdin (a heredoc or here-string) or from a pipe — not `python -m venv
# .env` or `node --env-file=.env app.js`.
function verdict(r, c) {
  if (GV) return "print"
  if (c == "G") return grep_loud(r) ? "grep" : ""
  if (r ~ /(^|[^A-Za-z0-9_])(cat|bat|head|tail|less|more|strings|xxd|od|nl|tac|rev|cut|paste|sed|awk|dotenv|source)([^A-Za-z0-9_]|$)/) return "print"
  if (r ~ /(^|[^A-Za-z0-9_])(python[0-9.]*|pypy[0-9.]*|node|nodejs|bun|deno|ruby|perl|php|lua[0-9.]*|luajit|Rscript|osascript|pwsh)([^A-Za-z0-9_]|$)/ && (r ~ /(^|[[:space:]])(-[A-Za-z]*[ceEp]|--eval|--print|-|eval)([[:space:]]|$)/ || index(r, "<<") || NS[cd] > 1)) return "interp"
  return grep_loud(r) ? "grep" : ""
}
function stage(r, c, own, v) {
  if (r !~ /[^[:space:]]/) return
  NS[cd]++
  c = klass(r)
  if (c == "U") {
    safe = 0; PU[cd] = 1; IU[cd] = 1
    if (r !~ /^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*=([^[:space:]()]|\([^()]*\))*[[:space:]]*)+$/) SU = 1
  }
  own = names(r)
  if (own) { PS[cd] = 1; T = 1; if (c == "C") TS = 1 }
  v = verdict(r, c)
  if (v != "") {
    if (PV[cd] == "") PV[cd] = v
    if (c == "B" || c == "C") { if (own) BW = 1 }
    else {
      if ((c == "F" || c == "G") && own) emit(v)
      if ((c == "U" || EXP[cd] || v == "interp") && VX == "") VX = v
      if (VA == "") VA = v
    }
  }
  if (T && VX != "") emit(VX)
  if (TS && VA != "") emit(VA)
  if (BW && SU) emit("print")
}
function stage_end() {
  flush()
  if (RAW[cd] ~ /^[[:space:]]*esac[[:space:]]*$/) CIN[cd] = CP[cd] = 0
  stage(RAW[cd])
  RAW[cd] = ""
  EXP[cd] = 0
}
function pipe_end() {
  if (PU[cd] && PS[cd] && PV[cd] != "") emit(PV[cd])
  PV[cd] = ""
  PU[cd] = PS[cd] = NS[cd] = 0
}
# AC: arithmetic $((…)), where << is a shift. PSUB: <(…) / >(…), whose U
# stages reach the outer pipeline.
function push_code(kind) {
  flush()
  AC[cd + 1] = (kind == "paren" && (prev == "(" || AC[cd]))
  PSUB[cd + 1] = (kind == "paren" && (prev == "<" || prev == ">"))
  cd++
  F[++sp] = "U"; K[sp] = kind
  RAW[cd] = FULL[cd] = PV[cd] = ""
  PU[cd] = PS[cd] = NS[cd] = IU[cd] = EXP[cd] = CIN[cd] = CP[cd] = 0
  ws = 1; lt = pp = dol = wl = 0
}
function pop_code(kind, inner, iu, ps) {
  stage_end()
  pipe_end()
  flush()
  inner = FULL[cd]
  iu = IU[cd]
  ps = PSUB[cd]
  kind = K[sp]
  sp--; cd--
  if (kind != "body") {
    inner = (kind == "tick" ? BT inner BT : "(" inner ")")
    if (F[sp] == "H") fb = fb inner
    else {
      RAW[cd] = RAW[cd] inner
      if (cd > 1) FULL[cd] = FULL[cd] inner
      if (ps && iu) PU[cd] = 1
    }
  }
  if (iu) IU[cd] = 1
  ws = lt = pp = dol = wl = 0
}
# `case WORD in` starts pattern mode (CP), where ) ends a pattern instead of
# a frame; ;; resumes it, and esac ends the case (CIN).
function word_end() {
  if (wl == 2 && w1 == "i" && w2 == "n" && !CP[cd] && !CIN[cd]) {
    flush()
    if (RAW[cd] ~ /^[[:space:]]*(([{!]|then|do|else|elif|if|while|until)[[:space:]]+)*case[[:space:]]/) CIN[cd] = CP[cd] = 1
  }
  wl = 0
}
function sep(c) {
  word_end()
  if (c == ";" && prev == ";" && CIN[cd]) CP[cd] = 1
  stage_end()
  ws = 1; lt = dol = 0
  if (c == "|") {
    if (pp && prev == "|") { pp = 0; pipe_end() } else pp = 1
    return
  }
  if (c == "&" && pp && prev == "|") return
  if (c == "\n" && pp) { newline(); return }
  pp = 0
  pipe_end()
  if (c == "\n") newline()
}
function newline(i) {
  if (inb) return
  if (!nq) { safe = 1; return }
  for (i = 1; i < cd; i++) if (klass(RAW[i]) == "U") safe = 0
  inb = 1; bsp = sp; bsafe = safe; bi = 1
  open_body()
}
function open_body() {
  dl = QD[bi]; ds = QS[bi]; lb = DB = ""; lok = 1
  if (bsafe) F[++sp] = QQ[bi] ? "L" : "H"
  else push_code("body")
}
function end_body() {
  if (bsafe) {
    flush()
    GV = 0
    if (names(DB) && verdict(DB, "B") != "") BW = 1
    if (BW && SU) emit("print")
    DB = ""
  }
  while (sp > bsp) { if (F[sp] == "U") pop_code(); else sp-- }
  esc = cmt = hdc = lt = pp = dol = wl = 0; ws = 1
  if (++bi <= nq) { open_body(); return }
  inb = nq = 0; safe = 1
}
function hd_char(c) {
  if (sp == hsp && F[sp] == "U" && !esc) {
    if (hdf && c == "-") { hds = 1; hdf = 0; return }
    hdf = 0
    if (c == " " || c == "\t") { if (hdw) hd_done(); return }
    if (index(";&|()<>\n", c)) { if (hdw) hd_done(); else hdc = 0; return }
  }
  hdr = hdr c; hdw = 1
}
function hd_done(q, k) {
  q = hdr
  k = gsub(SQ, "", q) + gsub(DQ, "", q) + gsub(/\\/, "", q)
  QD[++nq] = q; QS[nq] = hds; QQ[nq] = (k > 0)
  hdc = 0
}
# A lone & ends a command, but &> and >& are redirects, so its meaning waits
# for the next character (AMP). >| is a redirect too, not a pipe.
function code_char(c) {
  if (cmt) { if (c != "\n") return; cmt = 0 }
  if (amp) {
    amp = 0
    if (c == ">") append("&")
    else sep("&")
  }
  if (esc) { esc = 0; append(c); ws = dol = 0; return }
  if (c == "<") { lt++; append(c); ws = pp = dol = 0; wl = 3; return }
  if (lt == 2 && !inb && !AC[cd]) { hdc = 1; hdw = hds = 0; hdr = ""; hdf = 1; hsp = sp; hd_char(c) }
  lt = 0
  if (c == BS) { esc = 1; append(c); ws = pp = dol = 0; return }
  if (c == "#" && ws) { cmt = 1; return }
  if (CP[cd]) {
    if (c == "|") { append(c); ws = 1; return }
    if (c == "(") { flush(); if (RAW[cd] !~ /[^[:space:]]/) { append(c); return } }
    if (c == ")") {
      flush()
      if (RAW[cd] !~ /^[[:space:]]*esac[[:space:]]*$/) { CP[cd] = 0; sep(";"); return }
    }
  }
  if (c == "&") {
    if (prev == ">" || prev == "<") { append(c); ws = pp = dol = 0; wl = 3; return }
    if (pp && prev == "|") { sep(c); return }
    amp = 1
    return
  }
  if (c == "|" && prev == ">") { append(c); ws = pp = dol = 0; wl = 3; return }
  if (index(";|\n", c)) { sep(c); return }
  if (c == "(") { word_end(); push_code("paren"); return }
  if (c == ")") {
    word_end()
    flush()
    if (RAW[cd] ~ /^[[:space:]]*esac[[:space:]]*$/) CIN[cd] = CP[cd] = 0
    if (K[sp] == "paren") pop_code(); else sep(c)
    return
  }
  if (c == BT) {
    word_end()
    if (K[sp] == "tick") pop_code()
    else { EXP[cd] = 1; push_code("tick") }
    return
  }
  append(c)
  if (c == SQ) { F[++sp] = dol ? "A" : "S"; wl = 3 }
  else if (c == DQ) { F[++sp] = "D"; wl = 3 }
  if (c == " " || c == "\t") { word_end(); ws = 1; dol = 0; return }
  if (c == "$") EXP[cd] = 1
  if (++wl == 1) w1 = c
  else if (wl == 2) w2 = c
  ws = pp = 0
  dol = (c == "$")
}
function feed(c, m, e) {
  if (done) return
  if (inb) {
    if (c == "\n") {
      if (lok && lb == dl) { end_body(); prev = c; return }
      lb = ""; lok = 1
    } else if (lok && !(ds && lb == "" && c == "\t")) {
      lb = lb c
      if (length(lb) > length(dl)) lok = 0
    }
  }
  if (hdc) hd_char(c)
  m = F[sp]
  if (m == "U") code_char(c)
  else if (m == "S") { append(c); if (c == SQ) sp-- }
  else if (m == "A" || m == "D") {
    e = esc
    if (esc) { esc = 0; append(c) }
    else if (c == BS) { esc = 1; append(c) }
    else if (m == "D" && c == "(" && dol) push_code("paren")
    else if (m == "D" && c == BT) { EXP[cd] = 1; push_code("tick") }
    else {
      append(c)
      if (c == (m == "A" ? SQ : DQ)) sp--
      else if (m == "D" && c == "$") EXP[cd] = 1
    }
    dol = (m == "D" && c == "$" && !e)
  } else {
    e = esc
    if (m == "H" && esc) { esc = 0; data(c) }
    else if (m == "H" && c == BS) { esc = 1; data(c) }
    else if (m == "H" && c == "(" && dol) push_code("paren")
    else if (m == "H" && c == BT) push_code("tick")
    else data(c)
    dol = (m == "H" && c == "$" && !e)
  }
  prev = c
}
END {
  if (done) exit
  while (sp > 1) { if (F[sp] == "U") pop_code(); else sp-- }
  word_end()
  stage_end()
  pipe_end()
}'

credential_read() {
  case $1 in
  *.env* | *netrc* | *id_rsa* | *id_ed25519* | *id_ecdsa* | *.aws/credentials* | *.pem* | *.p12* | *.pfx*) ;;
  *) return 0 ;;
  esac
  awk "$awk_cred$awk_chars" <<<"$1"
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
  [[ $path =~ (^|/)$template_re$ ]] && exit 0
  [[ $path =~ $secret_path_re ]] || exit 0
  deny "Reading $path would print live credentials into this transcript, which costs a rotation. Nothing needs the value: the tool that consumes it reads the environment itself, and a missing key produces a clear error — run the tool and read that instead. To know a key is merely present without seeing it: grep -c '^NAME=' (count, not content)."
  ;;
grep)
  # Files-only and count modes never emit file content.
  [[ $mode == content ]] || exit 0
  # secret_path_re is anchored, so path and glob are tested separately. Template
  # names are stripped rather than exempting the call, so `{.env.example,.env}`
  # still trips on its `.env`.
  path_left=$(sed -E "s/($template_re)([^A-Za-z0-9_.*?[-])/\\4/g; s/($template_re)\$//" <<<"$path")
  glob_left=$(sed -E "s/($template_re)([^A-Za-z0-9_.*?[-])/\\4/g; s/($template_re)\$//" <<<"$grep_glob")
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
  search_spaces=("$(mask_cmd "$command")" "$(mask_quotes "$command")")
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
    masked_sub=$(mask_cmd "$sub")
    quoted_sub=$(mask_quotes "$sub")
    raw_spaces+=("$sub")
    search_spaces+=("$masked_sub" "$quoted_sub")
    [[ $interpreter == fish ]] && fish_spaces+=("$masked_sub" "$quoted_sub")
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

  # 3. Reading a credential file's content: the credential_read lexer (see its
  #    comment above deny). A failed lexer run fails loud rather than allowing.
  for space in "${raw_spaces[@]}"; do
    verdict=$(credential_read "$space") || {
      echo "secret-read-guard: credential-read check failed; guard NOT enforcing" >&2
      exit 1
    }
    case $verdict in
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
  ;;
esac

exit 0
