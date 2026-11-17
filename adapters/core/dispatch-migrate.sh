# shellcheck shell=bash
# dispatch-migrate — move a running local worker to another configured host:
# quiesce it, carry its branch, uncommitted work, task settings and (claude)
# session transcript, release this machine's claim to that host only, and
# resume it there through `dispatch --host`. Reached as `dispatch migrate`,
# which execs this binary (dispatch.sh intercepts the subcommand). The shebang
# and `set -euo pipefail` are prepended by writeShellApplication.
#
# Exit 0 moved (or already moved there), 1 failed (the message names the way
# on), 2 refused before anything stopped, 3 a cooperative worker did not
# quiesce in time.

# Its own binary on PATH, so dispatch.sh's guard never sees a direct call.
if [ -n "${CREW_OVERSEER:-}" ]; then
  echo "dispatch migrate: the overseer never dispatches; ask that repo's dispatcher with crew overseer msg" >&2
  exit 1
fi

usage() {
  echo "usage: dispatch migrate <target> --host NAME [--now] [--wait S] — see: dispatch migrate --help" >&2
}

# One line per flag: tests/dispatch-migrate.bats derives the flag list from the
# `^  --[a-z-]+\)` arms of the parse loop below and requires each to own a line.
_migrate_help() {
  cat <<'HELP'
usage: dispatch migrate <target> --host NAME [--now] [--wait S]

Move a running local worker to settings host NAME: pause it, carry its branch,
uncommitted work, task settings and (claude) session transcript there, release
this machine's claim to NAME only, and resume it under the same issue and
branch, followed from here like any dispatch --host worker.

  <target>     The worker: '#N' or N, a Linear id, a branch or a codename
  --host NAME  Settings host to move it to (hosts.NAME)
  --now        Stop the worker without asking it to checkpoint first (the
               human is leaving now); its uncommitted work travels as a patch
  --wait S     Seconds a cooperative worker gets to checkpoint and push
               (default 1800)

Exit 0 moved (or already moved to NAME), 1 failed (the message says how to
retry or abandon), 2 refused before anything stopped, 3 the worker did not
quiesce in time. After a failed remote step a re-run retries only that step;
dispatch resume <target> abandons the migration and resumes the worker here.

DISPATCH_MIGRATE_MAX_BYTES (for tests) lowers the one-request budget the
bundle must fit (default 31 MiB).

  dispatch migrate '#42' --host studio
  dispatch migrate feat/42-fix-the-roster-column --host studio --now
HELP
}

# Only a LEADING --help is the help screen (dispatch's #349 first-argument rule).
case "${1:-}" in
--help | -h)
  _migrate_help
  exit 0
  ;;
esac

wt_git_lib="${WORKTREE_GIT_LIB:-@worktreeGitLib@}"
# shellcheck source=/dev/null
. "$wt_git_lib"

launcher_common_lib="${LAUNCHER_COMMON_LIB:-@launcherCommonLib@}"
# replaceStrings rewrites the token on both sides of an equality check, so a
# string compare against the placeholder always matches after bake.
if [ ! -f "$launcher_common_lib" ]; then
  launcher_common_lib="$(dirname -- "${BASH_SOURCE[0]}")/launcher-common.sh"
fi
# shellcheck source=/dev/null
. "$launcher_common_lib"

_die() { # <exit code> <message>
  echo "dispatch migrate: $2" >&2
  exit "$1"
}

target=""
host=""
now=""
wait_s=1800
while [ $# -gt 0 ]; do
  case "$1" in
  --host)
    host="${2:-}"
    [ -n "$host" ] || {
      usage
      exit 2
    }
    shift 2
    ;;
  --now)
    now=1
    shift
    ;;
  --wait)
    wait_s="${2:-}"
    [[ $wait_s =~ ^[1-9][0-9]{0,6}$ ]] || _die 2 "--wait takes a whole number of seconds (got '$wait_s')"
    shift 2
    ;;
  -*)
    usage
    exit 2
    ;;
  *)
    [ -z "$target" ] || {
      usage
      exit 2
    }
    target="$1"
    shift
    ;;
  esac
done
if [ -z "$target" ] || [ -z "$host" ]; then
  usage
  exit 2
fi

_host_re='^[A-Za-z0-9][A-Za-z0-9._-]*$'
[[ $host =~ $_host_re ]] || _die 2 "--host takes a settings host name (got '$host')"
# Under crew remote-run's 32 MiB migrate request limit.
max_bytes="${DISPATCH_MIGRATE_MAX_BYTES:-$((31 << 20))}"
[[ $max_bytes =~ ^[1-9][0-9]{0,10}$ ]] || _die 2 "DISPATCH_MIGRATE_MAX_BYTES takes a whole number of bytes (got '$max_bytes')"

# The pane check mirrors dispatch's _refuse_chrome_from_worker.
if [ -n "${CREW_WORKER_ID:-}" ] || [ -n "${CREW_ROLE_ID:-}" ] ||
  { [ -n "${TMUX_PANE:-}" ] && [ -n "$(tmux display-message -p -t "$TMUX_PANE" '#{@crew_branch}' 2>/dev/null)" ]; }; then
  _die 2 "dispatcher-only — a worker or role pane cannot move a worker"
fi

from_host="$(uname -n)"
from_host="${from_host%%.*}"
[[ $from_host =~ $_host_re ]] || _die 2 "this host's name '$from_host' is not a plain host name"

common="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" ||
  _die 2 "run from inside the repository (any worktree)"
# #633: git and crew never run from a worker's worktree.
_wt_trusted_cwd "$common" || exit 2
crew_dir="$common/crew"
events="$crew_dir/events.jsonl"

lock=""
nudge_pid=""
nudge_log=""
scratch=""
_reap_nudge() {
  local rc=0
  [ -n "$nudge_pid" ] || return 0
  kill "$nudge_pid" 2>/dev/null || true
  wait "$nudge_pid" 2>/dev/null || rc=$?
  nudge_pid=""
  echo "dispatch migrate: crew nudge exit $rc${nudge_log:+: $(tr '\n' ' ' <"$nudge_log")}" >&2
}
_cleanup() {
  _reap_nudge
  if [ -n "$lock" ] && [ "$(readlink -- "$lock" 2>/dev/null)" = "$$" ]; then
    rm -f -- "$lock"
  fi
  [ -z "$scratch" ] || rm -rf -- "$scratch"
}
trap _cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
scratch="$(mktemp -d)"

# The worker the target names, from the dispatcher's own bus rows.
rt_rc=0
row="$(crew resolve-target "$target" 2>"$scratch/resolve.err")" || rt_rc=$?
if [ "$rt_rc" -ne 0 ]; then
  row="$(cat "$scratch/resolve.err")"
  _die 2 "${row#crew: resolve-target: }"
fi
# cut, not read: tab is IFS whitespace, so an empty field would shift the rest.
branch="$(cut -f1 <<<"$row")"
row_host="$(cut -f3 <<<"$row")"
crew_id="$(cut -f4 <<<"$row")"
[[ $crew_id =~ ^[A-Za-z0-9_-]+$ ]] || _die 2 "the bus names crew '$crew_id' for $branch — not a plain crew id"
_wt_plain_branch_name "$branch" && [[ $branch =~ ^[A-Za-z0-9][A-Za-z0-9._/@+-]*$ ]] ||
  _die 2 "the bus names branch '$branch' — not a plain branch name"

# _bus_state — one JSON line about the branch's newest local worker (the
# newest non-remote dispatch row followed through `resume` rows, as dispatch's
# _branch_release_host): its id, its newest status body, and the release mark
# when it is released (crew migrate mark's row is its newest status that is
# neither an `exited` backstop nor a watchdog row).
_bus_state() {
  [ -r "$events" ] || return 0
  jq -cnR --arg b "$branch" '[inputs | fromjson? | objects] as $all
    | [$all[] | select((.kind=="dispatch" or .kind=="resume") and (.branch // "") == $b)] as $rows
    | ([$rows[] | select(.kind=="dispatch" and .remote != true)] | last | .worker_id // "") as $first
    | (reduce ($rows[] | select(.kind=="resume" and (.worker_id | type) == "string" and .worker_id != "")) as $r ($first; if ($r.prev_worker_id // "") == . then $r.worker_id else . end)) as $w
    | [$all[] | select(.kind=="status" and .from == $w and $w != "")] as $st
    | ([$st[] | select(.body.state != "exited" and .body.source != "watchdog")] | last | .body // {}) as $rb
    | {worker: $w, last: ($st | last | .body // {}),
       release: (if $rb.state == "migrated" and $rb.source == "migrate" and ($rb.to_host | type) == "string" then $rb else null end)}' "$events"
}
state="$(_bus_state)"
release_host="$(jq -r '.release.to_host // empty' <<<"${state:-null}")"

if [ "$(cut -f5 <<<"$row")" = 1 ]; then
  if [ "$row_host" = "$host" ]; then
    echo "dispatch migrate: $branch already migrated to host $host"
    exit 0
  fi
  _die 2 "$branch is already on host $row_host — dispatch migrate moves a worker of this host only"
fi

# Two migrations of one branch can never both reach the mark. The lock is the
# dispatch lock's atomic ln -s idiom; a dead holder's lock is not reclaimed.
lock_path="$crew_dir/migrate-$(printf '%s' "$branch" | cksum | cut -d' ' -f1).lock"
if ! ln -s "$$" "$lock_path" 2>/dev/null; then
  held="$(readlink -- "$lock_path" 2>/dev/null || true)"
  if [[ $held =~ ^[0-9]+$ ]] && kill -0 "$held" 2>/dev/null; then
    _die 2 "another dispatch migrate of $branch is running (pid $held) — wait for it"
  fi
  _die 2 "a hard-killed dispatch migrate of $branch left its lock — remove $lock_path and retry"
fi
lock="$lock_path"

# The worktree comes from the dispatcher-written record whose crew dir and
# branch both match, never from a bus path.
cdir="$(realpath -m -- "$crew_dir")"
hits=()
for rec in "${XDG_DATA_HOME:-$HOME/.local/share}"/crew/worktrees/*; do
  [ -f "$rec" ] && [ ! -L "$rec" ] || continue
  mapfile -t _rl <"$rec"
  [ "${_rl[1]:-}" = "$cdir" ] && [ "${_rl[2]:-}" = "$branch" ] && [ -d "${_rl[0]:-}" ] || continue
  hits+=("${_rl[0]}"$'\t'"${_rl[3]:-}")
done
[ "${#hits[@]}" -gt 0 ] || _die 2 "no dispatcher record of a worktree for $branch on this host"
[ "${#hits[@]}" -eq 1 ] || _die 2 "$branch has several recorded worktrees — resolve that first"
wt="${hits[0]%%$'\t'*}"
admin="${hits[0]#*$'\t'}"

task_doc="$wt/WORKER_TASK.md"
[ -f "$task_doc" ] && [ ! -L "$task_doc" ] || _die 2 "no WORKER_TASK.md in $wt"
# The header is everything above the first `## ` section, so a body line that
# looks like a header field never reads as one.
header="$(sed '/^## /,$d' "$task_doc")"
_hdr() { grep -m1 "^$1: " <<<"$header" | cut -d' ' -f2- || true; }

# Worker-writable values: each must be a plain word before it reaches argv.
_require_safe() { # <label> <value> <regex>
  [[ $2 =~ $3 ]] || _die 2 "$branch's task doc has $1 ${2@Q}, not a plain value — fix or re-dispatch it"
}
tier="$(_hdr tier)"
model="$(_hdr model)"
effort="$(_hdr effort)"
agent="$(_hdr engine)"
plan="$(_hdr plan)"
mcp="$(_hdr mcp)"
title="$(_hdr title)"
_require_safe tier "$tier" '^(trivial|standard|deep)$'
_require_safe model "$model" '^[@A-Za-z0-9][][A-Za-z0-9._/:=,-]*$'
_require_safe effort "$effort" '^(low|medium|high|xhigh|max|ultra)$'
_require_safe engine "$agent" '^(claude|codex|cursor|pi)$'
_require_safe plan "$plan" '^(provided|required)$'
[ -z "$mcp" ] || _require_safe mcp "$mcp" '^[A-Za-z0-9][A-Za-z0-9._,-]*$'
[ -n "$title" ] || _die 2 "$branch's task doc has no title"

# What does not exist, or cannot be re-asserted, on another host.
[ "$(_hdr kind)" != review ] || _die 2 "$branch is a review worker (kind: review) — re-dispatch the review on host $host instead"
[ -z "$(_hdr base)" ] || _die 2 "$branch is a stacked layer (base: $(_hdr base)) — dispatch --host cannot carry a base"
add_dirs="$(grep '^add_dir: ' <<<"$header" | cut -d' ' -f2- | paste -sd' ' - || true)"
[ -z "$add_dirs" ] || _die 2 "--add-dir grants do not exist on host $host: $add_dirs"
[ -z "$(_hdr lazy)" ] || _die 2 "$branch has a lazy grid (lazy: 1) — dispatch --host cannot carry --lazy"
[ -z "$(_hdr code_mode)" ] || _die 2 "$branch runs Code Mode (code_mode:) — it is local-only in its trial"
[ -z "$(_hdr profile)" ] || _die 2 "$branch was dispatched in lane profile $(_hdr profile) (profile:), which dispatch --host does not carry"
if grep -qiE '^#+[[:space:]]*owner[[:space:]]+authori[sz]ation' "$task_doc"; then
  _die 2 "$branch's task carries an ## Owner authorization section — the owner's words cannot be re-asserted on host $host by the dispatcher"
fi

# The remote derives the branch from the title and the Closes line; it must be
# this one, or the remote would start a different branch.
mapfile -t closes < <(grep '^Closes ' <<<"$header" | cut -d' ' -f2- || true)
issue="${closes[0]:-}"
slug="$(printf '%s' "$title" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g' | cut -c1-40 | sed -E 's/^-+//; s/-+$//')"
if [[ $issue =~ ^#[0-9]+$ ]]; then
  derived="feat/${issue#\#}-$slug"
elif [[ $issue =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]]; then
  derived="$(printf '%s' "$issue" | tr '[:upper:]' '[:lower:]')-$slug"
else
  _die 2 "$branch's task doc has no 'Closes #N' or Linear Closes line — host $host could not derive its branch"
fi
[ "$derived" = "$branch" ] || _die 2 "host $host would derive branch $derived from the title and Closes line, not $branch"
also_closes=()
for tok in "${closes[@]:1}"; do
  [[ $tok =~ ^#[0-9]+$ || $tok =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || _die 2 "$branch's task doc has 'Closes $tok', not an issue"
  also_closes+=("${tok#\#}")
done

# The grid: each role's recorded spec, else the header's names (the remote then
# uses its default role engines).
roles=""
rj="$crew_dir/artifacts/$branch/roles.json"
if ! _artifacts_dir_bad "$branch" >/dev/null && [ -f "$rj" ] && [ ! -L "$rj" ]; then
  roles="$(jq -r 'to_entries | map("\(.key)=\(.value.agent):\(.value.model)\(if (.value.effort // "") != "" then "@" + .value.effort else "" end)") | join(",")' "$rj" 2>/dev/null)" ||
    _die 2 "$rj is not a roles record"
  _require_safe roles "$roles" '^[A-Za-z0-9][A-Za-z0-9._-]*=(claude|codex|cursor|pi):[A-Za-z0-9][][A-Za-z0-9._/:-]*(@(low|medium|high|xhigh|max|ultra))?(,[A-Za-z0-9][A-Za-z0-9._-]*=(claude|codex|cursor|pi):[A-Za-z0-9][][A-Za-z0-9._/:-]*(@(low|medium|high|xhigh|max|ultra))?)*$'
else
  roles="$(_hdr roles)"
  [ -z "$roles" ] || _require_safe roles "$roles" '^[A-Za-z0-9][A-Za-z0-9._-]*(,[A-Za-z0-9][A-Za-z0-9._-]*)*$'
fi

worker="$(jq -r '.worker // empty' <<<"${state:-null}")"
[ -n "$worker" ] || _die 2 "no local dispatch row names a worker for $branch"
last_state="$(jq -r '.last.state // empty' <<<"$state")"

recovery=""
if [ -n "$release_host" ]; then
  [ "$release_host" = "$host" ] ||
    _die 2 "$branch is marked migrating to host $release_host — retry: dispatch migrate $branch --host $release_host; abandon: dispatch resume $branch"
  recovery=1
else
  case "$last_state" in
  done | reaped) _die 2 "$branch's newest status is $last_state — there is no running worker to move" ;;
  esac
fi

# _engine_live — a lead pane of this branch, at its recorded worktree, runs an engine.
_engine_live() {
  local p_branch p_role p_path p_cmd
  while IFS=$'\x1f' read -r p_branch p_role p_path p_cmd; do
    [ "$p_branch" = "$branch" ] && [ "$p_path" = "$wt" ] || continue
    [ -z "$p_role" ] || [ "$p_role" = lead ] || continue
    crew engine-cmd "$p_cmd" 2>/dev/null && return 0
  done <<PANES
$(tmux list-panes -a -F '#{@crew_branch}'$'\x1f''#{@crew_role}'$'\x1f''#{pane_current_path}'$'\x1f''#{pane_current_command}' 2>/dev/null || true)
PANES
  return 1
}
if [ -z "$recovery" ] && [ -z "$now" ]; then
  case "$last_state" in
  failed | exited | migrated) _die 2 "$branch's newest status is $last_state — a cooperative migration needs a live worker; dispatch migrate --now moves a stopped one" ;;
  esac
  _engine_live || _die 2 "$branch has no live engine pane to ask — dispatch migrate --now moves a stopped worker"
fi

# Preflight, before the worker is touched: the host, its clone and the lead's
# engine, then whether its dispatch knows the remote half at all.
engines="$(crew remote-run --host "$host" --env "CREW_ID=$crew_id" -- dispatch --engines)" ||
  _die 2 "host $host did not answer dispatch --engines (exit $?) — nothing was stopped"
grep -qxF -- "$agent" <<<"$engines" ||
  _die 2 "host $host does not run $agent (engines: $(paste -sd' ' - <<<"$engines")) — nothing was stopped"
mc_rc=0
mc_out="$(crew remote-run --host "$host" --env "CREW_ID=$crew_id" -- dispatch --migrate-check "$branch" 2>"$scratch/migrate-check.err")" || mc_rc=$?
if [ "$mc_rc" -ne 0 ] || [ "$mc_out" != "migrate-check: v1 ok" ]; then
  cat "$scratch/migrate-check.err" >&2
  if grep -q '^migrate-check: ' "$scratch/migrate-check.err"; then
    _die 2 "host $host cannot take $branch (above) — nothing was stopped"
  fi
  _die 2 "host $host runs a dispatch without migrate support — update it first"
fi

stopped_hint="the worker is stopped; nothing was marked — resume it here with: dispatch resume $branch"
oid_re='^[0-9a-f]{40}([0-9a-f]{24})?$'

# _bundle_dir_ok <dir> — a real snapshot dir <crew_dir>/migrate/<branch>/<UTC>,
# where crew migrate snapshot makes one, outside the worker-writable artifacts.
_bundle_dir_ok() {
  [ -d "$1" ] && [ ! -L "$1" ] && [[ $(realpath -m -- "$1") =~ ^"$cdir/migrate/$branch/"[0-9]{8}T[0-9]{6}Z$ ]]
}

# _snapshot — crew migrate snapshot's line into snap_dir/patch/tree/tip.
_snapshot() {
  local snap
  snap="$(crew migrate snapshot --crew "$crew_id" "$branch")" || _die 1 "$stopped_hint"
  snap_dir="$(jq -r .dir <<<"$snap")"
  patch="$(jq -r .patch <<<"$snap")"
  tree="$(jq -r .tree <<<"$snap")"
  tip="$(jq -r .tip <<<"$snap")"
  [[ $tree =~ $oid_re && $tip =~ $oid_re ]] || _die 1 "crew migrate snapshot gave no tree and tip — $stopped_hint"
  _bundle_dir_ok "$snap_dir" ||
    _die 1 "crew migrate snapshot named $snap_dir, not a bundle dir under $crew_dir/migrate/$branch — $stopped_hint"
}

# The task body, verbatim; it rides in the same request as the bundle.
spec_file=""
if grep -qx '## Task' "$task_doc"; then
  spec_file="$scratch/spec.md"
  sed '1,/^## Task$/d' "$task_doc" >"$spec_file"
fi

if [ -n "$recovery" ]; then
  # The mark stands, so the worker was stopped and its bundle built: verify the
  # worktree is still what the mark recorded, then retry the remote step alone.
  mode="$(jq -r '.release.mode // empty' <<<"$state")"
  mark_tip="$(jq -r '.release.tip // empty' <<<"$state")"
  mark_tree="$(jq -r '.release.tree // empty' <<<"$state")"
  mark_patch="$(jq -r '.release.patch // empty' <<<"$state")"
  bundle="$(jq -r '.release.dir // empty' <<<"$state")"
  _snapshot
  # The verifying snapshot's own fresh dir is not the mark's.
  rm -f -- "$snap_dir/patch"
  rmdir -- "$snap_dir" 2>/dev/null || true
  if [ "$tree" != "$mark_tree" ] || [ "$tip" != "$mark_tip" ]; then
    _die 1 "$branch changed since it was marked migrating to $host (tip $tip, tree $tree; the mark has $mark_tip, $mark_tree) — abandon: dispatch resume $branch"
  fi
  patch="$mark_patch"
  _bundle_dir_ok "$bundle" && [ -f "$bundle/meta.json" ] && [ ! -L "$bundle/meta.json" ] ||
    _die 1 "the mark names no migration bundle for $branch ('$bundle') — abandon: dispatch resume $branch"
else
  mode=coop
  [ -z "$now" ] || mode=now
  if [ "$mode" = coop ]; then
    # Ask the worker to checkpoint and post migrated; poll its status against
    # one deadline while the nudge waits out a live turn in the background.
    start_ms="$(($(date +%s%N) / 1000000))"
    crew reply --crew "$crew_id" -- "$worker" "$(jq -nc --arg h "$host" '{directive: "migrate", to_host: $h}')" >/dev/null ||
      _die 1 "could not post the migrate directive to $worker — nothing was stopped"
    nudge_log="$scratch/nudge.log"
    crew nudge "$branch" --crew "$crew_id" --wait "$wait_s" </dev/null >"$nudge_log" 2>&1 &
    nudge_pid=$!
    _coop_outcome() {
      jq -rnR --arg w "$worker" --argjson since "$start_ms" '[inputs | fromjson? | objects
        | select(.kind == "status" and .from == $w and ((.ts | numbers) // 0) >= $since and .body.source != "watchdog")
        | .body | select(.state == "migrated" or .state == "done" or .state == "failed" or .state == "exited"
            or (.state == "blocked" and ((.detail // "") | tostring | startswith("migrate:"))))]
        | first // empty | [.state, (.to_host // ""), ((.detail // "") | tostring)] | @tsv' "$events"
    }
    deadline=$(($(date +%s) + wait_s))
    while outcome="$(_coop_outcome)" && [ -z "$outcome" ] && [ "$(date +%s)" -lt "$deadline" ]; do
      sleep 1
    done
    _reap_nudge
    # Read once more past the deadline: a migrated posted while the nudge
    # waited out a live turn is never missed.
    [ -n "$outcome" ] || outcome="$(_coop_outcome)"
    withdrawn=""
    if [ -z "$outcome" ]; then
      # crew status refuses migrated once the cancel lands, so a migrated row
      # read after it was posted before it, and still stands.
      withdrawn="the migrate directive was withdrawn"
      crew reply --crew "$crew_id" -- "$worker" "$(jq -nc --arg h "$host" '{directive: "migrate-cancel", to_host: $h}')" >/dev/null ||
        withdrawn="withdrawing the migrate directive failed, so it may still be pending"
      outcome="$(_coop_outcome)"
    fi
    o_state="$(cut -f1 <<<"$outcome")"
    o_host="$(cut -f2 <<<"$outcome")"
    o_detail="$(cut -f3- <<<"$outcome")"
    case "$o_state" in
    migrated)
      [ "$o_host" = "$host" ] || _die 1 "$worker posted migrated to host $o_host, not $host — nothing was stopped"
      ;;
    blocked)
      printf '%s\n' "$o_detail" >&2
      _die 1 "$worker could not checkpoint (above); it is untouched and live — answer it, then retry"
      ;;
    "") _die 3 "worker did not quiesce within ${wait_s}s; $withdrawn — ask the human; dispatch migrate --now only if they are leaving now" ;;
    *) _die 1 "$worker posted $o_state${o_detail:+ ($o_detail)} instead of migrating — nothing was moved" ;;
    esac
  fi

  crew migrate stop --crew "$crew_id" "$branch" >&2 || _die 1 "could not stop $branch — nothing was marked; resume it here with: dispatch resume $branch"
  _snapshot

  # A cooperative worker pushed its own checkpoint; --now pushes for it,
  # through the hooks and never forced. Either way origin must hold the tip.
  if [ "$mode" = now ]; then
    _wt_cfg_guard "$common" "$admin" || _die 1 "$stopped_hint"
    _wt_gitlink_ok "$admin" "$wt" || _die 1 "$wt/.git no longer points at $admin — $stopped_hint"
    push_rc=0
    # Hooks run here on purpose (owner requirement: a hook's refusal surfaces
    # verbatim, never bypassed); the one dispatcher-side git call that runs them.
    push_out="$(env -u CREW_ID -u CREW_WORKER_ID -u CREW_ROLE_ID -u TMUX -u TMUX_PANE \
      git --git-dir="$admin" --work-tree="$wt" push origin "refs/heads/$branch:refs/heads/$branch" 2>&1)" || push_rc=$?
    if [ "$push_rc" -ne 0 ]; then
      printf '%s\n' "$push_out" >&2
      _die 1 "$stopped_hint"
    fi
  fi
  origin_tip="$(_wt_git_common "$common" rev-parse --verify --quiet "refs/remotes/origin/$branch")" || origin_tip=""
  [ "$origin_tip" = "$tip" ] || _die 1 "origin/$branch is at ${origin_tip:-nothing}, not the worker's tip $tip — $stopped_hint"

  bundle="$snap_dir"
  # _bundle_put <name> — stdin to <bundle>/<name>; the rename replaces, never
  # follows, whatever is at that name.
  _bundle_put() {
    local tmp
    tmp="$(mktemp "$bundle/.$1.XXXXXX")"
    cat >"$tmp"
    mv -fT -- "$tmp" "$bundle/$1"
  }
  # _b64gz_bytes <file> — what <file> takes in the request: gzipped, then base64.
  _b64gz_bytes() {
    local n
    n="$(gzip -c -- "$1" | wc -c)"
    echo $((4 * ((n + 2) / 3)))
  }

  # Claude only: the lead's recorded session, at claude's key for this worktree.
  session=""
  transcript=""
  transcript_why=""
  lead_rec="$crew_dir/leads/$branch"
  if [ "$agent" != claude ]; then
    transcript_why="the $agent lead has no transcript that travels"
  elif ! _lead_record_safe || [ ! -f "$lead_rec" ]; then
    transcript_why="no lead session record"
  else
    rec_engine="" rec_id="" rec_extra=""
    read -r rec_engine rec_id rec_extra <"$lead_rec" || true
    projects="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects"
    if [ "$rec_engine" != claude ] || [ -n "$rec_extra" ] ||
      ! [[ $rec_id =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]; then
      transcript_why="the lead session record is not a claude session"
    else
      # A symlink would carry whatever it points at to the other host.
      transcript="$projects/${wt//[^A-Za-z0-9]/-}/$rec_id.jsonl"
      if [ ! -f "$transcript" ] || [ -L "$transcript" ]; then
        transcript=""
        for f in "$projects"/*/"$rec_id".jsonl; do
          [ -f "$f" ] && [ ! -L "$f" ] && transcript="$f"
        done
      fi
      if [ -z "$transcript" ]; then
        transcript_why="the lead's claude session has no transcript"
      else
        session="$rec_id"
      fi
    fi
  fi

  # One request carries the patch, the transcript and the task body, each
  # gzipped and base64'd; the slack covers meta.json and the argv.
  req_bytes=$((64 << 10))
  [ -z "$patch" ] || req_bytes=$((req_bytes + $(_b64gz_bytes "$patch")))
  [ -z "$spec_file" ] || req_bytes=$((req_bytes + $(wc -c <"$spec_file")))
  [ "$req_bytes" -le "$max_bytes" ] ||
    _die 1 "bundle too large for one request ($(((req_bytes + (1 << 20) - 1) >> 20)) MiB) — nothing was marked; the worker is stopped — resume it here with: dispatch resume $branch"
  if [ -n "$session" ] && [ $((req_bytes + $(_b64gz_bytes "$transcript"))) -gt "$max_bytes" ]; then
    session=""
    transcript_why="transcript dropped: bundle over budget"
  fi
  [ -z "$session" ] || _bundle_put transcript.jsonl <"$transcript"
  [ -z "$transcript_why" ] || echo "dispatch migrate: no transcript travels: $transcript_why" >&2

  # The handoff: crew-dir data that travels over ssh only. The msgs the old
  # session never pulled go with it; marking them delivered for that session
  # is harmless, it never runs again.
  last="$(_bus_state | jq -c '.last')"
  question="$(jq -rnR --arg w "$worker" '[inputs | fromjson? | objects] as $all
    | ([$all[] | select(.kind == "status" and .from == $w and .body.state == "blocked" and .body.source != "watchdog")] | last) as $q
    | if $q == null then empty
      elif any($all[]; .kind == "msg" and .to == $w and ((.ts | numbers) // 0) > (($q.ts | numbers) // 0)) then empty
      else $q.body.detail // empty | tostring end' "$events")"
  msgs=()
  inbox_failed=""
  if crew inbox "$worker" "$crew_id" --undelivered >"$scratch/inbox.jsonl"; then
    mapfile -t msgs < <(jq -r '"- \(.from // "?") at \(((.ts | numbers) // 0) / 1000 | floor | todate): \(.body | if type == "string" then . else tojson end | gsub("\n"; " "))"' "$scratch/inbox.jsonl")
  else
    inbox_failed=1
    echo "dispatch migrate: could not read the messages $worker never pulled (above) — the handoff says so" >&2
  fi
  worker_note=""
  handoff_md="$crew_dir/artifacts/$branch/handoff.md"
  # Worker-writable: a symlink would pull another file in.
  if [ "$mode" = coop ] && [ -f "$handoff_md" ] && [ ! -L "$handoff_md" ]; then
    worker_note="$(head -c $((32 << 10)) -- "$handoff_md")"
  fi
  _handoff() { # <dropped msg count> <msg lines...>
    local dropped="$1"
    shift
    printf '# Migration handoff\n\n'
    printf -- '- from host: %s\n- mode: %s\n- time: %s\n- tip: %s\n' "$from_host" "$mode" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$tip"
    printf -- '- uncommitted work travels as a patch: %s\n' "$([ -n "$patch" ] && echo yes || echo no)"
    printf -- '- engine transcript: %s\n' "${session:+travels}${transcript_why:+not carried ($transcript_why)}"
    printf -- '- last status: %s\n' "$(jq -r '"\(.state // "none")\(if (.detail // "") != "" then " — \(.detail)" else "" end)"' <<<"$last")"
    [ -z "$question" ] || printf -- '- last unanswered blocked question: %s\n' "$question"
    if [ -n "$inbox_failed" ]; then
      printf '\n## Messages to the old session it never read\n\nmessages could not be read\n'
    elif [ $# -gt 0 ] || [ "$dropped" -gt 0 ]; then
      printf '\n## Messages to the old session it never read\n\n'
      [ "$dropped" -eq 0 ] || printf '(%s older messages dropped to keep this handoff under 64 KiB)\n' "$dropped"
      [ $# -eq 0 ] || printf '%s\n' "$@"
    fi
    [ -z "$worker_note" ] || printf '\n## The worker'"'"'s handoff note\n\n%s\n' "$worker_note"
  }
  dropped=0
  handoff="$(_handoff 0 ${msgs[@]+"${msgs[@]}"})"
  while [ "$(printf '%s' "$handoff" | wc -c)" -gt $((64 << 10)) ] && [ "$dropped" -lt "${#msgs[@]}" ]; do
    dropped=$((dropped + 1))
    handoff="$(_handoff "$dropped" ${msgs[@]+"${msgs[@]:dropped}"})"
  done
  if [ "$(printf '%s' "$handoff" | wc -c)" -gt $((64 << 10)) ]; then
    handoff="$(printf '%s' "$handoff" | head -c $(((64 << 10) - 64)))"$'\n(truncated to 64 KiB)'
  fi
  jq -n --arg b "$branch" --arg t "$tip" --arg f "$from_host" --arg m "$mode" --arg s "$session" --arg h "$handoff" \
    '{v: 1, branch: $b, tip: $t, from_host: $f, mode: $m} + (if $s != "" then {session: $s} else {} end) + {handoff: $h}' |
    _bundle_put meta.json

  crew migrate mark --crew "$crew_id" "$branch" --to-host "$host" --mode "$mode" --tip "$tip" --tree "$tree" --dir "$bundle" ${patch:+--patch "$patch"} >&2 ||
    _die 1 "could not mark $branch released to $host — $stopped_hint"
fi

# The remote resume, through dispatch --host and every guard it keeps, with
# the old header's settings and the task body verbatim.
argv=(dispatch "$tier" "$model" --host "$host" --agent "$agent" --effort "$effort" --plan "$plan" --crew-id "$crew_id")
[ -z "$mcp" ] || argv+=(--mcp "$mcp")
[ -z "$roles" ] || argv+=(--roles "$roles")
[ "$(_hdr chrome)" != true ] || argv+=(--chrome)
for tok in ${also_closes[@]+"${also_closes[@]}"}; do
  argv+=(--also-closes "$tok")
done
argv+=("$issue" -- "$title")
run_env=(DISPATCH_SKIP_SPEC_LINT=1 "DISPATCH_MIGRATE_BUNDLE=$bundle")
[ -z "$spec_file" ] || run_env+=("DISPATCH_SPEC=$spec_file")
[ "$(_hdr draft)" != true ] || run_env+=(DISPATCH_DRAFT_PR=1)
rc=0
env -u DISPATCH_SPEC -u DISPATCH_DRAFT_PR "${run_env[@]}" "${argv[@]}" || rc=$?
[ "$rc" -eq 0 ] ||
  _die 1 "migration to $host failed (exit $rc); the worker is stopped and marked migrating to $host — retry: dispatch migrate $branch --host $host; abandon: dispatch resume $branch"

context="$(jq -rnR --arg b "$branch" --arg h "$host" '[inputs | fromjson? | objects
  | select(.kind == "dispatch" and .remote == true and .branch == $b and .host == $h)] | last // {}
  | if .engine_context == "resumed" then "resumed" else "fresh: \(.engine_context_why // "unknown")" end' "$events")"
echo "dispatch migrate: $branch → host $host (engine context: $context; patch: ${patch:-none})"
