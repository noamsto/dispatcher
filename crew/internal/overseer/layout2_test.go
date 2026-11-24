package overseer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// stateOpts is o with XDG_STATE_HOME under the fixture root, so layout-2 crews
// resolve there and never under the real home.
func (f *fixture) stateOpts(o Options) Options {
	inner := o.Getenv
	o.Getenv = func(k string) string {
		if k == "XDG_STATE_HOME" {
			return f.root + "/state"
		}
		return inner(k)
	}
	return o
}

func (f *fixture) crewState() string { return f.root + "/state/crew" }

// l2crew creates layout-2 crew id homed in home (a RepoTag, nil for none).
func (f *fixture) l2crew(id string, home *string, visiting bool) {
	f.t.Helper()
	m := bus.Meta{ID: id, HomeHost: "h2", HomeRepo: home, Visiting: visiting, CreatedBy: "register"}
	if err := bus.CreateMeta(f.crewState(), m); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) crewLog(id string) string {
	f.t.Helper()
	data, err := os.ReadFile(bus.StoreDir(f.crewState(), id) + "/events.jsonl")
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *fixture) msgL2(repo, crew string) (int, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunMsg(f.t.Context(), MsgRequest{Repo: repo, Crew: crew, Body: notice}, &stdout, &stderr, f.stateOpts(f.opts()))
	return code, stderr.String()
}

func TestLayout2MsgPostsToHomedCrew(t *testing.T) {
	f := newFixture(t)
	common := f.repo("alpha", "git@github.com:owner/alpha.git")
	f.l2crew("c2", &common, false)
	link := f.root + "/link"
	if err := os.Symlink(common, link); err != nil {
		t.Fatal(err)
	}
	label := repos.Label(common, "git@github.com:owner/alpha.git")
	before := f.logs()[common]
	for i, repo := range []string{common, link, label} {
		if code, stderr := f.msgL2(repo, "c2"); code != 0 || stderr != "" {
			t.Fatalf("%s: exit %d %q", repo, code, stderr)
		}
		want := strings.Repeat(rowFrom("h1", "c2", notice)+"\n", i+1)
		if got := f.crewLog("c2"); got != want {
			t.Fatalf("%s: crew log %q, want %q", repo, got, want)
		}
	}
	if got := f.logs()[common]; got != before {
		t.Errorf("repo log changed: %q", got)
	}
}

func TestLayout2MsgNullHomeCrew(t *testing.T) {
	f := newFixture(t)
	f.l2crew("c2", nil, false)
	if code, stderr := f.msgL2(repos.NoRepoLabel, "c2"); code != 0 || stderr != "" {
		t.Fatalf("exit %d %q", code, stderr)
	}
	if got := f.crewLog("c2"); got != rowFrom("h1", "c2", notice)+"\n" {
		t.Fatalf("crew log %q", got)
	}
}

func TestLayout2MsgRefusals(t *testing.T) {
	f := newFixture(t)
	alpha := f.repo("alpha", "")
	beta := f.repo("beta", "")
	f.l2crew("c2", &alpha, false)
	f.l2crew("c3", &alpha, true)
	f.l2crew("c4", nil, false)
	archived := f.crewState() + "/archive/c5"
	if err := os.MkdirAll(archived, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archived+"/meta.json", []byte(`{"layout":2,"id":"c5"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ repo, crew, want string }{
		{beta, "c2", "crew c2 is homed in " + alpha + ", not " + beta},
		{repos.NoRepoLabel, "c2", "crew c2 is homed in " + alpha + ", not (no repo)"},
		{alpha, "c4", "crew c4 is homed in (no repo), not " + alpha},
		{alpha, "c3", "crew c3 is visiting this host; its dispatcher runs on h2"},
		{alpha, "c5", "crew c5 is archived"},
	} {
		code, stderr := f.msgL2(tt.repo, tt.crew)
		if code != 1 || stderr != prefix+tt.want+"\n" {
			t.Errorf("%s %s: exit %d %q, want %q", tt.repo, tt.crew, code, stderr, tt.want)
		}
	}
	for _, id := range []string{"c2", "c3", "c4"} {
		if got := f.crewLog(id); got != "" {
			t.Errorf("%s log %q, want none", id, got)
		}
	}
}

func TestLayout2MsgLayout1CrewKeepsRepoPath(t *testing.T) {
	f := newFixture(t)
	common := f.repo("alpha", "")
	code, stderr := f.msgL2(common, "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d %q", code, stderr)
	}
	if got := f.logs()[common]; got != seed+"\n"+rowFrom("h1", "c1", notice)+"\n" {
		t.Errorf("repo log %q", got)
	}
}

func TestLayout2MsgNilGetenv(t *testing.T) {
	f := newFixture(t)
	common := f.repo("alpha", "")
	f.l2crew("c2", &common, false)
	t.Setenv("XDG_STATE_HOME", f.root+"/state")
	if err := os.Mkdir(f.root+"/home", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", f.root+"/home")
	t.Setenv("XDG_DATA_HOME", f.root+"/data")
	o := f.opts()
	o.Getenv = nil
	var stdout, stderr bytes.Buffer
	if code := RunMsg(t.Context(), MsgRequest{Repo: common, Crew: "c2", Body: notice}, &stdout, &stderr, o); code != 0 {
		t.Fatalf("exit %d %q", code, stderr.String())
	}
	if got := f.crewLog("c2"); got != rowFrom("h1", "c2", notice)+"\n" {
		t.Fatalf("crew log %q", got)
	}
}

func (f *fixture) deliverL2(repo, crew string) (int, string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunDeliver(f.t.Context(), DeliverRequest{Origin: "halo", Repo: repo, Crew: crew, Body: notice}, &stdout, &stderr, f.stateOpts(f.opts()))
	return code, stdout.String(), stderr.String()
}

func TestLayout2DeliverHomedVisitingAndWrongRepo(t *testing.T) {
	f := mbpFixture(t)
	alpha := f.commons[0]
	beta := f.repo("beta", "")
	f.l2crew("c2", &alpha, false)
	f.l2crew("c3", &alpha, true)

	label := repos.Label(alpha, "")
	code, stdout, stderr := f.deliverL2(label, "c2")
	if code != 0 || stderr != "" || stdout != `{"ts":`+tsText+`}`+"\n" {
		t.Fatalf("homed: exit %d %q %q", code, stdout, stderr)
	}
	if got := f.crewLog("c2"); got != rowFrom("halo", "c2", notice)+"\n" {
		t.Errorf("crew log %q", got)
	}
	for _, tt := range []struct {
		repo, crew string
		code       farRefusal
	}{
		{alpha, "c3", refusedCrew},
		{beta, "c2", refusedRepo},
	} {
		code, stdout, _ := f.deliverL2(tt.repo, tt.crew)
		if code != 1 || stdout != `{"refused":"`+string(tt.code)+`"}`+"\n" {
			t.Errorf("%s %s: exit %d %q, want code %s", tt.repo, tt.crew, code, stdout, tt.code)
		}
	}
	if got := f.crewLog("c3"); got != "" {
		t.Errorf("visiting crew log %q", got)
	}
}

func (tc *trustCase) runL2() (int, string, string) {
	tc.t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunTrust(tc.t.Context(), "%7", &stdout, &stderr, tc.stateOpts(tc.opts()))
	return code, stdout.String(), stderr.String()
}

func (tc *trustCase) writePane(id, pane string) {
	tc.t.Helper()
	if err := os.WriteFile(filepath.Join(bus.StoreDir(tc.crewState(), id), "pane"), []byte(pane+"\n"), 0o644); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *trustCase) wantAccepted(crew string) {
	tc.t.Helper()
	code, stdout, stderr := tc.runL2()
	if code != 0 || stderr != "" || stdout != "accepted the workspace-trust dialog in pane %7\n" {
		tc.t.Fatalf("exit %d %q %q", code, stdout, stderr)
	}
	if got, want := tc.crewLog(crew), `"body":"{\"kind\":\"notice\",\"detail\":\"accepted the workspace-trust dialog in pane %7\"}"`; !strings.Contains(got, want) || !strings.Contains(got, `"crew_id":"`+crew+`"`) {
		tc.t.Fatalf("crew log %q", got)
	}
}

func (tc *trustCase) wantRefused(want string) {
	tc.t.Helper()
	code, _, stderr := tc.runL2()
	if code != 1 || stderr != trustPrefix+want+"\n" {
		tc.t.Fatalf("exit %d %q, want %q", code, stderr, want)
	}
	if len(tc.typed()) != 0 {
		tc.t.Errorf("typed %q", tc.typed())
	}
}

func TestLayout2TrustNullHomeOwnPaneNeedsNoRepo(t *testing.T) {
	tc := newTrustCase(t)
	tc.l2crew("c2", nil, false)
	tc.crew = "c2"
	tc.path = "/not/a/repo"
	tc.notRepo = true
	tc.writePane("c2", "%7")
	tc.wantAccepted("c2")
}

func TestLayout2TrustStampedPaneInHomeRepo(t *testing.T) {
	tc := newTrustCase(t)
	tc.l2crew("c2", &tc.common, false)
	tc.crew = "c2"
	tc.wantAccepted("c2")
}

func TestLayout2TrustStampedPaneInIndexedRepo(t *testing.T) {
	tc := newTrustCase(t)
	home := tc.repo("home", "")
	tc.l2crew("c2", &home, false)
	if _, err := bus.IndexAdd(tc.crewState(), tc.common, "r", "c2", time.Now()); err != nil {
		t.Fatal(err)
	}
	tc.crew = "c2"
	tc.wantAccepted("c2")
}

func TestLayout2TrustStampedPaneInUnrelatedRepoRefused(t *testing.T) {
	tc := newTrustCase(t)
	home := tc.repo("home", "")
	tc.l2crew("c2", &home, false)
	tc.crew = "c2"
	tc.wantRefused("pane %7's directory " + panePath + " is not in crew c2's home repo or a repo it dispatched to")
	if got := tc.crewLog("c2"); got != "" {
		t.Errorf("crew log %q", got)
	}
}

func TestLayout2TrustStampedPaneNonRepoHomedCrewRefused(t *testing.T) {
	tc := newTrustCase(t)
	home := tc.repo("home", "")
	tc.l2crew("c2", &home, false)
	tc.crew = "c2"
	tc.path = "/not/a/repo"
	tc.notRepo = true
	tc.writePane("c2", "%7")
	tc.wantRefused(`pane %7's directory "/not/a/repo" is not in a git repo, and pane %7 is not the dispatcher pane of crew c2 with no home repo`)
}

func TestLayout2TrustStampedPaneNonRepoOtherPaneRefused(t *testing.T) {
	tc := newTrustCase(t)
	tc.l2crew("c2", nil, false)
	tc.crew = "c2"
	tc.path = "/not/a/repo"
	tc.notRepo = true
	tc.writePane("c2", "%9")
	tc.wantRefused(`pane %7's directory "/not/a/repo" is not in a git repo, and pane %7 is not the dispatcher pane of crew c2 with no home repo`)
}

func TestLayout2TrustUnstampedPaneFile(t *testing.T) {
	tc := newTrustCase(t)
	tc.l2crew("c2", nil, false)
	tc.l2crew("c3", nil, true)
	tc.crew = ""
	tc.path = "/not/a/repo"
	tc.notRepo = true
	for _, id := range []string{"c2", "c3"} {
		if err := os.WriteFile(filepath.Join(bus.StoreDir(tc.crewState(), id), "pane"), []byte("%7\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, _, stderr := tc.runL2()
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d %q", code, stderr)
	}
	if !strings.Contains(tc.crewLog("c2"), `"crew_id":"c2"`) || tc.crewLog("c3") != "" {
		t.Fatalf("c2 %q c3 %q", tc.crewLog("c2"), tc.crewLog("c3"))
	}
}

func TestLayout2TrustUnstampedAmbiguous(t *testing.T) {
	tc := newTrustCase(t)
	tc.l2crew("c2", nil, false)
	tc.l2crew("c3", nil, false)
	tc.crew = ""
	for _, id := range []string{"c2", "c3"} {
		if err := os.WriteFile(filepath.Join(bus.StoreDir(tc.crewState(), id), "pane"), []byte("%7\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, _, stderr := tc.runL2()
	want := trustPrefix + "pane %7 has no @crew_id stamp, and crews c2, c3 all name it as their dispatcher pane\n"
	if code != 1 || stderr != want {
		t.Fatalf("exit %d %q, want %q", code, stderr, want)
	}
	if len(tc.typed()) != 0 {
		t.Errorf("typed %q", tc.typed())
	}
}
