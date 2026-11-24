package repostest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const childEnv = "REPOSTEST_GUARD_CHILD"

// TestMain runs the child mode under Guard, the way a consumer package would.
func TestMain(m *testing.M) {
	if os.Getenv(childEnv) != "" {
		os.Exit(Guard(m).Run())
	}
	os.Exit(m.Run())
}

// TestGuardedChild is the body the parent tests re-exec: it writes the registry
// at the path in childEnv when asked to.
func TestGuardedChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("child of the guard subprocess tests")
	}
	if got := os.Getenv("XDG_DATA_HOME"); got == "" || strings.HasPrefix(os.Getenv(childEnv), got) {
		t.Fatalf("XDG_DATA_HOME %q was not redirected to a scratch dir", got)
	}
	if target := os.Getenv(childEnv); target != "1" {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("/w/leak/.git\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runChild(t *testing.T, target string) (string, error) {
	t.Helper()
	home := t.TempDir()
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "XDG_DATA_HOME=") || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, childEnv+"=")
	})
	env = append(env, "HOME="+home, childEnv+"="+strings.ReplaceAll(target, "$HOME", home))
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardedChild$")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestGuardFailsWhenTheRealRegistryIsWritten(t *testing.T) {
	out, err := runChild(t, "$HOME/.local/share/crew/repos")
	if err == nil {
		t.Fatalf("a child that wrote the real registry exited 0:\n%s", out)
	}
	if !strings.Contains(out, "real registry") {
		t.Errorf("output lacks the guard's message:\n%s", out)
	}
}

func TestGuardPassesACleanRun(t *testing.T) {
	if out, err := runChild(t, "1"); err != nil {
		t.Fatalf("a clean child failed: %v\n%s", err, out)
	}
}
