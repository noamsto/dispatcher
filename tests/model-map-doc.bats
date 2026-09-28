#!/usr/bin/env bats
# The generated doc regions in dispatch-orchestration.md (#560): the tier-rows
# and pace-downgrades tables are rendered from adapters/core/defaults.json, and
# `--check` also asserts the hand-written Model map table agrees with it.

setup() {
  load helpers
  GEN="$BATS_TEST_DIRNAME/../scripts/gen-model-map-doc.sh"
  DEFAULTS="$BATS_TEST_DIRNAME/../adapters/core/defaults.json"
  DOC="$BATS_TEST_DIRNAME/../adapters/core/protocols/dispatch-orchestration.md"
}

teardown() {
  [ -z "${TEST_REPO:-}" ] || teardown_repo
}

@test "doc check passes on the repo" {
  run bash "$GEN" --check
  [ "$status" -eq 0 ]
}

@test "doc check fails on a stale generated row" {
  tmpdoc="$BATS_TEST_TMPDIR/stale.md"
  cp "$DOC" "$tmpdoc"
  sed -i 's/openrouter\/deepseek\/deepseek-v4\.1-flash/openrouter\/deepseek\/deepseek-v9-flash/' "$tmpdoc"
  run bash "$GEN" --check "$DEFAULTS" "$tmpdoc"
  [ "$status" -eq 1 ]
}

@test "doc check fails on a worker cell that disagrees with default" {
  tmpdoc="$BATS_TEST_TMPDIR/mismatch.md"
  cp "$DOC" "$tmpdoc"
  sed -i '0,/\*\*opus\*\* → \*\*sonnet\*\*/s//\*\*sonnet\*\* → \*\*sonnet\*\*/' "$tmpdoc"
  run bash "$GEN" --check "$DEFAULTS" "$tmpdoc"
  [ "$status" -eq 1 ]
}

@test "doc check fails when the tier-rows markers are missing" {
  tmpdoc="$BATS_TEST_TMPDIR/nomarkers.md"
  cp "$DOC" "$tmpdoc"
  sed -i '/BEGIN generated:tier-rows\|END generated:tier-rows/d' "$tmpdoc"
  run bash "$GEN" --check "$DEFAULTS" "$tmpdoc"
  [ "$status" -eq 1 ]
}

@test "doc check fails when a row's default is not in its models" {
  tmpdefaults="$BATS_TEST_TMPDIR/bad-default.json"
  jq '.modelMap.pi.deep.default = "not-a-listed-model"' "$DEFAULTS" >"$tmpdefaults"
  run bash "$GEN" --check "$tmpdefaults" "$DOC"
  [ "$status" -eq 1 ]
  [[ "$output" == *"not-a-listed-model"* ]]
}

@test "write mode leaves no stray temp file when defaults.json is broken" {
  tmpdefaults="$BATS_TEST_TMPDIR/broken.json"
  printf 'not json' >"$tmpdefaults"
  tmpdoc="$BATS_TEST_TMPDIR/writeme.md"
  cp "$DOC" "$tmpdoc"
  run bash "$GEN" "$tmpdefaults" "$tmpdoc"
  [ "$status" -ne 0 ]
  run bash -c "ls \"$BATS_TEST_TMPDIR\"/writeme.md.*"
  [ "$status" -ne 0 ]
}

@test "doc check fails on a stale row hidden by a BEGIN marker with trailing whitespace" {
  tmpdoc="$BATS_TEST_TMPDIR/trailing-begin.md"
  cp "$DOC" "$tmpdoc"
  sed -i 's|<!-- BEGIN generated:tier-rows from adapters/core/defaults.json by scripts/gen-model-map-doc.sh -->|<!-- BEGIN generated:tier-rows from adapters/core/defaults.json by scripts/gen-model-map-doc.sh --> |' "$tmpdoc"
  sed -i '0,/`gpt-5\.4-mini` |/s//`gpt-5.4-nano` |/' "$tmpdoc"
  run bash "$GEN" --check "$DEFAULTS" "$tmpdoc"
  [ "$status" -ne 0 ]
}

@test "write mode refuses to truncate the doc when an END marker has trailing whitespace" {
  tmpdoc="$BATS_TEST_TMPDIR/trailing-end.md"
  cp "$DOC" "$tmpdoc"
  lines_before="$(wc -l <"$tmpdoc")"
  sed -i 's|<!-- END generated:pace-downgrades -->|<!-- END generated:pace-downgrades --> |' "$tmpdoc"
  run bash "$GEN" "$DEFAULTS" "$tmpdoc"
  [ "$status" -ne 0 ]
  [ "$(wc -l <"$tmpdoc")" -eq "$lines_before" ]
}

@test "a failed check leaves no temp files behind in TMPDIR" {
  export TMPDIR="$BATS_TEST_TMPDIR/emptytmp"
  mkdir -p "$TMPDIR"
  tmpdoc="$BATS_TEST_TMPDIR/nomarkers-tmpdir.md"
  cp "$DOC" "$tmpdoc"
  sed -i '/END generated:tier-rows/d' "$tmpdoc"
  run bash "$GEN" --check "$DEFAULTS" "$tmpdoc"
  [ "$status" -ne 0 ]
  [ -z "$(ls -A "$TMPDIR")" ]
}
