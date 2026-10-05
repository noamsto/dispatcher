#!/usr/bin/env bash
# The localModels lane's helpers: entry lookup, the live-slot holder scan, the
# endpoint probe, and the pi models.json renderer. One definition sourced by
# crew, dispatch and refresh-budget, so the three cannot drift.
#
# Baked into those as @localModelsLib@ by flake.nix; raw-source runs (bats)
# point $LOCAL_MODELS_LIB at this file — the override is for raw-source test
# runs only, set from the dispatcher's own env, never the worker's. Sourced,
# never executed — the shebang keeps the CI shellcheck glob happy.

# _local_entry <settings-json> <id> — print the localModels entry for the pi
# dispatch id, compacted, with maxConcurrent/tiers defaults filled; nothing
# when the id has no entry.
_local_entry() {
  jq -c --arg id "$2" \
    '.localModels[$id] // empty | {maxConcurrent: 1, tiers: ["trivial", "standard"]} + . | .maxConcurrent |= floor' <<<"$1"
}

# Byte-identical copy of crew.sh's engine table, pinned by tests/adapters.bats.
_is_engine_cmd() {
  local c="${1#.}"
  c="${c%-wrapped}"
  case "$c" in
  claude | codex | cursor-agent | node | pi) return 0 ;;
  esac
  return 1
}

# _local_holders <id> — one `<name> (<branch> <role>)` line per live tmux pane
# holding the id's slot: @crew_model equals the id and the pane still runs an
# engine (_is_engine_cmd). No tmux server yields no lines, status 0. Fields are
# \x1f-separated, not tab: tab is IFS whitespace, so `read` would collapse an
# empty @crew_role.
_local_holders() {
  local model cmd name branch role
  { tmux list-panes -a -F '#{@crew_model}'$'\x1f''#{pane_current_command}'$'\x1f''#{@crew_name}'$'\x1f''#{@crew_branch}'$'\x1f''#{@crew_role}' 2>/dev/null || true; } |
    while IFS=$'\x1f' read -r model cmd name branch role; do
      [ "$model" = "$1" ] || continue
      _is_engine_cmd "$cmd" || continue
      printf '%s (%s %s)\n' "${name:-?}" "${branch:-?}" "${role:-lead}"
    done
  return 0
}

# _local_probe <baseUrl> <model> — return 0 iff the endpoint's /models lists
# the model id exactly; otherwise print a one-phrase reason ("unreachable",
# "returned an HTTP error" or "did not list '<model>'") and return 1.
_local_probe() {
  local body rc=0
  body=$(curl -fsS --proto '=http,https' --max-time 5 "$1/models" 2>/dev/null) || rc=$?
  if [ "$rc" -eq 22 ]; then
    printf 'returned an HTTP error'
    return 1
  fi
  if [ "$rc" -ne 0 ]; then
    printf 'unreachable'
    return 1
  fi
  if ! jq -e --arg m "$2" '[.data[]?.id?] | index($m) != null' <<<"$body" >/dev/null 2>&1; then
    printf "did not list '%s'" "$2"
    return 1
  fi
}

# _local_pi_models_json <settings-json> — print pi's models.json document for
# every localModels entry, grouped by provider (key text before the first `/`;
# the rest is the model id). The apiKey is a dummy pi requires; local endpoints
# ignore it. `{"providers":{}}` when localModels is absent or empty.
_local_pi_models_json() {
  jq '{providers: ((.localModels // {}) | to_entries
    | map({provider: (.key | split("/")[0]), id: (.key | sub("^[^/]*/"; "")), value})
    | group_by(.provider)
    | map({key: .[0].provider, value: {
        baseUrl: .[0].value.baseUrl,
        api: "openai-completions",
        apiKey: .[0].provider,
        models: (map({id, contextWindow: .value.contextWindow}) | sort_by(.id))}})
    | from_entries)}' <<<"$1"
}
