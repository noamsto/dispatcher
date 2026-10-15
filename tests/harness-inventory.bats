bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# scripts/harness-inventory.sh: --check compares rows by identity — (case_id,
# source_file, expression) with multiplicity — and ignores `source_line`, so an
# edit that only moves lines passes while an assertion added, removed or changed
# fails. Write mode (#1028) re-derives column 4 of tests/harness/assertions.tsv
# by joining each row onto the line the check expects for its identity, and
# refuses to write when that join is not total. Cases are located by @test name
# through tests/harness/manifest.tsv, never by its source_line.
#
# Every case runs against a fixture repo under $BATS_TEST_TMPDIR; the real
# inventories are never read or written here.

inventory() { run --separate-stderr bash "$REPO/scripts/harness-inventory.sh" "$@"; }

setup() {
  bats_require_minimum_version 1.5.0
  REPO="$BATS_TEST_TMPDIR/repo"
  HARNESS="$REPO/tests/harness"
  mkdir -p "$REPO/scripts" "$HARNESS"
  cp "$BATS_TEST_DIRNAME/../scripts/harness-inventory.sh" "$REPO/scripts/harness-inventory.sh"

  printf 'case_id\tsource_file\tsource_line\tfamily\ttest_name\ttags\n' >"$HARNESS/manifest.tsv"
  manifest_row demo-alpha tests/crew-id.bats 3 'alpha one'
  manifest_row demo-beta tests/pr-watch.bats 3 'beta one'

  printf 'case_id\tsetup\tteardown\tsourced_functions\tpublic_subprocesses\tstubs\tenvironment_isolation\tstdout_boundary\tstderr_boundary\tstatus_boundary\tside_effect_boundary\n' >"$HARNESS/boundaries.tsv"
  boundary_row demo-alpha
  boundary_row demo-beta

  # Two cases; the second repeats an expression, so one key holds two lines.
  printf '#!/usr/bin/env bats\n\n@test "alpha one" {\n  [ "$status" -eq 0 ]\n  [ "$output" = "ok" ]\n}\n' >"$REPO/tests/crew-id.bats"
  printf '#!/usr/bin/env bats\n\n@test "beta one" {\n  [ "$status" -eq 0 ]\n  [ "$output" = "ok" ]\n  [ "$status" -eq 0 ]\n}\n' >"$REPO/tests/pr-watch.bats"
  : >"$REPO/tests/refresh-models.bats"
  : >"$REPO/tests/dispatch.bats"

  printf 'case_id\tassert_id\tsource_file\tsource_line\tkind\texpression\n' >"$HARNESS/assertions.tsv"
  assertion_row demo-alpha alpha-status tests/crew-id.bats 4 status '[ "$status" -eq 0 ]'
  assertion_row demo-alpha alpha-stdout tests/crew-id.bats 5 stdout '[ "$output" = "ok" ]'
  assertion_row demo-beta beta-status tests/pr-watch.bats 4 status '[ "$status" -eq 0 ]'
  assertion_row demo-beta beta-stdout tests/pr-watch.bats 5 stdout '[ "$output" = "ok" ]'
  assertion_row demo-beta beta-status-again tests/pr-watch.bats 6 status '[ "$status" -eq 0 ]'

  cp "$HARNESS/assertions.tsv" "$BATS_TEST_TMPDIR/before.tsv"
}

manifest_row() { # <case_id> <source_file> <source_line> <test_name>
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "${1%%-*}" "$4" fast >>"$HARNESS/manifest.tsv"
}

boundary_row() { # <case_id>
  local row=$1 field
  for field in setup teardown sourced_functions public_subprocesses stubs \
    environment_isolation stdout_boundary stderr_boundary status_boundary side_effect_boundary; do
    row+=$'\t'"$field"
  done
  printf '%s\n' "$row" >>"$HARNESS/boundaries.tsv"
}

assertion_row() {
  printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$@" >>"$HARNESS/assertions.tsv"
}

# shift_down FILE: insert a blank line at the top, moving every recorded
# source_line below it.
shift_down() {
  local file=$1
  {
    printf '\n'
    cat "$file"
  } >"$file.new" && mv "$file.new" "$file"
}

insert_after() { # <file> <line> <text>
  awk -v line="$2" -v text="$3" '{ print } NR == line { print text }' "$1" >"$1.new" &&
    mv "$1.new" "$1"
}

delete_line() { # <file> <line>
  awk -v line="$2" 'NR != line { print }' "$1" >"$1.new" && mv "$1.new" "$1"
}

@test "fixture: --check passes on a current inventory and says nothing" {
  inventory --check
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [ -z "$stderr" ]
}

@test "write mode leaves a current inventory byte-identical" {
  inventory
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  cmp "$HARNESS/assertions.tsv" "$BATS_TEST_TMPDIR/before.tsv"

  inventory --check
  [ "$status" -eq 0 ]
}

@test "--check ignores line shifts, stale manifest lines included" {
  shift_down "$REPO/tests/pr-watch.bats"

  inventory --check
  [ "$status" -eq 0 ]
  [ -z "$output" ]
  [ -z "$stderr" ]
}

@test "write mode re-derives shifted lines and touches only column 4" {
  shift_down "$REPO/tests/pr-watch.bats"

  inventory
  [ "$status" -eq 0 ]

  # Columns 1,2,3,5,6 are byte-identical: only source_line moved, and a key's
  # two lines were consumed in ascending order.
  diff <(cut -f1,2,3,5,6 "$BATS_TEST_TMPDIR/before.tsv") \
    <(cut -f1,2,3,5,6 "$HARNESS/assertions.tsv")
  [ "$(cut -f4 "$HARNESS/assertions.tsv" | tail -n +2 | tr '\n' ' ')" = "4 5 5 6 7 " ]

  inventory --check
  [ "$status" -eq 0 ]
}

@test "--check fails naming the row when an assertion is removed" {
  delete_line "$REPO/tests/pr-watch.bats" 4

  inventory --check
  [ "$status" -eq 1 ]
  [[ $output == *'+demo-beta'*'tests/pr-watch.bats'*'[ "$status" -eq 0 ]'* ]]
  [[ $stderr == *"assertion inventory is stale or incomplete"* ]]
}

@test "--check fails naming the row when an assertion is changed" {
  delete_line "$REPO/tests/pr-watch.bats" 5
  insert_after "$REPO/tests/pr-watch.bats" 4 '  [ "$output" = "rewritten" ]'

  inventory --check
  [ "$status" -eq 1 ]
  [[ $output == *'-demo-beta'*'[ "$output" = "rewritten" ]'* ]]
  [[ $output == *'+demo-beta'*'[ "$output" = "ok" ]'* ]]
}

@test "--check fails when a repeated assertion gains a copy" {
  insert_after "$REPO/tests/pr-watch.bats" 6 '  [ "$status" -eq 0 ]'

  inventory --check
  [ "$status" -eq 1 ]
  [[ $output == *'-demo-beta'*'[ "$status" -eq 0 ]'* ]]
}

@test "--check fails when a sampled @test is renamed" {
  sed -i 's/@test "beta one"/@test "beta renamed"/' "$REPO/tests/pr-watch.bats"

  inventory --check
  [ "$status" -eq 1 ]
  [[ $output == *'+demo-beta'* ]]
}

@test "write mode refuses when a bats assertion was removed, leaving the TSV alone" {
  delete_line "$REPO/tests/pr-watch.bats" 4

  inventory
  [ "$status" -ne 0 ]
  [[ $stderr == *"no bats assertion matches row beta-status-again"* ]]
  [[ $stderr == *"nothing written"* ]]
  cmp "$HARNESS/assertions.tsv" "$BATS_TEST_TMPDIR/before.tsv"
}

@test "write mode refuses when a bats assertion was added, leaving the TSV alone" {
  insert_after "$REPO/tests/pr-watch.bats" 4 '  [ "$output" = "extra" ]'

  inventory
  [ "$status" -ne 0 ]
  [[ $stderr == *"bats assertion demo-beta at tests/pr-watch.bats:5 is not in the inventory"* ]]
  [[ $stderr == *"nothing written"* ]]
  cmp "$HARNESS/assertions.tsv" "$BATS_TEST_TMPDIR/before.tsv"
}

@test "write mode refuses when a bats assertion was changed, leaving the TSV alone" {
  delete_line "$REPO/tests/pr-watch.bats" 5
  insert_after "$REPO/tests/pr-watch.bats" 4 '  [ "$output" = "rewritten" ]'

  inventory
  [ "$status" -ne 0 ]
  [[ $stderr == *"no bats assertion matches row beta-stdout"* ]]
  [[ $stderr == *"is not in the inventory"* ]]
  cmp "$HARNESS/assertions.tsv" "$BATS_TEST_TMPDIR/before.tsv"
}

@test "write mode keeps --check's CLI: no argument or one flag, nothing else" {
  inventory --write
  [ "$status" -eq 2 ]
  [[ $stderr == *"usage: "*"[--check]"* ]]

  inventory --check --check
  [ "$status" -eq 2 ]

  inventory --check extra
  [ "$status" -eq 2 ]
  cmp "$HARNESS/assertions.tsv" "$BATS_TEST_TMPDIR/before.tsv"
}
