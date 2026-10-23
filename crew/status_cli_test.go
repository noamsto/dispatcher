package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repo-keyed subcommand carries its full argv into the arm, but the repo
// check runs first: from outside a git repo `status worker working` refuses
// with the one line, exit 1, stdout empty.
func TestStatusRepoKeyedRefusalOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return dir, nil }}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"status", "worker", "working"}, &stdout, &stderr, e)
	if code != exitFailure || stdout.Len() != 0 || stderr.String() != "crew: not in a git repo\n" {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

// With CREW_ID unset the status arm resolves the crew through bus.CrewID, so
// the row it appends carries the WORKER_TASK.md crew_id.
func TestStatusRowCrewIDFromTaskDoc(t *testing.T) {
	for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_CLOCK"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	repo, logPath := repoWithLog(t, nil, 0) // CREW_ID unset
	if err := os.WriteFile(filepath.Join(repo, "WORKER_TASK.md"), []byte("crew_id: c-from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := env{getwd: func() (string, error) { return repo, nil }}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"status", "worker", "working"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d rows, want 1:\n%s", len(lines), data)
	}
	var row struct {
		CrewID string `json:"crew_id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("row does not parse: %v\n%s", err, lines[0])
	}
	if row.CrewID != "c-from-file" {
		t.Errorf("crew_id = %q, want c-from-file", row.CrewID)
	}
}

// TestStatusPiReviewerVerdictGateCLI drives the pi verdict path through the
// binary: the reviewer pane's verdict and the deslop seam are posted by
// `crew msg`, then `crew status <worker> pr_open "" <url>` decides. An accepting
// verdict writes the row; a reject, and anything that cancels an accept, exits 1
// with the review-seam refusal and writes none. The refusal is matched by
// substring here; the status package's gate table pins it whole.
func TestStatusPiReviewerVerdictGateCLI(t *testing.T) {
	const (
		piDoc     = "tier: standard\nkind: implement\nengine: pi\ncrew_id: c1\n"
		claudeDoc = "tier: standard\nkind: implement\nengine: claude\ncrew_id: c1\n"
		worker    = "worker:feat/x#s1-1"
		reviewer  = "role:feat/x:reviewer"
	)
	type post struct{ from, to, body string }
	verdict := func(v string) post {
		return post{reviewer, worker, `{"role":"reviewer","seam":"review","verdict":"` + v + `"}`}
	}
	deslop := post{worker, "review:c1", `{"seam":"deslop"}`}
	leadSeam := post{worker, "review:c1", `{"seam":"review","review_mode":"full"}`}
	reRequest := post{worker, reviewer, `{"seam":"review","artifact":"/a/review.diff","question":"Review this diff."}`}

	for _, tc := range []struct {
		name string
		doc  string
		post []post
		code int
	}{
		{"an accept alone counts as a review seam", piDoc, []post{verdict("accept"), deslop}, 0},
		{"a reject alone is refused", piDoc, []post{verdict("reject"), deslop}, 1},
		{"a reject cleared by a later accept counts", piDoc, []post{verdict("reject"), verdict("accept"), deslop}, 0},
		{"a reject is not cleared by the lead's own review seam", piDoc, []post{verdict("reject"), leadSeam, deslop}, 1},
		{"an accept voided by a re-request is refused", piDoc, []post{verdict("accept"), reRequest, deslop}, 1},
		{"off pi a pane verdict is not a review seam", claudeDoc, []post{verdict("accept"), deslop}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_CLOCK", "TMUX_PANE"} {
				t.Setenv(k, "")
				if err := os.Unsetenv(k); err != nil {
					t.Fatal(err)
				}
			}
			repo, logPath := repoWithLog(t, nil, 0)
			if err := os.WriteFile(filepath.Join(repo, "WORKER_TASK.md"), []byte(tc.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			e := env{getwd: func() (string, error) { return repo, nil }}
			ctx := context.Background()

			var stdout, stderr bytes.Buffer
			for _, p := range tc.post {
				stdout.Reset()
				stderr.Reset()
				if code := run(ctx, []string{"msg", p.from, p.to, p.body}, &stdout, &stderr, e); code != 0 {
					t.Fatalf("msg %s -> %s: %d %q", p.from, p.to, code, stderr.String())
				}
			}
			stdout.Reset()
			stderr.Reset()
			code := run(ctx, []string{"status", worker, "pr_open", "", "https://example.com/pr/1"}, &stdout, &stderr, e)
			if code != tc.code || stdout.Len() != 0 {
				t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			rows := 0
			for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
				if strings.Contains(line, `"kind":"status"`) {
					rows++
				}
			}
			if tc.code == 0 {
				if stderr.Len() != 0 {
					t.Errorf("refusal on an allowed post: %q", stderr.String())
				}
				if rows != 1 || !strings.Contains(string(data), `"state":"pr_open"`) {
					t.Errorf("the pr_open row was not written (%d status rows):\n%s", rows, data)
				}
				return
			}
			if !strings.Contains(stderr.String(), "no review seam") {
				t.Errorf("stderr %q does not name the review seam", stderr.String())
			}
			if rows != 0 {
				t.Errorf("the refusal wrote a status row:\n%s", data)
			}
		})
	}
}

// A pr_open with no detail argument at all: with an acceptance list the arm
// refuses the missing ledger and writes no row, and with no list the empty
// detail is the correct post.
func TestStatusNoDetailArgLedger(t *testing.T) {
	empty := ""
	for _, tc := range []struct {
		name string
		doc  string
		code int
	}{
		{"an acceptance list refuses the missing ledger", "tier: trivial\nkind: implement\ncrew_id: c1\n\n## Acceptance\n- AC1 do it\n", 1},
		{"no list makes the empty detail correct", "tier: trivial\nkind: implement\ncrew_id: c1\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_CLOCK", "TMUX_PANE"} {
				t.Setenv(k, "")
				if err := os.Unsetenv(k); err != nil {
					t.Fatal(err)
				}
			}
			repo, logPath := repoWithLog(t, &empty, 0o644)
			if err := os.WriteFile(filepath.Join(repo, "WORKER_TASK.md"), []byte(tc.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			e := env{getwd: func() (string, error) { return repo, nil }}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"status", "worker:feat/x#s1-1", "pr_open"}, &stdout, &stderr, e)
			if code != tc.code || stdout.Len() != 0 {
				t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.code == 1 {
				if !strings.Contains(stderr.String(), "acceptance list") {
					t.Errorf("stderr %q does not name the acceptance list", stderr.String())
				}
				if len(data) != 0 {
					t.Errorf("the refusal wrote rows: %s", data)
				}
			} else if lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); len(lines) != 1 ||
				!strings.Contains(lines[0], `"state":"pr_open"`) {
				t.Errorf("row not written: %s", data)
			}
		})
	}
}
