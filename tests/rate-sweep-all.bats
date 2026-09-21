bats_require_minimum_version 1.5.0

setup() {
  load helpers
  CREW="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_crew() { bash -euo pipefail "$CREW" "$@"; }
  # The host repo the sweep is launched from: no bus, no origin. --sweep-all
  # must not care which repo (if any) it runs in.
  setup_repo
  export HOME="$BATS_TEST_TMPDIR/home"
  export XDG_DATA_HOME="$BATS_TEST_TMPDIR/data"
  ROOT="$BATS_TEST_TMPDIR/root"
  mkdir -p "$HOME" "$ROOT"
  stub_gh
  unset CREW_ID CREW_SWEEP_ROOTS
}

teardown() {
  teardown_repo
}

# No test may make a real network call — gh is stubbed and answers every
# reconcile query with an open PR.
stub_gh() {
  stub_bin gh
  cat >"$STUB_DIR/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
pr) echo '{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}' ;;
api)
  for a in "$@"; do
    [ "$a" = graphql ] && {
      echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}'
      exit 0
    }
  done
  echo '{"workflow_runs":[]}'
  ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/gh"
}

# mk_repo <dir> [<owner/repo>] — a git repo with a one-run bus whose PR url
# targets its own slug (the reconcile gate spends gh credentials on the
# dispatching repo only). Without a slug the repo has no origin remote.
mk_repo() {
  local dir="$1" slug="${2:-}"
  mkdir -p "$dir"
  git init -q -b main "$dir"
  [ -z "$slug" ] || git -C "$dir" remote add origin "https://github.com/$slug.git"
  mkdir -p "$dir/.git/crew"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"feat/x", engine:"claude", model:"sonnet", tier:"standard", effort:"medium", title:"t"}' \
    >>"$dir/.git/crew/events.jsonl"
  jq -nc --arg pr "https://github.com/${slug:-acme/none}/pull/1" \
    '{ts:1500, crew_id:"c1", from:"worker:feat/x", to:"dispatcher:c1", kind:"status", body:{state:"pr_open", pr_url:$pr}}' \
    >>"$dir/.git/crew/events.jsonl"
}

store_repos() { jq -s -r 'map(.repo) | unique | join(",")' "$XDG_DATA_HOME/crew/ratings.jsonl"; }

@test "sweep-all: sweeps every repo under --root, nested ones included, into the shared store" {
  mk_repo "$ROOT/a" acme/alpha
  mk_repo "$ROOT/b" acme/beta
  mk_repo "$ROOT/group/c" acme/gamma
  run run_crew rate --sweep-all --root "$ROOT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"acme/alpha: swept, 1 runs in store"* ]]
  [[ "$output" == *"acme/beta: swept, 1 runs in store"* ]]
  [[ "$output" == *"acme/gamma: swept, 1 runs in store"* ]]
  [ "$(store_repos)" = "acme/alpha,acme/beta,acme/gamma" ]
}

@test "sweep-all: a repo with no origin and a dir with no bus are skipped, not failed" {
  mk_repo "$ROOT/a" acme/alpha
  mk_repo "$ROOT/noremote"
  mkdir -p "$ROOT/nobus/.git"
  run run_crew rate --sweep-all --root "$ROOT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"noremote: skipped (no origin remote)"* ]]
  [[ "$output" != *"nobus"* ]]
  [ "$(store_repos)" = "acme/alpha" ]
}

@test "sweep-all: a second run leaves the store byte-identical" {
  mk_repo "$ROOT/a" acme/alpha
  mk_repo "$ROOT/b" acme/beta
  run run_crew rate --sweep-all --root "$ROOT"
  [ "$status" -eq 0 ]
  cp "$XDG_DATA_HOME/crew/ratings.jsonl" "$BATS_TEST_TMPDIR/before.jsonl"
  run run_crew rate --sweep-all --root "$ROOT"
  [ "$status" -eq 0 ]
  cmp "$BATS_TEST_TMPDIR/before.jsonl" "$XDG_DATA_HOME/crew/ratings.jsonl"
}

@test "sweep-all: a repo outside --root is found through the registry once it has been swept" {
  mk_repo "$BATS_TEST_TMPDIR/elsewhere" acme/outside
  (cd "$BATS_TEST_TMPDIR/elsewhere" && run_crew rate)
  grep -qxF "$BATS_TEST_TMPDIR/elsewhere/.git" "$XDG_DATA_HOME/crew/repos"
  rm -f "$XDG_DATA_HOME/crew/ratings.jsonl"
  mkdir -p "$ROOT/empty"
  run run_crew rate --sweep-all --root "$ROOT/empty"
  [ "$status" -eq 0 ]
  [[ "$output" == *"acme/outside: swept, 1 runs in store"* ]]
}

@test "sweep-all: a registered repo that no longer exists is skipped as gone" {
  mkdir -p "$XDG_DATA_HOME/crew"
  printf '%s\n' "$BATS_TEST_TMPDIR/vanished/.git" >"$XDG_DATA_HOME/crew/repos"
  run run_crew rate --sweep-all --root "$ROOT"
  [ "$status" -eq 0 ]
  [[ "$output" == *"vanished/.git: skipped (gone)"* ]]
}

@test "sweep-all: a failing repo is reported, the rest still sweep, and the exit is 1" {
  mk_repo "$ROOT/a" acme/alpha
  mk_repo "$ROOT/bad" acme/broken
  printf 'not json\n' >"$ROOT/bad/.git/crew/events.jsonl"
  run run_crew rate --sweep-all --root "$ROOT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"acme/broken: failed (rc="* ]]
  [[ "$output" == *"acme/alpha: swept, 1 runs in store"* ]]
  [ "$(store_repos)" = "acme/alpha" ]
}

@test "sweep-all: no repos found is a clean exit 0" {
  run run_crew rate --sweep-all --root "$ROOT"
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "sweep-all: conflicting or malformed flags are errors" {
  run run_crew rate --sweep-all --report
  [ "$status" -eq 1 ]
  [[ "$output" == *"rate takes --report, --json, --pooled, and --sweep-all/--root"* ]]
  run run_crew rate --sweep-all --json
  [ "$status" -eq 1 ]
  run run_crew rate --sweep-all --pooled
  [ "$status" -eq 1 ]
  run run_crew rate --root "$ROOT"
  [ "$status" -eq 1 ]
  [[ "$output" == *"rate --root needs --sweep-all"* ]]
  run run_crew rate --sweep-all --root
  [ "$status" -eq 1 ]
  [[ "$output" == *"rate --root needs a directory"* ]]
}
