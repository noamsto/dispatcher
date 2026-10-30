package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	crewstatus "github.com/noamsto/dispatcher/crew/internal/status"
)

const peerSlug = "acme/widget"

// peerSide is one fake host: a clone with origin acme/widget and its settings.
type peerSide struct {
	name  string
	repo  string
	paths bus.Paths
	hosts map[string]remote.Host
}

func newPeerSide(t *testing.T, name string) *peerSide {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "config", "remote.origin.url", "git@github.com:acme/widget.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	paths, err := bus.Locate(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return &peerSide{name: name, repo: dir, paths: paths, hosts: map[string]remote.Host{}}
}

// peerPair is mbp and halo, each a peer: true host in the other's settings.
func peerPair(t *testing.T) (mbp, halo *peerSide) {
	t.Helper()
	mbp, halo = newPeerSide(t, "mbp"), newPeerSide(t, "halo")
	mbp.hosts[halo.name] = remote.Host{SSH: halo.name, Peer: true, Repos: map[string]string{peerSlug: halo.repo}}
	halo.hosts[mbp.name] = remote.Host{SSH: mbp.name, Peer: true, Repos: map[string]string{peerSlug: mbp.repo}}
	return mbp, halo
}

func (s *peerSide) settings() (map[string]remote.Host, error) { return s.hosts, nil }

// msg posts a dispatcher msg to peer:<to> on this side's bus, as `crew msg` does.
func (s *peerSide) msg(t *testing.T, to, body string) {
	t.Helper()
	var errb bytes.Buffer
	rc := crewstatus.RunMsg(t.Context(), crewstatus.MsgRequest{From: "dispatcher:c1", To: "peer:" + to, Body: body}, s.paths, &errb, crewstatus.Options{
		CrewID:    func() string { return "c1" },
		Dir:       s.repo,
		PeerHosts: s.settings,
		// The sender check reads no env, so a run inside a worker's env still posts.
		Sender: sender.Probes{Getenv: func(string) string { return "" }},
	})
	if rc != 0 {
		t.Fatalf("msg to peer:%s: rc %d: %s", to, rc, errb.String())
	}
}

// serveOn is the in-process transport to p: it checks the remote-exec request
// and runs peer.Serve in p's clone, as `crew remote-exec` would.
func serveOn(t *testing.T, p *peerSide) peer.Transport {
	return func(_ context.Context, dest string, request []byte) ([]byte, error) {
		var req remote.Request
		if err := json.Unmarshal(request, &req); err != nil {
			t.Errorf("request: %v", err)
			return nil, err
		}
		if dest != p.name || !slices.Equal(req.Argv, []string{"crew", "peer-sync"}) || req.Repo != p.repo ||
			req.Slug != peerSlug || req.Stdin == nil {
			t.Errorf("dest %q, request %+v", dest, req)
			return nil, errors.New("bad request")
		}
		var out, errb bytes.Buffer
		if rc := peer.Serve(strings.NewReader(*req.Stdin), &out, &errb, peer.ServeOptions{
			Paths: p.paths, Self: p.name, Hosts: p.settings,
		}); rc != 0 {
			line, _, _ := strings.Cut(errb.String(), "\n")
			return nil, fmt.Errorf("exit %d: %s", rc, line)
		}
		return out.Bytes(), nil
	}
}

// syncTo is one exchange from l with its peer p, outside any stream.
func syncTo(t *testing.T, l, p *peerSide) {
	t.Helper()
	h := l.hosts[p.name]
	if _, err := peer.Sync(t.Context(), peer.SyncOptions{
		Paths:     l.paths,
		Self:      l.name,
		Name:      p.name,
		Target:    remote.Target{SSH: h.SSH, Repo: h.Repos[peerSlug]},
		Slug:      peerSlug,
		Transport: serveOn(t, p),
	}); err != nil {
		t.Fatalf("sync %s→%s: %v", l.name, p.name, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// waitWithin is waitFor under a deadline of the case's own.
func waitWithin(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", d, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func peerLane(t *testing.T, s *peerSide, hooks peerHooks) *lane {
	t.Helper()
	return startLane(t, s.paths, entryStub(t, map[string]string{"watch": parkedWatch("0")}), Request{}, laneOpts{peer: hooks})
}

// noPeers is the hosts hook of a host that dials nobody.
func noPeers() (map[string]remote.Host, error) { return map[string]remote.Host{}, nil }

func TestPeerMsgWakesStream(t *testing.T) {
	mbp, halo := peerPair(t)
	mbp.msg(t, "halo", "deploy is yours")

	l := peerLane(t, halo, peerHooks{hosts: noPeers, wake: 50 * time.Millisecond})
	syncTo(t, mbp, halo)

	waitWithin(t, 5*time.Second, "the peer_msg line: "+l.raw(), func() bool { return len(l.ofKind("peer_msg")) == 1 })
	got := l.ofKind("peer_msg")[0]
	if got["crew"] != "c1" || got["peer"] != "mbp" || got["from"] != "dispatcher:c1" || got["body"] != "deploy is yours" {
		t.Errorf("peer_msg = %v", got)
	}
	if _, ok := got["ts"].(float64); !ok {
		t.Errorf("peer_msg ts = %#v", got["ts"])
	}
	if want := `{"stream":"peer_msg","crew":"c1","peer":"mbp","from":"dispatcher:c1","body":"deploy is yours","ts":`; !strings.Contains(l.raw(), want) {
		t.Errorf("line %q, want it to start %q", l.raw(), want)
	}
	store := peer.StoreFor(halo.paths)
	size := fileSize(t, filepath.Join(store.Dir, "mbp.jsonl"))
	waitFor(t, "mbp.seen at the file's end", func() bool { return store.Seen("mbp") == size })
	l.stop()

	// A restarted lane announces only what is new: the second message is the
	// proof its wake ran past the first.
	l = peerLane(t, halo, peerHooks{hosts: noPeers, wake: 50 * time.Millisecond})
	mbp.msg(t, "halo", "second")
	syncTo(t, mbp, halo)
	waitWithin(t, 5*time.Second, "the second peer_msg: "+l.raw(), func() bool { return len(l.ofKind("peer_msg")) >= 1 })
	if msgs := l.ofKind("peer_msg"); len(msgs) != 1 || msgs[0]["body"] != "second" {
		t.Errorf("restarted lane printed %v, want only the second message", msgs)
	}
	if text := l.errb.str(); text != "" {
		t.Errorf("stderr %q", text)
	}
}

func TestPeerSyncFromStream(t *testing.T) {
	mbp, halo := peerPair(t)
	mbp.msg(t, "halo", "from mbp")

	l := peerLane(t, mbp, peerHooks{
		hosts:     mbp.settings,
		transport: serveOn(t, halo),
		every:     100 * time.Millisecond,
		wake:      50 * time.Millisecond,
		self:      mbp.name,
	})
	haloStore := peer.StoreFor(halo.paths)
	waitWithin(t, 5*time.Second, "halo's peers/mbp.jsonl", func() bool {
		msgs, _, err := peer.Messages(haloStore, "mbp", 0)
		return err == nil && len(msgs) == 1 && msgs[0].Body == "from mbp"
	})

	halo.msg(t, "mbp", "from halo")
	waitWithin(t, 5*time.Second, "halo's msg on mbp's lane: "+l.raw(), func() bool { return len(l.ofKind("peer_msg")) >= 1 })
	got := l.ofKind("peer_msg")
	if len(got) != 1 || got[0]["peer"] != "halo" || got[0]["from"] != "dispatcher:c1" || got[0]["body"] != "from halo" {
		t.Errorf("peer_msg lines = %v", got)
	}
	if errs := l.ofKind("error"); len(errs) != 0 {
		t.Errorf("error lines = %v", errs)
	}
}

func TestPeerSyncErrorReportedOncePerHeartbeat(t *testing.T) {
	mbp, _ := peerPair(t)
	var calls atomic.Int64
	l := peerLane(t, mbp, peerHooks{
		hosts: mbp.settings,
		transport: func(context.Context, string, []byte) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New("ssh: exit 255: connection refused")
		},
		every: 50 * time.Millisecond,
		self:  mbp.name,
	})
	l.waitKind("error", 1)
	after := calls.Load()
	waitFor(t, "three more sync rounds", func() bool { return calls.Load() >= after+3 })

	errs := l.ofKind("error")
	if len(errs) != 1 {
		t.Fatalf("error lines = %v, want one within the heartbeat", errs)
	}
	e := errs[0]
	if e["crew"] != "c1" || e["host"] != "halo" || e["rc"] != float64(1) || e["detail"] != "peer halo: ssh: exit 255: connection refused" {
		t.Errorf("error line = %v", e)
	}
}

func TestPeerSettingsErrorIsSilent(t *testing.T) {
	mbp, _ := peerPair(t)
	var calls atomic.Int64
	l := peerLane(t, mbp, peerHooks{
		hosts: func() (map[string]remote.Host, error) {
			calls.Add(1)
			return nil, errors.New("dispatch-config exploded")
		},
		transport: func(context.Context, string, []byte) ([]byte, error) {
			t.Error("dialed with no settings")
			return nil, errors.New("unreachable")
		},
		every: 50 * time.Millisecond,
	})
	waitFor(t, "three settings reads", func() bool { return calls.Load() >= 3 })
	if text := l.raw(); text != "" {
		t.Errorf("stdout %q, want nothing", text)
	}
	if text := l.errb.str(); text != "" {
		t.Errorf("stderr %q", text)
	}
}
