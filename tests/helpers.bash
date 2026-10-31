# Shared bats helpers.

# `load helpers` runs once per test setup, so this is a fresh suite-local store
# even for tests that do not need a throwaway repository.
export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"

# Real `crew reap` invocations across the suite would each fork a ratings
# sweep; the fork would outlive the test and race teardown_repo's rm -rf.
# Tests that exercise the sweep opt in explicitly.
export CREW_RATE_AUTOSWEEP=0

# The crew under test — exported, as the two above it are, because it is what
# every bats file reads. CREW is the entrypoint the suite built (tests/
# setup_suite.bash compiles it), so run it directly: `bash "$CREW"` would hand a
# binary to bash and get 126.
export CREW="${CREW_GO_BIN:?tests/setup_suite.bash did not build crew-go}"

# Host-environment isolation: unset vars that leak from a running dispatcher or
# tmux session when bats itself runs from inside one.  Plain `bats tests/`
# must not inherit DISPATCH_ENGINES, the host tmux server, or other harness
# state that individual setup() functions clear piecemeal.
unset DISPATCH_ENGINES TMUX TMUX_PANE
unset CREW_ID CREW_WORKER_ID CREW_ROLE_ID
unset DISPATCHER_CRITICS_DIR DISPATCHER_REVIEWERS_DIR DISPATCHER_SKILLS_DIR
# The repo copy of the pinned pi subagent extension and agent roster pi leads load.
export DISPATCHER_PI_DIR="$BATS_TEST_DIRNAME/../adapters/pi"
unset DISPATCH_PROFILE DISPATCH_SKIP_MODEL_CHECK DISPATCH_IGNORE_RUNG DISPATCH_GRANT_ROOTS
unset DISPATCH_SPEC DISPATCH_SHAPE DISPATCH_DRAFT_PR DISPATCH_CLAUDE_CONNECTORS DISPATCH_CLASS_SIGIL
# Raw-source consumers carry the unsubstituted @dispatchConfig@; point them at
# the repo copy of the resolver (every test file lives in tests/).
export DISPATCH_CONFIG_BIN="$BATS_TEST_DIRNAME/../adapters/core/dispatch-config.sh"
# Same idiom, for the shared --add-dir grant validator lib.
export GRANT_CHECK_LIB="$BATS_TEST_DIRNAME/../adapters/core/grant-check.sh"
# And for the anchored-git lib (#539).
export WORKTREE_GIT_LIB="$BATS_TEST_DIRNAME/../adapters/core/worktree-git.sh"
# And for the shared localModels lane lib.
export LOCAL_MODELS_LIB="$BATS_TEST_DIRNAME/../adapters/core/local-models.sh"
# And for the shared OpenRouter key resolver lib.
export OPENROUTER_KEY_LIB="$BATS_TEST_DIRNAME/../adapters/core/openrouter-key.sh"
# And for the shared quota-predicate lib.
export BUDGET_GATE_LIB="$BATS_TEST_DIRNAME/../adapters/core/budget-gate.sh"
# And for the model-class resolver dispatch's tier gate runs.
export MODEL_RESOLVE_BIN="$BATS_TEST_DIRNAME/../adapters/core/model-resolve.sh"
# The host's ~/.config/dispatcher/settings.json must never reach a test.
export XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/config"
# Generated launch files (the mcpServers catalog renders) must land neither in
# the developer's cache nor in a test repo that a test points HOME at.
export XDG_CACHE_HOME="$BATS_TEST_TMPDIR/cache"
# A host's locked settings would override what each test sets up.
unset DISPATCH_LOCKED_SETTINGS
# Isolate tmux: point the socket dir at a private per-test directory so no
# test ever accidentally talks to the host tmux server.  Tests that need a
# real tmux server start one with `-L <sock>` under this dir and are
# unaffected.
export TMUX_TMPDIR="$BATS_TEST_TMPDIR/tmux"
mkdir -p "$TMUX_TMPDIR"

# setup_repo — a throwaway git repo as $TEST_REPO, cwd set to it.
setup_repo() {
  TEST_REPO="$(mktemp -d)"
  cd "$TEST_REPO" || return 1
  # The real tmux, captured before setup() prepends STUB_DIR to PATH: the
  # tmux-backed tests drive a private server through this while the stub (or a
  # dispatch shim) intercepts the code-under-test's own calls.
  REAL_TMUX="${REAL_TMUX:-$(command -v tmux || true)}"
  export REAL_TMUX
  # Every test gets a private data store; never let crew state leak in from
  # the shell running bats.
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  assert_isolated_xdg_data_home || return 1
  # Overriding $HOME is not enough: $XDG_CONFIG_HOME survives it, so git still
  # reads ~/.config/git/config, whose commit.gpgSign and tilde-relative
  # signingkey then re-expand against the new HOME and kill every commit.
  export GIT_CONFIG_GLOBAL=/dev/null
  git init -q -b main .
  git config user.email test@example.com
  git config user.name test
  export TEST_REPO
  # dispatch's fleet gate refuses on a loaded host; pin an idle one so no test
  # depends on the machine it runs on.
  printf '0.00 0.00 0.00 1/1 1\n' >"$BATS_TEST_TMPDIR/loadavg"
  export DISPATCH_LOADAVG_FILE="$BATS_TEST_TMPDIR/loadavg"
  # Every engine CLI stubbed by default: dispatch's on-PATH probe
  # (check_engine) must not fail a test merely because this environment (CI)
  # has no real engine CLIs installed. A test that needs a genuinely missing
  # binary removes its stub afterward (`rm "$STUB_DIR/<cli>"` +
  # path_without_real), so the probe's own negative tests stay honest.
  stub_bin claude
  stub_bin codex
  stub_bin cursor-agent
  stub_bin pi
}

assert_isolated_xdg_data_home() {
  case "${XDG_DATA_HOME:-}" in
  "$BATS_TEST_TMPDIR"/*) return 0 ;;
  esac
  printf 'XDG_DATA_HOME must be under BATS_TEST_TMPDIR (got %s)\n' \
    "${XDG_DATA_HOME:-<unset>}" >&2
  return 1
}

teardown_repo() {
  [ -n "${TEST_REPO:-}" ] && rm -rf "$TEST_REPO"
}

# seed_git_baseline [repo] — (re-)record the #557 git-config baseline from the
# current config, the way a dispatch's entry guard does; `crew git-baseline`
# itself never creates it.
seed_git_baseline() {
  local common
  common="$(git -C "${1:-$TEST_REPO}" rev-parse --path-format=absolute --git-common-dir)"
  rm -f "$common/crew/git-config-baseline"
  # an armed repo refuses to re-record a deleted baseline
  rm -rf "$XDG_DATA_HOME/crew/git-baseline-armed"
  # shellcheck source=/dev/null
  (. "$WORKTREE_GIT_LIB" && _wt_cfg_baseline_init "$common") 2>/dev/null
}

# plant_commondir [repo] — plant a `commondir` file in <repo>'s main git dir
# (#993) pointing at a self-consistent bare repo: an exec key in its config,
# recorded in its own baseline, so only the commondir pin can refuse it.
# Sets PLANTED_COMMONDIR to the planted file, SENTINEL to what the exec key
# touches. Run it after any git call that must see the real repo.
plant_commondir() {
  local repo fake
  repo="$(realpath "${1:-$TEST_REPO}")"
  fake="$BATS_TEST_TMPDIR/evil-common"
  SENTINEL="$BATS_TEST_TMPDIR/SENTINEL"
  printf '#!/bin/sh\ntouch %q\ncat\n' "$SENTINEL" >"$BATS_TEST_TMPDIR/evil-hit.sh"
  chmod +x "$BATS_TEST_TMPDIR/evil-hit.sh"
  git init -q --bare "$fake"
  git --git-dir="$fake" config core.fsmonitor "$BATS_TEST_TMPDIR/evil-hit.sh"
  mkdir -p "$fake/crew"
  printf '%s\0' $'#covers\nredirect' $'#covers\nlegacy' $'core.fsmonitor\n'"$BATS_TEST_TMPDIR/evil-hit.sh" \
    >"$fake/crew/git-config-baseline"
  printf '%s\n' "$fake" >"$repo/.git/commondir"
  export PLANTED_COMMONDIR="$repo/.git/commondir" SENTINEL
}

# marks_path <dir> <crew> <id> — the delivered-marks path of #290: <dir>/await/
# <key with every [^A-Za-z0-9._-] byte mapped to _>.<POSIX cksum of the raw key>,
# for key "<crew>-<id>". crew/internal/marks.Path writes it, and marks.TestPath
# plus TestPathMatchesShell pin the formula against real tr/cksum, so a test that
# only needs to find the file computes it here.
marks_path() {
  local key="$2-$3"
  printf '%s/await/%s.%s' "$1" \
    "$(printf '%s' "$key" | tr -c 'A-Za-z0-9._-' '_')" \
    "$(printf '%s' "$key" | cksum | cut -d' ' -f1)"
}

# launch_log — $STUB_LOG with every `bash '<crew>/launch/{launch,exit}.XXXXXX'`
# token replaced by the command that script execs, one line per send-keys
# call. Panes are typed only the short script path (#298), so launch content
# (engine flags, prompt text, env prefixes) is asserted through this.
launch_log() {
  local line before rest path after content
  while IFS= read -r line; do
    while [[ $line == *"bash '"* ]]; do
      before="${line%%bash \'*}"
      rest="${line#*bash \'}"
      path="${rest%%\'*}"
      after="${rest#*\'}"
      content="$(sed -n 's/^exec env //p' "$path")"
      line="${before}${content}${after}"
    done
    printf '%s\n' "$line"
  done <"$STUB_LOG"
}

# assert_add_dir_terminated <launch line> — the line carries --add-dir, and the
# token after the last one's value is an option. claude's --add-dir is
# variadic: anything else there (the positional prompt) would be swallowed as
# one more directory. When the line also carries --disallowedTools (variadic
# too), its value run must be non-empty and likewise option-terminated.
assert_add_dir_terminated() {
  local words i last=-1 k=-1 j
  read -r -a words <<<"$1"
  for i in "${!words[@]}"; do
    if [ "${words[$i]}" = --add-dir ]; then
      last=$i
    fi
  done
  [ "$last" -ge 0 ]
  [[ ${words[$((last + 2))]:-} == --* ]]
  for i in "${!words[@]}"; do
    if [ "${words[$i]}" = --disallowedTools ]; then
      k=$i
    fi
  done
  if [ "$k" -ge 0 ]; then
    j=$((k + 1))
    while [[ ${words[$j]:-} != --* && -n ${words[$j]:-} ]]; do
      j=$((j + 1))
    done
    [ "$j" -gt $((k + 1)) ]
    [[ ${words[$j]:-} == --* ]]
  fi
}

# stub_bin <name> — put a logging stub for <name> first on PATH.
# The stub appends its argv (NUL-free, one invocation per line) to
# $STUB_LOG and exits 0.
stub_bin() {
  STUB_DIR="${STUB_DIR:-$(mktemp -d)}"
  STUB_LOG="${STUB_LOG:-$STUB_DIR/calls.log}"
  cat >"$STUB_DIR/$1" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
exit 0
EOF
  chmod +x "$STUB_DIR/$1"
  export STUB_DIR STUB_LOG
  export PATH="$STUB_DIR:$PATH"
}

# path_without_real <cli> — $PATH with $STUB_DIR kept and every OTHER
# directory that would resolve a real <cli> dropped. `rm "$STUB_DIR/<cli>"`
# alone only uncovers whatever the developer's machine has installed further
# down PATH (this box has real codex/cursor/cursor-agent CLIs on it, since a
# maintainer of this repo daily-drives all three); the probe tests below need
# the removed engine to be genuinely unresolvable, not just absent from the
# stub dir, while leaving every other directory (and the tools before the
# gate need, e.g. bash and grep) intact.
path_without_real() {
  local cli="$1" dir kept=()
  local IFS=:
  # shellcheck disable=SC2206 # word-splitting on IFS=: is the point here
  local dirs=($PATH)
  for dir in "${dirs[@]}"; do
    if [ "$dir" != "$STUB_DIR" ] && [ -x "$dir/$cli" ]; then
      continue
    fi
    kept+=("$dir")
  done
  IFS=:
  printf '%s' "${kept[*]}"
}

# The runtime hash rule, mirrored from _check_protocol_rev (and flake.nix):
# the directory's files (dotfiles included), names sorted byte-wise, each
# hashed as `name:sha256;`, sha256 of the concatenation, first 16 hex.
_protocol_dir_rev() { # <dir>
  local dir="$1" entries="" names=() file
  shopt -s dotglob nullglob
  for file in "$dir"/*; do
    [ -f "$file" ] || continue
    names+=("$(basename "$file")")
  done
  shopt -u dotglob nullglob
  if [ ${#names[@]} -gt 0 ]; then
    mapfile -t names < <(printf '%s\n' "${names[@]}" | LC_ALL=C sort)
  fi
  for name in "${names[@]}"; do
    entries+="${name}:$(sha256sum "$dir/$name" | cut -d' ' -f1);"$'\n'
  done
  printf '%s' "$entries" | tr -d '\n' | sha256sum | cut -d' ' -f1 | cut -c1-16
}

# touch_worker_protocols <dir> — the core worker protocol and every phase file,
# as the source and the claude and pi renders: all a lead launch of any engine
# requires from the worker protocol set.
touch_worker_protocols() {
  local dir="$1" stem
  mkdir -p "$dir"
  for stem in WORKER_PROTOCOL WORKER_GRID WORKER_REBASE WORKER_PLAN WORKER_EXECUTE WORKER_REVIEW WORKER_SHIP; do
    touch "$dir/$stem.md" "$dir/$stem.claude.md" "$dir/$stem.pi.md"
  done
}

# Valid standard-tier launch tuples for the protocol-dir coverage tests in
# dispatch.bats and dispatch-resume.bats. One line per engine:
#   agent|model|effort|profile|bin|marker
# `profile` empty means personal. `bin`/`marker` drive _replay_lead_launch in
# the fresh happy-path test and are ignored by the resume tests. With no
# arguments all engines are emitted; passing agent names filters to those
# (fresh dispatch cannot use --no-grid with pi, so it asks for codex cursor).
# Codex first on purpose: a broken-guard sensitivity run then goes red on a
# newly-covered engine rather than an already-covered one.
protocol_engine_specs() { # [agent...]
  local line agent
  while IFS= read -r line; do
    agent="${line%%|*}"
    if [ $# -eq 0 ]; then
      printf '%s\n' "$line"
    else
      local want
      for want in "$@"; do
        [ "$want" = "$agent" ] && printf '%s\n' "$line"
      done
    fi
  done <<'EOF'
codex|gpt-5.6-terra|medium|work|codex|codex --profile worker
cursor|cursor-grok-4.6-medium|medium|work|cursor-agent|cursor-agent --force
pi|openrouter/deepseek/deepseek-v4.1-flash|high||pi|pi --name iris --model
EOF
}

# mint_spec — export a DISPATCH_SPEC with the summary a mint posts as the new
# issue's body.
mint_spec() {
  DISPATCH_SPEC="$BATS_TEST_TMPDIR/mint-spec.md"
  printf 'Summary for the minted issue.\n\n## Task\n\nDo the thing.\n' >"$DISPATCH_SPEC"
  export DISPATCH_SPEC
}

# shellcheck source=/dev/null
source "$(dirname "${BASH_SOURCE[0]}")/coverage.bash"
