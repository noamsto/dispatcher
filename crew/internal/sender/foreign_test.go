package sender

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/remote"
)

// counted is halo's OverseerHosts with settings hosts {mbp}, counting each
// settings and host-name read.
func counted(hostsErr error) (*OverseerHosts, *atomic.Int32, *atomic.Int32) {
	var hostsCalls, selfCalls atomic.Int32
	h := NewOverseerHosts(Probes{
		Self: func() (string, error) { selfCalls.Add(1); return "halo", nil },
		Hosts: func() (map[string]remote.Host, error) {
			hostsCalls.Add(1)
			if hostsErr != nil {
				return nil, hostsErr
			}
			return map[string]remote.Host{"mbp": {SSH: "mbp"}}, nil
		},
	})
	return h, &hostsCalls, &selfCalls
}

func msgRow(from string) map[string]any {
	return map[string]any{"ts": float64(1700), "kind": "msg", "crew_id": "c1", "to": "dispatcher:c1", "from": from, "body": "do the thing", "re": "q1"}
}

func evilReplacement() map[string]any {
	return map[string]any{
		"ts": float64(1700), "kind": "msg", "crew_id": "c1", "to": "dispatcher:c1",
		"from": "overseer:evil", "body": nil, "refused": "overseer host evil is not in settings hosts",
	}
}

func TestForeignOverseer(t *testing.T) {
	tests := []struct {
		name    string
		row     map[string]any
		want    map[string]any
		refused bool
	}{
		{"self passes", msgRow("overseer:halo"), msgRow("overseer:halo"), false},
		{"settings host passes", msgRow("overseer:mbp"), msgRow("overseer:mbp"), false},
		{"foreign host replaced", msgRow("overseer:evil"), evilReplacement(), true},
		{"worker untouched", msgRow("worker:feat/x"), msgRow("worker:feat/x"), false},
		{"dispatcher untouched", msgRow("dispatcher:c1"), msgRow("dispatcher:c1"), false},
		{"no from untouched", map[string]any{"ts": float64(1), "kind": "msg"}, map[string]any{"ts": float64(1), "kind": "msg"}, false},
		{"non-string from untouched", map[string]any{"from": float64(3)}, map[string]any{"from": float64(3)}, false},
		{"empty host replaced", map[string]any{"ts": float64(2), "kind": "msg", "crew_id": "c1", "from": "overseer:"},
			map[string]any{"ts": float64(2), "kind": "msg", "crew_id": "c1", "from": "overseer:", "body": nil, "refused": "overseer host  is not in settings hosts"}, true},
		{"no to omits to", map[string]any{"ts": float64(2), "kind": "msg", "crew_id": "c1", "from": "overseer:evil", "body": "x"},
			map[string]any{"ts": float64(2), "kind": "msg", "crew_id": "c1", "from": "overseer:evil", "body": nil, "refused": "overseer host evil is not in settings hosts"}, true},
		{"hostile from sanitized", map[string]any{"ts": float64(3), "kind": "msg", "crew_id": "c1", "to": "x", "from": "overseer:ev il\x1b[2J\n"},
			map[string]any{"ts": float64(3), "kind": "msg", "crew_id": "c1", "to": "x", "from": "overseer:ev?il??2J?", "body": nil, "refused": "overseer host ev?il??2J? is not in settings hosts"}, true},
		{"long from capped", map[string]any{"ts": float64(4), "kind": "msg", "crew_id": "c1", "from": "overseer:" + strings.Repeat("é", 300)},
			map[string]any{"ts": float64(4), "kind": "msg", "crew_id": "c1", "from": "overseer:" + strings.Repeat("?", 191), "body": nil, "refused": "overseer host " + strings.Repeat("?", 200) + " is not in settings hosts"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := counted(nil)
			in := maps.Clone(tc.row)
			got, refused := ForeignOverseer(tc.row, h)
			if refused != tc.refused || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ForeignOverseer(%v) = %v, %v; want %v, %v", tc.row, got, refused, tc.want, tc.refused)
			}
			if !reflect.DeepEqual(tc.row, in) {
				t.Errorf("input row mutated: %v, was %v", tc.row, in)
			}
		})
	}
}

func TestForeignOverseerLoadsSettingsOnce(t *testing.T) {
	h, hostsCalls, _ := counted(nil)
	for _, from := range []string{"worker:feat/x", "dispatcher:c1", "overseer:halo", "role:feat/x:reviewer", "overseer:halo"} {
		ForeignOverseer(msgRow(from), h)
	}
	if n := hostsCalls.Load(); n != 0 {
		t.Fatalf("settings read %d times with no foreign overseer sender, want 0", n)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			from := "overseer:evil"
			if i%2 == 0 {
				from = "overseer:mbp"
			}
			ForeignOverseer(msgRow(from), h)
		})
	}
	wg.Wait()
	if n := hostsCalls.Load(); n != 1 {
		t.Errorf("settings read %d times across many rows, want 1", n)
	}
}

func TestForeignOverseerNonOverseerLoadsNothing(t *testing.T) {
	h, hostsCalls, selfCalls := counted(nil)
	for _, from := range []string{"worker:feat/x", "dispatcher:c1", "peer:mbp", ""} {
		ForeignOverseer(msgRow(from), h)
	}
	if hostsCalls.Load() != 0 || selfCalls.Load() != 0 {
		t.Errorf("non-overseer rows read settings %d and host name %d times, want 0", hostsCalls.Load(), selfCalls.Load())
	}
}

func TestForeignOverseerHostsUnreadable(t *testing.T) {
	h, _, _ := counted(errors.New("bad\x1b[2J\njson"))
	if got, refused := ForeignOverseer(msgRow("overseer:mbp"), h); !refused || got["refused"] != "settings hosts cannot be read (bad?[2J?json)" {
		t.Errorf("overseer:mbp with unreadable settings = %v, %v; want refused", got, refused)
	}
	if _, refused := ForeignOverseer(msgRow("overseer:halo"), h); refused {
		t.Error("overseer:halo refused with unreadable settings, want self to pass")
	}

	nilHosts := NewOverseerHosts(Probes{Self: func() (string, error) { return "halo", nil }})
	for from, want := range map[string]bool{"overseer:halo": false, "overseer:mbp": true, "overseer:evil": true} {
		if _, refused := ForeignOverseer(msgRow(from), nilHosts); refused != want {
			t.Errorf("nil Hosts: %s refused = %v, want %v", from, refused, want)
		}
	}
}

func TestOverseerHostsSelfUnreadable(t *testing.T) {
	h := NewOverseerHosts(Probes{
		Self:  func() (string, error) { return "", errors.New("boom") },
		Hosts: func() (map[string]remote.Host, error) { return map[string]remote.Host{"mbp": {}}, nil },
	})
	for _, from := range []string{"overseer:halo", "overseer:mbp", "overseer:evil"} {
		if _, foreign := h.Foreign(from); !foreign {
			t.Errorf("Foreign(%q) with unreadable host name = false, want true", from)
		}
	}
}

func TestOverseerHostsForeign(t *testing.T) {
	h, _, _ := counted(nil)
	tests := []struct {
		from, host string
		foreign    bool
	}{
		{"worker:feat/x", "", false},
		{"overseer:halo", "halo", false},
		{"overseer:mbp", "mbp", false},
		{"overseer:evil", "evil", true},
		{"overseer:MBP", "MBP", true},
	}
	for _, tc := range tests {
		if host, foreign := h.Foreign(tc.from); host != tc.host || foreign != tc.foreign {
			t.Errorf("Foreign(%q) = %q, %v; want %q, %v", tc.from, host, foreign, tc.host, tc.foreign)
		}
	}

	var none *OverseerHosts
	if host, foreign := none.Foreign("overseer:halo"); host != "halo" || !foreign {
		t.Errorf("nil Foreign(overseer:halo) = %q, %v; want halo, true", host, foreign)
	}
	if host, foreign := none.Foreign("worker:feat/x"); host != "" || foreign {
		t.Errorf("nil Foreign(worker:feat/x) = %q, %v; want \"\", false", host, foreign)
	}
}

// flaky is halo's OverseerHosts with settings hosts {mbp} on a clock the test
// sets; a probe fails with its error while that is non-nil, and each read is
// counted.
type flaky struct {
	h                     *OverseerHosts
	now                   time.Time
	selfErr, hostsErr     error
	selfCalls, hostsCalls int
}

func newFlaky() *flaky {
	f := &flaky{now: time.Unix(1700000000, 0)}
	f.h = NewOverseerHosts(Probes{
		Self: func() (string, error) { f.selfCalls++; return "halo", f.selfErr },
		Hosts: func() (map[string]remote.Host, error) {
			f.hostsCalls++
			if f.hostsErr != nil {
				return nil, f.hostsErr
			}
			return map[string]remote.Host{"mbp": {}}, nil
		},
	})
	f.h.now = func() time.Time { return f.now }
	return f
}

// refused is the refusal ForeignOverseer states for a row from from, "" when
// the row passes.
func (f *flaky) refused(from string) string {
	got, refused := ForeignOverseer(msgRow(from), f.h)
	if !refused {
		return ""
	}
	return got["refused"].(string)
}

func TestOverseerHostsRetriesUnreadableSettings(t *testing.T) {
	const cause = "settings hosts cannot be read (exit status 1)"
	f := newFlaky()
	f.hostsErr = errors.New("exit status 1")
	for range 5 {
		if got := f.refused("overseer:mbp"); got != cause {
			t.Fatalf("overseer:mbp with unreadable settings refused %q, want %q", got, cause)
		}
	}
	if f.hostsCalls != 1 {
		t.Errorf("settings read %d times within 30s, want 1", f.hostsCalls)
	}

	f.hostsErr = nil
	f.now = f.now.Add(29 * time.Second)
	if got := f.refused("overseer:mbp"); got != cause || f.hostsCalls != 1 {
		t.Errorf("29s later: refused %q after %d reads, want %q after 1", got, f.hostsCalls, cause)
	}
	f.now = f.now.Add(time.Second)
	if got := f.refused("overseer:mbp"); got != "" || f.hostsCalls != 2 {
		t.Errorf("30s later: refused %q after %d reads, want a pass after 2", got, f.hostsCalls)
	}
	f.now = f.now.Add(time.Hour)
	if got := f.refused("overseer:evil"); got != "overseer host evil is not in settings hosts" || f.hostsCalls != 2 {
		t.Errorf("after a good read: refused %q after %d reads, want not-in-hosts after 2", got, f.hostsCalls)
	}
}

func TestOverseerHostsRetriesUnreadableSelf(t *testing.T) {
	const cause = "this host's name cannot be read (boom)"
	f := newFlaky()
	f.selfErr = errors.New("boom")
	for _, from := range []string{"overseer:halo", "overseer:mbp", "overseer:halo"} {
		if got := f.refused(from); got != cause {
			t.Fatalf("%s with an unreadable host name refused %q, want %q", from, got, cause)
		}
	}
	if f.selfCalls != 1 || f.hostsCalls != 0 {
		t.Errorf("host name read %d and settings %d times, want 1 and 0", f.selfCalls, f.hostsCalls)
	}

	f.selfErr = nil
	f.now = f.now.Add(30 * time.Second)
	if got := f.refused("overseer:halo"); got != "" || f.selfCalls != 2 {
		t.Errorf("30s later: overseer:halo refused %q after %d reads, want a pass after 2", got, f.selfCalls)
	}
	if got := f.refused("overseer:mbp"); got != "" || f.selfCalls != 2 {
		t.Errorf("after a good read: overseer:mbp refused %q after %d host-name reads, want a pass after 2", got, f.selfCalls)
	}
}

func TestOverseerHostsUnreadableReadOncePerWindow(t *testing.T) {
	h, hostsCalls, _ := counted(errors.New("bad json"))
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { ForeignOverseer(msgRow("overseer:mbp"), h) })
	}
	wg.Wait()
	if n := hostsCalls.Load(); n != 1 {
		t.Errorf("unreadable settings read %d times across many rows, want 1", n)
	}
}
