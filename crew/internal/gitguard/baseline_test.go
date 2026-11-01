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
	want := "git-config baseline: recorded " + r.baseline() + ": branches/.hidden, remotes/foo, core.pager, remote.origin.url\n" +
		"  remote.origin.url=https://***@example.com/r.git [sha256:" + sha8("https://u:tok@example.com/r.git") + "]\n" +
		noteHint(r.baseline())
	if got != want {
		t.Fatalf("note %q, want %q", got, want)
	}
	if strings.Contains(got, "SECRET") || strings.Contains(got, "tok") || strings.Contains(got, "u:") {
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
	want := "git-config baseline: recorded redirect keys in " + r.baseline() + ": remote.origin.url\n" +
		"  remote.origin.url=https://example.com/r.git\n" +
		noteHint(r.baseline())
	if got != want {
		t.Fatalf("note %q, want %q", got, want)
	}
	recs, _ := ReadBaseline(r.baseline())
	wantRecs := []string{LegacyMark, RedirectMark, "core.pager\nless", "remote.origin.url\nhttps://example.com/r.git"}
	if !slices.Equal(recs, wantRecs) {
		t.Fatalf("baseline %q, want %q", recs, wantRecs)
	}
}

// withheld is the text the note prints in place of a value it cannot show.
func withheld(reason string) string {
	return ": value withheld (" + reason + ") — check it in your own terminal, from each checkout or worktree that may set it: "
}

func TestInitMigrationNoteMasksOrWithholdsValues(t *testing.T) {
	r := newRepo(t)
	pairs := [][2]string{
		{"remote.origin.url", "https://u:tok@example.com/o/r.git"},
		{"remote.origin.pushurl", "https://u:tok@[::1]/r.git"},
		{"http.proxy", "user:pw@proxy.example:8080"},
		{"http.https://u:tok@example.com/.proxy", "http://p:pw@[::1]:3128"},
	}
	r.config(t, "core.pager", "less")
	r.config(t, "core.editor", "vim -SECRET")
	var recs []string
	for _, p := range pairs {
		r.config(t, p[0], p[1])
		recs = append(recs, p[0]+"\n"+p[1])
	}
	slices.Sort(recs)
	writeFile(t, r.baseline(), "core.pager\nless\x00")
	got, err := capture(func(w io.Writer) error { return Init(r.common, w) })
	if err != nil {
		t.Fatal(err)
	}
	line := map[string]string{
		"remote.origin.url":                     "  " + ShowKey("remote.origin.url") + "=https://***@example.com/o/r.git [sha256:" + sha8("https://u:tok@example.com/o/r.git") + "]\n",
		"remote.origin.pushurl":                 "  " + ShowKey("remote.origin.pushurl") + withheld("outside the mask grammar") + "git config --show-origin --get-all remote.origin.pushurl\n",
		"http.proxy":                            "  " + ShowKey("http.proxy") + "=***@proxy.example:8080 [sha256:" + sha8("user:pw@proxy.example:8080") + "]\n",
		"http.https://u:tok@example.com/.proxy": "  " + ShowKey("http.https://u:tok@example.com/.proxy") + withheld("outside the mask grammar") + "git config --show-origin --get-regexp " + Quote("^http\\.") + "\n",
	}
	var keys []string
	var lines strings.Builder
	for _, rec := range recs {
		k, _, _ := strings.Cut(rec, "\n")
		keys = append(keys, ShowKey(k))
		lines.WriteString(line[k])
	}
	want := "git-config baseline: recorded redirect keys in " + r.baseline() + ": " + strings.Join(keys, ", ") + "\n" +
		lines.String() + noteHint(r.baseline())
	if got != want {
		t.Fatalf("note:\n%q\nwant:\n%q", got, want)
	}
	for _, leak := range []string{"tok", "pw", "u:", "SECRET"} {
		if strings.Contains(got, leak) {
			t.Fatalf("note shows %q: %q", leak, got)
		}
	}
}

func TestNoteKeysWithholdsUnmaskableKeys(t *testing.T) {
	const (
		kUserinfo = "http.https://u:tok@[::1]/.proxy"
		kQuery    = "http.https://[::1]/?access_token=tok.proxy"
		kExec     = "credential.https://u:tok@[::1]/.helper"
	)
	pairs := [][2]string{
		{kUserinfo, "p:pw@proxy.example:3128"},
		{kQuery, "proxy.example:3128"},
		{kExec, "store"},
	}
	var recs []string
	for _, p := range pairs {
		recs = append(recs, p[0]+"\n"+p[1])
	}
	slices.Sort(recs)
	pointer := func(sec string) string {
		return " — check it in your own terminal, from each checkout or worktree that may set it: git config --show-origin --get-regexp " + Quote("^"+sec+`\.`) + "\n"
	}
	line := map[string]string{
		kUserinfo: "  " + ShowKey(kUserinfo) + "=***@proxy.example:3128 [sha256:" + sha8("p:pw@proxy.example:3128") + "]" + pointer("http"),
		kQuery:    "  " + ShowKey(kQuery) + "=proxy.example:3128" + pointer("http"),
		kExec:     "  " + ShowKey(kExec) + pointer("credential"),
	}
	var keys []string
	var lines strings.Builder
	for _, rec := range recs {
		k, _, _ := strings.Cut(rec, "\n")
		keys = append(keys, ShowKey(k))
		lines.WriteString(line[k])
	}
	const file = "/b/git-config-baseline"
	got, _ := capture(func(w io.Writer) error {
		noteKeys(w, file, file, recs, nil)
		return nil
	})
	want := "git-config baseline: recorded " + file + ": " + strings.Join(keys, ", ") + "\n" +
		lines.String() + noteHint(file)
	if got != want {
		t.Fatalf("note:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(got, "tok") {
		t.Fatalf("note shows the token: %q", got)
	}
}

func TestInitMigrationWithholdsAliasRewrittenRemote(t *testing.T) {
	const url = "ssh://git@example.com/o/r.git"
	for _, tc := range []struct {
		name  string
		alias bool
		line  string
	}{
		{"global alias rewrites the remote", true, "  remote.upstream.url" + withheld("an insteadOf alias rewrites it") + "git config --show-origin --get-all remote.upstream.url\n"},
		{"no alias masks it", false, "  remote.upstream.url=ssh://***@example.com/o/r.git [sha256:" + sha8(url) + "]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			if tc.alias {
				global := filepath.Join(r.root, "gitconfig")
				writeFile(t, global, "[url \"https://mirror.example/\"]\n\tinsteadOf = ssh://git@\n")
				t.Setenv("GIT_CONFIG_GLOBAL", global)
			}
			r.config(t, "remote.upstream.url", url)
			writeFile(t, r.baseline(), "core.pager\nless\x00")
			got, err := capture(func(w io.Writer) error { return Init(r.common, w) })
			if err != nil {
				t.Fatal(err)
			}
			want := "git-config baseline: recorded redirect keys in " + r.baseline() + ": remote.upstream.url\n" +
				tc.line + noteHint(r.baseline())
			if got != want {
				t.Fatalf("note:\n%q\nwant:\n%q", got, want)
			}
		})
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
