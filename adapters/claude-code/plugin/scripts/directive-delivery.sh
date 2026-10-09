#!/usr/bin/env bash
# Directive-delivery hook: hand a pi worker the dispatcher's directives mid-turn
# (#840). A pi turn can run for tens of minutes, and the worker reads its inbox
# only at a seam, so a `crew reply` ("stop", "rebase first") sat unread while
# D6's `unread: dispatcher directive` notice fired with nothing able to act on
# it. hookyard's pi bridge appends this handler's `additionalContext` to the next
# tool result, which is the only place a running turn looks.
#
# Registered on post_tool in the VERDICT lane (hookyard reads advice only from
# there; a fire-and-forget handler's stdout is dropped). Protocol: print
# {"hookSpecificOutput":{"additionalContext":"…"}} and exit 0 to advise; empty
# stdout is silence. Every failure falls through to `exit 0` with no stdout, so
# this is `set -u` only: an `-e` or pipefail would turn a missing log or a dead
# `crew` into an error where silence is wanted.
#
# Leads only: a session without a sessioned CREW_WORKER_ID (a human, a
# dispatcher, a branch-only id) or with CREW_ROLE_ID (a grid role pane inherits
# the lead's CREW_WORKER_ID, but its tool calls are not the lead's) never
# reaches the bus.
#
# Only `dispatcher:<own crew>` is delivered. A role verdict, another worker's
# msg or another crew's dispatcher stays unread for `crew await`, so the
# selection is `crew inbox --from dispatcher:<crew> --undelivered`: the delivered
# marks (#290) decide what is new, and inbox raises them under the session's marks
# lock, so two parallel tool calls deliver once and watchdog D6 and `crew nudge`
# see the directive as read.
#
# Cost: this runs after EVERY tool call, so the common case reads no row and
# forks nothing but `wc -c` (each fork is milliseconds on a loaded host; bash has
# no stat builtin): the bus dir comes from the task doc header, the cursor key is
# parameter expansion. A per-session cursor holds the bus byte size at the last
# completed check; equal size exits before stdin is read, and new bytes are
# prefiltered with grep for a `"msg"` row naming the dispatcher before the Go
# `crew inbox` is spawned. The prefilter is a superset test, so a false positive
# costs one slow-path call. Every cursor write is the size read at the START of
# the call: a row appended meanwhile is rechecked next call. A last byte that is
# not a newline is a row its writer has not finished, so the cursor stays put and
# that chunk is rescanned. A row `crew inbox` cannot parse stops its read for good
# (exit 5), so a complete tail moves the cursor past it anyway; otherwise every
# later tool call would run the slow path.
#
# The msg body is data. It travels only as jq input: never eval'd, never a printf
# format. Both markers carry a per-call nonce the body, written before this
# handler ran, cannot know, so it cannot close the advisory early or fake a second
# one. Neutralisation is a second layer: any `[` (or fullwidth `［`) that opens
# `end directive` or `dispatcher directive`, through whitespace, format characters
# or `_`/`-`, becomes `(`.
#
# Failure modes that drop a delivery (accepted): the router kills this handler
# after the marks were raised, or the bridge drops the advisory (it declines a
# non-array tool result). The msg is then marked delivered but unseen mid-turn;
# the worker's next `crew inbox --since <seen>` peek ignores marks and shows it.
#
# Portable to macOS's bash 3.2 and BSD userland: no mapfile, no ${var,,}.

set -u

[[ ${CREW_WORKER_ID:-} == worker:*'#s'* ]] || exit 0
[[ -z ${CREW_ROLE_ID:-} ]] || exit 0

command -v jq >/dev/null || exit 0
command -v crew >/dev/null || exit 0

# The task doc header (up to the first blank line) carries the crew id, resolved
# the way crew.sh's _crew_id resolves it (header, then the environment), and the
# bus dir. Reading the dir here spares the `git rev-parse` fork.
crew=''
crew_dir=''
if [[ -f $PWD/WORKER_TASK.md && -r $PWD/WORKER_TASK.md ]]; then
  while IFS= read -r line; do
    [[ -n $line ]] || break
    if [[ -z $crew && $line == crew_id:* ]]; then
      crew=${line#crew_id:}
      crew=${crew#"${crew%%[![:space:]]*}"}
      crew=${crew%"${crew##*[![:space:]]}"}
    elif [[ -z $crew_dir && $line == crew_dir:* ]]; then
      crew_dir=${line#crew_dir:}
      crew_dir=${crew_dir#"${crew_dir%%[![:space:]]*}"}
      crew_dir=${crew_dir%"${crew_dir##*[![:space:]]}"}
    else
      continue
    fi
    [[ -z $crew || -z $crew_dir ]] || break
  done <"$PWD/WORKER_TASK.md"
fi
[[ -n $crew ]] || crew=${CREW_ID:-}
[[ -n $crew ]] || exit 0

if [[ -z $crew_dir || ! -d $crew_dir ]]; then
  common=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) || exit 0
  [[ -n $common ]] || exit 0
  crew_dir=$common/crew
fi
log=$crew_dir/events.jsonl
[[ -f $log ]] || exit 0

# Cursor key: the id with every character outside the filename-safe set replaced,
# no fork. Two ids that sanitize alike share a file, so line 2 holds the full id
# and a cursor naming another id is read as 0 (a full prefilter rescan: slower,
# still correct).
key=${CREW_WORKER_ID//[!A-Za-z0-9._-]/_}
curdir=$crew_dir/directive-delivery
cursor_file=$curdir/$key

cursor=0
if [[ -f $cursor_file && -r $cursor_file ]]; then
  {
    read -r cursor
    read -r owner
  } 2>/dev/null <"$cursor_file"
  [[ ${owner:-} == "$CREW_WORKER_ID" ]] || cursor=0
fi
# Base 10 throughout: a stored `08` is not octal.
cursor=${cursor//[!0-9]/}
cursor=$((10#${cursor:-0}))

size=$(wc -c 2>/dev/null <"$log") || exit 0
size=${size//[!0-9]/}
[[ -n $size ]] || exit 0
size=$((10#$size))
[[ $cursor != "$size" ]] || exit 0
# Smaller log: truncated or rotated, so scan from the start.
((cursor <= size)) || cursor=0

# Written atomically — mktemp then mv, because two of these can run at once on
# parallel tool calls and a bare `printf >` is not one write(2).
save_cursor() { # <offset>
  local tmp
  mkdir -p "$curdir" 2>/dev/null || return 0
  tmp=$(mktemp "$curdir/.cur.XXXXXX" 2>/dev/null) || return 0
  if printf '%s\n%s\n' "$1" "$CREW_WORKER_ID" 2>/dev/null >"$tmp"; then
    mv "$tmp" "$cursor_file" 2>/dev/null || rm -f "$tmp"
  else
    rm -f "$tmp"
  fi
}

# $(…) strips a trailing newline, so an empty read means the log ends on a
# complete row.
last=$(tail -c +"$size" "$log" 2>/dev/null | head -c 1)
if [[ -z $last ]]; then
  next=$size
else
  next=$cursor
fi

# `grep -c` reads to EOF, so no stage dies of SIGPIPE under a caller's pipefail.
n=$(tail -c +$((cursor + 1)) "$log" 2>/dev/null | grep -F '"msg"' | grep -F -c "dispatcher:$crew")
n=${n//[!0-9]/}
if [[ ${n:-0} -eq 0 ]]; then
  save_cursor "$next"
  exit 0
fi

# Not `out`: a Nix shell exports it, and assigning an exported variable keeps it
# exported, so a large inbox would fail every later exec with E2BIG.
rows=$(crew inbox "$CREW_WORKER_ID" "$crew" --from "dispatcher:$crew" --undelivered 2>/dev/null)
rc=$?
# Whatever it printed is already marked delivered, so it is rendered even when
# the exit was non-zero (a torn tail). The cursor advances on success, so a failed
# call is retried; exit 5 over a complete tail is a corrupt row, which no retry
# fixes, so it advances too. Any other exit (a dead crew) keeps the cursor.
if ((rc == 0 || (rc == 5 && next == size))); then
  save_cursor "$next"
fi
[[ -n $rows ]] || exit 0

# The encoded handler stdout, not the text, is what hookyard's 64 KiB cap
# counts, and JSON escaping can inflate a body of quotes and backslashes; so each
# candidate is measured as the final object. Rows that do not fit are replaced by
# a count and a `--since` that re-reads them (marks do not gate `--since`).
nonce=$RANDOM$RANDOM
# shellcheck disable=SC2016 # jq program, not a bash format string
advisory=$(
  jq -c -Rs --arg nonce "$nonce" '
    def neut: gsub("［"; "(") | gsub("\\[(?=[\\s\\p{Cf}]*(end|dispatcher)[\\s\\p{Cf}_-]*directive)"; "("; "i");
    def tsof: (try (fromjson | .ts) catch null) | if type == "number" then . else 0 end;
    split("\n") | map(select(length > 0)) as $raw
    | ($raw | length) as $n
    | ($raw | map(neut)) as $lines
    | def render($k):
        ($n - $k) as $cut
        | ([ "[dispatcher directive \($nonce) — read and act before continuing]"
           , $lines[:$k][]
           , (if $cut > 0 then
                "… \($cut) more — run crew inbox \"$CREW_WORKER_ID\" --since \(
                  if $k > 0 then ($raw[$k - 1] | tsof) else (($raw[0] | tsof) - 1) end)"
              else empty end)
           , "[end directive \($nonce)]"
           , "Delivered and marked read. Handle it now as a Checkpoint-peek directive. If"
           , "you were about to `crew await` a dispatcher reply, this is it: do not await"
           , "it again. Do not move your seen-cursor for this msg: your next `crew inbox"
           , "--since <seen>` peek shows it again. Skip it there, and advance per the"
           , "Checkpoint-peek rules."
           ] | join("\n"));
      def wrap($k): {hookSpecificOutput: {additionalContext: render($k)}};
      def fits($k): (wrap($k) | tojson | utf8bytelength) < 61440;
      if $n == 0 then empty else
        (([limit(1; range(1; $n + 1) | select(fits(.) | not))] | .[0] // ($n + 1)) - 1) as $k
        | wrap($k)
      end
  ' <<<"$rows" 2>/dev/null
) || exit 0
[[ -n $advisory ]] || exit 0

# The router writes a payload of up to 1 MiB; the delivery path must not depend
# on it ignoring an early exit.
cat >/dev/null
printf '%s\n' "$advisory"
