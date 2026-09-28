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
