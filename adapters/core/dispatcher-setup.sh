#!/usr/bin/env bash
# dispatcher setup — detect what this machine offers, ask only for preferences,
# show a diff and write the user settings file
# (${XDG_CONFIG_HOME:-~/.config}/dispatcher/settings.json). Re-runnable: keys the
# owner already set are kept unless they accept a change, unknown keys survive
# verbatim. Never writes the locked layer, grantRoots or openrouter.keyFile
# (dispatch-config honours those only from the locked layer or env).
#
# usage: dispatcher setup [--yes] | [--check [--json]]
#   --yes     take the detected proposals without prompting or confirming
#   --check   no writes, no prompts: one line per key; exit 1 when a gap exists
#   --json    with --check, print the report as JSON
#
# A user file that is a symlink (home-manager) is never written: the proposed
# JSON is printed for the owner's nix config instead.
set -euo pipefail

usage() {
  echo "usage: dispatcher setup [--yes] | [--check [--json]]" >&2
  exit 2
}

check=0
json=0
yes=0
while [ $# -gt 0 ]; do
  case "$1" in
  --check) check=1 ;;
  --json) json=1 ;;
  --yes) yes=1 ;;
  -h | --help) usage ;;
  *) usage ;;
  esac
  shift
done
if [ "$json" = 1 ] && [ "$check" = 0 ]; then usage; fi
if [ "$check" = 1 ] && [ "$yes" = 1 ]; then usage; fi

launcher_common_lib="${LAUNCHER_COMMON_LIB:-@launcherCommonLib@}"
# replaceStrings rewrites the token on both sides of an equality check, so a
# string compare against the placeholder always matches after bake.
if [ ! -f "$launcher_common_lib" ]; then
  launcher_common_lib="$(dirname -- "${BASH_SOURCE[0]}")/launcher-common.sh"
fi
# shellcheck source=/dev/null
. "$launcher_common_lib"

openrouter_key_lib="${OPENROUTER_KEY_LIB:-@openrouterKeyLib@}"
if [ ! -f "$openrouter_key_lib" ]; then
  openrouter_key_lib="$(dirname -- "${BASH_SOURCE[0]}")/openrouter-key.sh"
fi
# shellcheck source=/dev/null
. "$openrouter_key_lib"

cfg_bin="${DISPATCH_CONFIG_BIN:-@dispatchConfig@}"
ENGINES_ALL="claude codex cursor pi"

interactive=0
# DISPATCHER_SETUP_INTERACTIVE=1 overrides only the [ -t 0 ] check (test seam for piped stdin).
if [ "$check" = 0 ] && [ "$yes" = 0 ] && { [ -t 0 ] || [ "${DISPATCHER_SETUP_INTERACTIVE:-}" = 1 ]; }; then interactive=1; fi

merged="$("$cfg_bin")"
origins="$("$cfg_bin" --show-origin)"
user_file="$("$cfg_bin" --layers | jq -r '.user.path')"
baseline_warnings="$("$cfg_bin" 2>&1 >/dev/null || true)"

# cur <jq path array> — {value, origin}: the merged value, and the layers that
# set its leaves ("unset" when none does).
cur() {
  jq -c --argjson p "$1" --argjson o "$origins" '
    {value: getpath($p),
     origin: ([$o | getpath($p) | .. | objects | select(has("origin") and has("value")) | .origin] | unique | if length == 0 then "unset" else join("+") end)}' <<<"$merged"
}

# ---- Detect ---------------------------------------------------------------
installed=()
for e in $ENGINES_ALL; do
  if command -v "$(engine_cli "$e")" >/dev/null 2>&1; then installed+=("$e"); fi
done
installed_json="$(jq -cn '$ARGS.positional' --args "${installed[@]}")"

cores="$(nproc)"
proposed_budget=$((cores / 2))
if [ "$proposed_budget" -lt 2 ]; then proposed_budget=2; fi
if [ "$proposed_budget" -gt 16 ]; then proposed_budget=16; fi

configured_hosts="$(jq -c '(.hosts // {}) | keys' <<<"$merged")"
host_candidates=()
ssh_config="${HOME:-/nonexistent}/.ssh/config"
if [ -r "$ssh_config" ]; then
  while IFS= read -r h; do
    [[ $h =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || continue
    jq -e --arg h "$h" 'index($h) | not' <<<"$configured_hosts" >/dev/null || continue
    timeout 3 ssh -G -- "$h" >/dev/null 2>&1 || continue
    host_candidates+=("$h")
  done < <(awk 'tolower($1) == "host" { for (i = 2; i <= NF; i++) if ($i !~ /[*?!]/) print $i }' "$ssh_config" | sort -u)
fi
hosts_json="$(jq -cn '$ARGS.positional' --args "${host_candidates[@]}")"

# The key source, never the key: the order mirrors refresh-budget (the settings
# keyFile seeds DISPATCH_OPENROUTER_KEY_FILE when the env leaves it unset).
or_found=false
or_source="none"
detect_openrouter_key() {
  local or_key keyfile
  keyfile="$(jq -r '.openrouter.keyFile // ""' <<<"$merged")"
  if [ -n "${DISPATCH_OPENROUTER_KEY_FILE:-}" ]; then
    or_source="DISPATCH_OPENROUTER_KEY_FILE"
  elif [ -n "$keyfile" ]; then
    DISPATCH_OPENROUTER_KEY_FILE="$keyfile"
    or_source="openrouter.keyFile ($(cur '["openrouter","keyFile"]' | jq -r .origin))"
  elif [ -n "${OPENROUTER_API_KEY:-}" ]; then
    or_source="OPENROUTER_API_KEY"
  else
    or_source="pi auth"
  fi
  _or_key || true
  if [ -n "$or_key" ]; then or_found=true; else or_source="none"; fi
}
detect_openrouter_key

has_overseer=false
if command -v overseer >/dev/null 2>&1; then has_overseer=true; fi

# ---- --check --------------------------------------------------------------
entries='[]'
# entry <key> <current-json> <origin> <detected-json> <gap>
entry() {
  entries="$(jq -c --arg k "$1" --argjson c "$2" --arg o "$3" --argjson d "$4" --arg g "$5" \
    '. + [{key: $k, current: $c, origin: $o, detected: $d, gap: $g}]' <<<"$entries")"
}

# entry_for <key> <jq path array> <detected-json> <gap>
entry_for() {
  local c
  c="$(cur "$2")"
  entry "$1" "$(jq -c .value <<<"$c")" "$(jq -r .origin <<<"$c")" "$3" "$4"
}

report() {
  local gap c enabled
  c="$(cur '["engines"]')"
  gap=""
  enabled="$(jq -c .value <<<"$c")"
  if [ "$enabled" = null ]; then
    [ "${#installed[@]}" -eq 0 ] || gap="engines unset: every engine is enabled, installed here: ${installed[*]}"
  elif ! jq -e --argjson i "$installed_json" '. as $e | all($e[]; . as $x | $i | index($x))' <<<"$enabled" >/dev/null; then
    gap="enabled engines not installed here"
  elif [ "$(jq -r .origin <<<"$c")" = base ] && ! jq -e --argjson i "$installed_json" '. as $e | all($i[]; . as $x | $e | index($x))' <<<"$enabled" >/dev/null; then
    gap="installed engines not enabled"
  fi
  entry_for engines '["engines"]' "$installed_json" "$gap"

  entry_for hosts '["hosts"]' "$hosts_json" ""

  c="$(cur '["fleetBudget"]')"
  gap=""
  if [ "$(jq -r .origin <<<"$c")" = base ]; then
    gap="fleetBudget is the built-in default; suggested $proposed_budget for $cores cores"
  fi
  entry_for fleetBudget '["fleetBudget"]' "$proposed_budget" "$gap"

  gap=""
  if [ "$or_found" = true ] && [ "$(cur '["openrouter","monthlyUsd"]' | jq -c .value)" = null ]; then
    gap="pi has no monthly target, so the pace gate cannot protect it"
  fi
  entry_for openrouter.monthlyUsd '["openrouter","monthlyUsd"]' "null" "$gap"

  entry_for anthropicCredits '["anthropicCredits"]' null ""
  entry_for dashAutoOpen '["dashAutoOpen"]' null ""
  entry_for overseer.autoRelaunch '["overseer","autoRelaunch"]' "$has_overseer" ""
  entry_for localModels '["localModels"]' null ""
  entry_for grantRoots '["grantRoots"]' null ""
  entry_for openrouter.keyFile '["openrouter","keyFile"]' null ""
}

managed=false
if [ -L "$user_file" ]; then managed=true; fi

if [ "$check" = 1 ]; then
  report
  gaps="$(jq -c '[.[] | select(.gap != "") | .key + ": " + .gap]' <<<"$entries")"
  if [ "$json" = 1 ]; then
    jq -n --arg f "$user_file" --argjson m "$managed" --argjson g "$gaps" --argjson k "$entries" \
      --argjson found "$or_found" --arg src "$or_source" \
      '{userFile: $f, managed: $m, gaps: $g, keys: $k, openrouterKey: {found: $found, source: $src}}'
  else
    jq -r '.[] | "\(.key): \(.current | tojson) [\(.origin)], detected \(.detected | tojson)" + (if .gap != "" then ", GAP: \(.gap)" else "" end)' <<<"$entries"
    echo "openrouter key: $or_source"
  fi
  [ "$gaps" = '[]' ]
  exit
fi

# ---- Propose --------------------------------------------------------------
old='{}'
if [ -r "$user_file" ]; then old="$(jq -c . "$user_file")"; fi
updates='{}'

# set_update <jq path array> <json value>
set_update() {
  updates="$(jq -c --argjson p "$1" --argjson v "$2" 'setpath($p; $v)' <<<"$updates")"
}

# prompt <text> — the owner's answer, empty when they just press enter.
prompt() {
  local ans
  read -r -p "$1" ans || ans=""
  printf '%s' "$ans"
}

# decide <label> <jq path array> <proposed-json|""> <parse fn>
# The default is the owner's own value when the user file holds one (a change
# needs an answer), else the proposal. A locked or env layer wins anyway, so
# those keys are skipped.
decide() {
  local label="$1" path="$2" proposal="$3" parse="$4" c origin value shown ans new
  c="$(cur "$path")"
  origin="$(jq -r .origin <<<"$c")"
  value="$(jq -c .value <<<"$c")"
  case "$origin" in
  *locked* | *env*)
    echo "$label: set by the $origin layer, left alone"
    return
    ;;
  esac
  new="$proposal"
  shown="${proposal:-none}"
  if [[ $origin == user ]]; then
    new=""
    shown="$value"
  fi
  if [ "$interactive" = 1 ]; then
    ans="$(prompt "$label [$shown]: ")"
    if [ -n "$ans" ]; then
      new="$("$parse" "$ans")" || {
        echo "$label: ignoring invalid answer '$ans'" >&2
        new=""
      }
    fi
  fi
  # A proposal equal to the built-in default is still written: it moves the key
  # out of "base", which --check reads as unconfigured.
  if [ -n "$new" ] && { [ "$new" != "$value" ] || [ "$new" = "$proposal" ]; }; then set_update "$path" "$new"; fi
}

parse_engines() {
  local w words
  read -ra words <<<"$1"
  for w in "${words[@]}"; do
    case " $ENGINES_ALL " in *" $w "*) ;; *) return 1 ;; esac
  done
  jq -cn '$ARGS.positional' --args "${words[@]}"
}
parse_nonneg_int() { [[ $1 =~ ^[0-9]+$ ]] && printf '%s' "$((10#$1))"; }
parse_positive_number() { [[ $1 =~ ^[0-9]+(\.[0-9]+)?$ ]] && jq -cse 'if length == 1 and (.[0] | type == "number") and .[0] > 0 then .[0] else empty end' <<<"$1"; }
parse_bool() {
  case "$1" in
  y | Y | yes | true) printf true ;;
  n | N | no | false) printf false ;;
  *) return 1 ;;
  esac
}
parse_credits() { jq -cse 'if length == 1 and (.[0] | type == "object") then .[0] else empty end' <<<"$1"; }

if [ "${#installed[@]}" -gt 0 ]; then
  decide engines '["engines"]' "$installed_json" parse_engines
fi
decide fleetBudget '["fleetBudget"]' "$proposed_budget" parse_nonneg_int

if [ "$interactive" = 1 ]; then
  for h in "${host_candidates[@]}"; do
    [ "$(prompt "add ssh host $h? [y/N]: ")" = y ] || continue
    if timeout 8 ssh -o BatchMode=yes -o ConnectTimeout=5 -- "$h" true </dev/null >/dev/null 2>&1; then
      set_update "[\"hosts\",\"$h\"]" "$(jq -cn --arg h "$h" '{ssh: $h}')"
    else
      echo "host $h: ssh check failed, not added" >&2
    fi
  done

  if [ "$or_found" = true ]; then
    decide openrouter.monthlyUsd '["openrouter","monthlyUsd"]' "" parse_positive_number
  fi
  echo 'anthropicCredits schema: {"balance_usd": 50, "expires": "2027-01-01", "as_of": "2026-11-01"}'
  decide anthropicCredits '["anthropicCredits"]' "" parse_credits
  decide dashAutoOpen '["dashAutoOpen"]' "" parse_bool
  if [ "$has_overseer" = true ]; then
    decide overseer.autoRelaunch '["overseer","autoRelaunch"]' "" parse_bool
  fi
fi

# Reported, never written: only the locked layer or env may set these.
for k in grantRoots openrouter.keyFile; do
  case "$k" in
  grantRoots) p='["grantRoots"]' ptr="programs.dispatcher.grantRoots or DISPATCH_GRANT_ROOTS" ;;
  *) p='["openrouter","keyFile"]' ptr="programs.dispatcher.openrouter.keyFile or DISPATCH_OPENROUTER_KEY_FILE" ;;
  esac
  c="$(cur "$p")"
  echo "$k: $(jq -c .value <<<"$c") [$(jq -r .origin <<<"$c")], not written by setup; set it with $ptr"
done
echo "localModels: report only, endpoints need your input"
echo "openrouter key: $or_source"

candidate="$(jq -c --argjson u "$updates" 'del(.grantRoots, .openrouter.keyFile) * $u' <<<"$old")"
if jq -e '. as $o | ($o | has("grantRoots")) or ($o.openrouter // {} | has("keyFile"))' <<<"$old" >/dev/null; then
  echo "note: grantRoots / openrouter.keyFile found in $user_file are dropped from the proposal"
fi

if [ "$managed" = true ]; then
  echo "$user_file is managed (a symlink); nothing written. Put this in the file it points to:"
  jq . <<<"$candidate"
  exit 0
fi

if [ "$(jq -cS . <<<"$old")" = "$(jq -cS . <<<"$candidate")" ]; then
  echo "already up to date"
  exit 0
fi

# ---- Validate, diff, confirm, write ---------------------------------------
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/dispatcher"
jq . <<<"$candidate" >"$scratch/dispatcher/settings.json"
if ! new_warnings="$(XDG_CONFIG_HOME="$scratch" "$cfg_bin" 2>&1 >/dev/null)"; then
  echo "dispatcher setup: the proposed settings do not resolve; nothing written:" >&2
  echo "$new_warnings" >&2
  exit 1
fi
new_warnings="${new_warnings//"$scratch/dispatcher/settings.json"/$user_file}"
new_warnings="$(grep -vxF -f <(printf '%s\n' "$baseline_warnings") <<<"$new_warnings" || true)"
if [ -n "$new_warnings" ]; then
  echo "dispatcher setup: the proposed settings add warnings; nothing written:" >&2
  echo "$new_warnings" >&2
  exit 1
fi

old_file="$user_file"
[ -e "$user_file" ] || old_file=/dev/null
diff -u --label "$user_file" --label "$user_file (proposed)" "$old_file" "$scratch/dispatcher/settings.json" || true

if [ "$yes" = 0 ]; then
  if [ "$interactive" = 0 ]; then
    echo "dispatcher setup: stdin is not a terminal; pass --yes to write. Nothing written." >&2
    exit 2
  fi
  [ "$(prompt "write $user_file? [y/N]: ")" = y ] || {
    echo "nothing written"
    exit 0
  }
fi

mkdir -p "$(dirname -- "$user_file")"
tmp="$(mktemp "$(dirname -- "$user_file")/.settings.XXXXXX")"
cp "$scratch/dispatcher/settings.json" "$tmp"
mv -f "$tmp" "$user_file"
echo "wrote $user_file"
