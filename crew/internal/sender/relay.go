package sender

import (
	"context"
	"errors"

	"github.com/noamsto/dispatcher/crew/internal/remote"
)

var errNoHosts = errors.New("no settings-hosts probe")

// CheckRelay returns "" when this caller may post a relayed overseer:<origin>
// row on behalf of another host's overseer, else the refusal reason. Like
// Check it is a tripwire on the sanctioned path (same uid, self-asserted
// origin), not authentication. Every probe that cannot be read refuses.
func CheckRelay(ctx context.Context, origin string, p Probes) string {
	if !remote.HostNameRe.MatchString(origin) {
		return "origin " + sanitize(origin) + " is not a valid host name, so no relay is posted"
	}
	self, err := p.self()
	if err != nil {
		return "this host's name cannot be read (" + err.Error() + "), so no relay is posted"
	}
	if origin == self {
		return "origin " + origin + " is this host's own name, and a relay comes only from another host"
	}
	if reason := p.relayHost(origin); reason != "" {
		return reason
	}
	if p.Getenv == nil {
		return "the session env cannot be read, so no relay is posted"
	}
	for _, k := range []string{"CREW_ID", "CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_OVERSEER"} {
		if p.Getenv(k) != "" {
			return "this session has " + k + " set, and only a session that is no dispatcher, worker, role or overseer posts a relay"
		}
	}
	rows, read := p.rows(ctx)
	s, anchored, walked := p.anchor(rows)
	if !read || !walked {
		return "the pane stamps cannot be read on " + self + " (tmux list-panes or the process ancestry failed), so no relay is posted"
	}
	if anchored && (s.branch != "" || s.role != "" || s.crew != "") {
		return "this session sits in a crew pane stamped with a branch, a role or a crew, and only a session outside every crew pane posts a relay"
	}
	return ""
}

func (p Probes) relayHost(origin string) string {
	if p.Hosts == nil {
		return "the settings hosts cannot be read (" + errNoHosts.Error() + "), so no relay is posted"
	}
	hosts, err := p.Hosts()
	if err != nil {
		return "the settings hosts cannot be read (" + err.Error() + "), so no relay is posted"
	}
	if _, ok := hosts[origin]; !ok {
		return "origin " + origin + " is not in settings hosts, so no relay is posted"
	}
	return ""
}
