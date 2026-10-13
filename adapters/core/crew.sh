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
# $dir is the caller's: the guards set it to the bus dir before replaying these.
# shellcheck disable=SC2154
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
# shellcheck disable=SC2154
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

sub="${1:-}"
shift || true

# --help anywhere it can mean "help": as the command itself, and as the FIRST
# argument of one (#812). Deliberately before the repo, crew-id and bus access
# below, so `crew <sub> --help` answers in a bare temp dir. Only the first
# position counts — `crew status w1 working --help` keeps --help as the detail.
#
# The screens are Go (crew/internal/entry, docs/crew-go-port.md); the routing
# stays here because when help wins is this preamble's contract.
case "$sub" in
--help | -h | help)
  exec "${CREW_GO_BIN:-@crewGoBin@}" help "${1:-}" "${2:-}"
  ;;
hold)
  # hold takes an action, so its --help is the second argument, not the first.
  case "${2:-}" in
  --help | -h)
    exec "${CREW_GO_BIN:-@crewGoBin@}" help hold "${1:-}"
    ;;
  esac
  ;;
fleet)
  # `fleet stream` is a sub-subcommand: its --help is the second argument.
  if [ "${1:-}" = stream ]; then
    case "${2:-}" in
    --help | -h)
      exec "${CREW_GO_BIN:-@crewGoBin@}" help fleet
      ;;
    esac
  fi
  ;;
esac
case "${1:-}" in
--help | -h)
  exec "${CREW_GO_BIN:-@crewGoBin@}" help "$sub"
  ;;
esac
# `id`, `new` and `identity` need no repo / no crew id.
if [ "$sub" = id ]; then
  # Read-only: resolves WORKER_TASK.md's crew_id, else CREW_ID, and never mints
  # (crew/internal/entry). The refusal text and the exit stay the arm's.
  exec "${CREW_GO_BIN:-@crewGoBin@}" id
fi
if [ "$sub" = new ]; then
  # The old bare-`id` minting behaviour, verbatim — now explicit and opt-in (#29).
  # The pid is this process's because the arm execs the binary that prints it.
  exec "${CREW_GO_BIN:-@crewGoBin@}" new
fi
if [ "$sub" = occupants ]; then
  # Ported to Go (crew/internal/reap); it needs no repo, so it execs before the
  # preamble resolves one.
  exec "${CREW_GO_BIN:-@crewGoBin@}" occupants "$@"
fi
if [ "$sub" = engine-cmd ]; then
  # The table is roster's, shared with `crew roster` (crew/internal/entry).
  exec "${CREW_GO_BIN:-@crewGoBin@}" engine-cmd "${1:-}"
fi
if [ "$sub" = pi-agent-dir ]; then
  # Seeds the shared pi worker dir (crew/internal/piagentdir). It needs no repo:
  # dispatch runs it before any worktree exists.
  exec "${CREW_GO_BIN:-@crewGoBin@}" pi-agent-dir
fi
# dash needs no repo either — it degrades every repo-scoped pane on its own.
# CREW_SELF is this script's own path, unresolved: the Go arm readlinks it into
# CREW_BIN, which is how crew-dash finds its way back to crew.
if [ "$sub" = dash ]; then
  CREW_SELF="$0" exec "${CREW_GO_BIN:-@crewGoBin@}" dash "$@"
fi

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
# above document the behaviour it implements.

case "$sub" in
nudge)
  # Ported to Go (crew/internal/nudge, docs/crew-go-port.md).
  exec "${CREW_GO_BIN:-@crewGoBin@}" nudge "$@"
  ;;
crews | log | report | sessions | roster | identity | inbox | hold | await | watch | retro | reply | resolve-target | pr-watch | where | stream | roster-render | adopt | status | msg | register | deregister | fleet | rate | git-baseline | reap | stack | stall-watch)
  # Ported to Go (crew/, docs/crew-go-port.md). CREW_GO_BIN is the
  # raw-source override; builds bake @crewGoBin@. CREW_SELF is this script's own
  # path — the `$0` the bash `stream` arm re-entered for its inner `watch`, its
  # `hold due` pre-check and its `crew reap`, which the Go `stream` still re-enters
  # through this script. It is also `roster-render`'s idea of which build it is
  # running: the daemon compares it against the installed `crew` and re-execs
  # that when they differ, so a home-manager switch reaches a running daemon
  # instead of waiting for the next dispatch. The script's path, not the
  # binary's, because a switch replaces the script and reads the binary fresh.
  CREW_SELF="$0" exec "${CREW_GO_BIN:-@crewGoBin@}" "$sub" "$@"
  ;;
*)
  # Bare `crew` and an unknown subcommand: the grouped list on stderr, not the
  # one 2,000-character line (#812). Exit stays 1 whatever the screen prints.
  "${CREW_GO_BIN:-@crewGoBin@}" help >&2
  exit 1
  ;;
esac
