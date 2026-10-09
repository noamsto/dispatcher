package rate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// fixture is an in-process sweep: a bus, a checkout whose git answers are
// scripted, a stubbed gh, and a store under a temp XDG root. The bats suite
// covers the same surface end to end through the bash arm; what lives here is
// the part a shell test cannot reach — a dead lock holder, an off-repo pr_url
// with a call log to inspect, the exact argv gh is handed.
type fixture struct {
	t      *testing.T
	paths  bus.Paths
	store  string
	calls  []string
	gh     func(args string) (string, bool)
	git    func(args string) (string, bool)
	config string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:     t,
		paths: bus.Paths{Common: filepath.Join(root, ".git")},
	}
	f.paths.Dir = f.paths.Common + "/crew"
	f.paths.Log = f.paths.Dir + "/events.jsonl"
	f.store = filepath.Join(root, "data", "crew", "ratings.jsonl")
	if err := os.MkdirAll(f.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.gh = func(string) (string, bool) { return "{}", true }
	// The checkout has a github origin, which is what gates the reconcile, and
	// a toplevel for the repo-name fallback.
	f.git = func(args string) (string, bool) {
		switch {
		case strings.HasPrefix(args, "config"):
			return "https://github.com/acme/widgets.git", true
		case strings.HasPrefix(args, "rev-parse"):
			return "/repo", true
		}
		return "", true
	}
	return f
}

// event appends one bus line.
func (f *fixture) event(obj string) {
	f.t.Helper()
	fl, err := os.OpenFile(f.paths.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = fl.Close() }()
	if _, err := fl.WriteString(obj + "\n"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) dispatch(branch string, ts int64, model, effort string) {
	f.event(fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":%q,"engine":"claude","model":%q,"tier":"standard","effort":%q,"title":"t"}`,
		ts, branch, model, effort))
}

func (f *fixture) status(from string, ts int64, state string, prURL ...string) {
	f.t.Helper()
	url := ""
	if len(prURL) > 0 {
		url = prURL[0]
	}
	pr := ""
	if url != "" {
		pr = fmt.Sprintf(`,"pr_url":%q`, url)
	}
	f.event(fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":"dispatcher:c1","kind":"status","body":{"state":%q%s}}`,
		ts, from, state, pr))
}

func (f *fixture) seedPRRow() {
	f.dispatch("feat/x", 1000, "sonnet", "medium")
	f.status("worker:feat/x#s1", 1500, "pr_open", "https://github.com/acme/widgets/pull/1")
	f.status("worker:feat/x#s1", 1600, "done")
}

// runQuiet is the sweep with its stdout captured (it must stay empty) and its
// stderr returned.
func (f *fixture) runQuiet() (string, int) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := Run(nil, f.paths, "/repo", &out, &errb, f.options())
	if out.Len() != 0 {
		f.t.Errorf("stdout = %q, want clean", out.String())
	}
	return errb.String(), code
}

func (f *fixture) options() Options {
	return Options{
		StorePath: f.store,
		LookupEnv: func(key string) string {
			if key == "DISPATCH_CONFIG_BIN" {
				return "dispatch-config"
			}
			return ""
		},
		Now: func() time.Time { return time.Unix(1800000000, 0) },
		Run: func(args ...string) (string, error) {
			joined := strings.Join(args, " ")
			f.calls = append(f.calls, joined)
			switch args[0] {
			case "dispatch-config":
				if f.config == "" {
					return "", fmt.Errorf("dispatch-config: not found")
				}
				return f.config, nil
			case "gh":
				out, ok := f.gh(strings.Join(args[1:], " "))
				if !ok {
					return out, fmt.Errorf("gh exited 1")
				}
				return out, nil
			case "git":
				out, ok := f.git(strings.Join(args[3:], " "))
				return out, boolErr(ok)
			}
			return "", fmt.Errorf("unexpected command %q", joined)
		},
	}
}

func boolErr(ok bool) error {
	if ok {
		return nil
	}
	return fmt.Errorf("exit status 1")
}

// defaultSettings is a two-rule burn table small enough to read.
const defaultSettings = `{"burnClasses":[{"match":"*sonnet*","class":"standard","weight":2}]}`

func (f *fixture) seedSettings() { f.config = defaultSettings }

// rows reads the store back as folded rows.
func (f *fixture) rows() []map[string]any {
	f.t.Helper()
	data, err := os.ReadFile(f.store)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			f.t.Fatalf("store line %q: %v", line, err)
		}
		out = append(out, row)
	}
	return out
}

func (f *fixture) callLog() string {
	return strings.Join(f.calls, "\n")
}

func (f *fixture) seedStore(line string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.store), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.store, []byte(line+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func TestSweepNoBusExitsClean(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	if _, code := f.runQuiet(); code != 0 {
		t.Fatalf("exit %d, want 0 with no bus", code)
	}
	if _, err := os.Stat(f.store); !os.IsNotExist(err) {
		t.Error("a sweep with no bus created a store")
	}
}

func TestSweepWritesOneRowPerRun(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	f.gh = func(args string) (string, bool) {
		switch {
		case strings.HasPrefix(args, "pr view"):
			return `{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}`, true
		case strings.Contains(args, "graphql"):
			return `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false}]}}}}}`, true
		default:
			return `{"workflow_runs":[]}`, true
		}
	}
	_, code := f.runQuiet()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	rows := f.rows()
	if len(rows) != 1 {
		t.Fatalf("store has %d rows: %v", len(rows), rows)
	}
	row := rows[0]
	if row["run_id"] != "acme/widgets:feat/x:1000" {
		t.Errorf("run_id = %v", row["run_id"])
	}
	if row["outcome"] != "pr_open" || row["owns_pr"] != true || row["pr_state"] != "OPEN" {
		t.Errorf("row = %v", row)
	}
	if row["cost_class"] != "standard" || row["cost_proxy"] != float64(2*600) {
		t.Errorf("cost = %v/%v, want standard/1200 (weight 2 x the 600 ms wall clock)", row["cost_class"], row["cost_proxy"])
	}
	if row["unresolved_notes"] != float64(1) {
		t.Errorf("unresolved_notes = %v, want 1", row["unresolved_notes"])
	}
	// The pr view call always carries the explicit URL: a bare `gh pr view`
	// would resolve the sweeping checkout's own branch into every row.
	if !strings.Contains(f.callLog(), "gh pr view https://github.com/acme/widgets/pull/1 --json state,closedAt,mergedAt,mergeCommit,commits,reviews") {
		t.Errorf("pr view argv:\n%s", f.callLog())
	}
	if !strings.Contains(f.callLog(), "gh api --method GET repos/acme/widgets/actions/runs -f branch=feat/x -F per_page=100") {
		t.Errorf("actions argv:\n%s", f.callLog())
	}
	if !strings.Contains(f.callLog(), "gh api graphql -f query="+threadsQuery+" -F owner=acme -F name=widgets -F number=1") {
		t.Errorf("graphql argv:\n%s", f.callLog())
	}
}

// TestSweepOffRepoGate is the zero-call guarantee: a pr_url that is not a PR of
// this repo may not spend this repo's credentials, and may not be queried even
// to find that out.
func TestSweepOffRepoGate(t *testing.T) {
	for _, url := range []string{
		"https://github.com/other/widgets/pull/1",
		"https://gitlab.com/acme/widgets/-/merge_requests/1",
		"https://github.com/acme/widgets/pull/1/",
		"not a url",
	} {
		t.Run(url, func(t *testing.T) {
			f := newFixture(t)
			f.seedSettings()
			f.dispatch("feat/x", 1000, "sonnet", "medium")
			f.status("worker:feat/x#s1", 1500, "pr_open", url)
			f.status("worker:feat/x#s1", 1600, "done")
			if _, code := f.runQuiet(); code != 0 {
				t.Fatalf("exit %d", code)
			}
			for _, call := range f.calls {
				if strings.HasPrefix(call, "gh ") {
					t.Errorf("a gh call ran for %s: %s", url, call)
				}
			}
			rows := f.rows()
			if len(rows) != 1 || rows[0]["outcome"] != "pr_open" {
				t.Errorf("rows = %v", rows)
			}
		})
	}
}

// TestSweepMergedSkipsView pins per-call finality: a MERGED row skips `pr view`
// but keeps querying Actions while its CI result is still unknown.
func TestSweepMergedSkipsView(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	f.seedStore(`{"run_id":"acme/widgets:feat/x:1000","repo":"acme/widgets","pr_state":"MERGED","merged_at_ms":1200,"merge_commit":"abc","unresolved_notes":0,"first_ci_green":null,"swept_at":1}`)
	f.gh = func(args string) (string, bool) {
		if strings.Contains(args, "actions/runs") {
			return `{"workflow_runs":[{"head_sha":"s","created_at":"2026-01-01T00:00:00Z","status":"completed","conclusion":"success"}]}`, true
		}
		return `{}`, true
	}
	if _, code := f.runQuiet(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(f.callLog(), "pr view") {
		t.Errorf("a merged row re-queried pr view:\n%s", f.callLog())
	}
	if !strings.Contains(f.callLog(), "actions/runs") {
		t.Errorf("a merged row with unknown CI skipped Actions:\n%s", f.callLog())
	}
	if strings.Contains(f.callLog(), "graphql") {
		t.Errorf("a merged row with known notes re-queried threads:\n%s", f.callLog())
	}
	rows := f.rows()
	if len(rows) != 2 {
		t.Fatalf("merge-forward appended %d rows, want the one changed row", len(rows))
	}
	last := rows[len(rows)-1]
	if last["pr_state"] != "MERGED" || last["first_ci_green"] != true {
		t.Errorf("merged row = %v", last)
	}
	// The merge commit carried forward from the store is what the revert probe
	// runs against, even though pr view never re-ran.
	if !strings.Contains(f.callLog(), "cat-file -e abc^{commit}") {
		t.Errorf("revert probe did not use the stored merge commit:\n%s", f.callLog())
	}
}

func TestSweepSecondSweepAppendsNothing(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	f.gh = func(args string) (string, bool) {
		switch {
		case strings.HasPrefix(args, "pr view"):
			return `{"state":"MERGED","closedAt":"2026-01-02T00:00:00Z","mergedAt":"2026-01-02T00:00:00Z","mergeCommit":null,"commits":[],"reviews":[]}`, true
		default:
			return `{"workflow_runs":[]}`, true
		}
	}
	if _, code := f.runQuiet(); code != 0 {
		t.Fatalf("first sweep exit %d", code)
	}
	first := len(f.rows())
	before, err := os.ReadFile(f.store)
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if _, code := f.runQuiet(); code != 0 {
		t.Fatalf("second sweep exit %d", code)
	}
	after, err := os.ReadFile(f.store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("an unchanged sweep appended: %d rows -> %d rows", first, len(f.rows()))
	}
}

func TestSweepBusyLock(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	lockd := filepath.Join(filepath.Dir(f.store), "ratings.lock.d")
	if err := os.MkdirAll(lockd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockd, "pid"), []byte(livePID(t)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr, code := f.runQuiet()
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, busy) {
		t.Errorf("stderr = %q", stderr)
	}
	if _, err := os.Stat(f.store); !os.IsNotExist(err) {
		t.Error("a refused lock still wrote the store")
	}
}

// livePID starts a child that stays alive for the test and returns its pid.
func livePID(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a live child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return strconv.Itoa(cmd.Process.Pid)
}

func TestSweepGHFailureKeepsStoredRows(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	f.seedStore(`{"run_id":"acme/widgets:feat/x:1000","repo":"acme/widgets","pr_state":"OPEN","first_ci_green":true,"unresolved_notes":3,"swept_at":1}`)
	f.gh = func(string) (string, bool) { return "", false }
	stderr, code := f.runQuiet()
	if code != 0 {
		t.Fatalf("exit %d, want 0 with gh down", code)
	}
	// pr view and the threads call are both still open (Actions already
	// answered), so two failures are counted.
	if !strings.Contains(stderr, "crew: rate: 2 gh call(s) failed; those rows kept their stored t2") {
		t.Errorf("stderr = %q", stderr)
	}
	rows := f.rows()
	last := rows[len(rows)-1]
	if last["unresolved_notes"] != float64(3) || last["first_ci_green"] != true ||
		last["pr_state"] != "OPEN" || last["last_query_ok"] != false {
		t.Errorf("stored t2 was not merged forward: %v", last)
	}
}

func TestSweepBurnWarning(t *testing.T) {
	f := newFixture(t)
	f.config = "" // dispatch-config unavailable
	f.seedPRRow()
	stderr, code := f.runQuiet()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "crew rate: could not resolve settings (dispatch-config unavailable)") {
		t.Errorf("stderr = %q", stderr)
	}
	if strings.Count(stderr, "\n") != 1 {
		t.Errorf("the burn warning must print once, got %q", stderr)
	}
	rows := f.rows()
	if len(rows) != 1 || rows[0]["cost_class"] != nil {
		t.Errorf("cost_class = %v, want null", rows)
	}
}

// TestSweepStoreIsADirectory is the append-open failure `reap`'s autosweep hook
// absorbs: the sweep must fail, not silently report success.
func TestSweepStoreIsADirectory(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	if err := os.MkdirAll(f.store, 0o755); err != nil {
		t.Fatal(err)
	}
	f.gh = func(string) (string, bool) { return `{}`, true }
	if _, code := f.runQuiet(); code == 0 {
		t.Fatal("a store that cannot be opened for append must fail the sweep")
	}
	if info, err := os.Stat(f.store); err != nil || !info.IsDir() {
		t.Error("the failed append replaced the store path")
	}
	if !strings.Contains(f.callLog(), "gh ") {
		t.Error("the sweep did not reach the reconcile before the append killed it")
	}
}

func TestSweepRegistry(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	f.gh = func(string) (string, bool) { return `{}`, true }
	if _, code := f.runQuiet(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	registry := filepath.Join(filepath.Dir(f.store), "repos")
	data, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != f.paths.Common+"\n" {
		t.Errorf("registry = %q, want %q", data, f.paths.Common+"\n")
	}
	if _, code := f.runQuiet(); code != 0 {
		t.Fatalf("second sweep exit %d", code)
	}
	again, _ := os.ReadFile(registry)
	if string(again) != f.paths.Common+"\n" {
		t.Errorf("registry after a second sweep = %q", again)
	}
}

// TestSweepRevertProbe covers the three states of `reverted`: a merge commit
// absent from this checkout is unknown, one present with no revert is false,
// and one a revert commit names is true.
func TestSweepRevertProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
		revert  string
		want    any
	}{
		{"absent", false, "", nil},
		{"present, no revert", true, "", false},
		{"reverted", true, "abc123\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedSettings()
			f.seedPRRow()
			f.gh = func(args string) (string, bool) {
				if strings.HasPrefix(args, "pr view") {
					return `{"state":"MERGED","closedAt":"2026-01-02T00:00:00Z","mergedAt":"2026-01-02T00:00:00Z","mergeCommit":{"oid":"abc123"},"commits":[],"reviews":[]}`, true
				}
				return `{"workflow_runs":[]}`, true
			}
			base := f.git
			f.git = func(args string) (string, bool) {
				switch {
				case strings.HasPrefix(args, "cat-file"):
					return "", tc.present
				case strings.HasPrefix(args, "log"):
					return tc.revert, true
				}
				return base(args)
			}
			if _, code := f.runQuiet(); code != 0 {
				t.Fatalf("exit %d", code)
			}
			rows := f.rows()
			got := rows[len(rows)-1]["reverted"]
			if got != tc.want {
				t.Errorf("reverted = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSweepNothingOnStdout is the contract the autosweep hook depends on: the
// sweep's only output is stderr warnings.
func TestSweepNothingOnStdout(t *testing.T) {
	f := newFixture(t)
	f.seedSettings()
	f.seedPRRow()
	_, code := f.runQuiet()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestFoldStoreTornFileIsEmpty pins the reader-side wart every crew reader
// shares: one torn line costs the whole store rather than the row.
func TestFoldStoreTornFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ratings.jsonl")
	if err := os.WriteFile(path, []byte(`{"run_id":"a","swept_at":1}`+"\n"+`{"run_id":"b","swept`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rows := foldStore(path).Len(); rows != 0 {
		t.Errorf("a torn store folded to %d rows, want []", rows)
	}
	if err := os.WriteFile(path, []byte(`{"run_id":"a","swept_at":1}`+"\n"+`{"run_id":"a","swept_at":2}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rows := foldStore(path); rows.Len() != 1 {
		t.Errorf("last-wins fold kept %d rows, want the newest one", rows.Len())
	}
}
