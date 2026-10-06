#!/usr/bin/env bash
# The quota predicates over refresh-budget's engine-budget.json: a window at
# >=95% that has not reset, and an engine's authoritative limit_reached. One
# definition sourced by dispatch's launch gates and crew's stall-watch, so the
# gate that refuses a launch and the watcher that flags a running worker cannot
# drift. Tri-state, because the cache is advisory: callers must be able to tell
# "clear" from "can't tell" (a stale or blind cache never reads as clear).
#
# Baked into those as @budgetGateLib@ by flake.nix; raw-source runs (bats)
# point $BUDGET_GATE_LIB at this file — the override is for raw-source test
# runs only, set from the dispatcher's own env, never the worker's. Sourced,
# never executed — the shebang keeps the CI shellcheck glob happy.

# _budget_query <cache> <engine> <now> <jq-filter> — print the lines
# <jq-filter> yields over the engine's cache entry (bound as $en, beside $e and
# $now). Returns 0 when it printed any, 1 when none, 2 when the cache can't
# tell: missing or unparsable, fetched_epoch not a number or over 2h old, or
# the engine's entry null (its probe failed or never ran).
_budget_query() {
  local out
  out=$(jq -r --arg e "$2" --argjson now "$3" '
    if (.fetched_epoch | type) != "number" or .fetched_epoch < $now - 7200
       or .engines[$e] == null then "unknown"
    else "known", (.engines[$e] as $en | '"$4"')
    end' "$1" 2>/dev/null) || return 2
  case "$out" in
  unknown) return 2 ;;
  known) return 1 ;;
  esac
  printf '%s\n' "${out#known$'\n'}"
}

# _budget_windows <cache> <engine> <now> — one `key<TAB>used_pct<TAB>
# resets_epoch<TAB>resets_iso` line per window at >=95% whose resets_at is null
# or still ahead (the cache can predate a reset); both reset fields are empty
# for a null resets_at. Returns per _budget_query.
# shellcheck disable=SC2016 # jq program text, not shell
_budget_windows() {
  _budget_query "$1" "$2" "$3" '
    ($en.windows // {}) | to_entries[]
    | select(.value.used_pct >= 95
             and (.value.resets_at == null or .value.resets_at > $now))
    | [.key, .value.used_pct,
       (if .value.resets_at then .value.resets_at else "" end),
       (if .value.resets_at then .value.resets_at | todateiso8601 else "" end)]
    | @tsv'
}

# _budget_limit <cache> <engine> <now> — one `reason<TAB>resets_epoch<TAB>
# resets_iso` line when the engine's limit_reached holds even with every window
# under 95%. Codex: a rate-limit-reached reason, a zeroed individual spend
# limit, spend control reached, or ordinary usage denied. Cursor: any limit
# object unless its resets_at has passed (the only limit carrying a reset).
# Pi: the OpenRouter key's own credit limit. Other engines have no such limit.
# Returns per _budget_query.
# shellcheck disable=SC2016 # jq program text, not shell
_budget_limit() {
  _budget_query "$1" "$2" "$3" '
    $en.limit_reached as $l
    | if $e == "pi" then ($l.reason // empty) | [., "", ""]
      elif $e == "cursor" then
        if $l == null or ($l.resets_at != null and $l.resets_at <= $now) then empty
        else [$l.reason // "limit reached",
              (if $l.resets_at then $l.resets_at else "" end),
              (if $l.resets_at then $l.resets_at | todateiso8601 else "" end)]
        end
      elif $e == "codex" then ($l // {}) as $l
        | if $l.rate_limit_reached_type != null then [$l.rate_limit_reached_type, "", ""]
          elif $l.individual_remaining_percent == 0 then ["spend control: 0% remaining", "", ""]
          elif $l.spend_control_reached == true then ["spend control reached", "", ""]
          elif $l.ordinary_usage_allowed == false then ["ordinary use not allowed", "", ""]
          else empty end
      else empty end
    | @tsv'
}
