package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const atStr = "2026-11-19T01:02:03Z"

type env struct {
	common string
	vars   map[string]string
	o      Options
}

// setup is a repo common dir and Options whose every probe is fake, with the
// state dir and HOME under t.TempDir().
func setup(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	home := filepath.Join(root, "home")
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("HOME", home)
	common := filepath.Join(root, "repo", ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	e := &env{common: common, vars: map[string]string{"XDG_STATE_HOME": state, "HOME": home}}
	e.o = Options{
		Getenv:   func(k string) string { return e.vars[k] },
		IsTTY:    func() bool { return true },
		TTYName:  func() string { return "/dev/pts/9" },
		User:     func() (string, error) { return "owner", nil },
		Hostname: func() (string, error) { return "h1", nil },
		Now:      func() time.Time { return time.Date(2026, 11, 19, 1, 2, 3, 0, time.UTC) },
		UID:      os.Getuid,
		Sender: sender.Probes{
			Tmux: &tmuxtest.Fake{Out: map[string]string{"list-panes": ""}},
			Proc: proctest.New(nil, map[int]int{os.Getpid(): 200, 200: 100, 100: 1}),
		},
	}
	return e
}

func (e *env) run(t *testing.T, req Request) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(t.Context(), req, e.common, &out, &errb, e.o)
	return code, out.String(), errb.String()
}

func (e *env) dir(t *testing.T) (string, string) {
	t.Helper()
	dir, real, err := Dir(e.common, e.o.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return dir, real
}

func (e *env) set(t *testing.T, name, text string) {
	t.Helper()
	if code, _, stderr := e.run(t, Request{Action: "set", Name: name, Text: text}); code != 0 {
		t.Fatalf("set %s: exit %d: %s", name, code, stderr)
	}
}

// writeRaw puts body at the policy file for name with mode 0600.
func (e *env) writeRaw(t *testing.T, name string, body []byte) string {
	t.Helper()
	dir, _ := e.dir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, name+".json")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// record is the record set would write for name in e's repo.
func (e *env) record(t *testing.T, name string) map[string]any {
	t.Helper()
	_, real := e.dir(t)
	return map[string]any{
		"name": name, "text": "merge when green", "repo": real, "by": "owner",
		"host": "h1", "uid": os.Getuid(), "tty": "/dev/pts/9", "at": atStr,
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const nightLine = "policy night — set by owner@h1 from /dev/pts/9 at " + atStr + ", verbatim:\n  merge when green; then start the next issue\n"

func TestHappyPath(t *testing.T) {
	e := setup(t)
	text := "merge when green; then start the next issue"

	code, stdout, stderr := e.run(t, Request{Action: "set", Name: "night", Text: text})
	if code != 0 || stdout != nightLine || stderr != "" {
		t.Fatalf("set = %d %q %q", code, stdout, stderr)
	}
	dir, real := e.dir(t)
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v", fi, err)
	}
	fi, err = os.Stat(filepath.Join(dir, "night.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, %v", fi, err)
	}

	if code, stdout, _ := e.run(t, Request{Action: "show", Name: "night"}); code != 0 || stdout != nightLine {
		t.Fatalf("show = %d %q", code, stdout)
	}
	code, stdout, _ = e.run(t, Request{Action: "show", Name: "night", JSON: true})
	var r Record
	if code != 0 || json.Unmarshal([]byte(stdout), &r) != nil {
		t.Fatalf("show --json = %d %q", code, stdout)
	}
	want := Record{Name: "night", Text: text, Repo: real, By: "owner", Host: "h1", UID: os.Getuid(), TTY: "/dev/pts/9", At: atStr}
	if r != want {
		t.Fatalf("show --json = %+v, want %+v", r, want)
	}

	if code, stdout, _ := e.run(t, Request{Action: "list"}); code != 0 || stdout != nightLine {
		t.Fatalf("list = %d %q", code, stdout)
	}
	wantBrief := "'night' set by owner@h1 from /dev/pts/9 at " + atStr + ": \"" + text + "\"\n"
	if code, stdout, _ := e.run(t, Request{Action: "list", Brief: true}); code != 0 || stdout != wantBrief {
		t.Fatalf("list --brief = %d %q", code, stdout)
	}
	code, stdout, _ = e.run(t, Request{Action: "list", JSON: true})
	var rs []Record
	if code != 0 || json.Unmarshal([]byte(stdout), &rs) != nil || len(rs) != 1 || rs[0] != want {
		t.Fatalf("list --json = %d %q", code, stdout)
	}

	if code, stdout, _ := e.run(t, Request{Action: "clear", Name: "night"}); code != 0 || stdout != "cleared standing policy night\n" {
		t.Fatalf("clear = %d %q", code, stdout)
	}
	if code, _, stderr := e.run(t, Request{Action: "show", Name: "night"}); code != 1 || stderr != "crew: policy: no standing policy night in this repo\n" {
		t.Fatalf("show after clear = %d %q", code, stderr)
	}
	if code, _, stderr := e.run(t, Request{Action: "clear", Name: "night"}); code != 1 || stderr != "crew: policy: no standing policy night\n" {
		t.Fatalf("clear missing = %d %q", code, stderr)
	}
	if code, stdout, _ := e.run(t, Request{Action: "list"}); code != 0 || stdout != "no standing policy in this repo\n" {
		t.Fatalf("empty list = %d %q", code, stdout)
	}
	if code, stdout, _ := e.run(t, Request{Action: "list", Brief: true}); code != 0 || stdout != "" {
		t.Fatalf("empty list --brief = %d %q", code, stdout)
	}
	if code, stdout, _ := e.run(t, Request{Action: "list", JSON: true}); code != 0 || stdout != "[]\n" {
		t.Fatalf("empty list --json = %d %q", code, stdout)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("policy dir left %v, %v", entries, err)
	}
}

func TestListNoDir(t *testing.T) {
	e := setup(t)
	in, refused, err := List(e.common, e.o)
	if in != nil || refused != nil || err != nil {
		t.Fatalf("List = %v %v %v", in, refused, err)
	}
}

func TestSetReplaces(t *testing.T) {
	e := setup(t)
	e.set(t, "night", "first")
	e.set(t, "night", "second")
	r, err := Load(e.common, "night", e.o)
	if err != nil || r.Text != "second" {
		t.Fatalf("Load = %+v, %v", r, err)
	}
	dir, _ := e.dir(t)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (no temp files left)", len(entries))
	}
}

func TestOwnerRefusals(t *testing.T) {
	markers := []struct{ name, kind string }{
		{"CREW_WORKER_ID", "a worker"},
		{"CREW_ROLE_ID", "a grid role"},
		{"CREW_OVERSEER", "the overseer"},
		{"CREW_ID", "a dispatcher"},
		{"DISPATCHER_LAUNCHER_PID", "a dispatcher"},
		{"DISPATCHER_HANDOFF_DIR", "a dispatcher"},
	}
	for _, m := range markers {
		for _, action := range []string{"set", "clear"} {
			t.Run(m.name+" "+action, func(t *testing.T) {
				e := setup(t)
				e.vars[m.name] = "x"
				code, _, stderr := e.run(t, Request{Action: action, Name: "night", Text: "go"})
				want := "crew: policy: " + m.name + " is set, so this is " + m.kind + " session, and only the repo owner writes a standing policy; unset it if your own shell exported it\n"
				if code != 1 || stderr != want {
					t.Fatalf("= %d %q, want %q", code, stderr, want)
				}
				assertNothingWritten(t, e)
			})
		}
	}

	t.Run("no tty", func(t *testing.T) {
		e := setup(t)
		e.o.IsTTY = func() bool { return false }
		code, _, stderr := e.run(t, Request{Action: "set", Name: "night", Text: "go"})
		want := "crew: policy: stdin is not a terminal: writing a standing policy needs your own terminal (not an agent's shell tool or Claude Code's ! prefix)\n"
		if code != 1 || stderr != want {
			t.Fatalf("= %d %q", code, stderr)
		}
		assertNothingWritten(t, e)
	})

	stamped := []struct{ name, row, desc string }{
		{"lead pane", "100\x1ffeat/x\x1flead\x1fc1\x1f@1\x1f%1\n", "pane %1 is stamped as the lead of feat/x"},
		{"role pane", "100\x1ffeat/x\x1freviewer\x1fc1\x1f@1\x1f%1\n", "pane %1 is stamped as role:feat/x:reviewer"},
		{"overseer pane", "100\x1f\x1foverseer\x1f\x1f@1\x1f%1\n", "pane %1 is stamped as the overseer's"},
	}
	for _, s := range stamped {
		t.Run(s.name, func(t *testing.T) {
			e := setup(t)
			e.o.Sender.Tmux = &tmuxtest.Fake{Out: map[string]string{"list-panes": s.row}}
			code, _, stderr := e.run(t, Request{Action: "set", Name: "night", Text: "go"})
			want := "crew: policy: this is a crew pane (" + s.desc + "), and only the repo owner writes a standing policy, from their own terminal\n"
			if code != 1 || stderr != want {
				t.Fatalf("= %d %q", code, stderr)
			}
			assertNothingWritten(t, e)
		})
	}

	t.Run("unstamped pane allowed", func(t *testing.T) {
		e := setup(t)
		e.o.Sender.Tmux = &tmuxtest.Fake{Out: map[string]string{"list-panes": "100\x1f\x1f\x1f\x1f@1\x1f%1\n"}}
		e.set(t, "night", "go")
	})
}

func assertNothingWritten(t *testing.T, e *env) {
	t.Helper()
	if _, err := os.Lstat(e.vars["XDG_STATE_HOME"]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state dir exists after a refusal: %v", err)
	}
}

func TestInvalidInput(t *testing.T) {
	nameReason := func(n string) string {
		return "policy name " + strconv.Quote(n) + " must match ^[a-z0-9][a-z0-9-]{0,31}$"
	}
	tests := []struct {
		name, policy, text, reason string
	}{
		{"uppercase name", "Night", "go", nameReason("Night")},
		{"leading dash", "-night", "go", nameReason("-night")},
		{"path name", "../x", "go", nameReason("../x")},
		{"long name", strings.Repeat("a", 33), "go", nameReason(strings.Repeat("a", 33))},
		{"empty name", "", "go", nameReason("")},
		{"empty text", "night", "", "the text is empty"},
		{"newline", "night", "a\nb", "the text has a control character (a newline counts); it must be one plain line"},
		{"control", "night", "a\x1bb", "the text has a control character (a newline counts); it must be one plain line"},
		{"hidden", "night", "a\u200bb", "the text has a hidden code point"},
		{"bad utf8", "night", "a\xffb", "the text is not valid UTF-8"},
		{"2001 runes", "night", strings.Repeat("é", 2001), "the text is 2001 characters; the limit is 2000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			code, _, stderr := e.run(t, Request{Action: "set", Name: tc.policy, Text: tc.text})
			want := "crew: policy: cannot set standing policy " + oneLine(tc.policy) + " — " + tc.reason + "\n"
			if code != 1 || stderr != want {
				t.Fatalf("= %d %q, want %q", code, stderr, want)
			}
			assertNothingWritten(t, e)
		})
	}
	if r := ValidText(strings.Repeat("é", 2000)); r != "" {
		t.Errorf("2000 runes refused: %s", r)
	}
	if r := ValidName("a" + strings.Repeat("-", 31)); r != "" {
		t.Errorf("32-char name refused: %s", r)
	}
}

func TestReadRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, e *env, rec map[string]any)
		reason string // substring of the refusal
	}{
		{"file symlink", func(t *testing.T, e *env, rec map[string]any) {
			target := filepath.Join(t.TempDir(), "x.json")
			if err := os.WriteFile(target, mustJSON(t, rec), 0o600); err != nil {
				t.Fatal(err)
			}
			dir, _ := e.dir(t)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "night.json")); err != nil {
				t.Fatal(err)
			}
		}, "is a symlink"},
		{"dir symlink", func(t *testing.T, e *env, rec map[string]any) {
			target := t.TempDir()
			if err := os.WriteFile(filepath.Join(target, "night.json"), mustJSON(t, rec), 0o600); err != nil {
				t.Fatal(err)
			}
			dir, _ := e.dir(t)
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
		}, "the policy dir"},
		{"mode 0644", func(t *testing.T, e *env, rec map[string]any) {
			if err := os.Chmod(e.writeRaw(t, "night", mustJSON(t, rec)), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "has mode 0644; it must have no group or other permission bits"},
		{"not regular", func(t *testing.T, e *env, _ map[string]any) {
			dir, _ := e.dir(t)
			if err := os.MkdirAll(filepath.Join(dir, "night.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "is not a regular file"},
		{"uid field not owner", func(t *testing.T, e *env, rec map[string]any) {
			rec["uid"] = os.Getuid() + 1
			e.writeRaw(t, "night", mustJSON(t, rec))
		}, "the record's uid " + strconv.Itoa(os.Getuid()+1) + " is not the file owner"},
		{"owner not this uid", func(t *testing.T, e *env, rec map[string]any) {
			e.writeRaw(t, "night", mustJSON(t, rec))
			e.o.UID = func() int { return os.Getuid() + 1 }
		}, "is owned by uid " + strconv.Itoa(os.Getuid()) + ", not this uid " + strconv.Itoa(os.Getuid()+1)},
		{"malformed", func(t *testing.T, e *env, _ map[string]any) {
			e.writeRaw(t, "night", []byte("{not json"))
		}, "is not a policy record"},
		{"unknown field", func(t *testing.T, e *env, rec map[string]any) {
			rec["extra"] = "x"
			e.writeRaw(t, "night", mustJSON(t, rec))
		}, "unknown field"},
		{"missing field", func(t *testing.T, e *env, rec map[string]any) {
			delete(rec, "by")
			e.writeRaw(t, "night", mustJSON(t, rec))
		}, "the record has no by"},
		{"name mismatch", func(t *testing.T, e *env, rec map[string]any) {
			rec["name"] = "day"
			e.writeRaw(t, "night", mustJSON(t, rec))
		}, `the record's name "day" is not its file name "night"`},
		{"repo mismatch", func(t *testing.T, e *env, rec map[string]any) {
			rec["repo"] = "/elsewhere/.git"
			e.writeRaw(t, "night", mustJSON(t, rec))
		}, `the record's repo "/elsewhere/.git" is not this repo`},
		{"bad text", func(t *testing.T, e *env, rec map[string]any) {
			rec["text"] = "a\nb"
			e.writeRaw(t, "night", mustJSON(t, rec))
		}, "the text has a control character"},
		{"bad at", func(t *testing.T, e *env, rec map[string]any) {
			rec["at"] = "yesterday"
			e.writeRaw(t, "night", mustJSON(t, rec))
		}, `the record's at "yesterday" is not an RFC 3339 time`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			tc.mutate(t, e, e.record(t, "night"))
			_, err := Load(e.common, "night", e.o)
			if err == nil || errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("Load err = %v, want a refusal containing %q", err, tc.reason)
			}
			code, _, stderr := e.run(t, Request{Action: "show", Name: "night"})
			prefix := "crew: policy: standing policy night is not in force — "
			if code != 1 || !strings.HasPrefix(stderr, prefix) || !strings.Contains(stderr, tc.reason) {
				t.Fatalf("show = %d %q", code, stderr)
			}
		})
	}
}

func TestLoadMissing(t *testing.T) {
	e := setup(t)
	if _, err := Load(e.common, "night", e.o); !errors.Is(err, ErrMissing) {
		t.Fatalf("no dir: err = %v, want ErrMissing", err)
	}
	e.set(t, "day", "go")
	if _, err := Load(e.common, "night", e.o); !errors.Is(err, ErrMissing) {
		t.Fatalf("no file: err = %v, want ErrMissing", err)
	}
}

func TestListRefusedOnStderr(t *testing.T) {
	e := setup(t)
	e.set(t, "day", "go")
	rec := e.record(t, "night")
	rec["name"] = "other"
	e.writeRaw(t, "night", mustJSON(t, rec))

	code, stdout, stderr := e.run(t, Request{Action: "list", Brief: true})
	if code != 0 || stdout != "'day' set by owner@h1 from /dev/pts/9 at "+atStr+": \"go\"\n" {
		t.Fatalf("list --brief = %d %q", code, stdout)
	}
	want := "crew: policy: standing policy night is not in force — the record's name \"other\" is not its file name \"night\"\n"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if _, stdout, _ := e.run(t, Request{Action: "list"}); strings.Contains(stdout, "night") || !strings.Contains(stdout, "policy day") {
		t.Fatalf("list = %q", stdout)
	}
}

func TestOutputEscapesFields(t *testing.T) {
	e := setup(t)
	e.o.User = func() (string, error) { return "own\ner", nil }
	e.o.Hostname = func() (string, error) { return "h\u200b1", nil }
	e.set(t, "night", "go")
	_, stdout, _ := e.run(t, Request{Action: "list", Brief: true})
	if strings.Count(stdout, "\n") != 1 || strings.Contains(stdout, "\u200b") || !strings.Contains(stdout, "own?er@") {
		t.Fatalf("brief = %q", stdout)
	}
}

func TestSymlinkedRepoSameRecord(t *testing.T) {
	e := setup(t)
	e.set(t, "night", "go")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(e.common), alias); err != nil {
		t.Fatal(err)
	}
	viaAlias := filepath.Join(alias, ".git")
	d1, r1 := e.dir(t)
	d2, r2, err := Dir(viaAlias, e.o.Getenv)
	if err != nil || d1 != d2 || r1 != r2 {
		t.Fatalf("Dir via alias = %q %q %v, want %q %q", d2, r2, err, d1, r1)
	}
	if r, err := Load(viaAlias, "night", e.o); err != nil || r.Text != "go" {
		t.Fatalf("Load via alias = %+v, %v", r, err)
	}
}

func TestDirOutsideCommon(t *testing.T) {
	e := setup(t)
	dir, real := e.dir(t)
	if strings.HasPrefix(dir, real+string(filepath.Separator)) || strings.HasPrefix(dir, filepath.Dir(real)+string(filepath.Separator)) {
		t.Fatalf("Dir %q is under the repo %q", dir, real)
	}
	if !strings.HasPrefix(dir, filepath.Join(e.vars["XDG_STATE_HOME"], "crew", "policy")+string(filepath.Separator)) {
		t.Fatalf("Dir %q is not under XDG_STATE_HOME", dir)
	}
}

func TestDirHomeFallback(t *testing.T) {
	e := setup(t)
	delete(e.vars, "XDG_STATE_HOME")
	dir, _ := e.dir(t)
	if !strings.HasPrefix(dir, filepath.Join(e.vars["HOME"], ".local/state/crew/policy")+string(filepath.Separator)) {
		t.Fatalf("Dir = %q, want under HOME/.local/state", dir)
	}
	delete(e.vars, "HOME")
	if _, _, err := Dir(e.common, e.o.Getenv); err == nil {
		t.Fatal("Dir with neither XDG_STATE_HOME nor HOME: want an error")
	}
}

func TestWriteRefusesSymlinkedDir(t *testing.T) {
	e := setup(t)
	dir, _ := e.dir(t)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := e.run(t, Request{Action: "set", Name: "night", Text: "go"})
	if code != 1 || !strings.Contains(stderr, "is a symlink") {
		t.Fatalf("set = %d %q", code, stderr)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
}

func TestWriteReportsNotInForce(t *testing.T) {
	e := setup(t)
	rec := Record{Name: "night", Text: "go", Repo: "/elsewhere", By: "owner", Host: "h1", UID: os.Getuid(), TTY: "tty", At: atStr}
	err := Write(e.common, rec, e.o)
	if err == nil || !strings.Contains(err.Error(), "but it is not in force") || !strings.Contains(err.Error(), "is not this repo") {
		t.Fatalf("Write = %v", err)
	}
}

func TestListUnreadableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any dir")
	}
	e := setup(t)
	e.set(t, "night", "go")
	dir, _ := e.dir(t)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, _, err := List(e.common, e.o); err == nil {
		t.Fatal("List of an unreadable dir returned no error")
	}
	if code, stdout, stderr := e.run(t, Request{Action: "list"}); code != 1 || stdout != "" || !strings.Contains(stderr, "crew: policy: ") {
		t.Fatalf("list = %d %q %q", code, stdout, stderr)
	}
}

func TestListClearRefusedDir(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		make         func(t *testing.T, dir string)
	}{
		{"symlink", "is a symlink", func(t *testing.T, dir string) {
			if err := os.Symlink(t.TempDir(), dir); err != nil {
				t.Fatal(err)
			}
		}},
		{"regular file", "is not a directory", func(t *testing.T, dir string) {
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := setup(t)
			dir, _ := e.dir(t)
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				t.Fatal(err)
			}
			tt.make(t, dir)
			code, stdout, stderr := e.run(t, Request{Action: "list"})
			if code != 1 || stdout != "" || !strings.Contains(stderr, tt.reason) {
				t.Errorf("list = %d %q %q", code, stdout, stderr)
			}
			code, stdout, stderr = e.run(t, Request{Action: "clear", Name: "night"})
			if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot clear standing policy night — ") || !strings.Contains(stderr, tt.reason) {
				t.Errorf("clear = %d %q %q", code, stdout, stderr)
			}
		})
	}
}

func TestClearRefusedFromStampedPane(t *testing.T) {
	e := setup(t)
	e.set(t, "night", "go")
	e.o.Sender.Tmux = &tmuxtest.Fake{Out: map[string]string{"list-panes": "100\x1ffeat/x\x1flead\x1fc1\x1f@1\x1f%1\n"}}
	code, _, stderr := e.run(t, Request{Action: "clear", Name: "night"})
	want := "crew: policy: this is a crew pane (pane %1 is stamped as the lead of feat/x), and only the repo owner writes a standing policy, from their own terminal\n"
	if code != 1 || stderr != want {
		t.Fatalf("= %d %q", code, stderr)
	}
	if _, err := Load(e.common, "night", e.o); err != nil {
		t.Fatalf("the refused clear removed the policy: %v", err)
	}
}

func TestShowInvalidName(t *testing.T) {
	e := setup(t)
	code, stdout, stderr := e.run(t, Request{Action: "show", Name: "../x"})
	want := "crew: policy: standing policy ../x is not in force — policy name \"../x\" must match ^[a-z0-9][a-z0-9-]{0,31}$\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("= %d %q %q", code, stdout, stderr)
	}
}
