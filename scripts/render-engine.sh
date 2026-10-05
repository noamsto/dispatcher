#!/usr/bin/env bash
# Render a shared markdown file for one engine. A block opens with the exact
# column-0 line `<!-- only:ENGINE[,ENGINE...] -->` and closes with the exact
# column-0 line `<!-- /only -->`; its content is kept only for the listed
# engines, marker lines never print. A line that starts (after any indent) with
# `<!--` plus `only:` or `/only` but is not exactly a marker line, including one
# with a trailing CR, is an error; marker-like text mid-line is plain text.
# Blocks cannot nest, hold headings, or have markers inside a code fence. A
# fence is 3+ backticks or tildes after any indent and closes on the same
# character at least as long.
# Usage: render-engine.sh <engine> <file>   (engine: claude|codex|cursor|pi)
set -euo pipefail

[[ $# -eq 2 ]] || {
  echo "usage: render-engine.sh <engine> <file>" >&2
  exit 2
}
case $1 in
claude | codex | cursor | pi) ;;
*)
  echo "render-engine.sh: unknown engine '$1'" >&2
  exit 2
  ;;
esac

awk -v engine="$1" '
function fail(msg) {
  print FILENAME ":" NR ": " msg > "/dev/stderr"
  failed = 1
  exit 1
}
{
  t = $0
  sub(/^[ \t]+/, "", t)
  if (fence) {
    if ((t ~ /^`+[ \t]*$/ || t ~ /^~+[ \t]*$/) && substr(t, 1, 1) == fchar) {
      sub(/[ \t]+$/, "", t)
      if (length(t) >= flen) fence = 0
    }
  } else if (t ~ /^(```|~~~)/) {
    fchar = substr(t, 1, 1)
    flen = match(t, fchar == "`" ? "^`+" : "^~+") ? RLENGTH : 0
    fence = 1
  }
}
/^[ \t]*<!--[ \t]*(only:|\/only)/ && !/^<!-- only:.* -->$/ && !/^<!-- \/only -->$/ { fail("malformed marker") }
/^<!-- only:.* -->$/ || /^<!-- \/only -->$/ {
  if (fence) fail("marker inside a code fence")
  if ($0 ~ /^<!-- \/only -->$/) {
    if (!open) fail("close without open")
    open = 0
    next
  }
  if (open) fail("nested block")
  list = $0
  sub(/^<!-- only:/, "", list)
  sub(/ -->$/, "", list)
  n = split(list, engines, ",")
  if (n == 0) fail("empty engine list")
  keep = 0
  for (i = 1; i <= n; i++) {
    if (engines[i] !~ /^(claude|codex|cursor|pi)$/) fail("unknown engine \"" engines[i] "\"")
    if (engines[i] == engine) keep = 1
  }
  open = 1
  next
}
open && !fence && /^#{1,6} / { fail("heading inside a block") }
!open || keep { print }
END { if (!failed && open) { print FILENAME ": unclosed block at EOF" > "/dev/stderr"; exit 1 } }
' "$2"
