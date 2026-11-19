package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
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

// dispatcherPID is the launcher pid the agent_pid cases are judged against;
// agent 300 runs under it through shell 200, 400 is alive elsewhere.
const dispatcherPID = 100

// tmuxServerPID is the dispatcher's tmux server, its parent; 500 is another
// server's alive pid.
const tmuxServerPID = 50

func paneProcs() *proctest.Table {
	return proctest.New(map[int]bool{tmuxServerPID: true, dispatcherPID: true, 400: true, 500: true}, map[int]int{dispatcherPID: tmuxServerPID, 200: dispatcherPID, 300: 200})
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
	for _, state := range []string{"processing", "waiting", "done", "idle", "compacting", "denied", "error"} {
		t.Run(state, func(t *testing.T) {
			dir := paneDir(t, 0o700, "81", "state="+state+"\nts=1\n")
			if got := readPaneState(envDir(dir), "%81", paneProcs(), dispatcherPID); got == nil || *got != state {
				t.Errorf("got %v, want %q", got, state)
			}
		})
	}

	for _, tc := range []struct{ name, text string }{
		{"agent_pid is a descendant", "state=waiting\nagent_pid=300\n"},
		{"agent_pid is the dispatcher", "agent_pid=100\nstate=waiting\n"},
		{"agent_pid absent", "state=waiting\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := paneDir(t, 0o700, "81", tc.text)
			if got := readPaneState(envDir(dir), "%81", paneProcs(), dispatcherPID); got == nil || *got != "waiting" {
				t.Errorf("got %v, want waiting", got)
			}
		})
	}

	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"server is the dispatcher's tmux server", "state=waiting\nserver=50\n", true},
		{"server is foreign", "state=waiting\nserver=500\n", false},
		{"server is dead", "state=waiting\nserver=999\n", false},
		{"server is the dispatcher itself", "state=waiting\nserver=100\n", false},
		{"server garbage", "state=waiting\nserver=abc\n", false},
		{"server zero", "state=waiting\nserver=0\n", false},
		{"server empty", "state=waiting\nserver=\n", false},
		{"agent_pid wins over a foreign server", "state=waiting\nagent_pid=300\nserver=500\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := paneDir(t, 0o700, "81", tc.text)
			got := readPaneState(envDir(dir), "%81", paneProcs(), dispatcherPID)
			if tc.want != (got != nil) {
				t.Errorf("got %v, want state %v", got, tc.want)
			}
		})
	}

	t.Run("agent_pid with no dispatcher pid", func(t *testing.T) {
		dir := paneDir(t, 0o700, "81", "state=waiting\nagent_pid=300\n")
		if got := readPaneState(envDir(dir), "%81", paneProcs(), 0); got != nil {
			t.Errorf("got %q, want nil", *got)
		}
	})

	t.Run("state among other keys", func(t *testing.T) {
		dir := paneDir(t, 0o700, "81", "ts=1\nstate=waiting\nmsg=a=b\n")
		if got := readPaneState(envDir(dir), "%81", paneProcs(), dispatcherPID); got == nil || *got != "waiting" {
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
		{"interrupted is derived, never written", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=interrupted\n") }, "%81"},
		{"agent_pid outside the dispatcher", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\nagent_pid=400\n") }, "%81"},
		{"agent_pid dead", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\nagent_pid=999\n") }, "%81"},
		{"agent_pid zero", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\nagent_pid=0\n") }, "%81"},
		{"agent_pid negative", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\nagent_pid=-1\n") }, "%81"},
		{"agent_pid garbage", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\nagent_pid=abc\n") }, "%81"},
		{"agent_pid empty", func(t *testing.T) string { return paneDir(t, 0o700, "81", "state=waiting\nagent_pid=\n") }, "%81"},
		{"oversized file", func(t *testing.T) string {
			return paneDir(t, 0o700, "81", strings.Repeat("x=y\n", 4096)+"state=waiting\n")
		}, "%81"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readPaneState(envDir(tc.dir(t)), tc.pane, paneProcs(), dispatcherPID); got != nil {
				t.Errorf("got %q, want nil", *got)
			}
		})
	}

	t.Run("missing dir is not created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "absent")
		if got := readPaneState(envDir(dir), "%81", paneProcs(), dispatcherPID); got != nil {
			t.Errorf("got %q, want nil", *got)
		}
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Errorf("the status dir was created: %v", err)
		}
	})
}
