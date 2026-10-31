#!/usr/bin/env bats
# The generated doc regions in dispatch-orchestration.md (#560): the
# model-classes (#1391) and pace-downgrades tables are rendered from
# adapters/core/defaults.json, and `--check` also asserts the hand-written
# Model map table names each tier row's class.

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

# keep_row runs one row under set -e and keeps going. finish_rows fails once,
# naming every row that failed. A short read must not pass with zero rows.
begin_rows() {
  ROW_FAILS=()
  ROW_N=0
}

keep_row() {
  local id=$1 err rc
  shift
  local -a cmd=("$@")
  ROW_N=$((ROW_N + 1))
  set +e
  err=$(
    set -e
    trap - ERR
    "${cmd[@]}" 2>&1
  )
  rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    ROW_FAILS+=("$id")
    printf 'row %s failed\n' "$id" >&2
    if [ -n "$err" ]; then
      printf '%s\n' "$err" >&2
    fi
    BATS_ERROR_STATUS=
    BATS_ERROR_SUFFIX=
  fi
}

finish_rows() {
  local want=$1
  if [ "$ROW_N" -ne "$want" ]; then
    printf 'expected %s rows, ran %s\n' "$want" "$ROW_N" >&2
    return 1
  fi
  if [ "${#ROW_FAILS[@]}" -gt 0 ]; then
    printf 'failed rows: %s\n' "${ROW_FAILS[*]}" >&2
    return 1
  fi
}

# F55: --check fails on a stale generated row, a worker cell that disagrees
# with the row's class, and missing model-classes markers. Each row copies the doc to its
# own file; no shared stub or cache.
@test "doc check fails when a generated region is stale or missing" {
  begin_rows
  local row expr
  while IFS='|' read -r row expr; do
    [ -n "$row" ] || continue
    keep_row "$row" doc_fail_row "$row" "$expr"
  done <<'ROWS'
stale-row|s/openrouter\/deepseek\/deepseek-v4\.1-flash/openrouter\/deepseek\/deepseek-v9-flash/
worker-cell|0,/\*\*@premium\*\* → @standard/s//\*\*@standard\*\* → @standard/
missing-markers|/BEGIN generated:model-classes\|END generated:model-classes/d
ROWS
  finish_rows 3
}

doc_fail_row() { # id sed-expr
  local tmpdoc="$BATS_TEST_TMPDIR/$1.md"
  cp "$DOC" "$tmpdoc"
  sed -i "$2" "$tmpdoc"
  run bash "$GEN" --check "$DEFAULTS" "$tmpdoc"
  [ "$status" -eq 1 ]
}

# #1391: one row per engine x class, with its tiers, patterns and seed default.
@test "the model-classes region lists each class's tiers, patterns and seed default" {
  region="$(sed -n '/BEGIN generated:model-classes/,/END generated:model-classes/p' "$DOC")"
  grep -qxF '| claude | `@cheap` | `trivial` | `trivial`, `standard` | `haiku`, `claude-haiku-*` | `haiku` |' <<<"$region"
  grep -qF '| pi | `@premium` | — | `deep` | `openrouter/anthropic/claude-opus-*` (needs pi_month_target)' <<<"$region"
  grep -qE '^\| pi \| `@premium` \|.*\| — \|$' <<<"$region"
  run grep -F '| pi | `@cheap` |' <<<"$region"
  [ "$status" -ne 0 ]
  want="$(jq '[.classes[] as $cl | "cheap", "standard", "premium"
    | select(. as $c | ($cl[$c] // [] | length > 0) or ([$cl.tiers[].class] | index($c) != null))] | length' "$DEFAULTS")"
  [ "$(grep -c '^| [a-z]* | `@' <<<"$region")" -eq "$want" ]
}

@test "doc check fails when a class seed default changes without regenerating" {
  tmpdefaults="$BATS_TEST_TMPDIR/new-seed.json"
  jq '.modelMap.codex.deep.default = "gpt-5.6-terra"' "$DEFAULTS" >"$tmpdefaults"
  run bash "$GEN" --check "$tmpdefaults" "$DOC"
  [ "$status" -eq 1 ]
  [[ "$output" == *'| codex | `@premium` |'*'`gpt-5.6-terra` |'* ]]
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
  sed -i 's|<!-- BEGIN generated:model-classes from adapters/core/defaults.json by scripts/gen-model-map-doc.sh -->|<!-- BEGIN generated:model-classes from adapters/core/defaults.json by scripts/gen-model-map-doc.sh --> |' "$tmpdoc"
  sed -i '0,/`gpt-5\.6-luna` |/s//`gpt-5.6-nano` |/' "$tmpdoc"
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
  sed -i '/END generated:model-classes/d' "$tmpdoc"
  run bash "$GEN" --check "$DEFAULTS" "$tmpdoc"
  [ "$status" -ne 0 ]
  [ -z "$(ls -A "$TMPDIR")" ]
}
