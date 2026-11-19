package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// paneDir is a status dir of the given mode holding panes/<id> = text.
func paneDir(t *testing.T, mode os.FileMode, id, text string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "status")
	if err := os.MkdirAll(filepath.Join(dir, "panes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if id != "" {
		if err := os.WriteFile(filepath.Join(dir, "panes", id), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadPaneState(t *testing.T) {
	envDir := func(dir string) func(string) string {
		return func(k string) string {
			if k == "CLAUDE_STATUS_DIR" {
				return dir
			}
			return ""
		}
	}
	for _, state := range []string{"processing", "waiting", "done", "idle", "compacting", "denied", "error", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			dir := paneDir(t, 0o700, "81", "state="+state+"\nts=1\n")
			if got := readPaneState(envDir(dir), "%81"); got == nil || *got != state {
				t.Errorf("got %v, want %q", got, state)
			}
		})
	}

	t.Run("state among other keys", func(t *testing.T) {
		dir := paneDir(t, 0o700, "81", "ts=1\nstate=waiting\nmsg=a=b\n")
		if got := readPaneState(envDir(dir), "%81"); got == nil || *got != "waiting" {
			t.Errorf("got %v", got)
		}
	})

	for _, tc := range []struct {
		name string
		dir  func(t *testing.T) string
		pane string
	}{
		{"missing file", func(t *testing.T) string { return paneDir(t, 0o700, "", "") }, "%81"},
		{"dir mode 0755", func(t *testing.T) string { return paneDir(t, 0o755, "81", "state=waiting\n") }, "%81"},
		{"dir mode 0750", func(t *testing.T) string { return paneDir(t, 0o750, "81", "state=waiting\n") }, "%81"},
		{"dir is a symlink", func(t *testing.T) string {
			real := paneDir(t, 0o700, "81", "state=waiting\n")
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, "%81"},
		{"pane file is a symlink", func(t *testing.T) string {
			dir := paneDir(t, 0o700, "", "")
			target := filepath.Join(t.TempDir(), "state")
			if err := os.WriteFile(target, []byte("state=waiting\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "panes", "81")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "%81"},
		{"pane file is a directory", func(t *testing.T) string {
			dir := paneDir(t, 0o700, "", "")
			if err := os.Mkdir(filepath.Join(dir, "panes", "81"), 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}, "%81"},
		{"unknown state", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=dancing\n") }, "%81"},
		{"empty state", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=\n") }, "%81"},
		{"no state key", func(t *testing.T) string { return paneDir(t, 0o700, "81", "ts=1\n") }, "%81"},
		{"state with a suffix", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting x\n") }, "%81"},
		{"path traversal", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\n") }, "%1/../x"},
		{"no percent", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\n") }, "abc"},
		{"bare number", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\n") }, "81"},
		{"empty pane", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\n") }, ""},
		{"trailing newline pane", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\n") }, "%81\n"},
		{"oversized file", func(t *testing.T) string {
			return paneDir(t, 0o700, "81", strings.Repeat("x=y\n", 4096)+"state=waiting\n")
		}, "%81"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readPaneState(envDir(tc.dir(t)), tc.pane); got != nil {
				t.Errorf("got %q, want nil", *got)
			}
		})
	}

	t.Run("missing dir is not created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "absent")
		if got := readPaneState(envDir(dir), "%81"); got != nil {
			t.Errorf("got %q, want nil", *got)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("the status dir was created: %v", err)
		}
	})
}
