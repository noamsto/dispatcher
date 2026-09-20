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
#                and is never overridden.
set -euo pipefail

mode=session-end
if [[ ${1:-} == --turn-end ]]; then
  mode=turn-end
fi

event="$(cat)"
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

# crew backstop. `crew` is a PATH CLI now, but this hook stays self-contained
# (inline append) to avoid depending on PATH at SessionEnd; envelope matches
# crew's status event.
#
# crew.sh's atomic-append helper, duplicated (not sourced) for the reason
# above. A bare `printf >>` isn't one write(2), so concurrent writers to this
# shared log can splice a large line with another process's append (#55, #61).
_bus_append() { printf '%s\n' "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"; }

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
  tmux display-message -t "$pane" -d 4000 "$msg" 2>/dev/null || true
fi

if [[ -n $log && $silent == 1 ]]; then
  mkdir -p "$common/crew"
  line="$(jq -nc --arg c "$crew_id" --arg m "$me" \
    '{ts:(now*1000|floor), crew_id:$c, from:$m, to:("dispatcher:"+$c), kind:"status", body:{state:"exited"}}')"
  _bus_append "$log" "$line"
fi
