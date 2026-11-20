package overseer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const (
	trustPrefix = "crew: overseer trust: "
	panePath    = "/work/r"
)

func readFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(b), "\n")
}

// trustCase is one machine for `crew overseer trust %7`: a registered repo
// whose bus holds crew c1, and a local pane %7 at panePath that tmux reports
// as cmd with the role and crew stamps given, showing frames[0] before the
// Enter and the later frames after it, the last one repeated.
type trustCase struct {
	*fixture
	common   string
	cmd      string
	role     string
	crew     string
	frames   []string
	accept   func(context.Context) (bool, error)
	gone     bool // tmux reports pane %7 as gone
	path     string
	notRepo  bool  // the pane's cwd has no git common dir
	capErr   error // capture-pane fails
	sendErr  error // send-keys fails
	tmuxPane *tmuxtest.Fake
	captures int
}

func newTrustCase(t *testing.T) *trustCase {
	t.Helper()
	f := newFixture(t)
	tc := &trustCase{
		fixture: f,
		common:  f.repo("r", ""),
		cmd:     "claude",
		crew:    "c1",
		path:    panePath,
		frames: []string{
			readFixture(t, "../frame/testdata/frames/prompt_trust.txt"),
			readFixture(t, "../frame/testdata/frames/idle_box.txt"),
		},
		accept: func(context.Context) (bool, error) { return true, nil },
	}
	tc.tmuxPane = &tmuxtest.Fake{Func: tc.answer}
	return tc
}

func (tc *trustCase) answer(args []string) (string, error) {
	switch args[0] {
	case "display-message":
		if tc.gone {
			return "", errors.New("can't find pane: %7")
		}
		return strings.Join([]string{"%7", tc.cmd, tc.path, tc.role, tc.crew}, "\x1f"), nil
	case "capture-pane":
		if tc.capErr != nil {
			return "", tc.capErr
		}
		frame := tc.frames[min(tc.captures, len(tc.frames)-1)]
		tc.captures++
		return frame, nil
	case "send-keys":
		return "", tc.sendErr
	}
	return "", tmuxtest.ErrNoServer
}

func (tc *trustCase) sentEnter() bool {
	for _, c := range tc.tmuxPane.Calls() {
		if strings.HasPrefix(c, "send-keys") {
			return true
		}
	}
	return false
}

// dispatcherPane writes crews/<crew>/pane naming pane in the repo's crew dir.
func (tc *trustCase) dispatcherPane(crew, pane string) {
	tc.t.Helper()
	dir := tc.common + "/crew/crews/" + crew
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tc.t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/pane", []byte(pane+"\n"), 0o644); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *trustCase) opts() Options {
	o := tc.fixture.opts()
	o.Clock.CrewClock = tc.root + "/clock"
	o.Git = &gittest.Fake{Func: func(dir string, args []string) (string, error) {
		if dir == panePath && !tc.notRepo && strings.Join(args, " ") == "rev-parse --path-format=absolute --git-common-dir" {
			return tc.common, nil
		}
		return "", &git.ExitError{Code: 128}
	}}
	o.Tmux = tc.tmuxPane
	o.AcceptTrust = tc.accept
	return o
}

func (tc *trustCase) run(pane string) (int, string, string) {
	tc.t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunTrust(tc.t.Context(), pane, &stdout, &stderr, tc.opts())
	return code, stdout.String(), stderr.String()
}

// typed is the send-keys calls the pane fake saw.
func (tc *trustCase) typed() []string {
	var keys []string
	for _, c := range tc.tmuxPane.Calls() {
		if strings.HasPrefix(c, "send-keys") {
			keys = append(keys, c)
		}
	}
	return keys
}

type trustRow struct {
	CrewID string `json:"crew_id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Body   string `json:"body"`
}

// newRows is the rows the run appended to the repo's log.
func (tc *trustCase) newRows(before string) []trustRow {
	tc.t.Helper()
	after := tc.logs()[tc.common]
	if !strings.HasPrefix(after, before) {
		tc.t.Fatalf("the log was rewritten:\nbefore %q\nafter  %q", before, after)
	}
	var rows []trustRow
	for line := range strings.Lines(strings.TrimPrefix(after, before)) {
		var r trustRow
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			tc.t.Fatalf("row %q: %v", line, err)
		}
		rows = append(rows, r)
	}
	return rows
}

// noticeRow is the row RunTrust posts to crew's dispatcher with detail.
func noticeRow(crew, detail string) trustRow {
	return trustRow{CrewID: crew, From: "overseer:h1", To: "dispatcher:" + crew, Kind: "msg", Body: `{"kind":"notice","detail":"` + detail + `"}`}
}

func TestTrustAccepts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*trustCase)
		crew  string
	}{
		{"crew from the @crew_id stamp", func(*trustCase) {}, "c1"},
		{"crew from a dispatcher pane file", func(tc *trustCase) {
			tc.crew = ""
			tc.dispatcherPane("c2", "%7")
			tc.dispatcherPane("c3", "%8")
		}, "c2"},
		{"nix-wrapped engine command", func(tc *trustCase) { tc.cmd = ".claude-wrapped" }, "c1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTrustCase(t)
			tt.setup(tc)
			before := tc.logs()[tc.common]
			code, stdout, stderr := tc.run("%7")
			if code != 0 || stderr != "" || stdout != "accepted the workspace-trust dialog in pane %7\n" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if keys := tc.typed(); len(keys) != 1 || keys[0] != "send-keys -t %7 Enter" {
				t.Fatalf("typed %q, want one Enter", keys)
			}
			want := noticeRow(tt.crew, "accepted the workspace-trust dialog in pane %7")
			if rows := tc.newRows(before); len(rows) != 1 || rows[0] != want {
				t.Fatalf("rows %+v, want one %+v", rows, want)
			}
		})
	}
}

func TestTrustStillShownTypesOnceAndTells(t *testing.T) {
	tc := newTrustCase(t)
	tc.frames = tc.frames[:1]
	before := tc.logs()[tc.common]
	code, stdout, stderr := tc.run("%7")
	if code != 3 || stdout != "" || stderr != trustPrefix+"typed Enter, workspace-trust dialog still shown in pane %7\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if keys := tc.typed(); len(keys) != 1 {
		t.Fatalf("typed %q, want one Enter and no retry", keys)
	}
	if tc.captures != trustPolls+1 {
		t.Errorf("%d captures, want the gate's and %d polls", tc.captures, trustPolls)
	}
	want := noticeRow("c1", "typed Enter, workspace-trust dialog still shown in pane %7")
	if rows := tc.newRows(before); len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows %+v, want one %+v", rows, want)
	}
}

// TestTrustNoticeFailure: the sender probe fails once the keys are typed, so
// the notice cannot be posted.
func TestTrustNoticeFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		frames int
		code   int
		lines  int
	}{
		{"accepted becomes 4", 2, 4, 1},
		{"still shown stays 3", 1, 3, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTrustCase(t)
			tc.frames = tc.frames[:tt.frames]
			tc.tmux.Func = func(args []string) (string, error) {
				if tc.sentEnter() {
					return "", errors.New("tmux gone")
				}
				return "", nil
			}
			before := tc.logs()[tc.common]
			code, stdout, stderr := tc.run("%7")
			lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
			last := lines[len(lines)-1]
			if code != tt.code || stdout != "" || len(lines) != tt.lines ||
				!strings.HasPrefix(last, trustPrefix+"typed Enter in pane %7, but the notice to dispatcher:c1 failed (crew: overseer msg: ") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
			if rows := tc.newRows(before); len(rows) != 0 {
				t.Fatalf("rows %+v, want none", rows)
			}
		})
	}
}

func TestTrustRefusals(t *testing.T) {
	trust := readFixture(t, "../frame/testdata/frames/prompt_trust.txt")
	for _, tt := range []struct {
		name  string
		pane  string
		setup func(*trustCase)
		code  int
		want  string // the stderr line after the prefix; "" checks the prefix alone
	}{
		{"not the overseer", "%7", func(tc *trustCase) { tc.env["CREW_OVERSEER"] = "" }, 2, "refusing sender 'overseer:h1' — "},
		{"host name unreadable", "%7", func(tc *trustCase) {
			tc.self = func() (string, error) { return "", errors.New("no uname") }
		}, 1, "this host's name cannot be read (no uname)"},
		{"setting off", "%7", func(tc *trustCase) {
			tc.accept = func(context.Context) (bool, error) { return false, nil }
		}, 2, "overseer.acceptTrust is off — the owner enables it"},
		{"settings unreadable", "%7", func(tc *trustCase) {
			tc.accept = func(context.Context) (bool, error) { return false, errors.New("dispatch-config: exit status 3") }
		}, 1, "overseer.acceptTrust cannot be read (dispatch-config: exit status 3)"},
		{"bad pane id", "7", func(*trustCase) {}, 1, `"7" is not a pane id (%N)`},
		{"pane id with a target suffix", "%7.1", func(*trustCase) {}, 1, `"%7.1" is not a pane id (%N)`},
		{"missing pane", "%7", func(tc *trustCase) { tc.gone = true }, 1, "no pane %7 on this host's tmux server"},
		{"overseer pane", "%7", func(tc *trustCase) { tc.role = "overseer"; tc.crew = "" }, 2, "pane %7 is the overseer's own pane"},
		{"non-engine command", "%7", func(tc *trustCase) { tc.cmd = "fish" }, 2, "pane %7 is not running an engine (fish)"},
		{"unresolvable crew", "%7", func(tc *trustCase) {
			tc.crew = ""
			tc.dispatcherPane("c2", "%8")
		}, 1, "pane %7 has no @crew_id stamp, and no crew of " + "COMMON names it as its dispatcher pane"},
		{"two crews name the pane", "%7", func(tc *trustCase) {
			tc.crew = ""
			tc.dispatcherPane("c2", "%7")
			tc.dispatcherPane("c3", "%7")
		}, 1, "pane %7 has no @crew_id stamp, and crews c2, c3 all name it as their dispatcher pane"},
		{"crew not on the bus", "%7", func(tc *trustCase) { tc.crew = "c9" }, 1, "crew c9 is not on COMMON's bus"},
		{"capture fails", "%7", func(tc *trustCase) { tc.capErr = errors.New("capture boom") }, 1, "pane %7 cannot be captured"},
		{"pane cwd not in a git repo", "%7", func(tc *trustCase) { tc.notRepo = true }, 1, "pane %7's directory " + panePath + " is not in a git repo"},
		{"pane cwd not absolute", "%7", func(tc *trustCase) { tc.path = "relative/dir" }, 1, "pane %7 has no working directory"},
		{"add-dir frame", "%7", func(tc *trustCase) {
			tc.frames = []string{readFixture(t, "../stall/testdata/prompt_adddir.txt")}
		}, 2, "add-dir trust dialog rejects injected keys — relay it to the human"},
		{"permission frame", "%7", func(tc *trustCase) {
			tc.frames = []string{readFixture(t, "../frame/testdata/frames/permission_bash.txt")}
		}, 2, "pane %7 does not show the workspace-trust dialog with '1. Yes, I trust this folder' selected"},
		{"idle frame", "%7", func(tc *trustCase) {
			tc.frames = []string{readFixture(t, "../frame/testdata/frames/idle_box.txt")}
		}, 2, "pane %7 does not show the workspace-trust dialog with '1. Yes, I trust this folder' selected"},
		{"cursor on No", "%7", func(tc *trustCase) {
			tc.frames = []string{strings.Replace(strings.Replace(trust, "> 1. Yes", "  1. Yes", 1), "2. No, exit", "> 2. No, exit", 1)}
		}, 2, "pane %7 does not show the workspace-trust dialog with '1. Yes, I trust this folder' selected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newTrustCase(t)
			tt.setup(tc)
			before := tc.logs()
			code, stdout, stderr := tc.run(tt.pane)
			want := trustPrefix + strings.ReplaceAll(tt.want, "COMMON", tc.common)
			if code != tt.code || stdout != "" || !strings.HasPrefix(stderr, want) || strings.Count(stderr, "\n") != 1 {
				t.Errorf("exit %d, stdout %q, stderr %q; want %d and one line %q…", code, stdout, stderr, tt.code, want)
			}
			if keys := tc.typed(); len(keys) != 0 {
				t.Errorf("typed %q on a refusal", keys)
			}
			if after := tc.logs(); !maps.Equal(after, before) {
				t.Errorf("a refusal changed a log:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

// TestTrustSendKeysFailure: a failed send-keys is a refusal that writes no row.
func TestTrustSendKeysFailure(t *testing.T) {
	tc := newTrustCase(t)
	tc.sendErr = errors.New("send boom")
	before := tc.logs()
	code, stdout, stderr := tc.run("%7")
	if want := trustPrefix + "send-keys to pane %7 failed\n"; code != 2 || stdout != "" || stderr != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want 2 and %q", code, stdout, stderr, want)
	}
	if after := tc.logs(); !maps.Equal(after, before) {
		t.Errorf("a refusal changed a log:\nbefore %q\nafter  %q", before, after)
	}
}

// TestTrustSenderRefusalReadsNothing: a caller that is not the overseer gets
// no settings read and no pane read, only the sender probes' own lookups.
func TestTrustSenderRefusalReadsNothing(t *testing.T) {
	tc := newTrustCase(t)
	tc.env["CREW_OVERSEER"] = ""
	read := false
	tc.accept = func(context.Context) (bool, error) { read = true; return true, nil }
	if code, _, _ := tc.run("%7"); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if calls := tc.tmuxPane.Calls(); len(calls) != 0 || read {
		t.Fatalf("pane calls %q, settings read %v; want neither", calls, read)
	}
}
