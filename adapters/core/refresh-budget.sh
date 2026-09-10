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
#   cursor — always null. Probed cursor-agent 2026.07: `status` is auth-only,
#     `about` shows the tier string but no numbers, and no usage/quota
#     subcommand exists. Plan usage is dashboard-only; the Enterprise Admin
#     API reports org-wide consumption events (admin key required), never a
#     member's remaining allowance.
set -euo pipefail

OUT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
OUT="$OUT_DIR/engine-budget.json"
STATUSLINE_CACHE="$OUT_DIR/claude-statusline.json"
CREDENTIALS="$HOME/.claude/.credentials.json"
STALE_AFTER_S=7200

warn() { printf 'refresh-budget: %s\n' "$*" >&2; }

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
# Anchored to the last 2 non-empty lines of each capture — the statusline
# sits one line above the input-box indicator by construction (verified
# live, pane %228, 2026-08-11):
#   🤖 Sonnet 5 🧠 high | 📊 170k/1M | ⚡ 69% (3h9m → 05:20) 7d 56% (9h49m)
#   -- INSERT -- ⏵⏵ auto mode on (shift+tab to cycle) · ← 3 agents
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
# The percentage is the LAST digit run of the text before the first '%',
# not the first run of the whole match: the first run breaks on the "7d"
# marker's own leading digit ("7d 61%" -> "761"), and the last run of the
# whole match breaks on the " -> HH:MM" tail instead ("(10m -> 05:20)" ->
# "20"). Only the text before '%' is scanned, so both traps are avoided at
# once.
#
# A derived clock is a best effort, not a live one: tmux capture-pane
# returns the pane's last render, so "now + remaining" inherits however
# stale that render is. That's the conservative direction for the rung gate
# — an overstated remaining understates elapsed_pct and makes it refuse
# harder, never less. The day-form (NNd) branch is implemented but
# UNOBSERVED: a live capture of every crew pane on this host showed no 7d
# segment rendered at all, so the only in-repo sample of the grammar is
# NNhNNm.
_pane_countdown() {
  local m="$1" nominal="$2" pct paren inner re d h mnt remaining=-1
  pct=$(printf '%s' "${m%%\%*}" | grep -oE '[0-9]+' | tail -1)
  paren=$(printf '%s' "$m" | grep -oE '\([^)]*\)$')
  if [[ -n $paren ]]; then
    inner="${paren#(}"
    inner="${inner%)}"
    re='^([0-9]+d)?([0-9]+h)?([0-9]+m)?( → [0-9]{2}:[0-9]{2})?$'
    if [[ $inner =~ $re ]] && [[ -n ${BASH_REMATCH[1]}${BASH_REMATCH[2]}${BASH_REMATCH[3]} ]]; then
      d="${BASH_REMATCH[1]%d}"
      h="${BASH_REMATCH[2]%h}"
      mnt="${BASH_REMATCH[3]%m}"
      remaining=$((${d:-0} * 86400 + ${h:-0} * 3600 + ${mnt:-0} * 60))
      ((remaining <= nominal)) || remaining=-1
    fi
  fi
  printf '%s\t%s\n' "$pct" "$remaining"
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
      tail=$(printf '%s\n' "$text" | grep -v '^[[:space:]]*$' | tail -2)
      m=$(printf '%s\n' "$tail" | grep -oE '⚡[[:space:]]*[0-9]+%([[:space:]]*\([^)]*\))?' | tail -1)
      if [[ -n $m ]]; then
        IFS=$'\t' read -r v rem <<<"$(_pane_countdown "$m" 18000)"
        if ((v > max5)) || { ((v == max5)) && ((rem > rem5)); }; then
          max5=$v
          rem5=$rem
        fi
      fi
      m=$(printf '%s\n' "$tail" | grep -oE '7d[[:space:]]*[0-9]+%([[:space:]]*\([^)]*\))?' | tail -1)
      if [[ -n $m ]]; then
        IFS=$'\t' read -r v rem <<<"$(_pane_countdown "$m" 604800)"
        if ((v > max7)) || { ((v == max7)) && ((rem > rem7)); }; then
          max7=$v
          rem7=$rem
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
    (map(select(.id == 2)) | .[0].result.rateLimits) as $r
    | {
        source: "app-server",
        credits_cover: ($r.credits.hasCredits // false),
        windows: (
          {}
          + (if $r.primary.usedPercent != null then {(wname($r.primary.windowDurationMins | if . != null then . * 60 else null end)): {used_pct: $r.primary.usedPercent, resets_at: $r.primary.resetsAt}} else {} end)
          + (if $r.secondary != null and $r.secondary.usedPercent != null then {(wname($r.secondary.windowDurationMins | if . != null then . * 60 else null end)): {used_pct: $r.secondary.usedPercent, resets_at: $r.secondary.resetsAt}} else {} end)
        )
      }
  ' <<<"$resp" 2>/dev/null
}

main() {
  local claude='null' codex='null' probe
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

  mkdir -p "$OUT_DIR"
  local tmp="$OUT.tmp.$$"
  jq -n \
    --arg fetched_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson fetched_epoch "$(date +%s)" \
    --argjson claude "$claude" \
    --argjson codex "$codex" \
    '{
       fetched_at: $fetched_at,
       fetched_epoch: $fetched_epoch,
       engines: {claude: $claude, codex: $codex, cursor: null}
     }' >"$tmp"
  mv "$tmp" "$OUT"

  printf '%s\n' "$OUT"

  # Shared by both jq programs below: a relative-duration renderer and the
  # nominal window lengths a pace figure can be computed against. 7d is a
  # real budget and gets its pace ("N points ahead of pace") named in
  # the advisory; 5h is a short rate limit that just gets a wait-vs-shed
  # steer; everything else (1d, unknown, other — codex's non-5h/7d buckets)
  # has no known length and stays generic. The pace figure uses the same
  # formula as dispatch.sh's gate 2 (minus its 70 floor and 15-point
  # threshold, which are the gate's business, not an advisory's), so the two
  # renderers never disagree on a number.
  local now jq_time_defs
  now=$(date +%s)
  # shellcheck disable=SC2016  # jq's own $vars, not bash expansions
  jq_time_defs='
    def reltime: . as $s
      | ($s / 86400 | floor) as $d
      | (($s % 86400) / 3600 | floor) as $h
      | (($s % 3600) / 60 | floor) as $m
      | if $d > 0 then "\($d)d \($h)h"
        elif $h > 0 then "\($h)h \($m)m"
        else "\($m)m" end;
    def wsecs($k): if $k == "5h" then 18000
      elif ($k == "7d" or $k == "7d_opus" or $k == "7d_sonnet") then 604800
      else null end;
    def elapsed_pct($resets_at; $L): (100 * ($L - ($resets_at - $now)) / $L) as $x
      | if $x < 0 then 0 elif $x > 100 then 100 else $x end;
  '

  jq -r --argjson now "$now" "$jq_time_defs"'
    .engines | to_entries[] | select(.value != null) | .key as $e |
    .value.windows | to_entries[] | select(.value.used_pct >= 85) |
    .key as $k | .value as $w |
    (if $k == "5h" then "5h" elif ($k == "7d" or $k == "7d_opus" or $k == "7d_sonnet") then "7d" else "other" end) as $fam |
    (wsecs($k)) as $L |
    (if $w.resets_at != null and $w.resets_at > $now then ($w.resets_at - $now) else null end) as $rem |
    (if $fam == "7d" and $rem != null then (($w.used_pct - elapsed_pct($w.resets_at; $L)) | round) else null end) as $ahead |
    (if $rem == null then ""
     else " (resets in \($rem | reltime)" + (if $ahead != null then ", \($ahead) points ahead of pace" else "" end) + ")"
     end) as $paren |
    (if $fam == "5h" then "short window: prefer waiting past the reset to shedding burn class"
     elif $fam == "7d" then "real budget: prefer a cheaper burn class or rotate engines"
     else "approaching quota (>=85%), prefer a cheaper burn class or rotate engines (see DISPATCHER_PROTOCOL.md)"
     end) as $advice |
    "\($e) \($k) at \($w.used_pct)%\($paren) — \($advice)"
  ' "$OUT" |
    while IFS= read -r line; do
      warn "budget lever: $line"
    done || true

  jq -r --argjson now "$now" "$jq_time_defs"'
    .engines | to_entries[] | .key as $e |
    if .value == null then "\($e): unknown"
    else "\($e): " + ([.value.windows | to_entries[] |
        "\(.key) \(.value.used_pct)% used" +
        (if .value.resets_at then
           " (resets \(.value.resets_at | todateiso8601)" +
           (if .value.resets_at > $now then ", in \((.value.resets_at - $now) | reltime)" else "" end) +
           ")"
         else "" end)
      ] | join(", ")) + (if .value.credits_cover then " [credits cover]" else "" end)
    end
  ' "$OUT"
}

main "$@"
