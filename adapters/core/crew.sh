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

# _resolve_target <target> [crew] — one TSV line (branch, codename, host, crew)
# per branch the target names, from the latest `dispatch` row of each branch in
# the bus: `#N`/`N` (branch `<type>/N-…`, or an `also_closes` entry), a Linear
# id (`eng-N-…`, or an `also_closes` entry), `worker:<branch>#<session>`, a
# branch, or a codename. `crew where` and `crew resolve-target` are Go (#902,
# crew/internal/resolve) with their own copy of this fold, so `nudge` is the
# last bash caller; a crew.bats row replays this helper against
# `crew resolve-target` on one fixture bus. The rows are unauthenticated
# (workers can append to the bus): a caller that acts on the result must still
# gate on the dispatcher-written worktree record.
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
# session has been handed}, one file per crew+session under $dir/await. `crew
# await` and `crew inbox` both raise it from Go (crew/internal/marks, the same
# path and the same two programs), so a reply taken through the straggler fold is
# not handed back by the next `await`. These three stay for `_unread_scan` (and
# through it `nudge` and `stall-watch --unread`), which read the same file, and
# for the crew.bats guards that replay Go's writes through them. Not a bus row:
# it never reaches `roster`, `watch` or another session's reads.
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

# tmux reads a C0 byte as terminal input, so a role assignment body must be one
# line. Matches the watcher's own gate (`_role_assignment_safe` in dispatch.sh):
# LC_ALL=C keeps [[:cntrl:]] meaning the C0 range under any ambient locale.
_has_c0() {
  local LC_ALL=C
  case "$1" in
  *[[:cntrl:]]*) return 0 ;;
  *) return 1 ;;
  esac
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

# _origin_repo — owner/name of this checkout's origin remote, or empty.
_origin_repo() {
  git config --get remote.origin.url 2>/dev/null |
    sed -E 's#(git@|https://)([^/:]+)[/:]##; s#\.git$##' || true
}

# _origin_github_repo — _origin_repo, only for a github.com origin: the slug
# drops the host, and _pr_url_in_repo only accepts github.com PR URLs.
_origin_github_repo() {
  case "$(git config --get remote.origin.url 2>/dev/null || true)" in
  https://github.com/* | git@github.com:*) _origin_repo ;;
  esac
}

# _pr_url_in_repo <url> <owner/name> — succeed when url is exactly a GitHub PR
# URL of that repo. A pr_url is unvalidated worker-written text, so it alone
# must never pick the repo gh is pointed at.
_pr_url_in_repo() {
  [[ $1 =~ ^https://github\.com/([^/]+)/([^/]+)/pull/[0-9]+$ ]] &&
    [ "${BASH_REMATCH[1]}/${BASH_REMATCH[2]}" = "$2" ]
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
# `dispatch`'s, which lists crew in its runtimeInputs — prepends its store bin dirs
# to PATH, so `command -v crew` in a running daemon answers the build that started it
# and never a home-manager switch: the ambient PATH a switch repoints starts at the
# first entry outside /nix/store. realpath'd to read like `_rr_self`, which is one.
# Always returns 0, printing nothing when nothing resolves: the caller captures it in
# an assignment, where a failing status would end the daemon.
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
  --timeout   Seconds to wait (default 300, capped at 600 — the tool ceiling).
              On expiry stdout stays empty and stderr gets "ended after Ns"
              — that line is the marker, the exit
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
usage: crew inbox <agent> [crew] [--since TS] [--from SENDER] [--undelivered]

Print the messages addressed to <agent> — messages only, never status rows.

  <agent>        worker:<branch>#s… (the session suffix is required), role:<branch>:<role>
  [crew]         Crew id; defaults to this repo's crew
  --since        One non-blocking pass over messages strictly newer than TS
  --from         Only messages from this sender (exact id)
  --undelivered  Only messages past this session's delivered marks; marks what it prints

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
usage: crew crews [--mine]

List every crew with traffic on this repo's bus: id, last and first event, worker
count, dispatcher pid and whether that pid is alive. Both sources count — a crew
dir alone (watch creates one) and an id seen only in the log.

  --mine  Print only the crews this caller owns — one id per line, no header —
          meaning the crews whose recorded dispatcher pid is a live ancestor of
          this process: the one it registered plus any 'crew adopt' re-attached
          to it. A dispatcher that adopted a restarted crew's workers uses this
          to watch every crew at once.

  crew crews
  crew crews --mine
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
usage: crew reap [--quiet] [--dry-run] [--no-wait] [--idle S] [--discard BRANCH]

Reclaim a worker's tmux window and worktree once its PR has landed. The PR, not
elapsed time, is the gate: a done worker sits for as long as its PR takes. The
PR is the latest session's, else any earlier session of the branch or a past
reap row, else `gh pr list --head`.

  --quiet     Suppress the per-worker notes
  --dry-run   Print what would be reclaimed, change nothing
  --no-wait   Do not wait out the idle threshold on a terminal lead
  --idle      Idle seconds before the idle-release phase kills the window (default 300)
  --discard BRANCH
              Save a finished worker's uncommitted state, then reclaim it

--discard acts on one done/failed/exited worker whose PR is MERGED or CLOSED,
or on a done/failed worker with no PR whose claimed issue(s) are all CLOSED. It
saves tracked, staged and untracked changes as one patch, <crew dir>/artifacts/
<branch>/discarded-<UTC ts>.patch (path printed; `git apply` it on the branch
tip). Ignored files are not saved, and a staged version since overwritten in
the work tree is not kept. The worktree is then removed the same anchored way,
keeping the local branch. It refuses an open PR, a live engine pane, a
non-terminal latest status, a busy reap lock and a tree holding an embedded git
repository. Never implicit: plain reap and the stream's reaps never discard.

Kept: an open PR, uncommitted changes, a live engine, a tip past the head of a
PR that is not the latest session's own, or no PR with a done/failed worker
whose claimed issue is not CLOSED, has no claim-issue row, or has commits on no
remote and not patch-equivalent to the default branch.

No crew filter: the workers worth reaping belong to earlier dispatcher sessions.

  crew reap --dry-run
  crew reap --discard feat/240-x
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
URL userinfo and query values in redirect values and URL-subsection keys are masked
with a sha256 fingerprint of the full value; the baseline stores full values.

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
    # tmux reads a control byte (newline, tab, escape, …) as terminal input, so
    # the role-watch types an assignment only when its body is one line. Refuse
    # it at send time for every role: recipient — a lead does not know its role's
    # delivery mode, and GRID_PROTOCOL.md treats assignments as one-line turns.
    case "$to" in
    role:*)
      if _has_c0 "${3:-}"; then
        echo "crew: msg: refusing to send to '$to': a role assignment body must be one line, but this one contains a control character (newline/tab/…); re-send it as compact JSON, e.g. jq -c" >&2
        exit 1
      fi
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
crews | log | report | sessions | roster | inbox | hold | await | watch | retro | reply | resolve-target | where | stream)
  # Ported to Go (crew/, docs/crew-go-port.md). CREW_GO_BIN is the
  # raw-source override; builds bake @crewGoBin@. CREW_SELF is this script's own
  # path — the `$0` the bash `stream` arm re-entered for its inner `watch`, its
  # `hold due` pre-check and its `crew reap`, which still live here.
  export CREW_SELF="$0"
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
  # The PATH this daemon started with, before any hop's wrapper prepended to it:
  # recorded once, exported so every hop's process keeps it, and restored on
  # every exec below — a hop then carries one wrapper prefix, not N.
  if [ -z "${CREW_RR_START_PATH+x}" ]; then
    export CREW_RR_START_PATH="$PATH"
  fi
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
  # The daemon's own argv, built once so `--detach` and the re-exec below start the
  # loop with the same validated arguments; `--pane` is a file record, not one.
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
  # record above already retargeted the running renderer, and that renderer is what
  # picks up a newer build (below), so a newcomer never has to displace it.
  _lock_acquire "$rr_lockd" "$$" || exit 0
  # `_lock_release` is unconditional; the pid check keeps a renderer whose lock
  # was reclaimed (crew dir removed and re-created) from deleting the new owner's.
  trap '[ "$(cat "$rr_lockd/pid" 2>/dev/null || true)" != "$$" ] || _lock_release "$rr_lockd"' EXIT
  trap 'exit 0' INT TERM
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
    # The sleep point is the only upgrade point, never mid-render. `exec` keeps the
    # pid, so the lock is never released or handed off: bash runs no EXIT trap on a
    # successful exec, and `_lock_acquire` is idempotent for the owner's own pid. A
    # crew already draining is left to exit rather than upgraded, because the hop
    # would restart its quiet window in the new build and the next dispatch starts
    # that build anyway. The entry is followed whenever it names a build other than
    # the one running — at start too, and a rollback is no exception; only the
    # running build is refused. PATH is restored to the recorded start first, so a
    # hop never grows it and a devshell's own entries ride along inside it.
    rr_want=$(_rr_installed_crew)
    if [ -z "$rr_idle_since" ] && [ -n "$rr_want" ] && [ "$rr_want" != "$_rr_self" ]; then
      PATH="${CREW_RR_START_PATH-$PATH}" exec "$rr_want" roster-render "${rr_args[@]}"
    fi
    sleep "$rr_interval"
  done
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
    # Ported to Go (crew/internal/rate, docs/crew-go-port.md): reads only the
    # global store — still zero network calls (spec §crew rate --report).
    # Flag parsing and the refusals stayed above; the flags pass through.
    go_args=(rate --report)
    if [ "$json" = true ]; then
      go_args+=(--json)
    fi
    if [ "$pooled" = true ]; then
      go_args+=(--pooled)
    fi
    exec "${CREW_GO_BIN:-@crewGoBin@}" "${go_args[@]}"
  fi

  # Ported to Go (crew/internal/rate, docs/crew-go-port.md): the run fold, the
  # burn table, the store lock shared with the arms that stayed bash, and the
  # GitHub reconcile. Flag parsing, the refusals and the --sweep-all loop above
  # stayed here, so the child re-enters this arm with no flags and inherits the
  # preamble's repo, bus and store paths.
  exec "${CREW_GO_BIN:-@crewGoBin@}" rate
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
  #
  # The loop runs in Go (crew/internal/stall, docs/crew-go-port.md); the comments
  # above document the behaviour it implements. `--sh <op>` is the Go side's
  # call-out into the bash helpers that stay here: release, nudge, unread,
  # budget (windows|limit) and local-model.
  # Every op exits its status + 10, so a failure before or around it — the
  # preamble's exit 1, a usage refusal, 127, a signal — never reads as a verdict.
  if [ "${1:-}" = --sh ]; then
    op="${2:-}"
    shift 2 || true
    rc=0
    case "$op" in
    release)
      _frame_classifier
      say() { :; }
      note() { :; }
      _release_windows "$@" "" || rc=$?
      ;;
    nudge)
      _frame_classifier
      out=$(_nudge_pane "$1" "$2" "$3" "$4" watchdog "$5") || rc=$?
      printf '%s\n' "$out"
      ;;
    unread) _unread_scan "$@" || rc=$? ;;
    budget)
      # shellcheck source=/dev/null
      . "${BUDGET_GATE_LIB:-@budgetGateLib@}"
      case "${1:-}" in
      windows)
        shift
        _budget_windows "$@" || rc=$?
        ;;
      limit)
        shift
        _budget_limit "$@" || rc=$?
        ;;
      *)
        echo "crew: stall-watch: unknown --sh budget predicate '${1:-}'" >&2
        exit 1
        ;;
      esac
      ;;
    local-model)
      # shellcheck source=/dev/null
      . "${LOCAL_MODELS_LIB:-@localModelsLib@}"
      _local_entry "$("${DISPATCH_CONFIG_BIN:-dispatch-config}" 2>/dev/null)" "$1" 2>/dev/null || true
      ;;
    *)
      echo "crew: stall-watch: unknown --sh op '$op'" >&2
      exit 1
      ;;
    esac
    exit $((rc + 10))
  fi
  CREW_SH="$(readlink -f "$0")" exec "${CREW_GO_BIN:-@crewGoBin@}" stall-watch "$@"
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
  # terminal, URL userinfo and query values in redirect values and URL-subsection
  # keys are masked with a sha256 fingerprint of the full value (the baseline
  # stores full values), and --accept writes exactly the pairs this run printed: each
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

  declare -A gb_base=() gb_seen=() gb_re=()
  gb_canon=
  gb_marked=
  gb_aliases=()
  gb_real="$(realpath -e -- "$common")" || gb_real=
  for gb_rec in "${gb_recs[@]}"; do
    [ -n "$gb_rec" ] || continue
    # shellcheck disable=SC2154 # set by the sourced worktree-git lib
    [ "$gb_rec" != "$_wt_cfg_redirect_mark" ] || gb_marked=1
    # A baselined alias the worker unset now can come back without drift.
    gb_key="${gb_rec%%$'\n'*}"
    if [[ $gb_rec == *$'\n'* && ($gb_key == url.*.insteadof || $gb_key == url.*.pushinsteadof) ]]; then
      gb_aliases+=("${gb_rec#*$'\n'}")
    fi
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

  # Every context's aliases apply to every pair: git in a linked worktree
  # may rewrite the main checkout's url through an alias only it sees.
  gb_shown=()
  gb_all=()
  for gb_ctx in "${gb_ctxs[@]}"; do
    gb_label="main checkout"
    [ "$gb_ctx" = "$common" ] || printf -v gb_label 'worktree %q' "${gb_ctx##*/}"
    mapfile -d '' gb_listing < <(git --git-dir="$gb_ctx" config --list --show-origin --show-scope -z)
    if ! wait $!; then
      echo "crew: cannot list the git config of $gb_ctx" >&2
      exit 1
    fi
    for ((gb_i = 0; gb_i + 2 < ${#gb_listing[@]}; gb_i += 3)); do
      gb_rec="${gb_listing[gb_i + 2]}"
      gb_all+=("$gb_label" "${gb_listing[gb_i]}" "${gb_listing[gb_i + 1]}" "$gb_rec")
      # A human's global insteadOf rewrites a local remote url too.
      [[ ${gb_rec%%$'\n'*} == url.*.insteadof || ${gb_rec%%$'\n'*} == url.*.pushinsteadof ]] || continue
      [[ $gb_rec == *$'\n'* ]] && gb_aliases+=("${gb_rec#*$'\n'}")
    done
  done
  for ((gb_j = 0; gb_j + 3 < ${#gb_all[@]}; gb_j += 4)); do
    [[ ${gb_all[gb_j + 1]} == local || ${gb_all[gb_j + 1]} == worktree ]] || continue
    gb_label="${gb_all[gb_j]}"
    gb_rec="${gb_all[gb_j + 3]}"
    [[ $gb_rec == *$'\n'* ]] || gb_rec+=$'\n'
    gb_key="${gb_rec%%$'\n'*}"
    gb_value="${gb_rec#"$gb_key"$'\n'}"
    _wt_cfg_guarded "$gb_key" "$gb_value" || continue
    _wt_cfg_canon "$gb_real" "$gb_rec" gb_canon
    [ -z "${gb_base["$gb_canon"]+x}" ] || continue
    gb_origin="${gb_all[gb_j + 2]#file:}"
    [ -z "${gb_seen["$gb_origin"$'\n'"$gb_rec"]+x}" ] || continue
    gb_seen["$gb_origin"$'\n'"$gb_rec"]=1
    _wt_cfg_show_pair "$gb_key" "$gb_value" gb_disp gb_aliases
    # shellcheck disable=SC2154 # set by _wt_cfg_show_pair
    printf '%s (%s, %q)\n' "$gb_disp" "$gb_label" "$gb_origin"
    gb_shown+=("$gb_rec")
    [[ $gb_key == url.*.insteadof || $gb_key == url.*.pushinsteadof ]] || continue
    # The alias may redirect a remote url accepted earlier while shown masked.
    # shellcheck disable=SC2034 # read by name in _wt_cfg_rewritten and _wt_cfg_show_pair
    gb_alias=("$gb_value")
    gb_re=()
    for ((gb_k = 0; gb_k + 3 < ${#gb_all[@]}; gb_k += 4)); do
      [[ ${gb_all[gb_k + 1]} == local || ${gb_all[gb_k + 1]} == worktree ]] || continue
      gb_rrec="${gb_all[gb_k + 3]}"
      gb_rkey="${gb_rrec%%$'\n'*}"
      [[ $gb_rrec == *$'\n'* && ($gb_rkey == remote.*.url || $gb_rkey == remote.*.pushurl) ]] || continue
      gb_rvalue="${gb_rrec#*$'\n'}"
      _wt_cfg_rewritten "$gb_rvalue" gb_alias || continue
      gb_rorigin="${gb_all[gb_k + 2]#file:}"
      [ -z "${gb_re["$gb_rorigin"$'\n'"$gb_rrec"]+x}" ] || continue
      gb_re["$gb_rorigin"$'\n'"$gb_rrec"]=1
      _wt_cfg_show_pair "$gb_rkey" "$gb_rvalue" gb_disp gb_alias
      printf '  rewritten by the alias above: %s (%s, %q)\n' "$gb_disp" "${gb_all[gb_k]}" "$gb_rorigin"
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
  discard=""
  discard_patch=""
  discard_st=""
  discard_tree=""
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
    --discard)
      [ -n "${2:-}" ] || {
        echo "crew: --discard needs a branch" >&2
        exit 1
      }
      discard="$2"
      shift
      ;;
    *)
      echo "crew: reap takes --quiet, --dry-run, --no-wait, --idle S and --discard BRANCH (got '$1')" >&2
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
  # Resolved once, after the cwd is trusted: bus pr_urls are checked against it.
  reap_repo=$(_origin_github_repo)
  # say: outcomes, always. note: kept-worker bookkeeping, silenced under --quiet
  # so the dispatch call site stays silent unless something actually happened.
  say() { echo "crew reap: $1"; }
  note() { [ -n "$quiet" ] || echo "crew reap: $1"; }
  refuse() {
    echo "crew reap: refusing --discard $discard — $1" >&2
    exit 1
  }
  if [ ! -f "$log" ]; then
    [ -z "$discard" ] || refuse "latest status is none"
    exit 0
  fi

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

  # _reap_pick_pr <own> <earlier> <reap-row> — the first bus PR, in that order,
  # that is a PR of this repo: sets pr and pr_src ("own" only for the latest
  # session's), else pr "-" for _reap_find_pr. A mismatch is never MERGED or
  # CLOSED, only "no PR from that source"; ignored names the last one skipped.
  _reap_pick_pr() {
    local u src=own
    pr="-"
    pr_src=fallback
    ignored=""
    for u in "$@"; do
      if [ "$u" != - ]; then
        if _pr_url_in_repo "$u" "$reap_repo"; then
          pr=$u
          pr_src=$src
          return 0
        fi
        ignored="ignoring cross-repo pr_url ${u//[[:cntrl:]]/?}"
      fi
      src=fallback
    done
  }

  # _reap_find_pr — with pr "-", look the branch's PR up on GitHub: pr becomes
  # its URL (pr_src fallback), or stays "-" when there is none. Status 1 sets why.
  _reap_find_pr() {
    local prs
    [ "$pr" = "-" ] || return 0
    pr_src=fallback
    if ! prs=$(gh pr list --head "$branch" --state all --json url,state,headRefOid,isCrossRepository 2>/dev/null); then
      why="could not list PRs for $branch"
      return 1
    fi
    # A fork's PR on the same head name is not this branch's.
    if [ -n "$prs" ] && ! pr=$(jq -er 'map(select(.isCrossRepository == false)) | first
        | if . == null then "-"
          elif (.url | type == "string" and test("^https://[^\\s]+/pull/[0-9]+$")) then .url
          else error("not a PR url") end' <<<"$prs" 2>/dev/null); then
      why="could not read PRs for $branch"
      return 1
    fi
  }

  # _reap_issue_closed — no PR: a done/failed branch whose claimed issues are
  # all CLOSED. Sets issues, pr, pr_state, label and nopr; status 1 sets why.
  _reap_issue_closed() {
    local claimed n istate
    local -a claimed_list
    nopr="$state but no PR on the bus or GitHub${ignored:+ ($ignored)}"
    case "$state" in
    done | failed) ;;
    *)
      why="$nopr; only done/failed reclaim on a closed issue"
      return 1
      ;;
    esac
    if ! claimed=$(jq -s -r --arg b "$branch" '
        map(select(.kind == "claim-issue" and .branch == $b and .issue != null) | .issue | tostring)
        | unique | .[]' "$log" 2>/dev/null); then
      why="$nopr; could not read the bus"
      return 1
    fi
    if [ -z "$claimed" ]; then
      why="$nopr; no claim-issue row"
      return 1
    fi
    mapfile -t claimed_list <<<"$claimed"
    issues=()
    for n in "${claimed_list[@]}"; do
      if ! [[ "$n" =~ ^[0-9]+$ ]]; then
        why="$nopr; claim-issue row names a non-numeric issue"
        return 1
      fi
      istate=$(gh issue view "$n" --json state --jq .state 2>/dev/null || true)
      if [ -z "$istate" ]; then
        why="$nopr; could not read issue #$n"
        return 1
      fi
      if [ "$istate" != CLOSED ]; then
        why="$nopr; issue #$n is $istate"
        return 1
      fi
      issues+=("$n")
    done
    pr=""
    pr_state=ISSUE_CLOSED
    label="issue #${issues[0]}"
    for n in "${issues[@]:1}"; do
      label+=", #$n"
    done
    label+=" CLOSED"
  }

  # _reap_remove — the anchored removal tail shared by the candidate loop and
  # --discard. Reads the loop's branch wtpath admin state pr pr_state label
  # mode issues, plus discard discard_patch discard_st discard_tree (empty in
  # plain reap; under --discard it also uses that block's _discard_tree and
  # tmpidx); sets removed=1 on success. Always returns 0: callers run it
  # bare, since a conditional call would switch set -e off inside it.
  _reap_remove() {
    removed=""
    # Kill leftover processes reparented out of the pane but still rooted in this
    # worktree (the #187 class: `yes` hogs reparented to systemd). Skip our own
    # process and its ancestors; a dangling cwd (dir already gone) still matches
    # by string. Runs BEFORE the --dry-run return so dry runs report it too.
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
      say "would reap $branch ($label) @ $wtpath"
      say "would prune record $anchor"
      return 0
    fi

    # Every scaffold artifact is untracked, and the re-check before the removal
    # treats ANY status line as dirt — the dirt check above only declares them
    # non-dirt so our own pipeline never pins a finished tree forever; each
    # file must ALSO be moved out physically, or the re-check keeps the tree
    # (feat/113-116-127-142 all sat blocked by untracked PLAN.md/SPEC.md
    # alone). gtrash everything so a post-mortem can still recover it.
    # --discard's patch already holds them, and its re-check compares
    # against the status saved with it, so they stay put.
    if [ -z "$discard" ]; then
      while IFS= read -r line; do
        [ -n "$line" ] || continue
        scaffold=$(printf '%s' "$line" | sed -nE 's/^\?\? (.*)$/\1/p')
        [ -n "$scaffold" ] || continue
        gtrash put "$wtpath/$scaffold" >/dev/null 2>&1 || true
      done <<SCAFFOLD
$(_wt_status "$admin" "$wtpath" --untracked-files=all | grep -E "$reap_scaffold_re" || true)
SCAFFOLD
    fi
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
      return 0
    fi
    if ! _wt_gitlink_ok "$admin" "$wtpath"; then
      say "keeping $branch — $tampered"
      return 0
    fi
    # --force skips git's own submodule refusal, and the status below hides
    # gitlinks: one staged since the first gate would go with its embedded repo.
    if ! staged=$(_wt_git "$admin" "$wtpath" ls-files --stage); then
      say "keeping $branch — could not read its index"
      return 0
    fi
    if grep -q '^160000 ' <<<"$staged"; then
      say "keeping $branch — it has submodules"
      return 0
    fi
    if ! st=$(_wt_status "$admin" "$wtpath" --untracked-files=all); then
      say "keeping $branch — git status failed"
      return 0
    fi
    # A new path or state change after the --discard save (a killed process
    # flushing on exit) is not in the patch, so it keeps the tree. Status lines
    # carry no content, so the tree comparison catches a later write to an
    # already-dirty path.
    if [ -n "$discard" ]; then
      if [ "$st" != "$discard_st" ]; then
        say "keeping $branch — changed while saving"
        return 0
      fi
      tmpidx=$(mktemp -d "$dir/discard.XXXXXX") || tmpidx=""
      now_tree=""
      if [ -n "$tmpidx" ] && _discard_tree; then
        now_tree=$(GIT_INDEX_FILE="$tmpidx/index" _wt_git "$admin" "$wtpath" write-tree 2>/dev/null) || now_tree=""
      fi
      [ -z "$tmpidx" ] || rm -rf -- "$tmpidx"
      tmpidx=""
      if [ -z "$now_tree" ]; then
        say "keeping $branch — could not re-check its uncommitted state"
        return 0
      fi
      if [ "$now_tree" != "$discard_tree" ]; then
        say "keeping $branch — changed while saving"
        return 0
      fi
    elif [ -n "$st" ]; then
      say "keeping $branch — uncommitted changes"
      return 0
    fi
    _wt_git_common "$common" worktree remove --force "$wtpath" >/dev/null 2>&1 || true
    # Judge success by the observable outcome, not the exit status (#194). The
    # removal never deletes the branch; reap deletes it below for a MERGED PR.
    wtleft=$(git worktree list --porcelain |
      awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    if [ -z "$wtleft" ] && [ ! -e "$wtpath" ]; then
      removed=1
      say "reaped $branch ($label${discard:+, discarded})"
      # The record's worktree is gone, so prune it now (#556). `|| true`: a
      # failed unlink (EACCES/EROFS) must not abort the reap row, branch delete
      # and label release below.
      rm -f -- "$anchor" || true
      line=$(jq -nc --arg branch "$branch" --arg pr "$pr" --arg pr_state "$pr_state" --arg wt "$wtpath" --arg mode "$mode" \
        --arg discard "$discard" --arg patch "$discard_patch" \
        '{ts:(now*1000|floor), kind:"reap", branch:$branch, pr:$pr, pr_state:$pr_state, worktree:$wt}
        + (if $mode == "issue" then {issues: $ARGS.positional} else {} end)
        + (if $discard != "" then {discarded: $patch} else {} end)' --args "${issues[@]}")
      _bus_append "$log" "$line"

      # A squash-merged PR's branch is never an ancestor of main, so git
      # still reads it as unmerged — delete it deliberately now that
      # gh has confirmed the merge, but ONLY when the local tip is exactly
      # the merged PR head. A resumed run or a human may have committed past
      # the merge, and those commits would be orphaned by a forced delete
      # (recoverable via reflog, but not by glance). Only a MERGED PR: a
      # CLOSED PR's branch may hold work worth reviving, and a branch already
      # deleted (a real merge or a prior reap) is a no-op. --discard keeps it:
      # its patch applies to that branch's tip.
      if [ -z "$discard" ] && [ "$mode" = pr ] && [ "$pr_state" = MERGED ] && git show-ref --verify --quiet "refs/heads/$branch"; then
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
      if [ "$mode" = issue ]; then
        closing_issues="${issues[*]}"
      elif ! closing_issues=$(gh pr view "$pr" --json closingIssuesReferences \
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
  # --discard acts on one branch only.
  [ -z "$discard" ] || autosweep=0
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
    # A skipped --discard would read as done, its state neither saved nor reclaimed.
    if [ -n "$nowait" ]; then
      [ -z "$discard" ] || refuse "another reap is running"
      note "another reap is running — skipped"
      exit 0
    fi
    if [ "$waited" -ge 120 ]; then
      [ -z "$discard" ] || refuse "another reap is still running after 120s"
      note "another reap is still running after 120s — skipped"
      exit 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
  tmpidx=""
  trap '_lock_release "$reap_lock"; [ -z "$tmpidx" ] || rm -rf -- "$tmpidx"' EXIT

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

  # _reap_latest [branch] — latest status per worker across every crew; keep
  # the ones in a terminal state (the idle-release pass's set too — see
  # $reap_terminal_states).
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
  # Fields: branch state own earlier reaprow ts later. The three PR sources stay
  # apart ("-" when absent) so _reap_pick_pr can validate each and decide pr_src:
  # own is the latest session's pr_url, earlier the newest of the branch's other
  # sessions, reaprow the newest reap row's.
  # With a branch: that branch's row whatever its state ("none" when no
  # status), for --discard.
  _reap_latest() {
    jq -s -r --arg only "${1:-}" --argjson terminal "$reap_terminal_states" --argjson claim_ttl "$claim_mask_ttl" '
        def wid_branch: ltrimstr("worker:") | sub("#[^#]*$";"");
        (map(select((.kind == "msg" or .kind == "status") and ((.from // "") | startswith("worker:")))
            | {branch: (.from | wid_branch), ts})
          | group_by(.branch) | map({key: .[0].branch, value: (map(.ts) | max)}) | from_entries) as $last
        # The latest session of a re-dispatched branch may not carry the PR.
        | (map(select(.kind == "reap" and ((.pr // "") | type == "string" and . != ""))
              | {branch, ts, pr}) | group_by(.branch)
            | map({key: .[0].branch, value: (max_by(.ts) | .pr)}) | from_entries) as $reapprs
        | (map(select(.kind == "status" and ((.from // "") | startswith("worker:")) and (.body.pr_url | type == "string" and . != ""))
              | {branch: (.from | wid_branch), from, ts, pr: .body.pr_url})) as $statprs
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
                   pr_url: (map(.body.pr_url) | map(select(type == "string" and . != "")) | last)})
            | map(select(.branch != ""))
            | group_by(.branch) | map(sort_by(.ts) | last)
            | if $only == "" then map(select(.state as $st | (($terminal + ["pr_open"]) | index($st)) != null))
              else map(select(.branch == $only)) end
          )
        | .[] | . as $c
        | [.branch, (.state | if . == null or . == "" then "none" else . end), (.pr_url // "-"),
            ([$statprs[] | select(.branch == $c.branch and .from != $c.session)] | if length == 0 then "-" else max_by(.ts) | .pr end),
            ($reapprs[.branch] // "-"), (.ts // 0), (if ($last[.branch] // 0) > .ts then "1" else "0" end)] | @tsv' "$log"
  }

  # _artifacts_dir_bad <branch> — dispatch.sh's, on $dir: succeed, printing the
  # first offender, when a component from $dir/artifacts down to the branch's
  # leaf is a symlink or exists as a non-directory.
  _artifacts_dir_bad() {
    local p="$dir/artifacts" part
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

  # --discard <branch>: a human's explicit call to drop a finished worker's
  # uncommitted state. Every refusal below touches nothing; the state is saved
  # as a patch before the shared removal tail runs.
  if [ -n "$discard" ]; then
    command -v gh >/dev/null || refuse "needs gh"
    # --branch also expands @{-N}, so the name must come back unchanged.
    canon=$(git check-ref-format --branch "$discard" 2>/dev/null) || refuse "not a valid branch name"
    [ "$canon" = "$discard" ] || refuse "not a valid branch name"
    branch="$discard"
    wtpath=$(git worktree list --porcelain |
      awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    [ -n "$wtpath" ] && [ -d "$wtpath" ] || refuse "no worktree for it"
    latest=$(_reap_latest "$branch") || refuse "could not read the bus"
    state=none
    pr="-"
    own="-" earlier="-" reaprow="-"
    [ -z "$latest" ] || IFS=$'\t' read -r _ state own earlier reaprow _ _ <<<"$latest"
    case "$state" in
    done | failed | exited) ;;
    *) refuse "latest status is $state" ;;
    esac
    live=$(tmux list-panes -a -F $'#{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}' 2>/dev/null || true)
    while IFS=$'\t' read -r _ _ pcmd ppath; do
      [ -n "$pcmd" ] || continue
      ! _pane_is_engine_at "$pcmd $ppath" "$wtpath" || refuse "an engine is running there"
    done <<<"$live"
    case "$reap_cwd/" in
    "$wtpath"/*) refuse "it is the current worktree" ;;
    esac
    mode="pr"
    issues=()
    _reap_pick_pr "$own" "$earlier" "$reaprow"
    _reap_find_pr || refuse "$why"
    if [ "$pr" = "-" ]; then
      mode=issue
      _reap_issue_closed || refuse "$why"
    else
      pr_state=$(gh pr view "$pr" --json state --jq .state 2>/dev/null || true)
      case "$pr_state" in
      MERGED | CLOSED) ;;
      "") refuse "could not read PR state ($pr)" ;;
      *) refuse "PR $pr_state" ;;
      esac
      label="$pr_state"
    fi
    admin=$(_wt_admin_dir "$common" "$wtpath") || refuse "no git admin dir for $wtpath"
    _wt_gitlink_ok "$admin" "$wtpath" || refuse "its .git does not point at its git admin dir"
    if [ -e "$admin/config.worktree" ] || [ -L "$admin/config.worktree" ]; then
      refuse "$admin/config.worktree exists"
    fi
    staged=$(_wt_git "$admin" "$wtpath" ls-files --stage) || refuse "could not read its index"
    ! grep -q '^160000 ' <<<"$staged" || refuse "it has submodules"
    { _wt_cfg_guard "$common" "$admin" && _wt_cfg_guard_cwd "$common"; } || refuse "git config drift"

    pre_st=$(_wt_status "$admin" "$wtpath" --untracked-files=all) || refuse "git status failed"
    discard_patch="$dir/artifacts/$branch/discarded-$(date -u +%Y%m%dT%H%M%SZ).patch"
    if [ -n "$dry" ]; then
      if [ -n "$pre_st" ]; then
        say "would save uncommitted state to $discard_patch"
      else
        say "no uncommitted changes to save"
      fi
      say "would reap $branch ($label) @ $wtpath"
      exit 0
    fi
    # A throwaway index seeded from the branch tip: `add -A` then stages
    # tracked, staged and untracked non-ignored files without touching the
    # worker's own index. The diff captures the work tree as `add -A` sees it:
    # a staged version since overwritten in the work tree is not kept.
    # $dir is ours, so the temp dir and the patch built in it are too.
    tmpidx=$(mktemp -d "$dir/discard.XXXXXX") || refuse "could not save uncommitted state"
    discard_fail() {
      rm -rf -- "$tmpidx"
      tmpidx=""
      refuse "$1"
    }
    _discard_tree() {
      GIT_INDEX_FILE="$tmpidx/index" _wt_git "$admin" "$wtpath" read-tree "refs/heads/$branch" >/dev/null 2>&1 &&
        GIT_INDEX_FILE="$tmpidx/index" _wt_git "$admin" "$wtpath" add -A >/dev/null 2>&1
    }
    _discard_tree || discard_fail "could not save uncommitted state"
    # add -A stages an untracked nested repo as a bare gitlink: the patch would
    # hold only its commit id while the removal deletes its contents.
    tmpstage=$(GIT_INDEX_FILE="$tmpidx/index" _wt_git "$admin" "$wtpath" ls-files --stage) ||
      discard_fail "could not save uncommitted state"
    ! grep -q '^160000 ' <<<"$tmpstage" || discard_fail "it holds an embedded git repository"
    discard_tree=$(GIT_INDEX_FILE="$tmpidx/index" _wt_git "$admin" "$wtpath" write-tree) ||
      discard_fail "could not save uncommitted state"
    # Pinned diff options: the operator's diff.* and color.* config would
    # otherwise reshape the patch so `git apply` rejects it.
    GIT_INDEX_FILE="$tmpidx/index" _wt_git "$admin" "$wtpath" diff --cached --binary --full-index \
      --no-color --src-prefix=a/ --dst-prefix=b/ --no-relative \
      --no-ext-diff --no-textconv "refs/heads/$branch" -- >"$tmpidx/patch" 2>/dev/null ||
      discard_fail "could not save uncommitted state"
    if [ -s "$tmpidx/patch" ]; then
      # artifacts/<branch> is worker-writable: a symlink on it redirects the patch.
      ! bad=$(_artifacts_dir_bad "$branch") || discard_fail "$bad is a symlink or not a directory"
      mkdir -p -- "$dir/artifacts/$branch" || discard_fail "could not save uncommitted state"
      ! bad=$(_artifacts_dir_bad "$branch") || discard_fail "$bad is a symlink or not a directory"
      if [ -e "$discard_patch" ] || [ -L "$discard_patch" ]; then
        discard_fail "$discard_patch already exists"
      fi
      mv -nT -- "$tmpidx/patch" "$discard_patch" 2>/dev/null || true
      if [ -e "$tmpidx/patch" ] || [ ! -f "$discard_patch" ] || [ -L "$discard_patch" ]; then
        discard_fail "could not save uncommitted state"
      fi
      say "saved uncommitted state to $discard_patch"
    else
      discard_patch=""
      say "no uncommitted changes to save"
    fi
    rm -rf -- "$tmpidx"
    tmpidx=""
    if ! post_st=$(_wt_status "$admin" "$wtpath" --untracked-files=all) || [ "$post_st" != "$pre_st" ]; then
      say "keeping $branch — changed while saving"
      exit 1
    fi
    discard_st=$pre_st
    _reap_remove
    [ -n "$removed" ] || exit 1
    exit 0
  fi

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
    map(select((.kind=="status" or .kind=="claim") and ((.from // "") | startswith("worker:")) and ((.from | wid_branch) != "")))
    | group_by(.from) | map(max_by(.ts))
    | group_by(.from | wid_branch) | map(max_by(.ts))
    | map(select(.body.state as $st | ($terminal | index($st)) != null))
    # A watchdog-posted failed marks a hung pane: keep it as evidence.
    | map(select((.body.source // "") != "watchdog" or .body.state != "failed"))
    | map(select((((now*1000) - .ts) / 1000) >= $idle))
    | .[] | [(.from | wid_branch), (((.from | wid_session) // "") | if . == "" then "-" else . end), .body.state, .ts] | @tsv' "$log")
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

  candidates=$(_reap_latest)
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
  while IFS=$'\t' read -r branch state own earlier reaprow cand_ts later; do
    [ -n "$branch" ] || continue
    wtpath=$(git worktree list --porcelain |
      awk -v b="refs/heads/$branch" '/^worktree /{p=$2} $0=="branch "b{print p}')
    [ -n "$wtpath" ] && [ -d "$wtpath" ] || continue

    mode="pr"
    issues=()
    _reap_pick_pr "$own" "$earlier" "$reaprow"
    if ! _reap_find_pr; then
      note "keeping $branch — $why${ignored:+ ($ignored)}"
      continue
    fi
    if [ "$pr" = "-" ]; then
      mode=issue
      if ! _reap_issue_closed; then
        note "keeping $branch — $why"
        continue
      fi
    else
      pr_state=$(gh pr view "$pr" --json state --jq .state 2>/dev/null || true)
      case "$pr_state" in
      MERGED | CLOSED) ;;
      "")
        note "keeping $branch — could not read PR state ($pr)${ignored:+ ($ignored)}"
        continue
        ;;
      *)
        note "keeping $branch — PR $pr_state${ignored:+ ($ignored)}"
        continue
        ;;
      esac
      label="$pr_state"
      # A PR from an earlier session, a reap row or GitHub predates any later
      # session's work: it vouches only for a tip at or behind its head.
      if [ "$pr_src" != own ]; then
        pr_head=$(gh pr view "$pr" --json headRefOid --jq .headRefOid 2>/dev/null || true)
        if [ -z "$pr_head" ]; then
          note "keeping $branch — could not verify $pr's head${ignored:+ ($ignored)}"
          continue
        fi
        if ! _wt_cfg_guard "$common"; then
          note "keeping $branch — git config drift"
          continue
        fi
        anc_rc=0
        _wt_git_common "$common" merge-base --is-ancestor "refs/heads/$branch" "$pr_head" 2>/dev/null || anc_rc=$?
        if [ "$anc_rc" = 1 ]; then
          note "keeping $branch — its tip has commits past $pr${ignored:+ ($ignored)}"
          continue
        elif [ "$anc_rc" != 0 ]; then
          note "keeping $branch — could not compare with $pr head (not fetched?)${ignored:+ ($ignored)}"
          continue
        fi
      fi
    fi

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

    # No PR vouches for the branch, so every commit must already be on a
    # remote or patch-equivalent to one on the default branch.
    if [ "$mode" = issue ]; then
      if ! _wt_cfg_guard "$common"; then
        note "keeping $branch — git config drift"
        continue
      fi
      if ! def=$(_wt_git_common "$common" symbolic-ref -q refs/remotes/origin/HEAD); then
        def=""
        if _wt_git_common "$common" show-ref --verify --quiet refs/remotes/origin/main; then
          def=refs/remotes/origin/main
        fi
      fi
      if [ -z "$def" ]; then
        note "keeping $branch — $nopr; no origin default branch ref"
        continue
      fi
      if ! unpushed=$(_wt_git_common "$common" rev-list "refs/heads/$branch" --not --remotes) ||
        ! cherry=$(_wt_git_common "$common" cherry "$def" "refs/heads/$branch"); then
        note "keeping $branch — $nopr; could not compare its commits to ${def#refs/remotes/}"
        continue
      fi
      ahead=0
      while read -r mark sha; do
        [ "$mark" = + ] || continue
        if grep -qxF -- "$sha" <<<"$unpushed"; then
          ahead=$((ahead + 1))
        fi
      done <<<"$cherry"
      if [ "$ahead" -gt 0 ]; then
        note "keeping $branch — $nopr; $ahead commit(s) not on any remote and not patch-equivalent to ${def#refs/remotes/}"
        continue
      fi
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

    _reap_remove
    [ -z "$removed" ] || reaped=$((reaped + 1))
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
