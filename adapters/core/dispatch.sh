# shellcheck shell=bash
# dispatch — scaffold a worker: issue/ticket -> worktree -> task file -> baked agent.
# Native PATH tool (was a fish autoload function). Crew id is delivered
# explicitly (--crew-id > $CREW_ID > error); a binary can't export env back to
# its caller, so the old `set -gx CREW_ID` persistence trick is gone.
# The shebang + `set -euo pipefail` are prepended by writeShellApplication, so
# this file is only the function body (see crew.sh for the same pattern).

usage() {
  echo -e "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh|max|ultra> [--agent claude|codex|cursor|pi] [--mcp <profile>] [--grid] [--no-grid] [--roles <r1[=model|agent:model][@effort],...>] [--plan provided|required] [--crew-id <id>] [--pr N] [--review] [--draft|--no-draft] [--ignore-budget] [--ignore-map] [LINEAR-ID|#N] <title...>\n       dispatch resume [--agent E] [--model M] [--effort E] [--mcp P] [--fresh] [--print] [extra prompt...]" >&2
}

valid_effort() {
  case "$1" in
  low | medium | high | xhigh | max | ultra) return 0 ;;
  *) return 1 ;;
  esac
}

valid_role_model() {
  local role_agent="$1" role_model="$2"
  case "$role_agent" in
  claude) [[ $role_model =~ ^(opus|sonnet|haiku|fable|claude-[a-z0-9][a-z0-9.-]*)$ ]] ;;
  codex) [[ $role_model =~ ^gpt-[0-9]+(\.[0-9]+)*(-[a-z0-9][a-z0-9.-]*)?$ ]] ;;
  cursor) [[ $role_model =~ ^([a-z0-9][a-z0-9.-]*)(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$ ]] ;;
  pi) [[ $role_model =~ ^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._/-]*$ ]] ;;
  *) return 1 ;;
  esac
}

# pace_rule_target <agent> <model> <effort> — refuse one premium launch target
# when its fresh 7d window is materially ahead of pace.
pace_rule_target() {
  local target_agent="$1" target_model="$2" target_effort="$3" model_downgrade="" effort_downgrade="" rung_pct used_pct ahead pace_notice pace_clause
  [ -z "${ignore_budget:-}" ] && [ -f "$budget_file" ] || return 0
  case "$target_agent:$target_model" in
  claude:opus | claude:claude-opus-* | claude:fable | claude:claude-fable-*) model_downgrade="sonnet" ;;
  codex:gpt-5.6-sol) model_downgrade="gpt-5.6-terra" ;;
  cursor:cursor-grok-4.6-high | cursor:cursor-grok-4.6-high\[* ) model_downgrade="cursor-grok-4.6-medium" ;;
  esac
  case "$target_effort" in
  max) effort_downgrade="xhigh" ;;
  xhigh) effort_downgrade="high" ;;
  esac
  [ -n "$model_downgrade$effort_downgrade" ] || return 0
  rung_pct=$(jq -r --arg e "$target_agent" --argjson now "$(date +%s)" '
    def elapsed_pct($w): (100 * (604800 - ($w.resets_at - $now)) / 604800) as $x
      | if $x < 0 then 0 elif $x > 100 then 100 else $x end;
    if (.fetched_epoch + 7200) < $now then empty
    elif .engines[$e] == null or .engines[$e].windows["7d"] == null then empty
    else .engines[$e].windows["7d"] as $w
      | if $w.used_pct < 70 then empty
        elif $w.resets_at == null then "\($w.used_pct)"
        else ($w.used_pct - elapsed_pct($w)) as $ahead
          | if $ahead > 15 then "\($w.used_pct)|\($ahead | round)" else empty end
        end
    end' "$budget_file" 2>/dev/null || true)
  [ -n "$rung_pct" ] || return 0
  used_pct="$rung_pct" pace_notice="" pace_clause=""
  if [[ $rung_pct == *"|"* ]]; then
    used_pct="${rung_pct%%|*}"; ahead="${rung_pct#*|}"
    pace_notice=" ($ahead ahead of pace)"; pace_clause=" and $ahead points ahead of pace"
  fi
  if [ -n "$model_downgrade" ]; then
    if [ "${DISPATCH_IGNORE_RUNG:-}" = "$target_model" ]; then
      echo "dispatch: rung refusal skipped (DISPATCH_IGNORE_RUNG) — '$target_model' on --agent $target_agent at 7d ${used_pct}%${pace_notice}" >&2
    else
      echo "dispatch: $target_agent 7d is at ${used_pct}%${pace_clause} — the premium rung ($target_model) is refused; use the standard rung ($model_downgrade) instead, set DISPATCH_IGNORE_RUNG=$target_model to override just this refusal, or pass --ignore-budget (the human's spend decision, also disarms the 95% stop). See dispatch-orchestration.md \"Tier map\"." >&2
      exit 1
    fi
  fi
  if [ -n "$effort_downgrade" ]; then
    if [ "${DISPATCH_IGNORE_RUNG:-}" = "$target_effort" ]; then
      echo "dispatch: effort refusal skipped (DISPATCH_IGNORE_RUNG) — '$target_effort' on --agent $target_agent at 7d ${used_pct}%${pace_notice}" >&2
    else
      echo "dispatch: $target_agent 7d is at ${used_pct}%${pace_clause} — the premium effort ($target_effort) is refused; use $effort_downgrade instead, set DISPATCH_IGNORE_RUNG=$target_effort to override just this refusal, or pass --ignore-budget (the human's spend decision, also disarms the 95% stop). See dispatch-orchestration.md \"Tier map\"." >&2
      exit 1
    fi
  fi
}

# Ensure the `dispatched` claim-marker label exists. A no-op if it already
# does — must never abort a dispatch on that account.
_ensure_dispatched_label() {
  gh label create dispatched --color 1D76DB \
    --description "Claimed by a dispatcher crew; a worker is on it" >/dev/null 2>&1 || true
}

# Post a best-effort context comment on a dispatched GitHub issue. The
# `dispatched` label stays the claim semaphore; this comment is history only
# and must never abort a dispatch.
_post_dispatch_comment() {
  local issue="$1" name="$2" engine="$3" model="$4" tier="$5" effort="$6" \
    branch="$7" wt_path="$8" session="$9" worker_id="${10}" crew_id="${11}" resume="${12}"
  local verb="dispatched" host="${HOSTNAME:-$(uname -n)}"
  [ "$resume" = true ] && verb="(resumed) dispatched"
  local body
  body="$(cat <<EOF
🚀 **$name** $verb — $engine · $model · $tier

| | |
|---|---|
| **Branch** | \`$branch\` |
| **Worktree** | \`$wt_path\` |
| **Host** | \`$host\` |
| **Agent** | $engine · $model · $tier (effort: $effort) |
| **Session** | \`$session\` |
| **Worker** | \`$worker_id\` |
| **Crew** | \`$crew_id\` |

<!-- dispatched -->
EOF
)"
  gh issue comment "$issue" --body "$body" >/dev/null 2>&1 || {
    echo "dispatch: could not post dispatch-context comment on issue #$issue (non-fatal)" >&2
  }
}

# crew.sh's atomic-append helper, duplicated (not sourced): this file builds
# as its own standalone writeShellApplication with no shared lib. A bare
# `printf >>` isn't one write(2), so concurrent writers to this shared log
# can splice a large line with another process's append (#55, #61).
_bus_append() { printf '%s\n' "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"; }

# Protocol directory. The env override is the dev loop: point it at a checkout
# and protocol edits take effect on the next dispatch with no rebuild. The
# default is substituted to a store path at build time.
PROTOCOL_DIR="${DISPATCHER_PROTOCOL_DIR:-@protocolDir@}"
# Absolute: role panes run it minutes later from the worktree, not from this cwd.
dispatch_self="$(realpath -- "$0")"
budget_file="${XDG_DATA_HOME:-$HOME/.local/share}/crew/engine-budget.json"

# Harness skill directory, handed to pi workers via --skill. Same env-override
# dev loop as PROTOCOL_DIR, same build-time store-path default. Unsubstituted
# (a non-Nix install) it is not a directory, and pi_skill_args' probe drops it.
SKILLS_DIR="${DISPATCHER_SKILLS_DIR:-@skillsDir@}"

# Engine roster helpers must precede every early command, including lazy role
# spawning, so every launch path rejects a disabled engine before scaffolding.
ENGINES_ALL="claude codex cursor pi"

engine_cli() {
  case "$1" in
  cursor) printf 'cursor-agent' ;;
  *) printf '%s' "$1" ;;
  esac
}

engine_enabled() {
  case " ${DISPATCH_ENGINES:-$ENGINES_ALL} " in
  *" $1 "*) return 0 ;;
  esac
  return 1
}

check_engine() {
  local cli
  engine_enabled "$1" || {
    echo "dispatch: $2 is not enabled here (enabled: ${DISPATCH_ENGINES:-$ENGINES_ALL})" >&2
    exit 1
  }
  cli="$(engine_cli "$1")"
  command -v "$cli" >/dev/null 2>&1 || {
    echo "dispatch: $2 is enabled but not installed (no '$cli' on PATH)" >&2
    exit 1
  }
}

# _require_protocol_files <dir> <file...> — abort before any scaffolding if
# a required protocol file is missing from $PROTOCOL_DIR. $DISPATCHER_PROTOCOL_DIR
# can point at a stale checkout (#177); this stops the launch instead of
# spawning an engine against a missing --append-system-prompt(-file) target.
_require_protocol_files() {
  local dir="$1" f missing=()
  shift
  for f in "$@"; do
    [ -f "$dir/$f" ] || missing+=("$f")
  done
  [ "${#missing[@]}" -eq 0 ] && return 0
  local override=""
  [ -n "${DISPATCHER_PROTOCOL_DIR:-}" ] && override=" (DISPATCHER_PROTOCOL_DIR=$DISPATCHER_PROTOCOL_DIR)"
  echo "dispatch: missing protocol file(s) in \$PROTOCOL_DIR ($dir)${override}: ${missing[*]} — refusing to launch" >&2
  exit 1
}

# _check_protocol_rev <dir> <label> — refuse a protocol directory whose content
# does not match this script's baked revision (#184, #193). The build
# substitutes the content hash of adapters/core/protocols for @protocolRev@;
# at runtime this recomputes the same hash from the files actually in $dir and
# refuses a mismatch before any scaffolding. The rule is byte-identical to
# flake.nix's: the directory's files (dotfiles included, matching readDir),
# names sorted byte-wise (matching builtins.attrNames), each hashed —
# `name:sha256;` entries, sha256 of the concatenation, first 16 hex chars. It
# is pinned against the Nix implementation by tests/module.bats, including an
# edge-case dir (dotfile, prefix-named pair) where a naive line-sort or a
# non-dotglob glob would diverge. A stale dir — an old store path held by a
# long-lived DISPATCHER_PROTOCOL_DIR export, or a checkout whose content
# drifted from the script's build — hashes differently and is refused; there
# is no committed PROTOCOL_REV file left to go stale, so two PRs editing
# different protocol files can merge in either order. Six small files hash in
# a few milliseconds. A raw checkout script (marker unsubstituted) cannot bind
# a revision and skips with a one-line warning.
_check_protocol_rev() {
  local dir="$1" label="$2" stamped_rev="@protocolRev@" dir_rev entries=""
  local names=() file name sig
  # The sentinel is the placeholder's *shape*, not the literal: flake.nix's
  # replaceStrings (and a test's sed) rewrite every @protocolRev@ occurrence,
  # so a literal comparison would make a substituted script skip its own
  # check. A baked rev (16 hex chars) never starts with @.
  if [[ "$stamped_rev" == @* ]]; then
    echo "$label: unsubstituted protocol revision (raw checkout script) — skipping the protocol revision consistency check" >&2
    return 0
  fi
  # Names only, not `name:hash;` lines: sorting full lines diverges from
  # attrNames when one name is a prefix of another ('X' vs 'X1' — line-sort
  # puts X1 first because '1' < ':'). dotglob makes the glob see dotfiles the
  # way readDir does; nullglob keeps an empty dir from globbing a literal '*'
  # into the file set. Sorting by name then hashing in that order mirrors
  # flake.nix's attrNames + map exactly.
  shopt -s dotglob nullglob
  for file in "$dir"/*; do
    [ -f "$file" ] || continue
    names+=("$(basename "$file")")
  done
  shopt -u dotglob nullglob
  if [ ${#names[@]} -gt 0 ]; then
    mapfile -t names < <(printf '%s\n' "${names[@]}" | LC_ALL=C sort)
  fi
  for name in "${names[@]}"; do
    sig="$(sha256sum "$dir/$name" | cut -d' ' -f1)"
    entries+="${name}:${sig};"$'\n'
  done
  # The newlines are a construction convenience only — strip them before
  # hashing, matching flake.nix's concatStringsSep "" (clean line-join with no
  # separator).
  dir_rev="$(printf '%s' "$entries" | tr -d '\n' | sha256sum | cut -d' ' -f1 | cut -c1-16)"
  if [ "$dir_rev" != "$stamped_rev" ]; then
    echo "$label: protocol directory version mismatch — refusing to launch" >&2
    echo "$label:   script protocol revision: $stamped_rev" >&2
    echo "$label:   \$PROTOCOL_DIR content revision: $dir_rev" >&2
    echo "$label:   \$PROTOCOL_DIR: $dir" >&2
    [ -n "${DISPATCHER_PROTOCOL_DIR:-}" ] && echo "$label:   DISPATCHER_PROTOCOL_DIR override: $DISPATCHER_PROTOCOL_DIR" >&2
    exit 1
  fi
}

# --- role-grid helpers -----------------------------------------------------

# role_color <role> — a stable tmux colour per role. Known roles get a semantic
# colour; anything else falls back to crew's deterministic FleetView palette, so
# a role is always the same colour run to run (`--hash`: never occupancy-shifted).
role_color() {
  case "$1" in
  spec-critic) printf 'colour141' ;; # mauve
  plan-critic) printf 'colour111' ;; # blue
  reviewer) printf 'colour114' ;;    # green
  security) printf 'colour174' ;;    # red
  consult) printf 'colour180' ;;     # yellow
  *) crew identity --hash "$1" 2>/dev/null | jq -r '.tmux // "colour250"' ;;
  esac
}

# decorate_pane <pane> <role> — put the role on the pane border, colour that
# border by role, and seed @crew_state (rendered on the border). tmux keeps these
# per pane, so a role's colour and label survive a tiled layout and a zoom.
decorate_pane() {
  local pane="$1" role="$2" color
  color="$(role_color "$role")"
  tmux set-option -p -t "$pane" @crew_role "$role"
  tmux set-option -p -t "$pane" @crew_role_color "$color"
  tmux set-option -p -t "$pane" @crew_state idle
  tmux set-option -p -t "$pane" pane-border-style "bg=#{@thm_bg},fg=$color"
  tmux set-option -p -t "$pane" pane-active-border-style "bg=#{@thm_bg},fg=$color,bold"
  tmux set-option -p -t "$pane" pane-border-format " #[bold]#{@crew_role}#[nobold] #{@crew_state} "
  tmux set-option -w -t "$pane" pane-border-status top
}

# layout_grid <window> — main-vertical, pinning the lead (pane 1, launched
# before any role pane splits off it) to 60% width. Role panes only carry
# short verdict traffic and need far less room than the lead's diff/test/tool
# output.
layout_grid() {
  local win="$1"
  tmux set-window-option -t "$win" main-pane-width 60%
  tmux select-layout -t "$win" main-vertical
}

# An empty PI_CODING_AGENT_DIR falls back to ~/.pi/agent, so a broken seeder
# must abort before pi ever launches.
pi_agent_dir=""
seed_pi_agent_dir() {
  pi_agent_dir="$(crew pi-agent-dir)" || pi_agent_dir=""
  case "$pi_agent_dir" in
  /*) [ -d "$pi_agent_dir" ] && return 0 ;;
  esac
  echo "dispatch: could not seed the pi worker agent dir (crew pi-agent-dir) — refusing to launch pi against ~/.pi/agent" >&2
  exit 1
}

# split_role_pane <window> <worktree> <role> <worker_id> <crew_id> — create a
# role pane, decorate it, and echo its pane id. `tmux new-window -e` scopes to
# that window's first pane only, so every pane split off it must repeat the lead's
# identity — the engine wrappers key their config on CREW_WORKER_ID, and a role
# pane without it runs as a personal session. CREW_ROLE_ID marks the pane as a
# role so dispatch-notify does not speak for the lead from it. Reads $branch from
# the caller scope.
split_role_pane() {
  local win="$1" wt="$2" role="$3" worker_id="$4" crew_id="$5" pane
  pane="$(tmux split-window -t "$win" -c "$wt" -e "CREW_WORKER_ID=$worker_id" -e "CREW_ID=$crew_id" -e "CREW_ROLE_ID=role:$branch:$role" -P -F '#{pane_id}')"
  decorate_pane "$pane" "$role"
  printf '%s' "$pane"
}

# shell_quote <var> <text> — set <var> to <text> as ONE single-quoted shell word,
# for splicing into a tmux send-keys command line the pane's own shell re-parses.
# ' and \ are closed out of the quotes and escaped outside them, the one form
# bash and fish agree on: fish (the pane shell) reads \\ and \' as escapes even
# inside single quotes. Not printf %q: that emits $'…' for non-printables or
# under a C locale, which fish cannot parse.
shell_quote() {
  local -n _out="$1"
  local _text="$2" _res="" _c _i
  for ((_i = 0; _i < ${#_text}; _i++)); do
    _c="${_text:_i:1}"
    case "$_c" in
    "'") _res+="'\\''" ;;
    "\\") _res+="'\\\\'" ;;
    *) _res+="$_c" ;;
    esac
  done
  _out="'$_res'"
}

# pi_skill_args <worktree> — emit --skill flags for the worktree's own project
# skill dirs (pi's project skill locations) and for the harness's own skills.
# The pi launches below pass --no-approve, which disables project discovery
# wholesale, so a worker must be handed its skills explicitly; --skill is
# additive and a missing path is only a warning. Only skills cross this line —
# project .pi settings, packages and extensions stay blocked, which is why this
# isn't just dropping --no-approve.
#
# $SKILLS_DIR carries the harness's own skills, which WORKER_PROTOCOL cites as
# the authority for the plan schema and the critic table. pi is the only engine
# with no adapter tree of its own to load them from.
pi_skill_args() {
  local wt="$1" d
  for d in "$wt/.pi/skills" "$wt/.agents/skills" "$SKILLS_DIR"; do
    [ -d "$d" ] && printf ' --skill %q' "$d"
  done
  return 0
}

# launch_role <pane> <worktree> <role> <agent> <model> <effort> — launch the role's engine
# with GRID_PROTOCOL as its system prompt (appended where supported, first prompt
# otherwise). Reads $agent_name and $branch from the caller scope.
#
# The launch line ends in a `; dispatch --role-exited …` continuation: it runs
# only when the engine returns to the pane's shell, so an engine that crashes at
# startup is reported, while a reap (which kills the pane and its shell) is silent.
# `;` is valid in both fish (the pane shell) and bash.
launch_role() {
  local pane="$1" wt="$2" role="$3" r_agent="$4" r_model="$5" r_effort="$6" prompt first quoted_model quoted_dir quoted_prompt quoted_first exit_hook
  printf -v quoted_model '%q' "$r_model"
  printf -v exit_hook " ; %q --role-exited %q --branch %q --pane '%s' --since %s" "$dispatch_self" "$role" "$branch" "$pane" "$(jq -nc 'now*1000|floor')"
  prompt="You are the $role role pane in this task grid. Read WORKER_TASK.md, resolve your role from @crew_role, then follow GRID_PROTOCOL.md: announce yourself and park for an assignment."
  first="Read $PROTOCOL_DIR/GRID_PROTOCOL.md and WORKER_TASK.md, then follow GRID_PROTOCOL.md: announce yourself and park for an assignment (you are the $role role)."
  shell_quote quoted_prompt "$prompt"
  shell_quote quoted_first "$first"
  case "$r_agent" in
  pi)
    [ -n "$pi_agent_dir" ] || {
      echo "dispatch: could not seed the pi worker agent dir (crew pi-agent-dir) — refusing to launch pi against ~/.pi/agent" >&2
      exit 1
    }
    printf -v quoted_dir '%q' "$pi_agent_dir"
    tmux send-keys -t "$pane" "PI_CODING_AGENT_DIR=$quoted_dir pi --name ${agent_name}-${role} --model $quoted_model --thinking $r_effort --append-system-prompt $PROTOCOL_DIR/GRID_PROTOCOL.md --no-approve$(pi_skill_args "$wt") $quoted_prompt$exit_hook" Enter
    ;;
  claude) tmux send-keys -t "$pane" "claude --name ${agent_name}-${role} --model $quoted_model --effort $r_effort --append-system-prompt-file $PROTOCOL_DIR/GRID_PROTOCOL.md --permission-mode auto $quoted_prompt$exit_hook" Enter ;;
  codex) tmux send-keys -t "$pane" "codex --profile worker -m $quoted_model -c model_reasoning_effort=$r_effort -c service_tier=default --dangerously-bypass-approvals-and-sandbox $quoted_first$exit_hook" Enter ;;
  cursor) tmux send-keys -t "$pane" "CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model $quoted_model $quoted_first$exit_hook" Enter ;;
  esac
}

# watch_role <role> <pane> — spawn the detached, engine-agnostic bus watcher for
# a role pane. It types each assignment into the pane and keeps @crew_state
# fresh, so the role never holds a repainting `crew await`.
watch_role() {
  nohup "$0" --role-watch "$1" --pane "$2" --branch "$branch" >/dev/null 2>&1 &
}

# `dispatch --role-watch <role> --pane <pane> [--branch <b>] [--interval S]` —
# an ENGINE-AGNOSTIC role supervisor. It watches the crew bus and, when the lead
# assigns this role work, types the assignment into the role's pane (a normal
# user turn) and keeps the pane's @crew_state fresh. That lets a role end its
# turn instead of holding a repainting `crew await`, and it needs only the pane
# and the bus — so it works for every engine, pi included.
if [ "${1:-}" = "--role-watch" ]; then
  role="${2:-}"
  [ -n "$role" ] || {
    echo "dispatch: --role-watch needs a role name" >&2
    exit 1
  }
  shift 2
  watch_pane=""
  watch_branch=""
  interval=2
  while [ $# -gt 0 ]; do
    case "$1" in
    --pane) watch_pane="${2:-}"; shift 2 ;;
    --branch) watch_branch="${2:-}"; shift 2 ;;
    --interval) interval="${2:-}"; shift 2 ;;
    *)
      echo "dispatch: --role-watch: unexpected argument '$1'" >&2
      exit 1
      ;;
    esac
  done
  [ -n "$watch_pane" ] || {
    echo "dispatch: --role-watch needs --pane <id>" >&2
    exit 1
  }
  watch_branch="${watch_branch:-$(git branch --show-current)}"
  log="$(git rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  role_id="role:$watch_branch:$role"
  since="$(jq -nc 'now*1000|floor')"
  # --role-exited marks a dead role with @crew_exited, an option this watcher
  # never writes: the state border may flicker, but a dead role can never read as
  # live to --spawn-role, and no assignment is typed into its shell prompt.
  watch_exited() {
    [ "$(tmux display-message -p -t "$watch_pane" '#{@crew_exited}' 2>/dev/null || true)" = 1 ]
  }
  watch_set_state() {
    watch_exited && return 0
    tmux set-option -p -t "$watch_pane" @crew_state "$1" 2>/dev/null || true
  }
  watch_set_state idle
  # Exits when the pane is gone (role reaped, or the window closed) or its
  # engine has exited.
  while tmux display-message -p -t "$watch_pane" '#{pane_id}' >/dev/null 2>&1; do
    watch_exited && break
    if [ -f "$log" ]; then
      batch="$(jq -c --arg me "$role_id" --argjson since "$since" \
        'select(.kind=="msg" and .ts>$since and ((.to==$me) or (.from==$me)))' "$log" 2>/dev/null || true)"
      if [ -n "$batch" ]; then
        printf '%s\n' "$batch" | while IFS= read -r ev; do
          if [ "$(printf '%s' "$ev" | jq -r '.to // ""')" = "$role_id" ]; then
            body="$(printf '%s' "$ev" | jq -r '.body // ""')"
            [ -n "$body" ] || continue
            watch_exited && continue
            watch_set_state working
            tmux send-keys -t "$watch_pane" -l "Assignment: $body" 2>/dev/null || true
            tmux send-keys -t "$watch_pane" Enter 2>/dev/null || true
          else
            # A verdict from the role — it is idle again.
            watch_set_state idle
          fi
        done
        next="$(printf '%s\n' "$batch" | jq -s 'map(.ts) | max // empty')"
        since="${next:-$since}"
      fi
    fi
    sleep "$interval"
  done
  exit 0
fi

# `dispatch --spawn-role <role>` — create a lazy grid's role pane on demand in
# the caller's own window/worktree, from roles.json. Idempotent.
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
  spawn_effort_explicit=""
  ignore_budget=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --agent) spawn_agent="${2:-}"; shift 2 ;;
    --model) spawn_model="${2:-}"; shift 2 ;;
    --effort) spawn_effort="${2:-}"; spawn_effort_explicit=1; shift 2 ;;
    --ignore-budget) ignore_budget=1; shift ;;
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
  _require_protocol_files "$PROTOCOL_DIR" WORKER_PROTOCOL.md EVIDENCE_REVIEW.md GRID_PROTOCOL.md
  _check_protocol_rev "$PROTOCOL_DIR" dispatch
  crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
  branch="$(git branch --show-current)"
  roles_file="$crew_dir/artifacts/$branch/roles.json"
  [ -f "$roles_file" ] || {
    echo "dispatch: no role grid recorded for $branch (dispatch without --lazy to use an up-front grid)" >&2
    exit 1
  }
  spec="$(jq -r --arg r "$role" '.[$r] // empty | [.agent, .model, (.effort // "")] | @tsv' "$roles_file")"
  [ -n "$spec" ] || {
    echo "dispatch: role '$role' is not part of this grid" >&2
    exit 1
  }
  agent_name="$(sed -n 's/^agent_name: //p' WORKER_TASK.md)"
  IFS=$'\t' read -r saved_agent saved_model saved_effort <<<"$spec"
  task_effort="$(sed -n 's/^effort: //p' WORKER_TASK.md)"
  effort="${spawn_effort:-${saved_effort:-$task_effort}}"
  spawn_agent="${spawn_agent:-$saved_agent}"
  spawn_model="${spawn_model:-$saved_model}"
  case "$spawn_agent" in
  claude | codex | cursor | pi) ;;
  *) echo "dispatch: --spawn-role '$role' has invalid agent '$spawn_agent'" >&2; exit 1 ;;
  esac
  valid_role_model "$spawn_agent" "$spawn_model" || {
    echo "dispatch: invalid model '$spawn_model' for role '$role'" >&2
    exit 1
  }
  valid_effort "$effort" || {
    echo "dispatch: --spawn-role '$role' has invalid effort '$effort' (expected low, medium, high, xhigh, max, or ultra)" >&2
    exit 1
  }
  if [ "$spawn_agent" = cursor ] && [ -n "$spawn_effort_explicit" ]; then
    echo "dispatch: role '$role' uses --agent cursor, which has no --effort; encode intensity in the bracketed model (for example cursor-model[effort=high])" >&2
    exit 1
  fi
  if { [ "$spawn_agent" = claude ] || [ "$spawn_agent" = pi ]; } && [ "$effort" = ultra ]; then
    echo "dispatch: role '$role' uses --agent $spawn_agent, which does not support --effort ultra" >&2
    exit 1
  fi
  check_engine "$spawn_agent" "role '$role' uses --agent $spawn_agent"
  win="$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}')"
  existing="$(tmux list-panes -t "$win" -F '#{pane_id}|#{@crew_role}|#{?@crew_exited,exited,live}' | awk -F'|' -v r="$role" '$2 == r && $3 != "exited" {print $1; exit}')"
  if [ -n "$existing" ]; then
    echo "role $role is already running in pane $existing"
    exit 0
  fi
  spawn_worker_id="${CREW_WORKER_ID:-$(sed -n 's/^worker_id: //p' WORKER_TASK.md)}"
  spawn_crew_id="${CREW_ID:-$(sed -n 's/^crew_id: //p' WORKER_TASK.md)}"
  if [ -z "$spawn_worker_id" ] || [ -z "$spawn_crew_id" ]; then
    echo "dispatch: --spawn-role: no worker_id/crew_id in the environment or WORKER_TASK.md — a role pane without them runs as a personal session" >&2
    exit 1
  fi
  pace_rule_target "$spawn_agent" "$spawn_model" "$effort"
  [ "$spawn_agent" = pi ] && seed_pi_agent_dir
  role_pane="$(split_role_pane "$win" "$PWD" "$role" "$spawn_worker_id" "$spawn_crew_id")"
  launch_role "$role_pane" "$PWD" "$role" "$spawn_agent" "$spawn_model" "$effort"
  watch_role "$role" "$role_pane"
  echo "spawned role $role ($spawn_agent/$spawn_model) in $role_pane"
  exit 0
fi

# `dispatch --reap-roles` — kill every role pane in the caller's window.
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

# `dispatch --role-exited <role> --branch <b> --pane <p>` — the continuation typed
# after a role's engine command (see launch_role); it runs only once that engine
# is back at the pane's shell. @crew_exited is a pane-local marker, unrelated to
# the bus `exited` state. A lead's `{"final":true}` release is the graceful exit and
# stays silent; --since is the launch time, so a `final` sent to an earlier
# incarnation of the role does not hide this crash. Anything else is a role that
# died before its verdict: tell the dispatcher (`blocked` wakes `crew watch`;
# `exited` would not) and the lead, whose `crew await` would otherwise wait forever.
if [ "${1:-}" = "--role-exited" ]; then
  role="${2:-}"
  [ -n "$role" ] || {
    echo "dispatch: --role-exited needs a role name" >&2
    exit 1
  }
  shift 2
  exited_branch=""
  exited_pane=""
  exited_since=0
  while [ $# -gt 0 ]; do
    case "$1" in
    --branch) exited_branch="${2:-}"; shift 2 ;;
    --pane) exited_pane="${2:-}"; shift 2 ;;
    --since) exited_since="${2:-0}"; shift 2 ;;
    *)
      echo "dispatch: --role-exited: unexpected argument '$1'" >&2
      exit 1
      ;;
    esac
  done
  [ -n "$exited_pane" ] || {
    echo "dispatch: --role-exited needs --pane <id>" >&2
    exit 1
  }
  exited_branch="${exited_branch:-$(git branch --show-current)}"
  role_id="role:$exited_branch:$role"
  tmux set-option -p -t "$exited_pane" @crew_exited 1 2>/dev/null || true
  tmux set-option -p -t "$exited_pane" @crew_state exited 2>/dev/null || true
  log="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)/crew/events.jsonl"
  if [ -f "$log" ] && jq -e --arg me "$role_id" --argjson since "$exited_since" \
    'select(.kind=="msg" and .to==$me and .ts>=$since and ((.body | fromjson? // {} | objects | .final) == true))' "$log" >/dev/null 2>&1; then
    exit 0
  fi
  crew status "$role_id" blocked "role $role engine exited (pane $exited_pane)" || true
  lead_id="${CREW_WORKER_ID:-$(sed -n 's/^worker_id: //p' WORKER_TASK.md 2>/dev/null | head -1 || true)}"
  if [ -n "$lead_id" ]; then
    crew msg "$role_id" "$lead_id" "$(jq -nc --arg r "$role" --arg p "$exited_pane" '{role:$r,event:"role_exited",pane:$p,detail:"engine exited before a verdict"}')" || true
  fi
  exit 0
fi

# `dispatch --engines` — the effective roster: enabled AND installed, in
# canonical order. The dispatcher protocol reads this before judging.
if [ "${1:-}" = "--engines" ]; then
  # shellcheck disable=SC2086 # intentional split of the fixed space-separated roster
  for e in $ENGINES_ALL; do
    engine_enabled "$e" || continue
    command -v "$(engine_cli "$e")" >/dev/null 2>&1 || continue
    echo "$e"
  done
  exit 0
fi

# `dispatch resume` is its own binary — resume skips the issue claim, branch
# creation, task-document rewrite and new-window paths this file is built
# around. Intercepted here so the subcommand reads as part of dispatch, and
# before the positional tier parse below, which would reject it as a tier.
# Scan all arguments so leading flags (e.g. --agent pi) don't mask resume;
# strip the resume token itself — dispatch-resume does not parse it.
for _arg in "$@"; do
  if [ "$_arg" = resume ]; then
    _resume_args=()
    for _a in "$@"; do [ "$_a" != resume ] && _resume_args+=("$_a"); done
    exec dispatch-resume "${_resume_args[@]}"
  fi
done

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
grid_roles=""
grid_flag=""
grid_lazy=""
no_grid=""
grid_status=""
crew_id_flag=""
plan_val="required"
ignore_budget=""
ignore_map=""
draft=false
if [ "${DISPATCH_DRAFT_PR:-}" = 1 ]; then
  draft=true
fi
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
  --no-grid)
    no_grid=1
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
  --ignore-map)
    ignore_map=1
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

# Engine gate. An engine must be enabled (on this machine's roster) and
# available (its CLI installed). $DISPATCH_ENGINES is set from
# programs.dispatcher.engines by home-manager; unset means every engine, so a
# non-Nix checkout and the test suite need no extra setup. $DISPATCH_PROFILE no
# longer gates engines — it is still read below for the work+claude+deep rung.
profile="${DISPATCH_PROFILE:-personal}"

check_engine "$agent" "--agent $agent"

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
      echo "dispatch: model '$model' does not match --agent claude — claude takes an alias (opus, sonnet, haiku, fable) or a full claude-* id (e.g. claude-fable-5-1). Did you mean --agent cursor? See dispatch-orchestration.md \"Model gate\"." >&2
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
    # Cursor fronts other vendors, so id shape is always checked but
    # membership is only knowable offline, best-effort, via a refreshed
    # cache — and only for a subset of cursor's id space (see below).
    # BASH_REMATCH is clobbered by the next [[ =~ ]], so both groups are
    # captured on the spot.
    re_cursor='^([a-z0-9][a-z0-9.-]*)(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$'
    cursor_base=""
    cursor_params=""
    if [[ $model =~ $re_cursor ]]; then
      cursor_base="${BASH_REMATCH[1]}"
      cursor_params="${BASH_REMATCH[2]}"
    fi
    if [ -z "$cursor_base" ] || [[ $cursor_base =~ ^(opus|sonnet|haiku|fable)$ ]]; then
      echo "dispatch: model '$model' does not match --agent cursor — cursor needs a full model id (e.g. kimi-k3-high, cursor-grok-4.6-medium, composer-2.5, claude-opus-5-high). Did you mean --agent claude? See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    # cursor has no --effort knob, so its claude-*/gpt-* ids carry the rung in
    # the id itself; a bracket block exempts only by naming effort= there.
    if [[ $cursor_base =~ ^(claude|gpt)- ]] && [[ ! $cursor_base =~ $re_effort_tail ]] && [[ ! $cursor_params =~ (\[|,)effort= ]]; then
      echo "dispatch: model '$model' is not a cursor id — cursor's claude-*/gpt-* ids carry an effort suffix (gpt-5.6-sol-high, gpt-5.6-sol-high-fast) because cursor has no --effort knob. Live list: cursor-agent --list-models. See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    # Existence check against a refresh-models.sh cache (same `?|strings`
    # idiom as codex's cache check above). Only for non-bracketed ids: a
    # bracketed cell like claude-opus-5[effort=high] resolves its real slug
    # from the bracket's effort= param, and cursor's live catalog only lists
    # the effort-suffixed forms (claude-opus-5-high, not bare claude-opus-5)
    # — so cursor_base there isn't itself an invocable id. A non-bracketed
    # id is checked verbatim, since cursor_base then equals the whole
    # $model, exactly what the live catalog lists.
    if [ -z "$cursor_params" ]; then
      cursor_cache="${XDG_DATA_HOME:-$HOME/.local/share}/crew/cursor-models-cache.json"
      # 24h, not the budget gate's 2h: a model catalog moves at the cadence
      # of new releases (days-to-weeks), not quota's hour-to-hour churn — a
      # 2h bound would leave this degraded almost all the time between
      # manual refresh-models runs.
      if jq -e --argjson now "$(date +%s)" '($now - .fetched_epoch) < 86400 and ([.models[]?|.slug?|strings]|length > 0)' "$cursor_cache" >/dev/null 2>&1 &&
        ! jq -e --arg m "$cursor_base" '[.models[]?|.slug?|strings]|index($m)' "$cursor_cache" >/dev/null; then
        known="$(jq -r '[.models[]?|.slug?|strings|gsub("[[:cntrl:]]";"")]|join(", ")' "$cursor_cache")"
        echo "dispatch: model '$model' is not in this account's cursor model list ($cursor_cache: $known). If it is genuinely new, run refresh-models to update the cache, or set DISPATCH_SKIP_MODEL_CHECK=$model. See dispatch-orchestration.md \"Model gate\"." >&2
        exit 1
      fi
    fi
    ;;
  pi)
    if [[ ! $model =~ ^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._/-]*$ ]]; then
      echo "dispatch: model '$model' does not match --agent pi — pi takes a provider-qualified model id (e.g. openrouter/deepseek/deepseek-v4-pro). See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
  esac
fi

# Escalation helpers — query the bus for prior failed workers and compute
# the one-rung-up escalation target per engine×tier×failed_model.
# _escalation_target <engine> <tier> <failed_model> — prints "<baseline> <escalated>"
# if the failed_model is exactly one rung below a valid escalation target.
# The second word may be "RECORD_ONLY" if the escalated model is already in the row.
# Keep in sync with dispatch-orchestration.md "Model map" execute ladder.
_escalation_target() {
  local eng="$1" tier="$2" failed="$3"
  case "$eng:$tier:$failed" in
  # claude: haiku → sonnet → opus → fable
  claude:standard:sonnet|claude:standard:claude-sonnet-*)         printf 'sonnet opus' ;;
  claude:trivial:haiku|claude:trivial:claude-haiku-*)             printf 'haiku RECORD_ONLY' ;;
  # claude:trivial:sonnet→opus removed — trivial tier must not reach above its row (#249 acceptance)
  claude:deep:sonnet|claude:deep:claude-sonnet-*)                 printf 'sonnet RECORD_ONLY' ;;
  claude:deep:opus|claude:deep:claude-opus-*)                     printf 'opus RECORD_ONLY' ;;
  # codex: luna → terra → sol
  codex:standard:gpt-5.6-luna)                                     printf 'luna RECORD_ONLY' ;;
  codex:standard:gpt-5.6-terra)                                    printf 'terra gpt-5.6-sol' ;;
  codex:deep:gpt-5.6-terra)                                        printf 'terra RECORD_ONLY' ;;
  # codex:trivial:luna→terra removed — trivial tier must not reach above its row
  # cursor: low → medium → high
  cursor:standard:cursor-grok-4.6-low*)                            printf 'low RECORD_ONLY' ;;
  cursor:standard:cursor-grok-4.6-medium*)                         printf 'medium cursor-grok-4.6-high' ;;
  cursor:deep:cursor-grok-4.6-medium*)                             printf 'medium RECORD_ONLY' ;;
  # cursor:trivial:low→medium removed — trivial tier must not reach above its row
  # pi: flash → v4.1-flash → v4-pro
  pi:standard:openrouter/deepseek/deepseek-v4-flash)               printf 'v4-flash RECORD_ONLY' ;;
  pi:standard:openrouter/deepseek/deepseek-v4.1-flash)             printf 'v4.1-flash openrouter/deepseek/deepseek-v4-pro' ;;
  pi:deep:openrouter/deepseek/deepseek-v4.1-flash)                 printf 'v4.1-flash RECORD_ONLY' ;;
  # pi:trivial:flash→v4.1-flash removed — trivial tier must not reach above its row
  esac
}

# _prior_failed_model <branch> <crew_dir> — prints the model of the most recent
# failed worker on this branch. Prints nothing if no prior failed exists.
_prior_failed_model() {
  local branch="$1" dir="$2" events
  events="$dir/events.jsonl"
  [ -f "$events" ] || return 0
  jq -r --arg b "$branch" '
    [., inputs]
    | . as $all
    | ([$all[] | select(.kind == "status" and
        ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b
        and .body.state == "failed") | .ts]) as $failed_ts
    | if ($failed_ts | length) == 0 then empty
      else $all
      | map(select(.kind == "dispatch" and .branch == $b
          and .ts < ($failed_ts | max)))
      | sort_by(-.ts)
      | .[0].model // empty
      end
  ' "$events" 2>/dev/null || true
}

# _prior_failed_escalation_available <branch> <crew_dir> — returns 0 if:
# 1. A worker on this branch posted status "failed" AND that worker's session
#    has a matching dispatch event on the same branch (anti-spoofing), AND
# 2. No dispatch or resume event on this branch already carries escalated_from.
_prior_failed_escalation_available() {
  local branch="$1" dir="$2" events
  events="$dir/events.jsonl"
  [ -f "$events" ] || return 1
  # Check 1: any failed status event on this branch whose session has a
  # matching dispatch row? This prevents fabricated failed status events —
  # the worker must have been actually dispatched on this branch.
  jq -e --arg b "$branch" '
    [., inputs] | . as $all
    | ([$all[] | select(.kind == "dispatch" and .branch == $b) | .session]) as $sessions
    | [.[] | select(.kind == "status" and .from != null
        and ((.from | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b)
        and .body.state == "failed")]
    | [.[] | . as $item | ($sessions | index($item.from | sub("^worker:[^#]*#"; ""))) as $idx | select($idx != null)]
    | length > 0
  ' "$events" >/dev/null 2>&1 || return 1
  # Check 2: no prior dispatch or resume event on this branch already carries escalated_from?
  jq -e --arg b "$branch" '
    [., inputs]
    | map(select((.kind == "dispatch" or .kind == "resume") and .branch == $b and (.escalated_from // "" | length > 0)))
    | length == 0
  ' "$events" >/dev/null 2>&1 || return 1
  return 0
}

# Pre-compute the branch and crew_dir for escalation checks (normally
# computed after this gate). At this point $* is the title.
if [ -n "$gh_issue" ]; then
  _escalation_slug="$(printf '%s' "$*" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g' | cut -c1-40 | sed -E 's/^-+//; s/-+$//')"
  _escalation_branch="feat/$gh_issue-$_escalation_slug"
  _escalation_crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
else
  _escalation_branch=""
  _escalation_crew_dir=""
fi

# Tier↔model gate (#89). Enforces tier-appropriateness on top of
# the dispatchability gate above — see dispatch-orchestration.md
# "Tier map". DISPATCH_SKIP_MODEL_CHECK does not cover this gate (it is
# about shape/cache staleness, not tier); --ignore-map does.
if [ -z "$ignore_map" ]; then
  tier_ok=1
  tier_expected=""
  case "$agent" in
  claude)
    case "$tier" in
    deep)
      tier_expected="opus, claude-opus-*, sonnet, claude-sonnet-*, fable, or claude-fable-*"
      [[ $model =~ ^(opus|claude-opus-.*|sonnet|claude-sonnet-.*|fable|claude-fable-.*)$ ]] || tier_ok=0
      ;;
    standard)
      tier_expected="sonnet or claude-sonnet-*"
      [[ $model =~ ^(sonnet|claude-sonnet-.*)$ ]] || tier_ok=0
      ;;
    trivial)
      tier_expected="sonnet, claude-sonnet-*, haiku, or claude-haiku-*"
      [[ $model =~ ^(sonnet|claude-sonnet-.*|haiku|claude-haiku-.*)$ ]] || tier_ok=0
      ;;
    # An unhandled tier can't happen today (the top-of-file case at line 34
    # already restricts $tier to trivial|standard|deep before this code
    # runs) — but fail CLOSED rather than silently accepting every model,
    # in case a future tier is ever added there without a matching update
    # here.
    *) tier_ok=0 ;;
    esac
    ;;
  codex)
    re_codex_legacy='^(gpt-5\.5|gpt-5\.4|gpt-5\.4-mini)$'
    case "$tier" in
    deep)
      tier_expected="gpt-5.6-sol, gpt-5.6-terra, or a legacy generation (gpt-5.5, gpt-5.4, gpt-5.4-mini)"
      [[ $model =~ ^(gpt-5\.6-sol|gpt-5\.6-terra)$ ]] || [[ $model =~ $re_codex_legacy ]] || tier_ok=0
      ;;
    standard)
      tier_expected="gpt-5.6-terra, gpt-5.6-luna, or a legacy generation (gpt-5.5, gpt-5.4, gpt-5.4-mini)"
      [[ $model =~ ^(gpt-5\.6-terra|gpt-5\.6-luna)$ ]] || [[ $model =~ $re_codex_legacy ]] || tier_ok=0
      ;;
    trivial)
      tier_expected="gpt-5.6-luna or a legacy generation (gpt-5.5, gpt-5.4, gpt-5.4-mini)"
      [[ $model =~ ^gpt-5\.6-luna$ ]] || [[ $model =~ $re_codex_legacy ]] || tier_ok=0
      ;;
    *) tier_ok=0 ;;
    esac
    ;;
  cursor)
    # Self-contained for $tiermap_cursor_base/$tiermap_cursor_params (unset
    # on the DISPATCH_SKIP_MODEL_CHECK skip path above) — but
    # $re_effort_tail is safe to reuse as-is: it's assigned once, before
    # that skip branch splits, so it's set on both paths.
    tiermap_re_cursor='^([a-z0-9][a-z0-9.-]*)(\[[a-z]+=[a-z0-9.-]+(,[a-z]+=[a-z0-9.-]+)*\])?$'
    tiermap_cursor_base="" tiermap_cursor_params=""
    if [[ $model =~ $tiermap_re_cursor ]]; then
      tiermap_cursor_base="${BASH_REMATCH[1]}"
      tiermap_cursor_params="${BASH_REMATCH[2]}"
    fi
    # composer-2.5[-fast] has no effort variants (dispatch-orchestration.md),
    # so a bracket block on it is never legitimate — require the whole
    # model string to match, not just the base.
    tiermap_is_composer=0
    [[ $model =~ ^composer-2\.5(-fast)?$ ]] && tiermap_is_composer=1
    tiermap_is_alt_effort=0
    if [[ $tiermap_cursor_base =~ ^(claude|gpt)- ]] && { [[ $tiermap_cursor_base =~ $re_effort_tail ]] || [[ $tiermap_cursor_params =~ (\[|,)effort= ]]; }; then
      tiermap_is_alt_effort=1
    fi
    # The gate enforces EFFORT appropriateness, so each row accepts its rung
    # with or without `-fast`: the suffix is a price/speed choice (2x the token
    # rate), not a different rung. The Tier map names the non-fast slug as the
    # default and `-fast` is the deliberate "I want this now" override.
    case "$tier" in
    deep)
      tier_expected="kimi-k3-high, cursor-grok-4.6-medium[-fast], cursor-grok-4.6-high[-fast], composer-2.5[-fast], or an effort-suffixed/bracketed claude-*/gpt-* id"
      [[ $model =~ ^(kimi-k3-high|cursor-grok-4\.6-(medium|high)(-fast)?)$ ]] ||
        [ "$tiermap_is_composer" = 1 ] || [ "$tiermap_is_alt_effort" = 1 ] || tier_ok=0
      ;;
    standard)
      tier_expected="cursor-grok-4.6-medium[-fast], cursor-grok-4.6-low[-fast], or composer-2.5[-fast]"
      [[ $model =~ ^cursor-grok-4\.6-(medium|low)(-fast)?$ ]] ||
        [ "$tiermap_is_composer" = 1 ] || tier_ok=0
      ;;
    trivial)
      tier_expected="cursor-grok-4.6-low[-fast] or composer-2.5[-fast]"
      [[ $model =~ ^cursor-grok-4\.6-low(-fast)?$ ]] || [ "$tiermap_is_composer" = 1 ] || tier_ok=0
      ;;
    *) tier_ok=0 ;;
    esac
    ;;
  pi)
    case "$tier" in
    deep)
      tier_expected="openrouter/deepseek/deepseek-v4-pro or openrouter/deepseek/deepseek-v4.1-flash"
      [[ $model =~ ^openrouter/deepseek/deepseek-v4(-pro|\.1-flash)$ ]] || tier_ok=0
      ;;
    standard)
      tier_expected="openrouter/deepseek/deepseek-v4.1-flash or openrouter/deepseek/deepseek-v4-flash"
      [[ $model =~ ^openrouter/deepseek/deepseek-v4(\.1)?-flash$ ]] || tier_ok=0
      ;;
    trivial)
      tier_expected="openrouter/deepseek/deepseek-v4-flash"
      [[ $model =~ ^openrouter/deepseek/deepseek-v4-flash$ ]] || tier_ok=0
      ;;
    *) tier_ok=0 ;;
    esac
    ;;
  esac
  if [ "$tier_ok" = 0 ]; then
    # Escalation: if the model is not in the tier's row but IS the one-rung-up
    # target from the failed model, AND a prior worker ended failed, allow it.
    if [ -n "${_escalation_branch:-}" ]; then
      failed_model="$(_prior_failed_model "$_escalation_branch" "$_escalation_crew_dir")"
      if [ -n "$failed_model" ]; then
        escalation_info="$(_escalation_target "$agent" "$tier" "$failed_model")"
        if [ -n "$escalation_info" ]; then
          escalation_baseline="${escalation_info%% *}"
          escalation_target="${escalation_info#* }"
          if [ "$escalation_target" != "RECORD_ONLY" ]; then
            if [ "$model" = "$escalation_target" ] || [[ $model =~ ^${escalation_target//./\\.} ]]; then
              if _prior_failed_escalation_available "$_escalation_branch" "$_escalation_crew_dir"; then
                tier_ok=1
                escalated_from="$escalation_baseline"
              fi
            fi
          fi
        fi
      fi
    fi
    if [ "$tier_ok" = 0 ]; then
      echo "dispatch: model '$model' is not $tier's row for --agent $agent — expected $tier_expected, or pass --ignore-map (the human's model decision). See dispatch-orchestration.md \"Tier map\"." >&2
      exit 1
    fi
  fi
fi

# Record-only escalation: model already in tier's row but one rung up from failed.
# Stamp WORKER_TASK.md only (dispatch event skips it — the gate already passed).
if [ "${escalated_from:-}" = "" ] && [ -z "$ignore_map" ] && [ "$tier_ok" = 1 ] && [ -n "${_escalation_branch:-}" ]; then
  failed_model="$(_prior_failed_model "$_escalation_branch" "$_escalation_crew_dir")"
  if [ -n "$failed_model" ]; then
    case "$agent:$tier:$failed_model:$model" in
    claude:trivial:haiku:sonnet|claude:trivial:haiku:claude-sonnet-*|\
    claude:trivial:claude-haiku-*:sonnet|claude:trivial:claude-haiku-*:claude-sonnet-*)
      escalated_from="haiku (record only)" ;;
    claude:deep:sonnet:opus|claude:deep:sonnet:claude-opus-*|\
    claude:deep:claude-sonnet-*:opus|claude:deep:claude-sonnet-*:claude-opus-*)
      escalated_from="sonnet (record only)" ;;
    claude:deep:opus:fable|claude:deep:opus:claude-fable-*|\
    claude:deep:claude-opus-*:fable|claude:deep:claude-opus-*:claude-fable-*)
      escalated_from="opus (record only)" ;;
    codex:standard:gpt-5.6-luna:gpt-5.6-terra)
      escalated_from="luna (record only)" ;;
    codex:deep:gpt-5.6-terra:gpt-5.6-sol)
      escalated_from="terra (record only)" ;;
    cursor:standard:cursor-grok-4.6-low:cursor-grok-4.6-medium*|\
    cursor:standard:cursor-grok-4.6-low*:cursor-grok-4.6-medium*)
      escalated_from="low (record only)" ;;
    cursor:deep:cursor-grok-4.6-medium:cursor-grok-4.6-high*|\
    cursor:deep:cursor-grok-4.6-medium*:cursor-grok-4.6-high*)
      escalated_from="medium (record only)" ;;
    pi:standard:openrouter/deepseek/deepseek-v4-flash:openrouter/deepseek/deepseek-v4.1-flash)
      escalated_from="v4-flash (record only)" ;;
    pi:deep:openrouter/deepseek/deepseek-v4.1-flash:openrouter/deepseek/deepseek-v4-pro)
      escalated_from="v4.1-flash (record only)" ;;
    esac
  fi
fi

# claude's and pi's --effort top out at max; rejecting `ultra` here fails before the
# worktree and pane exist, instead of at worker launch.
if { [ "$agent" = claude ] || [ "$agent" = pi ]; } && [ "$effort" = ultra ]; then
  echo "dispatch: --effort ultra is codex-only; $agent tops out at max" >&2
  exit 1
fi
if [ "$agent" != claude ] && [ -n "$mcp_profile" ]; then
  echo "dispatch: --mcp is claude-only; codex/cursor/pi base MCP comes from their own config" >&2
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

# Codex absolute-limit gate (#201): a codex response can be authoritative-
# exhausted while every percent window is below 95% (or no window exists) —
# the backend denies ordinary usage, names a rate-limit-reached reason, marks
# spend control reached, or reports a zeroed individual spend limit. Refuse
# codex on any of them, same severity and escape as the >=95% stop. Missing
# or stale data (older cache without limit_reached) fails open, like the rest
# of the budget gate.
if [ -z "$ignore_budget" ] && [ "$agent" = codex ] && [ -f "$budget_file" ]; then
  codex_abs=$(jq -r --argjson now "$(date +%s)" '
    if (.fetched_epoch + 7200) < $now then empty
    elif .engines.codex == null then empty
    else (.engines.codex.limit_reached // {}) as $l
      | if $l.rate_limit_reached_type != null then $l.rate_limit_reached_type
        elif $l.individual_remaining_percent == 0 then "spend control: 0% remaining"
        elif $l.spend_control_reached == true then "spend control reached"
        elif $l.ordinary_usage_allowed == false then "ordinary use not allowed"
        else empty end
    end' "$budget_file" 2>/dev/null || true)
  if [ -n "$codex_abs" ]; then
    echo "dispatch: codex quota exhausted (absolute limit: $codex_abs) — pick another engine, wait for the reset, or pass --ignore-budget" >&2
    exit 1
  fi
fi

# Role grid. Resolve the topology before scaffolding so a bad spec can't leave a
# half-built grid. `--roles` is explicit and wins; `--grid` derives the topology
# from the tier. Each spec is `name`, `name=<model>`, or `name=<agent>:<model>`;
# a leading token from the fixed agent set is the agent, so any other text before
# a `:` (a pi `:thinking` suffix, say) stays part of the model id.
role_names=()
role_agents=()
role_models=()
role_efforts=()
if [ -n "$no_grid" ]; then
  if [ -n "$grid_flag" ] || [ -n "$grid_roles" ]; then
    echo "dispatch: --no-grid conflicts with --grid/--roles" >&2
    exit 1
  fi
  if [ "$agent" = pi ] && [ "$tier" != trivial ]; then
    echo "dispatch: --no-grid cannot be used with --agent pi on standard/deep — pi has no native subagents and needs the grid for fresh critic/reviewer contexts" >&2
    exit 1
  fi
fi
# pi keeps its existing standard+deep default (critics AND its review-gate
# reviewer, since pi has no native review batch). Every other engine now also
# defaults to a grid on deep, but only for the critic phases — claude/codex/
# cursor already run a full native review-gate batch, so the default carries
# no reviewer role for them (see WORKER_PROTOCOL.md "Grid mode"). Roles
# inherit the lead's own agent/model below when --roles doesn't say
# otherwise, so this never requires a second engine. A review-kind worker has
# no spec/plan phase, so it never gets a default grid; nor does a non-pi deep
# dispatch whose plan is already provided, since that leaves no critic role
# to default to.
grid_default_non_pi=""
if [ -z "$grid_roles" ] && [ -z "$grid_flag" ] && [ -z "$no_grid" ] && [ "$kind" != review ]; then
  if [ "$agent" = pi ] && [ "$tier" != trivial ]; then
    grid_flag=1
  elif [ "$tier" = deep ] && [ "$plan_val" != provided ]; then
    grid_flag=1
    grid_default_non_pi=1
  fi
fi
if [ -z "$grid_roles" ] && [ -n "$grid_flag" ]; then
  if [ -n "$grid_default_non_pi" ]; then
    grid_roles="spec-critic,plan-critic"
  else
    case "$tier" in
    trivial) grid_roles="" ;;
    standard) grid_roles="plan-critic,reviewer" ;;
    deep) grid_roles="spec-critic,plan-critic,reviewer" ;;
    esac
  fi
fi
if [ -n "$grid_roles" ]; then
  IFS=',' read -r -a role_specs <<<"$grid_roles"
  for spec in "${role_specs[@]}"; do
    [ -n "$spec" ] || {
      echo "dispatch: role list contains an empty entry" >&2
      exit 1
    }
    role_effort="$effort"
    role_spec="$spec"
    role_effort_explicit=""
    if [[ $role_spec == *"@"* ]]; then
      role_spec_prefix="${role_spec%@*}"
      role_effort="${role_spec##*@}"
      if [ -z "$role_spec_prefix" ] || [ -z "$role_effort" ] || [[ $role_spec_prefix == *"@"* ]]; then
        echo "dispatch: invalid role effort suffix in '$spec' (use role[=model|agent:model][@effort])" >&2
        exit 1
      fi
      role_effort_explicit=1
      valid_effort "$role_effort" || {
        echo "dispatch: invalid effort '$role_effort' for role '$role_spec_prefix' (expected low, medium, high, xhigh, max, or ultra)" >&2
        exit 1
      }
      role_spec="$role_spec_prefix"
    fi
    role="${role_spec%%=*}"
    rest=""
    [ "$role" != "$role_spec" ] && rest="${role_spec#*=}"
    case "$role" in
    '' | *[!A-Za-z0-9_-]*)
      echo "dispatch: invalid role '$role' (letters, digits, _ and - only)" >&2
      exit 1
      ;;
    esac
    for existing_role in "${role_names[@]}"; do
      [ "$existing_role" != "$role" ] || {
        echo "dispatch: duplicate role '$role'" >&2
        exit 1
      }
    done
    role_agent="$agent"
    role_model="$model"
    if [ -n "$rest" ]; then
      case "${rest%%:*}" in
      claude | codex | cursor | pi)
        role_agent="${rest%%:*}"
        role_model="${rest#*:}"
        [ -n "$role_model" ] || {
          echo "dispatch: role '$role' needs a model after '$role_agent:'" >&2
          exit 1
        }
        ;;
      *) role_model="$rest" ;;
      esac
    fi
    if ! valid_role_model "$role_agent" "$role_model"; then
      echo "dispatch: invalid model '$role_model' for role '$role'" >&2
      exit 1
    fi
    if [ "$role_agent" = cursor ] && [ -n "$role_effort_explicit" ]; then
      echo "dispatch: role '$role' uses --agent cursor, which has no --effort; encode intensity in the bracketed model (for example cursor-model[effort=high])" >&2
      exit 1
    fi
    if { [ "$role_agent" = claude ] || [ "$role_agent" = pi ]; } && [ "$role_effort" = ultra ]; then
      echo "dispatch: role '$role' uses --agent $role_agent, which does not support --effort ultra" >&2
      exit 1
    fi
    check_engine "$role_agent" "role '$role' uses --agent $role_agent"
    role_names+=("$role")
    role_agents+=("$role_agent")
    role_models+=("$role_model")
    role_efforts+=("$role_effort")
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

# All launch targets are resolved now. Validate the lead and every eager role
# before any pane is created; lazy roles validate their final override later.
pace_rule_target "$agent" "$model" "$effort"
if [ -z "$grid_lazy" ]; then
  for i in "${!role_names[@]}"; do
    pace_rule_target "${role_agents[$i]}" "${role_models[$i]}" "${role_efforts[$i]}"
  done
fi

required_protocol_files=(WORKER_PROTOCOL.md EVIDENCE_REVIEW.md)
[ -n "$roles_stamp" ] && required_protocol_files+=(GRID_PROTOCOL.md)
_require_protocol_files "$PROTOCOL_DIR" "${required_protocol_files[@]}"
_check_protocol_rev "$PROTOCOL_DIR" dispatch

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

# Pre-scaffold gate check for `dispatch resume`, which re-runs the gates that
# are properties of now — profile, model shape, effort ceiling, quota, rung —
# rather than re-deriving them in a second copy that would drift. Everything
# above this point is pure validation: `_ensure_dispatched_label` and
# `crew reap` are below, as is the first string of scaffolding, so exiting
# here has no side effects. Resume suppresses the tier↔model gate for a pair
# the first dispatch already accepted by passing the existing --ignore-map.
if [ -n "${DISPATCH_PRECHECK:-}" ]; then
  exit 0
fi

# Seed once per run, before any window is created: the lead and its role panes
# share this one seed. Lazy roles are seeded on demand by --spawn-role instead.
if [ "$agent" = pi ]; then
  seed_pi_agent_dir
elif [ -z "$grid_lazy" ]; then
  for role_agent in "${role_agents[@]}"; do
    if [ "$role_agent" = pi ]; then
      seed_pi_agent_dir
      break
    fi
  done
fi

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
    if git show-ref --verify --quiet "refs/heads/$branch"; then
      echo "dispatch: issue #$gh_issue is already claimed, but branch $branch exists — proceeding onto it as a resume." >&2
    else
      existing_branch="$(git for-each-ref --format='%(refname:short)' "refs/heads/feat/$gh_issue-*" 2>/dev/null | head -1)"
      if [ -n "$existing_branch" ] && [ "$existing_branch" != "$branch" ]; then
        echo "dispatch: issue #$gh_issue is already claimed — the title resolves to branch '$branch', but '$existing_branch' already exists (title mismatch?). Use the exact original title, or pass a different issue number." >&2
      else
        echo "dispatch: issue #$gh_issue is already claimed (carries the 'dispatched' label) — another crew is on it. If that crew is gone, remove the label by hand and retry." >&2
      fi
      exit 1
    fi
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
  line=$(jq -nc --arg crew "$crew_id" --arg issue "$gh_issue" --arg branch "$branch" \
    '{ts:(now*1000|floor), crew_id:$crew, kind:"claim-issue", issue:$issue, branch:$branch}')
  _bus_append "$crew_dir/events.jsonl" "$line"
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
    line=$(jq -nc --arg crew "$crew_id" --arg issue "$num" --arg branch "$branch" \
      '{ts:(now*1000|floor), crew_id:$crew, kind:"claim-issue", issue:$issue, branch:$branch}')
    _bus_append "$crew_dir/events.jsonl" "$line"
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
ident_locked=""
ident_lock="$crew_dir/identity.lock"
trap 'rm -f "$dispatch_lock" "${claude_json_lock:-}"; [ -z "$ident_locked" ] || rmdir "$ident_lock" 2>/dev/null' EXIT INT TERM HUP

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
    line=$(jq -nc --arg crew "$crew_id" --arg branch "$branch" --arg state "$state" --argjson occ "$occ" \
      '{ts:(now*1000|floor), crew_id:$crew, kind:"reclaim", branch:$branch, state:$state,
          windows:($occ|map(.window))}')
    _bus_append "$crew_dir/events.jsonl" "$line"
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

# Session id is issued here and carried in the environment by all four engine
# launch paths. epoch+pid prevents two same-second dispatches on one branch from
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
line=$(jq -nc --arg crew "$crew_id" --arg from "$worker_id" \
  '{ts:(now*1000|floor), crew_id:$crew, from:$from, kind:"claim"}')
_bus_append "$crew_dir/events.jsonl" "$line"

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
# machine's own trusted origin, not a fork. A repo with no .envrc never used
# direnv and has no devshell to lose, so there's nothing to allow — skip it.
# Aborts the dispatch only when an .envrc is present and direnv actually
# fails to allow it, so a devshell-less worker never gets scaffolded to fail
# its gate in a confusing way much later.
if [ -n "$pr_number" ]; then
  echo "dispatch: --pr worktree — not auto-approving direnv; review $wt_path/.envrc and run \`direnv allow $wt_path\` by hand once you trust it" >&2
elif [ ! -e "$wt_path/.envrc" ]; then
  : # no .envrc — repo doesn't use direnv, nothing to allow
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

# FleetView-style codename+color: the branch's recorded one, else a slot no live
# worker holds. Picked and recorded under one lock so two racing dispatches
# cannot read the same free slot; the recorded name is what roster, tmux and
# `--name` all read back.
for _ in $(seq 1 100); do
  if mkdir "$ident_lock" 2>/dev/null; then
    ident_locked=1
    break
  fi
  sleep 0.1
done
[ -n "$ident_locked" ] || echo "dispatch: identity lock busy after 10s — picking a codename unlocked" >&2
ident=$(crew identity "$branch" "$crew_id")
agent_name=$(printf '%s' "$ident" | jq -r .name)
agent_color=$(printf '%s' "$ident" | jq -r .tmux)

# Log the dispatch decision to the crew bus for later `crew report`.
dispatch_shape="${DISPATCH_SHAPE:-}"
# task_kind rides along because only `dispatch` knows it: a `--review` worker is
# told not to push or open a PR, so a run with no PR is its success case, not a
# failure. Without this the ratings store cannot tell the two apart.
# escalated_from: stamped on the event only for genuine escalations (not record-only).
escalated_from_event=""
if [ -n "${escalated_from:-}" ] && [[ ! $escalated_from =~ "record only" ]]; then
  escalated_from_event="$escalated_from"
fi
line=$(jq -nc --arg crew "$crew_id" --arg branch "$branch" --arg session "$session" \
  --arg engine "$agent" --arg model "$model" --arg tier "$tier" --arg effort "$effort" \
  --arg shape "$dispatch_shape" --arg title "$title" --arg task_kind "$kind" \
  --arg plan "$plan_val" --argjson resume "$([ "$switch_mode" = resume ] && echo true || echo false)" \
  --argjson ident "$ident" \
  --arg escalated_from "$escalated_from_event" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"dispatch", branch:$branch, session:$session, engine:$engine, model:$model, tier:$tier, effort:$effort, shape:$shape, task_kind:$task_kind, title:$title, plan:$plan, resume:$resume} + $ident
   + if $escalated_from != "" then {escalated_from:$escalated_from} else {} end')
_bus_append "$crew_dir/events.jsonl" "$line"
if [ -n "$ident_locked" ]; then
  rmdir "$ident_lock" 2>/dev/null || true
  ident_locked=""
fi

# GitHub-issue dispatch only: post a context comment for at-a-glance history.
# Linear dispatches and `--pr` review dispatches set neither $gh_issue nor
# $num, so this is a no-op for them.
comment_issue="${gh_issue:-${num:-}}"
if [ -n "$comment_issue" ]; then
  _post_dispatch_comment "$comment_issue" "$agent_name" "$agent" "$model" "$tier" "$effort" \
    "$branch" "$wt_path" "$session" "$worker_id" "$crew_id" \
    "$([ "$switch_mode" = resume ] && echo true || echo "")"
fi

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
  printf 'tier: %s\nkind: %s\ndraft: %s\nengine: %s\nmodel: %s\neffort: %s\n' \
    "$tier" "$kind" "$draft" "$agent" "$model" "$effort"
  [ -n "${escalated_from:-}" ] && printf 'escalated_from: %s\n' "$escalated_from"
  printf 'mcp: %s\nplan: %s\ntitle: %s\n%s\ndispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\nworker_id: %s\nprotocol_dir: %s\n' \
    "$mcp_profile" "$plan_val" "$title" "$closes" "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name" "$worker_id" "$PROTOCOL_DIR"
  if [ -n "$pr_number" ]; then
    printf 'base: %s\n' "$base_ref"
  fi
  if [ "$switch_mode" = resume ]; then
    printf 'resume: true\n'
  fi
  # The role grid the lead should delegate to (absent = single-agent pipeline).
  [ -n "$roles_stamp" ] && printf 'roles: %s\n' "$roles_stamp"
  # A lazy grid creates no role panes up front; the lead spawns each at its seam.
  [ -n "$grid_lazy" ] && printf 'lazy: 1\n'
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

# Record resolved role specs so a lazy grid's lead can spawn each role on demand
# (`dispatch --spawn-role`), and so a role can be re-created after death.
if [ "${#role_names[@]}" -gt 0 ]; then
  roles_dir="$crew_dir/artifacts/$branch"
  mkdir -p "$roles_dir"
  for i in "${!role_names[@]}"; do
    jq -n --arg n "${role_names[$i]}" --arg a "${role_agents[$i]}" --arg m "${role_models[$i]}" --arg e "${role_efforts[$i]}" '{name:$n,agent:$a,model:$m,effort:$e}'
  done | jq -s 'map({key:.name,value:{agent:.agent,model:.model,effort:.effort}})|from_entries' > "$roles_dir/roles.json"
fi

# A detached new-window can inherit tmux's fallback size instead of the client
# that invoked dispatch. codex's startup banner boxes stay pinned at their
# initial width and never redraw on a later resize; claude and cursor both
# redraw cleanly, so `default-size` (lazytmux) already covers them. This
# fixes codex, and gives every engine the invoking client's own geometry,
# which only dispatch knows.
client_target=()
[ -n "${TMUX_PANE:-}" ] && client_target=(-t "$TMUX_PANE")
client_size="$(tmux display-message -p "${client_target[@]}" '#{client_width} #{client_height} #{status}' 2>/dev/null || true)"
window_size_mode="$(tmux show-option -qv "${client_target[@]}" window-size 2>/dev/null || true)"
if [ -z "$window_size_mode" ]; then
  window_size_mode="$(tmux show-option -gqv window-size 2>/dev/null || true)"
fi
client_width=""
client_height=""
status_rows=""
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
read -r win pane < <(tmux new-window -d -c "$wt_path" -n "$sanitized" -e "CREW_WORKER_ID=$worker_id" -e "CREW_ID=$crew_id" -P -F '#{window_id} #{pane_id}')
if [ -n "$client_width" ]; then
  tmux resize-window -t "$win" -x "$client_width" -y "$client_height"
  if [ -n "$window_size_mode" ] && [ "$window_size_mode" != manual ]; then
    tmux set-option -t "$win" window-size "$window_size_mode"
  fi
fi

# Printed so the dispatcher can address this session in the gap before the worker
# boots — its startup drain is unbounded, so a scoping note posted now still lands.
echo "worker_id: $worker_id"

# Identity surfaces: codename on the pane border + the CC prompt box (--name).
# lazytmux owns the tab text; @crew_* tint the status-bar tab.
tmux set-window-option -t "$win" @crew_name "$agent_name"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
# A grid lead's window border carries a "lead" marker; role panes label
# themselves at pane level, so this touches only the lead. @crew_name stays
# the bare codename — it is the occupancy join key.
lead_marker=" "
if [ "${#role_names[@]}" -gt 0 ]; then
  lead_marker=" lead "
fi
tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold]${lead_marker}"

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
# and partial work (#73). Every launch branch below builds its prompt as a plain
# variable and quotes it with a POSIX single-quote escape (quoted_prompt), so
# apostrophes survive into the pane's shell intact. The pane's real shell is
# fish, whose single-quote parsing (unlike POSIX sh) still treats a backslash
# specially, so this doesn't hold for a backslash next to another backslash or
# an apostrophe.
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

# Grid mode: tell the lead it has role panes, and that delegation is
# pane-scoped — only a phase with a pane skips the in-process path. The
# native code-review gate always runs regardless (WORKER_PROTOCOL.md →
# "Grid mode").
grid_note=""
if [ -n "$roles_stamp" ]; then
  grid_note=" You lead a role grid: role panes ($roles_stamp) share this worktree and are parked on the crew bus. Follow WORKER_PROTOCOL.md 'Grid mode' — delegate to the bus only the phases that have a pane; your engine-native code-review gate still runs as usual (a reviewer pane is additive, except on pi where it is the gate)."
fi

# WORKER_PROTOCOL.md reaches pi/claude leads as a system prompt, so its
# "sibling" protocol files have no referent unless the directory is named.
protocol_note=" Protocol files (EVIDENCE_REVIEW.md, GRID_PROTOCOL.md, ...) live in $PROTOCOL_DIR — also stamped as protocol_dir: in WORKER_TASK.md."

if [ "$agent" = codex ]; then
  # service_tier pinned: the interactive /fast toggle persists locally and would
  # otherwise leak into unattended workers, burning ChatGPT credits at 2.5x for
  # latency nobody is watching.
  # agents.*: enable native delegation, cap concurrency at 3 (parity with rule 1),
  # and pin subagent effort one rung down. Never pass ultra as subagent effort.
  prompt="Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}${grid_note}${protocol_note}"
  shell_quote quoted_prompt "$prompt"
  tmux send-keys -t "$pane" \
    "codex --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default -c agents.enabled=true -c agents.max_concurrent_threads_per_session=3 -c agents.default_subagent_reasoning_effort=$codex_subagent_effort --dangerously-bypass-approvals-and-sandbox $quoted_prompt" Enter
elif [ "$agent" = cursor ]; then
  # cursor-agent has no reasoning-effort flag — effort is encoded in the model
  # id ($model, e.g. claude-opus-5-high); composer-2.5 has no effort variants.
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
  prompt="Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}${grid_note}${protocol_note}"
  shell_quote quoted_prompt "$prompt"
  tmux send-keys -t "$pane" \
    "CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' $quoted_prompt" Enter
elif [ "$agent" = pi ]; then
  # pi's interactive TUI keeps pane output live. It accepts a file path as a
  # real appended system prompt; --no-approve ignores project-local resources,
  # so the worktree's own skills go over via --skill (pi_skill_args).
  printf -v quoted_dir '%q' "$pi_agent_dir"
  prompt="Read WORKER_TASK.md and run it end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}${grid_note}${protocol_note}"
  shell_quote quoted_prompt "$prompt"
  tmux send-keys -t "$pane" \
    "PI_CODING_AGENT_DIR=$quoted_dir pi --name $agent_name --model $model --thinking $effort --append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.md --no-approve$(pi_skill_args "$wt_path") $quoted_prompt" Enter
else
  prompt="Read WORKER_TASK.md and run it end-to-end.${push_mandate}${plan_note}${resume_note}${grid_note}${protocol_note}"
  shell_quote quoted_prompt "$prompt"
  tmux send-keys -t "$pane" \
    "claude --name $agent_name --model $model --effort $effort $mcp_flag $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto $quoted_prompt" Enter
fi

# Role grid: split the task window into one pane per role. Each role pane parks
# on the bus until the lead assigns it work; GRID_PROTOCOL.md is its system
# prompt. A role may run a different engine from the lead (cross-engine review).
# Split AFTER the lead launch so the lead keeps the first pane. NOT stall-watched
# on purpose: a parked role produces no output, which the pane-output watchdog
# would misread as a wedge.
if [ "${#role_names[@]}" -gt 0 ] && [ -z "$grid_lazy" ]; then
  for i in "${!role_names[@]}"; do
    role="${role_names[$i]}"
    role_pane="$(split_role_pane "$win" "$wt_path" "$role" "$worker_id" "$crew_id")"
    launch_role "$role_pane" "$wt_path" "$role" "${role_agents[$i]}" "${role_models[$i]}" "${role_efforts[$i]}"
    watch_role "$role" "$role_pane"
  done
  layout_grid "$win"
fi

# Optional live status pane (--status): a bounded roster loop over the crew bus.
if [ -n "$grid_status" ] && [ "${#role_names[@]}" -gt 0 ]; then
  status_pane="$(split_role_pane "$win" "$wt_path" status "$worker_id" "$crew_id")"
  tmux send-keys -t "$status_pane" "while true; do clear; crew roster 2>/dev/null | jq -r '.[] | \"  \\(.state)  \\(.from)\"'; sleep 3; done" Enter
  layout_grid "$win"
fi

# Detached stall watchdog (#103): a wedged worker sits in `working` with no
# output and never ends, so neither the bus nor the SessionEnd `exited` backstop
# notices. Pane output is only a valid liveness signal for an engine that streams
# — every engine launched above must, which is why all four run their own TUI
# rather than a buffered headless mode. This watches the pane's output and, if it
# goes silent through the startup window, posts `failed` so the dispatcher's
# `crew watch` wakes to recover. Engine-agnostic. nohup detaches it
# so it outlives this short-lived dispatch process; it self-exits on progress, a
# terminal state, or a vanished pane.
CREW_ID="$crew_id" nohup crew stall-watch "$worker_id" --pane "$pane" --engine "$agent" >/dev/null 2>&1 &
