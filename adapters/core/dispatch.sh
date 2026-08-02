# shellcheck shell=bash
# dispatch — scaffold a worker: issue/ticket -> worktree -> task file -> baked agent.
# Native PATH tool (was a fish autoload function). Crew id is delivered
# explicitly (--crew-id > $CREW_ID > error); a binary can't export env back to
# its caller, so the old `set -gx CREW_ID` persistence trick is gone.
# The shebang + `set -euo pipefail` are prepended by writeShellApplication, so
# this file is only the function body (see crew.sh for the same pattern).

usage() {
  echo "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh|max|ultra> [--agent claude|codex|cursor] [--mcp <profile>] [--plan provided|required] [--crew-id <id>] [LINEAR-ID|#N] <title...>" >&2
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
crew_id_flag=""
plan_val="required"
while [ $# -gt 0 ]; do
  case "$1" in
  --agent)
    agent="${2:-}"
    case "$agent" in
    claude | codex | cursor) ;;
    *)
      echo "dispatch: --agent must be claude, codex, or cursor" >&2
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

# Model gate. Reject a slug the chosen engine cannot run before anything is
# scaffolded — otherwise a wrong id surfaces as a 400 in a tmux pane the
# worktree, window and issue already paid for. Shape, not a model list: this
# file bakes into a store path, so a membership table would make every model
# bump a rebuild.
# Unanchored at the front on purpose: `gpt-5.5-extra-high` is a real cursor id
# and matches on its trailing `-high`.
re_effort_tail='-(none|low|medium|high|xhigh|max)(-fast)?$'
if [ "${DISPATCH_SKIP_MODEL_CHECK:-}" = "$model" ]; then
  echo "dispatch: model check skipped (DISPATCH_SKIP_MODEL_CHECK) — '$model' on --agent $agent is unverified" >&2
else
  case "$agent" in
  claude)
    re_claude_id='^claude-[a-z0-9]+(-[a-z0-9]+)*$'
    if [[ $model =~ $re_claude_id ]] && [[ $model =~ $re_effort_tail ]]; then
      echo "dispatch: model '$model' is an effort-suffixed cursor id — on --agent claude pass the bare id and set intensity with --effort. Did you mean --agent cursor? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    if [[ ! $model =~ ^(opus|sonnet|haiku|fable)$ ]] && [[ ! $model =~ $re_claude_id ]]; then
      echo "dispatch: model '$model' does not match --agent claude — claude takes an alias (opus, sonnet, haiku, fable) or a full claude-* id (e.g. claude-fable-5). Did you mean --agent cursor? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
  codex)
    if [[ ! $model =~ ^gpt-[0-9]+\.[0-9]+-[a-z0-9]+$ ]] && [[ ! $model =~ ^gpt-5\.[45]$ ]]; then
      if [[ $model =~ ^gpt-[0-9]+\.[0-9]+$ ]]; then
        gen="${model#gpt-}"
        echo "dispatch: model '$model' is not a codex slug — the $gen family ships only as variants (gpt-$gen-sol, gpt-$gen-terra, gpt-$gen-luna); there is no bare $model. See dispatch-orchestration.md \"Model gate\"." >&2
        exit 1
      fi
      echo "dispatch: model '$model' does not match --agent codex — codex takes gpt-* variant slugs (e.g. gpt-5.6-sol). Did you mean --agent claude? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    # The cache tightens the grammar and is never a prerequisite for it: probe
    # usability separately so the membership test's non-zero can only mean "not
    # on this account". Conflated, a rotated or half-written cache would block
    # every codex dispatch behind a file nobody edits by hand. The `?|strings`
    # projection is what makes that hold for a file that parses but whose
    # entries are not `{slug: string}` — a bare `.slug` there is a jq error, and
    # under `set -e` that kills dispatch even for a valid slug.
    codex_cache="$HOME/.codex/models_cache.json"
    if jq -e '[.models[]?|.slug?|strings]|length > 0' "$codex_cache" >/dev/null 2>&1 &&
      ! jq -e --arg m "$model" '[.models[]?|.slug?|strings]|index($m)' "$codex_cache" >/dev/null; then
      # Filtered to what the grammar accepts — the raw list advertises
      # codex-auto-review, an internal review model the gate rejects anyway.
      # Controls are stripped because this lands on a terminal, where an escape
      # sequence in a slug would be interpreted rather than shown.
      known="$(jq -r '[.models[]?|.slug?|strings|gsub("[[:cntrl:]]";"")|select(startswith("gpt-"))]|join(", ")' "$codex_cache")"
      echo "dispatch: model '$model' is not in this account's codex model list (~/.codex/models_cache.json: $known). If it is genuinely new, set DISPATCH_SKIP_MODEL_CHECK=$model and update the model map. See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
  cursor)
    # Cursor fronts other vendors, so membership is unknowable offline and only
    # id shape is checked. BASH_REMATCH is clobbered by the next [[ =~ ]], so
    # both groups are captured on the spot.
    re_cursor='^([a-z0-9][a-z0-9.-]*)(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$'
    cursor_base=""
    cursor_params=""
    if [[ $model =~ $re_cursor ]]; then
      cursor_base="${BASH_REMATCH[1]}"
      cursor_params="${BASH_REMATCH[2]}"
    fi
    if [ -z "$cursor_base" ] || [[ $cursor_base =~ ^(opus|sonnet|haiku|fable)$ ]]; then
      echo "dispatch: model '$model' does not match --agent cursor — cursor needs a full model id (e.g. kimi-k3-high, cursor-grok-4.5-medium-fast, composer-2.5, claude-opus-4-8-high). Did you mean --agent claude? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    # cursor has no --effort knob, so its claude-*/gpt-* ids carry the rung in
    # the id itself; a bracket block exempts only by naming effort= there.
    if [[ $cursor_base =~ ^(claude|gpt)- ]] && [[ ! $cursor_base =~ $re_effort_tail ]] && [[ ! $cursor_params =~ (\[|,)effort= ]]; then
      echo "dispatch: model '$model' is not a cursor id — cursor's claude-*/gpt-* ids carry an effort suffix (gpt-5.6-sol-high, gpt-5.6-sol-high-fast) because cursor has no --effort knob. Live list: cursor-agent --list-models. See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
  esac
fi
# claude's --effort tops out at max; rejecting `ultra` here fails before the
# worktree and pane exist, instead of at worker launch.
if [ "$agent" = claude ] && [ "$effort" = ultra ]; then
  echo "dispatch: --effort ultra is codex-only; claude tops out at max" >&2
  exit 1
fi
if [ "$agent" != claude ] && [ -n "$mcp_profile" ]; then
  echo "dispatch: --mcp is claude-only; codex/cursor base MCP comes from their own profile" >&2
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
  printf 'tier: %s\nengine: %s\nmodel: %s\neffort: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\n' \
    "$tier" "$agent" "$model" "$effort" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name"
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

# Execute subagents never read WORKER_PROTOCOL.md. Codex/cursor workers must
# stamp process-authority into every execute-subagent prompt so a fresh subagent
# cannot re-derive process via skills. Claude gets the same idea from rule 1 +
# the Agent tool; this clause is only for engines whose spawn prompt is the
# sole carrier.
process_authority=" Process authority: WORKER_PROTOCOL.md governs this worker session. When spawning execute subagents, grant implementation authority only — tell them not to re-derive worker process via skills, not to open PRs, and not to act as the worker."
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

if [ "$agent" = codex ]; then
  # service_tier pinned: the interactive /fast toggle persists locally and would
  # otherwise leak into unattended workers, burning ChatGPT credits at 2.5x for
  # latency nobody is watching.
  # agents.*: enable native delegation, cap concurrency at 3 (parity with rule 1),
  # and pin subagent effort one rung down. Never pass ultra as subagent effort.
  tmux send-keys -t "$pane" \
    "codex --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default -c agents.enabled=true -c agents.max_concurrent_threads_per_session=3 -c agents.default_subagent_reasoning_effort=$codex_subagent_effort --dangerously-bypass-approvals-and-sandbox 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.${plan_note}${process_authority}'" Enter
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
  # No CLI concurrency cap — rule 1's "capped at 3 concurrent" is protocol-only.
  tmux send-keys -t "$pane" \
    "CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end. Push when pre-push passes; open a PR.${plan_note}${process_authority}'" Enter
else
  tmux send-keys -t "$pane" \
    "claude --name $agent_name --model $model --effort $effort $mcp_flag $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and run it end-to-end. Push when pre-push passes; open a PR.${plan_note}'" Enter
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
