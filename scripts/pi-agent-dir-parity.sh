#!/usr/bin/env bash
# pi-agent-dir-parity.sh — prove `crew pi-agent-dir` still seeds exactly what it
# seeded before the seeder moved from crew.sh's _pi_agent_dir to Go (#1016).
#
# Every case builds a fixture home, runs the pre-change crew.sh (read out of git;
# the working tree is never touched), snapshots stdout, stderr, the exit status and
# the whole tree under the fixture home, then rebuilds the same fixture at the
# SAME path (the paths embed $HOME) and does the same against the working tree's
# Go binary — the entrypoint since #821, run directly with no bash and no
# crew.sh in front of it. The two snapshots must match byte for byte. Re-seed
# cases run their first seed(s) with the side under test, so each side is judged
# against its own earlier output.
#
# Run it from a dev shell, which is where `go` and `jq` live:
#
#   nix develop -c bash scripts/pi-agent-dir-parity.sh
#   nix develop -c bash scripts/pi-agent-dir-parity.sh --base origin/main
set -euo pipefail

# The last commit whose crew.sh still held the seeder — the parent of the port.
# Not origin/main: main's crew.sh has had no bash side to run since #1016, and
# the script would compare nothing against nothing (the reason help-parity.sh
# pins its own base).
base=90205b8195a31675c06d3ad78fd5ec10fec12282
while (($#)); do
  case "$1" in
  --base)
    base="${2:?--base needs a ref}"
    shift 2
    ;;
  -h | --help)
    printf 'usage: %s [--base REF]\n' "${0##*/}"
    exit 0
    ;;
  *)
    printf '%s: unknown argument %s\n' "${0##*/}" "$1" >&2
    exit 2
    ;;
  esac
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d)
home="$work/home"

cleanup() {
  # A case that dies mid-way may leave a mode-000 entry behind.
  chmod -R u+rwX "${work:?}" 2>/dev/null || true
  rm -rf "${work:?}"
}
trap cleanup EXIT

old_sh="$work/crew-old.sh"
git -C "$root" show "$base:adapters/core/crew.sh" >"$old_sh"
grep -q '^_pi_agent_dir()' "$old_sh" || {
  echo "$base's crew.sh has no _pi_agent_dir to compare against" >&2
  exit 1
}
(
  cd "$root/crew"
  go build -o "$work/crew-go" .
)
crew_go="$work/crew-go"
export CREW_GO_BIN="$crew_go"

# Nothing from the host may reach a case.
unset DISPATCH_LOCKED_SETTINGS PI_CODING_AGENT_DIR
export LOCAL_MODELS_LIB="$root/adapters/core/local-models.sh"
export DISPATCH_CONFIG_BIN="$root/adapters/core/dispatch-config.sh"

is_root=0
[ "$(id -u)" -eq 0 ] && is_root=1

ambient="$home/.pi/agent"
worker="$home/.pi/dispatcher-worker"
settings_file="$home/.config/dispatcher/settings.json"

# --- fixture helpers (run inside a case, with HOME already pointing at $home) ---

fixture() {
  mkdir -p "$ambient"
  cat >"$ambient/auth.json" <<'EOF'
{
  "opencode": {"type": "api_key", "key": "SECRET-FIXTURE-123"},
  "openrouter": {"type": "api_key", "key": "$OPENROUTER_API_KEY", "env": {"X": "y"}},
  "deepseek": {"type": "api_key", "key": "!echo x"},
  "anthropic": {"type": "oauth", "access": "a", "refresh": "r", "expires": 1},
  "a b": {"type": "api_key", "key": "bad-name"},
  "lit": {"type": "api_key", "key": "sk-a$b"}
}
EOF
  printf '{"defaultProjectTrust":"always"}\n' >"$ambient/settings.json"
  printf '{}\n' >"$ambient/trust.json"
  printf '{"models":[{"id":"deepseek/deepseek-v4.1-flash"}]}\n' >"$ambient/models-store.json"
}

dispatcher_settings() {
  mkdir -p "${settings_file%/*}"
  printf '%s\n' "$1" >"$settings_file"
}

bridge() {
  mkdir -p "$ambient/bin"
  printf '// hookyard bridge\n' >"$ambient/bin/hookyard-bridge.ts"
}

# seed — run the side under test; the last call of a case is the measured one
# (earlier calls are first seeds). The old side is a script, so bash runs it; the
# new side is the entrypoint itself. Fixtures never print secrets.
seed() {
  local rc=0
  if [ "$side" = old ]; then
    (cd "$home" && timeout 10 bash -euo pipefail "$old_sh" pi-agent-dir >"$work/$side.out" 2>"$work/$side.err") || rc=$?
  else
    (cd "$home" && timeout 10 "$crew_go" pi-agent-dir >"$work/$side.out" 2>"$work/$side.err") || rc=$?
  fi
  printf '%s\n' "$rc" >"$work/$side.rc"
}

# --- snapshots ---

# listing [extra-printf] — path, type, mode, link target of everything under the
# fixture home.
listing() {
  find "$home" -mindepth 1 -printf "%P\t%y\t%m\t%l${1:-}\n" | LC_ALL=C sort
}

contents() {
  local f
  find "$home" -type f -print0 | LC_ALL=C sort -z | while IFS= read -r -d '' f; do
    printf '== %s\n' "${f#"$home"/}"
    case "$f" in
    *.json)
      if ! jq -S -c . "$f" 2>/dev/null; then
        printf 'INVALID %s\n' "$(sha256sum <"$f" | cut -d' ' -f1)"
      fi
      ;;
    *) sha256sum <"$f" | cut -d' ' -f1 ;;
    esac
  done
}

snapshot() { # $1 = side
  {
    printf '## rc\n%s\n' "$(cat "$work/$1.rc")"
    printf '## tree\n'
    listing
    printf '## contents\n'
    contents
  } >"$work/$1.tree"
}

# --- cases ---
# Each runs in a subshell with HOME set and a clean home. Use `seed` for every
# run of the seeder; restore any mode you removed before the measured seed
# returns control.

c_fresh_full() {
  fixture
  seed
}
c_local_models() {
  fixture
  dispatcher_settings '{"localModels": {
  "lemonade/Big": {"baseUrl": "http://halo.test:13305/v1", "contextWindow": 131072, "maxTokens": 65536},
  "lemonade/R": {"baseUrl": "http://halo.test:13305/v1", "contextWindow": 4096, "reasoning": true, "thinkingFormat": "qwen-chat-template", "thinkingLevelMap": {"minimal": null}, "samplingParams": {"temperature": 0.6}, "samplingParamsByThinkingLevel": {"off": {"temperature": 0.7}}, "effortThinking": {"low": "off"}},
  "other/Plain": {"baseUrl": "http://other.test:1/v1", "contextWindow": 2048}
}}'
  seed
}
c_no_local_models() {
  fixture
  seed
}
c_local_models_dropped() {
  fixture
  dispatcher_settings '{"localModels":{"lemonade/Alpha":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}'
  seed
  rm "$settings_file"
  seed
}
c_provider_clash() {
  fixture
  printf '{"openai":{"type":"api_key","key":"sk-test-fixture"}}\n' >"$ambient/auth.json"
  dispatcher_settings '{"localModels":{"OpenAI/gpt-oss-120b":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}'
  seed
}
c_provider_clash_no_auth() {
  fixture
  rm "$ambient/auth.json"
  dispatcher_settings '{"localModels":{"openai/x":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}'
  seed
}
c_invalid_dispatcher_settings() {
  fixture
  dispatcher_settings '{"localModels":{"lemonade/Alpha":{"contextWindow":4096}}}'
  seed
}
c_hookyard_bridge() {
  fixture
  bridge
  seed
}
c_no_bridge() {
  fixture
  seed
}
c_bridge_reseed_order() {
  fixture
  bridge
  seed
  printf '{"extensions":["/some/other.ts","%s"]}\n' "$worker/bin/hookyard-bridge.ts" >"$worker/settings.json"
  seed
}
c_bridge_reseed_append() {
  fixture
  bridge
  seed
  printf '{"lastChangelogVersion":"9","defaultProjectTrust":"always","extensions":["/some/other.ts"]}\n' >"$worker/settings.json"
  seed
}
c_reseed_pi_settings() {
  fixture
  seed
  printf '{"lastChangelogVersion":"9","defaultProjectTrust":"always"}\n' >"$worker/settings.json"
  seed
}
c_worker_settings_garbage() {
  fixture
  seed
  printf 'not json{\n' >"$worker/settings.json"
  seed
}
c_worker_settings_array() {
  fixture
  seed
  printf '[1,2]\n' >"$worker/settings.json"
  seed
}
nonarray_ext() { # $1 = bad value
  fixture
  bridge
  seed
  printf '{"extensions":%s}\n' "$1" >"$worker/settings.json"
  seed
}
c_ext_object() { nonarray_ext '{"not":"an array"}'; }
c_ext_false() { nonarray_ext 'false'; }
c_ext_string() { nonarray_ext '"a string"'; }
c_ext_number() { nonarray_ext '3'; }
c_ext_null() { nonarray_ext 'null'; }
c_ext_nonarray_no_bridge() {
  fixture
  seed
  printf '{"extensions":3}\n' >"$worker/settings.json"
  seed
}
c_no_ambient_auth() {
  fixture
  rm "$ambient/auth.json"
  seed
}
c_no_ambient_auth_reseed() {
  fixture
  seed
  rm "$ambient/auth.json"
  seed
}
c_missing_ambient_dir() {
  mkdir -p "$home"
  seed
}
c_malformed_auth() {
  fixture
  printf 'torn{\n' >"$ambient/auth.json"
  seed
}
c_malformed_auth_link_kept() {
  fixture
  seed
  printf 'torn{\n' >"$ambient/auth.json"
  seed
}
c_array_auth() {
  fixture
  printf '[]\n' >"$ambient/auth.json"
  seed
}
c_string_auth() {
  fixture
  printf '"x"\n' >"$ambient/auth.json"
  seed
}
c_empty_auth() {
  fixture
  : >"$ambient/auth.json"
  seed
}
c_mode000_auth() {
  ((is_root)) && {
    skip=1
    return 0
  }
  fixture
  seed
  chmod 000 "$ambient/auth.json"
  seed
  chmod 600 "$ambient/auth.json"
}
c_fifo_auth() {
  fixture
  seed
  rm "$ambient/auth.json"
  mkfifo "$ambient/auth.json"
  seed
}
c_dangling_auth() {
  fixture
  seed
  rm "$ambient/auth.json"
  ln -s "$home/unmounted" "$ambient/auth.json"
  seed
}
c_looping_auth() {
  fixture
  seed
  rm "$ambient/auth.json"
  ln -s "$ambient/auth.json" "$ambient/auth.json"
  seed
}
c_auth_symlink_to_dir() {
  fixture
  seed
  rm "$ambient/auth.json"
  mkdir "$home/adir"
  ln -s "$home/adir" "$ambient/auth.json"
  seed
}
c_auth_symlink_valid() {
  fixture
  mv "$ambient/auth.json" "$home/real-auth.json"
  ln -s "$home/real-auth.json" "$ambient/auth.json"
  seed
}
c_auth_symlink_relative() {
  fixture
  mv "$ambient/auth.json" "$ambient/real-auth.json"
  ln -s real-auth.json "$ambient/auth.json"
  seed
}
c_auth_unsearchable() {
  ((is_root)) && {
    skip=1
    return 0
  }
  fixture
  seed
  rm "$ambient/auth.json"
  mkdir "$home/locked"
  printf '{}\n' >"$home/locked/auth.json"
  ln -s "$home/locked/auth.json" "$ambient/auth.json"
  chmod 000 "$home/locked"
  seed
  chmod 700 "$home/locked"
}
c_dangling_ambient_dir() {
  fixture
  seed
  rm -r "${ambient:?}"
  ln -s "$home/unmounted" "$ambient"
  seed
}
c_newline_component() {
  fixture
  seed
  mkdir -p "$home/.pi/weird"
  ln -s "$home/unmounted" "$home/.pi/weird"$'\n'
  export PI_CODING_AGENT_DIR="$home/.pi/weird"$'\n'"/agent"
  seed
}
c_worker_symlink_to_ambient() {
  fixture
  ln -s "$ambient" "$worker"
  seed
}
c_worker_regular_file() {
  fixture
  printf 'x\n' >"$worker"
  seed
}
c_worker_symlink_to_other_dir() {
  fixture
  mkdir "$home/elsewhere"
  ln -s "$home/elsewhere" "$worker"
  seed
}
c_worker_parent_symlinked() {
  fixture
  mkdir -p "$home/realpi"
  mv "$ambient" "$home/realpi/agent"
  rmdir "$home/.pi"
  ln -s "$home/realpi" "$home/.pi"
  seed
}
c_env_aliases_worker() {
  fixture
  seed
  ln -s "$worker" "$home/alias"
  export PI_CODING_AGENT_DIR="$home/alias"
  seed
}
c_env_equals_worker() {
  fixture
  seed
  export PI_CODING_AGENT_DIR="$worker"
  seed
}
c_env_relative() {
  fixture
  mkdir "$home/rel"
  printf '{"rel":{"type":"api_key","key":"k"}}\n' >"$home/rel/auth.json"
  export PI_CODING_AGENT_DIR=rel
  seed
}
c_env_tilde() {
  fixture
  mkdir -p "$home/custom"
  printf '{"tilde":{"type":"api_key","key":"k"}}\n' >"$home/custom/auth.json"
  # shellcheck disable=SC2088 # the literal ~/ prefix is the input under test
  export PI_CODING_AGENT_DIR='~/custom'
  seed
}
c_env_trailing_slash() {
  fixture
  mkdir -p "$home/custom"
  printf '{"slash":{"type":"api_key","key":"k"}}\n' >"$home/custom/auth.json"
  export PI_CODING_AGENT_DIR="$home/custom/"
  seed
}
c_env_absolute() {
  fixture
  mkdir -p "$home/custom/bin"
  printf '{"abs":{"type":"oauth","access":"a"}}\n' >"$home/custom/auth.json"
  printf '// b\n' >"$home/custom/bin/hookyard-bridge.ts"
  printf '{"models":[]}\n' >"$home/custom/models-store.json"
  export PI_CODING_AGENT_DIR="$home/custom"
  seed
}
c_env_empty() {
  fixture
  export PI_CODING_AGENT_DIR=
  seed
}
c_ambient_inside_worker() {
  fixture
  seed
  mkdir -p "$worker/sub"
  printf '{}\n' >"$worker/sub/auth.json"
  export PI_CODING_AGENT_DIR="$worker/sub"
  seed
}
c_ambient_inside_worker_via_link() {
  fixture
  seed
  mkdir -p "$worker/sub"
  ln -s "$worker/sub" "$home/sublink"
  export PI_CODING_AGENT_DIR="$home/sublink"
  seed
}
c_worker_inside_ambient() {
  fixture
  export PI_CODING_AGENT_DIR="$home/.pi"
  seed
}
c_worker_auth_is_dir() {
  fixture
  seed
  rm "$worker/auth.json"
  mkdir "$worker/auth.json"
  seed
}
c_worker_auth_dirlink() {
  fixture
  seed
  rm "$worker/auth.json"
  mkdir "$home/adir"
  ln -s "$home/adir" "$worker/auth.json"
  seed
}
c_worker_auth_stale_link() {
  fixture
  seed
  rm "$worker/auth.json"
  ln -s "$home/old-target.json" "$worker/auth.json"
  seed
}
c_worker_auth_regular_file() {
  fixture
  seed
  rm "$worker/auth.json"
  printf '{}\n' >"$worker/auth.json"
  seed
}
c_empty_auth_replaced() {
  fixture
  rm "$ambient/auth.json"
  seed
  printf '{"k":{"type":"api_key","key":"x"}}\n' >"$ambient/auth.json"
  seed
}
c_stale_models_store() {
  fixture
  seed
  printf 'torn{\n' >"$worker/models-store.json"
  seed
}
c_models_store_removed() {
  fixture
  seed
  rm "$ambient/models-store.json"
  seed
}
c_no_models_store() {
  fixture
  rm "$ambient/models-store.json"
  seed
}
c_worker_mode_loose() {
  fixture
  seed
  chmod 755 "$worker"
  seed
}
c_stale_modes() {
  fixture
  seed
  chmod 666 "$worker/settings.json" "$worker/models.json"
  chmod 666 "$worker/models-store.json"
  seed
}
c_stale_bridge_copy() {
  fixture
  bridge
  seed
  printf '// edited\n' >"$ambient/bin/hookyard-bridge.ts"
  seed
}
c_bridge_is_dir() {
  fixture
  mkdir -p "$ambient/bin/hookyard-bridge.ts"
  seed
}
c_empty_local_models() {
  fixture
  dispatcher_settings '{"localModels":{}}'
  seed
}

# Idempotence is its own check: a no-op re-seed must touch nothing, not even to
# rewrite identical bytes, so inodes and mtimes are compared first run vs second.
c_idempotent() {
  fixture
  bridge
  dispatcher_settings '{"localModels":{"lemonade/Alpha":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}'
  seed
  listing '\t%i\t%T@' >"$work/$side.idem1"
  seed
  listing '\t%i\t%T@' >"$work/$side.idem2"
}

cases=$(declare -F | sed -n 's/^declare -f c_//p')

ncases=0
seeded=0
refused=0
diffs=0
skipped=0

reset_home() {
  chmod -R u+rwX "${home:?}" 2>/dev/null || true
  rm -rf "${home:?}"
  mkdir -p "$home"
}

run_side() { # $1 = case, $2 = side
  local case_fn="c_$1" s="${2:?}"
  rm -f "${work:?}/$s".out "${work:?}/$s".err "${work:?}/$s".rc "${work:?}/$s".tree
  reset_home
  (
    side=$s
    skip=0
    export HOME="$home" XDG_CONFIG_HOME="$home/.config"
    unset PI_CODING_AGENT_DIR
    "$case_fn"
    if ((skip)); then
      : >"$work/$s.skipped"
    else
      [ -f "$work/$s.rc" ] || {
        echo "case $1 never seeded" >&2
        exit 1
      }
    fi
  )
  if [ -f "$work/$s.skipped" ]; then return 0; fi
  snapshot "$s"
  chmod -R u+rwX "${home:?}" 2>/dev/null || true
}

report() { # $1 = case, $2.. = problems
  local c=$1
  shift
  diffs=$((diffs + 1))
  printf 'DIFF  %s: %s\n' "$c" "$*"
}

for c in $cases; do
  rm -f "${work:?}/old.skipped" "${work:?}/new.skipped"
  run_side "$c" old
  run_side "$c" new
  if [ -f "$work/old.skipped" ]; then
    skipped=$((skipped + 1))
    printf 'skip  %s (running as root)\n' "$c"
    continue
  fi
  ncases=$((ncases + 1))
  if [ "$(cat "$work/old.rc")" -eq 0 ]; then seeded=$((seeded + 1)); else refused=$((refused + 1)); fi

  bad=()
  cmp -s "$work/old.out" "$work/new.out" || bad+=(stdout)
  cmp -s "$work/old.err" "$work/new.err" || bad+=(stderr)
  cmp -s "$work/old.tree" "$work/new.tree" || bad+=(tree)
  # A bash-internal error (not a crew refusal) names its line; neither side
  # should ever produce one for these inputs.
  for s in old new; do
    if grep -Eq 'line [0-9]+:' "$work/$s.err"; then bad+=("$s stderr has a bash 'line N:' error"); fi
    if grep -q '\.seed\.' "$work/$s.tree"; then bad+=("$s left a .seed.* file"); fi
  done
  if ((${#bad[@]})); then
    report "$c" "${bad[*]}"
    cmp -s "$work/old.out" "$work/new.out" || diff -u "$work/old.out" "$work/new.out" | head -20 || true
    cmp -s "$work/old.err" "$work/new.err" || diff -u "$work/old.err" "$work/new.err" | head -20 || true
    cmp -s "$work/old.tree" "$work/new.tree" || diff -u "$work/old.tree" "$work/new.tree" | head -40 || true
  fi

  if [ "$c" = idempotent ]; then
    for s in old new; do
      if ! cmp -s "$work/$s.idem1" "$work/$s.idem2"; then
        report "idempotent-unchanged ($s)" "second seed changed inodes or mtimes"
        diff -u "$work/$s.idem1" "$work/$s.idem2" | head -20 || true
      fi
    done
  fi
done

printf '\n%d cases (%d seeded, %d refused), %d diffs' "$ncases" "$seeded" "$refused" "$diffs"
((skipped == 0)) || printf ' (%d skipped)' "$skipped"
printf '\n'
if ((seeded == 0)); then
  echo "no case seeded successfully — the comparison covered refusals only" >&2
  exit 1
fi
((diffs == 0))
