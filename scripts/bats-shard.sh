#!/usr/bin/env bash
# Shard !timing bats cases across N bins.
#
#   bats-shard.sh <shard> <total> [tests-dir]
#   bats-shard.sh --plan [tests-dir]
#   bats-shard.sh --check [tests-dir]
#
# --weights <path> overrides tests/shard-weights.tsv. A missing default file
# falls back to bats --count per file. When a file has weights but a !timing
# family has no row, that family is still scheduled: its weight is the median
# per-case rate of the file's other same-filter families (weight_ms / cases,
# integer division; even counts take the lower middle) times this family's
# case count, at least 1ms. With no sibling rate, the weight is the case
# count. A warning names that fallback and scripts/bats-shard-weights.sh.
# An uncovered timing family warns that it has no weight and is not scheduled
# (regenerate with the same script). Stale rows (no matching case, or a file
# outside the tests dir) warn and are skipped. --check warns on that staleness
# and still exits 0. --plan prints the 4-shard CI assignment.
# Shard stdout is one unit per line: a bats path, or path<TAB>regex when only
# some families of that file land in the shard.
set -euo pipefail

usage() {
  cat >&2 <<EOF
usage: $0 <shard> <total> [tests-dir]
       $0 --plan [tests-dir]
       $0 --check [tests-dir]
       $0 --weights <path> <shard> <total> [tests-dir]
EOF
  exit 2
}

ere_escape() {
  local s=$1 i c out='' bs
  bs=$'\\'
  for ((i = 0; i < ${#s}; i++)); do
    c=${s:i:1}
    if [[ $c == '.' || $c == '*' || $c == '^' || $c == '$' || $c == '+' || $c == '?' ||
      $c == '(' || $c == ')' || $c == '[' || $c == ']' || $c == '{' || $c == '}' ||
      $c == '|' || $c == "$bs" ]]; then
      out+=\\"$c"
    else
      out+=$c
    fi
  done
  printf '%s' "$out"
}

# Named families become ^(fam1|fam2): . A "*" family (no colon in the name)
# is ^[^:]+$ so a split file can still select those cases.
family_filter() {
  local fam esc star=0 regex='' joined=''
  local -a named=()
  local -a ordered=()
  mapfile -t ordered < <(printf '%s\n' "$@" | LC_ALL=C sort -u)
  for fam in "${ordered[@]}"; do
    if [[ $fam == '*' ]]; then
      star=1
      continue
    fi
    esc=$(ere_escape "$fam")
    named+=("$esc")
  done
  if ((${#named[@]})); then
    joined=$(printf '%s|' "${named[@]}")
    joined=${joined%|}
    regex="^(${joined}):"
  fi
  if ((star)); then
    if [[ -n $regex ]]; then
      regex+='|^[^:]+$'
    else
      regex='^[^:]+$'
    fi
  fi
  printf '%s' "$regex"
}

# Plain families go through family_filter; a split part ("fam#j/k") adds an
# exact-name alternation of its cases.
unit_filter() {
  local file=$1 tok name esc regex alt=''
  shift
  local -a plain=()
  local -a names=()
  for tok in "$@"; do
    if [[ -n ${part_names[$file$'\x1f'$tok]+x} ]]; then
      mapfile -t names < <(printf '%s' "${part_names[$file$'\x1f'$tok]}")
      for name in "${names[@]}"; do
        esc=$(ere_escape "$name")
        alt+="$esc|"
      done
    else
      plain+=("$tok")
    fi
  done
  regex=
  if ((${#plain[@]})); then
    regex=$(family_filter "${plain[@]}")
  fi
  if [[ -n $alt ]]; then
    [[ -z $regex ]] || regex+='|'
    regex+="^(${alt%|})\$"
  fi
  printf '%s' "$regex"
}

unit_id() {
  local file=$1 family=$2
  if [[ $family == '*' ]]; then
    printf '%s' "$file"
  else
    printf '%s^(%s):' "$file" "$family"
  fi
}

weights=tests/shard-weights.tsv
weights_explicit=0
mode=shard
positional=()
while (($#)); do
  case $1 in
  --weights)
    (($# >= 2)) || usage
    weights=$2
    weights_explicit=1
    shift 2
    ;;
  --plan)
    [[ $mode == shard ]] || usage
    mode=plan
    shift
    ;;
  --check)
    [[ $mode == shard ]] || usage
    mode=check
    shift
    ;;
  --)
    shift
    positional+=("$@")
    break
    ;;
  -*)
    usage
    ;;
  *)
    positional+=("$1")
    shift
    ;;
  esac
done

shard=1
total=4
tests_dir=tests
case $mode in
shard)
  ((${#positional[@]} == 2 || ${#positional[@]} == 3)) || usage
  shard=${positional[0]}
  total=${positional[1]}
  if ((${#positional[@]} == 3)); then
    tests_dir=${positional[2]}
  fi
  [[ $shard =~ ^[1-9][0-9]*$ && $total =~ ^[1-9][0-9]*$ ]] || usage
  ((shard <= total)) || usage
  ;;
plan | check)
  ((${#positional[@]} <= 1)) || usage
  if ((${#positional[@]} == 1)); then
    tests_dir=${positional[0]}
  fi
  ;;
*)
  usage
  ;;
esac

mapfile -t files < <(find "$tests_dir" -maxdepth 1 -name '*.bats' ! -name module.bats -print | LC_ALL=C sort)

declare -A discovered
for file in "${files[@]}"; do
  discovered[$file]=1
done

# Weight rows are repo-relative (tests/...); a discovered path may be absolute
# when tests-dir is passed absolute. Resolve each row to its discovered path.
resolve_weight_file() {
  local wfile=$1 d
  if [[ -n ${discovered[$wfile]+x} ]]; then
    printf '%s' "$wfile"
    return 0
  fi
  for d in "${files[@]}"; do
    if [[ $d == */"$wfile" ]]; then
      printf '%s' "$d"
      return 0
    fi
  done
  return 1
}

declare -A weight_ms file_has_weight
weight_keys=()
unresolved_weights=()
if [[ ! -f $weights ]]; then
  if ((weights_explicit)); then
    printf 'bats-shard: weights file not found: %s\n' "$weights" >&2
    exit 1
  fi
else
  seen_header=0
  while IFS= read -r line || [[ -n $line ]]; do
    [[ -n $line ]] || continue
    if ((seen_header == 0)); then
      [[ $line == $'file\tfamily\tfilter\tweight_ms' ]] || {
        printf 'bats-shard: invalid weights header\n' >&2
        exit 1
      }
      seen_header=1
      continue
    fi
    IFS=$'\t' read -r -a cols <<<"$line"
    if ((${#cols[@]} != 4)); then
      printf 'bats-shard: invalid weights row: %s\n' "$line" >&2
      exit 1
    fi
    wfile=${cols[0]}
    wfam=${cols[1]}
    wfilter=${cols[2]}
    wms=${cols[3]}
    if [[ $wfilter != '!timing' && $wfilter != timing ]]; then
      printf 'bats-shard: invalid weights filter: %s\n' "$line" >&2
      exit 1
    fi
    if [[ ! $wms =~ ^[0-9]+$ ]]; then
      printf 'bats-shard: invalid weights ms: %s\n' "$line" >&2
      exit 1
    fi
    if resolved=$(resolve_weight_file "$wfile"); then
      wkey=$(printf '%s\x1f%s\x1f%s' "$resolved" "$wfam" "$wfilter")
      if [[ -n ${weight_ms[$wkey]+x} ]]; then
        printf 'bats-shard: duplicate weights row: %s / %s / %s\n' "$wfile" "$wfam" "$wfilter" >&2
        exit 1
      fi
      weight_ms[$wkey]=$wms
      file_has_weight[$resolved]=1
      weight_keys+=("$wkey")
    else
      unresolved_weights+=("$wfile / $wfam / $wfilter")
    fi
  done <"$weights"
  if ((seen_header == 0)); then
    printf 'bats-shard: invalid weights header\n' >&2
    exit 1
  fi
fi

declare -A present all_count keep_count timing_count family_count
present_keys=()
keep_tests=()
errors=()

inventory=
if ((${#files[@]})); then
  inventory=$(awk '
    function has_timing(tags,    n, i, parts) {
      if (tags == "") return 0
      n = split(tags, parts, ",")
      for (i = 1; i <= n; i++) {
        gsub(/^[ \t]+|[ \t]+$/, "", parts[i])
        if (parts[i] == "timing") return 1
      }
      return 0
    }
    FNR == 1 { file_tags = ""; test_tags = "" }
    /^# bats file_tags=/ {
      file_tags = $0
      sub(/^# bats file_tags=/, "", file_tags)
      next
    }
    /^# bats test_tags=/ {
      test_tags = $0
      sub(/^# bats test_tags=/, "", test_tags)
      next
    }
    /^@test / {
      if ($0 !~ /^@test ".*" \{[[:space:]]*$/) {
        printf "bats-shard: unsupported @test syntax at %s:%d\n", FILENAME, FNR > "/dev/stderr"
        exit 1
      }
      name = $0
      sub(/^@test "/, "", name)
      sub(/" \{[[:space:]]*$/, "", name)
      if (index(name, "\t") || index(name, "\r")) {
        printf "bats-shard: unsafe test name at %s:%d\n", FILENAME, FNR > "/dev/stderr"
        exit 1
      }
      tags = file_tags
      if (test_tags != "") tags = (tags == "" ? test_tags : tags "," test_tags)
      filter = has_timing(tags) ? "timing" : "!timing"
      family = "*"
      colon = index(name, ":")
      if (colon > 0) family = substr(name, 1, colon - 1)
      printf "%s\t%s\t%s\t%s\n", FILENAME, family, filter, name
      test_tags = ""
    }
  ' "${files[@]}")
fi

if [[ -n $inventory ]]; then
  while IFS=$'\t' read -r file family filter name; do
    [[ -n $file ]] || continue
    all_count[$file]=$((${all_count[$file]:-0} + 1))
    pkey=$(printf '%s\x1f%s\x1f%s' "$file" "$family" "$filter")
    if [[ -z ${present[$pkey]+x} ]]; then
      present[$pkey]=1
      present_keys+=("$pkey")
    fi
    family_count[$pkey]=$((${family_count[$pkey]:-0} + 1))
    if [[ $filter == '!timing' ]]; then
      keep_count[$file]=$((${keep_count[$file]:-0} + 1))
      keep_tests+=("${file}"$'\x1f'"${family}"$'\x1f'"${name}")
    else
      timing_count[$file]=$((${timing_count[$file]:-0} + 1))
    fi
  done <<<"$inventory"
fi

declare -A unit_weight unit_file unit_family unit_shard
unit_ids=()

add_unit() {
  local file=$1 family=$2 weight=$3 id
  id=$(unit_id "$file" "$family")
  if [[ -n ${unit_weight[$id]+x} ]]; then
    errors+=("duplicate unit $id")
    return
  fi
  unit_weight[$id]=$weight
  unit_file[$id]=$file
  unit_family[$id]=$family
  unit_ids+=("$id")
}

# Same-file, same-filter rates only. Even counts take the lower middle
# (10 and 30 → 10). No sibling rate → the family's own case count.
fallback_weight() {
  local file=$1 filter=$2 count=$3
  local -a rates=() sorted=()
  local wkey wfile wfam wfilter n wms idx rate weight
  for wkey in "${weight_keys[@]+"${weight_keys[@]}"}"; do
    IFS=$'\x1f' read -r wfile wfam wfilter <<<"$wkey"
    [[ $wfile == "$file" && $wfilter == "$filter" ]] || continue
    [[ -n ${present[$wkey]+x} ]] || continue
    n=${family_count[$wkey]:-0}
    ((n > 0)) || continue
    wms=${weight_ms[$wkey]}
    rates+=("$((10#$wms / n))")
  done
  if ((${#rates[@]} == 0)); then
    printf '%s' "$count"
    return
  fi
  mapfile -t sorted < <(printf '%s\n' "${rates[@]}" | LC_ALL=C sort -n)
  idx=$(((${#sorted[@]} - 1) / 2))
  rate=${sorted[$idx]}
  weight=$((rate * count))
  if ((weight < 1)); then
    weight=1
  fi
  printf '%s' "$weight"
}

for unresolved in "${unresolved_weights[@]+"${unresolved_weights[@]}"}"; do
  printf 'bats-shard: warning: weights row names no case (stale): %s\n' "$unresolved" >&2
done

for wkey in "${weight_keys[@]+"${weight_keys[@]}"}"; do
  IFS=$'\x1f' read -r wfile wfam wfilter <<<"$wkey"
  if [[ -z ${present[$wkey]+x} ]]; then
    printf 'bats-shard: warning: weights row names no case (stale): %s / %s / %s\n' "$wfile" "$wfam" "$wfilter" >&2
  fi
done

for pkey in "${present_keys[@]+"${present_keys[@]}"}"; do
  IFS=$'\x1f' read -r pfile pfam pfilter <<<"$pkey"
  [[ -n ${file_has_weight[$pfile]+x} ]] || continue
  [[ -z ${weight_ms[$pkey]+x} ]] || continue
  if [[ $pfilter != '!timing' ]]; then
    printf 'bats-shard: warning: no weight for %s / %s (%s); not scheduled — regenerate with scripts/bats-shard-weights.sh\n' \
      "$pfile" "$pfam" "$pfilter" >&2
    continue
  fi
  n=${family_count[$pkey]:-0}
  fb=$(fallback_weight "$pfile" "$pfilter" "$n")
  printf 'bats-shard: warning: no weight for %s / %s (%s); using fallback %sms — regenerate with scripts/bats-shard-weights.sh\n' \
    "$pfile" "$pfam" "$pfilter" "$fb" >&2
  add_unit "$pfile" "$pfam" "$fb"
done

for file in "${files[@]}"; do
  if [[ -n ${file_has_weight[$file]+x} ]]; then
    continue
  fi
  printf 'bats-shard: warning: no weights for %s; using bats --count\n' "$file" >&2
  if ((${keep_count[$file]:-0} > 0)); then
    count_weight=$(bats --count "$file")
    add_unit "$file" '*' "$count_weight"
  fi
done

for wkey in "${weight_keys[@]+"${weight_keys[@]}"}"; do
  IFS=$'\x1f' read -r wfile wfam wfilter <<<"$wkey"
  [[ $wfilter == '!timing' ]] || continue
  [[ -n ${discovered[$wfile]+x} && -n ${present[$wkey]+x} ]] || continue
  add_unit "$wfile" "$wfam" "${weight_ms[$wkey]}"
done

# A family heavier than 40% of an even shard budget (and at least a minute)
# cannot balance as one unit, so it is cut into parts of at most that weight.
# Without per-test weights the parts split the family's cases round-robin by position and share its weight
# in proportion to their case counts. A part's regex lists its names exactly.
declare -A family_names part_names test_part
for entry in "${keep_tests[@]+"${keep_tests[@]}"}"; do
  IFS=$'\x1f' read -r tfile tfam tname <<<"$entry"
  if [[ -z ${file_has_weight[$tfile]+x} ]]; then
    tfam='*'
  fi
  family_names[$tfile$'\x1f'$tfam]+="$tname"$'\n'
done

split_unit() {
  local id=$1 file family weight parts n i j cnt rem name
  local -a names=()
  local -a part_cnt=()
  file=${unit_file[$id]}
  family=${unit_family[$id]}
  weight=${unit_weight[$id]}
  mapfile -t names < <(printf '%s' "${family_names[$file$'\x1f'$family]:-}")
  n=${#names[@]}
  ((n >= 2)) || return 0
  parts=$(((10#$weight + split_threshold - 1) / split_threshold))
  ((parts <= n)) || parts=$n
  ((parts >= 2)) || return 0
  rem=$weight
  for ((j = 1; j <= parts; j++)); do
    part_cnt[j]=0
  done
  for ((i = 0; i < n; i++)); do
    name=${names[i]}
    if [[ -z ${test_part[$file$'\x1f'$family$'\x1f'$name]+x} ]]; then
      j=$((i % parts + 1))
      test_part[$file$'\x1f'$family$'\x1f'$name]="$family#$j/$parts"
      part_names[$file$'\x1f'"$family#$j/$parts"]+="$name"$'\n'
      part_cnt[j]=$((part_cnt[j] + 1))
    fi
  done
  unset 'unit_weight[$id]' 'unit_file[$id]' 'unit_family[$id]'
  for ((j = 1; j <= parts; j++)); do
    if ((j == parts)); then
      cnt=$rem
    else
      cnt=$((10#$weight * part_cnt[j] / n))
      rem=$((rem - cnt))
    fi
    add_unit "$file" "$family#$j/$parts" "$cnt"
  done
  split_dropped+=("$id")
}

split_dropped=()
split_total=0
for id in "${unit_ids[@]+"${unit_ids[@]}"}"; do
  split_total=$((split_total + 10#${unit_weight[$id]}))
done
split_threshold=$((split_total * 2 / (total * 5)))
((split_threshold >= 60000)) || split_threshold=60000
if ((split_threshold > 0)); then
  for id in "${unit_ids[@]+"${unit_ids[@]}"}"; do
    [[ -n ${file_has_weight[${unit_file[$id]}]+x} ]] || continue
    if ((10#${unit_weight[$id]} > split_threshold)); then
      split_unit "$id"
    fi
  done
  if ((${#split_dropped[@]})); then
    kept=()
    for id in "${unit_ids[@]}"; do
      [[ -n ${unit_weight[$id]+x} ]] && kept+=("$id")
    done
    unit_ids=("${kept[@]}")
  fi
fi

for entry in "${keep_tests[@]+"${keep_tests[@]}"}"; do
  IFS=$'\x1f' read -r tfile tfam tname <<<"$entry"
  if [[ -z ${file_has_weight[$tfile]+x} ]]; then
    tfam='*'
  fi
  tid=$(unit_id "$tfile" "$tfam")
  if [[ -n ${test_part[$tfile$'\x1f'$tfam$'\x1f'$tname]+x} ]]; then
    tid=$(unit_id "$tfile" "${test_part[$tfile$'\x1f'$tfam$'\x1f'$tname]}")
  fi
  if [[ -z ${unit_weight[$tid]+x} ]]; then
    errors+=("omitted case: $tfile / $tname")
  fi
done

if [[ $mode == check ]]; then
  for file in "${files[@]}"; do
    actual=$(bats --count "$file")
    if [[ ${all_count[$file]:-0} -ne $actual ]]; then
      errors+=("$file has $actual bats cases but ${all_count[$file]:-0} parsed")
    fi
    actual=$(bats --count --filter-tags '!timing' "$file")
    if [[ ${keep_count[$file]:-0} -ne $actual ]]; then
      errors+=("$file !timing count ${keep_count[$file]:-0} differs from bats --count $actual")
    fi
    actual=$(bats --count --filter-tags timing "$file")
    if [[ ${timing_count[$file]:-0} -ne $actual ]]; then
      errors+=("$file timing count ${timing_count[$file]:-0} differs from bats --count $actual")
    fi
  done
fi

if ((${#errors[@]})); then
  for err in "${errors[@]}"; do
    printf 'bats-shard: %s\n' "$err" >&2
  done
  exit 1
fi

if [[ $mode == check ]]; then
  exit 0
fi

if ((${#unit_ids[@]} == 0)); then
  exit 0
fi

declare -a shard_load
for ((i = 1; i <= total; i++)); do
  shard_load[i]=0
done

mapfile -t ordered < <(
  for id in "${unit_ids[@]}"; do
    printf '%s\t%s\n' "${unit_weight[$id]}" "$id"
  done | LC_ALL=C sort -t $'\t' -k1,1nr -k2,2 | cut -f2-
)

for id in "${ordered[@]}"; do
  target=1
  for ((i = 2; i <= total; i++)); do
    if ((shard_load[i] < shard_load[target])); then
      target=$i
    fi
  done
  unit_shard[$id]=$target
  shard_load[target]=$((shard_load[target] + 10#${unit_weight[$id]}))
done

if [[ $mode == plan ]]; then
  printf 'shard\tunit\tfile\tfamily\tweight_ms\tshard_weight_ms\n'
  while IFS=$'\t' read -r sh id; do
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$sh" "$id" "${unit_file[$id]}" "${unit_family[$id]}" \
      "${unit_weight[$id]}" "${shard_load[$sh]}"
  done < <(
    for id in "${unit_ids[@]}"; do
      printf '%s\t%s\n' "${unit_shard[$id]}" "$id"
    done | LC_ALL=C sort -t $'\t' -k1,1n -k2,2
  )
  exit 0
fi

declare -A shard_families all_families
for id in "${unit_ids[@]}"; do
  file=${unit_file[$id]}
  family=${unit_family[$id]}
  all_families[$file]+="$family"$'\n'
  if [[ ${unit_shard[$id]} -eq $shard ]]; then
    shard_families[$file]+="$family"$'\n'
  fi
done

if ((${#shard_families[@]} == 0)); then
  exit 0
fi

while IFS= read -r file; do
  mine=$(printf '%s' "${shard_families[$file]}" | sed '/^$/d' | LC_ALL=C sort -u)
  all=$(printf '%s' "${all_families[$file]}" | sed '/^$/d' | LC_ALL=C sort -u)
  if [[ $mine == "$all" ]]; then
    printf '%s\n' "$file"
    continue
  fi
  mapfile -t fams <<<"$mine"
  printf '%s\t%s\n' "$file" "$(unit_filter "$file" "${fams[@]}")"
done < <(printf '%s\n' "${!shard_families[@]}" | LC_ALL=C sort)

exit 0
