package rosterrender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// repoAt makes <root>/<name>/.git holding a crew bus with the given lines, the
// shape discovery lists, and is named for its checkout directory.
func repoAt(t *testing.T, name string, lines ...string) bus.Paths {
	t.Helper()
	common := filepath.Join(t.TempDir(), name, ".git")
	p := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBus(t, p, lines...)
	return p
}

func writeBus(t *testing.T, p bus.Paths, lines ...string) {
	t.Helper()
	text := strings.Join(lines, "\n")
	if text != "" {
		text += "\n"
	}
	if err := os.WriteFile(p.Log, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// workerEvents is a launched, working worker of crew c1.
func workerEvents(branch string) []string {
	return []string{
		dispatchEvent(branch, 1791360000000, "orion", "red", "colour1", "Task", "standard", "claude", "sonnet"),
		statusEvent("worker:"+branch+"#s1", 1791361200000, "working", "", "", ""),
	}
}

func okRepos(ps ...bus.Paths) []repos.Repo {
	out := make([]repos.Repo, len(ps))
	for i, p := range ps {
		out[i] = repos.Repo{Common: p.Common, Status: repos.StatusOK}
	}
	return out
}

// discoverStub is a Discover that counts its calls.
type discoverStub struct {
	calls int
	found []repos.Repo
	note  string
}

func (d *discoverStub) discover(w io.Writer) []repos.Repo {
	d.calls++
	if d.note != "" {
		_, _ = io.WriteString(w, d.note)
	}
	return d.found
}

func (l *loop) frame(t *testing.T) frame {
	t.Helper()
	if err := os.MkdirAll(l.cdir(), 0o755); err != nil {
		t.Fatal(err)
	}
	fr, err := buildFrame(t.Context(), l.opts, l.paths, l.cdir(), l.crew, roleRows(l.rolePanes(), l.paths.Dir, l.crew))
	if err != nil {
		t.Fatal(err)
	}
	return fr
}

func (l *loop) stderr() string { return l.opts.Stderr.(*bytes.Buffer).String() }

// clockAt makes the wall clock a variable the test advances.
func (l *loop) clockAt() *time.Time {
	at := time.Unix(1_791_360_000, 0)
	l.opts.Clock.Now = func() time.Time { return at }
	return &at
}

func (fr frame) planet(repo, branch string) (placed, bool) {
	for _, p := range fr.sc.planets {
		if p.repo == repo && p.branch == branch {
			return p, true
		}
	}
	return placed{}, false
}

func wedgeLabels(fr frame) []string {
	var out []string
	for _, w := range fr.sc.wedges {
		out = append(out, w.label)
	}
	slices.Sort(out)
	return out
}

func TestWithoutDiscoveryOnlyTheLocalBusDraws(t *testing.T) {
	l := newLoop(t)
	l.seed(t, workerEvents("feat/1-a")...)
	repoAt(t, "alpha", workerEvents("feat/9-z")...)
	l.windows = "feat/1-a\nfeat/9-z"

	fr := l.frame(t)
	if len(fr.sc.planets) != 1 || len(fr.sc.wedges) != 0 {
		t.Fatalf("%d planets, %d wedges, want 1 and 0", len(fr.sc.planets), len(fr.sc.wedges))
	}
	p := fr.sc.planets[0]
	if p.repo != l.paths.Common || p.repoLabel != clean(filepath.Base(l.paths.Common)) {
		t.Errorf("planet repo = %q (%q), want the local bus", p.repo, p.repoLabel)
	}
	if got := l.stderr(); got != "" {
		t.Errorf("stderr = %q", got)
	}
}

func TestEveryRepoHoldingTheCrewGetsAWedge(t *testing.T) {
	l := newLoop(t)
	l.seed(t, workerEvents("feat/1-a")...)
	alpha := repoAt(t, "alpha", workerEvents("feat/9-z")...)
	// A bus with rows for another crew only is never joined.
	strangers := repoAt(t, "gamma", strings.ReplaceAll(workerEvents("feat/8-y")[0], `"c1"`, `"c2"`))
	d := &discoverStub{found: append(okRepos(alpha, strangers),
		repos.Repo{Common: "/nonexistent/.git", Status: repos.StatusGone},
		repos.Repo{Common: "/nonexistent/other/.git", Status: repos.StatusNoBus}), note: "crew: roster-render: a note\n"}
	l.opts.Discover = d.discover
	l.windows = "feat/1-a\nfeat/9-z\nfeat/8-y"

	fr := l.frame(t)
	if want := []string{"001", "alpha"}; !slices.Equal(wedgeLabels(fr), want) {
		t.Fatalf("wedges = %v, want %v", wedgeLabels(fr), want)
	}
	if _, ok := fr.planet(l.paths.Common, "feat/1-a"); !ok {
		t.Error("the local worker is not drawn under the local repo")
	}
	p, ok := fr.planet(alpha.Common, "feat/9-z")
	if !ok || p.repoLabel != "alpha" {
		t.Errorf("the second repo's worker = %+v, %v", p, ok)
	}
	if len(fr.sc.planets) != 2 {
		t.Errorf("%d planets, want 2: the other crew's worker must not draw", len(fr.sc.planets))
	}
	if fr.lg.working != 2 || fr.lg.repos != 2 {
		t.Errorf("legend working %d repos %d, want 2 and 2", fr.lg.working, fr.lg.repos)
	}
	if got := l.stderr(); got != "crew: roster-render: a note\n" {
		t.Errorf("stderr = %q, want only discovery's note", got)
	}
}

func TestAnUnreadableBusIsSkippedWithOneLine(t *testing.T) {
	l := newLoop(t)
	l.seed(t, workerEvents("feat/1-a")...)
	garbage := repoAt(t, "garbage", "{not json")
	asDir := repoAt(t, "dir")
	if err := os.Remove(asDir.Log); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(asDir.Log, 0o755); err != nil {
		t.Fatal(err)
	}
	l.opts.Discover = (&discoverStub{found: okRepos(garbage, asDir)}).discover

	fr := l.frame(t)
	if len(fr.sc.planets) != 1 || len(fr.sc.wedges) != 0 {
		t.Errorf("%d planets, %d wedges, want the local worker alone", len(fr.sc.planets), len(fr.sc.wedges))
	}
	for _, p := range []bus.Paths{garbage, asDir} {
		line := "crew: roster-render: " + p.Common + ": skipped ("
		if got := strings.Count(l.stderr(), line); got != 1 {
			t.Errorf("%d skip lines for %s in %q, want 1", got, p.Common, l.stderr())
		}
	}
}

// tear appends a line the bus reader refuses and discovery's tolerant one skips.
func tear(t *testing.T, p bus.Paths) {
	t.Helper()
	f, err := os.OpenFile(p.Log, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{torn\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func TestABusThatBreaksAfterJoiningIsSkippedAndTheLocalOneDraws(t *testing.T) {
	for name, tc := range map[string]struct {
		disc                  *discovery
		planets, wedges, live int
		wantLastGood          bool
	}{
		"without a discovery record the repo is skipped": {nil, 1, 0, 1, false},
		"with one it keeps its last good frame":          {&discovery{}, 2, 2, 2, true},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLoop(t)
			l.seed(t, workerEvents("feat/1-a")...)
			alpha := repoAt(t, "alpha", workerEvents("feat/9-z")...)
			l.opts.Discover = (&discoverStub{found: okRepos(alpha)}).discover
			l.opts.disc = tc.disc
			l.windows = "feat/1-a\nfeat/9-z"
			if fr := l.frame(t); len(fr.sc.wedges) != 2 {
				t.Fatalf("%d wedges before the break, want 2", len(fr.sc.wedges))
			}

			tear(t, alpha)

			fr := l.frame(t)
			if len(fr.sc.planets) != tc.planets || len(fr.sc.wedges) != tc.wedges || fr.live != int64(tc.live) {
				t.Errorf("%d planets, %d wedges, live %d, want %d, %d and %d",
					len(fr.sc.planets), len(fr.sc.wedges), fr.live, tc.planets, tc.wedges, tc.live)
			}
			skip := "crew: roster-render: " + alpha.Common + ": skipped ("
			if !strings.Contains(l.stderr(), skip) {
				t.Errorf("no skip line in %q", l.stderr())
			}
			if got := strings.Contains(l.stderr(), "; showing its last good frame\n"); got != tc.wantLastGood {
				t.Errorf("last-good note = %v in %q, want %v", got, l.stderr(), tc.wantLastGood)
			}
		})
	}
}

func TestALastGoodResultIsACopy(t *testing.T) {
	d := &discovery{}
	d.remember("/r", sourceResult{planets: []planet{{branch: "b", moons: []moon{{role: "critic"}}}}})

	got, ok := d.recall("/r")
	if !ok {
		t.Fatal("nothing recalled")
	}
	got.planets[0].ci, got.planets[0].moons[0].x = "fail", 7
	again, _ := d.recall("/r")
	if again.planets[0].ci != "" || again.planets[0].moons[0].x != 0 {
		t.Errorf("a change to the recalled planets wrote through: %+v", again.planets[0])
	}
	if _, ok := d.recall("/other"); ok {
		t.Error("recalled a repo that never had a good pass")
	}
	if _, ok := (*discovery)(nil).recall("/r"); ok {
		t.Error("a nil record recalled something")
	}
}

func TestAJoinedBusThatBreaksDoesNotRetireTheDaemon(t *testing.T) {
	l := newLoop(t)
	utc(t)
	// The local crew is drained: its one worker waits on a PR, so only the joined
	// repo's worker is live.
	l.seed(t,
		dispatchEvent("feat/1-a", 1791360000000, "orion", "red", "colour1", "Task", "standard", "claude", "sonnet"),
		statusEvent("worker:feat/1-a#s1", 1791360100000, "pr_open", "", "", ""))
	alpha := repoAt(t, "alpha", workerEvents("feat/9-z")...)
	l.opts.Discover = (&discoverStub{found: okRepos(alpha)}).discover
	l.opts.disc = &discovery{}
	l.windows = "feat/1-a\nfeat/9-z"

	sleep := l.opts.Probes.Sleep
	ticks := 0
	l.opts.Probes.Sleep = func(ctx context.Context, d time.Duration) error {
		ticks++
		if ticks == 3 {
			tear(t, alpha)
		}
		return sleep(ctx, d)
	}
	l.run("10")

	if !l.hitCeiling() {
		t.Fatalf("the renderer retired at tick %d while the joined repo's worker was live\n%s", ticks, l.stderr())
	}
	if !strings.Contains(l.stderr(), "showing its last good frame") {
		t.Errorf("the break never reached a pass: %q", l.stderr())
	}
	dot := l.dot()
	if n := len(planetNode.FindAllString(dot, -1)); n != 2 {
		t.Errorf("%d planets in the last dot, want 2", n)
	}
	if !strings.Contains(dot, `label="alpha"`) {
		t.Errorf("the joined repo's wedge is gone from the dot:\n%s", dot)
	}
}

func TestTheLocalRepoIsNotJoinedTwice(t *testing.T) {
	l := newLoop(t)
	l.seed(t, workerEvents("feat/1-a")...)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(l.paths.Common, link); err != nil {
		t.Fatal(err)
	}
	for name, common := range map[string]string{"same path": l.paths.Common, "trailing slash": l.paths.Common + "/", "symlink": link} {
		t.Run(name, func(t *testing.T) {
			l.opts.Discover = (&discoverStub{found: []repos.Repo{{Common: common, Status: repos.StatusOK}}}).discover
			l.opts.Stderr = &bytes.Buffer{}
			fr := l.frame(t)
			if len(fr.sc.planets) != 1 || len(fr.sc.wedges) != 0 {
				t.Errorf("%d planets, %d wedges, want 1 and 0", len(fr.sc.planets), len(fr.sc.wedges))
			}
			if got := l.stderr(); got != "" {
				t.Errorf("stderr = %q", got)
			}
		})
	}
}

func TestTheSameBranchInTwoReposIsTwoPlanetsAndBothKeepASlot(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t, workerEvents("feat/1-a")...)
	alpha := repoAt(t, "alpha", workerEvents("feat/1-a")...)
	l.opts.Discover = (&discoverStub{found: okRepos(alpha)}).discover
	l.windows = "feat/1-a"

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	out := l.published()
	if n := len(planetNode.FindAllString(out, -1)); n != 2 {
		t.Errorf("%d planets drawn, want 2", n)
	}

	data, err := os.ReadFile(filepath.Join(l.cdir(), fileSlots))
	if err != nil {
		t.Fatal(err)
	}
	var s slotState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.Order {
		if e.Branch != "feat/1-a" {
			t.Errorf("slot for %q", e.Branch)
		}
		got = append(got, e.Repo)
	}
	want := []string{alpha.Common, l.paths.Common}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("slot repos = %v, want %v", got, want)
	}

	// The dot names each wedge by its repo.
	dot := l.dot()
	var labels []string
	for _, id := range []string{"w1", "w2"} {
		line := nodeLine(t, dot, id)
		for _, want := range []string{"001", "alpha"} {
			if strings.Contains(line, `label="`+want+`"`) {
				labels = append(labels, want)
			}
		}
	}
	slices.Sort(labels)
	if want := []string{"001", "alpha"}; !slices.Equal(labels, want) {
		t.Errorf("wedge label nodes = %v, want %v", labels, want)
	}
}

// prJSON is a `gh pr view` answer.
func prJSON(review, rollup string) string {
	return fmt.Sprintf(`{"state":"OPEN","reviewDecision":%q,"statusCheckRollup":%s}`, review, rollup)
}

// ciLoop is a crew with a pr_open worker per PR, on a repo whose origin is o/r.
func ciLoop(t *testing.T, urls map[string]string) (*loop, *ghtest.Fake, *time.Time) {
	t.Helper()
	l := newLoop(t)
	l.opts.Git = &gittest.Fake{Func: func(_ string, args []string) (string, error) {
		if slices.Contains(args, "config") {
			return "https://github.com/o/r.git", nil
		}
		return "", nil
	}}
	var events, windows []string
	for _, branch := range slices.Sorted(maps.Keys(urls)) {
		events = append(events,
			dispatchEvent(branch, 1791360000000, "orion", "red", "colour1", "Task", "standard", "claude", "sonnet"),
			statusEvent("worker:"+branch+"#s1", 1791361200000, "pr_open", "", "", urls[branch]))
		windows = append(windows, branch)
	}
	l.seed(t, events...)
	l.windows = strings.Join(windows, "\n")
	fake := &ghtest.Fake{Func: func(args []string) (string, error) {
		switch args[2] {
		case ciURL(1):
			return prJSON("", `[{"state":"SUCCESS"}]`), nil
		case ciURL(2):
			return prJSON("", `[{"status":"IN_PROGRESS"}]`), nil
		case ciURL(3):
			return prJSON("", `[{"status":"COMPLETED","conclusion":"FAILURE"}]`), nil
		case ciURL(4):
			return prJSON("", `[]`), nil
		case ciURL(5):
			return prJSON("REVIEW_REQUIRED", `[{"state":"SUCCESS"}]`), nil
		}
		return "", errors.New("no such PR")
	}}
	l.opts.Gh = fake
	return l, fake, l.clockAt()
}

func ciOf(t *testing.T, fr frame, repo, branch string) placed {
	t.Helper()
	p, ok := fr.planet(repo, branch)
	if !ok {
		t.Fatalf("no planet for %s", branch)
	}
	return p
}

func TestOpenPRsShowTheirCIStateAndAreAskedOnceAMinute(t *testing.T) {
	foreign := "https://github.com/x/y/pull/9"
	l, fake, at := ciLoop(t, map[string]string{
		"feat/ci-1": ciURL(1), "feat/ci-2": ciURL(2), "feat/ci-3": ciURL(3),
		"feat/ci-4": ciURL(4), "feat/ci-5": ciURL(5), "feat/ci-6": foreign,
	})
	repo := l.paths.Common

	fr := l.frame(t)
	for branch, want := range map[string]string{
		"feat/ci-1": "pass", "feat/ci-2": "pending", "feat/ci-3": "fail",
		"feat/ci-4": "none", "feat/ci-5": "pass", "feat/ci-6": "",
	} {
		if got := ciOf(t, fr, repo, branch).ci; got != want {
			t.Errorf("%s: ci = %q, want %q", branch, got, want)
		}
	}
	for branch, want := range map[string]bool{"feat/ci-1": false, "feat/ci-5": true, "feat/ci-6": false} {
		if got := ciOf(t, fr, repo, branch).waiting; got != want {
			t.Errorf("%s: waiting = %v, want %v", branch, got, want)
		}
	}
	if n := len(fake.Calls()); n != 5 {
		t.Errorf("%d gh calls, want 5: another repo's PR must not reach gh", n)
	}
	if fr.lg.ci != 5 || fr.lg.waiting != 1 {
		t.Errorf("legend ci %d waiting %d, want 5 and 1", fr.lg.ci, fr.lg.waiting)
	}
	if _, err := os.Stat(filepath.Join(l.cdir(), fileCI)); err != nil {
		t.Errorf("no CI cache: %v", err)
	}

	*at = at.Add(30 * time.Second)
	l.frame(t)
	if n := len(fake.Calls()); n != 5 {
		t.Errorf("%d gh calls after 30 s, want the same 5", n)
	}

	*at = at.Add(31 * time.Second)
	l.frame(t)
	if n := len(fake.Calls()); n != 10 {
		t.Errorf("%d gh calls after 61 s, want 10", n)
	}
}

func TestWithoutGhTheCacheIsStillDrawn(t *testing.T) {
	l, fake, _ := ciLoop(t, map[string]string{"feat/ci-1": ciURL(1)})
	l.frame(t)
	l.opts.Gh = nil
	fr := l.frame(t)
	if got := ciOf(t, fr, l.paths.Common, "feat/ci-1").ci; got != "pass" {
		t.Errorf("ci = %q, want the cached pass", got)
	}
	if n := len(fake.Calls()); n != 1 {
		t.Errorf("%d gh calls, want 1", n)
	}
}

func TestMapNeverAsksGitHubAndDrawsTheCachedState(t *testing.T) {
	l, fake, at := ciLoop(t, map[string]string{"feat/ci-1": ciURL(1)})
	l.frame(t)
	cache := filepath.Join(l.cdir(), fileCI)
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	var dots []string
	l.opts.Probes.Graphviz = func(_ context.Context, dot []byte) ([]byte, error) {
		dots = append(dots, string(dot))
		return []byte(`<svg width="500pt" height="400pt" viewBox="0.00 0.00 500.00 400.00" xmlns="x"></svg>`), nil
	}
	l.opts.Probes.Resvg = func(context.Context, []byte, int, int) ([]byte, error) { return []byte("PNG"), nil }
	l.opts.Stdout = io.Discard
	// Past the TTL, short of the max age: a daemon or --once would ask again.
	*at = at.Add(5 * time.Minute)
	req := Request{Crew: l.crew, Map: true, Px: "300x200", Out: filepath.Join(t.TempDir(), "map.png")}
	if code := Run(t.Context(), req, l.paths, l.opts); code != exitOK {
		t.Fatalf("code %d: %s", code, l.stderr())
	}
	if n := len(fake.Calls()); n != 1 {
		t.Errorf("--map made %d gh calls in all, want the daemon's 1", n)
	}
	after, err := os.ReadFile(cache)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("--map rewrote the CI cache: %q -> %q, %v", before, after, err)
	}

	cached := l.opts
	cached.Gh = nil
	fr, err := buildFrame(t.Context(), cached, l.paths, l.cdir(), l.crew, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ciOf(t, fr, l.paths.Common, "feat/ci-1").ci; got != "pass" {
		t.Fatalf("cached ci = %q, want pass", got)
	}
	if len(dots) != 1 || dots[0] != buildDOT(fr.sc, nil, "") {
		t.Errorf("--map did not draw the cached frame:\n%s", dots)
	}
}

func TestLiveCountSumsOverTheJoinedRepos(t *testing.T) {
	l := newLoop(t)
	alpha := repoAt(t, "alpha", workerEvents("feat/9-z")...)
	l.windows = "feat/9-z"
	cdir := l.cdir()
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := pass(t.Context(), l.opts, l.paths, cdir, l.crew, true, nil)
	if err != nil || got != 0 {
		t.Fatalf("local alone: live = %d, %v, want 0", got, err)
	}
	l.opts.Discover = (&discoverStub{found: okRepos(alpha)}).discover
	got, err = pass(t.Context(), l.opts, l.paths, cdir, l.crew, true, nil)
	if err != nil || got != 1 {
		t.Errorf("with the second repo: live = %d, %v, want 1", got, err)
	}
}

func TestTheLoopSignatureCoversEveryJoinedRepo(t *testing.T) {
	l := newLoop(t)
	l.seed(t, workerEvents("feat/1-a")...)
	alpha := repoAt(t, "alpha", workerEvents("feat/9-z")...)
	cdir := l.cdir()
	sig := func(panes string) string {
		t.Helper()
		srcs := l.opts.sources(t.Context(), l.paths, l.crew)
		return loopSig(busSize(l.paths), cdir, roleRows(panes, l.paths.Dir, l.crew)) +
			joinedSig(srcs[1:], panes, l.crew)
	}

	// One repo: the signature is what it always was.
	single := sig("")
	if want := fmt.Sprintf("%d|%s|%s", busSize(l.paths), record(cdir, filePane), ""); single != want {
		t.Errorf("single-source signature = %q, want %q", single, want)
	}

	l.opts.Discover = (&discoverStub{found: okRepos(alpha)}).discover
	two := sig("")
	if two == single {
		t.Fatal("joining a repo did not change the signature")
	}
	f, err := os.OpenFile(alpha.Log, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(statusEvent("worker:feat/9-z#s1", 1791361300000, "working", "x", "", "") + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	grown := sig("")
	if grown == two {
		t.Error("the second repo's bus grew and the signature did not change")
	}
	roles := alpha.Dir + "\t" + l.crew + "\tfeat/9-z\tspec-critic\tidle\t\n"
	if sig(roles) == grown {
		t.Error("a role pane of the second repo did not change the signature")
	}
}

func TestTheDaemonRedrawsWhenOnlyTheSecondRepoChanges(t *testing.T) {
	l := newLoop(t)
	utc(t)
	l.seed(t, workerEvents("feat/1-a")...)
	alpha := repoAt(t, "alpha", workerEvents("feat/9-z")...)
	l.opts.Discover = (&discoverStub{found: okRepos(alpha)}).discover
	l.windows = "feat/1-a\nfeat/9-z\nfeat/9-y"

	sleep := l.opts.Probes.Sleep
	ticks := 0
	l.opts.Probes.Sleep = func(ctx context.Context, d time.Duration) error {
		ticks++
		if ticks == 3 {
			f, err := os.OpenFile(alpha.Log, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Error(err)
			} else {
				for _, e := range workerEvents("feat/9-y") {
					_, _ = f.WriteString(e + "\n")
				}
				_ = f.Close()
			}
		}
		return sleep(ctx, d)
	}
	l.opts.disc = &discovery{}
	l.run("600")
	if got := l.graphvizCalls(); got != 2 {
		t.Errorf("%d draws, want the first and the one for the second repo's change", got)
	}
	if n := len(planetNode.FindAllString(l.dot(), -1)); n != 3 {
		t.Errorf("%d planets in the last dot, want 3", n)
	}
}

func TestDiscoveryIsReusedForFiveMinutes(t *testing.T) {
	l := newLoop(t)
	l.seed(t, workerEvents("feat/1-a")...)
	alpha := repoAt(t, "alpha", workerEvents("feat/9-z")...)
	d := &discoverStub{}
	l.opts.Discover = d.discover
	l.opts.disc = &discovery{}
	l.windows = "feat/1-a\nfeat/9-z"
	at := l.clockAt()

	if fr := l.frame(t); len(fr.sc.wedges) != 0 || d.calls != 1 {
		t.Fatalf("first pass: %d wedges, %d discoveries", len(fr.sc.wedges), d.calls)
	}
	d.found = okRepos(alpha)
	*at = at.Add(299 * time.Second)
	if fr := l.frame(t); len(fr.sc.wedges) != 0 || d.calls != 1 {
		t.Errorf("at 299 s: %d wedges, %d discoveries, want the first list again", len(fr.sc.wedges), d.calls)
	}
	*at = at.Add(time.Second)
	if fr := l.frame(t); len(fr.sc.wedges) != 2 || d.calls != 2 {
		t.Errorf("at 300 s: %d wedges, %d discoveries, want a fresh list", len(fr.sc.wedges), d.calls)
	}
}

func TestWithoutADiscoveryRecordEveryPassDiscovers(t *testing.T) {
	l := newLoop(t)
	l.seed(t, workerEvents("feat/1-a")...)
	d := &discoverStub{}
	l.opts.Discover = d.discover
	l.frame(t)
	l.frame(t)
	if d.calls != 2 {
		t.Errorf("%d discoveries over two one-shot passes, want 2", d.calls)
	}
}

func TestAnOldCacheDrawsNoBadge(t *testing.T) {
	l, _, at := ciLoop(t, map[string]string{"feat/ci-5": ciURL(5)})
	l.frame(t)
	l.opts.Gh = nil
	cached := func(age time.Duration) placed {
		t.Helper()
		*at = at.Add(age)
		return ciOf(t, l.frame(t), l.paths.Common, "feat/ci-5")
	}

	if p := cached(ciMaxAge); p.ci != "pass" || !p.waiting {
		t.Errorf("at the max age: ci %q waiting %v, want the cached pass", p.ci, p.waiting)
	}
	if p := cached(time.Second); p.ci != "" || p.waiting {
		t.Errorf("a second past it: ci %q waiting %v, want no badge", p.ci, p.waiting)
	}
}

func sourcesOf(labels ...string) []source {
	out := make([]source, len(labels))
	for i, l := range labels {
		out[i] = source{label: l}
	}
	return out
}

func TestNameWedges(t *testing.T) {
	long := strings.Repeat("x", 30)
	for name, tc := range map[string]struct {
		labels []string
		want   []string
	}{
		"distinct names stay the repo name":    {[]string{"a/one", "b/two"}, []string{"one", "two"}},
		"a checkout dir has no owner":          {[]string{"tools", "o/other"}, []string{"tools", "other"}},
		"same name, different owners":          {[]string{"a/tools", "b/tools", "c/other"}, []string{"a·tools", "b·tools", "other"}},
		"same owner and name, two clones":      {[]string{"o/tools", "o/tools", "o/tools"}, []string{"o·tools", "o·tools·2", "o·tools·3"}},
		"a name colliding with an owner label": {[]string{"tools", "a/tools"}, []string{"tools", "a·tools"}},
		"a suffix never meets an existing one": {[]string{"o/t", "o/t", "o·t·2"}, []string{"o·t", "o·t·2", "o·t·2·2"}},
		"a shared 24-rune prefix":              {[]string{"a/" + long + "1", "b/" + long + "2"}, []string{"a·" + strings.Repeat("x", 22), "b·" + strings.Repeat("x", 22)}},
		"a shared prefix under one owner": {
			[]string{"o/" + long + "1", "o/" + long + "2", "o/" + long + "3"},
			[]string{"o·" + strings.Repeat("x", 22), "o·" + strings.Repeat("x", 20) + "·2", "o·" + strings.Repeat("x", 20) + "·3"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srcs := sourcesOf(tc.labels...)
			nameWedges(srcs)
			var got []string
			for _, s := range srcs {
				got = append(got, s.wedge)
				if n := utf8.RuneCountInString(s.wedge); n > maxText {
					t.Errorf("%q is %d runes, want at most %d", s.wedge, n, maxText)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("wedges = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTwoClonesOfOneRepoGetDistinctWedgesAndKeys(t *testing.T) {
	l := newLoop(t)
	x := repoAt(t, "x", workerEvents("feat/1-a")...)
	y := repoAt(t, "y", workerEvents("feat/1-a")...)
	l.seed(t, workerEvents("feat/1-a")...)
	origins := map[string]string{
		l.paths.Common: "https://github.com/a/tools.git",
		x.Common:       "https://github.com/b/tools.git",
		y.Common:       "https://github.com/a/tools.git",
	}
	l.opts.Git = &gittest.Fake{Func: func(_ string, args []string) (string, error) {
		if slices.Contains(args, "config") {
			return origins[strings.TrimPrefix(args[0], "--git-dir=")], nil
		}
		return "", nil
	}}
	l.opts.Discover = (&discoverStub{found: okRepos(x, y)}).discover

	fr := l.frame(t)
	if want := []string{"a·tools", "a·tools·2", "b·tools"}; !slices.Equal(wedgeLabels(fr), want) {
		t.Fatalf("wedges = %q, want %q", wedgeLabels(fr), want)
	}
	seen := map[[2]string]bool{}
	for _, p := range fr.sc.planets {
		key := [2]string{p.repoLabel, p.branch}
		if seen[key] {
			t.Errorf("two planets share the (repo, branch) key %q", key)
		}
		seen[key] = true
	}
	if len(seen) != 3 {
		t.Errorf("%d planets, want 3", len(seen))
	}
}
