// Package drift is the drift guards' one switch. The guards are the tests that
// compare the Go port against the bash that still ships, so they read a repo
// file outside crew/ — adapters/core/dispatch.sh, adapters/core/local-models.sh,
// adapters/core/defaults.json — and the Nix build sandbox (src = ./crew) does
// not hold those files. A missing file skips there, and CI runs `go test ./...`
// from a full checkout with CREW_DRIFT_REQUIRED=1, where the same skip would be
// drift passing unnoticed.
package drift

import (
	"os"
	"testing"
)

// Env is the switch: set, and a drift guard whose repo file is absent fails
// instead of skipping. CI sets it; the Nix build leaves it unset.
const Env = "CREW_DRIFT_REQUIRED"

// Missing ends the test over a repo file a drift guard could not read, given the
// error from its os.ReadFile or os.Stat: a skip when the file is simply not in
// this tree, a failure under Env. It never returns.
func Missing(t *testing.T, path string, err error) {
	t.Helper()
	if os.Getenv(Env) != "" {
		t.Fatalf("%s not present (%v), and %s is set: this run must exercise the drift guards", path, err, Env)
	}
	t.Skipf("%s not present: %v", path, err)
}
