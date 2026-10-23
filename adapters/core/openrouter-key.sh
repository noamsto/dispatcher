#!/usr/bin/env bash
# The OpenRouter key resolver: one definition sourced by refresh-budget and
# refresh-scores, so the two cannot drift.
#
# Baked into those as @openrouterKeyLib@ by flake.nix; raw-source runs (bats)
# point $OPENROUTER_KEY_LIB at this file — the override is for raw-source test
# runs only. Sourced, never executed — the shebang keeps the CI shellcheck glob
# happy. The key resolves into the caller's `or_key` local and is never
# printed.

# _or_key — resolve the OpenRouter key into the caller's `or_key` local
# (never printed). DISPATCH_OPENROUTER_KEY_FILE, when set, is the exclusive
# source — an unreadable file or an empty first line leaves `or_key` empty
# rather than falling back to OPENROUTER_API_KEY, since spend is per key and
# the wrong key would pace pi against the wrong spend. Otherwise reads
# OPENROUTER_API_KEY, else pi's auth store, then its auth CLI.
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
# auth.json first, read-only: {"type":"api_key","key":...} (what pi writes for
# its OpenRouter login, since OpenRouter's OAuth hands back a plain API key) or
# {"type":"oauth","access":...}. Then pi's CLI, but only a pi that has the
# `auth` subcommand (0.83+): on an older pi the words become a prompt and pi
# runs a billed agent turn. Anything else leaves or_key empty.
_or_key_from_pi() {
  local auth="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}/auth.json" v="" ver=""
  if [[ -r $auth ]]; then
    v=$(jq -r '.openrouter | if .type == "api_key" then .key elif .type == "oauth" then .access else empty end | strings' "$auth" 2>/dev/null) || v=""
  fi
  if [[ ! $v =~ ^sk-or-v1-[A-Za-z0-9]+$ ]] && command -v pi >/dev/null 2>&1; then
    ver=$(timeout 5 pi --version </dev/null 2>/dev/null) || ver=""
    if [[ $ver =~ ([0-9]+)\.([0-9]+) ]] && ((BASH_REMATCH[1] > 0 || BASH_REMATCH[2] >= 83)); then
      v=$(cd / && timeout -k 2 10 pi auth print-api-key --provider openrouter </dev/null 2>/dev/null) || v=""
    fi
  fi
  [[ $v =~ ^sk-or-v1-[A-Za-z0-9]+$ ]] && or_key=$v
  return 0
}
