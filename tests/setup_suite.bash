setup_suite() {
  local root
  root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  mkdir -p "$BATS_SUITE_TMPDIR/bin"
  (cd "$root/crew" && GOTOOLCHAIN=local GOFLAGS=-buildvcs=false go build -o "$BATS_SUITE_TMPDIR/bin/crew-go" .)
  export CREW_GO_BIN="$BATS_SUITE_TMPDIR/bin/crew-go"
}
