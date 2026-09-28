#!/usr/bin/env bash
# dispatch-config — resolve the dispatcher's settings and print them as JSON.
#
# Four layers, merged with jq `*` (objects merge recursively, a later layer
# wins per key; arrays and scalars are replaced whole):
#   base   — defaults.json, baked in at build; a raw-source run reads the copy
#            beside this script.
#   user   — ${XDG_CONFIG_HOME:-~/.config}/dispatcher/settings.json, optional.
#   locked — when built by the home-manager module, the file baked in at
#            @lockedSettings@; otherwise the file named by
#            $DISPATCH_LOCKED_SETTINGS, optional. Once set it must be a
#            readable JSON object (fail closed: it is the security layer). A
#            baked build ignores $DISPATCH_LOCKED_SETTINGS, warning on stderr
#            if it names a different path.
#   env    — DISPATCH_ENGINES (whitespace-split), DISPATCH_GRANT_ROOTS
#            (colon-split), DISPATCH_OPENROUTER_MONTHLY_USD,
#            DISPATCH_OPENROUTER_KEY_FILE, DISPATCH_PROFILE,
#            DISPATCH_REPO_TRACKERS/DISPATCH_ORG_TRACKERS (whitespace-separated
#            key=value pairs, split at the first "="; a repeated key is
#            last-wins); an empty var contributes nothing.
#
# Security: grantRoots and openrouter.keyFile widen what a worker can read, so
# they are honoured only from the locked layer or the environment — a copy in
# the base or user layer is dropped with a warning on stderr.
#
# Tracker map keys (repoTrackers/orgTrackers) are lowercased within each layer
# before the merge, so a later layer's entry for the same key (any case)
# collapses and wins.
#
# --show-origin prints the same tree with every leaf replaced by
# {"value": …, "origin": "base|user|locked|env"}.
set -euo pipefail

show_origin=false
if [[ $# -eq 1 && $1 == --show-origin ]]; then
  show_origin=true
elif [[ $# -ne 0 ]]; then
  echo "usage: dispatch-config [--show-origin]" >&2
  exit 2
fi

die() {
  echo "dispatch-config: $*" >&2
  exit 1
}

# holds(p) — the value at path p exists, even when it is null.
# shellcheck disable=SC2016 # jq program text, not shell
jq_defs='def holds($p): (try getpath($p[:-1]) catch null) | type == "object" and has($p[-1]);'

# layer <path> — the file's single top-level JSON object, compacted.
layer() {
  jq -ces 'select(length == 1 and (.[0] | type) == "object") | .[0]' "$1" 2>/dev/null ||
    die "$1 is not a JSON object"
}

# check_no_blank_engines <json> <path> — die, naming <path>, when the layer's
# engines array holds an empty or whitespace-only string.
check_no_blank_engines() {
  if jq -e '(if (.engines | type) == "array" then .engines else [] end) | any(type == "string" and (gsub("^\\s+|\\s+$"; "") == ""))' <<<"$1" >/dev/null; then
    die "$2 sets a blank engine name in engines"
  fi
}

# strip <json> <path> — drop the keys only the locked and env layers may set.
strip() {
  local key
  for key in grantRoots openrouter.keyFile; do
    if jq -e --arg k "$key" "$jq_defs"' holds($k | split("."))' <<<"$1" >/dev/null; then
      echo "dispatch-config: ignoring $key from $2 — it is honoured only from the locked settings or the environment" >&2
    fi
  done
  jq -c 'del(.grantRoots) | if (.openrouter | type) == "object" then del(.openrouter.keyFile) else . end' <<<"$1"
}

base_file="@defaultsJson@"
if [[ $base_file == @* ]]; then
  base_file="$(dirname "${BASH_SOURCE[0]}")/defaults.json"
fi
base=$(layer "$base_file")
base=$(strip "$base" "$base_file")

user_file="${XDG_CONFIG_HOME:-$HOME/.config}/dispatcher/settings.json"
user='{}'
if [[ -e $user_file ]]; then
  user=$(layer "$user_file")
  check_no_blank_engines "$user" "$user_file"
  user=$(strip "$user" "$user_file")
fi

locked_file="@lockedSettings@"
if [[ $locked_file == @* ]]; then
  locked_file="${DISPATCH_LOCKED_SETTINGS:-}"
elif [[ -n ${DISPATCH_LOCKED_SETTINGS:-} && $DISPATCH_LOCKED_SETTINGS != "$locked_file" ]]; then
  echo "dispatch-config: ignoring DISPATCH_LOCKED_SETTINGS — this build bakes $locked_file" >&2
fi
locked='{}'
if [[ -n $locked_file ]]; then
  [[ -r $locked_file ]] || die "$locked_file is not readable"
  locked=$(layer "$locked_file")
  check_no_blank_engines "$locked" "$locked_file"
fi

env_layer=$(jq -cn '
  def from_env($var; $p; f): ($ENV[$var] // "") as $v | if $v == "" then . else setpath($p; $v | f) end;
  def from_env_nonempty($var; $p; f):
    ($ENV[$var] // "") as $v
    | if $v == "" then .
      else ($v | f) as $r | if ($r | length) > 0 then setpath($p; $r) else . end
      end;
  def trackers:
    [splits("\\s+")] | map(select(. != ""))
    | reduce .[] as $e ({}; ($e | capture("^(?<k>[^=]*)=(?<v>.*)$") // {k: $e, v: $e}) as $kv | . + {($kv.k | ascii_downcase): $kv.v});
  {}
  | from_env_nonempty("DISPATCH_ENGINES"; ["engines"]; [splits("\\s+")] | map(select(. != "")))
  | from_env("DISPATCH_GRANT_ROOTS"; ["grantRoots"]; split(":") | map(select(. != "")))
  | from_env("DISPATCH_OPENROUTER_MONTHLY_USD"; ["openrouter", "monthlyUsd"]; .)
  | from_env("DISPATCH_OPENROUTER_KEY_FILE"; ["openrouter", "keyFile"]; .)
  | from_env("DISPATCH_PROFILE"; ["profile"]; .)
  | from_env_nonempty("DISPATCH_REPO_TRACKERS"; ["repoTrackers"]; trackers)
  | from_env_nonempty("DISPATCH_ORG_TRACKERS"; ["orgTrackers"]; trackers)')

printf '%s\n' "$base" "$user" "$locked" "$env_layer" | jq -n --argjson show_origin "$show_origin" "$jq_defs"'
  def string_array: type == "array" and all(.[]; type == "string");
  def need($p; $what; ok):
    if holds($p) and (getpath($p) | ok | not)
    then "dispatch-config: \($p | join(".")) must be \($what) (merged settings)\n" | halt_error(1)
    else . end;
  def tag($layers; $p):
    if type == "object" and length > 0
    then with_entries(.key as $k | .value |= tag($layers; $p + [$k]))
    else {value: ., origin: ($layers | to_entries | map(select(.value | holds($p)) | .key) | last)}
    end;
  def lower_trackers:
    reduce ("repoTrackers", "orgTrackers") as $k
      (.; if (.[$k] | type) == "object" then .[$k] |= with_entries(.key |= ascii_downcase) else . end);
  [inputs | lower_trackers] as [$base, $user, $locked, $env]
  | $base * $user * $locked * $env
  | need(["engines"]; "a non-empty array of strings"; string_array and length > 0)
  | need(["grantRoots"]; "an array of strings without \":\""; string_array and all(.[]; contains(":") | not))
  | need(["openrouter"]; "an object"; type == "object")
  | need(["openrouter", "keyFile"]; "a string"; type == "string")
  | need(["openrouter", "monthlyUsd"]; "a number or string"; type == "number" or type == "string")
  | need(["profile"]; "a string"; type == "string")
  | need(["repoTrackers"]; "an object of strings"; type == "object" and all(.[]; type == "string"))
  | need(["orgTrackers"]; "an object of strings"; type == "object" and all(.[]; type == "string"))
  | if $show_origin then tag({base: $base, user: $user, locked: $locked, env: $env}; []) else . end'
