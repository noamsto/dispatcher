package rate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

const (
	pr1URL      = "https://github.com/acme/widgets/pull/1"
	viewOpen    = `{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}`
	viewMerged5 = `{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:05Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}`
	noRuns      = `{"workflow_runs":[]}`
	noThreads   = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`
)

// sweepFixture is the fixture with a small burn table, a gh serving the
// three calls a sweep can make from per-test responses, and a clock that moves
// one second per read so successive sweeps get distinct swept_at values.
type sweepFixture struct {
	*fixture
	view, runs, threads string
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	f := &sweepFixture{fixture: newFixture(t), view: viewOpen, runs: noRuns, threads: noThreads}
	f.seedSettings()
	reads := 0
	f.optionsNow = func() time.Time {
		reads++
		return time.Unix(1800000000+int64(reads), 0)
	}
	f.gh = func(args string) (string, bool) {
		switch {
		case strings.HasPrefix(args, "pr "):
			return f.view, true
		case strings.Contains(args, "graphql"):
			return f.threads, true
		}
		return f.runs, true
	}
	return f
}

func (f *sweepFixture) dispatchAs(branch string, ts int64, engine, model, tier, effort string) {
	f.event(fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":%q,"engine":%q,"model":%q,"tier":%q,"effort":%q,"title":"t"}`,
		ts, branch, engine, model, tier, effort))
}

func (f *sweepFixture) sweep() {
	f.t.Helper()
	if stderr, code := f.runQuiet(); code != 0 {
		f.t.Fatalf("sweep exit %d, stderr %q", code, stderr)
	}
}

// folded is the store read last-wins by run_id, the idiom --report uses.
func (f *sweepFixture) folded() []map[string]any {
	f.t.Helper()
	last := map[string]map[string]any{}
	var order []string
	for _, row := range f.rows() {
		id := row["run_id"].(string)
		if _, ok := last[id]; !ok {
			order = append(order, id)
		}
		last[id] = row
	}
	out := make([]map[string]any, 0, len(order))
	for _, id := range order {
		out = append(out, last[id])
	}
	return out
}

func (f *sweepFixture) row(branch string) map[string]any {
	f.t.Helper()
	for _, row := range f.folded() {
		if row["branch"] == branch {
			return row
		}
	}
	f.t.Fatalf("no row for %s in %v", branch, f.folded())
	return nil
}

func (f *sweepFixture) only() map[string]any {
	f.t.Helper()
	rows := f.folded()
	if len(rows) != 1 {
		f.t.Fatalf("store folds to %d rows: %v", len(rows), rows)
	}
	return rows[0]
}

func (f *sweepFixture) ghCalls() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "gh ") {
			out = append(out, strings.TrimPrefix(c, "gh "))
		}
	}
	return out
}

func (f *sweepFixture) ghCalled(prefix string) bool {
	for _, c := range f.ghCalls() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *sweepFixture) prRun(branch string, ts int64) {
	f.dispatchAs(branch, ts, "claude", "sonnet", "standard", "medium")
	f.status("worker:"+branch, ts+500, "pr_open", pr1URL)
}

func TestSweepOpenPRUpgradesToMergedWithTimeToMerge(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/upgrade", 1000)
	f.sweep()
	row := f.only()
	if row["outcome"] != "pr_open" || row["merged"] != false || row["time_to_merge_ms"] != nil {
		t.Errorf("first sweep row = %v", row)
	}

	f.view = `{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:10Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}`
	f.sweep()
	row = f.only()
	if row["outcome"] != "merged" || row["merged"] != true || row["time_to_merge_ms"] != float64(9000) {
		t.Errorf("second sweep row = %v", row)
	}
}

func TestSweepEveryGHCallFailingCountsOnStderrAndStdoutStaysClean(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/gh-fail", 1000)
	f.gh = func(string) (string, bool) { return "", false }
	stderr, code := f.runQuiet() // runQuiet fails the test on any stdout
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if want := "crew: rate: 3 gh call(s) failed; those rows kept their stored t2\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// A live foreign lock must be refused, not stomped, in both windows: the old
// bash release was an unconditional rm -rf, so a trap left armed across the
// unlocked network phase deleted a concurrent sweep's lock dir.
func TestSweepLiveForeignLockRefusesWindow1(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/lock-w1", 1000)
	lockd, pid := f.foreignLock()
	stderr, code := f.runQuiet()
	f.requireRefused(stderr, code, lockd, pid)
	if calls := f.ghCalls(); len(calls) != 0 {
		t.Errorf("window 1 was refused but gh ran: %v", calls)
	}
}

func TestSweepLiveForeignLockTakenDuringTheNetworkPhaseRefusesWindow2(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/lock-w2", 1000)
	pid := livePID(t)
	lockd := filepath.Join(filepath.Dir(f.store), "ratings.lock.d")
	// A concurrent sweep grabs the lock while this one is on the network,
	// between window 1 (released) and window 2 (about to re-acquire).
	inner := f.gh
	f.gh = func(args string) (string, bool) {
		if err := os.MkdirAll(filepath.Dir(lockd), 0o755); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(lockd, 0o755); err != nil && !os.IsExist(err) {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(lockd, "pid"), []byte(pid+"\n"), 0o644); err != nil {
			t.Error(err)
		}
		return inner(args)
	}
	stderr, code := f.runQuiet()
	f.requireRefused(stderr, code, lockd, pid)
}

func (f *sweepFixture) foreignLock() (lockd, pid string) {
	f.t.Helper()
	pid = livePID(f.t)
	lockd = filepath.Join(filepath.Dir(f.store), "ratings.lock.d")
	if err := os.MkdirAll(lockd, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockd, "pid"), []byte(pid+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return lockd, pid
}

func (f *sweepFixture) requireRefused(stderr string, code int, lockd, pid string) {
	f.t.Helper()
	if code != 1 || stderr != "crew: ratings store busy\n" {
		f.t.Errorf("exit %d, stderr %q, want 1 and the busy refusal", code, stderr)
	}
	held, err := os.ReadFile(filepath.Join(lockd, "pid"))
	if err != nil || strings.TrimSpace(string(held)) != pid {
		f.t.Errorf("the foreign lock was disturbed: pid file %q, err %v, want %s", held, err, pid)
	}
	if _, err := os.Stat(f.store); !os.IsNotExist(err) {
		f.t.Error("a refused window still wrote the store")
	}
}

func closedView(closedAt time.Time) string {
	return fmt.Sprintf(`{"state":"CLOSED","closedAt":%q,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}`,
		closedAt.UTC().Format(time.RFC3339))
}

func TestSweepClosedPRInsideThirtyDaysIsRequeried(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/closed-recent", 1000)
	f.view = closedView(time.Unix(1800000000, 0).Add(-10 * 24 * time.Hour))
	f.sweep()
	f.calls = nil
	f.sweep()
	if !f.ghCalled("pr view") {
		t.Errorf("a 10-day-old CLOSED PR was not re-queried: %v", f.ghCalls())
	}
}

func TestSweepClosedPROutsideThirtyDaysIsNotRequeried(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/closed-old", 1000)
	f.view = closedView(time.Unix(1800000000, 0).Add(-40 * 24 * time.Hour))
	f.sweep()
	f.calls = nil
	f.sweep()
	if f.ghCalled("pr view") {
		t.Errorf("a 40-day-old CLOSED PR was re-queried: %v", f.ghCalls())
	}
}

func TestSweepOtherRepoPRURLIsSkippedAndSameRepoViewCarriesTheURL(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/same-repo", 1000)
	f.dispatchAs("feat/other-repo", 2000, "claude", "sonnet", "standard", "medium")
	f.status("worker:feat/other-repo", 2500, "pr_open", "https://github.com/someone-else/other-repo/pull/2")
	f.sweep()
	if !f.ghCalled("pr view " + pr1URL + " ") {
		t.Errorf("same-repo pr view lacks the explicit URL: %v", f.ghCalls())
	}
	for _, c := range f.ghCalls() {
		if strings.Contains(c, "other-repo") {
			t.Errorf("a gh call named the other repo: %s", c)
		}
	}
}

func TestSweepBranchReuseGivesThePRAndItsCIWindowToTheEarlierRun(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/reuse", 1000)
	f.prRun("feat/reuse", 3000)
	f.view = `{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:10Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}`
	f.runs = `{"workflow_runs":[
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:01Z","status":"completed","conclusion":"success"},
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:04Z","status":"completed","conclusion":"failure"}
  ]}`
	f.sweep()
	rows := f.folded()
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["t0_ms"].(float64) < rows[j]["t0_ms"].(float64) })
	first, second := rows[0], rows[1]
	if first["owns_pr"] != true || first["first_ci_green"] != true || first["time_to_pr_ms"] == nil {
		t.Errorf("earlier run = %v, want it to own the PR with CI green", first)
	}
	if second["owns_pr"] != false || second["pr_state"] != nil || second["time_to_pr_ms"] != nil {
		t.Errorf("later run = %v, want no PR and no time to PR", second)
	}
}

func TestSweepReviewRoundsBatchesReviewToFixCyclesExcludingApprovedAndTrailing(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/rounds", 1000)
	f.view = `{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,
    "commits":[{"committedDate":"1970-01-01T00:00:04Z"},{"committedDate":"1970-01-01T00:00:07Z"}],
    "reviews":[
      {"state":"CHANGES_REQUESTED","submittedAt":"1970-01-01T00:00:01Z"},
      {"state":"COMMENTED","submittedAt":"1970-01-01T00:00:02Z"},
      {"state":"CHANGES_REQUESTED","submittedAt":"1970-01-01T00:00:03Z"},
      {"state":"APPROVED","submittedAt":"1970-01-01T00:00:05Z"},
      {"state":"CHANGES_REQUESTED","submittedAt":"1970-01-01T00:00:09Z"}
    ]}`
	f.sweep()
	if got := f.only()["review_rounds"]; got != float64(1) {
		t.Errorf("review_rounds = %v, want 1", got)
	}
}

func TestSweepFirstCIGreen(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs string
		want any
	}{
		{"mixed conclusions on the earliest sha render false", `{"workflow_runs":[
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:02Z","status":"completed","conclusion":"success"},
    {"head_sha":"shaX","created_at":"1970-01-01T00:00:03Z","status":"completed","conclusion":"failure"}
  ]}`, false},
		{"no Actions runs in the window is null, not false", noRuns, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.prRun("feat/ci", 1000)
			f.runs = tc.runs
			f.sweep()
			if got := f.only()["first_ci_green"]; got != tc.want {
				t.Errorf("first_ci_green = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSweepUnresolvedNotesCountsThreadsWithIsResolvedFalse(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/notes", 1000)
	f.threads = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
    {"isResolved":true},{"isResolved":false},{"isResolved":false}
  ]}}}}}`
	f.sweep()
	if got := f.only()["unresolved_notes"]; got != float64(2) {
		t.Errorf("unresolved_notes = %v, want 2", got)
	}
}

func TestSweepUnresolvedNotesStaysNullWhenTheFirstQueryFails(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/notes-fail", 1000)
	f.gh = func(string) (string, bool) { return "", false }
	f.sweep()
	row := f.only()
	if row["unresolved_notes"] != nil || row["last_query_ok"] != false {
		t.Errorf("unresolved_notes/last_query_ok = %v/%v, want null/false", row["unresolved_notes"], row["last_query_ok"])
	}
}

func TestSweepOutcomePrecedence(t *testing.T) {
	f := newSweepFixture(t)
	f.dispatchAs("outcome-merged", 1000, "claude", "sonnet", "standard", "medium")
	f.status("worker:outcome-merged", 1500, "pr_open", pr1URL)
	f.view = viewMerged5

	f.dispatchAs("outcome-running", 2000, "claude", "sonnet", "standard", "medium")
	f.status("worker:outcome-running", 2500, "working")

	f.dispatchAs("outcome-pr_open", 3000, "claude", "sonnet", "standard", "medium")
	f.status("worker:outcome-pr_open", 3500, "pr_open", "https://example.com/pr/9")

	// A no-PR task (review, measurement, experiment) that the worker finished.
	f.dispatchAs("outcome-done", 4000, "claude", "sonnet", "standard", "medium")
	f.status("worker:outcome-done", 4500, "done")

	f.dispatchAs("outcome-failed", 6000, "claude", "sonnet", "standard", "medium")
	f.status("worker:outcome-failed", 6500, "failed")

	f.dispatchAs("outcome-incomplete", 5000, "claude", "sonnet", "standard", "medium")
	f.status("worker:outcome-incomplete", 5500, "exited")

	f.sweep()
	for branch, want := range map[string]string{
		"outcome-merged": "merged", "outcome-running": "running", "outcome-pr_open": "pr_open",
		"outcome-done": "done", "outcome-failed": "failed", "outcome-incomplete": "incomplete",
	} {
		if got := f.row(branch)["outcome"]; got != want {
			t.Errorf("%s outcome = %v, want %s", branch, got, want)
		}
	}
}

func TestSweepTerminalStateIsTheWorkersOwnLastState(t *testing.T) {
	f := newSweepFixture(t)
	for i, state := range []string{"done", "failed", "exited"} {
		branch := "ts-" + state
		f.dispatchAs(branch, int64(1000*(i+1)), "claude", "sonnet", "standard", "medium")
		f.status("worker:"+branch, int64(1000*(i+1)+500), state)
	}
	f.sweep()
	for _, state := range []string{"done", "failed", "exited"} {
		if got := f.row("ts-" + state)["terminal_state"]; got != state {
			t.Errorf("ts-%s terminal_state = %v", state, got)
		}
	}
}

func TestSweepCostIsWeightTimesWallClock(t *testing.T) {
	path := filepath.Join("..", "..", "..", "adapters", "core", "defaults.json")
	shipped, err := os.ReadFile(path)
	if err != nil {
		drift.Missing(t, path, err)
	}
	type want struct {
		class any
		proxy any
	}
	for _, tc := range []struct {
		name                        string
		engine, model, tier, effort string
		want                        want
	}{
		// opus at high keeps the premium class; kimi-k3-high has no burnClasses
		// glob and takes the premium class floor (4).
		{"opus high", "claude", "opus", "deep", "high", want{"premium", float64(2400000)}},
		{"kimi-k3-high", "cursor", "kimi-k3-high", "deep", "high", want{"premium", float64(2400000)}},
		// Opus's proxy follows its effort (#554): the table is keyed by model
		// and effort, so opus@low prices at half of opus@high on one clock.
		{"opus low", "claude", "opus", "standard", "low", want{"standard", float64(1200000)}},
		{"cursor-grok-4.6-high", "cursor", "cursor-grok-4.6-high", "deep", "medium", want{"premium", float64(2400000)}},
		{"cursor-grok-4.6-medium", "cursor", "cursor-grok-4.6-medium", "standard", "medium", want{"standard", float64(1200000)}},
		{"cursor-grok-4.6-low", "cursor", "cursor-grok-4.6-low", "trivial", "medium", want{"cheap", float64(600000)}},
		// A 4.5 slug prices the same as its 4.6 sibling.
		{"cursor-grok-4.5-high", "cursor", "cursor-grok-4.5-high", "deep", "medium", want{"premium", float64(2400000)}},
		// `-fast` doubles the token rate: it lifts a rung one class.
		{"medium-fast", "cursor", "cursor-grok-4.6-medium-fast", "standard", "medium", want{"premium", float64(2400000)}},
		{"low-fast", "cursor", "cursor-grok-4.6-low-fast", "trivial", "medium", want{"standard", float64(1200000)}},
		{"high-fast", "cursor", "cursor-grok-4.6-high-fast", "deep", "medium", want{"premium", float64(4800000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.config = string(shipped)
			f.dispatchAs("cost", 1000, tc.engine, tc.model, tc.tier, tc.effort)
			f.status("worker:cost", 601000, "done")
			f.sweep()
			row := f.only()
			if row["cost_class"] != tc.want.class || row["cost_proxy"] != tc.want.proxy {
				t.Errorf("cost_class/cost_proxy = %v/%v, want %v/%v", row["cost_class"], row["cost_proxy"], tc.want.class, tc.want.proxy)
			}
		})
	}
}

func TestSweepUnclassedModelTakesItsClassFloor(t *testing.T) {
	const settings = `{"burnClasses":[{"match":"*sonnet*","class":"standard","weight":2}],
		"classes":{"claude":{"cheap":["haiku*"],"standard":[{"glob":"sonnet*"},"opus*"],"premium":["fable-*","?pus*"]}}}`
	for _, tc := range []struct {
		name, engine, model string
		class, proxy        any
	}{
		{"premium class pattern", "claude", "fable-6", "premium", float64(4 * 600)},
		{"the most expensive matching class wins", "claude", "opus-9", "premium", float64(4 * 600)},
		{"burnClasses wins over the class floor", "claude", "sonnet-7", "standard", float64(2 * 600)},
		{"no class pattern stays uncosted", "claude", "mystery-1", nil, nil},
		{"another engine's patterns do not apply", "codex", "fable-6", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSweepFixture(t)
			f.config = settings
			f.dispatchAs("floor", 1000, tc.engine, tc.model, "standard", "medium")
			f.status("worker:floor", 1600, "done")
			f.sweep()
			row := f.only()
			if row["cost_class"] != tc.class || row["cost_proxy"] != tc.proxy {
				t.Errorf("cost_class/cost_proxy = %v/%v, want %v/%v", row["cost_class"], row["cost_proxy"], tc.class, tc.proxy)
			}
		})
	}
}

// padStore writes ~300 KiB of filler rows, each a valid stored row with its
// own run_id so it survives the last-wins fold.
func (f *sweepFixture) padStore() int64 {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.store), 0o755); err != nil {
		f.t.Fatal(err)
	}
	var b strings.Builder
	for i := range 600 {
		fmt.Fprintf(&b, `{"repo":"acme/widgets","run_id":"pad-%d","engine":"claude","model":"opus","tier":"standard","outcome":"done","reached_pr":false,"pr_state":null,"swept_at":1,"pad":%q}`+"\n",
			i, strings.Repeat("x", 350))
	}
	if err := os.WriteFile(f.store, []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if b.Len() <= 204800 {
		f.t.Fatalf("pad store is %d bytes, want over 200 KiB", b.Len())
	}
	return int64(b.Len())
}

// argv limit regression (#838): the sweep once passed the folded store to jq
// as one command-line argument, so past Linux's per-argument limit every
// autosweep died before appending anything.
func TestSweepStoreOver200KiBAppendsItsRow(t *testing.T) {
	f := newSweepFixture(t)
	f.dispatchAs("feat/padded", 1000, "claude", "sonnet", "standard", "medium")
	f.status("worker:feat/padded", 1500, "done")
	before := f.padStore()
	f.sweep()
	if got := f.row("feat/padded"); got["outcome"] != "done" {
		t.Errorf("row = %v", got)
	}
	if info, err := os.Stat(f.store); err != nil || info.Size() <= before {
		t.Errorf("store did not grow past %d bytes: %v %v", before, info, err)
	}
}

func TestSweepStoreOver200KiBStillReconcilesAPROwningRun(t *testing.T) {
	f := newSweepFixture(t)
	f.dispatchAs("feat/padded-pr", 1000, "claude", "sonnet", "standard", "medium")
	f.status("worker:feat/padded-pr", 1500, "pr_open", "https://github.com/acme/widgets/pull/7")
	f.padStore()
	f.view = `{"state":"MERGED","closedAt":null,"mergedAt":"1970-01-01T00:00:10Z","mergeCommit":{"oid":"deadbeef"},"commits":[],"reviews":[]}`
	f.sweep()
	// Window 1's snapshot decides whether to call gh at all: an oversized
	// argument there yields zero rows to reconcile and a silently unmeasured sweep.
	views := 0
	for _, c := range f.ghCalls() {
		if strings.HasPrefix(c, "pr view https://github.com/acme/widgets/pull/7") {
			views++
		}
	}
	if views != 1 {
		t.Errorf("pr view ran %d times, want 1: %v", views, f.ghCalls())
	}
	if row := f.row("feat/padded-pr"); row["pr_state"] != "MERGED" || row["merged"] != true {
		t.Errorf("row = %v", row)
	}
}

func TestSweepTaskKindIsCarriedFromTheDispatchEventNullWhenAbsent(t *testing.T) {
	f := newSweepFixture(t)
	f.event(`{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"tk-review","engine":"cursor","model":"cursor-grok-4.6-medium-fast","tier":"standard","effort":"medium","task_kind":"review","title":"t"}`)
	f.status("worker:tk-review", 1500, "done")
	// A run dispatched before dispatch.sh recorded the field.
	f.dispatchAs("tk-legacy", 2000, "claude", "sonnet", "standard", "medium")
	f.status("worker:tk-legacy", 2500, "done")
	f.sweep()
	if got := f.row("tk-review")["task_kind"]; got != "review" {
		t.Errorf("tk-review task_kind = %v", got)
	}
	if got := f.row("tk-legacy")["task_kind"]; got != nil {
		t.Errorf("tk-legacy task_kind = %v, want null", got)
	}
}

// The round trip the single-sweep tests above cannot show: a sweep writes the
// t2 fields a later sweep carries forward, and a MERGED row with known notes
// makes the next sweep skip pr view and threads.
func TestSweepGHFailureAfterAGoodSweepPreservesStoredT2(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/mf", 1000)
	f.threads = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"isResolved":false}]}}}}}`
	f.sweep()
	row := f.only()
	if row["pr_state"] != "OPEN" || row["unresolved_notes"] != float64(1) || row["last_query_ok"] != true {
		t.Fatalf("good sweep row = %v, want OPEN/1/true", row)
	}
	f.gh = func(string) (string, bool) { return "", false }
	f.sweep()
	row = f.only()
	if row["pr_state"] != "OPEN" || row["unresolved_notes"] != float64(1) || row["last_query_ok"] != false {
		t.Errorf("failed sweep row = %v, want OPEN/1/false", row)
	}
}

func TestSweepMergedRowFromASweepSkipsViewAndThreadsNextTime(t *testing.T) {
	f := newSweepFixture(t)
	f.prRun("feat/finality", 1000)
	f.view = viewMerged5
	f.sweep()
	row := f.only()
	if row["pr_state"] != "MERGED" || row["first_ci_green"] != nil || row["unresolved_notes"] != float64(0) {
		t.Fatalf("first sweep row = %v, want MERGED/null/0", row)
	}
	f.calls = nil
	f.sweep()
	if f.ghCalled("pr view") || f.ghCalled("api graphql") {
		t.Errorf("second sweep re-queried a settled MERGED row: %v", f.ghCalls())
	}
	if !f.ghCalled("api --method GET repos/acme/widgets/actions/runs") {
		t.Errorf("second sweep skipped Actions while first_ci_green is null: %v", f.ghCalls())
	}
}
