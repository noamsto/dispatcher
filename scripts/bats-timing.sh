#!/usr/bin/env bash
# Per-test timing of the bats suite. TSV on stdout: file, status, ms, test name.
# Usage: scripts/bats-timing.sh [tests/<file>.bats ...]   (JOBS=N sets parallelism)
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

jobs="${JOBS:-16}"

# Reads TAP on stdin; $1 labels the rows.
to_tsv() {
  awk -v file="$1" '
    /^(not )?ok [0-9]+ / {
      status = ($1 == "not") ? "not-ok" : "ok"
      line = $0
      sub(/^(not )?ok [0-9]+ /, "", line)
      ms = 0
      if (line ~ / # skip/) {
        status = "skip"
        sub(/ # skip.*$/, "", line)
      }
      if (match(line, / in [0-9]+ms$/)) {
        ms = substr(line, RSTART + 4, RLENGTH - 6)
        line = substr(line, 1, RSTART - 1)
      }
      printf "%s\t%s\t%d\t%s\n", file, status, ms, line
    }'
}

# Failing tests make bats exit non-zero; the rows already carry that.
run_file() {
  local file="$1" tags="$2" label="${3:-$1}"
  bats --timing --filter-tags "$tags" --formatter tap "$file" 2>/dev/null | to_tsv "$label" || true
}

export -f to_tsv run_file

phase() {
  local name="$1" start=$SECONDS
  shift
  "$@"
  echo "phase $name: $((SECONDS - start))s" >&2
}

parallel_phase() {
  parallel -j "$jobs" run_file {} '!timing' ::: "$@"
}

serial_phase() {
  local f
  for f in "$@"; do
    run_file "$f" '!timing'
  done
}

timing_phase() {
  run_file tests/secret-read-guard.bats timing 'tests/secret-read-guard.bats[timing]'
}

if (($# > 0)); then
  files=("$@")
else
  mapfile -t files < <(ls tests/*.bats)
fi

parallel_files=()
serial_files=()
for f in "${files[@]}"; do
  if [[ $f == */module.bats ]]; then
    serial_files+=("$f")
  else
    parallel_files+=("$f")
  fi
done

total_start=$SECONDS
((${#parallel_files[@]} == 0)) || phase "parallel(-j$jobs, ${#parallel_files[@]} files)" parallel_phase "${parallel_files[@]}"
((${#serial_files[@]} == 0)) || phase "module.bats" serial_phase "${serial_files[@]}"
if printf '%s\n' "${files[@]}" | grep -qx 'tests/secret-read-guard.bats'; then
  phase "secret-read-guard[timing]" timing_phase
fi
echo "total: $((SECONDS - total_start))s" >&2
