#!/usr/bin/env bash
# refresh-harness — report new engine harness features the dispatcher does not
# use yet.
#
# Per enabled engine (`dispatch --engines`) it diffs the installed CLI's help
# flags (and, for claude, the CLAUDE_CODE_* env names baked into the binary)
# against the previous run's snapshot, and reads the engine's official
# changelog past a stored cursor. Every new name is classified against the
# dispatcher tree as used / unused / unknown, so the owner can triage.
#
# Writes under ${XDG_DATA_HOME:-~/.local/share}/crew: harness-state.json (next
# run's baseline), harness-report.json and .md (this run), harness-history.jsonl
# (one line per run that found anything). A first run only baselines. When new
# names are unused it posts a `crew:harness` bus msg to the live dispatchers.
# Network and probe failures warn and the run continues; exit 1 only when
# state/report cannot be written, 2 on bad usage.
set -euo pipefail

OUT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/crew"
STATE="$OUT_DIR/harness-state.json"
REPORT="$OUT_DIR/harness-report.json"
REPORT_MD="$OUT_DIR/harness-report.md"
HISTORY="$OUT_DIR/harness-history.jsonl"

TREE="${REFRESH_HARNESS_TREE:-@harnessTree@}"
# Written split so the flake's textual substitution of the token cannot make
# this comparison always false.
if [[ $TREE == @harness""Tree@ ]]; then
  TREE=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
fi

ALL_ENGINES="claude codex cursor pi"
CLAUDE_CHANGELOG_URL="https://raw.githubusercontent.com/anthropics/claude-code/main/CHANGELOG.md"
KEYWORDS='subagent|fork|team|effort|reasoning|hook|permission|mcp|resume|headless|print|worktree|model|environment variable|env var'

WORK=""
OLD=""
NEW=""
TREE_OK=1
RES_BIN=""
RES_PREFIX=""
CL_STATUS=""
CL_REASON=""
CL_CURSOR=""

usage() {
  cat <<'EOF'
usage: refresh-harness [-h|--help]

Diff each enabled engine's CLI flags, claude's env names and the engines'
changelogs against the previous run and classify what is new against the
dispatcher tree. Reads and writes under ${XDG_DATA_HOME:-~/.local/share}/crew.
EOF
}

warn() {
  printf 'refresh-harness: %s\n' "$*" >&2
  printf '%s\n' "$*" >>"$WORK/warnings.txt"
}

note() { printf '%s\n' "$*" >>"$WORK/notes.txt"; }

die() {
  printf 'refresh-harness: %s\n' "$*" >&2
  exit 1
}

write_atomic() {
  local dest=$1 tmp
  tmp=$(mktemp "$dest.XXXXXX") || die "cannot write $dest"
  if cat >"$tmp" && chmod 644 "$tmp" && mv -f "$tmp" "$dest"; then
    return 0
  fi
  rm -f "$tmp"
  die "cannot write $dest"
}

state_set_snapshot() {
  jq -c --arg e "$1" --slurpfile s "$2" '.engines[$e] = $s[0]' "$NEW" >"$NEW.tmp" && mv "$NEW.tmp" "$NEW"
}

state_set_cursor() {
  jq -c --arg id "$1" --arg c "$2" '.sources[$id] = {cursor: $c}' "$NEW" >"$NEW.tmp" && mv "$NEW.tmp" "$NEW"
}

load_state() {
  OLD="$WORK/old.json"
  NEW="$WORK/new.json"
  if [[ -f $STATE ]] && jq -e 'type == "object"' "$STATE" >/dev/null 2>&1; then
    jq -c '{engines: (.engines // {}), sources: (.sources // {})}' "$STATE" >"$OLD"
  else
    if [[ -f $STATE ]]; then warn "state file unreadable; baselining again"; fi
    echo '{"engines":{},"sources":{}}' >"$OLD"
  fi
  cp "$OLD" "$NEW"
}

cli_for() {
  case $1 in
  claude | codex | pi) echo "$1" ;;
  cursor) echo cursor-agent ;;
  *) return 1 ;;
  esac
}

# --- tree token sets --------------------------------------------------------

find_files() {
  local dir=$1
  shift
  if [[ -d $TREE/$dir ]]; then find "$TREE/$dir" "$@" -type f; fi
}

list_code_files() {
  find_files adapters/core -maxdepth 1 \( -name '*.sh' -o -name '*.json' \)
  find_files adapters/core/lane-profiles
  find_files adapters/core/reviewers -maxdepth 1 -name '*.sh'
  find_files adapters/claude-code/plugin/hooks
  find_files adapters/claude-code/plugin/scripts
  find_files adapters/pi/extensions
  find_files adapters/pi/src
  find_files nix
  find_files crew -name '*.go' ! -name '*_test.go'
  if [[ -f $TREE/flake.nix ]]; then echo "$TREE/flake.nix"; fi
}

build_tree_sets() {
  local f d
  if [[ ! -d $TREE ]]; then
    warn "tree $TREE is not readable; every name is unknown"
    TREE_OK=0
    return 0
  fi
  list_code_files >"$WORK/code-files.txt"
  : >"$WORK/code.txt"
  while IFS= read -r f; do
    grep -hIvE '^[[:space:]]*(#|//)' -- "$f" >>"$WORK/code.txt" || true
  done <"$WORK/code-files.txt"
  grep -hoE '[A-Z][A-Z0-9_]*' "$WORK/code.txt" | sort -u >"$WORK/code-env.txt" || true
  grep -hoE -- '--[a-z][a-z0-9-]*' "$WORK/code.txt" | sort -u >"$WORK/code-flags.txt" || true
  : >"$WORK/prose-raw-env.txt"
  : >"$WORK/prose-raw-flags.txt"
  for d in adapters/core/protocols adapters/core/skills adapters/core/commands docs; do
    if [[ -d $TREE/$d ]]; then
      grep -rIhoE '[A-Z][A-Z0-9_]*' "$TREE/$d" >>"$WORK/prose-raw-env.txt" || true
      grep -rIhoE -- '--[a-z][a-z0-9-]*' "$TREE/$d" >>"$WORK/prose-raw-flags.txt" || true
    fi
  done
  sort -u "$WORK/prose-raw-env.txt" >"$WORK/prose-env.txt"
  sort -u "$WORK/prose-raw-flags.txt" >"$WORK/prose-flags.txt"
}

# Flags on a code line that also names the engine's CLI word: the launch line.
build_launch_flags() {
  local engine=$1 cli=$2
  : >"$WORK/launch-$engine.txt"
  if ((TREE_OK == 0)); then return 0; fi
  grep -E "(^|[^A-Za-z0-9_-])${cli}([^A-Za-z0-9_-]|\$)" "$WORK/code.txt" |
    grep -oE -- '--[a-z][a-z0-9-]*' | sort -u >"$WORK/launch-$engine.txt" || true
}

in_set() { grep -qxF -e "$1" "$2"; }

# classify_name <engine> <name> — prints used, unused or unknown.
classify_name() {
  local engine=$1 name=$2
  if ((TREE_OK == 0)); then
    echo unknown
    return 0
  fi
  if [[ $name == --* ]]; then
    if in_set "$name" "$WORK/launch-$engine.txt"; then
      echo used
    elif in_set "$name" "$WORK/code-flags.txt" || in_set "$name" "$WORK/prose-flags.txt"; then
      echo unknown
    else
      echo unused
    fi
    return 0
  fi
  if in_set "$name" "$WORK/code-env.txt"; then
    echo used
  elif in_set "$name" "$WORK/prose-env.txt"; then
    echo unknown
  else
    echo unused
  fi
}

# classify_names <engine> — names on stdin, JSON array of {name, class} out.
classify_names() {
  local engine=$1 name
  while IFS= read -r name; do
    printf '%s\t' "$name"
    classify_name "$engine" "$name"
  done | jq -Rn '[inputs | split("\t") | {name: .[0], class: .[1]}]'
}

# --- Source A: installed binary ---------------------------------------------

probe() { env -u CREW_WORKER_ID -u CREW_ID timeout 60 "$@" </dev/null 2>/dev/null; }

# collect_flags <engine> <cli> — sorted unique help flags; fails when the help
# call fails or yields none.
collect_flags() {
  local engine=$1 cli=$2 out flags
  case $engine in
  codex) out=$(probe "$cli" --help && probe "$cli" exec --help) ;;
  *) out=$(probe "$cli" --help) ;;
  esac || return 1
  flags=$(printf '%s\n' "$out" | grep -oE -- '--[a-z][a-z0-9-]+' | sort -u) || true
  [[ -n $flags ]] || return 1
  printf '%s\n' "$flags"
}

# resolve_chain <name> — follow wrapper scripts from `command -v <name>`.
# RES_BIN: the first non-#! file (empty when the chain breaks). RES_PREFIX: the
# package prefix of the last file reached.
resolve_chain() {
  local name=$1 cur cand p base r seen depth=0
  RES_BIN=""
  RES_PREFIX=""
  cur=$(command -v "$name") || return 0
  cur=$(readlink -f "$cur") || return 0
  seen="|$cur|"
  while :; do
    RES_PREFIX=$(dirname "$(dirname "$cur")")
    if [[ $(head -c 2 "$cur") != '#!' ]]; then
      RES_BIN=$cur
      return 0
    fi
    depth=$((depth + 1))
    if ((depth > 8)); then return 0; fi
    cand=""
    while IFS= read -r p; do
      base=${p##*/}
      [[ $base == "$name" || $base == ".$name-wrapped" ]] || continue
      r=$(readlink -f "$p" 2>/dev/null) || continue
      [[ -f $r && $seen != *"|$r|"* ]] || continue
      cand=$r
      break
    done < <(grep -aoE '/[A-Za-z0-9._+/-]+' "$cur" | tac)
    [[ -n $cand ]] || return 0
    cur=$cand
    seen+="$cur|"
  done
}

# claude_env <binary> — CLAUDE_CODE_* names, never `grep -a` over the ELF.
claude_env() {
  strings -n 8 "$1" | grep -oE 'CLAUDE_CODE_[A-Z0-9_]+' | sort -u || true
}

# snapshot_binary <engine> <cli> — writes binary-<engine>.json and advances the
# stored snapshot.
snapshot_binary() {
  local engine=$1 cli=$2
  local notes="$WORK/notes-$engine.txt" old="$WORK/old-$engine.json"
  local snap="$WORK/snap-$engine.json" diff="$WORK/diff-$engine.json"
  local version=""
  : >"$notes"
  if ! jq -e --arg e "$engine" '.engines[$e]' "$OLD" >"$old" 2>/dev/null; then rm -f "$old"; fi

  if ! collect_flags "$engine" "$cli" >"$WORK/flags-$engine.txt"; then
    warn "$engine: help probe failed; snapshot kept"
    echo "help probe failed; snapshot kept" >>"$notes"
    echo '{"status":"failed","new_env":[],"new_flags":[],"removed_env":[],"removed_flags":[]}' >"$diff"
    version=$(jq -r '.version // ""' "$old" 2>/dev/null) || version=""
    finish_binary "$engine" "$version"
    return 0
  fi
  version=$(probe "$cli" --version | awk 'NR == 1 {print $1}') || version=""

  : >"$WORK/env-$engine.txt"
  if [[ $engine == claude ]]; then
    resolve_chain claude
    if [[ -n $RES_BIN ]]; then
      claude_env "$RES_BIN" >"$WORK/env-$engine.txt"
    else
      echo "env: binary not found" >>"$notes"
      if [[ -f $old ]]; then jq -r '.env[]?' "$old" >"$WORK/env-$engine.txt"; fi
    fi
  fi
  jq -Rn '[inputs] | unique' <"$WORK/env-$engine.txt" >"$WORK/env-$engine.json"
  jq -Rn '[inputs] | unique' <"$WORK/flags-$engine.txt" >"$WORK/flags-$engine.json"
  jq -n --arg v "$version" --slurpfile e "$WORK/env-$engine.json" --slurpfile f "$WORK/flags-$engine.json" \
    '{version: $v, env: $e[0], flags: $f[0]}' >"$snap"

  if [[ -f $old ]]; then
    jq -n --slurpfile s "$snap" --slurpfile o "$old" \
      '{status: "ok",
        new_env: ($s[0].env - ($o[0].env // [])), new_flags: ($s[0].flags - ($o[0].flags // [])),
        removed_env: (($o[0].env // []) - $s[0].env), removed_flags: (($o[0].flags // []) - $s[0].flags)}' >"$diff"
  else
    echo '{"status":"baseline","new_env":[],"new_flags":[],"removed_env":[],"removed_flags":[]}' >"$diff"
  fi
  state_set_snapshot "$engine" "$snap"
  finish_binary "$engine" "$version"
}

finish_binary() {
  local engine=$1 version=$2
  jq -r '.new_env[]' "$WORK/diff-$engine.json" | classify_names "$engine" >"$WORK/ne-$engine.json"
  jq -r '.new_flags[]' "$WORK/diff-$engine.json" | classify_names "$engine" >"$WORK/nf-$engine.json"
  jq -n --arg v "$version" --slurpfile d "$WORK/diff-$engine.json" \
    --slurpfile ne "$WORK/ne-$engine.json" --slurpfile nf "$WORK/nf-$engine.json" \
    --rawfile notes "$WORK/notes-$engine.txt" \
    '{status: $d[0].status, version: $v, new_env: $ne[0], new_flags: $nf[0],
      removed_env: $d[0].removed_env, removed_flags: $d[0].removed_flags,
      notes: ($notes | split("\n") | map(select(length > 0)))}' >"$WORK/binary-$engine.json"
}

# --- Source B: changelogs ---------------------------------------------------

old_cursor() { jq -r --arg id "$1" '.sources[$id].cursor // empty' "$OLD"; }

cl_fail() {
  CL_STATUS=failed
  CL_REASON=$2
  warn "$1: $2"
}

md_versions() {
  awk '/^## / {v = $2; gsub(/[][]/, "", v); print v}' "$1"
}

md_bullets() {
  awk '/^## / {v = $2; gsub(/[][]/, "", v); next}
       /^[[:space:]]*[-*] / {
         if (v == "") next
         t = $0
         sub(/^[[:space:]]*[-*] /, "", t)
         gsub(/[\t\r]/, " ", t)
         print v "\t" substr(t, 1, 400)
       }' "$1"
}

# changelog_md <id> <file> — sets CL_*, writes entries-<id>.jsonl for the
# sections above the cursor.
changelog_md() {
  local id=$1 file=$2 cursor
  local versions="$WORK/versions-$id.txt" sel="$WORK/sel-$id.txt"
  : >"$WORK/entries-$id.jsonl"
  md_versions "$file" >"$versions"
  if [[ ! -s $versions ]]; then
    cl_fail "$id" "no version sections found"
    return 0
  fi
  CL_CURSOR=$(head -n1 "$versions")
  CL_STATUS=ok
  cursor=$(old_cursor "$id")
  if [[ -z $cursor ]]; then
    CL_STATUS=baseline
    return 0
  fi
  awk -v c="$cursor" '
    {v[NR] = $0}
    END {
      for (i = 1; i <= NR; i++) if (v[i] == c) { f = i; break }
      if (!f) { print "missing"; for (i = 1; i <= 5 && i <= NR; i++) print v[i] }
      else { print "found"; for (i = 1; i < f; i++) print v[i] }
    }' "$versions" >"$sel"
  if [[ $(head -n1 "$sel") == missing ]]; then
    warn "$id: cursor $cursor not in the changelog; taking the newest 5 sections"
  fi
  tail -n +2 "$sel" >"$sel.new"
  [[ -s $sel.new ]] || return 0
  md_bullets "$file" | awk -F'\t' 'NR == FNR {n[$1] = 1; next} $1 in n' "$sel.new" - |
    jq -Rc 'split("\t") | {version: .[0], text: (.[1:] | join(" "))}' >"$WORK/entries-$id.jsonl"
}

changelog_claude() {
  local id=claude-changelog
  : >"$WORK/entries-$id.jsonl"
  if ! curl -fsS --max-time 30 "$CLAUDE_CHANGELOG_URL" >"$WORK/claude-changelog.md" 2>/dev/null; then
    cl_fail "$id" "fetch failed"
    return 0
  fi
  changelog_md "$id" "$WORK/claude-changelog.md"
}

changelog_pi() {
  local id=pi-changelog file=""
  resolve_chain pi
  : >"$WORK/entries-$id.jsonl"
  if [[ -n $RES_PREFIX ]]; then
    file=$(find "$RES_PREFIX" -maxdepth 4 -name CHANGELOG.md 2>/dev/null | head -n1) || file=""
  fi
  if [[ -z $file ]]; then
    cl_fail "$id" "CHANGELOG.md not found in the pi package"
    return 0
  fi
  changelog_md "$id" "$file"
}

changelog_codex() {
  local id=codex-releases cursor
  : >"$WORK/entries-$id.jsonl"
  if ! gh api 'repos/openai/codex/releases?per_page=30' >"$WORK/codex.json" 2>/dev/null; then
    cl_fail "$id" "fetch failed"
    return 0
  fi
  if ! CL_CURSOR=$(jq -r '[.[] | select(.prerelease | not) | .published_at] | max // empty' "$WORK/codex.json" 2>/dev/null); then
    cl_fail "$id" "unexpected response"
    return 0
  fi
  CL_STATUS=ok
  cursor=$(old_cursor "$id")
  if [[ -z $cursor ]]; then
    CL_STATUS=baseline
    return 0
  fi
  jq -c --arg c "$cursor" '
    .[] | select(.prerelease | not) | select(.published_at > $c)
    | .tag_name as $t
    | (.body // "" | gsub("\r"; "") | split("\n")[]
       | select(test("^\\s*[-*] ")) | sub("^\\s*[-*] "; "") | .[:400])
    | {version: $t, text: .}' "$WORK/codex.json" >"$WORK/entries-$id.jsonl"
}

# finish_changelog <engine> <id> — filter and classify the entries, write
# changelog-<engine>.json, advance the cursor on success.
finish_changelog() {
  local engine=$1 id=$2 in="$WORK/entries-$2.jsonl"
  local annot="$WORK/annot-$id.jsonl" cursor
  jq -c --arg kw "$KEYWORDS" '
    . + {names: ([.text | scan("--[a-z][a-z0-9-]+"), scan("[A-Z][A-Z0-9]*_[A-Z0-9_]+")] | unique)}
    | . + {keep: ((.text | test($kw; "i")) or (.names | length > 0))}' "$in" >"$annot"
  jq -r 'select(.keep) | .names[]' "$annot" | sort -u | classify_names "$engine" >"$WORK/cls-$id.json"
  jq -c --slurpfile m "$WORK/cls-$id.json" '
    select(.keep)
    | ($m[0] | map({key: .name, value: .class}) | from_entries) as $c
    | del(.keep)
    | .names |= map({name: ., class: $c[.]})
    | .class = (if any(.names[]; .class == "unused") then "unused"
                elif (.names | length) == 0 or any(.names[]; .class == "unknown") then "unknown"
                else "used" end)' "$annot" >"$WORK/kept-$id.jsonl"
  cursor=$CL_CURSOR
  if [[ $CL_STATUS == failed ]]; then cursor=$(old_cursor "$id"); fi
  jq -n --arg id "$id" --arg status "$CL_STATUS" --arg reason "$CL_REASON" --arg cursor "$cursor" \
    --slurpfile all "$annot" --slurpfile kept "$WORK/kept-$id.jsonl" \
    '{source: $id, status: $status}
     + (if $cursor == "" then {} else {cursor: $cursor} end)
     + {filtered_out: ($all | map(select(.keep | not)) | length), entries: $kept}
     + (if $reason == "" then {} else {reason: $reason} end)' >"$WORK/changelog-$engine.json"
  if [[ $CL_STATUS != failed && -n $CL_CURSOR ]]; then
    state_set_cursor "$id" "$CL_CURSOR"
  fi
}

changelog_for() {
  local engine=$1 id
  CL_STATUS="" CL_REASON="" CL_CURSOR=""
  case $engine in
  claude) changelog_claude ;;
  codex) changelog_codex ;;
  pi) changelog_pi ;;
  cursor)
    jq -n '{source: "cursor-changelog", status: "skipped", filtered_out: 0, entries: [],
              reason: "no machine-readable source"}' >"$WORK/changelog-cursor.json"
    return 0
    ;;
  esac
  case $engine in
  claude) id=claude-changelog ;;
  codex) id=codex-releases ;;
  pi) id=pi-changelog ;;
  esac
  finish_changelog "$engine" "$id"
}

# --- engines and report -----------------------------------------------------

list_engines() {
  local out e
  if out=$(dispatch --engines 2>/dev/null); then
    printf '%s\n' "$out"
    return 0
  fi
  warn "dispatch --engines failed; using every engine CLI on PATH"
  for e in $ALL_ENGINES; do
    if command -v "$(cli_for "$e")" >/dev/null; then echo "$e"; fi
  done
}

skip_engine() {
  jq -nc --arg e "$1" --arg r "$2" '{engine: $e, reason: $r}' >>"$WORK/skipped.jsonl"
}

process_engine() {
  local engine=$1 cli
  if ! cli=$(cli_for "$engine"); then
    skip_engine "$engine" "unsupported engine"
    return 0
  fi
  if ! command -v "$cli" >/dev/null; then
    skip_engine "$engine" "not on PATH"
    return 0
  fi
  build_launch_flags "$engine" "$cli"
  snapshot_binary "$engine" "$cli"
  changelog_for "$engine"
  jq --arg e "$engine" --slurpfile b "$WORK/binary-$engine.json" --slurpfile c "$WORK/changelog-$engine.json" \
    '. + {($e): {binary: $b[0], changelog: $c[0]}}' "$WORK/engines.json" >"$WORK/engines.tmp"
  mv "$WORK/engines.tmp" "$WORK/engines.json"
}

build_report() {
  [[ -f $WORK/warnings.txt ]] || : >"$WORK/warnings.txt"
  [[ -f $WORK/notes.txt ]] || : >"$WORK/notes.txt"
  jq -n --arg generated "$GENERATED" --arg tree "$TREE" \
    --slurpfile engines "$WORK/engines.json" --slurpfile skipped "$WORK/skipped.jsonl" \
    --rawfile warnings "$WORK/warnings.txt" --rawfile notes "$WORK/notes.txt" '
    {generated_at: $generated, tree: $tree, engines: $engines[0], skipped_engines: $skipped,
     warnings: ($warnings | split("\n") | map(select(length > 0))),
     notes: ($notes | split("\n") | map(select(length > 0)))}
    | . + {summary: ([.engines[] | (.binary.new_env[], .binary.new_flags[], .changelog.entries[]) | .class]
                     | {new_unused: map(select(. == "unused")) | length,
                        new_used: map(select(. == "used")) | length,
                        new_unknown: map(select(. == "unknown")) | length})}'
}

MD_JQ=$(
  cat <<'EOF'
def items: [
  (.binary.new_env[] | {class, line: "env `\(.name)`"}),
  (.binary.new_flags[] | {class, line: "flag `\(.name)`"}),
  (.changelog.entries[] | {class, line: ("changelog \(.version): \(.text)"
    + (if (.names | length) > 0 then " [" + (.names | map(.name) | join(", ")) + "]" else "" end))})
];
def section($cls): map(select(.class == $cls)) | if length > 0 then "### \($cls)", (.[] | "- " + .line), "" else empty end;
"# Harness report", "",
"Generated \(.generated_at) against `\(.tree)`.", "",
"New: \(.summary.new_unused) unused, \(.summary.new_unknown) unknown, \(.summary.new_used) used.", "",
(.engines | to_entries[] | .key as $name | .value as $v
  | "## \($name)", "",
    "binary \($v.binary.status) (version \($v.binary.version)); changelog \($v.changelog.status)"
      + (if $v.changelog.reason then " (\($v.changelog.reason))" else "" end), "",
    ($v | items | section("unused"), section("unknown"), section("used")),
    (($v.binary.removed_env + $v.binary.removed_flags) as $r
      | if ($r | length) > 0 then "### removed", ($r[] | "- `\(.)`"), "" else empty end),
    (if ($v.binary.notes | length) > 0 then "### notes", ($v.binary.notes[] | "- " + .), "" else empty end)),
(if (.skipped_engines | length) > 0 then "## skipped engines", "", (.skipped_engines[] | "- \(.engine): \(.reason)"), "" else empty end),
(if (.notes | length) > 0 then "## notes", "", (.notes[] | "- " + .), "" else empty end),
(if (.warnings | length) > 0 then "## warnings", "", (.warnings[] | "- " + .), "" else empty end)
EOF
)

write_report() {
  build_report >"$WORK/report.json"
  write_atomic "$REPORT" <"$WORK/report.json"
  jq -r "$MD_JQ" "$REPORT" | write_atomic "$REPORT_MD"
}

append_history() {
  jq -c '{generated_at, summary,
          engines: (.engines | map_values({new_env: .binary.new_env, new_flags: .binary.new_flags,
                                            entries: .changelog.entries}))}' "$REPORT" >>"$HISTORY" ||
    die "cannot write $HISTORY"
}

# --- bus msg ----------------------------------------------------------------

post_one() {
  local id=$1 dir=$2 body=$3
  if ! (cd -- "$dir" && CREW_ID=$id crew msg crew:harness "dispatcher:$id" "$body" </dev/null >/dev/null); then
    warn "bus msg to dispatcher:$id failed"
  fi
}

post_report() {
  local body id common posted=0
  if [[ -n ${CREW_WORKER_ID:-} || -n ${CREW_ROLE_ID:-} ]]; then
    note "worker run; no bus msg posted"
    return 0
  fi
  if ! command -v crew >/dev/null; then
    warn "crew not on PATH; no bus msg posted"
    return 0
  fi
  body=$(jq -c --arg report "$REPORT" '
    {kind: "harness_report", report: $report, new_unused: .summary.new_unused,
     top: [.engines | to_entries[] | .key as $e
           | (.value.binary.new_env[] | select(.class == "unused") | "\($e) env \(.name)"),
             (.value.binary.new_flags[] | select(.class == "unused") | "\($e) flag \(.name)"),
             (.value.changelog.entries[] | select(.class == "unused") | "\($e) changelog \(.version): \(.text[:60])")]
          | .[:10]}' "$REPORT")
  if [[ -n ${CREW_ID:-} ]]; then
    post_one "$CREW_ID" . "$body"
    return 0
  fi
  if ! crew fleet status --json >"$WORK/fleet.json" 2>/dev/null; then
    warn "crew fleet status failed; no bus msg posted"
    return 0
  fi
  jq -r '.repos[]? | select((.common // "") != "") | .common as $c
         | .crews[]? | select(.dispatcher.state == "live") | "\(.crew_id)\t\($c)"' \
    "$WORK/fleet.json" >"$WORK/targets.tsv" || {
    warn "crew fleet status output unreadable; no bus msg posted"
    return 0
  }
  while IFS=$'\t' read -r id common; do
    post_one "$id" "$common" "$body"
    posted=1
  done <"$WORK/targets.tsv"
  if ((posted == 0)); then note "no live dispatcher; report only"; fi
}

summary_lines() {
  jq -r '.engines | to_entries[]
         | "\(.key): binary \(.value.binary.status), \(.value.binary.new_env | length) new env, \(.value.binary.new_flags | length) new flags; changelog \(.value.changelog.status), \(.value.changelog.entries | length) entries"' "$REPORT"
}

main() {
  local engine unused any
  case ${1:-} in
  "") ;;
  -h | --help)
    usage
    return 0
    ;;
  *)
    usage >&2
    return 2
    ;;
  esac
  if (($# > 1)); then
    usage >&2
    return 2
  fi

  WORK=$(mktemp -d)
  trap 'rm -rf "$WORK"' EXIT
  mkdir -p "$OUT_DIR" || die "cannot create $OUT_DIR"
  GENERATED=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  : >"$WORK/warnings.txt"
  : >"$WORK/notes.txt"
  : >"$WORK/skipped.jsonl"
  echo '{}' >"$WORK/engines.json"

  load_state
  build_tree_sets
  list_engines >"$WORK/engines.txt"
  while IFS= read -r engine; do
    [[ -n $engine ]] || continue
    process_engine "$engine"
  done <"$WORK/engines.txt"

  write_atomic "$STATE" <"$NEW"
  write_report
  unused=$(jq '.summary.new_unused' "$REPORT")
  any=$(jq '.summary | .new_unused + .new_used + .new_unknown' "$REPORT")
  if ((any > 0)); then append_history; fi
  if ((unused > 0)); then
    post_report
    write_report
  fi

  printf '%s\n' "$REPORT"
  summary_lines
}

main "$@"
