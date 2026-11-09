#!/usr/bin/env bash
# Mutation check for the trust boundary (docs/dispatch-redesign.md, section 3).
#
#   trust-mutate.sh [--manifest <tsv>] [--jobs <n>] [--list] [ID...]
#   trust-mutate.sh --affected <base-ref>
#
# tests/trust-pins.tsv has one line per mutation site: a row ID may repeat when
# a guard is invoked at several sites. Columns (tab separated, `#` lines skipped):
#   id file pattern nth replacement test_file filter expected
# For each line the tool copies the tree, rewrites the nth line of <file>
# containing the fixed string <pattern> by replacing that string with
# <replacement> (keeping the surrounding syntax), requires `bash -n` to pass,
# then runs `bats --filter <filter> <test_file>`. The pin holds only if that run
# is red and its output contains <expected>, a fragment of the failing assertion.
# An unmutated run of the same filter must be green first, so a case that is
# already red cannot pass as a pin. A pin that cannot be made red is a finding.
#
# --list prints each resolved site and exits. --affected prints `yes` when a
# path changed since <base-ref> is in the manifest's file column (or is the
# manifest or this tool), else `no`.
#
# Exit: 0 all pins red, 1 a pin failed or a row is unusable, 2 usage.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
manifest=tests/trust-pins.tsv
jobs=${TRUST_MUTATE_JOBS:-4}
mode=run
base=
ids=()

usage() {
  echo "usage: $0 [--manifest <tsv>] [--jobs <n>] [--list] [ID...]" >&2
  echo "       $0 --affected <base-ref>" >&2
  exit 2
}

while (($#)); do
  case $1 in
  --manifest)
    (($# >= 2)) || usage
    manifest=$2
    shift 2
    ;;
  --jobs)
    (($# >= 2)) || usage
    jobs=$2
    shift 2
    ;;
  --list)
    mode=list
    shift
    ;;
  --affected)
    (($# >= 2)) || usage
    mode=affected
    base=$2
    shift 2
    ;;
  -h | --help) usage ;;
  -*) usage ;;
  *)
    ids+=("$1")
    shift
    ;;
  esac
done
[[ $jobs =~ ^[1-9][0-9]*$ ]] || usage

cd "$root"
[ -f "$manifest" ] || {
  echo "trust-mutate: no manifest at $manifest" >&2
  exit 1
}

# Parsed rows, index-aligned.
r_id=() r_file=() r_pat=() r_nth=() r_repl=() r_tfile=() r_filter=() r_expect=()
while IFS=$'\t' read -r id file pat nth repl tfile filter expect extra || [ -n "${id:-}" ]; do
  [ -n "$id" ] && [[ $id != '#'* ]] || continue
  if [ -n "${extra:-}" ] || [ -z "${expect:-}" ] || [[ ! $nth =~ ^[1-9][0-9]*$ ]]; then
    echo "trust-mutate: malformed manifest line for ${id}: want 8 tab-separated columns and a positive nth" >&2
    exit 1
  fi
  r_id+=("$id") r_file+=("$file") r_pat+=("$pat") r_nth+=("$nth") r_repl+=("$repl")
  r_tfile+=("$tfile") r_filter+=("$filter") r_expect+=("$expect")
done <"$manifest"
((${#r_id[@]})) || {
  echo "trust-mutate: $manifest has no rows" >&2
  exit 1
}

if [ "$mode" = affected ]; then
  changed="$(git diff --name-only "$base"...HEAD)" || {
    echo yes
    exit 0
  }
  watched="$manifest"$'\n'"scripts/trust-mutate.sh"$'\n'"$(printf '%s\n' "${r_file[@]}")"
  if grep -qxFf <(printf '%s\n' "$watched") <<<"$changed"; then echo yes; else echo no; fi
  exit 0
fi

# site_line <file> <pattern> <nth>: the line number of the nth line containing
# the fixed string, or nothing.
site_line() {
  awk -v pat="$2" -v nth="$3" 'index($0, pat) { if (++n == nth) { print NR; exit } }' "$1"
}

selected() {
  local want
  ((${#ids[@]})) || return 0
  for want in "${ids[@]}"; do [ "$want" = "$1" ] && return 0; done
  return 1
}

if [ "$mode" = list ]; then
  rc=0
  for i in "${!r_id[@]}"; do
    selected "${r_id[$i]}" || continue
    ln="$(site_line "${r_file[$i]}" "${r_pat[$i]}" "${r_nth[$i]}")"
    if [ -z "$ln" ]; then
      echo "${r_id[$i]}: NOT FOUND ${r_file[$i]} (nth ${r_nth[$i]}) ${r_pat[$i]}" >&2
      rc=1
      continue
    fi
    printf '%s\t%s:%s\t%s\n' "${r_id[$i]}" "${r_file[$i]}" "$ln" "$(sed -n "${ln}p" "${r_file[$i]}" | sed 's/^[[:space:]]*//')"
  done
  exit "$rc"
fi

start=$SECONDS
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT TERM

# copy_tree <dest>: the tracked and untracked-but-not-ignored files, so a pin
# authored in the working tree is checked before it is committed.
copy_tree() {
  mkdir -p "$1"
  git ls-files -z --cached --others --exclude-standard | tar --null -T - -cf - | tar -xf - -C "$1"
}

# run_bats <dir> <test_file> <filter> <out>: TAP into <out>; returns bats' status.
run_bats() {
  (cd "$1" && timeout --foreground "${TRUST_MUTATE_TIMEOUT:-1200}" bats --tap --filter "$3" "$2") >"$4" 2>&1
}

# Baselines: one unmutated run per distinct (test file, filter).
declare -A base_key=()
for i in "${!r_id[@]}"; do
  selected "${r_id[$i]}" || continue
  base_key["${r_tfile[$i]}"$'\t'"${r_filter[$i]}"]=1
done

declare -A base_res=()
pids=()
n=0
for k in "${!base_key[@]}"; do
  n=$((n + 1))
  (
    d="$work/base-$n"
    copy_tree "$d/t"
    rc=0
    run_bats "$d/t" "${k%%$'\t'*}" "${k#*$'\t'}" "$d/out" || rc=$?
    ok="$(grep -c '^ok ' "$d/out" || true)"
    if [ "$rc" -eq 0 ] && [ "$ok" -ge 1 ]; then
      echo green >"$d/res"
    else
      echo "red rc=$rc ok=$ok" >"$d/res"
    fi
  ) &
  pids+=("$!")
  if ((${#pids[@]} >= jobs)); then
    wait -n || true
    pids=("${pids[@]:1}")
  fi
  base_res["$k"]="$work/base-$n"
done
wait || true

# mutate_one <index>: writes <work>/mut-<index>/res = "<verdict> <detail>".
mutate_one() {
  local i=$1 d="$work/mut-$1" ln line pre post rc=0
  mkdir -p "$d"
  copy_tree "$d/t"
  local target="$d/t/${r_file[$i]}"
  ln="$(site_line "$target" "${r_pat[$i]}" "${r_nth[$i]}")"
  if [ -z "$ln" ]; then
    echo "unusable site not found: nth ${r_nth[$i]} line containing '${r_pat[$i]}' in ${r_file[$i]}" >"$d/res"
    return
  fi
  line="$(sed -n "${ln}p" "$target")"
  pre="${line%%"${r_pat[$i]}"*}"
  post="${line#*"${r_pat[$i]}"}"
  NEW="$pre${r_repl[$i]}$post" awk -v ln="$ln" 'NR == ln { print ENVIRON["NEW"]; next } { print }' "$target" >"$target.mut"
  if cmp -s "$target" "$target.mut"; then
    echo "unusable replacement changes nothing at ${r_file[$i]}:$ln" >"$d/res"
    return
  fi
  if ! bash -n "$target.mut" 2>"$d/syntax"; then
    echo "unusable mutated ${r_file[$i]}:$ln fails bash -n: $(head -n1 "$d/syntax")" >"$d/res"
    return
  fi
  cat "$target.mut" >"$target"
  rm -f "$target.mut"
  run_bats "$d/t" "${r_tfile[$i]}" "${r_filter[$i]}" "$d/out" || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "green ${r_file[$i]}:$ln stayed green: the guard is not pinned by ${r_tfile[$i]} --filter '${r_filter[$i]}'" >"$d/res"
  elif grep -vE '^(not )?ok ' "$d/out" | grep -qF -- "${r_expect[$i]}"; then
    echo "red ${r_file[$i]}:$ln" >"$d/res"
  else
    echo "wrong red ${r_file[$i]}:$ln, but the output never shows the expected assertion '${r_expect[$i]}'; failed instead: $(grep -E '^#.*(failed|\(in test)' "$d/out" | head -n4 | tr '\n' ' ')" >"$d/res"
  fi
}

pids=()
for i in "${!r_id[@]}"; do
  selected "${r_id[$i]}" || continue
  k="${r_tfile[$i]}"$'\t'"${r_filter[$i]}"
  if [ "$(cat "${base_res[$k]}/res")" != green ]; then
    mkdir -p "$work/mut-$i"
    echo "unusable baseline run of ${r_tfile[$i]} --filter '${r_filter[$i]}' is not green: $(cat "${base_res[$k]}/res")" >"$work/mut-$i/res"
    continue
  fi
  mutate_one "$i" &
  pids+=("$!")
  if ((${#pids[@]} >= jobs)); then
    wait -n || true
    pids=("${pids[@]:1}")
  fi
done
wait || true

red=0 bad=0 total=0
ran=()
for i in "${!r_id[@]}"; do
  selected "${r_id[$i]}" || continue
  total=$((total + 1))
  ran+=("${r_id[$i]}")
  res="$(cat "$work/mut-$i/res")"
  if [[ $res == red\ * ]]; then
    red=$((red + 1))
    printf 'red     %-5s %s\n' "${r_id[$i]}" "${res#red }"
  else
    bad=$((bad + 1))
    printf 'FAIL    %-5s %s\n' "${r_id[$i]}" "$res"
  fi
done
distinct="$(printf '%s\n' "${ran[@]}" | sort -u | wc -l)"
printf 'trust-mutate: %d mutations (%d rows): %d red, %d failed, %ds\n' "$total" "$distinct" "$red" "$bad" "$((SECONDS - start))"
((bad == 0))
