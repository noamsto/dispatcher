package entry

import (
	"os"
	"os/exec"
	"testing"
)

// gitRepo is a checkout to resolve a crew id in: bus.CrewID asks git itself, so
// the fixture is a real repo rather than a planted .git file.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command("git", "init", "-q", dir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Symlink(to, from); err != nil {
		t.Fatal(err)
	}
}
