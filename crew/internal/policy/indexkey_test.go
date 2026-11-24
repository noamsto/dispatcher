package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// The repo index and the policy dir key one repo by the same hash, so a
// repo's index file and its policies are found from either.
func TestPolicyDirKeyMatchesRepoIndex(t *testing.T) {
	common := filepath.Join(t.TempDir(), "repo", ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "XDG_STATE_HOME" {
			return filepath.Join(filepath.Dir(common), "state")
		}
		return ""
	}
	dir, real, err := Dir(common, getenv)
	if err != nil {
		t.Fatal(err)
	}
	index := bus.IndexPath(bus.StateRoot(getenv), real)
	if filepath.Base(dir) != filepath.Base(index) {
		t.Fatalf("policy key %s != index key %s", filepath.Base(dir), filepath.Base(index))
	}
}
