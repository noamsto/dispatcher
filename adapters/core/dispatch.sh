# shellcheck shell=bash
# dispatch — scaffold a worker: issue/ticket -> worktree -> task file -> baked agent.
# Native PATH tool (was a fish autoload function). Crew id is delivered
# explicitly (--crew-id > $CREW_ID > error); a binary can't export env back to
# its caller, so the old `set -gx CREW_ID` persistence trick is gone.
# The shebang + `set -euo pipefail` are prepended by writeShellApplication, so
# this file is only the function body (see crew.sh for the same pattern).

usage() {
  echo -e "usage: dispatch <trivial|standard|deep> <model> --effort <low|medium|high|xhigh|max|ultra> [--agent claude|codex|cursor|pi] [--mcp <profile>] [--grid] [--no-grid] [--roles <r1[=model|agent:model][@effort],...>] [--plan provided|required] [--crew-id <id>] [--base <ref|PR>] [--add-dir DIR]... [--owner-auth TEXT] [--pr N] [--parent N] [--review] [--draft|--no-draft] [--ignore-budget] [--ignore-map] [LINEAR-ID|#N] [--] <title...>\n       dispatch resume [--agent E] [--model M] [--effort E] [--mcp P] [--fresh] [--print] [extra prompt...]" >&2
}

valid_effort() {
  case "$1" in
  low | medium | high | xhigh | max | ultra) return 0 ;;
  *) return 1 ;;
  esac
}

# _canonical_number <token> — the canonical decimal form of a tracker or PR
# token: drop a leading '#', then every leading zero. The empty string comes
# back for an all-zero run, which every caller refuses. Textual rather than
# `10#` arithmetic, which overflows on a pathologically long run.
_canonical_number() {
  local n="${1#\#}"
  while [[ $n == 0* ]]; do n="${n#0}"; done
  printf '%s' "$n"
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
# when its fresh pace window (7d, or pi's month) is materially ahead of pace.
pace_rule_target() {
  local target_agent="$1" target_model="$2" target_effort="$3" model_downgrade="" effort_downgrade="" rung_pct win used_pct ahead pace_notice pace_clause
  [ -z "${ignore_budget:-}" ] && [ -f "$budget_file" ] || return 0
  model_downgrade="$(_pace_downgrade "$target_agent" "$target_model")"
  case "$target_effort" in
  max | xhigh) effort_downgrade="high" ;;
  esac
  [ -n "$model_downgrade$effort_downgrade" ] || return 0
  rung_pct=$(jq -r --arg e "$target_agent" --argjson now "$(date +%s)" '
    def elapsed_pct($w; $len): (100 * ($len - ($w.resets_at - $now)) / $len) as $x
      | if $x < 0 then 0 elif $x > 100 then 100 else $x end;
    def rule($w; $win):
      if $w.used_pct < 70 then empty
      elif $w.resets_at == null then "\($win)|\($w.used_pct)"
      else (if $w.starts_at != null then ($w.resets_at - $w.starts_at) else 604800 end) as $len
        | ($w.used_pct - elapsed_pct($w; $len)) as $ahead
        | if $ahead > 15 then "\($win)|\($w.used_pct)|\($ahead | round)" else empty end
      end;
    if (.fetched_epoch + 7200) < $now then empty
    elif .engines[$e] == null then empty
    elif .engines[$e].windows["7d"] != null then rule(.engines[$e].windows["7d"]; "7d")
    elif .engines[$e].windows.month != null then rule(.engines[$e].windows.month; "month")
    else empty
    end' "$budget_file" 2>/dev/null || true)
  [ -n "$rung_pct" ] || return 0
  win="${rung_pct%%|*}" rung_pct="${rung_pct#*|}"
  used_pct="$rung_pct" pace_notice="" pace_clause=""
  if [[ $rung_pct == *"|"* ]]; then
    used_pct="${rung_pct%%|*}"; ahead="${rung_pct#*|}"
    pace_notice=" ($ahead ahead of pace)"; pace_clause=" and $ahead points ahead of pace"
  fi
  if [ -n "$model_downgrade" ]; then
    if [ "${DISPATCH_IGNORE_RUNG:-}" = "$target_model" ]; then
      echo "dispatch: rung refusal skipped (DISPATCH_IGNORE_RUNG) — '$target_model' on --agent $target_agent at $win ${used_pct}%${pace_notice}" >&2
    else
      echo "dispatch: $target_agent $win is at ${used_pct}%${pace_clause} — the premium rung ($target_model) is refused; use the standard rung ($model_downgrade) instead, set DISPATCH_IGNORE_RUNG=$target_model to override just this refusal, or pass --ignore-budget (the human's spend decision, also disarms the 95% stop). See dispatch-orchestration.md \"Tier map\"." >&2
      exit 1
    fi
  fi
  if [ -n "$effort_downgrade" ]; then
    if [ "${DISPATCH_IGNORE_RUNG:-}" = "$target_effort" ]; then
      echo "dispatch: effort refusal skipped (DISPATCH_IGNORE_RUNG) — '$target_effort' on --agent $target_agent at $win ${used_pct}%${pace_notice}" >&2
    else
      echo "dispatch: $target_agent $win is at ${used_pct}%${pace_clause} — the premium effort ($target_effort) is refused; use $effort_downgrade instead, set DISPATCH_IGNORE_RUNG=$target_effort to override just this refusal, or pass --ignore-budget (the human's spend decision, also disarms the 95% stop). See dispatch-orchestration.md \"Tier map\"." >&2
      exit 1
    fi
  fi
}

# budget_stop <engine> [<role-label>] — refuse an engine whose quota is
# ~exhausted (>=95% of a window that has not reset). With a role-label, the
# refusal names the role. The cache is advisory data from refresh-budget — fail
# open when it is missing, stale (>2h), or silent on this engine ("unknown" is
# never "exhausted"). A window whose resets_at has already passed does not gate
# (the cache can predate the reset); a null resets_at still does. --ignore-budget
# is the manual escape hatch.
budget_stop() {
  local engine="$1" role_label="${2:-}" exhausted now_ts stale_before
  [ -z "${ignore_budget:-}" ] && [ -f "$budget_file" ] || return 0
  now_ts="$(date +%s)"
  stale_before=$((now_ts - 7200))
  exhausted=$(jq -r --arg e "$engine" --argjson stale_before "$stale_before" --argjson now "$now_ts" '
    if .fetched_epoch < $stale_before then empty
    elif .engines[$e] == null then empty
    else .engines[$e].windows | to_entries[]
      | select(.value.used_pct >= 95
               and (.value.resets_at == null or .value.resets_at > $now))
      | "\(.key) at \(.value.used_pct)%\(if .value.resets_at then ", resets \(.value.resets_at | todateiso8601)" else "" end)"
    end' "$budget_file" 2>/dev/null || true)
  [ -n "$exhausted" ] || return 0
  if [ -n "$role_label" ]; then
    echo "dispatch: role '$role_label' ($engine) quota exhausted ($(printf '%s' "$exhausted" | head -1)) — pick another engine, wait for the reset, or pass --ignore-budget" >&2
  else
    echo "dispatch: $engine quota exhausted ($(printf '%s' "$exhausted" | head -1)) — pick another engine, wait for the reset, or pass --ignore-budget" >&2
  fi
  exit 1
}

# _mint_leak_check <body> — run public-leak-guard over a minted issue's body as
# the `gh issue create` it becomes, and refuse on any verdict with the guard's
# reason, so whoever ran dispatch can rewrite the summary. flake.nix bakes the
# guard's store path; a raw run without PUBLIC_LEAK_GUARD skips the check and
# says so.
_mint_leak_check() {
  local guard="${PUBLIC_LEAK_GUARD:-@publicLeakGuard@}" body_file verdict
  if [ ! -r "$guard" ]; then
    echo "dispatch: public-leak guard not found; minted issue body not checked" >&2
    return 0
  fi
  body_file="$(mktemp)"
  printf '%s\n' "$1" >"$body_file"
  verdict="$(jq -nc --arg cmd "gh issue create --body-file $body_file" --arg cwd "$PWD" \
    '{hook_event_name:"PreToolUse",tool_name:"Bash",tool_input:{command:$cmd},cwd:$cwd}' |
    bash "$guard" 2>/dev/null || true)"
  rm -f "$body_file"
  [ -n "$verdict" ] || return 0
  echo "dispatch: refusing to mint — $(printf '%s' "$verdict" | jq -r '.hookSpecificOutput.permissionDecisionReason')" >&2
  exit 1
}

# Ensure the `dispatched` claim-marker label exists. A no-op if it already
# does — must never abort a dispatch on that account.
_ensure_dispatched_label() {
  gh label create dispatched --color 1D76DB \
    --description "Claimed by a dispatcher crew; a worker is on it" >/dev/null 2>&1 || true
}

# Print one line naming the first sign that a `dispatched` claim on <issue> is
# still live, or nothing when it is stale (#304). Always returns 0 and signals
# only via stdout: the caller runs it under `set -e`, and every failure to look
# is itself evidence — an unreachable origin or unreadable bus reads as live.
# Local branches are the caller's business, not checked here.
_claim_evidence() {
  local issue="$1" out rc events="$crew_dir/events.jsonl" pid waited bound outf
  # Bound the remote probe by wall clock (#321). Stock macOS ships no coreutils
  # `timeout`, so the old `timeout 20` guard simply vanished there and a stalled
  # origin could hang the dispatch forever. git's own `http.lowSpeed*` fails the
  # https transport that stalls, and a background watchdog (no coreutils needed)
  # is the transport-agnostic backstop the others cannot outlast. A killed probe
  # is non-zero, read as "origin unreachable" below — the same fail-closed
  # verdict `timeout` gave. Overridable so a test can shrink the window.
  bound="${DISPATCH_CLAIM_LS_REMOTE_TIMEOUT_S:-20}"
  # Read as evidence below, same as an unreachable origin (#557): ls-remote runs
  # sshCommand/credential helper, and _wt_cfg_guard already wrote its reason to
  # stderr.
  if ! _wt_cfg_guard_cwd "${crew_dir%/crew}" >&2; then
    echo "git config drift"
    return 0
  fi
  outf="$(mktemp "${TMPDIR:-/tmp}/dispatch-lsr.XXXXXX")" || {
    echo "origin unreachable"
    return 0
  }
  GIT_TERMINAL_PROMPT=0 \
    git -c http.lowSpeedLimit=1 -c "http.lowSpeedTime=$bound" \
    ls-remote --heads origin "refs/heads/feat/$issue-*" >"$outf" 2>/dev/null &
  pid=$!
  waited=0
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$bound" ]; do
    sleep 1
    waited=$((waited + 1))
  done
  rc=0
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    rc=1
  else
    wait "$pid" || rc=$?
  fi
  out="$(cat "$outf")"
  rm -f "$outf"
  if [ "$rc" -ne 0 ]; then
    echo "origin unreachable"
    return 0
  fi
  if [ -n "$out" ]; then
    out="${out%%$'\n'*}"
    echo "origin branch $(_evidence_text "${out##*refs/heads/}")"
    return 0
  fi
  [ -f "$events" ] || return 0
  # `fromjson? | objects` skips torn and non-object lines instead of failing
  # the whole read.
  out="$(jq -nrR --arg p "feat/$issue-" 'first(inputs | fromjson? | objects | select(.kind=="dispatch" and ((.branch // "") | tostring | startswith($p))) | .branch) // empty' "$events")" || {
    echo "events log unreadable"
    return 0
  }
  if [ -n "$out" ]; then
    echo "dispatch row for $(_evidence_text "$out")"
    return 0
  fi
  # Any claim row's dispatcher may still be between claiming and writing its
  # branch. A pid alone is not proof (#322): the log is never pruned, so a pid
  # the kernel later reused by an unrelated process would false-refuse forever,
  # and `kill -0` on another uid's live dispatcher fails EPERM, which the old
  # check read as dead and healed over. Liveness is cross-uid, the pid must look
  # like the dispatch shell that wrote the row, and it must predate its own row
  # (#351); a row older than `_CLAIM_STALE_MS` is abandoned whatever its pid.
  local pids pid row_ts
  pids="$(jq -nrR --arg i "$issue" 'inputs | fromjson? | objects | select(.kind=="claim-issue" and ((.issue | tostring) == $i)) | "\(.pid) \(.ts // "")"' "$events")" || {
    echo "events log unreadable"
    return 0
  }
  while IFS=' ' read -r pid row_ts; do
    case "$pid" in
    '' | *[!0-9]*) continue ;;
    *[1-9]*) ;;
    *) continue ;;
    esac
    if _claim_pid_live "$pid" "$row_ts"; then
      echo "dispatch in progress (pid $pid)"
      return 0
    fi
  done <<<"$pids"
  return 0
}

# A claim row older than this with no branch, remote branch or dispatch row is
# abandoned (#351). The claim -> dispatch-row window is seconds (reap, fetch,
# worktree); a day is far beyond any legitimate setup, so only a truly stranded
# or forged row reaches it.
_CLAIM_STALE_MS=$((24 * 60 * 60 * 1000))

# _claim_pid_live <pid> <row_ts_ms> — 0 when <pid> is a live process that could
# have written a claim row at <row_ts_ms>, 1 otherwise. Fail-closed: anything it
# cannot read counts as live. Four bounds compose it:
#   - cross-uid liveness: a successful `kill -0` is proof, and so is a `kill -0`
#     that failed EPERM — the signal reached the process and was refused, so it
#     exists; `ps -p` covers the case `kill` reports nothing at all. Both are
#     tried before a pid is called dead, so a foreign-uid claimant stays live
#     even where `ps` cannot see it (a `hidepid=2` mount).
#   - claimant shape: the row's pid is the dispatch shell that wrote it, so its
#     command line must name the harness. A live unrelated process — pid 1, a
#     daemon — that merely predates the row is not claimant evidence; without
#     this a stray or forged row naming one wedges the #304 self-heal (#351).
#   - no pid reuse: the claimant was already running when it wrote the row, so a
#     process that started after the row merely inherited the number.
#   - row age: a row older than _CLAIM_STALE_MS with no branch, remote branch or
#     dispatch row behind it (the caller checked those) is abandoned whatever
#     its pid — the bound for a forged row naming a live dispatch process.
_claim_pid_live() {
  local pid="$1" row_ts="$2" elapsed start_s kmsg args
  kmsg="$(LC_ALL=C kill -0 "$pid" 2>&1)" || {
    case "$kmsg" in
    *"not permitted"* | *"not allowed"*) ;; # EPERM: the process exists
    *) ps -p "$pid" -o pid= >/dev/null 2>&1 || return 1 ;;
    esac
  }
  # Claimant shape: only the dispatch shell that wrote the row counts. Readable
  # args that name something else (pid 1, a daemon) are not a claimant; args we
  # cannot read fail closed and stay plausible. -ww defeats COLUMNS truncation:
  # the built wrapper's `dispatch` token sits past column 80 behind the bash
  # store path, and a truncated read would call a live claimant stale.
  args="$(ps -ww -o args= -p "$pid" 2>/dev/null)" || args=""
  if [ -n "$args" ]; then
    case "$args" in
    *dispatch*) ;;
    *) return 1 ;;
    esac
  fi
  case "$row_ts" in
  '' | *[!0-9]*) return 0 ;; # no usable ts: reuse can't be ruled out
  esac
  # Age: a day-old row with nothing behind it is abandoned whatever the pid.
  if [ "$((10#$row_ts))" -lt "$(($(date +%s) * 1000 - _CLAIM_STALE_MS))" ]; then
    return 1
  fi
  elapsed="$(_ps_elapsed_s "$pid")" || return 0
  # 10# forces decimal: a row ts of "08" must not trip bash's octal parse.
  start_s=$(($(date +%s) - 10#$elapsed))
  # 2s of slack absorbs ps' second granularity, so a same-second claimant isn't
  # mistaken for a reuse.
  if [ "$((start_s * 1000))" -gt "$((10#$row_ts + 2000))" ]; then
    return 1
  fi
  return 0
}

# _ps_elapsed_s <pid> — the process's elapsed seconds, or return 1 when ps can't
# say. `etimes` is exact where the platform has it; the `etime` fallback parses
# the [[dd-]hh:]mm:ss macOS prints. Both are locale- and timezone-independent.
_ps_elapsed_s() {
  local pid="$1" out d h m s rest
  out="$(ps -o etimes= -p "$pid" 2>/dev/null | tr -d '[:space:]')"
  case "$out" in
  '' | *[!0-9]*) ;;
  *) printf '%s' "$out"; return 0 ;;
  esac
  out="$(ps -o etime= -p "$pid" 2>/dev/null | tr -d '[:space:]')"
  case "$out" in
  '' | *[!0-9:-]*) return 1 ;;
  esac
  d=0
  case "$out" in
  *-*) d="${out%%-*}"; out="${out#*-}" ;;
  esac
  [ -n "$d" ] || d=0
  case "$out" in
  *:*:*) h="${out%%:*}"; rest="${out#*:}"; m="${rest%%:*}"; s="${rest#*:}" ;;
  *:*) h=0; m="${out%%:*}"; s="${out#*:}" ;;
  *) h=0; m=0; s="$out" ;;
  esac
  printf '%s' "$((10#$d * 86400 + 10#$h * 3600 + 10#$m * 60 + 10#$s))"
}

# Branch names in evidence text come from the remote or the bus: strip control
# characters and cap the length before they reach a terminal.
_evidence_text() { printf '%s' "$1" | tr -cd '[:print:]' | cut -c1-120; }

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
_bus_append() {
  local p='' s1='' s2='' c=''
  if [ -s "$1" ]; then
    s1="$(wc -c <"$1" 2>/dev/null)" || true
    c="$(dd if="$1" bs=1 skip=$((s1 - 1)) count=1 2>/dev/null)" || true
    if [ -n "$c" ]; then
      s2="$(wc -c <"$1" 2>/dev/null)" || true
      [ "$s1" != "$s2" ] || p=$'\n'
    fi
  fi
  printf '%s%s\n' "$p" "$2" | dd bs=1048576 iflag=fullblock status=none >>"$1"
}

# _fetch_origin_branch <name> — fetch an untrusted (gh/stamp-derived) branch
# name into refs/remotes/origin/<name> only, always in the dispatcher's own
# repo. A bare positional would parse the name as a refspec
# (`+refs/heads/x:refs/remotes/origin/main` force-updates origin/main) or a
# fetch option (`--upload-pack=...`), so it must be a plain branch name and is
# spelled as an explicit refspec. Never run it in a worker's worktree: fetch
# honours that gitdir's config (#539), and refs/remotes are shared anyway.
_plain_branch_name() {
  [[ $1 == *:* || $1 == +* ]] && return 1
  git check-ref-format --branch "$1" >/dev/null
}
_fetch_origin_branch() {
  local name=$1
  # fetch runs sshCommand/credential helper/reference-transaction hooks (#557).
  _wt_cfg_guard_cwd "${crew_dir%/crew}" || return 1
  _plain_branch_name "$name" || return 1
  git fetch origin "+refs/heads/$name:refs/remotes/origin/$name"
}

# Unconditional, unlike the advisory hint lib: without it dispatch must abort,
# never fall back to discovery in a worker's worktree (#539).
wt_git_lib="${WORKTREE_GIT_LIB:-@worktreeGitLib@}"
# shellcheck source=/dev/null
. "$wt_git_lib"

# ssh Host aliases are written github.com-<name>. Anything else (a lookalike
# host, an extra @, a slash) is not GitHub.
_github_ssh_host() {
  local host
  host="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  [ "$host" = github.com ] && return 0
  [[ $host =~ ^github\.com-[^@/]+$ ]]
}

# owner/repo from a GitHub path. A trailing slash may sit before or after
# `.git` (`repo/.git`, `repo.git/`, `repo/`). Empty when it is not one slug.
_tracker_slug_from_path() {
  local path="$1"
  while [[ $path == */ ]]; do
    path="${path%/}"
  done
  if [[ $path == *.git ]]; then
    path="${path%.git}"
    while [[ $path == */ ]]; do
      path="${path%/}"
    done
  fi
  if [[ $path =~ ^[^/[:space:]]+/[^/[:space:]]+$ ]]; then
    printf '%s\n' "$path"
  fi
  return 0
}

# Configured origin, not `git remote get-url`: that applies insteadOf and
# would hide a GitHub slug behind a rewritten local path. https userinfo is
# discarded and never printed.
_resolve_tracker() {
  local url="" rest="" slug="" org="" want="" val="" which
  local host="" path="" want_cmp=""
  url="$(git config --get remote.origin.url 2>/dev/null || true)"
  case "$url" in
  https://*)
    rest="${url#https://}"
    rest="${rest##*@}"
    host="${rest%%/*}"
    host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
    if [ "$host" = github.com ] && [ "$rest" != "$host" ]; then
      path="${rest#*/}"
    fi
    ;;
  ssh://git@*/*)
    rest="${url#ssh://git@}"
    host="${rest%%/*}"
    if _github_ssh_host "$host"; then
      path="${rest#*/}"
    fi
    ;;
  git://*/*)
    rest="${url#git://}"
    host="${rest%%/*}"
    host="$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')"
    if [ "$host" = github.com ]; then
      path="${rest#*/}"
    fi
    ;;
  git@*:*)
    rest="${url#git@}"
    host="${rest%%:*}"
    if _github_ssh_host "$host"; then
      path="${rest#*:}"
    fi
    ;;
  *) path="" ;;
  esac
  if [ -n "$path" ]; then
    slug="$(_tracker_slug_from_path "$path")"
  fi
  if [ -n "$slug" ]; then
    org="${slug%%/*}"
    for which in repo org; do
      if [ "$which" = repo ]; then
        want="$slug"
      else
        want="$org"
      fi
      want_cmp="$(printf '%s' "$want" | tr '[:upper:]' '[:lower:]')"
      val="$(jq -r --arg m "${which}Trackers" --arg k "$want_cmp" '.[$m][$k] // empty' <<<"$settings")"
      [ -n "$val" ] || continue
      if [ "$val" = github ]; then
        printf '%s\n' github
        return 0
      fi
      if [[ $val =~ ^linear:([A-Z][A-Z0-9]*)$ ]]; then
        printf 'linear %s\n' "${BASH_REMATCH[1]}"
        return 0
      fi
    done
  fi
  printf '%s\n' github
}

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
  # The dirs are spliced into launch scripts and their prompts, and a role
  # launched by --spawn-role inherits this env from the worker (#470).
  if [[ ! $val =~ ^/[A-Za-z0-9._/+@-]*$ ]]; then
    echo "$label: $var must not contain shell metacharacters or spaces, got: ${val@Q}" >&2
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

# Protocol directory. The env override is the dev loop: point it at a checkout
# and protocol edits take effect on the next dispatch with no rebuild. The
# default is substituted to a store path at build time.
_resolve_dir PROTOCOL_DIR DISPATCHER_PROTOCOL_DIR "@protocolDir@" dispatch
# Absolute: role panes run it minutes later from the worktree, not from this cwd.
dispatch_self="$(realpath -- "$0")"
budget_file="${XDG_DATA_HOME:-$HOME/.local/share}/crew/engine-budget.json"

# Harness skill directory, handed to pi workers via --skill. Same env-override
# dev loop as PROTOCOL_DIR, same build-time store-path default. Unsubstituted
# (a non-Nix install) it is not a directory, and pi_skill_args' probe drops it.
_resolve_dir SKILLS_DIR DISPATCHER_SKILLS_DIR "@skillsDir@" dispatch

# Reviewer and critic markdown directories, handed to the launched session
# explicitly (write_launch_script) so it never reads a stale export left in the
# tmux server's environment.
_resolve_dir REVIEWERS_DIR DISPATCHER_REVIEWERS_DIR "@reviewersDir@" dispatch
_resolve_dir CRITICS_DIR DISPATCHER_CRITICS_DIR "@criticsDir@" dispatch

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

# _settings_load — resolve the settings (dispatch-config) into $settings. A
# set env var is kept verbatim, so a whitespace-only DISPATCH_ENGINES still
# enables nothing; the resolver fills only what env leaves empty, unexported.
_settings_load() {
  settings="$("${DISPATCH_CONFIG_BIN:-@dispatchConfig@}")"
  [ -n "${DISPATCH_ENGINES:-}" ] || DISPATCH_ENGINES="$(jq -r '.engines // [] | join(" ")' <<<"$settings")"
  [ -n "${DISPATCH_GRANT_ROOTS:-}" ] || DISPATCH_GRANT_ROOTS="$(jq -r '.grantRoots // [] | join(":")' <<<"$settings")"
}

# _glob_match <model> — true when <model> matches one of the bash globs on
# stdin, one per line.
_glob_match() {
  local glob
  while IFS= read -r glob; do
    # shellcheck disable=SC2053 # the unquoted RHS is the glob
    if [[ $1 == $glob ]]; then return 0; fi
  done
  return 1
}

# _exact_match <model> — true when <model> equals one of the lines on stdin,
# one per line.
_exact_match() {
  local x
  while IFS= read -r x; do
    if [ "$x" = "$1" ]; then return 0; fi
  done
  return 1
}

# _model_in_row <agent> <tier> <model> — true when modelMap's row admits
# <model> by a `models` glob or a `regex` ERE; a missing row admits nothing.
_model_in_row() {
  local kind pat
  while IFS=$'\t' read -r kind pat; do
    if [ "$kind" = ere ]; then
      if [[ $3 =~ $pat ]]; then return 0; fi
    else
      # shellcheck disable=SC2053 # the unquoted RHS is the glob
      if [[ $3 == $pat ]]; then return 0; fi
    fi
  done < <(jq -r --arg a "$1" --arg t "$2" '.modelMap[$a][$t] // {} | ("glob\t" + (.models // [])[]), ("ere\t" + (.regex // [])[])' <<<"$settings")
  return 1
}

# _row_expected <agent> <tier> — the row's "expected …" text for the refusal.
_row_expected() {
  jq -r --arg a "$1" --arg t "$2" '.modelMap[$a][$t].expected // ""' <<<"$settings"
}

# _escalation_hop <agent> <tier> <failed> <model> <inRow|outOfRow> — the
# baseline of the first escalation rule whose `failed` globs match <failed>,
# when that rule's <kind> list admits <model> (inRow: a glob; outOfRow: an
# exact id). Prints nothing otherwise.
_escalation_hop() {
  local rule="" i glob baseline
  while IFS=$'\t' read -r i glob; do
    # shellcheck disable=SC2053 # the unquoted RHS is the glob
    if [[ $3 == $glob ]]; then
      rule="$i"
      break
    fi
  done < <(jq -r --arg a "$1" --arg t "$2" '.escalation[$a][$t] // [] | to_entries[] | "\(.key)\t\(.value.failed[])"' <<<"$settings")
  [ -n "$rule" ] || return 0
  if {
    read -r baseline
    if [ "$5" = inRow ]; then _glob_match "$4"; else _exact_match "$4"; fi
  } < <(jq -r --arg a "$1" --arg t "$2" --argjson i "$rule" --arg k "$5" '.escalation[$a][$t][$i] | .baseline, (.[$k] // [])[]' <<<"$settings"); then
    printf '%s' "$baseline"
  fi
}

# _pace_downgrade <agent> <model> — the `to` of the first paceDowngrades entry
# whose `models` globs match <model>; prints nothing when none does.
_pace_downgrade() {
  local to glob
  while IFS=$'\t' read -r to glob; do
    # shellcheck disable=SC2053 # the unquoted RHS is the glob
    if [[ $2 == $glob ]]; then
      printf '%s' "$to"
      return 0
    fi
  done < <(jq -r --arg a "$1" '.paceDowngrades[$a] // [] | .[] | "\(.to)\t\(.models[])"' <<<"$settings")
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
# non-dotglob glob would diverge. A stale store path held by a long-lived
# DISPATCHER_PROTOCOL_DIR export is ignored by _resolve_dir before this check;
# what is refused here is a checkout whose content drifted from the script's
# build — it hashes differently. There is no committed PROTOCOL_REV file left
# to go stale, so two PRs editing different protocol files can merge in either
# order. Six small files hash in a few milliseconds. A raw checkout script
# (marker unsubstituted) cannot bind a revision and skips with a one-line
# warning.
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
    if [ -n "${DISPATCHER_PROTOCOL_DIR:-}" ]; then
      echo "$label:   DISPATCHER_PROTOCOL_DIR override: $DISPATCHER_PROTOCOL_DIR" >&2
      echo "$label:   remedy: unset DISPATCHER_PROTOCOL_DIR, or point it at a checkout matching this build" >&2
    fi
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

# theme_colour <thm-name> <fallback> — a tmux colour expression that prefers
# the theme's #{@thm_<name>} and falls back to <fallback> when tmux-og has not
# set it (tmux expands an unset option to empty, so `fg=` would be dropped
# silently and the glyph would inherit whatever colour preceded it).
theme_colour() { printf '#{?#{@thm_%s},#{@thm_%s},%s}' "$1" "$1" "$2"; }

# state_glyph <restore> — the state→glyph+colour widget shared by the lead's
# window border and every role pane's border. <restore> is re-emitted after the
# glyph so the rest of the label keeps the border's own tint. tmux evaluates
# the #{?} branches at render time, so @crew_state can change without the
# format being re-set. Unknown/empty state renders the idle glyph.
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

# grid_lead_format — the lead pane's border, state at a glance. @crew_name stays
# the bare codename (it is the occupancy join key); the marker, glyph, state and
# phase are painted only here. A watchdog `blocked` renders `blocked (watchdog)`
# so it is distinct from a worker's own blocked (a live question).
grid_lead_format() {
  printf ' %s #[bold]#{@crew_name}#[nobold] lead · #{@crew_state}#{?#{==:#{@crew_source},watchdog}, (watchdog),}#{?#{@crew_detail}, · #{@crew_detail},} ' "$(state_glyph '#{@crew_color}')"
}

# publish_grid_window <window> — the grid-hint contract's window half: @crew_grid
# marks the window as a grid (tmux-og's tmux-grid-refit honours it) and
# @crew_grid_main_pct is the lead's default share. Best-effort, so a stub or a
# vanished window never fails a dispatch.
publish_grid_window() {
  local win="$1"
  tmux set-window-option -t "$win" @crew_grid 1 2>/dev/null || true
  tmux set-window-option -t "$win" @crew_grid_main_pct 60 2>/dev/null || true
}

# publish_grid_lead <window> <pane> — @crew_role=lead on the lead pane plus the
# rich lead border format. Idempotent, and it deliberately does NOT touch
# @crew_state: a lazy re-spawn must not clobber a live blocked lead.
publish_grid_lead() {
  local win="$1" pane="$2"
  tmux set-option -p -t "$pane" @crew_role lead 2>/dev/null || true
  tmux set-window-option -t "$win" pane-border-format "$(grid_lead_format)" 2>/dev/null || true
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
  tmux set-option -p -t "$pane" pane-border-format " $(state_glyph '#{@crew_role_color}') #[bold]#{@crew_role}#[nobold] #{@crew_state} "
  tmux set-option -w -t "$pane" pane-border-status top
}

# layout_grid <window> — main-vertical, pinning the lead (pane 1, launched
# before any role pane splits off it) to 60% width. Role panes only carry
# short verdict traffic and need far less room than the lead's diff/test/tool
# output. The built-in fallback for a host without tmux-og's tmux-grid-refit.
layout_grid() {
  local win="$1"
  tmux set-window-option -t "$win" main-pane-width 60%
  tmux select-layout -t "$win" main-vertical
}

# refit_grid <window> — hand the responsive layout to tmux-og when its
# tmux-grid-refit is installed (the grid-hint contract), else keep the built-in
# main-vertical 60% fallback. A missing or failing refit is not an error.
refit_grid() {
  local win="$1"
  if command -v tmux-grid-refit >/dev/null 2>&1; then
    tmux-grid-refit "$win" 2>/dev/null || true
  else
    layout_grid "$win"
  fi
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

# _pane_is_ancestor <pane> — is <pane>'s pid one of this process's ancestors?
# A worker's own dispatch descends from its pane's shell; a pane id copied from
# another window does not. Bounded walk, spelling copied from crew.sh's
# _is_ancestor_pid. Assumes the engine's tool shell shares tmux's pid
# namespace — a pid-namespaced sandbox makes this refuse (fail closed).
# Only a concrete %id: a relative target (`@5.{bottom-right}`) re-resolves to
# another pane after this check.
_pane_is_ancestor() {
  local pane_pid p depth=0
  [[ $1 =~ ^%[0-9]+$ ]] || return 1
  pane_pid="$(tmux display-message -p -t "$1" '#{pane_pid}' 2>/dev/null || true)"
  case "$pane_pid" in '' | *[!0-9]*) return 1 ;; esac
  p=$$
  while [ "$depth" -lt 32 ]; do
    depth=$((depth + 1))
    p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d '[:space:]' || true)
    case "$p" in '' | *[!0-9]* | 0) return 1 ;; esac
    if [ "$p" = "$pane_pid" ]; then return 0; fi
  done
  return 1
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
  pane="$(tmux split-window -t "$win" -c "$wt" -e "CREW_WORKER_ID=$worker_id" -e "CREW_ID=$crew_id" -e "CREW_ROLE_ID=role:$branch:$role" -e "GIT_EDITOR=true" -e "GIT_SEQUENCE_EDITOR=:" -P -F '#{pane_id}')"
  decorate_pane "$pane" "$role"
  printf '%s' "$pane"
}

# _uuid — a random lowercase v4 uuid, the id a claude or pi lead is launched
# with (--session-id) so a resume can find its own session again. Duplicated in
# dispatch-resume.sh (standalone build); parity-tested.
_uuid() {
  local h
  h="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s-%s-4%s-%x%s-%s\n' "${h:0:8}" "${h:8:4}" "${h:13:3}" $(((0x${h:16:1} & 3) | 8)) "${h:17:3}" "${h:20:12}"
}

# _lead_record_safe — succeed when $crew_dir/leads/<branch> and every dir above
# it may be written: mkdir, mktemp and mv all follow a symlink planted at any of
# them. Mirrors the grant-record checks in the dispatch path.
_lead_record_safe() {
  local dir="$crew_dir/leads" part rec="$crew_dir/leads/$branch"
  local -a parts
  IFS=/ read -ra parts <<<"$branch"
  for part in "" "${parts[@]:0:${#parts[@]}-1}"; do
    dir="$dir${part:+/$part}"
    if [ -L "$dir" ] || { [ -e "$dir" ] && [ ! -d "$dir" ]; }; then
      return 1
    fi
  done
  if [ -L "$rec" ] || { [ -e "$rec" ] && [ ! -f "$rec" ]; }; then
    return 1
  fi
}

# _record_lead_session <engine> <id> — record which engine session is the
# lead's, as `<engine> <id>` in $crew_dir/leads/<branch>. Not under artifacts/,
# which is granted to workers. Only a lead launch writes it: role panes share
# the worktree and must never claim the lead's session.
_record_lead_session() {
  local rec="$crew_dir/leads/$branch" tmp
  if ! _lead_record_safe; then
    echo "dispatch: $rec is a symlink or not a regular file — not recording the lead session" >&2
    return 1
  fi
  (
    umask 077
    mkdir -p "$(dirname "$rec")"
    tmp="$(mktemp "$(dirname "$rec")/.lead.XXXXXX")"
    printf '%s %s\n' "$1" "$2" >"$tmp"
    mv -f -- "$tmp" "$rec"
  )
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
    \\) _res+="'\\\\'" ;;
    *) _res+="$_c" ;;
    esac
  done
  _out="'$_res'"
}

# write_launch_script <var> <cmdline> [exit] — write <cmdline> into a fresh 0700
# script under $crew_dir/launch and set <var> to the short line that runs it. A
# new pane's shell is often not reading yet when send-keys types into it, and
# the tty's canonical buffer (1024 bytes on macOS) cuts a longer line and drops
# its Enter (#298) — so a pane is only ever typed `bash <path>`. `exec env`
# makes the engine the pane's foreground process itself, which every
# #{pane_current_command} liveness check reads. Files older than a week are
# pruned on the way in.
#
# With `exit` the script is named exit.* and deletes itself when it runs: a
# role's exit hook runs only when its engine returns, possibly weeks later, so
# the age prune never reaches it while its pane is alive. A reaped pane never
# runs the hook, so an old exit.* whose pane is gone is reclaimed instead
# (issue #343).
#
# The launching process's resolved DISPATCH_GRANT_ROOTS is always pinned — `:`
# when empty, which every reader takes as no roots — so a lead's
# `dispatch --spawn-role` validates grants against the roots its dispatcher
# used, never re-resolving them from the pane's (tmux server's) env or locked
# layer (#572).
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
    printf -v _dirs '%sDISPATCH_GRANT_ROOTS=%q ' "$_dirs" "${DISPATCH_GRANT_ROOTS:-:}"
    _file="$(mktemp "$_dir/launch.XXXXXX")"
    printf '#!/usr/bin/env bash\nexec env %s%s%s\n' "$_unset" "$_dirs" "$2" >"$_file"
  fi
  chmod 700 "$_file"
  shell_quote _quoted "$_file"
  _launch="bash $_quoted"
}

# _symlink_chain_hops <path> — print, NUL-terminated, every symlink's own
# discovered location while walking <path> component by component (not
# `realpath`: a raw component-at-a-time walk, so a `..` in a target is
# resolved against whatever is *actually* resolved so far, even when that
# required following a symlink first), every directory a `..` backs out of,
# and the fully resolved path last. <path> need not exist past its last real
# component. Paths and link targets are split and captured NUL-safely, so a
# component holding a literal newline stays one component. Fails closed
# (silent, nonzero) on a readlink failure or past a bounded hop budget (a
# symlink cycle) — a caller that used a partial hop list on failure would be
# less safe than refusing outright. A component this user cannot search
# reads as "not a symlink" and is walked past textually, same as it would be
# for anyone without search access to it.
_symlink_chain_hops() {
  local resolved="" comp target budget=40
  local -a queue tcomps
  IFS=/ read -r -d '' -a queue < <(printf '%s\0' "${1#/}")
  while [ "${#queue[@]}" -gt 0 ]; do
    comp="${queue[0]}"
    queue=("${queue[@]:1}")
    case "$comp" in
    '' | '.') continue ;;
    '..')
      [ -z "$resolved" ] || printf '%s\0' "$resolved"
      resolved="${resolved%/*}"
      continue
      ;;
    esac
    if [ -L "$resolved/$comp" ]; then
      budget=$((budget - 1))
      [ "$budget" -gt 0 ] || return 1
      printf '%s\0' "$resolved/$comp"
      # the x sentinel keeps a target's trailing newline from $(...) stripping
      target="$(readlink -n -- "$resolved/$comp" && printf x)" || return 1
      target="${target%x}"
      [[ $target == /* ]] || target="$resolved/$target"
      resolved=""
      IFS=/ read -r -d '' -a tcomps < <(printf '%s\0' "${target#/}")
      queue=("${tcomps[@]}" "${queue[@]}")
    else
      resolved="$resolved/$comp"
    fi
  done
  printf '%s\0' "$resolved"
}

# _git_config_files <dir> — print, NUL-terminated, every config file git reads
# for <dir>, as git spells its origin (a relative origin joined to <dir>;
# non-file origins skipped), then every include.path/includeIf.*.path target
# whether or not it exists, since a worker could create it (a relative target
# joined to its including file's dir). Env overrides are dropped as in
# _git_protected_dirs. Fails closed, printing why.
_git_config_files() {
  local a="$1" f last="" kv v tf rc
  tf=$(mktemp) || return 1
  if ! env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
    git -C "$a" config --show-origin -z --list >"$tf"; then
    rm -f "$tf"
    printf >&2 'dispatch: git cannot list the config files of %s; refusing the grant\n' "$a"
    return 1
  fi
  # -z output: an origin record, then a key<newline>value record
  while IFS= read -r -d '' f && IFS= read -r -d '' kv; do
    [[ $f == file:* && $f != "$last" ]] || continue
    last="$f"
    f="${f#file:}"
    [[ $f == /* ]] || f="$a/$f"
    printf '%s\0' "$f"
  done <"$tf"
  rc=0
  env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
    git -C "$a" config --show-origin -z --path --get-regexp '^include(if\..*)?\.path$' >"$tf" || rc=$?
  # exit 1: no include is set
  if [ "$rc" -gt 1 ]; then
    rm -f "$tf"
    printf >&2 'dispatch: git cannot list the config includes of %s; refusing the grant\n' "$a"
    return 1
  fi
  while IFS= read -r -d '' f && IFS= read -r -d '' kv; do
    [[ $f == file:* ]] || continue
    f="${f#file:}"
    [[ $f == /* ]] || f="$a/$f"
    v="${kv#*$'\n'}"
    [[ $v == /* ]] || v="${f%/*}/$v"
    printf '%s\0' "$v"
  done <"$tf"
  rm -f "$tf"
}

# _git_protected_dirs <path> — print, NUL-terminated, the hooks dir, git dir,
# common dir and .git entry of every repo whose worktree contains <path>, a
# gitfile's gitdir: and a git dir's commondir as spelled, and the config
# files and include targets git reads there, as git spells them (see
# _git_config_files); then the global core.hooksPath when it is absolute,
# the global and system config files and include targets, and the global
# config candidates git reads when present, since a grant could create a
# missing one. A grant overlapping any of these is a grant on some repo's
# hooks or config: Husky's .husky via core.hooksPath, a submodule's git dir
# under .git/modules, an included file a worker could fill with
# core.hooksPath. git only reads config here, and the caller's repo-location,
# GIT_CONFIG and -c env overrides are dropped so the answer comes from the
# human's own config. Fails closed, printing why.
_git_protected_dirs() {
  local a="$1" out gd rc
  local -a lines
  while :; do
    if [ -e "$a/.git" ] || [ -L "$a/.git" ]; then
      # the x sentinel keeps a path's trailing newline from $(...) stripping
      out="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
        git -C "$a" rev-parse --git-path hooks --git-dir --git-common-dir && printf x)" || {
        printf >&2 'dispatch: git cannot resolve the hooks and git dirs of %s; refusing the grant\n' "$a"
        return 1
      }
      out="${out%x}"
      out="${out%$'\n'}"
      mapfile -t lines <<<"$out"
      # a path holding a newline splits into extra lines
      if [ "${#lines[@]}" -ne 3 ]; then
        printf >&2 'dispatch: cannot parse the hooks and git dirs of %s; refusing the grant\n' "$a"
        return 1
      fi
      # relative to $a, and spelled as configured: an absolute format would
      # resolve the symlinks the caller must walk hop by hop
      for out in "${lines[@]}"; do
        [[ $out == /* ]] || out="$a/$out"
        printf '%s\0' "$out"
      done
      printf '%s\0' "$a/.git"
      # git prints both dirs resolved; walk a gitfile's gitdir: and the
      # commondir as spelled so a link on either path is a hop
      gd="$a/.git"
      if [ -f "$a/.git" ]; then
        IFS= read -r -d '' out <"$a/.git" || :
        while [[ $out == *[$'\r\n'] ]]; do out="${out%?}"; done
        out="${out#gitdir: }"
        [[ $out == /* ]] || out="$a/$out"
        printf '%s\0' "$out"
        gd="$out"
      fi
      if [ -f "$gd/commondir" ]; then
        IFS= read -r -d '' out <"$gd/commondir" || :
        while [[ $out == *[$'\r\n'] ]]; do out="${out%?}"; done
        [[ $out == /* ]] || out="$gd/$out"
        printf '%s\0' "$out"
      fi
      _git_config_files "$a" || return 1
    fi
    [ "$a" != / ] || break
    a="${a%/*}"
    a="${a:-/}"
  done
  rc=0
  out="$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_COMMON_DIR -u GIT_CONFIG -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT \
    git -C / config --path --get core.hooksPath && printf x)" || rc=$?
  case $rc in
  0)
    out="${out%x}"
    out="${out%$'\n'}"
    [[ $out != /* ]] || printf '%s\0' "$out"
    ;;
  # exit 1: core.hooksPath is unset
  1) ;;
  *)
    printf >&2 'dispatch: git cannot read the global core.hooksPath; refusing the grant\n'
    return 1
    ;;
  esac
  _git_config_files / || return 1
  if [ -n "${GIT_CONFIG_GLOBAL:-}" ]; then
    [[ $GIT_CONFIG_GLOBAL != /* ]] || printf '%s\0' "$GIT_CONFIG_GLOBAL"
  else
    for out in "$HOME/.gitconfig" "${XDG_CONFIG_HOME:-$HOME/.config}/git/config"; do
      [[ $out != /* ]] || printf '%s\0' "$out"
    done
  fi
}

# _add_dir_ok <path> — print <path>'s canonical form if it may be granted to a
# worker as an extra directory, else fail. The root, $HOME, $crew_dir and
# secrets-dir checks fail silently (the dispatch-time caller words its own
# refusal); a symlink chain too deep or unreadable, a .git/.claude hit, and a
# failed find scan also print their own reason to stderr. Grantable only when
# <path> resolves inside a resolved root from $DISPATCH_GRANT_ROOTS
# (colon-separated); unset or empty grants nothing, so an unconfigured machine
# refuses every --add-dir. A root is skipped if it isn't absolute or existing,
# is /, or is $HOME or an ancestor of it under either spelling. Inside a root,
# defence in depth still refuses /, $HOME or an ancestor of it, anything
# inside or above $crew_dir (the grant records, bus log and launch scripts
# would become writable), and anything inside or above a secrets/credentials
# dir under $HOME, matched both as spelled and as resolved, so a ~/.ssh
# symlinked into /persist is still caught. ~/.config is refused whole: gh,
# gcloud and most other CLIs keep their credentials under it. Symlinks up to
# two levels deep inside a secrets dir are found and every hop of each one's
# chain is walked: a grant containing any hop is refused (the worker could
# retarget or replace it), so a home-manager/stow link into a root is caught
# even when it reaches the root through an intermediate directory symlink,
# and a grant inside the chain's final target is refused. A grant merely
# inside an intermediate hop is not: that cannot retarget the hop itself.
# A later check also refuses a grant containing a .git or .claude entry, then
# one equal to, inside or above the hooks dir, git dir or common dir of any
# repo whose worktree contains it, a config file or include target git reads
# for it, or an absolute global core.hooksPath (see _git_protected_dirs).
# Every symlink inside the grant is resolved too, and one whose target lies
# outside the grant and contains a hop of, or lies inside, any protected
# chain is refused: git reports those dirs resolved, so whichever file
# spelled a path through the grant, git's answer lands under the link's
# target.
_add_dir_ok() {
  local p h hs c s r g ok=""
  local -a roots
  [[ $1 == /* && $1 != *$'\n'* ]] || return 1
  [ -d "$1" ] || return 1
  p="$(realpath -e -- "$1")" || return 1
  h="$(realpath -e -- "$HOME")" || return 1
  hs="${HOME%/}"
  [ "$p" != / ] || return 1
  [[ "$h/" != "$p/"* && "$hs/" != "$p/"* ]] || return 1
  IFS=: read -ra roots <<<"${DISPATCH_GRANT_ROOTS:-}"
  for g in "${roots[@]}"; do
    [[ $g == /* ]] || continue
    r="$(realpath -e -- "$g" 2>/dev/null)" || continue
    [[ $r != / && "$h/" != "$r/"* && "$hs/" != "$r/"* ]] || continue
    if [[ "$p/" == "$r/"* ]]; then ok=1; fi
  done
  [ -n "$ok" ] || return 1
  if [ -n "${crew_dir:-}" ]; then
    c="$(realpath -m -- "$crew_dir")"
    [[ "$p/" != "$c/"* && "$c/" != "$p/"* ]] || return 1
  fi
  for s in .ssh .gnupg .aws .config .claude .codex .kube .docker .password-store .local/share/keyrings .cargo .azure .terraform.d .gradle .m2 .mozilla .var; do
    for r in "$h/$s" "$(realpath -m -- "$h/$s")"; do
      [[ "$p/" != "$r/"* && "$r/" != "$p/"* ]] || return 1
    done
    if [ -d "$h/$s" ]; then
      local _f _l _hop _hf _n
      local -a _hops
      _f=$(mktemp) || return 1
      find -H "$h/$s" -maxdepth 2 -type l -print0 > "$_f" 2>/dev/null || {
        rm -f "$_f"; printf >&2 'dispatch: find failed scanning %s for symlinks; refusing the grant\n' "$h/$s"; return 1
      }
      # shellcheck disable=SC2094 # rm only in the early-exit || branch, not while reading
      while IFS= read -r -d '' _l; do
        _hf=$(mktemp) || { rm -f "$_f"; return 1; }
        if ! _symlink_chain_hops "$_l" > "$_hf"; then
          rm -f "$_f" "$_hf"
          printf >&2 'dispatch: symlink chain too deep or unreadable resolving %s; refusing the grant\n' "$_l"
          return 1
        fi
        _hops=()
        while IFS= read -r -d '' _hop; do
          _hops+=("$_hop")
        done < "$_hf"
        rm -f "$_hf"
        for _hop in "${_hops[@]}"; do
          [[ "$_hop/" != "$p/"* ]] || { rm -f "$_f"; return 1; }
        done
        _n="${#_hops[@]}"
        if [ "$_n" -gt 0 ]; then
          _hop="${_hops[$((_n - 1))]}"
          [[ "$p/" != "$_hop/"* ]] || { rm -f "$_f"; return 1; }
        fi
      done < "$_f"
      rm -f "$_f"
    fi
  done
  local _lf _le _li _lt _gi _gown
  local -a _links=() _lres=()
  _lf=$(mktemp) || return 1
  if ! find "$p" -xdev \( \( -name .git -o -name .claude \) -print0 -quit \) -o \( -type l -print0 \) >"$_lf" 2>/dev/null; then
    rm -f "$_lf"
    printf >&2 'dispatch: find failed scanning %s for embedded repos; refusing the grant\n' "$p"
    return 1
  fi
  # shellcheck disable=SC2094 # rm only in the early-exit branch, not while reading
  while IFS= read -r -d '' _le; do
    case "${_le##*/}" in
    .git | .claude)
      rm -f "$_lf"
      printf >&2 'dispatch: %s contains a .git or .claude entry (%s); grant its narrowest subdir instead\n' "$p" "$_le"
      return 1
      ;;
    esac
    _links+=("$_le")
  done <"$_lf"
  local _gf _gd _ghf _ghop _gn _ghit
  # every hop and every final target, each beside the protected path it came from
  local -a _ghops _ghall=() _ghallo=() _gres=() _greso=()
  _gf=$(mktemp) || { rm -f "$_lf"; return 1; }
  _git_protected_dirs "$p" > "$_gf" || { rm -f "$_gf" "$_lf"; return 1; }
  # shellcheck disable=SC2094 # rm only in the early-exit branches, not while reading
  while IFS= read -r -d '' _gd; do
    _ghf=$(mktemp) || { rm -f "$_gf" "$_lf"; return 1; }
    if ! _symlink_chain_hops "$_gd" > "$_ghf"; then
      rm -f "$_gf" "$_ghf" "$_lf"
      printf >&2 'dispatch: symlink chain too deep or unreadable resolving %s; refusing the grant\n' "$_gd"
      return 1
    fi
    _ghops=()
    while IFS= read -r -d '' _ghop; do
      _ghops+=("$_ghop")
    done < "$_ghf"
    rm -f "$_ghf"
    _ghit=""
    for _ghop in "${_ghops[@]}"; do
      [[ "$_ghop/" != "$p/"* ]] || _ghit=1
      _ghall+=("$_ghop")
      _ghallo+=("$_gd")
    done
    _gn="${#_ghops[@]}"
    [[ "$p/" != "${_ghops[$((_gn - 1))]}/"* ]] || _ghit=1
    _gres+=("${_ghops[$((_gn - 1))]}")
    _greso+=("$_gd")
    if [ -n "$_ghit" ]; then
      rm -f "$_gf" "$_lf"
      printf >&2 'dispatch: %s overlaps git hooks, git dir or config file %s; grant a dir outside it\n' "$p" "$_gd"
      return 1
    fi
  done < "$_gf"
  rm -f "$_gf"
  if [ "${#_links[@]}" -gt 0 ]; then
    if ! printf '%s\0' "${_links[@]}" | xargs -0 realpath -m -z -- >"$_lf"; then
      rm -f "$_lf"
      printf >&2 'dispatch: cannot resolve the symlinks in %s; refusing the grant\n' "$p"
      return 1
    fi
    while IFS= read -r -d '' _lt; do
      _lres+=("$_lt")
    done <"$_lf"
  fi
  rm -f "$_lf"
  if [ "${#_lres[@]}" -ne "${#_links[@]}" ]; then
    printf >&2 'dispatch: cannot resolve the symlinks in %s; refusing the grant\n' "$p"
    return 1
  fi
  for _li in "${!_links[@]}"; do
    # the trailing slash makes a link to / compare as /
    _lt="${_lres[_li]%/}/"
    # a target inside the grant is covered by the grant's own checks
    [[ $_lt != "$p/"* ]] || continue
    _gown=""
    for _gi in "${!_ghall[@]}"; do
      [[ "${_ghall[_gi]%/}/" != "$_lt"* ]] || { _gown="${_ghallo[_gi]}"; break; }
    done
    if [ -z "$_gown" ]; then
      for _gi in "${!_gres[@]}"; do
        [[ $_lt != "${_gres[_gi]%/}/"* ]] || { _gown="${_greso[_gi]}"; break; }
      done
    fi
    if [ -n "$_gown" ]; then
      printf >&2 'dispatch: %s holds symlink %s to %s, which overlaps git hooks, git dir or config file %s; grant a dir without it\n' "$p" "${_links[_li]}" "${_lres[_li]}" "$_gown"
      return 1
    fi
  done
  printf '%s\n' "$p"
}

# _artifacts_dir_bad <branch> — succeed, printing the first offender, when a
# component from $crew_dir/artifacts down to the branch's leaf is a symlink or
# exists as a non-directory. mkdir -p and a redirect follow a symlink planted at
# any of them, including the parents of a slashed branch (#446).
_artifacts_dir_bad() {
  local p="$crew_dir/artifacts" part
  local -a parts
  IFS=/ read -ra parts <<<"$1"
  for part in "" "${parts[@]}"; do
    p="$p${part:+/$part}"
    if [ -L "$p" ] || { [ -e "$p" ] && [ ! -d "$p" ]; }; then
      printf '%s\n' "$p"
      return 0
    fi
  done
  return 1
}

# _protocol_dirs_record_bad — succeed, printing the first offender, when
# $crew_dir/protocol-dirs, a dir above a slashed branch's leaf, or the record
# itself is a symlink or the wrong type (mkdir/mv/reads follow a planted link).
_protocol_dirs_record_bad() {
  local p="$crew_dir/protocol-dirs" part rec="$crew_dir/protocol-dirs/$branch"
  local -a parts
  IFS=/ read -ra parts <<<"$branch"
  for part in "" "${parts[@]:0:${#parts[@]}-1}"; do
    p="$p${part:+/$part}"
    if [ -L "$p" ] || { [ -e "$p" ] && [ ! -d "$p" ]; }; then
      printf '%s\n' "$p"
      return 0
    fi
  done
  if [ -L "$rec" ] || { [ -e "$rec" ] && [ ! -f "$rec" ]; }; then
    printf '%s\n' "$rec"
    return 0
  fi
  return 1
}

# _record_protocol_dirs <worktree> — record the resolved protocol dirs and the
# worktree they belong to, for --spawn-role (#496): dispatcher-written, outside
# every prompt-free write grant a worker holds.
_record_protocol_dirs() {
  local rec="$crew_dir/protocol-dirs/$branch" n v tmp
  local -a lines=()
  for n in PROTOCOL_DIR SKILLS_DIR REVIEWERS_DIR CRITICS_DIR; do
    v="${!n}"
    [[ $v == /* ]] || v=""
    lines+=("$v")
  done
  lines+=("$(realpath -e -- "$1")")
  (
    umask 077
    mkdir -p "$(dirname "$rec")"
    tmp="$(mktemp "$(dirname "$rec")/.dirs.XXXXXX")"
    printf '%s\n' "${lines[@]}" >"$tmp"
    mv -f -- "$tmp" "$rec"
  )
}

# _record_worktree_anchor <worktree> <admin-dir> — write the record `dispatch
# resume` checks its git discovery against (#518).
_record_worktree_anchor() {
  local wt="$1" admin_real="$2" anchor dir bad="" tmp
  anchor="$(_worktree_anchor_path "$wt")"
  dir="$(dirname -- "$anchor")"
  if [ -L "$dir" ] || { [ -e "$dir" ] && [ ! -d "$dir" ]; }; then
    bad="$dir"
  elif [ -L "$anchor" ] || { [ -e "$anchor" ] && [ ! -f "$anchor" ]; }; then
    bad="$anchor"
  fi
  if [ -n "$bad" ]; then
    echo "dispatch: $bad is a symlink or the wrong type — not writing $wt's resume record" >&2
    return 0
  fi
  (
    umask 077
    mkdir -p "$dir"
    tmp="$(mktemp "$dir/.anchor.XXXXXX")"
    printf '%s\n' "$(realpath -e -- "$wt")" "$(realpath -m -- "$crew_dir")" "$branch" "$admin_real" >"$tmp"
    mv -f -- "$tmp" "$anchor"
  )
}

# launch_dir_args <engine> <branch> — emit the ` --add-dir <dir>` flags a claude
# launch needs so its tool calls never stop on a permission dialog nobody
# watches: the protocol, skills, reviewers and critics dirs, the branch's own
# artifacts dir, and the dispatch-time grants in $crew_dir/grants/<branch>.
#
# --add-dir makes a working directory: reads are prompt-free and edits follow
# the permission mode, which auto allows. The four protocol dirs are read-only
# (#442): a worker must never edit its own reviewer briefs, critics, skills or
# WORKER_PROTOCOL.md/GRID_PROTOCOL.md, so each gets `--add-dir` (reads stay
# prompt-free) plus an `Edit(//<dir>/**)` deny rule via `--disallowedTools`
# (verified: a `Read` allow rule does not override
# blockReadsOutsideWorkingDirectories, so `--add-dir` stays the only way to
# keep reads prompt-free). The artifacts dir and explicit grants stay plain
# write-capable `--add-dir`, re-validated on every launch. The record is the
# only authority: the worker edits WORKER_TASK.md, so its add_dir: header
# lines are a mirror, never read.
#
# claude's --add-dir and --disallowedTools are both variadic and would swallow
# the positional prompt, so callers splice this in right before
# --append-system-prompt-file, terminating both lists.
#
# Other engines get nothing: codex runs with
# --dangerously-bypass-approvals-and-sandbox and cursor with --force, so neither
# gates paths, and pi has no tool-permission layer at all.
launch_dir_args() {
  [ "$1" = claude ] || return 0
  local a c d bad line dirs=() rules=()
  local -A seen=()
  for d in "$PROTOCOL_DIR" "$SKILLS_DIR" "$REVIEWERS_DIR" "$CRITICS_DIR"; do
    [[ $d == /* ]] && [ -d "$d" ] || continue
    d="${d%/}"
    c="$(realpath -e -- "$d")"
    if [[ ! $d =~ ^/[A-Za-z0-9._/+@-]*$ || ! $c =~ ^/[A-Za-z0-9._/+@-]*$ ]]; then
      echo "dispatch: not granting $d — its path cannot be written as a read-only rule" >&2
      continue
    fi
    dirs+=("$d")
    rules+=("Edit(/$d/**)")
    [ "$c" = "$d" ] || rules+=("Edit(/$c/**)")
  done
  a="$crew_dir/artifacts/$2"
  if bad="$(_artifacts_dir_bad "$2")"; then
    echo "dispatch: $bad is a symlink or not a directory — not granting $a to $2" >&2
  else
    mkdir -p -- "$a"
    dirs+=("$a")
  fi
  if [ -f "$crew_dir/grants/$2" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      [ -n "$line" ] || continue
      if d="$(_add_dir_ok "$line")"; then
        dirs+=("$d")
      else
        echo "dispatch: dropping invalid grant '$line' for $2" >&2
      fi
    done <"$crew_dir/grants/$2"
  fi
  for d in "${dirs[@]}"; do
    [ -z "${seen[$d]:-}" ] || continue
    seen[$d]=1
    printf ' --add-dir %q' "$d"
  done
  if [ "${#rules[@]}" -gt 0 ]; then
    printf ' --disallowedTools'
    printf ' %q' "${rules[@]}"
  fi
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
# The pane is typed one short line that runs the launch script, then
# `; bash <exit script>` — the `dispatch --role-exited …` continuation — which
# the pane's shell runs only when the engine returns, so an engine that
# crashes at startup is reported, while a reap (which kills the pane and its
# shell) is silent. `;` is valid in fish (the pane shell), bash and zsh.
launch_role() {
  local pane="$1" wt="$2" role="$3" r_agent="$4" r_model="$5" r_effort="$6" prompt first quoted_model quoted_name quoted_dir quoted_prompt quoted_first cmd exit_cmd launch_line exit_line
  printf -v quoted_model '%q' "$r_model"
  printf -v quoted_name '%q' "${agent_name}-${role}"
  printf -v exit_cmd "%q --role-exited %q --branch %q --pane '%s' --since %s" "$dispatch_self" "$role" "$branch" "$pane" "$(jq -nc 'now*1000|floor')"
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
    cmd="${git_env}PI_CODING_AGENT_DIR=$quoted_dir pi --name $quoted_name --model $quoted_model --thinking $r_effort --append-system-prompt $PROTOCOL_DIR/GRID_PROTOCOL.md --no-approve$(pi_skill_args "$wt") $quoted_prompt"
    ;;
  claude) cmd="${git_env}claude --name $quoted_name --model $quoted_model --effort $r_effort$(launch_dir_args claude "$branch") --append-system-prompt-file $PROTOCOL_DIR/GRID_PROTOCOL.md --permission-mode auto $quoted_prompt" ;;
  codex) cmd="${git_env}codex --profile worker -m $quoted_model -c model_reasoning_effort=$r_effort -c service_tier=default --dangerously-bypass-approvals-and-sandbox $quoted_first" ;;
  cursor) cmd="${git_env}CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model $quoted_model $quoted_first" ;;
  esac
  write_launch_script launch_line "$cmd"
  write_launch_script exit_line "$exit_cmd" exit
  tmux send-keys -t "$pane" "$launch_line ; $exit_line" Enter
}

# watch_role <role> <pane> <agent> — spawn the detached, engine-agnostic bus
# watcher for a role pane. It types each assignment into the pane and keeps
# @crew_state fresh, so the role never holds a repainting `crew await`.
watch_role() {
  nohup "$0" --role-watch "$1" --pane "$2" --engine "$3" --branch "$branch" >/dev/null 2>&1 &
}

# watch_role_prompts <role> <pane> <agent> <crew> — a parked claude role can
# still sit on a permission dialog, so it gets a stall-watch under its role: id,
# which runs only the prompt detectors. Other engines have no prompt to detect.
watch_role_prompts() {
  [ "$3" = claude ] || return 0
  CREW_ID="$4" nohup crew stall-watch "role:$branch:$1" --pane "$2" --engine claude >/dev/null 2>&1 &
}

# `dispatch --role-watch <role> --pane <pane> [--engine E] [--branch <b>]
# [--interval S] [--defer-notice S]` — a role supervisor. It watches the crew
# bus and, when the lead assigns this role work, types the assignment into the
# role's pane (a normal user turn) and keeps the pane's @crew_state fresh. That
# lets a role end its turn instead of holding a repainting `crew await`.
#
# It sends keys ONLY to a pane whose capture is positively an idle input box
# (claude, pi); anything else — a permission dialog, an option-select or quota
# prompt, a live turn, an unrecognised frame, or an engine with no recognised
# idle frame (codex, cursor) — defers to the next tick. After --defer-notice
# seconds (default 60) of deferral it tells the lead once with an
# `assignment_deferred` msg, so a role that never receives its assignment is not
# mistaken for one that is working.
if [ "${1:-}" = "--role-watch" ]; then
  role="${2:-}"
  [ -n "$role" ] || {
    echo "dispatch: --role-watch needs a role name" >&2
    exit 1
  }
  shift 2
  watch_pane=""
  watch_branch=""
  engine=claude
  interval=2
  defer_notice=60
  while [ $# -gt 0 ]; do
    case "$1" in
    --pane) watch_pane="${2:-}"; shift 2 ;;
    --branch) watch_branch="${2:-}"; shift 2 ;;
    --engine) engine="${2:-}"; shift 2 ;;
    --defer-notice) defer_notice="${2:-}"; shift 2 ;;
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
  # --pane is caller-supplied and this watcher types into it as a user turn
  # (#521). The pane is not our ancestor, so serve it only when dispatch
  # stamped it as this role in this branch's window — never a lead — and only
  # by a concrete %id, which cannot re-resolve to another pane later.
  [[ $watch_pane =~ ^%[0-9]+$ ]] || {
    echo "dispatch: --role-watch: --pane must be a pane id (%N), got $watch_pane" >&2
    exit 1
  }
  [ "$role" != lead ] || {
    echo "dispatch: --role-watch: role 'lead' is never watched" >&2
    exit 1
  }
  watch_stamp="$(tmux display-message -p -t "$watch_pane" '#{@crew_role}|#{window_id}' 2>/dev/null || true)"
  IFS='|' read -r w_role w_win <<<"$watch_stamp"
  [ "$w_role" = "$role" ] || {
    echo "dispatch: --role-watch: pane $watch_pane's @crew_role ($w_role) does not match --role $role" >&2
    exit 1
  }
  w_branch="$(tmux show-options -wqv -t "$w_win" @crew_branch 2>/dev/null || true)"
  [ "$w_branch" = "$watch_branch" ] || {
    echo "dispatch: --role-watch: window $w_win's @crew_branch ($w_branch) does not match --branch $watch_branch" >&2
    exit 1
  }
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

  # Signatures copied byte-identically from crew.sh's stall-watch (this is a
  # standalone build; tests/adapters.bats pins each copy).
  re_option='^[[:space:]]*(>|❯|\*)?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]'
  re_meter='^[^[:alnum:]]*[A-Za-z]+…[[:space:]]\(([0-9]+h([[:space:]][0-9]+m)?([[:space:]][0-9]+s)?|[0-9]+m([[:space:]][0-9]+s)?|[0-9]+s)[[:space:]]·[[:space:]]↓[[:space:]][0-9.]+k?[[:space:]]tokens'
  re_subrow='^[[:space:]]*[^[:alnum:][:space:]]+[[:space:]]+[a-z][a-z-]+[[:space:]][[:space:]]+.*[[:space:]](([0-9]+h[[:space:]])?([0-9]+m[[:space:]])?[0-9]+s)[[:space:]]·[[:space:]]↓'
  # SGR/CSI stripping regex, built from a raw ESC byte via ANSI-C quoting —
  # never `\x1b` as escape text in a regex/awk source literal (a GNU
  # extension; this repo's shell-reviewer already flags `grep -P` the same
  # way for the same portability reason). Reused by _box_rows.
  csi_re=$'\033\\[[0-9;]*m'
  csi_sed="s/${csi_re}//g"
  # The dim-SGR marker claude wraps a `❯`-row prompt suggestion in (ghost
  # text), vs. a real unsent draft the user typed: nbsp separator + ESC[2m.
  # Built from a raw ESC byte via ANSI-C quoting, same reasoning as csi_re.
  _rw_esc=$'\033'
  _rw_ghost_marker=$'❯\xc2\xa0'"${_rw_esc}[2m"

  _meter_line() { printf '%s\n' "$1" | grep -E "$re_meter" | tail -1 || true; }
  _has_subrow() { printf '%s\n' "$1" | grep -qE "$re_subrow"; }

  _is_codex_hook_review_prompt() {
    local tail_n expected
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -4 || true)
    expected=$'Hooks need review\n  1 hook is new or changed.\n  Hooks can run outside the sandbox after you trust them.\n› 1. Review hooks  2. Trust all and continue  3. Continue without trusting'
    [ "$tail_n" = "$expected" ]
  }

  _is_permission_prompt() {
    local tail_n last above
    [ "$engine" = claude ] || return 1
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -10 || true)
    last=$(printf '%s\n' "$tail_n" | tail -1)
    case "$last" in
    *"Esc to cancel · Tab to amend"*) ;;
    *) return 1 ;;
    esac
    above=$(printf '%s\n' "$tail_n" | sed '$d')
    printf '%s\n' "$above" | grep -qE "$re_option" || return 1
    printf '%s\n' "$above" | grep -qF 'Do you want to proceed?'
  }

  _is_prompt() {
    local tail_n last above
    if [ "$engine" = codex ]; then
      _is_codex_hook_review_prompt "$1"
      return
    fi
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -7 || true)
    last=$(printf '%s\n' "$tail_n" | tail -1)
    case "$last" in
    *"Enter to select"* | *"Enter to confirm"*) ;;
    *) return 1 ;;
    esac
    above=$(printf '%s\n' "$tail_n" | sed '$d')
    printf '%s\n' "$above" | grep -qE "$re_option"
  }

  # _box_rows <text> <first-row-regex> — the input box of a claude or pi
  # pane: the LAST two `─` rules in the last 30 non-blank lines (blank
  # determined on the ANSI-stripped form, so this works identically on a
  # plain `capture-pane -p` or a colored `capture-pane -e` capture) bound
  # it, the first row inside must match <first-row-regex> against its
  # STRIPPED text, at most 12 rows sit inside and 1-5 rows follow the lower
  # rule. Prints the first inside row VERBATIM (colored, if the input was
  # colored — this is what lets a caller inspect its SGR attributes without
  # a separate correlation pass), then up to 7 rows above the upper rule
  # (stripped). The rule test is a prefix match on the stripped form, not a
  # character class.
  _box_rows() {
    printf '%s\n' "$1" |
      rx="$2" csi="$csi_re" awk '
        {
          stripped = $0
          gsub(ENVIRON["csi"], "", stripped)
          if (stripped ~ /^[[:space:]]*$/) next
          n++
          raw[n] = $0
          plain[n] = stripped
        }
        END {
          off = (n > 30) ? n - 30 : 0
          b = 0; a = 0
          for (i = n; i > off; i--) if (index(plain[i], "─") == 1) { if (!b) b = i; else { a = i; break } }
          if (!a || b - a < 1) exit 1
          if (b - a - 1 > 12 || n - b < 1 || n - b > 5) exit 1
          if (b - a == 1 && "" !~ ENVIRON["rx"]) exit 1
          if (b - a > 1 && plain[a + 1] !~ ENVIRON["rx"]) exit 1
          print (b - a > 1 ? raw[a + 1] : "")
          for (j = a - 1; j > off && j >= a - 7; j--) print plain[j]
        }'
  }

  # _claude_idle_box <text> <colored-text> <own> — positive idle shape of a
  # claude pane: a box whose first row is `❯` (not a numbered option), status
  # rows after it (fx_bgwait_*, fx_session_limit_refusal). A live turn keeps
  # the box drawn, so a meter line, subagent row or `esc to interrupt`
  # anywhere in the window, or a spinner line in the 7 rows above the box,
  # vetoes. The spinner check is positional so a transcript line like
  # `Summary…` cannot wedge an idle pane. `own=1` (the caller's own just-typed
  # text sitting in the box) counts as idle outright, without inspecting the
  # `❯` row at all — the escape hatch for the post-type recheck, otherwise the
  # watcher's own delivered text would defer forever. Otherwise a non-empty
  # `❯` row is idle only if it is ghost text (a dimmed prompt suggestion,
  # `_rw_ghost_marker` in the colored `$2` capture) rather than a real unsent
  # draft; with no colored capture to check, it fails closed (not idle).
  _claude_idle_box() {
    local tail_n out row above draft own="${3:-0}" colored_row
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -30 || true)
    [ -n "$(_meter_line "$tail_n")" ] && return 1
    _has_subrow "$tail_n" && return 1
    printf '%s\n' "$tail_n" | grep -qF 'esc to interrupt' && return 1
    out=$(_box_rows "$1" '^[[:space:]]*❯') || return 1
    row=$(printf '%s\n' "$out" | head -1)
    above=$(printf '%s\n' "$out" | tail -n +2)
    printf '%s\n' "$row" | grep -qE "$re_option" && return 1
    printf '%s\n' "$above" | grep -qE '^[^[:alnum:]]*[A-Za-z]+…' && return 1
    [ "$own" = 1 ] && return 0
    draft=$(printf '%s\n' "$row" | sed -E $'s/^[[:space:]]*❯([[:space:]]|\xc2\xa0)*//')
    [ -n "$draft" ] || return 0
    [ -n "$2" ] || return 1 # no colored capture available — fail closed
    colored_row=$(_box_rows "$2" '^[[:space:]]*❯') || return 1
    colored_row=$(printf '%s\n' "$colored_row" | head -1)
    printf '%s\n' "$colored_row" | grep -qF "$_rw_ghost_marker"
  }

  # _pi_live_turn <text> — a live pi turn replaces the editor's top rule
  # with a spinner-and-status label row (real capture, pi 0.87.1, both
  # during text generation and a bash tool call: `── ⠼ Working ──…`),
  # leaving the box beneath it looking exactly like an idle empty box. A
  # genuinely idle pi rule is a pure run of `─` (real capture); any
  # rule-shaped line (starts with `──`) that is NOT entirely dashes is
  # treated as a live-turn label, generalizing beyond the one literal
  # status text captured above. Checked anywhere in the last 30
  # non-empty lines, mirroring _claude_idle_box's position-flexible
  # _meter_line/_has_subrow/"esc to interrupt" checks. Uses index()/gsub()
  # on the bare glyph, never a quantifier directly on it, so this stays
  # correct under LC_ALL=C (see _box_rows's own comment on the same trap).
  _pi_live_turn() {
    printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -30 | awk '
      {
        if (index($0, "─") != 1) next
        line = $0
        gsub(/─/, "", line)
        if (line !~ /^[[:space:]]*$/) { found = 1; exit }
      }
      END { exit (found ? 0 : 1) }'
  }

  # _pi_idle_box <text> — positive idle shape of a pi pane, from a real capture
  # (pi 0.87.1): an editor bounded by two `─` rules, blank or holding text, with
  # the cwd and stats rows after the lower rule. A bare shell prompt or a boot
  # frame has no such box.
  _pi_idle_box() {
    local out row
    _pi_live_turn "$1" && return 1
    out=$(_box_rows "$1" '.*') || return 1
    row=$(printf '%s\n' "$out" | head -1)
    printf '%s\n' "$row" | grep -qE "$re_option" && return 1
    return 0
  }

  # _footer_composer_equal <text> <prefix> <footer-regex> <separator-rows>
  # <expected> <trailing-row-regex>... — confirm only the composer immediately
  # above one observed engine footer. Captures do not distinguish a literal
  # editor newline from terminal wrapping, so assignments containing newlines
  # cannot be safely reconstructed and are refused.
  _footer_composer_equal() {
    local text="$1" prefix="$2" footer_re="$3" separator_rows="$4" expected="$5" line value footer_i start_i i trailing_i
    local -a rows
    case "$expected" in
    *$'\n'*) return 1 ;;
    esac
    mapfile -t rows <<<"$text"
    footer_i=-1
    for ((i = 0; i < ${#rows[@]}; i++)); do
      [[ ${rows[i]} =~ $footer_re ]] || continue
      [ "$footer_i" -eq -1 ] || return 1
      footer_i=$i
    done
    [ "$footer_i" -gt "$separator_rows" ] || return 1

    # A footer-like transcript line or dialog overlay is not a writable
    # composer. The engine's observed trailing status rows must immediately
    # follow the footer and terminate the pane capture.
    trailing_i=0
    shift 5
    for footer_re in "$@"; do
      trailing_i=$((trailing_i + 1))
      [ $((footer_i + trailing_i)) -lt "${#rows[@]}" ] || return 1
      [[ ${rows[footer_i + trailing_i]} =~ $footer_re ]] || return 1
    done
    [ $((footer_i + trailing_i)) -eq $((${#rows[@]} - 1)) ] || return 1

    # The observed geometry is composer block, its engine-specific blank
    # separator, then the footer. Reconstruct exactly the one block touching
    # that separator; no prefix search is allowed elsewhere in the capture.
    for ((i = 1; i <= separator_rows; i++)); do
      [ -z "${rows[footer_i - i]}" ] || return 1
    done
    start_i=$((footer_i - separator_rows - 1))
    [ -n "${rows[start_i]}" ] || return 1
    while [ "$start_i" -gt 0 ] && [ -n "${rows[start_i - 1]}" ]; do
      start_i=$((start_i - 1))
    done
    line="${rows[start_i]}"
    [[ $line == "$prefix"* ]] || return 1
    value="${line#"$prefix"}"
    for ((i = start_i + 1; i < footer_i - separator_rows; i++)); do
      value+="${rows[i]}"
    done
    [ "$value" = "$expected" ]
  }

  # Codex's composer is not bordered. The captured idle frame has its product
  # banner and Vim status line around the composer; preserve all three anchors
  # so another terminal's `›` line cannot become writable.
  _codex_composer() {
    local text="$1" composer="$2"
    printf '%s\n' "$text" | grep -qE '^[[:space:]]*│ >_ OpenAI Codex \(v[0-9]' || return 1
    _footer_composer_equal "$text" '› ' '^[[:space:]]{2}[^[:space:]].*[[:space:]]Vim:[[:space:]]Insert$' 1 "$composer" \
      '^[[:space:]]{2}.*(for shortcuts|warnings).*$' || return 1
    printf '%s\n' "$text" | grep -qF 'esc to interrupt' && return 1
    printf '%s\n' "$text" | grep -qE '^[[:space:]]*[•·] Working \(' && return 1
    return 0
  }

  _codex_idle_box() {
    _codex_composer "$1" 'Ask Codex to do anything'
  }

  # Cursor's captured composer has no box either. Its identity, version and
  # mode rows are all required in addition to the exact empty prompt.
  _cursor_composer() {
    local text="$1" composer="$2"
    printf '%s\n' "$text" | grep -qFx '  Cursor Agent' || return 1
    printf '%s\n' "$text" | grep -qE '^[[:space:]]*v[0-9][0-9.]*-' || return 1
    _footer_composer_equal "$text" '  → ' '^[[:space:]]{2}[^[:space:]].*[[:space:]]Run Everything -- INSERT --$' 2 "$composer" \
      '^[[:space:]]{2}([~/]|[[:alnum:]_.-]+/).*' '^[[:space:]]*.*[[:space:]]·[[:space:]][^[:space:]]+$' || return 1
    printf '%s\n' "$text" | grep -qF 'ctrl+c to stop' && return 1
    printf '%s\n' "$text" | grep -qE 'Thinking[[:space:]]+[0-9]+ tokens' && return 1
    return 0
  }

  _cursor_idle_box() {
    _cursor_composer "$1" 'Plan, search, build anything'
  }

  # _role_pane_ready <text> [colored] [own] — the only gate in front of
  # send-keys. Fail closed: a permission dialog, an option-select or quota
  # prompt, a live turn, or any frame not positively recognised defers. Each
  # engine has a recognised idle shape from captured frames. Its recognizer
  # requires its own observed empty composer, so unknown, boot and dialog
  # frames defer. `colored`/`own` are forwarded to claude's `_claude_idle_box`
  # for the ghost-vs-draft check; every other engine's recognizer ignores them.
  _role_pane_ready() {
    [ -n "$1" ] || return 1
    _is_permission_prompt "$1" && return 1
    _is_prompt "$1" && return 1
    case "$engine" in
    claude) _claude_idle_box "$1" "$2" "$3" ;;
    codex) _codex_idle_box "$1" ;;
    cursor) _cursor_idle_box "$1" ;;
    pi) _pi_idle_box "$1" ;;
    *) return 1 ;;
    esac
  }

  # Codex and Cursor must prove the watcher just injected this assignment
  # before Enter is sent.  Their normal ready recognizers intentionally accept
  # only empty composers. Claude and pi retain their established re-check,
  # with `own=1` (the caller's own just-pasted text) so claude's ghost-vs-draft
  # check doesn't fail closed on the text this watcher itself just delivered —
  # `colored` is the paired `capture-pane -e` frame `_claude_idle_box` needs.
  _role_assignment_confirmed() {
    local text="$1" assignment="$2" colored="$3"
    _is_permission_prompt "$text" && return 1
    _is_prompt "$text" && return 1
    case "$engine" in
    codex) _codex_composer "$text" "Assignment: $assignment" ;;
    cursor) _cursor_composer "$text" "Assignment: $assignment" ;;
    *) _role_pane_ready "$text" "$colored" 1 ;;
    esac
  }

  # tmux send-keys would interpret control bytes as terminal input. Assignments
  # are ordinary single-line turns, so refuse every C0 byte before typing.
  _role_assignment_safe() {
    local LC_ALL=C
    case "$1" in
    *[[:cntrl:]]*) return 1 ;;
    *) return 0 ;;
    esac
  }

  # Assignments wait here until the pane is ready; one is delivered per tick so
  # the next capture sees the turn it started. `cooldown` skips the tick right
  # after a send, when the pane may not have repainted as busy yet. The text
  # is pasted via a tmux load-buffer/paste-buffer round trip (bracketed
  # paste, not send-keys -l): per tmux(1), `-p` wraps the paste in bracket
  # codes only when the destination has requested bracketed-paste mode, so
  # this helps an app that has (our engines' composers) but isn't a
  # guarantee — the pre-paste gate and post-paste re-check remain the actual
  # backstop. `-r` skips tmux's default LF→CR translation, so a future
  # loosening of `_role_assignment_safe`'s control-byte refusal can't
  # silently reintroduce CR-as-Enter through it. The pane is re-checked, and
  # only then is Enter sent: a dialog raised in between (a subagent's) is
  # never confirmed. Text left in the pane by a failed re-check is cleared
  # with C-u before the retry. `pending` lives in this process only, capped
  # at `pending_max` (oldest dropped silently once full); the watcher exits
  # with its pane.
  pending=()
  pending_max=50
  cooldown=0
  unsent=0
  lead_id=""
  deferred_since=0
  deferred_told=0
  # Exits when the pane is gone (role reaped, or the window closed) or its
  # engine has exited.
  while tmux display-message -p -t "$watch_pane" '#{pane_id}' >/dev/null 2>&1; do
    watch_exited && break
    if [ -f "$log" ]; then
      batch="$(jq -c --arg me "$role_id" --argjson since "$since" \
        'select(.kind=="msg" and .ts>$since and ((.to==$me) or (.from==$me)))' "$log" 2>/dev/null || true)"
      if [ -n "$batch" ]; then
        while IFS= read -r ev; do
          if [ "$(printf '%s' "$ev" | jq -r '.to // ""')" = "$role_id" ]; then
            body="$(printf '%s' "$ev" | jq -r '.body // ""')"
            [ -n "$body" ] || continue
            lead_id="$(printf '%s' "$ev" | jq -r '.from // ""')"
            [ "${#pending[@]}" -lt "$pending_max" ] || pending=("${pending[@]:1}")
            pending+=("$body")
            watch_set_state working
          elif [ "${#pending[@]}" -eq 0 ]; then
            # A verdict from the role — it is idle again.
            watch_set_state idle
          fi
        done <<<"$batch"
        next="$(printf '%s\n' "$batch" | jq -s 'map(.ts) | max // empty')"
        since="${next:-$since}"
      fi
    fi
    if [ "${#pending[@]}" -eq 0 ]; then
      deferred_since=0
      deferred_told=0
    elif [ "$cooldown" -gt 0 ]; then
      cooldown=$((cooldown - 1))
    elif ! watch_exited; then
      frame_e="$(tmux capture-pane -e -p -t "$watch_pane" 2>/dev/null || true)"
      frame="$(printf '%s' "$frame_e" | sed -E "$csi_sed" 2>/dev/null || true)"
      if _role_pane_ready "$frame" "$frame_e" "$unsent" && _role_assignment_safe "${pending[0]}"; then
        [ "$unsent" -eq 1 ] && { tmux send-keys -t "$watch_pane" C-u 2>/dev/null || true; }
        buf="rw-assign-$$-$RANDOM"
        if printf 'Assignment: %s' "${pending[0]}" | tmux load-buffer -b "$buf" - 2>/dev/null; then
          if tmux paste-buffer -p -d -r -b "$buf" -t "$watch_pane" 2>/dev/null; then
            frame_e="$(tmux capture-pane -e -p -t "$watch_pane" 2>/dev/null || true)"
            frame="$(printf '%s' "$frame_e" | sed -E "$csi_sed" 2>/dev/null || true)"
            if _role_assignment_confirmed "$frame" "${pending[0]}" "$frame_e"; then
              tmux send-keys -t "$watch_pane" Enter 2>/dev/null || true
              pending=("${pending[@]:1}")
              unsent=0
              cooldown=1
              deferred_since=0
              deferred_told=0
            else
              unsent=1
            fi
          else
            # paste-buffer failed (tmux hiccup, or the pane closed between
            # load-buffer and paste-buffer) — do NOT fall through to
            # _role_assignment_confirmed: nothing was actually pasted, so a
            # stale frame that happens to still look like an idle box could
            # get a spurious Enter and pending[0] wrongly dequeued as
            # delivered. Same reasoning as the load-buffer branch below:
            # leave $unsent untouched. -d never ran, so clean up the loaded
            # buffer explicitly — each attempt gets its own name ($RANDOM),
            # so nothing later would overwrite an orphaned one.
            tmux delete-buffer -b "$buf" 2>/dev/null || true
          fi
        else
          : # load-buffer failed (tmux hiccup); leave $unsent untouched — do
            # NOT set it to 1 here. Nothing was typed, so the box's current
            # content is whatever it was before this tick, not necessarily the
            # watcher's own text; setting unsent=1 would make next tick's
            # pre-type check pass with own=1 and issue a C-u that could wipe an
            # unrelated real draft.
        fi
      else
        [ "$deferred_since" -gt 0 ] || deferred_since="$(date +%s)"
        if [ "$deferred_told" -eq 0 ] && [ -n "$lead_id" ] &&
          [ $(($(date +%s) - deferred_since)) -ge "$defer_notice" ]; then
          deferred_told=1
          crew msg "$role_id" "$lead_id" "$(jq -nc --arg r "$role" --arg p "$watch_pane" --arg e "$engine" \
            '{role:$r,event:"assignment_deferred",pane:$p,engine:$e,detail:"assignment not delivered: the pane is not at an idle input box (a permission dialog, prompt, live turn or unrecognised frame), or its engine has no recognised idle frame"}')" 2>/dev/null || true
        fi
      fi
    fi
    sleep "$interval"
  done
  exit 0
fi

# git_env — prefix every engine launch with a non-interactive git editor.
# Workers inherit the user's interactive $EDITOR (nvim); any git command that
# opens an editor (rebase --continue, commit --amend, merge without --no-edit,
# rebase -i) then hangs forever in a TTY-less engine bash tool. GIT_EDITOR=true
# keeps git's prepared message; GIT_SEQUENCE_EDITOR=: accepts a rebase todo
# as-is. Prefix the send-keys commands (like the CREW_* vars) so the pane's
# own shell exports them before the engine starts.
git_env="GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: "

# `dispatch --spawn-role <role>` — create a lazy grid's role pane on demand in
# the caller's own window/worktree, from roles.json. Idempotent.
if [ "${1:-}" = "--spawn-role" ]; then
  _settings_load
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
  # The role's nohup'd --role-watch and stall-watch inherit this env and locate
  # the bus through git discovery; a worker-set GIT_* would steer them wrong.
  # ${!GIT_@}, not compgen: a non-interactive bash build has no compgen.
  for _v in "${!GIT_@}"; do unset "$_v"; done
  [ -f WORKER_TASK.md ] || {
    echo "dispatch: --spawn-role must run inside a worker worktree (no WORKER_TASK.md)" >&2
    exit 1
  }
  [ -n "${TMUX_PANE:-}" ] || {
    echo "dispatch: --spawn-role must run inside tmux" >&2
    exit 1
  }
  _pane_is_ancestor "$TMUX_PANE" || {
    echo "dispatch: --spawn-role: \$TMUX_PANE ($TMUX_PANE) is not this process's pane — run it from the lead's own pane" >&2
    exit 1
  }
  # crew_dir and branch come from the window dispatch stamped, never git
  # discovery: GIT_* env and the worktree's .git gitlink are worker-controlled,
  # and a worker can build a genuine repo + worktree to point them at (#496).
  win="$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}')"
  crew_dir="$(tmux show-options -wqv -t "$win" @crew_dir)"
  branch="$(tmux show-options -wqv -t "$win" @crew_branch)"
  if [[ $crew_dir != /* ]] || [ -z "$branch" ]; then
    echo "dispatch: --spawn-role: this window has no @crew_dir/@crew_branch (dispatched before they were stamped) — re-dispatch the task" >&2
    exit 1
  fi
  # The dirs come from the dispatch-time record, never this (worker's) env.
  if bad="$(_protocol_dirs_record_bad)"; then
    echo "dispatch: $bad is a symlink or the wrong type — refusing to use the protocol-dirs record" >&2
    exit 1
  fi
  [ -f "$crew_dir/protocol-dirs/$branch" ] || {
    echo "dispatch: --spawn-role: no protocol-dirs record for $branch — re-dispatch the task" >&2
    exit 1
  }
  mapfile -t rec_lines <"$crew_dir/protocol-dirs/$branch"
  wt_root="${rec_lines[4]:-}"
  [ -n "$wt_root" ] && [ "$wt_root" = "$(realpath -e -- "$PWD")" ] || {
    echo "dispatch: --spawn-role must run from the dispatched worktree's root (${rec_lines[4]:-unrecorded})" >&2
    exit 1
  }
  # Pin to the recorded root and use it below, never $PWD: a worker could cd
  # through a symlink to its worktree and retarget the link after this check.
  cd -- "$wt_root" || exit 1
  unset DISPATCHER_PROTOCOL_DIR DISPATCHER_SKILLS_DIR DISPATCHER_REVIEWERS_DIR DISPATCHER_CRITICS_DIR
  DISPATCHER_PROTOCOL_DIR="${rec_lines[0]:-}" _resolve_dir PROTOCOL_DIR DISPATCHER_PROTOCOL_DIR "@protocolDir@" dispatch
  DISPATCHER_SKILLS_DIR="${rec_lines[1]:-}" _resolve_dir SKILLS_DIR DISPATCHER_SKILLS_DIR "@skillsDir@" dispatch
  DISPATCHER_REVIEWERS_DIR="${rec_lines[2]:-}" _resolve_dir REVIEWERS_DIR DISPATCHER_REVIEWERS_DIR "@reviewersDir@" dispatch
  DISPATCHER_CRITICS_DIR="${rec_lines[3]:-}" _resolve_dir CRITICS_DIR DISPATCHER_CRITICS_DIR "@criticsDir@" dispatch
  _require_protocol_files "$PROTOCOL_DIR" WORKER_PROTOCOL.md EVIDENCE_REVIEW.md GRID_PROTOCOL.md
  _check_protocol_rev "$PROTOCOL_DIR" dispatch
  roles_file="$crew_dir/artifacts/$branch/roles.json"
  if bad="$(_artifacts_dir_bad "$branch")"; then
    echo "dispatch: $bad is a symlink or not a directory — refusing to use roles.json" >&2
    exit 1
  fi
  [ -f "$roles_file" ] || {
    echo "dispatch: no role grid recorded for $branch (dispatch without --lazy to use an up-front grid)" >&2
    exit 1
  }
  spec="$(jq -r --arg r "$role" '.[$r] // empty | [.agent, .model, (.effort // "")] | @tsv' "$roles_file")"
  [ -n "$spec" ] || {
    echo "dispatch: role '$role' is not part of this grid" >&2
    exit 1
  }
  # Both reach the role's launch script; the header and roles.json are worker-writable (#470).
  agent_name="$(sed -n 's/^agent_name: //p' WORKER_TASK.md)"
  for _v in "$role" "$agent_name"; do
    [[ $_v =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || {
      echo "dispatch: --spawn-role: refusing ${_v@Q} — a role or agent name must be a plain word" >&2
      exit 1
    }
  done
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
  budget_stop "$spawn_agent" "$role"
  [ "$spawn_agent" = pi ] && seed_pi_agent_dir
  role_pane="$(split_role_pane "$win" "$wt_root" "$role" "$spawn_worker_id" "$spawn_crew_id")"
  launch_role "$role_pane" "$wt_root" "$role" "$spawn_agent" "$spawn_model" "$effort"
  watch_role "$role" "$role_pane" "$spawn_agent"
  watch_role_prompts "$role" "$role_pane" "$spawn_agent" "$spawn_crew_id"
  # Persist the spec this pane actually launched with: a bare respawn of the
  # role (a died or stalled pane) must come back at the same rung, not silently
  # at the dispatch-time one.
  roles_tmp="$(mktemp "$roles_file.XXXXXX")"
  jq --arg r "$role" --arg a "$spawn_agent" --arg m "$spawn_model" --arg e "$effort" \
    '.[$r] = {agent: $a, model: $m, effort: $e}' "$roles_file" >"$roles_tmp"
  mv "$roles_tmp" "$roles_file"
  # The grid hints follow the pane count: this window now has >=1 role pane.
  # Only the lead publishes the lead hint (a role pane never runs this).
  if [ -z "${CREW_ROLE_ID:-}" ]; then
    publish_grid_lead "$win" "$TMUX_PANE"
  fi
  publish_grid_window "$win"
  refit_grid "$win"
  echo "spawned role $role ($spawn_agent/$spawn_model) in $role_pane"
  exit 0
fi

# `dispatch --reap-roles` — kill every role pane in the caller's window.
if [ "${1:-}" = "--reap-roles" ]; then
  [ -n "${TMUX_PANE:-}" ] || {
    echo "dispatch: --reap-roles must run inside tmux" >&2
    exit 1
  }
  _pane_is_ancestor "$TMUX_PANE" || {
    echo "dispatch: --reap-roles: \$TMUX_PANE ($TMUX_PANE) is not this process's pane — run it from the lead's own pane" >&2
    exit 1
  }
  win="$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}')"
  tmux list-panes -t "$win" -F '#{pane_id} #{@crew_role}' | while read -r p r; do
    [ -n "$r" ] || continue
    # `lead` is a pane value now (the contract), so a lead pane that is not the
    # caller's own must never be reaped.
    [ "$r" = lead ] && continue
    [ "$p" = "$TMUX_PANE" ] && continue
    tmux kill-pane -t "$p" 2>/dev/null || true
  done
  # The last role pane's death ends the grid: unset the hint so tmux-og stops
  # refitting this window. `lead` and empties are not role panes.
  remaining="$(tmux list-panes -t "$win" -F '#{@crew_role}' 2>/dev/null | grep -vx 'lead' | grep -v '^$' || true)"
  if [ -z "$remaining" ]; then
    tmux set-window-option -t "$win" -u @crew_grid 2>/dev/null || true
  fi
  refit_grid "$win"
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
  _pane_is_ancestor "$exited_pane" || {
    echo "dispatch: --role-exited: --pane ($exited_pane) is not this process's pane — run it from the role's own pane" >&2
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
  _settings_load
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
# ONLY the first argument selects the subcommand: scanning every argument
# would read the word `resume` out of a title and exec dispatch-resume with
# the rest of the title as its flags (#349). Subcommand flags belong after
# the subcommand — `dispatch resume --agent pi` — as the usage line shows.
if [ "${1:-}" = resume ]; then
  shift
  exec dispatch-resume "$@"
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
pr_number=""
pr_body=""
parent_issue=""
base_ref=""
base_flag=""
add_dir_flags=()
add_dirs=()
owner_auth=""
owner_auth_set=""
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
  --)
    # End-of-options separator: every token after it is title text, so a title
    # containing a flag-shaped word (e.g. --base) passes verbatim (#349).
    shift
    break
    ;;
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
  --base)
    base_flag="${2:-}"
    [ -n "$base_flag" ] || {
      echo "dispatch: --base needs a ref" >&2
      exit 1
    }
    shift 2
    ;;
  --add-dir)
    [ -n "${2:-}" ] || {
      echo "dispatch: --add-dir needs a directory" >&2
      exit 1
    }
    add_dir_flags+=("$2")
    shift 2
    ;;
  --owner-auth)
    [ $# -ge 2 ] || {
      echo "dispatch: --owner-auth needs the owner's quoted words and their scope" >&2
      exit 1
    }
    owner_auth="$2"
    owner_auth_set=1
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
  --parent)
    parent_issue="${2:-}"
    [ -n "$parent_issue" ] || {
      echo "dispatch: --parent needs an issue number" >&2
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
    if [[ $1 =~ ^[A-Z]{2,}-[0-9]+$ ]]; then
      linear_id="$1"
      shift
    elif [[ $1 =~ ^#?[0-9]+$ ]]; then
      # Whole-string match, then one canonical decimal form: grep matched per
      # line, so a multi-line value like $'foo\n42' passed and a leading-zero
      # '042' became a branch/claim key gh resolves as #42 (#320).
      gh_issue="$(_canonical_number "$1")"
      [ -n "$gh_issue" ] || {
        echo "dispatch: issue number must be a positive integer (got '$1')" >&2
        exit 1
      }
      shift
    elif [[ $1 =~ ^[#0-9[:space:]]+$ ]]; then
      # Digits, '#' and whitespace only, yet not a valid issue number: a
      # mangled tracker token ('4 2', '# 42') is refused rather than silently
      # treated as a title, which would mint a second issue for it.
      echo "dispatch: '$1' is not a valid issue number — pass a single decimal integer, '#N' or 'N'" >&2
      exit 1
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
  if [[ ! $pr_number =~ ^[0-9]+$ ]]; then
    echo "dispatch: --pr needs a PR number" >&2
    exit 1
  fi
  pr_number="$(_canonical_number "$pr_number")"
  [ -n "$pr_number" ] || {
    echo "dispatch: --pr needs a positive integer" >&2
    exit 1
  }
  if [ -n "$linear_id" ] || [ -n "$gh_issue" ]; then
    echo "dispatch: --pr cannot combine with a Linear id or GitHub issue token" >&2
    exit 1
  fi
fi

# --base and --pr are mutually exclusive: --pr already resolves the base from
# the PR's baseRefName, so a second source would be ambiguous. Pure validation,
# before the claim, the lock, or any worktree/window.
if [ -n "$base_flag" ] && [ -n "$pr_number" ]; then
  echo "dispatch: --base cannot combine with --pr (--pr already fixes the base from the PR)" >&2
  exit 1
fi

if [ -n "$parent_issue" ]; then
  if [[ $parent_issue =~ ^#?[0-9]+$ ]]; then
    parent_issue="$(_canonical_number "$parent_issue")"
  else
    parent_issue=""
  fi
  [ -n "$parent_issue" ] || {
    echo "dispatch: --parent needs a positive issue number" >&2
    exit 1
  }
  if [ -n "$linear_id" ] || [ -n "$gh_issue" ] || [ -n "$pr_number" ]; then
    echo "dispatch: --parent only applies to a minted issue — drop it, or drop the Linear id, issue token or --pr" >&2
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
  echo 'dispatch: no crew id — run '\''crew crews'\'' to find this repo'\''s crews and '\''crew adopt <id>'\'' to re-attach, or '\''crew new'\'' to start one; then pass --crew-id <id> or export CREW_ID' >&2
  exit 1
}

# Engine gate. An engine must be enabled (on this machine's roster) and
# available (its CLI installed). The roster is $DISPATCH_ENGINES when set,
# else the resolved `engines` setting (the home-manager module's locked layer,
# then the user file); unset everywhere means every engine, so a non-Nix
# checkout and the test suite need no extra setup. `profile` no longer gates
# engines — it is still read below for the work+claude+deep rung.
_settings_load
profile="$(jq -r '.profile // "personal"' <<<"$settings")"

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
      echo "dispatch: model '$model' does not match --agent cursor — cursor needs a full model id (e.g. kimi-k3-high, grok-4.7-medium, composer-2.5, claude-opus-5-high). Did you mean --agent claude? See dispatch-orchestration.md \"Model gate\"." >&2
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
      echo "dispatch: model '$model' does not match --agent pi — pi takes a provider-qualified model id (e.g. openrouter/deepseek/deepseek-v4.1-flash). See dispatch-orchestration.md \"Model gate\"." >&2
      exit 1
    fi
    ;;
  esac
fi

# Escalation helpers — query the bus for prior failed workers. The
# one-rung-up hops themselves live in defaults.json (_escalation_hop). The
# rules there encode three limits: a trivial row never escalates above itself
# (#249); standard/trivial opus has no in-row rung above it, so the dispatcher
# re-tiers that failure to deep; and pi deep has none, so a failed deep pi
# worker is re-dispatched on another engine.
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

# Pre-compute the branch and crew_dir for escalation checks (normally
# computed after this gate). At this point $* is the title.
# A Linear id overrides the issue-derived branch at the identity step below, so
# it has no failed history under the issue's branch name — no escalation.
if [ -n "$gh_issue" ] && [ -z "$linear_id" ]; then
  _escalation_slug="$(printf '%s' "$*" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g' | cut -c1-40 | sed -E 's/^-+//; s/-+$//')"
  _escalation_branch="feat/$gh_issue-$_escalation_slug"
  _escalation_crew_dir="$(git rev-parse --path-format=absolute --git-common-dir)/crew"
else
  _escalation_branch=""
  _escalation_crew_dir=""
fi

# Tier↔model gate (#89). Enforces tier-appropriateness on top of
# the dispatchability gate above — see dispatch-orchestration.md
# "Tier map". The rows live in defaults.json's modelMap, read through the
# settings resolver; a tier without a row admits nothing (fail closed).
# DISPATCH_SKIP_MODEL_CHECK does not cover this gate (it is
# about shape/cache staleness, not tier); --ignore-map does.
if [ -z "$ignore_map" ]; then
  tier_ok=0
  _model_in_row "$agent" "$tier" "$model" && tier_ok=1
  if [ "$tier_ok" = 0 ]; then
    # Escalation: if the model is not in the tier's row but IS the one-rung-up
    # target from the failed model, AND a prior worker ended failed, allow it.
    if [ -n "${_escalation_branch:-}" ]; then
      failed_model="$(_prior_failed_model "$_escalation_branch" "$_escalation_crew_dir" "$tier")"
      if [ -n "$failed_model" ]; then
        escalation_baseline="$(_escalation_hop "$agent" "$tier" "$failed_model" "$model" outOfRow)"
        if [ -n "$escalation_baseline" ] && _prior_failed_escalation_available "$_escalation_branch" "$_escalation_crew_dir"; then
          tier_ok=1
          escalated_from="$escalation_baseline"
        fi
      fi
    fi
    if [ "$tier_ok" = 0 ]; then
      tier_expected="$(_row_expected "$agent" "$tier")"
      echo "dispatch: model '$model' is not $tier's row for --agent $agent — expected $tier_expected, or pass --ignore-map (the human's model decision). See dispatch-orchestration.md \"Tier map\"." >&2
      exit 1
    fi
  fi
fi

# Record-only escalation: model already in tier's row but one rung up from failed.
# Stamp WORKER_TASK.md only (dispatch event skips it — the gate already passed).
if [ "${escalated_from:-}" = "" ] && [ -z "$ignore_map" ] && [ "$tier_ok" = 1 ] && [ -n "${_escalation_branch:-}" ]; then
  failed_model="$(_prior_failed_model "$_escalation_branch" "$_escalation_crew_dir" "$tier")"
  if [ -n "$failed_model" ]; then
    record_from="$(_escalation_hop "$agent" "$tier" "$failed_model" "$model" inRow)"
    [ -z "$record_from" ] || escalated_from="$record_from (record only)"
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

# Lead budget gate. budget_stop owns the >=95% predicate and its cache rules
# (fail open when stale/missing/silent; skip a window already past resets_at);
# the blocks below add the claude-blind warning and the codex absolute-limit
# gate. --ignore-budget is the manual escape hatch (e.g. credits cover it).
budget_file="${XDG_DATA_HOME:-$HOME/.local/share}/crew/engine-budget.json"
budget_stop "$agent"
if [ -z "$ignore_budget" ] && [ -f "$budget_file" ]; then
  now_ts="$(date +%s)"
  stale_before=$((now_ts - 7200))
  if [ "$agent" = claude ] && jq -e --argjson stale_before "$stale_before" --argjson now "$now_ts" \
    '.fetched_epoch >= $stale_before and .engines.claude == null' "$budget_file" >/dev/null 2>&1; then
    echo "dispatch: budget gate blind: claude quota unknown" >&2
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
  codex_abs=$(jq -r --argjson stale_before "$stale_before" --argjson now "$now_ts" '
    if .fetched_epoch < $stale_before then empty
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
# no spec/plan phase, so it gets no critic grid; only a pi review worker above
# trivial defaults to `reviewer,refuter` panes, since pi has no native subagents
# to fan out the reviewer batch and the per-finding refuters REVIEW_TASK.md
# requires (an explicit --grid derives the same pair on any engine, additive
# there). A non-pi deep dispatch whose plan is already provided gets no default
# grid either, since that leaves no critic role to default to.
grid_default_non_pi=""
if [ -z "$grid_roles" ] && [ -z "$grid_flag" ] && [ -z "$no_grid" ]; then
  if [ "$agent" = pi ] && [ "$tier" != trivial ]; then
    grid_flag=1
  elif [ "$kind" = review ]; then
    : # no spec/plan phase, so no critic grid
  elif [ "$tier" = deep ] && [ "$plan_val" != provided ]; then
    grid_flag=1
    grid_default_non_pi=1
  fi
fi
if [ -z "$grid_roles" ] && [ -n "$grid_flag" ]; then
  if [ "$kind" = review ]; then
    [ "$tier" = trivial ] || grid_roles="reviewer,refuter"
  elif [ -n "$grid_default_non_pi" ]; then
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
    budget_stop "${role_agents[$i]}" "${role_names[$i]}"
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
# A title reaches git as a branch slug and the task doc as a header; a newline
# in it is malformed input, not a title, and refusing it here keeps a
# per-line-validating artifact from ever entering the pipeline (#320).
[[ $title != *$'\n'* ]] || {
  echo "dispatch: the title must not contain a newline" >&2
  exit 1
}

# The "## " ban protects the carried-resume boundary: a "## Task" line inside
# the text would start the copy early.
if [ -n "$owner_auth_set" ] && [ -z "${owner_auth//[[:space:]]/}" ]; then
  echo "dispatch: --owner-auth needs the owner's quoted words and their scope" >&2
  exit 1
fi
if [ "${#owner_auth}" -gt 2000 ]; then
  echo "dispatch: --owner-auth is ${#owner_auth} chars (max 2000) — quote the owner's words and the scope they cover, nothing else" >&2
  exit 1
fi
if grep -q '^## ' <<<"$owner_auth"; then
  echo "dispatch: --owner-auth text may not contain a line starting with \"## \"" >&2
  exit 1
fi
if [ -n "${DISPATCH_SPEC:-}" ] && [ -f "${DISPATCH_SPEC:-}" ] && grep -qiE '^#+[[:space:]]*owner[[:space:]]+authori[sz]ation' "$DISPATCH_SPEC"; then
  echo "dispatch: DISPATCH_SPEC carries an ## Owner authorization section — pass the owner's words with --owner-auth so they appear in the dispatch command itself" >&2
  exit 1
fi

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

# A minted issue's body is the spec's summary — its text before the first `##`
# heading, written for someone outside this crew. The rest of the spec is the
# worker's own brief and stays out of the tracker. Past the precheck, so a resume
# (which never mints) is not held to it, and before the first side effect, so a
# refusal leaves nothing behind.
mint_body=""
if [ -z "$linear_id" ] && [ -z "$gh_issue" ] && [ -z "$pr_number" ]; then
  if [ -n "${DISPATCH_SPEC:-}" ] && [ -f "$DISPATCH_SPEC" ]; then
    mint_body="$(awk '/^## /{exit} {print}' "$DISPATCH_SPEC" | sed '/./,$!d')"
  fi
  [ -n "$mint_body" ] || {
    echo "dispatch: minting an issue needs a summary — open \$DISPATCH_SPEC with a paragraph for an outside reader (before its first '## ' heading), or pass an existing issue number" >&2
    exit 1
  }
  _mint_leak_check "$mint_body"
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

# Entry guard (#557): refuse a drifted config now, before `gh issue create`
# mints an issue that a later refusal would strand.
if ! _wt_cfg_baseline_init "${crew_dir%/crew}" || ! _wt_cfg_guard_cwd "${crew_dir%/crew}"; then
  exit 1
fi

for add_dir in "${add_dir_flags[@]}"; do
  canonical_dir="$(_add_dir_ok "$add_dir")" || {
    roots_now=none
    [ -z "${DISPATCH_GRANT_ROOTS//:/}" ] || roots_now="$DISPATCH_GRANT_ROOTS"
    echo "dispatch: --add-dir '$add_dir' refused — must be an existing absolute directory inside a configured grant root (programs.dispatcher.grantRoots / DISPATCH_GRANT_ROOTS, now: $roots_now) — ask the human to add a root, never set it inline; not /, \$HOME or an ancestor of it, not inside or above the crew dir, not a secrets/credentials dir, not a dir containing a .git or .claude entry, not inside or above a containing repo's git hooks dir (core.hooksPath or the default) or git dir, not holding a repo's git config file or a symlink into or over its hooks/git dir" >&2
    exit 1
  }
  add_dirs+=("$canonical_dir")
done

# Hoisted above the claim gate (#73): the gate keys its resume exemption on the
# resolved branch and records the claim to the bus under $crew_dir. All three are
# pure — string work, one `git rev-parse`, one `mkdir -p` — so the gate keeps its
# stated property of running before ANY scaffolding.
if [ -n "$gh_issue" ]; then
  branch="feat/$gh_issue-$slug"
fi

# A numeric --base is a PR to stack on: resolved to its head branch before the
# claim gate and mint mode's `gh issue create`, so a refused PR (merged, closed,
# fork head) never strands a `dispatched` label or mints an orphan issue.
if [ -n "$base_flag" ] && [[ $base_flag =~ ^[0-9]+$ ]]; then
  base_flag="$(_canonical_number "$base_flag")"
  [ -n "$base_flag" ] || {
    echo "dispatch: --base PR number must be a positive integer" >&2
    exit 1
  }
  base_pr_json=$(gh pr view "$base_flag" --json headRefName,state,isCrossRepository) || {
    echo "dispatch: --base $base_flag: could not resolve PR $base_flag" >&2
    exit 1
  }
  base_pr_head=$(printf '%s' "$base_pr_json" | jq -r '.headRefName // empty')
  base_pr_state=$(printf '%s' "$base_pr_json" | jq -r .state)
  base_pr_cross=$(printf '%s' "$base_pr_json" | jq -r .isCrossRepository)
  [ -n "$base_pr_head" ] || {
    echo "dispatch: --base $base_flag: could not resolve headRefName for PR $base_flag" >&2
    exit 1
  }
  [ "$base_pr_state" = OPEN ] || {
    echo "dispatch: --base $base_flag: PR is $base_pr_state — stack on an open PR or a branch" >&2
    exit 1
  }
  [ "$base_pr_cross" = false ] || {
    echo "dispatch: --base $base_flag: PR head is on a fork — a stacked PR's base must be a branch in this repo" >&2
    exit 1
  }
  base_flag="$base_pr_head"
fi

# --base <ref>: stack this worker on an unmerged branch instead of the default
# branch. The ref is fetched and its oid pinned here — before the claim gate and
# mint mode's `gh issue create` — so an unresolvable ref leaves no claim side
# effects. Runs on a resume too, because the stamped `base:` is what the worker's
# review diff, gate scope and PR target; a re-dispatch must not silently drop it.
if [ -n "$base_flag" ]; then
  if ! _fetch_origin_branch "$base_flag"; then
    echo "dispatch: --base '$base_flag' is not a plain branch name or could not be fetched from origin" >&2
    exit 1
  fi
  base_oid="$(git rev-parse --verify --quiet "origin/$base_flag^{commit}")" || {
    echo "dispatch: --base '$base_flag' does not resolve to a commit on origin — refusing to scaffold" >&2
    exit 1
  }
  base_ref="$base_flag"
  create_base_label="origin/$base_flag"
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
        exit 1
      fi
      # The label alone is no proof of a live crew: an interrupted dispatch
      # strands it (#304). Refuse only on evidence, and fail closed when the
      # evidence cannot be read. Not a lock — two heals can still race, and a
      # crew in another clone is invisible until it pushes a feat/N-* branch.
      claim_evidence="$(_claim_evidence "$gh_issue")"
      if [ -n "$claim_evidence" ]; then
        echo "dispatch: issue #$gh_issue is already claimed (carries the 'dispatched' label; $claim_evidence) — another crew is on it. If that crew is gone, remove the label by hand and retry." >&2
        exit 1
      fi
      echo "dispatch: issue #$gh_issue carries a stale 'dispatched' claim (no branch, remote branch or dispatch row for feat/$gh_issue-*) — re-claiming." >&2
    fi
  fi
  # The exemption skips the refusal only. --add-label is idempotent and runs on
  # both paths, which is what makes a reap-driven resume->create downgrade below
  # harmless: whichever mode this run ends in, the issue is labelled.
  # An explicit claim record, because `crew adopt` cannot infer one: the
  # kind:"dispatch" row carries no issue number and is written ~300 lines later,
  # so every failure in between would strand an unreleasable label (#73).
  # Written before the label so a row exists whenever the label does — a racing
  # dispatcher that sees the label must also see a claimant. A failed label write
  # below therefore leaves the row behind, but it is inert: its pid exits with
  # this dispatcher, and any later reuse of that pid belongs to a process that
  # started after the row's ts, so it never reads as claimant evidence (#322).
  line=$(jq -nc --arg crew "$crew_id" --arg issue "$gh_issue" --arg branch "$branch" --arg pid "$$" \
    '{ts:(now*1000|floor), crew_id:$crew, kind:"claim-issue", issue:$issue, branch:$branch, pid:($pid|tonumber)}')
  _bus_append "$crew_dir/events.jsonl" "$line"
  gh issue edit "$gh_issue" --add-label dispatched || {
    echo "dispatch: could not claim issue #$gh_issue (adding the 'dispatched' label failed)" >&2
    exit 1
  }
fi

# Reclaim workers whose PR already landed, before adding another one. Cheapest
# possible cleanup schedule: no daemon, no timer, and it runs exactly when the
# worktree/window count is about to grow. Non-fatal by construction — a dispatch
# must never fail because cleanup of unrelated, already-merged work failed.
# Any worker still booting on a branch this reap could otherwise mistake for
# idle-done is protected by the claim write near `worker_id=` below.
crew reap --quiet || true

# A switch onto a tree a worker or PR author wrote (resume, --pr, a stacked
# --base parent) runs with --no-hooks: the operator's hooks run in this shell,
# and a devshell hook would evaluate that tree's flake.nix here (#558). Such a
# worker's .pre-commit-config.yaml appears once its own direnv devshell loads.
#
# A default-base create is operator-trusted, so it blanks only worktrunk's
# post-switch *tmux* hook: we drive tmux ourselves below, and the hook would
# otherwise open a second, undecorated shell window at the same worktree
# (#123). Its own `$CLAUDECODE` guard only covers a Claude-launched dispatcher,
# and setting CLAUDECODE here would leak Claude's identity into a codex/cursor
# worker. The devshell hook still runs there — it materializes
# .pre-commit-config.yaml, without which the worker cannot commit at all.
wt_post_switch='post-switch.tmux=""'

# --pr resolves the head ref; the switch itself happens after the gate below, so
# a refusal costs no worktree and no window.
if [ -n "$pr_number" ]; then
  pr_json=$(gh pr view "$pr_number" --json headRefName,headRefOid,baseRefName,isCrossRepository,state,mergeCommit,body) || {
    echo "dispatch: --pr $pr_number: could not resolve PR $pr_number" >&2
    exit 1
  }
  pr_body=$(printf '%s' "$pr_json" | jq -r '.body // empty')
  pr_state=$(printf '%s' "$pr_json" | jq -r .state)
  [ "$pr_state" = OPEN ] || {
    pr_merge_oid=$(printf '%s' "$pr_json" | jq -r '.mergeCommit.oid // empty')
    echo "dispatch: --pr $pr_number: PR is $pr_state${pr_merge_oid:+ (merge commit $pr_merge_oid)} — a worker attaches only to an open PR" >&2
    exit 1
  }
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
  _plain_branch_name "$head" || {
    echo "dispatch: PR $pr_number head '$head' is not a plain branch name" >&2
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
    parent_args=()
    [ -z "$parent_issue" ] || parent_args=(--parent "$parent_issue")
    url=$(gh issue create --assignee @me --title "$title" --body "$mint_body" ${parent_args[@]+"${parent_args[@]}"} 2>/dev/null || true)
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
    # Row before label, as on the existing-issue path above (and inert the same
    # way if the label write fails; see #322).
    line=$(jq -nc --arg crew "$crew_id" --arg issue "$num" --arg branch "$branch" --arg pid "$$" \
      '{ts:(now*1000|floor), crew_id:$crew, kind:"claim-issue", issue:$issue, branch:$branch, pid:($pid|tonumber)}')
    _bus_append "$crew_dir/events.jsonl" "$line"
    gh issue edit "$num" --add-label dispatched || {
      echo "dispatch: created issue #$num but could not claim it (adding the 'dispatched' label failed)" >&2
      exit 1
    }
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

    if [ -n "$base_flag" ]; then
      # Already resolved and pinned above.
      create_base_oid="$base_oid"
    else
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
      _fetch_origin_branch "$default_branch" || {
        echo "dispatch: default branch '$default_branch' is not a plain branch name or could not be fetched from origin" >&2
        exit 1
      }
      # Pinned now, not re-resolved at switch time below: the occupancy/reclaim
      # gate in between shells out to crew/jq, giving a concurrent fetch a window
      # to move the floating ref — pinning keeps what's branched and what the
      # success line reports from ever diverging.
      create_base_oid="$(git rev-parse "origin/$default_branch")"
      create_base_label="origin/$default_branch"
    fi
    create_base_short="$(git rev-parse --short "$create_base_oid")"
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

# Main-context guard (#557), right before the `wt switch` calls below. Also
# guard $prev_wt's admin dir: worktrunk may run git there on attach. The
# primary worktree reads the common config, already guarded.
_wt_cfg_guard_cwd "${crew_dir%/crew}" || exit 1
if [ -n "$prev_wt" ]; then
  # No `exit` in the awk: an early close SIGPIPEs git and trips pipefail.
  primary_wt="$(git worktree list --porcelain | awk '/^worktree /{if (!p) p=$2} END{print p}')"
  if [ "$prev_wt" != "$primary_wt" ]; then
    prev_admin="$(_wt_admin_dir "${crew_dir%/crew}" "$prev_wt")" || {
      echo "dispatch: no git admin dir for $prev_wt" >&2
      exit 1
    }
    _wt_cfg_guard "${crew_dir%/crew}" "$prev_admin" || exit 1
  fi
fi

case "$switch_mode" in
create)
  if [ -n "$base_flag" ]; then
    wt switch -c "$branch" -b "$create_base_oid" -y --no-hooks
  else
    wt switch -c "$branch" -b "$create_base_oid" -y --config-set "$wt_post_switch"
  fi
  echo "dispatch: created branch $branch from $create_base_label ($create_base_short)"
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
  wt switch "$branch" -y --no-hooks
  branch_short="$(git rev-parse --short "$branch")"
  echo "dispatch: resuming branch $branch at $branch_short"
  ;;
name) wt switch "$branch" -y --no-hooks ;;
fetch-name)
  # A PR head branch is attacker-named; see _fetch_origin_branch.
  _fetch_origin_branch "$branch" || {
    echo "dispatch: PR head '$branch' is not a plain branch name or could not be fetched from origin" >&2
    exit 1
  }
  wt switch "$branch" -y --no-hooks
  ;;
pr-ref) wt switch "pr:$pr_number" -y --no-hooks ;;
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

# Every git call into the worktree below goes through this admin dir, never
# through the worktree's own (worker-writable) gitlink (#539).
wt_admin="$(_wt_admin_dir "${crew_dir%/crew}" "$wt_path")" || {
  echo "dispatch: no git admin dir for $wt_path" >&2
  exit 1
}

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
  worktree_head="$(_wt_git "$wt_admin" "$wt_path" rev-parse HEAD)"
  if [ "$worktree_head" != "$head_oid" ]; then
    # A worker's own WORKER_TASK.md is intentionally untracked and is only
    # trashed by `crew reap`, not on reclaim, so it alone must not count as
    # dirty. Captured first so the grep's `|| true` cannot mask a failed
    # status as clean.
    st="$(_wt_status "$wt_admin" "$wt_path")" || {
      echo "dispatch: git status failed for $wt_path — refusing to reset" >&2
      exit 1
    }
    dirt="$(printf '%s\n' "$st" | grep -v '^?? WORKER_TASK\.md$' || true)"
    if [ -z "$dirt" ]; then
      echo "dispatch: worktree HEAD $worktree_head != PR $pr_number head $head_oid — fetching and hard-resetting" >&2
      _fetch_origin_branch "$head" || {
        echo "dispatch: PR head '$head' is not a plain branch name or could not be fetched from origin" >&2
        exit 1
      }
      _wt_git "$wt_admin" "$wt_path" reset --hard "$head_oid"
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
  --arg worker "$worker_id" \
  --arg engine "$agent" --arg model "$model" --arg tier "$tier" --arg effort "$effort" \
  --arg shape "$dispatch_shape" --arg title "$title" --arg task_kind "$kind" \
  --arg plan "$plan_val" --argjson resume "$([ "$switch_mode" = resume ] && echo true || echo false)" \
  --argjson ident "$ident" \
  --arg escalated_from "$escalated_from_event" \
  --argjson owner_auth "$([ -n "$owner_auth" ] && echo true || echo false)" \
  '{ts:(now*1000|floor), crew_id:$crew, kind:"dispatch", branch:$branch, session:$session, worker_id:$worker, engine:$engine, model:$model, tier:$tier, effort:$effort, shape:$shape, task_kind:$task_kind, title:$title, plan:$plan, resume:$resume, owner_auth:$owner_auth} + $ident
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
  # The carried body is worker-writable, so a heading planted in it never counts as
  # an authorization — only a launch prompt (--owner-auth) does.
  carried_stripped="$(printf '%s\n' "$carried" | awk '
    {
      line = $0
      ll = tolower(line)
      if (skip) {
        if (match(line, /^#+[[:space:]]/) && RLENGTH - 1 <= lvl) { skip = 0 } else { next }
      }
      if (!skip && ll ~ /^#+[[:space:]]*owner[[:space:]]+authori[sz]ation/) {
        match(line, /^#+/); lvl = RLENGTH; skip = 1; next
      }
      print line
    }
  ')"
  if [ "$carried_stripped" != "$carried" ]; then
    echo "dispatch: dropped an owner-authorization section from the carried task — it was not in a launch prompt; pass --owner-auth to carry one" >&2
  fi
  carried="$carried_stripped"
fi

# A re-dispatch onto an existing branch (switch_mode=resume) is a resume, so
# keep the `base:` the first dispatch pinned unless this invocation supplies its
# own (--base; --pr never takes this mode). Read before the header write below
# truncates the file, exactly like $carried above.
if [ "$switch_mode" = resume ] && [ -z "$base_ref" ] && [ -f "$wt_path/WORKER_TASK.md" ]; then
  carried_base="$(sed -nE '/^$/q; s/^base: //p' "$wt_path/WORKER_TASK.md" | head -1)"
  [ -n "$carried_base" ] && base_ref="$carried_base"
fi

# Keep a stamp already on the task file. A missing or unrecognised line stays
# absent so a map change cannot retarget an in-flight task. No file means a
# pruned worktree: there is nothing to keep, so resolve.
tracker_stamp=""
if [ "$switch_mode" = resume ] && [ -f "$wt_path/WORKER_TASK.md" ]; then
  carried_tracker="$(sed -nE '/^$/q; s/^tracker: //p' "$wt_path/WORKER_TASK.md" | head -n 1)"
  if [ "$carried_tracker" = github ] || [[ $carried_tracker =~ ^linear\ [A-Z][A-Z0-9]*$ ]]; then
    tracker_stamp="$carried_tracker"
  fi
else
  tracker_stamp="$(_resolve_tracker)"
fi
if [ -n "$pr_number" ] && [ -n "$pr_body" ]; then
  if [[ $tracker_stamp =~ ^linear\ [A-Z][A-Z0-9]*$ ]] &&
    grep -qiE 'closes[[:space:]]*#[0-9]+' <<<"$pr_body"; then
    echo "dispatch: warning: tracker is $tracker_stamp but PR #$pr_number body closes a GitHub issue — stamp unchanged" >&2
  elif [ "$tracker_stamp" = github ] &&
    grep -qE 'Closes[[:space:]]*[A-Z]{2,}-[0-9]+' <<<"$pr_body"; then
    echo "dispatch: warning: tracker is github but PR #$pr_number body closes a Linear ticket — stamp unchanged" >&2
  fi
fi

# The grant record, not the header's add_dir: mirror, is what every claude
# launch reads (launch_dir_args): the worker edits WORKER_TASK.md, so the doc
# cannot be the authority. Same precedence as base: above — a re-dispatch with
# no --add-dir keeps the recorded set; anything else rewrites it, and an empty
# set truncates it so a stale record of an old same-named branch cannot leak.
grants_dir="$crew_dir/grants"
grant_record="$grants_dir/$branch"
# Every dir from grants/ down to a slashed branch's leaf: mkdir, chmod, mktemp
# and mv below all follow a symlink planted at any of them.
grant_parent="$grants_dir"
IFS=/ read -ra grant_parts <<<"$branch"
for grant_part in "" "${grant_parts[@]:0:${#grant_parts[@]}-1}"; do
  grant_parent="$grant_parent${grant_part:+/$grant_part}"
  if [ -L "$grant_parent" ] || { [ -e "$grant_parent" ] && [ ! -d "$grant_parent" ]; }; then
    echo "dispatch: $grant_parent is a symlink or not a directory — refusing to write a grant record" >&2
    exit 1
  fi
done
if [ -L "$grant_record" ] || { [ -e "$grant_record" ] && [ ! -f "$grant_record" ]; }; then
  echo "dispatch: $grant_record is a symlink or not a regular file — refusing to use it as a grant record" >&2
  exit 1
fi
# Refuse an unsafe lead record here too, before the worktree is half set up:
# the lead launch below writes it.
if ! _lead_record_safe; then
  echo "dispatch: $crew_dir/leads/$branch or a dir above it is a symlink or not a regular file/directory — refusing to record the lead session" >&2
  exit 1
fi
# A re-dispatch is a new lead: overwrite an older same-named branch's record now,
# so an abort before the launch cannot leave resume attaching to the old
# conversation (same reason the grant record above is rewritten). Any dispatch
# that reaches here is a post-records lead; the launch overwrites the tombstone,
# and a dispatch that aborts first must not look like a pre-records worker.
_record_lead_session pending -
if [ "$switch_mode" = resume ] && [ "${#add_dir_flags[@]}" -eq 0 ] && [ -f "$grant_record" ]; then
  mapfile -t add_dirs < <(sed '/^$/d' "$grant_record")
fi
# Written to a temp file and renamed into place: mv replaces a symlink planted
# at the predictable path instead of writing through it. The umask makes every
# dir mkdir creates between grants/ and a slashed branch's leaf 0700 too.
(
  umask 077
  mkdir -p "$(dirname "$grant_record")"
  chmod 700 "$grants_dir" "$(dirname "$grant_record")"
  grant_tmp="$(mktemp "$(dirname "$grant_record")/.grant.XXXXXX")"
  if [ "${#add_dirs[@]}" -gt 0 ]; then
    printf '%s\n' "${add_dirs[@]}" >"$grant_tmp"
  fi
  mv -f -- "$grant_tmp" "$grant_record"
)

# A lazy --spawn-role takes its dirs from this record, never the worker's env (#496).
if bad="$(_protocol_dirs_record_bad)"; then
  echo "dispatch: $bad is a symlink or the wrong type — refusing to write the protocol-dirs record" >&2
  exit 1
fi
_record_protocol_dirs "$wt_path"
_record_worktree_anchor "$wt_path" "$wt_admin"

# Stamp the task file: header fields the worker protocol reads, the closes
# line, and the full task body from $DISPATCH_SPEC (falls back to the title).
# The review contract is appended so the dispatcher never re-authors it as
# per-worker prose.
{
  printf 'tier: %s\nkind: %s\ndraft: %s\nengine: %s\nmodel: %s\neffort: %s\n' \
    "$tier" "$kind" "$draft" "$agent" "$model" "$effort"
  [ -n "${escalated_from:-}" ] && printf 'escalated_from: %s\n' "$escalated_from"
  printf 'mcp: %s\nplan: %s\ntitle: %s\n%s\n' \
    "$mcp_profile" "$plan_val" "$title" "$closes"
  [ -n "$tracker_stamp" ] && printf 'tracker: %s\n' "$tracker_stamp"
  printf 'dispatcher_pane: %s\ncrew_dir: %s\ncrew_id: %s\nagent_name: %s\nworker_id: %s\nprotocol_dir: %s\n' \
    "${TMUX_PANE:-}" "$crew_dir" "$crew_id" "$agent_name" "$worker_id" "$PROTOCOL_DIR"
  if [ -n "$base_ref" ]; then
    printf 'base: %s\n' "$base_ref"
  fi
  for add_dir in "${add_dirs[@]}"; do
    printf 'add_dir: %s\n' "$add_dir"
  done
  if [ "$switch_mode" = resume ]; then
    printf 'resume: true\n'
  fi
  # The role grid the lead should delegate to (absent = single-agent pipeline).
  [ -n "$roles_stamp" ] && printf 'roles: %s\n' "$roles_stamp"
  # A lazy grid creates no role panes up front; the lead spawns each at its seam.
  [ -n "$grid_lazy" ] && printf 'lazy: 1\n'
  [ -n "$owner_auth" ] && printf '\n## Owner authorization\n\n%s\n' "$owner_auth"
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

# Ensure WORKER_TASK.md is excluded from tracking across every worktree of this
# repo (info/exclude is per-repo, not per-worktree). Append only if missing;
# idempotent; never clobber. This keeps a worker's own scaffolding doc out of
# commits regardless of which repo dispatch targets.
exclude_file="$(git rev-parse --git-common-dir)/info/exclude"
if ! grep -qxF 'WORKER_TASK.md' "$exclude_file" 2>/dev/null; then
  printf '\n%s\n' 'WORKER_TASK.md' >>"$exclude_file"
fi

# Exclude rules do not apply to a file git already tracks, so once a
# WORKER_TASK.md slips into the base being dispatched the guard above is
# silently void — the worker's stamped doc rides its diff into every commit
# until someone removes it (#397). Warn, naming the fix; never touch the index
# here (removing a tracked file is the target repo's job, in its own PR). The
# check is against the dispatched worktree, which sits at that base for a
# create. Plain `ls-files` prints the path only if tracked, so a non-zero exit
# means the index was unreadable or _wt_git refused, which must not pass as
# "untracked" (#539).
tracked="$(_wt_git "$wt_admin" "$wt_path" ls-files -- WORKER_TASK.md)" || {
  echo "dispatch: could not read the index at $wt_path" >&2
  exit 1
}
if [ -n "$tracked" ]; then
  echo "dispatch: warning: WORKER_TASK.md is tracked at the base being dispatched — .git/info/exclude cannot hide a tracked file, so it will ride into this worker's commits. Remove it in its own PR: git rm --cached WORKER_TASK.md" >&2
fi

# Record resolved role specs so a lazy grid's lead can spawn each role on demand
# (`dispatch --spawn-role`), and so a role can be re-created after death.
if [ "${#role_names[@]}" -gt 0 ]; then
  roles_dir="$crew_dir/artifacts/$branch"
  if bad="$(_artifacts_dir_bad "$branch")"; then
    echo "dispatch: $bad is a symlink or not a directory — refusing to write roles.json" >&2
    exit 1
  fi
  # mv would drop the temp file inside a directory (or a symlink to one) at roles.json.
  if [ -d "$roles_dir/roles.json" ]; then
    echo "dispatch: $roles_dir/roles.json is a directory — refusing to write roles.json" >&2
    exit 1
  fi
  mkdir -p "$roles_dir"
  # Renamed into place: a redirect would write through a symlink planted at roles.json.
  roles_tmp="$(mktemp "$roles_dir/.roles.XXXXXX")"
  for i in "${!role_names[@]}"; do
    jq -n --arg n "${role_names[$i]}" --arg a "${role_agents[$i]}" --arg m "${role_models[$i]}" --arg e "${role_efforts[$i]}" '{name:$n,agent:$a,model:$m,effort:$e}'
  done | jq -s 'map({key:.name,value:{agent:.agent,model:.model,effort:.effort}})|from_entries' >"$roles_tmp"
  mv -f -- "$roles_tmp" "$roles_dir/roles.json"
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

# Cross-repo dispatch (#398, #420): the crew bus is per repo, so print the
# worker-repo lane command when the dispatcher's pane sits elsewhere and that
# bus is not being streamed. The detection is one sourced helper whose store
# path flake.nix bakes, shared with dispatch-resume.sh; a raw run without the
# override skips it rather than aborting under set -e (the hint is advisory).
hint_lib="${CROSS_REPO_HINT_LIB:-@crossRepoHintLib@}"
if [ -r "$hint_lib" ]; then
  # shellcheck source=/dev/null
  . "$hint_lib"
  cross_repo_hint "${crew_dir%/crew}" "$crew_id"
fi

# Identity surfaces: codename on the pane border + the CC prompt box (--name).
# lazytmux owns the tab text; @crew_* tint the status-bar tab.
tmux set-window-option -t "$win" @crew_name "$agent_name"
# --spawn-role finds its crew dir and branch here, not via git discovery, which
# the worker's env and worktree .git steer (#496).
tmux set-window-option -t "$win" @crew_dir "$crew_dir"
tmux set-window-option -t "$win" @crew_branch "$branch"
tmux set-window-option -t "$win" @crew_color "$agent_color"
tmux set-window-option -t "$win" pane-border-style "bg=#{@thm_bg},fg=$agent_color"
tmux set-window-option -t "$win" pane-active-border-style "bg=#{@thm_bg},fg=$agent_color,bold"
# A grid lead's window border carries a "lead" marker plus the live state; role
# panes label themselves at pane level, so this touches only the lead. @crew_name
# stays the bare codename — it is the occupancy join key.
if [ "${#role_names[@]}" -gt 0 ]; then
  publish_grid_lead "$win" "$pane"
  tmux set-option -p -t "$pane" @crew_state working 2>/dev/null || true
  tmux set-option -p -t "$pane" @crew_detail "" 2>/dev/null || true
  tmux set-option -p -t "$pane" @crew_source "" 2>/dev/null || true
else
  tmux set-window-option -t "$win" pane-border-format " #[bold]#{@crew_name}#[nobold] "
fi

# Deep claude workers get the read-only codex MCP for cross-model review
# (work profile only — mcp-codex.json is generated work-gated).
xreview_mcp=""
if [ "$profile" = work ] && [ "$agent" = claude ] && [ "$tier" = deep ]; then
  xreview_mcp="--mcp-config $HOME/.config/claude-code/mcp-codex.json"
fi

# When the dispatcher already wrote the plan into the task doc, say so in the
# launch prompt. A launch-prompt (user-turn) instruction is a "direct request",
# which satisfies using-superpowers' own escape hatch — so only the plan phase
# is skipped, not the gates that still run before push.
plan_note=""
if [ "$plan_val" = provided ]; then
  plan_note=" The task doc is your plan of record — extract the steps and implement; do not re-plan or re-critique the plan."
  if [ "$tier" != trivial ] && [ "$kind" != review ]; then
    plan_note="$plan_note Only planning is skipped: the fast deterministic gate and the code review gate still run before you push."
  fi
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
if [ "$kind" != review ] && [ "$tier" != trivial ]; then
  push_mandate=" Run your code review gate and record its review seam before you push.$push_mandate"
fi
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
if [ -n "$roles_stamp" ] && [ "$kind" = review ]; then
  grid_note=" You lead a review role grid: role panes ($roles_stamp) share this worktree and are parked on the crew bus. Follow REVIEW_TASK.md 'Role-grid path' — on pi the reviewer and refuter panes are your reviewer batch and refuters; on any other engine they are an additive second opinion next to your native batch."
elif [ -n "$roles_stamp" ]; then
  grid_note=" You lead a role grid: role panes ($roles_stamp) share this worktree and are parked on the crew bus. Follow WORKER_PROTOCOL.md 'Grid mode' — delegate to the bus only the phases that have a pane; your engine-native code-review gate still runs as usual (a reviewer pane is additive, except on pi where it is the gate)."
fi

# WORKER_PROTOCOL.md reaches pi/claude leads as a system prompt, so its
# "sibling" protocol files have no referent unless the directory is named.
protocol_note=" Protocol files (EVIDENCE_REVIEW.md, GRID_PROTOCOL.md, ...) live in $PROTOCOL_DIR — also stamped as protocol_dir: in WORKER_TASK.md."

owner_note=""
if [ -n "$owner_auth" ]; then
  owner_note=" Owner authorization, quoted by the dispatcher from the repo owner's own words in its session; it covers only the scope stated here: $owner_auth"
fi

# The recorded identity is replayed from the shared bus.
printf -v q_agent_name '%q' "$agent_name"

if [ "$agent" = codex ]; then
  # service_tier pinned: the interactive /fast toggle persists locally and would
  # otherwise leak into unattended workers, burning ChatGPT credits at 2.5x for
  # latency nobody is watching.
  # agents.*: enable native delegation, cap concurrency at 3 (parity with rule 1),
  # and pin subagent effort one rung down. Never pass ultra as subagent effort.
  prompt="Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}${grid_note}${protocol_note}${owner_note}"
  shell_quote quoted_prompt "$prompt"
  # codex and cursor cannot pre-assign a session id, so there is none to verify:
  # the record names the engine with `-`, replacing any earlier lead's record.
  _record_lead_session "$agent" -
  launch_cmd="${git_env}codex --profile worker -m $model -c model_reasoning_effort=$effort -c service_tier=default -c agents.enabled=true -c agents.max_concurrent_threads_per_session=3 -c agents.default_subagent_reasoning_effort=$codex_subagent_effort --dangerously-bypass-approvals-and-sandbox $quoted_prompt"
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
  prompt="Read $PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md, then run the task end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}${grid_note}${protocol_note}${owner_note}"
  shell_quote quoted_prompt "$prompt"
  _record_lead_session "$agent" -
  launch_cmd="${git_env}CURSOR_CLI_INDEXED_GREP=0 cursor-agent --force --trust --approve-mcps --disable-indexing --disable-codebase-ref --model '$model' $quoted_prompt"
elif [ "$agent" = pi ]; then
  # pi's interactive TUI keeps pane output live. It accepts a file path as a
  # real appended system prompt; --no-approve ignores project-local resources,
  # so the worktree's own skills go over via --skill (pi_skill_args).
  printf -v quoted_dir '%q' "$pi_agent_dir"
  prompt="Read WORKER_TASK.md and run it end-to-end.${push_mandate}${plan_note}${resume_note}${process_authority}${grid_note}${protocol_note}${owner_note}"
  shell_quote quoted_prompt "$prompt"
  lead_sid="$(_uuid)"
  _record_lead_session pi "$lead_sid"
  launch_cmd="${git_env}PI_CODING_AGENT_DIR=$quoted_dir pi --name $q_agent_name --model $model --thinking $effort --session-id $lead_sid --append-system-prompt $PROTOCOL_DIR/WORKER_PROTOCOL.md --no-approve$(pi_skill_args "$wt_path") $quoted_prompt"
else
  prompt="Read WORKER_TASK.md and run it end-to-end.${push_mandate}${plan_note}${resume_note}${grid_note}${protocol_note}${owner_note}"
  shell_quote quoted_prompt "$prompt"
  lead_sid="$(_uuid)"
  _record_lead_session claude "$lead_sid"
  launch_cmd="${git_env}claude --name $q_agent_name --model $model --effort $effort --session-id $lead_sid $mcp_flag $xreview_mcp$(launch_dir_args claude "$branch") --append-system-prompt-file $PROTOCOL_DIR/WORKER_PROTOCOL.md --permission-mode auto $quoted_prompt"
fi
write_launch_script launch_line "$launch_cmd"
tmux send-keys -t "$pane" "$launch_line" Enter

# Role grid: split the task window into one pane per role. Each role pane parks
# on the bus until the lead assigns it work; GRID_PROTOCOL.md is its system
# prompt. A role may run a different engine from the lead (cross-engine review).
# Split AFTER the lead launch so the lead keeps the first pane. A role pane gets
# only the prompt watch, never the liveness detectors: a parked role produces no
# output, which the pane-output watchdog would misread as a wedge.
if [ "${#role_names[@]}" -gt 0 ] && [ -z "$grid_lazy" ]; then
  publish_grid_window "$win"
  for i in "${!role_names[@]}"; do
    role="${role_names[$i]}"
    role_pane="$(split_role_pane "$win" "$wt_path" "$role" "$worker_id" "$crew_id")"
    launch_role "$role_pane" "$wt_path" "$role" "${role_agents[$i]}" "${role_models[$i]}" "${role_efforts[$i]}"
    watch_role "$role" "$role_pane" "${role_agents[$i]}"
    watch_role_prompts "$role" "$role_pane" "${role_agents[$i]}" "$crew_id"
  done
  refit_grid "$win"
fi

# Optional live status pane (--status): a bounded roster loop over the crew bus.
# A --lazy --status window gains a pane here even though it skipped the eager
# loop above, so it must publish the grid hint too.
if [ -n "$grid_status" ] && [ "${#role_names[@]}" -gt 0 ]; then
  publish_grid_window "$win"
  status_pane="$(split_role_pane "$win" "$wt_path" status "$worker_id" "$crew_id")"
  tmux send-keys -t "$status_pane" "while true; do clear; crew roster 2>/dev/null | jq -r '.[] | \"  \\(.state)  \\(.from)\"'; sleep 3; done" Enter
  refit_grid "$win"
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
