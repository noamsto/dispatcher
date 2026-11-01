package stall

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/nudge"
)

// D6 whole-watch tests: what a watch run writes to the bus, and how often it
// types, over a real bus and the real in-process unread scan. The
// detector-level half — the episode's state machine over a scripted scan — is
// in probe_detectors_test.go.

// d6Lead is a sessioned worker id, so the scan reads that session's delivered
// marks.
const d6Lead = "worker:feat/x#s1-1"

// d6Msg is a msg row to the lead, age seconds before the clock seed.
func d6Msg(h *harness, from, to string, age int64) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":%q,"kind":"msg","body":"x"}`,
		h.seed*1000-age*1000, from, to)
}

// d6Deliver replays what a reader handing msgs out does to them: marks.Record's
// marks under the lead's session, which is what takes them out of the scan.
// `crew inbox`, `crew await` and the pi directive-delivery hook (it shells out
// to `crew inbox --undelivered`) all deliver the same one way.
func d6Deliver(t *testing.T, h *harness, sid string, rows ...string) {
	t.Helper()
	vs := make([]any, 0, len(rows))
	for _, r := range rows {
		if r == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(r))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		vs = append(vs, v)
	}
	if !marks.Record(h.paths.Dir, "c1", sid, vs) {
		t.Fatal("marks.Record failed")
	}
}

// d6Nudge scripts the D6 type per call (1-based): rc 0 accepted, 2 refused,
// 3 typed but not accepted. What the type actually types, and every refusal,
// held and unconfirmed wording, is internal/nudge's contract.
type d6Nudge func(call int) (string, int)

func (f d6Nudge) at(call int) (string, int) {
	if f == nil {
		return "", 0
	}
	return f(call)
}

// d6Run is one whole watch: the lead on %9, --unread 10, the virtual clock and
// --max-life as the only bound, so no test sleeps in real time. Options.Unread
// is left nil on purpose: Run then wires internal/unread against the harness's
// bus, so every row comes off the same scan the arm runs.
type d6Run struct {
	req   Request
	frame string // the pane's static frame; done_idle.txt is an idle box
	state string // the lead's own status body; empty is `working`
	// roleAge/dispAge seed an undelivered role verdict / dispatcher directive
	// that many seconds before the clock seed; 0 seeds nothing.
	roleAge, dispAge int64
	leadReply        bool // the lead answered the role, newer than its verdict
	leadSays         bool // the lead msgged the dispatcher, which answers nothing
	nudgedBefore     bool // another nudge already covered the newest directive
	// delivered is the lead having read its inbox before the watch started;
	// deliverAt is the sample (1-based) where it reads it mid-run. deliverFrom
	// picks what that read handed out: "role", "disp" or both.
	delivered   bool
	deliverAt   int
	deliverFrom string
	nudge       d6Nudge
}

// d6WatchRun runs the watch and returns its harness, every watchdog row the run
// wrote, and the nudge requests it made.
func d6WatchRun(t *testing.T, tc d6Run) (*harness, []string, []nudge.PaneRequest) {
	t.Helper()
	def := func(p **string, v string) {
		if *p == nil {
			*p = ptr(v)
		}
	}
	if tc.frame == "" {
		tc.frame = "done_idle.txt"
	}
	if tc.req.Target == "" {
		tc.req.Target = d6Lead
	}
	if tc.state == "" {
		tc.state = `{"state":"working","detail":"","source":""}`
	}
	def(&tc.req.Pane, "%9")
	def(&tc.req.Engine, "claude")
	def(&tc.req.Grace, "0")
	def(&tc.req.Interval, "1")
	def(&tc.req.Unread, "10")
	def(&tc.req.Window, "0")
	def(&tc.req.Stall, "999")
	def(&tc.req.Idle, "999")
	def(&tc.req.Dead, "999")
	def(&tc.req.BudgetRefresh, "0")
	def(&tc.req.MaxLife, "13")

	f := newFake()
	f.sample = sampling(frameText(t, tc.frame))
	// D8 quiet, the way it is on a machine with no budget windows: both
	// predicates answer clear.
	f.sh = budgetSh("", 1, "", 1)
	var types []nudge.PaneRequest
	f.nudge = func(_ context.Context, req nudge.PaneRequest) (string, int) {
		types = append(types, req)
		return tc.nudge.at(len(types))
	}
	h := newHarness(t, f)

	role, disp := "", ""
	if tc.roleAge > 0 {
		role = d6Msg(h, "role:feat/x:reviewer", tc.req.Target, tc.roleAge)
	}
	if tc.dispAge > 0 {
		disp = d6Msg(h, "dispatcher:c1", tc.req.Target, tc.dispAge)
	}
	rows := []string{h.status(tc.req.Target, 0, tc.state)}
	for _, m := range []string{role, disp} {
		if m != "" {
			rows = append(rows, m)
		}
	}
	if tc.leadReply {
		rows = append(rows, d6Msg(h, tc.req.Target, "role:feat/x:reviewer", tc.roleAge-10))
	}
	if tc.leadSays {
		rows = append(rows, d6Msg(h, tc.req.Target, "dispatcher:c1", tc.dispAge-10))
	}
	if tc.nudgedBefore {
		// A nudge row for the newest directive, as `crew nudge` or an earlier
		// watchdog would have appended it.
		rows = append(rows, fmt.Sprintf(
			`{"ts":%d,"crew_id":"c1","kind":"nudge","from":"dispatcher:c1","to":%q,`+
				`"branch":"feat/x","pane":"%%9","engine":"claude","result":"held","detail":"","msg_ts":%d}`,
			h.seed*1000-1000, tc.req.Target, (h.seed-tc.dispAge)*1000))
	}
	h.writeRows(rows...)
	deliver := func(which string) {
		switch which {
		case "role":
			d6Deliver(t, h, tc.req.Target, role)
		case "disp":
			d6Deliver(t, h, tc.req.Target, disp)
		default:
			d6Deliver(t, h, tc.req.Target, role, disp)
		}
	}
	if tc.delivered {
		deliver(tc.deliverFrom)
	}

	if tc.deliverAt > 0 {
		sample := f.sample
		f.sample = func(n int) (string, bool) {
			if n == tc.deliverAt {
				deliver(tc.deliverFrom)
			}
			return sample(n)
		}
	}

	o := h.options()
	o.Unread = nil
	if code := Run(h.ctx, tc.req, h.paths, &h.stderr, o); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	return h, pdRows(t, h), types
}

const (
	d6Role30 = "blocked|unread: role verdict undelivered for 30s — lead is working but has not read it; nudge it (`crew nudge <lead>`) to run `crew await`"
	d6Disp   = "blocked|unread: dispatcher directive undelivered for %ds — lead is working but has not reached a peek seam (long stage or idle on a background task)"
)

func d6DispAt(age int64) string { return fmt.Sprintf(d6Disp, age) }

// The verdict side of D6: the oldest undelivered msg of the two kinds, posted
// once with its exact detail, and every silence that keeps it off the bus.
func TestUnreadWatchVerdict(t *testing.T) {
	cases := []struct {
		name string
		d6Run
		want []string
	}{
		{
			name: "flags a working lead with an undelivered role verdict",
			// NoNudge keeps the verdict side of D6 under test; the auto-nudge
			// side has its own cases.
			d6Run: d6Run{roleAge: 30, req: Request{NoNudge: true, MaxLife: ptr("3")}},
			want:  []string{d6Role30},
		},
		{
			name: "is silent once the verdict is delivered",
			// The lead read its inbox before the watch started.
			d6Run: d6Run{roleAge: 30, delivered: true, req: Request{MaxLife: ptr("3")}},
		},
		{
			name:  "is silent while the verdict is younger than --unread",
			d6Run: d6Run{roleAge: 1, req: Request{Unread: ptr("999"), MaxLife: ptr("3")}},
		},
		{
			name: "is silent when the lead is not working",
			// A pr_open lead is still watching its CI, but D6 speaks to a lead
			// that could take the msg.
			d6Run: d6Run{roleAge: 30, state: `{"state":"pr_open","detail":"x","source":""}`,
				req: Request{MaxLife: ptr("3")}},
		},
		{
			name:  "ignores a verdict the lead already answered",
			d6Run: d6Run{roleAge: 30, leadReply: true, req: Request{MaxLife: ptr("3")}},
		},
		{
			name: "is silent for a branch-keyed watchdog",
			// A branch-keyed watch cannot name the lead's session, so it cannot
			// read its marks.
			d6Run: d6Run{roleAge: 30, req: Request{Target: "worker:feat/x", MaxLife: ptr("3")}},
		},
		{
			name:  "flags a working lead with an undelivered dispatcher directive",
			d6Run: d6Run{dispAge: 30, req: Request{NoNudge: true, MaxLife: ptr("3")}},
			want:  []string{d6DispAt(30)},
		},
		{
			name: "keeps a dispatcher directive blocked while undelivered",
			// One episode, held: the run's later D6 ticks post nothing more.
			d6Run: d6Run{dispAge: 30, req: Request{NoNudge: true, MaxLife: ptr("9")}},
			want:  []string{d6DispAt(30)},
		},
		{
			name:  "does not treat a lead msg to the dispatcher as handling a directive",
			d6Run: d6Run{dispAge: 30, leadSays: true, req: Request{NoNudge: true, MaxLife: ptr("3")}},
			want:  []string{d6DispAt(30)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rows, _ := d6WatchRun(t, tc.d6Run)
			if !slices.Equal(rows, tc.want) {
				t.Fatalf("rows %q, want %q", rows, tc.want)
			}
		})
	}
}

// The clear, on the delivery the lead makes mid-run: one `working|unread:
// cleared`, and nothing after it.
func TestUnreadWatchClearsOnDelivery(t *testing.T) {
	for _, tc := range []struct{ name, deliver string }{
		{"role verdict", "role"},
		// The same clear the pi directive-delivery hook makes: it delivers by
		// running `crew inbox --undelivered`, so it lands on the same marks.
		{"dispatcher directive", "disp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role, disp := 0, 0
			if tc.deliver == "role" {
				role = 30
			} else {
				disp = 30
			}
			_, rows, _ := d6WatchRun(t, d6Run{
				roleAge: int64(role), dispAge: int64(disp),
				deliverFrom: tc.deliver, deliverAt: 3,
				req: Request{NoNudge: true, MaxLife: ptr("9")},
			})
			want := []string{"blocked|unread: ", "working|unread: cleared"}
			if len(rows) != 2 || !strings.HasPrefix(rows[0], want[0]) || rows[1] != want[1] {
				t.Fatalf("rows %q, want the verdict then %q", rows, want[1])
			}
		})
	}
}

// Two kinds, one episode: delivering only the verdict clears the stale label,
// and the directive still open re-opens the episode under its own wording.
func TestUnreadWatchRelabelsOnPartialDelivery(t *testing.T) {
	_, rows, _ := d6WatchRun(t, d6Run{
		roleAge: 40, dispAge: 30, deliverFrom: "role", deliverAt: 3,
		req: Request{NoNudge: true, MaxLife: ptr("13")},
	})
	want := []string{
		"blocked|unread: role verdict undelivered for 40s — lead is working but has not read it; nudge it (`crew nudge <lead>`) to run `crew await`",
		"working|unread: cleared",
		d6DispAt(38),
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows %q, want %q", rows, want)
	}
}

// The nudge side of D6 over a whole run: one type per directive, what it does
// to that tick's verdict, and the refusals that keep the watch silent.
func TestUnreadWatchAutoNudge(t *testing.T) {
	// nudge.go's frame-gate refusal: rc 2 tells stall nothing was typed, so the
	// next tick may call again.
	const liveTurn = "live turn or no idle input box"
	accepted := func(int) (string, int) { return "nudge accepted: %9 " + d6Lead, 0 }
	heldNudge := func(int) (string, int) {
		return "nudge held: %9 " + d6Lead + " — the line still sits in the input box after one Enter", 3
	}
	unconfirmed := func(call int) (string, int) {
		if call == 1 {
			return liveTurn, 2
		}
		return "nudge unconfirmed: %9 " + d6Lead + " — typed but not confirmed in the input box", 3
	}

	cases := []struct {
		name string
		d6Run
		wantRows []string
		// wantCalls is how many nudge calls the watch made over the run; a
		// refusal is a call that typed nothing.
		wantCalls int
	}{
		{
			name: "auto-nudges an idle lead once and skips that tick's post",
			// The accepted type skips this tick's post, and the lead that reads
			// its inbox right after never gets a verdict row at all.
			d6Run:     d6Run{dispAge: 30, deliverAt: 4, nudge: accepted},
			wantCalls: 1,
		},
		{
			name: "types an undelivered directive's nudge only once",
			// Accepted, but the lead never reads: the later D6 ticks of the run
			// post the verdict and never re-type.
			d6Run:     d6Run{dispAge: 30, nudge: accepted},
			wantRows:  []string{d6DispAt(34)},
			wantCalls: 1,
		},
		{
			// The directive is covered by that row, so the watch never types; it
			// still posts the verdict, on its first tick.
			name:     "does not re-type a directive another nudge already covered",
			d6Run:    d6Run{dispAge: 30, nudgedBefore: true, nudge: accepted, req: Request{MaxLife: ptr("3")}},
			wantRows: []string{d6DispAt(30)},
		},
		{
			name:      "does not nudge a lead mid-turn and posts as before",
			d6Run:     d6Run{dispAge: 30, req: Request{MaxLife: ptr("3")}, nudge: func(int) (string, int) { return liveTurn, 2 }},
			wantRows:  []string{d6DispAt(30)},
			wantCalls: 1,
		},
		{
			name:     "does not nudge with --no-nudge",
			d6Run:    d6Run{dispAge: 30, req: Request{NoNudge: true, MaxLife: ptr("13")}},
			wantRows: []string{d6DispAt(30)},
		},
		{
			name: "auto-nudges an idle lead for an overdue role verdict",
			// The accepted type skips this tick's post; the lead reads the role
			// verdict right after and never gets a verdict row.
			d6Run:     d6Run{roleAge: 30, deliverAt: 4, nudge: accepted},
			wantCalls: 1,
		},
		{
			name:     "role verdict on codex",
			d6Run:    d6Run{roleAge: 30, req: Request{Engine: ptr("codex")}, nudge: accepted},
			wantRows: []string{d6Role30},
		},
		{
			name:     "role verdict on cursor",
			d6Run:    d6Run{roleAge: 30, req: Request{Engine: ptr("cursor")}, nudge: accepted},
			wantRows: []string{d6Role30},
		},
		{
			name:     "role verdict with --no-nudge",
			d6Run:    d6Run{roleAge: 30, req: Request{NoNudge: true, MaxLife: ptr("3")}, nudge: accepted},
			wantRows: []string{d6Role30},
		},
		{
			name: "reports a typed-but-unaccepted auto-nudge once",
			// The line still sits in the box after the Enter: held, once, over
			// four D6 ticks.
			d6Run:     d6Run{dispAge: 30, nudge: heldNudge},
			wantRows:  []string{d6Held(30)},
			wantCalls: 1,
		},
		{
			// The episode is already open when the type comes back unconfirmed:
			// the failure still gets its own row.
			name:      "reports an unaccepted auto-nudge inside its own open unread: episode",
			d6Run:     d6Run{dispAge: 30, nudge: unconfirmed},
			wantRows:  []string{d6DispAt(30), d6Unconfirmed(34)},
			wantCalls: 2,
		},
		{
			name: "stops auto-nudging after an anchor refusal",
			// An anchor refusal means a stale pane or session: nudging stops for
			// good, and the verdict keeps posting.
			d6Run: d6Run{dispAge: 30, nudge: func(int) (string, int) {
				return "anchor: %9 is not the lead pane of @7", 2
			}},
			wantRows:  []string{d6DispAt(30)},
			wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, rows, types := d6WatchRun(t, tc.d6Run)
			if !slices.Equal(rows, tc.wantRows) {
				t.Fatalf("rows %q, want %q", rows, tc.wantRows)
			}
			// A type is the watchdog's own — the row it appends must not read
			// as a directive the lead was handed — and it points at the newest
			// undelivered directive's ts.
			want := nudge.PaneRequest{
				Pane: "%9", Engine: "claude", Session: d6Lead, Crew: "c1",
				Actor: "watchdog", MsgTS: strconv.FormatInt((h.seed-30)*1000, 10),
			}
			if len(types) != tc.wantCalls {
				t.Fatalf("types %+v, want %d", types, tc.wantCalls)
			}
			for i, req := range types {
				if req != want {
					t.Errorf("type %d = %+v, want %+v", i, req, want)
				}
			}
		})
	}
}

func d6Held(age int64) string {
	return "blocked|unread: dispatcher directive undelivered for " + strconv.FormatInt(age, 10) +
		"s — auto-nudge typed but not accepted (nudge held: %9 " + d6Lead + "); verify the pane with crew where"
}

func d6Unconfirmed(age int64) string {
	return "blocked|unread: dispatcher directive undelivered for " + strconv.FormatInt(age, 10) +
		"s — auto-nudge typed but not accepted (nudge unconfirmed: %9 " + d6Lead + "); verify the pane with crew where"
}
