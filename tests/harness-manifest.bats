bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# scripts/harness-manifest.sh --check compares rows by identity — (case_id,
# source_file, test_name, family, tags) — and ignores `source_line`, so an edit
# that only moves a @test passes while one added, removed or renamed fails.
#
# Every case runs against a fixture repo under $BATS_TEST_TMPDIR; the real
# manifest is never read or written here.

manifest() { run --separate-stderr bash "$REPO/scripts/harness-manifest.sh" "$@"; }

setup() {
  bats_require_minimum_version 1.5.0
  REPO="$BATS_TEST_TMPDIR/repo"
  mkdir -p "$REPO/scripts" "$REPO/tests/harness"
  cp "$BATS_TEST_DIRNAME/../scripts/harness-manifest.sh" "$REPO/scripts/harness-manifest.sh"

  printf '#!/usr/bin/env bats\n\n@test "alpha one" {\n  true\n}\n' >"$REPO/tests/crew-id.bats"
  printf '#!/usr/bin/env bats\n\n@test "beta one" {\n  true\n}\n' >"$REPO/tests/pr-watch.bats"
  : >"$REPO/tests/refresh-models.bats"
  : >"$REPO/tests/dispatch.bats"

  bash "$REPO/scripts/harness-manifest.sh"
}

@test "--check passes on a freshly written manifest" {
  manifest --check
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "--check ignores a line shift" {
  { printf '\n\n'; cat "$REPO/tests/pr-watch.bats"; } >"$REPO/new" && mv "$REPO/new" "$REPO/tests/pr-watch.bats"

  manifest --check
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "--check fails naming the row when a sampled @test is renamed" {
  sed -i 's/@test "beta one"/@test "beta two"/' "$REPO/tests/pr-watch.bats"

  manifest --check
  [ "$status" -eq 1 ]
  [[ $output == *'-pr-watch-beta-one'* ]]
  [[ $output == *'+pr-watch-beta-two'* ]]
  [[ $stderr == *"is stale"* ]]
}

@test "--check fails when a sampled @test is added or removed" {
  printf '\n@test "gamma" {\n  true\n}\n' >>"$REPO/tests/pr-watch.bats"
  manifest --check
  [ "$status" -eq 1 ]
  [[ $output == *'+pr-watch-gamma'* ]]

  printf '#!/usr/bin/env bats\n' >"$REPO/tests/crew-id.bats"
  manifest --check
  [ "$status" -eq 1 ]
}
