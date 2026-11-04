package watch

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

func overseerHosts() *sender.OverseerHosts {
	return sender.NewOverseerHosts(sender.Probes{
		Self:  func() (string, error) { return "halo", nil },
		Hosts: func() (map[string]remote.Host, error) { return map[string]remote.Host{"mbp": {}}, nil },
	})
}

func overseerRow(ts int64, from string) string {
	return `{"ts":` + itoa(ts) + `,"crew_id":"c1","from":"` + from +
		`","to":"dispatcher:c1","kind":"msg","body":"SECRETBODY ` + from + `"}`
}

func overseerLog() []string {
	return []string{overseerRow(1, "overseer:mbp"), overseerRow(2, "overseer:halo"), overseerRow(3, "overseer:evil")}
}

func assertForeignRefused(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "SECRETBODY overseer:mbp") || !strings.Contains(out, "SECRETBODY overseer:halo") {
		t.Errorf("trusted overseer rows altered: %s", out)
	}
	if strings.Contains(out, "SECRETBODY overseer:evil") {
		t.Errorf("foreign body leaked: %s", out)
	}
	if !strings.Contains(out, `"body":null`) || !strings.Contains(out, `"refused":"overseer host evil is not in settings hosts"`) {
		t.Errorf("no replacement row: %s", out)
	}
}

func TestForeignOverseerRowIsReplacedAndCursorAdvances(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, overseerLog()...)

	out, _, code, _ := runOpts(t, p, Options{Hosts: overseerHosts()}, Request{Timeout: "1", Interval: "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	assertForeignRefused(t, out)
	if got := cursorOf(t, p, "c1"); got != "3" {
		t.Errorf("cursor = %s, want 3", got)
	}
}

func TestNilHostsRefusesEveryOverseer(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, overseerLog()...)

	out, _, _, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if strings.Contains(out, "SECRETBODY") {
		t.Errorf("a body leaked with nil Hosts: %s", out)
	}
	if n := strings.Count(out, `"refused"`); n != 3 {
		t.Errorf("refused rows = %d, want 3: %s", n, out)
	}
}

func TestUnreadableHostsRefusesOnlyTheNonSelf(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, overseerLog()...)
	h := sender.NewOverseerHosts(sender.Probes{
		Self:  func() (string, error) { return "halo", nil },
		Hosts: func() (map[string]remote.Host, error) { return nil, io.ErrUnexpectedEOF },
	})

	out, _, _, _ := runOpts(t, p, Options{Hosts: h}, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if strings.Contains(out, "SECRETBODY overseer:evil") || strings.Contains(out, "SECRETBODY overseer:mbp") {
		t.Errorf("a non-self body leaked: %s", out)
	}
	if !strings.Contains(out, "SECRETBODY overseer:halo") || strings.Count(out, `"refused"`) != 2 {
		t.Errorf("self not passed or wrong refusals: %s", out)
	}
}

func TestRelayReplacesForeignOverseerRow(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, overseerLog()...)

	pk := &park{now: base}
	var out bytes.Buffer
	Relay(RelayRequest{Since: since("0")}, p, &out, io.Discard, Options{
		CrewID: func() string { return "c1" },
		Clock:  clock.Clock{Now: pk.Now},
		Hosts:  overseerHosts(),
		Sleep:  func(string, io.Writer) error { return errStop },
	})
	assertForeignRefused(t, out.String())
}
