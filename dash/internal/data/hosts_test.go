package data

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestHostsFromSettings(t *testing.T) {
	rows := fleetSettings(true).Rows
	rows = append(rows, leaf(`"x"`, "other", "thing"), leaf(`"y"`, "hosts", "aaa", "chrome"))
	got := HostsFromSettings(rows)
	if len(got) != 5 {
		t.Fatalf("hosts = %+v", got)
	}
	names := []string{got[0].Name, got[1].Name, got[2].Name, got[3].Name, got[4].Name}
	want := []string{"aaa", "box", "halo", "mbp", "tp"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
	box := got[1]
	if box.SSH != "box.lan" || !box.Peer || box.Tailscale == nil || *box.Tailscale != "box-nas" {
		t.Errorf("box = %+v", box)
	}
	if h := got[2]; h.SSH != "halo.lan" || h.Peer || h.Tailscale != nil {
		t.Errorf("halo = %+v", h)
	}
	if !TailscaleEnabled(rows) || TailscaleEnabled(fleetSettings(false).Rows) {
		t.Errorf("TailscaleEnabled wrong")
	}
	if TailscaleEnabled([]SettingRow{leaf(`"true"`, "dashTailscale")}) {
		t.Errorf("a string is not true")
	}
}

func TestTailscaleParse(t *testing.T) {
	doc, err := ParseTailscale(readTestdata(t, "tailscale_status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Self.HostName != "lap" || len(doc.Peers) != 6 {
		t.Fatalf("doc = %+v", doc)
	}
	hosts := HostsFromSettings(fleetSettings(true).Rows)
	reach, suggested := ReachFromTailscale(hosts, doc)
	byHost := map[string]HostReach{}
	for _, r := range reach {
		byHost[r.Host] = r
	}
	halo := byHost["halo"]
	if halo.Source != "tailscale" || halo.Online == nil || !*halo.Online || halo.LastSeen == nil || *halo.LastSeen != 1794821400 {
		t.Errorf("halo = %+v", halo)
	}
	if tp := byHost["tp"]; tp.Online == nil || *tp.Online || tp.LastSeen == nil { // HostName "TP" matches "tp"
		t.Errorf("tp = %+v", tp)
	}
	if mbp := byHost["mbp"]; mbp.Online == nil || !*mbp.Online { // matched by DNSName first label "MBP"
		t.Errorf("mbp = %+v", mbp)
	}
	box := byHost["box"] // tailscale mapping box-nas; zero LastSeen
	if box.Online == nil || *box.Online || box.LastSeen != nil || box.Error != nil {
		t.Errorf("box = %+v", box)
	}
	names := []string{}
	for _, s := range suggested {
		names = append(names, s.Name)
	}
	if len(names) != 2 || names[0] != "evil\x1b[31mhost" || names[1] != "phone" || !suggested[0].Online || suggested[1].Online {
		t.Errorf("suggested = %+v", suggested)
	}

	reach, _ = ReachFromTailscale([]HostConfig{{Name: "ghost", SSH: "g"}}, doc)
	if reach[0].Online != nil || reach[0].Error == nil || *reach[0].Error != "not in tailnet" || reach[0].Source != "tailscale" {
		t.Errorf("ghost = %+v", reach[0])
	}

	if _, err := ParseTailscale([]byte("garbage")); err == nil {
		t.Errorf("garbage must fail")
	}
	if _, err := ParseTailscale([]byte(`{"BackendState":"NeedsLogin"}`)); err == nil {
		t.Errorf("logged-out document must fail")
	}
}

func TestTailscaleSuggestedCap(t *testing.T) {
	doc := TailscaleDoc{Self: TailscaleNode{HostName: "me"}}
	for i := 0; i < 30; i++ {
		doc.Peers = append(doc.Peers, TailscaleNode{HostName: string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	_, suggested := ReachFromTailscale(nil, doc)
	if len(suggested) != 20 || suggested[0].Name != "aa" {
		t.Errorf("suggested = %d first %+v", len(suggested), suggested[0])
	}
}

func TestTailscaleMissingFallsBackToSSH(t *testing.T) {
	for _, tsErr := range []error{FakeExitError{Code: 127}, os_ErrNotFound{}} {
		r := fleetRunner(t)
		allSSH(r)
		r.Responses[tailscaleKey] = FakeResponse{Err: tsErr}
		r.Responses[sshKey("tp")] = FakeResponse{Stderr: []byte("ssh: connect to host tp: Connection timed out\nhint"), Err: FakeExitError{Code: 255}}
		sec := CollectFleet(context.Background(), r, baseConfig(), true, fleetSettings(true), fixedProber(r))
		if sec.TailscaleError == nil {
			t.Fatalf("tailscale error not set")
		}
		if len(sec.Suggested) != 0 || len(sec.Reach) != 4 {
			t.Fatalf("reach=%+v suggested=%+v", sec.Reach, sec.Suggested)
		}
		for _, re := range sec.Reach {
			if re.Source != "ssh" {
				t.Errorf("%s source = %s", re.Host, re.Source)
			}
		}
		byHost := map[string]HostReach{}
		for _, re := range sec.Reach {
			byHost[re.Host] = re
		}
		if h := byHost["halo"]; h.Online == nil || !*h.Online || h.Error != nil {
			t.Errorf("halo = %+v", h)
		}
		if h := byHost["tp"]; h.Online == nil || *h.Online || h.Error == nil || *h.Error != "ssh: connect to host tp: Connection timed out" {
			t.Errorf("tp = %+v", h)
		}
		probes := 0
		for _, c := range r.CallsSnapshot() {
			if c.Name == "ssh" {
				probes++
				if c.Args[0] != "-o" || c.Args[1] != "BatchMode=yes" || c.Args[2] != "-o" || c.Args[3] != "ConnectTimeout=3" || c.Args[4] != "--" || c.Args[6] != "true" {
					t.Errorf("argv = %v", c.Args)
				}
			}
		}
		if probes != 4 {
			t.Errorf("probes = %d, want 4", probes)
		}
	}
}

type os_ErrNotFound struct{}

func (os_ErrNotFound) Error() string {
	return "exec: \"tailscale\": executable file not found in $PATH"
}

func TestTailscaleDisabledSkipsBinary(t *testing.T) {
	r := fleetRunner(t)
	allSSH(r)
	sec := CollectFleet(context.Background(), r, baseConfig(), false, fleetSettings(false), fixedProber(r))
	for _, n := range callNames(r) {
		if n == "tailscale" {
			t.Fatal("tailscale was called with the setting off")
		}
	}
	if sec.TailscaleError != nil || sec.Reach[0].Source != "ssh" || len(sec.Hosts) != 4 {
		t.Errorf("sec = %+v", sec)
	}
}

func TestTailscaleGarbageFallsBack(t *testing.T) {
	r := fleetRunner(t)
	allSSH(r)
	r.Responses[tailscaleKey] = FakeResponse{Stdout: []byte("garbage")}
	sec := CollectFleet(context.Background(), r, baseConfig(), true, fleetSettings(true), fixedProber(r))
	if sec.TailscaleError == nil || sec.Reach[0].Source != "ssh" {
		t.Errorf("sec = %+v", sec)
	}
}

func TestSSHProbeCache(t *testing.T) {
	r := fleetRunner(t)
	allSSH(r)
	clock := &fakeClock{t: time.Unix(1794800000, 0)}
	p := NewHostProber(r, clock.Now)
	collect := func() {
		CollectFleet(context.Background(), r, baseConfig(), false, fleetSettings(false), p)
	}
	count := func() int {
		n := 0
		for _, c := range r.CallsSnapshot() {
			if c.Name == "ssh" {
				n++
			}
		}
		return n
	}
	collect()
	clock.Advance(59 * time.Second)
	collect()
	if got := count(); got != 4 {
		t.Fatalf("probes within 60s = %d, want 4", got)
	}
	clock.Advance(2 * time.Second)
	collect()
	if got := count(); got != 8 {
		t.Fatalf("probes after 61s = %d, want 8", got)
	}
}
