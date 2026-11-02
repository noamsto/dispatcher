package sender

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// pane is one `tmux list-panes -a -F Format` line.
func pane(pid, branch, role, crewID, win, id string) string {
	return strings.Join([]string{pid, branch, role, crewID, win, id}, "\x1f") + "\n"
}

// self is the caller's own pid, the foot of the fixtures' ancestry.
var self = os.Getpid()

// tree is the caller's ancestry: self → 200 → 100 → 1.
var tree = map[int]int{self: 200, 200: 100, 100: 1}

type fixture struct {
	env      map[string]string
	panes    string
	panesErr bool
	noPanes  bool
	noParent bool
	selfErr  bool
	noSelf   bool
	tree     map[int]int
}

func (f fixture) probes() Probes {
	parents := f.tree
	if parents == nil {
		parents = tree
	}
	p := Probes{
		Getenv: func(k string) string { return f.env[k] },
		Tmux:   &tmuxtest.Fake{Out: map[string]string{"list-panes": f.panes}},
		Proc:   proctest.New(nil, parents),
		Self:   func() (string, error) { return "h1", nil },
	}
	if f.selfErr {
		p.Self = func() (string, error) { return "", errors.New("boom") }
	}
	if f.noSelf {
		p.Self = nil
	}
	if f.panesErr {
		p.Tmux = &tmuxtest.Fake{}
	}
	if f.noPanes {
		p.Tmux = nil
	}
	if f.noParent {
		p.Proc = nil
	}
	return p
}

const (
	tripwire = "this is a worker or role session (CREW_WORKER_ID or CREW_ROLE_ID is set) and only the dispatcher posts as dispatcher:<crew>; post as your own id ($CREW_WORKER_ID for a lead, role:<branch>:<role> for a grid role)"
	leadX    = "this pane is stamped as the lead of feat/x, which posts only as worker:feat/x or worker:feat/x#s<session> ($CREW_WORKER_ID)"
	roleX    = "this pane is stamped as role:feat/x:reviewer, the only id it posts as"

	markedOnly      = "this is the overseer session (CREW_OVERSEER is set), which posts only as overseer:<host> through crew overseer msg"
	overseerOnly    = "only the overseer session (CREW_OVERSEER set to this host's name) posts as overseer:<host>"
	overseerIDs     = "this is a worker or role session (CREW_WORKER_ID or CREW_ROLE_ID is set) and only the overseer session posts as overseer:<host>; post as your own id ($CREW_WORKER_ID for a lead, role:<branch>:<role> for a grid role)"
	overseerProbe   = "the pane stamps cannot be read, and only a session outside every crew pane posts as overseer:h1"
	overseerStamped = "this pane is stamped as the overseer's (@crew_role overseer), which posts only as overseer:h1"
	notH1           = "the overseer session on h1 posts only as overseer:h1, not "
)

func TestCheck(t *testing.T) {
	worker := map[string]string{"CREW_WORKER_ID": "worker:feat/x#s1-2"}
	role := map[string]string{"CREW_ROLE_ID": "role:feat/x:reviewer"}
	lead := pane("100", "feat/x", "lead", "c1", "@1", "%1")
	leadNoRole := pane("100", "feat/x", "", "c1", "@1", "%1")
	reviewer := pane("100", "feat/x", "reviewer", "c1", "@1", "%1")
	dispatcherPane := pane("100", "", "", "", "@1", "%1")
	marked := map[string]string{"CREW_OVERSEER": "h1"}
	markedWorker := map[string]string{"CREW_OVERSEER": "h1", "CREW_WORKER_ID": "worker:feat/x#s1-2"}
	markedRole := map[string]string{"CREW_OVERSEER": "h1", "CREW_ROLE_ID": "role:feat/x:reviewer"}
	overseerPane := pane("100", "", "overseer", "", "@1", "%1")

	tests := []struct {
		name string
		from string
		crew string
		f    fixture
		want string
	}{
		{"tripwire worker env", "dispatcher:c1", "c1", fixture{env: worker}, tripwire},
		{"tripwire role env", "dispatcher:c1", "c1", fixture{env: role}, tripwire},
		{"tripwire beats an unstamped anchor", "dispatcher:c1", "c1", fixture{env: worker, panes: dispatcherPane}, tripwire},
		{"no env no anchor allows dispatcher", "dispatcher:c1", "c1", fixture{}, ""},
		{"tripwire ignores worker sender", "worker:feat/x", "c1", fixture{env: worker}, ""},
		{"tripwire ignores role sender", "role:feat/x:reviewer", "c1", fixture{env: role}, ""},

		{"lead allows own branch", "worker:feat/x", "c1", fixture{panes: lead}, ""},
		{"lead allows own session", "worker:feat/x#s1-2", "c1", fixture{panes: lead}, ""},
		{"empty role is lead", "worker:feat/x#s1-2", "c1", fixture{panes: leadNoRole}, ""},
		{"empty role refuses other branch", "worker:feat/y#s1-2", "c1", fixture{panes: leadNoRole}, leadX},
		{"lead refuses other branch", "worker:feat/y#s1-2", "c1", fixture{panes: lead}, leadX},
		{"lead refuses # in session", "worker:feat/x#s1#2", "c1", fixture{panes: lead}, leadX},
		{"lead refuses role id", "role:feat/x:reviewer", "c1", fixture{panes: lead}, leadX},
		{"lead refuses dispatcher", "dispatcher:c1", "c1", fixture{panes: lead}, leadX},
		{"lead refuses empty", "", "c1", fixture{panes: lead}, leadX},
		{"lead refuses review", "review:c1", "c1", fixture{panes: lead}, leadX},
		{"lead refuses bare prefix", "worker:feat/x#", "c1", fixture{panes: lead}, leadX},

		{"branch with # allows session", "worker:a#b#s1-2", "c1", fixture{panes: pane("100", "a#b", "lead", "c1", "@1", "%1")}, ""},
		{"branch with # allows bare", "worker:a#b", "c1", fixture{panes: pane("100", "a#b", "lead", "c1", "@1", "%1")}, ""},
		{"branch with # refuses # in session", "worker:a#b#s1#2", "c1", fixture{panes: pane("100", "a#b", "lead", "c1", "@1", "%1")},
			"this pane is stamped as the lead of a#b, which posts only as worker:a#b or worker:a#b#s<session> ($CREW_WORKER_ID)"},

		{"role allows own id", "role:feat/x:reviewer", "c1", fixture{panes: reviewer}, ""},
		{"role refuses lead id", "worker:feat/x#s1-2", "c1", fixture{panes: reviewer}, roleX},
		{"role refuses other role", "role:feat/x:plan-critic", "c1", fixture{panes: reviewer}, roleX},
		{"role refuses other branch", "role:feat/y:reviewer", "c1", fixture{panes: reviewer}, roleX},
		{"role refuses dispatcher", "dispatcher:c1", "c1", fixture{panes: reviewer}, roleX},

		{"crew mismatch", "worker:feat/x", "c2", fixture{panes: lead}, "this pane's window is stamped for crew c1, not c2"},
		{"role crew mismatch", "role:feat/x:reviewer", "c2", fixture{panes: reviewer}, "this pane's window is stamped for crew c1, not c2"},
		{"empty crew id unchecked", "worker:feat/x", "c2", fixture{panes: pane("100", "feat/x", "lead", "", "@1", "%1")}, ""},

		{"unstamped anchor allows dispatcher", "dispatcher:c1", "c1", fixture{panes: dispatcherPane}, ""},
		{"nearest pane wins", "worker:feat/y", "c1", fixture{panes: lead + pane("200", "feat/y", "lead", "c1", "@1", "%1")}, ""},
		{"nearest pane refuses outer id", "worker:feat/x", "c1", fixture{panes: lead + pane("200", "feat/y", "lead", "c1", "@1", "%1")},
			"this pane is stamped as the lead of feat/y, which posts only as worker:feat/y or worker:feat/y#s<session> ($CREW_WORKER_ID)"},
		{"nearest unstamped pane hides outer stamp", "dispatcher:c1", "c1", fixture{panes: lead + pane("200", "", "", "", "@1", "%1")}, ""},
		{"own pid is a pane", "worker:feat/x", "c1", fixture{panes: pane(strconv.Itoa(self), "feat/x", "lead", "c1", "@1", "%1"), tree: map[int]int{}}, ""},
		{"own pid pane refuses", "worker:feat/y", "c1", fixture{panes: pane(strconv.Itoa(self), "feat/x", "lead", "c1", "@1", "%1"), tree: map[int]int{}}, leadX},

		{"no pane in ancestry", "dispatcher:c1", "c1", fixture{panes: pane("999", "feat/x", "lead", "c1", "@1", "%1")}, ""},
		{"panes error", "dispatcher:c1", "c1", fixture{panes: lead, panesErr: true}, ""},
		{"nil panes", "dispatcher:c1", "c1", fixture{panes: lead, noPanes: true}, ""},
		{"nil parent", "dispatcher:c1", "c1", fixture{panes: lead, noParent: true}, ""},
		{"parent cycle terminates", "dispatcher:c1", "c1", fixture{panes: pane("999", "feat/x", "lead", "c1", "@1", "%1"), tree: map[int]int{self: 200, 200: self}}, ""},
		{"malformed line skipped", "worker:feat/x", "c1", fixture{panes: "garbage\n" + pane("x", "feat/y", "lead", "c1", "@1", "%1") + pane("0", "feat/y", "lead", "c1", "@1", "%1") + "100\x1ffeat/y\n" + lead}, ""},
		{"malformed line does not anchor", "dispatcher:c1", "c1", fixture{panes: "100\x1ffeat/y\x1flead\n"}, ""},

		{"overseer allows own host unanchored", "overseer:h1", "c1", fixture{env: marked}, ""},
		{"overseer allows own host from unstamped anchor", "overseer:h1", "c1", fixture{env: marked, panes: dispatcherPane}, ""},
		{"overseer allows own host from its pane", "overseer:h1", "c1", fixture{env: marked, panes: overseerPane}, ""},
		{"overseer pane ignores crew id", "overseer:h1", "c1", fixture{env: marked, panes: pane("100", "", "overseer", "c9", "@1", "%1")}, ""},
		{"overseer refuses worker env", "overseer:h1", "c1", fixture{env: markedWorker}, overseerIDs},
		{"overseer refuses role env", "overseer:h1", "c1", fixture{env: markedRole}, overseerIDs},
		{"overseer refuses unmarked session", "overseer:h1", "c1", fixture{}, overseerOnly},
		{"overseer refuses mark for other host", "overseer:h1", "c1", fixture{env: map[string]string{"CREW_OVERSEER": "h2"}},
			"CREW_OVERSEER is h2, not this host's name h1; " + overseerOnly},
		{"overseer refuses other host", "overseer:h2", "c1", fixture{env: marked}, notH1 + "overseer:h2"},
		{"overseer refuses bare prefix", "overseer:", "c1", fixture{env: marked}, notH1 + "overseer:"},
		{"overseer refuses suffix", "overseer:h1#x", "c1", fixture{env: marked}, notH1 + "overseer:h1#x"},
		{"overseer refuses other case", "overseer:H1", "c1", fixture{env: marked}, notH1 + "overseer:H1"},
		{"overseer refuses host-name error", "overseer:h1", "c1", fixture{env: marked, selfErr: true},
			"this host's name cannot be read (boom), so no session can post as overseer:<host>"},
		{"overseer refuses nil host-name probe", "overseer:h1", "c1", fixture{env: marked, noSelf: true},
			"this host's name cannot be read (no host-name probe), so no session can post as overseer:<host>"},
		{"overseer refuses lead pane", "overseer:h1", "c1", fixture{env: marked, panes: lead}, leadX},
		{"overseer refuses role pane", "overseer:h1", "c1", fixture{env: marked, panes: reviewer}, roleX},
		{"overseer refuses panes error", "overseer:h1", "c1", fixture{env: marked, panes: dispatcherPane, panesErr: true}, overseerProbe},
		{"overseer refuses nil panes", "overseer:h1", "c1", fixture{env: marked, noPanes: true}, overseerProbe},
		{"overseer refuses nil parent", "overseer:h1", "c1", fixture{env: marked, noParent: true}, overseerProbe},

		{"marked refuses dispatcher", "dispatcher:c1", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses worker", "worker:feat/x", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses worker session", "worker:feat/x#s1-2", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses role", "role:feat/x:reviewer", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses peer", "peer:h2", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses review", "review:c1", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses metrics", "metrics:c1", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses retro", "retro:c1", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses empty", "", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses custom", "custom:x", "c1", fixture{env: marked}, markedOnly},
		{"marked refuses despite host-name error", "worker:feat/x", "c1", fixture{env: marked, selfErr: true}, markedOnly},
		{"tripwire beats the mark", "dispatcher:c1", "c1", fixture{env: markedWorker}, tripwire},

		{"overseer pane refuses dispatcher", "dispatcher:c1", "c1", fixture{panes: overseerPane}, overseerStamped},
		{"overseer pane refuses worker", "worker:feat/x", "c1", fixture{panes: overseerPane}, overseerStamped},
		{"overseer pane env runs first", "overseer:h1", "c1", fixture{panes: overseerPane}, overseerOnly},
		{"overseer pane host-name error", "dispatcher:c1", "c1", fixture{panes: overseerPane, selfErr: true},
			"this pane is stamped as the overseer's (@crew_role overseer), and this host's name cannot be read (boom)"},
		{"branch-stamped overseer role is a grid role", "dispatcher:c1", "c1", fixture{panes: pane("100", "feat/x", "overseer", "c1", "@1", "%1")},
			"this pane is stamped as role:feat/x:overseer, the only id it posts as"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Check(t.Context(), tc.from, tc.crew, tc.f.probes()); got != tc.want {
				t.Errorf("Check(%q, %q) = %q, want %q", tc.from, tc.crew, got, tc.want)
			}
		})
	}
}

func TestCheckZeroProbes(t *testing.T) {
	if got := Check(t.Context(), "dispatcher:c1", "c1", Probes{}); got != "" {
		t.Errorf("Check with zero Probes = %q, want empty", got)
	}
	const want = "the session env cannot be read, and " + overseerOnly
	if got := Check(t.Context(), "overseer:h1", "c1", Probes{}); got != want {
		t.Errorf("Check(overseer:h1) with zero Probes = %q, want %q", got, want)
	}
}

func TestWatch(t *testing.T) {
	const idX = "worker:feat/x#s1-1"
	callerLead := pane("100", "feat/x", "lead", "c1", "@1", "%1")
	callerRole := pane("100", "feat/x", "reviewer", "c1", "@1", "%1")
	marked := map[string]string{"CREW_OVERSEER": "h1"}
	overseerTarget := pane("500", "", "overseer", "", "@2", "%2")
	const (
		leadY    = "pane %2 is stamped as the lead of feat/x, which posts only as worker:feat/x or worker:feat/x#s<session> ($CREW_WORKER_ID)"
		roleRev  = "pane %2 is stamped as role:feat/x:reviewer, the only id it posts as"
		outside  = "pane %2 is outside this pane's window"
		outside9 = "pane %9 is outside this pane's window"
	)
	tests := []struct {
		name string
		from string
		crew string
		pane string
		f    fixture
		want string
	}{
		{"unanchored unlisted target", idX, "c1", "%2", fixture{}, ""},
		{"unanchored unstamped target", idX, "c1", "%2", fixture{panes: pane("500", "", "", "", "@2", "%2")}, ""},
		{"unanchored lead target ok", idX, "c1", "%2", fixture{panes: pane("500", "feat/x", "lead", "c1", "@2", "%2")}, ""},
		{"unanchored lead target other branch", "worker:feat/y#s1-1", "c1", "%2", fixture{panes: pane("500", "feat/x", "lead", "c1", "@2", "%2")}, leadY},
		{"unanchored role target ok", "role:feat/x:reviewer", "c1", "%2", fixture{panes: pane("500", "feat/x", "reviewer", "c1", "@2", "%2")}, ""},
		{"unanchored role target other role", "role:feat/x:plan-critic", "c1", "%2", fixture{panes: pane("500", "feat/x", "reviewer", "c1", "@2", "%2")}, roleRev},
		{"unanchored role target lead id", "worker:feat/x", "c1", "%2", fixture{panes: pane("500", "feat/x", "reviewer", "c1", "@2", "%2")}, roleRev},
		{"unanchored target crew mismatch", idX, "c2", "%2", fixture{panes: pane("500", "feat/x", "lead", "c1", "@2", "%2")}, "pane %2's window is stamped for crew c1, not c2"},
		{"unanchored target empty crew", idX, "c2", "%2", fixture{panes: pane("500", "feat/x", "lead", "", "@2", "%2")}, ""},

		{"unanchored session target", idX, "c1", "sess:1.0", fixture{panes: pane("500", "feat/y", "lead", "c1", "@2", "%2")}, "pane sess:1.0 is not a pane id (%N)"},
		{"unanchored window-id target", idX, "c1", "@2", fixture{panes: pane("500", "feat/y", "lead", "c1", "@2", "%2")}, "pane @2 is not a pane id (%N)"},
		{"unanchored zero-padded target", idX, "c1", "%01", fixture{panes: pane("500", "feat/y", "lead", "c1", "@1", "%1")}, "pane %01 is not a pane id (%N)"},
		{"unanchored bare percent target", idX, "c1", "%", fixture{panes: pane("500", "feat/y", "lead", "c1", "@2", "%2")}, "pane % is not a pane id (%N)"},
		{"unanchored unlisted target with panes listed", idX, "c1", "%7", fixture{panes: pane("500", "feat/x", "lead", "c1", "@2", "%2")}, "pane %7 is not a live pane"},
		{"unanchored trailing-junk target", idX, "c1", "%2x", fixture{panes: pane("500", "feat/x", "lead", "c1", "@2", "%2")}, "pane %2x is not a pane id (%N)"},
		{"unanchored %0 unlisted", idX, "c1", "%0", fixture{}, ""},

		{"anchored own pane own id", idX, "c1", "%1", fixture{panes: callerLead}, ""},
		{"anchored other id is Check's refusal", "worker:feat/y#s1-1", "c1", "%1", fixture{panes: callerLead}, leadX},
		{"anchored target in other window", idX, "c1", "%2", fixture{panes: callerLead + pane("500", "feat/x", "lead", "c1", "@2", "%2")}, outside},
		{"anchored unlisted target", idX, "c1", "%9", fixture{panes: callerLead}, outside9},
		{"anchored same-window role pane with lead id", idX, "c1", "%3", fixture{panes: callerLead + pane("501", "feat/x", "reviewer", "c1", "@1", "%3")}, "pane %3 is stamped as role:feat/x:reviewer, the only id it posts as"},
		{"anchored role own pane", "role:feat/x:reviewer", "c1", "%1", fixture{panes: callerRole}, ""},
		{"anchored role watching lead pane", "role:feat/x:reviewer", "c1", "%3", fixture{panes: callerRole + pane("501", "feat/x", "lead", "c1", "@1", "%3")}, "pane %3 is stamped as the lead of feat/x, which posts only as worker:feat/x or worker:feat/x#s<session> ($CREW_WORKER_ID)"},

		{"panes error", idX, "c1", "%2", fixture{panes: callerLead, panesErr: true}, ""},
		{"nil probes", idX, "c1", "%2", fixture{noPanes: true}, ""},

		{"marked refuses lead id", idX, "c1", "%2", fixture{env: marked, panes: pane("500", "", "", "", "@2", "%2")}, markedOnly},
		{"anchored overseer pane refuses lead id", idX, "c1", "%1", fixture{panes: pane("100", "", "overseer", "", "@1", "%1")}, overseerStamped},
		{"overseer target refuses lead id", idX, "c1", "%2", fixture{panes: overseerTarget},
			"pane %2 is stamped as the overseer's (@crew_role overseer), which posts only as overseer:h1"},
		{"overseer target host-name error", idX, "c1", "%2", fixture{panes: overseerTarget, selfErr: true},
			"pane %2 is stamped as the overseer's (@crew_role overseer), and this host's name cannot be read (boom)"},
		{"overseer target allows overseer", "overseer:h1", "c1", "%2", fixture{env: marked, panes: overseerTarget}, ""},
		{"overseer panes error", "overseer:h1", "c1", "%2", fixture{env: marked, panes: overseerTarget, panesErr: true}, overseerProbe},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Watch(t.Context(), tc.from, tc.crew, tc.pane, tc.f.probes()); got != tc.want {
				t.Errorf("Watch(%q, %q, %q) = %q, want %q", tc.from, tc.crew, tc.pane, got, tc.want)
			}
		})
	}
	if got := Watch(t.Context(), idX, "c1", "%2", Probes{}); got != "" {
		t.Errorf("Watch with zero Probes = %q, want empty", got)
	}
}
