package data

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCrewBus(t *testing.T) {
	log := filepath.Join(t.TempDir(), "events.jsonl")
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"crew where --json --crew c1":    {Stdout: []byte(`{"crew_id":"c1","layout":1,"dir":"/x/myrepo/.git/crew/crews/c1","log":"/x/myrepo/.git/crew/events.jsonl","repo_dir":"/x/myrepo"}`)},
		"crew where --json --crew c2":    {Stdout: []byte(`{"crew_id":"c2","layout":2,"dir":"/state/crews/c2","log":"` + log + `","repo_dir":null}`)},
		"crew where --json":              {Stdout: []byte(`{"crew_id":"c3","layout":2,"dir":"/d","log":"/l"}`)},
		"crew where --json --crew bad":   {Stdout: []byte("not json")},
		"crew where --json --crew empty": {},
		"crew where --json --crew fail":  {Err: FakeExitError{Code: 2}},
	}}
	ctx := context.Background()
	cfg := baseConfig()

	b, err := ResolveCrewBus(ctx, r, cfg, "c1")
	if err != nil || b.Layout != 1 || b.Dir != "/x/myrepo/.git/crew/crews/c1" || b.RepoDir == nil || *b.RepoDir != "/x/myrepo" {
		t.Fatalf("layout 1 = %+v, %v", b, err)
	}
	b, err = ResolveCrewBus(ctx, r, cfg, "c2")
	if err != nil || b.Layout != 2 || b.Log != log || b.Dir != "/state/crews/c2" || b.RepoDir != nil {
		t.Fatalf("layout 2 = %+v, %v", b, err)
	}
	b, err = ResolveCrewBus(ctx, r, cfg, "")
	if err != nil || b.CrewID != "c3" {
		t.Fatalf("no --crew = %+v, %v", b, err)
	}
	for _, id := range []string{"bad", "empty", "fail"} {
		if _, err := ResolveCrewBus(ctx, r, cfg, id); err == nil {
			t.Errorf("%s: want error", id)
		}
	}
}

func TestResolveCrewDir(t *testing.T) {
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"crew where --dir --crew c1": {Stdout: []byte("/state/crews/c1\n")},
		"crew where --dir --crew e":  {Stdout: []byte("\n")},
		"crew where --dir --crew f":  {Err: FakeExitError{Code: 1}},
	}}
	ctx := context.Background()
	got, err := ResolveCrewDir(ctx, r, baseConfig(), "/repo", "c1")
	if err != nil || got != "/state/crews/c1" {
		t.Fatalf("got %q, %v", got, err)
	}
	if calls := r.CallsSnapshot(); calls[0].Dir != "/repo" {
		t.Errorf("dir = %q, want /repo", calls[0].Dir)
	}
	for _, id := range []string{"e", "f"} {
		if _, err := ResolveCrewDir(ctx, r, baseConfig(), "", id); err == nil {
			t.Errorf("%s: want error", id)
		}
	}
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"/x/myrepo/.git": "myrepo",
		"/x/myrepo":      "myrepo",
		"":               "",
	} {
		if got := RepoName(in); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoFromLog(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "events.jsonl")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := write(`{"kind":"msg","crew_id":"c2"}
{"kind":"dispatch","crew_id":"other","repo":"/x/other/.git"}
{"kind":"dispatch","crew_id":"c2","repo":"/x/myrepo/.git"}
{"kind":"dispatch","crew_id":"c2","repo":"/x/later/.git"}
`)
	if got := RepoFromLog(p, "c2"); got != "myrepo" {
		t.Errorf("got %q, want myrepo", got)
	}
	if got := RepoFromLog(write(`{"kind":"msg","crew_id":"c2"}`+"\n"), "c2"); got != "" {
		t.Errorf("no dispatch row = %q, want empty", got)
	}
	if got := RepoFromLog(filepath.Join(t.TempDir(), "missing"), "c2"); got != "" {
		t.Errorf("missing log = %q, want empty", got)
	}
}
