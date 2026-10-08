#!/usr/bin/env bash
# Regenerates the jq-produced goldens that the jsonv tests compare against.
# Dev-time only: the tests read the committed files and never run jq.
# Usage: bash gen.sh (any cwd). Requires jq 1.8.2 exactly.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

want=jq-1.8.2
got=$(jq --version)
if [ "$got" != "$want" ]; then
  echo "gen.sh: need $want, found $got" >&2
  exit 1
fi

palette='4;31:0;35:0;36:1;33:0;34:1;35:1;36:4;32'

# Inputs that need raw bytes a heredoc cannot carry portably.
printf '"a\xffb"\n["\xc3("]\n"\xe2\x82"\n"\xed\xa0\x80"\n"\xc0\x80 \xf4\x90\x80\x80"\n"\xe2A"\n"\xe2\x82A"\n"\xf0\x9f"\n"\xc3\xa9\xa9"\n{"k\xff":"v\x80"}\n' >docs/invalid-utf8.json

for doc in docs/*.json; do
  n=${doc%.json}
  jq . "$doc" >"$n.pretty"
  jq -c . "$doc" >"$n.compact"
  jq -C . "$doc" >"$n.color"
  jq -C -c . "$doc" >"$n.colorc"
  JQ_COLORS=$palette jq -C -c . "$doc" >"$n.custom"
done

: >literals.out
while IFS= read -r lit; do
  jq -c . <<<"$lit" >>literals.out
done <literals.txt

# computed.in rows: <float parsed by Go> TAB <jq expression yielding it>
: >computed.tsv
while IFS=$'\t' read -r f expr; do
  printf '%s\t%s\t%s\n' "$f" "$expr" "$(jq -nc "$expr")" >>computed.tsv
done <computed.in

# jqcolors.tsv rows: JQ_COLORS TAB accepted(1)/rejected(0) TAB coloured output
doc='[null,false,true,1,"s",[],{},{"k":0}]'
: >jqcolors.tsv
while IFS= read -r c; do
  out=$(JQ_COLORS=$c jq -C -c . <<<"$doc" 2>/dev/null)
  ok=1
  if JQ_COLORS=$c jq -C -c . <<<"$doc" 2>&1 >/dev/null | grep -q 'Failed to set'; then
    ok=0
  fi
  printf '%s\t%s\t%s\n' "$c" "$ok" "$out" >>jqcolors.tsv
done <jqcolors.txt

# compare.tsv rows: A TAB B TAB sign of jq's A <=> B (-1, 0, 1)
: >compare.tsv
while IFS=$'\t' read -r a b; do
  c=$(jq -nc --argjson a "$a" --argjson b "$b" \
    'if $a < $b then -1 elif $a == $b then 0 else 1 end')
  printf '%s\t%s\t%s\n' "$a" "$b" "$c" >>compare.tsv
done <compare.in
