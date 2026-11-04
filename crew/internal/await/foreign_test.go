package await

import (
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

func overseerHosts() *sender.OverseerHosts {
	return sender.NewOverseerHosts(sender.Probes{
		Self:  func() (string, error) { return "halo", nil },
		Hosts: func() (map[string]remote.Host, error) { return map[string]remote.Host{"mbp": {}}, nil },
	})
}

func TestForeignOverseerReplyIsReplacedAndMarked(t *testing.T) {
	cases := []struct {
		from    string
		refused bool
	}{
		{"overseer:mbp", false},
		{"overseer:halo", false},
		{"overseer:evil", true},
	}
	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			p := fixture(t)
			writeLog(t, p, msg(1, tc.from, disp, "SECRETBODY"))
			out, _, code := runOpts(t, p, Options{Hosts: overseerHosts()}, Request{Me: disp, Timeout: "5"})
			if code != 0 {
				t.Fatalf("code %d", code)
			}
			if got := strings.Contains(out, "SECRETBODY"); got == tc.refused {
				t.Errorf("body present = %v, out %s", got, out)
			}
			if tc.refused {
				if !strings.Contains(out, `"body":null`) || !strings.Contains(out, `"refused":"overseer host evil is not in settings hosts"`) {
					t.Errorf("no replacement row: %s", out)
				}
			}
			if out == "" {
				t.Fatal("nothing delivered")
			}
			if m := readMarks(t, p, disp); !strings.Contains(m, tc.from) {
				t.Errorf("marks = %q, want %s marked", m, tc.from)
			}
		})
	}
}

func TestNilHostsRefusesEveryOverseerReply(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, msg(1, "overseer:halo", disp, "SECRETBODY"))
	out, _, _ := run(t, p, Request{Me: disp, Timeout: "5"})
	if strings.Contains(out, "SECRETBODY") || !strings.Contains(out, `"refused"`) {
		t.Errorf("nil Hosts must refuse: %s", out)
	}
}
