package overseer

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

const deliverPrefix = "crew: overseer deliver: "

// mbpFixture is a plain session on mbp, whose settings hosts are mbp and halo,
// with one repo alpha.
func mbpFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.host("mbp", "halo")
	delete(f.env, "CREW_OVERSEER")
	f.repo("alpha", "")
	return f
}

func (f *fixture) deliver(req DeliverRequest) (int, string, string) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunDeliver(f.t.Context(), req, &stdout, &stderr, f.opts())
	return code, stdout.String(), stderr.String()
}

func relayReq(body string) DeliverRequest {
	return DeliverRequest{Origin: "halo", Repo: "alpha", Crew: "c1", Body: body}
}

func TestDeliverPostsOneRowFromTheOrigin(t *testing.T) {
	f := mbpFixture(t)
	before := f.logs()[f.commons[0]]

	code, stdout, stderr := f.deliver(relayReq(notice))
	if code != 0 || stderr != "" || stdout != `{"ts":`+tsText+`}`+"\n" {
		t.Fatalf("got %d %q %q", code, stdout, stderr)
	}
	if got, want := f.logs()[f.commons[0]], before+rowFrom("halo", "c1", notice)+"\n"; got != want {
		t.Errorf("log %q, want %q", got, want)
	}
}

func TestDeliverRefusesAndWritesNothing(t *testing.T) {
	relay := func(reason string) string {
		return deliverPrefix + "refusing relay from 'overseer:halo' — " + reason
	}
	long := `{"kind":"notice","detail":"` + strings.Repeat("x", peer.MaxBody) + `"}`
	cases := []struct {
		name string
		req  DeliverRequest
		mark func(f *fixture)
		want func(f *fixture) string
	}{
		{"origin not in settings hosts", DeliverRequest{Origin: "evil", Repo: "alpha", Crew: "c1", Body: notice}, nil,
			func(*fixture) string {
				return deliverPrefix + "refusing relay from 'overseer:evil' — origin evil is not in settings hosts, so no relay is posted"
			}},
		{"origin is this host", DeliverRequest{Origin: "mbp", Repo: "alpha", Crew: "c1", Body: notice}, nil,
			func(*fixture) string {
				return deliverPrefix + "refusing relay from 'overseer:mbp' — origin mbp is this host's own name, and a relay comes only from another host"
			}},
		{"origin is no host name", DeliverRequest{Origin: "a b", Repo: "alpha", Crew: "c1", Body: notice}, nil,
			func(*fixture) string { return deliverPrefix + `origin "a b" is not a valid host name` }},
		{"settings hosts unreadable", relayReq(notice), func(f *fixture) { f.hosts = nil },
			func(*fixture) string {
				return relay("the settings hosts cannot be read (no settings-hosts probe), so no relay is posted")
			}},
		{"a worker session", relayReq(notice), func(f *fixture) { f.env["CREW_WORKER_ID"] = "worker:feat/x" },
			func(*fixture) string {
				return relay("this session has CREW_WORKER_ID set, and only a session that is no worker, role or overseer posts a relay")
			}},
		{"an overseer session", relayReq(notice), func(f *fixture) { f.env["CREW_OVERSEER"] = "mbp" },
			func(*fixture) string {
				return relay("this session has CREW_OVERSEER set, and only a session that is no worker, role or overseer posts a relay")
			}},
		{"tmux cannot be read", relayReq(notice), func(f *fixture) { delete(f.tmux.Out, "list-panes") },
			func(f *fixture) string { return relay(sender.CheckRelay(f.t.Context(), "halo", f.opts().Sender)) }},
		{"over the byte limit", relayReq(long), nil,
			func(*fixture) string {
				return deliverPrefix + fmt.Sprintf("body is %d bytes, over the %d-byte limit", len(long), peer.MaxBody)
			}},
		{"a raw control", relayReq("{\"kind\":\"notice\",\"detail\":\"a\x1bb\"}"), nil,
			func(*fixture) string { return deliverPrefix + reasonControl }},
		{"a raw zero-width space", relayReq("{\"kind\":\"notice\",\"detail\":\"a\u200bb\"}"), nil,
			func(*fixture) string { return deliverPrefix + reasonControl }},
		{"an escaped ESC", relayReq(`{"kind":"notice","detail":"a\u001bb"}`), nil,
			func(*fixture) string { return deliverPrefix + reasonControl }},
		{"an escaped zero-width space", relayReq(`{"kind":"notice","detail":"a\u200bb"}`), nil,
			func(*fixture) string { return deliverPrefix + reasonControl }},
		{"a kind outside the set", relayReq(`{"kind":"directive"}`), nil,
			func(*fixture) string {
				return deliverPrefix + `body kind must be one of ask|request|capacity|notice, got "directive"`
			}},
		{"an unknown repo", DeliverRequest{Origin: "halo", Repo: "owner/unknown", Crew: "c1", Body: notice}, nil,
			func(*fixture) string {
				return deliverPrefix + "repo owner/unknown is not a discovered crew bus; pass a repo as crew fleet names it, or its git common dir"
			}},
		{"a crew not on the bus", DeliverRequest{Origin: "halo", Repo: "alpha", Crew: "c9", Body: notice}, nil,
			func(f *fixture) string { return deliverPrefix + "crew c9 is not on " + f.commons[0] + "'s bus" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := mbpFixture(t)
			if c.mark != nil {
				c.mark(f)
			}
			want := c.want(f)
			before := f.logs()
			code, stdout, stderr := f.deliver(c.req)
			if code != 1 || stdout != "" || stderr != want+"\n" {
				t.Errorf("got %d %q %q, want 1, no stdout, %q", code, stdout, stderr, want+"\n")
			}
			if after := f.logs(); !maps.Equal(after, before) {
				t.Errorf("a refused relay changed a log:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

func TestDeliverChecksTheRelayBeforeReadingAnyRepo(t *testing.T) {
	f := mbpFixture(t)
	f.env["CREW_WORKER_ID"] = "worker:feat/x"
	code, _, stderr := f.deliver(DeliverRequest{Origin: "halo", Repo: "nowhere/unknown", Crew: "c1", Body: notice})
	if code != 1 || !strings.HasPrefix(stderr, deliverPrefix+"refusing relay from 'overseer:halo'") {
		t.Errorf("got %d %q", code, stderr)
	}
	if calls := f.git.Calls(); len(calls) != 0 {
		t.Errorf("a refused relay still read repos: %q", calls)
	}
}

func TestDeliverRefusesWhenTheHostNameCannotBeRead(t *testing.T) {
	f := mbpFixture(t)
	f.self = func() (string, error) { return "", errors.New("no hostname") }
	before := f.logs()
	code, stdout, stderr := f.deliver(relayReq(notice))
	want := deliverPrefix + "refusing relay from 'overseer:halo' — this host's name cannot be read (no hostname), so no relay is posted\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Errorf("got %d %q %q, want %q", code, stdout, stderr, want)
	}
	if !maps.Equal(f.logs(), before) {
		t.Error("a refused relay changed a log")
	}
}
