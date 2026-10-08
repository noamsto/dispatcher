#!/usr/bin/env bash
# Terminal-status backstop: if a worker stops without having signalled, ping the
# dispatcher and record `exited` so the roster does not strand a `working` entry.
#
# Reads the hook JSON on stdin. Two modes, because the engines expose different
# events:
#   (default)    claude/codex SessionEnd — the session is over for good.
#   --turn-end   cursor `stop` — cursor has no session-end event, only end-of-turn.
#                A seeded worker runs its whole task in one turn, so a turn that
#                ends without a terminal status is the same silent stop. But a
#                `blocked` worker also ends its turn to wait for an answer, and
#                it is still alive — so `blocked` counts as a meaningful end here
#                and is never overridden. `stop` fires per turn and per subagent,
#                so only a stop whose pane engine is already gone posts `exited`
#                (#531 guard below); a parked live worker is left to the
#                stall-watch's prompt detector.
set -euo pipefail

mode=session-end
if [[ ${1:-} == --turn-end ]]; then
  mode=turn-end
fi

event="$(cat)"

# An in-process session switch is not a session end: pi emits session_shutdown
# for /new, /resume, /fork (AgentSessionRuntime.teardownCurrent) and an extension
# reload, and Claude Code SessionEnd reason `clear` or `resume`, all while the
# process stays alive. Skip only these known reasons — an unknown or missing
# reason still posts `exited`, because a missed `exited` costs the watchdog
# 30+ minutes, while a duplicate is harmless.
if [ "$mode" = session-end ]; then
  case "$(jq -r '.reason // empty' <<<"$event")" in
  new | resume | fork | reload | clear) exit 0 ;;
  esac
fi
# Cursor leaves .cwd empty and delivers the workspace in workspace_roots[0].
cwd="$(jq -r 'if (.cwd // "") != "" then .cwd else (.workspace_roots[0] // empty) end' <<<"$event")"
[[ -f "$cwd/WORKER_TASK.md" ]] || exit 0

# Only a session that can name itself may speak for this branch. `dispatch` puts
# CREW_WORKER_ID in the worker window's environment, so a session without one is
# something else in the same worktree — another engine's subagent session, a human's
# auxiliary pane — and must not post a terminal status over a live worker (#69).
# Probing for a live pane instead would deadlock: the worker's own SessionEnd blocks
# on its own hooks.
[[ -n ${CREW_WORKER_ID:-} ]] || exit 0

# A grid role pane inherits the lead's CREW_WORKER_ID (so its engine wrapper loads
# the worker config), but a role session ending is not the lead ending: posting
# `exited` under the lead's id would strand or reclaim a live worker. The pane's
# own `dispatch --role-exited` reports a dead role under its role id instead.
[[ -z ${CREW_ROLE_ID:-} ]] || exit 0

# _is_engine_cmd — byte-identical copy of crew.sh's table (standalone build; the
# byte-compare in tests/adapters.bats is what keeps the two in sync).
_is_engine_cmd() {
  local c="${1#.}"
  c="${c%-wrapped}"
  case "$c" in
  claude | codex | cursor-agent | node | pi) return 0 ;;
  esac
  return 1
}

# _engine_ancestors — count engine-named processes in this hook's ancestor chain,
# stopping at the pane boundary (the tmux server), so the broad `node` match is
# bounded to the pane's own subtree. A child session is nested inside the lead,
# so its end shows >=2 engines; the lead's own shows exactly 1. Non-blocking (no
# wait), so the #69 deadlock cannot occur; `ps` failing reads as 0 (fail-open).
_engine_ancestors() {
  local p="$PPID" n=0 c ppid depth=0
  while [ "$depth" -lt 32 ]; do
    depth=$((depth + 1))
    read -r ppid c < <(ps -o ppid=,comm= -p "$p" 2>/dev/null) || break
    [ -n "${c:-}" ] || break
    if _is_engine_cmd "$c"; then n=$((n + 1)); fi
    case "$c" in tmux*) break ;; esac
    case "$ppid" in '' | *[!0-9]* | 0 | 1) break ;; esac
    p="$ppid"
  done
  printf '%s' "$n"
}

# _pane_cmd — the lead pane's current foreground command, or empty. The same
# field `_pane_engine_alive` reads in crew.sh's stall-watch; CREW_NOTIFY_PROC_CMD
# overrides it for tests (mirrors CREW_STALL_PROC_CMD).
_pane_cmd() {
  local panes pid pcmd
  if [ -n "${CREW_NOTIFY_PROC_CMD:-}" ]; then
    eval "$CREW_NOTIFY_PROC_CMD" 2>/dev/null || true
    return 0
  fi
  [ -n "${TMUX_PANE:-}" ] || return 0
  panes="$(tmux list-panes -a -F '#{pane_id} #{pane_current_command}' 2>/dev/null || true)"
  while read -r pid pcmd; do
    if [ "$pid" = "$TMUX_PANE" ]; then
      printf '%s' "$pcmd"
      return 0
    fi
  done <<PANES
$panes
PANES
  return 0
}

# #531 — a child session ending is not the lead ending. The hook fires for any
# session sharing this window's environment, and cursor's native subagents
# inherit the lead's full CREW_WORKER_ID. Two independent detectors, either one
# suppresses: (1) nesting — the hook's ancestor chain has >=2 engine-named
# processes (the lead's own end has exactly 1); (2) a turn-end whose lead pane
# still runs an engine — `stop` fires per turn and per subagent, so that is not a
# death. (2) is turn-end-only: a default SessionEnd hook runs while its own engine
# is still alive, so a liveness check there would silence the genuine
# claude/codex/pi backstop.
if [ "$(_engine_ancestors)" -ge 2 ]; then exit 0; fi
if [ "$mode" = turn-end ] && _is_engine_cmd "$(_pane_cmd)"; then exit 0; fi

# crew backstop. `crew` is a PATH CLI now, but this hook stays self-contained
# (inline append) to avoid depending on PATH at SessionEnd; envelope matches
# crew's status event.
#
# crew.sh's atomic-append helper, duplicated (not sourced) for the reason
# above. A bare `printf >>` isn't one write(2), so concurrent writers to this
# shared log can splice a large line with another process's append (#55, #61).
_bus_append() {
  local p='' s1='' s2='' c=''
  if [ -s "$1" ]; then
    s1="$(wc -c <"$1" 2>/dev/null)" || true
    c="$(dd if="$1" bs=1 skip=$((s1 - 1)) count=1 2>/dev/null)" || true
    if [ -n "$c" ]; then
      s2="$(wc -c <"$1" 2>/dev/null)" || true
      [ "$s1" != "$s2" ] || p=$'\n'
    fi
  fi
  printf '%s%s\n' "$p" "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"
}

pane="$(grep -m1 '^dispatcher_pane:' "$cwd/WORKER_TASK.md" | cut -d' ' -f2 || true)"
branch="$(git -C "$cwd" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
crew_id="$(grep -m1 '^crew_id:' "$cwd/WORKER_TASK.md" | cut -d' ' -f2 || true)"
common="$(git -C "$cwd" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)"

# The worker id comes from the environment, never from WORKER_TASK.md: the doc
# is overwritten by the next dispatch on this worktree, so reading it here
# would post THIS session's `exited` against its successor (#17).
me="$CREW_WORKER_ID"
last=""
log=""
if [[ -n $crew_id && -n $common ]]; then
  log="$common/crew/events.jsonl"
  if [[ -f $log ]]; then
    last="$(jq -r --arg c "$crew_id" --arg m "$me" \
      'select(.crew_id==$c and .kind=="status" and .from==$m) | .body.state' "$log" 2>/dev/null | tail -1 || true)"
  fi
fi

silent=1
case "$mode:$last" in
*:done | *:failed | *:pr_open) silent=0 ;; # reached a meaningful end → don't override
turn-end:blocked) silent=0 ;;              # still alive, waiting on the dispatcher
esac

# A session that really ended is worth flagging either way; a turn that ended on
# a meaningful state is just the worker working, so stay quiet.
if [[ -n $pane ]] && { [[ $mode == session-end ]] || [[ $silent == 1 ]]; }; then
  if [[ $mode == session-end ]]; then
    msg="worker exited: $branch — check state"
  else
    msg="worker stopped without reporting: $branch — check state"
  fi
  tmux display-message -t "$pane" -d 4000 "${msg//#/##}" 2>/dev/null || true
fi

if [[ -n $log && $silent == 1 ]]; then
  mkdir -p "$common/crew"
  line="$(jq -nc --arg c "$crew_id" --arg m "$me" \
    '{ts:(now*1000|floor), crew_id:$c, from:$m, to:("dispatcher:"+$c), kind:"status", body:{state:"exited"}}')"
  _bus_append "$log" "$line"
fi
