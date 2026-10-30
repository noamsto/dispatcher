package gitguard

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func checkRepo(r repo) func(io.Writer) error {
	return func(w io.Writer) error { return Check(r.common, "", w) }
}

func TestCheckPassesOnBaseline(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.pager", "less")
	writeFile(t, r.common+"/remotes/foo", "URL: x\n")
	r.init(t)
	wantPass(t, checkRepo(r))
}

func TestCheckMissingBaseline(t *testing.T) {
	r := newRepo(t)
	wantRefusal(t, "refusing git: no git-config baseline at "+r.baseline()+" — the next dispatch records it, or run `crew git-baseline --accept` from your own terminal\n", checkRepo(r))
}

func TestCheckExecDrift(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	r.config(t, "core.pager", "less")
	cfg := Quote(r.common + "/config")
	wantRefusal(t, "refusing git: core.pager (from "+cfg+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  remove it: git config --file "+cfg+" --unset-all core.pager\n"+clearHint(), checkRepo(r))
}

func TestCheckRedirectDriftHintsEdit(t *testing.T) {
	r := newRepo(t)
	r.config(t, "remote.origin.url", "https://example.com/a.git")
	r.init(t)
	r.config(t, "remote.origin.url", "https://example.com/b.git")
	cfg := Quote(r.common + "/config")
	wantRefusal(t, "refusing git: remote.origin.url (from "+cfg+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  restore the baselined value or remove it: edit "+cfg+"\n"+clearHint(), checkRepo(r))
}

func TestCheckMaskedKeyHintsEdit(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	r.config(t, "http.https://u:tok@example.com/.extraheader", "x")
	r.config(t, "credential.https://u:tok@example.com.helper", "store")
	cfg := Quote(r.common + "/config")
	kd := ShowKey("credential.https://u:tok@example.com.helper")
	wantRefusal(t, "refusing git: "+kd+" (from "+cfg+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  restore the baselined value or remove it: edit "+cfg+"\n"+clearHint(), checkRepo(r))
}

func TestCheckIncludeOrigin(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	inc := filepath.Join(r.root, "inc")
	writeFile(t, inc, "[core]\n\tpager = evil\n")
	r.config(t, "include.path", inc)
	cfg := Quote(r.common + "/config")
	wantRefusal(t, "refusing git: core.pager (from "+Quote(inc)+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  remove it: git config --file "+Quote(inc)+" --unset-all core.pager\n"+
		"refusing git: include.path (from "+cfg+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  remove it: git config --file "+cfg+" --unset-all include.path\n"+clearHint(), checkRepo(r))
}

func TestCheckUnknownOrigin(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	r.config(t, "core.pager", "less")
	shimGit(t, "--show-origin")
	wantRefusal(t, "refusing git: core.pager is not in the git-config baseline "+r.baseline()+"\n"+clearHint(), checkRepo(r))
}

func TestCheckValuelessKey(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	f, err := os.OpenFile(r.common+"/config", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("[core]\n\tfsmonitor\n")
	_ = f.Close()
	cfg := Quote(r.common + "/config")
	wantRefusal(t, "refusing git: core.fsmonitor (from "+cfg+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  remove it: git config --file "+cfg+" --unset-all core.fsmonitor\n"+clearHint(), checkRepo(r))
}

func TestCheckSkipsEmptyRecords(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.pager", "less")
	writeFile(t, r.baseline(), "\x00"+RedirectMark+"\x00\x00"+LegacyMark+"\x00core.pager\nless\x00")
	wantPass(t, checkRepo(r))
}

func TestCheckPre678SkipsRedirectDrift(t *testing.T) {
	r := newRepo(t)
	r.config(t, "remote.origin.url", "https://example.com/b.git")
	writeFile(t, r.baseline(), LegacyMark+"\x00")
	wantPass(t, checkRepo(r))
	r.config(t, "core.pager", "less")
	cfg := Quote(r.common + "/config")
	wantRefusal(t, "refusing git: core.pager (from "+cfg+") is not in the git-config baseline "+r.baseline()+"\n"+
		"  remove it: git config --file "+cfg+" --unset-all core.pager\n"+clearHint(), checkRepo(r))
}

func TestCheckCannotList(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	writeFile(t, r.common+"/config", "[core\n")
	got, err := capture(checkRepo(r))
	if err == nil {
		t.Fatal("passed over an unparsable config")
	}
	if want := "refusing git: cannot list the git config of " + r.common + "\n"; len(got) < len(want) || got[len(got)-len(want):] != want {
		t.Fatalf("stderr %q, want suffix %q", got, want)
	}
}

func TestCheckLegacy(t *testing.T) {
	final := "refusing git: inspect and remove the legacy entry before re-running; the dispatcher never follows or repairs a legacy entry\n"
	unsafe := func(r repo, kind, shown string) string {
		return "refusing git: unsafe legacy " + kind + " entry " + shown + " in " + Quote(r.common) + "\n" + final
	}
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, r repo)
		want  func(r repo) string
	}{
		{"new file", func(t *testing.T, r repo) { writeFile(t, r.common+"/remotes/new", "URL: x\n") },
			func(r repo) string {
				return "refusing git: legacy file entry " + Quote(r.common+"/remotes/new") + " is new or changed from the git-config baseline " + Quote(r.baseline()) + "\n" + final
			}},
		{"changed file", func(t *testing.T, r repo) { writeFile(t, r.common+"/branches/old", "changed\n") },
			func(r repo) string {
				return "refusing git: legacy file entry " + Quote(r.common+"/branches/old") + " is new or changed from the git-config baseline " + Quote(r.baseline()) + "\n" + final
			}},
		{"symlink", func(t *testing.T, r repo) { mustDo(t, os.Symlink(r.main, r.common+"/remotes/l")) },
			func(r repo) string { return unsafe(r, "symlink", Quote(r.common+"/remotes/l")) }},
		{"directory", func(t *testing.T, r repo) { mustDo(t, os.Mkdir(r.common+"/remotes/d", 0o755)) },
			func(r repo) string { return unsafe(r, "directory", Quote(r.common+"/remotes/d")) }},
		{"fifo", func(t *testing.T, r repo) { mustDo(t, syscall.Mkfifo(r.common+"/remotes/p", 0o644)) },
			func(r repo) string { return unsafe(r, "fifo", Quote(r.common+"/remotes/p")) }},
		{"socket", plantSocket, func(r repo) string { return unsafe(r, "socket", Quote(r.common+"/remotes/s")) }},
		{"unsafe name", func(t *testing.T, r repo) { writeFile(t, r.common+"/remotes/a b\x1b", "x") },
			func(r repo) string { return unsafe(r, "unsafe-name", Quote(r.common+"/remotes/")+`$'a b\E'`) }},
		{"symlinked namespace", func(t *testing.T, r repo) {
			mustDo(t, os.RemoveAll(r.common+"/branches"))
			mustDo(t, os.Symlink(r.root, r.common+"/branches"))
		}, func(r repo) string { return unsafe(r, "symlink", Quote(r.common+"/branches/.")) }},
		{"unreadable namespace", func(t *testing.T, r repo) {
			if os.Geteuid() == 0 {
				t.Skip("root reads any dir")
			}
			mustDo(t, os.Chmod(r.common+"/branches", 0o300))
			t.Cleanup(func() { _ = os.Chmod(r.common+"/branches", 0o755) })
		}, func(r repo) string { return unsafe(r, "unreadable-dir", Quote(r.common+"/branches/.")) }},
		{"unreadable file", func(t *testing.T, r repo) {
			if os.Geteuid() == 0 {
				t.Skip("root reads any file")
			}
			mustDo(t, os.Chmod(r.common+"/branches/old", 0))
		}, func(r repo) string { return unsafe(r, "unreadable", Quote(r.common+"/branches/old")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			writeFile(t, r.common+"/branches/old", "old\n")
			mustDo(t, os.Mkdir(r.common+"/remotes", 0o755))
			r.init(t)
			wantPass(t, checkRepo(r))
			tc.plant(t, r)
			wantRefusal(t, tc.want(r), checkRepo(r))
		})
	}
}

func TestCheckLegacyUnmarkedBaseline(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.common+"/remotes/foo", "URL: x\n")
	writeFile(t, r.baseline(), RedirectMark+"\x00")
	wantRefusal(t, "refusing git: legacy file entry "+Quote(r.common+"/remotes/foo")+" is new or changed from the git-config baseline "+Quote(r.baseline())+"\n"+
		"refusing git: inspect and remove the legacy entry before re-running; the dispatcher never follows or repairs a legacy entry\n", checkRepo(r))
}

func plantSocket(t *testing.T, r repo) {
	t.Helper()
	// A unix socket path must fit sun_path (108 bytes); t.TempDir() may not.
	short, err := os.MkdirTemp("", "gg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	l, err := net.Listen("unix", filepath.Join(short, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := l.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = l.Close()
	mustDo(t, os.Rename(filepath.Join(short, "s"), r.common+"/remotes/s"))
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
