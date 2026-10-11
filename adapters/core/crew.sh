# shellcheck shell=bash
# File-based coordination bus: id | identity | status | msg | watch | roster | fleet | roster-render | inbox | stall-watch | pr-watch | log
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
#
# Picking a slot moved to Go with #981 (crew/internal/identity), leaving no bash
# caller for these three. They stay as the canonical pool: crew.bats's
# `_rr_palette` enumerates `_colors` from here to compile every palette stroke
# through d2, and identity_test.go asserts the Go pools against all three.
_names=(sage atlas nova ember reef iris amber coral moss slate rust plum lime rose sky onyx
  pine lagoon indigo fern bronze violet khaki ash brick mauve tan crimson fuchsia blush orchid cobalt)
_colors=(green blue magenta orange teal purple yellow salmon olive steel rust plum lime pink sky grey
  seagreen darkcyan indigo forestgreen darkgoldenrod mediumpurple darkkhaki dimgrey indianred palevioletred peru crimson mediumvioletred hotpink orchid royalblue)
_tmuxc=(colour28 colour32 colour127 colour130 colour30 colour98 colour136 colour167 colour100 colour67 colour166 colour96 colour64 colour162 colour25 colour244
  colour29 colour31 colour61 colour65 colour94 colour97 colour101 colour102 colour131 colour132 colour137 colour160 colour163 colour168 colour169 colour68)

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

# The _pid_alive.._recorded_pid_live helpers below have no caller in this file;
# they are the canonical copies tests/adapters.bats pins dispatch.sh and
# dispatch-resume.sh against.

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

# _frame_classifier — defines the claude/codex pane-frame predicates and their
# regexes. No arm calls it since stall-watch and reap went to Go (crew/internal/frame
# is the port); it stays as the canonical copy the drift tests and tests/adapters.bats
# read. Bodies stay 2-space indented: tests/adapters.bats byte-compares them against
# dispatch.sh's --role-watch copies.
# shellcheck disable=SC2329 # the predicates it defines are only called by those tests
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

# CREW_CLOCK=<file> is a test-only virtual clock for the waits in `await`, the
# hold paths and `stall-watch`: _clock_now reads epoch seconds from it, and the
# Go clock (crew/internal/clock) reads and advances it the same way — `crew nudge
# --wait` among its callers. It starts at the real time and only moves forward, so
# bus rows (real ms) posted during a run still sort at or before `now`, as they do
# in production. Unset, these are exactly `date +%s` and `sleep`.
_clock_now() {
  [ -n "${CREW_CLOCK:-}" ] || { date +%s; return; }
  [ -s "$CREW_CLOCK" ] || date +%s >"$CREW_CLOCK"
  cat "$CREW_CLOCK"
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
# not handed back by the next `await`. These three stay for the crew.bats guards
# that replay Go's writes through them (the unread scan reads the same file from
# Go since #968, via crew/internal/unread). Not a bus row:
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

# _bus_append <log> <line> — append <line> + newline to <log> as a single
# write(2) (#55). `iflag=fullblock` makes dd keep reading until its buffer is
# full or EOF instead of writing out whatever a single short read from the
# pipe returned — without it, dd's own read/write is no more atomic than the
# `printf >>` it replaces. Only POSIX-common flags (no GNU-only `oflag=append`
# or `of=`), so this also works under macOS's system BSD `dd`. `dispatch.sh`
# and `dispatch-notify.sh` are separate binaries with no shared lib to source
# this from, so each carries its own copy — keep them in sync (#61).
#
# Since the `stack` port (#1004) no crew.sh arm calls this any more — every
# Go arm appends through `internal/bus.Append` — but it stays as the canonical
# copy adapters.bats byte-compares the two standalone copies against, the same
# arrangement as `_pid_alive` and `_is_quota_cursor_limit`.
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
  fleet           One roster across every crew bus on the machine
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
  stack           Read or record stacked cut points; plan a stack rebase
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
  <state>    working | blocked | pr_open | done | failed | exited | reaped (only reap posts it)
  [detail]   Short context; put -- before one that starts with a dash
  [pr]       PR url, which pr_open names
  --restamp  blocked only: refresh liveness without re-delivering a reply
  --         Everything after this is positional

pr_open and done are refused on a standard/deep implement run until the branch
carries a review seam and a deslop seam, plus a plan seam when its task doc says
plan: required.

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
  fleet)
    cat <<'HELP'
usage: crew fleet [--all] [--crew ID] [--json]

One row per live branch across every crew bus on the machine, from any
directory (no git repo needed). Buses come from the repo registry plus a walk of
$CREW_SWEEP_ROOTS (colon-separated; default $HOME). Rows are the roster's, with
a repo column; by default only working, blocked and pr_open branches show.

  --all       Every branch, whatever its state
  --crew ID   Only this crew id
  --json      A JSON array of roster rows plus repo and crew_id

Repos that are unreadable or gone are noted on stderr and skipped.

  crew fleet --json | jq -r '.[] | [.repo, .branch, .state] | @tsv'

usage: crew fleet stream --crew ID [--rediscover S] [--states a,b,c] [--park S]
                         [--heartbeat S] [--coalesce S] [--retry S]
                         [--interval S] [--force]
       crew fleet stream --status --crew ID

One long-running lane that delivers a crew's bus events from every repo on the
machine, so a dispatcher whose workers land in other repos needs one Monitor
instead of one `crew stream` per repo. Lines are `crew stream`'s, and each batch
carries a top-level `repo` (the label `crew fleet` shows); error lines name the
repo they concern, and the lane's own heartbeat has none.

  --crew ID       The crew to follow (required; no ambient CREW_ID)
  --rediscover S  Look for newly joined buses every S seconds (default 300)
  --status        Report the lane (alive, stale or dead; exit 0, 1 or 2)
  --force         Replace a live lane, as `crew stream --force`
  the rest        As `crew stream`; --reap-every is refused

A bus joins once it holds a row for the crew, so no cursor is ever seeded in an
unrelated repo. It uses the same per-crew cursor `crew watch` keeps there; a
repo joined with no cursor file replays its backlog of qualifying events on its
first park, as a bare `crew watch` does.

hold_due and reap stay per repo: the fleet lane does not run reaps and does not
evaluate holds. The per-repo `crew stream` or `dispatch` does. A bus whose
per-repo stream is alive is left to it (noted once on stderr), so one event is
never delivered twice. A per-repo stream started while the fleet lane is
mid-park shows `another watch is already running` error lines until that park
ends (at most --park seconds).

Like `crew stream`, the lane holds the watch for each repo it has joined, so a
bare `crew watch` for that crew in that repo is refused ("another watch is
already running") while the lane runs.

The lane lock lives under
${XDG_STATE_HOME:-~/.local/state}/crew/fleet-stream/<crew>, outside every repo.
Gone repos and repos without a bus are noted once on stderr and skipped.

  crew fleet stream --crew <crew-id>
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
  --quiet      Exit after S seconds with no working/blocked/dispatched worker
               and no outstanding hold (default 1800; 0 exits as soon as the
               crew drains)

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
Legacy remotes/ and branches/ entries are listed and never accepted — remove them.

  crew git-baseline
HELP
    ;;
  stack)
    cat <<'HELP'
usage: crew stack cut [--record] <child-branch|PR#> [<oid>]

Print the parent oid a stacked child was last cut from, as the dispatcher
recorded it: the child's dispatch row (base_oid) or a later --record. Worker
status and msg rows are never read. Exit 1 when nothing is recorded: ask the
child, never guess.

  --record  Fetch origin/<parent> and origin/<child>, and when the parent tip is
            an ancestor of the child, record it as the new cut and print it.
            Refuses, writing nothing, when the child is not rebased onto it.
  <oid>     With --record: the cut the child reports. Recorded only when it is a
            commit and an ancestor of origin/<child>; the parent tip is not
            checked, as the parent may have moved. Refuses otherwise,
            including the child's own head.

  crew stack cut --record feat/240-child
  crew stack cut --record feat/240-child 0123456789abcdef0123456789abcdef01234567

usage: crew stack align <top-PR#|branch>

Plan the rebase of a whole stack from its top PR down to the default branch.
Read-only: nothing is written to the bus, dispatched, replied or pushed. Prints
one JSON line per layer, bottom to top:

  {pr, branch, parent, parent_pr, cut, mode, ready, directive, reason}

  mode       none (nothing to do), ff (git rebase origin/<parent>), onto
             (git rebase --onto origin/<parent> <cut>), or unknown (no usable
             recorded cut or parent: ask the child, never guess).
  directive  The text to send the child; null for none and unknown.
  ready      True for at most one layer: the lowest with work to do, and only
             when its cut is known; an unknown lowest layer leaves every layer
             false. Layers above it wait for its push, with their onto plan
             recomputed against the rewritten parent.

A layer's record is its newest dispatch or stack_cut row carrying both a parent
and a cut. A parent whose PR was squash-merged, or a layer retargeted onto the
default branch after it, rebases onto the default branch and names the merged
PR. The stack bottom, based on the default branch, is never rebased. A PR base
that differs from the recorded parent is unknown, as is a missing cut or one no
longer in the layer's branch. A branch name unsafe to put in a directive is
refused with no output.

  crew stack align 240
  crew stack align feat/240-top
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
fleet)
  # `fleet stream` is a sub-subcommand: its --help is the second argument.
  if [ "${1:-}" = stream ]; then
    case "${2:-}" in
    --help | -h)
      _crew_help fleet
      exit $?
      ;;
    esac
  fi
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
  # Ported to Go (crew/internal/reap); it needs no repo, so it execs before the
  # preamble resolves one.
  exec "${CREW_GO_BIN:-@crewGoBin@}" occupants "$@"
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

# The repo check. --path-format=absolute so main-checkout and worktrees resolve
# to a byte-identical path (load-bearing — see #29). Every arm below execs the
# Go binary, which resolves the bus dir itself (bus.Locate); this `dir` is left
# for `_await_state`, the marks-path helper the crew.bats guards replay, and no
# arm keeps a copy of `<common>/crew/events.jsonl` any more.
common=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)
if [ -z "$common" ]; then
  # `rate --sweep-all` walks every repo, so it is the one command that has no
  # use for the caller's own (a cron job or a shell in $HOME has none). The Go
  # side repeats the test (rate.WalksEveryRepo) for direct crew-go calls.
  case "$sub $*" in
  "rate "*--sweep-all*) ;;
  "fleet "*) ;;
  *)
    echo "crew: not in a git repo" >&2
    exit 1
    ;;
  esac
fi
dir="$common/crew"

case "$sub" in
nudge)
  # Ported to Go (crew/internal/nudge, docs/crew-go-port.md). It execs directly:
  # the one helper this arm used to export CREW_SH for — `_unread_scan`, shared
  # with stall-watch — moved to Go with #968 (crew/internal/unread).
  exec "${CREW_GO_BIN:-@crewGoBin@}" nudge "$@"
  ;;
crews | log | report | sessions | roster | identity | inbox | hold | await | watch | retro | reply | resolve-target | pr-watch | where | stream | roster-render | adopt | status | msg | register | deregister | fleet | rate | git-baseline | reap | stack)
  # Ported to Go (crew/, docs/crew-go-port.md). CREW_GO_BIN is the
  # raw-source override; builds bake @crewGoBin@. CREW_SELF is this script's own
  # path — the `$0` the bash `stream` arm re-entered for its inner `watch`, its
  # `hold due` pre-check and its `crew reap`, which the Go `stream` still re-enters
  # through this script. It is also `roster-render`'s idea of which build it is
  # running: the daemon compares it against the installed `crew` and re-execs
  # that when they differ, so a home-manager switch reaches a running daemon
  # instead of waiting for the next dispatch. The script's path, not the
  # binary's, because a switch replaces the script and reads the binary fresh.
  export CREW_SELF="$0"
  exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
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
  #                  lead past an overdue dispatcher directive (crew/internal/nudge,
  #                  once per newest directive); --no-nudge turns it off.
  #   D9 truncated:/provider-error:
  #                  a pi lead idle at an empty box under pi's `Response was
  #                  truncated before completion.` or `Error: ...` line gets one
  #                  constant continue line after two samples (typed through
  #                  crew/internal/nudge, the same gate the D6 auto-nudge runs:
  #                  lead only, pi panes only, refused on a
  #                  live turn, unsent input or a dialog, once per episode) and
  #                  posts `working "truncated: auto-continued - pane %N"` /
  #                  `provider-error: ...`; `blocked "... not resumed"` if the
  #                  frame persists. Never escalates to dead:.
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
  # and --release seconds (default 300, 0 = off) have passed, release its
  # window through reap.ReleaseWindows in process — no stream or reap needed. Claude
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
  # call-out into the bash helpers that stay here: budget (windows|limit) and
  # local-model. The finished-worker release runs in process (reap.ReleaseWindows).
  # Every op exits its status + 10, so a failure before or around it — the
  # preamble's exit 1, a usage refusal, 127, a signal — never reads as a verdict.
  if [ "${1:-}" = --sh ]; then
    op="${2:-}"
    shift 2 || true
    rc=0
    case "$op" in
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
*)
  # Bare `crew` and an unknown subcommand: the grouped list on stderr, not the
  # one 2,000-character line (#812). Exit stays 1.
  _crew_help '' >&2
  exit 1
  ;;
esac
