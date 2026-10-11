package gitguard

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitNUL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"\x00", []string{""}},
		{"a", []string{"a"}},
		{"a\x00", []string{"a"}},
		{"a\x00\x00b", []string{"a", "", "b"}},
		{"a\x00\x00", []string{"a", ""}},
	} {
		if got := splitNUL([]byte(tc.in)); !slices.Equal(got, tc.want) {
			t.Errorf("splitNUL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteBaselineSortsUniqueAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "git-config-baseline")
	if err := WriteBaseline(path, []string{"b", "a", "", "b", "#c"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\x00#c\x00a\x00b\x00" {
		t.Fatalf("baseline %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestWriteBaselineFailureRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	// Renaming a file over a directory fails.
	path := filepath.Join(dir, "git-config-baseline")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseline(path, []string{"a"}); err == nil {
		t.Fatal("rename over a directory passed")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestInitFresh(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.pager", "less -SECRET")
	r.config(t, "remote.origin.url", "https://u:tok@example.com/r.git")
	r.config(t, "user.name", "unguarded")
	writeFile(t, r.common+"/remotes/foo", "URL: x\n")
	writeFile(t, r.common+"/branches/.hidden", "y\n")
	if err := os.Symlink("/nowhere", r.common+"/remotes/link"); err != nil {
		t.Fatal(err)
	}
	got, err := capture(func(w io.Writer) error { return Init(r.common, w) })
	if err != nil {
		t.Fatal(err)
	}
	want := "git-config baseline: recorded " + r.baseline() + ": branches/.hidden, remotes/foo, core.pager, remote.origin.url\n"
	if got != want {
		t.Fatalf("note %q, want %q", got, want)
	}
	if strings.Contains(got, "SECRET") || strings.Contains(got, "tok") {
		t.Fatal("note shows a value")
	}
	recs, err := ReadBaseline(r.baseline())
	if err != nil {
		t.Fatal(err)
	}
	hash := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	wantRecs := []string{
		LegacyMark, RedirectMark,
		"#legacy\nbranches\nfile\n.hidden\n" + hash("y\n"),
		"#legacy\nremotes\nfile\nfoo\n" + hash("URL: x\n"),
		"core.pager\nless -SECRET",
		"remote.origin.url\nhttps://u:tok@example.com/r.git",
	}
	if !slices.Equal(recs, wantRecs) {
		t.Fatalf("baseline %q\nwant %q", recs, wantRecs)
	}
	before, _ := os.ReadFile(r.baseline())
	wantPass(t, func(w io.Writer) error { return Init(r.common, w) })
	if after, _ := os.ReadFile(r.baseline()); string(after) != string(before) {
		t.Fatal("second Init rewrote the baseline")
	}
}

func TestInitFreshUnionFailureWritesNothing(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.common+"/config", "[core\n")
	if _, err := capture(func(w io.Writer) error { return Init(r.common, w) }); err == nil {
		t.Fatal("Init passed over an unparsable config")
	}
	if entries, _ := os.ReadDir(r.common + "/crew"); len(entries) != 0 {
		t.Fatalf("left files: %v", entries)
	}
}

func TestInitLeavesSymlinkOrNonRegularBaseline(t *testing.T) {
	r := newRepo(t)
	if err := os.MkdirAll(r.common+"/crew", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nowhere", r.baseline()); err != nil {
		t.Fatal(err)
	}
	wantPass(t, func(w io.Writer) error { return Init(r.common, w) })
	if err := os.Remove(r.baseline()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.baseline(), 0o755); err != nil {
		t.Fatal(err)
	}
	wantPass(t, func(w io.Writer) error { return Init(r.common, w) })
}

func TestInitMigrationAddsRedirectPairsOnly(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.pager", "less")
	r.config(t, "core.editor", "vim")
	r.config(t, "remote.origin.url", "https://example.com/r.git")
	writeFile(t, r.baseline(), "core.pager\nless\x00")
	got, err := capture(func(w io.Writer) error { return Init(r.common, w) })
	if err != nil {
		t.Fatal(err)
	}
	if want := "git-config baseline: recorded redirect keys in " + r.baseline() + ": remote.origin.url\n"; got != want {
		t.Fatalf("note %q, want %q", got, want)
	}
	recs, _ := ReadBaseline(r.baseline())
	want := []string{LegacyMark, RedirectMark, "core.pager\nless", "remote.origin.url\nhttps://example.com/r.git"}
	if !slices.Equal(recs, want) {
		t.Fatalf("baseline %q, want %q", recs, want)
	}
}

func TestInitMigrationRefusesLegacyEntries(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), RedirectMark+"\x00")
	writeFile(t, r.common+"/remotes/x", "URL: y\n")
	wantRefusal(t, "refusing git-config baseline migration: legacy remotes or branches entries exist — inspect and remove them before re-running dispatch\n",
		func(w io.Writer) error { return Init(r.common, w) })
	if got, _ := os.ReadFile(r.baseline()); string(got) != RedirectMark+"\x00" {
		t.Fatalf("baseline changed: %q", got)
	}
}

func TestInitMigrationAddsLegacyMarkWithoutEntries(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), RedirectMark+"\x00\x00core.pager\nless\x00")
	wantPass(t, func(w io.Writer) error { return Init(r.common, w) })
	recs, _ := ReadBaseline(r.baseline())
	if want := []string{"", LegacyMark, RedirectMark, "core.pager\nless"}; !slices.Equal(recs, want) {
		t.Fatalf("baseline %q, want %q", recs, want)
	}
}
