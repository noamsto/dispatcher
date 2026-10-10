package bus

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func compact(t *testing.T, v jsonv.Value) string {
	t.Helper()
	var b bytes.Buffer
	if err := jsonv.Encode(&b, v, jsonv.Options{}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestReadEventsAbsent(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing file":     filepath.Join(dir, "events.jsonl"),
		"missing parent":   filepath.Join(dir, "nope", "events.jsonl"),
		"directory":        dir,
		"dangling symlink": link,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			evs, err := ReadEvents(path)
			if !errors.Is(err, ErrNoLog) {
				t.Fatalf("err = %v, want ErrNoLog", err)
			}
			if evs != nil {
				t.Fatalf("events = %v, want nil", evs)
			}
		})
	}
}

func TestReadEventsFollowsSymlinkToFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.jsonl")
	writeFile(t, real, `{"kind":"msg"}`)
	link := filepath.Join(dir, "events.jsonl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadEvents(link)
	if err != nil || len(evs) != 1 || evs[0].Kind != KindMsg {
		t.Fatalf("evs=%v err=%v", evs, err)
	}
}

func TestReadEventsOpenError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, `{"kind":"msg"}`)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	_, err := ReadEvents(path)
	var oe *OpenError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v, want *OpenError", err)
	}
	if oe.Path != path || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("OpenError = %+v", oe)
	}
	if errors.Is(err, ErrNoLog) {
		t.Fatal("an unreadable regular file must not look absent")
	}
}

func TestReadEventsDecodeError(t *testing.T) {
	cases := map[string]string{
		"malformed line": `{"kind":"msg"}` + "\n" + `{"kind":` + "\n" + `{"kind":"msg"}` + "\n",
		"torn tail":      `{"kind":"msg"}` + "\n" + `{"kind":"status","bo`,
		"garbage":        "not json\n",
		"trailing comma": `{"kind":"msg",}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			writeFile(t, path, content)
			evs, err := ReadEvents(path)
			var de *DecodeError
			if !errors.As(err, &de) {
				t.Fatalf("err = %v, want *DecodeError", err)
			}
			var se *jsonv.SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("DecodeError does not wrap *jsonv.SyntaxError: %v", err)
			}
			if de.Path != path {
				t.Fatalf("Path = %q", de.Path)
			}
			if evs != nil {
				t.Fatalf("events = %v, want nil on error", evs)
			}
		})
	}
}

func TestReadEventsEmpty(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "whitespace only": " \n\t\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			writeFile(t, path, content)
			evs, err := ReadEvents(path)
			if err != nil || len(evs) != 0 {
				t.Fatalf("evs=%v err=%v", evs, err)
			}
		})
	}
}

func TestReadEventsFields(t *testing.T) {
	content := `{"ts":1700000000123,"crew_id":"c1","kind":"status","from":"w1","to":"lead","branch":"feat/x","session":"s1","body":{"state":"working","detail":"hi"},"extra":[1,{"k":null}]}` + "\n" +
		`{"crew_id":"c2","kind":"zzz-unknown"}` + "\n" +
		`{"ts":"str","crew_id":5,"kind":7,"from":null,"to":false,"branch":["a"],"session":{},"body":3}` + "\n"
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, content)
	evs, err := ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("len = %d", len(evs))
	}

	e := evs[0]
	if e.CrewID != "c1" || e.Kind != KindStatus || e.From != "w1" || e.To != "lead" || e.Branch != "feat/x" || e.Session != "s1" {
		t.Fatalf("typed fields: %+v", e)
	}
	if got := compact(t, e.TS); got != "1700000000123" {
		t.Fatalf("TS = %s", got)
	}
	if got := compact(t, e.Body); got != `{"state":"working","detail":"hi"}` {
		t.Fatalf("Body = %s", got)
	}

	if evs[1].Kind != Kind("zzz-unknown") || evs[1].CrewID != "c2" {
		t.Fatalf("unrecognised kind not kept: %+v", evs[1])
	}
	if !evs[1].TS.IsNull() || !evs[1].Body.IsNull() {
		t.Fatalf("absent ts/body must be null: %+v", evs[1])
	}

	odd := evs[2]
	if odd.CrewID != "" || odd.Kind != "" || odd.From != "" || odd.To != "" || odd.Branch != "" || odd.Session != "" {
		t.Fatalf("non-string fields must read as empty: %+v", odd)
	}
	if compact(t, odd.TS) != `"str"` || compact(t, odd.Body) != "3" {
		t.Fatalf("TS/Body keep their raw value: %+v", odd)
	}
}

func TestReadEventsRawRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, `{ "ts" : 1700000000123 , "kind":"status",  "zz" : [ true , null, {"b":1,"a":2} ] , "s":"éé" }`)
	evs, err := ReadEvents(path)
	if err != nil || len(evs) != 1 {
		t.Fatalf("evs=%v err=%v", evs, err)
	}
	want := `{"ts":1700000000123,"kind":"status","zz":[true,null,{"b":1,"a":2}],"s":"éé"}`
	if got := compact(t, evs[0].Raw); got != want {
		t.Fatalf("Raw = %s\nwant  %s", got, want)
	}
}

func TestReadEventsOrderAcrossCrews(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path,
		`{"crew_id":"c1","kind":"dispatch","branch":"a"}`+"\n"+
			`{"crew_id":"c2","kind":"dispatch","branch":"b"}`+"\n"+
			`{"crew_id":"c1","kind":"status","branch":"a"}`+"\n"+
			`{"crew_id":"c2","kind":"reap","branch":"b"}`+"\n")
	evs, err := ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		got = append(got, e.CrewID+":"+string(e.Kind))
	}
	want := []string{"c1:dispatch", "c2:dispatch", "c1:status", "c2:reap"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestReadEventsStreamShapes(t *testing.T) {
	cases := []struct {
		name, content string
		kinds         []Kind
	}{
		{"concatenated values", `{"kind":"msg"}{"kind":"nudge"}`, []Kind{KindMsg, KindNudge}},
		{"value spans lines", "{\n\"kind\":\n\"claim\"\n}\n{\"kind\":\"release\"}", []Kind{KindClaim, KindRelease}},
		{"no trailing newline", `{"kind":"reclaim"}`, []Kind{KindReclaim}},
		{"blank lines", "\n\n{\"kind\":\"resume\"}\n\n", []Kind{KindResume}},
		{"bom", "\xef\xbb\xbf{\"kind\":\"nudge_wait\"}", []Kind{KindNudgeWait}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			writeFile(t, path, c.content)
			evs, err := ReadEvents(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) != len(c.kinds) {
				t.Fatalf("len = %d, want %d", len(evs), len(c.kinds))
			}
			for i, k := range c.kinds {
				if evs[i].Kind != k {
					t.Fatalf("evs[%d].Kind = %q, want %q", i, evs[i].Kind, k)
				}
			}
		})
	}
}

func TestReadEventsNonObjectValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeFile(t, path, "5\nnull\n\"s\"\n[1]\ntrue\n"+`{"kind":"msg"}`)
	evs, err := ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	wantRaw := []string{"5", "null", `"s"`, "[1]", "true", `{"kind":"msg"}`}
	if len(evs) != len(wantRaw) {
		t.Fatalf("len = %d", len(evs))
	}
	for i, w := range wantRaw {
		if got := compact(t, evs[i].Raw); got != w {
			t.Fatalf("evs[%d].Raw = %s, want %s", i, got, w)
		}
		if i < 5 && (evs[i].Kind != "" || evs[i].CrewID != "" || !evs[i].TS.IsNull() || !evs[i].Body.IsNull()) {
			t.Fatalf("evs[%d] typed fields must stay empty: %+v", i, evs[i])
		}
	}
}

func TestStateTerminal(t *testing.T) {
	cases := []struct {
		s    State
		want bool
	}{
		{StateWorking, false},
		{StateBlocked, false},
		{StatePROpen, false},
		{StateDone, true},
		{StateFailed, true},
		{StateExited, true},
		{State(""), false},
		{State("DONE"), false},
		{State("whatever"), false},
	}
	for _, c := range cases {
		if got := c.s.Terminal(); got != c.want {
			t.Errorf("State(%q).Terminal() = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestConstValues(t *testing.T) {
	for k, want := range map[Kind]string{
		KindStatus: "status", KindMsg: "msg", KindDispatch: "dispatch", KindResume: "resume",
		KindNudge: "nudge", KindNudgeWait: "nudge_wait", KindReap: "reap", KindRelease: "release",
		KindClaim: "claim", KindReclaim: "reclaim",
	} {
		if string(k) != want {
			t.Errorf("Kind %q != %q", k, want)
		}
	}
	for s, want := range map[State]string{
		StateWorking: "working", StateBlocked: "blocked", StatePROpen: "pr_open",
		StateDone: "done", StateFailed: "failed", StateExited: "exited",
	} {
		if string(s) != want {
			t.Errorf("State %q != %q", s, want)
		}
	}
}

func TestPaths(t *testing.T) {
	p := newPaths("/r/.git")
	if p.Common != "/r/.git" || p.Dir != "/r/.git/crew" || p.Log != "/r/.git/crew/events.jsonl" {
		t.Fatalf("%+v", p)
	}
	if got := p.CrewDir("c1"); got != "/r/.git/crew/crews/c1" {
		t.Fatalf("CrewDir = %q", got)
	}
}

func TestValidCrewID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"abc", true},
		{"A-b_c.d9", true},
		{"a.b", true},
		{"..a", true},
		{"a-", true},
		{"", false},
		{".", false},
		{"..", false},
		{"-x", false},
		{"-", false},
		{"a/b", false},
		{"../x", false},
		{"a b", false},
		{"a\nb", false},
		{"é", false},
		{"a*", false},
		{"a:b", false},
		{"a\x00b", false},
	}
	for _, c := range cases {
		if got := ValidCrewID(c.id); got != c.want {
			t.Errorf("ValidCrewID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}

// gitEnv isolates git from the developer's config and from any repo the test
// run itself sits inside.
func gitEnv(t *testing.T, ceiling string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_PREFIX", "GIT_OBJECT_DIRECTORY"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", ceiling)
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepo returns a symlink-resolved main checkout (so paths compare equal to
// git's) with one commit, and a linked worktree of it.
func newRepo(t *testing.T) (main, linked string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitEnv(t, root)
	main = filepath.Join(root, "main")
	if err := os.Mkdir(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, main, "init", "-q")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	linked = filepath.Join(root, "linked")
	git(t, main, "worktree", "add", "-q", "-b", "wt", linked)
	return main, linked
}

func TestLocate(t *testing.T) {
	main, linked := newRepo(t)
	sub := filepath.Join(main, "sub", "deeper")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wantCommon := filepath.Join(main, ".git")

	for name, cwd := range map[string]string{"main root": main, "main subdir": sub, "linked worktree": linked} {
		t.Run(name, func(t *testing.T) {
			p, err := Locate(context.Background(), cwd)
			if err != nil {
				t.Fatal(err)
			}
			if p.Common != wantCommon {
				t.Fatalf("Common = %q, want %q", p.Common, wantCommon)
			}
			if p.Dir != wantCommon+"/crew" || p.Log != wantCommon+"/crew/events.jsonl" {
				t.Fatalf("%+v", p)
			}
		})
	}
}

func TestLocateNotRepo(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitEnv(t, root)
	dir := filepath.Join(root, "plain")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Locate(context.Background(), dir)
	if !errors.Is(err, ErrNotRepo) {
		t.Fatalf("err = %v, want ErrNotRepo", err)
	}
	if p != (Paths{}) {
		t.Fatalf("Paths = %+v, want zero", p)
	}
	if _, err := Locate(context.Background(), filepath.Join(dir, "missing")); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("missing cwd: err = %v, want ErrNotRepo", err)
	}
}

func TestCrewID(t *testing.T) {
	cases := []struct {
		name string
		task *string // nil: no WORKER_TASK.md
		env  string
		want string
	}{
		{"plain", ptr("crew_id: x\n"), "", "x"},
		{"wins over env", ptr("crew_id: x\n"), "envid", "x"},
		{"no space is the whole line", ptr("crew_id:x\n"), "", "crew_id:x"},
		{"no space wins over env", ptr("crew_id:x\n"), "envid", "crew_id:x"},
		{"bare key is the whole line", ptr("crew_id:\n"), "", "crew_id:"},
		{"tab is not the delimiter", ptr("crew_id:\tx\n"), "", "crew_id:\tx"},
		{"two spaces: empty field falls back to env", ptr("crew_id:  x\n"), "envid", "envid"},
		{"two spaces, no env", ptr("crew_id:  x\n"), "", ""},
		{"trailing space only", ptr("crew_id: \n"), "envid", "envid"},
		{"extra words dropped", ptr("crew_id: x y z\n"), "", "x"},
		{"no trailing newline", ptr("crew_id: x"), "", "x"},
		{"crlf keeps the cr", ptr("crew_id: x\r\n"), "", "x\r"},
		{"crlf no space keeps the cr", ptr("crew_id:x\r\n"), "", "crew_id:x\r"},
		{"indented does not match", ptr(" crew_id: x\n"), "envid", "envid"},
		{"mid-line does not match", ptr("# crew_id: x\n"), "envid", "envid"},
		{"later line matches", ptr("# title\n\nbranch: b\ncrew_id: y\n"), "", "y"},
		{"first match wins", ptr("crew_id: a\ncrew_id: b\n"), "", "a"},
		{"first match wins even when empty", ptr("crew_id:  \ncrew_id: b\n"), "envid", "envid"},
		{"longer key with the prefix does not match", ptr("crew_idx: z\n"), "envid", "envid"},
		{"colon in the value", ptr("crew_id: a:b\n"), "", "a:b"},
		{"empty file", ptr(""), "envid", "envid"},
		{"no file, env", nil, "envid", "envid"},
		{"no file, no env", nil, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			main, _ := newRepo(t)
			if c.task != nil {
				writeFile(t, filepath.Join(main, "WORKER_TASK.md"), *c.task)
			}
			t.Setenv("CREW_ID", c.env)
			if got := CrewID(context.Background(), main); got != c.want {
				t.Fatalf("CrewID = %q, want %q", got, c.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestCrewIDFromSubdirAndWorktree(t *testing.T) {
	main, linked := newRepo(t)
	writeFile(t, filepath.Join(main, "WORKER_TASK.md"), "crew_id: main-crew\n")
	writeFile(t, filepath.Join(linked, "WORKER_TASK.md"), "crew_id: linked-crew\n")
	sub := filepath.Join(linked, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREW_ID", "envid")
	if got := CrewID(context.Background(), sub); got != "linked-crew" {
		t.Fatalf("from linked subdir = %q", got)
	}
	if got := CrewID(context.Background(), main); got != "main-crew" {
		t.Fatalf("from main = %q", got)
	}
}

func TestCrewIDTaskNotARegularFile(t *testing.T) {
	main, _ := newRepo(t)
	if err := os.Mkdir(filepath.Join(main, "WORKER_TASK.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREW_ID", "envid")
	if got := CrewID(context.Background(), main); got != "envid" {
		t.Fatalf("directory WORKER_TASK.md: got %q, want env fallback", got)
	}
}

func TestCrewIDTaskSymlink(t *testing.T) {
	main, _ := newRepo(t)
	target := filepath.Join(main, "elsewhere.md")
	writeFile(t, target, "crew_id: via-link\n")
	if err := os.Symlink(target, filepath.Join(main, "WORKER_TASK.md")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREW_ID", "")
	if got := CrewID(context.Background(), main); got != "via-link" {
		t.Fatalf("got %q", got)
	}
}

func TestCrewIDUnreadableTaskFallsBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	main, _ := newRepo(t)
	p := filepath.Join(main, "WORKER_TASK.md")
	writeFile(t, p, "crew_id: x\n")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREW_ID", "envid")
	if got := CrewID(context.Background(), main); got != "envid" {
		t.Fatalf("got %q", got)
	}
}

func TestCrewIDNotRepo(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitEnv(t, root)
	dir := filepath.Join(root, "plain")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "WORKER_TASK.md"), "crew_id: ignored\n")
	t.Setenv("CREW_ID", "envid")
	if got := CrewID(context.Background(), dir); got != "envid" {
		t.Fatalf("got %q", got)
	}
}

func TestIsTerminalFalseForNonTTY(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(f.Fd()) {
		t.Fatal("/dev/null is a char device but not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	if IsTerminal(w.Fd()) || IsTerminal(r.Fd()) {
		t.Fatal("a pipe is not a terminal")
	}
	reg, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	if IsTerminal(reg.Fd()) {
		t.Fatal("a regular file is not a terminal")
	}
	if IsTerminal(^uintptr(0)) {
		t.Fatal("an invalid fd is not a terminal")
	}
}

func TestReadEventsTolerant(t *testing.T) {
	t.Run("torn tail keeps the prefix with the error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		writeFile(t, path, `{"kind":"msg","crew_id":"a"}`+"\n"+`{"kind":"status","bo`)
		evs, err := ReadEventsTolerant(path)
		var de *DecodeError
		if !errors.As(err, &de) {
			t.Fatalf("err = %v, want *DecodeError", err)
		}
		if len(evs) != 1 || evs[0].CrewID != "a" {
			t.Fatalf("prefix lost: %v", evs)
		}
		if strict, err2 := ReadEvents(path); strict != nil || err2 == nil {
			t.Fatalf("ReadEvents must keep returning no events on a decode error: %v %v", strict, err2)
		}
	})
	t.Run("clean read agrees with ReadEvents", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		writeFile(t, path, `{"kind":"msg"}`+"\n")
		evs, err := ReadEventsTolerant(path)
		if err != nil || len(evs) != 1 || evs[0].Kind != KindMsg {
			t.Fatalf("evs=%v err=%v", evs, err)
		}
	})
	t.Run("missing log is ErrNoLog with no events", func(t *testing.T) {
		evs, err := ReadEventsTolerant(filepath.Join(t.TempDir(), "events.jsonl"))
		if !errors.Is(err, ErrNoLog) || evs != nil {
			t.Fatalf("evs=%v err=%v", evs, err)
		}
	})
}

// A watchdog is launched by dispatch with its crew in the environment, and its
// cwd is a worktree whose WORKER_TASK.md the watched worker can rewrite. For
// that caller the env is the anchor and the task doc is an argument; CrewID's
// own order stands for every other subcommand.
func TestCrewIDEnvFirst(t *testing.T) {
	task := "tier: standard\ncrew_id: taskdoc\nengine: pi\n"
	cases := []struct {
		name string
		task *string
		env  string
		want string
	}{
		{"a worker-edited task doc loses to the env", ptr(task), "1791464376-1014098", "1791464376-1014098"},
		{"empty env falls back to the task doc", ptr(task), "", "taskdoc"},
		{"no task doc, env only", nil, "envid", "envid"},
		{"neither", nil, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			main, _ := newRepo(t)
			if c.task != nil {
				writeFile(t, filepath.Join(main, "WORKER_TASK.md"), *c.task)
			}
			t.Setenv("CREW_ID", c.env)
			if got := CrewIDEnvFirst(context.Background(), main); got != c.want {
				t.Fatalf("CrewIDEnvFirst = %q, want %q", got, c.want)
			}
			if c.env != "" && c.task != nil {
				if got := CrewID(context.Background(), main); got != "taskdoc" {
					t.Fatalf("CrewID = %q, want the task doc to keep winning there", got)
				}
			}
		})
	}
}
