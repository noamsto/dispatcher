# shellcheck shell=bash
# dispatch-resume — relaunch the worker whose worktree you are standing in,
# continuing its own engine session. Reached as `dispatch resume`, which execs
# this binary (dispatch.sh intercepts the subcommand).
#
# Its own file rather than a mode inside dispatch.sh: resume skips the
# issue claim, branch creation, task-document rewrite and new-window paths
# entirely, and re-runs only the gates it names. The shebang and
# `set -euo pipefail` are prepended by writeShellApplication.

usage() {
  echo "usage: dispatch resume [--agent claude|codex|cursor|pi] [--model M] [--effort E] [--mcp <profile>] [--fresh] [--print] [--ignore-budget] [--ignore-map] [extra prompt...]" >&2
}

fresh=""
do_print=""
ignore_budget=""
ignore_map=""
agent_flag=""
model_flag=""
effort_flag=""
mcp_flag_val=""
extra=""

while [ $# -gt 0 ]; do
  case "$1" in
  --agent)
    agent_flag="${2:-}"
    [ -n "$agent_flag" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --model)
    model_flag="${2:-}"
    [ -n "$model_flag" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --effort)
    effort_flag="${2:-}"
    [ -n "$effort_flag" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --mcp)
    mcp_flag_val="${2:-}"
    [ -n "$mcp_flag_val" ] || {
      usage
      exit 1
    }
    shift 2
    ;;
  --fresh)
    fresh=1
    shift
    ;;
  --print)
    do_print=1
    shift
    ;;
  --ignore-budget)
    ignore_budget=1
    shift
    ;;
  --ignore-map)
    ignore_map=1
    shift
    ;;
  -*)
    usage
    exit 1
    ;;
  *)
    extra="${extra:+$extra }$1"
    shift
    ;;
  esac
done

git rev-parse --git-common-dir >/dev/null 2>&1 || {
  echo "dispatch resume: not in a git repository" >&2
  exit 1
}

wt_path="$(git rev-parse --show-toplevel)"
task_doc="$wt_path/WORKER_TASK.md"

# Task document first: the overwhelmingly common mistake is running this from
# the main checkout, which has no WORKER_TASK.md, and "not a worker's worktree"
# says more there than "primary worktree" would.
[ -f "$task_doc" ] || {
  echo "dispatch resume: no WORKER_TASK.md in $wt_path — this is not a dispatched worker's worktree. To start a new worker, use 'dispatch <tier> <model> --effort <e> <title>'." >&2
  exit 1
}

# Then the primary worktree, which catches the remaining case: a stray
# WORKER_TASK.md in the main checkout must not make this look legitimate. A
# worker there would run in the main checkout, which dispatch.sh refuses on its
# own resume path for the same reason.
# awk reads to EOF on purpose — an early exit SIGPIPEs git under pipefail.
primary_wt="$(git worktree list --porcelain | awk '/^worktree /{if (!p) p=$2} END{print p}')"
if [ "$wt_path" = "$primary_wt" ]; then
  echo "dispatch resume: $wt_path is the primary worktree — a worker must not run in the main checkout. cd into the worker's worktree and retry." >&2
  exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" != HEAD ] || {
  echo "dispatch resume: detached HEAD — a worker resumes onto its own branch. Check the branch out and retry." >&2
  exit 1
}

# Header reader. `cut -d' ' -f2-` keeps values containing spaces (title), and
# -m1 pins the first occurrence so a value echoed inside the ## Task body
# cannot shadow the header.
_hdr() { grep -m1 "^$1: " "$task_doc" | cut -d' ' -f2- || true; }

agent="${agent_flag:-$(_hdr engine)}"
model="${model_flag:-$(_hdr model)}"
effort="${effort_flag:-$(_hdr effort)}"
mcp_profile="${mcp_flag_val:-$(_hdr mcp)}"
tier="$(_hdr tier)"
crew_id="$(_hdr crew_id)"
agent_name="$(_hdr agent_name)"
prev_worker_id="$(_hdr worker_id)"
grid_roles="$(_hdr roles)"

# The launch tuple is what makes a resume faithful; without it we would be
# guessing at a model and effort the first dispatch already decided.
missing=""
[ -n "$agent" ] || missing="${missing:+$missing }engine"
[ -n "$model" ] || missing="${missing:+$missing }model"
[ -n "$effort" ] || missing="${missing:+$missing }effort"
[ -n "$crew_id" ] || missing="${missing:+$missing }crew_id"
[ -n "$tier" ] || missing="${missing:+$missing }tier"
[ -z "$missing" ] || {
  echo "dispatch resume: $task_doc header is missing: $missing — pass --agent/--model/--effort explicitly, or re-dispatch this branch." >&2
  exit 1
}

case "$agent" in
claude | codex | cursor | pi) ;;
*)
  echo "dispatch resume: unknown engine '$agent' in the task header — pass --agent claude|codex|cursor|pi" >&2
  exit 1
  ;;
esac

case "$tier" in
trivial | standard | deep) ;;
*)
  echo "dispatch resume: unknown tier '$tier' in the task header — expected trivial, standard or deep" >&2
  exit 1
  ;;
esac

# mcp is claude-only, and this is the one gate the precheck below cannot make:
# passing --mcp there would have dispatch resolve and validate the config file
# too, which Task 6 must do anyway to build the launch flag.
if [ "$agent" != claude ] && [ -n "$mcp_profile" ]; then
  echo "dispatch resume: mcp is claude-only; codex/cursor/pi base MCP comes from their own profile" >&2
  exit 1
fi

profile="${DISPATCH_PROFILE:-personal}"

mcp_arg=""
if [ -n "$mcp_profile" ]; then
  case "$mcp_profile" in
  analytics) mcp_file="$HOME/.config/claude-code/mcp-posthog.json" ;;
  *)
    echo "dispatch resume: unknown mcp profile '$mcp_profile' (valid: analytics)" >&2
    exit 1
    ;;
  esac
  [ -f "$mcp_file" ] || {
    echo "dispatch resume: mcp $mcp_profile config not found at $mcp_file" >&2
    exit 1
  }
  mcp_arg="--mcp-config $mcp_file"
fi

xreview_mcp=""
if [ "$profile" = work ] && [ "$agent" = claude ] && [ "$tier" = deep ]; then
  xreview_mcp="--mcp-config $HOME/.config/claude-code/mcp-codex.json"
fi

# Every other pre-scaffold gate is dispatch's, run through its precheck exit
# so there is exactly one copy of the profile, model-shape, effort-ceiling,
# budget and rung rules. `dispatch` resolves from the ambient PATH: it lists
# dispatch-resume in runtimeInputs for the `resume` exec, so naming it in ours
# would be an eval-time cycle.
command -v dispatch >/dev/null 2>&1 || {
  echo "dispatch resume: dispatch is not on PATH — both are installed together by the home-manager module" >&2
  exit 1
}
precheck=(--effort "$effort" --agent "$agent" --crew-id "$crew_id")
[ -n "$ignore_budget" ] && precheck+=(--ignore-budget)
# The tier↔model pair was adjudicated when this worker was first dispatched;
# only an explicit --model is a fresh choice that deserves re-gating.
if [ -z "$model_flag" ] || [ -n "$ignore_map" ]; then
  precheck+=(--ignore-map)
fi
DISPATCH_PRECHECK=1 dispatch "$tier" "$model" "${precheck[@]}" "resume precheck" || exit 1

# Placement. dispatch always opens a fresh window and refuses when anything
# already sits at the worktree — including a pane with an empty @crew_name,
# i.e. a human in a plain shell, which is exactly whoever runs this command.
# Inheriting that would refuse the primary use case, so resume reuses the pane
# that is already there and gives up the anti-stacking refusal in trade.
#
# Keyed on pane_current_path, not the window name: lazytmux renames worker
# windows, so the name dispatch assigned is long gone by now.
win=""
pane=""
pane_cmd=""
reused=""
while IFS=$'\t' read -r cand_win cand_pane cand_path cand_cmd _cand_name; do
  [ -n "$cand_win" ] || continue
  [ "$cand_path" = "$wt_path" ] || continue
  win="$cand_win"
  pane="$cand_pane"
  pane_cmd="$cand_cmd"
  reused=1
  break
done <<PANES
$(tmux list-panes -a -F '#{window_id}	#{pane_id}	#{pane_current_path}	#{pane_current_command}	#{@crew_name}' 2>/dev/null || true)
PANES

# Cheap partial guard (#111): refuse a pane that is still running an engine.
# `crew engine-cmd` shares crew.sh's own nix-wrapper-aware matcher
# (_is_engine_cmd) rather than duplicating it here. Deliberately a
# command-name sniff, not the stronger bus-state gate dispatch.sh uses for its
# own placement refusal (dispatch.sh:699-711) — see that gate's own comment
# for why its engine count is only advisory.
if [ -n "$reused" ] && crew engine-cmd "$pane_cmd" 2>/dev/null; then
  echo "dispatch resume: $wt_path's pane ($pane) is running $pane_cmd — a worker session is already alive there. Attach to it instead of resuming (tmux select-window -t $win), or wait for it to exit/finish first." >&2
  exit 1
fi

# --print is a dry run: report the placement the lookup above already found
# and stop before anything below opens a window or restyles a pane. On the
# create path there is no window or pane id yet, so those report as "-" and
# placement: create carries the meaning.
if [ -n "$do_print" ]; then
  printf 'branch: %s\nworktree: %s\nengine: %s\nmodel: %s\neffort: %s\nmcp: %s\ntier: %s\ncrew_id: %s\nagent_name: %s\nprev_worker_id: %s\ncontinue: %s\nwindow: %s\npane: %s\nplacement: %s\n' \
    "$branch" "$wt_path" "$agent" "$model" "$effort" "$mcp_profile" \
    "$tier" "$crew_id" "$agent_name" "$prev_worker_id" \
    "$([ -n "$fresh" ] && echo false || echo true)" \
    "${win:--}" "${pane:--}" "$([ -n "$reused" ] && echo reuse || echo create)"
  exit 0
fi

if [ -z "$pane" ]; then
  sanitized="${branch//\//-}"
  # Same client-geometry handling as dispatch: a detached new-window otherwise
  # inherits tmux's fallback size, and codex's startup banner never redraws.
  client_target=()
  [ -n "${TMUX_PANE:-}" ] && client_target=(-t "$TMUX_PANE")
  client_size="$(tmux display-message -p "${client_target[@]}" '#{client_width} #{client_height} #{status}' 2>/dev/null || true)"
  client_width=""
  client_height=""
  if [[ $client_size =~ ^([1-9][0-9]*)[[:space:]]+([1-9][0-9]*)[[:space:]]+(off|on|[0-9]+)$ ]]; then
    client_width="${BASH_REMATCH[1]}"
    client_height="${BASH_REMATCH[2]}"
    case "${BASH_REMATCH[3]}" in
    off) status_rows=0 ;;
    on) status_rows=1 ;;
    *) status_rows="${BASH_REMATCH[3]}" ;;
    esac
    client_height=$((client_height - status_rows))
    if ((client_height <= 0)); then
      client_width=""
      client_height=""
    fi
  fi
  read -r win pane < <(tmux new-window -d -c "$wt_path" -n "$sanitized" -P -F '#{window_id} #{pane_id}')
  if [ -n "$client_width" ]; then
    tmux resize-window -t "$win" -x "$client_width" -y "$client_height"
  fi
fi

if [ -z "$pane" ]; then
  echo "dispatch resume: could not resolve a tmux pane for $wt_path — is tmux running?" >&2
  exit 1
fi

# Identity surfaces. Re-stamped on both paths: a hand-made window carries none,
# and a reused worker window may have been renamed since.
agent_color="$(crew identity "$branch" | jq -r .tmux)"
tmux set-window-option -t "$win" @crew_name "$agent_name"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold] "

PROTOCOL_DIR="${DISPATCHER_PROTOCOL_DIR:-@protocolDir@}"
kind="$(_hdr kind)"
plan_val="$(_hdr plan)"

# Session identity. A resume gets a NEW session id and therefore a new
# worker_id: the pane, the watchdog and the bus rows are all new even when the
# conversation is not. dispatch.sh:832 owns the same shape.
session="${DISPATCH_SESSION_ID:-s$(date +%s)-$$}"
worker_id="worker:$branch#$session"

crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
mkdir -p "$crew_dir"

# crew.sh's atomic-append helper, duplicated for the same reason dispatch.sh
# duplicates it: this file builds as its own writeShellApplication with no
# shared lib, and a bare `printf >>` is not one write(2) (#55, #61).
_bus_append() { printf '%s\n' "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"; }

# Dispatcher liveness. `crew register` writes the pid, and `crew deregister`
# removes the whole directory on a clean exit — so absent means gone, a dead
# pid means it crashed, and only a live pid is a dispatcher still watching.
# A resume never mints or adopts a crew: it keeps posting under the crew_id in
# the task document, which is what `crew adopt` is for on the other side.
dispatcher_live=""
dispatcher_pane_new=""
cdir="$crew_dir/crews/$crew_id"
if [ -d "$cdir" ]; then
  epid="$(cat "$cdir/pid" 2>/dev/null || true)"
  case "$epid" in
  '' | *[!0-9]* | 0) ;;
  *)
    if kill -0 "$epid" 2>/dev/null; then
      dispatcher_live=1
      dispatcher_pane_new="$(cat "$cdir/pane" 2>/dev/null || true)"
    fi
    ;;
  esac
fi

# Rewrite header lines in place, never the whole document: the worker may have
# been handed a spec, and this header is the record we just read. worker_id
# MUST move — it carries the session, so leaving the old one would have the
# worker post under a dead bus identity. `resume:` is not always present
# (dispatch.sh only stamps it on a branch re-dispatch), so it needs an append
# path: absent a matching line, insert one at the end of the header block,
# just before the first blank line that separates it from the task body.
_hdr_set() { # $1=field  $2=value
  awk -v f="$1" -v v="$2" '
    !done && $0 ~ "^" f ": " { print f ": " v; done = 1; next }
    !done && /^$/ { print f ": " v; done = 1 }
    { print }
    END { if (!done) print f ": " v }
  ' "$task_doc" >"$task_doc.tmp" && mv "$task_doc.tmp" "$task_doc"
}
_hdr_set worker_id "$worker_id"
_hdr_set resume true
if [ -n "$dispatcher_live" ] && [ -n "$dispatcher_pane_new" ]; then
  _hdr_set dispatcher_pane "$dispatcher_pane_new"
fi

# The resume row. New kind: without it a worker resumed four times reports as
# one run, and the ratings rollup attributes the whole cost and latency to a
# single session. prev_worker_id is what chains the sessions back together.
line=$(jq -nc --arg crew "$crew_id" --arg branch "$branch" \
  --arg worker "$worker_id" --arg prev "$prev_worker_id" \
  --arg engine "$agent" --arg model "$model" --arg session "$session" \
  --argjson continued "$([ -n "$fresh" ] && echo false || echo true)" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"resume", branch:$branch,
     worker_id:$worker, prev_worker_id:$prev, engine:$engine, model:$model,
     session:$session, continued:$continued}')
_bus_append "$crew_dir/events.jsonl" "$line"

# Clears a stale exited/failed/done roster row so the crew reads as live again.
CREW_ID="$crew_id" crew status "$worker_id" working resumed || true

# A live dispatcher is told, deliberately. `crew watch` wakes on a message to
# the dispatcher but its default --states exclude `working`, so a status post
# alone would leave a dispatcher that wrote this worker off as failed still
# believing it dead — and free to re-dispatch the task onto this branch.
if [ -n "$dispatcher_live" ]; then
  CREW_ID="$crew_id" crew msg "$worker_id" "dispatcher:$crew_id" \
    "resumed on $branch (engine $agent, model $model) — this worker is live again, do not re-dispatch it" || true
  echo "dispatcher: reattached to live crew $crew_id"
else
  echo "dispatcher: none live for crew $crew_id — running solo (a later dispatcher can 'crew adopt $crew_id')"
fi

# The reorient prompt. dispatch's own resume_note sends a worker to SPEC.md and
# PLAN.md because it has no transcript to stand on; with the conversation
# restored the risk inverts, and the danger is trusting a stale last plan and
# redoing finished work. --fresh keeps dispatch's wording, since a fresh launch
# is exactly the no-transcript case that note was written for.
# No apostrophes anywhere in these strings.
if [ -n "$fresh" ]; then
  reorient=" You are resuming an interrupted run on this branch, not starting it: do not re-run the spec or plan phases. Read SPEC.md and PLAN.md (repo root or docs/superpowers/) and git status before anything else, then continue from the first unfinished step. Check whether this branch already has an open PR before you push, and push to that PR instead of opening a second one."
else
  reorient=" You were interrupted mid-task and this session has been resumed. Before anything else, establish where you actually got to from git log, git status and any open PR on this branch — do not trust the last plan in your transcript as your current position. Then continue from the first genuinely unfinished step. If this branch already has an open PR, push to it rather than opening a second one."
fi
reorient="${reorient//\'/}"
[ -n "$extra" ] && reorient="$reorient ${extra//\'/}"

plan_note=""
if [ "$plan_val" = provided ]; then
  plan_note=" The task doc is your plan of record — extract the steps and implement; do not re-plan or re-critique it."
fi

push_mandate=" Push when pre-push passes; open a PR."
if [ "$kind" = review ]; then
  push_mandate=" Review only — do not edit, commit, push, or open a PR; post one COMMENT review and report to the bus."
fi

grid_note=""
if [ -n "$grid_roles" ]; then
  grid_note=" You lead a role grid: role panes ($grid_roles) may still be parked in this window. Follow WORKER_PROTOCOL.md Grid mode and address them through the crew bus."
fi

# Execute subagents never read WORKER_PROTOCOL.md. Codex/cursor/pi workers must
# stamp process-authority into every execute-subagent prompt so a fresh subagent
# cannot re-derive process via skills. Claude gets the same idea from rule 1 +
# the Agent tool; this clause is only for engines whose spawn prompt is the
# sole carrier.
process_authority=" Process authority: WORKER_PROTOCOL.md governs this worker session. When spawning execute subagents, grant implementation authority only — tell them not to re-derive worker process via skills, not to open PRs, and not to act as the worker. When spawning review subagents, grant review authority only — tell them not to fix the code, not to commit or push, not to open PRs, and not to act as the worker."
if [ "$agent" = codex ] && [ "$effort" = ultra ]; then
  process_authority="$process_authority Session effort is ultra — Codex automatic delegation is the orchestration layer; do not add a second harness execute-subagent orchestration on top."
fi

# Codex execute-subagent effort: one rung below the session, floor at low,
# never ultra (ultra auto-delegates and must not nest). Model versions live in
# dispatch-orchestration.md — dispatch sets guardrails only.
codex_subagent_effort="$effort"
case "$effort" in
ultra) codex_subagent_effort=max ;;
max) codex_subagent_effort=xhigh ;;
xhigh) codex_subagent_effort=high ;;
high) codex_subagent_effort=medium ;;
medium) codex_subagent_effort=low ;;
low) codex_subagent_effort=low ;;
esac

# Printed so the dispatcher can address this session in the gap before the worker
# boots — its startup drain is unbounded, so a scoping note posted now still lands.
echo "worker_id: $worker_id"

if [ "$agent" = codex ]; then
  cont="resume --last"
  [ -n "$fresh" ] && cont=""
  tmux send-keys -t "$pane" \
    "CREW_WORKER_ID=$worker_id CREW_ID=$crew_id codex $cont --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default -c agents.enabled=true -c agents.max_concurrent_threads_per_session=3 -c agents.default_subagent_reasoning_effort=$codex_subagent_effort --dangerously-bypass-approvals-and-sandbox 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}'" Enter
elif [ "$agent" = cursor ]; then
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  tmux send-keys -t "$pane" \
    "CREW_WORKER_ID=$worker_id CREW_ID=$crew_id CURSOR_CLI_INDEXED_GREP=0 cursor-agent $cont --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}'" Enter
elif [ "$agent" = pi ]; then
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  tmux send-keys -t "$pane" \
    "CREW_WORKER_ID=$worker_id CREW_ID=$crew_id pi $cont --name $agent_name --model $model --thinking $effort --append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.md --no-approve 'Read WORKER_TASK.md and continue it.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}'" Enter
else
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  # Re-passing --append-system-prompt-file matters on a continue: it forces
  # --system-prompt-snapshot off, so WORKER_PROTOCOL.md is applied fresh rather
  # than replayed from the conversation's recorded prompt.
  tmux send-keys -t "$pane" \
    "CREW_WORKER_ID=$worker_id CREW_ID=$crew_id claude $cont --name $agent_name --model $model --effort $effort $mcp_arg $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and continue it.${push_mandate}${plan_note}${reorient}${grid_note}'" Enter
fi

# Re-arm the stall watchdog: the original self-exited when it saw the terminal
# state, and a resumed worker can wedge exactly the same way.
CREW_ID="$crew_id" nohup crew stall-watch "$worker_id" --pane "$pane" --engine "$agent" >/dev/null 2>&1 &
