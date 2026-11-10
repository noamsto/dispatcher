# shellcheck shell=bash
# dispatch-resume — relaunch the worker whose worktree you are standing in,
# continuing its own engine session. Reached as `dispatch resume`, which execs
# this binary (dispatch.sh intercepts the subcommand).
#
# Its own file rather than a mode inside dispatch.sh: resume skips the
# issue claim, branch creation, task-document rewrite and new-window paths
# entirely, and re-runs only the gates it names. The shebang and
# `set -euo pipefail` are prepended by writeShellApplication.

# Its own binary on PATH, so dispatch.sh's guard never sees a direct call.
if [ -n "${CREW_OVERSEER:-}" ]; then
  echo "dispatch resume: the overseer never dispatches; ask that repo's dispatcher with crew overseer msg" >&2
  exit 1
fi

usage() {
  echo "usage: dispatch resume [<target>] [flags] [extra prompt...] — see: dispatch resume --help" >&2
}

# `dispatch resume --help`, in the shape `dispatch --help` shares (#817). One
# line per flag: tests/dispatch-resume.bats derives the flag list from the
# `^  --[a-z-]+\)` arms of the parse loop below and requires each to own a line.
_resume_help() {
  cat <<'HELP'
usage: dispatch resume [<target>] [--agent E] [--model M] [--effort E]
                     [--mcp P] [--chrome] [--fresh] [--print] [--rearm-roles]
                     [--ignore-budget] [--ignore-map] [extra prompt...]

Relaunch the worker whose worktree you are standing in, continuing its own
engine session, and re-run only the gates this command names.

  <target>            Where the work is: the current directory (the default),
                      '#N' or N, a Linear id, a branch, a codename, or
                      worker:<branch>#<session>
  --agent E           Engine to relaunch on: claude | codex | cursor | pi
  --model M           Model for that engine (default: the task doc's); @cheap, @standard
                      or @premium resolves to that class's default, as in dispatch
  --effort E          low | medium | high | xhigh | max | ultra (default: the task doc's)
  --mcp LIST          MCP for the lead: analytics (claude) or mcpServers names,
                      comma-separated (default: the task doc's mcp: line)
  --chrome            Relaunch the claude lead with Claude in Chrome (dispatcher-only;
                      never carried from an earlier run)
  --fresh             Start a new engine session instead of continuing the old one
  --print             Print the launch command instead of running it
  --rearm-roles       Re-arm the role panes of this worker's restored window and
                      launch nothing (no launch flag or extra prompt;
                      --ignore-budget still applies)
  --ignore-budget     Spend past the quota stop (a human's spend decision)
  --ignore-map        Skip the tier-map model refusals (a human's model call)
  [extra prompt...]   Words appended to the carried task body for this run

  cd .dispatch-wt/feat-42-fix-the-roster-column && dispatch resume
  dispatch resume '#42' --agent pi --model lemonade/Qwen3.8-Flash --effort high
HELP
}

# Only a LEADING --help is the help screen, the #349 first-argument rule that
# keeps `resume` in a title from selecting the subcommand. It sits above every
# read of a worktree, a task doc or the bus, so help works from anywhere.
case "${1:-}" in
--help | -h)
  _resume_help
  exit 0
  ;;
esac

# Unconditional, unlike the advisory hint lib: without it resume must abort,
# never fall back to discovery in the worker's worktree (#539).
wt_git_lib="${WORKTREE_GIT_LIB:-@worktreeGitLib@}"
# shellcheck source=/dev/null
. "$wt_git_lib"

# The --add-dir grant validator, shared with dispatch and permission-check.
grant_check_lib="${GRANT_CHECK_LIB:-@grantCheckLib@}"
# shellcheck source=/dev/null
. "$grant_check_lib"

# Helpers every launcher shares (launcher-common.sh). A run from a checkout, or a
# copy of this script, finds it through LAUNCHER_COMMON_LIB or beside the script.
launcher_common_lib="${LAUNCHER_COMMON_LIB:-@launcherCommonLib@}"
# replaceStrings rewrites the token on both sides of an equality check, so a
# string compare against the placeholder always matches after bake.
if [ ! -f "$launcher_common_lib" ]; then
  launcher_common_lib="$(dirname -- "${BASH_SOURCE[0]}")/launcher-common.sh"
fi
# shellcheck source=/dev/null
. "$launcher_common_lib"

# Which side of a --host dispatch this process is on (a --host launch delivered
# DISPATCH_TMUX_SESSION), derived once through the shared library as dispatch.sh does.
wire_side="$(_wire_side)"

# Baked values the shared helpers read, kept in this file because
# launcher-common.sh carries no build-time token: the settings and model
# resolvers (each env-overridable at call time) and the protocol revision this build
# stamped.
# shellcheck disable=SC2034
dispatch_config_default="@dispatchConfig@"
# shellcheck disable=SC2034
model_resolve_bin="${MODEL_RESOLVE_BIN:-@modelResolve@}"
# shellcheck disable=SC2034
stamped_rev="@protocolRev@"

# Worker-only plugin disablement is shared with dispatch.  It is a separate
# --settings layer; the Nix wrapper merges it into its overlay (Claude itself
# keeps only the last --settings).
claude_worker_settings_lib="${CLAUDE_WORKER_SETTINGS_LIB:-@claudeWorkerSettingsLib@}"
# replaceStrings rewrites the token on both sides of an equality check, so a
# string compare against the placeholder always matches after bake.
if [ ! -f "$claude_worker_settings_lib" ]; then
  claude_worker_settings_lib="$(dirname -- "${BASH_SOURCE[0]}")/claude-worker-settings.sh"
fi
# shellcheck source=/dev/null
. "$claude_worker_settings_lib"

# The agent-browser config every worker launch pins, so a worker never reads one
# from the repo under work or the owner's home.
agent_browser_worker_config="${AGENT_BROWSER_WORKER_CONFIG:-@agentBrowserWorkerConfig@}"
# replaceStrings rewrites the token on both sides of an equality check, so a
# string compare against the placeholder always matches after bake.
if [ ! -f "$agent_browser_worker_config" ]; then
  agent_browser_worker_config="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)/agent-browser-worker.json"
fi

# #936: @launcherRuntimePath@, substituted by flake.nix's launcherPath, is this
# script's own pinned tool PATH — the store dirs its preamble prepends.
# engine_path (launcher-common.sh) reads it and prints the caller's PATH with exactly those dirs
# dropped, every other entry in order; the window creation below runs the tmux
# client under it, because tmux builds a new window's PATH from the client that
# opens it. A raw run from a checkout leaves the placeholder literal and leaves
# PATH untouched.
_launcher_runtime_path='@launcherRuntimePath@'
# _resume_remote <branch> <host> <crew> — a remote:true row's worker lives on
# <host>: its own dispatch resume runs there, against its own bus and tmux,
# with every resume argument but the target. The host name keys settings,
# which alone give the ssh target.
_resume_remote() {
  local rc=0 ssh session
  local -a rest=("${resume_args[@]:0:target_at}" "${resume_args[@]:target_at+1}")
  [ -z "$chrome" ] || {
    echo "dispatch resume: --chrome is not carried to a remote resume — $1 runs on $2; re-dispatch it with dispatch --host $2 --chrome" >&2
    exit 1
  }
  session="$(_remote_target_session "$3")" || exit 1
  crew remote-run --host "$2" --env "CREW_ID=$3" --env "DISPATCH_TMUX_SESSION=$session" -- \
    dispatch resume "$1" ${rest[@]+"${rest[@]}"} || rc=$?
  [ "$rc" -eq 0 ] || exit "$rc"
  ssh="$("${DISPATCH_CONFIG_BIN:-$dispatch_config_default}" | jq -r --arg h "$2" '.hosts[$h].ssh // empty')"
  _remote_window "$ssh" "$3" "$(_remote_worker_session "$2" "$3" "$1" "$session")"
  exit 0
}

# _resolve_target_worktree <target> — resolve a target to its worktree
# and cd there, so the cwd-bound path below runs unchanged (its record-mismatch
# refusals included). The branch comes from the dispatcher's own bus rows
# (`crew resolve-target`) and the worktree from the dispatcher-written record
# whose crew dir and branch both match — never from git state in a worktree or
# the worker's env.
_resolve_target_worktree() {
  local target="$1" row err rc=0 t_branch t_crew host here common cdir rec rec_wt where_out alive stamped p_path p_cmd p_role p_branch
  local -a hits=()
  git rev-parse --git-common-dir >/dev/null 2>&1 || {
    echo "dispatch resume: '$target' needs the repo's crew bus — run from inside the repository (any worktree)" >&2
    exit 1
  }
  # #633: a worker's own .git must not pick the row, and so the host.
  _wt_trusted_cwd "$(git rev-parse --path-format=absolute --git-common-dir)" || exit 1
  err="$(mktemp)"
  row="$(crew resolve-target "$target" 2>"$err")" || rc=$?
  if [ "$rc" -ne 0 ]; then
    row="$(cat "$err")"
    rm -f "$err"
    echo "dispatch resume: ${row#crew: resolve-target: }" >&2
    exit 1
  fi
  rm -f "$err"
  # cut, not read: tab is IFS whitespace, so an empty host or codename would
  # collapse and shift the fields.
  t_branch="$(cut -f1 <<<"$row")"
  host="$(cut -f3 <<<"$row")"
  t_crew="$(cut -f4 <<<"$row")"
  if [ "$(cut -f5 <<<"$row")" = 1 ]; then
    _resume_remote "$t_branch" "$host" "$t_crew"
  fi
  here="${HOSTNAME:-$(uname -n)}"
  if [ -n "$host" ] && [ "$host" != "$here" ]; then
    echo "dispatch resume: $t_branch was dispatched on host '$host' (this is '$here') — its worktree and session transcript live there; run dispatch resume on $host" >&2
    exit 1
  fi
  alive=""
  if where_out="$(crew where "$t_branch" ${t_crew:+--crew "$t_crew"} 2>/dev/null)"; then
    alive=1
  fi
  common="$(git rev-parse --path-format=absolute --git-common-dir)"
  cdir="$(realpath -m -- "$common/crew")"
  for rec in "${XDG_DATA_HOME:-$HOME/.local/share}"/crew/worktrees/*; do
    [ -f "$rec" ] && [ ! -L "$rec" ] || continue
    mapfile -t _rl <"$rec"
    [ "${_rl[1]:-}" = "$cdir" ] && [ "${_rl[2]:-}" = "$t_branch" ] && [ -d "${_rl[0]:-}" ] || continue
    hits+=("${_rl[0]}")
  done
  if [ "${#hits[@]}" -eq 0 ]; then
    echo "dispatch resume: no dispatcher record of a worktree for $t_branch on this host — re-dispatch the task onto its branch; the worktree carries over" >&2
    exit 1
  fi
  if [ "${#hits[@]}" -gt 1 ]; then
    echo "dispatch resume: $t_branch has several recorded worktrees (${hits[*]}) — cd into the one to resume" >&2
    exit 1
  fi
  rec_wt="${hits[0]}"
  if [ -n "$alive" ]; then
    # tmux-remux restores this worker's own window, which `crew where` still
    # sees. Proceed only when that window vouches for itself: every pane stamped
    # with the branch sits at the recorded worktree, and no lead pane runs an
    # engine. No @crew_dir filter, so the same branch name in another repo's
    # window fails closed. Role panes may run engines: tmux-og relaunches them.
    stamped=""
    while IFS=$'\x1f' read -r _ _ p_path p_cmd _ p_role p_branch; do
      [ "$p_branch" = "$t_branch" ] || continue
      stamped=1
      if [ "$p_path" != "$rec_wt" ]; then
        stamped=""
        break
      fi
      if [ -z "$rearm_roles" ] && { [ -z "$p_role" ] || [ "$p_role" = lead ]; } && crew engine-cmd "$p_cmd" 2>/dev/null; then
        stamped=""
        break
      fi
    done <<PANES
$(tmux list-panes -a -F '#{window_id}'$'\x1f''#{pane_id}'$'\x1f''#{pane_current_path}'$'\x1f''#{pane_current_command}'$'\x1f''#{@crew_name}'$'\x1f''#{@crew_role}'$'\x1f''#{@crew_branch}' 2>/dev/null || true)
PANES
    if [ -z "$stamped" ]; then
      echo "dispatch resume: $t_branch is still alive — $where_out" >&2
      exit 1
    fi
  fi
  cd -- "$rec_wt" || exit 1
}

fresh=""
do_print=""
rearm_roles=""
ignore_budget=""
ignore_map=""
agent_flag=""
model_flag=""
effort_flag=""
mcp_flag_val=""
chrome=""
extra=""
target=""
positional=()
# A routed resume forwards these, minus the target at index target_at.
resume_args=("$@")
target_at=""

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
  --chrome)
    chrome=1
    shift
    ;;
  --fresh)
    fresh=1
    shift
    ;;
  --print)
    do_print=1
    shift
    ;;
  --rearm-roles)
    rearm_roles=1
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
    [ -n "$target_at" ] || target_at=$((${#resume_args[@]} - $#))
    positional+=("$1")
    shift
    ;;
  esac
done

# Remote mode: a dispatcher on another host asked for this worker's window in
# the named session, possibly shared with windows that are not this crew's. The
# value becomes a tmux target, so its shape is fixed.
if [ "$wire_side" = remote ] && ! _remote_session_ok "$DISPATCH_TMUX_SESSION"; then
  echo "dispatch resume: DISPATCH_TMUX_SESSION must match ^[A-Za-z0-9_][A-Za-z0-9_-]*\$ (got '$DISPATCH_TMUX_SESSION')" >&2
  exit 1
fi

# An optional leading target — `#N`/N, a Linear id, `worker:<branch>#<session>`,
# a branch or a codename — lets this run from anywhere. A word of those shapes
# is always a target; any other word is one only when the bus knows it (or it
# is ambiguous), so an extra prompt such as "continue" keeps working.
# #993: the bare-word probe and _resolve_target_worktree read the bus under the
# discovered common dir, which a planted commondir redirects.
if [ "${#positional[@]}" -gt 0 ] && _r_common="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"; then
  _wt_cfg_commondir "$_r_common" || exit 1
fi
if [ "${#positional[@]}" -gt 0 ]; then
  _first="${positional[0]}"
  if [[ $_first =~ ^#?[0-9]+$ || $_first =~ ^[A-Za-z]+-[0-9]+$ || $_first == */* || $_first == worker:* ]]; then
    target="$_first"
  else
    _rc=0
    crew resolve-target "$_first" >/dev/null 2>&1 || _rc=$?
    [ "$_rc" -eq 1 ] || target="$_first"
  fi
  if [ -n "$target" ]; then
    positional=("${positional[@]:1}")
  fi
fi
extra="${positional[*]}"

if [ -n "$rearm_roles" ] && [ -n "$fresh$do_print$ignore_map$agent_flag$model_flag$effort_flag$mcp_flag_val$chrome$extra" ]; then
  usage
  exit 1
fi

if [ -n "$target" ]; then
  _resolve_target_worktree "$target"
fi

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

branch="$(git branch --show-current)"
[ -n "$branch" ] || {
  echo "dispatch resume: detached HEAD — a worker resumes onto its own branch. Check the branch out and retry." >&2
  exit 1
}

# The task header and the branch name are worker-writable, and the launch script
# they end up in runs as the operator, outside the engine's permission layer
# (#470). Each must be a single plain word before it is spliced in; the launch
# line %q-quotes them again.
_id_re='^[A-Za-z0-9][A-Za-z0-9._-]*$'
_branch_re='^[A-Za-z0-9][A-Za-z0-9._/@+-]*$'
_model_re='^[A-Za-z0-9][][A-Za-z0-9._/:=,-]*$'
_roles_re='^[A-Za-z0-9][A-Za-z0-9._-]*(,[A-Za-z0-9][A-Za-z0-9._-]*)*$'
_effort_re='^(low|medium|high|xhigh|max|ultra)$'
_require_safe() { # $1=label $2=value $3=regex
  [[ $2 =~ $3 ]] && return 0
  echo "dispatch resume: refusing to launch: $1 ${2@Q} is not a plain word — a worker-writable value must not reach the launch script unchecked. Fix or re-dispatch this branch." >&2
  exit 1
}

_require_safe "branch name" "$branch" "$_branch_re"

# crew_dir is read by the escalation checks and the lead-session lookup below;
# the directory itself is only created once --print has had its exit.
crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"

# crew_dir and branch above came through the worktree's gitlink and HEAD, both
# worker-writable, so they are trusted only once they match the record dispatch
# wrote for this worktree (#518).
anchor="$(_worktree_anchor_path "$wt_path")"
if [ -L "$anchor" ] || [ ! -f "$anchor" ]; then
  echo "dispatch resume: no dispatcher record for $wt_path (dispatched before resume anchoring, or not by dispatch) — re-dispatch the task onto its branch; the worktree carries over" >&2
  exit 1
fi
mapfile -t _anchor_lines <"$anchor"
_wt_real="$(realpath -e -- "$wt_path")"
_crew_dir_real="$(realpath -m -- "$crew_dir")"
_gitdir_real="$(realpath -e -- "$(git rev-parse --absolute-git-dir)")"
_anchor_mismatch() { # $1=label $2=discovered $3=recorded
  echo "dispatch resume: this worktree's $1 ($2) does not match the dispatcher's record ($3) — refusing to resume" >&2
  exit 1
}
[ "${_anchor_lines[0]:-}" = "$_wt_real" ] || _anchor_mismatch "worktree path" "$_wt_real" "${_anchor_lines[0]:-}"
[ "${_anchor_lines[1]:-}" = "$_crew_dir_real" ] || _anchor_mismatch "crew dir" "$_crew_dir_real" "${_anchor_lines[1]:-}"
[ "${_anchor_lines[2]:-}" = "$branch" ] || _anchor_mismatch "branch" "$branch" "${_anchor_lines[2]:-}"
[ "${_anchor_lines[3]:-}" = "$_gitdir_real" ] || _anchor_mismatch "git dir" "$_gitdir_real" "${_anchor_lines[3]:-}"

# Anchored crew dir is now trusted; refuse a drifted config before it reaches
# the launch script (#557). Both configs come from the dispatcher record
# (common dir + this worktree's admin dir), so a relative `.git/hooks` compares
# canonically against the baseline and no hook-capable git runs from the
# worker's cwd (#638).
_wt_cfg_guard "${_crew_dir_real%/crew}" || exit 1
_wt_cfg_guard "${_crew_dir_real%/crew}" "${_anchor_lines[3]}" || exit 1

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
# Stamped at dispatch only when the owning dispatcher is on another host.
dispatcher_host="$(_hdr dispatcher_host)"
[[ -z $dispatcher_host || $dispatcher_host =~ ^[A-Za-z0-9][A-Za-z0-9_-]*$ ]] || dispatcher_host=""
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

_require_safe crew_id "$crew_id" "$_id_re"
[ -z "$agent_name" ] || _require_safe agent_name "$agent_name" "$_id_re"
[ -z "$grid_roles" ] || _require_safe roles "$grid_roles" "$_roles_re"
# An explicit --model @class resolves once the settings load, below; the header
# is worker-writable, so a sigil read from it is refused here like any odd value.
[[ -n $model_flag && $model == @* ]] || _require_safe model "$model" "$_model_re"
_require_safe effort "$effort" "$_effort_re"

_resolve_dir PROTOCOL_DIR DISPATCHER_PROTOCOL_DIR "@protocolDir@" "dispatch resume"

# Harness skill directory for pi workers; see pi_skill_args (launcher-common.sh) and the
# matching block in dispatch.sh.
_resolve_dir SKILLS_DIR DISPATCHER_SKILLS_DIR "@skillsDir@" "dispatch resume"

# Reviewer and critic markdown directories, handed to the launched session
# explicitly (write_launch_script) so it never reads a stale export left in the
# tmux server's environment.
_resolve_dir REVIEWERS_DIR DISPATCHER_REVIEWERS_DIR "@reviewersDir@" "dispatch resume"
_resolve_dir CRITICS_DIR DISPATCHER_CRITICS_DIR "@criticsDir@" "dispatch resume"
# The pinned `subagent` extension and generated agent roster pi leads load.
_resolve_dir PI_DIR DISPATCHER_PI_DIR "@piDir@" "dispatch resume"

# _settings_load, _glob_match, _exact_match, _model_class, _class_item,
# _class_sigil and _escalation_hop live in launcher-common.sh, which holds their
# contracts: the settings come from dispatch-config, the escalation rules from
# defaults.json, an @class item's class from model-resolve over the catalog and
# class-state caches below (read by those helpers, not here).
# shellcheck disable=SC2034
model_catalog="${XDG_DATA_HOME:-$HOME/.local/share}/crew/model-catalog.json"
# shellcheck disable=SC2034
model_class_state="${XDG_DATA_HOME:-$HOME/.local/share}/crew/model-class-state.json"
mapfile -t required_protocol_files < <({ printf '%s\n' WORKER_PROTOCOL.md EVIDENCE_REVIEW.md; _worker_protocol_files "$agent"; } | awk '!seen[$0]++')
_require_protocol_files "$PROTOCOL_DIR" "dispatch resume" "${required_protocol_files[@]}"
_check_protocol_rev "$PROTOCOL_DIR" "dispatch resume"
_settings_load

# --model @class (D6) resolves as dispatch's positional sigil does, once. The
# launch, the resume row, the escalation check and the precheck all see that
# resolved id; the precheck gets the sigil only through DISPATCH_CLASS_SIGIL,
# for dispatch's class admission and refusal text, so a class-state write
# between the two can never make the checked id differ from the launched one.
model_sigil=""
if [[ -n $model_flag && $model == @* ]]; then
  model_sigil="$model"
  model="$(_class_sigil "$agent" "$tier" "$model_sigil")" || exit 1
  _require_safe model "$model" "$_model_re"
fi

# mcp is the one gate the precheck below cannot make: passing --mcp there would
# have dispatch resolve and validate the config files too, which this script
# must do anyway to build the launch flags.
if [ -n "$mcp_profile" ]; then
  # shellcheck disable=SC2154 # set by _settings_load (launcher-common.sh)
  mcp_profile="$(mcp_worker_optins "dispatch resume" "$settings" "$agent" "$mcp_profile")" || exit 1
fi

profile="$(jq -r '.profile // "personal"' <<<"$settings")"

mcp_arg=""
if [[ ,$mcp_profile, == *,analytics,* ]]; then
  mcp_file="$HOME/.config/claude-code/mcp-posthog.json"
  [ -f "$mcp_file" ] || {
    echo "dispatch resume: mcp analytics config not found at $mcp_file" >&2
    exit 1
  }
  mcp_arg="--mcp-config $mcp_file"
fi
mcp_args="$(mcp_worker_launch_args "dispatch resume" "$settings" "$agent" "$mcp_profile")" || exit 1

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
# dispatch's precheck owns the --chrome refusals and the holder gate.
[ -z "$chrome" ] || precheck+=(--chrome)
# The tier↔model pair was adjudicated when this worker was first dispatched;
# only an explicit --model is a fresh choice that deserves re-gating.
# Escalation: if a prior session failed and --model is one rung up, allow it.
if [ -z "$model_flag" ] || [ -n "$ignore_map" ]; then
  precheck+=(--ignore-map)
else
  orig_model="$(sed -n 's/^model: //p' "$wt_path/WORKER_TASK.md" | head -1)"
  if [ -n "$orig_model" ] && [ "$orig_model" != "$model" ]; then
    # The header is worker-writable, so the bus must agree it is what failed.
    bus_failed_model="$(_prior_failed_model "$branch" "$crew_dir" "$tier")"
    escalation_baseline=""
    [ "$bus_failed_model" = "$orig_model" ] && escalation_baseline="$(_escalation_hop "$agent" "$tier" "$orig_model" "$model" outOfRow)"
    if [ -n "$escalation_baseline" ] && _prior_failed_escalation_available "$branch" "$crew_dir"; then
      precheck+=(--ignore-map)
      escalated_from="$escalation_baseline"
    fi
  fi
fi
if [ -z "$rearm_roles" ]; then
  DISPATCH_CHROME_SELF="$_crew_dir_real"$'\x1f'"$branch" DISPATCH_CLASS_SIGIL="$model_sigil" DISPATCH_PRECHECK=1 dispatch "$tier" "$model" "${precheck[@]}" "resume precheck" || exit 1
fi

# _args_have <argv line> <token>... — true when every token appears in the line
# as whole space-separated words, in the given order.
_args_have() {
  local line=" $1 " tok
  shift
  for tok in "$@"; do
    [[ $line == *" $tok "* ]] || return 1
    line=" ${line#*" $tok "}"
  done
}

# Re-arm the role panes of a restored grid window. tmux-remux brings the panes
# back with @crew_role stamped but no role-watch and no role stall-watch.
# Identity needs no pane env:
# the sender anchors on the pane's @crew_role and the window's stamps. The
# watchers find the bus through git, so they rely on resume's cwd being the
# worktree.
# _stamp_dispatcher_host <window> — @crew_dispatcher_host from the task header.
_stamp_dispatcher_host() {
  if [ -n "$dispatcher_host" ]; then
    tmux set-window-option -t "$1" @crew_dispatcher_host "$dispatcher_host"
  else
    tmux set-window-option -u -t "$1" @crew_dispatcher_host 2>/dev/null || true
  fi
}

_rearm_role_panes() {
  local role_re='^[A-Za-z0-9][A-Za-z0-9._-]*$'
  local bad p role exited cmd path r_agent delivery line snapshot="" have_ps="" watched stalled
  local -a flags
  [ -f "$rj" ] && [ ! -L "$rj" ] || return 0
  if bad="$(_artifacts_dir_bad "$branch")"; then
    echo "dispatch resume: not re-arming role panes — $bad is a symlink or not a directory" >&2
    return 0
  fi
  while IFS=$'\x1f' read -r p role exited cmd path; do
    [ -n "$p" ] || continue
    { [ -z "$role" ] || [ "$role" = lead ] || [ "$exited" = 1 ]; } && continue
    # A genuine grid's role panes sit in this worktree. Window ownership rests
    # partly on the worker-writable agent_name header; the pane's own path does not.
    [ "$path" = "$wt_path" ] || continue
    r_agent=""
    [[ $role =~ $role_re ]] && r_agent="$(jq -r --arg r "$role" '.[$r].agent // empty' "$rj" 2>/dev/null || true)"
    case "$r_agent" in
    claude | codex | cursor | pi) ;;
    *)
      echo "dispatch resume: role pane $p ($role) has no usable engine in roles.json — not re-armed" >&2
      continue
      ;;
    esac
    if ! crew engine-cmd "$cmd" 2>/dev/null; then
      # A bare shell: let the lead's died-role path respawn it through --spawn-role.
      tmux set-option -p -t "$p" @crew_exited 1 2>/dev/null || true
      tmux set-option -p -t "$p" @crew_state exited 2>/dev/null || true
      echo "dispatch resume: role pane $p ($role) runs no engine — marked exited for the lead to respawn" >&2
      continue
    fi
    delivery=typed
    [ "$r_agent" != cursor ] || delivery=pull
    tmux set-option -p -t "$p" @crew_delivery "$delivery" 2>/dev/null || true
    if [ -z "$have_ps" ]; then
      # -ww: procps truncates args to $COLUMNS otherwise
      if ! snapshot="$(ps -ww -A -o args= 2>/dev/null)"; then
        echo "dispatch resume: ps failed — not re-arming role panes" >&2
        return 0
      fi
      have_ps=1
    fi
    rearmed=$((rearmed + 1))
    watched="" stalled=""
    while IFS= read -r line; do
      _args_have "$line" "--role-watch $role" "--pane $p" "--branch $branch" && watched=1
      _args_have "$line" "stall-watch role:$branch:$role" "--pane $p" && stalled=1
    done <<<"$snapshot"
    # Mirrors the launch rule of adapters/core/dispatch.sh watch_role / watch_role_prompts.
    [ -n "$watched" ] || (CREW_ID="$crew_id" nohup dispatch --role-watch "$role" --pane "$p" --engine "$r_agent" --branch "$branch" >/dev/null 2>&1 &)
    [ -z "$stalled" ] || continue
    [ "$r_agent" = claude ] || [ -z "$ignore_budget" ] || continue
    flags=()
    [ -z "$ignore_budget" ] || flags+=(--no-budget)
    (CREW_ID="$crew_id" nohup crew stall-watch "role:$branch:$role" --pane "$p" --engine "$r_agent" "${flags[@]}" >/dev/null 2>&1 &)
  done <<PANES
$(tmux list-panes -t "$win" -F '#{pane_id}'$'\x1f''#{@crew_role}'$'\x1f''#{@crew_exited}'$'\x1f''#{pane_current_command}'$'\x1f''#{pane_current_path}' 2>/dev/null || true)
PANES
}

# Placement. dispatch always opens a fresh window and refuses when anything
# already sits at the worktree — including a pane with an empty @crew_name,
# i.e. a human in a plain shell, which is exactly whoever runs this command.
# Inheriting that would refuse the primary use case, so resume reuses the pane
# that is already there and gives up the anti-stacking refusal in trade.
#
# Keyed on pane_current_path, not the window name: lazytmux renames worker
# windows, so the name dispatch assigned is long gone by now.
rj="$crew_dir/artifacts/$branch/roles.json"
# Role panes _rearm_role_panes handled (watched or already watched).
rearmed=0
win=""
pane=""
pane_cmd=""
win_own=""
reused=""
# A restored grid window also has role panes at the worktree: never launch the
# lead into one, and prefer this worker's own window (stamped for the branch,
# or restored with its codename) over a human shell at the same path elsewhere.
# \x1f, not tab: tab is IFS whitespace, so an empty field would collapse and
# shift the rest.
while IFS=$'\x1f' read -r cand_win cand_pane cand_path cand_cmd cand_name cand_role cand_branch; do
  [ -n "$cand_win" ] || continue
  [ "$cand_path" = "$wt_path" ] || continue
  [ -z "$cand_role" ] || [ "$cand_role" = lead ] || continue
  cand_own=""
  if [ "$cand_branch" = "$branch" ] || { [ -z "$cand_branch" ] && [ "$cand_name" = "$agent_name" ]; }; then
    cand_own=1
  fi
  if [ -z "$reused" ] || { [ -n "$cand_own" ] && [ -z "$win_own" ]; }; then
    win="$cand_win"
    pane="$cand_pane"
    pane_cmd="$cand_cmd"
    win_own="$cand_own"
    reused=1
  fi
done <<PANES
$(tmux list-panes -a -F '#{window_id}'$'\x1f''#{pane_id}'$'\x1f''#{pane_current_path}'$'\x1f''#{pane_current_command}'$'\x1f''#{@crew_name}'$'\x1f''#{@crew_role}'$'\x1f''#{@crew_branch}' 2>/dev/null || true)
PANES

if [ -n "$rearm_roles" ]; then
  if [ -z "$reused" ] || [ -z "$win_own" ]; then
    echo "dispatch resume: no window of this worker to re-arm at $wt_path — nothing to do" >&2
    exit 1
  fi
  # Same stamps as the full path: tmux-remux does not restore @crew_branch, and
  # --role-watch refuses a window whose stamp mismatches.
  tmux set-window-option -t "$win" @crew_dir "$crew_dir"
  tmux set-window-option -t "$win" @crew_branch "$branch"
  tmux set-window-option -t "$win" @crew_id "$crew_id"
  _stamp_dispatcher_host "$win"
  _rearm_role_panes
  if [ "$rearmed" -gt 0 ]; then
    echo "dispatch resume: re-armed $rearmed role pane(s) in window $win; lead not relaunched"
  else
    echo "dispatch resume: no role panes to re-arm in window $win"
  fi
  exit 0
fi

# Cheap partial guard (#111): refuse a pane that is still running an engine.
# `crew engine-cmd` shares the nix-wrapper-aware matcher local-models.sh's
# _is_engine_cmd carries rather than duplicating it here. Deliberately a
# command-name sniff, not the stronger bus-state gate dispatch.sh uses for its
# own placement refusal (dispatch.sh:699-711) — see that gate's own comment
# for why its engine count is only advisory.
if [ -n "$reused" ] && crew engine-cmd "$pane_cmd" 2>/dev/null; then
  echo "dispatch resume: $wt_path's pane ($pane) is running $pane_cmd — a worker session is already alive there. Attach to it instead of resuming (tmux select-window -t $win), or wait for it to exit/finish first." >&2
  [ -z "$win_own" ] || echo "dispatch resume: if only the role panes of this restored window need their watchers, run: dispatch resume --rearm-roles" >&2
  exit 1
fi

# Which engine session is the lead's own. Role panes share this worktree, so
# the engine's "latest session here" (--continue) may be a role's transcript.
# dispatch records the lead in $crew_dir/leads/<branch> as `<engine> <id>`
# (`-` for codex/cursor, whose id cannot be pre-assigned). A dispatch that
# reaches its lead launch always leaves a record, so an absent record means a
# worker dispatched before records existed. Resume never resumes a session it
# cannot attribute to the lead: it relaunches fresh with the reorient note.
# claude and pi read the id from the record. codex continues only by an id in
# the record: `codex resume --last` is the latest session globally, so resuming
# two codex leads back to back hands the second one the first's conversation,
# in the wrong worktree (#940). cursor's --continue stays: it resolves inside
# the cwd's own session bucket, so it cannot leave the worktree.
#   lead_cont   flag(s) after the engine binary that select the session
#   lead_sid    id for --session-id (claude/pi fresh launch; pi resume by id)
#   lead_record id to write to the record once the launch is committed to
_uuid_re='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
lead_rec="$crew_dir/leads/$branch"
lead_cont=""
lead_sid=""
lead_record=""
lead_session="-"
fresh_why=""
claude_projects="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects"
legacy_cont="--continue"

# Existence only here: roles.json is worker-writable. _rearm_role_panes reads
# it after the artifacts-dir check, for the role's engine only.
has_roles=""
[ -z "$grid_roles" ] || has_roles=1
{ [ -e "$rj" ] || [ -L "$rj" ]; } && has_roles=1

rec_state=none
rec_engine=""
rec_id=""
rec_extra=""
if ! _lead_record_safe; then
  rec_state=bad
elif [ -f "$lead_rec" ]; then
  rec_state=bad
  read -r rec_engine rec_id rec_extra <<<"$(head -n1 "$lead_rec" 2>/dev/null || true)" || true
  if [ -z "$rec_extra" ]; then
    case "$rec_engine" in
    claude | pi) [[ $rec_id =~ $_uuid_re ]] && rec_state=ok ;;
    codex)
      # `-` is "launched, id never learned": a valid record, nothing attributable.
      [[ $rec_id =~ $_uuid_re || $rec_id == - ]] && rec_state=ok
      ;;
    cursor) [ "$rec_id" = - ] && rec_state=ok ;;
    esac
  fi
fi

if [ -n "$fresh" ]; then
  :
elif [ "$rec_state" = bad ] && [ "$rec_engine $rec_id" = "pending -" ]; then
  fresh_why="this branch's last dispatch never launched its lead"
elif [ "$rec_state" = bad ]; then
  fresh_why="the lead's session record is malformed or unsafe"
elif [ "$rec_state" = ok ] && [ "$rec_engine" != "$agent" ]; then
  fresh_why="the lead ran $rec_engine, not $agent — its session cannot be resumed on $agent"
elif [ "$rec_state" = ok ] && [ "$rec_id" != - ]; then
  if [ "$agent" = claude ]; then
    rec_found=""
    for f in "$claude_projects"/*/"$rec_id".jsonl; do
      [ -f "$f" ] && rec_found=1
    done
    if [ -n "$rec_found" ]; then
      lead_cont="--resume $rec_id"
      lead_session="$rec_id"
    else
      fresh_why="the lead's recorded claude session has no transcript"
    fi
  elif [ "$agent" = codex ]; then
    # The record's id was written for this lead, so codex resumes exactly it.
    # No rollout probe: codex names a session id it cannot find and exits.
    lead_cont="resume $rec_id"
    lead_session="$rec_id"
  else
    # pi has no cheap existence probe: --session-id silently starts a new
    # session under that id if the transcript vanished.
    lead_sid="$rec_id"
    lead_session="$rec_id"
  fi
elif [ "$rec_state" = none ] && [ "$(_hdr engine)" != "$agent" ]; then
  fresh_why="the lead ran $(_hdr engine), not $agent, and has no recorded session"
elif [ -n "$has_roles" ]; then
  fresh_why="the lead shares this worktree with role panes and has no recorded session"
elif [ "$agent" = codex ]; then
  fresh_why="codex has no session id recorded for this lead, and its --last is the latest session globally, not this worktree's"
else
  # A pre-records solo worker, or a cursor solo lead (`cursor -`): both engines'
  # --continue resolves inside this worktree's own session bucket.
  lead_cont="$legacy_cont"
fi

if [ -n "$fresh_why" ]; then
  fresh=1
  echo "dispatch resume: $fresh_why — relaunching fresh with the reorient note" >&2
fi
if [ -n "$fresh" ]; then
  lead_cont=""
  lead_session="-"
  case "$agent" in
  claude | pi)
    lead_sid="$(_uuid)"
    lead_record="$lead_sid"
    ;;
  *) lead_record=- ;;
  esac
fi

# --print is a dry run: report the placement the lookup above already found
# and stop before anything below opens a window or restyles a pane. On the
# create path there is no window or pane id yet, so those report as "-" and
# placement: create carries the meaning.
if [ -n "$do_print" ]; then
  printf 'branch: %s\nworktree: %s\nengine: %s\nmodel: %s\neffort: %s\nmcp: %s\ntier: %s\ncrew_id: %s\nagent_name: %s\nprev_worker_id: %s\ncontinue: %s\nlead_session: %s\nwindow: %s\npane: %s\nplacement: %s\n' \
    "$branch" "$wt_path" "$agent" "$model" "$effort" "$mcp_profile" \
    "$tier" "$crew_id" "$agent_name" "$prev_worker_id" \
    "$([ -n "$fresh" ] && echo false || echo true)" "$lead_session" \
    "${win:--}" "${pane:--}" "$([ -n "$reused" ] && echo reuse || echo create)"
  exit 0
fi

# Rewrite the protocol-dirs record before anything opens a window, so the lead
# and a lazy --spawn-role launch against the same dirs (#496).
if bad="$(_protocol_dirs_record_bad)"; then
  echo "dispatch resume: $bad is a symlink or the wrong type — refusing to write the protocol-dirs record" >&2
  exit 1
fi
_record_protocol_dirs "$wt_path"

# Fail closed: an empty PI_CODING_AGENT_DIR falls back to ~/.pi/agent,
# so a broken seeder must abort before the worker ever launches against it.
if [ "$agent" = pi ]; then
  pi_agent_dir="$(crew pi-agent-dir)" || pi_agent_dir=""
  case "$pi_agent_dir" in
  /*) [ -d "$pi_agent_dir" ] || pi_agent_dir="" ;;
  *) pi_agent_dir="" ;;
  esac
  [ -n "$pi_agent_dir" ] || {
    echo "dispatch resume: could not seed the pi worker agent dir (crew pi-agent-dir) — refusing to launch pi against ~/.pi/agent" >&2
    exit 1
  }
  printf -v quoted_pi_dir '%q' "$pi_agent_dir"
  pi_subagent_ext="$PI_DIR/extensions/subagent/index.ts"
  if [ ! -f "$pi_subagent_ext" ]; then
    echo "dispatch resume: pi subagent extension missing: $pi_subagent_ext — set DISPATCHER_PI_DIR to an adapters/pi checkout or rebuild" >&2
    exit 1
  fi
  printf -v q_pi_ext '%q' "$pi_subagent_ext"
fi

if [ -z "$pane" ]; then
  sanitized="${branch//\//-}"
  client_width=""
  client_height=""
  if [ "$wire_side" = remote ]; then
    # Remote mode, as in dispatch: the named session, created with the
    # window when absent; no client to size from.
    win_args=(-c "$wt_path" -n "$sanitized" -P -F '#{window_id} #{pane_id}')
    # The `|| true` keeps a failed client reaching the "could not resolve a
    # tmux pane" message below instead of dying under errexit.
    if tmux has-session -t "=$DISPATCH_TMUX_SESSION" 2>/dev/null; then
      win_ids="$(_pane_client new-window -d -t "=$DISPATCH_TMUX_SESSION:" "${win_args[@]}" || true)"
    else
      # A racing dispatch may create the session first.
      win_ids="$(_pane_client new-session -d -s "$DISPATCH_TMUX_SESSION" "${win_args[@]}" 2>/dev/null)" ||
        win_ids="$(_pane_client new-window -d -t "=$DISPATCH_TMUX_SESSION:" "${win_args[@]}" || true)"
    fi
    read -r win pane <<<"$win_ids"
  else
    # Same client-geometry handling as dispatch: a detached new-window otherwise
    # inherits tmux's fallback size, and codex's startup banner never redraws.
    client_target=()
    [ -n "${TMUX_PANE:-}" ] && client_target=(-t "$TMUX_PANE")
    client_size="$(tmux display-message -p "${client_target[@]}" '#{client_width} #{client_height} #{status}' 2>/dev/null || true)"
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
    read -r win pane < <(_pane_client new-window -d -c "$wt_path" -n "$sanitized" -P -F '#{window_id} #{pane_id}')
  fi
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
# --spawn-role finds its crew dir and branch here, not via git discovery, which
# the worker's env and worktree .git steer (#496); the role watcher anchors its
# dispatcher on @crew_id.
tmux set-window-option -t "$win" @crew_dir "$crew_dir"
tmux set-window-option -t "$win" @crew_branch "$branch"
tmux set-window-option -t "$win" @crew_id "$crew_id"
_stamp_dispatcher_host "$win"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
# A grid lead keeps @crew_role=lead (the key crew.sh's status-publish guard
# checks) and the state-bearing border; a non-grid worker keeps the plain label.
if [ -n "$grid_roles" ] || [ "$(tmux show-option -wqv -t "$win" @crew_grid 2>/dev/null || true)" = 1 ]; then
  tmux set-option -p -t "$pane" @crew_role lead 2>/dev/null || true
  tmux set-window-option -t "$win" pane-border-format "$(grid_lead_format)"
else
  tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold]#{?#{@crew_dispatcher_host}, · ⇄ #{@crew_dispatcher_host},} "
fi

kind="$(_hdr kind)"
plan_val="$(_hdr plan)"

# Session identity. A resume gets a NEW session id and therefore a new
# worker_id: the pane, the watchdog and the bus rows are all new even when the
# conversation is not. dispatch.sh:832 owns the same shape.
session="${DISPATCH_SESSION_ID:-s$(date +%s)-$$}"
_require_safe session "$session" "$_id_re"
worker_id="worker:$branch#$session"

mkdir -p "$crew_dir"

[ -z "$lead_record" ] || _record_lead_session "$agent" "$lead_record" || true

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
    if _recorded_pid_live "$epid" "$cdir/pid"; then
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
_hdr_set protocol_dir "$PROTOCOL_DIR"
_hdr_set chrome "$([ -n "$chrome" ] && echo true || echo false)"
# The seam relaunch loop runs only under dispatch's own launch; a resumed lead
# runs without it, so it must not try the trigger.
[ "$(_hdr fresh_seams)" != true ] || _hdr_set fresh_seams false

# Ensure WORKER_TASK.md is excluded from tracking across every worktree.
exclude_file="${_crew_dir_real%/crew}/info/exclude"
if ! grep -qxF 'WORKER_TASK.md' "$exclude_file" 2>/dev/null; then
  printf '\n%s\n' 'WORKER_TASK.md' >>"$exclude_file"
fi

# Exclude rules do not apply to a file git already tracks, so a WORKER_TASK.md
# committed on this branch escapes the guard above and rides the diff into
# commits (#397). Warn, naming the fix; never touch the index here — removing
# a tracked file is the target repo's job, in its own PR. Plain `ls-files`
# prints the path only if tracked, so a non-zero exit means the index was
# unreadable or _wt_git refused, which must not pass as "untracked" (#539).
tracked="$(_wt_git "${_anchor_lines[3]}" "$wt_path" ls-files -- WORKER_TASK.md)" || {
  echo "dispatch resume: could not read the index at $wt_path" >&2
  exit 1
}
if [ -n "$tracked" ]; then
  echo "dispatch resume: warning: WORKER_TASK.md is tracked on this branch — .git/info/exclude cannot hide a tracked file, so it will ride into this worker's commits. Remove it in its own PR: git rm --cached WORKER_TASK.md" >&2
fi
if [ -n "$dispatcher_live" ] && [ -n "$dispatcher_pane_new" ]; then
  _hdr_set dispatcher_pane "$dispatcher_pane_new"
fi
# An escalation is the worker's new launch tuple: without the model line a later
# plain resume would read the old header and relaunch on the failed rung.
if [ -n "${escalated_from:-}" ]; then
  _hdr_set model "$model"
  _hdr_set escalated_from "$escalated_from"
fi

# The resume row. New kind: without it a worker resumed four times reports as
# one run, and the ratings rollup attributes the whole cost and latency to a
# single session. prev_worker_id is what chains the sessions back together.
escalated_from_event="${escalated_from:-}"
line=$(jq -nc --arg crew "$crew_id" --arg branch "$branch" \
  --arg worker "$worker_id" --arg prev "$prev_worker_id" \
  --arg engine "$agent" --arg model "$model" --arg session "$session" \
  --argjson continued "$([ -n "$fresh" ] && echo false || echo true)" \
  --arg engine_session "${lead_sid:-${lead_session#-}}" \
  --arg escalated_from "$escalated_from_event" \
  --argjson chrome "$([ -n "$chrome" ] && echo true || echo false)" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"resume", branch:$branch,
     worker_id:$worker, prev_worker_id:$prev, engine:$engine, model:$model,
     session:$session, continued:$continued,
     engine_session:(if $engine_session == "" then null else $engine_session end)}
   + if $escalated_from != "" then {escalated_from:$escalated_from} else {} end
   + if $chrome then {chrome:true} else {} end')
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
  plan_note=" The task doc is your plan of record — extract the steps and implement; do not re-plan or re-critique the plan."
  if [ "$tier" != trivial ] && [ "$kind" != review ]; then
    plan_note="$plan_note Only planning is skipped: the fast deterministic gate and the code review gate still run before you push."
  fi
fi

push_mandate=" Push when pre-push passes; open a PR."
if [ "$kind" != review ] && [ "$tier" != trivial ]; then
  push_mandate=" Run your code review gate and record its review seam before you push.$push_mandate"
fi
if [ "$kind" = review ]; then
  push_mandate=" Review only — do not edit, commit, push, or open a PR; post one COMMENT review and report to the bus."
fi

grid_note=""
if [ -n "$grid_roles" ] && [ "$kind" = review ]; then
  grid_note=" You lead a review role grid: role panes ($grid_roles) may still be parked in this window. Follow REVIEW_TASK.md Role-grid path and address them through the crew bus."
elif [ -n "$grid_roles" ]; then
  grid_file=$(_worker_protocol_files "$agent" | sed -n '/^WORKER_GRID/p')
  grid_note=" You lead a role grid: role panes ($grid_roles) may still be parked in this window. Follow $PROTOCOL_DIR/$grid_file Grid mode and address them through the crew bus."
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

protocol_note=" Protocol files (EVIDENCE_REVIEW.md, GRID_PROTOCOL.md, ...) live in $PROTOCOL_DIR — also stamped as protocol_dir: in WORKER_TASK.md."

# Printed so the dispatcher can address this session in the gap before the worker
# boots — its startup drain is unbounded, so a scoping note posted now still lands.
echo "worker_id: $worker_id"

# Cross-repo resume (#420, sharing #398's detection): the crew bus is per repo,
# so print the worker-repo lane command when the dispatcher's pane sits
# elsewhere and that bus is not being streamed. The detection is one sourced
# helper whose store path flake.nix bakes, shared with dispatch.sh; a raw run
# without the override skips it rather than aborting under set -e.
hint_lib="${CROSS_REPO_HINT_LIB:-@crossRepoHintLib@}"
if [ -r "$hint_lib" ]; then
  # shellcheck source=/dev/null
  . "$hint_lib"
  cross_repo_hint "${crew_dir%/crew}" "$crew_id"
fi

printf -v q_worker_id '%q' "$worker_id"
printf -v q_crew_id '%q' "$crew_id"
printf -v q_agent_name '%q' "$agent_name"
printf -v q_model '%q' "$model"
printf -v q_effort '%q' "$effort"

if [ "$agent" = codex ]; then
  cont="$lead_cont"
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: $(worker_browser_env "$branch" "$wt_path" "$agent_browser_worker_config")CREW_WORKER_ID=$q_worker_id CREW_ID=$q_crew_id codex $cont --profile worker -m $q_model -c model_reasoning_effort=$q_effort -c service_tier=default -c agents.enabled=true -c agents.max_concurrent_threads_per_session=3 -c agents.default_subagent_reasoning_effort=$codex_subagent_effort$mcp_args --dangerously-bypass-approvals-and-sandbox 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}${protocol_note}'"
elif [ "$agent" = cursor ]; then
  cont="$lead_cont"
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: $(worker_browser_env "$branch" "$wt_path" "$agent_browser_worker_config")CREW_WORKER_ID=$q_worker_id CREW_ID=$q_crew_id CURSOR_CLI_INDEXED_GREP=0 cursor-agent $cont --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model $q_model 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}${protocol_note}'"
elif [ "$agent" = pi ]; then
  cont="$lead_cont"
  pi_sid=""
  [ -z "$lead_sid" ] || pi_sid=" --session-id $lead_sid"
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: $(worker_browser_env "$branch" "$wt_path" "$agent_browser_worker_config")CREW_WORKER_ID=$q_worker_id CREW_ID=$q_crew_id PI_CODING_AGENT_DIR=$quoted_pi_dir pi $cont --name $q_agent_name --model $q_model --thinking $q_effort$pi_sid --append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.pi.md --no-approve -e $q_pi_ext$(pi_skill_args "$wt_path")$mcp_args 'Read WORKER_TASK.md and continue it.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}${protocol_note}'"
else
  cont="$lead_cont"
  claude_sid=""
  [ -z "$lead_sid" ] || claude_sid=" --session-id $lead_sid"
  # Re-passing --append-system-prompt-file matters on a continue: it forces
  # --system-prompt-snapshot off, so the worker protocol is applied fresh rather
  # than replayed from the conversation's recorded prompt.
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: $(worker_browser_env "$branch" "$wt_path" "$agent_browser_worker_config")CREW_WORKER_ID=$q_worker_id CREW_ID=$q_crew_id $(claude_lean_env)claude $cont --name $q_agent_name --model $q_model --effort $q_effort$claude_sid $mcp_arg$mcp_args $xreview_mcp --settings $(claude_worker_plugin_settings "$settings" "$chrome")$(launch_dir_args claude "$branch") --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.claude.md --permission-mode auto$(claude_chrome_flag "$chrome") 'Read WORKER_TASK.md and continue it.${push_mandate}${plan_note}${reorient}${grid_note}${protocol_note}'"
fi
write_launch_script launch_line "$launch_cmd"
# dispatch's localModels slot count reads this off the lead pane.
tmux set-option -p -t "$pane" @crew_model "$model" 2>/dev/null || true
# The --chrome holder scan reads this.
if [ -n "$chrome" ]; then
  tmux set-option -p -t "$pane" @crew_chrome 1 2>/dev/null || true
else
  tmux set-option -p -u -t "$pane" @crew_chrome 2>/dev/null || true
fi
# shellcheck disable=SC2154 # set by write_launch_script's nameref (_launch)
tmux send-keys -t "$pane" "$launch_line" Enter

# Re-arm the stall watchdog: the original self-exited when it saw the terminal
# state, and a resumed worker can wedge exactly the same way. --ignore-budget
# waives its budget detector. The subshell orphans it: resume may run inside
# another worker's stamped pane.
stall_flags=()
[ -n "$ignore_budget" ] && stall_flags+=(--no-budget)
(CREW_ID="$crew_id" nohup crew stall-watch "$worker_id" --pane "$pane" --engine "$agent" "${stall_flags[@]}" >/dev/null 2>&1 &)
# A human resuming from a worker's own pane must not retarget the renderer
# there; only the registered dispatcher's pane may move it.
rr_pane="${TMUX_PANE:-}"
if [ -n "$dispatcher_live" ] && [ "$rr_pane" != "$dispatcher_pane_new" ]; then
  rr_pane=""
fi
# A remote-mode worker's roster diagram belongs to its dispatcher's host.
[ "$wire_side" = remote ] || _ensure_roster_render "$crew_id" "$rr_pane"

# Resume re-stamps @crew_branch on whatever window it lands in, so only a window
# that was already this worker's (stamped for the branch, or restored with its
# codename) may have its role panes armed.
[ -z "$win_own" ] || _rearm_role_panes
