# shellcheck shell=bash
# Source this from a harness entry, then call ensure_crew_go_bin once per run.
#
# adapters/core/crew.sh names the Go binary through CREW_GO_BIN, whose fallback
# is an installed path only in the nix build: from a checkout, a harness that
# shells into crew.sh sees the @crewGoBin@ placeholder. tests/setup_suite.bash
# sets it for every bats run; the other harnesses have no bats suite to hook, so
# each entry calls this.
#
# An existing CREW_GO_BIN always wins, so a bench adapter builds once and its
# per-case children inherit — the build stays out of the measured window. The
# build directory lands in CREW_GO_DIR for the caller's cleanup trap,
# deliberately unexported: a child that sources this must not remove the binary
# its parent built. A failed build returns nonzero, which fails the run.

ensure_crew_go_bin() {
  if [[ -n ${CREW_GO_BIN:-} ]]; then
    return
  fi
  # Prefixed locals: the entries source this, and several keep a readonly `root`.
  local _ensure_root
  _ensure_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
  CREW_GO_DIR=$(mktemp -d)
  if ! (
    cd "$_ensure_root/crew" &&
      GOTOOLCHAIN=local GOFLAGS=-buildvcs=false go build -o "$CREW_GO_DIR/crew-go" .
  ); then
    echo "ensure_crew_go_bin: crew-go build failed" >&2
    return 1
  fi
  export CREW_GO_BIN="$CREW_GO_DIR/crew-go"
}

cleanup_crew_go_bin() {
  if [[ -n ${CREW_GO_DIR:-} ]]; then
    rm -rf "$CREW_GO_DIR"
  fi
}
