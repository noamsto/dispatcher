# shellcheck shell=bash
# dispatch — scaffold a worker: issue/ticket -> worktree -> task file -> baked agent.
# Native PATH tool (was a fish autoload function). Crew id is delivered
# explicitly (--crew-id > $CREW_ID > error); a binary can't export env back to
# its caller, so the old `set -gx CREW_ID` persistence trick is gone.
# The shebang + `set -euo pipefail` are prepended by writeShellApplication, so
# this file is only the function body (see crew.sh for the same pattern).

usage() {
  echo "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh|max|ultra> [--agent claude|codex|cursor|pi] [--mcp <profile>] [--roles <r1,r2,...>] [--plan provided|required] [--crew-id <id>] [LINEAR-ID|#N] <title...>" >&2
}

# Protocol directory. The env override is the dev loop: point it at a checkout
# and protocol edits take effect on the next dispatch with no rebuild. The
# default is substituted to a store path at build time.
PROTOCOL_DIR="${DISPATCHER_PROTOCOL_DIR:-@protocolDir@}"

tier="${1:-}"
model="${2:-}"
case "$tier" in
trivial | standard | deep) ;;
*)
  usage
  exit 1
  ;;
esac
[ -n "$model" ] || {
  usage
  exit 1
}
shift 2

# Leading options before the free-form title, order-independent. A LINEAR-ID or
# a GitHub issue number (#N / N) is detected by shape so the bare <title...>
# form still works.
agent=claude
effort=""
linear_id=""
gh_issue=""
mcp_profile=""
grid_roles=""
crew_id_flag=""
plan_val="required"
while [ $# -gt 0 ]; do
  case "$1" in
  --agent)
    agent="${2:-}"
    case "$agent" in
    claude | codex | cursor | pi) ;;
    *)
      echo "dispatch: --agent must be claude, codex, cursor, or pi" >&2
      exit 1
      ;;
    esac
    shift 2
    ;;
  --effort)
    effort="${2:-}"
    case "$effort" in
    low | medium | high | xhigh | max | ultra) ;;
    *)
      echo "dispatch: --effort must be low, medium, high, xhigh, max, or ultra" >&2
      exit 1
      ;;
    esac
    shift 2
    ;;
  --mcp)
    mcp_profile="${2:-}"
    [ -n "$mcp_profile" ] || {
      echo "dispatch: --mcp needs a profile (analytics)" >&2
      exit 1
    }
    shift 2
    ;;
  --roles)
    grid_roles="${2:-}"
    [ -n "$grid_roles" ] || {
      echo "dispatch: --roles needs a comma-separated list of roles" >&2
      exit 1
    }
    shift 2
    ;;
  --crew-id)
    crew_id_flag="${2:-}"
    [ -n "$crew_id_flag" ] || {
      echo "dispatch: --crew-id needs a value" >&2
      exit 1
    }
    shift 2
    ;;
  --plan)
    plan_val="${2:-}"
    case "$plan_val" in
    provided | required) ;;
    *)
      echo "dispatch: --plan must be provided or required" >&2
      exit 1
      ;;
    esac
    shift 2
    ;;
  *)
    if printf '%s' "$1" | grep -Eq '^[A-Z]{2,}-[0-9]+$'; then
      linear_id="$1"
      shift
    elif printf '%s' "$1" | grep -Eq '^#?[0-9]+$'; then
      gh_issue="${1#\#}"
      shift
    else
      break
    fi
    ;;
  esac
done

[ -n "$effort" ] || {
  echo "dispatch: --effort is required and must be judged independently from tier" >&2
  exit 1
}

# Crew id: explicit flag > inherited env > error. Launcher dispatchers inherit
# $CREW_ID from the claude process env; in-session dispatchers pass --crew-id.
crew_id="${crew_id_flag:-${CREW_ID:-}}"
[ -n "$crew_id" ] || {
  # shellcheck disable=SC2016  # $CREW_ID is documentation text, not an expansion
  echo 'dispatch: no crew id — pass --crew-id <id> (in-session) or run under a launcher/registered dispatcher ($CREW_ID)' >&2
  exit 1
}

# Work-only engine gate. $DISPATCH_PROFILE is set from osConfig.profile by
# home-manager (was a source-baked literal in the fish heredoc). Reject before
# scaffolding a worktree, so the failure is a clear message not a later
# `codex: command not found`.
profile="${DISPATCH_PROFILE:-personal}"
if [ "$agent" = codex ] && [ "$profile" != work ]; then
  echo "dispatch: --agent codex is work-profile only (no personal codex account)" >&2
  exit 1
fi
if [ "$agent" = cursor ] && [ "$profile" != work ]; then
  echo "dispatch: --agent cursor is work-profile only" >&2
  exit 1
fi
# claude's and pi's --effort top out at max; rejecting `ultra` here fails before
# the worktree and pane exist, instead of at worker launch.
if { [ "$agent" = claude ] || [ "$agent" = pi ]; } && [ "$effort" = ultra ]; then
  echo "dispatch: --effort ultra is codex-only; $agent tops out at max" >&2
  exit 1
fi
if [ "$agent" != claude ] && [ -n "$mcp_profile" ]; then
  echo "dispatch: --mcp is claude-only; codex/cursor/pi base MCP comes from their own config" >&2
  exit 1
fi

# Role grid (experimental, phase 1). Validate before scaffolding so a bad role
# can't leave a half-built grid: role panes are pi-only for now, and names must
# be safe to use as tmux option values and bus ids.
if [ -n "$grid_roles" ]; then
  if [ "$agent" != pi ]; then
    echo "dispatch: --roles currently requires --agent pi (role panes are pi-only in phase 1)" >&2
    exit 1
  fi
  IFS=',' read -r -a role_list <<<"$grid_roles"
  for role in "${role_list[@]}"; do
    case "$role" in
    '' | *[!A-Za-z0-9_-]*)
      echo "dispatch: invalid role '$role' (letters, digits, _ and - only)" >&2
      exit 1
      ;;
    esac
  done
fi

# Map an additive --mcp profile to its generated config (claude-only).
mcp_flag=""
if [ -n "$mcp_profile" ]; then
  case "$mcp_profile" in
  analytics) mcp_file="$HOME/.config/claude-code/mcp-posthog.json" ;;
  *)
    echo "dispatch: unknown --mcp profile '$mcp_profile' (valid: analytics)" >&2
    exit 1
    ;;
  esac
  [ -f "$mcp_file" ] || {
    echo "dispatch: --mcp $mcp_profile config not found at $mcp_file" >&2
    exit 1
  }
  mcp_flag="--mcp-config $mcp_file"
fi

title="$*"
[ -n "$title" ] || {
  usage
  exit 1
}

# Reclaim workers whose PR already landed, before adding another one. Cheapest
# possible cleanup schedule: no daemon, no timer, and it runs exactly when the
# worktree/window count is about to grow. Non-fatal by construction — a dispatch
# must never fail because cleanup of unrelated, already-merged work failed.
crew reap --quiet || true

# slug: lowercase, non-alnum -> single dash, first 40 chars, strip edge dashes.
slug=$(printf '%s' "$title" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g' | cut -c1-40 | sed -E 's/^-+//; s/-+$//')

# Identity + closes line. Linear mode derives both from the ticket (no gh); a
# passed GitHub issue number reuses that issue (no gh call). Otherwise GitHub
# mode mints an issue and aborts cleanly if that fails (issues disabled) rather
# than scaffolding a half-broken worker off an empty number.
if [ -n "$linear_id" ]; then
  branch="$(printf '%s' "$linear_id" | tr '[:upper:]' '[:lower:]')-$slug"
  closes="Closes $linear_id"
elif [ -n "$gh_issue" ]; then
  branch="feat/$gh_issue-$slug"
  closes="Closes #$gh_issue"
else
  url=$(gh issue create --assignee @me --title "$title" --body "Dispatched worker task." 2>/dev/null || true)
  num=$(printf '%s' "$url" | sed -nE 's#.*/([0-9]+)$#\1#p')
  [ -n "$num" ] || {
    echo "dispatch: could not create a GitHub issue (issues disabled?). Pass a Linear id, e.g. dispatch $tier $model ENG-1234 $title" >&2
    exit 1
  }
  branch="feat/$num-$slug"
  closes="Closes #$num"
fi

sanitized="${branch//\//-}"

# Blank worktrunk's post-switch *tmux* hook for this one call: we drive tmux
# ourselves below, and the hook would otherwise open a second, undecorated shell
# window at the same worktree (#123). Its own `$CLAUDECODE` guard only covers a
# Claude-launched dispatcher, and setting CLAUDECODE here would leak Claude's
# identity into a codex/cursor worker. Scoped to `tmux`, so the devshell hook
# still runs — it materializes .pre-commit-config.yaml, without which the worker
# cannot commit at all.
wt switch -c "$branch" -y --config-set 'post-switch.tmux=""'

# Ask git where worktrunk actually placed the worktree — its path template is
# user-configurable, so reconstructing it here drifts the moment that changes.
# awk must read to EOF: an early `exit` closes the pipe while git still has
# blocks to write, and the resulting SIGPIPE (141) trips pipefail + errexit,
# killing dispatch silently right after `wt switch` created the worktree.
wt_path="$(git worktree list --porcelain | awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')"
if [ -z "$wt_path" ]; then
  echo "dispatch: could not locate worktree for branch $branch" >&2
  exit 1
fi

crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
mkdir -p "$crew_dir"

# Log the dispatch decision to the crew bus for later `crew report`.
dispatch_shape="${DISPATCH_SHAPE:-}"
jq -nc --arg crew "$crew_id" --arg branch "$branch" \
  --arg engine "$agent" --arg model "$model" --arg tier "$tier" --arg effort "$effort" \
  --arg shape "$dispatch_shape" --arg title "$title" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"dispatch", branch:$branch, engine:$engine, model:$model, tier:$tier, effort:$effort, shape:$shape, title:$title}' \
  >>"$crew_dir/events.jsonl"

# FleetView-style codename+color, derived from the branch (deterministic).
ident=$(crew identity "$branch")
agent_name=$(printf '%s' "$ident" | jq -r .name)
agent_color=$(printf '%s' "$ident" | jq -r .tmux)

# Stamp the task file: header fields the worker protocol reads, the closes
# line, and the full task body from $DISPATCH_SPEC (falls back to the title).
{
  printf 'tier: %s\neffort: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\n' \
    "$tier" "$effort" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name"
  if [ -n "${DISPATCH_SPEC:-}" ] && [ -f "${DISPATCH_SPEC:-}" ]; then
    printf '\n## Task\n\n'
    cat "$DISPATCH_SPEC"
  fi
} >"$wt_path/WORKER_TASK.md"

read -r win pane < <(tmux new-window -d -c "$wt_path" -n "$sanitized" -P -F '#{window_id} #{pane_id}')

# Identity surfaces: codename on the pane border + the CC prompt box (--name).
# lazytmux owns the tab text; @crew_* tint the status-bar tab.
tmux set-window-option -t "$win" @crew_name "$agent_name"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold] "

# Deep claude workers get the read-only codex MCP for cross-model review
# (work profile only — mcp-codex.json is generated work-gated).
xreview_mcp=""
if [ "$profile" = work ] && [ "$agent" = claude ] && [ "$tier" = deep ]; then
  xreview_mcp="--mcp-config $HOME/.config/claude-code/mcp-codex.json"
fi

# When the dispatcher already wrote the plan into the task doc, say so in the
# launch prompt. A launch-prompt (user-turn) instruction is a "direct request",
# which satisfies using-superpowers' own escape hatch — so the worker skips the
# plan phase instead of re-deriving it.
plan_note=""
if [ "$plan_val" = provided ]; then
  plan_note=" The task doc is your plan of record — extract the steps and implement; do not re-plan or re-critique it."
fi

if [ "$agent" = codex ]; then
  # service_tier pinned: the interactive /fast toggle persists locally and would
  # otherwise leak into unattended workers, burning ChatGPT credits at 2.5x for
  # latency nobody is watching.
  tmux send-keys -t "$pane" \
    "codex --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default --dangerously-bypass-approvals-and-sandbox 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
elif [ "$agent" = cursor ]; then
  # cursor-agent has no reasoning-effort flag — effort is encoded in the model
  # id ($model, e.g. claude-opus-4-8-high); composer-2.5 has no effort variants.
  # A bare prompt argument (no -p) seeds and auto-submits cursor's own TUI;
  # --force/--trust/--approve-mcps make it unattended (codex bypass analog); base
  # MCP is the shared ~/.cursor/mcp.json. Headless -p is wrong for a worker: the
  # watchdog below reads pane output as liveness and -p prints nothing until the
  # task ends (#103/#111), while the TUI repaints as it works.
  #
  # Indexing OFF: parity with claude/codex (read + grep, no semantic index) and
  # it skips a merkle index build over a large monorepo. Not a stall fix — the
  # `cursor-retrieval` line these were meant to suppress comes from the in-process
  # file_service module, not the indexed-grep path.
  tmux send-keys -t "$pane" \
    "CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model $model 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
elif [ "$agent" = pi ]; then
  # Interactive TUI with an initial prompt: it auto-submits and repaints as it
  # works, so the pane stays a truthful liveness signal for stall-watch. `pi -p`
  # is buffered — it prints only at the end and would read as a wedge, the same
  # trap cursor hit (#103). --append-system-prompt takes text OR a file path (pi
  # reads the file when the argument exists), so the protocol is a real system
  # prompt here — unlike codex/cursor, which must inject it as a first prompt.
  # --no-approve ignores a target repo's project-local .pi/ resources in an
  # unattended run; global ~/.pi/agent config (auth, packages) still loads.
  # --thinking is a real knob (unlike cursor, where effort lives in the id).
  tmux send-keys -t "$pane" \
    "pi --name $agent_name --model $model --thinking $effort --append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.md --no-approve 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
else
  tmux send-keys -t "$pane" \
    "claude --name $agent_name --model $model --effort $effort $mcp_flag $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
fi

# Role grid (phase 1 mechanics): split the task window into one pane per role.
# Each role pane parks on the bus until the lead assigns it work; GRID_PROTOCOL.md
# is its system prompt. Split AFTER the lead launch so the lead keeps the first
# pane. NOT stall-watched on purpose: a parked role produces no output, which the
# pane-output watchdog would misread as a wedge.
if [ -n "$grid_roles" ]; then
  for role in "${role_list[@]}"; do
    read -r role_pane < <(tmux split-window -t "$win" -c "$wt_path" -P -F '#{pane_id}')
    tmux set-option -p -t "$role_pane" @crew_role "$role"
    tmux set-option -p -t "$role_pane" pane-border-format " #[bold]$role#[nobold] "
    tmux send-keys -t "$role_pane" \
      "pi --name ${agent_name}-${role} --model $model --thinking $effort --append-system-prompt $PROTOCOL_DIR/GRID_PROTOCOL.md --no-approve 'You are the $role role pane in this task grid. Read WORKER_TASK.md, resolve your role from @crew_role, then follow GRID_PROTOCOL.md: announce yourself and park for an assignment.'" Enter
  done
  tmux select-layout -t "$win" tiled
fi

# Detached stall watchdog (#103): a wedged worker sits in `working` with no
# output and never ends, so neither the bus nor the SessionEnd `exited` backstop
# notices. Pane output is only a valid liveness signal for an engine that streams
# — every engine launched above must, which is why all three run their own TUI
# rather than a buffered headless mode. This watches the pane's output and, if it
# goes silent through the startup window, posts `failed` so the dispatcher's
# `crew watch` wakes to recover. Engine-agnostic. nohup detaches it
# so it outlives this short-lived dispatch process; it self-exits on progress, a
# terminal state, or a vanished pane.
CREW_ID="$crew_id" nohup crew stall-watch "$branch" --pane "$pane" >/dev/null 2>&1 &
