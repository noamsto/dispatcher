package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
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
	err  error // set when no lookup can be right, so gh is not asked
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
			if cand.err != nil {
				cand.row["pr_error"] = cand.err.Error()
				return
			}
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

// PRChecksInfo is what the roster diagram reads of a PR: its CI state
// (pass, pending, fail, none) and GitHub's review decision ("" when none).
type PRChecksInfo struct{ CI, Review string }

// PRChecks is one `gh pr view` folded to PRChecksInfo, with viewPR's guard on
// which PRs reach gh.
func PRChecks(ctx context.Context, c gh.Client, url, repo string, timeout time.Duration) (PRChecksInfo, error) {
	v, err := fetchPRView(ctx, c, url, repo, timeout)
	if err != nil {
		return PRChecksInfo{}, err
	}
	return PRChecksInfo{CI: checkState(v), Review: v.ReviewDecision}, nil
}

// viewPR is one `gh pr view` as a status row's `pr` map.
func viewPR(ctx context.Context, c gh.Client, url, repo string, timeout time.Duration) (map[string]any, error) {
	v, err := fetchPRView(ctx, c, url, repo, timeout)
	if err != nil {
		return nil, err
	}
	var review any
	if v.ReviewDecision != "" {
		review = v.ReviewDecision
	}
	return map[string]any{"state": v.State, "merge_state": v.MergeStateStatus, "review": review, "checks": checks(v)}, nil
}

// fetchPRView is one `gh pr view`. A pr_url is worker-written text, so it only
// reaches gh as a PR of the repo whose bus it is on: never another repo's, and
// never a value gh would read as a flag or resolve against the cwd.
func fetchPRView(ctx context.Context, c gh.Client, url, repo string, timeout time.Duration) (prView, error) {
	if c == nil {
		return prView{}, errors.New("gh is not available")
	}
	if m := prURLRe.FindStringSubmatch(url); m == nil || !strings.EqualFold(m[1]+"/"+m[2], repo) {
		return prView{}, errors.New("pr_url is not a PR of this repo")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := c.Run(ctx, "pr", "view", url, "--json", prFields)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return prView{}, fmt.Errorf("gh timed out after %s", timeout)
		}
		// Run drops stderr, which is where gh says why: ask again for the line.
		// A retry that succeeds answers with the PR's JSON, which is no reason.
		msg, cerr := c.Combined(ctx, "pr", "view", url, "--json", prFields)
		if line := firstLine(msg); line != "" && (cerr != nil || !json.Valid([]byte(line))) {
			return prView{}, errors.New(line)
		}
		return prView{}, errors.New(firstLine(err.Error()))
	}
	var v prView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return prView{}, fmt.Errorf("gh output is not a PR: %s", firstLine(out))
	}
	return v, nil
}

// onGitHub is whether origin names a github.com remote, in its https, ssh or
// scp-like form. An empty origin is not known to be elsewhere.
func onGitHub(origin string) bool {
	origin = firstLine(origin)
	if origin == "" {
		return true
	}
	var host string
	if u, err := neturl.Parse(origin); err == nil && u.Host != "" {
		host = u.Hostname()
	} else if h, _, ok := strings.Cut(origin, ":"); ok {
		host = h
		if _, after, found := strings.Cut(h, "@"); found {
			host = after
		}
	} else {
		return false
	}
	host = strings.ToLower(host)
	// A dotless host is an ssh config alias, which may well be GitHub.
	return host == "github.com" || strings.HasSuffix(host, ".github.com") || !strings.Contains(host, ".")
}

// checks is checkState for the status row: no checks at all is `pass`, as no
// gate stands in the way.
func checks(v prView) string {
	if s := checkState(v); s != "none" {
		return s
	}
	return "pass"
}

// checkState folds the rollup to the worst of its entries: any failure is
// `fail`, else any unfinished check is `pending`. No entries at all is `none`.
func checkState(v prView) string {
	if len(v.StatusCheckRollup) == 0 {
		return "none"
	}
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
