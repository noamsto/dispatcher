package sender

import (
	"errors"
	"strconv"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// relayProbes is a far host `halo` whose settings hold `mbp`, an empty env, a
// tmux listing with no pane in the caller's ancestry, and an ancestry that
// walks to pid 1.
func relayProbes() Probes {
	return Probes{
		Getenv: func(string) string { return "" },
		Tmux:   &tmuxtest.Fake{Out: map[string]string{"list-panes": pane("999", "feat/x", "lead", "c1", "@1", "%1")}},
		Proc:   proctest.New(nil, tree),
		Self:   func() (string, error) { return "halo", nil },
		Hosts: func() (map[string]remote.Host, error) {
			return map[string]remote.Host{"mbp": {SSH: "mbp"}}, nil
		},
	}
}

func TestCheckRelay(t *testing.T) {
	const unread = "the pane stamps cannot be read on halo (tmux list-panes or the process ancestry failed), so no relay is posted"
	anchored := "this session sits in a crew pane stamped with a branch or a role, and only a session outside every crew pane posts a relay"
	envSet := func(k string) func(*Probes) {
		return func(p *Probes) { p.Getenv = func(n string) string { return map[string]string{k: "x"}[n] } }
	}
	panes := func(out string) func(*Probes) {
		return func(p *Probes) { p.Tmux = &tmuxtest.Fake{Out: map[string]string{"list-panes": out}} }
	}
	tests := []struct {
		name   string
		origin string
		mod    func(*Probes)
		want   string
	}{
		{"admits a settings host", "mbp", nil, ""},
		{"admits with no panes listed", "mbp", panes(""), ""},
		{"admits under an unstamped pane", "mbp", panes(pane("100", "", "", "", "@1", "%1")), ""},

		{"refuses a bad host name", "-mbp", nil, "origin -mbp is not a valid host name, so no relay is posted"},
		{"refuses an empty origin", "", nil, "origin  is not a valid host name, so no relay is posted"},
		{"refuses a path origin", "mbp/x", nil, "origin mbp/x is not a valid host name, so no relay is posted"},
		{"refuses a control char origin", "mbp\n", nil, "origin mbp? is not a valid host name, so no relay is posted"},
		{"refuses host-name error", "mbp", func(p *Probes) { p.Self = func() (string, error) { return "", errors.New("boom") } },
			"this host's name cannot be read (boom), so no relay is posted"},
		{"refuses nil host-name probe", "mbp", func(p *Probes) { p.Self = nil },
			"this host's name cannot be read (no host-name probe), so no relay is posted"},
		{"refuses self", "halo", nil, "origin halo is this host's own name, and a relay comes only from another host"},
		{"refuses nil hosts", "mbp", func(p *Probes) { p.Hosts = nil },
			"the settings hosts cannot be read (no settings-hosts probe), so no relay is posted"},
		{"refuses hosts error", "mbp", func(p *Probes) {
			p.Hosts = func() (map[string]remote.Host, error) { return nil, errors.New("bad json") }
		}, "the settings hosts cannot be read (bad json), so no relay is posted"},
		{"refuses origin not in hosts", "evil", nil, "origin evil is not in settings hosts, so no relay is posted"},
		{"refuses nil env", "mbp", func(p *Probes) { p.Getenv = nil }, "the session env cannot be read, so no relay is posted"},
		{"refuses worker env", "mbp", envSet("CREW_WORKER_ID"),
			"this session has CREW_WORKER_ID set, and only a session that is no worker, role or overseer posts a relay"},
		{"refuses role env", "mbp", envSet("CREW_ROLE_ID"),
			"this session has CREW_ROLE_ID set, and only a session that is no worker, role or overseer posts a relay"},
		{"refuses overseer env", "mbp", envSet("CREW_OVERSEER"),
			"this session has CREW_OVERSEER set, and only a session that is no worker, role or overseer posts a relay"},
		{"refuses tmux read failure", "mbp", func(p *Probes) { p.Tmux = &tmuxtest.Fake{} }, unread},
		{"refuses nil tmux", "mbp", func(p *Probes) { p.Tmux = nil }, unread},
		{"refuses nil proc", "mbp", func(p *Probes) { p.Proc = nil }, unread},
		{"refuses ancestry short of init", "mbp", func(p *Probes) { p.Proc = proctest.New(nil, map[int]int{self: 200}) }, unread},
		{"refuses parent cycle", "mbp", func(p *Probes) { p.Proc = proctest.New(nil, map[int]int{self: 200, 200: self}) }, unread},
		{"refuses a lead pane", "mbp", panes(pane("100", "feat/x", "lead", "c1", "@1", "%1")), anchored},
		{"refuses a lead pane with no role", "mbp", panes(pane("100", "feat/x", "", "c1", "@1", "%1")), anchored},
		{"refuses a role pane", "mbp", panes(pane("200", "feat/x", "reviewer", "c1", "@1", "%1")), anchored},
		{"refuses the overseer pane", "mbp", panes(pane("100", "", "overseer", "", "@1", "%1")), anchored},
		{"refuses own pid as a pane", "mbp", func(p *Probes) {
			p.Tmux = &tmuxtest.Fake{Out: map[string]string{"list-panes": pane(strconv.Itoa(self), "feat/x", "lead", "c1", "@1", "%1")}}
			p.Proc = proctest.New(nil, map[int]int{})
		}, anchored},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := relayProbes()
			if tc.mod != nil {
				tc.mod(&p)
			}
			if got := CheckRelay(t.Context(), tc.origin, p); got != tc.want {
				t.Errorf("CheckRelay(%q) = %q, want %q", tc.origin, got, tc.want)
			}
		})
	}
}

// TestCheckRelayLeavesCheck: the relay rule does not loosen Check, which still
// refuses overseer:<another host> from an otherwise admissible relay session.
func TestCheckRelayLeavesCheck(t *testing.T) {
	if got := Check(t.Context(), "overseer:mbp", "c1", relayProbes()); got != overseerOnly {
		t.Errorf("Check(overseer:mbp) = %q, want %q", got, overseerOnly)
	}
}
