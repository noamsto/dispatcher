package sender

import (
	"errors"
	"strings"
	"testing"
)

// pane is one `tmux list-panes -a -F Format` line.
func pane(pid, branch, role, crewID, win, id string) string {
	return strings.Join([]string{pid, branch, role, crewID, win, id}, "\x1f") + "\n"
}

// tree is the caller's ancestry: 300 → 200 → 100 → 1.
var tree = map[int]int{300: 200, 200: 100, 100: 1}

type fixture struct {
	env      map[string]string
	panes    string
	panesErr bool
	noPanes  bool
	noParent bool
	tree     map[int]int
	pid      int
}

func (f fixture) probes() Probes {
	p := Probes{
		Getenv: func(k string) string { return f.env[k] },
		Panes:  func() (string, error) { return f.panes, nil },
		Parent: func(pid int) (int, bool) {
			pp, ok := f.tree[pid]
			return pp, ok
		},
		PID: 300,
	}
	if f.tree == nil {
		p.Parent = func(pid int) (int, bool) {
			pp, ok := tree[pid]
			return pp, ok
		}
	}
	if f.pid != 0 {
		p.PID = f.pid
	}
	if f.panesErr {
		p.Panes = func() (string, error) { return "", errors.New("no server running") }
	}
	if f.noPanes {
		p.Panes = nil
	}
	if f.noParent {
		p.Parent = nil
	}
	return p
}

const (
	tripwire = "this is a worker or role session (CREW_WORKER_ID or CREW_ROLE_ID is set) and only the dispatcher posts as dispatcher:<crew>; post as your own id ($CREW_WORKER_ID for a lead, role:<branch>:<role> for a grid role)"
	leadX    = "this pane is stamped as the lead of feat/x, which posts only as worker:feat/x or worker:feat/x#s<session> ($CREW_WORKER_ID)"
	roleX    = "this pane is stamped as role:feat/x:reviewer, the only id it posts as"
)

func TestCheck(t *testing.T) {
	worker := map[string]string{"CREW_WORKER_ID": "worker:feat/x#s1-2"}
	role := map[string]string{"CREW_ROLE_ID": "role:feat/x:reviewer"}
	lead := pane("100", "feat/x", "lead", "c1", "@1", "%1")
	leadNoRole := pane("100", "feat/x", "", "c1", "@1", "%1")
	reviewer := pane("100", "feat/x", "reviewer", "c1", "@1", "%1")
	dispatcherPane := pane("100", "", "", "", "@1", "%1")

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
		{"own pid is a pane", "worker:feat/x", "c1", fixture{panes: pane("300", "feat/x", "lead", "c1", "@1", "%1"), tree: map[int]int{}}, ""},
		{"own pid pane refuses", "worker:feat/y", "c1", fixture{panes: pane("300", "feat/x", "lead", "c1", "@1", "%1"), tree: map[int]int{}}, leadX},

		{"no pane in ancestry", "dispatcher:c1", "c1", fixture{panes: pane("999", "feat/x", "lead", "c1", "@1", "%1")}, ""},
		{"panes error", "dispatcher:c1", "c1", fixture{panes: lead, panesErr: true}, ""},
		{"nil panes", "dispatcher:c1", "c1", fixture{panes: lead, noPanes: true}, ""},
		{"nil parent", "dispatcher:c1", "c1", fixture{panes: lead, noParent: true}, ""},
		{"parent cycle terminates", "dispatcher:c1", "c1", fixture{panes: pane("999", "feat/x", "lead", "c1", "@1", "%1"), tree: map[int]int{300: 200, 200: 300}}, ""},
		{"malformed line skipped", "worker:feat/x", "c1", fixture{panes: "garbage\n" + pane("x", "feat/y", "lead", "c1", "@1", "%1") + pane("0", "feat/y", "lead", "c1", "@1", "%1") + "100\x1ffeat/y\n" + lead}, ""},
		{"malformed line does not anchor", "dispatcher:c1", "c1", fixture{panes: "100\x1ffeat/y\x1flead\n"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Check(tc.from, tc.crew, tc.f.probes()); got != tc.want {
				t.Errorf("Check(%q, %q) = %q, want %q", tc.from, tc.crew, got, tc.want)
			}
		})
	}
}

func TestCheckZeroProbes(t *testing.T) {
	if got := Check("dispatcher:c1", "c1", Probes{}); got != "" {
		t.Errorf("Check with zero Probes = %q, want empty", got)
	}
}

func TestWatch(t *testing.T) {
	const idX = "worker:feat/x#s1-1"
	callerLead := pane("100", "feat/x", "lead", "c1", "@1", "%1")
	callerRole := pane("100", "feat/x", "reviewer", "c1", "@1", "%1")
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Watch(tc.from, tc.crew, tc.pane, tc.f.probes()); got != tc.want {
				t.Errorf("Watch(%q, %q, %q) = %q, want %q", tc.from, tc.crew, tc.pane, got, tc.want)
			}
		})
	}
	if got := Watch(idX, "c1", "%2", Probes{}); got != "" {
		t.Errorf("Watch with zero Probes = %q, want empty", got)
	}
}
