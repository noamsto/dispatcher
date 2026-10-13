package piagentdir

import (
	"bytes"
	"fmt"
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
		f.refused(t, "", "crew: "+f.dir+" is a symlink or not a directory — refusing to seed pi worker dir")
	})
	t.Run("regular file", func(t *testing.T) {
		f := newFixture(t)
		write(t, f.dir, "x", 0o644)
		f.refused(t, "", "crew: "+f.dir+" is a symlink or not a directory — refusing to seed pi worker dir")
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
		// Same real path: the dir-inside-ambient check fires first, as in bash.
		f.refused(t, alias, "crew: "+f.dir+" resolves inside the ambient pi dir "+alias+" — refusing to seed pi worker dir")
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
	t.Run("unusable settings reset to an object", func(t *testing.T) {
		f := newFixture(t)
		for _, s := range []string{`[1]`, `{"a":1} {"b":2}`, `{broken`, ``} {
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
	f := newFixture(t)
	f.setConfig(t, `{"localModels":{"Anthropic/m":{"baseUrl":"http://h","contextWindow":1}}}`)
	code, out, errOut := f.run(t, "")
	want := "crew: localModels provider 'Anthropic' has a stored pi credential in auth.json — rename the provider so the credential is never sent to a local endpoint\n"
	if code != 1 || out != "" || errOut != want {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if strings.Contains(errOut, "oauth") {
		t.Error("leaked credential")
	}
}

func TestDispatchConfigFailures(t *testing.T) {
	const refusal = "crew: could not resolve the dispatcher settings for the pi worker models.json — refusing to seed pi worker dir\n"
	f := newFixture(t)
	write(t, f.bin, "#!/bin/sh\nexit 3\n", 0o755)
	if code, out, errOut := f.run(t, ""); code != 1 || out != "" || errOut != refusal {
		t.Fatalf("%d %q %q", code, out, errOut)
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
