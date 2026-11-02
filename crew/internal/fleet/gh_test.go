package fleet

import (
	"bytes"
	"context"
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

func prRows(urls ...string) []map[string]any {
	rows := make([]map[string]any, len(urls))
	for i, u := range urls {
		rows[i] = map[string]any{"pr_url": u}
	}
	return rows
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
	rows := prRows(prURL(1), prURL(2), prURL(3), prURL(4), prURL(5), prURL(6))
	fake := ghAnswering(map[string]string{
		prURL(1): run("OPEN", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"SKIPPED"},{"state":"SUCCESS"}]`),
		prURL(2): run("OPEN", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"COMPLETED","conclusion":"FAILURE"},{"status":"IN_PROGRESS"}]`),
		prURL(3): run("OPEN", `[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"IN_PROGRESS","conclusion":""}]`),
		prURL(4): run("OPEN", `[{"state":"PENDING"}]`),
		prURL(5): run("OPEN", `[{"state":"ERROR"}]`),
		prURL(6): `{"state":"MERGED","mergeStateStatus":"UNKNOWN","reviewDecision":"","statusCheckRollup":[]}`,
	})
	enrichPRs(t.Context(), fake, rows, time.Second)

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

// blockingGh never answers until its context ends.
type blockingGh struct{ gh.Client }

func (blockingGh) Combined(ctx context.Context, _ ...string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestEnrichPRsTimeoutIsAPRError(t *testing.T) {
	rows := prRows(prURL(1))
	enrichPRs(t.Context(), blockingGh{}, rows, 20*time.Millisecond)
	msg, _ := rows[0]["pr_error"].(string)
	if !strings.Contains(msg, "timed out") {
		t.Errorf("pr_error = %q, want a timeout", msg)
	}
	if _, has := rows[0]["pr"]; has {
		t.Error("a failed lookup carries no pr")
	}
}

func TestEnrichPRsErrorIsGhsFirstLineWithoutControlCharacters(t *testing.T) {
	rows := prRows(prURL(1), prURL(2), prURL(3))
	fake := &ghtest.Fake{Func: func(args []string) (string, error) {
		switch args[2] {
		case prURL(1):
			return "GraphQL: Could not resolve\x1b[31m to a PullRequest\nsecond line", errors.New("exit status 1")
		case prURL(2):
			return "", errors.New("exit status 4")
		}
		return "not json at all", nil
	}}
	enrichPRs(t.Context(), fake, rows, time.Second)
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
	enrichPRs(t.Context(), fake, prRows(urls...), time.Second)
	if got := peak.Load(); got != ghParallel {
		t.Errorf("peak concurrency = %d, want %d", got, ghParallel)
	}
	if len(fake.Calls()) != 12 {
		t.Errorf("%d calls, want 12", len(fake.Calls()))
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
