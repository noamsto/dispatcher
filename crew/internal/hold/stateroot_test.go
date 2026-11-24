package hold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

func writeSpec(t *testing.T, tr *tree) string {
	t.Helper()
	spec := filepath.Join(tr.dir, "spec.md")
	if err := os.WriteFile(spec, []byte("the spec body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestSpecUnderCrewStateRoot(t *testing.T) {
	t.Run("pinned", func(t *testing.T) {
		tr := setup(t)
		state := t.TempDir()
		tr.paths = tr.paths.WithState(state)
		if err := bus.CreateMeta(state, bus.Meta{ID: "c1", CreatedBy: "register"}); err != nil {
			t.Fatal(err)
		}
		spec := writeSpec(t, tr)
		out, stderr, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Spec = spec }))
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		id := strings.TrimSpace(out)
		if _, err := os.Stat(filepath.Join(bus.StoreDir(state, "c1"), "holds", id+".md")); err != nil {
			t.Errorf("spec not under the store dir: %v", err)
		}
		if _, err := os.Stat(filepath.Join(tr.paths.Dir, "holds")); err == nil {
			t.Error("holds created under paths.Dir")
		}
	})

	t.Run("unpinned", func(t *testing.T) {
		tr := setup(t)
		tr.paths = tr.paths.WithState(t.TempDir())
		spec := writeSpec(t, tr)
		out, stderr, code := tr.run(t, fullAdd(func(a *AddRequest) { a.Spec = spec }))
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		if _, err := os.Stat(filepath.Join(tr.paths.Dir, "holds", strings.TrimSpace(out)+".md")); err != nil {
			t.Errorf("spec not under paths.Dir: %v", err)
		}
	})
}

func TestRefusedCrewCreatesNothing(t *testing.T) {
	tr := setup(t)
	state := t.TempDir()
	tr.paths = tr.paths.WithState(state)
	if err := bus.CreateMeta(state, bus.Meta{ID: "c1", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bus.ArchiveDir(state, "c1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bus.StoreDir(state, "c1"), bus.ArchiveDir(state, "c1")); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := tr.run(t, fullAdd(nil))
	_, err := tr.paths.Crew("c1")
	if code == 0 || stderr != "crew: "+err.Error()+"\n" {
		t.Errorf("code %d stderr %q, want the refusal verbatim", code, stderr)
	}
	if _, err := os.Stat(bus.StoreDir(state, "c1")); err == nil {
		t.Error("the refused add created a store dir")
	}
}
