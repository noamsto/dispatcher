package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

type testCase struct {
	id         string
	assertions []string
	run        func(*caseTest)
}

type caseTest struct {
	*testing.T
	want map[string]bool
	seen map[string]bool
}

type commandResult struct {
	stdout string
	stderr string
	status int
}

var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
}()

// adapters/core/crew.sh names the Go binary through CREW_GO_BIN, whose fallback
// is still the @crewGoBin@ placeholder in a checkout, so a raw-source run needs
// the variable. The bench adapter exports it once for the whole run; a bare
// `go test ./...` builds it here, as tests/setup_suite.bash does for bats.
func TestMain(m *testing.M) {
	dir, err := ensureCrewGoBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	status := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(status)
}

// ensureCrewGoBin returns the directory holding the binary it built, or "" when
// CREW_GO_BIN was already set and nothing was built.
func ensureCrewGoBin() (string, error) {
	if os.Getenv("CREW_GO_BIN") != "" {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "crew-go-harness")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "crew-go")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join(repoRoot, "crew")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-buildvcs=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("harness: build crew-go: %w\n%s", err, out)
	}
	if err := os.Setenv("CREW_GO_BIN", bin); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func TestManifest(t *testing.T) {
	cases := manifestCases()
	checkRegistryParity(t, cases)
	selected := os.Getenv("GO_HARNESS_CASE")
	seenCases := make(map[string]bool, len(cases))
	for _, tc := range cases {
		if seenCases[tc.id] {
			t.Fatalf("duplicate case ID %s", tc.id)
		}
		seenCases[tc.id] = true
	}
	if selected != "" && !seenCases[selected] {
		t.Fatalf("selected case ID is absent from registry: %s", selected)
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			// boundaries.tsv records private repositories, PATH stubs, and XDG data
			// roots for every case. Child-only environments avoid shared Go state.
			t.Parallel()
			ct := &caseTest{T: t, want: make(map[string]bool), seen: make(map[string]bool)}
			for _, id := range tc.assertions {
				ct.want[id] = true
			}
			tc.run(ct)
			for _, id := range tc.assertions {
				if !ct.seen[id] {
					t.Errorf("assertion %s was not exercised", id)
				}
			}
		})
	}
}

func readTSV(t *testing.T, path string) [][]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records [][]string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		records = append(records, strings.Split(scanner.Text(), "\t"))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func checkRegistryParity(t *testing.T, cases []testCase) {
	t.Helper()
	wantCases := make(map[string]bool)
	for row, fields := range readTSV(t, filepath.Join(repoRoot, "tests/harness/manifest.tsv")) {
		if row > 0 {
			wantCases[fields[0]] = true
		}
	}
	wantAssertions := make(map[string]map[string]bool)
	for row, fields := range readTSV(t, filepath.Join(repoRoot, "tests/harness/assertions.tsv")) {
		if row == 0 {
			continue
		}
		if wantAssertions[fields[0]] == nil {
			wantAssertions[fields[0]] = make(map[string]bool)
		}
		wantAssertions[fields[0]][fields[1]] = true
	}
	if len(cases) != 33 || len(wantCases) != 33 {
		t.Fatalf("registry has %d cases and manifest has %d; want 33 each", len(cases), len(wantCases))
	}
	seenCases := make(map[string]bool)
	for _, tc := range cases {
		if seenCases[tc.id] || !wantCases[tc.id] {
			t.Fatalf("duplicate or unknown registry case %s", tc.id)
		}
		seenCases[tc.id] = true
		gotAssertions := make(map[string]bool)
		for _, id := range tc.assertions {
			gotAssertions[id] = true
		}
		if len(gotAssertions) != len(tc.assertions) || !reflect.DeepEqual(gotAssertions, wantAssertions[tc.id]) {
			t.Fatalf("assertion metadata differs for %s: got %v want %v", tc.id, gotAssertions, wantAssertions[tc.id])
		}
	}
}

func (t *caseTest) check(id string, ok bool, format string, args ...any) {
	t.Helper()
	if !t.want[id] {
		t.Fatalf("assertion %s is not declared for this case", id)
	}
	t.seen[id] = true
	if !ok {
		t.Errorf("%s: %s", id, fmt.Sprintf(format, args...))
	}
}

func cleanEnv(overrides map[string]string, unset ...string) []string {
	drop := map[string]bool{
		"CREW_ID": true, "CREW_WORKER_ID": true, "CREW_ROLE_ID": true,
		"DISPATCH_ENGINES": true, "TMUX": true, "TMUX_PANE": true,
		"DISPATCHER_CRITICS_DIR": true, "DISPATCHER_REVIEWERS_DIR": true,
		"DISPATCHER_SKILLS_DIR": true, "DISPATCH_PROFILE": true,
		"DISPATCH_SKIP_MODEL_CHECK": true, "DISPATCH_IGNORE_RUNG": true,
		"DISPATCH_GRANT_ROOTS": true, "DISPATCH_SPEC": true,
		"DISPATCH_SHAPE": true, "DISPATCH_DRAFT_PR": true,
		"DISPATCH_CLAUDE_CONNECTORS": true, "DISPATCH_LOCKED_SETTINGS": true,
	}
	for _, name := range unset {
		drop[name] = true
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !drop[name] {
			if _, replaced := overrides[name]; !replaced {
				env = append(env, entry)
			}
		}
	}
	for name, value := range overrides {
		env = append(env, name+"="+value)
	}
	return env
}

func runCommand(dir string, env []string, name string, args ...string) commandResult {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	status := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			status = exit.ExitCode()
		} else {
			status = -1
		}
	}
	// bats `run` strips trailing newlines only (command-substitution semantics);
	// TrimSpace would also forgive leading/interior whitespace drift.
	return commandResult{strings.TrimRight(stdout.String(), "\r\n"), strings.TrimRight(stderr.String(), "\r\n"), status}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

type repoFixture struct {
	t       *caseTest
	dir     string
	data    string
	stubDir string
	stubLog string
	env     []string
}

func newRepoFixture(t *caseTest) *repoFixture {
	t.Helper()
	base := t.TempDir()
	f := &repoFixture{t: t, dir: filepath.Join(base, "repo"), data: filepath.Join(base, "data"), stubDir: filepath.Join(base, "bin")}
	f.stubLog = filepath.Join(f.stubDir, "calls.log")
	for _, dir := range []string{f.dir, f.data, f.stubDir, filepath.Join(base, "config"), filepath.Join(base, "tmux")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.env = cleanEnv(map[string]string{
		"XDG_DATA_HOME": f.data, "XDG_CONFIG_HOME": filepath.Join(base, "config"),
		"PR_WATCH_CLOCK": filepath.Join(base, "clock"),
		"TMUX_TMPDIR":    filepath.Join(base, "tmux"), "GIT_CONFIG_GLOBAL": "/dev/null",
		"STUB_DIR": f.stubDir, "STUB_LOG": f.stubLog,
		"PATH":                f.stubDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CREW_RATE_AUTOSWEEP": "0",
		"WORKTREE_GIT_LIB":    filepath.Join(repoRoot, "adapters/core/worktree-git.sh"),
		"DISPATCH_CONFIG_BIN": filepath.Join(repoRoot, "adapters/core/dispatch-config.sh"),
		"GRANT_CHECK_LIB":     filepath.Join(repoRoot, "adapters/core/grant-check.sh"),
	})
	for _, args := range [][]string{{"init", "-q", "-b", "main", "."}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "test"}} {
		result := runCommand(f.dir, f.env, "git", args...)
		if result.status != 0 {
			t.Fatalf("git %v: %s", args, result.stderr)
		}
	}
	for _, name := range []string{"claude", "codex", "cursor-agent", "pi"} {
		f.stub(name, "printf '%s\\n' \"$*\" >>\"$STUB_LOG\"\nexit 0")
	}
	return f
}

func (f *repoFixture) stub(name, body string) {
	f.t.Helper()
	writeExecutable(f.t.T, filepath.Join(f.stubDir, name), "#!/usr/bin/env bash\nset -euo pipefail\n"+body+"\n")
}

func (f *repoFixture) withEnv(values map[string]string, unset ...string) []string {
	all := make(map[string]string)
	for _, entry := range f.env {
		name, value, _ := strings.Cut(entry, "=")
		all[name] = value
	}
	for name, value := range values {
		all[name] = value
	}
	return cleanEnv(all, unset...)
}

func crewCase(task string, crewID string, cwd func(*repoFixture) string, statusID, stdoutID string) func(*caseTest) {
	return func(t *caseTest) {
		f := newRepoFixture(t)
		if task != "" {
			if err := os.WriteFile(filepath.Join(f.dir, "WORKER_TASK.md"), []byte(task), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		dir := f.dir
		if cwd != nil {
			dir = cwd(f)
		}
		env := f.env
		if crewID != "" {
			env = f.withEnv(map[string]string{"CREW_ID": crewID})
		}
		result := runCommand(dir, env, "bash", "-euo", "pipefail", filepath.Join(repoRoot, "adapters/core/crew.sh"), "id")
		t.check(statusID, result.status == 0, "status=%d stderr=%q", result.status, result.stderr)
		want := "c-from-taskdoc"
		if task == "" || !strings.Contains(task, "crew_id:") {
			want = crewID
		}
		t.check(stdoutID, result.stdout == want, "stdout=%q want=%q", result.stdout, want)
	}
}

func refreshFixture(t *caseTest) *repoFixture {
	f := newRepoFixture(t)
	f.stub("cursor-agent", `printf '%s\n' "$*" >>"$STUB_LOG"
if [[ -n "${SHIM_CURSOR_FAIL:-}" ]]; then exit 1; fi
cat <<'MODELS'
Available models

auto - Auto (default)
gpt-5.3-codex-low - Codex 5.3 Low
cursor-grok-4.6-high - Cursor Grok 4.6
cursor-grok-4.6-medium-fast - Cursor Grok 4.6 Medium Fast
cursor-grok-4.6-low-fast - Cursor Grok 4.6 Low Fast
claude-opus-5-high - Claude Opus 5 1M

Tip: use --model <id>
MODELS`)
	return f
}

func runRefresh(f *repoFixture, env []string) commandResult {
	return runCommand(f.dir, env, "bash", filepath.Join(repoRoot, "adapters/core/refresh-models.sh"))
}

func cachePath(f *repoFixture) string { return filepath.Join(f.data, "crew/cursor-models-cache.json") }

func readJSON(t *caseTest, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func refreshShape(t *caseTest) {
	f := refreshFixture(t)
	r := runRefresh(f, f.env)
	t.check("rm-shape-script-status", r.status == 0, "status=%d stderr=%q", r.status, r.stderr)
	cache := readJSON(t, cachePath(f))
	models := cache["models"].([]any)
	slugs := make(map[string]bool)
	for _, model := range models {
		slugs[model.(map[string]any)["slug"].(string)] = true
	}
	t.check("rm-shape-high-slug", slugs["cursor-grok-4.6-high"], "missing high slug")
	t.check("rm-shape-medium-fast-slug", slugs["cursor-grok-4.6-medium-fast"], "missing medium-fast slug")
	t.check("rm-shape-low-fast-slug", slugs["cursor-grok-4.6-low-fast"], "missing low-fast slug")
	t.check("rm-shape-excludes-auto", !slugs["auto"], "auto was cached")
	_, epochIsNumber := cache["fetched_epoch"].(float64)
	t.check("rm-shape-epoch-type", epochIsNumber, "fetched_epoch=%T", cache["fetched_epoch"])
}

func refreshAtomic(t *caseTest) {
	f := refreshFixture(t)
	r := runRefresh(f, f.env)
	t.check("rm-atomic-script-status", r.status == 0, "status=%d stderr=%q", r.status, r.stderr)
	_, err := os.Stat(cachePath(f))
	t.check("rm-atomic-cache-exists", err == nil, "cache stat: %v", err)
	tmp, _ := filepath.Glob(filepath.Join(f.data, "crew/*.tmp.*"))
	t.check("rm-atomic-no-temp-file", len(tmp) == 0, "temporary files=%v", tmp)
	models := readJSON(t, cachePath(f))["models"].([]any)
	t.check("rm-atomic-model-count", len(models) == 5, "model count=%d", len(models))
}

func seedCache(t *caseTest, f *repoFixture) []byte {
	t.Helper()
	path := cachePath(f)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"fetched_at":"stale","fetched_epoch":1,"models":[{"slug":"stale-model"}]}` + "\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func refreshFailure(t *caseTest) {
	f := refreshFixture(t)
	before := seedCache(t, f)
	r := runRefresh(f, f.withEnv(map[string]string{"SHIM_CURSOR_FAIL": "1"}))
	after, _ := os.ReadFile(cachePath(f))
	t.check("rm-stub-failure-status", r.status != 0, "status=%d", r.status)
	t.check("rm-stub-failure-cache-unchanged", bytes.Equal(before, after), "cache changed")
}

func refreshAbsent(t *caseTest) {
	f := refreshFixture(t)
	before := seedCache(t, f)
	parts := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
	kept := parts[:0]
	for _, dir := range parts {
		if _, err := os.Stat(filepath.Join(dir, "cursor-agent")); err != nil {
			kept = append(kept, dir)
		}
	}
	r := runRefresh(f, f.withEnv(map[string]string{"PATH": strings.Join(kept, string(os.PathListSeparator))}))
	after, _ := os.ReadFile(cachePath(f))
	t.check("rm-absent-status", r.status != 0, "status=%d", r.status)
	t.check("rm-absent-cache-unchanged", bytes.Equal(before, after), "cache changed")
}

type prFixture struct {
	*repoFixture
	view    string
	threads string
}

func newPRFixture(t *caseTest) *prFixture {
	f := &prFixture{repoFixture: newRepoFixture(t)}
	f.view = filepath.Join(filepath.Dir(f.dir), "view.json")
	f.threads = filepath.Join(filepath.Dir(f.dir), "threads.txt")
	_ = os.WriteFile(f.threads, nil, 0o644)
	f.stub("gh", `printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in pr) cat "$GH_VIEW" ;; api) cat "$GH_THREADS" ;; esac`)
	f.env = f.withEnv(map[string]string{"GH_VIEW": f.view, "GH_THREADS": f.threads})
	f.setView("aaa11", "OPEN", "SUCCESS")
	return f
}

func (f *prFixture) setView(sha, state, conclusion string) {
	f.t.Helper()
	view := fmt.Sprintf(`{"headRefOid":%q,"state":%q,"reviewDecision":"","latestReviews":[],"statusCheckRollup":[{"name":"check","status":"COMPLETED","conclusion":%q}],"comments":[]}`+"\n", sha, state, conclusion)
	if err := os.WriteFile(f.view, []byte(view), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *prFixture) runWatch(dir string, args ...string) commandResult {
	bashArgs := []string{"-euo", "pipefail", filepath.Join(repoRoot, "adapters/core/pr-watch.sh")}
	return runCommand(dir, f.env, "bash", append(bashArgs, args...)...)
}

func (f *prFixture) seed(statusID, stdoutID, stderrID string) {
	r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
	f.t.check(statusID, r.status == 0, "status=%d", r.status)
	f.t.check(stdoutID, r.stdout == "", "stdout=%q", r.stdout)
	f.t.check(stderrID, strings.Contains(r.stderr, "park ended after 1s"), "stderr=%q", r.stderr)
}

func decode(t *caseTest, value string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		t.Fatalf("invalid JSON %q: %v", value, err)
	}
	return object
}

func changed(event map[string]any) []string {
	values := event["changed"].([]any)
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = value.(string)
	}
	return result
}

func prCase(kind string) func(*caseTest) {
	return func(t *caseTest) {
		f := newPRFixture(t)
		switch kind {
		case "first":
			f.seed("pw-seed-status", "pw-seed-empty-stdout", "pw-seed-timeout-stderr")
			_, err := os.Stat(filepath.Join(f.data, "crew/pr-watch/o/r/42.json"))
			t.check("pw-first-cursor-exists", err == nil, "cursor stat: %v", err)
		case "head":
			f.seed("pw-head-seed-status", "pw-head-seed-empty-stdout", "pw-head-seed-timeout-stderr")
			f.setView("bbb22", "OPEN", "SUCCESS")
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
			e := decode(t, r.stdout)
			t.check("pw-head-status", r.status == 0, "status=%d", r.status)
			t.check("pw-head-event-shape", strings.Join(changed(e), ",") == "head_sha" && e["pr"] == float64(42) && e["repo"] == "o/r", "event=%v", e)
			state, was := e["state"].(map[string]any), e["was"].(map[string]any)
			t.check("pw-head-state-transition", state["head_sha"] == "bbb22" && was["head_sha"] == "aaa11", "event=%v", e)
		case "thread":
			f.seed("pw-thread-seed-status", "pw-thread-seed-empty-stdout", "pw-thread-seed-timeout-stderr")
			_ = os.WriteFile(f.threads, []byte("2026-08-04T10:00:00Z\n"), 0o644)
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
			e := decode(t, r.stdout)
			got := changed(e)
			sort.Strings(got)
			t.check("pw-thread-status", r.status == 0, "status=%d", r.status)
			t.check("pw-thread-change-set", strings.Join(got, ",") == "thread_at,thread_n", "changed=%v", got)
			t.check("pw-thread-count", e["state"].(map[string]any)["thread_n"] == float64(1), "event=%v", e)
		case "checks":
			f.seed("pw-checks-seed-status", "pw-checks-seed-empty-stdout", "pw-checks-seed-timeout-stderr")
			f.setView("aaa11", "OPEN", "FAILURE")
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
			e := decode(t, r.stdout)
			t.check("pw-checks-status", r.status == 0, "status=%d", r.status)
			t.check("pw-checks-event", strings.Join(changed(e), ",") == "checks" && e["state"].(map[string]any)["checks"] == "FAILURE", "event=%v", e)
		case "pending":
			_ = os.WriteFile(f.view, []byte(`{"headRefOid":"aaa11","state":"OPEN","reviewDecision":"","latestReviews":[],"statusCheckRollup":[{"name":"a","status":"COMPLETED","conclusion":"SUCCESS"},{"name":"b","status":"IN_PROGRESS","conclusion":""}],"comments":[]}`+"\n"), 0o644)
			f.seed("pw-pending-seed-status", "pw-pending-seed-empty-stdout", "pw-pending-seed-timeout-stderr")
			state := readJSON(t, filepath.Join(f.data, "crew/pr-watch/o/r/42.json"))
			t.check("pw-pending-state", state["checks"] == "PENDING", "state=%v", state)
		case "merged":
			f.seed("pw-merged-seed-status", "pw-merged-seed-empty-stdout", "pw-merged-seed-timeout-stderr")
			f.setView("aaa11", "MERGED", "SUCCESS")
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
			e := decode(t, r.stdout)
			t.check("pw-merged-status", r.status == 0, "status=%d", r.status)
			t.check("pw-merged-event", strings.Join(changed(e), ",") == "state" && e["state"].(map[string]any)["state"] == "MERGED", "event=%v", e)
		case "unchanged":
			f.seed("pw-unchanged-seed-status", "pw-unchanged-seed-empty-stdout", "pw-unchanged-seed-timeout-stderr")
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
			t.check("pw-unchanged-status", r.status == 0, "status=%d", r.status)
			t.check("pw-unchanged-empty-stdout", r.stdout == "", "stdout=%q", r.stdout)
		case "restart":
			f.seed("pw-restart-seed-status", "pw-restart-seed-empty-stdout", "pw-restart-seed-timeout-stderr")
			f.setView("bbb22", "OPEN", "SUCCESS")
			first := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
			t.check("pw-restart-event-status", first.status == 0, "status=%d", first.status)
			t.check("pw-restart-event-stdout", first.stdout != "", "stdout empty")
			second := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
			t.check("pw-restart-repeat-status", second.status == 0, "status=%d", second.status)
			t.check("pw-restart-repeat-empty-stdout", second.stdout == "", "stdout=%q", second.stdout)
		case "no-crew":
			f.seed("pw-no-crew-seed-status", "pw-no-crew-seed-empty-stdout", "pw-no-crew-seed-timeout-stderr")
			f.setView("bbb22", "OPEN", "SUCCESS")
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
			t.check("pw-no-crew-status", r.status == 0, "status=%d", r.status)
			t.check("pw-no-crew-event", r.stdout != "", "stdout empty")
			t.check("pw-no-crew-no-bus", !exists(filepath.Join(f.dir, ".git/crew")), "crew bus exists")
		case "explicit":
			r := f.runWatch("/", "42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
			t.check("pw-explicit-repo-status", r.status == 0, "status=%d stderr=%q", r.status, r.stderr)
			t.check("pw-explicit-repo-cursor", exists(filepath.Join(f.data, "crew/pr-watch/o/r/42.json")), "cursor absent")
		case "derived":
			runCommand(f.dir, f.env, "git", "remote", "add", "origin", "git@github.com:o/derived.git")
			r := f.runWatch(f.dir, "42", "--timeout", "1", "--interval", "1")
			t.check("pw-derived-repo-status", r.status == 0, "status=%d stderr=%q", r.status, r.stderr)
			t.check("pw-derived-repo-cursor", exists(filepath.Join(f.data, "crew/pr-watch/o/derived/42.json")), "cursor absent")
		case "missing":
			r := f.runWatch(f.dir, "--repo", "o/r")
			t.check("pw-missing-pr-status", r.status == 1, "status=%d", r.status)
			t.check("pw-missing-pr-usage", strings.Contains(r.stderr, "usage: pr-watch"), "stderr=%q", r.stderr)
		case "unbounded":
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "0")
			t.check("pw-unbounded-status", r.status == 1, "status=%d", r.status)
			t.check("pw-unbounded-message", strings.Contains(r.stderr, "must be > 0"), "stderr=%q", r.stderr)
		case "unknown":
			r := f.runWatch(f.dir, "42", "--bogus")
			t.check("pw-unknown-status", r.status == 1, "status=%d", r.status)
			t.check("pw-unknown-message", strings.Contains(r.stderr, "unknown arg"), "stderr=%q", r.stderr)
		case "gh-failure":
			f.stub("gh", "exit 1")
			r := f.runWatch(f.dir, "42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
			t.check("pw-gh-failure-status", r.status == 1, "status=%d", r.status)
			t.check("pw-gh-failure-message", strings.Contains(r.stderr, "could not read PR 42"), "stderr=%q", r.stderr)
		case "crew-event":
			f.seed("pw-crew-event-seed-status", "pw-crew-event-seed-empty-stdout", "pw-crew-event-seed-timeout-stderr")
			f.setView("bbb22", "OPEN", "SUCCESS")
			f.installWatchShim()
			r := runCommand(f.dir, f.withEnv(map[string]string{"CREW_ID": "c1"}), "bash", "-euo", "pipefail", filepath.Join(repoRoot, "adapters/core/crew.sh"), "pr-watch", "42", "--repo", "o/r", "--timeout", "30", "--interval", "1")
			t.check("pw-crew-event-status", r.status == 0, "status=%d stderr=%q", r.status, r.stderr)
			t.check("pw-crew-event-stdout", r.stdout != "", "stdout empty")
			row := eventRow(t, filepath.Join(f.dir, ".git/crew/events.jsonl"))
			t.check("pw-crew-event-bus-row", row == "pr-watch:42|dispatcher:c1|head_sha", "row=%q", row)
		case "crew-timeout":
			f.seed("pw-crew-timeout-seed-status", "pw-crew-timeout-seed-empty-stdout", "pw-crew-timeout-seed-timeout-stderr")
			f.installWatchShim()
			r := runCommand(f.dir, f.withEnv(map[string]string{"CREW_ID": "c1"}), "bash", "-euo", "pipefail", filepath.Join(repoRoot, "adapters/core/crew.sh"), "pr-watch", "42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
			t.check("pw-crew-timeout-status", r.status == 0, "status=%d stderr=%q", r.status, r.stderr)
			t.check("pw-crew-timeout-empty-stdout", r.stdout == "", "stdout=%q", r.stdout)
			t.check("pw-crew-timeout-no-bus-row", !exists(filepath.Join(f.dir, ".git/crew/events.jsonl")), "events file exists")
		case "crew-child-fail":
			// A failed park ends the arm with the child's status before the post,
			// and before the print — the partial event JSON included.
			f.stub("pr-watch", "printf '%s\\n' '{\"pr\":42,\"changed\":[\"head_sha\"]}'\nprintf '%s\\n' 'gh: something went wrong' >&2\nexit 3")
			r := runCommand(f.dir, f.withEnv(map[string]string{"CREW_ID": "c1"}), "bash", "-euo", "pipefail", filepath.Join(repoRoot, "adapters/core/crew.sh"), "pr-watch", "42")
			t.check("pw-crew-child-status", r.status == 3, "status=%d stderr=%q", r.status, r.stderr)
			t.check("pw-crew-child-stderr", strings.Contains(r.stderr, "gh: something went wrong"), "stderr=%q", r.stderr)
			t.check("pw-crew-child-no-partial-stdout", !strings.Contains(r.stdout, `"changed"`), "stdout=%q", r.stdout)
			t.check("pw-crew-child-no-bus-row", !exists(filepath.Join(f.dir, ".git/crew/events.jsonl")), "events file exists")
		case "default-clock":
			env := make([]string, 0, len(f.env))
			for _, entry := range f.env {
				name, _, _ := strings.Cut(entry, "=")
				if name == "PR_WATCH_CLOCK" {
					continue
				}
				env = append(env, entry)
			}
			started := time.Now()
			r := runCommand(f.dir, env, "bash", "-euo", "pipefail", filepath.Join(repoRoot, "adapters/core/pr-watch.sh"), "42", "--repo", "o/r", "--timeout", "1", "--interval", "1")
			elapsed := time.Since(started)
			t.check("pw-default-clock-status", r.status == 0, "status=%d stderr=%q", r.status, r.stderr)
			t.check("pw-default-clock-timeout-stderr", strings.Contains(r.stderr, "park ended after 1s"), "stderr=%q", r.stderr)
			t.check("pw-default-clock-elapsed", elapsed >= time.Second, "elapsed=%s", elapsed)
		}
	}
}

func (f *prFixture) installWatchShim() {
	f.stub("pr-watch", fmt.Sprintf("exec bash -euo pipefail %q \"$@\"", filepath.Join(repoRoot, "adapters/core/pr-watch.sh")))
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func eventRow(t *caseTest, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The bats oracle is `jq -r 'select(.kind=="msg") | ...'` compared against the
	// full stdout: every line must parse, every msg body must be a JSON string
	// with a changed list, and all rows join with newlines — not first-row-wins.
	trimmed := strings.TrimRight(string(data), "\r\n")
	if trimmed == "" {
		return ""
	}
	var rows []string
	for _, line := range strings.Split(trimmed, "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("event log line is not JSON: %v", err)
		}
		if event["kind"] != "msg" {
			continue
		}
		body, ok := event["body"].(string)
		if !ok {
			t.Fatalf("msg event body is not a string: %v", event["body"])
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("msg event body is not JSON: %v", err)
		}
		changed, ok := parsed["changed"].([]any)
		if !ok || len(changed) == 0 {
			t.Fatalf("msg event body has no changed list: %v", parsed)
		}
		rows = append(rows, fmt.Sprintf("%s|%s|%s", event["from"], event["to"], changed[0]))
	}
	return strings.Join(rows, "\n")
}

func ids(values ...string) []string { return values }

func manifestCases() []testCase {
	nested := func(f *repoFixture) string {
		dir := filepath.Join(f.dir, "a/b/c")
		_ = os.MkdirAll(dir, 0o755)
		return dir
	}
	return []testCase{
		{"crew-id-crew-id-resolves-from-worker-task-md-with-no-crew-id-in-the-environment-at-all", ids("cid-taskdoc-unset-status", "cid-taskdoc-unset-stdout"), crewCase("crew_id: c-from-taskdoc\n", "", nil, "cid-taskdoc-unset-status", "cid-taskdoc-unset-stdout")},
		{"crew-id-crew-id-the-task-document-wins-over-a-disagreeing-crew-id", ids("cid-taskdoc-wins-status", "cid-taskdoc-wins-stdout"), crewCase("crew_id: c-from-taskdoc\n", "c-from-env", nil, "cid-taskdoc-wins-status", "cid-taskdoc-wins-stdout")},
		{"crew-id-crew-id-resolves-from-a-subdirectory-of-the-worktree", ids("cid-subdir-status", "cid-subdir-stdout"), crewCase("crew_id: c-from-taskdoc\n", "", nested, "cid-subdir-status", "cid-subdir-stdout")},
		{"crew-id-crew-id-crew-id-still-resolves-when-no-worker-task-md-exists", ids("cid-env-status", "cid-env-stdout"), crewCase("", "c-from-env", nil, "cid-env-status", "cid-env-stdout")},
		{"crew-id-crew-id-falls-back-to-crew-id-when-worker-task-md-has-no-crew-id-line", ids("cid-taskdoc-missing-status", "cid-taskdoc-missing-stdout"), crewCase("title: some task\n", "c-from-env", nil, "cid-taskdoc-missing-status", "cid-taskdoc-missing-stdout")},
		{"refresh-models-a-realistic-list-models-fixture-parses-into-the-expected-cache-shape", ids("rm-shape-script-status", "rm-shape-high-slug", "rm-shape-medium-fast-slug", "rm-shape-low-fast-slug", "rm-shape-excludes-auto", "rm-shape-epoch-type"), refreshShape},
		{"refresh-models-the-write-is-atomic-and-lands-at-the-cursor-models-cache-path", ids("rm-atomic-script-status", "rm-atomic-cache-exists", "rm-atomic-no-temp-file", "rm-atomic-model-count"), refreshAtomic},
		{"refresh-models-a-stubbed-cursor-agent-failure-exits-nonzero-and-leaves-an-existing-cache-untouched", ids("rm-stub-failure-status", "rm-stub-failure-cache-unchanged"), refreshFailure},
		{"refresh-models-cursor-agent-absent-from-path-exits-nonzero-and-leaves-an-existing-cache-untouched", ids("rm-absent-status", "rm-absent-cache-unchanged"), refreshAbsent},
		{"pr-watch-the-first-park-seeds-the-cursor-and-reports-nothing", ids("pw-seed-status", "pw-seed-empty-stdout", "pw-seed-timeout-stderr", "pw-first-cursor-exists"), prCase("first")},
		{"pr-watch-fires-on-a-head-sha-move", ids("pw-head-seed-status", "pw-head-seed-empty-stdout", "pw-head-seed-timeout-stderr", "pw-head-status", "pw-head-event-shape", "pw-head-state-transition"), prCase("head")},
		{"pr-watch-fires-on-a-new-review-thread-reply", ids("pw-thread-seed-status", "pw-thread-seed-empty-stdout", "pw-thread-seed-timeout-stderr", "pw-thread-status", "pw-thread-change-set", "pw-thread-count"), prCase("thread")},
		{"pr-watch-fires-when-the-check-rollup-conclusion-flips", ids("pw-checks-seed-status", "pw-checks-seed-empty-stdout", "pw-checks-seed-timeout-stderr", "pw-checks-status", "pw-checks-event"), prCase("checks")},
		{"pr-watch-one-in-flight-check-keeps-the-rollup-pending-not-successful", ids("pw-pending-seed-status", "pw-pending-seed-empty-stdout", "pw-pending-seed-timeout-stderr", "pw-pending-state"), prCase("pending")},
		{"pr-watch-fires-when-the-pr-merges", ids("pw-merged-seed-status", "pw-merged-seed-empty-stdout", "pw-merged-seed-timeout-stderr", "pw-merged-status", "pw-merged-event"), prCase("merged")},
		{"pr-watch-an-unchanged-poll-reports-nothing-and-exits-0", ids("pw-unchanged-seed-status", "pw-unchanged-seed-empty-stdout", "pw-unchanged-seed-timeout-stderr", "pw-unchanged-status", "pw-unchanged-empty-stdout"), prCase("unchanged")},
		{"pr-watch-the-cursor-prevents-re-delivery-across-restarts", ids("pw-restart-seed-status", "pw-restart-seed-empty-stdout", "pw-restart-seed-timeout-stderr", "pw-restart-event-status", "pw-restart-event-stdout", "pw-restart-repeat-status", "pw-restart-repeat-empty-stdout"), prCase("restart")},
		{"pr-watch-runs-with-crew-id-unset-and-never-touches-the-bus", ids("pw-no-crew-seed-status", "pw-no-crew-seed-empty-stdout", "pw-no-crew-seed-timeout-stderr", "pw-no-crew-status", "pw-no-crew-event", "pw-no-crew-no-bus"), prCase("no-crew")},
		{"pr-watch-works-with-no-git-repo-at-all-when-repo-is-given", ids("pw-explicit-repo-status", "pw-explicit-repo-cursor"), prCase("explicit")},
		{"pr-watch-derives-the-repo-from-the-origin-remote", ids("pw-derived-repo-status", "pw-derived-repo-cursor"), prCase("derived")},
		{"pr-watch-aborts-without-a-pr-number", ids("pw-missing-pr-status", "pw-missing-pr-usage"), prCase("missing")},
		{"pr-watch-rejects-an-unbounded-park", ids("pw-unbounded-status", "pw-unbounded-message"), prCase("unbounded")},
		{"pr-watch-rejects-an-unknown-flag", ids("pw-unknown-status", "pw-unknown-message"), prCase("unknown")},
		{"pr-watch-a-first-poll-that-cannot-read-the-pr-fails-loudly", ids("pw-gh-failure-status", "pw-gh-failure-message"), prCase("gh-failure")},
		{"pr-watch-crew-pr-watch-posts-the-event-to-the-crew-s-dispatcher", ids("pw-crew-event-seed-status", "pw-crew-event-seed-empty-stdout", "pw-crew-event-seed-timeout-stderr", "pw-crew-event-status", "pw-crew-event-stdout", "pw-crew-event-bus-row"), prCase("crew-event")},
		{"pr-watch-crew-pr-watch-posts-nothing-when-the-park-times-out", ids("pw-crew-timeout-seed-status", "pw-crew-timeout-seed-empty-stdout", "pw-crew-timeout-seed-timeout-stderr", "pw-crew-timeout-status", "pw-crew-timeout-empty-stdout", "pw-crew-timeout-no-bus-row"), prCase("crew-timeout")},
		{"pr-watch-crew-pr-watch-exits-with-the-child-s-status-and-posts-nothing", ids("pw-crew-child-status", "pw-crew-child-stderr", "pw-crew-child-no-partial-stdout", "pw-crew-child-no-bus-row"), prCase("crew-child-fail")},
		{"pr-watch-default-clock-a-1s-park-really-waits", ids("pw-default-clock-status", "pw-default-clock-timeout-stderr", "pw-default-clock-elapsed"), prCase("default-clock")},
		{"role-watch-role-watch-a-permission-dialog-receives-no-keys-until-it-clears-then-the-assignment-lands-once", ids("rw-dialog-clear-no-sends", "rw-dialog-clear-one-send"), roleWatchCase("dialog-clear")},
		{"role-watch-role-watch-option-select-quota-live-turn-and-unrecognised-claude-frames-defer", ids("rw-defer-frames-captured"), roleWatchCase("defer-frames")},
		{"role-watch-role-watch-an-idle-claude-input-box-receives-the-assignment", ids("rw-idle-deliver-one-send"), roleWatchCase("idle")},
		{"role-watch-role-watch-queued-assignments-go-out-one-per-tick-in-order", ids("rw-queue-order-first-send", "rw-queue-order-second-once", "rw-queue-order-ordering"), roleWatchCase("queue")},
		{"role-watch-role-watch-a-dialog-raised-after-the-text-is-typed-is-never-confirmed", ids("rw-late-dialog-sends-two", "rw-late-dialog-single-enter"), roleWatchCase("late-dialog")},
	}
}
