package stall

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// D8 whole-watch tests: what a watch run writes to the bus. The detector-level
// halves — the episode and its gating, the refresh stamp and lock, the detail
// rendering — are in budget_test.go and probe_detectors_test.go.

// budgetRows is every status row on the bus as from|state|source|detail.
func budgetRows(t *testing.T, h *harness) []string {
	t.Helper()
	data, err := os.ReadFile(h.paths.Log)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var rows []string
	for {
		var v any
		if err := dec.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		row, _ := v.(map[string]any)
		if k, _ := row["kind"].(string); k != "status" {
			continue
		}
		body, _ := row["body"].(map[string]any)
		from, _ := row["from"].(string)
		st, _ := body["state"].(string)
		dt, _ := body["detail"].(string)
		src, ok := body["source"].(string)
		if !ok {
			src = "null"
		}
		rows = append(rows, strings.Join([]string{from, st, src, dt}, "|"))
	}
	return rows
}

// budgetWatch is the bats section's budget_watch: a whole watch over a scripted
// budget verdict, on a static pane with the virtual clock and --max-life as the
// bound, so no test waits in real time. refresh is what a refresh does to the
// cache (the bats budget_refresh_stub).
func budgetWatch(t *testing.T, h *harness, req Request, refresh func()) []string {
	t.Helper()
	if req.Target == "" {
		req.Target = "worker:feat/x"
	}
	def := func(p **string, v string) {
		if *p == nil {
			*p = ptr(v)
		}
	}
	def(&req.Pane, "%9")
	def(&req.Engine, "claude")
	def(&req.Grace, "0")
	def(&req.Interval, "1")
	def(&req.Window, "0")
	def(&req.Idle, "999")
	def(&req.Dead, "999")
	def(&req.BudgetRefresh, "0")
	def(&req.MaxLife, "6")
	o := h.options()
	if refresh != nil {
		newProbes := o.NewProbes
		o.NewProbes = func(pane string) probe.Probes {
			p := newProbes(pane)
			p.RefreshBudget = func(context.Context) { refresh() }
			return p
		}
	}
	if code := Run(h.ctx, req, h.paths, &h.stderr, o); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	return budgetRows(t, h)
}

// d8Window is one line of the windows predicate for a window that used that
// percent and resets that far from the clock seed: name, used_pct, resets_at,
// iso, the way budget-gate.sh prints it. d8ISO is the iso for that reset.
func d8Window(h *harness, name string, used int, off int64) string {
	line := name + "\t" + strconv.Itoa(used) + "\t\t"
	if off == 0 {
		return line
	}
	r := h.seed + off
	return name + "\t" + strconv.Itoa(used) + "\t" + strconv.FormatInt(r, 10) + "\t" +
		time.Unix(r, 0).UTC().Format(time.RFC3339)
}

func d8ISO(h *harness, off int64) string {
	return time.Unix(h.seed+off, 0).UTC().Format(time.RFC3339)
}

// The bus rows a whole watch writes over an exhausted cache: one blocked row for
// the episode, held across the watch's later D8 ticks, and the clearance once a
// read comes back under the line.
func TestBudgetWatchRows(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		// cache and stamp are the watcher's budget cache and its last refresh
		// attempt; %d is the clock seed for the cache, seed-30 for the stamp.
		cache, stamp string
		// win/used and lim answer the two predicates, with resets_at resetOff
		// from the clock. model is the pane's model, local what the local-model
		// lookup answers for it. afterFresh is what a refresh changes about the
		// answer, the way the bats stub rewrote the cache.
		win        string
		used       int
		wrc        int
		lim        string
		lrc        int
		resetOff   int64
		model      string
		local      string
		afterFresh bool
		// want is the row list, %s where a reset iso goes.
		want []string
	}{
		{
			// Two D8 ticks in six seconds: the second holds the episode, it does
			// not raise a second row.
			name: "one blocked row per episode, with the reset",
			win:  "7d", used: 97, wrc: 0, lrc: 1, resetOff: 15570,
			want: []string{"worker:feat/x|blocked|watchdog|budget: claude 7d at 97% (resets %s, in 4h 19m)"},
		},
		{
			// The cache is --budget-refresh old at the second D8 tick, the probe
			// runs, and the re-read finds the drop.
			name:  "clears to working when a refresh drops the engine below the line",
			req:   Request{BudgetRefresh: ptr("1")},
			cache: `{"fetched_epoch":%d}`,
			win:   "5h", used: 100, wrc: 0, lrc: 1, resetOff: 99999, afterFresh: true,
			want: []string{"worker:feat/x|blocked|watchdog|budget: claude 5h at 100% (resets %s, in 1d 3h)",
				"worker:feat/x|working|watchdog|budget: cleared"},
		},
		{
			name: "role mode posts under the role id, never a worker row",
			req:  Request{Target: "role:feat/x:reviewer", Engine: ptr("codex")},
			win:  "5h", used: 100, wrc: 0, lrc: 1, resetOff: 99999,
			want: []string{"role:feat/x:reviewer|blocked|watchdog|budget: codex 5h at 100% (resets %s, in 1d 3h)"},
		},
		{
			name: "role mode stays silent when its own engine has room",
			req:  Request{Target: "role:feat/x:reviewer", Engine: ptr("codex")},
			wrc:  1, lrc: 1,
			want: nil,
		},
		{
			// A stamp of seed-30 read as decimal is recent enough to hold the
			// refresh back; the watch posts what the cache says. The parse itself
			// is TestBudgetLastTry's.
			name:  "a zero-padded stamp is read as decimal",
			req:   Request{BudgetRefresh: ptr("900")},
			stamp: "0%d",
			win:   "5h", used: 100, wrc: 0, lrc: 1, resetOff: 99999,
			want: []string{"worker:feat/x|blocked|watchdog|budget: claude 5h at 100% (resets %s, in 1d 3h)"},
		},
		{
			name: "a pi watcher on a hosted model still posts the limit",
			req:  Request{Engine: ptr("pi")},
			wrc:  1, lim: "key credit limit exhausted", lrc: 0,
			model: "openrouter/x",
			want:  []string{"worker:feat/x|blocked|watchdog|budget: pi limit reached: key credit limit exhausted"},
		},
		{
			name: "a pi watcher on a local model is exempt",
			req:  Request{Engine: ptr("pi")},
			wrc:  1, lim: "key credit limit exhausted", lrc: 0,
			model: "local-x", local: `{"id":"local-x"}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			f.paneModel = tc.model
			o := h.options()
			if tc.cache != "" {
				writeFile(t, o.BudgetFile, replaceVerb(tc.cache, strconv.FormatInt(h.seed, 10)))
			}
			if tc.stamp != "" {
				writeFile(t, o.BudgetFile+".refresh-at", replaceVerb(tc.stamp, strconv.FormatInt(h.seed-30, 10))+"\n")
			}
			win := ""
			if tc.win != "" {
				win = d8Window(h, tc.win, tc.used, tc.resetOff)
			}
			refreshed, refreshes := false, 0
			f.sh = func(op string, args ...string) (string, int) {
				switch {
				case op == "local-model":
					return tc.local, shOffset
				case op != "budget":
					return "", shOffset
				case refreshed: // the cache a refresh just rewrote
					return "", 1 + shOffset
				case args[0] == "windows":
					return win, tc.wrc + shOffset
				}
				return tc.lim, tc.lrc + shOffset
			}
			rows := budgetWatch(t, h, tc.req, func() {
				refreshes++
				refreshed = true
			})
			want := make([]string, 0, len(tc.want))
			iso := d8ISO(h, tc.resetOff)
			for _, x := range tc.want {
				want = append(want, strings.Replace(x, "%s", iso, 1))
			}
			if !slices.Equal(rows, want) {
				t.Errorf("rows\n got %q\nwant %q", rows, want)
			}
			if tc.afterFresh && refreshes == 0 {
				t.Error("the watch never refreshed")
			}
			if tc.stamp != "" && refreshes != 0 {
				t.Errorf("refreshes %d, want the recent stamp to hold the refresh back", refreshes)
			}
			// Every predicate — "budget windows <file> <engine> <now>" — is asked
			// about the watch's own engine.
			engine := "claude"
			if tc.req.Engine != nil {
				engine = *tc.req.Engine
			}
			for _, c := range f.shCalls {
				p := strings.Fields(c)
				if len(p) == 5 && p[0] == "budget" && p[3] != engine {
					t.Errorf("%q asks about %s, want the watch's own engine %s", c, p[3], engine)
				}
			}
		})
	}
}

// A pi role has D8 and its end-of-life exit; with a hosted model D8 is on, so
// the watch polls like any other role instead of exiting at once.
func TestPiRoleHostedModelKeepsPolling(t *testing.T) {
	f := newFake()
	f.paneModel = "openrouter/x"
	f.sh = budgetSh("", 1, "", 1)
	h := newHarness(t, f)
	code, stderr := h.run(Request{Target: "role:feat/x:reviewer", Pane: ptr("%1"), Engine: ptr("pi"),
		BudgetRefresh: ptr("900"), Grace: ptr("0"), Interval: ptr("1"), MaxLife: ptr("4")})
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if f.samples == 0 {
		t.Error("the role watch never polled")
	}
	if n := countCalls(f, "local-model "); n != 1 {
		t.Errorf("local-model read %d times, want once for the D8 decision", n)
	}
	wantRows(t, h)
}
