package adopt

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
)

// stub is the scripted world: which pids live, who parents whom, and what gh,
// git and tmux answer. Every external call is logged with its argv, because the
// arm's calls are part of its contract — a test that only read the messages
// would pass a port that shipped the label off a live branch.
type stub struct {
	calls  []string
	ghOut  string
	ghErr  error
	ghEdit error
	wtOut  string
	wins   string
	self   string
	env    map[string]string
	alive  map[int]bool
	parent map[int]int
	comm   map[int]string
	args   string
	now    time.Time
}

func (s *stub) options() Options {
	return Options{
		Probes: crews.Probes{
			Alive:   func(pid int) bool { return s.alive[pid] },
			Elapsed: func(int) (int64, bool) { return 0, false },
			Mtime:   func(string) (int64, bool) { return 0, false },
			Parent:  func(pid int) (int, bool) { n, ok := s.parent[pid]; return n, ok },
			Comm:    func(pid int) (string, bool) { c, ok := s.comm[pid]; return c, ok },
		},
		Now: func() time.Time { return s.now },
		Gh: func(args ...string) (string, error) {
			s.calls = append(s.calls, "gh "+strings.Join(args, " "))
			return s.ghOut, s.ghErr
		},
		GhQuiet: func(args ...string) error {
			s.calls = append(s.calls, "gh "+strings.Join(args, " "))
			return s.ghEdit
		},
		Worktrees:  func() string { s.calls = append(s.calls, "git worktree list --porcelain"); return s.wtOut },
		Windows:    func() string { s.calls = append(s.calls, "tmux list-windows -a"); return s.wins },
		SelfWindow: func(pane string) string { s.calls = append(s.calls, "tmux display-message "+pane); return s.self },
		Args:       func(int) string { return s.args },
		LookupEnv:  func(k string) string { return s.env[k] },
		PID:        4242,
		PPID:       4241,
		Pwd:        "/repo",
	}
}

func (s *stub) called(want string) bool {
	for _, c := range s.calls {
		if c == want {
			return true
		}
	}
	return false
}

func (s *stub) countCalls(needle string) int {
	n := 0
	for _, c := range s.calls {
		if strings.Contains(c, needle) {
			n++
		}
	}
	return n
}

func testPaths(t *testing.T) bus.Paths {
	t.Helper()
	dir := t.TempDir()
	return bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
}

func writeLog(t *testing.T, p bus.Paths, rows ...string) {
	t.Helper()
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, r := range rows {
		body.WriteString(r + "\n")
	}
	if err := os.WriteFile(p.Log, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func claimRow(crew, issue, branch string, ts int64) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"claim-issue","issue":%q,"branch":%q}`, ts, crew, issue, branch)
}

func statusRow(ts int64, crew string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"status","from":"working","to":"pr_open"}`, ts, crew)
}

// registerCrew is `crew register <pid>`: the state a dispatcher leaves when it
// exits without deregistering.
func registerCrew(t *testing.T, p bus.Paths, id, pid string) {
	t.Helper()
	dir := p.CrewDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/pid", []byte(pid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPid(t *testing.T, p bus.Paths, id string) string {
	t.Helper()
	data, err := os.ReadFile(p.CrewDir(id) + "/pid")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(data), "\n")
}

func pidfileLog(t *testing.T, p bus.Paths) string {
	t.Helper()
	data, err := os.ReadFile(p.Dir + "/pidfile.log")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(data), "\n")
}

type result struct {
	code   int
	stdout string
	stderr string
}

func run(t *testing.T, s *stub, p bus.Paths, args ...string) result {
	t.Helper()
	var out, errOut strings.Builder
	code := Run(args, p, &out, &errOut, s.options())
	return result{code, out.String(), errOut.String()}
}

func TestUsageAndInvalidIDs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no id", nil, "crew: adopt [--force] <id> [pid]"},
		{"empty id", []string{""}, "crew: adopt [--force] <id> [pid]"},
		{"path escape", []string{"../../etc"}, "crew: invalid crew id"},
		{"leading dash", []string{"-x"}, "crew: invalid crew id"},
		{"dot", []string{"."}, "crew: invalid crew id"},
		{"shell metacharacters", []string{"c; rm -rf /"}, "crew: invalid crew id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stub{now: time.Unix(1700000000, 0)}
			got := run(t, s, testPaths(t), tc.args...)
			if got.code != 1 || got.stdout != "" {
				t.Errorf("code = %d, stdout = %q", got.code, got.stdout)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.want)
			}
		})
	}
}

func TestUnknownCrew(t *testing.T) {
	s := &stub{now: time.Unix(1700000000, 0)}
	got := run(t, s, testPaths(t), "c-nope", "123")
	if got.code != 1 || got.stdout != "" {
		t.Fatalf("code = %d, stdout = %q", got.code, got.stdout)
	}
	want := "crew: no crew 'c-nope' in this repo — run 'crew crews' to list them, or 'crew new' to start one\n"
	if got.stderr != want {
		t.Errorf("stderr = %q, want %q", got.stderr, want)
	}
	if len(s.calls) != 0 {
		t.Errorf("calls = %v, want none", s.calls)
	}
}

// A restart can take the crew directory with it; the id still has to appear in
// the log for adopt to accept it rather than invent a crew.
func TestKnownFromTheLogAlone(t *testing.T) {
	p := testPaths(t)
	writeLog(t, p, statusRow(1700000000000, "c-log"))
	s := &stub{now: time.Unix(1700000000, 0)}
	got := run(t, s, p, "c-log", "123")
	if got.code != 0 || got.stdout != "c-log\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", got.code, got.stdout, got.stderr)
	}
	if readPid(t, p, "c-log") != "123" {
		t.Errorf("pid file = %q, want 123", readPid(t, p, "c-log"))
	}
}

func TestRefusesALiveDispatcher(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-live", "4321")
	s := &stub{now: time.Unix(1700000000, 0), alive: map[int]bool{4321: true}}
	got := run(t, s, p, "c-live", "123")
	if got.code != 1 || got.stdout != "" {
		t.Fatalf("code = %d, stdout = %q", got.code, got.stdout)
	}
	want := "crew: crew 'c-live' still has a live dispatcher — 'crew new' starts your own; '--force' overrides if that process is a stale pid reuse\n"
	if got.stderr != want {
		t.Errorf("stderr = %q, want %q", got.stderr, want)
	}
	if readPid(t, p, "c-live") != "4321" {
		t.Errorf("the refusal rewrote the pid file: %q", readPid(t, p, "c-live"))
	}
	if line := pidfileLog(t, p); !strings.Contains(line, " adopt refused crew=c-live old=4321 new=123 ") {
		t.Errorf("pidfile.log = %q", line)
	}
}

// `dispatch` adopts the crew it just made on every retry, so a live owner that
// is this process's ancestor is the expected case, not a refusal.
func TestLiveAncestorIsIdempotent(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-own", "4321")
	s := &stub{
		now:    time.Unix(1700000000, 0),
		alive:  map[int]bool{4321: true},
		parent: map[int]int{os.Getpid(): 4321},
	}
	got := run(t, s, p, "c-own", "123")
	if got.code != 0 || got.stdout != "c-own\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", got.code, got.stdout, got.stderr)
	}
	if readPid(t, p, "c-own") != "123" {
		t.Errorf("pid file = %q, want 123", readPid(t, p, "c-own"))
	}
	if line := pidfileLog(t, p); !strings.Contains(line, " adopt ok ") {
		t.Errorf("pidfile.log = %q", line)
	}
}

func TestForceOverridesALiveOwner(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-live", "4321")
	writeLog(t, p, claimRow("c-live", "55", "feat/55-z", 100))
	s := &stub{now: time.Unix(1700000000, 0), alive: map[int]bool{4321: true}}
	got := run(t, s, p, "c-live", "--force", "123")
	if got.code != 0 || got.stdout != "c-live\n" {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", got.code, got.stdout, got.stderr)
	}
	if line := pidfileLog(t, p); !strings.Contains(line, " adopt forced ") {
		t.Errorf("pidfile.log = %q", line)
	}
	// --force reclaims the pid slot; it must not also strip a label the live
	// crew is still working under.
	if len(s.calls) != 0 {
		t.Errorf("force over a live owner made external calls: %v", s.calls)
	}
}

// `--force` is stripped from anywhere in the argv, so it never reaches the pid
// slot, and the pid defaults to the owner walk: $PPID is the shell crew.sh ran
// under, so the walk climbs past it to the first ancestor that is not a shell.
func TestForceIsNotWrittenAsThePid(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-force", "4321")
	s := &stub{
		now:    time.Unix(1700000000, 0),
		parent: map[int]int{4241: 4240, 4240: 4239},
		comm:   map[int]string{4241: "bash", 4240: "-/run/bin/zsh", 4239: "claude"},
	}
	if got := run(t, s, p, "c-force", "--force"); got.code != 0 {
		t.Fatalf("code = %d, stderr = %q", got.code, got.stderr)
	}
	if pid := readPid(t, p, "c-force"); pid != "4239" {
		t.Errorf("pid file = %q, want the first non-shell ancestor", pid)
	}
	// A process ps cannot name ends the walk at the direct parent.
	s.comm = nil
	run(t, s, p, "c-force")
	if pid := readPid(t, p, "c-force"); pid != "4241" {
		t.Errorf("pid file = %q, want the fallback parent", pid)
	}
}

func TestPidfileLogLine(t *testing.T) {
	p := testPaths(t)
	by := strings.Repeat("a", 110) + " 日本語テキストあいう"
	s := &stub{now: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), args: by}
	registerCrew(t, p, "c-new", "")
	if got := run(t, s, p, "c-new", "123"); got.code != 0 {
		t.Fatalf("code = %d, stderr = %q", got.code, got.stderr)
	}
	want := "2026-03-04T05:06:07Z adopt ok crew=c-new old=- new=123 pid=4242 ppid=4241 cwd=/repo by=" +
		string([]rune(by)[:120])
	if line := pidfileLog(t, p); line != want {
		t.Errorf("pidfile.log =\n%s\nwant\n%s", line, want)
	}
	if strings.Contains(pidfileLog(t, p), string([]rune(by)[120:])) {
		t.Error("by= was not capped at 120 characters")
	}
}

// A dead owner is the state the claim release exists for, and stdout stays the
// bare id so `CREW_ID=$(crew adopt …)` keeps working.
func TestReleasesADeadCrewsClaim(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-dead", "4321")
	writeLog(t, p,
		statusRow(1, "c-dead"),
		claimRow("c-dead", "83", "feat/83-foo", 100),
		claimRow("c-other", "99", "feat/99-b", 100),
	)
	s := &stub{now: time.Unix(1700000000, 0)}
	got := run(t, s, p, "c-dead", "123")
	if got.stdout != "c-dead\n" {
		t.Errorf("stdout = %q, want the bare id", got.stdout)
	}
	if got.stderr != "crew adopt: released the dispatched label on #83 (feat/83-foo)\n" {
		t.Errorf("stderr = %q", got.stderr)
	}
	if !s.called("gh issue edit 83 --remove-label dispatched") {
		t.Errorf("calls = %v", s.calls)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "99") {
			t.Errorf("another crew's claim was touched: %q", c)
		}
	}
}

func TestNoClaimRowsMeansNoGhCalls(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-dead", "4321")
	writeLog(t, p, statusRow(1, "c-dead"))
	s := &stub{now: time.Unix(1700000000, 0)}
	if got := run(t, s, p, "c-dead", "123"); got.code != 0 {
		t.Fatalf("code = %d, stderr = %q", got.code, got.stderr)
	}
	if len(s.calls) != 0 {
		t.Errorf("calls = %v, want none", s.calls)
	}
}

// The newest claim per issue wins across every crew before this crew is even
// considered, and a numeric 73 groups with a string "73" because `gh issue
// edit` treats them as one issue.
func TestNewestClaimWinsAcrossCrews(t *testing.T) {
	cases := []struct {
		name    string
		issue   string
		rows    []string
		release bool
	}{
		{
			name:  "re-taken by another crew",
			issue: "77",
			rows: []string{
				claimRow("c-dead", "77", "feat/77-old", 100),
				claimRow("c-live-other", "77", "feat/77-new", 200),
			},
		},
		{
			name:  "a numeric and a string issue are one issue",
			issue: "73",
			rows: []string{
				`{"ts":100,"crew_id":"c-dead","kind":"claim-issue","issue":73,"branch":"feat/73-old"}`,
				claimRow("c-live-other", "73", "feat/73-new", 200),
			},
		},
		{
			name:  "this crew re-took it",
			issue: "78",
			rows: []string{
				claimRow("c-other", "78", "feat/78-old", 100),
				claimRow("c-dead", "78", "feat/78-new", 200),
			},
			release: true,
		},
		{
			// The empty-branch row is filtered before the grouping, so the older
			// row is still the newest claim on the issue.
			name:  "a newer row with no branch is not a claim",
			issue: "79",
			rows: []string{
				claimRow("c-dead", "79", "feat/79-a", 100),
				claimRow("c-other", "79", "", 200),
			},
			release: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			registerCrew(t, p, "c-dead", "4321")
			writeLog(t, p, tc.rows...)
			s := &stub{now: time.Unix(1700000000, 0)}
			run(t, s, p, "c-dead", "123")
			want := fmt.Sprintf("gh issue edit %s --remove-label dispatched", tc.issue)
			if got := s.called(want); got != tc.release {
				t.Errorf("calls = %v, want %q released = %v", s.calls, want, tc.release)
			}
		})
	}
}

// One issue claimed on two branches: the safety gates look at every branch the
// issue was claimed on, not only the newest row's.
func TestEveryClaimedBranchIsGated(t *testing.T) {
	wt := t.TempDir()
	cases := []struct {
		name    string
		issue   string
		own     string
		other   string
		wtOut   string
		wins    string
		ghOut   string
		wantMsg string
	}{
		{
			name:    "this crew's branch heads an open PR",
			issue:   "83",
			own:     "feat/83-a",
			other:   "feat/83-b",
			ghOut:   "feat/83-a\n",
			wantMsg: "crew adopt: keeping #83 — feat/83-a heads an open PR",
		},
		{
			name:    "the other crew's branch heads an open PR",
			issue:   "83",
			own:     "feat/83-a",
			other:   "feat/83-b",
			ghOut:   "feat/83-b\n",
			wantMsg: "crew adopt: keeping #83 — feat/83-b heads an open PR",
		},
		{
			name:    "a worker occupies the other crew's worktree",
			issue:   "84",
			own:     "feat/84-a",
			other:   "feat/84-b",
			wtOut:   "worktree " + wt + "\nHEAD 1\nbranch refs/heads/feat/84-b\n\n",
			wins:    "@1\tsage\t" + wt,
			wantMsg: "crew adopt: keeping #84 — a worker still occupies feat/84-b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			registerCrew(t, p, "c-dead", "4321")
			writeLog(t, p,
				claimRow("c-live-other", tc.issue, tc.other, 50),
				claimRow("c-dead", tc.issue, tc.own, 100),
			)
			s := &stub{now: time.Unix(1700000000, 0), ghOut: tc.ghOut, wtOut: tc.wtOut, wins: tc.wins}
			got := run(t, s, p, "c-dead", "123")
			if !strings.Contains(got.stderr, tc.wantMsg) {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.wantMsg)
			}
			if s.called(fmt.Sprintf("gh issue edit %s --remove-label dispatched", tc.issue)) {
				t.Errorf("the label was stripped anyway: %v", s.calls)
			}
		})
	}
}

// A worktree path is matched exactly, and a window is an occupant only when it
// is crewed, is not the dispatcher's, and is not the caller's own.
func TestOccupancyRules(t *testing.T) {
	cases := []struct {
		name string
		wins string
		self string
		pane string
		want bool
	}{
		{"no windows", "", "", "", false},
		{"uncrewed window", "@1\t\t/wt", "", "", false},
		{"dispatcher window", "@1\tdispatcher\t/wt", "", "", false},
		{"another crew's window", "@1\tsage\t/wt", "", "", true},
		{"the caller's own window", "@1\tsage\t/wt", "@1", "%1", false},
		{"no TMUX_PANE to compare against", "@1\tsage\t/wt", "", "", true},
		{"another path", "@1\tsage\t/elsewhere", "", "", false},
		{"a longer path", "@1\tsage\t/wt-x", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stub{wins: tc.wins, self: tc.self, env: map[string]string{"TMUX_PANE": tc.pane}}
			if got := s.options().occupied("/wt"); got != tc.want {
				t.Errorf("occupied = %v, want %v", got, tc.want)
			}
		})
	}
}

// The caller's own window is only asked for inside tmux, and it is asked before
// the window list, as the helper runs them.
func TestSelfWindowOnlyInsideTmux(t *testing.T) {
	s := &stub{wins: "@1\tsage\t/wt"}
	if !s.options().occupied("/wt") {
		t.Fatal("expected an occupant")
	}
	if s.countCalls("display-message") != 0 {
		t.Errorf("calls = %v, want no display-message without TMUX_PANE", s.calls)
	}
	if !s.called("tmux list-windows -a") {
		t.Errorf("calls = %v", s.calls)
	}
}

func TestGhPrListFailureLeavesClaimsInPlace(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-dead", "4321")
	writeLog(t, p, claimRow("c-dead", "83", "feat/83-foo", 100))
	s := &stub{now: time.Unix(1700000000, 0), ghOut: "gh: could not resolve host", ghErr: errors.New("exit status 1")}
	got := run(t, s, p, "c-dead", "123")
	if got.code != 0 || got.stdout != "c-dead\n" {
		t.Fatalf("a failed claim release never fails the adopt: code = %d, stdout = %q", got.code, got.stdout)
	}
	want := "crew adopt: could not list open PRs (gh: could not resolve host) — leaving the recorded claims in place\n"
	if got.stderr != want {
		t.Errorf("stderr = %q, want %q", got.stderr, want)
	}
	if s.countCalls("issue edit") != 0 {
		t.Errorf("calls = %v", s.calls)
	}
}

func TestLabelRemovalFailureIsReported(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-dead", "4321")
	writeLog(t, p, claimRow("c-dead", "83", "feat/83-foo", 100))
	s := &stub{now: time.Unix(1700000000, 0), ghEdit: errors.New("exit status 1")}
	got := run(t, s, p, "c-dead", "123")
	want := "crew adopt: could not remove the dispatched label from #83 (feat/83-foo)\n"
	if got.stderr != want {
		t.Errorf("stderr = %q, want %q", got.stderr, want)
	}
}

// The issue reaches `gh issue edit` as one argv word, but the bus is
// caller-writable, so a malformed row is reported and skipped while the
// well-formed rows beside it still release.
func TestMalformedIssueIsSkipped(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-dead", "4321")
	writeLog(t, p,
		claimRow("c-dead", "90; rm -rf .", "feat/bad", 100),
		claimRow("c-dead", "91", "feat/91-ok", 100),
	)
	s := &stub{now: time.Unix(1700000000, 0)}
	got := run(t, s, p, "c-dead", "123")
	want := "crew adopt: skipping a claim row whose issue is not a number (90; rm -rf .)\n"
	if !strings.Contains(got.stderr, want) {
		t.Errorf("stderr = %q, want %q", got.stderr, want)
	}
	if !s.called("gh issue edit 91 --remove-label dispatched") {
		t.Errorf("calls = %v", s.calls)
	}
	for _, c := range s.calls {
		if strings.Contains(c, "rm -rf") {
			t.Errorf("a malformed issue reached gh: %q", c)
		}
	}
}

// Claims are released only once the pid file is on disk: a crash between the
// two would leave the labels gone and the crew unadopted.
func TestClaimsReleaseAfterThePidFileIsWritten(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-dead", "4321")
	writeLog(t, p, claimRow("c-dead", "83", "feat/83-foo", 100))
	s := &stub{now: time.Unix(1700000000, 0)}
	o := s.options()
	pidAtCall := ""
	o.GhQuiet = func(args ...string) error {
		pidAtCall = readPid(t, p, "c-dead")
		return nil
	}
	var out, errOut strings.Builder
	Run([]string{"c-dead", "123"}, p, &out, &errOut, o)
	if pidAtCall != "123" {
		t.Errorf("pid file at the gh call = %q, want it already rewritten", pidAtCall)
	}
}

// A corrupt log is the arm's `jq -s … 2>/dev/null || true`: no claims, and no
// message about it.
func TestCorruptLogMeansNoClaims(t *testing.T) {
	p := testPaths(t)
	registerCrew(t, p, "c-dead", "4321")
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Log, []byte("not json at all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &stub{now: time.Unix(1700000000, 0)}
	if got := run(t, s, p, "c-dead", "123"); got.code != 0 || got.stderr != "" {
		t.Fatalf("code = %d, stderr = %q", got.code, got.stderr)
	}
	if len(s.calls) != 0 {
		t.Errorf("calls = %v, want none", s.calls)
	}
}

// The claim fold is the arm's jq program, so its grouping, its ordering and the
// branches it hands the gates are pinned here rather than only through Run.
func TestClaimRows(t *testing.T) {
	p := testPaths(t)
	writeLog(t, p,
		claimRow("c-dead", "7", "feat/7-a", 100),
		claimRow("c-other", "7", "feat/7-b", 50),
		claimRow("c-dead", "10", "feat/10-a", 120),
		claimRow("c-other", "10", "feat/10-b", 200),
		`{"ts":300,"crew_id":"c-dead","kind":"claim-issue","issue":"11","branch":""}`,
		`{"ts":300,"crew_id":"c-dead","kind":"claim-issue","issue":null,"branch":"feat/12"}`,
	)
	rows := claimRows(p, "c-dead")
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only issue 7", rows)
	}
	got := rows[0]
	if got.issue != "7" || got.branch != "feat/7-a" {
		t.Errorf("row = %+v", got)
	}
	// The other crew's older branch rides along for the gates.
	if want := "feat/7-a,feat/7-b"; strings.Join(got.branches, ",") != want {
		t.Errorf("branches = %v, want %v", got.branches, want)
	}
}

func TestHelpers(t *testing.T) {
	if got := firstRunes("héllo wörld", 5); got != "héllo" {
		t.Errorf("firstRunes = %q", got)
	}
	if got := firstRunes("héllo", 10); got != "héllo" {
		t.Errorf("firstRunes = %q", got)
	}
	for _, tc := range []struct{ line, wid, nm, path string }{
		{"@1\tsage\t/wt", "@1", "sage", "/wt"},
		{"@1\t\t/wt", "@1", "/wt", ""},
		{"@1", "@1", "", ""},
		{"\tsage\t/wt", "sage", "/wt", ""},
		{"@1\tsage\t/wt\textra", "@1", "sage", "/wt\textra"},
		{"@1\tsage\t/wt\t", "@1", "sage", "/wt"},
		{"", "", "", ""},
	} {
		if wid, nm, path := read3(tc.line); wid != tc.wid || nm != tc.nm || path != tc.path {
			t.Errorf("read3(%q) = %q, %q, %q; want %q, %q, %q", tc.line, wid, nm, path, tc.wid, tc.nm, tc.path)
		}
	}
	if hasLine("feat/83\nfeat/84-x", "feat/84") {
		t.Error("hasLine matched a prefix")
	}
	if !hasLine("feat/83\nfeat/84", "feat/84") {
		t.Error("hasLine missed a whole line")
	}
	for _, bad := range []string{"", "0x1", "1a", "-1", "1.0", " 1"} {
		if isNumber(bad) {
			t.Errorf("isNumber(%q) = true", bad)
		}
	}
	for _, good := range []string{"1", "007", "91"} {
		if !isNumber(good) {
			t.Errorf("isNumber(%q) = false", good)
		}
	}
}
