# shellcheck shell=bash
# dispatch — scaffold a worker: issue/ticket -> worktree -> task file -> baked agent.
# Native PATH tool (was a fish autoload function). Crew id is delivered
# explicitly (--crew-id > $CREW_ID > error); a binary can't export env back to
# its caller, so the old `set -gx CREW_ID` persistence trick is gone.
# The shebang + `set -euo pipefail` are prepended by writeShellApplication, so
# this file is only the function body (see crew.sh for the same pattern).

usage() {
  echo "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh|max|ultra> [--agent claude|codex|cursor|pi] [--mcp <profile>] [--grid] [--roles <r1[=model|agent:model],...>] [--lazy] [--status] [--plan provided|required] [--crew-id <id>] [LINEAR-ID|#N] <title...>" >&2
  echo "       dispatch --spawn-role <role> [--agent E] [--model M] [--effort E]   # create a lazy grid's role pane on demand" >&2
  echo "       dispatch --reap-roles                                            # kill this window's role panes" >&2
}

# Protocol directory. The env override is the dev loop: point it at a checkout
# and protocol edits take effect on the next dispatch with no rebuild. The
# default is substituted to a store path at build time.
PROTOCOL_DIR="${DISPATCHER_PROTOCOL_DIR:-@protocolDir@}"

# --- role-grid helpers -----------------------------------------------------

# role_color <role> — a stable tmux colour per role. Known roles get a semantic
# colour; anything else falls back to crew's deterministic FleetView palette, so
# a role is always the same colour run to run ("always the same per role").
role_color() {
  case "$1" in
  spec-critic) printf 'colour141' ;; # mauve
  plan-critic) printf 'colour111' ;; # blue
  reviewer) printf 'colour114' ;;    # green
  security) printf 'colour174' ;;    # red
  consult) printf 'colour180' ;;     # yellow
  *) crew identity "$1" 2>/dev/null | jq -r '.tmux // "colour250"' ;;
  esac
}

# decorate_pane <pane> <role> — put the role on the pane border and colour that
# border by role. tmux keeps these styles per pane, so a role's colour and label
# survive a tiled layout and a zoom (prefix+z). Also turns pane borders on for the
# window, so the labels are actually rendered.
decorate_pane() {
  local pane="$1" role="$2" color
  color="$(role_color "$role")"
  tmux set-option -p -t "$pane" @crew_role "$role"
  tmux set-option -p -t "$pane" @crew_role_color "$color"
  tmux set-option -p -t "$pane" pane-border-style "bg=#{@thm_bg},fg=$color"
  tmux set-option -p -t "$pane" pane-active-border-style "bg=#{@thm_bg},fg=$color,bold"
  tmux set-option -p -t "$pane" pane-border-format " #[bold]#{@crew_role}#[nobold] "
  tmux set-option -w -t "$pane" pane-border-status top
}

# split_role_pane <window> <worktree> <role> — create a role pane, decorate it,
# and echo its pane id.
split_role_pane() {
  local win="$1" wt="$2" role="$3" pane
  pane="$(tmux split-window -t "$win" -c "$wt" -P -F '#{pane_id}')"
  decorate_pane "$pane" "$role"
  printf '%s' "$pane"
}

# launch_role <pane> <role> <agent> <model> — launch the role's engine with
# GRID_PROTOCOL as its system prompt (appended where the engine supports it,
# first prompt otherwise). Reads $agent_name / $effort from the caller scope.
launch_role() {
  local pane="$1" role="$2" r_agent="$3" r_model="$4"
  local prompt="You are the $role role pane in this task grid. Read WORKER_TASK.md, resolve your role from @crew_role, then follow GRID_PROTOCOL.md: announce yourself and park for an assignment."
  local first="Read $PROTOCOL_DIR/GRID_PROTOCOL.md and WORKER_TASK.md, then follow GRID_PROTOCOL.md: announce yourself and park for an assignment (you are the $role role)."
  case "$r_agent" in
  pi) tmux send-keys -t "$pane" "pi --name ${agent_name}-${role} --model $r_model --thinking $effort --append-system-prompt $PROTOCOL_DIR/GRID_PROTOCOL.md --no-approve '$prompt'" Enter ;;
  claude) tmux send-keys -t "$pane" "claude --name ${agent_name}-${role} --model $r_model --effort $effort --append-system-prompt-file $PROTOCOL_DIR/GRID_PROTOCOL.md --permission-mode auto '$prompt'" Enter ;;
  codex) tmux send-keys -t "$pane" "codex --profile worker -m $r_model -c model_reasoning_effort=$effort -c service_tier=default --dangerously-bypass-approvals-and-sandbox '$first'" Enter ;;
  cursor) tmux send-keys -t "$pane" "CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$r_model' '$first'" Enter ;;
  esac
}

# `dispatch --spawn-role <role>` — create a role pane on demand in the caller's
# own tmux window/worktree, from the grid recorded in roles.json. Idempotent:
# reuses an existing pane for that role. Used by a lazy grid's lead at a seam.
if [ "${1:-}" = "--spawn-role" ]; then
  role="${2:-}"
  [ -n "$role" ] || {
    echo "dispatch: --spawn-role needs a role name" >&2
    exit 1
  }
  shift 2
  spawn_agent=""
  spawn_model=""
  spawn_effort=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --agent) spawn_agent="${2:-}"; shift 2 ;;
    --model) spawn_model="${2:-}"; shift 2 ;;
    --effort) spawn_effort="${2:-}"; shift 2 ;;
    *)
      echo "dispatch: --spawn-role: unexpected argument '$1'" >&2
      exit 1
      ;;
    esac
  done
  [ -f WORKER_TASK.md ] || {
    echo "dispatch: --spawn-role must run inside a worker worktree (no WORKER_TASK.md)" >&2
    exit 1
  }
  [ -n "${TMUX_PANE:-}" ] || {
    echo "dispatch: --spawn-role must run inside tmux" >&2
    exit 1
  }
  crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  branch="$(git branch --show-current)"
  roles_file="$crew_dir/artifacts/$branch/roles.json"
  [ -f "$roles_file" ] || {
    echo "dispatch: no role grid recorded for $branch (dispatch without --lazy to use an up-front grid)" >&2
    exit 1
  }
  spec="$(jq -r --arg r "$role" '.[$r] // empty | "\(.agent) \(.model)"' "$roles_file")"
  [ -n "$spec" ] || {
    echo "dispatch: role '$role' is not part of this grid" >&2
    exit 1
  }
  agent_name="$(sed -n 's/^agent_name: //p' WORKER_TASK.md)"
  effort="${spawn_effort:-$(sed -n 's/^effort: //p' WORKER_TASK.md)}"
  spawn_agent="${spawn_agent:-${spec%% *}}"
  spawn_model="${spawn_model:-${spec#* }}"
  win="$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}')"
  existing="$(tmux list-panes -t "$win" -F '#{pane_id} #{@crew_role}' | awk -v r="$role" '$2 == r {print $1; exit}')"
  if [ -n "$existing" ]; then
    echo "role $role is already running in pane $existing"
    exit 0
  fi
  pane="$(split_role_pane "$win" "$PWD" "$role")"
  launch_role "$pane" "$role" "$spawn_agent" "$spawn_model"
  echo "spawned role $role ($spawn_agent/$spawn_model) in $pane"
  exit 0
fi

# `dispatch --reap-roles` — kill every role pane in the caller's window, so a
# finished grid reclaims its space without waiting for the whole window's reap.
if [ "${1:-}" = "--reap-roles" ]; then
  [ -n "${TMUX_PANE:-}" ] || {
    echo "dispatch: --reap-roles must run inside tmux" >&2
    exit 1
  }
  win="$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}')"
  tmux list-panes -t "$win" -F '#{pane_id} #{@crew_role}' | while read -r p r; do
    [ -n "$r" ] || continue
    [ "$p" = "$TMUX_PANE" ] && continue
    tmux kill-pane -t "$p" 2>/dev/null || true
  done
  echo "reaped role panes"
  exit 0
fi

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
grid_flag=""
grid_lazy=""
grid_status=""
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
  --grid)
    grid_flag=1
    shift
    ;;
  --lazy)
    grid_lazy=1
    shift
    ;;
  --status)
    grid_status=1
    shift
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

# Role grid. Resolve the topology before scaffolding so a bad spec can't leave a
# half-built grid. `--roles` is explicit and wins; `--grid` derives the topology
# from the tier. Each spec is `name`, `name=<model>`, or `name=<agent>:<model>`;
# a leading token from the fixed agent set is the agent, so any other text before
# a `:` (a pi `:thinking` suffix, say) stays part of the model id.
role_names=()
role_agents=()
role_models=()
if [ -z "$grid_roles" ] && [ -n "$grid_flag" ]; then
  case "$tier" in
  trivial) grid_roles="" ;;
  standard) grid_roles="plan-critic,reviewer" ;;
  deep) grid_roles="spec-critic,plan-critic,reviewer" ;;
  esac
fi
if [ -n "$grid_roles" ]; then
  IFS=',' read -r -a role_specs <<<"$grid_roles"
  for spec in "${role_specs[@]}"; do
    [ -n "$spec" ] || continue
    role="${spec%%=*}"
    rest=""
    [ "$role" != "$spec" ] && rest="${spec#*=}"
    case "$role" in
    '' | *[!A-Za-z0-9_-]*)
      echo "dispatch: invalid role '$role' (letters, digits, _ and - only)" >&2
      exit 1
      ;;
    esac
    role_agent="$agent"
    role_model="$model"
    if [ -n "$rest" ]; then
      case "${rest%%:*}" in
      claude | codex | cursor | pi)
        role_agent="${rest%%:*}"
        role_model="${rest#*:}"
        [ "$role_model" = "$rest" ] && role_model="$model"
        ;;
      *) role_model="$rest" ;;
      esac
    fi
    case "$role_agent" in
    codex | cursor)
      [ "$profile" = work ] || {
        echo "dispatch: role '$role' uses --agent $role_agent, which is work-profile only" >&2
        exit 1
      }
      ;;
    esac
    role_names+=("$role")
    role_agents+=("$role_agent")
    role_models+=("$role_model")
  done
fi
roles_stamp=""
if [ "${#role_names[@]}" -gt 0 ]; then
  roles_stamp="$(IFS=,; printf '%s' "${role_names[*]}")"
fi
if [ -n "$grid_lazy" ] && [ -z "$roles_stamp" ]; then
  echo "dispatch: --lazy needs --grid or --roles" >&2
  exit 1
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

# Record the resolved role specs so a lazy grid's lead can spawn each role on
# demand (`dispatch --spawn-role`), and so any role can be re-created after death.
if [ "${#role_names[@]}" -gt 0 ]; then
  roles_dir="$crew_dir/artifacts/$branch"
  mkdir -p "$roles_dir"
  for i in "${!role_names[@]}"; do
    jq -n --arg n "${role_names[$i]}" --arg a "${role_agents[$i]}" --arg m "${role_models[$i]}" '{name:$n,agent:$a,model:$m}'
  done | jq -s 'map({key:.name,value:{agent:.agent,model:.model}})|from_entries' > "$roles_dir/roles.json"
fi

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
  # The role grid the lead should delegate to (absent = single-agent pipeline).
  [ -n "$roles_stamp" ] && printf 'roles: %s\n' "$roles_stamp"
  # A lazy grid creates no role panes up front; the lead spawns each at its seam.
  [ -n "$grid_lazy" ] && printf 'lazy: 1\n'
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

# Grid mode: tell the lead it has role panes to delegate the critic/review phases
# to, over the bus, instead of running them in-process (WORKER_PROTOCOL.md →
# "Grid mode").
grid_note=""
if [ -n "$roles_stamp" ]; then
  grid_note=" You lead a role grid: role panes ($roles_stamp) share this worktree and are parked on the crew bus. Follow WORKER_PROTOCOL.md 'Grid mode' — delegate the critic/review phases to them over the bus instead of running them in-process."
  [ -n "$grid_lazy" ] && grid_note="$grid_note The grid is lazy: before assigning a role, create its pane with \`dispatch --spawn-role <role>\` (idempotent); reap them at the end with \`dispatch --reap-roles\`."
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
    "pi --name $agent_name --model $model --thinking $effort --append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.md --no-approve 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.${plan_note}${grid_note}'" Enter
else
  tmux send-keys -t "$pane" \
    "claude --name $agent_name --model $model --effort $effort $mcp_flag $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
fi

# Role grid: materialize the panes. A lazy grid creates none up front (the lead
# spawns each role at its seam with `dispatch --spawn-role`); otherwise split the
# window into one pane per role. Roles park on the bus until the lead assigns them
# work; GRID_PROTOCOL.md is their system prompt. A role may run a different engine
# from the lead (cross-engine review). Split AFTER the lead launch so the lead
# keeps the first pane. NOT stall-watched on purpose: a parked role produces no
# output, which the pane-output watchdog would misread as a wedge.
if [ "${#role_names[@]}" -gt 0 ] && [ -z "$grid_lazy" ]; then
  for i in "${!role_names[@]}"; do
    role_pane="$(split_role_pane "$win" "$wt_path" "${role_names[$i]}")"
    launch_role "$role_pane" "${role_names[$i]}" "${role_agents[$i]}" "${role_models[$i]}"
  done
  tmux select-layout -t "$win" tiled
fi

# Optional live status pane: a bounded roster loop over the crew bus.
if [ -n "$grid_status" ] && [ "${#role_names[@]}" -gt 0 ]; then
  status_pane="$(tmux split-window -t "$win" -c "$wt_path" -P -F '#{pane_id}')"
  decorate_pane "$status_pane" status
  tmux send-keys -t "$status_pane" "while true; do clear; crew roster 2>/dev/null | jq -r '.[] | \"  \\(.state)  \\(.from)\"'; sleep 3; done" Enter
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
