#!/usr/bin/env bash
# Classify every @test in tests/*.bats into exactly one class:
#
#   behaviour    runs/sources a production entry point and asserts on what it does
#   doc-pinning  asserts on the wording of protocol/command/skill/README markdown
#   structure    adapter sync, required section/file/frontmatter, and static greps of
#                production source that never run it
#   duplicate    body identical (whitespace collapsed, name and comments dropped)
#                to an earlier test in any file; the first occurrence keeps its class
#
# Rules apply in order duplicate -> structure -> doc-pinning -> behaviour. The
# classes are heuristics over test text, not semantics: spot-check before acting.
#
#   bats-classify.sh              TSV: file<TAB>line<TAB>class<TAB>name
#   bats-classify.sh --summary    markdown per-file x class count table
#   bats-classify.sh --near-dups  same-file tests whose bodies match once string
#                                 literals and numbers are placeholders
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${1:-}"
case "$mode" in
"" | --summary | --near-dups) ;;
*)
  printf 'usage: %s [--summary|--near-dups]\n' "${0##*/}" >&2
  exit 2
  ;;
esac

cd "$root/tests"
files=(*.bats)

awk -v mode="$mode" '
# Segments that only read text are blanked before looking for production
# tokens, so `grep x "$CREW"` is a read, not a run.
function unread(line) {
  gsub(/(^|[ (;|&])(run +)?(awk|sed) .*/, " ", line)
  gsub(/(^|[ (;|&])(run +)?(grep|rg|cmp|diff|cat|head|tail|wc|ls|stat|test|sha256sum|python3?|\[ -[a-z]) [^|;&]*/, " ", line)
  gsub(/^[[:space:]]*for [A-Za-z_]+ in [^;]*/, " ", line)
  return line
}

function tok(names, line) {
  return names != "" && match(line, "\\$\\{?(" names ")\\}?([^A-Za-z0-9_]|$)")
}

function fn_tok(names, line) {
  return names != "" && match(line, "(^|[^A-Za-z0-9_$./-])(" names ")([^A-Za-z0-9_-]|$)")
}

function lit_prod(l) {
  if (l ~ /^[[:space:]]*"[^"]*"( \\|; do)?$/) return 0
  return l ~ /(ROOT|BATS_TEST_DIRNAME|\.\.)[^ ]*\/(adapters\/core\/[A-Za-z0-9_\/-]+|scripts\/[A-Za-z0-9_-]+)\.sh|(^|[^A-Za-z0-9_])dash\/|WORKTREE_GIT_LIB|GRANT_CHECK_LIB|DISPATCH_CONFIG_BIN/ ||
    l ~ /(^|[^A-Za-z0-9_])\.\/(scripts|adapters\/core)\/[A-Za-z0-9_\/-]+\.sh/ ||
    l ~ /(^[[:space:]]*|[;&|(] *|run +)(eval|source|nix (eval|build|flake|run)) |(^[[:space:]]*|[;&|(] *|run +|-c +["\x27])\. [^ ]/ ||
    (l ~ /(^|[ (;|&])bash +(-[A-Za-z]+ +|pipefail +)*"\$/) ||
    (gsrc && l ~ /(^[[:space:]]*|[;&|(] *|run( +--separate-stderr| +!)* +|\$\( *)_[a-z][A-Za-z0-9_]*/)
}

function add_name(set, name) { return set == "" ? name : set "|" name }

# Collect VAR=... assignments that point at production code or at source docs.
function scan_assign(l, scope,    v, rhs) {
  if (match(l, /^[[:space:]]*(local |export )?[A-Za-z_][A-Za-z0-9_]*=/)) {
    v = substr(l, RSTART, RLENGTH); rhs = substr(l, RSTART + RLENGTH)
    sub(/^[[:space:]]*(local |export )?/, "", v); sub(/=$/, "", v)
    if (rhs ~ /nix (eval|build)/) {
      if (scope == "g") gprod = add_name(gprod, v); else lprod = add_name(lprod, v)
    } else if (rhs !~ /\$\(|`/ && rhs ~ /adapters\/core\/[A-Za-z0-9_\/-]+\.sh|scripts\/[A-Za-z0-9_-]+\.sh|dash\/|CREW_DASH_BIN/) {
      if (scope == "g") gprod = add_name(gprod, v); else lprod = add_name(lprod, v)
    } else if (rhs ~ /(adapters|ROOT|BATS_TEST_DIRNAME|\$core|\$base)/ && rhs ~ /protocols|commands|skills|reviewers|critics|rules|agents|README|\.mdc?"/) {
      if (scope == "g") gdoc = add_name(gdoc, v); else ldoc = add_name(ldoc, v)
    }
  }
}

# Per-file state outside tests: production vars (gprod), doc vars (gdoc) and
# helper functions that run production code (gfn).
function scan_global(l,    u, n) {
  if (l ~ /^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*\(\) *\{/) {
    n = l; sub(/^[[:space:]]*/, "", n); fn_name = substr(n, 1, index(n, "(") - 1)
    u = unread(l)
    fn_prod = lit_prod(u) || tok(gprod, u) || fn_tok(gfn, u)
    if (l ~ /\}[[:space:]]*$/) { if (fn_prod) gfn = add_name(gfn, fn_name); return }
    in_fn = 1; return
  }
  if (in_fn && l ~ /^[[:space:]]*}/) { if (fn_prod) gfn = add_name(gfn, fn_name); in_fn = 0; return }
  if (in_fn) {
    u = unread(l)
    if (lit_prod(u) || tok(gprod, u) || fn_tok(gfn, u)) fn_prod = 1
  }
  if (l ~ /^[[:space:]]*(\.|source) +"/) gsrc = 1
  scan_assign(l, "g")
}

function runs_prod(body, raw,    n, i, lines, u) {
  n = split(body, lines, "\n")
  for (i = 1; i <= n; i++) {
    if (lines[i] ~ /^[[:space:]]*(local )?[A-Za-z_][A-Za-z0-9_]*="[^"$]*(\$[A-Za-z_{][^"]*)?"$/) continue
    if (lines[i] ~ /\$\{?(OUT_[A-Z_]+|EVAL)\}?/) return 1
    u = raw ? lines[i] : unread(lines[i])
    if (lit_prod(u) || tok(gprod, u) || tok(lprod, u) || fn_tok(gfn, u)) return 1
  }
  return 0
}

function refs_doc(body) {
  if (body ~ /(adapters|\$ROOT|\$\{ROOT\}|BATS_TEST_DIRNAME)[^ ]*(protocols|commands|skills|reviewers|critics|rules|agents)|README|\.mdc|adapters\/[^ ]*\.md|\/protocols\/|SKILL\.md|[A-Za-z_]+\.md([^A-Za-z0-9_]|$)/) return 1
  return tok(gdoc, body) || tok(ldoc, body)
}

function classify(body,    d, nosq, prodref, prod, doc, stat, strong, n, i, bl, bare, wording) {
  lprod = ""; ldoc = ""
  n = split(body, bl, "\n")
  for (i = 1; i <= n; i++) scan_assign(bl[i], "l")
  for (i = 1; i <= n; i++) {
    if (!(lit_prod(bl[i]) || tok(gprod, bl[i]) || tok(lprod, bl[i])) || !match(bl[i], />[[:space:]]*"?\$\{?[a-z_]+/)) continue
    d = substr(bl[i], RSTART, RLENGTH); sub(/^>[[:space:]]*"?\$\{?/, "", d)
    if (body ~ ("(^|\n)[[:space:]]*(local )?" d "=")) lprod = add_name(lprod, d)
  }
  prod = runs_prod(body)
  nosq = body
  gsub(/\x27[^\x27]*\x27/, "\x27\x27", nosq)
  doc = refs_doc(nosq)
  prodref = runs_prod(body, 1)
  bare = body
  gsub(/\x27[^\x27]*[A-Za-z]+ [A-Za-z]+ [A-Za-z]+ [A-Za-z]+[^\x27]*\x27/, "", bare)
  gsub(/"[^"]*[A-Za-z]+ [A-Za-z]+ [A-Za-z]+ [A-Za-z]+[^"]*"/, "", bare)
  wording = bare != body
  stat = body ~ /(^|[^A-Za-z0-9_])(grep|rg|cmp|diff|awk|sed|jq|yq|python3?|cat)([^A-Za-z0-9_]|$)/ && body ~ /ROOT|BATS_TEST_DIRNAME|adapters\//
  strong = body ~ /(^|[^A-Za-z0-9_])cmp / || body ~ /(^|[^A-Za-z0-9_])diff (-[a-z]+ )?["$]/ || \
    bare ~ /frontmatter|alwaysApply|\^---|yaml\.safe_load|json\.load|manifest|hooks\.json|hookyard|plugin\.json/ || \
    (body ~ /\[ !? ?-[fdxe] / && body ~ /ROOT|BATS_TEST_DIRNAME|adapters\/|OUT_|nix\/store/) || \
    (!wording && body ~ /grep[^|]*["\x27]\^#+ /)
  if (strong && !prod) return "structure"
  if (doc && !prod && body ~ /(^|[^A-Za-z0-9_])(grep|rg|awk|sed)([^A-Za-z0-9_]|$)/) return "doc-pinning"
  if ((stat || prodref) && !prod && !doc) return "structure"
  return "behaviour"
}

function norm(body,    s) {
  s = body
  gsub(/[[:space:]]+/, " ", s)
  return s
}

function shape(body,    s) {
  s = body
  gsub(/"([^"\\]|\\.)*"/, "\"S\"", s)
  gsub(/\x27[^\x27]*\x27/, "\x27S\x27", s)
  gsub(/[0-9]+/, "N", s)
  gsub(/[[:space:]]+/, " ", s)
  return s
}

function flush() {
  if (!in_test) return
  in_test = 0
  nt++
  tfile[nt] = cur_file; tline[nt] = start; tname[nt] = name
  tbody[nt] = body
  tclass[nt] = classify(body)
}

FNR == 1 { flush(); cur_file = FILENAME; gprod = ""; gdoc = ""; gfn = ""; gsrc = 0; in_fn = 0 }
/^@test / {
  flush()
  in_test = 1; start = FNR; body = ""
  name = $0
  sub(/^@test "/, "", name)
  sub(/" \{$/, "", name)
  next
}
in_test && /^}/ { flush(); next }
in_test {
  if ($0 ~ /^[[:space:]]*#/) next
  body = body $0 "\n"
  next
}
{ scan_global($0) }
END {
  flush()
  for (i = 1; i <= nt; i++) {
    n = norm(tbody[i])
    if (n in seen) cls[i] = "duplicate"
    else { seen[n] = i; cls[i] = tclass[i] }
  }
  if (mode == "--near-dups") {
    for (i = 1; i <= nt; i++) {
      if (cls[i] == "duplicate") continue
      if (split(tbody[i], tmp, "\n") < 4) continue
      k = tfile[i] SUBSEP shape(tbody[i])
      if (k in anchor) {
        a = anchor[k]
        printf "%s\t%d\t%s\t%d\t%s\n", tfile[i], tline[a], tname[a], tline[i], tname[i]
      } else anchor[k] = i
    }
    exit
  }
  if (mode == "--summary") {
    printf "| file | behaviour | doc-pinning | structure | duplicate | total |\n"
    printf "|---|---:|---:|---:|---:|---:|\n"
    for (i = 1; i <= nt; i++) { c[tfile[i], cls[i]]++; if (!(tfile[i] in fs)) { fs[tfile[i]] = 1; order[++nf] = tfile[i] } }
    split("behaviour doc-pinning structure duplicate", cl, " ")
    for (f = 1; f <= nf; f++) {
      row = ""; tot = 0
      for (j = 1; j <= 4; j++) { v = c[order[f], cl[j]] + 0; row = row " " v " |"; tot += v; T[cl[j]] += v }
      printf "| %s |%s %d |\n", order[f], row, tot
      G += tot
    }
    printf "| **total** |"
    for (j = 1; j <= 4; j++) printf " **%d** |", T[cl[j]]
    printf " **%d** |\n", G
    exit
  }
  for (i = 1; i <= nt; i++) printf "%s\t%d\t%s\t%s\n", tfile[i], tline[i], cls[i], tname[i]
}
' "${files[@]}"
