#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

readonly manifest=tests/harness/manifest.tsv
readonly assertions=tests/harness/assertions.tsv
readonly boundaries=tests/harness/boundaries.tsv

usage() {
  echo "usage: $0 [--check]" >&2
  exit 2
}

case "${1:-}" in
"") mode="write" ;;
--check) mode="check" ;;
*) usage ;;
esac
(($# <= 1)) || usage

for file in "$manifest" "$assertions" "$boundaries"; do
  if [[ ! -f $file ]]; then
    echo "harness-inventory: missing $file" >&2
    exit 1
  fi
done

if ! awk -F '\t' '
  FILENAME == ARGV[1] {
    if (FNR > 1) manifest[$1] = 1
    next
  }
  BEGIN {
    expected = "case_id\tassert_id\tsource_file\tsource_line\tkind\texpression"
  }
  FNR == 1 { if ($0 != expected) exit 1; next }
  NF != 6 || !manifest[$1] || $2 == "" || $3 == "" || $4 !~ /^[0-9]+$/ || $5 == "" || $6 == "" { exit 1 }
  seen[$2]++ { exit 1 }
  seen_location[$1 SUBSEP $3 SUBSEP $4]++ { exit 1 }
  { per_case[$1]++; rows++ }
  END {
    if (!rows) exit 1
    for (case_id in manifest) if (!per_case[case_id]) exit 1
  }
' "$manifest" "$assertions"; then
  echo "harness-inventory: invalid assertion schema, ID, or case coverage" >&2
  exit 1
fi

if ! awk -F '\t' '
  FILENAME == ARGV[1] {
    if (FNR > 1) manifest[$1] = 1
    next
  }
  BEGIN {
    expected = "case_id\tsetup\tteardown\tsourced_functions\tpublic_subprocesses\tstubs\tenvironment_isolation\tstdout_boundary\tstderr_boundary\tstatus_boundary\tside_effect_boundary"
  }
  FNR == 1 { if ($0 != expected) exit 1; next }
  NF != 11 || !manifest[$1] { exit 1 }
  {
    for (field = 2; field <= 11; field++) if ($field == "") exit 1
    seen[$1]++
    rows++
  }
  END {
    if (!rows) exit 1
    for (case_id in manifest) if (seen[case_id] != 1) exit 1
  }
' "$manifest" "$boundaries"; then
  echo "harness-inventory: invalid boundary schema or manifest case coverage" >&2
  exit 1
fi

# The rows the inventory must hold, read from the bats files as they are now:
# case_id, source_file, source_line, expression. Cases are found by @test name
# through the manifest, never by its source_line. --check diffs this against the
# inventory ignoring source_line; write mode joins it back onto the existing rows.
expected_rows() {
  awk -F '\t' -v sq="'" '
      function is_assertion(line) {
        return (line ~ /^[[:space:]]*(\[ |\[\[ )/ && line !~ /\][[:space:]]*&&[[:space:]]*(continue|break|return)/) || \
          line ~ /(^[[:space:]]*|[|;][[:space:]]*)jq[[:space:]]+-e([[:space:]]|$)/
      }
      # Heredoc bodies are data, not code: track the delimiter and skip the
      # body, or stub scripts written by fixtures get misread as assertions.
      # (Dynamic regexes: a literal single quote would end the shell string.)
      function track_heredoc(line) {
        if (heredoc != "") {
          if (line ~ ("^[[:space:]]*" heredoc "[[:space:]]*$")) heredoc = ""
          return 1
        }
        if (line ~ ("<<-?[[:space:]]*[" sq "]?[A-Za-z_][A-Za-z0-9_]*[" sq "]?[[:space:]]*$")) {
          heredoc = line
          sub(".*<<-?[[:space:]]*[" sq "]?", "", heredoc)
          sub("[" sq "]?[[:space:]]*$", "", heredoc)
        }
        return 0
      }
      function emit(case_id, file, line, expression) {
        sub(/^[[:space:]]+/, "", expression)
        print case_id "\t" file "\t" line "\t" expression
      }

      FILENAME == ARGV[1] {
        if (FNR > 1) case_at[$2 SUBSEP $5] = $1
        next
      }

      FNR == 1 { pass[FILENAME]++ }

      pass[FILENAME] == 1 {
        if (track_heredoc($0)) next
        if ($0 ~ /^[[:alnum:]_]+\(\)[[:space:]]*\{[[:space:]]*$/) {
          helper = $0
          sub(/\(\).*/, "", helper)
          next
        }
        if (helper != "" && $0 ~ /^}/) {
          helper = ""
          next
        }
        if (helper != "" && is_assertion($0)) {
          key = FILENAME SUBSEP helper
          helper_count[key]++
          helper_line[key SUBSEP helper_count[key]] = FNR
          helper_expression[key SUBSEP helper_count[key]] = $0
        }
        next
      }

      /^@test / {
        name = $0
        sub(/^@test "/, "", name)
        sub(/" \{$/, "", name)
        current_case = case_at[FILENAME SUBSEP name]
        next
      }
      current_case != "" && /^}/ {
        current_case = ""
        next
      }
      current_case != "" {
        if (track_heredoc($0)) next
        if (is_assertion($0)) emit(current_case, FILENAME, FNR, $0)

        call = $0
        sub(/^[[:space:]]+/, "", call)
        sub(/[[:space:]].*$/, "", call)
        key = FILENAME SUBSEP call
        for (i = 1; i <= helper_count[key]; i++) {
          emit(current_case, FILENAME, helper_line[key SUBSEP i], helper_expression[key SUBSEP i])
        }
      }
    ' "$manifest" \
    tests/refresh-models.bats tests/pr-watch.bats tests/dispatch.bats \
    tests/refresh-models.bats tests/pr-watch.bats tests/dispatch.bats |
    LC_ALL=C sort -t $'\t' -k1,1 -k2,2 -k3,3n
}

if [[ $mode == check ]]; then
  # source_line is dropped from both sides, so a line shift alone never fails;
  # a whole-row sort keeps each key's multiplicity.
  if ! diff -u <(expected_rows | cut -f1,2,4 | LC_ALL=C sort) \
    <(tail -n +2 "$assertions" | cut -f1,3,6 | LC_ALL=C sort); then
    echo "harness-inventory: assertion inventory is stale or incomplete" >&2
    exit 1
  fi
else
  scratch=$(mktemp -d)
  trap 'rm -rf "$scratch"' EXIT
  expected_rows >"$scratch/expected.tsv"

  # Join each inventory row onto the expected line of its
  # (case_id, source_file, expression), taking a key's lines in ascending order
  # so repeated expressions keep their relative order; only column 4 moves.
  # A row that matches nothing, or an expected line no row claims, means the
  # bats file gained, lost or changed an assertion — writing then would invent
  # or drop inventory rows, so a partial join writes nothing.
  if ! awk -F '\t' -v OFS='\t' '
    FILENAME == ARGV[1] {
      key = $1 SUBSEP $2 SUBSEP $4
      # key_order keeps the refusal messages deterministic: awk hash order is not.
      if (count[key] == 0) key_order[++key_count] = key
      n = ++count[key]
      line_of[key SUBSEP n] = $3 + 0
      row_of[key SUBSEP n] = $0
      next
    }
    FNR == 1 { print; next }
    {
      key = $1 SUBSEP $3 SUBSEP $6
      pick = 0
      for (i = 1; i <= count[key]; i++) {
        if (!used[key SUBSEP i] && (pick == 0 || line_of[key SUBSEP i] < line_of[key SUBSEP pick])) {
          pick = i
        }
      }
      if (pick == 0) {
        printf "harness-inventory: no bats assertion matches row %s (%s %s: %s)\n", $2, $1, $3, $6 > "/dev/stderr"
        missing++
        next
      }
      used[key SUBSEP pick] = 1
      $4 = line_of[key SUBSEP pick]
      print
    }
    END {
      for (o = 1; o <= key_count; o++) {
        key = key_order[o]
        for (i = 1; i <= count[key]; i++) {
          if (used[key SUBSEP i]) continue
          split(row_of[key SUBSEP i], f, "\t")
          printf "harness-inventory: bats assertion %s at %s:%s is not in the inventory: %s\n", f[1], f[2], f[3], f[4] > "/dev/stderr"
          extra++
        }
      }
      if (missing || extra) {
        print "harness-inventory: the bats files and the inventory disagree; nothing written" > "/dev/stderr"
        exit 1
      }
    }
  ' "$scratch/expected.tsv" "$assertions" >"$scratch/out.tsv"; then
    exit 1
  fi

  cat "$scratch/out.tsv" >"$assertions"
fi
