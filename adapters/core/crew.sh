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

# A bus line MUST fit in one write(). `printf '%s\n' … >>"$log"` is atomic under
# O_APPEND only up to the 4096-byte stdio buffer; above it bash flushes in
# 4096-byte chunks and a concurrent append lands in the gap, splicing two records
# into one line (#20).
_LINE_MAX=4096
_ELIDED=' …[elided]'

# _shrink <text> <keep> — shorten <text> to roughly <keep> characters.
# A sink body (`metrics:`, `retro:`) is itself JSON, and cutting it as a blob ends
# the string mid-object: the enclosing bus line stays valid but the body no longer
# parses, so a reader loses the WHOLE record — every key, not just the long one —
# and `rate`'s fold used to die on it (#25). So shorten each long string leaf and
# re-encode instead, which keeps the record's shape and its short keys intact.
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
  printf '%s\n' "$line" >>"$log"
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
  # Sweep this repo's bus into the global ratings store. One record per RUN
  # (a run = a dispatch + the branch events until the next dispatch on that
  # branch). Append-only; readers fold last-wins by run_id. No crew filter —
  # ratings are cross-run/cross-crew evidence.
  [ -f "$log" ] || exit 0
  repo=$(git config --get remote.origin.url 2>/dev/null |
    sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##' || true)
  repo="${repo:-$(basename "$(git rev-parse --show-toplevel)")}"
  records=$(jq -s --arg repo "$repo" '
    (map(select(.kind=="dispatch"))) as $disp
    | [ ($disp | map(.branch) | unique)[] as $b
        | ($disp | map(select(.branch==$b)) | sort_by(.ts)) as $runs
        | range(0; ($runs|length)) as $i
        | $runs[$i] as $d
        | ($d.ts) as $t0
        | (if $i+1 < ($runs|length) then $runs[$i+1].ts else 9999999999999 end) as $t1
        | (map(select(
              .kind!="dispatch"
              and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
              and .ts >= $t0 and .ts < $t1))) as $ev
        | ($ev | map(select(.kind=="status"))) as $st
        | ($st | sort_by(.ts) | (.[-1] // null)) as $last
        | ($last.body.state) as $ls
        | ($st | map(.body.pr_url) | map(select(.!=null)) | last) as $pr
        | ($st | map(select(.body.state=="pr_open")) | sort_by(.ts) | (.[0] // null)) as $propen
        | ($st | map(select(.body.state=="blocked")) | length) as $blocked
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
            reached_pr: ($propen != null),
            pr_url: $pr,
            time_to_pr_ms: (if $propen != null then ($propen.ts - $t0) else null end),
            outcome: (
              if $pr != null then "pr_open"
              elif $ls == "failed" then "failed"
              elif $ls == "done" then "failed"
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
            reported_ok: (($m != null) and ($ls != null)),
            swept_at: (now*1000|floor)
          } ]' "$log")
  store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
  store="$store_dir/ratings.jsonl"
  mkdir -p "$store_dir"
  lockd="$store_dir/ratings.lock.d"
  if ! _lock_acquire "$lockd" "$$"; then
    echo "crew: ratings store busy" >&2
    exit 1
  fi
  trap '_lock_release "$lockd"' EXIT
  printf '%s' "$records" | jq -c '.[]' >>"$store"
  ;;
stall-watch)
  # stall-watch <worker-id> --pane <id> [--grace S] [--stall S] [--window S] [--interval S]
  # A detached liveness watchdog `dispatch` spawns per worker. The crew bus is
  # blind to a hung worker: a worker posts `working` once at launch then nothing
  # until a terminal state, so sticky `working` + climbing age is
  # indistinguishable from a slow-but-live worker — and the SessionEnd `exited`
  # backstop only fires when a session actually ENDS, which a wedged agent never
  # does (#103). This automates the manual `tmux capture-pane` that confirmed
  # that stall: sample the pane's visible output; if it doesn't change for
  # --stall seconds within the startup --window and the worker hasn't reached a
  # non-`working` state, post `failed` so `crew watch` wakes the dispatcher to
  # recover. Windowed to the startup phase (where the observed hang lives) so a
  # long, legitimately-quiet execute stage later isn't killed. Engine-agnostic,
  # but it assumes the engine streams to its pane — an engine that buffers until
  # completion looks stalled from here and gets failed on every real task, which
  # is exactly what `--output-format text` did to cursor workers.
  # CREW_STALL_SAMPLE_CMD overrides the pane sampler (its stdout is the "output",
  # its exit code is pane liveness) so the loop is testable without tmux.
  me="${1:-}"
  shift || true
  [ -n "$me" ] || {
    echo "crew: stall-watch <worker-id> --pane <id> [--grace S] [--stall S] [--window S] [--interval S]" >&2
    exit 1
  }
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  pane=""
  grace=45
  stall=300
  window=900
  interval=15
  while [ $# -gt 0 ]; do
    case "$1" in
    --pane)
      pane="${2:-}"
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
  # Sample the pane: print a hash of its visible output, return non-zero when the
  # pane is gone (worker ended — the `exited` backstop owns that case).
  _sample() {
    local out
    if [ -n "${CREW_STALL_SAMPLE_CMD:-}" ]; then
      out=$(eval "$CREW_STALL_SAMPLE_CMD" 2>/dev/null) || return 1
    else
      out=$(tmux capture-pane -p -t "$pane" 2>/dev/null) || return 1
    fi
    printf '%s' "$out" | cksum
  }
  # The worker proved it's alive once its latest status is anything past the
  # launch `working` heartbeat (progress, a deliberate `blocked` pause, or a
  # terminal state) — stop watching then.
  _progressed() {
    local st
    [ -f "$log" ] || return 1
    st=$(jq -r --arg c "$crew" --arg m "$me" \
      'select(.crew_id==$c and .kind=="status" and .from==$m) | .body.state' "$log" 2>/dev/null | tail -1 || true)
    case "$st" in
    "" | working) return 1 ;;
    *) return 0 ;;
    esac
  }
  start=$(date +%s)
  sleep "$grace"
  _progressed && exit 0
  last_hash=$(_sample) || exit 0
  last_change=$(date +%s)
  while :; do
    now=$(date +%s)
    [ $((now - start)) -ge "$window" ] && exit 0
    if [ $((now - last_change)) -ge "$stall" ]; then
      mkdir -p "$dir"
      line=$(jq -nc --arg crew "$crew" --arg from "$me" \
        --arg detail "stalled: no output for ${stall}s (suspected startup/indexing hang)" \
        '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
            kind:"status", body:{state:"failed", detail:$detail}}')
      printf '%s\n' "$line" >>"$log"
      exit 0
    fi
    sleep "$interval"
    _progressed && exit 0
    cur=$(_sample) || exit 0
    [ "$cur" != "$last_hash" ] && {
      last_hash="$cur"
      last_change=$(date +%s)
    }
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
    map(select(.kind=="status" and ((.from // "") | startswith("worker:"))))
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
    else
      say "keeping $branch — wt remove failed"
    fi
  done <<EOF
$candidates
EOF
  [ -n "$dry" ] || [ "$reaped" -gt 0 ] || note "nothing reclaimed"
  ;;
*)
  echo "usage: crew id | identity <branch> | occupants <worktree-path> | status <from> <state> [detail] [pr] | msg <from> <to> <body> | reply <to> <body> | await <agent> [--timeout S] [--interval S] | register [pid] | deregister | watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] | sessions <branch> [--crew ID] | roster [crew] | inbox <agent> [crew] [--since TS] | stall-watch <worker-id> --pane <id> [--grace S] [--stall S] [--window S] [--interval S] | pr-watch <N> [--repo owner/name] [--timeout S] [--interval S] | log [crew] | report [crew] | rate | reap [--quiet] [--dry-run] [--idle S]" >&2
  exit 1
  ;;
esac
