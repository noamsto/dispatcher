package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/gh"
)

const (
	ghTimeout  = 20 * time.Second
	ghParallel = 4
	prFields   = "state,mergeStateStatus,reviewDecision,statusCheckRollup"
)

type prView struct {
	State             string `json:"state"`
	MergeStateStatus  string `json:"mergeStateStatus"`
	ReviewDecision    string `json:"reviewDecision"`
	StatusCheckRollup []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
}

var prURLRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/[0-9]+$`)

// prCandidate is a worker row with a pr_url, and the repo whose bus it is on.
type prCandidate struct {
	row  map[string]any
	repo string
}

// enrichPRs asks GitHub about each PR candidate row, ghParallel at a time, and
// adds `pr` or, when the call fails, `pr_error`. It never fails the command,
// and never caches: GitHub is the durable source.
func enrichPRs(ctx context.Context, c gh.Client, cands []prCandidate, timeout time.Duration) {
	sem := make(chan struct{}, ghParallel)
	var wg sync.WaitGroup
	for _, cand := range cands {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			url, _ := cand.row["pr_url"].(string)
			pr, err := viewPR(ctx, c, url, cand.repo, timeout)
			if err != nil {
				cand.row["pr_error"] = err.Error()
				return
			}
			cand.row["pr"] = pr
		})
	}
	wg.Wait()
}

// viewPR is one `gh pr view`. A pr_url is worker-written text, so it only
// reaches gh as a PR of the repo whose bus it is on: never another repo's, and
// never a value gh would read as a flag or resolve against the cwd.
func viewPR(ctx context.Context, c gh.Client, url, repo string, timeout time.Duration) (map[string]any, error) {
	if c == nil {
		return nil, errors.New("gh is not available")
	}
	if m := prURLRe.FindStringSubmatch(url); m == nil || !strings.EqualFold(m[1]+"/"+m[2], repo) {
		return nil, errors.New("pr_url is not a PR of this repo")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := c.Run(ctx, "pr", "view", url, "--json", prFields)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("gh timed out after %s", timeout)
		}
		// Run drops stderr, which is where gh says why: ask again for the line.
		msg, _ := c.Combined(ctx, "pr", "view", url, "--json", prFields)
		if line := firstLine(msg); line != "" {
			return nil, errors.New(line)
		}
		return nil, errors.New(firstLine(err.Error()))
	}
	var v prView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("gh output is not a PR: %s", firstLine(out))
	}
	var review any
	if v.ReviewDecision != "" {
		review = v.ReviewDecision
	}
	return map[string]any{"state": v.State, "merge_state": v.MergeStateStatus, "review": review, "checks": checks(v)}, nil
}

// checks folds the rollup to the worst of its entries: any failure is `fail`,
// else any unfinished check is `pending`. No checks at all is `pass`, as no
// gate stands in the way.
func checks(v prView) string {
	result := "pass"
	for _, e := range v.StatusCheckRollup {
		// A check run carries status and conclusion; a commit status, a state.
		switch {
		case e.State != "":
			switch e.State {
			case "SUCCESS":
			case "PENDING", "EXPECTED":
				result = "pending"
			default:
				return "fail"
			}
		case e.Status != "COMPLETED":
			result = "pending"
		default:
			switch e.Conclusion {
			case "SUCCESS", "NEUTRAL", "SKIPPED":
			default:
				return "fail"
			}
		}
	}
	return result
}

// prOpen is whether a worker's PR awaits merge: GitHub's word under --gh, the
// candidate mark offline. A failed lookup is not a yes.
func prOpen(row map[string]any) bool {
	if pr, ok := row["pr"].(map[string]any); ok {
		return pr["state"] == "OPEN"
	}
	if _, failed := row["pr_error"]; failed {
		return false
	}
	return row["pr_candidate"] == true
}

// firstLine is gh's first output line with control characters removed: it is
// printed into a document an agent reads.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
}
