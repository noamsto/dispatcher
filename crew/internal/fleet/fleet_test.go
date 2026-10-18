package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

var fixedNow = time.Unix(1700000100, 0)

func worker(ts int64, crew, branch, state string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"status","from":"worker:%s#s1-1","to":"dispatcher:%s","body":{"state":%q}}`, ts*1000, crew, branch, crew, state)
}

func dispatch(ts int64, crew, branch, name string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q,"name":%q,"engine":"claude","model":"sonnet","tier":"standard","title":"Title of %s"}`, ts*1000, crew, branch, name, branch)
}

// mkRepo makes a git repo whose bus holds lines and returns its common dir.
func mkRepo(t *testing.T, root, name, origin string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Skipf("git init: %v\n%s", err, out)
	}
	common := filepath.Join(dir, ".git")
	if err := os.MkdirAll(filepath.Join(common, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(common, "crew", "events.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return common
}

type fixture struct {
	opts  Options
	alpha string
	beta  string
}

func setup(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	alpha := mkRepo(t, root, "alpha", "",
		dispatch(1700000000, "c1", "feat/a", "sage"),
		worker(1700000010, "c1", "feat/a", "working"),
		dispatch(1700000001, "c2", "feat/b", "feat/b"),
		worker(1700000020, "c2", "feat/b", "done"))
	beta := mkRepo(t, root, "beta",
		"",
		dispatch(1700000002, "c1", "feat/c", "feat/c"),
		worker(1700000030, "c1", "feat/c", "working"))
	nobus := filepath.Join(root, "nobus", ".git")
	if err := os.MkdirAll(nobus, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(root, "registry")
	reg := strings.Join([]string{alpha, beta, filepath.Join(root, "vanished", ".git"), nobus}, "\n") + "\n"
	if err := os.WriteFile(registry, []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	env := map[string]string{"HOME": empty, "CREW_SWEEP_ROOTS": empty}
	return fixture{
		alpha: alpha,
		beta:  beta,
		opts: Options{
			Env:       func(k string) string { return env[k] },
			Registry:  registry,
			Tmux:      &tmuxtest.Fake{Out: map[string]string{"list-panes": "", "list-windows": "feat/a\nfeat/c\n"}},
			Worktrees: func(string) (string, error) { return "", nil },
			Origin: func(common string) string {
				if common == alpha {
					return "https://github.com/acme/alpha.git"
				}
				return ""
			},
			Now: func() time.Time { return fixedNow },
		},
	}
}

func run(t *testing.T, f fixture, req Request) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(t.Context(), req, &out, &errb, f.opts)
	return out.String(), errb.String(), code
}

func dataLines(out string) []string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[1:]
}

func jsonRows(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, out)
	}
	return rows
}

func TestDefaultTableListsLiveRowsAcrossRepos(t *testing.T) {
	f := setup(t)
	out, errs, code := run(t, f, Request{})
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if !strings.HasPrefix(out, "REPO ") || !strings.Contains(strings.SplitN(out, "\n", 2)[0], "CODENAME") {
		t.Errorf("header missing:\n%s", out)
	}
	rows := dataLines(out)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got:\n%s", out)
	}
	if !strings.Contains(rows[0], "acme/alpha") || !strings.Contains(rows[0], "feat/a") || !strings.Contains(rows[0], "claude/sonnet/standard") || !strings.Contains(rows[0], "1m") {
		t.Errorf("alpha row: %q", rows[0])
	}
	if !strings.Contains(rows[1], "beta") || strings.Contains(rows[1], "acme") || !strings.Contains(rows[1], "feat/c") {
		t.Errorf("beta row: %q", rows[1])
	}
	if strings.Contains(out, "feat/b") {
		t.Errorf("done row shown without --all:\n%s", out)
	}
	if !strings.Contains(errs, "skipped (gone)") || !strings.Contains(errs, "skipped (no bus)") {
		t.Errorf("stderr lacks skip notes: %q", errs)
	}
}

func TestJSONHasRepoAndCrew(t *testing.T) {
	f := setup(t)
	out, _, code := run(t, f, Request{JSON: true})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	rows := jsonRows(t, out)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if r["repo"] == nil || r["crew_id"] != "c1" {
			t.Errorf("row lacks repo/crew_id: %v", r)
		}
	}
	if rows[0]["repo"] != "acme/alpha" {
		t.Errorf("repo = %v", rows[0]["repo"])
	}
}

func TestCrewAndAllFilters(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want int
		has  string
	}{
		{"crew c2 with all", Request{Crew: "c2", All: true}, 1, "feat/b"},
		{"crew c2 hides done", Request{Crew: "c2"}, 0, ""},
		{"crew c1", Request{Crew: "c1"}, 2, "feat/c"},
		{"all", Request{All: true}, 3, "feat/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, code := run(t, setup(t), tt.req)
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := len(dataLines(out)); got != tt.want {
				t.Errorf("rows = %d, want %d:\n%s", got, tt.want, out)
			}
			if !strings.Contains(out, tt.has) {
				t.Errorf("output lacks %q:\n%s", tt.has, out)
			}
		})
	}
}

func TestUnreadableBusIsSkippedNotFatal(t *testing.T) {
	f := setup(t)
	garbage := mkRepo(t, t.TempDir(), "junk", "")
	if err := os.WriteFile(garbage+"/crew/events.jsonl", []byte("{{{ not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _ := os.ReadFile(f.opts.Registry)
	if err := os.WriteFile(f.opts.Registry, append(reg, []byte(garbage+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errs, code := run(t, f, Request{})
	if code != 0 || len(dataLines(out)) != 2 {
		t.Errorf("code %d, rows:\n%s", code, out)
	}
	want := "crew: fleet: " + garbage + ": " + garbage + "/crew/events.jsonl: "
	if line := noteFor(errs, garbage); !strings.HasPrefix(line, want) || !strings.HasSuffix(line, " (rows from the readable prefix)") {
		t.Errorf("decode note = %q, want prefix %q; stderr %q", line, want, errs)
	}
}

// noteFor is the stderr lines naming common, joined; a one-line note is one.
func noteFor(errs, common string) string {
	var hits []string
	for _, l := range strings.Split(strings.TrimRight(errs, "\n"), "\n") {
		if strings.Contains(l, common) {
			hits = append(hits, l)
		}
	}
	return strings.Join(hits, "\n")
}

func TestTornTailKeepsPrefix(t *testing.T) {
	f := setup(t)
	file := f.beta + "/crew/events.jsonl"
	fh, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString(`{"ts":1,"kind":`)
	_ = fh.Close()
	out, errs, code := run(t, f, Request{})
	if code != 0 || !strings.Contains(out, "feat/c") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
	line := noteFor(errs, f.beta)
	if !strings.HasPrefix(line, "crew: fleet: "+f.beta+": ") || !strings.HasSuffix(line, " (rows from the readable prefix)") || strings.Contains(line, "\n") {
		t.Errorf("decode note = %q", line)
	}
}

func TestWorktreesFailureNoteIsOneLine(t *testing.T) {
	f := setup(t)
	root := t.TempDir()
	repo := mkRepo(t, root, "gamma", "", dispatch(1700000003, "c1", "feat/g", "feat/g"), worker(1700000040, "c1", "feat/g", "exited"))
	reg, _ := os.ReadFile(f.opts.Registry)
	if err := os.WriteFile(f.opts.Registry, append(reg, []byte(repo+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	f.opts.Worktrees = func(string) (string, error) {
		return "", &roster.ExitError{Code: 128, Stderr: "fatal: dubious ownership\n"}
	}
	_, errs, code := run(t, f, Request{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := "crew: fleet: " + repo + ": skipped (exit status 128: fatal: dubious ownership)"
	if got := noteFor(errs, repo); got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

func TestWorktreesProbedOncePerRepo(t *testing.T) {
	f := setup(t)
	repo := mkRepo(t, t.TempDir(), "gamma", "",
		dispatch(1700000003, "c1", "feat/g", "feat/g"), worker(1700000040, "c1", "feat/g", "exited"),
		dispatch(1700000004, "c2", "feat/h", "feat/h"), worker(1700000041, "c2", "feat/h", "exited"))
	if err := os.WriteFile(f.opts.Registry, []byte(repo+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.opts.Worktrees = func(string) (string, error) { calls++; return "", nil }
	if _, _, code := run(t, f, Request{All: true}); code != 0 || calls != 1 {
		t.Errorf("code %d, Worktrees called %d times", code, calls)
	}
}

func TestControlRunesAreNeutralisedInTable(t *testing.T) {
	f := setup(t)
	repo := mkRepo(t, t.TempDir(), "evil", "",
		`{"ts":1700000000000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","name":"n","title":"a\tb"}`,
		`{"ts":1700000010000,"crew_id":"c1","kind":"status","from":"worker:feat/x\nFORGED\u001b[2J#s1-1","to":"dispatcher:c1","body":{"state":"working"}}`)
	if err := os.WriteFile(f.opts.Registry, []byte(repo+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, code := run(t, f, Request{All: true})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if rows := dataLines(out); len(rows) != 1 {
		t.Fatalf("want 1 row, got:\n%s", out)
	}
	if strings.ContainsAny(out, "\t\x1b") || strings.Count(out, "\n") != 2 {
		t.Errorf("control bytes in table: %q", out)
	}
}

func TestAge(t *testing.T) {
	tests := []struct {
		v    any
		want string
	}{
		{float64(5), "5s"}, {float64(125), "2m"}, {json.Number("3725"), "1h2m"},
		{float64(90000), "1d1h"}, {nil, "-"}, {"x", "-"},
	}
	for _, tt := range tests {
		if got := age(map[string]any{"age_s": tt.v}); got != tt.want {
			t.Errorf("age = %q, want %q", got, tt.want)
		}
	}
}

func TestStaleRowsHiddenByDefault(t *testing.T) {
	f := setup(t)
	old := fixedNow.Unix() - 20*86400
	if err := os.WriteFile(filepath.Join(f.alpha, "crew", "events.jsonl"), []byte(strings.Join([]string{
		dispatch(old, "c9", "feat/old", "dusk"),
		worker(old, "c9", "feat/old", "working"),
		dispatch(1700000000, "c1", "feat/a", "sage"),
		worker(1700000010, "c1", "feat/a", "working"),
	}, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, errs, _ := run(t, f, Request{})
	if strings.Contains(out, "feat/old") || !strings.Contains(out, "feat/a") {
		t.Errorf("default view:\n%s", out)
	}
	if !strings.Contains(errs, "crew fleet: 1 stale rows hidden (--all shows them)") {
		t.Errorf("stderr %q", errs)
	}

	out, errs, _ = run(t, f, Request{All: true})
	if !strings.Contains(out, "feat/old") || strings.Contains(errs, "stale") {
		t.Errorf("--all:\nout %s\nerr %q", out, errs)
	}

	out, errs, _ = run(t, f, Request{JSON: true})
	stale := map[string]any{}
	for _, r := range jsonRows(t, out) {
		stale[r["branch"].(string)] = r["stale"]
	}
	if stale["feat/old"] != true || stale["feat/a"] != false || strings.Contains(errs, "stale") {
		t.Errorf("--json stale flags %v, stderr %q", stale, errs)
	}
}

func TestWorktreeVouchesOnlyWhileRowIsFresh(t *testing.T) {
	f := setup(t)
	f.opts.Tmux = &tmuxtest.Fake{}
	f.opts.Worktrees = func(string) (string, error) {
		return "worktree /w/a\nbranch refs/heads/feat/a\n\nworktree /w/c\nbranch refs/heads/feat/c\n", nil
	}
	if out, _, _ := run(t, f, Request{}); !strings.Contains(out, "feat/a") || !strings.Contains(out, "feat/c") {
		t.Errorf("fresh rows with a worktree hidden:\n%s", out)
	}
	f.opts.Now = func() time.Time { return fixedNow.Add(72 * time.Hour) }
	if out, errs, _ := run(t, f, Request{}); strings.Contains(out, "feat/") || !strings.Contains(errs, "2 stale rows hidden") {
		t.Errorf("old rows with a worktree shown:\n%s\n%q", out, errs)
	}
}

// Every crew's fold and the liveness check share one read of each tmux list.
func TestTmuxReadOnce(t *testing.T) {
	f := setup(t)
	tx := &tmuxtest.Fake{}
	f.opts.Tmux = tx
	run(t, f, Request{All: true})
	if got := tx.Calls(); len(got) != 2 {
		t.Errorf("tmux calls = %q, want one list-panes and one list-windows", got)
	}
}

func TestWorktreeProbeFailureKeepsFreshRows(t *testing.T) {
	f := setup(t)
	f.opts.Tmux = &tmuxtest.Fake{}
	f.opts.Worktrees = func(string) (string, error) { return "", fmt.Errorf("git: boom") }
	out, errs, _ := run(t, f, Request{})
	if !strings.Contains(out, "feat/a") || strings.Contains(errs, "stale") {
		t.Errorf("out %s\nerr %q", out, errs)
	}
}

func TestNonFiniteAgeIsClampedInJSONAndDashInTable(t *testing.T) {
	root := t.TempDir()
	nan := mkRepo(t, root, "nan", "",
		`{"ts":1e400,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}`)
	registry := filepath.Join(root, "registry")
	if err := os.WriteFile(registry, []byte(nan+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	env := map[string]string{"HOME": empty, "CREW_SWEEP_ROOTS": empty}
	o := Options{
		Env:       func(k string) string { return env[k] },
		Registry:  registry,
		Tmux:      &tmuxtest.Fake{Out: map[string]string{"list-windows": "feat/x\n"}},
		Worktrees: func(string) (string, error) { return "", nil },
		Origin:    func(string) string { return "" },
		Now:       func() time.Time { return fixedNow },
	}
	f := fixture{opts: o}

	out, errs, code := run(t, f, Request{JSON: true})
	if code != 0 {
		t.Fatalf("--json exit %d, stderr %q", code, errs)
	}
	rows := jsonRows(t, out)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got:\n%s", out)
	}
	if v, ok := rows[0]["age_s"]; !ok || v != json.Number("-1.7976931348623157e+308") {
		t.Errorf("age_s = %v (present %v), want -MaxFloat64 literal", v, ok)
	}

	out, errs, code = run(t, f, Request{})
	if code != 0 {
		t.Fatalf("table exit %d, stderr %q", code, errs)
	}
	data := dataLines(out)
	if len(data) != 1 || !strings.Contains(data[0], " - ") {
		t.Errorf("want one row with age -, got:\n%s", out)
	}
	if strings.Contains(out, "9223372036854775808") {
		t.Errorf("table leaks the int64 overflow age:\n%s", out)
	}
}
