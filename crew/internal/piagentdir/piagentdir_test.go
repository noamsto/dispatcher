package piagentdir

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/drift"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

const sampleSettings = `{"localModels":{
  "qwen/big":{"baseUrl":"http://h:1/v1","contextWindow":1000,"reasoning":true,"maxTokens":50,"thinkingFormat":"qwen"},
  "qwen/alpha":{"baseUrl":"http://h:1/v1","contextWindow":2000},
  "other/m":{"baseUrl":"http://o/v1","contextWindow":3,"reasoning":false}}}`

const sampleModels = `{"providers":{
  "other":{"baseUrl":"http://o/v1","api":"openai-completions","apiKey":"other","models":[{"id":"m","contextWindow":3,"reasoning":false}]},
  "qwen":{"baseUrl":"http://h:1/v1","api":"openai-completions","apiKey":"qwen","models":[
    {"id":"alpha","contextWindow":2000},
    {"id":"big","contextWindow":1000,"reasoning":true,"maxTokens":50,"compat":{"thinkingFormat":"qwen"}}]}}}`

type fixture struct {
	home, dir, ambient, bin string
}

// newFixture builds a HOME with an ambient pi dir holding auth.json,
// models-store.json and a bridge, and a stub dispatch-config.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	f := &fixture{home: home, dir: home + "/.pi/dispatcher-worker", ambient: home + "/.pi/agent"}
	write(t, f.ambient+"/auth.json", `{"anthropic":{"type":"oauth"}}`, 0o600)
	write(t, f.ambient+"/models-store.json", `{"x":1}`, 0o600)
	write(t, f.ambient+"/bin/hookyard-bridge.ts", "export default 1\n", 0o755)
	f.setConfig(t, sampleSettings)
	return f
}

func (f *fixture) setConfig(t *testing.T, out string) {
	t.Helper()
	f.bin = filepath.Join(f.home, "dispatch-config")
	write(t, f.bin, "#!/bin/sh\ncat <<'EOF'\n"+out+"\nEOF\n", 0o755)
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) opts(piDir string) Options {
	return Options{Home: f.home, HasHome: true, PIDir: piDir, ConfigBin: f.bin}
}

func (f *fixture) run(t *testing.T, piDir string) (code int, stdout, stderr string) {
	t.Helper()
	var o, e bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Run(&o, &e, f.opts(piDir)) }()
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("seed blocked (FIFO opened?)")
	}
	return code, o.String(), e.String()
}

func (f *fixture) refused(t *testing.T, piDir, wantErr string) {
	t.Helper()
	code, out, errOut := f.run(t, piDir)
	if code != 1 || out != "" || errOut != wantErr+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q, want 1, \"\", %q", code, out, errOut, wantErr)
	}
}

func (f *fixture) ok(t *testing.T) {
	t.Helper()
	code, out, errOut := f.run(t, "")
	if code != 0 || out != f.dir+"\n" || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// snapshot lists a tree — path, mode, type and content hash per entry — so a
// test can assert a seed wrote nothing outside the worker dir. A link is also
// its target and the bytes reached through it: the bats case hashed auth.json
// with sha256sum, which followed the link.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v %v\n", rel, fi.Mode().Perm(), fi.Mode().Type())
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			tgt, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, " -> %s\n", tgt)
			if c, err := os.ReadFile(p); err == nil {
				fmt.Fprintf(&b, "%x\n", sha256.Sum256(c))
			}
		case fi.Mode().IsRegular():
			c, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%x\n", sha256.Sum256(c))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// noCopiedSecret is the bats case's `grep -rF SECRET-FIXTURE-123 "$WORKER"`: no
// regular file the seed wrote may hold a credential, only the link may reach
// one. grep -r does not follow the links it walks, so neither does this.
func noCopiedSecret(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		c, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(c), secret) {
			t.Errorf("%s holds a copied credential", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func absent(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	return errors.Is(err, fs.ErrNotExist)
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	if g, w := testjson.Compact(testjson.MustParse(t, got)), testjson.Compact(testjson.MustParse(t, want)); g != w {
		t.Fatalf("got %s\nwant %s", g, w)
	}
}

func TestHappyPath(t *testing.T) {
	f := newFixture(t)
	f.ok(t)
	if m := mode(t, f.dir); m != 0o700 {
		t.Errorf("dir mode %o", m)
	}
	sameJSON(t, read(t, f.dir+"/settings.json"),
		fmt.Sprintf(`{"defaultProjectTrust":"never","extensions":[%q]}`, f.dir+"/bin/hookyard-bridge.ts"))
	if m := mode(t, f.dir+"/settings.json"); m != 0o644 {
		t.Errorf("settings mode %o", m)
	}
	if got, err := os.Readlink(f.dir + "/auth.json"); err != nil || got != f.ambient+"/auth.json" {
		t.Errorf("auth link %q %v", got, err)
	}
	if read(t, f.dir+"/models-store.json") != `{"x":1}` || mode(t, f.dir+"/models-store.json") != 0o644 {
		t.Error("models-store not copied 0644")
	}
	if read(t, f.dir+"/bin/hookyard-bridge.ts") != "export default 1\n" {
		t.Error("bridge not copied")
	}
	sameJSON(t, read(t, f.dir+"/models.json"), sampleModels)
	if m := mode(t, f.dir+"/models.json"); m != 0o644 {
		t.Errorf("models mode %o", m)
	}
}

func TestIdempotent(t *testing.T) {
	f := newFixture(t)
	f.ok(t)
	old := time.Unix(1_000_000_000, 0)
	files := []string{"settings.json", "models.json", "models-store.json", "bin/hookyard-bridge.ts"}
	type id struct {
		ino   uint64
		mtime time.Time
	}
	stat := func(p string, lstat bool) id {
		var fi os.FileInfo
		var err error
		if lstat {
			fi, err = os.Lstat(p)
		} else {
			fi, err = os.Stat(p)
		}
		if err != nil {
			t.Fatal(err)
		}
		return id{fi.Sys().(*syscall.Stat_t).Ino, fi.ModTime()}
	}
	for _, n := range files {
		if err := os.Chtimes(f.dir+"/"+n, old, old); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]id{}
	for _, n := range files {
		before[n] = stat(f.dir+"/"+n, false)
	}
	before["auth.json"] = stat(f.dir+"/auth.json", true)
	f.ok(t)
	for n, b := range before {
		if a := stat(f.dir+"/"+n, n == "auth.json"); a != b {
			t.Errorf("%s rewritten: %v -> %v", n, b, a)
		}
	}
	ents, _ := filepath.Glob(f.dir + "/.seed.*")
	more, _ := filepath.Glob(f.dir + "/bin/.seed.*")
	if len(ents)+len(more) != 0 {
		t.Errorf("leftover temps %v %v", ents, more)
	}
}

func TestWorkerDirRefusals(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Symlink(f.ambient, f.dir); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, f.ambient)
		f.refused(t, "", "crew: "+f.dir+" is a symlink or not a directory — refusing to seed pi worker dir")
		// Every write would have landed inside the ambient dir, and the chmod 700
		// would have widened it.
		if got := snapshot(t, f.ambient); got != before {
			t.Errorf("ambient dir written through the worker symlink:\n%s\nwant\n%s", got, before)
		}
	})
	t.Run("regular file", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.dir, "x", 0o644)
		f.refused(t, "", "crew: "+f.dir+" is a symlink or not a directory — refusing to seed pi worker dir")
		if got := read(t, f.dir); got != "x" {
			t.Errorf("the file was replaced: %q", got)
		}
	})
	t.Run("worker dir inside ambient", func(t *testing.T) {
		f := newFixture(t)
		f.refused(t, f.home+"/.pi", "crew: "+f.dir+" resolves inside the ambient pi dir "+f.home+"/.pi — refusing to seed pi worker dir")
	})
	t.Run("ambient inside worker dir via alias", func(t *testing.T) {
		f := newFixture(t)
		f.ok(t)
		write(t, f.dir+"/sub/auth.json", "{}", 0o600)
		alias := f.home + "/alias"
		if err := os.Symlink(f.dir+"/sub", alias); err != nil {
			t.Fatal(err)
		}
		f.refused(t, alias, "crew: ambient pi dir "+alias+" resolves inside "+f.dir+" — refusing to seed pi worker dir")
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.dir, alias); err != nil {
			t.Fatal(err)
		}
		seeded := snapshot(t, f.dir)
		// Same real path: the dir-inside-ambient check fires first, as in bash.
		f.refused(t, alias, "crew: "+f.dir+" resolves inside the ambient pi dir "+alias+" — refusing to seed pi worker dir")
		if got := snapshot(t, f.dir); got != seeded {
			t.Errorf("an aliased PI_CODING_AGENT_DIR rewrote the worker dir:\n%s\nwant\n%s", got, seeded)
		}
	})
	t.Run("relative HOME", func(t *testing.T) {
		root := t.TempDir()
		t.Chdir(root)
		write(t, root+"/h/.pi/dispatcher-worker/sub/auth.json", "{}", 0o600)
		var o, e bytes.Buffer
		code := Run(&o, &e, Options{Home: "h", HasHome: true, PIDir: root + "/h/.pi/dispatcher-worker/sub"})
		want := "crew: ambient pi dir " + root + "/h/.pi/dispatcher-worker/sub resolves inside h/.pi/dispatcher-worker — refusing to seed pi worker dir\n"
		if code != 1 || o.Len() != 0 || e.String() != want {
			t.Fatalf("%d %q %q", code, o.String(), e.String())
		}
	})
	t.Run("relative HOME via symlinked cwd, worker dir inside ambient", func(t *testing.T) {
		root := chdirViaSymlink(t)
		write(t, root+"/h/.pi/dispatcher-worker/sub/auth.json", "{}", 0o600)
		var o, e bytes.Buffer
		code := Run(&o, &e, Options{Home: "h", HasHome: true, PIDir: root + "/h/.pi/dispatcher-worker/sub"})
		want := "crew: ambient pi dir " + root + "/h/.pi/dispatcher-worker/sub resolves inside h/.pi/dispatcher-worker — refusing to seed pi worker dir\n"
		if code != 1 || o.Len() != 0 || e.String() != want {
			t.Fatalf("%d %q %q", code, o.String(), e.String())
		}
	})
	t.Run("relative HOME via symlinked cwd, ambient inside worker dir", func(t *testing.T) {
		root := chdirViaSymlink(t)
		write(t, root+"/h/.pi/dispatcher-worker/sub/auth.json", "{}", 0o600)
		var o, e bytes.Buffer
		code := Run(&o, &e, Options{Home: "h", HasHome: true, PIDir: root + "/h"})
		want := "crew: h/.pi/dispatcher-worker resolves inside the ambient pi dir " + root + "/h — refusing to seed pi worker dir\n"
		if code != 1 || o.Len() != 0 || e.String() != want {
			t.Fatalf("%d %q %q", code, o.String(), e.String())
		}
	})
	t.Run("unsearchable ambient", func(t *testing.T) {
		skipRoot(t)
		f := newFixture(t)
		if err := os.Chmod(f.ambient, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(f.ambient, 0o700) })
		f.refused(t, "", "crew: cd: "+f.ambient+": Permission denied")
	})
	t.Run("HOME unset", func(t *testing.T) {
		var o, e bytes.Buffer
		if code := Run(&o, &e, Options{}); code != 1 || o.Len() != 0 ||
			e.String() != "crew: HOME is unset — refusing to seed pi worker dir\n" {
			t.Fatalf("%d %q %q", code, o.String(), e.String())
		}
	})
}

func skipRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
}

// chdirViaSymlink enters the temp root through a symlink so os.Getwd reports the
// logical path, and returns the physical root.
func chdirViaSymlink(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link)
	t.Setenv("PWD", link)
	return root
}

func TestExtensions(t *testing.T) {
	for _, bad := range []string{`{"not":"an array"}`, `false`, `"a string"`, `3`} {
		t.Run(bad, func(t *testing.T) {
			f := newFixture(t)
			f.ok(t)
			content := `{"extensions":` + bad + `,"keep":1}`
			write(t, f.dir+"/settings.json", content, 0o644)
			f.refused(t, "", "crew: "+f.dir+"/settings.json has a non-array extensions value — refusing to seed pi worker dir")
			if read(t, f.dir+"/settings.json") != content {
				t.Error("settings.json changed")
			}
		})
	}
	t.Run("non-array tolerated without bridge", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Remove(f.ambient + "/bin/hookyard-bridge.ts"); err != nil {
			t.Fatal(err)
		}
		write(t, f.dir+"/settings.json", `{"extensions":3}`, 0o644)
		f.ok(t)
		sameJSON(t, read(t, f.dir+"/settings.json"), `{"extensions":3,"defaultProjectTrust":"never"}`)
	})
	t.Run("no bridge means no extensions key", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Remove(f.ambient + "/bin/hookyard-bridge.ts"); err != nil {
			t.Fatal(err)
		}
		f.ok(t)
		sameJSON(t, read(t, f.dir+"/settings.json"), `{"defaultProjectTrust":"never"}`)
		// A dangling entry would make pi fail to load an extension that is not there.
		if !absent(t, f.dir+"/bin/hookyard-bridge.ts") {
			t.Error("worker bridge seeded without an ambient one")
		}
	})
	t.Run("appended once, pi order kept, null extensions", func(t *testing.T) {
		f := newFixture(t)
		bridge := f.dir + "/bin/hookyard-bridge.ts"
		write(t, f.dir+"/settings.json", `{"extensions":["b","a"],"theme":"x"}`, 0o644)
		f.ok(t)
		f.ok(t)
		sameJSON(t, read(t, f.dir+"/settings.json"),
			fmt.Sprintf(`{"extensions":["b","a",%q],"theme":"x","defaultProjectTrust":"never"}`, bridge))
		write(t, f.dir+"/settings.json", `{"extensions":null}`, 0o644)
		f.ok(t)
		sameJSON(t, read(t, f.dir+"/settings.json"),
			fmt.Sprintf(`{"extensions":[%q],"defaultProjectTrust":"never"}`, bridge))
	})
	t.Run("pi-written settings survive, trust forced to never", func(t *testing.T) {
		f := newFixture(t)
		f.ok(t)
		// pi writes its own keys here; only defaultProjectTrust is crew's.
		write(t, f.dir+"/settings.json", `{"lastChangelogVersion":"9","defaultProjectTrust":"always"}`, 0o644)
		f.ok(t)
		sameJSON(t, read(t, f.dir+"/settings.json"),
			fmt.Sprintf(`{"lastChangelogVersion":"9","defaultProjectTrust":"never","extensions":[%q]}`, f.dir+"/bin/hookyard-bridge.ts"))
	})
	t.Run("unusable settings reset to an object", func(t *testing.T) {
		f := newFixture(t)
		for _, s := range []string{`[1]`, `{"a":1} {"b":2}`, `{broken`, `null`, ``} {
			write(t, f.dir+"/settings.json", s, 0o644)
			f.ok(t)
			if strings.Contains(read(t, f.dir+"/settings.json"), `"a"`) {
				t.Errorf("%q kept", s)
			}
		}
	})
}

func (f *fixture) seededThenGone(t *testing.T) {
	t.Helper()
	f.ok(t)
	if err := os.Remove(f.ambient + "/auth.json"); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) authKept(t *testing.T) {
	t.Helper()
	if got, err := os.Readlink(f.dir + "/auth.json"); err != nil || got != f.ambient+"/auth.json" {
		t.Errorf("worker auth link not kept: %q %v", got, err)
	}
}

func TestAuthRefusals(t *testing.T) {
	notReachable := func(f *fixture) string {
		return "crew: " + f.ambient + "/auth.json is not a reachable regular file — refusing to seed pi credentials"
	}
	unreadable := func(f *fixture) string {
		return "crew: " + f.ambient + "/auth.json is unreadable or not a JSON object — refusing to seed pi credentials"
	}
	cases := []struct {
		name  string
		skip  bool
		setup func(t *testing.T, f *fixture)
		want  func(f *fixture) string
	}{
		{"malformed", false, func(t *testing.T, f *fixture) { write(t, f.ambient+"/auth.json", `{`, 0o600) }, unreadable},
		{"array", false, func(t *testing.T, f *fixture) { write(t, f.ambient+"/auth.json", `[]`, 0o600) }, unreadable},
		{"empty", false, func(t *testing.T, f *fixture) { write(t, f.ambient+"/auth.json", ``, 0o600) }, unreadable},
		{"mode 000", true, func(t *testing.T, f *fixture) { write(t, f.ambient+"/auth.json", `{}`, 0) }, unreadable},
		{"fifo", false, func(t *testing.T, f *fixture) {
			if err := syscall.Mkfifo(f.ambient+"/auth.json", 0o600); err != nil {
				t.Fatal(err)
			}
		}, notReachable},
		{"dangling link", false, func(t *testing.T, f *fixture) {
			mustSymlink(t, f.home+"/unmounted/auth.json", f.ambient+"/auth.json")
		}, notReachable},
		{"looping link", false, func(t *testing.T, f *fixture) {
			mustSymlink(t, f.ambient+"/auth.json", f.ambient+"/auth.json")
		}, notReachable},
		{"link to dir", false, func(t *testing.T, f *fixture) {
			mustSymlink(t, f.home, f.ambient+"/auth.json")
		}, notReachable},
		{"directory", false, func(t *testing.T, f *fixture) {
			if err := os.Mkdir(f.ambient+"/auth.json", 0o700); err != nil {
				t.Fatal(err)
			}
		}, notReachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.skip {
				skipRoot(t)
			}
			f := newFixture(t)
			f.seededThenGone(t)
			c.setup(t, f)
			f.refused(t, "", c.want(f))
			f.authKept(t)
		})
	}

	t.Run("behind an unsearchable dir", func(t *testing.T) {
		skipRoot(t)
		f := newFixture(t)
		f.seededThenGone(t)
		sub := f.home + "/locked"
		write(t, sub+"/agent/x", "", 0o600)
		ambient := sub + "/agent"
		if err := os.Chmod(sub, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
		f.refused(t, ambient, "crew: "+ambient+"/auth.json is not a reachable regular file — refusing to seed pi credentials")
		f.authKept(t)
	})
	t.Run("link into an unsearchable dir", func(t *testing.T) {
		skipRoot(t)
		f := newFixture(t)
		f.seededThenGone(t)
		locked := f.home + "/locked"
		write(t, locked+"/auth.json", "{}", 0o600)
		mustSymlink(t, locked+"/auth.json", f.ambient+"/auth.json")
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		// Cleanup runs LIFO, so the mode is restored before t.TempDir's rm -rf.
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		f.refused(t, "", notReachable(f))
		f.authKept(t)
	})
	t.Run("dangling ambient dir symlink", func(t *testing.T) {
		f := newFixture(t)
		f.seededThenGone(t)
		amb := f.home + "/ambient-link"
		mustSymlink(t, f.home+"/unmounted", amb)
		f.refused(t, amb, "crew: "+amb+"/auth.json is not a reachable regular file — refusing to seed pi credentials")
		f.authKept(t)
	})
	t.Run("newline-suffixed component", func(t *testing.T) {
		f := newFixture(t)
		f.seededThenGone(t)
		if err := os.MkdirAll(f.home+"/.pi/weird", 0o755); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, f.home+"/unmounted", f.home+"/.pi/weird\n")
		amb := f.home + "/.pi/weird\n/agent"
		f.refused(t, amb, "crew: "+amb+"/auth.json is not a reachable regular file — refusing to seed pi credentials")
		f.authKept(t)
	})
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestNoAmbientAuth(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(f.ambient + "/auth.json"); err != nil {
		t.Fatal(err)
	}
	f.ok(t)
	if fi, _ := os.Lstat(f.dir + "/auth.json"); fi.Mode().IsRegular() != true || mode(t, f.dir+"/auth.json") != 0o600 || read(t, f.dir+"/auth.json") != "{}\n" {
		t.Error("auth.json not an empty 0600 object")
	}
	if err := os.RemoveAll(f.ambient); err != nil {
		t.Fatal(err)
	}
	f.ok(t)
	if read(t, f.dir+"/auth.json") != "{}\n" {
		t.Error("missing ambient dir not seeded empty")
	}
}

func TestAmbientPaths(t *testing.T) {
	f := newFixture(t)
	f.ok(t)
	write(t, f.home+"/rel/auth.json", `{}`, 0o600)
	f.ok(t)
	for _, set := range []string{"relative/dir", f.dir, "~/.pi/agent", f.ambient + "/"} {
		code, _, errOut := f.run(t, set)
		if code != 0 || errOut != "" {
			t.Fatalf("%q: %d %q", set, code, errOut)
		}
		if got, _ := os.Readlink(f.dir + "/auth.json"); got != f.ambient+"/auth.json" {
			t.Errorf("%q: ambient resolved to %q", set, got)
		}
	}
	write(t, f.home+"/other/auth.json", `{}`, 0o600)
	if code, _, _ := f.run(t, "~/other"); code != 0 {
		t.Fatal("~/ expansion failed")
	}
	if got, _ := os.Readlink(f.dir + "/auth.json"); got != f.home+"/other/auth.json" {
		t.Errorf("~/other linked %q", got)
	}
}

func TestSettingsClash(t *testing.T) {
	const clash = " has a stored pi credential in auth.json — rename the provider so the credential is never sent to a local endpoint\n"
	t.Run("oauth credential", func(t *testing.T) {
		f := newFixture(t)
		f.setConfig(t, `{"localModels":{"Anthropic/m":{"baseUrl":"http://h","contextWindow":1}}}`)
		code, out, errOut := f.run(t, "")
		if code != 1 || out != "" || errOut != "crew: localModels provider 'Anthropic'"+clash {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		if strings.Contains(errOut, "oauth") {
			t.Error("leaked credential")
		}
	})
	t.Run("stored api_key", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.ambient+"/auth.json", `{"openai":{"type":"api_key","key":"sk-test-fixture"}}`, 0o600)
		f.setConfig(t, `{"localModels":{"OpenAI/gpt-oss-120b":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}`)
		code, out, errOut := f.run(t, "")
		if code != 1 || out != "" || errOut != "crew: localModels provider 'OpenAI'"+clash {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
		if strings.Contains(errOut, "sk-test-fixture") {
			t.Error("the stored credential reached stderr")
		}
		// pi prefers a stored credential over models.json's dummy apiKey, so the
		// worker must never see the file at all.
		if !absent(t, f.dir+"/models.json") {
			t.Error("a clashing localModels provider still seeded models.json")
		}
	})
}

func TestDispatchConfigFailures(t *testing.T) {
	const refusal = "crew: could not resolve the dispatcher settings for the pi worker models.json — refusing to seed pi worker dir\n"
	f := newFixture(t)
	write(t, f.bin, "#!/bin/sh\nexit 3\n", 0o755)
	if code, out, errOut := f.run(t, ""); code != 1 || out != "" || errOut != refusal {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	// dispatch-config validates localModels on the merged tree, so an entry it
	// rejects (a missing baseUrl, say) lands here: named in the refusal, and
	// nothing seeded.
	if !absent(t, f.dir+"/models.json") {
		t.Error("unresolved settings still seeded models.json")
	}
	f.bin = f.home + "/missing"
	if code, out, errOut := f.run(t, ""); code != 1 || out != "" || !strings.HasSuffix(errOut, refusal) {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	f.setConfig(t, `not json`)
	if code, out, errOut := f.run(t, ""); code != 2 || out != "" || errOut != "jq: Invalid JSON text passed to --argjson\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

// A number literal keeps its own text through the seed: settings.json values are
// carried raw and the jq hand-off is json.Number, so nothing goes through
// float64 (which would print 18446744073709552000).
func TestNumberLiteralsKeepTheirText(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"localModels":{"p/m":{"baseUrl":"http://h/v1","contextWindow":18446744073709551617,"maxTokens":1.50}}}`)
	write(t, f.dir+"/settings.json", `{"contextWindow":18446744073709551617,"theme":"x"}`, 0o644)
	f.ok(t)
	if got := read(t, f.dir+"/settings.json"); !strings.Contains(got, "18446744073709551617") {
		t.Errorf("settings.json lost the literal: %s", got)
	}
	if got := read(t, f.dir+"/models.json"); !strings.Contains(got, "18446744073709551617") || !strings.Contains(got, "1.50") {
		t.Errorf("models.json lost a literal: %s", got)
	}
}

// #1141: pi registers a custom model as text-only unless models.json carries
// `input`, so the entry's array has to reach it verbatim — and an entry that
// declares none gains no `input` key at all.
func TestInputFieldPassthrough(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"localModels":{
  "p/vision":{"baseUrl":"http://h:1/v1","contextWindow":1000,"input":["text","image"]},
  "p/plain":{"baseUrl":"http://h:1/v1","contextWindow":2000}}}`)
	f.ok(t)
	sameJSON(t, read(t, f.dir+"/models.json"), `{"providers":{"p":{
  "baseUrl":"http://h:1/v1","api":"openai-completions","apiKey":"p","models":[
    {"id":"plain","contextWindow":2000},
    {"id":"vision","contextWindow":1000,"input":["text","image"]}]}}}`)
}

func TestModelsJSONFromLocalModels(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"localModels":{
      "lemonade/Qwen3.8-Flash-Next-MTP":{"baseUrl":"http://halo.test:13305/v1","contextWindow":131072},
      "lemonade/Alpha":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}`)
	f.ok(t)
	sameJSON(t, read(t, f.dir+"/models.json"), `{"providers":{"lemonade":{
      "baseUrl":"http://halo.test:13305/v1","api":"openai-completions","apiKey":"lemonade","models":[
        {"id":"Alpha","contextWindow":4096},
        {"id":"Qwen3.8-Flash-Next-MTP","contextWindow":131072}]}}}`)
	// localModels is dispatcher-owned: dropping the entry has to stop the model
	// being reachable, or a retired endpoint keeps a worker pointed at it.
	f.setConfig(t, `{}`)
	f.ok(t)
	sameJSON(t, read(t, f.dir+"/models.json"), `{"providers":{}}`)
}

func TestMaxTokensOnlyWhenSet(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"localModels":{
      "lemonade/Big":{"baseUrl":"http://halo.test:13305/v1","contextWindow":131072,"maxTokens":65536},
      "lemonade/Default":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096}}}`)
	f.ok(t)
	sameJSON(t, read(t, f.dir+"/models.json"), `{"providers":{"lemonade":{
      "baseUrl":"http://halo.test:13305/v1","api":"openai-completions","apiKey":"lemonade","models":[
        {"id":"Big","contextWindow":131072,"maxTokens":65536},
        {"id":"Default","contextWindow":4096}]}}}`)
	// An entry without the field comes out without the key: `"maxTokens":null`
	// is what makes pi reject the model rather than default it.
	if n := strings.Count(read(t, f.dir+"/models.json"), "maxTokens"); n != 1 {
		t.Errorf("maxTokens appears %d times in %s", n, read(t, f.dir+"/models.json"))
	}
}

func TestReasoningFieldsPassthrough(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"localModels":{
      "lemonade/R":{"baseUrl":"http://halo.test:13305/v1","contextWindow":4096,"reasoning":true,
        "thinkingFormat":"qwen-chat-template","thinkingLevelMap":{"minimal":null},
        "samplingParams":{"temperature":0.6},"samplingParamsByThinkingLevel":{"off":{"temperature":0.7}},
        "effortThinking":{"low":"off"}},
      "lemonade/P":{"baseUrl":"http://halo.test:13305/v1","contextWindow":2048}}}`)
	f.ok(t)
	// Only the fields pi reads: `thinkingFormat` under `compat`, and nothing else
	// — effortThinking is a dispatcher-side mapping, never pi's.
	sameJSON(t, read(t, f.dir+"/models.json"), `{"providers":{"lemonade":{
      "baseUrl":"http://halo.test:13305/v1","api":"openai-completions","apiKey":"lemonade","models":[
        {"id":"P","contextWindow":2048},
        {"id":"R","contextWindow":4096,"reasoning":true,
         "compat":{"thinkingFormat":"qwen-chat-template"},
         "thinkingLevelMap":{"minimal":null},
         "samplingParams":{"temperature":0.6},
         "samplingParamsByThinkingLevel":{"off":{"temperature":0.7}}}]}}}`)
}

// The api_key-only filter used to drop everything else, leaving an oauth-only
// machine with {} (#198). The link hands pi the file unparsed.
func TestAuthLinkKeepsEveryEntry(t *testing.T) {
	const sixEntries = `{
  "opencode": {"type": "api_key", "key": "SECRET-FIXTURE-123"},
  "openrouter": {"type": "api_key", "key": "$OPENROUTER_API_KEY", "env": {"X": "y"}},
  "deepseek": {"type": "api_key", "key": "!echo x"},
  "anthropic": {"type": "oauth", "access": "a", "refresh": "r", "expires": 1},
  "a b": {"type": "api_key", "key": "bad-name"},
  "lit": {"type": "api_key", "key": "sk-a$b"}
}`
	f := newFixture(t)
	write(t, f.ambient+"/auth.json", sixEntries, 0o600)
	f.ok(t)
	if got, err := os.Readlink(f.dir + "/auth.json"); err != nil || got != f.ambient+"/auth.json" {
		t.Fatalf("auth link %q %v", got, err)
	}
	sameJSON(t, read(t, f.dir+"/auth.json"), sixEntries)
	noCopiedSecret(t, f.dir, "SECRET-FIXTURE-123")
}

func TestAuthRefreshWritesThrough(t *testing.T) {
	f := newFixture(t)
	const seeded = `{"anthropic":{"type":"oauth","access":"a"}}`
	write(t, f.ambient+"/auth.json", seeded, 0o600)
	f.ok(t)
	// pi rewrites auth.json in place on a token refresh; through the link that
	// must land in the ambient file, not strand a divergent copy in the shared
	// worker dir.
	refreshed := `{"anthropic":{"type":"oauth","access":"refreshed"}}`
	if err := os.WriteFile(f.dir+"/auth.json", []byte(refreshed), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(f.dir + "/auth.json"); err != nil || got != f.ambient+"/auth.json" {
		t.Fatalf("the refresh replaced the link: %q %v", got, err)
	}
	if got := read(t, f.ambient + "/auth.json"); got != refreshed {
		t.Errorf("ambient auth.json is %q, want the refresh", got)
	}
}

func TestAmbientDirUntouched(t *testing.T) {
	f := newFixture(t)
	write(t, f.ambient+"/settings.json", `{"defaultProjectTrust":"always"}`, 0o644)
	write(t, f.ambient+"/trust.json", "{}", 0o644)
	before := snapshot(t, f.ambient)
	f.ok(t)
	if got := snapshot(t, f.ambient); got != before {
		t.Errorf("the seed wrote into the ambient pi dir:\n%s\nwant\n%s", got, before)
	}
	// trust.json is the user's per-project decision; seeding it would hand a
	// worker the ambient trust the never above exists to withhold.
	if !absent(t, f.dir+"/trust.json") {
		t.Error("trust.json seeded in the worker dir")
	}
}

func TestModelCatalog(t *testing.T) {
	const catalog = `{"models":[{"id":"deepseek/deepseek-v4.1-flash"}]}`
	t.Run("copied not linked", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.ambient+"/models-store.json", catalog, 0o600)
		f.ok(t)
		p := f.dir + "/models-store.json"
		// Every worker shares this dir; a link would aim N concurrent refreshes
		// at the user's real catalog.
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&fs.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			t.Fatalf("catalog is not a regular copy: %v", err)
		}
		if read(t, p) != catalog || mode(t, p) != 0o644 {
			t.Errorf("catalog %q mode %o", read(t, p), mode(t, p))
		}
	})
	t.Run("re-seed repairs a corrupted copy", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.ambient+"/models-store.json", catalog, 0o600)
		f.ok(t)
		write(t, f.dir+"/models-store.json", "torn{\n", 0o644)
		f.ok(t)
		sameJSON(t, read(t, f.dir+"/models-store.json"), catalog)
	})
	t.Run("no ambient catalog leaves none", func(t *testing.T) {
		f := newFixture(t)
		if err := os.Remove(f.ambient + "/models-store.json"); err != nil {
			t.Fatal(err)
		}
		f.ok(t)
		if !absent(t, f.dir+"/models-store.json") {
			t.Error("a worker catalog seeded without an ambient one")
		}
	})
}

func TestRelativeAmbientAuthSymlinkResolves(t *testing.T) {
	f := newFixture(t)
	write(t, f.home+"/.pi/secrets/auth.json", `{"opencode":{"type":"api_key","key":"SECRET-FIXTURE-123"}}`, 0o600)
	if err := os.Remove(f.ambient + "/auth.json"); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "../secrets/auth.json", f.ambient+"/auth.json")
	f.ok(t)
	if got, err := os.Readlink(f.dir + "/auth.json"); err != nil || got != f.ambient+"/auth.json" {
		t.Fatalf("worker link %q %v", got, err)
	}
	// The worker link is absolute, so the ambient link's relative target resolves
	// against the ambient dir — not the worker dir and not pi's cwd.
	t.Chdir(t.TempDir())
	if !strings.Contains(read(t, f.dir+"/auth.json"), "SECRET-FIXTURE-123") {
		t.Error("the relative ambient auth.json link does not read through")
	}
}

func TestMatchesBash(t *testing.T) {
	path := "../../../adapters/core/local-models.sh"
	b, err := os.ReadFile(path)
	if err != nil {
		drift.Missing(t, path, err)
	}
	m := regexp.MustCompile(`(?s)_local_pi_models_json\(\) \{\n  jq '(.*?)' <<<"\$1"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("could not find _local_pi_models_json's jq program")
	}
	var embedded []string
	for _, l := range strings.Split(modelsProg, "\n") {
		if !strings.HasPrefix(l, "#") {
			embedded = append(embedded, l)
		}
	}
	if got, want := strings.TrimSpace(strings.Join(embedded, "\n")), strings.TrimSpace(string(m[1])); got != want {
		t.Fatalf("models.jq drifted from local-models.sh\nembedded:\n%s\nbash:\n%s", got, want)
	}
}
