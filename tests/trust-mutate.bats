bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# scripts/trust-mutate.sh on a fixture repo under $BATS_TEST_TMPDIR: grouped rows
# resolve every site's line against the untouched file, and every peer edit gets
# the same no-change and syntax checks as the leader's. The real manifest is
# never read here.

setup() {
  REPO="$BATS_TEST_TMPDIR/repo"
  mkdir -p "$REPO/scripts" "$REPO/tests"
  cp "$BATS_TEST_DIRNAME/../scripts/trust-mutate.sh" "$REPO/scripts/trust-mutate.sh"
  git -C "$REPO" init -q

  cat >"$REPO/lib.sh" <<'EOF'
guard() { return 1; }
f() {
  guard a || return 1
  guard a || return 1
  echo done
}
EOF
  cat >"$REPO/peer.sh" <<'EOF'
g() {
  guard b || return 1
}
EOF
  printf '%s\n' '@test "f refuses" {' >"$REPO/tests/inner.bats"
  cat >>"$REPO/tests/inner.bats" <<'EOF'
  source "$BATS_TEST_DIRNAME/../lib.sh"
  run f
  [ "$status" -eq 1 ]
}
EOF
}

# manifest <row>...: writes pins.tsv from tab-joined rows, runs the tool on it.
mutate() {
  : >"$REPO/pins.tsv"
  local row
  for row in "$@"; do printf '%b\n' "$row" >>"$REPO/pins.tsv"; done
  git -C "$REPO" add -A
  run --separate-stderr bash "$REPO/scripts/trust-mutate.sh" --manifest pins.tsv --jobs 1
}

@test "trust-mutate: grouped peers sharing one pattern each land on their own site" {
  mutate '@grouped\tG' \
    'G\tlib.sh\tguard a\t2\ttrue\ttests/inner.bats\tf refuses\t"$status" -eq 1' \
    'G\tlib.sh\tguard a\t1\ttrue\ttests/inner.bats\tf refuses\t"$status" -eq 1'
  [ "$status" -eq 0 ]
  [[ $output == *"red     G"* ]]
  [[ $output == *"1 mutations (1 rows): 1 red, 0 failed"* ]]
}

@test "trust-mutate: a peer edit that breaks syntax is reported" {
  mutate '@grouped\tG' \
    'G\tlib.sh\tguard a\t1\ttrue\ttests/inner.bats\tf refuses\t"$status" -eq 1' \
    'G\tpeer.sh\tguard b\t1\tthen\ttests/inner.bats\tf refuses\t"$status" -eq 1'
  [ "$status" -eq 1 ]
  [[ $output == *"FAIL    G"*"peer.sh:2 fails bash -n"* ]]
}

@test "trust-mutate: a peer edit that changes nothing is reported" {
  mutate '@grouped\tG' \
    'G\tlib.sh\tguard a\t1\ttrue\ttests/inner.bats\tf refuses\t"$status" -eq 1' \
    'G\tpeer.sh\tguard b\t1\tguard b\ttests/inner.bats\tf refuses\t"$status" -eq 1'
  [ "$status" -eq 1 ]
  [[ $output == *"FAIL    G"*"changes nothing at peer.sh"* ]]
}

@test "trust-mutate: two sites resolving to one line are reported, not garbled" {
  mutate '@grouped\tG' \
    'G\tlib.sh\tguard a\t1\ttrue\ttests/inner.bats\tf refuses\t"$status" -eq 1' \
    'G\tlib.sh\tguard a\t1\tfalse\ttests/inner.bats\tf refuses\t"$status" -eq 1'
  [ "$status" -eq 1 ]
  [[ $output == *"FAIL    G"*"no longer contains"* ]]
}

@test "trust-mutate: a grouped peer in a missing file is reported" {
  mutate '@grouped\tG' \
    'G\tlib.sh\tguard a\t1\ttrue\ttests/inner.bats\tf refuses\t"$status" -eq 1' \
    'G\tnope.sh\tguard a\t1\ttrue\ttests/inner.bats\tf refuses\t"$status" -eq 1'
  [ "$status" -eq 1 ]
  [[ $output == *"FAIL    G"*"site not found"*"nope.sh"* ]]
}
