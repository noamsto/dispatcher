package drift

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The helper's whole contract is how the calling test ends, and neither ending
// is visible to the test that spawned it: both cases run TestHelperMissing in a
// child test binary, the way the stall signal tests do.
func TestMissingSkipsWithoutEnv(t *testing.T) {
	out, err := runChild(t, false)
	if err != nil {
		t.Fatalf("child failed without %s: %v\n%s", Env, err, out)
	}
	if !strings.Contains(out, "--- SKIP: TestHelperMissing") {
		t.Errorf("child did not skip:\n%s", out)
	}
}

func TestMissingFailsWithEnv(t *testing.T) {
	out, err := runChild(t, true)
	if err == nil {
		t.Fatalf("child passed with %s=1; a missing repo file must fail the guard:\n%s", Env, out)
	}
	if !strings.Contains(out, "--- FAIL: TestHelperMissing") {
		t.Errorf("child did not fail the way a Fatalf fails:\n%s", out)
	}
	if !strings.Contains(out, "dispatch.sh") {
		t.Errorf("failure does not name the missing file:\n%s", out)
	}
}

// runChild runs TestHelperMissing in a child of this test binary, with Env set
// or unset regardless of how the parent was run.
func runChild(t *testing.T, required bool) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMissing$", "-test.v")
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, Env+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, childGate+"=1")
	if required {
		env = append(env, Env+"=1")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// childGate keeps TestHelperMissing from running in a normal suite run: it must
// only ever run as the child of runChild.
const childGate = "CREW_DRIFT_CHILD"

func TestHelperMissing(t *testing.T) {
	if os.Getenv(childGate) == "" {
		t.Skip("child-process helper")
	}
	Missing(t, "adapters/core/dispatch.sh", errors.New("open adapters/core/dispatch.sh: no such file or directory"))
	t.Error("Missing returned")
}
