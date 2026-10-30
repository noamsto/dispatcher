package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

const testSlug = "acme/widget"

// side is one fake host: a clone with origin acme/widget and its settings.
type side struct {
	name  string
	repo  string
	paths bus.Paths
	hosts map[string]remote.Host
}

func newSide(t *testing.T, name string) *side {
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
	return &side{name: name, repo: dir, paths: paths, hosts: map[string]remote.Host{}}
}

// pair is mbp and halo, each a peer: true host in the other's settings.
func pair(t *testing.T) (mbp, halo *side) {
	t.Helper()
	mbp, halo = newSide(t, "mbp"), newSide(t, "halo")
	mbp.hosts[halo.name] = remote.Host{SSH: halo.name, Peer: true, Repos: map[string]string{testSlug: halo.repo}}
	halo.hosts[mbp.name] = remote.Host{SSH: mbp.name, Peer: true, Repos: map[string]string{testSlug: mbp.repo}}
	return mbp, halo
}

func fixedNow() time.Time { return testNow }

func (s *side) serveOptions() ServeOptions {
	return ServeOptions{
		Paths: s.paths,
		Self:  s.name,
		Hosts: func() (map[string]remote.Host, error) { return s.hosts, nil },
		Now:   fixedNow,
	}
}

// serveOn is the in-process transport to p: it checks the remote-exec request
// and runs Serve in p's clone, as `crew remote-exec` would.
func serveOn(t *testing.T, p *side) Transport {
	return func(_ context.Context, dest string, request []byte) ([]byte, error) {
		var req remote.Request
		if err := json.Unmarshal(request, &req); err != nil {
			t.Errorf("request: %v", err)
			return nil, err
		}
		if dest != p.name || !slices.Equal(req.Argv, []string{"crew", "peer-sync"}) || req.Repo != p.repo ||
			req.Slug != testSlug || req.V != 1 || req.Env == nil || req.Stdin == nil {
			t.Errorf("dest %q, request %+v", dest, req)
			return nil, errors.New("bad request")
		}
		var out, errb bytes.Buffer
		if rc := Serve(strings.NewReader(*req.Stdin), &out, &errb, p.serveOptions()); rc != 0 {
			line, _, _ := strings.Cut(errb.String(), "\n")
			return nil, fmt.Errorf("exit %d: %s", rc, line)
		}
		return out.Bytes(), nil
	}
}

// answer is a transport that returns resp whatever was asked.
func answer(t *testing.T, resp Response) Transport {
	return func(context.Context, string, []byte) ([]byte, error) {
		out, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		return out, nil
	}
}

func syncTo(l, p *side, tr Transport) (Result, error) {
	h := l.hosts[p.name]
	return Sync(context.Background(), SyncOptions{
		Paths:     l.paths,
		Self:      l.name,
		Name:      p.name,
		Target:    remote.Target{SSH: h.SSH, Repo: h.Repos[testSlug]},
		Slug:      testSlug,
		Transport: tr,
		Now:       fixedNow,
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(data)
}

func (s *side) peerFile(host string) string {
	return filepath.Join(StoreFor(s.paths).Dir, host+".jsonl")
}

func (s *side) state(t *testing.T, host string) State {
	t.Helper()
	st, err := StoreFor(s.paths).State(host)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (s *side) busID(t *testing.T) string {
	t.Helper()
	id, err := StoreFor(s.paths).BusID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func logSize(t *testing.T, s *side) int64 {
	t.Helper()
	st, err := os.Stat(s.paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// storedLines is what Append writes for rows at seqs from peer.
func storedLines(peer string, rows []string, seqs []int64) string {
	var b strings.Builder
	for i, r := range rows {
		fmt.Fprintf(&b, `{"peer":%q,"seq":%d,"row":%s}`+"\n", peer, seqs[i], r)
	}
	return b.String()
}

// Rows are written in allowlist order so the stored projection equals the line.
func busClaim(crew, issue string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"claim-issue","issue":%q,"branch":"feat/%s-x"}`, ms(time.Minute), crew, issue, issue)
}

func busDispatch(crew, branch string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q,"session":"s1-2","host":"mbp","title":"t"}`, ms(time.Minute), crew, branch)
}

func busMsg(crew, to, body string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"msg","from":"dispatcher:%s","to":%q,"body":%q}`, ms(time.Minute), crew, crew, to, body)
}

func TestSyncRoundTrip(t *testing.T) {
	mbp, halo := pair(t)
	mbpRows := []string{busClaim("c1", "42"), busDispatch("c1", "feat/42-x"), busMsg("c1", "peer:halo", "hi halo")}
	haloRows := []string{busDispatch("c2", "feat/7-y"), busMsg("c2", "peer:mbp", "hi mbp")}
	mbpSeqs := writeLines(t, mbp.paths.Log, mbpRows...)
	haloSeqs := writeLines(t, halo.paths.Log, haloRows...)
	mbpLog, haloLog := readFile(t, mbp.paths.Log), readFile(t, halo.paths.Log)

	res, err := syncTo(mbp, halo, serveOn(t, halo))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 2 || len(res.Dropped) != 0 || len(res.RemoteDropped) != 0 {
		t.Errorf("Result = %+v, want 2 stored, no drops", res)
	}
	wantAtHalo := storedLines("mbp", mbpRows, mbpSeqs)
	wantAtMbp := storedLines("halo", haloRows, haloSeqs)
	if got := readFile(t, halo.peerFile("mbp")); got != wantAtHalo {
		t.Errorf("halo peers/mbp.jsonl =\n%s\nwant\n%s", got, wantAtHalo)
	}
	if got := readFile(t, mbp.peerFile("halo")); got != wantAtMbp {
		t.Errorf("mbp peers/halo.jsonl =\n%s\nwant\n%s", got, wantAtMbp)
	}
	if readFile(t, mbp.paths.Log) != mbpLog || readFile(t, halo.paths.Log) != haloLog {
		t.Error("an events.jsonl changed")
	}

	res, err = syncTo(mbp, halo, serveOn(t, halo))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 0 {
		t.Errorf("second sync stored %d", res.Stored)
	}
	if readFile(t, halo.peerFile("mbp")) != wantAtHalo || readFile(t, mbp.peerFile("halo")) != wantAtMbp {
		t.Error("second sync changed a peers file")
	}
	mbpSize, haloSize := logSize(t, mbp), logSize(t, halo)
	if got, want := mbp.state(t, "halo"), (State{ID: halo.busID(t), HWM: haloSize, Ack: mbpSize}); got != want {
		t.Errorf("mbp halo.state = %+v, want %+v", got, want)
	}
	if got, want := halo.state(t, "mbp"), (State{ID: mbp.busID(t), HWM: mbpSize, Ack: haloSize}); got != want {
		t.Errorf("halo mbp.state = %+v, want %+v", got, want)
	}

	if res, err := syncTo(halo, mbp, serveOn(t, mbp)); err != nil || res.Stored != 0 {
		t.Fatalf("halo→mbp = %+v, %v; want nothing stored", res, err)
	}
	if readFile(t, halo.peerFile("mbp")) != wantAtHalo || readFile(t, mbp.peerFile("halo")) != wantAtMbp {
		t.Error("the reverse sync duplicated rows")
	}

	// New rows on both sides cross once, whichever side dials.
	newMbp, newHalo := []string{busMsg("c1", "peer:halo", "again")}, []string{busClaim("c2", "9")}
	newMbpSeqs := writeLines(t, mbp.paths.Log, newMbp...)
	newHaloSeqs := writeLines(t, halo.paths.Log, newHalo...)
	if res, err := syncTo(halo, mbp, serveOn(t, mbp)); err != nil || res.Stored != 1 {
		t.Fatalf("halo→mbp = %+v, %v; want 1 stored", res, err)
	}
	if res, err := syncTo(mbp, halo, serveOn(t, halo)); err != nil || res.Stored != 0 {
		t.Fatalf("mbp→halo = %+v, %v; want nothing stored", res, err)
	}
	if got, want := readFile(t, halo.peerFile("mbp")), wantAtHalo+storedLines("mbp", newMbp, newMbpSeqs); got != want {
		t.Errorf("halo peers/mbp.jsonl =\n%s\nwant\n%s", got, want)
	}
	if got, want := readFile(t, mbp.peerFile("halo")), wantAtMbp+storedLines("halo", newHalo, newHaloSeqs); got != want {
		t.Errorf("mbp peers/halo.jsonl =\n%s\nwant\n%s", got, want)
	}
}

func TestSyncDropsRowsAndReports(t *testing.T) {
	mbp, halo := pair(t)
	rows := []string{
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"status","from":"worker:feat/42-x","state":"working"}`, ms(time.Minute)),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"msg","from":"dispatcher:other","to":"peer:mbp","body":"hi"}`, ms(time.Minute)),
		busMsg("c1", "peer:mbp", strings.Repeat("x", MaxBody+1)),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"feat/1-\u001b[31m"}`, ms(time.Minute)),
	}
	seqs := writeLines(t, halo.paths.Log, rows...)
	resp := Response{V: 1, Host: "halo", ID: halo.busID(t), Cursor: seqs[len(seqs)-1], Dropped: map[string]int{"control": 2}}
	for i, r := range rows {
		resp.Rows = append(resp.Rows, Entry{Seq: seqs[i], Row: json.RawMessage(r)})
	}

	res, err := syncTo(mbp, halo, answer(t, resp))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"worker": 1, "identity": 1, "oversize": 1, "control": 1}
	if res.Stored != 0 || !reflect.DeepEqual(res.Dropped, want) || !reflect.DeepEqual(res.RemoteDropped, resp.Dropped) {
		t.Errorf("Result = %+v, want nothing stored, Dropped %v, RemoteDropped %v", res, want, resp.Dropped)
	}
	if got := readFile(t, mbp.peerFile("halo")); got != "" {
		t.Errorf("peers/halo.jsonl = %q, want nothing", got)
	}
	// The cursor still passes the dropped rows, so they are not offered again.
	if st := mbp.state(t, "halo"); st.HWM != resp.Cursor {
		t.Errorf("hwm = %d, want %d", st.HWM, resp.Cursor)
	}
}

func TestSyncFoldsUnknownRemoteDropReasons(t *testing.T) {
	mbp, halo := pair(t)
	junk := strings.Repeat("\x1b", 10<<10)
	resp := Response{V: 1, Host: "halo", ID: halo.busID(t), Dropped: map[string]int{junk: 3, "zz": 1, "control": 2}}
	res, err := syncTo(mbp, halo, answer(t, resp))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"other": 4, "control": 2}; !reflect.DeepEqual(res.RemoteDropped, want) {
		t.Errorf("RemoteDropped = %.200v, want %v", res.RemoteDropped, want)
	}
}

func TestServeDropsRowsAndReports(t *testing.T) {
	mbp, halo := pair(t)
	req := Request{V: 1, Host: "mbp", ID: mbp.busID(t), Cursor: 400, Rows: []Entry{
		{Seq: 100, Row: json.RawMessage(fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"nudge","from":"role:critic"}`, ms(time.Minute)))},
		{Seq: 200, Row: json.RawMessage(busMsg("c1", "peer:other", "hi"))},
		{Seq: 300, Row: json.RawMessage(`[1]`)},
		{Seq: 400, Row: json.RawMessage(busMsg("c1", "peer:halo", "ok"))},
		{Seq: 500, Row: json.RawMessage(busMsg("c1", "peer:halo", "past the cursor"))},
	}}
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if rc := Serve(bytes.NewReader(payload), &out, &errb, halo.serveOptions()); rc != 0 {
		t.Fatalf("Serve = %d, stderr %q", rc, errb.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"worker": 1, "identity": 1, "malformed": 1}; !reflect.DeepEqual(resp.Dropped, want) {
		t.Errorf("Dropped = %v, want %v", resp.Dropped, want)
	}
	if resp.V != 1 || resp.Host != "halo" || resp.ID != halo.busID(t) || resp.Ack != 400 {
		t.Errorf("Response = %+v", resp)
	}
	if got, want := readFile(t, halo.peerFile("mbp")), storedLines("mbp", []string{busMsg("c1", "peer:halo", "ok")}, []int64{400}); got != want {
		t.Errorf("peers/mbp.jsonl = %q, want %q", got, want)
	}
	if !strings.HasSuffix(out.String(), "}\n") || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("stdout = %q, want one JSON line", out.String())
	}
}

func TestSyncRefusesChangedServerID(t *testing.T) {
	mbp, halo := pair(t)
	writeLines(t, mbp.paths.Log, busClaim("c1", "42"))
	writeLines(t, halo.paths.Log, busDispatch("c2", "feat/7-y"))
	if _, err := syncTo(mbp, halo, serveOn(t, halo)); err != nil {
		t.Fatal(err)
	}
	writeLines(t, mbp.paths.Log, busClaim("c1", "43"))
	writeLines(t, halo.paths.Log, busDispatch("c2", "feat/8-z"))
	if err := os.WriteFile(filepath.Join(StoreFor(halo.paths).Dir, "self.id"), []byte(strings.Repeat("ab", 16)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	atHalo, atMbp := readFile(t, halo.peerFile("mbp")), readFile(t, mbp.peerFile("halo"))
	haloState, mbpState := halo.state(t, "mbp"), mbp.state(t, "halo")

	_, err := syncTo(mbp, halo, serveOn(t, halo))
	if err == nil || !strings.Contains(err.Error(), "identity") || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("Sync err = %v, want halo's exit 1 identity refusal", err)
	}
	if readFile(t, halo.peerFile("mbp")) != atHalo || readFile(t, mbp.peerFile("halo")) != atMbp {
		t.Error("a refused sync stored rows")
	}
	if halo.state(t, "mbp") != haloState || mbp.state(t, "halo") != mbpState {
		t.Error("a refused sync changed a state")
	}
}

func TestSyncRefusesChangedResponseID(t *testing.T) {
	mbp, halo := pair(t)
	writeLines(t, halo.paths.Log, busDispatch("c2", "feat/7-y"))
	if _, err := syncTo(mbp, halo, serveOn(t, halo)); err != nil {
		t.Fatal(err)
	}
	atMbp, st := readFile(t, mbp.peerFile("halo")), mbp.state(t, "halo")
	seq := writeLines(t, halo.paths.Log, busDispatch("c2", "feat/8-z"))[0]
	resp := Response{V: 1, Host: "halo", ID: strings.Repeat("cd", 16), Ack: 0, Cursor: seq,
		Rows: []Entry{{Seq: seq, Row: json.RawMessage(busDispatch("c2", "feat/8-z"))}}}

	_, err := syncTo(mbp, halo, answer(t, resp))
	if err == nil || err.Error() != "identity: peer halo id changed (remove peers/halo.state to re-pin)" {
		t.Fatalf("Sync err = %v, want the identity refusal", err)
	}
	if readFile(t, mbp.peerFile("halo")) != atMbp || mbp.state(t, "halo") != st {
		t.Error("a refused response was stored")
	}
}

func TestServeRefusesANonPeerHost(t *testing.T) {
	for name, hosts := range map[string]map[string]remote.Host{
		"not peer": {"mbp": {SSH: "mbp", Repos: map[string]string{testSlug: "/r"}}},
		"missing":  {"mini": {SSH: "mini", Peer: true}},
	} {
		t.Run(name, func(t *testing.T) {
			mbp, halo := pair(t)
			halo.hosts = hosts
			writeLines(t, mbp.paths.Log, busClaim("c1", "42"))
			writeLines(t, halo.paths.Log, busDispatch("c2", "feat/7-y"))
			_, err := syncTo(mbp, halo, serveOn(t, halo))
			if err == nil || !strings.Contains(err.Error(), "exit 1: crew: peer-sync: ") || !strings.Contains(err.Error(), "not a peer host") {
				t.Fatalf("Sync err = %v, want halo's exit 1 refusal", err)
			}
			if readFile(t, halo.peerFile("mbp")) != "" || readFile(t, mbp.peerFile("halo")) != "" {
				t.Error("a refused sync stored rows")
			}
		})
	}
}

func TestSyncRefusesAnotherResponseHost(t *testing.T) {
	mbp, halo := pair(t)
	seq := writeLines(t, halo.paths.Log, busDispatch("c2", "feat/7-y"))[0]
	host := "mini" + strings.Repeat("\x1b", 10<<10)
	resp := Response{V: 1, Host: host, ID: halo.busID(t), Cursor: seq,
		Rows: []Entry{{Seq: seq, Row: json.RawMessage(busDispatch("c2", "feat/7-y"))}}}
	_, err := syncTo(mbp, halo, answer(t, resp))
	if err == nil || err.Error() != "answered as another host" {
		t.Fatalf("Sync err = %.200q, want a host mismatch that does not echo the peer's host", err)
	}
	if readFile(t, mbp.peerFile("halo")) != "" || mbp.state(t, "halo") != (State{}) {
		t.Error("a response from the wrong host was stored")
	}
}

func TestSyncRefusesABadResponse(t *testing.T) {
	mbp, halo := pair(t)
	for name, out := range map[string]string{
		"not json":    "nope",
		"two objects": `{"v":1,"host":"halo","id":"` + halo.busID(t) + `"}{}`,
		"version":     `{"v":2,"host":"halo","id":"` + halo.busID(t) + `"}`,
		"bad id":      `{"v":1,"host":"halo","id":"x"}`,
		"oversize":    strings.Repeat(" ", MaxResponse+1),
	} {
		t.Run(name, func(t *testing.T) {
			tr := func(context.Context, string, []byte) ([]byte, error) { return []byte(out), nil }
			if _, err := syncTo(mbp, halo, tr); err == nil {
				t.Fatal("Sync accepted a bad response")
			}
			if mbp.state(t, "halo") != (State{}) {
				t.Error("a bad response wrote state")
			}
		})
	}
}

// TestSyncPinRace has halo dial mbp while mbp's own sync to halo is on the
// wire, so mbp's halo.state is pinned (and halo's rows stored) between Sync's
// two state reads: Sync must judge and dedupe against the re-read state.
func TestSyncPinRace(t *testing.T) {
	mbp, halo := pair(t)
	writeLines(t, mbp.paths.Log, busClaim("c1", "42"))
	haloRows := []string{busDispatch("c2", "feat/7-y"), busMsg("c2", "peer:mbp", "hi")}
	haloSeqs := writeLines(t, halo.paths.Log, haloRows...)

	toHalo := serveOn(t, halo)
	tr := func(ctx context.Context, dest string, request []byte) ([]byte, error) {
		if _, err := syncTo(halo, mbp, serveOn(t, mbp)); err != nil {
			t.Errorf("halo→mbp: %v", err)
		}
		return toHalo(ctx, dest, request)
	}
	res, err := syncTo(mbp, halo, tr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != 0 {
		t.Errorf("Stored = %d, want 0: halo's dial stored them first", res.Stored)
	}
	if got, want := readFile(t, mbp.peerFile("halo")), storedLines("halo", haloRows, haloSeqs); got != want {
		t.Errorf("mbp peers/halo.jsonl =\n%s\nwant\n%s", got, want)
	}
	if got := strings.Count(readFile(t, halo.peerFile("mbp")), "\n"); got != 1 {
		t.Errorf("halo peers/mbp.jsonl has %d lines, want 1", got)
	}
	if st := mbp.state(t, "halo"); st.ID != halo.busID(t) || st.HWM != logSize(t, halo) {
		t.Errorf("mbp halo.state = %+v", st)
	}
}

// A different id pinned while the request was out is caught by the re-read.
func TestSyncPinRaceMismatch(t *testing.T) {
	mbp, halo := pair(t)
	writeLines(t, halo.paths.Log, busDispatch("c2", "feat/7-y"))
	toHalo := serveOn(t, halo)
	tr := func(ctx context.Context, dest string, request []byte) ([]byte, error) {
		if err := StoreFor(mbp.paths).WriteState("halo", State{ID: strings.Repeat("ef", 16)}); err != nil {
			t.Fatal(err)
		}
		return toHalo(ctx, dest, request)
	}
	_, err := syncTo(mbp, halo, tr)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("Sync err = %v, want an identity refusal", err)
	}
	if readFile(t, mbp.peerFile("halo")) != "" {
		t.Error("rows stored under a mismatched pin")
	}
}

func TestSyncBusy(t *testing.T) {
	mbp, halo := pair(t)
	shortLockWait(t, 50*time.Millisecond)
	tr := func(ctx context.Context, dest string, request []byte) ([]byte, error) {
		out, err := serveOn(t, halo)(ctx, dest, request)
		unlock, ok := StoreFor(mbp.paths).Lock("halo")
		if !ok {
			t.Fatal("lock")
		}
		t.Cleanup(unlock)
		return out, err
	}
	if _, err := syncTo(mbp, halo, tr); err == nil || err.Error() != "busy" {
		t.Fatalf("Sync err = %v, want busy", err)
	}
}

// TestSyncHalvesAnOversizeRequest has mbp export rows whose quotes the
// remote-exec request escapes, so the batch fits MaxBatch but not the request
// cap.
func TestSyncHalvesAnOversizeRequest(t *testing.T) {
	old := maxRequest
	maxRequest = 64 << 10
	t.Cleanup(func() { maxRequest = old })
	mbp, halo := pair(t)
	var rows []string
	for range 40 {
		rows = append(rows, busMsg("c1", "peer:halo", strings.Repeat(`"`, 1000)))
	}
	seqs := writeLines(t, mbp.paths.Log, rows...)
	var sent []int
	toHalo := serveOn(t, halo)
	tr := func(ctx context.Context, dest string, request []byte) ([]byte, error) {
		if len(request) > maxRequest {
			t.Errorf("request is %d bytes", len(request))
		}
		var req remote.Request
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		var p Request
		if err := json.Unmarshal([]byte(*req.Stdin), &p); err != nil {
			return nil, err
		}
		sent = append(sent, len(p.Rows))
		if n := len(p.Rows); n > 0 && p.Cursor != p.Rows[n-1].Seq {
			t.Errorf("cursor %d, last row %d", p.Cursor, p.Rows[n-1].Seq)
		}
		return toHalo(ctx, dest, request)
	}
	if _, err := syncTo(mbp, halo, tr); err != nil {
		t.Fatal(err)
	}
	if sent[0] == 0 || sent[0] >= len(rows) {
		t.Fatalf("sent %d of %d rows, want a halved batch", sent[0], len(rows))
	}
	if st := halo.state(t, "mbp"); st.HWM != seqs[sent[0]-1] {
		t.Errorf("halo hwm = %d, want %d", st.HWM, seqs[sent[0]-1])
	}
	// The rest go on the next rounds.
	for len(sent) < 20 && sent[len(sent)-1] > 0 {
		if _, err := syncTo(mbp, halo, tr); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(readFile(t, halo.peerFile("mbp")), "\n"); got != len(rows) {
		t.Errorf("halo stored %d rows, want %d (rounds %v)", got, len(rows), sent)
	}
}

func TestServeRefusesMalformedPayload(t *testing.T) {
	_, halo := pair(t)
	id := strings.Repeat("ab", 16)
	for name, payload := range map[string]string{
		"not json":    "nope",
		"array":       "[]",
		"two objects": `{"v":1,"host":"mbp","id":"` + id + `"}{}`,
		"version":     `{"v":2,"host":"mbp","id":"` + id + `"}`,
		"bad id":      `{"v":1,"host":"mbp","id":"x"}`,
		"oversize":    strings.Repeat(" ", MaxPayload+1),
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if rc := Serve(strings.NewReader(payload), &out, &errb, halo.serveOptions()); rc != 2 {
				t.Fatalf("Serve = %d, want 2", rc)
			}
			if out.Len() != 0 || !strings.HasPrefix(errb.String(), "crew: peer-sync: ") {
				t.Errorf("stdout %q, stderr %q", out.String(), errb.String())
			}
		})
	}
}

func TestServeBusy(t *testing.T) {
	mbp, halo := pair(t)
	shortLockWait(t, 50*time.Millisecond)
	unlock, ok := StoreFor(halo.paths).Lock("mbp")
	if !ok {
		t.Fatal("lock")
	}
	defer unlock()
	payload := `{"v":1,"host":"mbp","id":"` + mbp.busID(t) + `"}`
	var out, errb bytes.Buffer
	if rc := Serve(strings.NewReader(payload), &out, &errb, halo.serveOptions()); rc != 1 || errb.String() != "crew: peer-sync: busy\n" {
		t.Fatalf("Serve = %d, stderr %q; want 1, busy", rc, errb.String())
	}
}

// sshOnPath puts a fake ssh running body first on PATH.
func sshOnPath(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!"+sh+"\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSSHTransport(t *testing.T) {
	t.Run("echo", func(t *testing.T) {
		sshOnPath(t, `[ "$*" = "`+strings.Join(remote.SSHArgv("me@halo"), " ")+`" ] || exit 9
cat
`)
		out, err := SSHTransport(context.Background(), "me@halo", []byte(`{"v":1}`))
		if err != nil || string(out) != `{"v":1}` {
			t.Fatalf("SSHTransport = %q, %v", out, err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		sshOnPath(t, "cat >/dev/null\necho 'connection refused' >&2\necho more >&2\nexit 255\n")
		_, err := SSHTransport(context.Background(), "me@halo", nil)
		if err == nil || err.Error() != "ssh: exit 255: connection refused" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("huge stderr line", func(t *testing.T) {
		sshOnPath(t, `cat >/dev/null
printf 'refused\033[31m\007 ' >&2
head -c 1048576 /dev/zero | tr '\0' x >&2
exit 255
`)
		_, err := SSHTransport(context.Background(), "me@halo", nil)
		if err == nil {
			t.Fatal("SSHTransport succeeded")
		}
		const prefix = "ssh: exit 255: "
		msg := err.Error()
		if !strings.HasPrefix(msg, prefix+"refused[31m xxx") || len(msg) > len(prefix)+stderrLineMax || strings.ContainsFunc(msg, isControl) {
			t.Fatalf("err = %.300q (%d bytes), want the first line without control characters, capped", msg, len(msg))
		}
	})
	t.Run("oversize", func(t *testing.T) {
		sshOnPath(t, fmt.Sprintf("head -c %d /dev/zero\n", MaxResponse+1))
		if _, err := SSHTransport(context.Background(), "me@halo", nil); err == nil {
			t.Fatal("SSHTransport accepted an oversize response")
		}
	})
}
