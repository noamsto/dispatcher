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
# Blocks the printing paths only. Dumping the environment counts even into a
# file, since a later command prints it; writing, testing existence, ignoring and
# deleting a credential file stay allowed — `test -f .env`, direnv, update-env
# are ordinary work.
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
# pre-tool-use schema. cursor: hookyard captures. cursor preToolUse Read/Grep
# keys are a live capture from cursor-agent 2026.09.26-dd393fe on 2026-09-27
# (tests/fixtures/cursor-pretool-read.json, tests/fixtures/cursor-pretool-grep.json).
# Allow is always no stdout, exit 0. Stdout carries exactly one deny object or
# nothing: cursor blocks the call on any non-JSON stdout, so a stray echo here
# would block every cursor tool call.
#
# A payload the guard cannot parse fails open but loud — exit 1, message on
# stderr — rather than closed: blocking every call on a broken host would stop
# every worker, and a non-zero hook exit is shown by every engine while the call
# proceeds.
#
# Runtime: runs standalone as `bash adapters/core/secret-read-guard.sh` with the
# hook JSON on stdin, inside or outside a dispatcher session. Needs bash >= 4
# (arrays, `[[ =~ ]]`, `${s:i}`, mapfile), jq, any POSIX awk (gawk, mawk, nawk/BWK
# and BusyBox are all exercised by the tests), grep -E and coreutils
# mktemp/cat/rm; it exports LC_ALL=C itself. PATH is required (command lookup);
# TMPDIR is optional (mktemp, default /tmp); no dispatcher variable (CREW_*) is
# read. There is no internal time budget: a hook timeout is an allow, so every
# pass is linear in command length — that is the guarantee.

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
     # Live capture (cursor-agent 2026.09.26-dd393fe, 2026-09-27): Read and
     # Grep send tool_input.file_path; Grep also sends pattern and omits
     # output_mode and glob. path/target_file stay as fallbacks.
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

# Threat model: rule 2 is a seatbelt for an accidental worker (A)
# who dumps its environment out of habit, not a boundary against a hostile
# one (B) — B already holds the secrets in its own environment and has
# unbounded non-dumper paths to them (interpreters, /proc, ps, eval, scripts).
#
# In scope: S1 a canonical dumper word, optionally /-rooted,
# at any bash command position, case arms included. S2 the canonical-idiom
# wrapper list, grown only when an idiom is seen in use. S3 a separator,
# comment, redirect or stdin ending, heredocs included. S4 a
# backslash-newline continuation splitting the dump. S5 the shell structure
# the masker already models. S6 a `#` misread as a comment, covered by the
# no-comment (J) reading; a command mixing a real comment with a misread one
# is an accepted limit.
#
# S7 escapes and quote splicing in the interpreter word, the -c flag, a
# credential name, a /proc path and a dumper word: the dequoted view (dequote)
# and the escape-stripped space (strip_escapes) read them as bash does. S8 a
# `-c` body anywhere in the raw text — after a misread quote or comment, in a
# here-string or a pipe into a shell, in a quoted argument — found by the raw
# finder beside the dequoted one.
#
# Out of scope: O1 word-forming obfuscation — variables, eval, aliases and
# ANSI-C escape decoding. O2 expansion-dependent structure — a
# substitution expanding to nothing (`env $(true)`). O3 lexer precision
# beyond the masker's model — quotes in "${x#...}", an escaped quote in a
# $'...' heredoc delimiter, a case nested in a double-quoted $(...). O4
# non-dumper paths, and a quoted ssh remote command (`ssh h '…'`).
#
# A spelling in O1-O4 is not a finding — cite this block instead of filing it.
#
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
# wrap_word/wrap_rest accept `)`, so a `$(...)` or backtick group (masked to
# `(`…`)`) can be a wrapper's option argument: `sudo -u $(id -un) env`.
wrap_word='[^[:space:];&|]+'
wrap_rest='[^[:space:];&|]*'
sudo_opts='(([[:space:]]+-[A-Za-z]*[ughpCDrtUTR][[:space:]]+'"$wrap_word"')|([[:space:]]+--(user|group|host|prompt|chdir|role|type|close-from|other-user|command-timeout)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
# env's options and NAME=value words: what remains when no command follows is a
# dump (`env -0`, `env -u X`, `env FOO=1`).
env_opts='(([[:space:]]+-[A-Za-z]*[uCSaP][[:space:]]+'"$wrap_word"')|([[:space:]]+--(unset|chdir|split-string|argv0|block-signal|default-signal|ignore-signal)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z0-9]'"$wrap_rest"')?)|([[:space:]]+[A-Za-z_][A-Za-z0-9_]*='"$wrap_rest"'))*'
# A slot-free wrapper's option: a flag, optionally followed by one non-flag word
# read as its argument.
opt_arg='([[:space:]]+-[^[:space:];&|]*([[:space:]]+[^-[:space:];&|][^[:space:];&|]*)?)'
# ssh/container/kube take a target slot (host, container, pod) that must not be
# swallowed as a preceding option's argument, so their options are named
# explicitly, sudo_opts-style, instead of using opt_arg.
wrap_host='[^-[:space:];&|][^[:space:];&|]*'
ssh_opts='(([[:space:]]+-[A-Za-z]*[bcDEeFIiJLlmOoPpRSWw][[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
ctr_opts='(([[:space:]]+-[A-Za-z]*[ewuvplfcH][[:space:]]+'"$wrap_word"')|([[:space:]]+--(env|env-file|volume|workdir|user|name|network|entrypoint|publish|mount|platform|label|file|project-name|profile|context|host)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
kube_opts='(([[:space:]]+-[A-Za-z]*[cn][[:space:]]+'"$wrap_word"')|([[:space:]]+--(container|namespace|context|kubeconfig)[[:space:]]+'"$wrap_word"')|([[:space:]]+--?([A-Za-z]'"$wrap_rest"')?))*'
cmd_prefix='((then|do|else|if|elif|while|until|!|command|exec|time|nohup|builtin)[[:space:]]+|(sudo|doas)'"$sudo_opts"'[[:space:]]+|env'"$env_opts"'[[:space:]]+|direnv[[:space:]]+exec[[:space:]]+('"$wrap_word"'[[:space:]]+)?|command([[:space:]]+(-p|--))+[[:space:]]+|time([[:space:]]+-p)+[[:space:]]+|exec([[:space:]]+-[cl]+|[[:space:]]+-a[[:space:]]+'"$wrap_word"')+[[:space:]]+|timeout'"$opt_arg"'*[[:space:]]+[0-9][^[:space:];&|]*[[:space:]]+|(nice|ionice|stdbuf|setsid|xargs|watch)'"$opt_arg"'*[[:space:]]+|ssh'"${ssh_opts}"'[[:space:]]+'"${wrap_host}"'[[:space:]]+|(docker|podman)'"$ctr_opts"'([[:space:]]+compose'"$ctr_opts"')?[[:space:]]+(exec|run)'"${ctr_opts}"'[[:space:]]+'"${wrap_host}"'[[:space:]]+|docker-compose'"${ctr_opts}"'[[:space:]]+(exec|run)'"${ctr_opts}"'[[:space:]]+'"${wrap_host}"'[[:space:]]+|kubectl'"${kube_opts}"'[[:space:]]+exec'"${kube_opts}"'[[:space:]]+'"${wrap_host}""$kube_opts"'([[:space:]]+--)?[[:space:]]+|mise'"$opt_arg"'*[[:space:]]+(exec|x)([[:space:]]+[^[:space:];&|]+)*[[:space:]]+--[[:space:]]+|nix'"$opt_arg"'*[[:space:]]+(develop|shell)([[:space:]]+[^[:space:];&|]+)*[[:space:]]+(-c|--command)[[:space:]]+|[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+)*'
# A case arm (`x) env`) is a command start only at line start or after ; & or
# `in`. A bare `)` is not: heredoc prose such as `(re)set` is scanned raw.
cmd_start='(^[[:space:]]*|[;&|({]+[[:space:]]*|(^|[;&]|[[:space:]]in[[:space:]])[[:space:]]*\(?[^[:space:]();&]+\)[[:space:]]*)'"$cmd_prefix"
# Where a bare dumper may end: a separator, a comment, an output redirect
# (`env >&2`, `env 2>&1`, `env >/tmp/x`, `env &>f`, `env {fd}>&2`), or stdin
# (`env <file`, `env <<EOF`) — every path still lands the dump somewhere
# legible, and a redirect to a file leaves it there for a later command to
# print. A `>` must follow a space, an fd number or `{fd}` (or open `>&`), and
# a `<` a space, so prose placeholders (`X=<empty>`) are not redirects; `<(` is
# a process substitution.
dump_end='$|[;&|)#]|[[:space:]][0-9]*>|[0-9]+>|>&|\{[A-Za-z_][A-Za-z0-9_]*\}[<>]|[[:space:]]<[^(]'
# A dumper path starts path-like, so the shebang in heredoc text
# (`#!/usr/bin/env -S bash`) is not read as a dumper path. An absolute one may
# hold `=` (`/nix/store/x-a=b/bin/env`), since no assignment starts with `/`.
# One starting with a name is a path unless the word is a bash assignment —
# `NAME=`, `NAME+=`, `NAME[SUB]=` or `NAME[SUB]+=` — so `CONFIG=deploy/env` and
# `a[1]+=deploy/env` are not paths, but `./a=b/env`, `a/b=c/env`,
# `a[1]x=b/env` and an unclosed `a[/env` are. SUB must hold no `[`, `]`, `\`,
# backtick or `$`: bash's subscript scan nests brackets and skips escapes and
# backtick/`${…}` spans, so only then is the first `]` the one bash closes on.
# Any of them reads as a path even where bash assigns (`a[b[1]]=x/env`,
# `a[$i]=x/env`), failing closed. Quoting is not modelled: a quoted `=`, `[` or
# `]` counts as bare.
# shellcheck disable=SC2016
path_pfx='(/[^[:space:];&|()]*/|[0-9.~][^[:space:];&|()]*/|[A-Za-z_][A-Za-z0-9_]*([^[:space:]A-Za-z0-9_=+[;&|()][^[:space:];&|()]*|\+([^=[:space:];&|()][^[:space:];&|()]*)?|\[[^][\\`$[:space:];&|()]*([[\\`$][^[:space:];&|()]*|\](\+([^=[:space:];&|()][^[:space:];&|()]*)?|[^=+[:space:];&|()][^[:space:];&|()]*)?)?)?/)?'
env_dump_re="$cmd_start""$path_pfx"'(printenv([[:space:]]+--?([A-Za-z0-9]'"$wrap_rest"')?)*|env'"$env_opts"')[[:space:]]*('"$dump_end"')'
# `printenv NAME` prints just that value — fine for HOME, a leak for a key.
printenv_secret_re="$cmd_start""$path_pfx"'printenv([[:space:]]+[^[:space:];&|]+)*[[:space:]]+[A-Za-z_]*(API_?KEY|SECRET|TOKEN|PASSWORD|PASSWD|CREDENTIAL|PRIVATE_KEY)'
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
builtin_dump_re="$cmd_start"'(set([[:space:]]+(-S|--show)([[:space:]]|'"$dump_end"')|[[:space:]]*('"$dump_end"'))|'"$declare_dump"'|export([[:space:]]+-p([[:space:]]|'"$dump_end"')|[[:space:]]*('"$dump_end"'))|'"$path_pfx"'tmux[[:space:]]+show-environment([[:space:]]|'"$dump_end"')|'"$path_pfx"'systemctl([[:space:]]+--user)?[[:space:]]+show-environment([[:space:]]|'"$dump_end"')|'"$path_pfx"'launchctl[[:space:]]+getenv([[:space:]]|'"$dump_end"'))'
# fish's scope flags (-x export, -g/-U/-l global/universal/local, -u unexport,
# -L) list that scope when no name follows; with a name they're the ordinary
# `set -gx PATH …` idiom. Checked only inside a confirmed `fish -c` body —
# bash's `set -x` is the harmless xtrace toggle.
fish_dump_re="$cmd_start"'set([[:space:]]+(-[xguUlL]+|--export|--global|--universal))+[[:space:]]*('"$dump_end"')'
# A `-c` argument is quoted, so mask_quotes alone would erase a dumper the
# shell actually runs. This only locates where the argument starts (through
# the interpreter, its options and trailing whitespace); decode_word extracts
# it. It models bash's option grammar: option words run until the first
# non-option word, and once a cluster held `c` that word is the body — so
# `-ic`, `-ce`, `-c -e` and `-o pipefail -c` all find it. An option word is a
# `-`/`+` cluster (one holding `o`/`O` takes the next word: `-euo pipefail`,
# `-O extglob`), a long `--norc`, `--rcfile`/`--init-file FILE`, or a bare `-`
# or `--`; leftmost-longest matching ends the match right before the body.
# After the flag a bare `-`/`--` ends the option run, so the next word is the
# body even when it looks like an option (`bash -c -- '--x=;env'`). Each run
# is capped at 8 words: an unbounded one restarts at every interpreter word of
# `bash -o bash -o …` and runs to the end each time — quadratic in glibc.
# The interpreters are the shells sharing bash's -c grammar (dash, ash and
# `busybox sh`, ksh, mksh) plus fish, read with the same grammar. The anchor
# takes `/` and `(` for `/usr/bin/fish -c` and `(bash -c …)`, and `;&|)` for a
# compact `true;bash -c …`.
#
# Matched twice per text. On the dequoted view (dequote, below) an escaped or
# quote-spliced `b\ash`, `"bash"`, `'-c'` or `$'bash'` reads as the word bash
# runs. On the raw text, as written, so a body after quoting the W=0 masker
# misreads (a `#` taken for a comment, fish's `\'` in '…', a backtick inside
# '…') or inside a here-string or a pipe into a shell is still found — the
# dequoted view alone would read it as data. The raw search over-scans a
# quoted mention (`echo 'see bash -c …'`); that is kept, as the price of not
# letting a real dumper through when the masker misreads quoting, and a
# backticked one (`echo 'run `bash -c env`'`) denies too. The raw search
# (shell_c_raw_re) drops the `;&|)` anchors, which there would read a pattern
# alternation (`rg 'foo|bash -c …'`) as a command; the dequoted search still
# finds a compact `true;bash -c …`. Both anchor on a backtick, which opens a
# command substitution (`` x=`bash -c …` ``).
shell_c_interp='(fish|bash|sh|zsh|dash|ksh|mksh|ash|busybox[[:space:]]+(sh|ash))'
shell_c_word='[-+][A-Za-z]*[oO][A-Za-z]*[[:space:]]+[^-+[:space:]][^[:space:]]*|[-+][A-Za-z]+|--(rcfile|init-file)[[:space:]]+[^[:space:]]+|--[A-Za-z][-A-Za-z]*(=[^[:space:]]*)?'
shell_c_flag='(-[A-Za-z]*([oO][A-Za-z]*c|c[A-Za-z]*[oO])[A-Za-z]*[[:space:]]+[^-+[:space:]][^[:space:]]*|-[A-Za-z]*c[A-Za-z]*)'
shell_c_body="${shell_c_interp}[[:space:]]+((${shell_c_word}|--?)[[:space:]]+){0,8}${shell_c_flag}[[:space:]]+((${shell_c_word})[[:space:]]+){0,8}(--?[[:space:]]+)?"
shell_c_re="(^|[[:space:]/(;&|)\`])${shell_c_body}"
shell_c_raw_re="(^|[[:space:]/(\`])${shell_c_body}"
# The 8-word caps would let a ninth option word hide the body (`bash -x ×9 -c
# env` matches neither run), so a run of option words that the body regex
# cannot walk past denies outright: before the flag, an interpreter followed by
# nine non-flag option words (shell_c_opt, which leaves out the `-c` clusters)
# and a `-`/`+` word; after the flag, eight option words and a ninth `-`/`+`
# word. The flag itself is in neither count, so `bash -x ×8 -c make` and
# `bash -c -x ×8 make` stay allowed. Fixed repetition, so each stays linear
# like the runs it guards. Each cap has two anchor variants, as the body regex
# does: raw for the raw text (`$cur`), full for the dequoted view.
shell_c_opt='-[A-Zabd-z]*[oO][A-Zabd-z]*[[:space:]]+[^-+[:space:]][^[:space:]]*|\+[A-Za-z]*[oO][A-Za-z]*[[:space:]]+[^-+[:space:]][^[:space:]]*|-[A-Zabd-z]+|\+[A-Za-z]+|--(rcfile|init-file)[[:space:]]+[^[:space:]]+|--[A-Za-z][-A-Za-z]*(=[^[:space:]]*)?'
shell_c_cap_pre="${shell_c_interp}[[:space:]]+((${shell_c_opt}|--?)[[:space:]]+){9}[-+]"
shell_c_cap_post="${shell_c_interp}[[:space:]]+((${shell_c_word}|--?)[[:space:]]+){0,8}${shell_c_flag}[[:space:]]+((${shell_c_word})[[:space:]]+){8}[-+]"
shell_c_cap_pre_raw_re="(^|[[:space:]/(\`])${shell_c_cap_pre}"
shell_c_cap_post_raw_re="(^|[[:space:]/(\`])${shell_c_cap_post}"
shell_c_cap_pre_re="(^|[[:space:]/(;&|)\`])${shell_c_cap_pre}"
shell_c_cap_post_re="(^|[[:space:]/(;&|)\`])${shell_c_cap_post}"
# Any read of /proc/<pid>/environ is a whole-environment dump, so this denies
# without a printing-tool gate. Tested against the raw command, quotes and all,
# which also catches it inside `fish -c '…'` without the -c extraction, and
# against each dequoted view, where a path split by quotes reads as written —
# by grep -E, not `[[ =~ ]]`, which is quadratic on `=/proc/=/proc/…`.
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
# every substr. With J set, a line ending in an odd backslash run loses that
# last backslash and joins the next line unbroken, as bash's backslash-newline
# continuation does.
# shellcheck disable=SC2016
awk_chars='
{
  if (NR > 1 && !jn) feed("\n")
  jn = 0
  jb = 0
  line = $0
  while (line != "") {
    chunk = substr(line, 1, 512)
    line = substr(line, 513)
    n = length(chunk)
    if (!J) for (i = 1; i <= n; i++) feed(substr(chunk, i, 1))
    else for (i = 1; i <= n; i++) {
      jc = substr(chunk, i, 1)
      if (jc != "\\") jb = 0
      else if (i == n && line == "" && jb % 2 == 0) { jn = 1; break }
      else jb++
      feed(jc)
    }
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

# Bash's removal of unquoted backslashes over an already-masked space (quoted
# spans are spaces there, so every backslash left is unquoted): `\X` is X, `\\`
# is one `\`, `\<newline>` is nothing, and a trailing lone `\` stays.
# shellcheck disable=SC2016
awk_escapes='
BEGIN { BS = "\\" }
function feed(c) {
  if (esc) {
    esc = 0
    if (c != "\n") printf "%s", c
  } else if (c == BS) esc = 1
  else printf "%s", c
}
END { if (esc) printf "%s", BS }'

strip_escapes() {
  awk "$awk_escapes$awk_chars" <<<"$1"
}

# Extracts one shell WORD starting at index `start` of `s`: concatenated
# unquoted / '…' / "…" / $'…' segments with quote removal applied, stopping at
# the first unquoted word terminator — `bash -c 'echo '\''hi'\''; env'` is ONE
# argument. No expansion is attempted; the result is only ever re-scanned,
# never executed. The $ of $'…' and $"…" is dropped, or it breaks the
# command-position anchor.
#
# Sets DECODED_WORD and DECODE_WORD_END (one past the last consumed index)
# instead of printing, so the raw finder can keep scanning past this word for
# a sibling `-c` body; a `$(...)` return would lose the second value. It also
# sets DECODED_FRAME_WORD: the word cut at its first unquoted, unescaped
# backtick, empty when there is none. A backtick that closes the enclosing
# frame (`` `bash -c '…'` ``) is not a word terminator, so it is glued to the
# word and the dump regexes then fail on the trailing `(`; the frame reading is
# the word without it. It is searched beside the full word as an over-scan,
# never instead of it, since a backtick that opens a substitution spliced into
# the word (`` bash -c 'ec'`date`'; … ``) must keep the full reading.
awk_decode='
BEGIN { SQ = sprintf("%c", 39); DQ = "\""; BS = "\\"; STOP = " \t\n;&|()"; bt = -1 }
function emit(s) {
  printf "%s", s
  pn += length(s)
}
function feed(c) {
  if (esc) {
    esc = 0
    pos++
    emit(c)
    return
  }
  if (st == "q") {
    pos++
    if (c == SQ) st = ""
    else emit(c)
    return
  }
  if (st != "") {
    pos++
    if (c == BS) esc = 1
    else if (c == (st == "d" ? DQ : SQ)) st = ""
    else emit(c)
    return
  }
  if (dollar) {
    dollar = 0
    if (c == SQ || c == DQ) {
      pos++
      st = (c == SQ ? "a" : "d")
      return
    }
    emit("$")
  }
  if (index(STOP, c)) exit
  pos++
  if (c == BS) esc = 1
  else if (c == "$") dollar = 1
  else if (c == SQ) st = "q"
  else if (c == DQ) st = "d"
  else {
    if (c == "`" && bt < 0) bt = pn
    emit(c)
  }
}
END {
  if (dollar) emit("$")
  if (esc && st != "") emit(BS)
  printf "\n%d\n%d", bt, pos
}'

decode_word() {
  local res rest frame_len
  res=$(awk "$awk_decode$awk_chars" <<<"${1:$2}")
  DECODE_WORD_END=$(($2 + ${res##*"$nl"}))
  rest=${res%"$nl"*}
  frame_len=${rest##*"$nl"}
  DECODED_WORD=${rest%"$nl"*}
  DECODED_FRAME_WORD=
  if ((frame_len >= 0)); then DECODED_FRAME_WORD=${DECODED_WORD:0:frame_len}; fi
}

# Rule 2's masker: what mask_quotes does, plus the shell structure it cannot see,
# so a quote character in prose cannot hide a later line and quoted code still
# shows. Every deviation from mask_quotes errs toward showing text (over-scan):
#   - `$'…'` is a quote whose `\'` does not close it.
#   - an unquoted `#` at word start begins a comment, emitted RAW with all quote,
#     heredoc and substitution openers suppressed to the newline. A comment
#     mis-detected (`a\ #`, `$((#`) would hide the openers it suppresses; the
#     J reading below covers that case.
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
#
# J=1 (mask_cmd_joined) is W=0 with no `#` comments and with backslash-newline
# continuations joined (in awk_chars), searched beside the others. The comment
# rule is a heuristic: the base reading is right for a real comment, J for a
# misdetected one; a command mixing both is an accepted limit.
#
# D=1 (dequote) is not a mask but bash's quote removal over the W=0 reading,
# so the `-c` finder sees the words bash runs and shares this machine's span
# boundaries: an apostrophe in a comment or heredoc body cannot open a quote
# here that the masker never opened. Quote characters and the `$` of `$'…'`
# / `$"…"` are dropped; a quoted character is itself, except a metacharacter
# (whitespace incl. CR/VT/FF, `; & | ( ) < >`), which is `_` so quoted data
# can never separate or anchor; outside quotes `\X` is X (a metacharacter
# `_`) and `\<newline>` nothing; inside "…" the backslash goes only before
# $ ` " \ and newline (that newline too). Comments, heredoc bodies and
# delimiter words, backticks and `$(` frames print as W=0 prints them, frame
# contents dequoted as code. ANSI-C escapes are not decoded. Not
# length-preserving, so K="k1 k2 …" (ascending) prints instead, one per line,
# the raw index (feed count) of the character that produced each output index
# — decode_word needs the raw offset — and stops reading after the last.
# shellcheck disable=SC2016
awk_mask_cmd='
BEGIN {
  SQ = sprintf("%c", 39); DQ = "\""; BS = "\\"; BT = "`"
  HDSTOP = " \t\n;&|()<>"
  META = " \t\n\r" sprintf("%c%c", 11, 12) ";&|()<>"
  sp = 0; pd = 0; hd_i = 0; hd_n = 0
  if (K != "") kc = split(K, ks, " ")
  kn = 1
}
function out(s, i) {
  if (K == "") { printf "%s", s; return }
  on += length(s)
  while (kn <= kc && on > ks[kn] + 0) { printf "%d\n", i; kn++ }
  if (kn > kc) { hit = 1; exit }
}
function dqc(c) { return index(META, c) ? "_" : c }
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
function code(c,    d, o, n, u) {
  d = dl
  dl = 0
  u = ud
  ud = 0
  if (u && c != SQ && c != DQ) out("$", ri - 1)
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
      else if (D) o = (c == "\n" ? "" : dqc(c))
    } else if (D) {
      if (q == DQ && c == "\n") o = ""
      else if (q == DQ && !index("$" BT DQ BS, c)) { out(BS, ri - 1); o = dqc(c) }
      else o = dqc(c)
    }
  } else if (cm) {
    o = c
    if (c == "\n") cm = 0
  } else if (q == SQ) {
    if (c == SQ) { q = ""; if (D) o = "" }
    else if (D) o = dqc(c)
  } else if (q == "A") {
    if (c == BS) esc = 1
    else if (c == SQ) q = ""
    if (D) o = (q == "" ? "" : dqc(c))
  } else if (q == DQ) {
    if (D) o = dqc(c)
    if (c == BS) { esc = 1; if (D) o = "" }
    else if (c == DQ) { q = ""; if (D) o = "" }
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
      if (D) o = ""
      else if (sp > 0 && (sk[sp - 1] == BT || sk[sp - 1] == "E")) o = " "
    } else if (c == SQ) { q = (d ? "A" : SQ); o = (D ? "" : " ") }
    else if (c == DQ) { q = DQ; o = (D ? "" : " ") }
    else if (c == BT) {
      if (W == 1) o = tick(n)
      else if (W == 2) { push(BT, ""); fr = 1; fb = 0; o = "(" }
      else if (sp > 0 && sk[sp - 1] == BT) { pop(); o = ")" }
      else { push(BT, ""); o = "(" }
    } else if (c == "(") pd++
    else if (c == ")") {
      if (pd > 0) pd--
      else if (sp > 0 && sk[sp - 1] == "(") pop()
    } else if (c == "#" && !J && (wordstart(prev) || (W == 1 && wo))) cm = 1
    else if (c == "$") {
      dl = 1
      if (D) { ud = 1; o = "" }
    }
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
    out("\n", ri)
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
    out(bbt ? "(" : ")", ri)
    return
  }
  out(c, ri)
}
function feed(c,    arm, wasesc) {
  ri = nf++
  if (fr) { framefeed(c); return }
  if (body) { bodyfeed(c); return }
  if (hs == 3) {
    if (hbt) { if (frameout(c)) hbt = 0; return }
    if (hbs) { hbs = 0; hw = hw c; out(" ", ri); return }
    if (W == 2 && c == BT && hq != SQ) { hbt = 1; fb = 0; hspan = 1; printf "("; return }
    if (hq != "") {
      if (c == hq) hq = ""
      else if (W == 2 && hq == DQ && c == BS) hbs = 1
      else hw = hw c
      out(" ", ri)
      return
    }
    if (c == BS) { hbs = 1; hquo = 1; out(" ", ri); return }
    if (c == SQ || c == DQ) { hq = c; hquo = 1; out(" ", ri); return }
    if (!index(HDSTOP, c)) { hw = hw c; out(" ", ri); return }
    if (hw != "" || hquo || hspan) {
      hd_n++
      hd_w[hd_n] = hspan ? "\n" : hw
      hd_d[hd_n] = hdash
      hd_q[hd_n] = hquo
    }
    hs = 0
  } else if (hs == 2) {
    if (c == " " || c == "\t") { out(c, ri); return }
    if (c == "-" && !hdash) { hdash = 1; out(" ", ri); return }
    if (c == "<") { hs = 0; out("<", ri); prev = c; return }
    if (index(HDSTOP, c)) hs = 0
    else { hs = 3; hw = ""; hq = ""; hbs = 0; hquo = 0; hbt = 0; hspan = 0; nf--; feed(c); return }
  }
  arm = (q == "" && !esc && !cm)
  wasesc = esc
  out(code(c), ri)
  if (arm && c == "<") {
    if (hs == 1) { hs = 2; hdash = 0 }
    else hs = 1
  } else if (hs == 1) hs = 0
  if (c == "\n" && q == "" && !wasesc && hd_i < hd_n) enterbody()
  prev = c
}
END {
  if (!D || hit) exit
  if (ud) out("$", ri)
  else if (esc && (q == "" || q == DQ)) out(BS, ri)
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

mask_cmd_joined() {
  awk -v W=0 -v J=1 "$awk_mask_cmd$awk_chars" <<<"$1"
}

dequote() {
  awk -v W=0 -v D=1 "$awk_mask_cmd$awk_chars" <<<"$1"
}

dequote_index() {
  awk -v W=0 -v D=1 -v K="$2" "$awk_mask_cmd$awk_chars" <<<"$1"
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
#     of -e/-f/-m/-A/-B/-C/-d/-D or of a long option that takes a value
#     (--label, --include, --glob, --max-count, ...: --label -c reads -c as
#     the label); ripgrep and ag get a stricter class. A CR,
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
  # The split must not cut a redirect: `>&`, `<&` and `>|` become \001/\002
  # forms on a copy, so `grep KEY .env 2>&1 -c` stays one stage. `&>` is not
  # protected: POSIX sh reads `grep … &>f -q` as `grep … &` then `>f -q`. The
  # P/I tests above read the original line.
  t = $0
  gsub(/>&/, ">\001", t); gsub(/<&/, "<\001", t); gsub(/>\|/, ">\002", t)
  ns = split(t, SEG, /[;&|()`]/)
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
  # \001/\002 are the protected redirect forms from the stage split.
  gsub(/[0-9]*(>>?[|&\001\002]?|<[<>&\001]?<?-?)[[:space:]]*[^[:space:]]+/, " ", m)
  n = split(m, W, /[[:space:]]+/)
  for (j = 1; j <= n; j++) {
    if (W[j] !~ /^((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)$/) continue
    found = 1
    rg = (W[j] ~ /^(rg|ripgrep|ag)$/)
    for (e = j + 1; e <= n && W[e] != "+" && W[e] !~ /^((e|f|z|u|ze|zf|bz|xz)?grep|rg|ripgrep|ag)$/; e++) ;
    for (x = j + 1; x < e && W[x] != "--"; x++) ;
    for (j++; j < x; j++) {
      if (W[j] ~ /^-(-(regexp|file|label|exclude|include|exclude-dir|exclude-from|include-dir|max-count|context|after-context|before-context|binary-files|devices|directories|group-separator|glob|iglob|type|type-not|type-add|replace|max-depth|encoding|max-filesize|engine|path-separator|sort|sortr|pre|pre-glob|ignore-file|colors|file-search-regex|path-to-ignore|ignore|after|before|depth)|[efmABCdD])$/ && j + 1 < x) j++
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
# match nothing here. A name can also end at a $ or ( the shell expands
# (`.env$(true)`, `.netrc$x`); templates are stripped before this test, so
# `.env.example$x` is still dropped first.
# shellcheck disable=SC2016
cmd_secret_re='(^|[[:space:]"'"'"'=/])\.env([[:space:]"'"'"';|&)>$(]|$|\.[A-Za-z0-9_-]+)|\.aws/credentials|(^|[[:space:]"'"'"'=/~])\.netrc([[:space:]"'"'"';|&)>$(]|$)|id_(rsa|ed25519|ecdsa)([[:space:]]|$)|\.(pem|p12|pfx)([[:space:]]|$)'
# The same names as the shell also spells them: after < : { , ( or a
# backtick, and before a glob, a brace, a backtick or a redirect.
# shellcheck disable=SC2016
cmd_secret_wide_re='(^|[[:space:]"'"'"'=/<:{,(`])\.env([[:space:]"'"'"';|&)><*?[{},`$(]|$|\.[A-Za-z0-9_*?[{-])|(^|[[:space:]"'"'"'=/<:{,(`])\.envrc\.local([[:space:]"'"'"';|&)><*?[{},`$(]|$)|\.aws/credentials|(^|[[:space:]"'"'"'=/<:{,(`~])\.netrc([[:space:]"'"'"';|&)><*?[{},`$(]|$)|id_(rsa|ed25519|ecdsa)([[:space:]"'"'"';|&)><*?[{},`$(]|$)|\.(pem|p12|pfx)([[:space:]"'"'"';|&)><*?[{},`$(]|$)'

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

# Every space with a backslash is judged again with its unquoted backslashes
# removed, as bash reads it: `p\rintenv` runs printenv.
check_dump_spaces() {
  local space views view
  for space in "$@"; do
    views=("$space")
    if [[ $space == *\\* ]]; then views+=("$(strip_escapes "$space")"); fi
    for view in "${views[@]}"; do
      if grep -qE "$env_dump_re" <<<"$view"; then
        deny "A bare env/printenv prints every secret in scope into this transcript. Name the one variable you need and test it without echoing its value."
      fi
      if grep -qE "$printenv_secret_re" <<<"$view"; then
        deny "printenv with a secret-named variable prints its live value into this transcript. To test presence: set -q NAME (fish), or branch on an -n test of the variable and echo only the words set or unset (bash/sh) — never the variable itself."
      fi
      if grep -qE "$builtin_dump_re" <<<"$view"; then
        deny "This lists or shows shell variables, which prints live values into this transcript — the same leak as env/printenv, just through a different command (set -S, declare -p, show-environment, …). To test presence: set -q NAME (fish), or branch on an -n test of the variable and echo only the words set or unset (bash/sh) — never the variable itself."
      fi
    done
  done
}

check_fish_spaces() {
  local space views view
  for space in "$@"; do
    views=("$space")
    if [[ $space == *\\* ]]; then views+=("$(strip_escapes "$space")"); fi
    for view in "${views[@]}"; do
      if grep -qE "$fish_dump_re" <<<"$view"; then
        deny "set with only scope flags (-x, -g, -U, …) and no name lists that scope's variables in fish, printing live values into this transcript — same leak as set -S. To test presence: set -q NAME."
      fi
    done
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
  #    single variable: each match queues the extracted body (nesting), and the
  #    search goes on past that body in the same text (siblings: `bash -c
  #    'true'; bash -c 'declare -p NAME'`). Bounded by total matches, so it
  #    always terminates; reaching that bound denies below. Each item is
  #    searched twice: on the raw text (shell_c_raw_re), resuming past the
  #    body's decoded word, and on its dequoted view (shell_c_re), made once
  #    per item so a long sibling chain stays linear, resuming past the body's
  #    word in that view (a quoted metacharacter there is `_`, so data never
  #    ends it early); every match end maps back to raw text in one more pass.
  #    The two sets of raw body starts merge in order, a body both found
  #    counted once, under the shared match cap.
  raw_spaces=("$command")
  search_spaces=("$(mask_cmd "$command")" "$(mask_quotes "$command")")
  fish_spaces=()
  subs=()
  sub_fish=()
  dequoted_views=()
  dequoted_fish_views=()
  word_re='^[^[:space:];&|()<>]*'
  worklist=("$command")
  worklist_fish=(0)
  wi=0
  matches=0
  option_cap=0
  while ((wi < ${#worklist[@]})) && ((matches < 20)); do
    cur=${worklist[wi]}
    dq=$(dequote "$cur")
    dequoted_views+=("$dq")
    if ((worklist_fish[wi])); then dequoted_fish_views+=("$dq"); fi
    if [[ $cur =~ $shell_c_cap_pre_raw_re || $cur =~ $shell_c_cap_post_raw_re || $dq =~ $shell_c_cap_pre_re || $dq =~ $shell_c_cap_post_re ]]; then option_cap=1; fi
    wi=$((wi + 1))
    raw_starts=()
    raw_interps=()
    raw_words=()
    raw_frames=()
    off=0
    while ((matches + ${#raw_starts[@]} < 20)); do
      rest=${cur:off}
      [[ $rest =~ $shell_c_raw_re ]] || break
      match=${BASH_REMATCH[0]}
      raw_interps+=("${BASH_REMATCH[2]}")
      prefix=${rest%%"$match"*}
      off=$((off + ${#prefix} + ${#match}))
      raw_starts+=("$off")
      decode_word "$cur" "$off"
      raw_words+=("$DECODED_WORD")
      raw_frames+=("$DECODED_FRAME_WORD")
      off=$DECODE_WORD_END
    done
    ends=()
    dq_interps=()
    off=0
    while ((matches + ${#ends[@]} < 20)); do
      rest=${dq:off}
      [[ $rest =~ $shell_c_re ]] || break
      match=${BASH_REMATCH[0]}
      dq_interps+=("${BASH_REMATCH[2]}")
      prefix=${rest%%"$match"*}
      off=$((off + ${#prefix} + ${#match}))
      ends+=($((off - 1)))
      [[ ${dq:off} =~ $word_re ]]
      off=$((off + ${#BASH_REMATCH[0]}))
    done
    dq_starts=()
    if ((${#ends[@]})); then
      # Captured before mapfile: inside `<<<"$(…)"` a failed pass escapes
      # errexit and reads as one empty start — an allow. Bare errexit on the
      # assignment would exit silently, so a failure empties idx instead.
      idx=$(dequote_index "$cur" "${ends[*]}") || idx=
      mapfile -t dq_starts <<<"$idx"
      if [[ -z $idx ]] || ((${#dq_starts[@]} != ${#ends[@]})); then
        echo "secret-read-guard: index map failed; guard NOT enforcing" >&2
        exit 1
      fi
    fi
    i=0
    j=0
    while ((matches < 20 && (i < ${#raw_starts[@]} || j < ${#dq_starts[@]}))); do
      if ((j < ${#dq_starts[@]} && (i == ${#raw_starts[@]} || dq_starts[j] + 1 < raw_starts[i]))); then
        decode_word "$cur" $((dq_starts[j] + 1))
        readings=("$DECODED_WORD")
        frame=$DECODED_FRAME_WORD
        interp=${dq_interps[j]}
        j=$((j + 1))
      else
        if ((j < ${#dq_starts[@]} && dq_starts[j] + 1 == raw_starts[i])); then j=$((j + 1)); fi
        readings=("${raw_words[i]}")
        frame=${raw_frames[i]}
        interp=${raw_interps[i]}
        i=$((i + 1))
      fi
      # The frame reading (decode_word) rides beside the full word, one match
      # for both; a duplicate found inside it counts toward the cap, which
      # fails closed.
      if [[ -n $frame && $frame != "${readings[0]}" ]]; then readings+=("$frame"); fi
      for sub in "${readings[@]}"; do
        masked_sub=$(mask_cmd "$sub")
        quoted_sub=$(mask_quotes "$sub")
        raw_spaces+=("$sub")
        search_spaces+=("$masked_sub" "$quoted_sub")
        subs+=("$sub")
        if [[ $interp == fish ]]; then
          fish_spaces+=("$masked_sub" "$quoted_sub")
          sub_fish+=(1)
        else
          sub_fish+=(0)
        fi
        worklist+=("$sub")
        worklist_fish+=("${sub_fish[-1]}")
      done
      matches=$((matches + 1))
    done
  done
  # The dequoted views are searched too: quote splicing in a dumper word
  # (`e""nv`, `de"cl"are -p`) hides it from both masks but not from bash.
  check_dump_spaces "${search_spaces[@]}" "${dequoted_views[@]}"
  check_fish_spaces ${fish_spaces[@]+"${fish_spaces[@]}"} ${dequoted_fish_views[@]+"${dequoted_fish_views[@]}"}

  printf -v proc_views '%s\n' "$command" "${dequoted_views[@]}"
  if grep -qE "$proc_environ_re" <<<"$proc_views"; then
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
  #    missed it, so a deny elsewhere never pays for it. The dequoted views are
  #    spaces too: quoting inside a credential name (`.e""nv`) hides it from
  #    the raw text but not from bash.
  raw_spaces+=("${dequoted_views[@]}")
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

  # The joined reading differs from W=0 only at a `#` or a backslash-newline.
  joined_spaces=()
  joined_fish_spaces=()
  if [[ $command == *'#'* || $command == *\\"$nl"* ]]; then joined_spaces+=("$(mask_cmd_joined "$command")"); fi
  for ((i = 0; i < ${#subs[@]}; i++)); do
    [[ ${subs[i]} == *'#'* || ${subs[i]} == *\\"$nl"* ]] || continue
    joined_sub=$(mask_cmd_joined "${subs[i]}")
    joined_spaces+=("$joined_sub")
    if ((sub_fish[i])); then joined_fish_spaces+=("$joined_sub"); fi
  done
  check_dump_spaces ${joined_spaces[@]+"${joined_spaces[@]}"}
  check_fish_spaces ${joined_fish_spaces[@]+"${joined_fish_spaces[@]}"}

  # The finder's blind spots deny last, so any specific deny above wins.
  if ((option_cap)); then
    deny "This shell invocation carries too many options for the secret-read guard to find its -c body; it cannot verify what runs. Drop the redundant options (set them inside the body with set -x / set -o …), or run the code from a script file."
  fi
  # The 20th body is queued but never searched, so reaching the cap always
  # leaves work unverified — and quoted mentions count toward it, so a padded
  # string must not push a real body past it. Fails closed on a command with
  # 20 or more real `-c` bodies too.
  if ((matches >= 20)); then
    deny "This command holds too many shell -c invocations for the secret-read guard to verify. Split the command into smaller ones, or run the code from a script file."
  fi
  ;;
esac

exit 0
