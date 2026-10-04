#!/usr/bin/env bash
# refresh-budget — probe per-engine subscription quota for budget-aware
# dispatching.
#
# Writes ${XDG_DATA_HOME:-~/.local/share}/crew/engine-budget.json. The
# dispatcher reads it when judging engine/rung, and dispatch.sh enforces the
# >=95% gate (see DISPATCHER_PROTOCOL.md). Refresh by hand or at dispatcher
# session start — no daemon. Every probe degrades to null on failure: a
# missing engine entry is "unknown", never "exhausted".
#
# --report renders the cached file's lever/summary output with no probing at
# all — no curl/codex/tmux/dispatch-config call. --report --json emits the
# same per-window figures (plus pi's openrouter fields) as one JSON document
# instead. Both exit 1 when no cache exists yet.
#
#   claude — GET /api/oauth/usage with the access token from
#     ~/.claude/.credentials.json (stays local, never printed). Fallback 1: a
#     statusline-dumped rate_limits payload at $XDG_DATA_HOME/crew/claude-
#     statusline.json, used only when its mtime is <2h old. Fallback 2: scrape
#     the ⚡ NN% / 7d NN% the statusline already renders out of live worker
#     tmux panes (neither of the above exists on a macOS host — Keychain
#     holds the OAuth token instead of the credentials file, and nothing
#     persists the statusline's ephemeral rate_limits payload — see
#     probe_claude_pane_scrape below for why this is scoped and anchored the
#     way it is).
#   codex  — `codex app-server --stdio` JSON-RPC account/rateLimits/read
#     (experimental API; any failure -> null).
#   cursor — GET https://cursor.com/api/usage-summary (the dashboard's own
#     endpoint) with cookie WorkosCursorSessionToken=<account>::<token>, sent
#     through curl -K - on stdin only. Probed only when cursor-agent is on
#     PATH; cursor-agent itself is never executed. Linux token:
#     ${XDG_CONFIG_HOME:-~/.config}/cursor/auth.json, first of .accessToken,
#     .access_token, .token — the field name is UNVERIFIED, so a miss
#     degrades to null; whether cursor-agent honours XDG_CONFIG_HOME is
#     unverified too. macOS token: `security find-generic-password -s
#     cursor-access-token -a cursor-user -w` (read-only; may raise a GUI ACL
#     prompt for an item another binary created — bounded by timeout 10,
#     then null). Account id: cli-config.json (Linux: next to auth.json;
#     macOS: ~/.cursor/) .authInfo.authId, .authInfo.userId, else the JWT
#     sub, with the provider prefix up to the last `|` stripped. The cookie
#     form is unverified live. Strictly read-only on Cursor's auth state:
#     never writes, refreshes, or rotates anything.
#   pi     — GET /api/v1/key on OpenRouter (usage-priced, so there is no
#     quota to probe — only a spend-vs-target check). Key resolution:
#     DISPATCH_OPENROUTER_KEY_FILE's first line, exclusively when set (an
#     unreadable or empty file is unknown, never a fallback), else
#     OPENROUTER_API_KEY, else (last resort) pi's own login: `pi auth
#     print-api-key --provider openrouter` (timeout-bounded), then
#     ${PI_CODING_AGENT_DIR:-~/.pi/agent}/auth.json read-only (.openrouter.key
#     when type is "api_key", .openrouter.access when "oauth"); the value must
#     look like an sk-or-v1- key. The key goes to curl through -K -
#     on stdin only, never argv, and is never printed or cached. A key whose
#     credit limit is set and exhausted (limit_remaining <= 0) sets
#     engines.pi.limit_reached, which gates `dispatch --agent pi`. spend_usd is
#     the per-key current-UTC-month figure (data.usage_monthly); the target
#     comes from DISPATCH_OPENROUTER_MONTHLY_USD, else a positive key `limit`
#     whose limit_reset is "monthly" (target_source key_limit). No key -> engines.pi is
#     null (informational, never blocking).
set -euo pipefail

OUT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
OUT="$OUT_DIR/engine-budget.json"
STATUSLINE_CACHE="$OUT_DIR/claude-statusline.json"
CREDENTIALS="$HOME/.claude/.credentials.json"
STALE_AFTER_S=7200

warn() { printf 'refresh-budget: %s\n' "$*" >&2; }

usage() {
  printf 'usage: refresh-budget [--report [--json]]\n' >&2
  exit 2
}

report_mode=false
json_mode=false
while [[ $# -gt 0 ]]; do
  case "$1" in
  --report)
    report_mode=true
    shift
    ;;
  --json)
    json_mode=true
    shift
    ;;
  *) usage ;;
  esac
done
[[ $json_mode == false || $report_mode == true ]] || usage

# Shared by report() (the lever/summary render, on both the fresh-probe path
# and --report) and report_json(). wsecs covers only the windows with a known
# nominal length — codex's 1d/unknown/other buckets have none, so no pace can
# be computed for them. A window carrying starts_at (pi's calendar `month`,
# cursor's billing cycle — neither has a fixed length) supplies its own.
# shellcheck disable=SC2016  # jq's own $vars, not bash expansions
JQ_DEFS='
  def reltime: . as $s
    | ($s / 86400 | floor) as $d
    | (($s % 86400) / 3600 | floor) as $h
    | (($s % 3600) / 60 | floor) as $m
    | if $d > 0 then "\($d)d \($h)h"
      elif $h > 0 then "\($h)h \($m)m"
      else "\($m)m" end;
  def wsecs($k; $w): if $w.starts_at != null and $w.resets_at != null then ($w.resets_at - $w.starts_at)
    elif $k == "5h" then 18000
    elif ($k == "7d" or $k == "7d_opus" or $k == "7d_sonnet") then 604800
    else null end;
  def elapsed_pct($resets_at; $L): (100 * ($L - ($resets_at - $now)) / $L) as $x
    | if $x < 0 then 0 elif $x > 100 then 100 else $x end;
  def usd: (. * 100 | round) as $c
    | "\($c / 100 | floor).\(($c % 100) | tostring | if length == 1 then "0" + . else . end)";
  # >=95% is a hold candidate; each engine gets exactly one gating window
  # (precedence: no usable deadline, then no nominal length, then latest
  # resets_at) and every sibling >=95% window on that engine defers to it.
  # A window whose resets_at has already passed does not gate at all (the
  # cache can predate the reset), matching the dispatch >=95% stop; a
  # null resets_at still gates (rule 1) because no deadline is usable.
  # Ties within a rule break on sorted key for a deterministic pick.
  def gating($windows):
    ($windows | to_entries
      | map(select(.value.used_pct >= 95
                   and (.value.resets_at == null or .value.resets_at > $now)))
      | sort_by(.key)) as $cands |
    if ($cands | length) == 0 then null
    else
      ($cands | map(select(.value.resets_at == null))) as $unreset |
      ($cands | map(select(wsecs(.key; .value) == null))) as $unsized |
      if ($unreset | length) > 0 then {key: $unreset[0].key, rule: 1}
      elif ($unsized | length) > 0 then {key: $unsized[0].key, rule: 2}
      else ($cands | sort_by([-.value.resets_at, .key]))[0] as $g | {key: $g.key, rule: 3}
      end
    end;
  # window_rows — every non-null engine crossed with every one of its
  # windows, with the figures both the text lever and --report --json need:
  # fam/L/rem/ahead per window, and advice (the lever verdict text) only once
  # a window is at or above the 85% floor — a lower window still carries
  # ahead (json wants it unconditionally) but never advice.
  def window_rows:
    .engines | to_entries[] | select(.value != null) | .key as $e |
    .value.windows as $windows |
    (gating($windows)) as $gate |
    $windows | to_entries[] |
    .key as $k | .value as $w |
    (if $k == "5h" then "5h" elif ($k == "7d" or $k == "7d_opus" or $k == "7d_sonnet") then "7d"
     elif $k == "month" then "month" else "other" end) as $fam |
    (wsecs($k; $w)) as $L |
    (if $w.resets_at != null and $w.resets_at > $now then ($w.resets_at - $now) else null end) as $rem |
    (if ($fam == "7d" or $fam == "month") and $rem != null then (($w.used_pct - elapsed_pct($w.resets_at; $L)) | round) else null end) as $ahead |
    (if $w.used_pct < 85 then null
     elif $w.used_pct < 95 then
       (if $fam == "5h" then "short window: prefer waiting past the reset to shedding burn class"
        elif $fam == "7d" then "real budget: prefer a cheaper burn class or rotate engines"
        elif $fam == "month" and $e == "pi" then "monthly spend target: keep standard/trivial work off pi and shed pi fan-out"
        elif $fam == "month" then "monthly plan quota: prefer a cheaper burn class or rotate engines"
        else "approaching quota (>=85%), prefer a cheaper burn class or rotate engines (see DISPATCHER_PROTOCOL.md)"
        end)
     elif $gate == null then "not binding: window has already reset"
     elif $k != $gate.key then "not binding: \($e) is gated until \($gate.key) resets"
     elif $gate.rule == 1 then "not holdable: no reset time, hand the task back"
     elif $gate.rule == 2 then "not holdable: window has no nominal length, hand the task back"
     elif elapsed_pct($w.resets_at; $L) >= 85 then "binding window; holdable: inside the window'"'"'s last 15%, wait past the reset"
     else "binding window; not holdable: \($rem | reltime) is outside the window'"'"'s last 15%, hand the task back"
     end) as $advice |
    {e: $e, k: $k, w: $w, fam: $fam, L: $L, rem: $rem, ahead: $ahead, advice: $advice};
  # projection_line — the pi/openrouter over-target lever line, or null; the
  # one string text and json both print, so they cannot disagree.
  def projection_line($e; $v):
    if $v.source == "openrouter_key" and $v.projected_month_end_usd != null and $v.target_usd != null
       and $v.projected_month_end_usd > $v.target_usd then
      "\($e) projected $\($v.projected_month_end_usd | usd) at month end, over the $\($v.target_usd | usd) monthly target — size \($e) fan-out down"
    else null end;
'

# probe_claude — print the claude engine object via the OAuth usage endpoint,
# falling back to a fresh statusline dump; return 1 when neither works.
probe_claude() {
  local resp out
  if [[ -f $CREDENTIALS ]]; then
    local token
    token=$(jq -r '.claudeAiOauth.accessToken // empty' "$CREDENTIALS")
    # The endpoint rate-limits hard (429 or empty 200 after ~1 call/min) —
    # either failure falls through to the statusline cache below.
    # Headers go through -K (stdin) rather than -H "$token" so the bearer
    # token never appears in this process's argv/`ps`.
    if [[ -n $token ]] && resp=$(curl -sf --max-time 15 -K - \
      "https://api.anthropic.com/api/oauth/usage" <<<"header = \"Authorization: Bearer $token\"
header = \"anthropic-beta: oauth-2025-04-20\"") && [[ -n $resp ]]; then
      # resets_at arrives as 2026-08-03T18:59:59.991098+00:00 — fromdateiso8601
      # only accepts Zulu whole seconds, so normalize first; unparseable -> null.
      if ! out=$(jq '
        def toepoch: if . == null then null
          else (try (sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601) catch null) end;
        {
          source: "oauth_usage",
          # The oauth usage payload carries no plan/subscription key (verified
          # live), so the tier is unknowable: null.
          plan_type: null,
          # `//` treats false as empty, so it cannot default a boolean: test
          # presence explicitly. Missing spend_limit_reached -> assume reached
          # (conservative: no credits cover).
          credits_cover: ((.extra_usage // {}) as $e
            | ($e.is_enabled == true)
              and ((if $e | has("spend_limit_reached") then $e.spend_limit_reached else true end) == false)),
          windows: (
            {}
            + (if .five_hour.utilization != null then {"5h": {used_pct: .five_hour.utilization, resets_at: (.five_hour.resets_at | toepoch)}} else {} end)
            + (if .seven_day.utilization != null then {"7d": {used_pct: .seven_day.utilization, resets_at: (.seven_day.resets_at | toepoch)}} else {} end)
            + (if .seven_day_opus.utilization != null then {"7d_opus": {used_pct: .seven_day_opus.utilization, resets_at: (.seven_day_opus.resets_at | toepoch)}} else {} end)
            + (if .seven_day_sonnet.utilization != null then {"7d_sonnet": {used_pct: .seven_day_sonnet.utilization, resets_at: (.seven_day_sonnet.resets_at | toepoch)}} else {} end)
          )
        }' <<<"$resp"); then
        return 1
      fi
      printf '%s\n' "$out"
      return 0
    fi
  fi
  # Fallback: statusline dump, fresh only — an old dump describes a quota that
  # has since drained or reset, which is worse than "unknown".
  if [[ -f $STATUSLINE_CACHE ]] &&
    (($(date +%s) - $(stat -c %Y "$STATUSLINE_CACHE") < STALE_AFTER_S)); then
    jq -e '.rate_limits | {
      source: "statusline_cache",
      plan_type: null,
      credits_cover: null,
      windows: (
        {}
        + (if .five_hour.used_percentage != null then {"5h": {used_pct: .five_hour.used_percentage, resets_at: .five_hour.resets_at}} else {} end)
        + (if .seven_day.used_percentage != null then {"7d": {used_pct: .seven_day.used_percentage, resets_at: .seven_day.resets_at}} else {} end)
      )
    }' "$STATUSLINE_CACHE" 2>/dev/null && return 0
  fi
  probe_claude_pane_scrape
}

# probe_claude_pane_scrape — third fallback: scrape the ⚡ NN% (5h) and 7d
# NN% markers the statusline already renders, from live worker pane
# captures. Scoped to windows `dispatch` tagged with @crew_name (excluding
# the dispatcher's own window, tagged literally "dispatcher" — same idiom
# as crew.sh's _occupants) rather than every tmux pane on the server: the
# dispatcher's own pane is itself a claude session with its own statusline,
# so an unscoped scrape could never degrade to "unknown" on a drained
# roster, and an unanchored one could pick up a stray "⚡ NN%" sitting in a
# worker's visible scrollback (e.g. example statusline text in a doc or task
# file a worker has open).
# Anchored to the bottom block of each capture: the statusline is the line
# directly above the LAST mode/input-box line (see _pane_statusline). Rows
# below the mode line (subagent list, blank lines) don't move the anchor, and
# a statusline-looking line further up the scrollback is never read (verified
# live, 2026-09-24):
#   🤖 Sonnet 5 🧠 med | 📊 120k/1M | ⚡ 13% (2h4m → 23:20) 7d 95% (3d14h)
#   -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · PR #308 · ← for agents
#
#   ● main
#   ◯ shell-reviewer  Review dispatch.sh diff        13s · ↓ 25.3k tokens
# No pane_current_command filter: this host's nix-wrapped claude binary
# reports as `.claude-wrapped`, not `claude`, so a literal-command filter
# would silently scrape nothing here — the regex itself is the filter.
# resets_at is derived from the countdown parenthetical the statusline
# renders next to each percentage, when it parses (see _pane_countdown
# below); credits_cover is unknowable from a screen scrape, so it stays
# null.

# _pane_countdown <match> <nominal_secs> — print "<pct>\t<remaining>" for
# one ⚡/7d match captured by probe_claude_pane_scrape below. <remaining> is
# -1 when the match carries no usable countdown.
#
# The percentage is the LAST digit run of the text before the first '%'.
# Scanning the whole match either way is wrong in both directions: the first
# run picks up the "7d" marker's own digit ("7d 61%" -> "761"), the last run
# picks up the " -> HH:MM" tail ("(10m -> 05:20)" -> "20").
#
# A derived clock is best-effort: capture-pane returns the pane's last
# render, so "now + remaining" inherits however stale that is. It errs
# toward refusing, since an overstated remaining understates elapsed_pct.
# The day-form (NNdNNh, no arrow) countdown is observed live on the 7d
# segment; the 7d segment itself only renders when that window is notable.
_pane_countdown() {
  local m="$1" nominal="$2" pct paren inner re d h mnt remaining=-1
  # Bash treats a leading-zero numeral ("08") as octal, so a captured digit
  # run must be forced to base 10 (10#...) before any arithmetic touches it
  # — otherwise a zero-padded reading is either a fatal "value too great for
  # base" error or a silently wrong value ("010" -> 8, not 10). Bounded to
  # {1,3} digits, second line of defence behind the marker regexes below:
  # a huge run must not silently wrap through 10# into a plausible pct.
  pct=$(printf '%s' "${m%%\%*}" | grep -oE '[0-9]{1,3}' | tail -1)
  pct=$((10#${pct:-0}))
  ((pct > 100)) && pct=100
  paren=$(printf '%s' "$m" | grep -oE '\([^)]*\)$')
  if [[ -n $paren ]]; then
    inner="${paren#(}"
    inner="${inner%)}"
    # Digit runs are bounded per component (not [0-9]+) so an absurd
    # parenthetical (e.g. a 15-digit day count) can't overflow 64-bit
    # arithmetic and wrap into a small positive number that passes the
    # nominal-length guard below — it fails to match instead and stays null.
    re='^([0-9]{1,3}d)?([0-9]{1,3}h)?([0-9]{1,2}m)?( → [0-9]{2}:[0-9]{2})?$'
    if [[ $inner =~ $re ]] && [[ -n ${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]} ]]; then
      d="${BASH_REMATCH[1]%d}"
      h="${BASH_REMATCH[2]%h}"
      mnt="${BASH_REMATCH[3]%m}"
      remaining=$((10#${d:-0} * 86400 + 10#${h:-0} * 3600 + 10#${mnt:-0} * 60))
      ((remaining <= nominal)) || remaining=-1
    fi
  fi
  printf '%s\t%s\n' "$pct" "$remaining"
}

# _pane_statusline <capture> — print the statusline of a pane capture: the
# non-empty line directly above the last mode/input-box line, or the last
# non-empty line when no mode line renders. Prints nothing for an empty pane.
_pane_statusline() {
  local -a lines
  local i last=-1 mode='^[[:space:]]*(⏵|⏸|-- [A-Z ]+ --|\? for shortcuts)'
  mapfile -t lines < <(printf '%s\n' "$1" | grep -v '^[[:space:]]*$')
  ((${#lines[@]} > 0)) || return 0
  for i in "${!lines[@]}"; do
    [[ ${lines[i]} =~ $mode ]] && last=$i
  done
  if ((last < 0)); then
    printf '%s\n' "${lines[-1]}"
  elif ((last > 0)); then
    printf '%s\n' "${lines[last - 1]}"
  fi
}

probe_claude_pane_scrape() {
  local wins panes wid nm pw pid text tail m v rem max5=-1 max7=-1 rem5=-1 rem7=-1
  command -v tmux >/dev/null 2>&1 || return 1
  wins=$(tmux list-windows -a -F '#{window_id}	#{@crew_name}' 2>/dev/null) || return 1
  panes=$(tmux list-panes -a -F '#{window_id}	#{pane_id}' 2>/dev/null) || return 1
  [[ -n $wins && -n $panes ]] || return 1
  while IFS=$'\t' read -r wid nm; do
    [[ -n $wid && -n $nm && $nm != dispatcher ]] || continue
    while IFS=$'\t' read -r pw pid; do
      [[ $pw == "$wid" ]] || continue
      text=$(tmux capture-pane -p -t "$pid" 2>/dev/null) || continue
      tail=$(_pane_statusline "$text")
      # {1,3}: a huge digit run must fail the match, not reach 10# — see
      # _pane_countdown.
      m=$(printf '%s\n' "$tail" | grep -oE '⚡[[:space:]]*[0-9]{1,3}%([[:space:]]*\([^)]*\))?' | tail -1)
      if [[ -n $m ]]; then
        IFS=$'\t' read -r v rem <<<"$(_pane_countdown "$m" 18000)"
        # A helper that crashed or produced no output must not be read as a
        # valid 0% — treat the pane as if the marker hadn't matched.
        if [[ $v =~ ^[0-9]+$ ]]; then
          if ((v > max5)) || { ((v == max5)) && ((rem > rem5)); }; then
            max5=$v
            rem5=$rem
          fi
        fi
      fi
      m=$(printf '%s\n' "$tail" | grep -oE '7d[[:space:]]*[0-9]{1,3}%([[:space:]]*\([^)]*\))?' | tail -1)
      if [[ -n $m ]]; then
        IFS=$'\t' read -r v rem <<<"$(_pane_countdown "$m" 604800)"
        if [[ $v =~ ^[0-9]+$ ]]; then
          if ((v > max7)) || { ((v == max7)) && ((rem > rem7)); }; then
            max7=$v
            rem7=$rem
          fi
        fi
      fi
    done <<PANES
$panes
PANES
  done <<WINS
$wins
WINS
  ((max5 >= 0)) || return 1
  local now resets5 resets7
  now=$(date +%s)
  if ((rem5 >= 0)); then resets5=$((now + rem5)); else resets5=null; fi
  if ((rem7 >= 0)); then resets7=$((now + rem7)); else resets7=null; fi
  jq -n --argjson p5 "$max5" --argjson p7 "$max7" --argjson r5 "$resets5" --argjson r7 "$resets7" '
    {
      source: "pane_scrape",
      plan_type: null,
      credits_cover: null,
      windows: (
        {"5h": {used_pct: $p5, resets_at: $r5}}
        + (if $p7 >= 0 then {"7d": {used_pct: $p7, resets_at: $r7}} else {} end)
      )
    }'
}

# probe_codex — print the codex engine object via app-server JSON-RPC; return
# 1 when codex is absent or the experimental call changes shape.
probe_codex() {
  command -v codex >/dev/null 2>&1 || return 1
  local resp
  resp=$({
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"refresh-budget","version":"1"}}}'
    sleep 1
    printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"account/rateLimits/read","params":{}}'
    sleep 3
  } | timeout 20 codex app-server --stdio 2>/dev/null) || true
  [[ -n $resp ]] || return 1
  # Window names come from the duration, not the primary/secondary label — the
  # backend has shipped 5h and 7d windows in the same slots over time. A
  # missing duration names "unknown" instead of defaulting to 0 (-> "5h"), so
  # it can't silently overwrite a real 5h window below and hide an exhausted
  # one behind it — "unknown" still feeds the exhaustion gate, just under its
  # own key.
  jq -es '
    def wname($s):
      if $s == null then "unknown"
      elif $s <= 18600 then "5h" elif $s <= 90000 then "1d" elif $s <= 691200 then "7d" else "other" end;
    (map(select(.id == 2)) | .[0].result) as $res
    | ($res.rateLimits) as $r
    | {
        source: "app-server",
        credits_cover: ($r.credits.hasCredits // false),
        plan_type: ($r.planType // null),
        windows: (
          {}
          + (if $r.primary.usedPercent != null then {(wname($r.primary.windowDurationMins | if . != null then . * 60 else null end)): {used_pct: $r.primary.usedPercent, resets_at: $r.primary.resetsAt}} else {} end)
          + (if $r.secondary != null and $r.secondary.usedPercent != null then {(wname($r.secondary.windowDurationMins | if . != null then . * 60 else null end)): {used_pct: $r.secondary.usedPercent, resets_at: $r.secondary.resetsAt}} else {} end)
        ),
        # Absolute-limit signals alongside the relative percent windows, so
        # dispatch can gate on exhaustion no percent window expresses.
        # ordinaryUsageAllowed is top-level on the response; the rest sit under
        # rateLimits. jq indexes a missing/null sub-object to null, so each
        # field degrades to null rather than failing the probe. The boolean
        # fields are written WITHOUT `// null`: `//` substitutes on `false`
        # too, which would collapse an explicit `ordinaryUsageAllowed: false`
        # (the exhaustion signal) into null and disarm the gate.
        limit_reached: {
          ordinary_usage_allowed: $res.ordinaryUsageAllowed,
          rate_limit_reached_type: $r.rateLimitReachedType,
          spend_control_reached: $r.spendControlReached,
          credits_unlimited: $r.credits.unlimited,
          credits_balance: $r.credits.balance,
          individual_remaining_percent: $r.individualLimit.remainingPercent,
          individual_resets_at: $r.individualLimit.resetsAt
        }
      }
  ' <<<"$resp" 2>/dev/null
}

# probe_cursor — print the cursor engine object from the dashboard's
# usage-summary; return 1 with no cursor-agent CLI, 2 with no usable access
# token, 3 with no usable account id, 4 when the call fails, 5 when the
# response is not recognised. Token and account are spliced into a curl
# config, so each must match the JWT / WorkOS id charset; the token reaches
# jq only on stdin, and every call that sees auth state discards stderr
# (jq errors can echo string values).
probe_cursor() {
  command -v cursor-agent >/dev/null 2>&1 || return 1
  local conf token account
  if [[ $(uname -s) == Darwin ]]; then
    conf="$HOME/.cursor/cli-config.json"
    token=$(timeout 10 security find-generic-password -s cursor-access-token -a cursor-user -w 2>/dev/null) || token=""
  else
    local dir="${XDG_CONFIG_HOME:-$HOME/.config}/cursor"
    conf="$dir/cli-config.json"
    token=$(jq -r '[.accessToken, .access_token, .token | strings | select(. != "")][0] // empty' "$dir/auth.json" 2>/dev/null) || token=""
  fi
  [[ $token =~ ^[A-Za-z0-9._-]+$ ]] || return 2
  account=$(jq -r '[.authInfo.authId, .authInfo.userId | strings | select(. != "")][0] // empty' "$conf" 2>/dev/null) || account=""
  if [[ -z $account ]]; then
    account=$(jq -Rr '
      split(".")[1] // empty
      | gsub("-"; "+") | gsub("_"; "/")
      | . + ("=" * ((4 - length % 4) % 4))
      | @base64d | fromjson | .sub | strings' <<<"$token" 2>/dev/null) || account=""
  fi
  account="${account##*|}"
  [[ $account =~ ^[A-Za-z0-9._-]+$ ]] || return 3

  local resp
  resp=$(curl -sf --max-time 15 -K - https://cursor.com/api/usage-summary <<<"header = \"Accept: application/json\"
header = \"Cookie: WorkosCursorSessionToken=$account::$token\"") || return 4

  # The team shape reports individualUsage.overall used/limit, the individual
  # shape per-pool plan percentages; max(auto, api) errs toward refusing. The
  # billing-cycle bounds are both kept or both dropped, so no half-sized
  # window is ever written. limit_reached compares the raw percentages
  # (used_pct is rounded for display only).
  jq -e '
    def toepoch:
      if type == "number" then (if . > 1e12 then . / 1000 else . end) | floor
      elif type == "string" then (try (sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601) catch null)
      else null end;
    def bounded: (.limit | type) == "number" and .limit > 0 and (.used | type) == "number";
    (.isUnlimited == true) as $unlimited
    | ([.individualUsage.overall | objects | select(bounded) | .used / .limit * 100][0]) as $overall
    | [.individualUsage.plan | objects | .autoPercentUsed, .apiPercentUsed | numbers] as $plan
    | (if $overall != null then $overall else ($plan | max) end) as $pct
    | if $pct == null and ($unlimited | not) then error("no usable percentage") else . end
    | (.billingCycleStart | toepoch) as $s
    | (.billingCycleEnd | toepoch) as $e
    | (if $s != null and $e != null and $e > $s then [$s, $e] else [null, null] end) as [$starts, $resets]
    | [.individualUsage.onDemand, .teamUsage.onDemand | objects | select(.enabled == true)] as $od
    | ([$plan[], $overall | numbers] | max) as $raw
    | {
        source: "usage_summary",
        plan_type: (if (.membershipType | type) == "string" then .membershipType else null end),
        credits_cover: ($od | any(.limit == null or (bounded and .used < .limit))),
        unlimited: $unlimited,
        windows: (if $unlimited then {} else {month: {used_pct: (($pct * 10 | round) / 10), starts_at: $starts, resets_at: $resets}} end),
        limit_reached: (
          if $unlimited then null
          elif $raw != null and $raw >= 100 then {reason: "plan usage at \($raw | round)%", resets_at: $resets}
          elif ($od | any(bounded and .used >= .limit)) then {reason: "on-demand limit reached", resets_at: $resets}
          else null end)
      }
  ' <<<"$resp" 2>/dev/null || return 5
}

# _or_key — resolve the OpenRouter key into the caller's `or_key` local
# (never printed). DISPATCH_OPENROUTER_KEY_FILE, when set, is the exclusive
# source — an unreadable file or an empty first line leaves `or_key` empty
# rather than falling back to OPENROUTER_API_KEY, since spend is per key and
# the wrong key would pace pi against the wrong spend. Otherwise reads
# OPENROUTER_API_KEY, else pi's auth CLI, then its auth store.
_or_key() {
  or_key=""
  if [[ -n ${DISPATCH_OPENROUTER_KEY_FILE:-} ]]; then
    if [[ -r $DISPATCH_OPENROUTER_KEY_FILE ]]; then
      IFS= read -r or_key <"$DISPATCH_OPENROUTER_KEY_FILE" || true
      or_key="${or_key//$'\r'/}"
      or_key="${or_key#"${or_key%%[![:space:]]*}"}"
      or_key="${or_key%"${or_key##*[![:space:]]}"}"
    fi
  else
    or_key="${OPENROUTER_API_KEY:-}"
    [[ -n $or_key ]] || _or_key_from_pi
  fi
}

# _or_key_from_pi — pi's OpenRouter login into the caller's `or_key` local.
# Tries pi's auth CLI first, then auth.json: {"type":"api_key","key":...} (what
# pi writes for its OpenRouter login, since OpenRouter's OAuth hands back a plain
# API key) or {"type":"oauth","access":...}. Anything else (other type, junk,
# missing file or field) leaves or_key empty.
_or_key_from_pi() {
  local auth="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}/auth.json" v=""
  if command -v pi >/dev/null 2>&1; then
    v=$(timeout 10 pi auth print-api-key --provider openrouter </dev/null 2>/dev/null) || v=""
  fi
  if [[ ! $v =~ ^sk-or-v1-[A-Za-z0-9]+$ && -r $auth ]]; then
    v=$(jq -r '.openrouter | if .type == "api_key" then .key elif .type == "oauth" then .access else empty end | strings' "$auth" 2>/dev/null) || v=""
  fi
  [[ $v =~ ^sk-or-v1-[A-Za-z0-9]+$ ]] && or_key=$v
  return 0
}

# _or_target — print the validated DISPATCH_OPENROUTER_MONTHLY_USD, or the
# jq literal "null" when unset, empty, or not a positive decimal (warning on
# the latter — a malformed target is treated as no target, never as a block).
_or_target() {
  local raw="${DISPATCH_OPENROUTER_MONTHLY_USD:-}"
  if [[ -z $raw ]]; then
    printf 'null'
  elif [[ $raw =~ ^[0-9]+(\.[0-9]+)?$ ]] && [[ $(jq -n --arg r "$raw" '$r | tonumber > 0') == true ]]; then
    printf '%s' "$raw"
  else
    warn "DISPATCH_OPENROUTER_MONTHLY_USD='$raw' is not a positive number; ignoring the target"
    printf 'null'
  fi
}

# probe_pi — print the pi engine object (OpenRouter per-key month-to-date
# spend vs an optional target); return 2 with no key configured, 3 when
# DISPATCH_OPENROUTER_KEY_FILE is set but unreadable or empty, 1 when the
# call fails or the response has no numeric data.usage_monthly.
probe_pi() {
  local or_key=""
  _or_key
  if [[ -z $or_key ]]; then
    [[ -n ${DISPATCH_OPENROUTER_KEY_FILE:-} ]] && return 3
    return 2
  fi

  local resp
  resp=$(curl -sf --max-time 15 -K - https://openrouter.ai/api/v1/key <<<"header = \"Authorization: Bearer $or_key\"") || return 1

  local target now out
  target=$(_or_target)
  now=$(date +%s)
  # Month bounds come from `now|gmtime` rather than adding a fixed 30d, since
  # months vary in length; December is rolled to next January explicitly
  # rather than relying on mktime's month-overflow normalization.
  out=$(jq -e --argjson now "$now" --argjson target "$target" '
    (.data.limit_reset) as $reset
    | (if $target != null then $target
       elif (.data.limit | type) == "number" and .data.limit > 0 and $reset == "monthly" then .data.limit
       else null end) as $tgt
    | (.data.usage_monthly) as $spend
    | (if ($spend | type) != "number" then error("bad shape: usage_monthly missing or non-numeric") else . end)
    | ($now | gmtime) as $g
    | ($g[0]) as $y | ($g[1]) as $m
    | ([$y, $m, 1, 0, 0, 0, 0, 0] | mktime) as $start
    | ((if $m == 11 then [$y + 1, 0, 1, 0, 0, 0, 0, 0] else [$y, $m + 1, 1, 0, 0, 0, 0, 0] end) | mktime) as $next
    | ($now - $start) as $elapsed_s
    | ($next - $start) as $L
    | (((100 * $elapsed_s / $L) * 10 | round) / 10) as $elapsed_pct
    | (if $elapsed_s < 86400 then null else (($spend / ($elapsed_s / $L) * 100 | round) / 100) end) as $projected
    | (if $tgt != null then ((($spend / $tgt * 100) * 10 | round) / 10) else null end) as $used_pct
    | {
        source: "openrouter_key",
        spend_usd: $spend,
        target_usd: $tgt,
        target_source: (if $target != null then "config" elif $tgt != null then "key_limit" else null end),
        elapsed_pct: $elapsed_pct,
        projected_month_end_usd: $projected,
        key_limit_usd: .data.limit,
        key_limit_remaining_usd: .data.limit_remaining,
        limit_reset: $reset,
        limit_reached: (
          if (.data.limit | type) == "number" and (.data.limit_remaining | type) == "number" and .data.limit_remaining <= 0
          then {reason: "OpenRouter key credit limit reached ($\(.data.limit) limit, $\(.data.limit_remaining) remaining)"}
          else null end),
        starts_at: $start,
        resets_at: $next,
        windows: (if $tgt != null then {month: {used_pct: $used_pct, starts_at: $start, resets_at: $next}} else {} end)
      }
  ' <<<"$resp" 2>/dev/null) || return 1
  printf '%s\n' "$out"
}

# report — render $OUT as budget-lever warnings plus a one-line summary per
# engine. Shared by `main` (right after a fresh probe+write) and `--report`
# (reading a pre-existing cache with no probing at all).
report() {
  local now
  now=$(date +%s)
  jq -r --argjson now "$now" "$JQ_DEFS"'
    (
      window_rows | select(.w.used_pct >= 85) |
      (if .rem == null then ""
       else " (resets in \(.rem | reltime)" + (if .ahead != null then ", \(.ahead) points ahead of pace" else "" end) + ")"
       end) as $paren |
      "\(.e) \(.k) at \(.w.used_pct)%\($paren) — \(.advice)"
    ),
    (
      .engines | to_entries[] | select(.value != null and .value.source == "openrouter_key") |
      .key as $e | .value as $v |
      projection_line($e; $v) | select(. != null)
    )
  ' "$OUT" |
    while IFS= read -r line; do
      warn "budget lever: $line"
    done || true

  jq -r --argjson now "$now" "$JQ_DEFS"'
    .engines | to_entries[] | .key as $e |
    if .value == null then "\($e): unknown"
    elif .value.source == "openrouter_key" then
      .value as $v |
      (if $v.projected_month_end_usd != null then "projected $\($v.projected_month_end_usd | usd) at month end" else "too early to project" end) as $proj |
      (if $v.resets_at != null then
         " (resets \($v.resets_at | todateiso8601)" +
         (if $v.resets_at > $now then ", in \(($v.resets_at - $now) | reltime)" else "" end) +
         ")"
       else "" end) as $reset |
      (if $v.target_usd != null then
         "\($e): openrouter $\($v.spend_usd | usd) of $\($v.target_usd | usd) monthly target\(if $v.target_source == "key_limit" then " (key limit)" else "" end) (\($v.windows.month.used_pct)% used, \($v.elapsed_pct)% of month elapsed, \($proj))"
       else
         "\($e): openrouter $\($v.spend_usd | usd) month-to-date (no monthly target; \($proj))"
       end) + $reset + (if $v.limit_reset != null then " [key limit resets: \($v.limit_reset)]" else "" end) + (if $v.limit_reached != null then " — LIMIT REACHED: \($v.limit_reached.reason)" else "" end)
    else "\($e): " + (if .value.plan_type then "[\(.value.plan_type)] " else "" end) +
      (if .value.unlimited == true then "unlimited" else ([.value.windows | to_entries[] |
        "\(.key) \(.value.used_pct)% used" +
        (if .value.resets_at then
           " (resets \(.value.resets_at | todateiso8601)" +
           (if .value.resets_at > $now then ", in \((.value.resets_at - $now) | reltime)" else "" end) +
           ")"
         else "" end)
      ] | join(", ")) end) + (if .value.credits_cover then " [credits cover]" else "" end) +
      (if (.value.limit_reached.reason | type) == "string" then " [limit reached: \(.value.limit_reached.reason)]" else "" end)
    end
  ' "$OUT"
}

# report_json — the --report --json rendering: one document over $OUT with
# the same per-window figures (resets_in_s/ahead_pts/verdict) the text lever
# uses, plus pi's openrouter fields and its shared projection string, so text
# and json can never disagree.
report_json() {
  local now
  now=$(date +%s)
  jq --argjson now "$now" "$JQ_DEFS"'
    [ window_rows ] as $rows |
    {
      fetched_epoch,
      engines: (.engines | to_entries | map(
        .key as $e |
        .value = (
          if .value == null then null
          else
            .value as $v |
            {
              source: $v.source,
              plan_type: $v.plan_type,
              credits_cover: $v.credits_cover,
              unlimited: ($v.unlimited == true),
              spend_usd: ($v.spend_usd // null),
              target_usd: ($v.target_usd // null),
              elapsed_pct: ($v.elapsed_pct // null),
              projected_month_end_usd: ($v.projected_month_end_usd // null),
              windows: [$rows[] | select(.e == $e) | {
                key: .k, used_pct: .w.used_pct, resets_at: .w.resets_at,
                resets_in_s: .rem, ahead_pts: .ahead, verdict: .advice
              }],
              target_source: ($v.target_source // null),
              limit_reset: ($v.limit_reset // null),
              limit_reached: ($v.limit_reached // null),
              projection: projection_line($e; $v)
            }
          end
        )
      ) | from_entries)
    }
  ' "$OUT"
}

main() {
  # DISPATCH_OPENROUTER_MONTHLY_USD / DISPATCH_OPENROUTER_KEY_FILE may come
  # from the settings resolver (dispatch-config) when unset in the
  # environment — see dispatch-config.sh for the layer order. Probe-path
  # only: --report never needs a target/key, so it never calls out for one.
  local settings
  settings="$("${DISPATCH_CONFIG_BIN:-@dispatchConfig@}")"
  [[ -n ${DISPATCH_OPENROUTER_MONTHLY_USD:-} ]] || DISPATCH_OPENROUTER_MONTHLY_USD="$(jq -r '.openrouter.monthlyUsd // "" | tostring' <<<"$settings")"
  [[ -n ${DISPATCH_OPENROUTER_KEY_FILE:-} ]] || DISPATCH_OPENROUTER_KEY_FILE="$(jq -r '.openrouter.keyFile // ""' <<<"$settings")"

  local claude='null' codex='null' cursor='null' pi='null' probe
  if probe=$(probe_claude); then
    claude=$probe
  else
    warn "claude quota unknown (oauth + statusline + pane-scrape all unavailable)"
  fi
  if probe=$(probe_codex); then
    codex=$probe
  else
    warn "codex quota unknown (no codex CLI or app-server call failed)"
  fi
  local cursor_probe rc
  cursor_probe=$(probe_cursor) && rc=0 || rc=$?
  case $rc in
  0) cursor=$cursor_probe ;;
  1) warn "cursor quota unknown (no cursor-agent CLI)" ;;
  2) warn "cursor quota unknown — no usable cursor-agent access token (auth.json fields tried: accessToken, access_token, token; macOS: keychain item cursor-access-token)" ;;
  3) warn "cursor quota unknown — no cursor account id (cli-config.json authInfo.authId/userId, JWT sub)" ;;
  4) warn "cursor quota unknown — usage-summary call failed (HTTP error, expired login, or timeout)" ;;
  *) warn "cursor quota unknown — usage-summary response not recognised" ;;
  esac
  local pi_probe
  pi_probe=$(probe_pi) && rc=0 || rc=$?
  if [[ $rc -eq 0 ]]; then
    pi=$pi_probe
  elif [[ $rc -eq 2 ]]; then
    warn "pi spend unknown — set OPENROUTER_API_KEY or programs.dispatcher.openrouter.keyFile (DISPATCH_OPENROUTER_KEY_FILE), or log pi in to OpenRouter"
  elif [[ $rc -eq 3 ]]; then
    warn "pi spend unknown — DISPATCH_OPENROUTER_KEY_FILE ($DISPATCH_OPENROUTER_KEY_FILE) is unreadable or empty"
  else
    warn "pi spend unknown — OpenRouter /api/v1/key call failed"
  fi

  mkdir -p "$OUT_DIR"
  local tmp="$OUT.tmp.$$"
  jq -n \
    --arg fetched_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson fetched_epoch "$(date +%s)" \
    --argjson claude "$claude" \
    --argjson codex "$codex" \
    --argjson cursor "$cursor" \
    --argjson pi "$pi" \
    '{
       fetched_at: $fetched_at,
       fetched_epoch: $fetched_epoch,
       engines: {claude: $claude, codex: $codex, cursor: $cursor, pi: $pi}
     }' >"$tmp"
  mv "$tmp" "$OUT"

  printf '%s\n' "$OUT"

  report
}

if [[ $report_mode == true ]]; then
  [[ -f $OUT ]] || {
    warn "no cached budget at $OUT — run refresh-budget"
    exit 1
  }
  if [[ $json_mode == true ]]; then
    report_json
  else
    report
  fi
  exit 0
fi

main "$@"
