# shellcheck shell=bash
# dispatch — scaffold a worker: issue/ticket -> worktree -> task file -> baked agent.
# Native PATH tool (was a fish autoload function). Crew id is delivered
# explicitly (--crew-id > $CREW_ID > error); a binary can't export env back to
# its caller, so the old `set -gx CREW_ID` persistence trick is gone.
# The shebang + `set -euo pipefail` are prepended by writeShellApplication, so
# this file is only the function body (see crew.sh for the same pattern).

usage() {
  echo "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh|max|ultra> [--agent claude|codex|cursor] [--mcp <profile>] [--plan provided|required] [--crew-id <id>] [--pr N] [--review] [--draft|--no-draft] [--ignore-budget] [LINEAR-ID|#N] <title...>" >&2
}

# Ensure the `dispatched` claim-marker label exists. A no-op if it already
# does — must never abort a dispatch on that account.
_ensure_dispatched_label() {
  gh label create dispatched --color 1D76DB \
    --description "Claimed by a dispatcher crew; a worker is on it" >/dev/null 2>&1 || true
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
pr_number=""
base_ref=""
kind=implement
mcp_profile=""
crew_id_flag=""
plan_val="required"
ignore_budget=""
draft=false
if [ "${DISPATCH_DRAFT_PR:-}" = 1 ]; then
  draft=true
fi
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
  --pr)
    pr_number="${2:-}"
    [ -n "$pr_number" ] || {
      echo "dispatch: --pr needs a PR number" >&2
      exit 1
    }
    shift 2
    ;;
  --review)
    kind=review
    shift
    ;;
  --draft)
    draft=true
    shift
    ;;
  --no-draft)
    draft=false
    shift
    ;;
  --ignore-budget)
    ignore_budget=1
    shift
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

if [ "$kind" = review ] && [ "$draft" = true ]; then
  echo "dispatch: --draft cannot be combined with --review" >&2
  exit 1
fi

[ -n "$effort" ] || {
  echo "dispatch: --effort is required and must be judged independently from tier" >&2
  exit 1
}

if [ -n "$pr_number" ]; then
  if ! printf '%s' "$pr_number" | grep -Eq '^[0-9]+$'; then
    echo "dispatch: --pr needs a PR number" >&2
    exit 1
  fi
  if [ -n "$linear_id" ] || [ -n "$gh_issue" ]; then
    echo "dispatch: --pr cannot combine with a Linear id or GitHub issue token" >&2
    exit 1
  fi
fi

# Reject before scaffolding: without a PR there is no head to attach to, and a
# review worker on a freshly minted feature branch has nothing to review. The
# contract file is checked here for the same reason — $DISPATCHER_PROTOCOL_DIR
# can point at a checkout predating it, and a review worker launched without the
# contract runs the implement pipeline against someone else's PR head.
review_contract="$PROTOCOL_DIR/REVIEW_TASK.md"
if [ "$kind" = review ]; then
  [ -n "$pr_number" ] || {
    echo "dispatch: --review requires --pr N" >&2
    exit 1
  }
  [ -f "$review_contract" ] || {
    echo "dispatch: --review found no review contract at $review_contract" >&2
    exit 1
  }
fi

# Crew id: explicit flag > inherited env > error. Launcher dispatchers inherit
# $CREW_ID from the claude process env; in-session dispatchers pass --crew-id.
crew_id="${crew_id_flag:-${CREW_ID:-}}"
[ -n "$crew_id" ] || {
  # shellcheck disable=SC2016  # $PPID is documentation text, not an expansion
  echo 'dispatch: no crew id — run '\''crew crews'\'' to find this repo'\''s crews and '\''crew adopt <id> $PPID'\'' to re-attach, or '\''crew new'\'' to start one; then pass --crew-id <id> or export CREW_ID' >&2
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

# Budget gate: refuse to add load to an engine whose quota is ~exhausted. The
# cache is advisory data from refresh-budget — fail open when it is missing,
# stale (>2h), or silent on this engine ("unknown" is never "exhausted").
# --ignore-budget is the manual escape hatch (e.g. credits cover the overage).
budget_file="${XDG_DATA_HOME:-$HOME/.local/share}/crew/engine-budget.json"
if [ -z "$ignore_budget" ] && [ -f "$budget_file" ]; then
  exhausted=$(jq -r --arg e "$agent" --argjson now "$(date +%s)" '
    if (.fetched_epoch + 7200) < $now then empty
    elif .engines[$e] == null then empty
    else .engines[$e].windows | to_entries[]
      | select(.value.used_pct >= 95)
      | "\(.key) at \(.value.used_pct)%\(if .value.resets_at then ", resets \(.value.resets_at | todateiso8601)" else "" end)"
    end' "$budget_file" 2>/dev/null || true)
  if [ -n "$exhausted" ]; then
    echo "dispatch: $agent quota exhausted ($(printf '%s' "$exhausted" | head -1)) — pick another engine, wait for the reset, or pass --ignore-budget" >&2
    exit 1
  fi
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

# slug: lowercase, non-alnum -> single dash, first 40 chars, strip edge dashes.
slug=$(printf '%s' "$title" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g' | cut -c1-40 | sed -E 's/^-+//; s/-+$//')

crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
mkdir -p "$crew_dir"

# Hoisted above the claim gate (#73): the gate keys its resume exemption on the
# resolved branch and records the claim to the bus under $crew_dir. All three are
# pure — string work, one `git rev-parse`, one `mkdir -p` — so the gate keeps its
# stated property of running before ANY scaffolding.
if [ -n "$gh_issue" ]; then
  branch="feat/$gh_issue-$slug"
fi

# Claim: GitHub issue only. $gh_issue is empty for both a Linear dispatch
# (own status/assignee semantics — every issue here already has an assignee,
# so that can't double as a claim signal) and a --pr review dispatch
# (attaches to a PR, not an issue) — reusing the tracker detection below
# rather than a second one. Read-then-claim runs before ANY scaffolding,
# reap's sweep included, so a same-issue dispatcher racing at human timescale
# loses on the label read, not after building a worktree. gh has no
# compare-and-swap, so this narrows that race rather than closing it.
if [ -n "$gh_issue" ]; then
  _ensure_dispatched_label
  issue_labels="$(gh issue view "$gh_issue" --json labels --jq '.labels[].name')" || {
    echo "dispatch: could not read labels for issue #$gh_issue" >&2
    exit 1
  }
  # Resume exemption (#73): a claimed issue still dispatches when the branch it
  # resolves to already exists — that is the interrupted run being continued, not
  # a second crew forking. Keyed on the exact branch, so a reworded dispatch
  # resolves to a name that does not exist and is still refused. Who is live on
  # that branch stays the occupancy gate's call, as for every other dispatch.
  if printf '%s\n' "$issue_labels" | grep -qx dispatched; then
    git show-ref --verify --quiet "refs/heads/$branch" || {
      echo "dispatch: issue #$gh_issue is already claimed (carries the 'dispatched' label) — another crew is on it. If that crew is gone, remove the label by hand and retry." >&2
      exit 1
    }
    echo "dispatch: issue #$gh_issue is already claimed, but branch $branch exists — proceeding onto it as a resume." >&2
  fi
  # The exemption skips the refusal only. --add-label is idempotent and runs on
  # both paths, which is what makes a reap-driven resume->create downgrade below
  # harmless: whichever mode this run ends in, the issue is labelled.
  gh issue edit "$gh_issue" --add-label dispatched || {
    echo "dispatch: could not claim issue #$gh_issue (adding the 'dispatched' label failed)" >&2
    exit 1
  }
  # An explicit claim record, because `crew adopt` cannot infer one: the
  # kind:"dispatch" row carries no issue number and is written ~300 lines later,
  # so every failure in between would strand an unreleasable label (#73).
  jq -nc --arg crew "$crew_id" --arg issue "$gh_issue" --arg branch "$branch" \
    '{ts:(now*1000|floor), crew_id:$crew, kind:"claim-issue", issue:$issue, branch:$branch}' \
    >>"$crew_dir/events.jsonl"
fi

# Reclaim workers whose PR already landed, before adding another one. Cheapest
# possible cleanup schedule: no daemon, no timer, and it runs exactly when the
# worktree/window count is about to grow. Non-fatal by construction — a dispatch
# must never fail because cleanup of unrelated, already-merged work failed.
# Any worker still booting on a branch this reap could otherwise mistake for
# idle-done is protected by the claim write near `worker_id=` below.
crew reap --quiet || true

# Blank worktrunk's post-switch *tmux* hook for this one call: we drive tmux
# ourselves below, and the hook would otherwise open a second, undecorated shell
# window at the same worktree (#123). Its own `$CLAUDECODE` guard only covers a
# Claude-launched dispatcher, and setting CLAUDECODE here would leak Claude's
# identity into a codex/cursor worker. Scoped to `tmux`, so the devshell hook
# still runs — it materializes .pre-commit-config.yaml, without which the worker
# cannot commit at all.
wt_post_switch='post-switch.tmux=""'

# --pr resolves the head ref; the switch itself happens after the gate below, so
# a refusal costs no worktree and no window.
if [ -n "$pr_number" ]; then
  pr_json=$(gh pr view "$pr_number" --json headRefName,headRefOid,baseRefName,isCrossRepository)
  head=$(printf '%s' "$pr_json" | jq -r .headRefName)
  head_oid=$(printf '%s' "$pr_json" | jq -r .headRefOid)
  base_ref=$(printf '%s' "$pr_json" | jq -r .baseRefName)
  cross=$(printf '%s' "$pr_json" | jq -r .isCrossRepository)
  [ -n "$head" ] && [ "$head" != null ] || {
    echo "dispatch: could not resolve headRefName for PR $pr_number" >&2
    exit 1
  }
  [ -n "$head_oid" ] && [ "$head_oid" != null ] && [ -n "$base_ref" ] && [ "$base_ref" != null ] || {
    echo "dispatch: could not resolve headRefOid/baseRefName for PR $pr_number" >&2
    exit 1
  }
  branch="$head"
  closes="pr: $pr_number"
  if git show-ref --verify --quiet "refs/heads/$head" ||
    git show-ref --verify --quiet "refs/remotes/origin/$head"; then
    switch_mode=name
  elif [ "$cross" = false ]; then
    switch_mode=fetch-name
  else
    switch_mode=pr-ref
  fi
else
  # Identity + closes line. Linear mode derives both from the ticket (no gh); a
  # passed GitHub issue number reuses that issue (no gh call). Otherwise GitHub
  # mode mints an issue and aborts cleanly if that fails (issues disabled) rather
  # than scaffolding a half-broken worker off an empty number.
  if [ -n "$linear_id" ]; then
    branch="$(printf '%s' "$linear_id" | tr '[:upper:]' '[:lower:]')-$slug"
    closes="Closes $linear_id"
  elif [ -n "$gh_issue" ]; then
    # $branch was already computed by the hoist above the claim gate.
    closes="Closes #$gh_issue"
  else
    url=$(gh issue create --assignee @me --title "$title" --body "Dispatched worker task." 2>/dev/null || true)
    num=$(printf '%s' "$url" | sed -nE 's#.*/([0-9]+)$#\1#p')
    [ -n "$num" ] || {
      echo "dispatch: could not create a GitHub issue (issues disabled?). Pass a Linear id, e.g. dispatch $tier $model ENG-1234 $title" >&2
      exit 1
    }
    # A minted issue is claimed by definition — stamp it right away. $branch is
    # assigned first so the claim record below can carry it (#73).
    branch="feat/$num-$slug"
    closes="Closes #$num"
    _ensure_dispatched_label
    gh issue edit "$num" --add-label dispatched || {
      echo "dispatch: created issue #$num but could not claim it (adding the 'dispatched' label failed)" >&2
      exit 1
    }
    jq -nc --arg crew "$crew_id" --arg issue "$num" --arg branch "$branch" \
      '{ts:(now*1000|floor), crew_id:$crew, kind:"claim-issue", issue:$issue, branch:$branch}' \
      >>"$crew_dir/events.jsonl"
  fi
  # Resume on ref existence alone (#73), which is exactly what `wt switch -c`
  # refuses on: a branch whose worktree was pruned or `wt remove`d never fires the
  # reclaim below, and -c died on it all the same. Resolved here rather than
  # hoisted with $branch, because `crew reap` above calls `wt remove` and can
  # delete a merged branch — a mode computed before it could already be stale.
  if git show-ref --verify --quiet "refs/heads/$branch"; then
    switch_mode=resume
  else
    switch_mode=create

    # New branch: base it on the remote default branch's fetched tip, not the
    # local ref of that name, which nothing here fast-forwards and can be
    # stale (#41). The name comes from gh rather than refs/remotes/origin/HEAD,
    # which is only as fresh as the last `git remote set-head`. Resolved before
    # the dispatch lock below, so a failure here costs no worktree and no
    # window — same as the --pr gate above.
    default_branch=$(gh repo view --json defaultBranchRef --jq .defaultBranchRef.name)
    [ -n "$default_branch" ] && [ "$default_branch" != null ] || {
      echo "dispatch: could not resolve the default branch via gh repo view" >&2
      exit 1
    }
    git fetch origin -- "$default_branch"
    # Pinned now, not re-resolved at switch time below: the occupancy/reclaim
    # gate in between shells out to crew/jq, giving a concurrent fetch a window
    # to move the floating ref — pinning keeps what's branched and what the
    # success line reports from ever diverging.
    default_base_oid="$(git rev-parse "origin/$default_branch")"
    default_base_label="origin/$default_branch"
    default_base_short="$(git rev-parse --short "$default_base_oid")"
  fi
fi

# Serialize the gate's check-then-act (occupancy read -> switch -> open window)
# across concurrent dispatches on ONE branch; ungated, two racers both see an
# empty worktree and both open a window — the stacking #17 forbids. `ln -s` is an
# atomic exclusive create that publishes the owner pid (the link target) in the
# same syscall, so it is the ONLY creator of the lock and exactly one racer wins.
# A stale (dead-owner) lock is NOT auto-reclaimed: portable shell has no
# compare-and-delete, so a remove-and-retake path races a fresh acquirer and lets
# two dispatches proceed — the very stacking this prevents. It refuses instead,
# which the EXIT/signal trap makes rare: every exit short of SIGKILL clears it.
# cksum keys the file so a branch name with a `/` can't fold onto another's (#24).
dispatch_lock="$crew_dir/dispatch-$(printf '%s' "$branch" | cksum | cut -d' ' -f1).lock"
if ! ln -s "$$" "$dispatch_lock" 2>/dev/null; then
  held=$(readlink "$dispatch_lock" 2>/dev/null || true)
  if [ -n "$held" ] && kill -0 "$held" 2>/dev/null; then
    echo "dispatch: another dispatch is already scaffolding $branch (pid $held) — wait for it or retry" >&2
  else
    echo "dispatch: a stale dispatch lock for $branch remains from a hard-killed dispatch — remove $dispatch_lock and retry" >&2
  fi
  exit 1
fi
trap 'rm -f "$dispatch_lock" "${claude_json_lock:-}"' EXIT INT TERM HUP

# Reuse-or-refuse (#17). git allows exactly one worktree per branch, so a dispatch
# onto a branch that already has one lands in the same directory. Occupancy is a
# WORKER WINDOW (crew occupants, keyed on @crew_name), not a running engine: a
# finished agent drops to a shell prompt, and a command-based check would read the
# window as empty.
#
# Only a TERMINAL bus state licenses the reclaim (#71). Every engine ships behind
# a wrapper, so "no engine here" reads false on a live worker whenever the wrapper
# is one the check doesn't recognise — far too weak to kill on. The bus state is
# the worker's own word, so it is the gate; the engine count is advisory. The cost
# is deliberate: a worker that dies without posting anything holds the branch until
# a human kills the window, which the refusal spells out and stall-watch resolves
# on its own after 30 minutes.
prev_wt="$(git worktree list --porcelain | awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')"
if [ -n "$prev_wt" ]; then
  occ=$(crew occupants "$prev_wt")
  if [ "$occ" != "[]" ]; then
    newest=$(crew sessions "$branch" | jq -c 'last')
    state=$(printf '%s' "$newest" | jq -r '.state // "none"')
    terminal=$(printf '%s' "$newest" | jq -r '.terminal // false')
    engine=$(printf '%s' "$occ" | jq -r 'map(select(.engine)) | length')
    # An `exited` row is the SessionEnd backstop, not the worker's own word, and
    # (#69) it fires under the bare `worker:$branch` id for a subagent too — so a
    # bare `exited` can be `last` while the real `#session` row is still
    # `working`. A live engine pane is the same defence-in-depth reap already
    # applies: refuse exactly like the non-terminal case rather than reclaim.
    if [ "$terminal" != true ] || { [ "$state" = exited ] && [ "$engine" -gt 0 ]; }; then
      nm=$(printf '%s' "$occ" | jq -r '.[0].name')
      win=$(printf '%s' "$occ" | jq -r '.[0].window')
      wid=$(printf '%s' "$newest" | jq -r '.worker_id // ""')
      {
        echo "dispatch: $nm ($wid) is $state in that worktree (window $win) — git allows one worktree per branch."
        [ "$engine" -gt 0 ] || echo "  no engine pane detected — it may have crashed, or the check may not recognise its wrapper; the bus has not seen it finish."
        echo "  redirect it:  crew reply worker:$branch \"<directive>\""
        echo "  or take over: tmux kill-window -t $win, then re-dispatch"
      } >&2
      exit 1
    fi
    # Terminal on the bus — finished work squatting the tree. Reclaim rather than
    # stack beside it. Best-effort, like reap's kills.
    for w in $(printf '%s' "$occ" | jq -r '.[].window'); do
      tmux kill-window -t "$w" 2>/dev/null || true
      echo "dispatch: reclaimed $w at $prev_wt (session $state)"
    done
    jq -nc --arg crew "$crew_id" --arg branch "$branch" --arg state "$state" --argjson occ "$occ" \
      '{ts:(now*1000|floor), crew_id:$crew, kind:"reclaim", branch:$branch, state:$state,
          windows:($occ|map(.window))}' >>"$crew_dir/events.jsonl"
  fi
fi

case "$switch_mode" in
create)
  wt switch -c "$branch" -b "$default_base_oid" -y --config-set "$wt_post_switch"
  echo "dispatch: created branch $branch from $default_base_label ($default_base_short)"
  # A reworded re-dispatch slugs to a different name, so it creates cleanly off the
  # default branch and silently strands the earlier branch's uncommitted work
  # (#73). Warn only — a second branch may be what the operator wants. Local
  # heads only: the stranded work is uncommitted and local.
  siblings="$(git for-each-ref --format='%(refname:short)' "refs/heads/${branch%"$slug"}*" | grep -vFx "$branch" || true)"
  if [ -n "$siblings" ]; then
    # The slug is a lossy 40-char projection, so the branch name cannot be read
    # back into a title; the original survives on the sibling's own dispatch row.
    # The branch comes back with it: max_by(.ts) spans every sibling, so with more
    # than one listed the title needs an owner. A Linear dispatch appends nothing
    # before this point, and bare jq on a missing events.jsonl exits 2 — fatal
    # under `set -euo pipefail`.
    sibling_recovered="$(jq -rs --arg sibs "$siblings" \
      '($sibs | split("\n")) as $b
       | [.[] | select(.kind == "dispatch" and (.branch | IN($b[])))]
       | max_by(.ts) | [(.branch // ""), ((.title // "") | gsub("[[:cntrl:]]"; ""))] | @tsv' \
      "$crew_dir/events.jsonl" 2>/dev/null || true)"
    IFS=$'\t' read -r sibling_branch sibling_title <<<"$sibling_recovered"
    {
      echo "dispatch: branch(es) for this id already exist:"
      printf '%s\n' "$siblings" | sed 's/^/  /'
      if [ -n "$sibling_title" ]; then
        echo "dispatch: creating $branch instead — the work in the branch above will be left behind. To resume $sibling_branch, re-dispatch with its original title:"
        # Printed as data on its own line, never interpolated into a paste-ready
        # command: the title is free-form operator text and quoting it correctly
        # for a shell is exactly where this would break.
        printf '    %s\n' "$sibling_title"
      else
        echo "dispatch: creating $branch instead — the work in the branch above will be left behind. No bus row carries its original title, so resuming it means reconstructing the wording that produced its name."
      fi
    } >&2
  fi
  ;;
resume)
  # `wt switch -c` was also, accidentally, what refused a branch checked out where
  # a worker has no business opening (#73). Occupancy cannot replace it: it keys on
  # @crew_name and skips the dispatcher's window and the caller's, so the primary
  # checkout, dispatch's own cwd and a human sitting in a plain shell all read as
  # empty. This runs after that gate, so it only ever sees a tree the gate allowed.
  if [ -n "$prev_wt" ]; then
    # No `exit` in the awk: an early close SIGPIPEs git and trips pipefail.
    primary_wt="$(git worktree list --porcelain | awk '/^worktree /{if (!p) p=$2} END{print p}')"
    if [ "$prev_wt" = "$primary_wt" ]; then
      echo "dispatch: $branch is checked out in the primary worktree $prev_wt — a worker must not run in the main checkout. Move the branch to its own worktree, then re-dispatch." >&2
      exit 1
    fi
    case "$PWD/" in
    "$prev_wt"/*)
      echo "dispatch: $branch is checked out at $prev_wt, the worktree this dispatch is running from — a worker would open on top of you. Re-dispatch from elsewhere." >&2
      exit 1
      ;;
    esac
    # A pane at that path with an EMPTY @crew_name is a non-worker occupant — a
    # human in a plain shell. Complements `crew occupants`, which requires a
    # non-empty @crew_name. list-panes, not list-windows: in a window format
    # pane_current_path resolves to the ACTIVE pane only, so a human in an
    # inactive pane here would go undetected.
    # @crew_name last, unlike `_occupants`' order: tab is IFS whitespace, so an
    # empty middle field collapses and `read` would shift the path into it — and
    # empty is exactly the value being matched on here.
    while IFS=$'\t' read -r res_win res_path res_name; do
      [ -n "$res_win" ] || continue
      [ "$res_path" = "$prev_wt" ] || continue
      [ -z "$res_name" ] || continue
      echo "dispatch: window $res_win is sitting in $prev_wt with no worker identity — a worker would open on top of it. Close that window, or take the branch over by hand." >&2
      exit 1
    done <<WINDOWS
$(tmux list-panes -a -F '#{window_id}	#{pane_current_path}	#{@crew_name}' 2>/dev/null || true)
WINDOWS
  fi
  wt switch "$branch" -y --config-set "$wt_post_switch"
  branch_short="$(git rev-parse --short "$branch")"
  echo "dispatch: resuming branch $branch at $branch_short"
  ;;
name) wt switch "$branch" -y --config-set "$wt_post_switch" ;;
fetch-name)
  # `--` before the ref: a PR head branch is attacker-named (up to git's ref
  # rules, which permit a leading `-`), and a bare positional would let a
  # branch named e.g. `--upload-pack=...` be parsed as a fetch option.
  git fetch origin -- "$branch"
  wt switch "$branch" -y --config-set "$wt_post_switch"
  ;;
pr-ref) wt switch "pr:$pr_number" -y --config-set "$wt_post_switch" ;;
esac

sanitized="${branch//\//-}"

# Session id is issued here and carried in the environment by all three engine
# launchers. epoch+pid prevents two same-second dispatches on one branch from
# sharing an identity (#17).
session="${DISPATCH_SESSION_ID:-s$(date +%s)-$$}"
worker_id="worker:$branch#$session"

# Claim the branch on the bus before the tmux window exists (#32): reap's
# idle-release loop reads a branch's newest bus event, and a session that
# hasn't posted `working` yet would otherwise still read as whatever the
# prior session last posted — often a stale `done` — releasing the window
# this dispatch is about to create. A claim has no `body`, so it can never
# itself satisfy reap's terminal-state check; it only masks a stale `done`
# until the worker's own `working` post supersedes it. If this dispatch
# aborts before that happens, the claim becomes the branch's permanent
# latest bus event and idle-release can never touch it again.
jq -nc --arg crew "$crew_id" --arg from "$worker_id" \
  '{ts:(now*1000|floor), crew_id:$crew, from:$from, kind:"claim"}' \
  >>"$crew_dir/events.jsonl"

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

# Pre-trust the worktree for claude (#40). Claude Code keys workspace trust by
# absolute path in ~/.claude.json under .projects["<path>"].hasTrustDialogAccepted
# — confirmed by inspecting an already-trusted checkout's own entry there, not
# guessed. A fresh worktree path is unknown to that store, and
# --permission-mode auto does NOT bypass the resulting trust dialog, so an
# unattended worker wedges on it before ever reading WORKER_TASK.md. Stamp
# trust here so the worker's first turn never sees the prompt. Locked with the
# same ln -s idiom as dispatch_lock above: ~/.claude.json is shared by every
# concurrent dispatch on this machine, and an unlocked read-modify-write would
# lose one racer's stamp to another's. The lock only serializes dispatch
# invocations against each other — a live claude session's own background
# writes to ~/.claude.json race it too, same as they'd race any other writer;
# that residual loss window is accepted, not solved, here. Aborts the dispatch
# on failure — a worker that can't be pre-trusted just reproduces the wedge
# this fixes.
if [ "$agent" = claude ]; then
  claude_json="$HOME/.claude.json"
  claude_json_lock_path="$claude_json.dispatch.lock"
  trusted=1
  for _ in 1 2 3 4 5; do
    # claude_json_lock (the trap-visible name at the top-level `trap` above)
    # is only ever assigned once ln -s has actually made us the owner — a
    # racer that exhausts all 5 attempts must exit with claude_json_lock still
    # unset, or the EXIT trap would delete a lock file some other, still-running
    # dispatch legitimately owns.
    if ln -s "$$" "$claude_json_lock_path" 2>/dev/null; then
      claude_json_lock="$claude_json_lock_path"
      trust_tmp="$(mktemp "$claude_json.tmp.XXXXXX")"
      if [ -f "$claude_json" ]; then
        existing="$(cat "$claude_json")"
      else
        existing='{}'
      fi
      if printf '%s' "$existing" | jq --arg path "$wt_path" \
        '.projects[$path].hasTrustDialogAccepted = true' >"$trust_tmp" \
        && mv "$trust_tmp" "$claude_json"; then
        trusted=0
      else
        rm -f "$trust_tmp"
      fi
      rm -f "$claude_json_lock"
      break
    fi
    sleep 1
  done
  if [ "$trusted" -ne 0 ]; then
    held=$(readlink "$claude_json_lock_path" 2>/dev/null || true)
    if [ -n "$held" ] && kill -0 "$held" 2>/dev/null; then
      echo "dispatch: could not pre-trust worktree $wt_path — $claude_json_lock_path is held by pid $held (another dispatch mid-scaffold) — the worker would wedge on the workspace-trust dialog" >&2
    else
      echo "dispatch: could not pre-trust worktree $wt_path — a stale lock from a hard-killed dispatch remains at $claude_json_lock_path; remove it and retry" >&2
    fi
    exit 1
  fi
fi

# Pre-allow direnv for the worktree (#40). direnv's allow-list re-validates
# *content* on every load, keyed by the realpath of the .envrc — so a fresh
# worktree's byte-identical .envrc is unseen even though the main checkout's
# copy is already allowed, but a genuinely different .envrc is (correctly)
# blocked again. A --pr worktree is checked out to the PR's actual head,
# which can be a fork (isCrossRepository, handled below) carrying
# attacker-controlled .envrc content — auto-approving there would rubber-stamp
# code an external PR author wrote, sight unseen, right before the worker's
# devshell (and the operator's own shell, if direnv-hooked) sources it. Only
# --pr is skipped: create/name/fetch-name all check out a branch from this
# machine's own trusted origin, not a fork. Aborts the dispatch on an
# unexpected direnv failure so a devshell-less worker never gets scaffolded to
# fail its gate in a confusing way much later.
if [ -n "$pr_number" ]; then
  echo "dispatch: --pr worktree — not auto-approving direnv; review $wt_path/.envrc and run \`direnv allow $wt_path\` by hand once you trust it" >&2
elif ! direnv allow "$wt_path"; then
  echo "dispatch: direnv allow failed for $wt_path — the worker's devshell will not load" >&2
  exit 1
fi

# --pr: verify the attached worktree actually sits at the PR head. `wt switch`
# attaches to an existing worktree without fetching or resetting it, so a
# stale local branch would otherwise go unnoticed.
if [ -n "$pr_number" ]; then
  worktree_head="$(git -C "$wt_path" rev-parse HEAD)"
  if [ "$worktree_head" != "$head_oid" ]; then
    # A worker's own WORKER_TASK.md is intentionally untracked and is only
    # trashed by `crew reap`, not on reclaim, so it alone must not count as
    # dirty.
    dirt="$(git -C "$wt_path" status --porcelain | grep -v '^?? WORKER_TASK\.md$' || true)"
    if [ -z "$dirt" ]; then
      echo "dispatch: worktree HEAD $worktree_head != PR $pr_number head $head_oid — fetching and hard-resetting" >&2
      # `--` before the ref: see the fetch-name comment above, same reasoning.
      git -C "$wt_path" fetch origin -- "$head"
      git -C "$wt_path" reset --hard "$head_oid"
    else
      echo "dispatch: worktree HEAD $worktree_head != PR $pr_number head $head_oid, and the worktree has uncommitted changes — refusing to reset. Resolve manually at $wt_path, then re-dispatch." >&2
      exit 1
    fi
  fi
fi

# Log the dispatch decision to the crew bus for later `crew report`.
dispatch_shape="${DISPATCH_SHAPE:-}"
jq -nc --arg crew "$crew_id" --arg branch "$branch" --arg session "$session" \
  --arg engine "$agent" --arg model "$model" --arg tier "$tier" --arg effort "$effort" \
  --arg shape "$dispatch_shape" --arg title "$title" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"dispatch", branch:$branch, session:$session, engine:$engine, model:$model, tier:$tier, effort:$effort, shape:$shape, title:$title}' \
  >>"$crew_dir/events.jsonl"

# FleetView-style codename+color, derived from the branch (deterministic).
ident=$(crew identity "$branch")
agent_name=$(printf '%s' "$ident" | jq -r .name)
agent_color=$(printf '%s' "$ident" | jq -r .tmux)

# A resume issued without re-passing $DISPATCH_SPEC would otherwise leave a
# header-only doc, destroying the task text — and, on a `plan: provided` run, the
# plan of record — of the run it is meant to continue (#73). Captured ABOVE the
# block below: `>` truncates the target before the block's first command runs, so
# reading the old file inside it reads zero bytes.
carried=""
if [ "$switch_mode" = resume ] && [ -z "${DISPATCH_SPEC:-}" ] && [ -f "$wt_path/WORKER_TASK.md" ]; then
  carried="$(sed -n '/^## Task$/,$p' "$wt_path/WORKER_TASK.md")"
fi

# Stamp the task file: header fields the worker protocol reads, the closes
# line, and the full task body from $DISPATCH_SPEC (falls back to the title).
# The review contract is appended so the dispatcher never re-authors it as
# per-worker prose.
{
  printf 'tier: %s\nkind: %s\ndraft: %s\nengine: %s\nmodel: %s\neffort: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\nworker_id: %s\n' \
    "$tier" "$kind" "$draft" "$agent" "$model" "$effort" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name" "$worker_id"
  if [ -n "$pr_number" ]; then
    printf 'base: %s\n' "$base_ref"
  fi
  if [ "$switch_mode" = resume ]; then
    printf 'resume: true\n'
  fi
  if [ -n "${DISPATCH_SPEC:-}" ] && [ -f "${DISPATCH_SPEC:-}" ]; then
    printf '\n## Task\n\n'
    cat "$DISPATCH_SPEC"
  elif [ -n "$carried" ]; then
    # $carried already opens with its own `## Task` heading.
    printf '\n%s\n' "$carried"
  fi
  if [ "$kind" = review ]; then
    printf '\n'
    cat "$review_contract"
  fi
} >"$wt_path/WORKER_TASK.md"

read -r win pane < <(tmux new-window -d -c "$wt_path" -n "$sanitized" -e "CREW_WORKER_ID=$worker_id" -e "CREW_ID=$crew_id" -P -F '#{window_id} #{pane_id}')

# Printed so the dispatcher can address this session in the gap before the worker
# boots — its startup drain is unbounded, so a scoping note posted now still lands.
echo "worker_id: $worker_id"

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

# Same carrier, for a worker landing in a tree that already holds its spec, plan
# and partial work (#73). No apostrophes anywhere in this string: all three launch
# strings single-quote the prompt inside a double-quoted `tmux send-keys`
# argument, and one apostrophe silently breaks the line.
resume_note=""
if [ "$switch_mode" = resume ]; then
  resume_note=" You are resuming an interrupted run on this branch, not starting it: do not re-run the spec or plan phases. Read SPEC.md and PLAN.md (repo root or docs/superpowers/) and git status before anything else, then continue from the first unfinished step. Check whether this branch already has an open PR before you push, and push to that PR instead of opening a second one."
fi

# The launch prompt is a user-turn instruction, so it outranks the protocol: a
# review worker told to "push and open a PR" here would do exactly that on
# someone else's PR head. Swap the mandate instead of relying on the contract to
# talk the worker out of it.
push_mandate=" Push when pre-push passes; open a PR."
if [ "$kind" = review ]; then
  push_mandate=" Review only — do not edit, commit, push, or open a PR; post one COMMENT review and report to the bus."
fi

# Execute subagents never read WORKER_PROTOCOL.md. Codex/cursor workers must
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

if [ "$agent" = codex ]; then
  # service_tier pinned: the interactive /fast toggle persists locally and would
  # otherwise leak into unattended workers, burning ChatGPT credits at 2.5x for
  # latency nobody is watching.
  # agents.*: enable native delegation, cap concurrency at 3 (parity with rule 1),
  # and pin subagent effort one rung down. Never pass ultra as subagent effort.
  tmux send-keys -t "$pane" \
    "codex --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default -c agents.enabled=true -c agents.max_concurrent_threads_per_session=3 -c agents.default_subagent_reasoning_effort=$codex_subagent_effort --dangerously-bypass-approvals-and-sandbox 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}'" Enter
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
    "CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}'" Enter
else
  tmux send-keys -t "$pane" \
    "claude --name $agent_name --model $model --effort $effort $mcp_flag $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and run it end-to-end.${push_mandate}${plan_note}${resume_note}'" Enter
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
CREW_ID="$crew_id" nohup crew stall-watch "$worker_id" --pane "$pane" --engine "$agent" >/dev/null 2>&1 &
