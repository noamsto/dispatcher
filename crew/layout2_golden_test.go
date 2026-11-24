package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// TestLayout2PinnedLifecycle runs the layout-1 golden's lifecycle for a crew
// registered under CREW_LAYOUT=2: nothing lands under <common>/crew, and every
// file lands in the crew's store dir under $XDG_STATE_HOME/crew.
func TestLayout2PinnedLifecycle(t *testing.T) {
	const crew = "pin1"
	c := newCrewsCLI(t)
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("CREW_LAYOUT", "2")
	clockFile := filepath.Join(t.TempDir(), "clock")
	if err := os.WriteFile(clockFile, []byte("1700000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREW_CLOCK", clockFile)
	pid := strconv.Itoa(os.Getppid())
	common := filepath.Join(c.repo, ".git")
	store := bus.StoreDir(bus.StateRoot(os.Getenv), crew)

	files := func(root string) []string {
		var rels []string
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rels = append(rels, rel)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		slices.Sort(rels)
		return rels
	}
	check := func(label string, wantStore []string) {
		t.Helper()
		if leaked := files(filepath.Join(common, "crew")); len(leaked) > 0 {
			t.Errorf("%s: pinned crew wrote under the repo bus: %v", label, leaked)
		}
		prefix, err := filepath.Rel(state, store)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, rel := range wantStore {
			want = append(want, filepath.Join(prefix, rel))
		}
		slices.Sort(want)
		if got := files(state); !slices.Equal(got, want) {
			t.Errorf("%s: $XDG_STATE_HOME files\n got: %v\nwant: %v", label, got, want)
		}
	}

	const worker = "worker:feat/x#s1-1"
	c.as(crew)
	c.mustRun("register", pid)
	c.mustRun("status", worker, "working")
	c.mustRun("msg", worker, "dispatcher:"+crew, "hello")
	c.mustRun("hold", "add", "--engine", "claude", "--window", "5h", "--resets-at", "1700000100",
		"--agent", "codex", "--ref", "r1", "--branch", "b1", "--tier", "standard", "--model", "sonnet",
		"--effort", "medium", "--crew", crew, "gold hold")
	c.mustRun("msg", "dispatcher:"+crew, worker, "go")
	c.mustRun("inbox", worker, crew)
	c.mustRun("adopt", crew, pid)

	line, err := json.Marshal(map[string]any{
		"ts": 1700000001000, "crew_id": crew, "kind": "resume", "branch": "feat/x",
		"worker_id": worker, "session": "s1", "repo": c.repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Append(filepath.Join(store, "events.jsonl"), string(line)); err != nil {
		t.Fatal(err)
	}

	// inbox's await mark and its lock, keyed by crew, session and cksum.
	const mark = "pin1-worker_feat_x_s1-1.1810048773"
	check("before deregister", []string{
		"await/" + mark, "await.lock/" + mark, "events.jsonl", "meta.json", "pid", "pid.start", "pidfile.log",
	})

	data, err := os.ReadFile(filepath.Join(store, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind":"status"`, `gold hold`, `"body":"hello"`, `"body":"go"`, `"kind":"resume"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("store log lacks %s:\n%s", want, data)
		}
	}

	c.mustRun("deregister")
	check("after deregister", []string{"await/" + mark, "await.lock/" + mark, "events.jsonl", "meta.json", "pidfile.log"})
}
