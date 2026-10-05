#!/usr/bin/env bash
# Line coverage of the bats suite.
# Stdout: per-test TSV (file, test, covered, unique), a per-file summary,
# then the unmeasured bucket. Stderr: peak disk, wall time, raw trace sizes.
# Usage: scripts/bats-coverage.sh [--out <dir>] [--reduce-only <dir>] [tests/<file>.bats ...]
#   JOBS=N sets file parallelism. Without files, runs tests/*.bats.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

jobs="${JOBS:-16}"

# Drop bats' own lib/libexec, map temp copies back onto repo sources by
# suffix, and fold generated adapter projections onto adapters/core/ when
# the same relative suffix exists there. Keep production sources only.
reduce_trace() {
  local trace="$1" dest="$2"
  awk -v repo="$REPO" -v bats_lib="$BATS_LIB" -v bats_libexec="$BATS_LIBEXEC" '
    FNR == NR { files[$0] = 1; next }
    function keep(rel) {
      return rel ~ /^(adapters\/core\/|scripts\/|dash\/)/
    }
    function map_path(p,    s, rel, rest, best, slash) {
      if (p == "") return ""
      if (bats_lib != "" && index(p, bats_lib "/") == 1) return ""
      if (bats_libexec != "" && index(p, bats_libexec "/") == 1) return ""
      if (index(p, "/bats-core/") > 0) return ""
      rel = ""
      if (repo != "" && index(p, repo "/") == 1) {
        rel = substr(p, length(repo) + 2)
        if (!(rel in files)) rel = ""
      }
      if (rel == "") {
        s = p
        while (s != "") {
          if (s in files) { rel = s; break }
          slash = index(s, "/")
          if (slash == 0) break
          s = substr(s, slash + 1)
        }
      }
      if (rel == "") return ""
      if (rel ~ /^adapters\/[^/]+\// && rel !~ /^adapters\/core\//) {
        rest = rel
        sub(/^adapters\/[^/]+\//, "", rest)
        s = rest
        best = ""
        while (s != "") {
          if (("adapters/core/" s) in files) { best = "adapters/core/" s; break }
          slash = index(s, "/")
          if (slash == 0) break
          s = substr(s, slash + 1)
        }
        if (best != "") rel = best
      }
      if (keep(rel)) return rel
      return ""
    }
    {
      if (substr($0, 1, 1) != "+") next
      rest = $0
      sub(/^\++/, "", rest)
      if (match(rest, /^[^:]+:[0-9]+\+/) == 0) next
      token = substr(rest, 1, RLENGTH - 1)
      if (match(token, /:[0-9]+$/) == 0) next
      path = substr(token, 1, RSTART - 1)
      lineno = substr(token, RSTART + 1)
      mapped = map_path(path)
      if (mapped == "") next
      print mapped ":" lineno
    }
  ' "$REPO_FILES" "$trace" | sort -u >"$dest"
}

note_disk() {
  local sz cur
  sz=$(du -sb "$WORK" 2>/dev/null | awk 'NR == 1 { print $1 }')
  [[ -n ${sz:-} ]] || return 0
  {
    flock 9
    cur=0
    if [[ -f $WORK/peak.note ]]; then
      cur=$(<"$WORK/peak.note")
    fi
    if ((sz > cur)); then
      printf '%s\n' "$sz" >"$WORK/peak.note"
    fi
  } 9>"$WORK/peak.lock"
}

tap_count() {
  local n
  n=$(grep -cE '^(not )?ok [0-9]+( |$)' "$1" || true)
  printf '%s\n' "${n:-0}"
}

# Stream TAP. On each ok/not-ok the test process has exited, so reduce that
# test's trace and delete the raw before the next test's trace can pile up.
janitor() {
  local base="$1" tap="$2"
  local line n trace dest sz
  : >"$tap"
  : >"$WORK/sizes/$base.tsv"
  while IFS= read -r line || [[ -n ${line:-} ]]; do
    printf '%s\n' "$line" >>"$tap"
    if [[ $line =~ ^(not[[:space:]]+)?ok[[:space:]]+([0-9]+) ]]; then
      n="${BASH_REMATCH[2]}"
      trace="$WORK/$base.$n.trace"
      dest="$LINES_DIR/$base.$n.lines"
      if [[ -f $trace ]]; then
        sz=$(wc -c <"$trace" | tr -d '[:space:]')
        note_disk
        if ! reduce_trace "$trace" "$dest"; then
          printf 'bats-coverage: reduce failed for %s\n' "$trace" >&2
          return 1
        fi
        rm -f "$trace"
      else
        sz=0
        : >"$dest"
      fi
      printf '%s\t%s\n' "$n" "$sz" >>"$WORK/sizes/$base.tsv"
    fi
  done
}

write_meta() {
  local file="$1" base="$2" tap_u="$3" tap_t="$4"
  awk -v file="$file" -v base="$base" -v tap_u="$tap_u" -v tap_t="$tap_t" \
    -v sizes="$WORK/sizes/$base.tsv" -v lines_dir="$LINES_DIR" '
    function absorb(tap, which,    line, n, name, s) {
      while ((getline line < tap) > 0) {
        if (line ~ /^(not )?ok [0-9]+( |$)/) {
          n = line
          sub(/^(not )?ok /, "", n)
          sub(/ .*$/, "", n)
          name = line
          sub(/^(not )?ok [0-9]+ /, "", name)
          s = (line ~ /^not /) ? "not-ok" : "ok"
          if (name ~ / # skip/) {
            s = "skip"
            sub(/ # skip.*$/, "", name)
          }
          sub(/ in [0-9]+ms$/, "", name)
          st[which, n] = s
          if (which == "u") nm[n] = name
          if (n + 0 > max) max = n + 0
        }
      }
      close(tap)
    }
    BEGIN {
      absorb(tap_u, "u")
      absorb(tap_t, "t")
      while ((getline line < sizes) > 0) {
        split(line, a, "\t")
        bytes[a[1]] = a[2]
      }
      close(sizes)
      for (n = 1; n <= max; n++) {
        lf = lines_dir "/" base "." n ".lines"
        covered = 0
        while ((getline l < lf) > 0) covered++
        close(lf)
        b = (n in bytes) ? bytes[n] : 0
        printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", file, n, nm[n], st["u", n], st["t", n], b, covered
      }
    }
  ' >"$WORK/meta/$base.tsv"
}

cover_file() {
  set -euo pipefail
  local file="$1" base tap_u tap_t want got i reduced
  base="$(basename "${file%.bats}")"
  mkdir -p "$LINES_DIR" "$WORK/sizes" "$WORK/meta" "$WORK/tap" "$WORK/records"
  tap_u="$WORK/tap/$base.untraced.tap"
  tap_t="$WORK/tap/$base.traced.tap"

  env -u BATS_COVERAGE_DIR -u BATS_COVERAGE_FILE bats --formatter tap "$file" >"$tap_u" || true

  set +e
  BATS_COVERAGE_DIR="$WORK" bats --formatter tap "$file" | janitor "$base" "$tap_t"
  local -a st=("${PIPESTATUS[@]}")
  set -e
  if [[ ${st[1]} -ne 0 ]]; then
    printf 'bats-coverage: %s: reducer failed\n' "$file" >&2
    return 1
  fi

  want=$(tap_count "$tap_u")
  got=$(tap_count "$tap_t")
  if [[ $want -eq 0 || $got -eq 0 ]]; then
    printf 'bats-coverage: %s: TAP listed no tests (untraced %s, traced %s)\n' "$file" "$want" "$got" >&2
    return 1
  fi
  if [[ $want -ne $got ]]; then
    printf 'bats-coverage: %s: untraced TAP count %s != traced TAP count %s\n' "$file" "$want" "$got" >&2
    return 1
  fi
  reduced=0
  for ((i = 1; i <= want; i++)); do
    if [[ ! -f $LINES_DIR/$base.$i.lines ]]; then
      printf 'bats-coverage: %s: missing reduced record for test %s\n' "$file" "$i" >&2
      return 1
    fi
    reduced=$((reduced + 1))
  done
  if [[ $reduced -ne $want ]]; then
    printf 'bats-coverage: %s: reduced-record count %s != TAP test count %s\n' "$file" "$reduced" "$want" >&2
    return 1
  fi
  local leftover=() t
  while IFS= read -r t; do
    leftover+=("$t")
  done < <(compgen -G "$WORK/$base.[0-9]*.trace" || true)
  if [[ ${#leftover[@]} -gt 0 ]]; then
    printf 'bats-coverage: %s: %s raw trace(s) left behind\n' "$file" "${#leftover[@]}" >&2
    return 1
  fi

  write_meta "$file" "$base" "$tap_u" "$tap_t"
  awk -F '\t' -v base="$base" '{ printf "%s\t%s\t%s\n", base, $2, $7 }' \
    "$WORK/meta/$base.tsv" >"$WORK/records/$base.tsv"
}

export -f reduce_trace note_disk tap_count janitor write_meta cover_file

startup_self_test() {
  local dir trace
  dir="$(mktemp -d)"
  cat >"$dir/coverage-selftest.bats" <<EOF
source "$REPO/tests/coverage.bash"
@test "startup self-test traces adapters/core/crew.sh" {
  bash "$REPO/adapters/core/crew.sh" id || true
}
EOF
  if ! BATS_COVERAGE_DIR="$WORK" bats --formatter tap "$dir/coverage-selftest.bats" >"$dir/out.tap"; then
    printf 'bats-coverage: startup self-test bats run failed\n' >&2
    cat "$dir/out.tap" >&2 || true
    rm -rf "$dir"
    exit 1
  fi
  trace="$WORK/coverage-selftest.1.trace"
  if [[ ! -f $trace ]] || ! grep -qE 'adapters/core/crew\.sh:[0-9]+\+' "$trace"; then
    printf 'bats-coverage: startup self-test failed: trace has no adapters/core/crew.sh line\n' >&2
    if [[ -f $trace ]]; then
      printf 'bats-coverage: trace head:\n' >&2
      head -n 30 "$trace" >&2 || true
    else
      printf 'bats-coverage: missing %s\n' "$trace" >&2
      ls -la "$WORK" >&2 || true
    fi
    rm -rf "$dir"
    exit 1
  fi
  rm -f "$trace"
  rm -rf "$dir"
}

start_sampler() {
  (
    local peak=0 sz
    while true; do
      sz=$(du -sb "$WORK" 2>/dev/null | awk 'NR == 1 { print $1 }')
      if [[ -n ${sz:-} ]] && ((sz > peak)); then
        peak=$sz
        printf '%s\n' "$peak" >"$WORK/peak.sampler"
      fi
      sleep 0.2
    done
  ) &
  sampler_pid=$!
}

stop_sampler() {
  if [[ -n ${sampler_pid:-} ]]; then
    kill "$sampler_pid" 2>/dev/null || true
    wait "$sampler_pid" 2>/dev/null || true
    sampler_pid=
  fi
}

peak_bytes() {
  local best=0 v f
  for f in "$WORK/peak.sampler" "$WORK/peak.note"; do
    [[ -f $f ]] || continue
    v=$(<"$f")
    if [[ ${v:-0} -gt $best ]]; then
      best=$v
    fi
  done
  printf '%s\n' "$best"
}

# Manifest columns: file, num, name, untraced, traced, bytes, covered.
# lines live at $LINES_DIR/<base>.<num>.lines
emit_report() {
  local manifest="$1"
  awk -v lines_dir="$LINES_DIR" -v records_out="${RECORDS_OUT:-}" '
    function base_of(path,    b) {
      b = path
      sub(/.*\//, "", b)
      sub(/\.bats$/, "", b)
      return b
    }
    function load_lines(i,    p, j, l, b) {
      b = base_of(file[i])
      p = lines_dir "/" b "." num[i] ".lines"
      j = 0
      while ((getline l < p) > 0) {
        j++
        linev[i, j] = l
      }
      close(p)
      nlines[i] = j
    }
    BEGIN { FS = "\t" }
    NF < 7 { next }
    {
      nrow++
      file[nrow] = $1
      num[nrow] = $2
      name[nrow] = $3
      ust[nrow] = $4
      tst[nrow] = $5
      bytes[nrow] = $6
      covered[nrow] = $7 + 0
      if (ust[nrow] != tst[nrow]) reason[nrow] = "outcome"
      if (covered[nrow] == 0) {
        reason[nrow] = (reason[nrow] == "" ? "no-lines" : reason[nrow] ",no-lines")
      }
      measured[nrow] = (reason[nrow] == "") ? 1 : 0
      if (measured[nrow]) load_lines(nrow)
    }
    END {
      for (i = 1; i <= nrow; i++) {
        if (!measured[i]) continue
        for (j = 1; j <= nlines[i]; j++) count[linev[i, j]]++
      }
      print "file\ttest\tcovered\tunique"
      for (i = 1; i <= nrow; i++) {
        uniq = "-"
        if (measured[i]) {
          uniq = 0
          for (j = 1; j <= nlines[i]; j++) if (count[linev[i, j]] == 1) uniq++
        }
        printf "%s\t%s\t%s\t%s\n", file[i], name[i], covered[i], uniq
        unique_n[i] = uniq
      }
      print ""
      print "file\ttests\tmeasured\tunmeasured\tzero_unique_share"
      i = 1
      while (i <= nrow) {
        f = file[i]
        tests = 0
        meas = 0
        unmeas = 0
        zero = 0
        while (i <= nrow && file[i] == f) {
          tests++
          if (measured[i]) {
            meas++
            if (unique_n[i] + 0 == 0) zero++
          } else unmeas++
          i++
        }
        if (meas == 0) share = "na"
        else share = sprintf("%.4f", zero / meas)
        printf "%s\t%s\t%s\t%s\t%s\n", f, tests, meas, unmeas, share
        file_unmeas[f] = unmeas
      }
      print ""
      un_total = 0
      for (i = 1; i <= nrow; i++) if (!measured[i]) un_total++
      printf "unmeasured\t%s\n", un_total
      for (i = 1; i <= nrow; i++) {
        if (measured[i]) continue
        printf "%s\t%s\t%s\n", file[i], name[i], reason[i]
      }
      if (records_out != "") {
        for (i = 1; i <= nrow; i++) {
          printf "%s\t%s\t%s\n", base_of(file[i]), num[i], covered[i] > records_out
        }
        close(records_out)
      }
    }
  ' "$manifest"
}

build_manifest() {
  local manifest="$1" f base
  : >"$manifest"
  for f in "${files[@]}"; do
    base="$(basename "${f%.bats}")"
    if [[ ! -f $WORK/meta/$base.tsv ]]; then
      printf 'bats-coverage: missing meta for %s\n' "$f" >&2
      return 1
    fi
    cat "$WORK/meta/$base.tsv" >>"$manifest"
  done
}

out=""
reduce_only=""
files=()
while [[ $# -gt 0 ]]; do
  case "$1" in
  --out)
    [[ $# -ge 2 ]] || {
      printf 'bats-coverage: --out needs a directory\n' >&2
      exit 2
    }
    out="$2"
    shift 2
    ;;
  --reduce-only)
    [[ $# -ge 2 ]] || {
      printf 'bats-coverage: --reduce-only needs a directory\n' >&2
      exit 2
    }
    reduce_only="$2"
    shift 2
    ;;
  --)
    shift
    files+=("$@")
    break
    ;;
  -*)
    printf 'bats-coverage: unknown option %s\n' "$1" >&2
    exit 2
    ;;
  *)
    files+=("$1")
    shift
    ;;
  esac
done

if [[ -n $reduce_only ]]; then
  [[ -d $reduce_only ]] || {
    printf 'bats-coverage: --reduce-only %s is not a directory\n' "$reduce_only" >&2
    exit 1
  }
  LINES_DIR="$reduce_only"
  if [[ -n $out ]]; then
    mkdir -p "$out"
    RECORDS_OUT="$out/records.tsv"
    : >"$RECORDS_OUT"
  else
    RECORDS_OUT=""
  fi
  export LINES_DIR RECORDS_OUT
  manifest="$(mktemp)"
  if [[ -f $reduce_only/names.tsv ]]; then
    cp "$reduce_only/names.tsv" "$manifest"
  else
    : >"$manifest"
    shopt -s nullglob
    for lf in "$reduce_only"/*.lines; do
      bn="$(basename "$lf" .lines)"
      n="${bn##*.}"
      b="${bn%.*}"
      covered=$(awk 'END { print NR }' "$lf")
      printf 'tests/%s.bats\t%s\t#%s\tok\tok\t0\t%s\n' "$b" "$n" "$n" "$covered" >>"$manifest"
    done
    shopt -u nullglob
  fi
  emit_report "$manifest"
  rm -f "$manifest"
  exit 0
fi

if [[ ${#files[@]} -eq 0 ]]; then
  mapfile -t files < <(ls tests/*.bats)
fi

REPO="$PWD"
bats_bin="$(command -v bats)"
bats_prefix="$(cd "$(dirname "$bats_bin")/.." && pwd)"
BATS_LIB="$bats_prefix/lib"
BATS_LIBEXEC="$bats_prefix/libexec"
WORK="$(mktemp -d "$PWD/.bats-coverage.XXXXXX")"
if [[ -n $out ]]; then
  mkdir -p "$out"
  LINES_DIR="$out"
  RECORDS_OUT="$out/records.tsv"
  : >"$RECORDS_OUT"
else
  LINES_DIR="$WORK/lines"
  RECORDS_OUT=""
fi
export REPO BATS_LIB BATS_LIBEXEC WORK LINES_DIR RECORDS_OUT
mkdir -p "$LINES_DIR"

sampler_pid=
cleanup() {
  local rc=$?
  stop_sampler || true
  if [[ -n ${WORK:-} && -d ${WORK:-} ]]; then
    rm -rf "$WORK"
  fi
  exit "$rc"
}
trap cleanup EXIT

start=$SECONDS
start_sampler
git ls-files >"$WORK/repo-files.txt"
REPO_FILES="$WORK/repo-files.txt"
export REPO_FILES

startup_self_test

parallel -j "$jobs" cover_file {} ::: "${files[@]}"

build_manifest "$WORK/manifest.tsv"
emit_report "$WORK/manifest.tsv"

if [[ -n $out ]]; then
  cp "$WORK/manifest.tsv" "$out/names.tsv"
fi

printf 'trace-bytes\tfile\tnum\tbytes\n' >&2
awk -F '\t' '{ printf "trace-bytes\t%s\t%s\t%s\n", $1, $2, $6 }' "$WORK/manifest.tsv" >&2

stop_sampler
printf 'peak-disk: %s bytes\n' "$(peak_bytes)" >&2
printf 'wall-time: %ss\n' "$((SECONDS - start))" >&2
