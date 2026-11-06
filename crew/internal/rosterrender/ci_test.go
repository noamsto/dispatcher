package rosterrender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const ciRepo = "o/r"

func ciURL(n int) string { return fmt.Sprintf("https://github.com/o/r/pull/%d", n) }

// ciStub answers `gh pr view <url>` and records the urls asked.
type ciStub struct {
	mu    sync.Mutex
	urls  []string
	fail  map[string]bool
	state string // the statusCheckRollup state of every answer
	rev   string
}

func (s *ciStub) Run(_ context.Context, args ...string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	url := args[2]
	s.urls = append(s.urls, url)
	if s.fail[url] {
		return "", errors.New("exit status 1")
	}
	state := s.state
	if state == "" {
		state = "SUCCESS"
	}
	return fmt.Sprintf(`{"state":"OPEN","reviewDecision":%q,"statusCheckRollup":[{"state":%q}]}`, s.rev, state), nil
}

func (s *ciStub) Combined(context.Context, ...string) (string, error) {
	return "gh down\nmore", nil
}

func (s *ciStub) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.urls)
}

func wantsOf(urls ...string) []ciWant {
	out := make([]ciWant, len(urls))
	for i, u := range urls {
		out[i] = ciWant{URL: u, Repo: ciRepo}
	}
	return out
}

func readCI(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, fileCI))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRefreshCITTL(t *testing.T) {
	dir := t.TempDir()
	s := &ciStub{rev: "REVIEW_REQUIRED"}
	t0 := time.Unix(1_000_000, 0)
	wants := wantsOf(ciURL(1), ciURL(2), ciURL(1))

	got := refreshCI(t.Context(), s, dir, wants, t0, &bytes.Buffer{})
	if n := len(s.calls()); n != 2 {
		t.Fatalf("first refresh made %d calls, want 2 (duplicate looked up once): %v", n, s.calls())
	}
	want := ciEntry{CI: "pass", Review: "REVIEW_REQUIRED", TS: t0.Unix()}
	if len(got) != 2 || got[ciURL(1)] != want || got[ciURL(2)] != want {
		t.Fatalf("entries = %v", got)
	}
	if !maps.Equal(loadCI(dir), got) {
		t.Fatalf("file holds %v, want %v", loadCI(dir), got)
	}

	refreshCI(t.Context(), s, dir, wants, t0.Add(30*time.Second), &bytes.Buffer{})
	if n := len(s.calls()); n != 2 {
		t.Fatalf("refresh inside the TTL made calls: %d", n)
	}

	got = refreshCI(t.Context(), s, dir, wants, t0.Add(60*time.Second), &bytes.Buffer{})
	if n := len(s.calls()); n != 4 {
		t.Fatalf("refresh at the TTL made %d calls total, want 4", n)
	}
	if got[ciURL(1)].TS != t0.Add(60*time.Second).Unix() {
		t.Fatalf("entry not renewed: %v", got[ciURL(1)])
	}
}

func TestRefreshCIFailureKeepsOldEntry(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	s := &ciStub{}
	refreshCI(t.Context(), s, dir, wantsOf(ciURL(1)), t0, &bytes.Buffer{})

	s.fail = map[string]bool{ciURL(1): true, ciURL(2): true}
	var stderr bytes.Buffer
	got := refreshCI(t.Context(), s, dir, wantsOf(ciURL(1), ciURL(2)), t0.Add(2*time.Minute), &stderr)

	tried := t0.Add(2 * time.Minute).Unix()
	if got[ciURL(1)] != (ciEntry{CI: "pass", TS: t0.Unix(), Tried: tried}) {
		t.Fatalf("old entry lost: %v", got)
	}
	if got[ciURL(2)] != (ciEntry{Tried: tried}) {
		t.Fatalf("a failed first lookup should leave only its attempt: %v", got)
	}
	before := len(s.calls())
	refreshCI(t.Context(), s, dir, wantsOf(ciURL(1), ciURL(2)), t0.Add(2*time.Minute+30*time.Second), &bytes.Buffer{})
	if n := len(s.calls()); n != before {
		t.Fatalf("a failed lookup was retried inside the TTL: %d new calls", n-before)
	}
	refreshCI(t.Context(), s, dir, wantsOf(ciURL(1), ciURL(2)), t0.Add(3*time.Minute), &bytes.Buffer{})
	if n := len(s.calls()); n != before+2 {
		t.Fatalf("failed lookups not retried after the TTL: %d new calls", n-before)
	}
	for _, u := range []string{ciURL(1), ciURL(2)} {
		if line := "crew: roster-render: ci " + u + ": gh down\n"; !strings.Contains(stderr.String(), line) {
			t.Fatalf("stderr %q lacks %q", stderr.String(), line)
		}
	}
	if len(loadCI(dir)) != 2 {
		t.Fatalf("file = %v", loadCI(dir))
	}
}

func TestRefreshCIPrunesAndSkipsUnchangedWrite(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Unix(1_000_000, 0)
	s := &ciStub{}
	refreshCI(t.Context(), s, dir, wantsOf(ciURL(1), ciURL(2)), t0, &bytes.Buffer{})

	got := refreshCI(t.Context(), s, dir, wantsOf(ciURL(1)), t0.Add(time.Second), &bytes.Buffer{})
	if len(got) != 1 || len(loadCI(dir)) != 1 {
		t.Fatalf("not pruned: returned %v, file %v", got, loadCI(dir))
	}

	path := filepath.Join(dir, fileCI)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	refreshCI(t.Context(), s, dir, wantsOf(ciURL(1)), t0.Add(2*time.Second), &bytes.Buffer{})
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("an unchanged cache was rewritten")
	}

	got = refreshCI(t.Context(), s, dir, nil, t0.Add(3*time.Second), &bytes.Buffer{})
	if len(got) != 0 || len(loadCI(dir)) != 0 {
		t.Fatalf("empty wants did not prune: %v", got)
	}
}

func TestRefreshCINoClientOnlyReads(t *testing.T) {
	dir := t.TempDir()
	if got := refreshCI(t.Context(), nil, dir, wantsOf(ciURL(1)), time.Unix(1, 0), &bytes.Buffer{}); len(got) != 0 {
		t.Fatalf("entries from nothing: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, fileCI)); !os.IsNotExist(err) {
		t.Fatalf("a file was created without gh: %v", err)
	}

	data := `{"v":1,"prs":{"` + ciURL(1) + `":{"ci":"fail","ts":5},"` + ciURL(9) + `":{"ci":"pass","ts":5}}}`
	if err := os.WriteFile(filepath.Join(dir, fileCI), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got := refreshCI(t.Context(), nil, dir, wantsOf(ciURL(1)), time.Unix(1_000_000, 0), &bytes.Buffer{})
	if len(got) != 2 || got[ciURL(1)].CI != "fail" {
		t.Fatalf("entries = %v, want the file as is", got)
	}
	if readCI(t, dir) != data {
		t.Fatal("a read-only refresh rewrote the file")
	}
}

func TestLoadCIUntrusted(t *testing.T) {
	long := strings.Repeat("k", 300)
	many := map[string]ciEntry{}
	for i := range 1000 {
		many[ciURL(i)] = ciEntry{CI: "pass", TS: 1}
	}
	manyJSON, err := json.Marshal(ciFile{V: ciVer, PRs: many})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body string
		want       int
	}{
		{"corrupt", `{"v":1,"prs":{`, 0},
		{"not json", "hello", 0},
		{"wrong version", `{"v":2,"prs":{"a":{"ci":"pass","ts":1}}}`, 0},
		{"no version", `{"prs":{"a":{"ci":"pass","ts":1}}}`, 0},
		{"bad ci", `{"v":1,"prs":{"a":{"ci":"x","ts":1},"b":{"ci":"pass","ts":1}}}`, 1},
		{"bad review", `{"v":1,"prs":{"a":{"ci":"pass","review":"MERGE ME","ts":1},"b":{"ci":"none","review":"APPROVED","ts":1}}}`, 1},
		{"long key", `{"v":1,"prs":{"` + long + `":{"ci":"pass","ts":1},"b":{"ci":"pass","ts":1}}}`, 1},
		{"many", string(manyJSON), maxPlanets},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, fileCI), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := loadCI(dir); len(got) != tc.want {
				t.Fatalf("kept %d entries, want %d: %v", len(got), tc.want, got)
			}
		})
	}
	if got := loadCI(t.TempDir()); len(got) != 0 {
		t.Fatalf("missing file loaded %v", got)
	}
}

func TestRefreshCIRefusesForeignPR(t *testing.T) {
	dir := t.TempDir()
	s := &ciStub{}
	foreign := "https://github.com/other/repo/pull/3"
	var stderr bytes.Buffer
	got := refreshCI(t.Context(), s, dir, []ciWant{{URL: foreign, Repo: ciRepo}, {URL: "--web", Repo: ciRepo}}, time.Unix(1_000_000, 0), &stderr)
	for u, e := range got {
		if e.CI != "" {
			t.Fatalf("a refused url %q got a state: %v", u, e)
		}
	}
	if len(s.calls()) != 0 {
		t.Fatalf("gh reached for a foreign PR: %v", s.calls())
	}
	if !strings.Contains(stderr.String(), "not a PR of this repo") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPROf(t *testing.T) {
	for url, want := range map[string]bool{
		"https://github.com/o/r/pull/1":        true,
		"https://github.com/O/R/pull/1":        true,
		"https://github.com/o/r2/pull/1":       false,
		"https://github.com/other/r/pull/1":    false,
		"https://github.com/o/r/issues/1":      false,
		"--web":                                false,
		"":                                     false,
		"http://github.com/o/r/pull/1":         false,
		"https://evil.test/https://github.com": false,

		"https://github.com/o/r/pull/1234567890":                  true,
		"https://github.com/o/r/pull/12345678901":                 false,
		"https://github.com/o/r/pull/" + strings.Repeat("9", 400): false,
		"https://github.com/o/r/pull/12x":                         false,
		"https://github.com/o/r/pull/x":                           false,
		"https://github.com/o/r/pull/1\x1b[31m":                   false,
		"https://github.com/o/r/pull/1\n":                         false,
		"https://github.com/o/r/pull/":                            false,
		"https://github.com/o/r/pull/1/":                          false,
		"https://github.com/o/r/pull/1/files":                     false,
		"https://github.com/o/r/pull/1?x=1":                       false,
		"https://github.com/o/r/pull/1#c":                         false,
		"https://github.com/o/r/pull/-1":                          false,
	} {
		if got := prOf(url, ciRepo); got != want {
			t.Errorf("prOf(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestApplyCI(t *testing.T) {
	u := ciURL(1)
	now := time.Unix(1_000_000, 0)
	ts := now.Unix()
	entries := map[string]ciEntry{
		u:          {CI: "pass", Review: "REVIEW_REQUIRED", TS: ts},
		ciURL(2):   {CI: "fail", Review: "REVIEW_REQUIRED", TS: ts},
		ciURL(3):   {CI: "pass", Review: "APPROVED", TS: ts},
		ciURL(4):   {CI: "pending", TS: ts},
		"untouchd": {CI: "pass", Review: "REVIEW_REQUIRED", TS: ts},
	}
	ps := []planet{
		{class: classPROpen, prURL: u},
		{class: classPROpen, prURL: ciURL(2)},
		{class: classPROpen, prURL: ciURL(3)},
		{class: classPROpen, prURL: ciURL(4), waiting: true},
		{class: classPROpen, prURL: ciURL(5)},
		{class: classPROpen},
		{class: classWorking, prURL: u},
		{class: classPROpen, prURL: ciURL(3), waiting: true},
	}
	applyCI(ps, entries, now)
	type view struct {
		ci      string
		waiting bool
	}
	want := []view{
		{"pass", true},
		{"fail", false},
		{"pass", false},
		{"pending", true},
		{"", false},
		{"", false},
		{"", false},
		{"pass", true},
	}
	for i, p := range ps {
		if got := (view{p.ci, p.waiting}); got != want[i] {
			t.Errorf("planet %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestApplyCIIgnoresAnEntryOlderThanTheMaxAge(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	entry := ciEntry{CI: "pass", Review: "REVIEW_REQUIRED", TS: now.Add(-ciMaxAge).Unix()}
	for name, tc := range map[string]struct {
		age  time.Duration
		want string
	}{
		"exactly the max age": {0, "pass"},
		"one second over":     {time.Second, ""},
	} {
		t.Run(name, func(t *testing.T) {
			ps := []planet{{class: classPROpen, prURL: ciURL(1)}}
			applyCI(ps, map[string]ciEntry{ciURL(1): entry}, now.Add(tc.age))
			if ps[0].ci != tc.want || ps[0].waiting != (tc.want != "") {
				t.Errorf("ci %q waiting %v, want %q", ps[0].ci, ps[0].waiting, tc.want)
			}
		})
	}
}
