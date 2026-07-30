# shellcheck shell=bash
# dispatcher — start an orchestrator session with DISPATCHER_PROTOCOL baked in.
# Ported from a fish autoload function; `dispatch` made the same move earlier.
# One behaviour change is unavoidable: a script cannot export CREW_ID back into
# the caller's interactive shell the way `set -gx` did, so the id is printed.

PROTOCOL_DIR="${DISPATCHER_PROTOCOL_DIR:-@protocolDir@}"
protocol="$PROTOCOL_DIR/DISPATCHER_PROTOCOL.md"

agent=claude
model=""
effort=""
task=""

while [ $# -gt 0 ]; do
  case "$1" in
  # Each flag guards its value explicitly. Without the guard a trailing
  # `--agent` makes `shift 2` fail, and under `set -e` the script dies
  # SILENTLY — worse than the fish original, where an out-of-range index
  # yielded empty and fell through to the validation message. Same shape
  # dispatch.sh uses for --mcp/--crew-id.
  --agent)
    agent="${2:-}"
    [ -n "$agent" ] || {
      echo "dispatcher: --agent needs a value (claude, codex, or cursor)" >&2
      exit 1
    }
    shift 2
    ;;
  --model)
    model="${2:-}"
    [ -n "$model" ] || {
      echo "dispatcher: --model needs a value" >&2
      exit 1
    }
    shift 2
    ;;
  --effort)
    effort="${2:-}"
    [ -n "$effort" ] || {
      echo "dispatcher: --effort needs a value (low, medium, high, xhigh, max, or ultra)" >&2
      exit 1
    }
    shift 2
    ;;
  *)
    task="${task:+$task }$1"
    shift
    ;;
  esac
done

case "$agent" in
claude | codex | cursor) ;;
*)
  echo "dispatcher: --agent must be claude, codex, or cursor" >&2
  exit 1
  ;;
esac

profile="${DISPATCH_PROFILE:-personal}"
if [ "$agent" != claude ] && [ "$profile" != work ]; then
  echo "dispatcher: --agent $agent is work-profile only" >&2
  exit 1
fi

if [ -n "$effort" ]; then
  case "$effort" in
  low | medium | high | xhigh | max | ultra) ;;
  *)
    echo "dispatcher: --effort must be low, medium, high, xhigh, max, or ultra" >&2
    exit 1
    ;;
  esac
fi

# Fixed identity for the orchestrator window so it stands out from the
# per-branch worker windows it spawns. Guarded on $TMUX — the dispatcher can be
# launched outside a tmux pane.
if [ -n "${TMUX:-}" ]; then
  tmux set-window-option pane-border-style "bg=#{@thm_bg},fg=#{@thm_mauve}"
  tmux set-window-option pane-active-border-style "bg=#{@thm_bg},fg=#{@thm_mauve},bold"
  tmux set-window-option pane-border-format " #[bold]dispatcher#[nobold] "
  # Same @crew_* options `dispatch` stamps on workers, so lazytmux renders the
  # orchestrator badge too. colour99 clears contrast on both Catppuccin themes.
  tmux set-window-option @crew_name dispatcher
  tmux set-window-option @crew_color colour99
  # No tmux event fires on a user-option set, and lazytmux's per-tick poll only
  # runs for the session a client is viewing — kick a reflow or the badge won't
  # render until you next switch to this window.
  tmux-reflow-windows \
    "$(tmux display-message -p -t "$TMUX_PANE" '#{session_name}')" \
    "$(tmux display-message -p -t "$TMUX_PANE" '#{window_width}')"
fi

session_name=dispatcher
[ -n "$task" ] && session_name="dispatcher: $task"

# Mint + export the crew id once at launch (mirrors `dispatch`), so the launched
# agent and every child `dispatch` inherit the SAME crew. `crew id` needs no git
# repo. Printed because a script cannot export back to the calling shell.
: "${CREW_ID:=$(crew id)}"
export CREW_ID
echo "crew id: $CREW_ID"

if git rev-parse --git-common-dir >/dev/null 2>&1; then
  crew register $$
fi

case "$agent" in
claude)
  set -- --name "$session_name" --append-system-prompt-file "$protocol"
  [ -n "$model" ] && set -- "$@" --model "$model"
  [ -n "$effort" ] && set -- "$@" --effort "$effort"
  claude "$@" ${task:+"$task"}
  ;;
codex | cursor)
  # Neither CLI has --append-system-prompt-file — inject the protocol as the
  # first prompt (the same pattern dispatch.sh uses for codex/cursor workers).
  prompt="Read $protocol and adopt the dispatcher role for the rest of this session: judge each task into tier + engine + model + effort per the rubric, scaffold one worker per task via dispatch, and run the crew-watch loop per YOUR engine's section of the protocol (you are a $agent dispatcher). CREW_ID is already exported in this environment, so dispatch and crew calls inherit it."
  [ -n "$task" ] && prompt="$prompt First task: $task"
  if [ "$agent" = codex ]; then
    # Effort high, not xhigh: blocked workers wait on a bounded ~300s in-band
    # window; xhigh turns would let blocks go stale. service_tier pinned — the
    # interactive /fast toggle persists locally and would otherwise leak into
    # the unattended dispatcher at 2.5x cost.
    codex --profile worker \
      -m "${model:-gpt-5.6-sol}" \
      -c "model_reasoning_effort=\"${effort:-high}\"" \
      -c "service_tier=\"default\"" \
      --dangerously-bypass-approvals-and-sandbox "$prompt"
  else
    # kimi-k3-high: third-family model, strong agentic tool use. --effort is
    # accepted-and-ignored (cursor encodes effort in the model id).
    [ -n "$effort" ] && echo "dispatcher: --effort is ignored for cursor (effort lives in the model id)" >&2
    cursor-agent --model "${model:-kimi-k3-high}" \
      --force --trust --approve-mcps --disable-indexing --disable-codebase-ref "$prompt"
  fi
  ;;
esac

# The launches above are children, not exec — deregister so the bus doesn't
# accumulate stale entries.
if git rev-parse --git-common-dir >/dev/null 2>&1; then
  crew deregister
fi
