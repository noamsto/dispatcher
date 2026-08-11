# shellcheck shell=bash
# File-based coordination bus: id | identity | status | msg | watch | roster | inbox | stall-watch | pr-watch | log
# A real CLI on PATH (not a fish fn) so BOTH the dispatcher (fish) and workers
# (their bash tool) can call it. Pure jq + append; the log is the state. The
# shebang + `set -euo pipefail` are prepended by writeShellApplication, so this
# source omits them (the shell= directive above keeps standalone shellcheck happy).

# Index-aligned codename pool (FleetView-style). identity is deterministic over
# the branch, so any caller recomputes the same name/color — never stored.
# _tmuxc are mid-tone 256-palette codes chosen for legibility on BOTH Catppuccin
# Latte (light) and Mocha (dark): each clears ~2.5:1 contrast (most >3:1) on
# either base, so the badge lazytmux tints doesn't wash out when the theme flips.
# The old bright/pale set (colour44/141/220/154/117…) was dark-mode-only.
#
# 32 slots, not 16: identity is `cksum % pool`, so collisions follow the birthday
# bound — at 16 a 4-worker crew collided about half the time (one run put #158,
# #164 and #221 all on "sage"). 32 roughly halves it; `roster` still disambiguates
# whatever slips through, because no stateless hash can guarantee uniqueness.
# The 16 added codes were picked by measured WCAG contrast against both bases
# (worst added: 2.86 mocha / 2.88 latte — above the 2.54 floor of the original 16).
_names=(sage atlas nova ember reef iris amber coral moss slate rust plum lime rose sky onyx
  pine lagoon indigo fern bronze violet khaki ash brick mauve tan crimson fuchsia blush orchid cobalt)
_colors=(green blue magenta orange teal purple yellow salmon olive steel rust plum lime pink sky grey
  seagreen darkcyan indigo forestgreen darkgoldenrod mediumpurple darkkhaki dimgrey indianred palevioletred peru crimson mediumvioletred hotpink orchid royalblue)
_tmuxc=(colour28 colour32 colour127 colour130 colour30 colour98 colour136 colour167 colour100 colour67 colour166 colour96 colour64 colour162 colour25 colour244
  colour29 colour31 colour61 colour65 colour94 colour97 colour101 colour102 colour131 colour132 colour137 colour160 colour163 colour168 colour169 colour68)

_identity() { # $1=branch -> {name,color,tmux}; cksum is POSIX (portable to macOS)
  local n i
  n=$(printf '%s' "$1" | cksum | cut -d' ' -f1)
  i=$((n % ${#_names[@]}))
  jq -nc --arg name "${_names[$i]}" --arg color "${_colors[$i]}" --arg tmux "${_tmuxc[$i]}" \
    '{name:$name, color:$color, tmux:$tmux}'
}

# _occupants <worktree_path> -> [{window,name,pane,engine}] — worker windows
# rooted at that path. Keyed on @crew_name (dispatch stamps it on every worker
# window), NOT on the pane's running command: a finished agent drops back to a
# shell prompt, and a command match would then read its window as empty and let
# the next dispatch stack a second worker onto the same tree (#17). Excludes the
# dispatcher's own window — it carries @crew_name too — and the caller's.
_occupants() {
  local wtp="$1" self_win="" wins panes out wid nm path pw pid cmd epane
  if [ -n "${TMUX_PANE:-}" ]; then
    self_win=$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}' 2>/dev/null || true)
  fi
  wins=$(tmux list-windows -a -F '#{window_id}	#{@crew_name}	#{pane_current_path}' 2>/dev/null || true)
  panes=$(tmux list-panes -a -F '#{window_id}	#{pane_id}	#{pane_current_command}' 2>/dev/null || true)
  out='[]'
  while IFS=$'\t' read -r wid nm path; do
    [ -n "$wid" ] || continue
    [ "$path" = "$wtp" ] || continue
    [ -n "$nm" ] || continue
    [ "$nm" != dispatcher ] || continue
    [ "$wid" != "$self_win" ] || continue
    epane=""
    while IFS=$'\t' read -r pw pid cmd; do
      [ "$pw" = "$wid" ] || continue
      case "$cmd" in
      claude | codex | cursor-agent)
        epane="$pid"
        break
        ;;
      esac
    done <<PANES
$panes
PANES
    out=$(printf '%s' "$out" | jq -c --arg w "$wid" --arg n "$nm" --arg p "$epane" \
      '. + [{window:$w, name:$n, pane:(if $p=="" then null else $p end), engine:($p!="")}]')
  done <<WINS
$wins
WINS
  printf '%s' "$out"
}

# _sessions <branch> <crew_or_empty> -> [{session,worker_id,state,ts,age_s,terminal}]
# oldest -> newest. Every fold is per SESSION: aggregating across a branch is how
# three workers came to read as one flip-flopping identity (#17). A session with a
# dispatch event but no status yet is still listed (state null) — dispatch needs to
# see a booting worker. No crew filter by default, same reason `reap` has none: the
# sessions worth inspecting are the ones from earlier dispatcher crews.
_sessions() {
  local branch="$1" crewf="$2"
  [ -f "$log" ] || {
    printf '[]'
    return 0
  }
  jq -s -c --arg b "$branch" --arg crew "$crewf" '
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
      map(select($crew=="" or .crew_id==$crew))
      | ( map(select(.kind=="dispatch" and .branch==$b))
          | map({session:(.session // null), ts:.ts}) ) as $disp
      | ( map(select(.kind=="status"
                     and ((.from // "") | startswith("worker:"))
                     and ((.from) | wid_branch) == $b))
          | map({session:((.from) | wid_session), state:.body.state, ts:.ts}) ) as $st
      | ( ($disp + $st) | map(.session) | unique ) as $ids
      | [ $ids[] as $s
          | ($st | map(select(.session == $s)) | sort_by(.ts) | last) as $latest
          | ($disp | map(select(.session == $s)) | sort_by(.ts) | last) as $d
          | { session: $s,
              worker_id: ("worker:" + $b + (if $s == null then "" else "#" + $s end)),
              state: ($latest.state // null),
              ts: ($latest.ts // $d.ts),
              terminal: ((["done","failed","exited"] | index($latest.state // "")) != null) } ]
      | sort_by(.ts)
      | map(. + {age_s: (((now*1000) - .ts) / 1000 | floor)})' "$log"
}

# _lock_acquire <lockdir> <owner_pid> — atomic mkdir gate with dead-PID reclaim.
# mkdir is atomic on POSIX, so it is the ONLY gate: exactly one caller wins.
# Returns 0 (acquired; owner_pid written inside for liveness) or 1 (held by a
# DIFFERENT live PID, or lost the reclaim race). If the held owner equals the
# requested owner, returns 0 idempotently — re-running /dispatcher in the same
# session (same session-stable PID) is a no-op success, not a refusal. rm -rf
# here is safe — the lock dir is transient coordination state, never user data.
_lock_acquire() {
  local ld="$1" owner="$2" held
  if mkdir "$ld" 2>/dev/null; then
    printf '%s\n' "$owner" >"$ld/pid"
    return 0
  fi
  held=$(cat "$ld/pid" 2>/dev/null || true)
  if [ "$held" = "$owner" ]; then
    return 0 # idempotent: this same owner already holds it
  fi
  if [ -n "$held" ] && kill -0 "$held" 2>/dev/null; then
    return 1
  fi
  rm -rf "$ld" # stale (owner PID dead/empty) — reclaim through the same mkdir gate
  if mkdir "$ld" 2>/dev/null; then
    printf '%s\n' "$owner" >"$ld/pid"
    return 0
  fi
  return 1
}

_lock_release() { rm -rf "$1"; }

# A bus line MUST fit in one write(). `printf '%s\n' … >>"$log"` is NOT reliably
# atomic under O_APPEND: bash's printf builtin doesn't guarantee one write(2)
# syscall per line, so under concurrent writers a line above a few KB can land
# as two separate writes with another process's append spliced into the gap,
# corrupting two records into one line (#20, #55). `_LINE_MAX` is a shrink
# target for readability, not what makes appends atomic — `_bus_append` below
# is what does that.
_LINE_MAX=4096
_ELIDED=' …[elided]'

# _bus_append <log> <line> — append <line> + newline to <log> as a single
# write(2) (#55). `iflag=fullblock` makes dd keep reading until its buffer is
# full or EOF instead of writing out whatever a single short read from the
# pipe returned — without it, dd's own read/write is no more atomic than the
# `printf >>` it replaces. Only POSIX-common flags (no GNU-only `oflag=append`
# or `of=`), so this also works under macOS's system BSD `dd`. Covers only the
# two call sites that use it (`status`/`msg`, and `_post`) — `reply`,
# `pr-watch` and `reap` still append directly and share the same hazard.
_bus_append() { printf '%s\n' "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"; }

# _shrink <text> <keep> — shorten <text> to roughly <keep> characters.
# A sink body (`metrics:`, `retro:`) is itself JSON, and cutting it as a blob ends
# the string mid-object: the enclosing bus line stays valid but the body no longer
# parses, so a reader loses the WHOLE record — every key, not just the long one
# (#25). So shorten each long string leaf and re-encode instead, which keeps the
# record's shape and its short keys intact.
# Plain-text bodies (a worker's question) and `status` details are not JSON and
# still get the blob cut. `cut -c` cuts on a character boundary, so multibyte text
# never splits mid-rune.
_shrink() {
  local text="$1" keep="$2"
  if printf '%s' "$text" | jq -e 'type=="object" or type=="array"' >/dev/null 2>&1; then
    printf '%s' "$text" | jq -c --argjson k "$keep" --arg e "$_ELIDED" \
      'walk(if type=="string" and (length > $k) then .[0:$k] + $e else . end)'
  else
    printf '%s%s' "$(printf '%s' "$text" | cut -c1-"$keep")" "$_ELIDED"
  fi
}

# _fit_line <builder> <text> — echo the line <builder> builds from <text>, with
# <text> shortened until the ENCODED line fits. Re-measures each pass rather than
# computing a cut from the input length: JSON escaping is nonlinear (a control byte
# becomes six characters). The proportional guess is floored at a 3/4 step so every
# pass strictly shrinks and the loop terminates.
_fit_line() {
  local build="$1" full="$2" text="$2" line n keep
  line=$("$build" "$text")
  keep=${#full}
  while :; do
    n=$(printf '%s' "$line" | wc -c)
    { [ "$n" -le "$_LINE_MAX" ] || [ "$keep" -eq 0 ]; } && break
    keep=$(((keep * _LINE_MAX / n) < (keep * 3 / 4) ? (keep * _LINE_MAX / n) : (keep * 3 / 4)))
    text=$(_shrink "$full" "$keep")
    line=$("$build" "$text")
  done
  printf '%s' "$line"
}

# Resolve the crew id: env wins; else read it from WORKER_TASK.md at the repo
# root, so a worker just calls `crew status …` with no env prefix to remember.
_crew_id() {
  if [ -n "${CREW_ID:-}" ]; then
    printf '%s' "$CREW_ID"
    return 0
  fi
  local top
  top=$(git rev-parse --show-toplevel 2>/dev/null || true)
  if [ -n "$top" ] && [ -f "$top/WORKER_TASK.md" ]; then
    grep -m1 '^crew_id:' "$top/WORKER_TASK.md" | cut -d' ' -f2 || true
  fi
}

# _burn_weight <model> — prints "<class>\t<weight>", or "" for a model this
# burn table (dispatch-orchestration.md → "Burn classes") does not name.
# kimi-k3* deliberately falls through to "" — the burn doc does not class it —
# so the cursor rungs are matched on their exact strings, never on a `*high*`
# glob that would also swallow kimi-k3-high.
_burn_weight() {
  case "$1" in
  composer-2.5*) printf 'free\t0' ;;
  *haiku* | gpt-5.6-luna | cursor-grok-4.5-low-fast) printf 'cheap\t1' ;;
  *sonnet* | gpt-5.6-terra | cursor-grok-4.5-medium-fast) printf 'standard\t2' ;;
  *opus* | gpt-5.6-sol | cursor-grok-4.5-high) printf 'premium\t4' ;;
  claude-fable-5 | *fable*) printf 'fable\t8' ;;
  *) printf '' ;;
  esac
}

# _gh_json <gh args…> — print gh's JSON on success, print NOTHING on any
# failure (network down, 404, expired token, gh absent from PATH). ALWAYS
# returns 0: call sites are plain `out=$(_gh_json …)` under the ambient
# `set -e`, where a non-zero return would abort the whole sweep at the first
# failed call instead of merging the stored t2 forward.
_gh_json() {
  local out
  if out=$(gh "$@" 2>/dev/null); then
    printf '%s' "$out"
  fi
  return 0
}

sub="${1:-}"
shift || true

# `id` and `identity` need no repo / no crew id.
if [ "$sub" = id ]; then
  printf '%s\n' "${CREW_ID:-$(date +%s)-$$}"
  exit 0
fi
if [ "$sub" = identity ]; then
  [ -n "${1:-}" ] || {
    echo "crew: identity <branch>" >&2
    exit 1
  }
  _identity "$1"
  exit 0
fi
if [ "$sub" = occupants ]; then
  [ -n "${1:-}" ] || {
    echo "crew: occupants <worktree-path>" >&2
    exit 1
  }
  _occupants "$1"
  printf '\n'
  exit 0
fi

# repo-keyed bus dir; --path-format=absolute so main-checkout and worktrees
# resolve to a byte-identical path (load-bearing — see #29).
common=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)
[ -n "$common" ] || {
  echo "crew: not in a git repo" >&2
  exit 1
}
dir="$common/crew"
log="$dir/events.jsonl"

case "$sub" in
status | msg)
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  mkdir -p "$dir"
  if [ "$sub" = status ]; then
    # status <from> <state> [detail] [pr_url]
    # Reject an unknown state: the log IS the state, and a junk value is silently
    # absorbed by every reader (`{"state":""}` reached the log once and rendered as
    # a blank roster row). Fail loudly at the writer instead.
    case "${2:-}" in
    working | blocked | pr_open | done | failed | exited) ;;
    *)
      echo "crew: status state must be one of working|blocked|pr_open|done|failed|exited (got '${2:-}')" >&2
      exit 1
      ;;
    esac
    # Terminal states are posted once. A worker that re-announces `pr_open`/`done`
    # (seen: the same pr_open+done pair 4s apart) wakes `watch` twice with an
    # identical batch, so the dispatcher handles the same completion again.
    case "${2:-}" in
    pr_open | done | failed | exited)
      if [ -f "$log" ]; then
        prev=$(jq -r --arg c "$crew" --arg m "${1:-}" \
          'select(.crew_id==$c and .kind=="status" and .from==$m)
             | "\(.body.state)\t\(.body.pr_url // "")"' "$log" 2>/dev/null | tail -1 || true)
        [ "$prev" = "${2:-}"$'\t'"${4:-}" ] && exit 0
      fi
      ;;
    esac
    from="${1:-}" state="${2:-}" pr="${4:-}"
    _build_status() {
      jq -nc --arg crew "$crew" --arg from "$from" --arg state "$state" \
        --arg detail "$1" --arg pr "$pr" \
        '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
          kind:"status",
          body:({state:$state}
                + (if $detail!="" then {detail:$detail} else {} end)
                + (if $pr!="" then {pr_url:$pr} else {} end))}'
    }
    line=$(_fit_line _build_status "${3:-}")
  else
    # msg <from> <to> <body>
    from="${1:-}" to="${2:-}"
    _build_msg() {
      jq -nc --arg crew "$crew" --arg from "$from" --arg to "$to" --arg body "$1" \
        '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:$to, kind:"msg", body:$body}'
    }
    line=$(_fit_line _build_msg "${3:-}")
  fi
  _bus_append "$log" "$line"
  ;;
reply)
  # reply <to> <body> — sugar over `msg`; from is dispatcher:<crew> so the
  # dispatcher needn't reconstruct its own id.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  mkdir -p "$dir"
  # A branch-only worker target resolves to the newest session on that branch, so
  # the dispatcher keeps writing `worker:<branch>` while the message lands on a
  # session that exists NOW. Resolving at send time is what makes inheritance
  # impossible: a stopped session's successor has a different id, so a directive
  # written for the former is never addressed to the latter (#17).
  to="${1:-}"
  case "$to" in
  worker:*)
    # Distinguish explicit session id from branch-only: extract the suffix after
    # the last '#' and check whether it has the sid shape s<epoch>-<pid>. A '#'
    # embedded in the branch name (legal in git) does not match that shape, so
    # those branch-only addresses fall through to _sessions resolution.
    _rest="${to##*#}"
    if [[ "$_rest" =~ ^s[0-9]+-[0-9]+$ ]]; then
      : # explicit worker:<branch>#s<epoch>-<pid>, honour verbatim
    else
      br="${to#worker:}"
      newest=$(_sessions "$br" "$crew" | jq -c 'last')
      [ -n "$newest" ] && [ "$newest" != null ] || {
        echo "crew: no session on $br — dispatch a worker before replying to one" >&2
        exit 1
      }
      if [ "$(printf '%s' "$newest" | jq -r .terminal)" = true ]; then
        echo "crew: newest session on $br is $(printf '%s' "$newest" | jq -r .state) — a stopped session never reads its inbox; re-dispatch with the context baked in" >&2
        exit 1
      fi
      to=$(printf '%s' "$newest" | jq -r .worker_id)
    fi
    ;;
  esac
  _build_reply() {
    jq -nc --arg crew "$crew" --arg to "$to" --arg body "$1" \
      '{ts:(now*1000|floor), crew_id:$crew, from:("dispatcher:"+$crew), to:$to, kind:"msg", body:$body}'
  }
  line=$(_fit_line _build_reply "${2:-}")
  printf '%s\n' "$line" >>"$log"
  ;;
await)
  # await <agent> [--timeout S] [--interval S] — block until a msg addressed to
  # <agent> arrives (ts strictly after the await started), print it, exit 0.
  # A timeout also exits 0: empty stdout, not the exit code, is the marker.
  # No LLM tokens burned: this is a held bash call, not a
  # spin loop. A late reply is never lost — it stays in the durable log for the
  # next activation.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  me="${1:-}"
  [ -n "$me" ] || {
    echo "crew: await <agent> [--timeout S] [--interval S]" >&2
    exit 1
  }
  shift || true
  timeout=300
  interval=2
  while [ $# -gt 0 ]; do
    case "$1" in
    --timeout)
      [ -n "${2:-}" ] || {
        echo "crew: --timeout needs a value" >&2
        exit 1
      }
      timeout="$2"
      shift 2
      ;;
    --interval)
      [ -n "${2:-}" ] || {
        echo "crew: --interval needs a value" >&2
        exit 1
      }
      interval="$2"
      shift 2
      ;;
    *)
      echo "crew: await: unknown arg '$1'" >&2
      exit 1
      ;;
    esac
  done
  start=$(jq -nc 'now*1000|floor')
  deadline=$((start + timeout * 1000))
  while :; do
    if [ -f "$log" ]; then
      ans=$(jq -c --arg crew "$crew" --arg me "$me" --argjson since "$start" \
        'select(.crew_id==$crew and .kind=="msg" and .to==$me and .ts>$since)' "$log" 2>/dev/null | tail -n1 || true)
      [ -n "$ans" ] && {
        printf '%s\n' "$ans"
        exit 0
      }
    fi
    [ "$(jq -nc 'now*1000|floor')" -ge "$deadline" ] && {
      echo "crew: await ended after ${timeout}s — no reply to $me yet" >&2
      exit 0
    }
    sleep "$interval"
  done
  ;;
register | deregister)
  # Per-crew registration (was an exclusive per-repo role lock). N crews may
  # share a repo: each is identified by its crew_id, so there is no
  # cross-crew contention and registration never refuses. The crew dir records
  # the dispatcher's long-lived PID (default $PPID), recorded for a future
  # stale-cleanup command (nothing reclaims automatically today), and holds
  # that crew's watch cursor + watch lock. Crew ids are unique by construction
  # (timestamp-pid), so re-registering a live crew is idempotent (re-mkdir -p,
  # pid rewritten).
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  cdir="$dir/crews/$crew"
  if [ "$sub" = register ]; then
    mkdir -p "$cdir"
    printf '%s\n' "${1:-$PPID}" >"$cdir/pid"
  else
    rm -rf "$cdir"
  fi
  ;;
watch)
  # watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] — block until
  # any worker event qualifies (a status whose state is in --states, or a msg to
  # the dispatcher), print {"cursor":TS,"events":[…]} and exit 0. An expired park
  # also exits 0 (empty stdout is the marker) so it isn't reported as a failed
  # background command. Default --timeout is a finite long park (3300s / 55min):
  # an indefinite watch is rejected, because a reaped indefinite watch is
  # undetectable. When --since is omitted the cursor is self-seeded from
  # the crew's cursor file (see below) so a stale caller cursor can't re-deliver.
  # Zero-token held poll, like `await`. Strict `>` matches `await` (same-ms edge).
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  mkdir -p "$dir"
  since=0
  since_explicit=0
  states="blocked,pr_open,done,failed"
  timeout=3300
  interval=2
  while [ $# -gt 0 ]; do
    case "$1" in
    --since)
      [ -n "${2:-}" ] || {
        echo "crew: --since needs a value" >&2
        exit 1
      }
      since="$2"
      since_explicit=1
      shift 2
      ;;
    --states)
      [ -n "${2:-}" ] || {
        echo "crew: --states needs a value" >&2
        exit 1
      }
      states="$2"
      shift 2
      ;;
    --timeout)
      [ -n "${2:-}" ] || {
        echo "crew: --timeout needs a value" >&2
        exit 1
      }
      timeout="$2"
      shift 2
      ;;
    --interval)
      [ -n "${2:-}" ] || {
        echo "crew: --interval needs a value" >&2
        exit 1
      }
      interval="$2"
      shift 2
      ;;
    *)
      echo "crew: watch: unknown arg '$1'" >&2
      exit 1
      ;;
    esac
  done
  case "$since" in '' | *[!0-9]*)
    echo "crew: --since must be an integer ms timestamp" >&2
    exit 1
    ;;
  esac
  case "$timeout" in '' | *[!0-9]*)
    echo "crew: --timeout must be a positive integer number of seconds" >&2
    exit 1
    ;;
  esac
  [ "$timeout" -gt 0 ] || {
    echo "crew: --timeout must be > 0 (indefinite watch unsupported: a reaped watch would be undetectable)" >&2
    exit 1
  }
  statesjson=$(printf '%s' "$states" | jq -Rc 'split(",") | map(select(length>0))')
  [ "$statesjson" = "[]" ] && {
    echo "crew: --states must be non-empty" >&2
    exit 1
  }
  cdir="$dir/crews/$crew"
  mkdir -p "$cdir"
  cursor_file="$cdir/cursor"
  if [ "$since_explicit" = 0 ]; then
    seed=$(cat "$cursor_file" 2>/dev/null || true)
    case "$seed" in '' | *[!0-9]*) seed=0 ;; esac
    since="$seed"
  fi
  wlock="$cdir/watch.lock.d"
  _lock_acquire "$wlock" "$$" || {
    echo "crew: another watch is already running for this crew ($crew)" >&2
    exit 1
  }
  trap '_lock_release "$wlock"' EXIT
  me="dispatcher:$crew"
  start=$(jq -nc 'now*1000|floor')
  deadline=$((start + timeout * 1000))
  while :; do
    if [ -f "$log" ]; then
      batch=$(jq -c -s --arg crew "$crew" --arg me "$me" --argjson since "$since" --argjson states "$statesjson" '
          map(select(.crew_id==$crew and .ts>$since
                and ( (.kind=="status" and (.body.state as $s | $states | index($s)))
                      or (.kind=="msg" and (.to==$me or .to=="*")) )))
          | sort_by(.ts)
          | select(length>0)
          | {cursor:(.[-1].ts), events:.}' "$log" 2>/dev/null || true)
      [ -n "$batch" ] && {
        printf '%s\n' "$batch"
        last=$(printf '%s' "$batch" | jq -r '.cursor')
        tmp=$(mktemp "$dir/.cursor.XXXXXX")
        printf '%s\n' "$last" >"$tmp"
        mv -f "$tmp" "$cursor_file"
        exit 0
      }
    fi
    [ "$(jq -nc 'now*1000|floor')" -ge "$deadline" ] && {
      echo "crew: watch park ended after ${timeout}s — no new events (cursor $since)" >&2
      exit 0
    }
    sleep "$interval"
  done
  ;;
sessions)
  branch="${1:-}"
  shift || true
  screw=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew)
      [ -n "${2:-}" ] || {
        echo "crew: --crew needs a value" >&2
        exit 1
      }
      screw="$2"
      shift 2
      ;;
    *)
      echo "crew: sessions <branch> [--crew ID]" >&2
      exit 1
      ;;
    esac
  done
  [ -n "$branch" ] || {
    echo "crew: sessions <branch> [--crew ID]" >&2
    exit 1
  }
  _sessions "$branch" "$screw"
  printf '\n'
  ;;
roster)
  crew="${1:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  # Fold per SESSION first, then collapse per BRANCH for display. Both halves are
  # load-bearing: aggregating across a branch is what made three workers read as
  # one flip-flopping identity, and it would also let an older session's `working`
  # resurrect a newer session's `exited` through prev_state (#17).
  base=$(jq -c -s --arg crew "$crew" '
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
      # title lives on the dispatch event (keyed by branch); join it per branch.
      # last wins on re-dispatch. missing (pre-title dispatch events) -> null.
      (map(select(.crew_id==$crew and .kind=="dispatch"))
        | map({key:.branch, value:(.title // null)}) | from_entries) as $titles
      | map(select(.crew_id==$crew and .kind=="status"
                   and ((.from // "") | startswith("worker:"))))
      | group_by(.from)
      | map(
          (max_by(.ts)) as $latest
          | {from: $latest.from,
             branch: ($latest.from | wid_branch),
             session: ($latest.from | wid_session),
             state: $latest.body.state,
             # `detail` is unbounded free text and the roster is an LLM-read
             # dashboard, so an untruncated row can crowd out the table. 120
             # fits every reserved prefix plus its payload; the full string
             # stays in the log. `source` tells a watchdog-posted `blocked`
             # (nobody is listening) from one the worker posts itself (someone is in await).
             detail: (($latest.body.detail // null) | if . == null then null else .[0:120] end),
             source: ($latest.body.source // null),
             ts: $latest.ts,
             # carry forward last-known pr_url — the terminal `done` event drops it
             pr_url: (map(.body.pr_url) | map(select(. != null)) | last),
             # last state that was NOT the exited backstop, so a spurious exited
             # can be resolved back to what the worker itself last reported.
             prev_state: (map(select(.body.state != "exited")) | max_by(.ts) | .body.state),
             age_s: ((now - ($latest.ts/1000))|floor)})
      | group_by(.branch)
      | map((sort_by(.ts) | last)
            + {title: (.[0].branch as $b | $titles[$b] // null),
               sessions: (sort_by(.ts) | map({session, state, age_s}))})' "$log")
  # Resolve a false `exited`. SessionEnd fires for more than the worker's own session
  # (a subagent ending, a human closing an auxiliary pane), and the hook sees only a
  # cwd — it cannot tell those apart, nor wait to find out: the worker's real
  # SessionEnd blocks on its own hooks, so probing liveness there deadlocks. Deciding
  # here is race-free because it runs long after the fact. Keyed on the worktree PATH,
  # NOT the window name — lazytmux renames worker windows to decorated labels
  # ("󰊤 #164 🧠 …"), so dispatch's `-n "$sanitized"` is long gone by read time.
  live=$(tmux list-panes -a -F '#{pane_current_command} #{pane_current_path}' 2>/dev/null || true)
  resolved='[]'
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    st=$(printf '%s' "$row" | jq -r '.state')
    branch=$(printf '%s' "$row" | jq -r '.branch')
    if [ "$st" = exited ] && [ -n "$branch" ]; then
      # awk reads to EOF on purpose: an early `exit` SIGPIPEs git under pipefail.
      wtpath=$(git worktree list --porcelain |
        awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
      if [ -n "$wtpath" ]; then
        while IFS= read -r pane; do
          case "$pane" in
          "claude $wtpath"* | "codex $wtpath"* | "cursor-agent $wtpath"*)
            row=$(printf '%s' "$row" | jq -c '.state = (.prev_state // "working") | .exit_suspect = true')
            break
            ;;
          esac
        done <<PANES
$live
PANES
      fi
    fi
    resolved=$(printf '%s' "$resolved" | jq -c --argjson r "$row" '. + [$r]')
  done <<EOF
$(printf '%s' "$base" | jq -c '.[]')
EOF
  idmap='{}'
  for br in $(printf '%s' "$resolved" | jq -r '.[].branch'); do
    id=$(_identity "$br")
    idmap=$(printf '%s' "$idmap" | jq -c --arg k "$br" --argjson v "$id" '. + {($k): $v}')
  done
  # Disambiguate colliding codenames. identity is a stateless hash, so two live
  # workers can legitimately land on the same name; suffix the issue/ticket token
  # from the branch (feat/207-… -> sage·207, eng-6789-… -> sage·eng-6789) so
  # "coral is blocked" stays a unique referent. Only collisions are suffixed.
  # prev_state is internal to the resolve step above — drop it unless it explains
  # a row the reader would otherwise mistrust.
  printf '%s' "$resolved" | jq --argjson m "$idmap" '
      map(. + ($m[.branch] // {}))
      | (map(select(.name != null) | .name) | group_by(.) | map(select(length > 1) | .[0])) as $dupes
      | map(if (.name as $n | $dupes | index($n))
            then .name = (.name + "·" + ((.branch | capture("(?:[a-z]+/)?(?<id>[A-Za-z]+-[0-9]+|[0-9]+)") | .id) // .branch))
            else . end)
      | map(if .exit_suspect then . else del(.prev_state) end)'
  ;;
inbox)
  # messages only — `roster` owns status (every status is addressed to the
  # dispatcher, so without this the inbox is status spam). `--since TS` is a
  # non-blocking single pass (no loop, unlike watch/await): return only msgs
  # strictly newer than TS. Omitting it returns all msgs to the agent, unchanged.
  me="${1:-}"
  shift || true
  crew=""
  since=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --since)
      [ -n "${2:-}" ] || {
        echo "crew: --since needs a value" >&2
        exit 1
      }
      since="$2"
      shift 2
      ;;
    *)
      crew="$1"
      shift
      ;;
    esac
  done
  crew="${crew:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  if [ -n "$since" ]; then
    case "$since" in '' | *[!0-9]*)
      echo "crew: --since must be an integer ms timestamp" >&2
      exit 1
      ;;
    esac
    jq -c --arg crew "$crew" --arg me "$me" --argjson since "$since" \
      'select(.crew_id==$crew and .kind=="msg" and (.to==$me or .to=="*") and .ts>$since)' "$log"
  else
    jq -c --arg crew "$crew" --arg me "$me" \
      'select(.crew_id==$crew and .kind=="msg" and (.to==$me or .to=="*"))' "$log"
  fi
  ;;
log)
  crew="${1:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  jq -c --arg crew "$crew" 'select(.crew_id==$crew)' "$log"
  ;;
report)
  crew="${1:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  printf 'engine\tmodel\ttier\tshape\toutcome\tduration_s\n'
  jq -s -r --arg crew "$crew" '
    map(select(.crew_id == $crew)) as $all
    | ($all | map(select(.kind == "dispatch")))[]
    | .branch as $b
    | ($all | map(select(.kind == "status" and ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b))) as $st
    | ($st | map(select(.body.state == "working")) | sort_by(.ts) | (.[0].ts // null)) as $start
    | ($st | sort_by(.ts) | (.[-1] // null)) as $last
    | [ .engine, .model, .tier, (.shape // "—"),
        ($last.body.state // "—"),
        (if ($start != null and $last != null) then (($last.ts - $start) / 1000 | floor | tostring) else "—" end)
      ] | @tsv' "$log"
  ;;
rate)
  # Sweep this repo's bus into the global ratings store, or (--report) render
  # the global store's per-(engine,model,tier) rollup. The report path never
  # touches the local bus or the network — see docs/superpowers/specs/
  # 2026-08-10-crew-rate-reconcile-report-design.md.
  report=false
  json=false
  while [ $# -gt 0 ]; do
    case "$1" in
    --report)
      report=true
      shift
      ;;
    --json)
      json=true
      shift
      ;;
    *)
      echo "crew: rate takes --report and --json" >&2
      exit 1
      ;;
    esac
  done
  if [ "$json" = true ] && [ "$report" = false ]; then
    echo "crew: rate takes --report and --json" >&2
    exit 1
  fi

  if [ "$report" = true ]; then
    # Reads the global store only — no local bus, no network (spec §crew
    # rate --report). The `crew roster` idiom: last row wins per run_id. A
    # missing or empty store folds to [], which renders as the header alone
    # so "no data yet" stays visibly distinct from "command did nothing".
    store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
    store="$store_dir/ratings.jsonl"
    rows=$(jq -s -c 'group_by(.run_id) | map(max_by(.swept_at))' "$store" 2>/dev/null || true)
    rows="${rows:-[]}"
    # One aggregation shared by both output modes: every column is
    # computed once as {value, k, n} — k is the aggregate's OWN
    # denominator, n is the row's total — and only the final branch differs:
    # --json prints that structure as-is, the table renders and pads it.
    printf '%s' "$rows" | jq -r --argjson want_json "$json" '
      def median:
        sort | length as $l
        | if $l == 0 then null
          elif ($l % 2) == 1 then .[($l - 1) / 2 | floor]
          else (.[($l / 2 | floor) - 1] + .[$l / 2 | floor]) / 2
          end;
      def mean: if length == 0 then null else add / length end;
      def nrows: map(select(.outcome != "running" and .outcome != "incomplete"));
      def agg(k; n; v): {value: v, k: k, n: n};
      def fmt1:
        if . == null then null
        else
          (. * 10 | round) as $t
          | (($t / 10) | floor) as $i
          | ($t - ($i * 10)) as $f
          | "\($i).\($f)"
        end;
      # One humanised-duration rule, shared by ttpr/ttmerge.
      def humanize_ms:
        if . == null then null
        elif . < 5400000 then ((. / 60000 | round | tostring) + "m")
        else (((. / 3600000) | fmt1) + "h")
        end;
      # Marker rules (spec §Making small samples impossible to miss):
      # "—" when unmeasured, "(k)" when the own denominator differs from n,
      # "!" when that denominator is below 5.
      def render_agg(k; n; text):
        if k == 0 then "—"
        else (text + (if k != n then "(\(k))" else "" end) + (if k < 5 then "!" else "" end))
        end;

      def stats:
        group_by([.engine, .model, .tier])
        | map(
            . as $g
            | ($g | nrows) as $n
            | ($n | length) as $ncount
            | {
                engine: $g[0].engine, model: $g[0].model, tier: $g[0].tier,
                n: agg($ncount; $ncount; $ncount),
                inc: ($g | map(select(.outcome == "incomplete")) | length),
                run: ($g | map(select(.outcome == "running")) | length),
                pend: ($n | map(select(.pr_state == "OPEN")) | length),
                pr_pct: (
                  ($n | map(select(.reached_pr == true)) | length) as $num
                  | agg($ncount; $ncount; (if $ncount == 0 then null else (($num / $ncount) * 100 | round) end))
                ),
                merge_pct: (
                  ($n | map(select(.pr_state == "MERGED" or .pr_state == "CLOSED"))) as $settled
                  | ($settled | length) as $k
                  | ($settled | map(select(.pr_state == "MERGED")) | length) as $num
                  | agg($k; $ncount; (if $k == 0 then null else (($num / $k) * 100 | round) end))
                ),
                ttpr_ms: (
                  ($n | map(.time_to_pr_ms) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | median))
                ),
                ttmerge_ms: (
                  ($n | map(.time_to_merge_ms) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | median))
                ),
                rework: (
                  ($n | map(.rework_count) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                # review_mode ∉ {none, null} — a downgraded-but-run review
                # still counts, only "no review happened" is excluded.
                high: (
                  ($n | map(select((.review_mode != null) and (.review_mode != "none") and (.review_high != null))) | map(.review_high)) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                rounds: (
                  ($n | map(.review_rounds) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                # blocked_count/watchdog_blocked_count are bus-derived, never
                # null, so this is the one aggregate whose own denominator is
                # always n — it never carries a "(k)".
                blocked: (
                  ($n | map((.blocked_count // 0) + (.watchdog_blocked_count // 0))) as $vals
                  | agg($ncount; $ncount; ($vals | mean))
                ),
                ci1_pct: (
                  ($n | map(.first_ci_green) | map(select(. != null))) as $vals
                  | ($vals | length) as $k
                  | ($vals | map(select(. == true)) | length) as $num
                  | agg($k; $ncount; (if $k == 0 then null else (($num / $k) * 100 | round) end))
                ),
                notes: (
                  ($n | map(.unresolved_notes) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                rev: ($g | map(select(.reverted == true)) | length),
                cost_hours: (
                  ($n | map(.cost_proxy) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; (($vals | mean) | if . == null then null else . / 3600000 end))
                )
              }
          )
        | sort_by([.engine, .model, .tier]);

      ["engine","model","tier","n","inc","run","pend","pr%","merge%","ttpr","ttmerge","rework","high","rounds","blocked","ci1","notes","rev","cost"] as $headers
      | [true,true,true,false,false,false,false,false,false,false,false,false,false,false,false,false,false,false,false] as $left
      | stats as $groups
      | if $want_json then $groups
        else
          ($groups | map(
              [
                .engine, .model, .tier,
                render_agg(.n.k; .n.n; (.n.value|tostring)),
                (.inc|tostring), (.run|tostring), (.pend|tostring),
                render_agg(.pr_pct.k; .pr_pct.n; (.pr_pct.value|tostring)),
                render_agg(.merge_pct.k; .merge_pct.n; (.merge_pct.value|tostring)),
                render_agg(.ttpr_ms.k; .ttpr_ms.n; (.ttpr_ms.value|humanize_ms)),
                render_agg(.ttmerge_ms.k; .ttmerge_ms.n; (.ttmerge_ms.value|humanize_ms)),
                render_agg(.rework.k; .rework.n; (.rework.value|fmt1)),
                render_agg(.high.k; .high.n; (.high.value|fmt1)),
                render_agg(.rounds.k; .rounds.n; (.rounds.value|fmt1)),
                render_agg(.blocked.k; .blocked.n; (.blocked.value|fmt1)),
                render_agg(.ci1_pct.k; .ci1_pct.n; (.ci1_pct.value|tostring)),
                render_agg(.notes.k; .notes.n; (.notes.value|fmt1)),
                (.rev|tostring),
                render_agg(.cost_hours.k; .cost_hours.n; (.cost_hours.value|fmt1))
              ]
            )) as $rows
          | ([$headers] + $rows) as $all
          | ($headers | length) as $ncols
          # Widths and padding computed here — never `column -t`: it is not in
          # runtimeInputs, and BSD vs util-linux pad "—" differently.
          | ([range(0; $ncols) | . as $c | ($all | map(.[$c] | length) | max)]) as $widths
          | ([range(0; $ncols) | . as $c
              | $headers[$c] as $cell
              | ($widths[$c] - ($cell|length)) as $pad
              | if $left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
             ] | join("  ")) as $header_line
          | if ($groups | length) == 0 then $header_line
            else
              ($rows | map(
                  . as $row
                  | [range(0; $ncols) | . as $c
                     | $row[$c] as $cell
                     | ($widths[$c] - ($cell|length)) as $pad
                     | if $left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
                    ] | join("  ")
                )) as $body_lines
              | ($groups | map(
                  [.n.k, .pr_pct.k, .merge_pct.k, .ttpr_ms.k, .ttmerge_ms.k, .rework.k, .high.k,
                   .rounds.k, .blocked.k, .ci1_pct.k, .notes.k, .cost_hours.k]
                  | any(. < 5)
                ) | map(select(.)) | length) as $flagged_count
              | ($groups | length) as $total
              | ([$header_line] + $body_lines
                 + ["", "! own sample < 5 — anecdote, not evidence.   value(k) = measured over k of n runs.   — = unmeasured."]
                 + ["\($flagged_count) of \($total) rows carry at least one small-sample quantity."]
                ) | join("\n")
            end
        end
    '
    exit 0
  fi

  # One record per RUN (a run = a dispatch + the branch events until the next
  # dispatch on that branch). Append-only; readers fold last-wins by run_id.
  # No crew filter — ratings are cross-run/cross-crew evidence.
  [ -f "$log" ] || exit 0
  repo=$(git config --get remote.origin.url 2>/dev/null |
    sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##' || true)
  repo="${repo:-$(basename "$(git rev-parse --show-toplevel)")}"
  # jq cannot call _burn_weight, so resolve each distinct model's burn class
  # in bash first and hand the result in as one lookup object.
  models=$(jq -s -r '[.[] | select(.kind=="dispatch") | (.model // "")] | unique | .[]' "$log")
  costmap='{}'
  while IFS= read -r model; do
    [ -n "$model" ] || continue
    cw=$(_burn_weight "$model")
    [ -n "$cw" ] || continue
    class="${cw%%$'\t'*}"
    weight="${cw#*$'\t'}"
    costmap=$(printf '%s' "$costmap" | jq -c --arg m "$model" --arg c "$class" --argjson w "$weight" \
      '. + {($m): [$c, $w]}')
  done <<<"$models"
  records=$(jq -s --arg repo "$repo" --argjson costmap "$costmap" '
    (map(select(.kind=="dispatch"))) as $disp
    | [ ($disp | map(.branch) | unique)[] as $b
        | ($disp | map(select(.branch==$b)) | sort_by(.ts)) as $runs
        | ($runs|length) as $n
        # Branch reuse (spec §Branch reuse and t2): the PR owner is the
        # earliest run on this branch whose OWN window contains a status
        # carrying a pr_url. Computed once per branch, entirely bus-derived,
        # before any per-run field depends on it.
        | ([ range(0; $n) as $k
            | $runs[$k] as $dk
            | $dk.ts as $tk0
            | (if $k+1 < $n then $runs[$k+1].ts else 9999999999999 end) as $tk1
            | (map(select(
                  .kind!="dispatch"
                  and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
                  and .ts >= $tk0 and .ts < $tk1))
               | map(select(.kind=="status"))
               | map(.body.pr_url) | map(select(.!=null)) | last)
          ]) as $owner_prs
        | ($owner_prs | to_entries | map(select(.value != null)) | (.[0].key // null)) as $owner_idx
        | range(0; $n) as $i
        | $runs[$i] as $d
        | ($d.ts) as $t0
        | (if $i+1 < $n then $runs[$i+1].ts else 9999999999999 end) as $t1
        | (if $i+1 < $n then $runs[$i+1].ts else null end) as $window_end
        | (map(select(
              .kind!="dispatch"
              and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
              and .ts >= $t0 and .ts < $t1))) as $ev
        | ($ev | map(select(.kind=="status"))) as $st
        | ($st | sort_by(.ts) | (.[-1] // null)) as $last
        | ($last.body.state) as $ls
        | ($st | map(.body.pr_url) | map(select(.!=null)) | last) as $pr
        | ($st | map(select(.body.state=="pr_open")) | sort_by(.ts) | (.[0] // null)) as $propen
        | ($pr != null and $i == $owner_idx) as $owns_pr
        # Narrower than the terminal set below on purpose (spec §Cost): a
        # zombie has an unbounded wall clock, not a zero one.
        | ($st | map(select(.body.state=="done" or .body.state=="failed")) | sort_by(.ts) | (.[-1] // null)) as $term
        | (if $term != null then ($term.ts - $t0) else null end) as $wall
        | (($d.shape // null) | if . == "" then null else . end) as $shape
        | ($costmap[$d.model // ""] // null) as $cw
        | (null) as $pr_state
        # Split rather than replace. `blocked_count` feeds an append-only,
        # cross-run ratings store whose rows are compared against rows written
        # before the watchdog existed; changing what populates that field in place
        # would make old and new rows silently non-comparable in the same field.
        # A new field leaves historical rows simply absent (null), which every
        # reader there already tolerates — and watchdog_blocked_count is direct
        # evidence of how often a model/tier wedges.
        | ($st | map(select(.body.state=="blocked" and (.body.source // "") != "watchdog")) | length) as $blocked
        | ($st | map(select(.body.state=="blocked" and (.body.source // "") == "watchdog")) | length) as $wblocked
        # `try fromjson catch null` so ONE unparseable body cannot abort the whole
        # sweep and lose every other run with it (#25). Such a run folds with null
        # metrics — the same shape a run that never emitted metrics already takes.
        | ($ev | map(select(.kind=="msg" and ((.to // "") | startswith("metrics:"))))
               | sort_by(.ts) | (.[-1] // null)
               | if . == null then null else (.body | try fromjson catch null) end) as $m
        | {
            run_id: ($repo + ":" + $b + ":" + ($t0|tostring)),
            repo: $repo, branch: $b,
            session: ($d.session // null),
            engine: $d.engine, model: $d.model, tier: $d.tier,
            effort: $d.effort, title: $d.title,
            shape: $shape,
            t0_ms: $t0,
            window_end_ms: $window_end,
            reached_pr: ($propen != null),
            pr_url: $pr,
            owns_pr: $owns_pr,
            time_to_pr_ms: (if $owns_pr and $propen != null then ($propen.ts - $t0) else null end),
            wall_clock_ms: $wall,
            cost_class: (if $cw == null then null else $cw[0] end),
            cost_proxy: (if $cw == null or $wall == null then null else ($cw[1] * $wall) end),
            outcome: (
              if ($owns_pr and $pr_state == "MERGED") then "merged"
              elif ($ls == "working" or $ls == "blocked") then "running"
              elif ($pr != null) then "pr_open"
              elif ($ls == "done" or $ls == "failed") then "failed"
              else "incomplete" end),
            rework_count: ($m.rework_count // null),
            replanned: (
              if $m == null or (($m | has("replanned")) | not)
              then null
              else $m.replanned
              end
            ),
            review_high: ($m.review_high // null),
            review_mode: ($m.review_mode // null),
            plan_critic_first_pass: ($m.plan_critic_first_pass // null),
            consulted: (if $m == null then null else $m.consulted end),
            blocked_count: $blocked,
            watchdog_blocked_count: $wblocked,
            reported_ok: (($m != null) and ($ls != null)),
            # t2 placeholders. Emitted null by the local fold so every row has
            # one shape from its first sweep; the reconcile below fills them.
            pr_state: $pr_state,
            last_query_ok: null,
            merged: null,
            closed_at_ms: null,
            merged_at_ms: null,
            merge_commit: null,
            time_to_merge_ms: null,
            review_rounds: null,
            first_ci_green: null,
            unresolved_notes: null,
            reverted: null,
            swept_at: (now*1000|floor)
          } ]' "$log")
  store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
  store="$store_dir/ratings.jsonl"
  mkdir -p "$store_dir"
  lockd="$store_dir/ratings.lock.d"
  # The `crew roster` idiom: last row wins per run_id. A missing store folds
  # to [], not an error.
  store_fold='group_by(.run_id) | map(max_by(.swept_at))'

  # --- lock window 1: read the store to decide which calls to skip ---------
  # The lock is never held across the network (spec §Merge-forward): on a repo
  # with many historical runs the reconcile is minutes long, and every other
  # repo's sweep would block on it. `_lock_release` is an unconditional
  # `rm -rf` with no owner check, so the EXIT trap is armed only while this
  # process actually holds the lock — left armed through the unlocked phase it
  # would delete a CONCURRENT sweep's lock dir on exit.
  if ! _lock_acquire "$lockd" "$$"; then
    echo "crew: ratings store busy" >&2
    exit 1
  fi
  trap '_lock_release "$lockd"' EXIT
  snapshot=$(jq -s -c "$store_fold" "$store" 2>/dev/null || true)
  _lock_release "$lockd"
  trap - EXIT
  snapshot="${snapshot:-[]}"

  # --- t2: the GitHub reconcile, unlocked ---------------------------------
  threads_q="query(\$owner:String!,\$name:String!,\$number:Int!){repository(owner:\$owner,name:\$name){pullRequest(number:\$number){reviewThreads(first:100){nodes{isResolved}}}}}"
  # gh marshals an unset timestamp as Go's zero time rather than null, and a
  # gh timestamp is an ISO-8601 string while t0_ms/window_end_ms are epoch ms —
  # comparing the two silently succeeds in jq (every string sorts above every
  # number), so the conversion happens at ingest, once.
  ts2ms='def ts2ms: if . == null or . == "" or startswith("0001-01-01") then null else (fromdateiso8601 * 1000) end;'
  patches='{}'
  gh_failures=0
  while IFS=$'\t' read -r run_id pr_url branch t0_ms win_end do_view do_actions do_threads s_commit s_merged; do
    # The gate is evaluated BEFORE any call: a pr_url that is not a GitHub PR
    # URL issues zero calls. owner/name come from THIS url, never from the
    # sweeping checkout, so a store holding several repos never cross-queries.
    if [[ ! $pr_url =~ ^https://github\.com/([^/]+)/([^/]+)/pull/([0-9]+)$ ]]; then
      continue
    fi
    owner="${BASH_REMATCH[1]}"
    name="${BASH_REMATCH[2]}"
    number="${BASH_REMATCH[3]}"
    # `pr_url` is unvalidated worker-written text, so the URL alone must not
    # decide which repo we spend the developer's gh credential on: a run can
    # only ever reconcile a PR in the repo it was dispatched from.
    if [ "$owner/$name" != "$repo" ]; then
      continue
    fi
    patch='{}'
    tried=false
    ok=true
    m_commit=""
    m_at=""
    if [ "$s_commit" != - ]; then m_commit="$s_commit"; fi
    if [ "$s_merged" != - ]; then m_at="$s_merged"; fi

    if [ "$do_view" = true ]; then
      tried=true
      # Always with the URL: a bare `gh pr view` resolves the sweeping
      # checkout's own branch and would write that PR into every row.
      out=$(_gh_json pr view "$pr_url" --json state,closedAt,mergedAt,mergeCommit,commits,reviews)
      if [ -n "$out" ]; then
        patch=$(printf '%s' "$out" | jq -c --argjson p "$patch" "$ts2ms"'
          ([ (.reviews // [])[]
             | select(.state == "CHANGES_REQUESTED" or .state == "COMMENTED")
             | (.submittedAt | ts2ms) ] | map(select(. != null))) as $rv
          | ([ (.commits // [])[] | (.committedDate | ts2ms) ] | map(select(. != null))) as $cm
          | $p + {
              pr_state: .state,
              closed_at_ms: (.closedAt | ts2ms),
              merged_at_ms: (.mergedAt | ts2ms),
              merge_commit: (.mergeCommit.oid // null),
              # A round is a maximal group of rework reviews followed by >=1
              # later commit, so three reviews before one fix commit is ONE
              # round: bucket each review by the first commit that answers it
              # and count the distinct buckets. Reviews never answered by a
              # commit bucket to null and contribute 0.
              review_rounds: ([ $rv[] as $r | ($cm | map(select(. > $r)) | min) ]
                              | map(select(. != null)) | unique | length)
            }')
        m_commit=$(printf '%s' "$patch" | jq -r '.merge_commit // ""')
        m_at=$(printf '%s' "$patch" | jq -r '(.merged_at_ms // "") | tostring')
      else
        ok=false
        gh_failures=$((gh_failures + 1))
      fi
    fi

    if [ "$do_actions" = true ]; then
      tried=true
      if [ "$win_end" = - ]; then win_end=null; fi
      out=$(_gh_json api --method GET "repos/$owner/$name/actions/runs" -f branch="$branch" -F per_page=100)
      if [ -n "$out" ]; then
        patch=$(printf '%s' "$out" | jq -c --argjson p "$patch" \
          --argjson t0 "$t0_ms" --argjson we "$win_end" '
          $p + {
            # Only the CI this run triggered: the dispatch-boundary window is
            # what stops run i+1 on a reused branch from inheriting the CI of
            # run i.
            first_ci_green: (
              [ (.workflow_runs // [])[]
                | {sha: .head_sha, c: (.created_at | fromdateiso8601 * 1000),
                   status: .status, concl: .conclusion} ]
              | map(select(.c >= $t0 and ($we == null or .c < $we)))
              | if length == 0 then null
                else group_by(.sha) | map({first: (map(.c) | min), runs: .}) | min_by(.first)
                     | .runs | map(select(.status == "completed"))
                     | all(.concl == "success" or .concl == "skipped" or .concl == "neutral")
                end)
          }')
      else
        ok=false
        gh_failures=$((gh_failures + 1))
      fi
    fi

    if [ "$do_threads" = true ]; then
      tried=true
      out=$(_gh_json api graphql -f query="$threads_q" -F owner="$owner" -F name="$name" -F number="$number")
      if [ -n "$out" ]; then
        patch=$(printf '%s' "$out" | jq -c --argjson p "$patch" '
          $p + {unresolved_notes: ([ (.data.repository.pullRequest.reviewThreads.nodes // [])[]
                                     | select(.isResolved == false) ] | length)}')
      else
        ok=false
        gh_failures=$((gh_failures + 1))
      fi
    fi

    # `reverted` — local, no API call. The existence probe is what keeps "not
    # reverted" distinguishable from "merge commit not in this checkout":
    # without it every unfetched repo would report a clean false.
    if [ -n "$m_commit" ] && [ -n "$m_at" ]; then
      if git cat-file -e "$m_commit^{commit}" 2>/dev/null; then
        # `--since` takes approxidate or @<epoch SECONDS>, and silently falls
        # back rather than erroring on a bare 13-digit millisecond value.
        if [ -n "$(git log --all --since="@$((m_at / 1000))" \
          --grep="This reverts commit $m_commit" --max-count=1 2>/dev/null || true)" ]; then
          rev=true
        else
          rev=false
        fi
      else
        rev=null
      fi
      patch=$(printf '%s' "$patch" | jq -c --argjson r "$rev" '. + {reverted: $r}')
    fi

    if [ "$tried" = true ]; then
      patch=$(printf '%s' "$patch" | jq -c --argjson ok "$ok" '. + {last_query_ok: $ok}')
    fi
    patches=$(printf '%s' "$patches" | jq -c --arg id "$run_id" --argjson p "$patch" '. + {($id): $p}')
  done < <(printf '%s' "$records" | jq -r --argjson snap "$snapshot" '
    ($snap | map({key: .run_id, value: .}) | from_entries) as $S
    | (now * 1000) as $now
    | .[] as $r
    | ($S[$r.run_id] // {}) as $s
    | select($r.owns_pr)
    # Per-call finality (spec §Per-call finality): a run-level skip would
    # strand any field whose own call failed on the sweep that first saw the
    # merge, with no path back.
    | [ $r.run_id, $r.pr_url, $r.branch, $r.t0_ms, ($r.window_end_ms // "-"),
        (($s.pr_state == "MERGED")
         or ($s.pr_state == "CLOSED" and $s.closed_at_ms != null
             and ($now - $s.closed_at_ms) >= 2592000000) | not),
        ($s.first_ci_green == null),
        (($s.pr_state == "MERGED" and $s.unresolved_notes != null) | not),
        ($s.merge_commit // "-"), ($s.merged_at_ms // "-") ] | @tsv')

  # --- lock window 2: merge forward against a FRESH read, append the diff --
  if ! _lock_acquire "$lockd" "$$"; then
    echo "crew: ratings store busy" >&2
    exit 1
  fi
  trap '_lock_release "$lockd"' EXIT
  fresh=$(jq -s -c "$store_fold" "$store" 2>/dev/null || true)
  fresh="${fresh:-[]}"
  printf '%s' "$records" | jq -c --argjson stored "$fresh" --argjson patches "$patches" '
    # The window-1 snapshot decided which calls to skip; carrying values
    # forward from it would drop a t2 upgrade another sweep landed while this
    # one was on the network.
    ($stored | map({key: .run_id, value: .}) | from_entries) as $S
    | (now * 1000 | floor) as $sw
    | ["pr_state","closed_at_ms","merged_at_ms","merge_commit",
       "review_rounds","first_ci_green","unresolved_notes","reverted"] as $t2keys
    | .[] as $r
    | ($S[$r.run_id] // {}) as $s
    | ($patches[$r.run_id] // {}) as $p
    # For every field whose call did not succeed, the STORED value carries
    # forward — writing null instead would let one expired token permanently
    # degrade every already-reconciled row.
    | ($t2keys | map(. as $k | {key: $k, value: (
          if ($r.owns_pr | not) then null
          elif ($p | has($k)) then $p[$k]
          else $s[$k] end)}) | from_entries) as $t2
    | ($r + $t2 + {
        merged: (if $t2.pr_state == null then null else ($t2.pr_state == "MERGED") end),
        time_to_merge_ms: (if $t2.merged_at_ms == null then null else ($t2.merged_at_ms - $r.t0_ms) end),
        last_query_ok: $p.last_query_ok,
        outcome: (if ($r.owns_pr and $t2.pr_state == "MERGED") then "merged" else $r.outcome end),
        swept_at: $sw
      }) as $new
    # Ignoring swept_at is what makes an unchanged sweep a byte-level no-op;
    # without it every sweep would append every row forever.
    | select($S[$r.run_id] == null
             or ($S[$r.run_id] | del(.swept_at)) != ($new | del(.swept_at)))
    | $new' >>"$store"
  _lock_release "$lockd"
  trap - EXIT
  # stdout stays clean (spec §crew rate --report renders the store, nothing
  # else) — an expired token or GitHub outage is signalled on stderr only.
  if [ "$gh_failures" -gt 0 ]; then
    echo "crew: rate: $gh_failures gh call(s) failed; those rows kept their stored t2" >&2
  fi
  ;;
stall-watch)
  # stall-watch <worker-id|branch> --pane <id> [--engine E] [--grace S] [--stall S]
  #   [--window S] [--interval S] [--idle S] [--dead S] [--max-life S]
  #
  # Lifetime-scoped liveness watchdog, spawned per worker by `dispatch`. The bus
  # reflects only what a worker POSTS, so a worker parked on an interactive
  # prompt, or whose turn died mid-task, is indistinguishable from one that is
  # working (#31). Four detectors read one pane capture per tick:
  #   D0 stalled:    static pane inside the startup --window whose frame is NOT a prompt
  #   D1 prompt:     prompt frame at the verified geometry, no meter, 2 samples
  #                  (quota: is D1's own content discriminator on the SAME
  #                  geometry, not a fifth detector — see _is_quota_prompt)
  #   D2 turn-stall: meter clock advancing, token string static, no live subagent row
  #   D3 quiet:      byte-identical pane for --idle
  # Every detector posts `blocked` — recoverable, answerable, and cheap to be
  # wrong about. Only quiet:/turn-stall: episodes escalate to `failed`, and only
  # after a second evidence check --dead later; a prompt still on screen is
  # evidence that nobody answered, not that the worker died, so it never
  # escalates (quota: never escalates either — same reasoning, opposite
  # recovery: stop dispatching, don't answer). Engine signatures are the data
  # table below: claude only, because a guessed signature is a false-positive
  # generator.
  # CREW_STALL_SAMPLE_CMD overrides the sampler (stdout = pane text, exit code =
  # pane liveness) so the loop is testable without tmux.
  arg="${1:-}"
  shift || true
  [ -n "$arg" ] || {
    echo "crew: stall-watch <worker-id|branch> --pane <id> [--engine E] [--grace S] [--stall S] [--window S] [--interval S] [--idle S] [--dead S] [--max-life S]" >&2
    exit 1
  }
  # INV-W0 — identity is branch-keyed and suffix-tolerant. dispatch has shipped
  # BOTH `worker:<branch>#s<session>` and a bare `<branch>`, and a watchdog
  # cannot know which one launched it. Under exact-string matching every safety
  # check below silently no-ops and the roster splits into two rows for one
  # worker (measured: EVIDENCE-2026-08-10.txt).
  branch="${arg#worker:}"
  branch="${branch%%#*}"
  me="worker:$branch"
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  pane=""
  engine="unknown"
  grace=45
  stall=300
  window=900
  interval=15
  idle=1800
  dead=1800
  max_life=43200
  while [ $# -gt 0 ]; do
    case "$1" in
    --pane)
      pane="${2:-}"
      shift 2
      ;;
    --engine)
      engine="${2:-}"
      shift 2
      ;;
    --grace)
      grace="${2:-}"
      shift 2
      ;;
    --stall)
      stall="${2:-}"
      shift 2
      ;;
    --window)
      window="${2:-}"
      shift 2
      ;;
    --interval)
      interval="${2:-}"
      shift 2
      ;;
    --idle)
      idle="${2:-}"
      shift 2
      ;;
    --dead)
      dead="${2:-}"
      shift 2
      ;;
    --max-life)
      max_life="${2:-}"
      shift 2
      ;;
    *)
      echo "crew: stall-watch: unknown arg '$1'" >&2
      exit 1
      ;;
    esac
  done
  [ -n "$pane" ] || {
    echo "crew: stall-watch needs --pane <id>" >&2
    exit 1
  }
  # Signature table. Enabling an engine is DATA, not logic: capture its prompt
  # and meter frames on a work-profile host, pin them as fixtures, add a row.
  # codex/cursor deliberately get the frame-free detectors (D0/D3) only — a
  # cursor footer that permanently contained an `Enter to select`-like string
  # would pin every cursor worker at `blocked` forever.
  case "$engine" in
  claude)
    sig_prompt=1
    sig_meter=1
    ;;
  *)
    sig_prompt=0
    sig_meter=0
    ;;
  esac
  # Multibyte-safe BY CONSTRUCTION, not by ambient locale: under LC_ALL=C a
  # bracket expression consumes one BYTE, so a single-character class would
  # never match `◯` (U+25EF, 3 bytes) and D2 would silently lose its only
  # measured false-positive guard. Hence `+` on the glyph classes and an
  # alternation rather than a bracket set for `❯`.
  re_option='^[[:space:]]*(>|❯|\*)?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]'
  re_meter='^[^[:alnum:]]*[A-Za-z]+…[[:space:]]\(([0-9]+h([[:space:]][0-9]+m)?([[:space:]][0-9]+s)?|[0-9]+m([[:space:]][0-9]+s)?|[0-9]+s)[[:space:]]·[[:space:]]↓[[:space:]][0-9.]+k?[[:space:]]tokens'
  re_subrow='^[[:space:]]*[^[:alnum:][:space:]]+[[:space:]]+[a-z][a-z-]+[[:space:]][[:space:]]+.*[[:space:]](([0-9]+h[[:space:]])?([0-9]+m[[:space:]])?[0-9]+s)[[:space:]]·[[:space:]]↓'

  # Raw pane text on stdout; non-zero when the pane is gone. The CALLER hashes:
  # D0/D3 read the hash, D1/D2 read the text.
  _sample() {
    if [ -n "${CREW_STALL_SAMPLE_CMD:-}" ]; then
      eval "$CREW_STALL_SAMPLE_CMD" 2>/dev/null
    else
      tmux capture-pane -p -t "$pane" 2>/dev/null
    fi
  }

  # C-3 — every bus read is scoped to THIS run. events.jsonl is append-only per
  # repo and re-dispatch onto the same branch is a first-class flow, so an
  # unscoped read lets the PREVIOUS run's failed/exited mute a freshly started
  # watchdog for its entire life, silently, at exit 0. Since the documented
  # recovery for every watchdog event is "kill and re-dispatch", that would
  # guarantee the run after any watchdog event has no watchdog at all.
  # One second of slack because `date +%s` truncates while bus timestamps are
  # ms: without it a status the launcher posted a fraction of a second before
  # this process started sorts below the cutoff and reads as a previous run.
  # Previous runs are minutes away, so the slack cannot reach one.
  run_start_ms=$((($(date +%s) - 1) * 1000))
  bus_ts=0
  bus_state=""
  bus_source=""
  bus_detail=""
  _bus_refresh() {
    local l
    bus_ts=0
    bus_state=""
    bus_source=""
    bus_detail=""
    [ -f "$log" ] || return 0
    l=$(jq -r --arg c "$crew" --arg m "$me" --argjson t0 "$run_start_ms" '
        select(.crew_id==$c and .kind=="status" and .ts>=$t0
               and (.from==$m or (.from|startswith($m+"#"))))
        | [(.ts|tostring), .body.state, (.body.source // ""), (.body.detail // "")]
        | @tsv' "$log" 2>/dev/null | tail -1 || true)
    [ -n "$l" ] || return 0
    IFS=$'\t' read -r bus_ts bus_state bus_source bus_detail <<BUSLINE
$l
BUSLINE
    return 0
  }

  # INV-W1 — the watchdog is a subordinate writer. Re-read the bus immediately
  # before EVERY append (the hazard lives in the gap between sampling the pane
  # and writing the line) and abort if the worker has finished. Exempt from the
  # read cadence below on purpose: appends are rare, staleness here is a lie.
  _post() { # _post <state> <detail>
    _bus_refresh
    case "$bus_state" in
    done | failed | exited) exit 0 ;;
    esac
    mkdir -p "$dir"
    local line
    line=$(jq -nc --arg crew "$crew" --arg from "$me" --arg state "$1" --arg detail "$2" \
      '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
          kind:"status", body:{state:$state, detail:$detail, source:"watchdog"}}')
    _bus_append "$log" "$line"
  }

  # INV-W3 — one open watchdog episode per branch PER PREFIX, checked on the bus
  # (the only thing two watchdogs on one branch share) rather than in process
  # memory. Same-prefix, not any-prefix, on purpose: a dead pane must still be
  # able to raise `quiet:` over an open `stalled:`, because `stalled:` does not
  # escalate (C-1) and `quiet:` is then the only path that frees fan-out budget.
  _post_blocked() { # _post_blocked <prefix> <detail>; returns 1 when suppressed
    _bus_refresh
    if [ "$bus_state" = blocked ] && [ "$bus_source" = watchdog ]; then
      case "$bus_detail" in
      "$1"*) return 1 ;;
      # C-1's other half: an open `prompt:` is sticky, un-supersedable by any
      # other prefix. Overwriting it with `quiet:` — which a frozen prompt frame
      # satisfies by construction — would hand the unanswered prompt an
      # escalation path `prompt:` itself is forbidden to have. `quota:` gets the
      # same protection: a quota-exhausted pane is static for hours (far past
      # --idle), so without this a D3 `quiet:` would eventually overwrite the
      # one signal telling the dispatcher to stop dispatching entirely.
      prompt:*) return 1 ;;
      quota:*) return 1 ;;
      esac
    fi
    _post blocked "$2"
  }

  # INV-W2 — a clearance never overwrites a later worker statement. `working` is
  # not in `watch`'s wake set, so this corrects the roster without a second wake.
  # Own-prefix only: under the any-prefix override another detector's episode may
  # be the live one, and clearing it would reset the roster to `working` with no
  # wake, leaving its later `failed` with no visible `blocked` to explain it.
  _post_clear() { # _post_clear <prefix>
    _bus_refresh
    if [ "$bus_state" = blocked ] && [ "$bus_source" = watchdog ]; then
      case "$bus_detail" in
      "$1"*) _post working "$1 cleared" ;;
      esac
    fi
  }

  # Geometry anchor: the footer must be the pane's LAST non-empty line, with a
  # numbered option within the 6 non-empty lines above it. A pane that is not
  # parked on a prompt ends on its input box, never on transcript text (A3), so
  # a prompt frame merely scrolling through — this very repo's bats fixtures —
  # cannot satisfy this. Relaxing it to "the last 10 lines" is exactly how those
  # fixtures become a false-positive source.
  _is_prompt() {
    local tail_n last above
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -7 || true)
    last=$(printf '%s\n' "$tail_n" | tail -1)
    case "$last" in
    *"Enter to select"* | *"Enter to confirm"*) ;;
    *) return 1 ;;
    esac
    above=$(printf '%s\n' "$tail_n" | sed '$d')
    printf '%s\n' "$above" | grep -qE "$re_option"
  }
  # _is_quota_prompt — content discriminator, ALWAYS called alongside _is_prompt
  # (never alone): _is_prompt already proves the pane is on-screen and shaped
  # like an option-select frame; this only decides WHICH option-select frame it
  # is. Searched over the last 12 non-empty lines — wider than _is_prompt's
  # tail-7 (the real rate-limit frame isn't captured anywhere in this repo yet,
  # unlike the pinned fixtures above it, so its exact line count above the
  # footer is unknown and a too-tight window risks silently degrading to
  # generic `prompt:`) but still bounded, not the whole capture: an unbounded
  # search would classify a genuinely different, answerable prompt as `quota:`
  # merely because this literal phrase happens to be visible somewhere higher
  # on the same screen (e.g. a worker with this very protocol doc scrolled
  # into view) — and `quota:` is sticky and escalation-exempt, so that
  # mislabel would leave a real question unanswered indefinitely.
  _is_quota_prompt() {
    printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -12 | grep -qF 'Stop and wait for limit to reset'
  }
  _meter_line() { printf '%s\n' "$1" | grep -E "$re_meter" | tail -1 || true; }
  _has_subrow() { printf '%s\n' "$1" | grep -qE "$re_subrow"; }

  start=$(date +%s)
  sleep "$grace"
  fails=0
  last_hash=""
  last_change=$(date +%s)
  tick=0
  d0_at=0
  d1_hits=0
  d1_at=0
  d1_kind=""
  d2_tok=""
  d2_clock=""
  d2_since=0
  d2_moved=0
  d2_at=0
  d3_at=0
  _bus_refresh
  while :; do
    now=$(date +%s)
    # --max-life exists because the watchdog is nohup-detached: without a hard
    # cap, a bug or an orphaned pane leaves a process polling forever.
    [ $((now - start)) -ge "$max_life" ] && exit 0
    # Exit on a terminal state — but NOT on pr_open: a worker in pr_open is
    # still working (#31's own prompt was rendered by a worker watching PR CI),
    # and not on `working`, which is a heartbeat.
    case "$bus_state" in
    done | failed | exited) exit 0 ;;
    esac

    if ! text=$(_sample); then
      # Pane-gone is a quorum, not a single failure: at a 12h lifetime one tmux
      # hiccup would otherwise disarm liveness for the rest of the run. Three
      # consecutive failures ≈45s at the default interval, and the `exited`
      # backstop owns the real case anyway.
      fails=$((fails + 1))
      [ "$fails" -ge 3 ] && exit 0
      sleep "$interval"
      tick=$((tick + 1))
      continue
    fi
    fails=0
    hash=$(printf '%s' "$text" | cksum)
    if [ "$hash" != "$last_hash" ]; then
      last_hash="$hash"
      last_change="$now"
    fi

    # While the worker's own latest word is a SELF-reported `blocked` it is in
    # `crew await` — the dispatcher is already awake about it, and a held await
    # leaves a static pane by construction (a D3 false positive waiting).
    suppressed=0
    if [ "$bus_state" = blocked ] && [ "$bus_source" != watchdog ]; then
      suppressed=1
    fi
    quiet_for=$((now - last_change))

    # ---- D1: interactive prompt --------------------------------------------
    # Presence, not transition: the workspace-trust frame is on screen from the
    # worker's FIRST sample, so an "appeared" conjunct could never fire on the
    # only frame with measured production occurrences.
    # quota: is a content discriminator on the SAME geometry, not a separate
    # detector — a quota-exhausted frame and a generic option-select frame are
    # mutually exclusive per sample, so one hit counter (d1_hits/d1_at) covers
    # both. d1_kind remembers which was actually posted so a mid-episode flip
    # (e.g. a prompt frame that becomes the quota frame with no intervening
    # non-prompt sample) reclassifies instead of staying mislabeled for the
    # rest of the run.
    if [ "$suppressed" = 0 ] && [ "$sig_prompt" = 1 ] &&
      _is_prompt "$text" && [ -z "$(_meter_line "$text")" ]; then
      if _is_quota_prompt "$text"; then
        kind=quota
      else
        kind=prompt
      fi
      if [ "$d1_at" != 0 ] && [ "$kind" != "$d1_kind" ]; then
        _post_clear "prompt:"
        _post_clear "quota:"
        d1_at=0
        d1_hits=0
      fi
      d1_hits=$((d1_hits + 1))
      if [ "$d1_hits" -ge 2 ] && [ "$d1_at" = 0 ]; then
        if [ "$kind" = quota ]; then
          if _post_blocked "quota:" "quota: quota exhausted — worker parked on the rate-limit prompt in pane $pane; do not re-dispatch — Esc dismisses it, resume continues from intact context on the next window"; then
            d1_at="$now"
            d1_kind=quota
          fi
        elif _post_blocked "prompt:" "prompt: interactive prompt in pane $pane — worker is waiting on input nobody can give"; then
          d1_at="$now"
          d1_kind=prompt
        fi
      fi
    else
      if [ "$d1_at" != 0 ]; then
        _post_clear "prompt:"
        _post_clear "quota:"
        d1_at=0
      fi
      d1_hits=0
      d1_kind=""
    fi

    # ---- D2: dead turn ----------------------------------------------------
    # Strings, never numbers: rounding to 0.1k and multi-unit durations make
    # arithmetic fragile and every parse failure a new branch.
    if [ "$suppressed" = 0 ] && [ "$sig_meter" = 1 ]; then
      m=$(_meter_line "$text")
      if [ -z "$m" ] || _has_subrow "$text"; then
        # A live subagent row is an UNCONDITIONAL veto: a healthy deep worker in
        # a subagent batch reproduces D2's exact signature — meter present,
        # clock rising, token string static — for minutes at a stretch (A1,
        # measured). The cost is a stated blind spot: a turn that dies with a
        # row still painted is invisible to D2.
        if [ "$d2_at" != 0 ]; then
          _post_clear "turn-stall:"
          d2_at=0
        fi
        d2_since=0
        d2_tok=""
        d2_clock=""
        d2_moved=0
      else
        clock=$(printf '%s' "$m" | sed -E 's/^[^(]*\(([^·]*)·.*/\1/')
        tok=$(printf '%s' "$m" | sed -E 's/.*↓[[:space:]]*([0-9.]+k?)[[:space:]]tokens.*/\1/')
        if [ "$d2_since" = 0 ] || [ "$tok" != "$d2_tok" ]; then
          if [ "$d2_at" != 0 ]; then
            _post_clear "turn-stall:"
            d2_at=0
          fi
          d2_tok="$tok"
          d2_clock="$clock"
          d2_since="$now"
          d2_moved=0
        elif [ "$clock" != "$d2_clock" ]; then
          # A rising clock proves the capture is a LIVE frame. A static clock
          # means a frozen renderer or copy-mode scrollback — evidence we cannot
          # trust, so the rule stays silent.
          d2_clock="$clock"
          d2_moved=1
        fi
        # Any status event from the worker in the window is a sign of life —
        # with the same one-second slack, since the window opens on a truncated
        # `date +%s` and the event carries ms.
        if [ "$bus_source" != watchdog ] && [ "$bus_ts" -ge $(((d2_since - 1) * 1000)) ]; then
          d2_since="$now"
          d2_moved=0
        fi
        if [ "$d2_at" = 0 ] && [ "$d2_moved" = 1 ] && [ $((now - d2_since)) -ge "$idle" ]; then
          if _post_blocked "turn-stall:" "turn-stall: token count static at $d2_tok for $((now - d2_since))s while the pane clock advanced"; then
            d2_at="$now"
          fi
        fi
      fi
    fi

    # ---- D3: quiet pane ---------------------------------------------------
    # Byte-identity, not "no meter": a healthy claude pane repaints its spinner
    # every second, so a working worker can never satisfy D3 even if every
    # claude signature rots to garbage. This is the failsafe for signature rot
    # and the only steady-state coverage codex and cursor get.
    if [ "$suppressed" = 0 ] && [ "$d3_at" = 0 ] && [ "$quiet_for" -ge "$idle" ]; then
      if [ "$bus_source" = watchdog ] || [ "$bus_ts" -lt $((last_change * 1000)) ]; then
        if _post_blocked "quiet:" "quiet: pane unchanged for ${quiet_for}s"; then
          d3_at="$now"
        fi
      fi
    fi
    if [ "$d3_at" != 0 ] && [ "$quiet_for" -lt "$idle" ]; then
      _post_clear "quiet:"
      d3_at=0
    fi

    # ---- D0: startup silence ----------------------------------------------
    # Classify BEFORE judging. A static pane that is a prompt belongs to D1 and
    # D0 says nothing about it — that one negative check is the session-3 fix.
    # The detail carries no diagnosis: the old `(suspected startup/indexing
    # hang)` was wrong on 3/3 measured workers, and an invented cause reads to
    # the dispatcher as corroboration.
    if [ "$suppressed" = 0 ] && [ "$d0_at" = 0 ] &&
      [ $((now - start)) -lt "$window" ] && [ "$quiet_for" -ge "$stall" ]; then
      case "$bus_state" in
      "" | working)
        if [ "$sig_prompt" = 1 ] && _is_prompt "$text"; then
          : # D1 owns this frame
        elif _post_blocked "stalled:" "stalled: no output for ${stall}s"; then
          d0_at="$now"
        fi
        ;;
      esac
    fi

    # ---- Escalation --------------------------------------------------------
    # The only path to `failed`, and it is a SECOND, later, independent evidence
    # check — not a timer. `prompt:` and `quota:` are both exempt (C-1): a frame
    # still on screen means nobody answered yet (or the quota window hasn't
    # reset), and escalating either would reproduce session 3 with a 30-minute
    # delay — for `quota:` specifically, laundering hours of an intact worker
    # into a `failed` that invites exactly the re-dispatch this state exists to
    # prevent. `stalled:` is exempt too — it is the branch that catches the
    # classifier's misses, so it must stay recoverable; a genuinely dead pane
    # reaches `failed` through the `quiet:` episode instead.
    # The live bus, not the in-process timer, decides: a timer armed before D1
    # claimed the branch (or before a second watchdog process superseded it —
    # INV-W3 allows two live per branch, coordinating only via the bus) still
    # points at an episode that is no longer the open one, and escalating it
    # would launder a `prompt:`/`quota:` into a `failed`.
    if [ "$d2_at" != 0 ] && [ $((now - d2_at)) -ge "$dead" ]; then
      _bus_refresh
      case "$bus_detail" in
      prompt:* | quota:*) ;;
      *)
        _post failed "dead: turn-stall: unchanged for $((now - d2_at))s"
        exit 0
        ;;
      esac
    fi
    if [ "$d3_at" != 0 ] && [ $((now - d3_at)) -ge "$dead" ]; then
      _bus_refresh
      case "$bus_detail" in
      prompt:* | quota:*) ;;
      *)
        _post failed "dead: quiet: unchanged for $((now - d3_at))s"
        exit 0
        ;;
      esac
    fi

    sleep "$interval"
    tick=$((tick + 1))
    # Bus-read cadence: a whole-file jq over a growing cross-crew log every tick
    # for 12h is ~2880 spawns per worker. Every 4th tick costs ≤60s of latency
    # against an 1800s threshold. _post's pre-write read is exempt.
    if [ $((tick % 4)) -eq 0 ]; then
      _bus_refresh
    fi
  done
  ;;
pr-watch)
  # pr-watch <N> [--repo owner/name] [--timeout S] [--interval S]
  # Thin bus bridge over the standalone `pr-watch` binary, which owns the park,
  # the change signals and the per-PR cursor — and needs no crew id at all. All
  # this adds is the post: addressed to this crew's dispatcher, so an armed
  # `crew watch` wakes. Stdout stays the event, so the wrapper still composes.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  ev=$(pr-watch "$@")
  # Empty stdout is pr-watch's timeout marker, not a failure — nothing to post.
  [ -n "$ev" ] || exit 0
  mkdir -p "$dir"
  jq -nc --arg crew "$crew" --arg from "pr-watch:${1:-}" --arg body "$ev" \
    '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
        kind:"msg", body:$body}' >>"$log"
  printf '%s\n' "$ev"
  ;;
reap)
  # Reclaim window + worktree for workers whose PR has landed. No crew filter:
  # the workers worth reaping are precisely the ones from earlier dispatcher
  # sessions, so scoping to the current crew id would skip every real candidate.
  #
  # The PR — not elapsed time — is the gate. A worker sits in `done` for as long
  # as its PR takes to merge, answering review comments and fixing CI the whole
  # time; a time-based sweep would delete live work.
  quiet=""
  dry=""
  idle=3600
  while [ $# -gt 0 ]; do
    case "$1" in
    --quiet) quiet=1 ;;
    --dry-run) dry=1 ;;
    --idle)
      [ -n "${2:-}" ] || {
        echo "crew: --idle needs a value in seconds" >&2
        exit 1
      }
      idle="$2"
      shift
      ;;
    *)
      echo "crew: reap takes --quiet, --dry-run and --idle S (got '$1')" >&2
      exit 1
      ;;
    esac
    shift
  done
  case "$idle" in '' | *[!0-9]*)
    echo "crew: --idle must be a non-negative integer number of seconds" >&2
    exit 1
    ;;
  esac
  [ -f "$log" ] || exit 0
  # say: outcomes, always. note: kept-worker bookkeeping, silenced under --quiet
  # so the dispatch call site stays silent unless something actually happened.
  say() { echo "crew reap: $1"; }
  note() { [ -n "$quiet" ] || echo "crew reap: $1"; }

  # Idle release: a session that reached a terminal state but whose window is
  # still sitting there keeps the tree occupied, and the PR gate below deliberately
  # will not touch it while its PR is open. Kill the WINDOW only — the worktree
  # stays, because a worker legitimately sits in `done` for as long as its PR
  # takes to merge (#17).
  while IFS=$'\t' read -r rbranch rsession rstate; do
    [ -n "$rbranch" ] || continue
    rwt=$(git worktree list --porcelain |
      awk -v b="refs/heads/$rbranch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    [ -n "$rwt" ] && [ -d "$rwt" ] || continue
    rocc=$(_occupants "$rwt")
    [ "$rocc" != '[]' ] || continue
    for w in $(printf '%s' "$rocc" | jq -r '.[].window'); do
      if [ -n "$dry" ]; then
        say "would release $w at $rwt ($rbranch $rstate)"
        continue
      fi
      tmux kill-window -t "$w" 2>/dev/null || true
      say "released $w at $rwt ($rbranch $rstate)"
    done
    [ -n "$dry" ] || jq -nc --arg branch "$rbranch" --arg session "$rsession" \
      --arg state "$rstate" --argjson occ "$rocc" \
      '{ts:(now*1000|floor), kind:"release", branch:$branch, session:$session,
          state:$state, windows:($occ|map(.window))}' >>"$log"
  done <<EOF
$(jq -s -r --argjson idle "$idle" '
    def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
    def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
    # A dispatch posts a "claim" for a branch before its window even exists.
    # It has no `body`, so it can never pass the terminal-state filter below —
    # it can only mask a stale prior `done` by winning the per-branch
    # max_by(.ts), never cause a release on its own.
    map(select((.kind=="status" or .kind=="claim") and ((.from // "") | startswith("worker:"))))
    | group_by(.from) | map(max_by(.ts))
    | group_by(.from | wid_branch) | map(max_by(.ts))
    | map(select(.body.state as $st | (["done","failed","exited"] | index($st)) != null))
    | map(select((((now*1000) - .ts) / 1000) >= $idle))
    | .[] | [(.from | wid_branch), ((.from | wid_session) // "-"), .body.state] | @tsv' "$log")
EOF
  # gh reads PR state; wt owns the worktree layout, so it does the removal and
  # resolves from the ambient session PATH (same as in dispatch) — hence checked.
  for tool in gh wt; do
    command -v "$tool" >/dev/null || {
      note "needs $tool"
      exit 0
    }
  done

  # Latest status per worker across every crew; keep the terminal `done` ones.
  # pr_url is carried forward because the `done` event itself drops it (same
  # reason roster does this).
  candidates=$(jq -s -r '
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      map(select(.kind=="status" and ((.from // "") | startswith("worker:"))))
      | group_by(.from) | map(
          (max_by(.ts)) as $latest
          | {branch: ($latest.from | wid_branch),
             ts: $latest.ts,
             state: $latest.body.state,
             pr_url: (map(.body.pr_url) | map(select(. != null)) | last)})
      | group_by(.branch) | map(sort_by(.ts) | last)
      | map(select(.state == "done"))
      | .[] | [.branch, (.pr_url // "-")] | @tsv' "$log")
  [ -n "$candidates" ] || {
    note "nothing done to reap"
    exit 0
  }

  # Panes running an engine, to tell "finished worker" from "someone is in there
  # right now". Same path-keyed idiom as roster: window names are rewritten by
  # lazytmux, so the worktree path is the only stable handle.
  live=$(tmux list-panes -a -F '#{pane_current_command} #{pane_current_path}' 2>/dev/null || true)

  reaped=0
  while IFS=$'\t' read -r branch pr; do
    [ -n "$branch" ] || continue
    wtpath=$(git worktree list --porcelain |
      awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    [ -n "$wtpath" ] && [ -d "$wtpath" ] || continue

    if [ "$pr" = "-" ]; then
      note "keeping $branch — done but no PR on the bus"
      continue
    fi
    pr_state=$(gh pr view "$pr" --json state --jq .state 2>/dev/null || true)
    case "$pr_state" in
    MERGED | CLOSED) ;;
    "")
      note "keeping $branch — could not read PR state ($pr)"
      continue
      ;;
    *)
      note "keeping $branch — PR $pr_state"
      continue
      ;;
    esac

    case "$live" in
    *"claude $wtpath"* | *"codex $wtpath"* | *"cursor-agent $wtpath"*)
      note "keeping $branch — an engine is still running there"
      continue
      ;;
    esac
    # Never remove the worktree the caller is standing in — it would leave the
    # invoking shell (or dispatch itself) on a path that no longer exists.
    case "$PWD/" in
    "$wtpath"/*)
      note "keeping $branch — it is the current worktree"
      continue
      ;;
    esac

    # Check for leftover work BEFORE touching anything. `wt remove` refuses a
    # dirty worktree on its own, but discovering that only after trashing the
    # task doc below would strip a worktree that then survives.
    dirt=$(git -C "$wtpath" status --porcelain | grep -v '^?? WORKER_TASK\.md$' || true)
    if [ -n "$dirt" ]; then
      note "keeping $branch — uncommitted changes"
      continue
    fi

    if [ -n "$dry" ]; then
      say "would reap $branch ($pr_state) @ $wtpath"
      continue
    fi

    # The task doc is dispatch's own artifact, but it is untracked — left in
    # place it reads as uncommitted work and `wt remove` refuses the worktree.
    # gtrash so a post-mortem can still recover it.
    if [ -f "$wtpath/WORKER_TASK.md" ]; then
      gtrash put "$wtpath/WORKER_TASK.md" >/dev/null 2>&1 || true
    fi
    # Kill the window ourselves rather than leaning on worktrunk's post-remove
    # hook: that hook short-circuits under $CLAUDECODE, so relying on it would
    # make cleanup work for codex/cursor dispatchers only (#123 in reverse).
    for wid in $(tmux list-windows -a -F '#{window_id} #{pane_current_path} #{@worktree}' 2>/dev/null |
      awk -v p="$wtpath" '$2 == p || $3 == p {print $1}'); do
      tmux kill-window -t "$wid" 2>/dev/null || true
    done
    # No -f: work is never deleted just because a PR merged — wt refuses a dirty
    # worktree, backing up the pre-check above. Branch deletion is wt's call: it
    # keeps unmerged ones. --no-hooks because the window is already gone.
    if wt remove --foreground --no-hooks "$branch" >/dev/null 2>&1; then
      reaped=$((reaped + 1))
      say "reaped $branch ($pr_state)"
      jq -nc --arg branch "$branch" --arg pr "$pr" --arg pr_state "$pr_state" --arg wt "$wtpath" \
        '{ts:(now*1000|floor), kind:"reap", branch:$branch, pr:$pr, pr_state:$pr_state, worktree:$wt}' \
        >>"$log"

      # Release the claim: drop `dispatched` from the issue(s) this PR
      # closes. Best-effort — a Linear PR closes no GitHub issue, and any gh
      # failure here must not block the sweep. The resolve call logs its own
      # failure rather than swallowing it, so it can't be confused with "no
      # closing issues" and leave the label stuck with no trace.
      if ! closing_issues=$(gh pr view "$pr" --json closingIssuesReferences \
        --jq '.closingIssuesReferences[].number' 2>&1); then
        note "could not resolve closing issues for PR $pr ($branch): $closing_issues"
        closing_issues=""
      fi
      for issue in $closing_issues; do
        gh issue edit "$issue" --remove-label dispatched >/dev/null 2>&1 ||
          note "could not remove the dispatched label from #$issue ($branch)"
      done
    else
      say "keeping $branch — wt remove failed"
    fi
  done <<EOF
$candidates
EOF
  [ -n "$dry" ] || [ "$reaped" -gt 0 ] || note "nothing reclaimed"
  ;;
*)
  echo "usage: crew id | identity <branch> | occupants <worktree-path> | status <from> <state> [detail] [pr] | msg <from> <to> <body> | reply <to> <body> | await <agent> [--timeout S] [--interval S] | register [pid] | deregister | watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] | sessions <branch> [--crew ID] | roster [crew] | inbox <agent> [crew] [--since TS] | stall-watch <worker-id> --pane <id> [--grace S] [--stall S] [--window S] [--interval S] | pr-watch <N> [--repo owner/name] [--timeout S] [--interval S] | log [crew] | report [crew] | rate [--report [--json]] | reap [--quiet] [--dry-run] [--idle S]" >&2
  exit 1
  ;;
esac
