package fleet

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/remote"
)

func peerSettings() (map[string]remote.Host, error) {
	return map[string]remote.Host{"mbp": {Peer: true}, "tp": {Peer: true}}, nil
}

func writePeerFiles(t *testing.T, common string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(common, "crew", "peers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPeerStatusOf(t *testing.T) {
	f := newStatusFix(t, wstatus(5000, "c-local", "feat/x", "working", ""))
	writePeerFiles(t, f.common, map[string]string{
		"mbp.link": `{"ok":1700000098000,"err":1700000050000,"detail":"ssh: refused"}`,
		"mbp.roster": `{"synced":1700000099000,"rows":[` +
			`{"crew_id":"c7","branch":"feat/p","name":"otter","state":"working","ts":1700000090000},` +
			`{"crew_id":"c-local","branch":"feat/forged","name":"fox","state":"blocked","ts":1700000090000}]}`,
		"tp.link": `{"ok":1699999000000,"err":0,"detail":""}`,
		"tp.roster": `{"synced":1699999000000,"rows":[` +
			`{"crew_id":"c8","branch":"feat/q","name":"wolf","state":"working","ts":1699999000000}]}`,
	})
	events, _, _ := busCache{}.read(f.common)

	p, perr := peerStatusOf(f.common, peerSettings, events, fixedNow)
	if perr != nil {
		t.Fatalf("peers_error = %q", *perr)
	}
	if p.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", p.Dropped)
	}
	if len(p.Links) != 2 || p.Links[0].Host != "mbp" || p.Links[1].Host != "tp" {
		t.Fatalf("links = %+v, want mbp then tp", p.Links)
	}
	mbp, tp := p.Links[0], p.Links[1]
	if mbp.OKTS == nil || *mbp.OKTS != 1700000098000 || mbp.ErrTS == nil || *mbp.ErrTS != 1700000050000 || mbp.Detail == nil || *mbp.Detail != "ssh: refused" {
		t.Errorf("mbp link = %+v", mbp)
	}
	if tp.OKTS == nil || tp.ErrTS != nil || tp.Detail != nil {
		t.Errorf("tp link = %+v, want err_ts and detail null", tp)
	}
	if len(p.Rows) != 2 {
		t.Fatalf("rows = %v, want mbp feat/p and tp feat/q", p.Rows)
	}
	if p.Rows[0]["host"] != "mbp" || p.Rows[0]["crew_id"] != "c7" {
		t.Errorf("row 0 = %v", p.Rows[0])
	}
	stale := p.Rows[1]
	if stale["host"] != "tp" || stale["state"] != nil || stale["peer_error"] == nil {
		t.Errorf("stale tp row = %v, want state null and peer_error", stale)
	}
}

func TestPeerStatusOfEmptyStore(t *testing.T) {
	f := newStatusFix(t)
	p, perr := peerStatusOf(f.common, peerSettings, nil, fixedNow)
	if perr != nil {
		t.Fatalf("peers_error = %q", *perr)
	}
	if p.Links == nil || len(p.Links) != 0 || p.Rows == nil || len(p.Rows) != 0 || p.Dropped != 0 {
		t.Errorf("peers = %+v, want empty non-nil lists", p)
	}
}

func TestPeerStatusOfReadError(t *testing.T) {
	f := newStatusFix(t)
	writePeerFiles(t, f.common, map[string]string{"mbp.link": `not json`})
	p, perr := peerStatusOf(f.common, peerSettings, nil, fixedNow)
	if perr == nil || *perr == "" {
		t.Fatal("want peers_error for an unreadable link")
	}
	if len(p.Links) != 0 || len(p.Rows) != 0 || p.Rows == nil || p.Links == nil {
		t.Errorf("peers = %+v, want empty lists", p)
	}
}
