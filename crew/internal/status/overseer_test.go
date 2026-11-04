package status

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// relayOpts is a far host `halo` whose settings hold `mbp`, an empty env, no
// anchor pane, and an ancestry that walks to pid 1.
func (f *fixture) relayOpts(mod func(*sender.Probes)) Options {
	o := f.opts()
	o.Sender = sender.Probes{
		Getenv: func(string) string { return "" },
		Tmux:   &tmuxtest.Fake{Out: map[string]string{"list-panes": ""}},
		Proc:   proctest.New(nil, map[int]int{os.Getpid(): 1}),
		Self:   func() (string, error) { return "halo", nil },
		Hosts:  func() (map[string]remote.Host, error) { return map[string]remote.Host{"mbp": {}}, nil },
	}
	if mod != nil {
		mod(&o.Sender)
	}
	return o
}

func (f *fixture) overseerRelay(self, body string, o Options) (int64, int, string) {
	var stderr bytes.Buffer
	ts, code := RunOverseerRelay(f.t.Context(), OverseerMsgRequest{Self: self, Crew: "c1", Body: body}, f.paths, &stderr, o)
	return ts, code, stderr.String()
}

func TestOverseerRelayPostsTheOriginHostsRow(t *testing.T) {
	f := newFixture(t)
	f.log()
	ts, code, stderr := f.overseerRelay("mbp", "hi", f.relayOpts(nil))
	if code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(`{"ts":` + tsText + `,"crew_id":"c1","from":"overseer:mbp","to":"dispatcher:c1","kind":"msg","body":"hi"}`)
	if want := int64(1700000000500); ts != want {
		t.Errorf("ts %d, want %d", ts, want)
	}
}

func TestOverseerRelayRefusesWithoutWriting(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		mod    func(*sender.Probes)
		want   string
	}{
		{"origin not in hosts", "other", nil, "origin other is not in settings hosts"},
		{"origin is this host", "halo", nil, "is this host's own name"},
		{"worker env", "mbp", func(p *sender.Probes) {
			p.Getenv = func(k string) string {
				if k == "CREW_WORKER_ID" {
					return "w"
				}
				return ""
			}
		}, "CREW_WORKER_ID set"},
		{"tmux read fails", "mbp", func(p *sender.Probes) {
			p.Tmux = &tmuxtest.Fake{Func: func([]string) (string, error) { return "", errors.New("boom") }}
		}, "pane stamps cannot be read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.log()
			ts, code, stderr := f.overseerRelay(tt.origin, "hi", f.relayOpts(tt.mod))
			prefix := "crew: overseer deliver: refusing relay from 'overseer:" + tt.origin + "' — "
			if ts != 0 || code != 1 || !strings.HasPrefix(stderr, prefix) || !strings.Contains(stderr, tt.want) {
				t.Errorf("got %d %d %q, want 0 1 %q ... %q", ts, code, stderr, prefix, tt.want)
			}
			f.wantNoRow()
		})
	}
}

func TestOverseerRelayRefusesARowOverTheLineMax(t *testing.T) {
	f := newFixture(t)
	f.log()
	ts, code, stderr := f.overseerRelay("mbp", strings.Repeat("x", bus.LineMax), f.relayOpts(nil))
	if ts != 0 || code != 1 || !strings.HasPrefix(stderr, "crew: overseer deliver: the encoded row is ") {
		t.Errorf("got %d %d %q", ts, code, stderr)
	}
	f.wantNoRow()
}

func TestOverseerMsgStillRefusesAnotherHost(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	code, stderr := f.overseerMsg("mbp", "c1", "hi")
	if code != 1 || !strings.HasPrefix(stderr, "crew: overseer msg: refusing sender 'overseer:mbp' — ") {
		t.Errorf("got %d %q", code, stderr)
	}
	f.wantNoRow()
}

func (f *fixture) overseerMsg(self, crew, body string) (int, string) {
	var stderr bytes.Buffer
	code := RunOverseerMsg(f.t.Context(), OverseerMsgRequest{Self: self, Crew: crew, Body: body}, f.paths, &stderr, f.opts())
	return code, stderr.String()
}

func TestOverseerMsgAppendsAnOverseerRow(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	body := `{"kind":"notice","detail":"x"}`
	if code, stderr := f.overseerMsg("h1", "c1", body); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(`{"ts":` + tsText + `,"crew_id":"c1","from":"overseer:h1","to":"dispatcher:c1","kind":"msg","body":"{\"kind\":\"notice\",\"detail\":\"x\"}"}`)
}

func TestOverseerMsgTakesTheCrewFromTheRequest(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	if code, stderr := f.overseerMsg("h1", "other", "hi"); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(`{"ts":` + tsText + `,"crew_id":"other","from":"overseer:h1","to":"dispatcher:other","kind":"msg","body":"hi"}`)
}

func TestOverseerMsgPostsARowAtTheLineMax(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	const bare = `{"ts":` + tsText + `,"crew_id":"c1","from":"overseer:h1","to":"dispatcher:c1","kind":"msg","body":""}`
	body := strings.Repeat("x", 4096-len(bare))
	if code, stderr := f.overseerMsg("h1", "c1", body); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	got := f.lines()
	if len(got) != 1 {
		t.Fatalf("log has %d rows, want 1", len(got))
	}
	if len(got[0]) != 4096 || strings.Contains(got[0], "elided") {
		t.Errorf("row is %d bytes, want an untouched 4096-byte row", len(got[0]))
	}
}

func TestOverseerMsgRefusesAnUnmarkedSession(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	delete(f.env, "CREW_OVERSEER")
	code, stderr := f.overseerMsg("h1", "c1", "hi")
	reason := sender.Check(t.Context(), "overseer:h1", "c1", f.opts().Sender)
	if reason == "" {
		t.Fatal("fixture does not trip the sender check")
	}
	if want := "crew: overseer msg: refusing sender 'overseer:h1' — " + reason + "\n"; code != 1 || stderr != want {
		t.Errorf("got %d %q, want 1 %q", code, stderr, want)
	}
	if _, err := os.Stat(f.paths.Dir); !os.IsNotExist(err) {
		t.Errorf("bus dir created on a refused sender: %v", err)
	}
	f.wantNoRow()
}

func TestOverseerMsgRefusesARowThatWouldBeElided(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	// Each \" in the body encodes to four bytes, so the 3400-byte body overflows the line.
	body := `{"detail":"` + strings.Repeat(`\"`, 1700) + `"}`
	code, stderr := f.overseerMsg("h1", "c1", body)
	line := `{"ts":` + tsText + `,"crew_id":"c1","from":"overseer:h1","to":"dispatcher:c1","kind":"msg","body":` + jsonStr(body) + `}`
	if len(line) <= 4096 {
		t.Fatalf("fixture row is %d bytes, not over the line", len(line))
	}
	want := fmt.Sprintf("crew: overseer msg: the encoded row is %d bytes, over the 4096-byte bus line, so the body would be elided\n", len(line))
	if code != 1 || stderr != want {
		t.Errorf("got %d %q, want 1 %q", code, stderr, want)
	}
	f.wantNoRow()
}
