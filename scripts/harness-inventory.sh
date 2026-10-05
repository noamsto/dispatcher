#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

readonly manifest=tests/harness/manifest.tsv
readonly assertions=tests/harness/assertions.tsv
readonly boundaries=tests/harness/boundaries.tsv

if [[ $# -ne 1 || $1 != --check ]]; then
  echo "usage: $0 --check" >&2
  exit 2
fi

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

if ! diff -u \
  <(
    awk -F '\t' '
      function is_assertion(line) {
        return (line ~ /^[[:space:]]*(\[ |\[\[ )/ && line !~ /\][[:space:]]*&&[[:space:]]*(continue|break|return)/) || \
          line ~ /(^[[:space:]]*|[|;][[:space:]]*)jq[[:space:]]+-e([[:space:]]|$)/
      }
      function emit(case_id, file, line, expression) {
        sub(/^[[:space:]]+/, "", expression)
        print case_id "\t" file "\t" line "\t" expression
      }

      FILENAME == ARGV[1] {
        if (FNR > 1) case_at[$2 SUBSEP $3] = $1
        next
      }

      FNR == 1 { pass[FILENAME]++ }

      pass[FILENAME] == 1 {
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
        current_case = case_at[FILENAME SUBSEP FNR]
        next
      }
      current_case != "" && /^}/ {
        current_case = ""
        next
      }
      current_case != "" {
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
      tests/crew-id.bats tests/refresh-models.bats tests/pr-watch.bats \
      tests/crew-id.bats tests/refresh-models.bats tests/pr-watch.bats |
      LC_ALL=C sort -t $'\t' -k1,1 -k2,2 -k3,3n
  ) \
  <(
    tail -n +2 "$assertions" |
      cut -f1,3,4,6 |
      LC_ALL=C sort -t $'\t' -k1,1 -k2,2 -k3,3n
  ); then
  echo "harness-inventory: assertion inventory is stale or incomplete" >&2
  exit 1
fi
