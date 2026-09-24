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

# pi_skill_args <worktree> — emit --skill flags for the worktree's own project
# skill dirs and for the harness's own skills ($SKILLS_DIR, set below).
# Duplicated from dispatch.sh (standalone build); see the comment there for why
# the pi launch's --no-approve needs an explicit --skill.
pi_skill_args() {
  local wt="$1" d
  for d in "$wt/.pi/skills" "$wt/.agents/skills" "$SKILLS_DIR"; do
    [ -d "$d" ] && printf ' --skill %q' "$d"
  done
  return 0
}

# shell_quote and write_launch_script: duplicated from dispatch.sh (standalone
# build); parity-tested against dispatch.sh's copies.
shell_quote() {
  local -n _out="$1"
  local _text="$2" _res="" _c _i
  for ((_i = 0; _i < ${#_text}; _i++)); do
    _c="${_text:_i:1}"
    case "$_c" in
    "'") _res+="'\\''" ;;
    \\) _res+="'\\\\'" ;;
    *) _res+="$_c" ;;
    esac
  done
  _out="'$_res'"
}

write_launch_script() {
  local -n _launch="$1"
  local _dir="$crew_dir/launch" _file _quoted
  # mkdir -p succeeds on a symlink to a dir, and every write would land in its target.
  if [ -L "$_dir" ] || { [ -e "$_dir" ] && [ ! -d "$_dir" ]; }; then
    echo "dispatch: $_dir is a symlink or not a directory — refusing to write a launch script" >&2
    exit 1
  fi
  # shellcheck disable=SC2174 # $crew_dir already exists; -m only needs to reach the new leaf, and chmod below covers a pre-existing one too
  mkdir -p -m 700 "$_dir"
  chmod 700 "$_dir"
  find "$_dir" -type f -name 'launch.*' -mtime +7 -delete 2>/dev/null || true
  # An exit.* deletes itself when its engine returns, but a reaped pane never
  # runs it, so the file leaks (#343). Reclaim an old exit.* only once the pane
  # it names is gone; a reused pane id just delays deletion. tmux failing, or
  # listing no panes, deletes nothing (fail-safe).
  local _panes _ef _epane
  if _panes="$(tmux list-panes -a -F '#{pane_id}' 2>/dev/null)" && [ -n "$_panes" ]; then
    while IFS= read -r _ef; do
      _epane="$(sed -n "s/.*--pane '\(%[0-9][0-9]*\)'.*/\1/p" "$_ef" 2>/dev/null || true)"
      _epane="${_epane%%$'\n'*}"
      [ -n "$_epane" ] || continue
      printf '%s\n' "$_panes" | grep -qxF -- "$_epane" || rm -f -- "$_ef"
    done < <(find "$_dir" -type f -name 'exit.*' -mtime +7 2>/dev/null)
  fi
  if [ "${3:-}" = exit ]; then
    _file="$(mktemp "$_dir/exit.XXXXXX")"
    # shellcheck disable=SC2016 # the literal "$0" is the generated script's own, expanded when IT runs, not now
    printf '#!/usr/bin/env bash\nrm -f -- "$0"\nexec env -u DISPATCHER_PROTOCOL_DIR -u DISPATCHER_SKILLS_DIR -u DISPATCHER_REVIEWERS_DIR -u DISPATCHER_CRITICS_DIR %s\n' "$2" >"$_file"
  else
    local _dirs="" _unset="" _n _v
    for _n in PROTOCOL SKILLS REVIEWERS CRITICS; do
      _v="${_n}_DIR"
      _v="${!_v:-}"
      if [[ $_v == /* ]]; then
        printf -v _dirs '%sDISPATCHER_%s_DIR=%q ' "$_dirs" "$_n" "$_v"
      else
        printf -v _unset '%s-u DISPATCHER_%s_DIR ' "$_unset" "$_n"
      fi
    done
    _file="$(mktemp "$_dir/launch.XXXXXX")"
    printf '#!/usr/bin/env bash\nexec env %s%s%s\n' "$_unset" "$_dirs" "$2" >"$_file"
  fi
  chmod 700 "$_file"
  shell_quote _quoted "$_file"
  _launch="bash $_quoted"
}

# _require_protocol_files <dir> <file...> — abort before any scaffolding if
# a required protocol file is missing from $PROTOCOL_DIR. $DISPATCHER_PROTOCOL_DIR
# can point at a stale checkout (#177); this stops the launch instead of
# spawning an engine against a missing --append-system-prompt(-file) target.
_require_protocol_files() {
  local dir="$1" f missing_files=()
  shift
  for f in "$@"; do
    [ -f "$dir/$f" ] || missing_files+=("$f")
  done
  [ "${#missing_files[@]}" -eq 0 ] && return 0
  local override=""
  [ -n "${DISPATCHER_PROTOCOL_DIR:-}" ] && override=" (DISPATCHER_PROTOCOL_DIR=$DISPATCHER_PROTOCOL_DIR)"
  echo "dispatch resume: missing protocol file(s) in \$PROTOCOL_DIR ($dir)${override}: ${missing_files[*]} — refusing to launch" >&2
  exit 1
}

# _check_protocol_rev <dir> <label> — refuse a protocol directory whose content
# does not match this script's baked revision (#184, #193). See dispatch.sh's
# copy of this helper for the full contract: a substituted marker recomputes
# the build's content hash from the files actually in $dir (same rule, same
# edge-case handling — sorted bare names, dotfiles included) and refuses a
# mismatch before any scaffolding; a raw checkout script skips with a one-line
# warning. There is no committed PROTOCOL_REV file to read.
_check_protocol_rev() {
  local dir="$1" label="$2" stamped_rev="@protocolRev@" dir_rev entries=""
  local names=() file name sig
  # The sentinel is the placeholder's *shape*, not the literal: flake.nix's
  # replaceStrings (and a test's sed) rewrite every @protocolRev@ occurrence,
  # so a literal comparison would make a substituted script skip its own
  # check. A baked rev (16 hex chars) never starts with @.
  if [[ $stamped_rev == @* ]]; then
    echo "$label: unsubstituted protocol revision (raw checkout script) — skipping the protocol revision consistency check" >&2
    return 0
  fi
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
  # Newlines are a construction convenience only — strip before hashing to
  # match flake.nix's concatStringsSep "".
  dir_rev="$(printf '%s' "$entries" | tr -d '\n' | sha256sum | cut -d' ' -f1 | cut -c1-16)"
  if [ "$dir_rev" != "$stamped_rev" ]; then
    echo "$label: protocol directory version mismatch — refusing to launch" >&2
    echo "$label:   script protocol revision: $stamped_rev" >&2
    echo "$label:   \$PROTOCOL_DIR content revision: $dir_rev" >&2
    echo "$label:   \$PROTOCOL_DIR: $dir" >&2
    if [ -n "${DISPATCHER_PROTOCOL_DIR:-}" ]; then
      echo "$label:   DISPATCHER_PROTOCOL_DIR override: $DISPATCHER_PROTOCOL_DIR" >&2
      echo "$label:   remedy: unset DISPATCHER_PROTOCOL_DIR, or point it at a checkout matching this build" >&2
    fi
    exit 1
  fi
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

# _resolve_dir <OUT_VAR> <ENV_VAR> <baked> <label> — resolve a DISPATCHER_*_DIR
# override against the baked default (#303). A shell or tmux server that
# outlives a rebuild keeps the previous build's export, so an override under the
# baked path's store root is kept only when its content equals the baked dir's:
# the current build's export sits at a different store path than the baked
# projection but holds the same files. A checkout override always wins, as does
# any override in a raw script (baked is not absolute). diff sits in an `if`
# because its exit 1 means "differs", not failure. A relative override is
# refused: a launched session runs in a task worktree, whose files a diff
# controls, and would resolve it there.
# A stale value is ignored with a notice and unset, so later diagnostics do not
# name it.
_resolve_dir() {
  local out="$1" var="$2" baked="$3" label="$4" val="${!2:-}"
  if [ -z "$val" ]; then
    printf -v "$out" '%s' "$baked"
    return 0
  fi
  if [[ $val != /* ]]; then
    echo "$label: $var must be an absolute path, got: $val" >&2
    exit 1
  fi
  if [[ $baked == /* && $val == "${baked%/*}"/* && $val != "$baked" ]] &&
    ! diff -rq -- "$val" "$baked" >/dev/null 2>&1; then
    echo "$label: ignoring stale $var from a previous build: $val; using $baked" >&2
    unset "$var"
    printf -v "$out" '%s' "$baked"
    return 0
  fi
  printf -v "$out" '%s' "$val"
}

_resolve_dir PROTOCOL_DIR DISPATCHER_PROTOCOL_DIR "@protocolDir@" "dispatch resume"

# Harness skill directory for pi workers; see pi_skill_args above and the
# matching block in dispatch.sh.
_resolve_dir SKILLS_DIR DISPATCHER_SKILLS_DIR "@skillsDir@" "dispatch resume"

# Reviewer and critic markdown directories, handed to the launched session
# explicitly (write_launch_script) so it never reads a stale export left in the
# tmux server's environment.
_resolve_dir REVIEWERS_DIR DISPATCHER_REVIEWERS_DIR "@reviewersDir@" "dispatch resume"
_resolve_dir CRITICS_DIR DISPATCHER_CRITICS_DIR "@criticsDir@" "dispatch resume"

_require_protocol_files "$PROTOCOL_DIR" WORKER_PROTOCOL.md EVIDENCE_REVIEW.md
_check_protocol_rev "$PROTOCOL_DIR" "dispatch resume"

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

# Escalation helpers (duplicated from dispatch.sh — this is a separate binary).
# _escalation_target <engine> <tier> <failed_model> — prints "<baseline> <escalated>"
# if the failed_model is exactly one rung below a valid escalation target.
_escalation_target() {
  local eng="$1" tier="$2" failed="$3"
  case "$eng:$tier:$failed" in
  claude:standard:sonnet | claude:standard:claude-sonnet-*) printf 'sonnet opus' ;;
  claude:trivial:haiku | claude:trivial:claude-haiku-*) printf 'haiku RECORD_ONLY' ;;
  # claude:trivial:sonnet→opus removed — trivial tier must not reach above its row (#249 acceptance)
  claude:deep:sonnet | claude:deep:claude-sonnet-*) printf 'sonnet RECORD_ONLY' ;;
  claude:deep:opus | claude:deep:claude-opus-*) printf 'opus RECORD_ONLY' ;;
  codex:standard:gpt-5.6-luna) printf 'luna RECORD_ONLY' ;;
  codex:standard:gpt-5.6-terra) printf 'terra gpt-5.6-sol' ;;
  codex:deep:gpt-5.6-terra) printf 'terra RECORD_ONLY' ;;
  # codex:trivial:luna→terra removed — trivial tier must not reach above its row
  cursor:standard:cursor-grok-4.6-low*) printf 'low RECORD_ONLY' ;;
  cursor:standard:cursor-grok-4.6-medium*) printf 'medium cursor-grok-4.6-high' ;;
  cursor:deep:cursor-grok-4.6-medium*) printf 'medium RECORD_ONLY' ;;
  # cursor:trivial:low→medium removed — trivial tier must not reach above its row
  pi:standard:openrouter/deepseek/deepseek-v4-flash) printf 'v4-flash RECORD_ONLY' ;;
  pi:deep:openrouter/deepseek/deepseek-v4.1-flash) printf 'v4.1-flash RECORD_ONLY' ;;
  # pi:trivial:flash→v4.1-flash removed — trivial tier must not reach above its row
  esac
}

# _prior_failed_model <branch> <crew_dir> <tier> — prints the model of the
# dispatch that the branch's latest terminal worker status (failed/done/pr_open)
# ended, provided that status is `failed` and that dispatch ran at <tier>. The
# dispatch is the failing worker's own session; a resume-only session falls back
# to the latest dispatch before the failure. Prints nothing otherwise.
_prior_failed_model() {
  local branch="$1" dir="$2" tier="$3" events
  events="$dir/events.jsonl"
  [ -f "$events" ] || return 0
  jq -r --arg b "$branch" --arg t "$tier" '
    [., inputs] | . as $all
    | ([$all[] | select(.kind == "status" and
        ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b
        and (.body.state == "failed" or .body.state == "done" or .body.state == "pr_open"))]
        | sort_by(.ts) | last) as $last
    | if $last == null or $last.body.state != "failed" then empty
      else ($last.from | sub("^worker:[^#]*#"; "")) as $sess
      | ($all | map(select(.kind == "dispatch" and .branch == $b and .ts < $last.ts))) as $ds
      | ((($ds | map(select(.session == $sess)) | last)
          // ($ds | sort_by(.ts) | last))) as $d
      | if $d != null and $d.tier == $t then $d.model // empty else empty end
      end
  ' "$events" 2>/dev/null || true
}

# _escalation_model_matches <target> <model> — true when <model> names the
# escalation <target> exactly (claude's target is the alias `opus`, which the
# claude shape gate also accepts as claude-opus-*; cursor takes `-fast`).
_escalation_model_matches() {
  local target="$1" m="$2"
  case "$target" in
  opus) [[ $m =~ ^(opus|claude-opus-.*)$ ]] ;;
  cursor-grok-*) [[ $m =~ ^${target//./\\.}(-fast)?$ ]] ;;
  *) [ "$m" = "$target" ] ;;
  esac
}

# _prior_failed_escalation_available <branch> <crew_dir> — returns 0 if:
# 1. The branch's latest terminal worker status (failed/done/pr_open) is
#    `failed`, AND that worker's session has a matching dispatch or resume
#    event on the same branch (anti-spoofing), AND
# 2. No dispatch or resume event on this branch carries escalated_from, AND no
#    dispatch followed the first failure (an unstamped record-only hop or a
#    same-model retry is an attempt too).
_prior_failed_escalation_available() {
  local branch="$1" dir="$2" events
  events="$dir/events.jsonl"
  [ -f "$events" ] || return 1
  # Check 1: the latest terminal status is a failure posted by a session that
  # was really dispatched (or resumed) on this branch.
  jq -e --arg b "$branch" '
    [., inputs] | . as $all
    | ([$all[] | select((.kind == "dispatch" or .kind == "resume") and .branch == $b) | .session]) as $sessions
    | [$all[] | select(.kind == "status" and .from != null
        and ((.from | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b)
        and (.body.state == "failed" or .body.state == "done" or .body.state == "pr_open"))]
    | sort_by(.ts) | last as $last
    | $last != null and $last.body.state == "failed"
      and ($sessions | index($last.from | sub("^worker:[^#]*#"; ""))) != null
  ' "$events" >/dev/null 2>&1 || return 1
  # Check 2: one-shot. No escalation stamp yet, and no dispatch after the
  # first failure (dispatch rows cover in-row hops, which are never stamped).
  jq -e --arg b "$branch" '
    [., inputs] | . as $all
    | ([$all[] | select(.kind == "status" and ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b
        and .body.state == "failed") | .ts] | min) as $first
    | ([$all[] | select((.kind == "dispatch" or .kind == "resume") and .branch == $b and (.escalated_from // "" | length > 0))] | length) as $stamped
    | ([$all[] | select(.kind == "dispatch" and .branch == $b and .ts > $first)] | length) as $later
    | $stamped == 0 and $later == 0
  ' "$events" >/dev/null 2>&1 || return 1
  return 0
}

# Pre-compute crew_dir for escalation checks (normally set later at line 477).
_escalation_crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"

precheck=(--effort "$effort" --agent "$agent" --crew-id "$crew_id")
[ -n "$ignore_budget" ] && precheck+=(--ignore-budget)
# The tier↔model pair was adjudicated when this worker was first dispatched;
# only an explicit --model is a fresh choice that deserves re-gating.
# Escalation: if a prior session failed and --model is one rung up, allow it.
if [ -z "$model_flag" ] || [ -n "$ignore_map" ]; then
  precheck+=(--ignore-map)
else
  orig_model="$(sed -n 's/^model: //p' "$wt_path/WORKER_TASK.md" | head -1)"
  if [ -n "$orig_model" ] && [ "$orig_model" != "$model" ]; then
    # The header is worker-writable, so the bus must agree it is what failed.
    bus_failed_model="$(_prior_failed_model "$branch" "$_escalation_crew_dir" "$tier")"
    escalation_info=""
    [ "$bus_failed_model" = "$orig_model" ] && escalation_info="$(_escalation_target "$agent" "$tier" "$orig_model")"
    if [ -n "$escalation_info" ]; then
      escalation_target="${escalation_info#* }"
      if [ "$escalation_target" != "RECORD_ONLY" ]; then
        if _escalation_model_matches "$escalation_target" "$model"; then
          if _prior_failed_escalation_available "$branch" "$_escalation_crew_dir"; then
            precheck+=(--ignore-map)
            escalated_from="${escalation_info%% *}"
          fi
        fi
      fi
    fi
  fi
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

# theme_colour <thm-name> <fallback> — a tmux colour expression that prefers
# the theme's #{@thm_<name>} and falls back to <fallback> when tmux-og has not
# set it. Byte-identical to dispatch.sh's copy — a parity test diffs the two
# (a drifted lead label in one path only is the failure mode).
theme_colour() { printf '#{?#{@thm_%s},#{@thm_%s},%s}' "$1" "$1" "$2"; }

# state_glyph <restore> — the state→glyph+colour widget for the lead border.
state_glyph() {
  local restore="$1" c_work c_idle c_block c_done c_fail
  c_work="$(theme_colour green green)"
  c_idle="$(theme_colour overlay_1 colour240)"
  c_block="$(theme_colour peach colour180)"
  c_done="$(theme_colour green green)"
  c_fail="$(theme_colour red red)"
  printf '#{?#{==:#{@crew_state},working},#[fg=%s]●#[fg=%s],#{?#{==:#{@crew_state},idle},#[fg=%s]○#[fg=%s],#{?#{==:#{@crew_state},blocked},#[fg=%s]⚠#[fg=%s],#{?#{==:#{@crew_state},done},#[fg=%s]✓#[fg=%s],#{?#{==:#{@crew_state},pr_open},#[fg=%s]✓#[fg=%s],#{?#{==:#{@crew_state},failed},#[fg=%s]✗#[fg=%s],#{?#{==:#{@crew_state},exited},#[fg=%s]✗#[fg=%s],#[fg=%s]○#[fg=%s]}}}}}}}' \
    "$c_work" "$restore" "$c_idle" "$restore" "$c_block" "$restore" \
    "$c_done" "$restore" "$c_done" "$restore" "$c_fail" "$restore" \
    "$c_fail" "$restore" "$c_idle" "$restore"
}

# grid_lead_format — the lead pane's state-at-a-glance border.
grid_lead_format() {
  printf ' %s #[bold]#{@crew_name}#[nobold] lead · #{@crew_state}#{?#{==:#{@crew_source},watchdog}, (watchdog),}#{?#{@crew_detail}, · #{@crew_detail},} ' "$(state_glyph '#{@crew_color}')"
}

# Identity surfaces. Re-stamped on both paths: a hand-made window carries none,
# and a reused worker window may have been renamed since.
agent_color="$(crew identity "$branch" | jq -r .tmux)"
tmux set-window-option -t "$win" @crew_name "$agent_name"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
# A grid lead keeps @crew_role=lead (the key crew.sh's status-publish guard
# checks) and the state-bearing border; a non-grid worker keeps the plain label.
if [ -n "$grid_roles" ] || [ "$(tmux show-option -wqv -t "$win" @crew_grid 2>/dev/null || true)" = 1 ]; then
  tmux set-option -p -t "$pane" @crew_role lead 2>/dev/null || true
  tmux set-window-option -t "$win" pane-border-format "$(grid_lead_format)"
else
  tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold] "
fi

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
_hdr_set protocol_dir "$PROTOCOL_DIR"

# Ensure WORKER_TASK.md is excluded from tracking across every worktree.
exclude_file="$(git rev-parse --git-common-dir)/info/exclude"
if ! grep -qxF 'WORKER_TASK.md' "$exclude_file" 2>/dev/null; then
  printf '\n%s\n' 'WORKER_TASK.md' >>"$exclude_file"
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
  --arg escalated_from "$escalated_from_event" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"resume", branch:$branch,
     worker_id:$worker, prev_worker_id:$prev, engine:$engine, model:$model,
     session:$session, continued:$continued}
   + if $escalated_from != "" then {escalated_from:$escalated_from} else {} end')
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

protocol_note=" Protocol files (EVIDENCE_REVIEW.md, GRID_PROTOCOL.md, ...) live in $PROTOCOL_DIR — also stamped as protocol_dir: in WORKER_TASK.md."

# Printed so the dispatcher can address this session in the gap before the worker
# boots — its startup drain is unbounded, so a scoping note posted now still lands.
echo "worker_id: $worker_id"

if [ "$agent" = codex ]; then
  cont="resume --last"
  [ -n "$fresh" ] && cont=""
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=$worker_id CREW_ID=$crew_id codex $cont --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default -c agents.enabled=true -c agents.max_concurrent_threads_per_session=3 -c agents.default_subagent_reasoning_effort=$codex_subagent_effort --dangerously-bypass-approvals-and-sandbox 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}${protocol_note}'"
elif [ "$agent" = cursor ]; then
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=$worker_id CREW_ID=$crew_id CURSOR_CLI_INDEXED_GREP=0 cursor-agent $cont --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' 'Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}${protocol_note}'"
elif [ "$agent" = pi ]; then
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=$worker_id CREW_ID=$crew_id PI_CODING_AGENT_DIR=$quoted_pi_dir pi $cont --name $agent_name --model $model --thinking $effort --append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.md --no-approve$(pi_skill_args "$wt_path") 'Read WORKER_TASK.md and continue it.${push_mandate}${plan_note}${reorient}${process_authority}${grid_note}${protocol_note}'"
else
  cont="--continue"
  [ -n "$fresh" ] && cont=""
  # Re-passing --append-system-prompt-file matters on a continue: it forces
  # --system-prompt-snapshot off, so WORKER_PROTOCOL.md is applied fresh rather
  # than replayed from the conversation's recorded prompt.
  launch_cmd="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=$worker_id CREW_ID=$crew_id claude $cont --name $agent_name --model $model --effort $effort $mcp_arg $xreview_mcp --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto 'Read WORKER_TASK.md and continue it.${push_mandate}${plan_note}${reorient}${grid_note}${protocol_note}'"
fi
write_launch_script launch_line "$launch_cmd"
# shellcheck disable=SC2154 # set by write_launch_script's nameref (_launch)
tmux send-keys -t "$pane" "$launch_line" Enter

# Re-arm the stall watchdog: the original self-exited when it saw the terminal
# state, and a resumed worker can wedge exactly the same way.
CREW_ID="$crew_id" nohup crew stall-watch "$worker_id" --pane "$pane" --engine "$agent" >/dev/null 2>&1 &
