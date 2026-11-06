package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
)

func prURL(n int) string { return fmt.Sprintf("https://github.com/acme/alpha/pull/%d", n) }

// prRows are candidates of acme/alpha, plus the rows the caller reads back.
func prRows(urls ...string) ([]prCandidate, []map[string]any) {
	cands := make([]prCandidate, len(urls))
	rows := make([]map[string]any, len(urls))
	for i, u := range urls {
		rows[i] = map[string]any{"pr_url": u}
		cands[i] = prCandidate{row: rows[i], repo: "acme/alpha"}
	}
	return cands, rows
}

// ghAnswering is a fake gh that answers `pr view <url>` from answers.
func ghAnswering(answers map[string]string) *ghtest.Fake {
	return &ghtest.Fake{Func: func(args []string) (string, error) {
		if out, ok := answers[args[2]]; ok {
			return out, nil
		}
		return "", errors.New("unexpected call")
	}}
}

func TestEnrichPRsStatesAndRollupFolding(t *testing.T) {
	run := func(state, rollup string) string {
		return fmt.Sprintf(`{"state":%q,"mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","statusCheckRollup":%s}`, state, rollup)
	}
	cands, rows := prRows(prURL(1), prURL(2), prURL(3), prURL(4), prURL(5), prURL(6))
	fake := ghAnswering(map[string]string{
		prURL(1): run("OPEN", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"SKIPPED"},{"state":"SUCCESS"}]`),
		prURL(2): run("OPEN", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"FAILURE"},{"status":"IN_PROGRESS"}]`),
		prURL(3): run("OPEN", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"IN_PROGRESS","conclusion":""}]`),
		prURL(4): run("OPEN", `[{"state":"PENDING"}]`),
		prURL(5): run("OPEN", `[{"state":"ERROR"}]`),
		prURL(6): `{"state":"MERGED","mergeStateStatus":"UNKNOWN","reviewDecision":"","statusCheckRollup":[]}`,
	})
	enrichPRs(t.Context(), fake, cands, time.Second)

	want := []struct{ state, checks string }{
		{"OPEN", "pass"}, {"OPEN", "fail"}, {"OPEN", "pending"}, {"OPEN", "pending"}, {"OPEN", "fail"}, {"MERGED", "pass"},
	}
	for i, w := range want {
		pr, ok := rows[i]["pr"].(map[string]any)
		if !ok || pr["state"] != w.state || pr["checks"] != w.checks {
			t.Errorf("row %d = %v, want state %s checks %s", i+1, rows[i], w.state, w.checks)
		}
	}
	first := rows[0]["pr"].(map[string]any)
	if first["merge_state"] != "CLEAN" || first["review"] != "APPROVED" {
		t.Errorf("pr = %v", first)
	}
	if rows[5]["pr"].(map[string]any)["review"] != nil {
		t.Errorf("an empty reviewDecision is null: %v", rows[5]["pr"])
	}
	got := fake.Calls()[0]
	if !strings.HasPrefix(got, "pr view https://") || !strings.HasSuffix(got, " --json state,mergeStateStatus,reviewDecision,statusCheckRollup") {
		t.Errorf("gh argv = %q", got)
	}
}

func TestCheckState(t *testing.T) {
	for _, tc := range []struct{ name, rollup, want string }{
		{"empty", `[]`, "none"},
		{"all success", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"state":"SUCCESS"}]`, "pass"},
		{"neutral and skipped", `[{"status":"COMPLETED","conclusion":"NEUTRAL"},{"status":"COMPLETED","conclusion":"SKIPPED"}]`, "pass"},
		{"in progress", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"IN_PROGRESS"}]`, "pending"},
		{"queued", `[{"status":"QUEUED"}]`, "pending"},
		{"failing conclusion", `[{"status":"IN_PROGRESS"},{"status":"COMPLETED","conclusion":"FAILURE"}]`, "fail"},
		{"status success", `[{"state":"SUCCESS"}]`, "pass"},
		{"status pending", `[{"state":"PENDING"}]`, "pending"},
		{"status error", `[{"state":"ERROR"}]`, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v prView
			if err := json.Unmarshal([]byte(`{"statusCheckRollup":`+tc.rollup+`}`), &v); err != nil {
				t.Fatal(err)
			}
			if got := checkState(v); got != tc.want {
				t.Errorf("checkState = %q, want %q", got, tc.want)
			}
			wantChecks := tc.want
			if wantChecks == "none" {
				wantChecks = "pass"
			}
			if got := checks(v); got != wantChecks {
				t.Errorf("checks = %q, want %q", got, wantChecks)
			}
		})
	}
}

func TestPRChecks(t *testing.T) {
	fake := ghAnswering(map[string]string{
		prURL(1): `{"state":"OPEN","reviewDecision":"APPROVED","statusCheckRollup":[{"status":"IN_PROGRESS"}]}`,
		prURL(2): `{"state":"OPEN","reviewDecision":"","statusCheckRollup":[]}`,
	})
	got, err := PRChecks(t.Context(), fake, prURL(1), "acme/alpha", time.Second)
	if err != nil || got != (PRChecksInfo{CI: "pending", Review: "APPROVED"}) {
		t.Errorf("PRChecks = %+v, %v", got, err)
	}
	got, err = PRChecks(t.Context(), fake, prURL(2), "acme/alpha", time.Second)
	if err != nil || got != (PRChecksInfo{CI: "none"}) {
		t.Errorf("PRChecks without a runner = %+v, %v", got, err)
	}

	calls := len(fake.Calls())
	if _, err := PRChecks(t.Context(), fake, "https://github.com/other/repo/pull/7", "acme/alpha", time.Second); err == nil || err.Error() != "pr_url is not a PR of this repo" {
		t.Errorf("another repo's PR: err = %v", err)
	}
	if len(fake.Calls()) != calls {
		t.Error("another repo's PR reached gh")
	}

	if _, err := PRChecks(t.Context(), failingGh{}, prURL(1), "acme/alpha", time.Second); err == nil || err.Error() != "GraphQL: Could not resolve to a PullRequest" {
		t.Errorf("gh failure: err = %v", err)
	}
}

// blockingGh never answers until its context ends.
type blockingGh struct{ gh.Client }

func (blockingGh) Run(ctx context.Context, _ ...string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestEnrichPRsTimeoutIsAPRError(t *testing.T) {
	cands, rows := prRows(prURL(1))
	enrichPRs(t.Context(), blockingGh{}, cands, 20*time.Millisecond)
	msg, _ := rows[0]["pr_error"].(string)
	if !strings.Contains(msg, "timed out") {
		t.Errorf("pr_error = %q, want a timeout", msg)
	}
	if _, has := rows[0]["pr"]; has {
		t.Error("a failed lookup carries no pr")
	}
}

func TestEnrichPRsErrorIsGhsFirstLineWithoutControlCharacters(t *testing.T) {
	cands, rows := prRows(prURL(1), prURL(2), prURL(3))
	fake := &ghtest.Fake{Func: func(args []string) (string, error) {
		switch args[2] {
		case prURL(1):
			return "GraphQL: Could not resolve\x1b[31m to a PullRequest\nsecond line", errors.New("exit status 1")
		case prURL(2):
			return "", errors.New("exit status 4")
		}
		return "not json at all", nil
	}}
	enrichPRs(t.Context(), fake, cands, time.Second)
	if got := rows[0]["pr_error"]; got != "GraphQL: Could not resolve[31m to a PullRequest" {
		t.Errorf("pr_error = %q", got)
	}
	if got := rows[1]["pr_error"]; got != "exit status 4" {
		t.Errorf("pr_error = %q", got)
	}
	if got, _ := rows[2]["pr_error"].(string); !strings.Contains(got, "not a PR") {
		t.Errorf("pr_error = %q", got)
	}
}

func TestEnrichPRsRunsFourAtATime(t *testing.T) {
	var running, peak atomic.Int32
	fake := &ghtest.Fake{Func: func([]string) (string, error) {
		n := running.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(15 * time.Millisecond)
		running.Add(-1)
		return `{"state":"OPEN","statusCheckRollup":[]}`, nil
	}}
	var urls []string
	for i := range 12 {
		urls = append(urls, prURL(i))
	}
	cands, _ := prRows(urls...)
	enrichPRs(t.Context(), fake, cands, time.Second)
	if got := peak.Load(); got != ghParallel {
		t.Errorf("peak concurrency = %d, want %d", got, ghParallel)
	}
	if len(fake.Calls()) != 12 {
		t.Errorf("%d calls, want 12", len(fake.Calls()))
	}
}

// A pr_url is worker-written text: only a PR of the repo whose bus it is on
// reaches gh, never another repo's, a bare number, or something gh reads as a
// flag.
func TestEnrichPRsOnlyAsksForThisReposPRs(t *testing.T) {
	cands, rows := prRows(
		"https://github.com/other/repo/pull/7",
		"123",
		"--jq=.",
		"https://github.com/acme/alpha/pull/8/files",
		"https://github.com/ACME/Alpha/pull/9",
	)
	fake := &ghtest.Fake{Func: func([]string) (string, error) { return `{"state":"OPEN","statusCheckRollup":[]}`, nil }}
	enrichPRs(t.Context(), fake, cands, time.Second)
	for i := range 4 {
		if rows[i]["pr_error"] != "pr_url is not a PR of this repo" || rows[i]["pr"] != nil {
			t.Errorf("row %d = %v, want a pr_error", i, rows[i])
		}
	}
	if rows[4]["pr"] == nil {
		t.Errorf("a PR of this repo, whatever its case, is asked for: %v", rows[4])
	}
	if calls := fake.Calls(); len(calls) != 1 || !strings.Contains(calls[0], "/ACME/Alpha/pull/9") {
		t.Errorf("gh calls = %v, want only the one valid PR", calls)
	}
}

// A failed gh prints nothing on stdout: its reason is only in the merged read.
type failingGh struct{ gh.Client }

func (failingGh) Run(context.Context, ...string) (string, error) {
	return "", errors.New("exit status 1")
}

func (failingGh) Combined(context.Context, ...string) (string, error) {
	return "GraphQL: Could not resolve to a PullRequest\nmore", errors.New("exit status 1")
}

func TestEnrichPRsErrorReasonComesFromStderr(t *testing.T) {
	cands, rows := prRows(prURL(1))
	enrichPRs(t.Context(), failingGh{}, cands, time.Second)
	if got := rows[0]["pr_error"]; got != "GraphQL: Could not resolve to a PullRequest" {
		t.Errorf("pr_error = %q, want gh's first stderr line", got)
	}
}

// gh's stderr on a successful call is no part of its JSON.
type noisyGh struct{ gh.Client }

func (noisyGh) Run(context.Context, ...string) (string, error) {
	return `{"state":"OPEN","statusCheckRollup":[]}`, nil
}

func (noisyGh) Combined(context.Context, ...string) (string, error) {
	return "warning: a new release is available\n" + `{"state":"OPEN","statusCheckRollup":[]}`, nil
}

func TestEnrichPRsReadsStdoutOnly(t *testing.T) {
	cands, rows := prRows(prURL(1))
	enrichPRs(t.Context(), noisyGh{}, cands, time.Second)
	if rows[0]["pr"] == nil || rows[0]["pr_error"] != nil {
		t.Errorf("row = %v, want the PR despite stderr noise", rows[0])
	}
}

func TestStatusGhNeverFailsTheCommand(t *testing.T) {
	url := func(n int) string { return fmt.Sprintf(`,"pr_url":%q`, prURL(n)) }
	f := newStatusFix(t,
		wstatus(300, "c1", "feat/a", "pr_open", url(1)),
		wstatus(200, "c1", "feat/b", "done", url(2)),
		wstatus(100, "c1", "feat/c", "done", url(3)),
	)
	f.opts.Gh = ghAnswering(map[string]string{
		prURL(1): `{"state":"OPEN","statusCheckRollup":[]}`,
		prURL(2): `{"state":"MERGED","statusCheckRollup":[]}`,
	})
	var out, errb bytes.Buffer
	if code := Status(t.Context(), StatusRequest{GH: true}, &out, &errb, f.opts); code != 0 {
		t.Fatalf("exit %d with a failing lookup, stderr:\n%s", code, errb.String())
	}
	c := f.collect(StatusRequest{GH: true}).crew(t, "c1")
	if c.worker(t, "feat/a")["pr"] == nil || c.worker(t, "feat/c")["pr_error"] == nil {
		t.Errorf("rows = %v", c.Workers)
	}
	// Awaiting merge is GitHub's word: the merged and the unknown do not count.
	if !strings.Contains(out.String(), "  1        0") {
		t.Errorf("only the OPEN PR awaits merge:\n%s", out.String())
	}

	f.opts.Gh = nil
	if code := Status(t.Context(), StatusRequest{GH: true, JSON: true}, io.Discard, io.Discard, f.opts); code != 0 {
		t.Errorf("exit %d with no gh", code)
	}
}
