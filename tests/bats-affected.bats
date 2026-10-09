# scripts/bats-affected.sh: map changed files to the bats files worth running.

setup() {
  load helpers
  bats_require_minimum_version 1.5.0
  REPO_ROOT="$(cd "$BATS_TEST_DIRNAME/.." && pwd)"
  AFFECTED="$REPO_ROOT/scripts/bats-affected.sh"
  export GIT_CONFIG_GLOBAL=/dev/null
  setup_repo
}

teardown() {
  teardown_repo
}

affected() { run --separate-stderr bash "$AFFECTED" "$@"; }

suite_count() { find "$REPO_ROOT/tests" -maxdepth 1 -name '*.bats' | wc -l; }

@test "affected: a source maps to exactly its row" {
  affected --files adapters/core/secret-read-guard.sh
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf '%s\n' tests/adapters.bats tests/dispatch.bats tests/secret-read-guard.bats)" ]
  [ -z "$stderr" ]
}

@test "affected: crew.sh selects its 20 files and not unrelated suites" {
  affected --files adapters/core/crew.sh
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 20 ]
  [[ "$output" == *tests/crew.bats* ]]
  [[ "$output" != *tests/secret-read-guard.bats* ]]
}

@test "affected: crew/ Go sources select crew.sh's 20 files" {
  affected --files crew/main.go
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 20 ]
  [[ "$output" == *tests/crew.bats* ]]
  [[ "$output" != *tests/secret-read-guard.bats* ]]
}

@test "affected: tests/helpers.bash selects the full suite and says why on stderr" {
  affected --files tests/helpers.bash
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq "$(suite_count)" ]
  [[ "$output" == *tests/module.bats* ]]
  [[ "$stderr" == *"full suite"* ]]
}

@test "affected: worktree-git.sh selects the full suite" {
  affected --files adapters/core/worktree-git.sh
  [ "${#lines[@]}" -eq "$(suite_count)" ]
  [[ "$stderr" == *"full suite"* ]]
}

@test "affected: a fixture under tests/ selects the full suite" {
  affected --files tests/fixtures/anything.json
  [ "${#lines[@]}" -eq "$(suite_count)" ]
}

@test "affected: an unmapped path falls back to the full suite" {
  affected --files newdir/thing.go
  [ "${#lines[@]}" -eq "$(suite_count)" ]
  [[ "$stderr" == *"no map row"* ]]
}

@test "affected: a changed bats file selects only itself" {
  affected --files tests/crew-id.bats
  [ "$output" = "tests/crew-id.bats" ]
  [ -z "$stderr" ]
}

@test "affected: a deleted bats file contributes nothing" {
  affected --files tests/no-such-file.bats
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "affected: docs-only changes select nothing" {
  affected --files docs/test-suite-audit.md LICENSE spikes/pi-bus/bus.ts
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [ -z "$stderr" ]
}

@test "affected: measurement tools select nothing and the script selects its own test" {
  affected --files scripts/bats-timing.sh scripts/bats-classify.sh
  [ -z "$output" ]
  affected --files scripts/bats-affected.sh
  [ "$output" = "tests/bats-affected.bats" ]
}

@test "affected: no changed files prints nothing" {
  affected --files
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "affected: several files select the sorted unique union" {
  affected --files adapters/core/secret-read-guard.sh adapters/core/pr-watch.sh tests/adapters.bats
  [ "$output" = "$(printf '%s\n' tests/adapters.bats tests/dispatch.bats tests/module.bats tests/pr-watch.bats tests/secret-read-guard.bats)" ]
}

@test "affected: every tracked core script, script and reviewer script has a map row" {
  local f missing=()
  while IFS= read -r f; do
    affected --files "$f"
    [[ "$stderr" != *"no map row"* ]] || missing+=("$f")
  done < <(git -C "$REPO_ROOT" ls-files 'adapters/core/*.sh' 'scripts/*.sh' 'adapters/core/reviewers/*.sh')
  [ "${#missing[@]}" -eq 0 ] || {
    echo "add a row to scripts/bats-affected.sh for: ${missing[*]}" >&2
    return 1
  }
}

@test "affected: a brand-new core script is unmapped, so it runs the full suite" {
  affected --files adapters/core/brand-new.sh
  [ "${#lines[@]}" -eq "$(suite_count)" ]
  [[ "$stderr" == *"no map row"* ]]
}

# git mode: the script derives its root from its own location, so run a copy
# inside a throwaway repository.
make_git_fixture() {
  mkdir -p scripts tests adapters/core
  cp "$AFFECTED" scripts/bats-affected.sh
  touch tests/adapters.bats tests/dispatch.bats tests/secret-read-guard.bats tests/module.bats
  : >adapters/core/secret-read-guard.sh
  git init -q -b main .
  git add -A
  git -c user.name=t -c user.email=t@t commit -q -m base
  git checkout -q -b topic
}

@test "affected git: a committed change since the merge-base is listed" {
  make_git_fixture
  echo x >adapters/core/secret-read-guard.sh
  git add -A
  git -c user.name=t -c user.email=t@t commit -q -m change
  run --separate-stderr bash scripts/bats-affected.sh --base main
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf '%s\n' tests/adapters.bats tests/dispatch.bats tests/secret-read-guard.bats)" ]
}

@test "affected git: unstaged, staged and untracked changes are all seen" {
  make_git_fixture
  echo x >>adapters/core/secret-read-guard.sh
  echo y >scratch.txt
  git add scratch.txt
  echo "# new" >tests/module.bats
  : >untracked-thing.go
  run --separate-stderr bash scripts/bats-affected.sh --base main
  [[ "$output" == *tests/secret-read-guard.bats* ]]
  [[ "$output" == *tests/module.bats* ]]
  # scratch.txt (staged) and untracked-thing.go have no row: full suite
  [ "${#lines[@]}" -eq 4 ]
  [[ "$stderr" == *"no map row"* ]]
}

@test "affected git: an untracked mapped file is seen" {
  make_git_fixture
  mkdir -p docs
  echo hi >docs/notes.md
  : >adapters/core/pr-watch.sh
  touch tests/pr-watch.bats
  run --separate-stderr bash scripts/bats-affected.sh --base main
  [ "$output" = "$(printf '%s\n' tests/adapters.bats tests/dispatch.bats tests/module.bats tests/pr-watch.bats)" ]
}

@test "affected git: a clean branch prints nothing" {
  make_git_fixture
  run --separate-stderr bash scripts/bats-affected.sh --base main
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "affected git: an unresolvable base falls back to the full suite" {
  make_git_fixture
  run --separate-stderr bash scripts/bats-affected.sh --base no-such-ref
  [ "$status" -eq 0 ]
  [ "${#lines[@]}" -eq 4 ]
  [[ "$stderr" == *"full suite"* ]]
}

@test "affected git: a committed rename out of a mapped path still selects the old path's row" {
  make_git_fixture
  mkdir -p docs
  git mv adapters/core/secret-read-guard.sh docs/secret-read-guard.md
  git -c user.name=t -c user.email=t@t commit -q -m rename
  run --separate-stderr bash scripts/bats-affected.sh --base main
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf '%s\n' tests/adapters.bats tests/dispatch.bats tests/secret-read-guard.bats)" ]
}

@test "affected: a missing last test file is skipped and the exit stays 0" {
  make_git_fixture
  rm tests/secret-read-guard.bats
  run --separate-stderr bash scripts/bats-affected.sh --files adapters/core/secret-read-guard.sh
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf '%s\n' tests/adapters.bats tests/dispatch.bats)" ]
}
