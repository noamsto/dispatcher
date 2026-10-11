package repos

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mkbus(t *testing.T, dir string) string {
	t.Helper()
	common := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(common, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(common, "crew", "events.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return common
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func discover(t *testing.T, o Options) ([]Repo, string) {
	t.Helper()
	var errb bytes.Buffer
	o.Stderr = &errb
	if o.Prefix == "" {
		o.Prefix = "crew: rate --sweep-all"
	}
	return Discover(o), errb.String()
}

func commons(rs []Repo) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Common)
	}
	return out
}

func TestWalkFindsNestedAndWorktreesIgnoresHiddenAndDeep(t *testing.T) {
	root := t.TempDir()
	a := mkbus(t, filepath.Join(root, "a"))
	g := mkbus(t, filepath.Join(root, "g", "c"))
	w := mkbus(t, filepath.Join(root, ".worktrees", "w"))
	mkbus(t, filepath.Join(root, ".hidden", "x"))
	// .git/crew/events.jsonl is 3 deep below the repo dir: repo at depth 3
	// puts the file at depth 6, repo at depth 4 puts it at 7.
	deepOK := mkbus(t, filepath.Join(root, "d1", "d2", "d3"))
	mkbus(t, filepath.Join(root, "e1", "e2", "e3", "e4"))

	rs, errs := discover(t, Options{Roots: []string{root}, Registry: filepath.Join(root, "none"), Env: envOf(nil)})
	want := []string{w, a, deepOK, g}
	if got := commons(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("commons = %v, want %v", got, want)
	}
	for _, r := range rs {
		if r.Status != "ok" {
			t.Errorf("%s status %q", r.Common, r.Status)
		}
	}
	if errs != "" {
		t.Errorf("stderr = %q", errs)
	}
}

func TestRegistryOutsideRootDedupedSorted(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	a := mkbus(t, filepath.Join(root, "a"))
	z := mkbus(t, filepath.Join(other, "z"))
	reg := filepath.Join(t.TempDir(), "repos")
	if err := os.WriteFile(reg, []byte(z+"\n"+a+"\n\n"+a+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, _ := discover(t, Options{Roots: []string{root}, Registry: reg, Env: envOf(nil)})
	want := []string{a, z}
	if strings.Compare(z, a) < 0 {
		want = []string{z, a}
	}
	if got := commons(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("commons = %v, want %v", got, want)
	}
}

func TestStatusGoneAndNoBus(t *testing.T) {
	root := t.TempDir()
	nobus := filepath.Join(root, "nb", ".git")
	if err := os.MkdirAll(nobus, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "missing", ".git")
	ok := mkbus(t, filepath.Join(root, "ok"))
	reg := filepath.Join(t.TempDir(), "repos")
	if err := os.WriteFile(reg, []byte(gone+"\n"+nobus+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, _ := discover(t, Options{Roots: []string{t.TempDir()}, Registry: reg, Env: envOf(nil)})
	got := map[string]Status{}
	for _, r := range rs {
		got[r.Common] = r.Status
	}
	want := map[string]Status{gone: "gone", nobus: "no bus"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	_ = ok
}

func TestNonDirRootWarnsAndNoneFoundNote(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	rs, errs := discover(t, Options{Roots: []string{file + "\n" + empty}, Registry: filepath.Join(empty, "none"), Env: envOf(nil)})
	if len(rs) != 0 {
		t.Fatalf("rs = %v", rs)
	}
	want := "crew: rate --sweep-all: " + file + " is not a directory\n" +
		"crew: rate --sweep-all: no repo with a crew bus found (roots: " + file + " " + empty + ")\n"
	if errs != want {
		t.Fatalf("stderr = %q, want %q", errs, want)
	}
}

func TestRootsResolution(t *testing.T) {
	home, r1, r2, flag := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	h := mkbus(t, filepath.Join(home, "h"))
	a := mkbus(t, filepath.Join(r1, "a"))
	b := mkbus(t, filepath.Join(r2, "b"))
	f := mkbus(t, filepath.Join(flag, "f"))
	reg := filepath.Join(home, "none")

	cases := []struct {
		name  string
		roots []string
		env   map[string]string
		want  []string
	}{
		{"home default", nil, map[string]string{"HOME": home}, []string{h}},
		{"env split", nil, map[string]string{"HOME": home, "CREW_SWEEP_ROOTS": r1 + ":" + r2}, []string{a, b}},
		{"flag overrides env", []string{flag}, map[string]string{"HOME": home, "CREW_SWEEP_ROOTS": r1}, []string{f}},
		{"flag with colon", []string{r1 + ":" + r2}, map[string]string{"HOME": home}, []string{a, b}},
		{"trailing colon", nil, map[string]string{"HOME": home, "CREW_SWEEP_ROOTS": r1 + ":"}, []string{a}},
	}
	for _, c := range cases {
		rs, errs := discover(t, Options{Roots: c.roots, Registry: reg, Env: envOf(c.env)})
		got := commons(rs)
		if len(c.want) > 1 && c.want[0] > c.want[1] {
			c.want[0], c.want[1] = c.want[1], c.want[0]
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v (stderr %q)", c.name, got, c.want, errs)
		}
	}
}

func TestUnreadableDirDoesNotAbort(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "a", "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	b := mkbus(t, filepath.Join(root, "b"))

	rs, _ := discover(t, Options{Roots: []string{root}, Registry: filepath.Join(root, "none"), Env: envOf(nil)})
	if got := commons(rs); !reflect.DeepEqual(got, []string{b}) {
		t.Fatalf("commons = %v", got)
	}
}

func TestSymlinksNotFollowed(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	mkbus(t, filepath.Join(elsewhere, "r"))
	if err := os.Symlink(filepath.Join(elsewhere, "r"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	rs, _ := discover(t, Options{Roots: []string{root}, Registry: filepath.Join(root, "none"), Env: envOf(nil)})
	if len(rs) != 0 {
		t.Fatalf("followed symlink: %v", rs)
	}
}

func TestRegistryPath(t *testing.T) {
	if got := RegistryPath(envOf(map[string]string{"HOME": "/h"})); got != "/h/.local/share/crew/repos" {
		t.Errorf("home: %s", got)
	}
	if got := RegistryPath(envOf(map[string]string{"HOME": "/h", "XDG_DATA_HOME": "/x"})); got != "/x/crew/repos" {
		t.Errorf("xdg: %s", got)
	}
}

func TestName(t *testing.T) {
	cases := []struct{ common, origin, want string }{
		{"/w/proj/.git", "https://github.com/acme/alpha.git", "acme/alpha"},
		{"/w/proj/.git", "https://github.com/acme/alpha", "acme/alpha"},
		{"/w/proj/.git", "git@github.com:acme/alpha.git", "acme/alpha"},
		{"/w/proj/.git", "ssh://git@host.example/acme/alpha.git", "acme/alpha"},
		{"/w/proj/.git", "", "proj"},
		{"/w/proj/.git", "/srv/local/repo", "proj"},
		{"/srv/name.git", "", "name"},
		{"/srv/bare", "", "bare"},
	}
	for _, c := range cases {
		if got := Name(c.common, c.origin); got != c.want {
			t.Errorf("Name(%q,%q) = %q, want %q", c.common, c.origin, got, c.want)
		}
	}
}
