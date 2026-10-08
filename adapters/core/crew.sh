# shellcheck shell=bash
# File-based coordination bus: id | identity | status | msg | watch | roster | roster-render | inbox | stall-watch | pr-watch | log
# A real CLI on PATH (not a fish fn) so BOTH the dispatcher (fish) and workers
# (their bash tool) can call it. Pure jq + append; the log is the state. The
# shebang + `set -euo pipefail` are prepended by writeShellApplication, so this
# source omits them (the shell= directive above keeps standalone shellcheck happy).

# Index-aligned codename pool (FleetView-style). `dispatch` picks a free slot
# (hash of the branch, stepped forward past live workers) and records it on the
# dispatch event; the pure hash is only the fallback for a branch with no record.
# _tmuxc are mid-tone 256-palette codes chosen for legibility on BOTH Catppuccin
# Latte (light) and Mocha (dark): each clears ~2.5:1 contrast (most >3:1) on
# either base, so the badge lazytmux tints doesn't wash out when the theme flips.
# The old bright/pale set (colour44/141/220/154/117…) was dark-mode-only.
#
# 32 slots, not 16: identity is `cksum % pool`, so collisions follow the birthday
# bound — at 16 a 4-worker crew collided about half the time (one run put #158,
# #164 and #221 all on "sage"). 32 roughly halves it; `roster` still disambiguates
# whatever slips through for legacy events with no recorded name.
# The 16 added codes were picked by measured WCAG contrast against both bases
# (worst added: 2.86 mocha / 2.88 latte — above the 2.54 floor of the original 16).
_names=(sage atlas nova ember reef iris amber coral moss slate rust plum lime rose sky onyx
  pine lagoon indigo fern bronze violet khaki ash brick mauve tan crimson fuchsia blush orchid cobalt)
_colors=(green blue magenta orange teal purple yellow salmon olive steel rust plum lime pink sky grey
  seagreen darkcyan indigo forestgreen darkgoldenrod mediumpurple darkkhaki dimgrey indianred palevioletred peru crimson mediumvioletred hotpink orchid royalblue)
_tmuxc=(colour28 colour32 colour127 colour130 colour30 colour98 colour136 colour167 colour100 colour67 colour166 colour96 colour64 colour162 colour25 colour244
  colour29 colour31 colour61 colour65 colour94 colour97 colour101 colour102 colour131 colour132 colour137 colour160 colour163 colour168 colour169 colour68)

_identity_at() { # $1=slot -> {name,color,tmux}
  jq -nc --arg name "${_names[$1]}" --arg color "${_colors[$1]}" --arg tmux "${_tmuxc[$1]}" \
    '{name:$name, color:$color, tmux:$tmux}'
}

_identity_slot() { # $1=branch -> hash slot; cksum is POSIX (portable to macOS)
  local n
  n=$(printf '%s' "$1" | cksum | cut -d' ' -f1)
  echo $((n % ${#_names[@]}))
}

_identity() { # $1=branch -> {name,color,tmux}
  _identity_at "$(_identity_slot "$1")"
}

# _where_die <msg> — a `crew where` resolution failure: message on stderr and
# a non-zero exit.
_where_die() {
  echo "crew: where: $*" >&2
  exit 1
}

# _resolve_target <target> [crew] — one TSV line (branch, codename, host, crew)
# per branch the target names, from the latest `dispatch` row of each branch in
# the bus: `#N`/`N` (branch `<type>/N-…`, or an `also_closes` entry), a Linear
# id (`eng-N-…`, or an `also_closes` entry), `worker:<branch>#<session>`, a
# branch, or a codename. Shared by `crew where` and `crew resolve-target`. The
# rows are unauthenticated (workers can append to the bus): a caller that acts
# on the result must still gate on the dispatcher-written worktree record.
_resolve_target() {
  [ -f "$log" ] || return 0
  jq -nrR --arg t "$1" --arg c "${2:-}" '
    ($t | ltrimstr("#")) as $s
    | ($s | ascii_downcase) as $l
    | (if $t | startswith("worker:") then $t | ltrimstr("worker:") | sub("#[^#]*$"; "") else $t end) as $b
    | (if $s | test("^[0-9]+$") then "issue"
       elif $s | test("^[A-Za-z]+-[0-9]+$") then "linear" else "name" end) as $k
    | [inputs | fromjson? | objects
        | select(.kind == "dispatch" and ((.branch // null) | type) == "string" and ($c == "" or .crew_id == $c))]
    | group_by(.branch) | map(max_by(.ts))[]
    | select(.branch == $b or (.name // "") == $t
        or ($k == "issue" and ((.branch | test("(^|/)" + $s + "-")) or any(((.also_closes // []) | if type == "array" then .[] else empty end); tostring == $s)))
        or ($k == "linear" and ((.branch | ascii_downcase | test("(^|/)" + $l + "-")) or any(((.also_closes // []) | if type == "array" then .[] else empty end); tostring | ascii_downcase == $l))))
    | [.branch, (.name // ""), (.host // ""), (.crew_id // "")] | @tsv' "$log" 2>/dev/null || true
}

# _identity_recorded <branch> — the identity `dispatch` recorded for this branch
# (latest dispatch event that carries one), or nothing for a legacy branch.
_identity_recorded() {
  [ -f "$log" ] || return 0
  jq -c -s --arg b "$1" '
    map(select(.kind == "dispatch" and .branch == $b and .name != null
      and (.name | strings | test("\\A[a-z][a-z0-9-]*\\z"))
      and (.tmux | strings | test("\\Acolour[0-9]+\\z"))))
    | last // empty | {name, color, tmux}' "$log" 2>/dev/null || true
}

# _identity_bus_occ <branch> <crew> — " name " for every OTHER branch dispatched
# into the crew, dispatched no later than this one, whose latest status is not
# terminal (legacy events count by hash). Earliest dispatch keeps a duplicated name.
_identity_bus_occ() {
  local br name occ=""
  [ -f "$log" ] || return 0
  while IFS=$'\t' read -r br name; do
    [ -n "$br" ] || continue
    if [ -z "$name" ]; then
      name=$(_identity "$br" | jq -r .name)
    fi
    occ="$occ $name "
  done < <(jq -r -s --arg crew "$2" --arg self "$1" '
    def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
    (map(select(.crew_id == $crew and .kind == "status" and ((.from // "") | startswith("worker:"))))
      | group_by(.from | wid_branch)
      | map({key: (.[0].from | wid_branch), value: (max_by(.ts) | .body.state)}) | from_entries) as $st
    | . as $all
    | (($all | map(select(.crew_id == $crew and .kind == "dispatch" and .branch == $self) | .ts) | max) // 1e18) as $selfts
    | $all | map(select(.crew_id == $crew and .kind == "dispatch" and .branch != $self))
    | group_by(.branch) | map(last)
    | .[] | select(.ts <= $selfts) | select(($st[.branch] // "") | IN("done", "failed", "exited") | not)
    | [.branch, (.name // "")] | @tsv' "$log" 2>/dev/null || true)
  printf '%s' "$occ"
}

# _identity_assign <branch> <crew> — the identity a dispatch should stamp: the
# branch's recorded one unless another live worker now holds that name, else its
# hash slot stepped forward to the first codename no live worker holds. Live =
# the crew's other non-terminal dispatched branches plus every tmux window
# already carrying @crew_name. The caller serialises concurrent dispatches (see
# dispatch.sh), so the pick is race-free.
_identity_assign() {
  local rec occ i slot base
  occ=$(_identity_bus_occ "$1" "$2")
  rec=$(_identity_recorded "$1")
  if [ -n "$rec" ]; then
    case "$occ" in *" $(printf '%s' "$rec" | jq -r .name) "*) ;; *)
      printf '%s\n' "$rec"
      return 0
      ;;
    esac
  fi
  occ="$occ $(tmux list-windows -a -F '#{@crew_name}' 2>/dev/null | tr '\n' ' ' || true) "
  base=$(_identity_slot "$1")
  slot=$base
  for ((i = 0; i < ${#_names[@]}; i++)); do
    slot=$(((base + i) % ${#_names[@]}))
    case "$occ" in *" ${_names[$slot]} "*) ;; *) break ;; esac
  done
  # Every codename taken: fall back to the hash slot rather than refuse.
  case "$occ" in *" ${_names[$slot]} "*) slot=$base ;; esac
  _identity_at "$slot"
}

# _is_engine_cmd <pane_current_command> — is this pane running an agent engine?
# Under Nix the literal binary name doesn't always reach tmux. The engines
# engines were measured on a live pane: claude reports `.claude-wrapped`
# (makeWrapper's hidden inner exec) and cursor-agent reports `node` (its
# wrapper ends in `exec -a "$0" "$NODE_BIN" index.js`) — tmux reads the
# executable's own name, so the `exec -a` renaming in both is invisible here.
# codex reports its own literal `codex` (its wrapper re-execs under that name),
# so it needs no special-casing beyond the plain match below.
#
# `node` is broad on purpose. It costs a false positive on, say, a dev server
# sharing the worktree — and every consequence of that is to KEEP something
# (refuse a dispatch, keep an idle window, keep a worktree), while a false
# negative kills a live worker (#71). Callers all pair it with the worktree path
# or a @crew_name window, which is what makes the trade safe.
_is_engine_cmd() {
  local c="${1#.}"
  c="${c%-wrapped}"
  case "$c" in
  claude | codex | cursor-agent | node | pi) return 0 ;;
  esac
  return 1
}

# _owner_pid — the pid `adopt`/`register` record when none is given. A bare
# $PPID is the calling shell, and from an agent's Bash tool that is a throwaway
# subshell that exits at once, so the crew reads as dead (#301). Walk up past
# shells to the first non-shell ancestor (the engine). Bounded; if the chain is
# all shells or ps fails, fall back to $PPID — the pre-#301 behaviour.
_owner_pid() {
  local p="$PPID" c depth=0
  while [ "$depth" -lt 32 ]; do
    depth=$((depth + 1))
    c=$(ps -o comm= -p "$p" 2>/dev/null | tr -d '[:space:]' || true)
    [ -n "$c" ] || break
    c="${c##*/}"
    case "${c#-}" in
    bash | sh | zsh | fish | dash | ksh) ;;
    *)
      printf '%s\n' "$p"
      return
      ;;
    esac
    p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d '[:space:]' || true)
    case "$p" in '' | *[!0-9]* | 0 | 1) break ;; esac
  done
  printf '%s\n' "$PPID"
}

# _pid_alive <pid> — 0 when a process with this pid exists, under any uid.
# `kill -0 0` signals the caller's own process group and `kill -0 -1`
# broadcasts, so only a positive integer is a liveness probe. A failed signal
# is not proof of death: another uid's process returns EPERM, and the signal
# merely being refused proves it exists, so EPERM reads alive. `ps -p` is the
# fallback for any other error (and where `kill` cannot name the error we
# parse). Fail closed: anything unreadable stays alive.
_pid_alive() {
  case "$1" in '' | *[!0-9]* | 0) return 1 ;; esac
  local kmsg
  kmsg="$(LC_ALL=C kill -0 "$1" 2>&1)" && return 0
  case "$kmsg" in
  *"not permitted"* | *"not allowed"*) return 0 ;; # EPERM: the process exists
  esac
  ps -p "$1" -o pid= >/dev/null 2>&1
}

# _file_mtime_s <file> — the file's mtime in epoch seconds, or return 1. GNU
# stat takes `-c %Y`; BSD/macOS takes `-f %m`. GNU first: its `-f` means
# "filesystem" and would print a filesystem block for a `-f %m` invocation.
_file_mtime_s() {
  stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null
}

# _ps_elapsed_s <pid> — the process's elapsed seconds, or return 1 when ps can
# not say. Duplicated from dispatch.sh (the two ship as standalone builds):
# `etimes` is exact where the platform has it; `etime` parses the
# [[dd-]hh:]mm:ss macOS prints. Both are locale- and timezone-independent.
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

# _pid_recycled <pid> <pidfile> — 0 when the process now holding <pid> started
# after <pidfile> was last written (plus 2s of slack for ps' second
# granularity), so it cannot be the dispatcher the file records: that process
# existed when the file was written, and a later start inherited the number.
# Returns 1 when either timestamp is unreadable, so a live process is never
# judged dead; a coarse-mtime filesystem only errs the same, safe way.
_pid_recycled() {
  local pid="$1" file="$2" elapsed file_s
  [ -f "$file" ] || return 1
  elapsed="$(_ps_elapsed_s "$pid")" || return 1
  file_s="$(_file_mtime_s "$file")" || return 1
  case "$elapsed" in '' | *[!0-9]*) return 1 ;; esac
  case "$file_s" in '' | *[!0-9]*) return 1 ;; esac
  [ "$(( $(date +%s) - 10#$elapsed ))" -gt "$(( 10#$file_s + 2 ))" ]
}

# _recorded_pid_live <pid> <pidfile> — 0 when <pid> can still be the dispatcher
# <pidfile> records: live and not a later-recycled pid.
_recorded_pid_live() {
  _pid_alive "$1" || return 1
  _pid_recycled "$1" "$2" && return 1
  return 0
}

# _is_ancestor_pid <pid> — is <pid> one of this process's ancestors? Bounded
# walk; `ps -o ppid= -p` is the one parent-of spelling identical on BSD and GNU.
_is_ancestor_pid() {
  local p=$$ depth=0
  while [ "$depth" -lt 32 ]; do
    depth=$((depth + 1))
    p=$(ps -o ppid= -p "$p" 2>/dev/null | tr -d '[:space:]' || true)
    case "$p" in '' | *[!0-9]* | 0) return 1 ;; esac
    if [ "$p" = "$1" ]; then return 0; fi
  done
  return 1
}

# _pidfile_log <action> <outcome> <crew> <old> <new> — one line per crew pid
# file mutation (or refusal) in $dir/pidfile.log, naming the caller so a crew
# dir that vanishes or changes owner can be traced (#432). Append-only and
# unbounded: it is diagnostic, not state, so it never fails its caller.
_pidfile_log() {
  {
    local by
    by=$(ps -o args= -p "$PPID" || true)
    mkdir -p "$dir"
    _bus_append "$dir/pidfile.log" "$(date -u +%Y-%m-%dT%H:%M:%SZ) $1 $2 crew=$3 old=${4:--} new=${5:--} pid=$$ ppid=$PPID cwd=$PWD by=${by:0:120}"
  } 2>/dev/null || true
}

# _pane_is_engine_at <pane_row> <worktree_path> — <pane_row> is one
# `#{pane_current_command} #{pane_current_path}` row. The path is the strong
# signal and must match exactly, never by prefix — /wt/foo would otherwise claim
# a pane sitting in /wt/foobar. The command is the weak signal (#71).
_pane_is_engine_at() {
  [ "${1#* }" = "$2" ] || return 1
  _is_engine_cmd "${1%% *}"
}

# _occupants <worktree_path> -> [{window,name,pane,command,engine,panes}] — worker windows
# rooted at that path. Keyed on @crew_name (dispatch stamps it on every worker
# window), NOT on the pane's running command: a finished agent drops back to a
# shell prompt, and a command match would then read its window as empty and let
# the next dispatch stack a second worker onto the same tree (#17). Excludes the
# dispatcher's own window — it carries @crew_name too — and the caller's. The
# engine/pane fields below ARE a command check (via _is_engine_cmd) and are
# advisory only — occupancy itself stays window-keyed.
_occupants() {
  local wtp="$1" self_win="" wins panes out wid nm path pw pid cmd epane ecmd
  if [ -n "${TMUX_PANE:-}" ]; then
    self_win=$(tmux display-message -p -t "$TMUX_PANE" '#{window_id}' 2>/dev/null || true)
  fi
  wins=$(tmux list-windows -a -F '#{window_id}	#{@crew_name}	#{pane_current_path}' 2>/dev/null || true)
  panes=$(tmux list-panes -a -F '#{window_id}	#{pane_id}	#{pane_current_command}' 2>/dev/null || true)
  out='[]'
  while IFS=$'\t' read -r wid nm path; do
    [ -n "$wid" ] || continue
    [ "$path" = "$wtp" ] || continue
    [ -n "$nm" ] || continue
    [ "$nm" != dispatcher ] || continue
    [ "$wid" != "$self_win" ] || continue
    epane="" ecmd="" eall='[]'
    while IFS=$'\t' read -r pw pid cmd; do
      [ "$pw" = "$wid" ] || continue
      if _is_engine_cmd "$cmd"; then
        [ -n "$epane" ] || { epane="$pid"; ecmd="$cmd"; }
        eall=$(printf '%s' "$eall" | jq -c --arg p "$pid" --arg c "$cmd" '. + [{pane:$p, command:$c}]')
      fi
    done <<PANES
$panes
PANES
    out=$(printf '%s' "$out" | jq -c --arg w "$wid" --arg n "$nm" --arg p "$epane" --arg c "$ecmd" --argjson all "$eall" \
      '. + [{window:$w, name:$n, pane:(if $p=="" then null else $p end), command:$c, engine:($p!=""), panes:$all}]')
  done <<WINS
$wins
WINS
  printf '%s' "$out"
}

# _frame_classifier — defines the claude/codex pane-frame predicates and their
# regexes. Shared by stall-watch and reap; they read the caller's $engine.
# Bodies stay 2-space indented: tests/adapters.bats byte-compares them against
# dispatch.sh's --role-watch copies.
_frame_classifier() {
  # Multibyte-safe BY CONSTRUCTION, not by ambient locale: under LC_ALL=C a
  # bracket expression consumes one BYTE, so a single-character class would
  # never match `◯` (U+25EF, 3 bytes) and D2 would silently lose its only
  # measured false-positive guard. Hence `+` on the glyph classes and an
  # alternation rather than a bracket set for `❯`.
  re_option='^[[:space:]]*(>|❯|\*)?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]'
  re_meter='^[^[:alnum:]]*[A-Za-z]+…[[:space:]]\(([0-9]+h([[:space:]][0-9]+m)?([[:space:]][0-9]+s)?|[0-9]+m([[:space:]][0-9]+s)?|[0-9]+s)[[:space:]]·[[:space:]]↓[[:space:]][0-9.]+k?[[:space:]]tokens'
  re_subrow='^[[:space:]]*[^[:alnum:][:space:]]+[[:space:]]+[a-z][a-z-]+[[:space:]][[:space:]]+.*[[:space:]](([0-9]+h[[:space:]])?([0-9]+m[[:space:]])?[0-9]+s)[[:space:]]·[[:space:]]↓'

  # The Codex hook-review frame is pasted verbatim in fx_codex_hooks_review.
  # Every visible line is anchored and the option row must end the pane, so a
  # task discussing hooks in its transcript cannot satisfy this signature.
  _is_codex_hook_review_prompt() {
    local tail_n expected
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -4 || true)
    expected=$'Hooks need review\n  1 hook is new or changed.\n  Hooks can run outside the sandbox after you trust them.\n› 1. Review hooks  2. Trust all and continue  3. Continue without trusting'
    [ "$tail_n" = "$expected" ]
  }

  # _is_permission_prompt — Claude's tool-permission dialog (#435), claude only.
  # Its footer `Esc to cancel · Tab to amend` is distinct from every
  # option-select prompt's `Enter to select`/`Enter to confirm`, and the same
  # geometry anchor applies: the footer must be the pane's LAST non-empty line,
  # so the dialog scrolled into the transcript (input box last) cannot match. A
  # numbered option row and `Do you want to proceed?` must sit within the
  # tail-10 non-empty lines ending at the footer (capture-bounded: the real
  # subagent frame has the question at depth 5, the `│` reason rows above it).
  # Deliberately NOT gated on the meter/subrow veto: a subagent raises the
  # dialog with a live subagent row and background-agent lines painted above it
  # (the real capture), so the footer anchor is the false-positive guard.
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

  # Geometry anchor: the footer must be the pane's LAST non-empty line, with a
  # numbered option within the 6 non-empty lines above it. A pane that is not
  # parked on a prompt ends on its input box, never on transcript text (A3), so
  # a prompt frame merely scrolling through — this very repo's bats fixtures —
  # cannot satisfy this. Relaxing it to "the last 10 lines" is exactly how those
  # fixtures become a false-positive source.
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
  # _is_quota_prompt — content discriminator, ALWAYS called alongside _is_prompt
  # (never alone): _is_prompt already proves the pane is on-screen and shaped
  # like an option-select frame; this only decides WHICH option-select frame it
  # is. Searched over the last 12 non-empty lines — wider than _is_prompt's
  # tail-7 (the real rate-limit frame isn't captured anywhere in this repo yet,
  # unlike the pinned fixtures above it, so its exact line count above the
  # footer is unknown and a too-tight window risks silently degrading to
  # generic `prompt:`) but still bounded, not the whole capture: an unbounded
  # search would classify a genuinely different, answerable prompt as `quota:`
  # merely because this literal phrase happens to be visible somewhere higher
  # on the same screen (e.g. a worker with this very protocol doc scrolled
  # into view) — and `quota:` is sticky and escalation-exempt, so that
  # mislabel would leave a real question unanswered indefinitely.
  _is_quota_prompt() {
    printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -12 | grep -qF 'Stop and wait for limit to reset'
  }
  # _is_quota_session_limit — content discriminator for the session-limit
  # refusal frame: a normal working pane, not an option-select prompt, so
  # unlike _is_quota_prompt it is not gated behind _is_prompt.
  # All three anchors must be present: any one alone false-triggers on a worker
  # that merely has this repo's own docs or fixtures on screen, and quota: is
  # sticky and escalation-exempt. The tail bound is the same hazard — the real
  # frame carries the two transcript anchors at non-empty depth 7-8, so a long
  # queued prompt in the input box can push them out of the window and this
  # detector silently misses the frame. `uses your weekly limit` sits in a
  # persistent hint row at depth 3, so it gets the tighter window.
  _is_quota_session_limit() {
    local tail_n tail_n6
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -15 || true)
    tail_n6=$(printf '%s\n' "$tail_n" | tail -6 || true)
    printf '%s\n' "$tail_n" | grep -qF "You've hit your session limit" &&
      printf '%s\n' "$tail_n" | grep -qF '/upgrade to increase your usage limit' &&
      printf '%s\n' "$tail_n6" | grep -qF 'uses your weekly limit'
  }
  # _is_quota_cursor_limit — content discriminator for cursor's monthly
  # usage-limit refusal frame: a normal working pane (no option-select
  # geometry), so like _is_quota_session_limit it is not gated behind
  # _is_prompt. Both anchors must sit in the tail window: either alone
  # false-triggers on a worker that merely has this repo's own docs or a
  # fixture on screen, and quota: is sticky and escalation-exempt. The real
  # frame carries the error line at non-empty depth 5 and `spendLimitHit:` at
  # depth 2.
  _is_quota_cursor_limit() {
    local tail_n
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -12 || true)
    printf '%s\n' "$tail_n" | grep -qF "You've reached your monthly usage limit" &&
      printf '%s\n' "$tail_n" | grep -qF 'spendLimitHit: true'
  }
  # _is_bg_wait — a FINISHED turn parked on a background shell: healthy, since
  # Claude Code re-invokes the session when the shell completes (#353). Both
  # anchors are required within the last 8 non-empty lines: the `· done HH:MM`
  # finished-turn marker and a non-zero `N shell(s)` count (`1 shell still
  # running` on the turn line, `· 1 shell ·` in the mode line), and no live
  # spinner/meter (a new turn started under a stale `done` line).
  # The prompt-suggestion line in the input box is neither anchor, and
  # _is_prompt needs an `Enter to select` footer, so it never reads as input.
  _is_bg_wait() {
    local tail_n
    tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -8 || true)
    printf '%s\n' "$tail_n" | grep -qE '·[[:space:]]done[[:space:]]+[0-9]{1,2}:[0-9]{2}' &&
      printf '%s\n' "$tail_n" | grep -qE '(^|[^0-9])[1-9][0-9]*[[:space:]]shells?([[:space:]]still running|[[:space:]]·|$)' &&
      ! printf '%s\n' "$tail_n" | grep -qE '^[^[:alnum:]]*[A-Za-z]+…' &&
      [ -z "$(_meter_line "$1")" ]
  }
  _meter_line() { printf '%s\n' "$1" | grep -E "$re_meter" | tail -1 || true; }
  _has_subrow() { printf '%s\n' "$1" | grep -qE "$re_subrow"; }

  # SGR/CSI stripping regex, built from a raw ESC byte via ANSI-C quoting —
  # never `\x1b` as escape text in a regex/awk source literal (a GNU
  # extension; this repo's shell-reviewer already flags `grep -P` the same
  # way for the same portability reason). Reused by _box_rows.
  csi_re=$'\033\\[[0-9;]*m'
  # The dim-SGR marker claude wraps a `❯`-row prompt suggestion in (ghost
  # text), vs. a real unsent draft the user typed: nbsp separator + ESC[2m.
  # Built from a raw ESC byte via ANSI-C quoting, same reasoning as csi_re.
  _rw_esc=$'\033'
  _rw_ghost_marker=$'❯\xc2\xa0'"${_rw_esc}[2m"

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
  # The editor's scroll indicators (`↑ N more` / `↓ N more`, real capture, pi
  # 0.99.1, drawn once a paste is taller than the editor) are rule-borne text but
  # not a live-turn label. pi-vim appends its mode label (` INSERT`, ` NORMAL`,
  # ` EX …`, ` VISUAL`, ` V-LINE`, plus a pending-command tail such as
  # ` NORMAL 3dw_`; real capture, pi 1.0.0 + pi-vim 0.14.2) to the LOWER rule, so
  # a vim-mode pane is idle despite the rule text. Strip that trailing label
  # before deciding. Any other rule text (e.g. `── ⠼ Working ──`) still vetoes.
  _pi_live_turn() {
    printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -30 | awk '
      {
        if (index($0, "─") != 1) next
        line = $0
        gsub(/─/, "", line)
        gsub(/↑ [0-9]+ more/, "", line)
        gsub(/↓ [0-9]+ more/, "", line)
        sub(/[[:space:]]+(INSERT|NORMAL|EX|VISUAL|V-LINE)([[:space:]].*)?[[:space:]]*$/, "", line)
        if (line !~ /^[[:space:]]*$/) { found = 1; exit }
      }
      END { exit (found ? 0 : 1) }'
  }

  # _pi_working_row <text> — a pi-vim working-indicator row: a braille glyph
  # carrying the `Working` status text, drawn as its own row above the editor box
  # instead of on the top rule (real capture, pi 1.0.0 + pi-vim 0.14.2:
  # ` ⠸ Working`). The braille range U+2800–U+28FF is matched by its lead byte
  # and two continuation ranges, never a quantifier directly on the glyph, so
  # this stays correct under LC_ALL=C.
  _pi_working_row() {
    printf '%s\n' "$1" | LC_ALL=C grep -qE $'^[[:space:]]*\xe2[\xa0-\xa3][\x80-\xbf].*Working'
  }

  # _pi_working_label <text> — POSITIVE live-turn evidence: a rule carrying the
  # `Working` status text or a braille spinner glyph (real capture, pi 0.87.1),
  # or a pi-vim working row adjacent to the editor box (real capture, pi 1.0.0).
  # `_pi_live_turn` is the wider fail-closed veto ("any rule text means not
  # idle"), which is right before a paste but is not proof that a turn started.
  # The row check is positional (rows `_box_rows` prints above the upper rule) so
  # a stale transcript row cannot dequeue a still-held assignment; when there is
  # no box, only the rule-borne evidence counts.
  _pi_working_label() {
    local out above
    printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -30 |
      LC_ALL=C grep -qE $'^\xe2\x94\x80.*(Working|\xe2[\xa0-\xa3][\x80-\xbf])' && return 0
    out=$(_box_rows "$1" '.*') || return 1
    above=$(printf '%s\n' "$out" | tail -n +2)
    _pi_working_row "$above"
  }

  # _pi_idle_box <text> — positive idle shape of a pi pane, from a real capture
  # (pi 0.87.1): an editor bounded by two `─` rules, blank or holding text, with
  # the cwd and stats rows after the lower rule. A bare shell prompt or a boot
  # frame has no such box. A vim-mode rule suffix is not live-turn evidence
  # (`_pi_live_turn`); a pi-vim working row among the box's above-rule rows is a
  # live turn and vetoes (real capture, pi 1.0.0 + pi-vim 0.14.2).
  _pi_idle_box() {
    local out row above
    _pi_live_turn "$1" && return 1
    out=$(_box_rows "$1" '.*') || return 1
    row=$(printf '%s\n' "$out" | head -1)
    printf '%s\n' "$row" | grep -qE "$re_option" && return 1
    above=$(printf '%s\n' "$out" | tail -n +2)
    _pi_working_row "$above" && return 1
    return 0
  }
}

# _pane_idle_reason <plain> <colored> [allow_bg] — 0 (prints nothing) iff a claude frame
# is provably idle; else prints a short keep reason and returns 1. Needs
# _frame_classifier already called. On top of --role-watch's
# _claude_idle_box, idle needs a finished turn's `· done HH:MM` marker just
# above the box, so a freshly booted pane or an unrecognised frame reads busy.
# The predicates read $engine, unset outside stall-watch, hence the local.
# allow_bg=1 skips the background shell/monitor veto, for callers that want an
# idle input box regardless of detached work.
_pane_idle_reason() {
  local engine=claude tail_n above
  if _is_permission_prompt "$1" || _is_prompt "$1" || _is_quota_session_limit "$1"; then
    printf '%s' "prompt on screen"
    return 1
  fi
  tail_n=$(printf '%s\n' "$1" | grep -v '^[[:space:]]*$' | tail -30 || true)
  if [ "${3:-0}" != 1 ] && grep -qE '(^|[^0-9])[1-9][0-9]*[[:space:]](shells?|monitors?)([[:space:]]still running|[[:space:]]·|$)' <<<"$tail_n"; then
    printf '%s' "background shell or monitor still running"
    return 1
  fi
  if ! _claude_idle_box "$1" "$2" 0; then
    printf '%s' "live turn, unsent input, or no idle input box"
    return 1
  fi
  above=$(_box_rows "$1" '^[[:space:]]*❯' | tail -n +2)
  if ! grep -qE '·[[:space:]]done[[:space:]]+[0-9]{1,2}:[0-9]{2}' <<<"$above"; then
    printf '%s' "no finished-turn marker above the input box"
    return 1
  fi
}

# release_grace — how long a finished (`done`/`failed`) worker's window stays
# open before it is released. The session is terminal (`reply` refuses it,
# nothing types into it), so the window only buys a human a glance at the final
# summary and a late dispatcher directive time to re-open the session.
release_grace=300

# _human_present <window-ids> <worktree> <status-ts-ms> <grace-s> — prints why
# and returns 0 when a person is evidently using a finished worker's window:
# it is the active window of an attached tmux session, it saw output within the
# grace, or its claude transcript holds a user turn newer than the status.
_human_present() {
  local wins="$1" wtp="$2" ts_ms="$3" grace="$4" now row wid active attached act dir latest f
  now=$(date +%s)
  while IFS=$'\t' read -r wid active attached act; do
    [ -n "$wid" ] || continue
    case " $wins " in *" $wid "*) ;; *) continue ;; esac
    if [ "$active" = 1 ] && [ "${attached:-0}" -gt 0 ] 2>/dev/null; then
      printf '%s' "$wid is visible in an attached tmux client"
      return 0
    fi
    if [ -n "$act" ] && [ $((now - act)) -lt "$grace" ] 2>/dev/null; then
      printf '%s' "$wid had activity within the grace"
      return 0
    fi
  done < <(tmux list-windows -a -F $'#{window_id}\t#{window_active}\t#{session_attached}\t#{window_activity}' 2>/dev/null || true)
  dir="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects/$(printf '%s' "$wtp" | sed 's/[^A-Za-z0-9]/-/g')"
  latest=""
  for f in "$dir"/*.jsonl; do
    [ -e "$f" ] || continue
    if [ -z "$latest" ] || [ "$f" -nt "$latest" ]; then latest="$f"; fi
  done
  if [ -n "$latest" ]; then
    # Only text a person typed: not tool results, isMeta injections, or the
    # harness's own task-notification/bash-output turns.
    row=$(jq -Rr --argjson ts "$ts_ms" '
      fromjson? | select(.type == "user" and .timestamp != null and ((.isMeta // false) | not))
      | ((.message.content | if type == "string" then . else ([.[]? | select(.type == "text") | .text] | join("\n")) end)) as $t
      | select($t != "" and ($t | test("^\\s*<(task-notification|bash-stdout|bash-stderr|local-command-stdout)>") | not))
      | select((.timestamp | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) * 1000 > $ts)
      | "x"' "$latest" 2>/dev/null | head -n 1 || true)
    if [ -n "$row" ]; then
      printf '%s' "the engine transcript has a user turn newer than the status"
      return 0
    fi
  fi
  return 1
}

# _pane_capture <pane> [colored] — the pane's text, or its ANSI-colored capture
# when the second argument is non-empty. CREW_STALL_SAMPLE_CMD /
# CREW_STALL_COLOR_CMD override it so frame checks are testable without tmux.
_pane_capture() {
  if [ -n "${2:-}" ] && [ -n "${CREW_STALL_COLOR_CMD:-}" ]; then
    eval "$CREW_STALL_COLOR_CMD" 2>/dev/null
  elif [ -n "${CREW_STALL_SAMPLE_CMD:-}" ]; then
    eval "$CREW_STALL_SAMPLE_CMD" 2>/dev/null
  elif [ -n "${2:-}" ]; then
    tmux capture-pane -e -p -t "$1" 2>/dev/null
  else
    tmux capture-pane -p -t "$1" 2>/dev/null
  fi
}

# _frames_busy <occupants-json> — prints why and returns 0 when any engine pane
# of a finished worker's windows is not provably idle; returns 1 when every one
# is. Needs _frame_classifier already called. Every pane is sampled twice, one
# gap apart: claude panes must pass _pane_idle_reason on both with identical
# text; other engines have no idle signature, so they need an unchanged frame
# and no prompt. An unreadable pane keeps, and so does unreadable input. The
# set of engine panes is what _is_engine_cmd recognises.
_frames_busy() {
  local rows pane cmd stripped engine i why cap
  local -a panes=() cmds=() plain=() colored=()
  rows=$(printf '%s' "$1" | jq -r '.[].panes[]? | [.pane, .command] | @tsv') || {
    printf '%s' "occupants are unreadable"
    return 0
  }
  while IFS=$'\t' read -r pane cmd; do
    [ -n "$pane" ] || continue
    panes+=("$pane")
    cmds+=("$cmd")
    cap=$(_pane_capture "$pane" || true)
    plain+=("$cap")
    colored+=("$(_pane_capture "$pane" colored || true)")
    if [ -z "$cap" ]; then
      printf '%s' "$pane is unreadable"
      return 0
    fi
  done <<<"$rows"
  [ "${#panes[@]}" -gt 0 ] || return 1
  sleep "${CREW_RELEASE_GAP:-1}"
  for i in "${!panes[@]}"; do
    pane="${panes[i]}"
    stripped="${cmds[i]#.}"
    stripped="${stripped%-wrapped}"
    engine="$stripped"
    if [ "$(_pane_capture "$pane" || true)" != "${plain[i]}" ]; then
      printf '%s' "$pane is still changing"
      return 0
    fi
    if [ "$stripped" = claude ]; then
      if ! why=$(_pane_idle_reason "${plain[i]}" "${colored[i]}"); then
        printf '%s' "$pane is not idle ($why)"
        return 0
      fi
    elif _is_prompt "${plain[i]}" || _is_permission_prompt "${plain[i]}"; then
      printf '%s' "$pane shows a prompt"
      return 0
    fi
  done
  return 1
}

# _release_windows <branch> <session> <state> <status-ts-ms> <grace-s> <dry> —
# kill the WINDOWs of a finished worker session, never its worktree: a worker
# legitimately sits in `done` for as long as its PR takes to merge (#17).
# Shared by reap's idle-release pass and stall-watch's finished-worker release;
# the caller defines say/note. Returns 3 (nothing killed) when a person is
# present or an engine pane is not provably idle, so the caller re-checks on its
# next cycle.
_release_windows() {
  local rbranch="$1" rsession="$2" rstate="$3" rts="$4" rgrace="$5" rdry="$6" rwt rocc w line why
  rwt=$(git --git-dir="$common" worktree list --porcelain |
    awk -v b="refs/heads/$rbranch" '/^worktree /{p=$2} $0=="branch "b{print p}')
  [ -n "$rwt" ] && [ -d "$rwt" ] || return 0
  rocc=$(_occupants "$rwt")
  [ "$rocc" != '[]' ] || return 0
  # `exited` is the SessionEnd backstop, not something the worker asserts, and
  # it fires for subagents and stray panes too (#69) — so an `exited` row with
  # a live engine in the tree is a false read, and killing the window would
  # kill a working agent. `done`/`failed` are the worker's own word and stay
  # releasable, engine or not: an agent legitimately idles in its pane after
  # posting `done`, which is exactly what idle-release exists to clean up (#17).
  if [ "$rstate" = exited ] &&
    [ "$(printf '%s' "$rocc" | jq -r 'map(select(.engine)) | length')" -gt 0 ]; then
    note "keeping $rbranch — exited but an engine is still running there"
    return 0
  fi
  if why=$(_human_present "$(printf '%s' "$rocc" | jq -r '[.[].window] | join(" ")')" "$rwt" "$rts" "$rgrace"); then
    note "keeping $rbranch — human present: $why"
    return 3
  fi
  if why=$(_frames_busy "$rocc"); then
    note "keeping $rbranch — pane busy: $why"
    return 3
  fi
  for w in $(printf '%s' "$rocc" | jq -r '.[].window'); do
    if [ -n "$rdry" ]; then
      say "would release $w at $rwt ($rbranch $rstate)"
      continue
    fi
    tmux kill-window -t "$w" 2>/dev/null || true
    say "released $w at $rwt ($rbranch $rstate)"
  done
  [ -n "$rdry" ] || {
    line=$(jq -nc --arg branch "$rbranch" --arg session "$rsession" \
      --arg state "$rstate" --argjson occ "$rocc" \
      '{ts:(now*1000|floor), kind:"release", branch:$branch, session:$session,
          state:$state, windows:($occ|map(.window))}') || return 1
    _bus_append "$log" "$line"
  }
}

# _is_session_id <id> — 0 when the suffix after the LAST '#' has the sid shape
# s<epoch>-<pid>. A '#' inside a branch name (legal in git) does not match, so
# `worker:feat/a#b` is branch-only while `worker:feat/a#b#s1-1` is sessioned.
_is_session_id() {
  [[ "${1##*#}" =~ ^s[0-9]+-[0-9]+$ ]]
}

# CREW_CLOCK=<file> is a test-only virtual clock for the waits in `await`, the
# hold paths and `stall-watch`: _clock_now reads epoch seconds from it and
# _clock_sleep advances it instead of waiting. It starts at the real time and
# only moves forward, so bus rows (real ms) posted during a run still sort at or
# before `now`, as they do in production. Unset, these are exactly `date +%s`
# and `sleep`.
_clock_now() {
  [ -n "${CREW_CLOCK:-}" ] || { date +%s; return; }
  [ -s "$CREW_CLOCK" ] || date +%s >"$CREW_CLOCK"
  cat "$CREW_CLOCK"
}
_clock_sleep() {
  [ -n "${CREW_CLOCK:-}" ] || { sleep "$1"; return; }
  local s="${1%%.*}"
  [ "$s" = "$1" ] || s=$((${s:-0} + 1))
  printf '%s\n' "$(($(_clock_now) + s))" >"$CREW_CLOCK.$$"
  mv -f "$CREW_CLOCK.$$" "$CREW_CLOCK"
}
_clock_now_ms() {
  [ -n "${CREW_CLOCK:-}" ] || { jq -nc 'now*1000|floor'; return; }
  printf '%s\n' "$(($(_clock_now) * 1000))"
}
# _clock_now_f — fractional epoch seconds, jq's `now` when the clock is unset.
_clock_now_f() {
  [ -n "${CREW_CLOCK:-}" ] || { jq -nc 'now'; return; }
  _clock_now
}

# Delivered marks (#290): {sender: ts of the last msg from that sender this
# session has been handed}, one file per crew+session under $dir/await. `await`
# and `inbox` both record into it, so a reply taken through the straggler fold is
# not handed back by the next `await`. Not a bus row: it never reaches `roster`,
# `watch` or another session's reads.
_await_state() { # <crew> <agent> -> path
  local key="$1-$2"
  printf '%s/await/%s.%s' "$dir" "$(printf '%s' "$key" | tr -c 'A-Za-z0-9._-' '_')" "$(printf '%s' "$key" | cksum | cut -d' ' -f1)"
}

# _await_marks <crew> <agent> -> one JSON object; a torn or unreadable file reads
# as {} (redeliver rather than go blind).
_await_marks() {
  local st
  st=$(_await_state "$1" "$2")
  [ -f "$st" ] && jq -cs 'map(objects) | add // {}' "$st" 2>/dev/null && return 0
  printf '{}'
}

# _await_record <crew> <agent> <msg-lines> — raise each sender's mark to the
# newest of the given msgs. Best effort: a failed write only means redelivery.
_await_record() {
  local st tmp
  st=$(_await_state "$1" "$2")
  mkdir -p "$dir/await" 2>/dev/null || return 0
  tmp=$(mktemp "$dir/await/.st.XXXXXX") || return 0
  if printf '%s\n' "$3" | jq -sc --argjson old "$(_await_marks "$1" "$2")" \
    'reduce .[] as $m ($old; .[$m.from] = ([(.[$m.from] // 0), $m.ts] | max))' >"$tmp" 2>/dev/null; then
    mv "$tmp" "$st" 2>/dev/null
  fi
  rm -f "$tmp"
}

# _unread_scan <crew> <branch> <me> <from_id> <t0> <mode> — mode `oldest` prints
# "<ts-ms> role|dispatcher" for the oldest role:<branch>:* or dispatcher:<crew>
# msg to this lead that is past the delivered mark; a role msg also drops out
# once answered by a later msg from the lead to that role (a dispatcher
# directive clears only on delivery); empty when none. Mode `dispatcher`
# considers only the undelivered dispatcher:<crew> msgs and prints
# "<oldest-ts> <newest-ts>". A sessioned watchdog matches its session id only,
# which scopes it to this run; a branch-keyed one has no marks to read and
# returns nothing. A msg scrolled out of the 2000-line tail reads as gone.
_unread_scan() {
  local crew="$1" branch="$2" me="$3" from_id="$4" t0="$5" mode="$6"
  # A branch-keyed watchdog cannot name the lead's session, so it cannot read
  # that session's delivered marks; stay silent rather than misreport.
  [ "$from_id" != "$me" ] || return 0
  [ -f "$log" ] || return 0
  tail -n 2000 "$log" 2>/dev/null | jq -Rnr --arg c "$crew" --arg b "role:$branch:" \
    --arg d "dispatcher:$crew" --arg me "$me" --arg f "$from_id" --argjson t0 "$t0" \
    --arg mode "$mode" --argjson marks "$(_await_marks "$crew" "$from_id")" '
      def lead($x): if $f == $me then ($x == $me or ($x | startswith($me + "#")))
                    else $x == $f end;
      [inputs | fromjson? | select(.crew_id == $c and .kind == "msg" and (.ts >= $t0 or $f != $me))] as $m
      | [$m[] | select(lead(.from))] as $sent
      | [$m[] | select(((.from | strings | startswith($b)) or .from == $d) and lead(.to) and .ts > ($marks[.from] // 0))
         | . as $r
         | select($r.from == $d or (any($sent[]; .to == $r.from and .ts > $r.ts) | not))] as $u
      | if $mode == "dispatcher" then
          [$u[] | select(.from == $d)] | if length == 0 then empty else "\(min_by(.ts).ts) \(max_by(.ts).ts)" end
        else
          ($u | min_by(.ts) // empty) | "\(.ts) \(if .from == $d then "dispatcher" else "role" end)"
        end' 2>/dev/null || true
}

# _sessions <branch> <crew_or_empty> -> [{session,worker_id,state,ts,age_s,terminal}]
# oldest -> newest. Every fold is per SESSION: aggregating across a branch is how
# three workers came to read as one flip-flopping identity (#17). A session with a
# dispatch event but no status yet is still listed (state null) — dispatch needs to
# see a booting worker. No crew filter by default, same reason `reap` has none: the
# sessions worth inspecting are the ones from earlier dispatcher crews.
# A session-less status row (watchdog, bare heartbeat, legacy) belongs to the
# sessioned session that had started by its ts: left as its own null entry it
# could become `last`, and `reply` would address a bare id no live inbox reads
# (#173). A resume row starts a session exactly as a dispatch row does, or a
# row landing before the resumed worker's first status would pick the dead
# predecessor. The row stays a null entry when no session precedes it, or when
# that session's own last word was terminal: a finished session reads no inbox,
# so reviving it would hand `reply` a dead address instead of a refusal.
_sessions() {
  local branch="$1" crewf="$2"
  [ -f "$log" ] || {
    printf '[]'
    return 0
  }
  jq -s -c --arg b "$branch" --arg crew "$crewf" '
      def split_wid: ltrimstr("worker:") as $r
        | ($r | capture("#(?<s>s[0-9]+-[0-9]+)$").s // null) as $s
        | {branch: (if $s == null then $r else ($r | rtrimstr("#" + $s)) end), session: $s};
      def is_terminal: . as $x | (["done","failed","exited"] | index($x // "")) != null;
      map(select($crew=="" or .crew_id==$crew))
      | ( map(select((.kind=="dispatch" or .kind=="resume") and .branch==$b))
          | map({session:(.session // null), ts:.ts}) ) as $disp
      | ( map(select(.kind=="status" and ((.from // "") | startswith("worker:")))
              | ((.from) | split_wid) as $w
              | select($w.branch == $b)
              | {session:$w.session, state:.body.state, ts:.ts}) ) as $raw
      | ( ($disp + $raw) | map(select(.session != null)) | group_by(.session)
          | map({session:.[0].session, start:(map(.ts) | min)}) ) as $starts
      | ( $raw | map(select(.session != null)) ) as $sessioned
      | ( $raw | map(if .session != null then . else
            .ts as $t
            | ($starts | map(select(.start <= $t)) | max_by(.start) | .session) as $s
            | ($sessioned | map(select(.session == $s and .ts <= $t)) | max_by(.ts) | .state) as $prev
            | if ($prev | is_terminal) then . else .session = $s end
          end) ) as $st
      | ( ($disp + $st) | map(.session) | unique ) as $ids
      | [ $ids[] as $s
          | ($st | map(select(.session == $s)) | sort_by(.ts) | last) as $latest
          | ($disp | map(select(.session == $s)) | sort_by(.ts) | last) as $d
          | { session: $s,
              worker_id: ("worker:" + $b + (if $s == null then "" else "#" + $s end)),
              state: ($latest.state // null),
              ts: ($latest.ts // $d.ts),
              terminal: ((["done","failed","exited"] | index($latest.state // "")) != null) } ]
      | sort_by(.ts)
      | map(. + {age_s: (((now*1000) - .ts) / 1000 | floor)})' "$log"
}

# _nudge_busy_reason <plain> <colored> <why> — a claude lead's
# _pane_idle_reason refusal, narrowed to what is actually on screen.
_nudge_busy_reason() {
  case "$3" in
  "prompt on screen")
    if _is_permission_prompt "$1"; then
      printf '%s' "permission dialog on screen"
    elif _is_quota_prompt "$1" || _is_quota_session_limit "$1"; then
      printf '%s' "quota frame on screen"
    else
      printf '%s' "dialog on screen (option-select or workspace trust)"
    fi
    ;;
  "live turn, unsent input, or no idle input box")
    # Only a non-empty, non-ghost row passes the own-text form after the
    # plain form failed.
    if _claude_idle_box "$1" "$2" 1; then
      printf '%s' "unsent input in the input box"
    else
      printf '%s' "live turn or no idle input box"
    fi
    ;;
  *) printf '%s' "$3" ;;
  esac
}

# The line a nudge types, literally: the lead's own shell expands the variable
# its engine environment exports, so nothing bus- or branch-derived is typed.
_nudge_line="crew inbox \"\$CREW_WORKER_ID\""

# _nudge_box_text <text> — every row inside the lead's input box (same bounds
# as _box_rows) concatenated with claude's `❯` and all whitespace dropped, so a
# box the engine wrapped (at a space or mid-word) still compares equal to the
# whitespace-free line; empty when the box is empty or absent.
_nudge_box_text() {
  local rx='.*'
  [ "$engine" != claude ] || rx='^[[:space:]]*❯'
  printf '%s\n' "$1" |
    rx="$rx" csi="$csi_re" awk '
      {
        stripped = $0
        gsub(ENVIRON["csi"], "", stripped)
        if (stripped ~ /^[[:space:]]*$/) next
        plain[++n] = stripped
      }
      END {
        off = (n > 30) ? n - 30 : 0
        b = 0; a = 0
        for (i = n; i > off; i--) if (index(plain[i], "─") == 1) { if (!b) b = i; else { a = i; break } }
        if (!a || b - a < 1 || b - a - 1 > 12 || n - b < 1 || n - b > 5) exit
        if (b - a > 1 && plain[a + 1] !~ ENVIRON["rx"]) exit
        s = ""
        for (i = a + 1; i < b; i++) s = s plain[i]
        sub(/^[[:space:]]*❯/, "", s)
        gsub(/[[:space:]]|\302\240/, "", s)
        print s
      }' || true
}

# _nudge_frame_gate <pane> — reads $engine. Returns 0 when the pane shows an
# idle, empty input box; else prints the refusal reason and returns 2.
_nudge_frame_gate() {
  local pane="$1" plain colored why
  plain=$(_pane_capture "$pane" || true)
  colored=$(_pane_capture "$pane" colored || true)
  [ -n "$plain" ] || {
    printf '%s\n' "pane unreadable"
    return 2
  }
  if [ "$engine" = claude ]; then
    if ! why=$(_pane_idle_reason "$plain" "$colored" 1); then
      _nudge_busy_reason "$plain" "$colored" "$why"
      printf '\n'
      return 2
    fi
  else
    why=""
    if _is_prompt "$plain"; then
      why="prompt on screen"
    elif ! _pi_idle_box "$plain"; then
      why="live turn or no idle input box"
    elif [ -n "$(_nudge_box_text "$plain")" ]; then
      why="unsent input in the input box"
    fi
    [ -z "$why" ] || {
      printf '%s\n' "$why"
      return 2
    }
  fi
}

# _nudge_pane <pane> <engine> <session-id> <crew> <actor> <msg-ts> — type the
# constant $_nudge_line into a worker lead's provably idle, empty input box and
# submit it, so a lead whose wake expired reads its unread directive. Needs
# _frame_classifier already called. Prints one line; returns 0 accepted, 2
# refused before typing (nothing typed, no bus row; an anchor-gate refusal
# starts with `anchor:`), 3 typed but not accepted (held, unknown,
# unconfirmed). Every typed attempt appends one `kind:"nudge"` row. Never
# retries the text or the Enter: whatever the pane shows instead may be a human's.
_nudge_pane() {
  local engine="$2"
  local pane="$1" sid="$3" crew="$4" actor="$5" msg_ts="$6" key branch wins panes prow wid role cmd wrow n last
  local p2 c2 p3 c3 tail_n out row result detail entry
  key="${_nudge_line//[[:space:]]/}"
  if ! _is_session_id "$sid"; then
    printf '%s\n' "'$sid' is not a sessioned worker id"
    return 2
  fi
  case "$msg_ts" in '' | *[!0-9]*)
    printf '%s\n' "msg ts '$msg_ts' is not a ms timestamp"
    return 2
    ;;
  esac
  case "$engine" in
  claude | pi) ;;
  *)
    printf '%s\n' "no verified idle lead frame for ${engine:-an unknown engine}"
    return 2
    ;;
  esac

  # The frame gate runs before the anchor gate: the anchor's _sessions reads
  # the whole bus log, and a busy lead is the common refusal. It runs again
  # right before typing, since that read leaves time for a human to start typing.
  _nudge_frame_gate "$pane" || return 2

  # Anchor: the pane is the lead of this crew's window for the session's
  # branch, still runs an engine, and the session is that branch's live newest.
  branch="${sid%#*}"
  branch="${branch#worker:}"
  wins=$(tmux list-windows -a -F $'#{window_id}\t#{@crew_branch}\t#{@crew_id}' 2>/dev/null) || {
    printf '%s\n' "anchor: cannot read tmux windows"
    return 2
  }
  panes=$(tmux list-panes -a -F $'#{window_id}\t#{pane_id}\t#{@crew_role}\t#{pane_current_command}' 2>/dev/null) || {
    printf '%s\n' "anchor: cannot read tmux panes"
    return 2
  }
  prow=$(printf '%s\n' "$panes" | awk -F'\t' -v p="$pane" '$2 == p { print; exit }')
  [ -n "$prow" ] || {
    printf '%s\n' "anchor: no pane $pane"
    return 2
  }
  wid=$(printf '%s' "$prow" | cut -f1)
  role=$(printf '%s' "$prow" | cut -f3)
  cmd=$(printf '%s' "$prow" | cut -f4)
  wrow=$(printf '%s\n' "$wins" | awk -F'\t' -v w="$wid" '$1 == w { print; exit }')
  if [ -z "$crew" ] || [ "$(printf '%s' "$wrow" | cut -f3)" != "$crew" ]; then
    printf '%s\n' "anchor: window $wid of $pane is not anchored to crew $crew"
    return 2
  fi
  if [ "$(printf '%s' "$wrow" | cut -f2)" != "$branch" ]; then
    printf '%s\n' "anchor: window $wid of $pane is not $branch's window"
    return 2
  fi
  if [ "$role" != lead ]; then
    n=$(printf '%s\n' "$panes" | awk -F'\t' -v w="$wid" '$1 == w' | grep -c . || true)
    if [ -n "$role" ] || [ "$n" -ne 1 ]; then
      printf '%s\n' "anchor: $pane is not the lead pane of $wid"
      return 2
    fi
  fi
  _is_engine_cmd "$cmd" || {
    printf '%s\n' "anchor: $pane is not running an engine (${cmd:-no command})"
    return 2
  }
  last=$(_sessions "$branch" "$crew" | jq -c 'last // empty' 2>/dev/null || true)
  if [ -z "$last" ] || ! printf '%s' "$last" | jq -e --arg s "$sid" '.worker_id == $s and (.terminal | not)' >/dev/null 2>&1; then
    printf '%s\n' "anchor: $sid is not the live newest session on $branch"
    return 2
  fi

  _nudge_frame_gate "$pane" || return 2
  tmux send-keys -t "$pane" -l "$_nudge_line" 2>/dev/null || {
    printf '%s\n' "send-keys failed"
    return 2
  }

  # Enter only once the line provably sits alone in a still-idle box: an Enter
  # into anything else could answer a dialog or submit a human's text. The
  # settle lets the engine redraw the typed text before it is read back.
  _clock_sleep "${CREW_NUDGE_SETTLE:-1}"
  p2=$(_pane_capture "$pane" || true)
  c2=$(_pane_capture "$pane" colored || true)
  entry=0
  if ! _is_prompt "$p2" && [ "$(_nudge_box_text "$p2")" = "$key" ]; then
    if [ "$engine" = claude ]; then
      ! _is_permission_prompt "$p2" && _claude_idle_box "$p2" "$c2" 1 && entry=1
    else
      _pi_idle_box "$p2" && entry=1
    fi
  fi
  if [ "$entry" = 0 ]; then
    result=unconfirmed
    detail="typed but not confirmed in the input box; no Enter sent — the text may be left as an unsent draft a human must clear (crew where), never re-typed"
  elif ! tmux send-keys -t "$pane" Enter 2>/dev/null; then
    result=unknown
    detail="typed and confirmed, but the Enter failed; the line may sit in the input box (crew where), never re-typed"
  else
    _clock_sleep "${CREW_NUDGE_GAP:-2}"
    p3=$(_pane_capture "$pane" || true)
    c3=$(_pane_capture "$pane" colored || true)
    result=unknown
    if [ "$engine" = claude ]; then
      tail_n=$(printf '%s\n' "$p3" | grep -v '^[[:space:]]*$' | tail -30 || true)
      if [ -n "$(_meter_line "$tail_n")" ] || _has_subrow "$tail_n" ||
        printf '%s\n' "$tail_n" | grep -qF 'esc to interrupt' ||
        { out=$(_box_rows "$p3" '^[[:space:]]*❯') &&
          printf '%s\n' "$out" | tail -n +2 | grep -qE '^[^[:alnum:]]*[A-Za-z]+…'; } ||
        _claude_idle_box "$p3" "$c3" 0; then
        result=accepted
      elif _claude_idle_box "$p3" "$c3" 1 && [ "$(_nudge_box_text "$p3")" = "$key" ]; then
        result=held
      fi
    elif _pi_working_label "$p3"; then
      result=accepted
    elif _pi_idle_box "$p3"; then
      row=$(_nudge_box_text "$p3")
      if [ -z "$row" ]; then
        result=accepted
      elif [ "$row" = "$key" ]; then
        result=held
      fi
    fi
    case "$result" in
    accepted) detail="the lead took the line (live turn or emptied input box)" ;;
    held) detail="the line still sits in the input box after one Enter; not re-sent — a human must submit or clear it (crew where)" ;;
    *) detail="the pane after Enter is neither a live turn nor the held line; verify it (crew where), never re-typed" ;;
    esac
  fi

  _bus_append "$log" "$(jq -nc --arg crew "$crew" --arg from "$actor" --arg to "$sid" \
    --arg branch "$branch" --arg pane "$pane" --arg engine "$engine" --arg result "$result" \
    --arg detail "$detail" --argjson msg_ts "$msg_ts" \
    '{ts:(now*1000|floor), crew_id:$crew, kind:"nudge", from:$from, to:$to, branch:$branch,
      pane:$pane, engine:$engine, result:$result, detail:$detail, msg_ts:$msg_ts}')"
  printf 'nudge %s: %s %s — %s\n' "$result" "$pane" "$sid" "$detail"
  [ "$result" = accepted ] || return 3
}

# _nudge_wait_row <state> <result> <rc> <detail> — the `nudge` arm's --wait
# bookkeeping row, from the arm's nudge_* variables.
_nudge_wait_row() {
  _bus_append "$log" "$(jq -nc --arg crew "$nudge_crew" --arg to "$nudge_sid" --arg branch "$nudge_branch" \
    --arg pane "$nudge_pane" --arg state "$1" --arg result "$2" --arg rc "$3" --arg detail "$4" \
    --argjson msg_ts "$nudge_ts" --argjson ts "$(_clock_now_ms)" \
    '{ts:$ts, crew_id:$crew, kind:"nudge_wait", from:("dispatcher:" + $crew), to:$to, branch:$branch,
      pane:$pane, msg_ts:$msg_ts, state:$state, result:$result, detail:$detail}
      + (if $rc == "" then {} else {rc:($rc | tonumber)} end)')"
}

# _lock_acquire <lockdir> <owner_pid> — atomic mkdir gate with dead-PID reclaim.
# mkdir is atomic on POSIX, so it is the ONLY gate: exactly one caller wins.
# Returns 0 (acquired; owner_pid written inside for liveness) or 1 (held by a
# DIFFERENT live PID, or lost the reclaim race). If the held owner equals the
# requested owner, returns 0 idempotently — re-running /dispatcher in the same
# session (same session-stable PID) is a no-op success, not a refusal. rm -rf
# here is safe — the lock dir is transient coordination state, never user data.
_lock_acquire() {
  local ld="$1" owner="$2" held
  if mkdir "$ld" 2>/dev/null; then
    printf '%s\n' "$owner" >"$ld/pid"
    return 0
  fi
  held=$(cat "$ld/pid" 2>/dev/null || true)
  if [ "$held" = "$owner" ]; then
    return 0 # idempotent: this same owner already holds it
  fi
  # Bare `kill -0`, not `_pid_alive`: a `0` holder must read as held (the
  # process-group semantics `stream --force` pins), and lock holders are
  # always this tool's own uid anyway.
  if [ -n "$held" ] && kill -0 "$held" 2>/dev/null; then
    return 1
  fi
  rm -rf "$ld" # stale (owner PID dead/empty) — reclaim through the same mkdir gate
  if mkdir "$ld" 2>/dev/null; then
    printf '%s\n' "$owner" >"$ld/pid"
    return 0
  fi
  return 1
}

_lock_release() { rm -rf "$1"; }

# A bus line MUST fit in one write(). `printf '%s\n' … >>"$log"` is NOT reliably
# atomic under O_APPEND: bash's printf builtin doesn't guarantee one write(2)
# syscall per line, so under concurrent writers a line above a few KB can land
# as two separate writes with another process's append spliced into the gap,
# corrupting two records into one line (#20, #55). `_LINE_MAX` is a shrink
# target for readability, not what makes appends atomic — `_bus_append` below
# is what does that.
_LINE_MAX=4096
_ELIDED=' …[elided]'

# _bus_append <log> <line> — append <line> + newline to <log> as a single
# write(2) (#55). `iflag=fullblock` makes dd keep reading until its buffer is
# full or EOF instead of writing out whatever a single short read from the
# pipe returned — without it, dd's own read/write is no more atomic than the
# `printf >>` it replaces. Only POSIX-common flags (no GNU-only `oflag=append`
# or `of=`), so this also works under macOS's system BSD `dd`. `dispatch.sh`
# and `dispatch-notify.sh` are separate binaries with no shared lib to source
# this from, so each carries its own copy — keep them in sync (#61).
#
# A hard kill mid-write leaves the log with no trailing newline (#391). If the
# next append simply wrote `<line>\n`, it would glue onto the torn fragment and
# a reader would lose the whole record. Read the final byte at the size `wc`
# sampled (dd seeks there — a racing `tail -c 1` can read a mid-file byte under
# concurrency) and, when it is not a newline, prepend it into the SAME single
# write so the torn fragment stays isolated while the append stays atomic. A
# lone non-newline read also happens when a concurrent writer's line is only
# partly visible to the filesystem, so only a size that stays stable on a
# second read counts as a torn line; otherwise a healthy log would occasionally
# gain a spurious blank line.
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

# _publish_pane_state <pane> <state> <detail> [source] — mirror a bus status onto
# the pane's border options (@crew_state/@crew_detail). Bulk of the grid-hint
# contract: the pane border format renders these, so the writers stay plain
# words. Best-effort and silent: no tmux, a gone pane, or a plain bash caller
# must never fail or delay the bus write that precedes it. Detail is truncated
# to the 40-char contract before it lands. `source` is a dispatcher-internal
# marker (watchdog) the border format renders distinctly; it is empty for a
# worker's own status.
_publish_pane_state() {
  local pane="$1" state="$2" detail="${3:-}" source="${4:-}"
  [ -n "$pane" ] || return 0
  command -v tmux >/dev/null 2>&1 || return 0
  tmux set-option -p -t "$pane" @crew_state "$state" 2>/dev/null || true
  tmux set-option -p -t "$pane" @crew_detail "${detail:0:40}" 2>/dev/null || true
  tmux set-option -p -t "$pane" @crew_source "$source" 2>/dev/null || true
}

# _shrink <text> <keep> — shorten <text> to roughly <keep> characters.
# A sink body (`metrics:`, `retro:`) is itself JSON, and cutting it as a blob ends
# the string mid-object: the enclosing bus line stays valid but the body no longer
# parses, so a reader loses the WHOLE record — every key, not just the long one
# (#25). So shorten each long string leaf and re-encode instead, which keeps the
# record's shape and its short keys intact.
# Plain-text bodies (a worker's question) and `status` details are not JSON and
# still get the blob cut. `cut -c` cuts on a character boundary, so multibyte text
# never splits mid-rune.
_shrink() {
  local text="$1" keep="$2"
  if printf '%s' "$text" | jq -e 'type=="object" or type=="array"' >/dev/null 2>&1; then
    printf '%s' "$text" | jq -c --argjson k "$keep" --arg e "$_ELIDED" \
      'walk(if type=="string" and (length > $k) then .[0:$k] + $e else . end)'
  else
    printf '%s%s' "$(printf '%s' "$text" | cut -c1-"$keep")" "$_ELIDED"
  fi
}

# _fit_line <builder> <text> — echo the line <builder> builds from <text>, with
# <text> shortened until the ENCODED line fits. Re-measures each pass rather than
# computing a cut from the input length: JSON escaping is nonlinear (a control byte
# becomes six characters). The proportional guess is floored at a 3/4 step so every
# pass strictly shrinks and the loop terminates.
_fit_line() {
  local build="$1" full="$2" text="$2" line n keep
  line=$("$build" "$text")
  keep=${#full}
  while :; do
    n=$(printf '%s' "$line" | wc -c)
    { [ "$n" -le "$_LINE_MAX" ] || [ "$keep" -eq 0 ]; } && break
    keep=$(((keep * _LINE_MAX / n) < (keep * 3 / 4) ? (keep * _LINE_MAX / n) : (keep * 3 / 4)))
    text=$(_shrink "$full" "$keep")
    line=$("$build" "$text")
  done
  printf '%s' "$line"
}

# Resolve the crew id: task-document-first, so a worker's own scaffold record
# always wins over its environment — env can be lost (a re-exec, an
# env-scrubbing nested shell, a worker resumed by other means) and silently
# reconstruct #54's bug, but WORKER_TASK.md was written once at scaffold time
# and cannot be. `git rev-parse --show-toplevel` walks up from cwd on its own,
# so this resolves correctly from any subdirectory of the worktree, not just
# its root. CREW_ID is the fallback for callers with no task doc at all (a
# dispatcher session, a bare `crew` invocation outside a worker).
_crew_id() {
  local top id
  top=$(git rev-parse --show-toplevel 2>/dev/null || true)
  if [ -n "$top" ] && [ -f "$top/WORKER_TASK.md" ]; then
    id=$(grep -m1 '^crew_id:' "$top/WORKER_TASK.md" | cut -d' ' -f2 || true)
    if [ -n "$id" ]; then
      printf '%s' "$id"
      return 0
    fi
  fi
  if [ -n "${CREW_ID:-}" ]; then
    printf '%s' "$CREW_ID"
    return 0
  fi
}

# _burn_weight <model> [effort] — prints "<class>\t<weight>", or "" for a
# model the burn table (defaults.json → "burnClasses", rendered in
# dispatch-orchestration.md → "Burn classes") does not name. Each rule is a
# shell glob matched in order, first match wins. Opus is the one rung whose
# class follows effort (low/medium burn at the standard class; high stays
# premium; xhigh/max spend more tokens per turn and weigh more), so its rule
# carries a `byEffort` map with a `default` for an absent or unrecognized
# effort; every other model is classed by name alone. kimi-k3* deliberately
# falls through to "" — the burn doc does not class it — so the cursor rungs
# are matched on their exact globs, never on a `*high*` glob that would also
# swallow kimi-k3-high.
#
# Cursor prices two independent axes, so the grok rungs are enumerated rather
# than collapsed: the effort suffix sets how many tokens a turn spends, while
# `-fast` doubles what each token costs (docs: grok 4.6 and 4.7 are $2/M in + $6/M out
# standard against $4/M + $12/M fast; 4.5 charges 3x on output). Reading `-fast`
# as the cheap lane put `-medium-fast` in the same class as sonnet while it
# actually burns like premium.
#
# The table is data, resolved through dispatch-config so a user or locked layer
# can refine it; crew.sh carries no second copy. A caller pricing many models
# can preload $_BURN_SETTINGS to skip the per-model resolve.
_burn_weight() {
  local model="$1" effort="${2:-}" rules settings="${_BURN_SETTINGS:-}"
  if [ -z "$settings" ] && command -v "${DISPATCH_CONFIG_BIN:-dispatch-config}" >/dev/null 2>&1; then
    settings="$("${DISPATCH_CONFIG_BIN:-dispatch-config}" 2>/dev/null)" || settings=""
  fi
  [ -n "$settings" ] || {
    printf ''
    return 0
  }
  # \x1f, not a tab: tab is IFS-whitespace, so `read` would collapse the empty
  # class/weight fields a byEffort rule leaves.
  rules="$(jq -r '.burnClasses // [] | .[] |
    [.match,
     (if has("class") then .class else "" end),
     (if has("weight") then (.weight | tostring) else "" end),
     (if has("byEffort") then (.byEffort | tojson) else "" end)]
    | join("\u001f")' <<<"$settings")"
  local pat cls wgt byeffort
  while IFS=$'\x1f' read -r pat cls wgt byeffort; do
    [ -n "$pat" ] || continue
    # shellcheck disable=SC2254 # $pat is a glob pattern, matched literally on purpose
    case "$model" in
    $pat)
      if [ -n "$byeffort" ]; then
        jq -r --arg e "$effort" '.[$e] // .default | "\(.class)\t\(.weight)"' <<<"$byeffort"
      else
        printf '%s\t%s' "$cls" "$wgt"
      fi
      return 0
      ;;
    esac
  done <<<"$rules"
  printf ''
}

# _gh_json <gh args…> — print gh's JSON on success, print NOTHING on any
# failure (network down, 404, expired token, gh absent from PATH). ALWAYS
# returns 0: call sites are plain `out=$(_gh_json …)` under the ambient
# `set -e`, where a non-zero return would abort the whole sweep at the first
# failed call instead of merging the stored t2 forward.
_gh_json() {
  local out
  if out=$(gh "$@" 2>/dev/null); then
    printf '%s' "$out"
  fi
  return 0
}

# _hold_crew <raw> — resolve `crew hold`'s --crew flag (or _crew_id when
# omitted) and apply the same caller-supplied-id guard `watch`/`stream` use
# (crew.sh:889-894). Not containment — `<dir>/holds/<id>.md` embeds no crew
# id — but a write path with a caller-supplied id guards like its siblings.
_hold_crew() {
  local crew="${1:-$(_crew_id)}"
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  case "$crew" in
  *[!A-Za-z0-9._-]* | -* | . | ..)
    echo "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'" >&2
    exit 1
    ;;
  esac
  printf '%s' "$crew"
}

# _hold_outstanding <crew> — adds to hold:<crew> with no matching release,
# crew-scoped, unlike `retro`'s deliberately unscoped
# fold (crew.sh:2042) — a hold belongs to one crew's queue. try/catch mirrors
# retro's fromjson wart (:2065): one unparseable body must not cost every
# other hold its row. Prints `[]`, never nothing, when the log is missing —
# `due`/`list` depend on that for an unambiguous empty-array read.
_hold_outstanding() {
  local crew="$1"
  [ -f "$log" ] || {
    printf '[]'
    return 0
  }
  jq -c -s --arg crew "$crew" --arg to "hold:$crew" '
    map(select(.crew_id==$crew and .kind=="msg" and .to==$to)
        | .body | (try fromjson catch null)) | map(select(. != null)) as $bodies
    | ($bodies | map(select(.released == true) | .id)) as $released
    | $bodies | map(select(.released != true
                           and (([.id] - $released) | length) > 0))
  ' "$log"
}

# _hold_render — tab table for `hold list`/`hold due`, matching `report`'s
# header+@tsv shape; the holds array is read from stdin.
_hold_render() {
  printf 'id\tengine\twindow\tresets_at\tref\tbranch\ttitle\n'
  jq -r '.[] | [.id, .wait.engine, .wait.window, (.wait.resets_at | tostring),
                .task.ref, .task.branch, .task.title] | @tsv'
}

# _rr_role_panes <crew> — "branch\trole\tstate" per live role pane of this
# crew, sorted. Keyed on the dispatcher-set window stamps, never on discovery.
_rr_role_panes() {
  { tmux list-panes -a -F $'#{@crew_dir}\t#{@crew_id}\t#{@crew_branch}\t#{@crew_role}\t#{@crew_state}\t#{@crew_exited}' 2>/dev/null || true; } |
    awk -F'\t' -v dir="$dir" -v crew="$1" \
      '$1 == dir && $2 == crew && $3 != "" && $4 != "" && $4 != "lead" && $6 == "" { print $3 "\t" $4 "\t" $5 }' |
    sort
}

# _rr_model <crew> <role_rows> — the renderer's input, {rows, holds, roles}.
# Every fallible step returns before the caller writes anything: a failed
# model must never replace a good diagram.
_rr_model() {
  local crew="$1" rows pending holds ids='{}' roles='[]' br role st engine f id
  [ -f "$log" ] || {
    printf '{"rows":[],"holds":[],"roles":[]}'
    return 0
  }
  rows=$(bash -euo pipefail "${_rr_self:-$0}" roster "$crew") || return 1
  # `roster` folds status rows only, so a launched session reads as nothing (or
  # as its previous session's row) until it posts: it is `dispatched` until then.
  pending=$(jq -c -s --arg crew "$crew" '
      map(select(.crew_id == $crew)) as $ev
      | [$ev[] | select(.kind == "status" and (.from | type) == "string")] as $st
      | [$ev[] | select(.kind == "dispatch" and (.branch | type) == "string")]
      | group_by(.branch)
      | map(max_by(.ts) as $d | $d.branch as $b
          | ([$ev[] | select((.kind == "dispatch" or .kind == "resume") and .branch == $b) | .ts] | max) as $launch
          | ([$st[] | select(.from == "worker:" + $b or (.from | startswith("worker:" + $b + "#"))) | .ts] | max) as $last
          # A re-dispatch without --base keeps the base an earlier dispatch named.
          | (map(select((.base | type) == "string")) | max_by(.ts) | .base) as $base
          | {branch: $b, ts: $launch, base: $base, title: ($d.title // null),
             tier: ($d.tier // null), engine: ($d.engine // null), model: ($d.model // null),
             pending: ($last == null or $launch > $last)})' "$log") || return 1
  while IFS= read -r br; do
    [ -n "$br" ] || continue
    id=$(_identity_recorded "$br")
    [ -n "$id" ] || id=$(_identity "$br")
    ids=$(printf '%s' "$ids" | jq -c --arg k "$br" --argjson v "$id" '. + {($k): $v}') || return 1
  done <<EOF
$(printf '%s' "$pending" | jq -r '.[] | select(.pending) | .branch')
EOF
  holds=$(_hold_outstanding "$crew") || return 1
  # roles.json is worker-writable: a symlink could aim it at any JSON the user
  # can read, so only a regular file is read and only a known engine name kept.
  while IFS=$'\t' read -r br role st; do
    [[ $role =~ ^[A-Za-z0-9._-]{1,32}$ ]] || continue
    engine=""
    f="$dir/artifacts/$br/roles.json"
    if [ -f "$f" ] && [ ! -L "$f" ]; then
      engine=$(jq -r --arg r "$role" '.[$r].agent // empty' "$f" 2>/dev/null || true)
    fi
    case "$engine" in claude | codex | cursor | pi) ;; *) engine="?" ;; esac
    roles=$(printf '%s' "$roles" | jq -c --arg b "$br" --arg r "$role" --arg s "$st" --arg e "$engine" \
      '. + [{branch: $b, role: $r, state: $s, engine: $e}]') || return 1
  done <<EOF
$2
EOF
  printf '%s\n%s\n%s\n%s\n%s\n' "${rows:-[]}" "$pending" "$ids" "$holds" "$roles" | jq -c -n '
      [inputs] as [$r, $p, $ids, $holds, $roles]
      | ($p | map({key: .branch, value: .}) | from_entries) as $pm
      | {rows: ([$r[] | select($pm[.branch].pending | not) | . + {base: $pm[.branch].base}]
                + [$p[] | select(.pending)
                   | {branch, ts, base, title, tier, engine, model, state: "dispatched",
                      detail: null, source: null, sessions: [], pr_url: null} + $ids[.branch]]
                | sort_by(.branch)),
         holds: $holds, roles: $roles}'
}

# _rr_d2 — model on stdin -> D2 text. Every bus-, record- or tmux-sourced string
# reaches the output only inside a quoted label (q); keys are generated and the
# only bare value, the stroke color, is looked up in a fixed table of quoted hex
# codes keyed by palette name (never taken from the record), so no bus text
# reaches a style value. The table holds a muted catppuccin-like hue per name
# that reads on both themes; a palette name without an entry gets no stroke. The
# compile test in tests/crew.bats fails if a palette entry has no hex.
_rr_d2() {
  jq -r --argjson palette "$(printf '%s\n' "${_colors[@]}" | jq -R . | jq -sc .)" \
    --argjson hex '{"green":"#76a76b","blue":"#6b8fd6","magenta":"#c46bb5","orange":"#d49a62","teal":"#5fa8a0","purple":"#9a7fd0","yellow":"#c9b45f","salmon":"#d38b84","olive":"#9aa05a","steel":"#7a96ad","rust":"#b8765a","plum":"#a2709e","lime":"#9bbd5c","pink":"#d08aa8","sky":"#6fb0d6","grey":"#8b90a0","seagreen":"#5fa586","darkcyan":"#4f98a6","indigo":"#7c78c8","forestgreen":"#689a63","darkgoldenrod":"#b99342","mediumpurple":"#9d86d3","darkkhaki":"#aaa66e","dimgrey":"#7d8190","indianred":"#c27470","palevioletred":"#c9809c","peru":"#bf8b5a","crimson":"#c9606f","mediumvioletred":"#b8639a","hotpink":"#d983b3","orchid":"#b97fc7","royalblue":"#6f8ae0"}' '
    def cap($n): (if type == "string" then . elif . == null then "" else tojson end) | .[0:$n];
    # Cut at a word boundary: a mid-word cut drops the partial word.
    def trunc($n): (if type == "string" then . elif . == null then "" else tojson end)
      | if length <= $n then .
        else .[0:$n - 1] as $c
          | ($c | sub("\\s+\\S*$"; "")) as $w
          | (if (.[$n - 1:$n] | test("\\s")) or ($w | length) < ($n / 2) then $c else $w end
             | sub("\\s+$"; "")) + "…"
        end;
    def orq: if . == "" then "?" else . end;
    def q: gsub("[\u0000-\u0009\u000b-\u001f\u007f-\u009f]"; "")
      | gsub("\\\\"; "\\\\") | gsub("\""; "\\\"") | gsub("\\$"; "\\$") | gsub("\n"; "\\n")
      | "\"" + . + "\"";
    def hhmm: if type == "number" then . / 1000 | floor | strflocaltime("%H:%M") else "?" end;
    def loop: test("(^|[^A-Za-z0-9])r[0-9]+($|[^A-Za-z0-9])|revision [0-9]+|(^|[^A-Za-z])fix($|[^A-Za-z])|re-review");
    def among($s): . as $x | any($s[]; . == $x);
    .roles as $roles
    | .rows as $rows
    | def cnt($s): [$rows[] | select(.state | among($s))] | length;
    ($rows | to_entries | map(.value + {key: "w\(.key + 1)"})) as $w
    | cnt(["failed", "exited"]) as $f
    | "title: \"Crew roster\" {near: top-center; shape: text}",
      "legend: \"\(cnt(["working", "dispatched"])) active · \(cnt(["blocked"])) blocked · \(cnt(["pr_open", "done"])) done\(if $f > 0 then " · \($f) failed" else "" end)\" {near: bottom-center; shape: text}",
      "dispatcher: \"dispatcher\" {style.bold: true}",
      ($w[] | .key as $k
        | (.detail | if type == "string" then . elif . == null then "" else tojson end) as $full
        | ((.state | cap(24) | orq) + (if .source == "watchdog" then " (watchdog)" else "" end)) as $pre
        | (" · since " + (.ts | hhmm) + (.sessions | if length > 1 then " · \(length) sessions" else "" end)) as $post
        | ($full | loop) as $lp
        | (if $lp then " (loop)" else "" end) as $mark
        | (80 - ($pre | length) - ($post | length) - ($mark | length) - 3) as $room
        | (if $room < 10 then "" else $full | trunc($room) end) as $detail
        | ([(.name | cap(60) | orq),
            (.title | trunc(80) | orq),
            ([.tier, .engine, .model] | map(cap(60) | orq) | join("·") | trunc(80)),
            ($pre + (if $detail == "" then "" else " · " + $detail + $mark end) + $post)]
           | join("\n") | q) as $label
        | (if .state | among(["working", "blocked", "dispatched"]) then
             .branch as $b | [$roles[] | select(.branch == $b)] | sort_by(.role)
           else [] end) as $rp
        | "\($k): \($label) {",
          (if ($rp | length) > 0 then "  grid-rows: 1" else empty end),
          "  style: {fill: transparent; \(if .color | among($palette) then ($hex[.color] | if . then "stroke: \"\(.)\"; " else "" end) else "" end)stroke-width: 3; font-size: 16\(if .source == "watchdog" then "; stroke-dash: 3" else "" end)}",
          ($rp | to_entries[]
           | "  r\(.key + 1): \(.value | .role + "\n" + .engine + (if .state != "" then " · " + (.state | cap(32)) else "" end) | q)"),
          "}",
          "dispatcher -> \($k)",
          ((.pr_url | cap(200)) as $u
           | if (.state | among(["pr_open", "done"])) and $u != "" then
               first(($u | capture("/pull/(?<n>[0-9]+)") | .n), "") as $n
               | "\($k)_pr: \(if $n == "" then "PR" else "PR #" + $n end | q) {shape: page}",
                 "\($k) -> \($k)_pr\(if $n == "" then "" else ": " + ("#" + $n | q) end)"
             else empty end)),
      ($w[] | .key as $k | .base as $base
        | first($w[] | select(.branch == $base and .key != $k
                              and (.state | among(["working", "blocked", "pr_open", "dispatched"]))))
        | "\($k) -> \(.key): \"stacked on\""),
      (.holds | sort_by(.id) | to_entries[] | "h\(.key + 1)" as $k | .value
        | ([(("hold " + (.task.ref | cap(60))) | trunc(80)),
            (("waiting on " + (.wait.engine | cap(60)) + " " + (.wait.window | cap(60))) | trunc(80)),
            ("until " + (.wait.resets_at | if type == "number" then strflocaltime("%m-%d %H:%M") else "?" end))]
           | join("\n")) as $label
        | "\($k): \($label | q) {shape: hexagon}",
          "dispatcher -> \($k): {style.stroke-dash: 3}")'
}

# _rr_target <crew> — one diagram per crew per repo bus:
# roster-<repo>-<MM-DD-HHMM>-<h4>.d2. <repo> is the dir that owns the bus; the
# time is the crew id's epoch prefix in the renderer's local timezone (the crew
# id's own sanitized text when the prefix is not a plain 1-12 digit number
# without a leading zero); h4 is a 16-bit checksum of bus path + crew, so two
# repos' renderers, or two crews started in the same minute, collide only
# with practically negligible probability.
_rr_target() {
  local repo=${common%/*} when=${1%%-*} sum
  if [[ $when =~ ^[1-9][0-9]{0,11}$ ]]; then
    printf -v when '%(%m-%d-%H%M)T' "$when"
  else
    when=$(printf '%s' "$1" | tr -c 'A-Za-z0-9._-' '_')
  fi
  sum=$(printf '%s|%s' "$common" "$1" | cksum)
  printf '%s/roster-%s-%s-%04x.d2' "${CREW_ROSTER_DIR:-/tmp/claude-status/images/diagrams/src}" \
    "$(printf '%s' "${repo##*/}" | tr -c 'A-Za-z0-9._-' '_')" "$when" $((${sum%% *} % 65536))
}

# _rr_put <target> <content> — same-dir temp + mv, so a reader never sees a
# partial file; a symlinked or non-regular target is refused, not replaced.
_rr_put() {
  local tmp
  if [ -L "$1" ] || { [ -e "$1" ] && [ ! -f "$1" ]; }; then
    echo "crew: roster-render: $1 is not a regular file — not writing it" >&2
    return 1
  fi
  tmp=$(mktemp "${1%/*}/.roster-render.XXXXXX") || return 1
  { printf '%s\n' "$2" >"$tmp" && mv -f "$tmp" "$1"; } || {
    rm -f "$tmp"
    return 1
  }
}

# _rr_publish <file> <pane> <cdir> <no_open> — show the diagram in the aeye
# carousel beside <pane>, opening it once per pane. aeye is probed per publish,
# so one installed or upgraded after the renderer started is picked up. A
# failed publish clears the record, so the next pass retries. Best-effort: always 0.
_rr_publish() {
  local open=() help re=$'(^|\n)[[:space:]]+publish-diagram([[:space:]]|$)'
  # Captured then matched, not piped to grep, so no SIGPIPE false negative.
  help=$(timeout 10 aeye --help 2>/dev/null || true)
  [[ $help =~ $re ]] || return 0
  [ -n "$4" ] || [ "$(cat "$3/roster-render.opened" 2>/dev/null || true)" = "$2" ] || open=(--open)
  if ! timeout 60 aeye publish-diagram "$1" --pane "$2" "${open[@]}" >/dev/null 2>&1; then
    rm -f "$3/roster-render.published"
    return 0
  fi
  _rr_put "$3/roster-render.published" "$2" || true
  [ ${#open[@]} -eq 0 ] || _rr_put "$3/roster-render.opened" "$2" || true
  return 0
}

# _rr_pass <crew> <cdir> <no_open> [role_rows] — one render: write the diagram when its text
# changed, publish when written or the recorded pane is not the one last
# published to, and print the live count (working/blocked/dispatched rows plus
# outstanding holds) — the only thing it prints on stdout.
_rr_pass() {
  local model text live target pane
  model=$(_rr_model "$1" "${4-$(_rr_role_panes "$1")}") || return 1
  text=$(printf '%s' "$model" | _rr_d2) || return 1
  live=$(printf '%s' "$model" | jq '([.rows[] | select(.state == "working" or .state == "blocked" or .state == "dispatched")] | length)
                                    + (.holds | length)') || return 1
  target=$(_rr_target "$1")
  pane=$(cat "$2/roster-render.pane" 2>/dev/null || true)
  [[ $pane =~ ^%[0-9]+$ ]] || pane=""
  if [ -f "$target" ] && [ ! -L "$target" ] && printf '%s\n' "$text" | cmp -s - "$target"; then
    [ -z "$pane" ] || [ "$pane" = "$(cat "$2/roster-render.published" 2>/dev/null || true)" ] ||
      _rr_publish "$target" "$pane" "$2" "$3"
  elif mkdir -p "${target%/*}" 2>/dev/null && _rr_put "$target" "$text"; then
    [ -z "$pane" ] || _rr_publish "$target" "$pane" "$2" "$3"
  fi
  printf '%s\n' "$live"
}

# _rr_installed_crew — the `crew` this process's environment resolves to, realpath'd,
# or nothing when nothing does. A writeShellApplication wrapper — crew's own, and
# every caller that lists crew in its runtimeInputs, `dispatch` first — prepends its
# store bin dirs to PATH, so `command -v crew` inside a running daemon answers the
# build that started it and never a home-manager switch. The ambient PATH, the one a
# switch repoints, starts at the first entry outside /nix/store. realpath'd so the
# answer reads like `_rr_self`, which is one: an entry that is a symlink into the
# running build names the build already running. Always returns 0, and prints
# nothing rather than failing when no entry resolves: the caller captures it in an
# assignment, where a failing status would end the daemon.
_rr_installed_crew() {
  local d t p="${PATH-}"
  while [ -n "$p" ]; do
    case "$p" in
    *:*)
      d="${p%%:*}"
      p="${p#*:}"
      ;;
    *)
      d="$p"
      p=""
      ;;
    esac
    # Absolute only: the daemon runs from $common, so a relative entry would
    # resolve against the bus dir rather than anywhere a crew is installed.
    case "$d" in /*) ;; *) continue ;; esac
    case "$d" in /nix/store | /nix/store/*) continue ;; esac
    t="$d/crew"
    [ -f "$t" ] && [ -x "$t" ] || continue
    readlink -f -- "$t" 2>/dev/null || true
    return 0
  done
  return 0
}

# _write_if_changed — running pi workers share the target dir, so replace via a
# same-dir temp + mv (never truncate in place) and skip identical content.
_write_if_changed() { # $1=target $2=mode $3=content
  local tmp
  if [ -f "$1" ] && printf '%s\n' "$3" | cmp -s - "$1"; then
    return 0
  fi
  tmp=$(mktemp "$(dirname "$1")/.seed.XXXXXX")
  printf '%s\n' "$3" >"$tmp"
  chmod "$2" "$tmp"
  mv -f "$tmp" "$1"
}

# _link_if_changed — point a worker file at its ambient original. pi resolves
# auth.json under PI_CODING_AGENT_DIR and offers no path override, so the worker
# needs its own entry; linking rather than copying is what keeps oauth usable,
# since pi writes with a plain writeFileSync (no temp+rename anywhere in the
# bundle) and a token refresh therefore lands in the ambient file instead of
# stranding a divergent copy in this shared dir.
_link_if_changed() { # $1=target $2=source
  local tmp
  # -d also catches a symlinked dir, which would swallow the mv as a rename into it.
  if [ -d "$1" ]; then
    echo "crew: $1 is a directory — refusing to seed pi worker dir" >&2
    exit 1
  fi
  if [ "$(readlink "$1" 2>/dev/null)" = "$2" ]; then
    return 0
  fi
  tmp=$(mktemp -u "$(dirname "$1")/.seed.XXXXXX")
  ln -s "$2" "$tmp"
  mv -f "$tmp" "$1"
}

# _copy_if_changed — snapshot an ambient cache file into the worker dir.
_copy_if_changed() { # $1=target $2=source
  local tmp
  if cmp -s "$2" "$1" 2>/dev/null; then
    return 0
  fi
  tmp=$(mktemp "$(dirname "$1")/.seed.XXXXXX")
  cat "$2" >"$tmp"
  chmod 644 "$tmp"
  mv -f "$tmp" "$1"
}

# _pi_agent_dir — seed the worker-scoped PI_CODING_AGENT_DIR and print its path.
# auth.json is linked, not filtered into `!jq` read-throughs: that filter dropped
# every oauth entry, which on an OAuth-login machine is all of them, leaving
# workers with no credential at all (#198). models-store.json is copied instead —
# it is a cache pi rewrites on refresh, and every worker shares this dir, so a
# link would aim N concurrent writers at the user's real catalog. models.json is
# generated from localModels on every seed — dispatcher-owned, so a removed entry
# stops being reachable — with a dummy apiKey (local endpoints ignore it).
_pi_agent_dir() {
  local dir="$HOME/.pi/dispatcher-worker" dir_real ambient ambient_real settings dsettings clash probe bridge bridge_entry
  ambient="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"
  case "$ambient" in \~/*) ambient="$HOME/${ambient#\~/}" ;; esac
  ambient="${ambient%/}"
  # A relative dir would break the read-through once pi runs in another cwd; an
  # inherited worker dir would make the seed read itself.
  case "$ambient" in /*) ;; *) ambient="$HOME/.pi/agent" ;; esac
  [ "$ambient" != "$dir" ] || ambient="$HOME/.pi/agent"

  # mkdir -p succeeds on a symlink to a dir, and every write would land in its target.
  if [ -L "$dir" ] || { [ -e "$dir" ] && [ ! -d "$dir" ]; }; then
    echo "crew: $dir is a symlink or not a directory — refusing to seed pi worker dir" >&2
    exit 1
  fi
  mkdir -p "$dir"
  chmod 700 "$dir"

  # A symlinked parent or an aliased PI_CODING_AGENT_DIR defeats the literal check above.
  dir_real=$(cd "$dir" && pwd -P)
  if [ -d "$ambient" ]; then
    ambient_real=$(cd "$ambient" && pwd -P)
    case "$dir_real/" in "$ambient_real/"*)
      echo "crew: $dir resolves inside the ambient pi dir $ambient — refusing to seed pi worker dir" >&2
      exit 1
      ;;
    esac
    case "$ambient_real/" in "$dir_real/"*)
      echo "crew: ambient pi dir $ambient resolves inside $dir — refusing to seed pi worker dir" >&2
      exit 1
      ;;
    esac
  fi

  # pi's only subprocess hook surface is the extension API, and
  # PI_CODING_AGENT_DIR *replaces* the ambient config dir rather than
  # augmenting it — so a worker would otherwise run no hook at all. Seed
  # hookyard's generated bridge from the ambient dir (byte-identical to the one
  # hookyard installs per settings path, since it holds absolute router paths)
  # at the same bin/hookyard-bridge.ts path hookyard itself uses, and register
  # it below. No ambient bridge means hookyard is not installed; leave the
  # worker unhooked rather than dangle an extensions entry (README documents
  # the machine prerequisite).
  bridge="$ambient/bin/hookyard-bridge.ts"
  bridge_entry=""
  if [ -f "$bridge" ]; then
    mkdir -p "$dir/bin"
    _copy_if_changed "$dir/bin/hookyard-bridge.ts" "$bridge"
    bridge_entry="$dir/bin/hookyard-bridge.ts"
  fi

  settings=$(jq -s 'if length == 1 and (.[0] | type) == "object" then .[0] else {} end' \
    "$dir/settings.json" 2>/dev/null) || settings='{}'
  # A non-array extensions value is hand-edited or future-schema; appending to
  # it would make jq die with an opaque `cannot be added` and take dispatch down
  # with it. Refuse legibly, like the auth.json checks below.
  if [ -n "$bridge_entry" ] &&
    ! jq -e '(.extensions == null) or (.extensions | type) == "array"' <<<"$settings" >/dev/null 2>&1; then
    echo "crew: $dir/settings.json has a non-array extensions value — refusing to seed pi worker dir" >&2
    exit 1
  fi
  # Append-if-absent is order-preserving: jq `unique` would sort a pi-written
  # multi-entry extensions list on every reseed.
  settings=$(jq -n --argjson base "$settings" --arg b "$bridge_entry" \
    '$base + {defaultProjectTrust: "never"}
     | if ($b != "" and ((.extensions // []) | index($b) | not)) then .extensions = ((.extensions // []) + [$b]) else . end')
  _write_if_changed "$dir/settings.json" 644 "$settings"

  # Only a genuinely missing file means "no keys". [ -e ] is also false for a
  # dangling or looping symlink and behind an unsearchable dir, so "missing"
  # needs its nearest existing ancestor to be a searchable directory; anything
  # else must be a regular file, since jq blocks forever on a FIFO.
  probe="$ambient/auth.json"
  # `$(dirname "$probe")` would strip a trailing newline from a path component
  # via command substitution, letting a dangling link/mode-000 dir/FIFO under a
  # newline-suffixed name masquerade as an existing ancestor. Parameter
  # expansion doesn't strip trailing newlines; probe is always absolute (see
  # above), so stripping to "" only happens one level below "/" and the
  # fallback restores it, guaranteeing termination.
  while [ ! -e "$probe" ] && [ ! -L "$probe" ]; do
    probe=${probe%/*}
    probe=${probe:-/}
  done
  if [ "$probe" = "$ambient/auth.json" ] || [ ! -d "$probe" ] || [ ! -x "$probe" ]; then
    [ -f "$ambient/auth.json" ] || {
      echo "crew: $ambient/auth.json is not a reachable regular file — refusing to seed pi credentials" >&2
      exit 1
    }
    # Linking hands pi the file unparsed, so validate here — a worker that dies on
    # a malformed auth.json reports it as "no API key", three layers from the cause.
    jq -e 'type == "object"' "$ambient/auth.json" >/dev/null 2>&1 || {
      echo "crew: $ambient/auth.json is unreadable or not a JSON object — refusing to seed pi credentials" >&2
      exit 1
    }
    _link_if_changed "$dir/auth.json" "$ambient/auth.json"
  else
    # No ambient file: seed an empty object so pi falls through to the env vars.
    _write_if_changed "$dir/auth.json" 600 '{}'
  fi

  # A cold catalog costs more than the "custom model id" warning: pi loses the
  # model's context window, cost and thinkingLevelMap — the last is what
  # --thinking resolves against.
  if [ -f "$ambient/models-store.json" ]; then
    _copy_if_changed "$dir/models-store.json" "$ambient/models-store.json"
  fi

  # shellcheck source=/dev/null
  . "${LOCAL_MODELS_LIB:-@localModelsLib@}"
  dsettings=$("${DISPATCH_CONFIG_BIN:-dispatch-config}") || {
    echo "crew: could not resolve the dispatcher settings for the pi worker models.json — refusing to seed pi worker dir" >&2
    exit 1
  }
  # pi prefers a stored auth.json credential over a models.json apiKey, and the
  # worker's auth.json links the user's real one — a clashing provider name
  # would send that credential to the local endpoint.
  clash=$(jq -nr --argjson s "$dsettings" --slurpfile a "$dir/auth.json" \
    '($a[0] | keys | map(ascii_downcase)) as $stored
     | [($s.localModels // {}) | keys[] | split("/")[0] | select(ascii_downcase | IN($stored[]))][0] // empty')
  if [ -n "$clash" ]; then
    echo "crew: localModels provider '$clash' has a stored pi credential in auth.json — rename the provider so the credential is never sent to a local endpoint" >&2
    exit 1
  fi
  _write_if_changed "$dir/models.json" 644 "$(_local_pi_models_json "$dsettings")"

  printf '%s\n' "$dir"
}

# Per-subcommand help (#812): one case entry per top-level subcommand — synopsis,
# one-line purpose, one line per flag, one example. Synopses come from each
# command's own flag parser below, never from memory: a flag listed here but not
# parsed there is a bug. Output goes to stdout and exits 0, so `crew x --help |
# grep` works; an unknown subcommand writes to stderr and returns 1.
_crew_help() { # [<sub> [<subsub>]]
  local sub="${1:-}" subsub="${2:-}"

  if [ -z "$sub" ]; then
    cat <<'HELP'
crew — file-based coordination bus for dispatcher and worker sessions

Usage: crew <command> [args]

Run 'crew <command> --help' for one command's flags and an example.

Bus I/O:
  status          Post or update a session's state on the bus
  msg             Post a message from one agent to another
  reply           Post a message as dispatcher:<crew>
  await           Block until a message answers an outstanding question
  inbox           Print the messages addressed to one agent
  log             Print every bus event for a crew

Watching:
  watch           Park until a qualifying event lands, then print it
  stream          Long-lived watch that pushes batches to a streaming lane
  stall-watch     Per-pane liveness watchdog, spawned per worker
  pr-watch        Park until a PR changes, then post the event
  nudge           Type a wake-up line into an idle lead's input box

Roster and reporting:
  roster          One row per branch: state, age, engine, model, PR
  roster-render   Draw and publish the crew's roster diagram
  sessions        Every session id recorded on a branch
  where           A human-usable address for a worker's pane
  report          Per-run dispatch table for a crew
  rate            Sweep the bus into the ratings store, or report it
  retro           Roll up the retro notes runs posted
  dash            Terminal dashboard over the bus

Identity and crews:
  id              Print this session's crew id (never mints one)
  new             Mint a fresh crew id
  identity        Print the codename assigned to a branch
  occupants       Print the crewed panes holding a worktree path
  register        Register this crew on the repo bus
  deregister      Drop this crew's registration
  crews           List every crew with traffic on this repo's bus
  adopt           Re-attach to an on-disk crew id
  resolve-target  Resolve a target to branch, codename, host and crew
  pi-agent-dir    Print the pi agent dir prepared for a dispatch

Holds:
  hold            Park a queued dispatch on a quota window

Maintenance:
  reap            Reclaim the window and worktree of a landed PR
  git-baseline    List or accept the exec-capable git-config baseline
  engine-cmd      Whether a tmux pane command is an engine
HELP
    return 0
  fi

  if [ "$sub" = hold ]; then
    case "$subsub" in
    add)
      cat <<'HELP'
usage: crew hold add --engine E --window W --resets-at EPOCH --agent A --ref R
                     --branch B --tier T --model M --effort F
                     [--plan P] [--mcp P] [--draft] [--shape S] [--spec FILE]
                     [--crew ID] <title...>

Queue a dispatch to be taken once the quota window it is parked on resets.

  --engine      Engine whose quota is being waited on (required)
  --window      Window name, e.g. week (required)
  --resets-at   Epoch seconds when that window resets (required)
  --agent       Engine the task will be dispatched to (required; task.engine)
  --ref         Branch or commit to dispatch from (required)
  --branch      Branch the worktree gets (required)
  --tier        trivial | standard | deep (required)
  --model       Model to launch (required)
  --effort      Reasoning effort to launch (required)
  --plan        Path to the plan of record
  --mcp         MCP config to pass through
  --draft       Open the PR as a draft
  --shape       Task shape recorded on the dispatch row
  --spec        Spec file to seed the worktree with
  --crew        Crew id; defaults to this repo's crew
  <title...>    One-line task title

--engine and --agent stay distinct and neither is inferred from the other.

  crew hold add --engine claude --window week --resets-at 1791600000 \
    --agent pi --ref main --branch feat/x --tier standard \
    --model lemonade/Qwen3.8-Flash --effort medium "Echo task"
HELP
      ;;
    list)
      cat <<'HELP'
usage: crew hold list [--crew ID] [--json]

Every hold on the crew, with its window, reset time and target branch.

  --crew  Crew id; defaults to this repo's crew
  --json  Machine-readable rows

  crew hold list --crew <crew-id>
HELP
      ;;
    due)
      cat <<'HELP'
usage: crew hold due [--crew ID] [--json]

Only the holds whose quota window has already reset — what a dispatcher may
dispatch now.

  --crew  Crew id; defaults to this repo's crew
  --json  Machine-readable rows

  crew hold due --json
HELP
      ;;
    park)
      cat <<'HELP'
usage: crew hold park <default> [--crew ID]

Print how many seconds to park a watch for: the earlier of <default> and the
moment the earliest outstanding hold's window resets, never below 1 — or
<default> itself when no hold is outstanding, or the earliest already matured.
Writes nothing; it is the length to hand 'crew watch --timeout'.

  <default>  Positive whole seconds
  --crew     Crew id; defaults to this repo's crew

  crew watch --timeout "$(crew hold park 3600)"
HELP
      ;;
    release)
      cat <<'HELP'
usage: crew hold release <id> [--crew ID]

Drop a hold so it is never dispatched again (a task dispatched by hand, or one
the owner cancelled).

  <id>     Hold id from `crew hold list`
  --crew   Crew id; defaults to this repo's crew

  crew hold release h-1
HELP
      ;;
    '')
      cat <<'HELP'
usage: crew hold add --engine E --window W --resets-at EPOCH --agent A --ref R
                     --branch B --tier T --model M --effort F
                     [--plan P] [--mcp P] [--draft] [--shape S] [--spec FILE]
                     [--crew ID] <title...>
       crew hold list [--crew ID] [--json]
       crew hold due [--crew ID] [--json]
       crew hold park <default> [--crew ID]
       crew hold release <id> [--crew ID]

A queued dispatch parked on a quota window, so a successor session can resume it
without a human. Records go to the synthetic hold:<crew> sink — they can never
wake or pollute the dispatcher that wrote them, and `crew log` still shows them.

Run 'crew hold <action> --help' for one action's flags.

  crew hold list --crew <crew-id>
HELP
      ;;
    *)
      printf 'crew: no help for hold %s\n' "$subsub" >&2
      return 1
      ;;
    esac
    return 0
  fi

  case "$sub" in
  status)
    cat <<'HELP'
usage: crew status <from> <state> [detail] [pr] [--restamp] [--]

Post or update a session's state on the crew bus.

  <from>     The posting session: worker:<branch>#s… or dispatcher:<crew>
  <state>    working | blocked | pr_open | done | failed | exited
  [detail]   Short context; put -- before one that starts with a dash
  [pr]       PR url, which pr_open names
  --restamp  blocked only: refresh liveness without re-delivering a reply
  --         Everything after this is positional

pr_open and done are refused on a standard/deep implement run until the branch
carries a review seam and a deslop seam.

  crew status "$CREW_WORKER_ID" working "execute: tests"
HELP
    ;;
  msg)
    cat <<'HELP'
usage: crew msg <from> <to> <body>

Post a message from one agent to another on the crew bus.

  <from>   Sender id, e.g. "$CREW_WORKER_ID"
  <to>     Recipient: dispatcher:<crew>, worker:<branch>#s…, role:<branch>:<role>
  <body>   One argument, so its quoting survives

A recipient left as a bare prefix (`dispatcher:` from an id that expanded to
nothing) is rejected rather than posted where nobody reads it.

  crew msg "$CREW_WORKER_ID" dispatcher:<crew-id> "gate is green"
HELP
    ;;
  reply)
    cat <<'HELP'
usage: crew reply <to> <body> [--crew ID]

Post a message as dispatcher:<crew>, so a dispatcher never rebuilds its own id.

  <to>    Recipient id (worker:<branch>#s…, role:<branch>:<role>)
  <body>  One argument
  --crew  Crew id when the caller's env carries no CREW_ID

  crew reply "worker:feat/x#s<epoch>-<pid>" "rebase onto main"
HELP
    ;;
  await)
    cat <<'HELP'
usage: crew await <agent> [--from SENDER] [--timeout S] [--interval S]

Block until a message answers <agent>'s outstanding question, print it, exit 0.

  <agent>     The id that asked — usually "$CREW_WORKER_ID"
  --from      Accept a reply only from this exact sender id
  --timeout   Seconds to wait (default 300). On expiry stdout stays empty and
              stderr gets "ended after Ns" — that line is the marker, the exit
              code stays 0, so never branch on $? here
  --interval  Poll interval in seconds (default 2)

Every due message from the reply's sender prints, oldest first, one compact JSON
object per line, so a backlog drains instead of hiding. A held poll, not a spin
loop: zero token cost while it waits.

  crew await "$CREW_WORKER_ID" --from "dispatcher:<crew-id>" --timeout 300
HELP
    ;;
  inbox)
    cat <<'HELP'
usage: crew inbox <agent> [crew] [--since TS]

Print the messages addressed to <agent> — messages only, never status rows.

  <agent>   worker:<branch>#s… (the session suffix is required), role:<branch>:<role>
  [crew]    Crew id; defaults to this repo's crew
  --since   One non-blocking pass over messages strictly newer than TS

Omitting --since returns everything, unchanged; a directive posted mid-stage is
only visible at the next peek, so carry the cursor forward. A worker:<…> reader
marks what it prints as delivered, so a later `crew await` will not return it.

  crew inbox "$CREW_WORKER_ID" --since 1791400000000
HELP
    ;;
  log)
    cat <<'HELP'
usage: crew log [crew]

Print every bus event for a crew as JSON lines: status rows, and msg rows to
real recipients or to the synthetic hold:, retro:, metrics: and review: sinks
that wake nobody — a deslop seam rides review:<crew>, not a sink of its own.

  [crew]  Crew id; defaults to this repo's crew

  crew log | jq -r 'select(.kind=="msg") | .from + " -> " + .to'
HELP
    ;;
  watch)
    cat <<'HELP'
usage: crew watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] [--crew ID]

Park until any worker event qualifies, then print {"cursor":TS,"events":[…]}.

  --since     Cursor to read from; omitted, it self-seeds from the crew's cursor
              file so a stale caller cursor cannot re-deliver
  --states    States that qualify (default blocked,pr_open,done,failed,exited);
              a msg to the dispatcher always qualifies
  --timeout   Park length in seconds (default 3300). An indefinite park is
              rejected: a reaped indefinite watch is undetectable
  --interval  Poll interval in seconds (default 2)
  --crew      Crew id when the caller's env carries no CREW_ID

An expired park exits 0 with empty stdout — that is the marker, not a failure.

  crew watch --since "$seen" --timeout 300
HELP
    ;;
  stream)
    cat <<'HELP'
usage: crew stream [--crew ID] [--states a,b,c] [--park S] [--heartbeat S]
                   [--coalesce S] [--retry S] [--interval S] [--force] [--reap-every S]
       crew stream --status [--crew ID]

Wrap `crew watch` in a long-lived process, so a streaming lane gets pushed
batches instead of re-arming a one-shot park every turn.

  --crew         Crew id — required in spirit: the command string a lane arms
                 carries no ambient CREW_ID
  --states       States that qualify (default blocked,pr_open,done,failed,exited)
  --park         Park per cycle in seconds (default 300)
  --heartbeat    Idle heartbeat every S seconds (default 3300)
  --coalesce     Collapse events within S seconds (default 5)
  --retry        Reconnect every S seconds after a failure (default 30)
  --interval     Inner watch poll interval (default 2)
  --force        Take over a lane that is already streaming
  --reap-every   Reap finished workers every S seconds (default 900)
  --status       Report the daemon's state instead of running one

Crew-resolution and usage failures exit 64, not 1, so a `--status` call that
could not find its crew does not read as the `dead` state it never measured.

  crew stream --crew <crew-id> --park 300
HELP
    ;;
  stall-watch)
    cat <<'HELP'
usage: crew stall-watch <worker-id|branch|role:branch:role> --pane <id> [--engine E]
                        [--grace S] [--stall S] [--window S] [--interval S] [--idle S]
                        [--dead S] [--max-life S] [--load S] [--release S] [--bg-wait S]
                        [--launch S] [--unread S] [--runaway-hits N] [--runaway-tokens N]
                        [--no-budget] [--budget-refresh S] [--no-nudge]

Lifetime-scoped liveness watchdog: the bus reflects only what a worker posts, so
one parked on a prompt looks exactly like one that is working.

  <target>    worker:<branch>#s…, a bare branch, or role:<branch>:<role> (a role
              id selects prompt-only mode)
  --pane      tmux pane id to sample (required)
  --engine    Engine family (claude|codex|cursor|pi); the default "unknown"
              leaves the prompt, meter, quota and runaway detectors off
  --grace     Silence after launch (default 45)
  --stall     Meter-advancing/token-static window before turn-stall: (default 300)
  --window    Startup window a static pane may still be starting in (default 900)
  --interval  Sample interval (default 15)
  --idle      Byte-identical pane that counts as quiet: (default 1800)
  --dead      Second-evidence delay before an episode escalates to failed (default 1800)
  --max-life  Watchdog lifetime cap (default 43200)
  --load      Host-load window for load: (default 300)
  --release   Wait after a done/failed before releasing the window (default 300)
  --bg-wait   How long a finished turn waiting on a background shell holds the
              static-pane and quiet detectors (default 7200)
  --launch    Seconds after which a bare shell with no engine is stalled: (default 150)
  --unread    Age of an undelivered directive that trips unread: (default 600)
  --runaway-hits    Samples of a leaked model sentinel before runaway: (default 3)
  --runaway-tokens  Output growth required alongside it (default 1500)
  --no-budget       Turn off the quota-window detector (dispatch's --ignore-budget)
  --budget-refresh  Refresh the engine budget cache every S (default 900)
  --no-nudge        Do not auto-nudge a lead past an overdue directive

Every detector posts a recoverable blocked state; only quiet: and turn-stall:
escalate, and only after the second --dead evidence check.

  crew stall-watch "worker:feat/x#s<epoch>-<pid>" --pane %N --grace 45
HELP
    ;;
  pr-watch)
    cat <<'HELP'
usage: crew pr-watch <N> [--repo owner/name] [--timeout S] [--interval S]

Park until PR <N> changes, print the event, and post it to dispatcher:<crew> so
an armed `crew watch` wakes.

  <N>         Pull request number
  --repo      owner/name; defaults to the current repo
  --timeout   Seconds to park
  --interval  Poll interval

The park, the change signals and the per-PR cursor live in the pr-watch binary;
empty stdout is that binary's timeout marker, not a failure.

  crew pr-watch 123 --timeout 600
HELP
    ;;
  nudge)
    cat <<'HELP'
usage: crew nudge <codename|branch|worker:<branch>#s…> [--crew ID] [--wait [SECONDS]]

Type the constant wake-up line into a lead's idle input box, so a lead whose wake
expired reads the directive `crew reply` already posted.

  <target>  Codename, branch or session id; the pane comes from the window's
            dispatcher-anchored stamps, never a saved pane id
  --crew    Crew id when the caller's env carries no CREW_ID
  --wait    Keep trying while the lead is on a live turn, up to SECONDS
            (default 1800)

Exit 0 typed and accepted, 1 usage or resolution error, 2 refused before typing,
3 typed but not accepted.

  crew nudge feat/x --crew <crew-id>
HELP
    ;;
  roster)
    cat <<'HELP'
usage: crew roster [crew]

A JSON array with one object per branch — sessions folded to the latest first,
so three sessions on a branch never read as one flip-flopping identity — each
with state, detail, age_s, engine, model, tier, pr_url, codename and colour.

  [crew]  Crew id; defaults to this repo's crew

  crew roster | jq -r '.[] | [.branch, .state, .name] | @tsv'
HELP
    ;;
  roster-render)
    cat <<'HELP'
usage: crew roster-render --crew ID [--pane %N] [--no-open] [--once | --detach]
                          [--interval S] [--quiet S]

Draw the crew's roster diagram from the bus and its live role panes, and publish
it to the aeye carousel beside the recorded dispatcher pane.

  --crew       Crew id (required). One renderer per repo bus: a crew spanning
               several repos gets one diagram per repo, named by the owner
  --pane       Dispatcher pane to publish beside
  --no-open    Render and record without opening the carousel
  --once       Render one frame and exit
  --detach     Record the pane, start the daemon detached, return
  --interval   Redraw interval (default 2)
  --quiet      Exit after S seconds with no live role pane (default 1800;
               0 exits as soon as the crew drains)

Usage errors exit 64, like `stream`; --once and --detach together are one.

  crew roster-render --crew <crew-id> --pane %N --detach
HELP
    ;;
  sessions)
    cat <<'HELP'
usage: crew sessions <branch> [--crew ID]

Every session id recorded on <branch> — the ids a resume continues from, and the
ones a watchdog or a reply may be addressed to.

  <branch>  Branch name
  --crew    Crew id; defaults to this repo's crew

  crew sessions feat/x
HELP
    ;;
  where)
    cat <<'HELP'
usage: crew where <codename|branch|%id> [--crew ID]

A human-usable address for a worker's pane: codename, session:window.pane, window
name, role and the jump command.

  <target>  Codename (lime), branch, or a tmux pane id such as %N
  --crew    Crew id; defaults to this repo's crew

Resolved from dispatcher-anchored state only — never git discovery in a worktree
or the caller's env. Non-zero with a readable message when the pane is gone.

  crew where lime
HELP
    ;;
  report)
    cat <<'HELP'
usage: crew report [crew]

Per-run dispatch table for a crew: engine, model, tier, shape, outcome, duration.

  [crew]  Crew id; defaults to this repo's crew

  crew report <crew-id>
HELP
    ;;
  rate)
    cat <<'HELP'
usage: crew rate [--report [--pooled] [--json]] [--sweep-all [--root DIR]...]

Sweep a repo's bus into the global ratings store, or render that store.

  (no flags)  Sweep this repo's bus into the ratings store
  --report    Render the per-(engine, model, tier) rollup instead; reads only the
              global store — no bus, no network
  --pooled    Pool the tiers into one row per engine and model
  --json      Machine-readable report
  --sweep-all Walk every repo instead of this one — the one command with no use
              for the caller's own, so it runs outside a repo
  --root      Repo root to sweep; repeatable, only with --sweep-all

  crew rate --report --json
HELP
    ;;
  retro)
    cat <<'HELP'
usage: crew retro [--report [--json]]

Read-only rollup of the retro notes workers and the dispatcher post to the
synthetic retro: and metrics: sinks.

  (no flags)  One row per run: branch, engine, model, tier, outcome, tags
  --report    Aggregate rollup of tags across runs
  --json      Machine-readable; only with --report

No crew filter and no positional: notes are cross-run evidence, like `rate`.

  crew retro --report
HELP
    ;;
  dash)
    cat <<'HELP'
usage: crew dash [--once|--json]

Terminal dashboard over the crew bus: roster, budget, holds, retro.

  --once  Print one snapshot and exit instead of running the UI
  --json  Emit that snapshot as JSON

  crew dash --once
HELP
    ;;
  id)
    cat <<'HELP'
usage: crew id

Print this session's crew id, resolved from WORKER_TASK.md and then CREW_ID.
Read-only: it never mints one. Exit 1 with the recovery hints when there is none.

  crew id
HELP
    ;;
  new)
    cat <<'HELP'
usage: crew new

Mint a fresh crew id (timestamp-pid, unique by construction) and print it.
Registers nothing — the dispatcher records it on its first event.

  CREW_ID=$(crew new)
HELP
    ;;
  identity)
    cat <<'HELP'
usage: crew identity <branch> [crew]
       crew identity --hash <name>

Print the codename and colours for <branch>, assigning one from the pool on first
call; --hash goes the other way, from a name to its identity.

  <branch>  Branch whose identity to print or assign
  [crew]    Crew the assignment is recorded for; defaults to this repo's crew

  crew identity feat/x
HELP
    ;;
  occupants)
    cat <<'HELP'
usage: crew occupants <worktree-path>

Print the crewed windows and panes standing on <worktree-path> — what a dispatch
would reuse or replace before it launches.

  <worktree-path>  Absolute path, exactly as the window records it

  crew occupants /home/me/git/.worktrees/repo/feat/x
HELP
    ;;
  register)
    cat <<'HELP'
usage: crew register [pid]

Register this crew on the repo bus. N crews may share a repo, each keyed by its
crew id, so there is no cross-crew lock.

  [pid]  Long-lived dispatcher pid to record; defaults to the nearest non-shell
         ancestor, which is what an agent's shell tool needs

  crew register
HELP
    ;;
  deregister)
    cat <<'HELP'
usage: crew deregister

Drop this crew's registration from the repo bus. Its events stay in the log.

  crew deregister
HELP
    ;;
  crews)
    cat <<'HELP'
usage: crew crews

List every crew with traffic on this repo's bus: id, last and first event, worker
count, dispatcher pid and whether that pid is alive. Both sources count — a crew
dir alone (watch creates one) and an id seen only in the log.

  crew crews
HELP
    ;;
  adopt)
    cat <<'HELP'
usage: crew adopt [--force] <id> [pid]

Re-attach to an on-disk crew after a restart lost CREW_ID.

  --force  Adopt even though another pid is recorded as its dispatcher
  <id>     Crew id, from `crew crews`
  [pid]    Dispatcher pid to record; defaults to the nearest non-shell ancestor

--force is stripped from anywhere in the args, so it can never land as the pid.
Adopting a crew whose recorded dispatcher pid is dead also releases the
`dispatched` label from the GitHub issues that crew claimed but never finished.

  crew adopt <crew-id>
HELP
    ;;
  resolve-target)
    cat <<'HELP'
usage: crew resolve-target <target> [--crew ID]

Resolve a target to one TSV line: branch, codename, host and crew.

  <target>  `#N`, a Linear id, a branch, a codename or a worker id
  --crew    Restrict the lookup to one crew

Exit 1 when nothing matches, 2 when several branches do (all of them listed).

  crew resolve-target '#123'
HELP
    ;;
  pi-agent-dir)
    cat <<'HELP'
usage: crew pi-agent-dir

Print the pi agent dir prepared for a dispatch, refreshing models.json from the
dispatch config. Fails when a local provider name would shadow a credential the
user stored in auth.json.

  PI_OFFICIAL_CACHE_DIR=$(crew pi-agent-dir)
HELP
    ;;
  reap)
    cat <<'HELP'
usage: crew reap [--quiet] [--dry-run] [--no-wait] [--idle S]

Reclaim a worker's tmux window and worktree once its PR has landed. The PR, not
elapsed time, is the gate: a done worker sits for as long as its PR takes.

  --quiet     Suppress the per-worker notes
  --dry-run   Print what would be reclaimed, change nothing
  --no-wait   Do not wait out the idle threshold on a terminal lead
  --idle      Idle seconds before the idle-release phase kills the window (default 300)

No crew filter: the workers worth reaping belong to earlier dispatcher sessions.

  crew reap --dry-run
HELP
    ;;
  git-baseline)
    cat <<'HELP'
usage: crew git-baseline [--accept]

List — or, with --accept, merge into — the exec-capable and redirecting git-config
baseline the worktree guard enforces.

  --accept  Merge exactly the pairs this run prints into the baseline. Needs a
            real terminal and a typed yes, and is refused otherwise. Listing
            exits 1 on drift or a missing baseline, 0 when clean.

Values print %q-escaped, so a planted escape sequence cannot redraw the terminal.

  crew git-baseline
HELP
    ;;
  engine-cmd)
    cat <<'HELP'
usage: crew engine-cmd <pane_current_command>

Whether a tmux pane command is an engine (claude, codex, cursor-agent, pi, node).
Exit 0 when it is, 1 when it is not; prints nothing.

  <pane_current_command>  tmux's #{pane_current_command}; a leading dot and a
                          -wrapped suffix are tolerated

  crew engine-cmd node && echo engine
HELP
    ;;
  *)
    printf 'crew: no help for %s\n\n' "$sub" >&2
    _crew_help '' >&2
    return 1
    ;;
  esac
}

sub="${1:-}"
shift || true

# --help anywhere it can mean "help": as the command itself, and as the FIRST
# argument of one (#812). Deliberately before the repo, crew-id and bus access
# below, so `crew <sub> --help` answers in a bare temp dir. Only the first
# position counts — `crew status w1 working --help` keeps --help as the detail.
case "$sub" in
--help | -h | help)
  _crew_help "${1:-}" "${2:-}"
  exit $?
  ;;
hold)
  # hold takes an action, so its --help is the second argument, not the first.
  case "${2:-}" in
  --help | -h)
    _crew_help hold "${1:-}"
    exit $?
    ;;
  esac
  ;;
esac
case "${1:-}" in
--help | -h)
  _crew_help "$sub"
  exit $?
  ;;
esac
# `id`, `new` and `identity` need no repo / no crew id.
if [ "$sub" = id ]; then
  # Read-only: resolves via _crew_id (WORKER_TASK.md, else env) and never mints.
  # Capture-then-print normalises _crew_id's inconsistent trailing newline (#29).
  id=$(_crew_id)
  if [ -n "$id" ]; then
    printf '%s\n' "$id"
    exit 0
  fi
  echo "crew: no crew id — CREW_ID unset and no WORKER_TASK.md crew_id; run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one" >&2
  exit 1
fi
if [ "$sub" = new ]; then
  # The old bare-`id` minting behaviour, verbatim — now explicit and opt-in (#29).
  printf '%s\n' "$(date +%s)-$$"
  exit 0
fi
if [ "$sub" = occupants ]; then
  [ -n "${1:-}" ] || {
    echo "crew: occupants <worktree-path>" >&2
    exit 1
  }
  _occupants "$1"
  printf '\n'
  exit 0
fi
if [ "$sub" = engine-cmd ]; then
  [ -n "${1:-}" ] || {
    echo "crew: engine-cmd <pane_current_command>" >&2
    exit 1
  }
  _is_engine_cmd "$1"
  exit $?
fi
if [ "$sub" = pi-agent-dir ]; then
  _pi_agent_dir
  exit 0
fi
# dash needs no repo either — it degrades every repo-scoped pane on its own.
if [ "$sub" = dash ]; then
  CREW_BIN=$(readlink -f "$0") exec crew-dash "$@"
fi

# repo-keyed bus dir; --path-format=absolute so main-checkout and worktrees
# resolve to a byte-identical path (load-bearing — see #29).
common=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)
if [ -z "$common" ]; then
  # `rate --sweep-all` walks every repo, so it is the one command that has no
  # use for the caller's own (a cron job or a shell in $HOME has none).
  case "$sub $*" in
  "rate "*--sweep-all*) ;;
  *)
    echo "crew: not in a git repo" >&2
    exit 1
    ;;
  esac
fi
dir="$common/crew"
log="$dir/events.jsonl"

if [ "$sub" = identity ]; then
  [ -n "${1:-}" ] || {
    echo "crew: identity <branch> [crew]" >&2
    exit 1
  }
  if [ "$1" = --hash ]; then
    _identity "${2:?crew: identity --hash <name>}"
    exit 0
  fi
  _identity_assign "$1" "${2:-$(_crew_id)}"
  exit 0
fi

case "$sub" in
where)
  # where <codename|branch|%id> [--crew ID] — a human-usable address for a
  # worker's pane: codename, session:window.pane, window name, role and the
  # jump command. Resolved from dispatcher-anchored state only (the window's
  # @crew_* stamps, the dispatch rows), never from git discovery in a worktree
  # or the worker's env. Non-zero with a readable message when it is gone.
  where_crew=$(_crew_id)
  where_target=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew)
      [ -n "${2:-}" ] || _where_die "--crew needs an id (usage: crew where <codename|branch|%id> [--crew ID])"
      where_crew="$2"
      shift 2
      ;;
    --*)
      _where_die "unknown flag '$1' (usage: crew where <codename|branch|%id> [--crew ID])"
      ;;
    *)
      [ -z "$where_target" ] || _where_die "one target only (usage: crew where <codename|branch|%id> [--crew ID])"
      where_target="$1"
      shift
      ;;
    esac
  done
  [ -n "$where_target" ] || _where_die "usage: crew where <codename|branch|%id> [--crew ID]"

  # A tmux read failure is not "the target is gone".
  where_wins=$(tmux list-windows -a -F $'#{window_id}\t#{@crew_branch}\t#{@crew_dir}\t#{@crew_id}\t#{@crew_name}\t#{session_name}\t#{window_index}\t#{window_name}' 2>/dev/null) ||
    _where_die "cannot read tmux windows (is a tmux server running?)"
  where_panes=$(tmux list-panes -a -F $'#{window_id}\t#{pane_id}\t#{@crew_role}\t#{pane_index}' 2>/dev/null) ||
    _where_die "cannot read tmux panes (is a tmux server running?)"
  # Windows anchored to this crew: a non-empty branch, this repo's crew dir,
  # and this crew's id when the window carries one. Keep the display fields.
  where_cwins=$(printf '%s\n' "$where_wins" |
    awk -F'\t' -v dir="$dir" -v crew="$where_crew" \
      'NF >= 8 && $2 != "" && $3 == dir && (crew == "" || $4 == "" || $4 == crew) { print $1 "\t" $2 "\t" $5 "\t" $6 "\t" $7 "\t" $8 }')

  where_win=""
  where_pane=""
  case "$where_target" in
  '%'[0-9]*)
    # A pane id keeps that exact pane.
    where_wid=$(printf '%s\n' "$where_panes" | awk -F'\t' -v p="$where_target" '$2 == p { print $1; exit }')
    [ -n "$where_wid" ] || _where_die "no pane $where_target"
    where_win=$(printf '%s\n' "$where_cwins" | awk -F'\t' -v w="$where_wid" '$1 == w { print; exit }')
    [ -n "$where_win" ] || _where_die "pane $where_target is not a pane of this crew"
    where_pane="$where_target"
    ;;
  *)
    where_win=$(printf '%s\n' "$where_cwins" | awk -F'\t' -v b="$where_target" '$2 == b { print; exit }')
    if [ -z "$where_win" ]; then
      where_matches=$(printf '%s\n' "$where_cwins" | awk -F'\t' -v n="$where_target" '$3 == n { print }')
      where_n=$(printf '%s\n' "$where_matches" | grep -c . || true)
      [ "$where_n" -le 1 ] || _where_die "ambiguous codename '$where_target' — matches $(printf '%s\n' "$where_matches" | cut -f2 | paste -sd, -); pass a branch or %pane"
      where_win="$where_matches"
    fi
    if [ -z "$where_win" ]; then
      where_dbr=$(_resolve_target "$where_target" "$where_crew" | cut -f1 | paste -sd, -)
      case "$where_dbr" in
      *,*) _where_die "ambiguous target '$where_target' — matches $where_dbr; pass a branch or %pane" ;;
      ?*) _where_die "no live pane for '$where_target' (branch $where_dbr) — its window is gone" ;;
      esac
      _where_die "no worker matches '$where_target'"
    fi
    ;;
  esac

  where_wid=$(printf '%s' "$where_win" | cut -f1)
  where_branch=$(printf '%s' "$where_win" | cut -f2)
  where_name=$(printf '%s' "$where_win" | cut -f3)
  where_sess=$(printf '%s' "$where_win" | cut -f4)
  where_widx=$(printf '%s' "$where_win" | cut -f5)
  where_wname=$(printf '%s' "$where_win" | cut -f6)

  if [ -z "$where_pane" ]; then
    where_pane=$(printf '%s\n' "$where_panes" | awk -F'\t' -v w="$where_wid" '$1 == w && $3 == "lead" { print $2; exit }')
    [ -n "$where_pane" ] || where_pane=$(printf '%s\n' "$where_panes" | awk -F'\t' -v w="$where_wid" '$1 == w { print $2; exit }')
  fi
  [ -n "$where_pane" ] || _where_die "no pane in the window for '$where_target'"

  where_role=$(printf '%s\n' "$where_panes" | awk -F'\t' -v p="$where_pane" '$2 == p { print $3; exit }')
  where_pidx=$(printf '%s\n' "$where_panes" | awk -F'\t' -v p="$where_pane" '$2 == p { print $4; exit }')
  # A plain (non-grid) dispatch window stamps no @crew_role: its lone pane is lead.
  [ -n "$where_role" ] || where_role=lead
  [ -n "$where_name" ] || where_name=$(_identity "$where_branch" | jq -r .name)

  printf '%s — %s:%s.%s "%s" (%s pane)   jump: ! tmux switch-client -t %s\n' \
    "$where_name" "$where_sess" "$where_widx" "$where_pidx" "$where_wname" "$where_role" "$where_pane"
  ;;
nudge)
  # nudge <codename|branch|worker:<branch>#s…> [--crew ID] — type the constant
  # $_nudge_line into a worker lead's idle input box, so a lead whose wake
  # expired reads the directive already posted with `crew reply`. The pane is resolved from the window's dispatcher-anchored @crew_*
  # stamps, never a pane id. Exit 0 accepted, 1 usage or resolution error, 2
  # refused before typing, 3 typed but not accepted.
  # --wait [SECONDS] (default 1800) loops _nudge_pane while the lead is on a
  # live turn, one waiter per lead session (a second call joins and reports the
  # first's outcome only when it covers the caller's msg); any other refusal
  # is immediate, and a lead that reads the msg meanwhile ends the wait untyped (exit 0).
  _nudge_fail() {
    echo "crew: nudge: $*" >&2
    exit 1
  }
  _nudge_refuse() {
    echo "crew: nudge: $*" >&2
    exit 2
  }
  # Typing into a sibling's pane is the dispatcher's call alone.
  nudge_top=$(git rev-parse --show-toplevel 2>/dev/null || true)
  if [ -n "${CREW_WORKER_ID:-}" ] || [ -n "${CREW_ROLE_ID:-}" ] ||
    { [ -n "$nudge_top" ] && [ -f "$nudge_top/WORKER_TASK.md" ]; }; then
    echo "crew: nudge is a dispatcher command" >&2
    exit 2
  fi
  nudge_crew=$(_crew_id)
  nudge_target=""
  nudge_wait=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew)
      [ -n "${2:-}" ] || _nudge_fail "--crew needs an id (usage: crew nudge <codename|branch|worker:<branch>#s…> [--crew ID] [--wait [SECONDS]])"
      nudge_crew="$2"
      shift 2
      ;;
    --wait)
      nudge_wait=1800
      case "${2:-}" in
      '' | *[!0-9]*) shift ;;
      *)
        nudge_wait="$2"
        shift 2
        ;;
      esac
      ;;
    --*)
      _nudge_fail "unknown flag '$1' (usage: crew nudge <codename|branch|worker:<branch>#s…> [--crew ID] [--wait [SECONDS]])"
      ;;
    *)
      [ -z "$nudge_target" ] || _nudge_fail "one target only (usage: crew nudge <codename|branch|worker:<branch>#s…> [--crew ID] [--wait [SECONDS]])"
      nudge_target="$1"
      shift
      ;;
    esac
  done
  [ -n "$nudge_target" ] || _nudge_fail "usage: crew nudge <codename|branch|worker:<branch>#s…> [--crew ID] [--wait [SECONDS]]"
  [ -n "$nudge_crew" ] || _nudge_fail "CREW_ID not set and no WORKER_TASK.md crew_id — pass --crew <id>"
  nudge_want=""
  case "$nudge_target" in
  '%'*) _nudge_fail "pass a codename, branch or worker id — a pane id is not an anchored address" ;;
  worker:*)
    nudge_target="${nudge_target#worker:}"
    if _is_session_id "$nudge_target"; then
      nudge_want="worker:$nudge_target"
      nudge_target="${nudge_target%#*}"
    fi
    ;;
  esac

  nudge_wins=$(tmux list-windows -a -F $'#{window_id}\t#{@crew_branch}\t#{@crew_dir}\t#{@crew_id}\t#{@crew_name}' 2>/dev/null) ||
    _nudge_fail "cannot read tmux windows (is a tmux server running?)"
  nudge_panes=$(tmux list-panes -a -F $'#{window_id}\t#{pane_id}\t#{@crew_role}\t#{pane_current_command}' 2>/dev/null) ||
    _nudge_fail "cannot read tmux panes (is a tmux server running?)"
  nudge_cwins=$(printf '%s\n' "$nudge_wins" |
    awk -F'\t' -v dir="$dir" -v crew="$nudge_crew" 'NF >= 5 && $2 != "" && $3 == dir && $4 == crew { print $1 "\t" $2 "\t" $5 }')
  nudge_win=$(printf '%s\n' "$nudge_cwins" | awk -F'\t' -v b="$nudge_target" '$2 == b { print; exit }')
  if [ -z "$nudge_win" ]; then
    nudge_matches=$(printf '%s\n' "$nudge_cwins" | awk -F'\t' -v n="$nudge_target" '$3 == n { print }')
    nudge_n=$(printf '%s\n' "$nudge_matches" | grep -c . || true)
    [ "$nudge_n" -le 1 ] || _nudge_fail "ambiguous codename '$nudge_target' — matches $(printf '%s\n' "$nudge_matches" | cut -f2 | paste -sd, -); pass a branch"
    nudge_win="$nudge_matches"
  fi
  if [ -z "$nudge_win" ]; then
    nudge_dbr=$(_resolve_target "$nudge_target" "$nudge_crew" | cut -f1 | paste -sd, -)
    case "$nudge_dbr" in
    *,*) _nudge_fail "ambiguous target '$nudge_target' — matches $nudge_dbr; pass a branch" ;;
    ?*) _nudge_fail "no live pane for '$nudge_target' (branch $nudge_dbr) — its window is gone" ;;
    esac
    _nudge_fail "no worker matches '$nudge_target'"
  fi
  nudge_wid=$(printf '%s' "$nudge_win" | cut -f1)
  nudge_branch=$(printf '%s' "$nudge_win" | cut -f2)
  nudge_pane=$(printf '%s\n' "$nudge_panes" | awk -F'\t' -v w="$nudge_wid" '$1 == w && $3 == "lead" { print $2; exit }')
  [ -n "$nudge_pane" ] || nudge_pane=$(printf '%s\n' "$nudge_panes" | awk -F'\t' -v w="$nudge_wid" '$1 == w { print $2; exit }')
  [ -n "$nudge_pane" ] || _nudge_fail "no pane in the window for '$nudge_target'"

  nudge_last=$(_sessions "$nudge_branch" "$nudge_crew" | jq -c 'last // empty')
  [ -n "$nudge_last" ] || _nudge_refuse "no session on $nudge_branch — dispatch a worker before nudging one"
  [ "$(printf '%s' "$nudge_last" | jq -r .terminal)" != true ] ||
    _nudge_refuse "newest session on $nudge_branch is $(printf '%s' "$nudge_last" | jq -r .state) — a stopped session never reads its inbox; re-dispatch with the context baked in"
  [ "$(printf '%s' "$nudge_last" | jq -r .state)" != null ] ||
    _nudge_refuse "lead has not posted its first status yet — wait for it to start"
  [ "$(printf '%s' "$nudge_last" | jq -r .session)" != null ] ||
    _nudge_refuse "$nudge_branch has no session id on the bus — a branch-only address can never reach a live worker's inbox; re-dispatch"
  nudge_sid=$(printf '%s' "$nudge_last" | jq -r .worker_id)
  [ -z "$nudge_want" ] || [ "$nudge_want" = "$nudge_sid" ] ||
    _nudge_refuse "$nudge_want is not the newest session on $nudge_branch ($nudge_sid)"
  nudge_engine=$(jq -nRr --arg c "$nudge_crew" --arg w "$nudge_sid" '
    [inputs | fromjson? | objects | select(.crew_id == $c and (.kind == "dispatch" or .kind == "resume") and .worker_id == $w)]
    | last | .engine // empty' "$log" 2>/dev/null || true)
  [ -n "$nudge_engine" ] || _nudge_refuse "no dispatch or resume row records $nudge_sid's engine"
  nudge_ts=$(_unread_scan "$nudge_crew" "$nudge_branch" "worker:$nudge_branch" "$nudge_sid" 0 dispatcher | cut -d' ' -f2)
  [ -n "$nudge_ts" ] ||
    _nudge_refuse "no unread msg from dispatcher:$nudge_crew to $nudge_sid — post the directive first (crew reply), then nudge"

  _frame_classifier
  nudge_rc=0
  if [ -z "$nudge_wait" ]; then
    nudge_out=$(_nudge_pane "$nudge_pane" "$nudge_engine" "$nudge_sid" "$nudge_crew" "dispatcher:$nudge_crew" "$nudge_ts") || nudge_rc=$?
    [ "$nudge_rc" -ne 2 ] || _nudge_refuse "refused — $nudge_out"
    printf '%s\n' "$nudge_out"
    exit "$nudge_rc"
  fi

  # <result> <rc> <detail> <line> — ends the wait: records the outcome once a
  # waiting row exists, so a joiner can report it.
  nudge_waiting=0
  _nudge_wait_end() {
    [ "$nudge_waiting" = 0 ] || _nudge_wait_row resolved "$1" "$2" "$3"
    [ "$2" -ne 2 ] || _nudge_refuse "$4"
    printf '%s\n' "$4"
    exit "$2"
  }
  nudge_start=$(_clock_now)
  nudge_start_ms=$(_clock_now_ms)
  nudge_interval="${CREW_NUDGE_WAIT_INTERVAL:-15}"
  nudge_ld="$dir/nudge-wait/$(printf '%s' "$nudge_sid" | tr -c 'A-Za-z0-9._-' '_').$(printf '%s' "$nudge_sid" | cksum | cut -d' ' -f1).d"
  mkdir -p "$dir/nudge-wait"
  nudge_joining=0
  until _lock_acquire "$nudge_ld" $$; do
    nudge_hpid=$(cat "$nudge_ld/pid" 2>/dev/null || true)
    if [ "$nudge_joining" = 0 ]; then
      nudge_joining=1
      printf 'nudge joining: %s %s — another crew nudge --wait (pid %s) owns this lead'"'"'s wait\n' "$nudge_pane" "$nudge_sid" "$nudge_hpid"
    fi
    [ "$(_clock_now)" -lt $((nudge_start + nudge_wait)) ] ||
      _nudge_refuse "refused — still waiting on another crew nudge --wait (pid $nudge_hpid)"
    _clock_sleep "$nudge_interval"
  done
  trap '_lock_release "$nudge_ld"' EXIT
  if [ "$nudge_joining" = 1 ]; then
    nudge_prev=$(tail -n 2000 "$log" 2>/dev/null | jq -Rnc --arg c "$nudge_crew" --arg to "$nudge_sid" --argjson t "$nudge_start_ms" --argjson m "$nudge_ts" '
      [inputs | fromjson? | objects | select(.crew_id == $c and .kind == "nudge_wait" and .state == "resolved" and .to == $to and .ts >= $t and (.msg_ts // 0) >= $m)] | last // empty')
    if [ -n "$nudge_prev" ]; then
      nudge_rc=$(printf '%s' "$nudge_prev" | jq -r .rc)
      nudge_res=$(printf '%s' "$nudge_prev" | jq -r '"\(.result): \(.detail)"')
      _nudge_wait_end joined "$nudge_rc" "$nudge_res" "nudge joined: $nudge_pane $nudge_sid — $nudge_res"
    fi
  fi

  while :; do
    nudge_prev=$(tail -n 2000 "$log" 2>/dev/null | jq -Rnc --arg c "$nudge_crew" --arg to "$nudge_sid" --argjson m "$nudge_ts" '
      [inputs | fromjson? | objects | select(.crew_id == $c and .kind == "nudge" and .to == $to and (.msg_ts // 0) >= $m)] | last // empty')
    if [ -n "$nudge_prev" ]; then
      nudge_res=$(printf '%s' "$nudge_prev" | jq -r .result)
      nudge_detail=$(printf '%s' "$nudge_prev" | jq -r .detail)
      nudge_rc=3
      [ "$nudge_res" != accepted ] || nudge_rc=0
      _nudge_wait_end already "$nudge_rc" "$nudge_res: $nudge_detail" "nudge already typed: $nudge_pane $nudge_sid — $nudge_res: $nudge_detail"
    fi
    read -r nudge_old _ <<<"$(_unread_scan "$nudge_crew" "$nudge_branch" "worker:$nudge_branch" "$nudge_sid" 0 dispatcher)"
    if [ -z "$nudge_old" ] || [ "$nudge_old" -gt "$nudge_ts" ]; then
      _nudge_wait_end read 0 "the lead read the msg during the wait; nothing typed" \
        "nudge not needed: $nudge_pane $nudge_sid — the lead read the msg during the wait; nothing typed"
    fi
    nudge_rc=0
    nudge_out=$(_nudge_pane "$nudge_pane" "$nudge_engine" "$nudge_sid" "$nudge_crew" "dispatcher:$nudge_crew" "$nudge_ts") || nudge_rc=$?
    if [ "$nudge_rc" -ne 2 ]; then
      nudge_res="${nudge_out#nudge }"
      _nudge_wait_end "${nudge_res%%:*}" "$nudge_rc" "${nudge_out#* — }" "$nudge_out"
    fi
    [ "$nudge_out" = "live turn or no idle input box" ] || _nudge_wait_end refused 2 "$nudge_out" "refused — $nudge_out"
    if [ "$nudge_waiting" = 0 ]; then
      nudge_waiting=1
      _nudge_wait_row waiting "" "" "live turn"
      printf 'nudge waiting: %s %s — live turn; re-checking every %ss for up to %ss\n' "$nudge_pane" "$nudge_sid" "$nudge_interval" "$nudge_wait"
    fi
    nudge_waited=$(($(_clock_now) - nudge_start))
    [ "$nudge_waited" -lt "$nudge_wait" ] ||
      _nudge_wait_end timeout 2 "$nudge_out (waited ${nudge_waited}s)" "refused — $nudge_out (waited ${nudge_waited}s)"
    _clock_sleep "$nudge_interval"
  done
  ;;
resolve-target)
  # resolve-target <target> [--crew ID] — the branch, codename, host and crew a
  # `#N`, Linear id, branch, codename or worker id names, as one TSV line.
  # Exit 1 when nothing matches, 2 when several branches do (all listed).
  rt_crew=""
  rt_target=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew)
      [ -n "${2:-}" ] || {
        echo "crew: resolve-target: --crew needs an id" >&2
        exit 1
      }
      rt_crew="$2"
      shift 2
      ;;
    --*)
      echo "crew: resolve-target: unknown flag '$1'" >&2
      exit 1
      ;;
    *)
      [ -z "$rt_target" ] || {
        echo "crew: resolve-target: one target only" >&2
        exit 1
      }
      rt_target="$1"
      shift
      ;;
    esac
  done
  [ -n "$rt_target" ] || {
    echo "usage: crew resolve-target <target> [--crew ID]" >&2
    exit 1
  }
  rt_out=$(_resolve_target "$rt_target" "$rt_crew")
  rt_n=$(printf '%s\n' "$rt_out" | grep -c . || true)
  if [ "$rt_n" -eq 0 ]; then
    echo "crew: resolve-target: no worker matches '$rt_target'" >&2
    exit 1
  fi
  if [ "$rt_n" -gt 1 ]; then
    echo "crew: resolve-target: ambiguous target '$rt_target' — matches: $(printf '%s\n' "$rt_out" | cut -f1 | paste -sd, - | sed 's/,/, /g'); pass a branch" >&2
    exit 2
  fi
  printf '%s\n' "$rt_out"
  ;;
status | msg)
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  mkdir -p "$dir"
  if [ "$sub" = status ]; then
    # status <from> <state> [detail] [pr_url] [--restamp] [--]
    # --restamp may sit anywhere; strip it so the positionals stay in order.
    restamp=false
    sargs=()
    while [ $# -gt 0 ]; do
      case "$1" in
      --restamp)
        restamp=true
        shift
        ;;
      --)
        shift
        sargs+=("$@")
        break
        ;;
      --*)
        echo "crew: status: unknown arg '$1' (use -- before a detail that starts with dashes)" >&2
        exit 1
        ;;
      *)
        sargs+=("$1")
        shift
        ;;
      esac
    done
    set -- "${sargs[@]+"${sargs[@]}"}"
    # Reject an unknown state: the log IS the state, and a junk value is silently
    # absorbed by every reader (`{"state":""}` reached the log once and rendered as
    # a blank roster row). Fail loudly at the writer instead.
    case "${2:-}" in
    working | blocked | pr_open | done | failed | exited) ;;
    *)
      echo "crew: status state must be one of working|blocked|pr_open|done|failed|exited (got '${2:-}')" >&2
      exit 1
      ;;
    esac
    if [ "$restamp" = true ] && [ "${2:-}" != blocked ]; then
      echo "crew: --restamp is only valid with blocked" >&2
      exit 1
    fi
    # Terminal states are posted once. A worker that re-announces `pr_open`/`done`
    # (seen: the same pr_open+done pair 4s apart) wakes `watch` twice with an
    # identical batch, so the dispatcher handles the same completion again.
    case "${2:-}" in
    pr_open | done | failed | exited)
      if [ -f "$log" ]; then
        prev=$(jq -r --arg c "$crew" --arg m "${1:-}" \
          'select(.crew_id==$c and .kind=="status" and .from==$m)
             | "\(.body.state)\t\(.body.pr_url // "")"' "$log" 2>/dev/null | tail -1 || true)
        [ "$prev" = "${2:-}"$'\t'"${4:-}" ] && exit 0
      fi
      ;;
    esac
    from="${1:-}" state="${2:-}" pr="${4:-}"
    # Seams from any earlier session on this branch in this crew count (the
    # resume case), and a worker that cannot review stops on an ungated state
    # (blocked/failed), so refusing here never strands one. A seam is the lead's
    # own post-verdict `review:<crew>` msg, or — on pi, whose reviewer pane IS
    # the gate — the pane's verdict. On pi the log is folded in order and fails
    # closed: only an exact accept/revise from the reviewer is honoured, any
    # other reviewer reply — a reject, an unknown or elided verdict, a body
    # that is not a JSON object, an unparsable line naming this crew and the
    # reviewer, or an object carrying neither seam nor verdict — counts as a
    # reject that blocks the lead's own seam until the reviewer's next
    # accept/revise; a reviewer note with a tag and no verdict, and a role
    # lifecycle event ({"event":"role_exited"}), are not verdicts and are
    # ignored. Any lead -> reviewer msg
    # except the bare {"final":true} release, even an unparseable one, voids
    # every earlier verdict AND the lead's own earlier review seam until a
    # fresh verdict lands. The assignment is never itself a seam. The deslop
    # seam is gated the same way (branch-keyed, earlier sessions count,
    # presence only): the lead's own {"seam":"deslop"} msg to review:<crew>.
    case "$state:$from" in
    pr_open:worker:* | done:worker:*)
      top=$(git rev-parse --show-toplevel 2>/dev/null || true)
      if [ -n "$top" ] && [ -f "$top/WORKER_TASK.md" ]; then
        tier=$(sed -n 's/^tier:[[:space:]]*//p' "$top/WORKER_TASK.md" | head -1 | tr -d '[:space:]' || true)
        kind=$(sed -n 's/^kind:[[:space:]]*//p' "$top/WORKER_TASK.md" | head -1 | tr -d '[:space:]' || true)
        engine=$(sed -n 's/^engine:[[:space:]]*//p' "$top/WORKER_TASK.md" | head -1 | tr -d '[:space:]' || true)
        if [ "$state:${kind:-implement}" = pr_open:implement ]; then
          # Every item must be <id> pass(<evidence>) or <id> waived(dispatcher[<sep><note>]);
          # evidence nests via Oniguruma \g<b>. The id is one token so `AC2 pending AC3 pass(y)`
          # cannot parse as one item. rc 1 = conforming; any rc but 0/1 means the check itself
          # failed, so refuse rather than let it through.
          ledger_rc=0
          d=${3:-}
          jq -en --arg d "$d" '
            "(?:[^\\s;,()]+\\s+)?(?:pass(?=\\(\\s*[^\\s)])(?<b>\\((?:[^()]|\\g<b>)*\\))|waived\\(dispatcher(?:[:;,\\s](?:[^()]|\\g<b>)*)?\\))" as $item
            | $d | test("^\\s*(?:\($item)(?:\\s*[;,]\\s*|\\s+|\\s*[;.]?\\s*$))*$"; "i") | not' \
            >/dev/null 2>&1 || ledger_rc=$?
          if [ "$ledger_rc" -ne 0 ] && [ "$ledger_rc" -ne 1 ]; then
            echo "crew: refusing pr_open for $from — could not check the acceptance ledger (jq exit $ledger_rc)" >&2
            exit 1
          fi
          hint='every acceptance ledger item must read <id> pass(<evidence>) or <id> waived(dispatcher), e.g. "AC1 pass(bats 12/12); AC2 waived(dispatcher)", with any note inside those parentheses; pending, not run, skipped, n/a, partial, or a note after the parentheses is refused. An item you cannot run is not a pass: post blocked "acceptance: <item> — <why>" and await the dispatcher, who alone waives it.'
          # A task doc has an acceptance list when a line is a `##`/`###` (or
          # deeper) heading — including one that wraps the word in bold or
          # underscores on the same line, `### **Acceptance criteria**` — a
          # bold/underscore-wrapped run at line start (`**Acceptance:**`,
          # `**Acceptance criteria**`), or a line-start `Acceptance:` — all
          # case-insensitive. The match stays anchored to one of those, so a
          # word merely mentioned mid-sentence, or a wrapped prose line that
          # only starts with the word, is not read as a list. A task doc with
          # no such list must not carry a ledger at all, so steer that worker
          # at the empty detail instead of at the grammar.
          acceptance_re='^[[:space:]]*(#{2,}[[:space:]]+[*_]{0,2}acceptance|[*_]{1,2}acceptance|acceptance:)'
          if [ "$ledger_rc" -eq 0 ]; then
            if grep -Eqi "$acceptance_re" "$top/WORKER_TASK.md"; then
              echo "crew: refusing pr_open for $from — $hint" >&2
            else
              echo "crew: refusing pr_open for $from — this task doc has no acceptance list (no heading, bold, or line-start spelling of Acceptance), so an empty detail is the correct pr_open: crew status \"\$CREW_WORKER_ID\" pr_open \"\" <url>. Do not invent a pass(...) item. ($hint)" >&2
            fi
            exit 1
          fi
          if [ -z "${d//[[:space:]]/}" ] && grep -Eqi "$acceptance_re" "$top/WORKER_TASK.md"; then
            echo "crew: refusing pr_open for $from — the task doc has an acceptance list, so the pr_open detail must carry its ledger: $hint" >&2
            exit 1
          fi
          # Content checks beyond the form: a local gate is not a CI run, and only a
          # dispatcher reply waives. An item is a CI item when its evidence, or its
          # task-doc Acceptance entry (matched by id, else the id's number, else the
          # item's ledger position), names CI as a word ("Non-CI" does not). The
          # waiver check is presence-only, not authentication: any process can post
          # as dispatcher:<crew>.
          ci_re='(^|[^-[:alnum:]_])CI([^[:alnum:]_]|$)'
          run_re='actions/runs/[0-9]+|(^|[^[:alnum:]_-])[Rr]un([ _-]?[Ii][Dd])?[ :#=]*[0-9]{6,}'
          accept_items=$(awk '
            /^[[:space:]]*(```|~~~)/ { fence = !fence; next }
            fence { next }
            tolower($0) ~ /^[[:space:]]*(##+[[:space:]]+[*_]*acceptance|[*_][*_]?acceptance|acceptance:)/ { on = 1; next }
            on && /^[[:space:]]*#+[[:space:]]/ { on = 0 }
            on && tolower($0) ~ /^[[:space:]]*[*_]+out[ -]of[ -]scope/ { on = 0 }
            on && /^([-*+]|[0-9]+[.)])[[:space:]]/ {
              sub(/^([-*+]|[0-9]+[.)])[[:space:]]+/, ""); print }
          ' "$top/WORKER_TASK.md") || {
            echo "crew: refusing pr_open for $from — could not read the task doc's acceptance list" >&2
            exit 1
          }
          items=$(jq -nr --arg d "$d" '
            "(?:(?<id>[^\\s;,()]+)\\s+)?(?:(?<k>pass)(?=\\(\\s*[^\\s)])(?<b>\\((?:[^()]|\\g<b>)*\\))|(?<w>waived)\\(dispatcher(?:[:;,\\s](?:[^()]|\\g<b>)*)?\\))" as $item
            | [$d | match($item; "gi") | [.captures[] | select(.name != null) | {(.name): .string}] | add]
            | .[] | [(.id // ""), (if .k != null then "pass" else "waived" end), (.b // "" | gsub("[\\n\\r\\x1f]"; " "))] | join("\u001f")') || {
            echo "crew: refusing pr_open for $from — could not parse the acceptance ledger items" >&2
            exit 1
          }
          idx=0
          while IFS=$'\x1f' read -r iid ikind iev; do
            [ -n "$ikind" ] || continue
            iid=${iid%:}
            idx=$((idx + 1))
            if [ "$ikind" = pass ]; then
              is_ci=false
              if [[ $iev =~ $ci_re ]]; then
                is_ci=true
              else
                entry=""
                if [ -n "$iid" ]; then
                  entry=$(printf '%s\n' "$accept_items" | awk -v id="$iid" '
                    { t = $0; gsub(/^[*_]+/, "", t) }
                    tolower(substr(t, 1, length(id))) == tolower(id) && substr(t, length(id) + 1, 1) !~ /[[:alnum:]_.]/ { print; exit }')
                fi
                if [ -z "$entry" ]; then
                  ord=$idx
                  [[ -z $iid || $iid =~ ^([Aa][Cc][-_]?)?0*([1-9][0-9]*)$ ]] && {
                    [ -z "$iid" ] || ord=${BASH_REMATCH[2]}
                    entry=$(printf '%s\n' "$accept_items" | awk -v n="$ord" 'NR == n')
                  }
                fi
                ! [[ $entry =~ $ci_re ]] || is_ci=true
              fi
              if [ "$is_ci" = true ] && ! [[ $iev =~ $run_re ]]; then
                echo "crew: refusing pr_open for $from — acceptance item '${iid:-?}' is a CI item, so its pass(...) must carry a CI run id or an actions/runs/<id> URL (for the PR's current head); a local gate (pre-push, bats-affected, shellcheck, nix flake check) is never CI evidence. If CI has not finished, wait for it, or block and ask the dispatcher to waive the item." >&2
                exit 1
              fi
            else
              # One clause (split on . ; ! ? newline and " but ") waives an id when it has
              # a waive word and the id as a token, and no negation anywhere in it.
              if [ -z "$iid" ]; then
                echo "crew: refusing pr_open for $from — a waived(dispatcher) item needs its acceptance id (e.g. AC3 waived(dispatcher)) so the dispatcher's waiver can name it." >&2
                exit 1
              fi
              if [ -f "$log" ] && jq -e -n -R --arg c "$crew" --arg f "$from" --arg id "$iid" '
                ($id | ascii_downcase | gsub("(?<ch>[^a-z0-9_ -])"; "\\\(.ch)")) as $ide
                | [inputs | (try fromjson catch null) | select(type == "object"
                  and .crew_id == $c and .kind == "msg" and .from == ("dispatcher:" + $c)
                  and .to == $f)
                  | (.body // "") | tostring | ascii_downcase
                  | split("(?:[.;!?](?=\\s|$)|\\n|\\bbut\\b)"; "g")[]
                  | select(test("\\bwaiv(?:e|es|ed|ing|er)\\b")
                    and (test("(?:\\b(?:not|never|no|cannot|without)\\b|n(?:\u0027|\u2019)t\\b)[^\\n]*\\bwaiv") | not)
                    and test("(?:^|[^a-z0-9_.-])" + $ide + "(?:$|[^a-z0-9_.-])"))] | length > 0' "$log" >/dev/null 2>&1; then
                continue
              fi
              echo "crew: refusing pr_open for $from — waived(dispatcher) needs a dispatcher waiver on the bus: a crew reply to this session ($from) that names the item ('${iid:-?}') in a waive phrase, e.g. \"waive ${iid:-<id>}\"; a negation (\"will not waive\") does not count (a reply sent to an earlier session does not carry over). Block and ask the dispatcher to waive the item; do not write the waiver yourself." >&2
              exit 1
            fi
          done <<<"$items"
        fi
        case "$tier:${kind:-implement}" in
        standard:implement | deep:implement)
          b="${from%#s*}"
          r="role:${b#worker:}:reviewer"
          seams=0
          if [ -f "$log" ]; then
            fold_rc=0
            seams=$(jq -Rnr --arg c "$crew" --arg b "$b" --arg r "$r" --arg e "$engine" '
              reduce inputs as $line (
                {ok: false, rejected: false, pending: false};
                (try [$line | fromjson] catch null) as $p
                | if $p == null then
                    if $e == "pi" and ($line | test("\\S")) and ($line | contains("\"crew_id\":" + ($c | tojson)) and contains($r | tojson | .[1:-1]))
                    then .ok = false | .rejected = true
                    else . end
                  elif ($p[0] | type) != "object" then .
                  else
                    $p[0] as $m
                    | ($m.crew_id == $c and $m.kind == "msg") as $mine
                    | (if $mine then (($m.body | fromjson?) // null) else null end) as $o
                    | (($m.from // "") | tostring | sub("#s[^#]*$"; "")) as $f
                    | (($m.to // "") | tostring | sub("#s[^#]*$"; "")) as $t
                    | if $e == "pi" and $mine and ($o | type) != "object" then
                        if $f == $r then .ok = false | .rejected = true
                        elif $f == $b and $m.to == $r then .pending = true | .ok = false
                        else . end
                      elif ($o | type) != "object" then .
                      elif $e == "pi" and $f == $r and ($o | has("seam") or has("verdict"))
                           and (($o | has("tag") and (has("verdict") | not)) | not) then
                        if $o.seam == "review" and $o.verdict == "accept" then
                          if $t == $b then .pending = false | .ok = true | .rejected = false else . end
                        elif $o.seam == "review" and $o.verdict == "revise" then
                          if $t == $b then .pending = false | .ok = false | .rejected = false else .ok = false end
                        else .ok = false | .rejected = true end
                      elif $e == "pi" and $f == $r
                           and (($o | has("seam") or has("verdict")) | not)
                           and (($o | has("event") or has("tag")) | not) then
                        .ok = false | .rejected = true
                      elif $e == "pi" and $f == $b and $m.to == $r
                           and (($o | keys) == ["final"] and $o.final == true | not) then
                        .pending = true | .ok = false
                      elif $o.seam != "review" or ($o | has("tag")) then .
                      elif $f == $b and $m.to == ("review:" + $c)
                           and (($o | has("review_mode") | not) or ($o.review_mode | IN("full", "downgraded"))) then
                        if .rejected or .pending then . else .ok = true end
                      else . end
                  end
              ) | if .ok then 1 else 0 end' "$log" 2>/dev/null) || fold_rc=$?
            if [ "$fold_rc" -ne 0 ]; then
              echo "crew: refusing $state for $from — could not read the crew log for the review seam (jq exit $fold_rc)" >&2
              exit 1
            fi
          fi
          if [ "${seams:-0}" != 1 ]; then
            echo "crew: refusing $state for $tier session $from — no review seam on the bus for this branch; run the code review gate, ingest its verdict, then crew msg \"\$CREW_WORKER_ID\" \"review:$crew\" '{\"seam\":\"review\",\"review_mode\":\"full\"}' (or downgraded) and retry; a review request or a pane that has not returned a verdict is not a review; a review that cannot run goes blocked/failed, never pr_open/done. On pi the reviewer pane's latest verdict decides: accept passes, revise needs your own review:$crew seam after you fix it, a reject (or any reply that is not an exact accept/revise) blocks until the reviewer's next verdict, and a re-request (any msg from you to the reviewer except the {\"final\":true} release) cancels every earlier verdict and your own earlier seam until a new verdict arrives" >&2
            exit 1
          fi
          deslop=0
          if [ -f "$log" ]; then
            deslop_rc=0
            deslop=$(jq -Rnr --arg c "$crew" --arg b "$b" '
              first(inputs
                | (try fromjson catch null)
                | select(type == "object" and .crew_id == $c and .kind == "msg"
                         and .to == ("review:" + $c)
                         and ((.from // "") | tostring | sub("#s[^#]*$"; "")) == $b)
                | (.body | fromjson? // null)
                | select(type == "object" and .seam == "deslop" and (has("tag") | not))
                | 1) // 0' "$log" 2>/dev/null) || deslop_rc=$?
            if [ "$deslop_rc" -ne 0 ]; then
              echo "crew: refusing $state for $from — could not read the crew log for the deslop seam (jq exit $deslop_rc)" >&2
              exit 1
            fi
          fi
          if [ "$deslop" != 1 ]; then
            echo "crew: refusing $state for $tier session $from — no deslop seam on the bus for this branch; run the harness deslop skill (dispatcher:deslop on claude, \$deslop on codex, deslop on cursor and pi) over the diff you are about to push, commit its cleanup, then crew msg \"\$CREW_WORKER_ID\" \"review:$crew\" '{\"seam\":\"deslop\"}' and retry. The seam records that the skill ran — never post it just to get past this gate" >&2
            exit 1
          fi
          ;;
        esac
      fi
      ;;
    esac
    _build_status() {
      jq -nc --arg crew "$crew" --arg from "$from" --arg state "$state" \
        --arg detail "$1" --arg pr "$pr" --argjson restamp "$restamp" \
        '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
          kind:"status",
          body:({state:$state}
                + (if $detail!="" then {detail:$detail} else {} end)
                + (if $pr!="" then {pr_url:$pr} else {} end)
                + (if $restamp then {restamp:true} else {} end))}'
    }
    line=$(_fit_line _build_status "${3:-}")
  else
    # msg <from> <to> <body>
    from="${1:-}" to="${2:-}"
    # A caller that expanded an unset shell var into the recipient (e.g.
    # `dispatcher:$CREW_ID` with CREW_ID empty) silently lands a message
    # nobody reads (#46). Fail loudly on any prefix left with no id.
    case "$to" in
    *:)
      echo "crew: msg recipient '$to' is missing an id after the colon" >&2
      exit 1
      ;;
    esac
    _build_msg() {
      jq -nc --arg crew "$crew" --arg from "$from" --arg to "$to" --arg body "$1" \
        '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:$to, kind:"msg", body:$body}'
    }
    line=$(_fit_line _build_msg "${3:-}")
  fi
  _bus_append "$log" "$line"
  # A worker's status belongs on its OWN lead pane; a role pane's does not
  # (CREW_ROLE_ID set, and its @crew_role is the role, not `lead`). The
  # @crew_role=lead check also keeps a dispatcher-process `crew status`
  # (dispatch-resume) from painting the dispatcher's own pane.
  if [ "$sub" = status ] && [ -z "${CREW_ROLE_ID:-}" ] && [ -n "${TMUX_PANE:-}" ] &&
    command -v tmux >/dev/null 2>&1 &&
    [ "$(tmux display-message -p -t "$TMUX_PANE" '#{@crew_role}' 2>/dev/null || true)" = lead ]; then
    _publish_pane_state "$TMUX_PANE" "$state" "${3:-}"
  fi
  ;;
reply)
  # reply <to> <body> [--crew ID] — sugar over `msg`; from is dispatcher:<crew>
  # so the dispatcher needn't reconstruct its own id.
  rcrew=""
  rargs=()
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew)
      [ -n "${2:-}" ] || {
        echo "crew: --crew needs a value" >&2
        exit 1
      }
      rcrew="$2"
      shift 2
      ;;
    *)
      rargs+=("$1")
      shift
      ;;
    esac
  done
  set -- "${rargs[@]+"${rargs[@]}"}"
  crew="${rcrew:-$(_crew_id)}"
  mkdir -p "$dir"
  # A branch-only worker target resolves to the newest session on that branch, so
  # the dispatcher keeps writing `worker:<branch>` while the message lands on a
  # session that exists NOW. Resolving at send time is what makes inheritance
  # impossible: a stopped session's successor has a different id, so a directive
  # written for the former is never addressed to the latter (#17).
  to="${1:-}"
  case "$to" in
  worker:*)
    if _is_session_id "$to"; then
      : # explicit worker:<branch>#s<epoch>-<pid>, honour verbatim
    else
      br="${to#worker:}"
      if [ -n "$crew" ]; then
        newest=$(_sessions "$br" "$crew" | jq -c 'last')
      else
        # No crew named: a dispatcher whose shell never exported CREW_ID. Look
        # across crews and take the single one with a live session on the branch.
        # "Live" is the crew's registered pid (see `crews`): a crew that crashed
        # before posting a terminal status leaves a non-terminal session that is
        # not listening, and must not force --crew while a real crew is live
        # (#327).
        live=""
        crews=""
        [ ! -f "$log" ] || crews=$(jq -r 'select(.crew_id != null) | .crew_id' "$log" | sort -u)
        while IFS= read -r c; do
          [ -n "$c" ] || continue
          n=$(_sessions "$br" "$c" | jq -c --arg c "$c" 'last // empty | select(.terminal | not) | . + {crew:$c}')
          [ -z "$n" ] || {
            # A present pid that is dead (or `0`/non-numeric, which `crews` also
            # reads as not alive) is a crashed crew. No pid at all is unknown,
            # not dead: an unregistered crew or a `--crew-id`-only caller stays
            # live, so this never narrows the pre-#327 resolver.
            cpid=$(cat "$dir/crews/$c/pid" 2>/dev/null || true)
            case "$cpid" in
            '') calive=null ;;
            *[!0-9]* | 0) calive=false ;;
            *) if _recorded_pid_live "$cpid" "$dir/crews/$c/pid"; then calive=true; else calive=false; fi ;;
            esac
            live+=$(printf '%s' "$n" | jq -c --argjson a "$calive" '. + {crew_alive:$a}')$'\n'
          }
        done <<<"$crews"
        # Drop known-dead crews only while a not-known-dead candidate remains. If
        # every candidate's crew is dead, keep them all — one still delivers,
        # several still refuse naming both, exactly as before this change.
        if printf '%s' "$live" | jq -e -s 'any(.[]; .crew_alive != false)' >/dev/null 2>&1; then
          live=$(printf '%s' "$live" | jq -c -s '.[] | select(.crew_alive != false)')
        fi
        nlive=$(printf '%s' "$live" | grep -c . || true)
        if [ "$nlive" -gt 1 ]; then
          echo "crew: CREW_ID not set and $br has live sessions in crews: $(printf '%s' "$live" | jq -r .crew | paste -sd, - | sed 's/,/, /g') — pass --crew <id>" >&2
          exit 1
        elif [ "$nlive" -eq 1 ]; then
          newest=$(printf '%s' "$live" | jq -c .)
          crew=$(printf '%s' "$newest" | jq -r .crew)
        else
          echo "crew: CREW_ID not set and no live session on $br in any crew — pass --crew <id> (or dispatch a worker before replying to one)" >&2
          exit 1
        fi
      fi
      [ -n "$newest" ] && [ "$newest" != null ] || {
        echo "crew: no session on $br — dispatch a worker before replying to one" >&2
        exit 1
      }
      if [ "$(printf '%s' "$newest" | jq -r .terminal)" = true ]; then
        echo "crew: newest session on $br is $(printf '%s' "$newest" | jq -r .state) — a stopped session never reads its inbox; re-dispatch with the context baked in" >&2
        exit 1
      fi
      if [ "$(printf '%s' "$newest" | jq -r .session)" = null ]; then
        echo "crew: $br has no session id on the bus — a branch-only address can never reach a live worker's inbox; re-dispatch" >&2
        exit 1
      fi
      to=$(printf '%s' "$newest" | jq -r .worker_id)
    fi
    ;;
  esac
  [ -n "$crew" ] || {
    echo "crew: CREW_ID not set and no WORKER_TASK.md crew_id — pass --crew <id>" >&2
    exit 1
  }
  _build_reply() {
    jq -nc --arg crew "$crew" --arg to "$to" --arg body "$1" \
      '{ts:(now*1000|floor), crew_id:$crew, from:("dispatcher:"+$crew), to:$to, kind:"msg", body:$body}'
  }
  line=$(_fit_line _build_reply "${2:-}")
  _bus_append "$log" "$line"
  ;;
await)
  # await <agent> [--from SENDER] [--timeout S] [--interval S] — block until a msg
  # addressed to <agent> answers its outstanding question, print it, exit 0.
  # Every due msg from the sender of the newest due one prints, oldest first, one
  # compact JSON object per line, so a backlog is drained instead of hidden: the
  # per-sender delivered mark can never rise past an undelivered sibling (#466).
  # --from restricts that to one exact sender id (a lead waiting on one role's
  # verdict); other senders' msgs are neither returned nor marked delivered.
  # A msg qualifies when this session has not been handed it yet: strictly newer
  # than the newest msg from that sender already handed to this session (the
  # per-sender delivered mark, written by await and inbox, #290). No outbound
  # question filter is applied: a reply that crossed the worker's own question
  # in flight is unread and must still be delivered (#385).
  # A timeout also exits 0: empty stdout, not the exit code, is the marker.
  # No LLM tokens burned: this is a held bash call, not a
  # spin loop. A late reply is never lost — it stays in the durable log for the
  # worker's next inbox check.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  me="${1:-}"
  [ -n "$me" ] || {
    echo "crew: await <agent> [--from SENDER] [--timeout S] [--interval S]" >&2
    exit 1
  }
  case "$me" in worker:*) _is_session_id "$me" || {
    echo "crew: $sub: '$me' has no session suffix — pass the session id (\$CREW_WORKER_ID); a branch-only worker id matches no message" >&2
    exit 1
  } ;; esac
  shift || true
  timeout=300
  interval=2
  from=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --timeout)
      [ -n "${2:-}" ] || {
        echo "crew: --timeout needs a value" >&2
        exit 1
      }
      timeout="$2"
      shift 2
      ;;
    --interval)
      [ -n "${2:-}" ] || {
        echo "crew: --interval needs a value" >&2
        exit 1
      }
      interval="$2"
      shift 2
      ;;
    --from)
      [ -n "${2:-}" ] || {
        echo "crew: --from needs a value" >&2
        exit 1
      }
      from="$2"
      shift 2
      ;;
    *)
      echo "crew: await: unknown arg '$1'" >&2
      exit 1
      ;;
    esac
  done
  start=$(_clock_now_ms)
  deadline=$((start + timeout * 1000))
  delivered=$(_await_marks "$crew" "$me")
  while :; do
    if [ -f "$log" ]; then
      # A msg from X is due when it is newer than the last msg from X this
      # session was handed (the delivered mark). The last due msg picks its
      # sender, whose whole due backlog then prints — so the mark, raised to
      # the batch's newest ts, never skips a sibling. `-R` + `fromjson?` skips
      # a torn trailing line (the hard-kill crash mode) instead of aborting
      # the read.
      ans=$(jq -Rnc --arg crew "$crew" --arg me "$me" --arg from "$from" --argjson got "$delivered" '
        reduce (inputs | fromjson?) as $e (
          {cands: []};
          if ($e.crew_id == $crew and $e.kind == "msg" and $e.to == $me and ($from == "" or $e.from == $from))
          then .cands += [$e]
          else . end
        )
        | .cands
        | map(select(.ts > ($got[.from] // 0)))
        | if length == 0 then empty
          else (.[-1].from) as $s
          | map(select(.from == $s)) | sort_by(.ts)[]
          end
      ' "$log" 2>/dev/null || true)
      [ -n "$ans" ] && {
        _await_record "$crew" "$me" "$ans"
        printf '%s\n' "$ans"
        exit 0
      }
    fi
    now=$(_clock_now_ms)
    [ "$now" -ge "$deadline" ] && {
      echo "crew: await ended after $(((now - start) / 1000))s — no reply to $me${from:+ from $from} yet" >&2
      exit 0
    }
    _clock_sleep "$interval"
  done
  ;;
register | deregister)
  # Per-crew registration (was an exclusive per-repo role lock). N crews may
  # share a repo: each is identified by its crew_id, so there is no
  # cross-crew contention. The crew dir records the dispatcher's long-lived PID
  # (default: nearest non-shell ancestor), recorded for a future stale-cleanup
  # command (nothing reclaims automatically today), and holds that crew's watch
  # cursor + watch lock. Crew ids are unique by construction (timestamp-pid), so
  # re-registering the same pid is idempotent (re-mkdir -p, pid rewritten). The
  # id resolves from the cwd worktree's WORKER_TASK.md before $CREW_ID, so a
  # caller in a worker worktree or a nested launcher can name a live
  # dispatcher's crew: register refuses to swap a live pid, and deregister
  # refuses unless the recorded pid is dead or is the caller's own parent or
  # owner (#432). The residual gap: a subagent or child that shares the
  # dispatcher's own pid as its parent can still deregister it.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id — run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one" >&2
    exit 1
  }
  # deregister rm -rf's "$dir/crews/$crew", so an unvalidated `..` removes the bus.
  case "$crew" in
  *[!A-Za-z0-9._-]* | -* | . | ..)
    echo "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'" >&2
    exit 1
    ;;
  esac
  cdir="$dir/crews/$crew"
  epid=$(cat "$cdir/pid" 2>/dev/null || true)
  if [ "$sub" = register ]; then
    pid="${1:-$(_owner_pid)}"
    if [ "$epid" != "$pid" ] && _recorded_pid_live "$epid" "$cdir/pid"; then
      _pidfile_log register refused "$crew" "$epid" "$pid"
      echo "crew: crew '$crew' still has a live dispatcher (pid $epid) — 'crew new' starts your own crew; if that pid is a stale reuse, recover with 'crew adopt --force $crew'" >&2
      exit 1
    fi
    mkdir -p "$cdir"
    printf '%s\n' "$pid" >"$cdir/pid"
    _pidfile_log register ok "$crew" "$epid" "$pid"
    # The pane, not just the pid: a worker reattaching to a live dispatcher has
    # to retarget its `dispatcher_pane:` ping, and the pid alone cannot name a
    # pane. Absent outside tmux, which readers must tolerate.
    if [ -n "${TMUX_PANE:-}" ]; then
      printf '%s\n' "$TMUX_PANE" >"$cdir/pane"
    fi
  else
    # Exit 0 on refusal: this is cleanup, and dispatcher.sh runs it from its exit path.
    if _recorded_pid_live "$epid" "$cdir/pid" && [ "$epid" != "$PPID" ] && [ "$epid" != "$(_owner_pid)" ]; then
      _pidfile_log deregister refused "$crew" "$epid"
      echo "crew: crew '$crew' still has a live dispatcher (pid $epid) that is not this caller's parent or owner — left in place" >&2
      exit 0
    fi
    if [ -d "$cdir" ]; then _pidfile_log deregister removed "$crew" "$epid"; fi
    rm -rf "$cdir"
  fi
  ;;
crews)
  # Discovery primitive (#29): union crews/*/ (a crew dir — not necessarily
  # registered, since `watch` also mkdir -p's one) with distinct crew_id in
  # events.jsonl (a crew that posted, possibly via --crew-id alone and never
  # registered). Neither source alone is complete.
  printf 'crew_id\tlast_event_s\tfirst_event_s\tworkers\tpid\talive\n'
  ids=""
  if [ -d "$dir/crews" ]; then
    for d in "$dir"/crews/*/; do
      [ -d "$d" ] || continue
      ids="$ids
$(basename "$d")"
    done
  fi
  if [ -f "$log" ]; then
    # Best-effort, like every other read of this log: a hard kill mid-append
    # leaves a torn trailing line, and that is exactly the crash this command
    # exists to recover from — a jq parse error must not cost the well-formed
    # crews above it.
    ids="$ids
$(jq -r 'select(.crew_id != null and .crew_id != "") | .crew_id' "$log" 2>/dev/null || true)"
  fi
  ids=$(printf '%s\n' "$ids" | sed '/^$/d' | sort -u)
  [ -n "$ids" ] || exit 0
  # Per-id metadata (pid + liveness) needs `kill -0`, which jq cannot do, so
  # it is gathered in bash and merged with the log-derived stats in one
  # final jq pass — that pass also owns the newest-first sort.
  meta='[]'
  while IFS= read -r cid; do
    pid=$(cat "$dir/crews/$cid/pid" 2>/dev/null || true)
    alive=null
    if [ -n "$pid" ]; then
      alive=false
      # `kill -0 0` signals the caller's own process group and `kill -0 -1`
      # broadcasts, so both all but always succeed — a non-positive pid would
      # report a dead crew as alive. Only a positive integer is a liveness probe.
      case "$pid" in
      *[!0-9]* | 0) ;;
      *) if _recorded_pid_live "$pid" "$dir/crews/$cid/pid"; then alive=true; fi ;;
      esac
    fi
    meta=$(printf '%s' "$meta" | jq -c --arg id "$cid" --arg pid "$pid" --argjson alive "$alive" \
      '. + [{id:$id, pid:(if $pid=="" then null else $pid end), alive:$alive}]')
  done <<<"$ids"
  stats='{}'
  if [ -f "$log" ]; then
    # Same torn-line tolerance as the id scan: lose the age/worker columns
    # rather than the table.
    stats=$(jq -c '
        def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
        map(select(.crew_id != null and .crew_id != ""))
        | group_by(.crew_id)
        | map({key: .[0].crew_id,
               value: {last: (map(.ts) | max), first: (map(.ts) | min),
                       workers: (map(select(.kind=="status" and ((.from // "") | startswith("worker:"))))
                                 | map(.from | wid_branch) | unique | length)}})
        | from_entries' -s "$log" 2>/dev/null || echo '{}')
  fi
  printf '%s' "$meta" | jq -r --argjson stats "$stats" '
    (now*1000) as $now
    | (map(select($stats[.id] != null)) | sort_by(-$stats[.id].last)) as $with
    | (map(select($stats[.id] == null))) as $without
    | ($with + $without)[]
    | ($stats[.id]) as $s
    | [ .id,
        (if $s == null then "—" else (($now - $s.last)/1000 | floor | tostring) end),
        (if $s == null then "—" else (($now - $s.first)/1000 | floor | tostring) end),
        ($s.workers // 0 | tostring),
        (.pid // "—"),
        (if .alive == null then "—" elif .alive then "yes" else "no" end)
      ] | @tsv'
  ;;
adopt)
  # adopt [--force] <id> [pid] — re-attach to an on-disk crew after a
  # restart lost CREW_ID (#29). --force is stripped from anywhere in the args
  # before positionals are read, so `crew adopt <id> --force` cannot leave the
  # literal string "--force" as the pid.
  force=""
  args=()
  for a in "$@"; do
    if [ "$a" = --force ]; then
      force=1
    else
      args+=("$a")
    fi
  done
  set -- "${args[@]}"
  id="${1:-}"
  [ -n "$id" ] || {
    echo "crew: adopt [--force] <id> [pid]" >&2
    exit 1
  }
  # The id is caller-supplied (`dispatch --crew-id`) and lands unsanitised in the
  # shared events.jsonl, which is this command's own "is it known" source — so it
  # is validated before it becomes a path. `/` or `..` would otherwise mkdir and
  # write a pid file outside the bus dir, and a leading `-` would be read as a
  # flag by anything that later interpolates it.
  case "$id" in
  *[!A-Za-z0-9._-]* | -* | . | ..)
    echo "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'" >&2
    exit 1
    ;;
  esac
  pid="${2:-$(_owner_pid)}"
  cdir="$dir/crews/$id"
  known=""
  [ -d "$cdir" ] && known=1
  if [ -z "$known" ] && [ -f "$log" ]; then
    hit=$(jq -r --arg id "$id" 'select(.crew_id == $id) | .crew_id' "$log" 2>/dev/null | head -1 || true)
    [ -n "$hit" ] && known=1
  fi
  [ -n "$known" ] || {
    echo "crew: no crew '$id' in this repo — run 'crew crews' to list them, or 'crew new' to start one" >&2
    exit 1
  }
  # Liveness sits OUTSIDE the --force guard because the claim release below keys
  # on it too: --force over a genuinely live dispatcher must release nothing.
  epid=$(cat "$cdir/pid" 2>/dev/null || true)
  live=""
  if _recorded_pid_live "$epid" "$cdir/pid"; then live=1; fi
  if [ -z "$force" ]; then
    # A live pid among our own ancestors is *this* session's crew, so re-adopting
    # is idempotent — the recovery path has to survive being run twice. It is a
    # heuristic, not proof: `kill -0` only says some process holds that number
    # now, so a stale pid recycled onto a shared ancestor reads as ours. Benign
    # in the only case it can occur — a recycled pid means the original
    # dispatcher is dead, which is exactly when adopting is right — and two live
    # dispatchers on one crew is independently refused by `watch`'s per-crew
    # lock.
    if [ -n "$live" ] && ! _is_ancestor_pid "$epid"; then
      _pidfile_log adopt refused "$id" "$epid" "$pid"
      echo "crew: crew '$id' still has a live dispatcher — 'crew new' starts your own; '--force' overrides if that process is a stale pid reuse" >&2
      exit 1
    fi
  fi
  mkdir -p "$cdir"
  printf '%s\n' "$pid" >"$cdir/pid"
  outcome=ok
  if [ -n "$force" ] && [ -n "$live" ]; then outcome=forced; fi
  _pidfile_log adopt "$outcome" "$id" "$epid" "$pid"

  # Release the claims this crew recorded taking (#73): a dead or absent pid is
  # exactly the state that leaves a `dispatched` label with nobody left to
  # release it. A `claim-issue` row proves the crew once TOOK a claim, never that
  # it still holds one — neither reap nor adopt writes a counter-record — so the
  # newest claim per issue across ALL crews wins, and only then is it kept when
  # it is ours; otherwise adopting a long-dead crew strips the claim a later crew
  # is working right now. `crew register` writes no bus row, so an absent log is
  # the ordinary case, not an anomaly.
  #
  # Each row is issue, our branch, then every OTHER branch ever claimed for that
  # issue by any crew. Candidacy is ours, but the gates below need the whole
  # issue: two crews may hold two branches for one issue, and checking only ours
  # would release the label out from under the other. `group_by` compares raw
  # JSON, hence the `tostring` key — a numeric 73 and a string "73" would
  # otherwise split into two groups that `gh issue edit` treats as one.
  claims=""
  pr_heads=""
  if [ -z "$live" ] && [ -f "$log" ]; then
    claims=$(jq -s -r --arg id "$id" '
      map(select(.kind=="claim-issue" and .issue != null and (.branch // "") != ""))
      | group_by(.issue|tostring)
      | map(
          max_by(.ts) as $mine
          | select($mine.crew_id == $id)
          | [($mine.issue|tostring), $mine.branch] + (map(.branch) | unique - [$mine.branch])
        )
      | .[] | @tsv' "$log" 2>/dev/null || true)
  fi
  # Every gh call below hangs off this: with nothing to release, adopt stays offline.
  if [ -n "$claims" ]; then
    # Explicit --limit — the default 30 would read a busy repo's PR-bearing branch
    # as PR-less and release a claim its open PR still holds.
    if ! pr_heads=$(gh pr list --state open --limit 500 --json headRefName --jq '.[].headRefName' 2>&1); then
      echo "crew adopt: could not list open PRs ($pr_heads) — leaving the recorded claims in place" >&2
      claims=""
    fi
  fi
  # Messages go to stderr without exception: stdout is a machine channel
  # (`CREW_ID=$(crew adopt <id> $PPID)`) and stays exactly the bare id.
  #
  # The claims snapshot is read once, above, and never revalidated: a dispatch
  # can record a newer claim while these `gh issue edit` calls run, so a
  # just-started dispatch can still lose its label. Accepted, for the reason the
  # claim gate itself states — `gh` offers no compare-and-swap, so every claim
  # path here narrows the race rather than closing it.
  while IFS=$'\t' read -r -a crow; do
    cissue="${crow[0]:-}"
    [ -n "$cissue" ] || continue
    # The bus log is caller-writable, so the issue is validated here, at the read
    # site, before it reaches `gh` — same reason the crew id is validated above
    # rather than trusted from its producer.
    case "$cissue" in
    *[!0-9]*)
      echo "crew adopt: skipping a claim row whose issue is not a number ($cissue)" >&2
      continue
      ;;
    esac
    cbranch="${crow[1]}"
    held=""
    for cb in "${crow[@]:1}"; do
      # _occupants takes a path, not a branch.
      cwt=$(git worktree list --porcelain |
        awk -v b="refs/heads/$cb" '/^worktree /{p=$2} $0=="branch "b{print p}')
      if [ -n "$cwt" ] && [ -d "$cwt" ] && [ "$(_occupants "$cwt")" != '[]' ]; then
        echo "crew adopt: keeping #$cissue — a worker still occupies $cb" >&2
        held=1
        break
      fi
      # An open PR means the work landed far enough to still hold the issue, and
      # `crew reap` releases it when that PR merges.
      if printf '%s\n' "$pr_heads" | grep -qxF "$cb"; then
        echo "crew adopt: keeping #$cissue — $cb heads an open PR" >&2
        held=1
        break
      fi
    done
    [ -z "$held" ] || continue
    # Best-effort: adopt is a recovery command and must not fail because gh is
    # unavailable.
    if gh issue edit "$cissue" --remove-label dispatched >/dev/null 2>&1; then
      echo "crew adopt: released the dispatched label on #$cissue ($cbranch)" >&2
    else
      echo "crew adopt: could not remove the dispatched label from #$cissue ($cbranch)" >&2
    fi
  done <<EOF
$claims
EOF
  printf '%s\n' "$id"
  ;;
watch)
  # watch [--since TS] [--states a,b,c] [--timeout S] [--interval S] — block until
  # any worker event qualifies (a status whose state is in --states, or a msg to
  # the dispatcher), print {"cursor":TS,"events":[…]} and exit 0. An expired park
  # also exits 0 (empty stdout is the marker) so it isn't reported as a failed
  # background command. Default --timeout is a finite long park (3300s / 55min):
  # an indefinite watch is rejected, because a reaped indefinite watch is
  # undetectable. When --since is omitted the cursor is self-seeded from
  # the crew's cursor file (see below) so a stale caller cursor can't re-deliver.
  # Zero-token held poll, like `await`. Strict `>` matches `await` (same-ms edge).
  mkdir -p "$dir"
  since=0
  since_explicit=0
  states="blocked,pr_open,done,failed,exited"
  timeout=3300
  interval=2
  wcrew=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --since)
      [ -n "${2:-}" ] || {
        echo "crew: --since needs a value" >&2
        exit 1
      }
      since="$2"
      since_explicit=1
      shift 2
      ;;
    --states)
      [ -n "${2:-}" ] || {
        echo "crew: --states needs a value" >&2
        exit 1
      }
      states="$2"
      shift 2
      ;;
    --timeout)
      [ -n "${2:-}" ] || {
        echo "crew: --timeout needs a value" >&2
        exit 1
      }
      timeout="$2"
      shift 2
      ;;
    --interval)
      [ -n "${2:-}" ] || {
        echo "crew: --interval needs a value" >&2
        exit 1
      }
      interval="$2"
      shift 2
      ;;
    --crew)
      [ -n "${2:-}" ] || {
        echo "crew: --crew needs a value" >&2
        exit 1
      }
      wcrew="$2"
      shift 2
      ;;
    *)
      echo "crew: watch: unknown arg '$1'" >&2
      exit 1
      ;;
    esac
  done
  case "$since" in '' | *[!0-9]*)
    echo "crew: --since must be an integer ms timestamp" >&2
    exit 1
    ;;
  esac
  case "$timeout" in '' | *[!0-9]*)
    echo "crew: --timeout must be a positive integer number of seconds" >&2
    exit 1
    ;;
  esac
  [ "$timeout" -gt 0 ] || {
    echo "crew: --timeout must be > 0 (indefinite watch unsupported: a reaped watch would be undetectable)" >&2
    exit 1
  }
  statesjson=$(printf '%s' "$states" | jq -Rc 'split(",") | map(select(length>0))')
  [ "$statesjson" = "[]" ] && {
    echo "crew: --states must be non-empty" >&2
    exit 1
  }
  # --crew names the crew explicitly so a Monitor-armed `crew stream` (whose
  # command string carries no ambient CREW_ID) can pass it down; when omitted,
  # resolution falls back to _crew_id exactly as before the flag existed.
  crew="${wcrew:-$(_crew_id)}"
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  # Same guard `adopt` applies to its caller-supplied id: `--crew` here is just
  # as caller-supplied, and unvalidated would let `/` or `..` mkdir and write a
  # pid file outside the bus dir.
  case "$crew" in
  *[!A-Za-z0-9._-]* | -* | . | ..)
    echo "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'" >&2
    exit 1
    ;;
  esac
  cdir="$dir/crews/$crew"
  mkdir -p "$cdir"
  cursor_file="$cdir/cursor"
  if [ "$since_explicit" = 0 ]; then
    seed=$(cat "$cursor_file" 2>/dev/null || true)
    case "$seed" in '' | *[!0-9]*) seed=0 ;; esac
    since="$seed"
  fi
  wlock="$cdir/watch.lock.d"
  _lock_acquire "$wlock" "$$" || {
    echo "crew: another watch is already running for this crew ($crew)" >&2
    exit 1
  }
  trap '_lock_release "$wlock"' EXIT
  me="dispatcher:$crew"
  start=$(jq -nc 'now*1000|floor')
  deadline=$((start + timeout * 1000))
  while :; do
    if [ -f "$log" ]; then
      # An `exited` is the SessionEnd backstop, not something a worker asserts.
      # It wakes the park only when it is the session's FIRST terminal state —
      # the engine died mid-run — so the gate below reuses `roster`'s prev-state
      # rule: the newest status for the same session that was not itself
      # `exited`. An `exited` posted after the session's own done/failed/pr_open
      # is the ordinary backstop and must stay silent, or every finished worker
      # double-wakes the dispatcher.
      batch=$(jq -c -s --arg crew "$crew" --arg me "$me" --argjson since "$since" --argjson states "$statesjson" '
          def prev_state($all; $e):
            ([ $all[]
               | select(.crew_id==$e.crew_id and .kind=="status" and .from==$e.from and .ts < $e.ts)
               | select(.body.state != "exited")
               | {ts: .ts, state: .body.state} ]
             | sort_by(.ts) | last | .state);
          def detail_text: if type == "string" then . else "" end;
          def norm: gsub(" *\\(cycle [0-9]+ of [0-9]+\\)"; "") | gsub("awaited [0-9]+s"; "awaited Ns");
          def prev_status($all; $e):
            ([ $all[]
               | select(.crew_id==$e.crew_id and .kind=="status" and .from==$e.from and .ts < $e.ts) ]
             | sort_by(.ts) | last);
          . as $all
          | map(. as $e
                | select($e.crew_id==$crew and $e.ts>$since)
                | select(
                    ( $e.kind=="status"
                      and ($e.body.state as $s | $states | index($s))
                      and ( if $e.body.state == "exited"
                            then (prev_state($all; $e) as $p | ($p == "working" or $p == "blocked"))
                            else true end )
                      and (prev_status($all; $e) as $prev
                           | ( ( $e.body.state == "blocked"
                                 and $prev != null
                                 and $prev.body.state == "blocked"
                                 and ((($e.body.detail | detail_text) | norm)
                                      == (($prev.body.detail | detail_text) | norm)) )
                               or ( $e.body.source == "watchdog"
                                    and $e.body.state == "working"
                                    and (($e.body.detail | detail_text) | test(" cleared$")) ) )
                           | not) )
                    or ( $e.kind=="msg" and ($e.to==$me or $e.to=="*") ) ))
          | sort_by(.ts)
          | select(length>0)
          | {cursor:(.[-1].ts), events:.}' "$log" 2>/dev/null || true)
      [ -n "$batch" ] && {
        printf '%s\n' "$batch"
        last=$(printf '%s' "$batch" | jq -r '.cursor')
        tmp=$(mktemp "$dir/.cursor.XXXXXX")
        printf '%s\n' "$last" >"$tmp"
        mv -f "$tmp" "$cursor_file"
        exit 0
      }
    fi
    [ "$(jq -nc 'now*1000|floor')" -ge "$deadline" ] && {
      echo "crew: watch park ended after ${timeout}s — no new events (cursor $since)" >&2
      exit 0
    }
    sleep "$interval"
  done
  ;;
stream)
  # stream [--crew ID] [--states a,b,c] [--park S] [--heartbeat S]
  #        [--coalesce S] [--retry S] [--interval S] [--force] [--reap-every S]
  # stream --status [--crew ID]
  # Wraps `watch` in a long-lived process, so a streaming lane gets pushed
  # batches instead of re-arming a one-shot park every turn. `--crew` is
  # required in spirit (not syntax): the command string a streaming lane arms
  # carries no ambient CREW_ID.
  #
  # Usage/crew-resolution failures exit 64, not 1 — otherwise a `--status`
  # call that simply couldn't find its crew reads as the `dead` state it
  # never measured.
  screw=""
  states="blocked,pr_open,done,failed,exited"
  park=300
  heartbeat=3300
  coalesce=5
  retry=30
  interval=2
  force=""
  statusmode=""
  reap_every=900
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew)
      [ -n "${2:-}" ] || {
        echo "crew: --crew needs a value" >&2
        exit 64
      }
      screw="$2"
      shift 2
      ;;
    --states)
      [ -n "${2:-}" ] || {
        echo "crew: --states needs a value" >&2
        exit 64
      }
      states="$2"
      shift 2
      ;;
    --park)
      [ -n "${2:-}" ] || {
        echo "crew: --park needs a value" >&2
        exit 64
      }
      park="$2"
      shift 2
      ;;
    --heartbeat)
      [ -n "${2:-}" ] || {
        echo "crew: --heartbeat needs a value" >&2
        exit 64
      }
      heartbeat="$2"
      shift 2
      ;;
    --coalesce)
      [ -n "${2:-}" ] || {
        echo "crew: --coalesce needs a value" >&2
        exit 64
      }
      coalesce="$2"
      shift 2
      ;;
    --retry)
      [ -n "${2:-}" ] || {
        echo "crew: --retry needs a value" >&2
        exit 64
      }
      retry="$2"
      shift 2
      ;;
    --interval)
      [ -n "${2:-}" ] || {
        echo "crew: --interval needs a value" >&2
        exit 64
      }
      interval="$2"
      shift 2
      ;;
    --force)
      force=1
      shift
      ;;
    --status)
      statusmode=1
      shift
      ;;
    --reap-every)
      [ -n "${2:-}" ] || {
        echo "crew: --reap-every needs a value" >&2
        exit 64
      }
      reap_every="$2"
      shift 2
      ;;
    *)
      echo "crew: stream: unknown arg '$1'" >&2
      exit 64
      ;;
    esac
  done
  _stream_posint() {
    case "$2" in '' | *[!0-9]*)
      echo "crew: --$1 must be a positive integer number of seconds" >&2
      exit 64
      ;;
    esac
    [ "$2" -gt 0 ] || {
      echo "crew: --$1 must be a positive integer number of seconds" >&2
      exit 64
    }
  }
  _stream_posint park "$park"
  _stream_posint heartbeat "$heartbeat"
  _stream_posint coalesce "$coalesce"
  _stream_posint retry "$retry"
  _stream_posint interval "$interval"
  # Non-negative, not positive like the others above: 0 is how the cadence
  # reap is disabled, not an error.
  case "$reap_every" in '' | *[!0-9]*)
    echo "crew: --reap-every must be a non-negative integer number of seconds" >&2
    exit 64
    ;;
  esac
  statesjson=$(printf '%s' "$states" | jq -Rc 'split(",") | map(select(length>0))')
  [ "$statesjson" = "[]" ] && {
    echo "crew: --states must be non-empty" >&2
    exit 64
  }
  crew="${screw:-$(_crew_id)}"
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 64
  }
  # Same guard `adopt` applies to its caller-supplied id: `--crew` here is just
  # as caller-supplied, and unvalidated would let `/` or `..` mkdir and write a
  # pid file outside the bus dir.
  case "$crew" in
  *[!A-Za-z0-9._-]* | -* | . | ..)
    echo "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'" >&2
    exit 64
    ;;
  esac
  cdir="$dir/crews/$crew"
  mkdir -p "$cdir"

  if [ -n "$statusmode" ]; then
    # A live pid is not evidence notifications are flowing (a rate-limit
    # auto-stop, a closed stdout, pid reuse all leave a pid that looks
    # alive), so `alive` also needs a tick younger than 2×park+60 — computed
    # from the park THE TICK ITSELF recorded, not this call's, so a stale
    # `--park` on the status call can't shift the answer. A lock with no
    # readable tick counts as `stale`, the safe direction: the tick is
    # written immediately on acquiring the lock.
    ldir="$cdir/stream.lock.d"
    lockpid=""
    [ -f "$ldir/pid" ] && lockpid=$(cat "$ldir/pid" 2>/dev/null || true)
    case "$lockpid" in '' | *[!0-9]* | 0) lockpid="" ;; esac
    live=""
    if [ -n "$lockpid" ] && _pid_alive "$lockpid"; then
      live=1
    fi
    if [ -z "$live" ]; then
      st=dead
      rc=2
      age_json=null
    else
      tick=$(cat "$cdir/stream.tick" 2>/dev/null || true)
      tickts=""
      tickpark=""
      if [ -n "$tick" ]; then
        tickts=$(printf '%s' "$tick" | jq -r '.ts // empty' 2>/dev/null || true)
        tickpark=$(printf '%s' "$tick" | jq -r '.park // empty' 2>/dev/null || true)
      fi
      case "$tickts" in '' | *[!0-9]*) tickts="" ;; esac
      case "$tickpark" in '' | *[!0-9]*) tickpark="" ;; esac
      if [ -z "$tickts" ] || [ -z "$tickpark" ]; then
        st=stale
        age_json=null
      else
        now_ms=$(jq -nc 'now*1000|floor')
        age_s=$(((now_ms - tickts) / 1000))
        if [ "$age_s" -lt "$((2 * tickpark + 60))" ]; then
          st=alive
        else
          st=stale
        fi
        age_json="$age_s"
      fi
      case "$st" in alive) rc=0 ;; *) rc=1 ;; esac
    fi
    pid_json="${lockpid:-null}"
    jq -nc --arg crew "$crew" --arg state "$st" --argjson pid "$pid_json" --argjson age "$age_json" \
      '{stream:"status", state:$state, crew:$crew, pid:$pid, age_s:$age}'
    exit "$rc"
  fi

  # Same `_lock_acquire` INV-1 relies on for `watch.lock.d`: a second stream
  # for one crew is refused, enforced by the tool instead of maintained by
  # hand. Acquired BEFORE any trap is armed — `_stream_cleanup` releases the
  # lock unconditionally, so a trap installed first would make the refusal
  # path below `rm -rf` the INCUMBENT stream's lock, after which a third
  # `crew stream` starts and two run for one crew.
  lockd="$cdir/stream.lock.d"
  if ! _lock_acquire "$lockd" "$$"; then
    holder=$(cat "$lockd/pid" 2>/dev/null || true)
    [ -n "$force" ] || {
      echo "crew: another stream is already running for this crew ($crew) — pid ${holder:-unknown}; use --force to reclaim a stale one" >&2
      exit 1
    }
    # Same sanitisation `--status` above applies to a lock pid before trusting
    # it: 0 and -1 both read as "live" to `kill -0`, but as a signal target 0
    # hits our whole process group and a negative pid hits every process we can
    # signal — neither is a single stale holder.
    case "$holder" in '' | *[!0-9]* | 0)
      echo "crew: --force found no valid holder pid for crew ($crew) stream lock (got '${holder:-empty}') — refusing to signal" >&2
      exit 1
      ;;
    esac
    # Bounded, not indefinite: an unbounded wait would hang forever against a
    # holder that ignores TERM. 50×0.1s gives the recorded pid 5s to clear.
    kill -TERM "$holder" 2>/dev/null || true
    cleared=""
    tries=0
    while [ "$tries" -lt 50 ]; do
      # Bare `kill -0`, paired with the `kill -TERM` above: confirms signal
      # delivery, not general liveness.
      kill -0 "$holder" 2>/dev/null || {
        cleared=1
        break
      }
      sleep 0.1
      tries=$((tries + 1))
    done
    [ -n "$cleared" ] || {
      echo "crew: --force sent TERM but pid $holder for crew ($crew) did not clear" >&2
      exit 1
    }
    _lock_acquire "$lockd" "$$" || {
      echo "crew: another stream is already running for this crew ($crew)" >&2
      exit 1
    }
  fi

  # Pinned, not `mktemp`: these are per-crew and already mutually excluded by
  # stream.lock.d, so two streams can't collide on them.
  outf="$cdir/stream.out"
  errf="$cdir/stream.err"
  holderrf="$cdir/stream.hold.err"
  reapoutf="$cdir/stream.reap.out"
  reaperrf="$cdir/stream.reap.err"
  # Each background reap writes its own `$reapoutf.<pid>`/`$reaperrf.<pid>`
  # pair, so a child outliving its stream cannot clobber the next one. Sweep
  # the base names and any orphan pair a previous child left behind; a child
  # still running can recreate its own pair.
  rm -f "$reapoutf" "$reaperrf" "$reapoutf".* "$reaperrf".*
  # Initialized before the trap is armed, so a signal landing before the
  # first iteration can't abort the handler on an unbound variable.
  child=""
  pending=""
  quiet=0
  last_err_key=""
  last_err_ts=0
  last_hold_key=""
  last_hold_ts=0
  last_holderr_key=""
  last_holderr_ts=0
  last_reaperr_key=""
  last_reaperr_ts=0
  # last_reap starts at stream start, not epoch 0, so the first cadence reap
  # is $reap_every out rather than firing on the very first iteration.
  reap_child=""
  reap_child_out=""
  reap_child_err=""
  last_reap=$(date +%s)

  # The handler must disarm itself first — its own closing `exit` would
  # otherwise re-enter it and re-print `$pending` — and must end by exiting
  # the process outright: a TERM/INT/HUP handler does not stop the shell, so
  # if it merely returned, `wait "$child"` (interrupted with rc>128) would
  # hand control straight back to the loop below, which would read that as
  # an ordinary inner-watch failure and launch a fresh one against temp
  # files already removed and a lock already released.
  _stream_cleanup() {
    trap - EXIT INT TERM HUP
    # TERM (not KILL): the inner watch's own EXIT trap must run to release
    # watch.lock.d. It is normally parked in `sleep "$interval"`, and bash
    # runs no trapped handler until that sleep returns. Hence the wait,
    # bounded by `--interval` rather than `--park`: draining without it reads
    # $outf empty or partial for a batch whose cursor has already advanced.
    if [ -n "$child" ]; then
      kill -TERM "$child" 2>/dev/null || true
      wait "$child" 2>/dev/null || true
    fi
    # Drain before delete, and reap (above) before drain: `pending` is what
    # this process already read out of $outf and was about to print; $outf
    # itself is what an orphaned-then-reaped child wrote but this process
    # never got to read. Never both — they are the same batch, and a
    # duplicate the reader can skip beats a silent drop it cannot.
    # `|| true` on every write below: a reader that hung up makes the write
    # fail, and under `set -e` that would abort this handler before it reaches
    # `rm -f`/`_lock_release`, leaving the lock held by a dead pid.
    if [ -n "$pending" ]; then
      printf '%s\n' "$pending" || true
    elif [ -s "$outf" ]; then
      cat "$outf" || true
    fi
    if [ -n "$reap_child_out" ] && [ -s "$reap_child_out" ]; then
      _stream_reap_print "$reap_child_out" || true
    fi
    rm -f "$outf" "$errf" "$holderrf" "$reapoutf" "$reaperrf" "$reapoutf".* "$reaperrf".*
    _lock_release "$lockd"
    exit 0
  }
  trap _stream_cleanup EXIT INT TERM HUP

  # -r, not bare -p: `jobs` here runs in a pipeline subshell, which never
  # clears the parent's finished-job entries, so `jobs -p` would list an
  # exited child forever and no reap would ever fire (or flush) again.
  _stream_reap_running() {
    [ -n "$reap_child" ] && jobs -rp | grep -qx "$reap_child"
  }

  # _stream_reap_flush — print and drop the tracked child's $reap_child_out
  # once it has exited (a running child may still be writing). Another
  # stream's outlived child writes its own pair and is never read here.
  _stream_reap_flush() {
    [ -n "$reap_child_out" ] || return 0
    ! _stream_reap_running || return 0
    _stream_reap_print "$reap_child_out"
    rm -f "$reap_child_out" "$reap_child_err"
  }

  # _stream_reap_print <file> — print and empty one reap output file. Reap
  # errors share the suppression scheme of the inner-watch errors below: one
  # line per key, re-emitted after --heartbeat. Every write is `|| true`:
  # _stream_cleanup calls this too, and must not abort before releasing the lock.
  _stream_reap_print() {
    local rf="$1" rline rkey r_ts
    [ -s "$rf" ] || return 0
    while IFS= read -r rline; do
      [ -n "$rline" ] || continue
      if [ "$(jq -r '.stream' <<<"$rline" 2>/dev/null || true)" = error ]; then
        rkey=$(jq -r '"\(.rc):\(.detail)"' <<<"$rline" 2>/dev/null | sed -E 's/[0-9]+/N/g' || true)
        r_ts=$(jq -nc 'now*1000|floor')
        if [ "$rkey" = "$last_reaperr_key" ] && [ "$((r_ts - last_reaperr_ts))" -lt "$((heartbeat * 1000))" ]; then
          continue
        fi
        last_reaperr_key="$rkey"
        last_reaperr_ts="$r_ts"
      fi
      printf '%s\n' "$rline" || true
    done <"$rf"
    : >"$rf" || true
  }

  # _stream_reap — a background `crew reap`, never awaited or killed by
  # _stream_cleanup: an interrupted removal strands a worktree. It ignores
  # HUP only; a signal to the whole process group still reaches it. Output goes
  # to a per-child `$reapoutf.$BASHPID` (the child's own pid, not the stream's
  # `$$`), printed once this stream's tracked child is gone, so a child that
  # outlives the stream cannot clobber the next one. --no-wait: a reap already
  # holding the lock covers this one.
  _stream_reap() {
    [ "$reap_every" -gt 0 ] || return 0
    ! _stream_reap_running || return 0
    _stream_reap_flush
    last_reap=$(date +%s)
    (
      trap '' HUP
      rc=0
      child_out="$reapoutf.$BASHPID"
      child_err="$reaperrf.$BASHPID"
      out=$(bash -euo pipefail "$0" reap --quiet --no-wait 2>"$child_err" </dev/null) || rc=$?
      r_ts=$(jq -nc 'now*1000|floor')
      {
        [ -z "$out" ] || jq -nc --arg crew "$crew" --arg out "$out" --argjson ts "$r_ts" \
          '{stream:"reap", crew:$crew, lines:($out | split("\n") | map(select(length>0) | sub("^crew reap: ";""))), ts:$ts}'
        [ "$rc" -eq 0 ] || jq -nc --arg crew "$crew" --argjson rc "$rc" --arg detail "$(head -n1 "$child_err" 2>/dev/null || true)" --argjson ts "$r_ts" \
          '{stream:"error", crew:$crew, rc:$rc, detail:("reap: " + $detail), ts:$ts}'
      } >"$child_out" || true
    ) </dev/null &
    reap_child=$!
    # $BASHPID inside the subshell is the subshell's own pid, which is $! here,
    # so these name exactly the pair the child writes.
    reap_child_out="$reapoutf.$reap_child"
    reap_child_err="$reaperrf.$reap_child"
  }

  # _stream_batch_wants_reap <batch-json> — true when the just-printed batch
  # (one JSON object per line) carries a terminal status, or a pr-watch msg
  # reporting the PR itself landed.
  _stream_batch_wants_reap() {
    jq -e -s 'any(.[]; .events[]? as $e |
        if $e.kind == "status" then
          (["done","failed","exited"] | index($e.body.state)) != null
        elif $e.kind == "msg" and (($e.from // "") | startswith("pr-watch:")) then
          ($e.body | fromjson? // {}) as $b |
          ((($b.changed // []) | index("state")) != null)
            and ((["MERGED","CLOSED"] | index($b.state.state)) != null)
        else false end
      )' >/dev/null 2>&1 <<<"$1"
  }

  while :; do
    # Written at the top of every iteration — including the first, right
    # after the lock is acquired — because a live lock pid is not evidence
    # that notifications are flowing (see --status above).
    tick_ts=$(jq -nc 'now*1000|floor')
    jq -nc --argjson pid "$$" --argjson ts "$tick_ts" --argjson park "$park" \
      '{pid:$pid, ts:$ts, park:$park}' >"$cdir/stream.tick"
    _stream_reap_flush
    # Ahead of the inner watch, so a matured hold announces on the batch path
    # too, and at `--park` resolution rather than `--heartbeat` — which is
    # coarser than the longest wait a hold can legally carry.
    #
    # The rc is captured rather than discarded with `|| true`: `due` exits 1
    # for the ordinary "nothing matured", so a blanket `|| true` would fold a
    # genuine failure into that same silence — and this is the one branch
    # nothing else watches, since a hold exists precisely because no human is
    # looking. Anything but 0/1 is reported like an inner-watch failure below.
    hold_rc=0
    matured=$(bash -euo pipefail "$0" hold due --crew "$crew" --json 2>"$holderrf") || hold_rc=$?
    if [ "$hold_rc" -gt 1 ]; then
      heline=$(head -n1 "$holderrf" 2>/dev/null || true)
      hekey="$hold_rc:$(printf '%s' "$heline" | sed -E 's/[0-9]+/N/g')"
      he_ts=$(jq -nc 'now*1000|floor')
      if [ "$hekey" != "$last_holderr_key" ] || [ "$((he_ts - last_holderr_ts))" -ge "$((heartbeat * 1000))" ]; then
        jq -nc --arg crew "$crew" --argjson rc "$hold_rc" --arg detail "$heline" --argjson ts "$he_ts" \
          '{stream:"error", crew:$crew, rc:$rc, detail:("hold due: " + $detail), ts:$ts}' || true
        last_holderr_key="$hekey"
        last_holderr_ts="$he_ts"
      fi
      matured=""
    fi
    case "$matured" in
    '' | '[]')
      # Empty as well as `[]`: a failed shell-out above may print nothing at
      # all, and neither answer may reach `jq`. Cleared rather than kept, so a
      # hold released and later re-added announces again.
      last_hold_key=""
      ;;
    *)
      # Keyed on the matured set, not on the transition into it: releasing one
      # of several holds changes the key, so the rest re-announce on the next
      # iteration instead of stranding until the stream restarts. Suppression
      # is the error path's below, unchanged.
      hkey=$(printf '%s' "$matured" | jq -r '[.[].id] | sort | join(",")' 2>/dev/null || true)
      h_ts=$(jq -nc 'now*1000|floor')
      if [ "$hkey" != "$last_hold_key" ] || [ "$((h_ts - last_hold_ts))" -ge "$((heartbeat * 1000))" ]; then
        jq -nc --arg crew "$crew" --argjson holds "$matured" --argjson ts "$h_ts" \
          '{stream:"hold_due", crew:$crew, holds:$holds, ts:$ts}' || true
        last_hold_key="$hkey"
        last_hold_ts="$h_ts"
      fi
      ;;
    esac
    # Never --since: the per-crew cursor file self-seeds `watch`, exactly as
    # a bare `crew watch` would, so the cursor keeps advancing across
    # iterations without this process tracking it itself.
    bash -euo pipefail "$0" watch --crew "$crew" --timeout "$park" --states "$states" --interval "$interval" \
      >"$outf" 2>"$errf" &
    child=$!
    rc=0
    wait "$child" || rc=$?
    child=""
    if [ -s "$outf" ]; then
      # A qualifying batch. Re-entering `watch` immediately would turn N
      # events trickling in over N seconds into N notifications, so the
      # `--coalesce` sleep is what holds one turn to one batch.
      pending=$(cat "$outf")
      # A gone reader must not abort the loop mid-write (`set -e`): the batch
      # is still consumed and the cursor still advances, only the print fails.
      printf '%s\n' "$pending" || true
      _stream_reap_flush
      # React to the event we already have rather than waiting out the
      # cadence: a terminal status or a PR-merged msg is worth a reap now.
      _stream_batch_wants_reap "$pending" && _stream_reap
      pending=""
      : >"$outf"
      quiet=0
      sleep "$coalesce"
    elif [ "$rc" -eq 0 ]; then
      # Park expired with nothing to report.
      quiet=$((quiet + park))
      if [ "$quiet" -ge "$heartbeat" ]; then
        hb_ts=$(jq -nc 'now*1000|floor')
        jq -nc --arg crew "$crew" --argjson quiet_s "$quiet" --argjson ts "$hb_ts" \
          '{stream:"heartbeat", crew:$crew, quiet_s:$quiet_s, ts:$ts}' || true
        quiet=0
      fi
    else
      # The inner watch failed (never to its own stderr — that stays in
      # $errf, or it would put non-JSON next to the JSON event stream). The
      # suppression key normalises away the cursor value and timestamps
      # `watch` embeds in its error lines, so a stable underlying cause
      # prints once instead of once per --retry.
      eline=$(head -n1 "$errf" 2>/dev/null || true)
      ekey="$rc:$(printf '%s' "$eline" | sed -E 's/[0-9]+/N/g')"
      e_ts=$(jq -nc 'now*1000|floor')
      if [ "$ekey" != "$last_err_key" ] || [ "$((e_ts - last_err_ts))" -ge "$((heartbeat * 1000))" ]; then
        jq -nc --arg crew "$crew" --argjson rc "$rc" --arg detail "$eline" --argjson ts "$e_ts" \
          '{stream:"error", crew:$crew, rc:$rc, detail:$detail, ts:$ts}' || true
        last_err_key="$ekey"
        last_err_ts="$e_ts"
      fi
      sleep "$retry"
    fi
    # Cadence: every branch above may have consumed anywhere from --interval
    # to --park+--retry seconds, so the elapsed check belongs here rather
    # than pinned to one branch.
    now_s=$(date +%s)
    if [ "$reap_every" -gt 0 ] && [ "$((now_s - last_reap))" -ge "$reap_every" ]; then
      _stream_reap
    fi
  done
  ;;
sessions | roster)
  # Ported to Go (crew/, docs/crew-go-port.md). CREW_GO_BIN is the
  # raw-source override; builds bake @crewGoBin@.
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
roster-render)
  # roster-render --crew ID [--pane %N] [--no-open] [--once | --detach] [--interval S] [--quiet S]
  # Draws the crew's roster diagram (roster-<repo>-<time>-<hash>.d2) from the bus and
  # the crew's live role panes, and publishes it to the aeye carousel beside the
  # recorded dispatcher pane. One renderer per repo bus: a crew spanning several
  # repos gets one diagram per repo, <repo> naming the dir that owns the bus.
  # --detach records the pane, then starts the daemon detached and returns.
  # Usage errors exit 64, like `stream`.
  rr_crew=""
  rr_pane=""
  rr_no_open=""
  rr_once=""
  rr_detach=""
  rr_interval=2
  rr_quiet=1800
  while [ $# -gt 0 ]; do
    case "$1" in
    --crew | --pane | --interval | --quiet)
      [ -n "${2:-}" ] || {
        echo "crew: $1 needs a value" >&2
        exit 64
      }
      case "$1" in
      --crew) rr_crew="$2" ;;
      --pane) rr_pane="$2" ;;
      --interval) rr_interval="$2" ;;
      --quiet) rr_quiet="$2" ;;
      esac
      shift 2
      ;;
    --no-open)
      rr_no_open=1
      shift
      ;;
    --once)
      rr_once=1
      shift
      ;;
    --detach)
      rr_detach=1
      shift
      ;;
    *)
      echo "crew: roster-render: unknown arg '$1'" >&2
      exit 64
      ;;
    esac
  done
  case "$rr_interval" in '' | *[!0-9]*)
    echo "crew: --interval must be a positive integer number of seconds" >&2
    exit 64
    ;;
  esac
  [ "$rr_interval" -gt 0 ] || {
    echo "crew: --interval must be a positive integer number of seconds" >&2
    exit 64
  }
  case "$rr_quiet" in '' | *[!0-9]*)
    echo "crew: --quiet must be a non-negative integer number of seconds" >&2
    exit 64
    ;;
  esac
  [ -n "$rr_crew" ] || {
    echo "crew: roster-render: --crew is required" >&2
    exit 64
  }
  [ -z "$rr_once" ] || [ -z "$rr_detach" ] || {
    echo "crew: roster-render: --once and --detach are mutually exclusive" >&2
    exit 64
  }
  case "$rr_crew" in
  *[!A-Za-z0-9._-]* | -* | . | ..)
    echo "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'" >&2
    exit 64
    ;;
  esac
  cdir="$dir/crews/$rr_crew"
  mkdir -p "$cdir"
  # Resolved before the cd below: `_rr_model` re-execs this script, and a
  # relative $0 would no longer resolve from $common.
  _rr_self=$(readlink -f "$0")
  # `dispatch resume` starts this from inside a worker worktree, and every
  # rebuild re-execs `crew roster`, which resolves the bus from cwd: once that
  # worktree is reaped the renderer would die with `not in a git repo`.
  cd -- "$common" || exit 1
  # The pane record is rewritten before any lock, so a new dispatcher pane
  # retargets an already-running renderer. Anchor, don't discover: a --pane is
  # recorded only when its shell is an ancestor of this process, so a caller
  # can point the renderer at its own pane but never at another's.
  if [ -n "$rr_pane" ]; then
    rr_pane_pid=""
    if [[ $rr_pane =~ ^%[0-9]+$ ]]; then
      rr_pane_pid=$(tmux display-message -p -t "$rr_pane" '#{pane_pid}' 2>/dev/null || true)
    fi
    if [[ $rr_pane_pid =~ ^[0-9]+$ ]] && _is_ancestor_pid "$rr_pane_pid"; then
      _rr_put "$cdir/roster-render.pane" "$rr_pane" || true
    else
      echo "crew: roster-render: --pane '$rr_pane' is not this caller's pane — ignored" >&2
    fi
  fi
  # The daemon's own argv, shared by `--detach` and by the re-exec below so both
  # start the loop with the same already-validated arguments: `--pane` is a file
  # record, never an argument, and `--once`/`--detach` are not forwarded.
  rr_args=(--crew "$rr_crew" --interval "$rr_interval" --quiet "$rr_quiet")
  [ -z "$rr_no_open" ] || rr_args+=(--no-open)
  rr_lockd="$cdir/roster-render.lock.d"
  if [ -n "$rr_detach" ]; then
    # A nohup'd child of the caller can be reparented before it checks
    # ancestry, so the pane was validated above and the daemon reads the record.
    CREW_ID="$rr_crew" nohup bash -euo pipefail "$_rr_self" roster-render "${rr_args[@]}" </dev/null >/dev/null 2>&1 &
    exit 0
  fi
  if [ -n "$rr_once" ]; then
    _rr_pass "$rr_crew" "$cdir" "$rr_no_open" >/dev/null
    exit 0
  fi
  # Acquired before the traps are armed, like `stream`: a refused start must
  # not release the incumbent's lock. A held lock is a silent no-op — the pane
  # record above already retargeted the running renderer, and a newer build's
  # start is no more than that: the incumbent upgrades itself (below) rather than
  # being signalled, because a TERM it acts on only after its current pass lets
  # the newcomer exit on the still-held lock and leaves the crew with no renderer.
  _lock_acquire "$rr_lockd" "$$" || exit 0
  # `_lock_release` is unconditional; the pid check keeps a renderer whose lock
  # was reclaimed (crew dir removed and re-created) from deleting the new owner's.
  trap '[ "$(cat "$rr_lockd/pid" 2>/dev/null || true)" != "$$" ] || _lock_release "$rr_lockd"' EXIT
  trap 'exit 0' INT TERM
  # The baseline for the loop's own re-resolution, taken once: a daemon started
  # while a newer build was already installed keeps running the one it was started
  # with, and follows the entry from then on.
  rr_entry=$(_rr_installed_crew)
  rr_live=1
  rr_sig=""
  rr_last_build=0
  rr_idle_since=""
  while :; do
    # Losing the lock (e.g. `crew deregister` removed the crew dir) ends the loop.
    [ "$(cat "$rr_lockd/pid" 2>/dev/null || true)" = "$$" ] || exit 0
    rr_now=$(_clock_now)
    rr_roles=$(_rr_role_panes "$rr_crew")
    rr_size=0
    [ ! -f "$log" ] || rr_size=$(wc -c <"$log")
    rr_newsig="$rr_size|$(cat "$cdir/roster-render.pane" 2>/dev/null || true)|$rr_roles"
    if [ "$rr_newsig" != "$rr_sig" ] || [ "$((rr_now - rr_last_build))" -ge 60 ]; then
      # A failed pass keeps the last live count rather than reading as drained.
      if rr_out=$(_rr_pass "$rr_crew" "$cdir" "$rr_no_open" "$rr_roles"); then
        rr_live=$rr_out
      fi
      rr_sig="$rr_newsig"
      rr_last_build="$rr_now"
    fi
    if [ "$rr_live" -eq 0 ]; then
      [ -n "$rr_idle_since" ] || rr_idle_since="$rr_now"
      [ "$((rr_now - rr_idle_since))" -lt "$rr_quiet" ] || exit 0
    else
      rr_idle_since=""
    fi
    # Wall-clock sleep: the poll interval is real time, while the quiet window
    # and the rebuild backstop read `_clock_now`, which tests drive via CREW_CLOCK.
    #
    # The sleep point is the only upgrade point, never mid-render: a build
    # installed since the daemon started is picked up in place. `exec` keeps the pid,
    # so the lock is never released or handed off (bash runs no EXIT trap on a
    # successful exec, and `_lock_acquire` is idempotent for the owner's own pid).
    # A crew already draining is left to exit instead of upgraded: the hop would
    # restart its quiet window in the new build, and the next dispatch starts that
    # build anyway. `_rr_self` is the flap guard — an entry that moved back to the
    # build already running is no upgrade. Each hop leaves the build it left on
    # PATH, which only a fresh daemon resets, and which can never hide the next
    # build, since the lookup reads outside /nix/store.
    rr_want=$(_rr_installed_crew)
    if [ -z "$rr_idle_since" ] && [ -n "$rr_want" ] && [ "$rr_want" != "$rr_entry" ] &&
      [ "$rr_want" != "$_rr_self" ]; then
      exec "$rr_want" roster-render "${rr_args[@]}"
    fi
    sleep "$rr_interval"
  done
  ;;
inbox)
  # messages only — `roster` owns status (every status is addressed to the
  # dispatcher, so without this the inbox is status spam). `--since TS` is a
  # non-blocking single pass (no loop, unlike watch/await): return only msgs
  # strictly newer than TS. Omitting it returns all msgs to the agent, unchanged.
  me="${1:-}"
  case "$me" in worker:*) _is_session_id "$me" || {
    echo "crew: $sub: '$me' has no session suffix — pass the session id (\$CREW_WORKER_ID); a branch-only worker id matches no message" >&2
    exit 1
  } ;; esac
  shift || true
  crew=""
  since=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --since)
      [ -n "${2:-}" ] || {
        echo "crew: --since needs a value" >&2
        exit 1
      }
      since="$2"
      shift 2
      ;;
    *)
      crew="$1"
      shift
      ;;
    esac
  done
  crew="${crew:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  rc=0
  if [ -n "$since" ]; then
    case "$since" in '' | *[!0-9]*)
      echo "crew: --since must be an integer ms timestamp" >&2
      exit 1
      ;;
    esac
    out=$(jq -c --arg crew "$crew" --arg me "$me" --argjson since "$since" \
      'select(.crew_id==$crew and .kind=="msg" and (.to==$me or .to=="*") and .ts>$since)' "$log") || rc=$?
  else
    out=$(jq -c --arg crew "$crew" --arg me "$me" \
      'select(.crew_id==$crew and .kind=="msg" and (.to==$me or .to=="*"))' "$log") || rc=$?
  fi
  [ -z "$out" ] || printf '%s\n' "$out"
  # A session that reads its inbox has been handed these msgs, so a later await
  # must not return them again (#290).
  case "$me" in worker:*) [ -z "$out" ] || _await_record "$crew" "$me" "$out" ;; esac
  exit "${rc:-0}"
  ;;
log)
  crew="${1:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  jq -c --arg crew "$crew" 'select(.crew_id==$crew)' "$log"
  ;;
report)
  crew="${1:-$(_crew_id)}"
  [ -f "$log" ] || exit 0
  printf 'engine\tmodel\ttier\tshape\toutcome\tduration_s\n'
  jq -s -r --arg crew "$crew" '
    map(select(.crew_id == $crew)) as $all
    | ($all | map(select(.kind == "dispatch")))[]
    | .branch as $b
    | ($all | map(select(.kind == "status" and ((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b))) as $st
    | ($st | map(select(.body.state == "working")) | sort_by(.ts) | (.[0].ts // null)) as $start
    | ($st | sort_by(.ts) | (.[-1] // null)) as $last
    | [ .engine, .model, .tier, (.shape // "—"),
        ($last.body.state // "—"),
        (if ($start != null and $last != null) then (($last.ts - $start) / 1000 | floor | tostring) else "—" end)
      ] | @tsv' "$log"
  ;;
rate)
  # Sweep this repo's bus into the global ratings store, or (--report) render
  # the global store's per-(engine,model,tier) rollup. The report path never
  # touches the local bus or the network — see docs/superpowers/specs/
  # 2026-08-10-crew-rate-reconcile-report-design.md.
  report=false
  json=false
  pooled=false
  sweep_all=false
  sweep_roots=""
  current_repo=""
  rate_usage="crew: rate takes --report, --json, --pooled, and --sweep-all/--root"
  while [ $# -gt 0 ]; do
    case "$1" in
    --report)
      report=true
      shift
      ;;
    --json)
      json=true
      shift
      ;;
    --pooled)
      pooled=true
      shift
      ;;
    --sweep-all)
      sweep_all=true
      shift
      ;;
    --root)
      [ $# -ge 2 ] && [ -n "$2" ] || {
        echo "crew: rate --root needs a directory" >&2
        exit 1
      }
      sweep_roots="${sweep_roots:+$sweep_roots
}$2"
      shift 2
      ;;
    *)
      echo "$rate_usage" >&2
      exit 1
      ;;
    esac
  done
  if [ -n "$sweep_roots" ] && [ "$sweep_all" = false ]; then
    echo "crew: rate --root needs --sweep-all" >&2
    exit 1
  fi
  if [ "$sweep_all" = true ]; then
    if [ "$report" = true ] || [ "$json" = true ] || [ "$pooled" = true ]; then
      echo "$rate_usage" >&2
      exit 1
    fi
  elif { [ "$json" = true ] || [ "$pooled" = true ]; } && [ "$report" = false ]; then
    echo "$rate_usage" >&2
    exit 1
  fi

  if [ "$sweep_all" = true ]; then
    # Sweeps every repo that has a crew bus, from any repo or none. Each repo
    # is swept by a child `crew rate`, so the per-repo lock, reconcile and
    # gh-credential gate stay exactly what a hand-run sweep uses.
    store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
    registry="$store_dir/repos"
    self=$(readlink -f "$0")
    roots="${sweep_roots:-${CREW_SWEEP_ROOTS:-}}"
    roots="${roots:-$HOME}"
    roots=$(printf '%s' "$roots" | tr ':' '\n')
    repos=$(
      {
        cat "$registry" 2>/dev/null || true
        printf '%s\n' "$roots" | while IFS= read -r root; do
          [ -d "$root" ] || {
            echo "crew: rate --sweep-all: $root is not a directory" >&2
            continue
          }
          # find exits 1 on any unreadable directory; that must not abort the walk.
          { find "$root" -mindepth 1 -maxdepth 6 \
            \( -name '.*' ! -name .git ! -name .worktrees -prune \) -o \
            -path '*/.git/crew/events.jsonl' -print 2>/dev/null || true; } |
            sed 's#/crew/events\.jsonl$##'
        done
      } | sort -u
    )
    [ -n "$repos" ] || echo "crew: rate --sweep-all: no repo with a crew bus found (roots: $(printf '%s' "$roots" | tr '\n' ' '))" >&2
    failed=0
    while IFS= read -r repo_common; do
      [ -n "$repo_common" ] || continue
      if [ ! -d "$repo_common" ]; then
        echo "$repo_common: skipped (gone)"
        continue
      fi
      if [ ! -f "$repo_common/crew/events.jsonl" ]; then
        echo "$repo_common: skipped (no bus)"
        continue
      fi
      workdir=$(dirname "$repo_common")
      origin=$(git -C "$workdir" config --get remote.origin.url 2>/dev/null || true)
      if [ -z "$origin" ]; then
        echo "$workdir: skipped (no origin remote)"
        continue
      fi
      slug=$(printf '%s' "$origin" | sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##')
      rc=0
      (cd "$workdir" && bash -euo pipefail "$self" rate) || rc=$?
      if [ "$rc" -ne 0 ]; then
        echo "$slug: failed (rc=$rc)"
        failed=$((failed + 1))
        continue
      fi
      n=$(jq -s --arg r "$slug" '[.[] | select(.repo == $r) | .run_id] | unique | length' "$store_dir/ratings.jsonl" 2>/dev/null || echo 0)
      echo "$slug: swept, $n runs in store"
    done <<EOF_REPOS
$repos
EOF_REPOS
    exit $((failed > 0))
  fi

  if [ "$report" = true ]; then
    # Reads the global store only — still zero network calls (spec §crew
    # rate --report). The `crew roster` idiom: last row wins per run_id. A
    # missing or empty store folds to [], which renders as the header alone
    # so "no data yet" stays visibly distinct from "command did nothing".
    # Unless --pooled is passed, this also resolves the local repo identity
    # (git config / git rev-parse, both local-only) to scope the report to
    # the current repo.
    if [ "$pooled" = false ]; then
      current_repo=$(git config --get remote.origin.url 2>/dev/null | sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##' || true)
      toplevel=$(git rev-parse --show-toplevel 2>/dev/null || true)
      current_repo="${current_repo:-$(basename "${toplevel:-unknown-repo}")}"
    fi
    store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
    store="$store_dir/ratings.jsonl"
    rows=$(jq -s -c 'group_by(.run_id) | map(max_by(.swept_at))' "$store" 2>/dev/null || true)
    rows="${rows:-[]}"
    # One aggregation shared by both output modes: every column is
    # computed once as {value, k, n} — k is the aggregate's OWN
    # denominator, n is the row's total — and only the final branch differs:
    # --json prints that structure as-is, the table renders and pads it.
    printf '%s' "$rows" | jq -r --argjson want_json "$json" --arg current_repo "$current_repo" --argjson pooled "$pooled" '
      . as $recs |
      # scoped mode intentionally drops rows with no/null .repo — an unlabeled row cannot be claimed to belong to "this repo".
      (if $pooled then $recs else ($recs | map(select(.repo == $current_repo))) end) as $recs |
      def median:
        sort | length as $l
        | if $l == 0 then null
          elif ($l % 2) == 1 then .[($l - 1) / 2 | floor]
          else (.[($l / 2 | floor) - 1] + .[$l / 2 | floor]) / 2
          end;
      def mean: if length == 0 then null else add / length end;
      def nrows: map(select(.outcome != "running" and .outcome != "incomplete"));
      def agg(k; n; v): {value: v, k: k, n: n};
      def fmt1:
        if . == null then null
        else
          (. * 10 | round) as $t
          | (($t / 10) | floor) as $i
          | ($t - ($i * 10)) as $f
          | "\($i).\($f)"
        end;
      # One humanised-duration rule, shared by ttpr/ttmerge.
      def humanize_ms:
        if . == null then null
        elif . < 5400000 then ((. / 60000 | round | tostring) + "m")
        else (((. / 3600000) | fmt1) + "h")
        end;
      # Marker rules (spec §Making small samples impossible to miss):
      # "—" when unmeasured, "(k)" when the own denominator differs from n,
      # "!" when that denominator is below 5.
      def render_agg(k; n; text):
        if k == 0 then "—"
        else (text + (if k != n then "(\(k))" else "" end) + (if k < 5 then "!" else "" end))
        end;

      def footer_line:
        if $pooled then
          ($recs | map(.repo // "unknown") | unique) as $buckets
          | ($buckets | length) as $r
          | ($buckets | sort) as $sorted
          | (if $r > 10 then (($sorted[0:10] | join(", ")) + ", and \($r - 10) more") else ($sorted | join(", ")) end) as $names
          | "Pooled over \($recs | length) runs from \($r) repo\(if $r == 1 then "" else "s" end): \($names)."
        else
          "Scoped to \($current_repo): \($recs | length) runs."
        end;

      # Tier x effort cross-tab, computed from the same $recs as a second
      # aggregation. known_efforts is the full --effort vocabulary (dispatch.sh:455);
      # tier_baseline names the typical rung per tier, marked with a trailing
      # "*" in the rendered cell (same suffix-on-cell-text idiom as the
      # "!"/"(k)" suffixes render_agg already appends). Any effort outside
      # known_efforts, or missing entirely, buckets to "unknown" — the
      # columns are always all 7, in this order, so the shape is stable
      # even when every row is "unknown".
      def known_efforts: ["low", "medium", "high", "xhigh", "max", "ultra"];
      def tier_baseline: {trivial: "low", standard: "medium", deep: "high"};
      def bucket_effort($e): if ($e != null) and (known_efforts | index($e)) then $e else "unknown" end;

      # Renders the whole cross-tab section (leading blank line through the
      # trailing legend line) as an array of lines, or [] when there are no
      # records — kept separate from the main table width/pad computation
      # above so that render path does not regress.
      def cross_tab_lines:
        if ($recs | length) == 0 then []
        else
          (known_efforts + ["unknown"]) as $cols
          | ($recs | map(.tier) | unique) as $tiers
          | ($tiers | map(. as $t
              | $cols | map(. as $c
                  | ($recs | map(select(.tier == $t and (bucket_effort(.effort) == $c))) | length)
                )
            )) as $counts
          | (["tier"] + $cols) as $ct_headers
          | ([true] + ($cols | map(false))) as $ct_left
          | ($tiers | to_entries | map(
              .key as $ti | .value as $t
              | [$t] + ($cols | to_entries | map(
                  .key as $ci | .value as $c
                  | ($counts[$ti][$ci] | tostring) as $base
                  | if (tier_baseline[$t] // null) == $c then ($base + "*") else $base end
                ))
            )) as $ct_rows
          | ([$ct_headers] + $ct_rows) as $ct_all
          | ($ct_headers | length) as $ct_ncols
          | ([range(0; $ct_ncols) | . as $c | ($ct_all | map(.[$c] | length) | max)]) as $ct_widths
          | ([range(0; $ct_ncols) | . as $c
              | $ct_headers[$c] as $cell
              | ($ct_widths[$c] - ($cell|length)) as $pad
              | if $ct_left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
             ] | join("  ")) as $ct_header_line
          | ($ct_rows | map(
              . as $row
              | [range(0; $ct_ncols) | . as $c
                 | $row[$c] as $cell
                 | ($ct_widths[$c] - ($cell|length)) as $pad
                 | if $ct_left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
                ] | join("  ")
            )) as $ct_body_lines
          | ([""] + [$ct_header_line] + $ct_body_lines + ["* = tier-typical effort rung"])
        end;

      def stats:
        $recs
        | group_by([.engine, .model, .tier])
        | map(
            . as $g
            | ($g | nrows) as $n
            | ($n | length) as $ncount
            | {
                engine: $g[0].engine, model: $g[0].model, tier: $g[0].tier,
                n: agg($ncount; $ncount; $ncount),
                inc: ($g | map(select(.outcome == "incomplete")) | length),
                run: ($g | map(select(.outcome == "running")) | length),
                pend: ($n | map(select(.pr_state == "OPEN")) | length),
                pr_pct: (
                  ($n | map(select(.reached_pr == true)) | length) as $num
                  | agg($ncount; $ncount; (if $ncount == 0 then null else (($num / $ncount) * 100 | round) end))
                ),
                merge_pct: (
                  ($n | map(select(.pr_state == "MERGED" or .pr_state == "CLOSED"))) as $settled
                  | ($settled | length) as $k
                  | ($settled | map(select(.pr_state == "MERGED")) | length) as $num
                  | agg($k; $ncount; (if $k == 0 then null else (($num / $k) * 100 | round) end))
                ),
                ttpr_ms: (
                  ($n | map(.time_to_pr_ms) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | median))
                ),
                ttmerge_ms: (
                  ($n | map(.time_to_merge_ms) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | median))
                ),
                rework: (
                  ($n | map(.rework_count) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                # review_mode ∉ {none, null} — a downgraded-but-run review
                # still counts, only "no review happened" is excluded.
                high: (
                  ($n | map(select((.review_mode != null) and (.review_mode != "none") and (.review_high != null))) | map(.review_high)) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                rounds: (
                  ($n | map(.review_rounds) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                # blocked_count/watchdog_blocked_count are bus-derived, never
                # null, so this is the one aggregate whose own denominator is
                # always n — it never carries a "(k)".
                blocked: (
                  ($n | map((.blocked_count // 0) + (.watchdog_blocked_count // 0))) as $vals
                  | agg($ncount; $ncount; ($vals | mean))
                ),
                ci1_pct: (
                  ($n | map(.first_ci_green) | map(select(. != null))) as $vals
                  | ($vals | length) as $k
                  | ($vals | map(select(. == true)) | length) as $num
                  | agg($k; $ncount; (if $k == 0 then null else (($num / $k) * 100 | round) end))
                ),
                notes: (
                  ($n | map(.unresolved_notes) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; ($vals | mean))
                ),
                rev: ($g | map(select(.reverted == true)) | length),
                cost_hours: (
                  ($n | map(.cost_proxy) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; (($vals | mean) | if . == null then null else . / 3600000 end))
                ),
                burn_median: (
                  ($n | map(.cost_proxy) | map(select(. != null))) as $vals
                  | agg(($vals|length); $ncount; (($vals | median) | if . == null then null else . / 3600000 end))
                )
              }
          )
        | sort_by([.engine, .model, .tier]);

      ["engine","model","tier","n","inc","run","pend","pr%","merge%","ttpr","ttmerge","rework","high","rounds","blocked","ci1","notes","rev","cost"] as $headers
      | [true,true,true,false,false,false,false,false,false,false,false,false,false,false,false,false,false,false,false] as $left
      | stats as $groups
      | if $want_json then $groups
        else
          ($groups | map(
              [
                .engine, .model, .tier,
                render_agg(.n.k; .n.n; (.n.value|tostring)),
                (.inc|tostring), (.run|tostring), (.pend|tostring),
                render_agg(.pr_pct.k; .pr_pct.n; (.pr_pct.value|tostring)),
                render_agg(.merge_pct.k; .merge_pct.n; (.merge_pct.value|tostring)),
                render_agg(.ttpr_ms.k; .ttpr_ms.n; (.ttpr_ms.value|humanize_ms)),
                render_agg(.ttmerge_ms.k; .ttmerge_ms.n; (.ttmerge_ms.value|humanize_ms)),
                render_agg(.rework.k; .rework.n; (.rework.value|fmt1)),
                render_agg(.high.k; .high.n; (.high.value|fmt1)),
                render_agg(.rounds.k; .rounds.n; (.rounds.value|fmt1)),
                render_agg(.blocked.k; .blocked.n; (.blocked.value|fmt1)),
                render_agg(.ci1_pct.k; .ci1_pct.n; (.ci1_pct.value|tostring)),
                render_agg(.notes.k; .notes.n; (.notes.value|fmt1)),
                (.rev|tostring),
                render_agg(.cost_hours.k; .cost_hours.n; (.cost_hours.value|fmt1))
              ]
            )) as $rows
          | ([$headers] + $rows) as $all
          | ($headers | length) as $ncols
          # Widths and padding computed here — never `column -t`: it is not in
          # runtimeInputs, and BSD vs util-linux pad "—" differently.
          | ([range(0; $ncols) | . as $c | ($all | map(.[$c] | length) | max)]) as $widths
          | ([range(0; $ncols) | . as $c
              | $headers[$c] as $cell
              | ($widths[$c] - ($cell|length)) as $pad
              | if $left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
             ] | join("  ")) as $header_line
          | if ($groups | length) == 0 then
              (if $pooled then $header_line else "\($current_repo): no runs swept for this repo yet" end)
            else
              ($rows | map(
                  . as $row
                  | [range(0; $ncols) | . as $c
                     | $row[$c] as $cell
                     | ($widths[$c] - ($cell|length)) as $pad
                     | if $left[$c] then ($cell + (" " * $pad)) else ((" " * $pad) + $cell) end
                    ] | join("  ")
                )) as $body_lines
              | ($groups | map(
                  [.n.k, .pr_pct.k, .merge_pct.k, .ttpr_ms.k, .ttmerge_ms.k, .rework.k, .high.k,
                   .rounds.k, .blocked.k, .ci1_pct.k, .notes.k, .cost_hours.k]
                  | any(. < 5)
                ) | map(select(.)) | length) as $flagged_count
              | ($groups | length) as $total
              | ([$header_line] + $body_lines
                 + ["", "! own sample < 5 — anecdote, not evidence.   value(k) = measured over k of n runs.   — = unmeasured."]
                 + ["\($flagged_count) of \($total) rows carry at least one small-sample quantity."]
                 + [footer_line]
                 + cross_tab_lines
                ) | join("\n")
            end
        end
    '
    exit 0
  fi

  # One record per RUN (a run = a dispatch + the branch events until the next
  # dispatch on that branch). Append-only; readers fold last-wins by run_id.
  # No crew filter — ratings are cross-run/cross-crew evidence.
  [ -f "$log" ] || exit 0
  repo=$(git config --get remote.origin.url 2>/dev/null |
    sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##' || true)
  repo="${repo:-$(basename "$(git rev-parse --show-toplevel)")}"
  # jq cannot call _burn_weight; resolve each distinct model+effort here and
  # key the lookup by both, so opus@low prices below opus@high (its class
  # follows effort).
  targets=$(jq -s -r '[.[] | select(.kind=="dispatch") | [(.model // ""), (.effort // "")]] | unique | .[] | @tsv' "$log")
  # Resolve the burn table once. _burn_weight runs in a command substitution
  # per model, so without this each row would fork dispatch-config afresh.
  _BURN_SETTINGS="$("${DISPATCH_CONFIG_BIN:-dispatch-config}" 2>/dev/null || true)"
  # Without a resolver the table is empty, which per-row looks exactly like a
  # legitimately unclassed model — say so once instead of reporting a clean run
  # of null cost classes.
  if [ -z "$_BURN_SETTINGS" ]; then
    echo "crew rate: could not resolve settings (dispatch-config unavailable) — every model will read as unclassed; set DISPATCH_CONFIG_BIN or install dispatch-config beside crew" >&2
  fi
  costmap='{}'
  while IFS=$'\t' read -r model effort; do
    [ -n "$model" ] || continue
    cw=$(_burn_weight "$model" "$effort")
    [ -n "$cw" ] || continue
    class="${cw%%$'\t'*}"
    weight="${cw#*$'\t'}"
    costmap=$(printf '%s' "$costmap" | jq -c --arg m "$model" --arg e "$effort" --arg c "$class" --argjson w "$weight" \
      '. + {(($m) + "\t" + ($e)): [$c, $w]}')
  done <<<"$targets"
  records=$(jq -s --arg repo "$repo" --argjson costmap "$costmap" '
    (map(select(.kind=="dispatch"))) as $disp
    | [ ($disp | map(.branch) | unique)[] as $b
        | ($disp | map(select(.branch==$b)) | sort_by(.ts)) as $runs
        | ($runs|length) as $n
        # Branch reuse (spec §Branch reuse and t2): the PR owner is the
        # earliest run on this branch whose OWN window contains a status
        # carrying a pr_url. Computed once per branch, entirely bus-derived,
        # before any per-run field depends on it.
        | ([ range(0; $n) as $k
            | $runs[$k] as $dk
            | $dk.ts as $tk0
            | (if $k+1 < $n then $runs[$k+1].ts else 9999999999999 end) as $tk1
            | (map(select(
                  .kind!="dispatch"
                  and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
                  and .ts >= $tk0 and .ts < $tk1))
               | map(select(.kind=="status"))
               | map(.body.pr_url) | map(select(.!=null)) | last)
          ]) as $owner_prs
        | ($owner_prs | to_entries | map(select(.value != null)) | (.[0].key // null)) as $owner_idx
        | range(0; $n) as $i
        | $runs[$i] as $d
        | ($d.ts) as $t0
        | (if $i+1 < $n then $runs[$i+1].ts else 9999999999999 end) as $t1
        | (if $i+1 < $n then $runs[$i+1].ts else null end) as $window_end
        | (map(select(
              .kind!="dispatch"
              and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$";"")) == $b)
              and .ts >= $t0 and .ts < $t1))) as $ev
        | ($ev | map(select(.kind=="status"))) as $st
        | ($st | sort_by(.ts) | (.[-1] // null)) as $last
        | ($last.body.state) as $ls
        | ($st | map(.body.pr_url) | map(select(.!=null)) | last) as $pr
        | ($st | map(select(.body.state=="pr_open")) | sort_by(.ts) | (.[0] // null)) as $propen
        | ($pr != null and $i == $owner_idx) as $owns_pr
        # Narrower than the terminal set below on purpose (spec §Cost): a
        # zombie has an unbounded wall clock, not a zero one.
        | ($st | map(select(.body.state=="done" or .body.state=="failed")) | sort_by(.ts) | (.[-1] // null)) as $term
        | (if $term != null then ($term.ts - $t0) else null end) as $wall
        | (($d.shape // null) | if . == "" then null else . end) as $shape
        | ($costmap[($d.model // "") + "\t" + ($d.effort // "")] // null) as $cw
        | (null) as $pr_state
        # Split rather than replace. `blocked_count` feeds an append-only,
        # cross-run ratings store whose rows are compared against rows written
        # before the watchdog existed; changing what populates that field in place
        # would make old and new rows silently non-comparable in the same field.
        # A new field leaves historical rows simply absent (null), which every
        # reader there already tolerates — and watchdog_blocked_count is direct
        # evidence of how often a model/tier wedges. A budget: row is a host-wide
        # quota crossing, not a model/tier wedge, so it stays out of
        # watchdog_blocked_count.
        | ($st | map(select(.body.state=="blocked" and (.body.source // "") != "watchdog")) | length) as $blocked
        | ($st | map(select(.body.state=="blocked" and (.body.source // "") == "watchdog"
                            and (((.body.detail // "") | if type == "string" then startswith("budget:") else false end) | not))) | length) as $wblocked
        # `try fromjson catch null` so ONE unparseable body cannot abort the whole
        # sweep and lose every other run with it (#25). Such a run folds with null
        # metrics — the same shape a run that never emitted metrics already takes.
        | ($ev | map(select(.kind=="msg" and ((.to // "") | startswith("metrics:"))))
               | sort_by(.ts) | (.[-1] // null)
               | if . == null then null else (.body | try fromjson catch null) end) as $m
        | {
            run_id: ($repo + ":" + $b + ":" + ($t0|tostring)),
            repo: $repo, branch: $b,
            session: ($d.session // null),
            engine: $d.engine, model: $d.model, tier: $d.tier,
            effort: $d.effort, title: $d.title,
            shape: $shape,
            # null for a run scaffolded by a `dispatch` older than this field.
            task_kind: ($d.task_kind // null),
            t0_ms: $t0,
            window_end_ms: $window_end,
            reached_pr: ($propen != null),
            pr_url: $pr,
            owns_pr: $owns_pr,
            time_to_pr_ms: (if $owns_pr and $propen != null then ($propen.ts - $t0) else null end),
            wall_clock_ms: $wall,
            cost_class: (if $cw == null then null else $cw[0] end),
            cost_proxy: (if $cw == null or $wall == null then null else ($cw[1] * $wall) end),
            # `done` is the success signal a worker posts for itself, and plenty
            # of dispatched work (reviews, measurements, experiments) has no PR
            # as its deliverable — so a missing PR is not evidence of failure.
            # Only a worker-reported `failed` is. Rows swept before this split
            # carry "failed" for both, so a reader spanning them must fold on
            # terminal_state, not on outcome.
            outcome: (
              if ($owns_pr and $pr_state == "MERGED") then "merged"
              elif ($ls == "working" or $ls == "blocked") then "running"
              elif ($pr != null) then "pr_open"
              elif ($ls == "done") then "done"
              elif ($ls == "failed") then "failed"
              else "incomplete" end),
            terminal_state: ($ls // null),
            rework_count: ($m.rework_count // null),
            replanned: (
              if $m == null or (($m | has("replanned")) | not)
              then null
              else $m.replanned
              end
            ),
            review_high: ($m.review_high // null),
            review_mode: ($m.review_mode // null),
            plan_critic_first_pass: ($m.plan_critic_first_pass // null),
            consulted: (if $m == null then null else $m.consulted end),
            blocked_count: $blocked,
            watchdog_blocked_count: $wblocked,
            reported_ok: (($m != null) and ($ls != null)),
            # t2 placeholders. Emitted null by the local fold so every row has
            # one shape from its first sweep; the reconcile below fills them.
            pr_state: $pr_state,
            last_query_ok: null,
            merged: null,
            closed_at_ms: null,
            merged_at_ms: null,
            merge_commit: null,
            time_to_merge_ms: null,
            review_rounds: null,
            first_ci_green: null,
            unresolved_notes: null,
            reverted: null,
            swept_at: (now*1000|floor)
          } ]' "$log")
  store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
  store="$store_dir/ratings.jsonl"
  mkdir -p "$store_dir"
  # `crew rate --sweep-all` also discovers repos from here, so a repo swept
  # once stays found wherever it lives on disk.
  registry="$store_dir/repos"
  grep -qxF "$common" "$registry" 2>/dev/null || printf '%s\n' "$common" >>"$registry"
  lockd="$store_dir/ratings.lock.d"
  # The `crew roster` idiom: last row wins per run_id. A missing store folds
  # to [], not an error.
  store_fold='group_by(.run_id) | map(max_by(.swept_at))'

  # --- lock window 1: read the store to decide which calls to skip ---------
  # The lock is never held across the network (spec §Merge-forward): on a repo
  # with many historical runs the reconcile is minutes long, and every other
  # repo's sweep would block on it. `_lock_release` is an unconditional
  # `rm -rf` with no owner check, so the EXIT trap is armed only while this
  # process actually holds the lock — left armed through the unlocked phase it
  # would delete a CONCURRENT sweep's lock dir on exit.
  if ! _lock_acquire "$lockd" "$$"; then
    echo "crew: ratings store busy" >&2
    exit 1
  fi
  trap '_lock_release "$lockd"' EXIT
  snapshot=$(jq -s -c "$store_fold" "$store" 2>/dev/null || true)
  _lock_release "$lockd"
  trap - EXIT
  snapshot="${snapshot:-[]}"

  # --- t2: the GitHub reconcile, unlocked ---------------------------------
  threads_q="query(\$owner:String!,\$name:String!,\$number:Int!){repository(owner:\$owner,name:\$name){pullRequest(number:\$number){reviewThreads(first:100){nodes{isResolved}}}}}"
  # gh marshals an unset timestamp as Go's zero time rather than null, and a
  # gh timestamp is an ISO-8601 string while t0_ms/window_end_ms are epoch ms —
  # comparing the two silently succeeds in jq (every string sorts above every
  # number), so the conversion happens at ingest, once.
  ts2ms='def ts2ms: if . == null or . == "" or startswith("0001-01-01") then null else (fromdateiso8601 * 1000) end;'
  patches='{}'
  gh_failures=0
  while IFS=$'\t' read -r run_id pr_url branch t0_ms win_end do_view do_actions do_threads s_commit s_merged; do
    # The gate is evaluated BEFORE any call: a pr_url that is not a GitHub PR
    # URL issues zero calls. owner/name come from THIS url, never from the
    # sweeping checkout, so a store holding several repos never cross-queries.
    if [[ ! $pr_url =~ ^https://github\.com/([^/]+)/([^/]+)/pull/([0-9]+)$ ]]; then
      continue
    fi
    owner="${BASH_REMATCH[1]}"
    name="${BASH_REMATCH[2]}"
    number="${BASH_REMATCH[3]}"
    # `pr_url` is unvalidated worker-written text, so the URL alone must not
    # decide which repo we spend the developer's gh credential on: a run can
    # only ever reconcile a PR in the repo it was dispatched from.
    if [ "$owner/$name" != "$repo" ]; then
      continue
    fi
    patch='{}'
    tried=false
    ok=true
    m_commit=""
    m_at=""
    if [ "$s_commit" != - ]; then m_commit="$s_commit"; fi
    if [ "$s_merged" != - ]; then m_at="$s_merged"; fi

    if [ "$do_view" = true ]; then
      tried=true
      # Always with the URL: a bare `gh pr view` resolves the sweeping
      # checkout's own branch and would write that PR into every row.
      out=$(_gh_json pr view "$pr_url" --json state,closedAt,mergedAt,mergeCommit,commits,reviews)
      if [ -n "$out" ]; then
        patch=$(printf '%s' "$out" | jq -c --argjson p "$patch" "$ts2ms"'
          ([ (.reviews // [])[]
             | select(.state == "CHANGES_REQUESTED" or .state == "COMMENTED")
             | (.submittedAt | ts2ms) ] | map(select(. != null))) as $rv
          | ([ (.commits // [])[] | (.committedDate | ts2ms) ] | map(select(. != null))) as $cm
          | $p + {
              pr_state: .state,
              closed_at_ms: (.closedAt | ts2ms),
              merged_at_ms: (.mergedAt | ts2ms),
              merge_commit: (.mergeCommit.oid // null),
              # A round is a maximal group of rework reviews followed by >=1
              # later commit, so three reviews before one fix commit is ONE
              # round: bucket each review by the first commit that answers it
              # and count the distinct buckets. Reviews never answered by a
              # commit bucket to null and contribute 0.
              review_rounds: ([ $rv[] as $r | ($cm | map(select(. > $r)) | min) ]
                              | map(select(. != null)) | unique | length)
            }')
        m_commit=$(printf '%s' "$patch" | jq -r '.merge_commit // ""')
        m_at=$(printf '%s' "$patch" | jq -r '(.merged_at_ms // "") | tostring')
      else
        ok=false
        gh_failures=$((gh_failures + 1))
      fi
    fi

    if [ "$do_actions" = true ]; then
      tried=true
      if [ "$win_end" = - ]; then win_end=null; fi
      out=$(_gh_json api --method GET "repos/$owner/$name/actions/runs" -f branch="$branch" -F per_page=100)
      if [ -n "$out" ]; then
        patch=$(printf '%s' "$out" | jq -c --argjson p "$patch" \
          --argjson t0 "$t0_ms" --argjson we "$win_end" '
          $p + {
            # Only the CI this run triggered: the dispatch-boundary window is
            # what stops run i+1 on a reused branch from inheriting the CI of
            # run i.
            first_ci_green: (
              [ (.workflow_runs // [])[]
                | {sha: .head_sha, c: (.created_at | fromdateiso8601 * 1000),
                   status: .status, concl: .conclusion} ]
              | map(select(.c >= $t0 and ($we == null or .c < $we)))
              | if length == 0 then null
                else group_by(.sha) | map({first: (map(.c) | min), runs: .}) | min_by(.first)
                     | .runs | map(select(.status == "completed"))
                     | all(.concl == "success" or .concl == "skipped" or .concl == "neutral")
                end)
          }')
      else
        ok=false
        gh_failures=$((gh_failures + 1))
      fi
    fi

    if [ "$do_threads" = true ]; then
      tried=true
      out=$(_gh_json api graphql -f query="$threads_q" -F owner="$owner" -F name="$name" -F number="$number")
      if [ -n "$out" ]; then
        patch=$(printf '%s' "$out" | jq -c --argjson p "$patch" '
          $p + {unresolved_notes: ([ (.data.repository.pullRequest.reviewThreads.nodes // [])[]
                                     | select(.isResolved == false) ] | length)}')
      else
        ok=false
        gh_failures=$((gh_failures + 1))
      fi
    fi

    # `reverted` — local, no API call. The existence probe is what keeps "not
    # reverted" distinguishable from "merge commit not in this checkout":
    # without it every unfetched repo would report a clean false.
    if [ -n "$m_commit" ] && [ -n "$m_at" ]; then
      if git cat-file -e "$m_commit^{commit}" 2>/dev/null; then
        # `--since` takes approxidate or @<epoch SECONDS>, and silently falls
        # back rather than erroring on a bare 13-digit millisecond value.
        if [ -n "$(git log --all --no-show-signature --since="@$((m_at / 1000))" \
          --grep="This reverts commit $m_commit" --max-count=1 2>/dev/null || true)" ]; then
          rev=true
        else
          rev=false
        fi
      else
        rev=null
      fi
      patch=$(printf '%s' "$patch" | jq -c --argjson r "$rev" '. + {reverted: $r}')
    fi

    if [ "$tried" = true ]; then
      patch=$(printf '%s' "$patch" | jq -c --argjson ok "$ok" '. + {last_query_ok: $ok}')
    fi
    patches=$(printf '%s' "$patches" | jq -c --arg id "$run_id" --argjson p "$patch" '. + {($id): $p}')
  done < <(printf '%s' "$records" | jq -r --argjson snap "$snapshot" '
    ($snap | map({key: .run_id, value: .}) | from_entries) as $S
    | (now * 1000) as $now
    | .[] as $r
    | ($S[$r.run_id] // {}) as $s
    | select($r.owns_pr)
    # Per-call finality (spec §Per-call finality): a run-level skip would
    # strand any field whose own call failed on the sweep that first saw the
    # merge, with no path back.
    | [ $r.run_id, $r.pr_url, $r.branch, $r.t0_ms, ($r.window_end_ms // "-"),
        (($s.pr_state == "MERGED")
         or ($s.pr_state == "CLOSED" and $s.closed_at_ms != null
             and ($now - $s.closed_at_ms) >= 2592000000) | not),
        ($s.first_ci_green == null),
        (($s.pr_state == "MERGED" and $s.unresolved_notes != null) | not),
        ($s.merge_commit // "-"), ($s.merged_at_ms // "-") ] | @tsv')

  # --- lock window 2: merge forward against a FRESH read, append the diff --
  if ! _lock_acquire "$lockd" "$$"; then
    echo "crew: ratings store busy" >&2
    exit 1
  fi
  trap '_lock_release "$lockd"' EXIT
  fresh=$(jq -s -c "$store_fold" "$store" 2>/dev/null || true)
  fresh="${fresh:-[]}"
  printf '%s' "$records" | jq -c --argjson stored "$fresh" --argjson patches "$patches" '
    # The window-1 snapshot decided which calls to skip; carrying values
    # forward from it would drop a t2 upgrade another sweep landed while this
    # one was on the network.
    ($stored | map({key: .run_id, value: .}) | from_entries) as $S
    | (now * 1000 | floor) as $sw
    | ["pr_state","closed_at_ms","merged_at_ms","merge_commit",
       "review_rounds","first_ci_green","unresolved_notes","reverted"] as $t2keys
    | .[] as $r
    | ($S[$r.run_id] // {}) as $s
    | ($patches[$r.run_id] // {}) as $p
    # For every field whose call did not succeed, the STORED value carries
    # forward — writing null instead would let one expired token permanently
    # degrade every already-reconciled row.
    | ($t2keys | map(. as $k | {key: $k, value: (
          if ($r.owns_pr | not) then null
          elif ($p | has($k)) then $p[$k]
          else $s[$k] end)}) | from_entries) as $t2
    | ($r + $t2 + {
        merged: (if $t2.pr_state == null then null else ($t2.pr_state == "MERGED") end),
        time_to_merge_ms: (if $t2.merged_at_ms == null then null else ($t2.merged_at_ms - $r.t0_ms) end),
        last_query_ok: $p.last_query_ok,
        outcome: (if ($r.owns_pr and $t2.pr_state == "MERGED") then "merged" else $r.outcome end),
        swept_at: $sw
      }) as $new
    # Ignoring swept_at is what makes an unchanged sweep a byte-level no-op;
    # without it every sweep would append every row forever.
    | select($S[$r.run_id] == null
             or ($S[$r.run_id] | del(.swept_at)) != ($new | del(.swept_at)))
    # The same single-write primitive `_bus_append` uses, but batched rather
    # than per-line, so the guarantee holds only while the whole batch fits
    # one `bs` block: past 1 MiB (~2k rows at ~500 B each) dd splits at an
    # arbitrary byte. A torn line is unrecoverable — every reader folds a
    # parse error to `[]` — and `--report` reads the store without taking
    # `ratings.lock.d`, so it can observe the split.
    | $new' | dd bs=1048576 iflag=fullblock status=none >>"$store"
  _lock_release "$lockd"
  trap - EXIT
  # stdout stays clean (spec §crew rate --report renders the store, nothing
  # else) — an expired token or GitHub outage is signalled on stderr only.
  if [ "$gh_failures" -gt 0 ]; then
    echo "crew: rate: $gh_failures gh call(s) failed; those rows kept their stored t2" >&2
  fi
  ;;
retro)
  # Read-only rollup of the retro notes workers and the dispatcher write to the
  # synthetic `retro:`/`metrics:` sinks (docs/superpowers/specs/
  # 2026-08-05-retro-notes-design.md). jq over this repo's bus, not the design's
  # cross-repo DuckDB: `rate` folds the same bus with jq, and retro reuses its
  # run attribution.
  report=false
  json=false
  while [ $# -gt 0 ]; do
    case "$1" in
    --report)
      report=true
      shift
      ;;
    --json)
      json=true
      shift
      ;;
    *)
      echo "crew: retro takes --report and --json" >&2
      exit 1
      ;;
    esac
  done
  if [ "$json" = true ] && [ "$report" = false ]; then
    echo "crew: retro takes --report and --json" >&2
    exit 1
  fi
  # No crew filter and no positional — notes are cross-run evidence, like `rate`.
  [ -f "$log" ] || exit 0
  [ "$report" = true ] || printf 'branch\tengine\tmodel\ttier\toutcome\ttags\n'
  jq -s -r --argjson want_report "$report" --argjson want_json "$json" '
    # unique_by would sort, and both the tag list on a row and the note order
    # within a run are read in first-appearance order.
    def uniq_order: reduce .[] as $x ([]; if any(.[]; . == $x) then . else . + [$x] end);
    # The same note reaches the reader twice by design: a worker keeps its
    # mid-execute seam note in the final snapshot too. Dedupe on the whole
    # triple, since {seam,tag} repeats legitimately with a different detail.
    def dedupe_notes:
      reduce .[] as $x ([];
        if any(.[]; [.seam, .tag, .detail] == [$x.seam, $x.tag, $x.detail])
        then . else . + [$x] end);
    def tagstr:
      (map(.tag | tostring)) as $ts
      | ($ts | uniq_order)
      | map(. as $t
            | ($ts | map(select(. == $t)) | length) as $c
            | if $c > 1 then "\($t) x\($c)" else $t end)
      | join(",");
    # `try fromjson catch null` so ONE unparseable body cannot abort the fold
    # and lose every other run with it (#25).
    def body_obj: (try fromjson catch null) | if type == "object" then . else null end;
    def note_list: if type == "array" then map(select(type == "object")) else [] end;
    # Tags and details are agent-authored free text, and a gate_thrash detail
    # quotes arbitrary source and command output. Tab/newline/CR become a space
    # so a padded row keeps its columns; the rest go, because a raw ESC
    # repaints the table — one note can erase or forge the row above it — and
    # the bidi overrides let a string misrepresent itself in place.
    def clean:
      gsub("[\t\n\r]"; " ")
      | gsub("[\\x00-\\x1f\\x7f\\x{0080}-\\x{009f}\\x{202a}-\\x{202e}\\x{2066}-\\x{2069}]"; "");
    def sample: clean | if length > 60 then (.[0:60] + "…") else . end;
    def pad_row($cells; $widths; $left):
      # A right-aligned column pads before its value, so only a left-aligned
      # LAST column would trail the row with spaces. It gets none.
      (($cells | length) - 1) as $end
      | [ range(0; $cells | length) | . as $c
          | $cells[$c] as $cell
          | ($widths[$c] - ($cell | length)) as $p
          | if $left[$c] | not then ((" " * $p) + $cell)
            elif $c == $end then $cell
            else ($cell + (" " * $p)) end
        ] | join("  ");

    # Unlike every other tag here, "plan_required_unaudited" is not authored by
    # a worker or the dispatcher — `crew retro` synthesizes it itself from the
    # bus below. Do not add it to a WORKER_PROTOCOL.md/DISPATCHER_PROTOCOL.md
    # retro-notes table; there is no writer to document.
    ["command_not_found", "gate_thrash", "approach_abandoned", "consult_failed",
     "rung_blocked", "review_unavailable", "plan_required_unaudited", "other",
     "misrouted", "fanout_binder", "spec_too_thin", "session_summary"] as $vocab
    | . as $all
    | ($all | map(select(.kind == "dispatch"))) as $disp
    # Dispatch-boundary windowing, as `crew rate` folds it: run i owns
    # [runs[i].ts, runs[i+1].ts), so a re-dispatch on a reused branch cannot
    # inherit the notes of the run before it.
    | [ ($disp | map(.branch) | unique)[] as $b
        | ($disp | map(select(.branch == $b)) | sort_by(.ts)) as $runs
        | ($runs | length) as $n
        | range(0; $n) as $i
        | $runs[$i] as $d
        | $d.ts as $t0
        | (if $i + 1 < $n then $runs[$i+1].ts else 9999999999999 end) as $t1
        # A `dispatch resume` row has no `from` field, so it is invisible to
        # $ev (which is filtered by `from`) — match it directly off $all instead.
        | ($all | any(.kind == "resume" and .branch == $b and .ts >= $t0 and .ts < $t1)) as $resumed
        | ($all | map(select(
              .kind != "dispatch"
              and (((.from // "") | ltrimstr("worker:") | sub("#[^#]*$"; "")) == $b)
              and .ts >= $t0 and .ts < $t1))) as $ev
        | ($ev | map(select(.kind == "status")) | sort_by(.ts) | (.[-1] // null)) as $last
        # Latest snapshot only: putting notes INSIDE the snapshot is what makes
        # a resumed run supersede rather than duplicate (design §Mechanism).
        | ($ev | map(select(.kind == "msg" and ((.to // "") | startswith("metrics:"))))
               | sort_by(.ts) | (.[-1] // null)
               | if . == null then null else (.body | body_obj) end) as $m
        | (if $m == null then [] else ($m.notes | note_list) end) as $snap
        # `plan: required` with a null plan-critic verdict flags a skipped gate
        # (#179) — except tier/task_kind runs with no plan phase, a resumed run
        # (either shape — a `dispatch resume` row has no `from`, hence $resumed
        # above), or a run that never reached the review gate (review_mode ==
        # "none"), which never got far enough to skip anything.
        | (if $d.plan == "required"
              and ($d.tier // null) != "trivial"
              and (($d.task_kind // "implement") != "review")
              and ($resumed | not)
              and (($d.resume // false) != true)
              and $m != null
              and (($m.plan_critic_first_pass // null) == null)
              and (($m.review_mode // null) != "none")
           then [{seam: "plan", tag: "plan_required_unaudited",
                  detail: "plan: required but plan_critic_first_pass is null in the metrics snapshot"}]
           else [] end) as $flag
        | ($ev | map(select(.kind == "msg"
                            and ((.to // "") | startswith("retro:"))
                            and ((.from // "") | startswith("worker:"))))
               | sort_by(.ts) | map(.body | body_obj) | map(select(. != null))) as $seam
        | { branch: $b, crew: ($d.crew_id // null), engine: ($d.engine // "—"), model: ($d.model // "—"),
            tier: ($d.tier // "—"),
            # Type-guarded for the same reason as body_obj: a status whose body
            # is not an object, or whose state is not a scalar, must cost this
            # one run its outcome, not abort the fold and take every other row
            # down with it (#25).
            outcome: (($last.body | if type == "object" then .state else null end)
                      | if (type == "object" or type == "array") then null else . end
                      // "—"),
            t0: $t0, is_run: true,
            notes: (($seam + $snap + $flag) | dedupe_notes) }
      ] as $wrows
    # Dispatcher notes are crew-scoped, not run-scoped: their `from` matches no
    # branch, so no window can hold them.
    | ($all | map(select(.kind == "msg"
                         and ((.to // "") | startswith("retro:"))
                         and ((.from // "") | startswith("dispatcher:"))))
            | group_by(.to)
            | map({ branch: ("dispatcher:" + (.[0].to | ltrimstr("retro:"))),
                    crew: (.[0].to | ltrimstr("retro:")),
                    engine: "—", model: "—", tier: "—", outcome: "—",
                    t0: (map(.ts) | min), is_run: false,
                    notes: (sort_by(.ts) | map(.body | body_obj)
                            | map(select(. != null)) | dedupe_notes) })) as $drows
    # A clean run is silent: no notes, no row.
    # Tags are cleaned here, once, so the grouping key, the vocabulary check,
    # the first report column, the bare-mode tag list and the `unknown` array
    # all name the same value. Details stay verbatim — JSON encoding renders
    # their control bytes inert — so only `sample` cleans them.
    | (($wrows | map(select((.notes | length) > 0)) | sort_by([.branch, .t0]))
       + ($drows | map(select((.notes | length) > 0)))
       | map(.notes |= map(.tag |= (tostring | clean)))) as $rows
    | [ $rows[] as $r
        | $r.notes[] as $note
        | { tag: ($note.tag | tostring),
            detail: (($note.detail // "") | tostring),
            run: (if $r.is_run then ($r.branch + "@" + ($r.t0 | tostring)) else null end),
            engine: (if $r.is_run then $r.engine else null end),
            outcome: $r.outcome } ] as $flat
    | ($flat | group_by(.tag) | map(
        .[0].tag as $t
        | (map(select(.run != null))) as $wn
        | ($wn | map(.run) | unique) as $rl
        | { tag: $t,
            known: (($vocab | index($t)) != null),
            hits: length,
            runs: ($rl | length),
            engines: ($wn | map(.engine) | unique),
            nondone_pct: (
              if ($rl | length) == 0 then null
              else ((($wn | group_by(.run) | map(.[0])
                          | map(select(.outcome != "done")) | length) * 100)
                    / ($rl | length) | round)
              end),
            details: map(.detail) })) as $groups
    # Known tags in vocabulary order, then unknown ones alphabetically.
    | (($groups | map(select(.known)) | sort_by(. as $g | $vocab | index($g.tag)))
       + ($groups | map(select(.known | not)) | sort_by(.tag))) as $sorted
    | ($sorted | map(select(.known | not)) | map(.tag)) as $unknown
    | if $want_json then
        { tags: $sorted, unknown: $unknown,
          rows: ($rows | map({
              kind: (if .is_run then "run" else "dispatcher" end),
              crew, branch, engine, model, tier, outcome, t0,
              notes: (.notes | map({seam, tag, detail}))
            })) }
      elif $want_report then
        ["tag", "hits", "runs", "engines", "nondone", "sample"] as $headers
        | [true, false, false, true, false, true] as $left
        | ($sorted | map([
              .tag, (.hits | tostring), (.runs | tostring),
              (if (.engines | length) == 0 then "—" else (.engines | join(",")) end),
              (if .nondone_pct == null then "—" else (.nondone_pct | tostring) end),
              ((.details | map(select(. != "")) | (.[0] // "")) | sample)
            ])) as $trows
        # Widths and padding computed here — never `column -t`: it is not in
        # runtimeInputs, and BSD vs util-linux pad "—" differently.
        | ([range(0; $headers | length) | . as $c
            | (([$headers] + $trows) | map(.[$c] | length) | max)]) as $widths
        | pad_row($headers; $widths; $left) as $header_line
        | if ($sorted | length) == 0 then $header_line
          else
            ([$header_line]
             + ($trows | map(pad_row(.; $widths; $left)))
             + (if ($unknown | length) > 0
                then ["\($unknown | length) unrecognized tag(s): \($unknown | join(", "))"]
                else [] end)) | join("\n")
          end
      else $rows[] | [.branch, .engine, .model, .tier, .outcome, (.notes | tagstr)] | @tsv
      end
  ' "$log"
  ;;
hold)
  # A queued dispatch parked on a quota window, so a successor session can
  # resume it without a human. Records go to the synthetic sink
  # `hold:<crew>`, matching `retro`/`metrics` — `watch`'s `to==$me or to=="*"`
  # predicate (crew.sh:917) matches neither, so a hold cannot wake or pollute
  # the inbox of the dispatcher that wrote it, while `crew log` still shows it.
  holdsub="${1:-}"
  shift || true
  case "$holdsub" in
  add)
    # crew hold add --engine E --window W --resets-at EPOCH --agent A --ref R
    #   --branch B --tier T --model M --effort F [--plan P] [--mcp P] [--draft]
    #   [--shape S] [--spec FILE] [--crew ID] <title...>
    #
    # --engine is the engine whose quota is being waited on; --agent is the
    # engine the task will be dispatched to (task.engine). Both are required
    # and kept distinct — neither is inferred from the other.
    engine="" window="" resets_at="" agent="" ref="" branch="" tier="" model="" effort=""
    plan="" mcp="" draft=false shape="" spec="" hcrew=""
    while [ $# -gt 0 ]; do
      case "$1" in
      --engine)
        [ -n "${2:-}" ] || {
          echo "crew: --engine needs a value" >&2
          exit 1
        }
        engine="$2"
        shift 2
        ;;
      --window)
        [ -n "${2:-}" ] || {
          echo "crew: --window needs a value" >&2
          exit 1
        }
        window="$2"
        shift 2
        ;;
      --resets-at)
        [ -n "${2:-}" ] || {
          echo "crew: --resets-at needs a value" >&2
          exit 1
        }
        resets_at="$2"
        shift 2
        ;;
      --agent)
        [ -n "${2:-}" ] || {
          echo "crew: --agent needs a value" >&2
          exit 1
        }
        agent="$2"
        shift 2
        ;;
      --ref)
        [ -n "${2:-}" ] || {
          echo "crew: --ref needs a value" >&2
          exit 1
        }
        ref="$2"
        shift 2
        ;;
      --branch)
        [ -n "${2:-}" ] || {
          echo "crew: --branch needs a value" >&2
          exit 1
        }
        branch="$2"
        shift 2
        ;;
      --tier)
        [ -n "${2:-}" ] || {
          echo "crew: --tier needs a value" >&2
          exit 1
        }
        tier="$2"
        shift 2
        ;;
      --model)
        [ -n "${2:-}" ] || {
          echo "crew: --model needs a value" >&2
          exit 1
        }
        model="$2"
        shift 2
        ;;
      --effort)
        [ -n "${2:-}" ] || {
          echo "crew: --effort needs a value" >&2
          exit 1
        }
        effort="$2"
        shift 2
        ;;
      --plan)
        [ -n "${2:-}" ] || {
          echo "crew: --plan needs a value" >&2
          exit 1
        }
        plan="$2"
        shift 2
        ;;
      --mcp)
        [ -n "${2:-}" ] || {
          echo "crew: --mcp needs a value" >&2
          exit 1
        }
        mcp="$2"
        shift 2
        ;;
      --draft)
        draft=true
        shift
        ;;
      --shape)
        [ -n "${2:-}" ] || {
          echo "crew: --shape needs a value" >&2
          exit 1
        }
        shape="$2"
        shift 2
        ;;
      --spec)
        [ -n "${2:-}" ] || {
          echo "crew: --spec needs a value" >&2
          exit 1
        }
        spec="$2"
        shift 2
        ;;
      --crew)
        [ -n "${2:-}" ] || {
          echo "crew: --crew needs a value" >&2
          exit 1
        }
        hcrew="$2"
        shift 2
        ;;
      -*)
        echo "crew: hold add: unknown arg '$1'" >&2
        exit 1
        ;;
      *)
        break
        ;;
      esac
    done
    title="$*"
    # Reject a missing required field by name, the way `status` rejects an
    # unknown state (crew.sh:366) — fail loudly at the writer, not downstream.
    [ -n "$engine" ] || {
      echo "crew: hold add: --engine is required" >&2
      exit 1
    }
    [ -n "$window" ] || {
      echo "crew: hold add: --window is required" >&2
      exit 1
    }
    [ -n "$resets_at" ] || {
      echo "crew: hold add: --resets-at is required" >&2
      exit 1
    }
    [ -n "$agent" ] || {
      echo "crew: hold add: --agent is required" >&2
      exit 1
    }
    [ -n "$ref" ] || {
      echo "crew: hold add: --ref is required" >&2
      exit 1
    }
    [ -n "$branch" ] || {
      echo "crew: hold add: --branch is required" >&2
      exit 1
    }
    [ -n "$tier" ] || {
      echo "crew: hold add: --tier is required" >&2
      exit 1
    }
    [ -n "$model" ] || {
      echo "crew: hold add: --model is required" >&2
      exit 1
    }
    [ -n "$effort" ] || {
      echo "crew: hold add: --effort is required" >&2
      exit 1
    }
    [ -n "$title" ] || {
      echo "crew: hold add: a title is required" >&2
      exit 1
    }
    # Shape only, never the floor — `refresh-budget.sh` alone owns
    # the 85%/95% judgment. Seconds here, matching `resets_at`'s own unit;
    # `id`/`ts` below are milliseconds, a different clock.
    case "$resets_at" in '' | *[!0-9]*)
      echo "crew: hold add: --resets-at must be an integer epoch-seconds timestamp" >&2
      exit 1
      ;;
    esac
    now=$(_clock_now)
    [ "$resets_at" -gt "$now" ] || {
      echo "crew: hold add: --resets-at must be in the future" >&2
      exit 1
    }
    if [ -n "$spec" ] && [ ! -r "$spec" ]; then
      echo "crew: hold add: --spec file '$spec' is not readable" >&2
      exit 1
    fi
    crew=$(_hold_crew "$hcrew")
    # setup_repo (tests/helpers.bash:13-28) creates a bare repo with no
    # .git/crew, so a `hold add` with no --spec would otherwise fail its
    # append — mirrors `status`/`msg`'s own mkdir -p (crew.sh:357).
    mkdir -p "$dir"
    # Computed once, before the builder: `_fit_line` calls the builder
    # repeatedly while shrinking the title, and an `id` minted inside it would
    # stop matching its own row's `ts` on any shrunk record.
    # Milliseconds, not `crew new`'s seconds (crew.sh:311): the duplicate guard
    # compares `id` against the ms `ts` fields dispatch.sh writes at :603 and
    # :1017, so copying `crew new` breaks that comparison by 1000x.
    ts=$(jq -nc 'now*1000|floor')
    id="$ts-$$"
    specpath=""
    if [ -n "$spec" ]; then
      mkdir -p "$dir/holds"
      specpath="$dir/holds/$id.md"
      # `--`: a --spec whose basename starts with a dash would otherwise be
      # parsed by cp as a flag bundle, long after `[ -r ]` accepted it.
      cp -- "$spec" "$specpath"
    fi
    # Mirrors `_build_status` exactly: every fixed field closes over the
    # enclosing scope via --arg/--argjson, and $1 is the only shrinkable part
    # (crew.sh:384-392). Handing `_shrink` the whole body would truncate `id`,
    # `task.branch` and `task.spec` along with the title.
    _build_hold() {
      jq -nc --arg crew "$crew" --arg id "$id" --argjson ts "$ts" \
        --arg wengine "$engine" --arg window "$window" --argjson resets_at "$resets_at" \
        --arg tengine "$agent" --arg ref "$ref" --arg branch "$branch" \
        --arg tier "$tier" --arg model "$model" --arg effort "$effort" \
        --arg plan "$plan" --arg mcp "$mcp" --argjson draft "$draft" \
        --arg shape "$shape" --arg spec "$specpath" --arg title "$1" \
        '{ts: $ts, crew_id: $crew, from: ("dispatcher:" + $crew),
          to: ("hold:" + $crew), kind: "msg",
          body: ({id: $id,
                  wait: {engine: $wengine, window: $window, resets_at: $resets_at},
                  task: {ref: $ref, branch: $branch, tier: $tier, engine: $tengine,
                         model: $model, effort: $effort,
                         plan: (if $plan == "" then null else $plan end),
                         mcp: (if $mcp == "" then null else $mcp end),
                         draft: $draft,
                         shape: (if $shape == "" then null else $shape end),
                         title: $title,
                         spec: (if $spec == "" then null else $spec end)}}
                 | tostring)}'
    }
    line=$(_fit_line _build_hold "$title")
    _bus_append "$log" "$line"
    # Prints only the minted id, so a caller can `release <id>` later or build
    # test assertions without going through `list --json`.
    printf '%s\n' "$id"
    ;;
  list)
    json=false
    hcrew=""
    while [ $# -gt 0 ]; do
      case "$1" in
      --json)
        json=true
        shift
        ;;
      --crew)
        [ -n "${2:-}" ] || {
          echo "crew: --crew needs a value" >&2
          exit 1
        }
        hcrew="$2"
        shift 2
        ;;
      *)
        echo "crew: hold list [--crew ID] [--json]" >&2
        exit 1
        ;;
      esac
    done
    crew=$(_hold_crew "$hcrew")
    holds=$(_hold_outstanding "$crew")
    if [ "$json" = true ]; then
      printf '%s\n' "$holds"
    else
      printf '%s' "$holds" | _hold_render
    fi
    ;;
  due)
    json=false
    hcrew=""
    while [ $# -gt 0 ]; do
      case "$1" in
      --json)
        json=true
        shift
        ;;
      --crew)
        [ -n "${2:-}" ] || {
          echo "crew: --crew needs a value" >&2
          exit 1
        }
        hcrew="$2"
        shift 2
        ;;
      *)
        echo "crew: hold due [--crew ID] [--json]" >&2
        exit 1
        ;;
      esac
    done
    crew=$(_hold_crew "$hcrew")
    # Matured is `<=`, not `<`. `_clock_now_f` is seconds, compared against
    # `wait.resets_at`, never the `now*1000` idiom `ts`/`id` use above.
    matured=$(_hold_outstanding "$crew" | jq -c --argjson now "$(_clock_now_f)" '[.[] | select(.wait.resets_at <= $now)]')
    n=$(printf '%s' "$matured" | jq 'length')
    if [ "$json" = true ]; then
      printf '%s\n' "$matured"
    else
      printf '%s' "$matured" | _hold_render
    fi
    # Exits 0 when any hold is matured, 1 when none — including a missing log,
    # since `_hold_outstanding` already prints `[]` for that case.
    if [ "$n" -gt 0 ]; then
      exit 0
    else
      exit 1
    fi
    ;;
  park)
    default="${1:-}"
    shift || true
    case "$default" in '' | *[!0-9]*)
      echo "crew: hold park <default> must be a positive integer number of seconds" >&2
      exit 1
      ;;
    esac
    [ "$default" -gt 0 ] || {
      echo "crew: hold park <default> must be a positive integer number of seconds" >&2
      exit 1
    }
    hcrew=""
    while [ $# -gt 0 ]; do
      case "$1" in
      --crew)
        [ -n "${2:-}" ] || {
          echo "crew: --crew needs a value" >&2
          exit 1
        }
        hcrew="$2"
        shift 2
        ;;
      *)
        echo "crew: hold park <default> [--crew ID]" >&2
        exit 1
        ;;
      esac
    done
    crew=$(_hold_crew "$hcrew")
    # The branch default when nothing is outstanding or the earliest is
    # already matured; otherwise min(default, earliest - now). Never below 1
    # — `crew watch` rejects `--timeout 0` (crew.sh:869-872) and a 0 here
    # would fail the cursor re-arm.
    _hold_outstanding "$crew" | jq -r --argjson default "$default" --argjson now "$(_clock_now_f)" '
      ([.[] | .wait.resets_at] | min) as $earliest
      | (if ($earliest == null or $earliest <= $now) then $default
         else ([$default, ($earliest - $now)] | min) end) as $raw
      | ([$raw, 1] | max) | floor'
    ;;
  release)
    hid="${1:-}"
    shift || true
    [ -n "$hid" ] || {
      echo "crew: hold release <id> [--crew ID]" >&2
      exit 1
    }
    hcrew=""
    while [ $# -gt 0 ]; do
      case "$1" in
      --crew)
        [ -n "${2:-}" ] || {
          echo "crew: --crew needs a value" >&2
          exit 1
        }
        hcrew="$2"
        shift 2
        ;;
      *)
        echo "crew: hold release <id> [--crew ID]" >&2
        exit 1
        ;;
      esac
    done
    crew=$(_hold_crew "$hcrew")
    mkdir -p "$dir"
    # Always appended — the bus is append-only (crew.sh:191) — so an unknown
    # or already-released id is a no-op by construction: `_hold_outstanding`
    # excludes any id with a matching release, however many it finds.
    _build_hold_release() {
      jq -nc --arg crew "$crew" --arg id "$hid" --arg body "$1" \
        '{ts: (now*1000|floor), crew_id: $crew, from: ("dispatcher:" + $crew),
          to: ("hold:" + $crew), kind: "msg", body: $body}'
    }
    relbody=$(jq -nc --arg id "$hid" '{id: $id, released: true}')
    line=$(_fit_line _build_hold_release "$relbody")
    _bus_append "$log" "$line"
    ;;
  *)
    echo "crew: hold add|list|due|park|release" >&2
    exit 1
    ;;
  esac
  ;;
stall-watch)
  # stall-watch <worker-id|branch|role:branch:role> --pane <id> [--engine E] [--grace S] [--stall S]
  #   [--window S] [--interval S] [--idle S] [--dead S] [--max-life S] [--load S] [--release S]
  #   [--no-budget] [--budget-refresh S]
  #
  # Lifetime-scoped liveness watchdog, spawned per worker by `dispatch`. The bus
  # reflects only what a worker POSTS, so a worker parked on an interactive
  # prompt, or whose turn died mid-task, is indistinguishable from one that is
  # working (#31). Seven pane detectors read one capture per tick (D6 reads the
  # bus instead, D8 the engine budget cache):
  #   D0 stalled:    static pane inside the startup --window whose frame is NOT a prompt
  #   D1 prompt:     prompt frame at the verified geometry, no meter, 2 samples
  #                  (quota: is D1's own content discriminator on the SAME
  #                  geometry — see _is_quota_prompt)
  #   D1b quota:     quota refusal frame — claude's session-limit refusal or
  #                  cursor's monthly usage limit; normal status bar, no
  #                  option-select prompt, 2 samples (see _is_quota_session_limit
  #                  and _is_quota_cursor_limit)
  #   D2 turn-stall: meter clock advancing, token string static, no live subagent row
  #   D3 quiet:      byte-identical pane for --idle
  #   D4 load:       host 1m load above the core count for --load seconds —
  #                  engine-independent, and it never escalates
  #   D5 stalled:    pane still a bare shell --launch seconds in and no engine
  #                  ever seen there (`stalled: launch-not-started`, #342);
  #                  clears when the engine appears
  #   D7 runaway:    a model sentinel token (`<｜end▁of▁thinking｜>`, `<|im_end|>`,
  #                  …) leaked into assistant prose — outside any tool-output
  #                  block — for --runaway-hits samples while the output-token
  #                  count grew by --runaway-tokens (#650). claude and pi only.
  #                  Never escalates: the turn is unrecoverable but the worktree
  #                  usually isn't, so verify the pane, then kill and re-dispatch.
  #   D6 unread:     lead `working` while a role:<branch>:* or dispatcher:<crew>
  #                  msg to its session sits past the delivered mark for
  #                  --unread (#330); clears once delivered. Repainting panes
  #                  never trip D2/D3, so this reads the bus (bounded tail,
  #                  4-tick cadence). Never escalates.
  #                  A sessioned claude/pi watchdog first auto-nudges an idle
  #                  lead past an overdue dispatcher directive (_nudge_pane,
  #                  once per newest directive); --no-nudge turns it off.
  #   D8 budget:     the engine has a quota window at >=95% that has not reset,
  #                  or its limit_reached holds, per refresh-budget's
  #                  engine-budget.json read through the launch gate's own
  #                  predicate (budget-gate.sh). The worker keeps running; the
  #                  dispatcher decides. Clears only on a fresh cache that reads
  #                  clear — a stale or blind one holds. Never escalates.
  #                  --no-budget turns it off (dispatch's --ignore-budget).
  # Every detector posts `blocked` — recoverable, answerable, and cheap to be
  # wrong about. Only quiet:/turn-stall: episodes escalate to `failed`, and only
  # after a second evidence check --dead later; a prompt still on screen is
  # evidence that nobody answered, not that the worker died, so it never
  # escalates (quota: never escalates either — same reasoning, opposite
  # recovery: stop dispatching, don't answer). quiet:'s escalation additionally
  # requires the pane's engine process to be gone (see _pane_engine_alive);
  # turn-stall:'s does not. Engine signatures are the data table below: claude
  # only, because a guessed signature is a false-positive generator.
  # CREW_STALL_SAMPLE_CMD overrides the sampler (stdout = pane text, exit code =
  # pane liveness) and CREW_STALL_PROC_CMD overrides the engine-liveness check
  # (see _pane_engine_alive), so the loop is testable without tmux.
  # Finished-worker release: once the worker's own latest word is `done`/`failed`
  # and --release seconds (default $release_grace, 0 = off) have passed, release its
  # window through reap's _release_windows — no stream or reap needed. Claude
  # panes must also show a provably idle frame on two consecutive ticks (a
  # live turn, prompt, unsent input or background shell keeps the window);
  # other engines have no idle signature, so their frame must be unchanged for
  # --release and show no prompt. A watchdog-posted `failed` (hung pane) is never
  # released here — it waits for --max-life or a reap. The worktree is never touched.
  # CREW_STALL_COLOR_CMD overrides the colored capture like CREW_STALL_SAMPLE_CMD.
  # D0/D3 stay silent (claude only) while a finished turn waits on a background
  # shell, for at most --bg-wait (default 2h) of unchanged frame.
  # role:<branch>:<role> selects prompt-only mode: a parked role pane
  # legitimately sits static, so only D1/D1b (claude) and D8 run; D0/D2-D7 and the dead:
  # escalation are off, and a role-mode-only check exits the watch once the
  # pane's engine returns to a bare shell.
  arg="${1:-}"
  shift || true
  [ -n "$arg" ] || {
    echo "crew: stall-watch <worker-id|branch|role:branch:role> --pane <id> [--engine E] [--grace S] [--stall S] [--window S] [--interval S] [--idle S] [--dead S] [--max-life S] [--load S] [--release S] [--bg-wait S] [--launch S] [--unread S] [--runaway-hits N] [--runaway-tokens N] [--no-budget] [--budget-refresh S] [--no-nudge]" >&2
    exit 1
  }
  # INV-W0 — identity is branch-keyed and suffix-tolerant. dispatch has shipped
  # BOTH `worker:<branch>#s<session>` and a bare `<branch>`, and a watchdog
  # cannot know which one launched it. Under exact-string matching every safety
  # check below silently no-ops and the roster splits into two rows for one
  # worker (measured: EVIDENCE-2026-08-10.txt). Reads stay branch-keyed; writes
  # carry the invoking session id, because a bare `from` becomes a session-less
  # row that strands a branch-only `reply` (#173). Only a session suffix is
  # stripped, so a branch containing '#' keys the same here and in _sessions.
  # A sessioned watchdog owns its pane only until a newer session posts on the
  # branch: resume reuses the pane and starts a second watchdog without killing
  # this one, and a killed engine posts no terminal state to stop it. Left
  # running it samples the successor's pane and posts under the dead session
  # id, which then reads as the branch's newest session and captures `reply`.
  # role: selects prompt-only mode: a role pane's own stall-watch, keyed
  # to that role's own bus id rather than the worker's, so its posts never
  # touch the worker row or the lead's watchdog episode state.
  case "$arg" in
  role:*)
    role_mode=1
    from_id="$arg"
    me="$arg"
    branch="${arg#role:}"
    branch="${branch%:*}"
    ;;
  *)
    role_mode=0
    id="worker:${arg#worker:}"
    if _is_session_id "$id"; then
      from_id="$id"
      branch="${id%#*}"
      branch="${branch#worker:}"
    else
      branch="${id#worker:}"
      from_id="worker:$branch"
    fi
    me="worker:$branch"
    ;;
  esac
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  pane=""
  engine="unknown"
  grace=45
  stall=300
  window=900
  interval=15
  idle=1800
  dead=1800
  max_life=43200
  load_win=300
  release=$release_grace
  bg_wait=7200
  launch=150
  unread=600
  runaway_hits=3
  runaway_tokens=1500
  no_budget=0
  budget_refresh=900
  nudge_on=1
  host_cores=$(nproc 2>/dev/null || echo 1)
  while [ $# -gt 0 ]; do
    case "$1" in
    --pane)
      pane="${2:-}"
      shift 2
      ;;
    --engine)
      engine="${2:-}"
      shift 2
      ;;
    --grace)
      grace="${2:-}"
      shift 2
      ;;
    --stall)
      stall="${2:-}"
      shift 2
      ;;
    --window)
      window="${2:-}"
      shift 2
      ;;
    --interval)
      interval="${2:-}"
      shift 2
      ;;
    --idle)
      idle="${2:-}"
      shift 2
      ;;
    --dead)
      dead="${2:-}"
      shift 2
      ;;
    --max-life)
      max_life="${2:-}"
      shift 2
      ;;
    --load)
      load_win="${2:-}"
      shift 2
      ;;
    --release)
      release="${2:-}"
      shift 2
      ;;
    --bg-wait)
      bg_wait="${2:-}"
      shift 2
      ;;
    --launch)
      launch="${2:-}"
      shift 2
      ;;
    --unread)
      unread="${2:-}"
      shift 2
      ;;
    --runaway-hits)
      runaway_hits="${2:-}"
      shift 2
      ;;
    --runaway-tokens)
      runaway_tokens="${2:-}"
      shift 2
      ;;
    --no-budget)
      no_budget=1
      shift
      ;;
    --budget-refresh)
      budget_refresh="${2:-}"
      shift 2
      ;;
    --no-nudge)
      nudge_on=0
      shift
      ;;
    *)
      echo "crew: stall-watch: unknown arg '$1'" >&2
      exit 1
      ;;
    esac
  done
  [ -n "$pane" ] || {
    echo "crew: stall-watch needs --pane <id>" >&2
    exit 1
  }
  case "$release" in '' | *[!0-9]*)
    echo "crew: stall-watch: --release must be a non-negative integer number of seconds" >&2
    exit 1
    ;;
  esac
  case "$budget_refresh" in '' | *[!0-9]*)
    echo "crew: stall-watch: --budget-refresh must be a non-negative integer number of seconds" >&2
    exit 1
    ;;
  esac
  # Signature table. Enabling an engine is DATA, not logic: capture its prompt
  # and meter frames on a work-profile host, pin them as fixtures, add a row.
  # Codex's single verified hook-review frame is deliberately narrower than
  # Claude's prompt geometry. cursor gets frame-free detectors (D0/D3) plus its
  # one verified refusal frame (D1b, see _is_quota_cursor_limit) — a cursor
  # footer that permanently contained an `Enter to select`-like string would pin
  # every cursor worker at `blocked` forever, so its prompt signatures stay off.
  case "$engine" in
  claude)
    sig_prompt=1
    sig_meter=1
    sig_session_limit=1
    sig_cursor_limit=0
    sig_bgwait=1
    sig_runaway=1
    ;;
  pi)
    sig_prompt=0
    sig_meter=0
    sig_session_limit=0
    sig_cursor_limit=0
    sig_bgwait=0
    sig_runaway=1
    ;;
  codex)
    sig_prompt=1
    sig_meter=0
    sig_session_limit=0
    sig_cursor_limit=0
    sig_bgwait=0
    sig_runaway=0
    ;;
  cursor)
    sig_prompt=0
    sig_meter=0
    sig_session_limit=0
    sig_cursor_limit=1
    sig_bgwait=0
    sig_runaway=0
    ;;
  *)
    sig_prompt=0
    sig_meter=0
    sig_session_limit=0
    sig_cursor_limit=0
    sig_bgwait=0
    sig_runaway=0
    ;;
  esac
  # Every role engine gets a watcher for D8's sake, but the prompt detectors
  # stay claude-only in role mode: no other engine's role pane ever had one,
  # and its frames were never verified against a parked role.
  if [ "$role_mode" = 1 ] && [ "$engine" != claude ]; then
    sig_prompt=0
    sig_session_limit=0
    sig_cursor_limit=0
  fi
  _frame_classifier

  # shellcheck source=/dev/null
  . "${BUDGET_GATE_LIB:-@budgetGateLib@}"
  budget_file="${XDG_DATA_HOME:-$HOME/.local/share}/crew/engine-budget.json"
  # Decided once: the engine and the pane's model are fixed for the watch.
  budget_on=0
  if [ "$no_budget" = 0 ]; then
    case "$engine" in
    claude | codex | cursor) budget_on=1 ;;
    pi)
      budget_on=1
      # A localModels id spends no OpenRouter quota (the launch gate skips it
      # too). Any failure to tell keeps D8 on.
      budget_model=$(tmux show-options -pqv -t "$pane" @crew_model 2>/dev/null || true)
      if [ -n "$budget_model" ]; then
        # shellcheck source=/dev/null
        . "${LOCAL_MODELS_LIB:-@localModelsLib@}"
        if [ -n "$(_local_entry "$("${DISPATCH_CONFIG_BIN:-dispatch-config}" 2>/dev/null)" "$budget_model" 2>/dev/null)" ]; then
          budget_on=0
        fi
      fi
      ;;
    esac
  fi

  # A non-claude role watch has only D8 and its own end-of-life exit live: the
  # prompt signatures stay claude-only, D0/D2-D7 and the dead: escalation are
  # worker mode. With the budget detector off as well nothing is left to detect,
  # and @crew_state for the pane belongs to dispatch's --role-watch.
  if [ "$role_mode" = 1 ] && [ "$budget_on" = 0 ] && [ "$engine" != claude ]; then
    exit 0
  fi

  # _budget_last_try — the later of the cache's fetched_epoch and the last
  # refresh attempt's stamp; an unreadable value counts as never.
  _budget_last_try() {
    local f s
    f=$(jq -r '.fetched_epoch | if type == "number" then floor else 0 end' "$budget_file" 2>/dev/null || true)
    s=$(cat "$budget_file.refresh-at" 2>/dev/null || true)
    # Base 10 forced: a stamp like `09` would read as bad octal in $((…)).
    if [[ "$f" =~ ^[0-9]{1,12}$ ]]; then f=$((10#$f)); else f=0; fi
    if [[ "$s" =~ ^[0-9]{1,12}$ ]]; then s=$((10#$s)); else s=0; fi
    if [ "$f" -ge "$s" ]; then echo "$f"; else echo "$s"; fi
  }

  # _budget_refresh_maybe <now> — run refresh-budget when the cache is
  # --budget-refresh old. Otherwise only a human or the dispatcher's session
  # start refreshes it, so a window crossed mid-run would go unseen until the
  # gates read it as stale. Single flight host-wide (the lock) plus the attempt
  # stamp: N watchers cost one probe per interval, and a failing probe backs off
  # the full interval instead of being retried every minute by every watcher.
  # Synchronous on purpose — one tick slipping <=120s once per interval is far
  # inside every detector threshold.
  # Returns 0 only when the probe ran: the caller re-reads the bus after a real
  # refresh, the one thing that can hold the tick up to 120s.
  _budget_refresh_maybe() {
    local last
    [ "$budget_refresh" != 0 ] || return 1
    last=$(_budget_last_try)
    [ $(($1 - last)) -ge "$budget_refresh" ] || return 1
    mkdir -p "${budget_file%/*}"
    _lock_acquire "$budget_file.refresh.d" "$$" || return 1
    last=$(_budget_last_try)
    if [ $(($1 - last)) -lt "$budget_refresh" ]; then
      _lock_release "$budget_file.refresh.d"
      return 1
    fi
    printf '%s\n' "$1" >"$budget_file.refresh-at.$$"
    mv -f "$budget_file.refresh-at.$$" "$budget_file.refresh-at"
    if [ -n "${CREW_BUDGET_REFRESH_CMD:-}" ]; then
      eval "$CREW_BUDGET_REFRESH_CMD" >/dev/null 2>&1 || true
    elif command -v refresh-budget >/dev/null 2>&1; then
      env -u CREW_WORKER_ID -u CREW_ID timeout 120 refresh-budget >/dev/null 2>&1 || true
    fi
    _lock_release "$budget_file.refresh.d"
    return 0
  }

  # _budget_reltime <secs> — mirrors refresh-budget's `reltime` jq def, so the
  # watchdog row and the budget report word a reset alike.
  _budget_reltime() {
    local d=$(($1 / 86400)) h=$(($1 % 86400 / 3600)) m=$(($1 % 3600 / 60))
    if [ "$d" -gt 0 ]; then
      echo "${d}d ${h}h"
    elif [ "$h" -gt 0 ]; then
      echo "${h}h ${m}m"
    else
      echo "${m}m"
    fi
  }

  # _budget_reset_note <resets> <iso> <now> — " (resets <iso>, in <rel>)". jq
  # can hand back a float or an exponent literal (`1E+10`), so anything but a
  # plain future epoch drops the relative part rather than break the arithmetic.
  _budget_reset_note() {
    local r="${1%%.*}"
    if [[ "$r" =~ ^[0-9]{1,12}$ ]] && [ $((10#$r)) -gt "$3" ]; then
      printf ' (resets %s, in %s)' "$2" "$(_budget_reltime $((10#$r - $3)))"
    else
      printf ' (resets %s)' "$2"
    fi
  }

  # _budget_detail <now> — the budget: detail for the first gating window, else
  # the limit. Returns 0 exhausted (detail printed), 1 clear, 2 can't tell.
  _budget_detail() {
    local win lim wrc lrc key pct reason resets iso detail
    win=$(_budget_windows "$budget_file" "$engine" "$1") && wrc=0 || wrc=$?
    lim=$(_budget_limit "$budget_file" "$engine" "$1") && lrc=0 || lrc=$?
    if [ "$wrc" = 0 ]; then
      IFS=$'\t' read -r key pct resets iso <<<"${win%%$'\n'*}"
      detail="budget: $engine $key at $pct%"
      if [ -n "$resets" ]; then
        detail="$detail$(_budget_reset_note "$resets" "$iso" "$1")"
      else
        detail="$detail (no reset time)"
      fi
    elif [ "$lrc" = 0 ]; then
      IFS=$'\t' read -r reason resets iso <<<"${lim%%$'\n'*}"
      detail="budget: $engine limit reached: $reason"
      [ -z "$resets" ] || detail="$detail$(_budget_reset_note "$resets" "$iso" "$1")"
    elif [ "$wrc" = 2 ] || [ "$lrc" = 2 ]; then
      return 2
    else
      return 1
    fi
    if jq -e --arg e "$engine" '.engines[$e].credits_cover == true' "$budget_file" >/dev/null 2>&1; then
      detail="$detail — credits cover: may be drawing paid credits"
    fi
    printf '%s\n' "$detail"
  }

  # Leaked model sentinels: DeepSeek `<｜…｜>` (fullwidth bars), ChatML/GPT
  # `<|…|>` with a known token name.
  re_sentinel='<｜[^｜>]{1,40}｜>|<\|(im_start|im_end|endoftext|eot_id|start_header_id|end_header_id)\|>'

  # _runaway_prose <text> — the bottom of the frame with tool output removed, so
  # a worker legitimately reading or editing text that contains a sentinel (this
  # very detector, a fixture) never matches. claude: a `⎿` result row and its
  # deeper-indented continuation, until the next `⏺` row; pi: a `Tool output`
  # row until the next blank line.
  _runaway_prose() {
    printf '%s\n' "$1" | tail -60 | awk '
      function flush(  i) { for (i = 1; i <= n; i++) print buf[i]; n = 0 }
      /^[[:space:]]*⏺/ { skip = 0; n = 0 }
      /^[[:space:]]*⏺[[:space:]]+[A-Za-z_]+\(/ { next }
      /^[[:space:]]*(>|❯)/ { next }
      /^[[:space:]]*⎿/ { skip = 1; next }
      /^[[:space:]]*Tool output/ { skip = 2; next }
      skip == 1 && (/^[[:space:]]*$/ || /^     /) { next }
      skip == 2 && (/^[[:space:]]*$/ || /^  /) { next }
      { skip = 0; buf[++n] = $0 }
      END { flush() }'
  }

  # _out_tokens <text> — output-token count as an integer: the last `↓ N[kM]`
  # in the bottom lines (claude's live meter, pi's footer). Empty when absent.
  _out_tokens() {
    printf '%s\n' "$1" | tail -12 | grep -oE '↓ ?[0-9.]+[kM]?' | tail -1 |
      awk '{ sub(/^↓ ?/, ""); n=$0; m=1; if (n ~ /k$/) m=1000; else if (n ~ /M$/) m=1000000; sub(/[kM]$/, "", n); printf "%d", n * m }' || true
  }

  # Raw pane text on stdout; non-zero when the pane is gone. The CALLER hashes:
  # D0/D3 read the hash, D1/D2 read the text.
  _sample() { _pane_capture "$pane"; }

  _sample_colored() { _pane_capture "$pane" colored; }

  # _pane_engine_alive — corroborating evidence for quiet:'s dead: escalation.
  # The trade-off: a turn that hangs while the engine process stays resident
  # (D2's subrow blind spot, documented above, is one way) can now never reach
  # dead: via quiet: — only a vanished process does.
  # CREW_STALL_PROC_CMD overrides this the same way CREW_STALL_SAMPLE_CMD
  # overrides _sample, so the loop stays testable without tmux.
  _pane_cmd() {
    local cmd panes pid pcmd
    if [ -n "${CREW_STALL_PROC_CMD:-}" ]; then
      cmd=$(eval "$CREW_STALL_PROC_CMD" 2>/dev/null || true)
    else
      cmd=""
      panes=$(tmux list-panes -a -F $'#{pane_id}\t#{pane_current_command}' 2>/dev/null || true)
      while IFS=$'\t' read -r pid pcmd; do
        [ "$pid" = "$pane" ] || continue
        cmd="$pcmd"
        break
      done <<PANES
$panes
PANES
    fi
    printf '%s' "$cmd"
  }
  _pane_engine_alive() {
    local cmd
    cmd=$(_pane_cmd)
    [ -n "$cmd" ] && _is_engine_cmd "$cmd"
  }
  # Only a bare interactive shell counts as "launch not started": an empty or
  # unknown command (tmux hiccup, exotic wrapper) must never raise D5.
  _is_shell_cmd() {
    case "${1#-}" in
    bash | sh | zsh | fish | dash | ksh) return 0 ;;
    esac
    return 1
  }

  # _load_read — emit ONE line "load1 nproc" (the D4 parse contract). The
  # override supplies both numbers; the default computes host_cores once (B1)
  # and reads the 1-minute average from /proc/loadavg.
  _load_read() {
    if [ -n "${CREW_STALL_LOAD_CMD:-}" ]; then
      eval "$CREW_STALL_LOAD_CMD" 2>/dev/null || true
    else
      awk -v n="$host_cores" '{print $1, n}' /proc/loadavg 2>/dev/null || true
    fi
  }

  # _top_consumers — up to two preformatted "cwd-tail comm pid pcpu%" lines,
  # read only at post time (never per tick). The sed collapses each cwd to its
  # last two path segments so the comm discriminator AND the pid (the kill
  # target the dispatcher's load: bullet names) survive the roster's 120-char
  # cut; a cwd of "/" or an unreadable cwd stays as ps reports it.
  _top_consumers() {
    if [ -n "${CREW_STALL_TOP_CMD:-}" ]; then
      eval "$CREW_STALL_TOP_CMD" 2>/dev/null || true
    else
      ps -eo cwd=,comm=,pid=,pcpu= --sort=-pcpu 2>/dev/null \
        | sed -E 's#.*/([^/]+/[^/]+) #\1 #' | head -2 || true
    fi
  }

  # C-3 — every bus read is scoped to THIS run. events.jsonl is append-only per
  # repo and re-dispatch onto the same branch is a first-class flow, so an
  # unscoped read lets the PREVIOUS run's failed/exited mute a freshly started
  # watchdog for its entire life, silently, at exit 0. Since the documented
  # recovery for every watchdog event is "kill and re-dispatch", that would
  # guarantee the run after any watchdog event has no watchdog at all.
  # One second of slack because `date +%s` truncates while bus timestamps are
  # ms: without it a status the launcher posted a fraction of a second before
  # this process started sorts below the cutoff and reads as a previous run.
  # Previous runs are minutes away, so the slack cannot reach one.
  run_start_ms=$((($(_clock_now) - 1) * 1000))
  own_epoch=""
  if [ "$from_id" != "$me" ]; then
    own_epoch="${from_id##*#s}"
    own_epoch="${own_epoch%-*}"
  fi
  bus_ts=0
  bus_state=""
  bus_source=""
  bus_detail=""
  # Step-aside (see INV-W0): only a session whose epoch is not older than ours
  # counts, so a straggler post from the predecessor cannot disarm the watchdog
  # that now owns the pane.
  _bus_refresh() {
    local rows l
    bus_ts=0
    bus_state=""
    bus_source=""
    bus_detail=""
    [ -f "$log" ] || return 0
    rows=$(jq -r --arg c "$crew" --arg m "$me" --arg f "$from_id" \
      --arg e0 "$own_epoch" --argjson t0 "$run_start_ms" '
        select(.crew_id==$c and .kind=="status" and .ts>=$t0
               and (.from==$m or (.from|startswith($m+"#"))))
        | ([.from | capture("^(?<w>.*)#s(?<e>[0-9]+)-[0-9]+$")] | first) as $sid
        | [(if $e0 != "" and .from != $f and $sid != null and $sid.w == $m
               and ($sid.e|tonumber) >= ($e0|tonumber) then "1" else "0" end),
           (.ts|tostring), .body.state, (.body.source // ""), (.body.detail // "")]
        | @tsv' "$log" 2>/dev/null || true)
    [ -n "$rows" ] || return 0
    case $'\n'"$rows" in
    *$'\n1\t'*) exit 0 ;;
    esac
    l="${rows##*$'\n'}"
    IFS=$'\t' read -r _ bus_ts bus_state bus_source bus_detail <<BUSLINE
$l
BUSLINE
    return 0
  }

  # INV-W1 — the watchdog is a subordinate writer. Re-read the bus immediately
  # before EVERY append (the hazard lives in the gap between sampling the pane
  # and writing the line) and abort if the worker has finished. Exempt from the
  # read cadence below on purpose: appends are rare, staleness here is a lie.
  _post() { # _post <state> <detail>
    _bus_refresh
    case "$bus_state" in
    done | failed | exited) exit 0 ;;
    esac
    mkdir -p "$dir"
    local line
    line=$(jq -nc --arg crew "$crew" --arg from "$from_id" --arg state "$1" --arg detail "$2" \
      '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
          kind:"status", body:{state:$state, detail:$detail, source:"watchdog"}}')
    _bus_append "$log" "$line"
    # Only a blocked state carries the watchdog marker, so a `_post_clear`
    # recovery (`working … cleared`) cannot render `working (watchdog)`.
    # Role mode skips this: --role-watch owns @crew_state for a role pane.
    if [ "$role_mode" = 0 ]; then
      if [ "$1" = blocked ]; then
        _publish_pane_state "$pane" "$1" "$2" watchdog
      else
        _publish_pane_state "$pane" "$1" "$2"
      fi
    fi
  }

  # INV-W3 — one open watchdog episode per branch PER PREFIX, checked on the bus
  # (the only thing two watchdogs on one branch share) rather than in process
  # memory. Same-prefix, not any-prefix, on purpose: a dead pane must still be
  # able to raise `quiet:` over an open `stalled:`, because `stalled:` does not
  # escalate (C-1) and `quiet:` is then the only path that frees fan-out budget.
  _post_blocked() { # _post_blocked <prefix> <detail>; returns 1 when suppressed
    _bus_refresh
    if [ "$bus_state" = blocked ] && [ "$bus_source" = watchdog ]; then
      case "$bus_detail" in
      "$1"*) return 1 ;;
      # C-1's other half: an open `prompt:` is sticky, un-supersedable by any
      # other prefix. Overwriting it with `quiet:` — which a frozen prompt frame
      # satisfies by construction — would hand the unanswered prompt an
      # escalation path `prompt:` itself is forbidden to have. `quota:` gets the
      # same protection: a quota-exhausted pane is static for hours (far past
      # --idle), so without this a D3 `quiet:` would eventually overwrite the
      # one signal telling the dispatcher to stop dispatching entirely.
      prompt:*) return 1 ;;
      quota:*) return 1 ;;
      esac
    fi
    _post blocked "$2"
  }

  # INV-W2 — a clearance never overwrites a later worker statement. `working` is
  # not in `watch`'s wake set, so this corrects the roster without a second wake.
  # Own-prefix only: under the any-prefix override another detector's episode may
  # be the live one, and clearing it would reset the roster to `working` with no
  # wake, leaving its later `failed` with no visible `blocked` to explain it.
  _post_clear() { # _post_clear <prefix>
    _bus_refresh
    if [ "$bus_state" = blocked ] && [ "$bus_source" = watchdog ]; then
      case "$bus_detail" in
      "$1"*) _post working "$1 cleared" ;;
      esac
    fi
  }

  # _permission_detail <text> <pane> — human-relay payload for a detected
  # tool-permission dialog (#435). The payload is pane-scraped and therefore
  # attacker-influenceable, so it is STRIPPED before it reaches the bus: ESC/CSI
  # sequences and control characters removed, folded to one line. The trailing
  # ` — pane <id>` is never truncated (recovery step 1 needs it), so only the
  # `permission — <tool>: <request>` prefix is capped, keeping the whole detail
  # at 160 characters. A frame whose tool/request cannot be parsed still fires,
  # with `(unparsed)`. The header is the LAST `· from the … agent` line in the
  # capture, not the first: a prior permission dialog scrolled into the
  # transcript must not make the relay report its stale tool/request while the
  # live dialog is the one parked at the bottom (footer-last geometry).
  _permission_detail() {
    local pane="$2" clean suffix body max
    clean=$(printf '%s\n' "$1" |
      sed -E $'s/\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)?//g; s/\x1b\\[[0-9;?]*[ -\\/]*[@-~]//g; s/\x1b[@-Z\\\\-_]//g' |
      tr -d '\000-\010\013-\037\177')
    body=$(printf '%s\n' "$clean" | awk '
      /^[[:space:]]*[A-Za-z][A-Za-z ]+ · from the / {
        t=$0; sub(/^[[:space:]]+/,"",t); sub(/[[:space:]]+· from the .*/,"",t); r=""; have=1; next
      }
      have && r == "" && $0 !~ /^[[:space:]]*$/ {
        r=$0; gsub(/^[[:space:]]+|[[:space:]]+$/,"",r)
      }
      END {
        if (have && r != "") printf "%s: %s", "prompt: permission — " t, r
        else printf "%s", "prompt: permission — (unparsed)"
      }')
    suffix=" — pane $pane"
    body=$(printf '%s' "$body" | tr '\n\t' '  ' | sed -E 's/[[:space:]]+/ /g')
    max=$((160 - ${#suffix}))
    [ "$max" -lt 0 ] && max=0
    printf '%s%s' "${body:0:$max}" "$suffix"
  }

  _unread_oldest() { _unread_scan "$crew" "$branch" "$me" "$from_id" "$run_start_ms" oldest; }

  # _finished_release — the worker posted done/failed: wait out --release, then
  # release its window and exit. Returns only when the worker has re-opened the
  # session (a later non-terminal status), so the main loop resumes watching.
  # Everything else exits, so a failed sample or --max-life never loops here.
  _try_release() {
    local rc=0
    _release_windows "$branch" "$rel_session" "$bus_state" "$bus_ts" "$release" "" || rc=$?
    [ "$rc" = 3 ] || exit 0
    idle_ticks=0
    prev_change=$(_clock_now)
  }

  _finished_release() {
    local t plain colored idle_ticks=0 prev_hash="" prev_change quiet_s rel_session=-
    ! _is_session_id "$from_id" || rel_session="${from_id##*#}"
    prev_change=$(_clock_now)
    say() { :; }
    note() { :; }
    while :; do
      _bus_refresh
      case "$bus_state" in
      done | failed) ;;
      '') _clock_sleep "$interval"; continue ;;
      *) return 0 ;;
      esac
      t=$(_clock_now)
      [ $((t - start)) -ge "$max_life" ] && exit 0
      # A watchdog-posted `failed` marks a hung pane: keep it as evidence.
      [ "$bus_source" != watchdog ] || exit 0
      if ! plain=$(_sample); then
        exit 0
      fi
      if [ "$(printf '%s' "$plain" | cksum)" != "$prev_hash" ]; then
        prev_hash=$(printf '%s' "$plain" | cksum)
        prev_change="$t"
      fi
      if [ $((t - bus_ts / 1000)) -ge "$release" ]; then
        if [ "$engine" = claude ]; then
          colored=$(_sample_colored || true)
          if (_pane_idle_reason "$plain" "$colored" >/dev/null); then
            idle_ticks=$((idle_ticks + 1))
          else
            idle_ticks=0
          fi
          [ "$idle_ticks" -lt 2 ] || _try_release
        else
          quiet_s=$((t - prev_change))
          ! _is_prompt "$plain" && ! _is_permission_prompt "$plain" || quiet_s=0
          [ "$quiet_s" -lt "$release" ] || _try_release
        fi
      fi
      _clock_sleep "$interval"
    done
  }

  start=$(_clock_now)
  _clock_sleep "$grace"
  fails=0
  last_hash=""
  last_change=$(_clock_now)
  tick=0
  d0_at=0
  d1_hits=0
  d1_at=0
  d1_kind=""
  d2_tok=""
  d2_clock=""
  d2_since=0
  d2_moved=0
  d2_at=0
  d1b_hits=0
  d1b_at=0
  d3_at=0
  d4_at=0
  d4_since=0
  d5_at=0
  d6_at=0
  d6_src=""
  nudged_ts=0
  nudge_off=0
  d7_hits=0
  d7_tok0=0
  d7_at=0
  d8_at=0
  engine_seen=0
  # Set when a tick advances without the loop-end cadence read (the pane-gone
  # continue), so the next D8 tick re-reads the bus even with no refresh; the
  # cadence read clears it.
  bus_stale=0
  _bus_refresh
  while :; do
    now=$(_clock_now)
    # --max-life exists because the watchdog is nohup-detached: without a hard
    # cap, a bug or an orphaned pane leaves a process polling forever.
    [ $((now - start)) -ge "$max_life" ] && exit 0
    # Exit on a terminal state — but NOT on pr_open: a worker in pr_open is
    # still working (#31's own prompt was rendered by a worker watching PR CI),
    # and not on `working`, which is a heartbeat.
    case "$bus_state" in
    done | failed)
      [ "$role_mode" = 1 ] || [ "$release" = 0 ] || { _finished_release; last_hash=""; last_change=$(_clock_now); continue; }
      exit 0
      ;;
    exited) exit 0 ;;
    esac

    if ! text=$(_sample); then
      # Pane-gone is a quorum, not a single failure: at a 12h lifetime one tmux
      # hiccup would otherwise disarm liveness for the rest of the run. Three
      # consecutive failures ≈45s at the default interval, and the `exited`
      # backstop owns the real case anyway.
      fails=$((fails + 1))
      [ "$fails" -ge 3 ] && exit 0
      _clock_sleep "$interval"
      tick=$((tick + 1))
      if [ $((tick % 4)) -eq 0 ]; then
        bus_stale=1
      fi
      continue
    fi
    fails=0
    hash=$(printf '%s' "$text" | cksum)
    if [ "$hash" != "$last_hash" ]; then
      last_hash="$hash"
      last_change="$now"
    fi

    # While the worker's own latest word is a SELF-reported `blocked` it is in
    # `crew await` — the dispatcher is already awake about it, and a held await
    # leaves a static pane by construction (a D3 false positive waiting).
    suppressed=0
    if [ "$bus_state" = blocked ] && [ "$bus_source" != watchdog ]; then
      suppressed=1
    fi
    quiet_for=$((now - last_change))
    # Waiting on a background shell silences D0/D3 — but only up to --bg-wait of
    # byte-identical frame, so a shell that never returns still reaches quiet:.
    bgwait=0
    if [ "$sig_bgwait" = 1 ] && [ "$quiet_for" -lt "$bg_wait" ] && _is_bg_wait "$text"; then
      bgwait=1
    fi

    # ---- D4: host load --------------------------------------------------
    # Engine-independent: reads the HOST, not the pane, so claude/codex/cursor/pi
    # all get it. --load seconds of load1 above the core count posts one load:
    # blocked episode; it clears when load drops back to/under the cores. Never
    # escalates (arms d4_at, never d2_at/d3_at). A malformed read stays silent
    # that tick rather than failing the whole watchdog.
    if [ "$suppressed" = 0 ] && [ "$role_mode" = 0 ]; then
      loadline=$(_load_read)
      if [[ "$loadline" =~ ^[0-9]+([.][0-9]+)?[[:space:]]+[0-9]+$ ]]; then
        read -r l1 cores <<<"$loadline"
        if awk -v l="$l1" -v c="$cores" 'BEGIN{exit !(l > c)}'; then
          [ "$d4_since" = 0 ] && d4_since="$now"
          if [ "$d4_at" = 0 ] && [ $((now - d4_since)) -ge "$load_win" ]; then
            detail2="load: 1m load $l1 on $cores cores for $((now - d4_since))s"
            top=$(_top_consumers)
            if [ -n "$top" ]; then
              detail2="$detail2 (top: $(printf '%s\n' "$top" | sed -n '1,2p' | awk 'NR>1{printf " | "}{printf "%s",$0}'))"
            fi
            if _post_blocked "load:" "$detail2"; then
              d4_at="$now"
            fi
          fi
        else
          if [ "$d4_at" != 0 ]; then
            _post_clear "load:"
          fi
          d4_at=0
          d4_since=0
        fi
      fi
    fi

    # ---- D8: engine budget ------------------------------------------------
    # The launch gates refuse an exhausted engine only at dispatch time; this
    # flags one that crosses the line while the worker runs. Bus-read cadence,
    # and only while the row is live work or a watchdog episode: a pr_open run
    # is finished and idle, and a watchdog row would mask it in roster/reap
    # (the later `budget: cleared` would even revive it in fan-out); a
    # self-reported blocked is already parked in a zero-token await
    # (`suppressed`). A refresh can hold the tick up to 120s, so the bus is
    # re-read after one and a pr_open or await posted meanwhile still gates. Every
    # other D8 tick uses the loop-end cadence read, or the startup read at tick 0.
    # A cache that can't tell holds the episode as it is, so a stale or blind read
    # never announces a clearance. Never escalates.
    if [ "$budget_on" = 1 ] && [ "$suppressed" = 0 ] && [ $((tick % 4)) -eq 0 ]; then
      # `if`, not a `||` chain: a 1 return here would end the watch under `set -e`.
      if _budget_refresh_maybe "$now" || [ "$bus_stale" = 1 ]; then
        _bus_refresh
        bus_stale=0
      fi
      if [ "$bus_state" = blocked ] && [ "$bus_source" != watchdog ]; then
        suppressed=1
      fi
      if [ "$suppressed" = 0 ]; then
        case "$bus_state" in
        "" | working | blocked)
          d8_detail=$(_budget_detail "$now") && d8_rc=0 || d8_rc=$?
          if [ "$d8_rc" = 0 ] && [ "$d8_at" = 0 ]; then
            if _post_blocked "budget:" "$d8_detail"; then
              d8_at="$now"
            fi
          elif [ "$d8_rc" = 1 ] && [ "$d8_at" != 0 ]; then
            _post_clear "budget:"
            d8_at=0
          fi
          ;;
        esac
      fi
    fi

    # ---- D5: launch not started ---------------------------------------------
    # dispatch posts `working` when it types the launch line, before the engine
    # runs, and the launch script `exec`s the engine, so a started engine is the
    # pane's foreground process. A pane still a shell --launch seconds in (from
    # this watchdog's start, which dispatch spawns right after that post) means
    # the binary is missing or the script failed. --launch must outlast a slow
    # direnv/devshell load. Once an engine has been seen this never fires again:
    # an engine that exits later is quiet:/dead: territory.
    if [ "$engine_seen" = 0 ] && [ "$suppressed" = 0 ] && [ "$role_mode" = 0 ]; then
      pcmd=$(_pane_cmd)
      if [ -n "$pcmd" ] && _is_engine_cmd "$pcmd"; then
        engine_seen=1
        if [ "$d5_at" != 0 ]; then
          _post_clear "stalled: launch-not-started"
          d5_at=0
        fi
      elif [ "$d5_at" = 0 ] && [ $((now - start)) -ge "$launch" ] && _is_shell_cmd "$pcmd"; then
        case "$bus_state" in
        "" | working)
          if _post_blocked "stalled: launch-not-started" "stalled: launch-not-started — pane $pane still a shell (${pcmd#-}) ${launch}s after launch; the engine is not running"; then
            d5_at="$now"
          fi
          ;;
        esac
      fi
    fi

    # ---- Role-mode end-of-life ----------------------------------------------
    # A role pane has no worker row to post a terminal status, so the watch
    # detects the engine's own return to a bare shell (a final release, or a
    # crash) and exits directly. Gated on having actually seen the engine once,
    # so a role pane that is still booting is never mistaken for one that
    # already exited.
    if [ "$role_mode" = 1 ]; then
      pcmd=$(_pane_cmd)
      if [ -n "$pcmd" ] && _is_engine_cmd "$pcmd"; then
        engine_seen=1
      elif [ "$engine_seen" = 1 ] && _is_shell_cmd "$pcmd"; then
        exit 0
      fi
    fi

    # ---- D1: interactive prompt --------------------------------------------
    # Presence, not transition: the workspace-trust frame is on screen from the
    # worker's FIRST sample, so an "appeared" conjunct could never fire on the
    # only frame with measured production occurrences.
    # quota: is a content discriminator on the SAME geometry, not a separate
    # detector — a quota-exhausted frame and a generic option-select frame are
    # mutually exclusive per sample, so one hit counter (d1_hits/d1_at) covers
    # both. d1_kind remembers which was actually posted so a mid-episode flip
    # (e.g. a prompt frame that becomes the quota frame with no intervening
    # non-prompt sample) reclassifies instead of staying mislabeled for the
    # rest of the run. The folding covers D1's rate-limit-prompt variant only;
    # D1b's session-limit frame carries its own counter (d1b_hits/d1b_at).
    # A tool-permission dialog (#435) is checked FIRST — its footer is not an
    # `Enter to select` frame, and it must not be gated on `_is_prompt`'s veto.
    d1_kind_this=""
    if [ "$suppressed" = 0 ] && [ "$sig_prompt" = 1 ]; then
      if _is_permission_prompt "$text"; then
        d1_kind_this=permission
      elif _is_prompt "$text" && [ -z "$(_meter_line "$text")" ]; then
        if _is_quota_prompt "$text"; then d1_kind_this=quota; else d1_kind_this=prompt; fi
      fi
    fi
    if [ -n "$d1_kind_this" ]; then
      kind="$d1_kind_this"
      if [ "$d1_at" != 0 ] && [ "$kind" != "$d1_kind" ]; then
        _post_clear "prompt:"
        _post_clear "quota:"
        d1_at=0
        d1_hits=0
      fi
      d1_hits=$((d1_hits + 1))
      if [ "$d1_hits" -ge 2 ] && [ "$d1_at" = 0 ]; then
        case "$kind" in
        permission)
          if _post_blocked "prompt:" "$(_permission_detail "$text" "$pane")"; then
            d1_at="$now"
            d1_kind=permission
          fi
          ;;
        quota)
          if _post_blocked "quota:" "quota: quota exhausted — worker parked on the rate-limit prompt in pane $pane; do not re-dispatch — Esc dismisses it, resume continues from intact context on the next window"; then
            d1_at="$now"
            d1_kind=quota
          fi
          ;;
        prompt)
          if _post_blocked "prompt:" "prompt: interactive prompt in pane $pane — worker is waiting on input nobody can give"; then
            d1_at="$now"
            d1_kind=prompt
          fi
          ;;
        esac
      fi
    else
      if [ "$d1_at" != 0 ]; then
        _post_clear "prompt:"
        _post_clear "quota:"
        d1_at=0
      fi
      d1_hits=0
      d1_kind=""
    fi

    # ---- D2: dead turn ----------------------------------------------------
    # Strings, never numbers: rounding to 0.1k and multi-unit durations make
    # arithmetic fragile and every parse failure a new branch.
    if [ "$suppressed" = 0 ] && [ "$sig_meter" = 1 ] && [ "$role_mode" = 0 ]; then
      m=$(_meter_line "$text")
      if [ -z "$m" ] || _has_subrow "$text"; then
        # A live subagent row is an UNCONDITIONAL veto: a healthy deep worker in
        # a subagent batch reproduces D2's exact signature — meter present,
        # clock rising, token string static — for minutes at a stretch (A1,
        # measured). The cost is a stated blind spot: a turn that dies with a
        # row still painted is invisible to D2.
        if [ "$d2_at" != 0 ]; then
          _post_clear "turn-stall:"
          d2_at=0
        fi
        d2_since=0
        d2_tok=""
        d2_clock=""
        d2_moved=0
      else
        clock=$(printf '%s' "$m" | sed -E 's/^[^(]*\(([^·]*)·.*/\1/')
        tok=$(printf '%s' "$m" | sed -E 's/.*↓[[:space:]]*([0-9.]+k?)[[:space:]]tokens.*/\1/')
        if [ "$d2_since" = 0 ] || [ "$tok" != "$d2_tok" ]; then
          if [ "$d2_at" != 0 ]; then
            _post_clear "turn-stall:"
            d2_at=0
          fi
          d2_tok="$tok"
          d2_clock="$clock"
          d2_since="$now"
          d2_moved=0
        elif [ "$clock" != "$d2_clock" ]; then
          # A rising clock proves the capture is a LIVE frame. A static clock
          # means a frozen renderer or copy-mode scrollback — evidence we cannot
          # trust, so the rule stays silent.
          d2_clock="$clock"
          d2_moved=1
        fi
        # Any status event from the worker in the window is a sign of life —
        # with the same one-second slack, since the window opens on a truncated
        # `date +%s` and the event carries ms.
        if [ "$bus_source" != watchdog ] && [ "$bus_ts" -ge $(((d2_since - 1) * 1000)) ]; then
          d2_since="$now"
          d2_moved=0
        fi
        if [ "$d2_at" = 0 ] && [ "$d2_moved" = 1 ] && [ $((now - d2_since)) -ge "$idle" ]; then
          if _post_blocked "turn-stall:" "turn-stall: token count static at $d2_tok for $((now - d2_since))s while the pane clock advanced"; then
            d2_at="$now"
          fi
        fi
      fi
    fi

    # ---- D1b: quota refusal (session limit / cursor monthly usage limit) ---
    # A second quota: frame, and its own detector because it satisfies neither
    # of the two above: no option-select geometry for D1, and it matches neither
    # re_meter nor re_subrow, so D2 reads it as "no active turn" and resets.
    # Without this block it falls through to D3 quiet:, which escalates (#93).
    # Both frames are verified and require no prompt geometry; each engine
    # enables only its own signature, so one hit counter and one post branch.
    d1b_kind=""
    if [ "$suppressed" = 0 ]; then
      if [ "$sig_session_limit" = 1 ] && _is_quota_session_limit "$text"; then
        d1b_kind=session
      elif [ "$sig_cursor_limit" = 1 ] && _is_quota_cursor_limit "$text"; then
        d1b_kind=cursor
      fi
    fi
    if [ -n "$d1b_kind" ]; then
      d1b_hits=$((d1b_hits + 1))
      if [ "$d1b_hits" -ge 2 ] && [ "$d1b_at" = 0 ]; then
        case "$d1b_kind" in
        session)
          if _post_blocked "quota:" "quota: session limit — do not re-dispatch; wait for the reset shown in pane $pane, or a human can run /low-priority there (spends weekly budget) — Esc/Enter will not submit a queued prompt while the limit holds"; then
            d1b_at="$now"
          fi
          ;;
        cursor)
          if _post_blocked "quota:" "quota: cursor monthly usage limit — do not re-dispatch; the limit resets on the Cursor billing cycle, not on a retry, so pane $pane stays parked until then"; then
            d1b_at="$now"
          fi
          ;;
        esac
      fi
    else
      if [ "$d1b_at" != 0 ]; then
        _post_clear "quota:"
        d1b_at=0
      fi
      d1b_hits=0
    fi

    # ---- D7: runaway output (#650) ----------------------------------------
    # The opposite of D2/D3: a degenerated turn repaints constantly and its
    # token count climbs, so it reads as a busy worker forever. A leaked model
    # sentinel in assistant prose is the engine-independent tell; it must
    # persist --runaway-hits samples AND the output-token count must have grown
    # by --runaway-tokens since the first hit, so a transient mention or a
    # quoted string in a short reply cannot post. Tool-output blocks are
    # excluded (see _runaway_prose). Never escalates (no d7 arm in the dead:
    # check). A lexical word-salad check (signal 3) is deliberately absent: it
    # needs real captures to tune and the sentinel+growth pair covers the
    # measured incident.
    if [ "$suppressed" = 0 ] && [ "$sig_runaway" = 1 ] && [ "$role_mode" = 0 ] &&
      prose=$(_runaway_prose "$text") && [[ "$prose" =~ $re_sentinel ]]; then
      tok=$(_out_tokens "$text")
      if [ -n "$tok" ]; then
        if [ "$d7_hits" = 0 ] || [ "$tok" -lt "$d7_tok0" ]; then
          d7_tok0="$tok"
        fi
        d7_hits=$((d7_hits + 1))
        if [ "$d7_at" = 0 ] && [ "$d7_hits" -ge "$runaway_hits" ] && [ $((tok - d7_tok0)) -ge "$runaway_tokens" ]; then
          if _post_blocked "runaway:" "runaway: leaked model sentinel in pane $pane for $d7_hits samples while output tokens grew by $((tok - d7_tok0)) — the turn is degenerate; verify the pane, then kill and re-dispatch (the worktree usually survives)"; then
            d7_at="$now"
          fi
        fi
      fi
    else
      if [ "$d7_at" != 0 ]; then
        _post_clear "runaway:"
        d7_at=0
      fi
      d7_hits=0
      d7_tok0=0
    fi

    # ---- D3: quiet pane ---------------------------------------------------
    # Byte-identity, not "no meter": a healthy claude pane repaints its spinner
    # every second, so a working worker can never satisfy D3 even if every
    # claude signature rots to garbage. This is the failsafe for signature rot
    # and the only steady-state coverage codex and cursor get.
    if [ "$suppressed" = 0 ] && [ "$bgwait" = 0 ] && [ "$d3_at" = 0 ] && [ "$quiet_for" -ge "$idle" ] && [ "$role_mode" = 0 ]; then
      if [ "$bus_source" = watchdog ] || [ "$bus_ts" -lt $((last_change * 1000)) ]; then
        if _post_blocked "quiet:" "quiet: pane unchanged for ${quiet_for}s"; then
          d3_at="$now"
        fi
      fi
    fi
    if [ "$d3_at" != 0 ] && [ "$quiet_for" -lt "$idle" ]; then
      _post_clear "quiet:"
      d3_at=0
    fi

    # ---- D0: startup silence ----------------------------------------------
    # Classify BEFORE judging. A static pane that is a prompt belongs to D1 and
    # D0 says nothing about it — that one negative check is the session-3 fix.
    # The detail carries no diagnosis: the old `(suspected startup/indexing
    # hang)` was wrong on 3/3 measured workers, and an invented cause reads to
    # the dispatcher as corroboration.
    if [ "$suppressed" = 0 ] && [ "$bgwait" = 0 ] && [ "$d0_at" = 0 ] && [ "$role_mode" = 0 ] &&
      [ $((now - start)) -lt "$window" ] && [ "$quiet_for" -ge "$stall" ]; then
      case "$bus_state" in
      "" | working)
        if [ "$sig_prompt" = 1 ] && { _is_permission_prompt "$text" || _is_prompt "$text"; }; then
          : # D1 owns this frame (generic or tool-permission)
        elif _post_blocked "stalled:" "stalled: no output for ${stall}s"; then
          d0_at="$now"
        fi
        ;;
      esac
    fi

    # ---- D6: unread role verdict or dispatcher directive --------------------
    # A lead that is `working` while a role's or the dispatcher's msg to its
    # session sits past the delivered mark (`_await_marks`) for --unread is a
    # #300-shaped deadlock: its pane repaints, so D2/D3 stay silent. Own prefix
    # rather than a `stalled:` sub-case because the recovery differs — nudge the
    # lead to `crew await`/`inbox`, don't unblock a pane. Only the newest 2000
    # bus lines are read and only every 4th tick, so the cost stays flat as the
    # log grows. A role msg the lead answered (a later msg from it to that role)
    # is handled, not unread; a dispatcher directive clears only on delivery.
    # The oldest of both is reported. Never escalates: a lead slow to read is
    # not dead.
    # An overdue dispatcher directive first gets one auto-nudge per newest
    # directive (a role verdict never does: the lead `crew await`s those). An
    # accepted nudge skips this tick's verdict, since the lead is reading its
    # inbox now; a typed-but-unaccepted one is reported once as the episode.
    # An anchor refusal means a stale pane or session: stop trying for good.
    if [ "$suppressed" = 0 ] && [ "$role_mode" = 0 ] && [ $((tick % 4)) -eq 0 ]; then
      d6_skip=0
      d6_nudge=0
      case "$bus_state" in
      "" | working) d6_nudge=1 ;;
      blocked) [ "$d6_at" = 0 ] || [ "$bus_source" != watchdog ] || [[ $bus_detail != unread:* ]] || d6_nudge=1 ;;
      esac
      if [ "$d6_nudge" = 1 ] && [ "$nudge_on" = 1 ] && [ "$nudge_off" = 0 ] && [ "$from_id" != "$me" ] &&
        { [ "$engine" = claude ] || [ "$engine" = pi ]; }; then
        read -r d_old d_new <<<"$(_unread_scan "$crew" "$branch" "$me" "$from_id" "$run_start_ms" dispatcher)"
        if [[ "$d_old" =~ ^[0-9]+$ ]] && [[ "$d_new" =~ ^[0-9]+$ ]] &&
          [ $((now * 1000 - d_old)) -ge $((unread * 1000)) ] && [ "$d_new" -gt "$nudged_ts" ]; then
          # Once per directive across watchdogs and the dispatcher: a restarted
          # watchdog or a manual `crew nudge` may already have typed for it.
          if tail -n 2000 "$log" 2>/dev/null | jq -Rne --arg c "$crew" --arg to "$from_id" --argjson m "$d_new" '
            any(inputs | fromjson? | objects; .crew_id == $c and .kind == "nudge" and .to == $to and (.msg_ts // 0) >= $m)' >/dev/null; then
            nudged_ts="$d_new"
          else
            nudge_rc=0
            nudge_out=$(_nudge_pane "$pane" "$engine" "$from_id" "$crew" watchdog "$d_new") || nudge_rc=$?
            case "$nudge_rc" in
            0)
              nudged_ts="$d_new"
              d6_skip=1
              ;;
            3)
              nudged_ts="$d_new"
              d6_skip=1
              # _post, not _post_blocked: our own open unread: episode must not
              # swallow the failure.
              _post blocked "unread: dispatcher directive undelivered for $(((now * 1000 - d_old) / 1000))s — auto-nudge typed but not accepted (${nudge_out%% — *}); verify the pane with crew where"
              d6_at="$now"
              d6_src=dispatcher
              ;;
            *) [[ $nudge_out != anchor:* ]] || nudge_off=1 ;;
            esac
          fi
        fi
      fi
      [ "$d6_skip" = 1 ] || case "$bus_state" in
      "" | working)
        [ "$bus_source" = watchdog ] || d6_at=0
        read -r oldest src <<<"$(_unread_oldest)"
        if [[ "$oldest" =~ ^[0-9]+$ ]] && [ $((now * 1000 - oldest)) -ge $((unread * 1000)) ]; then
          age=$(((now * 1000 - oldest) / 1000))
          if [ "$src" = dispatcher ]; then
            detail="unread: dispatcher directive undelivered for ${age}s — lead is working but has not reached a peek seam (long stage or idle on a background task)"
          else
            detail="unread: role verdict undelivered for ${age}s — lead is working but has not read it; nudge it to run \`crew await\`"
          fi
          if [ "$d6_at" = 0 ] && _post_blocked "unread:" "$detail"; then
            d6_at="$now"
            d6_src="$src"
          fi
        elif [ "$d6_at" != 0 ]; then
          _post_clear "unread:"
          d6_at=0
        fi
        ;;
      blocked)
        # Our own open episode: still clear it once the msg is delivered.
        if [ "$d6_at" != 0 ] && [ "$bus_source" = watchdog ]; then
          read -r oldest src <<<"$(_unread_oldest)"
          # A changed oldest source means the label is stale; the next tick re-posts.
          if ! [[ "$oldest" =~ ^[0-9]+$ ]] || [ $((now * 1000 - oldest)) -lt $((unread * 1000)) ] || [ "$src" != "$d6_src" ]; then
            _post_clear "unread:"
            d6_at=0
          fi
        fi
        ;;
      esac
    fi

    # ---- Escalation --------------------------------------------------------
    # The only path to `failed`, and it is a SECOND, later, independent evidence
    # check — not a timer. `prompt:` and `quota:` are both exempt (C-1): a frame
    # still on screen means nobody answered yet (or the quota window hasn't
    # reset), and escalating either would reproduce session 3 with a 30-minute
    # delay — for `quota:` specifically, laundering hours of an intact worker
    # into a `failed` that invites exactly the re-dispatch this state exists to
    # prevent. `stalled:` is exempt too — it is the branch that catches the
    # classifier's misses, so it must stay recoverable; a genuinely dead pane
    # reaches `failed` through the `quiet:` episode instead.
    # The live bus, not the in-process timer, decides: a timer armed before D1
    # claimed the branch (or before a second watchdog process superseded it —
    # INV-W3 allows two live per branch, coordinating only via the bus) still
    # points at an episode that is no longer the open one, and escalating it
    # would launder a `prompt:`/`quota:` into a `failed`.
    if [ "$role_mode" = 0 ] && [ "$d2_at" != 0 ] && [ $((now - d2_at)) -ge "$dead" ]; then
      _bus_refresh
      case "$bus_detail" in
      prompt:* | quota:* | runaway:*) ;;
      *)
        _post failed "dead: turn-stall: unchanged for $((now - d2_at))s"
        exit 0
        ;;
      esac
    fi
    if [ "$role_mode" = 0 ] && [ "$d3_at" != 0 ] && [ $((now - d3_at)) -ge "$dead" ]; then
      _bus_refresh
      case "$bus_detail" in
      prompt:* | quota:* | runaway:*) ;;
      *)
        if ! _pane_engine_alive; then
          _post failed "dead: quiet: unchanged for $((now - d3_at))s"
          exit 0
        fi
        ;;
      esac
    fi

    _clock_sleep "$interval"
    tick=$((tick + 1))
    # Bus-read cadence: a whole-file jq over a growing cross-crew log every tick
    # for 12h is ~2880 spawns per worker. Every 4th tick costs ≤60s of latency
    # against an 1800s threshold. _post's pre-write read is exempt.
    if [ $((tick % 4)) -eq 0 ]; then
      _bus_refresh
      bus_stale=0
    fi
  done
  ;;
pr-watch)
  # pr-watch <N> [--repo owner/name] [--timeout S] [--interval S]
  # Thin bus bridge over the standalone `pr-watch` binary, which owns the park,
  # the change signals and the per-PR cursor — and needs no crew id at all. All
  # this adds is the post: addressed to this crew's dispatcher, so an armed
  # `crew watch` wakes. Stdout stays the event, so the wrapper still composes.
  crew=$(_crew_id)
  [ -n "$crew" ] || {
    echo "crew: CREW_ID unset and no WORKER_TASK.md crew_id" >&2
    exit 1
  }
  ev=$(pr-watch "$@")
  # Empty stdout is pr-watch's timeout marker, not a failure — nothing to post.
  [ -n "$ev" ] || exit 0
  mkdir -p "$dir"
  line=$(jq -nc --arg crew "$crew" --arg from "pr-watch:${1:-}" --arg body "$ev" \
    '{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
        kind:"msg", body:$body}')
  _bus_append "$log" "$line"
  printf '%s\n' "$ev"
  ;;
git-baseline)
  # Lists — or, with --accept, merges into — the exec-capable and redirecting
  # git-config baseline _wt_cfg_guard enforces (#557, #585, #678). Only a dispatch records one
  # unasked. Values are shown %q-escaped so a planted ESC/CR cannot redraw the
  # terminal, and --accept writes exactly the pairs this run printed: each
  # context is read once, and a pair is shown and collected in one step.
  case "$#:${1:-}" in
  0:) accept= ;;
  1:--accept) accept=1 ;;
  *)
    echo "usage: crew git-baseline [--accept]" >&2
    exit 1
    ;;
  esac
  if [ -n "$accept" ] && [ ! -t 0 ]; then
    echo "crew: git-baseline --accept needs your own terminal (not Claude Code's ! prefix)" >&2
    exit 1
  fi

  wt_git_lib="${WORKTREE_GIT_LIB:-@worktreeGitLib@}"
  # shellcheck source=/dev/null
  . "$wt_git_lib"
  baseline_file="$common/crew/git-config-baseline"
  gb_recs=()
  if [ -f "$baseline_file" ]; then
    mapfile -d '' gb_recs <"$baseline_file"
  elif [ -z "$accept" ]; then
    echo "git-config baseline $baseline_file: none yet — the next dispatch records it, or run \`crew git-baseline --accept\` from your own terminal" >&2
    exit 1
  fi

  declare -A gb_base=() gb_seen=()
  gb_canon=
  gb_marked=
  gb_real="$(realpath -e -- "$common")" || gb_real=
  for gb_rec in "${gb_recs[@]}"; do
    [ -n "$gb_rec" ] || continue
    # shellcheck disable=SC2154 # set by the sourced worktree-git lib
    [ "$gb_rec" != "$_wt_cfg_redirect_mark" ] || gb_marked=1
    _wt_cfg_canon "$gb_real" "$gb_rec" gb_canon
    gb_base["$gb_canon"]=1
  done

  gb_ctxs=("$common")
  for gb_head in "$common"/worktrees/*/HEAD; do
    [ -f "$gb_head" ] && gb_ctxs+=("${gb_head%/HEAD}")
  done

  if [ -f "$baseline_file" ] && [ -z "$gb_marked" ]; then
    echo "git-config baseline $baseline_file predates redirect-key coverage — the next dispatch records the redirect keys present then, or --accept does"
  fi

  gb_shown=()
  for gb_ctx in "${gb_ctxs[@]}"; do
    gb_label="main checkout"
    [ "$gb_ctx" = "$common" ] || printf -v gb_label 'worktree %q' "${gb_ctx##*/}"
    mapfile -d '' gb_listing < <(git --git-dir="$gb_ctx" config --list --show-origin --show-scope -z)
    if ! wait $!; then
      echo "crew: cannot list the git config of $gb_ctx" >&2
      exit 1
    fi
    for ((gb_i = 0; gb_i + 2 < ${#gb_listing[@]}; gb_i += 3)); do
      [[ ${gb_listing[gb_i]} == local || ${gb_listing[gb_i]} == worktree ]] || continue
      gb_rec="${gb_listing[gb_i + 2]}"
      [[ $gb_rec == *$'\n'* ]] || gb_rec+=$'\n'
      gb_key="${gb_rec%%$'\n'*}"
      gb_value="${gb_rec#"$gb_key"$'\n'}"
      _wt_cfg_guarded "$gb_key" "$gb_value" || continue
      _wt_cfg_canon "$gb_real" "$gb_rec" gb_canon
      [ -z "${gb_base["$gb_canon"]+x}" ] || continue
      gb_origin="${gb_listing[gb_i + 1]#file:}"
      [ -z "${gb_seen["$gb_origin"$'\n'"$gb_rec"]+x}" ] || continue
      gb_seen["$gb_origin"$'\n'"$gb_rec"]=1
      printf '%q=%q (%s, %q)\n' "$gb_key" "$gb_value" "$gb_label" "$gb_origin"
      gb_shown+=("$gb_rec")
    done
  done

  if [ "${#gb_shown[@]}" -eq 0 ] && [ -f "$baseline_file" ] && [ -n "$gb_marked" ]; then
    echo "git-config baseline $baseline_file: no drift"
    exit 0
  fi
  if [ -z "$accept" ]; then
    exit 1
  fi

  if [ "${#gb_shown[@]}" -eq 0 ]; then
    echo "no guarded git config yet — yes records an empty baseline"
  fi
  printf 'Accept these into the baseline %s? type yes: ' "$baseline_file"
  read -r gb_answer
  if [ "$gb_answer" != yes ]; then
    echo "crew: baseline unchanged" >&2
    exit 1
  fi

  mkdir -p -- "$dir" || exit 1
  gb_tmp="$(mktemp "$baseline_file.XXXXXX")" || exit 1
  # Merge, never replace: this run shows only the drift, so a replace would
  # drop every pair already accepted. The marker is always written, so an
  # empty set is never a 0-byte file or a lone NUL (an empty record).
  if ! for gb_rec in "${gb_recs[@]}" "${gb_shown[@]}" "$_wt_cfg_redirect_mark"; do
    [ -z "$gb_rec" ] || printf '%s\0' "$gb_rec"
  done | LC_ALL=C sort -z -u >"$gb_tmp" || ! mv -f -- "$gb_tmp" "$baseline_file"; then
    rm -f -- "$gb_tmp"
    exit 1
  fi
  echo "git-config baseline $baseline_file: accepted"
  ;;
reap)
  # Reclaim window + worktree for workers whose PR has landed. No crew filter:
  # the workers worth reaping are precisely the ones from earlier dispatcher
  # sessions, so scoping to the current crew id would skip every real candidate.
  #
  # The PR — not elapsed time — is the gate. A worker sits in `done` for as long
  # as its PR takes to merge, answering review comments and fixing CI the whole
  # time; a time-based sweep would delete live work.
  quiet=""
  dry=""
  nowait=""
  idle=$release_grace
  while [ $# -gt 0 ]; do
    case "$1" in
    --quiet) quiet=1 ;;
    --dry-run) dry=1 ;;
    --no-wait) nowait=1 ;;
    --idle)
      [ -n "${2:-}" ] || {
        echo "crew: --idle needs a value in seconds" >&2
        exit 1
      }
      idle="$2"
      shift
      ;;
    *)
      echo "crew: reap takes --quiet, --dry-run, --no-wait and --idle S (got '$1')" >&2
      exit 1
      ;;
    esac
    shift
  done
  case "$idle" in '' | *[!0-9]*)
    echo "crew: --idle must be a non-negative integer number of seconds" >&2
    exit 1
    ;;
  esac
  claim_mask_ttl=3600
  # Unconditional, unlike the advisory hint lib: without it reap must abort,
  # never fall back to discovery in a worker's worktree (#539).
  wt_git_lib="${WORKTREE_GIT_LIB:-@worktreeGitLib@}"
  # shellcheck source=/dev/null
  . "$wt_git_lib"
  # Never run reap's git from a worker's worktree (#633); the caller's own
  # cwd still decides which worktree is kept.
  reap_cwd="$PWD"
  _wt_trusted_cwd "$common" || exit 1
  [ -f "$log" ] || exit 0
  # say: outcomes, always. note: kept-worker bookkeeping, silenced under --quiet
  # so the dispatch call site stays silent unless something actually happened.
  say() { echo "crew reap: $1"; }
  note() { [ -n "$quiet" ] || echo "crew reap: $1"; }

  # _reap_procs — emit "pid<TAB>cwd" lines for every enumerable process (the
  # caller filters by cwd prefix and ancestor). Linux /proc only, which is fine:
  # the whole harness is tmux + systemd + /proc already. CREW_REAP_PROC_CMD
  # overrides the enumeration for tests that must not touch the real /proc.
  _reap_procs() {
    local pid cwd c
    if [ -n "${CREW_REAP_PROC_CMD:-}" ]; then
      eval "$CREW_REAP_PROC_CMD" 2>/dev/null || true
      return 0
    fi
    for c in /proc/[0-9]*/cwd; do
      [ -e "$c" ] || continue
      pid=${c%/cwd}
      pid=${pid#/proc/}
      cwd=$(readlink "$c" 2>/dev/null || true)
      [ -n "$cwd" ] || continue
      printf '%s\t%s\n' "$pid" "$cwd"
    done
  }

  # CREW_RATE_AUTOSWEEP: unset/1 (default) = detached async sweep; sync =
  # foreground, for an operator watching a sweep or diagnosing a skipped one;
  # 0 = no sweep at all, which exists for test isolation and is NOT a
  # supported production switch.
  # Async mode is the detached hook: its stdout/stderr are the autosweep log,
  # so a skipped or failed sweep leaves a trace instead of vanishing.
  _rate_autosweep() {
    local mode="${1:-}" store_dir lockd sweeplog trimtmp rc=0
    store_dir="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
    lockd="$store_dir/ratings.sweep.lock.d"
    sweeplog="$store_dir/autosweep.log"
    # _lock_acquire cannot mkdir into a missing parent, so without this the
    # hook returns 1 and never sweeps — silently — on exactly the machines
    # that have never run `crew rate` by hand.
    mkdir -p "$store_dir"
    if [ "$mode" = async ]; then
      exec >>"$sweeplog" 2>&1
      echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) start pid=$BASHPID repo=$(git rev-parse --show-toplevel 2>/dev/null || true)"
    fi
    # The store is machine-global, so this lock is too: the skip below is
    # cross-repo, not same-repo, even though two repos' sweeps never contend
    # on `ratings.lock.d` itself. Self-healing — the bus is append-only, so
    # the next reap for the skipped repo backfills it in full.
    if _lock_acquire "$lockd" "$BASHPID"; then
      trap '_lock_release "$lockd"' EXIT
      bash -euo pipefail "$0" rate || rc=$?
      _lock_release "$lockd"
      trap - EXIT
      if [ "$mode" = async ]; then
        echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) done rc=$rc"
      fi
    elif [ "$mode" = async ]; then
      echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) a ratings sweep is already running — skipped"
    else
      note "a ratings sweep is already running — skipped"
    fi
    if [ "$mode" = async ]; then
      # In place, never mv: a concurrent sweep holds this file open with
      # O_APPEND, and replacing the inode would strand its `done rc=` line.
      trimtmp="$sweeplog.$BASHPID"
      tail -n 200 "$sweeplog" >"$trimtmp" && cat "$trimtmp" >"$sweeplog"
      rm -f "$trimtmp"
    fi
  }
  # An unrecognised value defaults to ON, loudly: reading a typo as "off"
  # would silently disable the sweep, which is the failure this hook exists
  # to fix. Warn rather than `exit 1` — reap's status is not the hook's to
  # change.
  autosweep="${CREW_RATE_AUTOSWEEP:-1}"
  case "$autosweep" in
  0 | 1 | sync) ;;
  *)
    echo "crew reap: CREW_RATE_AUTOSWEEP='$autosweep' is not 0, 1 or sync — sweeping anyway" >&2
    autosweep=1
    ;;
  esac
  case "$autosweep" in
  0) ;;
  *)
    if [ -n "$dry" ]; then
      note "dry run — skipping ratings sweep"
    elif [ "$autosweep" = sync ]; then
      _rate_autosweep
    else
      (
        trap '' HUP
        _rate_autosweep async
      ) </dev/null &
    fi
    ;;
  esac

  # Two reaps racing a removal on one tree strand it. After the autosweep
  # block because its sync mode clears the EXIT trap. dispatch waits (it reads
  # branch state right after its reap); the stream passes --no-wait.
  reap_lock="$dir/reap.lock.d"
  waited=0
  until _lock_acquire "$reap_lock" "$$"; do
    if [ -n "$nowait" ]; then
      note "another reap is running — skipped"
      exit 0
    fi
    if [ "$waited" -ge 120 ]; then
      note "another reap is still running after 120s — skipped"
      exit 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  trap '_lock_release "$reap_lock"' EXIT

  # The set of states a worker session ends in — shared by the idle-release
  # filter below and the reclaim filter further down so they can't drift
  # apart again (#68): a worker that `exited` (compacted, hit its context
  # limit, or the human took the pane) or `failed` is just as finished as one
  # that reported `done`, and the PR-state gate downstream is what actually
  # decides whether reclaiming its worktree is safe — not which of the three
  # terminal states it ended in.
  reap_terminal_states='["done","failed","exited"]'

  # Untracked scaffold our own pipeline writes into every worktree — must
  # never read as "uncommitted work" below. Only a TRACKED modification, or
  # an untracked file outside this set, is real dirt. Keep this pattern in
  # sync with the writers or a new scaffold file pins the worktree forever:
  # WORKER_TASK.md (dispatch.sh, `>"$wt_path/WORKER_TASK.md"`),
  # SPEC.md/PLAN.md/DECOMPOSITION.md (WORKER_PROTOCOL.md's spec-plan-critic
  # flow), REVIEW_NOTES.md (EVIDENCE_REVIEW.md's "Recurrence and handoff"
  # worktree-root ledger), docs/superpowers/plans/*.md (the superpowers
  # writing-plans skill), and PLAN_ROUND<N>.md (the superpowers writing-plans
  # round plans, e.g. PLAN_ROUND4.md observed in a reaped worker tree).
  # Anchored on the porcelain `?? ` prefix and the full path so a
  # real file merely named e.g. `src/PLAN.md` still counts as dirt.
  reap_scaffold_re='^\?\? (WORKER_TASK\.md|SPEC\.md|PLAN\.md|DECOMPOSITION\.md|REVIEW_NOTES\.md|PLAN_ROUND[0-9]+\.md|docs/superpowers/plans/[^/]*\.md)$'
  tampered="its .git changed during the reap; possible tampering: tell the human"

  # Idle release: a session that reached a terminal state but whose window is
  # still sitting there keeps the tree occupied, and the PR gate below deliberately
  # will not touch it while its PR is open. Kill the WINDOW only — the worktree
  # stays, because a worker legitimately sits in `done` for as long as its PR
  # takes to merge (#17).
  _frame_classifier
  while IFS=$'\t' read -r rbranch rsession rstate rts; do
    [ -n "$rbranch" ] || continue
    _release_windows "$rbranch" "$rsession" "$rstate" "$rts" "$idle" "$dry" || [ $? = 3 ]
  done <<EOF
$(jq -s -r --argjson idle "$idle" --argjson terminal "$reap_terminal_states" '
    def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
    def wid_session: ltrimstr("worker:") | (if test("#") then (split("#") | last) else null end);
    # A dispatch posts a "claim" for a branch before its window even exists.
    # It has no `body`, so it can never pass the terminal-state filter below —
    # it can only mask a stale prior `done` by winning the per-branch
    # max_by(.ts), never cause a release on its own.
    map(select((.kind=="status" or .kind=="claim") and ((.from // "") | startswith("worker:"))))
    | group_by(.from) | map(max_by(.ts))
    | group_by(.from | wid_branch) | map(max_by(.ts))
    | map(select(.body.state as $st | ($terminal | index($st)) != null))
    # A watchdog-posted failed marks a hung pane: keep it as evidence.
    | map(select((.body.source // "") != "watchdog" or .body.state != "failed"))
    | map(select((((now*1000) - .ts) / 1000) >= $idle))
    | .[] | [(.from | wid_branch), ((.from | wid_session) // "-"), .body.state, .ts] | @tsv' "$log")
EOF
  command -v gh >/dev/null || {
    note "needs gh"
    exit 0
  }

  # Orphan windows: report only — a human may be using one.
  while IFS=$'\t' read -r owid obranch ocdir; do
    [ -n "$owid" ] && [ -n "$obranch" ] || continue
    [ "$ocdir" = "$dir" ] || continue
    owt=$(git worktree list --porcelain |
      awk -v b="refs/heads/$obranch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    [ -n "$owt" ] && [ -d "$owt" ] && continue
    note "window $owid is stamped $obranch but its worktree is gone — not killed"
  done <<EOF
$(tmux list-windows -a -F $'#{window_id}\t#{@crew_branch}\t#{@crew_dir}' 2>/dev/null || true)
EOF

  # Latest status per worker across every crew; keep the ones in a terminal
  # state (same set the idle-release pass above uses — see $reap_terminal_states).
  # pr_url is carried forward because the `done` event itself drops it (same
  # reason roster does this). Fresh claims join the fold here only (not
  # idle-release permanence, not dispatch retract): a claim-latest session has
  # no body.state so it masks but never qualifies; a claim's age must be
  # nonnegative and under $claim_mask_ttl to mask at all — a future-dated
  # timestamp (clock skew, bad fixture, bad actor) yields a negative age and
  # must not win the fold forever, and claims older than $claim_mask_ttl drop
  # out so a prior terminal status can resurface.
  # pr_open is a candidate here only, never for idle release. `later`: some
  # session of the branch posted after the terminal status, so it is not idle.
  candidates=$(jq -s -r --argjson terminal "$reap_terminal_states" --argjson claim_ttl "$claim_mask_ttl" '
      def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
      (map(select((.kind == "msg" or .kind == "status") and ((.from // "") | startswith("worker:")))
          | {branch: (.from | wid_branch), ts})
        | group_by(.branch) | map({key: .[0].branch, value: (map(.ts) | max)}) | from_entries) as $last
      | (map(select(
              ((.from // "") | startswith("worker:"))
              and (
                .kind == "status"
                or (.kind == "claim" and ((((now*1000) - .ts) / 1000) as $age | $age >= 0 and $age < $claim_ttl))
              )))
          | group_by(.from) | map(
              (max_by(.ts)) as $latest
              | {branch: ($latest.from | wid_branch),
                 session: $latest.from,
                 ts: $latest.ts,
                 state: $latest.body.state,
                 pr_url: (map(.body.pr_url) | map(select(. != null)) | last)})
          | group_by(.branch) | map(sort_by(.ts) | last)
          | map(select(.state as $st | (($terminal + ["pr_open"]) | index($st)) != null))
        )
      | .[] | [.branch, .state, (.pr_url // "-"), .ts, (if ($last[.branch] // 0) > .ts then "1" else "0" end)] | @tsv' "$log")
  [ -n "$candidates" ] || {
    note "nothing done to reap"
    exit 0
  }

  # Panes running an engine, to tell "finished worker" from "someone is in there
  # right now". Same path-keyed idiom as roster: window names are rewritten by
  # lazytmux, so the worktree path is the only stable handle.
  live=$(tmux list-panes -a -F $'#{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}' 2>/dev/null || true)
  _frame_classifier

  reaped=0
  while IFS=$'\t' read -r branch state pr cand_ts later; do
    [ -n "$branch" ] || continue
    wtpath=$(git worktree list --porcelain |
      awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    [ -n "$wtpath" ] && [ -d "$wtpath" ] || continue

    if [ "$pr" = "-" ]; then
      note "keeping $branch — $state but no PR on the bus"
      continue
    fi
    pr_state=$(gh pr view "$pr" --json state --jq .state 2>/dev/null || true)
    case "$pr_state" in
    MERGED | CLOSED) ;;
    "")
      note "keeping $branch — could not read PR state ($pr)"
      continue
      ;;
    *)
      note "keeping $branch — PR $pr_state"
      continue
      ;;
    esac

    engine_panes=()
    while IFS=$'\t' read -r pwin ppane pcmd ppath; do
      [ -n "$pwin" ] || continue
      _pane_is_engine_at "$pcmd $ppath" "$wtpath" || continue
      engine_panes+=("$pwin"$'\t'"$ppane"$'\t'"$pcmd")
    done <<PANES
$live
PANES

    idle_windows=()
    idle_panes=()
    idle_frames=()
    if [ "${#engine_panes[@]}" -gt 0 ]; then
      keep_reason=""
      case "$state" in
      done | pr_open) ;;
      *) keep_reason="$state session" ;;
      esac
      [ -n "$keep_reason" ] || [ "$later" != 1 ] || keep_reason="session posted after its $state"
      if [ -z "$keep_reason" ]; then
        for pane_row in "${engine_panes[@]}"; do
          IFS=$'\t' read -r ewin epane ecmd <<<"$pane_row"
          stripped="${ecmd#.}"
          stripped="${stripped%-wrapped}"
          if [ "$stripped" != claude ]; then
            keep_reason="$ecmd has no idle signature"
            break
          fi
          capture=$(tmux capture-pane -p -t "$epane" 2>/dev/null || true)
          if [ -z "$capture" ]; then
            keep_reason="empty capture of $epane"
            break
          fi
          colored=$(tmux capture-pane -e -p -t "$epane" 2>/dev/null || true)
          if ! idle_reason=$(_pane_idle_reason "$capture" "$colored"); then
            keep_reason="$idle_reason"
            break
          fi
          idle_windows+=("$ewin")
          idle_panes+=("$epane")
          idle_frames+=("$capture")
        done
      fi
      if [ -n "$keep_reason" ]; then
        note "keeping $branch — an engine is still running there ($keep_reason)"
        continue
      fi
    fi
    # Never remove the worktree the caller is standing in — it would leave the
    # invoking shell (or dispatch itself) on a path that no longer exists.
    case "$reap_cwd/" in
    "$wtpath"/*)
      note "keeping $branch — it is the current worktree"
      continue
      ;;
    esac

    # Before reap's own anchored reads of the tree, the gitlink must point at
    # the real admin dir and no submodule may be there to recurse into (#539).
    # Each gate keeps the worktree.
    if ! admin=$(_wt_admin_dir "$common" "$wtpath"); then
      note "keeping $branch — no git admin dir for $wtpath"
      continue
    fi
    if ! _wt_gitlink_ok "$admin" "$wtpath"; then
      note "keeping $branch — its .git does not point at its git admin dir"
      continue
    fi
    if [ -e "$admin/config.worktree" ] || [ -L "$admin/config.worktree" ]; then
      note "keeping $branch — $admin/config.worktree exists"
      continue
    fi
    if ! staged=$(_wt_git "$admin" "$wtpath" ls-files --stage); then
      note "keeping $branch — could not read its index"
      continue
    fi
    if grep -q '^160000 ' <<<"$staged"; then
      note "keeping $branch — it has submodules"
      continue
    fi

    # Check for leftover work BEFORE touching anything. The removal is forced,
    # so this check and the re-check right before it are what keep dirty work.
    # --untracked-files=all: the default "normal" mode collapses a brand-new
    # untracked directory to just its own name (e.g. `?? docs/`), which would
    # never match the docs/superpowers/plans/*.md pattern in $reap_scaffold_re.
    if ! st=$(_wt_status "$admin" "$wtpath" --untracked-files=all); then
      note "keeping $branch — git status failed"
      continue
    fi
    dirt=$(printf '%s\n' "$st" | grep -vE "$reap_scaffold_re" || true)
    if [ -n "$dirt" ]; then
      note "keeping $branch — uncommitted changes"
      continue
    fi

    # Kill idle-engine windows, role panes with them. The first sample is
    # seconds stale by now (gh, git status): a pane that changed at all, or a
    # branch that posted since, keeps everything.
    if [ "${#idle_windows[@]}" -gt 0 ]; then
      if ! { _wt_cfg_guard "$common" "$admin" && _wt_cfg_guard_cwd "$common"; }; then
        note "keeping $branch — git config drift"
        continue
      fi
      resample=""
      for i in "${!idle_panes[@]}"; do
        capture=$(tmux capture-pane -p -t "${idle_panes[i]}" 2>/dev/null || true)
        colored=$(tmux capture-pane -e -p -t "${idle_panes[i]}" 2>/dev/null || true)
        if [ "$capture" != "${idle_frames[i]}" ] || ! _pane_idle_reason "$capture" "$colored" >/dev/null; then
          resample="pane changed between samples"
          break
        fi
      done
      # Anything but a clean `false` (an unreadable bus included) keeps.
      if [ -z "$resample" ] && [ "$(jq -n --arg w "worker:$branch" --argjson ts "$cand_ts" '
          any(inputs; (.kind == "status" or .kind == "msg")
            and ((.from // "") as $f | $f == $w or ($f | startswith($w + "#")))
            and .ts > $ts)' "$log" 2>/dev/null || true)" != false ]; then
        resample="the branch posted since"
      fi
      if [ -n "$resample" ]; then
        note "keeping $branch — $resample"
        continue
      fi
      while IFS= read -r w; do
        [ -n "$w" ] || continue
        if [ -n "$dry" ]; then
          say "would kill window $w ($branch idle engine)"
        else
          tmux kill-window -t "$w" 2>/dev/null || true
          say "killed window $w ($branch idle engine)"
        fi
      done < <(printf '%s\n' "${idle_windows[@]}" | sort -u)
    fi

    # Kill leftover processes reparented out of the pane but still rooted in this
    # worktree (the #187 class: `yes` hogs reparented to systemd). Skip our own
    # process and its ancestors; a dangling cwd (dir already gone) still matches
    # by string. Runs BEFORE the --dry-run continue so dry runs report it too.
    ancestors="$$ $BASHPID"
    p="$BASHPID"
    while [ "$p" -gt 1 ]; do
      pp=$(awk '/^PPid:/{print $2}' "/proc/$p/status" 2>/dev/null || true)
      [ -n "$pp" ] || break
      ancestors="$ancestors $pp"
      p="$pp"
    done
    while IFS=$'\t' read -r rpid rcwd; do
      [ -n "$rpid" ] || continue
      case " $ancestors " in
      *" $rpid "*) continue ;;
      esac
      case "$rcwd/" in
      "$wtpath"/*) ;;
      *) continue ;;
      esac
      if [ -n "$dry" ]; then
        say "would kill pid $rpid (cwd $rcwd)"
        continue
      fi
      kill "$rpid" 2>/dev/null || true
      say "killed pid $rpid (cwd $rcwd)"
    done <<PROCS
$(_reap_procs)
PROCS

    # The resume record's path is keyed by the worktree's own realpath, so it
    # must be computed while the directory still exists — before the removal (#556).
    anchor="$(_worktree_anchor_path "$wtpath")"

    if [ -n "$dry" ]; then
      say "would reap $branch ($pr_state) @ $wtpath"
      say "would prune record $anchor"
      continue
    fi

    # Every scaffold artifact is untracked, and the re-check before the removal
    # treats ANY status line as dirt — the dirt check above only declares them
    # non-dirt so our own pipeline never pins a finished tree forever; each
    # file must ALSO be moved out physically, or the re-check keeps the tree
    # (feat/113-116-127-142 all sat blocked by untracked PLAN.md/SPEC.md
    # alone). gtrash everything so a post-mortem can still recover it.
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      scaffold=$(printf '%s' "$line" | sed -nE 's/^\?\? (.*)$/\1/p')
      [ -n "$scaffold" ] || continue
      gtrash put "$wtpath/$scaffold" >/dev/null 2>&1 || true
    done <<SCAFFOLD
$(_wt_status "$admin" "$wtpath" --untracked-files=all | grep -E "$reap_scaffold_re" || true)
SCAFFOLD
    for wid in $(tmux list-windows -a -F '#{window_id} #{pane_current_path} #{@worktree}' 2>/dev/null |
      awk -v p="$wtpath" '$2 == p || $3 == p {print $1}'); do
      tmux kill-window -t "$wid" 2>/dev/null || true
    done
    # Remove anchored (#677): --git-dir=<common> discovers nothing through the
    # tree's worker-writable .git, and --force skips git's clean-check child (a
    # status run in the tree), so no git reads the tree's config or attributes.
    # The re-checks just before it are the keep gates; a single --force still
    # refuses a locked worktree. The cwd guard covers reap's remaining
    # cwd-discovered calls (worktree list, show-ref, rev-parse).
    if ! _wt_cfg_guard "$common" "$admin" || ! _wt_cfg_guard_cwd "$common"; then
      note "keeping $branch — git config drift"
      continue
    fi
    if ! _wt_gitlink_ok "$admin" "$wtpath"; then
      say "keeping $branch — $tampered"
      continue
    fi
    # --force skips git's own submodule refusal, and the status below hides
    # gitlinks: one staged since the first gate would go with its embedded repo.
    if ! staged=$(_wt_git "$admin" "$wtpath" ls-files --stage); then
      say "keeping $branch — could not read its index"
      continue
    fi
    if grep -q '^160000 ' <<<"$staged"; then
      say "keeping $branch — it has submodules"
      continue
    fi
    if ! st=$(_wt_status "$admin" "$wtpath" --untracked-files=all); then
      say "keeping $branch — git status failed"
      continue
    fi
    if [ -n "$st" ]; then
      say "keeping $branch — uncommitted changes"
      continue
    fi
    _wt_git_common "$common" worktree remove --force "$wtpath" >/dev/null 2>&1 || true
    # Judge success by the observable outcome, not the exit status (#194). The
    # removal never deletes the branch; reap deletes it below for a MERGED PR.
    wtleft=$(git worktree list --porcelain |
      awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    if [ -z "$wtleft" ] && [ ! -e "$wtpath" ]; then
      reaped=$((reaped + 1))
      say "reaped $branch ($pr_state)"
      # The record's worktree is gone, so prune it now (#556). `|| true`: a
      # failed unlink (EACCES/EROFS) must not abort the reap row, branch delete
      # and label release below.
      rm -f -- "$anchor" || true
      line=$(jq -nc --arg branch "$branch" --arg pr "$pr" --arg pr_state "$pr_state" --arg wt "$wtpath" \
        '{ts:(now*1000|floor), kind:"reap", branch:$branch, pr:$pr, pr_state:$pr_state, worktree:$wt}')
      _bus_append "$log" "$line"

      # A squash-merged PR's branch is never an ancestor of main, so git
      # still reads it as unmerged — delete it deliberately now that
      # gh has confirmed the merge, but ONLY when the local tip is exactly
      # the merged PR head. A resumed run or a human may have committed past
      # the merge, and those commits would be orphaned by a forced delete
      # (recoverable via reflog, but not by glance). Only a MERGED PR: a
      # CLOSED PR's branch may hold work worth reviving, and a branch already
      # deleted (a real merge or a prior reap) is a no-op.
      if [ "$pr_state" = MERGED ] && git show-ref --verify --quiet "refs/heads/$branch"; then
        pr_head=$(gh pr view "$pr" --json headRefOid --jq .headRefOid 2>/dev/null || true)
        if [ -n "$pr_head" ] && [ "$(git rev-parse "refs/heads/$branch")" = "$pr_head" ]; then
          if _wt_cfg_guard "$common"; then
            _wt_git_common "$common" branch -D "$branch" >/dev/null 2>&1 || true
          else
            note "kept local branch $branch — git config drift"
          fi
        elif [ -z "$pr_head" ]; then
          note "kept local branch $branch — could not verify the merged PR head"
        else
          note "kept local branch $branch — tip diverges from the merged PR head"
        fi
      fi

      # Release the claim: drop `dispatched` from the issue(s) this PR
      # closes. Best-effort — a Linear PR closes no GitHub issue, and any gh
      # failure here must not block the sweep. The resolve call logs its own
      # failure rather than swallowing it, so it can't be confused with "no
      # closing issues" and leave the label stuck with no trace.
      if ! closing_issues=$(gh pr view "$pr" --json closingIssuesReferences \
        --jq '.closingIssuesReferences[].number' 2>&1); then
        note "could not resolve closing issues for PR $pr ($branch): $closing_issues"
        closing_issues=""
      fi
      for issue in $closing_issues; do
        gh issue edit "$issue" --remove-label dispatched >/dev/null 2>&1 ||
          note "could not remove the dispatched label from #$issue ($branch)"
      done
    elif [ ! -d "$admin" ]; then
      # git validates the gitfile before deleting anything, so a gone admin dir
      # means a real tree whose contents only partly went (e.g. a read-only
      # subdir). It is no longer a worktree, so no later reap will see it.
      say "keeping $branch — removal failed partway; $wtpath is no longer a worktree: tell the human (fix its permissions, then remove it)"
      rm -f -- "$anchor" || true
    elif ! _wt_gitlink_ok "$admin" "$wtpath"; then
      say "keeping $branch — $tampered"
    else
      say "keeping $branch — worktree removal failed"
    fi
  done <<EOF
$candidates
EOF
  [ -n "$dry" ] || [ "$reaped" -gt 0 ] || note "nothing reclaimed"
  ;;
*)
  # Bare `crew` and an unknown subcommand: the grouped list on stderr, not the
  # one 2,000-character line (#812). Exit stays 1.
  _crew_help '' >&2
  exit 1
  ;;
esac
