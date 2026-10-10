# shellcheck shell=bash
# Source this from a harness entry, then call ensure_crew_go_bin once per run.
#
# adapters/core/crew.sh names the Go binary through CREW_GO_BIN. The nix build
# substitutes the installed path into that variable's fallback, but the raw
# source keeps the @crewGoBin@ placeholder, so a harness that shells into crew.sh
# from a checkout has to set it. tests/setup_suite.bash does that for every bats
# run; the other harnesses have no bats suite to hook, so each entry calls this.
#
# An existing CREW_GO_BIN always wins, so a bench adapter can build once and let
# its per-case children inherit — the build then stays out of the measured
# window. On success the build directory lands in CREW_GO_DIR for the caller's
# cleanup trap, deliberately unexported: a child that sources this must not
# remove the binary its parent built. A failed build returns nonzero, which fails
# the run.

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
