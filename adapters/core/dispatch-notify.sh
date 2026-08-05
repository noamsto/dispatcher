#!/usr/bin/env bash
# SessionEnd backstop: if a worker session ends without having signalled, ping the dispatcher.
# Reads the hook JSON on stdin (.cwd is delivered for SessionEnd).
set -euo pipefail

event="$(cat)"
cwd="$(jq -r '.cwd // empty' <<<"$event")"
[[ -f "$cwd/WORKER_TASK.md" ]] || exit 0

pane="$(grep -m1 '^dispatcher_pane:' "$cwd/WORKER_TASK.md" | cut -d' ' -f2 || true)"
branch="$(git -C "$cwd" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
if [[ -n $pane ]]; then
  tmux display-message -t "$pane" -d 4000 "worker exited: $branch — check state" 2>/dev/null || true
fi
# crew backstop: if the worker died before a terminal status, record `exited`
# so the roster doesn't strand a `working` entry. `crew` is a PATH CLI now, but
# this hook stays self-contained (inline append) to avoid depending on PATH at
# SessionEnd; envelope matches crew's status event.
crew_id="$(grep -m1 '^crew_id:' "$cwd/WORKER_TASK.md" | cut -d' ' -f2 || true)"
if [[ -n $crew_id ]]; then
  common="$(git -C "$cwd" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)"
  if [[ -n $common ]]; then
    log="$common/crew/events.jsonl"
    # The worker id comes from the environment, never from WORKER_TASK.md: the doc
    # is overwritten by the next dispatch on this worktree, so reading it here
    # would post THIS session's `exited` against its successor (#17). A session
    # launched before this change has no CREW_WORKER_ID and keeps the old id.
    me="${CREW_WORKER_ID:-worker:$branch}"
    last=""
    if [[ -f $log ]]; then
      last="$(jq -r --arg c "$crew_id" --arg m "$me" \
        'select(.crew_id==$c and .kind=="status" and .from==$m) | .body.state' "$log" 2>/dev/null | tail -1 || true)"
    fi
    case "$last" in
    done | failed | pr_open) : ;; # reached a meaningful end → don't override
    *)
      mkdir -p "$common/crew"
      line="$(jq -nc --arg c "$crew_id" --arg m "$me" \
        '{ts:(now*1000|floor), crew_id:$c, from:$m, to:("dispatcher:"+$c), kind:"status", body:{state:"exited"}}')"
      printf '%s\n' "$line" >>"$log"
      ;;
    esac
  fi
fi
