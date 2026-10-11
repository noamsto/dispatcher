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

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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
	opts   Options
	alpha  string
	beta   string
	broken string
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
			Panes:     func() string { return "" },
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

func run(t *testing.T, f fixture, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb, f.opts)
	return out.String(), errb.String(), code
}

func dataLines(out string) []string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return lines[1:]
}

func jsonRows(t *testing.T, out string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, out)
	}
	return rows
}

func TestDefaultTableListsLiveRowsAcrossRepos(t *testing.T) {
	f := setup(t)
	out, errs, code := run(t, f)
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
	out, _, code := run(t, f, "--json")
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
		args []string
		want int
		has  string
	}{
		{"crew c2 with all", []string{"--crew", "c2", "--all"}, 1, "feat/b"},
		{"crew c2 hides done", []string{"--crew", "c2"}, 0, ""},
		{"crew c1", []string{"--crew", "c1"}, 2, "feat/c"},
		{"all", []string{"--all"}, 3, "feat/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, code := run(t, setup(t), tt.args...)
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

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"--bogus"}, {"--crew"}, {"--crew", ""}, {"extra"}} {
		out, errs, code := run(t, setup(t), args...)
		if code != 1 || out != "" || strings.TrimSpace(errs) != usage {
			t.Errorf("%q: code %d, stdout %q, stderr %q", args, code, out, errs)
		}
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
	out, _, code := run(t, f)
	if code != 0 || len(dataLines(out)) != 2 {
		t.Errorf("code %d, rows:\n%s", code, out)
	}
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
	out, _, code := run(t, f)
	if code != 0 || !strings.Contains(out, "feat/c") {
		t.Errorf("code %d, output:\n%s", code, out)
	}
}

func TestPanesReadOnce(t *testing.T) {
	f := setup(t)
	calls := 0
	f.opts.Panes = func() string { calls++; return "" }
	if _, _, code := run(t, f, "--all"); code != 0 || calls > 1 {
		t.Errorf("code %d, Panes called %d times", code, calls)
	}
}

func TestAge(t *testing.T) {
	tests := []struct {
		v    jsonv.Value
		want string
	}{
		{jsonv.Num(5), "5s"}, {jsonv.Num(125), "2m"}, {jsonv.Num(3725), "1h2m"},
		{jsonv.Num(90000), "1d1h"}, {jsonv.Null(), "-"},
	}
	for _, tt := range tests {
		if got := age(jsonv.Object(jsonv.Member{Key: "age_s", Val: tt.v})); got != tt.want {
			t.Errorf("age = %q, want %q", got, tt.want)
		}
	}
}
