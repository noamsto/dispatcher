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
      echo "dispatcher: --agent needs a value (claude, codex, cursor, or pi)" >&2
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
claude | codex | cursor | pi) ;;
*)
  echo "dispatcher: --agent must be claude, codex, cursor, or pi" >&2
  exit 1
  ;;
esac

profile="${DISPATCH_PROFILE:-personal}"
case "$agent" in
codex | cursor)
  if [ "$profile" != work ]; then
    echo "dispatcher: --agent $agent is work-profile only" >&2
    exit 1
  fi
  ;;
esac

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
  # render until you next switch to this window. The reflow script is never on
  # PATH — lazytmux's tmux config calls it by nix-store path — so take the path
  # from the @reflow_bin option it stamps.
  reflow_bin=$(tmux show-option -gqv @reflow_bin)
  if [ -n "$reflow_bin" ]; then
    "$reflow_bin" \
      "$(tmux display-message -p -t "$TMUX_PANE" '#{session_name}')" \
      "$(tmux display-message -p -t "$TMUX_PANE" '#{window_width}')"
  fi
fi

session_name=dispatcher
[ -n "$task" ] && session_name="dispatcher: $task"

# Mint + export the crew id once at launch (mirrors `dispatch`), so the launched
# agent and every child `dispatch` inherit the SAME crew. `crew new` needs no
# git repo. Printed because a script cannot export back to the calling shell.
crew_id_was_unset=1
[ -z "${CREW_ID:-}" ] || crew_id_was_unset=0
: "${CREW_ID:=$(crew new)}"
export CREW_ID
echo "crew id: $CREW_ID"

if git rev-parse --git-common-dir >/dev/null 2>&1; then
  crew register $$

  # Discovery notice (#29): a mint that silently orphans other on-disk crews is
  # the exact split-brain this issue exists to close. `crew adopt` is the wrong
  # remedy on this entrance — CREW_ID is already exported into the launched
  # agent's env, so adopt's export line would land in the wrong shell; only
  # relaunching with it set re-attaches.
  if [ "$crew_id_was_unset" = 1 ]; then
    {
      other=0
      header=1
      while IFS=$'\t' read -r row_id _; do
        if [ "$header" = 1 ]; then
          header=0
          continue
        fi
        [ -n "$row_id" ] || continue
        [ "$row_id" = "$CREW_ID" ] && continue
        other=$((other + 1))
      done < <(crew crews)
      if [ "$other" -gt 0 ]; then
        echo "dispatcher: minted a NEW crew; this repo has $other other(s) — 'crew crews' lists them; to re-attach instead, relaunch as: CREW_ID=<id> dispatcher …" >&2
      fi
    } || true
  fi
fi

case "$agent" in
claude)
  # Pinned, not inherited: /model and /effort persist across sessions, so an
  # unpinned dispatcher judges tier+engine+model on whatever the last cheap
  # session was toggled to. high, not xhigh — same reason codex holds at high
  # below: blocked workers wait on a bounded ~300s in-band window.
  set -- --name "$session_name" --append-system-prompt-file "$protocol" \
    --model "${model:-opus}" --effort "${effort:-high}"
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
      -c 'service_tier="default"' \
      --dangerously-bypass-approvals-and-sandbox "$prompt"
  else
    # kimi-k3-high: third-family model, strong agentic tool use. --effort is
    # accepted-and-ignored (cursor encodes effort in the model id).
    [ -n "$effort" ] && echo "dispatcher: --effort is ignored for cursor (effort lives in the model id)" >&2
    cursor-agent --model "${model:-kimi-k3-high}" \
      --force --trust --approve-mcps --disable-indexing --disable-codebase-ref "$prompt"
  fi
  ;;
pi)
  # pi has a real --append-system-prompt (text or file contents), so the
  # protocol is baked as a system prompt rather than injected as a first user
  # prompt like codex/cursor — sturdier across compaction. --thinking is a real
  # effort knob. --no-approve ignores a target project's local resources; the
  # orchestrator runs in the dispatcher repo, and global ~/.pi/agent config
  # (auth, packages) still loads.
  # Profile-keyed default: OpenRouter on work (Noam's only configured provider
  # there), opencode Zen on personal. A personal host with OpenRouter would
  # resolve the work default fine; the split is what the default should be
  # when the operator did not pass --model. See dispatch-orchestration.md →
  # "Orchestrator engines".
  pi_default='openrouter/deepseek/deepseek-v4-pro'
  [ "$profile" = personal ] && pi_default='opencode/deepseek-v4-pro'
  set -- --name "$session_name" \
    --model "${model:-$pi_default}" \
    --thinking "${effort:-high}" \
    --no-approve \
    --append-system-prompt "$protocol"
  [ -n "$task" ] && set -- "$@" "$task"
  pi "$@"
  ;;
esac

# The launches above are children, not exec — deregister so the bus doesn't
# accumulate stale entries.
if git rev-parse --git-common-dir >/dev/null 2>&1; then
  crew deregister
fi
