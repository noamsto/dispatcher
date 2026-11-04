package inbox

import (
	"bytes"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

func overseerHosts() *sender.OverseerHosts {
	return sender.NewOverseerHosts(sender.Probes{
		Self:  func() (string, error) { return "halo", nil },
		Hosts: func() (map[string]remote.Host, error) { return map[string]remote.Host{"mbp": {}}, nil },
	})
}

func overseerLog(to string) string {
	var b strings.Builder
	for i, from := range []string{"overseer:mbp", "overseer:halo", "overseer:evil"} {
		b.WriteString(`{"ts":` + string(rune('1'+i)) + `,"crew_id":"c1","from":"` + from +
			`","to":"` + to + `","kind":"msg","body":"SECRETBODY ` + from + `"}` + "\n")
	}
	return b.String()
}

func runHosts(t *testing.T, p bus.Paths, h *sender.OverseerHosts, me string) string {
	t.Helper()
	var out bytes.Buffer
	code := Run(request(me), p, &out, &bytes.Buffer{}, Options{CrewID: func() string { return "c1" }, Hosts: h})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	return out.String()
}

func TestForeignOverseerRowIsReplaced(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, overseerLog("dispatcher:c1"))

	out := runHosts(t, p, overseerHosts(), "dispatcher:c1")
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

func TestNilHostsRefusesEveryOverseer(t *testing.T) {
	p := fixture(t)
	writeLog(t, p, overseerLog("dispatcher:c1"))

	out := runHosts(t, p, nil, "dispatcher:c1")
	if strings.Contains(out, "SECRETBODY") || strings.Count(out, `"refused"`) != 3 {
		t.Errorf("nil Hosts must refuse all three: %s", out)
	}
}

func TestForeignOverseerRowAdvancesWorkerMarks(t *testing.T) {
	p := fixture(t)
	const me = "worker:feat/x#s1-1"
	writeLog(t, p, overseerLog(me))

	out := runHosts(t, p, overseerHosts(), me)
	if strings.Contains(out, "SECRETBODY overseer:evil") {
		t.Errorf("foreign body leaked: %s", out)
	}
	if m := readMarks(t, p, "c1", me); !strings.Contains(m, `"overseer:evil":3`) {
		t.Errorf("marks = %q, want evil row marked", m)
	}
}
